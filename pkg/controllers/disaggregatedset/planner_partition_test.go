/*
Copyright 2026 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package disaggregatedset

import (
	"fmt"
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"

	disaggregatedsetv1 "sigs.k8s.io/lws/api/disaggregatedset/v1"
	leaderworkersetv1 "sigs.k8s.io/lws/api/leaderworkerset/v1"
)

func partitionedConfigs(surge, unavailable int, partitions RoleReplicaState) []RollingUpdateConfig {
	config := make([]RollingUpdateConfig, len(partitions))
	for i, partition := range partitions {
		config[i] = RollingUpdateConfig{MaxSurge: surge, MaxUnavailable: unavailable, Partition: partition}
	}
	return config
}

// settle applies planner steps with immediate readiness until the planner
// holds, checking every step against the executor's validation.
func settle(t *testing.T, state *RolloutState) {
	t.Helper()
	for range 100 {
		step := ComputeNextStep(*state)
		if step == nil {
			return
		}
		require.NoError(t, validateUpdateStep(*state, step))
		for i, desired := range state.Target.DesiredReplicas {
			ready := step.Past[i] + state.Target.ReadyReplicas[i]
			for _, parked := range state.ParkedOld {
				ready += parked.ReadyReplicas[i]
			}
			require.GreaterOrEqual(t, ready, desired-state.Config[i].MaxUnavailable, "role %d availability", i)
		}
		state.ActiveOld.observeFullyReady(step.Past)
		state.Target.observeFullyReady(step.New)
	}
	t.Fatal("the planner did not settle")
}

func TestPartitionHoldsRolloutAtEachStep(t *testing.T) {
	for _, tc := range []struct {
		desired    RoleReplicaState
		partitions []RoleReplicaState
	}{
		{desired: RoleReplicaState{3, 3}, partitions: []RoleReplicaState{{2, 2}, {1, 1}, {0, 0}}},
		{desired: RoleReplicaState{2, 6}, partitions: []RoleReplicaState{{1, 5}, {1, 3}, {0, 0}}},
		{desired: RoleReplicaState{2, 4}, partitions: []RoleReplicaState{{1, 2}, {0, 0}}},
	} {
		for _, budget := range [][3]int{{1, 0, 0}, {0, 1, 0}, {1, 1, 0}, {2, 0, 0}, {1, 0, 1}, {0, 1, 1}} {
			t.Run(fmt.Sprintf("%v/surge=%d,unavailable=%d,immediate=%d", tc.desired, budget[0], budget[1], budget[2]), func(t *testing.T) {
				zero := make(RoleReplicaState, len(tc.desired))
				state := rolloutState(tc.desired, tc.desired, tc.desired, nil, nil, zero, zero, tc.desired,
					partitionedConfigs(budget[0], budget[1], tc.partitions[0]))
				state.ScaleUpDuringRollout, state.ScaleDownDuringRollout = budget[2] == 1, budget[2] == 1
				for _, partitions := range tc.partitions {
					for i := range state.Config {
						state.Config[i].Partition = partitions[i]
					}
					settle(t, &state)
					for i, desired := range tc.desired {
						require.Equal(t, desired-partitions[i], state.Target.SpecReplicas[i], "target role %d at partition %v", i, partitions)
						require.Equal(t, partitions[i], state.ActiveOld.SpecReplicas[i], "old role %d at partition %v", i, partitions)
					}
				}
			})
		}
	}
}

func TestPartitionAtDesiredStartsNoTargetGroups(t *testing.T) {
	desired, zero := RoleReplicaState{2, 2}, RoleReplicaState{0, 0}
	state := rolloutState(desired, desired, desired, nil, nil, zero, zero, desired, partitionedConfigs(1, 1, RoleReplicaState{2, 5}))
	require.Nil(t, ComputeNextStep(state))
}

func TestRaisingPartitionDoesNotRetractTargetGroups(t *testing.T) {
	desired := RoleReplicaState{3, 3}
	state := rolloutState(desired, RoleReplicaState{1, 1}, RoleReplicaState{1, 1}, nil, nil,
		RoleReplicaState{2, 2}, RoleReplicaState{2, 2}, desired, partitionedConfigs(1, 0, RoleReplicaState{2, 2}))
	require.Nil(t, ComputeNextStep(state), "a raised partition stops progress; it does not roll back")
}

func TestPartitionKeepsTheCoordinationWindow(t *testing.T) {
	// Partitions 1 and 9 allow 9 and 1 new groups, but each role still
	// progresses within one window (1/10) of the least advanced role.
	desired, zero := RoleReplicaState{10, 10}, RoleReplicaState{0, 0}
	state := rolloutState(desired, desired, desired, nil, nil, zero, zero, desired, partitionedConfigs(9, 0, RoleReplicaState{1, 9}))
	step := ComputeNextStep(state)
	require.NotNil(t, step)
	require.NoError(t, validateUpdateStep(state, step))
	require.LessOrEqual(t, step.New[0]-step.New[1], 1, "new groups %v leave the coordination window", step.New)
	settle(t, &state)
	require.Equal(t, RoleReplicaState{2, 1}, state.Target.SpecReplicas)
	require.True(t, heldAtPartition(state))
}

func TestRaisingPartitionMidSurgeReturnsToDesired(t *testing.T) {
	// Two old and two target groups per role (a surge toward partition 1) when
	// the partition is raised to 2. Deferred keeps the target's groups and
	// drains the old revision to desired - target; Immediate takes the excess
	// from the target first.
	for _, tc := range []struct {
		immediate           bool
		wantOld, wantTarget RoleReplicaState
	}{
		{false, RoleReplicaState{1, 1}, RoleReplicaState{2, 2}},
		{true, RoleReplicaState{2, 2}, RoleReplicaState{1, 1}},
	} {
		t.Run(fmt.Sprintf("immediate=%t", tc.immediate), func(t *testing.T) {
			desired := RoleReplicaState{3, 3}
			state := rolloutState(desired, RoleReplicaState{2, 2}, RoleReplicaState{2, 2}, nil, nil,
				RoleReplicaState{2, 2}, RoleReplicaState{2, 2}, desired, partitionedConfigs(1, 0, RoleReplicaState{2, 2}))
			state.ScaleDownDuringRollout = tc.immediate
			require.False(t, heldAtPartition(state))
			settle(t, &state)
			require.Equal(t, tc.wantOld, state.ActiveOld.SpecReplicas)
			require.Equal(t, tc.wantTarget, state.Target.SpecReplicas)
			require.True(t, heldAtPartition(state))
		})
	}
}

func TestScaleDownDuringPartitionHold(t *testing.T) {
	for _, tc := range []struct {
		name              string
		immediate         bool
		partition         int
		old, target       RoleReplicaState // held at replicas 3
		wantOld, wantNext RoleReplicaState // after replicas drops to 2
	}{
		// Deferred never reduces the target during a rollout: old groups drain.
		{"deferred, partition 2", false, 2, RoleReplicaState{2, 2}, RoleReplicaState{1, 1}, RoleReplicaState{1, 1}, RoleReplicaState{1, 1}},
		{"deferred, partition 1", false, 1, RoleReplicaState{1, 1}, RoleReplicaState{2, 2}, RoleReplicaState{0, 0}, RoleReplicaState{2, 2}},
		// Immediate takes the excess from the target first, as a StatefulSet
		// removes its newest Pods first, keeping one group of a started role.
		{"immediate, partition 2", true, 2, RoleReplicaState{2, 2}, RoleReplicaState{1, 1}, RoleReplicaState{1, 1}, RoleReplicaState{1, 1}},
		{"immediate, partition 1", true, 1, RoleReplicaState{1, 1}, RoleReplicaState{2, 2}, RoleReplicaState{1, 1}, RoleReplicaState{1, 1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			desired := RoleReplicaState{2, 2}
			state := rolloutState(RoleReplicaState{3, 3}, tc.old, tc.old, nil, nil, tc.target, tc.target, desired,
				partitionedConfigs(1, 0, RoleReplicaState{tc.partition, tc.partition}))
			state.ScaleDownDuringRollout = tc.immediate
			settle(t, &state)
			require.Equal(t, tc.wantOld, state.ActiveOld.SpecReplicas, "old")
			require.Equal(t, tc.wantNext, state.Target.SpecReplicas, "target")
			require.True(t, heldAtPartition(state) || tc.wantOld[0] == 0)
		})
	}
}

func TestHeldAtPartitionRequiresSettledGroups(t *testing.T) {
	desired := RoleReplicaState{3, 3}
	held := func(oldReady, targetSpec, targetReady RoleReplicaState) bool {
		return heldAtPartition(rolloutState(desired, RoleReplicaState{2, 2}, oldReady, nil, nil,
			targetSpec, targetReady, desired, partitionedConfigs(1, 0, RoleReplicaState{2, 2})))
	}
	assert.True(t, held(RoleReplicaState{2, 2}, RoleReplicaState{1, 1}, RoleReplicaState{1, 1}))
	assert.False(t, held(RoleReplicaState{2, 2}, RoleReplicaState{1, 1}, RoleReplicaState{1, 0}), "target group starting")
	assert.False(t, held(RoleReplicaState{2, 1}, RoleReplicaState{1, 1}, RoleReplicaState{1, 1}), "old group not Ready")
	assert.False(t, held(RoleReplicaState{2, 2}, RoleReplicaState{1, 0}, RoleReplicaState{1, 0}), "decode below its partition target")
	assert.False(t, held(RoleReplicaState{2, 2}, RoleReplicaState{2, 1}, RoleReplicaState{2, 1}), "prefill surging")
}

func TestPartitionHeldRequiresEverySlice(t *testing.T) {
	executor := &RollingUpdateExecutor{rollingSlices: 2, heldSlices: 1}
	assert.False(t, executor.PartitionHeld(), "one slice is still rolling")
	executor.heldSlices = 2
	assert.True(t, executor.PartitionHeld())
	assert.False(t, (&RollingUpdateExecutor{}).PartitionHeld(), "nothing rolling")
}

func TestPartitionCountsParkedRevisionGroups(t *testing.T) {
	// One group per role remains on a parked revision, so the active old
	// revision only needs to keep one more to hold a partition of two.
	desired := RoleReplicaState{3, 3}
	state := rolloutState(desired, RoleReplicaState{2, 2}, RoleReplicaState{2, 2}, RoleReplicaState{1, 1}, RoleReplicaState{1, 1},
		RoleReplicaState{0, 0}, RoleReplicaState{0, 0}, desired, partitionedConfigs(1, 0, RoleReplicaState{2, 2}))
	settle(t, &state)
	require.Equal(t, RoleReplicaState{1, 1}, state.ActiveOld.SpecReplicas)
	require.Equal(t, RoleReplicaState{1, 1}, state.Target.SpecReplicas)
}

func TestExtractRollingUpdateConfigReadsRolePartition(t *testing.T) {
	ds := &disaggregatedsetv1.DisaggregatedSet{}
	for name, partition := range map[string]*int32{"prefill": ptr.To[int32](2), "decode": nil} {
		role := disaggregatedsetv1.DisaggregatedRoleSpec{Name: name}
		role.Spec.RolloutStrategy.RollingUpdateConfiguration = &leaderworkersetv1.RollingUpdateConfiguration{
			MaxSurge: intstr.FromInt(1), Partition: partition,
		}
		ds.Spec.Roles = append(ds.Spec.Roles, role)
	}
	config := extractRollingUpdateConfig(ds, []string{"prefill", "decode"}, map[string]int{"prefill": 3, "decode": 3})
	require.Equal(t, []RollingUpdateConfig{{MaxSurge: 1, Partition: 2}, {MaxSurge: 1}}, config)
	require.True(t, partitioned(config))
}

func TestPartitionAppliesToRollback(t *testing.T) {
	// A canary held at one new group per role is reverted: the original
	// revision is the target again, with two groups, and the canary is old.
	desired := RoleReplicaState{3, 3}
	canary := RoleReplicaState{1, 1}
	state := rolloutState(canary, canary, canary, nil, nil, RoleReplicaState{2, 2}, RoleReplicaState{2, 2}, desired,
		partitionedConfigs(1, 0, RoleReplicaState{2, 2}))
	settle(t, &state)
	require.Equal(t, canary, state.ActiveOld.SpecReplicas, "a non-zero partition also holds a rollback")

	for i := range state.Config {
		state.Config[i].Partition = 0
	}
	settle(t, &state)
	require.Equal(t, RoleReplicaState{0, 0}, state.ActiveOld.SpecReplicas)
	require.Equal(t, desired, state.Target.SpecReplicas)
}

func TestPartitionHoldCondition(t *testing.T) {
	partitioned := func(name string, partition int32) disaggregatedsetv1.DisaggregatedRoleSpec {
		role := staticRole(name)
		role.Spec.RolloutStrategy.RollingUpdateConfiguration = &leaderworkersetv1.RollingUpdateConfiguration{Partition: ptr.To(partition)}
		return role
	}
	status := func(name string, desired, replicas, ready, updated int32) disaggregatedsetv1.RoleStatus {
		return disaggregatedsetv1.RoleStatus{Name: name, DesiredReplicas: ptr.To(desired), Replicas: replicas, ReadyReplicas: ready, UpdatedReplicas: updated}
	}
	for _, tc := range []struct {
		name       string
		slices     int32
		partitions []int32 // per role: prefill, decode
		statuses   []disaggregatedsetv1.RoleStatus
		reason     string
		message    string
	}{
		{"held at partition", 1, []int32{2, 2},
			[]disaggregatedsetv1.RoleStatus{status("prefill", 3, 3, 3, 1), status("decode", 3, 3, 3, 1)},
			reasonRolloutPartitioned, "Rolling update held at the role partitions; groups on the current revision: prefill 1/3, decode 1/3"},
		{"one role complete, one held", 1, []int32{0, 2},
			[]disaggregatedsetv1.RoleStatus{status("prefill", 3, 3, 3, 3), status("decode", 3, 3, 3, 1)},
			reasonRolloutPartitioned, "Rolling update held at the role partitions; groups on the current revision: prefill 3/3, decode 1/3"},
		{"surging toward the partition", 1, []int32{2, 2},
			[]disaggregatedsetv1.RoleStatus{status("prefill", 3, 4, 3, 1), status("decode", 3, 3, 3, 1)}, "RolloutInProgress", ""},
		{"new group not ready", 1, []int32{2, 2},
			[]disaggregatedsetv1.RoleStatus{status("prefill", 3, 3, 2, 1), status("decode", 3, 3, 3, 1)}, "RolloutInProgress", ""},
		{"short of its own partition, held by a peer's", 1, []int32{1, 9},
			[]disaggregatedsetv1.RoleStatus{status("prefill", 10, 10, 10, 2), status("decode", 10, 10, 10, 1)},
			reasonRolloutPartitioned, "Rolling update held at the role partitions; groups on the current revision: prefill 2/10, decode 1/10"},
		{"partition above replicas, many slices", 2, []int32{math.MaxInt32, math.MaxInt32},
			[]disaggregatedsetv1.RoleStatus{status("prefill", 6, 6, 6, 0), status("decode", 6, 6, 6, 0)},
			reasonRolloutPartitioned, "Rolling update held at the role partitions; groups on the current revision: prefill 0/6, decode 0/6"},
		{"partition applies per slice", 2, []int32{2, 2},
			[]disaggregatedsetv1.RoleStatus{status("prefill", 6, 6, 6, 2), status("decode", 6, 6, 6, 2)},
			reasonRolloutPartitioned, "Rolling update held at the role partitions; groups on the current revision: prefill 2/6, decode 2/6"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ds := newDSWithRoles("ds", partitioned("prefill", tc.partitions[0]), partitioned("decode", tc.partitions[1]))
			ds.Spec.Slices = ptr.To(tc.slices)
			condition := disaggregatedSetCondition(ds, tc.statuses, false, nil, true)
			assert.Equal(t, string(disaggregatedsetv1.DisaggregatedSetProgressing), condition.Type)
			assert.Equal(t, tc.reason, condition.Reason)
			if tc.message != "" {
				assert.Equal(t, tc.message, condition.Message)
			}
		})
	}
	t.Run("counts alone do not report a hold", func(t *testing.T) {
		// A new LWS's status can briefly count an updated group before it counts it
		// as a replica; only the executor knows the rollout has nothing to do.
		ds := newDSWithRoles("ds", partitioned("prefill", 2))
		condition := disaggregatedSetCondition(ds, []disaggregatedsetv1.RoleStatus{status("prefill", 3, 3, 3, 1)}, false, nil, false)
		assert.Equal(t, "RolloutInProgress", condition.Reason)
	})
	t.Run("a complete rollout is Available, whatever the partition", func(t *testing.T) {
		ds := newDSWithRoles("ds", partitioned("prefill", 3))
		condition := disaggregatedSetCondition(ds, []disaggregatedsetv1.RoleStatus{status("prefill", 3, 3, 3, 3)}, true, nil, true)
		assert.Equal(t, string(disaggregatedsetv1.DisaggregatedSetAvailable), condition.Type)
	})
}

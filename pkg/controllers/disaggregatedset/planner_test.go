/*
Copyright 2025 The Kubernetes Authors.

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
	"math/rand"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func configs(surge, unavailable []int) []RollingUpdateConfig {
	result := make([]RollingUpdateConfig, len(surge))
	for i := range surge {
		result[i] = RollingUpdateConfig{MaxSurge: surge[i], MaxUnavailable: unavailable[i]}
	}
	return result
}

func requiredRoles(replicas RoleReplicaState) []bool {
	required := make([]bool, len(replicas))
	for i, count := range replicas {
		required[i] = count > 0
	}
	return required
}

func rolloutState(
	initial, activeSpec, activeReady, parkedSpec, parkedReady,
	newSpec, newReady, target RoleReplicaState,
	config []RollingUpdateConfig,
) RolloutState {
	availabilityBaseline := slicesClone(initial)
	for i, replicas := range parkedSpec {
		availabilityBaseline[i] = max(availabilityBaseline[i], replicas)
	}
	state := RolloutState{
		ActiveOld: ActiveRevisionState{
			RequiredRoles:    requiredRoles(initial),
			InitialReplicas:  slicesClone(initial),
			SpecReplicas:     slicesClone(activeSpec),
			RawReadyReplicas: slicesClone(activeReady),
			ReadyReplicas:    slicesClone(activeReady),
		},
		Target: TargetRevisionState{
			RequiredRoles:      requiredRoles(target),
			SpecReplicas:       slicesClone(newSpec),
			RawReadyReplicas:   slicesClone(newReady),
			ReadyReplicas:      slicesClone(newReady),
			DesiredReplicas:    slicesClone(target),
			UnschedulableRoles: make([]bool, len(target)),
		},
		AvailabilityBaseline: availabilityBaseline,
		Config:               append([]RollingUpdateConfig(nil), config...),
	}
	if parkedSpec != nil {
		state.ParkedOld = []ParkedRevisionState{{
			RequiredRoles:    requiredRoles(parkedSpec),
			SpecReplicas:     slicesClone(parkedSpec),
			RawReadyReplicas: slicesClone(parkedReady),
			ReadyReplicas:    slicesClone(parkedReady),
		}}
	}
	return state
}

func TestComputeNextStepIntersectsConstraints(t *testing.T) {
	tests := []struct {
		name               string
		state              RolloutState
		wantPast, wantNew  RoleReplicaState
		expectNoTransition bool
		wantBootstrap      bool
	}{
		{"complete rollout has no step", rolloutState(
			[]int{3, 6}, []int{0, 0}, []int{0, 0}, nil, nil, []int{4, 7}, []int{4, 7}, []int{3, 6},
			configs([]int{1, 1}, []int{0, 0})), nil, nil, true, false},
		{"fresh rollout grows within surge and pending bounds", rolloutState(
			[]int{4, 4}, []int{4, 4}, []int{4, 4}, nil, nil, []int{0, 0}, []int{0, 0}, []int{4, 4},
			configs([]int{1, 1}, []int{0, 0})), RoleReplicaState{4, 4}, RoleReplicaState{1, 1}, false, false},
		{"slow pods do not prevent another bounded batch", rolloutState(
			[]int{20, 20}, []int{18, 18}, []int{18, 18}, nil, nil, []int{2, 2}, []int{0, 0}, []int{20, 20},
			configs([]int{2, 2}, []int{2, 2})), RoleReplicaState{18, 18}, RoleReplicaState{4, 4}, false, false},
		{"fractional window holds a faster role", rolloutState(
			[]int{8, 4}, []int{8, 4}, []int{8, 4}, nil, nil, []int{0, 1}, []int{0, 1}, []int{8, 4},
			configs([]int{8, 0}, []int{0, 0})), nil, RoleReplicaState{4, 1}, false, false},
		{"complete parked capacity reduces this phase target", rolloutState(
			[]int{1, 1}, []int{1, 1}, []int{1, 1}, []int{1, 1}, []int{1, 1}, []int{0, 0}, []int{0, 0}, []int{2, 2},
			configs([]int{1, 1}, []int{0, 0})), nil, RoleReplicaState{1, 1}, false, false},
		{"zero budgets are genuinely blocked", rolloutState(
			[]int{1, 1}, []int{1, 1}, []int{1, 1}, nil, nil, []int{0, 0}, []int{0, 0}, []int{1, 1},
			configs([]int{0, 0}, []int{0, 0})), nil, nil, true, false},
		{"asymmetric zero-surge wedge bootstraps its missing role", rolloutState(
			[]int{1, 5}, []int{1, 4}, []int{1, 4}, nil, nil, []int{0, 1}, []int{0, 1}, []int{1, 5},
			configs([]int{0, 0}, []int{1, 1})), RoleReplicaState{1, 4}, RoleReplicaState{1, 1}, false, true},
		{"interrupted rollout bootstraps a surge slot occupied by old revisions", rolloutState(
			[]int{1, 4}, []int{1, 1}, []int{1, 1}, []int{1, 3}, []int{1, 3}, []int{0, 1}, []int{0, 1}, []int{1, 4},
			configs([]int{1, 1}, []int{0, 0})), RoleReplicaState{1, 1}, RoleReplicaState{1, 1}, false, true},
		{"does not add replicas while the bootstrap replica is unready", rolloutState(
			[]int{1, 5}, []int{1, 4}, []int{1, 4}, nil, nil, []int{1, 1}, []int{0, 1}, []int{1, 5},
			configs([]int{0, 0}, []int{1, 1})), nil, nil, true, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			step := ComputeNextStep(tc.state)
			if tc.expectNoTransition {
				assert.Nil(t, step)
				return
			}
			require.NotNil(t, step)
			assert.Equal(t, tc.wantBootstrap, step.UsesBootstrapSurge)
			if tc.wantPast != nil {
				assert.Equal(t, tc.wantPast, step.Past)
			}
			if tc.wantNew != nil {
				assert.Equal(t, tc.wantNew, step.New)
			}
		})
	}
}

func TestPhaseTargetSeedsEveryRequiredTargetRole(t *testing.T) {
	state := rolloutState(
		[]int{1, 4}, []int{1, 1}, []int{1, 1}, []int{1, 3}, []int{1, 3},
		[]int{0, 0}, []int{0, 0}, []int{1, 4},
		configs([]int{2, 2}, []int{0, 0}),
	)
	snapshot := snapshotForRolloutState(state)
	assert.Equal(t, RoleReplicaState{1, 1}, targetReplicasForActiveRevision(snapshot),
		"parked Prefill cannot replace the target revision's own Prefill")

	state = rolloutState(
		[]int{1, 4}, []int{1, 3}, []int{1, 3}, []int{1, 1}, []int{1, 1},
		[]int{0, 0}, []int{0, 0}, []int{1, 4},
		configs([]int{2, 2}, []int{0, 0}),
	)
	snapshot = snapshotForRolloutState(state)
	assert.Equal(t, RoleReplicaState{1, 3}, targetReplicasForActiveRevision(snapshot))
}

func TestComputeNextStepReplacesDisjointRolesFractionally(t *testing.T) {
	// The index-aligned dimensions are A, B, C: ordinary A is replaced by
	// ordinary B and C. Issued replicas alone cannot replace serving capacity.
	for _, tc := range []struct {
		name                       string
		old, oldReady, targetCount int
		targetSpec, targetReady    RoleReplicaState
		wantOld                    int
	}{
		{"target absent", 4, 4, 2, []int{0, 0}, []int{0, 0}, 4},
		{"target issued but unready", 4, 4, 2, []int{2, 2}, []int{0, 0}, 4},
		{"one target role absent", 4, 4, 2, []int{2, 0}, []int{2, 0}, 4},
		{"one target role unready", 4, 4, 2, []int{2, 2}, []int{2, 0}, 4},
		{"half replacement permits partial drain", 4, 4, 2, []int{2, 2}, []int{1, 1}, 2},
		{"least ready role bounds replacement", 4, 4, 2, []int{2, 2}, []int{2, 1}, 2},
		{"fractional replacement credit rounds down", 3, 3, 2, []int{2, 2}, []int{1, 1}, 2},
		{"API-sized baseline does not overflow replacement credit", 2147483647, 2147483647, 3, []int{3, 3}, []int{2, 2}, 715827883},
		{"complete replacement retires old", 4, 4, 2, []int{2, 2}, []int{2, 2}, 0},
		{"singleton targets unready", 2, 2, 1, []int{1, 1}, []int{0, 0}, 2},
		{"singleton target missing", 2, 2, 1, []int{1, 0}, []int{1, 0}, 2},
		{"singleton targets ready", 2, 2, 1, []int{1, 1}, []int{1, 1}, 0},
		{"one unready old pod is not free drain credit", 4, 3, 2, []int{2, 2}, []int{1, 1}, 3},
		{"two unready old pods leave no drain credit", 4, 2, 2, []int{2, 2}, []int{1, 1}, 4},
		{"fully unready old revision has no capacity to preserve", 4, 0, 2, []int{2, 2}, []int{0, 0}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			initial := RoleReplicaState{tc.old, 0, 0}
			state := rolloutState(initial, initial, []int{tc.oldReady, 0, 0}, nil, nil,
				append([]int{0}, tc.targetSpec...), append([]int{0}, tc.targetReady...),
				[]int{0, tc.targetCount, tc.targetCount}, configs([]int{1, 1, 1}, []int{0, 0, 0}))
			past := initial
			if step := ComputeNextStep(state); step != nil {
				past = step.Past
				require.NoError(t, validateUpdateStep(state, step))
			}
			assert.Equal(t, tc.wantOld, past[0], "a drain may delete Ready old pods first")
		})
	}
}

func TestDisjointRoleReplacementLeavesOtherTransitionsUnchanged(t *testing.T) {
	for _, tc := range []struct {
		name                               string
		initial, newSpec, newReady, target RoleReplicaState
		wantPast, wantNew                  RoleReplicaState
	}{
		{"same roles", []int{4, 4}, []int{1, 1}, []int{1, 1}, []int{4, 4}, []int{3, 3}, []int{1, 1}},
		{"cold start", []int{0, 0}, []int{0, 0}, []int{0, 0}, []int{2, 2}, []int{0, 0}, []int{2, 2}},
		{"scale to zero", []int{4, 4}, []int{0, 0}, []int{0, 0}, []int{0, 0}, []int{0, 0}, []int{0, 0}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state := rolloutState(tc.initial, tc.initial, tc.initial, nil, nil,
				tc.newSpec, tc.newReady, tc.target, configs([]int{1, 1}, []int{0, 0}))
			step := ComputeNextStep(state)
			require.NotNil(t, step)
			assert.Equal(t, tc.wantPast, step.Past)
			assert.Equal(t, tc.wantNew, step.New)
			require.NoError(t, validateUpdateStep(state, step))
		})
	}
}

func TestDisjointRoleReplacementPreservesParkedCapacityAccounting(t *testing.T) {
	for _, tc := range []struct {
		name                                 string
		old, baseline                        int
		parkedSpec, parkedReady, targetReady RoleReplicaState
		wantOld                              int
	}{
		{"parked half replacement is not cross-role credit", 4, 4, []int{0, 1, 1}, []int{0, 1, 1}, []int{0, 0, 0}, 4},
		{"parked full replacement is not cross-role credit", 4, 4, []int{0, 2, 2}, []int{0, 2, 2}, []int{0, 0, 0}, 4},
		{"parked capacity cannot augment target progress", 4, 4, []int{0, 1, 1}, []int{0, 1, 1}, []int{0, 1, 1}, 2},
		{"same-role parked capacity counts once", 4, 4, []int{2, 1, 1}, []int{2, 1, 1}, []int{0, 0, 0}, 2},
		{"interrupted baseline survives restart", 2, 4, []int{2, 0, 0}, []int{2, 0, 0}, []int{0, 0, 0}, 2},
		{"interrupted revision can retire with replacement", 2, 4, []int{2, 0, 0}, []int{2, 0, 0}, []int{0, 1, 1}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			initial := RoleReplicaState{tc.old, 0, 0}
			state := rolloutState(initial, initial, initial, tc.parkedSpec, tc.parkedReady,
				tc.targetReady, tc.targetReady, []int{0, 2, 2}, configs([]int{1, 1, 1}, []int{0, 0, 0}))
			state.AvailabilityBaseline = RoleReplicaState{tc.baseline, tc.parkedSpec[1], tc.parkedSpec[2]}
			past := initial
			if step := ComputeNextStep(state); step != nil {
				past = step.Past
				require.NoError(t, validateUpdateStep(state, step))
			}
			assert.Equal(t, tc.wantOld, past[0])
		})
	}
}

func TestDisjointRoleReplacementDoesNotSpendParkedCreditAcrossCandidates(t *testing.T) {
	// Older Y1 and newer X4 are replaced by target Y4. Y1 may retire on
	// another candidate's turn, so it cannot also authorize draining X.
	state := rolloutState([]int{4, 0}, []int{4, 0}, []int{4, 0}, []int{0, 1}, []int{0, 1},
		[]int{0, 1}, []int{0, 1}, []int{0, 4}, configs([]int{1, 1}, []int{0, 0}))
	first := ComputeNextStep(state)
	require.NotNil(t, first)
	assert.Equal(t, RoleReplicaState{3, 0}, first.Past)
	require.NoError(t, validateUpdateStep(state, first))
	// Observe the first drain, keep its new batch unready, then select Y1.
	next := rolloutState([]int{0, 1}, []int{0, 1}, []int{0, 1}, first.Past, first.Past,
		first.New, state.Target.ReadyReplicas, state.Target.DesiredReplicas, state.Config)
	next.AvailabilityBaseline = state.AvailabilityBaseline // X4/Y1 remains the shared baseline.
	second := ComputeNextStep(next)
	require.NotNil(t, second)
	assert.Equal(t, RoleReplicaState{0, 0}, second.Past)
	require.NoError(t, validateUpdateStep(next, second))
	assert.GreaterOrEqual(t, first.Past[0]+next.Target.ReadyReplicas[1], 4,
		"retiring parked Y must preserve a full X4/Y4 serving fraction")
}

func TestDisjointRoleReplacementKeepsFloorWhenOverlappingCandidateSelected(t *testing.T) {
	state := rolloutState([]int{4, 0}, []int{4, 0}, []int{4, 0}, []int{2, 1}, []int{2, 1},
		[]int{0, 1}, []int{0, 1}, []int{0, 4}, configs([]int{1, 1}, []int{0, 0}))
	first := ComputeNextStep(state)
	require.NotNil(t, first)
	assert.Equal(t, RoleReplicaState{1, 0}, first.Past, "same-role parked X counts against the shared floor")
	require.NoError(t, validateUpdateStep(state, first))
	// Selecting the overlapping X2/Y1 revision must not erase X's floor while
	// the disjoint X1 revision remains and the new Y batch is still unready.
	next := rolloutState([]int{2, 1}, []int{2, 1}, []int{2, 1}, first.Past, first.Past,
		first.New, state.Target.ReadyReplicas, state.Target.DesiredReplicas, state.Config)
	next.AvailabilityBaseline = state.AvailabilityBaseline
	past := next.ActiveOld.SpecReplicas
	if second := ComputeNextStep(next); second != nil {
		past = second.Past
		require.NoError(t, validateUpdateStep(next, second))
	}
	assert.Equal(t, 2, past[0], "candidate selection cannot spend readiness already preserving the X floor")
	assert.GreaterOrEqual(t, first.Past[0]+past[0]+next.Target.ReadyReplicas[1], 4)
}

func TestDisjointRoleReplacementBoundsIntroducedRoleWhenOverlappingCandidateSelected(t *testing.T) {
	// Y overlaps the target, Z is new, and parked X keeps the disjoint phase
	// active. Neither target role may issue a second unready batch.
	state := rolloutState([]int{0, 1, 0}, []int{0, 1, 0}, []int{0, 1, 0}, []int{4, 0, 0}, []int{4, 0, 0},
		[]int{0, 1, 1}, []int{0, 0, 0}, []int{0, 4, 4}, configs([]int{1, 1, 1}, []int{0, 0, 0}))
	assert.Nil(t, ComputeNextStep(state))
}

func TestDisjointRoleReplacementDoesNotSpendPendingDeletionCredit(t *testing.T) {
	for _, revision := range []string{"target", "active"} {
		t.Run(revision, func(t *testing.T) {
			state := rolloutState([]int{4, 0, 0}, []int{4, 0, 0}, []int{4, 0, 0}, nil, nil,
				[]int{0, 2, 2}, []int{0, 1, 1}, []int{0, 2, 2}, configs([]int{1, 1, 1}, []int{0, 0, 0}))
			wantOld := 2
			switch revision {
			case "target":
				state.Target.RawReadyReplicas = RoleReplicaState{0, 2, 2}
			case "active":
				state.ActiveOld.SpecReplicas, state.ActiveOld.RawReadyReplicas = []int{3, 0, 0}, []int{3, 0, 0}
				state.ActiveOld.ReadyReplicas = RoleReplicaState{2, 0, 0}
				wantOld = 3
			}
			past := state.ActiveOld.SpecReplicas
			if step := ComputeNextStep(state); step != nil {
				past = step.Past
				require.NoError(t, validateUpdateStep(state, step))
			}
			assert.Equal(t, wantOld, past[0])
		})
	}
}

func TestDisjointRoleReplacementAppliesMaxUnavailableAfterReplacementCredit(t *testing.T) {
	for _, ready := range []int{0, 1} {
		for _, unavailable := range []int{0, 1, 4} {
			state := rolloutState([]int{4, 0, 0}, []int{4, 0, 0}, []int{4, 0, 0}, nil, nil,
				[]int{0, 2, 2}, []int{0, ready, ready}, []int{0, 2, 2}, configs([]int{1, 1, 1}, []int{unavailable, 0, 0}))
			past := state.ActiveOld.SpecReplicas
			if step := ComputeNextStep(state); step != nil {
				past = step.Past
				require.NoError(t, validateUpdateStep(state, step))
			}
			assert.Equal(t, max(0, 4-2*ready-unavailable), past[0], "ready=%d unavailable=%d", ready, unavailable)
		}
	}
}

func TestDisjointRoleReplacementBoundsPendingGrowth(t *testing.T) {
	state := rolloutState([]int{4, 0, 0}, []int{4, 0, 0}, []int{4, 0, 0}, nil, nil,
		[]int{0, 0, 0}, []int{0, 0, 0}, []int{0, 2, 2}, configs([]int{1, 1, 1}, []int{0, 0, 0}))
	step := ComputeNextStep(state)
	require.NotNil(t, step)
	assert.Equal(t, RoleReplicaState{4, 0, 0}, step.Past)
	assert.Equal(t, RoleReplicaState{0, 1, 1}, step.New, "disjoint roles share the normal pending-replica budget")
	state.Target.SpecReplicas = step.New
	assert.Nil(t, ComputeNextStep(state), "an unready first batch cannot authorize another batch")
	state.Target.ReadyReplicas, state.Target.RawReadyReplicas = step.New, step.New
	step = ComputeNextStep(state)
	require.NotNil(t, step)
	assert.Equal(t, RoleReplicaState{0, 2, 2}, step.New)
	assert.Equal(t, RoleReplicaState{2, 0, 0}, step.Past)
	require.NoError(t, validateUpdateStep(state, step))
	state.ActiveOld.SpecReplicas, state.ActiveOld.RawReadyReplicas, state.ActiveOld.ReadyReplicas = step.Past, step.Past, step.Past
	state.Target.SpecReplicas = step.New
	assert.Nil(t, ComputeNextStep(state), "the second batch also waits for replacement readiness")
	state.Target.ReadyReplicas, state.Target.RawReadyReplicas = step.New, step.New
	step = ComputeNextStep(state)
	require.NotNil(t, step)
	assert.Equal(t, RoleReplicaState{0, 0, 0}, step.Past)
	require.NoError(t, validateUpdateStep(state, step))
	state.ActiveOld.SpecReplicas, state.ActiveOld.RawReadyReplicas, state.ActiveOld.ReadyReplicas = step.Past, step.Past, step.Past
	assert.Nil(t, ComputeNextStep(state), "the rollout converges after the replacement becomes Ready")
}

func TestComputeNextStepUsesRevisionAwareReadiness(t *testing.T) {
	// This is the corrected slide-7 state. Decode in C is Ready, but C has no
	// Ready Prefill, so none of C's readiness can authorize B's retirement.
	state := rolloutState(
		[]int{1, 1}, []int{1, 1}, []int{1, 1}, nil, nil,
		[]int{0, 2}, []int{0, 2}, []int{1, 2},
		configs([]int{1, 1}, []int{0, 0}),
	)
	step := ComputeNextStep(state)
	require.NotNil(t, step)
	assert.Equal(t, RoleReplicaState{1, 1}, step.Past, "B must remain complete")
	assert.Equal(t, RoleReplicaState{1, 2}, step.New, "ordinary growth creates C Prefill")

	state.Target.SpecReplicas = RoleReplicaState{1, 2}
	state.Target.RawReadyReplicas = RoleReplicaState{1, 2}
	state.Target.ReadyReplicas = RoleReplicaState{1, 2}
	step = ComputeNextStep(state)
	require.NotNil(t, step)
	assert.Equal(t, RoleReplicaState{0, 0}, step.Past, "B retires once C is complete and Ready")
	assert.Equal(t, RoleReplicaState{1, 2}, step.New)
}

func TestComputeNextStepReleasesCapacityForUnschedulableTargetRole(t *testing.T) {
	// A and B together provide exactly the 1P/4D availability floor. C's
	// bootstrap Prefill exists but cannot be scheduled. Retiring B releases a
	// Prefill slot and temporarily lowers usable Decode capacity from four to
	// three, which is the single unavailable replica permitted by the fallback.
	state := rolloutState(
		[]int{1, 4}, []int{1, 1}, []int{1, 1}, []int{1, 3}, []int{1, 3},
		[]int{1, 1}, []int{0, 1}, []int{1, 4},
		configs([]int{1, 1}, []int{0, 0}),
	)
	beforeBootstrap := state
	beforeBootstrap.Target.SpecReplicas = RoleReplicaState{0, 1}
	beforeBootstrap.Target.UnschedulableRoles = []bool{true, false}
	step := ComputeNextStep(beforeBootstrap)
	require.NotNil(t, step)
	assert.True(t, step.UsesBootstrapSurge,
		"bootstrap surge must be tried before relaxing availability")
	assert.False(t, step.UsesUnavailableFallback)

	assert.Nil(t, ComputeNextStep(state),
		"an unready target role alone must not relax availability")

	state.Target.UnschedulableRoles = []bool{true, false}
	step = ComputeNextStep(state)
	require.NotNil(t, step)
	assert.Equal(t, RoleReplicaState{0, 0}, step.Past)
	assert.Equal(t, RoleReplicaState{1, 1}, step.New)
	assert.True(t, step.UsesUnavailableFallback)
	assert.False(t, step.UsesBootstrapSurge)
	require.NoError(t, validateUpdateStep(state, step))

	// After B retires, the remaining A revision already holds the relaxed
	// availability floor. Ordinary target growth may continue, but the same
	// unschedulable Pod cannot cascade into another old-revision drain.
	state.ActiveOld.SpecReplicas = RoleReplicaState{1, 3}
	state.ActiveOld.RawReadyReplicas = RoleReplicaState{1, 3}
	state.ActiveOld.ReadyReplicas = RoleReplicaState{1, 3}
	state.ParkedOld = nil
	step = ComputeNextStep(state)
	require.NotNil(t, step)
	assert.Equal(t, RoleReplicaState{1, 3}, step.Past)
	assert.False(t, step.UsesUnavailableFallback)
}

func TestUnavailableFallbackRequiresExcessBootstrapCapacity(t *testing.T) {
	tests := []struct {
		name  string
		state RolloutState
	}{
		{
			name: "ordinary singleton rollout keeps its serving revision",
			state: rolloutState(
				[]int{1, 1}, []int{1, 1}, []int{1, 1}, nil, nil,
				[]int{1, 1}, []int{0, 0}, []int{1, 1},
				configs([]int{1, 1}, []int{0, 0}),
			),
		},
		{
			name: "ordinary rollout at its surge ceiling",
			state: rolloutState(
				[]int{4, 4}, []int{4, 4}, []int{4, 4}, nil, nil,
				[]int{1, 1}, []int{0, 0}, []int{4, 4},
				configs([]int{1, 1}, []int{0, 0}),
			),
		},
		{
			name: "excess surge cannot lower a positive floor to zero",
			state: rolloutState(
				[]int{1, 1}, []int{1, 1}, []int{1, 1}, []int{1, 1}, []int{0, 0},
				[]int{1, 1}, []int{0, 0}, []int{1, 1},
				configs([]int{1, 1}, []int{0, 0}),
			),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tc.state.Target.UnschedulableRoles = []bool{true, false}
			assert.Nil(t, ComputeNextStep(tc.state))
		})
	}
}

func TestUsableReadyReplicasRequiresEveryRequiredRole(t *testing.T) {
	required := []bool{true, true}
	assert.Equal(t, RoleReplicaState{0, 0}, usableReadyReplicas(required, RoleReplicaState{0, 2}))
	assert.Equal(t, RoleReplicaState{1, 2}, usableReadyReplicas(required, RoleReplicaState{1, 2}))
	assert.Equal(t, RoleReplicaState{1, 0}, usableReadyReplicas([]bool{true, false}, RoleReplicaState{1, 0}))
}

func TestIncompleteOldRevisionCanRetire(t *testing.T) {
	// Broken old revisions must not strand Decode replicas. A missing Prefill
	// cannot recover; an unready one still requires per-role availability checks.
	for _, tc := range []struct {
		name                     string
		spec, surge, unavailable []int
	}{
		{"missing Prefill", []int{0, 1}, []int{0, 1}, []int{1, 0}},
		{"unready Prefill", []int{1, 1}, []int{1, 1}, []int{0, 0}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state := rolloutState(
				[]int{1, 2}, tc.spec, []int{0, 1}, tc.spec, []int{0, 1},
				[]int{1, 1}, []int{1, 1}, []int{1, 2}, configs(tc.surge, tc.unavailable),
			)
			state.ParkedOld[0].RequiredRoles = []bool{true, true}
			step := ComputeNextStep(state)
			require.NotNil(t, step)
			assert.Equal(t, RoleReplicaState{0, 0}, step.Past)
			assert.Equal(t, RoleReplicaState{1, 1}, step.New)
			require.NoError(t, validateUpdateStep(state, step))
		})
	}
}

func TestAvailabilityFloorDoesNotChangeWithDrainCandidate(t *testing.T) {
	// A was created for 2P/2D but is now parked at 1P/1D. The newer B
	// candidate has a 1P/1D baseline. Selecting B must not lower the rollout's
	// zero-unavailability floor from 2P/2D to B's local 1P/1D baseline.
	state := rolloutState(
		[]int{1, 1}, []int{1, 1}, []int{1, 1}, []int{1, 1}, []int{1, 1},
		[]int{0, 0}, []int{0, 0}, []int{2, 2},
		configs([]int{1, 1}, []int{0, 0}),
	)
	state.AvailabilityBaseline = RoleReplicaState{2, 2}

	step := ComputeNextStep(state)
	require.NotNil(t, step)
	assert.Equal(t, RoleReplicaState{1, 1}, step.Past,
		"B must remain until target growth replaces its Ready capacity")
	assert.Equal(t, RoleReplicaState{1, 1}, step.New)
	require.NoError(t, validateUpdateStep(state, step))
}

func TestRevisionCompletenessIsAPlannerBound(t *testing.T) {
	state := rolloutState(
		[]int{2, 1}, []int{2, 1}, []int{2, 1}, nil, nil,
		[]int{1, 1}, []int{1, 1}, []int{2, 1},
		configs([]int{1, 1}, []int{0, 0}),
	)
	step := ComputeNextStep(state)
	require.NotNil(t, step)
	assert.Equal(t, RoleReplicaState{1, 1}, step.Past,
		"the planner must not return the otherwise-valid 1P/0D target")
}

func TestAvailabilityAccountsForCrossRoleReadyLoss(t *testing.T) {
	state := rolloutState(
		[]int{2, 2}, []int{2, 2}, []int{2, 1}, nil, nil,
		[]int{0, 0}, []int{0, 0}, []int{2, 2},
		configs([]int{1, 1}, []int{0, 1}),
	)
	snapshot := snapshotForRolloutState(state)
	assert.False(t, availabilityPreserved(snapshot, RoleReplicaState{2, 1}, state.ActiveOld.RequiredRoles),
		"losing Decode's last Ready replica would also invalidate Prefill readiness")
	assert.True(t, availabilityPreserved(snapshot, RoleReplicaState{2, 2}, state.ActiveOld.RequiredRoles))
}

func TestIncompleteRevisionsPreservePerRoleReadiness(t *testing.T) {
	state := rolloutState(
		[]int{2, 2}, []int{2, 2}, []int{0, 1}, nil, nil,
		[]int{1, 1}, []int{0, 1}, []int{2, 2},
		configs([]int{1, 1}, []int{0, 0}),
	)
	step := ComputeNextStep(state)
	require.NotNil(t, step)
	assert.Equal(t, RoleReplicaState{1, 2}, step.Past,
		"the unready replica may drain, but the other role's Ready replica must remain")
	require.NoError(t, validateUpdateStep(state, step))
}

func TestReadinessDipPreservesOldRevisionUntilReplacementIsReady(t *testing.T) {
	tests := []struct {
		name        string
		activeReady RoleReplicaState
		newSpec     RoleReplicaState
		newReady    RoleReplicaState
		wantPast    RoleReplicaState
		wantNew     RoleReplicaState
	}{
		{
			name:        "single-role dip does not retire the old revision",
			activeReady: RoleReplicaState{0, 8},
			newSpec:     RoleReplicaState{0, 0},
			newReady:    RoleReplicaState{0, 0},
			wantPast:    RoleReplicaState{1, 8},
			wantNew:     RoleReplicaState{1, 4},
		},
		{
			name:        "recovered old role and partial replacement allow a bounded drain",
			activeReady: RoleReplicaState{1, 8},
			newSpec:     RoleReplicaState{1, 4},
			newReady:    RoleReplicaState{1, 4},
			wantPast:    RoleReplicaState{1, 4},
			wantNew:     RoleReplicaState{1, 4},
		},
		{
			name:        "complete replacement readiness allows retirement",
			activeReady: RoleReplicaState{0, 8},
			newSpec:     RoleReplicaState{1, 8},
			newReady:    RoleReplicaState{1, 8},
			wantPast:    RoleReplicaState{0, 0},
			wantNew:     RoleReplicaState{1, 8},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			state := rolloutState(
				[]int{1, 8}, []int{1, 8}, tc.activeReady, nil, nil,
				tc.newSpec, tc.newReady, []int{1, 8},
				configs([]int{1, 4}, []int{0, 0}),
			)
			step := ComputeNextStep(state)
			require.NotNil(t, step)
			assert.Equal(t, tc.wantPast, step.Past)
			assert.Equal(t, tc.wantNew, step.New)
			require.NoError(t, validateUpdateStep(state, step))
		})
	}
}

func TestPendingDrainDoesNotDiscardObservedCapacityBelowFloor(t *testing.T) {
	// This is the state observed in scenario 15 before B was retired. B's raw
	// Ready replicas still keep Prefill above its floor, but a pending drain
	// leaves B with no committed Decode and therefore no committed usable
	// capacity. Retiring B would make the raw Ready drop visible to users.
	state := rolloutState(
		[]int{50, 25}, []int{7, 3}, []int{1, 0}, []int{37, 18}, []int{36, 18},
		[]int{11, 7}, []int{7, 5}, []int{50, 25},
		configs([]int{5, 5}, []int{5, 5}),
	)
	state.ActiveOld.RawReadyReplicas = RoleReplicaState{10, 5}
	state.ParkedOld[0].RawReadyReplicas = RoleReplicaState{37, 19}
	state.Target.RawReadyReplicas = RoleReplicaState{7, 5}

	snapshot := snapshotForRolloutState(state)
	assert.False(t, availabilityPreserved(snapshot, RoleReplicaState{0, 0}, state.ActiveOld.RequiredRoles),
		"B's observed Ready capacity is still needed to preserve the Prefill floor")
	assert.Nil(t, ComputeNextStep(state), "the planner must wait for pending drains or replacement readiness")
}

func TestPendingDrainOnOneRoleDoesNotRejectSafeDrainOfAnother(t *testing.T) {
	tests := []struct {
		name     string
		state    RolloutState
		rawReady RoleReplicaState
		wantPast RoleReplicaState
	}{
		{
			name: "usable readiness gap",
			state: rolloutState(
				[]int{4, 4}, []int{3, 4}, []int{2, 4}, nil, nil,
				[]int{1, 1}, []int{0, 0}, []int{4, 4},
				configs([]int{1, 1}, []int{1, 1}),
			),
			rawReady: RoleReplicaState{3, 4},
			wantPast: RoleReplicaState{3, 3},
		},
		{
			name: "per-role readiness gap",
			state: rolloutState(
				[]int{2, 2}, []int{2, 1}, []int{0, 0}, nil, nil,
				[]int{1, 1}, []int{0, 0}, []int{2, 2},
				configs([]int{1, 1}, []int{0, 0}),
			),
			rawReady: RoleReplicaState{0, 1},
			wantPast: RoleReplicaState{1, 1},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tc.state.ActiveOld.RawReadyReplicas = tc.rawReady
			step := ComputeNextStep(tc.state)
			require.NotNil(t, step)
			assert.Equal(t, tc.wantPast, step.Past)
			require.NoError(t, validateUpdateStep(tc.state, step))
		})
	}
}

func TestFractionalWindowBounds(t *testing.T) {
	counts := RoleReplicaState{8, 4}
	assert.Equal(t, RoleReplicaState{4, 1},
		boundGrowingRoleTargetsToWindow(RoleReplicaState{0, 1}, counts, RoleReplicaState{8, 1}))
	assert.Equal(t, RoleReplicaState{8, 3},
		boundDrainingRoleTargetsToWindow(counts, counts, RoleReplicaState{8, 2}))
	assert.Equal(t, RoleReplicaState{6, 1},
		boundGrowingRoleTargetsToWindow(RoleReplicaState{6, 1}, counts, RoleReplicaState{8, 1}),
		"the bound must not reverse existing growth")
	assert.Equal(t, RoleReplicaState{2, 3},
		boundDrainingRoleTargetsToWindow(RoleReplicaState{2, 3}, counts, RoleReplicaState{0, 3}),
		"the bound must not reverse an existing drain")
}

func TestHardNewReplicaLimits(t *testing.T) {
	snapshot := rolloutSnapshot{
		{
			InitialOldReplicas: 8, ActiveOldSpecReplicas: 6, OldSpecReplicas: 6,
			NewSpecReplicas: 3, NewCommittedReadyReplicas: 0, NewTargetReplicas: 8,
			Config: RollingUpdateConfig{MaxSurge: 2, MaxUnavailable: 2},
		},
		{
			InitialOldReplicas: 4, ActiveOldSpecReplicas: 3, OldSpecReplicas: 3,
			NewSpecReplicas: 2, NewCommittedReadyReplicas: 0, NewTargetReplicas: 4,
			Config: RollingUpdateConfig{MaxSurge: 2, MaxUnavailable: 2},
		},
	}
	assert.Equal(t, RoleReplicaState{4, 2}, hardNewReplicaLimits(snapshot))

	snapshot[0].OldSpecReplicas = 5
	snapshot[0].NewCommittedReadyReplicas = 1
	snapshot[1].OldSpecReplicas = 2
	snapshot[1].NewCommittedReadyReplicas = 1
	assert.Equal(t, RoleReplicaState{5, 3}, hardNewReplicaLimits(snapshot))

	// Once a role has no old Spec left, waiting for more target readiness cannot
	// protect old availability for that role. Surge remains a hard bound, but the
	// rest of the target may be issued immediately.
	snapshot[0].OldSpecReplicas = 0
	snapshot[0].NewSpecReplicas = 3
	snapshot[0].NewCommittedReadyReplicas = 0
	assert.Equal(t, 8, hardNewReplicaLimits(snapshot)[0])
}

func TestComputeAllStepsCompletes(t *testing.T) {
	for _, tc := range []struct {
		name               string
		initial, target    []int
		surge, unavailable []int
	}{
		{"asymmetric", []int{10, 2}, []int{6, 8}, []int{2, 2}, []int{0, 0}},
		{"zero surge", []int{4, 4}, []int{4, 4}, []int{0, 0}, []int{1, 1}},
		{"singleton role bootstrap", []int{1, 5}, []int{1, 5}, []int{0, 0}, []int{1, 1}},
		{"three roles", []int{6, 3, 2}, []int{6, 3, 2}, []int{1, 1, 1}, []int{0, 0, 0}},
		{"add role", []int{4, 4, 0}, []int{4, 4, 4}, []int{1, 1, 1}, []int{0, 0, 0}},
		{"remove role", []int{4, 4, 4}, []int{4, 4, 0}, []int{1, 1, 1}, []int{0, 0, 0}},
		{"disjoint roles", []int{4, 0, 0}, []int{0, 2, 2}, []int{1, 1, 1}, []int{0, 0, 0}},
		{"disjoint singleton targets", []int{2, 0, 0}, []int{0, 1, 1}, []int{1, 1, 1}, []int{0, 0, 0}},
		{"extreme imbalance", []int{1, 2, 10, 50}, []int{1, 2, 10, 50}, []int{1, 1, 1, 1}, []int{0, 0, 0, 0}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertPlannerRolloutInvariants(t, tc.initial, tc.target, configs(tc.surge, tc.unavailable))
		})
	}
}

func assertPlannerRolloutInvariants(
	t *testing.T,
	initial, target []int,
	config []RollingUpdateConfig,
) {
	t.Helper()
	steps := ComputeAllSteps(initial, target, config)
	require.NotEmpty(t, steps)
	last := steps[len(steps)-1]
	assert.Equal(t, make(RoleReplicaState, len(initial)), last.Past)
	assert.Equal(t, RoleReplicaState(target), last.New)

	bootstrapActive := make([]bool, len(initial))
	for stepIndex, current := range steps {
		for role := range initial {
			total := current.Past[role] + current.New[role]
			ceiling := max(initial[role], target[role]) + config[role].MaxSurge
			if current.UsesBootstrapSurge && total > ceiling {
				bootstrapActive[role] = true
			}
			if total <= ceiling {
				bootstrapActive[role] = false
			}
			if bootstrapActive[role] {
				ceiling++
			}
			floor := max(0, min(initial[role], target[role])-config[role].MaxUnavailable)
			assert.LessOrEqual(t, total, ceiling, "step %d role %d exceeds surge", stepIndex, role)
			assert.GreaterOrEqual(t, total, floor, "step %d role %d crosses availability", stepIndex, role)
			if stepIndex > 0 {
				previous := steps[stepIndex-1]
				assert.LessOrEqual(t, current.Past[role], previous.Past[role])
				assert.GreaterOrEqual(t, current.New[role], previous.New[role])
			}
		}
		oldProgress := make(RoleReplicaState, len(initial))
		for role := range initial {
			oldProgress[role] = initial[role] - current.Past[role]
		}
		assertProgressWithinFractionalWindow(t, initial, oldProgress, stepIndex, "old")
		assertProgressWithinFractionalWindow(t, target, current.New, stepIndex, "new")
	}
}

func assertProgressWithinFractionalWindow(
	t *testing.T,
	roleReplicaCounts, progress RoleReplicaState,
	stepIndex int,
	side string,
) {
	t.Helper()
	minProgress, maxProgress := 1.0, 0.0
	minPositiveRoleReplicaCount := 0
	for i, roleReplicaCount := range roleReplicaCounts {
		if roleReplicaCount <= 0 {
			continue
		}
		roleProgress := float64(progress[i]) / float64(roleReplicaCount)
		minProgress = min(minProgress, roleProgress)
		maxProgress = max(maxProgress, roleProgress)
		if minPositiveRoleReplicaCount == 0 || roleReplicaCount < minPositiveRoleReplicaCount {
			minPositiveRoleReplicaCount = roleReplicaCount
		}
	}
	if minPositiveRoleReplicaCount == 0 {
		return
	}
	assert.LessOrEqual(t,
		maxProgress-minProgress,
		1.0/float64(minPositiveRoleReplicaCount)+1e-9,
		"step %d exceeds the %s-side fractional window", stepIndex, side,
	)
}

func TestRevisionAwarePlannerFeasibilityOracle(t *testing.T) {
	rng := rand.New(rand.NewSource(907))
	rawReadyRNG := rand.New(rand.NewSource(908))
	for scenario := range 60000 {
		initial, activeSpec, activeReady := make(RoleReplicaState, 2), make(RoleReplicaState, 2), make(RoleReplicaState, 2)
		parkedSpec, parkedReady := make(RoleReplicaState, 2), make(RoleReplicaState, 2)
		newSpec, newReady, target := make(RoleReplicaState, 2), make(RoleReplicaState, 2), make(RoleReplicaState, 2)
		activeRawReady, parkedRawReady, newRawReady := make(RoleReplicaState, 2), make(RoleReplicaState, 2), make(RoleReplicaState, 2)
		config := make([]RollingUpdateConfig, 2)
		for role := range 2 {
			initial[role] = rng.Intn(5)
			current := 0
			if initial[role] > 0 {
				current = rng.Intn(initial[role] + 1)
			}
			activeSpec[role] = current
			activeReady[role] = rng.Intn(current + 1)
			parkedSpec[role] = rng.Intn(3)
			parkedReady[role] = rng.Intn(parkedSpec[role] + 1)
			target[role] = rng.Intn(5)
			if target[role] > 0 {
				newSpec[role] = rng.Intn(target[role] + 1)
			}
			newReady[role] = rng.Intn(newSpec[role] + 1)
			config[role] = RollingUpdateConfig{MaxSurge: rng.Intn(3), MaxUnavailable: rng.Intn(3)}
		}
		// Preserve the original corpus and add a disjoint A-to-B population,
		// including random parked revisions and interrupted availability baselines.
		if scenario >= 50000 {
			initial[1], activeSpec[1], activeReady[1] = 0, 0, 0
			target[0], newSpec[0], newReady[0] = 0, 0, 0
		}
		for role := range 2 {
			activeRawReady[role] = activeReady[role] + rawReadyRNG.Intn(activeSpec[role]-activeReady[role]+1)
			parkedRawReady[role] = parkedReady[role] + rawReadyRNG.Intn(parkedSpec[role]-parkedReady[role]+1)
			newRawReady[role] = newReady[role] + rawReadyRNG.Intn(newSpec[role]-newReady[role]+1)
		}
		state := rolloutState(initial, activeSpec, activeReady, parkedSpec, parkedReady, newSpec, newReady, target, config)
		state.ActiveOld.RawReadyReplicas = activeRawReady
		state.ParkedOld[0].RawReadyReplicas = parkedRawReady
		state.Target.RawReadyReplicas = newRawReady

		step := ComputeNextStep(state)
		feasible := hasFeasibleMutation(state)
		require.Equal(t, feasible, step != nil, "scenario %d: step=%v state=%+v", scenario, step, state)
		if step != nil {
			require.NoError(t, validateUpdateStep(state, step), "scenario %d: step=%v state=%+v", scenario, step, state)
		}

		if config[0].MaxSurge+config[0].MaxUnavailable > 0 && config[1].MaxSurge+config[1].MaxUnavailable > 0 {
			steps := ComputeAllSteps(initial, target, config)
			last := steps[len(steps)-1]
			require.Equal(t, make(RoleReplicaState, len(initial)), last.Past,
				"scenario %d did not drain: initial=%v target=%v config=%v", scenario, initial, target, config)
			require.Equal(t, target, last.New,
				"scenario %d did not reach its target: initial=%v target=%v config=%v", scenario, initial, target, config)
		}
	}
}

func hasFeasibleMutation(state RolloutState) bool {
	snapshot := snapshotForRolloutState(state)
	phaseTargets := targetReplicasForActiveRevision(snapshot)
	for old0 := 0; old0 <= state.ActiveOld.SpecReplicas[0]; old0++ {
		for old1 := 0; old1 <= state.ActiveOld.SpecReplicas[1]; old1++ {
			old := RoleReplicaState{old0, old1}
			if !slices.Equal(boundOldTargetsByRevisionCompleteness(state.ActiveOld.SpecReplicas, old, state.ActiveOld.RequiredRoles), old) ||
				!availabilityPreserved(snapshot, old, state.ActiveOld.RequiredRoles) ||
				!slices.Equal(boundDrainingRoleTargetsToWindow(state.ActiveOld.SpecReplicas, state.ActiveOld.InitialReplicas, old), old) {
				continue
			}
			if !slices.Equal(old, state.ActiveOld.SpecReplicas) {
				return true
			}
		}
	}
	hasNewMutation := func(newLimits RoleReplicaState) bool {
		for new0 := state.Target.SpecReplicas[0]; new0 <= min(phaseTargets[0], newLimits[0]); new0++ {
			for new1 := state.Target.SpecReplicas[1]; new1 <= min(phaseTargets[1], newLimits[1]); new1++ {
				newTarget := RoleReplicaState{new0, new1}
				if slices.Equal(boundGrowingRoleTargetsToWindow(state.Target.SpecReplicas, phaseTargets, newTarget), newTarget) &&
					!slices.Equal(newTarget, state.Target.SpecReplicas) {
					return true
				}
			}
		}
		return false
	}
	if hasNewMutation(hardNewReplicaLimits(snapshot)) {
		return true
	}
	bootstrap, ok := snapshotWithBootstrapSurge(snapshot, phaseTargets)
	return ok && hasNewMutation(hardNewReplicaLimits(bootstrap))
}

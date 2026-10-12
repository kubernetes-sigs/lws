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
	"context"
	"fmt"
	"math/rand"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	disaggregatedsetv1 "sigs.k8s.io/lws/api/disaggregatedset/v1"
	leaderworkersetv1 "sigs.k8s.io/lws/api/leaderworkerset/v1"
	disaggregatedsetutils "sigs.k8s.io/lws/pkg/utils/disaggregatedset"
)

func TestScalingDuringRolloutPreservesDisjointReplacementCredit(t *testing.T) {
	// A100 has already retired 95 replicas using B's 95% Ready fraction.
	// B's own unavailable budget cannot spend that replacement credit again.
	state := rolloutState([]int{100, 0}, []int{5, 0}, []int{5, 0}, nil, nil,
		[]int{0, 110}, []int{0, 95}, []int{0, 100}, configs([]int{1, 1}, []int{0, 10}))
	state.ScaleDuringRollout = true
	assert.Nil(t, ComputeNextStep(state), "all 95 replacement Ready replicas still preserve A's floor")
	assert.Error(t, validateUpdateStep(state, &UpdateStep{
		Past: []int{5, 0}, New: []int{0, 105},
	}), "shrinking B could leave only 90 Ready plus A5 against the shared baseline of 100")
}

func TestMovingTargetKeepsDisjointStartupBounds(t *testing.T) {
	for _, tc := range []struct {
		name               string
		old, issued, ready []int
		wantOld, wantNew   []int
		rebalance          bool
	}{
		{"drain before using released capacity", []int{4, 4, 0, 0}, []int{0, 0, 0, 0}, []int{0, 0, 0, 0}, []int{3, 3, 0, 0}, []int{0, 0, 0, 0}, false},
		{"start missing role first", []int{3, 3, 0, 0}, []int{0, 0, 1, 0}, []int{0, 0, 1, 0}, []int{3, 3, 0, 0}, []int{0, 0, 1, 1}, false},
		{"unready pair cannot spend another old group", []int{3, 3, 0, 0}, []int{0, 0, 1, 1}, []int{0, 0, 1, 0}, nil, nil, false},
		{"repair earlier unready surplus", []int{3, 3, 0, 0}, []int{0, 0, 3, 1}, []int{0, 0, 0, 0}, []int{3, 3, 0, 0}, []int{0, 0, 1, 1}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state := rolloutState([]int{4, 4, 0, 0}, tc.old, tc.old, nil, nil, tc.issued, tc.ready,
				[]int{0, 0, 4, 4}, configs([]int{0, 0, 0, 0}, []int{1, 1, 1, 1}))
			state.ScaleDuringRollout = true
			state.Target.UnschedulableRoles = []bool{false, false, true, true}
			step := ComputeNextStep(state)
			if tc.wantNew == nil {
				require.Nil(t, step)
				return
			}
			require.NotNil(t, step)
			assert.Equal(t, tc.wantOld, step.Past)
			assert.Equal(t, tc.wantNew, step.New)
			assert.Equal(t, tc.rebalance, step.UsesTargetRebalance)
			assert.False(t, step.UsesBootstrapSurge)
			assert.False(t, step.UsesUnavailableFallback)
			require.NoError(t, validateUpdateStep(state, step))
		})
	}
}

func TestMovingTargetKeepsRemovedRoleCoordination(t *testing.T) {
	// A removed role's desired zero is not a new capacity target. Its larger
	// saved unavailable budget must not bypass the old rollout fraction window.
	for _, advance := range []bool{false, true} {
		state := rolloutState([]int{10, 10, 0, 0}, []int{10, 10, 0, 0}, []int{10, 10, 0, 0}, nil, nil,
			[]int{0, 0, 0, 0}, []int{0, 0, 0, 0}, []int{0, 0, 10, 10}, configs([]int{1, 1, 0, 0}, []int{9, 1, 1, 1}))
		state.ScaleDuringRollout = advance
		step := ComputeNextStep(state)
		require.NotNil(t, step)
		assert.Equal(t, []int{8, 9, 0, 0}, step.Past, "advance=%v", advance)
		assert.Equal(t, []int{0, 0, 0, 0}, step.New)
		require.NoError(t, validateUpdateStep(state, step))
	}
}

func TestDisjointTargetCorrectionReadiness(t *testing.T) {
	for _, tc := range []struct {
		name                               string
		oldRaw, oldReady, newRaw, newReady int
		oldUnavailable, targetUnavailable  int
		parked                             int
		want                               int
	}{
		{"target budget cannot reuse replacement credit", 5, 5, 95, 95, 0, 10, 0, 110},
		{"source unavailable budget permits correction", 5, 5, 95, 95, 10, 10, 0, 105},
		{"surplus target readiness permits correction", 5, 5, 100, 100, 0, 10, 0, 105},
		{"already degraded source does not demand recovery", 5, 5, 80, 80, 0, 30, 0, 110},
		{"pending source deletion reserves credit", 10, 5, 95, 95, 0, 10, 0, 110},
		{"pending target deletion reserves credit", 5, 5, 100, 95, 0, 10, 0, 110},
		{"pending old loss does not freeze unready target excess", 100, 90, 0, 0, 0, 10, 0, 100},
		{"pending source deletion does not impose dummy full floor", 50, 40, 100, 100, 0, 50, 0, 100},
		{"parked readiness is not target replacement credit", 5, 5, 95, 95, 0, 10, 5, 105},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state := rolloutState([]int{100, 0}, []int{tc.oldReady, 0}, []int{tc.oldReady, 0}, nil, nil,
				[]int{0, 110}, []int{0, tc.newReady}, []int{0, 100}, configs([]int{1, 1}, []int{tc.oldUnavailable, tc.targetUnavailable}))
			state.ScaleDuringRollout = true
			state.ActiveOld.RawReadyReplicas = []int{tc.oldRaw, 0}
			state.Target.RawReadyReplicas = []int{0, tc.newRaw}
			if tc.parked > 0 {
				state.ParkedOld = []ParkedRevisionState{{RequiredRoles: []bool{true, true},
					SpecReplicas: []int{tc.parked, tc.parked}, RawReadyReplicas: []int{tc.parked, tc.parked}, ReadyReplicas: []int{tc.parked, tc.parked}}}
			}
			assert.Equal(t, []int{0, tc.want}, furthestTargetScaleDownTargets(state, snapshotForRolloutState(state)))
			assert.True(t, targetScaleDownPreservesAvailability(state, []int{0, tc.want}))
			if tc.want > 100 {
				assert.False(t, targetScaleDownPreservesAvailability(state, []int{0, tc.want - 1}), "one extra deletion must not spend reserved Ready credit")
			}
		})
	}
}

func TestDisjointTargetCorrectionKeepsWholeRevisionFraction(t *testing.T) {
	state := rolloutState([]int{3, 0, 0}, []int{1, 0, 0}, []int{1, 0, 0}, nil, nil,
		[]int{0, 4, 4}, []int{0, 2, 3}, []int{0, 2, 3}, configs([]int{1, 1, 1}, []int{0, 1, 1}))
	state.ScaleDuringRollout = true
	assert.Equal(t, []int{0, 4, 3}, furthestTargetScaleDownTargets(state, snapshotForRolloutState(state)))
	assert.True(t, targetScaleDownPreservesAvailability(state, []int{0, 4, 3}))
	assert.False(t, targetScaleDownPreservesAvailability(state, []int{0, 3, 3}), "B must retain ceil(2*2/3)=2 Ready; B1 replaces only one A")
}

func TestDisjointTargetCorrectionKeepsResidualSourceRole(t *testing.T) {
	// A is now desired zero, but this target still supplies five required A
	// replicas until B replaces more than 90% of the shared A100 baseline.
	state := rolloutState([]int{100, 0}, []int{5, 0}, []int{5, 0}, nil, nil,
		[]int{10, 110}, []int{10, 90}, []int{0, 100}, configs([]int{1, 1}, []int{0, 30}))
	state.ScaleDuringRollout = true
	assert.Equal(t, []int{5, 110}, furthestTargetScaleDownTargets(state, snapshotForRolloutState(state)))
	assert.True(t, targetScaleDownPreservesAvailability(state, []int{5, 110}))
	assert.False(t, targetScaleDownPreservesAvailability(state, []int{4, 110}))
}

func TestScalingDuringRolloutDecisions(t *testing.T) {
	for _, tc := range []struct {
		name                                   string
		old, oldReady, current, ready, desired []int
		initial                                []int
		surge, unavailable                     []int
		wantOld, wantNew                       []int
		bootstrap                              bool
		allUnscheduled                         []bool
	}{
		{
			name: "old first", old: []int{4, 4}, oldReady: []int{4, 4},
			current: []int{5, 5}, ready: []int{5, 5}, desired: []int{4, 4},
			wantOld: []int{0, 0}, wantNew: []int{5, 5},
		}, {
			name: "unknown scheduling preserves final target", old: []int{4, 4}, oldReady: []int{4, 4},
			current: []int{5, 5}, ready: []int{0, 0}, desired: []int{4, 4},
			wantOld: []int{4, 4}, wantNew: []int{4, 4},
		}, {
			name: "all unscheduled absorbs excess", old: []int{4, 4}, oldReady: []int{4, 4},
			current: []int{5, 5}, ready: []int{0, 0}, desired: []int{4, 4},
			wantOld: []int{4, 4}, wantNew: []int{1, 1}, allUnscheduled: []bool{true, true},
		}, {
			name: "scheduling protection is per role", old: []int{4, 4}, oldReady: []int{4, 4},
			current: []int{5, 5}, ready: []int{0, 0}, desired: []int{4, 4},
			wantOld: []int{4, 4}, wantNew: []int{1, 4}, allUnscheduled: []bool{true, false},
		}, {
			name:    "exact target after old drains",
			current: []int{5, 3}, ready: []int{5, 3}, desired: []int{4, 2}, wantNew: []int{4, 2},
		}, {
			name:    "a zero target really reaches zero",
			current: []int{2, 3}, ready: []int{2, 3}, desired: []int{0, 0}, wantNew: []int{0, 0},
		}, {
			name:    "first unready role cannot hide a later drain",
			current: []int{1, 5}, ready: []int{0, 4}, desired: []int{1, 4},
		}, {
			name:    "per-role readiness remains independent",
			current: []int{1, 5}, ready: []int{0, 5}, desired: []int{1, 4}, wantNew: []int{1, 4},
		}, {
			name:    "growth creates no readiness credit",
			current: []int{1, 5}, ready: []int{0, 4}, desired: []int{2, 4}, wantNew: []int{2, 5},
		}, {
			name:    "opposing target changes",
			current: []int{4, 8}, ready: []int{4, 8}, desired: []int{8, 4}, wantNew: []int{8, 4},
		}, {
			name: "raised floor preserves old ready", old: []int{4, 4}, oldReady: []int{4, 4},
			current: []int{1, 1}, ready: []int{1, 1}, desired: []int{8, 8},
			wantOld: []int{4, 4}, wantNew: []int{2, 2},
		}, {
			name: "grow while previous batch is unready", old: []int{8, 8}, oldReady: []int{8, 8},
			current: []int{2, 2}, ready: []int{0, 0}, desired: []int{12, 12},
			surge: []int{2, 2}, unavailable: []int{2, 2}, wantOld: []int{8, 8}, wantNew: []int{4, 4},
		}, {
			name: "one emergency coordination slot", initial: []int{2, 3}, old: []int{2, 2}, oldReady: []int{2, 2},
			current: []int{4, 2}, ready: []int{4, 2}, desired: []int{6, 4},
			surge: []int{2, 0}, unavailable: []int{0, 2}, wantOld: []int{2, 2}, wantNew: []int{6, 3}, bootstrap: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.old == nil {
				tc.old, tc.oldReady = []int{0, 0}, []int{0, 0}
			}
			if tc.wantOld == nil {
				tc.wantOld = tc.old
			}
			if tc.surge == nil {
				tc.surge = []int{1, 1}
			}
			if tc.unavailable == nil {
				tc.unavailable = []int{0, 0}
			}
			state := rolloutState(tc.old, tc.old, tc.oldReady, nil, nil, tc.current, tc.ready, tc.desired, configs(tc.surge, tc.unavailable))
			if tc.initial != nil {
				state.ActiveOld.InitialReplicas = tc.initial
			}
			state.ScaleDuringRollout = true
			state.Target.AllUnscheduled = tc.allUnscheduled
			step := ComputeNextStep(state)
			if tc.wantNew == nil {
				require.Nil(t, step)
				return
			}
			require.NotNil(t, step)
			assert.Equal(t, tc.wantOld, step.Past)
			assert.Equal(t, tc.wantNew, step.New)
			assert.Equal(t, tc.bootstrap, step.UsesBootstrapSurge)
			require.NoError(t, validateUpdateStep(state, step))
			assertScalingSafety(t, state, step)
		})
	}
}

func TestTargetCorrectionPreservesScheduledWork(t *testing.T) {
	state := rolloutState([]int{4}, []int{4}, []int{4}, nil, nil,
		[]int{5}, []int{0}, []int{4}, configs([]int{1}, []int{0}))
	state.ScaleDuringRollout = true
	for _, knownUnscheduled := range [][]bool{nil, {false}, {true}} {
		state.Target.AllUnscheduled = knownUnscheduled
		step := ComputeNextStep(state)
		require.NotNil(t, step)
		want := 4
		if len(knownUnscheduled) > 0 && knownUnscheduled[0] {
			want = 1
		}
		assert.Equal(t, []int{want}, step.New)
		require.NoError(t, validateUpdateStep(state, step))
		assert.Error(t, validateUpdateStep(state, &UpdateStep{Past: []int{4}, New: []int{want - 1}}),
			"the executor must independently reject a lower target")
	}
	// Readiness contradicts an all-unscheduled test/stub observation. Protect
	// startup work even when availability alone would permit the correction.
	for _, retained := range []int{0, 1} {
		state.Target.RawReadyReplicas, state.Target.ReadyReplicas = []int{1}, []int{retained}
		assert.Equal(t, []int{4}, furthestTargetScaleDownTargets(state, snapshotForRolloutState(state)))
		assert.Error(t, validateUpdateStep(state, &UpdateStep{Past: []int{4}, New: []int{1}}))
	}
	state.Target.RawReadyReplicas, state.Target.ReadyReplicas = []int{0}, []int{0}
	state.Target.SpecReplicas = []int{4}
	assert.Nil(t, ComputeNextStep(state), "even all-unscheduled work is not retracted at or below desired")
	state.Target.SpecReplicas = []int{5}
	state.ScaleDuringRollout = false
	assert.Nil(t, ComputeNextStep(state), "RolloutCoupled does not gain target corrections")
}

func TestScalingDuringRolloutObservesScheduledGroups(t *testing.T) {
	for _, identity := range []leaderworkersetv1.GroupIdentityType{leaderworkersetv1.GroupIdentityOrdinal, leaderworkersetv1.GroupIdentityHash} {
		for _, tc := range []struct {
			name string
			size int32
			want int32
		}{
			{"all unbound", 1, 1}, {"all bound", 1, 4}, {"mixed", 1, 4},
			{"all unbound workers", 2, 1}, {"bound worker", 2, 4},
			{"missing worker set", 2, 4}, {"missing worker Pod", 2, 4}, {"replaced worker set", 2, 4},
			{"lagging workers", 2, 4}, {"terminating worker", 2, 4},
			{"missing group", 1, 4}, {"excess group", 1, 4}, {"terminating group", 1, 4},
			{"missing native", 1, 4}, {"replaced native", 1, 4},
			{"lagging native", 1, 4}, {"native count mismatch", 1, 4},
			{"lagging LWS", 1, 4}, {"lagging ReplicaSet", 1, 4}, {"ReplicaSet count mismatch", 1, 4},
		} {
			if identity != leaderworkersetv1.GroupIdentityHash && (tc.name == "lagging ReplicaSet" || tc.name == "ReplicaSet count mismatch") {
				continue
			}
			t.Run(fmt.Sprintf("%s/%s", identity, tc.name), func(t *testing.T) {
				ds := newTestDisaggregatedSet(makeRoleSpec(testRolePrefill, 4, corev1.PodSpec{}, intstr.FromInt(1), intstr.FromInt(0)))
				ds.Spec.ScalingPolicy = &disaggregatedsetv1.DisaggregatedSetScalingPolicy{DuringRollout: disaggregatedsetv1.ScalingDuringRolloutPolicyAdvanceRollout}
				old := revisionLWS("old", testRolePrefill, 4, 4, time.Unix(1000, 0), 4)
				target := revisionLWS("target", testRolePrefill, 5, 0, time.Unix(2000, 0), 5)
				old.Spec.GroupIdentity, target.Spec.GroupIdentity = identity, identity
				old.Spec.LeaderWorkerTemplate.Size, target.Spec.LeaderWorkerTemplate.Size = ptr.To[int32](1), ptr.To(tc.size)
				objects := replicaGroupObjects(target, 5, 0)
				if tc.name == "excess group" {
					objects = replicaGroupObjects(target, 6, 0)
				}
				for _, object := range slices.Clone(objects) {
					leader, ok := object.(*corev1.Pod)
					if !ok {
						continue
					}
					leader.Status.Phase = corev1.PodPending // Pending can still have a node.
					if tc.name == "all bound" || tc.name == "mixed" && leader.Name == target.Name+"-0" {
						leader.Spec.NodeName = "node"
					}
					if tc.name == "terminating group" {
						leader.DeletionTimestamp, leader.Finalizers = ptr.To(metav1.Now()), []string{"test/hold"}
					}
					if tc.size == 1 {
						continue
					}
					leader.Annotations[leaderworkersetv1.SizeAnnotationKey] = "2"
					if tc.name == "missing worker set" {
						continue
					}
					workers := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{
						Name: leader.Name, Namespace: leader.Namespace, UID: types.UID(leader.Name + "-workers"), Generation: 1,
						Labels:          map[string]string{leaderworkersetv1.SetNameLabelKey: target.Name},
						OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(leader, corev1.SchemeGroupVersion.WithKind("Pod"))}},
						Spec: appsv1.StatefulSetSpec{Replicas: ptr.To[int32](1)}, Status: appsv1.StatefulSetStatus{ObservedGeneration: 1}}
					if tc.name == "lagging workers" {
						workers.Generation++
					}
					objects = append(objects, workers)
					if tc.name == "missing worker Pod" {
						continue
					}
					worker := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
						Name: leader.Name + "-1", Namespace: leader.Namespace, UID: types.UID(leader.Name + "-worker"),
						Labels:          map[string]string{leaderworkersetv1.SetNameLabelKey: target.Name, leaderworkersetv1.WorkerIndexLabelKey: "1"},
						OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(workers, appsv1.SchemeGroupVersion.WithKind("StatefulSet"))}},
						Status: corev1.PodStatus{Phase: corev1.PodPending}}
					if tc.name == "bound worker" {
						worker.Spec.NodeName = "node"
					}
					if tc.name == "terminating worker" {
						worker.DeletionTimestamp, worker.Finalizers = ptr.To(metav1.Now()), []string{"test/hold"}
					}
					if tc.name == "replaced worker set" {
						workers.UID += "-replacement"
					}
					objects = append(objects, worker)
				}
				switch tc.name {
				case "missing group":
					objects = objects[:len(objects)-1]
				case "missing native":
					objects = slices.Delete(objects, 1, 2)
				case "replaced native":
					objects[1].SetUID("replacement")
				case "lagging native":
					objects[1].SetGeneration(2)
				case "native count mismatch":
					switch native := objects[1].(type) {
					case *appsv1.StatefulSet:
						native.Spec.Replicas = ptr.To[int32](6)
					case *appsv1.Deployment:
						native.Spec.Replicas = ptr.To[int32](6)
					}
				case "lagging LWS":
					target.Generation++
				case "lagging ReplicaSet":
					objects[2].SetGeneration(2)
				case "ReplicaSet count mismatch":
					objects[2].(*appsv1.ReplicaSet).Spec.Replicas = ptr.To[int32](4)
				}
				c := newTestClient(append(objects, replicaGroupObjects(old, 4, 4)...)...)
				executor := newTestExecutor(c)
				executor.LWSManager = NewLeaderWorkerSetManager(c) // Exercise the real shared observer and adapter.
				lists, gets := 0, 0
				executor.LWSManager.apiReader = interceptor.NewClient(c, interceptor.Funcs{
					List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
						lists++
						return c.List(ctx, list, opts...)
					},
					Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
						gets++
						return c.Get(ctx, key, obj, opts...)
					},
				})
				observed, err := executor.LWSManager.observeRolloutReadiness(t.Context(), nil,
					disaggregatedsetutils.RevisionRoles{Roles: map[string]*leaderworkersetv1.LeaderWorkerSet{testRolePrefill: target}})
				require.NoError(t, err)
				assert.Equal(t, tc.want == 1, observed[target.Name].allUnscheduled)
				if tc.name == "all unbound" {
					wantLists := 2
					if identity == leaderworkersetv1.GroupIdentityHash {
						wantLists = 4
					}
					assert.Equal(t, wantLists, lists, "reuse existing descendant lists for scheduling facts")
					assert.Zero(t, gets, "no additional scheduling reads")
				}
				_, complete, err := reconcileTestTransition(t.Context(), executor, ds, "target", resolveDesiredReplicasByRole(ds, nil))
				require.NoError(t, err)
				assert.False(t, complete)
				assert.EqualValues(t, 4, getTestLWSReplicas(c, testNamespace, old.Name))
				assert.EqualValues(t, tc.want, getTestLWSReplicas(c, testNamespace, target.Name))
			})
		}
	}
}

func TestTargetDrainUsesRetainedNotRawReadiness(t *testing.T) {
	state := rolloutState([]int{0, 0}, []int{0, 0}, []int{0, 0}, nil, nil,
		[]int{5, 5}, []int{4, 4}, []int{3, 3}, configs([]int{1, 1}, []int{0, 0}))
	state.Target.RawReadyReplicas = []int{5, 5} // One victim per role is already reserved.
	state.ScaleDuringRollout = true
	step := ComputeNextStep(state)
	require.NotNil(t, step)
	assert.Equal(t, []int{4, 4}, step.New)
	require.NoError(t, validateUpdateStep(state, step))
	require.Error(t, validateUpdateStep(state, &UpdateStep{Past: []int{0, 0}, New: []int{3, 3}}))

	state.ScaleDuringRollout = false
	assert.Nil(t, ComputeNextStep(state), "default remains growth-only")
	require.Error(t, validateUpdateStep(state, step), "the executor independently rejects opt-out target shrink")
}

func TestMovingTargetUnschedulableFallbackUsesLatestSurgeCeiling(t *testing.T) {
	state := rolloutState([]int{8, 8}, []int{4, 4}, []int{3, 3}, nil, nil,
		[]int{1, 1}, []int{0, 0}, []int{3, 3}, configs([]int{1, 1}, []int{0, 0}))
	state.ScaleDuringRollout = true
	require.Nil(t, ComputeNextStep(state), "wait for the issued target group rather than stacking emergency slots")
	// Old+target Spec is 5: above the latest ceiling 3+1, but below
	// the historical ceiling 8+1. Only scheduler rejection permits a fallback.
	state.Target.UnschedulableRoles = []bool{true, false}
	step := ComputeNextStep(state)
	require.NotNil(t, step)
	assert.True(t, step.UsesUnavailableFallback)
	assert.Equal(t, []int{3, 3}, step.Past)
	assert.Equal(t, []int{1, 1}, step.New)
	require.NoError(t, validateUpdateStep(state, step))
	assertScalingSafety(t, state, step)
}

func TestMovingTargetCoordinationBootstrapDoesNotStack(t *testing.T) {
	state := rolloutState([]int{2, 3}, []int{2, 2}, []int{2, 2}, nil, nil,
		[]int{4, 2}, []int{4, 2}, []int{6, 4}, configs([]int{2, 0}, []int{0, 2}))
	state.ScaleDuringRollout = true
	step := ComputeNextStep(state)
	require.NotNil(t, step)
	require.True(t, step.UsesBootstrapSurge)
	require.Equal(t, []int{6, 3}, step.New)

	// The decode role is now one above its ceiling. Neither repeated
	// reconciliations nor its new Pod becoming Ready grant a second slot;
	// an availability-safe drain may release exactly the existing excess.
	state.Target.SpecReplicas = slices.Clone(step.New)
	for _, ready := range [][]int{{4, 2}, {4, 3}} {
		state.Target.ReadyReplicas = ready
		state.Target.RawReadyReplicas = slices.Clone(ready)
		correction := ComputeNextStep(state)
		require.NotNil(t, correction)
		require.False(t, correction.UsesBootstrapSurge)
		require.Equal(t, state.Target.SpecReplicas, correction.New)
		require.Equal(t, []int{2, 1}, correction.Past, "release only the one excess old group")
		require.NoError(t, validateUpdateStep(state, correction))
		assertScalingSafety(t, state, correction)
	}
	// Once the other role catches up, ordinary retirement releases capacity.
	state.Target.ReadyReplicas, state.Target.RawReadyReplicas = []int{6, 3}, []int{6, 3}
	step = ComputeNextStep(state)
	require.NotNil(t, step)
	require.False(t, step.UsesBootstrapSurge)
	require.Equal(t, []int{0, 0}, step.Past)
	assertScalingSafety(t, state, step)
}

// This oracle does not call the production availability or validator helpers.
// Treat every newly deleted replica as Ready, remove unusable revisions, then
// check serving capacity and each structurally complete role independently.
func assertScalingSafety(t *testing.T, state RolloutState, step *UpdateStep) {
	t.Helper()
	type revision struct {
		required          []bool
		spec, ready, next []int
	}
	revisions := []revision{
		{state.ActiveOld.RequiredRoles, state.ActiveOld.SpecReplicas, state.ActiveOld.ReadyReplicas, step.Past},
		{state.Target.RequiredRoles, state.Target.SpecReplicas, state.Target.ReadyReplicas, step.New},
	}
	for _, parked := range state.ParkedOld {
		revisions = append(revisions, revision{parked.RequiredRoles, parked.SpecReplicas, parked.ReadyReplicas, parked.SpecReplicas})
	}
	n := len(state.Config)
	beforeServing, afterServing, beforeRole, afterRole := make([]int, n), make([]int, n), make([]int, n), make([]int, n)
	parkedServing := make([]int, n)
	for revisionIndex, rev := range revisions {
		for phase := range 2 {
			spec, ready := slices.Clone(rev.spec), slices.Clone(rev.ready)
			if phase == 1 {
				for i := range spec {
					spec[i] = min(spec[i], rev.next[i]) // New growth is never Ready yet.
					ready[i] = max(0, ready[i]-(rev.spec[i]-spec[i]))
				}
			}
			present, serving, required := true, true, false
			for i, needed := range rev.required {
				if needed {
					required = true
					present = present && spec[i] > 0
					serving = serving && ready[i] > 0
				}
			}
			for i := range spec {
				if present && required {
					if phase == 0 {
						beforeRole[i] += ready[i]
					} else {
						afterRole[i] += ready[i]
					}
				}
				if serving && required {
					if phase == 0 {
						beforeServing[i] += ready[i]
						if revisionIndex >= 2 {
							parkedServing[i] += ready[i]
						}
					} else {
						afterServing[i] += ready[i]
					}
				}
			}
		}
	}
	phaseTargets, growing, oldReference, oldProgress, oldNext := make([]int, n), make([]int, n), make([]int, n), make([]int, n), make([]int, n)
	scaleCorrection := make([]bool, n)
	steps := 0
	for i := range state.Config {
		phaseTargets[i] = state.Target.DesiredReplicas[i] - parkedServing[i]
		if state.Target.DesiredReplicas[i] > 0 {
			phaseTargets[i] = max(1, phaseTargets[i])
		}
		phaseTargets[i] = max(state.Target.SpecReplicas[i], phaseTargets[i])
		growing[i] = max(state.Target.SpecReplicas[i], step.New[i])
		oldReference[i] = state.ActiveOld.InitialReplicas[i]
		if state.ScaleDuringRollout && state.Target.DesiredReplicas[i] > 0 {
			physical := state.ActiveOld.SpecReplicas[i] + state.Target.SpecReplicas[i]
			for _, parked := range state.ParkedOld {
				physical += parked.SpecReplicas[i]
			}
			excess := max(0, physical-state.Target.DesiredReplicas[i]-state.Config[i].MaxSurge)
			scaleCorrection[i] = state.ActiveOld.SpecReplicas[i]-step.Past[i] <= excess
		}
		oldProgress[i] = oldReference[i] - state.ActiveOld.SpecReplicas[i]
		oldNext[i] = oldReference[i] - step.Past[i]
		steps = max(steps, state.ActiveOld.InitialReplicas[i], state.Target.DesiredReplicas[i])
	}
	assertMovingWindow(t, state.Target.SpecReplicas, phaseTargets, growing, nil)
	assertMovingWindow(t, oldProgress, oldReference, oldNext, scaleCorrection)
	for i, config := range state.Config {
		if !step.UsesTargetRebalance && (i >= len(state.Target.AllUnscheduled) || !state.Target.AllUnscheduled[i] ||
			state.Target.RawReadyReplicas[i] > 0 || state.Target.ReadyReplicas[i] > 0) {
			require.GreaterOrEqual(t, step.New[i], min(state.Target.SpecReplicas[i], state.Target.DesiredReplicas[i]), "preserve scheduled or unknown work")
		}
		floor := max(0, state.Target.DesiredReplicas[i]-config.MaxUnavailable)
		if step.UsesUnavailableFallback && floor > 1 {
			floor--
		}
		require.GreaterOrEqual(t, afterServing[i], min(beforeServing[i], floor), "serving role %d: state=%+v step=%+v", i, state, step)
		// Draining another role cannot remove this role's physical replicas;
		// losing structural credit alone is covered by the serving check above.
		if step.Past[i] < state.ActiveOld.SpecReplicas[i] || step.New[i] < state.Target.SpecReplicas[i] {
			require.GreaterOrEqual(t, afterRole[i], min(beforeRole[i], floor), "per-role %d: state=%+v step=%+v", i, state, step)
		}
		old := state.ActiveOld.SpecReplicas[i]
		for _, parked := range state.ParkedOld {
			old += parked.SpecReplicas[i]
		}
		if step.New[i] > state.Target.SpecReplicas[i] {
			ceiling := state.Target.DesiredReplicas[i] + config.MaxSurge
			budget := config.MaxSurge + config.MaxUnavailable
			if step.UsesBootstrapSurge {
				ceiling = max(ceiling+1, old+1)
				budget = max(config.MaxSurge+1, old-state.Target.DesiredReplicas[i]+1) + config.MaxUnavailable
			}
			require.LessOrEqual(t, old+step.New[i], ceiling)
			if old > 0 {
				count := max(state.ActiveOld.InitialReplicas[i], state.Target.DesiredReplicas[i])
				allowance := (count*budget + steps - 1) / steps
				require.LessOrEqual(t, step.New[i], state.Target.ReadyReplicas[i]+allowance)
			}
		}
	}
}

// Independent fractional oracle: an existing skew is tolerated, but a role
// that advances may not pass the slowest proposed fraction plus one slot of
// the smallest positive role. Floating point is exact enough for these tiny
// test states, and intentionally differs from production integer arithmetic.
func assertMovingWindow(t *testing.T, current, counts, proposed []int, scaleCorrection []bool) {
	t.Helper()
	least, width := 1.0, 0.0
	for i, count := range counts {
		if count > 0 {
			least = min(least, float64(proposed[i])/float64(count))
			width = max(width, 1.0/float64(count))
		}
	}
	for i, count := range counts {
		if count > 0 && proposed[i] > current[i] && (scaleCorrection == nil || !scaleCorrection[i]) {
			require.LessOrEqual(t, float64(proposed[i])/float64(count), least+width+1e-9,
				"coordination current=%v counts=%v proposed=%v", current, counts, proposed)
		}
	}
}

func TestScalingDuringRolloutIndependentSafety(t *testing.T) {
	rng := rand.New(rand.NewSource(1023))
	for scenario := range 20000 {
		n := 2 + rng.Intn(2)
		initial := make([]int, n)
		for i := range initial {
			initial[i] = 5
		}
		state := rolloutState(initial, make([]int, n), make([]int, n), make([]int, n), make([]int, n),
			make([]int, n), make([]int, n), make([]int, n), make([]RollingUpdateConfig, n))
		state.ScaleDuringRollout = true
		state.Target.AllUnscheduled = make([]bool, n)
		for i := range n {
			state.ActiveOld.SpecReplicas[i] = rng.Intn(6)
			state.ParkedOld[0].SpecReplicas[i] = rng.Intn(4)
			state.Target.SpecReplicas[i] = rng.Intn(8)
			state.Target.DesiredReplicas[i] = rng.Intn(7)
			state.Config[i] = RollingUpdateConfig{MaxSurge: rng.Intn(3), MaxUnavailable: rng.Intn(3)}
			for _, counts := range []struct{ spec, raw, retained []int }{
				{state.ActiveOld.SpecReplicas, state.ActiveOld.RawReadyReplicas, state.ActiveOld.ReadyReplicas},
				{state.ParkedOld[0].SpecReplicas, state.ParkedOld[0].RawReadyReplicas, state.ParkedOld[0].ReadyReplicas},
				{state.Target.SpecReplicas, state.Target.RawReadyReplicas, state.Target.ReadyReplicas},
			} {
				counts.raw[i] = rng.Intn(counts.spec[i] + 1)
				counts.retained[i] = rng.Intn(counts.raw[i] + 1)
			}
			state.Target.AllUnscheduled[i] = state.Target.RawReadyReplicas[i] == 0 && rng.Intn(2) == 0
		}
		state.Target.RequiredRoles = requiredRoles(state.Target.DesiredReplicas)
		state.ParkedOld[0].RequiredRoles = requiredRoles(initial)
		if step := ComputeNextStep(state); step != nil {
			require.NoError(t, validateUpdateStep(state, step), "scenario %d state=%+v step=%+v", scenario, state, step)
			assertScalingSafety(t, state, step)
		}
	}
}

func TestScalingDuringRolloutDelayedConvergence(t *testing.T) {
	rng := rand.New(rand.NewSource(81259))
	for scenario := range 1000 {
		initial := []int{rng.Intn(6), rng.Intn(6)}
		state := rolloutState(initial, initial, initial, []int{1, 1}, []int{1, 1}, []int{0, 0}, []int{0, 0}, initial,
			configs([]int{rng.Intn(3), rng.Intn(3)}, []int{rng.Intn(3), rng.Intn(3)}))
		for i := range state.Config {
			if state.Config[i].MaxSurge+state.Config[i].MaxUnavailable == 0 {
				state.Config[i].MaxSurge = 1
			}
		}
		state.ScaleDuringRollout = true
		state.Target.AllUnscheduled = make([]bool, 2)
		for reconcile := range 120 {
			if reconcile < 4 {
				state.Target.DesiredReplicas = []int{rng.Intn(8), rng.Intn(8)}
				state.Target.RequiredRoles = requiredRoles(state.Target.DesiredReplicas)
			}
			// Native deletes and new readiness catch up separately. Raw Ready
			// stays stale for two reconciles after an issued drain.
			if reconcile%3 == 2 {
				state.ActiveOld.RawReadyReplicas = slices.Clone(state.ActiveOld.SpecReplicas)
				state.ActiveOld.ReadyReplicas = slices.Clone(state.ActiveOld.SpecReplicas)
				state.Target.RawReadyReplicas = slices.Clone(state.Target.SpecReplicas)
				state.Target.ReadyReplicas = slices.Clone(state.Target.SpecReplicas)
			}
			for i := range state.Target.AllUnscheduled {
				state.Target.AllUnscheduled[i] = state.Target.RawReadyReplicas[i] == 0 && rng.Intn(2) == 0
			}
			// The executor rebuilds baselines after target edits and retirement;
			// a removed role in an overlapping revision has no disjoint floor.
			var parkedSpec []int
			if len(state.ParkedOld) > 0 {
				parkedSpec = state.ParkedOld[0].SpecReplicas
			}
			state.AvailabilityBaseline = rolloutState(state.ActiveOld.InitialReplicas, state.ActiveOld.SpecReplicas,
				nil, parkedSpec, nil, nil, nil, state.Target.DesiredReplicas, nil).AvailabilityBaseline
			if step := ComputeNextStep(state); step != nil {
				require.NoError(t, validateUpdateStep(state, step), "scenario %d reconcile %d", scenario, reconcile)
				assertScalingSafety(t, state, step)
				for i := range 2 {
					state.ActiveOld.ReadyReplicas[i] = max(0, state.ActiveOld.ReadyReplicas[i]-(state.ActiveOld.SpecReplicas[i]-step.Past[i]))
					state.Target.ReadyReplicas[i] = max(0, state.Target.ReadyReplicas[i]-max(0, state.Target.SpecReplicas[i]-step.New[i]))
				}
				state.ActiveOld.SpecReplicas, state.Target.SpecReplicas = step.Past, step.New
			}
			if slices.Equal(state.ActiveOld.SpecReplicas, []int{0, 0}) && len(state.ParkedOld) > 0 {
				parked := state.ParkedOld[0]
				state.ActiveOld = ActiveRevisionState{RequiredRoles: parked.RequiredRoles, InitialReplicas: []int{1, 1},
					SpecReplicas: parked.SpecReplicas, RawReadyReplicas: parked.RawReadyReplicas, ReadyReplicas: parked.ReadyReplicas}
				state.ParkedOld = nil
			}
		}
		require.Equal(t, []int{0, 0}, state.ActiveOld.SpecReplicas, "scenario %d state=%+v", scenario, state)
		require.Empty(t, state.ParkedOld)
		require.Equal(t, state.Target.DesiredReplicas, state.Target.SpecReplicas, "scenario %d state=%+v", scenario, state)
	}
}

func TestScalingDuringRolloutPhysicalExcessDoesNotDeadlockFractions(t *testing.T) {
	state := rolloutState([]int{9, 2}, []int{5, 2}, []int{5, 2}, nil, nil,
		[]int{2, 5}, []int{2, 5}, []int{6, 10}, configs([]int{0, 1}, []int{1, 1}))
	state.ScaleDuringRollout = true
	step := ComputeNextStep(state)
	require.NotNil(t, step, "latest-scale excess must not deadlock old and target fractional windows")
	require.Equal(t, []int{4, 2}, step.Past, "only the one existing physical excess group may bypass coordination")
	require.Equal(t, state.Target.SpecReplicas, step.New)
	require.NoError(t, validateUpdateStep(state, step))
	assertScalingSafety(t, state, step)
	require.ErrorContains(t, validateUpdateStep(state, &UpdateStep{Past: []int{3, 2}, New: step.New}), "fractional coordination",
		"availability would permit two deletions, but only one is excess")
	state.ActiveOld.ReadyReplicas = []int{3, 2}
	require.Nil(t, ComputeNextStep(state), "physical excess is not permission to spend already-reserved Ready capacity")
	state.ActiveOld.ReadyReplicas = []int{5, 2}
	state.ScaleDuringRollout = false
	require.ErrorContains(t, validateUpdateStep(state, step), "fractional coordination", "RolloutCoupled retains its historical window")

	state = rolloutState([]int{1, 2}, []int{1, 2}, []int{1, 2}, nil, nil,
		[]int{3, 3}, []int{3, 3}, []int{2, 2}, configs([]int{0, 0}, []int{0, 0}))
	state.ScaleDuringRollout = true
	require.ErrorContains(t, validateUpdateStep(state, &UpdateStep{Past: []int{0, 1}, New: []int{3, 3}}), "required roles incomplete")
}

func TestScalingDuringRolloutFiveEditsReachPhysicalExcess(t *testing.T) {
	state := rolloutState([]int{9, 2}, []int{9, 2}, []int{9, 2}, nil, nil,
		[]int{0, 0}, []int{0, 0}, []int{9, 2}, configs([]int{0, 1}, []int{1, 1}))
	state.ScaleDuringRollout = true
	// Replay legal historical-window steps, proving the stalled snapshot is
	// reachable from a complete old revision rather than an invented state.
	for tick, change := range []struct{ desired, old, target []int }{
		{[]int{4, 10}, []int{5, 2}, []int{0, 2}},
		{[]int{8, 5}, []int{5, 2}, []int{1, 2}},
		{[]int{4, 7}, []int{5, 2}, []int{1, 3}},
		{[]int{7, 5}, []int{5, 2}, []int{2, 3}},
		{[]int{6, 10}, []int{5, 2}, []int{2, 4}},
		{[]int{6, 10}, []int{5, 2}, []int{2, 5}},
	} {
		state.Target.DesiredReplicas = change.desired
		if tick%3 == 2 {
			state.ActiveOld.RawReadyReplicas, state.ActiveOld.ReadyReplicas = slices.Clone(state.ActiveOld.SpecReplicas), slices.Clone(state.ActiveOld.SpecReplicas)
			state.Target.RawReadyReplicas, state.Target.ReadyReplicas = slices.Clone(state.Target.SpecReplicas), slices.Clone(state.Target.SpecReplicas)
		}
		step := &UpdateStep{Past: change.old, New: change.target}
		require.NoError(t, validateUpdateStep(state, step), "tick %d", tick)
		oldProgress, oldNext := make([]int, 2), make([]int, 2)
		for i := range 2 {
			oldProgress[i] = state.ActiveOld.InitialReplicas[i] - state.ActiveOld.SpecReplicas[i]
			oldNext[i] = state.ActiveOld.InitialReplicas[i] - change.old[i]
		}
		assertMovingWindow(t, oldProgress, state.ActiveOld.InitialReplicas, oldNext, nil)
		assertScalingSafety(t, state, step)
		for i := range 2 {
			state.ActiveOld.ReadyReplicas[i] = min(state.ActiveOld.ReadyReplicas[i], change.old[i])
		}
		state.ActiveOld.SpecReplicas, state.Target.SpecReplicas = change.old, change.target
	}
	state.Target.RawReadyReplicas, state.Target.ReadyReplicas = slices.Clone(state.Target.SpecReplicas), slices.Clone(state.Target.SpecReplicas)
	step := ComputeNextStep(state)
	require.NotNil(t, step)
	require.Equal(t, []int{4, 2}, step.Past)
	require.Equal(t, []int{2, 5}, step.New)
	require.NoError(t, validateUpdateStep(state, step))
	assertScalingSafety(t, state, step)
}

func TestScalingDuringRolloutRepeatedEditsConverge(t *testing.T) {
	for _, seed := range []int64{4041023, 9071105, 11371023} {
		rng := rand.New(rand.NewSource(seed))
		for trial := range 5000 {
			initial := []int{1 + rng.Intn(10), 1 + rng.Intn(10)}
			state := rolloutState(initial, initial, initial, nil, nil, []int{0, 0}, []int{0, 0}, initial,
				configs([]int{0, 1}, []int{1, 1}))
			state.ScaleDuringRollout = true
			history := []string{fmt.Sprintf("initial=%v", initial)}
			for tick := range 100 {
				if tick < 5 {
					state.Target.DesiredReplicas = []int{1 + rng.Intn(10), 1 + rng.Intn(10)}
					state.Target.RequiredRoles = requiredRoles(state.Target.DesiredReplicas)
				}
				if tick%3 == 2 {
					state.ActiveOld.RawReadyReplicas, state.ActiveOld.ReadyReplicas = slices.Clone(state.ActiveOld.SpecReplicas), slices.Clone(state.ActiveOld.SpecReplicas)
					state.Target.RawReadyReplicas, state.Target.ReadyReplicas = slices.Clone(state.Target.SpecReplicas), slices.Clone(state.Target.SpecReplicas)
				}
				step := ComputeNextStep(state)
				if tick < 15 {
					history = append(history, fmt.Sprintf("tick=%d desired=%v old=%v oldReady=%v target=%v targetReady=%v step=%+v", tick,
						state.Target.DesiredReplicas, state.ActiveOld.SpecReplicas, state.ActiveOld.ReadyReplicas, state.Target.SpecReplicas, state.Target.ReadyReplicas, step))
				}
				if step != nil {
					require.NoError(t, validateUpdateStep(state, step))
					assertScalingSafety(t, state, step)
					for i := range 2 {
						state.ActiveOld.ReadyReplicas[i] = max(0, state.ActiveOld.ReadyReplicas[i]-(state.ActiveOld.SpecReplicas[i]-step.Past[i]))
						state.Target.ReadyReplicas[i] = max(0, state.Target.ReadyReplicas[i]-max(0, state.Target.SpecReplicas[i]-step.New[i]))
					}
					state.ActiveOld.SpecReplicas, state.Target.SpecReplicas = step.Past, step.New
				}
			}
			if !slices.Equal(state.ActiveOld.SpecReplicas, []int{0, 0}) || !slices.Equal(state.Target.SpecReplicas, state.Target.DesiredReplicas) {
				t.Fatalf("reachable stall seed=%d trial=%d history=%v", seed, trial, history)
			}
		}
	}
}

func TestScalingDuringRolloutExecutorPolicies(t *testing.T) {
	for _, policy := range []*disaggregatedsetv1.DisaggregatedSetScalingPolicy{
		nil, {}, {DuringRollout: disaggregatedsetv1.ScalingDuringRolloutPolicyRolloutCoupled},
		{DuringRollout: disaggregatedsetv1.ScalingDuringRolloutPolicyAdvanceRollout},
	} {
		ds := newTwoRoleTestDisaggregatedSet([2]int32{4, 2}, [2]int{1, 1}, [2]int{})
		revision := disaggregatedsetutils.ComputeRevision(ds.Spec.Roles)
		ds.Spec.ScalingPolicy = policy
		assert.Equal(t, revision, disaggregatedsetutils.ComputeRevision(ds.Spec.Roles), "policy edits do not roll Pods")
		objects := revisionLWSObjects("old", [2]int32{}, [2]int32{}, [2]int32{5, 3}, time.Now())
		objects = append(objects, revisionLWSObjects(revision, [2]int32{5, 3}, [2]int32{5, 3}, [2]int32{5, 3}, time.Now())...)
		c := newTestClient(objects...)
		r := newTestReconciler(c)
		_, err := r.reconcileSlice(context.Background(), r.createRollingUpdateExecutor(), ds, 0, revision, resolveDesiredReplicasByRole(ds, nil))
		require.NoError(t, err)
		want := [2]int32{5, 3}
		if scalingDuringRolloutEnabled(ds) {
			want = [2]int32{4, 2}
		}
		assertRevisionReplicas(t, c, revision, want)
	}
}

func TestScalingDuringRolloutPrefersAnyOldDrainOverTargetShrink(t *testing.T) {
	now := time.Now()
	objects := revisionLWSObjects("hashB", [2]int32{1, 1}, [2]int32{1, 0}, [2]int32{1, 1}, now)
	objects = append(objects, revisionLWSObjects("hashA", [2]int32{2, 2}, [2]int32{2, 2}, [2]int32{2, 2}, now.Add(time.Hour))...)
	objects = append(objects, revisionLWSObjects("hashC", [2]int32{3, 3}, [2]int32{}, [2]int32{3, 3}, now.Add(2*time.Hour))...)
	c := newTestClient(objects...)
	ds := newTwoRoleTestDisaggregatedSet([2]int32{2, 2}, [2]int{1, 1}, [2]int{})
	ds.Spec.ScalingPolicy = &disaggregatedsetv1.DisaggregatedSetScalingPolicy{DuringRollout: disaggregatedsetv1.ScalingDuringRolloutPolicyAdvanceRollout}
	reconcileExistingForTest(t, newTestExecutor(c), ds, "hashC")
	assertRevisionReplicas(t, c, "hashA", [2]int32{2, 2})
	assertRevisionReplicas(t, c, "hashB", [2]int32{})
	assertRevisionReplicas(t, c, "hashC", [2]int32{3, 3})
}

func TestScalingDuringRolloutKeepsDefaultCandidatePreference(t *testing.T) {
	// B cannot drain, but can correct an earlier target surplus. A can drain
	// one Decode group. Only opt-in scaling should prefer A over B's repair.
	now := time.Now()
	objects := revisionLWSObjects("hashA", [2]int32{4, 4}, [2]int32{4, 4}, [2]int32{6, 4}, now)
	objects = append(objects, revisionLWSObjects("hashB", [2]int32{1, 1}, [2]int32{1, 0}, [2]int32{1, 1}, now.Add(time.Hour))...)
	objects = append(objects,
		revisionLWS("hashC", "new-prefill", 3, 0, now.Add(2*time.Hour), 4),
		revisionLWS("hashC", "new-decode", 1, 0, now.Add(2*time.Hour), 4))
	executor := newTestExecutor(newTestClient(objects...))
	ds := newTwoRoleTestDisaggregatedSet([2]int32{4, 4}, [2]int{1, 1}, [2]int{1, 1})
	old, target, err := executor.LWSManager.GetRevisionRolesList(context.Background(), ds, 0, "hashC")
	require.NoError(t, err)
	require.NotNil(t, target)
	readiness, err := executor.LWSManager.observeRolloutReadiness(context.Background(), old, *target)
	require.NoError(t, err)
	inputs := rolloutInputs{
		allRoleNames: append(testRoleNames(), "new-prefill", "new-decode"), targetReplicas: []int{0, 0, 4, 4},
		config: configs([]int{0, 0, 1, 1}, []int{1, 1, 1, 1}), readiness: readiness,
	}
	candidates := orderedRevisionCandidates(old, readiness)
	for _, tc := range []struct {
		advance  bool
		revision string
		step     UpdateStep
	}{
		{false, "hashB", UpdateStep{Past: []int{1, 1, 0, 0}, New: []int{0, 0, 1, 1}, UsesTargetRebalance: true}},
		{true, "hashA", UpdateStep{Past: []int{4, 3, 0, 0}, New: []int{0, 0, 3, 1}}},
	} {
		t.Run(fmt.Sprintf("advance=%v", tc.advance), func(t *testing.T) {
			inputs.scaleDuringRollout = tc.advance
			selected, state, step, err := executor.selectNextRolloutStep(context.Background(), candidates, old, *target, inputs)
			require.NoError(t, err)
			require.Equal(t, tc.revision, selected.Revision)
			require.Equal(t, &tc.step, step)
			require.NoError(t, validateUpdateStep(state, step))
		})
	}
}

func TestScalingDuringRolloutExposesLatestExternalTarget(t *testing.T) {
	ds := newTwoRoleTestDisaggregatedSet([2]int32{2, 2}, [2]int{1, 1}, [2]int{})
	ds.Spec.Roles[0].Scaling = &disaggregatedsetv1.RoleScaling{Mode: disaggregatedsetv1.RoleScalingExternal}
	old := disaggregatedsetutils.RevisionRolesList{{Revision: "old", Roles: map[string]*leaderworkersetv1.LeaderWorkerSet{
		testRolePrefill: revisionLWS("old", testRolePrefill, 2, 2, time.Now(), 2),
	}}}
	target := disaggregatedsetutils.RevisionRoles{Revision: "target", Roles: map[string]*leaderworkersetv1.LeaderWorkerSet{
		testRolePrefill: revisionLWS("target", testRolePrefill, 5, 0, time.Now(), 5),
	}}
	readTarget := func() []int {
		return rolloutTargetReplicas(ds, []string{testRolePrefill}, sets.New(testRolePrefill), old, target, map[string]int{testRolePrefill: 2})
	}
	assert.Equal(t, []int{5}, readTarget(), "default keeps the in-flight External target")
	ds.Spec.ScalingPolicy = &disaggregatedsetv1.DisaggregatedSetScalingPolicy{DuringRollout: disaggregatedsetv1.ScalingDuringRolloutPolicyAdvanceRollout}
	assert.Equal(t, []int{2}, readTarget(), "opt-in exposes the actual RoleScaler target")
}

func TestScalingDuringRolloutAppliesTargetDrainBeforeGrowth(t *testing.T) {
	for _, failDrain := range []bool{false, true} {
		t.Run(fmt.Sprintf("fail-drain=%v", failDrain), func(t *testing.T) {
			objects := revisionLWSObjects("target", [2]int32{4, 8}, [2]int32{4, 8}, [2]int32{4, 8}, time.Now())
			c := newTestClient(objects...)
			var writes []string
			observed := interceptor.NewClient(c, interceptor.Funcs{Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
				lws := obj.(*leaderworkersetv1.LeaderWorkerSet)
				writes = append(writes, fmt.Sprintf("%s=%d", lws.Labels[disaggregatedsetv1.RoleLabelKey], getLWSReplicas(lws)))
				if failDrain {
					return fmt.Errorf("injected drain failure")
				}
				return c.Patch(ctx, obj, patch, opts...)
			}})
			ds := newTwoRoleTestDisaggregatedSet([2]int32{8, 4}, [2]int{1, 1}, [2]int{})
			executor := newTestExecutor(observed)
			_, target, err := executor.LWSManager.GetRevisionRolesList(context.Background(), ds, 0, "target")
			require.NoError(t, err)
			require.NotNil(t, target)
			state := rolloutState([]int{0, 0}, []int{0, 0}, []int{0, 0}, nil, nil,
				[]int{4, 8}, []int{4, 8}, []int{8, 4}, configs([]int{1, 1}, []int{0, 0}))
			state.ScaleDuringRollout = true
			inputs := rolloutInputs{allRoleNames: testRoleNames(), targetRoleNames: testRoleNames(), scaleDuringRollout: true}
			err = executor.applyRolloutStep(context.Background(), ds, *target, inputs, disaggregatedsetutils.RevisionRoles{}, state, ComputeNextStep(state))
			if failDrain {
				require.ErrorContains(t, err, "injected drain failure")
				assert.Equal(t, []string{"decode=4"}, writes, "do not issue growth after a failed reduction")
				assertRevisionReplicas(t, c, "target", [2]int32{4, 8})
			} else {
				require.NoError(t, err)
				assert.Equal(t, []string{"decode=4", "prefill=8"}, writes)
				assertRevisionReplicas(t, c, "target", [2]int32{8, 4})
			}
		})
	}
}

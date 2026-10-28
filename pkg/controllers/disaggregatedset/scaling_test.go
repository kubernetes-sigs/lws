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
	"encoding/json"
	"fmt"
	"math/rand"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/tools/events"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	disaggregatedsetv1 "sigs.k8s.io/lws/api/disaggregatedset/v1"
	leaderworkersetv1 "sigs.k8s.io/lws/api/leaderworkerset/v1"
	"sigs.k8s.io/lws/pkg/replicagroups"
	disaggregatedsetutils "sigs.k8s.io/lws/pkg/utils/disaggregatedset"
)

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
		{"keep scheduled or unknown startup work", []int{3, 3, 0, 0}, []int{0, 0, 3, 1}, []int{0, 0, 0, 0}, nil, nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state := rolloutState([]int{4, 4, 0, 0}, tc.old, tc.old, nil, nil, tc.issued, tc.ready,
				[]int{0, 0, 4, 4}, configs([]int{0, 0, 0, 0}, []int{1, 1, 1, 1}))
			state.ScaleDownDuringRollout = true
			state.Target.UnschedulableRoles = []bool{false, false, true, true}
			state.Target.AllUnscheduled = []bool{false, false, tc.rebalance, tc.rebalance}
			step := ComputeNextStep(state)
			if tc.wantNew == nil {
				require.Nil(t, step)
				return
			}
			require.NotNil(t, step)
			assert.Equal(t, tc.wantOld, step.Past)
			assert.Equal(t, tc.wantNew, step.New)
			assert.Equal(t, tc.rebalance, step.Kind == TargetRebalanceStep)
			assert.False(t, step.Kind == BootstrapSurgeStep)
			assert.False(t, step.Kind == UnavailableFallbackStep)
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
		state.ScaleDownDuringRollout = advance
		step := ComputeNextStep(state)
		require.NotNil(t, step)
		assert.Equal(t, []int{8, 9, 0, 0}, step.Past, "advance=%v", advance)
		assert.Equal(t, []int{0, 0, 0, 0}, step.New)
		require.NoError(t, validateUpdateStep(state, step))
	}
}

func TestRolloutWithDisjointAndOverlappingOldRevisions(t *testing.T) {
	t.Parallel() // Each rollout builds its own fake client and executor.
	type layout struct {
		name               string
		up                 disaggregatedsetv1.RolloutScalingPolicy
		initial, current   [][]int32
		desired, target    []int32
		surge, unavailable []int
	}
	cases := []layout{{
		name:    "reported-A-B-C",
		initial: [][]int32{{6, 6, 0, 0}, {4, 0, 2, 2}}, current: [][]int32{{1, 1, 0, 0}, {4, 0, 2, 2}},
		desired: []int32{0, 0, 6, 6}, surge: []int{1, 1, 1, 1}, unavailable: []int{1, 1, 0, 0},
	}, {
		name: "interrupted-4306-mixed-removed-roles",
		initial: [][]int32{
			{5, 0, 0, 0, 0, 7, 4, 13, 0}, {20, 0, 0, 0, 0, 7, 0, 0, 0}, {0, 0, 0, 0, 0, 20, 0, 0, 20},
		},
		current: [][]int32{
			{1, 0, 0, 0, 0, 1, 1, 1, 0}, {1, 0, 0, 0, 0, 4, 0, 0, 0}, {0, 0, 0, 0, 0, 1, 0, 0, 5},
		},
		desired: []int32{3, 6, 17, 3, 11, 0, 0, 0, 0}, target: []int32{2, 6, 13, 3, 11, 0, 0, 0, 0},
		surge: []int{0, 2, 1, 1, 3, 1, 1, 1, 1}, unavailable: []int{1, 2, 1, 0, 2, 1, 1, 1, 1},
	}}
	rng := rand.New(rand.NewSource(5492154225))
	for roles := 4; roles <= 6; roles++ {
		for old := 2; old <= 4; old++ {
			for trial := range 1 {
				tc := layout{name: fmt.Sprintf("roles=%d/old=%d/trial=%d", roles, old, trial),
					desired: make([]int32, roles), surge: make([]int, roles), unavailable: make([]int, roles)}
				for role := range roles {
					tc.surge[role] = 1 // Removed roles use the executor's default surge.
					tc.unavailable[role] = 1 + rng.Intn(2)
					if role >= roles/2 {
						tc.desired[role] = int32(2 + rng.Intn(7))
						tc.surge[role], tc.unavailable[role] = rng.Intn(3), rng.Intn(3)
						if tc.surge[role]+tc.unavailable[role] == 0 {
							tc.surge[role] = 1
						}
					}
				}
				for revision := range old {
					initial, current := make([]int32, roles), make([]int32, roles)
					for role := range roles {
						// At least one disjoint and one overlapping history. Extra
						// revisions vary their shared roles, not just replica counts.
						present := revision%2 == 0 && role < roles/2 || revision%2 == 1 && (role == 0 || role >= roles/2)
						if revision > 1 && role > 0 && role != roles-1 && rng.Intn(2) == 0 {
							present = false
						}
						if present {
							initial[role] = int32(2 + rng.Intn(7))
							current[role] = int32(1 + rng.Intn(int(initial[role])))
						}
					}
					tc.initial, tc.current = append(tc.initial, initial), append(tc.current, current)
				}
				cases = append(cases, tc)
			}
		}
	}
	for _, tc := range slices.Clone(cases) {
		tc.name += "/ImmediateScaleUp"
		tc.up = disaggregatedsetv1.RolloutScalingImmediate
		cases = append(cases, tc)
	}
	for _, identity := range []leaderworkersetv1.GroupIdentityType{leaderworkersetv1.GroupIdentityOrdinal, leaderworkersetv1.GroupIdentityHash} {
		for _, policy := range []disaggregatedsetv1.RolloutScalingPolicy{disaggregatedsetv1.RolloutScalingDeferred, disaggregatedsetv1.RolloutScalingImmediate} {
			for _, tc := range cases {
				t.Run(fmt.Sprintf("%s/%s/%s", identity, policy, tc.name), func(t *testing.T) {
					t.Logf("initial=%v current=%v desired=%v surge=%v unavailable=%v", tc.initial, tc.current, tc.desired, tc.surge, tc.unavailable)
					ds := newTestDisaggregatedSet()
					ds.Spec.ScalingPolicy = &disaggregatedsetv1.DisaggregatedSetScalingPolicy{ScaleUpDuringRollout: tc.up, ScaleDownDuringRollout: policy}
					desired, budgets := map[string]int{}, map[string]int{}
					for role, count := range tc.desired {
						name := fmt.Sprintf("role-%d", role)
						budgets[name] = tc.unavailable[role]
						if count > 0 {
							desired[name] = int(count)
							ds.Spec.Roles = append(ds.Spec.Roles, makeRoleSpec(name, count, corev1.PodSpec{}, intstr.FromInt(tc.surge[role]), intstr.FromInt(tc.unavailable[role])))
						}
					}
					encoded, err := json.Marshal(budgets)
					require.NoError(t, err)
					ds.Annotations = map[string]string{disaggregatedsetv1.MaxUnavailableAnnotationKey: string(encoded)}
					var workloads []*leaderworkersetv1.LeaderWorkerSet
					var workloadRole, workloadRevision []int
					for revision, initial := range append(slices.Clone(tc.initial), tc.desired) {
						name := fmt.Sprintf("old-%d", revision)
						if revision == len(tc.initial) {
							name = "target"
						}
						for role, count := range initial {
							if count == 0 {
								continue
							}
							var current int32
							if revision < len(tc.initial) {
								current = tc.current[revision][role]
							} else if tc.target != nil {
								current = tc.target[role]
							}
							workloads = append(workloads, revisionLWS(name, fmt.Sprintf("role-%d", role), current, current, time.Unix(int64(revision), 0), count))
							workloadRole, workloadRevision = append(workloadRole, role), append(workloadRevision, revision)
						}
					}
					for tick := range 256 {
						var objects []client.Object
						var before, after []int32
						for _, lws := range workloads {
							count := getLWSReplicas(lws)
							before = append(before, count)
							lws.Spec.GroupIdentity, lws.Spec.LeaderWorkerTemplate.Size = identity, ptr.To[int32](1)
							lws.Status.ObservedGeneration = lws.Generation
							objects = append(objects, replicaGroupObjects(lws, count, count)...)
						}
						c := newTestClient(objects...)
						executor := newTestExecutor(c)
						executor.LWSManager = NewLeaderWorkerSetManager(c) // Real shared observer, not status stubs.
						executor.LWSManager.reader = rolloutDescendantReader(c, objects)
						_, complete, err := reconcileTestTransition(t.Context(), executor, ds, "target", desired)
						require.NoError(t, err)
						for _, lws := range workloads {
							require.NoError(t, c.Get(t.Context(), client.ObjectKeyFromObject(lws), lws))
							after = append(after, getLWSReplicas(lws))
						}
						t.Logf("tick=%d: %v -> %v", tick, before, after)
						bootstrap := false
						recorded := executor.Record.(*events.FakeRecorder).Events
						for len(recorded) > 0 {
							bootstrap = strings.Contains(<-recorded, EventReasonBootstrapSurge) || bootstrap
						}
						for i, count := range after {
							if workloadRevision[i] != len(tc.initial) {
								require.LessOrEqual(t, count, before[i], "never scale old revisions up")
							}
						}
						// Replacement credit uses only the slowest target role's Ready
						// fraction. Newly issued groups cannot fund the current step.
						replacementFraction := func(retained bool) (numerator, denominator int64) {
							numerator, denominator = 1, 1
							for i, count := range before {
								if workloadRevision[i] != len(tc.initial) {
									continue
								}
								if retained {
									count = min(count, after[i])
								}
								desired := int64(tc.desired[workloadRole[i]])
								if int64(count)*denominator < numerator*desired {
									numerator, denominator = int64(count), desired
								}
							}
							return
						}
						beforeNumerator, beforeDenominator := replacementFraction(false)
						afterNumerator, afterDenominator := replacementFraction(true)
						outageAllowed := false
						for i, count := range before {
							revision := workloadRevision[i]
							if revision >= len(tc.initial) || after[i] >= count {
								continue
							}
							for role, initial := range tc.initial[revision] {
								outageAllowed = outageAllowed || (initial > 0 && tc.desired[role] > 0 &&
									tc.unavailable[role] >= int(tc.desired[role]))
							}
						}
						// Independent safety: incomplete revisions provide no usable
						// capacity. Removed roles count only their disjoint histories.
						for role, desired := range tc.desired {
							var baseline, beforeTotal, afterTotal, beforeReady, retained int32
							var beforeIndividual, retainedIndividual int32
							for revision := range len(tc.initial) + 1 {
								if desired == 0 && revision < len(tc.initial) {
									overlaps := false
									for other, initial := range tc.initial[revision] {
										overlaps = overlaps || initial > 0 && tc.desired[other] > 0
									}
									if overlaps {
										continue
									}
								}
								beforeComplete, afterComplete, live := true, true, false
								var roleBefore, roleRetained int32
								for i, count := range before {
									if workloadRevision[i] != revision {
										continue
									}
									live = live || count > 0
									beforeComplete = beforeComplete && count > 0
									afterComplete = afterComplete && min(count, after[i]) > 0
									if workloadRole[i] == role {
										beforeTotal, afterTotal = beforeTotal+count, afterTotal+after[i]
										roleBefore, roleRetained = count, min(count, after[i])
									}
								}
								if revision < len(tc.initial) && live {
									baseline = max(baseline, tc.initial[revision][role])
								}
								if beforeComplete {
									beforeReady += roleBefore
								}
								beforeIndividual += roleBefore
								retainedIndividual += roleRetained
								if afterComplete {
									retained += roleRetained
								}
							}
							floor := desired
							if desired == 0 {
								beforeReady += int32(int64(baseline) * beforeNumerator / beforeDenominator)
								retained += int32(int64(baseline) * afterNumerator / afterDenominator)
								floor = baseline
							}
							floor = max(0, floor-int32(tc.unavailable[role]))
							if !outageAllowed || desired == 0 {
								require.GreaterOrEqual(t, retained, min(beforeReady, floor), "role %d preserves serving capacity", role)
							}
							if desired == 0 {
								continue
							}
							require.GreaterOrEqual(t, retainedIndividual, min(beforeIndividual, floor), "role %d preserves individual Ready groups", role)
							ceiling := desired + int32(tc.surge[role])
							if policy == disaggregatedsetv1.RolloutScalingDeferred {
								ceiling = max(baseline, desired) + int32(tc.surge[role])
							}
							if bootstrap {
								ceiling = max(ceiling+1, beforeTotal+1)
							}
							require.LessOrEqual(t, afterTotal, max(beforeTotal, ceiling), "role %d must not increase physical excess", role)
						}
						if complete {
							for i, count := range after {
								want := int32(0)
								if workloadRevision[i] == len(tc.initial) {
									want = tc.desired[workloadRole[i]]
								}
								require.Equal(t, want, count, "workload %s", workloads[i].Name)
							}
							return
						}
						require.NotEqual(t, before, after, "all groups are Ready but neither retirement nor growth makes progress")
					}
					t.Fatal("rollout did not complete")
				})
			}
		}
	}
}

// Each rollout tick rebuilds native descendants; reconciliation only writes LWS.
// Index those immutable fixtures like the production informer. The fake client's
// List otherwise serializes every Pod in every LWS before filtering, repeatedly.
// LWS reads/writes still use the fake client; Observe still checks real ownership,
// group membership and readiness, and receives independent copies of descendants.
func rolloutDescendantReader(c client.WithWatch, objects []client.Object) client.Reader {
	byLWS := make(map[client.ObjectKey][]client.Object)
	for _, object := range objects {
		name := object.GetLabels()[leaderworkersetv1.SetNameLabelKey]
		if name != "" {
			key := client.ObjectKey{Namespace: object.GetNamespace(), Name: name}
			byLWS[key] = append(byLWS[key], object)
		}
	}
	return interceptor.NewClient(c, interceptor.Funcs{
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, options ...client.ListOption) error {
			opts := (&client.ListOptions{}).ApplyOptions(options)
			if opts.LabelSelector == nil || opts.Namespace == "" || opts.FieldSelector != nil || opts.Limit != 0 || opts.Continue != "" {
				return c.List(ctx, list, options...)
			}
			name, exact := opts.LabelSelector.RequiresExactMatch(leaderworkersetv1.SetNameLabelKey)
			if !exact {
				return c.List(ctx, list, options...)
			}
			switch list := list.(type) {
			case *corev1.PodList:
				*list = corev1.PodList{}
			case *appsv1.StatefulSetList:
				*list = appsv1.StatefulSetList{}
			case *appsv1.ReplicaSetList:
				*list = appsv1.ReplicaSetList{}
			default:
				return c.List(ctx, list, options...)
			}
			for _, object := range byLWS[client.ObjectKey{Namespace: opts.Namespace, Name: name}] {
				if !opts.LabelSelector.Matches(labels.Set(object.GetLabels())) {
					continue
				}
				switch list := list.(type) {
				case *corev1.PodList:
					if pod, ok := object.(*corev1.Pod); ok {
						list.Items = append(list.Items, *pod.DeepCopy())
					}
				case *appsv1.StatefulSetList:
					if sts, ok := object.(*appsv1.StatefulSet); ok {
						list.Items = append(list.Items, *sts.DeepCopy())
					}
				case *appsv1.ReplicaSetList:
					if rs, ok := object.(*appsv1.ReplicaSet); ok {
						list.Items = append(list.Items, *rs.DeepCopy())
					}
				}
			}
			return nil
		},
	})
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
		{"overlapping parked readiness is not replacement credit", 5, 5, 95, 95, 0, 10, 5, 110},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state := rolloutState([]int{100, 0}, []int{tc.oldReady, 0}, []int{tc.oldReady, 0}, nil, nil,
				[]int{0, 110}, []int{0, tc.newReady}, []int{0, 100}, configs([]int{1, 1}, []int{tc.oldUnavailable, tc.targetUnavailable}))
			state.ScaleDownDuringRollout = true
			state.ActiveOld.RawReadyReplicas = []int{tc.oldRaw, 0}
			state.Target.RawReadyReplicas = []int{0, tc.newRaw}
			if tc.parked > 0 {
				state.ParkedOld = []ParkedRevisionState{{RequiredRoles: []bool{true, true},
					SpecReplicas: []int{tc.parked, tc.parked}, RawReadyReplicas: []int{tc.parked, tc.parked}, ReadyReplicas: []int{tc.parked, tc.parked}}}
			}
			assert.Equal(t, []int{0, tc.want}, furthestTargetScaleDownTargets(snapshotForRolloutState(state)))
			assert.True(t, targetScaleDownPreservesAvailability(snapshotForRolloutState(state), []int{0, tc.want}))
			if tc.want == 110 {
				assert.Nil(t, ComputeNextStep(state), "replacement credit still protects the old availability floor")
			}
			if tc.want > 100 {
				assert.False(t, targetScaleDownPreservesAvailability(snapshotForRolloutState(state), []int{0, tc.want - 1}), "one extra deletion must not spend reserved Ready credit")
				assert.Error(t, validateUpdateStep(state, &UpdateStep{Past: state.ActiveOld.SpecReplicas, New: []int{0, tc.want - 1}}))
			}
		})
	}
}

func TestDisjointTargetCorrectionKeepsWholeRevisionFraction(t *testing.T) {
	state := rolloutState([]int{3, 0, 0}, []int{1, 0, 0}, []int{1, 0, 0}, nil, nil,
		[]int{0, 4, 4}, []int{0, 2, 3}, []int{0, 2, 3}, configs([]int{1, 1, 1}, []int{0, 1, 1}))
	state.ScaleDownDuringRollout = true
	assert.Equal(t, []int{0, 4, 3}, furthestTargetScaleDownTargets(snapshotForRolloutState(state)))
	assert.True(t, targetScaleDownPreservesAvailability(snapshotForRolloutState(state), []int{0, 4, 3}))
	assert.False(t, targetScaleDownPreservesAvailability(snapshotForRolloutState(state), []int{0, 3, 3}), "B must retain ceil(2*2/3)=2 Ready; B1 replaces only one A")
}

func TestDisjointTargetCorrectionIgnoresOverlappingOldReadyCredit(t *testing.T) {
	for _, retainedOld := range []int{0, 1} {
		state := rolloutState([]int{1, 0, 1}, []int{1, 0, 1}, []int{1, 0, 1}, []int{0, 0, 1}, []int{0, 0, retainedOld},
			[]int{5, 5, 0}, []int{2, 2, 0}, []int{4, 4, 0}, configs([]int{0, 0, 0}, []int{4, 4, 1}))
		state.ScaleDownDuringRollout = true
		state.AvailabilityBaseline = []int{1, 0, 4}
		state.ParkedOld[0].RawReadyReplicas = []int{0, 0, 1}
		// The W baseline comes from the disjoint revision, not active A/W.
		// Its one W plus target half-readiness supply the floor of three.
		numerator, denominator := disjointTargetReadyFraction(snapshotForRolloutState(state))
		require.Equal(t, float64(3-retainedOld)/4, float64(numerator)/float64(denominator))
		require.Equal(t, []int{5, 5, 0}, furthestTargetScaleDownTargets(snapshotForRolloutState(state)))
		for _, next := range [][]int{{4, 5, 0}, {5, 4, 0}} {
			require.False(t, targetScaleDownPreservesAvailability(snapshotForRolloutState(state), next), "either deletion could consume required Ready credit")
		}
	}
}

func TestDisjointTargetCorrectionKeepsResidualSourceRole(t *testing.T) {
	// A is now desired zero, but this target still supplies five required A
	// replicas until B replaces more than 90% of the shared A100 baseline.
	state := rolloutState([]int{100, 0}, []int{5, 0}, []int{5, 0}, nil, nil,
		[]int{10, 110}, []int{10, 90}, []int{0, 100}, configs([]int{1, 1}, []int{0, 30}))
	state.ScaleDownDuringRollout = true
	assert.Equal(t, []int{5, 110}, furthestTargetScaleDownTargets(snapshotForRolloutState(state)))
	assert.True(t, targetScaleDownPreservesAvailability(snapshotForRolloutState(state), []int{5, 110}))
	assert.False(t, targetScaleDownPreservesAvailability(snapshotForRolloutState(state), []int{4, 110}))
}

func TestScalingDuringRolloutDecisions(t *testing.T) {
	for _, tc := range []struct {
		name                                   string
		old, oldReady, current, ready, desired []int
		surge, unavailable                     []int
		wantOld, wantNew                       []int
		allUnscheduled                         []bool
	}{
		{
			name: "old first", old: []int{4, 4}, oldReady: []int{4, 4},
			current: []int{5, 5}, ready: []int{5, 5}, desired: []int{4, 4},
			wantOld: []int{0, 0}, wantNew: []int{5, 5},
		}, {
			name: "scheduling protection is per role", old: []int{4, 4}, oldReady: []int{4, 4},
			current: []int{5, 5}, ready: []int{0, 0}, desired: []int{4, 4},
			wantOld: []int{4, 4}, wantNew: []int{1, 4}, allUnscheduled: []bool{true, false},
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
			name: "scale up preserves observed readiness below the desired floor", old: []int{4, 4}, oldReady: []int{4, 4},
			current: []int{1, 1}, ready: []int{1, 1}, desired: []int{8, 8},
			wantOld: []int{4, 4}, wantNew: []int{2, 2},
		}, {
			name: "grow while previous batch is unready", old: []int{8, 8}, oldReady: []int{8, 8},
			current: []int{2, 2}, ready: []int{0, 0}, desired: []int{12, 12},
			surge: []int{2, 2}, unavailable: []int{2, 2}, wantOld: []int{8, 8}, wantNew: []int{4, 4},
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
			state.ScaleDownDuringRollout = true
			state.Target.AllUnscheduled = tc.allUnscheduled
			step := ComputeNextStep(state)
			if tc.wantNew == nil {
				require.Nil(t, step)
				return
			}
			require.NotNil(t, step)
			assert.Equal(t, tc.wantOld, step.Past)
			assert.Equal(t, tc.wantNew, step.New)
			require.NoError(t, validateUpdateStep(state, step))
			assertScalingSafety(t, state, step)
		})
	}
}

func TestTargetCorrectionPreservesScheduledWork(t *testing.T) {
	state := rolloutState([]int{4}, []int{4}, []int{4}, nil, nil,
		[]int{5}, []int{0}, []int{4}, configs([]int{1}, []int{0}))
	state.ScaleDownDuringRollout = true
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
		assert.Equal(t, []int{4}, furthestTargetScaleDownTargets(snapshotForRolloutState(state)))
		assert.Error(t, validateUpdateStep(state, &UpdateStep{Past: []int{4}, New: []int{1}}))
	}
	state.Target.RawReadyReplicas, state.Target.ReadyReplicas = []int{0}, []int{0}
	state.Target.SpecReplicas = []int{4}
	assert.Nil(t, ComputeNextStep(state), "even all-unscheduled work is not retracted at or below desired")
	state.Target.SpecReplicas = []int{5}
	state.ScaleDownDuringRollout = false
	assert.Nil(t, ComputeNextStep(state), "Deferred does not gain target corrections")
}

func TestScaleUpUsesDesiredAvailabilityFloorForEitherDrain(t *testing.T) {
	for _, immediate := range []bool{false, true} {
		for _, active := range []int{4, 6} {
			parked := 10 - active
			state := rolloutState([]int{active}, []int{active}, []int{active}, []int{parked}, []int{parked},
				[]int{1}, []int{1}, []int{8}, configs([]int{1}, []int{0}))
			state.ScaleDownDuringRollout = immediate
			observed := snapshotForRolloutState(state)
			assert.Equal(t, 8, observed.availabilityFloor(0), "both candidates protect the desired capacity, not their smaller history")
			assert.Equal(t, []int{max(0, 8-parked-1)}, observed.oldDrain().minimumTargets())
			assert.Equal(t, 8, observed.targetDrain().roles[0].floor, "old and target reductions use the same floor")
		}
	}
}

func TestScaleUpWaitsForReadyCapacityBeforeDrainingOld(t *testing.T) {
	pair := func(n int) []int { return []int{n, n} }
	for _, immediate := range []bool{false, true} {
		for _, tc := range []struct {
			name                               string
			oldSpec, oldRaw, oldReady          int
			targetSpec, targetRaw, targetReady int
			wantOld, wantNew                   int
		}{
			{"slide 5: requested growth is not Ready", 4, 4, 4, 2, 1, 1, 4, 4},
			{"four Ready replacements permit one old deletion", 4, 4, 4, 4, 4, 4, 3, 6},
			{"pending old deletion cannot be spent twice", 3, 4, 3, 6, 4, 4, 3, 7},
			{"pending target deletion cannot fund an old drain", 4, 4, 4, 4, 4, 3, 4, 6},
			{"already below the floor: keep the remaining Ready", 2, 2, 2, 4, 1, 1, 2, 4},
		} {
			t.Run(fmt.Sprintf("immediate=%v/%s", immediate, tc.name), func(t *testing.T) {
				state := rolloutState(pair(4), pair(tc.oldSpec), pair(tc.oldReady), nil, nil,
					pair(tc.targetSpec), pair(tc.targetReady), pair(8), configs(pair(2), pair(1)))
				state.ScaleDownDuringRollout = immediate
				state.ActiveOld.RawReadyReplicas = pair(tc.oldRaw)
				state.Target.RawReadyReplicas = pair(tc.targetRaw)
				step := ComputeNextStep(state)
				if tc.wantOld == tc.oldSpec && tc.wantNew == tc.targetSpec {
					require.Nil(t, step)
				} else {
					require.Equal(t, &UpdateStep{Past: pair(tc.wantOld), New: pair(tc.wantNew)}, step)
					require.NoError(t, validateUpdateStep(state, step))
					assertScalingSafety(t, state, step)
				}
				require.Error(t, validateUpdateStep(state, &UpdateStep{Past: pair(tc.wantOld - 1), New: pair(tc.wantNew)}),
					"one more old deletion would worsen observed availability or cross the desired floor of seven")
			})
		}
	}
}

// Ownership and missing native/worker objects are covered by the shared
// observer tests. Here, incomplete membership must not become "unscheduled".
func TestAllGroupsUnscheduled(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*replicagroups.Snapshot)
		want   bool
	}{
		{name: "complete unbound group", want: true},
		{name: "no groups", change: func(s *replicagroups.Snapshot) { s.Groups = nil }},
		{name: "extra group", change: func(s *replicagroups.Snapshot) { s.Groups = append(s.Groups, s.Groups[0]) }},
		{name: "zero target", change: func(s *replicagroups.Snapshot) { *s.LWS.Spec.Replicas, s.Groups = 0, nil }},
		{name: "missing worker", change: func(s *replicagroups.Snapshot) { s.Groups[0].Pods = s.Groups[0].Pods[:1] }},
		{name: "extra worker", change: func(s *replicagroups.Snapshot) { s.Groups[0].Pods = append(s.Groups[0].Pods, &corev1.Pod{}) }},
		{name: "bound leader", change: func(s *replicagroups.Snapshot) { s.Groups[0].Leader.Spec.NodeName = "node" }},
		{name: "bound worker", change: func(s *replicagroups.Snapshot) { s.Groups[0].Pods[1].Spec.NodeName = "node" }},
		{name: "terminating group", change: func(s *replicagroups.Snapshot) { s.Groups[0].Terminating = true }},
		{name: "completed leader", change: func(s *replicagroups.Snapshot) { s.Groups[0].Leader.Status.Phase = corev1.PodSucceeded }},
		{name: "failed worker", change: func(s *replicagroups.Snapshot) { s.Groups[0].Pods[1].Status.Phase = corev1.PodFailed }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			leader := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{leaderworkersetv1.SizeAnnotationKey: "2"}}}
			worker := &corev1.Pod{}
			snapshot := &replicagroups.Snapshot{
				LWS:    &leaderworkersetv1.LeaderWorkerSet{Spec: leaderworkersetv1.LeaderWorkerSetSpec{Replicas: ptr.To[int32](1)}},
				Groups: []replicagroups.Group{{Leader: leader, Pods: []*corev1.Pod{leader, worker}}},
			}
			if tc.change != nil {
				tc.change(snapshot)
			}
			assert.Equal(t, tc.want, allGroupsUnscheduled(snapshot.Groups, int(getLWSReplicas(snapshot.LWS))))
		})
	}
}

func TestSubRoleSchedulingObservation(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*replicagroups.Snapshot)
		want   bool
	}{
		{name: "other child scheduled", want: true},
		{name: "leader scheduled", change: func(s *replicagroups.Snapshot) { s.Groups[0].Leader.Spec.NodeName = "node" }},
		{name: "worker scheduled", change: func(s *replicagroups.Snapshot) { s.Groups[0].Pods[1].Spec.NodeName = "node" }},
		{name: "missing group", change: func(s *replicagroups.Snapshot) { s.Groups = s.Groups[1:] }},
		{name: "missing worker", change: func(s *replicagroups.Snapshot) { s.Groups[0].Pods = s.Groups[0].Pods[:1] }},
		{name: "partial assignment", change: func(s *replicagroups.Snapshot) { s.Groups[0].Pods[1].Labels[disaggregatedsetv1.SubRoleLabelKey] = "b" }},
		{name: "terminating group", change: func(s *replicagroups.Snapshot) { s.Groups[0].Terminating = true }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSubRoleFixture(t, 0, "a", "b")
			s := f.observe()
			s.Groups[0].Ready = false
			for _, pod := range s.Groups[0].Pods {
				pod.Spec.NodeName, pod.Status = "", corev1.PodStatus{Phase: corev1.PodPending}
			}
			if tc.change != nil {
				tc.change(s)
			}
			observed := rolloutReadiness{}
			observeSubRoleReadiness(&subRoleState{snapshot: s, issued: map[string]int{"a": 1, "b": 1}}, observed)
			state := rolloutStateForRevision([]string{"model/a", "model/b"}, nil, disaggregatedsetutils.RevisionRoles{}, f.revision("target"), []int{1, 1}, configs([]int{1, 1}, []int{0, 0}), observed)
			require.Equal(t, []bool{tc.want, false}, state.Target.AllUnscheduled, "only complete, unbound groups of this child authorize cancellation")
		})
	}
}

func TestSubRoleScalingDuringRolloutPolicies(t *testing.T) {
	f := newSubRoleFixture(t, 0, "pool", "pool")
	f.ds.Spec.Roles = []disaggregatedsetv1.DisaggregatedRoleSpec{{Name: "model", SubRoles: []disaggregatedsetv1.DisaggregatedSubRoleSpec{{Name: "pool", Scaling: &disaggregatedsetv1.RoleScaling{Mode: disaggregatedsetv1.RoleScalingExternal}}}}}
	target := f.withRevision("target", "pool", "pool", "pool", "pool", "pool")
	old := disaggregatedsetutils.RevisionRolesList{f.revision("old")}
	observed := rolloutReadiness{}
	for fixture, count := range map[*subRoleFixture]int{f: 2, target: 5} {
		observeSubRoleReadiness(&subRoleState{snapshot: fixture.observe(), issued: map[string]int{"pool": count}}, observed)
	}
	for _, immediate := range []bool{false, true} {
		f.ds.Spec.ScalingPolicy = nil
		want := 5
		if immediate {
			f.ds.Spec.ScalingPolicy = &disaggregatedsetv1.DisaggregatedSetScalingPolicy{ScaleDownDuringRollout: disaggregatedsetv1.RolloutScalingImmediate}
			want = 2
		}
		inputs := buildRolloutInputs(f.ds, old, target.revision("target"), map[string]int{"model/pool": 2}, observed)
		require.Equal(t, immediate, inputs.scaleDownDuringRollout)
		require.Equal(t, []string{"model/pool"}, inputs.allRoleNames)
		require.Equal(t, RoleReplicaState{want}, inputs.targetReplicas, "only Deferred retains the issued External child target")
		complete, ready := rolloutCompletionStatus(nil, target.revision("target"), inputs.allRoleNames, RoleReplicaState{2}, observed, immediate)
		require.Equal(t, !immediate, complete, "Immediate must first reduce this child to its exact target")
		require.Equal(t, !immediate, ready)
	}
}

func TestScalingDuringRolloutObservesScheduledGroups(t *testing.T) {
	for _, identity := range []leaderworkersetv1.GroupIdentityType{leaderworkersetv1.GroupIdentityOrdinal, leaderworkersetv1.GroupIdentityHash} {
		for _, boundMember := range []string{"none", "leader", "worker"} {
			t.Run(fmt.Sprintf("%s/bound=%s", identity, boundMember), func(t *testing.T) {
				ds := newTestDisaggregatedSet(makeRoleSpec(testRolePrefill, 4, corev1.PodSpec{}, intstr.FromInt(1), intstr.FromInt(0)))
				ds.Spec.ScalingPolicy = &disaggregatedsetv1.DisaggregatedSetScalingPolicy{ScaleDownDuringRollout: disaggregatedsetv1.RolloutScalingImmediate}
				old := revisionLWS("old", testRolePrefill, 4, 4, time.Unix(1000, 0), 4)
				target := revisionLWS("target", testRolePrefill, 5, 0, time.Unix(2000, 0), 5)
				old.Spec.GroupIdentity, target.Spec.GroupIdentity = identity, identity
				old.Spec.LeaderWorkerTemplate.Size, target.Spec.LeaderWorkerTemplate.Size = ptr.To[int32](1), ptr.To[int32](2)
				objects := replicaGroupObjects(target, 5, 0)
				for _, object := range slices.Clone(objects) {
					leader, ok := object.(*corev1.Pod)
					if !ok {
						continue
					}
					leader.Status.Phase = corev1.PodPending // Pending can still have a node.
					leader.Annotations[leaderworkersetv1.SizeAnnotationKey] = "2"
					workers := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{
						Name: leader.Name, Namespace: leader.Namespace, UID: leader.UID + "-workers",
						Labels:          map[string]string{leaderworkersetv1.SetNameLabelKey: target.Name},
						OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(leader, corev1.SchemeGroupVersion.WithKind("Pod"))}},
						Spec: appsv1.StatefulSetSpec{Replicas: ptr.To[int32](1)}}
					worker := leader.DeepCopy()
					worker.Name, worker.UID = leader.Name+"-1", leader.UID+"-worker"
					worker.Labels[leaderworkersetv1.WorkerIndexLabelKey] = "1"
					worker.OwnerReferences = []metav1.OwnerReference{*metav1.NewControllerRef(workers, appsv1.SchemeGroupVersion.WithKind("StatefulSet"))}
					// One bound member in one group protects the entire target role.
					if leader.Name == target.Name+"-4" {
						if boundMember == "leader" {
							leader.Spec.NodeName = "node"
						} else if boundMember == "worker" {
							worker.Spec.NodeName = "node"
						}
					}
					objects = append(objects, workers, worker)
				}
				// Unbound Pods can be cancelled before native status catches up.
				for _, object := range objects {
					object.SetGeneration(2)
				}
				c := newTestClient(append(objects, replicaGroupObjects(old, 4, 4)...)...)
				executor := newTestExecutor(c)
				executor.LWSManager = NewLeaderWorkerSetManager(c)
				// TestReconcileBatchesLiveReads checks these same observations add no reads.
				_, complete, err := reconcileTestTransition(t.Context(), executor, ds, "target", resolveDesiredReplicasByRole(ds, nil))
				require.NoError(t, err)
				assert.False(t, complete)
				want := 4
				if boundMember == "none" {
					want = 1
				}
				assert.EqualValues(t, 4, getTestLWSReplicas(c, testNamespace, old.Name))
				assert.EqualValues(t, want, getTestLWSReplicas(c, testNamespace, target.Name))
			})
		}
	}
}

func TestTargetDrainUsesRetainedNotRawReadiness(t *testing.T) {
	state := rolloutState([]int{0, 0}, []int{0, 0}, []int{0, 0}, nil, nil,
		[]int{5, 5}, []int{4, 4}, []int{3, 3}, configs([]int{1, 1}, []int{0, 0}))
	state.Target.RawReadyReplicas = []int{5, 5} // One victim per role is already reserved.
	state.ScaleDownDuringRollout = true
	step := ComputeNextStep(state)
	require.NotNil(t, step)
	assert.Equal(t, []int{4, 4}, step.New)
	require.NoError(t, validateUpdateStep(state, step))
	require.Error(t, validateUpdateStep(state, &UpdateStep{Past: []int{0, 0}, New: []int{3, 3}}))

	state.ScaleDownDuringRollout = false
	assert.Nil(t, ComputeNextStep(state), "default remains growth-only")
	require.Error(t, validateUpdateStep(state, step), "the executor independently rejects opt-out target shrink")
}

func TestMovingTargetUnschedulableFallbackUsesLatestSurgeCeiling(t *testing.T) {
	state := rolloutState([]int{8, 8}, []int{4, 4}, []int{3, 3}, nil, nil,
		[]int{1, 1}, []int{0, 0}, []int{3, 3}, configs([]int{1, 1}, []int{0, 0}))
	state.ScaleDownDuringRollout = true
	require.Nil(t, ComputeNextStep(state), "wait for the issued target group rather than stacking emergency slots")
	// Old+target Spec is 5: above the latest ceiling 3+1, but below
	// the historical ceiling 8+1. Only scheduler rejection permits a fallback.
	state.Target.UnschedulableRoles = []bool{true, false}
	step := ComputeNextStep(state)
	require.NotNil(t, step)
	assert.True(t, step.Kind == UnavailableFallbackStep)
	assert.Equal(t, []int{3, 3}, step.Past)
	assert.Equal(t, []int{1, 1}, step.New)
	require.NoError(t, validateUpdateStep(state, step))
	assertScalingSafety(t, state, step)
}

func TestMovingTargetCoordinationBootstrapDoesNotStack(t *testing.T) {
	state := rolloutState([]int{2, 3}, []int{2, 2}, []int{2, 2}, []int{1, 0}, []int{0, 0},
		[]int{4, 2}, []int{4, 2}, []int{6, 4}, configs([]int{3, 0}, []int{0, 2}))
	// The incomplete parked revision provides no serving capacity toward
	// either current role's desired availability floor.
	state.ParkedOld[0].RequiredRoles = []bool{true, true}
	state.AvailabilityBaseline = []int{6, 4}
	state.ScaleDownDuringRollout = true
	step := ComputeNextStep(state)
	require.NotNil(t, step)
	require.True(t, step.Kind == BootstrapSurgeStep)
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
		require.False(t, correction.Kind == BootstrapSurgeStep)
		require.Equal(t, state.Target.SpecReplicas, correction.New)
		require.Equal(t, []int{2, 1}, correction.Past, "release only the one excess old group")
		require.NoError(t, validateUpdateStep(state, correction))
		assertScalingSafety(t, state, correction)
	}
	// Once the other role catches up, ordinary retirement releases capacity.
	state.Target.ReadyReplicas, state.Target.RawReadyReplicas = []int{6, 3}, []int{6, 3}
	step = ComputeNextStep(state)
	require.NotNil(t, step)
	require.False(t, step.Kind == BootstrapSurgeStep)
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
	oldSpec := slices.Clone(state.ActiveOld.SpecReplicas)
	for _, parked := range state.ParkedOld {
		revisions = append(revisions, revision{parked.RequiredRoles, parked.SpecReplicas, parked.ReadyReplicas, parked.SpecReplicas})
		for i := range oldSpec {
			oldSpec[i] += parked.SpecReplicas[i]
		}
	}
	hasDisjointOld := false
	for index, revision := range revisions {
		if index == 1 {
			continue // The target is not old history.
		}
		hasReplicas, overlaps := false, false
		for i, count := range revision.spec {
			hasReplicas = hasReplicas || count > 0
			overlaps = overlaps || (revision.required[i] && state.Target.DesiredReplicas[i] > 0)
		}
		hasDisjointOld = hasDisjointOld || (hasReplicas && !overlaps)
	}
	n := len(state.Config)
	beforeServing, afterServing, beforeRole, afterRole := make([]int, n), make([]int, n), make([]int, n), make([]int, n)
	parkedServing := make([]int, n)
	for revisionIndex, rev := range revisions {
		if !slices.Contains(rev.required, true) {
			continue
		}
		afterReady := make([]int, n)
		beforeUsable, afterUsable := true, true
		for i := range rev.spec {
			// Treat every deletion as Ready capacity lost; new growth grants none.
			afterReady[i] = max(0, rev.ready[i]-max(0, rev.spec[i]-rev.next[i]))
			if rev.required[i] {
				beforeUsable = beforeUsable && rev.ready[i] > 0
				afterUsable = afterUsable && afterReady[i] > 0
			}
		}
		for i := range rev.spec {
			beforeRole[i] += rev.ready[i]
			afterRole[i] += afterReady[i]
			if beforeUsable {
				beforeServing[i] += rev.ready[i]
				if revisionIndex >= 2 {
					parkedServing[i] += rev.ready[i]
				}
			}
			if afterUsable {
				afterServing[i] += afterReady[i]
			}
		}
	}
	phaseTargets, growing, oldReference, oldProgress, oldNext := make([]int, n), make([]int, n), make([]int, n), make([]int, n), make([]int, n)
	scaleCorrection, growthCorrection := make([]bool, n), make([]bool, n)
	requestedTopUp := make([]int, n)
	surgeBaselines := slices.Clone(state.Target.DesiredReplicas)
	scaleUp := false
	steps := 0
	hasTarget, targetComplete, hasRemovedBaseline := false, true, false
	for i, desired := range state.Target.DesiredReplicas {
		scaleUp = scaleUp || desired > state.AvailabilityBaseline[i]
		if !state.ScaleDownDuringRollout {
			surgeBaselines[i] = max(surgeBaselines[i], state.ActiveOld.InitialReplicas[i])
		}
		if desired > 0 {
			hasTarget = true
			targetComplete = targetComplete && state.Target.ReadyReplicas[i] >= desired
		} else {
			hasRemovedBaseline = hasRemovedBaseline || state.AvailabilityBaseline[i] > 0
		}
	}
	for i := range state.Config {
		phaseTargets[i] = state.Target.DesiredReplicas[i]
		if !hasTarget || targetComplete || !hasRemovedBaseline {
			phaseTargets[i] -= parkedServing[i]
		}
		if state.Target.DesiredReplicas[i] > 0 {
			phaseTargets[i] = max(1, phaseTargets[i])
		}
		phaseTargets[i] = max(state.Target.SpecReplicas[i], phaseTargets[i])
		growing[i] = max(state.Target.SpecReplicas[i], step.New[i])
		if state.ScaleUpDuringRollout && (state.AvailabilityBaseline[i] > 0 || !hasDisjointOld) {
			requestedTopUp[i] = max(0, state.Target.DesiredReplicas[i]-oldSpec[i])
			growthCorrection[i] = step.New[i] <= requestedTopUp[i]
		}
		oldReference[i] = state.ActiveOld.InitialReplicas[i]
		if (state.ScaleDownDuringRollout || scaleUp) && state.Target.DesiredReplicas[i] > 0 {
			physical := oldSpec[i] + state.Target.SpecReplicas[i]
			excess := max(0, physical-surgeBaselines[i]-state.Config[i].MaxSurge)
			scaleCorrection[i] = state.ActiveOld.SpecReplicas[i]-step.Past[i] <= excess
		}
		oldProgress[i] = oldReference[i] - state.ActiveOld.SpecReplicas[i]
		oldNext[i] = oldReference[i] - step.Past[i]
		steps = max(steps, state.ActiveOld.InitialReplicas[i], state.Target.DesiredReplicas[i])
	}
	assertMovingWindow(t, state.Target.SpecReplicas, phaseTargets, growing, growthCorrection)
	assertMovingWindow(t, oldProgress, oldReference, oldNext, scaleCorrection)
	outageAllowed := false
	if !slices.Equal(step.Past, state.ActiveOld.SpecReplicas) {
		for i, required := range state.ActiveOld.RequiredRoles {
			outageAllowed = outageAllowed || (required && state.Target.DesiredReplicas[i] > 0 &&
				state.Config[i].MaxUnavailable >= state.Target.DesiredReplicas[i])
		}
	}
	for i, config := range state.Config {
		if i >= len(state.Target.AllUnscheduled) || !state.Target.AllUnscheduled[i] ||
			state.Target.RawReadyReplicas[i] > 0 || state.Target.ReadyReplicas[i] > 0 {
			require.GreaterOrEqual(t, step.New[i], min(state.Target.SpecReplicas[i], state.Target.DesiredReplicas[i]), "preserve scheduled or unknown work")
		}
		floor := max(0, state.Target.DesiredReplicas[i]-config.MaxUnavailable)
		if step.Kind == UnavailableFallbackStep && floor > 1 {
			floor--
		}
		if !outageAllowed && afterServing[i] < min(beforeServing[i], floor) {
			t.Fatalf("serving role %d: got %d, need %d: state=%+v step=%+v", i, afterServing[i], min(beforeServing[i], floor), state, step)
		}
		// Draining another role cannot remove this role's physical replicas;
		// losing structural credit alone is covered by the serving check above.
		if (step.Past[i] < state.ActiveOld.SpecReplicas[i] || step.New[i] < state.Target.SpecReplicas[i]) && afterRole[i] < min(beforeRole[i], floor) {
			t.Fatalf("per-role %d: got %d, need %d: state=%+v step=%+v", i, afterRole[i], min(beforeRole[i], floor), state, step)
		}
		old := oldSpec[i]
		if step.New[i] > state.Target.SpecReplicas[i] {
			ceiling := surgeBaselines[i] + config.MaxSurge
			budget := config.MaxSurge + config.MaxUnavailable
			if step.Kind == BootstrapSurgeStep {
				ceiling = max(ceiling+1, old+1)
				budget = max(config.MaxSurge+1, old-surgeBaselines[i]+1) + config.MaxUnavailable
			}
			if old+step.New[i] > ceiling {
				t.Fatalf("role %d exceeds replica ceiling %d: state=%+v step=%+v", i, ceiling, state, step)
			}
			if old > 0 {
				count := max(state.ActiveOld.InitialReplicas[i], state.Target.DesiredReplicas[i])
				allowance := (count*budget + steps - 1) / steps
				if step.New[i] > max(state.Target.ReadyReplicas[i]+allowance, requestedTopUp[i]) {
					t.Fatalf("role %d exceeds growth allowance %d: state=%+v step=%+v", i, allowance, state, step)
				}
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
			if float64(proposed[i])/float64(count) > least+width+1e-9 {
				t.Fatalf("coordination current=%v counts=%v proposed=%v", current, counts, proposed)
			}
		}
	}
}

func TestScalingDuringRolloutIndependentSafety(t *testing.T) {
	for _, immediateUp := range []bool{false, true} {
		t.Run(fmt.Sprintf("immediateUp=%v", immediateUp), func(t *testing.T) {
			rng := rand.New(rand.NewSource(1023))
			// Share the 20,000-scenario budget across both scale-up policies.
			for scenario := range 10000 {
				n := 2 + rng.Intn(2)
				initial := make([]int, n)
				for i := range initial {
					initial[i] = 5
				}
				state := rolloutState(initial, make([]int, n), make([]int, n), make([]int, n), make([]int, n),
					make([]int, n), make([]int, n), make([]int, n), make([]RollingUpdateConfig, n))
				state.ScaleDownDuringRollout = scenario%2 == 0
				state.ScaleUpDuringRollout = immediateUp
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
				state.AvailabilityBaseline = rolloutState(initial, state.ActiveOld.SpecReplicas, nil,
					state.ParkedOld[0].SpecReplicas, nil, nil, nil, state.Target.DesiredReplicas, nil).AvailabilityBaseline
				if step := ComputeNextStep(state); step != nil {
					require.NoError(t, validateUpdateStep(state, step), "scenario %d state=%+v step=%+v", scenario, state, step)
					assertScalingSafety(t, state, step)
				}
			}
		})
	}
}

func TestScalingDuringRolloutDelayedConvergence(t *testing.T) {
	executor := newTestExecutor(newTestClient())
	for _, immediateUp := range []bool{false, true} {
		t.Run(fmt.Sprintf("immediateUp=%v", immediateUp), func(t *testing.T) {
			rng := rand.New(rand.NewSource(81259))
			for scenario := range 500 {
				initial := []int{rng.Intn(6), rng.Intn(6)}
				state := rolloutState(initial, initial, initial, []int{1, 1}, []int{1, 1}, []int{0, 0}, []int{0, 0}, initial,
					configs([]int{rng.Intn(3), rng.Intn(3)}, []int{rng.Intn(3), rng.Intn(3)}))
				for i := range state.Config {
					if state.Config[i].MaxSurge+state.Config[i].MaxUnavailable == 0 {
						state.Config[i].MaxSurge = 1
					}
				}
				state.Target.AllUnscheduled = make([]bool, 2)
				state.ScaleUpDuringRollout = immediateUp
				state.ScaleDownDuringRollout = scenario%2 == 0
				assertMovingRolloutConverges(t, executor, &state, 120, func(tick int) {
					if tick < 4 {
						state.Target.DesiredReplicas = []int{rng.Intn(8), rng.Intn(8)}
					}
					for i := range state.Target.AllUnscheduled {
						state.Target.AllUnscheduled[i] = state.Target.RawReadyReplicas[i] == 0 && rng.Intn(2) == 0
					}
				})
			}
		})
	}
}

// Both convergence populations use the same delayed native-controller model:
// readiness catches up every third tick; an issued deletion reserves it immediately.
// Selection only reads the empty fake client, so each test reuses its executor.
func assertMovingRolloutConverges(t *testing.T, executor *RollingUpdateExecutor, state *RolloutState, ticks int, editTarget func(int)) {
	t.Helper()
	roleNames := make([]string, len(state.Config))
	for i := range roleNames {
		roleNames[i] = fmt.Sprintf("role-%d", i)
	}
	readiness := rolloutReadiness{}
	makeRevision := func(name string, initial, spec, raw, ready []int) disaggregatedsetutils.RevisionRoles {
		revision := disaggregatedsetutils.RevisionRoles{Revision: name, Roles: map[string]*leaderworkersetv1.LeaderWorkerSet{}}
		for i, role := range roleNames {
			if initial[i] == 0 && spec[i] == 0 && name != "target" {
				continue
			}
			lws := revisionLWS(name, role, int32(spec[i]), int32(raw[i]), time.Unix(0, 0), int32(initial[i]))
			revision.Roles[role] = lws
			readiness[lws.Name] = replicaReadiness{raw: raw[i], committed: ready[i]}
		}
		return revision
	}
	old := disaggregatedsetutils.RevisionRolesList{makeRevision("old-0", state.ActiveOld.InitialReplicas,
		state.ActiveOld.SpecReplicas, state.ActiveOld.RawReadyReplicas, state.ActiveOld.ReadyReplicas)}
	for i, parked := range state.ParkedOld {
		old = append(old, makeRevision(fmt.Sprintf("old-%d", i+1), parked.SpecReplicas, parked.SpecReplicas, parked.RawReadyReplicas, parked.ReadyReplicas))
	}
	target := makeRevision("target", state.Target.DesiredReplicas, state.Target.SpecReplicas, state.Target.RawReadyReplicas, state.Target.ReadyReplicas)
	// Only planner inputs are simulated; candidate ordering, saved baselines,
	// and choosing another old revision when the first stalls are production code.
	history := []string{fmt.Sprintf("initial=%v parked=%+v config=%v immediateUp=%v immediateDown=%v",
		state.ActiveOld.InitialReplicas, state.ParkedOld, state.Config, state.ScaleUpDuringRollout, state.ScaleDownDuringRollout)}
	for tick := range ticks {
		if tick%3 == 2 {
			for _, revision := range append(slices.Clone(old), target) {
				for _, lws := range revision.Roles {
					count := int(getLWSReplicas(lws))
					readiness[lws.Name] = replicaReadiness{raw: count, committed: count}
				}
			}
		}
		for i, role := range roleNames {
			lws := target.Roles[role]
			state.Target.SpecReplicas[i] = int(getLWSReplicas(lws))
			state.Target.RawReadyReplicas[i], state.Target.ReadyReplicas[i] = readiness[lws.Name].raw, readiness[lws.Name].committed
		}
		editTarget(tick)
		// Keep every edit and RNG draw, but avoid replanning once old Spec is
		// zero and target Spec matches demand. Later edits resume planning.
		converged := slices.Equal(state.Target.SpecReplicas, state.Target.DesiredReplicas)
		for _, role := range roleNames {
			converged = converged && old.GetTotalReplicasPerRole(role) == 0
		}
		if converged {
			continue
		}
		for i, role := range roleNames {
			lws := target.Roles[role]
			observed := readiness[lws.Name]
			observed.allUnscheduled = i < len(state.Target.AllUnscheduled) && state.Target.AllUnscheduled[i]
			readiness[lws.Name] = observed
		}
		inputs := rolloutInputs{allRoleNames: roleNames, targetReplicas: state.Target.DesiredReplicas,
			config: state.Config, readiness: readiness, scaleUpDuringRollout: state.ScaleUpDuringRollout, scaleDownDuringRollout: state.ScaleDownDuringRollout}
		selected := disaggregatedsetutils.RevisionRoles{}
		var observed RolloutState
		var step *UpdateStep
		if candidates := orderedRevisionCandidates(old, readiness); len(candidates) > 0 {
			var err error
			selected, observed, step, err = executor.selectNextRolloutStep(t.Context(), candidates, old, target, inputs)
			require.NoError(t, err)
		} else {
			observed = rolloutStateForRevision(roleNames, old, selected, target, inputs.targetReplicas, inputs.config, readiness)
			observed.ScaleUpDuringRollout = inputs.scaleUpDuringRollout
			// With old groups gone, model steady-state scaling, which also
			// removes target surplus deferred until the rollout completed.
			observed.ScaleDownDuringRollout = true
			step = ComputeNextStep(observed)
		}
		if tick < 15 {
			history = append(history, fmt.Sprintf("tick=%d desired=%v selected=%s old=%v target=%v targetReady=%v step=%+v", tick,
				state.Target.DesiredReplicas, selected.Revision, observed.ActiveOld.SpecReplicas, state.Target.SpecReplicas, state.Target.ReadyReplicas, step))
		}
		if step != nil {
			*state = observed
			require.NoError(t, validateUpdateStep(*state, step), "history=%v", history)
			assertScalingSafety(t, *state, step)
			for _, change := range []struct {
				revision disaggregatedsetutils.RevisionRoles
				next     []int
			}{{selected, step.Past}, {target, step.New}} {
				for i, role := range roleNames {
					if lws := change.revision.Roles[role]; lws != nil {
						ready := readiness[lws.Name]
						ready.committed = max(0, ready.committed-max(0, int(getLWSReplicas(lws))-change.next[i]))
						readiness[lws.Name] = ready
						*lws.Spec.Replicas = int32(change.next[i])
					}
				}
			}
		}
	}
	for i, role := range roleNames {
		require.Zero(t, old.GetTotalReplicasPerRole(role), "history=%v", history)
		require.Equal(t, state.Target.DesiredReplicas[i], int(getLWSReplicas(target.Roles[role])), "history=%v", history)
	}
}

func TestScalingDuringRolloutFiveEditsReachPhysicalExcess(t *testing.T) {
	state := rolloutState([]int{9, 2}, []int{9, 2}, []int{9, 2}, nil, nil,
		[]int{0, 0}, []int{0, 0}, []int{9, 2}, configs([]int{0, 1}, []int{1, 1}))
	state.ScaleDownDuringRollout = true
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
	require.NotNil(t, step, "latest-scale excess must not deadlock old and target fractional windows")
	require.Equal(t, []int{4, 2}, step.Past, "only one excess group can leave while Decode is below its desired floor")
	require.Equal(t, []int{2, 5}, step.New)
	require.NoError(t, validateUpdateStep(state, step))
	assertScalingSafety(t, state, step)
	excessOnly := &UpdateStep{Past: []int{4, 2}, New: step.New}
	require.NoError(t, validateUpdateStep(state, excessOnly), "one physical excess group may also drain independently")
	require.ErrorContains(t, validateUpdateStep(state, &UpdateStep{Past: []int{3, 2}, New: step.New}), "fractional coordination",
		"availability would permit two deletions, but only one is excess")
	state.ActiveOld.ReadyReplicas = []int{3, 2}
	require.Nil(t, ComputeNextStep(state), "physical excess cannot spend already-reserved Ready capacity")
	state.ActiveOld.ReadyReplicas = []int{5, 2}
	state.ScaleDownDuringRollout = false
	require.ErrorContains(t, validateUpdateStep(state, excessOnly), "fractional coordination", "Deferred retains its historical window")

	state = rolloutState([]int{1, 2}, []int{1, 2}, []int{1, 2}, nil, nil,
		[]int{3, 3}, []int{3, 3}, []int{2, 2}, configs([]int{0, 0}, []int{0, 0}))
	state.ScaleDownDuringRollout = true
	require.ErrorContains(t, validateUpdateStep(state, &UpdateStep{Past: []int{0, 1}, New: []int{3, 3}}), "required roles incomplete")
}

func TestScalingDuringRolloutRepeatedEditsConverge(t *testing.T) {
	t.Parallel() // Its RNG, executor and simulated workloads are not shared.
	executor := newTestExecutor(newTestClient())
	for _, immediateUp := range []bool{false, true} {
		t.Run(fmt.Sprintf("immediateUp=%v", immediateUp), func(t *testing.T) {
			for _, seed := range []int64{4041023, 9071105, 11371023} {
				rng := rand.New(rand.NewSource(seed))
				for scenario := range 500 {
					initial := []int{1 + rng.Intn(10), 1 + rng.Intn(10)}
					state := rolloutState(initial, initial, initial, nil, nil, []int{0, 0}, []int{0, 0}, initial,
						configs([]int{0, 1}, []int{1, 1}))
					state.ScaleUpDuringRollout = immediateUp
					state.ScaleDownDuringRollout = scenario%2 == 0
					assertMovingRolloutConverges(t, executor, &state, 100, func(tick int) {
						if tick < 5 {
							state.Target.DesiredReplicas = []int{1 + rng.Intn(10), 1 + rng.Intn(10)}
						}
					})
				}
			}
		})
	}
}

func TestScalingDuringRolloutExecutorPolicies(t *testing.T) {
	for _, policy := range []*disaggregatedsetv1.DisaggregatedSetScalingPolicy{
		nil, {}, {ScaleDownDuringRollout: disaggregatedsetv1.RolloutScalingDeferred},
		{ScaleDownDuringRollout: disaggregatedsetv1.RolloutScalingImmediate},
		{ScaleUpDuringRollout: disaggregatedsetv1.RolloutScalingDeferred},
		{ScaleUpDuringRollout: disaggregatedsetv1.RolloutScalingImmediate},
		{ScaleUpDuringRollout: disaggregatedsetv1.RolloutScalingImmediate, ScaleDownDuringRollout: disaggregatedsetv1.RolloutScalingDeferred},
		{ScaleUpDuringRollout: disaggregatedsetv1.RolloutScalingImmediate, ScaleDownDuringRollout: disaggregatedsetv1.RolloutScalingImmediate},
	} {
		ds := newTwoRoleTestDisaggregatedSet([2]int32{4, 2}, [2]int{1, 1}, [2]int{})
		revision := disaggregatedsetutils.ComputeRevision(ds.Spec.Roles)
		legacyRevision := disaggregatedsetutils.ComputeRevisionV1(ds.Spec.Roles)
		ds.Spec.ScalingPolicy = policy
		assert.Equal(t, revision, disaggregatedsetutils.ComputeRevision(ds.Spec.Roles), "policy edits do not roll Pods")
		assert.Equal(t, legacyRevision, disaggregatedsetutils.ComputeRevisionV1(ds.Spec.Roles), "policy edits also preserve legacy revisions")
		objects := revisionLWSObjects("old", [2]int32{}, [2]int32{}, [2]int32{5, 3}, time.Now())
		objects = append(objects, revisionLWSObjects(revision, [2]int32{5, 3}, [2]int32{5, 3}, [2]int32{5, 3}, time.Now())...)
		c := newTestClient(objects...)
		r := newTestReconciler(c)
		_, err := r.reconcileSlice(context.Background(), r.createRollingUpdateExecutor(), ds, 0, revision, resolveDesiredReplicasByRole(ds, nil))
		require.NoError(t, err)
		want := [2]int32{5, 3}
		if scaleDownDuringRolloutEnabled(ds) {
			want = [2]int32{4, 2}
		}
		assertRevisionReplicas(t, c, revision, want)
	}
}

func TestScalingDuringRolloutPoliciesReachPlannerIndependently(t *testing.T) {
	for _, up := range []disaggregatedsetv1.RolloutScalingPolicy{"", disaggregatedsetv1.RolloutScalingDeferred, disaggregatedsetv1.RolloutScalingImmediate} {
		for _, down := range []disaggregatedsetv1.RolloutScalingPolicy{"", disaggregatedsetv1.RolloutScalingDeferred, disaggregatedsetv1.RolloutScalingImmediate} {
			t.Run(fmt.Sprintf("up=%s/down=%s", up, down), func(t *testing.T) {
				ds := newTwoRoleTestDisaggregatedSet([2]int32{8, 8}, [2]int{2, 2}, [2]int{1, 1})
				ds.Spec.ScalingPolicy = &disaggregatedsetv1.DisaggregatedSetScalingPolicy{ScaleUpDuringRollout: up, ScaleDownDuringRollout: down}
				objects := revisionLWSObjects("old", [2]int32{4, 4}, [2]int32{4, 4}, [2]int32{4, 4}, time.Now())
				objects = append(objects, revisionLWSObjects("target", [2]int32{2, 2}, [2]int32{1, 1}, [2]int32{8, 8}, time.Now())...)
				executor := newTestExecutor(newTestClient(objects...))
				old, target, err := executor.LWSManager.GetRevisionRolesList(t.Context(), ds, 0, "target")
				require.NoError(t, err)
				require.NotNil(t, target)
				inputs, err := executor.prepareRolloutInputs(t.Context(), ds, old, *target, resolveDesiredReplicasByRole(ds, nil))
				require.NoError(t, err)
				wantUp, wantDown := up == disaggregatedsetv1.RolloutScalingImmediate, down == disaggregatedsetv1.RolloutScalingImmediate
				assert.Equal(t, wantUp, inputs.scaleUpDuringRollout)
				assert.Equal(t, wantDown, inputs.scaleDownDuringRollout)
				_, state, step, err := executor.selectNextRolloutStep(t.Context(), orderedRevisionCandidates(old, inputs.readiness), old, *target, inputs)
				require.NoError(t, err)
				require.NotNil(t, step)
				assert.Equal(t, wantUp, state.ScaleUpDuringRollout)
				assert.Equal(t, wantDown, state.ScaleDownDuringRollout)
			})
		}
	}
}

func TestImmediateScaleUpUsesRequestedCapacity(t *testing.T) {
	for _, identity := range []leaderworkersetv1.GroupIdentityType{leaderworkersetv1.GroupIdentityOrdinal, leaderworkersetv1.GroupIdentityHash} {
		for _, down := range []disaggregatedsetv1.RolloutScalingPolicy{disaggregatedsetv1.RolloutScalingDeferred, disaggregatedsetv1.RolloutScalingImmediate} {
			for _, immediateUp := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/down=%s/immediateUp=%v", identity, down, immediateUp), func(t *testing.T) {
					ds := newTwoRoleTestDisaggregatedSet([2]int32{10, 10}, [2]int{1, 1}, [2]int{})
					ds.Spec.ScalingPolicy = &disaggregatedsetv1.DisaggregatedSetScalingPolicy{ScaleDownDuringRollout: down}
					if immediateUp {
						ds.Spec.ScalingPolicy.ScaleUpDuringRollout = disaggregatedsetv1.RolloutScalingImmediate
					}
					for i := range ds.Spec.Roles {
						ds.Spec.Roles[i].Spec.GroupIdentity = identity
					}
					objects := revisionLWSObjects("old", [2]int32{4, 4}, [2]int32{4, 4}, [2]int32{4, 4}, time.Now())
					objects = append(objects, revisionLWSObjects("target", [2]int32{2, 2}, [2]int32{}, [2]int32{10, 10}, time.Now())...)
					for _, object := range objects {
						object.(*leaderworkersetv1.LeaderWorkerSet).Spec.GroupIdentity = identity
					}
					c := newTestClient(objects...)
					executor := newTestExecutor(c)
					want := int32(2)
					for tick, desired := range []int32{10, 10, 12, 10, 14, 12, 14} {
						if tick == 1 {
							// Losing readiness does not free requested capacity.
							for _, role := range testRoleNames() {
								var old leaderworkersetv1.LeaderWorkerSet
								require.NoError(t, c.Get(t.Context(), client.ObjectKey{Namespace: testNamespace, Name: "test-0-old-" + role}, &old))
								old.Status.ReadyReplicas = 2
								require.NoError(t, c.Status().Update(t.Context(), &old))
							}
						}
						for i := range ds.Spec.Roles {
							ds.Spec.Roles[i].Spec.Replicas = ptr.To(desired)
						}
						if immediateUp {
							want = max(want, desired-4)
						}
						reconcileExistingForTest(t, executor, ds, "target")
						assertRevisionReplicas(t, c, "old", [2]int32{4, 4})
						assertRevisionReplicas(t, c, "target", [2]int32{want, want})
					}
					// Desired stays at 14. Observing old Spec fall from 4 to 2
					// must reopen Immediate's top-up from 10 to 12 target groups.
					for _, role := range testRoleNames() {
						var old leaderworkersetv1.LeaderWorkerSet
						require.NoError(t, c.Get(t.Context(), client.ObjectKey{Namespace: testNamespace, Name: "test-0-old-" + role}, &old))
						old.Spec.Replicas = ptr.To(int32(2))
						require.NoError(t, c.Update(t.Context(), &old))
					}
					if immediateUp {
						want = 12
					}
					reconcileExistingForTest(t, executor, ds, "target")
					assertRevisionReplicas(t, c, "old", [2]int32{2, 2})
					assertRevisionReplicas(t, c, "target", [2]int32{want, want})
				})
			}
		}
	}
}

func TestScalingDuringRolloutPrefersAnyOldDrainOverTargetShrink(t *testing.T) {
	now := time.Now()
	objects := revisionLWSObjects("hashB", [2]int32{1, 1}, [2]int32{1, 0}, [2]int32{1, 1}, now)
	objects = append(objects, revisionLWSObjects("hashA", [2]int32{2, 2}, [2]int32{2, 2}, [2]int32{2, 2}, now.Add(time.Hour))...)
	objects = append(objects, revisionLWSObjects("hashC", [2]int32{3, 3}, [2]int32{}, [2]int32{3, 3}, now.Add(2*time.Hour))...)
	c := newTestClient(objects...)
	ds := newTwoRoleTestDisaggregatedSet([2]int32{2, 2}, [2]int{1, 1}, [2]int{})
	ds.Spec.ScalingPolicy = &disaggregatedsetv1.DisaggregatedSetScalingPolicy{ScaleDownDuringRollout: disaggregatedsetv1.RolloutScalingImmediate}
	reconcileExistingForTest(t, newTestExecutor(c), ds, "hashC")
	assertRevisionReplicas(t, c, "hashA", [2]int32{2, 2})
	assertRevisionReplicas(t, c, "hashB", [2]int32{})
	assertRevisionReplicas(t, c, "hashC", [2]int32{3, 3})
}

func TestRolloutPrefersOldDrainOverTargetRebalance(t *testing.T) {
	// B can cancel unscheduled target surplus but cannot drain itself.
	// Both policies should prefer A's safe old Decode drain instead.
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
	readiness, err := executor.LWSManager.collectRolloutObservations(context.Background(), ds, old, *target, resolveDesiredReplicasByRole(ds, nil))
	require.NoError(t, err)
	name := target.Roles["new-prefill"].Name
	observed := readiness[name]
	observed.allUnscheduled = true
	readiness[name] = observed
	inputs := rolloutInputs{
		allRoleNames: append(testRoleNames(), "new-prefill", "new-decode"), targetReplicas: []int{0, 0, 4, 4},
		config: configs([]int{0, 0, 1, 1}, []int{1, 1, 1, 1}), readiness: readiness,
	}
	candidates := orderedRevisionCandidates(old, readiness)
	for _, immediate := range []bool{false, true} {
		t.Run(fmt.Sprintf("immediate=%v", immediate), func(t *testing.T) {
			inputs.scaleDownDuringRollout = immediate
			require.Equal(t, "hashB", candidates[0].Revision)
			first := rolloutStateForRevision(inputs.allRoleNames, old, candidates[0], *target, inputs.targetReplicas, inputs.config, readiness)
			first.ScaleDownDuringRollout = immediate
			repair := ComputeNextStep(first)
			require.Equal(t, &UpdateStep{Past: []int{1, 1, 0, 0}, New: []int{0, 0, 1, 1}, Kind: TargetRebalanceStep}, repair)
			require.NoError(t, validateUpdateStep(first, repair), "the competing target reduction must remain a safe option")
			selected, state, step, err := executor.selectNextRolloutStep(context.Background(), candidates, old, *target, inputs)
			require.NoError(t, err)
			require.Equal(t, "hashA", selected.Revision)
			require.Equal(t, &UpdateStep{Past: []int{4, 3, 0, 0}, New: []int{0, 0, 3, 1}}, step)
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
		return rolloutTargetReplicas(ds, []string{testRolePrefill}, sets.New(testRolePrefill), old, target, map[string]int{testRolePrefill: 2}, nil)
	}
	assert.Equal(t, []int{5}, readTarget(), "default keeps the in-flight External target")
	ds.Spec.ScalingPolicy = &disaggregatedsetv1.DisaggregatedSetScalingPolicy{ScaleUpDuringRollout: disaggregatedsetv1.RolloutScalingImmediate}
	assert.Equal(t, []int{5}, readTarget(), "immediate scale-up alone keeps the in-flight External target")
	ds.Spec.ScalingPolicy.ScaleDownDuringRollout = disaggregatedsetv1.RolloutScalingDeferred
	assert.Equal(t, []int{5}, readTarget(), "explicit Deferred scale-down keeps the in-flight External target too")
	ds.Spec.ScalingPolicy.ScaleDownDuringRollout = disaggregatedsetv1.RolloutScalingImmediate
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
			state.ScaleDownDuringRollout = true
			inputs := rolloutInputs{allRoleNames: testRoleNames(), scaleDownDuringRollout: true}
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

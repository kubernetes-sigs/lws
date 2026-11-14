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
	"maps"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/tools/events"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	disaggv1 "sigs.k8s.io/lws/api/disaggregatedset/v1"
	leaderv1 "sigs.k8s.io/lws/api/leaderworkerset/v1"
	disaggutils "sigs.k8s.io/lws/pkg/utils/disaggregatedset"
	podutils "sigs.k8s.io/lws/pkg/utils/pod"
)

func setCanaryPercent(ds *disaggv1.DisaggregatedSet, percent int32) {
	if ds.Spec.RolloutStrategy == nil {
		ds.Spec.RolloutStrategy = &disaggv1.DisaggregatedSetRolloutStrategy{}
	}
	ds.Spec.RolloutStrategy.CanaryPercent = ptr.To(percent)
}

func canaryConfigs(surge, unavailable int, old RoleReplicaState) []RollingUpdateConfig {
	config := make([]RollingUpdateConfig, len(old))
	for i, replicas := range old {
		config[i] = RollingUpdateConfig{MaxSurge: surge, MaxUnavailable: unavailable, CanaryOldReplicas: replicas, MinOldReplicas: replicas}
	}
	return config
}

func settleCanary(t *testing.T, state *RolloutState) {
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
	t.Fatal("canary did not settle")
}

func TestCanaryRoundingAndLogicalRoles(t *testing.T) {
	for _, tc := range [][3]int{{0, 25, 0}, {1, 25, 1}, {2, 1, 1}, {2, 99, 1}, {4, 0, 4}, {4, 25, 3}, {8, 25, 6}, {37, 25, 27}} {
		require.Equal(t, tc[2], canaryOldReplicas(tc[0], tc[1]))
	}
	ds := newDSWithRoles("ds", staticRole("prefill"), disaggv1.DisaggregatedRoleSpec{Name: "model", SubRoles: []disaggv1.DisaggregatedSubRoleSpec{{Name: "a"}, {Name: "b"}}})
	before := disaggutils.ComputeRevision(ds.Spec.Roles)
	for _, percent := range []int32{0, 25, 50, 99, 100} {
		setCanaryPercent(ds, percent)
		config := extractRollingUpdateConfig(ds, []string{"prefill", "model/a", "model/b"}, map[string]int{"prefill": 4, "model/a": 4, "model/b": 8})
		for i, desired := range []int{4, 4, 8} {
			want := 0
			if percent < 100 {
				want = canaryOldReplicas(desired, int(percent))
			}
			require.Equal(t, want, config[i].CanaryOldReplicas)
		}
		require.Equal(t, before, disaggutils.ComputeRevision(ds.Spec.Roles), "promotion must not restart the revision")
	}
	ds.Spec.RolloutStrategy = nil
	_, active := canaryPercent(ds)
	require.False(t, active)
}

func TestCanaryPlannerPromotesThroughHolds(t *testing.T) {
	for _, budget := range [][2]int{{1, 0}, {0, 1}} {
		state := newSimulationState([]int{4, 8}, []int{4, 8}, canaryConfigs(budget[0], budget[1], []int{0, 0}))
		state.ProportionalCanary = true
		for _, percent := range []int{0, 25, 50, 75, 100} {
			for i, desired := range state.Target.DesiredReplicas {
				state.Config[i].CanaryOldReplicas = 0
				if percent < 100 {
					state.Config[i].CanaryOldReplicas = canaryOldReplicas(desired, percent)
				}
				state.Config[i].MinOldReplicas = state.Config[i].CanaryOldReplicas
			}
			settleCanary(t, &state)
			for i, desired := range state.Target.DesiredReplicas {
				require.Equal(t, state.Config[i].CanaryOldReplicas, state.ActiveOld.SpecReplicas[i])
				require.Equal(t, desired-state.Config[i].CanaryOldReplicas, state.Target.SpecReplicas[i])
			}
		}
	}
}

func TestCanaryResizesBothRevisions(t *testing.T) {
	for _, identity := range []leaderv1.GroupIdentityType{leaderv1.GroupIdentityOrdinal, leaderv1.GroupIdentityHash} {
		for _, policy := range []disaggv1.RolloutScalingPolicy{disaggv1.RolloutScalingDeferred, disaggv1.RolloutScalingImmediate} {
			t.Run(fmt.Sprintf("%s/%s", identity, policy), func(t *testing.T) {
				ds := newDSWithRoles("test", staticRole("prefill"), staticRole("decode"))
				ds.UID = "uid"
				setCanaryPercent(ds, 25)
				ds.Spec.ScalingPolicy = &disaggv1.DisaggregatedSetScalingPolicy{ScaleUpDuringRollout: policy, ScaleDownDuringRollout: policy}
				objects := []client.Object{ds}
				for i, name := range testRoleNames() {
					ds.Spec.Roles[i].Spec.RolloutStrategy.RollingUpdateConfiguration = &leaderv1.RollingUpdateConfiguration{MaxSurge: intstr.FromInt(1)}
					for _, revision := range []string{"old", "new"} {
						n := int32(0)
						if revision == "old" {
							n = int32(4 * (i + 1))
						}
						lws := revisionLWS(revision, name, n, n, time.Now(), int32(4*(i+1)))
						lws.Spec.GroupIdentity = identity
						lws.Spec.RolloutStrategy.RollingUpdateConfiguration = &leaderv1.RollingUpdateConfiguration{MaxSurge: intstr.FromInt(2)}
						objects = append(objects, lws)
					}
				}
				c := newTestClient(objects...)
				executor := newTestExecutor(c)
				recorder := events.NewFakeRecorder(100)
				executor.Record = recorder
				settledCounts := func(desired map[string]int) (oldCounts, newCounts []int) {
					t.Helper()
					for range 100 {
						old, target, err := executor.LWSManager.GetRevisionRolesList(t.Context(), ds, 0, "new")
						require.NoError(t, err)
						previous := make([]int, 2)
						for i, name := range testRoleNames() {
							previous[i] = old.GetTotalReplicasPerRole(name) + int(getLWSReplicas(target.Roles[name]))
						}
						_, _, err = executor.reconcileExistingRollout(t.Context(), ds, old, *target, desired)
						require.NoError(t, err)
						require.Empty(t, executor.canaryMessage)
						emitted := ""
						for len(recorder.Events) > 0 {
							emitted += <-recorder.Events
						}
						list := &leaderv1.LeaderWorkerSetList{}
						require.NoError(t, c.List(t.Context(), list))
						changed := false
						oldCounts, newCounts = make([]int, 2), make([]int, 2)
						readyBefore, readyAfter := make([]int, 2), make([]int, 2)
						for i := range list.Items {
							lws := &list.Items[i]
							n := *lws.Spec.Replicas
							role := 0
							if lws.Labels[disaggv1.RoleLabelKey] == "decode" {
								role = 1
							}
							if lws.Labels[disaggv1.RevisionLabelKey] == "old" {
								oldCounts[role] = int(n)
								require.Equal(t, intstr.FromInt(2), lws.Spec.RolloutStrategy.RollingUpdateConfiguration.MaxSurge, "old growth retains old budgets")
								require.Equal(t, fmt.Sprint(4*(role+1)), lws.Annotations[disaggv1.InitialReplicasAnnotationKey])
							} else {
								newCounts[role] = int(n)
							}
							changed = changed || n != lws.Status.ReadyReplicas
							readyBefore[role] += int(lws.Status.ReadyReplicas)
							readyAfter[role] += int(min(n, lws.Status.ReadyReplicas))
							lws.Status.Replicas, lws.Status.ReadyReplicas = n, n
							require.NoError(t, c.Status().Update(t.Context(), lws))
						}
						for i, name := range testRoleNames() {
							require.GreaterOrEqual(t, readyAfter[i], min(readyBefore[i], desired[name]), "scale requests preserve the availability floor before new groups are Ready")
							if oldCounts[i]+newCounts[i] > max(previous[i], desired[name]+1) {
								require.Contains(t, emitted, EventReasonBootstrapSurge, "exceeding ordinary surge must use the existing explicit recovery")
								require.Contains(t, emitted, name)
								require.LessOrEqual(t, oldCounts[i]+newCounts[i], desired[name]+2, "at most one extra bootstrap group")
							}
						}
						if !changed {
							return
						}
					}
					t.Fatal("canary did not settle")
					return
				}
				for _, phase := range []struct {
					percent           int32
					desired, old, new [2]int
					checkRetained     bool
				}{
					{25, [2]int{4, 8}, [2]int{3, 6}, [2]int{1, 2}, false},
					{50, [2]int{4, 8}, [2]int{2, 4}, [2]int{2, 4}, true},
					{25, [2]int{4, 8}, [2]int{3, 6}, [2]int{1, 2}, true},
					{0, [2]int{4, 8}, [2]int{4, 8}, [2]int{0, 0}, true},
					{25, [2]int{4, 8}, [2]int{3, 6}, [2]int{1, 2}, true},
					{25, [2]int{8, 16}, [2]int{6, 12}, [2]int{2, 4}, false},
					{25, [2]int{4, 8}, [2]int{3, 6}, [2]int{1, 2}, false},
					{25, [2]int{20, 8}, [2]int{15, 6}, [2]int{5, 2}, false},
					{50, [2]int{20, 8}, [2]int{10, 4}, [2]int{10, 4}, false},
					{50, [2]int{16, 8}, [2]int{8, 4}, [2]int{8, 4}, false},
					{100, [2]int{16, 8}, [2]int{0, 0}, [2]int{16, 8}, false},
					// Once history is fully drained, lowering the percentage cannot revive it.
					{25, [2]int{24, 16}, [2]int{0, 0}, [2]int{24, 16}, false},
				} {
					setCanaryPercent(ds, phase.percent)
					a, b := settledCounts(map[string]int{"prefill": phase.desired[0], "decode": phase.desired[1]})
					require.Equal(t, phase.old[:], a, "old groups: percent=%d desired=%v", phase.percent, phase.desired)
					require.Equal(t, phase.new[:], b, "new groups: percent=%d desired=%v", phase.percent, phase.desired)
					if phase.checkRetained {
						list := &leaderv1.LeaderWorkerSetList{}
						require.NoError(t, c.List(t.Context(), list))
						require.Len(t, list.Items, 4, "zero holds retain both target LWS objects")
					}
				}
			})
		}
	}
}

func TestCanaryRestrictionsAndRetiredHistory(t *testing.T) {
	for _, tc := range []struct {
		name                       string
		percent, desired, old, new int
		oldName, message           string
	}{
		{"zero holds a single old group", 0, 1, 1, 0, "prefill", ""},
		{"layout change is explicit", 25, 4, 4, 0, "previous", "same active roles"},
		{"promotion permits layout change", 100, 4, 4, 0, "previous", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ds := newDSWithRoles("test", staticRole("prefill"))
			ds.UID = "uid"
			setCanaryPercent(ds, int32(tc.percent))
			a := revisionLWS("old", tc.oldName, int32(tc.old), int32(tc.old), time.Now(), int32(tc.old))
			a.Annotations[canaryStableAnnotation] = "true"
			b := revisionLWS("new", "prefill", int32(tc.new), int32(tc.new), time.Now(), int32(tc.desired))
			old := disaggutils.RevisionRolesList{{Revision: "old", Roles: map[string]*leaderv1.LeaderWorkerSet{tc.oldName: a}}}
			target := disaggutils.RevisionRoles{Revision: "new", Roles: map[string]*leaderv1.LeaderWorkerSet{"prefill": b}}
			c := newTestClient(ds, a, b)
			executor := newTestExecutor(c)
			desired := map[string]int{"prefill": tc.desired}
			inputs, err := executor.prepareRolloutInputs(t.Context(), ds, old, target, desired)
			require.NoError(t, err)
			result, handled, err := executor.reconcileCanaryRestoration(t.Context(), ds, old, target, desired, inputs)
			require.NoError(t, err)
			require.Zero(t, result.RequeueAfter, "unchanged history waits for an object event")
			require.Equal(t, tc.message != "", handled)
			if tc.message != "" {
				require.Contains(t, executor.canaryMessage, tc.message)
				r := newTestReconciler(c)
				require.NoError(t, r.updateStatus(t.Context(), ds, []string{"prefill"}, "new", desired, nil, ptr.To(false), executor.canaryMessage))
				require.Equal(t, "RolloutInProgress", meta.FindStatusCondition(ds.Status.Conditions, "Progressing").Reason)
			}
			stored := &leaderv1.LeaderWorkerSet{}
			require.NoError(t, c.Get(t.Context(), client.ObjectKeyFromObject(a), stored))
			require.EqualValues(t, tc.old, *stored.Spec.Replicas)
		})
	}
}

func TestCanaryExistingRolesCanResize(t *testing.T) {
	for _, tc := range []struct {
		name                                             string
		initial, old, current, desired, wantOld, wantNew [2]int32
	}{
		{"initially zero", [2]int32{4, 0}, [2]int32{3, 0}, [2]int32{1, 0}, [2]int32{4, 2}, [2]int32{3, 1}, [2]int32{1, 1}},
		{"interrupted drain", [2]int32{4, 8}, [2]int32{0, 6}, [2]int32{1, 2}, [2]int32{4, 8}, [2]int32{3, 6}, [2]int32{1, 2}},
		{"scale one role to zero", [2]int32{4, 8}, [2]int32{3, 6}, [2]int32{1, 2}, [2]int32{4, 0}, [2]int32{3, 0}, [2]int32{1, 0}},
		{"singleton keeps serving while peers grow", [2]int32{4, 8}, [2]int32{3, 6}, [2]int32{1, 2}, [2]int32{1, 16}, [2]int32{1, 16}, [2]int32{0, 0}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ds := newTwoRoleTestDisaggregatedSet(tc.desired, [2]int{1, 1}, [2]int{1, 1})
			setCanaryPercent(ds, 25)
			objects := revisionLWSObjects("old", tc.old, tc.old, tc.initial, time.Unix(1, 0))
			for _, object := range objects {
				object.(*leaderv1.LeaderWorkerSet).Annotations[canaryStableAnnotation] = "true"
			}
			objects = append(objects, revisionLWSObjects("new", tc.current, tc.current, tc.desired, time.Unix(2, 0))...)
			c := newTestClient(objects...)
			executor := newTestExecutor(c)
			for range 40 {
				reconcileExistingForTest(t, executor, ds, "new")
				if tc.desired[0] == 1 {
					require.Contains(t, executor.canaryMessage, "scaling continues")
				} else {
					require.Empty(t, executor.canaryMessage)
				}
				list := &leaderv1.LeaderWorkerSetList{}
				require.NoError(t, c.List(t.Context(), list))
				for i := range list.Items {
					lws := &list.Items[i]
					lws.Status.Replicas, lws.Status.ReadyReplicas = *lws.Spec.Replicas, *lws.Spec.Replicas
					require.NoError(t, c.Status().Update(t.Context(), lws))
				}
			}
			assertRevisionReplicas(t, c, "old", tc.wantOld)
			assertRevisionReplicas(t, c, "new", tc.wantNew)
			statuses, _, err := executor.LWSManager.collectRoleStatuses(t.Context(), ds, testRoleNames(), "new", resolveDesiredReplicasByRole(ds, nil))
			require.NoError(t, err)
			for i, status := range statuses {
				require.Equal(t, tc.desired[i], status.AvailableReplicas)
			}
			if tc.desired[0] == 1 {
				require.NoError(t, c.Create(t.Context(), ds))
				r := newTestReconciler(c)
				require.NoError(t, r.updateStatus(t.Context(), ds, testRoleNames(), "new", resolveDesiredReplicasByRole(ds, nil), nil, ptr.To(true), executor.canaryMessage))
				require.Equal(t, "RolloutInProgress", progressingReason(ds), "scaling has settled, but no canary has been tested")
			}
		})
	}
}

func TestCanaryNativeGroupsAndSubRoles(t *testing.T) {
	for _, identity := range []leaderv1.GroupIdentityType{leaderv1.GroupIdentityOrdinal, leaderv1.GroupIdentityHash} {
		for _, subRoles := range []bool{false, true} {
			for _, dimension := range []disaggv1.CanaryDimension{disaggv1.CanaryDimensionGroups, disaggv1.CanaryDimensionSlices} {
				t.Run(fmt.Sprintf("%s/subRoles=%t/%s", identity, subRoles, dimension), func(t *testing.T) {
					t.Parallel()

					f := newSubRoleFixtureForIdentity(t, identity, 0, "a", "b")
					old := f.withRevision("old", "a", "a", "a", "b", "b", "b")
					old.lws.Annotations[canaryStableAnnotation] = "true"
					require.NoError(t, f.manager.client.Update(t.Context(), old.lws))
					f.ds.Spec.Roles = []disaggv1.DisaggregatedRoleSpec{{Name: "model", SubRoles: []disaggv1.DisaggregatedSubRoleSpec{{Name: "a"}, {Name: "b"}}}}
					if !subRoles {
						f.ds.Spec.Roles = []disaggv1.DisaggregatedRoleSpec{staticRole("model")}
						// Reuse the native workload simulator, without subrole annotations
						// or assignments. Readiness comes from real Pod observations.
						for _, fixture := range []*subRoleFixture{old, f} {
							delete(fixture.lws.Annotations, subRoleReplicasAnnotation)
							delete(fixture.lws.Annotations, disaggv1.InitialSubRoleReplicasAnnotationKey)
							require.NoError(t, f.manager.client.Update(t.Context(), fixture.lws))
							for _, group := range fixture.observe().Groups {
								for _, pod := range group.Pods {
									delete(pod.Labels, disaggv1.SubRoleLabelKey)
									require.NoError(t, f.manager.client.Update(t.Context(), pod))
								}
							}
						}
					}
					setCanaryPercent(f.ds, 25)
					f.ds.Spec.RolloutStrategy.CanaryDimension = dimension
					f.ds.Spec.Slices = ptr.To[int32](2)
					require.NoError(t, f.manager.client.Update(t.Context(), f.ds))
					initial := old.lws.Annotations[disaggv1.InitialSubRoleReplicasAnnotationKey]
					executor := &RollingUpdateExecutor{LWSManager: f.manager, Record: events.NewFakeRecorder(100)}
					// Keep counts small: every group has a leader and two workers.
					for _, phase := range []struct {
						percent           int32
						desired, old, new [2]int
					}{
						{25, [2]int{8, 8}, [2]int{6, 6}, [2]int{2, 2}},
						{25, [2]int{12, 8}, [2]int{9, 6}, [2]int{3, 2}},
						{50, [2]int{12, 8}, [2]int{6, 4}, [2]int{6, 4}},
						{25, [2]int{12, 8}, [2]int{9, 6}, [2]int{3, 2}},
						{0, [2]int{12, 8}, [2]int{12, 8}, [2]int{0, 0}},
						{50, [2]int{8, 8}, [2]int{4, 4}, [2]int{4, 4}},
						{100, [2]int{8, 8}, [2]int{0, 0}, [2]int{8, 8}},
					} {
						setCanaryPercent(f.ds, phase.percent)
						if dimension == disaggv1.CanaryDimensionSlices && phase.percent > 0 {
							// This is selected slice 0. Reversing to 0 must restore its
							// baseline from zero through the real native adapters.
							phase.old, phase.new = [2]int{}, phase.desired
						}
						desired := map[string]int{"model": phase.desired[0] + phase.desired[1], "model/a": phase.desired[0], "model/b": phase.desired[1]}
						if !subRoles {
							delete(desired, "model/a")
							delete(desired, "model/b")
						}
						settled := false
						last := [2]string{}
						for range 80 {
							_, _, err := executor.reconcileExistingRollout(t.Context(), f.ds, disaggutils.RevisionRolesList{old.revision("old")}, f.revision("model"), desired)
							require.NoError(t, err)
							require.Empty(t, executor.canaryMessage)
							settled = true
							for index, fixture := range []*subRoleFixture{old, f} {
								fixture.native()
								if subRoles {
									fixture.sync()
								}
								observed := fixture.observe()
								actual := map[string]int{"a": 0, "b": 0}
								groups := activeSubRoleGroups(observed)
								for _, group := range groups {
									if subRoles {
										child, coherent := groupSubRole(group)
										settled = settled && coherent
										actual[child]++
									}
									for _, pod := range group.Pods {
										if podutils.PodRunningAndReady(*pod) {
											continue
										}
										settled = false
										pod.Status.Phase = corev1.PodRunning
										pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
										require.NoError(t, f.manager.client.Status().Update(t.Context(), pod))
									}
								}
								want := [2][2]int{phase.old, phase.new}[index]
								last[index] = fmt.Sprintf("%s: issued=%d groups=%d", fixture.lws.Name, getLWSReplicas(observed.LWS), len(groups))
								settled = settled && int(getLWSReplicas(observed.LWS)) == want[0]+want[1] && len(groups) == want[0]+want[1]
								if subRoles {
									counts, err := subRoleCounts(observed.LWS, subRoleReplicasAnnotation)
									require.NoError(t, err)
									settled = settled && maps.Equal(counts, map[string]int{"a": want[0], "b": want[1]}) && maps.Equal(counts, actual)
								}
							}
							if settled {
								break
							}
						}
						require.True(t, settled, "both revisions must have coherent assignments and exact counts: percent=%d desired=%v old=%v new=%v; %v", phase.percent, phase.desired, phase.old, phase.new, last)
					}
					require.Equal(t, initial, old.observe().LWS.Annotations[disaggv1.InitialSubRoleReplicasAnnotationKey])
				})
			}
		}
	}
}

func TestCanaryInitialCreationIgnoresHold(t *testing.T) {
	ds := newDSWithRoles("ds", staticRole("prefill"))
	ds.Spec.Roles[0].Spec.Replicas = ptr.To[int32](4)
	setCanaryPercent(ds, 0)
	ds.Annotations = map[string]string{disaggv1.RevisionHashVersionAnnotationKey: disaggv1.RevisionHashVersion}
	c := newTestClient(ds)
	r := newTestReconciler(c)
	for range 4 {
		_, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(ds)})
		require.NoError(t, err)
	}
	list := &leaderv1.LeaderWorkerSetList{}
	require.NoError(t, c.List(t.Context(), list))
	require.Len(t, list.Items, 1)
	require.EqualValues(t, 4, *list.Items[0].Spec.Replicas)
}

func TestCanaryStableGrowthCountsEveryOldRevision(t *testing.T) {
	ds := newDSWithRoles("test", staticRole("prefill"))
	ds.UID = "uid"
	setCanaryPercent(ds, 25)
	desired := map[string]int{"prefill": 8}
	old := disaggutils.RevisionRolesList{}
	objects := []client.Object{ds}
	for i, name := range []string{"stable", "previous-canary", "current"} {
		n := int32(1)
		if i == 0 {
			n = 2
		}
		lws := revisionLWS(name, "prefill", n, n, time.Unix(int64(i), 0), 4)
		if i == 0 {
			lws.Annotations[canaryStableAnnotation] = "true"
		}
		objects = append(objects, lws)
		old = append(old, disaggutils.RevisionRoles{Revision: name, Roles: map[string]*leaderv1.LeaderWorkerSet{"prefill": lws}})
	}
	target, old := old[2], old[:2]
	c := newTestClient(objects...)
	executor := newTestExecutor(c)
	inputs, err := executor.prepareRolloutInputs(t.Context(), ds, old, target, desired)
	require.NoError(t, err)
	_, handled, err := executor.reconcileCanaryRestoration(t.Context(), ds, old, target, desired, inputs)
	require.NoError(t, err)
	require.True(t, handled)
	for i, want := range []int32{6, 1} {
		stored := &leaderv1.LeaderWorkerSet{}
		require.NoError(t, c.Get(t.Context(), client.ObjectKeyFromObject(old[i].Roles["prefill"]), stored))
		require.Equal(t, want, *stored.Spec.Replicas, "replenish the anchor's own share; another old revision belongs to the canary side")
	}
}

func TestCanaryIndependentRoleScaleUpCanPromote(t *testing.T) {
	// At 25%, only Prefill scaled from 4 to 20 while Decode stayed at 8.
	// Stable growth left 15/6 old and 5/2 new. Now promote to 50%.
	for _, ordinary := range []bool{false, true} {
		t.Run(fmt.Sprintf("ordinary=%t", ordinary), func(t *testing.T) {
			state := rolloutState([]int{4, 8}, []int{15, 6}, []int{15, 6}, nil, nil, []int{5, 2}, []int{5, 2}, []int{20, 8}, canaryConfigs(1, 0, []int{10, 4}))
			state.ProportionalCanary = true
			for range 100 {
				if ordinary {
					// Ordinary observations retain at least the current Spec as baseline.
					state.ActiveOld.InitialReplicas = []int{max(4, state.ActiveOld.SpecReplicas[0]), max(8, state.ActiveOld.SpecReplicas[1])}
				}
				step := ComputeNextStep(state)
				if step == nil {
					break
				}
				require.NoError(t, validateUpdateStep(state, step))
				state.ActiveOld.observeFullyReady(step.Past)
				state.Target.observeFullyReady(step.New)
			}
			require.Equal(t, []int{10, 4}, state.ActiveOld.SpecReplicas)
			require.Equal(t, []int{10, 4}, state.Target.SpecReplicas)
		})
	}
}

func TestCanaryCoordinatesCanaryHistoryWithoutDrainingAnchor(t *testing.T) {
	desired := []int{20, 8}
	old := [][]int{{10, 4}, {5, 2}}
	target := []int{5, 2}
	for range 100 {
		var selected *UpdateStep
		var selectedState RolloutState
		selectedIndex := 0
		for active := range old {
			parked := old[1-active]
			state := rolloutState([]int{4, 8}, old[active], old[active], parked, parked, target, target, desired, canaryConfigs(1, 0, []int{10, 4}))
			state.ProportionalCanary = true
			if active != 0 {
				for i := range state.Config {
					state.Config[i].MinOldReplicas = 0
				}
			}
			step := ComputeNextStep(state)
			if step == nil {
				continue
			}
			// Match executor priority: any ordinary candidate beats bootstrap surge.
			if selected == nil || step.Kind != BootstrapSurgeStep {
				selected, selectedState, selectedIndex = step, state, active
			}
			if step.Kind != BootstrapSurgeStep {
				break
			}
		}
		if selected == nil {
			break
		}
		require.NoError(t, validateUpdateStep(selectedState, selected))
		bounds, _, err := prepareRolloutPlan(selectedState).boundsForStep(selected)
		require.NoError(t, err)
		for i, d := range desired {
			parked := old[1-selectedIndex][i]
			require.GreaterOrEqual(t, selected.Past[i]+parked+target[i], d, "no new readiness is spent before it exists")
			require.LessOrEqual(t, selected.Past[i]+parked+selected.New[i], max(old[selectedIndex][i]+parked+target[i], bounds.surgeCeiling(i)))
		}
		old[selectedIndex], target = selected.Past, selected.New
		require.Equal(t, []int{10, 4}, old[0], "the stable anchor retains its own share")
	}
	require.Equal(t, []int{10, 4}, target)
	require.Equal(t, []int{10, 4}, []int{old[0][0] + old[1][0], old[0][1] + old[1][1]})
}

func TestCanaryHoldRequiresEveryGroupAndSlice(t *testing.T) {
	for _, tc := range []struct {
		oldReady, targetSpec, targetReady RoleReplicaState
		held                              bool
	}{
		{[]int{3, 6}, []int{1, 2}, []int{1, 2}, true},
		{[]int{3, 6}, []int{1, 2}, []int{1, 1}, false},
		{[]int{3, 5}, []int{1, 2}, []int{1, 2}, false},
		{[]int{3, 6}, []int{0, 2}, []int{0, 2}, false},
		{[]int{3, 6}, []int{2, 2}, []int{2, 2}, false},
	} {
		state := rolloutState([]int{4, 8}, []int{3, 6}, tc.oldReady, nil, nil, tc.targetSpec, tc.targetReady, []int{4, 8}, canaryConfigs(1, 0, []int{3, 6}))
		require.Equal(t, tc.held, heldAtCanary(state))
	}
	executor := &RollingUpdateExecutor{rollingSlices: 2, heldSlices: 1}
	require.False(t, executor.CanaryHeld())
	executor.heldSlices = 2
	require.True(t, executor.CanaryHeld())
	require.False(t, (&RollingUpdateExecutor{}).CanaryHeld())
}

func TestCanaryHoldWakesOnEvents(t *testing.T) {
	for _, ready := range []int32{0, 2} {
		desired := [2]int32{4, 8}
		ds := newTwoRoleTestDisaggregatedSet(desired, [2]int{1, 1}, [2]int{})
		setCanaryPercent(ds, 25)
		objects := revisionLWSObjects("old", [2]int32{3, 6}, [2]int32{3, 6}, desired, time.Unix(1, 0))
		objects = append(objects, revisionLWSObjects("new", [2]int32{1, 2}, [2]int32{1, ready}, desired, time.Unix(2, 0))...)
		executor := newTestExecutor(newTestClient(objects...))
		result, _ := reconcileExistingForTest(t, executor, ds, "new")
		if ready == 2 {
			require.Zero(t, result.RequeueAfter, "a settled hold has no grace period to poll")
		} else {
			require.Positive(t, result.RequeueAfter, "unfinished groups still need scheduler grace checks")
		}
	}
}

func TestCanaryHoldCondition(t *testing.T) {
	for _, tc := range []struct {
		name                     string
		percent                  int32
		held, available          bool
		replicas, ready, updated int32
		reason                   string
	}{
		{"ready hold", 25, true, false, 8, 8, 2, reasonCanaryHeld},
		{"zero hold across two slices", 0, true, false, 8, 8, 0, reasonCanaryHeld},
		{"counts alone are not a hold", 25, false, false, 8, 8, 2, "RolloutInProgress"},
		{"surge is still draining", 25, true, false, 9, 8, 2, "RolloutInProgress"},
		{"group is not ready", 25, true, false, 8, 7, 2, "RolloutInProgress"},
		{"no global hold", 100, true, false, 8, 8, 2, "RolloutInProgress"},
		{"completed rollout", 25, true, true, 8, 8, 8, "AllRolesReady"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ds := newDSWithRoles("ds", staticRole("prefill"))
			ds.Spec.Slices = ptr.To[int32](2)
			setCanaryPercent(ds, tc.percent)
			statuses := []disaggv1.RoleStatus{{Name: "prefill", DesiredReplicas: ptr.To[int32](8), Replicas: tc.replicas, ReadyReplicas: tc.ready, UpdatedReplicas: tc.updated}}
			condition := disaggregatedSetCondition(ds, statuses, tc.available, nil, tc.held)
			require.Equal(t, tc.reason, condition.Reason)
			if tc.reason == reasonCanaryHeld {
				require.Contains(t, condition.Message, fmt.Sprintf("canaryPercent %d", tc.percent))
				require.Contains(t, condition.Message, fmt.Sprintf("prefill %d/8", tc.updated))
			}
		})
	}
}

func TestCanaryForwardPlannerDoesNotSpendTheHeldShare(t *testing.T) {
	// Lowering the percentage does not authorize the forward planner to drain
	// stable groups. The reverse pass must handle the surplus current groups.
	state := rolloutState([]int{4, 4}, []int{3, 3}, []int{3, 3}, nil, nil, []int{2, 2}, []int{2, 2}, []int{4, 4}, canaryConfigs(1, 0, []int{3, 3}))
	state.ProportionalCanary = true
	require.Nil(t, ComputeNextStep(state))
	// Restore the original template: its three groups are target again. The
	// forward planner cannot drain the canary's held group until full promotion.
	state = rolloutState([]int{1, 1}, []int{1, 1}, []int{1, 1}, nil, nil, []int{3, 3}, []int{3, 3}, []int{4, 4}, canaryConfigs(1, 0, []int{3, 3}))
	state.ProportionalCanary = true
	settleCanary(t, &state)
	require.Equal(t, []int{1, 1}, state.ActiveOld.SpecReplicas)
	state.Config, state.ProportionalCanary = canaryConfigs(1, 0, []int{0, 0}), false
	settleCanary(t, &state)
	require.Equal(t, []int{0, 0}, state.ActiveOld.SpecReplicas)
	require.Equal(t, []int{4, 4}, state.Target.SpecReplicas)
}

func TestCanaryKeepsAnchorDuringNestedRollout(t *testing.T) {
	for _, budget := range [][2]int{{1, 0}, {0, 1}} {
		t.Run(fmt.Sprint(budget), func(t *testing.T) {
			// A holds half of each role throughout B -> C. Only the other half
			// participates in this nested rolling update.
			inputs, old, target := canaryReverseTestInputs([]int{40, 40}, []int{20, 20}, []int{20, 20}, []int{0, 0}, budget[0], budget[1])
			inputs.stableRevision = old[0].Revision
			inputs.proportionalCanary, inputs.scaleUpDuringRollout, inputs.scaleDownDuringRollout = true, true, true
			for i := range inputs.config {
				inputs.config[i].CanaryOldReplicas = 20
			}
			for range 200 {
				selected, state, step, err := (&RollingUpdateExecutor{}).selectNextRolloutStep(t.Context(), orderedRevisionCandidates(old, inputs.readiness), old, target, inputs)
				require.NoError(t, err)
				if step == nil {
					break
				}
				require.NoError(t, validateUpdateStep(state, step))
				for i, name := range inputs.allRoleNames {
					selected.Roles[name].Spec.Replicas = ptr.To(int32(step.Past[i]))
					target.Roles[name].Spec.Replicas = ptr.To(int32(step.New[i]))
					for _, lws := range []*leaderv1.LeaderWorkerSet{selected.Roles[name], target.Roles[name]} {
						count := int(*lws.Spec.Replicas)
						inputs.readiness[lws.Name] = replicaReadiness{raw: count, committed: count}
					}
					require.EqualValues(t, 20, *old[0].Roles[name].Spec.Replicas, "the stable anchor must not be replaced by B or C")
				}
			}
			for _, name := range inputs.allRoleNames {
				require.EqualValues(t, 0, *old[1].Roles[name].Spec.Replicas)
				require.EqualValues(t, 20, *target.Roles[name].Spec.Replicas)
			}
		})
	}
}

func TestCanaryReverseDoesNotInventStableReadiness(t *testing.T) {
	for _, raw := range [][]int{{4, 0}, {4, 4}} {
		t.Run(fmt.Sprint(raw), func(t *testing.T) {
			inputs, old, target := canaryReverseTestInputs([]int{12, 12}, []int{4, 4}, []int{4, 4}, []int{4, 4}, 1, 0)
			for revision, counts := range [][]int{{4, 0}, {0, 4}} {
				for i, name := range inputs.allRoleNames {
					// Each old revision is missing a different serving role. In the
					// second case all groups still look Ready before pending deletion.
					observedRaw := raw[i]
					if revision == 1 {
						observedRaw = raw[1-i]
					}
					inputs.readiness[old[revision].Roles[name].Name] = replicaReadiness{raw: observedRaw, committed: counts[i]}
				}
			}
			state := canaryRestorationState(old, target, old[0], old[1], inputs)
			snapshot := snapshotForRolloutState(state)
			for i := range inputs.allRoleNames {
				require.Zero(t, snapshot.targetReady[i].usable, "the anchor is incomplete")
				require.Zero(t, snapshot.activeReady[i].usable, "another incomplete revision cannot complete it")
				require.Equal(t, 4, snapshot.old.parkedReady[i].usable, "only the complete current revision serves")
			}
			if step := ComputeNextStep(state); step != nil {
				require.NoError(t, validateUpdateStep(state, step))
				require.Equal(t, state.ActiveOld.SpecReplicas[1], step.Past[1], "reserved or non-serving old readiness cannot fund a Ready-group deletion")
			}
		})
	}
}

func TestCanaryMixedScalingAndRestart(t *testing.T) {
	ds := newDSWithRoles("test", staticRole("prefill"), staticRole("decode"))
	ds.UID = "uid"
	objects := []client.Object{ds}
	for i, name := range testRoleNames() {
		ds.Spec.Roles[i].Spec.RolloutStrategy.RollingUpdateConfiguration = &leaderv1.RollingUpdateConfiguration{MaxSurge: intstr.FromInt(1)}
		objects = append(objects, revisionLWS("old", name, 6, 6, time.Unix(0, 0), 8), revisionLWS("new", name, 2, 2, time.Unix(1, 0), 8))
	}
	c := newTestClient(objects...)
	for _, percent := range []int32{25, 0, 25} {
		setCanaryPercent(ds, percent)
		// Default Deferred is overridden only for this explicit canary. One role
		// must grow from 8 to20 while the other shrinks from 8 to4.
		desired := map[string]int{"prefill": 20, "decode": 4}
		executor := newTestExecutor(c) // Recreate between phases: no stored direction.
		settled := false
		for range 100 {
			old, target, err := executor.LWSManager.GetRevisionRolesList(t.Context(), ds, 0, "new")
			require.NoError(t, err)
			_, _, err = executor.reconcileExistingRollout(t.Context(), ds, old, *target, desired)
			require.NoError(t, err)
			require.Empty(t, executor.canaryMessage)
			list := &leaderv1.LeaderWorkerSetList{}
			require.NoError(t, c.List(t.Context(), list))
			settled = true
			for i := range list.Items {
				lws := &list.Items[i]
				role := lws.Labels[disaggv1.RoleLabelKey]
				want := canaryOldReplicas(desired[role], int(percent))
				if lws.Labels[disaggv1.RevisionLabelKey] == "new" {
					want = desired[role] - want
				}
				settled = settled && int(*lws.Spec.Replicas) == want && lws.Status.ReadyReplicas == *lws.Spec.Replicas
				lws.Status.Replicas, lws.Status.ReadyReplicas = *lws.Spec.Replicas, *lws.Spec.Replicas
				require.NoError(t, c.Status().Update(t.Context(), lws))
			}
			if settled {
				break
			}
		}
		require.True(t, settled, "percent=%d", percent)
		// At0, cleanup must retain the actual current revision's empty LWS.
		require.NoError(t, newTestReconciler(c).cleanupDrainedLWS(t.Context(), ds, 0, "new", false))
		for _, name := range testRoleNames() {
			lws := &leaderv1.LeaderWorkerSet{}
			require.NoError(t, c.Get(t.Context(), client.ObjectKey{Namespace: testNamespace, Name: "test-0-new-" + name}, lws))
			require.EqualValues(t, lws.Name, lws.UID, "re-promotion reuses the dormant current revision")
			require.Equal(t, fmt.Sprint(desired[name]), lws.Annotations[disaggv1.InitialReplicasAnnotationKey])
		}
	}
}

func TestCanaryFailedOldGroupsStillHoldWithFullUnavailability(t *testing.T) {
	for _, ready := range [][]int{{0, 0}, {0, 8}} {
		state := rolloutState([]int{8, 8}, []int{8, 8}, ready, nil, nil, []int{0, 0}, []int{0, 0}, []int{8, 8}, canaryConfigs(0, 8, []int{6, 6}))
		state.ProportionalCanary = true
		for range 20 {
			step := ComputeNextStep(state)
			if step == nil {
				break
			}
			require.NoError(t, validateUpdateStep(state, step))
			for i := range step.Past {
				require.GreaterOrEqual(t, step.Past[i], 6, "failed readiness is not permission to delete held history")
				state.ActiveOld.SpecReplicas[i] = step.Past[i]
				state.ActiveOld.ReadyReplicas[i] = min(ready[i], step.Past[i])
				state.ActiveOld.RawReadyReplicas[i] = state.ActiveOld.ReadyReplicas[i]
			}
			state.Target.observeFullyReady(step.New)
		}
		require.Equal(t, []int{6, 6}, state.ActiveOld.SpecReplicas)
		require.Equal(t, []int{2, 2}, state.Target.SpecReplicas)
		require.False(t, heldAtCanary(state), "an unready hold cannot report completion")
	}
}

func TestCanaryReverseReservesPendingCurrentDeletion(t *testing.T) {
	inputs, old, target := canaryReverseTestInputs([]int{8, 8}, []int{4, 4}, []int{0, 0}, []int{3, 3}, 1, 0)
	for _, name := range inputs.allRoleNames {
		inputs.readiness[target.Roles[name].Name] = replicaReadiness{raw: 4, committed: 2}
	}
	state := canaryRestorationState(old, target, old[0], target, inputs)
	step := ComputeNextStep(state)
	require.NotNil(t, step)
	require.NoError(t, validateUpdateStep(state, step))
	require.Equal(t, []int{3, 3}, step.Past, "the in-flight deletion cannot authorize another")
	require.Equal(t, []int{5, 5}, step.New, "stable growth may fill missing issued capacity")
	for _, name := range inputs.allRoleNames {
		old[0].Roles[name].Spec.Replicas = ptr.To[int32](5)
	}
	// The new stable groups are still loading; their requested replicas do not
	// increase committed readiness or permit further reverse progress.
	require.Nil(t, ComputeNextStep(canaryRestorationState(old, target, old[0], target, inputs)))
}

func TestCanaryRestoresAnchorAcrossInterruptedRollouts(t *testing.T) {
	for _, budget := range [][2]int{{1, 0}, {0, 1}} {
		for _, histories := range []int{2, 3} {
			t.Run(fmt.Sprintf("budgets%v/old%d", budget, histories), func(t *testing.T) {
				desired, stable, current := []int{3, 7}, []int{1, 2}, []int{2, 4}
				if histories == 3 {
					desired, stable, current = []int{4, 10}, []int{1, 3}, []int{2, 5}
				}
				ds := newDSWithRoles("test", staticRole("prefill"), staticRole("decode"))
				ds.UID = "uid"
				_, old, target := canaryReverseTestInputs(desired, stable, []int{1, 1}, current, budget[0], budget[1])
				if histories == 3 {
					extra := disaggutils.RevisionRoles{Revision: "extra", Roles: map[string]*leaderv1.LeaderWorkerSet{}}
					for i, name := range testRoleNames() {
						extra.Roles[name] = revisionLWS("extra", name, 1, 1, time.Unix(2, 0), int32(desired[i]))
					}
					old = append(old, extra)
				}
				objects := []client.Object{ds}
				for _, revision := range append(append(disaggutils.RevisionRolesList{}, old...), target) {
					for _, lws := range revision.Roles {
						objects = append(objects, lws)
					}
				}
				for i := range ds.Spec.Roles {
					ds.Spec.Roles[i].Spec.RolloutStrategy.RollingUpdateConfiguration = &leaderv1.RollingUpdateConfiguration{MaxSurge: intstr.FromInt(budget[0]), MaxUnavailable: intstr.FromInt(budget[1])}
				}
				c := newTestClient(objects...)
				for _, percent := range []int32{50, 75} {
					setCanaryPercent(ds, percent)
					executor := newTestExecutor(c)
					settled := false
					for range 100 {
						old, latest, err := executor.LWSManager.GetRevisionRolesList(t.Context(), ds, 0, "2")
						require.NoError(t, err)
						_, _, err = executor.reconcileExistingRollout(t.Context(), ds, old, *latest, map[string]int{"prefill": desired[0], "decode": desired[1]})
						require.NoError(t, err)
						list := &leaderv1.LeaderWorkerSetList{}
						require.NoError(t, c.List(t.Context(), list))
						ready := map[string]int{}
						settled = true
						for i := range list.Items {
							lws := &list.Items[i]
							ready[lws.Labels[disaggv1.RoleLabelKey]] += int(min(*lws.Spec.Replicas, lws.Status.ReadyReplicas))
							settled = settled && lws.Status.ReadyReplicas == *lws.Spec.Replicas
							lws.Status.Replicas, lws.Status.ReadyReplicas = *lws.Spec.Replicas, *lws.Spec.Replicas
							require.NoError(t, c.Status().Update(t.Context(), lws))
						}
						old, latest, err = executor.LWSManager.GetRevisionRolesList(t.Context(), ds, 0, "2")
						require.NoError(t, err)
						for i, name := range testRoleNames() {
							wantOld := canaryOldReplicas(desired[i], int(percent))
							require.GreaterOrEqual(t, ready[name], desired[i]-budget[1], "pending growth cannot fund a history deletion")
							settled = settled && old.GetTotalReplicasPerRole(name) == wantOld && int(*latest.Roles[name].Spec.Replicas) == desired[i]-wantOld
							for _, revision := range old {
								if revision.Revision == "0" {
									settled = settled && int(*revision.Roles[name].Spec.Replicas) == wantOld
								}
							}
						}
						if settled {
							break
						}
					}
					require.True(t, settled, "the stable anchor must keep its own share at percent=%d", percent)
				}
			})
		}
	}
}

func TestCanaryHoldRequiresServingHistory(t *testing.T) {
	// A complete stable revision does not make two other, complementary broken
	// revisions serve. Every issued group is Ready, but only3/8 can serve.
	state := rolloutState([]int{8, 8}, []int{1, 1}, []int{1, 1}, nil, nil, []int{2, 2}, []int{2, 2}, []int{8, 8}, canaryConfigs(1, 0, []int{6, 6}))
	for _, counts := range [][]int{{5, 0}, {0, 5}} {
		revision := newRevisionState(2)
		revision.RequiredRoles = []bool{true, true}
		revision.SpecReplicas = slicesClone(counts)
		revision.ReadyReplicas = slicesClone(counts)
		revision.RawReadyReplicas = slicesClone(counts)
		state.ParkedOld = append(state.ParkedOld, revision)
	}
	require.False(t, heldAtCanary(state), "Argo CD must not report a healthy hold using incomplete historical revisions")
}

func TestCanaryRejectsPlanBelowAnchorFloor(t *testing.T) {
	state := rolloutState([]int{4, 4}, []int{3, 3}, []int{3, 3}, nil, nil, []int{1, 1}, []int{1, 1}, []int{4, 4}, canaryConfigs(0, 4, []int{3, 3}))
	state.ProportionalCanary = true
	// Availability and coordination alone permit this deletion; the explicit
	// anchor reservation must independently reject it.
	step := &UpdateStep{Past: []int{2, 3}, New: []int{1, 1}, Kind: OrdinaryStep}
	require.ErrorContains(t, validateUpdateStep(state, step), "outside [3,3]")
}

func TestCanaryAnchorIdentityIsDurable(t *testing.T) {
	inputs, old, target := canaryReverseTestInputs([]int{8, 8}, []int{4, 4}, []int{2, 2}, []int{2, 2}, 1, 0)
	revisions := append(append(disaggutils.RevisionRolesList{}, old...), target)
	for _, revision := range revisions {
		for _, lws := range revision.Roles {
			lws.CreationTimestamp = old[0].Roles["prefill"].CreationTimestamp
		}
	}
	require.Empty(t, canaryStableRevision(revisions, inputs.allRoleNames, inputs.readiness).Revision, "same-second history must not be guessed by revision hash")
	old[0].Roles["prefill"].Annotations[canaryStableAnnotation] = "true"
	require.Equal(t, old[0].Revision, canaryStableRevision(revisions, inputs.allRoleNames, inputs.readiness).Revision, "a partially recorded marker still identifies the same anchor")
	old[1].Roles["decode"].Annotations[canaryStableAnnotation] = "true"
	require.Empty(t, canaryStableRevision(revisions, inputs.allRoleNames, inputs.readiness).Revision, "conflicting stable markers must block")
}

func TestCanaryGitRevertDoesNotPromoteAnotherAnchor(t *testing.T) {
	inputs, old, target := canaryReverseTestInputs([]int{8, 8}, []int{4, 4}, []int{2, 2}, []int{2, 2}, 1, 0)
	old[0].Roles["prefill"].Annotations[canaryStableAnnotation] = "true"
	ds := newDSWithRoles("test", staticRole("prefill"), staticRole("decode"))
	setCanaryPercent(ds, 50)
	// Git now points to A again, while B and C still have serving groups.
	rollback := buildRolloutInputs(ds, disaggutils.RevisionRolesList{old[1], target}, old[0], map[string]int{"prefill": 8, "decode": 8}, inputs.readiness)
	require.Equal(t, old[0].Revision, rollback.stableRevision)
	require.False(t, rollback.proportionalCanary, "rollback must fully restore A, not make B the stable half")
	for _, config := range rollback.config {
		require.Zero(t, config.CanaryOldReplicas)
		require.Zero(t, config.MinOldReplicas)
	}
}

func TestCanaryWholeSetZeroAndResume(t *testing.T) {
	for _, percent := range []int32{25, 50} {
		t.Run(fmt.Sprint(percent), func(t *testing.T) {
			ds := newDSWithRoles("test", staticRole("prefill"), staticRole("decode"))
			ds.UID = "uid"
			ds.Annotations = map[string]string{disaggv1.RevisionHashVersionAnnotationKey: disaggv1.RevisionHashVersion}
			setCanaryPercent(ds, percent)
			for i := range ds.Spec.Roles {
				ds.Spec.Roles[i].Spec.Replicas = ptr.To[int32](8)
			}
			revision := disaggutils.ComputeRevision(ds.Spec.Roles)
			objects := []client.Object{ds}
			for _, name := range testRoleNames() {
				stableCount := int32(canaryOldReplicas(8, int(percent)))
				stable := revisionLWS("stable", name, stableCount, stableCount, time.Unix(0, 0), 8)
				stable.Annotations[canaryStableAnnotation] = "true"
				objects = append(objects, stable, revisionLWS(revision, name, 8-stableCount, 8-stableCount, time.Unix(1, 0), 8))
			}
			c := newTestClient(objects...)
			for _, desired := range []int32{0, 8} {
				require.NoError(t, c.Get(t.Context(), client.ObjectKeyFromObject(ds), ds))
				for i := range ds.Spec.Roles {
					ds.Spec.Roles[i].Spec.Replicas = ptr.To(desired)
				}
				require.Equal(t, revision, disaggutils.ComputeRevision(ds.Spec.Roles), "scaling is not a new revision")
				require.NoError(t, c.Update(t.Context(), ds))
				reconciler := newTestReconciler(c) // Resume also works after a controller restart.
				settled := false
				for range 30 {
					_, err := reconciler.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(ds)})
					require.NoError(t, err)
					list := &leaderv1.LeaderWorkerSetList{}
					require.NoError(t, c.List(t.Context(), list))
					settled = true
					currentRoles, stableRoles := 0, 0
					for i := range list.Items {
						lws, want := &list.Items[i], int32(0)
						if lws.Labels[disaggv1.RevisionLabelKey] == revision {
							want, currentRoles = desired-int32(canaryOldReplicas(int(desired), int(percent))), currentRoles+1
							require.EqualValues(t, lws.Name, lws.UID, "zero keeps the current LWS for reuse")
						} else {
							want, stableRoles = int32(canaryOldReplicas(int(desired), int(percent))), stableRoles+1
							require.Equal(t, "true", lws.Annotations[canaryStableAnnotation])
							require.EqualValues(t, lws.Name, lws.UID, "zero keeps the baseline templates and identity")
						}
						settled = settled && *lws.Spec.Replicas == want && lws.Status.ReadyReplicas == want
						lws.Status.Replicas, lws.Status.ReadyReplicas = *lws.Spec.Replicas, *lws.Spec.Replicas
						require.NoError(t, c.Status().Update(t.Context(), lws))
					}
					settled = settled && currentRoles == 2 && stableRoles == 2
					if settled {
						break
					}
				}
				require.True(t, settled, "desired=%d with canaryPercent still%d", desired, percent)
				require.NoError(t, c.Get(t.Context(), client.ObjectKeyFromObject(ds), ds))
				require.Equal(t, percent, *ds.Spec.RolloutStrategy.CanaryPercent)
			}
		})
	}
}

func TestCanarySlicePromotionAndReversal(t *testing.T) {
	for _, mode := range []disaggv1.CanaryDimension{disaggv1.CanaryDimensionSlices, disaggv1.CanaryDimensionAuto} {
		for _, identity := range []leaderv1.GroupIdentityType{leaderv1.GroupIdentityOrdinal, leaderv1.GroupIdentityHash} {
			t.Run(fmt.Sprintf("%s/%s", mode, identity), func(t *testing.T) {
				ds := newTwoRoleTestDisaggregatedSet([2]int32{1, 1}, [2]int{1, 1}, [2]int{})
				ds.Spec.Slices = ptr.To[int32](8)
				ds.Annotations = map[string]string{disaggv1.RevisionHashVersionAnnotationKey: disaggv1.RevisionHashVersion}
				setCanaryPercent(ds, 25)
				ds.Spec.RolloutStrategy.CanaryDimension = mode
				objects := []client.Object{ds}
				for i := range ds.Spec.Roles {
					ds.Spec.Roles[i].Spec.GroupIdentity = identity
				}
				for slice := range 8 {
					for _, name := range testRoleNames() {
						lws := revisionLWS("stable", name, 1, 1, time.Unix(1, 0), 1)
						lws.Name = fmt.Sprintf("test-%d-stable-%s", slice, name)
						lws.UID = types.UID(lws.Name)
						lws.Spec.GroupIdentity = identity
						lws.Labels[disaggv1.SliceLabelKey] = strconv.Itoa(slice)
						objects = append(objects, lws)
					}
				}
				c := newTestClient(objects...)
				for _, phase := range []struct {
					percent, replicas int32
					version           string
				}{
					{25, 1, "B"}, {50, 1, "B"}, {25, 1, "B"},
					{50, 2, "B"}, // Auto remains slices even though groups can now be split.
					{50, 2, "C"}, {25, 2, "C"}, {0, 2, "C"}, {25, 2, "C"},
					{25, 0, "C"}, {25, 1, "C"}, {100, 1, "C"},
				} {
					require.NoError(t, c.Get(t.Context(), client.ObjectKeyFromObject(ds), ds))
					setCanaryPercent(ds, phase.percent)
					for i := range ds.Spec.Roles {
						ds.Spec.Roles[i].Spec.Replicas = ptr.To(phase.replicas)
						ds.Spec.Roles[i].Spec.LeaderWorkerTemplate.WorkerTemplate.Spec.Containers = []corev1.Container{{Name: "worker", Image: phase.version}}
					}
					require.NoError(t, c.Update(t.Context(), ds))
					revision := disaggutils.ComputeRevision(ds.Spec.Roles)
					r := newTestReconciler(c) // Every phase also tests controller restart.
					settled := false
					for range 100 {
						_, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(ds)})
						require.NoError(t, err)
						list := &leaderv1.LeaderWorkerSetList{}
						require.NoError(t, c.List(t.Context(), list))
						settled = true
						stableCount, targetCount := 0, 0
						for i := range list.Items {
							lws, want := &list.Items[i], int32(0)
							slice, err := strconv.Atoi(lws.Labels[disaggv1.SliceLabelKey])
							require.NoError(t, err)
							selected := phase.percent == 100 || slice < 8-canaryOldReplicas(8, int(phase.percent))
							switch lws.Labels[disaggv1.RevisionLabelKey] {
							case "stable":
								stableCount++
								if !selected {
									want = phase.replicas
								}
								require.EqualValues(t, lws.Name, lws.UID, "baseline identity survives at zero")
							case revision:
								targetCount++
								if selected {
									want = phase.replicas
								}
							}
							settled = settled && getLWSReplicas(lws) == want && lws.Status.ReadyReplicas == want
							lws.Status.Replicas, lws.Status.ReadyReplicas, lws.Status.UpdatedReplicas = getLWSReplicas(lws), getLWSReplicas(lws), getLWSReplicas(lws)
							require.NoError(t, c.Status().Update(t.Context(), lws))
						}
						wantStable := 16
						if phase.percent == 100 {
							wantStable = 0
						}
						settled = settled && stableCount == wantStable && targetCount == 16
						if settled {
							break
						}
					}
					require.True(t, settled, "phase %+v", phase)
					require.NoError(t, c.Get(t.Context(), client.ObjectKeyFromObject(ds), ds))
					if mode == disaggv1.CanaryDimensionAuto && phase.percent < 100 {
						require.Equal(t, "slices", ds.Annotations[canaryDimensionAnnotation])
					}
				}
			})
		}
	}
}

func TestCanaryAutoChoicePersists(t *testing.T) {
	for _, initial := range []int32{1, 4} {
		ds := newTwoRoleTestDisaggregatedSet([2]int32{initial, initial}, [2]int{1, 1}, [2]int{})
		ds.Spec.Slices = ptr.To[int32](8)
		setCanaryPercent(ds, 25)
		ds.Spec.RolloutStrategy.CanaryDimension = disaggv1.CanaryDimensionAuto
		c := newTestClient(ds)
		r := newTestReconciler(c)
		old := []*leaderv1.LeaderWorkerSet{revisionLWS("A", "prefill", initial, initial, time.Unix(1, 0))}
		changed, err := r.resolveCanaryDimension(t.Context(), ds, old, "B", map[string]int{"prefill": int(initial), "decode": int(initial)})
		require.NoError(t, err)
		require.True(t, changed)
		want := disaggv1.CanaryDimensionGroups
		if initial == 1 {
			want = disaggv1.CanaryDimensionSlices
		}
		for _, count := range []int{1, 4, 0} {
			require.NoError(t, c.Get(t.Context(), client.ObjectKeyFromObject(ds), ds))
			changed, err = newTestReconciler(c).resolveCanaryDimension(t.Context(), ds, old, "C", map[string]int{"prefill": count, "decode": count})
			require.NoError(t, err)
			require.False(t, changed)
			require.Equal(t, want, canaryDimension(ds))
		}
		ds.Spec.RolloutStrategy.CanaryPercent = nil
		changed, err = r.resolveCanaryDimension(t.Context(), ds, old, "C", nil)
		require.NoError(t, err)
		require.True(t, changed)
		require.Empty(t, ds.Annotations[canaryDimensionAnnotation])
	}
}

func canaryReverseTestInputs(desired, stable, previous, current RoleReplicaState, surge, unavailable int) (rolloutInputs, disaggutils.RevisionRolesList, disaggutils.RevisionRoles) {
	inputs := rolloutInputs{allRoleNames: testRoleNames(), targetReplicas: desired, readiness: rolloutReadiness{}}
	held := make(RoleReplicaState, len(desired))
	for i, replicas := range desired {
		held[i] = canaryOldReplicas(replicas, 25)
	}
	inputs.config = canaryConfigs(surge, unavailable, held)
	for i := range inputs.config {
		inputs.config[i].MinOldReplicas = 0 // The candidate-specific adapter sets the anchor floor.
	}
	revisions := make(disaggutils.RevisionRolesList, 3)
	for index, counts := range []RoleReplicaState{stable, previous, current} {
		revision := disaggutils.RevisionRoles{Revision: fmt.Sprint(index), Roles: map[string]*leaderv1.LeaderWorkerSet{}}
		for i, name := range inputs.allRoleNames {
			lws := revisionLWS(revision.Revision, name, int32(counts[i]), int32(counts[i]), time.Unix(int64(index), 0), int32(desired[i]))
			revision.Roles[name] = lws
			inputs.readiness[lws.Name] = replicaReadiness{raw: counts[i], committed: counts[i]}
		}
		revisions[index] = revision
	}
	return inputs, revisions[:2], revisions[2]
}

func TestCanaryRestorationReleasesUnschedulableBootstrapCapacity(t *testing.T) {
	for _, identity := range []leaderv1.GroupIdentityType{leaderv1.GroupIdentityOrdinal, leaderv1.GroupIdentityHash} {
		t.Run(string(identity), func(t *testing.T) {
			desired := [2]int32{1, 4}
			ds := newTwoRoleTestDisaggregatedSet(desired, [2]int{1, 1}, [2]int{})
			setCanaryPercent(ds, 0)
			objects := revisionLWSObjects("stable", [2]int32{1, 1}, [2]int32{0, 1}, desired, time.Unix(0, 0))
			objects = append(objects, revisionLWSObjects("previous", [2]int32{1, 3}, [2]int32{1, 3}, desired, time.Unix(1, 0))...)
			objects = append(objects, revisionLWSObjects("current", [2]int32{1, 1}, [2]int32{1, 1}, desired, time.Unix(2, 0))...)
			for _, object := range objects {
				object.(*leaderv1.LeaderWorkerSet).Spec.GroupIdentity = identity
			}
			pod := podWithSchedulingCondition(corev1.PodPending, corev1.ConditionFalse, corev1.PodReasonUnschedulable, metav1.Now())
			pod.Name, pod.Namespace = "restoring-prefill", testNamespace
			pod.Labels = map[string]string{leaderv1.SetNameLabelKey: "test-0-stable-prefill"}
			c := newTestClient(append(objects, pod)...)
			executor := newTestExecutor(c)
			recorder := executor.Record.(*events.FakeRecorder)
			// The bootstrap group already exceeds surge. Wait for scheduler evidence,
			// then release capacity once without cascading through healthy history.
			for _, persistent := range []bool{false, true, true} {
				if persistent {
					require.NoError(t, c.Get(t.Context(), client.ObjectKeyFromObject(pod), pod))
					pod.Status.Conditions[0].LastTransitionTime = metav1.NewTime(time.Now().Add(-unschedulablePodGracePeriod - time.Second))
					require.NoError(t, c.Status().Update(t.Context(), pod))
				}
				reconcileExistingForTest(t, executor, ds, "current")
				want := [2]int32{1, 1}
				if persistent {
					want = [2]int32{}
				}
				assertRevisionReplicas(t, c, "current", want)
				assertRevisionReplicas(t, c, "previous", [2]int32{1, 3})
				assertRevisionReplicas(t, c, "stable", [2]int32{1, 2})
			}
			emitted := ""
			for len(recorder.Events) > 0 {
				emitted += <-recorder.Events
			}
			require.Equal(t, 1, strings.Count(emitted, EventReasonAvailabilityFallback))
			require.Equal(t, 1, strings.Count(emitted, EventReasonBootstrapSurge))
		})
	}
}

func TestCanaryMixedResizeCanDrainWhileBaselineLoads(t *testing.T) {
	for _, identity := range []leaderv1.GroupIdentityType{leaderv1.GroupIdentityOrdinal, leaderv1.GroupIdentityHash} {
		t.Run(string(identity), func(t *testing.T) {
			// Demand changed in opposite directions. Stable Prefill is still
			// loading and short of its 15-group share; Decode can safely shrink.
			ds := newTwoRoleTestDisaggregatedSet([2]int32{20, 4}, [2]int{1, 1}, [2]int{})
			setCanaryPercent(ds, 25)
			objects := revisionLWSObjects("stable", [2]int32{14, 6}, [2]int32{6, 6}, [2]int32{8, 8}, time.Unix(0, 0))
			objects = append(objects, revisionLWSObjects("current", [2]int32{6, 1}, [2]int32{6, 1}, [2]int32{20, 4}, time.Unix(1, 0))...)
			for _, object := range objects {
				object.(*leaderv1.LeaderWorkerSet).Spec.GroupIdentity = identity
			}
			c := newTestClient(objects...)
			executor := newTestExecutor(c)
			reconcileExistingForTest(t, executor, ds, "current")
			assertRevisionReplicas(t, c, "stable", [2]int32{14, 3})
			assertRevisionReplicas(t, c, "current", [2]int32{6, 1})
		})
	}
}

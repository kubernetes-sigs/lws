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
	"errors"
	"fmt"
	"maps"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	disaggv1 "sigs.k8s.io/lws/api/disaggregatedset/v1"
	leaderv1 "sigs.k8s.io/lws/api/leaderworkerset/v1"
	"sigs.k8s.io/lws/pkg/replicagroups"
	disaggutils "sigs.k8s.io/lws/pkg/utils/disaggregatedset"
)

func (f *subRoleFixture) scale(target map[string]int) error {
	_, err := f.manager.syncSubRoles(f.t.Context(), f.ds, f.observe().LWS, subRoleUpdate{target: target})
	return err
}

func assertHashSubRoleCounts(t *testing.T, f *subRoleFixture, want map[string]int) {
	t.Helper()
	s := f.observe()
	require.Empty(t, s.LWS.Annotations[subRolePlanAnnotation])
	require.EqualValues(t, countSubRoles(want), getLWSReplicas(s.LWS))
	actual := maps.Clone(want)
	for name := range actual {
		actual[name] = 0
	}
	for _, group := range s.Groups {
		name, coherent := groupSubRole(group)
		require.True(t, coherent)
		actual[name]++
		require.NotEqual(t, "-1000", group.Leader.Annotations[corev1.PodDeletionCost], "completed survivors must lose the DS deletion preference")
	}
	require.Equal(t, want, actual)
}

func TestHashSubRoleCostsPrecedeScaleAndRepairActualSurvivors(t *testing.T) {
	for _, wrongVictim := range []bool{false, true} {
		t.Run(fmt.Sprint("wrong-native-victim=", wrongVictim), func(t *testing.T) {
			f := newSubRoleFixtureForIdentity(t, leaderv1.GroupIdentityHash, 0, "a", "a", "a", "b")
			before := f.observe()
			oldB := before.Groups[3].Leader.UID
			base := f.manager.client
			deletes, publications := 0, 0
			f.manager.client = interceptor.NewClient(base.(client.WithWatch), interceptor.Funcs{
				Delete: func(context.Context, client.WithWatch, client.Object, ...client.DeleteOption) error {
					deletes++
					return errors.New("DS must never delete a Hash group")
				},
				Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
					if lws, ok := obj.(*leaderv1.LeaderWorkerSet); ok && getLWSReplicas(lws) == 2 && publications == 0 {
						preferred := 0
						for _, group := range f.observe().Groups {
							if group.Leader.Annotations[corev1.PodDeletionCost] == "-1000" {
								preferred++
							}
						}
						require.Equal(t, 2, preferred, "costs must be observed before publishing the smaller target")
						publications++
					}
					return c.Patch(ctx, obj, patch, opts...)
				},
			})
			require.ErrorIs(t, f.scale(map[string]int{"a": 1, "b": 1}), errReplicaGroupsPending)
			for attempt := 0; attempt < 6 && getLWSReplicas(f.observe().LWS) != 2; attempt++ {
				f.sync()
			}
			require.Equal(t, 1, publications)
			accepted := f.observe()
			// Neither a restart nor repeated reconciliation may issue an extra
			// delete, clear costs early, or supersede the accepted pending target.
			f.manager = NewLeaderWorkerSetManager(f.manager.client)
			f.manager.apiReader = base
			for range 2 {
				f.sync()
			}
			require.ErrorIs(t, f.scale(map[string]int{"a": 4, "b": 1}), errReplicaGroupsPending)
			require.Equal(t, accepted.LWS.Annotations[subRolePlanAnnotation], f.observe().LWS.Annotations[subRolePlanAnnotation])
			require.Equal(t, accepted.Groups, f.observe().Groups)
			if wrongVictim {
				f.native(oldB)
			} else {
				f.native()
			}
			afterNative := f.observe()
			keptB, preferredSurvivor := false, false
			for _, group := range afterNative.Groups {
				keptB = keptB || group.Leader.UID == oldB
				preferredSurvivor = preferredSurvivor || group.Leader.Annotations[corev1.PodDeletionCost] == "-1000"
			}
			require.Equal(t, !wrongVictim, keptB)
			if wrongVictim {
				require.True(t, preferredSurvivor, "repair a survivor that had been preferred for deletion, never delete it again")
				require.NotEmpty(t, afterNative.LWS.Annotations[subRolePlanAnnotation])
			}
			for range 6 {
				f.sync()
				f.native()
			}
			assertHashSubRoleCounts(t, f, map[string]int{"a": 1, "b": 1})
			require.Zero(t, deletes, "only the native ReplicaSet simulation may delete Pods")
		})
	}
}

func TestHashSubRoleNativeRolloutStartup(t *testing.T) {
	for _, phase := range []string{"steady", "preparing", "published"} {
		t.Run(phase, func(t *testing.T) {
			f := newSubRoleFixtureForIdentity(t, leaderv1.GroupIdentityHash, 0, "a", "a", "b")
			if phase != "steady" {
				require.ErrorIs(t, f.scale(map[string]int{"a": 0, "b": 1}), errReplicaGroupsPending)
			}
			if phase == "published" {
				for attempt := 0; attempt < 6 && getLWSReplicas(f.observe().LWS) != 1; attempt++ {
					f.sync()
				}
				require.EqualValues(t, 1, getLWSReplicas(f.observe().LWS))
			}
			s := f.observe()
			pending := s.LWS.Annotations[subRolePlanAnnotation]
			// Deployment starts a new ReplicaSet while the old surplus is live.
			// Its unlabelled newborn needs routing before it can become Ready.
			rs := s.ReplicaSets[0].DeepCopy()
			rs.Name, rs.UID, rs.ResourceVersion = "new-rs", "new-rs", ""
			rs.Spec.Replicas, rs.Status.Replicas = ptr.To[int32](1), 1
			require.NoError(t, f.manager.client.Create(t.Context(), rs))
			f.createGroup(rs, 99, "")
			s.LeaderDeployment.Spec.Replicas = ptr.To(int32(getLWSReplicas(s.LWS)))
			require.NoError(t, f.manager.client.Update(t.Context(), s.LeaderDeployment))
			deletes := 0
			f.manager.client = interceptor.NewClient(f.manager.client.(client.WithWatch), interceptor.Funcs{Delete: func(context.Context, client.WithWatch, client.Object, ...client.DeleteOption) error {
				deletes++
				return errors.New("native controller owns deletions")
			}})
			f.sync()
			after := f.observe()
			require.False(t, subRoleNativeSettled(after))
			require.Equal(t, pending, after.LWS.Annotations[subRolePlanAnnotation])
			require.Equal(t, getLWSReplicas(s.LWS), getLWSReplicas(after.LWS), "startup repair cannot publish a physical scale")
			require.Len(t, after.Groups, 4)
			found := false
			for _, group := range after.Groups {
				if metav1.GetControllerOf(group.Leader).UID == rs.UID {
					found = true
					name, coherent := groupSubRole(group)
					require.True(t, coherent)
					if phase == "steady" {
						require.Contains(t, []string{"a", "b"}, name)
					} else {
						require.Equal(t, "b", name, "startup must use a positive desired child, not removed a")
					}
				}
			}
			require.True(t, found)
			require.Zero(t, deletes)
		})
	}
}

func TestHashSubRoleTransitionsAndStartup(t *testing.T) {
	for _, target := range []map[string]int{{"a": 1, "b": 1}, {"a": 1, "b": 2}, {"a": 2, "b": 2}, {"a": 0, "b": 0}} {
		t.Run(fmt.Sprint(target), func(t *testing.T) {
			f := newSubRoleFixtureForIdentity(t, leaderv1.GroupIdentityHash, 0, "a", "a", "b")
			require.ErrorIs(t, f.scale(target), errReplicaGroupsPending)
			for range 12 {
				f.sync()
				f.native()
			}
			assertHashSubRoleCounts(t, f, target)
		})
	}
	// All newborn labels are filled in one pass even while the groups load.
	f := newSubRoleFixtureForIdentity(t, leaderv1.GroupIdentityHash, 0, "a", "a", "a")
	for _, group := range f.observe().Groups {
		for _, pod := range group.Pods {
			delete(pod.Labels, disaggv1.SubRoleLabelKey)
			require.NoError(t, f.manager.client.Update(t.Context(), pod))
			pod.Status.Conditions = nil
			require.NoError(t, f.manager.client.Status().Update(t.Context(), pod))
		}
	}
	_, err := f.manager.syncSubRoles(t.Context(), f.ds, f.observe().LWS, subRoleUpdate{})
	require.ErrorIs(t, err, errReplicaGroupsPending)
	for _, group := range f.observe().Groups {
		name, coherent := groupSubRole(group)
		require.True(t, coherent)
		require.Equal(t, "a", name)
	}
}

func TestHashSubRolePendingPlanSurvivesWriteFailures(t *testing.T) {
	for _, failure := range []string{"cost patch", "Spec patch", "cost RV", "cost UID"} {
		t.Run(failure, func(t *testing.T) {
			f := newSubRoleFixtureForIdentity(t, leaderv1.GroupIdentityHash, 0, "a", "a", "a")
			require.ErrorIs(t, f.scale(map[string]int{"a": 2}), errReplicaGroupsPending)
			base, failed := f.manager.client, false
			f.manager.client = interceptor.NewClient(base.(client.WithWatch), interceptor.Funcs{Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
				_, pod := obj.(*corev1.Pod)
				lws, physical := obj.(*leaderv1.LeaderWorkerSet)
				if !failed && (pod && failure != "Spec patch" || physical && getLWSReplicas(lws) == 2 && failure == "Spec patch") {
					failed = true
					if failure == "cost patch" || failure == "Spec patch" {
						return errors.New("interrupted write")
					}
					current := &corev1.Pod{}
					require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(obj), current))
					if failure == "cost UID" {
						require.NoError(t, c.Delete(ctx, current))
						current.UID, current.ResourceVersion = "replacement", ""
						require.NoError(t, c.Create(ctx, current))
					} else {
						current.Annotations["concurrent"] = "keep"
						require.NoError(t, c.Update(ctx, current))
					}
				}
				return c.Patch(ctx, obj, patch, opts...)
			}})
			for attempt := 0; attempt < 6 && !failed; attempt++ {
				_, err := f.manager.syncSubRoles(t.Context(), f.ds, f.observe().LWS, subRoleUpdate{})
				if failed {
					require.Error(t, err)
				} else {
					require.ErrorIs(t, err, errReplicaGroupsPending)
				}
			}
			require.True(t, failed)
			require.EqualValues(t, 3, getLWSReplicas(f.observe().LWS), "failed guarded writes cannot publish a drain")
			require.NotEmpty(t, f.observe().LWS.Annotations[subRolePlanAnnotation])
			f.manager = NewLeaderWorkerSetManager(base)
			for range 12 {
				f.sync()
				f.native()
			}
			assertHashSubRoleCounts(t, f, map[string]int{"a": 2})
		})
	}
}

func TestCachedHashSubRolesRequireSettledNativeCounts(t *testing.T) {
	for _, change := range []string{"settled", "loading", "cost", "native cost", "pending", "native pending", "wrong distribution", "missing worker"} {
		t.Run(change, func(t *testing.T) {
			f := newSubRoleFixtureForIdentity(t, leaderv1.GroupIdentityHash, 0, "a", "b")
			s := f.observe()
			pod := s.Groups[0].Leader
			var object client.Object = pod
			switch change {
			case "loading":
				pod.Status.Conditions = nil
				require.NoError(t, f.manager.client.Status().Update(t.Context(), pod))
			case "cost":
				pod.Annotations[corev1.PodDeletionCost] = "-1000"
			case "native cost":
				pod.Annotations[corev1.PodDeletionCost] = "-100"
			case "pending":
				s.LWS.Annotations[subRolePlanAnnotation] = "pending"
				object = s.LWS
			case "native pending":
				s.ReplicaSets[0].Generation++
				object = s.ReplicaSets[0]
			case "wrong distribution":
				for _, member := range s.Groups[0].Pods {
					member.Labels[disaggv1.SubRoleLabelKey] = "b"
					require.NoError(t, f.manager.client.Update(t.Context(), member))
				}
			case "missing worker":
				require.NoError(t, f.manager.client.Delete(t.Context(), s.Groups[0].Pods[1]))
			}
			if change != "wrong distribution" {
				require.NoError(t, f.manager.client.Update(t.Context(), object))
			}
			s = f.observe()
			require.Equal(t, change == "settled" || change == "loading" || change == "native cost", cachedSubRolesMatchDesired(t.Context(), f.manager.client, s.LWS, map[string]int{"a": 1, "b": 1}))
			if change == "native cost" {
				f.sync()
				require.Equal(t, "-100", f.observe().Groups[0].Leader.Annotations[corev1.PodDeletionCost], "DS must preserve native scheduling preferences")
			}
		})
	}
}

func TestHashSubRoleDeletionPreferenceRestoresNativeGateCost(t *testing.T) {
	for _, gateRemains := range []bool{false, true} {
		t.Run(fmt.Sprint("gate-remains=", gateRemains), func(t *testing.T) {
			f := newSubRoleFixtureForIdentity(t, leaderv1.GroupIdentityHash, 0, "a")
			pod := f.observe().Groups[0].Leader
			pod.Spec.SchedulingGates = []corev1.PodSchedulingGate{{Name: leaderv1.GroupReplacementSchedulingGate}}
			pod.Annotations[corev1.PodDeletionCost] = "-100"
			require.NoError(t, f.manager.client.Update(t.Context(), pod))
			_, err := f.manager.preferSubRoleDeletion(t.Context(), f.lws, f.observe().Groups[0], true)
			require.NoError(t, err)
			pod = f.observe().Groups[0].Leader
			require.Equal(t, "-1000", pod.Annotations[corev1.PodDeletionCost])
			want := "-100"
			if !gateRemains {
				pod.Spec.SchedulingGates = nil
				want = ""
				require.NoError(t, f.manager.client.Update(t.Context(), pod))
			}
			_, err = f.manager.preferSubRoleDeletion(t.Context(), f.lws, f.observe().Groups[0], false)
			require.NoError(t, err)
			require.Equal(t, want, f.observe().Groups[0].Leader.Annotations[corev1.PodDeletionCost])
		})
	}
}

func TestHashSubRoleNativeObservationBarrier(t *testing.T) {
	for _, tc := range []struct {
		name    string
		change  func(*replicagroups.Snapshot)
		settled bool
	}{
		{"unready", func(s *replicagroups.Snapshot) { s.Groups[0].Ready = false }, true},
		{"LWS generation", func(s *replicagroups.Snapshot) { s.LWS.Generation++ }, false},
		{"Deployment generation", func(s *replicagroups.Snapshot) { s.LeaderDeployment.Generation++ }, false},
		{"ReplicaSet generation", func(s *replicagroups.Snapshot) { s.ReplicaSets[0].Generation++ }, false},
		{"stale native count", func(s *replicagroups.Snapshot) { s.ReplicaSets[0].Status.Replicas++ }, false},
		{"native target", func(s *replicagroups.Snapshot) { s.LeaderDeployment.Spec.Replicas = ptr.To[int32](3) }, false},
		{"native deleting", func(s *replicagroups.Snapshot) { s.LeaderDeployment.DeletionTimestamp = ptr.To(metav1.Now()) }, false},
		{"native absent", func(s *replicagroups.Snapshot) { s.LeaderDeployment = nil }, false},
		{"wrong ownership", func(s *replicagroups.Snapshot) { s.Groups[1].Leader.OwnerReferences[0].UID = "foreign" }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newSubRoleFixtureForIdentity(t, leaderv1.GroupIdentityHash, 0, "a", "b").observe()
			tc.change(s)
			require.Equal(t, tc.settled, subRoleNativeSettled(s))
		})
	}
}

func TestHashSubRolePlanValidationAndReadyReservation(t *testing.T) {
	lws := newSubRoleFixtureForIdentity(t, leaderv1.GroupIdentityHash, 0, "a", "b").lws
	for _, value := range []string{"{", "null", "{}", `{"counts":{"a":-1},"readyFloor":{}}`, `{"counts":{"a":1},"readyFloor":{"a":2}}`, `{"counts":{"a":1},"readyFloor":{"a":-1}}`} {
		lws.Annotations[subRolePlanAnnotation] = value
		_, err := readSubRolePlan(lws)
		require.Error(t, err, value)
	}
	s := newSubRoleFixtureForIdentity(t, leaderv1.GroupIdentityHash, 0, "a", "b").observe()
	s.Groups[1].Ready = false
	assignments := readySubRoleAssignments(s.Groups, map[string]int{"a": 1, "b": 1}, map[string]int{"b": 1})
	require.Equal(t, "b", assignments[s.Groups[0].Leader.UID])
	require.Equal(t, "a", assignments[s.Groups[1].Leader.UID])
}

func TestHashSubRoleLostReadyFloorDelaysPublication(t *testing.T) {
	f := newSubRoleFixtureForIdentity(t, leaderv1.GroupIdentityHash, 0, "a", "a", "a")
	require.ErrorIs(t, f.scale(map[string]int{"a": 2}), errReplicaGroupsPending)
	for _, group := range f.observe().Groups[:2] {
		group.Leader.Status.Conditions = nil
		require.NoError(t, f.manager.client.Status().Update(t.Context(), group.Leader))
	}
	for range 3 {
		f.sync()
		require.EqualValues(t, 3, getLWSReplicas(f.observe().LWS), "one Ready group cannot cover the accepted floor of two")
	}
	ready := f.observe().Groups[0].Leader
	ready.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
	require.NoError(t, f.manager.client.Status().Update(t.Context(), ready))
	for range 8 {
		f.sync()
		f.native()
	}
	assertHashSubRoleCounts(t, f, map[string]int{"a": 2})
}

func TestHashSubRoleAdapterMembershipAndRemoval(t *testing.T) {
	f := newSubRoleFixtureForIdentity(t, leaderv1.GroupIdentityHash, 0, "a", "a", "b")
	s := f.observe()
	spoof := s.Groups[0].Leader.DeepCopy()
	spoof.Name, spoof.UID, spoof.ResourceVersion = "spoof", "foreign", ""
	spoof.OwnerReferences[0].UID = "foreign-rs"
	require.NoError(t, f.manager.client.Create(t.Context(), spoof))
	worker := s.Groups[0].Pods[1]
	worker.Labels[disaggv1.SubRoleLabelKey] = "b"
	require.NoError(t, f.manager.client.Update(t.Context(), worker))
	s = f.observe()
	require.Len(t, s.Groups, 3, "foreign ownership cannot inflate logical readiness")
	observed := rolloutReadiness{}
	observeSubRoleReadiness(&subRoleState{snapshot: s, issued: map[string]int{"a": 2, "b": 1}, initial: map[string]int{"a": 2, "b": 1}}, observed)
	require.Equal(t, replicaReadiness{spec: 2, initial: 2, raw: 1, committed: 1}, observed.role(f.revision("old"), "model/a"))
	require.Equal(t, replicaReadiness{spec: 1, initial: 1, raw: 1, committed: 1}, observed.role(f.revision("old"), "model/b"))
	require.Equal(t, replicaReadiness{}, observed.role(f.revision("old"), "model"))
	f.sync() // Repair the partial group before accepting new membership.
	_, err := f.manager.syncSubRoles(t.Context(), f.ds, f.observe().LWS, subRoleUpdate{membership: map[string]int{"a": 2, "b": 1, "new": 1}})
	require.ErrorIs(t, err, errReplicaGroupsPending)
	counts, err := subRoleCounts(f.observe().LWS, subRoleReplicasAnnotation)
	require.NoError(t, err)
	require.Equal(t, map[string]int{"a": 2, "b": 1, "new": 0}, counts)
	for range 12 {
		_, err = f.manager.syncSubRoles(t.Context(), f.ds, f.observe().LWS, subRoleUpdate{membership: map[string]int{}})
		require.True(t, err == nil || errors.Is(err, errReplicaGroupsPending), "%v", err)
		f.native()
	}
	s = f.observe()
	require.EqualValues(t, 3, getLWSReplicas(s.LWS), "unpartitioning keeps physical capacity")
	require.Empty(t, s.LWS.Annotations[subRoleReplicasAnnotation])
	for _, group := range s.Groups {
		child, coherent := groupSubRole(group)
		require.True(t, coherent)
		require.Empty(t, child)
	}
}

func TestHashSubRoleCollectorEnrollsOnlyCurrentTarget(t *testing.T) {
	for _, ordinary := range []bool{false, true} {
		t.Run(fmt.Sprint("ordinary-old=", ordinary), func(t *testing.T) {
			children := []string{"a", "a"}
			if ordinary {
				children = []string{"", ""}
			}
			old := newSubRoleFixtureForIdentity(t, leaderv1.GroupIdentityHash, 0, children...)
			if ordinary {
				lws := old.observe().LWS
				delete(lws.Annotations, subRoleReplicasAnnotation)
				delete(lws.Annotations, disaggv1.InitialSubRoleReplicasAnnotationKey)
				require.NoError(t, old.manager.client.Update(t.Context(), lws))
			}
			target := old.withRevision("current", "a")
			target.ds.Spec.Roles = []disaggv1.DisaggregatedRoleSpec{{
				Name: "model", SubRoles: []disaggv1.DisaggregatedSubRoleSpec{{Name: "a"}, {Name: "new"}},
			}}
			target.ds.Spec.Roles[0].Spec.GroupIdentity = leaderv1.GroupIdentityHash
			desired := map[string]int{"model": 2, "model/a": 1, "model/new": 1}
			before := old.observe()
			var err error
			for range 8 {
				_, err = target.manager.collectRolloutObservations(t.Context(), target.ds,
					disaggutils.RevisionRolesList{old.revision("old")}, target.revision("current"), desired)
				if err == nil {
					break
				}
				require.ErrorIs(t, err, errReplicaGroupsPending)
			}
			require.NoError(t, err)
			after := old.observe()
			require.Equal(t, before.LWS.Annotations, after.LWS.Annotations, "old Hash layout and history stay frozen")
			require.Equal(t, before.Groups, after.Groups, "old Hash groups must not be enrolled or relabeled")
			counts, err := subRoleCounts(target.observe().LWS, subRoleReplicasAnnotation)
			require.NoError(t, err)
			require.Equal(t, map[string]int{"a": 1, "new": 0}, counts, "the current Hash target enrolls its new child before scaling")

			executor := &RollingUpdateExecutor{LWSManager: target.manager}
			require.NoError(t, executor.scaleRevision(t.Context(), target.ds, old.revision("old"),
				[]string{"model/a", "model/new", "model"}, RoleReplicaState{2, 0, 2}, scaleDown))
			require.Equal(t, before.LWS.Annotations, old.observe().LWS.Annotations, "old execution cannot add a foreign zero-count child")
			require.NoError(t, executor.scaleRevision(t.Context(), target.ds, target.revision("current"),
				[]string{"model/a", "model/new"}, RoleReplicaState{1, 1}, scaleUp), "accepted Hash growth does not block sibling roles")
			counts, err = subRoleCounts(target.observe().LWS, subRoleReplicasAnnotation)
			require.NoError(t, err)
			require.Equal(t, map[string]int{"a": 1, "new": 1}, counts)
			require.EqualValues(t, 2, getLWSReplicas(target.lws), "enrolled current children remain executable")
		})
	}
}

func TestHashSubRoleStaleCountsCannotUndoAcceptedDrain(t *testing.T) {
	for _, targets := range []RoleReplicaState{{4, 2}, {4, 3}, {3, 2}} {
		t.Run(fmt.Sprint(targets), func(t *testing.T) {
			f := newSubRoleFixtureForIdentity(t, leaderv1.GroupIdentityHash, 0, "a", "a", "a", "a", "a", "b")
			executor := &RollingUpdateExecutor{LWSManager: f.manager}
			original, names := f.revision("old"), []string{"model/a", "model/b"}
			require.NoError(t, executor.scaleRevision(t.Context(), f.ds, original, names, targets, scaleDown), "accepted Hash targets do not block sibling roles")
			for range 12 {
				f.sync()
				f.native()
			}
			counts, err := subRoleCounts(f.observe().LWS, subRoleReplicasAnnotation)
			require.NoError(t, err)
			require.Equal(t, map[string]int{"a": targets[0], "b": 1}, counts)
			require.ErrorIs(t, executor.scaleRevision(t.Context(), f.ds, original, names, targets, scaleUp), errReplicaGroupsPending)
			err = executor.scaleRevision(t.Context(), f.ds, f.revision("old"), names, targets, scaleUp)
			require.True(t, err == nil || errors.Is(err, errReplicaGroupsPending), "%v", err)
			counts, err = subRoleCounts(f.observe().LWS, subRoleReplicasAnnotation)
			require.NoError(t, err)
			require.Equal(t, map[string]int{"a": targets[0], "b": targets[1]}, counts)
		})
	}
}

func TestHashSubRoleAcceptedDrainPreservesReadyFloorWithoutWaitingForEverySurvivor(t *testing.T) {
	for _, initialReady := range []int{2, 0} {
		t.Run(fmt.Sprint(initialReady), func(t *testing.T) {
			f := newSubRoleFixtureForIdentity(t, leaderv1.GroupIdentityHash, 0, "a", "a", "a", "a")
			for _, group := range f.observe().Groups[initialReady:] {
				group.Leader.Status.Conditions = nil
				require.NoError(t, f.manager.client.Status().Update(t.Context(), group.Leader))
			}
			require.ErrorIs(t, f.scale(map[string]int{"a": 3}), errReplicaGroupsPending)
			plan, err := readSubRolePlan(f.observe().LWS)
			require.NoError(t, err)
			floor := map[string]int{}
			if initialReady > 0 {
				floor["a"] = initialReady - 1
			}
			require.Equal(t, floor, plan.ReadyFloor, "an explicit empty floor must survive serialized replay")
			f.manager = NewLeaderWorkerSetManager(f.manager.client)
			for range 12 {
				f.sync()
				f.native()
			}
			s := f.observe()
			require.Empty(t, s.LWS.Annotations[subRolePlanAnnotation])
			require.Len(t, s.Groups, 3)
			ready := 0
			for _, group := range s.Groups {
				if group.Ready {
					ready++
				}
			}
			require.GreaterOrEqual(t, ready, floor["a"])
			require.Less(t, ready, len(s.Groups), "safe progress must not require every retained group to recover")
		})
	}
}

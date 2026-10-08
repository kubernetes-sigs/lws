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
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	disaggv1 "sigs.k8s.io/lws/api/disaggregatedset/v1"
	leaderv1 "sigs.k8s.io/lws/api/leaderworkerset/v1"
	"sigs.k8s.io/lws/pkg/replicagroups"
)

func setSubRolePlanForTest(t *testing.T, lws *leaderv1.LeaderWorkerSet, plan *subRolePlan) {
	t.Helper()
	data, err := json.Marshal(plan)
	require.NoError(t, err)
	if lws.Annotations == nil {
		lws.Annotations = map[string]string{}
	}
	lws.Annotations[subRolePlanAnnotation] = string(data)
}

func (f *subRoleFixture) scale(target map[string]int) error {
	_, err := f.manager.syncSubRoles(f.t.Context(), f.ds, f.observe().LWS, subRoleUpdate{target: target})
	return err
}

func TestCachedHashSubRolesRequireSettledProtection(t *testing.T) {
	for _, change := range []string{"settled", "loading", "unprotected", "wrong UID", "victim", "health deletion", "pending", "legacy pending", "armed", "native pending", "wrong distribution", "missing worker"} {
		t.Run(change, func(t *testing.T) {
			f := newSubRoleFixtureForIdentity(t, leaderv1.GroupIdentityHash, 0, "a", "b")
			s := f.observe()
			pod := s.Groups[0].Leader
			var object client.Object = pod
			switch change {
			case "loading":
				pod.Status.Conditions = nil
				require.NoError(t, f.manager.client.Status().Update(t.Context(), pod))
			case "unprotected":
				delete(pod.Annotations, leaderv1.GroupScaleProtectionAnnotationKey)
			case "wrong UID":
				pod.Annotations[leaderv1.GroupScaleProtectionAnnotationKey] = string(s.LWS.UID) + "/previous-pod"
			case "victim":
				pod.Annotations[leaderv1.GroupScaleVictimAnnotationKey] = "stale-plan/" + string(pod.UID)
			case "health deletion":
				pod.Annotations[leaderv1.GroupReplacementDeleteAnnotationKey] = string(pod.UID)
			case "pending", "legacy pending", "armed":
				key := map[string]string{"pending": subRolePlanAnnotation, "legacy pending": legacySubRolePlanAnnotation, "armed": leaderv1.GroupScalePlanAnnotationKey}[change]
				s.LWS.Annotations[key] = "pending"
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
			require.Equal(t, change == "settled" || change == "loading", cachedSubRolesMatchDesired(t.Context(), f.manager.client, s.LWS, map[string]int{"a": 1, "b": 1}))
		})
	}
}

func TestHashSubRoleMetadataBatchesBeforePlanning(t *testing.T) {
	for _, change := range []string{"newborn", "partial", "protection"} {
		t.Run(change, func(t *testing.T) {
			f := newSubRoleFixtureForIdentity(t, leaderv1.GroupIdentityHash, 0, "a", "a", "a")
			for _, group := range f.observe().Groups {
				for _, pod := range group.Pods {
					if change == "newborn" || change == "partial" && pod != group.Leader {
						delete(pod.Labels, disaggv1.SubRoleLabelKey)
					}
					if change != "partial" {
						delete(pod.Annotations, leaderv1.GroupScaleProtectionAnnotationKey)
					}
					require.NoError(t, f.manager.client.Update(t.Context(), pod))
				}
			}
			reads := 0
			f.manager.apiReader = interceptor.NewClient(f.manager.client.(client.WithWatch), interceptor.Funcs{Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if _, ok := obj.(*leaderv1.LeaderWorkerSet); ok {
					reads++
				}
				return c.Get(ctx, key, obj, opts...)
			}})
			_, err := f.manager.syncSubRoles(t.Context(), f.ds, f.lws, subRoleUpdate{})
			require.ErrorIs(t, err, errReplicaGroupsPending)
			require.LessOrEqual(t, reads, 2+2*3, "at most two parent checks per changed group")
			for _, group := range f.observe().Groups {
				name, coherent := groupSubRole(group)
				require.True(t, coherent)
				require.Equal(t, "a", name)
				require.True(t, subRoleProtected(f.lws, group), "protection-only changes must not be skipped")
			}
		})
	}
	// Accept the target first, batch every victim marker, then arm the drain only
	// after observing those markers. Accepted writes remain visible to the executor.
	f := newSubRoleFixtureForIdentity(t, leaderv1.GroupIdentityHash, 0, "a", "a", "a", "b")
	state, err := f.manager.syncSubRoles(t.Context(), f.ds, f.lws, subRoleUpdate{target: map[string]int{"a": 1, "b": 1}})
	require.ErrorIs(t, err, errReplicaGroupsPending)
	require.True(t, state.written)
	require.EqualValues(t, 4, getLWSReplicas(state.lws))
	f.sync()
	s := f.observe()
	plan, err := readSubRolePlan(s.LWS)
	require.NoError(t, err)
	require.False(t, plan.Armed)
	victims := 0
	for _, group := range s.Groups {
		if subRoleVictim(group, plan) {
			victims++
		}
	}
	require.Equal(t, 2, victims)
	require.EqualValues(t, 4, getLWSReplicas(s.LWS))
	f.sync()
	require.EqualValues(t, 2, getLWSReplicas(f.observe().LWS))
}

func TestSubRoleIDLessOrdinalPlanUpgrade(t *testing.T) {
	for _, armed := range []bool{false, true} {
		t.Run(fmt.Sprint("armed=", armed), func(t *testing.T) {
			f := newSubRoleFixture(t, 0, "a", "b", "a")
			// Persist the earlier ID-less object format. The KISS implementation
			// keeps its selected prefix and never introduces Hash authority.
			want := map[string]int{"a": 1, "b": 1}
			lws := f.observe().LWS
			lws.Annotations["disaggregatedset.x-k8s.io/subrole-scale-plan"] = `{"counts":{"a":1,"b":1},"readyFloor":{"a":1,"b":1}}`
			if armed {
				lws.Annotations["disaggregatedset.x-k8s.io/subrole-scale-plan"] = `{"armed":true,"counts":{"a":1,"b":1},"readyFloor":{"a":1,"b":1}}`
				lws.Spec.Replicas = ptr.To[int32](2)
				setSubRoleJSON(lws, subRoleReplicasAnnotation, want)
			}
			require.NoError(t, f.manager.client.Update(t.Context(), lws))
			if armed {
				leader := f.observe().Groups[0].Leader
				leader.Status.Conditions = nil // Accepted drains do not acquire a new readiness gate.
				require.NoError(t, f.manager.client.Status().Update(t.Context(), leader))
			}
			before := f.observe()
			f.manager = NewLeaderWorkerSetManager(f.manager.client) // Restart with the upgraded implementation.
			f.sync()
			after := f.observe()
			require.EqualValues(t, 2, getLWSReplicas(after.LWS))
			require.JSONEq(t, `{"a":1,"b":1}`, after.LWS.Annotations[subRoleReplicasAnnotation])
			require.NotContains(t, after.LWS.Annotations[subRolePlanAnnotation], `"id"`)
			require.Empty(t, after.LWS.Annotations[leaderv1.GroupScalePlanAnnotationKey])
			require.Equal(t, before.Groups[:2], after.Groups[:2], "upgrade does not rewrite retained labels, fences, UIDs or resource versions")
			require.Equal(t, before.Groups[2].Leader.UID, after.Groups[2].Leader.UID)
			child, coherent := groupSubRole(after.Groups[2])
			require.True(t, coherent)
			require.Empty(t, child, "KISS clears outgoing routing before native deletion")
			require.Equal(t, before.LWS.OwnerReferences, after.LWS.OwnerReferences)
			f.native()
			f.sync()
			after = f.observe()
			require.Empty(t, after.LWS.Annotations[subRolePlanAnnotation])
			require.Empty(t, after.LWS.Annotations["disaggregatedset.x-k8s.io/subrole-scale-plan"])
			require.Len(t, after.Groups, 2)
			require.Equal(t, before.Groups[:2], after.Groups, "native Ordinal deletion retains the same prefix")
		})
	}
	// The same wire format is insufficient for Hash: DELETE authority needs a
	// unique plan ID both before and after arming.
	for _, armed := range []bool{false, true} {
		lws := &leaderv1.LeaderWorkerSet{Spec: leaderv1.LeaderWorkerSetSpec{GroupIdentity: leaderv1.GroupIdentityHash}}
		setSubRolePlanForTest(t, lws, &subRolePlan{Armed: armed, Counts: map[string]int{"a": 1}, ReadyFloor: map[string]int{"a": 1}})
		_, err := readSubRolePlan(lws)
		require.Error(t, err, "Hash object plans require a unique delete-authorization ID")
	}
}

func TestSubRoleLegacyOrdinalFloorUsesOnlyActualRetainedPrefix(t *testing.T) {
	for _, tc := range []struct {
		name        string
		legacy      bool
		prefixReady int
		wantSpec    int32
	}{
		{"legacy floor permits an unready survivor", true, 1, 3},
		{"Ready high suffix cannot supply the legacy floor", true, 0, 4},
		{"current count map still requires a healthy prefix", false, 1, 4},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSubRoleFixture(t, 7, "a", "a", "a", "a")
			for _, group := range f.observe().Groups[tc.prefixReady:3] {
				group.Leader.Status.Conditions = nil
				require.NoError(t, f.manager.client.Status().Update(t.Context(), group.Leader))
			}
			lws := f.observe().LWS
			key := subRolePlanAnnotation
			if tc.legacy {
				key = "disaggregatedset.x-k8s.io/subrole-scale-plan"
				lws.Annotations[key] = `{"counts":{"a":3},"readyFloor":{"a":1}}`
			} else {
				setSubRoleJSON(lws, key, map[string]int{"a": 3})
			}
			require.NoError(t, f.manager.client.Update(t.Context(), lws))
			before := f.observe()
			for range 6 {
				f.sync()
				f.native()
			}
			after := f.observe()
			require.Equal(t, tc.wantSpec, getLWSReplicas(after.LWS))
			require.Equal(t, before.Groups[:tc.wantSpec], after.Groups, "survivors must be the actual ordinal prefix")
			if tc.wantSpec == 3 {
				require.Empty(t, after.LWS.Annotations[key])
			} else {
				require.NotEmpty(t, after.LWS.Annotations[key])
			}
		})
	}
}

func TestSubRoleLegacyHashPlanUpgradeKeepsAcceptedVictim(t *testing.T) {
	f := newSubRoleFixtureForIdentity(t, leaderv1.GroupIdentityHash, 0, "a", "a", "b")
	s := f.observe()
	s.LWS.Annotations["disaggregatedset.x-k8s.io/subrole-scale-plan"] = `{"id":"legacy","armed":true,"counts":{"a":1,"b":1},"readyFloor":{"a":1,"b":1}}`
	s.LWS.Annotations[leaderv1.GroupScalePlanAnnotationKey] = "legacy"
	s.LWS.Spec.Replicas = ptr.To[int32](2)
	setSubRoleJSON(s.LWS, subRoleReplicasAnnotation, map[string]int{"a": 1, "b": 1})
	require.NoError(t, f.manager.client.Update(t.Context(), s.LWS))
	victim := s.Groups[1].Leader
	victim.Annotations[leaderv1.GroupScaleVictimAnnotationKey] = "legacy/" + string(victim.UID)
	require.NoError(t, f.manager.client.Update(t.Context(), victim))
	s.Groups[0].Leader.Status.Conditions = nil
	require.NoError(t, f.manager.client.Status().Update(t.Context(), s.Groups[0].Leader))
	before := f.observe()
	f.manager = NewLeaderWorkerSetManager(f.manager.client)
	for range 5 {
		f.native()
		f.sync()
	}
	after := f.observe()
	require.Empty(t, after.LWS.Annotations[subRolePlanAnnotation])
	require.Empty(t, after.LWS.Annotations["disaggregatedset.x-k8s.io/subrole-scale-plan"])
	require.Empty(t, after.LWS.Annotations[leaderv1.GroupScalePlanAnnotationKey])
	require.Equal(t, []replicagroups.Group{before.Groups[0], before.Groups[2]}, after.Groups,
		"legacy Hash plans keep their exact survivors without rechecking the accepted Ready floor")
	require.True(t, apierrors.IsNotFound(f.manager.client.Get(t.Context(), client.ObjectKeyFromObject(victim), &corev1.Pod{})))
}

func TestSubRolePlanAnnotationsDoNotGrowWithGroupCount(t *testing.T) {
	const replicas = 7001
	child := strings.Repeat("a", 63)
	s := &replicagroups.Snapshot{LWS: &leaderv1.LeaderWorkerSet{Spec: leaderv1.LeaderWorkerSetSpec{GroupIdentity: leaderv1.GroupIdentityHash}}}
	setSubRoleJSON(s.LWS, subRoleReplicasAnnotation, map[string]int{child: replicas})
	for i := range replicas {
		leader := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
			UID:    types.UID(fmt.Sprintf("%08d-0000-0000-0000-000000000000", i)),
			Labels: map[string]string{disaggv1.SubRoleLabelKey: child},
		}}
		s.Groups = append(s.Groups, replicagroups.Group{Leader: leader, Pods: []*corev1.Pod{leader}, Ready: true})
	}
	for _, target := range []int{replicas - 1, 0} {
		t.Run(fmt.Sprint(target), func(t *testing.T) {
			plan := &subRolePlan{ID: "plan", Armed: true, Counts: map[string]int{child: target}}
			setSubRolePlanForTest(t, s.LWS, plan)
			s.LWS.Annotations[leaderv1.GroupScalePlanAnnotationKey] = plan.ID
			bytes := 0
			for key, value := range s.LWS.Annotations {
				bytes += len(key) + len(value)
			}
			// Kubernetes caps the aggregate at 256 KiB, not each annotation.
			// Both nearly-all-survivor and all-victim old plans exceeded it.
			require.Less(t, bytes, 1024, "parent intent must contain child counts, not per-group UIDs")
			reloaded, err := readSubRolePlan(s.LWS)
			require.NoError(t, err)
			require.Equal(t, plan, reloaded)
		})
	}
}

func TestSubRoleArmedVictimRetryDoesNotWaitForHealthReplacement(t *testing.T) {
	f := newSubRoleFixtureForIdentity(t, leaderv1.GroupIdentityHash, 0, "a", "a", "a", "b", "b")
	ctx := t.Context()
	want := map[string]int{"a": 2, "b": 1}
	require.ErrorIs(t, f.scale(want), errReplicaGroupsPending)
	for range 4 {
		f.sync()
	}
	s := f.observe()
	plan, err := readSubRolePlan(s.LWS)
	require.NoError(t, err)
	require.True(t, plan.Armed)
	var victims []replicagroups.Group
	var lost *corev1.Pod
	for _, group := range s.Groups {
		if subRoleVictim(group, plan) {
			victims = append(victims, group)
		} else if lost == nil {
			lost = group.Leader.DeepCopy()
		}
	}
	require.Len(t, victims, 2)
	base := f.manager.client
	require.NoError(t, base.Delete(ctx, victims[0].Leader))
	require.NoError(t, base.Delete(ctx, lost)) // Independent native health deletion.
	f.native()
	s = f.observe()
	require.Len(t, s.Groups, 3, "native count already equals the new Spec; no replacement will be created yet")
	require.Len(t, subRolePlanAssignments(s, plan), 2)
	failed := false
	failure := errors.New("transient victim DELETE failure")
	f.manager.client = interceptor.NewClient(base.(client.WithWatch), interceptor.Funcs{
		Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			if obj.GetUID() == victims[1].Leader.UID && !failed {
				failed = true
				return failure
			}
			return c.Delete(ctx, obj, opts...)
		},
	})
	_, err = f.manager.syncSubRoles(ctx, f.ds, f.observe().LWS, subRoleUpdate{})
	require.ErrorIs(t, err, failure, "armed deletion must run even with fewer than target survivors")
	remaining := &corev1.Pod{}
	require.NoError(t, base.Get(ctx, client.ObjectKeyFromObject(victims[1].Leader), remaining))
	require.Equal(t, plan.ID+"/"+string(remaining.UID), remaining.Annotations[leaderv1.GroupScaleVictimAnnotationKey])
	// A new larger request must retry the accepted victim, never promote it.
	require.ErrorIs(t, f.scale(map[string]int{"a": 4, "b": 1}), errReplicaGroupsPending)
	require.True(t, apierrors.IsNotFound(base.Get(ctx, client.ObjectKeyFromObject(remaining), remaining)))
	replayed, err := readSubRolePlan(f.observe().LWS)
	require.NoError(t, err)
	require.Equal(t, plan.ID, replayed.ID)
	require.Equal(t, want, replayed.Counts)
	// Now a native replacement can fill the vacancy. The old worker-set UID
	// intentionally does not match, so this new group is not Ready yet.
	lost.ResourceVersion = ""
	lost.UID = "replacement-uid"
	require.NoError(t, base.Create(ctx, lost))
	f.native()
	for range 3 {
		f.sync()
	}
	require.Empty(t, f.observe().LWS.Annotations[subRolePlanAnnotation])
	require.NoError(t, base.Get(ctx, client.ObjectKeyFromObject(lost), lost))
	require.Equal(t, string(f.lws.UID)+"/"+string(lost.UID), lost.Annotations[leaderv1.GroupScaleProtectionAnnotationKey])
}

func TestSubRoleArmedAdoptionOnlyFillsVacancies(t *testing.T) {
	for _, tc := range []struct {
		name             string
		vacancy, victim  bool
		copied, expected bool
	}{
		{name: "native excess cannot displace a protected survivor"},
		{name: "new replacement fills missing child slot", vacancy: true, expected: true},
		{name: "an authorized victim cannot become the replacement", vacancy: true, victim: true},
		{name: "copied old victim UID does not taint replacement", vacancy: true, copied: true, expected: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSubRoleFixtureForIdentity(t, leaderv1.GroupIdentityHash, 0, "a", "b")
			s := f.observe()
			plan := &subRolePlan{ID: "plan", Armed: true, Counts: map[string]int{"a": 1, "b": 1}}
			fresh := s.Groups[0]
			fresh.Leader = fresh.Leader.DeepCopy()
			fresh.Leader.UID = "fresh"
			fresh.Leader.Annotations = map[string]string{}
			if tc.victim {
				fresh.Leader.Annotations[leaderv1.GroupScaleVictimAnnotationKey] = "plan/fresh"
			} else if tc.copied {
				fresh.Leader.Annotations[leaderv1.GroupScaleVictimAnnotationKey] = "plan/previous-uid"
			}
			if tc.vacancy {
				s.Groups = s.Groups[1:]
			}
			// Put fresh first: list order must not let it steal an occupied slot.
			s.Groups = append([]replicagroups.Group{fresh}, s.Groups...)
			assignments := subRolePlanAssignments(s, plan)
			_, adopted := assignments[fresh.Leader.UID]
			require.Equal(t, tc.expected, adopted)
			require.Equal(t, "b", assignments[s.Groups[len(s.Groups)-1].Leader.UID])
			if !tc.vacancy {
				require.Equal(t, "a", assignments[s.Groups[1].Leader.UID])
			}
		})
	}
}

func TestSubRoleOrdinarySyncDeletesOnlyUnassignedNativeExcess(t *testing.T) {
	f := newSubRoleFixtureForIdentity(t, leaderv1.GroupIdentityHash, 0, "a", "b")
	s := f.observe()
	fresh := s.Groups[0].Leader.DeepCopy()
	fresh.Name, fresh.UID, fresh.ResourceVersion = "first-extra", "extra-uid", ""
	delete(fresh.Labels, disaggv1.SubRoleLabelKey)
	delete(fresh.Annotations, leaderv1.GroupScaleProtectionAnnotationKey)
	require.NoError(t, f.manager.client.Create(t.Context(), fresh))
	require.Equal(t, fresh.UID, f.observe().Groups[0].Leader.UID)
	f.sync()
	require.True(t, apierrors.IsNotFound(f.manager.client.Get(t.Context(), client.ObjectKeyFromObject(fresh), fresh)),
		"the extra must be deleted, not protected in an already occupied child slot")
	for _, group := range s.Groups {
		current := &corev1.Pod{}
		require.NoError(t, f.manager.client.Get(t.Context(), client.ObjectKeyFromObject(group.Leader), current))
		require.Equal(t, group.Leader.Labels, current.Labels)
		require.Equal(t, group.Leader.Annotations, current.Annotations)
	}
}

func TestSubRoleSurplusDeletePreservesNativeAndUIDGuards(t *testing.T) {
	for _, gate := range []string{"ready", "native target pending", "missing assignments", "protected", "labelled", "ordinal"} {
		t.Run(gate, func(t *testing.T) {
			f := newSubRoleFixtureForIdentity(t, leaderv1.GroupIdentityHash, 0, "a", "b")
			s := f.observe()
			extra := s.Groups[0].Leader.DeepCopy()
			extra.Name, extra.UID, extra.ResourceVersion = "extra", "extra-uid", ""
			delete(extra.Labels, disaggv1.SubRoleLabelKey)
			delete(extra.Annotations, leaderv1.GroupScaleProtectionAnnotationKey)
			require.NoError(t, f.manager.client.Create(t.Context(), extra))
			s = f.observe()
			assignments := map[types.UID]string{}
			for _, group := range s.Groups {
				if child := group.Leader.Labels[disaggv1.SubRoleLabelKey]; child != "" {
					assignments[group.Leader.UID] = child
				}
			}
			switch gate {
			case "native target pending":
				s.LeaderDeployment.Generation++
			case "missing assignments":
				for uid := range assignments {
					delete(assignments, uid)
					break
				}
			case "protected", "labelled":
				for _, group := range s.Groups {
					if group.Leader.UID == extra.UID {
						if gate == "protected" {
							group.Leader.Annotations[leaderv1.GroupScaleProtectionAnnotationKey] = string(s.LWS.UID) + "/" + string(extra.UID)
						} else {
							group.Leader.Labels[disaggv1.SubRoleLabelKey] = "another-child"
						}
					}
				}
			case "ordinal":
				s.LWS.Spec.GroupIdentity = leaderv1.GroupIdentityOrdinal
			}
			deletes := 0
			f.manager.client = interceptor.NewClient(f.manager.client.(client.WithWatch), interceptor.Funcs{
				Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
					deletes++
					require.Equal(t, extra.UID, obj.GetUID(), "a selected survivor must never enter this delete path")
					preconditions := (&client.DeleteOptions{}).ApplyOptions(opts).Preconditions
					require.NotNil(t, preconditions)
					require.Equal(t, extra.UID, *preconditions.UID)
					require.Equal(t, extra.ResourceVersion, *preconditions.ResourceVersion)
					return c.Delete(ctx, obj, opts...)
				},
			})
			changed, err := f.manager.deleteSubRoleExtras(t.Context(), s, assignments)
			require.NoError(t, err)
			require.Equal(t, gate == "ready", changed)
			require.Equal(t, gate == "ready", deletes == 1)
		})
	}
}

func TestHashSubRoleUnarmedHealthLossReselectsVictim(t *testing.T) {
	f := newSubRoleFixtureForIdentity(t, leaderv1.GroupIdentityHash, 0, "a", "a", "a", "a", "a")
	require.ErrorIs(t, f.scale(map[string]int{"a": 4}), errReplicaGroupsPending)
	f.sync() // Propose a victim without authorizing deletion.
	s := f.observe()
	plan, err := readSubRolePlan(s.LWS)
	require.NoError(t, err)
	require.False(t, plan.Armed)
	require.True(t, subRoleVictim(s.Groups[4], plan))
	replacement := s.Groups[0].Leader.DeepCopy()
	require.NoError(t, f.manager.client.Delete(t.Context(), replacement))
	replacement.ResourceVersion, replacement.UID = "", "replacement-unready"
	replacement.Status.Conditions = nil
	require.NoError(t, f.manager.client.Create(t.Context(), replacement))
	f.native()
	f.sync()
	s = f.observe()
	plan, err = readSubRolePlan(s.LWS)
	require.NoError(t, err)
	require.False(t, plan.Armed)
	require.Empty(t, s.LWS.Annotations[leaderv1.GroupScalePlanAnnotationKey])
	require.True(t, subRoleVictim(s.Groups[0], plan), "the unready replacement becomes the new proposal")
	for range 8 {
		f.sync()
		f.native()
	}
	s = f.observe()
	require.Empty(t, s.LWS.Annotations[subRolePlanAnnotation])
	require.Len(t, s.Groups, 4)
	for _, group := range s.Groups {
		require.True(t, group.Ready)
		require.NotEqual(t, replacement.UID, group.Leader.UID)
	}
}

func TestHashSubRoleGroupPatchResumesAtLeaderCommit(t *testing.T) {
	for _, failedIndex := range []int{1, 2, 0} {
		t.Run(fmt.Sprint(failedIndex), func(t *testing.T) {
			f := newSubRoleFixtureForIdentity(t, leaderv1.GroupIdentityHash, 0, "a")
			group := f.observe().Groups[0]
			protection := group.Leader.Annotations[leaderv1.GroupScaleProtectionAnnotationKey]
			base := f.manager.client
			failure := errors.New("interrupted Pod patch")
			f.manager.client = interceptor.NewClient(base.(client.WithWatch), interceptor.Funcs{
				Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
					if obj.GetName() == group.Pods[failedIndex].Name {
						return failure
					}
					return c.Patch(ctx, obj, patch, opts...)
				},
			})
			require.ErrorIs(t, f.manager.labelSubRoleGroup(t.Context(), f.lws, group, "b"), failure)
			group = f.observe().Groups[0]
			require.Equal(t, "a", group.Leader.Labels[disaggv1.SubRoleLabelKey], "the leader is the assignment commit point")
			require.Equal(t, protection, group.Leader.Annotations[leaderv1.GroupScaleProtectionAnnotationKey])
			_, coherent := groupSubRole(group)
			require.Equal(t, failedIndex == 1, coherent)
			f.manager = NewLeaderWorkerSetManager(base) // Retry after a process restart.
			require.NoError(t, f.manager.labelSubRoleGroup(t.Context(), f.lws, group, "b"))
			group = f.observe().Groups[0]
			for _, pod := range group.Pods {
				require.Equal(t, "b", pod.Labels[disaggv1.SubRoleLabelKey])
			}
			require.Equal(t, protection, group.Leader.Annotations[leaderv1.GroupScaleProtectionAnnotationKey], "relabeling never rotates the UID fence")
			require.Equal(t, "keep", group.Leader.Annotations["user"])
			require.NoError(t, f.manager.labelSubRoleGroup(t.Context(), f.lws, group, ""))
			group = f.observe().Groups[0]
			for _, pod := range group.Pods {
				require.Empty(t, pod.Labels[disaggv1.SubRoleLabelKey])
			}
			require.Empty(t, group.Leader.Annotations[leaderv1.GroupScaleProtectionAnnotationKey])
		})
	}
}

func TestHashSubRoleNativeObservationBarrier(t *testing.T) {
	for _, tc := range []struct {
		name                    string
		change                  func(*replicagroups.Snapshot)
		beforeDeletes, complete bool
	}{
		{"unready groups do not block acknowledgement", func(s *replicagroups.Snapshot) { s.Groups[0].Ready = false }, true, true},
		{"LWS generation pending", func(s *replicagroups.Snapshot) { s.LWS.Generation++ }, false, false},
		{"Deployment generation pending", func(s *replicagroups.Snapshot) { s.LeaderDeployment.Generation++ }, false, false},
		{"ReplicaSet generation pending", func(s *replicagroups.Snapshot) { s.ReplicaSets[0].Generation++ }, true, false},
		{"native cache retains a deleted Pod", func(s *replicagroups.Snapshot) { s.ReplicaSets[0].Status.Replicas++ }, true, false},
		{"native target pending", func(s *replicagroups.Snapshot) { s.LeaderDeployment.Spec.Replicas = ptr.To[int32](3) }, false, false},
		{"native deleting", func(s *replicagroups.Snapshot) { s.LeaderDeployment.DeletionTimestamp = ptr.To(metav1.Now()) }, false, false},
		{"native absent", func(s *replicagroups.Snapshot) { s.LeaderDeployment = nil }, false, false},
		{"active UID belongs to another ReplicaSet", func(s *replicagroups.Snapshot) { s.Groups[1].Leader.OwnerReferences[0].UID = "old-rs" }, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newSubRoleFixtureForIdentity(t, leaderv1.GroupIdentityHash, 0, "a", "b").observe()
			tc.change(s)
			require.Equal(t, tc.beforeDeletes, subRoleNativeTargetsApplied(s))
			require.Equal(t, tc.complete, subRoleNativeSettled(s))
		})
	}
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
	require.Empty(t, s.LWS.Annotations[leaderv1.GroupScalePlanAnnotationKey])
	for _, group := range s.Groups {
		child, coherent := groupSubRole(group)
		require.True(t, coherent)
		require.Empty(t, child)
		require.Empty(t, group.Leader.Annotations[leaderv1.GroupScaleProtectionAnnotationKey])
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

func TestHashSubRoleReadyReservationPrecedesCountOnlyAssignments(t *testing.T) {
	s := newSubRoleFixtureForIdentity(t, leaderv1.GroupIdentityHash, 0, "a", "b").observe()
	s.Groups[1].Ready = false
	plan := &subRolePlan{ID: "accepted", Counts: map[string]int{"a": 1, "b": 1}, ReadyFloor: map[string]int{"b": 1}}
	assignments := subRolePlanAssignments(s, plan)
	require.Equal(t, "b", assignments[s.Groups[0].Leader.UID], "the only Ready group must serve b's floor")
	require.Equal(t, "a", assignments[s.Groups[1].Leader.UID], "count-only a can use the unready group")
}

func TestHashSubRoleTransactionsPreserveMatchingSurvivors(t *testing.T) {
	for _, target := range []map[string]int{{"a": 1, "b": 1}, {"a": 1, "b": 2}} {
		t.Run(fmt.Sprint(target), func(t *testing.T) {
			f := newSubRoleFixtureForIdentity(t, leaderv1.GroupIdentityHash, 0, "a", "a", "b")
			originalB := f.observe().Groups[2].Leader.UID
			require.ErrorIs(t, f.scale(target), errReplicaGroupsPending)
			for range 12 {
				f.sync()
				f.native()
			}
			s := f.observe()
			require.Empty(t, s.LWS.Annotations[subRolePlanAnnotation])
			require.Empty(t, s.LWS.Annotations[leaderv1.GroupScalePlanAnnotationKey])
			actual, keptB := map[string]int{}, false
			for _, group := range s.Groups {
				child, coherent := groupSubRole(group)
				require.True(t, coherent)
				actual[child]++
				keptB = keptB || child == "b" && group.Leader.UID == originalB
				require.True(t, subRoleProtected(s.LWS, group))
			}
			require.Equal(t, target, actual)
			require.True(t, keptB, "a matching Ready survivor must not be replaced by relabeling another group")
		})
	}
}

func TestHashSubRolePlanSurvivesLabelAndSpecWriteFailures(t *testing.T) {
	f := newSubRoleFixtureForIdentity(t, leaderv1.GroupIdentityHash, 0, "a", "a", "b")
	require.ErrorIs(t, f.scale(map[string]int{"a": 1, "b": 1}), errReplicaGroupsPending)
	labelFailure, specFailure := errors.New("leader patch failed"), errors.New("Spec patch failed")
	failedLabel, failedSpec := false, false
	f.manager.client = interceptor.NewClient(f.manager.client.(client.WithWatch), interceptor.Funcs{
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			if pod, ok := obj.(*corev1.Pod); ok && pod.Labels[leaderv1.WorkerIndexLabelKey] == "0" && !failedLabel {
				failedLabel = true
				return labelFailure
			}
			if lws, ok := obj.(*leaderv1.LeaderWorkerSet); ok && getLWSReplicas(lws) == 2 && !failedSpec {
				failedSpec = true
				return specFailure
			}
			return c.Patch(ctx, obj, patch, opts...)
		},
	})
	for range 16 {
		_, err := f.manager.syncSubRoles(t.Context(), f.ds, f.observe().LWS, subRoleUpdate{})
		require.True(t, err == nil || errors.Is(err, errReplicaGroupsPending) || errors.Is(err, labelFailure) || errors.Is(err, specFailure), "%v", err)
		if errors.Is(err, specFailure) {
			s := f.observe()
			require.EqualValues(t, 3, getLWSReplicas(s.LWS))
			require.Empty(t, s.LWS.Annotations[leaderv1.GroupScalePlanAnnotationKey], "failed Spec writes cannot publish delete authority")
			plan, err := readSubRolePlan(s.LWS)
			require.NoError(t, err)
			require.False(t, plan.Armed)
		}
		f.native()
	}
	require.True(t, failedLabel)
	require.True(t, failedSpec)
	s := f.observe()
	require.EqualValues(t, 2, getLWSReplicas(s.LWS))
	require.Empty(t, s.LWS.Annotations[subRolePlanAnnotation])
	require.Empty(t, s.LWS.Annotations[leaderv1.GroupScalePlanAnnotationKey])
}

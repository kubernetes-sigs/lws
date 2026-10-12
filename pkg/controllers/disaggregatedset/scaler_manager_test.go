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
	"context"
	"errors"
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	disaggregatedsetv1 "sigs.k8s.io/lws/api/disaggregatedset/v1"
	"sigs.k8s.io/lws/test/wrappers"
)

func newDSWithRoles(name string, roles ...disaggregatedsetv1.DisaggregatedRoleSpec) *disaggregatedsetv1.DisaggregatedSet {
	return &disaggregatedsetv1.DisaggregatedSet{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default", UID: types.UID("uid-" + name)},
		Spec:       disaggregatedsetv1.DisaggregatedSetSpec{Roles: roles},
	}
}

func externalRole(name string) disaggregatedsetv1.DisaggregatedRoleSpec {
	return disaggregatedsetv1.DisaggregatedRoleSpec{
		Name:    name,
		Scaling: &disaggregatedsetv1.RoleScaling{Mode: disaggregatedsetv1.RoleScalingExternal},
	}
}

func staticRole(name string) disaggregatedsetv1.DisaggregatedRoleSpec {
	return disaggregatedsetv1.DisaggregatedRoleSpec{Name: name}
}

func TestSubRoleStatusPreservesHistoricalCapacity(t *testing.T) {
	for _, tc := range []struct {
		name, child, invalid string
		ordinary, current    bool
		parent, childCount   int32
		assigned             bool
	}{
		{"ordinary history", "a", "", true, false, 2, 1, true},
		{"removed historical child", "removed", "", false, false, 2, 1, true},
		{"shared historical child", "a", "", false, false, 2, 2, true},
		{"unknown current child", "removed", "", false, true, 1, 1, false},
		{"incoherent history", "removed", "worker", false, false, 1, 1, false},
		{"unrecorded historical child", "a", "membership", false, false, 1, 1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSubRoleFixture(t, 0, "a")
			f.ds.Spec.Roles = []disaggregatedsetv1.DisaggregatedRoleSpec{{Name: "model", SubRoles: []disaggregatedsetv1.DisaggregatedSubRoleSpec{{Name: "a"}}}}
			require.NoError(t, f.manager.client.Update(t.Context(), f.ds))
			old := f.withRevision("old", tc.child)
			if tc.current {
				old.lws.Labels[disaggregatedsetv1.RevisionLabelKey] = f.lws.Name
			}
			if tc.ordinary {
				delete(old.lws.Annotations, subRoleReplicasAnnotation)
				delete(old.lws.Annotations, disaggregatedsetv1.InitialSubRoleReplicasAnnotationKey)
			} else if tc.invalid == "membership" {
				setSubRoleJSON(old.lws, subRoleReplicasAnnotation, map[string]int{"other": 1})
			}
			require.NoError(t, f.manager.client.Update(t.Context(), old.lws))
			for i, pod := range old.observe().Groups[0].Pods {
				if tc.ordinary || tc.invalid == "worker" && i == 1 {
					delete(pod.Labels, disaggregatedsetv1.SubRoleLabelKey)
					require.NoError(t, f.manager.client.Update(t.Context(), pod))
				}
			}
			r := &DisaggregatedSetReconciler{Client: f.manager.client, LWSManager: f.manager}
			desired := map[string]int{"model": 1, "model/a": 1}
			require.NoError(t, r.updateStatus(t.Context(), f.ds, []string{"model"}, f.lws.Name, desired, nil))
			require.Equal(t, []disaggregatedsetv1.RoleStatus{
				{Name: "model", Replicas: tc.parent, ReadyReplicas: tc.parent, UpdatedReplicas: 1},
				{Name: "model/a", Replicas: tc.childCount, ReadyReplicas: tc.childCount, UpdatedReplicas: 1},
			}, f.ds.Status.RoleStatuses)
			require.Equal(t, tc.assigned, meta.IsStatusConditionTrue(f.ds.Status.Conditions, string(disaggregatedsetv1.DisaggregatedSetSubRolesAssigned)))
			require.False(t, meta.IsStatusConditionTrue(f.ds.Status.Conditions, string(disaggregatedsetv1.DisaggregatedSetAvailable)), "a Ready target cannot hide surviving historical capacity or invalid assignments")
			require.NoError(t, f.manager.client.Delete(t.Context(), old.lws))
			require.NoError(t, r.updateStatus(t.Context(), f.ds, []string{"model"}, f.lws.Name, desired, nil))
			require.True(t, meta.IsStatusConditionTrue(f.ds.Status.Conditions, string(disaggregatedsetv1.DisaggregatedSetAvailable)))
		})
	}
}

func TestScalerManagerReconcileCreatesMissing(t *testing.T) {
	ds := newDSWithRoles("myds", externalRole("prefill"), staticRole("decode"))
	cl := fake.NewClientBuilder().WithScheme(wrappers.DisaggregatedSetTestScheme()).WithObjects(ds).Build()
	m := NewScalerManager(cl, events.NewFakeRecorder(10))

	scalers, err := m.Reconcile(context.TODO(), ds, nil)
	require.NoError(t, err)
	require.Contains(t, scalers, "prefill")
	require.NotContains(t, scalers, "decode")

	got := &disaggregatedsetv1.DisaggregatedSetRoleScaler{}
	require.NoError(t, cl.Get(context.TODO(), types.NamespacedName{Name: "myds-prefill", Namespace: "default"}, got))
	assert.Equal(t, "prefill", got.Labels[disaggregatedsetv1.RoleLabelKey])
	assert.Equal(t, "myds", got.Labels[disaggregatedsetv1.SetNameLabelKey])
	require.Len(t, got.OwnerReferences, 1)
	assert.Equal(t, ds.UID, got.OwnerReferences[0].UID)
	assert.Equal(t, ptr.To(true), got.OwnerReferences[0].Controller)
}

func TestScalerManagerReconcileDeletesOrphaned(t *testing.T) {
	ds := newDSWithRoles("myds", externalRole("prefill"), staticRole("decode"))
	stale := &disaggregatedsetv1.DisaggregatedSetRoleScaler{
		ObjectMeta: metav1.ObjectMeta{
			Name: "myds-old-role", Namespace: "default",
			Labels: map[string]string{
				disaggregatedsetv1.SetNameLabelKey: "myds",
				disaggregatedsetv1.RoleLabelKey:    "old-role",
			},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: disaggregatedsetv1.GroupVersion.String(), Kind: "DisaggregatedSet",
				Name: "myds", UID: ds.UID, Controller: ptr.To(true),
			}},
		},
	}
	cl := fake.NewClientBuilder().WithScheme(wrappers.DisaggregatedSetTestScheme()).WithObjects(ds, stale).Build()
	m := NewScalerManager(cl, events.NewFakeRecorder(10))

	_, err := m.Reconcile(context.TODO(), ds, nil)
	require.NoError(t, err)

	assert.True(t, apierrorsIsNotFound(cl.Get(context.TODO(), types.NamespacedName{Name: "myds-old-role", Namespace: "default"}, &disaggregatedsetv1.DisaggregatedSetRoleScaler{})))
}

func TestScalerManagerReconcileRefusesForeignScaler(t *testing.T) {
	ds := newDSWithRoles("myds", externalRole("prefill"), staticRole("decode"))
	foreign := &disaggregatedsetv1.DisaggregatedSetRoleScaler{
		ObjectMeta: metav1.ObjectMeta{Name: "myds-prefill", Namespace: "default"},
	}
	cl := fake.NewClientBuilder().WithScheme(wrappers.DisaggregatedSetTestScheme()).WithObjects(ds, foreign).Build()
	rec := events.NewFakeRecorder(10)
	m := NewScalerManager(cl, rec)

	scalers, err := m.Reconcile(context.TODO(), ds, nil)
	require.NoError(t, err)
	assert.NotContains(t, scalers, "prefill")

	got := &disaggregatedsetv1.DisaggregatedSetRoleScaler{}
	require.NoError(t, cl.Get(context.TODO(), types.NamespacedName{Name: "myds-prefill", Namespace: "default"}, got))
	assert.Empty(t, got.OwnerReferences, "foreign scaler must not be adopted")

	select {
	case ev := <-rec.Events:
		assert.Contains(t, ev, EventReasonScalerConflict)
	default:
		t.Fatal("expected conflict event")
	}
}

func TestResolveDesiredReplicasByRole(t *testing.T) {
	cases := []struct {
		name         string
		role         disaggregatedsetv1.DisaggregatedRoleSpec
		scalerHas    bool
		scalerVal    int32
		wantResolved bool
		wantReplicas int
	}{
		{"static default", staticRole("r"), false, 0, true, 1},
		{"static explicit", roleWithReplicas("r", 4), false, 0, true, 4},
		{"external + scaler seeded at 0", externalRole("r"), true, 0, true, 0},
		{"external + scaler written to 7", externalRole("r"), true, 7, true, 7},
		{"maximum target", externalRole("r"), true, math.MaxInt32, true, math.MaxInt32},
		{"external + scaler missing leaves target unresolved", externalRole("r"), false, 0, false, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ds := newDSWithRoles("d", tc.role)
			scalers := map[string]*disaggregatedsetv1.DisaggregatedSetRoleScaler{}
			if tc.scalerHas {
				scalers["r"] = &disaggregatedsetv1.DisaggregatedSetRoleScaler{
					Spec: disaggregatedsetv1.DisaggregatedSetRoleScalerSpec{Replicas: tc.scalerVal},
				}
			}
			desiredReplicasByRole := resolveDesiredReplicasByRole(ds, scalers)
			replicas, resolved := desiredReplicasByRole["r"]
			assert.Equal(t, tc.wantResolved, resolved)
			if tc.wantResolved {
				assert.Equal(t, tc.wantReplicas, replicas)
			}

			parent := roleWithReplicas("pool", 99) // Ignored when children exist.
			parent.SubRoles = []disaggregatedsetv1.DisaggregatedSubRoleSpec{
				{Name: "r", Replicas: tc.role.Spec.Replicas, Scaling: tc.role.Scaling},
				{Name: "default"}, {Name: "paused", Replicas: ptr.To[int32](0)},
			}
			want := map[string]int{"pool/default": 1, "pool/paused": 0}
			if tc.wantResolved {
				want["pool/r"] = tc.wantReplicas
				if tc.wantReplicas < math.MaxInt32 {
					want["pool"] = tc.wantReplicas + 1
				}
			}
			assert.Equal(t, want, resolveDesiredReplicasByRole(newDSWithRoles("d", parent), map[string]*disaggregatedsetv1.DisaggregatedSetRoleScaler{"pool/r": scalers["r"]}))
		})
	}
}

func roleWithReplicas(name string, replicas int32) disaggregatedsetv1.DisaggregatedRoleSpec {
	r := staticRole(name)
	r.Spec.Replicas = &replicas
	return r
}

func TestScalerManagerWriteStatus(t *testing.T) {
	ds := newDSWithRoles("myds", externalRole("prefill"))
	scaler := &disaggregatedsetv1.DisaggregatedSetRoleScaler{
		ObjectMeta: metav1.ObjectMeta{Name: "myds-prefill", Namespace: "default", Generation: 2},
		Spec:       disaggregatedsetv1.DisaggregatedSetRoleScalerSpec{Replicas: 5},
	}
	cl := fake.NewClientBuilder().
		WithScheme(wrappers.DisaggregatedSetTestScheme()).
		WithObjects(scaler).
		WithStatusSubresource(&disaggregatedsetv1.DisaggregatedSetRoleScaler{}).
		Build()
	m := NewScalerManager(cl, events.NewFakeRecorder(10))

	require.NoError(t, m.WriteStatus(context.TODO(), ds, map[string]*disaggregatedsetv1.DisaggregatedSetRoleScaler{"prefill": scaler}, map[string]int32{"prefill": 4}))

	got := &disaggregatedsetv1.DisaggregatedSetRoleScaler{}
	require.NoError(t, cl.Get(context.TODO(), types.NamespacedName{Name: "myds-prefill", Namespace: "default"}, got))
	assert.EqualValues(t, 4, got.Status.Replicas)
	assert.Equal(t, "disaggregatedset.x-k8s.io/name=myds,disaggregatedset.x-k8s.io/role=prefill,leaderworkerset.sigs.k8s.io/worker-index=0", got.Status.Selector)
	require.Len(t, got.Status.Conditions, 1)
	assert.Equal(t, metav1.ConditionTrue, got.Status.Conditions[0].Status)
}

// apierrorsIsNotFound is a tiny local helper to avoid importing apierrors just
// for one call in tests.
func apierrorsIsNotFound(err error) bool {
	return err != nil && client.IgnoreNotFound(err) == nil
}

func TestSubRoleScalerAndStatusLifecycle(t *testing.T) {
	for _, failure := range []string{"", "observe", "scaler", "set"} {
		t.Run(failure, func(t *testing.T) {
			f := newSubRoleFixture(t, 0, "a", "b")
			c := f.manager.client
			f.ds.Spec.Roles = []disaggregatedsetv1.DisaggregatedRoleSpec{{Name: "model", SubRoles: []disaggregatedsetv1.DisaggregatedSubRoleSpec{{Name: "b"}, {Name: "a", Scaling: externalRole("").Scaling}, {Name: "empty", Replicas: ptr.To[int32](0)}}}}
			require.NoError(t, c.Update(t.Context(), f.ds))
			previous := []disaggregatedsetv1.RoleStatus{{Name: "model", Replicas: 9}, {Name: "model/removed", Replicas: 1}}
			f.ds.Status.RoleStatuses = previous
			require.NoError(t, c.Status().Update(t.Context(), f.ds))
			old := f.withRevision("old", "a")
			podLists := 0
			f.manager.apiReader = interceptor.NewClient(c.(client.WithWatch), interceptor.Funcs{
				List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
					if _, pods := list.(*corev1.PodList); pods {
						podLists++
						if failure == "observe" {
							return errors.New("observation unavailable")
						}
					}
					return c.List(ctx, list, opts...)
				},
			})
			m := NewScalerManager(c, events.NewFakeRecorder(10))
			scalers, err := m.Reconcile(t.Context(), f.ds, func(key string) int32 {
				require.Equal(t, "model/a", key)
				return 9
			})
			require.NoError(t, err)
			require.Len(t, scalers, 1)
			scaler := scalers["model/a"]
			require.NotNil(t, scaler)
			require.Equal(t, "ds-model-a", scaler.Name)
			require.EqualValues(t, 9, scaler.Spec.Replicas)
			require.Equal(t, "model", scaler.Labels[disaggregatedsetv1.RoleLabelKey])
			require.Equal(t, "a", scaler.Labels[disaggregatedsetv1.SubRoleLabelKey])
			require.NoError(t, m.WriteStatus(t.Context(), f.ds, scalers, map[string]int32{"model/a": 9}))
			if failure == "scaler" {
				require.NoError(t, c.Delete(t.Context(), scaler))
			} else if failure == "set" {
				require.NoError(t, c.Delete(t.Context(), f.ds))
			}
			r := &DisaggregatedSetReconciler{Client: c, LWSManager: f.manager, ScalerManager: m}
			err = r.updateStatus(t.Context(), f.ds, []string{"model"}, f.lws.Name, map[string]int{"model": 3, "model/a": 2, "model/b": 1, "model/empty": 0}, scalers)
			require.Equal(t, failure != "", err != nil)
			want := []disaggregatedsetv1.RoleStatus{
				{Name: "model", Replicas: 3, ReadyReplicas: 3, UpdatedReplicas: 2},
				{Name: "model/b", Replicas: 1, ReadyReplicas: 1, UpdatedReplicas: 1},
				{Name: "model/a", Replicas: 2, ReadyReplicas: 2, UpdatedReplicas: 1}, {Name: "model/empty"},
			}
			wantScaler := int32(2)
			if failure == "observe" {
				want, wantScaler = previous, 9 // Unknown observation preserves both publications.
			}
			if failure != "scaler" {
				require.NoError(t, c.Get(t.Context(), client.ObjectKeyFromObject(scaler), scaler))
				require.Equal(t, wantScaler, scaler.Status.Replicas)
			}
			if failure != "set" {
				require.NoError(t, c.Get(t.Context(), client.ObjectKeyFromObject(f.ds), f.ds))
				require.Equal(t, want, f.ds.Status.RoleStatuses)
			}
			require.LessOrEqual(t, podLists, 2, "both writers share one observation per revision")
			if failure != "" {
				return
			}
			require.Equal(t, "disaggregatedset.x-k8s.io/name=ds,disaggregatedset.x-k8s.io/role=model,leaderworkerset.sigs.k8s.io/worker-index=0,disaggregatedset.x-k8s.io/subrole=a", scaler.Status.Selector)
			// Availability checks each child's target, not just the physical sum.
			require.NoError(t, c.Delete(t.Context(), old.lws))
			for _, a := range []int{1, 2} {
				desired := map[string]int{"model": 2, "model/a": a, "model/b": 2 - a, "model/empty": 0}
				require.NoError(t, r.updateStatus(t.Context(), f.ds, []string{"model"}, f.lws.Name, desired, nil))
				require.Equal(t, a == 1, meta.IsStatusConditionTrue(f.ds.Status.Conditions, string(disaggregatedsetv1.DisaggregatedSetAvailable)))
			}
			worker := f.observe().Groups[0].Pods[1]
			delete(worker.Labels, disaggregatedsetv1.SubRoleLabelKey)
			require.NoError(t, c.Update(t.Context(), worker))
			require.NoError(t, r.updateStatus(t.Context(), f.ds, []string{"model"}, f.lws.Name, map[string]int{"model": 2, "model/a": 1, "model/b": 1, "model/empty": 0}, nil))
			require.False(t, meta.IsStatusConditionTrue(f.ds.Status.Conditions, string(disaggregatedsetv1.DisaggregatedSetSubRolesAssigned)))
			require.EqualValues(t, 1, f.ds.Status.RoleStatuses[0].Replicas, "incoherent groups give neither parent nor child credit")
			// A new ordinary role must not adopt the retiring child's same DNS name.
			scaler.Finalizers = []string{"test/hold"}
			require.NoError(t, c.Update(t.Context(), scaler))
			f.ds.Spec.Roles = []disaggregatedsetv1.DisaggregatedRoleSpec{externalRole("model-a")}
			scalers, err = m.Reconcile(t.Context(), f.ds, func(string) int32 { return 3 })
			require.NoError(t, err)
			require.Empty(t, scalers)
			require.NoError(t, c.Get(t.Context(), client.ObjectKeyFromObject(scaler), scaler))
			require.False(t, scaler.DeletionTimestamp.IsZero())
			require.EqualValues(t, 9, scaler.Spec.Replicas, "retired target must not be adopted or reseeded")
			scaler.Finalizers = nil
			require.NoError(t, c.Update(t.Context(), scaler))
			require.True(t, apierrorsIsNotFound(c.Get(t.Context(), client.ObjectKeyFromObject(scaler), scaler)))
		})
	}
}

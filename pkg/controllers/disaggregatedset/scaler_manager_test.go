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
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	disaggregatedsetv1 "sigs.k8s.io/lws/api/disaggregatedset/v1"
	disaggregatedsetutils "sigs.k8s.io/lws/pkg/utils/disaggregatedset"
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

func TestScalerManagerReconcileCreatesMissing(t *testing.T) {
	ds := newDSWithRoles("myds", externalRole("prefill"), staticRole("decode"))
	cl := fake.NewClientBuilder().WithScheme(wrappers.DisaggregatedSetTestScheme()).WithObjects(ds).Build()
	m := NewScalerManager(cl, events.NewFakeRecorder(10))

	scalers, err := m.Reconcile(context.TODO(), ds, nil)
	require.NoError(t, err)
	require.Contains(t, scalers, disaggregatedsetutils.RoleKey{Role: "prefill"})
	require.NotContains(t, scalers, disaggregatedsetutils.RoleKey{Role: "decode"})

	got := &disaggregatedsetv1.DisaggregatedSetRoleScaler{}
	require.NoError(t, cl.Get(context.TODO(), types.NamespacedName{Name: "myds-prefill", Namespace: "default"}, got))
	assert.Equal(t, "prefill", got.Labels[disaggregatedsetv1.RoleLabelKey])
	assert.Equal(t, "myds", got.Labels[disaggregatedsetv1.SetNameLabelKey])
	require.Len(t, got.OwnerReferences, 1)
	assert.Equal(t, ds.UID, got.OwnerReferences[0].UID)
	assert.Equal(t, ptr.To(true), got.OwnerReferences[0].Controller)
}

func TestScalerManagerReconcileCreatesSubRoleScalers(t *testing.T) {
	ds := newDSWithRoles("myds", disaggregatedsetv1.DisaggregatedRoleSpec{
		Name: "decode",
		SubRoles: []disaggregatedsetv1.DisaggregatedSubRoleSpec{
			{Name: "short", Scaling: &disaggregatedsetv1.RoleScaling{Mode: disaggregatedsetv1.RoleScalingExternal}},
			{Name: "long", Replicas: ptr.To(int32(2))},
			{Name: "batch", Scaling: &disaggregatedsetv1.RoleScaling{Mode: disaggregatedsetv1.RoleScalingExternal}},
		},
	})
	cl := fake.NewClientBuilder().WithScheme(wrappers.DisaggregatedSetTestScheme()).WithObjects(ds).Build()
	m := NewScalerManager(cl, events.NewFakeRecorder(10))
	shortKey := disaggregatedsetutils.RoleKey{Role: "decode", SubRole: "short"}
	batchKey := disaggregatedsetutils.RoleKey{Role: "decode", SubRole: "batch"}

	scalers, err := m.Reconcile(context.TODO(), ds, func(key disaggregatedsetutils.RoleKey, existing scalerMap) int32 {
		assert.Empty(t, existing, "all seeds are computed before any scaler is created")
		if key == shortKey {
			return 3
		}
		return 1
	})
	require.NoError(t, err)
	require.Contains(t, scalers, shortKey)
	require.Contains(t, scalers, batchKey)

	short := &disaggregatedsetv1.DisaggregatedSetRoleScaler{}
	require.NoError(t, cl.Get(context.TODO(), types.NamespacedName{Name: "myds-decode-short", Namespace: "default"}, short))
	assert.EqualValues(t, 3, short.Spec.Replicas)
	assert.Equal(t, "decode", short.Labels[disaggregatedsetv1.RoleLabelKey])
	assert.Equal(t, "short", short.Labels[disaggregatedsetv1.SubRoleLabelKey])
	assert.True(t, apierrorsIsNotFound(cl.Get(context.TODO(), types.NamespacedName{Name: "myds-decode", Namespace: "default"}, &disaggregatedsetv1.DisaggregatedSetRoleScaler{})))
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
	assert.NotContains(t, scalers, disaggregatedsetutils.RoleKey{Role: "prefill"})

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

func TestGetTargetReplicasResolutionMatrix(t *testing.T) {
	cases := []struct {
		name         string
		role         disaggregatedsetv1.DisaggregatedRoleSpec
		scalerHas    bool
		scalerVal    int32
		currentNew   int
		wantReplicas int
	}{
		{"static default", staticRole("r"), false, 0, 0, 1},
		{"static explicit", roleWithReplicas("r", 4), false, 0, 0, 4},
		{"external + scaler seeded at 0", externalRole("r"), true, 0, 0, 0},
		{"external + scaler written to 7", externalRole("r"), true, 7, 0, 7},
		{"external + scaler missing (transient) falls back to currentNew", externalRole("r"), false, 0, 3, 3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ds := newDSWithRoles("d", tc.role)
			key := disaggregatedsetutils.RoleKey{Role: "r"}
			scalers := scalerMap{}
			if tc.scalerHas {
				scalers[key] = &disaggregatedsetv1.DisaggregatedSetRoleScaler{
					Spec: disaggregatedsetv1.DisaggregatedSetRoleScalerSpec{Replicas: tc.scalerVal},
				}
			}
			assert.Equal(t, tc.wantReplicas, getTargetReplicas(ds, key, scalers, tc.currentNew))
		})
	}
}

func TestMissingSubRoleScalerPreservesParentCapacity(t *testing.T) {
	ds := newDSWithRoles("d", disaggregatedsetv1.DisaggregatedRoleSpec{
		Name: "decode",
		SubRoles: []disaggregatedsetv1.DisaggregatedSubRoleSpec{
			{Name: "short", Scaling: &disaggregatedsetv1.RoleScaling{Mode: disaggregatedsetv1.RoleScalingExternal}},
			{Name: "long", Replicas: ptr.To(int32(2))},
		},
	})
	current := map[disaggregatedsetutils.RoleKey]int{
		{Role: "decode"}:                  7,
		{Role: "decode", SubRole: "long"}: 2,
	}
	assert.Equal(t, 7, getParentTargetReplicas(ds, "decode", nil, current))
}

func TestParentSubRoleTargetOverflowIsRejected(t *testing.T) {
	ds := newDSWithRoles("d", disaggregatedsetv1.DisaggregatedRoleSpec{
		Name: "decode",
		SubRoles: []disaggregatedsetv1.DisaggregatedSubRoleSpec{
			{Name: "short", Scaling: &disaggregatedsetv1.RoleScaling{Mode: disaggregatedsetv1.RoleScalingExternal}},
			{Name: "long", Scaling: &disaggregatedsetv1.RoleScaling{Mode: disaggregatedsetv1.RoleScalingExternal}},
		},
	})
	shortKey := disaggregatedsetutils.RoleKey{Role: "decode", SubRole: "short"}
	longKey := disaggregatedsetutils.RoleKey{Role: "decode", SubRole: "long"}
	scalers := scalerMap{
		shortKey: {Spec: disaggregatedsetv1.DisaggregatedSetRoleScalerSpec{Replicas: math.MaxInt32}},
		longKey:  {Spec: disaggregatedsetv1.DisaggregatedSetRoleScalerSpec{Replicas: 1}},
	}
	require.ErrorContains(t, validateParentReplicaTargets(ds, scalers), "exceeding the maximum LWS replica count")
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

	key := disaggregatedsetutils.RoleKey{Role: "prefill"}
	require.NoError(t, m.WriteStatus(context.TODO(), ds, scalerMap{key: scaler}, map[disaggregatedsetutils.RoleKey]int32{key: 4}))

	got := &disaggregatedsetv1.DisaggregatedSetRoleScaler{}
	require.NoError(t, cl.Get(context.TODO(), types.NamespacedName{Name: "myds-prefill", Namespace: "default"}, got))
	assert.EqualValues(t, 4, got.Status.Replicas)
	assert.Equal(t, "disaggregatedset.x-k8s.io/name=myds,disaggregatedset.x-k8s.io/role=prefill,leaderworkerset.sigs.k8s.io/worker-index=0", got.Status.Selector)
	require.Len(t, got.Status.Conditions, 1)
	assert.Equal(t, metav1.ConditionTrue, got.Status.Conditions[0].Status)
}

func TestScalerManagerWriteSubRoleStatusSelector(t *testing.T) {
	ds := newDSWithRoles("myds", staticRole("decode"))
	scaler := &disaggregatedsetv1.DisaggregatedSetRoleScaler{
		ObjectMeta: metav1.ObjectMeta{Name: "myds-decode-short", Namespace: "default", Generation: 2},
	}
	cl := fake.NewClientBuilder().
		WithScheme(wrappers.DisaggregatedSetTestScheme()).
		WithObjects(scaler).
		WithStatusSubresource(&disaggregatedsetv1.DisaggregatedSetRoleScaler{}).
		Build()
	m := NewScalerManager(cl, events.NewFakeRecorder(10))
	key := disaggregatedsetutils.RoleKey{Role: "decode", SubRole: "short"}
	require.NoError(t, m.WriteStatus(context.TODO(), ds, scalerMap{key: scaler}, map[disaggregatedsetutils.RoleKey]int32{key: 4}))

	got := &disaggregatedsetv1.DisaggregatedSetRoleScaler{}
	require.NoError(t, cl.Get(context.TODO(), types.NamespacedName{Name: scaler.Name, Namespace: scaler.Namespace}, got))
	assert.EqualValues(t, 4, got.Status.Replicas)
	assert.Equal(t, "disaggregatedset.x-k8s.io/name=myds,disaggregatedset.x-k8s.io/role=decode,disaggregatedset.x-k8s.io/subrole=short,leaderworkerset.sigs.k8s.io/worker-index=0", got.Status.Selector)
}

// apierrorsIsNotFound is a tiny local helper to avoid importing apierrors just
// for one call in tests.
func apierrorsIsNotFound(err error) bool {
	return err != nil && client.IgnoreNotFound(err) == nil
}

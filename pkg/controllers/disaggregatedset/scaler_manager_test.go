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

	scalers, err := m.Reconcile(context.TODO(), ds)
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

	_, err := m.Reconcile(context.TODO(), ds)
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

	scalers, err := m.Reconcile(context.TODO(), ds)
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

func TestGetTargetReplicasResolutionMatrix(t *testing.T) {
	cases := []struct {
		name         string
		role         disaggregatedsetv1.DisaggregatedRoleSpec
		scalerHas    bool
		scalerVal    *int32
		currentNew   int
		wantReplicas int
	}{
		{"static default", staticRole("r"), false, nil, 0, 1},
		{"static explicit", roleWithReplicas("r", 4), false, nil, 0, 4},
		{"external with scaler write", externalRole("r"), true, ptr.To(int32(7)), 0, 7},
		{"external without scaler write (fresh)", externalRole("r"), false, nil, 0, 0},
		{"external without scaler write (hold)", externalRole("r"), true, nil, 3, 3},
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
			assert.Equal(t, tc.wantReplicas, getTargetReplicas(ds, "r", scalers, tc.currentNew))
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
		Spec:       disaggregatedsetv1.DisaggregatedSetRoleScalerSpec{Replicas: ptr.To(int32(5))},
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
	assert.Equal(t, "disaggregatedset.x-k8s.io/name=myds,disaggregatedset.x-k8s.io/role=prefill", got.Status.Selector)
	require.Len(t, got.Status.Conditions, 1)
	assert.Equal(t, metav1.ConditionTrue, got.Status.Conditions[0].Status)
}

// apierrorsIsNotFound is a tiny local helper to avoid importing apierrors just
// for one call in tests.
func apierrorsIsNotFound(err error) bool {
	return err != nil && client.IgnoreNotFound(err) == nil
}

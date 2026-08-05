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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	leaderworkersetv1 "sigs.k8s.io/lws/api/leaderworkerset/v1"

	disaggregatedsetv1 "sigs.k8s.io/lws/api/disaggregatedset/v1"
	disaggregatedsetutils "sigs.k8s.io/lws/pkg/utils/disaggregatedset"
	"sigs.k8s.io/lws/test/wrappers"
)

func subRoleTestSet() *disaggregatedsetv1.DisaggregatedSet {
	return &disaggregatedsetv1.DisaggregatedSet{
		ObjectMeta: metav1.ObjectMeta{Name: "model", Namespace: "default", UID: "model-uid"},
		Spec: disaggregatedsetv1.DisaggregatedSetSpec{Roles: []disaggregatedsetv1.DisaggregatedRoleSpec{{
			Name: "decode",
			SubRoles: []disaggregatedsetv1.DisaggregatedSubRoleSpec{
				{Name: "short", Replicas: ptr.To(int32(5))},
				{Name: "long", Replicas: ptr.To(int32(2))},
			},
		}}},
	}
}

func subRoleTestLWS(ds *disaggregatedsetv1.DisaggregatedSet, revision string, replicas int32) *leaderworkersetv1.LeaderWorkerSet {
	return &leaderworkersetv1.LeaderWorkerSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      disaggregatedsetutils.GenerateName(ds.Name, 0, revision, "decode"),
			Namespace: ds.Namespace,
			Labels:    disaggregatedsetutils.GenerateLabels(ds.Name, 0, revision, "decode"),
		},
		Spec: leaderworkersetv1.LeaderWorkerSetSpec{Replicas: ptr.To(replicas)},
	}
}

func TestReconcileSingleParentCreatesSummedLWSAndServices(t *testing.T) {
	ctx := context.Background()
	ds := subRoleTestSet()
	cl := fake.NewClientBuilder().
		WithScheme(wrappers.DisaggregatedSetTestScheme()).
		WithObjects(ds).
		WithStatusSubresource(&disaggregatedsetv1.DisaggregatedSet{}, &disaggregatedsetv1.DisaggregatedSetRoleScaler{}).
		Build()
	recorder := events.NewFakeRecorder(20)
	r := &DisaggregatedSetReconciler{
		Client:            cl,
		Scheme:            wrappers.DisaggregatedSetTestScheme(),
		Record:            recorder,
		LWSManager:        NewLeaderWorkerSetManager(cl),
		ServiceManager:    NewServiceManager(cl, wrappers.DisaggregatedSetTestScheme()),
		ScalerManager:     NewScalerManager(cl, recorder),
		AssignmentManager: NewAssignmentManager(cl),
	}

	result, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ds.Namespace, Name: ds.Name}})
	require.NoError(t, err)
	assert.NotZero(t, result.RequeueAfter, "assignment waits for the LWS Pods to appear")

	lwsList := &leaderworkersetv1.LeaderWorkerSetList{}
	require.NoError(t, cl.List(ctx, lwsList, client.InNamespace(ds.Namespace)))
	require.Len(t, lwsList.Items, 1, "sub-roles share one physical LWS")
	assert.EqualValues(t, 7, *lwsList.Items[0].Spec.Replicas)

	services := &corev1.ServiceList{}
	require.NoError(t, cl.List(ctx, services, client.InNamespace(ds.Namespace)))
	require.Len(t, services.Items, 3, "one parent and one Service per sub-role")
	selectors := make(map[string]map[string]string, len(services.Items))
	for i := range services.Items {
		selectors[services.Items[i].Name] = services.Items[i].Spec.Selector
	}
	parentName := lwsList.Items[0].Name + "-prv"
	shortName := lwsList.Items[0].Name + "-short-prv"
	longName := lwsList.Items[0].Name + "-long-prv"
	assert.NotContains(t, selectors[parentName], disaggregatedsetv1.SubRoleLabelKey)
	assert.Equal(t, "short", selectors[shortName][disaggregatedsetv1.SubRoleLabelKey])
	assert.Equal(t, "long", selectors[longName][disaggregatedsetv1.SubRoleLabelKey])
}

func TestReconcileStaticSubRolesPerSlice(t *testing.T) {
	ctx := context.Background()
	ds := subRoleTestSet()
	ds.Spec.Slices = ptr.To(int32(2))
	cl := fake.NewClientBuilder().
		WithScheme(wrappers.DisaggregatedSetTestScheme()).
		WithObjects(ds).
		WithStatusSubresource(&disaggregatedsetv1.DisaggregatedSet{}).
		Build()
	recorder := events.NewFakeRecorder(20)
	r := &DisaggregatedSetReconciler{
		Client:            cl,
		Record:            recorder,
		LWSManager:        NewLeaderWorkerSetManager(cl),
		ServiceManager:    NewServiceManager(cl, wrappers.DisaggregatedSetTestScheme()),
		ScalerManager:     NewScalerManager(cl, recorder),
		AssignmentManager: NewAssignmentManager(cl),
	}

	_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ds.Namespace, Name: ds.Name}})
	require.NoError(t, err)
	lwsList := &leaderworkersetv1.LeaderWorkerSetList{}
	require.NoError(t, cl.List(ctx, lwsList, client.InNamespace(ds.Namespace)))
	require.Len(t, lwsList.Items, 2)
	for i := range lwsList.Items {
		assert.EqualValues(t, 7, *lwsList.Items[i].Spec.Replicas, "Static targets have per-slice semantics")
	}
	services := &corev1.ServiceList{}
	require.NoError(t, cl.List(ctx, services, client.InNamespace(ds.Namespace)))
	assert.Len(t, services.Items, 6)
}

func TestSeedForRolePreservesObservedSubRoleDistribution(t *testing.T) {
	ctx := context.Background()
	ds := subRoleTestSet()
	for i := range ds.Spec.Roles[0].SubRoles {
		ds.Spec.Roles[0].SubRoles[i].Scaling = &disaggregatedsetv1.RoleScaling{Mode: disaggregatedsetv1.RoleScalingExternal}
		ds.Spec.Roles[0].SubRoles[i].Replicas = nil
	}
	lws := subRoleTestLWS(ds, "revision", 7)
	objects := []client.Object{lws}
	for group := range 7 {
		subRole := "short"
		if group >= 5 {
			subRole = "long"
		}
		objects = append(objects, assignmentPod(lws.Name, group, 0, subRole))
	}
	cl := fake.NewClientBuilder().WithScheme(wrappers.DisaggregatedSetTestScheme()).WithObjects(objects...).Build()
	r := &DisaggregatedSetReconciler{
		Client:            cl,
		LWSManager:        NewLeaderWorkerSetManager(cl),
		AssignmentManager: NewAssignmentManager(cl),
	}

	seedFor, err := r.seedForRole(ctx, ds)
	require.NoError(t, err)
	assert.EqualValues(t, 5, seedFor(disaggregatedsetutils.RoleKey{Role: "decode", SubRole: "short"}, nil))
	assert.EqualValues(t, 2, seedFor(disaggregatedsetutils.RoleKey{Role: "decode", SubRole: "long"}, nil))
}

func TestSeedForNewExternalSubRoleAccountsForExistingTargets(t *testing.T) {
	ctx := context.Background()
	ds := &disaggregatedsetv1.DisaggregatedSet{
		ObjectMeta: metav1.ObjectMeta{Name: "model", Namespace: "default", UID: "model-uid"},
		Spec: disaggregatedsetv1.DisaggregatedSetSpec{Roles: []disaggregatedsetv1.DisaggregatedRoleSpec{{
			Name: "decode",
			SubRoles: []disaggregatedsetv1.DisaggregatedSubRoleSpec{
				{Name: "short", Scaling: &disaggregatedsetv1.RoleScaling{Mode: disaggregatedsetv1.RoleScalingExternal}},
				{Name: "long", Replicas: ptr.To(int32(2))},
				{Name: "new", Scaling: &disaggregatedsetv1.RoleScaling{Mode: disaggregatedsetv1.RoleScalingExternal}},
			},
		}}},
	}
	lws := subRoleTestLWS(ds, "revision", 7)
	objects := []client.Object{lws}
	for group := range 7 {
		subRole := "short"
		if group >= 5 {
			subRole = "long"
		}
		objects = append(objects, assignmentPod(lws.Name, group, 0, subRole))
	}
	cl := fake.NewClientBuilder().WithScheme(wrappers.DisaggregatedSetTestScheme()).WithObjects(objects...).Build()
	r := &DisaggregatedSetReconciler{LWSManager: NewLeaderWorkerSetManager(cl), AssignmentManager: NewAssignmentManager(cl)}
	seedFor, err := r.seedForRole(ctx, ds)
	require.NoError(t, err)
	shortKey := disaggregatedsetutils.RoleKey{Role: "decode", SubRole: "short"}
	newKey := disaggregatedsetutils.RoleKey{Role: "decode", SubRole: "new"}
	existing := scalerMap{shortKey: {
		ObjectMeta: metav1.ObjectMeta{Name: "model-decode-short"},
		Spec:       disaggregatedsetv1.DisaggregatedSetRoleScalerSpec{Replicas: 5},
	}}

	assert.Zero(t, seedFor(newKey, existing), "adding a pool must not inflate existing aggregate capacity")
}

func TestUpdateDisaggregatedSetStatusAggregatesSubRoles(t *testing.T) {
	ctx := context.Background()
	ds := subRoleTestSet()
	lws := subRoleTestLWS(ds, "revision", 2)
	lws.Status.Replicas = 2
	short := assignmentPod(lws.Name, 0, 0, "short")
	long := assignmentPod(lws.Name, 1, 0, "long")
	long.Status.Conditions[0].Status = corev1.ConditionFalse
	cl := fake.NewClientBuilder().
		WithScheme(wrappers.DisaggregatedSetTestScheme()).
		WithObjects(ds, lws, short, long).
		WithStatusSubresource(&disaggregatedsetv1.DisaggregatedSet{}).
		Build()
	r := &DisaggregatedSetReconciler{
		Client:            cl,
		LWSManager:        NewLeaderWorkerSetManager(cl),
		AssignmentManager: NewAssignmentManager(cl),
	}

	require.NoError(t, r.updateDisaggregatedSetStatus(ctx, ds, "revision"))
	got := &disaggregatedsetv1.DisaggregatedSet{}
	require.NoError(t, cl.Get(ctx, types.NamespacedName{Namespace: ds.Namespace, Name: ds.Name}, got))
	require.Len(t, got.Status.RoleStatuses, 1)
	roleStatus := got.Status.RoleStatuses[0]
	assert.EqualValues(t, 2, roleStatus.Replicas)
	assert.EqualValues(t, 1, roleStatus.ReadyReplicas)
	assert.EqualValues(t, 2, roleStatus.UpdatedReplicas)
	require.Len(t, roleStatus.SubRoleStatuses, 2)
	assert.Equal(t, disaggregatedsetv1.SubRoleStatus{Name: "short", Replicas: 1, ReadyReplicas: 1, UpdatedReplicas: 1}, roleStatus.SubRoleStatuses[0])
	assert.Equal(t, disaggregatedsetv1.SubRoleStatus{Name: "long", Replicas: 1, UpdatedReplicas: 1}, roleStatus.SubRoleStatuses[1])
	require.Len(t, got.Status.Conditions, 1)
	assert.Equal(t, metav1.ConditionTrue, got.Status.Conditions[0].Status)
}

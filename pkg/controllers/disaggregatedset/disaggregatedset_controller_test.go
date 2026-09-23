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

package disaggregatedset_test

import (
	"context"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	leaderworkersetv1 "sigs.k8s.io/lws/api/leaderworkerset/v1"

	disaggregatedsetv1 "sigs.k8s.io/lws/api/disaggregatedset/v1"
	controller "sigs.k8s.io/lws/pkg/controllers/disaggregatedset"
	disaggregatedsetutils "sigs.k8s.io/lws/pkg/utils/disaggregatedset"
	"sigs.k8s.io/lws/test/wrappers"
)

// Test-local role names
const (
	testControllerRolePrefill = "prefill"
	testControllerRoleDecode  = "decode"
)

// createOldLeaderWorkerSet creates a LeaderWorkerSet representing an existing LWS with the given revision.
// Useful for simulating pre-existing LWS objects in rolling update tests.
func createOldLeaderWorkerSet(disaggregatedSet *disaggregatedsetv1.DisaggregatedSet, role, revision string, replicas int32) *leaderworkersetv1.LeaderWorkerSet {
	labels := map[string]string{
		disaggregatedsetv1.SetNameLabelKey:  disaggregatedSet.Name,
		disaggregatedsetv1.SliceLabelKey:    "0",
		disaggregatedsetv1.RoleLabelKey:     role,
		disaggregatedsetv1.RevisionLabelKey: revision,
	}

	return wrappers.BuildBasicLeaderWorkerSet(disaggregatedSet.Name+"-0-"+revision+"-"+role, disaggregatedSet.Namespace).
		Labels(labels).
		Replica(int(replicas)).
		Size(1).
		StatusReplicas(replicas).
		ReadyReplicas(replicas).
		OwnerReference(metav1.OwnerReference{
			APIVersion: disaggregatedsetv1.GroupVersion.String(),
			Kind:       "DisaggregatedSet",
			Name:       disaggregatedSet.Name,
			UID:        disaggregatedSet.UID,
			Controller: ptr.To(true),
		}).
		WorkerTemplateSpec(corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "nginx:1.0"}}}).
		Obj()
}

func TestFreshDeploymentNoRollingUpdate(t *testing.T) {
	ctx := context.Background()
	scheme := wrappers.DisaggregatedSetTestScheme()

	disaggregatedSet := wrappers.BuildDisaggregatedSet("fresh-deploy", "default").
		WithRole(testControllerRolePrefill, 3, "nginx:1.0").
		WithRole(testControllerRoleDecode, 2, "nginx:1.0").
		Obj()

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(disaggregatedSet).
		WithStatusSubresource(&disaggregatedsetv1.DisaggregatedSet{}, &leaderworkersetv1.LeaderWorkerSet{}).Build()
	reconciler := &controller.DisaggregatedSetReconciler{
		Client:     fakeClient,
		Scheme:     scheme,
		LWSManager: controller.NewLeaderWorkerSetManager(fakeClient),
		Record:     events.NewFakeRecorder(100),
	}

	_, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: disaggregatedSet.Name, Namespace: disaggregatedSet.Namespace}})
	require.NoError(t, err, "Reconcile should succeed")

	newRevision := disaggregatedsetutils.ComputeRevision(disaggregatedSet.Spec.Roles)
	lwsManager := controller.NewLeaderWorkerSetManager(fakeClient)

	prefillInfo, _ := lwsManager.Get(ctx, disaggregatedSet, disaggregatedsetutils.GenerateName(disaggregatedSet.Name, 0, newRevision, testControllerRolePrefill))
	require.NotNil(t, prefillInfo, "prefill LWS should exist")
	assert.Equal(t, 3, int(*prefillInfo.Spec.Replicas), "prefill replicas")

	decodeInfo, _ := lwsManager.Get(ctx, disaggregatedSet, disaggregatedsetutils.GenerateName(disaggregatedSet.Name, 0, newRevision, testControllerRoleDecode))
	require.NotNil(t, decodeInfo, "decode LWS should exist")
	assert.Equal(t, 2, int(*decodeInfo.Spec.Replicas), "decode replicas")
}

func TestScalingWithoutRollingUpdate(t *testing.T) {
	ctx := context.Background()
	scheme := wrappers.DisaggregatedSetTestScheme()

	disaggregatedSet := wrappers.BuildDisaggregatedSet("scale-test", "default").
		WithRole(testControllerRolePrefill, 5, "nginx:1.0").
		WithRole(testControllerRoleDecode, 4, "nginx:1.0").
		Obj()
	revision := disaggregatedsetutils.ComputeRevision(disaggregatedSet.Spec.Roles)

	prefillRS := createOldLeaderWorkerSet(disaggregatedSet, testControllerRolePrefill, revision, 3)
	decodeRS := createOldLeaderWorkerSet(disaggregatedSet, testControllerRoleDecode, revision, 2)

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(disaggregatedSet, prefillRS, decodeRS).
		WithStatusSubresource(&disaggregatedsetv1.DisaggregatedSet{}, &leaderworkersetv1.LeaderWorkerSet{}).Build()
	reconciler := &controller.DisaggregatedSetReconciler{
		Client:     fakeClient,
		Scheme:     scheme,
		LWSManager: controller.NewLeaderWorkerSetManager(fakeClient),
		Record:     events.NewFakeRecorder(100),
	}

	_, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: disaggregatedSet.Name, Namespace: disaggregatedSet.Namespace}})
	require.NoError(t, err, "Reconcile should succeed")

	lwsManager := controller.NewLeaderWorkerSetManager(fakeClient)

	prefillInfo, _ := lwsManager.Get(ctx, disaggregatedSet, disaggregatedsetutils.GenerateName(disaggregatedSet.Name, 0, revision, testControllerRolePrefill))
	require.NotNil(t, prefillInfo, "prefill LWS should exist")
	assert.Equal(t, 5, int(*prefillInfo.Spec.Replicas), "prefill replicas should be scaled to 5")

	decodeInfo, _ := lwsManager.Get(ctx, disaggregatedSet, disaggregatedsetutils.GenerateName(disaggregatedSet.Name, 0, revision, testControllerRoleDecode))
	require.NotNil(t, decodeInfo, "decode LWS should exist")
	assert.Equal(t, 4, int(*decodeInfo.Spec.Replicas), "decode replicas should be scaled to 4")
}

// createSliceLWS builds an LWS for a specific slice using the real name/label
// helpers, so its name and slice label match what the controller generates.
func createSliceLWS(disaggregatedSet *disaggregatedsetv1.DisaggregatedSet, slice int, role, revision string) *leaderworkersetv1.LeaderWorkerSet {
	return wrappers.BuildBasicLeaderWorkerSet(disaggregatedsetutils.GenerateName(disaggregatedSet.Name, slice, revision, role), disaggregatedSet.Namespace).
		Labels(disaggregatedsetutils.GenerateLabels(disaggregatedSet.Name, slice, revision, role)).
		Replica(2).
		Size(1).
		StatusReplicas(2).
		ReadyReplicas(2).
		OwnerReference(metav1.OwnerReference{
			APIVersion: disaggregatedsetv1.GroupVersion.String(),
			Kind:       "DisaggregatedSet",
			Name:       disaggregatedSet.Name,
			UID:        disaggregatedSet.UID,
			Controller: ptr.To(true),
		}).
		WorkerTemplateSpec(corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "nginx:1.0"}}}).
		Obj()
}

func TestSlicesCreateOneSetPerSlice(t *testing.T) {
	ctx := context.Background()
	scheme := wrappers.DisaggregatedSetTestScheme()

	disaggregatedSet := wrappers.BuildDisaggregatedSet("multi-slice", "default").
		Slices(2).
		WithRole(testControllerRolePrefill, 2, "nginx:1.0").
		WithRole(testControllerRoleDecode, 3, "nginx:1.0").
		Obj()

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(disaggregatedSet).
		WithStatusSubresource(&disaggregatedsetv1.DisaggregatedSet{}, &leaderworkersetv1.LeaderWorkerSet{}).Build()
	reconciler := &controller.DisaggregatedSetReconciler{
		Client:     fakeClient,
		Scheme:     scheme,
		LWSManager: controller.NewLeaderWorkerSetManager(fakeClient),
		Record:     events.NewFakeRecorder(100),
	}

	_, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: disaggregatedSet.Name, Namespace: disaggregatedSet.Namespace}})
	require.NoError(t, err, "Reconcile should succeed")

	revision := disaggregatedsetutils.ComputeRevision(disaggregatedSet.Spec.Roles)
	lwsManager := controller.NewLeaderWorkerSetManager(fakeClient)

	for slice := range 2 {
		prefill, _ := lwsManager.Get(ctx, disaggregatedSet, disaggregatedsetutils.GenerateName(disaggregatedSet.Name, slice, revision, testControllerRolePrefill))
		require.NotNil(t, prefill, "prefill LWS should exist for slice %d", slice)
		assert.Equal(t, 2, int(*prefill.Spec.Replicas), "prefill replicas slice %d", slice)
		assert.Equal(t, strconv.Itoa(slice), prefill.Labels[disaggregatedsetv1.SliceLabelKey], "slice label slice %d", slice)

		decode, _ := lwsManager.Get(ctx, disaggregatedSet, disaggregatedsetutils.GenerateName(disaggregatedSet.Name, slice, revision, testControllerRoleDecode))
		require.NotNil(t, decode, "decode LWS should exist for slice %d", slice)
		assert.Equal(t, 3, int(*decode.Spec.Replicas), "decode replicas slice %d", slice)
	}

	var all leaderworkersetv1.LeaderWorkerSetList
	require.NoError(t, fakeClient.List(ctx, &all))
	assert.Len(t, all.Items, 4, "should create slices*roles LWS")
}

func TestSlicesScaleDownDeletesRemovedSlice(t *testing.T) {
	ctx := context.Background()
	scheme := wrappers.DisaggregatedSetTestScheme()

	// Desired slices = 1, but slice 1's LWS already exist (as if slices was 2).
	disaggregatedSet := wrappers.BuildDisaggregatedSet("scale-slice", "default").
		Slices(1).
		WithRole(testControllerRolePrefill, 2, "nginx:1.0").
		WithRole(testControllerRoleDecode, 2, "nginx:1.0").
		Obj()
	revision := disaggregatedsetutils.ComputeRevision(disaggregatedSet.Spec.Roles)

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		disaggregatedSet,
		createSliceLWS(disaggregatedSet, 0, testControllerRolePrefill, revision),
		createSliceLWS(disaggregatedSet, 0, testControllerRoleDecode, revision),
		createSliceLWS(disaggregatedSet, 1, testControllerRolePrefill, revision),
		createSliceLWS(disaggregatedSet, 1, testControllerRoleDecode, revision),
	).WithStatusSubresource(&disaggregatedsetv1.DisaggregatedSet{}, &leaderworkersetv1.LeaderWorkerSet{}).Build()
	reconciler := &controller.DisaggregatedSetReconciler{
		Client:     fakeClient,
		Scheme:     scheme,
		LWSManager: controller.NewLeaderWorkerSetManager(fakeClient),
		Record:     events.NewFakeRecorder(100),
	}

	_, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: disaggregatedSet.Name, Namespace: disaggregatedSet.Namespace}})
	require.NoError(t, err, "Reconcile should succeed")

	lwsManager := controller.NewLeaderWorkerSetManager(fakeClient)

	// Slice 0 is kept.
	s0, _ := lwsManager.Get(ctx, disaggregatedSet, disaggregatedsetutils.GenerateName(disaggregatedSet.Name, 0, revision, testControllerRolePrefill))
	require.NotNil(t, s0, "slice 0 prefill should be kept")

	// Slice 1 (>= desired) is deleted.
	s1p, _ := lwsManager.Get(ctx, disaggregatedSet, disaggregatedsetutils.GenerateName(disaggregatedSet.Name, 1, revision, testControllerRolePrefill))
	assert.Nil(t, s1p, "slice 1 prefill should be deleted")
	s1d, _ := lwsManager.Get(ctx, disaggregatedSet, disaggregatedsetutils.GenerateName(disaggregatedSet.Name, 1, revision, testControllerRoleDecode))
	assert.Nil(t, s1d, "slice 1 decode should be deleted")
}

// TestStatusPopulatedOnFreshDeployment: a fresh DisaggregatedSet has just created its
// LWS objects, which have not yet reported any ready/updated replicas. Status should
// reflect that with zero counts per role, not stay empty (#868).
func TestStatusPopulatedOnFreshDeployment(t *testing.T) {
	ctx := context.Background()
	scheme := wrappers.DisaggregatedSetTestScheme()

	disaggregatedSet := wrappers.BuildDisaggregatedSet("fresh-status", "default").
		WithRole(testControllerRolePrefill, 3, "nginx:1.0").
		WithRole(testControllerRoleDecode, 2, "nginx:1.0").
		Obj()

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(disaggregatedSet).
		WithStatusSubresource(&disaggregatedsetv1.DisaggregatedSet{}, &leaderworkersetv1.LeaderWorkerSet{}).Build()
	reconciler := &controller.DisaggregatedSetReconciler{
		Client:     fakeClient,
		Scheme:     scheme,
		LWSManager: controller.NewLeaderWorkerSetManager(fakeClient),
		Record:     events.NewFakeRecorder(100),
	}

	_, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: disaggregatedSet.Name, Namespace: disaggregatedSet.Namespace}})
	require.NoError(t, err, "Reconcile should succeed")

	var got disaggregatedsetv1.DisaggregatedSet
	require.NoError(t, fakeClient.Get(ctx, types.NamespacedName{Name: disaggregatedSet.Name, Namespace: disaggregatedSet.Namespace}, &got))

	require.Len(t, got.Status.RoleStatuses, 2, "one RoleStatus per role")
	assert.Equal(t, testControllerRolePrefill, got.Status.RoleStatuses[0].Name, "RoleStatuses order matches spec.roles")
	assert.Equal(t, testControllerRoleDecode, got.Status.RoleStatuses[1].Name, "RoleStatuses order matches spec.roles")
	for _, rs := range got.Status.RoleStatuses {
		assert.Zero(t, rs.Replicas, "freshly created LWS has not reported replicas yet")
		assert.Zero(t, rs.ReadyReplicas)
		assert.Zero(t, rs.UpdatedReplicas)
	}

	assert.Equal(t, got.Generation, got.Status.ObservedGeneration, "observedGeneration should track .metadata.generation")

	cond := meta.FindStatusCondition(got.Status.Conditions, string(disaggregatedsetv1.DisaggregatedSetProgressing))
	require.NotNil(t, cond, "Progressing condition should be set")
	assert.Equal(t, metav1.ConditionTrue, cond.Status)
	assert.Nil(t, meta.FindStatusCondition(got.Status.Conditions, string(disaggregatedsetv1.DisaggregatedSetAvailable)), "Available should not be set yet")
}

// TestStatusRoleCountsAggregateFromOwnedLWS: roleStatuses sums replicas/ready/updated
// from the LWS objects each role owns (#868).
func TestStatusRoleCountsAggregateFromOwnedLWS(t *testing.T) {
	ctx := context.Background()
	scheme := wrappers.DisaggregatedSetTestScheme()

	disaggregatedSet := wrappers.BuildDisaggregatedSet("ready-status", "default").
		WithRole(testControllerRolePrefill, 2, "nginx:1.0").
		WithRole(testControllerRoleDecode, 2, "nginx:1.0").
		Obj()
	revision := disaggregatedsetutils.ComputeRevision(disaggregatedSet.Spec.Roles)

	readyLWS := func(role string) *leaderworkersetv1.LeaderWorkerSet {
		return wrappers.BuildBasicLeaderWorkerSet(disaggregatedsetutils.GenerateName(disaggregatedSet.Name, 0, revision, role), disaggregatedSet.Namespace).
			Labels(disaggregatedsetutils.GenerateLabels(disaggregatedSet.Name, 0, revision, role)).
			Replica(2).
			Size(1).
			StatusReplicas(2).
			ReadyReplicas(2).
			UpdatedReplicas(2).
			OwnerReference(metav1.OwnerReference{
				APIVersion: disaggregatedsetv1.GroupVersion.String(),
				Kind:       "DisaggregatedSet",
				Name:       disaggregatedSet.Name,
				UID:        disaggregatedSet.UID,
				Controller: ptr.To(true),
			}).
			WorkerTemplateSpec(corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "nginx:1.0"}}}).
			Obj()
	}

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		disaggregatedSet,
		readyLWS(testControllerRolePrefill),
		readyLWS(testControllerRoleDecode),
	).WithStatusSubresource(&disaggregatedsetv1.DisaggregatedSet{}, &leaderworkersetv1.LeaderWorkerSet{}).Build()
	reconciler := &controller.DisaggregatedSetReconciler{
		Client:     fakeClient,
		Scheme:     scheme,
		LWSManager: controller.NewLeaderWorkerSetManager(fakeClient),
		Record:     events.NewFakeRecorder(100),
	}

	_, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: disaggregatedSet.Name, Namespace: disaggregatedSet.Namespace}})
	require.NoError(t, err, "Reconcile should succeed")

	var got disaggregatedsetv1.DisaggregatedSet
	require.NoError(t, fakeClient.Get(ctx, types.NamespacedName{Name: disaggregatedSet.Name, Namespace: disaggregatedSet.Namespace}, &got))

	require.Len(t, got.Status.RoleStatuses, 2)
	for _, rs := range got.Status.RoleStatuses {
		assert.EqualValues(t, 2, rs.Replicas, "role %s replicas", rs.Name)
		assert.EqualValues(t, 2, rs.ReadyReplicas, "role %s readyReplicas", rs.Name)
		assert.EqualValues(t, 2, rs.UpdatedReplicas, "role %s updatedReplicas", rs.Name)
	}

	cond := meta.FindStatusCondition(got.Status.Conditions, string(disaggregatedsetv1.DisaggregatedSetAvailable))
	require.NotNil(t, cond, "Available condition should be set once every role is at its desired count, ready and updated")
	assert.Equal(t, metav1.ConditionTrue, cond.Status)

	progressing := meta.FindStatusCondition(got.Status.Conditions, string(disaggregatedsetv1.DisaggregatedSetProgressing))
	if progressing != nil {
		assert.Equal(t, metav1.ConditionFalse, progressing.Status, "Progressing must not also be true once Available")
	}
}

// TestStatusProgressingWhenUnderDesiredCount: a role whose running replicas are all
// ready and updated is still Progressing if it hasn't reached its *desired* replica
// count yet — internal consistency alone isn't enough to call it Available.
func TestStatusProgressingWhenUnderDesiredCount(t *testing.T) {
	ctx := context.Background()
	scheme := wrappers.DisaggregatedSetTestScheme()

	disaggregatedSet := wrappers.BuildDisaggregatedSet("under-scaled", "default").
		WithRole(testControllerRolePrefill, 3, "nginx:1.0").
		WithRole(testControllerRoleDecode, 2, "nginx:1.0").
		Obj()
	revision := disaggregatedsetutils.ComputeRevision(disaggregatedSet.Spec.Roles)

	// prefill wants 3 but only 1 has come up so far; decode is fully at its desired 2.
	partialLWS := func(role string, replicas int32) *leaderworkersetv1.LeaderWorkerSet {
		return wrappers.BuildBasicLeaderWorkerSet(disaggregatedsetutils.GenerateName(disaggregatedSet.Name, 0, revision, role), disaggregatedSet.Namespace).
			Labels(disaggregatedsetutils.GenerateLabels(disaggregatedSet.Name, 0, revision, role)).
			Replica(int(replicas)).
			Size(1).
			StatusReplicas(replicas).
			ReadyReplicas(replicas).
			UpdatedReplicas(replicas).
			OwnerReference(metav1.OwnerReference{
				APIVersion: disaggregatedsetv1.GroupVersion.String(),
				Kind:       "DisaggregatedSet",
				Name:       disaggregatedSet.Name,
				UID:        disaggregatedSet.UID,
				Controller: ptr.To(true),
			}).
			WorkerTemplateSpec(corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "nginx:1.0"}}}).
			Obj()
	}

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		disaggregatedSet,
		partialLWS(testControllerRolePrefill, 1),
		partialLWS(testControllerRoleDecode, 2),
	).WithStatusSubresource(&disaggregatedsetv1.DisaggregatedSet{}, &leaderworkersetv1.LeaderWorkerSet{}).Build()
	reconciler := &controller.DisaggregatedSetReconciler{
		Client:     fakeClient,
		Scheme:     scheme,
		LWSManager: controller.NewLeaderWorkerSetManager(fakeClient),
		Record:     events.NewFakeRecorder(100),
	}

	_, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: disaggregatedSet.Name, Namespace: disaggregatedSet.Namespace}})
	require.NoError(t, err, "Reconcile should succeed")

	var got disaggregatedsetv1.DisaggregatedSet
	require.NoError(t, fakeClient.Get(ctx, types.NamespacedName{Name: disaggregatedSet.Name, Namespace: disaggregatedSet.Namespace}, &got))

	progressing := meta.FindStatusCondition(got.Status.Conditions, string(disaggregatedsetv1.DisaggregatedSetProgressing))
	require.NotNil(t, progressing, "under-desired-count role should keep the set Progressing")
	assert.Equal(t, metav1.ConditionTrue, progressing.Status)
	assert.Nil(t, meta.FindStatusCondition(got.Status.Conditions, string(disaggregatedsetv1.DisaggregatedSetAvailable)), "Available must not be set while prefill is under its desired count")
}

// TestStatusAvailableWhenPausedAtZero: the documented all-roles-zero pause state
// (XValidation on DisaggregatedSetSpec) should read as Available once fully
// drained, not stuck Progressing forever just because desired is 0.
func TestStatusAvailableWhenPausedAtZero(t *testing.T) {
	ctx := context.Background()
	scheme := wrappers.DisaggregatedSetTestScheme()

	disaggregatedSet := wrappers.BuildDisaggregatedSet("paused", "default").
		WithRole(testControllerRolePrefill, 0, "nginx:1.0").
		WithRole(testControllerRoleDecode, 0, "nginx:1.0").
		Obj()

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(disaggregatedSet).
		WithStatusSubresource(&disaggregatedsetv1.DisaggregatedSet{}, &leaderworkersetv1.LeaderWorkerSet{}).Build()
	reconciler := &controller.DisaggregatedSetReconciler{
		Client:     fakeClient,
		Scheme:     scheme,
		LWSManager: controller.NewLeaderWorkerSetManager(fakeClient),
		Record:     events.NewFakeRecorder(100),
	}

	_, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: disaggregatedSet.Name, Namespace: disaggregatedSet.Namespace}})
	require.NoError(t, err, "Reconcile should succeed")

	var got disaggregatedsetv1.DisaggregatedSet
	require.NoError(t, fakeClient.Get(ctx, types.NamespacedName{Name: disaggregatedSet.Name, Namespace: disaggregatedSet.Namespace}, &got))

	cond := meta.FindStatusCondition(got.Status.Conditions, string(disaggregatedsetv1.DisaggregatedSetAvailable))
	require.NotNil(t, cond, "a fully-drained, all-roles-zero DisaggregatedSet should be Available, not stuck Progressing")
	assert.Equal(t, metav1.ConditionTrue, cond.Status)
}

// TestStatusUsesScalerTargetForExternalRoles: for a role with scaling.mode:
// External, the effective desired count comes from its DisaggregatedSetRoleScaler,
// not the role's inline spec.replicas (which is documented as ignored in that
// mode). Comparing against the ignored inline value would leave the role stuck
// Progressing forever even once it's fully satisfied at its real, scaler-driven
// target (Copilot review on #980).
func TestStatusUsesScalerTargetForExternalRoles(t *testing.T) {
	ctx := context.Background()
	scheme := wrappers.DisaggregatedSetTestScheme()

	disaggregatedSet := wrappers.BuildDisaggregatedSet("external-scaling", "default").
		WithRole(testControllerRolePrefill, 5, "nginx:1.0"). // inline 5 must be ignored: External mode.
		WithRole(testControllerRoleDecode, 2, "nginx:1.0").
		Obj()
	disaggregatedSet.Spec.Roles[0].Scaling = &disaggregatedsetv1.RoleScaling{Mode: disaggregatedsetv1.RoleScalingExternal}

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(disaggregatedSet).
		WithStatusSubresource(&disaggregatedsetv1.DisaggregatedSet{}, &leaderworkersetv1.LeaderWorkerSet{}, &disaggregatedsetv1.DisaggregatedSetRoleScaler{}).Build()
	reconciler := &controller.DisaggregatedSetReconciler{
		Client:     fakeClient,
		Scheme:     scheme,
		LWSManager: controller.NewLeaderWorkerSetManager(fakeClient),
		Record:     events.NewFakeRecorder(100),
	}

	_, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: disaggregatedSet.Name, Namespace: disaggregatedSet.Namespace}})
	require.NoError(t, err, "first reconcile should succeed")

	revision := disaggregatedsetutils.ComputeRevision(disaggregatedSet.Spec.Roles)
	lwsManager := controller.NewLeaderWorkerSetManager(fakeClient)

	prefillLWS, err := lwsManager.Get(ctx, disaggregatedSet, disaggregatedsetutils.GenerateName(disaggregatedSet.Name, 0, revision, testControllerRolePrefill))
	require.NoError(t, err)
	require.NotNil(t, prefillLWS)
	require.EqualValues(t, 1, *prefillLWS.Spec.Replicas, "a fresh External role's LWS should be created at the scaler-seeded target (1), not the ignored inline replicas (5)")

	// Simulate the LWS reporting itself fully ready/updated at that scaler-driven target.
	prefillLWS.Status.Replicas, prefillLWS.Status.ReadyReplicas, prefillLWS.Status.UpdatedReplicas = 1, 1, 1
	require.NoError(t, fakeClient.Status().Update(ctx, prefillLWS))

	decodeLWS, err := lwsManager.Get(ctx, disaggregatedSet, disaggregatedsetutils.GenerateName(disaggregatedSet.Name, 0, revision, testControllerRoleDecode))
	require.NoError(t, err)
	require.NotNil(t, decodeLWS)
	decodeLWS.Status.Replicas, decodeLWS.Status.ReadyReplicas, decodeLWS.Status.UpdatedReplicas = 2, 2, 2
	require.NoError(t, fakeClient.Status().Update(ctx, decodeLWS))

	_, err = reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: disaggregatedSet.Name, Namespace: disaggregatedSet.Namespace}})
	require.NoError(t, err, "second reconcile should succeed")

	var got disaggregatedsetv1.DisaggregatedSet
	require.NoError(t, fakeClient.Get(ctx, types.NamespacedName{Name: disaggregatedSet.Name, Namespace: disaggregatedSet.Namespace}, &got))

	cond := meta.FindStatusCondition(got.Status.Conditions, string(disaggregatedsetv1.DisaggregatedSetAvailable))
	require.NotNil(t, cond, "Available should be set once the External role is ready at its scaler-driven target (1), not stuck Progressing by comparing against the ignored inline replicas (5)")
	assert.Equal(t, metav1.ConditionTrue, cond.Status)
}

// TestStatusProgressingWhenExternalRoleScalerMissing: an External role whose
// generated scaler name collides with a foreign, non-owned
// DisaggregatedSetRoleScaler is left out of the scalers map entirely (the
// ScalerManager declines to adopt it — same class of name-collision issue as
// #981 for LWS). Its target is then genuinely unknown, so status must read
// Progressing rather than falling back to a literal 0 that could spuriously
// match a role that also happens to have 0 actual replicas (Copilot review
// on #980).
func TestStatusProgressingWhenExternalRoleScalerMissing(t *testing.T) {
	ctx := context.Background()
	scheme := wrappers.DisaggregatedSetTestScheme()

	disaggregatedSet := wrappers.BuildDisaggregatedSet("scaler-collision", "default").
		WithRole(testControllerRolePrefill, 1, "nginx:1.0").
		Obj()
	disaggregatedSet.Spec.Roles[0].Scaling = &disaggregatedsetv1.RoleScaling{Mode: disaggregatedsetv1.RoleScalingExternal}

	// A scaler already occupies the name this role would generate, but it's
	// owned by a different DisaggregatedSet UID — ScalerManager won't adopt it.
	foreignScaler := &disaggregatedsetv1.DisaggregatedSetRoleScaler{
		ObjectMeta: metav1.ObjectMeta{
			Name:      controller.ScalerName(disaggregatedSet.Name, testControllerRolePrefill),
			Namespace: disaggregatedSet.Namespace,
			Labels: map[string]string{
				disaggregatedsetv1.SetNameLabelKey: disaggregatedSet.Name,
				disaggregatedsetv1.RoleLabelKey:    testControllerRolePrefill,
			},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: disaggregatedsetv1.GroupVersion.String(),
				Kind:       "DisaggregatedSet",
				Name:       "some-other-ds",
				UID:        "some-other-uid",
				Controller: ptr.To(true),
			}},
		},
	}

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(disaggregatedSet, foreignScaler).
		WithStatusSubresource(&disaggregatedsetv1.DisaggregatedSet{}, &leaderworkersetv1.LeaderWorkerSet{}, &disaggregatedsetv1.DisaggregatedSetRoleScaler{}).Build()
	reconciler := &controller.DisaggregatedSetReconciler{
		Client:     fakeClient,
		Scheme:     scheme,
		LWSManager: controller.NewLeaderWorkerSetManager(fakeClient),
		Record:     events.NewFakeRecorder(100),
	}

	_, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: disaggregatedSet.Name, Namespace: disaggregatedSet.Namespace}})
	require.NoError(t, err, "Reconcile should succeed even though the scaler couldn't be created")

	var got disaggregatedsetv1.DisaggregatedSet
	require.NoError(t, fakeClient.Get(ctx, types.NamespacedName{Name: disaggregatedSet.Name, Namespace: disaggregatedSet.Namespace}, &got))

	progressing := meta.FindStatusCondition(got.Status.Conditions, string(disaggregatedsetv1.DisaggregatedSetProgressing))
	require.NotNil(t, progressing, "a role with an unknown (missing/uncreatable) scaler target must read Progressing")
	assert.Equal(t, metav1.ConditionTrue, progressing.Status)
	assert.Nil(t, meta.FindStatusCondition(got.Status.Conditions, string(disaggregatedsetv1.DisaggregatedSetAvailable)), "must not read Available just because the role also has 0 actual replicas")
}

// TestStatusDropsRemovedRoleEvenWhileItsLWSStillDrains: roleStatuses mirrors the
// current spec.roles contract (RoleStatuses doc, #868 review). Removing a role
// from spec.roles must drop it from status.roleStatuses on the very next reconcile,
// even though its old LWS is not itself deleted by this reconcile (nothing currently
// scales down or removes a removed role's leftover LWS; that lifecycle gap is
// pre-existing and out of scope here) — status must not keep reporting a role the
// user no longer asked for.
func TestStatusDropsRemovedRoleEvenWhileItsLWSStillDrains(t *testing.T) {
	ctx := context.Background()
	scheme := wrappers.DisaggregatedSetTestScheme()

	disaggregatedSet := wrappers.BuildDisaggregatedSet("role-removal", "default").
		WithRole(testControllerRolePrefill, 2, "nginx:1.0").
		WithRole(testControllerRoleDecode, 2, "nginx:1.0").
		WithRole("extra", 2, "nginx:1.0").
		Obj()

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(disaggregatedSet).
		WithStatusSubresource(&disaggregatedsetv1.DisaggregatedSet{}, &leaderworkersetv1.LeaderWorkerSet{}).Build()
	reconciler := &controller.DisaggregatedSetReconciler{
		Client:     fakeClient,
		Scheme:     scheme,
		LWSManager: controller.NewLeaderWorkerSetManager(fakeClient),
		Record:     events.NewFakeRecorder(100),
	}

	_, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: disaggregatedSet.Name, Namespace: disaggregatedSet.Namespace}})
	require.NoError(t, err, "Reconcile should succeed")

	var got disaggregatedsetv1.DisaggregatedSet
	require.NoError(t, fakeClient.Get(ctx, types.NamespacedName{Name: disaggregatedSet.Name, Namespace: disaggregatedSet.Namespace}, &got))
	require.Len(t, got.Status.RoleStatuses, 3, "all three roles should be reported before removal")

	oldRevision := disaggregatedsetutils.ComputeRevision(got.Spec.Roles)
	extraLWSName := disaggregatedsetutils.GenerateName(got.Name, 0, oldRevision, "extra")

	// Remove "extra" from spec.roles, simulating a user edit.
	got.Spec.Roles = got.Spec.Roles[:2]
	require.NoError(t, fakeClient.Update(ctx, &got))

	_, err = reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: disaggregatedSet.Name, Namespace: disaggregatedSet.Namespace}})
	require.NoError(t, err, "Reconcile should succeed")

	require.NoError(t, fakeClient.Get(ctx, types.NamespacedName{Name: disaggregatedSet.Name, Namespace: disaggregatedSet.Namespace}, &got))
	require.Len(t, got.Status.RoleStatuses, 2, "removed role must disappear from roleStatuses")
	for _, rs := range got.Status.RoleStatuses {
		assert.NotEqual(t, "extra", rs.Name, "removed role must not reappear in roleStatuses")
	}

	// The old role's LWS is still there — status dropping it is a status-contract
	// choice, not a side effect of the LWS actually being gone.
	extraLWS, _ := controller.NewLeaderWorkerSetManager(fakeClient).Get(ctx, &got, extraLWSName)
	assert.NotNil(t, extraLWS, "removed role's old LWS is expected to still exist; this test pins the status contract, not cleanup")
}

func TestSlicesIncreaseWithRolloutNotBlocked(t *testing.T) {
	ctx := context.Background()
	scheme := wrappers.DisaggregatedSetTestScheme()

	disaggregatedSet := wrappers.BuildDisaggregatedSet("slice-rollout", "default").
		Slices(2).
		WithRole(testControllerRolePrefill, 2, "nginx:1.0").
		WithRole(testControllerRoleDecode, 2, "nginx:1.0").
		Obj()
	targetRevision := disaggregatedsetutils.ComputeRevision(disaggregatedSet.Spec.Roles)
	oldRevision := "oldrev01"
	require.NotEqual(t, oldRevision, targetRevision)

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		disaggregatedSet,
		createOldLeaderWorkerSet(disaggregatedSet, testControllerRolePrefill, oldRevision, 2),
		createOldLeaderWorkerSet(disaggregatedSet, testControllerRoleDecode, oldRevision, 2),
	).WithStatusSubresource(&disaggregatedsetv1.DisaggregatedSet{}, &leaderworkersetv1.LeaderWorkerSet{}).Build()
	reconciler := &controller.DisaggregatedSetReconciler{
		Client:     fakeClient,
		Scheme:     scheme,
		LWSManager: controller.NewLeaderWorkerSetManager(fakeClient),
		Record:     events.NewFakeRecorder(100),
	}

	_, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: disaggregatedSet.Name, Namespace: disaggregatedSet.Namespace}})
	require.NoError(t, err, "Reconcile should succeed")

	lwsManager := controller.NewLeaderWorkerSetManager(fakeClient)

	// Slice 0 rolls toward the new revision.
	s0, _ := lwsManager.Get(ctx, disaggregatedSet, disaggregatedsetutils.GenerateName(disaggregatedSet.Name, 0, targetRevision, testControllerRolePrefill))
	require.NotNil(t, s0, "slice 0 should start rolling to the new revision")

	// The sibling slice is reconciled independently while slice 0 is rolling.
	s1, _ := lwsManager.Get(ctx, disaggregatedSet, disaggregatedsetutils.GenerateName(disaggregatedSet.Name, 1, targetRevision, testControllerRolePrefill))
	require.NotNil(t, s1, "slice 1 should be created at the new revision without blocking")
}

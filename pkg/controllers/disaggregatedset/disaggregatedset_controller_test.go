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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
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
		}).
		WorkerTemplateSpec(corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "nginx:1.0"}}}).
		Obj()
}

// createLegacyLeaderWorkerSet builds a pre-slices LWS: legacy name and labels with no
// slice label, as produced by a controller that predates the slices feature.
func createLegacyLeaderWorkerSet(disaggregatedSet *disaggregatedsetv1.DisaggregatedSet, role, revision string, replicas, ready int32) *leaderworkersetv1.LeaderWorkerSet {
	labels := map[string]string{
		disaggregatedsetv1.SetNameLabelKey:  disaggregatedSet.Name,
		disaggregatedsetv1.RoleLabelKey:     role,
		disaggregatedsetv1.RevisionLabelKey: revision,
	}

	return wrappers.BuildBasicLeaderWorkerSet(disaggregatedsetutils.GenerateLegacyName(disaggregatedSet.Name, revision, role), disaggregatedSet.Namespace).
		Labels(labels).
		Replica(int(replicas)).
		Size(1).
		StatusReplicas(replicas).
		ReadyReplicas(ready).
		OwnerReference(metav1.OwnerReference{
			APIVersion: disaggregatedsetv1.GroupVersion.String(),
			Kind:       "DisaggregatedSet",
			Name:       disaggregatedSet.Name,
			UID:        disaggregatedSet.UID,
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
		Client:         fakeClient,
		Scheme:         scheme,
		LWSManager:     controller.NewLeaderWorkerSetManager(fakeClient),
		ServiceManager: controller.NewServiceManager(fakeClient, scheme),
		Record:         events.NewFakeRecorder(100),
	}

	_, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: disaggregatedSet.Name, Namespace: disaggregatedSet.Namespace}})
	require.NoError(t, err, "Reconcile should succeed")

	newRevision := disaggregatedsetutils.ComputeRevision(disaggregatedSet.Spec.Roles)
	lwsManager := controller.NewLeaderWorkerSetManager(fakeClient)

	prefillInfo, _ := lwsManager.Get(ctx, disaggregatedSet.Namespace, disaggregatedsetutils.GenerateName(disaggregatedSet.Name, 0, newRevision, testControllerRolePrefill))
	require.NotNil(t, prefillInfo, "prefill LWS should exist")
	assert.Equal(t, 3, int(*prefillInfo.Spec.Replicas), "prefill replicas")

	decodeInfo, _ := lwsManager.Get(ctx, disaggregatedSet.Namespace, disaggregatedsetutils.GenerateName(disaggregatedSet.Name, 0, newRevision, testControllerRoleDecode))
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
		Client:         fakeClient,
		Scheme:         scheme,
		LWSManager:     controller.NewLeaderWorkerSetManager(fakeClient),
		ServiceManager: controller.NewServiceManager(fakeClient, scheme),
		Record:         events.NewFakeRecorder(100),
	}

	_, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: disaggregatedSet.Name, Namespace: disaggregatedSet.Namespace}})
	require.NoError(t, err, "Reconcile should succeed")

	lwsManager := controller.NewLeaderWorkerSetManager(fakeClient)

	prefillInfo, _ := lwsManager.Get(ctx, disaggregatedSet.Namespace, disaggregatedsetutils.GenerateName(disaggregatedSet.Name, 0, revision, testControllerRolePrefill))
	require.NotNil(t, prefillInfo, "prefill LWS should exist")
	assert.Equal(t, 5, int(*prefillInfo.Spec.Replicas), "prefill replicas should be scaled to 5")

	decodeInfo, _ := lwsManager.Get(ctx, disaggregatedSet.Namespace, disaggregatedsetutils.GenerateName(disaggregatedSet.Name, 0, revision, testControllerRoleDecode))
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
		Client:         fakeClient,
		Scheme:         scheme,
		LWSManager:     controller.NewLeaderWorkerSetManager(fakeClient),
		ServiceManager: controller.NewServiceManager(fakeClient, scheme),
		Record:         events.NewFakeRecorder(100),
	}

	_, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: disaggregatedSet.Name, Namespace: disaggregatedSet.Namespace}})
	require.NoError(t, err, "Reconcile should succeed")

	revision := disaggregatedsetutils.ComputeRevision(disaggregatedSet.Spec.Roles)
	lwsManager := controller.NewLeaderWorkerSetManager(fakeClient)

	for slice := range 2 {
		prefill, _ := lwsManager.Get(ctx, disaggregatedSet.Namespace, disaggregatedsetutils.GenerateName(disaggregatedSet.Name, slice, revision, testControllerRolePrefill))
		require.NotNil(t, prefill, "prefill LWS should exist for slice %d", slice)
		assert.Equal(t, 2, int(*prefill.Spec.Replicas), "prefill replicas slice %d", slice)
		assert.Equal(t, strconv.Itoa(slice), prefill.Labels[disaggregatedsetv1.SliceLabelKey], "slice label slice %d", slice)

		decode, _ := lwsManager.Get(ctx, disaggregatedSet.Namespace, disaggregatedsetutils.GenerateName(disaggregatedSet.Name, slice, revision, testControllerRoleDecode))
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
		Client:         fakeClient,
		Scheme:         scheme,
		LWSManager:     controller.NewLeaderWorkerSetManager(fakeClient),
		ServiceManager: controller.NewServiceManager(fakeClient, scheme),
		Record:         events.NewFakeRecorder(100),
	}

	_, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: disaggregatedSet.Name, Namespace: disaggregatedSet.Namespace}})
	require.NoError(t, err, "Reconcile should succeed")

	lwsManager := controller.NewLeaderWorkerSetManager(fakeClient)

	// Slice 0 is kept.
	s0, _ := lwsManager.Get(ctx, disaggregatedSet.Namespace, disaggregatedsetutils.GenerateName(disaggregatedSet.Name, 0, revision, testControllerRolePrefill))
	require.NotNil(t, s0, "slice 0 prefill should be kept")

	// Slice 1 (>= desired) is deleted.
	s1p, _ := lwsManager.Get(ctx, disaggregatedSet.Namespace, disaggregatedsetutils.GenerateName(disaggregatedSet.Name, 1, revision, testControllerRolePrefill))
	assert.Nil(t, s1p, "slice 1 prefill should be deleted")
	s1d, _ := lwsManager.Get(ctx, disaggregatedSet.Namespace, disaggregatedsetutils.GenerateName(disaggregatedSet.Name, 1, revision, testControllerRoleDecode))
	assert.Nil(t, s1d, "slice 1 decode should be deleted")
}

// TestLegacyAdoptedInPlace: a single-slice DisaggregatedSet from a pre-slices release
// (label-less slice-0 LWS at the target revision) is adopted in place, not duplicated.
func TestLegacyAdoptedInPlace(t *testing.T) {
	ctx := context.Background()
	scheme := wrappers.DisaggregatedSetTestScheme()

	disaggregatedSet := wrappers.BuildDisaggregatedSet("legacy-adopt", "default").
		WithRole(testControllerRolePrefill, 2, "nginx:1.0").
		WithRole(testControllerRoleDecode, 2, "nginx:1.0").
		Obj()
	revision := disaggregatedsetutils.ComputeRevision(disaggregatedSet.Spec.Roles)

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		disaggregatedSet,
		createLegacyLeaderWorkerSet(disaggregatedSet, testControllerRolePrefill, revision, 2, 2),
		createLegacyLeaderWorkerSet(disaggregatedSet, testControllerRoleDecode, revision, 2, 2),
	).WithStatusSubresource(&disaggregatedsetv1.DisaggregatedSet{}, &leaderworkersetv1.LeaderWorkerSet{}).Build()
	reconciler := &controller.DisaggregatedSetReconciler{
		Client:         fakeClient,
		Scheme:         scheme,
		LWSManager:     controller.NewLeaderWorkerSetManager(fakeClient),
		ServiceManager: controller.NewServiceManager(fakeClient, scheme),
		Record:         events.NewFakeRecorder(100),
	}

	_, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: disaggregatedSet.Name, Namespace: disaggregatedSet.Namespace}})
	require.NoError(t, err, "Reconcile should succeed")

	lwsManager := controller.NewLeaderWorkerSetManager(fakeClient)

	// Legacy LWS kept under its legacy name.
	legacy, _ := lwsManager.Get(ctx, disaggregatedSet.Namespace, disaggregatedsetutils.GenerateLegacyName(disaggregatedSet.Name, revision, testControllerRolePrefill))
	require.NotNil(t, legacy, "legacy prefill LWS should be adopted in place")

	// No slice-aware duplicate created.
	dup, _ := lwsManager.Get(ctx, disaggregatedSet.Namespace, disaggregatedsetutils.GenerateName(disaggregatedSet.Name, 0, revision, testControllerRolePrefill))
	assert.Nil(t, dup, "no slice-aware duplicate should be created over a legacy LWS")

	var all leaderworkersetv1.LeaderWorkerSetList
	require.NoError(t, fakeClient.List(ctx, &all))
	assert.Len(t, all.Items, 2, "only the two legacy LWS should exist")
}

// TestSlicesIncreaseBlocksUntilLegacyMigrated: increasing slices above 1 over a legacy
// slice-0 deployment starts a same-revision migration of slice 0 and does not create the
// sibling slice until that migration completes.
func TestSlicesIncreaseBlocksUntilLegacyMigrated(t *testing.T) {
	ctx := context.Background()
	scheme := wrappers.DisaggregatedSetTestScheme()

	disaggregatedSet := wrappers.BuildDisaggregatedSet("legacy-grow", "default").
		Slices(2).
		WithRole(testControllerRolePrefill, 2, "nginx:1.0").
		WithRole(testControllerRoleDecode, 2, "nginx:1.0").
		Obj()
	revision := disaggregatedsetutils.ComputeRevision(disaggregatedSet.Spec.Roles)

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		disaggregatedSet,
		createLegacyLeaderWorkerSet(disaggregatedSet, testControllerRolePrefill, revision, 2, 2),
		createLegacyLeaderWorkerSet(disaggregatedSet, testControllerRoleDecode, revision, 2, 2),
	).WithStatusSubresource(&disaggregatedsetv1.DisaggregatedSet{}, &leaderworkersetv1.LeaderWorkerSet{}).Build()
	reconciler := &controller.DisaggregatedSetReconciler{
		Client:         fakeClient,
		Scheme:         scheme,
		LWSManager:     controller.NewLeaderWorkerSetManager(fakeClient),
		ServiceManager: controller.NewServiceManager(fakeClient, scheme),
		Record:         events.NewFakeRecorder(100),
	}

	_, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: disaggregatedSet.Name, Namespace: disaggregatedSet.Namespace}})
	require.NoError(t, err, "Reconcile should succeed")

	lwsManager := controller.NewLeaderWorkerSetManager(fakeClient)

	// Migration started: slice-aware slice-0 LWS exists (at 0 replicas).
	s0, _ := lwsManager.Get(ctx, disaggregatedSet.Namespace, disaggregatedsetutils.GenerateName(disaggregatedSet.Name, 0, revision, testControllerRolePrefill))
	require.NotNil(t, s0, "slice-aware slice-0 prefill should be created for migration")

	// Legacy still present (not yet drained).
	legacy, _ := lwsManager.Get(ctx, disaggregatedSet.Namespace, disaggregatedsetutils.GenerateLegacyName(disaggregatedSet.Name, revision, testControllerRolePrefill))
	require.NotNil(t, legacy, "legacy prefill should still exist during migration")

	// Sibling slice blocked: slice 1 not created.
	s1, _ := lwsManager.Get(ctx, disaggregatedSet.Namespace, disaggregatedsetutils.GenerateName(disaggregatedSet.Name, 1, revision, testControllerRolePrefill))
	assert.Nil(t, s1, "slice 1 must not be created until legacy slice 0 is migrated")
}

// TestSlicesIncreaseCompletesMigrationAndUnblocks: once the slice-aware slice 0 is at the
// target and the legacy LWS has drained, the migration finishes (legacy LWS and its
// slice-agnostic service deleted, slice-aware service created) and the sibling slice is
// created.
func TestSlicesIncreaseCompletesMigrationAndUnblocks(t *testing.T) {
	ctx := context.Background()
	scheme := wrappers.DisaggregatedSetTestScheme()

	disaggregatedSet := wrappers.BuildDisaggregatedSet("legacy-done", "default").
		Slices(2).
		WithRole(testControllerRolePrefill, 2, "nginx:1.0").
		WithRole(testControllerRoleDecode, 2, "nginx:1.0").
		Obj()
	revision := disaggregatedsetutils.ComputeRevision(disaggregatedSet.Spec.Roles)

	legacyPrefillSvc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      disaggregatedsetutils.GenerateLegacyName(disaggregatedSet.Name, revision, testControllerRolePrefill) + "-prv",
			Namespace: disaggregatedSet.Namespace,
			Labels: map[string]string{
				disaggregatedsetv1.SetNameLabelKey:  disaggregatedSet.Name,
				disaggregatedsetv1.RoleLabelKey:     testControllerRolePrefill,
				disaggregatedsetv1.RevisionLabelKey: revision,
			},
		},
		Spec: corev1.ServiceSpec{ClusterIP: corev1.ClusterIPNone},
	}

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		disaggregatedSet,
		// Legacy slice 0 fully drained.
		createLegacyLeaderWorkerSet(disaggregatedSet, testControllerRolePrefill, revision, 0, 0),
		createLegacyLeaderWorkerSet(disaggregatedSet, testControllerRoleDecode, revision, 0, 0),
		// Slice-aware slice 0 already at target and ready.
		createSliceLWS(disaggregatedSet, 0, testControllerRolePrefill, revision),
		createSliceLWS(disaggregatedSet, 0, testControllerRoleDecode, revision),
		legacyPrefillSvc,
	).WithStatusSubresource(&disaggregatedsetv1.DisaggregatedSet{}, &leaderworkersetv1.LeaderWorkerSet{}).Build()
	reconciler := &controller.DisaggregatedSetReconciler{
		Client:         fakeClient,
		Scheme:         scheme,
		LWSManager:     controller.NewLeaderWorkerSetManager(fakeClient),
		ServiceManager: controller.NewServiceManager(fakeClient, scheme),
		Record:         events.NewFakeRecorder(100),
	}

	_, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: disaggregatedSet.Name, Namespace: disaggregatedSet.Namespace}})
	require.NoError(t, err, "Reconcile should succeed")

	lwsManager := controller.NewLeaderWorkerSetManager(fakeClient)

	// Legacy LWS deleted.
	legacy, _ := lwsManager.Get(ctx, disaggregatedSet.Namespace, disaggregatedsetutils.GenerateLegacyName(disaggregatedSet.Name, revision, testControllerRolePrefill))
	assert.Nil(t, legacy, "legacy LWS should be deleted once migration completes")

	// Legacy slice-agnostic service deleted, slice-aware service created.
	err = fakeClient.Get(ctx, types.NamespacedName{Name: legacyPrefillSvc.Name, Namespace: disaggregatedSet.Namespace}, &corev1.Service{})
	assert.Error(t, err, "legacy slice-agnostic service should be deleted")
	err = fakeClient.Get(ctx, types.NamespacedName{Name: controller.GenerateServiceName(disaggregatedSet.Name, 0, revision, testControllerRolePrefill), Namespace: disaggregatedSet.Namespace}, &corev1.Service{})
	require.NoError(t, err, "slice-aware slice-0 service should be created")

	// Sibling slice now created (unblocked).
	s1, _ := lwsManager.Get(ctx, disaggregatedSet.Namespace, disaggregatedsetutils.GenerateName(disaggregatedSet.Name, 1, revision, testControllerRolePrefill))
	require.NotNil(t, s1, "slice 1 should be created once migration completes")
}

// TestSlicesIncreaseWithRolloutNotBlocked: when slices increases at the same time as a
// template change, the legacy slice-0 LWS is at the old revision (not the target), so no
// same-revision migration runs and the sibling slice is created right away at the new
// revision.
func TestSlicesIncreaseWithRolloutNotBlocked(t *testing.T) {
	ctx := context.Background()
	scheme := wrappers.DisaggregatedSetTestScheme()

	disaggregatedSet := wrappers.BuildDisaggregatedSet("legacy-rollout", "default").
		Slices(2).
		WithRole(testControllerRolePrefill, 2, "nginx:1.0").
		WithRole(testControllerRoleDecode, 2, "nginx:1.0").
		Obj()
	targetRevision := disaggregatedsetutils.ComputeRevision(disaggregatedSet.Spec.Roles)
	oldRevision := "oldrev01"
	require.NotEqual(t, oldRevision, targetRevision)

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		disaggregatedSet,
		// Legacy slice 0 at the OLD revision.
		createLegacyLeaderWorkerSet(disaggregatedSet, testControllerRolePrefill, oldRevision, 2, 2),
		createLegacyLeaderWorkerSet(disaggregatedSet, testControllerRoleDecode, oldRevision, 2, 2),
	).WithStatusSubresource(&disaggregatedsetv1.DisaggregatedSet{}, &leaderworkersetv1.LeaderWorkerSet{}).Build()
	reconciler := &controller.DisaggregatedSetReconciler{
		Client:         fakeClient,
		Scheme:         scheme,
		LWSManager:     controller.NewLeaderWorkerSetManager(fakeClient),
		ServiceManager: controller.NewServiceManager(fakeClient, scheme),
		Record:         events.NewFakeRecorder(100),
	}

	_, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: disaggregatedSet.Name, Namespace: disaggregatedSet.Namespace}})
	require.NoError(t, err, "Reconcile should succeed")

	lwsManager := controller.NewLeaderWorkerSetManager(fakeClient)

	// Slice 0 rolls toward the new revision (slice-aware new-revision LWS created).
	s0, _ := lwsManager.Get(ctx, disaggregatedSet.Namespace, disaggregatedsetutils.GenerateName(disaggregatedSet.Name, 0, targetRevision, testControllerRolePrefill))
	require.NotNil(t, s0, "slice 0 should start rolling to the new revision")

	// Sibling slice is NOT blocked: it is created at the new revision.
	s1, _ := lwsManager.Get(ctx, disaggregatedSet.Namespace, disaggregatedsetutils.GenerateName(disaggregatedSet.Name, 1, targetRevision, testControllerRolePrefill))
	require.NotNil(t, s1, "slice 1 should be created at the new revision without blocking")
}

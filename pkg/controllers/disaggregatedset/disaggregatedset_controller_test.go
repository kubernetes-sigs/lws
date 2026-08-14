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

// createLegacyLeaderWorkerSet builds a pre-slices LWS: legacy name and labels with no
// slice label, as produced by a controller that predates the slices feature. It is a
// healthy legacy slice-0 at full replicas.
func createLegacyLeaderWorkerSet(disaggregatedSet *disaggregatedsetv1.DisaggregatedSet, role, revision string) *leaderworkersetv1.LeaderWorkerSet {
	labels := map[string]string{
		disaggregatedsetv1.SetNameLabelKey:  disaggregatedSet.Name,
		disaggregatedsetv1.RoleLabelKey:     role,
		disaggregatedsetv1.RevisionLabelKey: revision,
	}

	return wrappers.BuildBasicLeaderWorkerSet(disaggregatedsetutils.GenerateLegacyName(disaggregatedSet.Name, revision, role), disaggregatedSet.Namespace).
		Labels(labels).
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
		Client:         fakeClient,
		Scheme:         scheme,
		LWSManager:     controller.NewLeaderWorkerSetManager(fakeClient),
		ServiceManager: controller.NewServiceManager(fakeClient, scheme),
		Record:         events.NewFakeRecorder(100),
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
	s0, _ := lwsManager.Get(ctx, disaggregatedSet, disaggregatedsetutils.GenerateName(disaggregatedSet.Name, 0, revision, testControllerRolePrefill))
	require.NotNil(t, s0, "slice 0 prefill should be kept")

	// Slice 1 (>= desired) is deleted.
	s1p, _ := lwsManager.Get(ctx, disaggregatedSet, disaggregatedsetutils.GenerateName(disaggregatedSet.Name, 1, revision, testControllerRolePrefill))
	assert.Nil(t, s1p, "slice 1 prefill should be deleted")
	s1d, _ := lwsManager.Get(ctx, disaggregatedSet, disaggregatedsetutils.GenerateName(disaggregatedSet.Name, 1, revision, testControllerRoleDecode))
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
		createLegacyLeaderWorkerSet(disaggregatedSet, testControllerRolePrefill, revision),
		createLegacyLeaderWorkerSet(disaggregatedSet, testControllerRoleDecode, revision),
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
	legacy, _ := lwsManager.Get(ctx, disaggregatedSet, disaggregatedsetutils.GenerateLegacyName(disaggregatedSet.Name, revision, testControllerRolePrefill))
	require.NotNil(t, legacy, "legacy prefill LWS should be adopted in place")

	// No slice-aware duplicate created.
	dup, _ := lwsManager.Get(ctx, disaggregatedSet, disaggregatedsetutils.GenerateName(disaggregatedSet.Name, 0, revision, testControllerRolePrefill))
	assert.Nil(t, dup, "no slice-aware duplicate should be created over a legacy LWS")

	var all leaderworkersetv1.LeaderWorkerSetList
	require.NoError(t, fakeClient.List(ctx, &all))
	assert.Len(t, all.Items, 2, "only the two legacy LWS should exist")
}

// TestLegacyMigratesToSliceAwareOnRollout: a pod-template change to a legacy (pre-slices,
// label-less) slice-0 DisaggregatedSet rolls it to the slice-aware form at the new revision.
func TestLegacyMigratesToSliceAwareOnRollout(t *testing.T) {
	ctx := context.Background()
	scheme := wrappers.DisaggregatedSetTestScheme()

	disaggregatedSet := wrappers.BuildDisaggregatedSet("legacy-migrate", "default").
		WithRole(testControllerRolePrefill, 2, "nginx:2.0").
		WithRole(testControllerRoleDecode, 2, "nginx:2.0").
		Obj()
	newRevision := disaggregatedsetutils.ComputeRevision(disaggregatedSet.Spec.Roles)
	// The legacy objects were created by a pre-slices release at an earlier revision.
	const oldRevision = "old12345"

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		disaggregatedSet,
		createLegacyLeaderWorkerSet(disaggregatedSet, testControllerRolePrefill, oldRevision),
		createLegacyLeaderWorkerSet(disaggregatedSet, testControllerRoleDecode, oldRevision),
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

	// The rollout creates the new revision in slice-aware form: the name carries the
	// -0- slice segment and the object carries the slice label.
	migrated, _ := lwsManager.Get(ctx, disaggregatedSet, disaggregatedsetutils.GenerateName(disaggregatedSet.Name, 0, newRevision, testControllerRolePrefill))
	require.NotNil(t, migrated, "slice-aware prefill LWS at the new revision should be created")
	assert.Equal(t, "0", migrated.Labels[disaggregatedsetv1.SliceLabelKey], "migrated LWS should carry the slice label")

	// The legacy (label-less) object keeps its old name while it drains.
	legacy, _ := lwsManager.Get(ctx, disaggregatedSet, disaggregatedsetutils.GenerateLegacyName(disaggregatedSet.Name, oldRevision, testControllerRolePrefill))
	assert.NotNil(t, legacy, "legacy prefill LWS should still exist while draining")
}

// TestSlicesIncreaseBlocksUntilLegacyMigrated: increasing slices above 1 over a legacy
// slice-0 deployment starts a same-revision migration of slice 0 and does not create the
// sibling slice until that migration completes.
// TestSlicesIncreaseRecreatesLegacySlice0: increasing slices above 1 over a pre-slices
// (label-less) slice-0 deletes the legacy LWS and its slice-agnostic service, and the
// slice loop then recreates slice 0 in slice-aware form alongside the new sibling, with no
// blocking.
func TestSlicesIncreaseRecreatesLegacySlice0(t *testing.T) {
	ctx := context.Background()
	scheme := wrappers.DisaggregatedSetTestScheme()

	disaggregatedSet := wrappers.BuildDisaggregatedSet("legacy-grow", "default").
		Slices(2).
		WithRole(testControllerRolePrefill, 2, "nginx:1.0").
		WithRole(testControllerRoleDecode, 2, "nginx:1.0").
		Obj()
	revision := disaggregatedsetutils.ComputeRevision(disaggregatedSet.Spec.Roles)

	// The legacy slice-agnostic service that would otherwise select the new sibling's pods.
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
		createLegacyLeaderWorkerSet(disaggregatedSet, testControllerRolePrefill, revision),
		createLegacyLeaderWorkerSet(disaggregatedSet, testControllerRoleDecode, revision),
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

	// Legacy slice-0 LWS deleted for both roles.
	legacyP, _ := lwsManager.Get(ctx, disaggregatedSet, disaggregatedsetutils.GenerateLegacyName(disaggregatedSet.Name, revision, testControllerRolePrefill))
	assert.Nil(t, legacyP, "legacy slice-0 prefill LWS should be deleted")
	legacyD, _ := lwsManager.Get(ctx, disaggregatedSet, disaggregatedsetutils.GenerateLegacyName(disaggregatedSet.Name, revision, testControllerRoleDecode))
	assert.Nil(t, legacyD, "legacy slice-0 decode LWS should be deleted")

	// Legacy slice-agnostic service deleted (before any sibling could be selected).
	err = fakeClient.Get(ctx, types.NamespacedName{Name: legacyPrefillSvc.Name, Namespace: disaggregatedSet.Namespace}, &corev1.Service{})
	assert.Error(t, err, "legacy slice-agnostic service should be deleted")

	// Slice 0 recreated slice-aware, and sibling slice 1 created in the same pass.
	s0, _ := lwsManager.Get(ctx, disaggregatedSet, disaggregatedsetutils.GenerateName(disaggregatedSet.Name, 0, revision, testControllerRolePrefill))
	require.NotNil(t, s0, "slice-aware slice-0 prefill should be recreated")
	s1, _ := lwsManager.Get(ctx, disaggregatedSet, disaggregatedsetutils.GenerateName(disaggregatedSet.Name, 1, revision, testControllerRolePrefill))
	require.NotNil(t, s1, "sibling slice 1 prefill should be created (no blocking)")
}

// TestSlicesIncreaseWithRolloutNotBlocked: when slices increases at the same time as a
// template change, the legacy slice-0 LWS is at the old revision (not the target), so no
// same-revision migration runs and the sibling slice is created right away at the new
// revision.
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
		Client:         fakeClient,
		Scheme:         scheme,
		LWSManager:     controller.NewLeaderWorkerSetManager(fakeClient),
		ServiceManager: controller.NewServiceManager(fakeClient, scheme),
		Record:         events.NewFakeRecorder(100),
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
		Client:         fakeClient,
		Scheme:         scheme,
		LWSManager:     controller.NewLeaderWorkerSetManager(fakeClient),
		ServiceManager: controller.NewServiceManager(fakeClient, scheme),
		Record:         events.NewFakeRecorder(100),
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
		Client:         fakeClient,
		Scheme:         scheme,
		LWSManager:     controller.NewLeaderWorkerSetManager(fakeClient),
		ServiceManager: controller.NewServiceManager(fakeClient, scheme),
		Record:         events.NewFakeRecorder(100),
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
		Client:         fakeClient,
		Scheme:         scheme,
		LWSManager:     controller.NewLeaderWorkerSetManager(fakeClient),
		ServiceManager: controller.NewServiceManager(fakeClient, scheme),
		Record:         events.NewFakeRecorder(100),
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
		Client:         fakeClient,
		Scheme:         scheme,
		LWSManager:     controller.NewLeaderWorkerSetManager(fakeClient),
		ServiceManager: controller.NewServiceManager(fakeClient, scheme),
		Record:         events.NewFakeRecorder(100),
	}

	_, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: disaggregatedSet.Name, Namespace: disaggregatedSet.Namespace}})
	require.NoError(t, err, "first reconcile should succeed")

	revision := disaggregatedsetutils.ComputeRevision(disaggregatedSet.Spec.Roles)
	lwsManager := controller.NewLeaderWorkerSetManager(fakeClient)

	prefillLWS, err := lwsManager.Get(ctx, disaggregatedSet.Namespace, disaggregatedsetutils.GenerateName(disaggregatedSet.Name, 0, revision, testControllerRolePrefill))
	require.NoError(t, err)
	require.NotNil(t, prefillLWS)
	require.EqualValues(t, 1, *prefillLWS.Spec.Replicas, "a fresh External role's LWS should be created at the scaler-seeded target (1), not the ignored inline replicas (5)")

	// Simulate the LWS reporting itself fully ready/updated at that scaler-driven target.
	prefillLWS.Status.Replicas, prefillLWS.Status.ReadyReplicas, prefillLWS.Status.UpdatedReplicas = 1, 1, 1
	require.NoError(t, fakeClient.Status().Update(ctx, prefillLWS))

	decodeLWS, err := lwsManager.Get(ctx, disaggregatedSet.Namespace, disaggregatedsetutils.GenerateName(disaggregatedSet.Name, 0, revision, testControllerRoleDecode))
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
		Client:         fakeClient,
		Scheme:         scheme,
		LWSManager:     controller.NewLeaderWorkerSetManager(fakeClient),
		ServiceManager: controller.NewServiceManager(fakeClient, scheme),
		Record:         events.NewFakeRecorder(100),
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
		Client:         fakeClient,
		Scheme:         scheme,
		LWSManager:     controller.NewLeaderWorkerSetManager(fakeClient),
		ServiceManager: controller.NewServiceManager(fakeClient, scheme),
		Record:         events.NewFakeRecorder(100),
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
		createLegacyLeaderWorkerSet(disaggregatedSet, testControllerRolePrefill, oldRevision),
		createLegacyLeaderWorkerSet(disaggregatedSet, testControllerRoleDecode, oldRevision),
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
	s0, _ := lwsManager.Get(ctx, disaggregatedSet, disaggregatedsetutils.GenerateName(disaggregatedSet.Name, 0, targetRevision, testControllerRolePrefill))
	require.NotNil(t, s0, "slice 0 should start rolling to the new revision")

	// Sibling slice is NOT blocked: it is created at the new revision.
	s1, _ := lwsManager.Get(ctx, disaggregatedSet, disaggregatedsetutils.GenerateName(disaggregatedSet.Name, 1, targetRevision, testControllerRolePrefill))
	require.NotNil(t, s1, "slice 1 should be created at the new revision without blocking")
}

// TestSlicesIncreaseIgnoresForeignOwnedLegacySlice0 is a regression test for
// #981: recreateLegacySlice0 must not delete/migrate a legacy-named LWS that
// exists but is owned by a different DisaggregatedSet (e.g. left over from a
// same-named DisaggregatedSet that was deleted and recreated before GC ran).
// The foreign object is left untouched, and the normal create path still
// proceeds for this DisaggregatedSet's own slice-aware LWS at both slices —
// increasing slices must not get stuck just because the legacy name is
// occupied by something else.
func TestSlicesIncreaseIgnoresForeignOwnedLegacySlice0(t *testing.T) {
	ctx := context.Background()
	scheme := wrappers.DisaggregatedSetTestScheme()

	disaggregatedSet := wrappers.BuildDisaggregatedSet("legacy-foreign", "default").
		Slices(2).
		WithRole(testControllerRolePrefill, 2, "nginx:1.0").
		WithRole(testControllerRoleDecode, 2, "nginx:1.0").
		Obj()
	revision := disaggregatedsetutils.ComputeRevision(disaggregatedSet.Spec.Roles)

	foreignDS := wrappers.BuildDisaggregatedSet("some-other-ds", "default").Obj()
	foreignOwnerRef := metav1.OwnerReference{
		APIVersion: disaggregatedsetv1.GroupVersion.String(),
		Kind:       "DisaggregatedSet",
		Name:       foreignDS.Name,
		UID:        foreignDS.UID,
		Controller: ptr.To(true),
	}
	foreignLegacyPrefill := wrappers.BuildBasicLeaderWorkerSet(
		disaggregatedsetutils.GenerateLegacyName(disaggregatedSet.Name, revision, testControllerRolePrefill), "default").
		Labels(map[string]string{
			disaggregatedsetv1.SetNameLabelKey:  disaggregatedSet.Name,
			disaggregatedsetv1.RoleLabelKey:     testControllerRolePrefill,
			disaggregatedsetv1.RevisionLabelKey: revision,
		}).
		Replica(2).
		Size(1).
		OwnerReference(foreignOwnerRef).
		WorkerTemplateSpec(corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "nginx:1.0"}}}).
		Obj()

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		disaggregatedSet,
		foreignLegacyPrefill,
	).WithStatusSubresource(&disaggregatedsetv1.DisaggregatedSet{}, &leaderworkersetv1.LeaderWorkerSet{}).Build()
	reconciler := &controller.DisaggregatedSetReconciler{
		Client:         fakeClient,
		Scheme:         scheme,
		LWSManager:     controller.NewLeaderWorkerSetManager(fakeClient),
		ServiceManager: controller.NewServiceManager(fakeClient, scheme),
		Record:         events.NewFakeRecorder(100),
	}

	_, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: disaggregatedSet.Name, Namespace: disaggregatedSet.Namespace}})
	require.NoError(t, err, "Reconcile should succeed even though the legacy name is occupied by a foreign LWS")

	lwsManager := controller.NewLeaderWorkerSetManager(fakeClient)

	// The foreign object at the legacy name must survive untouched. Get is
	// ownership-filtered, so fetch it as its actual owner (foreignDS) rather
	// than as disaggregatedSet, which would now correctly see it as absent.
	foreignAfter, err := lwsManager.Get(ctx, foreignDS, foreignLegacyPrefill.Name)
	require.NoError(t, err)
	require.NotNil(t, foreignAfter, "foreign-owned legacy LWS must not be deleted")
	require.Len(t, foreignAfter.OwnerReferences, 1)
	assert.Equal(t, foreignDS.UID, foreignAfter.OwnerReferences[0].UID, "foreign LWS ownership must be unchanged")

	// This DisaggregatedSet's own slice-aware LWS are still created normally at
	// both slices — the foreign object at the legacy name did not block anything.
	s0, _ := lwsManager.Get(ctx, disaggregatedSet, disaggregatedsetutils.GenerateName(disaggregatedSet.Name, 0, revision, testControllerRolePrefill))
	assert.NotNil(t, s0, "slice-aware slice-0 prefill should still be created")
	s1, _ := lwsManager.Get(ctx, disaggregatedSet, disaggregatedsetutils.GenerateName(disaggregatedSet.Name, 1, revision, testControllerRolePrefill))
	assert.NotNil(t, s1, "sibling slice 1 prefill should still be created")
}

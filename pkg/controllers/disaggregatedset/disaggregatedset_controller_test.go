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

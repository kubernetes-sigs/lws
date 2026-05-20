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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	leaderworkerset "sigs.k8s.io/lws/api/leaderworkerset/v1"

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
func createOldLeaderWorkerSet(disaggregatedSet *disaggregatedsetv1.DisaggregatedSet, role, revision string, replicas int32) *leaderworkerset.LeaderWorkerSet {
	labels := map[string]string{
		disaggregatedsetv1.SetNameLabelKey:  disaggregatedSet.Name,
		disaggregatedsetv1.RoleLabelKey:     role,
		disaggregatedsetv1.RevisionLabelKey: revision,
	}

	return wrappers.BuildDisaggregatedSetLWS(disaggregatedSet.Name+"-"+revision+"-"+role, disaggregatedSet.Namespace, role, revision).
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
		WithStatusSubresource(&disaggregatedsetv1.DisaggregatedSet{}, &leaderworkerset.LeaderWorkerSet{}).Build()
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

	prefillInfo, _ := lwsManager.Get(ctx, disaggregatedSet.Namespace, disaggregatedsetutils.GenerateName(disaggregatedSet.Name, testControllerRolePrefill, newRevision))
	require.NotNil(t, prefillInfo, "prefill LWS should exist")
	assert.Equal(t, 3, prefillInfo.Replicas, "prefill replicas")

	decodeInfo, _ := lwsManager.Get(ctx, disaggregatedSet.Namespace, disaggregatedsetutils.GenerateName(disaggregatedSet.Name, testControllerRoleDecode, newRevision))
	require.NotNil(t, decodeInfo, "decode LWS should exist")
	assert.Equal(t, 2, decodeInfo.Replicas, "decode replicas")
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
		WithStatusSubresource(&disaggregatedsetv1.DisaggregatedSet{}, &leaderworkerset.LeaderWorkerSet{}).Build()
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

	prefillInfo, _ := lwsManager.Get(ctx, disaggregatedSet.Namespace, disaggregatedsetutils.GenerateName(disaggregatedSet.Name, testControllerRolePrefill, revision))
	require.NotNil(t, prefillInfo, "prefill LWS should exist")
	assert.Equal(t, 5, prefillInfo.Replicas, "prefill replicas should be scaled to 5")

	decodeInfo, _ := lwsManager.Get(ctx, disaggregatedSet.Namespace, disaggregatedsetutils.GenerateName(disaggregatedSet.Name, testControllerRoleDecode, revision))
	require.NotNil(t, decodeInfo, "decode LWS should exist")
	assert.Equal(t, 4, decodeInfo.Replicas, "decode replicas should be scaled to 4")
}

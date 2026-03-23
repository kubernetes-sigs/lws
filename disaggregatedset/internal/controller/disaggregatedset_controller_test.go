/*
Copyright 2026.

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

package controller_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	leaderworkerset "sigs.k8s.io/lws/api/leaderworkerset/v1"

	"k8s.io/utils/ptr"

	disaggv1alpha1 "sigs.k8s.io/disaggregatedset/api/v1alpha1"
	controller "sigs.k8s.io/disaggregatedset/internal/controller"
)

// Test-local role names
const (
	testControllerRolePrefill = "prefill"
	testControllerRoleDecode  = "decode"
)

func testScheme() *runtime.Scheme {
	scheme := runtime.NewScheme()
	_ = disaggv1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)
	_ = leaderworkerset.AddToScheme(scheme)
	return scheme
}

// createOldLeaderWorkerSet creates a LeaderWorkerSet representing an existing workload with the given revision.
// Useful for simulating pre-existing workloads in rolling update tests.
func createOldLeaderWorkerSet(disaggregatedSet *disaggv1alpha1.DisaggregatedSet, role, revision string, replicas int32) *leaderworkerset.LeaderWorkerSet {
	labels := map[string]string{
		controller.LabelDisaggName: disaggregatedSet.Name,
		controller.LabelDisaggRole: role,
		controller.LabelRevision:   revision,
	}

	return &leaderworkerset.LeaderWorkerSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      disaggregatedSet.Name + "-" + revision + "-" + role,
			Namespace: disaggregatedSet.Namespace,
			Labels:    labels,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: disaggv1alpha1.GroupVersion.String(),
				Kind:       "DisaggregatedSet",
				Name:       disaggregatedSet.Name,
				UID:        disaggregatedSet.UID,
			}},
		},
		Spec: leaderworkerset.LeaderWorkerSetSpec{
			Replicas: ptr.To(replicas),
			LeaderWorkerTemplate: leaderworkerset.LeaderWorkerTemplate{
				Size: ptr.To(int32(1)),
				WorkerTemplate: corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{Labels: labels},
					Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "nginx:1.0"}}},
				},
			},
		},
		Status: leaderworkerset.LeaderWorkerSetStatus{
			Replicas:      replicas,
			ReadyReplicas: replicas, // Simulate ready state
		},
	}
}

func newTestDisaggregatedSet(name, namespace string, prefillReplicas, decodeReplicas int32, image string) *disaggv1alpha1.DisaggregatedSet {
	return &disaggv1alpha1.DisaggregatedSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			UID:       types.UID(name + "-uid"),
		},
		Spec: disaggv1alpha1.DisaggregatedSetSpec{
			Roles: []disaggv1alpha1.DisaggregatedRoleSpec{
				{
					Name: testControllerRolePrefill,
					LeaderWorkerSetSpec: leaderworkerset.LeaderWorkerSetSpec{
						Replicas: ptr.To(prefillReplicas),
						LeaderWorkerTemplate: leaderworkerset.LeaderWorkerTemplate{
							Size:           ptr.To(int32(1)),
							WorkerTemplate: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: image}}}},
						},
					},
				},
				{
					Name: testControllerRoleDecode,
					LeaderWorkerSetSpec: leaderworkerset.LeaderWorkerSetSpec{
						Replicas: ptr.To(decodeReplicas),
						LeaderWorkerTemplate: leaderworkerset.LeaderWorkerTemplate{
							Size:           ptr.To(int32(1)),
							WorkerTemplate: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: image}}}},
						},
					},
				},
			},
		},
	}
}

func TestFreshDeploymentNoRollingUpdate(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme()

	disaggregatedSet := newTestDisaggregatedSet("fresh-deploy", "default", 3, 2, "nginx:1.0")

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(disaggregatedSet).
		WithStatusSubresource(&disaggv1alpha1.DisaggregatedSet{}, &leaderworkerset.LeaderWorkerSet{}).Build()
	reconciler := &controller.DisaggregatedSetReconciler{
		Client:          fakeClient,
		Scheme:          scheme,
		WorkloadManager: controller.NewLeaderWorkerSetManager(fakeClient),
		ServiceManager:  controller.NewServiceManager(fakeClient, scheme),
		Recorder:        record.NewFakeRecorder(100),
	}

	_, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: disaggregatedSet.Name, Namespace: disaggregatedSet.Namespace}})
	require.NoError(t, err, "Reconcile should succeed")

	newRevision := controller.ComputeRevision(disaggregatedSet.Spec.Roles)
	workloadManager := controller.NewLeaderWorkerSetManager(fakeClient)

	prefillInfo, _ := workloadManager.Get(ctx, disaggregatedSet.Namespace, controller.GenerateName(disaggregatedSet.Name, testControllerRolePrefill, newRevision))
	require.NotNil(t, prefillInfo, "prefill workload should exist")
	assert.Equal(t, 3, prefillInfo.Replicas, "prefill replicas")

	decodeInfo, _ := workloadManager.Get(ctx, disaggregatedSet.Namespace, controller.GenerateName(disaggregatedSet.Name, testControllerRoleDecode, newRevision))
	require.NotNil(t, decodeInfo, "decode workload should exist")
	assert.Equal(t, 2, decodeInfo.Replicas, "decode replicas")
}

func TestScalingWithoutRollingUpdate(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme()

	disaggregatedSet := newTestDisaggregatedSet("scale-test", "default", 5, 4, "nginx:1.0")
	revision := controller.ComputeRevision(disaggregatedSet.Spec.Roles)

	prefillRS := createOldLeaderWorkerSet(disaggregatedSet, testControllerRolePrefill, revision, 3)
	decodeRS := createOldLeaderWorkerSet(disaggregatedSet, testControllerRoleDecode, revision, 2)

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(disaggregatedSet, prefillRS, decodeRS).
		WithStatusSubresource(&disaggv1alpha1.DisaggregatedSet{}, &leaderworkerset.LeaderWorkerSet{}).Build()
	reconciler := &controller.DisaggregatedSetReconciler{
		Client:          fakeClient,
		Scheme:          scheme,
		WorkloadManager: controller.NewLeaderWorkerSetManager(fakeClient),
		ServiceManager:  controller.NewServiceManager(fakeClient, scheme),
		Recorder:        record.NewFakeRecorder(100),
	}

	_, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: disaggregatedSet.Name, Namespace: disaggregatedSet.Namespace}})
	require.NoError(t, err, "Reconcile should succeed")

	workloadManager := controller.NewLeaderWorkerSetManager(fakeClient)

	prefillInfo, _ := workloadManager.Get(ctx, disaggregatedSet.Namespace, controller.GenerateName(disaggregatedSet.Name, testControllerRolePrefill, revision))
	require.NotNil(t, prefillInfo, "prefill workload should exist")
	assert.Equal(t, 5, prefillInfo.Replicas, "prefill replicas should be scaled to 5")

	decodeInfo, _ := workloadManager.Get(ctx, disaggregatedSet.Namespace, controller.GenerateName(disaggregatedSet.Name, testControllerRoleDecode, revision))
	require.NotNil(t, decodeInfo, "decode workload should exist")
	assert.Equal(t, 4, decodeInfo.Replicas, "decode replicas should be scaled to 4")
}

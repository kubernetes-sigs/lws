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

package controller

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	leaderworkerset "sigs.k8s.io/lws/api/leaderworkerset/v1"

	disaggv1alpha1 "sigs.k8s.io/disaggregatedset/api/v1alpha1"
)

// Test-local role names
const (
	testServiceRolePrefill = "prefill"
	testServiceRoleDecode  = "decode"
)

func TestServiceManager(t *testing.T) {
	ctx := context.Background()
	scheme := testSchemeForUnit()

	t.Run("no service created when only one role is ready", func(t *testing.T) {
		deployment := createTestDeployment("test-deploy", "default")

		fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(deployment).Build()
		serviceManager := NewServiceManager(fakeClient, scheme)

		// Only prefill is ready
		groupedWorkloads := GroupedWorkloads{
			{
				Revision: "abc12345",
				Roles: map[string]WorkloadInfo{
					testServiceRolePrefill: {Name: "test-abc12345-prefill", ReadyReplicas: 2},
					testServiceRoleDecode:  {Name: "test-abc12345-decode", ReadyReplicas: 0}, // not ready
				},
			},
		}

		err := serviceManager.ReconcileServices(ctx, deployment, groupedWorkloads, "abc12345")
		require.NoError(t, err)

		// Verify no services created
		serviceList := &corev1.ServiceList{}
		err = fakeClient.List(ctx, serviceList)
		require.NoError(t, err)
		assert.Empty(t, serviceList.Items, "no services should be created when only one role is ready")
	})

	t.Run("services created when both roles have >= 1 ready replica", func(t *testing.T) {
		deployment := createTestDeployment("test-deploy", "default")

		fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(deployment).Build()
		serviceManager := NewServiceManager(fakeClient, scheme)

		// Both roles ready
		groupedWorkloads := GroupedWorkloads{
			{
				Revision: "abc12345",
				Roles: map[string]WorkloadInfo{
					testServiceRolePrefill: {Name: "test-abc12345-prefill", ReadyReplicas: 1},
					testServiceRoleDecode:  {Name: "test-abc12345-decode", ReadyReplicas: 1},
				},
			},
		}

		err := serviceManager.ReconcileServices(ctx, deployment, groupedWorkloads, "abc12345")
		require.NoError(t, err)

		// Verify services created for both roles
		prefillService := &corev1.Service{}
		err = fakeClient.Get(ctx, types.NamespacedName{
			Name:      GenerateServiceName(deployment.Name, testServiceRolePrefill, "abc12345"),
			Namespace: deployment.Namespace,
		}, prefillService)
		require.NoError(t, err, "prefill service should exist")

		decodeService := &corev1.Service{}
		err = fakeClient.Get(ctx, types.NamespacedName{
			Name:      GenerateServiceName(deployment.Name, testServiceRoleDecode, "abc12345"),
			Namespace: deployment.Namespace,
		}, decodeService)
		require.NoError(t, err, "decode service should exist")
	})

	t.Run("service is headless with clusterIP None", func(t *testing.T) {
		deployment := createTestDeployment("test-deploy", "default")

		fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(deployment).Build()
		serviceManager := NewServiceManager(fakeClient, scheme)

		groupedWorkloads := GroupedWorkloads{
			{
				Revision: "abc12345",
				Roles: map[string]WorkloadInfo{
					testServiceRolePrefill: {Name: "test-abc12345-prefill", ReadyReplicas: 1},
					testServiceRoleDecode:  {Name: "test-abc12345-decode", ReadyReplicas: 1},
				},
			},
		}

		err := serviceManager.ReconcileServices(ctx, deployment, groupedWorkloads, "abc12345")
		require.NoError(t, err)

		prefillService := &corev1.Service{}
		err = fakeClient.Get(ctx, types.NamespacedName{
			Name:      GenerateServiceName(deployment.Name, testServiceRolePrefill, "abc12345"),
			Namespace: deployment.Namespace,
		}, prefillService)
		require.NoError(t, err)

		// Verify headless service
		assert.Equal(t, corev1.ClusterIPNone, prefillService.Spec.ClusterIP, "service should be headless (clusterIP: None)")
	})

	t.Run("service is portless with no ports defined", func(t *testing.T) {
		deployment := createTestDeployment("test-deploy", "default")

		fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(deployment).Build()
		serviceManager := NewServiceManager(fakeClient, scheme)

		groupedWorkloads := GroupedWorkloads{
			{
				Revision: "abc12345",
				Roles: map[string]WorkloadInfo{
					testServiceRolePrefill: {Name: "test-abc12345-prefill", ReadyReplicas: 1},
					testServiceRoleDecode:  {Name: "test-abc12345-decode", ReadyReplicas: 1},
				},
			},
		}

		err := serviceManager.ReconcileServices(ctx, deployment, groupedWorkloads, "abc12345")
		require.NoError(t, err)

		decodeService := &corev1.Service{}
		err = fakeClient.Get(ctx, types.NamespacedName{
			Name:      GenerateServiceName(deployment.Name, testServiceRoleDecode, "abc12345"),
			Namespace: deployment.Namespace,
		}, decodeService)
		require.NoError(t, err)

		// Verify portless service
		assert.Empty(t, decodeService.Spec.Ports, "service should be portless (no ports)")
	})

	t.Run("service name uses prv prefix", func(t *testing.T) {
		deployment := createTestDeployment("my-app", "default")

		fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(deployment).Build()
		serviceManager := NewServiceManager(fakeClient, scheme)

		groupedWorkloads := GroupedWorkloads{
			{
				Revision: "ef53f2d7",
				Roles: map[string]WorkloadInfo{
					testServiceRolePrefill: {Name: "my-app-ef53f2d7-prefill", ReadyReplicas: 1},
					testServiceRoleDecode:  {Name: "my-app-ef53f2d7-decode", ReadyReplicas: 1},
				},
			},
		}

		err := serviceManager.ReconcileServices(ctx, deployment, groupedWorkloads, "ef53f2d7")
		require.NoError(t, err)

		// Check expected service names with prv suffix
		expectedPrefillName := "my-app-ef53f2d7-prefill-prv"
		expectedDecodeName := "my-app-ef53f2d7-decode-prv"

		prefillService := &corev1.Service{}
		err = fakeClient.Get(ctx, types.NamespacedName{Name: expectedPrefillName, Namespace: "default"}, prefillService)
		require.NoError(t, err, "service should have correct name: %s", expectedPrefillName)

		decodeService := &corev1.Service{}
		err = fakeClient.Get(ctx, types.NamespacedName{Name: expectedDecodeName, Namespace: "default"}, decodeService)
		require.NoError(t, err, "service should have correct name: %s", expectedDecodeName)
	})

	t.Run("standard labels are applied", func(t *testing.T) {
		deployment := createTestDeployment("test-deploy", "default")

		fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(deployment).Build()
		serviceManager := NewServiceManager(fakeClient, scheme)

		groupedWorkloads := GroupedWorkloads{
			{
				Revision: "abc12345",
				Roles: map[string]WorkloadInfo{
					testServiceRolePrefill: {Name: "test-abc12345-prefill", ReadyReplicas: 1},
					testServiceRoleDecode:  {Name: "test-abc12345-decode", ReadyReplicas: 1},
				},
			},
		}

		err := serviceManager.ReconcileServices(ctx, deployment, groupedWorkloads, "abc12345")
		require.NoError(t, err)

		decodeService := &corev1.Service{}
		err = fakeClient.Get(ctx, types.NamespacedName{
			Name:      GenerateServiceName(deployment.Name, testServiceRoleDecode, "abc12345"),
			Namespace: deployment.Namespace,
		}, decodeService)
		require.NoError(t, err)

		// Verify standard labels are present
		assert.Equal(t, "test-deploy", decodeService.Labels[LabelDisaggName], "name label should be set")
		assert.Equal(t, "abc12345", decodeService.Labels[LabelRevision], "revision label should be set")
		assert.Equal(t, testServiceRoleDecode, decodeService.Labels[LabelDisaggRole], "role label should be set")
	})

	t.Run("selector matches pod labels for role and revision", func(t *testing.T) {
		deployment := createTestDeployment("test-deploy", "default")

		fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(deployment).Build()
		serviceManager := NewServiceManager(fakeClient, scheme)

		groupedWorkloads := GroupedWorkloads{
			{
				Revision: "abc12345",
				Roles: map[string]WorkloadInfo{
					testServiceRolePrefill: {Name: "test-abc12345-prefill", ReadyReplicas: 1},
					testServiceRoleDecode:  {Name: "test-abc12345-decode", ReadyReplicas: 1},
				},
			},
		}

		err := serviceManager.ReconcileServices(ctx, deployment, groupedWorkloads, "abc12345")
		require.NoError(t, err)

		decodeService := &corev1.Service{}
		err = fakeClient.Get(ctx, types.NamespacedName{
			Name:      GenerateServiceName(deployment.Name, testServiceRoleDecode, "abc12345"),
			Namespace: deployment.Namespace,
		}, decodeService)
		require.NoError(t, err)

		// Verify selector matches expected pod labels
		assert.Equal(t, "test-deploy", decodeService.Spec.Selector[LabelDisaggName])
		assert.Equal(t, "abc12345", decodeService.Spec.Selector[LabelRevision])
		assert.Equal(t, testServiceRoleDecode, decodeService.Spec.Selector[LabelDisaggRole])
	})

	t.Run("old services deleted when revision is drained", func(t *testing.T) {
		deployment := createTestDeployment("test-deploy", "default")

		// Create an old service
		oldService := &corev1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Name:      GenerateServiceName(deployment.Name, testServiceRoleDecode, "old12345"),
				Namespace: deployment.Namespace,
				Labels: map[string]string{
					LabelDisaggName: deployment.Name,
					LabelDisaggRole: testServiceRoleDecode,
					LabelRevision:   "old12345",
				},
			},
			Spec: corev1.ServiceSpec{
				ClusterIP: corev1.ClusterIPNone,
			},
		}

		fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(deployment, oldService).Build()
		serviceManager := NewServiceManager(fakeClient, scheme)

		// New revision is ready, old is drained
		groupedWorkloads := GroupedWorkloads{
			{
				Revision: "new12345",
				Roles: map[string]WorkloadInfo{
					testServiceRolePrefill: {Name: "test-new12345-prefill", ReadyReplicas: 1},
					testServiceRoleDecode:  {Name: "test-new12345-decode", ReadyReplicas: 1},
				},
			},
		}

		err := serviceManager.ReconcileServices(ctx, deployment, groupedWorkloads, "new12345")
		require.NoError(t, err)

		// Verify old service is deleted
		oldServiceCheck := &corev1.Service{}
		err = fakeClient.Get(ctx, types.NamespacedName{
			Name:      GenerateServiceName(deployment.Name, testServiceRoleDecode, "old12345"),
			Namespace: deployment.Namespace,
		}, oldServiceCheck)
		assert.Error(t, err, "old service should be deleted")

		// Verify new service exists
		newService := &corev1.Service{}
		err = fakeClient.Get(ctx, types.NamespacedName{
			Name:      GenerateServiceName(deployment.Name, testServiceRoleDecode, "new12345"),
			Namespace: deployment.Namespace,
		}, newService)
		require.NoError(t, err, "new service should exist")
	})

	t.Run("no flip-flop when multiple revisions are ready during rolling update", func(t *testing.T) {
		deployment := createTestDeployment("test-deploy", "default")

		// Create services for old revision (simulating existing state)
		oldPrefillService := &corev1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Name:      GenerateServiceName(deployment.Name, testServiceRolePrefill, "old12345"),
				Namespace: deployment.Namespace,
				Labels: map[string]string{
					LabelDisaggName: deployment.Name,
					LabelDisaggRole: testServiceRolePrefill,
					LabelRevision:   "old12345",
				},
			},
			Spec: corev1.ServiceSpec{
				ClusterIP: corev1.ClusterIPNone,
			},
		}
		oldDecodeService := &corev1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Name:      GenerateServiceName(deployment.Name, testServiceRoleDecode, "old12345"),
				Namespace: deployment.Namespace,
				Labels: map[string]string{
					LabelDisaggName: deployment.Name,
					LabelDisaggRole: testServiceRoleDecode,
					LabelRevision:   "old12345",
				},
			},
			Spec: corev1.ServiceSpec{
				ClusterIP: corev1.ClusterIPNone,
			},
		}

		fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(deployment, oldPrefillService, oldDecodeService).Build()
		serviceManager := NewServiceManager(fakeClient, scheme)

		// Both old and new revisions are ready (rolling update in progress)
		groupedWorkloads := GroupedWorkloads{
			{
				Revision: "old12345",
				Roles: map[string]WorkloadInfo{
					testServiceRolePrefill: {Name: "test-old12345-prefill", ReadyReplicas: 2},
					testServiceRoleDecode:  {Name: "test-old12345-decode", ReadyReplicas: 2},
				},
			},
			{
				Revision: "new12345",
				Roles: map[string]WorkloadInfo{
					testServiceRolePrefill: {Name: "test-new12345-prefill", ReadyReplicas: 1},
					testServiceRoleDecode:  {Name: "test-new12345-decode", ReadyReplicas: 1},
				},
			},
		}

		// Target revision is the new one
		targetRevision := "new12345"

		// First reconcile - new services created, old services kept (both still ready)
		err := serviceManager.ReconcileServices(ctx, deployment, groupedWorkloads, targetRevision)
		require.NoError(t, err)

		// Verify new services are created
		newPrefillService := &corev1.Service{}
		err = fakeClient.Get(ctx, types.NamespacedName{
			Name:      GenerateServiceName(deployment.Name, testServiceRolePrefill, "new12345"),
			Namespace: deployment.Namespace,
		}, newPrefillService)
		require.NoError(t, err, "new prefill service should exist")

		// Old services should STILL exist (both revisions are ready during rolling update)
		err = fakeClient.Get(ctx, types.NamespacedName{
			Name:      GenerateServiceName(deployment.Name, testServiceRolePrefill, "old12345"),
			Namespace: deployment.Namespace,
		}, &corev1.Service{})
		require.NoError(t, err, "old prefill service should still exist during rolling update")

		// Now simulate old revision being fully drained
		drainedWorkloads := GroupedWorkloads{
			{
				Revision: "old12345",
				Roles: map[string]WorkloadInfo{
					testServiceRolePrefill: {Name: "test-old12345-prefill", ReadyReplicas: 0},
					testServiceRoleDecode:  {Name: "test-old12345-decode", ReadyReplicas: 0},
				},
			},
			{
				Revision: "new12345",
				Roles: map[string]WorkloadInfo{
					testServiceRolePrefill: {Name: "test-new12345-prefill", ReadyReplicas: 2},
					testServiceRoleDecode:  {Name: "test-new12345-decode", ReadyReplicas: 2},
				},
			},
		}

		// Reconcile after drain - old services should be deleted
		err = serviceManager.ReconcileServices(ctx, deployment, drainedWorkloads, targetRevision)
		require.NoError(t, err)

		// Old services should now be deleted
		err = fakeClient.Get(ctx, types.NamespacedName{
			Name:      GenerateServiceName(deployment.Name, testServiceRolePrefill, "old12345"),
			Namespace: deployment.Namespace,
		}, &corev1.Service{})
		assert.Error(t, err, "old prefill service should be deleted after drain")

		err = fakeClient.Get(ctx, types.NamespacedName{
			Name:      GenerateServiceName(deployment.Name, testServiceRoleDecode, "old12345"),
			Namespace: deployment.Namespace,
		}, &corev1.Service{})
		assert.Error(t, err, "old decode service should be deleted after drain")
	})
}

func TestGenerateServiceName(t *testing.T) {
	tests := []struct {
		name     string
		baseName string
		role     string
		revision string
		expected string
	}{
		{
			name:     "prefill service",
			baseName: "my-app",
			role:     testServiceRolePrefill,
			revision: "abc12345",
			expected: "my-app-abc12345-prefill-prv",
		},
		{
			name:     "decode service",
			baseName: "my-app",
			role:     testServiceRoleDecode,
			revision: "def67890",
			expected: "my-app-def67890-decode-prv",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := GenerateServiceName(tt.baseName, tt.role, tt.revision)
			assert.Equal(t, tt.expected, result)
		})
	}
}

// createTestDeployment creates a test deployment without ServiceTemplate
func createTestDeployment(name, namespace string) *disaggv1alpha1.DisaggregatedSet {
	return &disaggv1alpha1.DisaggregatedSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			UID:       "test-uid",
		},
		Spec: disaggv1alpha1.DisaggregatedSetSpec{
			Roles: []disaggv1alpha1.DisaggregatedRoleSpec{
				{
					Name: testServiceRolePrefill,
					LeaderWorkerSetSpec: leaderworkerset.LeaderWorkerSetSpec{
						LeaderWorkerTemplate: leaderworkerset.LeaderWorkerTemplate{
							WorkerTemplate: corev1.PodTemplateSpec{
								Spec: corev1.PodSpec{
									Containers: []corev1.Container{{Name: "app", Image: "nginx:1.0"}},
								},
							},
						},
					},
				},
				{
					Name: testServiceRoleDecode,
					LeaderWorkerSetSpec: leaderworkerset.LeaderWorkerSetSpec{
						LeaderWorkerTemplate: leaderworkerset.LeaderWorkerTemplate{
							WorkerTemplate: corev1.PodTemplateSpec{
								Spec: corev1.PodSpec{
									Containers: []corev1.Container{{Name: "app", Image: "nginx:1.0"}},
								},
							},
						},
					},
				},
			},
		},
	}
}

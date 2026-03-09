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
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	leaderworkerset "sigs.k8s.io/lws/api/leaderworkerset/v1"

	disaggv1alpha1 "sigs.k8s.io/disaggregatedset/api/v1alpha1"
)

func TestServiceManager(t *testing.T) {
	ctx := context.Background()
	scheme := testSchemeForUnit()

	t.Run("no service created when ServiceTemplate is nil", func(t *testing.T) {
		deployment := &disaggv1alpha1.DisaggregatedSet{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-deploy",
				Namespace: "default",
				UID:       "test-uid",
			},
			Spec: disaggv1alpha1.DisaggregatedSetSpec{
				Prefill: &disaggv1alpha1.DisaggSideConfig{
					LeaderWorkerTemplate: leaderworkerset.LeaderWorkerTemplate{
						WorkerTemplate: corev1.PodTemplateSpec{
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{{Name: "app", Image: "nginx:1.0"}},
							},
						},
					},
					// ServiceTemplate is nil
				},
				Decode: &disaggv1alpha1.DisaggSideConfig{
					LeaderWorkerTemplate: leaderworkerset.LeaderWorkerTemplate{
						WorkerTemplate: corev1.PodTemplateSpec{
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{{Name: "app", Image: "nginx:1.0"}},
							},
						},
					},
					// ServiceTemplate is nil
				},
			},
		}

		fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(deployment).Build()
		serviceManager := NewServiceManager(fakeClient, scheme)

		// Both sides ready
		groupedWorkloads := GroupedWorkloads{
			{
				Revision: "abc12345",
				Sides: map[string]WorkloadInfo{
					SidePrefill: {Name: "test-abc12345-prefill", ReadyReplicas: 2},
					SideDecode:  {Name: "test-abc12345-decode", ReadyReplicas: 2},
				},
			},
		}

		err := serviceManager.ReconcileServices(ctx, deployment, groupedWorkloads, "abc12345")
		require.NoError(t, err)

		// Verify no services created
		serviceList := &corev1.ServiceList{}
		err = fakeClient.List(ctx, serviceList)
		require.NoError(t, err)
		assert.Empty(t, serviceList.Items, "no services should be created when ServiceTemplate is nil")
	})

	t.Run("no service created when only one side is ready", func(t *testing.T) {
		deployment := createDeploymentWithServiceTemplate("test-deploy", "default")

		fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(deployment).Build()
		serviceManager := NewServiceManager(fakeClient, scheme)

		// Only prefill is ready
		groupedWorkloads := GroupedWorkloads{
			{
				Revision: "abc12345",
				Sides: map[string]WorkloadInfo{
					SidePrefill: {Name: "test-abc12345-prefill", ReadyReplicas: 2},
					SideDecode:  {Name: "test-abc12345-decode", ReadyReplicas: 0}, // not ready
				},
			},
		}

		err := serviceManager.ReconcileServices(ctx, deployment, groupedWorkloads, "abc12345")
		require.NoError(t, err)

		// Verify no services created
		serviceList := &corev1.ServiceList{}
		err = fakeClient.List(ctx, serviceList)
		require.NoError(t, err)
		assert.Empty(t, serviceList.Items, "no services should be created when only one side is ready")
	})

	t.Run("services created when both sides have >= 1 ready replica", func(t *testing.T) {
		deployment := createDeploymentWithServiceTemplate("test-deploy", "default")

		fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(deployment).Build()
		serviceManager := NewServiceManager(fakeClient, scheme)

		// Both sides ready
		groupedWorkloads := GroupedWorkloads{
			{
				Revision: "abc12345",
				Sides: map[string]WorkloadInfo{
					SidePrefill: {Name: "test-abc12345-prefill", ReadyReplicas: 1},
					SideDecode:  {Name: "test-abc12345-decode", ReadyReplicas: 1},
				},
			},
		}

		err := serviceManager.ReconcileServices(ctx, deployment, groupedWorkloads, "abc12345")
		require.NoError(t, err)

		// Verify services created
		prefillService := &corev1.Service{}
		err = fakeClient.Get(ctx, types.NamespacedName{
			Name:      GenerateServiceName(deployment.Name, SidePrefill, "abc12345"),
			Namespace: deployment.Namespace,
		}, prefillService)
		require.NoError(t, err, "prefill service should exist")

		decodeService := &corev1.Service{}
		err = fakeClient.Get(ctx, types.NamespacedName{
			Name:      GenerateServiceName(deployment.Name, SideDecode, "abc12345"),
			Namespace: deployment.Namespace,
		}, decodeService)
		require.NoError(t, err, "decode service should exist")
	})

	t.Run("service has correct name format", func(t *testing.T) {
		deployment := createDeploymentWithServiceTemplate("my-app", "default")

		fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(deployment).Build()
		serviceManager := NewServiceManager(fakeClient, scheme)

		groupedWorkloads := GroupedWorkloads{
			{
				Revision: "ef53f2d7",
				Sides: map[string]WorkloadInfo{
					SidePrefill: {Name: "my-app-ef53f2d7-prefill", ReadyReplicas: 1},
					SideDecode:  {Name: "my-app-ef53f2d7-decode", ReadyReplicas: 1},
				},
			},
		}

		err := serviceManager.ReconcileServices(ctx, deployment, groupedWorkloads, "ef53f2d7")
		require.NoError(t, err)

		// Check expected service names
		expectedPrefillName := "my-app-ef53f2d7-prefill-svc"
		expectedDecodeName := "my-app-ef53f2d7-decode-svc"

		prefillService := &corev1.Service{}
		err = fakeClient.Get(ctx, types.NamespacedName{Name: expectedPrefillName, Namespace: "default"}, prefillService)
		require.NoError(t, err, "service should have correct name: %s", expectedPrefillName)

		decodeService := &corev1.Service{}
		err = fakeClient.Get(ctx, types.NamespacedName{Name: expectedDecodeName, Namespace: "default"}, decodeService)
		require.NoError(t, err, "service should have correct name: %s", expectedDecodeName)
	})

	t.Run("auto-populated labels are present and cannot be overridden", func(t *testing.T) {
		deployment := createDeploymentWithServiceTemplate("test-deploy", "default")
		// Add user labels that try to override auto-populated ones
		deployment.Spec.Decode.ServiceTemplate.Labels = map[string]string{
			"custom-label":  "custom-value",
			LabelDisaggName: "should-be-overridden",
			LabelRevision:   "should-be-overridden",
		}

		fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(deployment).Build()
		serviceManager := NewServiceManager(fakeClient, scheme)

		groupedWorkloads := GroupedWorkloads{
			{
				Revision: "abc12345",
				Sides: map[string]WorkloadInfo{
					SidePrefill: {Name: "test-abc12345-prefill", ReadyReplicas: 1},
					SideDecode:  {Name: "test-abc12345-decode", ReadyReplicas: 1},
				},
			},
		}

		err := serviceManager.ReconcileServices(ctx, deployment, groupedWorkloads, "abc12345")
		require.NoError(t, err)

		decodeService := &corev1.Service{}
		err = fakeClient.Get(ctx, types.NamespacedName{
			Name:      GenerateServiceName(deployment.Name, SideDecode, "abc12345"),
			Namespace: deployment.Namespace,
		}, decodeService)
		require.NoError(t, err)

		// Verify auto-populated labels take precedence
		assert.Equal(t, "test-deploy", decodeService.Labels[LabelDisaggName], "auto-populated label should override user label")
		assert.Equal(t, "abc12345", decodeService.Labels[LabelRevision], "auto-populated label should override user label")
		assert.Equal(t, SideDecode, decodeService.Labels[LabelDisaggSide], "auto-populated label should be present")
		// Verify user label is still present
		assert.Equal(t, "custom-value", decodeService.Labels["custom-label"], "user labels should be preserved")
	})

	t.Run("selector auto-populated by default", func(t *testing.T) {
		deployment := createDeploymentWithServiceTemplate("test-deploy", "default")

		fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(deployment).Build()
		serviceManager := NewServiceManager(fakeClient, scheme)

		groupedWorkloads := GroupedWorkloads{
			{
				Revision: "abc12345",
				Sides: map[string]WorkloadInfo{
					SidePrefill: {Name: "test-abc12345-prefill", ReadyReplicas: 1},
					SideDecode:  {Name: "test-abc12345-decode", ReadyReplicas: 1},
				},
			},
		}

		err := serviceManager.ReconcileServices(ctx, deployment, groupedWorkloads, "abc12345")
		require.NoError(t, err)

		decodeService := &corev1.Service{}
		err = fakeClient.Get(ctx, types.NamespacedName{
			Name:      GenerateServiceName(deployment.Name, SideDecode, "abc12345"),
			Namespace: deployment.Namespace,
		}, decodeService)
		require.NoError(t, err)

		// Verify selector is auto-populated
		assert.Equal(t, "test-deploy", decodeService.Spec.Selector[LabelDisaggName])
		assert.Equal(t, "abc12345", decodeService.Spec.Selector[LabelRevision])
		assert.Equal(t, SideDecode, decodeService.Spec.Selector[LabelDisaggSide])
	})

	t.Run("selector not auto-populated when AutoPopulateSelector is false", func(t *testing.T) {
		deployment := createDeploymentWithServiceTemplate("test-deploy", "default")
		autoPopulate := false
		deployment.Spec.Decode.ServiceTemplate.AutoPopulateSelector = &autoPopulate
		deployment.Spec.Decode.ServiceTemplate.Spec.Selector = map[string]string{
			"custom-selector": "custom-value",
		}

		fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(deployment).Build()
		serviceManager := NewServiceManager(fakeClient, scheme)

		groupedWorkloads := GroupedWorkloads{
			{
				Revision: "abc12345",
				Sides: map[string]WorkloadInfo{
					SidePrefill: {Name: "test-abc12345-prefill", ReadyReplicas: 1},
					SideDecode:  {Name: "test-abc12345-decode", ReadyReplicas: 1},
				},
			},
		}

		err := serviceManager.ReconcileServices(ctx, deployment, groupedWorkloads, "abc12345")
		require.NoError(t, err)

		decodeService := &corev1.Service{}
		err = fakeClient.Get(ctx, types.NamespacedName{
			Name:      GenerateServiceName(deployment.Name, SideDecode, "abc12345"),
			Namespace: deployment.Namespace,
		}, decodeService)
		require.NoError(t, err)

		// Verify selector is NOT auto-populated, uses user-provided
		assert.Equal(t, "custom-value", decodeService.Spec.Selector["custom-selector"])
		assert.Empty(t, decodeService.Spec.Selector[LabelDisaggName], "auto-populated selector should not be present")
	})

	t.Run("old services deleted when new revision becomes ready", func(t *testing.T) {
		deployment := createDeploymentWithServiceTemplate("test-deploy", "default")

		// Create an old service
		oldService := &corev1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Name:      GenerateServiceName(deployment.Name, SideDecode, "old12345"),
				Namespace: deployment.Namespace,
				Labels: map[string]string{
					LabelDisaggName: deployment.Name,
					LabelDisaggSide: SideDecode,
					LabelRevision:   "old12345",
				},
			},
			Spec: corev1.ServiceSpec{
				Ports: []corev1.ServicePort{{Port: 80}},
			},
		}

		fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(deployment, oldService).Build()
		serviceManager := NewServiceManager(fakeClient, scheme)

		// New revision is ready
		groupedWorkloads := GroupedWorkloads{
			{
				Revision: "new12345",
				Sides: map[string]WorkloadInfo{
					SidePrefill: {Name: "test-new12345-prefill", ReadyReplicas: 1},
					SideDecode:  {Name: "test-new12345-decode", ReadyReplicas: 1},
				},
			},
		}

		err := serviceManager.ReconcileServices(ctx, deployment, groupedWorkloads, "new12345")
		require.NoError(t, err)

		// Verify old service is deleted
		oldServiceCheck := &corev1.Service{}
		err = fakeClient.Get(ctx, types.NamespacedName{
			Name:      GenerateServiceName(deployment.Name, SideDecode, "old12345"),
			Namespace: deployment.Namespace,
		}, oldServiceCheck)
		assert.Error(t, err, "old service should be deleted")

		// Verify new service exists
		newService := &corev1.Service{}
		err = fakeClient.Get(ctx, types.NamespacedName{
			Name:      GenerateServiceName(deployment.Name, SideDecode, "new12345"),
			Namespace: deployment.Namespace,
		}, newService)
		require.NoError(t, err, "new service should exist")
	})

	t.Run("no flip-flop when multiple revisions are ready during rolling update", func(t *testing.T) {
		deployment := createDeploymentWithServiceTemplate("test-deploy", "default")

		// Create services for old revision (simulating existing state)
		oldPrefillService := &corev1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Name:      GenerateServiceName(deployment.Name, SidePrefill, "old12345"),
				Namespace: deployment.Namespace,
				Labels: map[string]string{
					LabelDisaggName: deployment.Name,
					LabelDisaggSide: SidePrefill,
					LabelRevision:   "old12345",
				},
			},
			Spec: corev1.ServiceSpec{
				Ports: []corev1.ServicePort{{Port: 80}},
			},
		}
		oldDecodeService := &corev1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Name:      GenerateServiceName(deployment.Name, SideDecode, "old12345"),
				Namespace: deployment.Namespace,
				Labels: map[string]string{
					LabelDisaggName: deployment.Name,
					LabelDisaggSide: SideDecode,
					LabelRevision:   "old12345",
				},
			},
			Spec: corev1.ServiceSpec{
				Ports: []corev1.ServicePort{{Port: 80}},
			},
		}

		fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(deployment, oldPrefillService, oldDecodeService).Build()
		serviceManager := NewServiceManager(fakeClient, scheme)

		// Both old and new revisions are ready (rolling update in progress)
		groupedWorkloads := GroupedWorkloads{
			{
				Revision: "old12345",
				Sides: map[string]WorkloadInfo{
					SidePrefill: {Name: "test-old12345-prefill", ReadyReplicas: 2},
					SideDecode:  {Name: "test-old12345-decode", ReadyReplicas: 2},
				},
			},
			{
				Revision: "new12345",
				Sides: map[string]WorkloadInfo{
					SidePrefill: {Name: "test-new12345-prefill", ReadyReplicas: 1},
					SideDecode:  {Name: "test-new12345-decode", ReadyReplicas: 1},
				},
			},
		}

		// Target revision is the new one (what the spec says we're rolling to)
		targetRevision := "new12345"

		// First reconcile - new services created, but OLD services kept (both still ready)
		err := serviceManager.ReconcileServices(ctx, deployment, groupedWorkloads, targetRevision)
		require.NoError(t, err)

		// Verify new services are created
		newPrefillService := &corev1.Service{}
		err = fakeClient.Get(ctx, types.NamespacedName{
			Name:      GenerateServiceName(deployment.Name, SidePrefill, "new12345"),
			Namespace: deployment.Namespace,
		}, newPrefillService)
		require.NoError(t, err, "new prefill service should exist")

		newDecodeService := &corev1.Service{}
		err = fakeClient.Get(ctx, types.NamespacedName{
			Name:      GenerateServiceName(deployment.Name, SideDecode, "new12345"),
			Namespace: deployment.Namespace,
		}, newDecodeService)
		require.NoError(t, err, "new decode service should exist")

		// Old services should STILL exist (both hashes are ready during rolling update)
		err = fakeClient.Get(ctx, types.NamespacedName{
			Name:      GenerateServiceName(deployment.Name, SidePrefill, "old12345"),
			Namespace: deployment.Namespace,
		}, &corev1.Service{})
		require.NoError(t, err, "old prefill service should still exist during rolling update")

		err = fakeClient.Get(ctx, types.NamespacedName{
			Name:      GenerateServiceName(deployment.Name, SideDecode, "old12345"),
			Namespace: deployment.Namespace,
		}, &corev1.Service{})
		require.NoError(t, err, "old decode service should still exist during rolling update")

		// Second reconcile - should be idempotent, no flip-flop
		err = serviceManager.ReconcileServices(ctx, deployment, groupedWorkloads, targetRevision)
		require.NoError(t, err)

		// Verify new services still exist (not deleted and recreated)
		err = fakeClient.Get(ctx, types.NamespacedName{
			Name:      GenerateServiceName(deployment.Name, SidePrefill, "new12345"),
			Namespace: deployment.Namespace,
		}, newPrefillService)
		require.NoError(t, err, "new prefill service should still exist after second reconcile")

		err = fakeClient.Get(ctx, types.NamespacedName{
			Name:      GenerateServiceName(deployment.Name, SideDecode, "new12345"),
			Namespace: deployment.Namespace,
		}, newDecodeService)
		require.NoError(t, err, "new decode service should still exist after second reconcile")

		// Old services should STILL exist (both hashes are still ready)
		err = fakeClient.Get(ctx, types.NamespacedName{
			Name:      GenerateServiceName(deployment.Name, SidePrefill, "old12345"),
			Namespace: deployment.Namespace,
		}, &corev1.Service{})
		require.NoError(t, err, "old prefill service should still exist after second reconcile")

		// Now simulate old revision being fully drained (0 ready replicas)
		drainedWorkloads := GroupedWorkloads{
			{
				Revision: "old12345",
				Sides: map[string]WorkloadInfo{
					SidePrefill: {Name: "test-old12345-prefill", ReadyReplicas: 0},
					SideDecode:  {Name: "test-old12345-decode", ReadyReplicas: 0},
				},
			},
			{
				Revision: "new12345",
				Sides: map[string]WorkloadInfo{
					SidePrefill: {Name: "test-new12345-prefill", ReadyReplicas: 2},
					SideDecode:  {Name: "test-new12345-decode", ReadyReplicas: 2},
				},
			},
		}

		// Third reconcile - old revision is drained, services should be deleted
		err = serviceManager.ReconcileServices(ctx, deployment, drainedWorkloads, targetRevision)
		require.NoError(t, err)

		// Old services should now be deleted (old revision is drained)
		err = fakeClient.Get(ctx, types.NamespacedName{
			Name:      GenerateServiceName(deployment.Name, SidePrefill, "old12345"),
			Namespace: deployment.Namespace,
		}, &corev1.Service{})
		assert.Error(t, err, "old prefill service should be deleted after old hash is drained")

		err = fakeClient.Get(ctx, types.NamespacedName{
			Name:      GenerateServiceName(deployment.Name, SideDecode, "old12345"),
			Namespace: deployment.Namespace,
		}, &corev1.Service{})
		assert.Error(t, err, "old decode service should be deleted after old hash is drained")
	})
}

// createDeploymentWithServiceTemplate creates a test deployment with ServiceTemplate configured
func createDeploymentWithServiceTemplate(name, namespace string) *disaggv1alpha1.DisaggregatedSet {
	return &disaggv1alpha1.DisaggregatedSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			UID:       "test-uid",
		},
		Spec: disaggv1alpha1.DisaggregatedSetSpec{
			Prefill: &disaggv1alpha1.DisaggSideConfig{
				LeaderWorkerTemplate: leaderworkerset.LeaderWorkerTemplate{
					WorkerTemplate: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{{Name: "app", Image: "nginx:1.0"}},
						},
					},
				},
				ServiceTemplate: &disaggv1alpha1.ServiceTemplate{
					Spec: corev1.ServiceSpec{
						Type: corev1.ServiceTypeClusterIP,
						Ports: []corev1.ServicePort{{
							Port:       80,
							TargetPort: intstr.FromInt(8080),
							Protocol:   corev1.ProtocolTCP,
						}},
					},
				},
			},
			Decode: &disaggv1alpha1.DisaggSideConfig{
				LeaderWorkerTemplate: leaderworkerset.LeaderWorkerTemplate{
					WorkerTemplate: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{{Name: "app", Image: "nginx:1.0"}},
						},
					},
				},
				ServiceTemplate: &disaggv1alpha1.ServiceTemplate{
					Spec: corev1.ServiceSpec{
						Type: corev1.ServiceTypeClusterIP,
						Ports: []corev1.ServicePort{{
							Port:       80,
							TargetPort: intstr.FromInt(8080),
							Protocol:   corev1.ProtocolTCP,
						}},
					},
				},
			},
		},
	}
}

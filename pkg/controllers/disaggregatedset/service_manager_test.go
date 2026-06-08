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
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	leaderworkersetv1 "sigs.k8s.io/lws/api/leaderworkerset/v1"

	disaggregatedsetv1 "sigs.k8s.io/lws/api/disaggregatedset/v1"
	disaggregatedsetutils "sigs.k8s.io/lws/pkg/utils/disaggregatedset"
	"sigs.k8s.io/lws/test/wrappers"
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
		deployment := wrappers.BuildDisaggregatedSet("test-deploy", "default").UID("test-uid").WithRoleNoReplicas(testServiceRolePrefill, "nginx:1.0").WithRoleNoReplicas(testServiceRoleDecode, "nginx:1.0").Obj()

		fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(deployment).Build()
		serviceManager := NewServiceManager(fakeClient, scheme)

		// Only prefill is ready
		revisionRoles := disaggregatedsetutils.RevisionRolesList{
			{
				Revision: "abc12345",
				Roles: map[string]*leaderworkersetv1.LeaderWorkerSet{
					testServiceRolePrefill: &leaderworkersetv1.LeaderWorkerSet{ObjectMeta: metav1.ObjectMeta{Name: "test-abc12345-prefill"}, Status: leaderworkersetv1.LeaderWorkerSetStatus{ReadyReplicas: 2}},
					testServiceRoleDecode:  &leaderworkersetv1.LeaderWorkerSet{ObjectMeta: metav1.ObjectMeta{Name: "test-abc12345-decode"}, Status: leaderworkersetv1.LeaderWorkerSetStatus{ReadyReplicas: 0}}, // not ready
				},
			},
		}

		err := serviceManager.ReconcileServices(ctx, deployment, 0, revisionRoles, "abc12345")
		require.NoError(t, err)

		// Verify no services created
		serviceList := &corev1.ServiceList{}
		err = fakeClient.List(ctx, serviceList)
		require.NoError(t, err)
		assert.Empty(t, serviceList.Items, "no services should be created when only one role is ready")
	})

	t.Run("services created when both roles have >= 1 ready replica", func(t *testing.T) {
		deployment := wrappers.BuildDisaggregatedSet("test-deploy", "default").UID("test-uid").WithRoleNoReplicas(testServiceRolePrefill, "nginx:1.0").WithRoleNoReplicas(testServiceRoleDecode, "nginx:1.0").Obj()

		fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(deployment).Build()
		serviceManager := NewServiceManager(fakeClient, scheme)

		// Both roles ready
		revisionRoles := disaggregatedsetutils.RevisionRolesList{
			{
				Revision: "abc12345",
				Roles: map[string]*leaderworkersetv1.LeaderWorkerSet{
					testServiceRolePrefill: &leaderworkersetv1.LeaderWorkerSet{ObjectMeta: metav1.ObjectMeta{Name: "test-abc12345-prefill"}, Status: leaderworkersetv1.LeaderWorkerSetStatus{ReadyReplicas: 1}},
					testServiceRoleDecode:  &leaderworkersetv1.LeaderWorkerSet{ObjectMeta: metav1.ObjectMeta{Name: "test-abc12345-decode"}, Status: leaderworkersetv1.LeaderWorkerSetStatus{ReadyReplicas: 1}},
				},
			},
		}

		err := serviceManager.ReconcileServices(ctx, deployment, 0, revisionRoles, "abc12345")
		require.NoError(t, err)

		// Verify services created for both roles
		prefillService := &corev1.Service{}
		err = fakeClient.Get(ctx, types.NamespacedName{
			Name:      GenerateServiceName(deployment.Name, 0, "abc12345", testServiceRolePrefill),
			Namespace: deployment.Namespace,
		}, prefillService)
		require.NoError(t, err, "prefill service should exist")

		decodeService := &corev1.Service{}
		err = fakeClient.Get(ctx, types.NamespacedName{
			Name:      GenerateServiceName(deployment.Name, 0, "abc12345", testServiceRoleDecode),
			Namespace: deployment.Namespace,
		}, decodeService)
		require.NoError(t, err, "decode service should exist")
	})

	t.Run("service is headless with clusterIP None", func(t *testing.T) {
		deployment := wrappers.BuildDisaggregatedSet("test-deploy", "default").UID("test-uid").WithRoleNoReplicas(testServiceRolePrefill, "nginx:1.0").WithRoleNoReplicas(testServiceRoleDecode, "nginx:1.0").Obj()

		fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(deployment).Build()
		serviceManager := NewServiceManager(fakeClient, scheme)

		revisionRoles := disaggregatedsetutils.RevisionRolesList{
			{
				Revision: "abc12345",
				Roles: map[string]*leaderworkersetv1.LeaderWorkerSet{
					testServiceRolePrefill: &leaderworkersetv1.LeaderWorkerSet{ObjectMeta: metav1.ObjectMeta{Name: "test-abc12345-prefill"}, Status: leaderworkersetv1.LeaderWorkerSetStatus{ReadyReplicas: 1}},
					testServiceRoleDecode:  &leaderworkersetv1.LeaderWorkerSet{ObjectMeta: metav1.ObjectMeta{Name: "test-abc12345-decode"}, Status: leaderworkersetv1.LeaderWorkerSetStatus{ReadyReplicas: 1}},
				},
			},
		}

		err := serviceManager.ReconcileServices(ctx, deployment, 0, revisionRoles, "abc12345")
		require.NoError(t, err)

		prefillService := &corev1.Service{}
		err = fakeClient.Get(ctx, types.NamespacedName{
			Name:      GenerateServiceName(deployment.Name, 0, "abc12345", testServiceRolePrefill),
			Namespace: deployment.Namespace,
		}, prefillService)
		require.NoError(t, err)

		// Verify headless service
		assert.Equal(t, corev1.ClusterIPNone, prefillService.Spec.ClusterIP, "service should be headless (clusterIP: None)")
	})

	t.Run("service is portless with no ports defined", func(t *testing.T) {
		deployment := wrappers.BuildDisaggregatedSet("test-deploy", "default").UID("test-uid").WithRoleNoReplicas(testServiceRolePrefill, "nginx:1.0").WithRoleNoReplicas(testServiceRoleDecode, "nginx:1.0").Obj()

		fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(deployment).Build()
		serviceManager := NewServiceManager(fakeClient, scheme)

		revisionRoles := disaggregatedsetutils.RevisionRolesList{
			{
				Revision: "abc12345",
				Roles: map[string]*leaderworkersetv1.LeaderWorkerSet{
					testServiceRolePrefill: &leaderworkersetv1.LeaderWorkerSet{ObjectMeta: metav1.ObjectMeta{Name: "test-abc12345-prefill"}, Status: leaderworkersetv1.LeaderWorkerSetStatus{ReadyReplicas: 1}},
					testServiceRoleDecode:  &leaderworkersetv1.LeaderWorkerSet{ObjectMeta: metav1.ObjectMeta{Name: "test-abc12345-decode"}, Status: leaderworkersetv1.LeaderWorkerSetStatus{ReadyReplicas: 1}},
				},
			},
		}

		err := serviceManager.ReconcileServices(ctx, deployment, 0, revisionRoles, "abc12345")
		require.NoError(t, err)

		decodeService := &corev1.Service{}
		err = fakeClient.Get(ctx, types.NamespacedName{
			Name:      GenerateServiceName(deployment.Name, 0, "abc12345", testServiceRoleDecode),
			Namespace: deployment.Namespace,
		}, decodeService)
		require.NoError(t, err)

		// Verify portless service
		assert.Empty(t, decodeService.Spec.Ports, "service should be portless (no ports)")
	})

	t.Run("service name uses prv prefix", func(t *testing.T) {
		deployment := wrappers.BuildDisaggregatedSet("my-app", "default").UID("test-uid").WithRoleNoReplicas(testServiceRolePrefill, "nginx:1.0").WithRoleNoReplicas(testServiceRoleDecode, "nginx:1.0").Obj()

		fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(deployment).Build()
		serviceManager := NewServiceManager(fakeClient, scheme)

		revisionRoles := disaggregatedsetutils.RevisionRolesList{
			{
				Revision: "ef53f2d7",
				Roles: map[string]*leaderworkersetv1.LeaderWorkerSet{
					testServiceRolePrefill: &leaderworkersetv1.LeaderWorkerSet{ObjectMeta: metav1.ObjectMeta{Name: "my-app-ef53f2d7-prefill"}, Status: leaderworkersetv1.LeaderWorkerSetStatus{ReadyReplicas: 1}},
					testServiceRoleDecode:  &leaderworkersetv1.LeaderWorkerSet{ObjectMeta: metav1.ObjectMeta{Name: "my-app-ef53f2d7-decode"}, Status: leaderworkersetv1.LeaderWorkerSetStatus{ReadyReplicas: 1}},
				},
			},
		}

		err := serviceManager.ReconcileServices(ctx, deployment, 0, revisionRoles, "ef53f2d7")
		require.NoError(t, err)

		// Check expected service names with prv suffix
		expectedPrefillName := "my-app-0-ef53f2d7-prefill-prv"
		expectedDecodeName := "my-app-0-ef53f2d7-decode-prv"

		prefillService := &corev1.Service{}
		err = fakeClient.Get(ctx, types.NamespacedName{Name: expectedPrefillName, Namespace: "default"}, prefillService)
		require.NoError(t, err, "service should have correct name: %s", expectedPrefillName)

		decodeService := &corev1.Service{}
		err = fakeClient.Get(ctx, types.NamespacedName{Name: expectedDecodeName, Namespace: "default"}, decodeService)
		require.NoError(t, err, "service should have correct name: %s", expectedDecodeName)
	})

	t.Run("standard labels are applied", func(t *testing.T) {
		deployment := wrappers.BuildDisaggregatedSet("test-deploy", "default").UID("test-uid").WithRoleNoReplicas(testServiceRolePrefill, "nginx:1.0").WithRoleNoReplicas(testServiceRoleDecode, "nginx:1.0").Obj()

		fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(deployment).Build()
		serviceManager := NewServiceManager(fakeClient, scheme)

		revisionRoles := disaggregatedsetutils.RevisionRolesList{
			{
				Revision: "abc12345",
				Roles: map[string]*leaderworkersetv1.LeaderWorkerSet{
					testServiceRolePrefill: &leaderworkersetv1.LeaderWorkerSet{ObjectMeta: metav1.ObjectMeta{Name: "test-abc12345-prefill"}, Status: leaderworkersetv1.LeaderWorkerSetStatus{ReadyReplicas: 1}},
					testServiceRoleDecode:  &leaderworkersetv1.LeaderWorkerSet{ObjectMeta: metav1.ObjectMeta{Name: "test-abc12345-decode"}, Status: leaderworkersetv1.LeaderWorkerSetStatus{ReadyReplicas: 1}},
				},
			},
		}

		err := serviceManager.ReconcileServices(ctx, deployment, 0, revisionRoles, "abc12345")
		require.NoError(t, err)

		decodeService := &corev1.Service{}
		err = fakeClient.Get(ctx, types.NamespacedName{
			Name:      GenerateServiceName(deployment.Name, 0, "abc12345", testServiceRoleDecode),
			Namespace: deployment.Namespace,
		}, decodeService)
		require.NoError(t, err)

		// Verify standard labels are present
		assert.Equal(t, "test-deploy", decodeService.Labels[disaggregatedsetv1.SetNameLabelKey], "name label should be set")
		assert.Equal(t, "abc12345", decodeService.Labels[disaggregatedsetv1.RevisionLabelKey], "revision label should be set")
		assert.Equal(t, testServiceRoleDecode, decodeService.Labels[disaggregatedsetv1.RoleLabelKey], "role label should be set")
		assert.Equal(t, "0", decodeService.Labels[disaggregatedsetv1.SliceLabelKey], "slice label should be set")
	})

	t.Run("selector matches pod labels for role and revision", func(t *testing.T) {
		deployment := wrappers.BuildDisaggregatedSet("test-deploy", "default").UID("test-uid").WithRoleNoReplicas(testServiceRolePrefill, "nginx:1.0").WithRoleNoReplicas(testServiceRoleDecode, "nginx:1.0").Obj()

		fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(deployment).Build()
		serviceManager := NewServiceManager(fakeClient, scheme)

		revisionRoles := disaggregatedsetutils.RevisionRolesList{
			{
				Revision: "abc12345",
				Roles: map[string]*leaderworkersetv1.LeaderWorkerSet{
					testServiceRolePrefill: &leaderworkersetv1.LeaderWorkerSet{ObjectMeta: metav1.ObjectMeta{Name: "test-abc12345-prefill"}, Status: leaderworkersetv1.LeaderWorkerSetStatus{ReadyReplicas: 1}},
					testServiceRoleDecode:  &leaderworkersetv1.LeaderWorkerSet{ObjectMeta: metav1.ObjectMeta{Name: "test-abc12345-decode"}, Status: leaderworkersetv1.LeaderWorkerSetStatus{ReadyReplicas: 1}},
				},
			},
		}

		err := serviceManager.ReconcileServices(ctx, deployment, 0, revisionRoles, "abc12345")
		require.NoError(t, err)

		decodeService := &corev1.Service{}
		err = fakeClient.Get(ctx, types.NamespacedName{
			Name:      GenerateServiceName(deployment.Name, 0, "abc12345", testServiceRoleDecode),
			Namespace: deployment.Namespace,
		}, decodeService)
		require.NoError(t, err)

		// Verify selector matches expected pod labels
		assert.Equal(t, "test-deploy", decodeService.Spec.Selector[disaggregatedsetv1.SetNameLabelKey])
		assert.Equal(t, "abc12345", decodeService.Spec.Selector[disaggregatedsetv1.RevisionLabelKey])
		assert.Equal(t, testServiceRoleDecode, decodeService.Spec.Selector[disaggregatedsetv1.RoleLabelKey])
		assert.Equal(t, "0", decodeService.Spec.Selector[disaggregatedsetv1.SliceLabelKey])
	})

	t.Run("old services deleted when revision is drained", func(t *testing.T) {
		deployment := wrappers.BuildDisaggregatedSet("test-deploy", "default").UID("test-uid").WithRoleNoReplicas(testServiceRolePrefill, "nginx:1.0").WithRoleNoReplicas(testServiceRoleDecode, "nginx:1.0").Obj()

		// Create an old service
		oldService := &corev1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Name:      GenerateServiceName(deployment.Name, 0, "old12345", testServiceRoleDecode),
				Namespace: deployment.Namespace,
				Labels: map[string]string{
					disaggregatedsetv1.SetNameLabelKey:  deployment.Name,
					disaggregatedsetv1.SliceLabelKey:    "0",
					disaggregatedsetv1.RoleLabelKey:     testServiceRoleDecode,
					disaggregatedsetv1.RevisionLabelKey: "old12345",
				},
			},
			Spec: corev1.ServiceSpec{
				ClusterIP: corev1.ClusterIPNone,
			},
		}

		fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(deployment, oldService).Build()
		serviceManager := NewServiceManager(fakeClient, scheme)

		// New revision is ready, old is drained
		revisionRoles := disaggregatedsetutils.RevisionRolesList{
			{
				Revision: "new12345",
				Roles: map[string]*leaderworkersetv1.LeaderWorkerSet{
					testServiceRolePrefill: &leaderworkersetv1.LeaderWorkerSet{ObjectMeta: metav1.ObjectMeta{Name: "test-new12345-prefill"}, Status: leaderworkersetv1.LeaderWorkerSetStatus{ReadyReplicas: 1}},
					testServiceRoleDecode:  &leaderworkersetv1.LeaderWorkerSet{ObjectMeta: metav1.ObjectMeta{Name: "test-new12345-decode"}, Status: leaderworkersetv1.LeaderWorkerSetStatus{ReadyReplicas: 1}},
				},
			},
		}

		err := serviceManager.ReconcileServices(ctx, deployment, 0, revisionRoles, "new12345")
		require.NoError(t, err)

		// Verify old service is deleted
		oldServiceCheck := &corev1.Service{}
		err = fakeClient.Get(ctx, types.NamespacedName{
			Name:      GenerateServiceName(deployment.Name, 0, "old12345", testServiceRoleDecode),
			Namespace: deployment.Namespace,
		}, oldServiceCheck)
		assert.Error(t, err, "old service should be deleted")

		// Verify new service exists
		newService := &corev1.Service{}
		err = fakeClient.Get(ctx, types.NamespacedName{
			Name:      GenerateServiceName(deployment.Name, 0, "new12345", testServiceRoleDecode),
			Namespace: deployment.Namespace,
		}, newService)
		require.NoError(t, err, "new service should exist")
	})

	t.Run("no flip-flop when multiple revisions are ready during rolling update", func(t *testing.T) {
		deployment := wrappers.BuildDisaggregatedSet("test-deploy", "default").UID("test-uid").WithRoleNoReplicas(testServiceRolePrefill, "nginx:1.0").WithRoleNoReplicas(testServiceRoleDecode, "nginx:1.0").Obj()

		// Create services for old revision (simulating existing state)
		oldPrefillService := &corev1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Name:      GenerateServiceName(deployment.Name, 0, "old12345", testServiceRolePrefill),
				Namespace: deployment.Namespace,
				Labels: map[string]string{
					disaggregatedsetv1.SetNameLabelKey:  deployment.Name,
					disaggregatedsetv1.SliceLabelKey:    "0",
					disaggregatedsetv1.RoleLabelKey:     testServiceRolePrefill,
					disaggregatedsetv1.RevisionLabelKey: "old12345",
				},
			},
			Spec: corev1.ServiceSpec{
				ClusterIP: corev1.ClusterIPNone,
			},
		}
		oldDecodeService := &corev1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Name:      GenerateServiceName(deployment.Name, 0, "old12345", testServiceRoleDecode),
				Namespace: deployment.Namespace,
				Labels: map[string]string{
					disaggregatedsetv1.SetNameLabelKey:  deployment.Name,
					disaggregatedsetv1.SliceLabelKey:    "0",
					disaggregatedsetv1.RoleLabelKey:     testServiceRoleDecode,
					disaggregatedsetv1.RevisionLabelKey: "old12345",
				},
			},
			Spec: corev1.ServiceSpec{
				ClusterIP: corev1.ClusterIPNone,
			},
		}

		fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(deployment, oldPrefillService, oldDecodeService).Build()
		serviceManager := NewServiceManager(fakeClient, scheme)

		// Both old and new revisions are ready (rolling update in progress)
		revisionRoles := disaggregatedsetutils.RevisionRolesList{
			{
				Revision: "old12345",
				Roles: map[string]*leaderworkersetv1.LeaderWorkerSet{
					testServiceRolePrefill: &leaderworkersetv1.LeaderWorkerSet{ObjectMeta: metav1.ObjectMeta{Name: "test-old12345-prefill"}, Status: leaderworkersetv1.LeaderWorkerSetStatus{ReadyReplicas: 2}},
					testServiceRoleDecode:  &leaderworkersetv1.LeaderWorkerSet{ObjectMeta: metav1.ObjectMeta{Name: "test-old12345-decode"}, Status: leaderworkersetv1.LeaderWorkerSetStatus{ReadyReplicas: 2}},
				},
			},
			{
				Revision: "new12345",
				Roles: map[string]*leaderworkersetv1.LeaderWorkerSet{
					testServiceRolePrefill: &leaderworkersetv1.LeaderWorkerSet{ObjectMeta: metav1.ObjectMeta{Name: "test-new12345-prefill"}, Status: leaderworkersetv1.LeaderWorkerSetStatus{ReadyReplicas: 1}},
					testServiceRoleDecode:  &leaderworkersetv1.LeaderWorkerSet{ObjectMeta: metav1.ObjectMeta{Name: "test-new12345-decode"}, Status: leaderworkersetv1.LeaderWorkerSetStatus{ReadyReplicas: 1}},
				},
			},
		}

		// Target revision is the new one
		targetRevision := "new12345"

		// First reconcile - new services created, old services kept (both still ready)
		err := serviceManager.ReconcileServices(ctx, deployment, 0, revisionRoles, targetRevision)
		require.NoError(t, err)

		// Verify new services are created
		newPrefillService := &corev1.Service{}
		err = fakeClient.Get(ctx, types.NamespacedName{
			Name:      GenerateServiceName(deployment.Name, 0, "new12345", testServiceRolePrefill),
			Namespace: deployment.Namespace,
		}, newPrefillService)
		require.NoError(t, err, "new prefill service should exist")

		// Old services should STILL exist (both revisions are ready during rolling update)
		err = fakeClient.Get(ctx, types.NamespacedName{
			Name:      GenerateServiceName(deployment.Name, 0, "old12345", testServiceRolePrefill),
			Namespace: deployment.Namespace,
		}, &corev1.Service{})
		require.NoError(t, err, "old prefill service should still exist during rolling update")

		// Now simulate old revision being fully drained
		drainedWorkloads := disaggregatedsetutils.RevisionRolesList{
			{
				Revision: "old12345",
				Roles: map[string]*leaderworkersetv1.LeaderWorkerSet{
					testServiceRolePrefill: &leaderworkersetv1.LeaderWorkerSet{ObjectMeta: metav1.ObjectMeta{Name: "test-old12345-prefill"}, Status: leaderworkersetv1.LeaderWorkerSetStatus{ReadyReplicas: 0}},
					testServiceRoleDecode:  &leaderworkersetv1.LeaderWorkerSet{ObjectMeta: metav1.ObjectMeta{Name: "test-old12345-decode"}, Status: leaderworkersetv1.LeaderWorkerSetStatus{ReadyReplicas: 0}},
				},
			},
			{
				Revision: "new12345",
				Roles: map[string]*leaderworkersetv1.LeaderWorkerSet{
					testServiceRolePrefill: &leaderworkersetv1.LeaderWorkerSet{ObjectMeta: metav1.ObjectMeta{Name: "test-new12345-prefill"}, Status: leaderworkersetv1.LeaderWorkerSetStatus{ReadyReplicas: 2}},
					testServiceRoleDecode:  &leaderworkersetv1.LeaderWorkerSet{ObjectMeta: metav1.ObjectMeta{Name: "test-new12345-decode"}, Status: leaderworkersetv1.LeaderWorkerSetStatus{ReadyReplicas: 2}},
				},
			},
		}

		// Reconcile after drain - old services should be deleted
		err = serviceManager.ReconcileServices(ctx, deployment, 0, drainedWorkloads, targetRevision)
		require.NoError(t, err)

		// Old services should now be deleted
		err = fakeClient.Get(ctx, types.NamespacedName{
			Name:      GenerateServiceName(deployment.Name, 0, "old12345", testServiceRolePrefill),
			Namespace: deployment.Namespace,
		}, &corev1.Service{})
		assert.Error(t, err, "old prefill service should be deleted after drain")

		err = fakeClient.Get(ctx, types.NamespacedName{
			Name:      GenerateServiceName(deployment.Name, 0, "old12345", testServiceRoleDecode),
			Namespace: deployment.Namespace,
		}, &corev1.Service{})
		assert.Error(t, err, "old decode service should be deleted after drain")
	})
}

func TestGenerateServiceName(t *testing.T) {
	tests := []struct {
		name     string
		baseName string
		slice    int
		role     string
		revision string
		expected string
	}{
		{
			name:     "prefill service",
			baseName: "my-app",
			slice:    0,
			role:     testServiceRolePrefill,
			revision: "abc12345",
			expected: "my-app-0-abc12345-prefill-prv",
		},
		{
			name:     "decode service slice 1",
			baseName: "my-app",
			slice:    1,
			role:     testServiceRoleDecode,
			revision: "def67890",
			expected: "my-app-1-def67890-decode-prv",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := GenerateServiceName(tt.baseName, tt.slice, tt.revision, tt.role)
			assert.Equal(t, tt.expected, result)
		})
	}
}

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
	"k8s.io/utils/ptr"
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

// readyLWS builds a slice-0 LWS fixture with the standard name and labels and a
// given ready replica count. Services are derived from the LWS, so fixtures must
// carry realistic names and labels.
func readyLWS(dsName, revision, role string, ready int32) *leaderworkersetv1.LeaderWorkerSet {
	lws := &leaderworkersetv1.LeaderWorkerSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:   disaggregatedsetutils.GenerateName(dsName, 0, revision, role),
			Labels: disaggregatedsetutils.GenerateLabels(dsName, 0, revision, role),
		},
		Spec: leaderworkersetv1.LeaderWorkerSetSpec{Replicas: ptr.To(ready)},
	}
	lws.Status.Replicas = ready
	lws.Status.ReadyReplicas = ready
	return lws
}

// serviceName mirrors the production service name (<lws-name>-prv, where the LWS
// name is GenerateName) for slice 0, for building expected names in assertions.
func serviceName(base, revision, role string) string {
	return disaggregatedsetutils.GenerateName(base, 0, revision, role) + "-prv"
}

func TestServiceManager(t *testing.T) {
	ctx := context.Background()
	scheme := testSchemeForUnit()

	t.Run("services are created without cross-role readiness", func(t *testing.T) {
		deployment := wrappers.BuildDisaggregatedSet("test-deploy", "default").UID("test-uid").WithRoleNoReplicas(testServiceRolePrefill, "nginx:1.0").WithRoleNoReplicas(testServiceRoleDecode, "nginx:1.0").Obj()

		fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(deployment).Build()
		serviceManager := NewServiceManager(fakeClient, scheme)

		// Only prefill is ready
		revisionRoles := disaggregatedsetutils.RevisionRolesList{
			{
				Revision: "abc12345",
				Roles: map[string]*leaderworkersetv1.LeaderWorkerSet{
					testServiceRolePrefill: readyLWS("test-deploy", "abc12345", testServiceRolePrefill, 2),
					testServiceRoleDecode:  readyLWS("test-deploy", "abc12345", testServiceRoleDecode, 0), // not ready
				},
			},
		}

		err := serviceManager.ReconcileServices(ctx, deployment, 0, revisionRoles, "abc12345")
		require.NoError(t, err)

		// Services are compatibility objects and no longer gate on all roles.
		serviceList := &corev1.ServiceList{}
		err = fakeClient.List(ctx, serviceList)
		require.NoError(t, err)
		assert.Len(t, serviceList.Items, 2)
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
					testServiceRolePrefill: readyLWS("test-deploy", "abc12345", testServiceRolePrefill, 1),
					testServiceRoleDecode:  readyLWS("test-deploy", "abc12345", testServiceRoleDecode, 1),
				},
			},
		}

		err := serviceManager.ReconcileServices(ctx, deployment, 0, revisionRoles, "abc12345")
		require.NoError(t, err)

		// Verify services created for both roles
		prefillService := &corev1.Service{}
		err = fakeClient.Get(ctx, types.NamespacedName{
			Name:      serviceName(deployment.Name, "abc12345", testServiceRolePrefill),
			Namespace: deployment.Namespace,
		}, prefillService)
		require.NoError(t, err, "prefill service should exist")

		decodeService := &corev1.Service{}
		err = fakeClient.Get(ctx, types.NamespacedName{
			Name:      serviceName(deployment.Name, "abc12345", testServiceRoleDecode),
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
					testServiceRolePrefill: readyLWS("test-deploy", "abc12345", testServiceRolePrefill, 1),
					testServiceRoleDecode:  readyLWS("test-deploy", "abc12345", testServiceRoleDecode, 1),
				},
			},
		}

		err := serviceManager.ReconcileServices(ctx, deployment, 0, revisionRoles, "abc12345")
		require.NoError(t, err)

		prefillService := &corev1.Service{}
		err = fakeClient.Get(ctx, types.NamespacedName{
			Name:      serviceName(deployment.Name, "abc12345", testServiceRolePrefill),
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
					testServiceRolePrefill: readyLWS("test-deploy", "abc12345", testServiceRolePrefill, 1),
					testServiceRoleDecode:  readyLWS("test-deploy", "abc12345", testServiceRoleDecode, 1),
				},
			},
		}

		err := serviceManager.ReconcileServices(ctx, deployment, 0, revisionRoles, "abc12345")
		require.NoError(t, err)

		decodeService := &corev1.Service{}
		err = fakeClient.Get(ctx, types.NamespacedName{
			Name:      serviceName(deployment.Name, "abc12345", testServiceRoleDecode),
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
					testServiceRolePrefill: readyLWS("my-app", "ef53f2d7", testServiceRolePrefill, 1),
					testServiceRoleDecode:  readyLWS("my-app", "ef53f2d7", testServiceRoleDecode, 1),
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
					testServiceRolePrefill: readyLWS("test-deploy", "abc12345", testServiceRolePrefill, 1),
					testServiceRoleDecode:  readyLWS("test-deploy", "abc12345", testServiceRoleDecode, 1),
				},
			},
		}

		err := serviceManager.ReconcileServices(ctx, deployment, 0, revisionRoles, "abc12345")
		require.NoError(t, err)

		decodeService := &corev1.Service{}
		err = fakeClient.Get(ctx, types.NamespacedName{
			Name:      serviceName(deployment.Name, "abc12345", testServiceRoleDecode),
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
					testServiceRolePrefill: readyLWS("test-deploy", "abc12345", testServiceRolePrefill, 1),
					testServiceRoleDecode:  readyLWS("test-deploy", "abc12345", testServiceRoleDecode, 1),
				},
			},
		}

		err := serviceManager.ReconcileServices(ctx, deployment, 0, revisionRoles, "abc12345")
		require.NoError(t, err)

		decodeService := &corev1.Service{}
		err = fakeClient.Get(ctx, types.NamespacedName{
			Name:      serviceName(deployment.Name, "abc12345", testServiceRoleDecode),
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
				Name:      serviceName(deployment.Name, "old12345", testServiceRoleDecode),
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
					testServiceRolePrefill: readyLWS("test-deploy", "new12345", testServiceRolePrefill, 1),
					testServiceRoleDecode:  readyLWS("test-deploy", "new12345", testServiceRoleDecode, 1),
				},
			},
		}

		err := serviceManager.ReconcileServices(ctx, deployment, 0, revisionRoles, "new12345")
		require.NoError(t, err)

		// Verify old service is deleted
		oldServiceCheck := &corev1.Service{}
		err = fakeClient.Get(ctx, types.NamespacedName{
			Name:      serviceName(deployment.Name, "old12345", testServiceRoleDecode),
			Namespace: deployment.Namespace,
		}, oldServiceCheck)
		assert.Error(t, err, "old service should be deleted")

		// Verify new service exists
		newService := &corev1.Service{}
		err = fakeClient.Get(ctx, types.NamespacedName{
			Name:      serviceName(deployment.Name, "new12345", testServiceRoleDecode),
			Namespace: deployment.Namespace,
		}, newService)
		require.NoError(t, err, "new service should exist")
	})

	t.Run("legacy slice-agnostic service is adopted and cleaned up on drain", func(t *testing.T) {
		deployment := wrappers.BuildDisaggregatedSet("test-deploy", "default").UID("test-uid").WithRoleNoReplicas(testServiceRolePrefill, "nginx:1.0").WithRoleNoReplicas(testServiceRoleDecode, "nginx:1.0").Obj()

		// A legacy (pre-slices) service: legacy name, no slice label.
		legacyPrefill := &corev1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Name:      disaggregatedsetutils.GenerateLegacyName(deployment.Name, "old12345", testServiceRolePrefill) + "-prv",
				Namespace: deployment.Namespace,
				Labels: map[string]string{
					disaggregatedsetv1.SetNameLabelKey:  deployment.Name,
					disaggregatedsetv1.RoleLabelKey:     testServiceRolePrefill,
					disaggregatedsetv1.RevisionLabelKey: "old12345",
				},
			},
			Spec: corev1.ServiceSpec{ClusterIP: corev1.ClusterIPNone},
		}

		fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(deployment, legacyPrefill).Build()
		serviceManager := NewServiceManager(fakeClient, scheme)

		// New slice-aware revision is ready; the legacy revision is drained.
		revisionRoles := disaggregatedsetutils.RevisionRolesList{
			{
				Revision: "new12345",
				Roles: map[string]*leaderworkersetv1.LeaderWorkerSet{
					testServiceRolePrefill: readyLWS("test-deploy", "new12345", testServiceRolePrefill, 1),
					testServiceRoleDecode:  readyLWS("test-deploy", "new12345", testServiceRoleDecode, 1),
				},
			},
		}

		err := serviceManager.ReconcileServices(ctx, deployment, 0, revisionRoles, "new12345")
		require.NoError(t, err)

		// The legacy slice-agnostic service belongs to slice 0 and its revision is
		// drained, so it should be deleted.
		err = fakeClient.Get(ctx, types.NamespacedName{Name: legacyPrefill.Name, Namespace: deployment.Namespace}, &corev1.Service{})
		assert.Error(t, err, "legacy service should be deleted once its revision drains")
	})

	t.Run("no flip-flop when multiple revisions are ready during rolling update", func(t *testing.T) {
		deployment := wrappers.BuildDisaggregatedSet("test-deploy", "default").UID("test-uid").WithRoleNoReplicas(testServiceRolePrefill, "nginx:1.0").WithRoleNoReplicas(testServiceRoleDecode, "nginx:1.0").Obj()

		// Create services for old revision (simulating existing state)
		oldPrefillService := &corev1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Name:      serviceName(deployment.Name, "old12345", testServiceRolePrefill),
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
				Name:      serviceName(deployment.Name, "old12345", testServiceRoleDecode),
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
					testServiceRolePrefill: readyLWS("test-deploy", "old12345", testServiceRolePrefill, 2),
					testServiceRoleDecode:  readyLWS("test-deploy", "old12345", testServiceRoleDecode, 2),
				},
			},
			{
				Revision: "new12345",
				Roles: map[string]*leaderworkersetv1.LeaderWorkerSet{
					testServiceRolePrefill: readyLWS("test-deploy", "new12345", testServiceRolePrefill, 1),
					testServiceRoleDecode:  readyLWS("test-deploy", "new12345", testServiceRoleDecode, 1),
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
			Name:      serviceName(deployment.Name, "new12345", testServiceRolePrefill),
			Namespace: deployment.Namespace,
		}, newPrefillService)
		require.NoError(t, err, "new prefill service should exist")

		// Old services should STILL exist (both revisions are ready during rolling update)
		err = fakeClient.Get(ctx, types.NamespacedName{
			Name:      serviceName(deployment.Name, "old12345", testServiceRolePrefill),
			Namespace: deployment.Namespace,
		}, &corev1.Service{})
		require.NoError(t, err, "old prefill service should still exist during rolling update")

		// Now simulate old revision being fully drained
		drainedWorkloads := disaggregatedsetutils.RevisionRolesList{
			{
				Revision: "old12345",
				Roles: map[string]*leaderworkersetv1.LeaderWorkerSet{
					testServiceRolePrefill: readyLWS("test-deploy", "old12345", testServiceRolePrefill, 0),
					testServiceRoleDecode:  readyLWS("test-deploy", "old12345", testServiceRoleDecode, 0),
				},
			},
			{
				Revision: "new12345",
				Roles: map[string]*leaderworkersetv1.LeaderWorkerSet{
					testServiceRolePrefill: readyLWS("test-deploy", "new12345", testServiceRolePrefill, 2),
					testServiceRoleDecode:  readyLWS("test-deploy", "new12345", testServiceRoleDecode, 2),
				},
			},
		}

		// Reconcile after drain - old services should be deleted
		err = serviceManager.ReconcileServices(ctx, deployment, 0, drainedWorkloads, targetRevision)
		require.NoError(t, err)

		// Old services should now be deleted
		err = fakeClient.Get(ctx, types.NamespacedName{
			Name:      serviceName(deployment.Name, "old12345", testServiceRolePrefill),
			Namespace: deployment.Namespace,
		}, &corev1.Service{})
		assert.Error(t, err, "old prefill service should be deleted after drain")

		err = fakeClient.Get(ctx, types.NamespacedName{
			Name:      serviceName(deployment.Name, "old12345", testServiceRoleDecode),
			Namespace: deployment.Namespace,
		}, &corev1.Service{})
		assert.Error(t, err, "old decode service should be deleted after drain")
	})

	t.Run("removed parent sub-role services remain until their revision drains", func(t *testing.T) {
		deployment := wrappers.BuildDisaggregatedSet("test-deploy", "default").UID("test-uid").WithRoleNoReplicas(testServiceRoleDecode, "nginx:1.0").Obj()
		oldLWS := readyLWS(deployment.Name, "old12345", "removed", 1)
		subRoleService := &corev1.Service{ObjectMeta: metav1.ObjectMeta{
			Name:      oldLWS.Name + "-short-prv",
			Namespace: deployment.Namespace,
			Labels: map[string]string{
				disaggregatedsetv1.SetNameLabelKey:  deployment.Name,
				disaggregatedsetv1.SliceLabelKey:    "0",
				disaggregatedsetv1.RevisionLabelKey: "old12345",
				disaggregatedsetv1.RoleLabelKey:     "removed",
				disaggregatedsetv1.SubRoleLabelKey:  "short",
			},
		}}
		fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(deployment, subRoleService).Build()
		serviceManager := NewServiceManager(fakeClient, scheme)
		targetLWS := readyLWS(deployment.Name, "new12345", testServiceRoleDecode, 1)
		revisions := disaggregatedsetutils.RevisionRolesList{
			{Revision: "old12345", Roles: map[string]*leaderworkersetv1.LeaderWorkerSet{"removed": oldLWS}},
			{Revision: "new12345", Roles: map[string]*leaderworkersetv1.LeaderWorkerSet{testServiceRoleDecode: targetLWS}},
		}

		require.NoError(t, serviceManager.ReconcileServices(ctx, deployment, 0, revisions, "new12345"))
		require.NoError(t, fakeClient.Get(ctx, types.NamespacedName{Name: subRoleService.Name, Namespace: deployment.Namespace}, &corev1.Service{}))

		oldLWS.Spec.Replicas = ptr.To(int32(0))
		require.NoError(t, serviceManager.ReconcileServices(ctx, deployment, 0, revisions, "new12345"))
		err := fakeClient.Get(ctx, types.NamespacedName{Name: subRoleService.Name, Namespace: deployment.Namespace}, &corev1.Service{})
		assert.Error(t, err)
	})
}

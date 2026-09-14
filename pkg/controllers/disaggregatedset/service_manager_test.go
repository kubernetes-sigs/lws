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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	disaggregatedsetv1 "sigs.k8s.io/lws/api/disaggregatedset/v1"
	leaderworkersetv1 "sigs.k8s.io/lws/api/leaderworkerset/v1"
	disaggregatedsetutils "sigs.k8s.io/lws/pkg/utils/disaggregatedset"
	"sigs.k8s.io/lws/test/wrappers"
)

const (
	testServiceRolePrefill = "prefill"
	testServiceRoleDecode  = "decode"
)

func readyLWS(dsName, revision, role string, ready int32) *leaderworkersetv1.LeaderWorkerSet {
	name := disaggregatedsetutils.GenerateName(dsName, 0, revision, role)
	lws := &leaderworkersetv1.LeaderWorkerSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "default",
			UID:       types.UID(name + "-uid"),
			Labels:    disaggregatedsetutils.GenerateLabels(dsName, 0, revision, role),
		},
	}
	lws.Status.ReadyReplicas = ready
	return lws
}

func serviceName(base, revision, role string) string {
	return disaggregatedsetutils.PrivateServiceName(disaggregatedsetutils.GenerateName(base, 0, revision, role))
}

func revisionRoles(dsName, revision string, decodeReady int32) disaggregatedsetutils.RevisionRolesList {
	return disaggregatedsetutils.RevisionRolesList{{
		Revision: revision,
		Roles: map[string]*leaderworkersetv1.LeaderWorkerSet{
			testServiceRolePrefill: readyLWS(dsName, revision, testServiceRolePrefill, 1),
			testServiceRoleDecode:  readyLWS(dsName, revision, testServiceRoleDecode, decodeReady),
		},
	}}
}

func TestServiceManager(t *testing.T) {
	ctx := context.Background()
	scheme := testSchemeForUnit()

	t.Run("does not create services until every role is ready", func(t *testing.T) {
		deployment := wrappers.BuildDisaggregatedSet("test-deploy", "default").UID("test-uid").
			WithRoleNoReplicas(testServiceRolePrefill, "nginx:1.0").
			WithRoleNoReplicas(testServiceRoleDecode, "nginx:1.0").Obj()
		fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(deployment).Build()
		serviceManager := NewServiceManager(fakeClient, scheme)

		err := serviceManager.ReconcileServices(ctx, deployment, revisionRoles(deployment.Name, "abc12345", 0), "abc12345")
		require.NoError(t, err)

		services := &corev1.ServiceList{}
		require.NoError(t, fakeClient.List(ctx, services))
		assert.Empty(t, services.Items)
	})

	t.Run("creates an LWS-owned headless service for each role", func(t *testing.T) {
		deployment := wrappers.BuildDisaggregatedSet("test-deploy", "default").UID("test-uid").
			WithRoleNoReplicas(testServiceRolePrefill, "nginx:1.0").
			WithRoleNoReplicas(testServiceRoleDecode, "nginx:1.0").Obj()
		roles := revisionRoles(deployment.Name, "abc12345", 1)
		fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(deployment).Build()
		serviceManager := NewServiceManager(fakeClient, scheme)

		require.NoError(t, serviceManager.ReconcileServices(ctx, deployment, roles, "abc12345"))

		for _, role := range []string{testServiceRolePrefill, testServiceRoleDecode} {
			service := &corev1.Service{}
			require.NoError(t, fakeClient.Get(ctx, types.NamespacedName{
				Name: serviceName(deployment.Name, "abc12345", role), Namespace: deployment.Namespace,
			}, service))
			assert.True(t, metav1.IsControlledBy(service, roles[0].Roles[role]))
			assert.False(t, metav1.IsControlledBy(service, deployment))
			assert.Equal(t, corev1.ClusterIPNone, service.Spec.ClusterIP)
			assert.Empty(t, service.Spec.Ports)
			assert.Equal(t, deployment.Name, service.Labels[disaggregatedsetv1.SetNameLabelKey])
			assert.Equal(t, "0", service.Labels[disaggregatedsetv1.SliceLabelKey])
			assert.Equal(t, role, service.Spec.Selector[disaggregatedsetv1.RoleLabelKey])
			assert.Equal(t, "abc12345", service.Spec.Selector[disaggregatedsetv1.RevisionLabelKey])
		}
	})

	t.Run("transfers an existing service from the DisaggregatedSet to its LWS", func(t *testing.T) {
		deployment := wrappers.BuildDisaggregatedSet("test-deploy", "default").UID("test-uid").
			WithRoleNoReplicas(testServiceRolePrefill, "nginx:1.0").
			WithRoleNoReplicas(testServiceRoleDecode, "nginx:1.0").Obj()
		roles := revisionRoles(deployment.Name, "abc12345", 1)
		decodeLWS := roles[0].Roles[testServiceRoleDecode]
		existing := &corev1.Service{ObjectMeta: metav1.ObjectMeta{
			Name:      disaggregatedsetutils.PrivateServiceName(decodeLWS.Name),
			Namespace: deployment.Namespace,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: disaggregatedsetv1.GroupVersion.String(),
				Kind:       "DisaggregatedSet",
				Name:       deployment.Name,
				UID:        deployment.UID,
				Controller: ptr.To(true),
			}},
		}}
		fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(deployment, existing).Build()
		serviceManager := NewServiceManager(fakeClient, scheme)

		require.NoError(t, serviceManager.ReconcileServices(ctx, deployment, roles, "abc12345"))

		got := &corev1.Service{}
		require.NoError(t, fakeClient.Get(ctx, types.NamespacedName{Name: existing.Name, Namespace: existing.Namespace}, got))
		assert.True(t, metav1.IsControlledBy(got, decodeLWS))
		assert.False(t, metav1.IsControlledBy(got, deployment))
	})

	t.Run("does not adopt a colliding uncontrolled service", func(t *testing.T) {
		deployment := wrappers.BuildDisaggregatedSet("test-deploy", "default").UID("test-uid").
			WithRoleNoReplicas(testServiceRolePrefill, "nginx:1.0").
			WithRoleNoReplicas(testServiceRoleDecode, "nginx:1.0").Obj()

		for _, tc := range []struct {
			name  string
			owner []metav1.OwnerReference
		}{
			{
				name: "foreign owner",
				owner: []metav1.OwnerReference{{
					APIVersion: disaggregatedsetv1.GroupVersion.String(),
					Kind:       "DisaggregatedSet",
					Name:       "other",
					UID:        "other-uid",
					Controller: ptr.To(true),
				}},
			},
			{name: "no owner"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				roles := revisionRoles(deployment.Name, "abc12345", 1)
				decodeLWS := roles[0].Roles[testServiceRoleDecode]
				colliding := &corev1.Service{ObjectMeta: metav1.ObjectMeta{
					Name:            disaggregatedsetutils.PrivateServiceName(decodeLWS.Name),
					Namespace:       deployment.Namespace,
					OwnerReferences: tc.owner,
				}}
				fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(deployment, colliding).Build()
				serviceManager := NewServiceManager(fakeClient, scheme)

				require.Error(t, serviceManager.ReconcileServices(ctx, deployment, roles, "abc12345"))

				got := &corev1.Service{}
				require.NoError(t, fakeClient.Get(ctx, types.NamespacedName{Name: colliding.Name, Namespace: colliding.Namespace}, got))
				assert.Equal(t, tc.owner, got.OwnerReferences)
			})
		}
	})

	t.Run("creates target services without deleting an older revision", func(t *testing.T) {
		deployment := wrappers.BuildDisaggregatedSet("test-deploy", "default").UID("test-uid").
			WithRoleNoReplicas(testServiceRolePrefill, "nginx:1.0").
			WithRoleNoReplicas(testServiceRoleDecode, "nginx:1.0").Obj()
		oldService := &corev1.Service{ObjectMeta: metav1.ObjectMeta{
			Name:      serviceName(deployment.Name, "old12345", testServiceRolePrefill),
			Namespace: deployment.Namespace,
		}}
		roles := append(
			revisionRoles(deployment.Name, "old12345", 1),
			revisionRoles(deployment.Name, "new12345", 1)...,
		)
		fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(deployment, oldService).Build()
		serviceManager := NewServiceManager(fakeClient, scheme)

		require.NoError(t, serviceManager.ReconcileServices(ctx, deployment, roles, "new12345"))
		require.NoError(t, fakeClient.Get(ctx, types.NamespacedName{Name: oldService.Name, Namespace: oldService.Namespace}, &corev1.Service{}))
		require.NoError(t, fakeClient.Get(ctx, types.NamespacedName{
			Name: serviceName(deployment.Name, "new12345", testServiceRolePrefill), Namespace: deployment.Namespace,
		}, &corev1.Service{}))
	})

	t.Run("recovers when the service is deleted between Create's AlreadyExists and the ownership check", func(t *testing.T) {
		deployment := wrappers.BuildDisaggregatedSet("test-deploy", "default").UID("test-uid").
			WithRoleNoReplicas(testServiceRolePrefill, "nginx:1.0").
			WithRoleNoReplicas(testServiceRoleDecode, "nginx:1.0").Obj()
		roles := revisionRoles(deployment.Name, "abc12345", 1)
		baseClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(deployment).Build()
		// Simulate a Service that is deleted (e.g. by GC) in the window between
		// Create's AlreadyExists and the follow-up ownership check: Create always
		// reports AlreadyExists, but the Service was never actually persisted.
		fakeClient := interceptor.NewClient(baseClient, interceptor.Funcs{
			Create: func(ctx context.Context, _ client.WithWatch, obj client.Object, _ ...client.CreateOption) error {
				return apierrors.NewAlreadyExists(schema.GroupResource{Resource: "services"}, obj.GetName())
			},
		})
		serviceManager := NewServiceManager(fakeClient, scheme)

		err := serviceManager.ReconcileServices(ctx, deployment, roles, "abc12345")
		require.NoError(t, err, "a service deleted before the ownership check should be left for the next reconcile to recreate")
	})
}

// dsOwnedService builds a Service as an older controller created it: named after
// the LWS, labelled with name/role/revision, and controlled by the DisaggregatedSet.
func dsOwnedService(name string, ds *disaggregatedsetv1.DisaggregatedSet, role, revision string) *corev1.Service {
	return &corev1.Service{ObjectMeta: metav1.ObjectMeta{
		Name:      name,
		Namespace: ds.Namespace,
		UID:       types.UID(name + "-uid"),
		Labels: map[string]string{
			disaggregatedsetv1.SetNameLabelKey:  ds.Name,
			disaggregatedsetv1.RoleLabelKey:     role,
			disaggregatedsetv1.RevisionLabelKey: revision,
		},
		OwnerReferences: []metav1.OwnerReference{{
			APIVersion: disaggregatedsetv1.GroupVersion.String(),
			Kind:       "DisaggregatedSet",
			Name:       ds.Name,
			UID:        ds.UID,
			Controller: ptr.To(true),
		}},
	}}
}

func TestMigrateLegacyServices(t *testing.T) {
	ctx := context.Background()
	scheme := testSchemeForUnit()
	const revision = "abc12345"

	newDeployment := func() *disaggregatedsetv1.DisaggregatedSet {
		return wrappers.BuildDisaggregatedSet("test-deploy", "default").UID("test-uid").
			WithRoleNoReplicas(testServiceRolePrefill, "nginx:1.0").
			WithRoleNoReplicas(testServiceRoleDecode, "nginx:1.0").Obj()
	}

	t.Run("hands a DisaggregatedSet-owned service to its LWS in place", func(t *testing.T) {
		deployment := newDeployment()
		lws := readyLWS(deployment.Name, revision, testServiceRolePrefill, 1)
		service := dsOwnedService(disaggregatedsetutils.PrivateServiceName(lws.Name), deployment, testServiceRolePrefill, revision)
		fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(deployment, lws, service).Build()

		require.NoError(t, NewServiceManager(fakeClient, scheme).
			migrateLegacyServices(ctx, deployment, []*leaderworkersetv1.LeaderWorkerSet{lws}))

		got := &corev1.Service{}
		require.NoError(t, fakeClient.Get(ctx, types.NamespacedName{Name: service.Name, Namespace: service.Namespace}, got))
		assert.True(t, metav1.IsControlledBy(got, lws))
		assert.False(t, metav1.IsControlledBy(got, deployment))
		assert.Equal(t, service.UID, got.UID, "the service must be migrated in place, never recreated")
	})

	t.Run("tolerates a service deleted between list and ownership transfer", func(t *testing.T) {
		deployment := newDeployment()
		lws := readyLWS(deployment.Name, revision, testServiceRolePrefill, 1)
		service := dsOwnedService(disaggregatedsetutils.PrivateServiceName(lws.Name), deployment, testServiceRolePrefill, revision)
		fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(deployment, lws, service).
			WithInterceptorFuncs(interceptor.Funcs{
				Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
					if key.Name == service.Name {
						return apierrors.NewNotFound(schema.GroupResource{Resource: "services"}, key.Name)
					}
					return c.Get(ctx, key, obj, opts...)
				},
			}).Build()

		require.NoError(t, NewServiceManager(fakeClient, scheme).
			migrateLegacyServices(ctx, deployment, []*leaderworkersetv1.LeaderWorkerSet{lws}))
	})

	// An old controller that stopped after deleting a drained LWS but before
	// deleting its Service leaves this behind; nothing else would ever remove it.
	t.Run("deletes a DisaggregatedSet-owned service whose LWS is already gone", func(t *testing.T) {
		deployment := newDeployment()
		orphan := dsOwnedService(serviceName(deployment.Name, "old12345", testServiceRolePrefill), deployment, testServiceRolePrefill, "old12345")
		live := readyLWS(deployment.Name, revision, testServiceRolePrefill, 1)
		fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(deployment, live, orphan).Build()

		require.NoError(t, NewServiceManager(fakeClient, scheme).
			migrateLegacyServices(ctx, deployment, []*leaderworkersetv1.LeaderWorkerSet{live}))

		err := fakeClient.Get(ctx, types.NamespacedName{Name: orphan.Name, Namespace: orphan.Namespace}, &corev1.Service{})
		assert.True(t, apierrors.IsNotFound(err), "orphaned legacy service should be deleted")
	})

	t.Run("never touches a service this DisaggregatedSet does not control", func(t *testing.T) {
		for _, tc := range []struct {
			name  string
			owner []metav1.OwnerReference
		}{
			{
				name: "foreign owner",
				owner: []metav1.OwnerReference{{
					APIVersion: disaggregatedsetv1.GroupVersion.String(),
					Kind:       "DisaggregatedSet",
					Name:       "other",
					UID:        "other-uid",
					Controller: ptr.To(true),
				}},
			},
			{name: "no owner"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				deployment := newDeployment()
				lws := readyLWS(deployment.Name, revision, testServiceRolePrefill, 1)
				// Carries this DisaggregatedSet's labels, so it is listed but must be skipped.
				service := dsOwnedService(disaggregatedsetutils.PrivateServiceName(lws.Name), deployment, testServiceRolePrefill, revision)
				service.OwnerReferences = tc.owner
				fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(deployment, lws, service).Build()

				require.NoError(t, NewServiceManager(fakeClient, scheme).
					migrateLegacyServices(ctx, deployment, []*leaderworkersetv1.LeaderWorkerSet{lws}))

				got := &corev1.Service{}
				require.NoError(t, fakeClient.Get(ctx, types.NamespacedName{Name: service.Name, Namespace: service.Namespace}, got))
				assert.Equal(t, tc.owner, got.OwnerReferences)
			})
		}
	})

	t.Run("never touches a DisaggregatedSet-owned service that is not a private role service", func(t *testing.T) {
		deployment := newDeployment()
		lws := readyLWS(deployment.Name, revision, testServiceRolePrefill, 1)
		unsuffixed := dsOwnedService(deployment.Name+"-user-facing", deployment, testServiceRolePrefill, revision)
		unlabelled := dsOwnedService(disaggregatedsetutils.PrivateServiceName(lws.Name), deployment, "", "")
		fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(deployment, lws, unsuffixed, unlabelled).Build()

		require.NoError(t, NewServiceManager(fakeClient, scheme).
			migrateLegacyServices(ctx, deployment, []*leaderworkersetv1.LeaderWorkerSet{lws}))

		for _, service := range []*corev1.Service{unsuffixed, unlabelled} {
			got := &corev1.Service{}
			require.NoError(t, fakeClient.Get(ctx, types.NamespacedName{Name: service.Name, Namespace: service.Namespace}, got),
				"service %s should be left alone", service.Name)
			assert.True(t, metav1.IsControlledBy(got, deployment), "service %s ownership should be unchanged", service.Name)
		}
	})
}

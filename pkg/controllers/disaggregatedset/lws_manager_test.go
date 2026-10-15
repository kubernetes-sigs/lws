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
	"errors"
	"fmt"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/events"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	leaderworkersetv1 "sigs.k8s.io/lws/api/leaderworkerset/v1"

	disaggregatedsetv1 "sigs.k8s.io/lws/api/disaggregatedset/v1"
	"sigs.k8s.io/lws/pkg/replicagroups"
	disaggregatedsetutils "sigs.k8s.io/lws/pkg/utils/disaggregatedset"
	"sigs.k8s.io/lws/pkg/webhooks"
	disaggwebhook "sigs.k8s.io/lws/pkg/webhooks/disaggregatedset"
	"sigs.k8s.io/lws/test/wrappers"
)

func TestCurrentRevisionRolloutBudgetUpdates(t *testing.T) {
	for _, identity := range []leaderworkersetv1.GroupIdentityType{leaderworkersetv1.GroupIdentityOrdinal, leaderworkersetv1.GroupIdentityHash} {
		for _, tc := range []struct {
			name                 string
			before, after        int32
			oldBudget, newBudget leaderworkersetv1.RollingUpdateConfiguration
		}{
			{name: "resume zero", after: 1, newBudget: leaderworkersetv1.RollingUpdateConfiguration{MaxSurge: intstr.FromInt(1)}},
			{name: "percentage needs new replica count", before: 1, after: 10, oldBudget: leaderworkersetv1.RollingUpdateConfiguration{MaxSurge: intstr.FromInt(1)}, newBudget: leaderworkersetv1.RollingUpdateConfiguration{MaxUnavailable: intstr.FromString("10%")}},
			{name: "pause at zero", before: 1, oldBudget: leaderworkersetv1.RollingUpdateConfiguration{MaxSurge: intstr.FromInt(1)}},
			{name: "budget only", before: 1, after: 1, oldBudget: leaderworkersetv1.RollingUpdateConfiguration{MaxSurge: intstr.FromInt(1)}, newBudget: leaderworkersetv1.RollingUpdateConfiguration{MaxUnavailable: intstr.FromInt(1)}},
		} {
			t.Run(string(identity)+"/"+tc.name, func(t *testing.T) {
				ds, role := testManagerDSWithRole("model")
				role.Spec.GroupIdentity = identity
				role.Spec.LeaderWorkerTemplate.Size = ptr.To[int32](1)
				role.Spec.Replicas = ptr.To(tc.before)
				role.Spec.RolloutStrategy.RollingUpdateConfiguration = &tc.oldBudget
				w := &disaggwebhook.DisaggregatedSetWebhook{}
				_, err := w.ValidateCreate(t.Context(), ds)
				require.NoError(t, err)
				oldDS := ds.DeepCopy()
				c := admittingLWSClient()
				manager := NewLeaderWorkerSetManager(c)
				revision := disaggregatedsetutils.ComputeRevision(ds.Spec.Roles)
				require.NoError(t, manager.Create(t.Context(), ds, role, 0, revision, int(tc.before), nil))
				// Creation now starts empty; establish the old count before updating it.
				r := &DisaggregatedSetReconciler{Client: c, LWSManager: manager}
				existing, err := manager.GetForRole(t.Context(), ds, 0, revision, role.Name)
				require.NoError(t, err)
				require.NoError(t, r.reconcileCurrentRevisionRole(t.Context(), ds, 0, existing, role, revision, map[string]int{role.Name: int(tc.before)}))
				role.Spec.Replicas = ptr.To(tc.after)
				role.Spec.RolloutStrategy.RollingUpdateConfiguration = &tc.newBudget
				_, err = w.ValidateUpdate(t.Context(), oldDS, ds)
				require.NoError(t, err)
				require.Equal(t, revision, disaggregatedsetutils.ComputeRevision(ds.Spec.Roles))
				before := ds.DeepCopy()
				for range 2 {
					existing, err := manager.GetForRole(t.Context(), ds, 0, revision, role.Name)
					require.NoError(t, err)
					require.NoError(t, r.reconcileCurrentRevisionRole(t.Context(), ds, 0, existing, role, revision, map[string]int{role.Name: int(tc.after)}))
				}
				got, err := manager.GetForRole(t.Context(), ds, 0, revision, role.Name)
				require.NoError(t, err)
				require.Equal(t, tc.after, *got.Spec.Replicas)
				require.Equal(t, tc.newBudget.MaxSurge, got.Spec.RolloutStrategy.RollingUpdateConfiguration.MaxSurge)
				require.Equal(t, tc.newBudget.MaxUnavailable, got.Spec.RolloutStrategy.RollingUpdateConfiguration.MaxUnavailable)
				resourceVersion := got.ResourceVersion
				require.NoError(t, r.reconcileCurrentRevisionRole(t.Context(), ds, 0, got, role, revision, map[string]int{role.Name: int(tc.after)}))
				got, err = manager.GetForRole(t.Context(), ds, 0, revision, role.Name)
				require.NoError(t, err)
				require.Equal(t, resourceVersion, got.ResourceVersion, "settled configuration must not be rewritten")
				require.Equal(t, before, ds, "reconciliation must not mutate the role template")
			})
		}
	}
}

func TestBudgetOnlyUpdateWaitsForInformer(t *testing.T) {
	lws := revisionLWS("target", testRolePrefill, 4, 4, time.Time{}, 4)
	manager, stored, cached := newObservedTestManager(t, lws)
	ds := newTestDisaggregatedSet()
	desired := defaultedRolloutStrategy(&leaderworkersetv1.RolloutStrategy{
		RollingUpdateConfiguration: &leaderworkersetv1.RollingUpdateConfiguration{MaxSurge: intstr.FromInt(2)},
	})
	require.NoError(t, manager.scale(t.Context(), ds, lws, 4, desired))
	written := readTestLWS(t, stored, lws)
	require.EqualValues(t, 4, getLWSReplicas(written))
	require.Equal(t, *desired, written.Spec.RolloutStrategy)
	require.Equal(t, lws.Spec, readTestLWS(t, cached, lws).Spec, "cache has not observed the write")
	require.ErrorIs(t, manager.pending.observed(t.Context(), ds.UID, "0"), errReplicaGroupsPending)
	require.ErrorIs(t, manager.scale(t.Context(), ds, lws, 4, desired), errReplicaGroupsPending)

	observed := deliverTestLWSUpdate(t, stored, cached, lws)
	require.NoError(t, manager.pending.observed(t.Context(), ds.UID, "0"))
	require.NoError(t, manager.scale(t.Context(), ds, observed, 4, desired))
	require.Equal(t, written.ResourceVersion, readTestLWS(t, stored, lws).ResourceVersion, "settled budget needs no rewrite")
}

// Fake clients otherwise accept patches that the real LWS webhook rejects.
func admittingLWSClient() client.WithWatch {
	return fake.NewClientBuilder().WithScheme(testSchemeForUnit()).WithInterceptorFuncs(lwsAdmissionInterceptors()).Build()
}

func lwsAdmissionInterceptors() interceptor.Funcs {
	w := &webhooks.LeaderWorkerSetWebhook{}
	return interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if lws, ok := obj.(*leaderworkersetv1.LeaderWorkerSet); ok {
				if err := w.Default(ctx, lws); err != nil {
					return err
				}
				if _, err := w.ValidateCreate(ctx, lws); err != nil {
					return err
				}
			}
			return c.Create(ctx, obj, opts...)
		},
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			if lws, ok := obj.(*leaderworkersetv1.LeaderWorkerSet); ok {
				old := &leaderworkersetv1.LeaderWorkerSet{}
				if err := c.Get(ctx, client.ObjectKeyFromObject(lws), old); err != nil {
					return err
				}
				if err := w.Default(ctx, lws); err != nil {
					return err
				}
				if _, err := w.ValidateUpdate(ctx, old, lws); err != nil {
					return err
				}
			}
			return c.Patch(ctx, obj, patch, opts...)
		},
	}
}

func TestSubRoleRolloutBudgetUpdates(t *testing.T) {
	for _, identity := range []leaderworkersetv1.GroupIdentityType{leaderworkersetv1.GroupIdentityOrdinal, leaderworkersetv1.GroupIdentityHash} {
		for _, tc := range []struct {
			name          string
			before, after int
			budget        leaderworkersetv1.RollingUpdateConfiguration
		}{
			{name: "resume zero", after: 1, budget: leaderworkersetv1.RollingUpdateConfiguration{MaxSurge: intstr.FromInt(1)}},
			{name: "percentage needs final count", before: 1, after: 10, budget: leaderworkersetv1.RollingUpdateConfiguration{MaxUnavailable: intstr.FromString("10%")}},
			{name: "pause zero", before: 1},
		} {
			t.Run(string(identity)+"/"+tc.name, func(t *testing.T) {
				children := make([]string, tc.before)
				for i := range children {
					children[i] = "a"
				}
				f := newSubRoleFixtureForIdentity(t, identity, 0, children...)
				f.lws.Spec.RolloutStrategy.RollingUpdateConfiguration = &leaderworkersetv1.RollingUpdateConfiguration{MaxSurge: intstr.FromInt(tc.before)}
				require.NoError(t, f.manager.client.Update(t.Context(), f.lws))
				f.manager.client = interceptor.NewClient(f.manager.client.(client.WithWatch), lwsAdmissionInterceptors())
				role := &disaggregatedsetv1.DisaggregatedRoleSpec{Name: "model", SubRoles: []disaggregatedsetv1.DisaggregatedSubRoleSpec{{Name: "a", Replicas: ptr.To(int32(tc.after))}}}
				role.Spec.RolloutStrategy.RollingUpdateConfiguration = &tc.budget
				r := &DisaggregatedSetReconciler{LWSManager: f.manager}
				for range 10 {
					err := r.reconcileCurrentRevisionRole(t.Context(), f.ds, 0, f.observe().LWS, role, f.lws.Name, map[string]int{"model": tc.after, "model/a": tc.after})
					require.True(t, err == nil || errors.Is(err, errReplicaGroupsPending), "%v", err)
					f.native()
				}
				got := f.observe().LWS
				require.EqualValues(t, tc.after, *got.Spec.Replicas)
				require.Equal(t, tc.budget.MaxSurge, got.Spec.RolloutStrategy.RollingUpdateConfiguration.MaxSurge)
				require.Equal(t, tc.budget.MaxUnavailable, got.Spec.RolloutStrategy.RollingUpdateConfiguration.MaxUnavailable)
			})
		}
	}
}

var managerTestLabels = map[string]string{
	disaggregatedsetv1.SetNameLabelKey:  "test-deployment",
	disaggregatedsetv1.RoleLabelKey:     "prefill",
	disaggregatedsetv1.RevisionLabelKey: "abc123",
}

func TestRolloutResumesZeroRoleWithUpdatedBudget(t *testing.T) {
	for _, identity := range []leaderworkersetv1.GroupIdentityType{leaderworkersetv1.GroupIdentityOrdinal, leaderworkersetv1.GroupIdentityHash} {
		for _, child := range []string{"", "a"} {
			t.Run(string(identity)+"/"+child, func(t *testing.T) {
				f := newSubRoleFixtureForIdentity(t, identity, 0)
				role := disaggregatedsetv1.DisaggregatedRoleSpec{Name: "model"}
				if child == "" {
					delete(f.lws.Annotations, subRoleReplicasAnnotation)
					delete(f.lws.Annotations, disaggregatedsetv1.InitialSubRoleReplicasAnnotationKey)
				} else {
					role.SubRoles = []disaggregatedsetv1.DisaggregatedSubRoleSpec{{Name: child}}
					setSubRoleJSON(f.lws, subRoleReplicasAnnotation, map[string]int{child: 0})
					setSubRoleJSON(f.lws, disaggregatedsetv1.InitialSubRoleReplicasAnnotationKey, map[string]int{child: 1})
				}
				f.lws.Spec.RolloutStrategy.RollingUpdateConfiguration = &leaderworkersetv1.RollingUpdateConfiguration{}
				require.NoError(t, f.manager.client.Update(t.Context(), f.lws))
				f.manager.client = interceptor.NewClient(f.manager.client.(client.WithWatch), lwsAdmissionInterceptors())
				role.Spec.RolloutStrategy.RollingUpdateConfiguration = &leaderworkersetv1.RollingUpdateConfiguration{MaxSurge: intstr.FromInt(1)}
				f.ds.Spec.Roles = []disaggregatedsetv1.DisaggregatedRoleSpec{role}
				executor := &RollingUpdateExecutor{LWSManager: f.manager, Record: events.NewFakeRecorder(10)}
				for range 5 {
					f.observe()
					err := executor.scaleRevision(t.Context(), f.ds, f.revision("target"), []string{childRoleKey(role.Name, child)}, RoleReplicaState{1}, scaleUp)
					require.True(t, err == nil || errors.Is(err, errReplicaGroupsPending), "%v", err)
				}
				got := f.observe().LWS
				require.EqualValues(t, 1, *got.Spec.Replicas)
				require.Equal(t, intstr.FromInt(1), got.Spec.RolloutStrategy.RollingUpdateConfiguration.MaxSurge)
			})
		}
	}
}

func TestRolloutBudgetFallback(t *testing.T) {
	percentage := leaderworkersetv1.RollingUpdateConfiguration{MaxUnavailable: intstr.FromString("10%")}
	surge := leaderworkersetv1.RollingUpdateConfiguration{MaxSurge: intstr.FromString("1%")}
	fallback := leaderworkersetv1.RollingUpdateConfiguration{MaxUnavailable: intstr.FromInt(1)}
	for _, identity := range []leaderworkersetv1.GroupIdentityType{leaderworkersetv1.GroupIdentityOrdinal, leaderworkersetv1.GroupIdentityHash} {
		for _, child := range []string{"", "a"} {
			for _, tc := range []struct {
				name                      string
				before                    int
				counts                    []int
				direction                 scaleDirection
				current, desired, interim leaderworkersetv1.RollingUpdateConfiguration
			}{
				{name: "old percentage drains to zero", before: 10, counts: []int{9, 5, 1, 0}, direction: scaleDown, current: percentage, desired: surge, interim: fallback},
				{name: "zero budgets resume in steps", counts: []int{1, 5, 9, 10}, direction: scaleUp, desired: percentage, interim: fallback},
				{name: "valid existing surge is retained", before: 1, counts: []int{2, 9, 10}, direction: scaleUp, current: surge, desired: percentage, interim: surge},
			} {
				t.Run(string(identity)+"/"+child+"/"+tc.name, func(t *testing.T) {
					children := make([]string, tc.before)
					for i := range children {
						children[i] = "a"
					}
					f := newSubRoleFixtureForIdentity(t, identity, 0, children...)
					role := disaggregatedsetv1.DisaggregatedRoleSpec{Name: "model"}
					if child == "" {
						delete(f.lws.Annotations, subRoleReplicasAnnotation)
						delete(f.lws.Annotations, disaggregatedsetv1.InitialSubRoleReplicasAnnotationKey)
					} else {
						role.SubRoles = []disaggregatedsetv1.DisaggregatedSubRoleSpec{{Name: child, Replicas: ptr.To[int32](10)}}
						setSubRoleJSON(f.lws, subRoleReplicasAnnotation, map[string]int{child: tc.before})
						setSubRoleJSON(f.lws, disaggregatedsetv1.InitialSubRoleReplicasAnnotationKey, map[string]int{child: 10})
					}
					f.lws.Spec.RolloutStrategy = *defaultedRolloutStrategy(&leaderworkersetv1.RolloutStrategy{RollingUpdateConfiguration: tc.current.DeepCopy()})
					if identity == leaderworkersetv1.GroupIdentityOrdinal && tc.before > 0 {
						f.lws.Spec.RolloutStrategy.RollingUpdateConfiguration.Partition = ptr.To[int32](2)
					}
					require.NoError(t, f.manager.client.Update(t.Context(), f.lws))
					f.manager.client = interceptor.NewClient(f.manager.client.(client.WithWatch), lwsAdmissionInterceptors())
					role.Spec.Replicas = ptr.To[int32](10)
					role.Spec.RolloutStrategy.RollingUpdateConfiguration = tc.desired.DeepCopy()
					f.ds.Spec.Roles = []disaggregatedsetv1.DisaggregatedRoleSpec{role}
					before := f.ds.DeepCopy()
					executor := &RollingUpdateExecutor{LWSManager: f.manager, Record: events.NewFakeRecorder(100)}
					for _, count := range tc.counts {
						// Subrole assignment may need several reconciles before the physical scale.
						for range 8 {
							err := executor.scaleRevision(t.Context(), f.ds, f.revision("revision"), []string{childRoleKey(role.Name, child)}, RoleReplicaState{count}, tc.direction)
							require.True(t, err == nil || errors.Is(err, errReplicaGroupsPending), "%v", err)
							f.native()
							if getLWSReplicas(f.observe().LWS) == int32(count) {
								break
							}
						}
						got := f.observe().LWS
						require.EqualValues(t, count, *got.Spec.Replicas)
						want := defaultedRolloutStrategy(&leaderworkersetv1.RolloutStrategy{RollingUpdateConfiguration: tc.interim.DeepCopy()})
						if identity == leaderworkersetv1.GroupIdentityOrdinal && tc.before > 0 {
							want.RollingUpdateConfiguration.Partition = ptr.To[int32](2)
						}
						if tc.direction == scaleUp && count == 10 {
							want = defaultedRolloutStrategy(&role.Spec.RolloutStrategy)
						}
						require.Equal(t, *want, got.Spec.RolloutStrategy, "replicas=%d", count)
					}
					require.Equal(t, before, f.ds, "repair only the child's budget, not the planner's policy")
				})
			}
		}
	}
}

func buildManagerTestLWS(annotations map[string]string) *leaderworkersetv1.LeaderWorkerSet {
	return wrappers.BuildBasicLeaderWorkerSet("test-lws", "default").
		Labels(managerTestLabels).
		Replica(3).
		Annotation(annotations).
		Obj()
}

// testManagerDS returns a minimal DisaggregatedSet fixture for ownership
// checks in Scale/GetForRole tests.
func testManagerDS(name string) *disaggregatedsetv1.DisaggregatedSet {
	return wrappers.BuildDisaggregatedSet(name, "default").Obj()
}

func testManagerDSWithRole(name string) (*disaggregatedsetv1.DisaggregatedSet, *disaggregatedsetv1.DisaggregatedRoleSpec) {
	ds := wrappers.BuildDisaggregatedSet(name, "default").WithRole("prefill", 1, "").Obj()
	return ds, &ds.Spec.Roles[0]
}

// ownerRefFor builds the controller OwnerReference this package's own
// LeaderWorkerSetManager.Create sets, so fixtures match real objects.
func ownerRefFor(ds *disaggregatedsetv1.DisaggregatedSet) metav1.OwnerReference {
	return metav1.OwnerReference{
		APIVersion: disaggregatedsetv1.GroupVersion.String(),
		Kind:       "DisaggregatedSet",
		Name:       ds.Name,
		UID:        ds.UID,
		Controller: ptr.To(true),
	}
}

// buildOwnedManagerTestLWS is buildManagerTestLWS plus a controller
// OwnerReference to owner, for ownership-check tests.
func buildOwnedManagerTestLWS(name string, replicas int32, owner *disaggregatedsetv1.DisaggregatedSet) *leaderworkersetv1.LeaderWorkerSet {
	return wrappers.BuildBasicLeaderWorkerSet(name, "default").
		Labels(managerTestLabels).
		Replica(int(replicas)).
		OwnerReference(ownerRefFor(owner)).
		Obj()
}

// TestParseInitialReplicasAnnotation tests the parseInitialReplicasAnnotation function.
func TestParseInitialReplicasAnnotation(t *testing.T) {
	testCases := []struct {
		name        string
		annotations map[string]string
		expected    *int
	}{
		{
			name:        "nil annotations map returns nil",
			annotations: nil,
			expected:    nil,
		},
		{
			name:        "missing annotation returns nil",
			annotations: map[string]string{"other-key": "value"},
			expected:    nil,
		},
		{
			name:        "invalid non-numeric annotation returns nil",
			annotations: map[string]string{disaggregatedsetv1.InitialReplicasAnnotationKey: "not-a-number"},
			expected:    nil,
		},
		{
			name:        "negative annotation returns nil",
			annotations: map[string]string{disaggregatedsetv1.InitialReplicasAnnotationKey: "-1"},
			expected:    nil,
		},
		{
			name:        "valid annotation returns correct value",
			annotations: map[string]string{disaggregatedsetv1.InitialReplicasAnnotationKey: "5"},
			expected:    ptr.To(5),
		},
		{
			name:        "zero value annotation returns zero",
			annotations: map[string]string{disaggregatedsetv1.InitialReplicasAnnotationKey: "0"},
			expected:    ptr.To(0),
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			leaderWorkerSet := &leaderworkersetv1.LeaderWorkerSet{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: testCase.annotations,
				},
			}
			result := parseInitialReplicasAnnotation(leaderWorkerSet)
			if testCase.expected == nil {
				require.Nil(t, result)
			} else {
				require.NotNil(t, result)
				require.Equal(t, *testCase.expected, *result)
			}
		})
	}
}

// TestGetLWSReplicas tests the getLWSReplicas function.
func TestGetLWSReplicas(t *testing.T) {
	testCases := []struct {
		name     string
		replicas *int32
		expected int32
	}{
		{
			name:     "nil replicas returns 1",
			replicas: nil,
			expected: 1,
		},
		{
			name:     "set replicas returns correct value",
			replicas: ptr.To(int32(5)),
			expected: 5,
		},
		{
			name:     "zero replicas returns 0",
			replicas: ptr.To(int32(0)),
			expected: 0,
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			leaderWorkerSet := &leaderworkersetv1.LeaderWorkerSet{
				Spec: leaderworkersetv1.LeaderWorkerSetSpec{
					Replicas: testCase.replicas,
				},
			}
			result := getLWSReplicas(leaderWorkerSet)
			require.Equal(t, testCase.expected, result)
		})
	}
}

// TestManagerDelete tests the manager's Delete method.
func TestManagerDelete(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, leaderworkersetv1.AddToScheme(scheme))

	t.Run("successfully deletes existing LWS", func(t *testing.T) {
		existingLWS := buildManagerTestLWS(nil)
		existingLWS.UID = types.UID("test-lws-uid")

		var deleteOptions client.DeleteOptions
		fakeClient := fake.NewClientBuilder().
			WithScheme(scheme).
			WithRuntimeObjects(existingLWS).
			WithInterceptorFuncs(interceptor.Funcs{
				Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
					deleteOptions.ApplyOptions(opts)
					return c.Delete(ctx, obj, opts...)
				},
			}).
			Build()

		manager := NewLeaderWorkerSetManager(fakeClient)
		err := manager.deleteInForeground(context.Background(), existingLWS)

		require.NoError(t, err)
		require.NotNil(t, deleteOptions.PropagationPolicy)
		assert.Equal(t, metav1.DeletePropagationForeground, *deleteOptions.PropagationPolicy)
		require.NotNil(t, deleteOptions.Preconditions)
		require.NotNil(t, deleteOptions.Preconditions.UID)
		assert.Equal(t, existingLWS.UID, *deleteOptions.Preconditions.UID)
	})

	t.Run("returns nil when LWS not found (idempotent)", func(t *testing.T) {
		fakeClient := fake.NewClientBuilder().
			WithScheme(scheme).
			Build()

		manager := NewLeaderWorkerSetManager(fakeClient)
		err := manager.deleteInForeground(context.Background(), &leaderworkersetv1.LeaderWorkerSet{
			ObjectMeta: metav1.ObjectMeta{Name: "nonexistent", Namespace: "default"},
		})

		require.NoError(t, err) // Should not error, deletion is idempotent
	})
}

func TestTerminatingLWSIsIgnored(t *testing.T) {
	scheme := testSchemeForUnit()
	ds := testManagerDS("test")
	ds.UID = types.UID("uid")

	terminating := func(revision string, replicas int32, ready bool) *leaderworkersetv1.LeaderWorkerSet {
		readyReplicas := int32(0)
		if ready {
			readyReplicas = replicas
		}
		lws := revisionLWS(revision, testRolePrefill, replicas, readyReplicas, time.Time{}, replicas)
		now := metav1.Now()
		lws.DeletionTimestamp = &now
		lws.Finalizers = []string{"foregroundDeletion"}
		return lws
	}

	for _, tc := range []struct {
		name, revision string
		ready          bool
	}{
		{"excluded from rollout discovery regardless of Spec", "old", false},
		{"terminating target is not reported as usable capacity", "target", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fakeClient := fake.NewClientBuilder().WithScheme(scheme).
				WithRuntimeObjects(terminating(tc.revision, 3, tc.ready)).Build()
			oldRevisions, newRevision, err := NewLeaderWorkerSetManager(fakeClient).
				GetRevisionRolesList(context.Background(), ds, 0, "target")
			require.NoError(t, err)
			assert.Empty(t, oldRevisions)
			assert.Nil(t, newRevision)
		})
	}

	t.Run("cleanup does not delete or emit an event again", func(t *testing.T) {
		deleteCalls := 0
		fakeClient := fake.NewClientBuilder().WithScheme(scheme).
			WithRuntimeObjects(terminating("old", 0, false)).
			WithInterceptorFuncs(interceptor.Funcs{
				Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
					deleteCalls++
					return c.Delete(ctx, obj, opts...)
				},
			}).Build()
		recorder := events.NewFakeRecorder(10)
		reconciler := &DisaggregatedSetReconciler{
			LWSManager: NewLeaderWorkerSetManager(fakeClient),
			Record:     recorder,
		}

		require.NoError(t, reconciler.cleanupDrainedLWS(context.Background(), ds, 0, "target", false))
		assert.Zero(t, deleteCalls)
		assert.Empty(t, recorder.Events)
	})
}

func TestCleanupDrainedLWSRetainsAtMostOneMarker(t *testing.T) {
	scheme := testSchemeForUnit()
	ds := testManagerDS("test")
	ds.UID = types.UID("uid")
	createdAt := time.Now()
	old := func(revision string, replicas int32, age time.Duration) *leaderworkersetv1.LeaderWorkerSet {
		return revisionLWS(revision, testRolePrefill, replicas, replicas, createdAt.Add(age), replicas)
	}

	for _, tc := range []struct {
		name               string
		objects            []client.Object
		rolloutComplete    bool
		remainingRevisions []string
	}{
		{
			name:               "non-drained revision replaces every zero marker",
			objects:            []client.Object{old("A", 0, 0), old("B", 0, time.Hour), old("C", 1, 2*time.Hour)},
			remainingRevisions: []string{"C"},
		},
		{
			name:               "newest zero revision is retained while target is unready",
			objects:            []client.Object{old("A", 0, 0), old("B", 0, time.Hour)},
			remainingRevisions: []string{"B"},
		},
		{
			name:            "completion removes the final marker",
			objects:         []client.Object{old("A", 0, 0), old("B", 0, time.Hour)},
			rolloutComplete: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(tc.objects...).Build()
			reconciler := &DisaggregatedSetReconciler{
				LWSManager: NewLeaderWorkerSetManager(fakeClient),
				Record:     events.NewFakeRecorder(20),
			}

			require.NoError(t, reconciler.cleanupDrainedLWS(context.Background(), ds, 0, "target", tc.rolloutComplete))
			remaining, err := reconciler.LWSManager.ListForSlice(context.Background(), ds, 0, "")
			require.NoError(t, err)
			revisions := make([]string, 0, len(remaining))
			for _, lws := range remaining {
				revisions = append(revisions, lws.Labels[disaggregatedsetv1.RevisionLabelKey])
			}
			assert.ElementsMatch(t, tc.remainingRevisions, revisions)
		})
	}
}

func TestReconcileSliceRecoversFromMissingGrowth(t *testing.T) {
	for _, identity := range []leaderworkersetv1.GroupIdentityType{leaderworkersetv1.GroupIdentityOrdinal, leaderworkersetv1.GroupIdentityHash} {
		for _, tc := range []struct {
			name                      string
			actual, reported, desired int32
			old, terminating, blocked bool
		}{
			{name: "cancel missing group", actual: 2, reported: 2, desired: 2},
			{name: "cancel entirely missing growth", desired: 0},
			{name: "drain old to free quota", actual: 2, reported: 2, desired: 4, old: true},
			{name: "Pod cache still behind", actual: 2, reported: 3, desired: 2, blocked: true},
			{name: "deletion still unobserved", actual: 4, reported: 4, desired: 2, blocked: true},
			{name: "termination already observed", actual: 3, reported: 3, desired: 2, terminating: true},
		} {
			t.Run(string(identity)+"/"+tc.name, func(t *testing.T) {
				ds := newTestDisaggregatedSet(makeRoleSpec(testRolePrefill, tc.desired, corev1.PodSpec{}, intstr.FromInt(1), intstr.FromInt(1)))
				target := revisionLWS("target", testRolePrefill, 3, tc.actual, time.Unix(2, 0), 3)
				target.Spec.GroupIdentity, target.Spec.LeaderWorkerTemplate.Size = identity, ptr.To[int32](1)
				objects := replicaGroupObjects(target, tc.actual, tc.actual)
				for _, object := range objects {
					switch object := object.(type) {
					case *appsv1.StatefulSet:
						object.Status.Replicas = tc.reported
					case *appsv1.ReplicaSet:
						object.Status.Replicas = tc.reported
						if tc.terminating {
							object.Status.Replicas-- // ReplicaSet status excludes terminating Pods.
						}
					}
				}
				if tc.terminating {
					pod := objects[len(objects)-1].(*corev1.Pod)
					pod.Finalizers, pod.DeletionTimestamp = []string{"test/hold"}, ptr.To(metav1.Now())
				}
				old := revisionLWS("old", testRolePrefill, 4, 4, time.Unix(1, 0), 4)
				if tc.old {
					old.Spec.GroupIdentity, old.Spec.LeaderWorkerTemplate.Size = identity, ptr.To[int32](1)
					objects = append(objects, replicaGroupObjects(old, 4, 4)...)
				}
				c := newTestClient(objects...)
				r := newTestReconciler(c)
				r.LWSManager = NewLeaderWorkerSetManager(writeOnlyTestClient(t, c))
				r.LWSManager.reader, r.LWSManager.pending = c, testPendingWrites(c)
				before := &corev1.PodList{}
				require.NoError(t, c.List(t.Context(), before))
				for range 4 { // Includes takeover and baseline receipt observations.
					_, err := r.reconcileSlice(t.Context(), r.createRollingUpdateExecutor(), ds, 0, "target", map[string]int{testRolePrefill: int(tc.desired)})
					if err != nil {
						require.ErrorIs(t, err, errReplicaGroupsPending)
					}
				}
				got := getTestLWSReplicas(c, ds.Namespace, target.Name)
				switch {
				case tc.blocked:
					require.EqualValues(t, 3, got, "unobserved deletions cannot be bypassed")
				case tc.old:
					retainedOld := getTestLWSReplicas(c, ds.Namespace, old.Name)
					require.Less(t, retainedOld, int32(4), "missing target growth must not block safe old drain")
					require.GreaterOrEqual(t, retainedOld+tc.actual, int32(3), "retain desired - maxUnavailable Ready groups")
				default:
					require.EqualValues(t, tc.desired, got)
				}
				after := &corev1.PodList{}
				require.NoError(t, c.List(t.Context(), after))
				require.Equal(t, before.Items, after.Items, "recovery changes intent, never directly deletes Pods")
			})
		}
	}
}

func TestRolloutWaitsForMetadataReceipts(t *testing.T) {
	for _, change := range []string{"old baseline", "target baseline", "replacement policy"} {
		t.Run(change, func(t *testing.T) {
			ds := newTestDisaggregatedSet(makeRoleSpec(testRolePrefill, 4, corev1.PodSpec{}, intstr.FromInt(1), intstr.FromInt(1)))
			old := revisionLWS("old", testRolePrefill, 4, 4, time.Unix(1, 0), 4)
			target := revisionLWS("target", testRolePrefill, 1, 1, time.Unix(2, 0), 4)
			if change == "old baseline" {
				delete(old.Annotations, disaggregatedsetv1.InitialReplicasAnnotationKey)
			} else if change == "target baseline" {
				setInitialReplicasAnnotation(target, 1)
			} else {
				ds.Spec.Roles[0].Spec.GroupReplacementPolicy = leaderworkersetv1.GroupReplacementImmediate
				old.Spec.GroupReplacementPolicy = leaderworkersetv1.GroupReplacementImmediate
			}
			var objects []client.Object
			for _, lws := range []*leaderworkersetv1.LeaderWorkerSet{old, target} {
				lws.Spec.GroupIdentity, lws.Spec.LeaderWorkerTemplate.Size = leaderworkersetv1.GroupIdentityHash, ptr.To[int32](1)
				objects = append(objects, replicaGroupObjects(lws, getLWSReplicas(lws), getLWSReplicas(lws))...)
			}
			manager, base, cached := newObservedTestManager(t, objects...)
			r := newTestReconciler(base)
			r.LWSManager = manager
			r.LWSManager.pending.initialized = map[types.UID]client.ObjectKey{old.UID: client.ObjectKeyFromObject(ds), target.UID: client.ObjectKeyFromObject(ds)}
			for range 3 {
				_, err := r.reconcileSlice(t.Context(), r.createRollingUpdateExecutor(), ds, 0, "target", map[string]int{testRolePrefill: 4})
				if err != nil {
					require.ErrorIs(t, err, errReplicaGroupsPending)
				}
				require.EqualValues(t, 4, getTestLWSReplicas(base, ds.Namespace, old.Name), "metadata writes must be observed before ANY drain")
				require.EqualValues(t, 1, getTestLWSReplicas(base, ds.Namespace, target.Name))
			}
			for _, lws := range []*leaderworkersetv1.LeaderWorkerSet{old, target} {
				deliverTestLWSUpdate(t, base, cached, lws)
			}
			_, err := r.reconcileSlice(t.Context(), r.createRollingUpdateExecutor(), ds, 0, "target", map[string]int{testRolePrefill: 4})
			require.NoError(t, err)
			require.Less(t, getTestLWSReplicas(base, ds.Namespace, old.Name), int32(4), "observation unblocks rollout progress")
		})
	}
}

func TestReconcileSliceWaitsForPolicyObservationBeforeScaling(t *testing.T) {
	for _, tc := range []struct {
		name             string
		desired, initial int32
	}{
		{"policy only", 4, 4},
		{"policy and replicas with baseline patch", 6, 4},
		{"policy and replicas with baseline already persisted", 6, 6},
		{"policy and scale down", 2, 4},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lws := revisionLWS("A", testRolePrefill, 4, 4, time.Time{}, tc.initial)
			lws.Generation = 1
			lws.Spec.GroupIdentity = leaderworkersetv1.GroupIdentityHash
			lws.Spec.GroupReplacementPolicy = leaderworkersetv1.GroupReplacementPostTermination
			lws.Status.ObservedGeneration = lws.Generation
			lws.Spec.LeaderWorkerTemplate.Size = ptr.To[int32](1)
			objects := replicaGroupObjects(lws, 4, 4)
			manager, base, cached := newObservedTestManager(t, objects...)
			o := manager.pending
			r := &DisaggregatedSetReconciler{LWSManager: manager}
			ds := newTwoRoleTestDisaggregatedSet([2]int32{tc.desired, 4}, [2]int{1, 1}, [2]int{})
			ds.Spec.Roles = ds.Spec.Roles[:1]
			ds.Spec.Roles[0].Spec.GroupIdentity = leaderworkersetv1.GroupIdentityHash
			ds.Spec.Roles[0].Spec.GroupReplacementPolicy = leaderworkersetv1.GroupReplacementImmediate
			// Skip takeover so the first pass really reaches the policy write.
			o.initialized = map[types.UID]client.ObjectKey{lws.UID: client.ObjectKeyFromObject(ds)}
			_, err := r.reconcileSlice(t.Context(), r.createRollingUpdateExecutor(), ds, 0, "A", map[string]int{testRolePrefill: int(tc.desired)})
			require.ErrorIs(t, err, errReplicaGroupsPending)
			require.Equal(t, leaderworkersetv1.GroupReplacementImmediate, readTestLWS(t, base, lws).Spec.GroupReplacementPolicy, "policy was actually written")
			require.Equal(t, leaderworkersetv1.GroupReplacementPostTermination, readTestLWS(t, cached, lws).Spec.GroupReplacementPolicy, "informer remains behind")
			require.EqualValues(t, 4, getTestLWSReplicas(base, lws.Namespace, lws.Name), "wait for our policy write before changing scale")
			deliverTestLWSUpdate(t, base, cached, lws)
			result, err := r.reconcileSlice(t.Context(), r.createRollingUpdateExecutor(), ds, 0, "A", map[string]int{testRolePrefill: int(tc.desired)})
			require.NoError(t, err)
			if tc.initial != tc.desired {
				require.Equal(t, time.Second, result.RequeueAfter, "baseline is another write to observe")
				require.EqualValues(t, 4, getTestLWSReplicas(base, lws.Namespace, lws.Name))
				deliverTestLWSUpdate(t, base, cached, lws)
				_, err = r.reconcileSlice(t.Context(), r.createRollingUpdateExecutor(), ds, 0, "A", map[string]int{testRolePrefill: int(tc.desired)})
			}
			require.NoError(t, err)
			stored := readTestLWS(t, base, lws)
			require.Equal(t, tc.desired, getLWSReplicas(stored), "scale after the policy write is observed")
			require.Equal(t, leaderworkersetv1.GroupReplacementImmediate, stored.Spec.GroupReplacementPolicy)
			initial, ok := disaggregatedsetutils.GetInitialReplicas(stored)
			require.True(t, ok)
			require.Equal(t, tc.desired, initial)
		})
	}
}

func TestSubRoleReconcileUsesOnlyInformerReads(t *testing.T) {
	ds := newDSWithRoles("ds", disaggregatedsetv1.DisaggregatedRoleSpec{Name: "model", SubRoles: []disaggregatedsetv1.DisaggregatedSubRoleSpec{{Name: "a"}, {Name: "b"}}})
	desired := resolveDesiredReplicasByRole(ds, nil)
	budget, err := maxUnavailableAnnotation(ds, rolloutRoleNames(ds), desired)
	require.NoError(t, err)
	ds.Annotations = map[string]string{disaggregatedsetv1.RevisionHashVersionAnnotationKey: disaggregatedsetv1.RevisionHashVersion, disaggregatedsetv1.MaxUnavailableAnnotationKey: budget}
	c := newTestClient(ds)
	f := &subRoleFixture{t: t, ds: ds, manager: NewLeaderWorkerSetManager(c)}
	revision := disaggregatedsetutils.ComputeRevision(ds.Spec.Roles)
	f = f.withRevision(disaggregatedsetutils.GenerateName(ds.Name, 0, revision, "model"), "a", "b")
	f.lws.Labels[disaggregatedsetv1.RevisionLabelKey] = revision
	require.NoError(t, c.Update(t.Context(), f.lws))
	f.manager.pending = testPendingWrites(c)
	f.manager.client = writeOnlyTestClient(t, c)
	r := newTestReconciler(c)
	r.LWSManager = f.manager
	request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(ds)}
	_, err = r.Reconcile(t.Context(), request)
	require.NoError(t, err)

	// The old condition stays True, but the updated Pod cache must force repair.
	s, err := replicagroups.Observe(t.Context(), c, f.lws)
	require.NoError(t, err)
	worker := s.Groups[0].Pods[1]
	delete(worker.Labels, disaggregatedsetv1.SubRoleLabelKey)
	require.NoError(t, c.Update(t.Context(), worker))
	_, err = r.Reconcile(t.Context(), request)
	require.NoError(t, err)
	require.NoError(t, c.Get(t.Context(), client.ObjectKeyFromObject(worker), worker))
	require.Equal(t, "a", worker.Labels[disaggregatedsetv1.SubRoleLabelKey])
}

func TestSubRoleScaleWaitsForInformerObservations(t *testing.T) {
	f := newSubRoleFixture(t, 0, "a", "a", "b")
	before := f.observe()
	base := f.manager.client.(client.WithWatch)
	hideLWS, hidePods := true, true
	reader := interceptor.NewClient(base, interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, object client.Object, opts ...client.GetOption) error {
			if lws, ok := object.(*leaderworkersetv1.LeaderWorkerSet); ok && hideLWS {
				before.LWS.DeepCopyInto(lws)
				return nil
			}
			return c.Get(ctx, key, object, opts...)
		},
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if pods, ok := list.(*corev1.PodList); ok && hidePods {
				pods.Items = nil
				for _, group := range before.Groups {
					for _, pod := range group.Pods {
						pods.Items = append(pods.Items, *pod.DeepCopy())
					}
				}
				return nil
			}
			return c.List(ctx, list, opts...)
		},
	})
	f.manager.reader, f.manager.pending = reader, testPendingWrites(reader)
	_, err := f.manager.syncSubRoles(t.Context(), f.ds, before.LWS, subRoleUpdate{target: map[string]int{"a": 1, "b": 1}})
	require.ErrorIs(t, err, errReplicaGroupsPending)
	r := &DisaggregatedSetReconciler{LWSManager: f.manager}
	f.ds.Annotations = map[string]string{disaggregatedsetv1.MaxUnavailableAnnotationKey: `{"model/removed":2}`}
	for range 3 {
		_, err = r.reconcileSlice(t.Context(), r.createRollingUpdateExecutor(), f.ds, 0, "target", nil)
		require.ErrorIs(t, err, errReplicaGroupsPending, "unseen assignment intent blocks the whole slice")
		_, err = r.seedForRole(t.Context(), f.ds, "target")("model/a")
		require.ErrorIs(t, err, errReplicaGroupsPending, "a scaler must not freeze the old child count")
		roles, err := f.manager.retainedRolloutRoleNames(t.Context(), f.ds)
		require.NoError(t, err)
		require.Contains(t, roles, "model/removed", "unseen writes must not prune child budget history")
	}
	hideLWS = false
	require.NoError(t, f.manager.pending.observed(t.Context(), f.ds.UID, "0"))
	f.sync() // Relabel the retained second group from a to b.
	for range 3 {
		_, err = f.manager.syncSubRoles(t.Context(), f.ds, f.observe().LWS, subRoleUpdate{})
		require.Error(t, err, "stale Pod versions must not complete the assignment")
		require.EqualValues(t, 3, getTestLWSReplicas(base, f.ds.Namespace, f.lws.Name))
	}
	hidePods = false
	f.sync() // Observe the retained labels, clear the tail, and publish replicas=2.
	require.EqualValues(t, 2, getLWSReplicas(f.observe().LWS))
	_, err = f.manager.observeRolloutReadiness(t.Context(), nil, f.revision("old"))
	require.ErrorIs(t, err, errReplicaGroupsPending, "the native scale must also be observed")
	f.native()
	f.sync()
	observed, err := f.manager.collectRolloutObservations(t.Context(), f.ds, disaggregatedsetutils.RevisionRolesList{f.revision("old")}, disaggregatedsetutils.RevisionRoles{}, nil)
	require.NoError(t, err)
	for name, initial := range map[string]int{"model/a": 2, "model/b": 1} {
		require.Equal(t, replicaReadiness{spec: 1, initial: initial, raw: 1, committed: 1}, observed.role(f.revision("old"), name))
	}
}

func TestOrdinaryWritesRejectConcurrentChanges(t *testing.T) {
	lws := revisionLWS("A", testRolePrefill, 4, 4, time.Now(), 4)
	base := newTestClient(lws)
	concurrentChange := func(current *leaderworkersetv1.LeaderWorkerSet) { current.Spec.Replicas = ptr.To[int32](3) }
	c := interceptor.NewClient(base, interceptor.Funcs{
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			current := &leaderworkersetv1.LeaderWorkerSet{}
			require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(obj), current))
			concurrentChange(current)
			require.NoError(t, c.Update(ctx, current))
			return c.Patch(ctx, obj, patch, opts...)
		},
	})
	ds := newTwoRoleTestDisaggregatedSet([2]int32{4, 4}, [2]int{1, 1}, [2]int{})
	err := NewLeaderWorkerSetManager(c).Scale(t.Context(), ds, lws, 2)
	require.True(t, apierrors.IsConflict(err), "stale scale write must conflict: %v", err)
	assert.EqualValues(t, 3, getTestLWSReplicas(base, testNamespace, lws.Name))

	// Enabling subroles between the baseline read and patch must also conflict.
	concurrentChange = func(current *leaderworkersetv1.LeaderWorkerSet) {
		current.Annotations[subRoleReplicasAnnotation] = `{"pool":3}`
		current.Annotations[disaggregatedsetv1.InitialSubRoleReplicasAnnotationKey] = `{"pool":4}`
	}
	err = NewLeaderWorkerSetManager(c).UpdateInitialReplicas(t.Context(), ds, lws, 5)
	require.True(t, apierrors.IsConflict(err), "ordinary baseline must conflict with live enablement: %v", err)
	live := &leaderworkersetv1.LeaderWorkerSet{}
	require.NoError(t, base.Get(t.Context(), client.ObjectKeyFromObject(lws), live))
	assert.Equal(t, "4", live.Annotations[disaggregatedsetv1.InitialReplicasAnnotationKey])
	assert.Equal(t, "4", lws.Annotations[disaggregatedsetv1.InitialReplicasAnnotationKey])
	assert.Equal(t, `{"pool":4}`, live.Annotations[disaggregatedsetv1.InitialSubRoleReplicasAnnotationKey])
}

func TestScaleRequiresObservedIdentityAndSpec(t *testing.T) {
	for _, change := range []string{"smaller Spec", "larger Spec", "generation", "replacement", "deleting", "sub-roles enabled", "status only"} {
		t.Run(change, func(t *testing.T) {
			observed := revisionLWS("A", testRolePrefill, 4, 4, time.Now(), 4)
			live := observed.DeepCopy()
			switch change {
			case "smaller Spec":
				live.Spec.Replicas = ptr.To[int32](1)
			case "larger Spec":
				live.Spec.Replicas = ptr.To[int32](6)
			case "generation":
				live.Generation++
			case "replacement":
				live.UID = "replacement"
			case "deleting":
				live.DeletionTimestamp, live.Finalizers = ptr.To(metav1.Now()), []string{"test/hold"}
			case "sub-roles enabled":
				live.Annotations[subRoleReplicasAnnotation] = `{"pool":4}`
				live.Annotations[disaggregatedsetv1.InitialSubRoleReplicasAnnotationKey] = `{"pool":4}`
			case "status only":
				live.Status.ReadyReplicas = 3
			}
			c := newTestClient(live)
			ds := newTwoRoleTestDisaggregatedSet([2]int32{4, 4}, [2]int{1, 1}, [2]int{})
			err := NewLeaderWorkerSetManager(c).Scale(t.Context(), ds, observed, 3)
			if change == "status only" {
				require.NoError(t, err)
				assert.EqualValues(t, 3, getTestLWSReplicas(c, testNamespace, live.Name))
			} else {
				require.True(t, apierrors.IsConflict(err), "changed scale input must retry: %v", err)
				assert.Equal(t, getLWSReplicas(live), getTestLWSReplicas(c, testNamespace, live.Name))
			}
			if change == "sub-roles enabled" {
				require.ErrorIs(t, NewLeaderWorkerSetManager(c).UpdateInitialReplicas(t.Context(), ds, observed, 5), errReplicaGroupsPending)
				require.NoError(t, c.Get(t.Context(), client.ObjectKeyFromObject(live), live))
				assert.Equal(t, "4", live.Annotations[disaggregatedsetv1.InitialReplicasAnnotationKey])
				assert.Equal(t, "4", observed.Annotations[disaggregatedsetv1.InitialReplicasAnnotationKey])
				assert.Equal(t, `{"pool":4}`, live.Annotations[disaggregatedsetv1.InitialSubRoleReplicasAnnotationKey])
			}
		})
	}
}

// TestManagerScale tests the manager's Scale method.
func TestManagerScale(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, leaderworkersetv1.AddToScheme(scheme))
	ds := testManagerDS("test-deployment")

	t.Run("skips patch when already at desired scale", func(t *testing.T) {
		existingLWS := buildOwnedManagerTestLWS("test-lws", 5, ds)

		fakeClient := fake.NewClientBuilder().
			WithScheme(scheme).
			WithRuntimeObjects(existingLWS).
			Build()

		manager := NewLeaderWorkerSetManager(fakeClient)
		err := manager.Scale(context.Background(), ds, existingLWS, 5)

		require.NoError(t, err)
	})

	t.Run("scales to new replica count", func(t *testing.T) {
		existingLWS := buildOwnedManagerTestLWS("test-lws", 3, ds)

		fakeClient := fake.NewClientBuilder().
			WithScheme(scheme).
			WithRuntimeObjects(existingLWS).
			Build()

		manager := NewLeaderWorkerSetManager(fakeClient)
		err := manager.Scale(context.Background(), ds, existingLWS, 5)

		require.NoError(t, err)
	})

	t.Run("returns error when LWS not found", func(t *testing.T) {
		fakeClient := fake.NewClientBuilder().
			WithScheme(scheme).
			Build()

		manager := NewLeaderWorkerSetManager(fakeClient)
		err := manager.Scale(context.Background(), ds, buildOwnedManagerTestLWS("nonexistent", 1, ds), 5)

		require.Error(t, err)
	})

	// Regression test for #981: a same-named LWS that exists but is owned by a
	// different DisaggregatedSet (e.g. left over from a same-named
	// DisaggregatedSet that was deleted and recreated before GC ran) must be
	// refused, not mutated.
	t.Run("refuses to scale a foreign-owned LWS with the same name", func(t *testing.T) {
		foreignDS := testManagerDS("some-other-ds")
		foreignLWS := buildOwnedManagerTestLWS("test-lws", 3, foreignDS)

		fakeClient := fake.NewClientBuilder().
			WithScheme(scheme).
			WithRuntimeObjects(foreignLWS).
			Build()

		manager := NewLeaderWorkerSetManager(fakeClient)
		err := manager.Scale(context.Background(), ds, foreignLWS, 5)
		require.Error(t, err, "scaling a foreign-owned LWS must be refused")

		var got leaderworkersetv1.LeaderWorkerSet
		require.NoError(t, fakeClient.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "test-lws"}, &got))
		assert.EqualValues(t, 3, *got.Spec.Replicas, "the foreign LWS must not have been mutated")
	})
}

func TestManagerUpdateInitialReplicas(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, leaderworkersetv1.AddToScheme(scheme))
	ds := testManagerDS("test-deployment")

	t.Run("synchronizes the supplied object when the stored value is already correct", func(t *testing.T) {
		storedLWS := buildOwnedManagerTestLWS("test-lws", 3, ds)
		storedLWS.Annotations = map[string]string{disaggregatedsetv1.InitialReplicasAnnotationKey: "5"}
		observedLWS := storedLWS.DeepCopy()
		observedLWS.Annotations = nil

		fakeClient := fake.NewClientBuilder().
			WithScheme(scheme).
			WithRuntimeObjects(storedLWS).
			Build()

		manager := NewLeaderWorkerSetManager(fakeClient)
		require.NoError(t, manager.UpdateInitialReplicas(context.Background(), ds, observedLWS, 5))
		assert.Equal(t, "5", observedLWS.Annotations[disaggregatedsetv1.InitialReplicasAnnotationKey])
	})

	t.Run("updates Kubernetes and the supplied object", func(t *testing.T) {
		existingLWS := buildOwnedManagerTestLWS("test-lws", 3, ds)
		existingLWS.Annotations = map[string]string{
			disaggregatedsetv1.InitialReplicasAnnotationKey: "5",
			"other-key": "other-value",
		}

		fakeClient := fake.NewClientBuilder().
			WithScheme(scheme).
			WithRuntimeObjects(existingLWS).
			Build()

		manager := NewLeaderWorkerSetManager(fakeClient)
		require.NoError(t, manager.UpdateInitialReplicas(context.Background(), ds, existingLWS, 10))
		assert.Equal(t, "10", existingLWS.Annotations[disaggregatedsetv1.InitialReplicasAnnotationKey])
		assert.Equal(t, "other-value", existingLWS.Annotations["other-key"])

		var stored leaderworkersetv1.LeaderWorkerSet
		require.NoError(t, fakeClient.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "test-lws"}, &stored))
		assert.Equal(t, "10", stored.Annotations[disaggregatedsetv1.InitialReplicasAnnotationKey])
		assert.Equal(t, "other-value", stored.Annotations["other-key"])
	})

	t.Run("sets annotation when not present", func(t *testing.T) {
		existingLWS := buildOwnedManagerTestLWS("test-lws", 3, ds)

		fakeClient := fake.NewClientBuilder().
			WithScheme(scheme).
			WithRuntimeObjects(existingLWS).
			Build()

		manager := NewLeaderWorkerSetManager(fakeClient)
		require.NoError(t, manager.UpdateInitialReplicas(context.Background(), ds, existingLWS, 5))
		assert.Equal(t, "5", existingLWS.Annotations[disaggregatedsetv1.InitialReplicasAnnotationKey])

		var stored leaderworkersetv1.LeaderWorkerSet
		require.NoError(t, fakeClient.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "test-lws"}, &stored))
		assert.Equal(t, "5", stored.Annotations[disaggregatedsetv1.InitialReplicasAnnotationKey])
	})

	t.Run("refuses a foreign-owned LWS", func(t *testing.T) {
		foreignDS := testManagerDS("some-other-ds")
		foreignLWS := buildOwnedManagerTestLWS("test-lws", 3, foreignDS)
		fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(foreignLWS).Build()

		manager := NewLeaderWorkerSetManager(fakeClient)
		err := manager.UpdateInitialReplicas(context.Background(), ds, foreignLWS, 5)
		require.Error(t, err)
	})

	t.Run("returns error when LWS not found", func(t *testing.T) {
		fakeClient := fake.NewClientBuilder().
			WithScheme(scheme).
			Build()

		manager := NewLeaderWorkerSetManager(fakeClient)
		missing := &leaderworkersetv1.LeaderWorkerSet{ObjectMeta: metav1.ObjectMeta{Name: "nonexistent", Namespace: "default"}}
		err := manager.UpdateInitialReplicas(context.Background(), ds, missing, 5)

		require.Error(t, err)
	})
}

// TestManagerCreate tests the manager's Create method.
func TestManagerCreate(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, leaderworkersetv1.AddToScheme(scheme))
	require.NoError(t, disaggregatedsetv1.AddToScheme(scheme))

	testDeploy, testRole := testManagerDSWithRole("test-deploy")
	testRevision := disaggregatedsetutils.ComputeRevision(testDeploy.Spec.Roles)
	testLWSName := disaggregatedsetutils.GenerateName(testDeploy.Name, 0, testRevision, testRole.Name)

	t.Run("returns nil when LWS already exists and is owned by this DS (idempotent)", func(t *testing.T) {
		// Represents a concurrent reconcile of this same DisaggregatedSet
		// having already created it.
		existingLWS := buildOwnedManagerTestLWS(testLWSName, 3, testDeploy)

		fakeClient := fake.NewClientBuilder().
			WithScheme(scheme).
			WithRuntimeObjects(existingLWS).
			Build()

		manager := NewLeaderWorkerSetManager(fakeClient)
		err := manager.Create(context.Background(), testDeploy, testRole, 0, testRevision, 3, nil)
		require.NoError(t, err) // Should not error, creation is idempotent
	})

	// Regression test for #981 (Copilot review on #983): a same-named LWS that
	// exists but is owned by a different DisaggregatedSet must not be silently
	// treated as "already created" — this DS's owned-object watches will never
	// fire for a foreign object, so silently returning nil here could leave the
	// role permanently missing an LWS. Create must error so the reconcile
	// requeues instead.
	t.Run("errors when the name is taken by a foreign-owned LWS", func(t *testing.T) {
		foreignDS := testManagerDS("some-other-ds")
		foreignLWS := buildOwnedManagerTestLWS(testLWSName, 3, foreignDS)

		fakeClient := fake.NewClientBuilder().
			WithScheme(scheme).
			WithRuntimeObjects(foreignLWS).
			Build()

		manager := NewLeaderWorkerSetManager(fakeClient)
		err := manager.Create(context.Background(), testDeploy, testRole, 0, testRevision, 3, nil)
		require.Error(t, err, "must not silently no-op when the name is taken by a foreign-owned LWS")
	})

	t.Run("successfully creates new LWS", func(t *testing.T) {
		fakeClient := fake.NewClientBuilder().
			WithScheme(scheme).
			Build()

		manager := NewLeaderWorkerSetManager(fakeClient)
		err := manager.Create(context.Background(), testDeploy, testRole, 0, testRevision, 3, nil)
		require.NoError(t, err)

		var lws leaderworkersetv1.LeaderWorkerSet
		require.NoError(t, fakeClient.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: testLWSName}, &lws))
		require.NotNil(t, lws.Spec.Replicas)
		assert.Zero(t, *lws.Spec.Replicas)
		initial, ok := disaggregatedsetutils.GetInitialReplicas(&lws)
		require.True(t, ok)
		assert.EqualValues(t, 3, initial)
	})

	t.Run("merges user metadata with system labels taking precedence", func(t *testing.T) {
		fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
		manager := NewLeaderWorkerSetManager(fakeClient)

		ds, role := testManagerDSWithRole("test")
		role.Labels = map[string]string{"kueue.x-k8s.io/queue-name": "q1", "app": "user-app"}
		role.Annotations = map[string]string{"note": "val"}
		revision := disaggregatedsetutils.ComputeRevision(ds.Spec.Roles)

		err := manager.Create(context.Background(), ds, role, 0, revision, 1, nil)
		require.NoError(t, err)

		var lws leaderworkersetv1.LeaderWorkerSet
		require.NoError(t, fakeClient.Get(context.Background(),
			client.ObjectKey{Name: disaggregatedsetutils.GenerateName(ds.Name, 0, revision, role.Name), Namespace: ds.Namespace}, &lws))

		require.Equal(t, "q1", lws.Labels["kueue.x-k8s.io/queue-name"]) // user label
		require.Equal(t, "test-0-prefill", lws.Labels["app"])           // generated system label wins
		require.Equal(t, "val", lws.Annotations["note"])                // user annotation
	})

	t.Run("injects placement affinity into leader and worker templates", func(t *testing.T) {
		fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
		manager := NewLeaderWorkerSetManager(fakeClient)

		ds, role := testManagerDSWithRole("test-deploy")
		ds.Spec.PlacementPolicy = &disaggregatedsetv1.PlacementPolicy{
			Type: disaggregatedsetv1.PlacementExclusiveTopology, Topology: "topology.example.com/rack",
		}
		role.Spec.LeaderWorkerTemplate.Size = ptr.To(int32(2))
		role.Spec.LeaderWorkerTemplate.LeaderTemplate = &corev1.PodTemplateSpec{}
		role.Spec.LeaderWorkerTemplate.WorkerTemplate = corev1.PodTemplateSpec{}
		revision := disaggregatedsetutils.ComputeRevision(ds.Spec.Roles)

		err := manager.Create(context.Background(), ds, role, 1, revision, 2, nil)
		require.NoError(t, err)

		var lws leaderworkersetv1.LeaderWorkerSet
		require.NoError(t, fakeClient.Get(context.Background(),
			client.ObjectKey{Name: disaggregatedsetutils.GenerateName(ds.Name, 1, revision, role.Name), Namespace: ds.Namespace}, &lws))

		// Both the leader and worker templates must carry the injected placement terms.
		for name, tmpl := range map[string]*corev1.PodTemplateSpec{
			"leader": lws.Spec.LeaderWorkerTemplate.LeaderTemplate,
			"worker": &lws.Spec.LeaderWorkerTemplate.WorkerTemplate,
		} {
			require.NotNil(t, tmpl.Spec.Affinity, "%s affinity", name)
			require.NotNil(t, tmpl.Spec.Affinity.PodAffinity, "%s podAffinity", name)
			require.NotNil(t, tmpl.Spec.Affinity.PodAntiAffinity, "%s podAntiAffinity", name)

			affTerms := tmpl.Spec.Affinity.PodAffinity.RequiredDuringSchedulingIgnoredDuringExecution
			require.Len(t, affTerms, 1, "%s podAffinity terms", name)
			require.Equal(t, "topology.example.com/rack", affTerms[0].TopologyKey, "%s topologyKey", name)
			require.Equal(t, []metav1.LabelSelectorRequirement{
				{Key: disaggregatedsetv1.SetNameLabelKey, Operator: metav1.LabelSelectorOpIn, Values: []string{"test-deploy"}},
				{Key: disaggregatedsetv1.SliceLabelKey, Operator: metav1.LabelSelectorOpIn, Values: []string{"1"}},
			}, affTerms[0].LabelSelector.MatchExpressions, "%s podAffinity selector", name)

			// ExclusiveTopology => same-set spread + cross-set exclusion = two anti-affinity terms.
			require.Len(t, tmpl.Spec.Affinity.PodAntiAffinity.RequiredDuringSchedulingIgnoredDuringExecution, 2, "%s podAntiAffinity terms", name)
		}
	})

	t.Run("no affinity injected without a placement policy", func(t *testing.T) {
		fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
		manager := NewLeaderWorkerSetManager(fakeClient)

		ds, role := testManagerDSWithRole("test-deploy")
		role.Spec.LeaderWorkerTemplate.Size = ptr.To(int32(2))
		role.Spec.LeaderWorkerTemplate.LeaderTemplate = &corev1.PodTemplateSpec{}
		revision := disaggregatedsetutils.ComputeRevision(ds.Spec.Roles)

		err := manager.Create(context.Background(), ds, role, 0, revision, 1, nil)
		require.NoError(t, err)

		var lws leaderworkersetv1.LeaderWorkerSet
		require.NoError(t, fakeClient.Get(context.Background(),
			client.ObjectKey{Name: disaggregatedsetutils.GenerateName(ds.Name, 0, revision, role.Name), Namespace: ds.Namespace}, &lws))
		require.Nil(t, lws.Spec.LeaderWorkerTemplate.WorkerTemplate.Spec.Affinity, "worker affinity")
		require.Nil(t, lws.Spec.LeaderWorkerTemplate.LeaderTemplate.Spec.Affinity, "leader affinity")
	})
}

// TestComputeRevision tests the disaggregatedsetutils.ComputeRevision function.
func TestComputeRevision(t *testing.T) {
	t.Run("returns consistent revision for same inputs", func(t *testing.T) {
		roles := []disaggregatedsetv1.DisaggregatedRoleSpec{
			{
				Name: "prefill",
				LeaderWorkerSetTemplateSpec: leaderworkersetv1.LeaderWorkerSetTemplateSpec{Spec: leaderworkersetv1.LeaderWorkerSetSpec{
					Replicas: ptr.To(int32(2)),
					LeaderWorkerTemplate: leaderworkersetv1.LeaderWorkerTemplate{
						Size: ptr.To(int32(1)),
					},
				}},
			},
			{
				Name: "decode",
				LeaderWorkerSetTemplateSpec: leaderworkersetv1.LeaderWorkerSetTemplateSpec{Spec: leaderworkersetv1.LeaderWorkerSetSpec{
					Replicas: ptr.To(int32(3)),
					LeaderWorkerTemplate: leaderworkersetv1.LeaderWorkerTemplate{
						Size: ptr.To(int32(1)),
					},
				}},
			},
		}

		revision1 := disaggregatedsetutils.ComputeRevision(roles)
		revision2 := disaggregatedsetutils.ComputeRevision(roles)

		require.Equal(t, revision1, revision2)
		require.Len(t, revision1, 8) // Truncated to 8 characters
	})

	t.Run("returns different revision for different Size", func(t *testing.T) {
		roles1 := []disaggregatedsetv1.DisaggregatedRoleSpec{
			{
				Name: "prefill",
				LeaderWorkerSetTemplateSpec: leaderworkersetv1.LeaderWorkerSetTemplateSpec{Spec: leaderworkersetv1.LeaderWorkerSetSpec{
					Replicas: ptr.To(int32(2)),
					LeaderWorkerTemplate: leaderworkersetv1.LeaderWorkerTemplate{
						Size: ptr.To(int32(1)),
					},
				}},
			},
			{
				Name: "decode",
				LeaderWorkerSetTemplateSpec: leaderworkersetv1.LeaderWorkerSetTemplateSpec{Spec: leaderworkersetv1.LeaderWorkerSetSpec{
					Replicas: ptr.To(int32(3)),
					LeaderWorkerTemplate: leaderworkersetv1.LeaderWorkerTemplate{
						Size: ptr.To(int32(1)),
					},
				}},
			},
		}
		roles2 := []disaggregatedsetv1.DisaggregatedRoleSpec{
			{
				Name: "prefill",
				LeaderWorkerSetTemplateSpec: leaderworkersetv1.LeaderWorkerSetTemplateSpec{Spec: leaderworkersetv1.LeaderWorkerSetSpec{
					Replicas: ptr.To(int32(2)),
					LeaderWorkerTemplate: leaderworkersetv1.LeaderWorkerTemplate{
						Size: ptr.To(int32(2)), // Different
					},
				}},
			},
			{
				Name: "decode",
				LeaderWorkerSetTemplateSpec: leaderworkersetv1.LeaderWorkerSetTemplateSpec{Spec: leaderworkersetv1.LeaderWorkerSetSpec{
					Replicas: ptr.To(int32(3)),
					LeaderWorkerTemplate: leaderworkersetv1.LeaderWorkerTemplate{
						Size: ptr.To(int32(1)),
					},
				}},
			},
		}

		revision1 := disaggregatedsetutils.ComputeRevision(roles1)
		revision2 := disaggregatedsetutils.ComputeRevision(roles2)

		require.NotEqual(t, revision1, revision2)
	})

	t.Run("returns different revision for different role names", func(t *testing.T) {
		roles1 := []disaggregatedsetv1.DisaggregatedRoleSpec{
			{
				Name: "prefill",
				LeaderWorkerSetTemplateSpec: leaderworkersetv1.LeaderWorkerSetTemplateSpec{Spec: leaderworkersetv1.LeaderWorkerSetSpec{
					Replicas: ptr.To(int32(2)),
					LeaderWorkerTemplate: leaderworkersetv1.LeaderWorkerTemplate{
						Size: ptr.To(int32(1)),
					},
				}},
			},
			{
				Name: "decode",
				LeaderWorkerSetTemplateSpec: leaderworkersetv1.LeaderWorkerSetTemplateSpec{Spec: leaderworkersetv1.LeaderWorkerSetSpec{
					Replicas: ptr.To(int32(3)),
					LeaderWorkerTemplate: leaderworkersetv1.LeaderWorkerTemplate{
						Size: ptr.To(int32(1)),
					},
				}},
			},
		}
		roles2 := []disaggregatedsetv1.DisaggregatedRoleSpec{
			{
				Name: "other-role",
				LeaderWorkerSetTemplateSpec: leaderworkersetv1.LeaderWorkerSetTemplateSpec{Spec: leaderworkersetv1.LeaderWorkerSetSpec{
					Replicas: ptr.To(int32(2)),
					LeaderWorkerTemplate: leaderworkersetv1.LeaderWorkerTemplate{
						Size: ptr.To(int32(1)),
					},
				}},
			},
			{
				Name: "decode",
				LeaderWorkerSetTemplateSpec: leaderworkersetv1.LeaderWorkerSetTemplateSpec{Spec: leaderworkersetv1.LeaderWorkerSetSpec{
					Replicas: ptr.To(int32(3)),
					LeaderWorkerTemplate: leaderworkersetv1.LeaderWorkerTemplate{
						Size: ptr.To(int32(1)),
					},
				}},
			},
		}

		revision1 := disaggregatedsetutils.ComputeRevision(roles1)
		revision2 := disaggregatedsetutils.ComputeRevision(roles2)

		require.NotEqual(t, revision1, revision2)
	})

	t.Run("handles empty roles slice", func(t *testing.T) {
		roles := []disaggregatedsetv1.DisaggregatedRoleSpec{}

		revision := disaggregatedsetutils.ComputeRevision(roles)
		require.Len(t, revision, 8)
	})
}

// TestManagerListSliceBucketing verifies that List only places explicitly labeled
// LWS objects in a slice.
func TestManagerListSliceBucketing(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, leaderworkersetv1.AddToScheme(scheme))

	ds := wrappers.BuildDisaggregatedSet("test-deployment", "default").Obj()
	ownerRef := metav1.OwnerReference{
		APIVersion: disaggregatedsetv1.GroupVersion.String(),
		Kind:       "DisaggregatedSet",
		Name:       ds.Name,
		UID:        ds.UID,
		Controller: ptr.To(true),
	}
	sliced := func(name, slice string) *leaderworkersetv1.LeaderWorkerSet {
		return wrappers.BuildBasicLeaderWorkerSet(name, "default").Labels(map[string]string{
			disaggregatedsetv1.SetNameLabelKey: ds.Name,
			disaggregatedsetv1.RoleLabelKey:    "prefill",
			disaggregatedsetv1.SliceLabelKey:   slice,
		}).OwnerReference(ownerRef).Obj()
	}
	unlabeled := wrappers.BuildBasicLeaderWorkerSet("unlabeled", "default").Labels(map[string]string{
		disaggregatedsetv1.SetNameLabelKey: ds.Name,
		disaggregatedsetv1.RoleLabelKey:    "prefill",
	}).OwnerReference(ownerRef).Obj()
	// Same name/role labels, but not actually owned by ds (e.g. a leftover from a
	// same-named DisaggregatedSet that was deleted and recreated) — must never be
	// counted as one of ds's own replicas.
	unowned := wrappers.BuildBasicLeaderWorkerSet("unowned", "default").Labels(map[string]string{
		disaggregatedsetv1.SetNameLabelKey: ds.Name,
		disaggregatedsetv1.RoleLabelKey:    "prefill",
		disaggregatedsetv1.SliceLabelKey:   "0",
	}).Obj()

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).
		WithRuntimeObjects(sliced("s0", "0"), sliced("s1", "1"), unlabeled, unowned).Build()
	manager := NewLeaderWorkerSetManager(fakeClient)

	names := func(list []*leaderworkersetv1.LeaderWorkerSet) []string {
		out := make([]string, 0, len(list))
		for _, l := range list {
			out = append(out, l.Name)
		}
		return out
	}

	t.Run("slice 0 excludes label-less and unowned objects", func(t *testing.T) {
		got, err := manager.ListForSlice(context.Background(), ds, 0, "")
		require.NoError(t, err)
		require.ElementsMatch(t, []string{"s0"}, names(got))
	})

	t.Run("slice 1 excludes label-less objects", func(t *testing.T) {
		got, err := manager.ListForSlice(context.Background(), ds, 1, "")
		require.NoError(t, err)
		require.ElementsMatch(t, []string{"s1"}, names(got))
	})

	t.Run("all slices excludes label-less and unowned objects", func(t *testing.T) {
		got, err := manager.ListAll(context.Background(), ds, "")
		require.NoError(t, err)
		require.ElementsMatch(t, []string{"s0", "s1"}, names(got))
	})
}

func TestManagerListRejectsDuplicateRoles(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, leaderworkersetv1.AddToScheme(scheme))
	ds := testManagerDS("test-deployment")
	first := buildOwnedManagerTestLWS("first", 3, ds).DeepCopy()
	first.Labels[disaggregatedsetv1.SliceLabelKey] = "0"

	for _, tc := range []struct {
		name   string
		change func(*leaderworkersetv1.LeaderWorkerSet)
		count  int
	}{
		{"same role", func(*leaderworkersetv1.LeaderWorkerSet) {}, 0},
		{"different slice", func(lws *leaderworkersetv1.LeaderWorkerSet) { lws.Labels[disaggregatedsetv1.SliceLabelKey] = "1" }, 2},
		{"different revision", func(lws *leaderworkersetv1.LeaderWorkerSet) {
			lws.Labels[disaggregatedsetv1.RevisionLabelKey] = "other"
		}, 2},
		{"different role", func(lws *leaderworkersetv1.LeaderWorkerSet) { lws.Labels[disaggregatedsetv1.RoleLabelKey] = "decode" }, 2},
		{"unowned", func(lws *leaderworkersetv1.LeaderWorkerSet) { lws.OwnerReferences = nil }, 1},
		{"foreign owner", func(lws *leaderworkersetv1.LeaderWorkerSet) {
			lws.OwnerReferences = []metav1.OwnerReference{ownerRefFor(testManagerDS("other-ds"))}
		}, 1},
		{"stale owner", func(lws *leaderworkersetv1.LeaderWorkerSet) { lws.OwnerReferences[0].UID = "previous-ds" }, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			second := first.DeepCopy()
			second.Name = "second"
			tc.change(second)
			manager := NewLeaderWorkerSetManager(fake.NewClientBuilder().WithScheme(scheme).WithObjects(first, second).Build())
			got, err := manager.ListAll(t.Context(), ds, "")
			if tc.count != 0 {
				require.NoError(t, err)
				require.Len(t, got, tc.count)
				return
			}
			require.ErrorContains(t, err, `LeaderWorkerSets "first" and "second" claim the same slice "0", revision "abc123", role "prefill"`)
			require.Nil(t, got, "do not return a partial list that hides a duplicate")
			_, err = manager.ListForSlice(t.Context(), ds, 0, "")
			require.Error(t, err, "cleanup must reject the same ambiguous role")
			old, target, err := manager.GetRevisionRolesList(t.Context(), ds, 0, "abc123")
			require.Error(t, err, "live rollout discovery must not silently overwrite a role")
			require.Nil(t, old)
			require.Nil(t, target)
		})
	}
}

// TestManagerGetForRoleIgnoresForeignOwnedLWS is a regression test for #981:
// GetForRole must treat a same-named LWS as absent when it is owned by a different
// DisaggregatedSet (e.g. left over from a same-named DisaggregatedSet that was
// deleted and recreated before GC ran), rather than returning it for the caller
// to scale.
func TestManagerGetForRoleIgnoresForeignOwnedLWS(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, leaderworkersetv1.AddToScheme(scheme))

	ds := testManagerDS("test-deployment")
	foreignDS := testManagerDS("some-other-ds")
	const revision, role = "abc123", "prefill"

	t.Run("slice-aware name occupied by a foreign LWS reads as absent", func(t *testing.T) {
		slice := 0
		name := disaggregatedsetutils.GenerateName(ds.Name, slice, revision, role)
		foreignLWS := buildOwnedManagerTestLWS(name, 3, foreignDS)

		fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(foreignLWS).Build()
		manager := NewLeaderWorkerSetManager(fakeClient)

		got, err := manager.GetForRole(context.Background(), ds, slice, revision, role)
		require.NoError(t, err)
		assert.Nil(t, got, "a foreign-owned LWS at the generated name must not be returned")
	})

	t.Run("pre-slices name is not consulted", func(t *testing.T) {
		slice := 0
		oldName := ds.Name + "-" + revision + "-" + role
		oldLWS := buildOwnedManagerTestLWS(oldName, 3, ds)

		fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(oldLWS).Build()
		manager := NewLeaderWorkerSetManager(fakeClient)

		got, err := manager.GetForRole(context.Background(), ds, slice, revision, role)
		require.NoError(t, err)
		assert.Nil(t, got, "an LWS under the pre-slices name must not be returned")
	})

	t.Run("owned LWS at the generated name is still returned normally", func(t *testing.T) {
		slice := 0
		name := disaggregatedsetutils.GenerateName(ds.Name, slice, revision, role)
		ownedLWS := buildOwnedManagerTestLWS(name, 3, ds)

		fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(ownedLWS).Build()
		manager := NewLeaderWorkerSetManager(fakeClient)

		got, err := manager.GetForRole(context.Background(), ds, slice, revision, role)
		require.NoError(t, err)
		require.NotNil(t, got)
		assert.Equal(t, name, got.Name)
	})
}

func TestManagerCreateGroupIdentityPassthrough(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, leaderworkersetv1.AddToScheme(scheme))
	require.NoError(t, disaggregatedsetv1.AddToScheme(scheme))

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	manager := NewLeaderWorkerSetManager(fakeClient)

	ds, role := testManagerDSWithRole("test-deploy")
	role.Spec.GroupIdentity = leaderworkersetv1.GroupIdentityHash
	role.Spec.LeaderWorkerTemplate.Size = ptr.To(int32(2))
	revision := disaggregatedsetutils.ComputeRevision(ds.Spec.Roles)

	require.NoError(t, manager.Create(context.Background(), ds, role, 0, revision, 2, nil))

	lwsName := disaggregatedsetutils.GenerateName(ds.Name, 0, revision, role.Name)
	lws, err := manager.Get(context.Background(), ds, lwsName)
	require.NoError(t, err)
	require.NotNil(t, lws)
	require.Equal(t, leaderworkersetv1.GroupIdentityHash, lws.Spec.GroupIdentity)
}

func TestComputeRevisionGroupIdentity(t *testing.T) {
	buildRoles := func(groupIdentity leaderworkersetv1.GroupIdentityType) []disaggregatedsetv1.DisaggregatedRoleSpec {
		return []disaggregatedsetv1.DisaggregatedRoleSpec{
			{
				Name: "prefill",
				LeaderWorkerSetTemplateSpec: leaderworkersetv1.LeaderWorkerSetTemplateSpec{Spec: leaderworkersetv1.LeaderWorkerSetSpec{
					GroupIdentity: groupIdentity,
					LeaderWorkerTemplate: leaderworkersetv1.LeaderWorkerTemplate{
						Size: ptr.To(int32(1)),
					},
				}},
			},
		}
	}

	t.Run("empty and explicit Ordinal produce the same revision", func(t *testing.T) {
		// Objects persisted before the field existed must keep their revision
		// once the API server starts defaulting groupIdentity to Ordinal.
		require.Equal(t,
			disaggregatedsetutils.ComputeRevisionV1(buildRoles("")),
			disaggregatedsetutils.ComputeRevisionV1(buildRoles(leaderworkersetv1.GroupIdentityOrdinal)))
	})

	t.Run("Hash produces a different revision", func(t *testing.T) {
		require.NotEqual(t,
			disaggregatedsetutils.ComputeRevision(buildRoles(leaderworkersetv1.GroupIdentityOrdinal)),
			disaggregatedsetutils.ComputeRevision(buildRoles(leaderworkersetv1.GroupIdentityHash)))
	})
}

func testPendingWrites(reader client.Reader) *pendingWrites {
	return &pendingWrites{Reader: reader, writes: make(map[client.ObjectKey]*pendingWrite)}
}

func newObservedTestManager(t *testing.T, objects ...client.Object) (*LeaderWorkerSetManager, client.WithWatch, client.WithWatch) {
	t.Helper()
	writer, cached := newTestClient(objects...), newTestClient(objects...)
	manager := NewLeaderWorkerSetManager(writeOnlyTestClient(t, writer))
	manager.reader, manager.pending = cached, testPendingWrites(cached)
	return manager, writer, cached
}

func readTestLWS(t *testing.T, reader client.Reader, object client.Object) *leaderworkersetv1.LeaderWorkerSet {
	t.Helper()
	lws := &leaderworkersetv1.LeaderWorkerSet{}
	require.NoError(t, reader.Get(t.Context(), client.ObjectKeyFromObject(object), lws))
	return lws
}

// Deliver one LWS update without aliasing the fake informer to the writer.
func deliverTestLWSUpdate(t *testing.T, writer client.Reader, cached client.Client, object client.Object) *leaderworkersetv1.LeaderWorkerSet {
	t.Helper()
	current := readTestLWS(t, writer, object)
	previous := readTestLWS(t, cached, object)
	if current.ResourceVersion != previous.ResourceVersion {
		current.ResourceVersion = previous.ResourceVersion
		require.NoError(t, cached.Update(t.Context(), current))
	}
	return current
}

func writeOnlyTestClient(t *testing.T, c client.WithWatch) client.WithWatch {
	t.Helper()
	return interceptor.NewClient(c, interceptor.Funcs{
		Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
			t.Error("observation bypassed the informer")
			return errors.New("direct GET")
		},
		List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
			t.Error("observation bypassed the informer")
			return errors.New("direct LIST")
		},
	})
}

func TestTakeoverRejectsLateScaleDown(t *testing.T) {
	for _, identity := range []leaderworkersetv1.GroupIdentityType{leaderworkersetv1.GroupIdentityOrdinal, leaderworkersetv1.GroupIdentityHash} {
		for _, order := range []string{"old-first", "takeover-first", "crash-during-takeover"} {
			t.Run(string(identity)+"/"+order, func(t *testing.T) {
				ds := newTestDisaggregatedSet(makeRoleSpec(testRolePrefill, 4, corev1.PodSpec{}, intstr.FromInt(0), intstr.FromInt(1)))
				var objects []client.Object
				for i, revision := range []string{"a", "b", "c"} {
					count := int32(2)
					if revision == "c" {
						count = 0
					}
					lws := revisionLWS(revision, testRolePrefill, count, count, time.Unix(int64(i+1), 0), 4)
					lws.Spec.GroupIdentity, lws.Spec.LeaderWorkerTemplate.Size = identity, ptr.To[int32](1)
					objects = append(objects, replicaGroupObjects(lws, count, count)...)
				}
				base := newTestClient(objects...)
				var release func() error
				oldWriter := interceptor.NewClient(base, interceptor.Funcs{
					Patch: func(ctx context.Context, c client.WithWatch, object client.Object, patch client.Patch, opts ...client.PatchOption) error {
						payload, err := patch.Data(object)
						require.NoError(t, err)
						copy := object.DeepCopyObject().(client.Object)
						release = func() error { return c.Patch(t.Context(), copy, client.RawPatch(patch.Type(), payload), opts...) }
						return errors.New("old controller lost the response")
					},
				})
				old := NewLeaderWorkerSetManager(oldWriter)
				old.reader, old.pending = base, testPendingWrites(base)
				b := &leaderworkersetv1.LeaderWorkerSet{}
				require.NoError(t, base.Get(t.Context(), client.ObjectKey{Namespace: ds.Namespace, Name: "test-0-b-prefill"}, b))
				require.Error(t, old.Scale(t.Context(), ds, b, 1))
				require.NotNil(t, release)
				if order == "old-first" {
					require.NoError(t, release())
				}
				newController := func() *DisaggregatedSetReconciler {
					r := newTestReconciler(base)
					r.LWSManager = NewLeaderWorkerSetManager(base)
					r.LWSManager.pending = testPendingWrites(base)
					return r
				}
				r := newController()
				if order == "crash-during-takeover" {
					// A second crash loses the new receipt expectations too.
					require.ErrorIs(t, r.LWSManager.prepareSlice(t.Context(), ds, 0), errReplicaGroupsPending)
					r = newController()
				}
				for range 3 {
					// The user rolled back to B: the planner would now drain A.
					_, err := r.reconcileSlice(t.Context(), r.createRollingUpdateExecutor(), ds, 0, "b", map[string]int{testRolePrefill: 4})
					if err != nil {
						require.ErrorIs(t, err, errReplicaGroupsPending)
					}
				}
				if order != "old-first" {
					require.True(t, apierrors.IsConflict(release()), "the original request cannot spend the allowance a second time")
				}
				wantA, wantB := 1, 2
				if order == "old-first" {
					wantA, wantB = 2, 1
				}
				require.EqualValues(t, wantA, getTestLWSReplicas(base, ds.Namespace, "test-0-a-prefill"))
				require.EqualValues(t, wantB, getTestLWSReplicas(base, ds.Namespace, "test-0-b-prefill"))
				// All original groups were Ready. These issued targets retain
				// three, instead of the pre-fix result A=1, B=1 (floor=3).
			})
		}
	}
}

func TestTakeoverWaitsForEveryReceipt(t *testing.T) {
	ds := newTestDisaggregatedSet()
	a := revisionLWS("old", testRolePrefill, 2, 2, time.Time{}, 2)
	b := revisionLWS("old", testRoleDecode, 2, 2, time.Time{}, 2)
	base := newTestClient(a, b)
	cached := newTestClient(a.DeepCopy(), b.DeepCopy())
	patches := 0
	writer := interceptor.NewClient(base, interceptor.Funcs{
		Patch: func(ctx context.Context, c client.WithWatch, object client.Object, patch client.Patch, opts ...client.PatchOption) error {
			payload, err := patch.Data(object)
			require.NoError(t, err)
			require.NotContains(t, string(payload), "\"spec\"", "takeover only changes metadata")
			require.Contains(t, string(payload), "\"resourceVersion\"")
			patches++
			return c.Patch(ctx, object, patch, opts...)
		},
	})
	manager := NewLeaderWorkerSetManager(writer)
	manager.reader, manager.pending = cached, testPendingWrites(cached)
	require.ErrorIs(t, manager.prepareSlice(t.Context(), ds, 0), errReplicaGroupsPending)
	require.Equal(t, 2, patches)
	require.Empty(t, manager.pending.initialized, "write responses do not release the barrier")
	require.NoError(t, manager.prepareSlice(t.Context(), ds, 1), "other slices are independent")
	for i, lws := range []*leaderworkersetv1.LeaderWorkerSet{a, b} {
		stored := deliverTestLWSUpdate(t, base, cached, lws)
		require.Equal(t, lws.Spec, stored.Spec)
		require.Len(t, stored.Annotations, len(lws.Annotations)+1, "reuse the receipt; no takeover annotation")
		err := manager.prepareSlice(t.Context(), ds, 0)
		if i == 0 {
			require.ErrorIs(t, err, errReplicaGroupsPending)
		} else {
			require.NoError(t, err)
		}
	}
	require.NoError(t, manager.prepareSlice(t.Context(), ds, 0))
	require.Equal(t, 2, patches, "takeover is once per UID, not every reconcile")
	manager.pending.forget(client.ObjectKeyFromObject(ds))
	require.Empty(t, manager.pending.initialized)
}

func TestObserveReadinessRejectsPartialRoleMaps(t *testing.T) {
	a := revisionLWS("old", testRolePrefill, 1, 1, time.Time{}, 1)
	b := revisionLWS("old", testRoleDecode, 1, 1, time.Time{}, 1)
	a.Spec.LeaderWorkerTemplate.Size, b.Spec.LeaderWorkerTemplate.Size = ptr.To[int32](1), ptr.To[int32](1)
	objects := append(replicaGroupObjects(a, 1, 1), replicaGroupObjects(b, 1, 1)...)
	objects[len(objects)-1].SetAnnotations(nil) // The second role has invalid group metadata.
	manager := NewLeaderWorkerSetManager(newTestClient(objects...))
	readiness, err := manager.observeCachedReadiness(t.Context(), []*leaderworkersetv1.LeaderWorkerSet{a, b})
	require.ErrorContains(t, err, "invalid group size")
	require.Nil(t, readiness, "the first role's Ready credit must not escape a failed observation")
}

func TestWriteReceiptsWaitForTheWholeSlice(t *testing.T) {
	a := revisionLWS("old", testRolePrefill, 4, 4, time.Time{}, 4)
	b := revisionLWS("old", testRoleDecode, 4, 4, time.Time{}, 4)
	manager, base, cached := newObservedTestManager(t, a, b)
	o := manager.pending
	ds := newTestDisaggregatedSet()
	// Two writes in one already-planned step are allowed. The next step must
	// observe BOTH, even if it would only mutate a different role.
	for _, lws := range []*leaderworkersetv1.LeaderWorkerSet{a, b} {
		require.NoError(t, manager.Scale(t.Context(), ds, lws, 3))
	}
	require.ErrorIs(t, o.observed(t.Context(), ds.UID, "0"), errReplicaGroupsPending)
	require.NoError(t, o.observed(t.Context(), ds.UID, "1"), "another slice is independent")
	require.NoError(t, o.observed(t.Context(), types.UID("another-owner"), "0"))
	r := &DisaggregatedSetReconciler{LWSManager: manager}
	seed := r.seedForRole(t.Context(), ds, "current")
	_, err := seed(testRolePrefill)
	require.ErrorIs(t, err, errReplicaGroupsPending, "a new scaler must not freeze a stale count")
	for i, lws := range []*leaderworkersetv1.LeaderWorkerSet{a, b} {
		// The informer can skip the exact write response and deliver a later
		// status update. The receipt, not equality of resource versions, matters.
		deliverTestLWSUpdate(t, base, cached, lws)
		err := o.observed(t.Context(), ds.UID, "0")
		if i == 0 {
			require.ErrorIs(t, err, errReplicaGroupsPending)
		} else {
			require.NoError(t, err)
		}
	}
	require.Empty(t, o.writes)
	seed = r.seedForRole(t.Context(), ds, "current") // The pending result ended the previous reconcile.
	count, err := seed(testRolePrefill)
	require.NoError(t, err)
	require.EqualValues(t, 3, count)
}

func TestScalerSeedsShareOneLazyObservation(t *testing.T) {
	ds := newTestDisaggregatedSet()
	a := revisionLWS("current", testRolePrefill, 2, 2, time.Time{}, 2)
	b := revisionLWS("current", testRoleDecode, 3, 3, time.Time{}, 3)
	setSubRoleJSON(b, subRoleReplicasAnnotation, map[string]int{"a": 1, "b": 2})
	base := newTestClient(a, b)
	lists := 0
	reader := interceptor.NewClient(base, interceptor.Funcs{List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
		lists++
		return c.List(ctx, list, opts...)
	}})
	r := &DisaggregatedSetReconciler{LWSManager: NewLeaderWorkerSetManager(reader)}
	seed := r.seedForRole(t.Context(), ds, "current")
	require.Zero(t, lists, "existing scalers need no seed observation")
	for role, want := range map[string]int32{testRolePrefill: 2, testRoleDecode: 3, "decode/a": 1, "decode/b": 2, "missing": 1} {
		got, err := seed(role)
		require.NoError(t, err)
		require.Equal(t, want, got, role)
	}
	require.Equal(t, 1, lists)
	current := readTestLWS(t, base, a)
	current.Spec.Replicas = ptr.To[int32](6)
	require.NoError(t, base.Update(t.Context(), current))
	got, err := r.seedForRole(t.Context(), ds, "current")(testRolePrefill)
	require.NoError(t, err)
	require.EqualValues(t, 6, got, "the next reconcile takes a fresh snapshot")
	require.Equal(t, 2, lists)
}

func TestScalerSeedDistinguishesBootstrapFromDrainedCapacity(t *testing.T) {
	for _, identity := range []leaderworkersetv1.GroupIdentityType{leaderworkersetv1.GroupIdentityOrdinal, leaderworkersetv1.GroupIdentityHash} {
		for _, children := range []bool{false, true} {
			for _, stage := range []string{"created", "takeover", "policy write", "rejected growth", "grown then zero", "zero intent", "old empty", "running old", "other slice", "unmarked zero"} {
				t.Run(fmt.Sprintf("%s/children=%t/%s", identity, children, stage), func(t *testing.T) {
					ds, role := testManagerDSWithRole("bootstrap")
					role.Spec.GroupIdentity = identity
					initial, initialChildren := 4, map[string]int(nil)
					if stage == "zero intent" {
						initial = 0
					}
					if children {
						role.SubRoles = []disaggregatedsetv1.DisaggregatedSubRoleSpec{{Name: "a"}, {Name: "b"}}
						initialChildren = fitSubRoles(map[string]int{"a": 1, "b": 3}, initial)
					}
					base := newTestClient()
					manager := NewLeaderWorkerSetManager(base)
					manager.pending = testPendingWrites(base)
					require.NoError(t, manager.Create(t.Context(), ds, role, 0, "current", initial, initialChildren))
					current, err := manager.GetForRole(t.Context(), ds, 0, "current", role.Name)
					require.NoError(t, err)
					current.UID = "created-uid" // The fake client does not assign UIDs.
					require.NoError(t, base.Update(t.Context(), current))
					want, wantA, wantB := int32(initial), int32(min(initial, 1)), int32(max(initial-1, 0))
					switch stage {
					case "takeover":
						require.ErrorIs(t, manager.prepareSlice(t.Context(), ds, 0), errReplicaGroupsPending)
						require.NoError(t, manager.prepareSlice(t.Context(), ds, 0))
					case "policy write":
						require.NoError(t, manager.SyncGroupReplacementPolicy(t.Context(), current, leaderworkersetv1.GroupReplacementImmediate))
					case "rejected growth":
						manager.client = interceptor.NewClient(base, interceptor.Funcs{Patch: func(_ context.Context, _ client.WithWatch, _ client.Object, _ client.Patch, _ ...client.PatchOption) error {
							return apierrors.NewConflict(leaderworkersetv1.GroupVersion.WithResource("leaderworkersets").GroupResource(), current.Name, errors.New("concurrent write"))
						}})
						require.True(t, apierrors.IsConflict(manager.Scale(t.Context(), ds, current, 4)))
						require.True(t, isUnscaledCreation(readTestLWS(t, base, current)), "rejected growth cannot retire bootstrap intent")
					case "grown then zero":
						for _, replicas := range []int{4, 0} {
							current = readTestLWS(t, base, current)
							if children {
								state := &subRoleState{lws: current, issued: fitSubRoles(initialChildren, replicas), initial: initialChildren}
								require.ErrorIs(t, manager.saveSubRoles(t.Context(), state), errReplicaGroupsPending)
							} else {
								require.NoError(t, manager.Scale(t.Context(), ds, current, replicas))
							}
							require.NoError(t, manager.pending.observed(t.Context(), ds.UID, "0"))
						}
						want, wantA, wantB = 0, 0, 0
						require.False(t, isUnscaledCreation(readTestLWS(t, base, current)), "bootstrap cannot return after the first growth")
					case "old empty", "unmarked zero":
						if stage == "old empty" {
							current.Labels[disaggregatedsetv1.RevisionLabelKey] = "old"
						} else {
							delete(current.Annotations, observationReceiptAnnotation)
						}
						require.NoError(t, base.Update(t.Context(), current))
						want, wantA, wantB = 0, 0, 0
					case "running old", "other slice":
						old := current.DeepCopy()
						old.Name, old.UID, old.ResourceVersion = "old", "old-uid", ""
						old.Labels[disaggregatedsetv1.RevisionLabelKey] = "old"
						old.Spec.Replicas = ptr.To[int32](2)
						if stage == "other slice" {
							old.Labels[disaggregatedsetv1.SliceLabelKey] = "1"
							want, wantA, wantB = 6, 2, 4
						} else {
							want, wantA, wantB = 2, 1, 1
						}
						require.NoError(t, base.Create(t.Context(), old))
					}
					// A restart discards all local state; only persisted intent counts.
					r := &DisaggregatedSetReconciler{LWSManager: NewLeaderWorkerSetManager(base)}
					seed := r.seedForRole(t.Context(), ds, "current")
					wants := map[string]int32{role.Name: want}
					if children {
						wants[role.Name+"/a"], wants[role.Name+"/b"] = wantA, wantB
					}
					for name, expected := range wants {
						got, err := seed(name)
						require.NoError(t, err)
						require.Equal(t, expected, got, name)
					}
				})
			}
		}
	}
}

func TestWriteReceiptRecovery(t *testing.T) {
	for _, operation := range []string{"scale", "takeover"} {
		for _, tc := range []struct {
			name                             string
			rejected, applied, statusChanged bool
		}{
			{name: "rejected", rejected: true},
			{name: "unapplied"},
			{name: "applied with lost response", applied: true},
			{name: "unapplied then status changed", statusChanged: true},
			{name: "applied then status changed", applied: true, statusChanged: true},
		} {
			t.Run(operation+"/"+tc.name, func(t *testing.T) {
				lws := revisionLWS("old", testRolePrefill, 4, 4, time.Time{}, 4)
				base := newTestClient(lws)
				key := client.ObjectKeyFromObject(lws)
				cachedLWS := &leaderworkersetv1.LeaderWorkerSet{}
				require.NoError(t, base.Get(t.Context(), key, cachedLWS))
				cached := interceptor.NewClient(base, interceptor.Funcs{
					Get: func(_ context.Context, _ client.WithWatch, _ client.ObjectKey, object client.Object, _ ...client.GetOption) error {
						cachedLWS.DeepCopyInto(object.(*leaderworkersetv1.LeaderWorkerSet))
						return nil
					},
					List: func(_ context.Context, _ client.WithWatch, list client.ObjectList, _ ...client.ListOption) error {
						list.(*leaderworkersetv1.LeaderWorkerSetList).Items = []leaderworkersetv1.LeaderWorkerSet{*cachedLWS.DeepCopy()}
						return nil
					},
				})
				calls := 0
				writer := interceptor.NewClient(base, interceptor.Funcs{
					Patch: func(ctx context.Context, c client.WithWatch, object client.Object, patch client.Patch, opts ...client.PatchOption) error {
						calls++
						if calls == 1 {
							if tc.rejected {
								current := readTestLWS(t, base, lws)
								current.Status.ReadyReplicas++
								require.NoError(t, c.Status().Update(ctx, current))
								return c.Patch(ctx, object, patch, opts...)
							}
							if tc.applied {
								require.NoError(t, c.Patch(ctx, object.DeepCopyObject().(client.Object), patch, opts...))
							}
							return errors.New("response lost")
						}
						return c.Patch(ctx, object, patch, opts...)
					},
				})
				manager := NewLeaderWorkerSetManager(writer)
				manager.reader, manager.pending = cached, testPendingWrites(cached)
				ds := newTestDisaggregatedSet()
				issue := func() error {
					if operation == "scale" {
						return manager.Scale(t.Context(), ds, cachedLWS, 3)
					}
					err := manager.prepareSlice(t.Context(), ds, 0)
					if errors.Is(err, errReplicaGroupsPending) {
						return nil // Successful takeover writes wait for their receipt.
					}
					return err
				}
				require.Error(t, issue())
				if tc.rejected {
					require.Empty(t, manager.pending.writes, "a definitive rejection reserves nothing")
					require.True(t, apierrors.IsConflict(issue()), "the stale cache still conflicts")
					require.NoError(t, base.Get(t.Context(), key, cachedLWS))
					require.NoError(t, issue(), "a fresh observation allows a new decision")
				}

				current := &leaderworkersetv1.LeaderWorkerSet{}
				if tc.statusChanged {
					// Even an unapplied request now conflicts on every replay.
					require.NoError(t, base.Get(t.Context(), key, current))
					current.Status.Replicas++
					require.NoError(t, base.Status().Update(t.Context(), current))
					require.NotEqual(t, cachedLWS.ResourceVersion, current.ResourceVersion)
				}
				for range 3 {
					require.ErrorIs(t, manager.pending.observed(t.Context(), ds.UID, "0"), errReplicaGroupsPending,
						"neither retries nor conflicts can release the barrier while the cache is stale")
				}
				beforeObservation := calls
				require.NoError(t, base.Get(t.Context(), key, cachedLWS)) // Deliver the watch update.
				require.NoError(t, manager.pending.observed(t.Context(), ds.UID, "0"))
				require.Empty(t, manager.pending.writes)
				require.Equal(t, beforeObservation, calls, "observe the outcome; do not refresh and replay an old decision")
				if !tc.applied && tc.statusChanged {
					require.EqualValues(t, 4, getLWSReplicas(cachedLWS))
					// A new decision based on the current object can now make progress.
					require.NoError(t, issue())
					require.ErrorIs(t, manager.pending.observed(t.Context(), ds.UID, "0"), errReplicaGroupsPending)
				}
				require.NoError(t, base.Get(t.Context(), key, current))
				if operation == "takeover" {
					require.EqualValues(t, 4, getLWSReplicas(current), "takeover only changes metadata")
					current.DeepCopyInto(cachedLWS)
					require.NoError(t, manager.prepareSlice(t.Context(), ds, 0), "takeover recovers after the outcome is observed")
				} else {
					require.EqualValues(t, 3, getLWSReplicas(current))
				}
			})
		}
	}
}

func TestWriteReceiptRetiresDeletedChild(t *testing.T) {
	for _, state := range []string{"disappeared", "terminating", "replaced"} {
		t.Run(state, func(t *testing.T) {
			lws := revisionLWS("old", testRolePrefill, 4, 4, time.Time{}, 4)
			if state == "terminating" {
				lws.Finalizers = []string{"test.example/hold"}
			}
			manager, base, cached := newObservedTestManager(t, lws)
			ds := newTestDisaggregatedSet()
			require.NoError(t, manager.Scale(t.Context(), ds, lws, 3))
			retire := func(c client.WithWatch) {
				require.NoError(t, c.Delete(t.Context(), lws))
				if state == "replaced" {
					replacement := lws.DeepCopy()
					replacement.UID, replacement.ResourceVersion = "replacement-uid", ""
					replacement.Spec.Replicas = ptr.To[int32](7)
					require.NoError(t, c.Create(t.Context(), replacement))
				}
			}
			retire(base)
			require.ErrorIs(t, manager.pending.observed(t.Context(), ds.UID, "0"), errReplicaGroupsPending,
				"the cache has not observed either the write or deletion yet")
			retire(cached) // Observe deletion/replacement without ever seeing the receipt.
			require.NoError(t, manager.pending.observed(t.Context(), ds.UID, "0"))
			require.Empty(t, manager.pending.writes)

			current := &leaderworkersetv1.LeaderWorkerSet{}
			switch state {
			case "disappeared":
				role := &disaggregatedsetv1.DisaggregatedRoleSpec{Name: testRolePrefill}
				role.Spec = lws.Spec
				require.NoError(t, manager.Create(t.Context(), ds, role, 0, "old", 4, nil))
				require.NoError(t, base.Get(t.Context(), client.ObjectKeyFromObject(lws), current))
				require.Zero(t, getLWSReplicas(current), "recreation cannot start Pods before a fresh plan")
				require.NoError(t, manager.pending.observed(t.Context(), ds.UID, "0"))
			case "terminating":
				require.NoError(t, base.Get(t.Context(), client.ObjectKeyFromObject(lws), current))
				require.False(t, current.DeletionTimestamp.IsZero(), "physical disappearance is not required")
			case "replaced":
				require.True(t, apierrors.IsConflict(manager.Scale(t.Context(), ds, lws, 1)), "an old decision cannot change the replacement")
				require.NoError(t, base.Get(t.Context(), client.ObjectKeyFromObject(lws), current))
				require.Equal(t, types.UID("replacement-uid"), current.UID)
				require.EqualValues(t, 7, getLWSReplicas(current))
				require.ErrorIs(t, manager.prepareSlice(t.Context(), ds, 0), errReplicaGroupsPending, "a replacement needs its own takeover")
				deliverTestLWSUpdate(t, base, cached, current)
			}
			require.NoError(t, manager.prepareSlice(t.Context(), ds, 0), "deleted or replaced children cannot block takeover")
		})
	}
}

func TestDeletionReceiptDoesNotWaitForPhysicalRemoval(t *testing.T) {
	lws := revisionLWS("old", testRolePrefill, 0, 0, time.Time{}, 4)
	lws.Finalizers = []string{"hold-test-deletion"}
	manager, _, cached := newObservedTestManager(t, lws)
	o := manager.pending
	require.NoError(t, manager.deleteInForeground(t.Context(), lws))
	owner := metav1.GetControllerOf(lws).UID
	require.ErrorIs(t, o.observed(t.Context(), owner, "0"), errReplicaGroupsPending)
	require.NoError(t, cached.Delete(t.Context(), lws))
	require.NoError(t, o.observed(t.Context(), owner, "0"))
	current := readTestLWS(t, cached, lws)
	require.False(t, current.DeletionTimestamp.IsZero())
}

func TestCreateDefersGrowthUntilObserved(t *testing.T) {
	for _, identity := range []leaderworkersetv1.GroupIdentityType{leaderworkersetv1.GroupIdentityOrdinal, leaderworkersetv1.GroupIdentityHash} {
		for _, outcome := range []string{"unapplied", "response lost", "late commit", "deleted before observation"} {
			t.Run(string(identity)+"/"+outcome, func(t *testing.T) {
				ds, role := testManagerDSWithRole("create-test")
				role.Spec.GroupIdentity = identity
				base, cached := newTestClient(), newTestClient()
				var delayed client.Object
				calls := 0
				writer := interceptor.NewClient(base, interceptor.Funcs{Create: func(ctx context.Context, c client.WithWatch, object client.Object, opts ...client.CreateOption) error {
					calls++
					require.Zero(t, getLWSReplicas(object.(*leaderworkersetv1.LeaderWorkerSet)))
					if calls != 1 {
						return c.Create(ctx, object, opts...)
					}
					delayed = object.DeepCopyObject().(client.Object)
					if outcome == "response lost" || outcome == "deleted before observation" {
						require.NoError(t, c.Create(ctx, object, opts...))
					}
					if outcome == "deleted before observation" {
						return c.Delete(ctx, object)
					}
					return errors.New("create response lost")
				}})
				manager := NewLeaderWorkerSetManager(writeOnlyTestClient(t, writer))
				manager.reader, manager.pending = cached, testPendingWrites(cached)
				err := manager.Create(t.Context(), ds, role, 0, "obsolete", 4, nil)
				if outcome == "deleted before observation" {
					require.NoError(t, err)
				} else {
					require.Error(t, err)
				}
				require.NoError(t, manager.pending.observed(t.Context(), ds.UID, "0"))
				require.Empty(t, manager.pending.writes)
				require.Equal(t, 1, calls, "checking receipts must not replay an obsolete create")

				// The user selects another revision and replica count while the
				// informer is still empty. Even a later original commit stays empty.
				require.NoError(t, manager.Create(t.Context(), ds, role, 0, "latest", 2, nil))
				if outcome == "late commit" {
					require.NoError(t, base.Create(t.Context(), delayed))
				}
				all := &leaderworkersetv1.LeaderWorkerSetList{}
				require.NoError(t, base.List(t.Context(), all))
				for i := range all.Items {
					require.Zero(t, getLWSReplicas(&all.Items[i]), "no obsolete or unobserved workload can start Pods")
				}
				manager.reader, manager.pending.Reader = base, base // Deliver the current informer state.
				current, err := manager.GetForRole(t.Context(), ds, 0, "latest", role.Name)
				require.NoError(t, err)
				r := &DisaggregatedSetReconciler{LWSManager: manager}
				require.NoError(t, r.reconcileCurrentRevisionRole(t.Context(), ds, 0, current, role, "latest", map[string]int{role.Name: 2}))
				require.EqualValues(t, 2, getTestLWSReplicas(base, ds.Namespace, current.Name))
			})
		}
	}
}

func TestEmptyCreationRequeuesUntilObserved(t *testing.T) {
	for _, desired := range []int{0, 4} {
		t.Run(fmt.Sprint(desired), func(t *testing.T) {
			ds, role := testManagerDSWithRole("empty-create")
			manager, _, _ := newObservedTestManager(t)
			r := &DisaggregatedSetReconciler{LWSManager: manager}
			result, err := r.reconcileCurrentRevision(t.Context(), ds, 0, "current", nil, map[string]int{role.Name: desired})
			require.NoError(t, err)
			if desired > 0 {
				require.Equal(t, time.Second, result.RequeueAfter)
			} else {
				require.Zero(t, result.RequeueAfter, "a zero target needs no growth")
			}
			require.ErrorIs(t, manager.Create(t.Context(), ds, role, 0, "current", desired, nil), errReplicaGroupsPending,
				"AlreadyExists plus a lagging cache is a normal observation wait")
			require.Empty(t, manager.pending.initialized, "AlreadyExists must not mark an unobserved UID initialized")
		})
	}
}

func TestCreateSubRolesStartEmpty(t *testing.T) {
	for _, identity := range []leaderworkersetv1.GroupIdentityType{leaderworkersetv1.GroupIdentityOrdinal, leaderworkersetv1.GroupIdentityHash} {
		t.Run(string(identity), func(t *testing.T) {
			ds, role := testManagerDSWithRole("create-subroles")
			role.Spec.GroupIdentity = identity
			role.SubRoles = []disaggregatedsetv1.DisaggregatedSubRoleSpec{{Name: "a"}, {Name: "b"}}
			base := newTestClient()
			manager := NewLeaderWorkerSetManager(base)
			manager.pending = testPendingWrites(base)
			require.NoError(t, manager.Create(t.Context(), ds, role, 0, "current", 6, nil))
			current, err := manager.GetForRole(t.Context(), ds, 0, "current", role.Name)
			require.NoError(t, err)
			current.UID = "created-child"
			require.NoError(t, base.Update(t.Context(), current))
			for _, object := range replicaGroupObjects(current, 0, 0)[1:] {
				require.NoError(t, base.Create(t.Context(), object))
			}
			// A changed desired layout is metadata first, never permission to
			// start the original six groups while discovery catches up.
			_, err = manager.syncSubRoles(t.Context(), ds, current, subRoleUpdate{membership: map[string]int{"a": 1, "b": 2}})
			require.ErrorIs(t, err, errReplicaGroupsPending)
			current, err = manager.GetForRole(t.Context(), ds, 0, "current", role.Name)
			require.NoError(t, err)
			require.Zero(t, getLWSReplicas(current))
			issued, err := subRoleCounts(current, subRoleReplicasAnnotation)
			require.NoError(t, err)
			require.Equal(t, map[string]int{"a": 0, "b": 0}, issued)
			initial, ok := disaggregatedsetutils.GetInitialReplicas(current)
			require.True(t, ok)
			require.EqualValues(t, 6, initial, "the original intended baseline remains available to interrupted rollouts")
			r := &DisaggregatedSetReconciler{LWSManager: manager}
			for range 3 {
				require.NoError(t, manager.pending.observed(t.Context(), ds.UID, "0"))
				current, err = manager.GetForRole(t.Context(), ds, 0, "current", role.Name)
				require.NoError(t, err)
				err = r.reconcileCurrentRevisionRole(t.Context(), ds, 0, current, role, "current",
					map[string]int{role.Name: 3, role.Name + "/a": 1, role.Name + "/b": 2})
				if err != nil {
					require.ErrorIs(t, err, errReplicaGroupsPending)
				}
			}
			current, err = manager.GetForRole(t.Context(), ds, 0, "current", role.Name)
			require.NoError(t, err)
			require.EqualValues(t, 3, getLWSReplicas(current), "only the current child targets may start groups")
			issued, err = subRoleCounts(current, subRoleReplicasAnnotation)
			require.NoError(t, err)
			require.Equal(t, map[string]int{"a": 1, "b": 2}, issued)
		})
	}
}

func TestPendingWritesForgetOnlyTheirParent(t *testing.T) {
	parent := client.ObjectKey{Namespace: "ns", Name: "parent"}
	o := testPendingWrites(nil)
	o.writes[client.ObjectKey{Namespace: "ns", Name: "child"}] = &pendingWrite{parent: parent}
	o.forget(client.ObjectKey{Namespace: "other", Name: parent.Name})
	require.Len(t, o.writes, 1)
	o.forget(parent)
	require.Empty(t, o.writes)
}

func TestDisaggregatedSetRequest(t *testing.T) {
	ds := testManagerDS("parent")
	for _, tc := range []struct {
		name string
		edit func(*metav1.OwnerReference)
		want bool
	}{
		{"owned", func(*metav1.OwnerReference) {}, true},
		{"wrong kind", func(o *metav1.OwnerReference) { o.Kind = "Other" }, false},
		{"wrong API", func(o *metav1.OwnerReference) { o.APIVersion = "other/v1" }, false},
		{"not controller", func(o *metav1.OwnerReference) { o.Controller = ptr.To(false) }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			owner := ownerRefFor(ds)
			tc.edit(&owner)
			request, ok := disaggregatedSetRequest(&leaderworkersetv1.LeaderWorkerSet{ObjectMeta: metav1.ObjectMeta{Namespace: ds.Namespace, OwnerReferences: []metav1.OwnerReference{owner}}})
			require.Equal(t, tc.want, ok)
			if ok {
				require.Equal(t, client.ObjectKeyFromObject(ds), request.NamespacedName)
			}
		})
	}
}

type notificationQueue struct {
	workqueue.TypedRateLimitingInterface[ctrl.Request]
	added chan struct{}
}

func (q *notificationQueue) Add(request ctrl.Request) {
	q.TypedRateLimitingInterface.Add(request)
	q.added <- struct{}{}
}

type observationTransport struct {
	http.RoundTripper
	mu       sync.Mutex
	reads    []string
	requests []*http.Request
}

func (t *observationTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if request.Method == http.MethodGet {
		t.mu.Lock()
		t.requests = append(t.requests, request)
		if request.URL.Query().Get("watch") != "true" {
			t.reads = append(t.reads, request.URL.Path)
		}
		t.mu.Unlock()
	}
	return t.RoundTripper.RoundTrip(request)
}

// Real API/watch checks complement the deterministic stale-cache tests above.
// make test supplies KUBEBUILDER_ASSETS; no existing cluster is used.
func TestInformerObservationWithAPIServer(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("set KUBEBUILDER_ASSETS for the real API server test; make test supplies it")
	}
	environment := &envtest.Environment{UseExistingCluster: ptr.To(false), CRDDirectoryPaths: []string{filepath.Join("..", "..", "..", "config", "crd", "bases")}, ErrorIfCRDPathMissing: true}
	config, err := environment.Start()
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, environment.Stop()) })
	scheme := testSchemeForUnit()
	writer, err := client.NewWithWatch(config, client.Options{Scheme: scheme})
	require.NoError(t, err)
	start := func(t *testing.T, queue workqueue.TypedRateLimitingInterface[ctrl.Request]) (*LeaderWorkerSetManager, *observationTransport, func()) {
		cfg := rest.CopyConfig(config)
		trace := &observationTransport{}
		cfg.WrapTransport = func(rt http.RoundTripper) http.RoundTripper { trace.RoundTripper = rt; return trace }
		mgr, err := ctrl.NewManager(cfg, ctrl.Options{Scheme: scheme, Metrics: metricsserver.Options{BindAddress: "0"}})
		require.NoError(t, err)
		pending := testPendingWrites(nil)
		o, err := replicagroups.NewObserver(mgr)
		require.NoError(t, err)
		if queue == nil {
			queue = workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[ctrl.Request]())
			t.Cleanup(queue.ShutDown)
		}
		require.NoError(t, o.Source(disaggregatedSetRequest).Start(t.Context(), queue))
		require.True(t, o.NeedLeaderElection())
		ctx, cancel := context.WithCancel(t.Context())
		finished := make(chan error, 1)
		go func() { finished <- o.Start(ctx) }()
		stop := sync.OnceFunc(func() { cancel(); require.NoError(t, <-finished) })
		t.Cleanup(stop)
		wait, stopWaiting := context.WithTimeout(ctx, 20*time.Second)
		defer stopWaiting()
		require.NoError(t, o.List(wait, &leaderworkersetv1.LeaderWorkerSetList{}))
		trace.mu.Lock()
		requests := append([]*http.Request(nil), trace.requests...)
		trace.reads = nil
		trace.mu.Unlock()
		var order []string
		seen := map[string]bool{}
		for _, request := range requests {
			kind := path.Base(request.URL.Path)
			switch kind {
			case "leaderworkersets", "statefulsets", "deployments", "replicasets", "pods":
				selector := ""
				if kind == "pods" || kind == "replicasets" {
					selector = leaderworkersetv1.SetNameLabelKey
				}
				require.Equal(t, selector, request.URL.Query().Get("labelSelector"), "filter both LIST and WATCH for %s", kind)
				if !seen[kind] {
					require.NotEqual(t, "0", request.URL.Query().Get("resourceVersion"), "bootstrap must be consistent")
					order, seen[kind] = append(order, kind), true
				}
			}
		}
		require.Equal(t, []string{"leaderworkersets", "statefulsets", "deployments", "replicasets", "pods"}, order,
			"per-kind cache options must not start descendant informers ahead of their parents")
		manager := NewLeaderWorkerSetManager(writeOnlyTestClient(t, writer))
		pending.Reader = o
		manager.reader, manager.pending = o, pending
		return manager, trace, stop
	}
	for _, tc := range []struct {
		name                          string
		watchFirst, deleteBeforeCheck bool
	}{
		{"create response before watch", false, false},
		{"watch before create response", true, false},
		{"watch misses creation and deletion", false, true},
		{"creation deleted before reconcile", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const revision = "created"
			ds := newTestDisaggregatedSet()
			role := &disaggregatedsetv1.DisaggregatedRoleSpec{Name: testRolePrefill}
			role.Spec = revisionLWS("created", testRolePrefill, 4, 0, time.Time{}).Spec
			role.Spec.StartupPolicy = leaderworkersetv1.LeaderCreatedStartupPolicy
			role.Spec.RolloutStrategy.Type = leaderworkersetv1.RollingUpdateStrategyType
			role.Spec.LeaderWorkerTemplate.WorkerTemplate.Spec.Containers = []corev1.Container{{Name: "test", Image: "test.invalid/image"}}
			manager := NewLeaderWorkerSetManager(writer)
			manager.pending = testPendingWrites(nil)
			if tc.watchFirst {
				manager, _, _ = start(t, nil)
			}
			key := client.ObjectKey{Namespace: ds.Namespace, Name: disaggregatedsetutils.GenerateName(ds.Name, 0, revision, role.Name)}
			current := &leaderworkersetv1.LeaderWorkerSet{}
			manager.client = interceptor.NewClient(writeOnlyTestClient(t, writer), interceptor.Funcs{
				Create: func(ctx context.Context, c client.WithWatch, object client.Object, opts ...client.CreateOption) error {
					if err := c.Create(ctx, object, opts...); err != nil {
						return err
					}
					if tc.watchFirst {
						// Deliver the watch while Create's response is still withheld.
						require.Eventually(t, func() bool { return manager.reader.Get(ctx, key, current) == nil }, 5*time.Second, 10*time.Millisecond)
						require.NotEmpty(t, current.UID)
						if !tc.deleteBeforeCheck {
							require.NoError(t, manager.pending.observed(ctx, ds.UID, "0"))
						}
					}
					return nil
				},
			})
			ctx := t.Context()
			require.NoError(t, manager.Create(ctx, ds, role, 0, revision, 4, nil))
			require.NoError(t, writer.Get(ctx, key, current))
			require.Zero(t, getLWSReplicas(current))
			require.True(t, isUnscaledCreation(current))
			uid := current.UID
			if tc.deleteBeforeCheck {
				require.NoError(t, writer.Delete(ctx, current))
			}
			if !tc.watchFirst {
				fresh, _, _ := start(t, nil)
				manager.reader, manager.pending.Reader = fresh.reader, fresh.reader
			}
			if tc.deleteBeforeCheck {
				require.Eventually(t, func() bool { return apierrors.IsNotFound(manager.reader.Get(ctx, key, current)) }, 5*time.Second, 10*time.Millisecond)
				require.NoError(t, manager.pending.observed(ctx, ds.UID, "0"), "a deleted creation must not block a fully current cache")
				require.NoError(t, manager.Create(ctx, ds, role, 0, revision, 4, nil))
				require.Eventually(t, func() bool { return manager.reader.Get(ctx, key, current) == nil && current.UID != uid }, 5*time.Second, 10*time.Millisecond)
			}
			require.Empty(t, manager.pending.writes)
			require.Contains(t, manager.pending.initialized, current.UID)
			require.NoError(t, manager.prepareSlice(ctx, ds, 0), "a successful creation needs no redundant takeover write")
			r := &DisaggregatedSetReconciler{LWSManager: manager}
			for range 3 {
				require.NoError(t, manager.reader.Get(ctx, key, current))
				if err := r.reconcileCurrentRevisionRole(ctx, ds, 0, current, role, revision, map[string]int{role.Name: 2}); err != nil {
					require.ErrorIs(t, err, errReplicaGroupsPending)
				}
				require.Eventually(t, func() bool { return manager.pending.observed(ctx, ds.UID, "0") == nil }, 5*time.Second, 10*time.Millisecond)
			}
			require.NoError(t, manager.reader.Get(ctx, key, current))
			require.EqualValues(t, 2, getLWSReplicas(current), "growth uses the latest desired size, not the four from creation")
			require.False(t, isUnscaledCreation(current), "growth atomically retires the bootstrap marker")
			require.NoError(t, writer.Delete(ctx, current))
		})
	}
	t.Run("routing-label change wakes an idle parent", func(t *testing.T) {
		ds := testManagerDS("notification-test")
		ds.UID = "notification-test-owner"
		lws := wrappers.BuildLeaderWorkerSet(ds.Namespace).Obj()
		lws.Name = ds.Name
		lws.OwnerReferences = []metav1.OwnerReference{ownerRefFor(ds)}
		require.NoError(t, writer.Create(t.Context(), lws))
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: ds.Name, Namespace: ds.Namespace, Labels: map[string]string{leaderworkersetv1.SetNameLabelKey: lws.Name, disaggregatedsetv1.SubRoleLabelKey: "a"}},
			Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "test", Image: "test.invalid/image"}}},
		}
		require.NoError(t, writer.Create(t.Context(), pod))
		queue := &notificationQueue{
			TypedRateLimitingInterface: workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[ctrl.Request]()),
			added:                      make(chan struct{}, 3),
		}
		t.Cleanup(queue.ShutDown)
		start(t, queue)
		wait := func() {
			select {
			case <-queue.added:
			case <-time.After(5 * time.Second):
				t.Fatal("missing informer notification")
			}
		}
		wait() // Initial LWS replay.
		wait() // Initial Pod replay.
		request, _ := queue.Get()
		queue.Done(request)
		require.Zero(t, queue.Len(), "parent is idle, with no polling retry")
		delete(pod.Labels, disaggregatedsetv1.SubRoleLabelKey)
		require.NoError(t, writer.Update(t.Context(), pod))
		wait()
		require.Equal(t, 1, queue.Len(), "routing-label drift must enqueue the parent for repair")
		request, _ = queue.Get()
		queue.Done(request)
		require.Equal(t, client.ObjectKeyFromObject(ds), request.NamespacedName)
	})
	t.Run("conditional Pod label patches", func(t *testing.T) {
		lws := revisionLWS("pod-labels", testRolePrefill, 1, 0, time.Time{}, 1)
		lws.ResourceVersion = "1"
		manager := NewLeaderWorkerSetManager(writeOnlyTestClient(t, writer))
		// Deliberately keep the reader unchanged: error classification must not
		// depend on a Pod watch having caught up, or fall back to a live GET.
		manager.reader = fake.NewClientBuilder().WithScheme(scheme).WithObjects(lws).Build()
		label := func(pod *corev1.Pod, name string) error {
			return manager.labelSubRoleGroup(t.Context(), lws, replicagroups.Group{Leader: pod, Pods: []*corev1.Pod{pod}}, name)
		}
		for _, change := range []string{"resource-version", "replacement", "uid", "invalid-label"} {
			t.Run(change, func(t *testing.T) {
				pod := &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{Name: "label-" + change, Namespace: lws.Namespace, Labels: map[string]string{"user": "preserved"}},
					Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "test", Image: "test.invalid/image"}}},
				}
				require.NoError(t, writer.Create(t.Context(), pod))
				stale := pod.DeepCopy()
				name := "b"
				switch change {
				case "resource-version":
					pod.Labels["concurrent"] = "preserved"
					require.NoError(t, writer.Update(t.Context(), pod))
				case "replacement", "uid":
					require.NoError(t, writer.Delete(t.Context(), pod))
					pod.UID, pod.ResourceVersion = "", ""
					require.NoError(t, writer.Create(t.Context(), pod))
					require.NotEqual(t, stale.UID, pod.UID)
					if change == "uid" {
						stale.ResourceVersion = pod.ResourceVersion // Verify the UID fence independently.
					}
				case "invalid-label":
					name = "invalid/value"
				}
				err := label(stale, name)
				if change == "invalid-label" {
					require.True(t, apierrors.IsInvalid(err), "%v", err)
					require.NotErrorIs(t, err, errReplicaGroupsPending, "genuine validation errors must remain visible")
				} else {
					require.ErrorIs(t, err, errReplicaGroupsPending)
				}
				require.NoError(t, writer.Get(t.Context(), client.ObjectKeyFromObject(pod), pod))
				require.NotContains(t, pod.Labels, disaggregatedsetv1.SubRoleLabelKey, "rejected writes must not change routing")
				require.NoError(t, label(pod, "b"), "retry from the current observation succeeds")
				require.NoError(t, writer.Get(t.Context(), client.ObjectKeyFromObject(pod), pod))
				require.Equal(t, "b", pod.Labels[disaggregatedsetv1.SubRoleLabelKey])
				require.NoError(t, label(pod, ""))
				require.NoError(t, writer.Get(t.Context(), client.ObjectKeyFromObject(pod), pod))
				require.NotContains(t, pod.Labels, disaggregatedsetv1.SubRoleLabelKey)
				require.Equal(t, "preserved", pod.Labels["user"])
				if change == "resource-version" {
					require.Equal(t, "preserved", pod.Labels["concurrent"])
				}
			})
		}
	})
	for _, identity := range []leaderworkersetv1.GroupIdentityType{leaderworkersetv1.GroupIdentityOrdinal, leaderworkersetv1.GroupIdentityHash} {
		t.Run(string(identity), func(t *testing.T) {
			lws := revisionLWS(string(identity), testRolePrefill, 2, 0, time.Time{}, 2)
			lws.Name = "test-" + map[bool]string{true: "hash", false: "ordinal"}[identity == leaderworkersetv1.GroupIdentityHash]
			lws.Spec.GroupIdentity = identity
			lws.Spec.LeaderWorkerTemplate.Size = ptr.To[int32](1)
			objects := replicaGroupObjects(lws, 2, 0)
			var unrelated []client.Object
			ids := make(map[types.UID]types.UID)
			var victim *corev1.Pod
			// Create a real owner graph. envtest runs no native controllers, so
			// this test can advance each acknowledgement/watch independently.
			for _, object := range objects {
				oldUID := object.GetUID()
				object.SetUID("")
				for i := range object.GetOwnerReferences() {
					owner := &object.GetOwnerReferences()[i]
					if uid, ok := ids[owner.UID]; ok {
						owner.UID = uid
					}
				}
				labels := map[string]string{leaderworkersetv1.SetNameLabelKey: lws.Name}
				template := corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: labels}, Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "test", Image: "test.invalid/image"}}}}
				switch object := object.(type) {
				case *leaderworkersetv1.LeaderWorkerSet:
					object.Spec.LeaderWorkerTemplate.WorkerTemplate = template
					object.Spec.StartupPolicy = leaderworkersetv1.LeaderCreatedStartupPolicy
					object.Spec.RolloutStrategy.Type = leaderworkersetv1.RollingUpdateStrategyType
				case *appsv1.StatefulSet:
					object.Labels = nil // Leader lookup must still work by name.
					object.Spec.Template, object.Spec.Selector = template, &metav1.LabelSelector{MatchLabels: labels}
					object.Spec.ServiceName = lws.Name
				case *appsv1.Deployment:
					object.Labels = nil
					object.Spec.Template, object.Spec.Selector = template, &metav1.LabelSelector{MatchLabels: labels}
				case *appsv1.ReplicaSet:
					object.Spec.Template, object.Spec.Selector = template, &metav1.LabelSelector{MatchLabels: labels}
				case *corev1.Pod:
					object.Spec.Containers = template.Spec.Containers
					object.Finalizers = []string{"test.example/hold"}
					victim = object
				}
				require.NoError(t, writer.Create(t.Context(), object))
				require.NotEmpty(t, object.GetManagedFields())
				ids[oldUID] = object.GetUID()
				switch object.(type) {
				case *corev1.Pod, *appsv1.ReplicaSet:
					other := object.DeepCopyObject().(client.Object)
					other.SetName("unrelated-" + object.GetName())
					other.SetUID("")
					other.SetResourceVersion("")
					other.SetLabels(nil)
					require.NoError(t, writer.Create(t.Context(), other))
					unrelated = append(unrelated, other)
				}
			}
			acknowledge := func(replicas int32) {
				for _, object := range objects {
					require.NoError(t, writer.Get(t.Context(), client.ObjectKeyFromObject(object), object))
					switch object := object.(type) {
					case *leaderworkersetv1.LeaderWorkerSet:
						object.Status.ObservedGeneration = object.Generation
					case *appsv1.StatefulSet:
						object.Spec.Replicas = ptr.To(replicas)
						require.NoError(t, writer.Update(t.Context(), object))
						object.Status.ObservedGeneration = object.Generation
					case *appsv1.Deployment:
						object.Spec.Replicas = ptr.To(replicas)
						require.NoError(t, writer.Update(t.Context(), object))
						object.Status.ObservedGeneration = object.Generation
					case *appsv1.ReplicaSet:
						object.Spec.Replicas = ptr.To(replicas)
						require.NoError(t, writer.Update(t.Context(), object))
						object.Status.ObservedGeneration = object.Generation
					default:
						continue // Pods stay Pending and are never made Ready.
					}
					require.NoError(t, writer.Status().Update(t.Context(), object))
				}
			}
			acknowledge(2)
			manager, trace, stop := start(t, nil)
			o := manager.reader
			for _, object := range objects {
				cached := object.DeepCopyObject().(client.Object)
				require.NoError(t, o.Get(t.Context(), client.ObjectKeyFromObject(object), cached))
				require.Empty(t, cached.GetManagedFields(), "strip unused managed fields for every cached kind")
			}
			for _, object := range unrelated {
				key := client.ObjectKeyFromObject(object)
				cached := object.DeepCopyObject().(client.Object)
				require.True(t, apierrors.IsNotFound(o.Get(t.Context(), key, cached)), "unrelated Pods and ReplicaSets are not cached")
				object.SetLabels(map[string]string{leaderworkersetv1.SetNameLabelKey: "other-lws"})
				require.NoError(t, writer.Update(t.Context(), object))
				require.Eventually(t, func() bool { return o.Get(t.Context(), key, cached) == nil }, 5*time.Second, 10*time.Millisecond)
				object.SetLabels(nil)
				require.NoError(t, writer.Update(t.Context(), object))
				require.Eventually(t, func() bool { return apierrors.IsNotFound(o.Get(t.Context(), key, cached)) }, 5*time.Second, 10*time.Millisecond)
			}
			observe := func() error {
				current := &leaderworkersetv1.LeaderWorkerSet{}
				if err := o.Get(t.Context(), client.ObjectKeyFromObject(lws), current); err != nil {
					return err
				}
				_, err := manager.observeCachedReadiness(t.Context(), []*leaderworkersetv1.LeaderWorkerSet{current})
				return err
			}
			require.NoError(t, observe(), "Pending Pods do not block scaling")
			if identity == leaderworkersetv1.GroupIdentityOrdinal {
				// Exercise the merged subrole path with real watch delivery too:
				// assign Pending groups, then remove routing without live reads.
				for _, membership := range []map[string]int{{"a": 1, "b": 1}, {}} {
					require.Eventually(t, func() bool {
						if err := manager.pending.observed(t.Context(), metav1.GetControllerOf(lws).UID, "0"); err != nil {
							return false
						}
						require.NoError(t, o.Get(t.Context(), client.ObjectKeyFromObject(lws), lws))
						_, err := manager.syncSubRoles(t.Context(), newTestDisaggregatedSet(), lws, subRoleUpdate{membership: membership})
						if err != nil {
							require.ErrorIs(t, err, errReplicaGroupsPending)
							return false
						}
						return true
					}, 5*time.Second, 10*time.Millisecond)
					snapshot, err := replicagroups.Observe(t.Context(), o, lws)
					require.NoError(t, err)
					for _, group := range snapshot.Groups {
						child, coherent := groupSubRole(group)
						require.True(t, coherent)
						if len(membership) == 0 {
							require.Empty(t, child)
						} else {
							require.Contains(t, membership, child)
						}
						require.False(t, group.Ready, "assignment must not require readiness")
					}
				}
			}
			// Hold an old conditional scale-down across a takeover. Changing
			// only the receipt must invalidate that request at the real API server.
			stale := &leaderworkersetv1.LeaderWorkerSet{}
			require.NoError(t, o.Get(t.Context(), client.ObjectKeyFromObject(lws), stale))
			oldPatch := client.MergeFromWithOptions(stale.DeepCopy(), client.MergeFromWithOptimisticLock{})
			stale.Spec.Replicas = ptr.To[int32](1)
			manager.pending = testPendingWrites(o)
			require.Eventually(t, func() bool {
				err := manager.prepareSlice(t.Context(), newTestDisaggregatedSet(), 0)
				if err != nil {
					require.True(t, errors.Is(err, errReplicaGroupsPending) || apierrors.IsConflict(err), "%v", err)
				}
				return err == nil
			}, 5*time.Second, 10*time.Millisecond)
			require.True(t, apierrors.IsConflict(writer.Patch(t.Context(), stale, oldPatch)))
			require.NoError(t, o.Get(t.Context(), client.ObjectKeyFromObject(lws), lws))
			require.NoError(t, observe(), "metadata takeover must not restart native scaling or wait for readiness")
			require.NoError(t, manager.Scale(t.Context(), newTestDisaggregatedSet(), lws, 1))
			require.Eventually(t, func() bool {
				return manager.pending.observed(t.Context(), metav1.GetControllerOf(lws).UID, "0") == nil
			}, 5*time.Second, 10*time.Millisecond)
			require.ErrorIs(t, observe(), errReplicaGroupsPending, "the LWS receipt alone is insufficient")
			acknowledge(1)
			require.ErrorIs(t, observe(), errReplicaGroupsPending, "native acknowledgement alone is insufficient")
			trace.mu.Lock()
			reads := append([]string(nil), trace.reads...)
			trace.mu.Unlock()
			require.Empty(t, reads, "all observation reads after startup are served by informers")
			stop()
			// A restart loses local receipts; a cold, ordered bootstrap must still
			// see the pending deletion left by the previous process.
			manager, _, _ = start(t, nil)
			o = manager.reader
			require.ErrorIs(t, observe(), errReplicaGroupsPending)
			require.NoError(t, writer.Delete(t.Context(), victim))
			require.Eventually(t, func() bool { return observe() == nil }, 5*time.Second, 10*time.Millisecond)
			require.NoError(t, writer.Get(t.Context(), client.ObjectKeyFromObject(victim), victim))
			require.False(t, victim.DeletionTimestamp.IsZero(), "physical removal is not required")
		})
	}
}

func TestBudgetHistoryDoesNotPruneAnUnobservedWrite(t *testing.T) {
	ds := newTestDisaggregatedSet()
	ds.Annotations = map[string]string{disaggregatedsetv1.MaxUnavailableAnnotationKey: `{"removed":2}`}
	lws := revisionLWS("old", "removed", 0, 0, time.Time{}, 4)
	lws.Annotations[observationReceiptAnnotation] = "updated"
	base := newTestClient(ds, lws)
	// The receipt GET advances the fake cache. The subsequent list must see
	// the updated LWS before history can be pruned.
	visible := false
	cached := interceptor.NewClient(base, interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, object client.Object, opts ...client.GetOption) error {
			visible = true
			return c.Get(ctx, key, object, opts...)
		},
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if !visible {
				return nil
			}
			return c.List(ctx, list, opts...)
		},
	})
	o := testPendingWrites(cached)
	o.writes[client.ObjectKeyFromObject(lws)] = &pendingWrite{owner: ds.UID, slice: "0", uid: lws.UID, receipt: "updated", baseVersion: "previous"}
	r := newTestReconciler(base)
	r.LWSManager.reader, r.LWSManager.pending = o, o
	require.NoError(t, r.syncMaxUnavailable(t.Context(), ds, map[string]int{}))
	require.Equal(t, 2, rememberedMaxUnavailable(ds)["removed"])
}

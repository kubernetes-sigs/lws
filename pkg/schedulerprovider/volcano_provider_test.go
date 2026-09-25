/*
Copyright 2025.

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

package schedulerprovider

import (
	"context"
	"errors"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	volcanov1beta1 "volcano.sh/apis/pkg/apis/scheduling/v1beta1"

	leaderworkerset "sigs.k8s.io/lws/api/leaderworkerset/v1"
)

var scheme = runtime.NewScheme()

func init() {
	_ = volcanov1beta1.AddToScheme(scheme)
	_ = leaderworkerset.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)
}

func TestVolcanoProvider_CreatePodGroupIfNotExists(t *testing.T) {
	testLeaderPod1 := createTestLeaderPod("test-lws-0", "default", "test-lws", "0", "abc123")
	testLeaderPod2 := createTestLeaderPod("test-lws-1", "default", "test-lws", "1", "def456")
	testLeaderPod3 := createTestLeaderPod("test-lws-0", "default", "test-lws", "0", "xyz789")
	testLeaderPod4 := createTestLeaderPod("test-lws-2", "default-1", "test-lws-1", "2", "jkl012")
	staleLeaderPod := createTestLeaderPod("test-lws-0", "default", "test-lws", "0", "stale123")
	staleLeaderPod.UID = "old-test-lws-0"
	deletionTimestamp := metav1.Now()

	tests := []struct {
		name           string
		lws            *leaderworkerset.LeaderWorkerSet
		leaderPod      *corev1.Pod
		existingPG     *volcanov1beta1.PodGroup
		injectGetError error
		expectError    bool
		expectErrorIs  error
		expectExists   bool
		expectedPG     *volcanov1beta1.PodGroup
	}{
		{
			name: "create podgroup for LeaderCreated policy",
			lws: &leaderworkerset.LeaderWorkerSet{
				ObjectMeta: metav1.ObjectMeta{Name: "test-lws", Namespace: "default"},
				Spec: leaderworkerset.LeaderWorkerSetSpec{
					StartupPolicy: leaderworkerset.LeaderCreatedStartupPolicy,
					LeaderWorkerTemplate: leaderworkerset.LeaderWorkerTemplate{
						Size: ptr.To[int32](3),
						WorkerTemplate: corev1.PodTemplateSpec{
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{{
									Name: "worker", Image: "nginx",
									Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m")}},
								}},
							},
						},
					},
				},
			},
			leaderPod:   testLeaderPod1,
			expectError: false,
			expectedPG: &volcanov1beta1.PodGroup{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-lws-0-abc123",
					Namespace: "default",
					Labels: map[string]string{
						leaderworkerset.GroupIndexLabelKey: "0",
						leaderworkerset.SetNameLabelKey:    "test-lws",
						leaderworkerset.RevisionKey:        "abc123",
					},
					OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(testLeaderPod1, corev1.SchemeGroupVersion.WithKind("Pod"))},
				},
				Spec: volcanov1beta1.PodGroupSpec{
					MinMember:    3,
					MinResources: &corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("300m")},
				},
			},
		},
		{
			name: "create podgroup for LeaderReady policy",
			lws: &leaderworkerset.LeaderWorkerSet{
				ObjectMeta: metav1.ObjectMeta{Name: "test-lws", Namespace: "default"},
				Spec: leaderworkerset.LeaderWorkerSetSpec{
					StartupPolicy: leaderworkerset.LeaderReadyStartupPolicy,
					LeaderWorkerTemplate: leaderworkerset.LeaderWorkerTemplate{
						Size: ptr.To[int32](3),
						WorkerTemplate: corev1.PodTemplateSpec{
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{{
									Name: "worker", Image: "nginx",
									Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m")}},
								}},
							},
						},
					},
				},
			},
			leaderPod:   testLeaderPod2,
			expectError: false,
			expectedPG: &volcanov1beta1.PodGroup{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-lws-1-def456",
					Namespace: "default",
					Labels: map[string]string{
						leaderworkerset.GroupIndexLabelKey: "1",
						leaderworkerset.SetNameLabelKey:    "test-lws",
						leaderworkerset.RevisionKey:        "def456",
					},
					OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(testLeaderPod2, corev1.SchemeGroupVersion.WithKind("Pod"))},
				},
				Spec: volcanov1beta1.PodGroupSpec{
					MinMember:    1,
					MinResources: &corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("300m")},
				},
			},
		},
		{
			name: "podgroup already exists",
			lws: &leaderworkerset.LeaderWorkerSet{
				ObjectMeta: metav1.ObjectMeta{Name: "test-lws", Namespace: "default"},
				Spec: leaderworkerset.LeaderWorkerSetSpec{
					LeaderWorkerTemplate: leaderworkerset.LeaderWorkerTemplate{Size: ptr.To[int32](3)},
				},
			},
			leaderPod: testLeaderPod3,
			existingPG: &volcanov1beta1.PodGroup{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-lws-0-xyz789",
					Namespace: "default",
					Labels: map[string]string{
						leaderworkerset.GroupIndexLabelKey: "0",
						leaderworkerset.SetNameLabelKey:    "test-lws",
						leaderworkerset.RevisionKey:        "xyz789",
					},
					OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(testLeaderPod3, corev1.SchemeGroupVersion.WithKind("Pod"))},
				},
				Spec: volcanov1beta1.PodGroupSpec{MinMember: 3},
			},
			expectError: false,
			expectedPG: &volcanov1beta1.PodGroup{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-lws-0-xyz789",
					Namespace: "default",
					Labels: map[string]string{
						leaderworkerset.GroupIndexLabelKey: "0",
						leaderworkerset.SetNameLabelKey:    "test-lws",
						leaderworkerset.RevisionKey:        "xyz789",
					},
					OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(testLeaderPod3, corev1.SchemeGroupVersion.WithKind("Pod"))},
				},
				Spec: volcanov1beta1.PodGroupSpec{MinMember: 3},
			},
		},
		{
			name: "wait for stale podgroup owned by previous leader pod to be deleted",
			lws: &leaderworkerset.LeaderWorkerSet{
				ObjectMeta: metav1.ObjectMeta{Name: "test-lws", Namespace: "default"},
				Spec: leaderworkerset.LeaderWorkerSetSpec{
					LeaderWorkerTemplate: leaderworkerset.LeaderWorkerTemplate{Size: ptr.To[int32](3)},
				},
			},
			leaderPod: testLeaderPod3,
			existingPG: &volcanov1beta1.PodGroup{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-lws-0-xyz789",
					Namespace: "default",
					Labels: map[string]string{
						leaderworkerset.GroupIndexLabelKey: "0",
						leaderworkerset.SetNameLabelKey:    "test-lws",
						leaderworkerset.RevisionKey:        "xyz789",
					},
					OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(staleLeaderPod, corev1.SchemeGroupVersion.WithKind("Pod"))},
				},
				Spec: volcanov1beta1.PodGroupSpec{MinMember: 3},
			},
			expectError:  true,
			expectExists: true,
		},
		{
			name: "return error without deleting podgroup owned by unexpected object",
			lws: &leaderworkerset.LeaderWorkerSet{
				ObjectMeta: metav1.ObjectMeta{Name: "test-lws", Namespace: "default"},
				Spec: leaderworkerset.LeaderWorkerSetSpec{
					LeaderWorkerTemplate: leaderworkerset.LeaderWorkerTemplate{Size: ptr.To[int32](3)},
				},
			},
			leaderPod: testLeaderPod3,
			existingPG: &volcanov1beta1.PodGroup{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-lws-0-xyz789",
					Namespace: "default",
					Labels: map[string]string{
						leaderworkerset.GroupIndexLabelKey: "0",
						leaderworkerset.SetNameLabelKey:    "test-lws",
						leaderworkerset.RevisionKey:        "xyz789",
					},
					OwnerReferences: []metav1.OwnerReference{
						{
							APIVersion: leaderworkerset.GroupVersion.String(),
							Kind:       "LeaderWorkerSet",
							Name:       "test-lws",
							UID:        "test-lws",
							Controller: ptr.To(true),
						},
					},
				},
				Spec: volcanov1beta1.PodGroupSpec{MinMember: 3},
			},
			expectError:   true,
			expectErrorIs: ErrUnexpectedPodGroupOwner,
			expectExists:  true,
		},
		{
			name: "podgroup already exists with current owner and no labels",
			lws: &leaderworkerset.LeaderWorkerSet{
				ObjectMeta: metav1.ObjectMeta{Name: "test-lws", Namespace: "default"},
				Spec: leaderworkerset.LeaderWorkerSetSpec{
					LeaderWorkerTemplate: leaderworkerset.LeaderWorkerTemplate{Size: ptr.To[int32](3)},
				},
			},
			leaderPod: testLeaderPod3,
			existingPG: &volcanov1beta1.PodGroup{
				ObjectMeta: metav1.ObjectMeta{
					Name:            "test-lws-0-xyz789",
					Namespace:       "default",
					OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(testLeaderPod3, corev1.SchemeGroupVersion.WithKind("Pod"))},
				},
				Spec: volcanov1beta1.PodGroupSpec{MinMember: 3},
			},
			expectError: false,
			expectedPG: &volcanov1beta1.PodGroup{
				ObjectMeta: metav1.ObjectMeta{
					Name:            "test-lws-0-xyz789",
					Namespace:       "default",
					OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(testLeaderPod3, corev1.SchemeGroupVersion.WithKind("Pod"))},
				},
				Spec: volcanov1beta1.PodGroupSpec{MinMember: 3},
			},
		},
		{
			name: "requeue while podgroup is being deleted",
			lws: &leaderworkerset.LeaderWorkerSet{
				ObjectMeta: metav1.ObjectMeta{Name: "test-lws", Namespace: "default"},
				Spec: leaderworkerset.LeaderWorkerSetSpec{
					LeaderWorkerTemplate: leaderworkerset.LeaderWorkerTemplate{Size: ptr.To[int32](3)},
				},
			},
			leaderPod: testLeaderPod3,
			existingPG: &volcanov1beta1.PodGroup{
				ObjectMeta: metav1.ObjectMeta{
					Name:              "test-lws-0-xyz789",
					Namespace:         "default",
					DeletionTimestamp: &deletionTimestamp,
					Finalizers:        []string{"volcano.sh/test"},
					Labels: map[string]string{
						leaderworkerset.GroupIndexLabelKey: "0",
						leaderworkerset.SetNameLabelKey:    "test-lws",
						leaderworkerset.RevisionKey:        "xyz789",
					},
					OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(testLeaderPod3, corev1.SchemeGroupVersion.WithKind("Pod"))},
				},
				Spec: volcanov1beta1.PodGroupSpec{MinMember: 3},
			},
			expectError: true,
		},
		{
			name: "create podgroup inherit volcano annotations",
			lws: &leaderworkerset.LeaderWorkerSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-lws-1",
					Namespace: "default-1",
					Annotations: map[string]string{
						"volcano.sh/sla-waiting-time": "5m",
					},
				},
				Spec: leaderworkerset.LeaderWorkerSetSpec{
					LeaderWorkerTemplate: leaderworkerset.LeaderWorkerTemplate{
						Size: ptr.To[int32](3),
						WorkerTemplate: corev1.PodTemplateSpec{
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{{
									Name: "worker", Image: "nginx",
									Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m")}},
								}},
							},
						},
					},
				},
			},
			leaderPod:   testLeaderPod4,
			expectError: false,
			expectedPG: &volcanov1beta1.PodGroup{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-lws-1-2-jkl012",
					Namespace: "default-1",
					Labels: map[string]string{
						leaderworkerset.GroupIndexLabelKey: "2",
						leaderworkerset.SetNameLabelKey:    "test-lws-1",
						leaderworkerset.RevisionKey:        "jkl012",
					},
					Annotations: map[string]string{
						"volcano.sh/sla-waiting-time": "5m",
					},
					OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(testLeaderPod4, corev1.SchemeGroupVersion.WithKind("Pod"))},
				},
				Spec: volcanov1beta1.PodGroupSpec{
					MinMember:    3,
					MinResources: &corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("300m")},
				},
			},
		},
		{
			name: "generic error on getting podgroup",
			lws: &leaderworkerset.LeaderWorkerSet{
				ObjectMeta: metav1.ObjectMeta{Name: "test-lws", Namespace: "default"},
				Spec: leaderworkerset.LeaderWorkerSetSpec{
					LeaderWorkerTemplate: leaderworkerset.LeaderWorkerTemplate{Size: ptr.To[int32](3)},
				},
			},
			leaderPod:      testLeaderPod1,
			injectGetError: errors.New("unexpected error"),
			expectError:    true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var objs []client.Object
			if tt.existingPG != nil {
				objs = append(objs, tt.existingPG)
			}

			builder := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...)
			if tt.injectGetError != nil {
				builder.WithInterceptorFuncs(interceptor.Funcs{
					Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
						if _, ok := obj.(*volcanov1beta1.PodGroup); ok {
							return tt.injectGetError
						}
						return c.Get(ctx, key, obj)
					},
				})
			}
			fakeClient := builder.Build()

			provider := NewVolcanoProvider(fakeClient)
			err := provider.CreatePodGroupIfNotExists(context.TODO(), tt.lws, tt.leaderPod)

			if tt.expectError {
				assert.Error(t, err)
				if tt.expectErrorIs != nil {
					assert.ErrorIs(t, err, tt.expectErrorIs)
				} else {
					assert.NotErrorIs(t, err, ErrUnexpectedPodGroupOwner)
				}
				if tt.injectGetError != nil {
					assert.Equal(t, tt.injectGetError, err)
				}
				if tt.expectExists {
					var actualPG volcanov1beta1.PodGroup
					pgName := tt.leaderPod.Annotations[volcanov1beta1.KubeGroupNameAnnotationKey]
					err = fakeClient.Get(context.TODO(), types.NamespacedName{Name: pgName, Namespace: tt.lws.Namespace}, &actualPG)
					assert.NoError(t, err)
				}
				return
			}

			assert.NoError(t, err)

			var actualPG volcanov1beta1.PodGroup
			pgName := tt.leaderPod.Annotations[volcanov1beta1.KubeGroupNameAnnotationKey]
			err = fakeClient.Get(context.TODO(), types.NamespacedName{Name: pgName, Namespace: tt.lws.Namespace}, &actualPG)
			assert.NoError(t, err)

			opts := []cmp.Option{
				cmpopts.IgnoreFields(metav1.ObjectMeta{}, "ResourceVersion"),
			}

			if diff := cmp.Diff(tt.expectedPG, &actualPG, opts...); diff != "" {
				t.Errorf("PodGroup mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestVolcanoProvider_RecreatesPodGroupAfterGarbageCollection(t *testing.T) {
	ctx := context.Background()
	lws := &leaderworkerset.LeaderWorkerSet{
		ObjectMeta: metav1.ObjectMeta{Name: "test-lws", Namespace: "default"},
		Spec: leaderworkerset.LeaderWorkerSetSpec{
			LeaderWorkerTemplate: leaderworkerset.LeaderWorkerTemplate{Size: ptr.To[int32](3)},
		},
	}
	leaderPod := createTestLeaderPod("test-lws-0", "default", "test-lws", "0", "abc123")
	previousLeaderPod := leaderPod.DeepCopy()
	previousLeaderPod.UID = "previous-leader"
	pgName := leaderPod.Annotations[volcanov1beta1.KubeGroupNameAnnotationKey]
	stalePG := &volcanov1beta1.PodGroup{
		ObjectMeta: metav1.ObjectMeta{
			Name:            pgName,
			Namespace:       lws.Namespace,
			OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(previousLeaderPod, corev1.SchemeGroupVersion.WithKind("Pod"))},
		},
	}

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(stalePG).Build()
	provider := NewVolcanoProvider(fakeClient)

	err := provider.CreatePodGroupIfNotExists(ctx, lws, leaderPod)
	assert.Error(t, err)
	assert.NotErrorIs(t, err, ErrUnexpectedPodGroupOwner)
	var actualPG volcanov1beta1.PodGroup
	assert.NoError(t, fakeClient.Get(ctx, types.NamespacedName{Name: pgName, Namespace: lws.Namespace}, &actualPG))

	assert.NoError(t, fakeClient.Delete(ctx, &actualPG)) // Simulate owner-reference garbage collection.
	assert.NoError(t, provider.CreatePodGroupIfNotExists(ctx, lws, leaderPod))
	assert.NoError(t, fakeClient.Get(ctx, types.NamespacedName{Name: pgName, Namespace: lws.Namespace}, &actualPG))

	owner := metav1.GetControllerOf(&actualPG)
	if assert.NotNil(t, owner) {
		assert.Equal(t, leaderPod.UID, owner.UID)
	}
}

// TestVolcanoProvider_CreatePodGroupIfNotExists_TypedSchedulingIsNoop reproduces
// https://github.com/kubernetes-sigs/lws/issues/1075: with spec.scheduling set,
// ReconcileScheduling has already pre-created a PodGroup controlled by the
// LeaderWorkerSet itself, before any leader Pod exists. CreatePodGroupIfNotExists
// must not treat that LWS-owned PodGroup as an unexpected owner and block worker
// creation; it must leave PodGroup ownership to ReconcileScheduling entirely.
func TestVolcanoProvider_CreatePodGroupIfNotExists_TypedSchedulingIsNoop(t *testing.T) {
	ctx := context.Background()
	lws := &leaderworkerset.LeaderWorkerSet{
		ObjectMeta: metav1.ObjectMeta{Name: "test-lws", Namespace: "default"},
		Spec: leaderworkerset.LeaderWorkerSetSpec{
			LeaderWorkerTemplate: leaderworkerset.LeaderWorkerTemplate{Size: ptr.To[int32](3)},
			Scheduling:           &leaderworkerset.LeaderWorkerSetScheduling{},
		},
	}
	leaderPod := createTestLeaderPod("test-lws-0", "default", "test-lws", "0", "abc123")
	pgName := leaderPod.Annotations[volcanov1beta1.KubeGroupNameAnnotationKey]
	lwsOwnedPG := &volcanov1beta1.PodGroup{
		ObjectMeta: metav1.ObjectMeta{
			Name:            pgName,
			Namespace:       lws.Namespace,
			OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(lws, leaderworkerset.GroupVersion.WithKind("LeaderWorkerSet"))},
		},
		Spec: volcanov1beta1.PodGroupSpec{MinMember: 3},
	}

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(lwsOwnedPG).Build()
	provider := NewVolcanoProvider(fakeClient)

	assert.NoError(t, provider.CreatePodGroupIfNotExists(ctx, lws, leaderPod))

	// The LWS-owned PodGroup must be left untouched: still owned by the
	// LeaderWorkerSet, not reassigned or recreated under the leader Pod.
	var actualPG volcanov1beta1.PodGroup
	assert.NoError(t, fakeClient.Get(ctx, types.NamespacedName{Name: pgName, Namespace: lws.Namespace}, &actualPG))
	owner := metav1.GetControllerOf(&actualPG)
	if assert.NotNil(t, owner) {
		assert.Equal(t, "LeaderWorkerSet", owner.Kind)
		assert.Equal(t, lws.UID, owner.UID)
	}
}

// Helper function to create test leader pods
func createTestLeaderPod(name, namespace, lwsName, groupIndex, revision string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			UID:       types.UID(name),
			Annotations: map[string]string{
				volcanov1beta1.KubeGroupNameAnnotationKey: GetPodGroupName(lwsName, groupIndex, revision),
			},
			Labels: map[string]string{
				leaderworkerset.SetNameLabelKey:    lwsName,
				leaderworkerset.GroupIndexLabelKey: groupIndex,
				leaderworkerset.RevisionKey:        revision,
			},
		},
	}
}

// TestVolcanoProviderReconcileSchedulingGroupIdentity covers both identity
// modes: Ordinal instances are named after contiguous indexes and can be
// pre-created, while Hash group names only exist once admission stamps a
// leader pod, so the pod-driven path owns them.
func TestVolcanoProviderReconcileSchedulingGroupIdentity(t *testing.T) {
	ctx := context.Background()
	newLWS := func(identity leaderworkerset.GroupIdentityType) *leaderworkerset.LeaderWorkerSet {
		return &leaderworkerset.LeaderWorkerSet{
			ObjectMeta: metav1.ObjectMeta{Name: "test-lws", Namespace: "default"},
			Spec: leaderworkerset.LeaderWorkerSetSpec{
				Replicas:      ptr.To[int32](2),
				GroupIdentity: identity,
				Scheduling:    &leaderworkerset.LeaderWorkerSetScheduling{},
				LeaderWorkerTemplate: leaderworkerset.LeaderWorkerTemplate{
					Size: ptr.To[int32](2),
					WorkerTemplate: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
						Containers: []corev1.Container{{Name: "worker", Image: "worker:latest"}},
					}},
				},
			},
		}
	}

	for _, tc := range []struct {
		name       string
		identity   leaderworkerset.GroupIdentityType
		wantGroups int
	}{
		{name: "ordinal pre-creates one PodGroup per replica", identity: leaderworkerset.GroupIdentityOrdinal, wantGroups: 2},
		{name: "hash defers PodGroups to the leader pods", identity: leaderworkerset.GroupIdentityHash},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lws := newLWS(tc.identity)
			fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(lws).Build()

			err := NewVolcanoProvider(fakeClient).ReconcileScheduling(ctx, lws, 2, "revision-1")
			assert.NoError(t, err)

			groups := &volcanov1beta1.PodGroupList{}
			assert.NoError(t, fakeClient.List(ctx, groups, client.InNamespace(lws.Namespace)))
			assert.Len(t, groups.Items, tc.wantGroups)
		})
	}
}

func TestVolcanoProviderCreatePodGroupIfNotExistsOwnership(t *testing.T) {
	ctx := context.Background()
	newLWS := func(identity leaderworkerset.GroupIdentityType, typedScheduling bool) *leaderworkerset.LeaderWorkerSet {
		lws := &leaderworkerset.LeaderWorkerSet{
			ObjectMeta: metav1.ObjectMeta{Name: "test-lws", Namespace: "default", UID: "lws-uid-123"},
			Spec: leaderworkerset.LeaderWorkerSetSpec{
				Replicas:      ptr.To[int32](2),
				GroupIdentity: identity,
				LeaderWorkerTemplate: leaderworkerset.LeaderWorkerTemplate{
					Size: ptr.To[int32](2),
					WorkerTemplate: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
						Containers: []corev1.Container{{Name: "worker", Image: "worker:latest"}},
					}},
				},
			},
		}
		if typedScheduling {
			lws.Spec.Scheduling = &leaderworkerset.LeaderWorkerSetScheduling{}
		}
		return lws
	}

	// Hash groups never reuse a name, so an LWS-owned PodGroup would outlive its
	// group. The leader owns it instead and it is garbage collected with it.
	t.Run("hash identity with typed scheduling creates leader-owned PodGroup", func(t *testing.T) {
		lws := newLWS(leaderworkerset.GroupIdentityHash, true)
		fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(lws).Build()
		provider := NewVolcanoProvider(fakeClient)

		leader := createTestLeaderPod("leader-hash", lws.Namespace, lws.Name, "hash123", "rev1")
		err := provider.CreatePodGroupIfNotExists(ctx, lws, leader)
		assert.NoError(t, err)

		pg := &volcanov1beta1.PodGroup{}
		err = fakeClient.Get(ctx, types.NamespacedName{Name: GetPodGroupName(lws.Name, "hash123", "rev1"), Namespace: lws.Namespace}, pg)
		assert.NoError(t, err)

		owner := metav1.GetControllerOf(pg)
		assert.NotNil(t, owner)
		assert.Equal(t, "Pod", owner.Kind)
		assert.Equal(t, leader.Name, owner.Name)
		assert.Equal(t, leader.UID, owner.UID)

		// Subsequent call succeeds idempotently.
		err = provider.CreatePodGroupIfNotExists(ctx, lws, leader)
		assert.NoError(t, err)
	})

	t.Run("ordinal identity with typed scheduling validates pre-created LWS-owned PodGroup", func(t *testing.T) {
		lws := newLWS(leaderworkerset.GroupIdentityOrdinal, true)
		fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(lws).Build()
		provider := NewVolcanoProvider(fakeClient)

		// Pre-create PodGroups via ReconcileScheduling.
		err := provider.ReconcileScheduling(ctx, lws, 2, "rev1")
		assert.NoError(t, err)

		leader := createTestLeaderPod("leader-0", lws.Namespace, lws.Name, "0", "rev1")
		err = provider.CreatePodGroupIfNotExists(ctx, lws, leader)
		assert.NoError(t, err)

		pg := &volcanov1beta1.PodGroup{}
		err = fakeClient.Get(ctx, types.NamespacedName{Name: GetPodGroupName(lws.Name, "0", "rev1"), Namespace: lws.Namespace}, pg)
		assert.NoError(t, err)

		owner := metav1.GetControllerOf(pg)
		assert.NotNil(t, owner)
		assert.Equal(t, "LeaderWorkerSet", owner.Kind)
		assert.Equal(t, lws.Name, owner.Name)
		assert.Equal(t, lws.UID, owner.UID)
	})

	t.Run("legacy mode creates leader-pod-owned PodGroup", func(t *testing.T) {
		lws := newLWS(leaderworkerset.GroupIdentityOrdinal, false)
		fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(lws).Build()
		provider := NewVolcanoProvider(fakeClient)

		leader := createTestLeaderPod("leader-0", lws.Namespace, lws.Name, "0", "rev1")
		err := provider.CreatePodGroupIfNotExists(ctx, lws, leader)
		assert.NoError(t, err)

		pg := &volcanov1beta1.PodGroup{}
		err = fakeClient.Get(ctx, types.NamespacedName{Name: GetPodGroupName(lws.Name, "0", "rev1"), Namespace: lws.Namespace}, pg)
		assert.NoError(t, err)

		owner := metav1.GetControllerOf(pg)
		assert.NotNil(t, owner)
		assert.Equal(t, "Pod", owner.Kind)
		assert.Equal(t, leader.Name, owner.Name)
		assert.Equal(t, leader.UID, owner.UID)
	})
}

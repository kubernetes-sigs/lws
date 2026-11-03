/*
Copyright 2026 The Kubernetes Authors.

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
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	disaggregatedsetv1 "sigs.k8s.io/lws/api/disaggregatedset/v1"
	leaderworkersetv1 "sigs.k8s.io/lws/api/leaderworkerset/v1"
	"sigs.k8s.io/lws/pkg/computedomain"
	"sigs.k8s.io/lws/pkg/features"
	disaggregatedsetutils "sigs.k8s.io/lws/pkg/utils/disaggregatedset"
)

func TestLWSManagerCreateInjectsSliceComputeDomainClaims(t *testing.T) {
	features.SetFeatureGateDuringTest(t, features.AutoComputeDomain, true)

	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, leaderworkersetv1.AddToScheme(scheme))
	require.NoError(t, disaggregatedsetv1.AddToScheme(scheme))
	require.NoError(t, computedomain.AddToScheme(scheme))

	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	manager := NewLeaderWorkerSetManager(c)

	gpuRes := corev1.ResourceRequirements{
		Limits: corev1.ResourceList{
			computedomain.NvidiaGPUResourceName: resource.MustParse("4"),
		},
	}
	ds := &disaggregatedsetv1.DisaggregatedSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "serving",
			Namespace: "default",
			UID:       types.UID("ds-uid-1"),
			Annotations: map[string]string{
				disaggregatedsetv1.AutoComputeDomainAnnotationKey: leaderworkersetv1.AutoComputeDomainEnabled,
			},
		},
		Spec: disaggregatedsetv1.DisaggregatedSetSpec{
			Slices: ptr.To[int32](2),
			Roles: []disaggregatedsetv1.DisaggregatedRoleSpec{
				{
					Name: "prefill",
					LeaderWorkerSetTemplateSpec: leaderworkersetv1.LeaderWorkerSetTemplateSpec{
						Spec: leaderworkersetv1.LeaderWorkerSetSpec{
							LeaderWorkerTemplate: leaderworkersetv1.LeaderWorkerTemplate{
								Size: ptr.To[int32](2),
								LeaderTemplate: &corev1.PodTemplateSpec{
									Spec: corev1.PodSpec{
										Containers: []corev1.Container{{Name: "cpu-leader"}},
									},
								},
								WorkerTemplate: corev1.PodTemplateSpec{
									Spec: corev1.PodSpec{
										Containers: []corev1.Container{
											{Name: "gpu-worker", Resources: gpuRes},
											{Name: "cpu-proxy"},
										},
									},
								},
							},
						},
					},
				},
				{
					Name: "encoder",
					LeaderWorkerSetTemplateSpec: leaderworkersetv1.LeaderWorkerSetTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{
							Annotations: map[string]string{
								disaggregatedsetv1.AutoComputeDomainAnnotationKey: leaderworkersetv1.AutoComputeDomainDisabled,
							},
						},
						Spec: leaderworkersetv1.LeaderWorkerSetSpec{
							LeaderWorkerTemplate: leaderworkersetv1.LeaderWorkerTemplate{
								Size: ptr.To[int32](1),
								WorkerTemplate: corev1.PodTemplateSpec{
									Spec: corev1.PodSpec{
										Containers: []corev1.Container{{Name: "vision-gpu", Resources: gpuRes}},
									},
								},
							},
						},
					},
				},
			},
		},
	}

	rev := disaggregatedsetutils.ComputeDisaggregatedSetRevision(ds)
	ctx := t.Context()

	// Create slice 0 and slice 1 for prefill, and slice 0 for opted-out encoder.
	require.NoError(t, manager.Create(ctx, ds, &ds.Spec.Roles[0], 0, rev, 1, nil))
	require.NoError(t, manager.Create(ctx, ds, &ds.Spec.Roles[0], 1, rev, 1, nil))
	require.NoError(t, manager.Create(ctx, ds, &ds.Spec.Roles[1], 0, rev, 1, nil))

	// Original role spec must not have been mutated across slices.
	require.Empty(t, ds.Spec.Roles[0].Spec.LeaderWorkerTemplate.WorkerTemplate.Spec.ResourceClaims)

	var prefillSlice0, prefillSlice1, encoderSlice0 leaderworkersetv1.LeaderWorkerSet
	require.NoError(t, c.Get(ctx, types.NamespacedName{
		Namespace: "default",
		Name:      disaggregatedsetutils.GenerateName("serving", 0, rev, "prefill"),
	}, &prefillSlice0))
	require.NoError(t, c.Get(ctx, types.NamespacedName{
		Namespace: "default",
		Name:      disaggregatedsetutils.GenerateName("serving", 1, rev, "prefill"),
	}, &prefillSlice1))
	require.NoError(t, c.Get(ctx, types.NamespacedName{
		Namespace: "default",
		Name:      disaggregatedsetutils.GenerateName("serving", 0, rev, "encoder"),
	}, &encoderSlice0))

	wantCD0 := computedomain.SliceComputeDomainName(ds, 0, rev)
	wantCD1 := computedomain.SliceComputeDomainName(ds, 1, rev)

	// Slice 0 prefill: CPU leader untouched, GPU worker claimed with wantCD0, CPU proxy skipped.
	require.Empty(t, prefillSlice0.Spec.LeaderWorkerTemplate.LeaderTemplate.Spec.ResourceClaims)
	require.Len(t, prefillSlice0.Spec.LeaderWorkerTemplate.WorkerTemplate.Spec.ResourceClaims, 1)
	require.Equal(t, wantCD0, *prefillSlice0.Spec.LeaderWorkerTemplate.WorkerTemplate.Spec.ResourceClaims[0].ResourceClaimTemplateName)
	require.Len(t, prefillSlice0.Spec.LeaderWorkerTemplate.WorkerTemplate.Spec.Containers[0].Resources.Claims, 1)
	require.Empty(t, prefillSlice0.Spec.LeaderWorkerTemplate.WorkerTemplate.Spec.Containers[1].Resources.Claims)

	// Slice 1 prefill receives distinct slice 1 ComputeDomain name.
	require.Len(t, prefillSlice1.Spec.LeaderWorkerTemplate.WorkerTemplate.Spec.ResourceClaims, 1)
	require.Equal(t, wantCD1, *prefillSlice1.Spec.LeaderWorkerTemplate.WorkerTemplate.Spec.ResourceClaims[0].ResourceClaimTemplateName)

	// Opted-out encoder receives zero ComputeDomain claims.
	require.Empty(t, encoderSlice0.Spec.LeaderWorkerTemplate.WorkerTemplate.Spec.ResourceClaims)
	require.Empty(t, encoderSlice0.Spec.LeaderWorkerTemplate.WorkerTemplate.Spec.Containers[0].Resources.Claims)

	t.Run("gate disabled refuses to create enrolled children without claims", func(t *testing.T) {
		features.SetFeatureGateDuringTest(t, features.AutoComputeDomain, false)

		err := manager.Create(ctx, ds, &ds.Spec.Roles[0], 1, "rev-gate-off", 1, nil)
		require.ErrorContains(t, err, "AutoComputeDomain feature gate is disabled")
		var missing leaderworkersetv1.LeaderWorkerSet
		require.Error(t, c.Get(ctx, types.NamespacedName{
			Namespace: "default",
			Name:      disaggregatedsetutils.GenerateName("serving", 1, "rev-gate-off", "prefill"),
		}, &missing))

		// A role opted out of the slice ComputeDomain is unaffected.
		require.NoError(t, manager.Create(ctx, ds, &ds.Spec.Roles[1], 1, "rev-gate-off", 1, nil))
	})
}

func TestLegacyDisaggregatedSetMigratesRevisionOnRoleAutoComputeDomain(t *testing.T) {
	features.SetFeatureGateDuringTest(t, features.AutoComputeDomain, true)

	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, leaderworkersetv1.AddToScheme(scheme))
	require.NoError(t, disaggregatedsetv1.AddToScheme(scheme))
	require.NoError(t, computedomain.AddToScheme(scheme))

	gpuRes := corev1.ResourceRequirements{
		Limits: corev1.ResourceList{
			computedomain.NvidiaGPUResourceName: resource.MustParse("4"),
		},
	}
	ds := &disaggregatedsetv1.DisaggregatedSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "legacy-serving",
			Namespace: "default",
			UID:       types.UID("legacy-ds-uid"),
		},
		Spec: disaggregatedsetv1.DisaggregatedSetSpec{
			Roles: []disaggregatedsetv1.DisaggregatedRoleSpec{{
				Name: "prefill",
				LeaderWorkerSetTemplateSpec: leaderworkersetv1.LeaderWorkerSetTemplateSpec{
					Spec: leaderworkersetv1.LeaderWorkerSetSpec{
						Replicas: ptr.To[int32](1),
						LeaderWorkerTemplate: leaderworkersetv1.LeaderWorkerTemplate{
							Size: ptr.To[int32](2),
							WorkerTemplate: corev1.PodTemplateSpec{
								Spec: corev1.PodSpec{
									Containers: []corev1.Container{{Name: "vllm", Resources: gpuRes}},
								},
							},
						},
					},
				},
			}},
		},
	}

	v1Rev := disaggregatedsetutils.ComputeRevisionV1(ds.Spec.Roles)
	legacyChild := &leaderworkersetv1.LeaderWorkerSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      disaggregatedsetutils.GenerateName(ds.Name, 0, v1Rev, "prefill"),
			Namespace: ds.Namespace,
			Labels:    disaggregatedsetutils.GenerateLabels(ds.Name, 0, v1Rev, "prefill"),
			Annotations: map[string]string{
				disaggregatedsetv1.InitialReplicasAnnotationKey: "1",
			},
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(ds, disaggregatedsetv1.GroupVersion.WithKind("DisaggregatedSet")),
			},
		},
		Spec: ds.Spec.Roles[0].Spec,
		Status: leaderworkersetv1.LeaderWorkerSetStatus{
			Replicas:      1,
			ReadyReplicas: 1,
		},
	}

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&disaggregatedsetv1.DisaggregatedSet{}).
		WithObjects(ds, legacyChild).
		Build()
	reconciler := NewDisaggregatedSetReconciler(c, scheme, events.NewFakeRecorder(100))
	ctx := t.Context()
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ds.Namespace, Name: ds.Name}}

	// Without auto-compute-domain, an unversioned legacy DisaggregatedSet stays on v1.
	_, err := reconciler.Reconcile(ctx, req)
	require.NoError(t, err)
	var current disaggregatedsetv1.DisaggregatedSet
	require.NoError(t, c.Get(ctx, req.NamespacedName, &current))
	require.Empty(t, current.Annotations[disaggregatedsetv1.RevisionHashVersionAnnotationKey])

	// Enabling per-role replica auto-compute-domain (Option 1) must migrate the
	// DisaggregatedSet to v2 and create a new revision child LWS carrying the role annotation.
	current.Spec.Roles[0].Annotations = map[string]string{
		leaderworkersetv1.AutoComputeDomainAnnotationKey: leaderworkersetv1.AutoComputeDomainEnabled,
	}
	require.NoError(t, c.Update(ctx, &current))

	// First reconcile persists the v2 revision-hash-version marker and waits for cache observation.
	_, err = reconciler.Reconcile(ctx, req)
	require.NoError(t, err)
	require.NoError(t, c.Get(ctx, req.NamespacedName, &current))
	require.Equal(t, disaggregatedsetv1.RevisionHashVersion, current.Annotations[disaggregatedsetv1.RevisionHashVersionAnnotationKey])

	// Second reconcile computes the v2 revision and creates the target child LWS with auto-compute-domain enabled.
	_, err = reconciler.Reconcile(ctx, req)
	require.NoError(t, err)

	v2Rev := disaggregatedsetutils.ComputeDisaggregatedSetRevision(&current)
	require.NotEqual(t, v1Rev, v2Rev)

	var v2Child leaderworkersetv1.LeaderWorkerSet
	require.NoError(t, c.Get(ctx, types.NamespacedName{
		Namespace: ds.Namespace,
		Name:      disaggregatedsetutils.GenerateName(ds.Name, 0, v2Rev, "prefill"),
	}, &v2Child))
	require.Equal(t, leaderworkersetv1.AutoComputeDomainEnabled, v2Child.Annotations[leaderworkersetv1.AutoComputeDomainAnnotationKey])
}

func TestSliceComputeDomainToDisaggregatedSet(t *testing.T) {
	cd := func(labels map[string]string) *computedomain.ComputeDomain {
		return &computedomain.ComputeDomain{ObjectMeta: metav1.ObjectMeta{Name: "cd", Namespace: "ns", Labels: labels}}
	}
	mapFn := computedomain.MapComputeDomainToOwner(disaggregatedsetv1.SetNameLabelKey)
	got := mapFn(t.Context(), cd(map[string]string{
		disaggregatedsetv1.SetNameLabelKey: "serving",
		computedomain.OwnerUIDLabelKey:     "ds-uid",
	}))
	require.Equal(t, []ctrl.Request{{NamespacedName: types.NamespacedName{Namespace: "ns", Name: "serving"}}}, got)

	// A ComputeDomain LWS does not manage, or a replica ComputeDomain, maps to nothing.
	require.Empty(t, mapFn(t.Context(), cd(map[string]string{disaggregatedsetv1.SetNameLabelKey: "serving"})))
	require.Empty(t, mapFn(t.Context(), cd(map[string]string{
		leaderworkersetv1.SetNameLabelKey: "model",
		computedomain.OwnerUIDLabelKey:    "lws-uid",
	})))
}

func TestDeletingDisaggregatedSetCleansUpComputeDomains(t *testing.T) {
	features.SetFeatureGateDuringTest(t, features.AutoComputeDomain, true)

	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, leaderworkersetv1.AddToScheme(scheme))
	require.NoError(t, disaggregatedsetv1.AddToScheme(scheme))
	require.NoError(t, computedomain.AddToScheme(scheme))

	gpuRes := corev1.ResourceRequirements{
		Limits: corev1.ResourceList{
			computedomain.NvidiaGPUResourceName: resource.MustParse("4"),
		},
	}
	now := metav1.Now()
	ds := &disaggregatedsetv1.DisaggregatedSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "serving",
			Namespace:         "default",
			UID:               types.UID("ds-uid-deleting"),
			DeletionTimestamp: &now,
			Finalizers:        []string{"test/hold"},
			Annotations: map[string]string{
				disaggregatedsetv1.AutoComputeDomainAnnotationKey: leaderworkersetv1.AutoComputeDomainEnabled,
			},
		},
		Spec: disaggregatedsetv1.DisaggregatedSetSpec{
			Slices: ptr.To[int32](2),
			Roles: []disaggregatedsetv1.DisaggregatedRoleSpec{{
				Name: "prefill",
				LeaderWorkerSetTemplateSpec: leaderworkersetv1.LeaderWorkerSetTemplateSpec{
					Spec: leaderworkersetv1.LeaderWorkerSetSpec{
						Replicas: ptr.To[int32](1),
						LeaderWorkerTemplate: leaderworkersetv1.LeaderWorkerTemplate{
							Size: ptr.To[int32](2),
							WorkerTemplate: corev1.PodTemplateSpec{
								Spec: corev1.PodSpec{
									Containers: []corev1.Container{{Name: "vllm", Resources: gpuRes}},
								},
							},
						},
					},
				},
			}},
		},
	}

	cd0Name := computedomain.SliceComputeDomainName(ds, 0, "rev1")
	cd1Name := computedomain.SliceComputeDomainName(ds, 1, "rev1")

	cd0 := &computedomain.ComputeDomain{
		ObjectMeta: metav1.ObjectMeta{
			Name:       cd0Name,
			Namespace:  ds.Namespace,
			Finalizers: []string{leaderworkersetv1.ComputeDomainProtectionFinalizer},
			Labels: map[string]string{
				disaggregatedsetv1.SetNameLabelKey: ds.Name,
				computedomain.OwnerUIDLabelKey:     string(ds.UID),
			},
		},
	}
	cd1 := &computedomain.ComputeDomain{
		ObjectMeta: metav1.ObjectMeta{
			Name:       cd1Name,
			Namespace:  ds.Namespace,
			Finalizers: []string{leaderworkersetv1.ComputeDomainProtectionFinalizer},
			Labels: map[string]string{
				disaggregatedsetv1.SetNameLabelKey: ds.Name,
				computedomain.OwnerUIDLabelKey:     string(ds.UID),
			},
		},
	}

	childSlice0 := &leaderworkersetv1.LeaderWorkerSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      disaggregatedsetutils.GenerateName(ds.Name, 0, "rev1", "prefill"),
			Namespace: ds.Namespace,
			Labels:    disaggregatedsetutils.GenerateLabels(ds.Name, 0, "rev1", "prefill"),
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(ds, disaggregatedsetv1.GroupVersion.WithKind("DisaggregatedSet")),
			},
		},
		Spec: ds.Spec.Roles[0].Spec,
	}

	drainingPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "serving-0-rev1-pod",
			Namespace: ds.Namespace,
		},
		Spec: corev1.PodSpec{
			ResourceClaims: []corev1.PodResourceClaim{{
				Name:                      computedomain.ChannelClaimName,
				ResourceClaimTemplateName: ptr.To(cd0Name),
			}},
		},
	}

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&disaggregatedsetv1.DisaggregatedSet{}).
		WithIndex(&corev1.Pod{}, computedomain.PodClaimTemplateIndex, computedomain.PodClaimTemplateNames).
		WithObjects(ds, cd0, cd1, childSlice0, drainingPod).
		Build()
	reconciler := NewDisaggregatedSetReconciler(c, scheme, events.NewFakeRecorder(100))
	ctx := t.Context()
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ds.Namespace, Name: ds.Name}}

	// First reconcile: cd1 is unreferenced (no child LWS, no pod) and must be deleted.
	// cd0 has both a child LWS and a draining pod, so it must be retained.
	res, err := reconciler.Reconcile(ctx, req)
	require.NoError(t, err)
	require.Equal(t, ctrl.Result{}, res)

	var retainedCD computedomain.ComputeDomain
	require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: ds.Namespace, Name: cd0Name}, &retainedCD))

	var deletedCD computedomain.ComputeDomain
	err = c.Get(ctx, types.NamespacedName{Namespace: ds.Namespace, Name: cd1Name}, &deletedCD)
	require.True(t, apierrors.IsNotFound(err), "unreferenced slice ComputeDomain must be deleted on deleting DisaggregatedSet")

	// Delete child LWS; cd0 must still be retained because drainingPod references it.
	require.NoError(t, c.Delete(ctx, childSlice0))
	res, err = reconciler.Reconcile(ctx, req)
	require.NoError(t, err)
	require.Equal(t, ctrl.Result{}, res)
	require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: ds.Namespace, Name: cd0Name}, &retainedCD))

	// Delete draining pod; cd0 must now be released and deleted.
	require.NoError(t, c.Delete(ctx, drainingPod))
	res, err = reconciler.Reconcile(ctx, req)
	require.NoError(t, err)
	require.Equal(t, ctrl.Result{}, res)

	err = c.Get(ctx, types.NamespacedName{Namespace: ds.Namespace, Name: cd0Name}, &deletedCD)
	require.True(t, apierrors.IsNotFound(err), "slice ComputeDomain must be deleted once its child LWS and referencing pods are gone")
}

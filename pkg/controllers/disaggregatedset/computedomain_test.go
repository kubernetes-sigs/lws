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
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
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

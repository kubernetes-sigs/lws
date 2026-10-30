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
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/utils/ptr"

	disaggv1 "sigs.k8s.io/lws/api/disaggregatedset/v1"
	leaderworkerset "sigs.k8s.io/lws/api/leaderworkerset/v1"
	"sigs.k8s.io/lws/pkg/computedomain"
	"sigs.k8s.io/lws/pkg/features"
)

func cdRESTMapper() apimeta.RESTMapper {
	mapper := apimeta.NewDefaultRESTMapper([]schema.GroupVersion{computedomain.GroupVersion})
	mapper.Add(computedomain.GroupVersion.WithKind(computedomain.Kind), apimeta.RESTScopeNamespace)
	return mapper
}

func baseGPUDisaggregatedSet() *disaggv1.DisaggregatedSet {
	gpuContainer := corev1.Container{
		Name:  "vllm",
		Image: "vllm:latest",
		Resources: corev1.ResourceRequirements{
			Limits: corev1.ResourceList{
				computedomain.NvidiaGPUResourceName: resource.MustParse("4"),
			},
		},
	}
	makeRole := func(name string) disaggv1.DisaggregatedRoleSpec {
		return disaggv1.DisaggregatedRoleSpec{
			Name: name,
			LeaderWorkerSetTemplateSpec: leaderworkerset.LeaderWorkerSetTemplateSpec{
				Spec: leaderworkerset.LeaderWorkerSetSpec{
					Replicas: ptr.To[int32](1),
					LeaderWorkerTemplate: leaderworkerset.LeaderWorkerTemplate{
						Size: ptr.To[int32](2),
						WorkerTemplate: corev1.PodTemplateSpec{
							Spec: corev1.PodSpec{Containers: []corev1.Container{gpuContainer}},
						},
					},
				},
			},
		}
	}
	return &disaggv1.DisaggregatedSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "serving",
			Namespace: "default",
		},
		Spec: disaggv1.DisaggregatedSetSpec{
			Slices: ptr.To[int32](2),
			Roles:  []disaggv1.DisaggregatedRoleSpec{makeRole("prefill"), makeRole("decode")},
		},
	}
}

func TestDisaggregatedSetComputeDomainValidationMatrix(t *testing.T) {
	ctx := t.Context()

	t.Run("feature gate disabled blocks slice and role opt-ins", func(t *testing.T) {
		features.SetFeatureGateDuringTest(t, features.AutoComputeDomain, false)
		hook := &DisaggregatedSetWebhook{RESTMapper: cdRESTMapper()}

		sliceDS := baseGPUDisaggregatedSet()
		sliceDS.Annotations = map[string]string{disaggv1.AutoComputeDomainAnnotationKey: leaderworkerset.AutoComputeDomainEnabled}
		_, err := hook.ValidateCreate(ctx, sliceDS)
		require.ErrorContains(t, err, "AutoComputeDomain")

		roleReplicaDS := baseGPUDisaggregatedSet()
		roleReplicaDS.Spec.Roles[0].Annotations = map[string]string{leaderworkerset.AutoComputeDomainAnnotationKey: leaderworkerset.AutoComputeDomainEnabled}
		_, err = hook.ValidateCreate(ctx, roleReplicaDS)
		require.ErrorContains(t, err, "AutoComputeDomain")
	})

	t.Run("enforces KEP-1259 scope resolution matrix, container overrides, reserved claims, and warnings", func(t *testing.T) {
		features.SetFeatureGateDuringTest(t, features.AutoComputeDomain, true)
		hook := &DisaggregatedSetWebhook{RESTMapper: cdRESTMapper()}

		// 1. Slice scope enabled across roles passes without warnings.
		sliceDS := baseGPUDisaggregatedSet()
		sliceDS.Annotations = map[string]string{disaggv1.AutoComputeDomainAnnotationKey: leaderworkerset.AutoComputeDomainEnabled}
		warnings, err := hook.ValidateCreate(ctx, sliceDS)
		require.NoError(t, err)
		require.Empty(t, warnings)

		// 2. Role enabling disaggregatedset.x-k8s.io/auto-compute-domain without parent enabled is rejected.
		orphanRoleSlice := baseGPUDisaggregatedSet()
		orphanRoleSlice.Spec.Roles[0].Annotations = map[string]string{
			disaggv1.AutoComputeDomainAnnotationKey: leaderworkerset.AutoComputeDomainEnabled,
		}
		_, err = hook.ValidateCreate(ctx, orphanRoleSlice)
		require.ErrorContains(t, err, "requires disaggregatedset.x-k8s.io/auto-compute-domain")

		// 3. Simultaneous slice scope + role replica scope without opt-out is rejected.
		conflictDS := sliceDS.DeepCopy()
		conflictDS.Spec.Roles[0].Annotations = map[string]string{
			leaderworkerset.AutoComputeDomainAnnotationKey: leaderworkerset.AutoComputeDomainEnabled,
		}
		_, err = hook.ValidateCreate(ctx, conflictDS)
		require.ErrorContains(t, err, "cannot enable replica-scoped ComputeDomain")

		// 4. Role opt-out of slice scope + override to replica scope is allowed!
		overrideDS := sliceDS.DeepCopy()
		overrideDS.Spec.Roles[0].Annotations = map[string]string{
			disaggv1.AutoComputeDomainAnnotationKey:        leaderworkerset.AutoComputeDomainDisabled,
			leaderworkerset.AutoComputeDomainAnnotationKey: leaderworkerset.AutoComputeDomainEnabled,
		}
		warnings, err = hook.ValidateCreate(ctx, overrideDS)
		require.NoError(t, err)
		require.Empty(t, warnings)

		// 5. Invalid annotation values on DS or role rejected.
		badValDS := baseGPUDisaggregatedSet()
		badValDS.Annotations = map[string]string{disaggv1.AutoComputeDomainAnnotationKey: "invalid"}
		_, err = hook.ValidateCreate(ctx, badValDS)
		require.Error(t, err)

		// 6. Unknown container in compute-domain-containers rejected.
		badContainers := sliceDS.DeepCopy()
		badContainers.Spec.Roles[0].Spec.LeaderWorkerTemplate.WorkerTemplate.Annotations = map[string]string{
			leaderworkerset.ComputeDomainContainersAnnotationKey: "missing",
		}
		_, err = hook.ValidateCreate(ctx, badContainers)
		require.ErrorContains(t, err, "missing")

		// 7. Reserved claim name rejected on enrolled role.
		reservedClaim := sliceDS.DeepCopy()
		reservedClaim.Spec.Roles[1].Spec.LeaderWorkerTemplate.WorkerTemplate.Spec.ResourceClaims = []corev1.PodResourceClaim{{
			Name:                      computedomain.ChannelClaimName,
			ResourceClaimTemplateName: ptr.To("user-claim"),
		}}
		_, err = hook.ValidateCreate(ctx, reservedClaim)
		require.ErrorContains(t, err, computedomain.ChannelClaimName)

		// 8. Zero GPU containers across enrolled roles emits warning.
		zeroGPU := sliceDS.DeepCopy()
		for i := range zeroGPU.Spec.Roles {
			zeroGPU.Spec.Roles[i].Spec.LeaderWorkerTemplate.WorkerTemplate.Spec.Containers[0].Resources = corev1.ResourceRequirements{}
		}
		warnings, err = hook.ValidateCreate(ctx, zeroGPU)
		require.NoError(t, err)
		require.NotEmpty(t, warnings)
	})
}

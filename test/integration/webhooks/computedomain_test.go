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

package webhooks

import (
	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	disaggregatedsetv1 "sigs.k8s.io/lws/api/disaggregatedset/v1"
	leaderworkersetv1 "sigs.k8s.io/lws/api/leaderworkerset/v1"
	"sigs.k8s.io/lws/pkg/computedomain"
	"sigs.k8s.io/lws/pkg/features"
	testing "sigs.k8s.io/lws/test/testutils"
	"sigs.k8s.io/lws/test/wrappers"
)

var _ = ginkgo.Describe("AutoComputeDomain webhook admission and mutation", func() {
	var ns *corev1.Namespace

	gpuContainer := corev1.Container{
		Name:  "vllm",
		Image: "nvcr.io/nvidia/vllm:latest",
		Resources: corev1.ResourceRequirements{
			Limits: corev1.ResourceList{
				computedomain.NvidiaGPUResourceName: resource.MustParse("4"),
			},
		},
	}

	ginkgo.BeforeEach(func() {
		ns = &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "wh-cd-ns-"}}
		gomega.Expect(k8sClient.Create(ctx, ns)).To(gomega.Succeed())
	})

	ginkgo.AfterEach(func() {
		gomega.Expect(testing.DeleteNamespace(ctx, k8sClient, ns)).To(gomega.Succeed())
	})

	ginkgo.It("rejects auto-compute-domain when AutoComputeDomain feature gate is disabled", func() {
		features.SetFeatureGateDuringTest(ginkgo.GinkgoTB(), features.AutoComputeDomain, false)

		lws := wrappers.BuildLeaderWorkerSet(ns.Name).Name("blocked-lws").Obj()
		lws.Annotations = map[string]string{
			leaderworkersetv1.AutoComputeDomainAnnotationKey: leaderworkersetv1.AutoComputeDomainEnabled,
		}
		lws.Spec.LeaderWorkerTemplate.WorkerTemplate.Spec.Containers = []corev1.Container{gpuContainer}
		gomega.Expect(k8sClient.Create(ctx, lws)).To(gomega.MatchError(gomega.ContainSubstring("AutoComputeDomain")))
	})

	ginkgo.It("validates LeaderWorkerSet & DisaggregatedSet annotations and mutates Pods when enabled", func() {
		features.SetFeatureGateDuringTest(ginkgo.GinkgoTB(), features.AutoComputeDomain, true)

		// Valid LWS succeeds.
		lws := wrappers.BuildLeaderWorkerSet(ns.Name).Name("valid-lws").Obj()
		lws.Annotations = map[string]string{
			leaderworkersetv1.AutoComputeDomainAnnotationKey: leaderworkersetv1.AutoComputeDomainEnabled,
		}
		lws.Spec.LeaderWorkerTemplate.WorkerTemplate.Spec.Containers = []corev1.Container{gpuContainer}
		gomega.Expect(k8sClient.Create(ctx, lws)).To(gomega.Succeed())

		// Conflicting DisaggregatedSet (slice + replica on same role without opt-out) is rejected;
		// valid DisaggregatedSet with role opt-out succeeds.
		ds := &disaggregatedsetv1.DisaggregatedSet{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "serving",
				Namespace: ns.Name,
				Annotations: map[string]string{
					disaggregatedsetv1.AutoComputeDomainAnnotationKey: leaderworkersetv1.AutoComputeDomainEnabled,
				},
			},
			Spec: disaggregatedsetv1.DisaggregatedSetSpec{
				Roles: []disaggregatedsetv1.DisaggregatedRoleSpec{{
					Name: "prefill",
					LeaderWorkerSetTemplateSpec: leaderworkersetv1.LeaderWorkerSetTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{
							Annotations: map[string]string{
								leaderworkersetv1.AutoComputeDomainAnnotationKey: leaderworkersetv1.AutoComputeDomainEnabled,
							},
						},
						Spec: leaderworkersetv1.LeaderWorkerSetSpec{
							Replicas:      ptr.To[int32](1),
							StartupPolicy: leaderworkersetv1.LeaderCreatedStartupPolicy,
							RolloutStrategy: leaderworkersetv1.RolloutStrategy{
								Type: leaderworkersetv1.RollingUpdateStrategyType,
							},
							LeaderWorkerTemplate: leaderworkersetv1.LeaderWorkerTemplate{
								Size: ptr.To[int32](2),
								WorkerTemplate: corev1.PodTemplateSpec{
									Spec: corev1.PodSpec{Containers: []corev1.Container{gpuContainer}},
								},
							},
						},
					},
				}},
			},
		}
		gomega.Expect(k8sClient.Create(ctx, ds)).To(gomega.MatchError(gomega.ContainSubstring("cannot enable replica-scoped ComputeDomain")))

		ds.Spec.Roles[0].Annotations[disaggregatedsetv1.AutoComputeDomainAnnotationKey] = leaderworkersetv1.AutoComputeDomainDisabled
		gomega.Expect(k8sClient.Create(ctx, ds)).To(gomega.Succeed())

		// Live Pod mutating webhook stamps compute-domain-name and injects DRA claims into GPU containers.
		leaderPod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "valid-lws-0",
				Namespace: ns.Name,
				Labels: map[string]string{
					leaderworkersetv1.SetNameLabelKey:     "valid-lws",
					leaderworkersetv1.WorkerIndexLabelKey: "0",
					leaderworkersetv1.RevisionKey:         "rev1",
				},
				Annotations: map[string]string{
					leaderworkersetv1.SizeAnnotationKey:                "2",
					leaderworkersetv1.ComputeDomainPrefixAnnotationKey: "valid-lws-abcde",
				},
			},
			Spec: corev1.PodSpec{
				Subdomain: "valid-lws",
				Containers: []corev1.Container{
					gpuContainer,
					{Name: "proxy", Image: "envoy:latest"},
				},
			},
		}
		gomega.Expect(k8sClient.Create(ctx, leaderPod)).To(gomega.Succeed())
		cdName := leaderPod.Annotations[leaderworkersetv1.ComputeDomainNameAnnotationKey]
		gomega.Expect(cdName).To(gomega.HavePrefix("valid-lws-abcde-rev1-0-"))
		gomega.Expect(leaderPod.Spec.ResourceClaims).To(gomega.HaveLen(1))
		gomega.Expect(*leaderPod.Spec.ResourceClaims[0].ResourceClaimTemplateName).To(gomega.Equal(cdName))
		gomega.Expect(leaderPod.Spec.Containers[0].Resources.Claims).To(gomega.HaveLen(1))
		gomega.Expect(leaderPod.Spec.Containers[1].Resources.Claims).To(gomega.BeEmpty())
	})
})

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

package webhooks

import (
	"context"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"

	disaggregatedset "sigs.k8s.io/lws/api/disaggregatedset/v1"
	leaderworkerset "sigs.k8s.io/lws/api/leaderworkerset/v1"
	"sigs.k8s.io/lws/test/wrappers"
)

var _ = ginkgo.Describe("disaggregatedset placement policy validation", func() {

	// Each test runs in a separate namespace.
	var ns *corev1.Namespace
	ginkgo.BeforeEach(func() {
		ns = &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: "test-ns-",
			},
		}
		gomega.Expect(k8sClient.Create(ctx, ns)).To(gomega.Succeed())
	})

	buildDisaggregatedSet := func() *wrappers.DisaggregatedSetWrapper {
		disagg := wrappers.BuildDisaggregatedSet("placement-test", ns.Name).
			WithRole("prefill", 1, "nginx:1.14.2").
			WithRole("decode", 1, "nginx:1.14.2")
		// The DisaggregatedSet CRD enum-validates these fields but has no
		// defaulting webhook to fill them in, so set them explicitly.
		for i := range disagg.Spec.Roles {
			disagg.Spec.Roles[i].Spec.RolloutStrategy.Type = leaderworkerset.RollingUpdateStrategyType
			disagg.Spec.Roles[i].Spec.StartupPolicy = leaderworkerset.LeaderCreatedStartupPolicy
		}
		return disagg
	}

	type testValidationCase struct {
		makeDisaggregatedSet func() *disaggregatedset.DisaggregatedSet
		expectedError        string
	}
	ginkgo.DescribeTable("creation composing placement policy with LWS exclusive placement",
		func(tc *testValidationCase) {
			ctx := context.Background()
			err := k8sClient.Create(ctx, tc.makeDisaggregatedSet())
			if tc.expectedError == "" {
				gomega.Expect(err).To(gomega.Succeed())
			} else {
				gomega.Expect(err).To(gomega.HaveOccurred())
				gomega.Expect(err.Error()).To(gomega.ContainSubstring(tc.expectedError))
			}
		},
		ginkgo.Entry("placement policy alone is allowed", &testValidationCase{
			makeDisaggregatedSet: func() *disaggregatedset.DisaggregatedSet {
				return buildDisaggregatedSet().
					WithPlacementPolicy(disaggregatedset.PlacementExclusiveSlice, "kubernetes.io/hostname").
					Obj()
			},
		}),
		ginkgo.Entry("LWS exclusive-topology annotation alone is allowed", &testValidationCase{
			makeDisaggregatedSet: func() *disaggregatedset.DisaggregatedSet {
				return buildDisaggregatedSet().
					WithRoleAnnotation("prefill", leaderworkerset.ExclusiveKeyAnnotationKey, "kubernetes.io/hostname").
					Obj()
			},
		}),
		ginkgo.Entry("policy combined with exclusive-topology on the role metadata is rejected", &testValidationCase{
			makeDisaggregatedSet: func() *disaggregatedset.DisaggregatedSet {
				return buildDisaggregatedSet().
					WithPlacementPolicy(disaggregatedset.PlacementExclusiveSlice, "kubernetes.io/hostname").
					WithRoleAnnotation("prefill", leaderworkerset.ExclusiveKeyAnnotationKey, "kubernetes.io/hostname").
					Obj()
			},
			expectedError: leaderworkerset.ExclusiveKeyAnnotationKey,
		}),
		ginkgo.Entry("policy combined with exclusive-topology on the worker template is rejected", &testValidationCase{
			makeDisaggregatedSet: func() *disaggregatedset.DisaggregatedSet {
				return buildDisaggregatedSet().
					WithPlacementPolicy(disaggregatedset.PlacementExclusiveTopology, "kubernetes.io/hostname").
					WithWorkerTemplateAnnotation("decode", leaderworkerset.ExclusiveKeyAnnotationKey, "kubernetes.io/hostname").
					Obj()
			},
			expectedError: leaderworkerset.ExclusiveKeyAnnotationKey,
		}),
		ginkgo.Entry("policy combined with subgroup-exclusive-topology is rejected", &testValidationCase{
			makeDisaggregatedSet: func() *disaggregatedset.DisaggregatedSet {
				return buildDisaggregatedSet().
					WithPlacementPolicy(disaggregatedset.PlacementExclusiveSlice, "kubernetes.io/hostname").
					WithRoleAnnotation("decode", leaderworkerset.SubGroupExclusiveKeyAnnotationKey, "kubernetes.io/hostname").
					Obj()
			},
			expectedError: leaderworkerset.SubGroupExclusiveKeyAnnotationKey,
		}),
	)

	ginkgo.It("rejects adding an LWS exclusive-topology annotation to a DisaggregatedSet with a placement policy", func() {
		ctx := context.Background()
		disagg := buildDisaggregatedSet().
			WithPlacementPolicy(disaggregatedset.PlacementExclusiveSlice, "kubernetes.io/hostname").
			Obj()
		gomega.Expect(k8sClient.Create(ctx, disagg)).To(gomega.Succeed())

		var fetched disaggregatedset.DisaggregatedSet
		gomega.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: disagg.Name, Namespace: disagg.Namespace}, &fetched)).To(gomega.Succeed())
		fetched.Spec.Roles[0].ObjectMeta.Annotations = map[string]string{
			leaderworkerset.ExclusiveKeyAnnotationKey: "kubernetes.io/hostname",
		}
		err := k8sClient.Update(ctx, &fetched)
		gomega.Expect(err).To(gomega.HaveOccurred())
		gomega.Expect(err.Error()).To(gomega.ContainSubstring(leaderworkerset.ExclusiveKeyAnnotationKey))
	})

	ginkgo.It("rejects adding a placement policy to a DisaggregatedSet with an LWS exclusive-topology annotation", func() {
		ctx := context.Background()
		disagg := buildDisaggregatedSet().
			WithRoleAnnotation("prefill", leaderworkerset.ExclusiveKeyAnnotationKey, "kubernetes.io/hostname").
			Obj()
		gomega.Expect(k8sClient.Create(ctx, disagg)).To(gomega.Succeed())

		var fetched disaggregatedset.DisaggregatedSet
		gomega.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: disagg.Name, Namespace: disagg.Namespace}, &fetched)).To(gomega.Succeed())
		fetched.Spec.PlacementPolicy = &disaggregatedset.PlacementPolicy{
			Type:     disaggregatedset.PlacementExclusiveSlice,
			Topology: "kubernetes.io/hostname",
		}
		err := k8sClient.Update(ctx, &fetched)
		gomega.Expect(err).To(gomega.HaveOccurred())
		gomega.Expect(err.Error()).To(gomega.ContainSubstring(leaderworkerset.ExclusiveKeyAnnotationKey))
	})
})

var _ = ginkgo.Describe("disaggregatedset group identity", func() {

	var ns *corev1.Namespace
	ginkgo.BeforeEach(func() {
		ns = &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: "test-ns-",
			},
		}
		gomega.Expect(k8sClient.Create(ctx, ns)).To(gomega.Succeed())
	})

	buildDisaggregatedSet := func(name string) *wrappers.DisaggregatedSetWrapper {
		disagg := wrappers.BuildDisaggregatedSet(name, ns.Name).
			WithRole("prefill", 1, "nginx:1.14.2").
			WithRole("decode", 1, "nginx:1.14.2")
		for i := range disagg.Spec.Roles {
			disagg.Spec.Roles[i].Spec.RolloutStrategy.Type = leaderworkerset.RollingUpdateStrategyType
			disagg.Spec.Roles[i].Spec.StartupPolicy = leaderworkerset.LeaderCreatedStartupPolicy
		}
		return disagg
	}

	ginkgo.It("persists a Hash groupIdentity role through the CRD schema", func() {
		disagg := buildDisaggregatedSet("gi-hash").Obj()
		disagg.Spec.Roles[0].Spec.GroupIdentity = leaderworkerset.GroupIdentityHash
		gomega.Expect(k8sClient.Create(ctx, disagg)).To(gomega.Succeed())

		fetched := &disaggregatedset.DisaggregatedSet{}
		gomega.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: disagg.Name, Namespace: ns.Name}, fetched)).To(gomega.Succeed())
		gomega.Expect(fetched.Spec.Roles[0].Spec.GroupIdentity).To(gomega.Equal(leaderworkerset.GroupIdentityHash))
		// The role without an explicit value gets the CRD default.
		gomega.Expect(fetched.Spec.Roles[1].Spec.GroupIdentity).To(gomega.Equal(leaderworkerset.GroupIdentityOrdinal))
	})

	ginkgo.It("rejects a Hash role with subGroupPolicy at admission", func() {
		disagg := buildDisaggregatedSet("gi-hash-subgroup").Obj()
		disagg.Spec.Roles[0].Spec.GroupIdentity = leaderworkerset.GroupIdentityHash
		disagg.Spec.Roles[0].Spec.LeaderWorkerTemplate.SubGroupPolicy = &leaderworkerset.SubGroupPolicy{
			SubGroupSize: ptr.To(int32(1)),
		}
		err := k8sClient.Create(ctx, disagg)
		gomega.Expect(err).To(gomega.HaveOccurred())
		gomega.Expect(err.Error()).To(gomega.ContainSubstring("subGroupPolicy is not supported with groupIdentity Hash"))
	})
})

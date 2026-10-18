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
	"math"
	"slices"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/tools/events"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	disaggregatedset "sigs.k8s.io/lws/api/disaggregatedset/v1"
	leaderworkerset "sigs.k8s.io/lws/api/leaderworkerset/v1"
	disaggregatedsetcontroller "sigs.k8s.io/lws/pkg/controllers/disaggregatedset"
	disaggregatedsetutils "sigs.k8s.io/lws/pkg/utils/disaggregatedset"
	"sigs.k8s.io/lws/test/wrappers"
)

var _ = ginkgo.Describe("disaggregatedset policy validation", func() {

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

	policy := func(up disaggregatedset.RolloutScaleUpPolicy, down disaggregatedset.RolloutScaleDownPolicy) *disaggregatedset.DisaggregatedSetScalingPolicy {
		return &disaggregatedset.DisaggregatedSetScalingPolicy{ScaleUpDuringRollout: up, ScaleDownDuringRollout: down}
	}
	ginkgo.DescribeTable("rollout scaling policy defaulting and validation",
		func(input, want *disaggregatedset.DisaggregatedSetScalingPolicy, invalidField string) {
			disagg := buildDisaggregatedSet().Obj()
			disagg.Spec.ScalingPolicy = input
			err := k8sClient.Create(ctx, disagg)
			if invalidField != "" {
				gomega.Expect(apierrors.IsInvalid(err)).To(gomega.BeTrue())
				gomega.Expect(err.Error()).To(gomega.ContainSubstring(invalidField))
				return
			}
			gomega.Expect(err).To(gomega.Succeed())
			var fetched disaggregatedset.DisaggregatedSet
			gomega.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: disagg.Name, Namespace: ns.Name}, &fetched)).To(gomega.Succeed())
			if want != nil {
				want = want.DeepCopy()
				want.SubRoleScaleDelaySeconds = ptr.To[int32](2)
			}
			gomega.Expect(fetched.Spec.ScalingPolicy).To(gomega.Equal(want))
		},
		ginkgo.Entry("omitted policy", nil, nil, ""),
		ginkgo.Entry("empty policy defaults both fields", policy("", ""), policy("RolloutCoupled", "Deferred"), ""),
		ginkgo.Entry("scale-up only defaults scale-down", policy("Immediate", ""), policy("Immediate", "Deferred"), ""),
		ginkgo.Entry("scale-down only defaults scale-up", policy("", "Immediate"), policy("RolloutCoupled", "Immediate"), ""),
		ginkgo.Entry("RolloutCoupled and Deferred", policy("RolloutCoupled", "Deferred"), policy("RolloutCoupled", "Deferred"), ""),
		ginkgo.Entry("RolloutCoupled and Immediate", policy("RolloutCoupled", "Immediate"), policy("RolloutCoupled", "Immediate"), ""),
		ginkgo.Entry("Immediate and Deferred", policy("Immediate", "Deferred"), policy("Immediate", "Deferred"), ""),
		ginkgo.Entry("Immediate and Immediate", policy("Immediate", "Immediate"), policy("Immediate", "Immediate"), ""),
		ginkgo.Entry("invalid scale-up policy", policy("invalid", "Deferred"), nil, "scaleUpDuringRollout"),
		ginkgo.Entry("invalid scale-down policy", policy("RolloutCoupled", "invalid"), nil, "scaleDownDuringRollout"),
	)

	ginkgo.DescribeTable("sub-role scale delay validation",
		func(seconds *int32, valid bool) {
			disagg := buildDisaggregatedSet().Obj()
			disagg.Spec.ScalingPolicy = &disaggregatedset.DisaggregatedSetScalingPolicy{SubRoleScaleDelaySeconds: seconds}
			err := k8sClient.Create(ctx, disagg)
			if !valid {
				gomega.Expect(apierrors.IsInvalid(err)).To(gomega.BeTrue())
				gomega.Expect(err.Error()).To(gomega.ContainSubstring("subRoleScaleDelaySeconds"))
				return
			}
			gomega.Expect(err).To(gomega.Succeed())
			gomega.Expect(disagg.Spec.ScalingPolicy.SubRoleScaleDelaySeconds).To(gomega.Equal(seconds))
			disagg.Spec.ScalingPolicy.SubRoleScaleDelaySeconds = nil
			gomega.Expect(k8sClient.Update(ctx, disagg)).To(gomega.Succeed())
			gomega.Expect(disagg.Spec.ScalingPolicy.SubRoleScaleDelaySeconds).To(gomega.Equal(ptr.To[int32](2)))
			disagg.Spec.ScalingPolicy.SubRoleScaleDelaySeconds = ptr.To[int32](61)
			gomega.Expect(apierrors.IsInvalid(k8sClient.Update(ctx, disagg))).To(gomega.BeTrue())
		},
		ginkgo.Entry("disabled", ptr.To[int32](0), true),
		ginkgo.Entry("maximum", ptr.To[int32](60), true),
		ginkgo.Entry("negative", ptr.To[int32](-1), false),
		ginkgo.Entry("above maximum", ptr.To[int32](61), false),
	)

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

var _ = ginkgo.Describe("disaggregatedset zero-replica rollout validation", func() {
	ginkgo.It("rejects invalid rollout budgets on create and update", func() {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "test-ns-"}}
		gomega.Expect(k8sClient.Create(ctx, ns)).To(gomega.Succeed())

		disagg := wrappers.BuildDisaggregatedSet("zero-replicas", ns.Name).
			WithRole("prefill", 0, "nginx:1.14.2").
			WithRole("decode", 0, "nginx:1.14.2").Obj()
		for i := range disagg.Spec.Roles {
			disagg.Spec.Roles[i].Spec.RolloutStrategy = leaderworkerset.RolloutStrategy{
				Type:                       leaderworkerset.RollingUpdateStrategyType,
				RollingUpdateConfiguration: &leaderworkerset.RollingUpdateConfiguration{},
			}
			disagg.Spec.Roles[i].Spec.StartupPolicy = leaderworkerset.LeaderCreatedStartupPolicy
		}

		invalid := disagg.DeepCopy()
		invalid.Spec.Roles[0].Spec.RolloutStrategy.RollingUpdateConfiguration.MaxSurge = intstr.FromInt32(-1)
		err := k8sClient.Create(ctx, invalid)
		gomega.Expect(err).To(gomega.HaveOccurred())
		gomega.Expect(err.Error()).To(gomega.ContainSubstring("maxSurge"))

		// Zero budgets are valid while all roles are scaled to zero.
		gomega.Expect(k8sClient.Create(ctx, disagg)).To(gomega.Succeed())
		disagg.Spec.Roles[0].Spec.RolloutStrategy.RollingUpdateConfiguration.MaxUnavailable = intstr.FromString("101%")
		err = k8sClient.Update(ctx, disagg)
		gomega.Expect(err).To(gomega.HaveOccurred())
		gomega.Expect(err.Error()).To(gomega.ContainSubstring("maxUnavailable"))
	})
})

var _ = ginkgo.Describe("disaggregatedset External role rollout budgets", func() {
	ginkgo.DescribeTable("validates budgets of roles with defaulted replicas",
		func(external bool, maxUnavailable intstr.IntOrString, wantError string) {
			ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "test-ns-"}}
			gomega.Expect(k8sClient.Create(ctx, ns)).To(gomega.Succeed())

			disagg := wrappers.BuildDisaggregatedSet("external-budgets", ns.Name).
				WithRole("prefill", 1, "nginx:1.14.2").
				WithRole("decode", 1, "nginx:1.14.2").Obj()
			for i := range disagg.Spec.Roles {
				role := &disagg.Spec.Roles[i]
				role.Spec.Replicas = nil
				role.Spec.RolloutStrategy = leaderworkerset.RolloutStrategy{
					Type: leaderworkerset.RollingUpdateStrategyType,
					RollingUpdateConfiguration: &leaderworkerset.RollingUpdateConfiguration{
						MaxSurge:       intstr.FromInt32(0),
						MaxUnavailable: maxUnavailable,
					},
				}
				role.Spec.StartupPolicy = leaderworkerset.LeaderCreatedStartupPolicy
				if external {
					role.Scaling = &disaggregatedset.RoleScaling{Mode: disaggregatedset.RoleScalingExternal}
				}
			}

			err := k8sClient.Create(ctx, disagg)
			if wantError != "" {
				gomega.Expect(err).To(gomega.MatchError(gomega.ContainSubstring(wantError)))
				return
			}
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(disagg), disagg)).To(gomega.Succeed())
			for _, role := range disagg.Spec.Roles {
				gomega.Expect(role.Spec.Replicas).To(gomega.Equal(ptr.To[int32](1)), role.Name)
			}
		},
		ginkgo.Entry("accepts an External percentage that rounds to zero at the default replicas", true, intstr.FromString("25%"), ""),
		ginkgo.Entry("rejects an External pair that is zero at every size", true, intstr.FromString("0%"), "rollingUpdateConfiguration.maxUnavailable"),
		ginkgo.Entry("rejects a Static percentage that rounds to zero at the default replicas", false, intstr.FromString("25%"), "rollingUpdateConfiguration.maxUnavailable"),
	)
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

	ginkgo.It("rejects a Hash role with volumeClaimTemplates at admission", func() {
		disagg := buildDisaggregatedSet("gi-hash-vct").Obj()
		disagg.Spec.Roles[0].Spec.GroupIdentity = leaderworkerset.GroupIdentityHash
		disagg.Spec.Roles[0].Spec.LeaderWorkerTemplate.VolumeClaimTemplates = []corev1.PersistentVolumeClaim{{
			ObjectMeta: metav1.ObjectMeta{Name: "data"},
		}}
		err := k8sClient.Create(ctx, disagg)
		gomega.Expect(err).To(gomega.HaveOccurred())
		gomega.Expect(err.Error()).To(gomega.ContainSubstring("volumeClaimTemplates are not supported with groupIdentity Hash"))
	})

	ginkgo.It("accepts a Hash role with maxGroupRestarts at admission", func() {
		disagg := buildDisaggregatedSet("gi-hash-restart-budget").Obj()
		disagg.Spec.Roles[0].Spec.GroupIdentity = leaderworkerset.GroupIdentityHash
		disagg.Spec.Roles[0].Spec.LeaderWorkerTemplate.MaxGroupRestarts = ptr.To(int32(1))
		gomega.Expect(k8sClient.Create(ctx, disagg)).To(gomega.Succeed())
	})

	ginkgo.DescribeTable("virtual role admission and schema",
		func(change, wantError string) {
			ds := buildDisaggregatedSet("virtual-schema").Obj()
			ds.Spec.Roles = ds.Spec.Roles[:1] // One physical role is enough.
			ds.Spec.Roles[0].SubRoles = []disaggregatedset.DisaggregatedSubRoleSpec{{Name: "hot"}, {Name: "cold", Replicas: ptr.To[int32](0)}}
			gomega.Expect(k8sClient.Create(ctx, ds)).To(gomega.Succeed())
			role := &ds.Spec.Roles[0]
			external := &disaggregatedset.RoleScaling{Mode: disaggregatedset.RoleScalingExternal}
			switch change {
			case "duplicate":
				role.SubRoles[1].Name = "hot"
			case "negative":
				role.SubRoles[0].Replicas = ptr.To[int32](-1)
			case "external replicas":
				role.SubRoles[1].Scaling = external
			case "invalid child name":
				role.SubRoles[0].Name = "parent/child"
			case "Hash":
				role.Spec.GroupIdentity = leaderworkerset.GroupIdentityHash
			case "static slices":
				ds.Spec.Slices = ptr.To[int32](2)
			case "parent scaling":
				role.Scaling = external
			case "external slices":
				role.SubRoles[0].Scaling, ds.Spec.Slices = external, ptr.To[int32](2)
			case "overflow":
				role.SubRoles[0].Replicas, role.SubRoles[1].Replicas = ptr.To[int32](math.MaxInt32), ptr.To[int32](1)
			case "worker label":
				role.Spec.LeaderWorkerTemplate.WorkerTemplate.Labels = map[string]string{disaggregatedset.SubRoleLabelKey: "hot"}
			case "leader label":
				role.Spec.LeaderWorkerTemplate.LeaderTemplate = role.Spec.LeaderWorkerTemplate.WorkerTemplate.DeepCopy()
				role.Spec.LeaderWorkerTemplate.LeaderTemplate.Labels = map[string]string{disaggregatedset.SubRoleLabelKey: "hot"}
			case "role label":
				role.Labels = map[string]string{disaggregatedset.SubRoleLabelKey: "hot"}
			case "scaler collision":
				role.SubRoles[0].Scaling = external
				other := *role.DeepCopy()
				other.Name, other.SubRoles, other.Scaling = role.Name+"-hot", nil, external
				ds.Spec.Roles = append(ds.Spec.Roles, other)
			case "ignored parent zero":
				role.Spec.Replicas = ptr.To[int32](0)
				role.Spec.RolloutStrategy.RollingUpdateConfiguration = &leaderworkerset.RollingUpdateConfiguration{MaxSurge: intstr.FromInt(0), MaxUnavailable: intstr.FromInt(0)}
			case "rollout overrides":
				role.SubRoles[0].MaxSurge, role.SubRoles[0].MaxUnavailable = ptr.To(intstr.FromInt(0)), ptr.To(intstr.FromString("100%"))
				role.SubRoles[1].MaxUnavailable = ptr.To(intstr.FromInt(0))
			case "invalid rollout override":
				role.SubRoles[0].MaxSurge = ptr.To(intstr.FromString("101%"))
			case "zero rollout override":
				role.SubRoles[0].MaxSurge, role.SubRoles[0].MaxUnavailable = ptr.To(intstr.FromInt(0)), ptr.To(intstr.FromInt(0))
			}
			fresh := ds.DeepCopy()
			expectedSubRoles := fresh.Spec.Roles[0].SubRoles
			fresh.Name, fresh.ResourceVersion, fresh.UID = ds.Name+"-create", "", ""
			for _, err := range []error{k8sClient.Create(ctx, fresh), k8sClient.Update(ctx, ds)} {
				if wantError != "" {
					gomega.Expect(err).To(gomega.MatchError(gomega.ContainSubstring(wantError)))
				} else {
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
				}
			}
			if wantError != "" {
				return
			}
			gomega.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: ds.Name, Namespace: ds.Namespace}, ds)).To(gomega.Succeed())
			gomega.Expect(ds.Spec.Roles[0].SubRoles).To(gomega.Equal(expectedSubRoles))
			statuses := []disaggregatedset.RoleStatus{
				{Name: role.Name, DesiredReplicas: ptr.To[int32](1), Replicas: 1, ReadyReplicas: 1, UpdatedReplicas: 1, SubRoleStatuses: []disaggregatedset.SubRoleStatus{
					{Name: "hot", DesiredReplicas: ptr.To[int32](1), Replicas: 1, ReadyReplicas: 1, UpdatedReplicas: 1},
					{Name: "cold", DesiredReplicas: ptr.To[int32](0)},
				}},
			}
			ds.Status.RoleStatuses = statuses
			gomega.Expect(k8sClient.Status().Update(ctx, ds)).To(gomega.Succeed())
			gomega.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: ds.Name, Namespace: ds.Namespace}, ds)).To(gomega.Succeed())
			gomega.Expect(ds.Status.RoleStatuses).To(gomega.Equal(statuses))
		},
		ginkgo.Entry("persists default Ordinal pools and independent zero", "", ""),
		ginkgo.Entry("allows static children across slices", "static slices", ""),
		ginkgo.Entry("rejects duplicate child keys", "duplicate", "Duplicate value"),
		ginkgo.Entry("rejects negative targets", "negative", "greater than or equal to 0"),
		ginkgo.Entry("rejects External replicas", "external replicas", "replicas must be omitted"),
		ginkgo.Entry("rejects slash in child name", "invalid child name", ".name"),
		ginkgo.Entry("persists Hash pools and independent zero", "Hash", ""),
		ginkgo.Entry("rejects parent scaling", "parent scaling", "parent scaling must be omitted"),
		ginkgo.Entry("rejects External slices", "external slices", "spec.slices > 1"),
		ginkgo.Entry("rejects overflowing totals", "overflow", "replicas must be equal or less than 1000000"),
		ginkgo.Entry("reserves worker labels", "worker label", "reserved"),
		ginkgo.Entry("reserves leader labels", "leader label", "reserved"),
		ginkgo.Entry("reserves role labels", "role label", "reserved"),
		ginkgo.Entry("rejects scaler collision", "scaler collision", "scaler name collides"),
		ginkgo.Entry("cannot hide zero/zero budgets behind ignored parent zero", "ignored parent zero", "must not be 0"),
		ginkgo.Entry("persists independent rollout overrides including explicit zero", "rollout overrides", ""),
		ginkgo.Entry("rejects invalid child rollout budgets", "invalid rollout override", "subRoles[0].maxSurge"),
		ginkgo.Entry("rejects child rollout budgets that cannot progress", "zero rollout override", "subRoles[0].maxUnavailable"),
	)
})

var _ = ginkgo.Describe("disaggregatedset rollout defaults", func() {
	ginkgo.It("persists the CRD rollout defaults on roles that omit rolloutStrategy", func() {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "test-ns-"}}
		gomega.Expect(k8sClient.Create(ctx, ns)).To(gomega.Succeed())

		disagg := wrappers.BuildDisaggregatedSet("rollout-defaults", ns.Name).
			WithRole("prefill", 1, "nginx:1.14.2").
			WithRole("decode", 1, "nginx:1.14.2").Obj()
		for i := range disagg.Spec.Roles {
			disagg.Spec.Roles[i].Spec.StartupPolicy = leaderworkerset.LeaderCreatedStartupPolicy
		}
		// The typed client serializes an empty RolloutStrategy as {"type": ""}, which the
		// enum rejects. Send the manifest the way a YAML author would, without the key.
		content, err := runtime.DefaultUnstructuredConverter.ToUnstructured(disagg)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		roles, _, err := unstructured.NestedSlice(content, "spec", "roles")
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		for _, role := range roles {
			unstructured.RemoveNestedField(role.(map[string]interface{}), "spec", "rolloutStrategy")
		}
		gomega.Expect(unstructured.SetNestedSlice(content, roles, "spec", "roles")).To(gomega.Succeed())
		manifest := &unstructured.Unstructured{Object: content}
		manifest.SetGroupVersionKind(disaggregatedset.GroupVersion.WithKind("DisaggregatedSet"))
		gomega.Expect(k8sClient.Create(ctx, manifest)).To(gomega.Succeed())

		persisted := &disaggregatedset.DisaggregatedSet{}
		gomega.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: disagg.Name, Namespace: ns.Name}, persisted)).To(gomega.Succeed())
		gomega.Expect(persisted.Spec.Roles).To(gomega.HaveLen(2))
		for _, role := range persisted.Spec.Roles {
			gomega.Expect(role.Spec.RolloutStrategy.Type).To(gomega.Equal(leaderworkerset.RollingUpdateStrategyType), role.Name)
			gomega.Expect(role.Spec.RolloutStrategy.RollingUpdateConfiguration).To(gomega.Equal(&leaderworkerset.RollingUpdateConfiguration{
				Partition:      ptr.To[int32](0),
				MaxUnavailable: intstr.FromInt32(1),
				MaxSurge:       intstr.FromInt32(0),
			}), role.Name)
		}
	})
})

var _ = ginkgo.Describe("DisaggregatedSet static role template validation", func() {
	ginkgo.DescribeTable("validates creation and replacement templates",
		func(mutate func(*leaderworkerset.LeaderWorkerSet), wantField string) {
			ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "template-contract-"}}
			gomega.Expect(k8sClient.Create(ctx, ns)).To(gomega.Succeed())
			lws := wrappers.BuildLeaderWorkerSet(ns.Name).Size(3).Obj()
			expectDisaggregatedSetRoleTemplateValidation(lws, mutate, wantField)
		},
		ginkgo.Entry("rejects zero size", func(l *leaderworkerset.LeaderWorkerSet) {
			l.Spec.LeaderWorkerTemplate.Size = ptr.To[int32](0)
		}, "spec.leaderWorkerTemplate.size"),
		ginkgo.Entry("rejects zero rollout budgets with defaulted replicas", func(l *leaderworkerset.LeaderWorkerSet) {
			l.Spec.Replicas = nil
			l.Spec.RolloutStrategy.RollingUpdateConfiguration = &leaderworkerset.RollingUpdateConfiguration{}
		}, "spec.rolloutStrategy.rollingUpdateConfiguration.maxUnavailable"),
		ginkgo.Entry("rejects indivisible subgroups", func(l *leaderworkerset.LeaderWorkerSet) {
			l.Spec.LeaderWorkerTemplate.Size = ptr.To[int32](5)
			l.Spec.LeaderWorkerTemplate.SubGroupPolicy = &leaderworkerset.SubGroupPolicy{SubGroupSize: ptr.To[int32](3)}
		}, "spec.leaderWorkerTemplate.subGroupPolicy.subGroupSize"),
		ginkgo.Entry("requires subgroup size", func(l *leaderworkerset.LeaderWorkerSet) {
			l.Spec.LeaderWorkerTemplate.SubGroupPolicy = &leaderworkerset.SubGroupPolicy{}
		}, "spec.leaderWorkerTemplate.subGroupPolicy.subGroupSize"),
		ginkgo.Entry("requires subgroup policy for exclusive topology", func(l *leaderworkerset.LeaderWorkerSet) {
			l.Annotations = map[string]string{leaderworkerset.SubGroupExclusiveKeyAnnotationKey: "rack"}
		}, "metadata.annotations"),
		ginkgo.Entry("requires subgroup policy for an empty exclusive topology annotation", func(l *leaderworkerset.LeaderWorkerSet) {
			l.Annotations = map[string]string{leaderworkerset.SubGroupExclusiveKeyAnnotationKey: ""}
		}, "metadata.annotations"),
		ginkgo.Entry("admits defaulted size and replicas", func(l *leaderworkerset.LeaderWorkerSet) {
			l.Spec.LeaderWorkerTemplate.Size = nil
			l.Spec.Replicas = nil
		}, ""),
		ginkgo.Entry("allows replacement to enable subgroups", func(l *leaderworkerset.LeaderWorkerSet) {
			l.Spec.LeaderWorkerTemplate.SubGroupPolicy = &leaderworkerset.SubGroupPolicy{SubGroupSize: ptr.To[int32](2)}
		}, ""),
		ginkgo.Entry("allows replacement to change group identity", func(l *leaderworkerset.LeaderWorkerSet) {
			l.Spec.GroupIdentity = leaderworkerset.GroupIdentityHash
		}, ""),
	)

	ginkgo.It("ignores inline External replicas without skipping template validation", func() {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "external-contract-"}}
		gomega.Expect(k8sClient.Create(ctx, ns)).To(gomega.Succeed())
		lws := wrappers.BuildLeaderWorkerSet(ns.Name).Size(1000000).Replica(100000).Obj()
		disagg := disaggregatedSetTemplate(lws)
		for i := range disagg.Spec.Roles {
			disagg.Spec.Roles[i].Scaling = &disaggregatedset.RoleScaling{Mode: disaggregatedset.RoleScalingExternal}
		}
		gomega.Expect(k8sClient.Create(ctx, disagg)).To(gomega.Succeed())
		expectGeneratedLeaderWorkerSets(ctx, disagg)
		disagg.Spec.Roles[1].Spec.LeaderWorkerTemplate.Size = ptr.To[int32](0)
		err := k8sClient.Update(ctx, disagg, client.DryRunAll)
		gomega.Expect(err).To(gomega.HaveOccurred())
		gomega.Expect(err.Error()).To(gomega.ContainSubstring("spec.roles[1].spec.leaderWorkerTemplate.size"))

		role := &disagg.Spec.Roles[1]
		role.Spec.LeaderWorkerTemplate.Size = ptr.To[int32](3)
		role.Spec.Replicas = ptr.To[int32](0)
		role.Spec.RolloutStrategy.RollingUpdateConfiguration = &leaderworkerset.RollingUpdateConfiguration{
			MaxUnavailable: intstr.FromInt32(-1),
		}
		create := disagg.DeepCopy()
		create.ResourceVersion = ""
		create.Name = "external-create"
		for _, err := range []error{
			k8sClient.Create(ctx, create, client.DryRunAll),
			k8sClient.Update(ctx, disagg, client.DryRunAll),
		} {
			gomega.Expect(err).To(gomega.HaveOccurred())
			gomega.Expect(err.Error()).To(gomega.ContainSubstring("spec.roles[1].spec.rolloutStrategy.rollingUpdateConfiguration.maxUnavailable"))
		}
	})

	ginkgo.DescribeTable("validates metadata used by generated children",
		func(mutate func(*disaggregatedset.DisaggregatedSet), wantField string) {
			ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "metadata-contract-"}}
			gomega.Expect(k8sClient.Create(ctx, ns)).To(gomega.Succeed())
			disagg := disaggregatedSetTemplate(wrappers.BuildLeaderWorkerSet(ns.Name).Obj())
			gomega.Expect(k8sClient.Create(ctx, disagg)).To(gomega.Succeed())
			originalName := disagg.Name
			mutate(disagg)
			create := disagg.DeepCopy()
			create.ResourceVersion = ""
			if create.Name == originalName {
				create.Name = "metadata-create"
			}
			err := k8sClient.Create(ctx, create, client.DryRunAll)
			if err == nil {
				expectGeneratedLeaderWorkerSets(ctx, create)
			}
			gomega.Expect(err).To(gomega.HaveOccurred())
			gomega.Expect(err.Error()).To(gomega.ContainSubstring(wantField))
			if disagg.Name == originalName {
				err = k8sClient.Update(ctx, disagg, client.DryRunAll)
				gomega.Expect(err).To(gomega.HaveOccurred())
				gomega.Expect(err.Error()).To(gomega.ContainSubstring(wantField))
			}
		},
		ginkgo.Entry("rejects a numeric name prefix", func(d *disaggregatedset.DisaggregatedSet) {
			d.Name = "1template"
		}, "metadata.name"),
		ginkgo.Entry("rejects dots in the generated name", func(d *disaggregatedset.DisaggregatedSet) {
			d.Name = "template.check"
		}, "metadata.name"),
		ginkgo.Entry("rejects invalid role labels", func(d *disaggregatedset.DisaggregatedSet) {
			d.Spec.Roles[1].Labels = map[string]string{"invalid key": "value"}
		}, "spec.roles[1].metadata.labels"),
		ginkgo.Entry("rejects invalid role annotations", func(d *disaggregatedset.DisaggregatedSet) {
			d.Spec.Roles[1].Annotations = map[string]string{"invalid key": "value"}
		}, "spec.roles[1].metadata.annotations"),
	)
})

var _ = ginkgo.Describe("DisaggregatedSet legacy admission compatibility", func() {
	newDisaggregatedSet := func() *disaggregatedset.DisaggregatedSet {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "legacy-template-"}}
		gomega.Expect(k8sClient.Create(ctx, ns)).To(gomega.Succeed())
		ds := disaggregatedSetTemplate(wrappers.BuildLeaderWorkerSet(ns.Name).Replica(1).Size(3).Obj())
		ds.Name = "legacy"
		ds.Finalizers = []string{"example.com/cleanup"}
		return ds
	}

	ginkgo.DescribeTable("preserves unchanged legacy data during updates and cleanup",
		func(mutate func(*disaggregatedset.DisaggregatedSet), wantField string) {
			ds := newDisaggregatedSet()
			mutate(ds)
			createLegacyDisaggregatedSet(ds, wantField)
			originalSpec := ds.Spec.DeepCopy()
			ds.Annotations = map[string]string{"example.com/reconciled": "true"}
			gomega.Expect(k8sClient.Update(ctx, ds)).To(gomega.Succeed())
			gomega.Expect(ds.Spec).To(gomega.Equal(*originalSpec))

			slices.Reverse(ds.Spec.Roles)
			gomega.Expect(k8sClient.Update(ctx, ds)).To(gomega.Succeed())
			ds.Spec.Roles[0].Spec.LeaderWorkerTemplate.WorkerTemplate.Spec.Containers[0].Image = "nginx:1.27"
			gomega.Expect(k8sClient.Update(ctx, ds)).To(gomega.Succeed())

			invalid := ds.DeepCopy()
			newRole := invalid.Spec.Roles[0].DeepCopy()
			newRole.Name = "new-role"
			newRole.Spec.LeaderWorkerTemplate.Size = ptr.To[int32](5)
			newRole.Spec.LeaderWorkerTemplate.SubGroupPolicy = &leaderworkerset.SubGroupPolicy{SubGroupSize: ptr.To[int32](3)}
			invalid.Spec.Roles = append(invalid.Spec.Roles, *newRole)
			err := k8sClient.Update(ctx, invalid, client.DryRunAll)
			gomega.Expect(err).To(gomega.HaveOccurred())
			gomega.Expect(err.Error()).To(gomega.ContainSubstring("spec.roles[2].spec.leaderWorkerTemplate.subGroupPolicy.subGroupSize"))

			if ds.Spec.Roles[1].Labels["invalid key"] != "" || ds.Spec.Roles[1].Annotations["invalid key"] != "" {
				ds.Spec.Roles[1].Labels, ds.Spec.Roles[1].Annotations = nil, nil
				gomega.Expect(k8sClient.Update(ctx, ds)).To(gomega.Succeed())
			}
			for _, annotations := range []map[string]string{
				{"example.com/note": "added"},
				{"example.com/note": "changed"},
				nil,
			} {
				ds.Spec.Roles[1].Annotations = annotations
				gomega.Expect(k8sClient.Update(ctx, ds)).To(gomega.Succeed())
			}
			// Partitioned roles ignore the parent target, including large values.
			if len(ds.Spec.Roles[1].SubRoles) > 0 {
				slices.Reverse(ds.Spec.Roles[1].SubRoles)
				ds.Spec.Roles[1].Spec.Replicas = ptr.To[int32](1000000)
				gomega.Expect(k8sClient.Update(ctx, ds)).To(gomega.Succeed())
				ds.Spec.Roles[1].SubRoles[0].Scaling = &disaggregatedset.RoleScaling{Mode: disaggregatedset.RoleScalingStatic}
				gomega.Expect(k8sClient.Update(ctx, ds)).To(gomega.Succeed())
				ds.Spec.Roles[1].SubRoles[0].Scaling = nil
				gomega.Expect(k8sClient.Update(ctx, ds)).To(gomega.Succeed())
			}
			for i := range ds.Spec.Roles {
				ds.Spec.Roles[i].Spec.Replicas = ptr.To[int32](0)
				for j := range ds.Spec.Roles[i].SubRoles {
					ds.Spec.Roles[i].SubRoles[j].Replicas = ptr.To[int32](0)
				}
			}
			gomega.Expect(k8sClient.Update(ctx, ds)).To(gomega.Succeed())
			gomega.Expect(k8sClient.Delete(ctx, ds)).To(gomega.Succeed())
			gomega.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(ds), ds)).To(gomega.Succeed())
			gomega.Expect(ds.DeletionTimestamp).NotTo(gomega.BeNil())
			ds.Finalizers = nil
			gomega.Expect(k8sClient.Update(ctx, ds)).To(gomega.Succeed())
			gomega.Eventually(func() bool {
				err := k8sClient.Get(ctx, client.ObjectKeyFromObject(ds), &disaggregatedset.DisaggregatedSet{})
				if err != nil {
					gomega.Expect(apierrors.IsNotFound(err)).To(gomega.BeTrue())
				}
				return apierrors.IsNotFound(err)
			}).Should(gomega.BeTrue())
		},
		ginkgo.Entry("dotted name", func(ds *disaggregatedset.DisaggregatedSet) {
			ds.Name = "legacy.set"
		}, "metadata.name"),
		ginkgo.Entry("invalid copied metadata", func(ds *disaggregatedset.DisaggregatedSet) {
			ds.Spec.Roles[0].Labels = map[string]string{"invalid key": "value"}
			ds.Spec.Roles[0].Annotations = map[string]string{"invalid key": "value"}
		}, "metadata.annotations"),
		ginkgo.Entry("indivisible subgroups", func(ds *disaggregatedset.DisaggregatedSet) {
			ds.Spec.Roles[0].Annotations = map[string]string{"invalid key": "value"}
			ds.Spec.Roles[0].Spec.LeaderWorkerTemplate.Size = ptr.To[int32](5)
			ds.Spec.Roles[0].Spec.LeaderWorkerTemplate.SubGroupPolicy = &leaderworkerset.SubGroupPolicy{SubGroupSize: ptr.To[int32](3)}
			ds.Spec.Roles[0].Spec.RolloutStrategy.RollingUpdateConfiguration = nil
		}, "subGroupPolicy.subGroupSize"),
		ginkgo.Entry("External zero budgets", func(ds *disaggregatedset.DisaggregatedSet) {
			ds.Spec.Roles[0].Scaling = &disaggregatedset.RoleScaling{Mode: disaggregatedset.RoleScalingExternal}
			ds.Spec.Roles[0].Spec.Replicas = ptr.To[int32](4)
			ds.Spec.Roles[0].Spec.RolloutStrategy.RollingUpdateConfiguration = &leaderworkerset.RollingUpdateConfiguration{}
		}, "rollingUpdateConfiguration.maxUnavailable"),
		ginkgo.Entry("excessive physical pod count", func(ds *disaggregatedset.DisaggregatedSet) {
			ds.Spec.Roles[0].Spec.Replicas = ptr.To[int32](1000000)
			ds.Spec.Roles[0].Spec.LeaderWorkerTemplate.Size = ptr.To[int32](2148)
		}, "product of replicas"),
		ginkgo.Entry("sub-role template", func(ds *disaggregatedset.DisaggregatedSet) {
			ds.Spec.Roles[0].SubRoles = []disaggregatedset.DisaggregatedSubRoleSpec{{Name: "hot"}, {Name: "cold"}}
			ds.Spec.Roles[0].Spec.LeaderWorkerTemplate.Size = ptr.To[int32](5)
			ds.Spec.Roles[0].Spec.LeaderWorkerTemplate.SubGroupPolicy = &leaderworkerset.SubGroupPolicy{SubGroupSize: ptr.To[int32](3)}
		}, "subGroupPolicy.subGroupSize"),
	)

	ginkgo.It("deletes a positive legacy External role without repair or scale-to-zero", func() {
		ds := newDisaggregatedSet()
		ds.Spec.Roles[0].Scaling = &disaggregatedset.RoleScaling{Mode: disaggregatedset.RoleScalingExternal}
		ds.Spec.Roles[0].Spec.Replicas = ptr.To[int32](4)
		ds.Spec.Roles[0].Spec.RolloutStrategy.RollingUpdateConfiguration = &leaderworkerset.RollingUpdateConfiguration{}
		createLegacyDisaggregatedSet(ds, "rollingUpdateConfiguration.maxUnavailable")
		manager := disaggregatedsetcontroller.NewScalerManager(k8sClient, events.NewFakeRecorder(10))
		scalers, err := manager.Reconcile(ctx, ds, func(string) (int32, error) { return 4, nil })
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(scalers[ds.Spec.Roles[0].Name].Spec.Replicas).To(gomega.Equal(int32(4)))
		originalSpec := ds.Spec.DeepCopy()
		gomega.Expect(k8sClient.Delete(ctx, ds)).To(gomega.Succeed())
		gomega.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(ds), ds)).To(gomega.Succeed())
		gomega.Expect(ds.DeletionTimestamp).NotTo(gomega.BeNil())
		gomega.Expect(ds.Spec).To(gomega.Equal(*originalSpec))
		ds.Finalizers = nil
		gomega.Expect(k8sClient.Update(ctx, ds)).To(gomega.Succeed())
		gomega.Eventually(func() bool {
			err := k8sClient.Get(ctx, client.ObjectKeyFromObject(ds), &disaggregatedset.DisaggregatedSet{})
			if err != nil {
				gomega.Expect(apierrors.IsNotFound(err)).To(gomega.BeTrue())
			}
			return apierrors.IsNotFound(err)
		}).Should(gomega.BeTrue())
	})

	ginkgo.It("requires changed or renamed legacy templates to become valid", func() {
		ds := newDisaggregatedSet()
		ds.Spec.Roles[0].Spec.LeaderWorkerTemplate.Size = ptr.To[int32](5)
		ds.Spec.Roles[0].Spec.LeaderWorkerTemplate.SubGroupPolicy = &leaderworkerset.SubGroupPolicy{SubGroupSize: ptr.To[int32](3)}
		createLegacyDisaggregatedSet(ds, "subGroupPolicy.subGroupSize")
		for _, mutate := range []func(*disaggregatedset.DisaggregatedSet){
			func(d *disaggregatedset.DisaggregatedSet) { d.Spec.Roles[0].Name = "renamed" },
			func(d *disaggregatedset.DisaggregatedSet) {
				d.Spec.Roles[0].Spec.LeaderWorkerTemplate.Size = ptr.To[int32](8)
			},
			func(d *disaggregatedset.DisaggregatedSet) { d.Spec.Roles[0].Spec.Replicas = ptr.To[int32](2) },
			func(d *disaggregatedset.DisaggregatedSet) {
				d.Spec.Roles[0].Scaling = &disaggregatedset.RoleScaling{Mode: disaggregatedset.RoleScalingExternal}
			},
			func(d *disaggregatedset.DisaggregatedSet) {
				d.Spec.Roles[0].Spec.LeaderWorkerTemplate.Size = ptr.To[int32](8)
				for i := range d.Spec.Roles {
					d.Spec.Roles[i].Spec.Replicas = ptr.To[int32](0)
				}
			},
		} {
			invalid := ds.DeepCopy()
			mutate(invalid)
			err := k8sClient.Update(ctx, invalid, client.DryRunAll)
			gomega.Expect(err).To(gomega.HaveOccurred())
			gomega.Expect(err.Error()).To(gomega.ContainSubstring("subGroupPolicy.subGroupSize"))
		}
		ds.Spec.Roles[0].Spec.LeaderWorkerTemplate.Size = ptr.To[int32](6)
		gomega.Expect(k8sClient.Update(ctx, ds)).To(gomega.Succeed())
	})

	ginkgo.It("revalidates legacy templates when sub-role inputs change", func() {
		ds := newDisaggregatedSet()
		ds.Spec.Roles[0].SubRoles = []disaggregatedset.DisaggregatedSubRoleSpec{{Name: "hot"}, {Name: "cold"}}
		ds.Spec.Roles[0].Spec.LeaderWorkerTemplate.Size = ptr.To[int32](5)
		ds.Spec.Roles[0].Spec.LeaderWorkerTemplate.SubGroupPolicy = &leaderworkerset.SubGroupPolicy{SubGroupSize: ptr.To[int32](3)}
		createLegacyDisaggregatedSet(ds, "subGroupPolicy.subGroupSize")
		for _, mutate := range []func(*disaggregatedset.DisaggregatedRoleSpec){
			func(r *disaggregatedset.DisaggregatedRoleSpec) { r.SubRoles[0].Name = "renamed" },
			func(r *disaggregatedset.DisaggregatedRoleSpec) { r.SubRoles[0].Replicas = ptr.To[int32](2) },
			func(r *disaggregatedset.DisaggregatedRoleSpec) { r.SubRoles[0].Replicas = ptr.To[int32](0) },
			func(r *disaggregatedset.DisaggregatedRoleSpec) {
				r.SubRoles[0].Scaling = &disaggregatedset.RoleScaling{Mode: disaggregatedset.RoleScalingExternal}
			},
			func(r *disaggregatedset.DisaggregatedRoleSpec) { r.SubRoles = nil },
			func(r *disaggregatedset.DisaggregatedRoleSpec) {
				r.SubRoles = append(r.SubRoles, disaggregatedset.DisaggregatedSubRoleSpec{Name: "new"})
			},
			func(r *disaggregatedset.DisaggregatedRoleSpec) {
				r.Scaling = &disaggregatedset.RoleScaling{Mode: disaggregatedset.RoleScalingStatic}
			},
			func(r *disaggregatedset.DisaggregatedRoleSpec) { r.SubRoles[0].MaxSurge = ptr.To(intstr.FromInt32(2)) },
		} {
			invalid := ds.DeepCopy()
			mutate(&invalid.Spec.Roles[0])
			err := k8sClient.Update(ctx, invalid, client.DryRunAll)
			gomega.Expect(err).To(gomega.HaveOccurred())
			gomega.Expect(err.Error()).To(gomega.ContainSubstring("subGroupPolicy.subGroupSize"))
		}
	})

	ginkgo.DescribeTable("revalidates percentage budgets on positive replica changes",
		func(subRoles bool) {
			ds := newDisaggregatedSet()
			for i := range ds.Spec.Roles {
				role := &ds.Spec.Roles[i]
				role.Spec.Replicas = ptr.To[int32](2)
				role.Spec.RolloutStrategy.RollingUpdateConfiguration = &leaderworkerset.RollingUpdateConfiguration{
					MaxUnavailable: intstr.FromString("50%"),
				}
				if subRoles {
					role.SubRoles = []disaggregatedset.DisaggregatedSubRoleSpec{{Name: "hot"}, {Name: "cold"}}
				}
			}
			gomega.Expect(k8sClient.Create(ctx, ds)).To(gomega.Succeed())
			for i := range ds.Spec.Roles {
				if subRoles {
					ds.Spec.Roles[i].SubRoles[0].Replicas = ptr.To[int32](0)
				} else {
					ds.Spec.Roles[i].Spec.Replicas = ptr.To[int32](1)
				}
			}
			err := k8sClient.Update(ctx, ds, client.DryRunAll)
			gomega.Expect(err).To(gomega.HaveOccurred())
			gomega.Expect(err.Error()).To(gomega.ContainSubstring("rollingUpdateConfiguration.maxUnavailable"))
			for i := range ds.Spec.Roles {
				ds.Spec.Roles[i].Spec.Replicas = ptr.To[int32](0)
				for j := range ds.Spec.Roles[i].SubRoles {
					ds.Spec.Roles[i].SubRoles[j].Replicas = ptr.To[int32](0)
				}
			}
			gomega.Expect(k8sClient.Update(ctx, ds)).To(gomega.Succeed())
		},
		ginkgo.Entry("role replicas", false),
		ginkgo.Entry("sub-role aggregate replicas", true),
	)

	ginkgo.It("preserves live replicas when switching a Static role to External", func() {
		ds := newDisaggregatedSet()
		ds.Annotations = map[string]string{disaggregatedset.RevisionHashVersionAnnotationKey: disaggregatedset.RevisionHashVersion}
		for i := range ds.Spec.Roles {
			ds.Spec.Roles[i].Spec.Replicas = ptr.To[int32](4)
			ds.Spec.Roles[i].Spec.RolloutStrategy.RollingUpdateConfiguration = &leaderworkerset.RollingUpdateConfiguration{
				MaxUnavailable: intstr.FromString("50%"),
			}
		}
		gomega.Expect(k8sClient.Create(ctx, ds)).To(gomega.Succeed())
		recorder := events.NewFakeRecorder(100)
		reconciler := disaggregatedsetcontroller.NewDisaggregatedSetReconciler(k8sClient, k8sClient.Scheme(), recorder)
		request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(ds)}
		result, err := reconciler.Reconcile(ctx, request)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(result.RequeueAfter).To(gomega.BeNumerically(">", 0))
		// The first pass creates empty children; the requested retry scales them.
		_, err = reconciler.Reconcile(ctx, request)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		roleName := ds.Spec.Roles[0].Name
		childName := disaggregatedsetutils.GenerateName(ds.Name, 0, disaggregatedsetutils.ComputeRevision(ds.Spec.Roles), roleName)
		child := &leaderworkerset.LeaderWorkerSet{}
		gomega.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: childName, Namespace: ds.Namespace}, child)).To(gomega.Succeed())
		gomega.Expect(ptr.Deref(child.Spec.Replicas, 0)).To(gomega.Equal(int32(4)))
		childUID := child.UID

		gomega.Expect(k8sClient.Get(ctx, request.NamespacedName, ds)).To(gomega.Succeed())
		ds.Spec.Roles[0].Scaling = &disaggregatedset.RoleScaling{Mode: disaggregatedset.RoleScalingExternal}
		gomega.Expect(k8sClient.Update(ctx, ds)).To(gomega.Succeed())
		_, err = reconciler.Reconcile(ctx, request)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		scaler := &disaggregatedset.DisaggregatedSetRoleScaler{}
		gomega.Expect(k8sClient.Get(ctx, types.NamespacedName{
			Name: disaggregatedsetcontroller.ScalerName(ds.Name, roleName), Namespace: ds.Namespace,
		}, scaler)).To(gomega.Succeed())
		gomega.Expect(scaler.Spec.Replicas).To(gomega.Equal(int32(4)))
		gomega.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(child), child)).To(gomega.Succeed())
		gomega.Expect(child.UID).To(gomega.Equal(childUID))
		gomega.Expect(ptr.Deref(child.Spec.Replicas, 0)).To(gomega.Equal(int32(4)))
		unavailable, err := intstr.GetScaledValueFromIntOrPercent(
			&child.Spec.RolloutStrategy.RollingUpdateConfiguration.MaxUnavailable, int(scaler.Spec.Replicas), false)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(unavailable).To(gomega.Equal(2))
	})
})

func expectDisaggregatedSetRoleTemplateValidation(lws *leaderworkerset.LeaderWorkerSet, mutate func(*leaderworkerset.LeaderWorkerSet), wantField string) {
	ginkgo.GinkgoHelper()
	disagg := disaggregatedSetTemplate(lws)
	gomega.Expect(k8sClient.Create(ctx, disagg)).To(gomega.Succeed())
	mutate(lws)
	lwsErr := k8sClient.Create(ctx, lws.DeepCopy(), client.DryRunAll)
	if wantField == "" {
		gomega.Expect(lwsErr).NotTo(gomega.HaveOccurred())
	} else {
		gomega.Expect(lwsErr).To(gomega.HaveOccurred())
		gomega.Expect(lwsErr.Error()).To(gomega.ContainSubstring(wantField))
	}
	// Keep prefill valid to verify that validation also reaches later roles.
	disagg.Spec.Roles[1].Spec = *lws.Spec.DeepCopy()
	disagg.Spec.Roles[1].ObjectMeta.Annotations = lws.Annotations
	create := disagg.DeepCopy()
	create.ResourceVersion = ""
	create.Name = "template-create"
	for _, err := range []error{
		k8sClient.Create(ctx, create, client.DryRunAll),
		k8sClient.Update(ctx, disagg, client.DryRunAll),
	} {
		if wantField == "" {
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		} else {
			gomega.Expect(err).To(gomega.HaveOccurred())
			gomega.Expect(err.Error()).To(gomega.ContainSubstring("spec.roles[1]." + wantField))
		}
	}
	if wantField == "" {
		expectGeneratedLeaderWorkerSets(ctx, create)
	}
}

// createLegacyDisaggregatedSet seeds an object accepted by an earlier webhook,
// while retaining current CRD validation. Only its namespace is temporarily
// excluded, and the current webhook must reject a matching create afterward.
func createLegacyDisaggregatedSet(ds *disaggregatedset.DisaggregatedSet, wantField string) {
	ginkgo.GinkgoHelper()
	gomega.Expect(admissionregistrationv1.AddToScheme(k8sClient.Scheme())).To(gomega.Succeed())
	var configs admissionregistrationv1.ValidatingWebhookConfigurationList
	gomega.Expect(k8sClient.List(ctx, &configs)).To(gomega.Succeed())
	var configName string
	var hookIndex int
	var originalSelector *metav1.LabelSelector
	for _, config := range configs.Items {
		for i, hook := range config.Webhooks {
			if hook.Name == "vdisaggregatedset.kb.io" {
				configName, hookIndex = config.Name, i
				originalSelector = hook.NamespaceSelector.DeepCopy()
			}
		}
	}
	gomega.Expect(configName).NotTo(gomega.BeEmpty())
	setSelector := func(selector *metav1.LabelSelector) {
		var config admissionregistrationv1.ValidatingWebhookConfiguration
		gomega.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: configName}, &config)).To(gomega.Succeed())
		config.Webhooks[hookIndex].NamespaceSelector = selector
		gomega.Expect(k8sClient.Update(ctx, &config)).To(gomega.Succeed())
	}
	selector := originalSelector.DeepCopy()
	if selector == nil {
		selector = &metav1.LabelSelector{}
	}
	selector.MatchExpressions = append(selector.MatchExpressions, metav1.LabelSelectorRequirement{
		Key: corev1.LabelMetadataName, Operator: metav1.LabelSelectorOpNotIn, Values: []string{ds.Namespace},
	})
	setSelector(selector)
	ginkgo.DeferCleanup(func() { setSelector(originalSelector) })
	gomega.Eventually(func() error {
		err := k8sClient.Create(ctx, ds.DeepCopy(), client.DryRunAll)
		if err != nil {
			gomega.Expect(apierrors.IsInvalid(err) || apierrors.IsForbidden(err)).To(gomega.BeTrue())
			gomega.Expect(err.Error()).To(gomega.ContainSubstring(wantField))
		}
		return err
	}, "10s", "100ms").Should(gomega.Succeed())
	gomega.Expect(k8sClient.Create(ctx, ds)).To(gomega.Succeed())
	setSelector(originalSelector)
	gomega.Eventually(func() string {
		probe := ds.DeepCopy()
		probe.Name += "-copy"
		probe.ResourceVersion = ""
		err := k8sClient.Create(ctx, probe, client.DryRunAll)
		if err == nil {
			return ""
		}
		gomega.Expect(apierrors.IsInvalid(err) || apierrors.IsForbidden(err)).To(gomega.BeTrue())
		return err.Error()
	}, "10s", "100ms").Should(gomega.ContainSubstring(wantField))
}

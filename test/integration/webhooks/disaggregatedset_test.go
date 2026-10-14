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

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"

	disaggregatedset "sigs.k8s.io/lws/api/disaggregatedset/v1"
	leaderworkerset "sigs.k8s.io/lws/api/leaderworkerset/v1"
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

	ginkgo.DescribeTable("rollout scale-down policy defaulting and validation",
		func(policy *disaggregatedset.DisaggregatedSetScalingPolicy, want disaggregatedset.RolloutScaleDownPolicy, valid bool) {
			disagg := buildDisaggregatedSet().Obj()
			disagg.Spec.ScalingPolicy = policy
			err := k8sClient.Create(ctx, disagg)
			if !valid {
				gomega.Expect(apierrors.IsInvalid(err)).To(gomega.BeTrue())
				gomega.Expect(err.Error()).To(gomega.ContainSubstring("scaleDownDuringRollout"))
				return
			}
			gomega.Expect(err).To(gomega.Succeed())
			var fetched disaggregatedset.DisaggregatedSet
			gomega.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: disagg.Name, Namespace: ns.Name}, &fetched)).To(gomega.Succeed())
			if policy == nil {
				gomega.Expect(fetched.Spec.ScalingPolicy).To(gomega.BeNil())
				return
			}
			gomega.Expect(fetched.Spec.ScalingPolicy).NotTo(gomega.BeNil())
			gomega.Expect(fetched.Spec.ScalingPolicy.ScaleDownDuringRollout).To(gomega.Equal(want))
		},
		ginkgo.Entry("omitted policy", nil, disaggregatedset.RolloutScaleDownPolicy(""), true),
		ginkgo.Entry("empty policy defaults to Deferred", &disaggregatedset.DisaggregatedSetScalingPolicy{}, disaggregatedset.RolloutScaleDownDeferred, true),
		ginkgo.Entry("explicit Deferred", &disaggregatedset.DisaggregatedSetScalingPolicy{ScaleDownDuringRollout: disaggregatedset.RolloutScaleDownDeferred}, disaggregatedset.RolloutScaleDownDeferred, true),
		ginkgo.Entry("explicit Immediate", &disaggregatedset.DisaggregatedSetScalingPolicy{ScaleDownDuringRollout: disaggregatedset.RolloutScaleDownImmediate}, disaggregatedset.RolloutScaleDownImmediate, true),
		ginkgo.Entry("invalid policy", &disaggregatedset.DisaggregatedSetScalingPolicy{ScaleDownDuringRollout: "invalid"}, disaggregatedset.RolloutScaleDownPolicy(""), false),
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
				{Name: role.Name, Replicas: 1, ReadyReplicas: 1, UpdatedReplicas: 1, SubRoleStatuses: []disaggregatedset.SubRoleStatus{
					{Name: "hot", Replicas: 1, ReadyReplicas: 1, UpdatedReplicas: 1},
					{Name: "cold"},
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

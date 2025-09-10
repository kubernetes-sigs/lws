/*
Copyright 2023 The Kubernetes Authors.
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

	"github.com/google/go-cmp/cmp"
	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"

	leaderworkerset "sigs.k8s.io/lws/api/leaderworkerset/v1"
	"sigs.k8s.io/lws/test/wrappers"
)

var _ = ginkgo.Describe("leaderworkerset defaulting, creation and update", func() {

	// Each test runs in a separate namespace.
	var ns *corev1.Namespace
	ginkgo.BeforeEach(func() {
		// Create test namespace before each test.
		ns = &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: "test-ns-",
			},
		}
		gomega.Expect(k8sClient.Create(ctx, ns)).To(gomega.Succeed())

		gomega.Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns.Namespace, Name: ns.Name}, ns)).Should(gomega.Succeed())
	})
	type testDefaultingCase struct {
		makeLeaderWorkerSet func(ns *corev1.Namespace) *wrappers.LeaderWorkerSetWrapper
		getExpectedLWS      func(lws *leaderworkerset.LeaderWorkerSet) *wrappers.LeaderWorkerSetWrapper
	}
	ginkgo.DescribeTable("test defaulting",
		func(tc *testDefaultingCase) {
			ctx := context.Background()

			// Create LeaderWorkerSet object.
			ginkgo.By("creating LeaderWorkerSet ")
			lws := tc.makeLeaderWorkerSet(ns).Obj()
			gomega.Expect(k8sClient.Create(ctx, lws)).Should(gomega.Succeed())

			var fetchedLWS leaderworkerset.LeaderWorkerSet
			gomega.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: lws.Name, Namespace: lws.Namespace}, &fetchedLWS)).Should(gomega.Succeed())
			expectedLWS := tc.getExpectedLWS(lws)
			gomega.Expect(cmp.Diff(expectedLWS.Spec, fetchedLWS.Spec)).Should(gomega.Equal(""))
		},
		ginkgo.Entry("apply defaulting logic for replicas", &testDefaultingCase{
			makeLeaderWorkerSet: func(ns *corev1.Namespace) *wrappers.LeaderWorkerSetWrapper {
				lwsWrapper := wrappers.BuildLeaderWorkerSet(ns.Name)
				lwsWrapper.Spec.Replicas = nil
				return lwsWrapper
			},
			getExpectedLWS: func(lws *leaderworkerset.LeaderWorkerSet) *wrappers.LeaderWorkerSetWrapper {
				return wrappers.BuildLeaderWorkerSet(ns.Name).Replica(1).RestartPolicy(leaderworkerset.RecreateGroupOnPodRestart)
			},
		}),
		ginkgo.Entry("apply defaulting logic for size", &testDefaultingCase{
			makeLeaderWorkerSet: func(ns *corev1.Namespace) *wrappers.LeaderWorkerSetWrapper {
				lwsWrapper := wrappers.BuildLeaderWorkerSet(ns.Name)
				lwsWrapper.Spec.LeaderWorkerTemplate.Size = nil
				return lwsWrapper
			},
			getExpectedLWS: func(lws *leaderworkerset.LeaderWorkerSet) *wrappers.LeaderWorkerSetWrapper {
				return wrappers.BuildLeaderWorkerSet(ns.Name).Size(1).RestartPolicy(leaderworkerset.RecreateGroupOnPodRestart)
			},
		}),
		ginkgo.Entry("defaulting logic won't apply when shouldn't", &testDefaultingCase{
			makeLeaderWorkerSet: func(ns *corev1.Namespace) *wrappers.LeaderWorkerSetWrapper {
				return wrappers.BuildLeaderWorkerSet(ns.Name).Replica(2).Size(2)
			},
			getExpectedLWS: func(lws *leaderworkerset.LeaderWorkerSet) *wrappers.LeaderWorkerSetWrapper {
				return wrappers.BuildLeaderWorkerSet(ns.Name).Replica(2).Size(2).LeaderTemplateSpec(wrappers.MakeLeaderPodSpec()).WorkerTemplateSpec(wrappers.MakeWorkerPodSpec()).RestartPolicy(leaderworkerset.RecreateGroupOnPodRestart)
			},
		}),
		ginkgo.Entry("defaulting logic applies when leaderworkertemplate.restartpolicy is not set", &testDefaultingCase{
			makeLeaderWorkerSet: func(ns *corev1.Namespace) *wrappers.LeaderWorkerSetWrapper {
				return wrappers.BuildLeaderWorkerSet(ns.Name).Replica(2).Size(2).RestartPolicy("")
			},
			getExpectedLWS: func(lws *leaderworkerset.LeaderWorkerSet) *wrappers.LeaderWorkerSetWrapper {
				return wrappers.BuildLeaderWorkerSet(ns.Name).Replica(2).Size(2).RestartPolicy(leaderworkerset.RecreateGroupOnPodRestart)
			},
		}),
		ginkgo.Entry("defaulting logic won't apply when leaderworkertemplate.restartpolicy is set", &testDefaultingCase{
			makeLeaderWorkerSet: func(ns *corev1.Namespace) *wrappers.LeaderWorkerSetWrapper {
				return wrappers.BuildLeaderWorkerSet(ns.Name).Replica(2).Size(2).RestartPolicy(leaderworkerset.NoneRestartPolicy)
			},
			getExpectedLWS: func(lws *leaderworkerset.LeaderWorkerSet) *wrappers.LeaderWorkerSetWrapper {
				return wrappers.BuildLeaderWorkerSet(ns.Name).Replica(2).Size(2).RestartPolicy(leaderworkerset.NoneRestartPolicy)
			},
		}),
		ginkgo.Entry("DeprecatedDefaultRestartPolicy will be shift to NoneRestartPolicy", &testDefaultingCase{
			makeLeaderWorkerSet: func(ns *corev1.Namespace) *wrappers.LeaderWorkerSetWrapper {
				return wrappers.BuildLeaderWorkerSet(ns.Name).Replica(2).Size(2).RestartPolicy(leaderworkerset.DeprecatedDefaultRestartPolicy)
			},
			getExpectedLWS: func(lws *leaderworkerset.LeaderWorkerSet) *wrappers.LeaderWorkerSetWrapper {
				return wrappers.BuildLeaderWorkerSet(ns.Name).Replica(2).Size(2).RestartPolicy(leaderworkerset.NoneRestartPolicy)
			},
		}),
		ginkgo.Entry("defaulting logic applies when spec.startpolicy is not set", &testDefaultingCase{
			makeLeaderWorkerSet: func(ns *corev1.Namespace) *wrappers.LeaderWorkerSetWrapper {
				return wrappers.BuildLeaderWorkerSet(ns.Name).Replica(2).Size(2)
			},
			getExpectedLWS: func(lws *leaderworkerset.LeaderWorkerSet) *wrappers.LeaderWorkerSetWrapper {
				return wrappers.BuildLeaderWorkerSet(ns.Name).Replica(2).Size(2).StartupPolicy(leaderworkerset.LeaderCreatedStartupPolicy)
			},
		}),
		ginkgo.Entry("defaulting logic applies when spec.startpolicy is set", &testDefaultingCase{
			makeLeaderWorkerSet: func(ns *corev1.Namespace) *wrappers.LeaderWorkerSetWrapper {
				return wrappers.BuildLeaderWorkerSet(ns.Name).Replica(2).Size(2).StartupPolicy(leaderworkerset.LeaderReadyStartupPolicy)
			},
			getExpectedLWS: func(lws *leaderworkerset.LeaderWorkerSet) *wrappers.LeaderWorkerSetWrapper {
				return wrappers.BuildLeaderWorkerSet(ns.Name).Replica(2).Size(2).StartupPolicy(leaderworkerset.LeaderReadyStartupPolicy)
			},
		}),
		ginkgo.Entry("defaulting of subdomainPolicy applies when spec.NetworkConfig is not set", &testDefaultingCase{
			makeLeaderWorkerSet: func(ns *corev1.Namespace) *wrappers.LeaderWorkerSetWrapper {
				lwsWrapper := wrappers.BuildLeaderWorkerSet(ns.Name)
				lwsWrapper.Spec.NetworkConfig = nil
				return lwsWrapper
			},
			getExpectedLWS: func(lws *leaderworkerset.LeaderWorkerSet) *wrappers.LeaderWorkerSetWrapper {
				return wrappers.BuildLeaderWorkerSet(ns.Name).SubdomainPolicy(leaderworkerset.SubdomainShared)
			},
		}),
		ginkgo.Entry("apply default rollout strategy", &testDefaultingCase{
			makeLeaderWorkerSet: func(ns *corev1.Namespace) *wrappers.LeaderWorkerSetWrapper {
				return wrappers.BuildLeaderWorkerSet(ns.Name).RolloutStrategy(leaderworkerset.RolloutStrategy{}) // unset rollout strategy
			},
			getExpectedLWS: func(lws *leaderworkerset.LeaderWorkerSet) *wrappers.LeaderWorkerSetWrapper {
				return wrappers.BuildLeaderWorkerSet(ns.Name).RestartPolicy(leaderworkerset.RecreateGroupOnPodRestart).RolloutStrategy(leaderworkerset.RolloutStrategy{
					Type: leaderworkerset.RollingUpdateStrategyType,
					RollingUpdateConfiguration: &leaderworkerset.RollingUpdateConfiguration{
						Partition:      ptr.To[int32](0),
						MaxUnavailable: intstr.FromInt32(1),
						MaxSurge:       intstr.FromInt32(0),
					}})
			},
		}),
		ginkgo.Entry("wouldn't apply default rollout strategy when configured", &testDefaultingCase{
			makeLeaderWorkerSet: func(ns *corev1.Namespace) *wrappers.LeaderWorkerSetWrapper {
				return wrappers.BuildLeaderWorkerSet(ns.Name).
					RolloutStrategy(leaderworkerset.RolloutStrategy{
						Type: leaderworkerset.RollingUpdateStrategyType,
						RollingUpdateConfiguration: &leaderworkerset.RollingUpdateConfiguration{
							Partition:      ptr.To[int32](2),
							MaxUnavailable: intstr.FromInt32(2),
							MaxSurge:       intstr.FromInt32(1),
						}})
			},
			getExpectedLWS: func(lws *leaderworkerset.LeaderWorkerSet) *wrappers.LeaderWorkerSetWrapper {
				return wrappers.BuildLeaderWorkerSet(ns.Name).
					RestartPolicy(leaderworkerset.RecreateGroupOnPodRestart).
					RolloutStrategy(leaderworkerset.RolloutStrategy{
						Type: leaderworkerset.RollingUpdateStrategyType,
						RollingUpdateConfiguration: &leaderworkerset.RollingUpdateConfiguration{
							Partition:      ptr.To[int32](2),
							MaxUnavailable: intstr.FromInt32(2),
							MaxSurge:       intstr.FromInt32(1),
						}})
			},
		}),
	)

	type testValidationCase struct {
		makeLeaderWorkerSet   func(ns *corev1.Namespace) *wrappers.LeaderWorkerSetWrapper
		lwsCreationShouldFail bool
		updateLeaderWorkerSet func(lws *leaderworkerset.LeaderWorkerSet)
		updateShouldFail      bool
	}
	ginkgo.DescribeTable("test creation and update",
		func(tc *testValidationCase) {
			ctx := context.Background()

			// Create LeaderWorkerSet object.
			ginkgo.By("creating leaderworkerset")
			lws := tc.makeLeaderWorkerSet(ns).Obj()

			// Verify lws created successfully.
			ginkgo.By("checking that leaderworkerset creation succeeds")
			if tc.lwsCreationShouldFail {
				gomega.Expect(k8sClient.Create(ctx, lws)).Should(gomega.Not(gomega.Succeed()))
				return
			}
			gomega.Expect(k8sClient.Create(ctx, lws)).Should(gomega.Succeed())

			var fetchedLWS leaderworkerset.LeaderWorkerSet
			gomega.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: lws.Name, Namespace: lws.Namespace}, &fetchedLWS)).Should(gomega.Succeed())

			if tc.updateLeaderWorkerSet != nil {
				tc.updateLeaderWorkerSet(&fetchedLWS)
				// Verify leaderworkerset created successfully.
				if tc.updateShouldFail {
					gomega.Expect(k8sClient.Update(ctx, &fetchedLWS)).Should(gomega.Not(gomega.Succeed()))
				} else {
					gomega.Expect(k8sClient.Update(ctx, &fetchedLWS)).Should(gomega.Succeed())
				}
			}
		},
		ginkgo.Entry("creation with invalid name should fail", &testValidationCase{
			makeLeaderWorkerSet: func(ns *corev1.Namespace) *wrappers.LeaderWorkerSetWrapper {
				return wrappers.BuildLeaderWorkerSet(ns.Name).Name("2cube")
			},
			lwsCreationShouldFail: true,
		}),
		ginkgo.Entry("creation with invalid replicas and size should fail", &testValidationCase{
			makeLeaderWorkerSet: func(ns *corev1.Namespace) *wrappers.LeaderWorkerSetWrapper {
				return wrappers.BuildLeaderWorkerSet(ns.Name).Replica(100000).Size(1000000)
			},
			lwsCreationShouldFail: true,
		}),
		ginkgo.Entry("creation with invalid size should fail", &testValidationCase{
			makeLeaderWorkerSet: func(ns *corev1.Namespace) *wrappers.LeaderWorkerSetWrapper {
				return wrappers.BuildLeaderWorkerSet(ns.Name).Replica(2).Size(-1)
			},
			lwsCreationShouldFail: true,
		}),
		ginkgo.Entry("creation with invalid replicas should fail", &testValidationCase{
			makeLeaderWorkerSet: func(ns *corev1.Namespace) *wrappers.LeaderWorkerSetWrapper {
				return wrappers.BuildLeaderWorkerSet(ns.Name).Size(2).Replica(-1)
			},
			lwsCreationShouldFail: true,
		}),
		ginkgo.Entry("creation with invalid startpolicy should fail", &testValidationCase{
			makeLeaderWorkerSet: func(ns *corev1.Namespace) *wrappers.LeaderWorkerSetWrapper {
				return wrappers.BuildLeaderWorkerSet(ns.Name).StartupPolicy("invalidValue")
			},
			lwsCreationShouldFail: true,
		}),
		ginkgo.Entry("creation with invalid subGroupSize should fail", &testValidationCase{
			makeLeaderWorkerSet: func(ns *corev1.Namespace) *wrappers.LeaderWorkerSetWrapper {
				return wrappers.BuildLeaderWorkerSet(ns.Name).Size(2).SubGroupSize(-1)
			},
			lwsCreationShouldFail: true,
		}),
		ginkgo.Entry("creation with subGroupSize larger than size should fail", &testValidationCase{
			makeLeaderWorkerSet: func(ns *corev1.Namespace) *wrappers.LeaderWorkerSetWrapper {
				return wrappers.BuildLeaderWorkerSet(ns.Name).Size(2).SubGroupSize(3)
			},
			lwsCreationShouldFail: true,
		}),
		ginkgo.Entry("creation where (subGroupSize-1) is not divisible by 1 and SubGroupPolicyTypeLeaderExcluded should fail", &testValidationCase{
			makeLeaderWorkerSet: func(ns *corev1.Namespace) *wrappers.LeaderWorkerSetWrapper {
				return wrappers.BuildLeaderWorkerSet(ns.Name).Size(4).SubGroupSize(2).SubGroupType(leaderworkerset.SubGroupPolicyTypeLeaderExcluded)
			},
			lwsCreationShouldFail: true,
		}),
		ginkgo.Entry("update to size should succeed", &testValidationCase{
			makeLeaderWorkerSet: func(ns *corev1.Namespace) *wrappers.LeaderWorkerSetWrapper {
				return wrappers.BuildLeaderWorkerSet(ns.Name).Size(2)
			},
			updateLeaderWorkerSet: func(lws *leaderworkerset.LeaderWorkerSet) {
				lws.Spec.LeaderWorkerTemplate.Size = ptr.To[int32](3)
			},
			updateShouldFail: false,
		}),
		ginkgo.Entry("update with invalid replicas should fail (larger than maxInt32)", &testValidationCase{
			makeLeaderWorkerSet: func(ns *corev1.Namespace) *wrappers.LeaderWorkerSetWrapper {
				return wrappers.BuildLeaderWorkerSet(ns.Name).Replica(100000).Size(1)
			},
			updateLeaderWorkerSet: func(lws *leaderworkerset.LeaderWorkerSet) {
				lws.Spec.LeaderWorkerTemplate.Size = ptr.To[int32](100000000)
			},
			updateShouldFail: true,
		}),
		ginkgo.Entry("update with invalid replicas should fail (number is negative)", &testValidationCase{
			makeLeaderWorkerSet: func(ns *corev1.Namespace) *wrappers.LeaderWorkerSetWrapper {
				return wrappers.BuildLeaderWorkerSet(ns.Name).Replica(1).Size(1)
			},
			updateLeaderWorkerSet: func(lws *leaderworkerset.LeaderWorkerSet) {
				lws.Spec.Replicas = ptr.To[int32](-1)
			},
			updateShouldFail: true,
		}),
		ginkgo.Entry("update with invalid startpolicy should fail", &testValidationCase{
			makeLeaderWorkerSet: func(ns *corev1.Namespace) *wrappers.LeaderWorkerSetWrapper {
				return wrappers.BuildLeaderWorkerSet(ns.Name).StartupPolicy(leaderworkerset.LeaderReadyStartupPolicy)
			},
			updateLeaderWorkerSet: func(lws *leaderworkerset.LeaderWorkerSet) {
				lws.Spec.StartupPolicy = "invalidValue"
			},
			updateShouldFail: true,
		}),
		ginkgo.Entry("number of subGroupSize can not be updated", &testValidationCase{
			makeLeaderWorkerSet: func(ns *corev1.Namespace) *wrappers.LeaderWorkerSetWrapper {
				return wrappers.BuildLeaderWorkerSet(ns.Name).Replica(1).Size(2).SubGroupSize(1)
			},
			updateLeaderWorkerSet: func(lws *leaderworkerset.LeaderWorkerSet) {
				lws.Spec.LeaderWorkerTemplate.SubGroupPolicy.SubGroupSize = ptr.To[int32](2)
			},
			updateShouldFail: true,
		}),
		ginkgo.Entry("number of subGroupSize cannot be added after update", &testValidationCase{
			makeLeaderWorkerSet: func(ns *corev1.Namespace) *wrappers.LeaderWorkerSetWrapper {
				return wrappers.BuildLeaderWorkerSet(ns.Name).Replica(1).Size(2)
			},
			updateLeaderWorkerSet: func(lws *leaderworkerset.LeaderWorkerSet) {
				lws.Spec.LeaderWorkerTemplate.SubGroupPolicy = &leaderworkerset.SubGroupPolicy{}
				lws.Spec.LeaderWorkerTemplate.SubGroupPolicy.SubGroupSize = ptr.To[int32](2)
			},
			updateShouldFail: true,
		}),
		ginkgo.Entry("number of subGroupSize can not be removed after update", &testValidationCase{
			makeLeaderWorkerSet: func(ns *corev1.Namespace) *wrappers.LeaderWorkerSetWrapper {
				return wrappers.BuildLeaderWorkerSet(ns.Name).Replica(1).Size(2).SubGroupSize(1)
			},
			updateLeaderWorkerSet: func(lws *leaderworkerset.LeaderWorkerSet) {
				lws.Spec.LeaderWorkerTemplate.SubGroupPolicy = nil
			},
			updateShouldFail: true,
		}),
		ginkgo.Entry("number of replicas can be updated", &testValidationCase{
			makeLeaderWorkerSet: func(ns *corev1.Namespace) *wrappers.LeaderWorkerSetWrapper {
				return wrappers.BuildLeaderWorkerSet(ns.Name).Replica(1).Size(1)
			},
			updateLeaderWorkerSet: func(lws *leaderworkerset.LeaderWorkerSet) {
				lws.Spec.Replicas = ptr.To[int32](3)
			},
			updateShouldFail: false,
		}),
		ginkgo.Entry("leader/workerTemplate can be updated", &testValidationCase{
			makeLeaderWorkerSet: func(ns *corev1.Namespace) *wrappers.LeaderWorkerSetWrapper {
				return wrappers.BuildLeaderWorkerSet(ns.Name)
			},
			updateLeaderWorkerSet: func(lws *leaderworkerset.LeaderWorkerSet) {
				lws.Spec.LeaderWorkerTemplate.LeaderTemplate.Spec = wrappers.MakeWorkerPodSpec()
				lws.Spec.LeaderWorkerTemplate.WorkerTemplate.Spec = wrappers.MakeLeaderPodSpec()
			},
			updateShouldFail: false,
		}),
		ginkgo.Entry("subdomainPolicy can be updated from UniquePerReplica to Shared", &testValidationCase{
			makeLeaderWorkerSet: func(ns *corev1.Namespace) *wrappers.LeaderWorkerSetWrapper {
				return wrappers.BuildLeaderWorkerSet(ns.Name).SubdomainPolicy(leaderworkerset.SubdomainUniquePerReplica)
			},
			updateLeaderWorkerSet: func(lws *leaderworkerset.LeaderWorkerSet) {
				*lws.Spec.NetworkConfig.SubdomainPolicy = leaderworkerset.SubdomainShared
			},
			updateShouldFail: false,
		}),
		ginkgo.Entry("subdomainPolicy can be updated from Shared to UniquePerReplica", &testValidationCase{
			makeLeaderWorkerSet: func(ns *corev1.Namespace) *wrappers.LeaderWorkerSetWrapper {
				return wrappers.BuildLeaderWorkerSet(ns.Name).SubdomainPolicy(leaderworkerset.SubdomainShared)
			},
			updateLeaderWorkerSet: func(lws *leaderworkerset.LeaderWorkerSet) {
				*lws.Spec.NetworkConfig.SubdomainPolicy = leaderworkerset.SubdomainUniquePerReplica
			},
			updateShouldFail: false,
		}),
		ginkgo.Entry("subdomainPolicy can be updated from nil to UniquePerReplica", &testValidationCase{
			makeLeaderWorkerSet: func(ns *corev1.Namespace) *wrappers.LeaderWorkerSetWrapper {
				lwsWrapper := wrappers.BuildLeaderWorkerSet(ns.Name)
				lwsWrapper.Spec.NetworkConfig = nil
				return lwsWrapper
			},
			updateLeaderWorkerSet: func(lws *leaderworkerset.LeaderWorkerSet) {
				*lws.Spec.NetworkConfig.SubdomainPolicy = leaderworkerset.SubdomainUniquePerReplica
			},
			updateShouldFail: false,
		}),
		ginkgo.Entry("subdomainPolicy can be updated to nil", &testValidationCase{
			makeLeaderWorkerSet: func(ns *corev1.Namespace) *wrappers.LeaderWorkerSetWrapper {
				return wrappers.BuildLeaderWorkerSet(ns.Name)
			},
			updateLeaderWorkerSet: func(lws *leaderworkerset.LeaderWorkerSet) {
				lws.Spec.NetworkConfig.SubdomainPolicy = nil
			},
			updateShouldFail: false,
		}),
		ginkgo.Entry("set restart policy should succeed", &testValidationCase{
			makeLeaderWorkerSet: func(ns *corev1.Namespace) *wrappers.LeaderWorkerSetWrapper {
				return wrappers.BuildLeaderWorkerSet(ns.Name).RestartPolicy(leaderworkerset.NoneRestartPolicy)
			},
			lwsCreationShouldFail: false,
		}),
		ginkgo.Entry("set restart policy should fail when value is invalid", &testValidationCase{
			makeLeaderWorkerSet: func(ns *corev1.Namespace) *wrappers.LeaderWorkerSetWrapper {
				lws := wrappers.BuildLeaderWorkerSet(ns.Name)
				lws.Spec.LeaderWorkerTemplate.RestartPolicy = "invalidValue"
				return lws
			},
			lwsCreationShouldFail: true,
		}),
		ginkgo.Entry("set invalid rolloutStrategyType should be failed", &testValidationCase{
			makeLeaderWorkerSet: func(ns *corev1.Namespace) *wrappers.LeaderWorkerSetWrapper {
				lws := wrappers.BuildLeaderWorkerSet(ns.Name)
				lws.Spec.RolloutStrategy.Type = "invalidValue"
				return lws
			},
			lwsCreationShouldFail: true,
		}),
		ginkgo.Entry("set maxUnavailable greater than replicas is allowed", &testValidationCase{
			makeLeaderWorkerSet: func(ns *corev1.Namespace) *wrappers.LeaderWorkerSetWrapper {
				lws := wrappers.BuildLeaderWorkerSet(ns.Name)
				lws.Spec.RolloutStrategy.RollingUpdateConfiguration.MaxUnavailable = intstr.FromInt32(10)
				return lws
			},
			lwsCreationShouldFail: false,
		}),
		ginkgo.Entry("set maxUnavailable greater than 100% should be failed", &testValidationCase{
			makeLeaderWorkerSet: func(ns *corev1.Namespace) *wrappers.LeaderWorkerSetWrapper {
				lws := wrappers.BuildLeaderWorkerSet(ns.Name)
				lws.Spec.RolloutStrategy.RollingUpdateConfiguration.MaxUnavailable = intstr.FromString("200%")
				return lws
			},
			lwsCreationShouldFail: true,
		}),
		ginkgo.Entry("set maxUnavailable less than 0 should be failed", &testValidationCase{
			makeLeaderWorkerSet: func(ns *corev1.Namespace) *wrappers.LeaderWorkerSetWrapper {
				lws := wrappers.BuildLeaderWorkerSet(ns.Name)
				lws.Spec.RolloutStrategy.RollingUpdateConfiguration.MaxUnavailable = intstr.FromInt32(-1)
				return lws
			},
			lwsCreationShouldFail: true,
		}),
		ginkgo.Entry("set maxSurge greater than replicas is allowed", &testValidationCase{
			makeLeaderWorkerSet: func(ns *corev1.Namespace) *wrappers.LeaderWorkerSetWrapper {
				lws := wrappers.BuildLeaderWorkerSet(ns.Name)
				lws.Spec.RolloutStrategy.RollingUpdateConfiguration.MaxSurge = intstr.FromInt32(10)
				return lws
			},
			lwsCreationShouldFail: false,
		}),
		ginkgo.Entry("set maxSurge greater than 100% should be failed", &testValidationCase{
			makeLeaderWorkerSet: func(ns *corev1.Namespace) *wrappers.LeaderWorkerSetWrapper {
				lws := wrappers.BuildLeaderWorkerSet(ns.Name)
				lws.Spec.RolloutStrategy.RollingUpdateConfiguration.MaxSurge = intstr.FromString("200%")
				return lws
			},
			lwsCreationShouldFail: true,
		}),
		ginkgo.Entry("set maxSurge less than 0 should be failed", &testValidationCase{
			makeLeaderWorkerSet: func(ns *corev1.Namespace) *wrappers.LeaderWorkerSetWrapper {
				lws := wrappers.BuildLeaderWorkerSet(ns.Name)
				lws.Spec.RolloutStrategy.RollingUpdateConfiguration.MaxSurge = intstr.FromInt32(-1)
				return lws
			},
			lwsCreationShouldFail: true,
		}),
		ginkgo.Entry("set maxUnavailable and maxSurge both to 0 should be failed", &testValidationCase{
			makeLeaderWorkerSet: func(ns *corev1.Namespace) *wrappers.LeaderWorkerSetWrapper {
				lws := wrappers.BuildLeaderWorkerSet(ns.Name)
				lws.Spec.RolloutStrategy.RollingUpdateConfiguration.MaxUnavailable = intstr.FromInt32(0)
				return lws
			},
			lwsCreationShouldFail: true,
		}),
		ginkgo.Entry("set replica to 0 no matter maxUnavailable or maxSurge is should be allowed", &testValidationCase{
			makeLeaderWorkerSet: func(ns *corev1.Namespace) *wrappers.LeaderWorkerSetWrapper {
				lws := wrappers.BuildLeaderWorkerSet(ns.Name)
				lws.Spec.Replicas = ptr.To(int32(0))
				lws.Spec.RolloutStrategy.RollingUpdateConfiguration.MaxUnavailable = intstr.FromString("25%")
				lws.Spec.RolloutStrategy.RollingUpdateConfiguration.MaxSurge = intstr.FromString("25%")
				return lws
			},
			lwsCreationShouldFail: false,
		}),
		ginkgo.Entry("set maxSurge to 0 and maxUnavailable to non-zero should be failed", &testValidationCase{
			makeLeaderWorkerSet: func(ns *corev1.Namespace) *wrappers.LeaderWorkerSetWrapper {
				lws := wrappers.BuildLeaderWorkerSet(ns.Name)
				lws.Spec.RolloutStrategy.RollingUpdateConfiguration.MaxUnavailable = intstr.FromString("25%")
				lws.Spec.RolloutStrategy.RollingUpdateConfiguration.MaxSurge = intstr.FromInt32(0)
				return lws
			},
			lwsCreationShouldFail: true,
		}),
		ginkgo.Entry("creation with negative partition should fail", &testValidationCase{
			makeLeaderWorkerSet: func(ns *corev1.Namespace) *wrappers.LeaderWorkerSetWrapper {
				lws := wrappers.BuildLeaderWorkerSet(ns.Name)
				lws.Spec.RolloutStrategy.RollingUpdateConfiguration.Partition = ptr.To[int32](-1)
				return lws
			},
			lwsCreationShouldFail: true,
		}),
		ginkgo.Entry("creation with partition greater than replicas should succeed", &testValidationCase{
			makeLeaderWorkerSet: func(ns *corev1.Namespace) *wrappers.LeaderWorkerSetWrapper {
				lws := wrappers.BuildLeaderWorkerSet(ns.Name).Replica(3)
				lws.Spec.RolloutStrategy.RollingUpdateConfiguration.Partition = ptr.To[int32](5)
				return lws
			},
			lwsCreationShouldFail: false,
		}),
		ginkgo.Entry("creation with partition equal to replicas should succeed", &testValidationCase{
			makeLeaderWorkerSet: func(ns *corev1.Namespace) *wrappers.LeaderWorkerSetWrapper {
				lws := wrappers.BuildLeaderWorkerSet(ns.Name).Replica(3)
				lws.Spec.RolloutStrategy.RollingUpdateConfiguration.Partition = ptr.To[int32](3)
				return lws
			},
			lwsCreationShouldFail: false,
		}),
		ginkgo.Entry("creation with partition zero should succeed", &testValidationCase{
			makeLeaderWorkerSet: func(ns *corev1.Namespace) *wrappers.LeaderWorkerSetWrapper {
				lws := wrappers.BuildLeaderWorkerSet(ns.Name).Replica(3)
				lws.Spec.RolloutStrategy.RollingUpdateConfiguration.Partition = ptr.To[int32](0)
				return lws
			},
			lwsCreationShouldFail: false,
		}),
		ginkgo.Entry("update partition should succeed", &testValidationCase{
			makeLeaderWorkerSet: func(ns *corev1.Namespace) *wrappers.LeaderWorkerSetWrapper {
				return wrappers.BuildLeaderWorkerSet(ns.Name).Replica(5)
			},
			updateLeaderWorkerSet: func(lws *leaderworkerset.LeaderWorkerSet) {
				lws.Spec.RolloutStrategy.RollingUpdateConfiguration.Partition = ptr.To[int32](2)
			},
			updateShouldFail: false,
		}),
		ginkgo.Entry("update partition to negative should fail", &testValidationCase{
			makeLeaderWorkerSet: func(ns *corev1.Namespace) *wrappers.LeaderWorkerSetWrapper {
				return wrappers.BuildLeaderWorkerSet(ns.Name).Replica(5)
			},
			updateLeaderWorkerSet: func(lws *leaderworkerset.LeaderWorkerSet) {
				lws.Spec.RolloutStrategy.RollingUpdateConfiguration.Partition = ptr.To[int32](-1)
			},
			updateShouldFail: true,
		}),
		ginkgo.Entry("update partition to greater than replicas should succeed", &testValidationCase{
			makeLeaderWorkerSet: func(ns *corev1.Namespace) *wrappers.LeaderWorkerSetWrapper {
				return wrappers.BuildLeaderWorkerSet(ns.Name).Replica(5)
			},
			updateLeaderWorkerSet: func(lws *leaderworkerset.LeaderWorkerSet) {
				lws.Spec.RolloutStrategy.RollingUpdateConfiguration.Partition = ptr.To[int32](10)
			},
			updateShouldFail: false,
		}),
		ginkgo.Entry("update partition to equal replicas should succeed", &testValidationCase{
			makeLeaderWorkerSet: func(ns *corev1.Namespace) *wrappers.LeaderWorkerSetWrapper {
				return wrappers.BuildLeaderWorkerSet(ns.Name).Replica(5)
			},
			updateLeaderWorkerSet: func(lws *leaderworkerset.LeaderWorkerSet) {
				lws.Spec.RolloutStrategy.RollingUpdateConfiguration.Partition = ptr.To[int32](5)
			},
			updateShouldFail: false,
		}),
		ginkgo.Entry("update partition to zero should succeed", &testValidationCase{
			makeLeaderWorkerSet: func(ns *corev1.Namespace) *wrappers.LeaderWorkerSetWrapper {
				return wrappers.BuildLeaderWorkerSet(ns.Name).Replica(5)
			},
			updateLeaderWorkerSet: func(lws *leaderworkerset.LeaderWorkerSet) {
				lws.Spec.RolloutStrategy.RollingUpdateConfiguration.Partition = ptr.To[int32](0)
			},
			updateShouldFail: false,
		}),
	)
})

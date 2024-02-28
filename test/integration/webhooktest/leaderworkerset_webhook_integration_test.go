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
package webhook

import (
	"context"

	"github.com/google/go-cmp/cmp"
	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"

	leaderworkerset "sigs.k8s.io/leader-worker-set/api/leaderworkerset/v1"
	testutils "sigs.k8s.io/leader-worker-set/test/testutils"
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
		makeLeaderWorkerSet func(ns *corev1.Namespace) *testutils.LeaderWorkerSetWrapper
		getExpectedLWS      func(lws *leaderworkerset.LeaderWorkerSet) *testutils.LeaderWorkerSetWrapper
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
			makeLeaderWorkerSet: func(ns *corev1.Namespace) *testutils.LeaderWorkerSetWrapper {
				lwsWrapper := testutils.BuildLeaderWorkerSet(ns.Name)
				lwsWrapper.Spec.Replicas = nil
				return lwsWrapper
			},
			getExpectedLWS: func(lws *leaderworkerset.LeaderWorkerSet) *testutils.LeaderWorkerSetWrapper {
				return testutils.BuildLeaderWorkerSet(ns.Name).Replica(1).RestartPolicy(leaderworkerset.Default)
			},
		}),
		ginkgo.Entry("apply defaulting logic for size", &testDefaultingCase{
			makeLeaderWorkerSet: func(ns *corev1.Namespace) *testutils.LeaderWorkerSetWrapper {
				lwsWrapper := testutils.BuildLeaderWorkerSet(ns.Name)
				lwsWrapper.Spec.LeaderWorkerTemplate.Size = nil
				return lwsWrapper
			},
			getExpectedLWS: func(lws *leaderworkerset.LeaderWorkerSet) *testutils.LeaderWorkerSetWrapper {
				return testutils.BuildLeaderWorkerSet(ns.Name).Size(1).RestartPolicy(leaderworkerset.Default)
			},
		}),
		ginkgo.Entry("defaulting logic won't apply when shouldn't", &testDefaultingCase{
			makeLeaderWorkerSet: func(ns *corev1.Namespace) *testutils.LeaderWorkerSetWrapper {
				return testutils.BuildLeaderWorkerSet(ns.Name).Replica(2).Size(2)
			},
			getExpectedLWS: func(lws *leaderworkerset.LeaderWorkerSet) *testutils.LeaderWorkerSetWrapper {
				return testutils.BuildLeaderWorkerSet(ns.Name).Replica(2).Size(2).LeaderTemplateSpec(testutils.MakeLeaderPodSpec()).WorkerTemplateSpec(testutils.MakeWorkerPodSpec()).RestartPolicy(leaderworkerset.Default)
			},
		}),
		ginkgo.Entry("defaulting logic applies when leaderworkertemplate.restartpolicy is not set", &testDefaultingCase{
			makeLeaderWorkerSet: func(ns *corev1.Namespace) *testutils.LeaderWorkerSetWrapper {
				return testutils.BuildLeaderWorkerSet(ns.Name).Replica(2).Size(2)
			},
			getExpectedLWS: func(lws *leaderworkerset.LeaderWorkerSet) *testutils.LeaderWorkerSetWrapper {
				return testutils.BuildLeaderWorkerSet(ns.Name).Replica(2).Size(2).RestartPolicy(leaderworkerset.Default)
			},
		}),
		ginkgo.Entry("defaulting logic won't apply when leaderworkertemplate.restartpolicy is set", &testDefaultingCase{
			makeLeaderWorkerSet: func(ns *corev1.Namespace) *testutils.LeaderWorkerSetWrapper {
				return testutils.BuildLeaderWorkerSet(ns.Name).Replica(2).Size(2).RestartPolicy(leaderworkerset.RecreateGroupOnPodRestart)
			},
			getExpectedLWS: func(lws *leaderworkerset.LeaderWorkerSet) *testutils.LeaderWorkerSetWrapper {
				return testutils.BuildLeaderWorkerSet(ns.Name).Replica(2).Size(2).RestartPolicy(leaderworkerset.RecreateGroupOnPodRestart)
			},
		}),
	)

	type testValidationCase struct {
		makeLeaderWorkerSet   func(ns *corev1.Namespace) *testutils.LeaderWorkerSetWrapper
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
		ginkgo.Entry("creation with invalid replicas and size should fail", &testValidationCase{
			makeLeaderWorkerSet: func(ns *corev1.Namespace) *testutils.LeaderWorkerSetWrapper {
				return testutils.BuildLeaderWorkerSet(ns.Name).Replica(100000).Size(1000000)
			},
			lwsCreationShouldFail: true,
		}),
		ginkgo.Entry("creation with invalid size should fail", &testValidationCase{
			makeLeaderWorkerSet: func(ns *corev1.Namespace) *testutils.LeaderWorkerSetWrapper {
				return testutils.BuildLeaderWorkerSet(ns.Name).Replica(2).Size(-1)
			},
			lwsCreationShouldFail: true,
		}),
		ginkgo.Entry("creation with invalid replicas should fail", &testValidationCase{
			makeLeaderWorkerSet: func(ns *corev1.Namespace) *testutils.LeaderWorkerSetWrapper {
				return testutils.BuildLeaderWorkerSet(ns.Name).Size(2).Replica(-1)
			},
			lwsCreationShouldFail: true,
		}),
		ginkgo.Entry("update with invalid replicas should fail (larger than maxInt32)", &testValidationCase{
			makeLeaderWorkerSet: func(ns *corev1.Namespace) *testutils.LeaderWorkerSetWrapper {
				return testutils.BuildLeaderWorkerSet(ns.Name).Replica(100000).Size(1)
			},
			updateLeaderWorkerSet: func(lws *leaderworkerset.LeaderWorkerSet) {
				lws.Spec.LeaderWorkerTemplate.Size = ptr.To[int32](100000000)
			},
			updateShouldFail: true,
		}),
		ginkgo.Entry("update with invalid replicas should fail (number is negative)", &testValidationCase{
			makeLeaderWorkerSet: func(ns *corev1.Namespace) *testutils.LeaderWorkerSetWrapper {
				return testutils.BuildLeaderWorkerSet(ns.Name).Replica(1).Size(1)
			},
			updateLeaderWorkerSet: func(lws *leaderworkerset.LeaderWorkerSet) {
				lws.Spec.Replicas = ptr.To[int32](-1)
			},
			updateShouldFail: true,
		}),
		ginkgo.Entry("number of worker replicas can't be updated", &testValidationCase{
			makeLeaderWorkerSet: func(ns *corev1.Namespace) *testutils.LeaderWorkerSetWrapper {
				return testutils.BuildLeaderWorkerSet(ns.Name).Replica(1).Size(1)
			},
			updateLeaderWorkerSet: func(lws *leaderworkerset.LeaderWorkerSet) {
				lws.Spec.LeaderWorkerTemplate.Size = ptr.To[int32](2)
			},
			updateShouldFail: true,
		}),
		ginkgo.Entry("number of replicas can be updated", &testValidationCase{
			makeLeaderWorkerSet: func(ns *corev1.Namespace) *testutils.LeaderWorkerSetWrapper {
				return testutils.BuildLeaderWorkerSet(ns.Name).Replica(1).Size(1)
			},
			updateLeaderWorkerSet: func(lws *leaderworkerset.LeaderWorkerSet) {
				lws.Spec.Replicas = ptr.To[int32](3)
			},
			updateShouldFail: false,
		}),
		ginkgo.Entry("not allowlisted fields of spec are immutable", &testValidationCase{
			makeLeaderWorkerSet: func(ns *corev1.Namespace) *testutils.LeaderWorkerSetWrapper {
				return testutils.BuildLeaderWorkerSet(ns.Name)
			},
			updateLeaderWorkerSet: func(lws *leaderworkerset.LeaderWorkerSet) {
				lws.Spec.LeaderWorkerTemplate.WorkerTemplate.Spec = testutils.MakeLeaderPodSpec()
			},
			updateShouldFail: true,
		}),
		ginkgo.Entry("set restart polciy should succeed", &testValidationCase{
			makeLeaderWorkerSet: func(ns *corev1.Namespace) *testutils.LeaderWorkerSetWrapper {
				return testutils.BuildLeaderWorkerSet(ns.Name).RestartPolicy(leaderworkerset.RecreateGroupOnPodRestart)
			},
			lwsCreationShouldFail: false,
		}),
		ginkgo.Entry("set restart polciy should fail when value is invalid", &testValidationCase{
			makeLeaderWorkerSet: func(ns *corev1.Namespace) *testutils.LeaderWorkerSetWrapper {
				lws := testutils.BuildLeaderWorkerSet(ns.Name)
				lws.Spec.LeaderWorkerTemplate.RestartPolicy = "invalidValue"
				return lws
			},
			lwsCreationShouldFail: true,
		}),
	)
})

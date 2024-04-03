/*
Copyright 2024 The Kubernetes Authors.
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

package e2e

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	testing "sigs.k8s.io/lws/test/testutils"
)

var _ = Describe("leaderWorkerSet e2e tests", func() {

	// Each test runs in a separate namespace.
	var ns *corev1.Namespace

	BeforeEach(func() {
		// Create test namespace before each test.
		ns = &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: "test-ns-",
			},
		}
		Expect(k8sClient.Create(ctx, ns)).To(Succeed())

		// Wait for namespace to exist before proceeding with test.
		Eventually(func() bool {
			err := k8sClient.Get(ctx, types.NamespacedName{Namespace: ns.Namespace, Name: ns.Name}, ns)
			return err == nil
		}, timeout, interval).Should(BeTrue())
	})

	AfterEach(func() {
		Expect(testing.DeleteNamespace(ctx, k8sClient, ns)).To(Succeed())
	})

	It("Can deploy lws", func() {
		lws := testing.BuildLeaderWorkerSet(ns.Name).Obj()
		Expect(k8sClient.Create(ctx, lws)).To(Succeed())
		testing.ExpectLeaderWorkerSetAvailable(ctx, k8sClient, lws, "All replicas are ready")
	})

	It("Can rolling update lws", func() {
		lws := testing.BuildLeaderWorkerSet(ns.Name).Obj()
		Expect(k8sClient.Create(ctx, lws)).To(Succeed())

		// Wait for leaderWorkerSet ready then update it.
		testing.ExpectLeaderWorkerSetAvailable(ctx, k8sClient, lws, "All replicas are ready")
		testing.UpdateWorkerTemplate(ctx, k8sClient, lws)

		// Wait for leaderWorkerSet ready again.
		testing.ExpectLeaderWorkerSetAvailable(ctx, k8sClient, lws, "All replicas are ready")
		testing.ExpectValidLeaderStatefulSet(ctx, lws, k8sClient)
		testing.ExpectValidWorkerStatefulSets(ctx, lws, k8sClient, true)
		testing.ExpectValidPods(ctx, k8sClient, lws)
	})
})

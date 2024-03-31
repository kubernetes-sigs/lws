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

	leaderworkerset "sigs.k8s.io/lws/api/leaderworkerset/v1"
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

	It("Can deploy lws", func() {
		leaderWorkerSetSpec := testing.BuildLeaderWorkerSet(ns.Name).Replica(3).Obj()

		Expect(k8sClient.Create(ctx, leaderWorkerSetSpec)).To(Succeed())

		var leaderWorkerSetStruct leaderworkerset.LeaderWorkerSet
		Eventually(func() error {
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: leaderWorkerSetSpec.Name, Namespace: ns.Name}, &leaderWorkerSetStruct); err != nil {
				return err
			}
			return nil
		}, testing.Timeout, testing.Interval).Should(Succeed())
	})
})

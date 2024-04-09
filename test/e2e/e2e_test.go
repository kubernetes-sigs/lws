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
	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"sigs.k8s.io/controller-runtime/pkg/client"
	leaderworkerset "sigs.k8s.io/lws/api/leaderworkerset/v1"
	v1 "sigs.k8s.io/lws/api/leaderworkerset/v1"
	"sigs.k8s.io/lws/test/testutils"
	testing "sigs.k8s.io/lws/test/testutils"
)

var _ = ginkgo.Describe("leaderWorkerSet e2e tests", func() {

	// Each test runs in a separate namespace.
	var ns *corev1.Namespace
	var lws *leaderworkerset.LeaderWorkerSet

	ginkgo.BeforeEach(func() {
		// Create test namespace before each test.
		ns = &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: "test-ns-",
			},
		}
		gomega.Expect(k8sClient.Create(ctx, ns)).Should(gomega.Succeed())

		// Wait for namespace to exist before proceeding with test.
		gomega.Eventually(func() bool {
			err := k8sClient.Get(ctx, types.NamespacedName{Namespace: ns.Namespace, Name: ns.Name}, ns)
			return err == nil
		}, timeout, interval).Should(gomega.BeTrue())
	})

	ginkgo.AfterEach(func() {
		gomega.Expect(testing.DeleteNamespace(ctx, k8sClient, ns)).To(gomega.Succeed())
	})

	ginkgo.It("Can deploy lws with 'replicas', 'size', and 'restart policy' set", func() {
		lws = testing.BuildLeaderWorkerSet(ns.Name).Replica(4).Size(5).RestartPolicy(v1.RecreateGroupOnPodRestart).Obj()
		testing.MustCreateLws(ctx, k8sClient, lws)
		testing.ExpectLeaderWorkerSetAvailable(ctx, k8sClient, lws, "All replicas are ready")
		testing.GetStatefulSets(ctx, lws, k8sClient, &appsv1.StatefulSetList{})

		gomega.Expect(*lws.Spec.Replicas).To(gomega.Equal(int32(4)))
		gomega.Expect(*lws.Spec.LeaderWorkerTemplate.Size).To(gomega.Equal(int32(5)))
		gomega.Expect(lws.Spec.LeaderWorkerTemplate.RestartPolicy).To(gomega.Equal(v1.RecreateGroupOnPodRestart))
	})

	ginkgo.It("Can perform a rolling update", func() {
		lws := testing.BuildLeaderWorkerSet(ns.Name).Obj()
		testing.MustCreateLws(ctx, k8sClient, lws)

		// Wait for leaderWorkerSet to be ready then update it.
		testing.ExpectLeaderWorkerSetAvailable(ctx, k8sClient, lws, "All replicas are ready")
		testing.UpdateWorkerTemplate(ctx, k8sClient, lws)

		testing.ExpectValidLeaderStatefulSet(ctx, lws, k8sClient)
		testing.ExpectValidWorkerStatefulSets(ctx, lws, k8sClient, true)
		testing.ExpectValidPods(ctx, k8sClient, lws, &corev1.PodList{})
		// Wait for leaderWorkerSet to be ready again.
		testing.ExpectLeaderWorkerSetAvailable(ctx, k8sClient, lws, "All replicas are ready")
	})

	ginkgo.It("Adds env vars to containers when using TPU", func() {
		leaderPodSpec := testing.MakeLeaderPodSpecWithTPUResource()
		workerPodSpec := testutils.MakeWorkerPodSpecWithTPUResource()
		lws = testing.BuildLeaderWorkerSet(ns.Name).Replica(2).Size(2).LeaderTemplateSpec(leaderPodSpec).WorkerTemplateSpec(workerPodSpec).Obj()

		testing.MustCreateLws(ctx, k8sClient, lws)
		lwsPods := &corev1.PodList{}
		testing.ExpectValidPods(ctx, k8sClient, lws, lwsPods)

		for _, p := range lwsPods.Items {
			gomega.Expect(testing.HasTPUEnvVarsPopulated(p)).To(gomega.BeTrue())
		}
	})

	ginkgo.It("Doesnt add env vars to containers when not using TPU", func() {
		leaderPodSpec := testutils.MakeLeaderPodSpec()
		workerPodSpec := testutils.MakeWorkerPodSpec()
		lws = testing.BuildLeaderWorkerSet(ns.Name).Replica(2).Size(2).LeaderTemplateSpec(leaderPodSpec).WorkerTemplateSpec(workerPodSpec).Obj()

		testing.MustCreateLws(ctx, k8sClient, lws)
		lwsPods := &corev1.PodList{}
		testing.ExpectValidPods(ctx, k8sClient, lws, lwsPods)

		for _, p := range lwsPods.Items {
			gomega.Expect(testing.HasTPUEnvVarsPopulated(p)).To(gomega.BeFalse())
		}
	})

	ginkgo.It("Pod restart will delete the pod group when restart policy is RecreateGroupOnPodRestart", func() {
		lws = testing.BuildLeaderWorkerSet(ns.Name).Replica(1).Size(3).RestartPolicy(v1.RecreateGroupOnPodRestart).Obj()
		testing.MustCreateLws(ctx, k8sClient, lws)
		testing.ExpectLeaderWorkerSetAvailable(ctx, k8sClient, lws, "All replicas are ready")

		initialPods := &corev1.PodList{}
		testing.ExpectValidPods(ctx, k8sClient, lws, initialPods)

		var leaderPod corev1.Pod
		gomega.Eventually(k8sClient.Get(ctx, types.NamespacedName{Name: lws.Name + "-0", Namespace: lws.Namespace}, &leaderPod)).Should(gomega.Succeed())
		var firstWorker corev1.Pod
		gomega.Eventually(k8sClient.Get(ctx, types.NamespacedName{Name: leaderPod.Name + "-1", Namespace: leaderPod.Namespace}, &firstWorker)).Should(gomega.Succeed())

		// Get all lws pod UIDs now and compare them to the UIDs after deletion
		// With RecreateGroupOnPodRestart they should all be different

		initialPodUIDs := make(map[types.UID]struct{})
		for _, w := range initialPods.Items {
			initialPodUIDs[w.UID] = struct{}{}
		}

		k8sClient.Delete(ctx, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: lws.Namespace, Name: lws.Name + "-0-1"}})

		gomega.Eventually(func() (int, error) {
			finalPods := &corev1.PodList{}
			err := k8sClient.List(ctx, finalPods, client.InNamespace(lws.Namespace))
			if err != nil {
				return -1, err
			}

			// Ensure the initial and final pod lists are of equal length
			gomega.Expect(len(finalPods.Items)).To(gomega.Equal(len(initialPods.Items)))
			numberOfPodsInCommon := 0
			for _, p := range finalPods.Items {
				_, ok := initialPodUIDs[p.UID]
				if ok {
					numberOfPodsInCommon++
				}
			}
			return numberOfPodsInCommon, nil
		}, timeout, interval).Should(gomega.Equal(0))
	})
})

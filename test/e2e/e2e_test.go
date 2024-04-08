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

	"sigs.k8s.io/controller-runtime/pkg/client"
	leaderworkerset "sigs.k8s.io/lws/api/leaderworkerset/v1"
	v1 "sigs.k8s.io/lws/api/leaderworkerset/v1"
	"sigs.k8s.io/lws/test/testutils"
	testing "sigs.k8s.io/lws/test/testutils"
)

var _ = Describe("leaderWorkerSet e2e tests", func() {

	// Each test runs in a separate namespace.
	var ns *corev1.Namespace
	var lws *leaderworkerset.LeaderWorkerSet

	BeforeEach(func() {
		// Create test namespace before each test.
		ns = &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: "test-ns-",
			},
		}
		Eventually(k8sClient.Create(ctx, ns)).Should(Succeed())

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
		lws = testing.BuildLeaderWorkerSet(ns.Name).Obj()
		testing.MustCreateLws(ctx, k8sClient, lws)
		testing.ExpectLeaderWorkerSetAvailable(ctx, k8sClient, lws, "All replicas are ready")
	})

	It("Can deploy lws with 'replicas', 'size', and 'restart policy' set", func() {
		lws = testing.BuildLeaderWorkerSet(ns.Name).Replica(4).Size(5).RestartPolicy(v1.RecreateGroupOnPodRestart).Obj()
		testing.MustCreateLws(ctx, k8sClient, lws)
		testing.ExpectLeaderWorkerSetAvailable(ctx, k8sClient, lws, "All replicas are ready")

		Expect(*lws.Spec.Replicas).To(Equal(int32(4)))
		Expect(*lws.Spec.LeaderWorkerTemplate.Size).To(Equal(int32(5)))
		Expect(lws.Spec.LeaderWorkerTemplate.RestartPolicy).To(Equal(v1.RecreateGroupOnPodRestart))
	})

	It("Can perform a rolling update", func() {
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

	It("Adds env vars to containers when using TPU", func() {
		leaderPodSpec := testing.MakeLeaderPodSpecWithTPUResource()
		workerPodSpec := testutils.MakeWorkerPodSpecWithTPUResource()
		lws = testing.BuildLeaderWorkerSet(ns.Name).Replica(2).Size(2).LeaderTemplateSpec(leaderPodSpec).WorkerTemplateSpec(workerPodSpec).Obj()
		testing.MustCreateLws(ctx, k8sClient, lws)

		lwsPods := &corev1.PodList{}
		testing.ExpectValidPods(ctx, k8sClient, lws, lwsPods)
		//testing.GetPods(ctx, lws, k8sClient, lwsPods)

		Eventually(func() (bool, error) {
			var allPods corev1.PodList
			err := k8sClient.List(ctx, &allPods, client.InNamespace(lws.Namespace))
			if err != nil {
				return false, err
			}
			for _, p := range allPods.Items {
				if !testing.HasTPUEnvVarsPopulated(p) {
					return false, nil
				}
			}
			return true, nil
		}, timeout, interval).Should(BeTrue())
	})

	It("Doesnt add env vars to containers when not using TPU", func() {
		leaderPodSpec := testutils.MakeLeaderPodSpec()
		workerPodSpec := testutils.MakeWorkerPodSpec()
		lws = testing.BuildLeaderWorkerSet(ns.Name).Replica(2).Size(2).LeaderTemplateSpec(leaderPodSpec).WorkerTemplateSpec(workerPodSpec).Obj()

		testing.MustCreateLws(ctx, k8sClient, lws)
		lwsPods := &corev1.PodList{}
		testing.ExpectValidPods(ctx, k8sClient, lws, lwsPods)
		//testing.GetPods(ctx, lws, k8sClient, lwsPods)

		for _, p := range lwsPods.Items {
			Expect(testing.HasTPUEnvVarsPopulated(p)).To(BeFalse())
		}
	})

	It("Pod restart will not recreate the pod group when restart policy is Default", func() {
		lws = testing.BuildLeaderWorkerSet(ns.Name).Replica(1).Size(3).RestartPolicy(v1.DefaultRestartPolicy).Obj()
		testing.MustCreateLws(ctx, k8sClient, lws)
		testing.ExpectLeaderWorkerSetAvailable(ctx, k8sClient, lws, "All replicas are ready")
		testing.ExpectValidPods(ctx, k8sClient, lws, &corev1.PodList{})
		//testing.GetPods(ctx, lws, k8sClient, &corev1.PodList{})

		var leaderPod corev1.Pod
		Eventually(k8sClient.Get(ctx, types.NamespacedName{Name: lws.Name + "-0", Namespace: lws.Namespace}, &leaderPod)).Should(Succeed())
		var firstWorker corev1.Pod
		Eventually(k8sClient.Get(ctx, types.NamespacedName{Name: leaderPod.Name + "-1", Namespace: leaderPod.Namespace}, &firstWorker)).Should(Succeed())
		var workers corev1.PodList
		Eventually(k8sClient.List(ctx, &workers, client.InNamespace(lws.Namespace))).Should(Succeed())

		// Get all lws pod UIDs now and compare them to the UIDs after deletion
		// With DefaultRestartPolicy they should all be the same except for the restarted pod
		initialPodUIDs := make(map[types.UID]struct{})
		for _, w := range workers.Items {
			initialPodUIDs[w.UID] = struct{}{}
		}

		Eventually(k8sClient.Delete(ctx, &firstWorker)).Should(Succeed())

		Consistently(func() (*metav1.Time, error) {
			err := k8sClient.Get(ctx, types.NamespacedName{Name: lws.Name + "-0", Namespace: lws.Namespace}, &leaderPod)
			if err != nil {
				return nil, err
			}
			return leaderPod.DeletionTimestamp, nil
		}).Should(BeNil())

		Eventually(func() (int, error) {
			var leaders corev1.PodList
			err := k8sClient.List(ctx, &leaders, client.InNamespace(lws.Namespace), &client.MatchingLabels{leaderworkerset.WorkerIndexLabelKey: "0"})
			if err != nil {
				return -1, err
			}
			return len(leaders.Items), nil
		}, timeout, interval).Should(Equal(int(*lws.Spec.Replicas)))

		Eventually(func() (int, error) {
			err := k8sClient.List(ctx, &workers, client.InNamespace(lws.Namespace))
			if err != nil {
				return -1, err
			}
			numberOfPodsInCommon := 0
			for _, w := range workers.Items {
				_, ok := initialPodUIDs[w.UID]
				if ok {
					numberOfPodsInCommon++
				}
			}
			return numberOfPodsInCommon, nil
		}, timeout, interval).Should(Equal(int(*lws.Spec.LeaderWorkerTemplate.Size) - 1))
	})

	It("Pod restart will delete the pod group when restart policy is RecreateGroupOnPodRestart", func() {
		lws = testing.BuildLeaderWorkerSet(ns.Name).Replica(1).Size(3).RestartPolicy(v1.RecreateGroupOnPodRestart).Obj()
		testing.MustCreateLws(ctx, k8sClient, lws)
		testing.ExpectLeaderWorkerSetAvailable(ctx, k8sClient, lws, "All replicas are ready")

		//testing.GetPods(ctx, lws, k8sClient, &corev1.PodList{})
		testing.ExpectValidPods(ctx, k8sClient, lws, &corev1.PodList{})

		var leaderPod corev1.Pod
		Eventually(k8sClient.Get(ctx, types.NamespacedName{Name: lws.Name + "-0", Namespace: lws.Namespace}, &leaderPod)).Should(Succeed())
		var firstWorker corev1.Pod
		Eventually(k8sClient.Get(ctx, types.NamespacedName{Name: leaderPod.Name + "-1", Namespace: leaderPod.Namespace}, &firstWorker)).Should(Succeed())
		var workers corev1.PodList
		Eventually(k8sClient.List(ctx, &workers, client.InNamespace(lws.Namespace))).Should(Succeed())

		// Get all lws pod UIDs now and compare them to the UIDs after deletion
		// With RecreateGroupOnPodRestart they should all be different
		initialPodUIDs := make(map[types.UID]struct{})
		for _, w := range workers.Items {
			initialPodUIDs[w.UID] = struct{}{}
		}

		Eventually(k8sClient.Delete(ctx, &firstWorker)).Should(Succeed())

		Eventually(func() (int, error) {
			var leaders corev1.PodList
			err := k8sClient.List(ctx, &leaders, client.InNamespace(lws.Namespace), &client.MatchingLabels{leaderworkerset.WorkerIndexLabelKey: "0"})
			if err != nil {
				return -1, err
			}
			return len(leaders.Items), nil
		}, timeout, interval).Should(Equal(int(*lws.Spec.Replicas)))

		Eventually(func() (int, error) {
			err := k8sClient.List(ctx, &workers, client.InNamespace(lws.Namespace))
			if err != nil {
				return -1, err
			}
			numberOfPodsInCommon := 0
			for _, w := range workers.Items {
				_, ok := initialPodUIDs[w.UID]
				if ok {
					numberOfPodsInCommon++
				}
			}
			return numberOfPodsInCommon, nil
		}, timeout, interval).Should(Equal(0))
	})
})

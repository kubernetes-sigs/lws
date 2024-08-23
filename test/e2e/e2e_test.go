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
		gomega.Expect(k8sClient.Create(ctx, ns)).To(gomega.Succeed())

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

		statefulSets := &appsv1.StatefulSetList{}
		testing.GetStatefulSets(ctx, lws, k8sClient, statefulSets)

		pods := &corev1.PodList{}
		testing.ExpectValidPods(ctx, k8sClient, lws, pods)

		gomega.Expect(*lws.Spec.Replicas).To(gomega.Equal(int32(4)))
		gomega.Expect(*lws.Spec.LeaderWorkerTemplate.Size).To(gomega.Equal(int32(5)))
		gomega.Expect(lws.Spec.LeaderWorkerTemplate.RestartPolicy).To(gomega.Equal(v1.RecreateGroupOnPodRestart))

		expectedLabels := []string{v1.SetNameLabelKey, v1.GroupIndexLabelKey, v1.WorkerIndexLabelKey, v1.TemplateRevisionHashKey}
		expectedAnnotations := []string{v1.LeaderPodNameAnnotationKey, v1.SizeAnnotationKey}

		for _, pod := range pods.Items {
			for _, expectedLabel := range expectedLabels {
				gomega.Expect(pod.Labels[expectedLabel]).To(gomega.Not(gomega.BeNil()))
			}

			for _, expectedAnnotation := range expectedAnnotations {
				gomega.Expect(pod.Labels[expectedAnnotation]).To(gomega.Not(gomega.BeNil()))
			}
		}

		for _, statefulSet := range statefulSets.Items {
			for _, expectedLabel := range expectedLabels {
				gomega.Expect(statefulSet.Labels[expectedLabel]).To(gomega.Not(gomega.BeNil()))
			}
		}
	})

	ginkgo.It("Can create/update a lws with size=1", func() {
		lws = testing.BuildLeaderWorkerSet(ns.Name).Replica(4).MaxSurge(1).Size(1).RestartPolicy(v1.RecreateGroupOnPodRestart).Obj()
		testing.MustCreateLws(ctx, k8sClient, lws)

		testing.ExpectValidLeaderStatefulSet(ctx, k8sClient, lws, 4)
		testing.ExpectValidWorkerStatefulSets(ctx, lws, k8sClient, true)
		testing.ExpectLeaderWorkerSetAvailable(ctx, k8sClient, lws, "All replicas are ready")

		testing.UpdateWorkerTemplate(ctx, k8sClient, lws)
		// Happen during rolling update.
		testing.ExpectValidLeaderStatefulSet(ctx, k8sClient, lws, 5)

		testing.ExpectValidLeaderStatefulSet(ctx, k8sClient, lws, 4)
		testing.ExpectValidWorkerStatefulSets(ctx, lws, k8sClient, true)
		testing.ExpectLeaderWorkerSetAvailable(ctx, k8sClient, lws, "All replicas are ready")
	})

	ginkgo.It("Can perform a rolling update", func() {
		lws := testing.BuildLeaderWorkerSet(ns.Name).Obj()
		testing.MustCreateLws(ctx, k8sClient, lws)

		// Wait for leaderWorkerSet to be ready then update it.
		testing.ExpectLeaderWorkerSetAvailable(ctx, k8sClient, lws, "All replicas are ready")
		testing.UpdateWorkerTemplate(ctx, k8sClient, lws)

		testing.ExpectValidLeaderStatefulSet(ctx, k8sClient, lws, 2)
		testing.ExpectValidWorkerStatefulSets(ctx, lws, k8sClient, true)
		testing.ExpectValidPods(ctx, k8sClient, lws, &corev1.PodList{})
		// Wait for leaderWorkerSet to be ready again.
		testing.ExpectLeaderWorkerSetAvailable(ctx, k8sClient, lws, "All replicas are ready")
	})

	ginkgo.It("Can perform a rolling update with maxSurge set", func() {
		lws := testing.BuildLeaderWorkerSet(ns.Name).Replica(4).MaxSurge(4).Obj()
		testing.MustCreateLws(ctx, k8sClient, lws)

		// Wait for leaderWorkerSet to be ready then update it.
		testing.ExpectLeaderWorkerSetAvailable(ctx, k8sClient, lws, "All replicas are ready")
		testing.UpdateWorkerTemplate(ctx, k8sClient, lws)

		// Happen during rolling update.
		testing.ExpectValidLeaderStatefulSet(ctx, k8sClient, lws, 7)

		// Rolling update completes.
		testing.ExpectValidLeaderStatefulSet(ctx, k8sClient, lws, 4)
		testing.ExpectValidWorkerStatefulSets(ctx, lws, k8sClient, true)
		testing.ExpectValidPods(ctx, k8sClient, lws, &corev1.PodList{})
		// Wait for leaderWorkerSet to be ready again.
		testing.ExpectLeaderWorkerSetAvailable(ctx, k8sClient, lws, "All replicas are ready")
	})

	ginkgo.It("Can deploy lws with subgroupsize set", func() {
		leaderPodSpec := testing.MakeLeaderPodSpecWithTPUResource()
		workerPodSpec := testing.MakeWorkerPodSpecWithTPUResource()
		lws := testing.BuildLeaderWorkerSet(ns.Name).Replica(2).Size(4).SubGroupSize(2).LeaderTemplateSpec(leaderPodSpec).WorkerTemplateSpec(workerPodSpec).Obj()

		testing.MustCreateLws(ctx, k8sClient, lws)

		lwsPods := &corev1.PodList{}
		testing.ExpectValidPods(ctx, k8sClient, lws, lwsPods)

		expectedLabels := []string{v1.SetNameLabelKey, v1.GroupIndexLabelKey, v1.WorkerIndexLabelKey, v1.TemplateRevisionHashKey, v1.SubGroupIndexLabelKey}
		expectedAnnotations := []string{v1.LeaderPodNameAnnotationKey, v1.SizeAnnotationKey, v1.SubGroupSizeAnnotationKey}

		for _, pod := range lwsPods.Items {

			gomega.Expect(testing.HasTPUEnvVarsPopulated(pod)).To(gomega.BeTrue())

			for _, expectedLabel := range expectedLabels {
				gomega.Expect(pod.Labels[expectedLabel]).To(gomega.Not(gomega.BeNil()))
			}

			for _, expectedAnnotation := range expectedAnnotations {
				gomega.Expect(pod.Labels[expectedAnnotation]).To(gomega.Not(gomega.BeNil()))
			}

		}
	})
	ginkgo.It("Can perform a rolling update with subgroupsize and MaxSurge set", func() {
		lws := testing.BuildLeaderWorkerSet(ns.Name).Replica(4).MaxSurge(4).SubGroupSize(1).Obj()
		testing.MustCreateLws(ctx, k8sClient, lws)

		// Wait for leaderWorkerSet to be ready then update it.
		testing.ExpectLeaderWorkerSetAvailable(ctx, k8sClient, lws, "All replicas are ready")
		testing.UpdateWorkerTemplate(ctx, k8sClient, lws)

		// Happen during rolling update.
		testing.ExpectValidLeaderStatefulSet(ctx, k8sClient, lws, 7)

		// Rolling update completes.
		testing.ExpectValidLeaderStatefulSet(ctx, k8sClient, lws, 4)
		testing.ExpectValidWorkerStatefulSets(ctx, lws, k8sClient, true)
		testing.ExpectValidPods(ctx, k8sClient, lws, &corev1.PodList{})
		// Wait for leaderWorkerSet to be ready again.
		testing.ExpectLeaderWorkerSetAvailable(ctx, k8sClient, lws, "All replicas are ready")
	})

	ginkgo.It("Adds env vars to containers when using TPU", func() {
		leaderPodSpec := testing.MakeLeaderPodSpecWithTPUResource()
		workerPodSpec := testing.MakeWorkerPodSpecWithTPUResource()
		lws = testing.BuildLeaderWorkerSet(ns.Name).Replica(2).Size(4).LeaderTemplateSpec(leaderPodSpec).WorkerTemplateSpec(workerPodSpec).Obj()

		testing.MustCreateLws(ctx, k8sClient, lws)
		lwsPods := &corev1.PodList{}
		testing.ExpectValidPods(ctx, k8sClient, lws, lwsPods)

		for _, p := range lwsPods.Items {
			gomega.Expect(testing.HasTPUEnvVarsPopulated(p)).To(gomega.BeTrue())
		}
	})

	ginkgo.It("When changing subdomainPolicy, adds correct env vars", func() {
		leaderPodSpec := testing.MakeLeaderPodSpecWithTPUResource()
		workerPodSpec := testing.MakeWorkerPodSpecWithTPUResource()
		lws := testing.BuildLeaderWorkerSet(ns.Name).Replica(2).Size(4).SubGroupSize(2).LeaderTemplateSpec(leaderPodSpec).WorkerTemplateSpec(workerPodSpec).Obj()
		testing.MustCreateLws(ctx, k8sClient, lws)
		testing.ExpectValidPods(ctx, k8sClient, lws, &corev1.PodList{})
		testing.UpdateSubdomainPolicy(ctx, k8sClient, lws)
		testing.ExpectValidPods(ctx, k8sClient, lws, &corev1.PodList{})
		testing.UpdateReplicaCount(ctx, k8sClient, lws, 2)

		lwsPods := &corev1.PodList{}
		testing.ExpectValidPods(ctx, k8sClient, lws, lwsPods)

		for _, pod := range lwsPods.Items {
			if pod.Annotations[leaderworkerset.GroupIndexLabelKey] == "0" {
				gomega.Expect(testing.CheckTPUContainerHasCorrectEnvVars(pod, "test-sample-0.test-sample,test-sample-0-1.test-sample"))
			} else {
				gomega.Expect(testing.CheckTPUContainerHasCorrectEnvVars(pod, "test-sample-1.test-sample-1,test-sample-1-1.test-sample-1"))
			}
			gomega.Expect(testing.HasTPUEnvVarsPopulated(pod)).To(gomega.BeTrue())
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

		// Get all lws pod UIDs now and compare them to the UIDs after deletion
		// With RecreateGroupOnPodRestart they should all be different
		initialPodUIDs := make(map[types.UID]struct{})
		for _, w := range initialPods.Items {
			initialPodUIDs[w.UID] = struct{}{}
		}

		gomega.Expect(k8sClient.Delete(ctx, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: lws.Namespace, Name: lws.Name + "-0-1"}})).To(gomega.Succeed())

		gomega.Eventually(func() (int, error) {
			finalPods := &corev1.PodList{}
			err := k8sClient.List(ctx, finalPods, client.InNamespace(lws.Namespace))
			if err != nil {
				return -1, err
			}

			// Ensure the initial and final pod lists are of equal length
			if len(finalPods.Items) != len(initialPods.Items) {
				return -1, nil
			}

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

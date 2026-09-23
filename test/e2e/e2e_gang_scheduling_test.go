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

package e2e

import (
	"strconv"
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	schedulingv1alpha3 "k8s.io/api/scheduling/v1alpha3"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	leaderworkerset "sigs.k8s.io/lws/api/leaderworkerset/v1"
	v1 "sigs.k8s.io/lws/api/leaderworkerset/v1"
	"sigs.k8s.io/lws/pkg/schedulerprovider"
	testing "sigs.k8s.io/lws/test/testutils"
	"sigs.k8s.io/lws/test/wrappers"
)

var _ = ginkgo.Describe("leaderWorkerSet e2e gang scheduling tests", func() {
	ginkgo.Context("with volcano gang scheduling enabled", ginkgo.Ordered, func() {
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

		ginkgo.BeforeAll(func() {
			if schedulerProvider != schedulerprovider.Volcano {
				ginkgo.Skip("Volcano gang scheduling tests require SCHEDULER_PROVIDER=volcano")
			}
		})

		ginkgo.It("Should create PodGroups when LWS is created with LeaderCreated startup policy", func() {
			lws = wrappers.BuildLeaderWorkerSet(ns.Name).
				Replica(2).
				Size(4).
				SchedulerName("volcano").
				StartupPolicy(v1.LeaderCreatedStartupPolicy).
				Obj()

			testing.MustCreateLws(ctx, k8sClient, lws)
			testing.ExpectLeaderWorkerSetAvailable(ctx, k8sClient, lws, "All replicas are ready")
			// Verify PodGroups are created with correct spec and owner reference
			testing.ExpectValidPodGroups(ctx, k8sClient, schedulerprovider.Volcano, lws, 2)
			pods := &corev1.PodList{}
			testing.ExpectValidPods(ctx, k8sClient, lws, pods)
		})

		ginkgo.It("Should create PodGroups when LWS is created with LeaderReady startup policy", func() {
			lws = wrappers.BuildLeaderWorkerSet(ns.Name).
				Replica(2).
				Size(4).
				SchedulerName("volcano").
				StartupPolicy(v1.LeaderReadyStartupPolicy).
				Obj()

			testing.MustCreateLws(ctx, k8sClient, lws)
			testing.ExpectLeaderWorkerSetAvailable(ctx, k8sClient, lws, "All replicas are ready")
			// Verify PodGroups are created with correct spec and owner reference
			testing.ExpectValidPodGroups(ctx, k8sClient, schedulerprovider.Volcano, lws, 2)
			pods := &corev1.PodList{}
			testing.ExpectValidPods(ctx, k8sClient, lws, pods)
		})

		ginkgo.It("Should clean up PodGroups when LWS is deleted", func() {
			lws = wrappers.BuildLeaderWorkerSet(ns.Name).
				Replica(1).
				Size(2).
				SchedulerName("volcano").
				Obj()
			testing.MustCreateLws(ctx, k8sClient, lws)
			testing.ExpectLeaderWorkerSetAvailable(ctx, k8sClient, lws, "All replicas are ready")
			// Verify PodGroups are created with correct spec and owner reference
			testing.ExpectValidPodGroups(ctx, k8sClient, schedulerprovider.Volcano, lws, 1)
			// Delete LWS
			testing.DeleteLWSWithForground(ctx, k8sClient, lws)
			// Verify PodGroups are eventually cleaned up
			testing.ExpectValidPodGroups(ctx, k8sClient, schedulerprovider.Volcano, lws, 0)
		})

		ginkgo.It("Should recreate PodGroups with correct size when LWS is resized", func() {
			lws = wrappers.BuildLeaderWorkerSet(ns.Name).
				Replica(2).
				Size(2).
				SchedulerName("volcano").
				Obj()
			testing.MustCreateLws(ctx, k8sClient, lws)
			testing.ExpectLeaderWorkerSetAvailable(ctx, k8sClient, lws, "All replicas are ready")
			// Verify initial PodGroups with size=2
			testing.ExpectValidPodGroups(ctx, k8sClient, schedulerprovider.Volcano, lws, 2)
			// Resize to size=4
			testing.UpdateSize(ctx, k8sClient, lws, int32(4))
			// Rolling update completes
			testing.ExpectValidLeaderStatefulSet(ctx, k8sClient, lws, 2)
			testing.ExpectValidWorkerStatefulSets(ctx, lws, k8sClient, true)
			testing.ExpectLeaderWorkerSetAvailable(ctx, k8sClient, lws, "All replicas are ready")
			// Pods have been recreated with new size
			lwsPods := &corev1.PodList{}
			testing.ExpectValidPods(ctx, k8sClient, lws, lwsPods)
			for _, p := range lwsPods.Items {
				gomega.Expect(testing.CheckAnnotation(p, leaderworkerset.SizeAnnotationKey, strconv.Itoa(4))).To(gomega.Succeed())
			}
			// Verify PodGroups are recreated with new size
			testing.ExpectValidPodGroups(ctx, k8sClient, schedulerprovider.Volcano, lws, 2)
		})

		ginkgo.It("Should recreate PodGroups when LWS performs rolling update", func() {
			lws = wrappers.BuildLeaderWorkerSet(ns.Name).
				Replica(2).
				Size(3).
				SchedulerName("volcano").
				Obj()
			testing.MustCreateLws(ctx, k8sClient, lws)
			testing.ExpectLeaderWorkerSetAvailable(ctx, k8sClient, lws, "All replicas are ready")
			testing.ExpectValidPodGroups(ctx, k8sClient, schedulerprovider.Volcano, lws, 2)
			// Trigger rolling update
			testing.UpdateWorkerTemplate(ctx, k8sClient, lws)
			// Rolling update completes
			testing.ExpectValidLeaderStatefulSet(ctx, k8sClient, lws, 2)
			testing.ExpectValidWorkerStatefulSets(ctx, lws, k8sClient, true)
			testing.ExpectLeaderWorkerSetAvailable(ctx, k8sClient, lws, "All replicas are ready")
			// Verify final state: still 2 PodGroups exist with updated configuration
			// ExpectValidPodGroups automatically uses current revision, ensuring new PodGroups are validated
			testing.ExpectValidPodGroups(ctx, k8sClient, schedulerprovider.Volcano, lws, 2)
		})
	})

	ginkgo.Context("with Kubernetes WAS enabled", ginkgo.Label("WorkloadAwareScheduling"), func() {
		var ns *corev1.Namespace
		var lws *leaderworkerset.LeaderWorkerSet

		ginkgo.BeforeEach(func() {
			if schedulerProvider != schedulerprovider.Kubernetes {
				ginkgo.Skip("WAS tests require SCHEDULER_PROVIDER=kubernetes")
			}
			ns = &corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					GenerateName: "was-e2e-",
				},
			}
			gomega.Expect(k8sClient.Create(ctx, ns)).To(gomega.Succeed())
			lws = wrappers.BuildLeaderWorkerSet(ns.Name).
				Replica(2).
				Size(2).
				Obj()
			lws.Spec.Scheduling = &leaderworkerset.LeaderWorkerSetScheduling{}
		})

		ginkgo.AfterEach(func() {
			gomega.Expect(testing.DeleteNamespace(ctx, k8sClient, ns)).To(gomega.Succeed())
		})

		ginkgo.It("Should schedule each replica as a gang", func() {
			testing.MustCreateLws(ctx, k8sClient, lws)
			testing.ExpectWASPodGroupsScheduled(ctx, k8sClient, lws, 4, 2, 2)
			testing.ExpectLeaderWorkerSetAvailable(ctx, k8sClient, lws, "All replicas are ready")
		})

		ginkgo.It("Should schedule the whole LWS as a gang", func() {
			lws.Spec.Scheduling.SchedulingPolicy = &schedulingv1alpha3.WorkloadCompositePodGroupSchedulingPolicy{
				Gang: &schedulingv1alpha3.WorkloadCompositePodGroupGangSchedulingPolicy{},
			}
			testing.MustCreateLws(ctx, k8sClient, lws)
			testing.ExpectWASPodGroupsScheduled(ctx, k8sClient, lws, 4, 4)
			testing.ExpectLeaderWorkerSetAvailable(ctx, k8sClient, lws, "All replicas are ready")
		})

		ginkgo.It("Should keep the gang pending until enough capacity is available", func() {
			ginkgo.By("advertising two test-specific resource units on one node")
			nodes := &corev1.NodeList{}
			gomega.Expect(k8sClient.List(ctx, nodes)).To(gomega.Succeed())
			var nodeKey client.ObjectKey
			for _, node := range nodes.Items {
				if node.Spec.Unschedulable {
					continue
				}
				tainted := false
				for _, taint := range node.Spec.Taints {
					if taint.Effect == corev1.TaintEffectNoSchedule || taint.Effect == corev1.TaintEffectNoExecute {
						tainted = true
					}
				}
				for _, condition := range node.Status.Conditions {
					if condition.Type == corev1.NodeReady && condition.Status == corev1.ConditionTrue && !tainted {
						nodeKey = client.ObjectKeyFromObject(&node)
						break
					}
				}
				if nodeKey.Name != "" {
					break
				}
			}
			gomega.Expect(nodeKey.Name).NotTo(gomega.BeEmpty(), "capacity tests need a Ready, schedulable, untainted node")
			resourceName := corev1.ResourceName("example.com/" + ns.Name)
			updateCapacity := func(remove bool) {
				gomega.Eventually(func(g gomega.Gomega) {
					node := &corev1.Node{}
					g.Expect(k8sClient.Get(ctx, nodeKey, node)).To(gomega.Succeed())
					original := node.DeepCopy()
					if remove {
						delete(node.Status.Capacity, resourceName)
						delete(node.Status.Allocatable, resourceName)
					} else {
						node.Status.Capacity[resourceName] = resource.MustParse("2")
						node.Status.Allocatable[resourceName] = resource.MustParse("2")
					}
					g.Expect(k8sClient.Status().Patch(ctx, node, client.MergeFrom(original))).To(gomega.Succeed())
				}, timeout, interval).Should(gomega.Succeed())
			}
			updateCapacity(false)
			ginkgo.DeferCleanup(updateCapacity, true)

			resources := corev1.ResourceRequirements{
				Requests: corev1.ResourceList{resourceName: resource.MustParse("1")},
				Limits:   corev1.ResourceList{resourceName: resource.MustParse("1")},
			}
			lws.Spec.Replicas = ptr.To[int32](1)
			lws.Spec.LeaderWorkerTemplate.LeaderTemplate.Spec.Containers[0].Resources = resources
			lws.Spec.LeaderWorkerTemplate.WorkerTemplate.Spec.Containers[0].Resources = resources
			blocker := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "capacity-blocker", Namespace: ns.Name},
				Spec:       *lws.Spec.LeaderWorkerTemplate.LeaderTemplate.Spec.DeepCopy(),
			}
			gomega.Expect(k8sClient.Create(ctx, blocker)).To(gomega.Succeed())
			gomega.Eventually(func(g gomega.Gomega) {
				g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(blocker), blocker)).To(gomega.Succeed())
				g.Expect(blocker.Status.Phase).To(gomega.Equal(corev1.PodRunning))
			}, timeout, interval).Should(gomega.Succeed())

			ginkgo.By("checking that neither gang member binds while only one resource unit is free")
			testing.MustCreateLws(ctx, k8sClient, lws)
			assertPending := func(g gomega.Gomega) {
				pods := &corev1.PodList{}
				g.Expect(k8sClient.List(ctx, pods, client.InNamespace(ns.Name), client.MatchingLabels{leaderworkerset.SetNameLabelKey: lws.Name})).To(gomega.Succeed())
				g.Expect(pods.Items).To(gomega.HaveLen(2))
				for _, pod := range pods.Items {
					g.Expect(pod.Spec.SchedulingGroup).NotTo(gomega.BeNil())
					g.Expect(pod.Spec.NodeName).To(gomega.BeEmpty())
					g.Expect(pod.Status.Phase).To(gomega.Equal(corev1.PodPending))
				}
			}
			gomega.Eventually(assertPending, timeout, interval).Should(gomega.Succeed())
			gomega.Consistently(assertPending, 10*time.Second, interval).Should(gomega.Succeed())

			ginkgo.By("releasing capacity and waiting for the entire gang to become Ready")
			gomega.Expect(k8sClient.Delete(ctx, blocker)).To(gomega.Succeed())
			testing.ExpectWASPodGroupsScheduled(ctx, k8sClient, lws, 2, 2)
		})
	})
})

/*
Copyright 2026 The Kubernetes Authors.

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

package controllers

import (
	"context"
	"fmt"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	schedulingv1alpha3 "k8s.io/api/scheduling/v1alpha3"
	schedulingv1beta1 "k8s.io/api/scheduling/v1beta1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	leaderworkerset "sigs.k8s.io/lws/api/leaderworkerset/v1"
	"sigs.k8s.io/lws/pkg/schedulerprovider"
	testing "sigs.k8s.io/lws/test/testutils"
	"sigs.k8s.io/lws/test/wrappers"
)

var _ = ginkgo.Describe("Workload-aware scheduling controller", func() {
	var ns *corev1.Namespace

	ginkgo.BeforeEach(func() {
		ns = &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "lws-scheduling-ns-"}}
		gomega.Expect(k8sClient.Create(ctx, ns)).To(gomega.Succeed())
	})

	ginkgo.AfterEach(func() {
		gomega.Expect(testing.DeleteNamespace(ctx, k8sClient, ns)).To(gomega.Succeed())
	})

	ginkgo.It("creates replica Workload before releasing the leader StatefulSet and materializes PodGroups per leader", func() {
		lws := wrappers.BuildLeaderWorkerSet(ns.Name).
			Name("was-replica").
			Replica(2).
			Size(3).
			Obj()
		lws.Spec.Scheduling = &leaderworkerset.LeaderWorkerSetScheduling{}
		gomega.Expect(k8sClient.Create(ctx, lws)).To(gomega.Succeed())

		var workload *schedulingv1beta1.Workload
		leaderStatefulSet := &appsv1.StatefulSet{}
		gomega.Eventually(func(g gomega.Gomega) {
			workload = &schedulingv1beta1.Workload{}
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns.Name, Name: schedulerprovider.KubernetesWorkloadName(lws)}, workload)).To(gomega.Succeed())
			g.Expect(workload.Spec.PodGroupTemplates).To(gomega.HaveLen(1))
			g.Expect(workload.Spec.PodGroupTemplates[0].Name).To(gomega.Equal("replica"))
			g.Expect(workload.Spec.PodGroupTemplates[0].SchedulingPolicy.Gang).NotTo(gomega.BeNil())
			g.Expect(workload.Spec.PodGroupTemplates[0].SchedulingPolicy.Gang.MinCount).To(gomega.Equal(int32(3)))

			g.Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns.Name, Name: lws.Name}, leaderStatefulSet)).To(gomega.Succeed())
			g.Expect(leaderStatefulSet.Spec.Template.Annotations[schedulerprovider.WorkloadSchedulingAnnotationKey]).To(gomega.Equal(string(schedulerprovider.SchedulingModeReplica)))
			g.Expect(leaderStatefulSet.Spec.Template.Annotations[schedulerprovider.WorkloadNameAnnotationKey]).To(gomega.Equal(schedulerprovider.KubernetesWorkloadName(lws)))

			persistedLWS := &leaderworkerset.LeaderWorkerSet{}
			g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(lws), persistedLWS)).To(gomega.Succeed())
			g.Expect(apimeta.IsStatusConditionTrue(persistedLWS.Status.Conditions, string(leaderworkerset.LeaderWorkerSetWorkloadSchedulingCreated))).To(gomega.BeTrue())
		}, testing.Timeout, testing.Interval).Should(gomega.Succeed())

		gomega.Expect(testing.CreateLeaderPodsWithInjectFn(ctx, *leaderStatefulSet, k8sClient, lws, 0, 2, func(pod *corev1.Pod) {
			pod.Spec.SchedulingGates = []corev1.PodSchedulingGate{{Name: leaderworkerset.GroupReplacementSchedulingGate}}
		})).To(gomega.Succeed())

		gomega.Eventually(func(g gomega.Gomega) {
			groups := &schedulingv1beta1.PodGroupList{}
			g.Expect(k8sClient.List(ctx, groups, client.InNamespace(ns.Name), client.MatchingLabels{
				leaderworkerset.SetNameLabelKey: lws.Name,
			})).To(gomega.Succeed())
			g.Expect(groups.Items).To(gomega.HaveLen(2))
			for i := range groups.Items {
				g.Expect(groups.Items[i].Spec.WorkloadRef).NotTo(gomega.BeNil())
				g.Expect(groups.Items[i].Spec.WorkloadRef.WorkloadName).To(gomega.Equal(schedulerprovider.KubernetesWorkloadName(lws)))
				g.Expect(groups.Items[i].Spec.WorkloadRef.TemplateName).To(gomega.Equal("replica"))
				controller := metav1.GetControllerOf(&groups.Items[i])
				g.Expect(controller).NotTo(gomega.BeNil())
				g.Expect(controller.Kind).To(gomega.Equal("LeaderWorkerSet"))
				g.Expect(controller.Name).To(gomega.Equal(lws.Name))
				g.Expect(controller.Controller).To(gomega.Equal(ptr.To(true)))
				var workloadOwner *metav1.OwnerReference
				for j := range groups.Items[i].OwnerReferences {
					ref := &groups.Items[i].OwnerReferences[j]
					if ref.Kind == "Workload" {
						workloadOwner = ref
						break
					}
				}
				g.Expect(workloadOwner).NotTo(gomega.BeNil())
				g.Expect(workloadOwner.Name).To(gomega.Equal(workload.Name))
				g.Expect(workloadOwner.UID).To(gomega.Equal(workload.UID))
				g.Expect(workloadOwner.Controller).To(gomega.Equal(ptr.To(false)))
			}
		}, testing.Timeout, testing.Interval).Should(gomega.Succeed())
	})

	ginkgo.It("uses the UID-indexed parent Workload for delegated scheduling", func() {
		controller := true
		parent := &appsv1.ControllerRevision{
			ObjectMeta: metav1.ObjectMeta{Name: "delegated-parent", Namespace: ns.Name},
			Data:       runtime.RawExtension{Raw: []byte("{}")},
			Revision:   1,
		}
		gomega.Expect(k8sClient.Create(ctx, parent)).To(gomega.Succeed())
		parentOwner := metav1.OwnerReference{
			APIVersion: appsv1.SchemeGroupVersion.String(),
			Kind:       "ControllerRevision",
			Name:       parent.Name,
			UID:        parent.UID,
			Controller: &controller,
		}
		workload := &schedulingv1beta1.Workload{
			ObjectMeta: metav1.ObjectMeta{
				Name:            "delegated-parent-workload",
				Namespace:       ns.Name,
				OwnerReferences: []metav1.OwnerReference{parentOwner},
			},
			Spec: schedulingv1beta1.WorkloadSpec{
				ControllerRef: &schedulingv1beta1.TypedLocalObjectReference{
					APIGroup: appsv1.GroupName, Kind: parentOwner.Kind, Name: parentOwner.Name,
				},
				PodGroupTemplates: []schedulingv1beta1.PodGroupTemplate{{
					Name: "child-template",
					SchedulingPolicy: schedulingv1beta1.PodGroupSchedulingPolicy{
						Gang: &schedulingv1beta1.GangSchedulingPolicy{MinCount: 3},
					},
				}},
			},
		}
		gomega.Expect(k8sClient.Create(ctx, workload)).To(gomega.Succeed())

		lws := wrappers.BuildLeaderWorkerSet(ns.Name).
			Name("delegated-child").
			Replica(1).
			Size(3).
			Obj()
		lws.OwnerReferences = []metav1.OwnerReference{parentOwner}
		lws.Annotations = map[string]string{schedulerprovider.GroupTemplateNameAnnotation: "child-template"}
		lws.Spec.Scheduling = &leaderworkerset.LeaderWorkerSetScheduling{}
		gomega.Expect(k8sClient.Create(ctx, lws)).To(gomega.Succeed())

		leaderStatefulSet := &appsv1.StatefulSet{}
		gomega.Eventually(func(g gomega.Gomega) {
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns.Name, Name: lws.Name}, leaderStatefulSet)).To(gomega.Succeed())
		}, testing.Timeout, testing.Interval).Should(gomega.Succeed())

		gomega.Expect(testing.CreateLeaderPods(ctx, *leaderStatefulSet, k8sClient, lws, 0, 1)).To(gomega.Succeed())

		gomega.Eventually(func(g gomega.Gomega) {
			groups := &schedulingv1beta1.PodGroupList{}
			g.Expect(k8sClient.List(ctx, groups, client.InNamespace(ns.Name), client.MatchingLabels{
				leaderworkerset.SetNameLabelKey: lws.Name,
			})).To(gomega.Succeed())
			g.Expect(groups.Items).To(gomega.HaveLen(1))
			g.Expect(groups.Items[0].Spec.WorkloadRef).NotTo(gomega.BeNil())
			g.Expect(groups.Items[0].Spec.WorkloadRef.WorkloadName).To(gomega.Equal(workload.Name))
			g.Expect(groups.Items[0].Spec.WorkloadRef.TemplateName).To(gomega.Equal("child-template"))
			controller := metav1.GetControllerOf(&groups.Items[0])
			g.Expect(controller).NotTo(gomega.BeNil())
			g.Expect(controller.Kind).To(gomega.Equal("LeaderWorkerSet"))
			for _, ref := range groups.Items[0].OwnerReferences {
				g.Expect(ref.Kind).NotTo(gomega.Equal("Workload"))
			}
		}, testing.Timeout, testing.Interval).Should(gomega.Succeed())
	})

	ginkgo.It("updates the stable whole-LWS gang minimum when replicas scale", func() {
		lws := wrappers.BuildLeaderWorkerSet(ns.Name).
			Name("was-whole-lws").
			Replica(2).
			Size(3).
			Obj()
		lws.Spec.Scheduling = &leaderworkerset.LeaderWorkerSetScheduling{
			SchedulingPolicy: &schedulingv1alpha3.WorkloadCompositePodGroupSchedulingPolicy{
				Gang: &schedulingv1alpha3.WorkloadCompositePodGroupGangSchedulingPolicy{},
			},
		}
		gomega.Expect(k8sClient.Create(ctx, lws)).To(gomega.Succeed())

		assertGangMinimum := func(g gomega.Gomega, want int32) {
			workload := &schedulingv1beta1.Workload{}
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns.Name, Name: schedulerprovider.KubernetesWorkloadName(lws)}, workload)).To(gomega.Succeed())
			g.Expect(workload.Spec.PodGroupTemplates).To(gomega.HaveLen(1))
			g.Expect(workload.Spec.PodGroupTemplates[0].SchedulingPolicy.Gang).NotTo(gomega.BeNil())
			g.Expect(workload.Spec.PodGroupTemplates[0].SchedulingPolicy.Gang.MinCount).To(gomega.Equal(want))

			group := &schedulingv1beta1.PodGroup{}
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns.Name, Name: schedulerprovider.KubernetesLWSGroupName(lws)}, group)).To(gomega.Succeed())
			g.Expect(group.Spec.SchedulingPolicy.Gang).NotTo(gomega.BeNil())
			g.Expect(group.Spec.SchedulingPolicy.Gang.MinCount).To(gomega.Equal(want))
		}
		gomega.Eventually(func(g gomega.Gomega) {
			assertGangMinimum(g, 6)
		}, testing.Timeout, testing.Interval).Should(gomega.Succeed())

		// Scaling to zero keeps the Workload template valid with minCount=1,
		// while removing the runtime whole-LWS PodGroup.
		gomega.Eventually(func() error {
			persisted := &leaderworkerset.LeaderWorkerSet{}
			if err := k8sClient.Get(context.Background(), client.ObjectKeyFromObject(lws), persisted); err != nil {
				return err
			}
			persisted.Spec.Replicas = ptr.To[int32](0)
			return k8sClient.Update(context.Background(), persisted)
		}, testing.Timeout, testing.Interval).Should(gomega.Succeed())
		deletingGroup := &schedulingv1beta1.PodGroup{}
		gomega.Eventually(func(g gomega.Gomega) {
			workload := &schedulingv1beta1.Workload{}
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns.Name, Name: schedulerprovider.KubernetesWorkloadName(lws)}, workload)).To(gomega.Succeed())
			g.Expect(workload.Spec.PodGroupTemplates[0].SchedulingPolicy.Gang.MinCount).To(gomega.Equal(int32(1)))
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns.Name, Name: schedulerprovider.KubernetesLWSGroupName(lws)}, deletingGroup)).To(gomega.Succeed())
			g.Expect(deletingGroup.DeletionTimestamp).NotTo(gomega.BeNil())
		}, testing.Timeout, testing.Interval).Should(gomega.Succeed())

		// envtest does not run the upstream PodGroup protection controller, so
		// emulate its finalizer removal after LWS has requested deletion.
		deletingGroup.Finalizers = nil
		gomega.Expect(k8sClient.Update(ctx, deletingGroup)).To(gomega.Succeed())
		gomega.Eventually(func() bool {
			err := k8sClient.Get(ctx, types.NamespacedName{Namespace: ns.Name, Name: schedulerprovider.KubernetesLWSGroupName(lws)}, &schedulingv1beta1.PodGroup{})
			return apierrors.IsNotFound(err)
		}, testing.Timeout, testing.Interval).Should(gomega.BeTrue())

		gomega.Eventually(func() error {
			persisted := &leaderworkerset.LeaderWorkerSet{}
			if err := k8sClient.Get(context.Background(), client.ObjectKeyFromObject(lws), persisted); err != nil {
				return err
			}
			persisted.Spec.Replicas = ptr.To[int32](3)
			return k8sClient.Update(context.Background(), persisted)
		}, testing.Timeout, testing.Interval).Should(gomega.Succeed())

		gomega.Eventually(func(g gomega.Gomega) {
			assertGangMinimum(g, 9)
			groups := &schedulingv1beta1.PodGroupList{}
			g.Expect(k8sClient.List(ctx, groups, client.InNamespace(ns.Name), client.MatchingLabels{
				leaderworkerset.SetNameLabelKey: lws.Name,
			})).To(gomega.Succeed())
			g.Expect(groups.Items).To(gomega.HaveLen(1))
		}, testing.Timeout, testing.Interval).Should(gomega.Succeed())
	})

	ginkgo.It("compiles the Workload for groupIdentity Hash and defers replica PodGroups to leaders", func() {
		lws := wrappers.BuildLeaderWorkerSet(ns.Name).
			Name("was-hash").
			Replica(2).
			Size(3).
			Obj()
		lws.Spec.GroupIdentity = leaderworkerset.GroupIdentityHash
		lws.Spec.Scheduling = &leaderworkerset.LeaderWorkerSetScheduling{}
		gomega.Expect(k8sClient.Create(ctx, lws)).To(gomega.Succeed())

		gomega.Eventually(func(g gomega.Gomega) {
			workload := &schedulingv1beta1.Workload{}
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns.Name, Name: schedulerprovider.KubernetesWorkloadName(lws)}, workload)).To(gomega.Succeed())
			g.Expect(workload.Spec.PodGroupTemplates).To(gomega.HaveLen(1))
			g.Expect(workload.Spec.PodGroupTemplates[0].Name).To(gomega.Equal("replica"))
			g.Expect(workload.Spec.PodGroupTemplates[0].SchedulingPolicy.Gang).NotTo(gomega.BeNil())
			g.Expect(workload.Spec.PodGroupTemplates[0].SchedulingPolicy.Gang.MinCount).To(gomega.Equal(int32(3)))

			// Group keys only exist once admission stamps a leader pod, so the
			// LWS controller must not guess ordinal replica instances here.
			groups := &schedulingv1beta1.PodGroupList{}
			g.Expect(k8sClient.List(ctx, groups, client.InNamespace(ns.Name), client.MatchingLabels{
				leaderworkerset.SetNameLabelKey: lws.Name,
			})).To(gomega.Succeed())
			g.Expect(groups.Items).To(gomega.BeEmpty())

			leaderDeployment := &appsv1.Deployment{}
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns.Name, Name: lws.Name}, leaderDeployment)).To(gomega.Succeed())
			g.Expect(leaderDeployment.Spec.Template.Annotations[schedulerprovider.WorkloadSchedulingAnnotationKey]).To(gomega.Equal(string(schedulerprovider.SchedulingModeReplica)))
			g.Expect(leaderDeployment.Spec.Template.Annotations[schedulerprovider.WorkloadNameAnnotationKey]).To(gomega.Equal(schedulerprovider.KubernetesWorkloadName(lws)))

			persistedLWS := &leaderworkerset.LeaderWorkerSet{}
			g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(lws), persistedLWS)).To(gomega.Succeed())
			g.Expect(apimeta.IsStatusConditionTrue(persistedLWS.Status.Conditions, string(leaderworkerset.LeaderWorkerSetWorkloadSchedulingCreated))).To(gomega.BeTrue())
		}, testing.Timeout, testing.Interval).Should(gomega.Succeed())
	})

	ginkgo.It("keeps the gang minimum of a replica on the previous revision when the size changes", func() {
		lws := wrappers.BuildLeaderWorkerSet(ns.Name).
			Name("was-size-change").
			Replica(1).
			Size(3).
			Obj()
		lws.Spec.Scheduling = &leaderworkerset.LeaderWorkerSetScheduling{}
		gomega.Expect(k8sClient.Create(ctx, lws)).To(gomega.Succeed())

		leaderStatefulSet := &appsv1.StatefulSet{}
		gomega.Eventually(func(g gomega.Gomega) {
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns.Name, Name: lws.Name}, leaderStatefulSet)).To(gomega.Succeed())
		}, testing.Timeout, testing.Interval).Should(gomega.Succeed())
		gomega.Expect(testing.CreateLeaderPods(ctx, *leaderStatefulSet, k8sClient, lws, 0, 1)).To(gomega.Succeed())
		leaderPod := &corev1.Pod{}
		gomega.Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns.Name, Name: lws.Name + "-0"}, leaderPod)).To(gomega.Succeed())

		podGroupKey := types.NamespacedName{Namespace: ns.Name, Name: schedulerprovider.KubernetesPodGroupName(lws, "0", leaderPod.Labels[leaderworkerset.RevisionKey])}
		workerStatefulSetKey := types.NamespacedName{Namespace: ns.Name, Name: leaderPod.Name}
		assertOldGroup := func(g gomega.Gomega) {
			group := &schedulingv1beta1.PodGroup{}
			g.Expect(k8sClient.Get(ctx, podGroupKey, group)).To(gomega.Succeed())
			g.Expect(group.Spec.SchedulingPolicy.Gang).NotTo(gomega.BeNil())
			g.Expect(group.Spec.SchedulingPolicy.Gang.MinCount).To(gomega.Equal(int32(3)))
			workers := &appsv1.StatefulSet{}
			g.Expect(k8sClient.Get(ctx, workerStatefulSetKey, workers)).To(gomega.Succeed())
			g.Expect(workers.Spec.Replicas).To(gomega.Equal(ptr.To[int32](2)))
		}
		gomega.Eventually(assertOldGroup, testing.Timeout, testing.Interval).Should(gomega.Succeed())

		// The size change starts a rollout and moves the Workload template to
		// the new size, while the old leader keeps running.
		gomega.Eventually(func() error {
			persisted := &leaderworkerset.LeaderWorkerSet{}
			if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(lws), persisted); err != nil {
				return err
			}
			persisted.Spec.LeaderWorkerTemplate.Size = ptr.To[int32](4)
			return k8sClient.Update(ctx, persisted)
		}, testing.Timeout, testing.Interval).Should(gomega.Succeed())
		gomega.Eventually(func(g gomega.Gomega) {
			workload := &schedulingv1beta1.Workload{}
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns.Name, Name: schedulerprovider.KubernetesWorkloadName(lws)}, workload)).To(gomega.Succeed())
			g.Expect(workload.Spec.PodGroupTemplates).To(gomega.HaveLen(1))
			g.Expect(workload.Spec.PodGroupTemplates[0].SchedulingPolicy.Gang.MinCount).To(gomega.Equal(int32(4)))
		}, testing.Timeout, testing.Interval).Should(gomega.Succeed())

		// The old leader is still reconciled: its worker StatefulSet is
		// recreated with the old size.
		workers := &appsv1.StatefulSet{}
		gomega.Expect(k8sClient.Get(ctx, workerStatefulSetKey, workers)).To(gomega.Succeed())
		gomega.Expect(k8sClient.Delete(ctx, workers)).To(gomega.Succeed())
		gomega.Eventually(func(g gomega.Gomega) {
			recreated := &appsv1.StatefulSet{}
			g.Expect(k8sClient.Get(ctx, workerStatefulSetKey, recreated)).To(gomega.Succeed())
			g.Expect(recreated.UID).NotTo(gomega.Equal(workers.UID))
			assertOldGroup(g)
		}, testing.Timeout, testing.Interval).Should(gomega.Succeed())

		// A recreated PodGroup of the old revision keeps the old minimum.
		group := &schedulingv1beta1.PodGroup{}
		gomega.Expect(k8sClient.Get(ctx, podGroupKey, group)).To(gomega.Succeed())
		gomega.Expect(k8sClient.Delete(ctx, group)).To(gomega.Succeed())
		// envtest does not run the upstream PodGroup protection controller, so
		// emulate its finalizer removal.
		gomega.Eventually(func() error {
			deleting := &schedulingv1beta1.PodGroup{}
			if err := k8sClient.Get(ctx, podGroupKey, deleting); err != nil || deleting.UID != group.UID {
				return client.IgnoreNotFound(err)
			}
			deleting.Finalizers = nil
			return k8sClient.Update(ctx, deleting)
		}, testing.Timeout, testing.Interval).Should(gomega.Succeed())
		// Trigger a reconcile of the old leader.
		gomega.Eventually(func() error {
			if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(leaderPod), leaderPod); err != nil {
				return err
			}
			if leaderPod.Annotations == nil {
				leaderPod.Annotations = map[string]string{}
			}
			leaderPod.Annotations["test.leaderworkerset.sigs.k8s.io/touch"] = "1"
			return k8sClient.Update(ctx, leaderPod)
		}, testing.Timeout, testing.Interval).Should(gomega.Succeed())
		gomega.Eventually(func(g gomega.Gomega) {
			recreated := &schedulingv1beta1.PodGroup{}
			g.Expect(k8sClient.Get(ctx, podGroupKey, recreated)).To(gomega.Succeed())
			g.Expect(recreated.UID).NotTo(gomega.Equal(group.UID))
			assertOldGroup(g)
		}, testing.Timeout, testing.Interval).Should(gomega.Succeed())
	})

	ginkgo.It("keeps the whole-LWS gang minimum reachable while a rollout increases the size", func() {
		lws := wrappers.BuildLeaderWorkerSet(ns.Name).
			Name("was-whole-lws-size").
			Replica(2).
			Size(2).
			Obj()
		lws.Spec.Scheduling = &leaderworkerset.LeaderWorkerSetScheduling{
			SchedulingPolicy: &schedulingv1alpha3.WorkloadCompositePodGroupSchedulingPolicy{
				Gang: &schedulingv1alpha3.WorkloadCompositePodGroupGangSchedulingPolicy{},
			},
		}
		gomega.Expect(k8sClient.Create(ctx, lws)).To(gomega.Succeed())

		podGroupKey := types.NamespacedName{Namespace: ns.Name, Name: schedulerprovider.KubernetesLWSGroupName(lws)}
		// expectGangMinimums waits for the gang minimum of the Workload template
		// and of the whole-LWS PodGroup.
		expectGangMinimums := func(templateMinCount, podGroupMinCount int32) {
			gomega.Eventually(func(g gomega.Gomega) {
				workload := &schedulingv1beta1.Workload{}
				g.Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns.Name, Name: schedulerprovider.KubernetesWorkloadName(lws)}, workload)).To(gomega.Succeed())
				g.Expect(workload.Spec.PodGroupTemplates).To(gomega.HaveLen(1))
				g.Expect(workload.Spec.PodGroupTemplates[0].SchedulingPolicy.Gang.MinCount).To(gomega.Equal(templateMinCount))
				group := &schedulingv1beta1.PodGroup{}
				g.Expect(k8sClient.Get(ctx, podGroupKey, group)).To(gomega.Succeed())
				g.Expect(group.Spec.SchedulingPolicy.Gang).NotTo(gomega.BeNil())
				g.Expect(group.Spec.SchedulingPolicy.Gang.MinCount).To(gomega.Equal(podGroupMinCount))
			}, testing.Timeout, testing.Interval).Should(gomega.Succeed())
		}

		leaderStatefulSet := &appsv1.StatefulSet{}
		gomega.Eventually(func(g gomega.Gomega) {
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns.Name, Name: lws.Name}, leaderStatefulSet)).To(gomega.Succeed())
		}, testing.Timeout, testing.Interval).Should(gomega.Succeed())
		// The pod webhook, which this suite does not run, adds every pod to the
		// whole-LWS PodGroup.
		joinPodGroup := func(pod *corev1.Pod) {
			pod.Spec.SchedulingGroup = &corev1.PodSchedulingGroup{PodGroupName: ptr.To(podGroupKey.Name)}
		}
		gomega.Expect(testing.CreateLeaderPodsWithInjectFn(ctx, *leaderStatefulSet, k8sClient, lws, 0, 2, joinPodGroup)).To(gomega.Succeed())
		expectGangMinimums(4, 4)

		// The size increase starts a rollout and moves the template minimum to
		// 6, which the 4 pods of the old groups can never meet.
		testing.UpdateSize(ctx, k8sClient, lws, 3)
		expectGangMinimums(6, 4)

		// Earlier versions raised the PodGroup minimum with the template, which
		// stalled the rollout. It is lowered again.
		gomega.Eventually(func() error {
			group := &schedulingv1beta1.PodGroup{}
			if err := k8sClient.Get(ctx, podGroupKey, group); err != nil {
				return err
			}
			group.Spec.SchedulingPolicy.Gang.MinCount = 6
			return k8sClient.Update(ctx, group)
		}, testing.Timeout, testing.Interval).Should(gomega.Succeed())
		expectGangMinimums(6, 4)

		// The rollout replaces the old groups with groups of the new size one
		// at a time, and the minimum follows their pods.
		resized := &leaderworkerset.LeaderWorkerSet{}
		gomega.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(lws), resized)).To(gomega.Succeed())
		replaceGroup := func(index int32) {
			// The pod controller creates the worker StatefulSet of the leader.
			gomega.Eventually(func() error {
				return k8sClient.Get(ctx, types.NamespacedName{Namespace: ns.Name, Name: fmt.Sprintf("%s-%d", lws.Name, index)}, &appsv1.StatefulSet{})
			}, testing.Timeout, testing.Interval).Should(gomega.Succeed())
			testing.DeleteLeaderPod(ctx, k8sClient, lws, index, index+1)
			gomega.Expect(testing.CreateLeaderPodsWithInjectFn(ctx, *leaderStatefulSet, k8sClient, resized, int(index), int(index)+1, joinPodGroup)).To(gomega.Succeed())
		}
		replaceGroup(1)
		expectGangMinimums(6, 5)
		replaceGroup(0)
		expectGangMinimums(6, 6)
	})

	ginkgo.It("cleans up replica PodGroup when the last member worker pod is deleted after scale-down", func() {
		lws := wrappers.BuildLeaderWorkerSet(ns.Name).
			Name("was-scale-cleanup").
			Replica(1).
			Size(2).
			Obj()
		lws.Spec.Scheduling = &leaderworkerset.LeaderWorkerSetScheduling{}
		gomega.Expect(k8sClient.Create(ctx, lws)).To(gomega.Succeed())

		leaderStatefulSet := &appsv1.StatefulSet{}
		gomega.Eventually(func(g gomega.Gomega) {
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns.Name, Name: lws.Name}, leaderStatefulSet)).To(gomega.Succeed())
			g.Expect(leaderStatefulSet.Labels[leaderworkerset.RevisionKey]).NotTo(gomega.BeEmpty())
		}, testing.Timeout, testing.Interval).Should(gomega.Succeed())

		revision := leaderStatefulSet.Labels[leaderworkerset.RevisionKey]
		podGroupName := schedulerprovider.KubernetesPodGroupName(lws, "0", revision)

		leaderPod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      lws.Name + "-0",
				Namespace: ns.Name,
				Labels: map[string]string{
					leaderworkerset.SetNameLabelKey:     lws.Name,
					leaderworkerset.WorkerIndexLabelKey: "0",
					leaderworkerset.GroupIndexLabelKey:  "0",
					leaderworkerset.RevisionKey:         revision,
				},
				Annotations: map[string]string{
					schedulerprovider.WorkloadSchedulingAnnotationKey: string(schedulerprovider.SchedulingModeReplica),
				},
			},
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{{
					Name:  "leader",
					Image: "nginx",
				}},
				Hostname:  lws.Name + "-0",
				Subdomain: lws.Name,
				SchedulingGroup: &corev1.PodSchedulingGroup{
					PodGroupName: ptr.To(podGroupName),
				},
			},
		}
		gomega.Expect(k8sClient.Create(ctx, leaderPod)).To(gomega.Succeed())

		gomega.Eventually(func(g gomega.Gomega) {
			groups := &schedulingv1beta1.PodGroupList{}
			g.Expect(k8sClient.List(ctx, groups, client.InNamespace(ns.Name), client.MatchingLabels{
				leaderworkerset.SetNameLabelKey: lws.Name,
			})).To(gomega.Succeed())
			g.Expect(groups.Items).To(gomega.HaveLen(1))
			g.Expect(groups.Items[0].Name).To(gomega.Equal(podGroupName))
		}, testing.Timeout, testing.Interval).Should(gomega.Succeed())

		workerPod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      lws.Name + "-0-1",
				Namespace: ns.Name,
				Labels: map[string]string{
					leaderworkerset.SetNameLabelKey:     lws.Name,
					leaderworkerset.WorkerIndexLabelKey: "1",
					leaderworkerset.GroupIndexLabelKey:  "0",
				},
				Annotations: map[string]string{
					schedulerprovider.WorkloadSchedulingAnnotationKey: string(schedulerprovider.SchedulingModeReplica),
				},
			},
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{{
					Name:  "worker",
					Image: "nginx",
				}},
				Hostname:  lws.Name + "-0-1",
				Subdomain: lws.Name,
				SchedulingGroup: &corev1.PodSchedulingGroup{
					PodGroupName: ptr.To(podGroupName),
				},
			},
		}
		gomega.Expect(k8sClient.Create(ctx, workerPod)).To(gomega.Succeed())

		// Scale replicas down to 0
		gomega.Eventually(func() error {
			persisted := &leaderworkerset.LeaderWorkerSet{}
			if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(lws), persisted); err != nil {
				return err
			}
			persisted.Spec.Replicas = ptr.To[int32](0)
			return k8sClient.Update(ctx, persisted)
		}, testing.Timeout, testing.Interval).Should(gomega.Succeed())

		// Delete the leader pod and worker StatefulSet first while keeping worker pod alive.
		gomega.Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, leaderPod))).To(gomega.Succeed())
		workerSts := &appsv1.StatefulSet{
			ObjectMeta: metav1.ObjectMeta{
				Name:      leaderPod.Name,
				Namespace: ns.Name,
			},
		}
		_ = client.IgnoreNotFound(k8sClient.Delete(ctx, workerSts))

		// Verify the PodGroup is retained because the worker pod is still referencing it.
		gomega.Consistently(func() error {
			return k8sClient.Get(ctx, types.NamespacedName{Namespace: ns.Name, Name: podGroupName}, &schedulingv1beta1.PodGroup{})
		}, "1s", "100ms").Should(gomega.Succeed())

		// Delete the worker pod.
		gomega.Expect(k8sClient.Delete(ctx, workerPod)).To(gomega.Succeed())

		// The obsolete PodGroup should have deletion requested automatically.
		deletingGroup := &schedulingv1beta1.PodGroup{}
		gomega.Eventually(func(g gomega.Gomega) {
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns.Name, Name: podGroupName}, deletingGroup)).To(gomega.Succeed())
			g.Expect(deletingGroup.DeletionTimestamp).NotTo(gomega.BeNil())
		}, testing.Timeout, testing.Interval).Should(gomega.Succeed())

		// envtest does not run the upstream PodGroup protection controller, so
		// emulate its finalizer removal after LWS has requested deletion.
		deletingGroup.Finalizers = nil
		gomega.Expect(k8sClient.Update(ctx, deletingGroup)).To(gomega.Succeed())
		gomega.Eventually(func() bool {
			err := k8sClient.Get(ctx, types.NamespacedName{Namespace: ns.Name, Name: podGroupName}, &schedulingv1beta1.PodGroup{})
			return apierrors.IsNotFound(err)
		}, testing.Timeout, testing.Interval).Should(gomega.BeTrue())
	})

	ginkgo.It("gives a recreated ordinal leader its own PodGroup while the previous one is still terminating", func() {
		lws := wrappers.BuildLeaderWorkerSet(ns.Name).
			Name("was-recreate").
			Replica(1).
			Size(2).
			Obj()
		lws.Spec.Scheduling = &leaderworkerset.LeaderWorkerSetScheduling{}
		gomega.Expect(k8sClient.Create(ctx, lws)).To(gomega.Succeed())

		leaderStatefulSet := &appsv1.StatefulSet{}
		gomega.Eventually(func(g gomega.Gomega) {
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns.Name, Name: lws.Name}, leaderStatefulSet)).To(gomega.Succeed())
			g.Expect(leaderStatefulSet.Spec.Template.Annotations[schedulerprovider.WorkloadNameAnnotationKey]).NotTo(gomega.BeEmpty())
		}, testing.Timeout, testing.Interval).Should(gomega.Succeed())

		// createLeader stands in for the statefulset controller and the pod
		// webhook. Every leader of group 0 has the same name, index and revision.
		createLeader := func() *corev1.Pod {
			gomega.Expect(testing.CreateLeaderPodsWithInjectFn(ctx, *leaderStatefulSet, k8sClient, lws, 0, 1, func(pod *corev1.Pod) {
				for key, value := range leaderStatefulSet.Spec.Template.Annotations {
					pod.Annotations[key] = value
				}
				pod.Spec.SchedulingGates = []corev1.PodSchedulingGate{{Name: leaderworkerset.GroupReplacementSchedulingGate}}
				gomega.Expect(schedulerprovider.NewKubernetesProvider(k8sClient).InjectPodGroupMetadata(pod)).To(gomega.Succeed())
			})).To(gomega.Succeed())
			leader := &corev1.Pod{}
			gomega.Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns.Name, Name: lws.Name + "-0"}, leader)).To(gomega.Succeed())
			gomega.Expect(leader.Spec.SchedulingGroup).NotTo(gomega.BeNil())
			// The pod webhook gives the leader a group incarnation of its own.
			gomega.Expect(leader.Annotations[schedulerprovider.WorkloadNameAnnotationKey]).To(gomega.HavePrefix(schedulerprovider.KubernetesWorkloadName(lws) + "."))
			return leader
		}
		// expectAdmitted waits for the PodGroup of the leader, for its gate to be
		// lifted and for a worker statefulset that joins the same PodGroup.
		expectAdmitted := func(leader *corev1.Pod) *schedulingv1beta1.PodGroup {
			group := &schedulingv1beta1.PodGroup{}
			gomega.Eventually(func(g gomega.Gomega) {
				g.Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns.Name, Name: *leader.Spec.SchedulingGroup.PodGroupName}, group)).To(gomega.Succeed())
				g.Expect(group.DeletionTimestamp).To(gomega.BeNil())
				pod := &corev1.Pod{}
				g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(leader), pod)).To(gomega.Succeed())
				g.Expect(pod.UID).To(gomega.Equal(leader.UID))
				g.Expect(pod.Spec.SchedulingGates).To(gomega.BeEmpty())
				workerStatefulSet := &appsv1.StatefulSet{}
				g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(leader), workerStatefulSet)).To(gomega.Succeed())
				if !metav1.IsControlledBy(workerStatefulSet, leader) {
					// envtest runs no garbage collector, so remove the worker
					// statefulset of a previous leader.
					_ = k8sClient.Delete(ctx, workerStatefulSet, client.Preconditions{UID: &workerStatefulSet.UID})
				}
				g.Expect(metav1.IsControlledBy(workerStatefulSet, leader)).To(gomega.BeTrue())
				g.Expect(workerStatefulSet.Spec.Template.Annotations).To(gomega.HaveKeyWithValue(
					schedulerprovider.WorkloadNameAnnotationKey, leader.Annotations[schedulerprovider.WorkloadNameAnnotationKey]))
			}, testing.Timeout, testing.Interval).Should(gomega.Succeed())
			return group
		}

		first := createLeader()
		firstGroup := expectAdmitted(first)

		// Hold the PodGroup the way the PodGroup protection finalizer does
		// while pods still reference it.
		gomega.Eventually(func() error {
			if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(firstGroup), firstGroup); err != nil {
				return err
			}
			firstGroup.Finalizers = append(firstGroup.Finalizers, "lws.test/hold")
			return k8sClient.Update(ctx, firstGroup)
		}, testing.Timeout, testing.Interval).Should(gomega.Succeed())
		gomega.Expect(k8sClient.Delete(ctx, first)).To(gomega.Succeed())
		gomega.Eventually(func(g gomega.Gomega) {
			g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(firstGroup), firstGroup)).To(gomega.Succeed())
			g.Expect(firstGroup.DeletionTimestamp).NotTo(gomega.BeNil())
		}, testing.Timeout, testing.Interval).Should(gomega.Succeed())

		second := createLeader()
		gomega.Expect(second.UID).NotTo(gomega.Equal(first.UID))
		secondGroup := expectAdmitted(second)
		gomega.Expect(secondGroup.Name).NotTo(gomega.Equal(firstGroup.Name))
		gomega.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(firstGroup), firstGroup)).To(gomega.Succeed())
		gomega.Expect(firstGroup.DeletionTimestamp).NotTo(gomega.BeNil(), "the new leader must not wait for the previous PodGroup")

		// Once the previous PodGroup is released, only the new leader's remains.
		gomega.Eventually(func() error {
			if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(firstGroup), firstGroup); err != nil {
				return err
			}
			firstGroup.Finalizers = nil
			return k8sClient.Update(ctx, firstGroup)
		}, testing.Timeout, testing.Interval).Should(gomega.Succeed())
		gomega.Eventually(func(g gomega.Gomega) {
			groups := &schedulingv1beta1.PodGroupList{}
			g.Expect(k8sClient.List(ctx, groups, client.InNamespace(ns.Name), client.MatchingLabels{
				leaderworkerset.SetNameLabelKey: lws.Name,
			})).To(gomega.Succeed())
			g.Expect(groups.Items).To(gomega.HaveLen(1))
			g.Expect(groups.Items[0].Name).To(gomega.Equal(secondGroup.Name))
		}, testing.Timeout, testing.Interval).Should(gomega.Succeed())
	})

	ginkgo.Context("when an LWS upgrade splits a group across PodGroups", func() {
		// startGroup creates leader 0 of a new LWS the way the statefulset
		// controller and the pod webhook of this version do, and returns it
		// with the worker statefulset the pod controller creates for it.
		startGroup := func(name string) (*leaderworkerset.LeaderWorkerSet, *corev1.Pod, *appsv1.StatefulSet) {
			lws := wrappers.BuildLeaderWorkerSet(ns.Name).
				Name(name).
				Replica(1).
				Size(2).
				Obj()
			lws.Spec.Scheduling = &leaderworkerset.LeaderWorkerSetScheduling{}
			gomega.Expect(k8sClient.Create(ctx, lws)).To(gomega.Succeed())
			leaderStatefulSet := &appsv1.StatefulSet{}
			gomega.Eventually(func(g gomega.Gomega) {
				g.Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns.Name, Name: lws.Name}, leaderStatefulSet)).To(gomega.Succeed())
				g.Expect(leaderStatefulSet.Spec.Template.Annotations[schedulerprovider.WorkloadNameAnnotationKey]).NotTo(gomega.BeEmpty())
			}, testing.Timeout, testing.Interval).Should(gomega.Succeed())
			gomega.Expect(testing.CreateLeaderPodsWithInjectFn(ctx, *leaderStatefulSet, k8sClient, lws, 0, 1, func(pod *corev1.Pod) {
				for key, value := range leaderStatefulSet.Spec.Template.Annotations {
					pod.Annotations[key] = value
				}
				pod.Spec.SchedulingGates = []corev1.PodSchedulingGate{{Name: leaderworkerset.GroupReplacementSchedulingGate}}
				gomega.Expect(schedulerprovider.NewKubernetesProvider(k8sClient).InjectPodGroupMetadata(pod)).To(gomega.Succeed())
			})).To(gomega.Succeed())
			leader := &corev1.Pod{}
			gomega.Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns.Name, Name: lws.Name + "-0"}, leader)).To(gomega.Succeed())
			workers := &appsv1.StatefulSet{}
			gomega.Eventually(func(g gomega.Gomega) {
				g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(leader), workers)).To(gomega.Succeed())
				g.Expect(metav1.IsControlledBy(workers, leader)).To(gomega.BeTrue())
			}, testing.Timeout, testing.Interval).Should(gomega.Succeed())
			return lws, leader, workers
		}
		// splitGroup gives the worker statefulset the template the pod
		// controller of a version without group incarnations creates: its
		// workers join the PodGroups of the plain workload name instead of
		// those of the leader.
		splitGroup := func(lws *leaderworkerset.LeaderWorkerSet, workers *appsv1.StatefulSet) {
			gomega.Eventually(func() error {
				if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(workers), workers); err != nil {
					return err
				}
				workers.Spec.Template.Annotations[schedulerprovider.WorkloadNameAnnotationKey] = schedulerprovider.KubernetesWorkloadName(lws)
				return k8sClient.Update(ctx, workers)
			}, testing.Timeout, testing.Interval).Should(gomega.Succeed())
		}

		ginkgo.It("recreates the group while none of its workers is scheduled", func() {
			lws, leader, workers := startGroup("was-split")
			splitGroup(lws, workers)
			// envtest runs no garbage collector, so the foreground deletion of
			// the leader waits for its worker statefulset.
			gomega.Eventually(func(g gomega.Gomega) {
				pod := &corev1.Pod{}
				g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(leader), pod)).To(gomega.Succeed())
				g.Expect(pod.UID).To(gomega.Equal(leader.UID))
				g.Expect(pod.DeletionTimestamp).NotTo(gomega.BeNil())
				g.Expect(pod.Finalizers).To(gomega.ContainElement(metav1.FinalizerDeleteDependents))
			}, testing.Timeout, testing.Interval).Should(gomega.Succeed())
			testing.ValidateEvent(ctx, k8sClient, "RecreateGroup", corev1.EventTypeNormal, fmt.Sprintf(
				"Worker statefulset %s was created by a previous LWS version and its workers do not join the PodGroups of leader pod %s, deleted the leader pod to recreate group 0",
				workers.Name, leader.Name), ns.Name)
		})

		ginkgo.It("leaves the group alone once one of its workers is scheduled", func() {
			lws, leader, workers := startGroup("was-split-scheduled")
			worker := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      leader.Name + "-1",
					Namespace: ns.Name,
					Labels: map[string]string{
						leaderworkerset.SetNameLabelKey:     lws.Name,
						leaderworkerset.GroupIndexLabelKey:  "0",
						leaderworkerset.WorkerIndexLabelKey: "1",
					},
					OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(workers, appsv1.SchemeGroupVersion.WithKind("StatefulSet"))},
				},
				Spec: corev1.PodSpec{
					NodeName:   "node-1",
					Containers: []corev1.Container{{Name: "worker", Image: "nginx"}},
				},
			}
			gomega.Expect(k8sClient.Create(ctx, worker)).To(gomega.Succeed())
			expectLeaderKept := func(duration string) {
				gomega.Consistently(func(g gomega.Gomega) {
					pod := &corev1.Pod{}
					g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(leader), pod)).To(gomega.Succeed())
					g.Expect(pod.UID).To(gomega.Equal(leader.UID))
					g.Expect(pod.DeletionTimestamp).To(gomega.BeNil())
				}, duration, testing.Interval).Should(gomega.Succeed())
			}
			// Let the cache of the pod controller observe the scheduled worker
			// before the group is split.
			expectLeaderKept("1s")
			splitGroup(lws, workers)
			expectLeaderKept("2s")
		})
	})
})

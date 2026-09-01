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

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	schedulingv1alpha3 "k8s.io/api/scheduling/v1alpha3"
	schedulingv1beta1 "k8s.io/api/scheduling/v1beta1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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

	ginkgo.It("creates replica Workload and PodGroups before releasing the leader StatefulSet", func() {
		lws := wrappers.BuildLeaderWorkerSet(ns.Name).
			Name("was-replica").
			Replica(2).
			Size(3).
			Obj()
		lws.Spec.Scheduling = &leaderworkerset.LeaderWorkerSetScheduling{}
		gomega.Expect(k8sClient.Create(ctx, lws)).To(gomega.Succeed())

		gomega.Eventually(func(g gomega.Gomega) {
			workload := &schedulingv1beta1.Workload{}
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns.Name, Name: schedulerprovider.KubernetesWorkloadName(lws)}, workload)).To(gomega.Succeed())
			g.Expect(workload.Spec.PodGroupTemplates).To(gomega.HaveLen(1))
			g.Expect(workload.Spec.PodGroupTemplates[0].Name).To(gomega.Equal("replica"))
			g.Expect(workload.Spec.PodGroupTemplates[0].SchedulingPolicy.Gang).NotTo(gomega.BeNil())
			g.Expect(workload.Spec.PodGroupTemplates[0].SchedulingPolicy.Gang.MinCount).To(gomega.Equal(int32(3)))

			groups := &schedulingv1beta1.PodGroupList{}
			g.Expect(k8sClient.List(ctx, groups, client.InNamespace(ns.Name), client.MatchingLabels{
				leaderworkerset.SetNameLabelKey: lws.Name,
			})).To(gomega.Succeed())
			g.Expect(groups.Items).To(gomega.HaveLen(2))
			for i := range groups.Items {
				g.Expect(groups.Items[i].Spec.WorkloadRef).NotTo(gomega.BeNil())
				g.Expect(groups.Items[i].Spec.WorkloadRef.WorkloadName).To(gomega.Equal(schedulerprovider.KubernetesWorkloadName(lws)))
				g.Expect(groups.Items[i].Spec.WorkloadRef.TemplateName).To(gomega.Equal("replica"))
			}

			leaderStatefulSet := &appsv1.StatefulSet{}
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns.Name, Name: lws.Name}, leaderStatefulSet)).To(gomega.Succeed())
			g.Expect(leaderStatefulSet.Spec.Template.Annotations[schedulerprovider.WorkloadSchedulingAnnotationKey]).To(gomega.Equal(string(schedulerprovider.SchedulingModeReplica)))
			g.Expect(leaderStatefulSet.Spec.Template.Annotations[schedulerprovider.WorkloadNameAnnotationKey]).To(gomega.Equal(schedulerprovider.KubernetesWorkloadName(lws)))

			persistedLWS := &leaderworkerset.LeaderWorkerSet{}
			g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(lws), persistedLWS)).To(gomega.Succeed())
			g.Expect(apimeta.IsStatusConditionTrue(persistedLWS.Status.Conditions, string(leaderworkerset.LeaderWorkerSetWorkloadSchedulingReady))).To(gomega.BeTrue())
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
})

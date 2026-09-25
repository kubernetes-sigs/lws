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
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	leaderworkerset "sigs.k8s.io/lws/api/leaderworkerset/v1"
	podutils "sigs.k8s.io/lws/pkg/utils/pod"
	testing "sigs.k8s.io/lws/test/testutils"
	"sigs.k8s.io/lws/test/wrappers"
)

var _ = ginkgo.Describe("Group replacement policy", func() {
	// Neither the Deployment controller nor the pod webhook run in this suite,
	// so leader pods are created by hand the way admission would leave them.
	makeHashLeader := func(lws *leaderworkerset.LeaderWorkerSet, name, revisionKey string, gated bool) *corev1.Pod {
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: lws.Namespace,
				Labels: map[string]string{
					leaderworkerset.SetNameLabelKey:         lws.Name,
					leaderworkerset.WorkerIndexLabelKey:     "0",
					leaderworkerset.GroupIndexLabelKey:      name,
					leaderworkerset.GroupUniqueHashLabelKey: name,
					leaderworkerset.RevisionKey:             revisionKey,
				},
				Annotations: map[string]string{
					leaderworkerset.SizeAnnotationKey:          "2",
					leaderworkerset.GroupIdentityAnnotationKey: string(leaderworkerset.GroupIdentityHash),
				},
			},
			Spec: *lws.Spec.LeaderWorkerTemplate.WorkerTemplate.Spec.DeepCopy(),
		}
		pod.Spec.Hostname = name
		pod.Spec.Subdomain = lws.Name
		if gated {
			pod.Spec.SchedulingGates = []corev1.PodSchedulingGate{{Name: leaderworkerset.GroupReplacementSchedulingGate}}
		}
		return pod
	}

	ginkgo.It("holds a gated leader back until a terminating leader is gone", func() {
		ctx := context.Background()
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "lws-ns-"}}
		gomega.Expect(k8sClient.Create(ctx, ns)).To(gomega.Succeed())

		lws := wrappers.BuildLeaderWorkerSet(ns.Name).Replica(1).Size(2).Obj()
		lws.Spec.GroupIdentity = leaderworkerset.GroupIdentityHash
		gomega.Expect(k8sClient.Create(ctx, lws)).To(gomega.Succeed())

		var deploy appsv1.Deployment
		gomega.Eventually(func() error {
			return k8sClient.Get(ctx, types.NamespacedName{Name: lws.Name, Namespace: lws.Namespace}, &deploy)
		}, testing.Timeout, testing.Interval).Should(gomega.Succeed())
		revisionKey := deploy.Labels[leaderworkerset.RevisionKey]

		// A finalizer stands in for the foreground deletion that keeps a real
		// leader around until its worker statefulset is gone.
		const holdFinalizer = "lws.test/hold"
		old := makeHashLeader(lws, "old-leader", revisionKey, false)
		old.Finalizers = []string{holdFinalizer}
		gomega.Expect(k8sClient.Create(ctx, old)).To(gomega.Succeed())
		gomega.Expect(k8sClient.Delete(ctx, old)).To(gomega.Succeed())
		gomega.Eventually(func() bool {
			var pod corev1.Pod
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: old.Name, Namespace: old.Namespace}, &pod); err != nil {
				return false
			}
			return pod.DeletionTimestamp != nil
		}, testing.Timeout, testing.Interval).Should(gomega.BeTrue())

		replacement := makeHashLeader(lws, "new-leader", revisionKey, true)
		gomega.Expect(k8sClient.Create(ctx, replacement)).To(gomega.Succeed())
		replacementKey := types.NamespacedName{Name: replacement.Name, Namespace: replacement.Namespace}

		ginkgo.By("keeping the replacement gated and without a worker statefulset while the old leader terminates")
		gomega.Consistently(func() bool {
			var pod corev1.Pod
			if err := k8sClient.Get(ctx, replacementKey, &pod); err != nil {
				return false
			}
			if !podutils.HasSchedulingGate(&pod, leaderworkerset.GroupReplacementSchedulingGate) {
				return false
			}
			var sts appsv1.StatefulSet
			return apierrors.IsNotFound(k8sClient.Get(ctx, types.NamespacedName{Namespace: pod.Namespace, Name: pod.Spec.Hostname}, &sts))
		}, 3*time.Second, testing.Interval).Should(gomega.BeTrue())

		ginkgo.By("releasing the old leader")
		gomega.Eventually(func() error {
			var pod corev1.Pod
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: old.Name, Namespace: old.Namespace}, &pod); err != nil {
				return err
			}
			pod.Finalizers = nil
			return k8sClient.Update(ctx, &pod)
		}, testing.Timeout, testing.Interval).Should(gomega.Succeed())

		ginkgo.By("admitting the replacement and creating its worker statefulset")
		gomega.Eventually(func() bool {
			var pod corev1.Pod
			if err := k8sClient.Get(ctx, replacementKey, &pod); err != nil {
				return false
			}
			if podutils.HasSchedulingGate(&pod, leaderworkerset.GroupReplacementSchedulingGate) {
				return false
			}
			var sts appsv1.StatefulSet
			return k8sClient.Get(ctx, types.NamespacedName{Namespace: pod.Namespace, Name: pod.Spec.Hostname}, &sts) == nil
		}, testing.Timeout, testing.Interval).Should(gomega.BeTrue())
	})

	ginkgo.It("admits a gated leader immediately under the Immediate policy", func() {
		ctx := context.Background()
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "lws-ns-"}}
		gomega.Expect(k8sClient.Create(ctx, ns)).To(gomega.Succeed())

		lws := wrappers.BuildLeaderWorkerSet(ns.Name).Replica(1).Size(2).Obj()
		lws.Spec.GroupIdentity = leaderworkerset.GroupIdentityHash
		lws.Spec.GroupReplacementPolicy = leaderworkerset.GroupReplacementImmediate
		gomega.Expect(k8sClient.Create(ctx, lws)).To(gomega.Succeed())

		var deploy appsv1.Deployment
		gomega.Eventually(func() error {
			return k8sClient.Get(ctx, types.NamespacedName{Name: lws.Name, Namespace: lws.Namespace}, &deploy)
		}, testing.Timeout, testing.Interval).Should(gomega.Succeed())
		revisionKey := deploy.Labels[leaderworkerset.RevisionKey]

		old := makeHashLeader(lws, "old-leader", revisionKey, false)
		old.Finalizers = []string{"lws.test/hold"}
		gomega.Expect(k8sClient.Create(ctx, old)).To(gomega.Succeed())
		gomega.Expect(k8sClient.Delete(ctx, old)).To(gomega.Succeed())

		replacement := makeHashLeader(lws, "new-leader", revisionKey, true)
		gomega.Expect(k8sClient.Create(ctx, replacement)).To(gomega.Succeed())
		replacementKey := types.NamespacedName{Name: replacement.Name, Namespace: replacement.Namespace}

		gomega.Eventually(func() bool {
			var pod corev1.Pod
			if err := k8sClient.Get(ctx, replacementKey, &pod); err != nil {
				return false
			}
			return !podutils.HasSchedulingGate(&pod, leaderworkerset.GroupReplacementSchedulingGate)
		}, testing.Timeout, testing.Interval).Should(gomega.BeTrue())

		// Leave the namespace deletable.
		gomega.Eventually(func() error {
			var pod corev1.Pod
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: old.Name, Namespace: old.Namespace}, &pod); err != nil {
				return client.IgnoreNotFound(err)
			}
			pod.Finalizers = nil
			return k8sClient.Update(ctx, &pod)
		}, testing.Timeout, testing.Interval).Should(gomega.Succeed())
	})

	// A rolling update or scale down deletes the leader through the ReplicaSet
	// in the background, so the leader object can be gone while its workers
	// still hold capacity. The surviving worker must keep the slot occupied.
	ginkgo.It("holds a gated leader while a worker of an already deleted leader still exists", func() {
		ctx := context.Background()
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "lws-ns-"}}
		gomega.Expect(k8sClient.Create(ctx, ns)).To(gomega.Succeed())

		lws := wrappers.BuildLeaderWorkerSet(ns.Name).Replica(1).Size(2).Obj()
		lws.Spec.GroupIdentity = leaderworkerset.GroupIdentityHash
		gomega.Expect(k8sClient.Create(ctx, lws)).To(gomega.Succeed())

		var deploy appsv1.Deployment
		gomega.Eventually(func() error {
			return k8sClient.Get(ctx, types.NamespacedName{Name: lws.Name, Namespace: lws.Namespace}, &deploy)
		}, testing.Timeout, testing.Interval).Should(gomega.Succeed())
		revisionKey := deploy.Labels[leaderworkerset.RevisionKey]

		const holdFinalizer = "lws.test/hold"
		orphanWorker := makeHashLeader(lws, "old-leader-1", revisionKey, false)
		orphanWorker.Labels[leaderworkerset.WorkerIndexLabelKey] = "1"
		orphanWorker.Labels[leaderworkerset.GroupIndexLabelKey] = "old-leader"
		orphanWorker.Labels[leaderworkerset.GroupUniqueHashLabelKey] = "old-leader"
		orphanWorker.Spec.Hostname = ""
		orphanWorker.Finalizers = []string{holdFinalizer}
		gomega.Expect(k8sClient.Create(ctx, orphanWorker)).To(gomega.Succeed())
		gomega.Expect(k8sClient.Delete(ctx, orphanWorker)).To(gomega.Succeed())

		replacement := makeHashLeader(lws, "new-leader", revisionKey, true)
		gomega.Expect(k8sClient.Create(ctx, replacement)).To(gomega.Succeed())
		replacementKey := types.NamespacedName{Name: replacement.Name, Namespace: replacement.Namespace}

		ginkgo.By("keeping the replacement gated while the orphaned worker exists")
		gomega.Consistently(func() bool {
			var pod corev1.Pod
			if err := k8sClient.Get(ctx, replacementKey, &pod); err != nil {
				return false
			}
			if !podutils.HasSchedulingGate(&pod, leaderworkerset.GroupReplacementSchedulingGate) {
				return false
			}
			var sts appsv1.StatefulSet
			return apierrors.IsNotFound(k8sClient.Get(ctx, types.NamespacedName{Namespace: pod.Namespace, Name: pod.Spec.Hostname}, &sts))
		}, 3*time.Second, testing.Interval).Should(gomega.BeTrue())

		ginkgo.By("releasing the orphaned worker")
		gomega.Eventually(func() error {
			var pod corev1.Pod
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: orphanWorker.Name, Namespace: orphanWorker.Namespace}, &pod); err != nil {
				return err
			}
			pod.Finalizers = nil
			return k8sClient.Update(ctx, &pod)
		}, testing.Timeout, testing.Interval).Should(gomega.Succeed())

		ginkgo.By("admitting the replacement once the old group has no pods left")
		gomega.Eventually(func() bool {
			var pod corev1.Pod
			if err := k8sClient.Get(ctx, replacementKey, &pod); err != nil {
				return false
			}
			if podutils.HasSchedulingGate(&pod, leaderworkerset.GroupReplacementSchedulingGate) {
				return false
			}
			var sts appsv1.StatefulSet
			return k8sClient.Get(ctx, types.NamespacedName{Namespace: pod.Namespace, Name: pod.Spec.Hostname}, &sts) == nil
		}, testing.Timeout, testing.Interval).Should(gomega.BeTrue())
	})

	ginkgo.It("enforces maxGroupRestarts in Hash mode, holds back gated replacements, sets Degraded, and recovers on recover=true", func() {
		ctx := context.Background()
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "lws-ns-"}}
		gomega.Expect(k8sClient.Create(ctx, ns)).To(gomega.Succeed())

		lws := wrappers.BuildLeaderWorkerSet(ns.Name).Replica(1).Size(2).
			RestartPolicy(leaderworkerset.RecreateGroupOnPodRestart).MaxGroupRestarts(1).Obj()
		lws.Spec.GroupIdentity = leaderworkerset.GroupIdentityHash
		lws.Spec.GroupReplacementPolicy = leaderworkerset.GroupReplacementImmediate
		gomega.Expect(k8sClient.Create(ctx, lws)).To(gomega.Succeed())

		var deploy appsv1.Deployment
		gomega.Eventually(func() error {
			return k8sClient.Get(ctx, types.NamespacedName{Name: lws.Name, Namespace: lws.Namespace}, &deploy)
		}, testing.Timeout, testing.Interval).Should(gomega.Succeed())
		revisionKey := deploy.Labels[leaderworkerset.RevisionKey]

		ginkgo.By("creating initial hash leader and triggering its first failure within budget")
		leader1 := makeHashLeader(lws, "hash-leader-1", revisionKey, false)
		gomega.Expect(k8sClient.Create(ctx, leader1)).To(gomega.Succeed())
		leader1.Status.Phase = corev1.PodRunning
		leader1.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "leader", RestartCount: 1}}
		gomega.Expect(k8sClient.Status().Update(ctx, leader1)).To(gomega.Succeed())

		gomega.Eventually(func() bool {
			var pod corev1.Pod
			err := k8sClient.Get(ctx, types.NamespacedName{Name: leader1.Name, Namespace: leader1.Namespace}, &pod)
			if apierrors.IsNotFound(err) {
				return true
			}
			if err == nil && pod.DeletionTimestamp != nil {
				pod.Finalizers = nil
				_ = k8sClient.Update(ctx, &pod)
			}
			return false
		}, testing.Timeout, testing.Interval).Should(gomega.BeTrue())

		ginkgo.By("admitting replacement leader2 and verifying it claims the restart count")
		leader2 := makeHashLeader(lws, "hash-leader-2", revisionKey, true)
		gomega.Expect(k8sClient.Create(ctx, leader2)).To(gomega.Succeed())
		leader2Key := types.NamespacedName{Name: leader2.Name, Namespace: leader2.Namespace}

		gomega.Eventually(func() bool {
			var pod corev1.Pod
			if err := k8sClient.Get(ctx, leader2Key, &pod); err != nil {
				return false
			}
			return !podutils.HasSchedulingGate(&pod, leaderworkerset.GroupReplacementSchedulingGate)
		}, testing.Timeout, testing.Interval).Should(gomega.BeTrue())

		gomega.Eventually(func() string {
			var currentLWS leaderworkerset.LeaderWorkerSet
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: lws.Name, Namespace: lws.Namespace}, &currentLWS); err != nil {
				return ""
			}
			return currentLWS.Annotations[leaderworkerset.GroupRestartCountsAnnotationKey]
		}, testing.Timeout, testing.Interval).Should(gomega.ContainSubstring(revisionKey + "/hash-leader-2"))

		ginkgo.By("triggering a second failure on leader2 to exhaust the restart budget")
		gomega.Eventually(func() error {
			var pod corev1.Pod
			if err := k8sClient.Get(ctx, leader2Key, &pod); err != nil {
				return err
			}
			pod.Status.Phase = corev1.PodRunning
			pod.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "leader", RestartCount: 1}}
			return k8sClient.Status().Update(ctx, &pod)
		}, testing.Timeout, testing.Interval).Should(gomega.Succeed())

		gomega.Eventually(func() bool {
			var pod corev1.Pod
			if err := k8sClient.Get(ctx, leader2Key, &pod); err != nil {
				return false
			}
			return pod.DeletionTimestamp != nil &&
				pod.Annotations[leaderworkerset.GroupRestartBudgetExhaustedAnnotationKey] == "true"
		}, testing.Timeout, testing.Interval).Should(gomega.BeTrue())

		ginkgo.By("keeping gated replacement leader3 held back and setting Degraded=True on LWS")
		leader3 := makeHashLeader(lws, "hash-leader-3", revisionKey, true)
		gomega.Expect(k8sClient.Create(ctx, leader3)).To(gomega.Succeed())
		leader3Key := types.NamespacedName{Name: leader3.Name, Namespace: leader3.Namespace}

		gomega.Consistently(func() bool {
			var pod corev1.Pod
			if err := k8sClient.Get(ctx, leader3Key, &pod); err != nil {
				return false
			}
			return podutils.HasSchedulingGate(&pod, leaderworkerset.GroupReplacementSchedulingGate)
		}, 2*time.Second, testing.Interval).Should(gomega.BeTrue())

		gomega.Eventually(func() bool {
			var currentLWS leaderworkerset.LeaderWorkerSet
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: lws.Name, Namespace: lws.Namespace}, &currentLWS); err != nil {
				return false
			}
			for _, c := range currentLWS.Status.Conditions {
				if c.Type == string(leaderworkerset.LeaderWorkerSetDegraded) && c.Status == metav1.ConditionTrue {
					return true
				}
			}
			return false
		}, testing.Timeout, testing.Interval).Should(gomega.BeTrue())

		ginkgo.By("setting recover=true on retained leader2 to clear the budget and admit leader3")
		gomega.Eventually(func() error {
			var pod corev1.Pod
			if err := k8sClient.Get(ctx, leader2Key, &pod); err != nil {
				return err
			}
			if pod.Annotations == nil {
				pod.Annotations = map[string]string{}
			}
			pod.Annotations[leaderworkerset.GroupRestartBudgetRecoverAnnotationKey] = "true"
			return k8sClient.Update(ctx, &pod)
		}, testing.Timeout, testing.Interval).Should(gomega.Succeed())

		gomega.Eventually(func() bool {
			var pod corev1.Pod
			if err := k8sClient.Get(ctx, leader2Key, &pod); apierrors.IsNotFound(err) {
				return true
			} else if err != nil {
				return false
			}
			for _, f := range pod.Finalizers {
				if f == leaderworkerset.GroupRestartBudgetCleanupFinalizer {
					return false
				}
			}
			// envtest does not run the garbage collector to remove foregroundDeletion.
			pod.Finalizers = nil
			_ = k8sClient.Update(ctx, &pod)
			return false
		}, testing.Timeout, testing.Interval).Should(gomega.BeTrue())

		gomega.Eventually(func() bool {
			var pod corev1.Pod
			if err := k8sClient.Get(ctx, leader3Key, &pod); err != nil {
				return false
			}
			return !podutils.HasSchedulingGate(&pod, leaderworkerset.GroupReplacementSchedulingGate)
		}, testing.Timeout, testing.Interval).Should(gomega.BeTrue())
	})
})

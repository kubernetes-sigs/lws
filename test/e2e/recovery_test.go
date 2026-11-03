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

package e2e

import (
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	leaderworkerset "sigs.k8s.io/lws/api/leaderworkerset/v1"
	podutils "sigs.k8s.io/lws/pkg/utils/pod"
	"sigs.k8s.io/lws/test/testutils"
	"sigs.k8s.io/lws/test/wrappers"
)

// Restarting every controller-manager Pod affects the entire cluster, so this
// spec must not overlap other specs, including when Ginkgo runs in parallel.
var _ = ginkgo.Describe("LeaderWorkerSet recovery", ginkgo.Serial, func() {
	ginkgo.It("resumes group recreation after a controller restart during foreground deletion", func() {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "lws-recovery-"}}
		gomega.Expect(k8sClient.Create(ctx, ns)).To(gomega.Succeed())
		ginkgo.DeferCleanup(func() {
			gomega.Expect(testutils.DeleteNamespace(ctx, k8sClient, ns)).To(gomega.Succeed())
		})

		// A one-restart budget makes replaying the same failure observable: a
		// duplicate recreation would exhaust the budget instead of recovering.
		lws := wrappers.BuildLeaderWorkerSet(ns.Name).Replica(2).Size(4).
			RestartPolicy(leaderworkerset.RecreateGroupOnPodRestart).MaxGroupRestarts(1).Obj()
		lws.Spec.GroupIdentity = leaderworkerset.GroupIdentityOrdinal
		// Exercise Pod lifecycle only, with a small image and no engine startup.
		lws.Spec.LeaderWorkerTemplate.WorkerTemplate.Spec.Containers = []corev1.Container{{
			Name: "worker", Image: "registry.k8s.io/pause:3.10",
		}}
		lws.Spec.LeaderWorkerTemplate.LeaderTemplate = nil // Use the same lightweight template for leaders.
		testutils.MustCreateLws(ctx, k8sClient, lws)
		testutils.ExpectLeaderWorkerSetAvailable(ctx, k8sClient, lws, "All replicas are ready")

		var initialPods map[string]types.UID
		gomega.Eventually(func() error {
			var err error
			initialPods, err = readyRecoveryPodUIDs(lws)
			return err
		}, timeout, interval).Should(gomega.Succeed())

		leaderKey := types.NamespacedName{Namespace: ns.Name, Name: lws.Name + "-0"}
		var oldLeader corev1.Pod
		gomega.Expect(k8sClient.Get(ctx, leaderKey, &oldLeader)).To(gomega.Succeed())
		var oldWorkers appsv1.StatefulSet
		gomega.Expect(k8sClient.Get(ctx, leaderKey, &oldWorkers)).To(gomega.Succeed())
		var unaffectedWorkers appsv1.StatefulSet
		gomega.Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns.Name, Name: lws.Name + "-1"}, &unaffectedWorkers)).To(gomega.Succeed())

		// Hold the old leader after the foreground DELETE is accepted. This
		// fixes the interruption point without racing a fast garbage collector
		// or relying on sleeps to catch the controller halfway through recovery.
		const holdFinalizer = "lws.test/hold-recovery"
		oldLeader.Finalizers = append(oldLeader.Finalizers, holdFinalizer)
		gomega.Expect(k8sClient.Update(ctx, &oldLeader)).To(gomega.Succeed())
		releaseLeader := func() {
			gomega.Eventually(func() error {
				var current corev1.Pod
				if err := k8sClient.Get(ctx, leaderKey, &current); err != nil {
					return client.IgnoreNotFound(err)
				}
				if current.UID != oldLeader.UID || !slices.Contains(current.Finalizers, holdFinalizer) {
					return nil
				}
				current.Finalizers = slices.DeleteFunc(current.Finalizers, func(f string) bool { return f == holdFinalizer })
				return k8sClient.Update(ctx, &current)
			}, timeout, interval).Should(gomega.Succeed())
		}
		ginkgo.DeferCleanup(releaseLeader)

		ginkgo.By("failing a worker and waiting until its leader is being deleted")
		worker := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: ns.Name, Name: lws.Name + "-0-1"}}
		gomega.Expect(k8sClient.Delete(ctx, worker)).To(gomega.Succeed())
		gomega.Eventually(func(g gomega.Gomega) {
			var current corev1.Pod
			g.Expect(k8sClient.Get(ctx, leaderKey, &current)).To(gomega.Succeed())
			g.Expect(current.UID).To(gomega.Equal(oldLeader.UID))
			g.Expect(current.DeletionTimestamp).NotTo(gomega.BeNil())
			g.Expect(recoveryRestartCounts(lws)).To(gomega.Equal(map[string]int32{
				oldLeader.Labels[leaderworkerset.RevisionKey] + "/0": 1,
			}))
		}, timeout, interval).Should(gomega.Succeed())

		ginkgo.By("restarting every controller-manager Pod while the old group is still terminating")
		restartRecoveryControllers()
		var terminatingLeader corev1.Pod
		gomega.Expect(k8sClient.Get(ctx, leaderKey, &terminatingLeader)).To(gomega.Succeed())
		gomega.Expect(terminatingLeader.UID).To(gomega.Equal(oldLeader.UID))
		gomega.Expect(terminatingLeader.DeletionTimestamp).NotTo(gomega.BeNil())

		ginkgo.By("releasing deletion and recovering only the failed group")
		releaseLeader()
		var recoveredPods map[string]types.UID
		gomega.Eventually(func(g gomega.Gomega) {
			var err error
			recoveredPods, err = readyRecoveryPodUIDs(lws)
			g.Expect(err).NotTo(gomega.HaveOccurred())
			for name, uid := range initialPods {
				g.Expect(recoveredPods).To(gomega.HaveKey(name))
				if name == oldLeader.Name || name == oldLeader.Name+"-1" || name == oldLeader.Name+"-2" || name == oldLeader.Name+"-3" {
					g.Expect(recoveredPods[name]).NotTo(gomega.Equal(uid), "failed-group pod %s must be replaced", name)
				} else {
					g.Expect(recoveredPods[name]).To(gomega.Equal(uid), "healthy-group pod %s must survive", name)
				}
			}
			var workers appsv1.StatefulSetList
			g.Expect(k8sClient.List(ctx, &workers, client.InNamespace(ns.Name), client.MatchingLabels{
				leaderworkerset.SetNameLabelKey: lws.Name,
			})).To(gomega.Succeed())
			g.Expect(workers.Items).To(gomega.HaveLen(3)) // One leader and two worker StatefulSets.
			for _, sts := range workers.Items {
				switch sts.Name {
				case oldWorkers.Name:
					g.Expect(sts.UID).NotTo(gomega.Equal(oldWorkers.UID))
					g.Expect(metav1.GetControllerOf(&sts)).To(gomega.HaveField("UID", recoveredPods[oldLeader.Name]))
				case unaffectedWorkers.Name:
					g.Expect(sts.UID).To(gomega.Equal(unaffectedWorkers.UID))
				}
			}
		}, timeout, interval).Should(gomega.Succeed())
		testutils.ExpectLeaderWorkerSetAvailable(ctx, k8sClient, lws, "All replicas are ready")

		ginkgo.By("checking that the recovered group remains stable and spends only one restart")
		gomega.Consistently(func(g gomega.Gomega) {
			pods, err := readyRecoveryPodUIDs(lws)
			g.Expect(err).NotTo(gomega.HaveOccurred())
			g.Expect(pods).To(gomega.Equal(recoveredPods))
			g.Expect(recoveryRestartCounts(lws)).To(gomega.Equal(map[string]int32{
				oldLeader.Labels[leaderworkerset.RevisionKey] + "/0": 1,
			}))
		}, 5*time.Second, interval).Should(gomega.Succeed())
	})
})

func readyRecoveryPodUIDs(lws *leaderworkerset.LeaderWorkerSet) (map[string]types.UID, error) {
	var pods corev1.PodList
	if err := k8sClient.List(ctx, &pods, client.InNamespace(lws.Namespace), client.MatchingLabels{
		leaderworkerset.SetNameLabelKey: lws.Name,
	}); err != nil {
		return nil, err
	}
	want := int(*lws.Spec.Replicas * *lws.Spec.LeaderWorkerTemplate.Size)
	if len(pods.Items) != want {
		return nil, fmt.Errorf("got %d pods, want %d", len(pods.Items), want)
	}
	uids := make(map[string]types.UID, len(pods.Items))
	for _, pod := range pods.Items {
		if pod.DeletionTimestamp != nil || !podutils.PodRunningAndReady(pod) {
			return nil, fmt.Errorf("pod %s is not running and ready or is terminating", pod.Name)
		}
		uids[pod.Name] = pod.UID
	}
	return uids, nil
}

func recoveryRestartCounts(lws *leaderworkerset.LeaderWorkerSet) (map[string]int32, error) {
	var current leaderworkerset.LeaderWorkerSet
	if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(lws), &current); err != nil {
		return nil, err
	}
	counts := map[string]int32{}
	if err := json.Unmarshal([]byte(current.Annotations[leaderworkerset.GroupRestartCountsAnnotationKey]), &counts); err != nil {
		return nil, err
	}
	return counts, nil
}

func restartRecoveryControllers() {
	namespace := os.Getenv("LWS_NAMESPACE")
	if namespace == "" {
		namespace = "lws-system"
	}
	var deployment appsv1.Deployment
	gomega.Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: namespace, Name: "lws-controller-manager"}, &deployment)).To(gomega.Succeed())
	selector, err := metav1.LabelSelectorAsSelector(deployment.Spec.Selector)
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
	var pods corev1.PodList
	gomega.Expect(k8sClient.List(ctx, &pods, client.InNamespace(namespace), client.MatchingLabelsSelector{Selector: selector})).To(gomega.Succeed())
	gomega.Expect(pods.Items).NotTo(gomega.BeEmpty())
	oldUIDs := make(map[types.UID]struct{}, len(pods.Items))
	for i := range pods.Items {
		oldUIDs[pods.Items[i].UID] = struct{}{}
		gomega.Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, &pods.Items[i]))).To(gomega.Succeed())
	}
	// Wait for new processes, rather than Deployment availability, which may
	// still describe the old Pods. Restored webhook readiness also allows the
	// held leader's finalizer to be removed safely.
	gomega.Eventually(func(g gomega.Gomega) {
		g.Expect(k8sClient.List(ctx, &pods, client.InNamespace(namespace), client.MatchingLabelsSelector{Selector: selector})).To(gomega.Succeed())
		g.Expect(pods.Items).To(gomega.HaveLen(int(*deployment.Spec.Replicas)))
		for _, pod := range pods.Items {
			g.Expect(oldUIDs).NotTo(gomega.HaveKey(pod.UID))
			g.Expect(pod.DeletionTimestamp).To(gomega.BeNil())
			g.Expect(podutils.PodRunningAndReady(pod)).To(gomega.BeTrue())
		}
	}, timeout, interval).Should(gomega.Succeed())
}

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
	"encoding/json"
	"strings"
	"sync"
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	leaderworkerset "sigs.k8s.io/lws/api/leaderworkerset/v1"
	testing "sigs.k8s.io/lws/test/testutils"
	"sigs.k8s.io/lws/test/wrappers"
)

// laggingPodNamespacePrefix marks namespaces whose Pod events reach the
// manager's cache late, as on a busy cluster where the Pod watch falls behind
// the much smaller LeaderWorkerSet watch.
const laggingPodNamespacePrefix = "lagging-pods-"

// podInformerLag is the manager's Pod cache transform. It delays every Pod
// event of the lagging namespaces.
func podInformerLag(obj interface{}) (interface{}, error) {
	if m, ok := obj.(metav1.Object); ok && strings.HasPrefix(m.GetNamespace(), laggingPodNamespacePrefix) {
		time.Sleep(100 * time.Millisecond)
	}
	return obj, nil
}

var _ = ginkgo.Describe("Group restart budget", func() {
	ginkgo.It("charges one group failure once when several workers restart together", func() {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: laggingPodNamespacePrefix}}
		gomega.Expect(k8sClient.Create(ctx, ns)).To(gomega.Succeed())
		const size = 8
		lws := wrappers.BuildLeaderWorkerSet(ns.Name).Replica(1).Size(size).
			RestartPolicy(leaderworkerset.RecreateGroupOnPodRestart).MaxGroupRestarts(3).Obj()
		gomega.Expect(k8sClient.Create(ctx, lws)).To(gomega.Succeed())

		var leaderSts appsv1.StatefulSet
		gomega.Eventually(func() error {
			return k8sClient.Get(ctx, types.NamespacedName{Name: lws.Name, Namespace: lws.Namespace}, &leaderSts)
		}, testing.Timeout, testing.Interval).Should(gomega.Succeed())
		gomega.Expect(testing.CreateLeaderPods(ctx, leaderSts, k8sClient, lws, 0, 1)).To(gomega.Succeed())
		var leader corev1.Pod
		gomega.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: lws.Name + "-0", Namespace: lws.Namespace}, &leader)).To(gomega.Succeed())
		testing.CreateWorkerPodsForLeaderPod(ctx, leader, k8sClient, *lws)

		var workers corev1.PodList
		gomega.Expect(k8sClient.List(ctx, &workers, client.InNamespace(lws.Namespace), client.MatchingLabels{"worker.pod": "workers"})).To(gomega.Succeed())
		gomega.Expect(workers.Items).To(gomega.HaveLen(size - 1))

		ginkgo.By("restarting every worker at once, as a collective error would")
		var wg sync.WaitGroup
		for i := range workers.Items {
			worker := workers.Items[i]
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer ginkgo.GinkgoRecover()
				worker.Status.Phase = corev1.PodRunning
				worker.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "worker", RestartCount: 1}}
				gomega.Expect(k8sClient.Status().Update(ctx, &worker)).To(gomega.Succeed())
			}()
		}
		wg.Wait()

		gomega.Eventually(func(g gomega.Gomega) {
			var pod corev1.Pod
			g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(&leader), &pod)).To(gomega.Succeed())
			g.Expect(pod.DeletionTimestamp).NotTo(gomega.BeNil())
		}, testing.Timeout, testing.Interval).Should(gomega.Succeed())

		ginkgo.By("checking that the failure charged the budget once")
		countKey := leader.Labels[leaderworkerset.RevisionKey] + "/0"
		// Every worker event reaches the lagging cache within a second.
		gomega.Consistently(func(g gomega.Gomega) {
			var current leaderworkerset.LeaderWorkerSet
			g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(lws), &current)).To(gomega.Succeed())
			var counts map[string]int32
			g.Expect(json.Unmarshal([]byte(current.Annotations[leaderworkerset.GroupRestartCountsAnnotationKey]), &counts)).To(gomega.Succeed())
			g.Expect(counts).To(gomega.Equal(map[string]int32{countKey: 1}))
		}, 2*time.Second, testing.Interval).Should(gomega.Succeed())
	})
})

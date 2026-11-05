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

package managerrestart

import (
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
	testing "sigs.k8s.io/lws/test/testutils"
	"sigs.k8s.io/lws/test/wrappers"
)

// recreateTimeout bounds how long a running controller may take to start
// recreating the group. A healthy controller does so within a few seconds.
const recreateTimeout = 30 * time.Second

// createGroup creates a one-group LWS with RecreateGroupOnPodRestart and its
// leader and worker Pods (envtest runs no StatefulSet controller). It returns
// the leader as created.
func createGroup(prefix string) (*leaderworkerset.LeaderWorkerSet, corev1.Pod) {
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: prefix}}
	gomega.Expect(k8sClient.Create(ctx, ns)).To(gomega.Succeed())
	lws := wrappers.BuildLeaderWorkerSet(ns.Name).Replica(1).Size(2).
		RestartPolicy(leaderworkerset.RecreateGroupOnPodRestart).Obj()
	gomega.Expect(k8sClient.Create(ctx, lws)).To(gomega.Succeed())

	var leaderSts appsv1.StatefulSet
	gomega.Eventually(func() error {
		return k8sClient.Get(ctx, client.ObjectKeyFromObject(lws), &leaderSts)
	}, testing.Timeout, testing.Interval).Should(gomega.Succeed())
	gomega.Expect(testing.CreateLeaderPods(ctx, leaderSts, k8sClient, lws, 0, 1)).To(gomega.Succeed())
	var leader corev1.Pod
	gomega.Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns.Name, Name: lws.Name + "-0"}, &leader)).To(gomega.Succeed())
	testing.CreateWorkerPodsForLeaderPod(ctx, leader, k8sClient, *lws)
	return lws, leader
}

// deleteWorker deletes worker 1 of the group and waits until the API server
// has accepted the deletion: the Pod is gone, or terminating if a finalizer
// holds it. The Pod has no node, so without a finalizer it is removed at
// once, as it would be after a worker's graceful termination ends or on a
// force delete.
func deleteWorker(leader corev1.Pod) {
	worker := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: leader.Namespace, Name: leader.Name + "-1"}}
	gomega.Expect(k8sClient.Delete(ctx, worker)).To(gomega.Succeed())
	gomega.Eventually(func(g gomega.Gomega) {
		var current corev1.Pod
		err := k8sClient.Get(ctx, client.ObjectKeyFromObject(worker), &current)
		if apierrors.IsNotFound(err) {
			return
		}
		g.Expect(err).NotTo(gomega.HaveOccurred())
		g.Expect(current.DeletionTimestamp).NotTo(gomega.BeNil())
	}, testing.Timeout, testing.Interval).Should(gomega.Succeed())
}

// replaceWorkerIfGone recreates worker 1 with the same name and a new UID,
// as its StatefulSet would, if the deleted Pod is already gone. A Pod that is
// still terminating keeps its name, so no replacement can be created yet.
func replaceWorkerIfGone(lws *leaderworkerset.LeaderWorkerSet, leader corev1.Pod) {
	var current corev1.Pod
	err := k8sClient.Get(ctx, types.NamespacedName{Namespace: leader.Namespace, Name: leader.Name + "-1"}, &current)
	if !apierrors.IsNotFound(err) {
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		return
	}
	testing.CreateWorkerPodsForLeaderPod(ctx, leader, k8sClient, *lws)
}

// expectGroupRecreation waits for the controller to start recreating the
// group: the original leader is deleted (envtest has no garbage collector,
// so it stays terminating) or already replaced.
func expectGroupRecreation(leader corev1.Pod) {
	gomega.Eventually(func(g gomega.Gomega) {
		var current corev1.Pod
		err := k8sClient.Get(ctx, client.ObjectKeyFromObject(&leader), &current)
		if client.IgnoreNotFound(err) == nil && err != nil {
			return
		}
		g.Expect(err).NotTo(gomega.HaveOccurred())
		if current.UID != leader.UID {
			return
		}
		g.Expect(current.DeletionTimestamp).NotTo(gomega.BeNil(),
			"leader %s was not deleted after its worker was deleted", leader.Name)
	}, recreateTimeout, testing.Interval).Should(gomega.Succeed())
}

var _ = ginkgo.Describe("Worker deletion across a controller manager restart", func() {
	ginkgo.It("recreates the group when a worker is deleted while the manager runs", func() {
		startManager()
		_, leader := createGroup("worker-delete-running-")
		deleteWorker(leader)
		expectGroupRecreation(leader)
	})

	// Control: a restarted manager handles deletions it observes. This shows
	// that the specs below exercise a working replacement manager.
	ginkgo.It("recreates the group when a worker is deleted after the manager restarted", func() {
		stop := startManager()
		_, leader := createGroup("worker-delete-restarted-")
		stop()
		startManager()
		deleteWorker(leader)
		expectGroupRecreation(leader)
	})

	ginkgo.It("recreates the group when a worker was deleted while the manager was down", func() {
		stop := startManager()
		_, leader := createGroup("worker-delete-down-")
		// Let the running manager observe the healthy group before it stops.
		gomega.Consistently(func(g gomega.Gomega) {
			var current corev1.Pod
			g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(&leader), &current)).To(gomega.Succeed())
			g.Expect(current.DeletionTimestamp).To(gomega.BeNil())
		}, 2*time.Second, testing.Interval).Should(gomega.Succeed())
		stop()

		deleteWorker(leader)
		startManager()
		expectGroupRecreation(leader)
	})

	ginkgo.It("recreates the group when a worker was replaced while the manager was down", func() {
		stop := startManager()
		lws, leader := createGroup("worker-replace-down-")
		gomega.Consistently(func(g gomega.Gomega) {
			var current corev1.Pod
			g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(&leader), &current)).To(gomega.Succeed())
			g.Expect(current.DeletionTimestamp).To(gomega.BeNil())
		}, 2*time.Second, testing.Interval).Should(gomega.Succeed())
		stop()

		deleteWorker(leader)
		// A worker StatefulSet recreates a removed Pod with the same name and a
		// new UID, and the replacement has no restarts or deletion timestamp.
		replaceWorkerIfGone(lws, leader)
		startManager()
		expectGroupRecreation(leader)
	})
})

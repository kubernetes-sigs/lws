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
	"crypto/sha256"
	"fmt"
	"slices"
	"strconv"
	"strings"
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
	revisionutils "sigs.k8s.io/lws/pkg/utils/revision"
	testing "sigs.k8s.io/lws/test/testutils"
	"sigs.k8s.io/lws/test/wrappers"
)

type testGroup struct {
	lws       *leaderworkerset.LeaderWorkerSet
	leader    corev1.Pod
	workerSet appsv1.StatefulSet
	workers   []corev1.Pod
}

// createGroup emulates both StatefulSet controllers. The workers are owned
// by the worker StatefulSet, just as they are in a cluster.
func createGroup(prefix string, policy leaderworkerset.RestartPolicyType, size int) testGroup {
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: prefix}}
	gomega.Expect(k8sClient.Create(ctx, ns)).To(gomega.Succeed())
	lws := wrappers.BuildLeaderWorkerSet(ns.Name).Replica(1).Size(size).RestartPolicy(policy).Obj()
	gomega.Expect(k8sClient.Create(ctx, lws)).To(gomega.Succeed())

	var leaderSts appsv1.StatefulSet
	gomega.Eventually(func() error {
		return k8sClient.Get(ctx, client.ObjectKeyFromObject(lws), &leaderSts)
	}, testing.Timeout, testing.Interval).Should(gomega.Succeed())
	gomega.Expect(testing.CreateLeaderPods(ctx, leaderSts, k8sClient, lws, 0, 1)).To(gomega.Succeed())

	group := testGroup{lws: lws}
	key := types.NamespacedName{Namespace: ns.Name, Name: lws.Name + "-0"}
	gomega.Expect(k8sClient.Get(ctx, key, &group.leader)).To(gomega.Succeed())
	gomega.Eventually(func() error {
		return k8sClient.Get(ctx, key, &group.workerSet)
	}, testing.Timeout, testing.Interval).Should(gomega.Succeed())
	for ordinal := 1; ordinal < size; ordinal++ {
		group.workers = append(group.workers, createWorker(group.workerSet, ordinal))
	}
	return group
}

func createWorker(sts appsv1.StatefulSet, ordinal int) corev1.Pod {
	worker := corev1.Pod{
		ObjectMeta: *sts.Spec.Template.ObjectMeta.DeepCopy(),
		Spec:       *sts.Spec.Template.Spec.DeepCopy(),
	}
	worker.Name = sts.Name + "-" + strconv.Itoa(ordinal)
	worker.Namespace = sts.Namespace
	worker.Labels[leaderworkerset.WorkerIndexLabelKey] = strconv.Itoa(ordinal)
	worker.OwnerReferences = []metav1.OwnerReference{*metav1.NewControllerRef(&sts, appsv1.SchemeGroupVersion.WithKind("StatefulSet"))}
	gomega.Expect(k8sClient.Create(ctx, &worker)).To(gomega.Succeed())
	return worker
}

// deleteWorker waits until the Pod is absent, as after its termination grace
// period. This ensures a restart cannot recover by observing a terminating Pod.
func deleteWorker(worker corev1.Pod) {
	gomega.Expect(k8sClient.Delete(ctx, &worker)).To(gomega.Succeed())
	gomega.Eventually(func() bool {
		return apierrors.IsNotFound(k8sClient.Get(ctx, client.ObjectKeyFromObject(&worker), &corev1.Pod{}))
	}, testing.Timeout, testing.Interval).Should(gomega.BeTrue())
}

func expectWorkerRecord(group testGroup, started bool) {
	uids := make([]string, len(group.workers))
	for i := range group.workers {
		uids[i] = string(group.workers[i].UID)
	}
	slices.Sort(uids)
	sum := sha256.Sum256([]byte(strings.Join(uids, ",")))
	want := fmt.Sprintf("%s|sha256:%x:%d", revisionutils.GetRevisionKey(&group.leader), sum, len(uids))
	if started {
		want += ":started"
	}
	gomega.Eventually(func(g gomega.Gomega) {
		var current corev1.Pod
		g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(&group.leader), &current)).To(gomega.Succeed())
		g.Expect(current.Annotations[leaderworkerset.WorkerUIDsAnnotationKey]).To(gomega.Equal(want))
	}, testing.Timeout, testing.Interval).Should(gomega.Succeed())
}

func expectGroupRecreation(leader corev1.Pod) {
	gomega.Eventually(func(g gomega.Gomega) {
		var current corev1.Pod
		err := k8sClient.Get(ctx, client.ObjectKeyFromObject(&leader), &current)
		if apierrors.IsNotFound(err) {
			return
		}
		g.Expect(err).NotTo(gomega.HaveOccurred())
		if current.UID != leader.UID {
			return
		}
		// Envtest has no garbage collector, so the original leader can
		// stay terminating until its dependents have been removed.
		g.Expect(current.DeletionTimestamp).NotTo(gomega.BeNil())
	}, 30*time.Second, testing.Interval).Should(gomega.Succeed())
}

func expectLeaderKept(leader corev1.Pod) {
	gomega.Consistently(func(g gomega.Gomega) {
		var current corev1.Pod
		g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(&leader), &current)).To(gomega.Succeed())
		g.Expect(current.UID).To(gomega.Equal(leader.UID))
		g.Expect(current.DeletionTimestamp).To(gomega.BeNil())
	}, 3*time.Second, testing.Interval).Should(gomega.Succeed())
}

var _ = ginkgo.Describe("Worker deletion across a controller manager restart", func() {
	ginkgo.It("recreates the group when a worker is deleted while the manager runs", func() {
		startManager()
		group := createGroup("worker-delete-running-", leaderworkerset.RecreateGroupOnPodRestart, 2)
		deleteWorker(group.workers[0])
		expectGroupRecreation(group.leader)
	})

	ginkgo.It("recreates the group when a worker is deleted after the manager restarted", func() {
		stop := startManager()
		group := createGroup("worker-delete-restarted-", leaderworkerset.RecreateGroupOnPodRestart, 2)
		stop()
		startManager()
		deleteWorker(group.workers[0])
		expectGroupRecreation(group.leader)
	})

	ginkgo.It("recreates the group when a worker was deleted while the manager was down", func() {
		stop := startManager()
		group := createGroup("worker-delete-down-", leaderworkerset.RecreateGroupOnPodRestart, 2)
		expectWorkerRecord(group, false)
		stop()

		deleteWorker(group.workers[0])
		startManager()
		expectGroupRecreation(group.leader)
	})

	ginkgo.It("recreates the group when a worker was replaced while the manager was down", func() {
		stop := startManager()
		group := createGroup("worker-replace-down-", leaderworkerset.RecreateGroupOnPodRestart, 2)
		expectWorkerRecord(group, false)
		stop()

		deleteWorker(group.workers[0])
		replacement := createWorker(group.workerSet, 1)
		gomega.Expect(replacement.Name).To(gomega.Equal(group.workers[0].Name))
		gomega.Expect(replacement.UID).NotTo(gomega.Equal(group.workers[0].UID))
		startManager()
		expectGroupRecreation(group.leader)
	})

	ginkgo.It("keeps an AfterStart group whose Pending worker was deleted while the manager was down", func() {
		stop := startManager()
		group := createGroup("pending-worker-down-", leaderworkerset.RecreateGroupAfterStart, 2)
		testing.SetPodToRunning(ctx, k8sClient, group.leader.Name, group.lws)
		gomega.Expect(group.workers[0].Status.Phase).To(gomega.Equal(corev1.PodPending))
		gomega.Consistently(func(g gomega.Gomega) {
			var current corev1.Pod
			g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(&group.leader), &current)).To(gomega.Succeed())
			g.Expect(current.Annotations[leaderworkerset.WorkerUIDsAnnotationKey]).To(gomega.BeEmpty())
		}, time.Second, testing.Interval).Should(gomega.Succeed())
		stop()

		deleteWorker(group.workers[0])
		startManager()
		expectLeaderKept(group.leader)
	})

	ginkgo.DescribeTable("recreates an AfterStart group that lost running workers while the manager was down", func(lostWorkers int) {
		stop := startManager()
		group := createGroup("running-workers-down-", leaderworkerset.RecreateGroupAfterStart, 3)
		testing.SetPodToRunning(ctx, k8sClient, group.leader.Name, group.lws)
		for _, worker := range group.workers {
			testing.SetPodToRunning(ctx, k8sClient, worker.Name, group.lws)
		}
		expectWorkerRecord(group, true)
		stop()

		for _, worker := range group.workers[:lostWorkers] {
			deleteWorker(worker)
		}
		startManager()
		expectGroupRecreation(group.leader)
	},
		ginkgo.Entry("with one worker missing and another still running", 1),
		ginkgo.Entry("with both workers missing", 2),
	)

	ginkgo.It("recreates a started AfterStart group with an unschedulable replacement worker", func() {
		stop := startManager()
		group := createGroup("pending-replacement-", leaderworkerset.RecreateGroupAfterStart, 2)
		testing.SetPodToRunning(ctx, k8sClient, group.leader.Name, group.lws)
		testing.SetPodToRunning(ctx, k8sClient, group.workers[0].Name, group.lws)
		expectWorkerRecord(group, true)
		stop()

		deleteWorker(group.workers[0])
		replacement := createWorker(group.workerSet, 1)
		gomega.Expect(replacement.UID).NotTo(gomega.Equal(group.workers[0].UID))
		gomega.Expect(replacement.Status.Phase).To(gomega.Equal(corev1.PodPending))
		replacement.Status.Conditions = []corev1.PodCondition{{
			Type:   corev1.PodScheduled,
			Status: corev1.ConditionFalse,
			Reason: corev1.PodReasonUnschedulable,
		}}
		gomega.Expect(k8sClient.Status().Update(ctx, &replacement)).To(gomega.Succeed())

		startManager()
		expectGroupRecreation(group.leader)
	})
})

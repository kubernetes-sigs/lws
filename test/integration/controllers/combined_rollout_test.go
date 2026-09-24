/*
Copyright 2026.

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
	"fmt"
	"strconv"
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	leaderworkerset "sigs.k8s.io/lws/api/leaderworkerset/v1"
	"sigs.k8s.io/lws/pkg/utils"
	testing "sigs.k8s.io/lws/test/testutils"
)

type combinedRolloutOptions struct {
	duringUpdate       bool
	afterPartialUpdate bool
	downscale          bool
}

// These growth scenarios run the real asynchronous LWS controller, not
// kube-controller-manager. Explicitly simulate native acknowledgement, Pod
// creation, whole-group kubelet readiness and GC. Native failure modes also
// have separate real-apiserver and exhaustive planner tests in pkg/controllers.
func testCombinedScaleSurge(lws *leaderworkerset.LeaderWorkerSet, options combinedRolloutOptions) {
	const annotation = "leaderworkerset.sigs.k8s.io/combined-rollout"
	key := client.ObjectKeyFromObject(lws)
	baseline := *lws.Spec.Replicas
	desired := baseline + 2
	surge := int32(lws.Spec.RolloutStrategy.RollingUpdateConfiguration.MaxSurge.IntValue())
	size := *lws.Spec.LeaderWorkerTemplate.Size
	groupKey := func(i int32) client.ObjectKey {
		return client.ObjectKey{Namespace: key.Namespace, Name: fmt.Sprintf("%s-%d", key.Name, i)}
	}
	getLeader := func(i int32) *corev1.Pod {
		var pod corev1.Pod
		gomega.Expect(k8sClient.Get(ctx, groupKey(i), &pod)).To(gomega.Succeed())
		return &pod
	}
	getSTS := func() *appsv1.StatefulSet {
		var sts appsv1.StatefulSet
		gomega.Expect(k8sClient.Get(ctx, key, &sts)).To(gomega.Succeed())
		return &sts
	}
	// Bounded Eventually only acknowledges concrete observed spec generations;
	// it never fabricates healthy Pods or silently completes a rollout.
	advance := func(predicate func(*appsv1.StatefulSet) bool) {
		gomega.Eventually(func() (bool, error) {
			var sts appsv1.StatefulSet
			if err := k8sClient.Get(ctx, key, &sts); err != nil {
				return false, err
			}
			native := "native-" + sts.Spec.Template.Labels[leaderworkerset.RevisionKey]
			if sts.Status.ObservedGeneration < sts.Generation || sts.Status.UpdateRevision != native || sts.Status.Replicas != *sts.Spec.Replicas {
				sts.Status.ObservedGeneration, sts.Status.UpdateRevision, sts.Status.Replicas = sts.Generation, native, *sts.Spec.Replicas
				if sts.Status.CurrentRevision == "" {
					sts.Status.CurrentRevision = native
				}
				if err := k8sClient.Status().Update(ctx, &sts); err != nil {
					return false, err
				}
			}
			return predicate(&sts), nil
		}, testing.Timeout, testing.Interval).Should(gomega.BeTrue())
	}
	change := func(mutate func(*leaderworkerset.LeaderWorkerSet)) {
		gomega.Expect(retry.RetryOnConflict(retry.DefaultRetry, func() error {
			var live leaderworkerset.LeaderWorkerSet
			if err := k8sClient.Get(ctx, key, &live); err != nil {
				return err
			}
			mutate(&live)
			return k8sClient.Update(ctx, &live)
		})).To(gomega.Succeed())
	}
	// Idempotent readiness: an existing worker must already have the right
	// owner and revision. Never relabel an old Pod to manufacture an update.
	ready := func(i int32, workersReady bool) {
		podKey := groupKey(i)
		gomega.Expect(retry.RetryOnConflict(retry.DefaultRetry, func() error {
			var pod corev1.Pod
			if err := k8sClient.Get(ctx, podKey, &pod); err != nil {
				return err
			}
			pod.Status.Phase = corev1.PodRunning
			pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
			return k8sClient.Status().Update(ctx, &pod)
		})).To(gomega.Succeed())
		if size > 1 {
			leader := getLeader(i)
			var workers appsv1.StatefulSet
			gomega.Eventually(func(g gomega.Gomega) {
				g.Expect(k8sClient.Get(ctx, podKey, &workers)).To(gomega.Succeed())
				g.Expect(workers.DeletionTimestamp).To(gomega.BeNil())
				g.Expect(metav1.GetControllerOf(&workers)).NotTo(gomega.BeNil())
				g.Expect(metav1.GetControllerOf(&workers).UID).To(gomega.Equal(leader.UID))
				g.Expect(workers.Labels[leaderworkerset.RevisionKey]).To(gomega.Equal(leader.Labels[leaderworkerset.RevisionKey]))
				g.Expect(*workers.Spec.Replicas).To(gomega.Equal(size - 1))
			}, testing.Timeout, testing.Interval).Should(gomega.Succeed())
			native := "native-" + workers.Labels[leaderworkerset.RevisionKey]
			for j := int32(1); j <= *workers.Spec.Replicas; j++ {
				workerKey := client.ObjectKey{Namespace: key.Namespace, Name: fmt.Sprintf("%s-%d", workers.Name, j)}
				var p corev1.Pod
				err := k8sClient.Get(ctx, workerKey, &p)
				gomega.Expect(client.IgnoreNotFound(err)).To(gomega.Succeed())
				if apierrors.IsNotFound(err) {
					p = corev1.Pod{ObjectMeta: *workers.Spec.Template.ObjectMeta.DeepCopy(), Spec: *workers.Spec.Template.Spec.DeepCopy()}
					p.Name, p.Namespace = workerKey.Name, workerKey.Namespace
					p.Labels[leaderworkerset.WorkerIndexLabelKey] = strconv.Itoa(int(j))
					p.Labels[appsv1.ControllerRevisionHashLabelKey] = native
					p.OwnerReferences = []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "StatefulSet", Name: workers.Name, UID: workers.UID, Controller: ptr.To(true)}}
					p.Spec.Hostname, p.Spec.Subdomain = p.Name, workers.Spec.ServiceName
					gomega.Expect(k8sClient.Create(ctx, &p)).To(gomega.Succeed())
				}
				gomega.Expect(metav1.GetControllerOf(&p)).NotTo(gomega.BeNil())
				gomega.Expect(metav1.GetControllerOf(&p).UID).To(gomega.Equal(workers.UID))
				gomega.Expect(p.Labels[leaderworkerset.RevisionKey]).To(gomega.Equal(leader.Labels[leaderworkerset.RevisionKey]))
				gomega.Expect(p.Labels[appsv1.ControllerRevisionHashLabelKey]).To(gomega.Equal(native))
				gomega.Expect(p.DeletionTimestamp).To(gomega.BeNil())
				if workersReady {
					gomega.Expect(retry.RetryOnConflict(retry.DefaultRetry, func() error {
						if err := k8sClient.Get(ctx, workerKey, &p); err != nil {
							return err
						}
						p.Status.Phase = corev1.PodRunning
						p.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
						return k8sClient.Status().Update(ctx, &p)
					})).To(gomega.Succeed())
				}
			}
			gomega.Expect(retry.RetryOnConflict(retry.DefaultRetry, func() error {
				if err := k8sClient.Get(ctx, podKey, &workers); err != nil {
					return err
				}
				workers.Status.ObservedGeneration = workers.Generation
				workers.Status.Replicas, workers.Status.UpdatedReplicas = *workers.Spec.Replicas, *workers.Spec.Replicas
				workers.Status.ReadyReplicas, workers.Status.AvailableReplicas = 0, 0
				if workersReady {
					workers.Status.ReadyReplicas, workers.Status.AvailableReplicas = *workers.Spec.Replicas, *workers.Spec.Replicas
				}
				workers.Status.CurrentRevision, workers.Status.UpdateRevision = native, native
				return k8sClient.Status().Update(ctx, &workers)
			})).To(gomega.Succeed())
		}
	}
	create := func(i int32) {
		sts := getSTS()
		pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("%s-%d", key.Name, i), Namespace: key.Namespace,
			Labels:          map[string]string{leaderworkerset.SetNameLabelKey: key.Name, leaderworkerset.GroupIndexLabelKey: strconv.Itoa(int(i)), leaderworkerset.WorkerIndexLabelKey: "0", leaderworkerset.RevisionKey: sts.Spec.Template.Labels[leaderworkerset.RevisionKey], appsv1.ControllerRevisionHashLabelKey: "native-" + sts.Spec.Template.Labels[leaderworkerset.RevisionKey]},
			Annotations:     map[string]string{leaderworkerset.SizeAnnotationKey: strconv.Itoa(int(*lws.Spec.LeaderWorkerTemplate.Size))},
			OwnerReferences: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "StatefulSet", Name: sts.Name, UID: sts.UID, Controller: ptr.To(true)}}},
			Spec: *sts.Spec.Template.Spec.DeepCopy()}
		// envtest runs neither the native StatefulSet controller nor Pod
		// admission. Supply the DNS identity they assign before workers start.
		pod.Labels[leaderworkerset.GroupUniqueHashLabelKey] = utils.Sha1Hash(fmt.Sprintf("%s/%s", pod.Namespace, pod.Name))
		pod.Spec.Hostname = pod.Name
		pod.Spec.Subdomain = sts.Spec.ServiceName
		if lws.Spec.NetworkConfig != nil && ptr.Deref(lws.Spec.NetworkConfig.SubdomainPolicy, leaderworkerset.SubdomainShared) == leaderworkerset.SubdomainUniquePerReplica {
			pod.Spec.Subdomain = pod.Name
		}
		gomega.Expect(k8sClient.Create(ctx, pod)).To(gomega.Succeed())
	}
	remove := func(i int32) {
		podKey := groupKey(i)
		leader := getLeader(i)
		// Mark the leader terminating BEFORE removing dependents. The default
		// RecreateGroupOnPodRestart policy must not interpret simulated GC as
		// an independent worker failure, or recreate workers for a live leader.
		gomega.Expect(k8sClient.Delete(ctx, leader, &client.DeleteOptions{
			Preconditions:     &metav1.Preconditions{UID: &leader.UID},
			PropagationPolicy: ptr.To(metav1.DeletePropagationForeground), GracePeriodSeconds: ptr.To[int64](0),
		})).To(gomega.Succeed())
		if size > 1 {
			var workers appsv1.StatefulSet
			err := k8sClient.Get(ctx, podKey, &workers)
			gomega.Expect(client.IgnoreNotFound(err)).To(gomega.Succeed())
			if err == nil {
				gomega.Expect(metav1.GetControllerOf(&workers)).NotTo(gomega.BeNil())
				gomega.Expect(metav1.GetControllerOf(&workers).UID).To(gomega.Equal(leader.UID))
				gomega.Expect(k8sClient.Delete(ctx, &workers, &client.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &workers.UID}})).To(gomega.Succeed())
			}
			var pods corev1.PodList
			gomega.Expect(k8sClient.List(ctx, &pods, client.InNamespace(key.Namespace), client.MatchingLabels{leaderworkerset.SetNameLabelKey: key.Name, leaderworkerset.GroupIndexLabelKey: strconv.Itoa(int(i))})).To(gomega.Succeed())
			for j := range pods.Items {
				if pods.Items[j].Name != podKey.Name {
					p := &pods.Items[j]
					gomega.Expect(metav1.GetControllerOf(p)).NotTo(gomega.BeNil())
					gomega.Expect(metav1.GetControllerOf(p).UID).To(gomega.Equal(workers.UID))
					gomega.Expect(k8sClient.Delete(ctx, p, &client.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &p.UID}, GracePeriodSeconds: ptr.To[int64](0)})).To(gomega.Succeed())
					gomega.Eventually(func() bool {
						return apierrors.IsNotFound(k8sClient.Get(ctx, client.ObjectKeyFromObject(p), &corev1.Pod{}))
					}, testing.Timeout, testing.Interval).Should(gomega.BeTrue())
				}
			}
			gomega.Eventually(func() bool { return apierrors.IsNotFound(k8sClient.Get(ctx, podKey, &appsv1.StatefulSet{})) }, testing.Timeout, testing.Interval).Should(gomega.BeTrue())
		}
		gomega.Expect(retry.RetryOnConflict(retry.DefaultRetry, func() error {
			var pod corev1.Pod
			if err := k8sClient.Get(ctx, podKey, &pod); err != nil {
				return client.IgnoreNotFound(err)
			}
			gomega.Expect(pod.UID).To(gomega.Equal(leader.UID))
			// envtest lacks GC; release the controller's foreground finalizer.
			if pod.DeletionTimestamp != nil && len(pod.Finalizers) > 0 {
				pod.Finalizers = nil
				if err := k8sClient.Update(ctx, &pod); err != nil {
					return client.IgnoreNotFound(err)
				}
			}
			return client.IgnoreNotFound(k8sClient.Delete(ctx, &pod, &client.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &leader.UID}, GracePeriodSeconds: ptr.To[int64](0)}))
		})).To(gomega.Succeed())
		gomega.Eventually(func() bool { return apierrors.IsNotFound(k8sClient.Get(ctx, podKey, &corev1.Pod{})) }, testing.Timeout, testing.Interval).Should(gomega.BeTrue())
	}
	// Assert one stable observation, including the legacy informational status.
	// Terminating leaders still count there until their worker set is removed;
	// the combined budget deliberately does NOT count them as available credit.
	rolloutStarted := false
	stage := func(replicas, partition, readyCount, updatedCount int32, updating bool) {
		ginkgo.By(fmt.Sprintf("checking stable stage: replicas=%d partition=%d ready=%d updated=%d updating=%t", replicas, partition, readyCount, updatedCount, updating))
		check := func(g gomega.Gomega) {
			var sts appsv1.StatefulSet
			var live leaderworkerset.LeaderWorkerSet
			g.Expect(k8sClient.Get(ctx, key, &sts)).To(gomega.Succeed())
			g.Expect(k8sClient.Get(ctx, key, &live)).To(gomega.Succeed())
			g.Expect(*sts.Spec.Replicas).To(gomega.Equal(replicas))
			g.Expect(*sts.Spec.UpdateStrategy.RollingUpdate.Partition).To(gomega.Equal(partition))
			g.Expect(sts.Status.ObservedGeneration).To(gomega.Equal(sts.Generation))
			g.Expect(live.Status.Replicas).To(gomega.Equal(replicas))
			g.Expect(live.Status.ReadyReplicas).To(gomega.Equal(readyCount))
			g.Expect(live.Status.UpdatedReplicas).To(gomega.Equal(updatedCount))
			for _, condition := range []leaderworkerset.LeaderWorkerSetConditionType{leaderworkerset.LeaderWorkerSetProgressing, leaderworkerset.LeaderWorkerSetUpdateInProgress} {
				g.Expect(meta.IsStatusConditionTrue(live.Status.Conditions, string(condition))).To(gomega.Equal(updating))
				// UpdateInProgress is absent before the first rollout, not False.
				// Once a rollout starts, require explicit completion conditions.
				if rolloutStarted || condition != leaderworkerset.LeaderWorkerSetUpdateInProgress {
					g.Expect(meta.IsStatusConditionFalse(live.Status.Conditions, string(condition))).To(gomega.Equal(!updating))
				}
			}
			g.Expect(meta.IsStatusConditionTrue(live.Status.Conditions, string(leaderworkerset.LeaderWorkerSetAvailable))).To(gomega.Equal(!updating))
		}
		gomega.Eventually(check, testing.Timeout, testing.Interval).Should(gomega.Succeed())
		gomega.Consistently(check, 2*time.Second, testing.Interval).Should(gomega.Succeed())
	}
	oldRevision := getSTS().Labels[leaderworkerset.RevisionKey]
	oldUIDs := make(map[int32]types.UID)
	for i := int32(0); i < baseline; i++ {
		podKey := client.ObjectKey{Namespace: key.Namespace, Name: fmt.Sprintf("%s-%d", key.Name, i)}
		var pod corev1.Pod
		gomega.Expect(k8sClient.Get(ctx, podKey, &pod)).To(gomega.Succeed())
		pod.Labels[appsv1.ControllerRevisionHashLabelKey] = "native-" + oldRevision
		gomega.Expect(k8sClient.Update(ctx, &pod)).To(gomega.Succeed())
		oldUIDs[i] = pod.UID
		ready(i, true)
	}
	advance(func(*appsv1.StatefulSet) bool { return true })
	stage(baseline, 0, baseline, baseline, false)
	testing.ExpectValidWorkerStatefulSets(ctx, lws, k8sClient, true)
	first := baseline - 1
	partial := int32(0)
	retained := make(map[string]corev1.Pod)
	checkRetained := func(g gomega.Gomega) {
		for name, original := range retained {
			var pod corev1.Pod
			g.Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: key.Namespace, Name: name}, &pod)).To(gomega.Succeed())
			g.Expect(pod.UID).To(gomega.Equal(original.UID))
			g.Expect(pod.Labels[leaderworkerset.RevisionKey]).To(gomega.Equal(original.Labels[leaderworkerset.RevisionKey]))
			g.Expect(pod.Labels[appsv1.ControllerRevisionHashLabelKey]).To(gomega.Equal(original.Labels[appsv1.ControllerRevisionHashLabelKey]))
			g.Expect(pod.OwnerReferences).To(gomega.Equal(original.OwnerReferences))
			g.Expect(pod.DeletionTimestamp).To(gomega.BeNil())
		}
	}
	ginkgo.By("publishing growth concurrently with a template update, or during an ordinary surge rollout")
	change(func(live *leaderworkerset.LeaderWorkerSet) {
		live.Spec.LeaderWorkerTemplate.WorkerTemplate.Spec.Containers[0].Image = "test.invalid/combined-update"
		if !options.duringUpdate {
			live.Spec.Replicas = ptr.To(desired)
		}
	})
	rolloutStarted = true
	if options.duringUpdate {
		advance(func(sts *appsv1.StatefulSet) bool {
			return sts.Labels[leaderworkerset.RevisionKey] != oldRevision && *sts.Spec.Replicas == baseline+surge
		})
		for i := baseline; i < baseline+surge; i++ {
			create(i)
		}
		stage(baseline+surge, baseline-1, baseline, surge, true)
		if options.afterPartialUpdate {
			ginkgo.By("natively replacing the eligible ordinal 3 before publishing growth")
			gomega.Expect(baseline).To(gomega.Equal(int32(4)))
			gomega.Expect(surge).To(gomega.BeZero())
			gomega.Expect(getSTS().Annotations[annotation]).To(gomega.BeEmpty())
			remove(first)
			create(first)
			gomega.Expect(getLeader(first).UID).NotTo(gomega.Equal(oldUIDs[first]))
			ready(first, true)
			first--
			partial = 1
			advance(func(sts *appsv1.StatefulSet) bool { return *sts.Spec.UpdateStrategy.RollingUpdate.Partition == first })
			stage(baseline, first, baseline, partial, true)
			var pods corev1.PodList
			gomega.Expect(k8sClient.List(ctx, &pods, client.InNamespace(key.Namespace), client.MatchingLabels{leaderworkerset.SetNameLabelKey: key.Name, leaderworkerset.GroupIndexLabelKey: "3"})).To(gomega.Succeed())
			gomega.Expect(pods.Items).To(gomega.HaveLen(int(size)))
			for _, pod := range pods.Items {
				gomega.Expect(pod.Labels[leaderworkerset.RevisionKey]).To(gomega.Equal(getSTS().Labels[leaderworkerset.RevisionKey]))
				retained[pod.Name] = pod
			}
		}
		change(func(live *leaderworkerset.LeaderWorkerSet) { live.Spec.Replicas = ptr.To(desired) })
	}
	advance(func(sts *appsv1.StatefulSet) bool {
		var state struct {
			Phase    string                     `json:"phase"`
			Baseline int32                      `json:"b"`
			Pending  map[string]json.RawMessage `json:"pending"`
		}
		if json.Unmarshal([]byte(sts.Annotations[annotation]), &state) != nil {
			return false
		}
		return state.Phase == "active" && state.Baseline == baseline && state.Pending[strconv.Itoa(int(first))] != nil && *sts.Spec.Replicas == desired && *sts.Spec.UpdateStrategy.RollingUpdate.Partition == first
	})
	// Desired additions, rather than an extra surge, supply growth capacity.
	if !options.duringUpdate || surge == 0 {
		for i := baseline; i < desired; i++ {
			create(i)
		}
	}
	gomega.Expect(*getSTS().Spec.Replicas).To(gomega.Equal(desired))
	ginkgo.By("withholding worker readiness: Ready leaders alone cannot pay for another old group")
	for i := baseline; i < desired; i++ {
		ready(i, false)
	}
	advance(func(*appsv1.StatefulSet) bool { return getLeader(first).DeletionTimestamp != nil })
	stage(desired, first, baseline, partial+2, true)
	// Check the exact original UID obligation, not just the partition. Lower
	// old groups remain live and the already-updated group is never replaced.
	type reservation struct {
		UID      types.UID `json:"uid"`
		Revision string    `json:"revision"`
	}
	var held struct {
		Pending map[string]reservation `json:"pending"`
	}
	gomega.Expect(json.Unmarshal([]byte(getSTS().Annotations[annotation]), &held)).To(gomega.Succeed())
	// Reconciliation between native additions can reserve a still-missing
	// growth slot. Such zero-credit obligations also persist until the whole
	// group is Ready. Require exactly one old obligation, validate every extra
	// as a growth-hole obligation, then pin the entire map across both barriers.
	oldReservations := 0
	for ordinal, obligation := range held.Pending {
		i := int32(mustCombinedOrdinal(ordinal))
		gomega.Expect(obligation.Revision).To(gomega.Equal(getSTS().Labels[leaderworkerset.RevisionKey]))
		if i == first {
			oldReservations++
			gomega.Expect(obligation.UID).To(gomega.Equal(oldUIDs[first]))
		} else {
			gomega.Expect(i).To(gomega.BeNumerically(">=", baseline))
			gomega.Expect(i).To(gomega.BeNumerically("<", desired))
			gomega.Expect(obligation.UID).To(gomega.Equal(types.UID("missing")))
		}
	}
	gomega.Expect(oldReservations).To(gomega.Equal(1))
	ginkgo.By(fmt.Sprintf("holding exact reservation map across leader-only readiness: %v", held.Pending))
	barrier := func(g gomega.Gomega) {
		sts := getSTS()
		var state struct {
			Baseline  int32                  `json:"b"`
			Desired   int32                  `json:"d"`
			Protected int32                  `json:"protected"`
			Pending   map[string]reservation `json:"pending"`
		}
		g.Expect(json.Unmarshal([]byte(sts.Annotations[annotation]), &state)).To(gomega.Succeed())
		g.Expect(state.Baseline).To(gomega.Equal(baseline))
		g.Expect(state.Desired).To(gomega.Equal(desired))
		g.Expect(state.Protected).To(gomega.BeZero())
		g.Expect(state.Pending).To(gomega.Equal(held.Pending))
		g.Expect(state.Pending[strconv.Itoa(int(first))].UID).To(gomega.Equal(oldUIDs[first]))
		g.Expect(state.Pending[strconv.Itoa(int(first))].Revision).To(gomega.Equal(sts.Labels[leaderworkerset.RevisionKey]))
		for i := int32(0); i < first; i++ {
			pod := getLeader(i)
			g.Expect(pod.UID).To(gomega.Equal(oldUIDs[i]))
			g.Expect(pod.DeletionTimestamp).To(gomega.BeNil())
		}
		checkRetained(g)
	}
	gomega.Consistently(barrier, 2*time.Second, testing.Interval).Should(gomega.Succeed())
	if options.downscale {
		ginkgo.By("downscaling with an outstanding old-leader reservation")
		change(func(live *leaderworkerset.LeaderWorkerSet) { live.Spec.Replicas = ptr.To[int32](2) })
		advance(func(sts *appsv1.StatefulSet) bool { return *sts.Spec.Replicas == 2 })
		for i := int32(2); i < desired; i++ {
			remove(i)
		}
		desired = 2
	} else {
		// The reserved replacement is also held at leader-only readiness.
		// Its new UID must not refund the old reservation until workers recover.
		remove(first)
		create(first)
		gomega.Expect(getLeader(first).UID).NotTo(gomega.Equal(oldUIDs[first]))
		ready(first, false)
		advance(func(*appsv1.StatefulSet) bool { return true })
		stage(desired, first, baseline-1, partial+3, true)
		gomega.Consistently(barrier, 2*time.Second, testing.Interval).Should(gomega.Succeed())
		for i := baseline; i < desired; i++ {
			ready(i, true)
		}
	}
	// Replace only leaders actually selected for deletion, using NEW UIDs.
	// Skip ordinal 3 in the partial-update case: it is already at the target.
	for i := min(first, desired-1); i >= 0; i-- {
		podKey := client.ObjectKey{Namespace: key.Namespace, Name: fmt.Sprintf("%s-%d", key.Name, i)}
		if options.downscale || i != first {
			advance(func(*appsv1.StatefulSet) bool {
				var pod corev1.Pod
				err := k8sClient.Get(ctx, podKey, &pod)
				return apierrors.IsNotFound(err) || (err == nil && pod.DeletionTimestamp != nil)
			})
			remove(i)
			create(i)
			gomega.Expect(getLeader(i).UID).NotTo(gomega.Equal(oldUIDs[i]))
		}
		if options.downscale && i == desired-1 {
			// Once growth is withdrawn, a Pending replacement can request the
			// configured surge. Simulate those native additions too; missing
			// surge Pods must not be mistaken for available capacity.
			advance(func(sts *appsv1.StatefulSet) bool { return *sts.Spec.Replicas == desired+2 })
			for extra := desired; extra < desired+2; extra++ {
				create(extra)
				ready(extra, false)
			}
			ready(i, false)
			stage(desired+2, i, desired-1, 3, true)
			gomega.Consistently(func(g gomega.Gomega) {
				g.Expect(getLeader(0).UID).To(gomega.Equal(oldUIDs[0]))
				g.Expect(getLeader(0).DeletionTimestamp).To(gomega.BeNil())
			}, 2*time.Second, testing.Interval).Should(gomega.Succeed())
			for extra := desired; extra < desired+2; extra++ {
				ready(extra, true)
			}
		}
		ready(i, true)
	}
	advance(func(sts *appsv1.StatefulSet) bool {
		return *sts.Spec.Replicas == desired && *sts.Spec.UpdateStrategy.RollingUpdate.Partition == 0
	})
	if options.downscale {
		for extra := desired; extra < desired+2; extra++ {
			remove(extra)
		}
	}
	stage(desired, 0, desired, desired, false)
	gomega.Eventually(checkRetained, testing.Timeout, testing.Interval).Should(gomega.Succeed())
	gomega.Expect(getSTS().Annotations[annotation]).NotTo(gomega.BeEmpty(), "must wait for native currentRevision promotion")
	gomega.Expect(retry.RetryOnConflict(retry.DefaultRetry, func() error {
		sts := getSTS()
		sts.Status.CurrentRevision = sts.Status.UpdateRevision
		return k8sClient.Status().Update(ctx, sts)
	})).To(gomega.Succeed())
	advance(func(sts *appsv1.StatefulSet) bool { return sts.Annotations[annotation] == "" })
	gomega.Expect(getSTS().Status.CurrentRevision).To(gomega.Equal(getSTS().Status.UpdateRevision))
	stage(desired, 0, desired, desired, false)
	testing.ExpectValidLeaderStatefulSet(ctx, k8sClient, lws, desired)
	testing.ExpectValidWorkerStatefulSets(ctx, lws, k8sClient, true)
	var pods corev1.PodList
	var sets appsv1.StatefulSetList
	selector := client.MatchingLabels{leaderworkerset.SetNameLabelKey: key.Name}
	gomega.Expect(k8sClient.List(ctx, &pods, client.InNamespace(key.Namespace), selector)).To(gomega.Succeed())
	gomega.Expect(k8sClient.List(ctx, &sets, client.InNamespace(key.Namespace), selector)).To(gomega.Succeed())
	gomega.Expect(pods.Items).To(gomega.HaveLen(int(desired * size)))
	gomega.Expect(sets.Items).To(gomega.HaveLen(int(desired + 1)))
	leaders := 0
	for _, pod := range pods.Items {
		gomega.Expect(pod.DeletionTimestamp).To(gomega.BeNil())
		gomega.Expect(pod.Labels[leaderworkerset.RevisionKey]).To(gomega.Equal(getSTS().Labels[leaderworkerset.RevisionKey]))
		owner := metav1.GetControllerOf(&pod)
		gomega.Expect(owner).NotTo(gomega.BeNil())
		var ownerSet appsv1.StatefulSet
		gomega.Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: key.Namespace, Name: owner.Name}, &ownerSet)).To(gomega.Succeed())
		gomega.Expect(owner.UID).To(gomega.Equal(ownerSet.UID))
		gomega.Expect(pod.Labels[appsv1.ControllerRevisionHashLabelKey]).To(gomega.Equal(ownerSet.Status.UpdateRevision))
		if pod.Labels[leaderworkerset.WorkerIndexLabelKey] == "0" {
			leaders++
		} else {
			leader := getLeader(int32(mustCombinedOrdinal(pod.Labels[leaderworkerset.GroupIndexLabelKey])))
			gomega.Expect(metav1.GetControllerOf(&ownerSet).UID).To(gomega.Equal(leader.UID))
		}
	}
	gomega.Expect(leaders).To(gomega.Equal(int(desired)))
	testing.ExpectLeaderWorkerSetAvailable(ctx, k8sClient, lws, "All replicas are ready")
}

func mustCombinedOrdinal(value string) int {
	i, err := strconv.Atoi(value)
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
	return i
}

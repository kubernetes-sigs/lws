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

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	leaderworkerset "sigs.k8s.io/lws/api/leaderworkerset/v1"
	testing "sigs.k8s.io/lws/test/testutils"
)

// These growth scenarios run the real asynchronous LWS controller, not
// kube-controller-manager. Explicitly simulate native acknowledgement, Pod
// creation, whole-group kubelet readiness and GC. Native failure modes also
// have separate real-apiserver and exhaustive planner tests in pkg/controllers.
func testCombinedScaleSurge(lws *leaderworkerset.LeaderWorkerSet, duringUpdate bool) {
	const annotation = "leaderworkerset.sigs.k8s.io/combined-rollout"
	key := client.ObjectKeyFromObject(lws)
	baseline := *lws.Spec.Replicas
	desired := baseline + 2
	surge := int32(lws.Spec.RolloutStrategy.RollingUpdateConfiguration.MaxSurge.IntValue())
	downscale := duringUpdate && surge > 0
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
	ready := func(i int32) {
		podKey := client.ObjectKey{Namespace: key.Namespace, Name: fmt.Sprintf("%s-%d", key.Name, i)}
		gomega.Expect(retry.RetryOnConflict(retry.DefaultRetry, func() error {
			var pod corev1.Pod
			if err := k8sClient.Get(ctx, podKey, &pod); err != nil {
				return err
			}
			pod.Status.Phase = corev1.PodRunning
			pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
			return k8sClient.Status().Update(ctx, &pod)
		})).To(gomega.Succeed())
		if *lws.Spec.LeaderWorkerTemplate.Size > 1 {
			var workers appsv1.StatefulSet
			gomega.Eventually(func() error { return k8sClient.Get(ctx, podKey, &workers) }, testing.Timeout, testing.Interval).Should(gomega.Succeed())
			native := "native-" + workers.Labels[leaderworkerset.RevisionKey]
			for j := int32(1); j <= *workers.Spec.Replicas; j++ {
				p := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("%s-%d", workers.Name, j), Namespace: key.Namespace,
					Labels:          map[string]string{leaderworkerset.SetNameLabelKey: key.Name, leaderworkerset.GroupIndexLabelKey: strconv.Itoa(int(i)), leaderworkerset.WorkerIndexLabelKey: strconv.Itoa(int(j)), leaderworkerset.RevisionKey: workers.Labels[leaderworkerset.RevisionKey], appsv1.ControllerRevisionHashLabelKey: native},
					OwnerReferences: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "StatefulSet", Name: workers.Name, UID: workers.UID, Controller: ptr.To(true)}}}, Spec: *workers.Spec.Template.Spec.DeepCopy()}
				gomega.Expect(k8sClient.Create(ctx, p)).To(gomega.Succeed())
				p.Status.Phase = corev1.PodRunning
				p.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
				gomega.Expect(k8sClient.Status().Update(ctx, p)).To(gomega.Succeed())
			}
			gomega.Expect(retry.RetryOnConflict(retry.DefaultRetry, func() error {
				if err := k8sClient.Get(ctx, podKey, &workers); err != nil {
					return err
				}
				workers.Status.ObservedGeneration = workers.Generation
				workers.Status.Replicas, workers.Status.ReadyReplicas, workers.Status.AvailableReplicas = *workers.Spec.Replicas, *workers.Spec.Replicas, *workers.Spec.Replicas
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
		pod.Spec.Hostname = pod.Name
		pod.Spec.Subdomain = sts.Spec.ServiceName
		if lws.Spec.NetworkConfig != nil && ptr.Deref(lws.Spec.NetworkConfig.SubdomainPolicy, leaderworkerset.SubdomainShared) == leaderworkerset.SubdomainUniquePerReplica {
			pod.Spec.Subdomain = pod.Name
		}
		gomega.Expect(k8sClient.Create(ctx, pod)).To(gomega.Succeed())
	}
	remove := func(i int32) {
		podKey := client.ObjectKey{Namespace: key.Namespace, Name: fmt.Sprintf("%s-%d", key.Name, i)}
		if *lws.Spec.LeaderWorkerTemplate.Size > 1 {
			var workers appsv1.StatefulSet
			err := k8sClient.Get(ctx, podKey, &workers)
			gomega.Expect(client.IgnoreNotFound(err)).To(gomega.Succeed())
			if err == nil {
				gomega.Expect(k8sClient.Delete(ctx, &workers)).To(gomega.Succeed())
			}
			var pods corev1.PodList
			gomega.Expect(k8sClient.List(ctx, &pods, client.InNamespace(key.Namespace), client.MatchingLabels{leaderworkerset.SetNameLabelKey: key.Name, leaderworkerset.GroupIndexLabelKey: strconv.Itoa(int(i))})).To(gomega.Succeed())
			for j := range pods.Items {
				if pods.Items[j].Name != podKey.Name {
					gomega.Expect(k8sClient.Delete(ctx, &pods.Items[j], client.GracePeriodSeconds(0))).To(gomega.Succeed())
				}
			}
		}
		gomega.Expect(retry.RetryOnConflict(retry.DefaultRetry, func() error {
			var pod corev1.Pod
			if err := k8sClient.Get(ctx, podKey, &pod); err != nil {
				return client.IgnoreNotFound(err)
			}
			// envtest lacks GC; release the controller's foreground finalizer.
			if pod.DeletionTimestamp != nil && len(pod.Finalizers) > 0 {
				pod.Finalizers = nil
				if err := k8sClient.Update(ctx, &pod); err != nil {
					return client.IgnoreNotFound(err)
				}
			}
			return client.IgnoreNotFound(k8sClient.Delete(ctx, &pod, client.GracePeriodSeconds(0)))
		})).To(gomega.Succeed())
		gomega.Eventually(func() bool { return apierrors.IsNotFound(k8sClient.Get(ctx, podKey, &corev1.Pod{})) }, testing.Timeout, testing.Interval).Should(gomega.BeTrue())
	}
	oldRevision := getSTS().Labels[leaderworkerset.RevisionKey]
	for i := int32(0); i < baseline; i++ {
		podKey := client.ObjectKey{Namespace: key.Namespace, Name: fmt.Sprintf("%s-%d", key.Name, i)}
		var pod corev1.Pod
		gomega.Expect(k8sClient.Get(ctx, podKey, &pod)).To(gomega.Succeed())
		pod.Labels[appsv1.ControllerRevisionHashLabelKey] = "native-" + oldRevision
		gomega.Expect(k8sClient.Update(ctx, &pod)).To(gomega.Succeed())
		ready(i)
	}
	advance(func(*appsv1.StatefulSet) bool { return true })
	ginkgo.By("publishing growth concurrently with a template update, or during an ordinary surge rollout")
	change(func(live *leaderworkerset.LeaderWorkerSet) {
		live.Spec.LeaderWorkerTemplate.WorkerTemplate.Spec.Containers[0].Image = "test.invalid/combined-update"
		if !duringUpdate {
			live.Spec.Replicas = ptr.To(desired)
		}
	})
	if duringUpdate {
		advance(func(sts *appsv1.StatefulSet) bool {
			return sts.Labels[leaderworkerset.RevisionKey] != oldRevision && *sts.Spec.Replicas == baseline+surge
		})
		for i := baseline; i < baseline+surge; i++ {
			create(i)
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
		return state.Phase == "active" && state.Baseline == baseline && len(state.Pending) == 1 && *sts.Spec.Replicas == desired && *sts.Spec.UpdateStrategy.RollingUpdate.Partition == baseline-1
	})
	// Desired additions, rather than an extra surge, supply growth capacity.
	if !duringUpdate || surge == 0 {
		for i := baseline; i < desired; i++ {
			create(i)
		}
	}
	gomega.Expect(*getSTS().Spec.Replicas).To(gomega.Equal(desired))
	if downscale {
		ginkgo.By("downscaling with an outstanding old-leader reservation")
		change(func(live *leaderworkerset.LeaderWorkerSet) { live.Spec.Replicas = ptr.To[int32](2) })
		advance(func(sts *appsv1.StatefulSet) bool { return *sts.Spec.Replicas == 2 })
		for i := int32(2); i < desired; i++ {
			remove(i)
		}
		desired = 2
	} else {
		for i := baseline; i < desired; i++ {
			ready(i)
		}
	}
	// Replace only leaders actually selected for deletion, using NEW UIDs.
	for i := min(baseline, desired) - 1; i >= 0; i-- {
		podKey := client.ObjectKey{Namespace: key.Namespace, Name: fmt.Sprintf("%s-%d", key.Name, i)}
		advance(func(*appsv1.StatefulSet) bool {
			var pod corev1.Pod
			err := k8sClient.Get(ctx, podKey, &pod)
			return apierrors.IsNotFound(err) || (err == nil && pod.DeletionTimestamp != nil)
		})
		remove(i)
		create(i)
		if downscale && i == desired-1 {
			// Once growth is withdrawn, a Pending replacement can request the
			// configured surge. Simulate those native additions too; missing
			// surge Pods must not be mistaken for available capacity.
			advance(func(sts *appsv1.StatefulSet) bool { return *sts.Spec.Replicas == desired+2 })
			for extra := desired; extra < desired+2; extra++ {
				create(extra)
				ready(extra)
			}
		}
		ready(i)
	}
	advance(func(sts *appsv1.StatefulSet) bool {
		return *sts.Spec.Replicas == desired && *sts.Spec.UpdateStrategy.RollingUpdate.Partition == 0
	})
	if downscale {
		for extra := desired; extra < desired+2; extra++ {
			remove(extra)
		}
	}
	gomega.Expect(getSTS().Annotations[annotation]).NotTo(gomega.BeEmpty(), "must wait for native currentRevision promotion")
	gomega.Expect(retry.RetryOnConflict(retry.DefaultRetry, func() error {
		sts := getSTS()
		sts.Status.CurrentRevision = sts.Status.UpdateRevision
		return k8sClient.Status().Update(ctx, sts)
	})).To(gomega.Succeed())
	advance(func(sts *appsv1.StatefulSet) bool { return sts.Annotations[annotation] == "" })
	testing.ExpectValidLeaderStatefulSet(ctx, k8sClient, lws, desired)
	testing.ExpectLeaderWorkerSetAvailable(ctx, k8sClient, lws, "All replicas are ready")
}

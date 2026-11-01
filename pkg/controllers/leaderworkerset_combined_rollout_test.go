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
	"context"
	"fmt"
	"strconv"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/tools/events"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	leaderworkerset "sigs.k8s.io/lws/api/leaderworkerset/v1"
	revisionutils "sigs.k8s.io/lws/pkg/utils/revision"
	"sigs.k8s.io/lws/test/wrappers"
)

func newCombinedTestClient(t *testing.T, objs ...client.Object) (client.Client, *LeaderWorkerSetReconciler) {
	t.Helper()
	scheme := lwsStatusScheme(t)
	k8sClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&leaderworkerset.LeaderWorkerSet{}, &appsv1.StatefulSet{}, &corev1.Pod{}).
		WithIndex(&appsv1.StatefulSet{}, lwsOwnerKey, func(rawObj client.Object) []string {
			sts := rawObj.(*appsv1.StatefulSet)
			owner := metav1.GetControllerOf(sts)
			if owner == nil || owner.APIVersion != apiGVStr || owner.Kind != "LeaderWorkerSet" {
				return nil
			}
			return []string{owner.Name}
		}).
		WithObjects(objs...).
		Build()
	reconciler := NewLeaderWorkerSetReconciler(k8sClient, scheme, events.NewFakeRecorder(64))
	return k8sClient, reconciler
}

func makeOwnedGroupObjects(
	lws *leaderworkerset.LeaderWorkerSet,
	leaderSts *appsv1.StatefulSet,
	ord int,
	groupSize int,
	revisionKey, stsRevHash string,
	leaderReady, workerStsReady, workerPodsReady bool,
) []client.Object {
	leaderName := fmt.Sprintf("%s-%d", lws.Name, ord)
	leaderUID := types.UID(fmt.Sprintf("%s-uid-%s", leaderName, revisionKey))
	leaderPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      leaderName,
			Namespace: lws.Namespace,
			UID:       leaderUID,
			Labels: map[string]string{
				leaderworkerset.SetNameLabelKey:       lws.Name,
				leaderworkerset.WorkerIndexLabelKey:   "0",
				leaderworkerset.GroupIndexLabelKey:    strconv.Itoa(ord),
				leaderworkerset.RevisionKey:           revisionKey,
				appsv1.ControllerRevisionHashLabelKey: stsRevHash,
			},
			Annotations: map[string]string{
				leaderworkerset.SizeAnnotationKey: strconv.Itoa(groupSize),
			},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: appsv1.SchemeGroupVersion.String(),
				Kind:       "StatefulSet",
				Name:       leaderSts.Name,
				UID:        leaderSts.UID,
				Controller: ptr.To(true),
			}},
		},
	}
	if leaderReady {
		leaderPod.Status = corev1.PodStatus{
			Phase:      corev1.PodRunning,
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}},
		}
	} else {
		leaderPod.Status = corev1.PodStatus{Phase: corev1.PodPending}
	}
	objs := []client.Object{leaderPod}
	if groupSize <= 1 {
		return objs
	}

	workerStsUID := types.UID(fmt.Sprintf("%s-wsts-uid-%s", leaderName, revisionKey))
	workerCount := int32(groupSize - 1)
	workerRevHash := "wrev-" + revisionKey
	workerSts := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:       leaderName,
			Namespace:  lws.Namespace,
			UID:        workerStsUID,
			Generation: 1,
			Labels: map[string]string{
				leaderworkerset.SetNameLabelKey:    lws.Name,
				leaderworkerset.GroupIndexLabelKey: strconv.Itoa(ord),
				leaderworkerset.RevisionKey:        revisionKey,
				leaderworkerset.RoleLabelKey:       leaderworkerset.RoleWorker,
			},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: corev1.SchemeGroupVersion.String(),
				Kind:       "Pod",
				Name:       leaderPod.Name,
				UID:        leaderPod.UID,
				Controller: ptr.To(true),
			}},
		},
		Spec: appsv1.StatefulSetSpec{Replicas: ptr.To(workerCount)},
		Status: appsv1.StatefulSetStatus{
			ObservedGeneration: 1,
			Replicas:           workerCount,
			CurrentRevision:    workerRevHash,
			UpdateRevision:     workerRevHash,
		},
	}
	if workerStsReady {
		workerSts.Status.ReadyReplicas = workerCount
		workerSts.Status.AvailableReplicas = workerCount
	}
	objs = append(objs, workerSts)

	for wIdx := 1; wIdx < groupSize; wIdx++ {
		wPod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      fmt.Sprintf("%s-%d", leaderName, wIdx),
				Namespace: lws.Namespace,
				UID:       types.UID(fmt.Sprintf("%s-%d-uid-%s", leaderName, wIdx, revisionKey)),
				Labels: map[string]string{
					leaderworkerset.SetNameLabelKey:       lws.Name,
					leaderworkerset.GroupIndexLabelKey:    strconv.Itoa(ord),
					leaderworkerset.WorkerIndexLabelKey:   strconv.Itoa(wIdx),
					leaderworkerset.RevisionKey:           revisionKey,
					appsv1.ControllerRevisionHashLabelKey: workerRevHash,
				},
				Annotations: map[string]string{
					leaderworkerset.SizeAnnotationKey:          strconv.Itoa(groupSize),
					leaderworkerset.LeaderPodNameAnnotationKey: leaderName,
				},
				OwnerReferences: []metav1.OwnerReference{{
					APIVersion: appsv1.SchemeGroupVersion.String(),
					Kind:       "StatefulSet",
					Name:       workerSts.Name,
					UID:        workerSts.UID,
					Controller: ptr.To(true),
				}},
			},
		}
		if workerPodsReady {
			wPod.Status = corev1.PodStatus{
				Phase:      corev1.PodRunning,
				Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}},
			}
		} else {
			wPod.Status = corev1.PodStatus{Phase: corev1.PodPending}
		}
		objs = append(objs, wPod)
	}
	return objs
}

// ackLeaderStatefulSet simulates Kubernetes StatefulSetController observing the
// current StatefulSet generation and updating ObservedGeneration/UpdateRevision.
func ackLeaderStatefulSet(t *testing.T, ctx context.Context, c client.Client, key types.NamespacedName) *appsv1.StatefulSet {
	t.Helper()
	var sts appsv1.StatefulSet
	if err := c.Get(ctx, key, &sts); err != nil {
		t.Fatalf("getting leader statefulset: %v", err)
	}
	if sts.UID == "" {
		sts.UID = "leader-sts-uid"
	}
	if sts.Generation == 0 {
		sts.Generation = 1
	}
	sts.Status.ObservedGeneration = sts.Generation
	sts.Status.Replicas = *sts.Spec.Replicas
	sts.Status.UpdateRevision = "sts-" + revisionutils.GetRevisionKey(&sts)
	if sts.Status.CurrentRevision == "" {
		sts.Status.CurrentRevision = sts.Status.UpdateRevision
	}
	if err := c.Status().Update(ctx, &sts); err != nil {
		t.Fatalf("updating leader statefulset status: %v", err)
	}
	return &sts
}

// TestCombinedRolloutIssue717PendingScaleUpDoesNotBlockReplacement verifies the
// exact #717 scenario: scaling 1 -> 2 together with a template update under
// maxUnavailable=1, maxSurge=0 replaces ordinal 0 even while ordinal 1 stays
// Pending, including on pre-1.35 clusters where StatefulSetController stops its
// descending update loop at Pending ordinal 1.
func TestCombinedRolloutIssue717PendingScaleUpDoesNotBlockReplacement(t *testing.T) {
	ctx := context.Background()
	lws := wrappers.BuildBasicLeaderWorkerSet("test-sample", "default").
		WorkerTemplateSpec(wrappers.MakeWorkerPodSpec()).
		Replica(1).
		Size(2).
		RolloutStrategy(leaderworkerset.RolloutStrategy{
			Type: leaderworkerset.RollingUpdateStrategyType,
			RollingUpdateConfiguration: &leaderworkerset.RollingUpdateConfiguration{
				Partition:      ptr.To[int32](0),
				MaxUnavailable: intstr.FromInt32(1),
				MaxSurge:       intstr.FromInt32(0),
			},
		}).Obj()
	lws.UID = "lws-uid"
	lws.Generation = 1

	c, r := newCombinedTestClient(t, lws)
	req := reconcile.Request{NamespacedName: client.ObjectKeyFromObject(lws)}

	// Initial creation at replicas=1, rev1.
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("initial Reconcile() error = %v", err)
	}
	sts := ackLeaderStatefulSet(t, ctx, c, req.NamespacedName)
	rev1 := revisionutils.GetRevisionKey(sts)
	for _, obj := range makeOwnedGroupObjects(lws, sts, 0, 2, rev1, "sts-"+rev1, true, true, true) {
		if err := c.Create(ctx, obj); err != nil {
			t.Fatalf("creating group 0 object: %v", err)
		}
	}

	// Combined scale-up (1 -> 2) + worker template update (rev2).
	if err := c.Get(ctx, req.NamespacedName, lws); err != nil {
		t.Fatalf("reading lws: %v", err)
	}
	lws.Generation = 2
	lws.Spec.Replicas = ptr.To[int32](2)
	lws.Spec.LeaderWorkerTemplate.WorkerTemplate.Spec.Containers[0].Image = "worker:v2"
	if err := c.Update(ctx, lws); err != nil {
		t.Fatalf("updating lws: %v", err)
	}

	// Pass 1: publishes rev2 with replicas=2, partition=1, baseline="1", stsMaxUnavailable=2.
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("pass 1 Reconcile() error = %v", err)
	}
	if err := c.Get(ctx, req.NamespacedName, sts); err != nil {
		t.Fatalf("reading sts after pass 1: %v", err)
	}
	if got := *sts.Spec.Replicas; got != 2 {
		t.Fatalf("pass 1 sts.Spec.Replicas = %d, want 2", got)
	}
	if got := *sts.Spec.UpdateStrategy.RollingUpdate.Partition; got != 1 {
		t.Fatalf("pass 1 sts partition = %d, want 1", got)
	}
	if got := sts.Annotations[rolloutBaselineAnnotationKey]; got != "1" {
		t.Fatalf("pass 1 baseline annotation = %q, want %q", got, "1")
	}
	if got := sts.Spec.UpdateStrategy.RollingUpdate.MaxUnavailable.IntVal; got != 2 {
		t.Fatalf("pass 1 sts maxUnavailable = %d, want 2 (1 maxUnavailable + 1 scale-up slot)", got)
	}

	// StatefulSetController observes rev2 and creates ordinal 1, which remains Pending.
	sts.Generation = 2
	if err := c.Update(ctx, sts); err != nil {
		t.Fatalf("bumping sts generation: %v", err)
	}
	sts = ackLeaderStatefulSet(t, ctx, c, req.NamespacedName)
	rev2 := revisionutils.GetRevisionKey(sts)
	for _, obj := range makeOwnedGroupObjects(lws, sts, 1, 2, rev2, "sts-"+rev2, false, false, false) {
		// Only Pending leader pod exists while waiting on node capacity.
		if pod, ok := obj.(*corev1.Pod); ok && pod.Labels[leaderworkerset.WorkerIndexLabelKey] == "0" {
			if err := c.Create(ctx, pod); err != nil {
				t.Fatalf("creating pending group 1 leader: %v", err)
			}
		}
	}

	// Pass 2: baseline=1, maxUnavailable=1 => floor=0, so partition drops to 0
	// despite ordinal 1 being Pending. Ordinal 0 is not deleted yet because
	// StatefulSetController has not observed partition=0.
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("pass 2 Reconcile() error = %v", err)
	}
	if err := c.Get(ctx, req.NamespacedName, sts); err != nil {
		t.Fatalf("reading sts after pass 2: %v", err)
	}
	if got := *sts.Spec.UpdateStrategy.RollingUpdate.Partition; got != 0 {
		t.Fatalf("pass 2 sts partition = %d, want 0", got)
	}
	var pod0 corev1.Pod
	if err := c.Get(ctx, types.NamespacedName{Name: "test-sample-0", Namespace: "default"}, &pod0); err != nil {
		t.Fatalf("expected test-sample-0 to still exist before sts observes partition=0: %v", err)
	}

	// Simulate pre-1.35 StatefulSetController observing partition=0 (Generation 3)
	// while stopping its descending update loop at Pending test-sample-1.
	sts.Generation = 3
	if err := c.Update(ctx, sts); err != nil {
		t.Fatalf("bumping sts generation to 3: %v", err)
	}
	ackLeaderStatefulSet(t, ctx, c, req.NamespacedName)

	// Pass 3: deleteStaleOrdinalLeaders deletes stale test-sample-0 so
	// StatefulSetController can recreate it on rev2 immediately.
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("pass 3 Reconcile() error = %v", err)
	}
	err := c.Get(ctx, types.NamespacedName{Name: "test-sample-0", Namespace: "default"}, &pod0)
	if !apierrors.IsNotFound(err) {
		t.Fatalf("expected stale test-sample-0 to be deleted, got err=%v", err)
	}

	// Repeated reconciles and controller restarts remain idempotent and keep baseline="1".
	restarted := NewLeaderWorkerSetReconciler(c, lwsStatusScheme(t), events.NewFakeRecorder(64))
	for i := range 3 {
		if _, err := restarted.Reconcile(ctx, req); err != nil {
			t.Fatalf("restart Reconcile(%d) error = %v", i, err)
		}
	}
	if err := c.Get(ctx, req.NamespacedName, sts); err != nil {
		t.Fatalf("reading sts after restart reconciles: %v", err)
	}
	if got := sts.Annotations[rolloutBaselineAnnotationKey]; got != "1" {
		t.Fatalf("baseline annotation during in-flight replacement = %q, want %q", got, "1")
	}

	// Once both groups become whole-group Ready on rev2, rollout converges and clears baseline.
	var pendingLeader1 corev1.Pod
	if err := c.Get(ctx, types.NamespacedName{Name: "test-sample-1", Namespace: "default"}, &pendingLeader1); err != nil {
		t.Fatalf("getting pending test-sample-1: %v", err)
	}
	if err := c.Delete(ctx, &pendingLeader1); err != nil {
		t.Fatalf("deleting pending test-sample-1 for replacement: %v", err)
	}
	var oldWorkerSts0 appsv1.StatefulSet
	if err := c.Get(ctx, types.NamespacedName{Name: "test-sample-0", Namespace: "default"}, &oldWorkerSts0); err == nil {
		_ = c.Delete(ctx, &oldWorkerSts0)
	}
	var oldWorkerPod0 corev1.Pod
	if err := c.Get(ctx, types.NamespacedName{Name: "test-sample-0-1", Namespace: "default"}, &oldWorkerPod0); err == nil {
		_ = c.Delete(ctx, &oldWorkerPod0)
	}
	for _, ord := range []int{0, 1} {
		for _, obj := range makeOwnedGroupObjects(lws, sts, ord, 2, rev2, "sts-"+rev2, true, true, true) {
			if err := c.Create(ctx, obj); err != nil {
				t.Fatalf("creating converged group %d object: %v", ord, err)
			}
		}
	}
	if _, err := restarted.Reconcile(ctx, req); err != nil {
		t.Fatalf("converged Reconcile() error = %v", err)
	}
	if err := c.Get(ctx, req.NamespacedName, sts); err != nil {
		t.Fatalf("reading converged sts: %v", err)
	}
	if _, exists := sts.Annotations[rolloutBaselineAnnotationKey]; exists {
		t.Fatalf("expected %s to be cleared on convergence, got %q", rolloutBaselineAnnotationKey, sts.Annotations[rolloutBaselineAnnotationKey])
	}
}

// TestCombinedRolloutMaxUnavailableZeroWaitsForWholeGroupReadyAddition verifies
// that maxUnavailable=0, maxSurge=1 never disrupts baseline groups until an
// added/surge group is whole-group Ready (leader + worker StatefulSet + worker
// Pods, with matching controller UIDs and non-terminating members), and never
// double-spends a single Ready addition across reconciles or restarts.
func TestCombinedRolloutMaxUnavailableZeroWaitsForWholeGroupReadyAddition(t *testing.T) {
	ctx := context.Background()
	lws := wrappers.BuildBasicLeaderWorkerSet("test-sample", "default").
		WorkerTemplateSpec(wrappers.MakeWorkerPodSpec()).
		Replica(2).
		Size(2).
		RolloutStrategy(leaderworkerset.RolloutStrategy{
			Type: leaderworkerset.RollingUpdateStrategyType,
			RollingUpdateConfiguration: &leaderworkerset.RollingUpdateConfiguration{
				Partition:      ptr.To[int32](0),
				MaxUnavailable: intstr.FromInt32(0),
				MaxSurge:       intstr.FromInt32(1),
			},
		}).Obj()
	lws.UID = "lws-uid"
	lws.Generation = 1

	c, r := newCombinedTestClient(t, lws)
	req := reconcile.Request{NamespacedName: client.ObjectKeyFromObject(lws)}

	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("initial Reconcile() error = %v", err)
	}
	sts := ackLeaderStatefulSet(t, ctx, c, req.NamespacedName)
	rev1 := revisionutils.GetRevisionKey(sts)
	for _, ord := range []int{0, 1} {
		for _, obj := range makeOwnedGroupObjects(lws, sts, ord, 2, rev1, "sts-"+rev1, true, true, true) {
			if err := c.Create(ctx, obj); err != nil {
				t.Fatalf("creating group %d: %v", ord, err)
			}
		}
	}

	// Combined scale-up (2 -> 4) + template update with maxUnavailable=0, maxSurge=1.
	if err := c.Get(ctx, req.NamespacedName, lws); err != nil {
		t.Fatalf("reading lws: %v", err)
	}
	lws.Generation = 2
	lws.Spec.Replicas = ptr.To[int32](4)
	lws.Spec.LeaderWorkerTemplate.WorkerTemplate.Spec.Containers[0].Image = "worker:v2"
	if err := c.Update(ctx, lws); err != nil {
		t.Fatalf("updating lws: %v", err)
	}

	// Pass 1 publishes rev2 with partition=2, replicas=4, baseline="2".
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("pass 1 Reconcile() error = %v", err)
	}
	if err := c.Get(ctx, req.NamespacedName, sts); err != nil {
		t.Fatalf("reading sts: %v", err)
	}
	sts.Generation = 2
	_ = c.Update(ctx, sts)
	sts = ackLeaderStatefulSet(t, ctx, c, req.NamespacedName)
	rev2 := revisionutils.GetRevisionKey(sts)

	// Ordinal 2 has Ready leader + Ready workerSts, EXCEPT worker pod 2-1 is still Pending!
	for _, obj := range makeOwnedGroupObjects(lws, sts, 2, 2, rev2, "sts-"+rev2, true, true, false) {
		if err := c.Create(ctx, obj); err != nil {
			t.Fatalf("creating partial group 2: %v", err)
		}
	}
	// Pass 2: expands surge to 5, MUST keep partition=2 because worker pod 2-1 is not Ready!
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("pass 2 Reconcile() error = %v", err)
	}
	if err := c.Get(ctx, req.NamespacedName, sts); err != nil {
		t.Fatalf("reading sts after pass 2: %v", err)
	}
	if got := *sts.Spec.UpdateStrategy.RollingUpdate.Partition; got != 2 {
		t.Fatalf("partition with unready worker pod = %d, want 2", got)
	}
	sts.Generation = 3
	_ = c.Update(ctx, sts)
	sts = ackLeaderStatefulSet(t, ctx, c, req.NamespacedName)

	// Mark worker pod 2-1 Ready, EXCEPT give workerSts a stale controller UID -> still must NOT grant credit!
	var wSts2 appsv1.StatefulSet
	if err := c.Get(ctx, types.NamespacedName{Name: "test-sample-2", Namespace: "default"}, &wSts2); err != nil {
		t.Fatalf("getting workerSts 2: %v", err)
	}
	realLeaderUID := wSts2.OwnerReferences[0].UID
	wSts2.OwnerReferences[0].UID = "stale-leader-uid"
	if err := c.Update(ctx, &wSts2); err != nil {
		t.Fatalf("updating workerSts 2 owner: %v", err)
	}
	var wPod2 corev1.Pod
	if err := c.Get(ctx, types.NamespacedName{Name: "test-sample-2-1", Namespace: "default"}, &wPod2); err != nil {
		t.Fatalf("getting worker pod 2-1: %v", err)
	}
	wPod2.Status = corev1.PodStatus{
		Phase:      corev1.PodRunning,
		Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}},
	}
	if err := c.Status().Update(ctx, &wPod2); err != nil {
		t.Fatalf("updating worker pod 2-1 status: %v", err)
	}
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("stale-owner Reconcile() error = %v", err)
	}
	if err := c.Get(ctx, req.NamespacedName, sts); err != nil {
		t.Fatalf("reading sts after stale-owner check: %v", err)
	}
	if got := *sts.Spec.UpdateStrategy.RollingUpdate.Partition; got != 2 {
		t.Fatalf("partition with stale workerSts owner = %d, want 2", got)
	}

	// Restore valid owner UID on workerSts 2 -> group 2 is now whole-group Ready!
	if err := c.Get(ctx, types.NamespacedName{Name: "test-sample-2", Namespace: "default"}, &wSts2); err != nil {
		t.Fatalf("getting workerSts 2: %v", err)
	}
	wSts2.OwnerReferences[0].UID = realLeaderUID
	if err := c.Update(ctx, &wSts2); err != nil {
		t.Fatalf("restoring workerSts 2 owner: %v", err)
	}

	// Partition now steps down by 1 (from 2 to 1), keeping group 0 + group 2 Ready (2 >= floor 2).
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("whole-group-ready Reconcile() error = %v", err)
	}
	if err := c.Get(ctx, req.NamespacedName, sts); err != nil {
		t.Fatalf("reading sts: %v", err)
	}
	if got := *sts.Spec.UpdateStrategy.RollingUpdate.Partition; got != 1 {
		t.Fatalf("partition after 1 whole-group Ready addition = %d, want 1", got)
	}
	sts.Generation = 4
	_ = c.Update(ctx, sts)
	ackLeaderStatefulSet(t, ctx, c, req.NamespacedName)

	// Reconciling repeatedly and across controller restarts while group 1 is
	// replacing MUST NOT double-spend group 2's credit to drop partition to 0!
	restarted := NewLeaderWorkerSetReconciler(c, lwsStatusScheme(t), events.NewFakeRecorder(64))
	for i := range 4 {
		if _, err := restarted.Reconcile(ctx, req); err != nil {
			t.Fatalf("restart Reconcile(%d) error = %v", i, err)
		}
		if err := c.Get(ctx, req.NamespacedName, sts); err != nil {
			t.Fatalf("reading sts: %v", err)
		}
		if got := *sts.Spec.UpdateStrategy.RollingUpdate.Partition; got != 1 {
			t.Fatalf("restart pass %d partition = %d, want 1 (must not double-spend credit)", i, got)
		}
	}
}

func TestCombinedRolloutSupersedingTemplatePreservesBaseline(t *testing.T) {
	ctx := context.Background()
	lws := wrappers.BuildBasicLeaderWorkerSet("test-sample", "default").
		Replica(6).
		Size(1).
		RolloutStrategy(leaderworkerset.RolloutStrategy{
			Type: leaderworkerset.RollingUpdateStrategyType,
			RollingUpdateConfiguration: &leaderworkerset.RollingUpdateConfiguration{
				Partition:      ptr.To[int32](0),
				MaxUnavailable: intstr.FromInt32(1),
				MaxSurge:       intstr.FromInt32(0),
			},
		}).Obj()

	// Active rollout that started at baseline=4 and scaled to 6 (floor = 4 - 1 = 3).
	sts := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      lws.Name,
			Namespace: lws.Namespace,
			Annotations: map[string]string{
				leaderworkerset.ReplicasAnnotationKey: "6",
				rolloutBaselineAnnotationKey:          "4",
			},
		},
		Spec: appsv1.StatefulSetSpec{
			Replicas: ptr.To[int32](6),
			UpdateStrategy: appsv1.StatefulSetUpdateStrategy{
				Type:          appsv1.RollingUpdateStatefulSetStrategyType,
				RollingUpdate: &appsv1.RollingUpdateStatefulSetStrategy{Partition: ptr.To[int32](3)},
			},
		},
	}

	// Superseding template update (rev3) combined with another scale-up to 8!
	lws.Spec.Replicas = ptr.To[int32](8)
	objs := []client.Object{
		lwsStatusLeaderPod(lws, 0, "rev-1", true),
		lwsStatusLeaderPod(lws, 1, "rev-1", true),
		lwsStatusLeaderPod(lws, 2, "rev-1", true),
		lwsStatusLeaderPod(lws, 3, "rev-2", true), // Ready on superseded rev-2, stale for rev-3!
	}
	r, _ := lwsStatusNewReconciler(t, objs...)

	// Pass 1 (lwsUpdated=true): preserves baseline=4 rather than overwriting with 6 or 8!
	plan1, err := r.planRollingUpdate(ctx, lws, sts, "rev-3", true)
	if err != nil {
		t.Fatalf("planRollingUpdate(pass 1) error = %v", err)
	}
	if plan1.baseline == nil || *plan1.baseline != 4 {
		t.Fatalf("pass 1 baseline = %v, want 4", plan1.baseline)
	}
	if plan1.partition != 6 || plan1.replicas != 8 {
		t.Fatalf("pass 1 (partition, replicas) = (%d, %d), want (6, 8)", plan1.partition, plan1.replicas)
	}

	// Pass 2 (lwsUpdated=false): sts now has replicas=8, partition=6, baseline=4.
	sts.Spec.Replicas = ptr.To[int32](8)
	sts.Spec.UpdateStrategy.RollingUpdate.Partition = ptr.To[int32](6)
	sts.Annotations[leaderworkerset.ReplicasAnnotationKey] = "8"
	plan2, err := r.planRollingUpdate(ctx, lws, sts, "rev-3", false)
	if err != nil {
		t.Fatalf("planRollingUpdate(pass 2) error = %v", err)
	}
	// Surviving Ready groups: 0,1,2 (rev-1) + 3 (rev-2) = 4 Ready stale groups; floor = 4 - 1 = 3.
	// Uncreated 7,6,5,4 cost 0 Ready groups; ordinal 3 spends the 1 affordable group (4 -> 3 >= floor 3);
	// ordinals 0,1,2 remain protected => partition = 3!
	if plan2.partition != 3 {
		t.Fatalf("pass 2 partition = %d, want 3", plan2.partition)
	}
	if plan2.baseline == nil || *plan2.baseline != 4 {
		t.Fatalf("pass 2 baseline = %v, want 4", plan2.baseline)
	}
}

func TestCombinedRolloutScaleDownPercentageRoundsToZeroAndPrunesCounts(t *testing.T) {
	ctx := context.Background()
	lws := wrappers.BuildBasicLeaderWorkerSet("test-sample", "default").
		Replica(3).
		Size(1).
		Annotation(map[string]string{
			leaderworkerset.GroupRestartCountsAnnotationKey: `{"rev-1/0":1,"rev-1/3":2}`,
		}).
		RolloutStrategy(leaderworkerset.RolloutStrategy{
			Type: leaderworkerset.RollingUpdateStrategyType,
			RollingUpdateConfiguration: &leaderworkerset.RollingUpdateConfiguration{
				Partition:      ptr.To[int32](0),
				MaxUnavailable: intstr.FromString("25%"),
				MaxSurge:       intstr.FromInt32(0),
			},
		}).Obj()

	sts := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      lws.Name,
			Namespace: lws.Namespace,
			Annotations: map[string]string{
				leaderworkerset.ReplicasAnnotationKey: "4",
				rolloutBaselineAnnotationKey:          "4",
			},
			Labels: map[string]string{
				leaderworkerset.SetNameLabelKey: lws.Name,
				leaderworkerset.RevisionKey:     "rev-2",
				leaderworkerset.RoleLabelKey:    leaderworkerset.RoleLeader,
			},
		},
		Spec: appsv1.StatefulSetSpec{
			Replicas: ptr.To[int32](4),
			UpdateStrategy: appsv1.StatefulSetUpdateStrategy{
				Type:          appsv1.RollingUpdateStatefulSetStrategyType,
				RollingUpdate: &appsv1.RollingUpdateStatefulSetStrategy{Partition: ptr.To[int32](3)},
			},
		},
	}

	objs := []client.Object{
		lws, sts,
		lwsStatusLeaderPod(lws, 0, "rev-1", true),
		lwsStatusLeaderPod(lws, 1, "rev-1", true),
		lwsStatusLeaderPod(lws, 2, "rev-1", true),
		lwsStatusLeaderPod(lws, 3, "rev-2", false),
	}
	c, r := newCombinedTestClient(t, objs...)
	req := reconcile.Request{NamespacedName: client.ObjectKeyFromObject(lws)}

	// Ensure rev-2 ControllerRevision exists so Reconcile treats rev-2 as current.
	rev2CR, err := revisionutils.NewRevision(ctx, c, lws, "rev-2")
	if err != nil {
		t.Fatalf("NewRevision() error = %v", err)
	}
	if _, err := revisionutils.CreateRevision(ctx, c, rev2CR); err != nil {
		t.Fatalf("CreateRevision() error = %v", err)
	}

	// Pass 1 shrinks sts.Spec.Replicas from 4 -> 3 and prunes group 3's restart count!
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("pass 1 Reconcile() error = %v", err)
	}
	var updatedSts appsv1.StatefulSet
	if err := c.Get(ctx, req.NamespacedName, &updatedSts); err != nil {
		t.Fatalf("reading sts: %v", err)
	}
	if got := *updatedSts.Spec.Replicas; got != 3 {
		t.Fatalf("sts.Spec.Replicas after shrink = %d, want 3", got)
	}
	var updatedLWS leaderworkerset.LeaderWorkerSet
	if err := c.Get(ctx, req.NamespacedName, &updatedLWS); err != nil {
		t.Fatalf("reading lws: %v", err)
	}
	if got := updatedLWS.Annotations[leaderworkerset.GroupRestartCountsAnnotationKey]; got != `{"rev-1/0":1}` {
		t.Fatalf("group restart counts after shrink = %q, want %q", got, `{"rev-1/0":1}`)
	}

	// Pass 2: 25% of 3 rounds down to 0, so floor = 3 - 0 = 3; healthy groups 0..2 stay protected (partition=3)!
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("pass 2 Reconcile() error = %v", err)
	}
	if err := c.Get(ctx, req.NamespacedName, &updatedSts); err != nil {
		t.Fatalf("reading sts after pass 2: %v", err)
	}
	if got := *updatedSts.Spec.UpdateStrategy.RollingUpdate.Partition; got != 3 {
		t.Fatalf("partition when maxUnavailable rounds to 0 = %d, want 3", got)
	}
}

func TestCombinedRolloutNeutralRepairAndExhaustedGroups(t *testing.T) {
	ctx := context.Background()
	lws := wrappers.BuildBasicLeaderWorkerSet("test-sample", "default").
		Replica(3).
		Size(1).
		RolloutStrategy(leaderworkerset.RolloutStrategy{
			Type: leaderworkerset.RollingUpdateStrategyType,
			RollingUpdateConfiguration: &leaderworkerset.RollingUpdateConfiguration{
				Partition:      ptr.To[int32](0),
				MaxUnavailable: intstr.FromInt32(0),
				MaxSurge:       intstr.FromInt32(1),
			},
		}).Obj()

	// Ordinal 0 Ready old, Ordinal 1 unready old, Ordinal 2 Ready old, Ordinal 3 exhausted new!
	exhaustedNew := lwsStatusLeaderPod(lws, 3, "rev-2", true)
	exhaustedNew.Annotations = map[string]string{
		leaderworkerset.GroupRestartBudgetExhaustedAnnotationKey: "true",
	}
	sts := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      lws.Name,
			Namespace: lws.Namespace,
			Annotations: map[string]string{
				leaderworkerset.ReplicasAnnotationKey: "3",
				rolloutBaselineAnnotationKey:          "3",
			},
		},
		Spec: appsv1.StatefulSetSpec{
			Replicas: ptr.To[int32](4),
			UpdateStrategy: appsv1.StatefulSetUpdateStrategy{
				Type:          appsv1.RollingUpdateStatefulSetStrategyType,
				RollingUpdate: &appsv1.RollingUpdateStatefulSetStrategy{Partition: ptr.To[int32](3)},
			},
		},
	}

	r, _ := lwsStatusNewReconciler(t,
		lwsStatusLeaderPod(lws, 0, "rev-1", true),
		lwsStatusLeaderPod(lws, 1, "rev-1", false),
		lwsStatusLeaderPod(lws, 2, "rev-1", true),
		exhaustedNew,
	)

	// Exhausted ordinal 3 must NOT grant credit, and unready ordinal 1 sits below
	// Ready ordinal 2 when surviving (2) < floor (3), so partition MUST stay at 3!
	plan, err := r.planRollingUpdate(ctx, lws, sts, "rev-2", false)
	if err != nil {
		t.Fatalf("planRollingUpdate() error = %v", err)
	}
	if plan.partition != 3 {
		t.Fatalf("partition = %d, want 3 (must not skip Ready ordinal 2 or credit exhausted ordinal 3)", plan.partition)
	}

	// Conversely, once surge ordinal 3 is Ready on rev-2 while ordinal 2 is exhausted
	// on rev-1 and ordinal 1 is unready on rev-1, step-bounded neutral repair advances
	// partition from 3 -> 2 (repairing ordinal 2 first), and once ordinal 2 is Ready
	// on rev-2 advances 2 -> 1 while keeping Ready ordinal 0 protected!
	exhaustedOld2 := lwsStatusLeaderPod(lws, 2, "rev-1", true)
	exhaustedOld2.Annotations = map[string]string{
		leaderworkerset.GroupRestartBudgetExhaustedAnnotationKey: "true",
	}
	r2, _ := lwsStatusNewReconciler(t,
		lwsStatusLeaderPod(lws, 0, "rev-1", true),
		lwsStatusLeaderPod(lws, 1, "rev-1", false),
		exhaustedOld2,
		lwsStatusLeaderPod(lws, 3, "rev-2", true),
	)
	plan2, err := r2.planRollingUpdate(ctx, lws, sts, "rev-2", false)
	if err != nil {
		t.Fatalf("planRollingUpdate(neutral repair step 1) error = %v", err)
	}
	if plan2.partition != 2 {
		t.Fatalf("neutral repair step 1 partition = %d, want 2", plan2.partition)
	}

	sts.Spec.UpdateStrategy.RollingUpdate.Partition = ptr.To[int32](2)
	r3, _ := lwsStatusNewReconciler(t,
		lwsStatusLeaderPod(lws, 0, "rev-1", true),
		lwsStatusLeaderPod(lws, 1, "rev-1", false),
		lwsStatusLeaderPod(lws, 2, "rev-2", true),
		lwsStatusLeaderPod(lws, 3, "rev-2", true),
	)
	plan3, err := r3.planRollingUpdate(ctx, lws, sts, "rev-2", false)
	if err != nil {
		t.Fatalf("planRollingUpdate(neutral repair step 2) error = %v", err)
	}
	if plan3.partition != 1 {
		t.Fatalf("neutral repair step 2 partition = %d, want 1", plan3.partition)
	}
}

func TestCombinedRolloutMixedGroupSize(t *testing.T) {
	ctx := context.Background()
	// Rolling from Size=1 (rev-1) to Size=2 (rev-2) while scaling 1 -> 2 with maxUnavailable=0, maxSurge=1.
	lws := wrappers.BuildBasicLeaderWorkerSet("test-sample", "default").
		WorkerTemplateSpec(wrappers.MakeWorkerPodSpec()).
		Replica(2).
		Size(2).
		RolloutStrategy(leaderworkerset.RolloutStrategy{
			Type: leaderworkerset.RollingUpdateStrategyType,
			RollingUpdateConfiguration: &leaderworkerset.RollingUpdateConfiguration{
				Partition:      ptr.To[int32](0),
				MaxUnavailable: intstr.FromInt32(0),
				MaxSurge:       intstr.FromInt32(1),
			},
		}).Obj()

	oldSize1Leader := lwsStatusLeaderPod(lws, 0, "rev-1", true)
	oldSize1Leader.Annotations = map[string]string{leaderworkerset.SizeAnnotationKey: "1"}

	newSize2Leader := lwsStatusLeaderPod(lws, 1, "rev-2", true)
	newSize2Leader.Annotations = map[string]string{leaderworkerset.SizeAnnotationKey: "2"}
	// Ordinal 1 has no worker StatefulSet yet, so as a Size=2 group it is NOT Ready!

	r, _ := lwsStatusNewReconciler(t, oldSize1Leader, newSize2Leader)
	states, err := r.getReplicaStates(ctx, lws, 2, "rev-2")
	if err != nil {
		t.Fatalf("getReplicaStates() error = %v", err)
	}
	if !states[0].ready || states[0].updated {
		t.Fatalf("states[0] (stamped size=1 old group) = %+v, want {ready:true, updated:false}", states[0])
	}
	if states[1].ready || states[1].updated {
		t.Fatalf("states[1] (size=2 missing workerSts) = %+v, want {ready:false, updated:false}", states[1])
	}

	// updateConditions also honors the stamped size=1 on ordinal 0, reporting 1 ReadyReplica.
	_, _, err = r.updateConditions(ctx, lws, "rev-2", 2)
	if err != nil {
		t.Fatalf("updateConditions() error = %v", err)
	}
	if lws.Status.ReadyReplicas != 1 {
		t.Fatalf("status.readyReplicas = %d, want 1", lws.Status.ReadyReplicas)
	}
}

func TestPodWatchPredicateReadinessAndTerminationTransitions(t *testing.T) {
	r := &LeaderWorkerSetReconciler{}
	pred := r.podWatchPredicate()

	workerPending := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-lws-0-1",
			Namespace: "default",
			Labels: map[string]string{
				leaderworkerset.SetNameLabelKey:     "test-lws",
				leaderworkerset.WorkerIndexLabelKey: "1",
			},
		},
		Status: corev1.PodStatus{Phase: corev1.PodPending},
	}
	workerReady := workerPending.DeepCopy()
	workerReady.Status = corev1.PodStatus{
		Phase:      corev1.PodRunning,
		Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}},
	}
	if !pred.Update(event.UpdateEvent{ObjectOld: workerPending, ObjectNew: workerReady}) {
		t.Fatal("expected worker readiness transition to trigger reconcile")
	}
	if pred.Update(event.UpdateEvent{ObjectOld: workerReady, ObjectNew: workerReady.DeepCopy()}) {
		t.Fatal("expected unchanged Ready worker status update to be ignored")
	}

	now := metav1.Now()
	workerTerminating := workerReady.DeepCopy()
	workerTerminating.DeletionTimestamp = &now
	if !pred.Update(event.UpdateEvent{ObjectOld: workerReady, ObjectNew: workerTerminating}) {
		t.Fatal("expected worker termination transition to trigger reconcile")
	}

	hashWorkerReady := workerReady.DeepCopy()
	hashWorkerReady.Annotations = map[string]string{
		leaderworkerset.GroupIdentityAnnotationKey: string(leaderworkerset.GroupIdentityHash),
	}
	if pred.Update(event.UpdateEvent{ObjectOld: workerPending, ObjectNew: hashWorkerReady}) {
		t.Fatal("expected hash-mode worker readiness update to be ignored by LWS predicate")
	}
}

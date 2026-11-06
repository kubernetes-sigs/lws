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
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/tools/events"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	leaderworkerset "sigs.k8s.io/lws/api/leaderworkerset/v1"
	revisionutils "sigs.k8s.io/lws/pkg/utils/revision"
	"sigs.k8s.io/lws/test/wrappers"
)

func newCombinedTestClient(t *testing.T, objs ...client.Object) (client.Client, *LeaderWorkerSetReconciler) {
	t.Helper()
	return newCombinedTestClientWithInterceptor(t, interceptor.Funcs{}, objs...)
}

func newCombinedTestClientWithInterceptor(t *testing.T, funcs interceptor.Funcs, objs ...client.Object) (client.Client, *LeaderWorkerSetReconciler) {
	t.Helper()
	scheme := lwsStatusScheme(t)
	baseClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&leaderworkerset.LeaderWorkerSet{}, &appsv1.StatefulSet{}, &appsv1.Deployment{}, &corev1.Pod{}).
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
	reconcilerClient := interceptor.NewClient(baseClient, funcs)
	reconciler := NewLeaderWorkerSetReconciler(reconcilerClient, scheme, events.NewFakeRecorder(128))
	return baseClient, reconciler
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

func TestRollingUpdatePartitionWithFloorMultipleUnreadyPrefix(t *testing.T) {
	ff := replicaState{ready: false, updated: false}
	tf := replicaState{ready: true, updated: false}
	tt := replicaState{ready: true, updated: true}

	// Regression test: baseline=3, maxUnavailable=2 => floor=1.
	// Ordinals 0 and 1 are unready while ordinal 2 is the last Ready group.
	// Partition must remain at 3 so ordinal 2 is not disrupted below floor=1.
	if got := rollingUpdatePartitionWithFloor([]replicaState{ff, ff, tf}, 1, 3, 0); got != 3 {
		t.Fatalf("rollingUpdatePartitionWithFloor([ff, ff, tf], floor=1, cur=3) = %d, want 3", got)
	}

	// Regression test: already-credited (ready && updated) ordinal 2 at >= currentPartition
	// must not be double-counted as an additional Ready group below partition.
	if got := rollingUpdatePartitionWithFloor([]replicaState{ff, ff, tt, tf}, 2, 2, 0); got != 2 {
		t.Fatalf("rollingUpdatePartitionWithFloor([ff, ff, tt, tf], floor=2, cur=2) = %d, want 2", got)
	}
}

// guaranteedAvailable counts the Ready groups a partition keeps available:
// every Ready group below partition, plus the updated Ready groups at or above
// it. Old groups at or above partition may all be replaced at once.
func guaranteedAvailable(states []replicaState, partition int32) int32 {
	var available int32
	for idx := range states {
		if !states[idx].ready {
			continue
		}
		if int32(idx) < partition || states[idx].updated {
			available++
		}
	}
	return available
}

// TestRollingUpdatePartitionWithFloorStateSpace enumerates every (ready,
// updated) vector for up to 6 ordinals against every floor, current partition,
// and user partition, and checks the floor and liveness properties the
// combined rollout relies on.
func TestRollingUpdatePartitionWithFloorStateSpace(t *testing.T) {
	kinds := []replicaState{
		{ready: false, updated: false},
		{ready: true, updated: false},
		{ready: false, updated: true},
		{ready: true, updated: true},
	}
	checked := 0
	for n := 1; n <= 6; n++ {
		states := make([]replicaState, n)
		total := 1
		for range n {
			total *= len(kinds)
		}
		for code := 0; code < total; code++ {
			rest := code
			for idx := range states {
				states[idx] = kinds[rest%len(kinds)]
				rest /= len(kinds)
			}
			for floor := int32(0); floor <= int32(n); floor++ {
				for current := int32(0); current <= int32(n); current++ {
					for user := int32(0); user <= current; user++ {
						checked++
						got := rollingUpdatePartitionWithFloor(states, floor, current, user)
						desc := fmt.Sprintf("states=%v floor=%d current=%d user=%d -> %d", states, floor, current, user, got)

						// Partition only moves down, and never below the user partition.
						if got < user || got > current {
							t.Fatalf("partition out of bounds: %s", desc)
						}

						// Lowering the partition never drops guaranteed availability below
						// floor, unless the current partition already sits below it.
						before := guaranteedAvailable(states, current)
						after := guaranteedAvailable(states, got)
						if after < min(floor, before) {
							t.Fatalf("floor violated: available %d -> %d: %s", before, after, desc)
						}

						// Liveness: once every group at or above the user partition is
						// updated and Ready, the rollout finishes.
						if allUpdatedAndReadyFrom(states, user, int32(n)) && got != user {
							t.Fatalf("rollout did not finish: %s", desc)
						}

						// Liveness: a partition held above the user partition is either
						// still replacing an old group or is held by the floor. Updated
						// Ready groups below the current partition only count as credit
						// when everything above them is updated and Ready (see
						// rollingUpdatePartitionWithFloor), so skip states that depend on
						// that credit.
						if got > user {
							oldAbove := false
							for idx := got; int(idx) < n; idx++ {
								if !states[idx].updated {
									oldAbove = true
									break
								}
							}
							updatedReadyBelowCurrent := false
							for idx := user; idx < current; idx++ {
								if states[idx].ready && states[idx].updated {
									updatedReadyBelowCurrent = true
									break
								}
							}
							if !oldAbove && after > floor && !updatedReadyBelowCurrent {
								t.Fatalf("partition held without reason: %s", desc)
							}
						}
					}
				}
			}
		}
	}
	if checked == 0 {
		t.Fatal("no states checked")
	}
}

// TestPreserveFloorDuringSurgeTrimStateSpace checks that surge trimming keeps
// the smallest replica count at or above the raw target that preserves
// min(floor, reachable Ready groups).
func TestPreserveFloorDuringSurgeTrimStateSpace(t *testing.T) {
	kinds := []replicaState{{ready: false}, {ready: true}}
	for n := 1; n <= 7; n++ {
		states := make([]replicaState, n)
		for code := 0; code < 1<<n; code++ {
			for idx := range states {
				states[idx] = kinds[(code>>idx)&1]
			}
			for stsReplicas := int32(0); stsReplicas <= int32(n)+1; stsReplicas++ {
				maxReplicas := min(stsReplicas, int32(n))
				for raw := int32(0); raw <= maxReplicas; raw++ {
					for floor := int32(0); floor <= int32(n); floor++ {
						got := preserveFloorDuringSurgeTrim(states, raw, stsReplicas, floor)
						desc := fmt.Sprintf("states=%v raw=%d sts=%d floor=%d -> %d", states, raw, stsReplicas, floor, got)
						if got < raw || got > max(raw, maxReplicas) {
							t.Fatalf("target out of bounds: %s", desc)
						}
						goal := min(floor, countReadyStates(states[:maxReplicas]))
						if floor > 0 && countReadyStates(states[:got]) < goal {
							t.Fatalf("floor violated: %s", desc)
						}
						// Minimal: one fewer replica would drop below the goal.
						if got > raw && countReadyStates(states[:got-1]) >= goal {
							t.Fatalf("kept more surge replicas than needed: %s", desc)
						}
					}
				}
			}
		}
	}
}

func TestDeleteStaleOrdinalLeadersRespectsAvailabilityFloorOnCrash(t *testing.T) {
	ctx := context.Background()
	lws := wrappers.BuildBasicLeaderWorkerSet("test-sample", "default").
		Replica(5).
		Size(1).
		RolloutStrategy(leaderworkerset.RolloutStrategy{
			Type: leaderworkerset.RollingUpdateStrategyType,
			RollingUpdateConfiguration: &leaderworkerset.RollingUpdateConfiguration{
				Partition:      ptr.To[int32](0),
				MaxUnavailable: intstr.FromInt32(2),
				MaxSurge:       intstr.FromInt32(0),
			},
		}).Obj()
	lws.UID = "lws-uid"

	// Partition stepped from 4 -> 2 when baseline=4, maxUnavailable=2 (floor=2).
	// Before deleteStaleOrdinalLeaders ran, ordinal 0 crashed (unready), leaving
	// 3 Ready stale groups (ordinals 1, 2, 3) and Pending ordinal 4.
	sts := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:       lws.Name,
			Namespace:  lws.Namespace,
			UID:        "leader-sts-uid",
			Generation: 3,
			Labels: map[string]string{
				leaderworkerset.SetNameLabelKey: lws.Name,
				leaderworkerset.RevisionKey:     "rev-2",
				leaderworkerset.RoleLabelKey:    leaderworkerset.RoleLeader,
			},
			Annotations: map[string]string{
				leaderworkerset.ReplicasAnnotationKey: "5",
				rolloutBaselineAnnotationKey:          "4",
			},
		},
		Spec: appsv1.StatefulSetSpec{
			Replicas: ptr.To[int32](5),
			UpdateStrategy: appsv1.StatefulSetUpdateStrategy{
				Type:          appsv1.RollingUpdateStatefulSetStrategyType,
				RollingUpdate: &appsv1.RollingUpdateStatefulSetStrategy{Partition: ptr.To[int32](2)},
			},
		},
		Status: appsv1.StatefulSetStatus{
			ObservedGeneration: 3,
			Replicas:           5,
			CurrentRevision:    "sts-rev-1",
			UpdateRevision:     "sts-rev-2",
		},
	}

	makeLeader := func(ord int, rev string, ready bool) *corev1.Pod {
		objs := makeOwnedGroupObjects(lws, sts, ord, 1, rev, "sts-"+rev, ready, ready, ready)
		return objs[0].(*corev1.Pod)
	}

	c, r := newCombinedTestClient(t,
		lws, sts,
		makeLeader(0, "rev-1", false), // crashed below partition!
		makeLeader(1, "rev-1", true),
		makeLeader(2, "rev-1", true),
		makeLeader(3, "rev-1", true),
		makeLeader(4, "rev-2", false), // Pending scale-up ordinal
	)
	// Only 1 Ready stale leader (highest ordinal 3) may be deleted (3 -> 2 == floor);
	// ordinal 2 MUST remain protected!
	if _, err := r.deleteStaleOrdinalLeaders(ctx, lws, sts, "rev-2", 2, 5, 2); err != nil {
		t.Fatalf("deleteStaleOrdinalLeaders() error = %v", err)
	}
	var pod3, pod2 corev1.Pod
	if err := c.Get(ctx, types.NamespacedName{Name: "test-sample-3", Namespace: "default"}, &pod3); !apierrors.IsNotFound(err) {
		t.Fatalf("expected highest stale ordinal test-sample-3 to be deleted, got err=%v", err)
	}
	if err := c.Get(ctx, types.NamespacedName{Name: "test-sample-2", Namespace: "default"}, &pod2); err != nil {
		t.Fatalf("expected test-sample-2 to remain protected at floor=2, got err=%v", err)
	}
	// The deletion happens outside the StatefulSet controller, so it must leave
	// an event on the LWS.
	select {
	case event := <-r.Record.(*events.FakeRecorder).Events:
		if !strings.Contains(event, "Normal GroupsUpdating") || !strings.Contains(event, "test-sample-3") {
			t.Fatalf("unexpected event for stale leader deletion: %s", event)
		}
	default:
		t.Fatal("expected a GroupsUpdating event for the deleted stale leader test-sample-3")
	}

	// Re-running at survivingReady == floor (2) deletes nothing while Ready,
	// whereas if test-sample-2 itself becomes unready it can be neutral-repaired.
	if _, err := r.deleteStaleOrdinalLeaders(ctx, lws, sts, "rev-2", 2, 5, 2); err != nil {
		t.Fatalf("deleteStaleOrdinalLeaders(at floor) error = %v", err)
	}
	if err := c.Get(ctx, types.NamespacedName{Name: "test-sample-2", Namespace: "default"}, &pod2); err != nil {
		t.Fatalf("expected Ready test-sample-2 to stay protected on repeat pass: %v", err)
	}
}

func TestCombinedRolloutScaleDownNotDelayedByUnobservedStatefulSetGeneration(t *testing.T) {
	ctx := context.Background()
	lws := wrappers.BuildBasicLeaderWorkerSet("test-sample", "default").
		Replica(2).
		Size(1).
		RolloutStrategy(leaderworkerset.RolloutStrategy{
			Type: leaderworkerset.RollingUpdateStrategyType,
			RollingUpdateConfiguration: &leaderworkerset.RollingUpdateConfiguration{
				Partition:      ptr.To[int32](0),
				MaxUnavailable: intstr.FromInt32(1),
				MaxSurge:       intstr.FromInt32(1),
			},
		}).Obj()
	lws.UID = "lws-uid"

	// Previous pass started a rollout at replicas=4 (+1 surge = 5) in Generation 2,
	// while StatefulSetController has only observed Generation 1 so far.
	sts := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:       lws.Name,
			Namespace:  lws.Namespace,
			UID:        "leader-sts-uid",
			Generation: 2,
			Labels: map[string]string{
				leaderworkerset.SetNameLabelKey: lws.Name,
				leaderworkerset.RevisionKey:     "rev-2",
				leaderworkerset.RoleLabelKey:    leaderworkerset.RoleLeader,
			},
			Annotations: map[string]string{
				leaderworkerset.ReplicasAnnotationKey: "4",
				rolloutBaselineAnnotationKey:          "4",
			},
		},
		Spec: appsv1.StatefulSetSpec{
			Replicas: ptr.To[int32](5),
			UpdateStrategy: appsv1.StatefulSetUpdateStrategy{
				Type:          appsv1.RollingUpdateStatefulSetStrategyType,
				RollingUpdate: &appsv1.RollingUpdateStatefulSetStrategy{Partition: ptr.To[int32](4)},
			},
		},
		Status: appsv1.StatefulSetStatus{
			ObservedGeneration: 1,
			Replicas:           4,
		},
	}

	c, r := newCombinedTestClient(t, lws, sts)
	rev2CR, err := revisionutils.NewRevision(ctx, c, lws, "rev-2")
	if err != nil {
		t.Fatalf("NewRevision() error = %v", err)
	}
	if _, err := revisionutils.CreateRevision(ctx, c, rev2CR); err != nil {
		t.Fatalf("CreateRevision() error = %v", err)
	}

	req := reconcile.Request{NamespacedName: client.ObjectKeyFromObject(lws)}
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	var updatedSts appsv1.StatefulSet
	if err := c.Get(ctx, req.NamespacedName, &updatedSts); err != nil {
		t.Fatalf("reading sts: %v", err)
	}
	// Scale-down from desired 4 -> 2 (+1 surge = 3) must apply immediately rather
	// than leaving sts.Spec.Replicas at 5 while overwriting ReplicasAnnotationKey to "2".
	if got := *updatedSts.Spec.Replicas; got != 3 {
		t.Fatalf("sts.Spec.Replicas on immediate scale-down = %d, want 3", got)
	}
	if got := updatedSts.Annotations[leaderworkerset.ReplicasAnnotationKey]; got != "2" {
		t.Fatalf("ReplicasAnnotationKey = %q, want %q", got, "2")
	}
	if got := updatedSts.Annotations[rolloutBaselineAnnotationKey]; got != "2" {
		t.Fatalf("rolloutBaselineAnnotationKey = %q, want %q", got, "2")
	}
}

func TestResolveRolloutBaselineCorruptedAnnotationFallback(t *testing.T) {
	for _, bad := range []string{"not-an-int", "-3", "99999999999999999999"} {
		sts := &appsv1.StatefulSet{
			ObjectMeta: metav1.ObjectMeta{
				Annotations: map[string]string{
					rolloutBaselineAnnotationKey:          bad,
					leaderworkerset.ReplicasAnnotationKey: "3",
				},
			},
			Spec: appsv1.StatefulSetSpec{Replicas: ptr.To[int32](4)},
		}
		got, err := resolveRolloutBaseline(sts, 5)
		if err != nil {
			t.Fatalf("resolveRolloutBaseline(%q) unexpected error: %v", bad, err)
		}
		if got != 3 {
			t.Fatalf("resolveRolloutBaseline(%q) = %d, want 3", bad, got)
		}
	}
}

func TestHashCombinedRolloutAndScaleUpAvailabilityFloor(t *testing.T) {
	ctx := context.Background()
	lws := wrappers.BuildBasicLeaderWorkerSet("test-hash", "default").
		WorkerTemplateSpec(wrappers.MakeWorkerPodSpec()).
		Replica(1).
		Size(2).
		RolloutStrategy(leaderworkerset.RolloutStrategy{
			Type: leaderworkerset.RollingUpdateStrategyType,
			RollingUpdateConfiguration: &leaderworkerset.RollingUpdateConfiguration{
				MaxUnavailable: intstr.FromInt32(1),
				MaxSurge:       intstr.FromInt32(0),
			},
		}).Obj()
	lws.Spec.GroupIdentity = leaderworkerset.GroupIdentityHash
	lws.UID = "lws-hash-uid"
	lws.Generation = 1

	c, r := newCombinedTestClient(t, lws)
	req := reconcile.Request{NamespacedName: client.ObjectKeyFromObject(lws)}

	// Initial creation at replicas=1, rev1.
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("initial Reconcile() error = %v", err)
	}
	var deploy appsv1.Deployment
	if err := c.Get(ctx, req.NamespacedName, &deploy); err != nil {
		t.Fatalf("reading deploy: %v", err)
	}
	if _, hasBaseline := deploy.Annotations[rolloutBaselineAnnotationKey]; hasBaseline {
		t.Fatalf("unexpected baseline annotation on initial creation: %v", deploy.Annotations)
	}
	if got := deploy.Spec.Strategy.RollingUpdate.MaxUnavailable.IntVal; got != 1 {
		t.Fatalf("initial deploy maxUnavailable = %d, want 1", got)
	}

	// #717 in Hash mode: combined scale-up (1 -> 2) + worker template update (rev2)
	// with maxUnavailable=1, maxSurge=0 => baseline=1, floor=0, deploy maxUnavailable = 2 - 0 = 2.
	if err := c.Get(ctx, req.NamespacedName, lws); err != nil {
		t.Fatalf("reading lws: %v", err)
	}
	lws.Generation = 2
	lws.Spec.Replicas = ptr.To[int32](2)
	lws.Spec.LeaderWorkerTemplate.WorkerTemplate.Spec.Containers[0].Image = "worker:v2"
	if err := c.Update(ctx, lws); err != nil {
		t.Fatalf("updating lws: %v", err)
	}
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("combined 1->2 Reconcile() error = %v", err)
	}
	if err := c.Get(ctx, req.NamespacedName, &deploy); err != nil {
		t.Fatalf("reading deploy after 1->2 rollout: %v", err)
	}
	if got := deploy.Annotations[rolloutBaselineAnnotationKey]; got != "1" {
		t.Fatalf("deploy baseline annotation = %q, want %q", got, "1")
	}
	if got := deploy.Spec.Strategy.RollingUpdate.MaxUnavailable.IntVal; got != 2 {
		t.Fatalf("deploy maxUnavailable = %d, want 2 (desired 2 - floor 0)", got)
	}

	// Superseding template update (rev3) + scale-up to 4 with maxUnavailable=0, maxSurge=1:
	// baseline stays 1, floor = max(0, 1 - 0) = 1, deploy maxUnavailable = 4 - 1 = 3.
	if err := c.Get(ctx, req.NamespacedName, lws); err != nil {
		t.Fatalf("reading lws: %v", err)
	}
	lws.Generation = 3
	lws.Spec.Replicas = ptr.To[int32](4)
	lws.Spec.RolloutStrategy.RollingUpdateConfiguration.MaxUnavailable = intstr.FromInt32(0)
	lws.Spec.RolloutStrategy.RollingUpdateConfiguration.MaxSurge = intstr.FromInt32(1)
	lws.Spec.LeaderWorkerTemplate.WorkerTemplate.Spec.Containers[0].Image = "worker:v3"
	if err := c.Update(ctx, lws); err != nil {
		t.Fatalf("updating lws to rev3: %v", err)
	}
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("rev3 Reconcile() error = %v", err)
	}
	if err := c.Get(ctx, req.NamespacedName, &deploy); err != nil {
		t.Fatalf("reading deploy after rev3: %v", err)
	}
	if got := deploy.Annotations[rolloutBaselineAnnotationKey]; got != "1" {
		t.Fatalf("deploy baseline annotation on superseding update = %q, want %q", got, "1")
	}
	if got := deploy.Spec.Strategy.RollingUpdate.MaxUnavailable.IntVal; got != 3 {
		t.Fatalf("deploy maxUnavailable on 4 replicas with floor 1 = %d, want 3", got)
	}

	// Converge all 4 groups on rev3 -> clears baseline annotation and restores configured maxUnavailable=0!
	rev3 := revisionutils.GetRevisionKey(&deploy)
	deploy.Generation = 3
	if err := c.Update(ctx, &deploy); err != nil {
		t.Fatalf("updating deploy generation: %v", err)
	}
	deploy.Status = appsv1.DeploymentStatus{
		ObservedGeneration: 3,
		Replicas:           4,
		UpdatedReplicas:    4,
		ReadyReplicas:      4,
	}
	if err := c.Status().Update(ctx, &deploy); err != nil {
		t.Fatalf("updating deploy status: %v", err)
	}
	for i := range 4 {
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      fmt.Sprintf("hash-leader-%d", i),
				Namespace: lws.Namespace,
				Labels: map[string]string{
					leaderworkerset.SetNameLabelKey:     lws.Name,
					leaderworkerset.WorkerIndexLabelKey: "0",
					leaderworkerset.GroupIndexLabelKey:  fmt.Sprintf("hash-%d", i),
					leaderworkerset.RevisionKey:         rev3,
				},
			},
			Status: corev1.PodStatus{
				Phase:      corev1.PodRunning,
				Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}},
			},
		}
		if err := c.Create(ctx, pod); err != nil {
			t.Fatalf("creating ready hash leader %d: %v", i, err)
		}
	}

	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("convergence Reconcile() error = %v", err)
	}
	if err := c.Get(ctx, req.NamespacedName, &deploy); err != nil {
		t.Fatalf("reading converged deploy: %v", err)
	}
	if _, hasBaseline := deploy.Annotations[rolloutBaselineAnnotationKey]; hasBaseline {
		t.Fatalf("expected %s to be cleared on convergence, got %v", rolloutBaselineAnnotationKey, deploy.Annotations)
	}
	if got := deploy.Spec.Strategy.RollingUpdate.MaxUnavailable.IntVal; got != 0 {
		t.Fatalf("converged deploy maxUnavailable = %d, want restored 0", got)
	}
}

func TestRolloutBaselineActive(t *testing.T) {
	old := replicaState{ready: true}
	oldDown := replicaState{}
	updated := replicaState{ready: true, updated: true}
	updatedDown := replicaState{updated: true}

	for _, tc := range []struct {
		name                     string
		states                   []replicaState
		partition, userPartition int32
		lwsReplicas              int32
		want                     bool
	}{
		{
			name:        "partition still protects old groups",
			states:      []replicaState{old, updated},
			partition:   1,
			lwsReplicas: 2,
			want:        true,
		},
		{
			name:        "a desired group still runs an older revision",
			states:      []replicaState{oldDown, updated},
			lwsReplicas: 2,
			want:        true,
		},
		{
			name:        "every desired group runs the target revision, one unready",
			states:      []replicaState{updated, updatedDown, updated},
			lwsReplicas: 3,
			want:        false,
		},
		{
			name:        "surge ordinal beyond spec.replicas is ignored",
			states:      []replicaState{updated, updated, oldDown},
			lwsReplicas: 2,
			want:        false,
		},
		{
			name:          "user partition holds old groups while an addition above it is Pending",
			states:        []replicaState{old, old, updated, updatedDown},
			partition:     2,
			userPartition: 2,
			lwsReplicas:   4,
			want:          true,
		},
		{
			name:          "user partition holds old groups and every group above it is Ready",
			states:        []replicaState{old, old, updated, updated},
			partition:     2,
			userPartition: 2,
			lwsReplicas:   4,
			want:          false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := rolloutBaselineActive(tc.states, tc.partition, tc.userPartition, tc.lwsReplicas); got != tc.want {
				t.Errorf("rolloutBaselineActive() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestCombinedRolloutUnreadyGroupDoesNotLeaveBaselineForNextRollout verifies
// that a group still unready after a combined rollout reaches the target
// revision does not keep rollout-baseline-replicas, so the next template update
// is paced against a fresh baseline instead of the smaller pre-scale-up one.
func TestCombinedRolloutUnreadyGroupDoesNotLeaveBaselineForNextRollout(t *testing.T) {
	ctx := context.Background()
	lws := wrappers.BuildBasicLeaderWorkerSet("test-sample", "default").
		WorkerTemplateSpec(wrappers.MakeWorkerPodSpec()).
		Replica(2).
		Size(1).
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

	c, _ := newCombinedTestClient(t, lws)
	r := NewLeaderWorkerSetReconciler(c, lwsStatusScheme(t), events.NewFakeRecorder(256))
	req := reconcile.Request{NamespacedName: client.ObjectKeyFromObject(lws)}

	reconcileAndAck := func(step string) *appsv1.StatefulSet {
		t.Helper()
		if _, err := r.Reconcile(ctx, req); err != nil {
			t.Fatalf("%s: Reconcile() error = %v", step, err)
		}
		return ackLeaderStatefulSet(t, ctx, c, req.NamespacedName)
	}
	createLeader := func(sts *appsv1.StatefulSet, ord int, rev string, ready bool) {
		t.Helper()
		for _, obj := range makeOwnedGroupObjects(lws, sts, ord, 1, rev, "sts-"+rev, ready, ready, ready) {
			if err := c.Create(ctx, obj); err != nil {
				t.Fatalf("creating leader %d on %s: %v", ord, rev, err)
			}
		}
	}
	deleteLeader := func(ord int) {
		t.Helper()
		pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("%s-%d", lws.Name, ord), Namespace: lws.Namespace}}
		if err := c.Delete(ctx, pod); err != nil {
			t.Fatalf("deleting leader %d: %v", ord, err)
		}
	}
	updateLWS := func(replicas int32, image string) {
		t.Helper()
		if err := c.Get(ctx, req.NamespacedName, lws); err != nil {
			t.Fatalf("reading lws: %v", err)
		}
		lws.Generation++
		lws.Spec.Replicas = ptr.To(replicas)
		lws.Spec.LeaderWorkerTemplate.WorkerTemplate.Spec.Containers[0].Image = image
		if err := c.Update(ctx, lws); err != nil {
			t.Fatalf("updating lws: %v", err)
		}
	}
	expect := func(step string, sts *appsv1.StatefulSet, wantPartition int32, wantBaseline string) {
		t.Helper()
		if got := *sts.Spec.UpdateStrategy.RollingUpdate.Partition; got != wantPartition {
			t.Fatalf("%s: partition = %d, want %d", step, got, wantPartition)
		}
		got, found := sts.Annotations[rolloutBaselineAnnotationKey]
		if wantBaseline == "" && found {
			t.Fatalf("%s: unexpected %s=%q", step, rolloutBaselineAnnotationKey, got)
		}
		if wantBaseline != "" && got != wantBaseline {
			t.Fatalf("%s: %s = %q, want %q", step, rolloutBaselineAnnotationKey, got, wantBaseline)
		}
	}

	sts := reconcileAndAck("create")
	rev1 := revisionutils.GetRevisionKey(sts)
	createLeader(sts, 0, rev1, true)
	createLeader(sts, 1, rev1, true)

	// Combined scale-up (2 -> 4) and template update: baseline 2, floor 1.
	updateLWS(4, "worker:v2")
	sts = reconcileAndAck("combined update")
	rev2 := revisionutils.GetRevisionKey(sts)
	expect("combined update", sts, 2, "2")

	// Both additions are Ready, so both old groups are exposed.
	createLeader(sts, 2, rev2, true)
	createLeader(sts, 3, rev2, true)
	sts = reconcileAndAck("additions ready")
	expect("additions ready", sts, 0, "2")

	// The old groups are replaced on rev2, but group 1 stays unready.
	deleteLeader(0)
	deleteLeader(1)
	createLeader(sts, 0, rev2, true)
	createLeader(sts, 1, rev2, false)
	sts = reconcileAndAck("revision converged with an unready group")
	expect("revision converged with an unready group", sts, 0, "")

	// The next template update starts from a fresh baseline of 4 (floor 3).
	// Group 1 is already unavailable, so no Ready group may be replaced.
	updateLWS(4, "worker:v3")
	sts = reconcileAndAck("next update")
	expect("next update", sts, 4, "4")
	sts = reconcileAndAck("next update, next pass")
	expect("next update, next pass", sts, 4, "4")
}

// TestCombinedRolloutPlainScaleDownWithUnreadyGroupRecordsNoBaseline verifies
// that a replica-only scale-down does not start a rollout baseline because a
// surviving group is unready, including when a surge replica is kept for it.
func TestCombinedRolloutPlainScaleDownWithUnreadyGroupRecordsNoBaseline(t *testing.T) {
	for _, tc := range []struct {
		name                     string
		maxUnavailable, maxSurge int32
		wantReplicas             int32
	}{
		{name: "maxUnavailable=1, maxSurge=0", maxUnavailable: 1, maxSurge: 0, wantReplicas: 4},
		{name: "maxUnavailable=0, maxSurge=1 keeps a surge replica", maxUnavailable: 0, maxSurge: 1, wantReplicas: 5},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lws := wrappers.BuildBasicLeaderWorkerSet("test-sample", "default").
				Replica(4).
				Size(1).
				RolloutStrategy(leaderworkerset.RolloutStrategy{
					Type: leaderworkerset.RollingUpdateStrategyType,
					RollingUpdateConfiguration: &leaderworkerset.RollingUpdateConfiguration{
						Partition:      ptr.To[int32](0),
						MaxUnavailable: intstr.FromInt32(tc.maxUnavailable),
						MaxSurge:       intstr.FromInt32(tc.maxSurge),
					},
				}).Obj()
			sts := &appsv1.StatefulSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:        lws.Name,
					Namespace:   lws.Namespace,
					Annotations: map[string]string{leaderworkerset.ReplicasAnnotationKey: "6"},
				},
				Spec: appsv1.StatefulSetSpec{
					Replicas: ptr.To[int32](6),
					UpdateStrategy: appsv1.StatefulSetUpdateStrategy{
						Type:          appsv1.RollingUpdateStatefulSetStrategyType,
						RollingUpdate: &appsv1.RollingUpdateStatefulSetStrategy{Partition: ptr.To[int32](0)},
					},
				},
			}
			var objs []client.Object
			for i := 0; i < 6; i++ {
				objs = append(objs, lwsStatusLeaderPod(lws, i, "rev-1", i != 1))
			}
			r, _ := lwsStatusNewReconciler(t, objs...)

			plan, err := r.planRollingUpdate(context.Background(), lws, sts, "rev-1", false)
			if err != nil {
				t.Fatalf("planRollingUpdate() error = %v", err)
			}
			if plan.baseline != nil {
				t.Fatalf("baseline = %d, want none for a replica-only scale-down", *plan.baseline)
			}
			if plan.replicas != tc.wantReplicas {
				t.Fatalf("replicas = %d, want %d", plan.replicas, tc.wantReplicas)
			}
		})
	}
}

func TestIsHashRolloutConvergedIgnoresReadiness(t *testing.T) {
	lws := wrappers.BuildBasicLeaderWorkerSet("test-hash", "default").Replica(2).Size(1).Obj()
	lws.Spec.GroupIdentity = leaderworkerset.GroupIdentityHash

	leader := func(name, revisionKey string, ready bool) *corev1.Pod {
		pod := lwsStatusLeaderPod(lws, 0, revisionKey, ready)
		pod.Name = name
		pod.Labels[leaderworkerset.GroupIndexLabelKey] = name
		return pod
	}
	// An exhausted group is terminated and held by finalizers while its
	// replacement waits behind the group-replacement gate.
	exhausted := leader("exhausted", "rev-2", true)
	exhausted.Annotations = map[string]string{leaderworkerset.GroupRestartBudgetExhaustedAnnotationKey: "true"}
	exhausted.Finalizers = []string{leaderworkerset.GroupRestartBudgetCleanupFinalizer}
	exhausted.DeletionTimestamp = ptr.To(metav1.Now())
	converged := appsv1.DeploymentStatus{ObservedGeneration: 2, Replicas: 2, UpdatedReplicas: 2, ReadyReplicas: 2}
	withStatus := func(mutate func(*appsv1.DeploymentStatus)) appsv1.DeploymentStatus {
		status := converged
		mutate(&status)
		return status
	}

	for _, tc := range []struct {
		name   string
		status appsv1.DeploymentStatus
		pods   []client.Object
		want   bool
	}{
		{
			name:   "every leader Ready",
			status: converged,
			pods:   []client.Object{leader("a", "rev-2", true), leader("b", "rev-2", true)},
			want:   true,
		},
		{
			name:   "one leader unready",
			status: withStatus(func(s *appsv1.DeploymentStatus) { s.ReadyReplicas = 1 }),
			pods:   []client.Object{leader("a", "rev-2", true), leader("b", "rev-2", false)},
			want:   true,
		},
		{
			name:   "one group exhausted",
			status: withStatus(func(s *appsv1.DeploymentStatus) { s.ReadyReplicas = 1 }),
			pods:   []client.Object{leader("a", "rev-2", true), exhausted, leader("replacement", "rev-2", false)},
			want:   true,
		},
		{
			name:   "an old-revision leader is still running",
			status: converged,
			pods:   []client.Object{leader("a", "rev-2", true), leader("old", "rev-1", true)},
			want:   false,
		},
		{
			name:   "Deployment has not observed its generation",
			status: withStatus(func(s *appsv1.DeploymentStatus) { s.ObservedGeneration = 1 }),
			pods:   []client.Object{leader("a", "rev-2", true), leader("b", "rev-2", true)},
			want:   false,
		},
		{
			name:   "old ReplicaSet still has pods",
			status: withStatus(func(s *appsv1.DeploymentStatus) { s.UpdatedReplicas = 1 }),
			pods:   []client.Object{leader("a", "rev-2", true), leader("b", "rev-2", true)},
			want:   false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			deploy := &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:       lws.Name,
					Namespace:  lws.Namespace,
					Generation: 2,
					Labels:     map[string]string{leaderworkerset.RevisionKey: "rev-2"},
				},
				Spec:   appsv1.DeploymentSpec{Replicas: ptr.To[int32](2)},
				Status: tc.status,
			}
			pods := make([]client.Object, 0, len(tc.pods))
			for _, pod := range tc.pods {
				pods = append(pods, pod.DeepCopyObject().(client.Object))
			}
			r, _ := lwsStatusNewReconciler(t, pods...)
			got, err := r.isHashRolloutConverged(context.Background(), lws, deploy, "rev-2")
			if err != nil {
				t.Fatalf("isHashRolloutConverged() error = %v", err)
			}
			if got != tc.want {
				t.Errorf("isHashRolloutConverged() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestHashUnreadyGroupDoesNotLeaveBaselineForNextRollout verifies that a Hash
// group still unready after the leader Deployment reaches the target revision
// does not carry rollout-baseline-replicas or the widened maxUnavailable into
// the next template update.
func TestHashUnreadyGroupDoesNotLeaveBaselineForNextRollout(t *testing.T) {
	ctx := context.Background()
	lws := wrappers.BuildBasicLeaderWorkerSet("test-hash", "default").
		WorkerTemplateSpec(wrappers.MakeWorkerPodSpec()).
		Replica(1).
		Size(2).
		RolloutStrategy(leaderworkerset.RolloutStrategy{
			Type: leaderworkerset.RollingUpdateStrategyType,
			RollingUpdateConfiguration: &leaderworkerset.RollingUpdateConfiguration{
				MaxUnavailable: intstr.FromInt32(1),
				MaxSurge:       intstr.FromInt32(0),
			},
		}).Obj()
	lws.Spec.GroupIdentity = leaderworkerset.GroupIdentityHash
	lws.UID = "lws-hash-uid"
	lws.Generation = 1

	c, r := newCombinedTestClient(t, lws)
	req := reconcile.Request{NamespacedName: client.ObjectKeyFromObject(lws)}
	var deploy appsv1.Deployment

	reconcileAndGet := func(step string) {
		t.Helper()
		if _, err := r.Reconcile(ctx, req); err != nil {
			t.Fatalf("%s: Reconcile() error = %v", step, err)
		}
		if err := c.Get(ctx, req.NamespacedName, &deploy); err != nil {
			t.Fatalf("%s: reading deploy: %v", step, err)
		}
	}
	updateLWS := func(replicas int32, image string) {
		t.Helper()
		if err := c.Get(ctx, req.NamespacedName, lws); err != nil {
			t.Fatalf("reading lws: %v", err)
		}
		lws.Generation++
		lws.Spec.Replicas = ptr.To(replicas)
		lws.Spec.LeaderWorkerTemplate.WorkerTemplate.Spec.Containers[0].Image = image
		if err := c.Update(ctx, lws); err != nil {
			t.Fatalf("updating lws: %v", err)
		}
	}
	expect := func(step, wantBaseline string, wantMaxUnavailable int32) {
		t.Helper()
		got, found := deploy.Annotations[rolloutBaselineAnnotationKey]
		if wantBaseline == "" && found {
			t.Fatalf("%s: unexpected %s=%q", step, rolloutBaselineAnnotationKey, got)
		}
		if wantBaseline != "" && got != wantBaseline {
			t.Fatalf("%s: %s = %q, want %q", step, rolloutBaselineAnnotationKey, got, wantBaseline)
		}
		if got := deploy.Spec.Strategy.RollingUpdate.MaxUnavailable.IntVal; got != wantMaxUnavailable {
			t.Fatalf("%s: deploy maxUnavailable = %d, want %d", step, got, wantMaxUnavailable)
		}
	}

	reconcileAndGet("create")

	// Combined scale-up (1 -> 2) and template update: baseline 1, floor 0.
	updateLWS(2, "worker:v2")
	reconcileAndGet("combined update")
	expect("combined update", "1", 2)

	// Every leader runs rev2, but one group is still unready.
	rev2 := revisionutils.GetRevisionKey(&deploy)
	deploy.Generation = 2
	if err := c.Update(ctx, &deploy); err != nil {
		t.Fatalf("updating deploy generation: %v", err)
	}
	deploy.Status = appsv1.DeploymentStatus{ObservedGeneration: 2, Replicas: 2, UpdatedReplicas: 2, ReadyReplicas: 1}
	if err := c.Status().Update(ctx, &deploy); err != nil {
		t.Fatalf("updating deploy status: %v", err)
	}
	for i, ready := range []bool{true, false} {
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      fmt.Sprintf("hash-leader-%d", i),
				Namespace: lws.Namespace,
				Labels: map[string]string{
					leaderworkerset.SetNameLabelKey:     lws.Name,
					leaderworkerset.WorkerIndexLabelKey: "0",
					leaderworkerset.GroupIndexLabelKey:  fmt.Sprintf("hash-%d", i),
					leaderworkerset.RevisionKey:         rev2,
				},
			},
		}
		if ready {
			pod.Status = corev1.PodStatus{
				Phase:      corev1.PodRunning,
				Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}},
			}
		}
		if err := c.Create(ctx, pod); err != nil {
			t.Fatalf("creating hash leader %d: %v", i, err)
		}
	}
	reconcileAndGet("revision converged with an unready group")
	expect("revision converged with an unready group", "", 1)

	// The next template update starts from a fresh baseline of 2, so the
	// configured maxUnavailable is not widened.
	updateLWS(2, "worker:v3")
	reconcileAndGet("next update")
	expect("next update", "2", 1)
}

// TestOrdinalRollbackBeforePodCacheObservesDeletePreservesFloor reproduces the
// stale-cache rollback race: A0 (rev A) and B1 (rev B) are Ready, B2 (rev B) is
// Pending, and floor is 1. After LWS deletes A0, the user rolls back to rev A
// before the Pod informer observes A0's deletion. The controller must withhold
// A0's readiness credit and keep B1 protected until A0's replacement is Ready.
func TestOrdinalRollbackBeforePodCacheObservesDeletePreservesFloor(t *testing.T) {
	ctx := context.Background()
	initialWorkerSpec := wrappers.MakeWorkerPodSpec()
	initialWorkerImage := initialWorkerSpec.Containers[0].Image

	lws := wrappers.BuildBasicLeaderWorkerSet("test-sample", "default").
		WorkerTemplateSpec(initialWorkerSpec).
		Replica(1).
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

	swallowA0Delete := true
	var deletedPods []string
	c, r := newCombinedTestClientWithInterceptor(t, interceptor.Funcs{
		Delete: func(ctx context.Context, cli client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			pod, ok := obj.(*corev1.Pod)
			if ok {
				deletedPods = append(deletedPods, pod.Name)
				if pod.Name == "test-sample-0" && swallowA0Delete {
					// Simulate API server accepting DELETE A0 while the local Pod
					// informer cache has not yet received the watch update.
					return nil
				}
			}
			return cli.Delete(ctx, obj, opts...)
		},
	}, lws)
	req := reconcile.Request{NamespacedName: client.ObjectKeyFromObject(lws)}

	// Step 1: Create initial LWS at replicas=1 on rev A with Ready group A0.
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("initial Reconcile() error = %v", err)
	}
	sts := ackLeaderStatefulSet(t, ctx, c, req.NamespacedName)
	revA := revisionutils.GetRevisionKey(sts)
	for _, obj := range makeOwnedGroupObjects(lws, sts, 0, 2, revA, "sts-"+revA, true, true, true) {
		if err := c.Create(ctx, obj); err != nil {
			t.Fatalf("creating A0 object: %v", err)
		}
	}

	// Step 2: Combined scale-up (1 -> 3) + rollout to rev B (baseline=1, floor=1).
	if err := c.Get(ctx, req.NamespacedName, lws); err != nil {
		t.Fatalf("reading lws: %v", err)
	}
	lws.Generation = 2
	lws.Spec.Replicas = ptr.To[int32](3)
	lws.Spec.LeaderWorkerTemplate.WorkerTemplate.Spec.Containers[0].Image = "worker:v2"
	if err := c.Update(ctx, lws); err != nil {
		t.Fatalf("updating lws to rev B: %v", err)
	}
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("rev B publish Reconcile() error = %v", err)
	}
	sts = ackLeaderStatefulSet(t, ctx, c, req.NamespacedName)
	revB := revisionutils.GetRevisionKey(sts)

	// B1 is whole-group Ready on rev B; B2 remains Pending on rev B.
	for _, obj := range makeOwnedGroupObjects(lws, sts, 1, 2, revB, "sts-"+revB, true, true, true) {
		if err := c.Create(ctx, obj); err != nil {
			t.Fatalf("creating B1 object: %v", err)
		}
	}
	for _, obj := range makeOwnedGroupObjects(lws, sts, 2, 2, revB, "sts-"+revB, false, false, false) {
		if err := c.Create(ctx, obj); err != nil {
			t.Fatalf("creating B2 object: %v", err)
		}
	}

	// Step 3: B1 grants replacement credit for A0: partition steps 1 -> 0, then
	// deleteStaleOrdinalLeaders sends DELETE A0 (cache still shows A0 Ready).
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("partition-drop Reconcile() error = %v", err)
	}
	sts = ackLeaderStatefulSet(t, ctx, c, req.NamespacedName)
	if got := *sts.Spec.UpdateStrategy.RollingUpdate.Partition; got != 0 {
		t.Fatalf("partition on rev B = %d, want 0", got)
	}
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("delete-A0 Reconcile() error = %v", err)
	}
	if len(deletedPods) != 1 || deletedPods[0] != "test-sample-0" {
		t.Fatalf("deletedPods = %v, want [test-sample-0]", deletedPods)
	}

	// Step 4: Roll back LWS to rev A before the Pod cache observes A0's deletion.
	if err := c.Get(ctx, req.NamespacedName, lws); err != nil {
		t.Fatalf("reading lws for rollback: %v", err)
	}
	lws.Generation = 3
	lws.Spec.LeaderWorkerTemplate.WorkerTemplate.Spec.Containers[0].Image = initialWorkerImage
	if err := c.Update(ctx, lws); err != nil {
		t.Fatalf("updating lws for rollback: %v", err)
	}

	// Publish rollback template (partition raised to 3), ack, then advance rollback.
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("rollback publish Reconcile() error = %v", err)
	}
	sts = ackLeaderStatefulSet(t, ctx, c, req.NamespacedName)
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("rollback partition Reconcile() error = %v", err)
	}
	sts = ackLeaderStatefulSet(t, ctx, c, req.NamespacedName)
	if got := *sts.Spec.UpdateStrategy.RollingUpdate.Partition; got != 2 {
		t.Fatalf("rollback partition with unobserved A0 delete = %d, want 2 (protecting B1)", got)
	}
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("rollback delete-check Reconcile() error = %v", err)
	}

	var b1Leader corev1.Pod
	if err := c.Get(ctx, types.NamespacedName{Name: "test-sample-1", Namespace: "default"}, &b1Leader); err != nil {
		t.Fatalf("expected Ready B1 to remain protected at floor=1, got err=%v", err)
	}
	if err := c.Get(ctx, req.NamespacedName, lws); err != nil {
		t.Fatalf("reading lws status: %v", err)
	}
	if apimeta.IsStatusConditionTrue(lws.Status.Conditions, string(leaderworkerset.LeaderWorkerSetAvailable)) {
		t.Fatalf("expected Available=False while A0 delete is pending and B2 is Pending, got %+v", lws.Status.Conditions)
	}

	// Step 5: Informer observes A0 replaced by Pending A0' (new UID on rev A).
	// B1 must remain protected while A0' is Pending, then become replaceable once A0' is Ready.
	swallowA0Delete = false
	for _, name := range []string{"test-sample-0", "test-sample-0-1"} {
		if err := c.Delete(ctx, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"}}); err != nil {
			t.Fatalf("removing old %s: %v", name, err)
		}
	}
	if err := c.Delete(ctx, &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: "test-sample-0", Namespace: "default"}}); err != nil {
		t.Fatalf("removing old worker sts 0: %v", err)
	}
	replacementA0 := makeOwnedGroupObjects(lws, sts, 0, 2, revA, "sts-"+revA, false, false, false)
	replacementA0[0].(*corev1.Pod).UID = "test-sample-0-uid-recreated"
	replacementA0[1].(*appsv1.StatefulSet).OwnerReferences[0].UID = "test-sample-0-uid-recreated"
	for _, obj := range replacementA0 {
		if err := c.Create(ctx, obj); err != nil {
			t.Fatalf("creating Pending replacement A0': %v", err)
		}
	}

	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("Reconcile() with Pending A0' error = %v", err)
	}
	sts = ackLeaderStatefulSet(t, ctx, c, req.NamespacedName)
	if got := *sts.Spec.UpdateStrategy.RollingUpdate.Partition; got != 2 {
		t.Fatalf("partition while replacement A0' is Pending = %d, want 2", got)
	}

	// Mark A0' whole-group Ready -> partition drops to 0 and B1 can now be replaced.
	a0Pod := replacementA0[0].(*corev1.Pod).DeepCopy()
	if err := c.Get(ctx, client.ObjectKeyFromObject(a0Pod), a0Pod); err != nil {
		t.Fatalf("getting A0': %v", err)
	}
	a0Pod.Status = corev1.PodStatus{
		Phase:      corev1.PodRunning,
		Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}},
	}
	if err := c.Status().Update(ctx, a0Pod); err != nil {
		t.Fatalf("marking A0' leader Ready: %v", err)
	}
	var a0WorkerSts appsv1.StatefulSet
	if err := c.Get(ctx, types.NamespacedName{Name: "test-sample-0", Namespace: "default"}, &a0WorkerSts); err != nil {
		t.Fatalf("getting A0' worker sts: %v", err)
	}
	a0WorkerSts.Status.ReadyReplicas = 1
	a0WorkerSts.Status.AvailableReplicas = 1
	if err := c.Status().Update(ctx, &a0WorkerSts); err != nil {
		t.Fatalf("marking A0' worker sts Ready: %v", err)
	}
	var a0WorkerPod corev1.Pod
	if err := c.Get(ctx, types.NamespacedName{Name: "test-sample-0-1", Namespace: "default"}, &a0WorkerPod); err != nil {
		t.Fatalf("getting A0' worker pod: %v", err)
	}
	a0WorkerPod.Status = corev1.PodStatus{
		Phase:      corev1.PodRunning,
		Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}},
	}
	if err := c.Status().Update(ctx, &a0WorkerPod); err != nil {
		t.Fatalf("marking A0' worker pod Ready: %v", err)
	}

	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("Reconcile() after A0' Ready error = %v", err)
	}
	sts = ackLeaderStatefulSet(t, ctx, c, req.NamespacedName)
	if got := *sts.Spec.UpdateStrategy.RollingUpdate.Partition; got != 1 {
		t.Fatalf("partition after replacement A0' is Ready = %d, want 1 (exposing B1 at ord 1)", got)
	}
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("Reconcile() replacing B1 error = %v", err)
	}
	if err := c.Get(ctx, types.NamespacedName{Name: "test-sample-1", Namespace: "default"}, &b1Leader); !apierrors.IsNotFound(err) {
		t.Fatalf("expected B1 to be deleted once A0' satisfies floor=1, got err=%v", err)
	}
}

// TestOrdinalTimedOutDeleteAndRollbackResolvesWithoutFloorViolation verifies
// that an ambiguous/timed-out DELETE on A0 is never forgotten across rollback:
// it either replays to deterministic completion while holding B1 protected, or
// clears cleanly if the informer observes A0 at a newer ResourceVersion without DeletionTimestamp.
func TestOrdinalTimedOutDeleteAndRollbackResolvesWithoutFloorViolation(t *testing.T) {
	t.Run("replay_confirms_delete_after_rollback", func(t *testing.T) {
		ctx := context.Background()
		initialWorkerSpec := wrappers.MakeWorkerPodSpec()
		initialWorkerImage := initialWorkerSpec.Containers[0].Image

		lws := wrappers.BuildBasicLeaderWorkerSet("test-sample", "default").
			WorkerTemplateSpec(initialWorkerSpec).
			Replica(1).
			Size(1).
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

		deleteAttempts := 0
		var recordedPreconditions []*metav1.Preconditions
		c, r := newCombinedTestClientWithInterceptor(t, interceptor.Funcs{
			Delete: func(ctx context.Context, cli client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
				pod, ok := obj.(*corev1.Pod)
				if ok && pod.Name == "test-sample-0" {
					deleteAttempts++
					delOpts := &client.DeleteOptions{}
					delOpts.ApplyOptions(opts)
					recordedPreconditions = append(recordedPreconditions, delOpts.Preconditions)
					if deleteAttempts == 1 {
						return context.DeadlineExceeded
					}
					// Replay succeeds on API server while local cache still lags.
					return nil
				}
				return cli.Delete(ctx, obj, opts...)
			},
		}, lws)
		req := reconcile.Request{NamespacedName: client.ObjectKeyFromObject(lws)}

		if _, err := r.Reconcile(ctx, req); err != nil {
			t.Fatalf("initial Reconcile() error = %v", err)
		}
		sts := ackLeaderStatefulSet(t, ctx, c, req.NamespacedName)
		revA := revisionutils.GetRevisionKey(sts)
		for _, obj := range makeOwnedGroupObjects(lws, sts, 0, 1, revA, "sts-"+revA, true, true, true) {
			if err := c.Create(ctx, obj); err != nil {
				t.Fatalf("creating A0: %v", err)
			}
		}

		// Scale 1 -> 3 + update to rev B; B1 Ready, B2 Pending; step partition to 0.
		if err := c.Get(ctx, req.NamespacedName, lws); err != nil {
			t.Fatalf("reading lws: %v", err)
		}
		lws.Generation = 2
		lws.Spec.Replicas = ptr.To[int32](3)
		lws.Spec.LeaderWorkerTemplate.WorkerTemplate.Spec.Containers[0].Image = "worker:v2"
		if err := c.Update(ctx, lws); err != nil {
			t.Fatalf("updating lws: %v", err)
		}
		if _, err := r.Reconcile(ctx, req); err != nil {
			t.Fatalf("rev B Reconcile() error = %v", err)
		}
		sts = ackLeaderStatefulSet(t, ctx, c, req.NamespacedName)
		revB := revisionutils.GetRevisionKey(sts)
		for _, obj := range makeOwnedGroupObjects(lws, sts, 1, 1, revB, "sts-"+revB, true, true, true) {
			if err := c.Create(ctx, obj); err != nil {
				t.Fatalf("creating B1: %v", err)
			}
		}
		for _, obj := range makeOwnedGroupObjects(lws, sts, 2, 1, revB, "sts-"+revB, false, false, false) {
			if err := c.Create(ctx, obj); err != nil {
				t.Fatalf("creating B2: %v", err)
			}
		}
		if _, err := r.Reconcile(ctx, req); err != nil {
			t.Fatalf("partition-drop Reconcile() error = %v", err)
		}
		ackLeaderStatefulSet(t, ctx, c, req.NamespacedName)

		// Initial DELETE(A0) times out.
		if _, err := r.Reconcile(ctx, req); err == nil {
			t.Fatal("expected timeout error from initial DELETE(A0)")
		}

		// User immediately rolls back to rev A while DELETE(A0) outcome is uncertain.
		if err := c.Get(ctx, req.NamespacedName, lws); err != nil {
			t.Fatalf("reading lws: %v", err)
		}
		lws.Generation = 3
		lws.Spec.LeaderWorkerTemplate.WorkerTemplate.Spec.Containers[0].Image = initialWorkerImage
		if err := c.Update(ctx, lws); err != nil {
			t.Fatalf("rolling back lws: %v", err)
		}
		res, err := r.Reconcile(ctx, req)
		if err != nil {
			t.Fatalf("rollback publish Reconcile() error = %v", err)
		}
		if res.RequeueAfter != pendingDeleteRequeueDelay {
			t.Fatalf("Reconcile() RequeueAfter = %v, want %v while delete is uncertain", res.RequeueAfter, pendingDeleteRequeueDelay)
		}
		ackLeaderStatefulSet(t, ctx, c, req.NamespacedName)

		// Next reconcile replays conditional DELETE(A0), confirms it, and keeps B1 protected.
		if _, err := r.Reconcile(ctx, req); err != nil {
			t.Fatalf("rollback replay Reconcile() error = %v", err)
		}
		sts = ackLeaderStatefulSet(t, ctx, c, req.NamespacedName)
		if deleteAttempts != 2 {
			t.Fatalf("deleteAttempts = %d, want 2 (initial + replay)", deleteAttempts)
		}
		if len(recordedPreconditions) != 2 ||
			recordedPreconditions[0] == nil || recordedPreconditions[1] == nil ||
			recordedPreconditions[0].UID == nil || recordedPreconditions[1].UID == nil ||
			*recordedPreconditions[0].UID != *recordedPreconditions[1].UID ||
			recordedPreconditions[0].ResourceVersion == nil || recordedPreconditions[1].ResourceVersion == nil ||
			*recordedPreconditions[0].ResourceVersion != *recordedPreconditions[1].ResourceVersion {
			t.Fatalf("expected identical UID+ResourceVersion preconditions across replay, got %+v", recordedPreconditions)
		}
		if got := *sts.Spec.UpdateStrategy.RollingUpdate.Partition; got != 2 {
			t.Fatalf("partition after replay = %d, want 2 (protecting B1)", got)
		}
		var b1 corev1.Pod
		if err := c.Get(ctx, types.NamespacedName{Name: "test-sample-1", Namespace: "default"}, &b1); err != nil {
			t.Fatalf("expected B1 to survive protected: %v", err)
		}
	})

	t.Run("conflict_then_informer_advances_without_delete", func(t *testing.T) {
		ctx := context.Background()
		initialWorkerSpec := wrappers.MakeWorkerPodSpec()
		initialWorkerImage := initialWorkerSpec.Containers[0].Image

		lws := wrappers.BuildBasicLeaderWorkerSet("test-sample", "default").
			WorkerTemplateSpec(initialWorkerSpec).
			Replica(1).
			Size(1).
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

		deleteAttempts := 0
		c, r := newCombinedTestClientWithInterceptor(t, interceptor.Funcs{
			Delete: func(ctx context.Context, cli client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
				pod, ok := obj.(*corev1.Pod)
				if ok && pod.Name == "test-sample-0" {
					deleteAttempts++
					if deleteAttempts == 1 {
						return context.DeadlineExceeded
					}
					return apierrors.NewConflict(schema.GroupResource{Resource: "pods"}, pod.Name, fmt.Errorf("rv mismatch"))
				}
				return cli.Delete(ctx, obj, opts...)
			},
		}, lws)
		req := reconcile.Request{NamespacedName: client.ObjectKeyFromObject(lws)}

		if _, err := r.Reconcile(ctx, req); err != nil {
			t.Fatalf("initial Reconcile() error = %v", err)
		}
		sts := ackLeaderStatefulSet(t, ctx, c, req.NamespacedName)
		revA := revisionutils.GetRevisionKey(sts)
		for _, obj := range makeOwnedGroupObjects(lws, sts, 0, 1, revA, "sts-"+revA, true, true, true) {
			if err := c.Create(ctx, obj); err != nil {
				t.Fatalf("creating A0: %v", err)
			}
		}

		if err := c.Get(ctx, req.NamespacedName, lws); err != nil {
			t.Fatalf("reading lws: %v", err)
		}
		lws.Generation = 2
		lws.Spec.Replicas = ptr.To[int32](3)
		lws.Spec.LeaderWorkerTemplate.WorkerTemplate.Spec.Containers[0].Image = "worker:v2"
		if err := c.Update(ctx, lws); err != nil {
			t.Fatalf("updating lws: %v", err)
		}
		if _, err := r.Reconcile(ctx, req); err != nil {
			t.Fatalf("rev B Reconcile() error = %v", err)
		}
		sts = ackLeaderStatefulSet(t, ctx, c, req.NamespacedName)
		revB := revisionutils.GetRevisionKey(sts)
		for _, obj := range makeOwnedGroupObjects(lws, sts, 1, 1, revB, "sts-"+revB, true, true, true) {
			if err := c.Create(ctx, obj); err != nil {
				t.Fatalf("creating B1: %v", err)
			}
		}
		for _, obj := range makeOwnedGroupObjects(lws, sts, 2, 1, revB, "sts-"+revB, false, false, false) {
			if err := c.Create(ctx, obj); err != nil {
				t.Fatalf("creating B2: %v", err)
			}
		}
		if _, err := r.Reconcile(ctx, req); err != nil {
			t.Fatalf("partition-drop Reconcile() error = %v", err)
		}
		ackLeaderStatefulSet(t, ctx, c, req.NamespacedName)

		// Initial DELETE(A0) times out.
		if _, err := r.Reconcile(ctx, req); err == nil {
			t.Fatal("expected timeout error from initial DELETE(A0)")
		}

		// Roll back to rev A; replay hits 409 Conflict -> holds partition at 3 and requeues!
		if err := c.Get(ctx, req.NamespacedName, lws); err != nil {
			t.Fatalf("reading lws: %v", err)
		}
		lws.Generation = 3
		lws.Spec.LeaderWorkerTemplate.WorkerTemplate.Spec.Containers[0].Image = initialWorkerImage
		if err := c.Update(ctx, lws); err != nil {
			t.Fatalf("rolling back lws: %v", err)
		}
		if _, err := r.Reconcile(ctx, req); err != nil {
			t.Fatalf("rollback publish Reconcile() error = %v", err)
		}
		ackLeaderStatefulSet(t, ctx, c, req.NamespacedName)

		res, err := r.Reconcile(ctx, req)
		if err != nil {
			t.Fatalf("conflict replay Reconcile() error = %v", err)
		}
		if res.RequeueAfter != pendingDeleteRequeueDelay {
			t.Fatalf("RequeueAfter on conflict = %v, want %v", res.RequeueAfter, pendingDeleteRequeueDelay)
		}
		sts = ackLeaderStatefulSet(t, ctx, c, req.NamespacedName)
		if got := *sts.Spec.UpdateStrategy.RollingUpdate.Partition; got != 4 {
			t.Fatalf("partition while awaiting informer update = %d, want 4", got)
		}

		// Informer now observes A0 at a newer ResourceVersion without DeletionTimestamp,
		// proving the dropped conditional DELETE can never execute -> A0 is credited again!
		var a0 corev1.Pod
		if err := c.Get(ctx, types.NamespacedName{Name: "test-sample-0", Namespace: "default"}, &a0); err != nil {
			t.Fatalf("getting A0: %v", err)
		}
		if a0.Annotations == nil {
			a0.Annotations = make(map[string]string)
		}
		a0.Annotations["kubelet-heartbeat"] = "bumped"
		if err := c.Update(ctx, &a0); err != nil {
			t.Fatalf("bumping A0 ResourceVersion: %v", err)
		}

		if _, err := r.Reconcile(ctx, req); err != nil {
			t.Fatalf("post-RV-advance Reconcile() error = %v", err)
		}
		sts = ackLeaderStatefulSet(t, ctx, c, req.NamespacedName)
		if got := *sts.Spec.UpdateStrategy.RollingUpdate.Partition; got != 1 {
			t.Fatalf("partition once surviving A0 satisfies floor = %d, want 1 (exposing B1 at ord 1)", got)
		}
		if deleteAttempts != 2 {
			t.Fatalf("expected no further DELETE replays after conflict + RV advance, got %d", deleteAttempts)
		}
	})
}

// TestDeleteStaleOrdinalLeadersSingleSnapshotDetectsConcurrentTermination
// verifies that if a sibling group begins terminating while applyLeaderStatefulSet
// is running, deleteStaleOrdinalLeaders refreshes states and pods together rather
// than spending stale readiness credit from planRollingUpdate.
func TestDeleteStaleOrdinalLeadersSingleSnapshotDetectsConcurrentTermination(t *testing.T) {
	ctx := context.Background()
	lws := wrappers.BuildBasicLeaderWorkerSet("test-sample", "default").
		WorkerTemplateSpec(wrappers.MakeWorkerPodSpec()).
		Replica(3).
		Size(1).
		RolloutStrategy(leaderworkerset.RolloutStrategy{
			Type: leaderworkerset.RollingUpdateStrategyType,
			RollingUpdateConfiguration: &leaderworkerset.RollingUpdateConfiguration{
				Partition:      ptr.To[int32](0),
				MaxUnavailable: intstr.FromInt32(1),
				MaxSurge:       intstr.FromInt32(0),
			},
		}).Obj()
	lws.UID = "lws-uid"
	lws.Generation = 2

	sts := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:       lws.Name,
			Namespace:  lws.Namespace,
			UID:        "leader-sts-uid",
			Generation: 3,
			Labels: map[string]string{
				leaderworkerset.SetNameLabelKey: lws.Name,
				leaderworkerset.RevisionKey:     "rev-2",
				leaderworkerset.RoleLabelKey:    leaderworkerset.RoleLeader,
			},
			Annotations: map[string]string{
				leaderworkerset.ReplicasAnnotationKey: "3",
				rolloutBaselineAnnotationKey:          "2",
			},
		},
		Spec: appsv1.StatefulSetSpec{
			Replicas: ptr.To[int32](3),
			UpdateStrategy: appsv1.StatefulSetUpdateStrategy{
				Type:          appsv1.RollingUpdateStatefulSetStrategyType,
				RollingUpdate: &appsv1.RollingUpdateStatefulSetStrategy{Partition: ptr.To[int32](0)},
			},
		},
		Status: appsv1.StatefulSetStatus{
			ObservedGeneration: 3,
			Replicas:           3,
			CurrentRevision:    "sts-rev-1",
			UpdateRevision:     "sts-rev-2",
		},
	}

	makeLeader := func(ord int, rev string, ready bool) *corev1.Pod {
		pod := makeOwnedGroupObjects(lws, sts, ord, 1, rev, "sts-"+rev, ready, ready, ready)[0].(*corev1.Pod)
		if ord == 1 {
			pod.Finalizers = []string{"test/hold-terminating"}
		}
		return pod
	}

	// Simulate ordinal 1 beginning termination in the informer cache right after
	// planRollingUpdate ran (during Apply on the leader StatefulSet).
	terminatedInApply := false
	var c client.Client
	c, r := newCombinedTestClientWithInterceptor(t, interceptor.Funcs{
		Apply: func(ctx context.Context, cli client.WithWatch, obj runtime.ApplyConfiguration, opts ...client.ApplyOption) error {
			err := cli.Apply(ctx, obj, opts...)
			if err == nil && !terminatedInApply {
				terminatedInApply = true
				if delErr := c.Delete(ctx, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "test-sample-1", Namespace: "default"}}); delErr != nil {
					t.Fatalf("marking test-sample-1 terminating: %v", delErr)
				}
			}
			return err
		},
	}, lws, sts,
		makeLeader(0, "rev-1", true),
		makeLeader(1, "rev-1", true),
		makeLeader(2, "rev-2", false),
	)
	rev2CR, err := revisionutils.NewRevision(ctx, c, lws, "rev-2")
	if err != nil {
		t.Fatalf("NewRevision() error = %v", err)
	}
	if _, err := revisionutils.CreateRevision(ctx, c, rev2CR); err != nil {
		t.Fatalf("CreateRevision() error = %v", err)
	}
	// Restore sts ObservedGeneration == Generation after Apply in Reconcile? Note:
	// Reconcile reads leaderSts before Apply and passes that pointer into deleteStaleOrdinalLeaders.
	req := reconcile.Request{NamespacedName: client.ObjectKeyFromObject(lws)}
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	// Ordinal 1 is now terminating (DeletionTimestamp != nil), leaving only
	// ordinal 0 Ready at floor=1, so ordinal 0 MUST NOT be deleted!
	var pod0 corev1.Pod
	if err := c.Get(ctx, types.NamespacedName{Name: "test-sample-0", Namespace: "default"}, &pod0); err != nil {
		t.Fatalf("expected test-sample-0 to stay alive at floor=1, got err=%v", err)
	}
	if pod0.DeletionTimestamp != nil {
		t.Fatalf("expected test-sample-0 DeletionTimestamp == nil, got %v", pod0.DeletionTimestamp)
	}
}

// TestPreserveFloorDuringSurgeTrim verifies that surge reclamation does not
// scale down a Ready higher surge ordinal when lower desired/surge ordinals are
// still unready and trimming from the tail would violate the availability floor.
func TestPreserveFloorDuringSurgeTrim(t *testing.T) {
	ctx := context.Background()
	lws := wrappers.BuildBasicLeaderWorkerSet("test-sample", "default").
		Replica(2).
		Size(1).
		RolloutStrategy(leaderworkerset.RolloutStrategy{
			Type: leaderworkerset.RollingUpdateStrategyType,
			RollingUpdateConfiguration: &leaderworkerset.RollingUpdateConfiguration{
				Partition:      ptr.To[int32](0),
				MaxUnavailable: intstr.FromInt32(0),
				MaxSurge:       intstr.FromInt32(2),
			},
		}).Obj()
	lws.UID = "lws-uid"

	sts := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:       lws.Name,
			Namespace:  lws.Namespace,
			UID:        "leader-sts-uid",
			Generation: 2,
			Labels: map[string]string{
				leaderworkerset.SetNameLabelKey: lws.Name,
				leaderworkerset.RevisionKey:     "rev-2",
				leaderworkerset.RoleLabelKey:    leaderworkerset.RoleLeader,
			},
			Annotations: map[string]string{
				leaderworkerset.ReplicasAnnotationKey: "2",
				rolloutBaselineAnnotationKey:          "2",
			},
		},
		Spec: appsv1.StatefulSetSpec{
			Replicas: ptr.To[int32](4),
			UpdateStrategy: appsv1.StatefulSetUpdateStrategy{
				Type:          appsv1.RollingUpdateStatefulSetStrategyType,
				RollingUpdate: &appsv1.RollingUpdateStatefulSetStrategy{Partition: ptr.To[int32](0)},
			},
		},
		Status: appsv1.StatefulSetStatus{
			ObservedGeneration: 2,
			Replicas:           4,
			CurrentRevision:    "sts-rev-1",
			UpdateRevision:     "sts-rev-2",
		},
	}
	makeLeader := func(ord int, rev string, ready bool) *corev1.Pod {
		return makeOwnedGroupObjects(lws, sts, ord, 1, rev, "sts-"+rev, ready, ready, ready)[0].(*corev1.Pod)
	}

	// Floor is 2 (baseline=2, maxUnavailable=0).
	// Ord 0 is unready, ord 1 is Ready, ord 2 (surge) is Pending, ord 3 (surge) is Ready.
	// Raw surge formula wants 2 + 1 = 3, which would kill Ready ord 3 and drop Ready groups to 1!
	c, r := newCombinedTestClient(t,
		lws, sts,
		makeLeader(0, "rev-2", false),
		makeLeader(1, "rev-2", true),
		makeLeader(2, "rev-2", false),
		makeLeader(3, "rev-2", true),
	)
	plan, err := r.planRollingUpdate(ctx, lws, sts, "rev-2", false)
	if err != nil {
		t.Fatalf("planRollingUpdate() error = %v", err)
	}
	if plan.replicas != 4 {
		t.Fatalf("plan.replicas = %d, want 4 (preserving Ready surge ordinal 3 to satisfy floor=2)", plan.replicas)
	}

	// Also verify Case 2 (superseding update / rollback while surged) preserves ord 3 when needed:
	lws.Spec.RolloutStrategy.RollingUpdateConfiguration.MaxUnavailable = intstr.FromInt32(1) // floor = 1
	var ord1 corev1.Pod
	if err := c.Get(ctx, types.NamespacedName{Name: "test-sample-1", Namespace: "default"}, &ord1); err != nil {
		t.Fatalf("getting ord 1: %v", err)
	}
	ord1.Status = corev1.PodStatus{Phase: corev1.PodPending}
	if err := c.Status().Update(ctx, &ord1); err != nil {
		t.Fatalf("marking ord 1 Pending: %v", err)
	}
	// Now ord 3 is the ONLY Ready group (satisfying floor=1), while raw Case 2 wants 2 + (2-1) = 3.
	planCase2, err := r.planRollingUpdate(ctx, lws, sts, "rev-3", true)
	if err != nil {
		t.Fatalf("planRollingUpdate(Case 2) error = %v", err)
	}
	if planCase2.replicas != 4 {
		t.Fatalf("planCase2.replicas = %d, want 4 (preserving sole Ready surge ordinal 3)", planCase2.replicas)
	}

	// Once ord 0 becomes Ready, floor=1 is satisfied within [0..3), so surge trims cleanly to 3.
	var ord0 corev1.Pod
	if err := c.Get(ctx, types.NamespacedName{Name: "test-sample-0", Namespace: "default"}, &ord0); err != nil {
		t.Fatalf("getting ord 0: %v", err)
	}
	ord0.Status = corev1.PodStatus{
		Phase:      corev1.PodRunning,
		Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}},
	}
	if err := c.Status().Update(ctx, &ord0); err != nil {
		t.Fatalf("marking ord 0 Ready: %v", err)
	}
	planTrimmed, err := r.planRollingUpdate(ctx, lws, sts, "rev-2", false)
	if err != nil {
		t.Fatalf("planRollingUpdate(after ord 0 Ready) error = %v", err)
	}
	if planTrimmed.replicas != 2 {
		t.Fatalf("planTrimmed.replicas = %d, want 2 once unready desired replicas (1) fit maxUnavailable=1", planTrimmed.replicas)
	}
}

// surgeReductionFixture returns an Ordinal rollout with replicas=4,
// maxUnavailable=1 (floor 3), and maxSurge lowered from 2 to 0 while six groups
// exist. Only ordinals 0, 1, and 5 are Ready.
func surgeReductionFixture(t *testing.T) (*leaderworkerset.LeaderWorkerSet, *appsv1.StatefulSet, *LeaderWorkerSetReconciler) {
	t.Helper()
	lws := wrappers.BuildBasicLeaderWorkerSet("test-sample", "default").
		Replica(4).
		Size(1).
		RolloutStrategy(leaderworkerset.RolloutStrategy{
			Type: leaderworkerset.RollingUpdateStrategyType,
			RollingUpdateConfiguration: &leaderworkerset.RollingUpdateConfiguration{
				Partition:      ptr.To[int32](0),
				MaxUnavailable: intstr.FromInt32(1),
				MaxSurge:       intstr.FromInt32(0),
			},
		}).Obj()
	lws.UID = "lws-uid"
	sts := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:       lws.Name,
			Namespace:  lws.Namespace,
			UID:        "leader-sts-uid",
			Generation: 3,
			Labels: map[string]string{
				leaderworkerset.SetNameLabelKey: lws.Name,
				leaderworkerset.RevisionKey:     "rev-2",
				leaderworkerset.RoleLabelKey:    leaderworkerset.RoleLeader,
			},
			Annotations: map[string]string{
				leaderworkerset.ReplicasAnnotationKey: "4",
				rolloutBaselineAnnotationKey:          "4",
			},
		},
		Spec: appsv1.StatefulSetSpec{
			Replicas: ptr.To[int32](6),
			UpdateStrategy: appsv1.StatefulSetUpdateStrategy{
				Type:          appsv1.RollingUpdateStatefulSetStrategyType,
				RollingUpdate: &appsv1.RollingUpdateStatefulSetStrategy{Partition: ptr.To[int32](2)},
			},
		},
		Status: appsv1.StatefulSetStatus{
			ObservedGeneration: 3,
			Replicas:           6,
			CurrentRevision:    "sts-rev-1",
			UpdateRevision:     "sts-rev-2",
		},
	}
	objs := []client.Object{lws, sts}
	for ord, group := range []struct {
		rev   string
		ready bool
	}{{"rev-1", true}, {"rev-1", true}, {"rev-2", false}, {"rev-2", false}, {"rev-2", false}, {"rev-2", true}} {
		objs = append(objs, makeOwnedGroupObjects(lws, sts, ord, 1, group.rev, "sts-"+group.rev, group.ready, group.ready, group.ready)...)
	}
	_, r := newCombinedTestClient(t, objs...)
	return lws, sts, r
}

// TestSurgeReductionKeepsReadyTailOrdinalForFloor verifies that lowering
// maxSurge does not trim a Ready ordinal above the new burst limit when the
// floor still needs it, with or without a simultaneous template update.
func TestSurgeReductionKeepsReadyTailOrdinalForFloor(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name          string
		revisionKey   string
		templateEdit  bool
		wantReplicas  int32
		wantPartition int32
	}{
		// Case 5: removing Ready ordinal 5 would leave 2 Ready < floor 3.
		{name: "surge lowered", revisionKey: "rev-2", wantReplicas: 6, wantPartition: 2},
		// Case 2: the same reduction published with a new template.
		{name: "surge lowered with template update", revisionKey: "rev-3", templateEdit: true, wantReplicas: 6, wantPartition: 6},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lws, sts, r := surgeReductionFixture(t)
			plan, err := r.planRollingUpdate(ctx, lws, sts, tc.revisionKey, tc.templateEdit)
			if err != nil {
				t.Fatalf("planRollingUpdate() error = %v", err)
			}
			if plan.replicas != tc.wantReplicas || plan.partition != tc.wantPartition {
				t.Fatalf("plan (replicas, partition) = (%d, %d), want (%d, %d)",
					plan.replicas, plan.partition, tc.wantReplicas, tc.wantPartition)
			}
		})
	}
}

// TestHashCompletedRolloutBaselineIsNotInherited verifies that a new Hash
// revision does not inherit the baseline of a previous rollout that already
// reached its desired count, even when the baseline-clearing pass has not run.
func TestHashCompletedRolloutBaselineIsNotInherited(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		// scaleBeforeEdit scales the completed rollout before the next
		// template update arrives.
		scaleBeforeEdit    bool
		wantBaseline       string
		wantMaxUnavailable int32
	}{
		{name: "template update right after completion", wantBaseline: "4", wantMaxUnavailable: 1},
		{name: "scale-up then template update", scaleBeforeEdit: true, wantBaseline: "6", wantMaxUnavailable: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lws := wrappers.BuildBasicLeaderWorkerSet("test-hash", "default").
				WorkerTemplateSpec(wrappers.MakeWorkerPodSpec()).
				Replica(1).
				Size(2).
				RolloutStrategy(leaderworkerset.RolloutStrategy{
					Type: leaderworkerset.RollingUpdateStrategyType,
					RollingUpdateConfiguration: &leaderworkerset.RollingUpdateConfiguration{
						MaxUnavailable: intstr.FromInt32(1),
						MaxSurge:       intstr.FromInt32(0),
					},
				}).Obj()
			lws.Spec.GroupIdentity = leaderworkerset.GroupIdentityHash
			lws.UID = "lws-hash-uid"
			lws.Generation = 1

			c, r := newCombinedTestClient(t, lws)
			req := reconcile.Request{NamespacedName: client.ObjectKeyFromObject(lws)}
			var deploy appsv1.Deployment
			reconcileAndGet := func(step string) {
				t.Helper()
				if _, err := r.Reconcile(ctx, req); err != nil {
					t.Fatalf("%s: Reconcile() error = %v", step, err)
				}
				if err := c.Get(ctx, req.NamespacedName, &deploy); err != nil {
					t.Fatalf("%s: reading deploy: %v", step, err)
				}
			}
			updateLWS := func(replicas int32, image string) {
				t.Helper()
				if err := c.Get(ctx, req.NamespacedName, lws); err != nil {
					t.Fatalf("reading lws: %v", err)
				}
				lws.Generation++
				lws.Spec.Replicas = ptr.To(replicas)
				lws.Spec.LeaderWorkerTemplate.WorkerTemplate.Spec.Containers[0].Image = image
				if err := c.Update(ctx, lws); err != nil {
					t.Fatalf("updating lws: %v", err)
				}
			}

			reconcileAndGet("create")
			// Combined scale-up (1 -> 4) and template update: baseline 1.
			updateLWS(4, "worker:v2")
			reconcileAndGet("combined update")
			if got := deploy.Annotations[rolloutBaselineAnnotationKey]; got != "1" {
				t.Fatalf("combined update baseline = %q, want %q", got, "1")
			}

			// The rollout completes: all four leaders run rev2 and are Ready.
			rev2 := revisionutils.GetRevisionKey(&deploy)
			deploy.Generation = 2
			if err := c.Update(ctx, &deploy); err != nil {
				t.Fatalf("updating deploy generation: %v", err)
			}
			deploy.Status = appsv1.DeploymentStatus{ObservedGeneration: 2, Replicas: 4, UpdatedReplicas: 4, ReadyReplicas: 4, AvailableReplicas: 4}
			if err := c.Status().Update(ctx, &deploy); err != nil {
				t.Fatalf("updating deploy status: %v", err)
			}
			for i := range 4 {
				pod := &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:      fmt.Sprintf("hash-leader-%d", i),
						Namespace: lws.Namespace,
						Labels: map[string]string{
							leaderworkerset.SetNameLabelKey:     lws.Name,
							leaderworkerset.WorkerIndexLabelKey: "0",
							leaderworkerset.GroupIndexLabelKey:  fmt.Sprintf("hash-%d", i),
							leaderworkerset.RevisionKey:         rev2,
						},
					},
					Status: corev1.PodStatus{
						Phase:      corev1.PodRunning,
						Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}},
					},
				}
				if err := c.Create(ctx, pod); err != nil {
					t.Fatalf("creating hash leader %d: %v", i, err)
				}
			}

			replicas := int32(4)
			if tc.scaleBeforeEdit {
				replicas = 6
				updateLWS(replicas, "worker:v2")
				reconcileAndGet("scale-up")
			}
			// The next template update arrives before any other pass.
			updateLWS(replicas, "worker:v3")
			reconcileAndGet("next update")
			if got := deploy.Annotations[rolloutBaselineAnnotationKey]; got != tc.wantBaseline {
				t.Fatalf("next update baseline = %q, want %q", got, tc.wantBaseline)
			}
			if got := deploy.Spec.Strategy.RollingUpdate.MaxUnavailable.IntVal; got != tc.wantMaxUnavailable {
				t.Fatalf("next update deploy maxUnavailable = %d, want %d", got, tc.wantMaxUnavailable)
			}
		})
	}
}

// TestRejectedReplayKeepsTimedOutDeletePending verifies that a retry rejected
// by admission does not resolve the original timed-out DELETE, which can still
// succeed with the same UID and ResourceVersion preconditions.
func TestRejectedReplayKeepsTimedOutDeletePending(t *testing.T) {
	ctx := context.Background()
	lws := wrappers.BuildBasicLeaderWorkerSet("test-sample", "default").Replica(1).Size(1).Obj()
	lws.UID = "lws-uid"
	pod := lwsStatusLeaderPod(lws, 0, "rev-a", true)
	pod.UID = "a0-uid"

	deleteAttempts := 0
	scheme := lwsStatusScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pod).WithInterceptorFuncs(interceptor.Funcs{
		Delete: func(ctx context.Context, cli client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			deleteAttempts++
			if deleteAttempts == 1 {
				return context.DeadlineExceeded
			}
			return apierrors.NewForbidden(schema.GroupResource{Resource: "pods"}, obj.GetName(), fmt.Errorf("denied by admission policy"))
		},
	}).Build()
	if err := c.Get(ctx, client.ObjectKeyFromObject(pod), pod); err != nil {
		t.Fatalf("getting A0: %v", err)
	}

	var p pendingLeaderDeletes
	if _, err := p.deleteLeaderPod(ctx, c, lws, pod); err == nil {
		t.Fatal("expected the initial DELETE to time out")
	}
	podKey := client.ObjectKeyFromObject(pod)
	for attempt := range 2 {
		pendingUIDs, hasUncertain, err := p.observeAndResolve(ctx, c, lws, []corev1.Pod{*pod}, true)
		if !apierrors.IsForbidden(err) {
			t.Fatalf("replay %d error = %v, want Forbidden", attempt, err)
		}
		if _, pending := pendingUIDs[pod.UID]; !pending || !hasUncertain || !p.isPending(podKey, pod.UID) {
			t.Fatalf("replay %d: A0 pending=%v uncertain=%v tracked=%v, want all true", attempt, pending, hasUncertain, p.isPending(podKey, pod.UID))
		}
	}
	if deleteAttempts != 3 {
		t.Fatalf("deleteAttempts = %d, want 3 (initial + two replays)", deleteAttempts)
	}
}

// TestTakeoverBarrierWithholdsCreditFromLeadersDeletedBeforeTakeover reproduces
// a takeover with a Pod cache that started before leader election: the cache
// still shows A0 Ready although the previous leader already deleted it. After a
// rollback to rev A, trimming surge ordinal B1 would leave no Ready group.
func TestTakeoverBarrierWithholdsCreditFromLeadersDeletedBeforeTakeover(t *testing.T) {
	ctx := context.Background()
	lws := wrappers.BuildBasicLeaderWorkerSet("test-sample", "default").
		Replica(1).
		Size(1).
		RolloutStrategy(leaderworkerset.RolloutStrategy{
			Type: leaderworkerset.RollingUpdateStrategyType,
			RollingUpdateConfiguration: &leaderworkerset.RollingUpdateConfiguration{
				Partition:      ptr.To[int32](0),
				MaxUnavailable: intstr.FromInt32(0),
				MaxSurge:       intstr.FromInt32(1),
			},
		}).Obj()
	lws.UID = "lws-uid"
	sts := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:       lws.Name,
			Namespace:  lws.Namespace,
			UID:        "leader-sts-uid",
			Generation: 4,
			Labels: map[string]string{
				leaderworkerset.SetNameLabelKey: lws.Name,
				leaderworkerset.RevisionKey:     "rev-a",
				leaderworkerset.RoleLabelKey:    leaderworkerset.RoleLeader,
			},
			Annotations: map[string]string{
				leaderworkerset.ReplicasAnnotationKey: "1",
				rolloutBaselineAnnotationKey:          "1",
			},
		},
		Spec: appsv1.StatefulSetSpec{
			Replicas: ptr.To[int32](2),
			UpdateStrategy: appsv1.StatefulSetUpdateStrategy{
				Type:          appsv1.RollingUpdateStatefulSetStrategyType,
				RollingUpdate: &appsv1.RollingUpdateStatefulSetStrategy{Partition: ptr.To[int32](1)},
			},
		},
		Status: appsv1.StatefulSetStatus{
			ObservedGeneration: 4,
			Replicas:           2,
			CurrentRevision:    "sts-rev-b",
			UpdateRevision:     "sts-rev-a",
		},
	}
	a0 := makeOwnedGroupObjects(lws, sts, 0, 1, "rev-a", "sts-rev-a", true, true, true)[0]
	b1 := makeOwnedGroupObjects(lws, sts, 1, 1, "rev-b", "sts-rev-b", true, true, true)[0]
	cache, r := newCombinedTestClient(t, lws, sts, a0, b1)
	r.apiReader = fake.NewClientBuilder().WithScheme(lwsStatusScheme(t)).WithObjects(b1.DeepCopyObject().(client.Object)).Build()

	plan, err := r.planRollingUpdate(ctx, lws, sts, "rev-a", false)
	if err != nil {
		t.Fatalf("planRollingUpdate() error = %v", err)
	}
	if plan.replicas != 2 {
		t.Fatalf("plan.replicas = %d, want 2 (keep Ready B1 while A0 is already deleted)", plan.replicas)
	}

	// Once the informer observes A0's deletion, the barrier entry resolves.
	if err := cache.Delete(ctx, a0); err != nil {
		t.Fatalf("deleting A0 from the cache: %v", err)
	}
	if _, err := r.planRollingUpdate(ctx, lws, sts, "rev-a", false); err != nil {
		t.Fatalf("planRollingUpdate() after cache sync error = %v", err)
	}
	if r.pendingDeletes.hasEntriesFor(client.ObjectKeyFromObject(lws)) {
		t.Fatal("expected the takeover entry for A0 to resolve once the cache observes its deletion")
	}
}

// TestOrdinalCompletedRolloutBaselineIsNotInherited is the Ordinal analog of
// TestHashCompletedRolloutBaselineIsNotInherited: a template update that arrives
// after a 1 -> 4 rollout finished, but before the baseline-clearing pass, must
// start from baseline 4 (floor 3), not the stale baseline 1 (floor 0).
func TestOrdinalCompletedRolloutBaselineIsNotInherited(t *testing.T) {
	ctx := context.Background()
	lws := wrappers.BuildBasicLeaderWorkerSet("test-sample", "default").
		Replica(4).
		Size(1).
		RolloutStrategy(leaderworkerset.RolloutStrategy{
			Type: leaderworkerset.RollingUpdateStrategyType,
			RollingUpdateConfiguration: &leaderworkerset.RollingUpdateConfiguration{
				Partition:      ptr.To[int32](0),
				MaxUnavailable: intstr.FromInt32(1),
				MaxSurge:       intstr.FromInt32(0),
			},
		}).Obj()
	lws.UID = "lws-uid"
	sts := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:       lws.Name,
			Namespace:  lws.Namespace,
			UID:        "leader-sts-uid",
			Generation: 3,
			Labels: map[string]string{
				leaderworkerset.SetNameLabelKey: lws.Name,
				leaderworkerset.RevisionKey:     "rev-2",
				leaderworkerset.RoleLabelKey:    leaderworkerset.RoleLeader,
			},
			Annotations: map[string]string{
				leaderworkerset.ReplicasAnnotationKey: "4",
				rolloutBaselineAnnotationKey:          "1",
			},
		},
		Spec: appsv1.StatefulSetSpec{
			Replicas: ptr.To[int32](4),
			UpdateStrategy: appsv1.StatefulSetUpdateStrategy{
				Type:          appsv1.RollingUpdateStatefulSetStrategyType,
				RollingUpdate: &appsv1.RollingUpdateStatefulSetStrategy{Partition: ptr.To[int32](0)},
			},
		},
		Status: appsv1.StatefulSetStatus{
			ObservedGeneration: 3,
			Replicas:           4,
			CurrentRevision:    "sts-rev-2",
			UpdateRevision:     "sts-rev-2",
		},
	}
	objs := []client.Object{lws, sts}
	for ord := range 4 {
		objs = append(objs, makeOwnedGroupObjects(lws, sts, ord, 1, "rev-2", "sts-rev-2", true, true, true)...)
	}
	_, r := newCombinedTestClient(t, objs...)

	plan, err := r.planRollingUpdate(ctx, lws, sts, "rev-3", true)
	if err != nil {
		t.Fatalf("planRollingUpdate(publish) error = %v", err)
	}
	if plan.baseline == nil || *plan.baseline != 4 || plan.floor != 3 {
		t.Fatalf("publish (baseline, floor) = (%d, %d), want (4, 3)", ptr.Deref(plan.baseline, -1), plan.floor)
	}

	// The StatefulSet observes rev-3; the next pass may replace only one group.
	sts.Labels[leaderworkerset.RevisionKey] = "rev-3"
	sts.Annotations[rolloutBaselineAnnotationKey] = strconv.Itoa(int(*plan.baseline))
	sts.Spec.UpdateStrategy.RollingUpdate.Partition = ptr.To(plan.partition)
	sts.Status.UpdateRevision = "sts-rev-3"
	next, err := r.planRollingUpdate(ctx, lws, sts, "rev-3", false)
	if err != nil {
		t.Fatalf("planRollingUpdate(advance) error = %v", err)
	}
	if next.partition != 3 {
		t.Fatalf("advance partition = %d, want 3 (one group below floor 3 at a time)", next.partition)
	}
}

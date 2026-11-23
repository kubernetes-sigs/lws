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
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	leaderworkerset "sigs.k8s.io/lws/api/leaderworkerset/v1"
	revisionutils "sigs.k8s.io/lws/pkg/utils/revision"
	"sigs.k8s.io/lws/test/wrappers"
)

const (
	workerUIDRevision = "revision-uids"
	workerUIDOld      = types.UID("11111111-1111-1111-1111-111111111111")
	workerUIDNew      = types.UID("22222222-2222-2222-2222-222222222222")
)

func TestFormatWorkerUIDRecordUsesHashPastTheLimit(t *testing.T) {
	previous := workerUIDListMax
	workerUIDListMax = 1
	t.Cleanup(func() { workerUIDListMax = previous })

	formatted := formatWorkerUIDRecord("rev", []types.UID{workerUIDOld, workerUIDNew})
	parsed, err := parseWorkerUIDRecord(formatted)
	if err != nil {
		t.Fatalf("parseWorkerUIDRecord(%q) error = %v", formatted, err)
	}
	if !parsed.hashed || parsed.count != 2 || parsed.revision != "rev" {
		t.Fatalf("parsed record = %+v, want a hash of 2 UIDs for revision rev", parsed)
	}
	if !parsed.matches([]types.UID{workerUIDNew, workerUIDOld}) {
		t.Fatal("hash record does not match the same UIDs in a different order")
	}
	if parsed.matches([]types.UID{workerUIDNew}) {
		t.Fatal("hash record matched a smaller set")
	}
}

func TestEnsureWorkerUIDRecordWritesOnceWhenTheGroupForms(t *testing.T) {
	leader := workerUIDLeader()
	worker := workerUIDPod(leader, workerUIDOld)
	counter := &workerUIDPatchCounter{}
	r := workerUIDReconciler(t, counter, leader, worker)

	pending, err := r.ensureWorkerUIDRecord(context.Background(), leader, 2)
	if err != nil || pending {
		t.Fatalf("ensureWorkerUIDRecord() = %t, %v, want written", pending, err)
	}
	want := workerUIDRecordValue(workerUIDRevision, workerUIDOld)
	if leader.Annotations[leaderworkerset.WorkerUIDsAnnotationKey] != want {
		t.Fatalf("annotation = %q, want %q", leader.Annotations[leaderworkerset.WorkerUIDsAnnotationKey], want)
	}
	if counter.patches != 1 {
		t.Fatalf("patches after record = %d, want 1", counter.patches)
	}

	pending, err = r.ensureWorkerUIDRecord(context.Background(), leader, 2)
	if err != nil || pending {
		t.Fatalf("second ensureWorkerUIDRecord() = %t, %v, want a no-op", pending, err)
	}
	if counter.patches != 1 {
		t.Fatalf("patches after second ensure = %d, want 1", counter.patches)
	}
}

func TestEnsureWorkerUIDRecordWaitsUntilEveryWorkerExists(t *testing.T) {
	leader := workerUIDLeader()
	r := workerUIDReconciler(t, nil, leader)

	pending, err := r.ensureWorkerUIDRecord(context.Background(), leader, 2)
	if err != nil || !pending {
		t.Fatalf("ensureWorkerUIDRecord() = %t, %v, want pending", pending, err)
	}
	if leader.Annotations[leaderworkerset.WorkerUIDsAnnotationKey] != "" {
		t.Fatalf("annotation = %q, want empty while the group is forming", leader.Annotations[leaderworkerset.WorkerUIDsAnnotationKey])
	}
}

func TestHandleRestartPolicyDoesNotRecreateAnUnrecordedGroup(t *testing.T) {
	lws := workerUIDLWS()
	leader := workerUIDLeader()
	r := workerUIDReconciler(t, nil, lws, leader)

	deleted, _, err := r.handleRestartPolicy(context.Background(), *leader, *lws)
	if err != nil || deleted {
		t.Fatalf("handleRestartPolicy() = %t, %v, want no recreation before the group is recorded", deleted, err)
	}
}

func TestHandleRestartPolicyRecreatesWhenRecordedWorkerIsGone(t *testing.T) {
	lws := workerUIDLWS()
	leader := workerUIDLeader()
	leader.Annotations[leaderworkerset.WorkerUIDsAnnotationKey] = workerUIDRecordValue(workerUIDRevision, workerUIDOld)
	r := workerUIDReconciler(t, nil, lws, leader)
	before := podCtrlReadCounters(lws)

	for range 2 {
		if _, _, err := r.handleRestartPolicy(context.Background(), *leader, *lws); err != nil {
			t.Fatalf("handleRestartPolicy() error = %v", err)
		}
	}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(leader), &corev1.Pod{}); !apierrors.IsNotFound(err) {
		t.Fatalf("leader Get error = %v, want NotFound", err)
	}
	got := podCtrlReadCounters(lws)
	if got.recreations-before.recreations != 1 {
		t.Fatalf("recreation counter increment = %v, want 1", got.recreations-before.recreations)
	}
}

func TestHandleRestartPolicyRecreatesWhenRecordedWorkerIsReplaced(t *testing.T) {
	lws := workerUIDLWS()
	leader := workerUIDLeader()
	leader.Annotations[leaderworkerset.WorkerUIDsAnnotationKey] = workerUIDRecordValue(workerUIDRevision, workerUIDOld)
	replacement := workerUIDPod(leader, workerUIDNew)
	replacement.Status.Phase = corev1.PodRunning
	r := workerUIDReconciler(t, nil, lws, leader, replacement)

	deleted, _, err := r.handleRestartPolicy(context.Background(), *leader, *lws)
	if err != nil || !deleted {
		t.Fatalf("handleRestartPolicy() = %t, %v, want recreation", deleted, err)
	}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(leader), &corev1.Pod{}); !apierrors.IsNotFound(err) {
		t.Fatalf("leader Get error = %v, want NotFound", err)
	}
}

func TestHandleRestartPolicyIdentityLossHonorsBackoffAndBudget(t *testing.T) {
	t.Run("backoff", func(t *testing.T) {
		lws := wrappers.BuildLeaderWorkerSet("default").Replica(1).Size(2).
			RestartPolicy(leaderworkerset.RecreateGroupOnPodRestart).
			RestartBackoff(ptr.To[int32](10), ptr.To[int32](300)).Obj()
		leader := workerUIDLeader()
		leader.CreationTimestamp = metav1.NewTime(time.Now().Add(-2 * time.Second))
		leader.Annotations[leaderworkerset.WorkerUIDsAnnotationKey] = workerUIDRecordValue(workerUIDRevision, workerUIDOld)
		r := workerUIDReconciler(t, nil, lws, leader)
		before := podCtrlReadCounters(lws)

		deleted, result, err := r.handleRestartPolicy(context.Background(), *leader, *lws)
		if err != nil || deleted {
			t.Fatalf("handleRestartPolicy() = %t, %v, want backoff", deleted, err)
		}
		if result.RequeueAfter < 5*time.Second || result.RequeueAfter > 9*time.Second {
			t.Fatalf("RequeueAfter = %s, want about 8s", result.RequeueAfter)
		}
		var got corev1.Pod
		if err := r.Get(context.Background(), client.ObjectKeyFromObject(leader), &got); err != nil {
			t.Fatal(err)
		}
		if got.Annotations[leaderworkerset.GroupRecreatePendingAnnotationKey] == "" {
			t.Fatal("expected the backoff decision to be persisted before the leader is deleted")
		}
		if _, _, err := r.handleRestartPolicy(context.Background(), *leader, *lws); err != nil {
			t.Fatalf("second handleRestartPolicy() error = %v", err)
		}
		if podCtrlReadCounters(lws).backoffs-before.backoffs != 1 {
			t.Fatalf("backoff counter increment = %v, want 1", podCtrlReadCounters(lws).backoffs-before.backoffs)
		}
	})

	t.Run("budget", func(t *testing.T) {
		lws := wrappers.BuildLeaderWorkerSet("budget-uids").Replica(1).Size(2).
			RestartPolicy(leaderworkerset.RecreateGroupOnPodRestart).MaxGroupRestarts(0).Obj()
		leader := workerUIDLeader()
		leader.Namespace = lws.Namespace
		leader.Labels[leaderworkerset.SetNameLabelKey] = lws.Name
		leader.Annotations[leaderworkerset.WorkerUIDsAnnotationKey] = workerUIDRecordValue(workerUIDRevision, workerUIDOld)
		r := workerUIDReconciler(t, nil, lws, leader)
		before := podCtrlReadCounters(lws)

		if _, _, err := r.handleRestartPolicy(context.Background(), *leader, *lws); err != nil {
			t.Fatalf("handleRestartPolicy() error = %v", err)
		}
		if podCtrlReadCounters(lws).budgetExhausted-before.budgetExhausted != 1 || podCtrlReadCounters(lws).recreations != before.recreations {
			t.Fatalf("counters = %+v, before %+v, want one exhaustion and no recreation", podCtrlReadCounters(lws), before)
		}
	})
}

func TestHandleRestartPolicyAfterStartWaitsForAPendingReplacement(t *testing.T) {
	lws := wrappers.BuildLeaderWorkerSet("default").Replica(1).Size(2).RestartGroupAfterStart().Obj()
	leader := workerUIDLeader()
	leader.Status.Phase = corev1.PodRunning
	leader.Annotations[leaderworkerset.WorkerUIDsAnnotationKey] = workerUIDRecordValue(workerUIDRevision, workerUIDOld)
	replacement := workerUIDPod(leader, workerUIDNew)
	replacement.Status.Phase = corev1.PodPending
	r := workerUIDReconciler(t, nil, lws, leader, replacement)

	deleted, _, err := r.handleRestartPolicy(context.Background(), *leader, *lws)
	if err != nil || deleted {
		t.Fatalf("handleRestartPolicy() = %t, %v, want no recreation while the replacement is pending", deleted, err)
	}

	replacement.Status.Phase = corev1.PodRunning
	if err := r.Status().Update(context.Background(), replacement); err != nil {
		t.Fatal(err)
	}
	deleted, _, err = r.handleRestartPolicy(context.Background(), *leader, *lws)
	if err != nil || !deleted {
		t.Fatalf("handleRestartPolicy() after start = %t, %v, want recreation", deleted, err)
	}
}

func TestWorkerFollowsRestartPolicySkipsRevisionLookupOnceRecorded(t *testing.T) {
	lws := workerUIDLWS()
	leader := workerUIDLeader()
	worker := workerUIDPod(leader, workerUIDOld)
	leader.Annotations[leaderworkerset.WorkerUIDsAnnotationKey] = workerUIDRecordValue(workerUIDRevision, workerUIDOld)
	counter := &workerUIDListCounter{}
	base := workerUIDReconciler(t, nil, lws, leader, worker)
	counter.Client = base.Client
	base.Client = counter

	follow, err := base.workerFollowsRestartPolicy(context.Background(), worker, lws)
	if err != nil || follow {
		t.Fatalf("workerFollowsRestartPolicy() = %t, %v, want false", follow, err)
	}
	if counter.revisionLists != 0 {
		t.Fatalf("ControllerRevision lists = %d, want 0 for a recorded worker", counter.revisionLists)
	}
}

func TestWorkerFollowsRestartPolicyIgnoresAStaleWorker(t *testing.T) {
	lws := workerUIDLWS()
	leader := workerUIDLeader()
	leader.Annotations[leaderworkerset.WorkerUIDsAnnotationKey] = workerUIDRecordValue(workerUIDRevision, workerUIDOld)
	current := workerUIDPod(leader, workerUIDOld)
	stale := workerUIDPod(leader, workerUIDNew)
	stale.Name = leader.Name + "-stale"
	stale.Labels[leaderworkerset.RevisionKey] = "previous-revision"
	r := workerUIDReconciler(t, nil, lws, leader, current, stale)

	follow, err := r.workerFollowsRestartPolicy(context.Background(), stale, lws)
	if err != nil || follow {
		t.Fatalf("workerFollowsRestartPolicy(stale) = %t, %v, want false", follow, err)
	}
	deleted, _, err := r.handleRestartPolicy(context.Background(), *leader, *lws)
	if err != nil || deleted {
		t.Fatalf("handleRestartPolicy() = %t, %v, want the recorded worker to keep the group", deleted, err)
	}
}

func TestReconcilePodRecordsWorkerUIDsAndRecreatesAfterRestart(t *testing.T) {
	scheme := podCtrlTestScheme(t)
	lws := workerUIDLWS()
	lws.UID = "lws-uid"
	lws.Kind = "LeaderWorkerSet"
	lws.APIVersion = leaderworkerset.GroupVersion.String()
	revisionClient := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&corev1.Pod{}).WithObjects(lws.DeepCopy()).Build()
	revision, err := revisionutils.NewRevision(context.Background(), revisionClient, lws, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := revisionClient.Create(context.Background(), revision); err != nil {
		t.Fatal(err)
	}

	leader := workerUIDLeader()
	leader.Labels[leaderworkerset.RevisionKey] = revisionutils.GetRevisionKey(revision)
	leader.Spec.Hostname = leader.Name
	leader.Spec.Subdomain = lws.Name
	worker := workerUIDPod(leader, workerUIDOld)
	worker.Labels[leaderworkerset.RevisionKey] = revisionutils.GetRevisionKey(revision)

	r := &PodReconciler{
		Client: fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&corev1.Pod{}).
			WithObjects(lws.DeepCopy(), revision.DeepCopy(), leader.DeepCopy(), worker.DeepCopy()).Build(),
		Scheme: scheme,
		Record: fakeEventRecorder{},
	}
	if _, err := r.reconcilePod(context.Background(), podReconcileRequestForPod(leader, false)); err != nil {
		t.Fatalf("reconcilePod(leader) error = %v", err)
	}
	var recorded corev1.Pod
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(leader), &recorded); err != nil {
		t.Fatal(err)
	}
	wantRecord := workerUIDRecordValue(revisionutils.GetRevisionKey(revision), workerUIDOld)
	if recorded.Annotations[leaderworkerset.WorkerUIDsAnnotationKey] != wantRecord {
		t.Fatalf("annotation = %q, want %q", recorded.Annotations[leaderworkerset.WorkerUIDsAnnotationKey], wantRecord)
	}

	// The controller stops and the worker is removed before it starts again.
	if err := r.Delete(context.Background(), worker); err != nil {
		t.Fatal(err)
	}
	fresh := &PodReconciler{Client: r.Client, Scheme: scheme, Record: fakeEventRecorder{}}
	if _, err := fresh.reconcilePod(context.Background(), podReconcileRequestForPod(&recorded, false)); err != nil {
		t.Fatalf("reconcilePod(leader) after worker loss error = %v", err)
	}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(leader), &corev1.Pod{}); !apierrors.IsNotFound(err) {
		t.Fatalf("leader Get error = %v, want NotFound after the recorded worker disappeared", err)
	}
}

func TestRevisionChangeReplacesTheRecordWithoutRecreating(t *testing.T) {
	lws := workerUIDLWS()
	leader := workerUIDLeader()
	leader.Annotations[leaderworkerset.WorkerUIDsAnnotationKey] = workerUIDRecordValue(workerUIDRevision, workerUIDOld)
	leader.Labels[leaderworkerset.RevisionKey] = "next-revision"
	r := workerUIDReconciler(t, nil, lws, leader)

	deleted, _, err := r.handleRestartPolicy(context.Background(), *leader, *lws)
	if err != nil || deleted {
		t.Fatalf("handleRestartPolicy() = %t, %v, want no recreation when the recorded revision is stale", deleted, err)
	}

	next := workerUIDPod(leader, workerUIDNew)
	if err := r.Create(context.Background(), next); err != nil {
		t.Fatal(err)
	}
	// ensure compares against the client copy of the leader.
	var stored corev1.Pod
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(leader), &stored); err != nil {
		t.Fatal(err)
	}
	pending, err := r.ensureWorkerUIDRecord(context.Background(), &stored, 2)
	if err != nil || pending {
		t.Fatalf("ensureWorkerUIDRecord() = %t, %v, want the new revision recorded", pending, err)
	}
	if stored.Annotations[leaderworkerset.WorkerUIDsAnnotationKey] != workerUIDRecordValue("next-revision", workerUIDNew) {
		t.Fatalf("annotation = %q, want the next revision's worker", stored.Annotations[leaderworkerset.WorkerUIDsAnnotationKey])
	}
}

func TestFinishLeaderReconcileDoesNotShortenBackoff(t *testing.T) {
	leader := workerUIDLeader()
	lws := workerUIDLWS()
	r := workerUIDReconciler(t, nil, lws, leader)
	backoff := ctrl.Result{RequeueAfter: 8 * time.Second}

	result, err := r.finishLeaderReconcile(context.Background(), leader, lws, backoff)
	if err != nil {
		t.Fatal(err)
	}
	if result.RequeueAfter != backoff.RequeueAfter {
		t.Fatalf("RequeueAfter = %s, want the backoff delay %s kept", result.RequeueAfter, backoff.RequeueAfter)
	}
}

func workerUIDRecordValue(revision string, uid types.UID) string {
	return revision + "|" + string(uid)
}

func workerUIDLWS() *leaderworkerset.LeaderWorkerSet {
	return wrappers.BuildLeaderWorkerSet("default").Replica(1).Size(2).
		RestartPolicy(leaderworkerset.RecreateGroupOnPodRestart).Obj()
}

func workerUIDLeader() *corev1.Pod {
	leader := wrappers.MakePodWithLabels("test-sample", "0", "0", "default", 2)
	leader.UID = "leader-uid"
	leader.Labels[leaderworkerset.RevisionKey] = workerUIDRevision
	return leader
}

func workerUIDPod(leader *corev1.Pod, uid types.UID) *corev1.Pod {
	worker := wrappers.MakePodWithLabels(leader.Labels[leaderworkerset.SetNameLabelKey], leader.Labels[leaderworkerset.GroupIndexLabelKey], "1", leader.Namespace, 2)
	worker.UID = uid
	worker.Labels[leaderworkerset.RevisionKey] = leader.Labels[leaderworkerset.RevisionKey]
	worker.Annotations[leaderworkerset.LeaderPodNameAnnotationKey] = leader.Name
	worker.OwnerReferences = []metav1.OwnerReference{
		*metav1.NewControllerRef(leader, corev1.SchemeGroupVersion.WithKind("Pod")),
	}
	return worker
}

func workerUIDReconciler(t *testing.T, counter *workerUIDPatchCounter, objects ...client.Object) *PodReconciler {
	t.Helper()
	scheme := podCtrlTestScheme(t)
	builder := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&corev1.Pod{}).WithObjects(objects...)
	var c client.Client = builder.Build()
	if counter != nil {
		counter.Client = c
		c = counter
	}
	return &PodReconciler{Client: c, Scheme: scheme, Record: fakeEventRecorder{}}
}

type workerUIDPatchCounter struct {
	client.Client
	patches int
}

func (c *workerUIDPatchCounter) Patch(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
	c.patches++
	return c.Client.Patch(ctx, obj, patch, opts...)
}

type workerUIDListCounter struct {
	client.Client
	revisionLists int
}

func (c *workerUIDListCounter) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	if _, ok := list.(*appsv1.ControllerRevisionList); ok {
		c.revisionLists++
	}
	return c.Client.List(ctx, list, opts...)
}

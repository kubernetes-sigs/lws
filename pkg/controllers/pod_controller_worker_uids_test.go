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
	"strconv"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"k8s.io/utils/ptr"
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

func TestWorkerUIDRecordFormat(t *testing.T) {
	record := workerUIDRecord{revision: workerUIDRevision, digest: digestUIDs([]string{string(workerUIDNew), string(workerUIDOld)}), count: 2}
	for _, started := range []bool{false, true} {
		record.started = started
		if got := parseWorkerUIDRecord(record.String()); got != record {
			t.Fatalf("record after round trip = %+v, want %+v", got, record)
		}
	}
}

func TestWorkerUIDRecordWritesOnceWhenTheGroupForms(t *testing.T) {
	ctx := context.Background()
	lws := workerUIDLWS()
	leader := workerUIDLeader()
	worker := workerUIDPod(leader, workerUIDOld)
	counter := &workerUIDPatchCounter{}
	r := workerUIDReconciler(t, counter, lws, leader, worker)

	for range 2 {
		leader = readWorkerUIDLeader(t, r, leader)
		if err := r.recordWorkerUIDs(ctx, leader, lws); err != nil {
			t.Fatal(err)
		}
	}
	want := workerUIDRecordValue(workerUIDRevision, workerUIDOld)
	if got := readWorkerUIDLeader(t, r, leader).Annotations[leaderworkerset.WorkerUIDsAnnotationKey]; got != want {
		t.Fatalf("record = %q, want %q", got, want)
	}
	if counter.patches != 1 {
		t.Fatalf("patches = %d, want 1", counter.patches)
	}
}

func TestWorkerUIDRecordWaitsUntilEveryWorkerExists(t *testing.T) {
	lws := workerUIDLWS()
	leader := workerUIDLeader()
	r := workerUIDReconciler(t, nil, lws, leader)

	if err := r.recordWorkerUIDs(context.Background(), readWorkerUIDLeader(t, r, leader), lws); err != nil {
		t.Fatal(err)
	}
	if got := readWorkerUIDLeader(t, r, leader).Annotations[leaderworkerset.WorkerUIDsAnnotationKey]; got != "" {
		t.Fatalf("record = %q while the group is still forming", got)
	}
}

func TestWorkerUIDRecordProtectsAgainstStaleWrites(t *testing.T) {
	ctx := context.Background()
	for _, replaceLeader := range []bool{false, true} {
		t.Run(strconv.FormatBool(replaceLeader), func(t *testing.T) {
			lws := workerUIDLWS()
			leader := workerUIDLeader()
			worker := workerUIDPod(leader, workerUIDOld)
			r := workerUIDReconciler(t, nil, lws, leader, worker)
			stale := readWorkerUIDLeader(t, r, leader)
			want := workerUIDRecordValue(workerUIDRevision, workerUIDOld)
			if replaceLeader {
				if err := r.Delete(ctx, leader); err != nil {
					t.Fatal(err)
				}
				replacement := workerUIDLeader()
				replacement.UID = "new-leader-uid"
				want = workerUIDRecordValue(workerUIDRevision, workerUIDNew)
				replacement.Annotations[leaderworkerset.WorkerUIDsAnnotationKey] = want
				if err := r.Create(ctx, replacement); err != nil {
					t.Fatal(err)
				}
				leader = replacement
			} else {
				if err := r.recordWorkerUIDs(ctx, readWorkerUIDLeader(t, r, leader), lws); err != nil {
					t.Fatal(err)
				}
				if err := r.Delete(ctx, worker); err != nil {
					t.Fatal(err)
				}
				if err := r.Create(ctx, workerUIDPod(leader, workerUIDNew)); err != nil {
					t.Fatal(err)
				}
			}

			if err := r.recordWorkerUIDs(ctx, stale, lws); !apierrors.IsConflict(err) {
				t.Fatalf("error writing a stale record = %v, want Conflict", err)
			}
			if got := readWorkerUIDLeader(t, r, leader).Annotations[leaderworkerset.WorkerUIDsAnnotationKey]; got != want {
				t.Fatalf("record = %q after stale write, want %q", got, want)
			}
		})
	}
}

func TestWorkerUIDRecordFitsBesideExistingAnnotations(t *testing.T) {
	lws := workerUIDLWS()
	leader := workerUIDLeader()
	leader.Annotations["example.com/metadata"] = strings.Repeat("a", 255*1024)
	r := workerUIDReconciler(t, nil, lws, leader, workerUIDPod(leader, workerUIDOld))

	if err := r.recordWorkerUIDs(context.Background(), readWorkerUIDLeader(t, r, leader), lws); err != nil {
		t.Fatal(err)
	}
	stored := readWorkerUIDLeader(t, r, leader)
	if got := stored.Annotations[leaderworkerset.WorkerUIDsAnnotationKey]; !strings.Contains(got, "|sha256:") || len(got) > 128 {
		t.Fatalf("record = %q, want a compact digest", got)
	}
	if got := stored.Annotations["example.com/metadata"]; got != leader.Annotations["example.com/metadata"] {
		t.Fatal("record patch changed an existing annotation")
	}
}

func TestHandleRestartPolicyDoesNotRecreateWithoutAValidWorkerRecord(t *testing.T) {
	for _, tc := range []struct {
		name   string
		record string
	}{
		{name: "unrecorded"},
		{name: "arbitrary payload", record: workerUIDRevision + "|invalid"},
		{name: "plain UID list", record: workerUIDRevision + "|" + string(workerUIDOld)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lws := workerUIDLWS()
			leader := workerUIDLeader()
			leader.Annotations[leaderworkerset.WorkerUIDsAnnotationKey] = tc.record
			r := workerUIDReconciler(t, nil, lws, leader)

			deleted, _, err := r.handleRestartPolicy(context.Background(), *leader, *lws)
			if err != nil || deleted {
				t.Fatalf("handleRestartPolicy() = %t, %v, want no recreation without a valid record", deleted, err)
			}
		})
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

func TestHandleRestartPolicyReportsTheMissingWorkerName(t *testing.T) {
	lws := workerUIDLWS()
	leader := workerUIDLeader()
	leader.Annotations[leaderworkerset.WorkerUIDsAnnotationKey] = workerUIDRecordValue(workerUIDRevision, workerUIDOld)
	r := workerUIDReconciler(t, nil, lws, leader)
	recorder := events.NewFakeRecorder(1)
	r.Record = recorder

	deleted, _, err := r.handleRestartPolicy(context.Background(), *leader, *lws)
	if err != nil || !deleted {
		t.Fatalf("handleRestartPolicy() = %t, %v, want recreation", deleted, err)
	}
	select {
	case event := <-recorder.Events:
		if !strings.Contains(event, "Worker pod "+leader.Name+"-1 failed") {
			t.Fatalf("event = %q, want the missing worker's Pod name", event)
		}
	default:
		t.Fatal("no recreation event")
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

func TestHandleRestartPolicyAfterStartUsesRecordedStartupState(t *testing.T) {
	for _, tc := range []struct {
		name          string
		started       bool
		unschedulable bool
	}{
		{name: "record does not show startup"},
		{name: "started with Pending replacement", started: true},
		{name: "started with unschedulable replacement", started: true, unschedulable: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lws := wrappers.BuildLeaderWorkerSet("default").Replica(1).Size(2).RestartGroupAfterStart().Obj()
			leader := workerUIDLeader()
			leader.Status.Phase = corev1.PodRunning
			record := parseWorkerUIDRecord(workerUIDRecordValue(workerUIDRevision, workerUIDOld))
			record.started = tc.started
			leader.Annotations[leaderworkerset.WorkerUIDsAnnotationKey] = record.String()
			replacement := workerUIDPod(leader, workerUIDNew)
			replacement.Status.Phase = corev1.PodPending
			if tc.unschedulable {
				replacement.Status.Conditions = []corev1.PodCondition{{
					Type:   corev1.PodScheduled,
					Status: corev1.ConditionFalse,
					Reason: corev1.PodReasonUnschedulable,
				}}
			}
			r := workerUIDReconciler(t, nil, lws, leader, replacement)

			deleted, _, err := r.handleRestartPolicy(context.Background(), *leader, *lws)
			if err != nil || deleted != tc.started {
				t.Fatalf("handleRestartPolicy() = %t, %v, want recreation=%t", deleted, err, tc.started)
			}
		})
	}
}

func TestReconcileWorkerSkipsUIDChecksOnRoutineUpdates(t *testing.T) {
	lws := workerUIDLWS()
	leader := workerUIDLeader()
	worker := workerUIDPod(leader, workerUIDOld)
	leader.Annotations[leaderworkerset.WorkerUIDsAnnotationKey] = workerUIDRecordValue(workerUIDRevision, workerUIDOld)
	r := workerUIDReconciler(t, nil, lws, leader, worker)
	counter := &workerUIDListCounter{Client: r.Client}
	r.Client = counter

	if _, err := r.reconcilePod(context.Background(), podReconcileRequestForPod(worker, false)); err != nil {
		t.Fatal(err)
	}
	if counter.lists != 0 {
		t.Fatalf("routine worker update made %d List calls, want 0", counter.lists)
	}
}

func TestWorkerUIDRecordIgnoresStaleWorkers(t *testing.T) {
	lws := workerUIDLWS()
	leader := workerUIDLeader()
	leader.Annotations[leaderworkerset.WorkerUIDsAnnotationKey] = workerUIDRecordValue(workerUIDRevision, workerUIDOld)
	sts := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{
		Name: leader.Name, Namespace: leader.Namespace, UID: "current-worker-sts",
		OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(leader, corev1.SchemeGroupVersion.WithKind("Pod"))},
	}}
	current := workerUIDPod(leader, workerUIDOld)
	current.OwnerReferences = []metav1.OwnerReference{*metav1.NewControllerRef(sts, appsv1.SchemeGroupVersion.WithKind("StatefulSet"))}
	stale := current.DeepCopy()
	stale.Name = leader.Name + "-2"
	stale.UID = workerUIDNew
	stale.OwnerReferences[0].UID = "old-worker-sts"
	oldRevision := current.DeepCopy()
	oldRevision.Name = leader.Name + "-3"
	oldRevision.UID = "old-revision-worker"
	oldRevision.Labels[leaderworkerset.RevisionKey] = "old-revision"
	r := workerUIDReconciler(t, nil, lws, leader, sts, current, stale, oldRevision)

	deleted, _, err := r.handleRestartPolicy(context.Background(), *leader, *lws)
	if err != nil || deleted {
		t.Fatalf("handleRestartPolicy() = %t, %v, want only current workers to count", deleted, err)
	}
}

func TestReconcilePodRecordsWorkerUIDsAndRecreatesAfterRestart(t *testing.T) {
	scheme := podCtrlTestScheme(t)
	lws := workerUIDLWS()
	lws.Spec.StartupPolicy = leaderworkerset.LeaderReadyStartupPolicy
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
			WithIndex(&corev1.Pod{}, workerPodGroupIndex, indexWorkerPodGroup).
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
	if err := r.recordWorkerUIDs(context.Background(), readWorkerUIDLeader(t, r, leader), lws); err != nil {
		t.Fatal(err)
	}
	if got := readWorkerUIDLeader(t, r, leader).Annotations[leaderworkerset.WorkerUIDsAnnotationKey]; got != workerUIDRecordValue("next-revision", workerUIDNew) {
		t.Fatalf("annotation = %q, want the next revision's worker", got)
	}
}

func TestWorkerUIDRecordStartsFreshAfterLeaderReplacement(t *testing.T) {
	for _, identity := range []leaderworkerset.GroupIdentityType{leaderworkerset.GroupIdentityOrdinal, leaderworkerset.GroupIdentityHash} {
		t.Run(string(identity), func(t *testing.T) {
			ctx := context.Background()
			lws := workerUIDLWS()
			lws.Spec.GroupIdentity = identity
			oldLeader := workerUIDLeader()
			oldWorker := workerUIDPod(oldLeader, workerUIDOld)
			oldWorker.Name = oldLeader.Name + "-2"
			newLeader := workerUIDLeader()
			newLeader.UID = "replacement-leader"
			newLeader.Labels[leaderworkerset.RevisionKey] = "smaller-group-revision"
			if identity == leaderworkerset.GroupIdentityHash {
				newLeader.Name = "test-sample-9d55c5fbc-s6msg"
				newLeader.Spec.Hostname = "test-sample-9212d4c0"
				newLeader.Labels[leaderworkerset.GroupIndexLabelKey] = "9212d4c0"
			}
			newWorker := workerUIDPod(newLeader, workerUIDNew)
			newWorker.Name = workerStatefulSetName(newLeader) + "-1"
			r := workerUIDReconciler(t, nil, lws, newLeader, newWorker, oldWorker)

			if err := r.recordWorkerUIDs(ctx, readWorkerUIDLeader(t, r, newLeader), lws); err != nil {
				t.Fatal(err)
			}
			stored := readWorkerUIDLeader(t, r, newLeader)
			want := workerUIDRecordValue(newLeader.Labels[leaderworkerset.RevisionKey], workerUIDNew)
			if got := stored.Annotations[leaderworkerset.WorkerUIDsAnnotationKey]; got != want {
				t.Fatalf("replacement leader record = %q, want %q", got, want)
			}
			if lost, err := r.detectWorkerUIDLoss(ctx, stored); err != nil || lost != nil {
				t.Fatalf("worker loss = %+v, error = %v with the replacement group intact", lost, err)
			}
		})
	}
}

func workerUIDRecordValue(revision string, uid types.UID) string {
	return (workerUIDRecord{revision: revision, digest: digestUIDs([]string{string(uid)}), count: 1}).String()
}

func readWorkerUIDLeader(t *testing.T, r *PodReconciler, leader *corev1.Pod) *corev1.Pod {
	t.Helper()
	var stored corev1.Pod
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(leader), &stored); err != nil {
		t.Fatal(err)
	}
	return &stored
}

func workerUIDLWS() *leaderworkerset.LeaderWorkerSet {
	return wrappers.BuildLeaderWorkerSet("default").Replica(1).Size(2).
		RestartPolicy(leaderworkerset.RecreateGroupOnPodRestart).Obj()
}

func workerUIDLeader() *corev1.Pod {
	leader := wrappers.MakePodWithLabels("test-sample", "0", "0", "default", 2)
	leader.UID = "leader-uid"
	leader.Status.Phase = corev1.PodPending
	leader.Labels[leaderworkerset.RevisionKey] = workerUIDRevision
	return leader
}

func workerUIDPod(leader *corev1.Pod, uid types.UID) *corev1.Pod {
	worker := wrappers.MakePodWithLabels(leader.Labels[leaderworkerset.SetNameLabelKey], leader.Labels[leaderworkerset.GroupIndexLabelKey], "1", leader.Namespace, 2)
	worker.UID = uid
	worker.Status.Phase = corev1.PodPending
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
	builder := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&corev1.Pod{}).
		WithIndex(&corev1.Pod{}, workerPodGroupIndex, indexWorkerPodGroup).WithObjects(objects...)
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
	lists int
}

func (c *workerUIDListCounter) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	c.lists++
	return c.Client.List(ctx, list, opts...)
}

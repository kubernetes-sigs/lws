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
	"errors"
	"slices"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	leaderworkerset "sigs.k8s.io/lws/api/leaderworkerset/v1"
	revisionutils "sigs.k8s.io/lws/pkg/utils/revision"
	"sigs.k8s.io/lws/test/wrappers"
)

func workerDeletionTestPods(lws *leaderworkerset.LeaderWorkerSet) (*corev1.Pod, *corev1.Pod) {
	labels := func(workerIndex string) map[string]string {
		return map[string]string{
			leaderworkerset.SetNameLabelKey:     lws.Name,
			leaderworkerset.WorkerIndexLabelKey: workerIndex,
			leaderworkerset.GroupIndexLabelKey:  "0",
			leaderworkerset.RevisionKey:         "revision-1",
		}
	}
	leader := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: lws.Name + "-0", Namespace: lws.Namespace, UID: "leader", Labels: labels("0"),
	}}
	worker := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: lws.Name + "-0-1", Namespace: lws.Namespace, UID: "worker", Labels: labels("1"),
		OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(leader, corev1.SchemeGroupVersion.WithKind("Pod"))},
	}}
	return leader, worker
}

func newWorkerDeletionTestReconciler(objs ...client.Object) (*PodReconciler, client.Client) {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = appsv1.AddToScheme(scheme)
	_ = leaderworkerset.AddToScheme(scheme)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
	return &PodReconciler{Client: c, Scheme: scheme, Record: fakeEventRecorder{}}, c
}

func TestSyncWorkerDeletionFinalizerOnLivePods(t *testing.T) {
	cases := map[string]struct {
		policy        leaderworkerset.RestartPolicyType
		lwsDeleting   bool
		reconcileLead bool
		wantFinalizer bool
	}{
		"RecreateGroupOnPodRestart worker gets the finalizer": {
			policy: leaderworkerset.RecreateGroupOnPodRestart, wantFinalizer: true,
		},
		"RecreateGroupAfterStart worker gets the finalizer": {
			policy: leaderworkerset.RecreateGroupAfterStart, wantFinalizer: true,
		},
		"None policy worker does not get the finalizer": {
			policy: leaderworkerset.NoneRestartPolicy,
		},
		"worker of a deleting LWS does not get the finalizer": {
			policy: leaderworkerset.RecreateGroupOnPodRestart, lwsDeleting: true,
		},
		"leader does not get the finalizer": {
			policy: leaderworkerset.RecreateGroupOnPodRestart, reconcileLead: true,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			lws := wrappers.BuildLeaderWorkerSet("default").Name("test-sample").Replica(1).Size(2).
				RestartPolicy(tc.policy).Obj()
			if tc.lwsDeleting {
				lws.Finalizers = []string{"test/hold"}
				lws.DeletionTimestamp = ptrTime(metav1.Now())
			}
			leader, worker := workerDeletionTestPods(lws)
			r, c := newWorkerDeletionTestReconciler(lws, leader, worker)

			target := worker
			if tc.reconcileLead {
				target = leader
			}
			if _, err := r.reconcilePod(context.Background(), podReconcileRequestForPod(target, false)); err != nil {
				t.Fatalf("reconcilePod() error = %v", err)
			}
			var got corev1.Pod
			if err := c.Get(context.Background(), client.ObjectKeyFromObject(target), &got); err != nil {
				t.Fatalf("Get() error = %v", err)
			}
			if has := controllerutil.ContainsFinalizer(&got, leaderworkerset.WorkerDeletionFinalizer); has != tc.wantFinalizer {
				t.Errorf("finalizer present = %v, want %v (finalizers %v)", has, tc.wantFinalizer, got.Finalizers)
			}
		})
	}
}

func TestSyncWorkerDeletionFinalizerReleasesHandledDeletion(t *testing.T) {
	lws := wrappers.BuildLeaderWorkerSet("default").Name("test-sample").Replica(1).Size(2).
		RestartPolicy(leaderworkerset.RecreateGroupOnPodRestart).Obj()
	leader, worker := workerDeletionTestPods(lws)
	worker.Finalizers = []string{leaderworkerset.WorkerDeletionFinalizer}
	worker.DeletionTimestamp = ptrTime(metav1.Now())
	r, c := newWorkerDeletionTestReconciler(lws, leader, worker)

	if _, err := r.reconcilePod(context.Background(), podReconcileRequestForPod(worker, false)); err != nil {
		t.Fatalf("reconcilePod() error = %v", err)
	}
	// The terminating worker triggered group recreation, so the leader is gone.
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(leader), &corev1.Pod{}); !apierrors.IsNotFound(err) {
		t.Errorf("leader still exists after its worker was deleted, err = %v", err)
	}
	// Releasing the only finalizer lets the API server remove the worker.
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(worker), &corev1.Pod{}); !apierrors.IsNotFound(err) {
		t.Errorf("worker still exists after its deletion was handled, err = %v", err)
	}
}

func ptrTime(t metav1.Time) *metav1.Time { return &t }

func TestWorkerTemplateRestartPolicyAnnotation(t *testing.T) {
	cases := map[string]struct {
		policy leaderworkerset.RestartPolicyType
		want   string
	}{
		"RecreateGroupOnPodRestart is stamped": {
			policy: leaderworkerset.RecreateGroupOnPodRestart, want: string(leaderworkerset.RecreateGroupOnPodRestart),
		},
		"RecreateGroupAfterStart is stamped": {
			policy: leaderworkerset.RecreateGroupAfterStart, want: string(leaderworkerset.RecreateGroupAfterStart),
		},
		"None is not stamped": {
			policy: leaderworkerset.NoneRestartPolicy,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			lws := wrappers.BuildLeaderWorkerSet("default").Name("test-sample").Replica(1).Size(2).
				RestartPolicy(tc.policy).Obj()
			leader, _ := workerDeletionTestPods(lws)
			leader.Labels[leaderworkerset.GroupUniqueHashLabelKey] = "test-key"
			cfg, err := constructWorkerStatefulSetApplyConfiguration(*leader, *lws)
			if err != nil {
				t.Fatalf("constructWorkerStatefulSetApplyConfiguration() error = %v", err)
			}
			got, found := cfg.Spec.Template.Annotations[leaderworkerset.RestartPolicyAnnotationKey]
			if found != (tc.want != "") || got != tc.want {
				t.Errorf("template annotation %s = %q (present %v), want %q", leaderworkerset.RestartPolicyAnnotationKey, got, found, tc.want)
			}
		})
	}
}

func newWorkerDeletionInterceptedReconciler(funcs interceptor.Funcs, objs ...client.Object) (*PodReconciler, client.Client) {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = appsv1.AddToScheme(scheme)
	_ = leaderworkerset.AddToScheme(scheme)
	storage := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
	return &PodReconciler{Client: interceptor.NewClient(storage, funcs), Scheme: scheme, Record: fakeEventRecorder{}}, storage
}

func terminatingWorkerWithFinalizer(worker *corev1.Pod) {
	worker.Finalizers = []string{leaderworkerset.WorkerDeletionFinalizer}
	worker.DeletionTimestamp = ptrTime(metav1.Now())
}

func workerFinalizers(t *testing.T, c client.Client, worker *corev1.Pod) []string {
	t.Helper()
	var got corev1.Pod
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(worker), &got); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		t.Fatalf("Get() error = %v", err)
	}
	return got.Finalizers
}

// The finalizer decision must use the snapshot that was reconciled. If only a
// later read observed the deletion, releasing the finalizer would drop a
// deletion whose restart policy was never applied.
func TestWorkerDeletionFinalizerUsesReconciledSnapshot(t *testing.T) {
	lws := wrappers.BuildLeaderWorkerSet("default").Name("test-sample").Replica(1).Size(2).
		RestartPolicy(leaderworkerset.RecreateGroupOnPodRestart).Obj()
	leader, worker := workerDeletionTestPods(lws)
	terminatingWorkerWithFinalizer(worker)
	workerReads := 0
	r, c := newWorkerDeletionInterceptedReconciler(interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if err := c.Get(ctx, key, obj, opts...); err != nil {
				return err
			}
			if pod, ok := obj.(*corev1.Pod); ok && key == client.ObjectKeyFromObject(worker) {
				workerReads++
				if workerReads == 1 {
					// The first read comes from a cache that has not seen the deletion yet.
					pod.DeletionTimestamp = nil
				}
			}
			return nil
		},
	}, lws, leader, worker)

	if _, err := r.reconcilePod(context.Background(), podReconcileRequestForPod(worker, false)); err != nil {
		t.Fatalf("reconcilePod() error = %v", err)
	}
	if got := workerFinalizers(t, c, worker); len(got) != 1 || got[0] != leaderworkerset.WorkerDeletionFinalizer {
		t.Errorf("worker finalizers = %v, want the worker-deletion finalizer retained until its deletion is reconciled", got)
	}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(leader), &corev1.Pod{}); err != nil {
		t.Errorf("leader was deleted although the reconciled snapshot showed a live worker, err = %v", err)
	}
}

// A merge patch replaces the whole finalizer list, so a patch built from a
// stale snapshot must not overwrite a finalizer added in the meantime.
func TestWorkerDeletionFinalizerKeepsConcurrentFinalizer(t *testing.T) {
	const other = "test/other"
	lws := wrappers.BuildLeaderWorkerSet("default").Name("test-sample").Replica(1).Size(2).
		RestartPolicy(leaderworkerset.RecreateGroupOnPodRestart).Obj()
	leader, worker := workerDeletionTestPods(lws)
	patched := false
	r, c := newWorkerDeletionInterceptedReconciler(interceptor.Funcs{
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			if !patched && client.ObjectKeyFromObject(obj) == client.ObjectKeyFromObject(worker) {
				patched = true
				// Another writer adds a finalizer after the snapshot was read.
				var current corev1.Pod
				if err := c.Get(ctx, client.ObjectKeyFromObject(worker), &current); err != nil {
					return err
				}
				current.Finalizers = append(current.Finalizers, other)
				if err := c.Update(ctx, &current); err != nil {
					return err
				}
			}
			return c.Patch(ctx, obj, patch, opts...)
		},
	}, lws, leader, worker)

	result, err := r.reconcilePod(context.Background(), podReconcileRequestForPod(worker, false))
	if err != nil {
		t.Fatalf("reconcilePod() error = %v, want a requeue after the conflict", err)
	}
	if !result.Requeue {
		t.Errorf("reconcilePod() result = %+v, want Requeue after the conflict", result)
	}
	if got := workerFinalizers(t, c, worker); len(got) != 1 || got[0] != other {
		t.Fatalf("worker finalizers after the conflict = %v, want only %q", got, other)
	}
	// The retry starts from a fresh snapshot and keeps both finalizers.
	if _, err := r.reconcilePod(context.Background(), podReconcileRequestForPod(worker, false)); err != nil {
		t.Fatalf("second reconcilePod() error = %v", err)
	}
	got := workerFinalizers(t, c, worker)
	if !slices.Contains(got, other) || !slices.Contains(got, leaderworkerset.WorkerDeletionFinalizer) {
		t.Errorf("worker finalizers after the retry = %v, want %q and the worker-deletion finalizer", got, other)
	}
}

func TestWorkerDeletionFinalizerRetainedWhenHandlingFails(t *testing.T) {
	lws := wrappers.BuildLeaderWorkerSet("default").Name("test-sample").Replica(1).Size(2).
		RestartPolicy(leaderworkerset.RecreateGroupOnPodRestart).Obj()
	leader, worker := workerDeletionTestPods(lws)
	terminatingWorkerWithFinalizer(worker)
	r, c := newWorkerDeletionInterceptedReconciler(interceptor.Funcs{
		Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			if client.ObjectKeyFromObject(obj) == client.ObjectKeyFromObject(leader) {
				return errors.New("injected leader deletion failure")
			}
			return c.Delete(ctx, obj, opts...)
		},
	}, lws, leader, worker)

	if _, err := r.reconcilePod(context.Background(), podReconcileRequestForPod(worker, false)); err == nil {
		t.Fatal("reconcilePod() error = nil, want the leader deletion failure")
	}
	if got := workerFinalizers(t, c, worker); len(got) != 1 || got[0] != leaderworkerset.WorkerDeletionFinalizer {
		t.Errorf("worker finalizers = %v, want the worker-deletion finalizer retained for the retry", got)
	}
}

// When restartBackoff defers a recreation, the decision is persisted on the
// leader before the worker's finalizer is released, so a controller restart
// during the backoff does not lose it.
func TestWorkerDeletionFinalizerReleasedAfterBackoffIsPersisted(t *testing.T) {
	lws := wrappers.BuildLeaderWorkerSet("default").Name("test-sample").Replica(1).Size(2).
		RestartPolicy(leaderworkerset.RecreateGroupOnPodRestart).
		RestartBackoff(ptr.To[int32](3600), ptr.To[int32](3600)).Obj()
	leader, worker := workerDeletionTestPods(lws)
	leader.CreationTimestamp = metav1.Now()
	terminatingWorkerWithFinalizer(worker)
	r, c := newWorkerDeletionTestReconciler(lws, leader, worker)

	result, err := r.reconcilePod(context.Background(), podReconcileRequestForPod(worker, false))
	if err != nil {
		t.Fatalf("reconcilePod() error = %v", err)
	}
	if result.RequeueAfter <= 0 {
		t.Errorf("reconcilePod() result = %+v, want a requeue after the backoff", result)
	}
	var gotLeader corev1.Pod
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(leader), &gotLeader); err != nil {
		t.Fatalf("leader Get() error = %v, want the leader kept during backoff", err)
	}
	if gotLeader.Annotations[leaderworkerset.GroupRecreatePendingAnnotationKey] == "" {
		t.Errorf("leader annotations = %v, want the pending recreation persisted", gotLeader.Annotations)
	}
	if got := workerFinalizers(t, c, worker); len(got) != 0 {
		t.Errorf("worker finalizers = %v, want the finalizer released once the decision is persisted", got)
	}
}

func TestWorkerDeletionFinalizerFollowsGroupRevisionPolicy(t *testing.T) {
	t.Run("live None policy skips the revision lookup", func(t *testing.T) {
		lws := wrappers.BuildLeaderWorkerSet("default").Name("test-sample").Replica(1).Size(2).
			RestartPolicy(leaderworkerset.NoneRestartPolicy).Obj()
		leader, worker := workerDeletionTestPods(lws)
		revisionLists := 0
		r, c := newWorkerDeletionInterceptedReconciler(interceptor.Funcs{
			List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				if _, ok := list.(*appsv1.ControllerRevisionList); ok {
					revisionLists++
				}
				return c.List(ctx, list, opts...)
			},
		}, lws, leader, worker)
		if _, err := r.reconcilePod(context.Background(), podReconcileRequestForPod(worker, false)); err != nil {
			t.Fatalf("reconcilePod() error = %v", err)
		}
		if revisionLists != 0 {
			t.Errorf("ControllerRevision lists = %d, want none for a routine worker of a None group", revisionLists)
		}
		if got := workerFinalizers(t, c, worker); len(got) != 0 {
			t.Errorf("worker finalizers = %v, want none", got)
		}
	})

	t.Run("worker of a None revision gets no finalizer under a live Recreate policy", func(t *testing.T) {
		lws := wrappers.BuildLeaderWorkerSet("default").Name("test-sample").Replica(1).Size(2).
			RestartPolicy(leaderworkerset.RecreateGroupOnPodRestart).Obj()
		oldLws := lws.DeepCopy()
		oldLws.Spec.LeaderWorkerTemplate.RestartPolicy = leaderworkerset.NoneRestartPolicy
		leader, worker := workerDeletionTestPods(lws)
		r, c := newWorkerDeletionTestReconciler(lws, leader, worker)
		revision, err := revisionutils.NewRevision(context.Background(), c, oldLws, "")
		if err != nil {
			t.Fatalf("NewRevision() error = %v", err)
		}
		if err := c.Create(context.Background(), revision); err != nil {
			t.Fatalf("Create(revision) error = %v", err)
		}
		var current corev1.Pod
		if err := c.Get(context.Background(), client.ObjectKeyFromObject(worker), &current); err != nil {
			t.Fatalf("Get(worker) error = %v", err)
		}
		current.Labels[leaderworkerset.RevisionKey] = revisionutils.GetRevisionKey(revision)
		if err := c.Update(context.Background(), &current); err != nil {
			t.Fatalf("Update(worker) error = %v", err)
		}
		if _, err := r.reconcilePod(context.Background(), podReconcileRequestForPod(&current, false)); err != nil {
			t.Fatalf("reconcilePod() error = %v", err)
		}
		if got := workerFinalizers(t, c, worker); len(got) != 0 {
			t.Errorf("worker finalizers = %v, want none for a worker whose revision uses None", got)
		}
	})
}

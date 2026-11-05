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

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	leaderworkerset "sigs.k8s.io/lws/api/leaderworkerset/v1"
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

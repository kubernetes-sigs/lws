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

	"github.com/prometheus/client_golang/prometheus/testutil"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	leaderworkerset "sigs.k8s.io/lws/api/leaderworkerset/v1"
	"sigs.k8s.io/lws/pkg/metrics"
	"sigs.k8s.io/lws/test/wrappers"
)

// podCtrlCounters holds the pod controller counters of one LeaderWorkerSet.
type podCtrlCounters struct {
	recreations     float64
	budgetExhausted float64
	backoffs        float64
}

func podCtrlReadCounters(lws *leaderworkerset.LeaderWorkerSet) podCtrlCounters {
	return podCtrlCounters{
		recreations:     testutil.ToFloat64(metrics.GroupRecreationsTotal.WithLabelValues(lws.Namespace, lws.Name, metrics.GroupRecreateReasonPodRestart)),
		budgetExhausted: testutil.ToFloat64(metrics.GroupRestartBudgetExhaustedTotal.WithLabelValues(lws.Namespace, lws.Name)),
		backoffs:        testutil.ToFloat64(metrics.GroupRestartBackoffsTotal.WithLabelValues(lws.Namespace, lws.Name)),
	}
}

// TestHandleRestartPolicyMetrics checks that each restart-policy outcome is
// counted once per group, however often the group is reconciled afterwards.
func TestHandleRestartPolicyMetrics(t *testing.T) {
	scheme := podCtrlTestScheme(t)

	restartedLeader := func(lws *leaderworkerset.LeaderWorkerSet, age time.Duration) *corev1.Pod {
		leader := wrappers.MakePodWithLabels(lws.Name, "0", "0", lws.Namespace, 1)
		leader.Labels[leaderworkerset.RevisionKey] = "revision-a"
		leader.CreationTimestamp = metav1.NewTime(time.Now().Add(-age))
		leader.Status.Phase = corev1.PodRunning
		leader.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "leader", RestartCount: 1}}
		return leader
	}

	tests := []struct {
		name string
		lws  *leaderworkerset.LeaderWorkerSet
		// leaderAge is how long ago the leader pod was created.
		leaderAge time.Duration
		want      podCtrlCounters
	}{
		{
			name: "a restarted container recreates the group",
			lws:  wrappers.BuildLeaderWorkerSet("metrics-recreate").Size(1).Obj(),
			want: podCtrlCounters{recreations: 1},
		},
		{
			name: "a used up restart budget terminates the group",
			lws:  wrappers.BuildLeaderWorkerSet("metrics-budget").Size(1).MaxGroupRestarts(0).Obj(),
			want: podCtrlCounters{budgetExhausted: 1},
		},
		{
			name:      "restart backoff delays the recreation",
			lws:       wrappers.BuildLeaderWorkerSet("metrics-backoff").Size(1).RestartBackoff(ptr.To[int32](10), ptr.To[int32](300)).Obj(),
			leaderAge: 2 * time.Second,
			want:      podCtrlCounters{backoffs: 1},
		},
		{
			name:      "a recreation after the backoff window is not a backoff",
			lws:       wrappers.BuildLeaderWorkerSet("metrics-backoff-passed").Size(1).RestartBackoff(ptr.To[int32](10), ptr.To[int32](300)).Obj(),
			leaderAge: 12 * time.Second,
			want:      podCtrlCounters{recreations: 1},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			leader := restartedLeader(tc.lws, tc.leaderAge)
			c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(tc.lws, leader).Build()
			r := &PodReconciler{Client: c, Scheme: scheme, Record: fakeEventRecorder{}}
			before := podCtrlReadCounters(tc.lws)

			// A second reconcile sees the group terminating or its recreation
			// pending and must not count it again.
			for range 2 {
				if _, _, err := r.handleRestartPolicy(context.Background(), *leader, *tc.lws.DeepCopy()); err != nil {
					t.Fatalf("handleRestartPolicy() error = %v", err)
				}
			}

			after := podCtrlReadCounters(tc.lws)
			got := podCtrlCounters{
				recreations:     after.recreations - before.recreations,
				budgetExhausted: after.budgetExhausted - before.budgetExhausted,
				backoffs:        after.backoffs - before.backoffs,
			}
			if got != tc.want {
				t.Errorf("counter increments = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestHandleRestartPolicyMetricsHealthyWorker(t *testing.T) {
	scheme := podCtrlTestScheme(t)
	lws := wrappers.BuildLeaderWorkerSet("metrics-healthy").Size(2).Obj()
	leader := wrappers.MakePodWithLabels(lws.Name, "0", "0", lws.Namespace, 2)
	worker := wrappers.MakePodWithLabels(lws.Name, "0", "1", lws.Namespace, 2)
	worker.Status.Phase = corev1.PodRunning
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(lws, leader, worker).Build()
	r := &PodReconciler{Client: c, Scheme: scheme, Record: fakeEventRecorder{}}
	before := podCtrlReadCounters(lws)

	deleted, _, err := r.handleRestartPolicy(context.Background(), *worker, *lws.DeepCopy())
	if err != nil || deleted {
		t.Fatalf("handleRestartPolicy() = %t, %v, want false and no error", deleted, err)
	}
	if after := podCtrlReadCounters(lws); after != before {
		t.Errorf("counters changed from %+v to %+v for a healthy worker", before, after)
	}
}

func TestLeaderWorkerSetReconcilerClearsMetricsOfDeletedLWS(t *testing.T) {
	scheme := podCtrlTestScheme(t)
	gone := types.NamespacedName{Namespace: "metrics-deleted", Name: "test-sample"}
	kept := types.NamespacedName{Namespace: "metrics-kept", Name: "test-sample"}
	metrics.GroupRecreated(gone.Namespace, gone.Name, metrics.GroupRecreateReasonPodRestart)
	metrics.GroupRecreated(gone.Namespace, gone.Name, metrics.GroupRecreateReasonUpgradeSplit)
	metrics.GroupRestartBudgetExhausted(gone.Namespace, gone.Name)
	metrics.GroupRestartBackedOff(gone.Namespace, gone.Name)
	metrics.GroupRecreated(kept.Namespace, kept.Name, metrics.GroupRecreateReasonPodRestart)
	t.Cleanup(func() { metrics.ClearLeaderWorkerSet(kept.Namespace, kept.Name) })

	r := NewLeaderWorkerSetReconciler(fake.NewClientBuilder().WithScheme(scheme).Build(), scheme, fakeEventRecorder{})
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: gone}); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	// DeleteLabelValues reports whether the series existed.
	for name, existed := range map[string]bool{
		"group_recreations_total PodRestart":   metrics.GroupRecreationsTotal.DeleteLabelValues(gone.Namespace, gone.Name, metrics.GroupRecreateReasonPodRestart),
		"group_recreations_total UpgradeSplit": metrics.GroupRecreationsTotal.DeleteLabelValues(gone.Namespace, gone.Name, metrics.GroupRecreateReasonUpgradeSplit),
		"group_restart_budget_exhausted_total": metrics.GroupRestartBudgetExhaustedTotal.DeleteLabelValues(gone.Namespace, gone.Name),
		"group_restart_backoffs_total":         metrics.GroupRestartBackoffsTotal.DeleteLabelValues(gone.Namespace, gone.Name),
	} {
		if existed {
			t.Errorf("the deleted LeaderWorkerSet still has a %s series", name)
		}
	}
	if got := testutil.ToFloat64(metrics.GroupRecreationsTotal.WithLabelValues(kept.Namespace, kept.Name, metrics.GroupRecreateReasonPodRestart)); got != 1 {
		t.Errorf("group_recreations_total of another LeaderWorkerSet = %v, want 1", got)
	}
}

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
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	leaderworkerset "sigs.k8s.io/lws/api/leaderworkerset/v1"
	"sigs.k8s.io/lws/test/wrappers"
)

func restartBudgetTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{corev1.AddToScheme, leaderworkerset.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	return scheme
}

// Multiple workers can observe an updated LWS while still seeing the old
// leader alive. The UID, rather than the Pod watch, identifies a charged failure.
func TestHandleRestartPolicyChargesBudgetOncePerGroupFailure(t *testing.T) {
	for _, tc := range []struct {
		name       string
		limit      *int32
		backoff    bool
		afterStart bool
		charged    types.UID
		wantCount  int32
	}{
		{name: "already charged", limit: ptr.To[int32](3), charged: "failed-leader", wantCount: 1},
		{name: "already charged at budget limit", limit: ptr.To[int32](1), charged: "failed-leader", wantCount: 1},
		{name: "already charged with backoff only", backoff: true, charged: "failed-leader", wantCount: 1},
		{name: "already charged before start cache catches up", limit: ptr.To[int32](3), afterStart: true, charged: "failed-leader", wantCount: 1},
		{name: "replacement leader is a new failure", limit: ptr.To[int32](3), charged: "previous-leader", wantCount: 2},
		{name: "existing count without UID remains usable", limit: ptr.To[int32](3), wantCount: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			scheme := restartBudgetTestScheme(t)
			lws := wrappers.BuildLeaderWorkerSet("default").Replica(1).Size(3).
				RestartPolicy(leaderworkerset.RecreateGroupOnPodRestart).Obj()
			lws.Spec.LeaderWorkerTemplate.MaxGroupRestarts = tc.limit
			if tc.backoff {
				lws.Spec.LeaderWorkerTemplate.RestartBackoff = &leaderworkerset.RestartBackoff{BaseSeconds: ptr.To[int32](60)}
			}
			if tc.afterStart {
				lws.Spec.LeaderWorkerTemplate.RestartPolicy = leaderworkerset.RecreateGroupAfterStart
			}
			lws.Annotations = map[string]string{leaderworkerset.GroupRestartCountsAnnotationKey: `{"revision-a/0":1}`}
			if tc.charged != "" {
				lws.Annotations[leaderworkerset.GroupRestartChargedLeadersAnnotationKey] = fmt.Sprintf(`{"revision-a/0":%q}`, tc.charged)
			}
			leader := wrappers.MakePodWithLabels(lws.Name, "0", "0", lws.Namespace, 3)
			leader.UID = "failed-leader"
			leader.Labels[leaderworkerset.RevisionKey] = "revision-a"
			leader.CreationTimestamp = metav1.Now()
			leader.Finalizers = []string{"test.lws/retain"}
			worker := wrappers.MakePodWithLabels(lws.Name, "0", "1", lws.Namespace, 3)
			worker.Labels[leaderworkerset.RevisionKey] = "revision-a"
			worker.Status.ContainerStatuses = []corev1.ContainerStatus{{RestartCount: 1}}
			worker.OwnerReferences = []metav1.OwnerReference{*metav1.NewControllerRef(leader, corev1.SchemeGroupVersion.WithKind("Pod"))}
			cli := fake.NewClientBuilder().WithScheme(scheme).WithObjects(lws, leader, worker).Build()
			r := &PodReconciler{Client: cli, Record: fakeEventRecorder{}}
			deleted, result, err := r.handleRestartPolicy(t.Context(), *worker, *lws)
			if err != nil || !deleted || result.RequeueAfter != 0 {
				t.Fatalf("handleRestartPolicy() = (%t, %+v, %v), want immediate deletion", deleted, result, err)
			}
			var got leaderworkerset.LeaderWorkerSet
			if err := cli.Get(t.Context(), client.ObjectKeyFromObject(lws), &got); err != nil {
				t.Fatal(err)
			}
			assertRestartBudgetState(t, &got, map[string]int32{"revision-a/0": tc.wantCount}, map[string]types.UID{"revision-a/0": leader.UID})
			var terminating corev1.Pod
			if err := cli.Get(t.Context(), client.ObjectKeyFromObject(leader), &terminating); err != nil {
				t.Fatal(err)
			}
			if terminating.DeletionTimestamp == nil || terminating.Annotations[leaderworkerset.GroupRestartBudgetExhaustedAnnotationKey] == "true" {
				t.Fatalf("expected a deleting leader without exhaustion, got %+v", terminating.ObjectMeta)
			}
		})
	}
}

func TestHandleRestartPolicyRetriesDeletionAfterPersistingCharge(t *testing.T) {
	scheme := restartBudgetTestScheme(t)
	lws := wrappers.BuildLeaderWorkerSet("default").Replica(1).Size(1).
		RestartPolicy(leaderworkerset.RecreateGroupOnPodRestart).MaxGroupRestarts(1).Obj()
	lws.Spec.LeaderWorkerTemplate.RestartBackoff = &leaderworkerset.RestartBackoff{BaseSeconds: ptr.To[int32](60)}
	leader := wrappers.MakePodWithLabels(lws.Name, "0", "0", lws.Namespace, 1)
	leader.UID = "failed-leader"
	leader.Labels[leaderworkerset.RevisionKey] = "revision-a"
	leader.CreationTimestamp = metav1.NewTime(time.Now().Add(-90 * time.Second))
	leader.Status.ContainerStatuses = []corev1.ContainerStatus{{RestartCount: 1}}
	leader.Finalizers = []string{"test.lws/retain"}
	deleteErr := errors.New("temporary delete failure")
	deleteCalls := 0
	cli := fake.NewClientBuilder().WithScheme(scheme).WithObjects(lws, leader).
		WithInterceptorFuncs(interceptor.Funcs{
			Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
				deleteCalls++
				var current leaderworkerset.LeaderWorkerSet
				if err := c.Get(ctx, client.ObjectKeyFromObject(lws), &current); err != nil {
					t.Fatal(err)
				}
				// Both values must already be durable before either delete attempt.
				assertRestartBudgetState(t, &current, map[string]int32{"revision-a/0": 1}, map[string]types.UID{"revision-a/0": leader.UID})
				if deleteCalls == 1 {
					return deleteErr
				}
				return c.Delete(ctx, obj, opts...)
			},
		}).Build()
	r := &PodReconciler{Client: cli, Record: fakeEventRecorder{}}
	if _, _, err := r.handleRestartPolicy(t.Context(), *leader, *lws); !errors.Is(err, deleteErr) {
		t.Fatalf("first reconcile error = %v, want delete error", err)
	}
	deleted, result, err := r.handleRestartPolicy(t.Context(), *leader, *lws)
	if err != nil || !deleted || result.RequeueAfter != 0 || deleteCalls != 2 {
		t.Fatalf("retry = (%t, %+v, %v), deletes=%d; want deletion without exhaustion or backoff", deleted, result, err, deleteCalls)
	}
}

func TestHandleRestartPolicyDeletesOnlyObservedLeader(t *testing.T) {
	for _, state := range []string{"deleting", "gone", "replaced"} {
		t.Run(state, func(t *testing.T) {
			scheme := restartBudgetTestScheme(t)
			lws := wrappers.BuildLeaderWorkerSet("default").Replica(1).Size(1).
				RestartPolicy(leaderworkerset.RecreateGroupOnPodRestart).MaxGroupRestarts(1).Obj()
			lws.Annotations = map[string]string{
				leaderworkerset.GroupRestartCountsAnnotationKey:         `{"revision-a/0":1}`,
				leaderworkerset.GroupRestartChargedLeadersAnnotationKey: `{"revision-a/0":"failed-leader"}`,
			}
			cached := wrappers.MakePodWithLabels(lws.Name, "0", "0", lws.Namespace, 1)
			cached.UID = "failed-leader"
			cached.Labels[leaderworkerset.RevisionKey] = "revision-a"
			cached.Status.ContainerStatuses = []corev1.ContainerStatus{{RestartCount: 1}}
			live := cached.DeepCopy()
			objects := []client.Object{lws}
			if state == "deleting" {
				live.Finalizers = []string{"test.lws/retain"}
				now := metav1.Now()
				live.DeletionTimestamp = &now
			} else if state == "replaced" {
				live.UID = "replacement-leader"
			}
			if state != "gone" {
				objects = append(objects, live)
			}
			deleteCalls := 0
			cli := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).
				WithInterceptorFuncs(interceptor.Funcs{
					Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
						if pod, ok := obj.(*corev1.Pod); ok {
							*pod = *cached.DeepCopy()
							return nil
						}
						return c.Get(ctx, key, obj, opts...)
					},
					Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
						deleteCalls++
						options := (&client.DeleteOptions{}).ApplyOptions(opts)
						if options.Preconditions == nil || ptr.Deref(options.Preconditions.UID, "") != cached.UID {
							t.Fatalf("missing observed leader UID precondition: %+v", options.Preconditions)
						}
						// The fake client does not enforce UID preconditions.
						if state == "replaced" {
							return apierrors.NewConflict(corev1.Resource("pods"), obj.GetName(), errors.New("UID precondition failed"))
						}
						return c.Delete(ctx, obj, opts...)
					},
				}).Build()
			r := &PodReconciler{Client: cli, Record: fakeEventRecorder{}}
			deleted, _, err := r.handleRestartPolicy(t.Context(), *cached, *lws)
			if err != nil || !deleted || deleteCalls != 1 {
				t.Fatalf("reconcile = (%t, %v), deletes=%d", deleted, err, deleteCalls)
			}
			var current leaderworkerset.LeaderWorkerSet
			if err := cli.Get(t.Context(), client.ObjectKeyFromObject(lws), &current); err != nil {
				t.Fatal(err)
			}
			assertRestartBudgetState(t, &current, map[string]int32{"revision-a/0": 1}, map[string]types.UID{"revision-a/0": cached.UID})
			if state == "replaced" {
				var pods corev1.PodList
				if err := cli.List(t.Context(), &pods); err != nil {
					t.Fatal(err)
				}
				if len(pods.Items) != 1 || pods.Items[0].UID != live.UID || pods.Items[0].DeletionTimestamp != nil {
					t.Fatalf("replacement leader was affected: %+v", pods.Items)
				}
			}
		})
	}
}

func TestPersistGroupRestartCountDeduplicatesAfterConflict(t *testing.T) {
	scheme := restartBudgetTestScheme(t)
	lws := wrappers.BuildLeaderWorkerSet("default").Obj()
	leader := wrappers.MakePodWithLabels(lws.Name, "0", "0", lws.Namespace, 1)
	leader.UID = "failed-leader"
	leader.Labels[leaderworkerset.RevisionKey] = "revision-a"
	updates := 0
	cli := fake.NewClientBuilder().WithScheme(scheme).WithObjects(lws).
		WithInterceptorFuncs(interceptor.Funcs{
			Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
				updates++
				if updates != 1 {
					t.Fatal("reconcile tried to write a second charge after conflict")
				}
				// Another worker commits the same failure and another group's
				// counter after this reconcile read the cached LWS.
				var current leaderworkerset.LeaderWorkerSet
				if err := c.Get(ctx, client.ObjectKeyFromObject(lws), &current); err != nil {
					return err
				}
				current.Annotations = map[string]string{
					leaderworkerset.GroupRestartCountsAnnotationKey:         `{"revision-a/0":1,"revision-a/1":2}`,
					leaderworkerset.GroupRestartChargedLeadersAnnotationKey: `{"revision-a/0":"failed-leader","revision-a/1":"other-leader"}`,
				}
				if err := c.Update(ctx, &current); err != nil {
					return err
				}
				return apierrors.NewConflict(leaderworkerset.Resource("leaderworkersets"), obj.GetName(), errors.New("stale resource version"))
			},
		}).Build()
	r := &PodReconciler{Client: cli}
	if err := r.persistGroupRestartCount(t.Context(), lws, leader, 1); err != nil {
		t.Fatal(err)
	}
	// A later reconcile sees the increased count but the same leader UID.
	if err := r.persistGroupRestartCount(t.Context(), lws, leader, 2); err != nil {
		t.Fatal(err)
	}
	var current leaderworkerset.LeaderWorkerSet
	if err := cli.Get(t.Context(), client.ObjectKeyFromObject(lws), &current); err != nil {
		t.Fatal(err)
	}
	assertRestartBudgetState(t, &current,
		map[string]int32{"revision-a/0": 1, "revision-a/1": 2},
		map[string]types.UID{"revision-a/0": leader.UID, "revision-a/1": "other-leader"})
	if updates != 1 {
		t.Fatalf("update attempts = %d, want 1", updates)
	}
}

func TestRestartBudgetPrunesChargedLeadersWithCounts(t *testing.T) {
	for _, action := range []string{"recovery", "revision", "scale-down", "surge", "hash handoff", "hash scale-down", "last entry"} {
		t.Run(action, func(t *testing.T) {
			scheme := restartBudgetTestScheme(t)
			lws := wrappers.BuildLeaderWorkerSet("default").Replica(1).Obj()
			lws.Annotations = map[string]string{
				leaderworkerset.GroupRestartCountsAnnotationKey:         `{"revision-a/0":1,"revision-a/1":2,"old-revision/0":3}`,
				leaderworkerset.GroupRestartChargedLeadersAnnotationKey: `{"revision-a/0":"leader-0","revision-a/1":"leader-1","old-revision/0":"old-leader"}`,
			}
			leader := wrappers.MakePodWithLabels(lws.Name, "0", "0", lws.Namespace, 1)
			leader.Labels[leaderworkerset.RevisionKey] = "revision-a"
			if action == "last entry" {
				lws.Annotations[leaderworkerset.GroupRestartCountsAnnotationKey] = `{"revision-a/0":1}`
				lws.Annotations[leaderworkerset.GroupRestartChargedLeadersAnnotationKey] = `{"revision-a/0":"leader-0"}`
			}
			cli := fake.NewClientBuilder().WithScheme(scheme).WithObjects(lws).Build()
			pods := &PodReconciler{Client: cli}
			sets := &LeaderWorkerSetReconciler{Client: cli}
			var err error
			wantCounts := map[string]int32{}
			wantCharged := map[string]types.UID{}
			switch action {
			case "recovery":
				err = pods.clearGroupRestartCount(t.Context(), lws, leader)
				wantCounts = map[string]int32{"revision-a/1": 2, "old-revision/0": 3}
				wantCharged = map[string]types.UID{"revision-a/1": "leader-1", "old-revision/0": "old-leader"}
			case "revision":
				err = sets.cleanupObsoleteGroupRestartCounts(t.Context(), lws, "revision-a")
				wantCounts = map[string]int32{"revision-a/0": 1, "revision-a/1": 2}
				wantCharged = map[string]types.UID{"revision-a/0": "leader-0", "revision-a/1": "leader-1"}
			case "scale-down":
				err = sets.pruneScaledDownGroupRestartCounts(t.Context(), lws, 1)
				wantCounts = map[string]int32{"revision-a/0": 1, "old-revision/0": 3}
				wantCharged = map[string]types.UID{"revision-a/0": "leader-0", "old-revision/0": "old-leader"}
			case "surge":
				err = sets.pruneScaledDownGroupRestartCounts(t.Context(), lws, 2)
				wantCounts = map[string]int32{"revision-a/0": 1, "revision-a/1": 2, "old-revision/0": 3}
				wantCharged = map[string]types.UID{"revision-a/0": "leader-0", "revision-a/1": "leader-1", "old-revision/0": "old-leader"}
			case "last entry":
				err = pods.clearGroupRestartCount(t.Context(), lws, leader)
			case "hash handoff":
				leader.Labels[leaderworkerset.GroupIndexLabelKey] = "new-group"
				err = pods.claimGroupRestartCountForHashLeader(t.Context(), lws, leader)
				wantCounts = map[string]int32{"revision-a/0": 1, "revision-a/new-group": 2, "old-revision/0": 3}
				wantCharged = map[string]types.UID{"revision-a/0": "leader-0", "old-revision/0": "old-leader"}
			case "hash scale-down":
				err = sets.pruneHashGroupRestartCounts(t.Context(), lws, "revision-a")
				wantCounts = map[string]int32{"revision-a/1": 2, "old-revision/0": 3}
				wantCharged = map[string]types.UID{"revision-a/1": "leader-1", "old-revision/0": "old-leader"}
			}
			if err != nil {
				t.Fatal(err)
			}
			var current leaderworkerset.LeaderWorkerSet
			if err := cli.Get(t.Context(), client.ObjectKeyFromObject(lws), &current); err != nil {
				t.Fatal(err)
			}
			assertRestartBudgetState(t, &current, wantCounts, wantCharged)
			if action == "last entry" {
				for _, key := range []string{leaderworkerset.GroupRestartCountsAnnotationKey, leaderworkerset.GroupRestartChargedLeadersAnnotationKey} {
					if _, found := current.Annotations[key]; found {
						t.Fatalf("empty annotation %s was not removed", key)
					}
				}
			}
		})
	}
}

func assertRestartBudgetState(t *testing.T, lws *leaderworkerset.LeaderWorkerSet, wantCounts map[string]int32, wantCharged map[string]types.UID) {
	t.Helper()
	counts, err := parseGroupRestartCounts(lws.Annotations[leaderworkerset.GroupRestartCountsAnnotationKey])
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(wantCounts, counts); diff != "" {
		t.Fatalf("restart counts mismatch (-want,+got):\n%s", diff)
	}
	// Decode independently of the production parser to verify persisted UIDs.
	charged := map[string]types.UID{}
	if raw := lws.Annotations[leaderworkerset.GroupRestartChargedLeadersAnnotationKey]; raw != "" {
		if err := json.Unmarshal([]byte(raw), &charged); err != nil {
			t.Fatal(err)
		}
	}
	if diff := cmp.Diff(wantCharged, charged); diff != "" {
		t.Fatalf("charged leaders mismatch (-want,+got):\n%s", diff)
	}
}

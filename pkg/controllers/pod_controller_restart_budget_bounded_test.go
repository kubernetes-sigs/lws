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
	"fmt"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	leaderworkerset "sigs.k8s.io/lws/api/leaderworkerset/v1"
	"sigs.k8s.io/lws/test/wrappers"
)

func TestRestartBudgetWaitsWithoutEvictingActiveCharges(t *testing.T) {
	scheme := restartBudgetTestScheme(t)
	lws := wrappers.BuildLeaderWorkerSet("default").Replica(maxGroupRestartChargedLeaders + 1).Size(1).
		RestartPolicy(leaderworkerset.RecreateGroupOnPodRestart).MaxGroupRestarts(1).Obj()
	counts := map[string]int32{}
	charged := map[string]types.UID{}
	objects := []client.Object{lws}
	for i := range maxGroupRestartChargedLeaders + 1 {
		leader := wrappers.MakePodWithLabels(lws.Name, fmt.Sprint(i), "0", lws.Namespace, 1)
		leader.UID = types.UID(fmt.Sprintf("leader-%d", i))
		leader.Labels[leaderworkerset.RevisionKey] = "revision-a"
		leader.Finalizers = []string{"test.lws/retain"}
		leader.Status.Phase = corev1.PodRunning
		leader.Status.ContainerStatuses = []corev1.ContainerStatus{{RestartCount: 1}}
		objects = append(objects, leader)
		if i < maxGroupRestartChargedLeaders {
			counts[groupRestartCountKey(leader)] = 1
			charged[groupRestartCountKey(leader)] = leader.UID
		}
	}
	countsJSON, err := json.Marshal(counts)
	if err != nil {
		t.Fatal(err)
	}
	chargedJSON, err := json.Marshal(charged)
	if err != nil {
		t.Fatal(err)
	}
	lws.Annotations = map[string]string{
		leaderworkerset.GroupRestartCountsAnnotationKey:         string(countsJSON),
		leaderworkerset.GroupRestartChargedLeadersAnnotationKey: string(chargedJSON),
	}
	cli := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
	r := &PodReconciler{Client: cli, Record: fakeEventRecorder{}}
	leader := objects[len(objects)-1].(*corev1.Pod)
	deleted, result, err := r.handleRestartPolicy(t.Context(), *leader, *lws)
	if err != nil || deleted || result.RequeueAfter <= 0 {
		t.Fatalf("full charge map = (%t, %+v, %v), want deferred recreation", deleted, result, err)
	}
	var current leaderworkerset.LeaderWorkerSet
	if err := cli.Get(t.Context(), client.ObjectKeyFromObject(lws), &current); err != nil {
		t.Fatal(err)
	}
	assertRestartBudgetState(t, &current, counts, charged)

	// A deletion that failed earlier must still be retried at the budget
	// limit, even while other groups are waiting for accounting space.
	previous := objects[1].(*corev1.Pod)
	deleted, result, err = r.handleRestartPolicy(t.Context(), *previous, *lws)
	if err != nil || !deleted || result.RequeueAfter != 0 {
		t.Fatalf("retry while full = (%t, %+v, %v)", deleted, result, err)
	}
	var terminating corev1.Pod
	if err := cli.Get(t.Context(), client.ObjectKeyFromObject(previous), &terminating); err != nil {
		t.Fatal(err)
	}
	if terminating.DeletionTimestamp == nil || terminating.Annotations[leaderworkerset.GroupRestartBudgetExhaustedAnnotationKey] != "" {
		t.Fatalf("retry incorrectly exhausted the budget: %+v", terminating.ObjectMeta)
	}

	deleted, result, err = r.handleRestartPolicy(t.Context(), *leader, *lws)
	if err != nil || !deleted || result.RequeueAfter != 0 {
		t.Fatalf("recreation after reclaiming a completed deletion = (%t, %+v, %v)", deleted, result, err)
	}
	delete(charged, groupRestartCountKey(previous))
	charged[groupRestartCountKey(leader)] = leader.UID
	counts[groupRestartCountKey(leader)] = 1
	if err := cli.Get(t.Context(), client.ObjectKeyFromObject(lws), &current); err != nil {
		t.Fatal(err)
	}
	assertRestartBudgetState(t, &current, counts, charged)
}

// Reclaiming a UID must not let a reconcile holding an older leader snapshot
// mistake an already charged failure for a new one or exhaust its budget.
func TestRestartBudgetRechecksLeaderAfterChargeReclamation(t *testing.T) {
	for _, state := range []string{"gone", "deleting", "replaced"} {
		for _, reconcile := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/reconcile=%t", state, reconcile), func(t *testing.T) {
				scheme := restartBudgetTestScheme(t)
				lws := wrappers.BuildLeaderWorkerSet("default").Size(1).
					RestartPolicy(leaderworkerset.RecreateGroupOnPodRestart).MaxGroupRestarts(1).Obj()
				lws.Annotations = map[string]string{
					leaderworkerset.GroupRestartCountsAnnotationKey:         `{"revision-a/0":1}`,
					leaderworkerset.GroupRestartChargedLeadersAnnotationKey: `{"revision-a/0":"failed-leader"}`,
				}
				leader := wrappers.MakePodWithLabels(lws.Name, "0", "0", lws.Namespace, 1)
				leader.UID = "failed-leader"
				leader.Labels[leaderworkerset.RevisionKey] = "revision-a"
				leader.Status.Phase = corev1.PodRunning
				leader.Status.ContainerStatuses = []corev1.ContainerStatus{{RestartCount: 1}}
				reclaimed := false
				cli := fake.NewClientBuilder().WithScheme(scheme).WithObjects(lws, leader).
					WithInterceptorFuncs(interceptor.Funcs{
						Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
							if _, ok := obj.(*leaderworkerset.LeaderWorkerSet); ok && !reclaimed {
								reclaimed = true
								var current leaderworkerset.LeaderWorkerSet
								if err := c.Get(ctx, key, &current); err != nil {
									return err
								}
								delete(current.Annotations, leaderworkerset.GroupRestartChargedLeadersAnnotationKey)
								if err := c.Update(ctx, &current); err != nil {
									return err
								}
								live := leader.DeepCopy()
								if state == "deleting" {
									live.Finalizers = []string{"test.lws/retain"}
									if err := c.Update(ctx, live); err != nil {
										return err
									}
								}
								if err := c.Delete(ctx, live); err != nil {
									return err
								}
								if state == "replaced" {
									live.UID = "replacement-leader"
									live.ResourceVersion = ""
									if err := c.Create(ctx, live); err != nil {
										return err
									}
								}
							}
							return c.Get(ctx, key, obj, opts...)
						},
					}).Build()
				r := &PodReconciler{Client: cli, Record: fakeEventRecorder{}}
				if reconcile {
					if _, _, err := r.handleRestartPolicy(t.Context(), *leader, *lws); err != nil {
						t.Fatal(err)
					}
				} else if err := r.persistGroupRestartCount(t.Context(), lws, leader, 2); err != nil {
					t.Fatal(err)
				}
				var current leaderworkerset.LeaderWorkerSet
				if err := cli.Get(t.Context(), client.ObjectKeyFromObject(lws), &current); err != nil {
					t.Fatal(err)
				}
				assertRestartBudgetState(t, &current, map[string]int32{"revision-a/0": 1}, map[string]types.UID{})
				var pods corev1.PodList
				if err := cli.List(t.Context(), &pods); err != nil {
					t.Fatal(err)
				}
				for _, pod := range pods.Items {
					if pod.Annotations[leaderworkerset.GroupRestartBudgetExhaustedAnnotationKey] != "" || (state == "replaced" && pod.DeletionTimestamp != nil) {
						t.Fatalf("stale failure affected a retained or replacement leader: %+v", pod.ObjectMeta)
					}
				}
			})
		}
	}
}

func TestRestartBudgetReclaimedHistoryAcrossControllerCaches(t *testing.T) {
	for _, state := range []string{"gone", "deleting", "replaced"} {
		for _, reconcile := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/reconcile=%t", state, reconcile), func(t *testing.T) {
				scheme := restartBudgetTestScheme(t)
				lws := wrappers.BuildLeaderWorkerSet("default").Size(1).
					RestartPolicy(leaderworkerset.RecreateGroupOnPodRestart).MaxGroupRestarts(1).Obj()
				lws.Annotations = map[string]string{leaderworkerset.GroupRestartCountsAnnotationKey: `{"revision-a/0":1}`}
				cached := wrappers.MakePodWithLabels(lws.Name, "0", "0", lws.Namespace, 1)
				cached.UID = "failed-leader"
				cached.Labels[leaderworkerset.RevisionKey] = "revision-a"
				cached.Status.Phase = corev1.PodRunning
				cached.Status.ContainerStatuses = []corev1.ContainerStatus{{RestartCount: 1}}
				objects := []client.Object{lws}
				if state != "gone" {
					live := cached.DeepCopy()
					if state == "replaced" {
						live.UID = "replacement-leader"
					} else {
						now := metav1.Now()
						live.DeletionTimestamp = &now
						live.Finalizers = []string{"test.lws/retain"}
					}
					objects = append(objects, live)
				}
				liveClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
				cachedClient := interceptor.NewClient(liveClient, interceptor.Funcs{
					Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
						if pod, ok := obj.(*corev1.Pod); ok {
							*pod = *cached.DeepCopy()
							return nil
						}
						return c.Get(ctx, key, obj, opts...)
					},
				})
				r := &PodReconciler{Client: cachedClient, APIReader: liveClient, Record: fakeEventRecorder{}}
				if reconcile {
					if _, _, err := r.handleRestartPolicy(t.Context(), *cached, *lws); err != nil {
						t.Fatal(err)
					}
				} else {
					stale := lws.DeepCopy()
					stale.ResourceVersion = "older-cache-snapshot"
					if err := r.persistGroupRestartCount(t.Context(), stale, cached, 2); err != nil {
						t.Fatal(err)
					}
				}
				var current leaderworkerset.LeaderWorkerSet
				if err := liveClient.Get(t.Context(), client.ObjectKeyFromObject(lws), &current); err != nil {
					t.Fatal(err)
				}
				assertRestartBudgetState(t, &current, map[string]int32{"revision-a/0": 1}, map[string]types.UID{})
				var pods corev1.PodList
				if err := liveClient.List(t.Context(), &pods); err != nil {
					t.Fatal(err)
				}
				for _, pod := range pods.Items {
					if pod.Annotations[leaderworkerset.GroupRestartBudgetExhaustedAnnotationKey] != "" || (state == "replaced" && pod.DeletionTimestamp != nil) {
						t.Fatalf("stale controller cache affected a live leader: %+v", pod.ObjectMeta)
					}
				}
			})
		}
	}
}

func TestRestartBudgetLiveReadsAreLimitedToReclaimedHistory(t *testing.T) {
	for _, tc := range []struct {
		name      string
		count     int32
		charged   types.UID
		wantReads int
	}{
		{name: "initial failure"},
		{name: "charged deletion retry", count: 1, charged: "failed-leader"},
		{name: "replacement with retained history", count: 1, charged: "previous-leader"},
		{name: "reclaimed history", count: 1, wantReads: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lws := wrappers.BuildLeaderWorkerSet("default").Size(1).
				RestartPolicy(leaderworkerset.RecreateGroupOnPodRestart).MaxGroupRestarts(3).Obj()
			lws.Annotations = map[string]string{leaderworkerset.GroupRestartCountsAnnotationKey: fmt.Sprintf(`{"revision-a/0":%d}`, tc.count)}
			if tc.charged != "" {
				lws.Annotations[leaderworkerset.GroupRestartChargedLeadersAnnotationKey] = fmt.Sprintf(`{"revision-a/0":%q}`, tc.charged)
			}
			leader := wrappers.MakePodWithLabels(lws.Name, "0", "0", lws.Namespace, 1)
			leader.UID = "failed-leader"
			leader.Labels[leaderworkerset.RevisionKey] = "revision-a"
			leader.Status.Phase = corev1.PodRunning
			leader.Status.ContainerStatuses = []corev1.ContainerStatus{{RestartCount: 1}}
			cli := fake.NewClientBuilder().WithScheme(restartBudgetTestScheme(t)).WithObjects(lws, leader).Build()
			reads := 0
			reader := interceptor.NewClient(cli, interceptor.Funcs{
				Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
					reads++
					return c.Get(ctx, key, obj, opts...)
				},
			})
			r := &PodReconciler{Client: cli, APIReader: reader, Record: fakeEventRecorder{}}
			deleted, _, err := r.handleRestartPolicy(t.Context(), *leader, *lws)
			if err != nil || !deleted {
				t.Fatalf("handleRestartPolicy = (%t, %v)", deleted, err)
			}
			if reads != tc.wantReads {
				t.Fatalf("live reads = %d, want %d", reads, tc.wantReads)
			}
		})
	}
}

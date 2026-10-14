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

package replicagroups

import (
	"context"
	"errors"
	"fmt"
	"math/bits"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	toolscache "k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	leaderworkersetv1 "sigs.k8s.io/lws/api/leaderworkerset/v1"
)

type fixture struct {
	lws        *leaderworkersetv1.LeaderWorkerSet
	workload   client.Object
	replicaSet *appsv1.ReplicaSet
	leaders    []*corev1.Pod
	workerSets []*appsv1.StatefulSet
	workers    [][]*corev1.Pod
}

func newFixture(identity leaderworkersetv1.GroupIdentityType, replicas, size int) *fixture {
	name := "role-a"
	f := &fixture{lws: &leaderworkersetv1.LeaderWorkerSet{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "test", UID: types.UID(name + "-lws-uid"), Generation: 7},
		Spec: leaderworkersetv1.LeaderWorkerSetSpec{
			Replicas: ptr.To(int32(replicas)), GroupIdentity: identity,
			LeaderWorkerTemplate: leaderworkersetv1.LeaderWorkerTemplate{Size: ptr.To(int32(size))},
		},
		// Deliberately wrong aggregate counts must not affect group observations.
		Status: leaderworkersetv1.LeaderWorkerSetStatus{ObservedGeneration: 7, Replicas: 99, ReadyReplicas: 99},
	}}
	metadata := func(name string, owner client.Object, kind schema.GroupVersionKind) metav1.ObjectMeta {
		return metav1.ObjectMeta{
			Name: name, Namespace: f.lws.Namespace, UID: types.UID(name + "-uid"), Generation: 3,
			Labels:          map[string]string{leaderworkersetv1.SetNameLabelKey: f.lws.Name},
			OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(owner, kind)},
		}
	}
	meta := metadata(f.lws.Name, f.lws, leaderworkersetv1.GroupVersion.WithKind("LeaderWorkerSet"))
	f.workload = &appsv1.StatefulSet{
		ObjectMeta: meta, Spec: appsv1.StatefulSetSpec{Replicas: ptr.To(int32(replicas))},
		Status: appsv1.StatefulSetStatus{ObservedGeneration: 3, Replicas: int32(replicas)},
	}
	owner, kind := f.workload, appsv1.SchemeGroupVersion.WithKind("StatefulSet")
	if identity == leaderworkersetv1.GroupIdentityHash {
		f.workload = &appsv1.Deployment{
			ObjectMeta: meta, Spec: appsv1.DeploymentSpec{Replicas: ptr.To(int32(replicas))},
			Status: appsv1.DeploymentStatus{ObservedGeneration: 3, Replicas: int32(replicas)},
		}
		f.replicaSet = &appsv1.ReplicaSet{
			ObjectMeta: metadata(f.lws.Name+"-rs", f.workload, appsv1.SchemeGroupVersion.WithKind("Deployment")),
			Spec:       appsv1.ReplicaSetSpec{Replicas: ptr.To(int32(replicas))},
			Status:     appsv1.ReplicaSetStatus{ObservedGeneration: 3, Replicas: int32(replicas)},
		}
		owner, kind = f.replicaSet, appsv1.SchemeGroupVersion.WithKind("ReplicaSet")
	}
	for i := 0; i < replicas; i++ {
		leaderName := fmt.Sprintf("%s-%d", f.lws.Name, i)
		groupIndex := strconv.Itoa(i)
		hostname := ""
		if identity == leaderworkersetv1.GroupIdentityHash {
			leaderName = fmt.Sprintf("%s-rs-pod-%d", f.lws.Name, i)
			groupIndex = fmt.Sprintf("hash%d", i)
			hostname = f.lws.Name + "-" + groupIndex
		}
		leader := &corev1.Pod{
			ObjectMeta: metadata(leaderName, owner, kind), Spec: corev1.PodSpec{Hostname: hostname},
			Status: readyPodStatus(),
		}
		leader.Labels[leaderworkersetv1.WorkerIndexLabelKey] = "0"
		leader.Labels[leaderworkersetv1.GroupIndexLabelKey] = groupIndex
		leader.Annotations = map[string]string{leaderworkersetv1.SizeAnnotationKey: strconv.Itoa(size)}
		f.leaders = append(f.leaders, leader)
		if size == 1 {
			continue
		}
		workerName := leaderName
		if hostname != "" {
			workerName = hostname
		}
		workers := &appsv1.StatefulSet{
			ObjectMeta: metadata(workerName, leader, corev1.SchemeGroupVersion.WithKind("Pod")),
			Spec: appsv1.StatefulSetSpec{
				Replicas: ptr.To(int32(size - 1)), Ordinals: &appsv1.StatefulSetOrdinals{Start: 1},
			},
			Status: appsv1.StatefulSetStatus{
				ObservedGeneration: 3, AvailableReplicas: int32(size - 1), CurrentRevision: "a", UpdateRevision: "a",
			},
		}
		// In Ordinal mode the worker set shares the leader's name, not its UID.
		workers.UID = types.UID(workerName + "-workers-uid")
		f.workerSets = append(f.workerSets, workers)
		var members []*corev1.Pod
		for j := 1; j < size; j++ {
			worker := &corev1.Pod{
				ObjectMeta: metadata(fmt.Sprintf("%s-%d", workerName, j), workers, appsv1.SchemeGroupVersion.WithKind("StatefulSet")),
				Status:     readyPodStatus(),
			}
			worker.Labels[leaderworkersetv1.WorkerIndexLabelKey] = strconv.Itoa(j)
			worker.Labels[leaderworkersetv1.GroupIndexLabelKey] = groupIndex
			members = append(members, worker)
		}
		f.workers = append(f.workers, members)
	}
	return f
}

func TestScalingObservation(t *testing.T) {
	// StatefulSet status counts all observed Pods; ReplicaSet status excludes
	// terminating and terminal Pods. Keep lagging-status cases explicit below.
	oneActiveLeader := func(f *fixture) {
		if f.replicaSet != nil {
			f.replicaSet.Status.Replicas = 1
		} else {
			f.workload.(*appsv1.StatefulSet).Status.Replicas = int32(len(f.leaders))
		}
	}
	for _, identity := range []leaderworkersetv1.GroupIdentityType{leaderworkersetv1.GroupIdentityOrdinal, leaderworkersetv1.GroupIdentityHash} {
		for _, tc := range []struct {
			name               string
			change             func(*fixture)
			scaling, deletions bool
		}{
			{"steady", func(*fixture) {}, true, true},
			{"Pending leaders, no workers under LeaderReady", func(f *fixture) {
				f.lws.Spec.StartupPolicy = leaderworkersetv1.LeaderReadyStartupPolicy
				f.workerSets, f.workers = nil, nil
				for _, pod := range f.leaders {
					pod.Status = corev1.PodStatus{Phase: corev1.PodPending}
				}
			}, true, true},
			{"loading leaders", func(f *fixture) {
				for _, pod := range f.leaders {
					pod.Status.Conditions = nil
				}
			}, true, true},
			{"LWS not acknowledged", func(f *fixture) { f.lws.Generation++ }, false, false},
			{"native controller not acknowledged", func(f *fixture) { f.workload.SetGeneration(4) }, false, false},
			{"native scale not applied", func(f *fixture) { f.lws.Spec.Replicas = ptr.To[int32](1) }, false, false},
			{"missing new leader", func(f *fixture) { f.leaders = f.leaders[:1] }, false, false},
			{"deletion not yet observed", func(f *fixture) { f.scale(1) }, false, false},
			{"deletion observed but not finished", func(f *fixture) { f.scale(1); holdTermination(f.leaders[1]) }, true, true},
			{"deleted group still needed", func(f *fixture) { holdTermination(f.leaders[1]); oneActiveLeader(f) }, false, true},
			{"terminal leader", func(f *fixture) { f.leaders[1].Status.Phase = corev1.PodFailed; oneActiveLeader(f) }, false, true},
			{"missing leader acknowledged", func(f *fixture) { f.leaders = f.leaders[:1]; oneActiveLeader(f) }, false, true},
			{"status still counts terminating leader", func(f *fixture) { holdTermination(f.leaders[1]) }, false, identity == leaderworkersetv1.GroupIdentityOrdinal},
			{"status still counts terminal leader", func(f *fixture) { f.leaders[1].Status.Phase = corev1.PodFailed }, false, identity == leaderworkersetv1.GroupIdentityOrdinal},
			{"confirmed shortfall", func(f *fixture) { f.scale(3) }, false, true},
			{"Pods lag native status", func(f *fixture) { f.scale(3); f.leaders = f.leaders[:1] }, false, false},
		} {
			t.Run(fmt.Sprintf("%s/%s", identity, tc.name), func(t *testing.T) {
				f := newFixture(identity, 2, 2)
				tc.change(f)
				snapshot, err := Observe(t.Context(), newReader(t, f.objects()...), f.lws)
				require.NoError(t, err)
				require.Equal(t, tc.scaling, snapshot.ScalingObserved())
				require.Equal(t, tc.deletions, snapshot.DeletionsObserved())
			})
		}
	}
	t.Run("one Hash ReplicaSet cannot hide another's pending deletion", func(t *testing.T) {
		f := newFixture(leaderworkersetv1.GroupIdentityHash, 3, 1)
		f.leaders = f.leaders[:2]
		f.replicaSet.Spec.Replicas, f.replicaSet.Status.Replicas = ptr.To[int32](1), 2
		missing := f.replicaSet.DeepCopy()
		missing.Name, missing.UID, missing.Spec.Replicas, missing.Status.Replicas = "missing", "missing", ptr.To[int32](2), 0
		snapshot, err := Observe(t.Context(), newReader(t, append(f.objects(), missing)...), f.lws)
		require.NoError(t, err)
		require.False(t, snapshot.DeletionsObserved(), "total active < desired is insufficient")
	})
}

func TestObserveNeverPaginates(t *testing.T) {
	f := newFixture(leaderworkersetv1.GroupIdentityOrdinal, 501, 1)
	base := newReader(t, f.objects()...)
	reader := interceptor.NewClient(base, interceptor.Funcs{
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			options := (&client.ListOptions{}).ApplyOptions(opts)
			require.Zero(t, options.Limit, "a cache limit silently truncates without API continuation")
			require.Empty(t, options.Continue)
			return c.List(ctx, list, opts...)
		},
	})
	snapshot, err := Observe(t.Context(), reader, f.lws)
	require.NoError(t, err)
	require.Len(t, snapshot.Groups, 501)
	require.True(t, snapshot.ScalingObserved())
}

func readyPodStatus() corev1.PodStatus {
	return corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}}
}

func (f *fixture) objects() []client.Object {
	objects := []client.Object{f.lws, f.workload}
	if f.replicaSet != nil {
		objects = append(objects, f.replicaSet)
	}
	for _, leader := range f.leaders {
		objects = append(objects, leader)
	}
	for i, workers := range f.workerSets {
		objects = append(objects, workers)
		for _, pod := range f.workers[i] {
			objects = append(objects, pod)
		}
	}
	return objects
}

func newReader(t *testing.T, objects ...client.Object) client.WithWatch {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, appsv1.AddToScheme(scheme))
	require.NoError(t, leaderworkersetv1.AddToScheme(scheme))
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).
		WithStatusSubresource(&leaderworkersetv1.LeaderWorkerSet{}).Build()
}

func holdTermination(object client.Object) {
	object.SetDeletionTimestamp(ptr.To(metav1.NewTime(time.Unix(1000, 0))))
	object.SetFinalizers([]string{"test/hold-termination"})
}

func TestObserveGroups(t *testing.T) {
	for _, identity := range []leaderworkersetv1.GroupIdentityType{"", leaderworkersetv1.GroupIdentityOrdinal, leaderworkersetv1.GroupIdentityHash} {
		for _, size := range []int{1, 3} {
			t.Run(fmt.Sprintf("%s/size%d", identity, size), func(t *testing.T) {
				f := newFixture(identity, 4, size)
				// Desired replicas are not a filter: both future consumers need
				// individual excess groups, even before deletion is issued.
				f.lws.Spec.Replicas = ptr.To[int32](2)
				f.leaders[1].Status.Conditions[0].Status = corev1.ConditionFalse
				holdTermination(f.leaders[2])
				objects := f.objects()
				slices.Reverse(objects)
				observed, err := Observe(t.Context(), newReader(t, objects...), f.lws)
				require.NoError(t, err)
				require.NotNil(t, observed)
				require.Len(t, observed.Groups, 4)
				for i, group := range observed.Groups {
					assert.Equal(t, f.leaders[i].UID, group.Leader.UID)
					assert.Equal(t, i != 1, group.Ready)
					assert.Equal(t, i == 2, group.Terminating)
					require.Len(t, group.Pods, size)
					assert.Same(t, group.Leader, group.Pods[0])
					if size > 1 {
						require.NotNil(t, group.WorkerStatefulSet)
						assert.Equal(t, f.workerSets[i].Name, group.WorkerStatefulSet.Name)
						for j, worker := range group.Pods[1:] {
							assert.Equal(t, f.workers[i][j].UID, worker.UID)
						}
					}
					if identity == leaderworkersetv1.GroupIdentityHash {
						assert.Equal(t, -1, group.Ordinal)
					} else {
						assert.Equal(t, i, group.Ordinal)
					}
				}
				if identity == leaderworkersetv1.GroupIdentityHash {
					assert.Nil(t, observed.LeaderStatefulSet)
					assert.NotNil(t, observed.LeaderDeployment)
					assert.Len(t, observed.ReplicaSets, 1)
				} else {
					assert.NotNil(t, observed.LeaderStatefulSet)
					assert.Nil(t, observed.LeaderDeployment)
					assert.Empty(t, observed.ReplicaSets)
				}
			})
		}
	}
}

func TestObserveWholeGroupReadinessAndTermination(t *testing.T) {
	tests := []struct {
		name        string
		change      func(*fixture)
		ready       bool
		terminating bool
		unknown     bool
	}{
		{name: "ready", change: func(*fixture) {}, ready: true},
		{name: "leader unready", change: func(f *fixture) { f.leaders[0].Status.Conditions = nil }},
		{name: "leader completed", change: func(f *fixture) { f.leaders[0].Status.Phase = corev1.PodSucceeded }},
		{name: "worker failed", change: func(f *fixture) { f.workers[0][0].Status.Phase = corev1.PodFailed }},
		{name: "old worker unready after template shrinks to one", change: func(f *fixture) {
			f.lws.Spec.LeaderWorkerTemplate.Size = ptr.To[int32](1)
			f.workers[0][0].Status.Conditions[0].Status = corev1.ConditionFalse
		}},
		{name: "healthy old group after template grows", change: func(f *fixture) { f.lws.Spec.LeaderWorkerTemplate.Size = ptr.To[int32](4) }, ready: true},
		{name: "healthy old group after template shrinks", change: func(f *fixture) { f.lws.Spec.LeaderWorkerTemplate.Size = ptr.To[int32](2) }, ready: true},
		{name: "healthy old leader-only group after template grows", change: func(f *fixture) {
			f.leaders[0].Annotations[leaderworkersetv1.SizeAnnotationKey] = "1"
			f.workerSets, f.workers = nil, nil
		}, ready: true},
		{name: "size annotation absent", change: func(f *fixture) { delete(f.leaders[0].Annotations, leaderworkersetv1.SizeAnnotationKey) }, unknown: true},
		{name: "size annotation malformed", change: func(f *fixture) { f.leaders[0].Annotations[leaderworkersetv1.SizeAnnotationKey] = "bad" }, unknown: true},
		{name: "size annotation zero", change: func(f *fixture) { f.leaders[0].Annotations[leaderworkersetv1.SizeAnnotationKey] = "0" }, unknown: true},
		{name: "size annotation negative", change: func(f *fixture) { f.leaders[0].Annotations[leaderworkersetv1.SizeAnnotationKey] = "-1" }, unknown: true},
		{name: "worker missing", change: func(f *fixture) { f.workers[0] = f.workers[0][1:] }},
		{name: "worker set missing", change: func(f *fixture) { f.workerSets, f.workers = nil, nil }},
		{name: "workers unavailable", change: func(f *fixture) { f.workerSets[0].Status.AvailableReplicas-- }},
		{name: "worker revisions differ", change: func(f *fixture) { f.workerSets[0].Status.CurrentRevision = "old" }},
		{name: "wrong worker target", change: func(f *fixture) { f.workerSets[0].Spec.Replicas = ptr.To[int32](1) }},
		{name: "nil worker target", change: func(f *fixture) { f.workerSets[0].Spec.Replicas = nil }},
		{name: "leader terminating", change: func(f *fixture) { holdTermination(f.leaders[0]) }, ready: true, terminating: true},
		{name: "worker terminating", change: func(f *fixture) { holdTermination(f.workers[0][0]) }, ready: true, terminating: true},
		{name: "worker set terminating", change: func(f *fixture) { holdTermination(f.workerSets[0]) }, ready: true, terminating: true},
		// Neither policy annotations nor generation fences hide the observation.
		{name: "consumer policy annotation", change: func(f *fixture) {
			f.leaders[0].Annotations["test.example.com/availability-credit"] = "withheld"
		}, ready: true},
		{name: "worker generation pending", change: func(f *fixture) { f.workerSets[0].Status.ObservedGeneration-- }, ready: true},
	}
	for _, identity := range []leaderworkersetv1.GroupIdentityType{leaderworkersetv1.GroupIdentityOrdinal, leaderworkersetv1.GroupIdentityHash} {
		for _, tc := range tests {
			t.Run(string(identity)+"/"+tc.name, func(t *testing.T) {
				f := newFixture(identity, 1, 3)
				tc.change(f)
				observed, err := Observe(t.Context(), newReader(t, f.objects()...), f.lws)
				if tc.unknown {
					require.ErrorContains(t, err, "invalid group size")
					assert.Nil(t, observed, "unknown must not become observed zero readiness")
					return
				}
				require.NoError(t, err)
				require.Len(t, observed.Groups, 1)
				assert.Equal(t, tc.ready, observed.Groups[0].Ready)
				assert.Equal(t, tc.terminating, observed.Groups[0].Terminating)
			})
		}
	}
}

func TestAvailabilityUsesEachGroupsRevisionSize(t *testing.T) {
	for _, identity := range []leaderworkersetv1.GroupIdentityType{leaderworkersetv1.GroupIdentityOrdinal, leaderworkersetv1.GroupIdentityHash} {
		t.Run(string(identity), func(t *testing.T) {
			f := newFixture(identity, 2, 3)
			// A partition (Ordinal) or overlapping ReplicaSets (Hash) keeps an
			// old size-three group beside a new leader-only group.
			f.lws.Spec.LeaderWorkerTemplate.Size = ptr.To[int32](1)
			f.leaders[1].Annotations[leaderworkersetv1.SizeAnnotationKey] = "1"
			f.workerSets, f.workers = f.workerSets[:1], f.workers[:1]
			objects := f.objects()
			if f.replicaSet != nil {
				f.replicaSet.Spec.Replicas = ptr.To[int32](1)
				newRS := f.replicaSet.DeepCopy()
				newRS.Name, newRS.UID = "new-rs", "new-rs"
				f.leaders[1].OwnerReferences[0] = *metav1.NewControllerRef(newRS, appsv1.SchemeGroupVersion.WithKind("ReplicaSet"))
				objects = append(objects, newRS)
			}
			for _, oldReady := range []bool{true, false} {
				want := int32(2)
				if !oldReady {
					// Keep the asynchronous worker-set status Ready: the actual
					// required worker must still prevent unsafe credit.
					f.workers[0][0].Status.Conditions[0].Status = corev1.ConditionFalse
					want = 1
				}
				observed, err := Observe(t.Context(), newReader(t, objects...), f.lws)
				require.NoError(t, err)
				assert.Equal(t, Availability{ReadyReplicas: want, RetainedReadyReplicas: want}, observed.Availability())
			}
		})
	}
}

func TestObserveRejectsStaleOwnershipAtEveryLink(t *testing.T) {
	tests := []struct {
		name       string
		change     func(*fixture)
		wantGroups int
		wantPods   int
	}{
		{name: "workload belongs to old LWS", change: func(f *fixture) { f.lws.UID = "replacement-lws" }},
		{name: "leaders belong to old workload", change: func(f *fixture) { f.workload.SetUID("replacement-workload") }},
		{name: "workers belong to old leader", change: func(f *fixture) { f.leaders[0].UID = "replacement-leader" }, wantGroups: 1, wantPods: 1},
		{name: "Pods belong to old workers", change: func(f *fixture) { f.workerSets[0].UID = "replacement-workers" }, wantGroups: 1, wantPods: 1},
		{name: "worker has foreign owner", change: func(f *fixture) { f.workers[0][0].OwnerReferences[0].UID = "foreign" }, wantGroups: 1, wantPods: 2},
		{name: "leader has no controller", change: func(f *fixture) { f.leaders[0].OwnerReferences[0].Controller = ptr.To(false) }},
	}
	for _, identity := range []leaderworkersetv1.GroupIdentityType{leaderworkersetv1.GroupIdentityOrdinal, leaderworkersetv1.GroupIdentityHash} {
		for _, tc := range tests {
			t.Run(string(identity)+"/"+tc.name, func(t *testing.T) {
				f := newFixture(identity, 1, 3)
				tc.change(f)
				observed, err := Observe(t.Context(), newReader(t, f.objects()...), f.lws)
				require.NoError(t, err)
				require.Len(t, observed.Groups, tc.wantGroups)
				assert.Zero(t, observed.Availability())
				if tc.wantGroups > 0 {
					assert.False(t, observed.Groups[0].Ready)
					assert.Len(t, observed.Groups[0].Pods, tc.wantPods)
				}
			})
		}
	}
	t.Run("Hash leaders belong to old ReplicaSet", func(t *testing.T) {
		f := newFixture(leaderworkersetv1.GroupIdentityHash, 1, 3)
		f.replicaSet.UID = "replacement-rs"
		observed, err := Observe(t.Context(), newReader(t, f.objects()...), f.lws)
		require.NoError(t, err)
		assert.Empty(t, observed.Groups)
	})
}

func TestObserveAbsentOrReplacedObjects(t *testing.T) {
	for _, absent := range []string{"LWS", "replacement LWS", "workload", "leader"} {
		t.Run(absent, func(t *testing.T) {
			f := newFixture(leaderworkersetv1.GroupIdentityOrdinal, 1, 3)
			expected := f.lws.DeepCopy()
			objects := f.objects()
			switch absent {
			case "LWS":
				objects = objects[1:]
			case "replacement LWS":
				f.lws.UID = "replacement"
			case "workload":
				objects = slices.DeleteFunc(objects, func(object client.Object) bool { return object == f.workload })
			case "leader":
				objects = slices.DeleteFunc(objects, func(object client.Object) bool { return object == f.leaders[0] })
			}
			observed, err := Observe(t.Context(), newReader(t, objects...), expected)
			require.NoError(t, err)
			if absent == "LWS" || absent == "replacement LWS" {
				assert.Nil(t, observed)
			} else {
				require.NotNil(t, observed)
				assert.Equal(t, f.lws.UID, observed.LWS.UID)
				assert.Empty(t, observed.Groups)
				assert.Zero(t, observed.Availability())
			}
		})
	}
	for _, expected := range []*leaderworkersetv1.LeaderWorkerSet{nil, {}} {
		observed, err := Observe(t.Context(), nil, expected)
		assert.Error(t, err)
		assert.Nil(t, observed)
	}
}

func TestObserveScopesListsAndPreservesMultipleReplicaSets(t *testing.T) {
	f := newFixture(leaderworkersetv1.GroupIdentityHash, 1, 3)
	old := f.replicaSet.DeepCopy()
	old.Name, old.UID, old.Spec.Replicas = "a-old-rs", "old-rs", ptr.To[int32](0)
	holdTermination(old)
	oldLeader := f.leaders[0].DeepCopy()
	oldLeader.Name, oldLeader.UID = "old-leader", "old-leader"
	oldLeader.OwnerReferences[0] = *metav1.NewControllerRef(old, appsv1.SchemeGroupVersion.WithKind("ReplicaSet"))
	holdTermination(oldLeader)
	foreign := old.DeepCopy()
	foreign.Name, foreign.UID, foreign.OwnerReferences[0].UID = "foreign-rs", "foreign-rs", "foreign-deployment"
	foreignLeader := oldLeader.DeepCopy()
	foreignLeader.Name, foreignLeader.UID, foreignLeader.OwnerReferences[0].UID = "foreign-leader", "foreign-leader", foreign.UID
	otherNamespace := f.leaders[0].DeepCopy()
	otherNamespace.Namespace, otherNamespace.UID = "other", "other-namespace"
	otherSet := f.leaders[0].DeepCopy()
	otherSet.Name, otherSet.UID = "other-set", "other-set"
	otherSet.Labels[leaderworkersetv1.SetNameLabelKey] = "other"
	objects := append(f.objects(), old, oldLeader, foreign, foreignLeader, otherNamespace, otherSet)
	observed, err := Observe(t.Context(), newReader(t, objects...), f.lws)
	require.NoError(t, err)
	require.Len(t, observed.ReplicaSets, 2)
	assert.Equal(t, old.UID, observed.ReplicaSets[0].UID)
	assert.Equal(t, int32(0), *observed.ReplicaSets[0].Spec.Replicas)
	assert.False(t, observed.ReplicaSets[0].DeletionTimestamp.IsZero())
	require.Len(t, observed.Groups, 2)
	assert.Equal(t, oldLeader.UID, observed.Groups[0].Leader.UID)
	assert.True(t, observed.Groups[0].Terminating)
	assert.False(t, observed.Groups[0].Ready)
	assert.Equal(t, f.leaders[0].UID, observed.Groups[1].Leader.UID)
	assert.Equal(t, Availability{ReadyReplicas: 1, RetainedReadyReplicas: 1}, observed.Availability())
}

func TestObserveOrdinalsAndResidualWorkers(t *testing.T) {
	f := newFixture(leaderworkersetv1.GroupIdentityOrdinal, 12, 12)
	// Ordinals come from the native StatefulSet names, not mutable labels.
	f.leaders[0].Labels[leaderworkersetv1.GroupIndexLabelKey] = "not-an-ordinal"
	observed, err := Observe(t.Context(), newReader(t, f.objects()...), f.lws)
	require.NoError(t, err)
	for i, group := range observed.Groups {
		assert.Equal(t, i, group.Ordinal)
		for j, worker := range group.Pods[1:] {
			assert.Equal(t, f.workers[i][j].Name, worker.Name)
		}
	}
	// A nonzero native start excludes Ready groups below its retained range.
	observed.LeaderStatefulSet.Spec.Ordinals = &appsv1.StatefulSetOrdinals{Start: 2}
	assert.Equal(t, Availability{ReadyReplicas: 12, RetainedReadyReplicas: 10}, observed.Availability())

	f = newFixture(leaderworkersetv1.GroupIdentityOrdinal, 1, 3)
	f.lws.Spec.LeaderWorkerTemplate.Size = ptr.To[int32](1)
	holdTermination(f.workers[0][1])
	observed, err = Observe(t.Context(), newReader(t, f.objects()...), f.lws)
	require.NoError(t, err)
	require.Len(t, observed.Groups, 1)
	assert.Len(t, observed.Groups[0].Pods, 3)
	assert.True(t, observed.Groups[0].Ready)
	assert.True(t, observed.Groups[0].Terminating)
}

func TestObserveReadsCachedWorkloadsBeforeOnePodList(t *testing.T) {
	for _, identity := range []leaderworkersetv1.GroupIdentityType{leaderworkersetv1.GroupIdentityOrdinal, leaderworkersetv1.GroupIdentityHash} {
		t.Run(string(identity), func(t *testing.T) {
			f := newFixture(identity, 1, 3)
			expected := f.lws.DeepCopy()
			f.lws.Spec.Replicas, f.lws.Generation = ptr.To[int32](3), 9
			f.workload.SetGeneration(10)
			f.workerSets[0].Generation = 11
			if f.replicaSet != nil {
				f.replicaSet.Generation = 12
			}
			holdTermination(f.workers[0][0])
			var calls []string
			trace := interceptor.Funcs{
				Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
					calls = append(calls, fmt.Sprintf("get %T", obj))
					return c.Get(ctx, key, obj, opts...)
				},
				List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
					calls = append(calls, fmt.Sprintf("list %T", list))
					return c.List(ctx, list, opts...)
				},
			}
			reader := interceptor.NewClient(newReader(t, f.objects()...), trace)
			observed, err := Observe(t.Context(), reader, expected)
			require.NoError(t, err)
			assert.Equal(t, int32(1), *expected.Spec.Replicas, "input must remain untouched")
			assert.Equal(t, int32(3), *observed.LWS.Spec.Replicas)
			assert.Equal(t, int64(9), observed.LWS.Generation)
			assert.Equal(t, int64(7), observed.LWS.Status.ObservedGeneration)
			assert.True(t, observed.Groups[0].Terminating)
			assert.Equal(t, int64(11), observed.Groups[0].WorkerStatefulSet.Generation)
			assert.Equal(t, int64(3), observed.Groups[0].WorkerStatefulSet.Status.ObservedGeneration)
			want := []string{"get *v1.LeaderWorkerSet", "get *v1.StatefulSet", "list *v1.StatefulSetList", "list *v1.PodList"}
			if identity == leaderworkersetv1.GroupIdentityHash {
				want = []string{"get *v1.LeaderWorkerSet", "get *v1.Deployment", "list *v1.ReplicaSetList", "list *v1.StatefulSetList", "list *v1.PodList"}
				assert.Equal(t, int64(10), observed.LeaderDeployment.Generation)
				assert.Equal(t, int64(3), observed.LeaderDeployment.Status.ObservedGeneration)
				assert.Equal(t, int64(12), observed.ReplicaSets[0].Generation)
			} else {
				assert.Equal(t, int64(10), observed.LeaderStatefulSet.Generation)
				assert.Equal(t, int64(3), observed.LeaderStatefulSet.Status.ObservedGeneration)
			}
			assert.Equal(t, want, calls)
			// Fallback lookup must also preserve positive availability for settled groups.
			f = newFixture(identity, 1, 3)
			expected = f.lws.DeepCopy()
			reader = interceptor.NewClient(newReader(t, f.objects()...), trace)
			for _, label := range []string{f.lws.Name, "", "wrong"} {
				f.workload.SetLabels(map[string]string{leaderworkersetv1.SetNameLabelKey: label})
				require.NoError(t, reader.Update(t.Context(), f.workload))
				single, err := Observe(t.Context(), reader, expected)
				require.NoError(t, err)
				assert.EqualValues(t, 1, single.Availability().RetainedReadyReplicas)

			}
		})
	}
}

func TestObserveReadErrorsReturnNoPartialSnapshot(t *testing.T) {
	for _, operation := range []string{"lws", "workload", "replicasets", "workers", "pods"} {
		t.Run(operation, func(t *testing.T) {
			f := newFixture(leaderworkersetv1.GroupIdentityHash, 1, 3)
			f.workload.SetLabels(nil) // Exercise the named fallback, including failures of its GET.
			failure := errors.New("cache read unavailable")
			reader := interceptor.NewClient(newReader(t, f.objects()...), interceptor.Funcs{
				Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
					_, isLWS := obj.(*leaderworkersetv1.LeaderWorkerSet)
					if (operation == "lws" && isLWS) || (operation == "workload" && !isLWS) {
						return failure
					}
					return c.Get(ctx, key, obj, opts...)
				},
				List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
					var fail bool
					switch list.(type) {
					case *appsv1.ReplicaSetList:
						fail = operation == "replicasets"
					case *appsv1.StatefulSetList:
						fail = operation == "workers"
					case *corev1.PodList:
						fail = operation == "pods"
					}
					if fail {
						return failure
					}
					return c.List(ctx, list, opts...)
				},
			})
			observed, err := Observe(t.Context(), reader, f.lws)
			assert.ErrorIs(t, err, failure)
			assert.Nil(t, observed)
		})
	}
	t.Run("list NotFound is not an empty successful snapshot", func(t *testing.T) {
		f := newFixture(leaderworkersetv1.GroupIdentityOrdinal, 1, 1)
		reader := interceptor.NewClient(newReader(t, f.objects()...), interceptor.Funcs{
			List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
				return apierrors.NewNotFound(schema.GroupResource{Resource: "statefulsets"}, "")
			},
		})
		observed, err := Observe(t.Context(), reader, f.lws)
		assert.True(t, apierrors.IsNotFound(err))
		assert.Nil(t, observed)
	})
}

func (f *fixture) scale(replicas int) {
	f.lws.Spec.Replicas = ptr.To(int32(replicas))
	switch workload := f.workload.(type) {
	case *appsv1.StatefulSet:
		workload.Spec.Replicas = ptr.To(int32(replicas))
	case *appsv1.Deployment:
		workload.Spec.Replicas = ptr.To(int32(replicas))
		f.replicaSet.Spec.Replicas = ptr.To(int32(replicas))
	}
}

func TestAvailability(t *testing.T) {
	tests := []struct {
		name                  string
		spec, actual, ready   int
		change                func(*fixture)
		wantOrdinal, wantHash int32
	}{
		{name: "child target still old", spec: 3, actual: 2, ready: 2, change: func(f *fixture) {
			f.scale(2)
			f.lws.Spec.Replicas = ptr.To[int32](3)
		}},
		{name: "older delete call can still run", spec: 3, actual: 4, ready: 4, change: func(f *fixture) {
			f.workload.SetGeneration(f.workload.GetGeneration() + 1)
		}},
		{name: "unacknowledged down-up ABA", spec: 3, actual: 3, ready: 3, change: func(f *fixture) {
			f.lws.Generation += 2
		}},
		{name: "Deployment ack is not ReplicaSet ack", spec: 2, actual: 2, ready: 2, wantOrdinal: 2, change: func(f *fixture) {
			if f.replicaSet != nil {
				f.replicaSet.Generation++
			}
		}},
		{name: "worker ack pending", spec: 2, actual: 2, ready: 2, wantOrdinal: 1, wantHash: 1, change: func(f *fixture) { f.workerSets[0].Generation++ }},
		{name: "worker terminating", spec: 2, actual: 2, ready: 2, wantOrdinal: 1, wantHash: 1, change: func(f *fixture) { holdTermination(f.workers[0][0]) }},
		{name: "worker set terminating", spec: 2, actual: 2, ready: 2, wantOrdinal: 1, wantHash: 1, change: func(f *fixture) { holdTermination(f.workerSets[0]) }},
		{name: "restart budget exhausted", spec: 2, actual: 2, ready: 2, wantOrdinal: 1, wantHash: 1, change: func(f *fixture) {
			f.leaders[0].Annotations[leaderworkersetv1.GroupRestartBudgetExhaustedAnnotationKey] = "true"
		}},
		{name: "deleting LWS", spec: 2, actual: 2, ready: 2, change: func(f *fixture) { holdTermination(f.lws) }},
		{name: "deleting native workload", spec: 2, actual: 2, ready: 2, change: func(f *fixture) { holdTermination(f.workload) }},
		{name: "completed leader is not an unissued victim", spec: 2, actual: 3, ready: 2, wantOrdinal: 2, wantHash: 2, change: func(f *fixture) { f.leaders[2].Status.Phase = corev1.PodSucceeded }},
		{name: "failed leader is not an unissued victim", spec: 2, actual: 3, ready: 2, wantOrdinal: 2, wantHash: 2, change: func(f *fixture) { f.leaders[2].Status.Phase = corev1.PodFailed }},
	}
	for _, identity := range []leaderworkersetv1.GroupIdentityType{"", leaderworkersetv1.GroupIdentityOrdinal, leaderworkersetv1.GroupIdentityHash} {
		for _, tc := range tests {
			t.Run(string(identity)+"/"+tc.name, func(t *testing.T) {
				f := newFixture(identity, tc.actual, 2)
				f.scale(tc.spec)
				for _, leader := range f.leaders[tc.ready:] {
					leader.Status.Conditions[0].Status = corev1.ConditionFalse
				}
				if tc.change != nil {
					tc.change(f)
				}
				observed, err := Observe(t.Context(), newReader(t, f.objects()...), f.lws)
				require.NoError(t, err)
				retained := tc.wantOrdinal
				if identity == leaderworkersetv1.GroupIdentityHash {
					retained = tc.wantHash
				}
				assert.Equal(t, Availability{
					ReadyReplicas: int32(tc.ready), RetainedReadyReplicas: retained,
				}, observed.Availability())
			})
		}
	}
}

func TestHashAvailabilityReservesVictimsPerReplicaSet(t *testing.T) {
	f := newFixture(leaderworkersetv1.GroupIdentityHash, 3, 1)
	f.replicaSet.Spec.Replicas = ptr.To[int32](1)
	second := f.replicaSet.DeepCopy()
	second.Name, second.UID, second.Spec.Replicas = "other-rs", "other-rs", ptr.To[int32](2)
	objects := append(f.objects(), second)
	observed, err := Observe(t.Context(), newReader(t, objects...), f.lws)
	require.NoError(t, err)
	// All 3 Ready groups belong to the RS targeting 1. The other RS's unused
	// target of 2 must not erase the first RS's two pending deletions.
	assert.EqualValues(t, 1, observed.Availability().RetainedReadyReplicas)
	assert.False(t, observed.ScalingObserved(), "equal total counts cannot hide another ReplicaSet's pending deletions")

	f.leaders[0].OwnerReferences[0] = *metav1.NewControllerRef(second, appsv1.SchemeGroupVersion.WithKind("ReplicaSet"))
	for _, leader := range f.leaders[1:] {
		leader.Status.Conditions[0].Status = corev1.ConditionFalse
	}
	observed, err = Observe(t.Context(), newReader(t, objects...), f.lws)
	require.NoError(t, err)
	// Conversely, the first RS's excess unready groups must not withhold the
	// second RS's one Ready group, which is not exposed to its deletions.
	assert.EqualValues(t, 1, observed.Availability().RetainedReadyReplicas)

	second.Generation++
	observed, err = Observe(t.Context(), newReader(t, objects...), f.lws)
	require.NoError(t, err)
	assert.Zero(t, observed.Availability().RetainedReadyReplicas)
	assert.False(t, observed.ScalingObserved(), "every ReplicaSet must acknowledge its target")

	second.Status.ObservedGeneration++
	second.Spec.Replicas = ptr.To[int32](1)
	observed, err = Observe(t.Context(), newReader(t, objects...), f.lws)
	require.NoError(t, err)
	assert.Zero(t, observed.Availability().RetainedReadyReplicas, "unfinished target redistribution")
}

func TestAvailabilityExhaustiveVictimSets(t *testing.T) {
	// 3,410 observations: all readiness/termination combinations for up to
	// four groups, every target from zero to four, and both identity modes.
	// For Hash, enumerate every possible remaining victim set, independently
	// of the formula under test and without trusting deletion preferences.
	for _, identity := range []leaderworkersetv1.GroupIdentityType{leaderworkersetv1.GroupIdentityOrdinal, leaderworkersetv1.GroupIdentityHash} {
		for spec := 0; spec <= 4; spec++ {
			for actual := 0; actual <= 4; actual++ {
				for readyMask := 0; readyMask < 1<<actual; readyMask++ {
					for terminatingMask := 0; terminatingMask < 1<<actual; terminatingMask++ {
						f := newFixture(identity, actual, 1)
						f.scale(spec)
						for i, leader := range f.leaders {
							if readyMask&(1<<i) == 0 {
								leader.Status.Conditions[0].Status = corev1.ConditionFalse
							}
							if terminatingMask&(1<<i) != 0 {
								holdTermination(leader)
							}
						}
						observed, err := Observe(t.Context(), newReader(t, f.objects()...), f.lws)
						require.NoError(t, err)
						activeMask := (1<<actual - 1) &^ terminatingMask
						want := bits.OnesCount(uint(readyMask & activeMask & (1<<spec - 1)))
						if identity == leaderworkersetv1.GroupIdentityHash {
							want = actual
							deletions := max(0, bits.OnesCount(uint(activeMask))-spec)
							for victims := 0; victims < 1<<actual; victims++ {
								if victims & ^activeMask == 0 && bits.OnesCount(uint(victims)) == deletions {
									want = min(want, bits.OnesCount(uint(readyMask&activeMask&^victims)))
								}
							}
						}
						assert.Equalf(t, Availability{
							ReadyReplicas: int32(bits.OnesCount(uint(readyMask))), RetainedReadyReplicas: int32(want),
						}, observed.Availability(), "%s spec=%d actual=%d ready=%b terminating=%b", identity, spec, actual, readyMask, terminatingMask)
					}
				}
			}
		}
	}
}

func TestObserveValidatesExpectedIdentity(t *testing.T) {
	for _, expected := range []*leaderworkersetv1.LeaderWorkerSet{
		nil, {},
		{ObjectMeta: metav1.ObjectMeta{Name: "name", Namespace: "test"}},
		{ObjectMeta: metav1.ObjectMeta{UID: "uid", Namespace: "test"}},
		{ObjectMeta: metav1.ObjectMeta{Name: "name", UID: "uid"}},
	} {
		snapshot, err := Observe(t.Context(), nil, expected)
		require.Error(t, err)
		require.Nil(t, snapshot)
	}
}

type sourceTestCache struct {
	cache.Cache
	observe func() error
}

func (*sourceTestCache) Start(ctx context.Context) error {
	<-ctx.Done()
	return nil
}

func (c *sourceTestCache) GetInformer(context.Context, client.Object, ...cache.InformerGetOption) (cache.Informer, error) {
	return nil, c.observe()
}

func TestObserverSource(t *testing.T) {
	for _, sourceFirst := range []bool{false, true} {
		t.Run(fmt.Sprint("source-first=", sourceFirst), func(t *testing.T) {
			queue := workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[reconcile.Request]())
			t.Cleanup(queue.ShutDown)
			o := &Observer{}
			source := o.Source(func(lws *leaderworkersetv1.LeaderWorkerSet) (reconcile.Request, bool) {
				return reconcile.Request{NamespacedName: client.ObjectKeyFromObject(lws)}, lws.Name != "unrelated"
			})
			busy := &leaderworkersetv1.LeaderWorkerSet{ObjectMeta: metav1.ObjectMeta{Name: "busy", Namespace: "ns"}}
			idle := busy.DeepCopy()
			idle.Name = "idle"
			stopped := errors.New("observer stopped")
			o.cache = &sourceTestCache{observe: func() error {
				for range 2048 {
					o.notify(busy)
				}
				o.notify(idle) // Must not disappear behind the burst.
				o.notify(&leaderworkersetv1.LeaderWorkerSet{ObjectMeta: metav1.ObjectMeta{Name: "unrelated"}})
				return stopped
			}}
			require.True(t, o.NeedLeaderElection())
			connect := func() { require.NoError(t, source.Start(t.Context(), queue)) }
			if sourceFirst {
				connect()
			}
			finished := make(chan error, 1)
			go func() { finished <- o.Start(t.Context()) }()
			if !sourceFirst {
				connect()
			}
			select {
			case err := <-finished:
				require.ErrorIs(t, err, stopped, "manager must receive observer errors")
			case <-time.After(time.Second):
				t.Fatal("observer did not start after queue publication")
			}
			require.Equal(t, 2, queue.Len(), "workqueue deduplicates without dropping another request")
			for _, name := range []string{"busy", "idle"} {
				request, shutdown := queue.Get()
				require.False(t, shutdown)
				require.Equal(t, name, request.Name)
				queue.Done(request)
			}
			o.notify(idle) // A new event wakes an otherwise idle consumer.
			require.Equal(t, 1, queue.Len())
		})
	}
	t.Run("cancel before source startup", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		o := &Observer{}
		o.Source(nil)
		require.NoError(t, o.Start(ctx), "cancellation must not start the cache without its queue")
	})
}

type listCheckingCache struct {
	cache.Cache
	query *client.ListOptions
}

func (c *listCheckingCache) List(_ context.Context, _ client.ObjectList, options ...client.ListOption) error {
	c.query = (&client.ListOptions{}).ApplyOptions(options)
	return nil
}

func TestObserverIndexesWithoutChangingCallerSelectors(t *testing.T) {
	c := &listCheckingCache{}
	o := &Observer{cache: c, ready: make(chan struct{})}
	close(o.ready)
	for _, labels := range []client.MatchingLabels{nil, {leaderworkersetv1.SetNameLabelKey: "model"}} {
		query := &client.ListOptions{}
		client.MatchingFields{"metadata.name": "pod"}.ApplyToList(query)
		labels.ApplyToList(query)
		before := query.FieldSelector.String()
		require.NoError(t, o.List(t.Context(), &corev1.PodList{}, query))
		name, indexed := c.query.FieldSelector.RequiresExactMatch(setNameIndex)
		require.Equal(t, labels != nil, indexed)
		if indexed {
			require.Equal(t, "model", name)
		}
		name, found := c.query.FieldSelector.RequiresExactMatch("metadata.name")
		require.True(t, found)
		require.Equal(t, "pod", name)
		require.Equal(t, before, query.FieldSelector.String(), "the caller's options remain unchanged")
		require.Equal(t, query.LabelSelector, c.query.LabelSelector)
	}
}

type notificationCache struct {
	cache.Cache
	parent *leaderworkersetv1.LeaderWorkerSet
	query  *client.GetOptions
	key    client.ObjectKey
}

func (c *notificationCache) Get(_ context.Context, key client.ObjectKey, object client.Object, options ...client.GetOption) error {
	c.key = key
	c.query = (&client.GetOptions{}).ApplyOptions(options)
	*object.(*leaderworkersetv1.LeaderWorkerSet) = *c.parent
	return nil
}

func TestLeaderNotificationsUseOwnerInsteadOfRoutingLabel(t *testing.T) {
	for _, identity := range []leaderworkersetv1.GroupIdentityType{leaderworkersetv1.GroupIdentityOrdinal, leaderworkersetv1.GroupIdentityHash} {
		for _, label := range []string{"", "wrong-lws", "role-a"} {
			t.Run(string(identity)+"/"+label, func(t *testing.T) {
				f := newFixture(identity, 1, 2)
				c := &notificationCache{parent: f.lws}
				calls := 0
				o := &Observer{cache: c, changed: func(lws *leaderworkersetv1.LeaderWorkerSet) { calls++; require.Equal(t, f.lws.UID, lws.UID) }}
				f.workload.SetLabels(map[string]string{leaderworkersetv1.SetNameLabelKey: label})
				o.notify(f.workload)
				require.Equal(t, 1, calls)
				require.Equal(t, client.ObjectKeyFromObject(f.lws), c.key, "even a label naming another LWS must not redirect the event")
				o.notify(f.workerSets[0])
				require.Equal(t, 2, calls, "worker StatefulSets still route through their LWS label")
				require.Equal(t, client.ObjectKeyFromObject(f.lws), c.key)
				f.workload.GetOwnerReferences()[0].UID = "previous-owner"
				o.notify(f.workload)
				require.Equal(t, 2, calls, "a replaced LWS does not own the old leader workload")
			})
		}
	}
}

func TestObserverNotificationUsesReadOnlyParent(t *testing.T) {
	parent := newFixture(leaderworkersetv1.GroupIdentityOrdinal, 1, 1).lws
	before := parent.DeepCopy()
	c := &notificationCache{parent: parent}
	calls := 0
	o := &Observer{cache: c, changed: func(lws *leaderworkersetv1.LeaderWorkerSet) {
		calls++
		require.Equal(t, before, lws)
	}}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: parent.Namespace, Labels: map[string]string{leaderworkersetv1.SetNameLabelKey: parent.Name}}}
	for _, event := range []any{parent, pod, toolscache.DeletedFinalStateUnknown{Obj: pod}} {
		o.notify(event)
	}
	require.Equal(t, 3, calls)
	require.Equal(t, ptr.To(true), c.query.UnsafeDisableDeepCopy, "parent lookup is read-only, like direct LWS notifications")
	require.Equal(t, before, parent, "notification must not mutate the cached object")
}

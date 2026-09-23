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
	"errors"
	"fmt"
	"slices"
	"strconv"
	"testing"

	"github.com/google/go-cmp/cmp"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/lru"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	leaderworkerset "sigs.k8s.io/lws/api/leaderworkerset/v1"
	revisionutils "sigs.k8s.io/lws/pkg/utils/revision"
	"sigs.k8s.io/lws/test/wrappers"
)

// The helpers below are prefixed with lwsStatus to keep them clearly scoped to
// the status/SSA unit tests added in this file and its hash counterpart.

// lwsStatusScheme returns a scheme with every API group the leader reconciler touches.
func lwsStatusScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	for _, addToScheme := range []func(*runtime.Scheme) error{
		corev1.AddToScheme,
		appsv1.AddToScheme,
		leaderworkerset.AddToScheme,
	} {
		if err := addToScheme(scheme); err != nil {
			t.Fatalf("building scheme: %v", err)
		}
	}
	return scheme
}

// lwsStatusNewReconciler builds a reconciler backed by a fake client seeded with objs.
func lwsStatusNewReconciler(t *testing.T, objs ...client.Object) (*LeaderWorkerSetReconciler, client.Client) {
	t.Helper()
	return lwsStatusNewReconcilerWithInterceptor(t, interceptor.Funcs{}, objs...)
}

// lwsStatusNewReconcilerWithInterceptor is lwsStatusNewReconciler with injected client failures.
func lwsStatusNewReconcilerWithInterceptor(t *testing.T, funcs interceptor.Funcs, objs ...client.Object) (*LeaderWorkerSetReconciler, client.Client) {
	t.Helper()
	scheme := lwsStatusScheme(t)
	k8sClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&leaderworkerset.LeaderWorkerSet{}).
		WithObjects(objs...).
		WithInterceptorFuncs(funcs).
		Build()
	return &LeaderWorkerSetReconciler{
		Client:                k8sClient,
		Scheme:                scheme,
		Record:                fakeEventRecorder{},
		revisionEqualityCache: lru.New(maxRevisionEqualityCacheEntries),
	}, k8sClient
}

// lwsStatusLeaderPod builds the leader pod of group index for lws.
func lwsStatusLeaderPod(lws *leaderworkerset.LeaderWorkerSet, index int, revisionKey string, ready bool) *corev1.Pod {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-%d", lws.Name, index),
			Namespace: lws.Namespace,
			Labels: map[string]string{
				leaderworkerset.SetNameLabelKey:     lws.Name,
				leaderworkerset.WorkerIndexLabelKey: "0",
				leaderworkerset.GroupIndexLabelKey:  strconv.Itoa(index),
				leaderworkerset.RevisionKey:         revisionKey,
			},
		},
	}
	if ready {
		pod.Status = corev1.PodStatus{
			Phase:      corev1.PodRunning,
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}},
		}
	}
	return pod
}

// lwsStatusWorkerSts builds the worker statefulset of group index for lws. Its name
// matches the leader pod name, which is how the controller correlates the two.
func lwsStatusWorkerSts(lws *leaderworkerset.LeaderWorkerSet, index int, revisionKey string, ready bool) *appsv1.StatefulSet {
	sts := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-%d", lws.Name, index),
			Namespace: lws.Namespace,
			Labels: map[string]string{
				leaderworkerset.SetNameLabelKey:    lws.Name,
				leaderworkerset.GroupIndexLabelKey: strconv.Itoa(index),
				leaderworkerset.RevisionKey:        revisionKey,
			},
		},
		Spec: appsv1.StatefulSetSpec{Replicas: ptr.To[int32](1)},
	}
	if ready {
		// StatefulsetReady also requires currentRevision == updateRevision, which
		// holds for the zero value.
		sts.Status.AvailableReplicas = 1
	}
	return sts
}

// lwsStatusTrueConditionTypes returns the sorted set of condition types currently true.
func lwsStatusTrueConditionTypes(lws *leaderworkerset.LeaderWorkerSet) []string {
	var trueTypes []string
	for _, condition := range lws.Status.Conditions {
		if condition.Status == metav1.ConditionTrue {
			trueTypes = append(trueTypes, condition.Type)
		}
	}
	slices.Sort(trueTypes)
	return trueTypes
}

func TestEnsureHPAPodSelector(t *testing.T) {
	const wantSelector = "leaderworkerset.sigs.k8s.io/name=test-sample,leaderworkerset.sigs.k8s.io/worker-index=0"

	tests := []struct {
		name         string
		lws          *leaderworkerset.LeaderWorkerSet
		wantUpdated  bool
		wantSelector string
	}{
		{
			name:         "selector is empty, it gets populated with the leader pod selector",
			lws:          wrappers.BuildLeaderWorkerSet("default").Obj(),
			wantUpdated:  true,
			wantSelector: wantSelector,
		},
		{
			name: "selector already set, it is left untouched",
			lws: func() *leaderworkerset.LeaderWorkerSet {
				lws := wrappers.BuildLeaderWorkerSet("default").Obj()
				lws.Status.HPAPodSelector = "stale=selector"
				return lws
			}(),
			wantUpdated:  false,
			wantSelector: "stale=selector",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			updated, err := ensureHPAPodSelector(tc.lws)
			if err != nil {
				t.Fatalf("ensureHPAPodSelector() unexpected error: %v", err)
			}
			if updated != tc.wantUpdated {
				t.Errorf("ensureHPAPodSelector() updated = %t, want %t", updated, tc.wantUpdated)
			}
			if diff := cmp.Diff(tc.wantSelector, tc.lws.Status.HPAPodSelector); diff != "" {
				t.Errorf("unexpected HPAPodSelector (-want +got):\n%s", diff)
			}
		})
	}
}

func TestGetLeaderStatefulSet(t *testing.T) {
	lws := wrappers.BuildLeaderWorkerSet("default").Obj()
	existing := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: lws.Name, Namespace: lws.Namespace},
		Spec:       appsv1.StatefulSetSpec{Replicas: ptr.To[int32](3)},
	}
	getErr := apierrors.NewInternalError(errors.New("boom"))

	tests := []struct {
		name        string
		objs        []client.Object
		funcs       interceptor.Funcs
		wantNil     bool
		wantErr     bool
		wantReplica int32
	}{
		{
			name:        "leader statefulset exists, it is returned",
			objs:        []client.Object{existing},
			wantReplica: 3,
		},
		{
			name:    "leader statefulset missing, nil is returned without an error",
			wantNil: true,
		},
		{
			name: "non NotFound errors are propagated",
			funcs: interceptor.Funcs{
				Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
					return getErr
				},
			},
			wantNil: true,
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			reconciler, _ := lwsStatusNewReconcilerWithInterceptor(t, tc.funcs, tc.objs...)
			sts, err := reconciler.getLeaderStatefulSet(context.Background(), lws)
			if tc.wantErr != (err != nil) {
				t.Fatalf("getLeaderStatefulSet() error = %v, wantErr %t", err, tc.wantErr)
			}
			if tc.wantNil {
				if sts != nil {
					t.Fatalf("getLeaderStatefulSet() = %v, want nil", sts)
				}
				return
			}
			if sts == nil {
				t.Fatal("getLeaderStatefulSet() = nil, want a statefulset")
			}
			if *sts.Spec.Replicas != tc.wantReplica {
				t.Errorf("getLeaderStatefulSet() replicas = %d, want %d", *sts.Spec.Replicas, tc.wantReplica)
			}
		})
	}
}

func TestGetOrCreateRevision(t *testing.T) {
	newLWS := func() *leaderworkerset.LeaderWorkerSet {
		lws := wrappers.BuildLeaderWorkerSet("default").Obj()
		lws.UID = types.UID("lws-uid")
		return lws
	}

	t.Run("no revision exists yet, one is created and persisted", func(t *testing.T) {
		lws := newLWS()
		reconciler, k8sClient := lwsStatusNewReconciler(t, lws)

		revision, err := reconciler.getOrCreateRevision(context.Background(), "", lws)
		if err != nil {
			t.Fatalf("getOrCreateRevision() unexpected error: %v", err)
		}
		if revisionutils.GetRevisionKey(revision) == "" {
			t.Error("getOrCreateRevision() returned a revision without a revision key")
		}
		var revisions appsv1.ControllerRevisionList
		if err := k8sClient.List(context.Background(), &revisions, client.InNamespace(lws.Namespace)); err != nil {
			t.Fatalf("listing revisions: %v", err)
		}
		if len(revisions.Items) != 1 {
			t.Fatalf("got %d persisted revisions, want 1", len(revisions.Items))
		}
		if got := revisions.Items[0].Name; got != revision.Name {
			t.Errorf("persisted revision name = %q, want %q", got, revision.Name)
		}
	})

	t.Run("revision for the key already exists, it is reused without creating another", func(t *testing.T) {
		lws := newLWS()
		reconciler, k8sClient := lwsStatusNewReconciler(t, lws)

		first, err := reconciler.getOrCreateRevision(context.Background(), "", lws)
		if err != nil {
			t.Fatalf("seeding revision: %v", err)
		}
		second, err := reconciler.getOrCreateRevision(context.Background(), revisionutils.GetRevisionKey(first), lws)
		if err != nil {
			t.Fatalf("getOrCreateRevision() unexpected error: %v", err)
		}
		if second.Name != first.Name {
			t.Errorf("getOrCreateRevision() returned revision %q, want the existing %q", second.Name, first.Name)
		}
		var revisions appsv1.ControllerRevisionList
		if err := k8sClient.List(context.Background(), &revisions, client.InNamespace(lws.Namespace)); err != nil {
			t.Fatalf("listing revisions: %v", err)
		}
		if len(revisions.Items) != 1 {
			t.Errorf("got %d persisted revisions, want 1", len(revisions.Items))
		}
	})

	t.Run("revision key is known but the revision is gone, it is recreated with that key", func(t *testing.T) {
		lws := newLWS()
		reconciler, _ := lwsStatusNewReconciler(t, lws)

		revision, err := reconciler.getOrCreateRevision(context.Background(), "stale-key", lws)
		if err != nil {
			t.Fatalf("getOrCreateRevision() unexpected error: %v", err)
		}
		if got := revisionutils.GetRevisionKey(revision); got != "stale-key" {
			t.Errorf("getOrCreateRevision() revision key = %q, want %q", got, "stale-key")
		}
	})

	t.Run("creating the revision fails, the error is propagated", func(t *testing.T) {
		lws := newLWS()
		reconciler, _ := lwsStatusNewReconcilerWithInterceptor(t, interceptor.Funcs{
			Create: func(context.Context, client.WithWatch, client.Object, ...client.CreateOption) error {
				return apierrors.NewInternalError(errors.New("boom"))
			},
		}, lws)

		if _, err := reconciler.getOrCreateRevision(context.Background(), "", lws); err == nil {
			t.Fatal("getOrCreateRevision() error = nil, want an error")
		}
	})
}

func TestUpdateConditions(t *testing.T) {
	const (
		currentRevision = "rev-current"
		oldRevision     = "rev-old"
	)

	tests := []struct {
		name string
		// lws is the set under test; objs are the leader pods / worker statefulsets.
		lws                 *leaderworkerset.LeaderWorkerSet
		objs                []client.Object
		wantUpdate          bool
		wantUpdateDone      bool
		wantReadyReplicas   int32
		wantUpdatedReplicas int32
		wantConditions      []string
		wantErr             bool
	}{
		{
			name: "all groups ready and on the current revision, the set is Available",
			lws:  wrappers.BuildLeaderWorkerSet("default").Replica(2).Size(2).Obj(),
			objs: func() []client.Object {
				lws := wrappers.BuildLeaderWorkerSet("default").Obj()
				return []client.Object{
					lwsStatusLeaderPod(lws, 0, currentRevision, true), lwsStatusWorkerSts(lws, 0, currentRevision, true),
					lwsStatusLeaderPod(lws, 1, currentRevision, true), lwsStatusWorkerSts(lws, 1, currentRevision, true),
				}
			}(),
			wantUpdate:          true,
			wantUpdateDone:      true,
			wantReadyReplicas:   2,
			wantUpdatedReplicas: 2,
			wantConditions:      []string{string(leaderworkerset.LeaderWorkerSetAvailable)},
		},
		{
			name: "a group still runs the old revision, the set becomes UpdateInProgress",
			lws:  wrappers.BuildLeaderWorkerSet("default").Replica(2).Size(2).Obj(),
			objs: func() []client.Object {
				lws := wrappers.BuildLeaderWorkerSet("default").Obj()
				return []client.Object{
					lwsStatusLeaderPod(lws, 0, oldRevision, true), lwsStatusWorkerSts(lws, 0, oldRevision, true),
					lwsStatusLeaderPod(lws, 1, currentRevision, true), lwsStatusWorkerSts(lws, 1, currentRevision, true),
				}
			}(),
			wantUpdate:          true,
			wantUpdateDone:      false,
			wantReadyReplicas:   2,
			wantUpdatedReplicas: 1,
			// updateConditions asks for both UpdateInProgress and Progressing here,
			// but setConditions only applies the first one that changes per call, so
			// Progressing only appears on the next reconcile. See
			// TestSetConditionsAppliesOneConditionPerCall.
			wantConditions: []string{
				string(leaderworkerset.LeaderWorkerSetUpdateInProgress),
			},
		},
		{
			name: "all groups updated but one is not ready, the set is only Progressing",
			lws:  wrappers.BuildLeaderWorkerSet("default").Replica(2).Size(2).Obj(),
			objs: func() []client.Object {
				lws := wrappers.BuildLeaderWorkerSet("default").Obj()
				return []client.Object{
					lwsStatusLeaderPod(lws, 0, currentRevision, true), lwsStatusWorkerSts(lws, 0, currentRevision, true),
					lwsStatusLeaderPod(lws, 1, currentRevision, true), lwsStatusWorkerSts(lws, 1, currentRevision, false),
				}
			}(),
			wantUpdate:          true,
			wantUpdateDone:      false,
			wantReadyReplicas:   1,
			wantUpdatedReplicas: 2,
			wantConditions:      []string{string(leaderworkerset.LeaderWorkerSetProgressing)},
		},
		{
			name: "size one means no worker statefulsets, leader pods alone decide readiness",
			lws:  wrappers.BuildLeaderWorkerSet("default").Replica(2).Size(1).Obj(),
			objs: func() []client.Object {
				lws := wrappers.BuildLeaderWorkerSet("default").Obj()
				return []client.Object{
					lwsStatusLeaderPod(lws, 0, currentRevision, true),
					lwsStatusLeaderPod(lws, 1, currentRevision, true),
				}
			}(),
			wantUpdate:          true,
			wantUpdateDone:      true,
			wantReadyReplicas:   2,
			wantUpdatedReplicas: 2,
			wantConditions:      []string{string(leaderworkerset.LeaderWorkerSetAvailable)},
		},
		{
			name: "worker statefulset not created yet, the group is skipped entirely",
			lws:  wrappers.BuildLeaderWorkerSet("default").Replica(2).Size(2).Obj(),
			objs: func() []client.Object {
				lws := wrappers.BuildLeaderWorkerSet("default").Obj()
				return []client.Object{
					lwsStatusLeaderPod(lws, 0, currentRevision, true), lwsStatusWorkerSts(lws, 0, currentRevision, true),
					lwsStatusLeaderPod(lws, 1, currentRevision, true),
				}
			}(),
			wantUpdate:          true,
			wantUpdateDone:      false,
			wantReadyReplicas:   1,
			wantUpdatedReplicas: 1,
			wantConditions:      []string{string(leaderworkerset.LeaderWorkerSetProgressing)},
		},
		{
			name: "burst replicas count towards status but not towards rollout completion",
			lws:  wrappers.BuildLeaderWorkerSet("default").Replica(1).Size(2).Obj(),
			objs: func() []client.Object {
				lws := wrappers.BuildLeaderWorkerSet("default").Obj()
				return []client.Object{
					lwsStatusLeaderPod(lws, 0, currentRevision, true), lwsStatusWorkerSts(lws, 0, currentRevision, true),
					lwsStatusLeaderPod(lws, 1, currentRevision, true), lwsStatusWorkerSts(lws, 1, currentRevision, true),
				}
			}(),
			wantUpdate:          true,
			wantUpdateDone:      true,
			wantReadyReplicas:   2,
			wantUpdatedReplicas: 2,
			wantConditions:      []string{string(leaderworkerset.LeaderWorkerSetAvailable)},
		},
		{
			name: "a non zero partition keeps the rollout open even when the partitioned groups are done",
			lws:  wrappers.BuildLeaderWorkerSet("default").Replica(2).Size(2).Partition(1).Obj(),
			objs: func() []client.Object {
				lws := wrappers.BuildLeaderWorkerSet("default").Obj()
				return []client.Object{
					// Group 0 is below the partition, so it legitimately stays on the old revision.
					lwsStatusLeaderPod(lws, 0, oldRevision, true), lwsStatusWorkerSts(lws, 0, oldRevision, true),
					lwsStatusLeaderPod(lws, 1, currentRevision, true), lwsStatusWorkerSts(lws, 1, currentRevision, true),
				}
			}(),
			wantUpdate:          true,
			wantUpdateDone:      false,
			wantReadyReplicas:   2,
			wantUpdatedReplicas: 1,
			wantConditions:      []string{string(leaderworkerset.LeaderWorkerSetAvailable)},
		},
		{
			name: "leader pod with an unparsable group index surfaces an error",
			lws:  wrappers.BuildLeaderWorkerSet("default").Replica(1).Size(2).Obj(),
			objs: func() []client.Object {
				lws := wrappers.BuildLeaderWorkerSet("default").Obj()
				pod := lwsStatusLeaderPod(lws, 0, currentRevision, true)
				pod.Labels[leaderworkerset.GroupIndexLabelKey] = "not-a-number"
				return []client.Object{pod}
			}(),
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			reconciler, _ := lwsStatusNewReconciler(t, tc.objs...)

			gotUpdate, gotUpdateDone, err := reconciler.updateConditions(context.Background(), tc.lws, currentRevision)
			if tc.wantErr != (err != nil) {
				t.Fatalf("updateConditions() error = %v, wantErr %t", err, tc.wantErr)
			}
			if tc.wantErr {
				return
			}
			if gotUpdate != tc.wantUpdate {
				t.Errorf("updateConditions() update = %t, want %t", gotUpdate, tc.wantUpdate)
			}
			if gotUpdateDone != tc.wantUpdateDone {
				t.Errorf("updateConditions() updateDone = %t, want %t", gotUpdateDone, tc.wantUpdateDone)
			}
			if tc.lws.Status.ReadyReplicas != tc.wantReadyReplicas {
				t.Errorf("status.readyReplicas = %d, want %d", tc.lws.Status.ReadyReplicas, tc.wantReadyReplicas)
			}
			if tc.lws.Status.UpdatedReplicas != tc.wantUpdatedReplicas {
				t.Errorf("status.updatedReplicas = %d, want %d", tc.lws.Status.UpdatedReplicas, tc.wantUpdatedReplicas)
			}
			if diff := cmp.Diff(tc.wantConditions, lwsStatusTrueConditionTypes(tc.lws)); diff != "" {
				t.Errorf("unexpected true conditions (-want +got):\n%s", diff)
			}
		})
	}
}

func TestUpdateConditionsListError(t *testing.T) {
	lws := wrappers.BuildLeaderWorkerSet("default").Obj()
	reconciler, _ := lwsStatusNewReconcilerWithInterceptor(t, interceptor.Funcs{
		List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
			return apierrors.NewInternalError(errors.New("boom"))
		},
	})

	if _, _, err := reconciler.updateConditions(context.Background(), lws, "rev"); err == nil {
		t.Fatal("updateConditions() error = nil, want an error")
	}
}

// TestSetConditionsAppliesOneConditionPerCall pins down a surprising behaviour of
// setConditions: it accumulates its return value with
// `shouldUpdate = shouldUpdate || setCondition(...)`, and Go short-circuits `||`
// once shouldUpdate is true. Every condition after the first one that changes is
// therefore silently dropped, and updateConditions/updateStatusHash need a second
// reconcile before both UpdateInProgress and Progressing show up.
func TestSetConditionsAppliesOneConditionPerCall(t *testing.T) {
	lws := wrappers.BuildLeaderWorkerSet("default").Obj()
	requested := func() []metav1.Condition {
		return []metav1.Condition{
			makeCondition(leaderworkerset.LeaderWorkerSetUpdateInProgress, lws),
			makeCondition(leaderworkerset.LeaderWorkerSetProgressing, lws),
		}
	}

	if !setConditions(lws, requested()) {
		t.Fatal("setConditions() = false, want true on the first call")
	}
	want := []string{string(leaderworkerset.LeaderWorkerSetUpdateInProgress)}
	if diff := cmp.Diff(want, lwsStatusTrueConditionTypes(lws)); diff != "" {
		t.Errorf("after one call, unexpected true conditions (-want +got):\n%s", diff)
	}

	if !setConditions(lws, requested()) {
		t.Fatal("setConditions() = false, want true on the second call")
	}
	want = []string{
		string(leaderworkerset.LeaderWorkerSetProgressing),
		string(leaderworkerset.LeaderWorkerSetUpdateInProgress),
	}
	if diff := cmp.Diff(want, lwsStatusTrueConditionTypes(lws)); diff != "" {
		t.Errorf("after two calls, unexpected true conditions (-want +got):\n%s", diff)
	}

	if setConditions(lws, requested()) {
		t.Error("setConditions() = true on the third call, want false once everything converged")
	}
}

func TestUpdateStatus(t *testing.T) {
	const revisionKey = "rev-current"

	t.Run("status and conditions are recomputed and persisted", func(t *testing.T) {
		lws := wrappers.BuildLeaderWorkerSet("default").Replica(2).Size(2).Generation(7).Obj()
		leaderSts := &appsv1.StatefulSet{
			ObjectMeta: metav1.ObjectMeta{Name: lws.Name, Namespace: lws.Namespace},
			Spec:       appsv1.StatefulSetSpec{Replicas: ptr.To[int32](2)},
			Status:     appsv1.StatefulSetStatus{Replicas: 2},
		}
		reconciler, k8sClient := lwsStatusNewReconciler(t, lws, leaderSts,
			lwsStatusLeaderPod(lws, 0, revisionKey, true), lwsStatusWorkerSts(lws, 0, revisionKey, true),
			lwsStatusLeaderPod(lws, 1, revisionKey, true), lwsStatusWorkerSts(lws, 1, revisionKey, true),
		)

		updateDone, err := reconciler.updateStatus(context.Background(), lws, revisionKey)
		if err != nil {
			t.Fatalf("updateStatus() unexpected error: %v", err)
		}
		if !updateDone {
			t.Error("updateStatus() updateDone = false, want true")
		}

		var persisted leaderworkerset.LeaderWorkerSet
		if err := k8sClient.Get(context.Background(), types.NamespacedName{Name: lws.Name, Namespace: lws.Namespace}, &persisted); err != nil {
			t.Fatalf("reading back the leaderworkerset: %v", err)
		}
		if persisted.Status.Replicas != 2 {
			t.Errorf("persisted status.replicas = %d, want 2", persisted.Status.Replicas)
		}
		if persisted.Status.ReadyReplicas != 2 {
			t.Errorf("persisted status.readyReplicas = %d, want 2", persisted.Status.ReadyReplicas)
		}
		if persisted.Status.UpdatedReplicas != 2 {
			t.Errorf("persisted status.updatedReplicas = %d, want 2", persisted.Status.UpdatedReplicas)
		}
		if persisted.Status.ObservedGeneration != 7 {
			t.Errorf("persisted status.observedGeneration = %d, want 7", persisted.Status.ObservedGeneration)
		}
		if persisted.Status.HPAPodSelector == "" {
			t.Error("persisted status.hpaPodSelector is empty, want the leader pod selector")
		}
		want := []string{string(leaderworkerset.LeaderWorkerSetAvailable)}
		if diff := cmp.Diff(want, lwsStatusTrueConditionTypes(&persisted)); diff != "" {
			t.Errorf("unexpected persisted conditions (-want +got):\n%s", diff)
		}
	})

	t.Run("nothing changed, no status write is issued", func(t *testing.T) {
		lws := wrappers.BuildLeaderWorkerSet("default").Replica(1).Size(1).Generation(1).Obj()
		lws.Status = leaderworkerset.LeaderWorkerSetStatus{
			Replicas:           1,
			ReadyReplicas:      1,
			UpdatedReplicas:    1,
			ObservedGeneration: 1,
			HPAPodSelector:     "leaderworkerset.sigs.k8s.io/name=test-sample,leaderworkerset.sigs.k8s.io/worker-index=0",
			Conditions: []metav1.Condition{{
				Type:               string(leaderworkerset.LeaderWorkerSetAvailable),
				Status:             metav1.ConditionTrue,
				ObservedGeneration: 1,
				Reason:             "AllGroupsReady",
				Message:            "All replicas are ready",
				LastTransitionTime: metav1.Now(),
			}},
		}
		leaderSts := &appsv1.StatefulSet{
			ObjectMeta: metav1.ObjectMeta{Name: lws.Name, Namespace: lws.Namespace},
			Spec:       appsv1.StatefulSetSpec{Replicas: ptr.To[int32](1)},
			Status:     appsv1.StatefulSetStatus{Replicas: 1},
		}

		statusWrites := 0
		reconciler, _ := lwsStatusNewReconcilerWithInterceptor(t, interceptor.Funcs{
			SubResourceUpdate: func(ctx context.Context, c client.Client, subResourceName string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
				statusWrites++
				return c.SubResource(subResourceName).Update(ctx, obj, opts...)
			},
		}, lws, leaderSts, lwsStatusLeaderPod(lws, 0, revisionKey, true))

		updateDone, err := reconciler.updateStatus(context.Background(), lws, revisionKey)
		if err != nil {
			t.Fatalf("updateStatus() unexpected error: %v", err)
		}
		if !updateDone {
			t.Error("updateStatus() updateDone = false, want true")
		}
		if statusWrites != 0 {
			t.Errorf("status was written %d times, want 0", statusWrites)
		}
	})

	t.Run("leader statefulset missing, the error is propagated", func(t *testing.T) {
		lws := wrappers.BuildLeaderWorkerSet("default").Obj()
		reconciler, _ := lwsStatusNewReconciler(t, lws)

		if _, err := reconciler.updateStatus(context.Background(), lws, revisionKey); !apierrors.IsNotFound(err) {
			t.Fatalf("updateStatus() error = %v, want NotFound", err)
		}
	})

	t.Run("conflicting status write is reported to the caller", func(t *testing.T) {
		lws := wrappers.BuildLeaderWorkerSet("default").Replica(1).Size(1).Obj()
		leaderSts := &appsv1.StatefulSet{
			ObjectMeta: metav1.ObjectMeta{Name: lws.Name, Namespace: lws.Namespace},
			Spec:       appsv1.StatefulSetSpec{Replicas: ptr.To[int32](1)},
			Status:     appsv1.StatefulSetStatus{Replicas: 1},
		}
		reconciler, _ := lwsStatusNewReconcilerWithInterceptor(t, interceptor.Funcs{
			SubResourceUpdate: func(context.Context, client.Client, string, client.Object, ...client.SubResourceUpdateOption) error {
				return apierrors.NewConflict(schema.GroupResource{Group: leaderworkerset.GroupVersion.Group, Resource: "leaderworkersets"}, lws.Name, errors.New("conflict"))
			},
		}, lws, leaderSts, lwsStatusLeaderPod(lws, 0, revisionKey, true))

		updateDone, err := reconciler.updateStatus(context.Background(), lws, revisionKey)
		if !apierrors.IsConflict(err) {
			t.Fatalf("updateStatus() error = %v, want Conflict", err)
		}
		if updateDone {
			t.Error("updateStatus() updateDone = true, want false when the write failed")
		}
	})
}

func TestGetReplicaStates(t *testing.T) {
	const (
		currentRevision = "rev-current"
		oldRevision     = "rev-old"
	)

	tests := []struct {
		name        string
		lws         *leaderworkerset.LeaderWorkerSet
		stsReplicas int32
		objs        []client.Object
		want        []replicaState
	}{
		{
			name:        "every group has a ready and updated leader pod plus worker statefulset",
			lws:         wrappers.BuildLeaderWorkerSet("default").Replica(2).Size(2).Obj(),
			stsReplicas: 2,
			objs: func() []client.Object {
				lws := wrappers.BuildLeaderWorkerSet("default").Obj()
				return []client.Object{
					lwsStatusLeaderPod(lws, 0, currentRevision, true), lwsStatusWorkerSts(lws, 0, currentRevision, true),
					lwsStatusLeaderPod(lws, 1, currentRevision, true), lwsStatusWorkerSts(lws, 1, currentRevision, true),
				}
			}(),
			want: []replicaState{{ready: true, updated: true}, {ready: true, updated: true}},
		},
		{
			name:        "missing leader pod marks the whole group as neither ready nor updated",
			lws:         wrappers.BuildLeaderWorkerSet("default").Replica(2).Size(2).Obj(),
			stsReplicas: 2,
			objs: func() []client.Object {
				lws := wrappers.BuildLeaderWorkerSet("default").Obj()
				return []client.Object{
					lwsStatusLeaderPod(lws, 0, currentRevision, true), lwsStatusWorkerSts(lws, 0, currentRevision, true),
					lwsStatusWorkerSts(lws, 1, currentRevision, true),
				}
			}(),
			want: []replicaState{{ready: true, updated: true}, {ready: false, updated: false}},
		},
		{
			name:        "missing worker statefulset marks the whole group as neither ready nor updated",
			lws:         wrappers.BuildLeaderWorkerSet("default").Replica(2).Size(2).Obj(),
			stsReplicas: 2,
			objs: func() []client.Object {
				lws := wrappers.BuildLeaderWorkerSet("default").Obj()
				return []client.Object{
					lwsStatusLeaderPod(lws, 0, currentRevision, true), lwsStatusWorkerSts(lws, 0, currentRevision, true),
					lwsStatusLeaderPod(lws, 1, currentRevision, true),
				}
			}(),
			want: []replicaState{{ready: true, updated: true}, {ready: false, updated: false}},
		},
		{
			name:        "leader and worker readiness and revision are combined per group",
			lws:         wrappers.BuildLeaderWorkerSet("default").Replica(2).Size(2).Obj(),
			stsReplicas: 2,
			objs: func() []client.Object {
				lws := wrappers.BuildLeaderWorkerSet("default").Obj()
				return []client.Object{
					// Updated leader, worker statefulset not ready yet.
					lwsStatusLeaderPod(lws, 0, currentRevision, true), lwsStatusWorkerSts(lws, 0, currentRevision, false),
					// Ready group, but the worker statefulset is still on the old revision.
					lwsStatusLeaderPod(lws, 1, currentRevision, true), lwsStatusWorkerSts(lws, 1, oldRevision, true),
				}
			}(),
			want: []replicaState{{ready: false, updated: true}, {ready: true, updated: false}},
		},
		{
			name:        "size one ignores worker statefulsets altogether",
			lws:         wrappers.BuildLeaderWorkerSet("default").Replica(2).Size(1).Obj(),
			stsReplicas: 2,
			objs: func() []client.Object {
				lws := wrappers.BuildLeaderWorkerSet("default").Obj()
				return []client.Object{
					lwsStatusLeaderPod(lws, 0, currentRevision, true),
					lwsStatusLeaderPod(lws, 1, oldRevision, false),
				}
			}(),
			want: []replicaState{{ready: true, updated: true}, {ready: false, updated: false}},
		},
		{
			name:        "no leader pods at all yields an all zero state slice",
			lws:         wrappers.BuildLeaderWorkerSet("default").Replica(2).Size(1).Obj(),
			stsReplicas: 2,
			want:        []replicaState{{}, {}},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			reconciler, _ := lwsStatusNewReconciler(t, tc.objs...)

			got, err := reconciler.getReplicaStates(context.Background(), tc.lws, tc.stsReplicas, currentRevision)
			if err != nil {
				t.Fatalf("getReplicaStates() unexpected error: %v", err)
			}
			if diff := cmp.Diff(tc.want, got, cmp.AllowUnexported(replicaState{})); diff != "" {
				t.Errorf("unexpected replica states (-want +got):\n%s", diff)
			}
		})
	}
}

func TestGetReplicaStatesListError(t *testing.T) {
	lws := wrappers.BuildLeaderWorkerSet("default").Obj()
	reconciler, _ := lwsStatusNewReconcilerWithInterceptor(t, interceptor.Funcs{
		List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
			return apierrors.NewInternalError(errors.New("boom"))
		},
	})

	if _, err := reconciler.getReplicaStates(context.Background(), lws, 2, "rev"); err == nil {
		t.Fatal("getReplicaStates() error = nil, want an error")
	}
}

// lwsStatusRollingUpdateSts builds a leader statefulset in the middle of a rollout.
func lwsStatusRollingUpdateSts(lws *leaderworkerset.LeaderWorkerSet, replicas, partition int32, replicasAnnotation string) *appsv1.StatefulSet {
	sts := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      lws.Name,
			Namespace: lws.Namespace,
		},
		Spec: appsv1.StatefulSetSpec{
			Replicas: ptr.To(replicas),
			UpdateStrategy: appsv1.StatefulSetUpdateStrategy{
				Type: appsv1.RollingUpdateStatefulSetStrategyType,
				RollingUpdate: &appsv1.RollingUpdateStatefulSetStrategy{
					Partition: ptr.To(partition),
				},
			},
		},
	}
	if replicasAnnotation != "" {
		sts.Annotations = map[string]string{leaderworkerset.ReplicasAnnotationKey: replicasAnnotation}
	}
	return sts
}

func TestRollingUpdateParameters(t *testing.T) {
	const revisionKey = "rev-current"

	// rollingLWS is a size 1 set so that leader pods alone drive the replica states.
	rollingLWS := func(replicas int, partition int32, maxUnavailable, maxSurge intstr.IntOrString) *leaderworkerset.LeaderWorkerSet {
		return wrappers.BuildLeaderWorkerSet("default").
			Replica(replicas).
			Size(1).
			RolloutStrategy(leaderworkerset.RolloutStrategy{
				Type: leaderworkerset.RollingUpdateStrategyType,
				RollingUpdateConfiguration: &leaderworkerset.RollingUpdateConfiguration{
					Partition:      ptr.To(partition),
					MaxUnavailable: maxUnavailable,
					MaxSurge:       maxSurge,
				},
			}).Obj()
	}

	tests := []struct {
		name          string
		lws           *leaderworkerset.LeaderWorkerSet
		sts           *appsv1.StatefulSet
		lwsUpdated    bool
		pods          func(*leaderworkerset.LeaderWorkerSet) []client.Object
		wantPartition int32
		wantReplicas  int32
		wantErr       bool
	}{
		{
			name:          "statefulset does not exist yet, everything is created at once",
			lws:           rollingLWS(3, 0, intstr.FromInt32(1), intstr.FromInt32(0)),
			wantPartition: 0,
			wantReplicas:  3,
		},
		{
			name:          "statefulset does not exist yet, the user partition still floors the result",
			lws:           rollingLWS(3, 2, intstr.FromInt32(1), intstr.FromInt32(0)),
			wantPartition: 2,
			wantReplicas:  3,
		},
		{
			name:          "rollout already finished, partition stays at zero",
			lws:           rollingLWS(3, 0, intstr.FromInt32(1), intstr.FromInt32(0)),
			sts:           lwsStatusRollingUpdateSts(wrappers.BuildLeaderWorkerSet("default").Obj(), 3, 0, "3"),
			wantPartition: 0,
			wantReplicas:  3,
		},
		{
			name:          "scaling up mid rollout delays the rollout and keeps the partition",
			lws:           rollingLWS(4, 0, intstr.FromInt32(1), intstr.FromInt32(0)),
			sts:           lwsStatusRollingUpdateSts(wrappers.BuildLeaderWorkerSet("default").Obj(), 2, 2, "2"),
			wantPartition: 2,
			wantReplicas:  4,
		},
		{
			name:          "a fresh update scales down and rolls at the same time",
			lws:           rollingLWS(2, 0, intstr.FromInt32(1), intstr.FromInt32(0)),
			sts:           lwsStatusRollingUpdateSts(wrappers.BuildLeaderWorkerSet("default").Obj(), 3, 0, "3"),
			lwsUpdated:    true,
			wantPartition: 2,
			wantReplicas:  2,
		},
		{
			name: "rollout in progress, the partition steps down by maxUnavailable",
			lws:  rollingLWS(3, 0, intstr.FromInt32(1), intstr.FromInt32(0)),
			sts:  lwsStatusRollingUpdateSts(wrappers.BuildLeaderWorkerSet("default").Obj(), 3, 3, "3"),
			pods: func(lws *leaderworkerset.LeaderWorkerSet) []client.Object {
				return []client.Object{
					lwsStatusLeaderPod(lws, 0, "rev-old", true),
					lwsStatusLeaderPod(lws, 1, "rev-old", true),
					lwsStatusLeaderPod(lws, 2, "rev-old", true),
				}
			},
			wantPartition: 2,
			wantReplicas:  3,
		},
		{
			name: "rollout in progress, an already updated tail replica lets the partition drop further",
			lws:  rollingLWS(3, 0, intstr.FromInt32(1), intstr.FromInt32(0)),
			sts:  lwsStatusRollingUpdateSts(wrappers.BuildLeaderWorkerSet("default").Obj(), 3, 2, "3"),
			pods: func(lws *leaderworkerset.LeaderWorkerSet) []client.Object {
				return []client.Object{
					lwsStatusLeaderPod(lws, 0, "rev-old", true),
					lwsStatusLeaderPod(lws, 1, "rev-old", true),
					lwsStatusLeaderPod(lws, 2, revisionKey, true),
				}
			},
			wantPartition: 1,
			wantReplicas:  3,
		},
		{
			name: "replicas shrank during a rollout, a single surge replica is reclaimed",
			lws:  rollingLWS(2, 0, intstr.FromInt32(1), intstr.FromInt32(1)),
			sts:  lwsStatusRollingUpdateSts(wrappers.BuildLeaderWorkerSet("default").Obj(), 3, 3, "3"),
			pods: func(lws *leaderworkerset.LeaderWorkerSet) []client.Object {
				return []client.Object{
					lwsStatusLeaderPod(lws, 0, revisionKey, true),
					lwsStatusLeaderPod(lws, 1, revisionKey, true),
					lwsStatusLeaderPod(lws, 2, revisionKey, true),
				}
			},
			wantPartition: 3,
			wantReplicas:  2,
		},
		{
			name: "replicas shrank during a rollout, several surge replicas are reclaimed at once",
			lws:  rollingLWS(2, 0, intstr.FromInt32(1), intstr.FromInt32(2)),
			sts:  lwsStatusRollingUpdateSts(wrappers.BuildLeaderWorkerSet("default").Obj(), 4, 4, "4"),
			pods: func(lws *leaderworkerset.LeaderWorkerSet) []client.Object {
				return []client.Object{
					lwsStatusLeaderPod(lws, 0, revisionKey, true),
					lwsStatusLeaderPod(lws, 1, revisionKey, true),
					lwsStatusLeaderPod(lws, 2, revisionKey, true),
					lwsStatusLeaderPod(lws, 3, revisionKey, true),
				}
			},
			wantPartition: 4,
			wantReplicas:  2,
		},
		{
			name:    "maxSurge is not a valid int or percentage",
			lws:     rollingLWS(3, 0, intstr.FromInt32(1), intstr.FromString("bogus")),
			sts:     lwsStatusRollingUpdateSts(wrappers.BuildLeaderWorkerSet("default").Obj(), 3, 0, "3"),
			wantErr: true,
		},
		{
			name:    "maxUnavailable is not a valid int or percentage",
			lws:     rollingLWS(3, 0, intstr.FromString("bogus"), intstr.FromInt32(0)),
			sts:     lwsStatusRollingUpdateSts(wrappers.BuildLeaderWorkerSet("default").Obj(), 3, 0, "3"),
			wantErr: true,
		},
		{
			name: "the replicas annotation is missing from the leader statefulset",
			lws:  rollingLWS(3, 0, intstr.FromInt32(1), intstr.FromInt32(0)),
			sts:  lwsStatusRollingUpdateSts(wrappers.BuildLeaderWorkerSet("default").Obj(), 3, 3, ""),
			pods: func(lws *leaderworkerset.LeaderWorkerSet) []client.Object {
				return []client.Object{lwsStatusLeaderPod(lws, 0, revisionKey, true)}
			},
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var objs []client.Object
			if tc.pods != nil {
				objs = tc.pods(tc.lws)
			}
			reconciler, _ := lwsStatusNewReconciler(t, objs...)

			partition, replicas, err := reconciler.rollingUpdateParameters(context.Background(), tc.lws, tc.sts, revisionKey, tc.lwsUpdated)
			if tc.wantErr != (err != nil) {
				t.Fatalf("rollingUpdateParameters() error = %v, wantErr %t", err, tc.wantErr)
			}
			if tc.wantErr {
				return
			}
			if partition != tc.wantPartition {
				t.Errorf("rollingUpdateParameters() partition = %d, want %d", partition, tc.wantPartition)
			}
			if replicas != tc.wantReplicas {
				t.Errorf("rollingUpdateParameters() replicas = %d, want %d", replicas, tc.wantReplicas)
			}
		})
	}
}

func TestSSAWithStatefulset(t *testing.T) {
	ctx := context.Background()
	lws := wrappers.BuildLeaderWorkerSet("default").Replica(3).Size(2).Obj()
	lws.UID = types.UID("lws-uid")
	reconciler, k8sClient := lwsStatusNewReconciler(t, lws)

	if err := reconciler.SSAWithStatefulset(ctx, lws, 2, 3, "rev-1"); err != nil {
		t.Fatalf("SSAWithStatefulset() unexpected error: %v", err)
	}

	var sts appsv1.StatefulSet
	key := types.NamespacedName{Name: lws.Name, Namespace: lws.Namespace}
	if err := k8sClient.Get(ctx, key, &sts); err != nil {
		t.Fatalf("reading back the leader statefulset: %v", err)
	}
	if *sts.Spec.Replicas != 3 {
		t.Errorf("leader statefulset replicas = %d, want 3", *sts.Spec.Replicas)
	}
	if got := *sts.Spec.UpdateStrategy.RollingUpdate.Partition; got != 2 {
		t.Errorf("leader statefulset partition = %d, want 2", got)
	}
	if got := revisionutils.GetRevisionKey(&sts); got != "rev-1" {
		t.Errorf("leader statefulset revision key = %q, want %q", got, "rev-1")
	}
	if got := sts.Annotations[leaderworkerset.ReplicasAnnotationKey]; got != "3" {
		t.Errorf("leader statefulset replicas annotation = %q, want %q", got, "3")
	}
	owner := metav1.GetControllerOf(&sts)
	if owner == nil || owner.Kind != "LeaderWorkerSet" || owner.Name != lws.Name {
		t.Errorf("leader statefulset controller owner = %+v, want the leaderworkerset", owner)
	}

	// Re-applying with new parameters must converge the existing object rather than fail.
	if err := reconciler.SSAWithStatefulset(ctx, lws, 0, 3, "rev-2"); err != nil {
		t.Fatalf("SSAWithStatefulset() second apply unexpected error: %v", err)
	}
	if err := k8sClient.Get(ctx, key, &sts); err != nil {
		t.Fatalf("reading back the leader statefulset: %v", err)
	}
	if got := *sts.Spec.UpdateStrategy.RollingUpdate.Partition; got != 0 {
		t.Errorf("leader statefulset partition after re-apply = %d, want 0", got)
	}
	if got := revisionutils.GetRevisionKey(&sts); got != "rev-2" {
		t.Errorf("leader statefulset revision key after re-apply = %q, want %q", got, "rev-2")
	}
}

func TestSSAWithStatefulsetOwnerReferenceError(t *testing.T) {
	// A scheme without the LWS types cannot resolve the owner GVK.
	scheme := runtime.NewScheme()
	if err := appsv1.AddToScheme(scheme); err != nil {
		t.Fatalf("building scheme: %v", err)
	}
	reconciler := &LeaderWorkerSetReconciler{
		Client: fake.NewClientBuilder().WithScheme(scheme).Build(),
		Scheme: scheme,
		Record: fakeEventRecorder{},
	}
	lws := wrappers.BuildLeaderWorkerSet("default").Obj()

	if err := reconciler.SSAWithStatefulset(context.Background(), lws, 0, 2, "rev-1"); err == nil {
		t.Fatal("SSAWithStatefulset() error = nil, want an error for the unregistered owner type")
	}
}

func TestServerSideApplyPropagatesPatchErrors(t *testing.T) {
	lws := wrappers.BuildLeaderWorkerSet("default").Obj()
	lws.UID = types.UID("lws-uid")
	reconciler, _ := lwsStatusNewReconcilerWithInterceptor(t, interceptor.Funcs{
		Patch: func(context.Context, client.WithWatch, client.Object, client.Patch, ...client.PatchOption) error {
			return apierrors.NewInternalError(errors.New("boom"))
		},
	}, lws)

	if err := reconciler.SSAWithStatefulset(context.Background(), lws, 0, 2, "rev-1"); err == nil {
		t.Fatal("SSAWithStatefulset() error = nil, want the patch error")
	}
}

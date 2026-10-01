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

package disaggregatedset

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	disaggregatedsetv1 "sigs.k8s.io/lws/api/disaggregatedset/v1"
	leaderworkersetv1 "sigs.k8s.io/lws/api/leaderworkerset/v1"
)

func reversalWorkloads(lws *leaderworkersetv1.LeaderWorkerSet) []client.Object {
	meta := metav1.ObjectMeta{Name: lws.Name, Namespace: lws.Namespace, UID: types.UID(lws.Name + "-workload"), Generation: 2,
		OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(lws, leaderworkersetv1.GroupVersion.WithKind("LeaderWorkerSet"))}}
	replicas := getLWSReplicas(lws)
	if lws.Spec.GroupIdentity == leaderworkersetv1.GroupIdentityHash {
		deployment := &appsv1.Deployment{ObjectMeta: meta, Spec: appsv1.DeploymentSpec{Replicas: ptr.To(replicas)},
			Status: appsv1.DeploymentStatus{Replicas: replicas, ObservedGeneration: 2}}
		rs := &appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Name: lws.Name + "-rs", Namespace: lws.Namespace, Generation: 2,
			OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(deployment, appsv1.SchemeGroupVersion.WithKind("Deployment"))}},
			Spec: appsv1.ReplicaSetSpec{Replicas: ptr.To(replicas)}, Status: appsv1.ReplicaSetStatus{Replicas: replicas, ObservedGeneration: 2}}
		return []client.Object{deployment, rs}
	}
	return []client.Object{&appsv1.StatefulSet{ObjectMeta: meta, Spec: appsv1.StatefulSetSpec{Replicas: ptr.To(replicas)},
		Status: appsv1.StatefulSetStatus{Replicas: replicas, ObservedGeneration: 2}}}
}

func reversalLeaderPod(lws *leaderworkersetv1.LeaderWorkerSet, index int) *corev1.Pod {
	return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: fmt.Sprintf("%s-%d", lws.Name, index), Namespace: lws.Namespace,
		Labels: map[string]string{leaderworkersetv1.SetNameLabelKey: lws.Name, leaderworkersetv1.WorkerIndexLabelKey: "0"},
	}, Status: corev1.PodStatus{Phase: corev1.PodRunning,
		Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}}}
}

func TestRollbackWaitsForPriorDownscale(t *testing.T) {
	for _, identity := range []leaderworkersetv1.GroupIdentityType{leaderworkersetv1.GroupIdentityOrdinal, leaderworkersetv1.GroupIdentityHash} {
		t.Run(string(identity), func(t *testing.T) {
			ctx := context.Background()
			createdAt := time.Unix(1000, 0)
			objects := revisionLWSObjects("A", [2]int32{4, 4}, [2]int32{4, 4}, [2]int32{4, 4}, createdAt)
			objects = append(objects, revisionLWSObjects("B", [2]int32{2, 2}, [2]int32{2, 2}, [2]int32{4, 4}, createdAt.Add(time.Hour))...)
			for _, object := range objects[:2] {
				lws := object.(*leaderworkersetv1.LeaderWorkerSet)
				lws.Spec.GroupIdentity, lws.UID = identity, types.UID(lws.Name)
				for i := range 4 {
					pod := reversalLeaderPod(lws, i)
					pod.Finalizers = []string{"test/hold-termination"}
					objects = append(objects, pod)
				}
				// The child has applied the desired shrink, before its Pods finish.
				shrunk := lws.DeepCopy()
				shrunk.Spec.Replicas = ptr.To[int32](2)
				objects = append(objects, reversalWorkloads(shrunk)...)
			}
			c := newTestClient(objects...)
			ds := newTwoRoleTestDisaggregatedSet([2]int32{4, 4}, [2]int{1, 1}, [2]int{})
			manager := NewLeaderWorkerSetManager(c)
			for _, role := range testRoleNames() {
				name := "test-0-A-" + role
				require.NoError(t, manager.Scale(ctx, ds, name, 2))
				for i := 2; i < 4; i++ {
					pod := &corev1.Pod{}
					require.NoError(t, c.Get(ctx, client.ObjectKey{Namespace: testNamespace, Name: fmt.Sprintf("%s-%d", name, i)}, pod))
					require.NoError(t, c.Delete(ctx, pod))
				}
			}
			reconcile := func() bool {
				// Recreate the executor to exercise the durable direction marker.
				_, complete, err := newTestExecutor(c).ReconcileRevisionTransition(ctx, ds, 0, "A", resolveDesiredReplicasByRole(ds, nil))
				require.NoError(t, err)
				return complete
			}
			for range 2 {
				require.False(t, reconcile())
				assertRevisionReplicas(t, c, "A", [2]int32{2, 2})
				assertRevisionReplicas(t, c, "B", [2]int32{2, 2})
			}
			// Replica and Ready counts come from different observations. A
			// matching replica count must not release the older Ready credit.
			for _, role := range testRoleNames() {
				lws := &leaderworkersetv1.LeaderWorkerSet{}
				require.NoError(t, c.Get(ctx, client.ObjectKey{Namespace: testNamespace, Name: "test-0-A-" + role}, lws))
				lws.Status.Replicas = 2
				require.EqualValues(t, 4, lws.Status.ReadyReplicas)
				require.NoError(t, c.Status().Update(ctx, lws))
			}
			require.False(t, reconcile())
			assertRevisionReplicas(t, c, "A", [2]int32{2, 2})
			assertRevisionReplicas(t, c, "B", [2]int32{2, 2})
			// Even matching counters must not hide still-terminating Pods.
			simulateAllReady(c)
			require.False(t, reconcile())
			assertRevisionReplicas(t, c, "A", [2]int32{2, 2})
			for _, role := range testRoleNames() {
				for i := 2; i < 4; i++ {
					pod := &corev1.Pod{}
					require.NoError(t, c.Get(ctx, client.ObjectKey{Namespace: testNamespace, Name: fmt.Sprintf("test-0-A-%s-%d", role, i)}, pod))
					pod.Finalizers = nil
					require.NoError(t, c.Update(ctx, pod))
				}
			}
			require.False(t, reconcile())
			assertRevisionReplicas(t, c, "A", [2]int32{3, 3})
			// Growth intent is not replacement readiness: without a status change,
			// both B replicas must remain through repeated reconciliations.
			require.False(t, reconcile())
			assertRevisionReplicas(t, c, "B", [2]int32{2, 2})
			for range 6 {
				simulateAllReady(c)
				if reconcile() {
					assertRevisionReplicas(t, c, "A", [2]int32{4, 4})
					return
				}
			}
			t.Fatal("rollback did not resume after the old drain settled")
		})
	}
}

func TestScaleDownSettlementRequiresCurrentChildObservation(t *testing.T) {
	tests := []struct {
		name               string
		lwsObserved        int64
		childSpec, current int32
		childObserved      int64
		leaders            int
		terminating        bool
		want               bool
	}{
		{"settled without waiting for Ready", 2, 2, 2, 2, 2, false, true},
		{"LWS generation not observed", 1, 2, 2, 2, 2, false, false},
		{"child Spec not updated", 2, 4, 2, 2, 2, false, false},
		{"child generation not observed", 2, 2, 2, 1, 2, false, false},
		{"child still reports excess replicas", 2, 2, 4, 2, 2, false, false},
		{"excess Pods without deletion timestamps", 2, 2, 2, 2, 4, false, false},
		{"terminating member", 2, 2, 2, 2, 2, true, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			lws := revisionLWS("A", testRolePrefill, 2, 0, time.Now(), 4)
			lws.Generation, lws.Status.ObservedGeneration = 2, tc.lwsObserved
			lws.Annotations[disaggregatedsetv1.ScaleDownPendingAnnotationKey] = "true"
			workload := reversalWorkloads(lws)[0].(*appsv1.StatefulSet)
			workload.Spec.Replicas = ptr.To(tc.childSpec)
			workload.Status.Replicas, workload.Status.ObservedGeneration = tc.current, tc.childObserved
			objects := []client.Object{lws, workload}
			for i := range tc.leaders {
				pod := reversalLeaderPod(lws, i)
				if tc.terminating && i == 0 {
					pod.DeletionTimestamp, pod.Finalizers = ptr.To(metav1.Now()), []string{"test/hold-termination"}
				}
				objects = append(objects, pod)
			}
			manager := NewLeaderWorkerSetManager(newTestClient(objects...))
			settled, err := manager.scaleDownSettled(context.Background(), lws)
			require.NoError(t, err)
			assert.Equal(t, tc.want, settled)
		})
	}
}

func TestScaleDownSettlementUsesLiveReplicaSets(t *testing.T) {
	lws := revisionLWS("A", testRolePrefill, 2, 0, time.Now(), 4)
	lws.Spec.GroupIdentity = leaderworkersetv1.GroupIdentityHash
	lws.Annotations[disaggregatedsetv1.ScaleDownPendingAnnotationKey] = "true"
	objects := append([]client.Object{lws}, reversalWorkloads(lws)...)
	objects = append(objects, reversalLeaderPod(lws, 0), reversalLeaderPod(lws, 1))
	manager := NewLeaderWorkerSetManager(newTestClient(objects...))
	// Cached objects look settled, but the live ReplicaSet has not applied the
	// Deployment's shrink yet. A Deployment generation alone is insufficient.
	rs := objects[2].(*appsv1.ReplicaSet)
	rs.Status.ObservedGeneration = 1
	live := newTestClient(objects...)
	manager.apiReader = live
	settled, err := manager.scaleDownSettled(context.Background(), lws)
	require.NoError(t, err)
	assert.False(t, settled)
	require.NoError(t, live.Get(context.Background(), client.ObjectKeyFromObject(rs), rs))
	rs.Status.ObservedGeneration = rs.Generation
	require.NoError(t, live.Status().Update(context.Background(), rs))
	settled, err = manager.scaleDownSettled(context.Background(), lws)
	require.NoError(t, err)
	assert.True(t, settled)
}

func TestScaleRejectsConcurrentReplicaChange(t *testing.T) {
	lws := revisionLWS("A", testRolePrefill, 4, 4, time.Now(), 4)
	base := fake.NewClientBuilder().WithScheme(testSchemeForUnit()).WithObjects(lws).Build()
	c := interceptor.NewClient(base, interceptor.Funcs{
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			current := &leaderworkersetv1.LeaderWorkerSet{}
			if err := c.Get(ctx, client.ObjectKeyFromObject(obj), current); err != nil {
				return err
			}
			current.Spec.Replicas = ptr.To[int32](3)
			if err := c.Update(ctx, current); err != nil {
				return err
			}
			return c.Patch(ctx, obj, patch, opts...)
		},
	})
	ds := newTwoRoleTestDisaggregatedSet([2]int32{4, 4}, [2]int{1, 1}, [2]int{})
	err := NewLeaderWorkerSetManager(c).Scale(context.Background(), ds, lws.Name, 2)
	require.True(t, apierrors.IsConflict(err), "stale scale write must conflict: %v", err)
	require.NoError(t, base.Get(context.Background(), client.ObjectKeyFromObject(lws), lws))
	assert.EqualValues(t, 3, *lws.Spec.Replicas)
	assert.NotContains(t, lws.Annotations, disaggregatedsetv1.ScaleDownPendingAnnotationKey)
}

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
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	disaggregatedsetv1 "sigs.k8s.io/lws/api/disaggregatedset/v1"
	leaderworkersetv1 "sigs.k8s.io/lws/api/leaderworkerset/v1"
)

type testObservationCache struct {
	cache.Cache
	reader client.Reader
}

func (c testObservationCache) Get(ctx context.Context, key client.ObjectKey, object client.Object, opts ...client.GetOption) error {
	return c.reader.Get(ctx, key, object, opts...)
}

func (c testObservationCache) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	return c.reader.List(ctx, list, opts...)
}

func testObservations(reader client.Reader) *informerObservation {
	o := &informerObservation{cache: testObservationCache{reader: reader}, ready: make(chan struct{}), writes: make(map[client.ObjectKey]*observedWrite)}
	close(o.ready)
	return o
}

func TestWriteReceiptsWaitForTheWholeSlice(t *testing.T) {
	a := revisionLWS("old", testRolePrefill, 4, 4, time.Time{}, 4)
	b := revisionLWS("old", testRoleDecode, 4, 4, time.Time{}, 4)
	base := newTestClient(a, b)
	cached := newTestClient(a.DeepCopy(), b.DeepCopy())
	o := testObservations(cached)
	manager := NewLeaderWorkerSetManager(base)
	manager.reader, manager.observations = o, o
	ds := newTestDisaggregatedSet()
	// Two writes in one already-planned step are allowed. The next step must
	// observe BOTH, even if it would only mutate a different role.
	for _, lws := range []*leaderworkersetv1.LeaderWorkerSet{a, b} {
		require.NoError(t, manager.Scale(t.Context(), ds, lws, 3))
	}
	require.ErrorIs(t, o.observed(t.Context(), ds.UID, "0"), errReplicaGroupsPending)
	require.NoError(t, o.observed(t.Context(), ds.UID, "1"), "another slice is independent")
	require.NoError(t, o.observed(t.Context(), types.UID("another-owner"), "0"))
	r := &DisaggregatedSetReconciler{LWSManager: manager}
	seed := r.seedForRole(t.Context(), ds)
	_, err := seed(testRolePrefill)
	require.ErrorIs(t, err, errReplicaGroupsPending, "a new scaler must not freeze a stale count")
	for i, lws := range []*leaderworkersetv1.LeaderWorkerSet{a, b} {
		current := &leaderworkersetv1.LeaderWorkerSet{}
		require.NoError(t, base.Get(t.Context(), client.ObjectKeyFromObject(lws), current))
		// The informer can skip the exact write response and deliver a later
		// status update. The receipt, not equality of resource versions, matters.
		previous := &leaderworkersetv1.LeaderWorkerSet{}
		require.NoError(t, cached.Get(t.Context(), client.ObjectKeyFromObject(lws), previous))
		current.ResourceVersion = previous.ResourceVersion
		require.NoError(t, cached.Update(t.Context(), current))
		err := o.observed(t.Context(), ds.UID, "0")
		if i == 0 {
			require.ErrorIs(t, err, errReplicaGroupsPending)
		} else {
			require.NoError(t, err)
		}
	}
	require.Empty(t, o.writes)
	count, err := seed(testRolePrefill)
	require.NoError(t, err)
	require.EqualValues(t, 3, count)
}

func TestWriteReceiptErrors(t *testing.T) {
	for _, mode := range []string{"rejected", "not-applied", "applied"} {
		t.Run(mode, func(t *testing.T) {
			lws := revisionLWS("old", testRolePrefill, 4, 4, time.Time{}, 4)
			base, cached := newTestClient(lws), newTestClient(lws.DeepCopy())
			o := testObservations(cached)
			calls := 0
			writer := interceptor.NewClient(base, interceptor.Funcs{
				Patch: func(ctx context.Context, c client.WithWatch, object client.Object, patch client.Patch, opts ...client.PatchOption) error {
					calls++
					if calls == 1 {
						if mode == "rejected" {
							return apierrors.NewConflict(leaderworkersetv1.GroupVersion.WithResource("leaderworkersets").GroupResource(), object.GetName(), errors.New("concurrent write"))
						}
						if mode == "applied" {
							copy := object.DeepCopyObject().(client.Object)
							require.NoError(t, c.Patch(ctx, copy, patch, opts...))
						}
						return errors.New("response lost")
					}
					return c.Patch(ctx, object, patch, opts...)
				},
			})
			manager := NewLeaderWorkerSetManager(writer)
			manager.reader, manager.observations = o, o
			ds := newTestDisaggregatedSet()
			require.Error(t, manager.Scale(t.Context(), ds, lws, 3))
			if mode == "rejected" {
				require.Empty(t, o.writes, "a definitive rejection reserves nothing")
				return
			}
			for range 3 {
				require.ErrorIs(t, o.observed(t.Context(), ds.UID, "0"), errReplicaGroupsPending, "neither elapsed retries nor a retry conflict proves safety")
			}
			current := &leaderworkersetv1.LeaderWorkerSet{}
			require.NoError(t, base.Get(t.Context(), client.ObjectKeyFromObject(lws), current))
			require.EqualValues(t, 3, getLWSReplicas(current))
			previous := &leaderworkersetv1.LeaderWorkerSet{}
			require.NoError(t, cached.Get(t.Context(), client.ObjectKeyFromObject(lws), previous))
			current.ResourceVersion = previous.ResourceVersion
			require.NoError(t, cached.Update(t.Context(), current))
			require.NoError(t, o.observed(t.Context(), ds.UID, "0"))
		})
	}
}

func TestDeletionReceiptDoesNotWaitForPhysicalRemoval(t *testing.T) {
	lws := revisionLWS("old", testRolePrefill, 0, 0, time.Time{}, 4)
	lws.Finalizers = []string{"hold-test-deletion"}
	base, cached := newTestClient(lws), newTestClient(lws.DeepCopy())
	o := testObservations(cached)
	manager := NewLeaderWorkerSetManager(base)
	manager.observations = o
	require.NoError(t, manager.deleteInForeground(t.Context(), lws))
	owner := metav1.GetControllerOf(lws).UID
	require.ErrorIs(t, o.observed(t.Context(), owner, "0"), errReplicaGroupsPending)
	require.NoError(t, cached.Delete(t.Context(), lws))
	require.NoError(t, o.observed(t.Context(), owner, "0"))
	current := &leaderworkersetv1.LeaderWorkerSet{}
	require.NoError(t, cached.Get(t.Context(), client.ObjectKeyFromObject(lws), current))
	require.False(t, current.DeletionTimestamp.IsZero())
}

func TestCreateReceiptSurvivesCoalescingAndParentDeletion(t *testing.T) {
	lws := revisionLWS("new", testRolePrefill, 0, 0, time.Time{}, 4)
	lws.UID = ""
	base, cached := newTestClient(), newTestClient()
	o := testObservations(cached)
	require.NoError(t, o.mutate(t.Context(), lws, false, func(ctx context.Context) error { return base.Create(ctx, lws) }))
	owner := metav1.GetControllerOf(lws)
	require.ErrorIs(t, o.observed(t.Context(), owner.UID, "0"), errReplicaGroupsPending, "absence from an older cache cannot acknowledge creation")
	// Deliver a later version bearing the same receipt; no exact RV match.
	lws.ResourceVersion = ""
	require.NoError(t, cached.Create(t.Context(), lws))
	require.NoError(t, o.observed(t.Context(), owner.UID, "0"))
	require.Empty(t, o.writes)
	key := client.ObjectKeyFromObject(lws)
	parent := client.ObjectKey{Namespace: lws.Namespace, Name: owner.Name}
	o.writes[key] = &observedWrite{parent: parent, receipt: "unseen"}
	o.forget(client.ObjectKey{Namespace: "other", Name: owner.Name})
	require.Len(t, o.writes, 1)
	o.forget(parent)
	require.Empty(t, o.writes)
}

func TestBudgetHistoryDoesNotPruneAnUnobservedCreate(t *testing.T) {
	ds := newTestDisaggregatedSet()
	ds.Annotations = map[string]string{disaggregatedsetv1.MaxUnavailableAnnotationKey: `{"removed":2}`}
	lws := revisionLWS("old", "removed", 0, 0, time.Time{}, 4)
	lws.Annotations[observationReceiptAnnotation] = "created"
	base := newTestClient(ds, lws)
	// The receipt GET advances the fake cache. The subsequent list must see
	// the created LWS before history can be pruned.
	visible := false
	cached := interceptor.NewClient(base, interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, object client.Object, opts ...client.GetOption) error {
			visible = true
			return c.Get(ctx, key, object, opts...)
		},
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if !visible {
				return nil
			}
			return c.List(ctx, list, opts...)
		},
	})
	o := testObservations(cached)
	o.writes[client.ObjectKeyFromObject(lws)] = &observedWrite{owner: ds.UID, slice: "0", uid: lws.UID, receipt: "created"}
	r := newTestReconciler(base)
	r.LWSManager.reader, r.LWSManager.observations = o, o
	require.NoError(t, r.syncMaxUnavailable(t.Context(), ds, map[string]int{}))
	require.Equal(t, 2, rememberedMaxUnavailable(ds)["removed"])
}

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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	disaggregatedsetv1 "sigs.k8s.io/lws/api/disaggregatedset/v1"
	leaderworkersetv1 "sigs.k8s.io/lws/api/leaderworkerset/v1"
)

func TestUpdateStatusExcludesTerminatingWorkloads(t *testing.T) {
	for _, tc := range []struct {
		name                     string
		target, currentReady     int32
		oldSpec                  int32
		deleteOld, deleteCurrent bool
		wantReplicas, wantReady  int32
		wantUpdated              int32
		wantAvailable            bool
	}{
		{name: "cancelled old revision retains stale status", target: 2, currentReady: 2, deleteOld: true, wantReplicas: 2, wantReady: 2, wantUpdated: 2, wantAvailable: true},
		{name: "terminating old revision still has a positive spec", target: 2, currentReady: 2, oldSpec: 2, deleteOld: true, wantReplicas: 2, wantReady: 2, wantUpdated: 2, wantAvailable: true},
		{name: "scaled down old revision still drains", target: 2, currentReady: 2, wantReplicas: 4, wantReady: 4, wantUpdated: 2},
		{name: "terminating current revision is not ready capacity", target: 2, currentReady: 2, deleteOld: true, deleteCurrent: true},
		{name: "stale old readiness does not hide an unready target", target: 2, currentReady: 1, deleteOld: true, wantReplicas: 2, wantReady: 1, wantUpdated: 2},
		{name: "scale to zero can finish", deleteOld: true, wantAvailable: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ds := newDSWithRoles("ds", externalRole("decode"))
			current := servingLWS(ds, "decode", "0", "current", tc.target, tc.currentReady)
			old := servingLWS(ds, "decode", "0", "old", tc.oldSpec, 2, 2)
			old.Status.Replicas, old.Status.UpdatedReplicas = 2, 2
			finalizers := []string{"example.com/scheduler", "example.com/other-controller"}
			old.Finalizers = append([]string(nil), finalizers...)
			current.Finalizers = append([]string(nil), finalizers...)
			c := newTestClient(ds, current, old)
			if tc.deleteOld {
				require.NoError(t, c.Delete(t.Context(), old))
			}
			if tc.deleteCurrent {
				require.NoError(t, c.Delete(t.Context(), current))
			}
			r := newTestReconciler(c)
			scalers, err := r.ScalerManager.Reconcile(t.Context(), ds, func(string) (int32, error) { return tc.target, nil })
			require.NoError(t, err)
			require.Contains(t, scalers, "decode")
			require.NoError(t, c.Get(t.Context(), client.ObjectKeyFromObject(ds), ds))
			require.NoError(t, r.updateStatus(t.Context(), ds, []string{"decode"}, "current", map[string]int{"decode": int(tc.target)}, scalers))
			require.NoError(t, c.Get(t.Context(), client.ObjectKeyFromObject(ds), ds))
			require.Len(t, ds.Status.RoleStatuses, 1)
			status := ds.Status.RoleStatuses[0]
			assert.Equal(t, ptr.To(tc.target), status.DesiredReplicas)
			assert.Equal(t, tc.wantReplicas, status.Replicas)
			assert.Equal(t, tc.wantReady, status.ReadyReplicas)
			assert.Equal(t, tc.wantReady, status.AvailableReplicas)
			assert.Equal(t, tc.wantUpdated, status.UpdatedReplicas)
			assert.Equal(t, tc.wantAvailable, meta.IsStatusConditionTrue(ds.Status.Conditions, string(disaggregatedsetv1.DisaggregatedSetAvailable)))
			assert.Equal(t, !tc.wantAvailable, meta.IsStatusConditionTrue(ds.Status.Conditions, string(disaggregatedsetv1.DisaggregatedSetProgressing)))
			scaler := &disaggregatedsetv1.DisaggregatedSetRoleScaler{}
			require.NoError(t, c.Get(t.Context(), client.ObjectKeyFromObject(scalers["decode"]), scaler))
			assert.Equal(t, tc.wantReplicas, scaler.Status.Replicas)
			for _, lws := range []*leaderworkersetv1.LeaderWorkerSet{old, current} {
				require.NoError(t, c.Get(t.Context(), client.ObjectKeyFromObject(lws), lws))
				assert.Equal(t, finalizers, lws.Finalizers)
			}
			assert.Equal(t, tc.deleteOld, !old.DeletionTimestamp.IsZero())
			assert.Equal(t, tc.deleteCurrent, !current.DeletionTimestamp.IsZero())
		})
	}
}

func TestSubRoleStatusExcludesTerminatingWorkloads(t *testing.T) {
	for _, identity := range []leaderworkersetv1.GroupIdentityType{leaderworkersetv1.GroupIdentityOrdinal, leaderworkersetv1.GroupIdentityHash} {
		t.Run(string(identity), func(t *testing.T) {
			f := newSubRoleFixtureForIdentity(t, identity, 0, "a")
			f.ds.Spec.Roles = []disaggregatedsetv1.DisaggregatedRoleSpec{{Name: "model", SubRoles: []disaggregatedsetv1.DisaggregatedSubRoleSpec{{Name: "a"}}}}
			require.NoError(t, f.manager.client.Update(t.Context(), f.ds))
			old := f.withRevision("old", "a")
			old.lws.Finalizers = []string{"example.com/scheduler"}
			require.NoError(t, f.manager.client.Update(t.Context(), old.lws))
			r := newTestReconciler(f.manager.client)
			r.LWSManager = f.manager
			desired := map[string]int{"model": 1, "model/a": 1}
			require.NoError(t, r.updateStatus(t.Context(), f.ds, []string{"model"}, f.lws.Name, desired, nil))
			require.EqualValues(t, 2, f.ds.Status.RoleStatuses[0].Replicas)
			require.False(t, meta.IsStatusConditionTrue(f.ds.Status.Conditions, string(disaggregatedsetv1.DisaggregatedSetAvailable)))

			require.NoError(t, f.manager.client.Delete(t.Context(), old.lws))
			require.NoError(t, r.updateStatus(t.Context(), f.ds, []string{"model"}, f.lws.Name, desired, nil))
			require.Equal(t, []disaggregatedsetv1.RoleStatus{
				{Name: "model", DesiredReplicas: ptr.To[int32](1), MinAvailableReplicas: ptr.To[int32](0), Replicas: 1, ReadyReplicas: 1, AvailableReplicas: 1, UpdatedReplicas: 1, SubRoleStatuses: []disaggregatedsetv1.SubRoleStatus{
					{Name: "a", DesiredReplicas: ptr.To[int32](1), MinAvailableReplicas: ptr.To[int32](0), Replicas: 1, ReadyReplicas: 1, AvailableReplicas: 1, UpdatedReplicas: 1},
				}},
			}, f.ds.Status.RoleStatuses)
			require.True(t, meta.IsStatusConditionTrue(f.ds.Status.Conditions, string(disaggregatedsetv1.DisaggregatedSetAvailable)))
			condition := meta.FindStatusCondition(f.ds.Status.Conditions, string(disaggregatedsetv1.DisaggregatedSetSubRolesAssigned))
			require.NotNil(t, condition)
			assert.Equal(t, metav1.ConditionTrue, condition.Status)
			require.NoError(t, f.manager.client.Get(t.Context(), client.ObjectKeyFromObject(old.lws), old.lws))
			assert.False(t, old.lws.DeletionTimestamp.IsZero())
			assert.Equal(t, []string{"example.com/scheduler"}, old.lws.Finalizers)
		})
	}
}

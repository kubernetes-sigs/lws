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
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	leaderworkerset "sigs.k8s.io/lws/api/leaderworkerset/v1"
	revisionutils "sigs.k8s.io/lws/pkg/utils/revision"
	"sigs.k8s.io/lws/test/wrappers"
)

// TestRolloutOfGroupsWithoutWorkerStsReportsUpdateInProgress covers rolling out
// a fix to a LeaderReady LWS whose leaders never became Ready, so no worker
// StatefulSet was ever created. The rollout advances either way; the groups
// still on the old revision must keep UpdateInProgress true, exactly as they do
// when their worker StatefulSets exist.
func TestRolloutOfGroupsWithoutWorkerStsReportsUpdateInProgress(t *testing.T) {
	tests := []struct {
		name          string
		withWorkerSts bool
	}{
		{name: "old groups with an unready worker statefulset", withWorkerSts: true},
		{name: "old leaders never Ready, no worker statefulset", withWorkerSts: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			lws := wrappers.BuildBasicLeaderWorkerSet("test-sample", "default").
				Replica(2).
				Size(2).
				StartupPolicy(leaderworkerset.LeaderReadyStartupPolicy).
				RolloutStrategy(leaderworkerset.RolloutStrategy{
					Type: leaderworkerset.RollingUpdateStrategyType,
					RollingUpdateConfiguration: &leaderworkerset.RollingUpdateConfiguration{
						Partition:      ptr.To[int32](0),
						MaxUnavailable: intstr.FromInt32(1),
						MaxSurge:       intstr.FromInt32(0),
					},
				}).Obj()
			lws.UID = "lws-uid"

			// The leader StatefulSet already carries the fixed revision, with
			// the partition still protecting both groups on the old one.
			sts := &appsv1.StatefulSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:       lws.Name,
					Namespace:  lws.Namespace,
					UID:        "leader-sts-uid",
					Generation: 2,
					Labels: map[string]string{
						leaderworkerset.SetNameLabelKey: lws.Name,
						leaderworkerset.RevisionKey:     "rev-2",
						leaderworkerset.RoleLabelKey:    leaderworkerset.RoleLeader,
					},
					Annotations: map[string]string{
						leaderworkerset.ReplicasAnnotationKey: "2",
					},
				},
				Spec: appsv1.StatefulSetSpec{
					Replicas: ptr.To[int32](2),
					UpdateStrategy: appsv1.StatefulSetUpdateStrategy{
						Type:          appsv1.RollingUpdateStatefulSetStrategyType,
						RollingUpdate: &appsv1.RollingUpdateStatefulSetStrategy{Partition: ptr.To[int32](2)},
					},
				},
				Status: appsv1.StatefulSetStatus{
					ObservedGeneration: 2,
					Replicas:           2,
					CurrentRevision:    "sts-rev-1",
					UpdateRevision:     "sts-rev-2",
				},
			}

			objs := []client.Object{lws, sts}
			for ord := 0; ord < 2; ord++ {
				group := makeOwnedGroupObjects(lws, sts, ord, 2, "rev-1", "sts-rev-1", false, false, false)
				if !tc.withWorkerSts {
					// Keep only the leader pod: under LeaderReady the pod
					// controller creates the worker statefulset once the leader
					// is Ready, which never happened.
					group = group[:1]
				}
				objs = append(objs, group...)
			}

			c, r := newCombinedTestClient(t, objs...)
			rev2CR, err := revisionutils.NewRevision(ctx, c, lws, "rev-2")
			if err != nil {
				t.Fatalf("NewRevision() error = %v", err)
			}
			if _, err := revisionutils.CreateRevision(ctx, c, rev2CR); err != nil {
				t.Fatalf("CreateRevision() error = %v", err)
			}

			req := reconcile.Request{NamespacedName: client.ObjectKeyFromObject(lws)}
			for i := 0; i < 3; i++ {
				if _, err := r.Reconcile(ctx, req); err != nil {
					t.Fatalf("Reconcile() #%d error = %v", i+1, err)
				}
			}

			var updatedSts appsv1.StatefulSet
			if err := c.Get(ctx, req.NamespacedName, &updatedSts); err != nil {
				t.Fatalf("reading leader sts: %v", err)
			}
			if got := *updatedSts.Spec.UpdateStrategy.RollingUpdate.Partition; got != 1 {
				t.Fatalf("partition = %d, want 1 so that the rollout is under way", got)
			}

			var got leaderworkerset.LeaderWorkerSet
			if err := c.Get(ctx, req.NamespacedName, &got); err != nil {
				t.Fatalf("reading lws: %v", err)
			}
			if !apimeta.IsStatusConditionTrue(got.Status.Conditions, string(leaderworkerset.LeaderWorkerSetUpdateInProgress)) {
				t.Errorf("UpdateInProgress is not True while both groups still run the old revision, conditions: %+v", got.Status.Conditions)
			}
		})
	}
}

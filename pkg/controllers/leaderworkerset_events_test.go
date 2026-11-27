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
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/tools/events"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	leaderworkerset "sigs.k8s.io/lws/api/leaderworkerset/v1"
	revisionutils "sigs.k8s.io/lws/pkg/utils/revision"
	"sigs.k8s.io/lws/test/wrappers"
)

// TestGroupsUpdatingEventReportsUpdateAction asserts that lowering the leader
// StatefulSet partition records GroupsUpdating with the Update action. The
// action is a short machine-readable verb that event consumers filter and
// aggregate on, so it must not carry the human-readable note.
func TestGroupsUpdatingEventReportsUpdateAction(t *testing.T) {
	ctx := context.Background()
	lws := wrappers.BuildBasicLeaderWorkerSet("test-sample", "default").
		Replica(3).
		Size(2).
		RolloutStrategy(leaderworkerset.RolloutStrategy{
			Type: leaderworkerset.RollingUpdateStrategyType,
			RollingUpdateConfiguration: &leaderworkerset.RollingUpdateConfiguration{
				Partition:      ptr.To[int32](0),
				MaxUnavailable: intstr.FromInt32(1),
				MaxSurge:       intstr.FromInt32(0),
			},
		}).Obj()
	lws.UID = "lws-uid"

	// The leader StatefulSet already carries the target revision, so the LWS
	// itself is not updated in this pass and the rollout only advances the
	// partition over the groups that still run the old revision.
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
				leaderworkerset.ReplicasAnnotationKey: "3",
			},
		},
		Spec: appsv1.StatefulSetSpec{
			Replicas: ptr.To[int32](3),
			UpdateStrategy: appsv1.StatefulSetUpdateStrategy{
				Type:          appsv1.RollingUpdateStatefulSetStrategyType,
				RollingUpdate: &appsv1.RollingUpdateStatefulSetStrategy{Partition: ptr.To[int32](3)},
			},
		},
		Status: appsv1.StatefulSetStatus{
			ObservedGeneration: 2,
			Replicas:           3,
			CurrentRevision:    "sts-rev-1",
			UpdateRevision:     "sts-rev-2",
		},
	}

	objs := []client.Object{lws, sts}
	// Every group is whole-group Ready on the old revision, so the rollout can
	// take its first step and lower the partition from 3 to 2.
	for ord := 0; ord < 3; ord++ {
		objs = append(objs, makeOwnedGroupObjects(lws, sts, ord, 2, "rev-1", "sts-rev-1", true, true, true)...)
	}

	c, r := newCombinedTestClient(t, objs...)
	rev2CR, err := revisionutils.NewRevision(ctx, c, lws, "rev-2")
	if err != nil {
		t.Fatalf("NewRevision() error = %v", err)
	}
	if _, err := revisionutils.CreateRevision(ctx, c, rev2CR); err != nil {
		t.Fatalf("CreateRevision() error = %v", err)
	}

	// Only a verbose FakeRecorder keeps the action in the recorded string.
	recorder := r.Record.(*events.FakeRecorder)
	recorder.Verbose = true

	req := reconcile.Request{NamespacedName: client.ObjectKeyFromObject(lws)}
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	var updatedSts appsv1.StatefulSet
	if err := c.Get(ctx, req.NamespacedName, &updatedSts); err != nil {
		t.Fatalf("reading sts: %v", err)
	}
	if got := *updatedSts.Spec.UpdateStrategy.RollingUpdate.Partition; got != 2 {
		t.Fatalf("partition = %d, want 2 so that the rollout emits a GroupsUpdating event", got)
	}

	want := strings.Join([]string{"Normal", GroupsUpdating, Update, "Updating replica 2"}, " ")
	var got []string
	for len(recorder.Events) > 0 {
		ev := <-recorder.Events
		got = append(got, ev)
		if strings.Contains(ev, want) {
			return
		}
	}
	t.Fatalf("no event containing %q, got %v", want, got)
}

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

package schedulerprovider

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	schedulingv1alpha3 "k8s.io/api/scheduling/v1alpha3"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	volcanov1beta1 "volcano.sh/apis/pkg/apis/scheduling/v1beta1"

	leaderworkerset "sigs.k8s.io/lws/api/leaderworkerset/v1"
)

func volcanoScheduledLWS() *leaderworkerset.LeaderWorkerSet {
	return &leaderworkerset.LeaderWorkerSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-lws",
			Namespace: "default",
			UID:       types.UID("test-lws-uid"),
			Annotations: map[string]string{
				volcanov1beta1.QueueNameAnnotationKey: "research",
				"volcano.sh/task-spec":                "leader",
				"example.com/not-volcano":             "ignored",
			},
		},
		Spec: leaderworkerset.LeaderWorkerSetSpec{
			Replicas:   ptr.To[int32](2),
			Scheduling: &leaderworkerset.LeaderWorkerSetScheduling{},
			LeaderWorkerTemplate: leaderworkerset.LeaderWorkerTemplate{
				Size: ptr.To[int32](3),
				WorkerTemplate: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "worker", Image: "worker:latest"}},
				}},
			},
		},
	}
}

func TestVolcanoProvider_ReconcileScheduling(t *testing.T) {
	ctx := context.Background()

	t.Run("creates one podgroup per replica", func(t *testing.T) {
		lws := volcanoScheduledLWS()
		fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()

		require.NoError(t, NewVolcanoProvider(fakeClient).ReconcileScheduling(ctx, lws, 2, "rev1"))

		for _, index := range []string{"0", "1"} {
			var pg volcanov1beta1.PodGroup
			name := GetPodGroupName(lws.Name, index, "rev1")
			require.NoError(t, fakeClient.Get(ctx, types.NamespacedName{Name: name, Namespace: lws.Namespace}, &pg))

			assert.Equal(t, *lws.Spec.LeaderWorkerTemplate.Size, pg.Spec.MinMember)
			assert.Equal(t, "research", pg.Spec.Queue)
			assert.Equal(t, map[string]string{
				leaderworkerset.SetNameLabelKey:    lws.Name,
				leaderworkerset.GroupIndexLabelKey: index,
				leaderworkerset.RevisionKey:        "rev1",
			}, pg.Labels)
			// Only volcano.sh/ prefixed annotations are inherited. The queue
			// name lives under scheduling.volcano.sh/ and is copied into the
			// spec instead.
			assert.Equal(t, map[string]string{
				"volcano.sh/task-spec": "leader",
			}, pg.Annotations)

			owner := metav1.GetControllerOf(&pg)
			require.NotNil(t, owner)
			assert.Equal(t, lws.Name, owner.Name)
			assert.Equal(t, "LeaderWorkerSet", owner.Kind)
		}
	})

	t.Run("is a no-op without spec.scheduling", func(t *testing.T) {
		lws := volcanoScheduledLWS()
		lws.Spec.Scheduling = nil
		fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()

		require.NoError(t, NewVolcanoProvider(fakeClient).ReconcileScheduling(ctx, lws, 2, "rev1"))

		var list volcanov1beta1.PodGroupList
		require.NoError(t, fakeClient.List(ctx, &list))
		assert.Empty(t, list.Items)
	})

	t.Run("is idempotent", func(t *testing.T) {
		lws := volcanoScheduledLWS()
		fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
		provider := NewVolcanoProvider(fakeClient)

		require.NoError(t, provider.ReconcileScheduling(ctx, lws, 2, "rev1"))
		require.NoError(t, provider.ReconcileScheduling(ctx, lws, 2, "rev1"))

		var list volcanov1beta1.PodGroupList
		require.NoError(t, fakeClient.List(ctx, &list))
		assert.Len(t, list.Items, 2)
	})

	t.Run("rejects levels other than replica", func(t *testing.T) {
		lws := volcanoScheduledLWS()
		lws.Spec.Scheduling.SchedulingConstraints = &schedulingv1alpha3.WorkloadCompositePodGroupSchedulingConstraints{}
		fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()

		err := NewVolcanoProvider(fakeClient).ReconcileScheduling(ctx, lws, 2, "rev1")
		require.Error(t, err)
		assert.Equal(t, ReasonUnsupportedProviderCapability, ReconcileErrorReason(err))
	})

	t.Run("rejects an ambiguous scheduling level", func(t *testing.T) {
		lws := volcanoScheduledLWS()
		lws.Spec.Scheduling.SchedulingConstraints = &schedulingv1alpha3.WorkloadCompositePodGroupSchedulingConstraints{}
		lws.Spec.Scheduling.Replica = &leaderworkerset.LeaderWorkerSetReplicaScheduling{}
		fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()

		err := NewVolcanoProvider(fakeClient).ReconcileScheduling(ctx, lws, 2, "rev1")
		require.Error(t, err)
		assert.Equal(t, ReasonInvalidSchedulingConfiguration, ReconcileErrorReason(err))
	})

	t.Run("rejects replica fields Volcano cannot honor", func(t *testing.T) {
		cases := map[string]func(*leaderworkerset.LeaderWorkerSetReplicaScheduling){
			"basic policy": func(replica *leaderworkerset.LeaderWorkerSetReplicaScheduling) {
				replica.SchedulingPolicy = &schedulingv1alpha3.WorkloadCompositePodGroupSchedulingPolicy{
					Basic: &schedulingv1alpha3.WorkloadCompositePodGroupBasicSchedulingPolicy{},
				}
			},
			"scheduling constraints": func(replica *leaderworkerset.LeaderWorkerSetReplicaScheduling) {
				replica.SchedulingConstraints = &schedulingv1alpha3.WorkloadCompositePodGroupSchedulingConstraints{}
			},
			"disruption mode": func(replica *leaderworkerset.LeaderWorkerSetReplicaScheduling) {
				replica.DisruptionMode = &schedulingv1alpha3.WorkloadCompositePodGroupDisruptionMode{}
			},
		}

		for name, mutate := range cases {
			t.Run(name, func(t *testing.T) {
				lws := volcanoScheduledLWS()
				replica := &leaderworkerset.LeaderWorkerSetReplicaScheduling{}
				mutate(replica)
				lws.Spec.Scheduling.Replica = replica
				fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()

				err := NewVolcanoProvider(fakeClient).ReconcileScheduling(ctx, lws, 1, "rev1")
				require.Error(t, err)
				assert.Equal(t, ReasonUnsupportedProviderCapability, ReconcileErrorReason(err))
			})
		}
	})
}

func TestVolcanoProvider_ReconcileSchedulingExistingPodGroupOwnership(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name       string
		owner      string
		deleting   bool
		wantError  bool
		unexpected bool
	}{
		{name: "current LWS", owner: "current"},
		{name: "previous LWS with the same name", owner: "previous", wantError: true},
		{name: "other controller", owner: "other", wantError: true, unexpected: true},
		{name: "no controller", wantError: true, unexpected: true},
		{name: "deleting current LWS PodGroup", owner: "current", deleting: true, wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lws := volcanoScheduledLWS()
			pgName := GetPodGroupName(lws.Name, "0", "rev1")
			pg := &volcanov1beta1.PodGroup{ObjectMeta: metav1.ObjectMeta{Name: pgName, Namespace: lws.Namespace}}
			switch tc.owner {
			case "current":
				pg.OwnerReferences = []metav1.OwnerReference{*metav1.NewControllerRef(lws, leaderworkerset.GroupVersion.WithKind("LeaderWorkerSet"))}
			case "previous":
				previous := lws.DeepCopy()
				previous.UID = "previous-lws-uid"
				pg.OwnerReferences = []metav1.OwnerReference{*metav1.NewControllerRef(previous, leaderworkerset.GroupVersion.WithKind("LeaderWorkerSet"))}
			case "other":
				other := lws.DeepCopy()
				other.Name = "other-lws"
				pg.OwnerReferences = []metav1.OwnerReference{*metav1.NewControllerRef(other, leaderworkerset.GroupVersion.WithKind("LeaderWorkerSet"))}
			}
			if tc.deleting {
				now := metav1.Now()
				pg.DeletionTimestamp = &now
				pg.Finalizers = []string{"volcano.sh/test"}
			}

			fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pg).Build()
			provider := NewVolcanoProvider(fakeClient)
			err := provider.ReconcileScheduling(ctx, lws, 2, "rev1")
			if !tc.wantError {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Equal(t, ReasonPodGroupCreateFailed, ReconcileErrorReason(err))
			assert.Equal(t, tc.unexpected, errors.Is(err, ErrUnexpectedPodGroupOwner))
			var remaining volcanov1beta1.PodGroupList
			require.NoError(t, fakeClient.List(ctx, &remaining))
			assert.Len(t, remaining.Items, 1)
			if tc.owner == "previous" {
				require.NoError(t, fakeClient.Delete(ctx, pg))
				require.NoError(t, provider.ReconcileScheduling(ctx, lws, 2, "rev1"))
				var replacement volcanov1beta1.PodGroup
				require.NoError(t, fakeClient.Get(ctx, types.NamespacedName{Name: pgName, Namespace: lws.Namespace}, &replacement))
				require.NotNil(t, metav1.GetControllerOf(&replacement))
				assert.Equal(t, lws.UID, metav1.GetControllerOf(&replacement).UID)
			}
		})
	}
}

func TestVolcanoProvider_InjectPodGroupMetadata(t *testing.T) {
	provider := NewVolcanoProvider(fake.NewClientBuilder().WithScheme(scheme).Build())

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "test-lws-1",
			Annotations: map[string]string{},
			Labels: map[string]string{
				leaderworkerset.SetNameLabelKey:    "test-lws",
				leaderworkerset.GroupIndexLabelKey: "1",
				leaderworkerset.RevisionKey:        "rev1",
			},
		},
	}

	require.NoError(t, provider.InjectPodGroupMetadata(pod))
	// The webhook stamped name must match the one ReconcileScheduling creates.
	assert.Equal(t, GetPodGroupName("test-lws", "1", "rev1"),
		pod.Annotations[volcanov1beta1.KubeGroupNameAnnotationKey])
}

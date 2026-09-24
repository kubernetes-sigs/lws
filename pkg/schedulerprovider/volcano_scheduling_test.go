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
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	schedulingv1alpha3 "k8s.io/api/scheduling/v1alpha3"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
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

func TestVolcanoProviderHashCleanupAfterScaleDown(t *testing.T) {
	ctx := context.Background()
	lws := volcanoScheduledLWS()
	lws.Spec.GroupIdentity = leaderworkerset.GroupIdentityHash
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(lws).Build()
	provider := NewVolcanoProvider(fakeClient)

	leaders := []*corev1.Pod{
		createTestLeaderPod("leader-a", lws.Namespace, lws.Name, "hash-a", "rev1"),
		createTestLeaderPod("leader-b", lws.Namespace, lws.Name, "hash-b", "rev1"),
	}
	for _, leader := range leaders {
		leader.Labels[leaderworkerset.WorkerIndexLabelKey] = "0"
		require.NoError(t, fakeClient.Create(ctx, leader))
		require.NoError(t, provider.CreatePodGroupIfNotExists(ctx, lws, leader))
	}
	worker := createTestLeaderPod("worker-a", lws.Namespace, lws.Name, "hash-a", "rev1")
	worker.Labels[leaderworkerset.WorkerIndexLabelKey] = "1"
	require.NoError(t, fakeClient.Create(ctx, worker))

	require.NoError(t, provider.ReconcileScheduling(ctx, lws, 2, "rev1"))
	for _, leader := range leaders {
		require.NoError(t, fakeClient.Get(ctx, types.NamespacedName{
			Name: leader.Annotations[volcanov1beta1.KubeGroupNameAnnotationKey], Namespace: lws.Namespace,
		}, &volcanov1beta1.PodGroup{}))
		require.NoError(t, fakeClient.Delete(ctx, leader))
	}
	*lws.Spec.Replicas = 0
	require.NoError(t, provider.ReconcileScheduling(ctx, lws, 0, "rev1"))
	groups := &volcanov1beta1.PodGroupList{}
	require.NoError(t, fakeClient.List(ctx, groups, client.InNamespace(lws.Namespace)))
	require.Len(t, groups.Items, 1, "the group with a worker must survive leader deletion")
	assert.Equal(t, "hash-a", groups.Items[0].Labels[leaderworkerset.GroupIndexLabelKey])

	require.NoError(t, fakeClient.Delete(ctx, worker))
	require.NoError(t, provider.ReconcileScheduling(ctx, lws, 0, "rev1"))
	require.NoError(t, fakeClient.List(ctx, groups, client.InNamespace(lws.Namespace)))
	assert.Empty(t, groups.Items, "no old PodGroups should remain after scale-down")

	for i := range 4 {
		leader := createTestLeaderPod(
			fmt.Sprintf("new-leader-%d", i), lws.Namespace, lws.Name, fmt.Sprintf("new-hash-%d", i), "rev1")
		require.NoError(t, fakeClient.Create(ctx, leader))
		require.NoError(t, provider.CreatePodGroupIfNotExists(ctx, lws, leader))
	}
	*lws.Spec.Replicas = 4
	require.NoError(t, provider.ReconcileScheduling(ctx, lws, 4, "rev1"))
	require.NoError(t, fakeClient.List(ctx, groups, client.InNamespace(lws.Namespace)))
	assert.Len(t, groups.Items, 4, "only PodGroups for the new leaders should remain")
}

func TestVolcanoProviderHashCleanupOnlyDeletesOwnedPodGroups(t *testing.T) {
	ctx := context.Background()
	lws := volcanoScheduledLWS()
	lws.Spec.GroupIdentity = leaderworkerset.GroupIdentityHash
	oldOwner := lws.DeepCopy()
	oldOwner.UID = types.UID("previous-lws")
	leader := createTestLeaderPod("legacy-leader", lws.Namespace, lws.Name, "legacy", "rev1")
	groups := []*volcanov1beta1.PodGroup{
		{ObjectMeta: metav1.ObjectMeta{Name: "owned", Namespace: lws.Namespace,
			Labels:          map[string]string{leaderworkerset.SetNameLabelKey: lws.Name},
			OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(lws, leaderworkerset.GroupVersion.WithKind("LeaderWorkerSet"))}}},
		{ObjectMeta: metav1.ObjectMeta{Name: "previous", Namespace: lws.Namespace,
			Labels:          map[string]string{leaderworkerset.SetNameLabelKey: lws.Name},
			OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(oldOwner, leaderworkerset.GroupVersion.WithKind("LeaderWorkerSet"))}}},
		{ObjectMeta: metav1.ObjectMeta{Name: "legacy", Namespace: lws.Namespace,
			Labels:          map[string]string{leaderworkerset.SetNameLabelKey: lws.Name},
			OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(leader, corev1.SchemeGroupVersion.WithKind("Pod"))}}},
	}
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(groups[0], groups[1], groups[2]).Build()
	require.NoError(t, NewVolcanoProvider(fakeClient).ReconcileScheduling(ctx, lws, 0, "rev1"))
	for i, group := range groups {
		err := fakeClient.Get(ctx, types.NamespacedName{Name: group.Name, Namespace: lws.Namespace}, &volcanov1beta1.PodGroup{})
		if i == 0 {
			assert.True(t, apierrors.IsNotFound(err), "expected owned group to be deleted, got %v", err)
		} else {
			require.NoError(t, err, "foreign and legacy PodGroups must remain untouched")
		}
	}
}

func TestVolcanoProviderHashCleanupReportsDeleteFailure(t *testing.T) {
	ctx := context.Background()
	lws := volcanoScheduledLWS()
	lws.Spec.GroupIdentity = leaderworkerset.GroupIdentityHash
	group := &volcanov1beta1.PodGroup{ObjectMeta: metav1.ObjectMeta{
		Name: "stale", Namespace: lws.Namespace,
		Labels:          map[string]string{leaderworkerset.SetNameLabelKey: lws.Name},
		OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(lws, leaderworkerset.GroupVersion.WithKind("LeaderWorkerSet"))},
	}}
	deleteErr := errors.New("delete failed")
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(group).WithInterceptorFuncs(interceptor.Funcs{
		Delete: func(_ context.Context, _ client.WithWatch, _ client.Object, _ ...client.DeleteOption) error {
			return deleteErr
		},
	}).Build()
	err := NewVolcanoProvider(fakeClient).ReconcileScheduling(ctx, lws, 0, "rev1")
	require.ErrorIs(t, err, deleteErr)
	assert.Equal(t, ReasonPodGroupCleanupBlocked, ReconcileErrorReason(err))
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

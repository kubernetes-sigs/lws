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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	schedulingv1alpha3 "k8s.io/api/scheduling/v1alpha3"
	schedulingv1beta1 "k8s.io/api/scheduling/v1beta1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	leaderworkerset "sigs.k8s.io/lws/api/leaderworkerset/v1"
)

// hashGroupKey is the shape of the group identity that pod admission draws for
// every hash-identity leader: a sha1 hash, not an ordinal.
const (
	hashGroupKey = "3f2a1b9c8d7e6f504132231415161718191a1b1c"
	hashRevision = "revision-1"
)

func testHashScheduledLWS() *leaderworkerset.LeaderWorkerSet {
	lws := testScheduledLWS()
	lws.Spec.GroupIdentity = leaderworkerset.GroupIdentityHash
	return lws
}

// hashLeaderPod mirrors what the pod webhook stamps on a hash-identity leader:
// the group key lands in the group index label and the PodGroup reference is
// derived from it.
func hashLeaderPod(lws *leaderworkerset.LeaderWorkerSet, groupKey string) *corev1.Pod {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      lws.Name + "-abcde",
			Namespace: lws.Namespace,
			Labels: map[string]string{
				leaderworkerset.SetNameLabelKey:     lws.Name,
				leaderworkerset.WorkerIndexLabelKey: "0",
				leaderworkerset.GroupIndexLabelKey:  groupKey,
				leaderworkerset.RevisionKey:         hashRevision,
			},
			Annotations: map[string]string{
				leaderworkerset.GroupIdentityAnnotationKey: string(leaderworkerset.GroupIdentityHash),
				WorkloadSchedulingAnnotationKey:            WorkloadSchedulingValue(lws),
				WorkloadNameAnnotationKey:                  KubernetesWorkloadName(lws),
			},
		},
	}
	return pod
}

func TestKubernetesProviderHashDefersReplicaPodGroupsToLeaders(t *testing.T) {
	ctx := context.Background()
	lws := testHashScheduledLWS()
	fakeClient := newKubernetesFakeClientBuilder().Build()
	provider := NewKubernetesProvider(fakeClient)

	// The LWS controller compiles the Workload, but cannot enumerate group keys.
	require.NoError(t, provider.ReconcileScheduling(ctx, lws, 2, "revision-1"))
	workload := &schedulingv1beta1.Workload{}
	require.NoError(t, fakeClient.Get(ctx, types.NamespacedName{Namespace: lws.Namespace, Name: KubernetesWorkloadName(lws)}, workload))
	require.Len(t, workload.Spec.PodGroupTemplates, 1)
	assert.Equal(t, replicaWorkloadTemplateName, workload.Spec.PodGroupTemplates[0].Name)

	groups := &schedulingv1beta1.PodGroupList{}
	require.NoError(t, fakeClient.List(ctx, groups, client.InNamespace(lws.Namespace)))
	assert.Empty(t, groups.Items, "hash identity has no ordinal replica instances to pre-create")

	leader := hashLeaderPod(lws, hashGroupKey)
	require.NoError(t, provider.CreatePodGroupIfNotExists(ctx, lws, leader))

	name := KubernetesPodGroupName(lws, hashGroupKey, "revision-1")
	podGroup := &schedulingv1beta1.PodGroup{}
	require.NoError(t, fakeClient.Get(ctx, types.NamespacedName{Namespace: lws.Namespace, Name: name}, podGroup))
	require.NotNil(t, podGroup.Spec.WorkloadRef)
	assert.Equal(t, KubernetesWorkloadName(lws), podGroup.Spec.WorkloadRef.WorkloadName)
	assert.Equal(t, replicaWorkloadTemplateName, podGroup.Spec.WorkloadRef.TemplateName)
	require.NotNil(t, podGroup.Spec.SchedulingPolicy.Gang)
	assert.Equal(t, int32(3), podGroup.Spec.SchedulingPolicy.Gang.MinCount)
	assert.Equal(t, hashGroupKey, podGroup.Labels[leaderworkerset.GroupIndexLabelKey])
	assert.Equal(t, "revision-1", podGroup.Labels[leaderworkerset.RevisionKey])
	assert.Equal(t, string(SchedulingModeReplica), podGroup.Labels[SchedulingLevelLabelKey])

	// KEP-666: the leaf group is controller-owned by the LWS, never by the leader pod.
	controller := metav1.GetControllerOf(podGroup)
	require.NotNil(t, controller)
	assert.Equal(t, "LeaderWorkerSet", controller.Kind)
	assert.Equal(t, lws.Name, controller.Name)
	workloadOwner := workloadOwnerReference(podGroup)
	require.NotNil(t, workloadOwner)
	assert.Equal(t, workload.UID, workloadOwner.UID)

	// The webhook computes the same name from the pod alone.
	require.NoError(t, provider.InjectPodGroupMetadata(leader))
	require.NotNil(t, leader.Spec.SchedulingGroup)
	assert.Equal(t, name, ptr.Deref(leader.Spec.SchedulingGroup.PodGroupName, ""))

	// A second pass is idempotent.
	require.NoError(t, provider.CreatePodGroupIfNotExists(ctx, lws, leader))
}

func TestKubernetesProviderHashCleanupKeepsGroupsWithMemberPods(t *testing.T) {
	ctx := context.Background()
	lws := testHashScheduledLWS()
	fakeClient := newKubernetesFakeClientBuilder().Build()
	provider := NewKubernetesProvider(fakeClient)
	require.NoError(t, provider.ReconcileScheduling(ctx, lws, 2, "revision-1"))

	leader := hashLeaderPod(lws, hashGroupKey)
	require.NoError(t, provider.CreatePodGroupIfNotExists(ctx, lws, leader))
	name := KubernetesPodGroupName(lws, hashGroupKey, "revision-1")

	// Even before any pod has spec.schedulingGroup populated (or before member pods
	// are admitted), the active leader's labels keep the PodGroup alive.
	require.NoError(t, fakeClient.Create(ctx, leader))
	require.NoError(t, provider.ReconcileScheduling(ctx, lws, 2, "revision-1"))
	require.NoError(t, fakeClient.Get(ctx, types.NamespacedName{Namespace: lws.Namespace, Name: name}, &schedulingv1beta1.PodGroup{}))

	// With spec.schedulingGroup stamped, the group continues to be retained.
	require.NoError(t, provider.InjectPodGroupMetadata(leader))
	require.NoError(t, fakeClient.Update(ctx, leader))
	require.NoError(t, provider.ReconcileScheduling(ctx, lws, 2, "revision-1"))
	require.NoError(t, fakeClient.Get(ctx, types.NamespacedName{Namespace: lws.Namespace, Name: name}, &schedulingv1beta1.PodGroup{}))

	// Once the leader and all member pods are deleted, the next reconcile collects the PodGroup.
	require.NoError(t, fakeClient.Delete(ctx, leader))
	require.NoError(t, provider.ReconcileScheduling(ctx, lws, 2, "revision-1"))
	err := fakeClient.Get(ctx, types.NamespacedName{Namespace: lws.Namespace, Name: name}, &schedulingv1beta1.PodGroup{})
	assert.True(t, apierrors.IsNotFound(err), "expected the unused hash PodGroup to be deleted, got %v", err)
}

func TestKubernetesProviderHashRoleMode(t *testing.T) {
	ctx := context.Background()
	lws := testHashScheduledLWS()
	lws.Spec.Scheduling = &leaderworkerset.LeaderWorkerSetScheduling{
		Replica: &leaderworkerset.LeaderWorkerSetReplicaScheduling{
			Worker: &leaderworkerset.LeaderWorkerSetWorkerScheduling{
				SchedulingPolicy: &schedulingv1alpha3.WorkloadPodGroupSchedulingPolicy{
					Gang: &schedulingv1alpha3.WorkloadPodGroupGangSchedulingPolicy{},
				},
			},
		},
	}
	fakeClient := newKubernetesFakeClientBuilder().Build()
	provider := NewKubernetesProvider(fakeClient)
	require.NoError(t, provider.ReconcileScheduling(ctx, lws, 2, "revision-1"))

	leader := hashLeaderPod(lws, hashGroupKey)
	require.NoError(t, provider.CreatePodGroupIfNotExists(ctx, lws, leader))

	for _, role := range []string{leaderWorkloadTemplateName, workerWorkloadTemplateName} {
		podGroup := &schedulingv1beta1.PodGroup{}
		name := KubernetesRolePodGroupName(lws, hashGroupKey, role, "revision-1")
		require.NoError(t, fakeClient.Get(ctx, types.NamespacedName{Namespace: lws.Namespace, Name: name}, podGroup))
		assert.Equal(t, role, podGroup.Labels[PodGroupRoleLabelKey])
		assert.Equal(t, hashGroupKey, podGroup.Labels[leaderworkerset.GroupIndexLabelKey])
		assert.Equal(t, role, podGroup.Spec.WorkloadRef.TemplateName)
	}

	// Worker pods resolve to the worker group, leaders to the leader group.
	worker := hashLeaderPod(lws, hashGroupKey)
	worker.Labels[leaderworkerset.WorkerIndexLabelKey] = "1"
	require.NoError(t, provider.InjectPodGroupMetadata(worker))
	assert.Equal(t, KubernetesRolePodGroupName(lws, hashGroupKey, workerWorkloadTemplateName, "revision-1"),
		ptr.Deref(worker.Spec.SchedulingGroup.PodGroupName, ""))

	// When only the leader pod exists (no worker pods created yet), ReconcileScheduling
	// must retain BOTH leader and worker PodGroups.
	require.NoError(t, provider.InjectPodGroupMetadata(leader))
	require.NoError(t, fakeClient.Create(ctx, leader))
	require.NoError(t, provider.ReconcileScheduling(ctx, lws, 2, "revision-1"))
	for _, role := range []string{leaderWorkloadTemplateName, workerWorkloadTemplateName} {
		name := KubernetesRolePodGroupName(lws, hashGroupKey, role, "revision-1")
		require.NoError(t, fakeClient.Get(ctx, types.NamespacedName{Namespace: lws.Namespace, Name: name}, &schedulingv1beta1.PodGroup{}),
			"expected %s PodGroup to be retained before worker pods exist", role)
	}

	// Once the leader is deleted and no member pods exist, both PodGroups are cleaned up.
	require.NoError(t, fakeClient.Delete(ctx, leader))
	require.NoError(t, provider.ReconcileScheduling(ctx, lws, 2, "revision-1"))
	for _, role := range []string{leaderWorkloadTemplateName, workerWorkloadTemplateName} {
		name := KubernetesRolePodGroupName(lws, hashGroupKey, role, "revision-1")
		err := fakeClient.Get(ctx, types.NamespacedName{Namespace: lws.Namespace, Name: name}, &schedulingv1beta1.PodGroup{})
		assert.True(t, apierrors.IsNotFound(err), "expected %s PodGroup to be cleaned up after leader deletion", role)
	}
}

func TestKubernetesProviderHashWholeLWSGroupStaysControllerDriven(t *testing.T) {
	ctx := context.Background()
	lws := testHashScheduledLWS()
	lws.Spec.Scheduling = &leaderworkerset.LeaderWorkerSetScheduling{
		SchedulingPolicy: &schedulingv1alpha3.WorkloadCompositePodGroupSchedulingPolicy{
			Gang: &schedulingv1alpha3.WorkloadCompositePodGroupGangSchedulingPolicy{},
		},
	}
	fakeClient := newKubernetesFakeClientBuilder().Build()
	provider := NewKubernetesProvider(fakeClient)
	require.NoError(t, provider.ReconcileScheduling(ctx, lws, 2, "revision-1"))

	// The whole-LWS name does not depend on the group identity scheme.
	podGroup := &schedulingv1beta1.PodGroup{}
	require.NoError(t, fakeClient.Get(ctx, types.NamespacedName{Namespace: lws.Namespace, Name: KubernetesLWSGroupName(lws)}, podGroup))
	require.NotNil(t, podGroup.Spec.SchedulingPolicy.Gang)
	assert.Equal(t, int32(6), podGroup.Spec.SchedulingPolicy.Gang.MinCount)

	leader := hashLeaderPod(lws, hashGroupKey)
	require.NoError(t, provider.CreatePodGroupIfNotExists(ctx, lws, leader))
	groups := &schedulingv1beta1.PodGroupList{}
	require.NoError(t, fakeClient.List(ctx, groups, client.InNamespace(lws.Namespace)))
	assert.Len(t, groups.Items, 1, "whole-LWS mode needs no per-leader PodGroup")

	require.NoError(t, provider.InjectPodGroupMetadata(leader))
	assert.Equal(t, KubernetesLWSGroupName(lws), ptr.Deref(leader.Spec.SchedulingGroup.PodGroupName, ""))
}

func TestKubernetesProviderHashRequiresWorkloadBeforeLeaderGroups(t *testing.T) {
	ctx := context.Background()
	lws := testHashScheduledLWS()
	fakeClient := newKubernetesFakeClientBuilder().Build()
	provider := NewKubernetesProvider(fakeClient)

	leader := hashLeaderPod(lws, hashGroupKey)
	err := provider.CreatePodGroupIfNotExists(ctx, lws, leader)
	require.Error(t, err)
	assert.Equal(t, ReasonWorkloadCreateFailed, ReconcileErrorReason(err))
	groups := &schedulingv1beta1.PodGroupList{}
	require.NoError(t, fakeClient.List(ctx, groups, client.InNamespace(lws.Namespace)))
	assert.Empty(t, groups.Items)
}

func TestKubernetesProviderCreatePodGroupIfNotExistsIsNoopForOrdinal(t *testing.T) {
	ctx := context.Background()
	lws := testScheduledLWS()
	fakeClient := newKubernetesFakeClientBuilder().Build()
	provider := NewKubernetesProvider(fakeClient)
	require.NoError(t, provider.ReconcileScheduling(ctx, lws, 1, "revision-1"))

	leader := hashLeaderPod(lws, "0")
	delete(leader.Annotations, leaderworkerset.GroupIdentityAnnotationKey)
	require.NoError(t, provider.CreatePodGroupIfNotExists(ctx, lws, leader))

	groups := &schedulingv1beta1.PodGroupList{}
	require.NoError(t, fakeClient.List(ctx, groups, client.InNamespace(lws.Namespace)))
	assert.Len(t, groups.Items, 1, "ordinal instances are pre-created by the LWS controller only")
}

func TestKubernetesProviderHashRejectsLeaderWithoutGroupLabels(t *testing.T) {
	ctx := context.Background()
	lws := testHashScheduledLWS()
	fakeClient := newKubernetesFakeClientBuilder().Build()
	provider := NewKubernetesProvider(fakeClient)
	require.NoError(t, provider.ReconcileScheduling(ctx, lws, 1, "revision-1"))

	leader := hashLeaderPod(lws, hashGroupKey)
	delete(leader.Labels, leaderworkerset.GroupIndexLabelKey)
	err := provider.CreatePodGroupIfNotExists(ctx, lws, leader)
	require.Error(t, err)
	assert.Equal(t, ReasonInvalidSchedulingConfiguration, ReconcileErrorReason(err))
}

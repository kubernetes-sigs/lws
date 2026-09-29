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
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	leaderworkerset "sigs.k8s.io/lws/api/leaderworkerset/v1"
)

const (
	ordinalRevisionA = "revision-a"
	ordinalRevisionB = "revision-b"
)

// oldRevisionLeaderPod mirrors an ordinal-identity leader that the leader
// StatefulSet creates at revision A, the revision a partition keeps it on, with
// the PodGroup reference the pod webhook stamps on it.
func oldRevisionLeaderPod(t *testing.T, provider *KubernetesProvider, lws *leaderworkerset.LeaderWorkerSet, groupIndex string) *corev1.Pod {
	t.Helper()
	pod := hashLeaderPod(lws, groupIndex)
	pod.Name = lws.Name + "-" + groupIndex
	pod.Labels[leaderworkerset.RevisionKey] = ordinalRevisionA
	delete(pod.Annotations, leaderworkerset.GroupIdentityAnnotationKey)
	require.NoError(t, provider.InjectPodGroupMetadata(pod))
	return pod
}

func requirePodGroupExists(t *testing.T, c client.Client, lws *leaderworkerset.LeaderWorkerSet, name, msg string) {
	t.Helper()
	err := c.Get(context.Background(), types.NamespacedName{Namespace: lws.Namespace, Name: name}, &schedulingv1beta1.PodGroup{})
	require.NoError(t, err, msg)
}

func assertPodGroupGone(t *testing.T, c client.Client, lws *leaderworkerset.LeaderWorkerSet, name, msg string) {
	t.Helper()
	err := c.Get(context.Background(), types.NamespacedName{Namespace: lws.Namespace, Name: name}, &schedulingv1beta1.PodGroup{})
	assert.True(t, apierrors.IsNotFound(err), "%s, got %v", msg, err)
}

// A partitioned scale-down removes an old-revision replica and its PodGroup.
// Scaling back up recreates that replica from the old revision, because it
// stays below the partition, so it references an old-revision PodGroup that
// ReconcileScheduling never enumerates. The leader's pod-driven path has to
// materialize it, or the replica waits for a PodGroup that never appears.
func TestKubernetesProviderOrdinalRecreatesOldRevisionPodGroupAfterPartitionedScaleUp(t *testing.T) {
	ctx := context.Background()
	lws := testScheduledLWS()
	fakeClient := newKubernetesFakeClientBuilder().Build()
	provider := NewKubernetesProvider(fakeClient)

	require.NoError(t, provider.ReconcileScheduling(ctx, lws, 2, ordinalRevisionA))
	leader0 := oldRevisionLeaderPod(t, provider, lws, "0")
	leader1 := oldRevisionLeaderPod(t, provider, lws, "1")
	require.NoError(t, fakeClient.Create(ctx, leader0))
	require.NoError(t, fakeClient.Create(ctx, leader1))

	// A template update with partition 2 targets revision B, but both
	// replicas stay on A, so their A PodGroups are still in use.
	require.NoError(t, provider.ReconcileScheduling(ctx, lws, 2, ordinalRevisionB))
	oldGroup := KubernetesPodGroupName(lws, "1", ordinalRevisionA)
	requirePodGroupExists(t, fakeClient, lws, oldGroup, "replica 1 still runs revision A")

	// Scale 2 -> 1: replica 1 and its A PodGroup go away.
	require.NoError(t, fakeClient.Delete(ctx, leader1))
	require.NoError(t, provider.ReconcileScheduling(ctx, lws, 1, ordinalRevisionB))
	assertPodGroupGone(t, fakeClient, lws, oldGroup, "expected the unused A PodGroup of replica 1 to be collected")

	// Scale 1 -> 2: the LWS controller only creates the B PodGroup, while the
	// leader StatefulSet recreates ordinal 1 from revision A.
	require.NoError(t, provider.ReconcileScheduling(ctx, lws, 2, ordinalRevisionB))
	leader1 = oldRevisionLeaderPod(t, provider, lws, "1")
	require.Equal(t, oldGroup, ptr.Deref(leader1.Spec.SchedulingGroup.PodGroupName, ""))
	require.NoError(t, fakeClient.Create(ctx, leader1))
	require.NoError(t, provider.CreatePodGroupIfNotExists(ctx, lws, leader1))
	requirePodGroupExists(t, fakeClient, lws, oldGroup, "the recreated leader must find the PodGroup it references")

	require.NoError(t, provider.ReconcileScheduling(ctx, lws, 2, ordinalRevisionB))
	requirePodGroupExists(t, fakeClient, lws, oldGroup, "the next LWS reconcile must not collect a PodGroup a live leader references")
}

// A restarted leader below the partition is recreated at the old revision
// after the LWS reconcile has already collected its PodGroups. In role mode the
// worker PodGroup is not referenced by any pod until the worker StatefulSet
// exists, so cleanup has to keep it for the live leader.
func TestKubernetesProviderOrdinalRoleModeKeepsOldRevisionGroupsForLiveLeader(t *testing.T) {
	ctx := context.Background()
	lws := testScheduledLWS()
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
	require.NoError(t, provider.ReconcileScheduling(ctx, lws, 2, ordinalRevisionB))

	leader := oldRevisionLeaderPod(t, provider, lws, "0")
	require.NoError(t, fakeClient.Create(ctx, leader))
	require.NoError(t, provider.CreatePodGroupIfNotExists(ctx, lws, leader))

	require.NoError(t, provider.ReconcileScheduling(ctx, lws, 2, ordinalRevisionB))
	for _, role := range []string{leaderWorkloadTemplateName, workerWorkloadTemplateName} {
		name := KubernetesRolePodGroupName(lws, "0", role, ordinalRevisionA)
		requirePodGroupExists(t, fakeClient, lws, name, "expected the revision A "+role+" PodGroup to be retained before worker pods exist")
	}

	require.NoError(t, fakeClient.Delete(ctx, leader))
	require.NoError(t, provider.ReconcileScheduling(ctx, lws, 2, ordinalRevisionB))
	for _, role := range []string{leaderWorkloadTemplateName, workerWorkloadTemplateName} {
		name := KubernetesRolePodGroupName(lws, "0", role, ordinalRevisionA)
		assertPodGroupGone(t, fakeClient, lws, name, "expected the revision A "+role+" PodGroup to be collected after the leader is gone")
	}
}

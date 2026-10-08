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
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	schedulingv1alpha3 "k8s.io/api/scheduling/v1alpha3"
	schedulingv1beta1 "k8s.io/api/scheduling/v1beta1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	leaderworkerset "sigs.k8s.io/lws/api/leaderworkerset/v1"
)

func init() {
	_ = schedulingv1alpha3.AddToScheme(scheme)
	_ = schedulingv1beta1.AddToScheme(scheme)
}

func newKubernetesFakeClientBuilder() *fake.ClientBuilder {
	return fake.NewClientBuilder().WithScheme(scheme).
		WithIndex(&schedulingv1beta1.Workload{}, workloadControllerUIDIndex, workloadControllerUIDIndexValues).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				if obj.GetUID() == "" {
					obj.SetUID(types.UID(obj.GetName() + "-generated-uid"))
				}
				return c.Create(ctx, obj, opts...)
			},
		})
}

func ordinalLeaderPod(lws *leaderworkerset.LeaderWorkerSet, groupIndex string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-%s", lws.Name, groupIndex),
			Namespace: lws.Namespace,
			Labels: map[string]string{
				leaderworkerset.SetNameLabelKey:     lws.Name,
				leaderworkerset.GroupIndexLabelKey:  groupIndex,
				leaderworkerset.WorkerIndexLabelKey: "0",
				leaderworkerset.RevisionKey:         "revision-1",
			},
		},
	}
}

func TestKubernetesProviderReconcileScheduling(t *testing.T) {
	ctx := context.Background()
	lws := testScheduledLWS()
	fakeClient := newKubernetesFakeClientBuilder().Build()
	provider := NewKubernetesProvider(fakeClient)

	err := provider.ReconcileScheduling(ctx, lws, 2, "revision-1")
	require.NoError(t, err)

	workload := &schedulingv1beta1.Workload{}
	require.NoError(t, fakeClient.Get(ctx, types.NamespacedName{Namespace: lws.Namespace, Name: KubernetesWorkloadName(lws)}, workload))
	require.Len(t, workload.Spec.PodGroupTemplates, 1)
	assert.Equal(t, replicaWorkloadTemplateName, workload.Spec.PodGroupTemplates[0].Name)
	require.NotNil(t, workload.Spec.PodGroupTemplates[0].SchedulingPolicy.Gang)
	assert.Equal(t, int32(3), workload.Spec.PodGroupTemplates[0].SchedulingPolicy.Gang.MinCount)
	assert.Equal(t, "high-priority", workload.Spec.PodGroupTemplates[0].PriorityClassName)
	require.NotNil(t, workload.Spec.ControllerRef)
	assert.Equal(t, "LeaderWorkerSet", workload.Spec.ControllerRef.Kind)
	assert.Equal(t, lws.Name, workload.Spec.ControllerRef.Name)

	for groupIndex, name := range []string{KubernetesPodGroupName(lws, "0", "revision-1"), KubernetesPodGroupName(lws, "1", "revision-1")} {
		require.NoError(t, provider.CreatePodGroupIfNotExists(ctx, lws, ordinalLeaderPod(lws, fmt.Sprint(groupIndex))))
		podGroup := &schedulingv1beta1.PodGroup{}
		require.NoError(t, fakeClient.Get(ctx, types.NamespacedName{Namespace: lws.Namespace, Name: name}, podGroup))
		require.NotNil(t, podGroup.Spec.WorkloadRef)
		assert.Equal(t, KubernetesWorkloadName(lws), podGroup.Spec.WorkloadRef.WorkloadName)
		assert.Equal(t, replicaWorkloadTemplateName, podGroup.Spec.WorkloadRef.TemplateName)
		assert.Equal(t, int32(3), podGroup.Spec.SchedulingPolicy.Gang.MinCount)
		assert.Equal(t, fmt.Sprint(groupIndex), podGroup.Labels[leaderworkerset.GroupIndexLabelKey])
		assert.Equal(t, "revision-1", podGroup.Labels[leaderworkerset.RevisionKey])
		assert.Equal(t, string(SchedulingModeReplica), podGroup.Labels[SchedulingLevelLabelKey])
		controller := metav1.GetControllerOf(podGroup)
		require.NotNil(t, controller)
		assert.Equal(t, "LeaderWorkerSet", controller.Kind)
		assert.Equal(t, lws.Name, controller.Name)
		assert.Equal(t, ptr.To(true), controller.Controller)
		workloadOwner := workloadOwnerReference(podGroup)
		require.NotNil(t, workloadOwner)
		assert.Equal(t, workload.Name, workloadOwner.Name)
		assert.Equal(t, workload.UID, workloadOwner.UID)
		assert.Equal(t, ptr.To(false), workloadOwner.Controller)
	}
}

func TestBuildFlatWorkloadReplicaConfiguration(t *testing.T) {
	lws := testScheduledLWS()
	lws.Spec.Scheduling = &leaderworkerset.LeaderWorkerSetScheduling{
		Replica: &leaderworkerset.LeaderWorkerSetReplicaScheduling{
			SchedulingPolicy: &schedulingv1alpha3.WorkloadCompositePodGroupSchedulingPolicy{
				Gang: &schedulingv1alpha3.WorkloadCompositePodGroupGangSchedulingPolicy{},
			},
			SchedulingConstraints: &schedulingv1alpha3.WorkloadCompositePodGroupSchedulingConstraints{
				Topology: []schedulingv1alpha3.TopologyConstraint{{Key: "topology.kubernetes.io/zone"}},
			},
			DisruptionMode: &schedulingv1alpha3.WorkloadCompositePodGroupDisruptionMode{
				All: &schedulingv1alpha3.WorkloadCompositePodGroupAllDisruptionMode{},
			},
		},
	}

	workload, err := buildFlatWorkload(lws)
	require.NoError(t, err)
	require.Len(t, workload.Spec.PodGroupTemplates, 1)
	template := workload.Spec.PodGroupTemplates[0]
	require.NotNil(t, template.SchedulingPolicy.Gang)
	assert.Equal(t, int32(3), template.SchedulingPolicy.Gang.MinCount)
	require.NotNil(t, template.SchedulingConstraints)
	assert.Equal(t, "topology.kubernetes.io/zone", template.SchedulingConstraints.Topology[0].Key)
	require.NotNil(t, template.DisruptionMode)
	require.NotNil(t, template.DisruptionMode.All)
}

func TestBuildFlatWorkloadRoleResourceClaims(t *testing.T) {
	lws := testScheduledLWS()
	lws.Spec.Scheduling = &leaderworkerset.LeaderWorkerSetScheduling{
		Replica: &leaderworkerset.LeaderWorkerSetReplicaScheduling{
			Worker: &leaderworkerset.LeaderWorkerSetWorkerScheduling{
				ResourceClaims: []schedulingv1alpha3.WorkloadPodGroupResourceClaim{{
					Name:                      "gpu",
					ResourceClaimTemplateName: ptr.To("shared-gpu-template"),
				}},
			},
		},
	}
	lws.Spec.LeaderWorkerTemplate.WorkerTemplate.Spec.ResourceClaims = []corev1.PodResourceClaim{{
		Name:                      "gpu",
		ResourceClaimTemplateName: ptr.To("shared-gpu-template"),
	}}

	workload, err := buildFlatWorkload(lws)
	require.NoError(t, err)
	require.Len(t, workload.Spec.PodGroupTemplates, 2)
	var worker *schedulingv1beta1.PodGroupTemplate
	for i := range workload.Spec.PodGroupTemplates {
		if workload.Spec.PodGroupTemplates[i].Name == workerWorkloadTemplateName {
			worker = &workload.Spec.PodGroupTemplates[i]
		}
	}
	require.NotNil(t, worker)
	require.Len(t, worker.ResourceClaims, 1)
	assert.Equal(t, "gpu", worker.ResourceClaims[0].Name)
	assert.Equal(t, lws.Name, workload.Labels[leaderworkerset.SetNameLabelKey])
}

func TestKubernetesProviderIsolatesRecreatedLWSByUID(t *testing.T) {
	ctx := context.Background()
	oldLWS := testScheduledLWS()
	newLWS := oldLWS.DeepCopy()
	newLWS.UID = types.UID("replacement-lws-uid")
	fakeClient := newKubernetesFakeClientBuilder().Build()
	provider := NewKubernetesProvider(fakeClient)

	require.NoError(t, provider.ReconcileScheduling(ctx, oldLWS, 1, "revision-1"))
	require.NoError(t, provider.CreatePodGroupIfNotExists(ctx, oldLWS, ordinalLeaderPod(oldLWS, "0")))
	require.NoError(t, provider.ReconcileScheduling(ctx, newLWS, 1, "revision-1"))
	require.NoError(t, provider.CreatePodGroupIfNotExists(ctx, newLWS, ordinalLeaderPod(newLWS, "0")))
	assert.NotEqual(t, KubernetesWorkloadName(oldLWS), KubernetesWorkloadName(newLWS))

	for _, lws := range []*leaderworkerset.LeaderWorkerSet{oldLWS, newLWS} {
		workload := &schedulingv1beta1.Workload{}
		require.NoError(t, fakeClient.Get(ctx, types.NamespacedName{Namespace: lws.Namespace, Name: KubernetesWorkloadName(lws)}, workload))
		assert.Equal(t, lws.UID, metav1.GetControllerOf(workload).UID)
		group := &schedulingv1beta1.PodGroup{}
		require.NoError(t, fakeClient.Get(ctx, types.NamespacedName{Namespace: lws.Namespace, Name: KubernetesPodGroupName(lws, "0", "revision-1")}, group))
		assert.Equal(t, lws.UID, metav1.GetControllerOf(group).UID)
	}
}

func TestKubernetesProviderRejectsForeignWorkloadAtComputedName(t *testing.T) {
	ctx := context.Background()
	lws := testScheduledLWS()
	foreign := &schedulingv1beta1.Workload{ObjectMeta: metav1.ObjectMeta{
		Name: KubernetesWorkloadName(lws), Namespace: lws.Namespace,
	}}
	fakeClient := newKubernetesFakeClientBuilder().WithObjects(foreign).Build()

	err := NewKubernetesProvider(fakeClient).ReconcileScheduling(ctx, lws, 1, "revision-1")
	require.Error(t, err)
	assert.Equal(t, ReasonWorkloadCreateFailed, ReconcileErrorReason(err))
	groups := &schedulingv1beta1.PodGroupList{}
	require.NoError(t, fakeClient.List(ctx, groups, client.InNamespace(lws.Namespace)))
	assert.Empty(t, groups.Items)
}

func TestKubernetesSchedulingNamesAreUIDQualifiedAndBounded(t *testing.T) {
	lws := testScheduledLWS()
	lws.Name = strings.Repeat("a", 253)
	other := lws.DeepCopy()
	other.UID = types.UID("other-uid")

	for _, name := range []string{
		KubernetesWorkloadName(lws),
		KubernetesLWSGroupName(lws),
		KubernetesPodGroupName(lws, "1000000", strings.Repeat("b", 63)),
		KubernetesRolePodGroupName(lws, "1000000", workerWorkloadTemplateName, strings.Repeat("b", 63)),
		kubernetesRuntimeName(KubernetesWorkloadName(lws)+".x7k2p", "1000000", workerWorkloadTemplateName, strings.Repeat("b", 63)),
	} {
		assert.LessOrEqual(t, len(name), 253)
		assert.Empty(t, validation.IsDNS1123Subdomain(name), name)
	}
	workloadName := KubernetesWorkloadName(lws)
	uidHash := workloadName[strings.LastIndexByte(workloadName, '-')+1:]
	assert.Contains(t, KubernetesRolePodGroupName(lws, "0", workerWorkloadTemplateName, "revision"), "-"+uidHash+"-0-worker-revision")
	assert.NotEqual(t, KubernetesWorkloadName(lws), KubernetesWorkloadName(other))
}

func TestKubernetesProviderWholeLWSMode(t *testing.T) {
	ctx := context.Background()
	lws := testScheduledLWS()
	lws.Spec.Scheduling = &leaderworkerset.LeaderWorkerSetScheduling{
		SchedulingPolicy: &schedulingv1alpha3.WorkloadCompositePodGroupSchedulingPolicy{
			Gang: &schedulingv1alpha3.WorkloadCompositePodGroupGangSchedulingPolicy{},
		},
	}
	fakeClient := newKubernetesFakeClientBuilder().Build()

	require.NoError(t, NewKubernetesProvider(fakeClient).ReconcileScheduling(ctx, lws, 2, "revision-1"))
	workload := &schedulingv1beta1.Workload{}
	require.NoError(t, fakeClient.Get(ctx, types.NamespacedName{Namespace: lws.Namespace, Name: KubernetesWorkloadName(lws)}, workload))
	require.Len(t, workload.Spec.PodGroupTemplates, 1)
	assert.Equal(t, lwsWorkloadTemplateName, workload.Spec.PodGroupTemplates[0].Name)
	assert.Equal(t, int32(6), workload.Spec.PodGroupTemplates[0].SchedulingPolicy.Gang.MinCount)

	group := &schedulingv1beta1.PodGroup{}
	require.NoError(t, fakeClient.Get(ctx, types.NamespacedName{Namespace: lws.Namespace, Name: KubernetesLWSGroupName(lws)}, group))
	assert.Equal(t, lwsWorkloadTemplateName, group.Spec.WorkloadRef.TemplateName)
	assert.Equal(t, string(SchedulingModeLWS), group.Labels[SchedulingLevelLabelKey])
	assert.NotContains(t, group.Labels, leaderworkerset.RevisionKey)

	// Whole-LWS uses one stable PodGroup, so cardinality changes patch the
	// mutable gang minimum instead of creating a revision-specific group.
	lws.Spec.Replicas = ptr.To[int32](3)
	require.NoError(t, NewKubernetesProvider(fakeClient).ReconcileScheduling(ctx, lws, 3, "revision-2"))
	require.NoError(t, fakeClient.Get(ctx, types.NamespacedName{Namespace: lws.Namespace, Name: KubernetesLWSGroupName(lws)}, group))
	assert.Equal(t, int32(9), group.Spec.SchedulingPolicy.Gang.MinCount)

	// At zero replicas the Workload is retained with a valid placeholder, but
	// the runtime whole-LWS PodGroup is removed. Scale-up updates the Workload
	// before recreating the group.
	lws.Spec.Replicas = ptr.To[int32](0)
	require.NoError(t, NewKubernetesProvider(fakeClient).ReconcileScheduling(ctx, lws, 0, "revision-3"))
	require.NoError(t, fakeClient.Get(ctx, types.NamespacedName{Namespace: lws.Namespace, Name: KubernetesWorkloadName(lws)}, workload))
	assert.Equal(t, int32(1), workload.Spec.PodGroupTemplates[0].SchedulingPolicy.Gang.MinCount)
	err := fakeClient.Get(ctx, types.NamespacedName{Namespace: lws.Namespace, Name: KubernetesLWSGroupName(lws)}, group)
	assert.True(t, apierrors.IsNotFound(err))

	lws.Spec.Replicas = ptr.To[int32](1)
	require.NoError(t, NewKubernetesProvider(fakeClient).ReconcileScheduling(ctx, lws, 1, "revision-4"))
	require.NoError(t, fakeClient.Get(ctx, types.NamespacedName{Namespace: lws.Namespace, Name: KubernetesLWSGroupName(lws)}, group))
	assert.Equal(t, int32(3), group.Spec.SchedulingPolicy.Gang.MinCount)
}

func TestKubernetesProviderBlocksPodsWhileDesiredPodGroupIsTerminating(t *testing.T) {
	ctx := context.Background()
	lws := testScheduledLWS()
	lws.Spec.Scheduling = &leaderworkerset.LeaderWorkerSetScheduling{
		SchedulingPolicy: &schedulingv1alpha3.WorkloadCompositePodGroupSchedulingPolicy{
			Gang: &schedulingv1alpha3.WorkloadCompositePodGroupGangSchedulingPolicy{},
		},
	}
	fakeClient := newKubernetesFakeClientBuilder().Build()
	provider := NewKubernetesProvider(fakeClient)
	require.NoError(t, provider.ReconcileScheduling(ctx, lws, 2, "revision-1"))

	group := &schedulingv1beta1.PodGroup{}
	key := types.NamespacedName{Namespace: lws.Namespace, Name: KubernetesLWSGroupName(lws)}
	require.NoError(t, fakeClient.Get(ctx, key, group))
	group.Finalizers = []string{"test.scheduling.k8s.io/protection"}
	require.NoError(t, fakeClient.Update(ctx, group))
	require.NoError(t, fakeClient.Delete(ctx, group))

	err := provider.ReconcileScheduling(ctx, lws, 2, "revision-1")
	require.Error(t, err)
	assert.Equal(t, ReasonPodGroupCleanupBlocked, ReconcileErrorReason(err))
}

func TestKubernetesProviderLeaderWorkerMode(t *testing.T) {
	ctx := context.Background()
	lws := testScheduledLWS()
	lws.Spec.LeaderWorkerTemplate.LeaderTemplate = &corev1.PodTemplateSpec{Spec: corev1.PodSpec{PriorityClassName: "high-priority"}}
	lws.Spec.Scheduling = &leaderworkerset.LeaderWorkerSetScheduling{
		Replica: &leaderworkerset.LeaderWorkerSetReplicaScheduling{
			Leader: &leaderworkerset.LeaderWorkerSetLeaderScheduling{
				SchedulingPolicy: &schedulingv1alpha3.WorkloadPodGroupSchedulingPolicy{Gang: &schedulingv1alpha3.WorkloadPodGroupGangSchedulingPolicy{}},
			},
			Worker: &leaderworkerset.LeaderWorkerSetWorkerScheduling{
				SchedulingPolicy: &schedulingv1alpha3.WorkloadPodGroupSchedulingPolicy{Gang: &schedulingv1alpha3.WorkloadPodGroupGangSchedulingPolicy{}},
			},
		},
	}
	fakeClient := newKubernetesFakeClientBuilder().Build()
	provider := NewKubernetesProvider(fakeClient)

	require.NoError(t, provider.ReconcileScheduling(ctx, lws, 1, "revision-1"))
	workload := &schedulingv1beta1.Workload{}
	require.NoError(t, fakeClient.Get(ctx, types.NamespacedName{Namespace: lws.Namespace, Name: KubernetesWorkloadName(lws)}, workload))
	require.Len(t, workload.Spec.PodGroupTemplates, 2)
	assert.Equal(t, leaderWorkloadTemplateName, workload.Spec.PodGroupTemplates[0].Name)
	assert.Equal(t, int32(1), workload.Spec.PodGroupTemplates[0].SchedulingPolicy.Gang.MinCount)
	assert.Equal(t, "high-priority", workload.Spec.PodGroupTemplates[0].PriorityClassName)
	assert.Equal(t, workerWorkloadTemplateName, workload.Spec.PodGroupTemplates[1].Name)
	assert.Equal(t, int32(2), workload.Spec.PodGroupTemplates[1].SchedulingPolicy.Gang.MinCount)
	assert.Equal(t, "high-priority", workload.Spec.PodGroupTemplates[1].PriorityClassName)

	require.NoError(t, provider.CreatePodGroupIfNotExists(ctx, lws, ordinalLeaderPod(lws, "0")))
	for role, name := range map[string]string{
		leaderWorkloadTemplateName: KubernetesRolePodGroupName(lws, "0", leaderWorkloadTemplateName, "revision-1"),
		workerWorkloadTemplateName: KubernetesRolePodGroupName(lws, "0", workerWorkloadTemplateName, "revision-1"),
	} {
		group := &schedulingv1beta1.PodGroup{}
		require.NoError(t, fakeClient.Get(ctx, types.NamespacedName{Namespace: lws.Namespace, Name: name}, group))
		assert.Equal(t, role, group.Spec.WorkloadRef.TemplateName)
		assert.Equal(t, role, group.Labels[PodGroupRoleLabelKey])
		assert.Equal(t, string(SchedulingModeRole), group.Labels[SchedulingLevelLabelKey])
	}
}

// TestKubernetesProviderRecreatedOrdinalLeaderGetsFreshPodGroups covers a
// leader that the statefulset controller recreates with the same name, group
// index and revision while cleanup is deleting the PodGroups of its
// predecessor. The PodGroup protection finalizer holds those PodGroups while
// any pod references them, so the new leader must not wait on them.
func TestKubernetesProviderRecreatedOrdinalLeaderGetsFreshPodGroups(t *testing.T) {
	tests := map[string]struct {
		mutate     func(*leaderworkerset.LeaderWorkerSet)
		wantGroups int
	}{
		"replica mode": {wantGroups: 1},
		"role mode": {
			mutate: func(lws *leaderworkerset.LeaderWorkerSet) {
				lws.Spec.LeaderWorkerTemplate.LeaderTemplate = &corev1.PodTemplateSpec{Spec: corev1.PodSpec{PriorityClassName: "high-priority"}}
				lws.Spec.Scheduling = &leaderworkerset.LeaderWorkerSetScheduling{
					Replica: &leaderworkerset.LeaderWorkerSetReplicaScheduling{
						Leader: &leaderworkerset.LeaderWorkerSetLeaderScheduling{
							SchedulingPolicy: &schedulingv1alpha3.WorkloadPodGroupSchedulingPolicy{Gang: &schedulingv1alpha3.WorkloadPodGroupGangSchedulingPolicy{}},
						},
						Worker: &leaderworkerset.LeaderWorkerSetWorkerScheduling{
							SchedulingPolicy: &schedulingv1alpha3.WorkloadPodGroupSchedulingPolicy{Gang: &schedulingv1alpha3.WorkloadPodGroupGangSchedulingPolicy{}},
						},
					},
				}
			},
			wantGroups: 2,
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			lws := testScheduledLWS()
			if tc.mutate != nil {
				tc.mutate(lws)
			}
			fakeClient := newKubernetesFakeClientBuilder().Build()
			provider := NewKubernetesProvider(fakeClient)
			require.NoError(t, provider.ReconcileScheduling(ctx, lws, 1, "revision-1"))

			// admitLeader does what the pod webhook and the pod controller do for
			// a new leader pod of group 0 and returns the names of its PodGroups.
			admitLeader := func() (*corev1.Pod, []string) {
				leader := ordinalLeaderPod(lws, "0")
				leader.Annotations = map[string]string{
					WorkloadSchedulingAnnotationKey: WorkloadSchedulingValue(lws),
					WorkloadNameAnnotationKey:       KubernetesWorkloadName(lws),
				}
				leader.Spec.SchedulingGates = []corev1.PodSchedulingGate{{Name: leaderworkerset.GroupReplacementSchedulingGate}}
				require.NoError(t, provider.InjectPodGroupMetadata(leader))
				require.NoError(t, fakeClient.Create(ctx, leader))
				require.NoError(t, provider.CreatePodGroupIfNotExists(ctx, lws, leader))
				groups, err := leaderPodGroups(lws, leader)
				require.NoError(t, err)
				require.Len(t, groups, tc.wantGroups)
				names := make([]string, 0, len(groups))
				for _, group := range groups {
					names = append(names, group.name)
				}
				assert.Contains(t, names, *leader.Spec.SchedulingGroup.PodGroupName)
				return leader, names
			}
			getGroup := func(name string) *schedulingv1beta1.PodGroup {
				group := &schedulingv1beta1.PodGroup{}
				require.NoError(t, fakeClient.Get(ctx, types.NamespacedName{Namespace: lws.Namespace, Name: name}, group))
				return group
			}

			first, firstGroups := admitLeader()
			// Stand in for the PodGroup protection finalizer.
			for _, name := range firstGroups {
				group := getGroup(name)
				group.Finalizers = []string{"test.scheduling.k8s.io/protection"}
				require.NoError(t, fakeClient.Update(ctx, group))
			}
			require.NoError(t, fakeClient.Delete(ctx, first))
			require.NoError(t, provider.ReconcileScheduling(ctx, lws, 1, "revision-1"))
			for _, name := range firstGroups {
				assert.False(t, getGroup(name).DeletionTimestamp.IsZero(), "cleanup deletes the PodGroups of a deleted leader")
			}

			second, secondGroups := admitLeader()
			assert.Equal(t, first.Name, second.Name)
			for _, name := range secondGroups {
				assert.NotContains(t, firstGroups, name)
				assert.True(t, getGroup(name).DeletionTimestamp.IsZero())
			}

			// Cleanup keeps the PodGroups of the new leader, including the role
			// mode worker PodGroup that no pod references yet.
			require.NoError(t, provider.ReconcileScheduling(ctx, lws, 1, "revision-1"))
			for _, name := range secondGroups {
				assert.True(t, getGroup(name).DeletionTimestamp.IsZero())
			}

			// Once the finalizer is released only the new leader's PodGroups remain.
			for _, name := range firstGroups {
				group := getGroup(name)
				group.Finalizers = nil
				require.NoError(t, fakeClient.Update(ctx, group))
			}
			groups := &schedulingv1beta1.PodGroupList{}
			require.NoError(t, fakeClient.List(ctx, groups, client.InNamespace(lws.Namespace)))
			remaining := make([]string, 0, len(groups.Items))
			for _, group := range groups.Items {
				remaining = append(remaining, group.Name)
			}
			assert.ElementsMatch(t, secondGroups, remaining)
		})
	}
}

func TestGroupWorkloadName(t *testing.T) {
	lws := testScheduledLWS()
	workloadName := KubernetesWorkloadName(lws)
	other := testScheduledLWS()
	other.UID = types.UID("other-lws-uid")
	tests := map[string]struct {
		annotations map[string]string
		want        string
	}{
		"leader with a group incarnation": {
			annotations: map[string]string{WorkloadNameAnnotationKey: workloadName + ".x7k2p"},
			want:        workloadName + ".x7k2p",
		},
		"leader admitted before group incarnations were introduced": {
			annotations: map[string]string{WorkloadNameAnnotationKey: workloadName},
			want:        workloadName,
		},
		"leader without a workload name": {
			want: workloadName,
		},
		"group incarnation of another LeaderWorkerSet": {
			annotations: map[string]string{WorkloadNameAnnotationKey: KubernetesWorkloadName(other) + ".x7k2p"},
			want:        workloadName,
		},
		"malformed group incarnation": {
			annotations: map[string]string{WorkloadNameAnnotationKey: workloadName + ".x7k2p.b"},
			want:        workloadName,
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			leader := ordinalLeaderPod(lws, "0")
			leader.Annotations = tc.annotations
			assert.Equal(t, tc.want, GroupWorkloadName(lws, leader))
		})
	}
}

// TestKubernetesProviderGroupPodGroupNamesAgree checks that the pod webhook and
// the pod controller agree on the PodGroups of an Ordinal group. Every LWS
// version admits workers the same way: their PodGroup names derive from the
// workload name on the worker statefulset template, which is the one of their
// leader, see GroupWorkloadName.
func TestKubernetesProviderGroupPodGroupNamesAgree(t *testing.T) {
	tests := map[string]struct {
		mutate       func(*leaderworkerset.LeaderWorkerSet)
		leaderSuffix string
		workerSuffix string
	}{
		"replica mode": {leaderSuffix: "-0-revision-1", workerSuffix: "-0-revision-1"},
		"role mode": {
			mutate: func(lws *leaderworkerset.LeaderWorkerSet) {
				lws.Spec.Scheduling = &leaderworkerset.LeaderWorkerSetScheduling{
					Replica: &leaderworkerset.LeaderWorkerSetReplicaScheduling{
						Leader: &leaderworkerset.LeaderWorkerSetLeaderScheduling{},
					},
				}
			},
			leaderSuffix: "-0-leader-revision-1",
			workerSuffix: "-0-worker-revision-1",
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			lws := testScheduledLWS()
			if tc.mutate != nil {
				tc.mutate(lws)
			}
			provider := NewKubernetesProvider(nil)
			workloadName := KubernetesWorkloadName(lws)
			// admit admits a pod of group 0 whose template carries
			// templateWorkloadName and returns its PodGroup name.
			admit := func(pod *corev1.Pod, workerIndex, templateWorkloadName string) string {
				pod.Labels[leaderworkerset.WorkerIndexLabelKey] = workerIndex
				pod.Annotations = map[string]string{
					WorkloadSchedulingAnnotationKey: WorkloadSchedulingValue(lws),
					WorkloadNameAnnotationKey:       templateWorkloadName,
				}
				require.NoError(t, provider.InjectPodGroupMetadata(pod))
				require.NotNil(t, pod.Spec.SchedulingGroup)
				return ptr.Deref(pod.Spec.SchedulingGroup.PodGroupName, "")
			}

			leader := ordinalLeaderPod(lws, "0")
			leaderGroup := admit(leader, "0", workloadName)
			groupWorkloadName := GroupWorkloadName(lws, leader)
			incarnation, found := strings.CutPrefix(groupWorkloadName, workloadName+".")
			require.True(t, found, "leader %s has no group incarnation", groupWorkloadName)
			require.True(t, isGroupIncarnation(incarnation))
			assert.Equal(t, workloadName+"."+incarnation+tc.leaderSuffix, leaderGroup)

			workerGroup := admit(ordinalLeaderPod(lws, "0"), "1", groupWorkloadName)
			assert.Equal(t, workloadName+"."+incarnation+tc.workerSuffix, workerGroup)

			groups, err := leaderPodGroups(lws, leader)
			require.NoError(t, err)
			names := make([]string, 0, len(groups))
			for _, group := range groups {
				names = append(names, group.name)
			}
			want := []string{leaderGroup}
			if workerGroup != leaderGroup {
				want = append(want, workerGroup)
			}
			assert.ElementsMatch(t, want, names)
		})
	}
}

// TestKubernetesProviderKeepsPodGroupsOfLeadersAdmittedBeforeIncarnations
// checks that an LWS upgrade leaves existing groups alone: a leader pod
// admitted by a previous LWS version keeps its PodGroups, named after the plain
// workload name, next to those of a leader pod with a group incarnation.
func TestKubernetesProviderKeepsPodGroupsOfLeadersAdmittedBeforeIncarnations(t *testing.T) {
	ctx := context.Background()
	lws := testScheduledLWS()
	lws.Spec.LeaderWorkerTemplate.LeaderTemplate = &corev1.PodTemplateSpec{Spec: corev1.PodSpec{PriorityClassName: "high-priority"}}
	lws.Spec.Scheduling = &leaderworkerset.LeaderWorkerSetScheduling{
		Replica: &leaderworkerset.LeaderWorkerSetReplicaScheduling{
			Leader: &leaderworkerset.LeaderWorkerSetLeaderScheduling{},
		},
	}
	fakeClient := newKubernetesFakeClientBuilder().Build()
	provider := NewKubernetesProvider(fakeClient)
	require.NoError(t, provider.ReconcileScheduling(ctx, lws, 2, "revision-1"))
	workloadName := KubernetesWorkloadName(lws)
	templateAnnotations := func() map[string]string {
		return map[string]string{
			WorkloadSchedulingAnnotationKey: WorkloadSchedulingValue(lws),
			WorkloadNameAnnotationKey:       workloadName,
		}
	}

	// A previous LWS version admitted the leader of group 0.
	legacy := ordinalLeaderPod(lws, "0")
	legacy.Annotations = templateAnnotations()
	legacy.Spec.SchedulingGroup = &corev1.PodSchedulingGroup{
		PodGroupName: ptr.To(KubernetesRolePodGroupName(lws, "0", leaderWorkloadTemplateName, "revision-1")),
	}
	// This version admits the leader of group 1.
	leader := ordinalLeaderPod(lws, "1")
	leader.Annotations = templateAnnotations()
	require.NoError(t, provider.InjectPodGroupMetadata(leader))
	incarnated := GroupWorkloadName(lws, leader)
	require.NotEqual(t, workloadName, incarnated)
	for _, pod := range []*corev1.Pod{legacy, leader} {
		require.NoError(t, fakeClient.Create(ctx, pod))
		require.NoError(t, provider.CreatePodGroupIfNotExists(ctx, lws, pod))
	}

	// Cleanup keeps them all, including the worker PodGroups that no pod
	// references yet.
	require.NoError(t, provider.ReconcileScheduling(ctx, lws, 2, "revision-1"))
	groups := &schedulingv1beta1.PodGroupList{}
	require.NoError(t, fakeClient.List(ctx, groups, client.InNamespace(lws.Namespace)))
	names := make([]string, 0, len(groups.Items))
	for _, group := range groups.Items {
		assert.True(t, group.DeletionTimestamp.IsZero(), group.Name)
		names = append(names, group.Name)
	}
	assert.ElementsMatch(t, []string{
		KubernetesRolePodGroupName(lws, "0", leaderWorkloadTemplateName, "revision-1"),
		KubernetesRolePodGroupName(lws, "0", workerWorkloadTemplateName, "revision-1"),
		kubernetesRuntimeName(incarnated, "1", leaderWorkloadTemplateName, "revision-1"),
		kubernetesRuntimeName(incarnated, "1", workerWorkloadTemplateName, "revision-1"),
	}, names)
}

func TestBuildFlatWorkloadSynthesizesOmittedRoleAsBasic(t *testing.T) {
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

	workload, err := buildFlatWorkload(lws)
	require.NoError(t, err)
	require.Len(t, workload.Spec.PodGroupTemplates, 2)
	require.NotNil(t, workload.Spec.PodGroupTemplates[0].SchedulingPolicy.Basic)
	require.NotNil(t, workload.Spec.PodGroupTemplates[1].SchedulingPolicy.Gang)
	assert.Equal(t, int32(2), workload.Spec.PodGroupTemplates[1].SchedulingPolicy.Gang.MinCount)
}

func TestPhaseOnePolicyDefaults(t *testing.T) {
	tests := map[string]struct {
		configure func(*leaderworkerset.LeaderWorkerSet)
		wantNames []string
		wantGang  []int32
	}{
		"empty scheduling defaults replica gang": {
			wantNames: []string{replicaWorkloadTemplateName},
			wantGang:  []int32{3},
		},
		"explicit empty replica defaults gang": {
			configure: func(lws *leaderworkerset.LeaderWorkerSet) {
				lws.Spec.Scheduling.Replica = &leaderworkerset.LeaderWorkerSetReplicaScheduling{}
			},
			wantNames: []string{replicaWorkloadTemplateName},
			wantGang:  []int32{3},
		},
		"whole LWS omitted policy defaults basic": {
			configure: func(lws *leaderworkerset.LeaderWorkerSet) {
				lws.Spec.Scheduling.SchedulingConstraints = &schedulingv1alpha3.WorkloadCompositePodGroupSchedulingConstraints{}
			},
			wantNames: []string{lwsWorkloadTemplateName},
			wantGang:  []int32{0},
		},
		"role omitted policies default basic": {
			configure: func(lws *leaderworkerset.LeaderWorkerSet) {
				lws.Spec.Scheduling.Replica = &leaderworkerset.LeaderWorkerSetReplicaScheduling{
					Leader: &leaderworkerset.LeaderWorkerSetLeaderScheduling{},
				}
			},
			wantNames: []string{leaderWorkloadTemplateName, workerWorkloadTemplateName},
			wantGang:  []int32{0, 0},
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			lws := testScheduledLWS()
			if tc.configure != nil {
				tc.configure(lws)
			}
			workload, err := buildFlatWorkload(lws)
			require.NoError(t, err)
			require.Len(t, workload.Spec.PodGroupTemplates, len(tc.wantNames))
			for i := range tc.wantNames {
				template := workload.Spec.PodGroupTemplates[i]
				assert.Equal(t, tc.wantNames[i], template.Name)
				if tc.wantGang[i] == 0 {
					require.NotNil(t, template.SchedulingPolicy.Basic)
				} else {
					require.NotNil(t, template.SchedulingPolicy.Gang)
					assert.Equal(t, tc.wantGang[i], template.SchedulingPolicy.Gang.MinCount)
				}
			}
		})
	}
}

func TestKubernetesProviderDoesNotCreatePodGroupsWhenWorkloadCreationFails(t *testing.T) {
	ctx := context.Background()
	lws := testScheduledLWS()
	createErr := errors.New("injected Workload create failure")
	podGroupCreated := false
	fakeClient := newKubernetesFakeClientBuilder().WithInterceptorFuncs(interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			switch obj.(type) {
			case *schedulingv1beta1.Workload:
				return createErr
			case *schedulingv1beta1.PodGroup:
				podGroupCreated = true
			}
			return c.Create(ctx, obj, opts...)
		},
	}).Build()

	err := NewKubernetesProvider(fakeClient).ReconcileScheduling(ctx, lws, 2, "revision-1")
	require.ErrorIs(t, err, createErr)
	assert.False(t, podGroupCreated, "PodGroup must not be created before its Workload")
}

func TestKubernetesProviderIdempotentReconcileDoesNotWrite(t *testing.T) {
	ctx := context.Background()
	lws := testScheduledLWS()
	writes := 0
	workloadLists := 0
	fakeClient := newKubernetesFakeClientBuilder().WithInterceptorFuncs(interceptor.Funcs{
		List: func(ctx context.Context, c client.WithWatch, obj client.ObjectList, opts ...client.ListOption) error {
			if _, ok := obj.(*schedulingv1beta1.WorkloadList); ok {
				workloadLists++
			}
			return c.List(ctx, obj, opts...)
		},
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			writes++
			return c.Create(ctx, obj, opts...)
		},
		Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
			writes++
			return c.Update(ctx, obj, opts...)
		},
		Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			writes++
			return c.Delete(ctx, obj, opts...)
		},
	}).Build()
	provider := NewKubernetesProvider(fakeClient)

	require.NoError(t, provider.ReconcileScheduling(ctx, lws, 2, "revision-1"))
	require.Positive(t, writes)
	assert.Equal(t, 1, workloadLists, "ownership discovery uses one UID-indexed Workload list")
	writes = 0
	workloadLists = 0

	require.NoError(t, provider.ReconcileScheduling(ctx, lws, 2, "revision-1"))
	assert.Zero(t, writes, "an unchanged reconciliation must not write scheduling resources")
	assert.Equal(t, 1, workloadLists)
}

func TestUpdateMutablePodGroupFieldsAcceptsAPIDefaults(t *testing.T) {
	lws := testScheduledLWS()
	desired := &schedulingv1beta1.PodGroup{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "test-group",
			Namespace:       lws.Namespace,
			OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(lws, leaderworkerset.GroupVersion.WithKind("LeaderWorkerSet"))},
		},
		Spec: schedulingv1beta1.PodGroupSpec{
			SchedulingPolicy: schedulingv1beta1.PodGroupSchedulingPolicy{
				Gang: &schedulingv1beta1.GangSchedulingPolicy{MinCount: 3},
			},
		},
	}
	current := desired.DeepCopy()
	current.Spec.DisruptionMode = &schedulingv1beta1.DisruptionMode{Single: &schedulingv1beta1.SingleDisruptionMode{}}
	current.Spec.Priority = ptr.To[int32](0)
	current.Spec.PreemptionPolicy = ptr.To(schedulingv1beta1.PreemptLowerPriority)

	require.NoError(t, updateMutablePodGroupFields(context.Background(), newKubernetesFakeClientBuilder().Build(), current, desired, false))
}

func TestUpdateMutablePodGroupFieldsAddsWorkloadOwnerRef(t *testing.T) {
	lws := testScheduledLWS()
	lwsOwner := *metav1.NewControllerRef(lws, leaderworkerset.GroupVersion.WithKind("LeaderWorkerSet"))
	workloadOwner := metav1.OwnerReference{
		APIVersion: schedulingv1beta1.SchemeGroupVersion.String(),
		Kind:       "Workload",
		Name:       "test-workload",
		UID:        types.UID("workload-uid"),
		Controller: ptr.To(false),
	}
	desired := &schedulingv1beta1.PodGroup{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "test-group",
			Namespace:       lws.Namespace,
			OwnerReferences: []metav1.OwnerReference{lwsOwner, workloadOwner},
			Labels:          map[string]string{leaderworkerset.SetNameLabelKey: lws.Name},
		},
		Spec: schedulingv1beta1.PodGroupSpec{
			SchedulingPolicy: schedulingv1beta1.PodGroupSchedulingPolicy{
				Gang: &schedulingv1beta1.GangSchedulingPolicy{MinCount: 3},
			},
		},
	}
	current := desired.DeepCopy()
	current.OwnerReferences = []metav1.OwnerReference{lwsOwner}
	current.Spec.DisruptionMode = &schedulingv1beta1.DisruptionMode{Single: &schedulingv1beta1.SingleDisruptionMode{}}
	current.Spec.Priority = ptr.To[int32](0)
	current.Spec.PreemptionPolicy = ptr.To(schedulingv1beta1.PreemptLowerPriority)

	fakeClient := newKubernetesFakeClientBuilder().WithObjects(current.DeepCopy()).Build()
	stored := &schedulingv1beta1.PodGroup{}
	require.NoError(t, fakeClient.Get(context.Background(), client.ObjectKeyFromObject(current), stored))
	require.NoError(t, updateMutablePodGroupFields(context.Background(), fakeClient, stored, desired, false))
	require.NotNil(t, workloadOwnerReference(stored))
	assert.Equal(t, workloadOwner.UID, workloadOwnerReference(stored).UID)

	updated := &schedulingv1beta1.PodGroup{}
	require.NoError(t, fakeClient.Get(context.Background(), client.ObjectKeyFromObject(current), updated))
	require.NotNil(t, workloadOwnerReference(updated))
	assert.Equal(t, workloadOwner.UID, workloadOwnerReference(updated).UID)
}

func TestCleanupUnusedPodGroupsRetainsGroupsReferencedByPods(t *testing.T) {
	ctx := context.Background()
	lws := testScheduledLWS()
	labels := map[string]string{leaderworkerset.SetNameLabelKey: lws.Name}
	owner := *metav1.NewControllerRef(lws, leaderworkerset.GroupVersion.WithKind("LeaderWorkerSet"))
	used := &schedulingv1beta1.PodGroup{ObjectMeta: metav1.ObjectMeta{Name: "used", Namespace: lws.Namespace, Labels: labels, OwnerReferences: []metav1.OwnerReference{owner}}}
	unused := &schedulingv1beta1.PodGroup{ObjectMeta: metav1.ObjectMeta{Name: "unused", Namespace: lws.Namespace, Labels: labels, OwnerReferences: []metav1.OwnerReference{owner}}}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "member", Namespace: lws.Namespace, Labels: labels},
		Spec:       corev1.PodSpec{SchedulingGroup: &corev1.PodSchedulingGroup{PodGroupName: ptr.To("used")}},
	}
	fakeClient := newKubernetesFakeClientBuilder().WithObjects(used, unused, pod).Build()

	require.NoError(t, NewKubernetesProvider(fakeClient).cleanupUnusedPodGroups(ctx, lws, nil))
	require.NoError(t, fakeClient.Get(ctx, client.ObjectKeyFromObject(used), &schedulingv1beta1.PodGroup{}))
	err := fakeClient.Get(ctx, client.ObjectKeyFromObject(unused), &schedulingv1beta1.PodGroup{})
	assert.True(t, apierrors.IsNotFound(err))
}

func TestKubernetesProviderInjectPodGroupMetadata(t *testing.T) {
	tests := map[string]struct {
		mode        SchedulingMode
		workerIndex string
		want        string
		// wantIncarnation means the workload name in want is followed by the
		// group incarnation.
		wantIncarnation bool
	}{
		"whole LWS":      {mode: SchedulingModeLWS, want: "test-lws-lws"},
		"replica":        {mode: SchedulingModeReplica, want: "test-lws-4-revision-1"},
		"replica leader": {mode: SchedulingModeReplica, workerIndex: "0", want: "test-lws-4-revision-1", wantIncarnation: true},
		"leader":         {mode: SchedulingModeRole, workerIndex: "0", want: "test-lws-4-leader-revision-1", wantIncarnation: true},
		"worker":         {mode: SchedulingModeRole, workerIndex: "2", want: "test-lws-4-worker-revision-1"},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			workloadName := "test-lws-uidhash"
			pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
				Annotations: map[string]string{
					WorkloadSchedulingAnnotationKey: string(tc.mode),
					WorkloadNameAnnotationKey:       workloadName,
				},
				Labels: map[string]string{
					leaderworkerset.SetNameLabelKey:     "test-lws",
					leaderworkerset.GroupIndexLabelKey:  "4",
					leaderworkerset.WorkerIndexLabelKey: tc.workerIndex,
					leaderworkerset.RevisionKey:         "revision-1",
				},
			}}

			require.NoError(t, NewKubernetesProvider(nil).InjectPodGroupMetadata(pod))
			require.NotNil(t, pod.Spec.SchedulingGroup)
			require.NotNil(t, pod.Spec.SchedulingGroup.PodGroupName)
			if tc.wantIncarnation {
				incarnation, found := strings.CutPrefix(pod.Annotations[WorkloadNameAnnotationKey], workloadName+".")
				require.True(t, found)
				require.True(t, isGroupIncarnation(incarnation))
				workloadName += "." + incarnation
			} else {
				assert.Equal(t, workloadName, pod.Annotations[WorkloadNameAnnotationKey])
			}
			assert.Equal(t, strings.Replace(tc.want, "test-lws", workloadName, 1), *pod.Spec.SchedulingGroup.PodGroupName)
		})
	}
}

func TestKubernetesProviderDelegatedWorkload(t *testing.T) {
	ctx := context.Background()
	controller := true
	lws := testScheduledLWS()
	parentOwner := metav1.OwnerReference{
		APIVersion: "example.test/v1",
		Kind:       "ParentJob",
		Name:       "parent",
		UID:        types.UID("parent-uid"),
		Controller: &controller,
	}
	lws.OwnerReferences = []metav1.OwnerReference{parentOwner}
	lws.Annotations = map[string]string{
		GroupTemplateNameAnnotation:       "child-template",
		ParentCompositePodGroupAnnotation: "parent-group",
	}
	workload := &schedulingv1beta1.Workload{
		ObjectMeta: metav1.ObjectMeta{Name: "parent-workload", Namespace: lws.Namespace, OwnerReferences: []metav1.OwnerReference{parentOwner}},
		Spec: schedulingv1beta1.WorkloadSpec{
			ControllerRef: &schedulingv1beta1.TypedLocalObjectReference{APIGroup: "example.test", Kind: "ParentJob", Name: "parent"},
			PodGroupTemplates: []schedulingv1beta1.PodGroupTemplate{{
				Name: "child-template",
				// The parent owns the minimum, which need not match the LWS size.
				SchedulingPolicy: schedulingv1beta1.PodGroupSchedulingPolicy{
					Gang: &schedulingv1beta1.GangSchedulingPolicy{MinCount: 2},
				},
			}},
		},
	}
	staleOwner := parentOwner
	staleOwner.UID = types.UID("stale-parent-uid")
	staleWorkload := workload.DeepCopy()
	staleWorkload.Name = "stale-parent-workload"
	staleWorkload.OwnerReferences = []metav1.OwnerReference{staleOwner}
	parentGets := 0
	workloadLists := 0
	fakeClient := newKubernetesFakeClientBuilder().WithObjects(workload, staleWorkload).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if _, ok := obj.(*unstructured.Unstructured); ok {
					parentGets++
				}
				if _, ok := obj.(*schedulingv1alpha3.CompositePodGroup); ok {
					parentGets++
				}
				return c.Get(ctx, key, obj, opts...)
			},
			List: func(ctx context.Context, c client.WithWatch, obj client.ObjectList, opts ...client.ListOption) error {
				if _, ok := obj.(*schedulingv1beta1.WorkloadList); ok {
					workloadLists++
				}
				return c.List(ctx, obj, opts...)
			},
		}).Build()

	provider := NewKubernetesProvider(fakeClient)
	require.NoError(t, provider.ReconcileScheduling(ctx, lws, 4, "revision-1"))
	assert.Equal(t, 1, workloadLists, "each owner level uses one UID-indexed Workload lookup")
	assert.Equal(t, 0, parentGets, "must not GET the third-party parent or a CompositePodGroup after the Workload is found")
	for i := 0; i < 4; i++ {
		require.NoError(t, provider.CreatePodGroupIfNotExists(ctx, lws, ordinalLeaderPod(lws, fmt.Sprint(i))))
	}
	assert.Equal(t, 0, parentGets, "must not GET the third-party parent or a CompositePodGroup after the Workload is found")
	group := &schedulingv1beta1.PodGroup{}
	require.NoError(t, fakeClient.Get(ctx, types.NamespacedName{Namespace: lws.Namespace, Name: KubernetesPodGroupName(lws, "0", "revision-1")}, group))
	require.NotNil(t, group.Spec.WorkloadRef)
	assert.Equal(t, "parent-workload", group.Spec.WorkloadRef.WorkloadName)
	assert.Equal(t, "child-template", group.Spec.WorkloadRef.TemplateName)
	require.NotNil(t, group.Spec.SchedulingPolicy.Gang)
	assert.Equal(t, int32(2), group.Spec.SchedulingPolicy.Gang.MinCount, "delegated groups keep the parent template minimum")
	assert.Nil(t, group.Spec.ParentCompositePodGroupName, "Phase 1 does not attach a parent CompositePodGroup")
	assert.Nil(t, workloadOwnerReference(group), "delegated groups must not own a parent Workload")
	lwsController := metav1.GetControllerOf(group)
	require.NotNil(t, lwsController)
	assert.Equal(t, "LeaderWorkerSet", lwsController.Kind)
	groups := &schedulingv1beta1.PodGroupList{}
	require.NoError(t, fakeClient.List(ctx, groups, client.InNamespace(lws.Namespace)))
	assert.Len(t, groups.Items, 4)

	rootWorkload := &schedulingv1beta1.Workload{}
	err := fakeClient.Get(ctx, client.ObjectKeyFromObject(lws), rootWorkload)
	assert.True(t, apierrors.IsNotFound(err), "a delegated LWS must not create a second Workload")
}

func TestKubernetesProviderDelegatedWorkloadRejectsAmbiguousParent(t *testing.T) {
	ctx := context.Background()
	controller := true
	owner := metav1.OwnerReference{
		APIVersion: "example.test/v1",
		Kind:       "ParentJob",
		Name:       "parent",
		UID:        types.UID("parent-uid"),
		Controller: &controller,
	}
	lws := testScheduledLWS()
	lws.OwnerReferences = []metav1.OwnerReference{owner}
	lws.Annotations = map[string]string{GroupTemplateNameAnnotation: "child-template"}
	parent := &unstructured.Unstructured{}
	parent.SetGroupVersionKind(schema.GroupVersionKind{Group: "example.test", Version: "v1", Kind: "ParentJob"})
	parent.SetName(owner.Name)
	parent.SetNamespace(lws.Namespace)
	parent.SetUID(owner.UID)
	workload := func(name string) *schedulingv1beta1.Workload {
		return &schedulingv1beta1.Workload{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: lws.Namespace, OwnerReferences: []metav1.OwnerReference{owner}},
			Spec: schedulingv1beta1.WorkloadSpec{ControllerRef: &schedulingv1beta1.TypedLocalObjectReference{
				APIGroup: "example.test", Kind: owner.Kind, Name: owner.Name,
			}},
		}
	}
	fakeClient := newKubernetesFakeClientBuilder().
		WithObjects(parent, workload("parent-workload-a"), workload("parent-workload-b")).
		Build()

	_, err := NewKubernetesProvider(fakeClient).findDelegatedWorkload(ctx, lws)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "multiple parent Workloads")
}

// A size change creates a new revision and moves the Workload templates to the
// new size. A group still on the old revision keeps PodGroups with the gang
// minimum of its old size: an existing one is not reported as drift, and a
// recreated one is neither unschedulable (size increase) nor a partial gang
// (size decrease).
func TestKubernetesProviderSizeChangeKeepsGroupGangMinimum(t *testing.T) {
	gang := func() *schedulingv1alpha3.WorkloadPodGroupSchedulingPolicy {
		return &schedulingv1alpha3.WorkloadPodGroupSchedulingPolicy{Gang: &schedulingv1alpha3.WorkloadPodGroupGangSchedulingPolicy{}}
	}
	modes := map[string]struct {
		scheduling func() *leaderworkerset.LeaderWorkerSetScheduling
		// minimums returns the gang minimum of each PodGroup of a group.
		minimums func(lws *leaderworkerset.LeaderWorkerSet, groupIndex, revision string, size int32) map[string]int32
	}{
		"replica": {
			scheduling: func() *leaderworkerset.LeaderWorkerSetScheduling { return &leaderworkerset.LeaderWorkerSetScheduling{} },
			minimums: func(lws *leaderworkerset.LeaderWorkerSet, groupIndex, revision string, size int32) map[string]int32 {
				return map[string]int32{KubernetesPodGroupName(lws, groupIndex, revision): size}
			},
		},
		"role": {
			scheduling: func() *leaderworkerset.LeaderWorkerSetScheduling {
				return &leaderworkerset.LeaderWorkerSetScheduling{Replica: &leaderworkerset.LeaderWorkerSetReplicaScheduling{
					Leader: &leaderworkerset.LeaderWorkerSetLeaderScheduling{SchedulingPolicy: gang()},
					Worker: &leaderworkerset.LeaderWorkerSetWorkerScheduling{SchedulingPolicy: gang()},
				}}
			},
			minimums: func(lws *leaderworkerset.LeaderWorkerSet, groupIndex, revision string, size int32) map[string]int32 {
				return map[string]int32{
					KubernetesRolePodGroupName(lws, groupIndex, leaderWorkloadTemplateName, revision): 1,
					KubernetesRolePodGroupName(lws, groupIndex, workerWorkloadTemplateName, revision): size - 1,
				}
			},
		},
	}
	identities := map[string]struct {
		groupIdentity      leaderworkerset.GroupIdentityType
		leaderPod          func(*leaderworkerset.LeaderWorkerSet, string) *corev1.Pod
		oldGroup, newGroup string
	}{
		"ordinal": {leaderworkerset.GroupIdentityOrdinal, ordinalLeaderPod, "0", "1"},
		"hash":    {leaderworkerset.GroupIdentityHash, hashLeaderPod, hashGroupKey, "8c9d0e1f2a3b4c5d6e7f8091a2b3c4d5e6f70819"},
	}
	sizeChanges := map[string]struct{ oldSize, newSize int32 }{
		"increase": {oldSize: 3, newSize: 5},
		"decrease": {oldSize: 5, newSize: 3},
	}
	withSize := func(pod *corev1.Pod, size int32) *corev1.Pod {
		if pod.Annotations == nil {
			pod.Annotations = map[string]string{}
		}
		pod.Annotations[leaderworkerset.SizeAnnotationKey] = fmt.Sprint(size)
		return pod
	}

	for modeName, mode := range modes {
		for identityName, identity := range identities {
			for changeName, change := range sizeChanges {
				t.Run(modeName+"/"+identityName+"/"+changeName, func(t *testing.T) {
					ctx := context.Background()
					fakeClient := newKubernetesFakeClientBuilder().Build()
					provider := NewKubernetesProvider(fakeClient)
					// assertMinimums checks the gang minimum of the given
					// PodGroups and returns their resource versions.
					assertMinimums := func(want map[string]int32) map[string]string {
						t.Helper()
						versions := make(map[string]string, len(want))
						for name, minCount := range want {
							podGroup := &schedulingv1beta1.PodGroup{}
							require.NoError(t, fakeClient.Get(ctx, types.NamespacedName{Namespace: "default", Name: name}, podGroup))
							require.NotNil(t, podGroup.Spec.SchedulingPolicy.Gang, name)
							assert.Equal(t, minCount, podGroup.Spec.SchedulingPolicy.Gang.MinCount, name)
							versions[name] = podGroup.ResourceVersion
						}
						return versions
					}

					oldLWS := testScheduledLWS()
					oldLWS.Spec.GroupIdentity = identity.groupIdentity
					oldLWS.Spec.Scheduling = mode.scheduling()
					oldLWS.Spec.LeaderWorkerTemplate.Size = ptr.To(change.oldSize)
					require.NoError(t, provider.ReconcileScheduling(ctx, oldLWS, 2, "revision-1"))
					// The running leader keeps its PodGroups out of cleanup.
					oldLeader := withSize(identity.leaderPod(oldLWS, identity.oldGroup), change.oldSize)
					require.NoError(t, fakeClient.Create(ctx, oldLeader))
					require.NoError(t, provider.CreatePodGroupIfNotExists(ctx, oldLWS, oldLeader))
					oldGroups := mode.minimums(oldLWS, identity.oldGroup, "revision-1", change.oldSize)
					assertMinimums(oldGroups)

					newLWS := oldLWS.DeepCopy()
					newLWS.Spec.LeaderWorkerTemplate.Size = ptr.To(change.newSize)
					require.NoError(t, provider.ReconcileScheduling(ctx, newLWS, 2, "revision-2"))
					existing := assertMinimums(oldGroups)

					// The pod controller reconciles the old leader with its
					// revision applied, which restores oldLWS, or with newLWS
					// when it cannot find the revision.
					require.NoError(t, provider.CreatePodGroupIfNotExists(ctx, oldLWS, oldLeader))
					require.NoError(t, provider.CreatePodGroupIfNotExists(ctx, newLWS, oldLeader))
					unannotated := oldLeader.DeepCopy()
					delete(unannotated.Annotations, leaderworkerset.SizeAnnotationKey)
					require.NoError(t, provider.CreatePodGroupIfNotExists(ctx, oldLWS, unannotated))
					assert.Equal(t, existing, assertMinimums(oldGroups), "PodGroups with the right minimum are not written")

					// Recreated PodGroups of the old revision keep the old minimum.
					for name := range oldGroups {
						require.NoError(t, fakeClient.Delete(ctx, &schedulingv1beta1.PodGroup{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: name}}))
					}
					require.NoError(t, provider.CreatePodGroupIfNotExists(ctx, oldLWS, oldLeader))
					assertMinimums(oldGroups)

					// Groups of the new revision get the new minimum.
					newLeader := withSize(identity.leaderPod(newLWS, identity.newGroup), change.newSize)
					newLeader.Labels[leaderworkerset.RevisionKey] = "revision-2"
					require.NoError(t, provider.CreatePodGroupIfNotExists(ctx, newLWS, newLeader))
					assertMinimums(mode.minimums(newLWS, identity.newGroup, "revision-2", change.newSize))
				})
			}
		}
	}
}

// Earlier versions materialized the PodGroup of a group still on the previous
// revision from the template of the new size. Such a PodGroup gets the gang
// minimum of its own group back, whichever way the size changed, instead of
// failing every reconcile as drift.
func TestKubernetesProviderCorrectsGangMinimumOfExistingPodGroup(t *testing.T) {
	ctx := context.Background()
	lws := testScheduledLWS()
	fakeClient := newKubernetesFakeClientBuilder().Build()
	provider := NewKubernetesProvider(fakeClient)
	require.NoError(t, provider.ReconcileScheduling(ctx, lws, 2, "revision-1"))
	leader := ordinalLeaderPod(lws, "0")
	require.NoError(t, provider.CreatePodGroupIfNotExists(ctx, lws, leader))

	key := types.NamespacedName{Namespace: lws.Namespace, Name: KubernetesPodGroupName(lws, "0", "revision-1")}
	podGroup := &schedulingv1beta1.PodGroup{}
	for _, stale := range []int32{5, 2} {
		require.NoError(t, fakeClient.Get(ctx, key, podGroup))
		podGroup.Spec.SchedulingPolicy.Gang.MinCount = stale
		require.NoError(t, fakeClient.Update(ctx, podGroup))

		require.NoError(t, provider.CreatePodGroupIfNotExists(ctx, lws, leader))
		require.NoError(t, fakeClient.Get(ctx, key, podGroup))
		assert.Equal(t, int32(3), podGroup.Spec.SchedulingPolicy.Gang.MinCount)
	}

	// Only the minimum is corrected; the other scheduling fields stay immutable.
	podGroup.Spec.DisruptionMode = &schedulingv1beta1.DisruptionMode{All: &schedulingv1beta1.AllDisruptionMode{}}
	require.NoError(t, fakeClient.Update(ctx, podGroup))
	err := provider.CreatePodGroupIfNotExists(ctx, lws, leader)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "immutable scheduling configuration drift")
}

func TestWholeLWSMembership(t *testing.T) {
	cases := map[string]struct {
		replicas, size int32
		groupSizes     []int32
		want           int32
	}{
		"no groups yet":                         {replicas: 2, size: 3, want: 6},
		"all groups have the size":              {replicas: 2, size: 3, groupSizes: []int32{3, 3}, want: 6},
		"a group is being recreated":            {replicas: 2, size: 3, groupSizes: []int32{3}, want: 6},
		"scale up":                              {replicas: 3, size: 3, groupSizes: []int32{3, 3}, want: 9},
		"size increase, no group replaced":      {replicas: 2, size: 3, groupSizes: []int32{2, 2}, want: 4},
		"size increase, replacement pending":    {replicas: 2, size: 3, groupSizes: []int32{2}, want: 4},
		"size increase, a group replaced":       {replicas: 2, size: 3, groupSizes: []int32{3, 2}, want: 5},
		"size increase, last old group deleted": {replicas: 2, size: 3, groupSizes: []int32{3}, want: 6},
		"size increase with surge":              {replicas: 2, size: 3, groupSizes: []int32{2, 3, 2}, want: 4},
		"size increase with scale up":           {replicas: 3, size: 3, groupSizes: []int32{3, 2}, want: 7},
		"successive size increases":             {replicas: 3, size: 4, groupSizes: []int32{4, 2, 3}, want: 9},
		"size decrease":                         {replicas: 2, size: 2, groupSizes: []int32{3, 3}, want: 4},
		"size decrease, a group replaced":       {replicas: 2, size: 2, groupSizes: []int32{2, 3}, want: 4},
		"zero replicas":                         {replicas: 0, size: 3, groupSizes: []int32{3}, want: 1},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, tc.want, wholeLWSMembership(tc.replicas, tc.size, tc.groupSizes))
		})
	}
}

// A size increase raises the gang minimum of the Workload template to the new
// replicas * size at once, while the groups of the old size only leave as the
// rollout replaces them. The whole-LWS PodGroup, which all revisions share,
// keeps a minimum that the existing groups can meet, and reaches the template
// minimum once the last group of the old size is gone.
func TestKubernetesProviderWholeLWSSizeIncreaseKeepsGangMinimumReachable(t *testing.T) {
	identities := map[string]struct {
		groupIdentity leaderworkerset.GroupIdentityType
		leaderPod     func(*leaderworkerset.LeaderWorkerSet, string) *corev1.Pod
	}{
		"ordinal": {leaderworkerset.GroupIdentityOrdinal, ordinalLeaderPod},
		"hash": {leaderworkerset.GroupIdentityHash, func(lws *leaderworkerset.LeaderWorkerSet, group string) *corev1.Pod {
			pod := hashLeaderPod(lws, group)
			pod.Name = lws.Name + "-" + group
			return pod
		}},
	}
	for name, identity := range identities {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			fakeClient := newKubernetesFakeClientBuilder().Build()
			provider := NewKubernetesProvider(fakeClient)
			lws := testScheduledLWS()
			lws.Spec.GroupIdentity = identity.groupIdentity
			lws.Spec.Scheduling = &leaderworkerset.LeaderWorkerSetScheduling{
				SchedulingPolicy: &schedulingv1alpha3.WorkloadCompositePodGroupSchedulingPolicy{
					Gang: &schedulingv1alpha3.WorkloadCompositePodGroupGangSchedulingPolicy{},
				},
			}
			lws.Spec.LeaderWorkerTemplate.Size = ptr.To[int32](2)
			podGroupKey := types.NamespacedName{Namespace: lws.Namespace, Name: KubernetesLWSGroupName(lws)}
			// reconcile runs ReconcileScheduling as the LWS controller does and
			// checks the gang minimum of the Workload template and the PodGroup.
			reconcile := func(revision string, templateMinCount, podGroupMinCount int32) {
				t.Helper()
				require.NoError(t, provider.ReconcileScheduling(ctx, lws, 2, revision))
				workload := &schedulingv1beta1.Workload{}
				require.NoError(t, fakeClient.Get(ctx, types.NamespacedName{Namespace: lws.Namespace, Name: KubernetesWorkloadName(lws)}, workload))
				assert.Equal(t, templateMinCount, workload.Spec.PodGroupTemplates[0].SchedulingPolicy.Gang.MinCount, "Workload template")
				podGroup := &schedulingv1beta1.PodGroup{}
				require.NoError(t, fakeClient.Get(ctx, podGroupKey, podGroup))
				assert.Equal(t, podGroupMinCount, podGroup.Spec.SchedulingPolicy.Gang.MinCount, "PodGroup")
			}
			newLeader := func(group, revision string, size int32) *corev1.Pod {
				leader := identity.leaderPod(lws, group)
				leader.Labels[leaderworkerset.RevisionKey] = revision
				if leader.Annotations == nil {
					leader.Annotations = map[string]string{}
				}
				leader.Annotations[leaderworkerset.SizeAnnotationKey] = fmt.Sprint(size)
				// The pod webhook adds every pod to the whole-LWS PodGroup.
				leader.Spec.SchedulingGroup = &corev1.PodSchedulingGroup{PodGroupName: ptr.To(podGroupKey.Name)}
				return leader
			}
			createLeader := func(group, revision string, size int32) *corev1.Pod {
				t.Helper()
				leader := newLeader(group, revision, size)
				require.NoError(t, fakeClient.Create(ctx, leader))
				return leader
			}

			// The leader pods of a deleted LWS of the same name, which can still
			// be terminating, have the same labels but belong to its PodGroup.
			previousLWS := lws.DeepCopy()
			previousLWS.UID = "previous-lws-uid"
			previousLeader := newLeader("9", "revision-0", 1)
			previousLeader.Spec.SchedulingGroup.PodGroupName = ptr.To(KubernetesLWSGroupName(previousLWS))
			require.NoError(t, fakeClient.Create(ctx, previousLeader))

			reconcile("revision-1", 4, 4)
			oldLeader0 := createLeader("0", "revision-1", 2)
			oldLeader1 := createLeader("1", "revision-1", 2)
			reconcile("revision-1", 4, 4)

			// The size increase starts a rollout and moves the template minimum
			// to 6, which the 4 pods of the old groups can never meet.
			lws.Spec.LeaderWorkerTemplate.Size = ptr.To[int32](3)
			reconcile("revision-2", 6, 4)

			// Earlier versions raised the PodGroup minimum with the template,
			// which stalled the rollout. It is lowered again.
			podGroup := &schedulingv1beta1.PodGroup{}
			require.NoError(t, fakeClient.Get(ctx, podGroupKey, podGroup))
			podGroup.Spec.SchedulingPolicy.Gang.MinCount = 6
			require.NoError(t, fakeClient.Update(ctx, podGroup))
			reconcile("revision-2", 6, 4)

			// The rollout replaces group 1 with a group of the new size.
			require.NoError(t, fakeClient.Delete(ctx, oldLeader1))
			reconcile("revision-2", 6, 4)
			createLeader("1", "revision-2", 3)
			reconcile("revision-2", 6, 5)

			// The old group 0 counts until its leader pod is deleted.
			oldLeader0.Finalizers = []string{"test.leaderworkerset.sigs.k8s.io/hold"}
			require.NoError(t, fakeClient.Update(ctx, oldLeader0))
			require.NoError(t, fakeClient.Delete(ctx, oldLeader0))
			reconcile("revision-2", 6, 5)
			require.NoError(t, fakeClient.Get(ctx, client.ObjectKeyFromObject(oldLeader0), oldLeader0))
			oldLeader0.Finalizers = nil
			require.NoError(t, fakeClient.Update(ctx, oldLeader0))
			reconcile("revision-2", 6, 6)
			createLeader("0", "revision-2", 3)
			reconcile("revision-2", 6, 6)

			// A size decrease lowers both minimums at once, as before.
			lws.Spec.LeaderWorkerTemplate.Size = ptr.To[int32](2)
			reconcile("revision-3", 4, 4)
		})
	}
}

func TestValidatePhaseOneWorkloadImmutability(t *testing.T) {
	ctx := context.Background()

	newRoleLWS := func() *leaderworkerset.LeaderWorkerSet {
		lws := testScheduledLWS()
		lws.Spec.LeaderWorkerTemplate.LeaderTemplate = &corev1.PodTemplateSpec{
			Spec: corev1.PodSpec{
				PriorityClassName: "high-priority",
				ResourceClaims: []corev1.PodResourceClaim{
					{Name: "leader-claim", ResourceClaimName: ptr.To("shared-leader-claim")},
					{Name: "leader-claim-2", ResourceClaimName: ptr.To("shared-leader-claim-2")},
				},
				Containers: []corev1.Container{{Name: "leader", Image: "leader:latest"}},
			},
		}
		lws.Spec.LeaderWorkerTemplate.WorkerTemplate.Spec.ResourceClaims = []corev1.PodResourceClaim{
			{Name: "worker-claim", ResourceClaimName: ptr.To("shared-worker-claim")},
		}
		lws.Spec.Scheduling = &leaderworkerset.LeaderWorkerSetScheduling{
			Replica: &leaderworkerset.LeaderWorkerSetReplicaScheduling{
				Leader: &leaderworkerset.LeaderWorkerSetLeaderScheduling{
					ResourceClaims: []schedulingv1alpha3.WorkloadPodGroupResourceClaim{
						{Name: "leader-claim", ResourceClaimName: ptr.To("shared-leader-claim")},
					},
				},
				Worker: &leaderworkerset.LeaderWorkerSetWorkerScheduling{
					SchedulingPolicy: &schedulingv1alpha3.WorkloadPodGroupSchedulingPolicy{
						Gang: &schedulingv1alpha3.WorkloadPodGroupGangSchedulingPolicy{},
					},
				},
			},
		}
		return lws
	}

	t.Run("rejects immutable field mutations across all levels", func(t *testing.T) {
		cases := []struct {
			name      string
			base      func() *leaderworkerset.LeaderWorkerSet
			mutate    func(*leaderworkerset.LeaderWorkerSet)
			wantField string
		}{
			{
				name: "replica default gang -> basic",
				base: testScheduledLWS,
				mutate: func(l *leaderworkerset.LeaderWorkerSet) {
					l.Spec.Scheduling.Replica = &leaderworkerset.LeaderWorkerSetReplicaScheduling{
						SchedulingPolicy: &schedulingv1alpha3.WorkloadCompositePodGroupSchedulingPolicy{
							Basic: &schedulingv1alpha3.WorkloadCompositePodGroupBasicSchedulingPolicy{},
						},
					}
				},
				wantField: "spec.scheduling.replica.schedulingPolicy",
			},
			{
				name: "replica explicit gang -> basic",
				base: func() *leaderworkerset.LeaderWorkerSet {
					l := testScheduledLWS()
					l.Spec.Scheduling.Replica = &leaderworkerset.LeaderWorkerSetReplicaScheduling{
						SchedulingPolicy: &schedulingv1alpha3.WorkloadCompositePodGroupSchedulingPolicy{
							Gang: &schedulingv1alpha3.WorkloadCompositePodGroupGangSchedulingPolicy{},
						},
					}
					return l
				},
				mutate: func(l *leaderworkerset.LeaderWorkerSet) {
					l.Spec.Scheduling.Replica.SchedulingPolicy = &schedulingv1alpha3.WorkloadCompositePodGroupSchedulingPolicy{
						Basic: &schedulingv1alpha3.WorkloadCompositePodGroupBasicSchedulingPolicy{},
					}
				},
				wantField: "spec.scheduling.replica.schedulingPolicy",
			},
			{
				name: "replica default disruption -> all",
				base: testScheduledLWS,
				mutate: func(l *leaderworkerset.LeaderWorkerSet) {
					l.Spec.Scheduling.Replica = &leaderworkerset.LeaderWorkerSetReplicaScheduling{
						DisruptionMode: &schedulingv1alpha3.WorkloadCompositePodGroupDisruptionMode{
							All: &schedulingv1alpha3.WorkloadCompositePodGroupAllDisruptionMode{},
						},
					}
				},
				wantField: "spec.scheduling.replica.disruptionMode",
			},
			{
				name: "replica explicit single -> all",
				base: func() *leaderworkerset.LeaderWorkerSet {
					l := testScheduledLWS()
					l.Spec.Scheduling.Replica = &leaderworkerset.LeaderWorkerSetReplicaScheduling{
						DisruptionMode: &schedulingv1alpha3.WorkloadCompositePodGroupDisruptionMode{
							Single: &schedulingv1alpha3.WorkloadCompositePodGroupSingleDisruptionMode{},
						},
					}
					return l
				},
				mutate: func(l *leaderworkerset.LeaderWorkerSet) {
					l.Spec.Scheduling.Replica.DisruptionMode = &schedulingv1alpha3.WorkloadCompositePodGroupDisruptionMode{
						All: &schedulingv1alpha3.WorkloadCompositePodGroupAllDisruptionMode{},
					}
				},
				wantField: "spec.scheduling.replica.disruptionMode",
			},
			{
				name: "replica schedulingConstraints changed",
				base: func() *leaderworkerset.LeaderWorkerSet {
					l := testScheduledLWS()
					l.Spec.Scheduling.Replica = &leaderworkerset.LeaderWorkerSetReplicaScheduling{
						SchedulingConstraints: &schedulingv1alpha3.WorkloadCompositePodGroupSchedulingConstraints{
							Topology: []schedulingv1alpha3.TopologyConstraint{{Key: "topology.kubernetes.io/zone"}},
						},
					}
					return l
				},
				mutate: func(l *leaderworkerset.LeaderWorkerSet) {
					l.Spec.Scheduling.Replica.SchedulingConstraints = &schedulingv1alpha3.WorkloadCompositePodGroupSchedulingConstraints{
						Topology: []schedulingv1alpha3.TopologyConstraint{{Key: "kubernetes.io/hostname"}},
					}
				},
				wantField: "spec.scheduling.replica.schedulingConstraints",
			},
			{
				name: "whole-LWS gang -> basic",
				base: func() *leaderworkerset.LeaderWorkerSet {
					l := testScheduledLWS()
					l.Spec.Scheduling = &leaderworkerset.LeaderWorkerSetScheduling{
						SchedulingPolicy: &schedulingv1alpha3.WorkloadCompositePodGroupSchedulingPolicy{
							Gang: &schedulingv1alpha3.WorkloadCompositePodGroupGangSchedulingPolicy{},
						},
					}
					return l
				},
				mutate: func(l *leaderworkerset.LeaderWorkerSet) {
					l.Spec.Scheduling.SchedulingPolicy = &schedulingv1alpha3.WorkloadCompositePodGroupSchedulingPolicy{
						Basic: &schedulingv1alpha3.WorkloadCompositePodGroupBasicSchedulingPolicy{},
					}
				},
				wantField: "spec.scheduling.schedulingPolicy",
			},
			{
				name: "role leader resourceClaims changed",
				base: newRoleLWS,
				mutate: func(l *leaderworkerset.LeaderWorkerSet) {
					l.Spec.Scheduling.Replica.Leader.ResourceClaims = []schedulingv1alpha3.WorkloadPodGroupResourceClaim{
						{Name: "leader-claim-2", ResourceClaimName: ptr.To("shared-leader-claim-2")},
					}
				},
				wantField: "spec.scheduling.replica.leader.resourceClaims",
			},
			{
				name: "role worker gang -> basic",
				base: newRoleLWS,
				mutate: func(l *leaderworkerset.LeaderWorkerSet) {
					l.Spec.Scheduling.Replica.Worker.SchedulingPolicy = &schedulingv1alpha3.WorkloadPodGroupSchedulingPolicy{
						Basic: &schedulingv1alpha3.WorkloadPodGroupBasicSchedulingPolicy{},
					}
				},
				wantField: "spec.scheduling.replica.worker.schedulingPolicy",
			},
		}

		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				oldLWS := tc.base()
				require.Empty(t, ValidatePhaseOneWorkload(ctx, nil, oldLWS))

				updated := oldLWS.DeepCopy()
				tc.mutate(updated)
				errs := ValidatePhaseOneWorkload(ctx, oldLWS, updated)
				require.NotEmpty(t, errs)
				assert.Equal(t, tc.wantField, errs[0].Field)
				assert.Contains(t, errs[0].Detail, "immutable after creation")
			})
		}
	})

	t.Run("allows mutable size, replicas, and explicit gang minCount updates", func(t *testing.T) {
		oldReplica := testScheduledLWS()
		newReplica := oldReplica.DeepCopy()
		newReplica.Spec.Replicas = ptr.To[int32](4)
		newReplica.Spec.LeaderWorkerTemplate.Size = ptr.To[int32](5)
		newReplica.Spec.Scheduling.Replica = &leaderworkerset.LeaderWorkerSetReplicaScheduling{
			SchedulingPolicy: &schedulingv1alpha3.WorkloadCompositePodGroupSchedulingPolicy{
				Gang: &schedulingv1alpha3.WorkloadCompositePodGroupGangSchedulingPolicy{},
			},
		}
		assert.Empty(t, ValidatePhaseOneWorkload(ctx, oldReplica, newReplica))

		oldWhole := testScheduledLWS()
		oldWhole.Spec.Scheduling = &leaderworkerset.LeaderWorkerSetScheduling{
			SchedulingPolicy: &schedulingv1alpha3.WorkloadCompositePodGroupSchedulingPolicy{
				Gang: &schedulingv1alpha3.WorkloadCompositePodGroupGangSchedulingPolicy{},
			},
		}
		newWhole := oldWhole.DeepCopy()
		newWhole.Spec.Replicas = ptr.To[int32](3)
		assert.Empty(t, ValidatePhaseOneWorkload(ctx, oldWhole, newWhole))
	})
}

func testScheduledLWS() *leaderworkerset.LeaderWorkerSet {
	return &leaderworkerset.LeaderWorkerSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-lws",
			Namespace: "default",
			UID:       types.UID("test-lws-uid"),
		},
		Spec: leaderworkerset.LeaderWorkerSetSpec{
			Replicas:   ptr.To[int32](2),
			Scheduling: &leaderworkerset.LeaderWorkerSetScheduling{},
			LeaderWorkerTemplate: leaderworkerset.LeaderWorkerTemplate{
				Size: ptr.To[int32](3),
				WorkerTemplate: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
					PriorityClassName: "high-priority",
					Containers:        []corev1.Container{{Name: "worker", Image: "worker:latest"}},
				}},
			},
		},
	}
}

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
		WithIndex(&schedulingv1beta1.Workload{}, workloadControllerUIDIndex, workloadControllerUIDIndexValues)
}

func TestKubernetesProviderReconcileScheduling(t *testing.T) {
	ctx := context.Background()
	lws := testScheduledLWS()
	fakeClient := newKubernetesFakeClientBuilder().Build()

	err := NewKubernetesProvider(fakeClient).ReconcileScheduling(ctx, lws, 2, "revision-1")
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
		podGroup := &schedulingv1beta1.PodGroup{}
		require.NoError(t, fakeClient.Get(ctx, types.NamespacedName{Namespace: lws.Namespace, Name: name}, podGroup))
		require.NotNil(t, podGroup.Spec.WorkloadRef)
		assert.Equal(t, KubernetesWorkloadName(lws), podGroup.Spec.WorkloadRef.WorkloadName)
		assert.Equal(t, replicaWorkloadTemplateName, podGroup.Spec.WorkloadRef.TemplateName)
		assert.Equal(t, int32(3), podGroup.Spec.SchedulingPolicy.Gang.MinCount)
		assert.Equal(t, fmt.Sprint(groupIndex), podGroup.Labels[leaderworkerset.GroupIndexLabelKey])
		assert.Equal(t, "revision-1", podGroup.Labels[leaderworkerset.RevisionKey])
		assert.Equal(t, string(SchedulingModeReplica), podGroup.Labels[SchedulingLevelLabelKey])
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
			ResourceClaims: []schedulingv1alpha3.WorkloadPodGroupResourceClaim{{
				Name:              "gpu",
				ResourceClaimName: ptr.To("shared-gpu"),
			}},
		},
	}
	lws.Spec.LeaderWorkerTemplate.WorkerTemplate.Spec.ResourceClaims = []corev1.PodResourceClaim{{
		Name:              "gpu",
		ResourceClaimName: ptr.To("shared-gpu"),
	}}

	workload, err := buildFlatWorkload(context.Background(), lws)
	require.NoError(t, err)
	require.Len(t, workload.Spec.PodGroupTemplates, 1)
	template := workload.Spec.PodGroupTemplates[0]
	require.NotNil(t, template.SchedulingPolicy.Gang)
	assert.Equal(t, int32(3), template.SchedulingPolicy.Gang.MinCount)
	require.NotNil(t, template.SchedulingConstraints)
	assert.Equal(t, "topology.kubernetes.io/zone", template.SchedulingConstraints.Topology[0].Key)
	require.NotNil(t, template.DisruptionMode)
	require.NotNil(t, template.DisruptionMode.All)
	require.Len(t, template.ResourceClaims, 1)
	assert.Equal(t, "gpu", template.ResourceClaims[0].Name)
	assert.Equal(t, ptr.To("shared-gpu"), template.ResourceClaims[0].ResourceClaimName)
}

func TestBuildFlatWorkloadWholeLWSResourceClaims(t *testing.T) {
	lws := testScheduledLWS()
	lws.Spec.Scheduling = &leaderworkerset.LeaderWorkerSetScheduling{
		SchedulingPolicy: &schedulingv1alpha3.WorkloadCompositePodGroupSchedulingPolicy{
			Basic: &schedulingv1alpha3.WorkloadCompositePodGroupBasicSchedulingPolicy{},
		},
		ResourceClaims: []schedulingv1alpha3.WorkloadPodGroupResourceClaim{{
			Name:                      "gpu",
			ResourceClaimTemplateName: ptr.To("shared-gpu-template"),
		}},
	}
	lws.Spec.LeaderWorkerTemplate.WorkerTemplate.Spec.ResourceClaims = []corev1.PodResourceClaim{{
		Name:                      "gpu",
		ResourceClaimTemplateName: ptr.To("shared-gpu-template"),
	}}

	workload, err := buildFlatWorkload(context.Background(), lws)
	require.NoError(t, err)
	require.Len(t, workload.Spec.PodGroupTemplates, 1)
	require.Len(t, workload.Spec.PodGroupTemplates[0].ResourceClaims, 1)
	assert.Equal(t, "gpu", workload.Spec.PodGroupTemplates[0].ResourceClaims[0].Name)
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
	require.NoError(t, provider.ReconcileScheduling(ctx, newLWS, 1, "revision-1"))
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
	} {
		assert.LessOrEqual(t, len(name), 253)
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

	require.NoError(t, NewKubernetesProvider(fakeClient).ReconcileScheduling(ctx, lws, 1, "revision-1"))
	workload := &schedulingv1beta1.Workload{}
	require.NoError(t, fakeClient.Get(ctx, types.NamespacedName{Namespace: lws.Namespace, Name: KubernetesWorkloadName(lws)}, workload))
	require.Len(t, workload.Spec.PodGroupTemplates, 2)
	assert.Equal(t, leaderWorkloadTemplateName, workload.Spec.PodGroupTemplates[0].Name)
	assert.Equal(t, int32(1), workload.Spec.PodGroupTemplates[0].SchedulingPolicy.Gang.MinCount)
	assert.Equal(t, "high-priority", workload.Spec.PodGroupTemplates[0].PriorityClassName)
	assert.Equal(t, workerWorkloadTemplateName, workload.Spec.PodGroupTemplates[1].Name)
	assert.Equal(t, int32(2), workload.Spec.PodGroupTemplates[1].SchedulingPolicy.Gang.MinCount)
	assert.Equal(t, "high-priority", workload.Spec.PodGroupTemplates[1].PriorityClassName)

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

	workload, err := buildFlatWorkload(context.Background(), lws)
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
			workload, err := buildFlatWorkload(context.Background(), lws)
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
	}{
		"whole LWS": {mode: SchedulingModeLWS, want: "test-lws-lws"},
		"replica":   {mode: SchedulingModeReplica, want: "test-lws-4-revision-1"},
		"leader":    {mode: SchedulingModeRole, workerIndex: "0", want: "test-lws-4-leader-revision-1"},
		"worker":    {mode: SchedulingModeRole, workerIndex: "2", want: "test-lws-4-worker-revision-1"},
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
	parent := &unstructured.Unstructured{}
	parent.SetGroupVersionKind(schema.GroupVersionKind{Group: "example.test", Version: "v1", Kind: "ParentJob"})
	parent.SetName("parent")
	parent.SetNamespace(lws.Namespace)
	parent.SetUID(parentOwner.UID)
	workload := &schedulingv1beta1.Workload{
		ObjectMeta: metav1.ObjectMeta{Name: "parent-workload", Namespace: lws.Namespace, OwnerReferences: []metav1.OwnerReference{parentOwner}},
		Spec: schedulingv1beta1.WorkloadSpec{
			ControllerRef: &schedulingv1beta1.TypedLocalObjectReference{APIGroup: "example.test", Kind: "ParentJob", Name: "parent"},
			PodGroupTemplates: []schedulingv1beta1.PodGroupTemplate{{
				Name: "child-template",
				SchedulingPolicy: schedulingv1beta1.PodGroupSchedulingPolicy{
					Gang: &schedulingv1beta1.GangSchedulingPolicy{MinCount: 3},
				},
			}},
		},
	}
	staleOwner := parentOwner
	staleOwner.UID = types.UID("stale-parent-uid")
	staleWorkload := workload.DeepCopy()
	staleWorkload.Name = "stale-parent-workload"
	staleWorkload.OwnerReferences = []metav1.OwnerReference{staleOwner}
	parentGroup := &schedulingv1alpha3.CompositePodGroup{ObjectMeta: metav1.ObjectMeta{Name: "parent-group", Namespace: lws.Namespace}}
	parentGets := 0
	workloadLists := 0
	fakeClient := newKubernetesFakeClientBuilder().WithObjects(parent, workload, staleWorkload, parentGroup).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
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

	require.NoError(t, NewKubernetesProvider(fakeClient).ReconcileScheduling(ctx, lws, 4, "revision-1"))
	assert.Equal(t, 1, workloadLists, "each owner level uses one UID-indexed Workload lookup")
	assert.Equal(t, 1, parentGets, "the shared parent must be checked once per reconciliation")
	group := &schedulingv1beta1.PodGroup{}
	require.NoError(t, fakeClient.Get(ctx, types.NamespacedName{Namespace: lws.Namespace, Name: KubernetesPodGroupName(lws, "0", "revision-1")}, group))
	require.NotNil(t, group.Spec.WorkloadRef)
	assert.Equal(t, "parent-workload", group.Spec.WorkloadRef.WorkloadName)
	assert.Equal(t, "child-template", group.Spec.WorkloadRef.TemplateName)
	assert.Equal(t, ptr.To("parent-group"), group.Spec.ParentCompositePodGroupName)
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

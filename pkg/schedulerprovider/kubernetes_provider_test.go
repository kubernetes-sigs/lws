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
	"strconv"
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
	"k8s.io/component-helpers/scheduling/schedulingv1/workloadbuilder"
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

func TestKubernetesProviderReconcileScheduling(t *testing.T) {
	ctx := context.Background()
	lws := testScheduledLWS()
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()

	err := NewKubernetesProvider(fakeClient).ReconcileScheduling(ctx, lws, 2, "revision-1")
	require.NoError(t, err)

	workload := &schedulingv1beta1.Workload{}
	require.NoError(t, fakeClient.Get(ctx, client.ObjectKeyFromObject(lws), workload))
	require.Len(t, workload.Spec.PodGroupTemplates, 1)
	assert.Equal(t, workloadTemplateName, workload.Spec.PodGroupTemplates[0].Name)
	require.NotNil(t, workload.Spec.PodGroupTemplates[0].SchedulingPolicy.Gang)
	assert.Equal(t, int32(3), workload.Spec.PodGroupTemplates[0].SchedulingPolicy.Gang.MinCount)
	assert.Equal(t, "high-priority", workload.Spec.PodGroupTemplates[0].PriorityClassName)
	require.NotNil(t, workload.Spec.ControllerRef)
	assert.Equal(t, "LeaderWorkerSet", workload.Spec.ControllerRef.Kind)
	assert.Equal(t, lws.Name, workload.Spec.ControllerRef.Name)

	for groupIndex, name := range []string{"test-lws-0-revision-1", "test-lws-1-revision-1"} {
		podGroup := &schedulingv1beta1.PodGroup{}
		require.NoError(t, fakeClient.Get(ctx, types.NamespacedName{Namespace: lws.Namespace, Name: name}, podGroup))
		require.NotNil(t, podGroup.Spec.WorkloadRef)
		assert.Equal(t, lws.Name, podGroup.Spec.WorkloadRef.WorkloadName)
		assert.Equal(t, workloadTemplateName, podGroup.Spec.WorkloadRef.TemplateName)
		assert.Equal(t, int32(3), podGroup.Spec.SchedulingPolicy.Gang.MinCount)
		assert.Equal(t, strconv.Itoa(groupIndex), podGroup.Labels[leaderworkerset.GroupIndexLabelKey])
		assert.Equal(t, "revision-1", podGroup.Labels[leaderworkerset.RevisionKey])
	}
}

func TestNewWorkloadBuilderMapsV1Beta1SchedulingConfiguration(t *testing.T) {
	lws := testScheduledLWS()
	lws.Spec.Scheduling = &leaderworkerset.LeaderWorkerSetSchedulingConfiguration{
		SchedulingPolicy: &schedulingv1beta1.PodGroupSchedulingPolicy{
			Gang: &schedulingv1beta1.GangSchedulingPolicy{MinCount: 3},
		},
		SchedulingConstraints: &schedulingv1beta1.PodGroupSchedulingConstraints{
			Topology: []schedulingv1beta1.TopologyConstraint{{Key: "topology.kubernetes.io/zone"}},
		},
		DisruptionMode: &schedulingv1beta1.DisruptionMode{
			All: &schedulingv1beta1.AllDisruptionMode{},
		},
		ResourceClaims: []schedulingv1beta1.PodGroupResourceClaim{{
			Name:              "gpu",
			ResourceClaimName: ptr.To("shared-gpu"),
		}},
	}

	builder := NewWorkloadBuilder(lws)
	require.Empty(t, builder.Validate(context.Background(), workloadbuilder.ValidationInput{}))
	workload, err := builder.BuildWorkload()
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

func TestKubernetesProviderDoesNotCreatePodGroupsWhenWorkloadCreationFails(t *testing.T) {
	ctx := context.Background()
	lws := testScheduledLWS()
	createErr := errors.New("injected Workload create failure")
	podGroupCreated := false
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithInterceptorFuncs(interceptor.Funcs{
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

func TestKubernetesProviderInjectPodGroupMetadata(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Annotations: map[string]string{WorkloadSchedulingAnnotationKey: "true"},
			Labels: map[string]string{
				leaderworkerset.SetNameLabelKey:    "test-lws",
				leaderworkerset.GroupIndexLabelKey: "4",
				leaderworkerset.RevisionKey:        "revision-1",
			},
		},
	}

	require.NoError(t, NewKubernetesProvider(nil).InjectPodGroupMetadata(pod))
	require.NotNil(t, pod.Spec.SchedulingGroup)
	require.NotNil(t, pod.Spec.SchedulingGroup.PodGroupName)
	assert.Equal(t, "test-lws-4-revision-1", *pod.Spec.SchedulingGroup.PodGroupName)
}

func TestKubernetesProviderDelegatedWorkload(t *testing.T) {
	ctx := context.Background()
	controller := true
	lws := testScheduledLWS()
	lws.OwnerReferences = []metav1.OwnerReference{{
		APIVersion: "example.test/v1",
		Kind:       "ParentJob",
		Name:       "parent",
		UID:        types.UID("parent-uid"),
		Controller: &controller,
	}}
	lws.Annotations = map[string]string{
		GroupTemplateNameAnnotation:       "child-template",
		ParentCompositePodGroupAnnotation: "parent-group",
	}
	parent := &unstructured.Unstructured{}
	parent.SetGroupVersionKind(schema.GroupVersionKind{Group: "example.test", Version: "v1", Kind: "ParentJob"})
	parent.SetName("parent")
	parent.SetNamespace(lws.Namespace)
	workload := &schedulingv1beta1.Workload{
		ObjectMeta: metav1.ObjectMeta{Name: "parent-workload", Namespace: lws.Namespace},
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
	parentGroup := &schedulingv1alpha3.CompositePodGroup{ObjectMeta: metav1.ObjectMeta{Name: "parent-group", Namespace: lws.Namespace}}
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(parent, workload, parentGroup).Build()

	require.NoError(t, NewKubernetesProvider(fakeClient).ReconcileScheduling(ctx, lws, 1, "revision-1"))
	group := &schedulingv1beta1.PodGroup{}
	require.NoError(t, fakeClient.Get(ctx, types.NamespacedName{Namespace: lws.Namespace, Name: "test-lws-0-revision-1"}, group))
	require.NotNil(t, group.Spec.WorkloadRef)
	assert.Equal(t, "parent-workload", group.Spec.WorkloadRef.WorkloadName)
	assert.Equal(t, "child-template", group.Spec.WorkloadRef.TemplateName)
	assert.Equal(t, ptr.To("parent-group"), group.Spec.ParentCompositePodGroupName)

	rootWorkload := &schedulingv1beta1.Workload{}
	err := fakeClient.Get(ctx, client.ObjectKeyFromObject(lws), rootWorkload)
	assert.True(t, apierrors.IsNotFound(err), "a delegated LWS must not create a second Workload")
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
			Scheduling: &leaderworkerset.LeaderWorkerSetSchedulingConfiguration{},
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

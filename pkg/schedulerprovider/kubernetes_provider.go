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
	"fmt"
	"reflect"
	"strconv"

	corev1 "k8s.io/api/core/v1"
	schedulingv1alpha3 "k8s.io/api/scheduling/v1alpha3"
	schedulingv1beta1 "k8s.io/api/scheduling/v1beta1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/component-helpers/scheduling/schedulingv1/workloadbuilder"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	leaderworkerset "sigs.k8s.io/lws/api/leaderworkerset/v1"
)

const (
	// Kubernetes identifies the upstream scheduling.k8s.io provider.
	Kubernetes ProviderType = "kubernetes"

	workloadTemplateName = "replica"

	// WorkloadSchedulingAnnotationKey is copied to managed pod templates and
	// tells the pod webhook to attach the upstream SchedulingGroup reference.
	WorkloadSchedulingAnnotationKey = "leaderworkerset.sigs.k8s.io/workload-aware-scheduling"
)

// KubernetesProvider manages upstream Workload and PodGroup resources.
type KubernetesProvider struct {
	client client.Client
}

func NewKubernetesProvider(c client.Client) *KubernetesProvider {
	return &KubernetesProvider{client: c}
}

// NewWorkloadItem maps the versioned LWS fields while retaining their source
// paths for workloadbuilder validation.
func NewWorkloadItem(lws *leaderworkerset.LeaderWorkerSet) *workloadbuilder.WorkloadItem {
	size := ptr.Deref(lws.Spec.LeaderWorkerTemplate.Size, 1)
	priorityClassName := lws.Spec.LeaderWorkerTemplate.WorkerTemplate.Spec.PriorityClassName
	if lws.Spec.LeaderWorkerTemplate.LeaderTemplate != nil {
		priorityClassName = lws.Spec.LeaderWorkerTemplate.LeaderTemplate.Spec.PriorityClassName
	}

	item := &workloadbuilder.WorkloadItem{
		Name: workloadTemplateName,
		Path: field.NewPath("spec", "scheduling"),
		DefaultConfig: &workloadbuilder.SchedulingConfig{
			Policy: &workloadbuilder.SchedulingPolicy{
				Gang: &workloadbuilder.GangSchedulingPolicy{},
			},
			PriorityClassName: priorityClassName,
		},
		Callbacks: []workloadbuilder.SchedulingConfigFunc{
			func(config *workloadbuilder.SchedulingConfig) {
				if config.Policy != nil && config.Policy.Gang != nil && config.Policy.Gang.MinCount == nil {
					config.Policy.Gang.MinCount = ptr.To(size)
				}
			},
		},
	}
	if lws.Spec.Scheduling != nil {
		input := toWorkloadBuilderInput(lws.Spec.Scheduling)
		item.Input = workloadbuilder.WorkloadInput{
			Policy: workloadbuilder.PolicyInput{
				PodGroupData: input.policy,
				PathElements: []string{"schedulingPolicy"},
			},
			Constraints: workloadbuilder.ConstraintsInput{
				PodGroupData: input.constraints,
				PathElements: []string{"schedulingConstraints"},
			},
			DisruptionMode: workloadbuilder.DisruptionModeInput{
				PodGroupData: input.disruptionMode,
				PathElements: []string{"disruptionMode"},
			},
			ResourceClaims: workloadbuilder.ResourceClaimsInput{
				PodGroupData: input.resourceClaims,
				PathElements: []string{"resourceClaims"},
			},
		}
	}
	return item
}

// NewWorkloadBuilder maps an LWS scheduling configuration onto the standard
// Kubernetes workloadbuilder input. It is shared by admission and reconcile so
// the two paths cannot drift.
func NewWorkloadBuilder(lws *leaderworkerset.LeaderWorkerSet) *workloadbuilder.Builder {
	owner := metav1.NewControllerRef(lws, leaderworkerset.GroupVersion.WithKind("LeaderWorkerSet"))
	return workloadbuilder.NewBuilder(NewWorkloadItem(lws), workloadbuilder.BuildOptions{
		Name:      lws.Name,
		Namespace: lws.Namespace,
		Owner:     owner,
		AllowedPolicies: []workloadbuilder.SchedulingPolicyOption{
			workloadbuilder.BasicPolicy,
			workloadbuilder.GangPolicy,
		},
		AllowedDisruptionModes: []workloadbuilder.DisruptionModeOption{
			workloadbuilder.SingleMode,
			workloadbuilder.AllMode,
		},
	})
}

// ReconcileScheduling enforces Workload -> PodGroup -> Pod creation order.
func (p *KubernetesProvider) ReconcileScheduling(ctx context.Context, lws *leaderworkerset.LeaderWorkerSet, replicas int32, revision string) error {
	if lws.Spec.Scheduling == nil {
		return nil
	}

	persisted, templateName, err := p.reconcileWorkload(ctx, lws)
	if err != nil {
		return err
	}

	materializer := workloadbuilder.NewBuilderFromExistingWorkload(persisted, workloadbuilder.BuildOptions{
		Owner: metav1.NewControllerRef(lws, leaderworkerset.GroupVersion.WithKind("LeaderWorkerSet")),
	})
	desiredGroups := make(map[string]struct{}, replicas)
	for groupIndex := int32(0); groupIndex < replicas; groupIndex++ {
		index := strconv.FormatInt(int64(groupIndex), 10)
		name := GetPodGroupName(lws.Name, index, revision)
		desiredGroups[name] = struct{}{}
		podGroup, err := materializer.NewPodGroup(name, templateName)
		if err != nil {
			return NewReconcileError(ReasonInvalidSchedulingConfiguration, fmt.Errorf("materialize PodGroup %q: %w", name, err))
		}
		podGroup.TypeMeta = metav1.TypeMeta{
			APIVersion: schedulingv1beta1.SchemeGroupVersion.String(),
			Kind:       "PodGroup",
		}
		podGroup.Labels = map[string]string{
			leaderworkerset.SetNameLabelKey:    lws.Name,
			leaderworkerset.GroupIndexLabelKey: index,
			leaderworkerset.RevisionKey:        revision,
		}
		if parentName := lws.Annotations[ParentCompositePodGroupAnnotation]; parentName != "" {
			parent := &schedulingv1alpha3.CompositePodGroup{}
			if err := p.client.Get(ctx, types.NamespacedName{Namespace: lws.Namespace, Name: parentName}, parent); err != nil {
				return NewReconcileError(ReasonParentWorkloadNotReady, fmt.Errorf("get parent CompositePodGroup %s/%s: %w", lws.Namespace, parentName, err))
			}
			podGroup.Spec.ParentCompositePodGroupName = ptr.To(parentName)
		}
		existing := &schedulingv1beta1.PodGroup{}
		key := types.NamespacedName{Namespace: podGroup.Namespace, Name: name}
		if err := p.client.Get(ctx, key, existing); err == nil {
			if err := validateExistingPodGroup(existing, podGroup); err != nil {
				return NewReconcileError(ReasonInvalidSchedulingConfiguration, err)
			}
		} else if !apierrors.IsNotFound(err) {
			return workloadAPIError(ReasonPodGroupCreateFailed, fmt.Errorf("get PodGroup %s: %w", key, err))
		} else if err := p.client.Create(ctx, podGroup); err != nil && !apierrors.IsAlreadyExists(err) {
			return workloadAPIError(ReasonPodGroupCreateFailed, fmt.Errorf("create PodGroup %s/%s: %w", podGroup.Namespace, name, err))
		}
	}

	// During scale-down the StatefulSet owns deletion of member Pods. Do not
	// delete a PodGroup until those Pods are actually gone; the next reconcile
	// will complete cleanup after the StatefulSet has observed the lower replica
	// count.
	if err := p.cleanupUnusedPodGroups(ctx, lws, desiredGroups); err != nil {
		return NewReconcileError(ReasonPodGroupCleanupBlocked, err)
	}
	return nil
}

func validateExistingPodGroup(current, desired *schedulingv1beta1.PodGroup) error {
	currentOwner := metav1.GetControllerOf(current)
	desiredOwner := metav1.GetControllerOf(desired)
	ownerMatches := currentOwner != nil && desiredOwner != nil &&
		currentOwner.APIVersion == desiredOwner.APIVersion && currentOwner.Kind == desiredOwner.Kind && currentOwner.Name == desiredOwner.Name
	if ownerMatches && currentOwner.UID != "" && desiredOwner.UID != "" {
		ownerMatches = currentOwner.UID == desiredOwner.UID
	}
	if !ownerMatches || !reflect.DeepEqual(current.Spec, desired.Spec) {
		return fmt.Errorf("PodGroup %s/%s has immutable scheduling configuration drift", current.Namespace, current.Name)
	}
	return nil
}

func (p *KubernetesProvider) reconcileWorkload(ctx context.Context, lws *leaderworkerset.LeaderWorkerSet) (*schedulingv1beta1.Workload, string, error) {
	if templateName := lws.Annotations[GroupTemplateNameAnnotation]; templateName != "" {
		workload, err := p.findDelegatedWorkload(ctx, lws)
		if err != nil {
			return nil, "", NewReconcileError(ReasonParentWorkloadNotReady, err)
		}
		return workload, templateName, nil
	}

	builder := NewWorkloadBuilder(lws)
	if errs := builder.Validate(ctx, workloadbuilder.ValidationInput{}); len(errs) > 0 {
		return nil, "", NewReconcileError(ReasonInvalidSchedulingConfiguration, errs.ToAggregate())
	}
	desiredWorkload, err := builder.BuildWorkload()
	if err != nil {
		return nil, "", NewReconcileError(ReasonInvalidSchedulingConfiguration, fmt.Errorf("build Workload: %w", err))
	}
	desiredWorkload.TypeMeta = metav1.TypeMeta{
		APIVersion: schedulingv1beta1.SchemeGroupVersion.String(),
		Kind:       "Workload",
	}

	persisted := &schedulingv1beta1.Workload{}
	key := types.NamespacedName{Name: lws.Name, Namespace: lws.Namespace}
	if err := p.client.Get(ctx, key, persisted); err != nil {
		if !apierrors.IsNotFound(err) {
			return nil, "", workloadAPIError(ReasonWorkloadCreateFailed, fmt.Errorf("get Workload %s: %w", key, err))
		}
		if err := p.client.Create(ctx, desiredWorkload); err != nil && !apierrors.IsAlreadyExists(err) {
			return nil, "", workloadAPIError(ReasonWorkloadCreateFailed, fmt.Errorf("create Workload %s: %w", key, err))
		}
		if err := p.client.Get(ctx, key, persisted); err != nil {
			return nil, "", workloadAPIError(ReasonWorkloadCreateFailed, fmt.Errorf("get persisted Workload %s: %w", key, err))
		}
	} else if err := updateMutableWorkloadFields(ctx, p.client, persisted, desiredWorkload); err != nil {
		return nil, "", NewReconcileError(ReasonInvalidSchedulingConfiguration, err)
	}
	return persisted, workloadTemplateName, nil
}

func (p *KubernetesProvider) findDelegatedWorkload(ctx context.Context, lws *leaderworkerset.LeaderWorkerSet) (*schedulingv1beta1.Workload, error) {
	owner := metav1.GetControllerOf(lws)
	if owner == nil {
		return nil, fmt.Errorf("%s requires a controller owner", GroupTemplateNameAnnotation)
	}
	workloads := &schedulingv1beta1.WorkloadList{}
	if err := p.client.List(ctx, workloads, client.InNamespace(lws.Namespace)); err != nil {
		return nil, fmt.Errorf("list delegated Workloads: %w", err)
	}

	var selected *schedulingv1beta1.Workload
	for owner != nil {
		gv, err := schema.ParseGroupVersion(owner.APIVersion)
		if err != nil {
			return nil, fmt.Errorf("parse owner apiVersion %q: %w", owner.APIVersion, err)
		}
		for i := range workloads.Items {
			ref := workloads.Items[i].Spec.ControllerRef
			if ref != nil && ref.APIGroup == gv.Group && ref.Kind == owner.Kind && ref.Name == owner.Name {
				selected = &workloads.Items[i]
			}
		}

		parent := &unstructured.Unstructured{}
		parent.SetGroupVersionKind(schema.GroupVersionKind{Group: gv.Group, Version: gv.Version, Kind: owner.Kind})
		if err := p.client.Get(ctx, types.NamespacedName{Namespace: lws.Namespace, Name: owner.Name}, parent); err != nil {
			if selected != nil && apierrors.IsNotFound(err) {
				break
			}
			return nil, fmt.Errorf("follow controller owner %s %s/%s: %w", owner.Kind, lws.Namespace, owner.Name, err)
		}
		owner = metav1.GetControllerOf(parent)
	}
	if selected == nil {
		return nil, fmt.Errorf("no parent Workload matches the LWS controller-owner chain")
	}
	return selected, nil
}

// updateMutableWorkloadFields updates the template's mutable scheduling policy
// while preserving priority fields populated by admission.
func updateMutableWorkloadFields(ctx context.Context, c client.Client, current, desired *schedulingv1beta1.Workload) error {
	if len(current.Spec.PodGroupTemplates) != 1 || len(desired.Spec.PodGroupTemplates) != 1 {
		return fmt.Errorf("Workload %s/%s must contain exactly one PodGroup template", current.Namespace, current.Name)
	}
	oldTemplate := current.Spec.PodGroupTemplates[0]
	newTemplate := desired.Spec.PodGroupTemplates[0]
	oldPolicy := oldTemplate.SchedulingPolicy
	newPolicy := newTemplate.SchedulingPolicy
	if oldTemplate.Name != newTemplate.Name ||
		(oldPolicy.Basic == nil) != (newPolicy.Basic == nil) ||
		(oldPolicy.Gang == nil) != (newPolicy.Gang == nil) ||
		!reflect.DeepEqual(oldTemplate.SchedulingConstraints, newTemplate.SchedulingConstraints) ||
		!reflect.DeepEqual(oldTemplate.ResourceClaims, newTemplate.ResourceClaims) ||
		!reflect.DeepEqual(oldTemplate.DisruptionMode, newTemplate.DisruptionMode) ||
		oldTemplate.PriorityClassName != newTemplate.PriorityClassName {
		return fmt.Errorf("Workload %s/%s has immutable scheduling configuration drift", current.Namespace, current.Name)
	}
	if oldPolicy.Gang != nil && newPolicy.Gang != nil && oldPolicy.Gang.MinCount != newPolicy.Gang.MinCount {
		current.Spec.PodGroupTemplates[0].SchedulingPolicy.Gang.MinCount = newPolicy.Gang.MinCount
		if err := c.Update(ctx, current); err != nil {
			return fmt.Errorf("update Workload %s/%s gang minCount: %w", current.Namespace, current.Name, err)
		}
	}
	return nil
}

func workloadAPIError(fallbackReason string, err error) error {
	if apimeta.IsNoMatchError(err) {
		return NewReconcileError(ReasonAPINotAvailable, err)
	}
	return NewReconcileError(fallbackReason, err)
}

func (p *KubernetesProvider) cleanupUnusedPodGroups(ctx context.Context, lws *leaderworkerset.LeaderWorkerSet, desired map[string]struct{}) error {
	groups := &schedulingv1beta1.PodGroupList{}
	if err := p.client.List(ctx, groups, client.InNamespace(lws.Namespace), client.MatchingLabels{
		leaderworkerset.SetNameLabelKey: lws.Name,
	}); err != nil {
		return fmt.Errorf("list PodGroups: %w", err)
	}
	pods := &corev1.PodList{}
	if err := p.client.List(ctx, pods, client.InNamespace(lws.Namespace), client.MatchingLabels{
		leaderworkerset.SetNameLabelKey: lws.Name,
	}); err != nil {
		return fmt.Errorf("list Pods before PodGroup cleanup: %w", err)
	}
	for i := range groups.Items {
		group := &groups.Items[i]
		if _, keep := desired[group.Name]; keep {
			continue
		}
		inUse := false
		for j := range pods.Items {
			ref := pods.Items[j].Spec.SchedulingGroup
			if ref != nil && ref.PodGroupName != nil && *ref.PodGroupName == group.Name {
				inUse = true
				break
			}
		}
		if !inUse {
			if err := p.client.Delete(ctx, group); err != nil && !apierrors.IsNotFound(err) {
				return fmt.Errorf("delete unused PodGroup %s/%s: %w", group.Namespace, group.Name, err)
			}
		}
	}
	return p.cleanupUnusedWorkloads(ctx, lws)
}

func (p *KubernetesProvider) cleanupUnusedWorkloads(ctx context.Context, lws *leaderworkerset.LeaderWorkerSet) error {
	workloads := &schedulingv1beta1.WorkloadList{}
	if err := p.client.List(ctx, workloads, client.InNamespace(lws.Namespace)); err != nil {
		return fmt.Errorf("list Workloads before cleanup: %w", err)
	}
	for i := range workloads.Items {
		workload := &workloads.Items[i]
		owner := metav1.GetControllerOf(workload)
		if owner == nil || owner.APIVersion != leaderworkerset.GroupVersion.String() || owner.Kind != "LeaderWorkerSet" || owner.Name != lws.Name || workload.Name == lws.Name {
			continue
		}
		if err := p.client.Delete(ctx, workload); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete obsolete Workload %s/%s: %w", workload.Namespace, workload.Name, err)
		}
	}
	return nil
}

// CreatePodGroupIfNotExists is intentionally a no-op: the LWS reconciler owns
// Kubernetes PodGroups and creates them before any member Pod.
func (p *KubernetesProvider) CreatePodGroupIfNotExists(context.Context, *leaderworkerset.LeaderWorkerSet, *corev1.Pod) error {
	return nil
}

// InjectPodGroupMetadata associates a managed Pod with its revision-specific
// upstream PodGroup.
func (p *KubernetesProvider) InjectPodGroupMetadata(pod *corev1.Pod) error {
	if pod.Annotations[WorkloadSchedulingAnnotationKey] != "true" {
		return nil
	}
	name := GetPodGroupName(
		pod.Labels[leaderworkerset.SetNameLabelKey],
		pod.Labels[leaderworkerset.GroupIndexLabelKey],
		pod.Labels[leaderworkerset.RevisionKey],
	)
	pod.Spec.SchedulingGroup = &corev1.PodSchedulingGroup{PodGroupName: ptr.To(name)}
	return nil
}

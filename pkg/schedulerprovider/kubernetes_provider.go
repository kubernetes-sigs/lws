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
	"k8s.io/component-helpers/scheduling/schedulingv1/workloadbuilder"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	leaderworkerset "sigs.k8s.io/lws/api/leaderworkerset/v1"
)

const (
	// Kubernetes identifies the upstream scheduling.k8s.io provider.
	Kubernetes ProviderType = "kubernetes"

	// WorkloadSchedulingAnnotationKey is copied to managed pod templates and
	// tells the pod webhook to attach the upstream SchedulingGroup reference.
	WorkloadSchedulingAnnotationKey = "leaderworkerset.sigs.k8s.io/workload-aware-scheduling"

	// SchedulingLevelLabelKey and PodGroupRoleLabelKey describe which LWS
	// hierarchy level a runtime PodGroup represents.
	SchedulingLevelLabelKey = "leaderworkerset.sigs.k8s.io/scheduling-level"
	PodGroupRoleLabelKey    = "leaderworkerset.sigs.k8s.io/role"
)

// KubernetesProvider manages upstream Workload and PodGroup resources.
type KubernetesProvider struct {
	client client.Client
}

func NewKubernetesProvider(c client.Client) *KubernetesProvider {
	return &KubernetesProvider{client: c}
}

// ReconcileScheduling enforces Workload -> PodGroup -> Pod creation order.
func (p *KubernetesProvider) ReconcileScheduling(ctx context.Context, lws *leaderworkerset.LeaderWorkerSet, replicas int32, revision string) error {
	if lws.Spec.Scheduling == nil {
		return nil
	}

	persisted, err := p.reconcileWorkload(ctx, lws)
	if err != nil {
		return err
	}

	materializer := workloadbuilder.NewBuilderFromExistingWorkload(persisted, workloadbuilder.BuildOptions{
		Owner: metav1.NewControllerRef(lws, leaderworkerset.GroupVersion.WithKind("LeaderWorkerSet")),
	})
	runtimeGroups, err := phaseOneRuntimeGroups(lws, replicas, revision)
	if err != nil {
		return NewReconcileError(ReasonInvalidSchedulingConfiguration, err)
	}
	parentName := lws.Annotations[ParentCompositePodGroupAnnotation]
	if parentName != "" {
		parent := &schedulingv1alpha3.CompositePodGroup{}
		if err := p.client.Get(ctx, types.NamespacedName{Namespace: lws.Namespace, Name: parentName}, parent); err != nil {
			return NewReconcileError(ReasonParentWorkloadNotReady, fmt.Errorf("get parent CompositePodGroup %s/%s: %w", lws.Namespace, parentName, err))
		}
	}
	if templateName := lws.Annotations[GroupTemplateNameAnnotation]; templateName != "" {
		for i := range runtimeGroups {
			runtimeGroups[i].templateName = templateName
		}
	}
	desiredGroups := make(map[string]struct{}, len(runtimeGroups))
	for _, runtimeGroup := range runtimeGroups {
		name := runtimeGroup.name
		desiredGroups[name] = struct{}{}
		podGroup, err := materializer.NewPodGroup(name, runtimeGroup.templateName)
		if err != nil {
			return NewReconcileError(ReasonInvalidSchedulingConfiguration, fmt.Errorf("materialize PodGroup %q: %w", name, err))
		}
		podGroup.TypeMeta = metav1.TypeMeta{
			APIVersion: schedulingv1beta1.SchemeGroupVersion.String(),
			Kind:       "PodGroup",
		}
		podGroup.Labels = runtimeGroup.labels
		if parentName != "" {
			podGroup.Spec.ParentCompositePodGroupName = ptr.To(parentName)
		}
		existing := &schedulingv1beta1.PodGroup{}
		key := types.NamespacedName{Namespace: podGroup.Namespace, Name: name}
		if err := p.client.Get(ctx, key, existing); err == nil {
			if err := updateMutablePodGroupFields(ctx, p.client, existing, podGroup, runtimeGroup.allowMinCountUpdate); err != nil {
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

type runtimePodGroup struct {
	name                string
	templateName        string
	labels              map[string]string
	allowMinCountUpdate bool
}

func phaseOneRuntimeGroups(lws *leaderworkerset.LeaderWorkerSet, replicas int32, revision string) ([]runtimePodGroup, error) {
	mode, err := SchedulingModeFor(lws)
	if err != nil {
		return nil, err
	}
	baseLabels := func(level SchedulingMode) map[string]string {
		return map[string]string{
			leaderworkerset.SetNameLabelKey: lws.Name,
			SchedulingLevelLabelKey:         string(level),
		}
	}

	switch mode {
	case SchedulingModeLWS:
		return []runtimePodGroup{{
			name:                GetLWSGroupName(lws.Name),
			templateName:        lwsWorkloadTemplateName,
			labels:              baseLabels(mode),
			allowMinCountUpdate: true,
		}}, nil
	case SchedulingModeReplica:
		groups := make([]runtimePodGroup, 0, replicas)
		for groupIndex := int32(0); groupIndex < replicas; groupIndex++ {
			index := strconv.FormatInt(int64(groupIndex), 10)
			labels := baseLabels(mode)
			labels[leaderworkerset.GroupIndexLabelKey] = index
			labels[leaderworkerset.RevisionKey] = revision
			groups = append(groups, runtimePodGroup{
				name:         GetPodGroupName(lws.Name, index, revision),
				templateName: replicaWorkloadTemplateName,
				labels:       labels,
			})
		}
		return groups, nil
	case SchedulingModeRole:
		groups := make([]runtimePodGroup, 0, replicas*2)
		for groupIndex := int32(0); groupIndex < replicas; groupIndex++ {
			index := strconv.FormatInt(int64(groupIndex), 10)
			for _, role := range []string{leaderWorkloadTemplateName, workerWorkloadTemplateName} {
				labels := baseLabels(mode)
				labels[leaderworkerset.GroupIndexLabelKey] = index
				labels[leaderworkerset.RevisionKey] = revision
				labels[PodGroupRoleLabelKey] = role
				groups = append(groups, runtimePodGroup{
					name:         GetRolePodGroupName(lws.Name, index, role, revision),
					templateName: role,
					labels:       labels,
				})
			}
		}
		return groups, nil
	default:
		return nil, fmt.Errorf("unsupported scheduling mode %q", mode)
	}
}

func updateMutablePodGroupFields(ctx context.Context, c client.Client, current, desired *schedulingv1beta1.PodGroup, allowMinCountUpdate bool) error {
	currentOwner := metav1.GetControllerOf(current)
	desiredOwner := metav1.GetControllerOf(desired)
	ownerMatches := currentOwner != nil && desiredOwner != nil &&
		currentOwner.APIVersion == desiredOwner.APIVersion && currentOwner.Kind == desiredOwner.Kind && currentOwner.Name == desiredOwner.Name
	if ownerMatches && currentOwner.UID != "" && desiredOwner.UID != "" {
		ownerMatches = currentOwner.UID == desiredOwner.UID
	}
	currentSpec := current.Spec.DeepCopy()
	desiredSpec := desired.Spec.DeepCopy()
	// The API server owns resolved priority fields and defaults disruptionMode
	// to Single. Normalize those values before checking controller-owned drift.
	desiredSpec.Priority = currentSpec.Priority
	desiredSpec.PreemptionPolicy = currentSpec.PreemptionPolicy
	defaultDisruptionMode := func(spec *schedulingv1beta1.PodGroupSpec) {
		if spec.DisruptionMode == nil {
			spec.DisruptionMode = &schedulingv1beta1.DisruptionMode{
				Single: &schedulingv1beta1.SingleDisruptionMode{},
			}
		}
	}
	defaultDisruptionMode(currentSpec)
	defaultDisruptionMode(desiredSpec)
	var changed bool
	if allowMinCountUpdate && currentSpec.SchedulingPolicy.Gang != nil && desiredSpec.SchedulingPolicy.Gang != nil {
		if currentSpec.SchedulingPolicy.Gang.MinCount != desiredSpec.SchedulingPolicy.Gang.MinCount {
			current.Spec.SchedulingPolicy.Gang.MinCount = desired.Spec.SchedulingPolicy.Gang.MinCount
			changed = true
		}
		currentSpec.SchedulingPolicy.Gang.MinCount = desiredSpec.SchedulingPolicy.Gang.MinCount
	}
	if !ownerMatches || !reflect.DeepEqual(currentSpec, desiredSpec) {
		return fmt.Errorf("PodGroup %s/%s has immutable scheduling configuration drift", current.Namespace, current.Name)
	}
	if current.Labels == nil {
		current.Labels = make(map[string]string, len(desired.Labels))
	}
	for key, value := range desired.Labels {
		if current.Labels[key] != value {
			current.Labels[key] = value
			changed = true
		}
	}
	if changed {
		if err := c.Update(ctx, current); err != nil {
			return fmt.Errorf("update mutable PodGroup fields for %s/%s: %w", current.Namespace, current.Name, err)
		}
	}
	return nil
}

func (p *KubernetesProvider) reconcileWorkload(ctx context.Context, lws *leaderworkerset.LeaderWorkerSet) (*schedulingv1beta1.Workload, error) {
	if templateName := lws.Annotations[GroupTemplateNameAnnotation]; templateName != "" {
		workload, err := p.findDelegatedWorkload(ctx, lws)
		if err != nil {
			return nil, NewReconcileError(ReasonParentWorkloadNotReady, err)
		}
		return workload, nil
	}

	desiredWorkload, err := buildFlatWorkload(ctx, lws)
	if err != nil {
		return nil, NewReconcileError(ReasonInvalidSchedulingConfiguration, fmt.Errorf("build Workload: %w", err))
	}
	desiredWorkload.TypeMeta = metav1.TypeMeta{
		APIVersion: schedulingv1beta1.SchemeGroupVersion.String(),
		Kind:       "Workload",
	}

	persisted := &schedulingv1beta1.Workload{}
	key := types.NamespacedName{Name: lws.Name, Namespace: lws.Namespace}
	if err := p.client.Get(ctx, key, persisted); err != nil {
		if !apierrors.IsNotFound(err) {
			return nil, workloadAPIError(ReasonWorkloadCreateFailed, fmt.Errorf("get Workload %s: %w", key, err))
		}
		if err := p.client.Create(ctx, desiredWorkload); err == nil {
			// Create returns the admitted object. Reuse it instead of issuing a
			// cache-backed GET that may briefly observe NotFound.
			persisted = desiredWorkload
		} else {
			if !apierrors.IsAlreadyExists(err) {
				return nil, workloadAPIError(ReasonWorkloadCreateFailed, fmt.Errorf("create Workload %s: %w", key, err))
			}
			if err := p.client.Get(ctx, key, persisted); err != nil {
				return nil, workloadAPIError(ReasonWorkloadCreateFailed, fmt.Errorf("get persisted Workload %s: %w", key, err))
			}
		}
	} else if err := updateMutableWorkloadFields(ctx, p.client, persisted, desiredWorkload); err != nil {
		return nil, NewReconcileError(ReasonInvalidSchedulingConfiguration, err)
	}
	return persisted, nil
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
	if len(current.Spec.PodGroupTemplates) != len(desired.Spec.PodGroupTemplates) {
		return fmt.Errorf("Workload %s/%s has immutable PodGroup template set drift", current.Namespace, current.Name)
	}
	currentByName := make(map[string]int, len(current.Spec.PodGroupTemplates))
	for i := range current.Spec.PodGroupTemplates {
		currentByName[current.Spec.PodGroupTemplates[i].Name] = i
	}

	changed := false
	for i := range desired.Spec.PodGroupTemplates {
		newTemplate := desired.Spec.PodGroupTemplates[i]
		oldIndex, found := currentByName[newTemplate.Name]
		if !found {
			return fmt.Errorf("Workload %s/%s has immutable PodGroup template set drift", current.Namespace, current.Name)
		}
		oldTemplate := current.Spec.PodGroupTemplates[oldIndex]
		oldPolicy := oldTemplate.SchedulingPolicy
		newPolicy := newTemplate.SchedulingPolicy
		if (oldPolicy.Basic == nil) != (newPolicy.Basic == nil) ||
			(oldPolicy.Gang == nil) != (newPolicy.Gang == nil) ||
			!reflect.DeepEqual(oldTemplate.SchedulingConstraints, newTemplate.SchedulingConstraints) ||
			!reflect.DeepEqual(oldTemplate.ResourceClaims, newTemplate.ResourceClaims) ||
			!reflect.DeepEqual(oldTemplate.DisruptionMode, newTemplate.DisruptionMode) ||
			oldTemplate.PriorityClassName != newTemplate.PriorityClassName {
			return fmt.Errorf("Workload %s/%s has immutable scheduling configuration drift in template %q", current.Namespace, current.Name, newTemplate.Name)
		}
		if oldPolicy.Gang != nil && newPolicy.Gang != nil && oldPolicy.Gang.MinCount != newPolicy.Gang.MinCount {
			current.Spec.PodGroupTemplates[oldIndex].SchedulingPolicy.Gang.MinCount = newPolicy.Gang.MinCount
			changed = true
		}
	}
	if changed {
		if err := c.Update(ctx, current); err != nil {
			return fmt.Errorf("update Workload %s/%s gang minCount values: %w", current.Namespace, current.Name, err)
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
	inUseGroups := make(map[string]struct{}, len(pods.Items))
	for i := range pods.Items {
		ref := pods.Items[i].Spec.SchedulingGroup
		if ref != nil && ref.PodGroupName != nil {
			inUseGroups[*ref.PodGroupName] = struct{}{}
		}
	}
	for i := range groups.Items {
		group := &groups.Items[i]
		if _, keep := desired[group.Name]; keep {
			continue
		}
		if _, inUse := inUseGroups[group.Name]; !inUse {
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
	mode := SchedulingMode(pod.Annotations[WorkloadSchedulingAnnotationKey])
	if mode == "" {
		return nil
	}
	// "true" was emitted by the initial implementation and remains readable
	// during an in-place controller upgrade.
	if mode == "true" {
		mode = SchedulingModeReplica
	}
	lwsName := pod.Labels[leaderworkerset.SetNameLabelKey]
	var name string
	switch mode {
	case SchedulingModeLWS:
		name = GetLWSGroupName(lwsName)
	case SchedulingModeReplica:
		name = GetPodGroupName(lwsName, pod.Labels[leaderworkerset.GroupIndexLabelKey], pod.Labels[leaderworkerset.RevisionKey])
	case SchedulingModeRole:
		role := workerWorkloadTemplateName
		if pod.Labels[leaderworkerset.WorkerIndexLabelKey] == "0" {
			role = leaderWorkloadTemplateName
		}
		name = GetRolePodGroupName(lwsName, pod.Labels[leaderworkerset.GroupIndexLabelKey], role, pod.Labels[leaderworkerset.RevisionKey])
	default:
		return fmt.Errorf("unsupported workload-aware scheduling mode %q", mode)
	}
	pod.Spec.SchedulingGroup = &corev1.PodSchedulingGroup{PodGroupName: ptr.To(name)}
	return nil
}

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
	"hash/fnv"
	"reflect"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	schedulingv1beta1 "k8s.io/api/scheduling/v1beta1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	utilrand "k8s.io/apimachinery/pkg/util/rand"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/component-helpers/scheduling/schedulingv1/workloadbuilder"
	"k8s.io/utils/dump"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	leaderworkerset "sigs.k8s.io/lws/api/leaderworkerset/v1"
)

const (
	// Kubernetes identifies the upstream scheduling.k8s.io provider.
	Kubernetes ProviderType = "kubernetes"

	// WorkloadSchedulingAnnotationKey is set on managed pod templates by the LWS
	// controller. The pod webhook reads it to choose a PodGroup name.
	WorkloadSchedulingAnnotationKey = "leaderworkerset.sigs.k8s.io/workload-aware-scheduling"
	// WorkloadNameAnnotationKey is set on managed pod templates by the LWS
	// controller. The pod webhook uses it because the admission request has no LWS object.
	WorkloadNameAnnotationKey = "leaderworkerset.sigs.k8s.io/workload-name"
	// SchedulingLevelLabelKey is set on created PodGroups by the kubernetes provider.
	SchedulingLevelLabelKey = "leaderworkerset.sigs.k8s.io/scheduling-level"
	// PodGroupRoleLabelKey is set on role-mode PodGroups by the kubernetes provider.
	PodGroupRoleLabelKey = "leaderworkerset.sigs.k8s.io/role"

	workloadControllerUIDIndex = "leaderworkerset.sigs.k8s.io/workload-controller-uid"
)

// SetupKubernetesIndexes registers cache indexes used by the Kubernetes provider.
func SetupKubernetesIndexes(indexer client.FieldIndexer) error {
	return indexer.IndexField(context.Background(), &schedulingv1beta1.Workload{}, workloadControllerUIDIndex, workloadControllerUIDIndexValues)
}

func workloadControllerUIDIndexValues(raw client.Object) []string {
	owner := metav1.GetControllerOf(raw)
	if owner == nil || owner.UID == "" {
		return nil
	}
	return []string{string(owner.UID)}
}

// KubernetesWorkloadName returns the UID-qualified Workload name.
func KubernetesWorkloadName(lws *leaderworkerset.LeaderWorkerSet) string {
	hasher := fnv.New32a()
	_, _ = fmt.Fprintf(hasher, "%v", dump.ForHash(lws.UID))
	hash := utilrand.SafeEncodeString(fmt.Sprint(hasher.Sum32()))
	maxPrefixLen := validation.DNS1123SubdomainMaxLength - len(hash) - 1
	prefix := lws.Name
	if len(prefix) > maxPrefixLen {
		prefix = prefix[:maxPrefixLen]
	}
	return prefix + "-" + hash
}

func kubernetesRuntimeName(workloadName string, parts ...string) string {
	separator := strings.LastIndexByte(workloadName, '-')
	prefix := workloadName
	identity := ""
	if separator > 0 {
		prefix = workloadName[:separator]
		identity = workloadName[separator+1:]
	}
	allParts := make([]string, 0, len(parts)+1)
	if identity != "" {
		allParts = append(allParts, identity)
	}
	allParts = append(allParts, parts...)
	suffix := strings.Join(allParts, "-")
	maxPrefixLen := validation.DNS1123SubdomainMaxLength - len(suffix) - 1
	if len(prefix) > maxPrefixLen {
		prefix = prefix[:maxPrefixLen]
	}
	return prefix + "-" + suffix
}

// KubernetesLWSGroupName returns the UID-qualified whole-LWS PodGroup name.
func KubernetesLWSGroupName(lws *leaderworkerset.LeaderWorkerSet) string {
	return kubernetesRuntimeName(KubernetesWorkloadName(lws), "lws")
}

// KubernetesPodGroupName returns a UID-qualified replica PodGroup name.
func KubernetesPodGroupName(lws *leaderworkerset.LeaderWorkerSet, groupIndex, revision string) string {
	return kubernetesRuntimeName(KubernetesWorkloadName(lws), groupIndex, revision)
}

// KubernetesRolePodGroupName returns a UID-qualified role PodGroup name.
func KubernetesRolePodGroupName(lws *leaderworkerset.LeaderWorkerSet, groupIndex, role, revision string) string {
	return kubernetesRuntimeName(KubernetesWorkloadName(lws), groupIndex, role, revision)
}

// KubernetesProvider manages upstream Workload and PodGroup resources.
type KubernetesProvider struct {
	client client.Client
}

func NewKubernetesProvider(c client.Client) *KubernetesProvider {
	return &KubernetesProvider{client: c}
}

// ReconcileScheduling creates scheduling objects before pods:
//   - find or create the Workload (or look up a parent-owned delegated Workload)
//   - create or update one PodGroup per desired instance
//   - require a parent CompositePodGroup to exist when that annotation is set
//   - delete unused LWS-owned PodGroups that no longer have member pods
//
// With groupIdentity Hash the per-replica instances are not known here: a group
// is identified by a key that admission draws for every leader pod, so those
// PodGroups are materialized by CreatePodGroupIfNotExists while the leader is
// still scheduling gated. Cleanup keeps every group that still has member pods,
// so it stays safe even though those names are not in the desired set.
func (p *KubernetesProvider) ReconcileScheduling(ctx context.Context, lws *leaderworkerset.LeaderWorkerSet, replicas int32, revision string) error {
	if lws.Spec.Scheduling == nil {
		return nil
	}

	persisted, err := p.reconcileWorkload(ctx, lws)
	if err != nil {
		return err
	}

	groups, err := desiredPodGroups(lws, replicas, revision)
	if err != nil {
		return NewReconcileError(ReasonInvalidSchedulingConfiguration, err)
	}
	desiredGroups := make(map[string]struct{}, len(groups))
	for _, group := range groups {
		desiredGroups[group.name] = struct{}{}
	}
	if err := p.ensurePodGroups(ctx, lws, persisted, groups); err != nil {
		return err
	}

	if err := p.cleanupUnusedPodGroups(ctx, lws, desiredGroups); err != nil {
		return NewReconcileError(ReasonPodGroupCleanupBlocked, err)
	}
	return nil
}

// ensurePodGroups materializes the given PodGroups from the persisted Workload
// templates and reconciles the mutable fields of the ones that already exist.
func (p *KubernetesProvider) ensurePodGroups(ctx context.Context, lws *leaderworkerset.LeaderWorkerSet, persisted *schedulingv1beta1.Workload, groups []desiredPodGroup) error {
	if len(groups) == 0 {
		return nil
	}
	materializer := workloadbuilder.NewBuilderFromExistingWorkload(persisted, workloadbuilder.BuildOptions{
		Owner: metav1.NewControllerRef(lws, leaderworkerset.GroupVersion.WithKind("LeaderWorkerSet")),
	})
	// TODO(phase2): honor ParentCompositePodGroupAnnotation by reading the named
	// CompositePodGroup (API reader or dedicated RBAC, not the cached client)
	// and setting PodGroup.Spec.ParentCompositePodGroupName. A cached Get
	// without list/watch stalls the reconciler.
	parentTemplateName := lws.Annotations[GroupTemplateNameAnnotation]
	delegated := parentTemplateName != ""
	for _, group := range groups {
		name := group.name
		if delegated {
			group.templateName = parentTemplateName
		}
		podGroup, err := materializer.NewPodGroup(name, group.templateName)
		if err != nil {
			return NewReconcileError(ReasonInvalidSchedulingConfiguration, fmt.Errorf("materialize PodGroup %q: %w", name, err))
		}
		podGroup.TypeMeta = metav1.TypeMeta{
			APIVersion: schedulingv1beta1.SchemeGroupVersion.String(),
			Kind:       "PodGroup",
		}
		podGroup.Labels = group.labels
		if !delegated {
			attachWorkloadOwnerReference(podGroup, persisted)
		}
		existing := &schedulingv1beta1.PodGroup{}
		key := types.NamespacedName{Namespace: podGroup.Namespace, Name: name}
		if err := p.client.Get(ctx, key, existing); err == nil {
			if !existing.DeletionTimestamp.IsZero() {
				return NewReconcileError(ReasonPodGroupCleanupBlocked, fmt.Errorf("PodGroup %s is still terminating", key))
			}
			if err := updateMutablePodGroupFields(ctx, p.client, existing, podGroup, group.allowMinCountUpdate); err != nil {
				return NewReconcileError(ReasonInvalidSchedulingConfiguration, err)
			}
		} else if !apierrors.IsNotFound(err) {
			return workloadAPIError(ReasonPodGroupCreateFailed, fmt.Errorf("get PodGroup %s: %w", key, err))
		} else if err := p.client.Create(ctx, podGroup); err != nil {
			if !apierrors.IsAlreadyExists(err) {
				return workloadAPIError(ReasonPodGroupCreateFailed, fmt.Errorf("create PodGroup %s/%s: %w", podGroup.Namespace, name, err))
			}
			if err := p.client.Get(ctx, key, existing); err != nil {
				return workloadAPIError(ReasonPodGroupCreateFailed, fmt.Errorf("get existing PodGroup %s: %w", key, err))
			}
			if !existing.DeletionTimestamp.IsZero() {
				return NewReconcileError(ReasonPodGroupCleanupBlocked, fmt.Errorf("PodGroup %s is still terminating", key))
			}
			if err := updateMutablePodGroupFields(ctx, p.client, existing, podGroup, group.allowMinCountUpdate); err != nil {
				return NewReconcileError(ReasonInvalidSchedulingConfiguration, err)
			}
		}
	}
	return nil
}

type desiredPodGroup struct {
	name                string
	templateName        string
	labels              map[string]string
	allowMinCountUpdate bool
}

func desiredPodGroups(lws *leaderworkerset.LeaderWorkerSet, replicas int32, revision string) ([]desiredPodGroup, error) {
	mode, err := SchedulingModeFor(lws)
	if err != nil {
		return nil, err
	}

	switch mode {
	case SchedulingModeLWS:
		if replicas == 0 {
			return nil, nil
		}
		return []desiredPodGroup{{
			name:                kubernetesRuntimeName(KubernetesWorkloadName(lws), "lws"),
			templateName:        lwsWorkloadTemplateName,
			labels:              basePodGroupLabels(lws, mode),
			allowMinCountUpdate: true,
		}}, nil
	case SchedulingModeReplica, SchedulingModeRole:
		// Hash group identity draws a group key per leader pod at admission
		// time, so the LWS controller cannot enumerate the replica instances
		// up front. CreatePodGroupIfNotExists materializes them instead.
		if hashGroupIdentity(lws) {
			return nil, nil
		}
		groups := make([]desiredPodGroup, 0, replicas*2)
		for groupIndex := int32(0); groupIndex < replicas; groupIndex++ {
			groups = append(groups, replicaPodGroups(lws, mode, strconv.FormatInt(int64(groupIndex), 10), revision)...)
		}
		return groups, nil
	default:
		return nil, fmt.Errorf("unsupported scheduling mode %q", mode)
	}
}

func basePodGroupLabels(lws *leaderworkerset.LeaderWorkerSet, mode SchedulingMode) map[string]string {
	return map[string]string{
		leaderworkerset.SetNameLabelKey: lws.Name,
		SchedulingLevelLabelKey:         string(mode),
	}
}

// replicaPodGroups returns the PodGroups of a single replica: one in replica
// mode, a leader and a worker one in role mode. groupIndex is the value of the
// group index label, an ordinal with groupIdentity Ordinal and the group key
// with groupIdentity Hash.
func replicaPodGroups(lws *leaderworkerset.LeaderWorkerSet, mode SchedulingMode, groupIndex, revision string) []desiredPodGroup {
	workloadName := KubernetesWorkloadName(lws)
	if mode == SchedulingModeReplica {
		labels := basePodGroupLabels(lws, mode)
		labels[leaderworkerset.GroupIndexLabelKey] = groupIndex
		labels[leaderworkerset.RevisionKey] = revision
		return []desiredPodGroup{{
			name:         kubernetesRuntimeName(workloadName, groupIndex, revision),
			templateName: replicaWorkloadTemplateName,
			labels:       labels,
		}}
	}
	groups := make([]desiredPodGroup, 0, 2)
	for _, role := range []string{leaderWorkloadTemplateName, workerWorkloadTemplateName} {
		labels := basePodGroupLabels(lws, mode)
		labels[leaderworkerset.GroupIndexLabelKey] = groupIndex
		labels[leaderworkerset.RevisionKey] = revision
		labels[PodGroupRoleLabelKey] = role
		groups = append(groups, desiredPodGroup{
			name:         kubernetesRuntimeName(workloadName, groupIndex, role, revision),
			templateName: role,
			labels:       labels,
		})
	}
	return groups
}

func hashGroupIdentity(lws *leaderworkerset.LeaderWorkerSet) bool {
	return lws.Spec.GroupIdentity == leaderworkerset.GroupIdentityHash
}

func updateMutablePodGroupFields(ctx context.Context, c client.Client, current, desired *schedulingv1beta1.PodGroup, allowMinCountUpdate bool) error {
	currentOwner := metav1.GetControllerOf(current)
	desiredOwner := metav1.GetControllerOf(desired)
	ownerMatches := controllerReferencesEqual(currentOwner, desiredOwner)
	currentSpec := current.Spec.DeepCopy()
	desiredSpec := desired.Spec.DeepCopy()
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
	if ensureWorkloadOwnerReference(current, desired) {
		changed = true
	}
	if changed {
		if err := c.Update(ctx, current); err != nil {
			return fmt.Errorf("update mutable PodGroup fields for %s/%s: %w", current.Namespace, current.Name, err)
		}
	}
	return nil
}

func attachWorkloadOwnerReference(podGroup *schedulingv1beta1.PodGroup, workload *schedulingv1beta1.Workload) {
	if workload == nil || workload.UID == "" {
		return
	}
	desired := metav1.OwnerReference{
		APIVersion: schedulingv1beta1.SchemeGroupVersion.String(),
		Kind:       "Workload",
		Name:       workload.Name,
		UID:        workload.UID,
		Controller: ptr.To(false),
	}
	ensureOwnerReference(&podGroup.ObjectMeta, desired)
}

func ensureWorkloadOwnerReference(current, desired *schedulingv1beta1.PodGroup) bool {
	want := workloadOwnerReference(desired)
	if want == nil {
		return false
	}
	return ensureOwnerReference(&current.ObjectMeta, *want)
}

func workloadOwnerReference(obj metav1.Object) *metav1.OwnerReference {
	for i := range obj.GetOwnerReferences() {
		ref := &obj.GetOwnerReferences()[i]
		if ref.Kind == "Workload" && (ref.Controller == nil || !*ref.Controller) {
			return ref.DeepCopy()
		}
	}
	return nil
}

func ensureOwnerReference(meta *metav1.ObjectMeta, desired metav1.OwnerReference) bool {
	refs := meta.OwnerReferences
	for i := range refs {
		if refs[i].Kind != desired.Kind || refs[i].APIVersion != desired.APIVersion {
			continue
		}
		if refs[i].Name == desired.Name && refs[i].UID == desired.UID &&
			ptr.Deref(refs[i].Controller, false) == ptr.Deref(desired.Controller, false) {
			return false
		}
		refs[i] = desired
		meta.OwnerReferences = refs
		return true
	}
	meta.OwnerReferences = append(meta.OwnerReferences, desired)
	return true
}

func (p *KubernetesProvider) reconcileWorkload(ctx context.Context, lws *leaderworkerset.LeaderWorkerSet) (*schedulingv1beta1.Workload, error) {
	if templateName := lws.Annotations[GroupTemplateNameAnnotation]; templateName != "" {
		// A parent controller owns the Workload; look it up and do not create one.
		workload, err := p.findDelegatedWorkload(ctx, lws)
		if err != nil {
			return nil, NewReconcileError(ReasonParentWorkloadNotReady, err)
		}
		return workload, nil
	}

	desiredWorkload, err := buildFlatWorkload(lws)
	if err != nil {
		return nil, NewReconcileError(ReasonInvalidSchedulingConfiguration, fmt.Errorf("build Workload: %w", err))
	}
	desiredWorkload.TypeMeta = metav1.TypeMeta{
		APIVersion: schedulingv1beta1.SchemeGroupVersion.String(),
		Kind:       "Workload",
	}

	persisted, err := p.findOwnedWorkload(ctx, lws)
	if err != nil {
		return nil, workloadAPIError(ReasonWorkloadCreateFailed, err)
	}
	if persisted != nil {
		if persisted.Name != desiredWorkload.Name {
			return nil, NewReconcileError(ReasonInvalidSchedulingConfiguration, fmt.Errorf("owned Workload %s/%s does not have the expected UID-qualified name %q", persisted.Namespace, persisted.Name, desiredWorkload.Name))
		}
		// Scale can change whole-LWS gang minCount; other Workload spec fields stay immutable.
		if err := updateMutableWorkloadFields(ctx, p.client, persisted, desiredWorkload); err != nil {
			return nil, NewReconcileError(ReasonInvalidSchedulingConfiguration, err)
		}
		return persisted, nil
	}

	if err := p.client.Create(ctx, desiredWorkload); err == nil {
		return desiredWorkload, nil
	} else if !apierrors.IsAlreadyExists(err) {
		return nil, workloadAPIError(ReasonWorkloadCreateFailed, fmt.Errorf("create Workload %s/%s: %w", desiredWorkload.Namespace, desiredWorkload.Name, err))
	}

	persisted = &schedulingv1beta1.Workload{}
	key := types.NamespacedName{Namespace: desiredWorkload.Namespace, Name: desiredWorkload.Name}
	if err := p.client.Get(ctx, key, persisted); err != nil {
		return nil, workloadAPIError(ReasonWorkloadCreateFailed, fmt.Errorf("get existing Workload %s: %w", key, err))
	}
	if !workloadControlledByLWS(persisted, lws) {
		return nil, NewReconcileError(ReasonWorkloadCreateFailed, fmt.Errorf("Workload %s already exists but is not controlled by this LeaderWorkerSet UID", key))
	}
	return persisted, nil
}

func (p *KubernetesProvider) findOwnedWorkload(ctx context.Context, lws *leaderworkerset.LeaderWorkerSet) (*schedulingv1beta1.Workload, error) {
	workloads := &schedulingv1beta1.WorkloadList{}
	if err := p.client.List(ctx, workloads, client.InNamespace(lws.Namespace), client.MatchingFields{
		workloadControllerUIDIndex: string(lws.UID),
	}); err != nil {
		return nil, fmt.Errorf("list Workloads for LeaderWorkerSet %s/%s: %w", lws.Namespace, lws.Name, err)
	}
	var selected *schedulingv1beta1.Workload
	for i := range workloads.Items {
		candidate := &workloads.Items[i]
		if !workloadControlledByLWS(candidate, lws) {
			continue
		}
		if selected != nil {
			return nil, fmt.Errorf("multiple Workloads are controlled by LeaderWorkerSet %s/%s UID %q", lws.Namespace, lws.Name, lws.UID)
		}
		selected = candidate
	}
	return selected, nil
}

func workloadControlledByLWS(workload *schedulingv1beta1.Workload, lws *leaderworkerset.LeaderWorkerSet) bool {
	wantOwner := metav1.NewControllerRef(lws, leaderworkerset.GroupVersion.WithKind("LeaderWorkerSet"))
	owner := metav1.GetControllerOf(workload)
	ref := workload.Spec.ControllerRef
	return controllerReferencesEqual(owner, wantOwner) && ref != nil &&
		ref.APIGroup == leaderworkerset.GroupVersion.Group && ref.Kind == "LeaderWorkerSet" && ref.Name == lws.Name
}

func controllerReferencesEqual(current, desired *metav1.OwnerReference) bool {
	if current == nil || desired == nil {
		return false
	}
	return current.APIVersion == desired.APIVersion && current.Kind == desired.Kind &&
		current.Name == desired.Name && current.UID == desired.UID
}

// findDelegatedWorkload walks the LWS controller-owner chain until it finds the
// parent-owned Workload that contains the annotated group template. Stop at the
// first matching hop so we do not GET third-party parents after the Workload
// is already known (ClusterRole does not cover those kinds).
func (p *KubernetesProvider) findDelegatedWorkload(ctx context.Context, lws *leaderworkerset.LeaderWorkerSet) (*schedulingv1beta1.Workload, error) {
	owner := metav1.GetControllerOf(lws)
	if owner == nil {
		return nil, fmt.Errorf("%s requires a controller owner", GroupTemplateNameAnnotation)
	}

	for owner != nil {
		if owner.UID == "" {
			return nil, fmt.Errorf("controller owner %s %s/%s has no UID", owner.Kind, lws.Namespace, owner.Name)
		}
		gv, err := schema.ParseGroupVersion(owner.APIVersion)
		if err != nil {
			return nil, fmt.Errorf("parse owner apiVersion %q: %w", owner.APIVersion, err)
		}
		workloads := &schedulingv1beta1.WorkloadList{}
		if err := p.client.List(ctx, workloads, client.InNamespace(lws.Namespace), client.MatchingFields{
			workloadControllerUIDIndex: string(owner.UID),
		}); err != nil {
			return nil, fmt.Errorf("list delegated Workloads for controller owner UID %q: %w", owner.UID, err)
		}
		var selected *schedulingv1beta1.Workload
		for i := range workloads.Items {
			candidate := &workloads.Items[i]
			ref := candidate.Spec.ControllerRef
			if controllerReferencesEqual(metav1.GetControllerOf(candidate), owner) && ref != nil &&
				ref.APIGroup == gv.Group && ref.Kind == owner.Kind && ref.Name == owner.Name {
				if selected != nil {
					return nil, fmt.Errorf("multiple parent Workloads match the LWS controller-owner chain: %s/%s and %s/%s", selected.Namespace, selected.Name, candidate.Namespace, candidate.Name)
				}
				selected = candidate
			}
		}
		if selected != nil {
			return selected, nil
		}

		parent := &unstructured.Unstructured{}
		parent.SetGroupVersionKind(schema.GroupVersionKind{Group: gv.Group, Version: gv.Version, Kind: owner.Kind})
		if err := p.client.Get(ctx, types.NamespacedName{Namespace: lws.Namespace, Name: owner.Name}, parent); err != nil {
			return nil, fmt.Errorf("follow controller owner %s %s/%s: %w", owner.Kind, lws.Namespace, owner.Name, err)
		}
		if parent.GetUID() != owner.UID {
			return nil, fmt.Errorf("controller owner %s %s/%s UID changed from %q to %q", owner.Kind, lws.Namespace, owner.Name, owner.UID, parent.GetUID())
		}
		owner = metav1.GetControllerOf(parent)
	}
	return nil, fmt.Errorf("no parent Workload matches the LWS controller-owner chain")
}

// updateMutableWorkloadFields applies gang minCount (for example whole-LWS scale)
// and copies labels. Other spec differences are treated as immutable drift.
func updateMutableWorkloadFields(ctx context.Context, c client.Client, current, desired *schedulingv1beta1.Workload) error {
	if !controllerReferencesEqual(metav1.GetControllerOf(current), metav1.GetControllerOf(desired)) ||
		!reflect.DeepEqual(current.Spec.ControllerRef, desired.Spec.ControllerRef) {
		return fmt.Errorf("Workload %s/%s is not controlled by the expected LeaderWorkerSet UID", current.Namespace, current.Name)
	}
	if len(current.Spec.PodGroupTemplates) != len(desired.Spec.PodGroupTemplates) {
		return fmt.Errorf("Workload %s/%s has immutable PodGroup template set drift", current.Namespace, current.Name)
	}
	currentByName := make(map[string]int, len(current.Spec.PodGroupTemplates))
	for i := range current.Spec.PodGroupTemplates {
		currentByName[current.Spec.PodGroupTemplates[i].Name] = i
	}

	changed := false
	if current.Labels == nil {
		current.Labels = make(map[string]string, len(desired.Labels))
	}
	for key, value := range desired.Labels {
		if current.Labels[key] != value {
			current.Labels[key] = value
			changed = true
		}
	}
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
	desiredOwner := metav1.NewControllerRef(lws, leaderworkerset.GroupVersion.WithKind("LeaderWorkerSet"))
	for i := range groups.Items {
		group := &groups.Items[i]
		if !controllerReferencesEqual(metav1.GetControllerOf(group), desiredOwner) {
			continue
		}
		if _, keep := desired[group.Name]; keep {
			continue
		}
		if _, inUse := inUseGroups[group.Name]; !inUse {
			if err := p.client.Delete(ctx, group); err != nil && !apierrors.IsNotFound(err) {
				return fmt.Errorf("delete unused PodGroup %s/%s: %w", group.Namespace, group.Name, err)
			}
		}
	}
	return nil
}

// CreatePodGroupIfNotExists materializes the PodGroups of a single replica for
// a groupIdentity Hash LeaderWorkerSet. With Ordinal identity every instance is
// known up front and ReconcileScheduling has already created it, so this is a
// no-op there.
//
// The pod controller calls this while the leader pod still carries the group
// replacement scheduling gate, which keeps the KEP-666 ordering: the leaf
// PodGroup exists before any member pod can be scheduled. The PodGroup is
// controller-owned by the LeaderWorkerSet, never by the leader pod, so a leader
// restart reuses it instead of racing garbage collection.
func (p *KubernetesProvider) CreatePodGroupIfNotExists(ctx context.Context, lws *leaderworkerset.LeaderWorkerSet, leaderPod *corev1.Pod) error {
	groups, err := leaderPodGroups(lws, leaderPod)
	if err != nil {
		return NewReconcileError(ReasonInvalidSchedulingConfiguration, err)
	}
	if len(groups) == 0 {
		return nil
	}
	persisted, err := p.findWorkload(ctx, lws)
	if err != nil {
		return err
	}
	return p.ensurePodGroups(ctx, lws, persisted, groups)
}

// leaderPodGroups returns the PodGroups that back the group of leaderPod, or
// nothing when the LWS does not need pod-driven materialization.
func leaderPodGroups(lws *leaderworkerset.LeaderWorkerSet, leaderPod *corev1.Pod) ([]desiredPodGroup, error) {
	if lws.Spec.Scheduling == nil || !hashGroupIdentity(lws) {
		return nil, nil
	}
	mode, err := SchedulingModeFor(lws)
	if err != nil {
		return nil, err
	}
	// The whole-LWS PodGroup has a group independent name and is created by
	// the LeaderWorkerSet controller in both identity modes.
	if mode == SchedulingModeLWS {
		return nil, nil
	}
	groupIndex := leaderPod.Labels[leaderworkerset.GroupIndexLabelKey]
	if groupIndex == "" {
		return nil, fmt.Errorf("leader pod %s/%s has no %s label", leaderPod.Namespace, leaderPod.Name, leaderworkerset.GroupIndexLabelKey)
	}
	revision := leaderPod.Labels[leaderworkerset.RevisionKey]
	if revision == "" {
		return nil, fmt.Errorf("leader pod %s/%s has no %s label", leaderPod.Namespace, leaderPod.Name, leaderworkerset.RevisionKey)
	}
	return replicaPodGroups(lws, mode, groupIndex, revision), nil
}

// findWorkload looks up the Workload that already backs the LeaderWorkerSet
// without creating one: only the LeaderWorkerSet controller compiles and
// creates it. Callers retry until it shows up.
func (p *KubernetesProvider) findWorkload(ctx context.Context, lws *leaderworkerset.LeaderWorkerSet) (*schedulingv1beta1.Workload, error) {
	if templateName := lws.Annotations[GroupTemplateNameAnnotation]; templateName != "" {
		workload, err := p.findDelegatedWorkload(ctx, lws)
		if err != nil {
			return nil, NewReconcileError(ReasonParentWorkloadNotReady, err)
		}
		return workload, nil
	}
	persisted, err := p.findOwnedWorkload(ctx, lws)
	if err != nil {
		return nil, workloadAPIError(ReasonWorkloadCreateFailed, err)
	}
	if persisted == nil {
		return nil, NewReconcileError(ReasonWorkloadCreateFailed,
			fmt.Errorf("Workload %s/%s has not been created yet", lws.Namespace, KubernetesWorkloadName(lws)))
	}
	return persisted, nil
}

func (p *KubernetesProvider) InjectPodGroupMetadata(pod *corev1.Pod) error {
	mode := SchedulingMode(pod.Annotations[WorkloadSchedulingAnnotationKey])
	if mode == "" {
		return nil
	}
	if mode == "true" {
		mode = SchedulingModeReplica
	}
	workloadName := pod.Annotations[WorkloadNameAnnotationKey]
	if workloadName == "" {
		workloadName = pod.Labels[leaderworkerset.SetNameLabelKey]
	}
	var name string
	switch mode {
	case SchedulingModeLWS:
		name = kubernetesRuntimeName(workloadName, "lws")
	case SchedulingModeReplica:
		name = kubernetesRuntimeName(workloadName, pod.Labels[leaderworkerset.GroupIndexLabelKey], pod.Labels[leaderworkerset.RevisionKey])
	case SchedulingModeRole:
		role := workerWorkloadTemplateName
		if pod.Labels[leaderworkerset.WorkerIndexLabelKey] == "0" {
			role = leaderWorkloadTemplateName
		}
		name = kubernetesRuntimeName(workloadName, pod.Labels[leaderworkerset.GroupIndexLabelKey], role, pod.Labels[leaderworkerset.RevisionKey])
	default:
		return fmt.Errorf("unsupported workload-aware scheduling mode %q", mode)
	}
	pod.Spec.SchedulingGroup = &corev1.PodSchedulingGroup{PodGroupName: ptr.To(name)}
	return nil
}

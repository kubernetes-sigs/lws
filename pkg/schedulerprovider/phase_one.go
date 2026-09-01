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

	corev1 "k8s.io/api/core/v1"
	schedulingv1alpha3 "k8s.io/api/scheduling/v1alpha3"
	schedulingv1beta1 "k8s.io/api/scheduling/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/component-helpers/scheduling/schedulingv1/workloadbuilder"
	"k8s.io/utils/ptr"

	leaderworkerset "sigs.k8s.io/lws/api/leaderworkerset/v1"
)

// SchedulingMode identifies the one LWS hierarchy level materialized as flat
// PodGroups during phase 1.
type SchedulingMode string

const (
	SchedulingModeLWS     SchedulingMode = "lws"
	SchedulingModeReplica SchedulingMode = "replica"
	SchedulingModeRole    SchedulingMode = "role"

	lwsWorkloadTemplateName     = "lws"
	replicaWorkloadTemplateName = "replica"
	leaderWorkloadTemplateName  = "leader"
	workerWorkloadTemplateName  = "worker"
)

func workloadBuildOptions(lws *leaderworkerset.LeaderWorkerSet) workloadbuilder.BuildOptions {
	return workloadbuilder.BuildOptions{
		Name:      KubernetesWorkloadName(lws),
		Namespace: lws.Namespace,
		Owner:     metav1.NewControllerRef(lws, leaderworkerset.GroupVersion.WithKind("LeaderWorkerSet")),
		AllowedPolicies: []workloadbuilder.SchedulingPolicyOption{
			workloadbuilder.BasicPolicy,
			workloadbuilder.GangPolicy,
		},
		AllowedDisruptionModes: []workloadbuilder.DisruptionModeOption{
			workloadbuilder.SingleMode,
			workloadbuilder.AllMode,
		},
	}
}

// SchedulingModeFor selects the active phase-1 level. Empty scheduling and an
// explicitly empty replica node both select replica mode.
func SchedulingModeFor(lws *leaderworkerset.LeaderWorkerSet) (SchedulingMode, error) {
	if lws.Spec.Scheduling == nil {
		return "", fmt.Errorf("spec.scheduling is not configured")
	}

	scheduling := lws.Spec.Scheduling
	topActive := scheduling.SchedulingPolicy != nil || scheduling.SchedulingConstraints != nil ||
		scheduling.DisruptionMode != nil || len(scheduling.ResourceClaims) > 0
	replicaFieldsActive := false
	roleActive := false
	if scheduling.Replica != nil {
		replica := scheduling.Replica
		replicaFieldsActive = replica.SchedulingPolicy != nil || replica.SchedulingConstraints != nil ||
			replica.DisruptionMode != nil || len(replica.ResourceClaims) > 0
		roleActive = replica.Leader != nil || replica.Worker != nil
	}

	active := make([]SchedulingMode, 0, 3)
	if topActive {
		active = append(active, SchedulingModeLWS)
	}
	// An explicitly empty replica object selects replica mode, except when it is
	// merely the parent of an active leader/worker level.
	if replicaFieldsActive || (scheduling.Replica != nil && !roleActive) {
		active = append(active, SchedulingModeReplica)
	}
	if roleActive {
		active = append(active, SchedulingModeRole)
	}
	if len(active) == 0 {
		return SchedulingModeReplica, nil
	}
	if len(active) != 1 {
		return "", fmt.Errorf("phase 1 requires exactly one active scheduling level, got %v", active)
	}
	return active[0], nil
}

// WorkloadSchedulingValue is stored on managed Pod templates so the Pod
// webhook can choose the level-aware runtime PodGroup name.
func WorkloadSchedulingValue(lws *leaderworkerset.LeaderWorkerSet) string {
	mode, err := SchedulingModeFor(lws)
	if err != nil {
		return string(SchedulingModeReplica)
	}
	return string(mode)
}

func phaseOneLeafItems(lws *leaderworkerset.LeaderWorkerSet) ([]*workloadbuilder.WorkloadItem, SchedulingMode, error) {
	mode, err := SchedulingModeFor(lws)
	if err != nil {
		return nil, "", err
	}

	size := ptr.Deref(lws.Spec.LeaderWorkerTemplate.Size, 1)
	replicas := ptr.Deref(lws.Spec.Replicas, 1)
	leaderPriority := lws.Spec.LeaderWorkerTemplate.WorkerTemplate.Spec.PriorityClassName
	if lws.Spec.LeaderWorkerTemplate.LeaderTemplate != nil {
		leaderPriority = lws.Spec.LeaderWorkerTemplate.LeaderTemplate.Spec.PriorityClassName
	}
	workerPriority := lws.Spec.LeaderWorkerTemplate.WorkerTemplate.Spec.PriorityClassName

	switch mode {
	case SchedulingModeLWS:
		config := lws.Spec.Scheduling
		minCount := replicas * size
		if minCount == 0 {
			// A Workload template must remain valid while a whole-LWS gang is
			// scaled to zero. No runtime PodGroup is created until scale-up.
			minCount = 1
		}
		item := newLeafItem(
			lwsWorkloadTemplateName,
			field.NewPath("spec", "scheduling"),
			lowerCompositePolicy(config.SchedulingPolicy),
			lowerCompositeConstraints(config.SchedulingConstraints),
			lowerCompositeDisruptionMode(config.DisruptionMode),
			config.ResourceClaims,
			basicSchedulingPolicy(),
			leaderPriority,
			minCount,
		)
		return []*workloadbuilder.WorkloadItem{item}, mode, nil
	case SchedulingModeReplica:
		config := lws.Spec.Scheduling.Replica
		if config == nil {
			config = &leaderworkerset.LeaderWorkerSetReplicaScheduling{}
		}
		item := newLeafItem(
			replicaWorkloadTemplateName,
			field.NewPath("spec", "scheduling", "replica"),
			lowerCompositePolicy(config.SchedulingPolicy),
			lowerCompositeConstraints(config.SchedulingConstraints),
			lowerCompositeDisruptionMode(config.DisruptionMode),
			config.ResourceClaims,
			gangSchedulingPolicy(),
			leaderPriority,
			size,
		)
		return []*workloadbuilder.WorkloadItem{item}, mode, nil
	case SchedulingModeRole:
		replica := lws.Spec.Scheduling.Replica
		leader := replica.Leader
		if leader == nil {
			leader = &leaderworkerset.LeaderWorkerSetLeaderScheduling{}
		}
		worker := replica.Worker
		if worker == nil {
			worker = &leaderworkerset.LeaderWorkerSetWorkerScheduling{}
		}
		return []*workloadbuilder.WorkloadItem{
			newLeafItem(
				leaderWorkloadTemplateName,
				field.NewPath("spec", "scheduling", "replica", "leader"),
				leader.SchedulingPolicy,
				leader.SchedulingConstraints,
				leader.DisruptionMode,
				leader.ResourceClaims,
				basicSchedulingPolicy(),
				leaderPriority,
				1,
			),
			newLeafItem(
				workerWorkloadTemplateName,
				field.NewPath("spec", "scheduling", "replica", "worker"),
				worker.SchedulingPolicy,
				worker.SchedulingConstraints,
				worker.DisruptionMode,
				worker.ResourceClaims,
				basicSchedulingPolicy(),
				workerPriority,
				size-1,
			),
		}, mode, nil
	default:
		return nil, "", fmt.Errorf("unsupported scheduling mode %q", mode)
	}
}

func newLeafItem(
	name string,
	path *field.Path,
	policy *schedulingv1alpha3.WorkloadPodGroupSchedulingPolicy,
	constraints *schedulingv1alpha3.WorkloadPodGroupSchedulingConstraints,
	disruptionMode *schedulingv1alpha3.WorkloadPodGroupDisruptionMode,
	resourceClaims []schedulingv1alpha3.WorkloadPodGroupResourceClaim,
	defaultPolicy *workloadbuilder.SchedulingPolicy,
	priorityClassName string,
	gangMinCount int32,
) *workloadbuilder.WorkloadItem {
	return &workloadbuilder.WorkloadItem{
		Name: name,
		Path: path,
		DefaultConfig: &workloadbuilder.SchedulingConfig{
			Policy:            defaultPolicy,
			PriorityClassName: priorityClassName,
		},
		Input: workloadbuilder.WorkloadInput{
			Policy: workloadbuilder.PolicyInput{
				PodGroupData: policy,
				PathElements: []string{"schedulingPolicy"},
			},
			Constraints: workloadbuilder.ConstraintsInput{
				PodGroupData: constraints,
				PathElements: []string{"schedulingConstraints"},
			},
			DisruptionMode: workloadbuilder.DisruptionModeInput{
				PodGroupData: disruptionMode,
				PathElements: []string{"disruptionMode"},
			},
			ResourceClaims: workloadbuilder.ResourceClaimsInput{
				PodGroupData: resourceClaims,
				PathElements: []string{"resourceClaims"},
			},
		},
		Callbacks: []workloadbuilder.SchedulingConfigFunc{
			func(config *workloadbuilder.SchedulingConfig) {
				if config.Policy != nil && config.Policy.Gang != nil && config.Policy.Gang.MinCount == nil {
					config.Policy.Gang.MinCount = ptr.To(gangMinCount)
				}
			},
		},
	}
}

func basicSchedulingPolicy() *workloadbuilder.SchedulingPolicy {
	return &workloadbuilder.SchedulingPolicy{Basic: &workloadbuilder.BasicSchedulingPolicy{}}
}

func gangSchedulingPolicy() *workloadbuilder.SchedulingPolicy {
	return &workloadbuilder.SchedulingPolicy{Gang: &workloadbuilder.GangSchedulingPolicy{}}
}

func lowerCompositePolicy(policy *schedulingv1alpha3.WorkloadCompositePodGroupSchedulingPolicy) *schedulingv1alpha3.WorkloadPodGroupSchedulingPolicy {
	if policy == nil {
		return nil
	}
	result := &schedulingv1alpha3.WorkloadPodGroupSchedulingPolicy{}
	if policy.Basic != nil {
		result.Basic = &schedulingv1alpha3.WorkloadPodGroupBasicSchedulingPolicy{}
	}
	if policy.Gang != nil {
		// minGroupCount counts child groups and therefore is never converted to
		// minCount. LWS derives the flat leaf's complete pod membership instead.
		result.Gang = &schedulingv1alpha3.WorkloadPodGroupGangSchedulingPolicy{}
	}
	return result
}

func lowerCompositeConstraints(constraints *schedulingv1alpha3.WorkloadCompositePodGroupSchedulingConstraints) *schedulingv1alpha3.WorkloadPodGroupSchedulingConstraints {
	if constraints == nil {
		return nil
	}
	return &schedulingv1alpha3.WorkloadPodGroupSchedulingConstraints{Topology: append([]schedulingv1alpha3.TopologyConstraint(nil), constraints.Topology...)}
}

func lowerCompositeDisruptionMode(mode *schedulingv1alpha3.WorkloadCompositePodGroupDisruptionMode) *schedulingv1alpha3.WorkloadPodGroupDisruptionMode {
	if mode == nil {
		return nil
	}
	result := &schedulingv1alpha3.WorkloadPodGroupDisruptionMode{}
	if mode.Single != nil {
		result.Single = &schedulingv1alpha3.WorkloadPodGroupSingleDisruptionMode{}
	}
	if mode.All != nil {
		result.All = &schedulingv1alpha3.WorkloadPodGroupAllDisruptionMode{}
	}
	return result
}

func compositeValidationItem(lws *leaderworkerset.LeaderWorkerSet, mode SchedulingMode) *workloadbuilder.WorkloadItem {
	var (
		name        string
		path        *field.Path
		policy      *schedulingv1alpha3.WorkloadCompositePodGroupSchedulingPolicy
		constraints *schedulingv1alpha3.WorkloadCompositePodGroupSchedulingConstraints
		disruption  *schedulingv1alpha3.WorkloadCompositePodGroupDisruptionMode
		defaultMode = basicSchedulingPolicy()
	)
	if mode == SchedulingModeLWS {
		name = lwsWorkloadTemplateName
		path = field.NewPath("spec", "scheduling")
		policy = lws.Spec.Scheduling.SchedulingPolicy
		constraints = lws.Spec.Scheduling.SchedulingConstraints
		disruption = lws.Spec.Scheduling.DisruptionMode
	} else {
		name = replicaWorkloadTemplateName
		path = field.NewPath("spec", "scheduling", "replica")
		defaultMode = gangSchedulingPolicy()
		if lws.Spec.Scheduling.Replica != nil {
			policy = lws.Spec.Scheduling.Replica.SchedulingPolicy
			constraints = lws.Spec.Scheduling.Replica.SchedulingConstraints
			disruption = lws.Spec.Scheduling.Replica.DisruptionMode
		}
	}
	return &workloadbuilder.WorkloadItem{
		Name:          name,
		Path:          path,
		DefaultConfig: &workloadbuilder.SchedulingConfig{Policy: defaultMode},
		Input: workloadbuilder.WorkloadInput{
			Policy:         workloadbuilder.PolicyInput{CompositePodGroupData: policy, PathElements: []string{"schedulingPolicy"}},
			Constraints:    workloadbuilder.ConstraintsInput{CompositePodGroupData: constraints, PathElements: []string{"schedulingConstraints"}},
			DisruptionMode: workloadbuilder.DisruptionModeInput{CompositePodGroupData: disruption, PathElements: []string{"disruptionMode"}},
		},
		// A child makes workloadbuilder validate this node with the composite
		// building-block validators. The child is validation-only in phase 1.
		Children: []*workloadbuilder.WorkloadItem{{
			Name:          name + "-validation-leaf",
			Path:          path,
			DefaultConfig: &workloadbuilder.SchedulingConfig{Policy: basicSchedulingPolicy()},
		}},
	}
}

// ValidatePhaseOneWorkload validates the original level-appropriate building
// blocks and the flat leaf inputs produced by phase-1 lowering.
func ValidatePhaseOneWorkload(ctx context.Context, oldLWS, lws *leaderworkerset.LeaderWorkerSet) field.ErrorList {
	path := field.NewPath("spec", "scheduling")
	mode, err := SchedulingModeFor(lws)
	if err != nil {
		return field.ErrorList{field.Invalid(path, lws.Spec.Scheduling, err.Error())}
	}

	var allErrs field.ErrorList
	var oldMode SchedulingMode
	if oldLWS != nil && oldLWS.Spec.Scheduling != nil {
		oldMode, err = SchedulingModeFor(oldLWS)
		if err == nil && oldMode != mode {
			allErrs = append(allErrs, field.Forbidden(path, "cannot switch the active scheduling level after creation"))
		}
		if oldPriority, oldConsistent := workloadPriorityClassName(oldLWS); oldConsistent {
			if newPriority, newConsistent := workloadPriorityClassName(lws); newConsistent && newPriority != oldPriority {
				allErrs = append(allErrs, field.Forbidden(field.NewPath("spec", "leaderWorkerTemplate"), "cannot change priorityClassName while workload-aware scheduling is configured"))
			}
		}
	}

	if mode == SchedulingModeLWS || mode == SchedulingModeReplica {
		item := compositeValidationItem(lws, mode)
		input := workloadbuilder.ValidationInput{}
		if oldLWS != nil && oldMode == mode {
			input.OldRoot = compositeValidationItem(oldLWS, oldMode)
		}
		allErrs = append(allErrs, workloadbuilder.NewBuilder(item, workloadBuildOptions(lws)).Validate(ctx, input)...)
	}

	items, _, _ := phaseOneLeafItems(lws)
	oldItemsByName := map[string]*workloadbuilder.WorkloadItem{}
	if oldLWS != nil && oldMode == mode {
		if oldItems, _, oldErr := phaseOneLeafItems(oldLWS); oldErr == nil {
			for _, item := range oldItems {
				oldItemsByName[item.Name] = item
			}
		}
	}
	for _, item := range items {
		allErrs = append(allErrs, workloadbuilder.NewBuilder(item, workloadBuildOptions(lws)).Validate(ctx, workloadbuilder.ValidationInput{
			OldRoot: oldItemsByName[item.Name],
		})...)
	}

	allErrs = append(allErrs, validatePhaseOneSemantics(lws, mode)...)
	return allErrs
}

func workloadPriorityClassName(lws *leaderworkerset.LeaderWorkerSet) (string, bool) {
	workerPriority := lws.Spec.LeaderWorkerTemplate.WorkerTemplate.Spec.PriorityClassName
	if lws.Spec.LeaderWorkerTemplate.LeaderTemplate == nil {
		return workerPriority, true
	}
	leaderPriority := lws.Spec.LeaderWorkerTemplate.LeaderTemplate.Spec.PriorityClassName
	return leaderPriority, leaderPriority == workerPriority
}

func validatePhaseOneSemantics(lws *leaderworkerset.LeaderWorkerSet, mode SchedulingMode) field.ErrorList {
	path := field.NewPath("spec", "scheduling")
	size := ptr.Deref(lws.Spec.LeaderWorkerTemplate.Size, 1)
	var allErrs field.ErrorList

	if mode == SchedulingModeLWS {
		policy := lws.Spec.Scheduling.SchedulingPolicy
		if policy != nil && policy.Gang != nil && policy.Gang.MinGroupCount != nil {
			allErrs = append(allErrs, field.Forbidden(path.Child("schedulingPolicy", "gang", "minGroupCount"), "minGroupCount requires CompositePodGroup support and is not available in phase 1"))
		}
	}
	if mode == SchedulingModeReplica {
		var policy *schedulingv1alpha3.WorkloadCompositePodGroupSchedulingPolicy
		if lws.Spec.Scheduling.Replica != nil {
			policy = lws.Spec.Scheduling.Replica.SchedulingPolicy
		}
		if policy != nil && policy.Gang != nil && policy.Gang.MinGroupCount != nil {
			allErrs = append(allErrs, field.Forbidden(path.Child("replica", "schedulingPolicy", "gang", "minGroupCount"), "minGroupCount requires CompositePodGroup support and is not available in phase 1"))
		}
	}
	if mode == SchedulingModeRole {
		if size < 2 {
			allErrs = append(allErrs, field.Invalid(field.NewPath("spec", "leaderWorkerTemplate", "size"), size, "leader/worker scheduling requires size >= 2"))
		}
		replica := lws.Spec.Scheduling.Replica
		if replica.Leader != nil {
			allErrs = append(allErrs, validateLeafGangMembership(replica.Leader.SchedulingPolicy, 1, path.Child("replica", "leader"))...)
		}
		if replica.Worker != nil {
			allErrs = append(allErrs, validateLeafGangMembership(replica.Worker.SchedulingPolicy, size-1, path.Child("replica", "worker"))...)
		}
	}

	gangContainsBothRoles := mode == SchedulingModeLWS && compositePolicyIsGang(lws.Spec.Scheduling.SchedulingPolicy, false)
	if mode == SchedulingModeReplica {
		var policy *schedulingv1alpha3.WorkloadCompositePodGroupSchedulingPolicy
		if lws.Spec.Scheduling.Replica != nil {
			policy = lws.Spec.Scheduling.Replica.SchedulingPolicy
		}
		gangContainsBothRoles = compositePolicyIsGang(policy, true)
	}
	if gangContainsBothRoles && lws.Spec.StartupPolicy == leaderworkerset.LeaderReadyStartupPolicy {
		allErrs = append(allErrs, field.Forbidden(path, "a gang containing the leader and workers is incompatible with startupPolicy LeaderReady"))
	}

	if lws.Annotations[leaderworkerset.ExclusiveKeyAnnotationKey] != "" && (gangAtSelectedLevel(lws, mode) || topologyAtSelectedLevel(lws, mode)) {
		allErrs = append(allErrs, field.Forbidden(path, "gang scheduling or workload topology constraints cannot be combined with exclusive topology in alpha"))
	}

	leaderTemplate := &lws.Spec.LeaderWorkerTemplate.WorkerTemplate
	if lws.Spec.LeaderWorkerTemplate.LeaderTemplate != nil {
		leaderTemplate = lws.Spec.LeaderWorkerTemplate.LeaderTemplate
	}
	workerTemplate := &lws.Spec.LeaderWorkerTemplate.WorkerTemplate
	if leaderTemplate.Spec.PriorityClassName != workerTemplate.Spec.PriorityClassName {
		allErrs = append(allErrs, field.Invalid(path, nil, "all managed pod templates must use the same priorityClassName"))
	}
	if leaderTemplate.Spec.SchedulingGroup != nil || workerTemplate.Spec.SchedulingGroup != nil {
		allErrs = append(allErrs, field.Forbidden(path, "managed pod templates must not set spec.schedulingGroup"))
	}

	if mode == SchedulingModeLWS {
		allErrs = append(allErrs, validateClaims(lws.Spec.Scheduling.ResourceClaims, []*corev1.PodTemplateSpec{leaderTemplate, workerTemplate}, path.Child("resourceClaims"))...)
	}
	if mode == SchedulingModeReplica && lws.Spec.Scheduling.Replica != nil {
		allErrs = append(allErrs, validateClaims(lws.Spec.Scheduling.Replica.ResourceClaims, []*corev1.PodTemplateSpec{leaderTemplate, workerTemplate}, path.Child("replica", "resourceClaims"))...)
	}
	if mode == SchedulingModeRole {
		replica := lws.Spec.Scheduling.Replica
		if replica.Leader != nil {
			allErrs = append(allErrs, validateClaims(replica.Leader.ResourceClaims, []*corev1.PodTemplateSpec{leaderTemplate}, path.Child("replica", "leader", "resourceClaims"))...)
		}
		if replica.Worker != nil {
			allErrs = append(allErrs, validateClaims(replica.Worker.ResourceClaims, []*corev1.PodTemplateSpec{workerTemplate}, path.Child("replica", "worker", "resourceClaims"))...)
		}
	}
	return allErrs
}

func validateLeafGangMembership(policy *schedulingv1alpha3.WorkloadPodGroupSchedulingPolicy, expected int32, path *field.Path) field.ErrorList {
	if policy == nil || policy.Gang == nil || policy.Gang.MinCount == nil {
		return nil
	}
	if *policy.Gang.MinCount != expected {
		return field.ErrorList{field.Invalid(path.Child("schedulingPolicy", "gang", "minCount"), *policy.Gang.MinCount, fmt.Sprintf("must equal complete leaf membership %d", expected))}
	}
	return nil
}

func compositePolicyIsGang(policy *schedulingv1alpha3.WorkloadCompositePodGroupSchedulingPolicy, defaultGang bool) bool {
	if policy == nil || (policy.Basic == nil && policy.Gang == nil) {
		return defaultGang
	}
	return policy.Basic == nil && policy.Gang != nil
}

func leafPolicyIsGang(policy *schedulingv1alpha3.WorkloadPodGroupSchedulingPolicy) bool {
	return policy != nil && policy.Basic == nil && policy.Gang != nil
}

func gangAtSelectedLevel(lws *leaderworkerset.LeaderWorkerSet, mode SchedulingMode) bool {
	switch mode {
	case SchedulingModeLWS:
		return compositePolicyIsGang(lws.Spec.Scheduling.SchedulingPolicy, false)
	case SchedulingModeReplica:
		if lws.Spec.Scheduling.Replica == nil {
			return true
		}
		return compositePolicyIsGang(lws.Spec.Scheduling.Replica.SchedulingPolicy, true)
	case SchedulingModeRole:
		replica := lws.Spec.Scheduling.Replica
		return (replica.Leader != nil && leafPolicyIsGang(replica.Leader.SchedulingPolicy)) ||
			(replica.Worker != nil && leafPolicyIsGang(replica.Worker.SchedulingPolicy))
	default:
		return false
	}
}

func topologyAtSelectedLevel(lws *leaderworkerset.LeaderWorkerSet, mode SchedulingMode) bool {
	switch mode {
	case SchedulingModeLWS:
		return lws.Spec.Scheduling.SchedulingConstraints != nil && len(lws.Spec.Scheduling.SchedulingConstraints.Topology) > 0
	case SchedulingModeReplica:
		return lws.Spec.Scheduling.Replica != nil && lws.Spec.Scheduling.Replica.SchedulingConstraints != nil && len(lws.Spec.Scheduling.Replica.SchedulingConstraints.Topology) > 0
	case SchedulingModeRole:
		replica := lws.Spec.Scheduling.Replica
		return (replica.Leader != nil && replica.Leader.SchedulingConstraints != nil && len(replica.Leader.SchedulingConstraints.Topology) > 0) ||
			(replica.Worker != nil && replica.Worker.SchedulingConstraints != nil && len(replica.Worker.SchedulingConstraints.Topology) > 0)
	default:
		return false
	}
}

func validateClaims(claims []schedulingv1alpha3.WorkloadPodGroupResourceClaim, templates []*corev1.PodTemplateSpec, path *field.Path) field.ErrorList {
	var allErrs field.ErrorList
	for i := range claims {
		claim := claims[i]
		for _, template := range templates {
			matched := false
			for j := range template.Spec.ResourceClaims {
				podClaim := template.Spec.ResourceClaims[j]
				if podClaim.Name == claim.Name && reflect.DeepEqual(podClaim.ResourceClaimName, claim.ResourceClaimName) && reflect.DeepEqual(podClaim.ResourceClaimTemplateName, claim.ResourceClaimTemplateName) {
					matched = true
					break
				}
			}
			if !matched {
				allErrs = append(allErrs, field.Invalid(path.Index(i), claim.Name, "must have a matching reference in every member pod template"))
			}
		}
	}
	return allErrs
}

// buildFlatWorkload compiles each selected phase-1 leaf independently and
// merges the stable templates into one Workload.
func buildFlatWorkload(ctx context.Context, lws *leaderworkerset.LeaderWorkerSet) (*schedulingv1beta1.Workload, error) {
	if errs := ValidatePhaseOneWorkload(ctx, nil, lws); len(errs) > 0 {
		return nil, errs.ToAggregate()
	}
	items, _, err := phaseOneLeafItems(lws)
	if err != nil {
		return nil, err
	}

	var result *schedulingv1beta1.Workload
	for _, item := range items {
		workload, err := workloadbuilder.NewBuilder(item, workloadBuildOptions(lws)).BuildWorkload()
		if err != nil {
			return nil, err
		}
		if result == nil {
			result = workload
			result.Labels = map[string]string{leaderworkerset.SetNameLabelKey: lws.Name}
			continue
		}
		result.Spec.PodGroupTemplates = append(result.Spec.PodGroupTemplates, workload.Spec.PodGroupTemplates...)
	}
	return result, nil
}

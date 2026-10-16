/*
Copyright 2023.

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

package webhooks

import (
	"context"
	"fmt"
	"math"
	"strconv"

	corev1 "k8s.io/api/core/v1"
	schedulingv1beta1 "k8s.io/api/scheduling/v1beta1"
	meta "k8s.io/apimachinery/pkg/api/meta"
	apivalidation "k8s.io/apimachinery/pkg/api/validation"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	utilvalidation "k8s.io/apimachinery/pkg/util/validation"

	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	v1 "sigs.k8s.io/lws/api/leaderworkerset/v1"
	"sigs.k8s.io/lws/pkg/features"
	"sigs.k8s.io/lws/pkg/schedulerprovider"
	"sigs.k8s.io/lws/pkg/utils"
)

type LeaderWorkerSetWebhook struct {
	SchedulerProvider schedulerprovider.ProviderType
	RESTMapper        meta.RESTMapper
}

// SetupLeaderWorkerSetWebhook will setup the manager to manage the webhooks

func SetupLeaderWorkerSetWebhook(mgr ctrl.Manager, options ...LeaderWorkerSetWebhook) error {
	hook := &LeaderWorkerSetWebhook{}
	if len(options) > 0 {
		hook = &options[0]
	}
	if hook.RESTMapper == nil {
		hook.RESTMapper = mgr.GetRESTMapper()
	}
	return ctrl.NewWebhookManagedBy(mgr, &v1.LeaderWorkerSet{}).
		WithDefaulter(hook).
		WithValidator(hook).
		Complete()
}

//+kubebuilder:webhook:path=/mutate-leaderworkerset-x-k8s-io-v1-leaderworkerset,mutating=true,failurePolicy=fail,sideEffects=None,groups=leaderworkerset.x-k8s.io,resources=leaderworkersets,verbs=create;update,versions=v1,name=mleaderworkerset.kb.io,admissionReviewVersions=v1

var _ admission.Defaulter[*v1.LeaderWorkerSet] = &LeaderWorkerSetWebhook{}

// Default implements admission.Defaulter[*v1.LeaderWorkerSet] so a webhook will be registered for the type
func (r *LeaderWorkerSetWebhook) Default(ctx context.Context, lws *v1.LeaderWorkerSet) error {
	if lws.Spec.Replicas == nil {
		lws.Spec.Replicas = ptr.To[int32](1)
	}

	if lws.Spec.LeaderWorkerTemplate.RestartPolicy == "" {
		lws.Spec.LeaderWorkerTemplate.RestartPolicy = v1.RecreateGroupOnPodRestart
	}

	if lws.Spec.GroupIdentity == "" {
		lws.Spec.GroupIdentity = v1.GroupIdentityOrdinal
	}

	if lws.Spec.GroupReplacementPolicy == "" {
		lws.Spec.GroupReplacementPolicy = v1.GroupReplacementPostTermination
	}

	if lws.Spec.PodTerminationPolicy == "" {
		lws.Spec.PodTerminationPolicy = v1.SequentialPodTerminationPolicy
	}

	if lws.Spec.LeaderWorkerTemplate.RestartPolicy == v1.DeprecatedDefaultRestartPolicy {
		lws.Spec.LeaderWorkerTemplate.RestartPolicy = v1.NoneRestartPolicy
	}

	if lws.Spec.RolloutStrategy.Type == "" {
		lws.Spec.RolloutStrategy.Type = v1.RollingUpdateStrategyType
	}

	if lws.Spec.RolloutStrategy.Type == v1.RollingUpdateStrategyType && lws.Spec.RolloutStrategy.RollingUpdateConfiguration == nil {
		lws.Spec.RolloutStrategy.RollingUpdateConfiguration = &v1.RollingUpdateConfiguration{
			MaxUnavailable: intstr.FromInt32(1),
			MaxSurge:       intstr.FromInt32(0),
			Partition:      ptr.To[int32](0),
		}
	}

	if lws.Spec.NetworkConfig == nil {
		lws.Spec.NetworkConfig = &v1.NetworkConfig{}
		subdomainPolicy := v1.SubdomainShared
		lws.Spec.NetworkConfig = &v1.NetworkConfig{
			SubdomainPolicy: &subdomainPolicy,
		}
	} else if lws.Spec.NetworkConfig.SubdomainPolicy == nil {
		subdomainPolicy := v1.SubdomainShared
		lws.Spec.NetworkConfig.SubdomainPolicy = &subdomainPolicy
	}

	if lws.Spec.LeaderWorkerTemplate.RestartBackoff != nil {
		if lws.Spec.LeaderWorkerTemplate.RestartBackoff.BaseSeconds == nil {
			lws.Spec.LeaderWorkerTemplate.RestartBackoff.BaseSeconds = ptr.To(v1.DefaultRestartBackoffBaseSeconds)
		}
		if lws.Spec.LeaderWorkerTemplate.RestartBackoff.CapSeconds == nil {
			lws.Spec.LeaderWorkerTemplate.RestartBackoff.CapSeconds = ptr.To(v1.DefaultRestartBackoffCapSeconds)
		}
	}

	return nil
}

//+kubebuilder:webhook:path=/validate-leaderworkerset-x-k8s-io-v1-leaderworkerset,mutating=false,failurePolicy=fail,sideEffects=None,groups=leaderworkerset.x-k8s.io,resources=leaderworkersets,verbs=create;update,versions=v1,name=vleaderworkerset.kb.io,admissionReviewVersions=v1

var _ admission.Validator[*v1.LeaderWorkerSet] = &LeaderWorkerSetWebhook{}

// ValidateCreate implements admission.Validator[*v1.LeaderWorkerSet] so a webhook will be registered for the type
func (r *LeaderWorkerSetWebhook) ValidateCreate(ctx context.Context, lws *v1.LeaderWorkerSet) (admission.Warnings, error) {
	allErrs := r.generalValidate(lws)
	allErrs = append(allErrs, r.validateScheduling(ctx, nil, lws)...)
	return placementPolicyWarnings(lws), allErrs.ToAggregate()
}

// ValidateUpdate implements admission.Validator[*v1.LeaderWorkerSet] so a webhook will be registered for the type
func (r *LeaderWorkerSetWebhook) ValidateUpdate(ctx context.Context, oldLws, newLws *v1.LeaderWorkerSet) (admission.Warnings, error) {
	allErrs := r.generalValidate(newLws)
	allErrs = append(allErrs, r.validateScheduling(ctx, oldLws, newLws)...)
	specPath := field.NewPath("spec")
	subGroupSizePath := specPath.Child("leaderWorkerTemplate", "subGroupPolicy", "subGroupSize")
	newSubGroupPolicy := newLws.Spec.LeaderWorkerTemplate.SubGroupPolicy
	oldSubGroupPolicy := oldLws.Spec.LeaderWorkerTemplate.SubGroupPolicy

	if newSubGroupPolicy != nil && oldSubGroupPolicy != nil && newSubGroupPolicy.SubGroupSize != nil && oldSubGroupPolicy.SubGroupSize != nil {
		allErrs = append(allErrs, apivalidation.ValidateImmutableField(*newSubGroupPolicy.SubGroupSize, *oldSubGroupPolicy.SubGroupSize, subGroupSizePath)...)
	}
	if newSubGroupPolicy != nil && oldSubGroupPolicy == nil {
		allErrs = append(allErrs, field.Invalid(subGroupSizePath, newSubGroupPolicy.SubGroupSize, "cannot enable subGroupSize after the lws is already created"))
	}
	if newSubGroupPolicy == nil && oldSubGroupPolicy != nil {
		allErrs = append(allErrs, field.Invalid(subGroupSizePath, oldSubGroupPolicy.SubGroupSize, "cannot remove subGroupSize after enabled"))
	}
	if newLws.Spec.NetworkConfig != nil && newLws.Spec.NetworkConfig.SubdomainPolicy == nil {
		allErrs = append(allErrs, field.Invalid(specPath.Child("networkConfig", "subdomainPolicy"), newLws.Spec.NetworkConfig.SubdomainPolicy, "cannot set subdomainPolicy as null"))
	}

	if normalizeGroupIdentity(newLws.Spec.GroupIdentity) != normalizeGroupIdentity(oldLws.Spec.GroupIdentity) {
		allErrs = append(allErrs, field.Invalid(specPath.Child("groupIdentity"), newLws.Spec.GroupIdentity, "groupIdentity is immutable"))
	}

	if normalizePodTerminationPolicy(newLws.Spec.PodTerminationPolicy) != normalizePodTerminationPolicy(oldLws.Spec.PodTerminationPolicy) {
		allErrs = append(allErrs, field.Invalid(specPath.Child("podTerminationPolicy"), newLws.Spec.PodTerminationPolicy, "podTerminationPolicy is immutable"))
	}

	return placementPolicyWarnings(newLws), allErrs.ToAggregate()
}

func (r *LeaderWorkerSetWebhook) validateScheduling(ctx context.Context, oldLws, lws *v1.LeaderWorkerSet) field.ErrorList {
	path := field.NewPath("spec", "scheduling")
	if lws.Spec.Scheduling == nil {
		if oldLws != nil && oldLws.Spec.Scheduling != nil {
			return field.ErrorList{field.Forbidden(path, "cannot remove scheduling after creation")}
		}
		return nil
	}

	var allErrs field.ErrorList
	// Disabling the gate, removing the scheduler provider, or disabling the
	// upstream API prevents new opt-ins, but existing scheduled objects must
	// remain updateable so they can scale down and be deleted safely.
	newOptIn := oldLws == nil || oldLws.Spec.Scheduling == nil
	if !features.Enabled(features.WorkloadAwareScheduling) && newOptIn {
		allErrs = append(allErrs, field.Forbidden(path, "requires the WorkloadAwareScheduling feature gate"))
	}
	if r.SchedulerProvider == "" {
		if newOptIn {
			allErrs = append(allErrs, field.Required(path, "requires a configured scheduler provider"))
		}
	} else if r.SchedulerProvider == schedulerprovider.Volcano {
		allErrs = append(allErrs, validateVolcanoScheduling(lws, path)...)
	} else if r.SchedulerProvider != schedulerprovider.Kubernetes {
		allErrs = append(allErrs, field.NotSupported(path, r.SchedulerProvider, []string{string(schedulerprovider.Kubernetes), string(schedulerprovider.Volcano)}))
	}
	if r.SchedulerProvider == schedulerprovider.Kubernetes && r.RESTMapper != nil && newOptIn {
		// Require scheduling.k8s.io/v1beta1 Workload and PodGroup APIs.
		for _, resource := range []string{"Workload", "PodGroup"} {
			if _, err := r.RESTMapper.RESTMapping(schema.GroupKind{Group: schedulingv1beta1.GroupName, Kind: resource}, schedulingv1beta1.SchemeGroupVersion.Version); err != nil {
				allErrs = append(allErrs, field.Forbidden(path, fmt.Sprintf("scheduling.k8s.io/v1beta1 %s API is not available: %v", resource, err)))
			}
		}
	}
	if r.SchedulerProvider == schedulerprovider.Kubernetes {
		// Users or a parent controller may set these KEP-6089 annotations; LWS does not.
		// group-template-name names a template in a parent-owned Workload and requires
		// a controller owner. parent-compositepodgroup is optional and requires the
		// template annotation; Phase 1 stores it and does not Get a CompositePodGroup.
		// Delegated scheduling is limited to whole-LWS or replica mode.
		templateName, delegated := lws.Annotations[schedulerprovider.GroupTemplateNameAnnotation]
		parentName, hasParent := lws.Annotations[schedulerprovider.ParentCompositePodGroupAnnotation]
		if hasParent && !delegated {
			allErrs = append(allErrs, field.Forbidden(field.NewPath("metadata", "annotations").Key(schedulerprovider.ParentCompositePodGroupAnnotation), "requires the group-template-name delegation annotation"))
		}
		if delegated {
			if templateName == "" {
				allErrs = append(allErrs, field.Required(field.NewPath("metadata", "annotations").Key(schedulerprovider.GroupTemplateNameAnnotation), "must name a parent Workload template"))
			}
			if metav1.GetControllerOf(lws) == nil {
				allErrs = append(allErrs, field.Forbidden(field.NewPath("metadata", "annotations").Key(schedulerprovider.GroupTemplateNameAnnotation), "delegated scheduling requires a controller owner"))
			}
			if hasParent && parentName == "" {
				allErrs = append(allErrs, field.Required(field.NewPath("metadata", "annotations").Key(schedulerprovider.ParentCompositePodGroupAnnotation), "must name a parent CompositePodGroup"))
			}
			if mode, err := schedulerprovider.SchedulingModeFor(lws); err == nil && mode == schedulerprovider.SchedulingModeRole {
				allErrs = append(allErrs, field.Forbidden(field.NewPath("metadata", "annotations").Key(schedulerprovider.GroupTemplateNameAnnotation), "delegated scheduling supports only whole-LWS or replica mode"))
			}
		}
	}
	if oldLws != nil && oldLws.Spec.Scheduling != nil {
		for _, annotation := range []string{
			schedulerprovider.GroupTemplateNameAnnotation,
			schedulerprovider.ParentCompositePodGroupAnnotation,
		} {
			oldVal, oldSet := oldLws.Annotations[annotation]
			newVal, newSet := lws.Annotations[annotation]
			if oldSet != newSet || oldVal != newVal {
				allErrs = append(allErrs, field.Forbidden(
					field.NewPath("metadata", "annotations").Key(annotation),
					"is immutable once spec.scheduling is set",
				))
			}
		}
		// The delegated parent Workload is resolved from the controller owner on
		// every reconcile, so pointing the LWS at a different controller would
		// silently move it to another parent's Workload.
		if _, delegated := oldLws.Annotations[schedulerprovider.GroupTemplateNameAnnotation]; delegated {
			oldOwner, newOwner := metav1.GetControllerOf(oldLws), metav1.GetControllerOf(lws)
			if oldOwner != nil && newOwner != nil && oldOwner.UID != newOwner.UID {
				allErrs = append(allErrs, field.Forbidden(
					field.NewPath("metadata", "ownerReferences"),
					"controller owner cannot be swapped for delegated scheduling",
				))
			}
		}
	}
	if oldLws != nil && oldLws.Spec.Scheduling == nil {
		allErrs = append(allErrs, field.Forbidden(path, "cannot add scheduling after creation"))
	}

	allErrs = append(allErrs, schedulerprovider.ValidatePhaseOneWorkload(ctx, oldLws, lws)...)
	return allErrs
}

func validateVolcanoScheduling(lws *v1.LeaderWorkerSet, path *field.Path) field.ErrorList {
	var allErrs field.ErrorList
	mode, err := schedulerprovider.SchedulingModeFor(lws)
	if err == nil && mode != schedulerprovider.SchedulingModeReplica {
		allErrs = append(allErrs, field.Forbidden(path, "the Volcano provider supports the typed API only at replica level"))
		return allErrs
	}
	replica := lws.Spec.Scheduling.Replica
	if replica == nil {
		return allErrs
	}
	if replica.SchedulingPolicy != nil && replica.SchedulingPolicy.Basic != nil {
		allErrs = append(allErrs, field.Forbidden(path.Child("replica", "schedulingPolicy"), "the Volcano provider does not support Basic policy"))
	}
	if replica.SchedulingConstraints != nil {
		allErrs = append(allErrs, field.Forbidden(path.Child("replica", "schedulingConstraints"), "the Volcano provider does not support typed scheduling constraints"))
	}
	if replica.DisruptionMode != nil {
		allErrs = append(allErrs, field.Forbidden(path.Child("replica", "disruptionMode"), "the Volcano provider does not support typed disruption mode"))
	}
	if replica.Leader != nil && len(replica.Leader.ResourceClaims) > 0 {
		allErrs = append(allErrs, field.Forbidden(path.Child("replica", "leader", "resourceClaims"), "the Volcano provider does not support shared resource claims"))
	}
	if replica.Worker != nil && len(replica.Worker.ResourceClaims) > 0 {
		allErrs = append(allErrs, field.Forbidden(path.Child("replica", "worker", "resourceClaims"), "the Volcano provider does not support shared resource claims"))
	}
	return allErrs
}

// ValidateDelete implements admission.Validator[*v1.LeaderWorkerSet] so a webhook will be registered for the type
func (r *LeaderWorkerSetWebhook) ValidateDelete(ctx context.Context, lws *v1.LeaderWorkerSet) (admission.Warnings, error) {
	return nil, nil
}

func (r *LeaderWorkerSetWebhook) generalValidate(lws *v1.LeaderWorkerSet) field.ErrorList {
	specPath := field.NewPath("spec")
	metadataPath := field.NewPath("metadata")

	// Since the lws name is used as the name for headless service, it must be DNS-1035 compliant
	ValidateName := apivalidation.NameIsDNS1035Label
	allErrs := apivalidation.ValidateObjectMeta(&lws.ObjectMeta, true, apivalidation.ValidateNameFunc(ValidateName), field.NewPath("metadata"))
	allErrs = append(allErrs, ValidateReplicaCount(specPath.Child("replicas"), int64(ptr.Deref(lws.Spec.Replicas, 1)), ptr.Deref(lws.Spec.LeaderWorkerTemplate.Size, 1))...)
	allErrs = append(allErrs, ValidateLeaderWorkerSetTemplate(specPath, metadataPath, lws)...)
	allErrs = append(allErrs, ValidateRollingUpdateBudget(
		lws.Spec.RolloutStrategy.RollingUpdateConfiguration,
		ptr.Deref(lws.Spec.Replicas, 1),
		specPath.Child("rolloutStrategy", "rollingUpdateConfiguration"),
	)...)
	if rollout := lws.Spec.RolloutStrategy.RollingUpdateConfiguration; rollout != nil && rollout.Partition != nil {
		partitionPath := specPath.Child("rolloutStrategy", "rollingUpdateConfiguration", "partition")
		allErrs = append(allErrs, validateNonnegativeField(int64(*rollout.Partition), partitionPath)...)
	}
	if normalizeGroupIdentity(lws.Spec.GroupIdentity) == v1.GroupIdentityHash {
		if maxLen := utilvalidation.DNS1035LabelMaxLength - HashNameSuffixLen(&lws.Spec); len(lws.Name) > maxLen {
			allErrs = append(allErrs, field.Invalid(metadataPath.Child("name"), lws.Name, fmt.Sprintf("must be no more than %d characters with groupIdentity Hash", maxLen)))
		}
	}
	return allErrs
}

// ValidatePodTerminationPolicy validates that podTerminationPolicy is one of the supported values.
func ValidatePodTerminationPolicy(specPath *field.Path, spec *v1.LeaderWorkerSetSpec) field.ErrorList {
	allErrs := field.ErrorList{}
	if spec.PodTerminationPolicy == "" {
		return allErrs
	}
	switch spec.PodTerminationPolicy {
	case v1.SequentialPodTerminationPolicy, v1.ParallelPodTerminationPolicy:
		return allErrs
	default:
		allErrs = append(allErrs, field.NotSupported(
			specPath.Child("podTerminationPolicy"),
			spec.PodTerminationPolicy,
			[]string{string(v1.SequentialPodTerminationPolicy), string(v1.ParallelPodTerminationPolicy)},
		))
	}
	return allErrs
}

// ValidateLeaderWorkerSetTemplate validates static constraints shared by LWS
// objects and embedded templates. Callers validate replica limits, rollout
// budgets, object metadata, scheduler support, and update transitions separately.
func ValidateLeaderWorkerSetTemplate(specPath, metadataPath *field.Path, lws *v1.LeaderWorkerSet) field.ErrorList {
	var allErrs field.ErrorList
	if ptr.Deref(lws.Spec.LeaderWorkerTemplate.Size, 1) < 1 {
		allErrs = append(allErrs, field.Invalid(specPath.Child("leaderWorkerTemplate", "size"), lws.Spec.LeaderWorkerTemplate.Size, "size must be equal or greater than 1"))
	}
	allErrs = append(allErrs, ValidateSubGroupPolicy(specPath, &lws.Spec)...)
	if lws.Spec.LeaderWorkerTemplate.SubGroupPolicy == nil {
		if value, found := lws.Annotations[v1.SubGroupExclusiveKeyAnnotationKey]; found {
			allErrs = append(allErrs, field.Invalid(metadataPath.Child("annotations", v1.SubGroupExclusiveKeyAnnotationKey), value, "cannot have subgroup-exclusive-topology without subGroupSize set"))
		}
	}
	allErrs = append(allErrs, validatePlacementPolicy(specPath, lws)...)
	allErrs = append(allErrs, validatePlacementPolicyAnnotations(specPath.Child("placementPolicy"), lws)...)
	allErrs = append(allErrs, ValidateMaxGroupRestarts(specPath, &lws.Spec)...)
	allErrs = append(allErrs, ValidateRestartBackoff(specPath, &lws.Spec)...)
	allErrs = append(allErrs, ValidateGroupIdentity(specPath, &lws.Spec)...)
	allErrs = append(allErrs, ValidatePodTerminationPolicy(specPath, &lws.Spec)...)
	return allErrs
}

// ValidateRollingUpdateBudget validates rollout budgets for a resolved replica count.
func ValidateRollingUpdateBudget(rollout *v1.RollingUpdateConfiguration, replicas int32, path *field.Path) field.ErrorList {
	if replicas == 0 {
		return validateRollingUpdateBudget(rollout, path, nil)
	}
	return validateRollingUpdateBudget(rollout, path, func(value *intstr.IntOrString, roundUp bool) (int, error) {
		return intstr.GetScaledValueFromIntOrPercent(value, int(replicas), roundUp)
	})
}

// ValidateUnknownTargetRollingUpdateBudget validates rollout budgets whose replica
// target is unknown: maxSurge and maxUnavailable are rejected only when both are zero.
func ValidateUnknownTargetRollingUpdateBudget(rollout *v1.RollingUpdateConfiguration, path *field.Path) field.ErrorList {
	return validateRollingUpdateBudget(rollout, path, func(value *intstr.IntOrString, _ bool) (int, error) {
		if percent, isPercent := getPercentValue(*value); isPercent {
			return percent, nil
		}
		return value.IntValue(), nil
	})
}

// validateRollingUpdateBudget validates each budget's format and, when resolve is
// set, rejects maxSurge and maxUnavailable that both resolve to zero.
func validateRollingUpdateBudget(rollout *v1.RollingUpdateConfiguration, path *field.Path, resolve func(value *intstr.IntOrString, roundUp bool) (int, error)) field.ErrorList {
	if rollout == nil {
		return nil
	}
	var allErrs field.ErrorList
	maxUnavailablePath := path.Child("maxUnavailable")
	maxSurgePath := path.Child("maxSurge")
	allErrs = append(allErrs, ValidatePositiveIntOrPercent(rollout.MaxUnavailable, maxUnavailablePath)...)
	allErrs = append(allErrs, IsNotMoreThan100Percent(rollout.MaxUnavailable, maxUnavailablePath)...)
	allErrs = append(allErrs, ValidatePositiveIntOrPercent(rollout.MaxSurge, maxSurgePath)...)
	allErrs = append(allErrs, IsNotMoreThan100Percent(rollout.MaxSurge, maxSurgePath)...)
	if len(allErrs) > 0 || resolve == nil {
		return allErrs
	}
	maxUnavailableValue, err := resolve(&rollout.MaxUnavailable, false)
	if err != nil {
		allErrs = append(allErrs, field.Invalid(maxUnavailablePath, rollout.MaxUnavailable, "invalid value"))
	}
	maxSurgeValue, err := resolve(&rollout.MaxSurge, true)
	if err != nil {
		allErrs = append(allErrs, field.Invalid(maxSurgePath, rollout.MaxSurge, "invalid value"))
	}
	if len(allErrs) == 0 && maxUnavailableValue == 0 && maxSurgeValue == 0 {
		allErrs = append(allErrs, field.Invalid(maxUnavailablePath, rollout.MaxUnavailable, "must not be 0 when `maxSurge` is 0"))
	}
	return allErrs
}

// ValidateMaxGroupRestarts validates that maxGroupRestarts is only configured
// with a restartPolicy that recreates the group.
// Exported for DisaggregatedSet webhook reuse.
func ValidateMaxGroupRestarts(specPath *field.Path, spec *v1.LeaderWorkerSetSpec) field.ErrorList {
	allErrs := field.ErrorList{}
	if spec.LeaderWorkerTemplate.MaxGroupRestarts == nil {
		return allErrs
	}
	restartPolicy := spec.LeaderWorkerTemplate.RestartPolicy
	if restartPolicy == "" {
		restartPolicy = v1.RecreateGroupOnPodRestart
	}
	// maxGroupRestarts applies to the two policies that recreate an entire group.
	// Reject it for policies that only rely on the kubelet to restart containers.
	if restartPolicy != v1.RecreateGroupOnPodRestart && restartPolicy != v1.RecreateGroupAfterStart {
		allErrs = append(allErrs, field.Invalid(
			specPath.Child("leaderWorkerTemplate", "maxGroupRestarts"),
			*spec.LeaderWorkerTemplate.MaxGroupRestarts,
			"maxGroupRestarts is only supported when restartPolicy recreates the group",
		))
	}
	return allErrs
}

// ValidateRestartBackoff validates that restartBackoff is only configured
// with a restartPolicy that recreates the group, and that baseSeconds and capSeconds are positive.
// Exported for DisaggregatedSet webhook reuse.
func ValidateRestartBackoff(specPath *field.Path, spec *v1.LeaderWorkerSetSpec) field.ErrorList {
	allErrs := field.ErrorList{}
	if spec.LeaderWorkerTemplate.RestartBackoff == nil {
		return allErrs
	}
	restartPolicy := spec.LeaderWorkerTemplate.RestartPolicy
	if restartPolicy == "" {
		restartPolicy = v1.RecreateGroupOnPodRestart
	}
	backoffPath := specPath.Child("leaderWorkerTemplate", "restartBackoff")
	if restartPolicy != v1.RecreateGroupOnPodRestart && restartPolicy != v1.RecreateGroupAfterStart {
		allErrs = append(allErrs, field.Invalid(
			backoffPath,
			spec.LeaderWorkerTemplate.RestartBackoff,
			"restartBackoff is only supported when restartPolicy recreates the group",
		))
	}
	baseSeconds := spec.LeaderWorkerTemplate.RestartBackoff.BaseSeconds
	capSeconds := spec.LeaderWorkerTemplate.RestartBackoff.CapSeconds
	if baseSeconds != nil && *baseSeconds <= 0 {
		allErrs = append(allErrs, field.Invalid(
			backoffPath.Child("baseSeconds"),
			*baseSeconds,
			"must be greater than 0",
		))
	}
	if capSeconds != nil && *capSeconds <= 0 {
		allErrs = append(allErrs, field.Invalid(
			backoffPath.Child("capSeconds"),
			*capSeconds,
			"must be greater than 0",
		))
	}
	effectiveBase := v1.DefaultRestartBackoffBaseSeconds
	if baseSeconds != nil {
		effectiveBase = *baseSeconds
	}
	effectiveCap := v1.DefaultRestartBackoffCapSeconds
	if capSeconds != nil {
		effectiveCap = *capSeconds
	}
	if effectiveBase > 0 && effectiveCap > 0 && effectiveBase > effectiveCap {
		badPath := backoffPath.Child("baseSeconds")
		badVal := any(effectiveBase)
		if baseSeconds == nil {
			badPath = backoffPath.Child("capSeconds")
			badVal = effectiveCap
		}
		allErrs = append(allErrs, field.Invalid(
			badPath,
			badVal,
			"baseSeconds must not be greater than capSeconds",
		))
	}
	return allErrs
}

func normalizeGroupIdentity(gi v1.GroupIdentityType) v1.GroupIdentityType {
	if gi == "" {
		return v1.GroupIdentityOrdinal
	}
	return gi
}

func normalizePodTerminationPolicy(policy v1.PodTerminationPolicyType) v1.PodTerminationPolicyType {
	if policy == "" {
		return v1.SequentialPodTerminationPolicy
	}
	return policy
}

// ValidateGroupIdentity rejects unsupported groupIdentity combinations.
// Exported for DisaggregatedSet webhook reuse.
func ValidateGroupIdentity(specPath *field.Path, spec *v1.LeaderWorkerSetSpec) field.ErrorList {
	allErrs := field.ErrorList{}
	if normalizeGroupIdentity(spec.GroupIdentity) != v1.GroupIdentityHash {
		if spec.GroupReplacementPolicy == v1.GroupReplacementImmediate {
			allErrs = append(allErrs, field.Invalid(specPath.Child("groupReplacementPolicy"), spec.GroupReplacementPolicy, "Immediate is only supported with groupIdentity Hash"))
		}
		return allErrs
	}
	giPath := specPath.Child("groupIdentity")
	if len(spec.LeaderWorkerTemplate.VolumeClaimTemplates) > 0 {
		allErrs = append(allErrs, field.Invalid(giPath, spec.GroupIdentity, "volumeClaimTemplates are not supported with groupIdentity Hash"))
	}
	if spec.RolloutStrategy.RollingUpdateConfiguration != nil && spec.RolloutStrategy.RollingUpdateConfiguration.Partition != nil && *spec.RolloutStrategy.RollingUpdateConfiguration.Partition != 0 {
		allErrs = append(allErrs, field.Invalid(giPath, spec.GroupIdentity, "rollingUpdateConfiguration.partition is not supported with groupIdentity Hash"))
	}
	// Each hash group needs its own leader host name.
	leaderTemplate := &spec.LeaderWorkerTemplate.WorkerTemplate
	if spec.LeaderWorkerTemplate.LeaderTemplate != nil {
		leaderTemplate = spec.LeaderWorkerTemplate.LeaderTemplate
	}
	if leaderTemplate.Spec.Hostname != "" {
		allErrs = append(allErrs, field.Invalid(giPath, spec.GroupIdentity, "a leader pod hostname is not supported with groupIdentity Hash"))
	}
	return allErrs
}

// HashNameSuffixLen is how many characters hash mode adds to the LWS name in
// the longest name it derives. The leader host name adds "-" plus 8 characters
// of the group key. The worker StatefulSet is named after that host name, and
// its pods get a controller-revision-hash label that adds another "-" plus up to 10.
func HashNameSuffixLen(spec *v1.LeaderWorkerSetSpec) int {
	const hostnameSuffixLen, revisionLabelSuffixLen = 1 + hashDNSPrefixLength, 11
	if spec.LeaderWorkerTemplate.Size != nil && *spec.LeaderWorkerTemplate.Size > 1 {
		return hostnameSuffixLen + revisionLabelSuffixLen
	}
	return hostnameSuffixLen
}

// This is mostly inspired by https://github.com/kubernetes/kubernetes/blob/be4b7176dc131ea842cab6882cd4a06dbfeed12a/pkg/apis/apps/validation/validation.go#L460,
// but it's not importable.

// ValidatePositiveIntOrPercent tests if a given value is a valid int or percentage.
func ValidatePositiveIntOrPercent(intOrPercent intstr.IntOrString, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}
	switch intOrPercent.Type {
	case intstr.String:
		for _, msg := range utilvalidation.IsValidPercent(intOrPercent.StrVal) {
			allErrs = append(allErrs, field.Invalid(fldPath, intOrPercent, msg))
		}
	case intstr.Int:
		allErrs = append(allErrs, validateNonnegativeField(int64(intOrPercent.IntValue()), fldPath)...)
	default:
		allErrs = append(allErrs, field.Invalid(fldPath, intOrPercent, "must be an integer or percentage (e.g '5%%')"))
	}
	return allErrs
}

// IsNotMoreThan100Percent tests is a value can be represented as a percentage
// and if this value is not more than 100%.
func IsNotMoreThan100Percent(intOrStringValue intstr.IntOrString, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}
	value, isPercent := getPercentValue(intOrStringValue)
	if !isPercent || value <= 100 {
		return nil
	}
	allErrs = append(allErrs, field.Invalid(fldPath, intOrStringValue, "must not be greater than 100%"))
	return allErrs
}
func getPercentValue(intOrStringValue intstr.IntOrString) (int, bool) {
	if intOrStringValue.Type != intstr.String {
		return 0, false
	}
	if len(utilvalidation.IsValidPercent(intOrStringValue.StrVal)) != 0 {
		return 0, false
	}
	value, _ := strconv.Atoi(intOrStringValue.StrVal[:len(intOrStringValue.StrVal)-1])
	return value, true
}

// validateNonnegativeField validates that given value is not negative.
func validateNonnegativeField(value int64, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}
	if value < 0 {
		allErrs = append(allErrs, field.Invalid(fldPath, value, "must be greater than or equal to 0"))
	}
	return allErrs
}

// ValidateReplicaCount checks the physical LWS replica count and total Pod count.
// An int64 count also supports sums of DisaggregatedSet sub-role targets.
func ValidateReplicaCount(path *field.Path, replicas int64, size int32) field.ErrorList {
	if replicas < 0 {
		return field.ErrorList{field.Invalid(path, replicas, "replicas must be equal or greater than 0")}
	}
	if replicas > 1000000 {
		return field.ErrorList{field.Invalid(path, replicas, "replicas must be equal or less than 1000000")}
	}
	if replicas*int64(size) > math.MaxInt32 {
		return field.ErrorList{field.Invalid(path, replicas, fmt.Sprintf("the product of replicas and worker replicas must not exceed %d", math.MaxInt32))}
	}
	return nil
}

// ValidateSubGroupPolicy checks that subgroups fit within a replica group.
// Exported for DisaggregatedSet webhook reuse.
func ValidateSubGroupPolicy(specPath *field.Path, spec *v1.LeaderWorkerSetSpec) field.ErrorList {
	allErrs := field.ErrorList{}
	policy := spec.LeaderWorkerTemplate.SubGroupPolicy
	if policy == nil {
		return allErrs
	}
	size := ptr.Deref(spec.LeaderWorkerTemplate.Size, 1)
	subGroupSizePath := specPath.Child("leaderWorkerTemplate", "subGroupPolicy", "subGroupSize")
	if policy.SubGroupSize == nil {
		return append(allErrs, field.Required(subGroupSizePath, "subGroupSize is required"))
	}

	subGroupSize := *policy.SubGroupSize
	if subGroupSize < 1 {
		return append(allErrs, field.Invalid(subGroupSizePath, subGroupSize, "subGroupSize must be equal or greater than 1"))
	}
	if (size%subGroupSize != 0) && ((size-1)%subGroupSize != 0) {
		allErrs = append(allErrs, field.Invalid(subGroupSizePath, subGroupSize, "size or size - 1 must be divisible by subGroupSize"))
	}
	if size < subGroupSize {
		allErrs = append(allErrs, field.Invalid(subGroupSizePath, subGroupSize, "subGroupSize cannot be larger than size"))
	}
	if policy.Type != nil &&
		(*policy.Type == v1.SubGroupPolicyTypeLeaderExcluded) &&
		((size-1)%subGroupSize != 0) {
		allErrs = append(allErrs, field.Invalid(subGroupSizePath, subGroupSize, "size-1 must be divisible by subGroupSize when using LeaderExcluded"))
	}
	return allErrs
}

// validatePlacementPolicy validates what the CRD schema cannot: that each
// topology is a valid label key, that subgroup placement has subgroups to place,
// and that the group and subgroup levels use different keys. With the same key,
// each subgroup would need its own domain inside the single domain of its group,
// which cannot schedule once a group has more than one subgroup, and is
// redundant otherwise. The keys are compared after resolving each level from
// the field or its annotation, so a level set through an annotation is checked
// too. The check runs only when placementPolicy is set, so objects that use only
// the annotations are validated as before.
func validatePlacementPolicy(specPath *field.Path, lws *v1.LeaderWorkerSet) field.ErrorList {
	allErrs := field.ErrorList{}
	policy := lws.Spec.PlacementPolicy
	if policy == nil {
		return allErrs
	}
	policyPath := specPath.Child("placementPolicy")
	subGroupPath := policyPath.Child("subGroup")
	allErrs = append(allErrs, validateTopologyKey(policy.Topology, policyPath.Child("topology"))...)
	subGroupFromField := false
	if policy.SubGroup != nil {
		allErrs = append(allErrs, validateTopologyKey(policy.SubGroup.Topology, subGroupPath.Child("topology"))...)
		subGroupFromField = policy.SubGroup.Type == v1.PlacementExclusiveTopology
		if subGroupFromField && lws.Spec.LeaderWorkerTemplate.SubGroupPolicy == nil {
			allErrs = append(allErrs, field.Invalid(subGroupPath, policy.SubGroup, "requires leaderWorkerTemplate.subGroupPolicy"))
		}
	}
	groupKey, group := utils.ExclusiveTopologyKey(lws)
	subGroupKey, subGroup := utils.SubGroupExclusiveTopologyKey(lws)
	if group && subGroup && groupKey == subGroupKey {
		path := policyPath.Child("topology")
		if subGroupFromField {
			path = subGroupPath.Child("topology")
		}
		allErrs = append(allErrs, field.Invalid(path, subGroupKey, "group and subgroup exclusive placement must use different topology keys"))
	}
	return allErrs
}

func validateTopologyKey(topology string, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}
	if topology == "" {
		// The CRD schema requires topology when the type needs one.
		return allErrs
	}
	for _, msg := range utilvalidation.IsQualifiedName(topology) {
		allErrs = append(allErrs, field.Invalid(fldPath, topology, msg))
	}
	return allErrs
}

// validatePlacementPolicyAnnotations rejects an ExclusiveTopology placement
// that disagrees with the exclusive-topology annotation of the same level. A
// matching pair is accepted so a value can be moved from the annotation to the
// field in two steps. A level whose type is None falls back to the annotation,
// since the API server defaults the type whenever placementPolicy is set.
func validatePlacementPolicyAnnotations(policyPath *field.Path, lws *v1.LeaderWorkerSet) field.ErrorList {
	policy := lws.Spec.PlacementPolicy
	if policy == nil {
		return nil
	}
	allErrs := validatePlacementPolicyAnnotation(policy.Type, policy.Topology, lws.Annotations[v1.ExclusiveKeyAnnotationKey],
		v1.ExclusiveKeyAnnotationKey, policyPath.Child("topology"))
	if policy.SubGroup != nil {
		allErrs = append(allErrs, validatePlacementPolicyAnnotation(policy.SubGroup.Type, policy.SubGroup.Topology, lws.Annotations[v1.SubGroupExclusiveKeyAnnotationKey],
			v1.SubGroupExclusiveKeyAnnotationKey, policyPath.Child("subGroup", "topology"))...)
	}
	return allErrs
}

func validatePlacementPolicyAnnotation(placementType v1.PlacementType, topology, annotationValue, annotation string, fldPath *field.Path) field.ErrorList {
	if placementType != v1.PlacementExclusiveTopology || annotationValue == "" || topology == annotationValue {
		return nil
	}
	return field.ErrorList{field.Invalid(fldPath, topology, fmt.Sprintf("conflicts with annotation %s=%s", annotation, annotationValue))}
}

// kueueTopologyAnnotations are the pod template annotations that request Kueue
// topology-aware scheduling.
var kueueTopologyAnnotations = []string{
	"kueue.x-k8s.io/podset-required-topology",
	"kueue.x-k8s.io/podset-preferred-topology",
	"kueue.x-k8s.io/podset-unconstrained-topology",
}

// placementPolicyWarnings warns about the deprecated exclusive-topology
// annotations, and about exclusive placement on a template that also requests
// Kueue topology-aware scheduling, which does not see the injected affinity.
func placementPolicyWarnings(lws *v1.LeaderWorkerSet) admission.Warnings {
	var warnings admission.Warnings
	if _, found := lws.Annotations[v1.ExclusiveKeyAnnotationKey]; found {
		warnings = append(warnings, fmt.Sprintf("annotation %s is deprecated, use spec.placementPolicy instead", v1.ExclusiveKeyAnnotationKey))
	}
	if _, found := lws.Annotations[v1.SubGroupExclusiveKeyAnnotationKey]; found {
		warnings = append(warnings, fmt.Sprintf("annotation %s is deprecated, use spec.placementPolicy.subGroup instead", v1.SubGroupExclusiveKeyAnnotationKey))
	}
	_, exclusive := utils.ExclusiveTopologyKey(lws)
	_, subGroupExclusive := utils.SubGroupExclusiveTopologyKey(lws)
	if !exclusive && !subGroupExclusive {
		return warnings
	}
	templates := []*corev1.PodTemplateSpec{&lws.Spec.LeaderWorkerTemplate.WorkerTemplate}
	if lws.Spec.LeaderWorkerTemplate.LeaderTemplate != nil {
		templates = append(templates, lws.Spec.LeaderWorkerTemplate.LeaderTemplate)
	}
	for _, template := range templates {
		for _, annotation := range kueueTopologyAnnotations {
			if _, found := template.Annotations[annotation]; found {
				return append(warnings, fmt.Sprintf("exclusive placement is combined with %s: Kueue topology-aware scheduling does not account for the affinity that exclusive placement injects, so a group can be assigned a domain it cannot schedule in", annotation))
			}
		}
	}
	return warnings
}

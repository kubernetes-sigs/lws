/*
Copyright 2025 The Kubernetes Authors.

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

package disaggregatedset

import (
	"context"
	"fmt"
	"strconv"

	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	disaggv1 "sigs.k8s.io/lws/api/disaggregatedset/v1"
	leaderworkerset "sigs.k8s.io/lws/api/leaderworkerset/v1"
	"sigs.k8s.io/lws/pkg/webhooks"
)

// DisaggregatedSetWebhook handles validation for DisaggregatedSet resources.
type DisaggregatedSetWebhook struct{}

// SetupDisaggregatedSetWebhook registers the webhook with the manager.
func SetupDisaggregatedSetWebhook(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr, &disaggv1.DisaggregatedSet{}).
		WithValidator(&DisaggregatedSetWebhook{}).
		Complete()
}

//+kubebuilder:webhook:path=/validate-disaggregatedset-x-k8s-io-v1-disaggregatedset,mutating=false,failurePolicy=fail,sideEffects=None,groups=disaggregatedset.x-k8s.io,resources=disaggregatedsets,verbs=create;update,versions=v1,name=vdisaggregatedset.kb.io,admissionReviewVersions=v1

var _ admission.Validator[*disaggv1.DisaggregatedSet] = &DisaggregatedSetWebhook{}

// ValidateCreate implements admission.Validator for create operations.
func (w *DisaggregatedSetWebhook) ValidateCreate(ctx context.Context, disagg *disaggv1.DisaggregatedSet) (admission.Warnings, error) {
	warnings, allErrs := w.validate(disagg)
	allErrs = append(allErrs, w.validatePlacement(disagg)...)
	return warnings, allErrs.ToAggregate()
}

// ValidateUpdate implements admission.Validator for update operations.
func (w *DisaggregatedSetWebhook) ValidateUpdate(ctx context.Context, oldDisagg, newDisagg *disaggv1.DisaggregatedSet) (admission.Warnings, error) {
	warnings, allErrs := w.validate(newDisagg)
	allErrs = append(allErrs, w.validatePlacement(newDisagg)...)
	return warnings, allErrs.ToAggregate()
}

// ValidateDelete implements admission.Validator for delete operations.
func (w *DisaggregatedSetWebhook) ValidateDelete(ctx context.Context, disagg *disaggv1.DisaggregatedSet) (admission.Warnings, error) {
	return nil, nil
}

func (w *DisaggregatedSetWebhook) validate(obj *disaggv1.DisaggregatedSet) (admission.Warnings, field.ErrorList) {
	var allErrs field.ErrorList
	var warnings admission.Warnings
	rolesPath := field.NewPath("spec", "roles")

	hasExternal := false
	for i, role := range obj.Spec.Roles {
		rolePath := rolesPath.Index(i)
		allErrs = append(allErrs, w.validateRoleRolloutStrategy(role, rolePath)...)

		if role.Scaling == nil || role.Scaling.Mode != disaggv1.RoleScalingExternal {
			continue
		}
		hasExternal = true

		// Scaler name is "<ds>-<role>" and must fit within the Kubernetes 253-character limit.
		if scalerName := obj.Name + "-" + role.Name; len(scalerName) > 253 {
			allErrs = append(allErrs, field.Invalid(rolePath.Child("name"), role.Name,
				fmt.Sprintf("would produce scaler name %q exceeding 253 characters", scalerName)))
		}

		// spec.replicas is ignored for External roles; explicit values > 1
		// almost certainly indicate confusion about which knob takes effect.
		if role.Spec.Replicas != nil && *role.Spec.Replicas > 1 {
			warnings = append(warnings, fmt.Sprintf(
				"role %q sets scaling.mode: External and spec.replicas: %d — spec.replicas is ignored; drive replicas via DisaggregatedSetRoleScaler %q instead",
				role.Name, *role.Spec.Replicas, obj.Name+"-"+role.Name))
		}
	}

	// Alpha: External scaling and slices > 1 are incompatible. The scaler
	// design for multi-slice is deferred to a follow-up KEP.
	if hasExternal && obj.Spec.Slices != nil && *obj.Spec.Slices > 1 {
		allErrs = append(allErrs, field.Forbidden(
			field.NewPath("spec", "slices"),
			"spec.slices > 1 is not supported while any role has scaling.mode: External (alpha restriction)"))
	}

	allErrs = append(allErrs, w.validateGeneratedNames(obj)...)

	return warnings, allErrs
}

// validateGeneratedNames rejects the DisaggregatedSet if any role would produce
// a generated LWS name or derived Service name (<lws>-prv) exceeding the
// DNS-1035 63-character limit. The generated name format is:
//
//	<dsName>-<sliceIndex>-<revision 8 chars>-<roleName>
//
// and the service appends "-prv".
func (w *DisaggregatedSetWebhook) validateGeneratedNames(obj *disaggv1.DisaggregatedSet) field.ErrorList {
	var allErrs field.ErrorList

	// Worst-case slice index string length: slices can be 1-100 → index 0-99 → up to 2 digits.
	slices := int32(1)
	if obj.Spec.Slices != nil {
		slices = *obj.Spec.Slices
	}
	maxSliceIndex := slices - 1
	sliceDigits := len(strconv.Itoa(int(maxSliceIndex)))

	const (
		dns1035MaxLen                     = 63
		revisionLen                       = 8  // hex characters in the revision hash
		serviceSuffixLen                  = 4  // len("-prv")
		separators                        = 3  // three "-" between dsName, slice, revision, roleName
		statefulSetRevisionLabelSuffixLen = 11 // len("-<10-char-hash>")
	)

	rolesPath := field.NewPath("spec", "roles")
	for i, role := range obj.Spec.Roles {
		lwsNameLen := len(obj.Name) + separators + sliceDigits + revisionLen + len(role.Name)

		groupIndexDigits := 1
		if role.Spec.Replicas != nil && *role.Spec.Replicas > 0 {
			groupIndexDigits = len(strconv.Itoa(int(*role.Spec.Replicas - 1)))
		}

		// The worker StatefulSet pods get a label "controller-revision-hash" with value:
		// <lwsName>-<groupIndex>-<hash>
		// Which translates to: lwsNameLen + 1 (dash) + groupIndexDigits + statefulSetRevisionLabelSuffixLen
		workerRevisionLabelSuffixLen := 1 + groupIndexDigits + statefulSetRevisionLabelSuffixLen

		maxSuffixLen := serviceSuffixLen
		if workerRevisionLabelSuffixLen > maxSuffixLen {
			maxSuffixLen = workerRevisionLabelSuffixLen
		}

		maxNameLen := lwsNameLen + maxSuffixLen

		if maxNameLen > dns1035MaxLen {
			combinedLimit := dns1035MaxLen - separators - sliceDigits - revisionLen - maxSuffixLen
			allErrs = append(allErrs, field.Invalid(
				rolesPath.Index(i).Child("name"),
				role.Name,
				fmt.Sprintf(
					"the generated names (%d chars) would exceed the DNS-1035 limit of %d characters (accounting for service name and StatefulSet revision hash labels); "+
						"reduce the DisaggregatedSet name and/or role name (combined limit: %d characters)",
					maxNameLen, dns1035MaxLen, combinedLimit,
				),
			))
		}
	}
	return allErrs
}

// validatePlacement validates the DisaggregatedSet PlacementPolicy. A non-None policy
// needs a topology key, and conflicts with the LWS group-level exclusive-topology
// annotation on a role: both co-locate/exclude at overlapping levels, so the slice
// would never schedule.
func (w *DisaggregatedSetWebhook) validatePlacement(obj *disaggv1.DisaggregatedSet) field.ErrorList {
	var allErrs field.ErrorList

	policy := obj.Spec.PlacementPolicy
	if policy == nil || policy.Type == disaggv1.PlacementNone || policy.Type == "" {
		return allErrs
	}
	policyPath := field.NewPath("spec", "placementPolicy")

	if policy.Topology == "" {
		allErrs = append(allErrs, field.Required(policyPath.Child("topology"),
			"topology is required when type is not None"))
	}

	rolesPath := field.NewPath("spec", "roles")
	for i, role := range obj.Spec.Roles {
		if key, found := roleExclusiveTopologyAnnotation(role); found {
			allErrs = append(allErrs, field.Forbidden(
				rolesPath.Index(i),
				fmt.Sprintf("the %q annotation must not be combined with a non-None spec.placementPolicy.type (%s)",
					key, policy.Type)))
		}
	}

	return allErrs
}

// exclusiveTopologyAnnotationKeys are the LWS annotations that make the LWS pod
// webhook inject its own exclusive-placement affinity, at the group and subgroup
// level respectively. Either one conflicts with a DisaggregatedSet placement policy.
var exclusiveTopologyAnnotationKeys = []string{
	leaderworkerset.ExclusiveKeyAnnotationKey,
	leaderworkerset.SubGroupExclusiveKeyAnnotationKey,
}

// roleExclusiveTopologyAnnotation returns the first LWS exclusive-topology annotation
// (group or subgroup level) a role carries anywhere it takes effect: the LWS metadata,
// or the leader/worker pod templates (the LWS pod webhook reads these from the pod, so
// a template-level annotation would enable LWS exclusive placement too).
func roleExclusiveTopologyAnnotation(role disaggv1.DisaggregatedRoleSpec) (string, bool) {
	template := role.Spec.LeaderWorkerTemplate
	for _, key := range exclusiveTopologyAnnotationKeys {
		if _, ok := role.ObjectMeta.Annotations[key]; ok {
			return key, true
		}
		if template.LeaderTemplate != nil {
			if _, ok := template.LeaderTemplate.Annotations[key]; ok {
				return key, true
			}
		}
		if _, ok := template.WorkerTemplate.Annotations[key]; ok {
			return key, true
		}
	}
	return "", false
}

// validateRoleRolloutStrategy validates the RolloutStrategy fields for a role.
// DisaggregatedSet handles rolling updates differently from LWS and does not support:
// - RolloutStrategy.Type other than RollingUpdate (or empty, which defaults to RollingUpdate)
// - RolloutStrategy.RollingUpdateConfiguration.Partition
func (w *DisaggregatedSetWebhook) validateRoleRolloutStrategy(role disaggv1.DisaggregatedRoleSpec, rolePath *field.Path) field.ErrorList {
	var allErrs field.ErrorList
	rolloutPath := rolePath.Child("spec", "rolloutStrategy")

	// Validate Type - must be empty or RollingUpdate
	if role.Spec.RolloutStrategy.Type != "" && role.Spec.RolloutStrategy.Type != leaderworkerset.RollingUpdateStrategyType {
		allErrs = append(allErrs, field.NotSupported(
			rolloutPath.Child("type"),
			role.Spec.RolloutStrategy.Type,
			[]string{string(leaderworkerset.RollingUpdateStrategyType), ""},
		))
	}

	if role.Spec.RolloutStrategy.RollingUpdateConfiguration != nil {
		rucPath := rolloutPath.Child("rollingUpdateConfiguration")
		ruc := role.Spec.RolloutStrategy.RollingUpdateConfiguration

		// Validate Partition - must not be set (DisaggregatedSet manages rollouts across roles)
		if ruc.Partition != nil && *ruc.Partition != 0 {
			allErrs = append(allErrs, field.Forbidden(
				rucPath.Child("partition"),
				"partition is not supported by DisaggregatedSet; rolling updates are managed across roles by the DisaggregatedSet controller",
			))
		}

		// Validate that maxSurge and maxUnavailable are not both zero when replicas > 0.
		// This mirrors the identical check in the LWS webhook (leaderworkerset_webhook.go).
		allErrs = append(allErrs, validateRoleMaxSurgeUnavailable(ruc, role.Spec.Replicas, rucPath)...)
	}

	return allErrs
}

// validateRoleMaxSurgeUnavailable rejects the combination maxSurge=0 + maxUnavailable=0
// for roles with at least one replica, matching the validation already present in the
// LWS webhook. Percentage values (e.g. "0%") are resolved via GetScaledValueFromIntOrPercent
// so they are caught as well.
func validateRoleMaxSurgeUnavailable(ruc *leaderworkerset.RollingUpdateConfiguration, replicas *int32, rucPath *field.Path) field.ErrorList {
	var allErrs field.ErrorList

	// Roles with zero (or unset) replicas are exempt – an all-zero DisaggregatedSet is
	// legitimate and accepted by the LWS webhook too.
	if replicas == nil || *replicas == 0 {
		return nil
	}
	replicaCount := int(*replicas)

	maxUnavailable := ruc.MaxUnavailable
	maxUnavailablePath := rucPath.Child("maxUnavailable")

	maxSurge := ruc.MaxSurge
	maxSurgePath := rucPath.Child("maxSurge")

	// Individual field validation (non-negative, ≤ 100%).
	allErrs = append(allErrs, webhooks.ValidatePositiveIntOrPercent(maxUnavailable, maxUnavailablePath)...)
	allErrs = append(allErrs, webhooks.IsNotMoreThan100Percent(maxUnavailable, maxUnavailablePath)...)
	allErrs = append(allErrs, webhooks.ValidatePositiveIntOrPercent(maxSurge, maxSurgePath)...)
	allErrs = append(allErrs, webhooks.IsNotMoreThan100Percent(maxSurge, maxSurgePath)...)

	if len(allErrs) > 0 {
		// Skip the combined check if individual fields are already invalid.
		return allErrs
	}

	maxUnavailableValue, err := intstr.GetScaledValueFromIntOrPercent(&maxUnavailable, replicaCount, false)
	if err != nil {
		allErrs = append(allErrs, field.Invalid(maxUnavailablePath, maxUnavailable, "invalid value"))
		return allErrs
	}
	maxSurgeValue, err := intstr.GetScaledValueFromIntOrPercent(&maxSurge, replicaCount, true)
	if err != nil {
		allErrs = append(allErrs, field.Invalid(maxSurgePath, maxSurge, "invalid value"))
		return allErrs
	}

	if maxUnavailableValue == 0 && maxSurgeValue == 0 {
		allErrs = append(allErrs, field.Invalid(
			maxUnavailablePath,
			maxUnavailable,
			"must not be 0 when `maxSurge` is 0",
		))
	}
	return allErrs
}

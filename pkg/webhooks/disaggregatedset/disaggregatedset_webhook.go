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
	allErrs := w.validateRoles(disagg)
	return nil, allErrs.ToAggregate()
}

// ValidateUpdate implements admission.Validator for update operations.
func (w *DisaggregatedSetWebhook) ValidateUpdate(ctx context.Context, oldDisagg, newDisagg *disaggv1.DisaggregatedSet) (admission.Warnings, error) {
	allErrs := w.validateRoles(newDisagg)
	return nil, allErrs.ToAggregate()
}

// ValidateDelete implements admission.Validator for delete operations.
func (w *DisaggregatedSetWebhook) ValidateDelete(ctx context.Context, disagg *disaggv1.DisaggregatedSet) (admission.Warnings, error) {
	return nil, nil
}

// validateRoles validates all roles in the DisaggregatedSet spec.
func (w *DisaggregatedSetWebhook) validateRoles(obj *disaggv1.DisaggregatedSet) field.ErrorList {
	var allErrs field.ErrorList
	rolesPath := field.NewPath("spec", "roles")

	for i, role := range obj.Spec.Roles {
		rolePath := rolesPath.Index(i)
		allErrs = append(allErrs, w.validateRoleRolloutStrategy(role, rolePath)...)
	}

	return allErrs
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

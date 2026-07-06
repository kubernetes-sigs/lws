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

	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	disaggv1 "sigs.k8s.io/lws/api/disaggregatedset/v1"
	leaderworkerset "sigs.k8s.io/lws/api/leaderworkerset/v1"
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
	allErrs = append(allErrs, w.validatePlacement(disagg)...)
	return nil, allErrs.ToAggregate()
}

// ValidateUpdate implements admission.Validator for update operations.
func (w *DisaggregatedSetWebhook) ValidateUpdate(ctx context.Context, oldDisagg, newDisagg *disaggv1.DisaggregatedSet) (admission.Warnings, error) {
	allErrs := w.validateRoles(newDisagg)
	allErrs = append(allErrs, w.validatePlacement(newDisagg)...)
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
		if roleHasExclusiveTopology(role) {
			allErrs = append(allErrs, field.Forbidden(
				rolesPath.Index(i),
				fmt.Sprintf("the %q annotation must not be combined with a non-None spec.placementPolicy.type (%s)",
					leaderworkerset.ExclusiveKeyAnnotationKey, policy.Type)))
		}
	}

	return allErrs
}

// roleHasExclusiveTopology reports whether a role carries the LWS group-level
// exclusive-topology annotation anywhere it takes effect: the LWS metadata, or the
// leader/worker pod templates (the LWS pod webhook reads it from the pod, so a
// template-level annotation would enable LWS exclusive placement too).
func roleHasExclusiveTopology(role disaggv1.DisaggregatedRoleSpec) bool {
	if _, ok := role.ObjectMeta.Annotations[leaderworkerset.ExclusiveKeyAnnotationKey]; ok {
		return true
	}
	template := role.Spec.LeaderWorkerTemplate
	if template.LeaderTemplate != nil {
		if _, ok := template.LeaderTemplate.Annotations[leaderworkerset.ExclusiveKeyAnnotationKey]; ok {
			return true
		}
	}
	if _, ok := template.WorkerTemplate.Annotations[leaderworkerset.ExclusiveKeyAnnotationKey]; ok {
		return true
	}
	return false
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

	// Validate Partition - must not be set (DisaggregatedSet manages rollouts across roles)
	if role.Spec.RolloutStrategy.RollingUpdateConfiguration != nil {
		if role.Spec.RolloutStrategy.RollingUpdateConfiguration.Partition != nil && *role.Spec.RolloutStrategy.RollingUpdateConfiguration.Partition != 0 {
			allErrs = append(allErrs, field.Forbidden(
				rolloutPath.Child("rollingUpdateConfiguration", "partition"),
				"partition is not supported by DisaggregatedSet; rolling updates are managed across roles by the DisaggregatedSet controller",
			))
		}
	}

	return allErrs
}

/*
Copyright 2026.

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

package webhook

import (
	"context"

	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
	leaderworkerset "sigs.k8s.io/lws/api/leaderworkerset/v1"

	disaggv1alpha1 "sigs.k8s.io/disaggregatedset/api/v1alpha1"
)

// DisaggregatedSetWebhook handles validation for DisaggregatedSet resources.
type DisaggregatedSetWebhook struct{}

// SetupDisaggregatedSetWebhook registers the webhook with the manager.
func SetupDisaggregatedSetWebhook(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr, &disaggv1alpha1.DisaggregatedSet{}).
		WithValidator(&DisaggregatedSetWebhook{}).
		Complete()
}

//+kubebuilder:webhook:path=/validate-disaggregatedset-x-k8s-io-v1alpha1-disaggregatedset,mutating=false,failurePolicy=fail,sideEffects=None,groups=disaggregatedset.x-k8s.io,resources=disaggregatedsets,verbs=create;update,versions=v1alpha1,name=vdisaggregatedset.kb.io,admissionReviewVersions=v1

var _ admission.Validator[*disaggv1alpha1.DisaggregatedSet] = &DisaggregatedSetWebhook{}

// ValidateCreate implements admission.Validator for create operations.
func (w *DisaggregatedSetWebhook) ValidateCreate(ctx context.Context, disagg *disaggv1alpha1.DisaggregatedSet) (admission.Warnings, error) {
	allErrs := w.validatePhases(disagg)
	return nil, allErrs.ToAggregate()
}

// ValidateUpdate implements admission.Validator for update operations.
func (w *DisaggregatedSetWebhook) ValidateUpdate(ctx context.Context, oldDisagg, newDisagg *disaggv1alpha1.DisaggregatedSet) (admission.Warnings, error) {
	allErrs := w.validatePhases(newDisagg)
	return nil, allErrs.ToAggregate()
}

// ValidateDelete implements admission.Validator for delete operations.
func (w *DisaggregatedSetWebhook) ValidateDelete(ctx context.Context, disagg *disaggv1alpha1.DisaggregatedSet) (admission.Warnings, error) {
	return nil, nil
}

// validatePhases validates all phases in the DisaggregatedSet spec.
func (w *DisaggregatedSetWebhook) validatePhases(obj *disaggv1alpha1.DisaggregatedSet) field.ErrorList {
	var allErrs field.ErrorList
	phasesPath := field.NewPath("spec", "phases")

	for i, phase := range obj.Spec.Phases {
		phasePath := phasesPath.Index(i)
		allErrs = append(allErrs, w.validatePhaseRolloutStrategy(phase, phasePath)...)
	}

	return allErrs
}

// validatePhaseRolloutStrategy validates the RolloutStrategy fields for a phase.
// DisaggregatedSet handles rolling updates differently from LWS and does not support:
// - RolloutStrategy.Type other than RollingUpdate (or empty, which defaults to RollingUpdate)
// - RolloutStrategy.RollingUpdateConfiguration.Partition
func (w *DisaggregatedSetWebhook) validatePhaseRolloutStrategy(phase disaggv1alpha1.DisaggregatedPhaseSpec, phasePath *field.Path) field.ErrorList {
	var allErrs field.ErrorList
	rolloutPath := phasePath.Child("rolloutStrategy")

	// Validate Type - must be empty or RollingUpdate
	if phase.RolloutStrategy.Type != "" && phase.RolloutStrategy.Type != leaderworkerset.RollingUpdateStrategyType {
		allErrs = append(allErrs, field.NotSupported(
			rolloutPath.Child("type"),
			phase.RolloutStrategy.Type,
			[]string{string(leaderworkerset.RollingUpdateStrategyType), ""},
		))
	}

	// Validate Partition - must not be set (DisaggregatedSet manages rollouts across phases)
	if phase.RolloutStrategy.RollingUpdateConfiguration != nil {
		if phase.RolloutStrategy.RollingUpdateConfiguration.Partition != nil && *phase.RolloutStrategy.RollingUpdateConfiguration.Partition != 0 {
			allErrs = append(allErrs, field.Forbidden(
				rolloutPath.Child("rollingUpdateConfiguration", "partition"),
				"partition is not supported by DisaggregatedSet; rolling updates are managed across phases by the DisaggregatedSet controller",
			))
		}
	}

	return allErrs
}

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

	apivalidation "k8s.io/apimachinery/pkg/api/validation"
	utilvalidation "k8s.io/apimachinery/pkg/util/validation"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	v1 "sigs.k8s.io/lws/api/leaderworkerset/v1"
)

type LeaderWorkerSetWebhook struct{}

// SetupLeaderWorkerSetWebhook will setup the manager to manage the webhooks
func SetupLeaderWorkerSetWebhook(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr).
		For(&v1.LeaderWorkerSet{}).
		WithDefaulter(&LeaderWorkerSetWebhook{}).
		WithValidator(&LeaderWorkerSetWebhook{}).
		Complete()
}

//+kubebuilder:webhook:path=/mutate-leaderworkerset-x-k8s-io-v1-leaderworkerset,mutating=true,failurePolicy=fail,sideEffects=None,groups=leaderworkerset.x-k8s.io,resources=leaderworkersets,verbs=create;update,versions=v1,name=mleaderworkerset.kb.io,admissionReviewVersions=v1

var _ webhook.CustomDefaulter = &LeaderWorkerSetWebhook{}

// Default implements webhook.Defaulter so a webhook will be registered for the type
func (r *LeaderWorkerSetWebhook) Default(ctx context.Context, obj runtime.Object) error {
	lws := obj.(*v1.LeaderWorkerSet)
	if lws.Spec.LeaderWorkerTemplate.RestartPolicy == "" {
		lws.Spec.LeaderWorkerTemplate.RestartPolicy = v1.DefaultRestartPolicy
	}

	if lws.Spec.RolloutStrategy.Type == "" {
		lws.Spec.RolloutStrategy.Type = v1.RollingUpdateStrategyType
	}

	if lws.Spec.RolloutStrategy.Type == v1.RollingUpdateStrategyType && lws.Spec.RolloutStrategy.RollingUpdateConfiguration == nil {
		lws.Spec.RolloutStrategy.RollingUpdateConfiguration = &v1.RollingUpdateConfiguration{
			MaxUnavailable: intstr.FromInt32(1),
			MaxSurge:       intstr.FromInt32(0),
		}
	}
	return nil
}

//+kubebuilder:webhook:path=/validate-leaderworkerset-x-k8s-io-v1-leaderworkerset,mutating=false,failurePolicy=fail,sideEffects=None,groups=leaderworkerset.x-k8s.io,resources=leaderworkersets,verbs=create;update,versions=v1,name=vleaderworkerset.kb.io,admissionReviewVersions=v1

var _ webhook.CustomValidator = &LeaderWorkerSetWebhook{}

// ValidateCreate implements webhook.Validator so a webhook will be registered for the type
func (r *LeaderWorkerSetWebhook) ValidateCreate(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	warnings, allErrs := r.generalValidate(obj)
	return warnings, allErrs.ToAggregate()
}

// ValidateUpdate implements webhook.Validator so a webhook will be registered for the type
func (r *LeaderWorkerSetWebhook) ValidateUpdate(ctx context.Context, oldObj, newObj runtime.Object) (admission.Warnings, error) {
	warnings, allErrs := r.generalValidate(newObj)

	oldLws := oldObj.(*v1.LeaderWorkerSet)
	newLws := newObj.(*v1.LeaderWorkerSet)
	allErrs = append(allErrs, apivalidation.ValidateImmutableField(*newLws.Spec.LeaderWorkerTemplate.Size, *oldLws.Spec.LeaderWorkerTemplate.Size, field.NewPath("spec", "leaderWorkerTemplate", "size"))...)
	return warnings, allErrs.ToAggregate()
}

// ValidateDelete implements webhook.Validator so a webhook will be registered for the type
func (r *LeaderWorkerSetWebhook) ValidateDelete(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	return nil, nil
}

func (r *LeaderWorkerSetWebhook) generalValidate(obj runtime.Object) (admission.Warnings, field.ErrorList) {
	lws := obj.(*v1.LeaderWorkerSet)
	specPath := field.NewPath("spec")

	var allErrs field.ErrorList
	// Ensure replicas and groups number are valid
	if lws.Spec.Replicas != nil && *lws.Spec.Replicas < 0 {
		allErrs = append(allErrs, field.Invalid(specPath.Child("replicas"), lws.Spec.Replicas, "replicas must be equal or greater than 0"))
	}
	if *lws.Spec.LeaderWorkerTemplate.Size < 1 {
		allErrs = append(allErrs, field.Invalid(specPath.Child("leaderWorkerTemplate", "size"), lws.Spec.LeaderWorkerTemplate.Size, "size must be equal or greater than 1"))
	}
	if int64(*lws.Spec.Replicas)*int64(*lws.Spec.LeaderWorkerTemplate.Size) > math.MaxInt32 {
		allErrs = append(allErrs, field.Invalid(specPath.Child("replicas"), lws.Spec.Replicas, fmt.Sprintf("the product of replicas and worker replicas must not exceed %d", math.MaxInt32)))
	}

	maxUnavailable := lws.Spec.RolloutStrategy.RollingUpdateConfiguration.MaxUnavailable
	maxUnavailablePath := specPath.Child("rolloutStrategy", "rollingUpdateConfiguration", "maxUnavailable")
	if lws.Spec.RolloutStrategy.RollingUpdateConfiguration != nil {
		allErrs = append(allErrs, validatePositiveIntOrPercent(maxUnavailable, maxUnavailablePath, false)...)
		// This is aligned with Statefulset.
		allErrs = append(allErrs, isNotMoreThan100Percent(maxUnavailable, maxUnavailablePath)...)
	}

	maxSurge := lws.Spec.RolloutStrategy.RollingUpdateConfiguration.MaxSurge
	maxSurgePath := specPath.Child("rolloutStrategy", "rollingUpdateConfiguration", "maxSurge")
	if lws.Spec.RolloutStrategy.RollingUpdateConfiguration != nil {
		allErrs = append(allErrs, validatePositiveIntOrPercent(maxSurge, maxSurgePath, true)...)
		allErrs = append(allErrs, isNotMoreThan100Percent(maxSurge, maxSurgePath)...)
	}
	return nil, allErrs
}

// This is mostly inspired by https://github.com/kubernetes/kubernetes/blob/be4b7176dc131ea842cab6882cd4a06dbfeed12a/pkg/apis/apps/validation/validation.go#L460,
// but it's not importable.

// validatePositiveIntOrPercent tests if a given value is a valid int or percentage.
func validatePositiveIntOrPercent(intOrPercent intstr.IntOrString, fldPath *field.Path, includingZero bool) field.ErrorList {
	allErrs := field.ErrorList{}
	switch intOrPercent.Type {
	case intstr.String:
		for _, msg := range utilvalidation.IsValidPercent(intOrPercent.StrVal) {
			allErrs = append(allErrs, field.Invalid(fldPath, intOrPercent, msg))
		}
	case intstr.Int:
		allErrs = append(allErrs, validateNonnegativeField(int64(intOrPercent.IntValue()), fldPath, includingZero)...)
	default:
		allErrs = append(allErrs, field.Invalid(fldPath, intOrPercent, "must be an integer or percentage (e.g '5%%')"))
	}
	return allErrs
}

// isNotMoreThan100Percent tests is a value can be represented as a percentage
// and if this value is not more than 100%.
func isNotMoreThan100Percent(intOrStringValue intstr.IntOrString, fldPath *field.Path) field.ErrorList {
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
func validateNonnegativeField(value int64, fldPath *field.Path, includingZero bool) field.ErrorList {
	allErrs := field.ErrorList{}
	if includingZero {
		if value < 0 {
			allErrs = append(allErrs, field.Invalid(fldPath, value, "must be grater than to equal to 0"))
		}
	} else {
		if value <= 0 {
			allErrs = append(allErrs, field.Invalid(fldPath, value, "must be grater than 0"))
		}
	}
	return allErrs
}

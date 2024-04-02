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

	apivalidation "k8s.io/apimachinery/pkg/api/validation"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/utils/ptr"
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
	if lws.Spec.Replicas == nil {
		lws.Spec.Replicas = ptr.To[int32](1)
	}
	if lws.Spec.LeaderWorkerTemplate.Size == nil {
		lws.Spec.LeaderWorkerTemplate.Size = ptr.To[int32](1)
	}
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
	warnings, allErrs := r.generalValidate(ctx, obj)
	return warnings, allErrs.ToAggregate()
}

// ValidateUpdate implements webhook.Validator so a webhook will be registered for the type
func (r *LeaderWorkerSetWebhook) ValidateUpdate(ctx context.Context, oldObj, newObj runtime.Object) (admission.Warnings, error) {
	warnings, allErrs := r.generalValidate(ctx, newObj)

	oldLws := oldObj.(*v1.LeaderWorkerSet)
	newLws := newObj.(*v1.LeaderWorkerSet)
	allErrs = append(allErrs, apivalidation.ValidateImmutableField(*newLws.Spec.LeaderWorkerTemplate.Size, *oldLws.Spec.LeaderWorkerTemplate.Size, field.NewPath("spec", "leaderWorkerTemplate", "size"))...)
	return warnings, allErrs.ToAggregate()
}

// ValidateDelete implements webhook.Validator so a webhook will be registered for the type
func (r *LeaderWorkerSetWebhook) ValidateDelete(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	return nil, nil
}

func (r *LeaderWorkerSetWebhook) generalValidate(ctx context.Context, obj runtime.Object) (admission.Warnings, field.ErrorList) {
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
	if lws.Spec.LeaderWorkerTemplate.RestartPolicy != v1.DefaultRestartPolicy && lws.Spec.LeaderWorkerTemplate.RestartPolicy != v1.RecreateGroupOnPodRestart {
		allErrs = append(allErrs, field.Invalid(specPath.Child("leaderWorkerTemplate", "restartPolicy"), lws.Spec.LeaderWorkerTemplate.RestartPolicy, "only support [Default, RecreateGroupOnPodRestart]"))

	}
	if lws.Spec.RolloutStrategy.RollingUpdateConfiguration != nil {
		value, err := intstr.GetScaledValueFromIntOrPercent(&lws.Spec.RolloutStrategy.RollingUpdateConfiguration.MaxUnavailable, int(*lws.Spec.Replicas), false)
		if err != nil || value > int(*lws.Spec.Replicas) {
			allErrs = append(allErrs, field.Invalid(specPath.Child("rolloutStrategy", "rollingUpdateConfiguration", "maxUnavailable"), lws.Spec.RolloutStrategy.RollingUpdateConfiguration.MaxUnavailable, "maxUnavailable must not greater than replicas"))
		}
	}
	return nil, allErrs
}

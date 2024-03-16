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
	"errors"
	"fmt"
	"math"

	apiequality "k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	v1 "sigs.k8s.io/lws/api/leaderworkerset/v1"
)

// log is for logging in this package.
var leaderworkersetlog = logf.Log.WithName("leaderworkerset-resource")

type LeaderWorkerSetWebhook struct{}

// SetupWebhookWithManager will setup the manager to manage the webhooks
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
			MaxSurge:       intstr.FromInt32(1),
		}
	}
	return nil
}

//+kubebuilder:webhook:path=/validate-leaderworkerset-x-k8s-io-v1-leaderworkerset,mutating=false,failurePolicy=fail,sideEffects=None,groups=leaderworkerset.x-k8s.io,resources=leaderworkersets,verbs=create;update,versions=v1,name=vleaderworkerset.kb.io,admissionReviewVersions=v1

var _ webhook.CustomValidator = &LeaderWorkerSetWebhook{}

// ValidateCreate implements webhook.Validator so a webhook will be registered for the type
func (r *LeaderWorkerSetWebhook) ValidateCreate(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	lws := obj.(*v1.LeaderWorkerSet)
	var allErrs []error
	// Ensure replicas and groups number are valid
	if lws.Spec.Replicas != nil && *lws.Spec.Replicas < 0 {
		allErrs = append(allErrs, fmt.Errorf("the Replicas %d is invalid", lws.Spec.Replicas))
	}
	if *lws.Spec.LeaderWorkerTemplate.Size < 1 {
		allErrs = append(allErrs, fmt.Errorf("the Size %d is invalid", lws.Spec.LeaderWorkerTemplate.Size))
	}
	if int64(*lws.Spec.Replicas)*int64(*lws.Spec.LeaderWorkerTemplate.Size) > math.MaxInt32 {
		allErrs = append(allErrs, fmt.Errorf("the product of replicas and worker replicas must not exceed %d for lws '%s'", math.MaxInt32, lws.Name))
	}
	if lws.Spec.LeaderWorkerTemplate.RestartPolicy != v1.DefaultRestartPolicy && lws.Spec.LeaderWorkerTemplate.RestartPolicy != v1.RecreateGroupOnPodRestart {
		allErrs = append(allErrs, fmt.Errorf("invalid value for leaderworkertemplate.restartpolicy %s", lws.Spec.LeaderWorkerTemplate.RestartPolicy))
	}
	return nil, errors.Join(allErrs...)
}

// ValidateUpdate implements webhook.Validator so a webhook will be registered for the type
func (r *LeaderWorkerSetWebhook) ValidateUpdate(ctx context.Context, oldObj, newObj runtime.Object) (admission.Warnings, error) {
	var allErrs []error
	newLws := newObj.(*v1.LeaderWorkerSet)
	oldLws := oldObj.(*v1.LeaderWorkerSet)
	if newLws.Spec.Replicas != nil && *newLws.Spec.Replicas < 0 {
		allErrs = append(allErrs, fmt.Errorf("the Replicas %d is invalid", newLws.Spec.Replicas))
	}
	newLwsClone := newLws.DeepCopy()
	// Replicas can be mutated
	newLwsClone.Spec.Replicas = oldLws.Spec.Replicas
	if !apiequality.Semantic.DeepEqual(oldLws.Spec, newLwsClone.Spec) {
		allErrs = append(allErrs, field.Forbidden(field.NewPath("spec"), "updates to leaderworkerset spec for fields other than 'replicas' are forbidden"))
	}
	return nil, errors.Join(allErrs...)
}

// ValidateDelete implements webhook.Validator so a webhook will be registered for the type
func (r *LeaderWorkerSetWebhook) ValidateDelete(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	return nil, nil
}

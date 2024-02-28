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

package v1

import (
	"errors"
	"fmt"
	"math"

	apiequality "k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/utils/pointer"
	ctrl "sigs.k8s.io/controller-runtime"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

// log is for logging in this package.
var leaderworkersetlog = logf.Log.WithName("leaderworkerset-resource")

// SetupWebhookWithManager will setup the manager to manage the webhooks
func (r *LeaderWorkerSet) SetupWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr).
		For(r).
		Complete()
}

//+kubebuilder:webhook:path=/mutate-leaderworkerset-x-k8s-io-v1-leaderworkerset,mutating=true,failurePolicy=fail,sideEffects=None,groups=leaderworkerset.x-k8s.io,resources=leaderworkersets,verbs=create;update,versions=v1,name=mleaderworkerset.kb.io,admissionReviewVersions=v1

var _ webhook.Defaulter = &LeaderWorkerSet{}

// Default implements webhook.Defaulter so a webhook will be registered for the type
func (r *LeaderWorkerSet) Default() {
	if r.Spec.Replicas == nil {
		r.Spec.Replicas = pointer.Int32(1)
	}
	if r.Spec.LeaderWorkerTemplate.Size == nil {
		r.Spec.LeaderWorkerTemplate.Size = pointer.Int32(1)
	}
	if r.Spec.LeaderWorkerTemplate.RestartPolicy == "" {
		r.Spec.LeaderWorkerTemplate.RestartPolicy = Default
	}
}

//+kubebuilder:webhook:path=/validate-leaderworkerset-x-k8s-io-v1-leaderworkerset,mutating=false,failurePolicy=fail,sideEffects=None,groups=leaderworkerset.x-k8s.io,resources=leaderworkersets,verbs=create;update,versions=v1,name=vleaderworkerset.kb.io,admissionReviewVersions=v1

var _ webhook.Validator = &LeaderWorkerSet{}

// ValidateCreate implements webhook.Validator so a webhook will be registered for the type
func (r *LeaderWorkerSet) ValidateCreate() (admission.Warnings, error) {
	var allErrs []error
	// Ensure replicas and groups number are valid
	if r.Spec.Replicas != nil && *r.Spec.Replicas < 0 {
		allErrs = append(allErrs, fmt.Errorf("the Replicas %d is invalid", r.Spec.Replicas))
	}
	if *r.Spec.LeaderWorkerTemplate.Size < 1 {
		allErrs = append(allErrs, fmt.Errorf("the Size %d is invalid", r.Spec.LeaderWorkerTemplate.Size))
	}
	if int64(*r.Spec.Replicas)*int64(*r.Spec.LeaderWorkerTemplate.Size) > math.MaxInt32 {
		allErrs = append(allErrs, fmt.Errorf("the product of replicas and worker replicas must not exceed %d for lws '%s'", math.MaxInt32, r.Name))
	}
	if r.Spec.LeaderWorkerTemplate.RestartPolicy != Default && r.Spec.LeaderWorkerTemplate.RestartPolicy != RecreateGroupOnPodRestart {
		allErrs = append(allErrs, fmt.Errorf("invalid value for leaderworkertemplate.restartpolicy %s", r.Spec.LeaderWorkerTemplate.RestartPolicy))
	}
	return nil, errors.Join(allErrs...)
}

// ValidateUpdate implements webhook.Validator so a webhook will be registered for the type
func (r *LeaderWorkerSet) ValidateUpdate(old runtime.Object) (admission.Warnings, error) {
	var allErrs []error
	oldLeaderWorkerSet := old.(*LeaderWorkerSet)
	if r.Spec.Replicas != nil && *r.Spec.Replicas < 0 {
		allErrs = append(allErrs, fmt.Errorf("the Replicas %d is invalid", r.Spec.Replicas))
	}
	newLeaderWorkerSetClone := r.DeepCopy()
	// Replicas can be mutated
	newLeaderWorkerSetClone.Spec.Replicas = oldLeaderWorkerSet.Spec.Replicas
	if !apiequality.Semantic.DeepEqual(oldLeaderWorkerSet.Spec, newLeaderWorkerSetClone.Spec) {
		allErrs = append(allErrs, field.Forbidden(field.NewPath("spec"), "updates to leaderworkerset spec for fields other than 'replicas' are forbidden"))
	}
	return nil, errors.Join(allErrs...)
}

// ValidateDelete implements webhook.Validator so a webhook will be registered for the type
func (r *LeaderWorkerSet) ValidateDelete() (admission.Warnings, error) {
	leaderworkersetlog.Info("validate delete", "name", r.Name)

	// TODO(user): fill in your validation logic upon object deletion.
	return nil, nil
}

/*
Copyright 2025.

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

package config

import (
	"strings"

	apimachineryvalidation "k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/utils/ptr"

	configapi "sigs.k8s.io/lws/api/config/v1alpha1"
	"sigs.k8s.io/lws/pkg/schedulerprovider"
)

var (
	internalCertManagementPath = field.NewPath("internalCertManagement")
	schedulerProviderPath      = field.NewPath("schedulerProvider")
)

func validate(c *configapi.Configuration) field.ErrorList {
	var allErrs field.ErrorList
	allErrs = append(allErrs, validateSchedulerProvider(c)...)
	allErrs = append(allErrs, validateInternalCertManagement(c)...)
	return allErrs
}

func validateSchedulerProvider(c *configapi.Configuration) field.ErrorList {
	var allErrs field.ErrorList
	if c.GangSchedulingManagement != nil {
		if c.GangSchedulingManagement.SchedulerProvider == nil || *c.GangSchedulingManagement.SchedulerProvider == "" {
			allErrs = append(allErrs, field.Required(schedulerProviderPath, "must be set when gang scheduling is enabled"))
			return allErrs
		}
		// Validate that the scheduler provider is in the supported list
		if !schedulerprovider.SupportedSchedulerProviders.Has(*c.GangSchedulingManagement.SchedulerProvider) {
			allErrs = append(allErrs, field.NotSupported(schedulerProviderPath, c.GangSchedulingManagement.SchedulerProvider, schedulerprovider.SupportedSchedulerProviders.UnsortedList()))
		}
	}
	return allErrs
}

func validateInternalCertManagement(c *configapi.Configuration) field.ErrorList {
	var allErrs field.ErrorList
	if c.InternalCertManagement == nil || !ptr.Deref(c.InternalCertManagement.Enable, false) {
		return allErrs
	}
	if svcName := c.InternalCertManagement.WebhookServiceName; svcName != nil {
		if errs := apimachineryvalidation.IsDNS1035Label(*svcName); len(errs) != 0 {
			allErrs = append(allErrs, field.Invalid(internalCertManagementPath.Child("webhookServiceName"), svcName, strings.Join(errs, ",")))
		}
	}
	if secName := c.InternalCertManagement.WebhookSecretName; secName != nil {
		if errs := apimachineryvalidation.IsDNS1123Subdomain(*secName); len(errs) != 0 {
			allErrs = append(allErrs, field.Invalid(internalCertManagementPath.Child("webhookSecretName"), secName, strings.Join(errs, ",")))
		}
	}
	return allErrs
}

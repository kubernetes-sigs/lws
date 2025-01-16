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
)

var (
	internalCertManagementPath = field.NewPath("internalCertManagement")
)

func validate(c *configapi.Configuration) field.ErrorList {
	var allErrs field.ErrorList
	allErrs = append(allErrs, validateInternalCertManagement(c)...)
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

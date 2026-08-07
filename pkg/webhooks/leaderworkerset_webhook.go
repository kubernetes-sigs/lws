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

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/apimachinery/pkg/util/version"
	"k8s.io/client-go/discovery"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
	"sigs.k8s.io/lws/pkg/features"

	v1 "sigs.k8s.io/lws/api/leaderworkerset/v1"
)

type LeaderWorkerSetWebhook struct {
	isK8s136Capable bool
}

// SetupLeaderWorkerSetWebhook will setup the manager to manage the webhooks
func SetupLeaderWorkerSetWebhook(mgr ctrl.Manager) error {
	discoveryClient, err := discovery.NewDiscoveryClientForConfig(mgr.GetConfig())
	isK8s136Capable := false
	if err == nil {
		serverVersion, err := discoveryClient.ServerVersion()
		if err == nil {
			parsedVersion, err := version.ParseGeneric(serverVersion.String())
			if err == nil {
				k8s136, _ := version.ParseGeneric("v1.36.0")
				if parsedVersion.AtLeast(k8s136) {
					isK8s136Capable = true
				}
			}
		}
	}

	webhook := &LeaderWorkerSetWebhook{
		isK8s136Capable: isK8s136Capable,
	}

	return ctrl.NewWebhookManagedBy(mgr, &v1.LeaderWorkerSet{}).
		WithDefaulter(webhook).
		WithValidator(webhook).
		Complete()
}

//+kubebuilder:webhook:path=/mutate-leaderworkerset-x-k8s-io-v1-leaderworkerset,mutating=true,failurePolicy=fail,sideEffects=None,groups=leaderworkerset.x-k8s.io,resources=leaderworkersets,verbs=create;update,versions=v1,name=mleaderworkerset.kb.io,admissionReviewVersions=v1

var _ admission.Defaulter[*v1.LeaderWorkerSet] = &LeaderWorkerSetWebhook{}

// Default implements admission.Defaulter[*v1.LeaderWorkerSet] so a webhook will be registered for the type
func (r *LeaderWorkerSetWebhook) Default(ctx context.Context, lws *v1.LeaderWorkerSet) error {
	if lws.Spec.LeaderWorkerTemplate.RestartPolicy == "" {
		lws.Spec.LeaderWorkerTemplate.RestartPolicy = v1.RecreateGroupOnPodRestart
	}

	if lws.Spec.LeaderWorkerTemplate.RestartPolicy == v1.DeprecatedDefaultRestartPolicy {
		lws.Spec.LeaderWorkerTemplate.RestartPolicy = v1.NoneRestartPolicy
	}

	if lws.Spec.RolloutStrategy.Type == "" {
		lws.Spec.RolloutStrategy.Type = v1.RollingUpdateStrategyType
	}

	if lws.Spec.RolloutStrategy.Type == v1.RollingUpdateStrategyType && lws.Spec.RolloutStrategy.RollingUpdateConfiguration == nil {
		lws.Spec.RolloutStrategy.RollingUpdateConfiguration = &v1.RollingUpdateConfiguration{
			MaxUnavailable: intstr.FromInt32(1),
			MaxSurge:       intstr.FromInt32(0),
			Partition:      ptr.To[int32](0),
		}
	}

	if lws.Spec.NetworkConfig == nil {
		lws.Spec.NetworkConfig = &v1.NetworkConfig{}
		subdomainPolicy := v1.SubdomainShared
		lws.Spec.NetworkConfig = &v1.NetworkConfig{
			SubdomainPolicy: &subdomainPolicy,
		}
	} else if lws.Spec.NetworkConfig.SubdomainPolicy == nil {
		subdomainPolicy := v1.SubdomainShared
		lws.Spec.NetworkConfig.SubdomainPolicy = &subdomainPolicy
	}

	return nil
}

//+kubebuilder:webhook:path=/validate-leaderworkerset-x-k8s-io-v1-leaderworkerset,mutating=false,failurePolicy=fail,sideEffects=None,groups=leaderworkerset.x-k8s.io,resources=leaderworkersets,verbs=create;update,versions=v1,name=vleaderworkerset.kb.io,admissionReviewVersions=v1

var _ admission.Validator[*v1.LeaderWorkerSet] = &LeaderWorkerSetWebhook{}

// ValidateCreate implements admission.Validator[*v1.LeaderWorkerSet] so a webhook will be registered for the type
func (r *LeaderWorkerSetWebhook) ValidateCreate(ctx context.Context, lws *v1.LeaderWorkerSet) (admission.Warnings, error) {
	allErrs := r.generalValidate(lws)
	return nil, allErrs.ToAggregate()
}

// ValidateUpdate implements admission.Validator[*v1.LeaderWorkerSet] so a webhook will be registered for the type
func (r *LeaderWorkerSetWebhook) ValidateUpdate(ctx context.Context, oldLws, newLws *v1.LeaderWorkerSet) (admission.Warnings, error) {
	allErrs := r.generalValidate(newLws)
	specPath := field.NewPath("spec")

	if newLws.Spec.LeaderWorkerTemplate.SubGroupPolicy != nil && oldLws.Spec.LeaderWorkerTemplate.SubGroupPolicy != nil {
		allErrs = append(allErrs, apivalidation.ValidateImmutableField(*newLws.Spec.LeaderWorkerTemplate.SubGroupPolicy.SubGroupSize, *oldLws.Spec.LeaderWorkerTemplate.SubGroupPolicy.SubGroupSize, field.NewPath("spec", "leaderWorkerTemplate", "SubGroupPolicy", "subGroupSize"))...)
	}
	if newLws.Spec.LeaderWorkerTemplate.SubGroupPolicy != nil && oldLws.Spec.LeaderWorkerTemplate.SubGroupPolicy == nil {
		allErrs = append(allErrs, field.Invalid(specPath.Child("leaderWorkerTemplate", "SubGroupPolicy", "subGroupSize"), newLws.Spec.LeaderWorkerTemplate.SubGroupPolicy.SubGroupSize, "cannot enable subGroupSize after the lws is already created"))
	}
	if newLws.Spec.LeaderWorkerTemplate.SubGroupPolicy == nil && oldLws.Spec.LeaderWorkerTemplate.SubGroupPolicy != nil {
		allErrs = append(allErrs, field.Invalid(specPath.Child("leaderWorkerTemplate", "SubGroupPolicy", "subGroupSize"), oldLws.Spec.LeaderWorkerTemplate.SubGroupPolicy.SubGroupSize, "cannot remove subGroupSize after enabled"))
	}
	if newLws.Spec.NetworkConfig != nil && newLws.Spec.NetworkConfig.SubdomainPolicy == nil {
		allErrs = append(allErrs, field.Invalid(specPath.Child("networkConfig", "subdomainPolicy"), oldLws.Spec.NetworkConfig.SubdomainPolicy, "cannot set subdomainPolicy as null"))
	}

	return nil, allErrs.ToAggregate()
}

// ValidateDelete implements admission.Validator[*v1.LeaderWorkerSet] so a webhook will be registered for the type
func (r *LeaderWorkerSetWebhook) ValidateDelete(ctx context.Context, lws *v1.LeaderWorkerSet) (admission.Warnings, error) {
	return nil, nil
}

func (r *LeaderWorkerSetWebhook) generalValidate(lws *v1.LeaderWorkerSet) field.ErrorList {
	specPath := field.NewPath("spec")
	metadataPath := field.NewPath("metadata")

	// Since the lws name is used as the name for headless service, it must be DNS-1035 compliant
	ValidateName := apivalidation.NameIsDNS1035Label
	allErrs := apivalidation.ValidateObjectMeta(&lws.ObjectMeta, true, apivalidation.ValidateNameFunc(ValidateName), field.NewPath("metadata"))
	// Ensure replicas and groups number are valid
	if lws.Spec.Replicas != nil {
		if *lws.Spec.Replicas < 0 {
			allErrs = append(allErrs, field.Invalid(specPath.Child("replicas"), lws.Spec.Replicas, "replicas must be equal or greater than 0"))
		}
		if *lws.Spec.Replicas > 1000000 {
			allErrs = append(allErrs, field.Invalid(specPath.Child("replicas"), lws.Spec.Replicas, "replicas must be equal or less than 1000000"))
		}
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
		allErrs = append(allErrs, ValidatePositiveIntOrPercent(maxUnavailable, maxUnavailablePath)...)
		// This is aligned with Statefulset.
		allErrs = append(allErrs, IsNotMoreThan100Percent(maxUnavailable, maxUnavailablePath)...)
	}

	maxSurge := lws.Spec.RolloutStrategy.RollingUpdateConfiguration.MaxSurge
	maxSurgePath := specPath.Child("rolloutStrategy", "rollingUpdateConfiguration", "maxSurge")
	if lws.Spec.RolloutStrategy.RollingUpdateConfiguration != nil {
		allErrs = append(allErrs, ValidatePositiveIntOrPercent(maxSurge, maxSurgePath)...)
		allErrs = append(allErrs, IsNotMoreThan100Percent(maxSurge, maxSurgePath)...)
	}

	if lws.Spec.RolloutStrategy.RollingUpdateConfiguration != nil {
		// Validate partition value
		partition := lws.Spec.RolloutStrategy.RollingUpdateConfiguration.Partition
		partitionPath := specPath.Child("rolloutStrategy", "rollingUpdateConfiguration", "partition")
		allErrs = append(allErrs, validateNonnegativeField(int64(*partition), partitionPath)...)
	}

	maxUnavailableValue, err := intstr.GetScaledValueFromIntOrPercent(&maxUnavailable, int(*lws.Spec.Replicas), false)
	if err != nil {
		allErrs = append(allErrs, field.Invalid(maxUnavailablePath, maxUnavailable, "invalid value"))
	}
	maxSurgeValue, err := intstr.GetScaledValueFromIntOrPercent(&maxSurge, int(*lws.Spec.Replicas), true)
	if err != nil {
		allErrs = append(allErrs, field.Invalid(maxSurgePath, maxSurge, "invalid value"))
	}
	if maxUnavailableValue == 0 && maxSurgeValue == 0 && *lws.Spec.Replicas != 0 {
		// Both MaxSurge and MaxUnavailable cannot be zero.
		allErrs = append(allErrs, field.Invalid(maxUnavailablePath, maxUnavailable, "must not be 0 when `maxSurge` is 0"))
	}

	if lws.Spec.LeaderWorkerTemplate.SubGroupPolicy != nil {
		allErrs = append(allErrs, validateUpdateSubGroupPolicy(specPath, lws)...)
	} else {
		if _, foundSubEpKey := lws.Annotations[v1.SubGroupExclusiveKeyAnnotationKey]; foundSubEpKey {
			allErrs = append(allErrs, field.Invalid(metadataPath.Child("annotations", v1.SubGroupExclusiveKeyAnnotationKey), lws.Annotations[v1.SubGroupExclusiveKeyAnnotationKey], "cannot have subgroup-exclusive-topology without subGroupSize set"))
		}
	}

	if lws.Spec.LeaderWorkerTemplate.RestartPolicy == v1.InPlaceGroupRestart {
		if !features.FeatureGate.Enabled(features.InPlaceGroupRestart) {
			allErrs = append(allErrs, field.Forbidden(specPath.Child("leaderWorkerTemplate", "restartPolicy"), "InPlaceGroupRestart feature gate is not enabled"))
		}
		if !r.isK8s136Capable {
			allErrs = append(allErrs, field.Forbidden(specPath.Child("leaderWorkerTemplate", "restartPolicy"), "InPlaceGroupRestart requires Kubernetes server version 1.36 or higher"))
		}
		if lws.Spec.StartupPolicy != v1.LeaderCreatedStartupPolicy {
			allErrs = append(allErrs, field.Invalid(specPath.Child("startupPolicy"), lws.Spec.StartupPolicy, "must be LeaderCreated when restartPolicy is InPlaceGroupRestart"))
		}

		if lws.Spec.LeaderWorkerTemplate.InPlaceGroupRestartConfig != nil {
			configPath := specPath.Child("leaderWorkerTemplate", "inPlaceGroupRestartConfig")
			config := lws.Spec.LeaderWorkerTemplate.InPlaceGroupRestartConfig

			// Validate Triggers and Rule Limits
			if len(config.Triggers) > 0 {
				triggerNames := make(map[string]bool)
				for i, trigger := range config.Triggers {
					triggerPath := configPath.Child("triggers").Index(i)
					key := string(trigger.Role) + "/" + trigger.ContainerName
					if triggerNames[key] {
						allErrs = append(allErrs, field.Duplicate(triggerPath, key))
					}
					triggerNames[key] = true

					for _, code := range trigger.RecoverableExitCodes {
						if code == 88 {
							allErrs = append(allErrs, field.Invalid(triggerPath.Child("recoverableExitCodes"), code, "exit code 88 is reserved for the lws-restart-agent and cannot be used by workloads"))
						}
					}
				}

				// Verify trigger containers exist
				for i, trigger := range config.Triggers {
					triggerPath := configPath.Child("triggers").Index(i)

					if trigger.Role == v1.LeaderRole || trigger.Role == v1.BothRole {
						if lws.Spec.LeaderWorkerTemplate.LeaderTemplate != nil {
							found := false
							for _, c := range lws.Spec.LeaderWorkerTemplate.LeaderTemplate.Spec.Containers {
								if c.Name == trigger.ContainerName {
									found = true
									break
								}
							}
							for _, c := range lws.Spec.LeaderWorkerTemplate.LeaderTemplate.Spec.InitContainers {
								if c.Name == trigger.ContainerName {
									found = true
									break
								}
							}
							if !found {
								allErrs = append(allErrs, field.Invalid(triggerPath.Child("containerName"), trigger.ContainerName, "container not found in LeaderTemplate"))
							}
						}
					}

					if trigger.Role == v1.WorkerRole || trigger.Role == v1.BothRole {
						found := false
						for _, c := range lws.Spec.LeaderWorkerTemplate.WorkerTemplate.Spec.Containers {
							if c.Name == trigger.ContainerName {
								found = true
								break
							}
						}
						for _, c := range lws.Spec.LeaderWorkerTemplate.WorkerTemplate.Spec.InitContainers {
							if c.Name == trigger.ContainerName {
								found = true
								break
							}
						}
						if !found {
							allErrs = append(allErrs, field.Invalid(triggerPath.Child("containerName"), trigger.ContainerName, "container not found in WorkerTemplate"))
						}
					}
				}

				// Verify reserved names and conflicting rules
				checkPodTemplate := func(podTemplate *corev1.PodTemplateSpec, role string) {
					if podTemplate == nil {
						return
					}
					path := specPath.Child("leaderWorkerTemplate", role)

					for _, vol := range podTemplate.Spec.Volumes {
						if vol.Name == "in-place-restart-state" || vol.Name == "lws-api" || vol.Name == "no-sa-token" {
							allErrs = append(allErrs, field.Invalid(path.Child("volumes"), vol.Name, "volume name is reserved for lws-restart-agent"))
						}
					}

					checkContainers := func(containers []corev1.Container, kind string) {
						for _, c := range containers {
							if c.Name == "lws-restart-marker" || c.Name == "lws-restart-agent" || c.Name == "lws-restart-barrier" {
								allErrs = append(allErrs, field.Invalid(path.Child(kind), c.Name, "container name is reserved for lws-restart-agent"))
							}
							for _, r := range c.RestartPolicyRules {
								if r.Action == corev1.ContainerRestartRuleActionRestartAllContainers {
									for _, code := range r.ExitCodes.Values {
										if code == 88 {
											allErrs = append(allErrs, field.Invalid(path.Child(kind), c.Name, "exit code 88 is reserved for lws-restart-agent in RestartPolicyRules"))
										}
									}
								}
							}
						}
					}
					checkContainers(podTemplate.Spec.Containers, "containers")
					checkContainers(podTemplate.Spec.InitContainers, "initContainers")
				}
				checkPodTemplate(lws.Spec.LeaderWorkerTemplate.LeaderTemplate, "leaderTemplate")
				checkPodTemplate(&lws.Spec.LeaderWorkerTemplate.WorkerTemplate, "workerTemplate")

				// Validate max 20 restart rules per container
				// The LWS controller injects rules per trigger, plus the agent gets 1.
				if lws.Spec.LeaderWorkerTemplate.LeaderTemplate != nil {
					for _, c := range lws.Spec.LeaderWorkerTemplate.LeaderTemplate.Spec.Containers {
						injected := 0
						for _, t := range config.Triggers {
							if (t.Role == v1.LeaderRole || t.Role == v1.BothRole) && t.ContainerName == c.Name {
								injected++
							}
						}
						if len(c.RestartPolicyRules)+injected > 20 {
							allErrs = append(allErrs, field.Invalid(specPath.Child("leaderWorkerTemplate", "leaderTemplate"), c.Name, "the total number of RestartPolicyRules (including injected triggers) cannot exceed 20 per container"))
						}
					}
					for _, c := range lws.Spec.LeaderWorkerTemplate.LeaderTemplate.Spec.InitContainers {
						injected := 0
						for _, t := range config.Triggers {
							if (t.Role == v1.LeaderRole || t.Role == v1.BothRole) && t.ContainerName == c.Name {
								injected++
							}
						}
						if len(c.RestartPolicyRules)+injected > 20 {
							allErrs = append(allErrs, field.Invalid(specPath.Child("leaderWorkerTemplate", "leaderTemplate"), c.Name, "the total number of RestartPolicyRules (including injected triggers) cannot exceed 20 per container"))
						}
					}
				}

				for _, c := range lws.Spec.LeaderWorkerTemplate.WorkerTemplate.Spec.Containers {
					injected := 0
					for _, t := range config.Triggers {
						if (t.Role == v1.WorkerRole || t.Role == v1.BothRole) && t.ContainerName == c.Name {
							injected++
						}
					}
					if len(c.RestartPolicyRules)+injected > 20 {
						allErrs = append(allErrs, field.Invalid(specPath.Child("leaderWorkerTemplate", "workerTemplate"), c.Name, "the total number of RestartPolicyRules (including injected triggers) cannot exceed 20 per container"))
					}
				}
				for _, c := range lws.Spec.LeaderWorkerTemplate.WorkerTemplate.Spec.InitContainers {
					injected := 0
					for _, t := range config.Triggers {
						if (t.Role == v1.WorkerRole || t.Role == v1.BothRole) && t.ContainerName == c.Name {
							injected++
						}
					}
					if len(c.RestartPolicyRules)+injected > 20 {
						allErrs = append(allErrs, field.Invalid(specPath.Child("leaderWorkerTemplate", "workerTemplate"), c.Name, "the total number of RestartPolicyRules (including injected triggers) cannot exceed 20 per container"))
					}
				}
			}
		}
	}
	return allErrs
}

// This is mostly inspired by https://github.com/kubernetes/kubernetes/blob/be4b7176dc131ea842cab6882cd4a06dbfeed12a/pkg/apis/apps/validation/validation.go#L460,
// but it's not importable.

// ValidatePositiveIntOrPercent tests if a given value is a valid int or percentage.
func ValidatePositiveIntOrPercent(intOrPercent intstr.IntOrString, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}
	switch intOrPercent.Type {
	case intstr.String:
		for _, msg := range utilvalidation.IsValidPercent(intOrPercent.StrVal) {
			allErrs = append(allErrs, field.Invalid(fldPath, intOrPercent, msg))
		}
	case intstr.Int:
		allErrs = append(allErrs, validateNonnegativeField(int64(intOrPercent.IntValue()), fldPath)...)
	default:
		allErrs = append(allErrs, field.Invalid(fldPath, intOrPercent, "must be an integer or percentage (e.g '5%%')"))
	}
	return allErrs
}

// IsNotMoreThan100Percent tests is a value can be represented as a percentage
// and if this value is not more than 100%.
func IsNotMoreThan100Percent(intOrStringValue intstr.IntOrString, fldPath *field.Path) field.ErrorList {
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
func validateNonnegativeField(value int64, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}
	if value < 0 {
		allErrs = append(allErrs, field.Invalid(fldPath, value, "must be greater than or equal to 0"))
	}
	return allErrs
}

func validateUpdateSubGroupPolicy(specPath *field.Path, lws *v1.LeaderWorkerSet) field.ErrorList {
	allErrs := field.ErrorList{}
	size := int32(*lws.Spec.LeaderWorkerTemplate.Size)
	subGroupSize := int32(*lws.Spec.LeaderWorkerTemplate.SubGroupPolicy.SubGroupSize)
	if subGroupSize < 1 {
		allErrs = append(allErrs, field.Invalid(specPath.Child("leaderWorkerTemplate", "SubGroupPolicy", "subGroupSize"), lws.Spec.LeaderWorkerTemplate.SubGroupPolicy.SubGroupSize, "subGroupSize must be equal or greater than 1"))
	}
	if (size%subGroupSize != 0) && ((size-1)%subGroupSize != 0) {
		allErrs = append(allErrs, field.Invalid(specPath.Child("leaderWorkerTemplate", "SubGroupPolicy", "subGroupSize"), lws.Spec.LeaderWorkerTemplate.SubGroupPolicy.SubGroupSize, "size or size - 1 must be divisible by subGroupSize"))
	}
	if size < subGroupSize {
		allErrs = append(allErrs, field.Invalid(specPath.Child("leaderWorkerTemplate", "SubGroupPolicy", "subGroupSize"), lws.Spec.LeaderWorkerTemplate.SubGroupPolicy.SubGroupSize, "subGroupSize cannot be larger than size"))
	}
	if lws.Spec.LeaderWorkerTemplate.SubGroupPolicy.Type != nil &&
		(*lws.Spec.LeaderWorkerTemplate.SubGroupPolicy.Type == v1.SubGroupPolicyTypeLeaderExcluded) &&
		((size-1)%subGroupSize != 0) {
		allErrs = append(allErrs, field.Invalid(specPath.Child("leaderWorkerTemplate", "SubGroupPolicy", "subGroupSize"), lws.Spec.LeaderWorkerTemplate.SubGroupPolicy.SubGroupSize, "size-1 must be divisible by subGroupSize when using LeaderExcluded"))
	}
	return allErrs
}

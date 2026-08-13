/*
Copyright 2026 The Kubernetes Authors.

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

package schedulerprovider

import (
	schedulingv1alpha3 "k8s.io/api/scheduling/v1alpha3"
	schedulingv1beta1 "k8s.io/api/scheduling/v1beta1"

	leaderworkerset "sigs.k8s.io/lws/api/leaderworkerset/v1"
)

// workloadBuilderInput contains the compatibility input required by the
// Kubernetes 1.37 workloadbuilder. LWS exposes v1beta1 scheduling configuration;
// the builder still accepts the former controller-facing v1alpha3 shapes.
type workloadBuilderInput struct {
	policy         *schedulingv1alpha3.WorkloadPodGroupSchedulingPolicy
	constraints    *schedulingv1alpha3.WorkloadPodGroupSchedulingConstraints
	disruptionMode *schedulingv1alpha3.WorkloadPodGroupDisruptionMode
	resourceClaims []schedulingv1alpha3.WorkloadPodGroupResourceClaim
}

func toWorkloadBuilderInput(config *leaderworkerset.LeaderWorkerSetSchedulingConfiguration) workloadBuilderInput {
	if config == nil {
		return workloadBuilderInput{}
	}

	return workloadBuilderInput{
		policy:         toWorkloadBuilderPolicy(config.SchedulingPolicy),
		constraints:    toWorkloadBuilderConstraints(config.SchedulingConstraints),
		disruptionMode: toWorkloadBuilderDisruptionMode(config.DisruptionMode),
		resourceClaims: toWorkloadBuilderResourceClaims(config.ResourceClaims),
	}
}

func toWorkloadBuilderPolicy(policy *schedulingv1beta1.PodGroupSchedulingPolicy) *schedulingv1alpha3.WorkloadPodGroupSchedulingPolicy {
	if policy == nil {
		return nil
	}

	converted := &schedulingv1alpha3.WorkloadPodGroupSchedulingPolicy{}
	if policy.Basic != nil {
		converted.Basic = &schedulingv1alpha3.WorkloadPodGroupBasicSchedulingPolicy{}
	}
	if policy.Gang != nil {
		minCount := policy.Gang.MinCount
		converted.Gang = &schedulingv1alpha3.WorkloadPodGroupGangSchedulingPolicy{}
		if minCount > 0 {
			converted.Gang.MinCount = &minCount
		}
	}
	return converted
}

func toWorkloadBuilderConstraints(constraints *schedulingv1beta1.PodGroupSchedulingConstraints) *schedulingv1alpha3.WorkloadPodGroupSchedulingConstraints {
	if constraints == nil {
		return nil
	}

	converted := &schedulingv1alpha3.WorkloadPodGroupSchedulingConstraints{
		Topology: make([]schedulingv1alpha3.TopologyConstraint, len(constraints.Topology)),
	}
	for i := range constraints.Topology {
		converted.Topology[i].Key = constraints.Topology[i].Key
	}
	return converted
}

func toWorkloadBuilderDisruptionMode(mode *schedulingv1beta1.DisruptionMode) *schedulingv1alpha3.WorkloadPodGroupDisruptionMode {
	if mode == nil {
		return nil
	}

	converted := &schedulingv1alpha3.WorkloadPodGroupDisruptionMode{}
	if mode.Single != nil {
		converted.Single = &schedulingv1alpha3.WorkloadPodGroupSingleDisruptionMode{}
	}
	if mode.All != nil {
		converted.All = &schedulingv1alpha3.WorkloadPodGroupAllDisruptionMode{}
	}
	return converted
}

func toWorkloadBuilderResourceClaims(claims []schedulingv1beta1.PodGroupResourceClaim) []schedulingv1alpha3.WorkloadPodGroupResourceClaim {
	if claims == nil {
		return nil
	}

	converted := make([]schedulingv1alpha3.WorkloadPodGroupResourceClaim, len(claims))
	for i := range claims {
		converted[i] = schedulingv1alpha3.WorkloadPodGroupResourceClaim{
			Name:                      claims[i].Name,
			ResourceClaimName:         claims[i].ResourceClaimName,
			ResourceClaimTemplateName: claims[i].ResourceClaimTemplateName,
		}
	}
	return converted
}

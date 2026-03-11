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

package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	leaderworkerset "sigs.k8s.io/lws/api/leaderworkerset/v1"
)

// NOTE: json tags are required.  Any new fields you add must have json tags for the fields to be serialized.

// ServiceTemplateMetadata defines metadata (labels and annotations) for the Service.
type ServiceTemplateMetadata struct {
	// Labels to add to the Service. These are merged with the auto-populated
	// labels (disaggregatedset.x-k8s.io/name, disaggregatedset.x-k8s.io/revision, disaggregatedset.x-k8s.io/side).
	// User-provided labels cannot override auto-populated labels.
	// +optional
	Labels map[string]string `json:"labels,omitempty"`

	// Annotations to add to the Service.
	// +optional
	Annotations map[string]string `json:"annotations,omitempty"`
}

// ServiceTemplate defines the template for creating a Service alongside the LWS.
// Services are only created when both prefill and decode sides are ready.
type ServiceTemplate struct {
	// Spec is the Service specification.
	// The selector will be auto-populated unless AutoPopulateSelector is set to false.
	// +required
	Spec corev1.ServiceSpec `json:"spec"`

	// Metadata specifies labels and annotations for the Service.
	// Labels from metadata.labels take precedence over the flat labels field.
	// +optional
	Metadata *ServiceTemplateMetadata `json:"metadata,omitempty"`

	// Labels to add to the Service. These are merged with the auto-populated
	// labels (disaggregatedset.x-k8s.io/name, disaggregatedset.x-k8s.io/revision, disaggregatedset.x-k8s.io/side).
	// User-provided labels cannot override auto-populated labels.
	// Deprecated: Use metadata.labels instead. This field is kept for backward compatibility.
	// +optional
	Labels map[string]string `json:"labels,omitempty"`

	// AutoPopulateSelector controls whether the selector is auto-populated
	// with disaggregatedset.x-k8s.io labels. Defaults to true.
	// Set to false if you want to manage the selector manually.
	// +kubebuilder:default=true
	// +optional
	AutoPopulateSelector *bool `json:"autoPopulateSelector,omitempty"`
}

// RolloutStrategy defines the rolling update parameters for a side.
type RolloutStrategy struct {
	// MaxUnavailable is the maximum number of replicas that can be unavailable during the update.
	// Value can be an absolute number (ex: 5) or a percentage of total replicas (ex: 10%).
	// Absolute number is calculated from percentage by rounding down.
	// Defaults to 0.
	// +kubebuilder:validation:XIntOrString
	// +kubebuilder:default=0
	// +optional
	MaxUnavailable *intstr.IntOrString `json:"maxUnavailable,omitempty"`

	// MaxSurge is the maximum number of replicas that can be scheduled above the original number of replicas.
	// Value can be an absolute number (ex: 5) or a percentage of total replicas (ex: 10%).
	// Absolute number is calculated from percentage by rounding up.
	// Defaults to 1.
	// +kubebuilder:validation:XIntOrString
	// +kubebuilder:default=1
	// +optional
	MaxSurge *intstr.IntOrString `json:"maxSurge,omitempty"`
}

// DisaggSideConfig defines the configuration for the prefill/decode side.
// This structure embeds LeaderWorkerSetSpec from sigs.k8s.io/lws and adds
// disagg-specific fields like ServiceTemplate.
type DisaggSideConfig struct {
	// Replicas is the number of leader-worker groups.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:default=1
	// +optional
	Replicas *int32 `json:"replicas,omitempty"`

	// LeaderWorkerTemplate defines the template for leader/worker pods.
	// +required
	LeaderWorkerTemplate leaderworkerset.LeaderWorkerTemplate `json:"leaderWorkerTemplate"`

	// RolloutStrategy defines the rolling update parameters.
	// +optional
	RolloutStrategy *RolloutStrategy `json:"rolloutStrategy,omitempty"`

	// StartupPolicy determines when workers should start relative to the leader.
	// +kubebuilder:default=LeaderCreated
	// +optional
	StartupPolicy leaderworkerset.StartupPolicyType `json:"startupPolicy,omitempty"`

	// NetworkConfig defines the network configuration of the group.
	// +optional
	NetworkConfig *leaderworkerset.NetworkConfig `json:"networkConfig,omitempty"`

	// ServiceTemplate defines an optional Service to create alongside the LWS.
	// Services are only created when both prefill and decode sides are ready.
	// +optional
	ServiceTemplate *ServiceTemplate `json:"serviceTemplate,omitempty"`
}

// DisaggregatedSetSpec defines the desired state of DisaggregatedSet
// +kubebuilder:validation:XValidation:rule="(self.prefill.replicas == 0 && self.decode.replicas == 0) || (self.prefill.replicas > 0 && self.decode.replicas > 0)",message="replicas must be zero for both sides or non-zero for both sides"
type DisaggregatedSetSpec struct {
	// Prefill configuration for the disaggregated deployment
	// +required
	Prefill *DisaggSideConfig `json:"prefill"`

	// Decode configuration for the disaggregated deployment
	// +required
	Decode *DisaggSideConfig `json:"decode"`
}

// DisaggregatedSetStatus defines the observed state of DisaggregatedSet.
type DisaggregatedSetStatus struct {
	// For Kubernetes API conventions, see:
	// https://github.com/kubernetes/community/blob/master/contributors/devel/sig-architecture/api-conventions.md#typical-status-properties

	// conditions represent the current state of the DisaggregatedSet resource.
	// Each condition has a unique type and reflects the status of a specific aspect of the resource.
	//
	// Standard condition types include:
	// - "Available": the resource is fully functional
	// - "Progressing": the resource is being created or updated
	// - "Degraded": the resource failed to reach or maintain its desired state
	//
	// The status of each condition is one of True, False, or Unknown.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status

// DisaggregatedSet is the Schema for the disaggregatedsets API
type DisaggregatedSet struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of DisaggregatedSet
	// +required
	Spec DisaggregatedSetSpec `json:"spec"`

	// status defines the observed state of DisaggregatedSet
	// +optional
	Status DisaggregatedSetStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// DisaggregatedSetList contains a list of DisaggregatedSet
type DisaggregatedSetList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []DisaggregatedSet `json:"items"`
}

func init() {
	SchemeBuilder.Register(&DisaggregatedSet{}, &DisaggregatedSetList{})
}

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

// +kubebuilder:skip
package computedomain

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

const (
	GroupName = "resource.nvidia.com"
	Version   = "v1beta1"
	Kind      = "ComputeDomain"
)

var (
	GroupVersion  = schema.GroupVersion{Group: GroupName, Version: Version}
	GroupKind     = schema.GroupKind{Group: GroupName, Kind: Kind}
	SchemeBuilder = runtime.NewSchemeBuilder(addKnownTypes)
	AddToScheme   = SchemeBuilder.AddToScheme
)

func addKnownTypes(scheme *runtime.Scheme) error {
	scheme.AddKnownTypes(GroupVersion,
		&ComputeDomain{},
		&ComputeDomainList{},
	)
	metav1.AddToGroupVersion(scheme, GroupVersion)
	return nil
}

// ComputeDomain is a minimal internal representation of the NVIDIA DRA
// ComputeDomain (resource.nvidia.com/v1beta1) custom resource, avoiding
// external Go module dependencies while allowing controller-runtime to use a
// typed informer cache instead of uncached unstructured reads.
type ComputeDomain struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec ComputeDomainSpec `json:"spec,omitempty"`
}

type ComputeDomainSpec struct {
	NumNodes int                       `json:"numNodes"`
	Channel  *ComputeDomainChannelSpec `json:"channel,omitempty"`
}

type ComputeDomainChannelSpec struct {
	AllocationMode        string                             `json:"allocationMode,omitempty"`
	ResourceClaimTemplate ComputeDomainResourceClaimTemplate `json:"resourceClaimTemplate"`
}

type ComputeDomainResourceClaimTemplate struct {
	Name string `json:"name"`
}

type ComputeDomainList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ComputeDomain `json:"items"`
}

func (in *ComputeDomain) DeepCopyInto(out *ComputeDomain) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	out.Spec.NumNodes = in.Spec.NumNodes
	if in.Spec.Channel != nil {
		channelCopy := *in.Spec.Channel
		out.Spec.Channel = &channelCopy
	}
}

func (in *ComputeDomain) DeepCopy() *ComputeDomain {
	if in == nil {
		return nil
	}
	out := new(ComputeDomain)
	in.DeepCopyInto(out)
	return out
}

func (in *ComputeDomain) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

func (in *ComputeDomainList) DeepCopyInto(out *ComputeDomainList) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ListMeta.DeepCopyInto(&out.ListMeta)
	if in.Items != nil {
		out.Items = make([]ComputeDomain, len(in.Items))
		for i := range in.Items {
			in.Items[i].DeepCopyInto(&out.Items[i])
		}
	}
}

func (in *ComputeDomainList) DeepCopy() *ComputeDomainList {
	if in == nil {
		return nil
	}
	out := new(ComputeDomainList)
	in.DeepCopyInto(out)
	return out
}

func (in *ComputeDomainList) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

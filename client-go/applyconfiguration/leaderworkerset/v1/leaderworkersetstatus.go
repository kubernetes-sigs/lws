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
// Code generated by applyconfiguration-gen. DO NOT EDIT.

package v1

import (
	metav1 "k8s.io/client-go/applyconfigurations/meta/v1"
)

// LeaderWorkerSetStatusApplyConfiguration represents a declarative configuration of the LeaderWorkerSetStatus type for use
// with apply.
type LeaderWorkerSetStatusApplyConfiguration struct {
	Conditions      []metav1.ConditionApplyConfiguration `json:"conditions,omitempty"`
	ReadyReplicas   *int32                               `json:"readyReplicas,omitempty"`
	UpdatedReplicas *int32                               `json:"updatedReplicas,omitempty"`
	Replicas        *int32                               `json:"replicas,omitempty"`
	HPAPodSelector  *string                              `json:"hpaPodSelector,omitempty"`
}

// LeaderWorkerSetStatusApplyConfiguration constructs a declarative configuration of the LeaderWorkerSetStatus type for use with
// apply.
func LeaderWorkerSetStatus() *LeaderWorkerSetStatusApplyConfiguration {
	return &LeaderWorkerSetStatusApplyConfiguration{}
}

// WithConditions adds the given value to the Conditions field in the declarative configuration
// and returns the receiver, so that objects can be build by chaining "With" function invocations.
// If called multiple times, values provided by each call will be appended to the Conditions field.
func (b *LeaderWorkerSetStatusApplyConfiguration) WithConditions(values ...*metav1.ConditionApplyConfiguration) *LeaderWorkerSetStatusApplyConfiguration {
	for i := range values {
		if values[i] == nil {
			panic("nil value passed to WithConditions")
		}
		b.Conditions = append(b.Conditions, *values[i])
	}
	return b
}

// WithReadyReplicas sets the ReadyReplicas field in the declarative configuration to the given value
// and returns the receiver, so that objects can be built by chaining "With" function invocations.
// If called multiple times, the ReadyReplicas field is set to the value of the last call.
func (b *LeaderWorkerSetStatusApplyConfiguration) WithReadyReplicas(value int32) *LeaderWorkerSetStatusApplyConfiguration {
	b.ReadyReplicas = &value
	return b
}

// WithUpdatedReplicas sets the UpdatedReplicas field in the declarative configuration to the given value
// and returns the receiver, so that objects can be built by chaining "With" function invocations.
// If called multiple times, the UpdatedReplicas field is set to the value of the last call.
func (b *LeaderWorkerSetStatusApplyConfiguration) WithUpdatedReplicas(value int32) *LeaderWorkerSetStatusApplyConfiguration {
	b.UpdatedReplicas = &value
	return b
}

// WithReplicas sets the Replicas field in the declarative configuration to the given value
// and returns the receiver, so that objects can be built by chaining "With" function invocations.
// If called multiple times, the Replicas field is set to the value of the last call.
func (b *LeaderWorkerSetStatusApplyConfiguration) WithReplicas(value int32) *LeaderWorkerSetStatusApplyConfiguration {
	b.Replicas = &value
	return b
}

// WithHPAPodSelector sets the HPAPodSelector field in the declarative configuration to the given value
// and returns the receiver, so that objects can be built by chaining "With" function invocations.
// If called multiple times, the HPAPodSelector field is set to the value of the last call.
func (b *LeaderWorkerSetStatusApplyConfiguration) WithHPAPodSelector(value string) *LeaderWorkerSetStatusApplyConfiguration {
	b.HPAPodSelector = &value
	return b
}

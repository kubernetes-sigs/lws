/*
Copyright 2023 The Kubernetes Authors.
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
package testutils

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"

	leaderworkerset "sigs.k8s.io/lws/api/leaderworkerset/v1"
)

type LeaderWorkerSetWrapper struct {
	leaderworkerset.LeaderWorkerSet
}

func (lwsWrapper *LeaderWorkerSetWrapper) Obj() *leaderworkerset.LeaderWorkerSet {
	return &lwsWrapper.LeaderWorkerSet
}

func (lwsWrapper *LeaderWorkerSetWrapper) Replica(count int) *LeaderWorkerSetWrapper {
	lwsWrapper.Spec.Replicas = ptr.To[int32](int32(count))
	return lwsWrapper
}

func (lwsWrapper *LeaderWorkerSetWrapper) MaxUnavailable(value int) *LeaderWorkerSetWrapper {
	lwsWrapper.Spec.RolloutStrategy.RollingUpdateConfiguration.MaxUnavailable = intstr.FromInt(value)
	return lwsWrapper
}

func (lwsWrapper *LeaderWorkerSetWrapper) Size(count int) *LeaderWorkerSetWrapper {
	lwsWrapper.Spec.LeaderWorkerTemplate.Size = ptr.To[int32](int32(count))
	return lwsWrapper
}

func (lwsWrapper *LeaderWorkerSetWrapper) WorkerTemplateSpec(spec corev1.PodSpec) *LeaderWorkerSetWrapper {
	lwsWrapper.Spec.LeaderWorkerTemplate.WorkerTemplate.Spec = spec
	return lwsWrapper
}

func (lwsWrapper *LeaderWorkerSetWrapper) LeaderTemplateSpec(spec corev1.PodSpec) *LeaderWorkerSetWrapper {
	if lwsWrapper.Spec.LeaderWorkerTemplate.LeaderTemplate == nil {
		lwsWrapper.Spec.LeaderWorkerTemplate.LeaderTemplate = &corev1.PodTemplateSpec{}
	}
	lwsWrapper.Spec.LeaderWorkerTemplate.LeaderTemplate.Spec = spec
	return lwsWrapper
}

func (lwsWrapper *LeaderWorkerSetWrapper) ExclusivePlacement() *LeaderWorkerSetWrapper {
	lwsWrapper.Annotations = map[string]string{}
	lwsWrapper.Annotations[leaderworkerset.ExclusiveKeyAnnotationKey] = "cloud.google.com/gke-nodepool"
	return lwsWrapper
}

func (lwsWrapper *LeaderWorkerSetWrapper) RestartPolicy(policy leaderworkerset.RestartPolicyType) *LeaderWorkerSetWrapper {
	lwsWrapper.Spec.LeaderWorkerTemplate.RestartPolicy = policy
	return lwsWrapper
}

func (lwsWrapper *LeaderWorkerSetWrapper) RolloutStrategy(strategy leaderworkerset.RolloutStrategy) *LeaderWorkerSetWrapper {
	lwsWrapper.Spec.RolloutStrategy = strategy
	return lwsWrapper
}

func (lwsWrapper *LeaderWorkerSetWrapper) Annotation(annotations map[string]string) *LeaderWorkerSetWrapper {
	lwsWrapper.Annotations = annotations
	return lwsWrapper
}

func (lwsWrapper *LeaderWorkerSetWrapper) Conditions(conditions []metav1.Condition) *LeaderWorkerSetWrapper {
	lwsWrapper.Status.Conditions = conditions
	return lwsWrapper
}

func BuildBasicLeaderWorkerSet(name, ns string) *LeaderWorkerSetWrapper {
	return &LeaderWorkerSetWrapper{
		leaderworkerset.LeaderWorkerSet{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: ns,
			},
			Spec: leaderworkerset.LeaderWorkerSetSpec{
				LeaderWorkerTemplate: leaderworkerset.LeaderWorkerTemplate{},
			},
		},
	}
}

func BuildLeaderWorkerSet(nsName string) *LeaderWorkerSetWrapper {
	lws := leaderworkerset.LeaderWorkerSet{}
	lws.Name = "test-sample"
	lws.Namespace = nsName
	lws.Spec = leaderworkerset.LeaderWorkerSetSpec{}
	lws.Spec.Replicas = ptr.To[int32](2)
	lws.Spec.LeaderWorkerTemplate = leaderworkerset.LeaderWorkerTemplate{RestartPolicy: leaderworkerset.DefaultRestartPolicy}
	lws.Spec.LeaderWorkerTemplate.Size = ptr.To[int32](2)
	lws.Spec.LeaderWorkerTemplate.LeaderTemplate = &corev1.PodTemplateSpec{}
	lws.Spec.LeaderWorkerTemplate.LeaderTemplate.Spec = MakeLeaderPodSpec()
	lws.Spec.LeaderWorkerTemplate.WorkerTemplate.Spec = MakeWorkerPodSpec()
	// Manually set this for we didn't enable webhook in controller tests.
	lws.Spec.RolloutStrategy = leaderworkerset.RolloutStrategy{
		Type: leaderworkerset.RollingUpdateStrategyType,
		RollingUpdateConfiguration: &leaderworkerset.RollingUpdateConfiguration{
			MaxUnavailable: intstr.FromInt32(1),
			MaxSurge:       intstr.FromInt32(0),
		},
	}
	return &LeaderWorkerSetWrapper{
		lws,
	}
}

func MakeWorkerPodSpec() corev1.PodSpec {
	return corev1.PodSpec{
		Containers: []corev1.Container{
			{
				Name:  "leader",
				Image: "nginx:1.14.2",
				Ports: []corev1.ContainerPort{
					{
						ContainerPort: 8080,
						Protocol:      "TCP",
					},
				},
			},
		},
	}
}

func MakeWorkerPodSpecWithTPUResource() corev1.PodSpec {
	return corev1.PodSpec{
		Containers: []corev1.Container{
			{
				Name:  "leader",
				Image: "nginx:1.14.2",
				Ports: []corev1.ContainerPort{
					{
						ContainerPort: 8080,
						Protocol:      "TCP",
					},
				},
				Resources: corev1.ResourceRequirements{
					Requests: map[corev1.ResourceName]resource.Quantity{
						corev1.ResourceName("google.com/tpu"): resource.MustParse("4"),
					},
					Limits: map[corev1.ResourceName]resource.Quantity{
						corev1.ResourceName("google.com/tpu"): resource.MustParse("4"),
					},
				},
			},
		},
		Subdomain: "default",
	}
}

func MakeLeaderPodSpec() corev1.PodSpec {
	return corev1.PodSpec{
		Containers: []corev1.Container{
			{
				Name:  "worker",
				Image: "nginx:1.14.2",
			},
		},
	}
}

func MakeLeaderPodSpecWithTPUResource() corev1.PodSpec {
	return corev1.PodSpec{
		Containers: []corev1.Container{
			{
				Name:  "worker",
				Image: "busybox",
				Resources: corev1.ResourceRequirements{
					Limits: map[corev1.ResourceName]resource.Quantity{
						corev1.ResourceName("google.com/tpu"): resource.MustParse("4"),
					},
				},
			},
		},
		Subdomain: "default",
	}
}

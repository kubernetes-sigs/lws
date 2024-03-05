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

package accelerator

import (
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"

	leaderworkerset "sigs.k8s.io/lws/api/leaderworkerset/v1"
	statefulsetutils "sigs.k8s.io/lws/pkg/utils/statefulset"
)

var (
	TpuResourceName    = corev1.ResourceName("google.com/tpu")
	TpuWorkerHostNames = "TPU_WORKER_HOSTNAMES"
	TpuWorkerId        = "TPU_WORKER_ID"
)

// PodRequestsTPUs returns true if the pod requesting TPUs
func PodRequestsTPUs(podTs corev1.PodSpec) bool {
	return containersRequestTPUs(podTs.Containers...) || containersRequestTPUs(podTs.InitContainers...)
}

// numTPUsRequested returns the number of requested TPUs
func numTPUsRequested(container corev1.Container) int64 {
	if l := container.Resources.Limits; l != nil {
		if resource := l[TpuResourceName]; !resource.IsZero() {
			return resource.Value()
		}
	}
	if r := container.Resources.Requests; r != nil {
		if resource := r[TpuResourceName]; !resource.IsZero() {
			return resource.Value()
		}
	}
	return 0
}

// containersRequestTPUs returns true if the container requests TPUs
func containersRequestTPUs(containers ...corev1.Container) bool {
	for _, container := range containers {
		if numTPUsRequested(container) != 0 {
			return true
		}
	}
	return false
}

// getContainerRequestingTPUs returns the container that requests TPUs
// Assumption is that only one container on a pod will be requesting TPU resource.
func getContainerRequestingTPUs(spec *corev1.PodSpec) *corev1.Container {
	for i, container := range spec.Containers {
		if containersRequestTPUs(container) {
			return &spec.Containers[i]
		}
	}
	for i, container := range spec.InitContainers {
		if containersRequestTPUs(container) {
			return &spec.InitContainers[i]
		}
	}
	return nil
}

// AddTPUVariables adds TPU related environment variables to containers
func AddTPUVariables(pod *corev1.Pod, size int) error {
	container := getContainerRequestingTPUs(&pod.Spec)
	if container == nil {
		return nil
	}
	for _, env := range container.Env {
		if env.Name == TpuWorkerHostNames || env.Name == TpuWorkerId {
			return nil
		}
	}
	var hostnames []string
	if workerIndex, found := pod.Labels[leaderworkerset.WorkerIndexLabelKey]; found {
		leaderName := ""
		if workerIndex == "0" {
			// hostname for leader pod, leaderPodName.Subdomain
			hostnames = append(hostnames, fmt.Sprintf("%s.%s", pod.Name, pod.Spec.Subdomain))
			leaderName = pod.Name
		} else {
			// hostname for leader pod
			leaderName, _ = statefulsetutils.GetParentNameAndOrdinal(pod.Name)
			if leaderName == "" {
				return fmt.Errorf("parsing parent name from pod %s", pod.Name)
			}
			hostnames = append(hostnames, fmt.Sprintf("%s.%s", leaderName, pod.Spec.Subdomain))
		}
		for i := 1; i <= size-1; i++ {
			// hostname for worker pod, leaderPodName-Index.Subdomain
			hostnames = append(hostnames, fmt.Sprintf("%s-%d.%s", leaderName, i, pod.Spec.Subdomain))
		}
	} else {
		// if worker identity label is missing, do nothing
		return nil
	}
	container.Env = append(container.Env,
		corev1.EnvVar{
			Name:  TpuWorkerHostNames,
			Value: strings.Join(hostnames[:], ","),
		},
		corev1.EnvVar{
			Name:  TpuWorkerId,
			Value: pod.Labels[leaderworkerset.WorkerIndexLabelKey],
		},
	)
	return nil
}

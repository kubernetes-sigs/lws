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
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"

	leaderworkerset "sigs.k8s.io/lws/api/leaderworkerset/v1"

	statefulsetutils "sigs.k8s.io/lws/pkg/utils/statefulset"
)

const (
	TpuResourceName                 corev1.ResourceName = corev1.ResourceName("google.com/tpu")
	TpuWorkerHostNames              string              = "TPU_WORKER_HOSTNAMES"
	TpuWorkerId                     string              = "TPU_WORKER_ID"
	LeaderRequestsTPUsAnnotationKey string              = "leaderworkerset.sigs.k8s.io/leader-requests-tpus"
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

func addTPUVariablesSubGroup(pod *corev1.Pod, size int) error {
	container := getContainerRequestingTPUs(&pod.Spec)
	if container == nil {
		return nil
	}

	for _, env := range container.Env {
		// The assumption is that other env vars are added as well
		if env.Name == TpuWorkerHostNames || env.Name == TpuWorkerId {
			return nil
		}
	}

	leaderName := pod.Name
	subGroupSize, err := strconv.Atoi(pod.Annotations[leaderworkerset.SubGroupSizeAnnotationKey])
	if err != nil {
		return err
	}

	subGroupIndex, err := strconv.Atoi(pod.Labels[leaderworkerset.SubGroupIndexLabelKey])
	if err != nil {
		return err
	}

	workerIndex, err := strconv.Atoi(pod.Labels[leaderworkerset.WorkerIndexLabelKey])
	if err != nil {
		return err
	}
	tpuWorkerId := (workerIndex) % subGroupSize

	if pod.Annotations[LeaderRequestsTPUsAnnotationKey] != "true" {
		tpuWorkerId = (workerIndex - 1) % subGroupSize
	}

	start := subGroupSize*subGroupIndex + 1
	end := subGroupSize * (subGroupIndex + 1)
	var hostnames []string

	if pod.Labels[leaderworkerset.WorkerIndexLabelKey] == "0" {
		// The leader requests TPU resources, so it should be included in hostnames.
		hostnames = append(hostnames, fmt.Sprintf("%s.%s", leaderName, pod.Spec.Subdomain))
		end -= 1
	} else {
		leaderName, _ = statefulsetutils.GetParentNameAndOrdinal(pod.Name)
		if leaderName == "" {
			return fmt.Errorf("parsing parent name from pod %s", pod.Name)
		}
		if pod.Annotations[LeaderRequestsTPUsAnnotationKey] == "true" && subGroupIndex == 0 {
			// SubGroup 0 contains the leader, and the leader is requesting TPU resources, so
			// the hostname list should shift to the left by one
			end -= 1
			hostnames = append(hostnames, fmt.Sprintf("%s.%s", leaderName, pod.Spec.Subdomain))
		} else if pod.Annotations[LeaderRequestsTPUsAnnotationKey] == "true" {
			// Since the first subGroup has been shifted to the left by one, all other subsequent
			// subGroups should be shifted as well
			start -= 1
			end -= 1
		}
	}

	for i := start; i <= end; i++ {
		hostnames = append(hostnames, fmt.Sprintf("%s-%d.%s", leaderName, i, pod.Spec.Subdomain))
	}

	container.Env = append(container.Env,
		corev1.EnvVar{
			Name:  TpuWorkerHostNames,
			Value: strings.Join(hostnames[:], ","),
		},
		corev1.EnvVar{
			Name:  TpuWorkerId,
			Value: fmt.Sprint(tpuWorkerId),
		},
	)
	return nil

}

// AddTPUVariables adds TPU related environment variables to containers
func AddTPUVariables(pod *corev1.Pod, size int) error {
	_, foundSubGroupSize := pod.Annotations[leaderworkerset.SubGroupSizeAnnotationKey]
	if foundSubGroupSize {
		return addTPUVariablesSubGroup(pod, size)
	}
	container := getContainerRequestingTPUs(&pod.Spec)
	if container == nil {
		return nil
	}
	for _, env := range container.Env {
		// The assumption is that other env vars are added as well
		if env.Name == TpuWorkerHostNames || env.Name == TpuWorkerId {
			return nil
		}
	}

	leaderName := pod.Name
	tpuWorkerId := 0
	var hostnames []string
	if pod.Labels[leaderworkerset.WorkerIndexLabelKey] == "0" {
		// if this is a leader, then we know it is requesting TPUs, and the leader will get TPU_WORKER_ID=0
		hostnames = append(hostnames, fmt.Sprintf("%s.%s", leaderName, pod.Spec.Subdomain))
	} else {
		leaderName, tpuWorkerId = statefulsetutils.GetParentNameAndOrdinal(pod.Name)
		if leaderName == "" {
			return fmt.Errorf("parsing parent name from pod %s", pod.Name)
		}
		if pod.Annotations[LeaderRequestsTPUsAnnotationKey] == "true" {
			// The leader requests TPUs, and so it will be added to the hostnames and will get TPU_WORKER_ID=0
			hostnames = append(hostnames, fmt.Sprintf("%s.%s", leaderName, pod.Spec.Subdomain))
		} else {
			// The leader doesn't request TPUs, and so it is only the workers that will be assigned
			// TPU_WORKER_ID, and so we have to shift the IDs by 1 since the leader is not a TPU worker.
			tpuWorkerId = tpuWorkerId - 1
		}
	}

	for i := 1; i <= size-1; i++ {
		// hostname for worker pod, leaderPodName-Index.Subdomain
		hostnames = append(hostnames, fmt.Sprintf("%s-%d.%s", leaderName, i, pod.Spec.Subdomain))
	}

	container.Env = append(container.Env,
		corev1.EnvVar{
			Name:  TpuWorkerHostNames,
			Value: strings.Join(hostnames[:], ","),
		},
		corev1.EnvVar{
			Name:  TpuWorkerId,
			Value: fmt.Sprint(tpuWorkerId),
		},
	)
	return nil
}

// AddTPUAnnotations adds TPU specific annotations.
func AddTPUAnnotations(leaderPod corev1.Pod, annotations map[string]string) {
	if PodRequestsTPUs(leaderPod.Spec) {
		annotations[LeaderRequestsTPUsAnnotationKey] = "true"
	}
}

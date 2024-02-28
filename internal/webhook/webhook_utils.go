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

package webhook

import (
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	leaderworkerset "sigs.k8s.io/leader-worker-set/api/leaderworkerset/v1"
	commonutils "sigs.k8s.io/leader-worker-set/internal/commonutils"
)

var (
	TpuResourceName    = corev1.ResourceName("google.com/tpu")
	TpuWorkerHostNames = "TPU_WORKER_HOSTNAMES"
	TpuWorkerId        = "TPU_WORKER_ID"
)

// podRequestsTPUs returns true if the pod requesting TPUs
func podRequestsTPUs(podTs corev1.PodSpec) bool {
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

// addTPUVariables adds TPU related environment variables to containers
func addTPUVariables(pod *corev1.Pod, size int) error {
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
			leaderName, _ = commonutils.GetParentNameAndOrdinal(pod.Name)
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

func genGroupUniqueKey(ns string, podName string) string {
	return sha1Hash(fmt.Sprintf("%s/%s", ns, podName))
}

// sha1Hash accepts an input string and returns the 40 character SHA1 hash digest of the input string.
func sha1Hash(s string) string {
	h := sha1.New()
	h.Write([]byte(s))
	return hex.EncodeToString(h.Sum(nil))
}

// SetExclusiveAffinities set the node affinity/anti-affinity for the leader pod
func SetExclusiveAffinities(pod *corev1.Pod, groupUniqueKey string) {
	if pod.Spec.Affinity == nil {
		pod.Spec.Affinity = &corev1.Affinity{}
	}
	if pod.Spec.Affinity.PodAffinity == nil {
		pod.Spec.Affinity.PodAffinity = &corev1.PodAffinity{}
	}
	if pod.Spec.Affinity.PodAntiAffinity == nil {
		pod.Spec.Affinity.PodAntiAffinity = &corev1.PodAntiAffinity{}
	}
	// Pod affinity ensures the pods of this set land on the same topology domain.
	pod.Spec.Affinity.PodAffinity.RequiredDuringSchedulingIgnoredDuringExecution = append(pod.Spec.Affinity.PodAffinity.RequiredDuringSchedulingIgnoredDuringExecution,
		corev1.PodAffinityTerm{
			LabelSelector: &metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{
				{
					Key:      leaderworkerset.GroupUniqueHashLabelKey,
					Operator: metav1.LabelSelectorOpIn,
					Values:   []string{groupUniqueKey},
				},
			}},
			TopologyKey: pod.Annotations[leaderworkerset.ExclusiveKeyAnnotationKey],
		})
	// Pod anti-affinity ensures exclusively this set lands on the topology, preventing multiple sets per topology domain.
	pod.Spec.Affinity.PodAntiAffinity.RequiredDuringSchedulingIgnoredDuringExecution = append(pod.Spec.Affinity.PodAntiAffinity.RequiredDuringSchedulingIgnoredDuringExecution,
		corev1.PodAffinityTerm{
			LabelSelector: &metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{
				{
					Key:      leaderworkerset.GroupUniqueHashLabelKey,
					Operator: metav1.LabelSelectorOpExists,
				},
				{
					Key:      leaderworkerset.GroupUniqueHashLabelKey,
					Operator: metav1.LabelSelectorOpNotIn,
					Values:   []string{groupUniqueKey},
				},
			}},
			TopologyKey: pod.Annotations[leaderworkerset.ExclusiveKeyAnnotationKey],
		})
}

// exclusiveAffinityApplied return true if the exclusive placement terms have been applied
func exclusiveAffinityApplied(pod corev1.Pod) bool {
	if pod.Spec.Affinity == nil || pod.Spec.Affinity.PodAffinity == nil || pod.Spec.Affinity.PodAntiAffinity == nil {
		return false
	}
	hasAffinity := false
	hasAntiAffinity := false
	for _, podAffinityTerm := range pod.Spec.Affinity.PodAffinity.RequiredDuringSchedulingIgnoredDuringExecution {
		if podAffinityTerm.TopologyKey == pod.Annotations[leaderworkerset.ExclusiveKeyAnnotationKey] {
			hasAffinity = true
		}
	}
	for _, podAntiahasAntiAffinity := range pod.Spec.Affinity.PodAntiAffinity.RequiredDuringSchedulingIgnoredDuringExecution {
		if podAntiahasAntiAffinity.TopologyKey == pod.Annotations[leaderworkerset.ExclusiveKeyAnnotationKey] {
			hasAntiAffinity = true
		}
	}
	return hasAffinity && hasAntiAffinity
}

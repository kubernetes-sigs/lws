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

package pod

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"

	leaderworkerset "sigs.k8s.io/lws/api/leaderworkerset/v1"
)

// ContainerRestarted return true when there is any container in the pod that gets restarted
func ContainerRestarted(pod corev1.Pod) bool {
	if pod.Status.Phase == corev1.PodRunning || pod.Status.Phase == corev1.PodPending {
		for j := range pod.Status.InitContainerStatuses {
			stat := pod.Status.InitContainerStatuses[j]
			if stat.RestartCount > 0 {
				return true
			}
		}
		for j := range pod.Status.ContainerStatuses {
			stat := pod.Status.ContainerStatuses[j]
			if stat.RestartCount > 0 {
				return true
			}
		}
	}
	return false
}

// PodDeleted checks if the worker pod has been deleted
func PodDeleted(pod corev1.Pod) bool {
	return pod.DeletionTimestamp != nil
}

// LeaderPod check is the pod is a leader pod
func LeaderPod(pod corev1.Pod) bool {
	return pod.Labels[leaderworkerset.WorkerIndexLabelKey] == "0"
}

// PodRunningAndReady checks if the pod condition is running and marked as ready.
func PodRunningAndReady(pod corev1.Pod) bool {
	return pod.Status.Phase == corev1.PodRunning && podReady(pod)
}

func podReady(pod corev1.Pod) bool {
	return podReadyConditionTrue(pod.Status)
}

func podReadyConditionTrue(status corev1.PodStatus) bool {
	condition := getPodReadyCondition(status)
	return condition != nil && condition.Status == corev1.ConditionTrue
}

func getPodReadyCondition(status corev1.PodStatus) *corev1.PodCondition {
	_, condition := getPodCondition(&status, corev1.PodReady)
	return condition
}

func getPodCondition(status *corev1.PodStatus, conditionType corev1.PodConditionType) (int, *corev1.PodCondition) {
	if status == nil {
		return -1, nil
	}
	return getPodConditionFromList(status.Conditions, conditionType)
}

func getPodConditionFromList(conditions []corev1.PodCondition, conditionType corev1.PodConditionType) (int, *corev1.PodCondition) {
	if conditions == nil {
		return -1, nil
	}
	for i := range conditions {
		if conditions[i].Type == conditionType {
			return i, &conditions[i]
		}
	}
	return -1, nil
}

// addEnvVarsIfNotExists adds env vars to the container if they don't already exist.
// It takes a slice of existing env vars and appends new env vars to the beginning of it,
// ensuring that the 'firstEnv' and 'e' env vars maintain their order at the front.
// The function then adds any existing env vars from 'c.Env' that are not in 'e' to the newEnvVars slice.
func addEnvVarsIfNotExists(c *corev1.Container, firstEnv corev1.EnvVar, e ...corev1.EnvVar) {
	newEnvVars := make([]corev1.EnvVar, 0)

	// Add firstEnv to the beginning of the newEnvVars slice
	newEnvVars = append(newEnvVars, firstEnv)
	newEnvVars = append(newEnvVars, e...)

	// Add existing env vars from c.Env that are not in e to newEnvVars
	for _, env := range c.Env {
		exists := false
		for _, newEnv := range newEnvVars {
			if newEnv.Name == env.Name {
				exists = true
				break
			}
		}
		if !exists {
			newEnvVars = append(newEnvVars, env)
		}
	}
	c.Env = newEnvVars
}

// AddLWSVariables adds environment variable to every container.
func AddLWSVariables(pod *corev1.Pod) error {
	lwsName, found := pod.Labels[leaderworkerset.SetNameLabelKey]
	if !found {
		return fmt.Errorf("Failure constructing environment variables, no name label found for pod %v", klog.KObj(pod))
	}

	groupIndex, found := pod.Labels[leaderworkerset.GroupIndexLabelKey]
	if !found {
		return fmt.Errorf("Failure constructing environment variables, no group index label found for pod %v", klog.KObj(pod))
	}

	// The headless service name is assumed to be the same as the LWS name.
	// See function [createHeadlessServiceIfNotExists](sigs.k8s.io/lws/pkg/controllers/leaderworkerset_controller.go).
	leaderAddressEnvVar := corev1.EnvVar{
		Name:  leaderworkerset.LwsLeaderAddress,
		Value: fmt.Sprintf("%s-%s.%s.%s", lwsName, groupIndex, lwsName, pod.ObjectMeta.Namespace),
	}

	size, found := pod.Annotations[leaderworkerset.SizeAnnotationKey]
	if !found {
		return fmt.Errorf("Failure constructing environment variables, no size annotation found for pod %v", klog.KObj(pod))
	}

	// The group size is assumed to be the same as the number of replicas.
	sizeEnvVar := corev1.EnvVar{
		Name:  leaderworkerset.LwsGroupSize,
		Value: size,
	}

	// The order of injection needs attention, see
	// https://github.com/kubernetes-sigs/lws/pull/152
	for i := range pod.Spec.Containers {
		addEnvVarsIfNotExists(&pod.Spec.Containers[i], leaderAddressEnvVar, sizeEnvVar)
	}
	for i := range pod.Spec.InitContainers {
		addEnvVarsIfNotExists(&pod.Spec.InitContainers[i], leaderAddressEnvVar, sizeEnvVar)
	}

	return nil
}

// IsPodReady returns true if a pod is ready; false otherwise.
func IsPodReady(pod *corev1.Pod) bool {
	return IsPodReadyConditionTrue(pod.Status)
}

// IsPodReadyConditionTrue returns true if a pod is ready; false otherwise.
func IsPodReadyConditionTrue(status corev1.PodStatus) bool {
	condition := GetPodReadyCondition(status)
	return condition != nil && condition.Status == corev1.ConditionTrue
}

// GetPodReadyCondition extracts the pod ready condition from the given status and returns that.
// Returns nil if the condition is not present.
func GetPodReadyCondition(status corev1.PodStatus) *corev1.PodCondition {
	_, condition := GetPodCondition(&status, corev1.PodReady)
	return condition
}

// GetPodCondition extracts the provided condition from the given status and returns that.
// Returns nil and -1 if the condition is not present, and the index of the located condition.
func GetPodCondition(status *corev1.PodStatus, conditionType corev1.PodConditionType) (int, *corev1.PodCondition) {
	if status == nil {
		return -1, nil
	}
	return GetPodConditionFromList(status.Conditions, conditionType)
}

// GetPodConditionFromList extracts the provided condition from the given list of condition and
// returns the index of the condition and the condition. Returns -1 and nil if the condition is not present.
func GetPodConditionFromList(conditions []corev1.PodCondition, conditionType corev1.PodConditionType) (int, *corev1.PodCondition) {
	if conditions == nil {
		return -1, nil
	}
	for i := range conditions {
		if conditions[i].Type == conditionType {
			return i, &conditions[i]
		}
	}
	return -1, nil
}

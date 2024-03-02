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

package controller

import (
	"fmt"
	"strconv"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	appsapplyv1 "k8s.io/client-go/applyconfigurations/apps/v1"
	coreapplyv1 "k8s.io/client-go/applyconfigurations/core/v1"
	metaapplyv1 "k8s.io/client-go/applyconfigurations/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"

	leaderworkerset "sigs.k8s.io/lws/api/leaderworkerset/v1"
)

var (
	fieldManager = "leader-worker-set"
)

// constructLeaderStatefulSetApplyConfiguration constructs the apply configuration for the leader StatefulSet
func constructLeaderStatefulSetApplyConfiguration(lws *leaderworkerset.LeaderWorkerSet) (*appsapplyv1.StatefulSetApplyConfiguration, error) {
	var podTemplateSpec corev1.PodTemplateSpec
	if lws.Spec.LeaderWorkerTemplate.LeaderTemplate != nil {
		podTemplateSpec = *lws.Spec.LeaderWorkerTemplate.LeaderTemplate.DeepCopy()
	} else {
		podTemplateSpec = *lws.Spec.LeaderWorkerTemplate.WorkerTemplate.DeepCopy()
	}
	// construct pod template spec configuration
	obj, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&podTemplateSpec)
	if err != nil {
		return nil, err
	}
	var podTemplateApplyConfiguration coreapplyv1.PodTemplateSpecApplyConfiguration
	err = runtime.DefaultUnstructuredConverter.FromUnstructured(obj, &podTemplateApplyConfiguration)
	if err != nil {
		return nil, err
	}
	podTemplateApplyConfiguration.WithLabels(map[string]string{
		leaderworkerset.WorkerIndexLabelKey: "0",
		leaderworkerset.SetNameLabelKey:     lws.Name,
	})
	podAnnotations := make(map[string]string)
	podAnnotations[leaderworkerset.SizeAnnotationKey] = strconv.Itoa(int(*lws.Spec.LeaderWorkerTemplate.Size))
	if lws.Annotations[leaderworkerset.ExclusiveKeyAnnotationKey] != "" {
		podAnnotations[leaderworkerset.ExclusiveKeyAnnotationKey] = lws.Annotations[leaderworkerset.ExclusiveKeyAnnotationKey]
	}
	podTemplateApplyConfiguration.WithAnnotations(podAnnotations)
	// construct statefulset apply configuration
	statefulSetConfig := appsapplyv1.StatefulSet(lws.Name, lws.Namespace).
		WithSpec(appsapplyv1.StatefulSetSpec().
			WithServiceName(lws.Name).
			WithReplicas(*lws.Spec.Replicas).
			WithPodManagementPolicy(appsv1.ParallelPodManagement).
			WithTemplate(&podTemplateApplyConfiguration).
			WithServiceName(lws.Name).
			WithSelector(metaapplyv1.LabelSelector().
				WithMatchLabels(map[string]string{
					leaderworkerset.SetNameLabelKey:     lws.Name,
					leaderworkerset.WorkerIndexLabelKey: "0",
				}))).
		WithLabels(map[string]string{
			leaderworkerset.SetNameLabelKey: lws.Name,
		})
	return statefulSetConfig, nil
}

// setControllerReferenceWithStatefulSet set controller reference for the StatefulSet
func setControllerReferenceWithStatefulSet(owner metav1.Object, sts *appsapplyv1.StatefulSetApplyConfiguration, scheme *runtime.Scheme) error {
	// Validate the owner.
	ro, ok := owner.(runtime.Object)
	if !ok {
		return fmt.Errorf("%T is not a runtime.Object, cannot call SetOwnerReference", owner)
	}
	gvk, err := apiutil.GVKForObject(ro, scheme)
	if err != nil {
		return err
	}
	sts.WithOwnerReferences(metaapplyv1.OwnerReference().
		WithAPIVersion(gvk.GroupVersion().String()).
		WithKind(gvk.Kind).
		WithName(owner.GetName()).
		WithUID(owner.GetUID()).
		WithBlockOwnerDeletion(true).
		WithController(true))
	return nil
}

// constructWorkerStatefulSetApplyConfiguration constructs the apply configuration for the leader StatefulSet
func constructWorkerStatefulSetApplyConfiguration(pod corev1.Pod, lws leaderworkerset.LeaderWorkerSet) (*appsapplyv1.StatefulSetApplyConfiguration, error) {
	podTemplateSpec := *lws.Spec.LeaderWorkerTemplate.WorkerTemplate.DeepCopy()
	// construct pod template spec configuration
	obj, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&podTemplateSpec)
	if err != nil {
		return nil, err
	}
	var podTemplateApplyConfiguration coreapplyv1.PodTemplateSpecApplyConfiguration
	err = runtime.DefaultUnstructuredConverter.FromUnstructured(obj, &podTemplateApplyConfiguration)
	if err != nil {
		return nil, err
	}
	labelMap := map[string]string{
		leaderworkerset.GroupIndexLabelKey:      pod.Labels[leaderworkerset.GroupIndexLabelKey],
		leaderworkerset.SetNameLabelKey:         lws.Name,
		leaderworkerset.GroupUniqueHashLabelKey: pod.Labels[leaderworkerset.GroupUniqueHashLabelKey],
	}
	podTemplateApplyConfiguration.WithLabels(labelMap)
	podAnnotations := make(map[string]string)
	podAnnotations[leaderworkerset.SizeAnnotationKey] = strconv.Itoa(int(*lws.Spec.LeaderWorkerTemplate.Size))
	podAnnotations[leaderworkerset.LeaderPodNameAnnotationKey] = pod.Name
	if lws.Annotations[leaderworkerset.ExclusiveKeyAnnotationKey] != "" {
		podAnnotations[leaderworkerset.ExclusiveKeyAnnotationKey] = lws.Annotations[leaderworkerset.ExclusiveKeyAnnotationKey]
	}
	podTemplateApplyConfiguration.WithAnnotations(podAnnotations)
	// construct statefulset apply configuration
	statefulSetConfig := appsapplyv1.StatefulSet(pod.Name, pod.Namespace).
		WithSpec(appsapplyv1.StatefulSetSpec().
			WithServiceName(lws.Name).
			WithReplicas(*lws.Spec.LeaderWorkerTemplate.Size - 1).
			WithPodManagementPolicy(appsv1.ParallelPodManagement).
			WithTemplate(&podTemplateApplyConfiguration).
			WithOrdinals(appsapplyv1.StatefulSetOrdinals().WithStart(1)).
			WithSelector(metaapplyv1.LabelSelector().
				WithMatchLabels(labelMap))).
		WithLabels(labelMap)
	return statefulSetConfig, nil
}

// containerRestarted return true when there is any container in the pod that gets restarted
func containerRestarted(pod corev1.Pod) bool {
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

// podDeleted checks if the worker pod has been deleted
func podDeleted(pod corev1.Pod) bool {
	return pod.DeletionTimestamp != nil
}

// leaderPod check is the pod is a leader pod
func leaderPod(pod corev1.Pod) bool {
	return pod.Labels[leaderworkerset.WorkerIndexLabelKey] == "0"
}

func podRunningAndReady(pod *corev1.Pod) bool {
	return pod.Status.Phase == corev1.PodRunning && podReady(pod)
}

func podReady(pod *corev1.Pod) bool {
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

func makeCondition(available bool) metav1.Condition {
	condtype := string(leaderworkerset.LeaderWorkerSetProgressing)
	reason := "GroupsAreProgressing"
	message := "Creating resources"

	if available {
		condtype = string(leaderworkerset.LeaderWorkerSetAvailable)
		reason = "AllGroupsReady"
		message = "all replicas are ready"
	}

	condition := metav1.Condition{
		Type:               condtype,
		Status:             metav1.ConditionStatus(corev1.ConditionTrue),
		LastTransitionTime: metav1.Now(),
		Reason:             reason,
		Message:            message,
	}
	return condition
}

func exclusiveConditionTypes(condition1 metav1.Condition, condition2 metav1.Condition) bool {
	if (condition1.Type == string(leaderworkerset.LeaderWorkerSetAvailable) && condition2.Type == string(leaderworkerset.LeaderWorkerSetProgressing)) ||
		(condition1.Type == string(leaderworkerset.LeaderWorkerSetProgressing) && condition2.Type == string(leaderworkerset.LeaderWorkerSetAvailable)) {
		return true
	}
	return false
}

func setCondition(lws *leaderworkerset.LeaderWorkerSet, newCondition metav1.Condition) bool {
	newCondition.LastTransitionTime = metav1.Now()
	found := false
	shouldUpdate := false

	// Precondition: newCondition has status true.
	for i, curCondition := range lws.Status.Conditions {
		if newCondition.Type == curCondition.Type {
			if newCondition.Status != curCondition.Status {
				// the conditions match but one is true and one is false. Update the stored condition
				// with the new condition.
				lws.Status.Conditions[i] = newCondition
				shouldUpdate = true
			}
			// if both are true or both are false, do nothing.
			found = true
		} else {
			// if the conditions are not of the same type, do nothing unless one is Progressing and one is
			// Available and both are true. Must be mutually exclusive.
			if exclusiveConditionTypes(curCondition, newCondition) &&
				(newCondition.Status == metav1.ConditionTrue) && (curCondition.Status == metav1.ConditionTrue) {
				// Progressing is true and Availalbe is true. Prevent this.
				curCondition.Status = metav1.ConditionFalse
				shouldUpdate = true
			}
		}
	}
	// condition doesn't exist, update only if the status is true
	if newCondition.Status == metav1.ConditionTrue && !found {
		lws.Status.Conditions = append(lws.Status.Conditions, newCondition)
		shouldUpdate = true
	}
	return shouldUpdate
}

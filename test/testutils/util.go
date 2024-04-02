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
	"context"
	"fmt"
	"strconv"

	"github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	leaderworkerset "sigs.k8s.io/lws/api/leaderworkerset/v1"
	"sigs.k8s.io/lws/pkg/utils"
	acceleratorutils "sigs.k8s.io/lws/pkg/utils/accelerators"
)

func CreateWorkerPodsForLeaderPod(ctx context.Context, leaderPod corev1.Pod, k8sClient client.Client, lws leaderworkerset.LeaderWorkerSet) {
	gomega.Eventually(func() error {
		for i := 1; i <= int(*lws.Spec.LeaderWorkerTemplate.Size); i++ {
			worker := corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      leaderPod.Name + "-" + strconv.Itoa(i),
					Namespace: leaderPod.Namespace,
					Labels: map[string]string{
						leaderworkerset.SetNameLabelKey:     lws.Name,
						"worker.pod":                        "workers",
						leaderworkerset.WorkerIndexLabelKey: strconv.Itoa(i),
					},
					Annotations: map[string]string{
						leaderworkerset.SizeAnnotationKey: strconv.Itoa(int(*lws.Spec.LeaderWorkerTemplate.Size)),
					},
				},
				Spec: lws.Spec.LeaderWorkerTemplate.WorkerTemplate.Spec,
			}
			// Set the controller owner reference for garbage collection and reconciliation.
			if err := ctrl.SetControllerReference(&leaderPod, &worker, scheme.Scheme); err != nil {
				return err
			}
			if err := k8sClient.Create(ctx, &worker); err != nil {
				return err
			}
		}
		return nil
	}).Should(gomega.Succeed())
}

func DeleteLeaderPods(ctx context.Context, k8sClient client.Client, lws leaderworkerset.LeaderWorkerSet) {
	// delete pods with the highest indexes
	var leaders corev1.PodList
	gomega.Expect(k8sClient.List(ctx, &leaders, client.InNamespace(lws.Namespace), &client.MatchingLabels{leaderworkerset.WorkerIndexLabelKey: "0"})).To(gomega.Succeed())
	// we don't have "slice" package before go1.21, could only manually delete pods with largest index
	for i := range leaders.Items {
		index, _ := strconv.Atoi(leaders.Items[i].Name[len(leaders.Items[i].Name)-1:])
		if index >= int(*lws.Spec.Replicas) {
			gomega.Expect(k8sClient.Delete(ctx, &leaders.Items[i])).To(gomega.Succeed())
			// delete worker statefulset on behalf of kube-controller-manager
			var sts appsv1.StatefulSet
			gomega.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: leaders.Items[i].Name, Namespace: lws.Namespace}, &sts)).To(gomega.Succeed())
			gomega.Expect(k8sClient.Delete(ctx, &sts)).To(gomega.Succeed())
		}
	}
}

func CreateLeaderPods(ctx context.Context, leaderSts appsv1.StatefulSet, k8sClient client.Client, lws *leaderworkerset.LeaderWorkerSet, start int, end int) error {
	var podTemplateSpec corev1.PodTemplateSpec
	// if leader template is nil, use worker template
	if lws.Spec.LeaderWorkerTemplate.LeaderTemplate != nil {
		podTemplateSpec = *lws.Spec.LeaderWorkerTemplate.LeaderTemplate.DeepCopy()
	} else {
		podTemplateSpec = *lws.Spec.LeaderWorkerTemplate.WorkerTemplate.DeepCopy()
	}
	for i := start; i < end; i++ {
		pod := corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      lws.Name + "-" + strconv.Itoa(i),
				Namespace: lws.Namespace,
				Labels: map[string]string{
					leaderworkerset.SetNameLabelKey:         lws.Name,
					leaderworkerset.WorkerIndexLabelKey:     strconv.Itoa(0),
					leaderworkerset.GroupIndexLabelKey:      strconv.Itoa(i),
					leaderworkerset.GroupUniqueHashLabelKey: "randomValue",
					leaderworkerset.TemplateRevisionHashKey: utils.LeaderWorkerTemplateHash(lws),
				},
				Annotations: map[string]string{
					leaderworkerset.SizeAnnotationKey: strconv.Itoa(int(*lws.Spec.LeaderWorkerTemplate.Size)),
				},
			},
			Spec: podTemplateSpec.Spec,
		}
		if lws.Annotations[leaderworkerset.ExclusiveKeyAnnotationKey] != "" {
			pod.Annotations[leaderworkerset.ExclusiveKeyAnnotationKey] = lws.Annotations[leaderworkerset.ExclusiveKeyAnnotationKey]
		}
		// Set the controller owner reference for garbage collection and reconciliation.
		if err := ctrl.SetControllerReference(&leaderSts, &pod, scheme.Scheme); err != nil {
			return err
		}
		if err := k8sClient.Create(ctx, &pod); err != nil {
			return err
		}
	}
	return nil
}

func GetLeaderStatefulset(ctx context.Context, lws *leaderworkerset.LeaderWorkerSet, k8sClient client.Client, sts *appsv1.StatefulSet) {
	gomega.Eventually(func() error {
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: lws.Name, Namespace: lws.Namespace}, sts); err != nil {
			return err
		}
		return nil
	}, Timeout, Interval).Should(gomega.Succeed())
}

// SetPodGroupsToReady set all podGroups of the leaderWorkerSet to ready state.
func SetPodGroupsToReady(ctx context.Context, k8sClient client.Client, lws *leaderworkerset.LeaderWorkerSet) {
	stsSelector := client.MatchingLabels(map[string]string{
		leaderworkerset.SetNameLabelKey: lws.Name,
	})
	// update the condition based on the status of all statefulsets owned by the lws.
	var stsList appsv1.StatefulSetList
	gomega.Eventually(func() (int, error) {
		if err := k8sClient.List(ctx, &stsList, stsSelector, client.InNamespace(lws.Namespace)); err != nil {
			return -1, err
		}
		return len(stsList.Items), nil
	}, Timeout, Interval).Should(gomega.Equal(int(*lws.Spec.Replicas + 1)))

	for i, sts := range stsList.Items {
		if sts.Name != lws.Name {
			SetPodGroupToReady(ctx, k8sClient, &stsList.Items[i], lws)
		}
	}
}

// SetPodGroupToReady set one podGroup(leaderPod+workerStatefulset) of leaderWorkerSet to ready state, workerPods not included.
func SetPodGroupToReady(ctx context.Context, k8sClient client.Client, statefulset *appsv1.StatefulSet, lws *leaderworkerset.LeaderWorkerSet) {
	hash := utils.LeaderWorkerTemplateHash(lws)

	gomega.Eventually(func() error {
		var sts appsv1.StatefulSet
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: statefulset.Name, Namespace: statefulset.Namespace}, &sts); err != nil {
			return err
		}

		sts.Status.ReadyReplicas = *sts.Spec.Replicas
		sts.Status.Replicas = *sts.Spec.Replicas
		sts.Status.CurrentRevision = ""
		sts.Status.UpdateRevision = ""
		return k8sClient.Status().Update(ctx, &sts)
	}, Timeout, Interval).Should(gomega.Succeed())

	gomega.Eventually(func() error {
		var leaderPod corev1.Pod
		if err := k8sClient.Get(ctx, client.ObjectKey{Namespace: statefulset.Namespace, Name: statefulset.Name}, &leaderPod); err != nil {
			return err
		}

		leaderPod.Labels[leaderworkerset.TemplateRevisionHashKey] = hash
		return k8sClient.Update(ctx, &leaderPod)
	}, Timeout, Interval).Should(gomega.Succeed())

	gomega.Eventually(func() error {
		var leaderPod corev1.Pod
		if err := k8sClient.Get(ctx, client.ObjectKey{Namespace: statefulset.Namespace, Name: statefulset.Name}, &leaderPod); err != nil {
			return err
		}

		leaderPod.Status.Phase = corev1.PodRunning
		condition := corev1.PodCondition{
			Type:   corev1.PodReady,
			Status: corev1.ConditionTrue,
		}
		leaderPod.Status.Conditions = append(leaderPod.Status.Conditions, condition)
		return k8sClient.Status().Update(ctx, &leaderPod)
	}, Timeout, Interval).Should(gomega.Succeed())
}

// SetStatefulsetToUnReady set statefulset to unready.
func SetStatefulsetToUnReady(ctx context.Context, k8sClient client.Client, sts *appsv1.StatefulSet) {
	sts.Status.CurrentRevision = "fuz"
	sts.Status.UpdateRevision = "bar"
	gomega.Expect(k8sClient.Status().Update(ctx, sts)).Should(gomega.Succeed())
}

func CheckLeaderWorkerSetHasCondition(ctx context.Context, k8sClient client.Client, lws *leaderworkerset.LeaderWorkerSet, condition metav1.Condition) (bool, error) {
	var fetchedLWS leaderworkerset.LeaderWorkerSet
	if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: lws.Namespace, Name: lws.Name}, &fetchedLWS); err != nil {
		return false, err
	}
	for _, c := range fetchedLWS.Status.Conditions {
		if c.Type == condition.Type && c.Status == condition.Status {
			if condition.Message != "" {
				return condition.Message == c.Message, nil
			}
			return true, nil
		}
	}
	return false, nil
}

func HasTPUEnvVarsPopulated(pod corev1.Pod) bool {
	var containers []corev1.Container
	containers = append(containers, pod.Spec.Containers...)
	containers = append(containers, pod.Spec.InitContainers...)
	for _, container := range containers {
		for _, env := range container.Env {
			if env.Name == acceleratorutils.TpuWorkerHostNames || env.Name == acceleratorutils.TpuWorkerId {
				return true
			}
		}
	}
	return false
}

func CheckTPUContainerHasCorrectEnvVars(pod corev1.Pod, envVal string) error {
	for _, container := range pod.Spec.Containers {
		for _, env := range container.Env {
			if env.Name == acceleratorutils.TpuWorkerHostNames {
				if env.Value != envVal {
					return fmt.Errorf("incorrect env value for %s, expect %s, got %s", acceleratorutils.TpuWorkerHostNames, envVal, env.Value)
				}
			}
			if env.Name == acceleratorutils.TpuWorkerId {
				if pod.Labels[leaderworkerset.WorkerIndexLabelKey] == "0" ||
					pod.Annotations[acceleratorutils.LeaderRequestsTPUsAnnotationKey] == "true" {
					if env.Value != pod.Labels[leaderworkerset.WorkerIndexLabelKey] {
						return fmt.Errorf("incorrect env value for %s", acceleratorutils.TpuWorkerId)
					}
				} else {
					index, _ := strconv.Atoi(pod.Labels[leaderworkerset.WorkerIndexLabelKey])
					if env.Value != fmt.Sprint(index-1) {
						return fmt.Errorf("incorrect env value for %s", acceleratorutils.TpuWorkerId)
					}
				}

			}
		}
	}
	return nil
}

func ValidatePodExclusivePlacementTerms(pod corev1.Pod) bool {
	if pod.Spec.Affinity == nil || pod.Spec.Affinity.PodAffinity == nil || pod.Spec.Affinity.PodAntiAffinity == nil {
		return false
	}
	termsCount := 0
	validAffinity := false
	validAntiAffinity := false
	for _, podAffinityTerm := range pod.Spec.Affinity.PodAffinity.RequiredDuringSchedulingIgnoredDuringExecution {
		if podAffinityTerm.TopologyKey == pod.Annotations[leaderworkerset.ExclusiveKeyAnnotationKey] {
			requirement := podAffinityTerm.LabelSelector.MatchExpressions[0]
			if requirement.Key == leaderworkerset.GroupUniqueHashLabelKey && requirement.Operator == metav1.LabelSelectorOpIn && requirement.Values[0] != "" {
				validAffinity = true
				termsCount++
			}
		}
	}
	for _, podAntiAffinity := range pod.Spec.Affinity.PodAntiAffinity.RequiredDuringSchedulingIgnoredDuringExecution {
		if podAntiAffinity.TopologyKey == pod.Annotations[leaderworkerset.ExclusiveKeyAnnotationKey] {
			requirements := podAntiAffinity.LabelSelector.MatchExpressions
			hasExist := false
			hasNotIn := false
			for _, requirement := range requirements {
				if requirement.Key == leaderworkerset.GroupUniqueHashLabelKey && requirement.Operator == metav1.LabelSelectorOpExists {
					hasExist = true
				}
				if requirement.Key == leaderworkerset.GroupUniqueHashLabelKey && requirement.Operator == metav1.LabelSelectorOpNotIn && requirement.Values[0] != "" {
					hasNotIn = true
				}
			}
			validAntiAffinity = hasExist && hasNotIn
		}
	}
	return validAffinity && validAntiAffinity && termsCount == 1
}

func UpdateReplicaCount(ctx context.Context, k8sClient client.Client, lws *leaderworkerset.LeaderWorkerSet, count int32) {
	gomega.Eventually(func() error {
		lws.Spec.Replicas = ptr.To[int32](count)
		return k8sClient.Update(ctx, lws)
	}, Timeout, Interval).Should(gomega.Succeed())
}

func UpdateLeaderTemplate(ctx context.Context, k8sClient client.Client, leaderWorkerSet *leaderworkerset.LeaderWorkerSet) {
	gomega.Eventually(func() error {
		var lws leaderworkerset.LeaderWorkerSet
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: leaderWorkerSet.Name, Namespace: leaderWorkerSet.Namespace}, &lws); err != nil {
			return err
		}

		if lws.Spec.LeaderWorkerTemplate.LeaderTemplate.Labels == nil {
			lws.Spec.LeaderWorkerTemplate.LeaderTemplate.Labels = map[string]string{}
		}
		lws.Spec.LeaderWorkerTemplate.LeaderTemplate.Spec.Containers[0].Image = "nginx:1.16.1"
		return k8sClient.Update(ctx, &lws)
	}, Timeout, Interval).Should(gomega.Succeed())
}

func UpdateWorkerTemplate(ctx context.Context, k8sClient client.Client, leaderWorkerSet *leaderworkerset.LeaderWorkerSet) {
	gomega.Eventually(func() error {
		var lws leaderworkerset.LeaderWorkerSet
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: leaderWorkerSet.Name, Namespace: leaderWorkerSet.Namespace}, &lws); err != nil {
			return err
		}

		if lws.Spec.LeaderWorkerTemplate.WorkerTemplate.Labels == nil {
			lws.Spec.LeaderWorkerTemplate.WorkerTemplate.Labels = map[string]string{}
		}
		lws.Spec.LeaderWorkerTemplate.WorkerTemplate.Spec.Containers[0].Image = "nginx:1.16.1"
		return k8sClient.Update(ctx, &lws)
	}, Timeout, Interval).Should(gomega.Succeed())
}

// DeleteNamespace deletes all objects the tests typically create in the namespace.
func DeleteNamespace(ctx context.Context, c client.Client, ns *corev1.Namespace) error {
	if ns == nil {
		return nil
	}
	if err := c.DeleteAllOf(ctx, &leaderworkerset.LeaderWorkerSet{}, client.InNamespace(ns.Name)); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	if err := c.Delete(ctx, ns); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}

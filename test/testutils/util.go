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
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	leaderworkerset "sigs.k8s.io/lws/api/leaderworkerset/v1"
	"sigs.k8s.io/lws/pkg/schedulerprovider"
	"sigs.k8s.io/lws/pkg/utils"
	acceleratorutils "sigs.k8s.io/lws/pkg/utils/accelerators"
	revisionutils "sigs.k8s.io/lws/pkg/utils/revision"
)

func MustCreateLws(ctx context.Context, k8sClient client.Client, lws *leaderworkerset.LeaderWorkerSet) {
	gomega.Expect(k8sClient.Create(ctx, lws)).Should(gomega.Succeed())
	gomega.Eventually(func() error {
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: lws.Name, Namespace: lws.Namespace}, lws); err != nil {
			return err
		}
		return nil
	}, Timeout, Interval).Should(gomega.Succeed())
}

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
						leaderworkerset.RevisionKey:         revisionutils.GetRevisionKey(&leaderPod),
						leaderworkerset.GroupIndexLabelKey:  leaderPod.Labels[leaderworkerset.GroupIndexLabelKey],
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

func DeleteWorkerPods(ctx context.Context, k8sClient client.Client, lws *leaderworkerset.LeaderWorkerSet) {
	var workers corev1.PodList
	gomega.Eventually(func() bool {
		gomega.Expect(k8sClient.List(ctx, &workers, client.InNamespace(lws.Namespace), &client.MatchingLabels{"worker.pod": "workers"})).To(gomega.Succeed())
		return len(workers.Items) == int(*lws.Spec.LeaderWorkerTemplate.Size)
	}, Timeout, Interval).Should(gomega.Equal(true))
	for i := range workers.Items {
		gomega.Expect(k8sClient.Delete(ctx, &workers.Items[i])).To(gomega.Succeed())
	}
}

func DeleteLeaderPods(ctx context.Context, k8sClient client.Client, lws *leaderworkerset.LeaderWorkerSet) {
	// delete pods with the highest indexes
	var leaders corev1.PodList
	gomega.Expect(k8sClient.List(ctx, &leaders, client.InNamespace(lws.Namespace), &client.MatchingLabels{leaderworkerset.WorkerIndexLabelKey: "0"})).To(gomega.Succeed())

	var leaderWorkerSet leaderworkerset.LeaderWorkerSet
	gomega.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: lws.Name, Namespace: lws.Namespace}, &leaderWorkerSet)).To(gomega.Succeed())

	// we don't have "slice" package before go1.21, could only manually delete pods with largest index
	for i := range leaders.Items {
		index, _ := strconv.Atoi(leaders.Items[i].Name[len(leaders.Items[i].Name)-1:])
		if index >= int(*leaderWorkerSet.Spec.Replicas) {
			gomega.Expect(k8sClient.Delete(ctx, &leaders.Items[i])).To(gomega.Succeed())
			// delete worker statefulset on behalf of kube-controller-manager
			var sts appsv1.StatefulSet
			gomega.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: leaders.Items[i].Name, Namespace: lws.Namespace}, &sts)).To(gomega.Succeed())
			gomega.Expect(k8sClient.Delete(ctx, &sts)).To(gomega.Succeed())
		}
	}

	gomega.Eventually(func() bool {
		var stsList appsv1.StatefulSetList
		gomega.Expect(k8sClient.List(ctx, &stsList, client.InNamespace(lws.Namespace), &client.MatchingLabels{leaderworkerset.LeaderPodNameAnnotationKey: lws.Name})).To(gomega.Succeed())
		return len(stsList.Items) == int(*lws.Spec.Replicas)-1
	})
}

func DeleteLeaderPod(ctx context.Context, k8sClient client.Client, lws *leaderworkerset.LeaderWorkerSet, start, end int32) {
	for index := start; index < end; index++ {
		name := lws.Name + "-" + strconv.Itoa(int(index))
		var pod corev1.Pod
		gomega.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: lws.Namespace}, &pod)).To(gomega.Succeed())
		gomega.Expect(k8sClient.Delete(ctx, &pod)).To(gomega.Succeed())
		var sts appsv1.StatefulSet
		gomega.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: lws.Namespace}, &sts)).To(gomega.Succeed())
		gomega.Expect(k8sClient.Delete(ctx, &sts)).To(gomega.Succeed())
	}
}

func CreateLeaderPods(ctx context.Context, leaderSts appsv1.StatefulSet, k8sClient client.Client, lws *leaderworkerset.LeaderWorkerSet, start int, end int) error {
	cr, err := revisionutils.NewRevision(ctx, k8sClient, lws, "")
	if err != nil {
		return err
	}

	return createLeaderPods(ctx, k8sClient, leaderSts, lws, revisionutils.GetRevisionKey(cr), start, end)
}

func CreateLeaderPodsWithInjectFn(ctx context.Context, leaderSts appsv1.StatefulSet, k8sClient client.Client, lws *leaderworkerset.LeaderWorkerSet, start int, end int, injectFn func(*corev1.Pod)) error {
	cr, err := revisionutils.NewRevision(ctx, k8sClient, lws, "")
	if err != nil {
		return err
	}

	return createLeaderPodsWithInjectFn(ctx, k8sClient, leaderSts, lws, revisionutils.GetRevisionKey(cr), start, end, injectFn)
}

func CreateLeaderPodsFromRevisionNumber(ctx context.Context, leaderSts appsv1.StatefulSet, k8sClient client.Client, lws *leaderworkerset.LeaderWorkerSet, start, end, revisionNumber int) {
	selector, err := metav1.LabelSelectorAsSelector(&metav1.LabelSelector{MatchLabels: map[string]string{
		leaderworkerset.SetNameLabelKey: lws.Name,
	}})
	gomega.Expect(err).ShouldNot(gomega.HaveOccurred())
	revisions, err := revisionutils.ListRevisions(ctx, k8sClient, lws, selector)
	gomega.Expect(err).ShouldNot(gomega.HaveOccurred())
	var targetRevision *appsv1.ControllerRevision
	for i, revision := range revisions {
		if revision.Revision == int64(revisionNumber) {
			targetRevision = revisions[i]
		}
	}
	targetLws, err := revisionutils.ApplyRevision(lws, targetRevision)
	gomega.Expect(err).ShouldNot(gomega.HaveOccurred())
	gomega.Expect(createLeaderPods(ctx, k8sClient, leaderSts, targetLws, revisionutils.GetRevisionKey(targetRevision), start, end)).To(gomega.Succeed())
}

func makeLeaderPod(leaderSts appsv1.StatefulSet, lws *leaderworkerset.LeaderWorkerSet, revisionKey string, groupIndex int) (*corev1.Pod, error) {
	var podTemplateSpec corev1.PodTemplateSpec
	if lws.Spec.LeaderWorkerTemplate.LeaderTemplate != nil {
		podTemplateSpec = *lws.Spec.LeaderWorkerTemplate.LeaderTemplate.DeepCopy()
	} else {
		podTemplateSpec = *lws.Spec.LeaderWorkerTemplate.WorkerTemplate.DeepCopy()
	}

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      lws.Name + "-" + strconv.Itoa(groupIndex),
			Namespace: lws.Namespace,
			Labels: map[string]string{
				leaderworkerset.SetNameLabelKey:         lws.Name,
				leaderworkerset.WorkerIndexLabelKey:     strconv.Itoa(0),
				leaderworkerset.GroupIndexLabelKey:      strconv.Itoa(groupIndex),
				leaderworkerset.GroupUniqueHashLabelKey: "randomValue",
				leaderworkerset.RevisionKey:             revisionKey,
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
	if err := ctrl.SetControllerReference(&leaderSts, pod, scheme.Scheme); err != nil {
		return nil, err
	}

	return pod, nil
}

// createLeaderPodsWithInjectFn creates leader pods with injected metadata using the provided inject function.
func createLeaderPodsWithInjectFn(ctx context.Context, k8sClient client.Client, leaderSts appsv1.StatefulSet, lws *leaderworkerset.LeaderWorkerSet, revisionKey string, start, end int, injectFn func(*corev1.Pod)) error {
	for i := start; i < end; i++ {
		pod, err := makeLeaderPod(leaderSts, lws, revisionKey, i)
		if err != nil {
			return err
		}
		if injectFn != nil {
			injectFn(pod)
		}
		if err = k8sClient.Create(ctx, pod); err != nil {
			return err
		}
	}

	return nil
}

func createLeaderPods(ctx context.Context, k8sClient client.Client, leaderSts appsv1.StatefulSet, lws *leaderworkerset.LeaderWorkerSet, revisionKey string, start, end int) error {
	for i := start; i < end; i++ {
		pod, err := makeLeaderPod(leaderSts, lws, revisionKey, i)
		if err != nil {
			return err
		}
		if err = k8sClient.Create(ctx, pod); err != nil {
			return err
		}
	}

	return nil
}

// This should only be used in e2e test, since integration test will not automatically create worker pods.
func ExpectValidPods(ctx context.Context, k8sClient client.Client, lws *leaderworkerset.LeaderWorkerSet, podList *corev1.PodList) {
	gomega.Eventually(func() error {
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: lws.Name, Namespace: lws.Namespace}, lws); err != nil {
			return err
		}
		cr, err := revisionutils.NewRevision(ctx, k8sClient, lws, "")
		if err != nil {
			return err
		}
		labelSelector := client.MatchingLabels(map[string]string{
			leaderworkerset.SetNameLabelKey: lws.Name,
			leaderworkerset.RevisionKey:     revisionutils.GetRevisionKey(cr),
		})

		if err := k8sClient.List(ctx, podList, labelSelector, client.InNamespace(lws.Namespace)); err != nil {
			return err
		}

		if len(podList.Items) != int((*lws.Spec.Replicas)*(*lws.Spec.LeaderWorkerTemplate.Size)) {
			return fmt.Errorf("expected %d pods, got %d", (int((*lws.Spec.Replicas) * (*lws.Spec.LeaderWorkerTemplate.Size))), len(podList.Items))
		}

		var leaderTemplateSpec corev1.PodTemplateSpec
		// if leader template is nil, use worker template
		if lws.Spec.LeaderWorkerTemplate.LeaderTemplate != nil {
			leaderTemplateSpec = *lws.Spec.LeaderWorkerTemplate.LeaderTemplate.DeepCopy()
		} else {
			leaderTemplateSpec = *lws.Spec.LeaderWorkerTemplate.WorkerTemplate.DeepCopy()
		}

		workerTemplateSpec := lws.Spec.LeaderWorkerTemplate.WorkerTemplate.DeepCopy()

		for _, pod := range podList.Items {
			if pod.Labels[leaderworkerset.WorkerIndexLabelKey] == "0" && pod.Spec.Containers[0].Name != leaderTemplateSpec.Spec.Containers[0].Name {
				return errors.New("container name not right")
			}
			if pod.Labels[leaderworkerset.WorkerIndexLabelKey] != "0" && pod.Spec.Containers[0].Name != workerTemplateSpec.Spec.Containers[0].Name {
				return errors.New("container name not right")
			}
		}

		return nil
	}, Timeout, Interval).Should(gomega.Succeed())
}

func GetLeaderStatefulset(ctx context.Context, lws *leaderworkerset.LeaderWorkerSet, k8sClient client.Client, sts *appsv1.StatefulSet) {
	gomega.Eventually(func() error {
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: lws.Name, Namespace: lws.Namespace}, sts); err != nil {
			return err
		}
		return nil
	}, Timeout, Interval).Should(gomega.Succeed())
}

func GetStatefulSets(ctx context.Context, lws *leaderworkerset.LeaderWorkerSet, k8sClient client.Client, stsl *appsv1.StatefulSetList) {
	gomega.Eventually(func() (int, error) {
		if err := k8sClient.List(ctx, stsl, client.InNamespace(lws.Namespace)); err != nil {
			return 0, err
		}
		return len(stsl.Items), nil
	}, Timeout, Interval).Should(gomega.Equal(int(*lws.Spec.Replicas) + 1))
}

// SetSuperPodToReady set all podGroups of the leaderWorkerSet to ready state.
func SetSuperPodToReady(ctx context.Context, k8sClient client.Client, lws *leaderworkerset.LeaderWorkerSet, podGroupNumber int32) {
	stsSelector := client.MatchingLabels(map[string]string{
		leaderworkerset.SetNameLabelKey: lws.Name,
	})

	// update the condition based on the status of all statefulsets owned by the lws.
	var stsList appsv1.StatefulSetList

	gomega.Eventually(func() (int, error) {
		if err := k8sClient.List(ctx, &stsList, stsSelector, client.InNamespace(lws.Namespace)); err != nil {
			return 0, err
		}
		return len(stsList.Items) - 1, nil
	}, Timeout, Interval).Should(gomega.Equal(int(podGroupNumber)))

	for i, sts := range stsList.Items {
		if sts.Name != lws.Name {
			SetPodGroupToReady(ctx, k8sClient, stsList.Items[i].Name, lws)
		}
	}
}

func SetLeaderPodToReady(ctx context.Context, k8sClient client.Client, podName string, lws *leaderworkerset.LeaderWorkerSet) {
	gomega.Eventually(func() error {
		var leaderPod corev1.Pod
		if err := k8sClient.Get(ctx, client.ObjectKey{Namespace: lws.Namespace, Name: podName}, &leaderPod); err != nil {
			return err
		}

		var leaderSts appsv1.StatefulSet
		if err := k8sClient.Get(ctx, client.ObjectKey{Namespace: lws.Namespace, Name: lws.Name}, &leaderSts); err != nil {
			return err
		}
		leaderPod.Labels[leaderworkerset.RevisionKey] = revisionutils.GetRevisionKey(&leaderSts)
		return k8sClient.Update(ctx, &leaderPod)
	}, Timeout, Interval).Should(gomega.Succeed())

	gomega.Eventually(func() error {
		var leaderPod corev1.Pod
		if err := k8sClient.Get(ctx, client.ObjectKey{Namespace: lws.Namespace, Name: podName}, &leaderPod); err != nil {
			return err
		}

		leaderPod.Status.Phase = corev1.PodRunning
		condition := corev1.PodCondition{
			Type:   corev1.PodReady,
			Status: corev1.ConditionTrue,
		}
		leaderPod.Status.Conditions = append(leaderPod.Status.Conditions, condition)
		deleteWorkerStatefulSetIfExists(ctx, k8sClient, podName, lws)
		return k8sClient.Status().Update(ctx, &leaderPod)
	}, Timeout, Interval).Should(gomega.Succeed())
}

// SetPodGroupToReady set one podGroup(leaderPod+workerStatefulset) of leaderWorkerSet to ready state, workerPods not included.
func SetPodGroupToReady(ctx context.Context, k8sClient client.Client, statefulsetName string, lws *leaderworkerset.LeaderWorkerSet) {
	SetLeaderPodToReady(ctx, k8sClient, statefulsetName, lws)
	gomega.Eventually(func() error {
		var sts appsv1.StatefulSet
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: statefulsetName, Namespace: lws.Namespace}, &sts); err != nil {
			return err
		}

		sts.Status.ReadyReplicas = *sts.Spec.Replicas
		sts.Status.Replicas = *sts.Spec.Replicas
		sts.Status.CurrentRevision = ""
		sts.Status.UpdateRevision = ""
		return k8sClient.Status().Update(ctx, &sts)
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

func contains(envVars []corev1.EnvVar, e string) bool {
	for _, env := range envVars {
		if env.Name == e {
			return true
		}
	}
	return false
}

func containerHasAllEnvVars(c corev1.Container, envVars []string) bool {
	for _, e := range envVars {
		if !contains(c.Env, e) {
			return false
		}
	}
	return true
}

func hasAllEnvVarPopulated(pod corev1.Pod, envVars []string) bool {
	var containers []corev1.Container
	containers = append(containers, pod.Spec.Containers...)
	containers = append(containers, pod.Spec.InitContainers...)
	for _, container := range containers {
		if !containerHasAllEnvVars(container, envVars) {
			return false
		}
	}
	return true
}

func HasLWSEnvVarsPopulated(pod corev1.Pod) bool {
	return hasAllEnvVarPopulated(pod, []string{leaderworkerset.LwsLeaderAddress, leaderworkerset.LwsGroupSize, leaderworkerset.LwsWorkerIndex})
}

func CheckAnnotation(pod corev1.Pod, key, val string) error {
	podVal, exists := pod.ObjectMeta.Annotations[leaderworkerset.SizeAnnotationKey]
	if !exists {
		return errors.New("Pod annotation not set")
	}
	if podVal != val {
		return fmt.Errorf("Pod annotation value is not correct, want: %v, got: %v", val, podVal)
	}
	return nil
}

func CheckContainerHasCorrectEnvVar(pod corev1.Pod, expect corev1.EnvVar) error {
	for _, container := range pod.Spec.Containers {
		for _, env := range container.Env {
			if env.Name == expect.Name && env.Value != expect.Value {
				return fmt.Errorf("incorrect env value for %s, expect %s, got %s", expect.Name, expect.Value, env.Value)
			}
		}
	}
	return nil
}

func IsContainerFirstEnvVarLWSLeaderAddress(pod corev1.Pod) error {
	for _, container := range pod.Spec.Containers {
		// check the first env var is the LWS_LEADER_ADDRESS
		if container.Env[0].Name != leaderworkerset.LwsLeaderAddress {
			return fmt.Errorf("expecting first container env var to be LWS_LEADER_ADDRESS, but got %s", container.Env[0].Name)
		}
	}
	return nil
}

func HasTPUEnvVarsPopulated(pod corev1.Pod) bool {
	return hasAllEnvVarPopulated(pod, []string{acceleratorutils.TpuWorkerHostNames, acceleratorutils.TpuWorkerId, acceleratorutils.TpuName})
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
				if subGroupSize, foundSubGroupSize := pod.Annotations[leaderworkerset.SubGroupSizeAnnotationKey]; foundSubGroupSize {
					workerIndex, _ := strconv.Atoi(pod.Labels[leaderworkerset.WorkerIndexLabelKey])
					subGroupSize, _ := strconv.Atoi(subGroupSize)
					index := (workerIndex) % subGroupSize
					if pod.Annotations[acceleratorutils.LeaderRequestsTPUsAnnotationKey] != "true" {
						index = (workerIndex - 1) % subGroupSize
					}
					if env.Value != fmt.Sprint(index) {
						return fmt.Errorf("incorrect env value for %s", acceleratorutils.TpuWorkerId)
					}
				} else if pod.Labels[leaderworkerset.WorkerIndexLabelKey] == "0" ||
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

func ValidatePodExclusivePlacementTerms(pod corev1.Pod, exclusiveAnnotationKey string, uniqueHashLabelKey string) bool {
	if pod.Spec.Affinity == nil || pod.Spec.Affinity.PodAffinity == nil || pod.Spec.Affinity.PodAntiAffinity == nil {
		return false
	}
	termsCount := 0
	validAffinity := false
	validAntiAffinity := false
	for _, podAffinityTerm := range pod.Spec.Affinity.PodAffinity.RequiredDuringSchedulingIgnoredDuringExecution {
		if podAffinityTerm.TopologyKey == pod.Annotations[exclusiveAnnotationKey] {
			requirement := podAffinityTerm.LabelSelector.MatchExpressions[0]
			if requirement.Key == uniqueHashLabelKey && requirement.Operator == metav1.LabelSelectorOpIn && requirement.Values[0] != "" {
				validAffinity = true
				termsCount++
			}
		}
	}
	for _, podAntiAffinity := range pod.Spec.Affinity.PodAntiAffinity.RequiredDuringSchedulingIgnoredDuringExecution {
		if podAntiAffinity.TopologyKey == pod.Annotations[exclusiveAnnotationKey] {
			requirements := podAntiAffinity.LabelSelector.MatchExpressions
			hasExist := false
			hasNotIn := false
			for _, requirement := range requirements {
				if requirement.Key == uniqueHashLabelKey && requirement.Operator == metav1.LabelSelectorOpExists {
					hasExist = true
				}
				if requirement.Key == uniqueHashLabelKey && requirement.Operator == metav1.LabelSelectorOpNotIn && requirement.Values[0] != "" {
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
		var leaderworkerset leaderworkerset.LeaderWorkerSet
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: lws.Name, Namespace: lws.Namespace}, &leaderworkerset); err != nil {
			return err
		}

		leaderworkerset.Spec.Replicas = ptr.To[int32](count)
		return k8sClient.Update(ctx, &leaderworkerset)
	}, Timeout, Interval).Should(gomega.Succeed())
}

func UpdateSize(ctx context.Context, k8sClient client.Client, lws *leaderworkerset.LeaderWorkerSet, size int32) {
	gomega.Eventually(func() error {
		var leaderworkerset leaderworkerset.LeaderWorkerSet
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: lws.Name, Namespace: lws.Namespace}, &leaderworkerset); err != nil {
			return err
		}

		leaderworkerset.Spec.LeaderWorkerTemplate.Size = ptr.To(size)
		return k8sClient.Update(ctx, &leaderworkerset)
	}, Timeout, Interval).Should(gomega.Succeed())
}

func UpdateSubdomainPolicy(ctx context.Context, k8sClient client.Client, lws *leaderworkerset.LeaderWorkerSet, subdomainPolicy leaderworkerset.SubdomainPolicy) {
	gomega.Eventually(func() error {
		var newLws leaderworkerset.LeaderWorkerSet
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: lws.Name, Namespace: lws.Namespace}, &newLws); err != nil {
			return err
		}

		newLws.Spec.NetworkConfig = &leaderworkerset.NetworkConfig{
			SubdomainPolicy: &subdomainPolicy,
		}
		return k8sClient.Update(ctx, &newLws)
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
		lws.Spec.LeaderWorkerTemplate.LeaderTemplate.Spec.Containers[0].Name = "new-leader-name"
		return k8sClient.Update(ctx, &lws)
	}, Timeout, Interval).Should(gomega.Succeed())
}

func UpdateWorkerTemplateImage(ctx context.Context, k8sClient client.Client, leaderWorkerSet *leaderworkerset.LeaderWorkerSet) {
	gomega.Eventually(func() error {
		var lws leaderworkerset.LeaderWorkerSet
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: leaderWorkerSet.Name, Namespace: leaderWorkerSet.Namespace}, &lws); err != nil {
			return err
		}

		lws.Spec.LeaderWorkerTemplate.WorkerTemplate.Spec.Containers[0].Image = "docker.io/nginxinc/nginx-unprivileged:1.27"
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
		lws.Spec.LeaderWorkerTemplate.WorkerTemplate.Spec.Containers[0].Name = "new-worker-name"
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

func SetLeaderPodsToReady(ctx context.Context, k8sClient client.Client, lws *leaderworkerset.LeaderWorkerSet, start, end int) {
	var leaderSts appsv1.StatefulSet
	gomega.Eventually(func() error {
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: lws.Name, Namespace: lws.Namespace}, &leaderSts); err != nil {
			return err
		}
		return nil
	}, Timeout, Interval).Should(gomega.Succeed())

	for i := start; i < end; i++ {
		SetLeaderPodToReady(ctx, k8sClient, fmt.Sprintf("%s-%d", leaderSts.Name, i), lws)
	}

	// If size=1, we should trigger the leader sts update or the controller will not run reconciliation.
	gomega.Eventually(func() error {
		var sts appsv1.StatefulSet
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: lws.Name, Namespace: lws.Namespace}, &sts); err != nil {
			return err
		}
		sts.Status.ReadyReplicas = *sts.Spec.Replicas
		sts.Status.Replicas = *sts.Spec.Replicas
		sts.Status.CurrentRevision = ""
		sts.Status.UpdateRevision = ""
		return k8sClient.Status().Update(ctx, &sts)
	}, Timeout, Interval).Should(gomega.Succeed())
}

func deleteWorkerStatefulSetIfExists(ctx context.Context, k8sClient client.Client, statefulsetName string, lws *leaderworkerset.LeaderWorkerSet) {
	// in cases where size = 1, the workerstatefulset does not exist
	gomega.Eventually(func() error {
		var sts appsv1.StatefulSet
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: statefulsetName, Namespace: lws.Namespace}, &sts); err != nil {
			if client.IgnoreNotFound(err) != nil {
				return err
			}
			return nil
		}
		return k8sClient.Delete(ctx, &sts)
	}, Timeout, Interval).Should(gomega.Succeed())
}

func DeleteLWSWithForground(ctx context.Context, k8sClient client.Client, lws *leaderworkerset.LeaderWorkerSet) {
	gomega.Eventually(func() error {
		return k8sClient.Delete(ctx, lws, client.PropagationPolicy(metav1.DeletePropagationForeground))
	}, Timeout, Interval).Should(gomega.Succeed())
}

// GetProjectDir will return the directory where the project is
func GetProjectDir() (string, error) {
	wd, err := os.Getwd()
	if err != nil {
		return wd, err
	}
	wd = strings.Replace(wd, "/test/e2e", "", -1)
	return wd, nil
}

// Run executes the provided command within this context
func Run(cmd *exec.Cmd) (string, error) {
	dir, _ := GetProjectDir()
	cmd.Dir = dir

	if err := os.Chdir(cmd.Dir); err != nil {
		_, _ = fmt.Fprintf(ginkgo.GinkgoWriter, "chdir dir: %s\n", err)
	}

	cmd.Env = append(os.Environ(), "GO111MODULE=on")
	command := strings.Join(cmd.Args, " ")
	_, _ = fmt.Fprintf(ginkgo.GinkgoWriter, "running: %s\n", command)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return string(output), fmt.Errorf("%s failed with error: (%v) %s", command, err, string(output))
	}

	return string(output), nil
}

// GetNonEmptyLines converts given command output string into individual objects
// according to line breakers, and ignores the empty elements in it.
func GetNonEmptyLines(output string) []string {
	var res []string
	elements := strings.Split(output, "\n")
	for _, element := range elements {
		if element != "" {
			res = append(res, element)
		}
	}

	return res
}

func SetLwsPartition(ctx context.Context, k8sClient client.Client, lws *leaderworkerset.LeaderWorkerSet, partition int32) {
	gomega.Eventually(func() error {
		var newLws leaderworkerset.LeaderWorkerSet
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: lws.Name, Namespace: lws.Namespace}, &newLws); err != nil {
			return err
		}

		newLws.Spec.RolloutStrategy.RollingUpdateConfiguration.Partition = &partition
		return k8sClient.Update(ctx, &newLws)
	}, Timeout, Interval).Should(gomega.Succeed())
}

// getPodGroupGVK returns the GroupVersionKind for PodGroup based on the scheduler provider type
func getPodGroupGVK(provider schedulerprovider.ProviderType) schema.GroupVersionKind {
	switch provider {
	case schedulerprovider.Volcano:
		return schema.GroupVersionKind{
			Group:   "scheduling.volcano.sh",
			Version: "v1beta1",
			Kind:    "PodGroup",
		}
	default:
		// Return empty GVK for unsupported provider types
		return schema.GroupVersionKind{}
	}
}

// validateOwnerReference validates the owner reference in a PodGroup using unstructured access
func validateOwnerReference(podGroup *unstructured.Unstructured, leaderPod *corev1.Pod) {
	ownerRefs, found, err := unstructured.NestedSlice(podGroup.Object, "metadata", "ownerReferences")
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
	gomega.Expect(found).To(gomega.BeTrue())
	gomega.Expect(ownerRefs).To(gomega.HaveLen(1))

	ownerRefMap := ownerRefs[0].(map[string]interface{})
	gomega.Expect(ownerRefMap["apiVersion"]).To(gomega.Equal("v1"))
	gomega.Expect(ownerRefMap["kind"]).To(gomega.Equal("Pod"))
	gomega.Expect(ownerRefMap["name"]).To(gomega.Equal(leaderPod.Name))
	gomega.Expect(ownerRefMap["uid"]).To(gomega.Equal(string(leaderPod.UID)))

	controller, found, err := unstructured.NestedBool(ownerRefMap, "controller")
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
	gomega.Expect(found).To(gomega.BeTrue())
	gomega.Expect(controller).To(gomega.BeTrue())
}

// validatePodGroupSpec validates the PodGroup spec using unstructured access
func validatePodGroupSpec(podGroup *unstructured.Unstructured, lws *leaderworkerset.LeaderWorkerSet) {
	minMember, found, err := unstructured.NestedInt64(podGroup.Object, "spec", "minMember")
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
	gomega.Expect(found).To(gomega.BeTrue())

	// For LeaderReady startup policy, minMember should be 1 (leader only)
	// For LeaderCreated startup policy, minMember should be size (leader + workers)
	expectedMinMember := int64(*lws.Spec.LeaderWorkerTemplate.Size)
	if lws.Spec.StartupPolicy == leaderworkerset.LeaderReadyStartupPolicy {
		expectedMinMember = 1
	}

	gomega.Expect(minMember).To(gomega.Equal(expectedMinMember))

	// Validate minResources - should be the same for both LeaderCreated and LeaderReady
	// as it represents total resources needed for the entire PodGroup [1 Leader + (size-1) Worker pods]
	expectedMinResources := utils.CalculatePGMinResources(lws)

	minResourcesMap, found, err := unstructured.NestedMap(podGroup.Object, "spec", "minResources")
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
	gomega.Expect(found).To(gomega.BeTrue())

	// Validate each resource type in minResources
	for resourceName, expectedQuantity := range expectedMinResources {
		actualQuantityStr, exists := minResourcesMap[string(resourceName)]
		gomega.Expect(exists).To(gomega.BeTrue(), "Resource %s should exist in minResources", resourceName)

		actualQuantity, err := resource.ParseQuantity(actualQuantityStr.(string))
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(actualQuantity.Equal(expectedQuantity)).To(gomega.BeTrue(),
			"Resource %s: expected %s, got %s", resourceName, expectedQuantity.String(), actualQuantity.String())
	}
}

// ExpectValidPodGroups validates that PodGroups are correctly created for a LeaderWorkerSet with proper owner references and spec configuration.
// When expectedCount is 0, it validates that no PodGroups exist for the LWS, checking cleanup.
// When expectedCount > 0, it validates both count and detailed configuration of each PodGroup.
func ExpectValidPodGroups(ctx context.Context, k8sClient client.Client, provider schedulerprovider.ProviderType, lws *leaderworkerset.LeaderWorkerSet, expectedCount int) {
	gvk := getPodGroupGVK(provider)
	if gvk.Empty() {
		ginkgo.Fail("Unsupported scheduler provider for PodGroup validation")
		return
	}

	// Get the current revision from StatefulSet to construct PodGroup names
	var leaderSts appsv1.StatefulSet
	gomega.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: lws.Name, Namespace: lws.Namespace}, &leaderSts)).To(gomega.Succeed())
	revision := revisionutils.GetRevisionKey(&leaderSts)

	// Use unstructured list to count PodGroups
	podGroupList := &unstructured.UnstructuredList{}
	podGroupList.SetGroupVersionKind(gvk)
	gomega.Eventually(func() (int, error) {
		err := k8sClient.List(ctx, podGroupList, client.InNamespace(lws.Namespace))
		if err != nil {
			return 0, err
		}

		lwsPodGroups := 0
		for _, pg := range podGroupList.Items {
			labels := pg.GetLabels()
			if labels[leaderworkerset.SetNameLabelKey] == lws.Name &&
				labels[leaderworkerset.RevisionKey] == revision {
				lwsPodGroups++
			}
		}

		return lwsPodGroups, nil
	}, Timeout, Interval).Should(gomega.Equal(expectedCount))

	// Verify each PodGroup has correct owner reference and spec
	for i := 0; i < expectedCount; i++ {
		groupIndex := fmt.Sprintf("%d", i)
		expectedPgName := schedulerprovider.GetPodGroupName(lws.Name, groupIndex, revision)
		leaderPodName := fmt.Sprintf("%s-%d", lws.Name, i)

		leaderPod := &corev1.Pod{}
		gomega.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: leaderPodName, Namespace: lws.Namespace}, leaderPod)).To(gomega.Succeed())

		// Get the PodGroup using unstructured object
		podGroup := &unstructured.Unstructured{}
		podGroup.SetGroupVersionKind(gvk)
		gomega.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: expectedPgName, Namespace: lws.Namespace}, podGroup)).To(gomega.Succeed())

		// Validate podgroup owner reference and spec
		validateOwnerReference(podGroup, leaderPod)
		validatePodGroupSpec(podGroup, lws)
	}
}

// UpdatePodGroupAtIndex manually updates the PodGroup for a specific group index
func UpdatePodGroupAtIndex(ctx context.Context, k8sClient client.Client, providerType schedulerprovider.ProviderType, schedulerProvider schedulerprovider.SchedulerProvider, lws *leaderworkerset.LeaderWorkerSet, groupIndex string) {
	gvk := getPodGroupGVK(providerType)
	if gvk.Empty() {
		ginkgo.Fail("Unsupported scheduler provider for PodGroup update")
		return
	}

	// Get revisions for PodGroup naming
	var leaderSts appsv1.StatefulSet
	gomega.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: lws.Name, Namespace: lws.Namespace}, &leaderSts)).To(gomega.Succeed())
	revision := revisionutils.GetRevisionKey(&leaderSts)

	// Manually simulate replica update: delete old leader Pod and PodGroup, create new ones
	// Delete old leader pod
	oldPod := &corev1.Pod{}
	podName := fmt.Sprintf("%s-%s", lws.Name, groupIndex)
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: podName, Namespace: lws.Namespace}, oldPod); err == nil {
		gomega.Expect(k8sClient.Delete(ctx, oldPod)).To(gomega.Succeed())
	}

	// Delete old PodGroup (simulating garbage collection) using unstructured object
	oldPgName := schedulerprovider.GetPodGroupName(lws.Name, groupIndex, revision)
	oldPodGroup := &unstructured.Unstructured{}
	oldPodGroup.SetGroupVersionKind(gvk)
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: oldPgName, Namespace: lws.Namespace}, oldPodGroup); err == nil {
		gomega.Expect(k8sClient.Delete(ctx, oldPodGroup)).To(gomega.Succeed())
	}

	// Create new pod (this will trigger new PodGroup creation)
	startIndex, err := strconv.Atoi(groupIndex)
	gomega.Expect(err).ToNot(gomega.HaveOccurred())
	injectFn := func(pod *corev1.Pod) {
		gomega.Expect(schedulerProvider).NotTo(gomega.BeNil())
		err = schedulerProvider.InjectPodGroupMetadata(pod)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
	}
	gomega.Expect(CreateLeaderPodsWithInjectFn(ctx, leaderSts, k8sClient, lws, startIndex, startIndex+1, injectFn)).To(gomega.Succeed())
}

// ExpectValidPodGroupAtIndex checks that a PodGroup exists for the specified group index and has the correct owner reference
func ExpectValidPodGroupAtIndex(ctx context.Context, k8sClient client.Client, provider schedulerprovider.ProviderType, lws *leaderworkerset.LeaderWorkerSet, groupIndex string) {
	gvk := getPodGroupGVK(provider)
	if gvk.Empty() {
		ginkgo.Fail("Unsupported scheduler provider for PodGroup validation")
		return
	}

	// Get revisions for PodGroup naming
	var leaderSts appsv1.StatefulSet
	gomega.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: lws.Name, Namespace: lws.Namespace}, &leaderSts)).To(gomega.Succeed())
	revision := revisionutils.GetRevisionKey(&leaderSts)

	expectedPgName := schedulerprovider.GetPodGroupName(lws.Name, groupIndex, revision)
	leaderPodName := fmt.Sprintf("%s-%s", lws.Name, groupIndex)

	leaderPod := &corev1.Pod{}
	gomega.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: leaderPodName, Namespace: lws.Namespace}, leaderPod)).To(gomega.Succeed())

	// Verify podgroup owner reference
	podGroup := &unstructured.Unstructured{}
	podGroup.SetGroupVersionKind(gvk)
	gomega.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: expectedPgName, Namespace: lws.Namespace}, podGroup)).To(gomega.Succeed())
	validateOwnerReference(podGroup, leaderPod)
}

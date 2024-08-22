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

package webhooks

import (
	"context"
	"fmt"
	"strconv"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	leaderworkerset "sigs.k8s.io/lws/api/leaderworkerset/v1"
	"sigs.k8s.io/lws/pkg/utils"
	acceleratorutils "sigs.k8s.io/lws/pkg/utils/accelerators"
	podutils "sigs.k8s.io/lws/pkg/utils/pod"
	statefulsetutils "sigs.k8s.io/lws/pkg/utils/statefulset"
)

type PodWebhook struct{}

func SetupPodWebhook(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr).
		For(&corev1.Pod{}).
		WithDefaulter(&PodWebhook{}).
		WithValidator(&PodWebhook{}).
		Complete()
}

//+kubebuilder:webhook:path=/validate--v1-pod,mutating=false,failurePolicy=fail,sideEffects=None,groups="",resources=pods,verbs=create;update,versions=v1,name=vpod.kb.io,sideEffects=None,admissionReviewVersions=v1

// validate admits a pod if a specific annotation exists.
func (p *PodWebhook) validate(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	log := logf.FromContext(ctx)
	pod, ok := obj.(*corev1.Pod)
	if !ok {
		return nil, fmt.Errorf("expected a Pod but got a %T", obj)
	}

	log.V(2).Info("Validating Pod")

	// if pod is not part of leaderworkerset, skip
	_, found := pod.Labels[leaderworkerset.SetNameLabelKey]
	if !found {
		return nil, nil
	}

	return nil, nil
}

func (p *PodWebhook) ValidateCreate(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	return p.validate(ctx, obj)
}

func (p *PodWebhook) ValidateUpdate(ctx context.Context, oldObj, newObj runtime.Object) (admission.Warnings, error) {
	return nil, nil
}

func (p *PodWebhook) ValidateDelete(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	return nil, nil
}

//+kubebuilder:webhook:path=/mutate--v1-pod,mutating=true,failurePolicy=fail,groups="",resources=pods,verbs=create;update,versions=v1,name=mpod.kb.io,sideEffects=None,admissionReviewVersions=v1

func (p *PodWebhook) Default(ctx context.Context, obj runtime.Object) error {
	log := logf.FromContext(ctx)
	pod, ok := obj.(*corev1.Pod)
	if !ok {
		return fmt.Errorf("expected a Pod but got a %T", obj)
	}

	log.V(2).Info("Defaulting Pod")
	// if pod is not part of leaderworkerset, skip
	_, found := pod.Labels[leaderworkerset.SetNameLabelKey]
	if !found {
		return nil
	}
	size, exist := pod.Annotations[leaderworkerset.SizeAnnotationKey]
	if !exist {
		return fmt.Errorf("size annotation is unexpectedly missing for pod %s", pod.Name)
	}
	podCount, err := strconv.Atoi(size)
	if err != nil {
		return err
	}
	// adding labels for pods
	if podutils.LeaderPod(*pod) {
		// add group index label to group pods
		if _, found := pod.Labels[leaderworkerset.GroupIndexLabelKey]; !found {
			_, groupIndex := statefulsetutils.GetParentNameAndOrdinal(pod.Name)
			if groupIndex == -1 {
				return fmt.Errorf("parsing pod ordinal for pod %s", pod.Name)
			}
			pod.Labels[leaderworkerset.GroupIndexLabelKey] = fmt.Sprint(groupIndex)
		}
		subdomainPolicy, foundSubdomainPolicy := pod.Annotations[leaderworkerset.SubdomainPolicyAnnotationKey]
		if foundSubdomainPolicy && subdomainPolicy == string(leaderworkerset.SubdomainUniquePerReplica) {
			pod.Spec.Subdomain = pod.Name
		}
		// add group unique key label for exclusive placement, and use it to check whether the node affinity has been applied
		var groupUniqueKey string
		if _, foundGroupKey := pod.Labels[leaderworkerset.GroupUniqueHashLabelKey]; !foundGroupKey {
			groupUniqueKey = genGroupUniqueKey(pod.Namespace, pod.Name)
			pod.Labels[leaderworkerset.GroupUniqueHashLabelKey] = groupUniqueKey
		} else {
			groupUniqueKey = pod.Labels[leaderworkerset.GroupUniqueHashLabelKey]
		}
		if epKey, foundEpKey := pod.Annotations[leaderworkerset.ExclusiveKeyAnnotationKey]; foundEpKey {
			SetExclusiveAffinities(pod, groupUniqueKey, epKey, leaderworkerset.GroupUniqueHashLabelKey)
		}
		_, foundSubGroupSize := pod.Annotations[leaderworkerset.SubGroupSizeAnnotationKey]
		if foundSubGroupSize && pod.Labels[leaderworkerset.SubGroupIndexLabelKey] == "" {
			// The leader pod always lands on SubGroup 0.
			pod.Labels[leaderworkerset.SubGroupIndexLabelKey] = "0"
			subGroupUniqueKey := genGroupUniqueKey(pod.Name, "0")
			pod.Labels[leaderworkerset.SubGroupUniqueHashLabelKey] = subGroupUniqueKey

			if subEpKey, foundSubEpKey := pod.Annotations[leaderworkerset.SubGroupExclusiveKeyAnnotationKey]; foundSubEpKey {
				SetExclusiveAffinities(pod, subGroupUniqueKey, subEpKey, leaderworkerset.SubGroupUniqueHashLabelKey)
			}
		}
	} else {
		_, workerIndex := statefulsetutils.GetParentNameAndOrdinal(pod.Name)
		if workerIndex == -1 {
			return fmt.Errorf("parsing pod ordinal for pod %s", pod.Name)
		}
		pod.Labels[leaderworkerset.WorkerIndexLabelKey] = fmt.Sprint(workerIndex)
		subGroupSize, foundSubGroupSize := pod.Annotations[leaderworkerset.SubGroupSizeAnnotationKey]
		if foundSubGroupSize && pod.Labels[leaderworkerset.SubGroupIndexLabelKey] == "" {
			subGroupSizeInt, err := strconv.Atoi(subGroupSize)
			if err != nil {
				return err
			}
			leaderName := pod.Annotations[leaderworkerset.LeaderPodNameAnnotationKey]
			subGroupIndexKey := getSubGroupIndex(podCount, subGroupSizeInt, workerIndex)
			pod.Labels[leaderworkerset.SubGroupIndexLabelKey] = subGroupIndexKey
			subGroupUniqueKey := genGroupUniqueKey(leaderName, subGroupIndexKey)
			pod.Labels[leaderworkerset.SubGroupUniqueHashLabelKey] = subGroupUniqueKey
			if subEpKey, foundSubEpKey := pod.Annotations[leaderworkerset.SubGroupExclusiveKeyAnnotationKey]; foundSubEpKey {
				SetExclusiveAffinities(pod, subGroupUniqueKey, subEpKey, leaderworkerset.SubGroupUniqueHashLabelKey)
			}
		}
	}

	// injecting env vars if needed
	if acceleratorutils.PodRequestsTPUs(pod.Spec) {
		if err := acceleratorutils.AddTPUVariables(pod, podCount); err != nil {
			return err
		}
	}

	if err := podutils.AddLWSVariables(pod); err != nil {
		return err
	}

	return nil
}

func genGroupUniqueKey(ns string, podName string) string {
	return utils.Sha1Hash(fmt.Sprintf("%s/%s", ns, podName))
}

// SetExclusiveAffinities set the pod affinity/anti-affinity
func SetExclusiveAffinities(pod *corev1.Pod, groupUniqueKey string, topologyKey string, podAffinityKey string) {
	if exclusiveAffinityApplied(*pod, topologyKey) {
		return
	}
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
					Key:      podAffinityKey,
					Operator: metav1.LabelSelectorOpIn,
					Values:   []string{groupUniqueKey},
				},
			}},
			TopologyKey: topologyKey,
		})
	// Pod anti-affinity ensures exclusively this set lands on the topology, preventing multiple sets per topology domain.
	pod.Spec.Affinity.PodAntiAffinity.RequiredDuringSchedulingIgnoredDuringExecution = append(pod.Spec.Affinity.PodAntiAffinity.RequiredDuringSchedulingIgnoredDuringExecution,
		corev1.PodAffinityTerm{
			LabelSelector: &metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{
				{
					Key:      podAffinityKey,
					Operator: metav1.LabelSelectorOpExists,
				},
				{
					Key:      podAffinityKey,
					Operator: metav1.LabelSelectorOpNotIn,
					Values:   []string{groupUniqueKey},
				},
			}},
			TopologyKey: topologyKey,
		})
}

// exclusiveAffinityApplied return true if the exclusive placement terms have been applied
func exclusiveAffinityApplied(pod corev1.Pod, topologyKey string) bool {
	if pod.Spec.Affinity == nil || pod.Spec.Affinity.PodAffinity == nil || pod.Spec.Affinity.PodAntiAffinity == nil {
		return false
	}
	hasAffinity := false
	hasAntiAffinity := false
	for _, podAffinityTerm := range pod.Spec.Affinity.PodAffinity.RequiredDuringSchedulingIgnoredDuringExecution {
		if podAffinityTerm.TopologyKey == topologyKey {
			hasAffinity = true
		}
	}
	for _, term := range pod.Spec.Affinity.PodAntiAffinity.RequiredDuringSchedulingIgnoredDuringExecution {
		if term.TopologyKey == topologyKey {
			hasAntiAffinity = true
		}
	}
	return hasAffinity && hasAntiAffinity
}

func getSubGroupIndex(podCount int, subGroupSize int, workerIndex int) string {
	if (podCount-1)%subGroupSize == 0 {
		// Leader is considered as extra pod, it is part of the first group
		return fmt.Sprint((workerIndex - 1) / subGroupSize)
	}
	return fmt.Sprint(workerIndex / subGroupSize)
}

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

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	leaderworkerset "sigs.k8s.io/lws/api/leaderworkerset/v1"
)

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

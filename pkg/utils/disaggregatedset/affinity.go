/*
Copyright 2025 The Kubernetes Authors.

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

package disaggregatedset

import (
	"strconv"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	disaggregatedsetv1 "sigs.k8s.io/lws/api/disaggregatedset/v1"
)

// SetPlacementAffinities injects the pod affinity and anti-affinity terms for the
// given placement policy into podSpec, preserving any affinity already present. It
// is a no-op for a nil policy or PlacementNone. Selectors key off the name and slice
// labels the controller stamps on managed pods, so the terms resolve at scheduling.
//
// ExclusiveSlice co-locates a slice's roles (podAffinity name+slice) and spreads
// this set's slices (podAntiAffinity same name, other slice). ExclusiveTopology adds
// a term excluding other DisaggregatedSets' slices (name not-in, slice exists),
// giving one slice per domain.
func SetPlacementAffinities(podSpec *corev1.PodSpec, dsName string, slice int, policy *disaggregatedsetv1.PlacementPolicy) {
	// A non-None policy without a topology key would produce affinity terms with an
	// empty topologyKey, which the API rejects. The webhook already blocks this; the
	// guard here keeps the builder correct on its own if validation is ever bypassed.
	if policy == nil || policy.Type == disaggregatedsetv1.PlacementNone || policy.Type == "" || policy.Topology == "" {
		return
	}
	sliceStr := strconv.Itoa(slice)
	topologyKey := policy.Topology

	// Deep-copy any existing affinity so appended terms never mutate a pod template
	// shared with the caller's spec. DeepCopy is nil-safe.
	affinity := podSpec.Affinity.DeepCopy()
	if affinity == nil {
		affinity = &corev1.Affinity{}
	}
	if affinity.PodAffinity == nil {
		affinity.PodAffinity = &corev1.PodAffinity{}
	}
	if affinity.PodAntiAffinity == nil {
		affinity.PodAntiAffinity = &corev1.PodAntiAffinity{}
	}

	// Co-locate this slice's roles in one domain.
	affinity.PodAffinity.RequiredDuringSchedulingIgnoredDuringExecution = append(
		affinity.PodAffinity.RequiredDuringSchedulingIgnoredDuringExecution,
		corev1.PodAffinityTerm{
			LabelSelector: &metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{
				{Key: disaggregatedsetv1.SetNameLabelKey, Operator: metav1.LabelSelectorOpIn, Values: []string{dsName}},
				{Key: disaggregatedsetv1.SliceLabelKey, Operator: metav1.LabelSelectorOpIn, Values: []string{sliceStr}},
			}},
			TopologyKey: topologyKey,
		})

	// Spread this DisaggregatedSet's slices: repel same-set pods of a different slice.
	// Other DisaggregatedSets (different name) are not matched, so they may share.
	//
	// NotIn also matches pods missing the slice label entirely, which is what we want
	// for slices != 0: pods created before the slices feature carry no slice label and
	// are semantically slice 0, so they must repel other slices. For slice 0 itself the
	// opposite holds, an unlabeled legacy pod is the same slice, not a different one,
	// so the term additionally requires the label to exist. Without that, a slice-0 pod
	// with placement enabled is repelled by its own legacy predecessor and never
	// schedules during upgrade.
	spreadExprs := []metav1.LabelSelectorRequirement{
		{Key: disaggregatedsetv1.SetNameLabelKey, Operator: metav1.LabelSelectorOpIn, Values: []string{dsName}},
	}
	if slice == 0 {
		spreadExprs = append(spreadExprs,
			metav1.LabelSelectorRequirement{Key: disaggregatedsetv1.SliceLabelKey, Operator: metav1.LabelSelectorOpExists})
	}
	spreadExprs = append(spreadExprs,
		metav1.LabelSelectorRequirement{Key: disaggregatedsetv1.SliceLabelKey, Operator: metav1.LabelSelectorOpNotIn, Values: []string{sliceStr}})
	affinity.PodAntiAffinity.RequiredDuringSchedulingIgnoredDuringExecution = append(
		affinity.PodAntiAffinity.RequiredDuringSchedulingIgnoredDuringExecution,
		corev1.PodAffinityTerm{
			LabelSelector: &metav1.LabelSelector{MatchExpressions: spreadExprs},
			TopologyKey:   topologyKey,
		})

	// ExclusiveTopology also excludes every other DisaggregatedSet's slice from the
	// domain, giving a 1:1 domain-to-slice mapping. name Exists + slice Exists scopes
	// the term to DisaggregatedSet-managed pods (NotIn alone would also match pods
	// missing the name label), so unrelated workloads may still share the domain.
	if policy.Type == disaggregatedsetv1.PlacementExclusiveTopology {
		affinity.PodAntiAffinity.RequiredDuringSchedulingIgnoredDuringExecution = append(
			affinity.PodAntiAffinity.RequiredDuringSchedulingIgnoredDuringExecution,
			corev1.PodAffinityTerm{
				LabelSelector: &metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{
					{Key: disaggregatedsetv1.SetNameLabelKey, Operator: metav1.LabelSelectorOpExists},
					{Key: disaggregatedsetv1.SetNameLabelKey, Operator: metav1.LabelSelectorOpNotIn, Values: []string{dsName}},
					{Key: disaggregatedsetv1.SliceLabelKey, Operator: metav1.LabelSelectorOpExists},
				}},
				TopologyKey: topologyKey,
			})
	}

	podSpec.Affinity = affinity
}

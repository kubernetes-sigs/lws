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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	disaggregatedsetv1 "sigs.k8s.io/lws/api/disaggregatedset/v1"
)

func TestSetPlacementAffinities(t *testing.T) {
	const (
		dsName = "my-ds"
		topo   = "cloud.google.com/gke-nodepool"
	)

	t.Run("nil policy injects nothing", func(t *testing.T) {
		podSpec := &corev1.PodSpec{}
		SetPlacementAffinities(podSpec, dsName, 0, nil)
		assert.Nil(t, podSpec.Affinity)
	})

	t.Run("None injects nothing", func(t *testing.T) {
		podSpec := &corev1.PodSpec{}
		SetPlacementAffinities(podSpec, dsName, 0, &disaggregatedsetv1.PlacementPolicy{
			Type: disaggregatedsetv1.PlacementNone, Topology: topo,
		})
		assert.Nil(t, podSpec.Affinity)
	})

	t.Run("non-None with empty topology injects nothing", func(t *testing.T) {
		podSpec := &corev1.PodSpec{}
		SetPlacementAffinities(podSpec, dsName, 0, &disaggregatedsetv1.PlacementPolicy{
			Type: disaggregatedsetv1.PlacementExclusiveSlice, Topology: "",
		})
		assert.Nil(t, podSpec.Affinity)
	})

	t.Run("ExclusiveSlice injects co-location and same-set spread", func(t *testing.T) {
		podSpec := &corev1.PodSpec{}
		SetPlacementAffinities(podSpec, dsName, 1, &disaggregatedsetv1.PlacementPolicy{
			Type: disaggregatedsetv1.PlacementExclusiveSlice, Topology: topo,
		})
		require.NotNil(t, podSpec.Affinity)
		require.NotNil(t, podSpec.Affinity.PodAffinity)
		require.NotNil(t, podSpec.Affinity.PodAntiAffinity)

		affTerms := podSpec.Affinity.PodAffinity.RequiredDuringSchedulingIgnoredDuringExecution
		require.Len(t, affTerms, 1)
		assert.Equal(t, topo, affTerms[0].TopologyKey)
		assert.Equal(t, []metav1.LabelSelectorRequirement{
			{Key: disaggregatedsetv1.SetNameLabelKey, Operator: metav1.LabelSelectorOpIn, Values: []string{dsName}},
			{Key: disaggregatedsetv1.SliceLabelKey, Operator: metav1.LabelSelectorOpIn, Values: []string{"1"}},
		}, affTerms[0].LabelSelector.MatchExpressions)

		antiTerms := podSpec.Affinity.PodAntiAffinity.RequiredDuringSchedulingIgnoredDuringExecution
		require.Len(t, antiTerms, 1)
		assert.Equal(t, topo, antiTerms[0].TopologyKey)
		assert.Equal(t, []metav1.LabelSelectorRequirement{
			{Key: disaggregatedsetv1.SetNameLabelKey, Operator: metav1.LabelSelectorOpIn, Values: []string{dsName}},
			{Key: disaggregatedsetv1.SliceLabelKey, Operator: metav1.LabelSelectorOpNotIn, Values: []string{"1"}},
		}, antiTerms[0].LabelSelector.MatchExpressions)
	})

	t.Run("slice 0 spread requires the slice label to exist", func(t *testing.T) {
		// Legacy pods created before the slices feature carry no slice label and are
		// semantically slice 0. NotIn alone matches label-less pods, so without the
		// Exists requirement a slice-0 pod would be repelled by its own legacy
		// predecessor during upgrade and never schedule.
		podSpec := &corev1.PodSpec{}
		SetPlacementAffinities(podSpec, dsName, 0, &disaggregatedsetv1.PlacementPolicy{
			Type: disaggregatedsetv1.PlacementExclusiveSlice, Topology: topo,
		})
		require.NotNil(t, podSpec.Affinity)

		antiTerms := podSpec.Affinity.PodAntiAffinity.RequiredDuringSchedulingIgnoredDuringExecution
		require.Len(t, antiTerms, 1)
		assert.Equal(t, []metav1.LabelSelectorRequirement{
			{Key: disaggregatedsetv1.SetNameLabelKey, Operator: metav1.LabelSelectorOpIn, Values: []string{dsName}},
			{Key: disaggregatedsetv1.SliceLabelKey, Operator: metav1.LabelSelectorOpExists},
			{Key: disaggregatedsetv1.SliceLabelKey, Operator: metav1.LabelSelectorOpNotIn, Values: []string{"0"}},
		}, antiTerms[0].LabelSelector.MatchExpressions)
	})

	t.Run("ExclusiveTopology adds cross-set exclusion", func(t *testing.T) {
		podSpec := &corev1.PodSpec{}
		SetPlacementAffinities(podSpec, dsName, 0, &disaggregatedsetv1.PlacementPolicy{
			Type: disaggregatedsetv1.PlacementExclusiveTopology, Topology: topo,
		})
		require.NotNil(t, podSpec.Affinity)
		require.Len(t, podSpec.Affinity.PodAffinity.RequiredDuringSchedulingIgnoredDuringExecution, 1)

		antiTerms := podSpec.Affinity.PodAntiAffinity.RequiredDuringSchedulingIgnoredDuringExecution
		require.Len(t, antiTerms, 2)
		// The first anti-affinity term is the same-set spread; the second excludes
		// every other DisaggregatedSet's slice. Requiring the name label to exist
		// keeps the NotIn from matching unrelated pods that lack the label entirely.
		assert.Equal(t, topo, antiTerms[1].TopologyKey)
		assert.Equal(t, []metav1.LabelSelectorRequirement{
			{Key: disaggregatedsetv1.SetNameLabelKey, Operator: metav1.LabelSelectorOpExists},
			{Key: disaggregatedsetv1.SetNameLabelKey, Operator: metav1.LabelSelectorOpNotIn, Values: []string{dsName}},
			{Key: disaggregatedsetv1.SliceLabelKey, Operator: metav1.LabelSelectorOpExists},
		}, antiTerms[1].LabelSelector.MatchExpressions)
	})

	t.Run("preserves existing affinity without mutating the caller's copy", func(t *testing.T) {
		existing := &corev1.Affinity{
			PodAffinity: &corev1.PodAffinity{
				RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{
					{TopologyKey: "existing-key"},
				},
			},
			NodeAffinity: &corev1.NodeAffinity{},
		}
		podSpec := &corev1.PodSpec{Affinity: existing}
		SetPlacementAffinities(podSpec, dsName, 0, &disaggregatedsetv1.PlacementPolicy{
			Type: disaggregatedsetv1.PlacementExclusiveSlice, Topology: topo,
		})

		// The injected term is appended after the pre-existing one, and unrelated
		// affinity (node affinity) is retained.
		require.Len(t, podSpec.Affinity.PodAffinity.RequiredDuringSchedulingIgnoredDuringExecution, 2)
		assert.Equal(t, "existing-key", podSpec.Affinity.PodAffinity.RequiredDuringSchedulingIgnoredDuringExecution[0].TopologyKey)
		assert.NotNil(t, podSpec.Affinity.NodeAffinity)

		// The caller's original affinity object is deep-copied, not mutated in place.
		assert.Len(t, existing.PodAffinity.RequiredDuringSchedulingIgnoredDuringExecution, 1)
	})
}

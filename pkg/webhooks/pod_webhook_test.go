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

package webhooks

import (
	"testing"

	"github.com/google/go-cmp/cmp"
	leaderworkerset "sigs.k8s.io/lws/api/leaderworkerset/v1"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestGenGroupUniqueKey(t *testing.T) {
	tests := []struct {
		name        string
		podName     string
		namespace   string
		expectedKey string
	}{
		{
			name:        "same namespace, pod name",
			podName:     "test-sample",
			namespace:   "default",
			expectedKey: "95e88034e460983f51a9952fe128729fbc0663b5",
		},
		{
			name:        "same namespace, different pod name",
			podName:     "podName",
			namespace:   "default",
			expectedKey: "390b34ab671d29e9997d7d4252b8bbf8da02f5b7",
		},
		{
			name:        "different namespace, same pod name",
			podName:     "test-sample",
			namespace:   "leaderworkerset",
			expectedKey: "39f5d7e9122b9d94d3932e3720b43fd3b56347e8",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			key := genGroupUniqueKey(tc.namespace, tc.podName)
			if diff := cmp.Diff(tc.expectedKey, key); diff != "" {
				t.Errorf("unexpected key %s", diff)
			}
		})
	}
}

func TestSetExclusiveAffinities(t *testing.T) {
	tests := []struct {
		name           string
		pod            *corev1.Pod
		groupUniqueKey string
		topologyKey    string
		podAffinityKey string
		scope          leaderworkerset.PlacementType
		enforcement    leaderworkerset.PlacementEnforcement
		expectedPod    *corev1.Pod
	}{
		{
			name: "Pod with only Exclusive Topology Annotation",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{"leaderworkerset.sigs.k8s.io/exclusive-topology": "topologyKey"},
				},
			},
			groupUniqueKey: "test-key",
			topologyKey:    "topologyKey",
			podAffinityKey: leaderworkerset.GroupUniqueHashLabelKey,
			scope:          leaderworkerset.PlacementExclusiveTopology,
			expectedPod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{"leaderworkerset.sigs.k8s.io/exclusive-topology": "topologyKey"},
				},
				Spec: corev1.PodSpec{
					Affinity: &corev1.Affinity{
						PodAffinity: &corev1.PodAffinity{
							RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{{
								TopologyKey: "topologyKey",
								LabelSelector: &metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{
									{
										Key:      "leaderworkerset.sigs.k8s.io/group-key",
										Operator: "In",
										Values:   []string{"test-key"},
									},
								}},
							}},
						},
						PodAntiAffinity: &corev1.PodAntiAffinity{
							RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{{
								TopologyKey: "topologyKey",
								LabelSelector: &metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{
									{
										Key:      "leaderworkerset.sigs.k8s.io/group-key",
										Operator: "Exists",
									},
									{
										Key:      "leaderworkerset.sigs.k8s.io/group-key",
										Operator: "NotIn",
										Values:   []string{"test-key"},
									},
								}},
							}},
						},
					},
				},
			},
		},
		{
			name: "Pod with Exclusive Annotation, Affinity, and AntiAffinity",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{"leaderworkerset.sigs.k8s.io/exclusive-topology": "topologyKey"},
				},
				Spec: corev1.PodSpec{
					Affinity: &corev1.Affinity{
						PodAffinity: &corev1.PodAffinity{
							RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{{TopologyKey: "topologyKey"}},
						},
						PodAntiAffinity: &corev1.PodAntiAffinity{
							RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{{TopologyKey: "topologyKey"}},
						},
					},
				},
			},
			groupUniqueKey: "test-key",
			topologyKey:    "topologyKey",
			podAffinityKey: leaderworkerset.GroupUniqueHashLabelKey,
			scope:          leaderworkerset.PlacementExclusiveTopology,
			expectedPod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{"leaderworkerset.sigs.k8s.io/exclusive-topology": "topologyKey"},
				},
				Spec: corev1.PodSpec{
					Affinity: &corev1.Affinity{
						PodAffinity: &corev1.PodAffinity{
							// The pod's own terms are kept; the exclusive
							// placement is added next to them.
							RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{
								{TopologyKey: "topologyKey"},
								{TopologyKey: "topologyKey", LabelSelector: &metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{{
									Key:      "leaderworkerset.sigs.k8s.io/group-key",
									Operator: "In",
									Values:   []string{"test-key"},
								}}}},
							},
						},
						PodAntiAffinity: &corev1.PodAntiAffinity{
							RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{
								{TopologyKey: "topologyKey"},
								{TopologyKey: "topologyKey", LabelSelector: &metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{
									{Key: "leaderworkerset.sigs.k8s.io/group-key", Operator: "Exists"},
									{Key: "leaderworkerset.sigs.k8s.io/group-key", Operator: "NotIn", Values: []string{"test-key"}},
								}}},
							},
						},
					},
				},
			},
		},
		{
			// ExclusiveSet scopes the anti-affinity to the pods of this
			// LeaderWorkerSet via the set name label.
			name: "ExclusiveSet scopes the anti-affinity to this LeaderWorkerSet",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Labels:      map[string]string{"leaderworkerset.sigs.k8s.io/name": "test-lws"},
					Annotations: map[string]string{"leaderworkerset.sigs.k8s.io/exclusive-topology": "topologyKey"},
				},
			},
			groupUniqueKey: "test-key",
			topologyKey:    "topologyKey",
			podAffinityKey: leaderworkerset.GroupUniqueHashLabelKey,
			scope:          leaderworkerset.PlacementExclusiveSet,
			expectedPod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Labels:      map[string]string{"leaderworkerset.sigs.k8s.io/name": "test-lws"},
					Annotations: map[string]string{"leaderworkerset.sigs.k8s.io/exclusive-topology": "topologyKey"},
				},
				Spec: corev1.PodSpec{
					Affinity: &corev1.Affinity{
						PodAffinity: &corev1.PodAffinity{
							RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{{
								TopologyKey: "topologyKey",
								LabelSelector: &metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{
									{
										Key:      "leaderworkerset.sigs.k8s.io/group-key",
										Operator: "In",
										Values:   []string{"test-key"},
									},
								}},
							}},
						},
						PodAntiAffinity: &corev1.PodAntiAffinity{
							RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{{
								TopologyKey: "topologyKey",
								LabelSelector: &metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{
									{
										Key:      "leaderworkerset.sigs.k8s.io/group-key",
										Operator: "Exists",
									},
									{
										Key:      "leaderworkerset.sigs.k8s.io/group-key",
										Operator: "NotIn",
										Values:   []string{"test-key"},
									},
									{
										Key:      "leaderworkerset.sigs.k8s.io/name",
										Operator: "In",
										Values:   []string{"test-lws"},
									},
								}},
							}},
						},
					},
				},
			},
		},
		{
			// Without the set name label there is nothing to scope with, so
			// the term stays global rather than dropping the exclusivity.
			name: "ExclusiveSet without the set name label stays global",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{"leaderworkerset.sigs.k8s.io/exclusive-topology": "topologyKey"},
				},
			},
			groupUniqueKey: "test-key",
			topologyKey:    "topologyKey",
			podAffinityKey: leaderworkerset.GroupUniqueHashLabelKey,
			scope:          leaderworkerset.PlacementExclusiveSet,
			expectedPod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{"leaderworkerset.sigs.k8s.io/exclusive-topology": "topologyKey"},
				},
				Spec: corev1.PodSpec{
					Affinity: &corev1.Affinity{
						PodAffinity: &corev1.PodAffinity{
							RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{{
								TopologyKey: "topologyKey",
								LabelSelector: &metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{
									{
										Key:      "leaderworkerset.sigs.k8s.io/group-key",
										Operator: "In",
										Values:   []string{"test-key"},
									},
								}},
							}},
						},
						PodAntiAffinity: &corev1.PodAntiAffinity{
							RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{{
								TopologyKey: "topologyKey",
								LabelSelector: &metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{
									{
										Key:      "leaderworkerset.sigs.k8s.io/group-key",
										Operator: "Exists",
									},
									{
										Key:      "leaderworkerset.sigs.k8s.io/group-key",
										Operator: "NotIn",
										Values:   []string{"test-key"},
									},
								}},
							}},
						},
					},
				},
			},
		},
		{
			name: "Preferred enforcement moves the anti-affinity to a preference",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{"leaderworkerset.sigs.k8s.io/exclusive-topology": "topologyKey"},
				},
			},
			groupUniqueKey: "test-key",
			topologyKey:    "topologyKey",
			podAffinityKey: leaderworkerset.GroupUniqueHashLabelKey,
			scope:          leaderworkerset.PlacementExclusiveSet,
			enforcement:    leaderworkerset.PlacementPreferred,
			expectedPod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{"leaderworkerset.sigs.k8s.io/exclusive-topology": "topologyKey"},
				},
				Spec: corev1.PodSpec{
					Affinity: &corev1.Affinity{
						PodAffinity: &corev1.PodAffinity{
							RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{{
								TopologyKey: "topologyKey",
								LabelSelector: &metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{
									{
										Key:      "leaderworkerset.sigs.k8s.io/group-key",
										Operator: "In",
										Values:   []string{"test-key"},
									},
								}},
							}},
						},
						PodAntiAffinity: &corev1.PodAntiAffinity{
							PreferredDuringSchedulingIgnoredDuringExecution: []corev1.WeightedPodAffinityTerm{{
								Weight: 100,
								PodAffinityTerm: corev1.PodAffinityTerm{
									TopologyKey: "topologyKey",
									LabelSelector: &metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{
										{
											Key:      "leaderworkerset.sigs.k8s.io/group-key",
											Operator: "Exists",
										},
										{
											Key:      "leaderworkerset.sigs.k8s.io/group-key",
											Operator: "NotIn",
											Values:   []string{"test-key"},
										},
									}},
								},
							}},
						},
					},
				},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			SetExclusiveAffinities(tc.pod, tc.groupUniqueKey, tc.topologyKey, tc.podAffinityKey, tc.scope, tc.enforcement)
			if diff := cmp.Diff(tc.pod, tc.expectedPod); diff != "" {
				t.Errorf("unexpected set exclusive affinities operation: %s", diff)
			}
		})
	}

	t.Run("a Preferred enforcement is not injected twice", func(t *testing.T) {
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Annotations: map[string]string{"leaderworkerset.sigs.k8s.io/exclusive-topology": "topologyKey"},
			},
		}
		for range 2 {
			SetExclusiveAffinities(pod, "test-key", "topologyKey", leaderworkerset.GroupUniqueHashLabelKey,
				leaderworkerset.PlacementExclusiveSet, leaderworkerset.PlacementPreferred)
		}
		weighted := pod.Spec.Affinity.PodAntiAffinity.PreferredDuringSchedulingIgnoredDuringExecution
		if len(weighted) != 1 {
			t.Errorf("preferred anti-affinity terms = %d, want 1", len(weighted))
		}
		required := pod.Spec.Affinity.PodAffinity.RequiredDuringSchedulingIgnoredDuringExecution
		if len(required) != 1 {
			t.Errorf("required affinity terms = %d, want 1", len(required))
		}
	})

	t.Run("a user-declared preferred anti-affinity on the same topology key does not skip the injection", func(t *testing.T) {
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Annotations: map[string]string{"leaderworkerset.sigs.k8s.io/exclusive-topology": "topologyKey"},
			},
			Spec: corev1.PodSpec{
				Affinity: &corev1.Affinity{
					PodAffinity: &corev1.PodAffinity{
						RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{{TopologyKey: "topologyKey"}},
					},
					PodAntiAffinity: &corev1.PodAntiAffinity{
						PreferredDuringSchedulingIgnoredDuringExecution: []corev1.WeightedPodAffinityTerm{{
							Weight: 50,
							PodAffinityTerm: corev1.PodAffinityTerm{
								TopologyKey: "topologyKey",
								LabelSelector: &metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{{
									Key:      "app",
									Operator: metav1.LabelSelectorOpIn,
									Values:   []string{"other"},
								}}},
							},
						}},
					},
				},
			},
		}
		SetExclusiveAffinities(pod, "test-key", "topologyKey", leaderworkerset.GroupUniqueHashLabelKey,
			leaderworkerset.PlacementExclusiveSet, leaderworkerset.PlacementPreferred)
		weighted := pod.Spec.Affinity.PodAntiAffinity.PreferredDuringSchedulingIgnoredDuringExecution
		if len(weighted) != 2 {
			t.Errorf("preferred anti-affinity terms = %d, want 2 (the user term plus the injected one)", len(weighted))
		}
		injected := weighted[len(weighted)-1].PodAffinityTerm.LabelSelector
		hasExists, hasNotIn := false, false
		for _, expression := range injected.MatchExpressions {
			if expression.Key != leaderworkerset.GroupUniqueHashLabelKey {
				continue
			}
			switch expression.Operator {
			case metav1.LabelSelectorOpExists:
				hasExists = true
			case metav1.LabelSelectorOpNotIn:
				hasNotIn = len(expression.Values) == 1 && expression.Values[0] == "test-key"
			}
		}
		if !hasExists || !hasNotIn {
			t.Errorf("injected preferred anti-affinity selector = %#v, want group-key Exists and NotIn test-key", injected)
		}
	})

	t.Run("a user-declared required affinity and anti-affinity on the same topology key do not skip the injection", func(t *testing.T) {
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{leaderworkerset.SetNameLabelKey: "test-lws"}},
			Spec: corev1.PodSpec{
				Affinity: &corev1.Affinity{
					PodAffinity: &corev1.PodAffinity{
						RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{{
							TopologyKey:   "topologyKey",
							LabelSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "cache"}},
						}},
					},
					PodAntiAffinity: &corev1.PodAntiAffinity{
						RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{{
							TopologyKey:   "topologyKey",
							LabelSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "noisy"}},
						}},
					},
				},
			},
		}
		SetExclusiveAffinities(pod, "test-key", "topologyKey", leaderworkerset.GroupUniqueHashLabelKey,
			leaderworkerset.PlacementExclusiveSet, leaderworkerset.PlacementRequired)
		if got := len(pod.Spec.Affinity.PodAffinity.RequiredDuringSchedulingIgnoredDuringExecution); got != 2 {
			t.Errorf("required affinity terms = %d, want 2 (the user term plus the injected one)", got)
		}
		if got := len(pod.Spec.Affinity.PodAntiAffinity.RequiredDuringSchedulingIgnoredDuringExecution); got != 2 {
			t.Errorf("required anti-affinity terms = %d, want 2 (the user term plus the injected one)", got)
		}
		if !hasExclusiveTerm(pod.Spec.Affinity.PodAffinity.RequiredDuringSchedulingIgnoredDuringExecution, "topologyKey", leaderworkerset.GroupUniqueHashLabelKey) {
			t.Error("expected the injected affinity term to be present")
		}
		if !hasExclusiveTerm(pod.Spec.Affinity.PodAntiAffinity.RequiredDuringSchedulingIgnoredDuringExecution, "topologyKey", leaderworkerset.GroupUniqueHashLabelKey) {
			t.Error("expected the injected anti-affinity term to be present")
		}
	})

	t.Run("a Required enforcement is not injected twice", func(t *testing.T) {
		pod := &corev1.Pod{}
		for range 2 {
			SetExclusiveAffinities(pod, "test-key", "topologyKey", leaderworkerset.GroupUniqueHashLabelKey,
				leaderworkerset.PlacementExclusiveTopology, leaderworkerset.PlacementRequired)
		}
		if got := len(pod.Spec.Affinity.PodAffinity.RequiredDuringSchedulingIgnoredDuringExecution); got != 1 {
			t.Errorf("required affinity terms = %d, want 1", got)
		}
		if got := len(pod.Spec.Affinity.PodAntiAffinity.RequiredDuringSchedulingIgnoredDuringExecution); got != 1 {
			t.Errorf("required anti-affinity terms = %d, want 1", got)
		}
	})
}

func TestHasExclusiveTerm(t *testing.T) {
	term := func(topologyKey string, expressions ...metav1.LabelSelectorRequirement) corev1.PodAffinityTerm {
		return corev1.PodAffinityTerm{
			TopologyKey:   topologyKey,
			LabelSelector: &metav1.LabelSelector{MatchExpressions: expressions},
		}
	}
	groupKey := metav1.LabelSelectorRequirement{Key: leaderworkerset.GroupUniqueHashLabelKey, Operator: metav1.LabelSelectorOpExists}
	tests := []struct {
		name         string
		terms        []corev1.PodAffinityTerm
		topologyKey  string
		expectedTerm bool
	}{
		{
			name: "the injected exclusive placement",
			terms: []corev1.PodAffinityTerm{term("topologyKey",
				groupKey,
				metav1.LabelSelectorRequirement{Key: leaderworkerset.GroupUniqueHashLabelKey, Operator: metav1.LabelSelectorOpNotIn, Values: []string{"test-key"}})},
			topologyKey:  "topologyKey",
			expectedTerm: true,
		},
		{
			name: "a stale group key still counts as applied",
			terms: []corev1.PodAffinityTerm{term("topologyKey",
				groupKey,
				metav1.LabelSelectorRequirement{Key: leaderworkerset.GroupUniqueHashLabelKey, Operator: metav1.LabelSelectorOpNotIn, Values: []string{"a-stale-key"}})},
			topologyKey:  "topologyKey",
			expectedTerm: true,
		},
		{
			name: "a user-declared term on the same topology key is not the placement",
			terms: []corev1.PodAffinityTerm{term("topologyKey",
				metav1.LabelSelectorRequirement{Key: "app", Operator: metav1.LabelSelectorOpIn, Values: []string{"other"}})},
			topologyKey:  "topologyKey",
			expectedTerm: false,
		},
		{
			name:         "a different topology key is not the placement",
			terms:        []corev1.PodAffinityTerm{term("topologyKey1", groupKey)},
			topologyKey:  "topologyKey",
			expectedTerm: false,
		},
		{
			name:         "a term without a selector is not the placement",
			terms:        []corev1.PodAffinityTerm{{TopologyKey: "topologyKey"}},
			topologyKey:  "topologyKey",
			expectedTerm: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := hasExclusiveTerm(tc.terms, tc.topologyKey, leaderworkerset.GroupUniqueHashLabelKey); got != tc.expectedTerm {
				t.Errorf("hasExclusiveTerm() = %t, want %t", got, tc.expectedTerm)
			}
		})
	}
}

func TestGetSubGroupIndex(t *testing.T) {
	tests := []struct {
		name                  string
		podCount              int
		subGroupSize          int
		workerIndex           int
		leaderOnly            bool
		expectedSubGroupIndex string
	}{
		{
			name:                  "Even number of pods",
			podCount:              4,
			subGroupSize:          2,
			workerIndex:           2,
			expectedSubGroupIndex: "1",
		},
		{
			name:                  "Odd number of pods, first subgroup has an extra pod",
			podCount:              5,
			subGroupSize:          2,
			workerIndex:           2,
			expectedSubGroupIndex: "0",
		},
		{
			name:                  "SubGroupSize 1, workerIndex 1",
			podCount:              3,
			subGroupSize:          1,
			workerIndex:           1,
			expectedSubGroupIndex: "1",
		},
		{
			name:                  "SubGroupSize 1, workerIndex 2",
			podCount:              3,
			subGroupSize:          1,
			workerIndex:           2,
			expectedSubGroupIndex: "2",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			subGroupIndex := getSubGroupIndex(tc.podCount, tc.subGroupSize, tc.workerIndex)
			if tc.expectedSubGroupIndex != subGroupIndex {
				t.Errorf("Expected subGroupIndex to be %s, got %s", tc.expectedSubGroupIndex, subGroupIndex)
			}
		})
	}
}

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
			name: "Pod with the injected placement already applied",
			pod: func() *corev1.Pod {
				pod := &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Annotations: map[string]string{"leaderworkerset.sigs.k8s.io/exclusive-topology": "topologyKey"},
					},
				}
				SetExclusiveAffinities(pod, "test-key", "topologyKey", leaderworkerset.GroupUniqueHashLabelKey)
				return pod
			}(),
			groupUniqueKey: "test-key",
			topologyKey:    "topologyKey",
			podAffinityKey: leaderworkerset.GroupUniqueHashLabelKey,
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
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			SetExclusiveAffinities(tc.pod, tc.groupUniqueKey, tc.topologyKey, tc.podAffinityKey)
			if diff := cmp.Diff(tc.pod, tc.expectedPod); diff != "" {
				t.Errorf("unexpected set exclusive affinities operation: %s", diff)
			}
		})
	}

	const (
		exclusiveTopologyKey = "topologyKey"
		exclusiveGroupKey    = "test-key"
	)

	t.Run("user affinity terms on the same topology key are preserved", func(t *testing.T) {
		pod := testPodWithUserTerms(exclusiveTopologyKey)
		SetExclusiveAffinities(pod, exclusiveGroupKey, exclusiveTopologyKey, leaderworkerset.GroupUniqueHashLabelKey)

		if got := len(pod.Spec.Affinity.PodAffinity.RequiredDuringSchedulingIgnoredDuringExecution); got != 2 {
			t.Fatalf("pod affinity terms = %d, want 2 (the user's plus the injected one)", got)
		}
		if got := len(pod.Spec.Affinity.PodAntiAffinity.RequiredDuringSchedulingIgnoredDuringExecution); got != 2 {
			t.Fatalf("pod anti-affinity terms = %d, want 2 (the user's plus the injected one)", got)
		}
	})

	t.Run("a term carrying a stale group key is not duplicated", func(t *testing.T) {
		pod := testPodWithUserTerms(exclusiveTopologyKey)
		pod.Spec.Affinity.PodAffinity.RequiredDuringSchedulingIgnoredDuringExecution = append(
			pod.Spec.Affinity.PodAffinity.RequiredDuringSchedulingIgnoredDuringExecution,
			injectedAffinityTerm("stale-key", exclusiveTopologyKey))
		pod.Spec.Affinity.PodAntiAffinity.RequiredDuringSchedulingIgnoredDuringExecution = append(
			pod.Spec.Affinity.PodAntiAffinity.RequiredDuringSchedulingIgnoredDuringExecution,
			injectedAntiAffinityTerm("stale-key", exclusiveTopologyKey))

		SetExclusiveAffinities(pod, exclusiveGroupKey, exclusiveTopologyKey, leaderworkerset.GroupUniqueHashLabelKey)

		if got := len(pod.Spec.Affinity.PodAffinity.RequiredDuringSchedulingIgnoredDuringExecution); got != 2 {
			t.Errorf("pod affinity terms = %d, want 2: a stale group key must not be injected again", got)
		}
		if got := len(pod.Spec.Affinity.PodAntiAffinity.RequiredDuringSchedulingIgnoredDuringExecution); got != 2 {
			t.Errorf("pod anti-affinity terms = %d, want 2: a stale group key must not be injected again", got)
		}
	})

	t.Run("a partially injected placement only fills the missing list", func(t *testing.T) {
		pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "test-0", Namespace: "default"}}
		pod.Spec.Affinity = &corev1.Affinity{
			PodAffinity: &corev1.PodAffinity{
				RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{
					injectedAffinityTerm(exclusiveGroupKey, exclusiveTopologyKey),
				},
			},
			PodAntiAffinity: &corev1.PodAntiAffinity{},
		}

		SetExclusiveAffinities(pod, exclusiveGroupKey, exclusiveTopologyKey, leaderworkerset.GroupUniqueHashLabelKey)

		if got := len(pod.Spec.Affinity.PodAffinity.RequiredDuringSchedulingIgnoredDuringExecution); got != 1 {
			t.Errorf("pod affinity terms = %d, want 1: the affinity half is already there", got)
		}
		if got := len(pod.Spec.Affinity.PodAntiAffinity.RequiredDuringSchedulingIgnoredDuringExecution); got != 1 {
			t.Errorf("pod anti-affinity terms = %d, want 1: the missing half must be filled in", got)
		}
	})
}

// testPodWithUserTerms returns a pod whose template already declares its own
// required affinity pair on topologyKey, selecting a workload other than the
// exclusive placement.
func testPodWithUserTerms(topologyKey string) *corev1.Pod {
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "test-0", Namespace: "default"}}
	pod.Spec.Affinity = &corev1.Affinity{
		PodAffinity: &corev1.PodAffinity{
			RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{{
				LabelSelector: &metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{
					{
						Key:      "tier",
						Operator: metav1.LabelSelectorOpIn,
						Values:   []string{"cache"},
					},
				}},
				TopologyKey: topologyKey,
			}},
		},
		PodAntiAffinity: &corev1.PodAntiAffinity{
			RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{{
				LabelSelector: &metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{
					{
						Key:      "tier",
						Operator: metav1.LabelSelectorOpExists,
					},
				}},
				TopologyKey: topologyKey,
			}},
		},
	}
	return pod
}

func injectedAffinityTerm(groupUniqueKey, topologyKey string) corev1.PodAffinityTerm {
	return corev1.PodAffinityTerm{
		LabelSelector: &metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{
			{
				Key:      leaderworkerset.GroupUniqueHashLabelKey,
				Operator: metav1.LabelSelectorOpIn,
				Values:   []string{groupUniqueKey},
			},
		}},
		TopologyKey: topologyKey,
	}
}

func injectedAntiAffinityTerm(groupUniqueKey, topologyKey string) corev1.PodAffinityTerm {
	return corev1.PodAffinityTerm{
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
		TopologyKey: topologyKey,
	}
}

func TestHasExclusiveTerm(t *testing.T) {
	const (
		topologyKey    = "topologyKey"
		podAffinityKey = "leaderworkerset.sigs.k8s.io/group-key"
	)
	terms := func(topologyKey string, expressions ...metav1.LabelSelectorRequirement) []corev1.PodAffinityTerm {
		return []corev1.PodAffinityTerm{{
			LabelSelector: &metav1.LabelSelector{MatchExpressions: expressions},
			TopologyKey:   topologyKey,
		}}
	}

	tests := []struct {
		name     string
		terms    []corev1.PodAffinityTerm
		expected bool
	}{
		{
			name:     "empty list",
			terms:    nil,
			expected: false,
		},
		{
			name: "term on another topology key",
			terms: terms("rack", metav1.LabelSelectorRequirement{
				Key:      podAffinityKey,
				Operator: metav1.LabelSelectorOpExists,
			}),
			expected: false,
		},
		{
			name:     "term without a selector",
			terms:    []corev1.PodAffinityTerm{{TopologyKey: topologyKey}},
			expected: false,
		},
		{
			name: "term selecting another workload",
			terms: terms(topologyKey, metav1.LabelSelectorRequirement{
				Key:      "tier",
				Operator: metav1.LabelSelectorOpIn,
				Values:   []string{"cache"},
			}),
			expected: false,
		},
		{
			name: "term carrying the injected In expression",
			terms: terms(topologyKey, metav1.LabelSelectorRequirement{
				Key:      podAffinityKey,
				Operator: metav1.LabelSelectorOpIn,
				Values:   []string{"some-hash"},
			}),
			expected: true,
		},
		{
			name: "term carrying the injected Exists expression",
			terms: terms(topologyKey, metav1.LabelSelectorRequirement{
				Key:      podAffinityKey,
				Operator: metav1.LabelSelectorOpExists,
			}),
			expected: true,
		},
		{
			name: "a stale group key value still counts as applied",
			terms: terms(topologyKey, metav1.LabelSelectorRequirement{
				Key:      podAffinityKey,
				Operator: metav1.LabelSelectorOpIn,
				Values:   []string{"a-different-hash"},
			}),
			expected: true,
		},
		{
			name: "term carrying the group key plus a scope expression",
			terms: terms(topologyKey,
				metav1.LabelSelectorRequirement{
					Key:      podAffinityKey,
					Operator: metav1.LabelSelectorOpExists,
				},
				metav1.LabelSelectorRequirement{
					Key:      leaderworkerset.SetNameLabelKey,
					Operator: metav1.LabelSelectorOpIn,
					Values:   []string{"test"},
				},
			),
			expected: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := hasExclusiveTerm(tc.terms, topologyKey, podAffinityKey); got != tc.expected {
				t.Errorf("hasExclusiveTerm() = %t, want %t", got, tc.expected)
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
		leaderExcluded        bool
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
		{
			name:                  "LeaderExcluded, size 2, SubGroupSize 1, workerIndex 1",
			podCount:              2,
			subGroupSize:          1,
			workerIndex:           1,
			leaderExcluded:        true,
			expectedSubGroupIndex: "0",
		},
		{
			name:                  "LeaderExcluded, size 4, SubGroupSize 1, workerIndex 1",
			podCount:              4,
			subGroupSize:          1,
			workerIndex:           1,
			leaderExcluded:        true,
			expectedSubGroupIndex: "0",
		},
		{
			name:                  "LeaderExcluded, size 4, SubGroupSize 1, workerIndex 2",
			podCount:              4,
			subGroupSize:          1,
			workerIndex:           2,
			leaderExcluded:        true,
			expectedSubGroupIndex: "1",
		},
		{
			name:                  "LeaderExcluded, size 4, SubGroupSize 1, workerIndex 3",
			podCount:              4,
			subGroupSize:          1,
			workerIndex:           3,
			leaderExcluded:        true,
			expectedSubGroupIndex: "2",
		},
		{
			name:                  "LeaderExcluded, size 5, SubGroupSize 2, workerIndex 2",
			podCount:              5,
			subGroupSize:          2,
			workerIndex:           2,
			leaderExcluded:        true,
			expectedSubGroupIndex: "0",
		},
		{
			name:                  "LeaderExcluded, size 5, SubGroupSize 2, workerIndex 3",
			podCount:              5,
			subGroupSize:          2,
			workerIndex:           3,
			leaderExcluded:        true,
			expectedSubGroupIndex: "1",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			subGroupIndex := getSubGroupIndex(tc.podCount, tc.subGroupSize, tc.workerIndex, tc.leaderExcluded)
			if tc.expectedSubGroupIndex != subGroupIndex {
				t.Errorf("Expected subGroupIndex to be %s, got %s", tc.expectedSubGroupIndex, subGroupIndex)
			}
		})
	}
}

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
}

func TestExclusiveAffinityApplied(t *testing.T) {
	const (
		groupKey    = "test-key"
		topologyKey = "topologyKey"
	)
	podWith := func(affinity *corev1.Affinity) corev1.Pod {
		return corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Annotations: map[string]string{
					"leaderworkerset.sigs.k8s.io/exclusive-topology": topologyKey,
				},
			},
			Spec: corev1.PodSpec{Affinity: affinity},
		}
	}
	terms := func(topologyKey string, expressions ...metav1.LabelSelectorRequirement) []corev1.PodAffinityTerm {
		return []corev1.PodAffinityTerm{{
			LabelSelector: &metav1.LabelSelector{MatchExpressions: expressions},
			TopologyKey:   topologyKey,
		}}
	}
	groupAffinity := terms(topologyKey, metav1.LabelSelectorRequirement{
		Key:      leaderworkerset.GroupUniqueHashLabelKey,
		Operator: metav1.LabelSelectorOpIn,
		Values:   []string{groupKey},
	})
	groupAntiAffinity := terms(topologyKey,
		metav1.LabelSelectorRequirement{
			Key:      leaderworkerset.GroupUniqueHashLabelKey,
			Operator: metav1.LabelSelectorOpExists,
		},
		metav1.LabelSelectorRequirement{
			Key:      leaderworkerset.GroupUniqueHashLabelKey,
			Operator: metav1.LabelSelectorOpNotIn,
			Values:   []string{groupKey},
		},
	)
	foreignAffinity := terms(topologyKey, metav1.LabelSelectorRequirement{
		Key:      "tier",
		Operator: metav1.LabelSelectorOpIn,
		Values:   []string{"cache"},
	})
	foreignAntiAffinity := terms(topologyKey, metav1.LabelSelectorRequirement{
		Key:      "tier",
		Operator: metav1.LabelSelectorOpExists,
	})

	tests := []struct {
		name                              string
		pod                               corev1.Pod
		expectedAppliedExclusivePlacement bool
	}{
		{
			name: "the injected placement is applied",
			pod: podWith(&corev1.Affinity{
				PodAffinity:     &corev1.PodAffinity{RequiredDuringSchedulingIgnoredDuringExecution: groupAffinity},
				PodAntiAffinity: &corev1.PodAntiAffinity{RequiredDuringSchedulingIgnoredDuringExecution: groupAntiAffinity},
			}),
			expectedAppliedExclusivePlacement: true,
		},
		{
			name: "the anti-affinity carries the group expressions plus a scope expression",
			pod: podWith(&corev1.Affinity{
				PodAffinity: &corev1.PodAffinity{
					RequiredDuringSchedulingIgnoredDuringExecution: groupAffinity,
				},
				PodAntiAffinity: &corev1.PodAntiAffinity{
					RequiredDuringSchedulingIgnoredDuringExecution: terms(topologyKey,
						metav1.LabelSelectorRequirement{
							Key:      leaderworkerset.GroupUniqueHashLabelKey,
							Operator: metav1.LabelSelectorOpExists,
						},
						metav1.LabelSelectorRequirement{
							Key:      leaderworkerset.GroupUniqueHashLabelKey,
							Operator: metav1.LabelSelectorOpNotIn,
							Values:   []string{groupKey},
						},
						metav1.LabelSelectorRequirement{
							Key:      leaderworkerset.SetNameLabelKey,
							Operator: metav1.LabelSelectorOpIn,
							Values:   []string{"test"},
						},
					),
				},
			}),
			expectedAppliedExclusivePlacement: true,
		},
		{
			name: "has pod affinity only",
			pod: podWith(&corev1.Affinity{
				PodAffinity: &corev1.PodAffinity{RequiredDuringSchedulingIgnoredDuringExecution: groupAffinity},
			}),
			expectedAppliedExclusivePlacement: false,
		},
		{
			name: "has pod anti-affinity only",
			pod: podWith(&corev1.Affinity{
				PodAntiAffinity: &corev1.PodAntiAffinity{RequiredDuringSchedulingIgnoredDuringExecution: groupAntiAffinity},
			}),
			expectedAppliedExclusivePlacement: false,
		},
		{
			name: "topology key does not match",
			pod: podWith(&corev1.Affinity{
				PodAffinity:     &corev1.PodAffinity{RequiredDuringSchedulingIgnoredDuringExecution: terms("topologyKey1", groupAffinity[0].LabelSelector.MatchExpressions...)},
				PodAntiAffinity: &corev1.PodAntiAffinity{RequiredDuringSchedulingIgnoredDuringExecution: groupAntiAffinity},
			}),
			expectedAppliedExclusivePlacement: false,
		},
		{
			name: "both terms exist but select another workload",
			pod: podWith(&corev1.Affinity{
				PodAffinity:     &corev1.PodAffinity{RequiredDuringSchedulingIgnoredDuringExecution: foreignAffinity},
				PodAntiAffinity: &corev1.PodAntiAffinity{RequiredDuringSchedulingIgnoredDuringExecution: foreignAntiAffinity},
			}),
			expectedAppliedExclusivePlacement: false,
		},
		{
			name: "the anti-affinity selects another workload",
			pod: podWith(&corev1.Affinity{
				PodAffinity:     &corev1.PodAffinity{RequiredDuringSchedulingIgnoredDuringExecution: groupAffinity},
				PodAntiAffinity: &corev1.PodAntiAffinity{RequiredDuringSchedulingIgnoredDuringExecution: foreignAntiAffinity},
			}),
			expectedAppliedExclusivePlacement: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			appliedExclusivePlacement := exclusiveAffinityApplied(tc.pod, groupKey, topologyKey, leaderworkerset.GroupUniqueHashLabelKey)
			if appliedExclusivePlacement != tc.expectedAppliedExclusivePlacement {
				t.Errorf("Expected value %t, got %t", tc.expectedAppliedExclusivePlacement, appliedExclusivePlacement)
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

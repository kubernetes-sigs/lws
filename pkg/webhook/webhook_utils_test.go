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
	"testing"

	"github.com/google/go-cmp/cmp"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
		expectedPod    *corev1.Pod
	}{
		{
			name: "Pod with only Exclusive Topology Annotation",
			pod: &corev1.Pod{
				ObjectMeta: v1.ObjectMeta{
					Annotations: map[string]string{"leaderworkerset.sigs.k8s.io/exclusive-topology": "topologyKey"},
				},
			},
			groupUniqueKey: "test-key",
			expectedPod: &corev1.Pod{
				ObjectMeta: v1.ObjectMeta{
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
				ObjectMeta: v1.ObjectMeta{
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
			expectedPod: &corev1.Pod{
				ObjectMeta: v1.ObjectMeta{
					Annotations: map[string]string{"leaderworkerset.sigs.k8s.io/exclusive-topology": "topologyKey"},
				},
				Spec: corev1.PodSpec{
					Affinity: &corev1.Affinity{
						PodAffinity: &corev1.PodAffinity{
							RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{
								{
									TopologyKey: "topologyKey",
								},
								{
									TopologyKey: "topologyKey",
									LabelSelector: &metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{
										{
											Key:      "leaderworkerset.sigs.k8s.io/group-key",
											Operator: "In",
											Values:   []string{"test-key"},
										},
									}},
								},
							},
						},
						PodAntiAffinity: &corev1.PodAntiAffinity{
							RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{
								{
									TopologyKey: "topologyKey",
								},
								{
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
							},
						},
					},
				},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			SetExclusiveAffinities(tc.pod, tc.groupUniqueKey)
			if diff := cmp.Diff(tc.pod, tc.expectedPod); diff != "" {
				t.Errorf("unexpected set exclusive affinities operation: %s", diff)
			}
		})
	}
}

func TestExclusiveAffinityApplied(t *testing.T) {
	tests := []struct {
		name                              string
		pod                               corev1.Pod
		expectedAppliedExclusivePlacement bool
	}{
		{
			name: "Has annotiation, Pod Affinity and Pod AntiAffinity",
			pod: corev1.Pod{
				ObjectMeta: v1.ObjectMeta{
					Annotations: map[string]string{
						"leaderworkerset.sigs.k8s.io/exclusive-topology": "topologyKey",
					},
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
			expectedAppliedExclusivePlacement: true,
		},
		{
			name: "Has annotiation, Pod Affinity, doesn't have Pod AntiAffinity",
			pod: corev1.Pod{
				ObjectMeta: v1.ObjectMeta{
					Annotations: map[string]string{
						"leaderworkerset.sigs.k8s.io/exclusive-topology": "topologyKey",
					},
				},
				Spec: corev1.PodSpec{
					Affinity: &corev1.Affinity{
						PodAffinity: &corev1.PodAffinity{
							RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{{TopologyKey: "topologyKey"}},
						},
					},
				},
			},
			expectedAppliedExclusivePlacement: false,
		},
		{
			name: "Has annotiation, Pod AntiAffinity, doesn't have Pod Affinity",
			pod: corev1.Pod{
				ObjectMeta: v1.ObjectMeta{
					Annotations: map[string]string{
						"leaderworkerset.sigs.k8s.io/exclusive-topology": "topologyKey",
					},
				},
				Spec: corev1.PodSpec{
					Affinity: &corev1.Affinity{
						PodAntiAffinity: &corev1.PodAntiAffinity{
							RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{{TopologyKey: "topologyKey"}},
						},
					},
				},
			},
			expectedAppliedExclusivePlacement: false,
		},
		{
			name: "Has annotiation, Pod Affinity and Pod AntiAffinity, Topology Key doesn't match",
			pod: corev1.Pod{
				ObjectMeta: v1.ObjectMeta{
					Annotations: map[string]string{
						"leaderworkerset.sigs.k8s.io/exclusive-topology": "topologyKey",
					},
				},
				Spec: corev1.PodSpec{
					Affinity: &corev1.Affinity{
						PodAffinity: &corev1.PodAffinity{
							RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{{TopologyKey: "topologyKey1"}},
						},
						PodAntiAffinity: &corev1.PodAntiAffinity{
							RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{{TopologyKey: "topologyKey"}},
						},
					},
				},
			},
			expectedAppliedExclusivePlacement: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			appliedExclusivePlacement := exclusiveAffinityApplied(tc.pod)
			if appliedExclusivePlacement != tc.expectedAppliedExclusivePlacement {
				t.Errorf("Expected value %t, got %t", tc.expectedAppliedExclusivePlacement, appliedExclusivePlacement)
			}
		})
	}
}

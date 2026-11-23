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
		scope          leaderworkerset.PlacementScope
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
			scope:          leaderworkerset.PlacementScopeAll,
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
			scope:          leaderworkerset.PlacementScopeAll,
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
			// Own scope scopes the anti-affinity to the pods of this
			// LeaderWorkerSet via the set name label.
			name: "Own scope scopes the anti-affinity to this LeaderWorkerSet",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Labels:      map[string]string{"leaderworkerset.sigs.k8s.io/name": "test-lws"},
					Annotations: map[string]string{"leaderworkerset.sigs.k8s.io/exclusive-topology": "topologyKey"},
				},
			},
			groupUniqueKey: "test-key",
			topologyKey:    "topologyKey",
			podAffinityKey: leaderworkerset.GroupUniqueHashLabelKey,
			scope:          leaderworkerset.PlacementScopeOwn,
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
			name: "Own scope without the set name label stays global",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{"leaderworkerset.sigs.k8s.io/exclusive-topology": "topologyKey"},
				},
			},
			groupUniqueKey: "test-key",
			topologyKey:    "topologyKey",
			podAffinityKey: leaderworkerset.GroupUniqueHashLabelKey,
			scope:          leaderworkerset.PlacementScopeOwn,
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
			scope:          leaderworkerset.PlacementScopeOwn,
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
				leaderworkerset.PlacementScopeOwn, leaderworkerset.PlacementPreferred)
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
			leaderworkerset.PlacementScopeOwn, leaderworkerset.PlacementPreferred)
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

	// This covers term injection only. A required affinity to pods that do not
	// exist is unsatisfiable on its own; the injected own-group affinity does
	// not add a new deadlock because the scheduler's self-affinity check
	// satisfies it from the leader's own group-key label.
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
			leaderworkerset.PlacementScopeOwn, leaderworkerset.PlacementRequired)
		if got := len(pod.Spec.Affinity.PodAffinity.RequiredDuringSchedulingIgnoredDuringExecution); got != 2 {
			t.Errorf("required affinity terms = %d, want 2 (the user term plus the injected one)", got)
		}
		if got := len(pod.Spec.Affinity.PodAntiAffinity.RequiredDuringSchedulingIgnoredDuringExecution); got != 2 {
			t.Errorf("required anti-affinity terms = %d, want 2 (the user term plus the injected one)", got)
		}
		if !hasExclusiveTerm(pod.Spec.Affinity.PodAffinity.RequiredDuringSchedulingIgnoredDuringExecution, exclusiveAffinityTerm("topologyKey", leaderworkerset.GroupUniqueHashLabelKey, "test-key"), leaderworkerset.GroupUniqueHashLabelKey) {
			t.Error("expected the injected affinity term to be present")
		}
		antiAffinity := exclusiveAntiAffinityTerm("topologyKey", leaderworkerset.GroupUniqueHashLabelKey, "test-key", leaderworkerset.PlacementScopeOwn, "test-lws")
		if !hasExclusiveTerm(pod.Spec.Affinity.PodAntiAffinity.RequiredDuringSchedulingIgnoredDuringExecution, antiAffinity, leaderworkerset.GroupUniqueHashLabelKey) {
			t.Error("expected the injected anti-affinity term to be present")
		}
	})

	t.Run("a user DoesNotExist anti-affinity does not suppress the injection", func(t *testing.T) {
		pod := &corev1.Pod{
			Spec: corev1.PodSpec{
				Affinity: &corev1.Affinity{
					PodAntiAffinity: &corev1.PodAntiAffinity{
						RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{{
							TopologyKey: "topologyKey",
							LabelSelector: &metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{{
								Key:      leaderworkerset.GroupUniqueHashLabelKey,
								Operator: metav1.LabelSelectorOpDoesNotExist,
							}}},
						}},
					},
				},
			},
		}
		SetExclusiveAffinities(pod, "test-key", "topologyKey", leaderworkerset.GroupUniqueHashLabelKey,
			leaderworkerset.PlacementScopeAll, leaderworkerset.PlacementRequired)
		terms := pod.Spec.Affinity.PodAntiAffinity.RequiredDuringSchedulingIgnoredDuringExecution
		if len(terms) != 2 {
			t.Fatalf("required anti-affinity terms = %d, want 2 (the user term plus the injected one)", len(terms))
		}
		if !hasExclusiveTerm(terms, exclusiveAntiAffinityTerm("topologyKey", leaderworkerset.GroupUniqueHashLabelKey, "test-key", leaderworkerset.PlacementScopeAll, ""), leaderworkerset.GroupUniqueHashLabelKey) {
			t.Error("expected the injected anti-affinity term to be present")
		}
	})

	t.Run("a preferred term with another weight does not suppress the injection", func(t *testing.T) {
		userTerm := exclusiveAntiAffinityTerm("topologyKey", leaderworkerset.GroupUniqueHashLabelKey, "test-key", leaderworkerset.PlacementScopeAll, "")
		pod := &corev1.Pod{
			Spec: corev1.PodSpec{
				Affinity: &corev1.Affinity{
					PodAntiAffinity: &corev1.PodAntiAffinity{
						PreferredDuringSchedulingIgnoredDuringExecution: []corev1.WeightedPodAffinityTerm{{
							Weight:          50,
							PodAffinityTerm: userTerm,
						}},
					},
				},
			},
		}
		SetExclusiveAffinities(pod, "test-key", "topologyKey", leaderworkerset.GroupUniqueHashLabelKey,
			leaderworkerset.PlacementScopeAll, leaderworkerset.PlacementPreferred)
		weighted := pod.Spec.Affinity.PodAntiAffinity.PreferredDuringSchedulingIgnoredDuringExecution
		if len(weighted) != 2 {
			t.Fatalf("preferred anti-affinity terms = %d, want 2 (the user term plus the injected one)", len(weighted))
		}
		if got := weighted[len(weighted)-1].Weight; got != preferredEnforcementWeight {
			t.Errorf("injected preferred weight = %d, want %d", got, preferredEnforcementWeight)
		}
	})

	t.Run("a Required enforcement is not injected twice", func(t *testing.T) {
		pod := &corev1.Pod{}
		for range 2 {
			SetExclusiveAffinities(pod, "test-key", "topologyKey", leaderworkerset.GroupUniqueHashLabelKey,
				leaderworkerset.PlacementScopeAll, leaderworkerset.PlacementRequired)
		}
		if got := len(pod.Spec.Affinity.PodAffinity.RequiredDuringSchedulingIgnoredDuringExecution); got != 1 {
			t.Errorf("required affinity terms = %d, want 1", got)
		}
		if got := len(pod.Spec.Affinity.PodAntiAffinity.RequiredDuringSchedulingIgnoredDuringExecution); got != 1 {
			t.Errorf("required anti-affinity terms = %d, want 1", got)
		}
	})
}

func exclusiveAffinityTerm(topologyKey, labelKey, groupUniqueKey string) corev1.PodAffinityTerm {
	return corev1.PodAffinityTerm{
		TopologyKey: topologyKey,
		LabelSelector: &metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{{
			Key:      labelKey,
			Operator: metav1.LabelSelectorOpIn,
			Values:   []string{groupUniqueKey},
		}}},
	}
}

func exclusiveAntiAffinityTerm(topologyKey, labelKey, groupUniqueKey string, scope leaderworkerset.PlacementScope, lwsName string) corev1.PodAffinityTerm {
	expressions := []metav1.LabelSelectorRequirement{
		{Key: labelKey, Operator: metav1.LabelSelectorOpExists},
		{Key: labelKey, Operator: metav1.LabelSelectorOpNotIn, Values: []string{groupUniqueKey}},
	}
	if scope == leaderworkerset.PlacementScopeOwn {
		expressions = append(expressions, metav1.LabelSelectorRequirement{
			Key:      leaderworkerset.SetNameLabelKey,
			Operator: metav1.LabelSelectorOpIn,
			Values:   []string{lwsName},
		})
	}
	return corev1.PodAffinityTerm{
		TopologyKey:   topologyKey,
		LabelSelector: &metav1.LabelSelector{MatchExpressions: expressions},
	}
}

func TestHasExclusiveTerm(t *testing.T) {
	term := func(topologyKey string, expressions ...metav1.LabelSelectorRequirement) corev1.PodAffinityTerm {
		return corev1.PodAffinityTerm{
			TopologyKey:   topologyKey,
			LabelSelector: &metav1.LabelSelector{MatchExpressions: expressions},
		}
	}
	expected := exclusiveAntiAffinityTerm("topologyKey", leaderworkerset.GroupUniqueHashLabelKey, "test-key", leaderworkerset.PlacementScopeAll, "")
	groupKey := metav1.LabelSelectorRequirement{Key: leaderworkerset.GroupUniqueHashLabelKey, Operator: metav1.LabelSelectorOpExists}
	notIn := func(values ...string) metav1.LabelSelectorRequirement {
		return metav1.LabelSelectorRequirement{Key: leaderworkerset.GroupUniqueHashLabelKey, Operator: metav1.LabelSelectorOpNotIn, Values: values}
	}
	tests := []struct {
		name         string
		terms        []corev1.PodAffinityTerm
		expectedTerm bool
	}{
		{
			name:         "the injected exclusive placement",
			terms:        []corev1.PodAffinityTerm{expected},
			expectedTerm: true,
		},
		{
			name:         "a stale group key still counts as applied",
			terms:        []corev1.PodAffinityTerm{term("topologyKey", groupKey, notIn("a-stale-key"))},
			expectedTerm: true,
		},
		{
			name:         "a DoesNotExist user term does not suppress the placement",
			terms:        []corev1.PodAffinityTerm{term("topologyKey", metav1.LabelSelectorRequirement{Key: leaderworkerset.GroupUniqueHashLabelKey, Operator: metav1.LabelSelectorOpDoesNotExist})},
			expectedTerm: false,
		},
		{
			name:         "a user term with only the group-key expressions is not the placement",
			terms:        []corev1.PodAffinityTerm{term("topologyKey", groupKey)},
			expectedTerm: false,
		},
		{
			name:         "an extra scope expression is not the All placement",
			terms:        []corev1.PodAffinityTerm{exclusiveAntiAffinityTerm("topologyKey", leaderworkerset.GroupUniqueHashLabelKey, "test-key", leaderworkerset.PlacementScopeOwn, "test-lws")},
			expectedTerm: false,
		},
		{
			name:         "a different topology key is not the placement",
			terms:        []corev1.PodAffinityTerm{term("topologyKey1", groupKey, notIn("test-key"))},
			expectedTerm: false,
		},
		{
			name:         "a term without a selector is not the placement",
			terms:        []corev1.PodAffinityTerm{{TopologyKey: "topologyKey"}},
			expectedTerm: false,
		},
		{
			name:         "a multi-value NotIn is not the placement",
			terms:        []corev1.PodAffinityTerm{term("topologyKey", groupKey, notIn("test-key", "other-key"))},
			expectedTerm: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := hasExclusiveTerm(tc.terms, expected, leaderworkerset.GroupUniqueHashLabelKey); got != tc.expectedTerm {
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

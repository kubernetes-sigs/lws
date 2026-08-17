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
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"

	disaggv1 "sigs.k8s.io/lws/api/disaggregatedset/v1"
	leaderworkerset "sigs.k8s.io/lws/api/leaderworkerset/v1"
)

func TestValidateCreate(t *testing.T) {
	webhook := &DisaggregatedSetWebhook{}
	ctx := context.Background()

	tests := []struct {
		name        string
		obj         *disaggv1.DisaggregatedSet
		expectError bool
		errorMsg    string
	}{
		{
			name: "valid DisaggregatedSet with no rolloutStrategy",
			obj: &disaggv1.DisaggregatedSet{
				ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
				Spec: disaggv1.DisaggregatedSetSpec{
					Roles: []disaggv1.DisaggregatedRoleSpec{
						{
							Name: "prefill",
							LeaderWorkerSetTemplateSpec: leaderworkerset.LeaderWorkerSetTemplateSpec{
								Spec: leaderworkerset.LeaderWorkerSetSpec{
									Replicas: ptr.To(int32(2)),
								},
							},
						},
						{
							Name: "decode",
							LeaderWorkerSetTemplateSpec: leaderworkerset.LeaderWorkerSetTemplateSpec{
								Spec: leaderworkerset.LeaderWorkerSetSpec{
									Replicas: ptr.To(int32(2)),
								},
							},
						},
					},
				},
			},
			expectError: false,
		},
		{
			name: "valid DisaggregatedSet with RollingUpdate type",
			obj: &disaggv1.DisaggregatedSet{
				ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
				Spec: disaggv1.DisaggregatedSetSpec{
					Roles: []disaggv1.DisaggregatedRoleSpec{
						{
							Name: "prefill",
							LeaderWorkerSetTemplateSpec: leaderworkerset.LeaderWorkerSetTemplateSpec{
								Spec: leaderworkerset.LeaderWorkerSetSpec{
									Replicas: ptr.To(int32(2)),
									RolloutStrategy: leaderworkerset.RolloutStrategy{
										Type: leaderworkerset.RollingUpdateStrategyType,
										RollingUpdateConfiguration: &leaderworkerset.RollingUpdateConfiguration{
											MaxSurge:       intstr.FromInt32(1),
											MaxUnavailable: intstr.FromInt32(0),
										},
									},
								},
							},
						},
						{
							Name: "decode",
							LeaderWorkerSetTemplateSpec: leaderworkerset.LeaderWorkerSetTemplateSpec{
								Spec: leaderworkerset.LeaderWorkerSetSpec{
									Replicas: ptr.To(int32(2)),
								},
							},
						},
					},
				},
			},
			expectError: false,
		},
		{
			name: "valid DisaggregatedSet with partition set to 0",
			obj: &disaggv1.DisaggregatedSet{
				ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
				Spec: disaggv1.DisaggregatedSetSpec{
					Roles: []disaggv1.DisaggregatedRoleSpec{
						{
							Name: "prefill",
							LeaderWorkerSetTemplateSpec: leaderworkerset.LeaderWorkerSetTemplateSpec{
								Spec: leaderworkerset.LeaderWorkerSetSpec{
									Replicas: ptr.To(int32(2)),
									RolloutStrategy: leaderworkerset.RolloutStrategy{
										RollingUpdateConfiguration: &leaderworkerset.RollingUpdateConfiguration{
											Partition:      ptr.To(int32(0)),
											MaxUnavailable: intstr.FromInt32(1),
										},
									},
								},
							},
						},
						{
							Name: "decode",
							LeaderWorkerSetTemplateSpec: leaderworkerset.LeaderWorkerSetTemplateSpec{
								Spec: leaderworkerset.LeaderWorkerSetSpec{
									Replicas: ptr.To(int32(2)),
								},
							},
						},
					},
				},
			},
			expectError: false,
		},
		{
			name: "invalid DisaggregatedSet with partition set to non-zero",
			obj: &disaggv1.DisaggregatedSet{
				ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
				Spec: disaggv1.DisaggregatedSetSpec{
					Roles: []disaggv1.DisaggregatedRoleSpec{
						{
							Name: "prefill",
							LeaderWorkerSetTemplateSpec: leaderworkerset.LeaderWorkerSetTemplateSpec{
								Spec: leaderworkerset.LeaderWorkerSetSpec{
									Replicas: ptr.To(int32(2)),
									RolloutStrategy: leaderworkerset.RolloutStrategy{
										RollingUpdateConfiguration: &leaderworkerset.RollingUpdateConfiguration{
											Partition: ptr.To(int32(1)),
										},
									},
								},
							},
						},
						{
							Name: "decode",
							LeaderWorkerSetTemplateSpec: leaderworkerset.LeaderWorkerSetTemplateSpec{
								Spec: leaderworkerset.LeaderWorkerSetSpec{
									Replicas: ptr.To(int32(2)),
								},
							},
						},
					},
				},
			},
			expectError: true,
			errorMsg:    "partition",
		},
		{
			name: "invalid DisaggregatedSet with unsupported rollout type",
			obj: &disaggv1.DisaggregatedSet{
				ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
				Spec: disaggv1.DisaggregatedSetSpec{
					Roles: []disaggv1.DisaggregatedRoleSpec{
						{
							Name: "prefill",
							LeaderWorkerSetTemplateSpec: leaderworkerset.LeaderWorkerSetTemplateSpec{
								Spec: leaderworkerset.LeaderWorkerSetSpec{
									Replicas: ptr.To(int32(2)),
									RolloutStrategy: leaderworkerset.RolloutStrategy{
										Type: "SomeOtherType",
									},
								},
							},
						},
						{
							Name: "decode",
							LeaderWorkerSetTemplateSpec: leaderworkerset.LeaderWorkerSetTemplateSpec{
								Spec: leaderworkerset.LeaderWorkerSetSpec{
									Replicas: ptr.To(int32(2)),
								},
							},
						},
					},
				},
			},
			expectError: true,
			errorMsg:    "type",
		},
		{
			name: "invalid DisaggregatedSet with multiple validation errors",
			obj: &disaggv1.DisaggregatedSet{
				ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
				Spec: disaggv1.DisaggregatedSetSpec{
					Roles: []disaggv1.DisaggregatedRoleSpec{
						{
							Name: "prefill",
							LeaderWorkerSetTemplateSpec: leaderworkerset.LeaderWorkerSetTemplateSpec{
								Spec: leaderworkerset.LeaderWorkerSetSpec{
									Replicas: ptr.To(int32(2)),
									RolloutStrategy: leaderworkerset.RolloutStrategy{
										Type: "InvalidType",
										RollingUpdateConfiguration: &leaderworkerset.RollingUpdateConfiguration{
											Partition: ptr.To(int32(5)),
										},
									},
								},
							},
						},
						{
							Name: "decode",
							LeaderWorkerSetTemplateSpec: leaderworkerset.LeaderWorkerSetTemplateSpec{
								Spec: leaderworkerset.LeaderWorkerSetSpec{
									Replicas: ptr.To(int32(2)),
								},
							},
						},
					},
				},
			},
			expectError: true,
			errorMsg:    "type",
		},
		{
			name: "valid placement policy ExclusiveSlice with topology",
			obj: &disaggv1.DisaggregatedSet{
				ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
				Spec: disaggv1.DisaggregatedSetSpec{
					PlacementPolicy: &disaggv1.PlacementPolicy{
						Type:     disaggv1.PlacementExclusiveSlice,
						Topology: "cloud.google.com/gke-nodepool",
					},
					Roles: []disaggv1.DisaggregatedRoleSpec{
						{Name: "prefill", LeaderWorkerSetTemplateSpec: leaderworkerset.LeaderWorkerSetTemplateSpec{Spec: leaderworkerset.LeaderWorkerSetSpec{Replicas: ptr.To(int32(2))}}},
						{Name: "decode", LeaderWorkerSetTemplateSpec: leaderworkerset.LeaderWorkerSetTemplateSpec{Spec: leaderworkerset.LeaderWorkerSetSpec{Replicas: ptr.To(int32(2))}}},
					},
				},
			},
			expectError: false,
		},
		{
			name: "invalid placement policy missing topology",
			obj: &disaggv1.DisaggregatedSet{
				ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
				Spec: disaggv1.DisaggregatedSetSpec{
					PlacementPolicy: &disaggv1.PlacementPolicy{Type: disaggv1.PlacementExclusiveTopology},
					Roles: []disaggv1.DisaggregatedRoleSpec{
						{Name: "prefill", LeaderWorkerSetTemplateSpec: leaderworkerset.LeaderWorkerSetTemplateSpec{Spec: leaderworkerset.LeaderWorkerSetSpec{Replicas: ptr.To(int32(2))}}},
						{Name: "decode", LeaderWorkerSetTemplateSpec: leaderworkerset.LeaderWorkerSetTemplateSpec{Spec: leaderworkerset.LeaderWorkerSetSpec{Replicas: ptr.To(int32(2))}}},
					},
				},
			},
			expectError: true,
			errorMsg:    "topology",
		},
		{
			name: "invalid placement policy combined with LWS exclusive-topology annotation",
			obj: &disaggv1.DisaggregatedSet{
				ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
				Spec: disaggv1.DisaggregatedSetSpec{
					PlacementPolicy: &disaggv1.PlacementPolicy{
						Type:     disaggv1.PlacementExclusiveSlice,
						Topology: "cloud.google.com/gke-nodepool",
					},
					Roles: []disaggv1.DisaggregatedRoleSpec{
						{
							Name: "prefill",
							LeaderWorkerSetTemplateSpec: leaderworkerset.LeaderWorkerSetTemplateSpec{
								ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{leaderworkerset.ExclusiveKeyAnnotationKey: "rack"}},
								Spec:       leaderworkerset.LeaderWorkerSetSpec{Replicas: ptr.To(int32(2))},
							},
						},
						{Name: "decode", LeaderWorkerSetTemplateSpec: leaderworkerset.LeaderWorkerSetTemplateSpec{Spec: leaderworkerset.LeaderWorkerSetSpec{Replicas: ptr.To(int32(2))}}},
					},
				},
			},
			expectError: true,
			errorMsg:    "exclusive-topology",
		},
		{
			name: "invalid placement policy with exclusive-topology on the worker template",
			obj: &disaggv1.DisaggregatedSet{
				ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
				Spec: disaggv1.DisaggregatedSetSpec{
					PlacementPolicy: &disaggv1.PlacementPolicy{
						Type:     disaggv1.PlacementExclusiveSlice,
						Topology: "cloud.google.com/gke-nodepool",
					},
					Roles: []disaggv1.DisaggregatedRoleSpec{
						{
							Name: "prefill",
							LeaderWorkerSetTemplateSpec: leaderworkerset.LeaderWorkerSetTemplateSpec{
								Spec: leaderworkerset.LeaderWorkerSetSpec{
									Replicas: ptr.To(int32(2)),
									LeaderWorkerTemplate: leaderworkerset.LeaderWorkerTemplate{
										WorkerTemplate: corev1.PodTemplateSpec{
											ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{leaderworkerset.ExclusiveKeyAnnotationKey: "rack"}},
										},
									},
								},
							},
						},
						{Name: "decode", LeaderWorkerSetTemplateSpec: leaderworkerset.LeaderWorkerSetTemplateSpec{Spec: leaderworkerset.LeaderWorkerSetSpec{Replicas: ptr.To(int32(2))}}},
					},
				},
			},
			expectError: true,
			errorMsg:    "exclusive-topology",
		},
		{
			name: "invalid placement policy combined with LWS subgroup-exclusive-topology annotation",
			obj: &disaggv1.DisaggregatedSet{
				ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
				Spec: disaggv1.DisaggregatedSetSpec{
					PlacementPolicy: &disaggv1.PlacementPolicy{
						Type:     disaggv1.PlacementExclusiveSlice,
						Topology: "cloud.google.com/gke-nodepool",
					},
					Roles: []disaggv1.DisaggregatedRoleSpec{
						{
							Name: "prefill",
							LeaderWorkerSetTemplateSpec: leaderworkerset.LeaderWorkerSetTemplateSpec{
								ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{leaderworkerset.SubGroupExclusiveKeyAnnotationKey: "rack"}},
								Spec:       leaderworkerset.LeaderWorkerSetSpec{Replicas: ptr.To(int32(2))},
							},
						},
						{Name: "decode", LeaderWorkerSetTemplateSpec: leaderworkerset.LeaderWorkerSetTemplateSpec{Spec: leaderworkerset.LeaderWorkerSetSpec{Replicas: ptr.To(int32(2))}}},
					},
				},
			},
			expectError: true,
			errorMsg:    "subgroup-exclusive-topology",
		},
		{
			name: "invalid placement policy with subgroup-exclusive-topology on the leader template",
			obj: &disaggv1.DisaggregatedSet{
				ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
				Spec: disaggv1.DisaggregatedSetSpec{
					PlacementPolicy: &disaggv1.PlacementPolicy{
						Type:     disaggv1.PlacementExclusiveSlice,
						Topology: "cloud.google.com/gke-nodepool",
					},
					Roles: []disaggv1.DisaggregatedRoleSpec{
						{
							Name: "prefill",
							LeaderWorkerSetTemplateSpec: leaderworkerset.LeaderWorkerSetTemplateSpec{
								Spec: leaderworkerset.LeaderWorkerSetSpec{
									Replicas: ptr.To(int32(2)),
									LeaderWorkerTemplate: leaderworkerset.LeaderWorkerTemplate{
										LeaderTemplate: &corev1.PodTemplateSpec{
											ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{leaderworkerset.SubGroupExclusiveKeyAnnotationKey: "rack"}},
										},
									},
								},
							},
						},
						{Name: "decode", LeaderWorkerSetTemplateSpec: leaderworkerset.LeaderWorkerSetTemplateSpec{Spec: leaderworkerset.LeaderWorkerSetSpec{Replicas: ptr.To(int32(2))}}},
					},
				},
			},
			expectError: true,
			errorMsg:    "subgroup-exclusive-topology",
		},
		{
			name: "invalid placement policy with subgroup-exclusive-topology on the worker template",
			obj: &disaggv1.DisaggregatedSet{
				ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
				Spec: disaggv1.DisaggregatedSetSpec{
					PlacementPolicy: &disaggv1.PlacementPolicy{
						Type:     disaggv1.PlacementExclusiveSlice,
						Topology: "cloud.google.com/gke-nodepool",
					},
					Roles: []disaggv1.DisaggregatedRoleSpec{
						{
							Name: "prefill",
							LeaderWorkerSetTemplateSpec: leaderworkerset.LeaderWorkerSetTemplateSpec{
								Spec: leaderworkerset.LeaderWorkerSetSpec{
									Replicas: ptr.To(int32(2)),
									LeaderWorkerTemplate: leaderworkerset.LeaderWorkerTemplate{
										WorkerTemplate: corev1.PodTemplateSpec{
											ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{leaderworkerset.SubGroupExclusiveKeyAnnotationKey: "rack"}},
										},
									},
								},
							},
						},
						{Name: "decode", LeaderWorkerSetTemplateSpec: leaderworkerset.LeaderWorkerSetTemplateSpec{Spec: leaderworkerset.LeaderWorkerSetSpec{Replicas: ptr.To(int32(2))}}},
					},
				},
			},
			expectError: true,
			errorMsg:    "subgroup-exclusive-topology",
		},
		{
			name: "invalid: maxSurge=0 and maxUnavailable=0 with replicas > 0 (int literal)",
			obj: &disaggv1.DisaggregatedSet{
				ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
				Spec: disaggv1.DisaggregatedSetSpec{
					Roles: []disaggv1.DisaggregatedRoleSpec{
						{
							Name: "prefill",
							LeaderWorkerSetTemplateSpec: leaderworkerset.LeaderWorkerSetTemplateSpec{
								Spec: leaderworkerset.LeaderWorkerSetSpec{
									Replicas: ptr.To(int32(2)),
									RolloutStrategy: leaderworkerset.RolloutStrategy{
										RollingUpdateConfiguration: &leaderworkerset.RollingUpdateConfiguration{
											MaxSurge:       intstr.FromInt32(0),
											MaxUnavailable: intstr.FromInt32(0),
										},
									},
								},
							},
						},
						{
							Name: "decode",
							LeaderWorkerSetTemplateSpec: leaderworkerset.LeaderWorkerSetTemplateSpec{
								Spec: leaderworkerset.LeaderWorkerSetSpec{
									Replicas: ptr.To(int32(2)),
								},
							},
						},
					},
				},
			},
			expectError: true,
			errorMsg:    "must not be 0 when `maxSurge` is 0",
		},
		{
			name: "invalid: maxSurge=0% and maxUnavailable=0% with replicas > 0 (percentage)",
			obj: &disaggv1.DisaggregatedSet{
				ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
				Spec: disaggv1.DisaggregatedSetSpec{
					Roles: []disaggv1.DisaggregatedRoleSpec{
						{
							Name: "prefill",
							LeaderWorkerSetTemplateSpec: leaderworkerset.LeaderWorkerSetTemplateSpec{
								Spec: leaderworkerset.LeaderWorkerSetSpec{
									Replicas: ptr.To(int32(2)),
									RolloutStrategy: leaderworkerset.RolloutStrategy{
										RollingUpdateConfiguration: &leaderworkerset.RollingUpdateConfiguration{
											MaxSurge:       intstr.FromString("0%"),
											MaxUnavailable: intstr.FromString("0%"),
										},
									},
								},
							},
						},
						{
							Name: "decode",
							LeaderWorkerSetTemplateSpec: leaderworkerset.LeaderWorkerSetTemplateSpec{
								Spec: leaderworkerset.LeaderWorkerSetSpec{
									Replicas: ptr.To(int32(2)),
								},
							},
						},
					},
				},
			},
			expectError: true,
			errorMsg:    "must not be 0 when `maxSurge` is 0",
		},
		{
			name: "valid: maxSurge=0 and maxUnavailable=1 (only one is zero)",
			obj: &disaggv1.DisaggregatedSet{
				ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
				Spec: disaggv1.DisaggregatedSetSpec{
					Roles: []disaggv1.DisaggregatedRoleSpec{
						{
							Name: "prefill",
							LeaderWorkerSetTemplateSpec: leaderworkerset.LeaderWorkerSetTemplateSpec{
								Spec: leaderworkerset.LeaderWorkerSetSpec{
									Replicas: ptr.To(int32(2)),
									RolloutStrategy: leaderworkerset.RolloutStrategy{
										RollingUpdateConfiguration: &leaderworkerset.RollingUpdateConfiguration{
											MaxSurge:       intstr.FromInt32(0),
											MaxUnavailable: intstr.FromInt32(1),
										},
									},
								},
							},
						},
						{
							Name: "decode",
							LeaderWorkerSetTemplateSpec: leaderworkerset.LeaderWorkerSetTemplateSpec{
								Spec: leaderworkerset.LeaderWorkerSetSpec{
									Replicas: ptr.To(int32(2)),
								},
							},
						},
					},
				},
			},
			expectError: false,
		},
		{
			name: "valid: maxSurge=0 and maxUnavailable=0 but replicas=0 (zero-replica exemption)",
			obj: &disaggv1.DisaggregatedSet{
				ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
				Spec: disaggv1.DisaggregatedSetSpec{
					Roles: []disaggv1.DisaggregatedRoleSpec{
						{
							Name: "prefill",
							LeaderWorkerSetTemplateSpec: leaderworkerset.LeaderWorkerSetTemplateSpec{
								Spec: leaderworkerset.LeaderWorkerSetSpec{
									Replicas: ptr.To(int32(0)),
									RolloutStrategy: leaderworkerset.RolloutStrategy{
										RollingUpdateConfiguration: &leaderworkerset.RollingUpdateConfiguration{
											MaxSurge:       intstr.FromInt32(0),
											MaxUnavailable: intstr.FromInt32(0),
										},
									},
								},
							},
						},
						{
							Name: "decode",
							LeaderWorkerSetTemplateSpec: leaderworkerset.LeaderWorkerSetTemplateSpec{
								Spec: leaderworkerset.LeaderWorkerSetSpec{
									Replicas: ptr.To(int32(0)),
								},
							},
						},
					},
				},
			},
			expectError: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := webhook.ValidateCreate(ctx, tt.obj)
			if tt.expectError {
				require.Error(t, err)
				require.Contains(t, err.Error(), tt.errorMsg)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestValidateUpdate(t *testing.T) {
	webhook := &DisaggregatedSetWebhook{}
	ctx := context.Background()

	validObj := &disaggv1.DisaggregatedSet{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
		Spec: disaggv1.DisaggregatedSetSpec{
			Roles: []disaggv1.DisaggregatedRoleSpec{
				{
					Name: "prefill",
					LeaderWorkerSetTemplateSpec: leaderworkerset.LeaderWorkerSetTemplateSpec{
						Spec: leaderworkerset.LeaderWorkerSetSpec{
							Replicas: ptr.To(int32(2)),
						},
					},
				},
				{
					Name: "decode",
					LeaderWorkerSetTemplateSpec: leaderworkerset.LeaderWorkerSetTemplateSpec{
						Spec: leaderworkerset.LeaderWorkerSetSpec{
							Replicas: ptr.To(int32(2)),
						},
					},
				},
			},
		},
	}

	invalidObj := &disaggv1.DisaggregatedSet{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
		Spec: disaggv1.DisaggregatedSetSpec{
			Roles: []disaggv1.DisaggregatedRoleSpec{
				{
					Name: "prefill",
					LeaderWorkerSetTemplateSpec: leaderworkerset.LeaderWorkerSetTemplateSpec{
						Spec: leaderworkerset.LeaderWorkerSetSpec{
							Replicas: ptr.To(int32(2)),
							RolloutStrategy: leaderworkerset.RolloutStrategy{
								RollingUpdateConfiguration: &leaderworkerset.RollingUpdateConfiguration{
									Partition: ptr.To(int32(1)),
								},
							},
						},
					},
				},
				{
					Name: "decode",
					LeaderWorkerSetTemplateSpec: leaderworkerset.LeaderWorkerSetTemplateSpec{
						Spec: leaderworkerset.LeaderWorkerSetSpec{
							Replicas: ptr.To(int32(2)),
						},
					},
				},
			},
		},
	}

	t.Run("valid update", func(t *testing.T) {
		_, err := webhook.ValidateUpdate(ctx, validObj, validObj)
		require.NoError(t, err)
	})

	t.Run("invalid update with partition", func(t *testing.T) {
		_, err := webhook.ValidateUpdate(ctx, validObj, invalidObj)
		require.Error(t, err)
		require.Contains(t, err.Error(), "partition")
	})

	t.Run("invalid update: maxSurge=0 and maxUnavailable=0 with replicas > 0", func(t *testing.T) {
		bothZero := &disaggv1.DisaggregatedSet{
			ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
			Spec: disaggv1.DisaggregatedSetSpec{
				Roles: []disaggv1.DisaggregatedRoleSpec{
					{
						Name: "prefill",
						LeaderWorkerSetTemplateSpec: leaderworkerset.LeaderWorkerSetTemplateSpec{
							Spec: leaderworkerset.LeaderWorkerSetSpec{
								Replicas: ptr.To(int32(2)),
								RolloutStrategy: leaderworkerset.RolloutStrategy{
									RollingUpdateConfiguration: &leaderworkerset.RollingUpdateConfiguration{
										MaxSurge:       intstr.FromInt32(0),
										MaxUnavailable: intstr.FromInt32(0),
									},
								},
							},
						},
					},
					{
						Name: "decode",
						LeaderWorkerSetTemplateSpec: leaderworkerset.LeaderWorkerSetTemplateSpec{
							Spec: leaderworkerset.LeaderWorkerSetSpec{
								Replicas: ptr.To(int32(2)),
							},
						},
					},
				},
			},
		}
		_, err := webhook.ValidateUpdate(ctx, validObj, bothZero)
		require.Error(t, err)
		require.Contains(t, err.Error(), "must not be 0 when `maxSurge` is 0")
	})
}

func TestValidateDelete(t *testing.T) {
	webhook := &DisaggregatedSetWebhook{}
	ctx := context.Background()

	obj := &disaggv1.DisaggregatedSet{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
	}

	_, err := webhook.ValidateDelete(ctx, obj)
	require.NoError(t, err)
}

func TestValidateExternalScalingRules(t *testing.T) {
	webhook := &DisaggregatedSetWebhook{}
	ctx := context.Background()
	external := &disaggv1.RoleScaling{Mode: disaggv1.RoleScalingExternal}

	t.Run("warns when External + replicas > 1", func(t *testing.T) {
		obj := &disaggv1.DisaggregatedSet{
			ObjectMeta: metav1.ObjectMeta{Name: "ds", Namespace: "default"},
			Spec: disaggv1.DisaggregatedSetSpec{Roles: []disaggv1.DisaggregatedRoleSpec{
				{Name: "prefill", Scaling: external, LeaderWorkerSetTemplateSpec: leaderworkerset.LeaderWorkerSetTemplateSpec{Spec: leaderworkerset.LeaderWorkerSetSpec{Replicas: ptr.To(int32(3))}}},
			}},
		}
		warnings, err := webhook.ValidateCreate(ctx, obj)
		require.NoError(t, err)
		require.Len(t, warnings, 1)
		require.Contains(t, warnings[0], "spec.replicas is ignored")
	})

	t.Run("rejects External + spec.slices > 1", func(t *testing.T) {
		obj := &disaggv1.DisaggregatedSet{
			ObjectMeta: metav1.ObjectMeta{Name: "ds", Namespace: "default"},
			Spec: disaggv1.DisaggregatedSetSpec{
				Slices: ptr.To(int32(2)),
				Roles:  []disaggv1.DisaggregatedRoleSpec{{Name: "prefill", Scaling: external}},
			},
		}
		_, err := webhook.ValidateCreate(ctx, obj)
		require.Error(t, err)
		require.Contains(t, err.Error(), "spec.slices > 1")
	})

	t.Run("rejects when scaler name would exceed 253 chars", func(t *testing.T) {
		longDS := ""
		for i := 0; i < 250; i++ {
			longDS += "a"
		}
		obj := &disaggv1.DisaggregatedSet{
			ObjectMeta: metav1.ObjectMeta{Name: longDS, Namespace: "default"},
			Spec: disaggv1.DisaggregatedSetSpec{Roles: []disaggv1.DisaggregatedRoleSpec{
				{Name: "prefill", Scaling: external},
			}},
		}
		_, err := webhook.ValidateCreate(ctx, obj)
		require.Error(t, err)
		require.Contains(t, err.Error(), "253 characters")
	})
}

func TestValidateCreateGroupIdentity(t *testing.T) {
	webhook := &DisaggregatedSetWebhook{}
	ctx := context.Background()

	buildDS := func(spec leaderworkerset.LeaderWorkerSetSpec) *disaggv1.DisaggregatedSet {
		return &disaggv1.DisaggregatedSet{
			ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
			Spec: disaggv1.DisaggregatedSetSpec{
				Roles: []disaggv1.DisaggregatedRoleSpec{
					{
						Name:                        "prefill",
						LeaderWorkerSetTemplateSpec: leaderworkerset.LeaderWorkerSetTemplateSpec{Spec: spec},
					},
				},
			},
		}
	}

	tests := []struct {
		name        string
		obj         *disaggv1.DisaggregatedSet
		expectError bool
		errorMsg    string
	}{
		{
			name: "hash role is accepted",
			obj: buildDS(leaderworkerset.LeaderWorkerSetSpec{
				Replicas:      ptr.To(int32(2)),
				GroupIdentity: leaderworkerset.GroupIdentityHash,
			}),
			expectError: false,
		},
		{
			name: "hash role with subGroupPolicy is rejected",
			obj: buildDS(leaderworkerset.LeaderWorkerSetSpec{
				Replicas:      ptr.To(int32(2)),
				GroupIdentity: leaderworkerset.GroupIdentityHash,
				LeaderWorkerTemplate: leaderworkerset.LeaderWorkerTemplate{
					SubGroupPolicy: &leaderworkerset.SubGroupPolicy{
						SubGroupSize: ptr.To(int32(2)),
					},
				},
			}),
			expectError: true,
			errorMsg:    "subGroupPolicy is not supported with groupIdentity Hash",
		},
		{
			name: "ordinal role with subGroupPolicy is accepted",
			obj: buildDS(leaderworkerset.LeaderWorkerSetSpec{
				Replicas:      ptr.To(int32(2)),
				GroupIdentity: leaderworkerset.GroupIdentityOrdinal,
				LeaderWorkerTemplate: leaderworkerset.LeaderWorkerTemplate{
					SubGroupPolicy: &leaderworkerset.SubGroupPolicy{
						SubGroupSize: ptr.To(int32(2)),
					},
				},
			}),
			expectError: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := webhook.ValidateCreate(ctx, tc.obj)
			if tc.expectError {
				require.Error(t, err)
				require.Contains(t, err.Error(), tc.errorMsg)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

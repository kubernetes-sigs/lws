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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"

	disaggv1alpha1 "sigs.k8s.io/lws/api/disaggregatedset/v1alpha1"
	leaderworkerset "sigs.k8s.io/lws/api/leaderworkerset/v1"
)

func TestValidateCreate(t *testing.T) {
	webhook := &DisaggregatedSetWebhook{}
	ctx := context.Background()

	tests := []struct {
		name        string
		obj         *disaggv1alpha1.DisaggregatedSet
		expectError bool
		errorMsg    string
	}{
		{
			name: "valid DisaggregatedSet with no rolloutStrategy",
			obj: &disaggv1alpha1.DisaggregatedSet{
				ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
				Spec: disaggv1alpha1.DisaggregatedSetSpec{
					Roles: []disaggv1alpha1.DisaggregatedRoleSpec{
						{
							Name: "prefill",
							LeaderWorkerSetSpec: leaderworkerset.LeaderWorkerSetSpec{
								Replicas: ptr.To(int32(2)),
							},
						},
						{
							Name: "decode",
							LeaderWorkerSetSpec: leaderworkerset.LeaderWorkerSetSpec{
								Replicas: ptr.To(int32(2)),
							},
						},
					},
				},
			},
			expectError: false,
		},
		{
			name: "valid DisaggregatedSet with RollingUpdate type",
			obj: &disaggv1alpha1.DisaggregatedSet{
				ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
				Spec: disaggv1alpha1.DisaggregatedSetSpec{
					Roles: []disaggv1alpha1.DisaggregatedRoleSpec{
						{
							Name: "prefill",
							LeaderWorkerSetSpec: leaderworkerset.LeaderWorkerSetSpec{
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
						{
							Name: "decode",
							LeaderWorkerSetSpec: leaderworkerset.LeaderWorkerSetSpec{
								Replicas: ptr.To(int32(2)),
							},
						},
					},
				},
			},
			expectError: false,
		},
		{
			name: "valid DisaggregatedSet with partition set to 0",
			obj: &disaggv1alpha1.DisaggregatedSet{
				ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
				Spec: disaggv1alpha1.DisaggregatedSetSpec{
					Roles: []disaggv1alpha1.DisaggregatedRoleSpec{
						{
							Name: "prefill",
							LeaderWorkerSetSpec: leaderworkerset.LeaderWorkerSetSpec{
								Replicas: ptr.To(int32(2)),
								RolloutStrategy: leaderworkerset.RolloutStrategy{
									RollingUpdateConfiguration: &leaderworkerset.RollingUpdateConfiguration{
										Partition: ptr.To(int32(0)),
									},
								},
							},
						},
						{
							Name: "decode",
							LeaderWorkerSetSpec: leaderworkerset.LeaderWorkerSetSpec{
								Replicas: ptr.To(int32(2)),
							},
						},
					},
				},
			},
			expectError: false,
		},
		{
			name: "invalid DisaggregatedSet with partition set to non-zero",
			obj: &disaggv1alpha1.DisaggregatedSet{
				ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
				Spec: disaggv1alpha1.DisaggregatedSetSpec{
					Roles: []disaggv1alpha1.DisaggregatedRoleSpec{
						{
							Name: "prefill",
							LeaderWorkerSetSpec: leaderworkerset.LeaderWorkerSetSpec{
								Replicas: ptr.To(int32(2)),
								RolloutStrategy: leaderworkerset.RolloutStrategy{
									RollingUpdateConfiguration: &leaderworkerset.RollingUpdateConfiguration{
										Partition: ptr.To(int32(1)),
									},
								},
							},
						},
						{
							Name: "decode",
							LeaderWorkerSetSpec: leaderworkerset.LeaderWorkerSetSpec{
								Replicas: ptr.To(int32(2)),
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
			obj: &disaggv1alpha1.DisaggregatedSet{
				ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
				Spec: disaggv1alpha1.DisaggregatedSetSpec{
					Roles: []disaggv1alpha1.DisaggregatedRoleSpec{
						{
							Name: "prefill",
							LeaderWorkerSetSpec: leaderworkerset.LeaderWorkerSetSpec{
								Replicas: ptr.To(int32(2)),
								RolloutStrategy: leaderworkerset.RolloutStrategy{
									Type: "SomeOtherType",
								},
							},
						},
						{
							Name: "decode",
							LeaderWorkerSetSpec: leaderworkerset.LeaderWorkerSetSpec{
								Replicas: ptr.To(int32(2)),
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
			obj: &disaggv1alpha1.DisaggregatedSet{
				ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
				Spec: disaggv1alpha1.DisaggregatedSetSpec{
					Roles: []disaggv1alpha1.DisaggregatedRoleSpec{
						{
							Name: "prefill",
							LeaderWorkerSetSpec: leaderworkerset.LeaderWorkerSetSpec{
								Replicas: ptr.To(int32(2)),
								RolloutStrategy: leaderworkerset.RolloutStrategy{
									Type: "InvalidType",
									RollingUpdateConfiguration: &leaderworkerset.RollingUpdateConfiguration{
										Partition: ptr.To(int32(5)),
									},
								},
							},
						},
						{
							Name: "decode",
							LeaderWorkerSetSpec: leaderworkerset.LeaderWorkerSetSpec{
								Replicas: ptr.To(int32(2)),
							},
						},
					},
				},
			},
			expectError: true,
			errorMsg:    "type",
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

	validObj := &disaggv1alpha1.DisaggregatedSet{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
		Spec: disaggv1alpha1.DisaggregatedSetSpec{
			Roles: []disaggv1alpha1.DisaggregatedRoleSpec{
				{
					Name: "prefill",
					LeaderWorkerSetSpec: leaderworkerset.LeaderWorkerSetSpec{
						Replicas: ptr.To(int32(2)),
					},
				},
				{
					Name: "decode",
					LeaderWorkerSetSpec: leaderworkerset.LeaderWorkerSetSpec{
						Replicas: ptr.To(int32(2)),
					},
				},
			},
		},
	}

	invalidObj := &disaggv1alpha1.DisaggregatedSet{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
		Spec: disaggv1alpha1.DisaggregatedSetSpec{
			Roles: []disaggv1alpha1.DisaggregatedRoleSpec{
				{
					Name: "prefill",
					LeaderWorkerSetSpec: leaderworkerset.LeaderWorkerSetSpec{
						Replicas: ptr.To(int32(2)),
						RolloutStrategy: leaderworkerset.RolloutStrategy{
							RollingUpdateConfiguration: &leaderworkerset.RollingUpdateConfiguration{
								Partition: ptr.To(int32(1)),
							},
						},
					},
				},
				{
					Name: "decode",
					LeaderWorkerSetSpec: leaderworkerset.LeaderWorkerSetSpec{
						Replicas: ptr.To(int32(2)),
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
}

func TestValidateDelete(t *testing.T) {
	webhook := &DisaggregatedSetWebhook{}
	ctx := context.Background()

	obj := &disaggv1alpha1.DisaggregatedSet{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
	}

	_, err := webhook.ValidateDelete(ctx, obj)
	require.NoError(t, err)
}

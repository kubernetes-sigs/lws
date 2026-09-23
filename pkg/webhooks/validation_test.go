/*
Copyright 2026 The Kubernetes Authors.

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
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	schedulingv1alpha3 "k8s.io/api/scheduling/v1alpha3"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/utils/ptr"

	leaderworkerset "sigs.k8s.io/lws/api/leaderworkerset/v1"
	"sigs.k8s.io/lws/pkg/features"
	"sigs.k8s.io/lws/pkg/schedulerprovider"
)

func TestValidateVolcanoScheduling(t *testing.T) {
	features.SetFeatureGateDuringTest(t, features.WorkloadAwareScheduling, true)
	hook := &LeaderWorkerSetWebhook{SchedulerProvider: schedulerprovider.Volcano}

	tests := map[string]struct {
		mutate  func(*leaderworkerset.LeaderWorkerSet)
		wantErr string
	}{
		"replica level gang is supported": {},
		"lws level is rejected": {
			mutate: func(lws *leaderworkerset.LeaderWorkerSet) {
				lws.Spec.Scheduling.SchedulingConstraints = &schedulingv1alpha3.WorkloadCompositePodGroupSchedulingConstraints{}
			},
			wantErr: "the Volcano provider supports the typed API only at replica level",
		},
		"basic policy is rejected": {
			mutate: func(lws *leaderworkerset.LeaderWorkerSet) {
				lws.Spec.Scheduling.Replica = &leaderworkerset.LeaderWorkerSetReplicaScheduling{
					SchedulingPolicy: &schedulingv1alpha3.WorkloadCompositePodGroupSchedulingPolicy{
						Basic: &schedulingv1alpha3.WorkloadCompositePodGroupBasicSchedulingPolicy{},
					},
				}
			},
			wantErr: "does not support Basic policy",
		},
		"scheduling constraints are rejected": {
			mutate: func(lws *leaderworkerset.LeaderWorkerSet) {
				lws.Spec.Scheduling.Replica = &leaderworkerset.LeaderWorkerSetReplicaScheduling{
					SchedulingConstraints: &schedulingv1alpha3.WorkloadCompositePodGroupSchedulingConstraints{},
				}
			},
			wantErr: "does not support typed scheduling constraints",
		},
		"disruption mode is rejected": {
			mutate: func(lws *leaderworkerset.LeaderWorkerSet) {
				lws.Spec.Scheduling.Replica = &leaderworkerset.LeaderWorkerSetReplicaScheduling{
					DisruptionMode: &schedulingv1alpha3.WorkloadCompositePodGroupDisruptionMode{},
				}
			},
			wantErr: "does not support typed disruption mode",
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			lws := validScheduledLWS()
			if tc.mutate != nil {
				tc.mutate(lws)
			}
			errs := hook.validateScheduling(context.Background(), nil, lws)
			if tc.wantErr == "" {
				assert.Empty(t, errs, "errors: %v", errs)
				return
			}
			require.NotEmpty(t, errs)
			assert.Contains(t, errs.ToAggregate().Error(), tc.wantErr)
		})
	}

	// Leader and worker fields select role mode, which is rejected before the
	// per-field checks run. The resource claim checks are therefore only
	// reached when the active level cannot be resolved at all, which happens
	// when a replica-level field is set alongside a role-level one.
	t.Run("shared resource claims are rejected", func(t *testing.T) {
		claims := []schedulingv1alpha3.WorkloadPodGroupResourceClaim{{
			Name: "gpu", ResourceClaimName: ptr.To("shared-gpu"),
		}}
		cases := map[string]*leaderworkerset.LeaderWorkerSetReplicaScheduling{
			"leader": {
				DisruptionMode: &schedulingv1alpha3.WorkloadCompositePodGroupDisruptionMode{},
				Leader:         &leaderworkerset.LeaderWorkerSetLeaderScheduling{ResourceClaims: claims},
			},
			"worker": {
				DisruptionMode: &schedulingv1alpha3.WorkloadCompositePodGroupDisruptionMode{},
				Worker:         &leaderworkerset.LeaderWorkerSetWorkerScheduling{ResourceClaims: claims},
			},
		}

		for name, replica := range cases {
			t.Run(name, func(t *testing.T) {
				lws := validScheduledLWS()
				lws.Spec.Scheduling.Replica = replica
				errs := validateVolcanoScheduling(lws, field.NewPath("spec", "scheduling"))
				require.NotEmpty(t, errs)
				assert.Contains(t, errs.ToAggregate().Error(), "does not support shared resource claims")
			})
		}
	})
}

func TestValidateSchedulingUnknownProvider(t *testing.T) {
	features.SetFeatureGateDuringTest(t, features.WorkloadAwareScheduling, true)

	t.Run("missing provider", func(t *testing.T) {
		hook := &LeaderWorkerSetWebhook{}
		errs := hook.validateScheduling(context.Background(), nil, validScheduledLWS())
		require.NotEmpty(t, errs)
		assert.Contains(t, errs.ToAggregate().Error(), "requires a configured scheduler provider")
	})

	t.Run("unsupported provider", func(t *testing.T) {
		hook := &LeaderWorkerSetWebhook{SchedulerProvider: schedulerprovider.ProviderType("yarn")}
		errs := hook.validateScheduling(context.Background(), nil, validScheduledLWS())
		require.NotEmpty(t, errs)
		assert.Contains(t, errs.ToAggregate().Error(), "Unsupported value")
	})
}

func TestLeaderWorkerSetWebhookValidateDelete(t *testing.T) {
	hook := &LeaderWorkerSetWebhook{}

	// Deletion is always admitted; the webhook only exists so the type is registered.
	warnings, err := hook.ValidateDelete(context.Background(), validScheduledLWS())
	assert.NoError(t, err)
	assert.Nil(t, warnings)
}

func TestValidatePositiveIntOrPercent(t *testing.T) {
	path := field.NewPath("spec", "rolloutStrategy", "rollingUpdateConfiguration", "maxUnavailable")

	tests := map[string]struct {
		value   intstr.IntOrString
		wantErr string
	}{
		"positive int": {
			value: intstr.FromInt32(2),
		},
		"zero": {
			value: intstr.FromInt32(0),
		},
		"negative int": {
			value:   intstr.FromInt32(-1),
			wantErr: "must be greater than or equal to 0",
		},
		"valid percent": {
			value: intstr.FromString("30%"),
		},
		"missing percent sign": {
			value:   intstr.FromString("30"),
			wantErr: "a valid percent string must be",
		},
		"not a number": {
			value:   intstr.FromString("thirty%"),
			wantErr: "a valid percent string must be",
		},
		"unknown type": {
			value:   intstr.IntOrString{Type: intstr.Type(42)},
			wantErr: "must be an integer or percentage",
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			errs := ValidatePositiveIntOrPercent(tc.value, path)
			if tc.wantErr == "" {
				assert.Empty(t, errs, "errors: %v", errs)
				return
			}
			require.NotEmpty(t, errs)
			assert.Contains(t, errs.ToAggregate().Error(), tc.wantErr)
			assert.Equal(t, path.String(), errs[0].Field)
		})
	}
}

func TestPodWebhookValidate(t *testing.T) {
	hook := NewPodWebhook(nil)
	ctx := context.Background()

	lwsPod := &corev1.Pod{}
	lwsPod.Labels = map[string]string{leaderworkerset.SetNameLabelKey: "test-lws"}
	unrelatedPod := &corev1.Pod{}

	for name, pod := range map[string]*corev1.Pod{
		"pod owned by a leaderworkerset": lwsPod,
		"unrelated pod":                  unrelatedPod,
	} {
		t.Run(name, func(t *testing.T) {
			// The pod validator admits everything today; it exists so that the
			// webhook is registered for pods.
			warnings, err := hook.ValidateCreate(ctx, pod)
			assert.NoError(t, err)
			assert.Nil(t, warnings)

			warnings, err = hook.ValidateUpdate(ctx, pod, pod)
			assert.NoError(t, err)
			assert.Nil(t, warnings)

			warnings, err = hook.ValidateDelete(ctx, pod)
			assert.NoError(t, err)
			assert.Nil(t, warnings)
		})
	}
}

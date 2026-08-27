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
	corev1 "k8s.io/api/core/v1"
	schedulingv1alpha3 "k8s.io/api/scheduling/v1alpha3"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	leaderworkerset "sigs.k8s.io/lws/api/leaderworkerset/v1"
	"sigs.k8s.io/lws/pkg/features"
	"sigs.k8s.io/lws/pkg/schedulerprovider"
)

func TestValidateScheduling(t *testing.T) {
	tests := map[string]struct {
		mutate   func(*leaderworkerset.LeaderWorkerSet)
		gates    features.Gates
		wantErrs int
	}{
		"empty scheduling defaults to replica-sized gang": {
			gates: features.Gates{features.WorkloadAwareScheduling: true},
		},
		"feature gate disabled": {
			wantErrs: 1,
		},
		"phase one rejects composite minGroupCount": {
			gates: features.Gates{features.WorkloadAwareScheduling: true},
			mutate: func(lws *leaderworkerset.LeaderWorkerSet) {
				lws.Spec.Scheduling.SchedulingPolicy = &schedulingv1alpha3.WorkloadCompositePodGroupSchedulingPolicy{
					Gang: &schedulingv1alpha3.WorkloadCompositePodGroupGangSchedulingPolicy{MinGroupCount: ptr.To[int32](2)},
				}
			},
			wantErrs: 1,
		},
		"gang is incompatible with LeaderReady": {
			gates: features.Gates{features.WorkloadAwareScheduling: true},
			mutate: func(lws *leaderworkerset.LeaderWorkerSet) {
				lws.Spec.StartupPolicy = leaderworkerset.LeaderReadyStartupPolicy
			},
			wantErrs: 1,
		},
		"multiple active levels are rejected": {
			gates: features.Gates{features.WorkloadAwareScheduling: true},
			mutate: func(lws *leaderworkerset.LeaderWorkerSet) {
				lws.Spec.Scheduling.SchedulingConstraints = &schedulingv1alpha3.WorkloadCompositePodGroupSchedulingConstraints{}
				lws.Spec.Scheduling.Replica = &leaderworkerset.LeaderWorkerSetReplicaScheduling{}
			},
			wantErrs: 1,
		},
		"role leaves may use different priorities": {
			gates: features.Gates{features.WorkloadAwareScheduling: true},
			mutate: func(lws *leaderworkerset.LeaderWorkerSet) {
				lws.Spec.LeaderWorkerTemplate.LeaderTemplate = &corev1.PodTemplateSpec{Spec: corev1.PodSpec{PriorityClassName: "other-priority"}}
				lws.Spec.Scheduling.Replica = &leaderworkerset.LeaderWorkerSetReplicaScheduling{
					Leader: &leaderworkerset.LeaderWorkerSetPodGroupScheduling{},
				}
			},
		},
		"worker-only gang is compatible with LeaderReady": {
			gates: features.Gates{features.WorkloadAwareScheduling: true},
			mutate: func(lws *leaderworkerset.LeaderWorkerSet) {
				lws.Spec.StartupPolicy = leaderworkerset.LeaderReadyStartupPolicy
				lws.Spec.Scheduling.Replica = &leaderworkerset.LeaderWorkerSetReplicaScheduling{
					Worker: &leaderworkerset.LeaderWorkerSetPodGroupScheduling{
						SchedulingPolicy: &schedulingv1alpha3.WorkloadPodGroupSchedulingPolicy{
							Gang: &schedulingv1alpha3.WorkloadPodGroupGangSchedulingPolicy{},
						},
					},
				}
			},
		},
		"leaf gang minimum must equal membership": {
			gates: features.Gates{features.WorkloadAwareScheduling: true},
			mutate: func(lws *leaderworkerset.LeaderWorkerSet) {
				lws.Spec.Scheduling.Replica = &leaderworkerset.LeaderWorkerSetReplicaScheduling{
					Worker: &leaderworkerset.LeaderWorkerSetPodGroupScheduling{
						SchedulingPolicy: &schedulingv1alpha3.WorkloadPodGroupSchedulingPolicy{
							Gang: &schedulingv1alpha3.WorkloadPodGroupGangSchedulingPolicy{MinCount: ptr.To[int32](1)},
						},
					},
				}
			},
			wantErrs: 1,
		},
		"leader and worker priority must match": {
			gates: features.Gates{features.WorkloadAwareScheduling: true},
			mutate: func(lws *leaderworkerset.LeaderWorkerSet) {
				lws.Spec.LeaderWorkerTemplate.LeaderTemplate = &corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{PriorityClassName: "other-priority"},
				}
			},
			wantErrs: 1,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			lws := validScheduledLWS()
			if tc.mutate != nil {
				tc.mutate(lws)
			}
			hook := &LeaderWorkerSetWebhook{
				FeatureGates:      tc.gates,
				SchedulerProvider: schedulerprovider.Kubernetes,
			}
			errs := hook.validateScheduling(context.Background(), nil, lws)
			assert.Len(t, errs, tc.wantErrs, "errors: %v", errs)
		})
	}
}

func TestValidateSchedulingUpdate(t *testing.T) {
	hook := &LeaderWorkerSetWebhook{
		FeatureGates:      features.Gates{features.WorkloadAwareScheduling: true},
		SchedulerProvider: schedulerprovider.Kubernetes,
	}

	t.Run("active level is immutable", func(t *testing.T) {
		oldLWS := validScheduledLWS()
		newLWS := oldLWS.DeepCopy()
		newLWS.Spec.Scheduling = &leaderworkerset.LeaderWorkerSetScheduling{
			SchedulingPolicy: &schedulingv1alpha3.WorkloadCompositePodGroupSchedulingPolicy{
				Basic: &schedulingv1alpha3.WorkloadCompositePodGroupBasicSchedulingPolicy{},
			},
		}
		errs := hook.validateScheduling(context.Background(), oldLWS, newLWS)
		assert.NotEmpty(t, errs)
		assert.Contains(t, errs.ToAggregate().Error(), "cannot switch the active scheduling level")
	})

	t.Run("generated replica minCount follows size", func(t *testing.T) {
		oldLWS := validScheduledLWS()
		newLWS := oldLWS.DeepCopy()
		newLWS.Spec.LeaderWorkerTemplate.Size = ptr.To[int32](4)
		assert.Empty(t, hook.validateScheduling(context.Background(), oldLWS, newLWS))
	})
}

func validScheduledLWS() *leaderworkerset.LeaderWorkerSet {
	return &leaderworkerset.LeaderWorkerSet{
		ObjectMeta: metav1.ObjectMeta{Name: "test-lws", Namespace: "default"},
		Spec: leaderworkerset.LeaderWorkerSetSpec{
			Scheduling: &leaderworkerset.LeaderWorkerSetScheduling{},
			LeaderWorkerTemplate: leaderworkerset.LeaderWorkerTemplate{
				Size: ptr.To[int32](3),
				WorkerTemplate: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
					PriorityClassName: "high-priority",
					Containers:        []corev1.Container{{Name: "worker", Image: "worker:latest"}},
				}},
			},
		},
	}
}

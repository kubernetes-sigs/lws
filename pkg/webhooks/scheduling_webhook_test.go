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
	schedulingv1beta1 "k8s.io/api/scheduling/v1beta1"
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
		"gang minimum must equal size": {
			gates: features.Gates{features.WorkloadAwareScheduling: true},
			mutate: func(lws *leaderworkerset.LeaderWorkerSet) {
				lws.Spec.Scheduling.SchedulingPolicy = &schedulingv1beta1.PodGroupSchedulingPolicy{
					Gang: &schedulingv1beta1.GangSchedulingPolicy{MinCount: 2},
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

func validScheduledLWS() *leaderworkerset.LeaderWorkerSet {
	return &leaderworkerset.LeaderWorkerSet{
		ObjectMeta: metav1.ObjectMeta{Name: "test-lws", Namespace: "default"},
		Spec: leaderworkerset.LeaderWorkerSetSpec{
			Scheduling: &leaderworkerset.LeaderWorkerSetSchedulingConfiguration{},
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

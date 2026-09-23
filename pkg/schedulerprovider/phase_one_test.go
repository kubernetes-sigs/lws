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

package schedulerprovider

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	schedulingv1alpha3 "k8s.io/api/scheduling/v1alpha3"
	"k8s.io/utils/ptr"

	leaderworkerset "sigs.k8s.io/lws/api/leaderworkerset/v1"
)

func TestSchedulingModeFor(t *testing.T) {
	tests := map[string]struct {
		mutate  func(*leaderworkerset.LeaderWorkerSet)
		want    SchedulingMode
		wantErr string
	}{
		"nil scheduling": {
			mutate: func(lws *leaderworkerset.LeaderWorkerSet) {
				lws.Spec.Scheduling = nil
			},
			wantErr: "spec.scheduling is not configured",
		},
		"empty scheduling defaults to replica": {
			want: SchedulingModeReplica,
		},
		"explicit empty replica selects replica": {
			mutate: func(lws *leaderworkerset.LeaderWorkerSet) {
				lws.Spec.Scheduling.Replica = &leaderworkerset.LeaderWorkerSetReplicaScheduling{}
			},
			want: SchedulingModeReplica,
		},
		"lws-level fields select lws": {
			mutate: func(lws *leaderworkerset.LeaderWorkerSet) {
				lws.Spec.Scheduling.SchedulingConstraints = &schedulingv1alpha3.WorkloadCompositePodGroupSchedulingConstraints{}
			},
			want: SchedulingModeLWS,
		},
		"replica-level fields select replica": {
			mutate: func(lws *leaderworkerset.LeaderWorkerSet) {
				lws.Spec.Scheduling.Replica = &leaderworkerset.LeaderWorkerSetReplicaScheduling{
					DisruptionMode: &schedulingv1alpha3.WorkloadCompositePodGroupDisruptionMode{
						Single: &schedulingv1alpha3.WorkloadCompositePodGroupSingleDisruptionMode{},
					},
				}
			},
			want: SchedulingModeReplica,
		},
		"leader or worker selects role": {
			mutate: func(lws *leaderworkerset.LeaderWorkerSet) {
				lws.Spec.Scheduling.Replica = &leaderworkerset.LeaderWorkerSetReplicaScheduling{
					Leader: &leaderworkerset.LeaderWorkerSetLeaderScheduling{},
				}
			},
			want: SchedulingModeRole,
		},
		"replica object that only parents roles is not replica mode": {
			mutate: func(lws *leaderworkerset.LeaderWorkerSet) {
				lws.Spec.Scheduling.Replica = &leaderworkerset.LeaderWorkerSetReplicaScheduling{
					Worker: &leaderworkerset.LeaderWorkerSetWorkerScheduling{},
				}
			},
			want: SchedulingModeRole,
		},
		"lws and replica levels cannot both be active": {
			mutate: func(lws *leaderworkerset.LeaderWorkerSet) {
				lws.Spec.Scheduling.SchedulingConstraints = &schedulingv1alpha3.WorkloadCompositePodGroupSchedulingConstraints{}
				lws.Spec.Scheduling.Replica = &leaderworkerset.LeaderWorkerSetReplicaScheduling{}
			},
			wantErr: "exactly one active scheduling level is required",
		},
		"replica fields and role leaves cannot both be active": {
			mutate: func(lws *leaderworkerset.LeaderWorkerSet) {
				lws.Spec.Scheduling.Replica = &leaderworkerset.LeaderWorkerSetReplicaScheduling{
					SchedulingPolicy: &schedulingv1alpha3.WorkloadCompositePodGroupSchedulingPolicy{
						Gang: &schedulingv1alpha3.WorkloadCompositePodGroupGangSchedulingPolicy{},
					},
					Leader: &leaderworkerset.LeaderWorkerSetLeaderScheduling{},
				}
			},
			wantErr: "exactly one active scheduling level is required",
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			lws := testScheduledLWS()
			if tc.mutate != nil {
				tc.mutate(lws)
			}
			got, err := SchedulingModeFor(lws)
			if tc.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestValidatePhaseOneWorkload(t *testing.T) {
	tests := map[string]struct {
		mutate  func(*leaderworkerset.LeaderWorkerSet)
		update  func(oldLWS, newLWS *leaderworkerset.LeaderWorkerSet)
		wantErr string
	}{
		"empty scheduling is valid replica gang": {},
		"explicit empty replica is valid": {
			mutate: func(lws *leaderworkerset.LeaderWorkerSet) {
				lws.Spec.Scheduling.Replica = &leaderworkerset.LeaderWorkerSetReplicaScheduling{}
			},
		},
		"rejects lws minGroupCount": {
			mutate: func(lws *leaderworkerset.LeaderWorkerSet) {
				lws.Spec.Scheduling.SchedulingPolicy = &schedulingv1alpha3.WorkloadCompositePodGroupSchedulingPolicy{
					Gang: &schedulingv1alpha3.WorkloadCompositePodGroupGangSchedulingPolicy{MinGroupCount: ptr.To[int32](2)},
				}
			},
			wantErr: "minGroupCount is not supported",
		},
		"rejects replica minGroupCount": {
			mutate: func(lws *leaderworkerset.LeaderWorkerSet) {
				lws.Spec.Scheduling.Replica = &leaderworkerset.LeaderWorkerSetReplicaScheduling{
					SchedulingPolicy: &schedulingv1alpha3.WorkloadCompositePodGroupSchedulingPolicy{
						Gang: &schedulingv1alpha3.WorkloadCompositePodGroupGangSchedulingPolicy{MinGroupCount: ptr.To[int32](2)},
					},
				}
			},
			wantErr: "minGroupCount is not supported",
		},
		"replica gang is incompatible with LeaderReady": {
			mutate: func(lws *leaderworkerset.LeaderWorkerSet) {
				lws.Spec.StartupPolicy = leaderworkerset.LeaderReadyStartupPolicy
			},
			wantErr: "incompatible with startupPolicy LeaderReady",
		},
		"worker-only gang is compatible with LeaderReady": {
			mutate: func(lws *leaderworkerset.LeaderWorkerSet) {
				lws.Spec.StartupPolicy = leaderworkerset.LeaderReadyStartupPolicy
				lws.Spec.Scheduling.Replica = &leaderworkerset.LeaderWorkerSetReplicaScheduling{
					Worker: &leaderworkerset.LeaderWorkerSetWorkerScheduling{
						SchedulingPolicy: &schedulingv1alpha3.WorkloadPodGroupSchedulingPolicy{
							Gang: &schedulingv1alpha3.WorkloadPodGroupGangSchedulingPolicy{},
						},
					},
				}
			},
		},
		"multiple active levels are rejected": {
			mutate: func(lws *leaderworkerset.LeaderWorkerSet) {
				lws.Spec.Scheduling.SchedulingConstraints = &schedulingv1alpha3.WorkloadCompositePodGroupSchedulingConstraints{}
				lws.Spec.Scheduling.Replica = &leaderworkerset.LeaderWorkerSetReplicaScheduling{}
			},
			wantErr: "exactly one active scheduling level is required",
		},
		"leader and worker priority must match": {
			mutate: func(lws *leaderworkerset.LeaderWorkerSet) {
				lws.Spec.LeaderWorkerTemplate.LeaderTemplate = &corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{PriorityClassName: "other-priority"},
				}
			},
			wantErr: "same priorityClassName",
		},
		"role mode requires size >= 2": {
			mutate: func(lws *leaderworkerset.LeaderWorkerSet) {
				lws.Spec.LeaderWorkerTemplate.Size = ptr.To[int32](1)
				lws.Spec.Scheduling.Replica = &leaderworkerset.LeaderWorkerSetReplicaScheduling{
					Leader: &leaderworkerset.LeaderWorkerSetLeaderScheduling{},
				}
			},
			wantErr: "size >= 2",
		},
		"leaf gang minimum must equal membership": {
			mutate: func(lws *leaderworkerset.LeaderWorkerSet) {
				lws.Spec.Scheduling.Replica = &leaderworkerset.LeaderWorkerSetReplicaScheduling{
					Worker: &leaderworkerset.LeaderWorkerSetWorkerScheduling{
						SchedulingPolicy: &schedulingv1alpha3.WorkloadPodGroupSchedulingPolicy{
							Gang: &schedulingv1alpha3.WorkloadPodGroupGangSchedulingPolicy{MinCount: ptr.To[int32](1)},
						},
					},
				}
			},
			wantErr: "must equal complete leaf membership",
		},
		"gang cannot combine with exclusive topology": {
			mutate: func(lws *leaderworkerset.LeaderWorkerSet) {
				lws.Annotations = map[string]string{leaderworkerset.ExclusiveKeyAnnotationKey: "topology.kubernetes.io/zone"}
			},
			wantErr: "cannot be combined with exclusive topology",
		},
		"managed templates cannot preset schedulingGroup": {
			mutate: func(lws *leaderworkerset.LeaderWorkerSet) {
				lws.Spec.LeaderWorkerTemplate.WorkerTemplate.Spec.SchedulingGroup = &corev1.PodSchedulingGroup{
					PodGroupName: ptr.To("user-managed"),
				}
			},
			wantErr: "must not set spec.schedulingGroup",
		},
		"worker resource claims matching the template are admitted": {
			mutate: func(lws *leaderworkerset.LeaderWorkerSet) {
				lws.Spec.LeaderWorkerTemplate.WorkerTemplate.Spec.ResourceClaims = []corev1.PodResourceClaim{{
					Name: "gpu", ResourceClaimName: ptr.To("shared-gpu"),
				}}
				lws.Spec.Scheduling.Replica = &leaderworkerset.LeaderWorkerSetReplicaScheduling{
					Worker: &leaderworkerset.LeaderWorkerSetWorkerScheduling{
						ResourceClaims: []schedulingv1alpha3.WorkloadPodGroupResourceClaim{{
							Name: "gpu", ResourceClaimName: ptr.To("shared-gpu"),
						}},
					},
				}
			},
		},
		"worker resource claims must match the worker template": {
			mutate: func(lws *leaderworkerset.LeaderWorkerSet) {
				lws.Spec.Scheduling.Replica = &leaderworkerset.LeaderWorkerSetReplicaScheduling{
					Worker: &leaderworkerset.LeaderWorkerSetWorkerScheduling{
						ResourceClaims: []schedulingv1alpha3.WorkloadPodGroupResourceClaim{{
							Name: "gpu", ResourceClaimName: ptr.To("shared-gpu"),
						}},
					},
				}
			},
			wantErr: "matching reference in every member pod template",
		},
		"whole LWS gang supports zero replicas": {
			mutate: func(lws *leaderworkerset.LeaderWorkerSet) {
				lws.Spec.Replicas = ptr.To[int32](0)
				lws.Spec.Scheduling.SchedulingPolicy = &schedulingv1alpha3.WorkloadCompositePodGroupSchedulingPolicy{
					Gang: &schedulingv1alpha3.WorkloadCompositePodGroupGangSchedulingPolicy{},
				}
			},
		},
		"active level is immutable": {
			update: func(oldLWS, newLWS *leaderworkerset.LeaderWorkerSet) {
				newLWS.Spec.Scheduling = &leaderworkerset.LeaderWorkerSetScheduling{
					SchedulingPolicy: &schedulingv1alpha3.WorkloadCompositePodGroupSchedulingPolicy{
						Basic: &schedulingv1alpha3.WorkloadCompositePodGroupBasicSchedulingPolicy{},
					},
				}
			},
			wantErr: "cannot switch the active scheduling level",
		},
		"shared priority class is immutable": {
			update: func(oldLWS, newLWS *leaderworkerset.LeaderWorkerSet) {
				newLWS.Spec.LeaderWorkerTemplate.WorkerTemplate.Spec.PriorityClassName = "new-priority"
			},
			wantErr: "cannot change priorityClassName",
		},
		"generated replica minCount follows size": {
			update: func(oldLWS, newLWS *leaderworkerset.LeaderWorkerSet) {
				newLWS.Spec.LeaderWorkerTemplate.Size = ptr.To[int32](4)
			},
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			lws := testScheduledLWS()
			var oldLWS *leaderworkerset.LeaderWorkerSet
			if tc.update != nil {
				oldLWS = lws.DeepCopy()
				lws = oldLWS.DeepCopy()
				tc.update(oldLWS, lws)
			}
			if tc.mutate != nil {
				tc.mutate(lws)
			}
			errs := ValidatePhaseOneWorkload(context.Background(), oldLWS, lws)
			if tc.wantErr == "" {
				assert.Empty(t, errs, "errors: %v", errs)
				return
			}
			require.NotEmpty(t, errs)
			assert.Contains(t, errs.ToAggregate().Error(), tc.wantErr)
		})
	}
}

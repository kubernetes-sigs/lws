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
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	schedulingv1alpha3 "k8s.io/api/scheduling/v1alpha3"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	leaderworkerset "sigs.k8s.io/lws/api/leaderworkerset/v1"
)

func TestKubernetesProvider_InjectPodGroupMetadata(t *testing.T) {
	podFor := func(annotations, labels map[string]string) *corev1.Pod {
		return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
			Name:        "test-lws-0",
			Annotations: annotations,
			Labels:      labels,
		}}
	}
	labels := map[string]string{
		leaderworkerset.SetNameLabelKey:     "test-lws",
		leaderworkerset.GroupIndexLabelKey:  "1",
		leaderworkerset.RevisionKey:         "rev1",
		leaderworkerset.WorkerIndexLabelKey: "2",
	}

	tests := map[string]struct {
		annotations map[string]string
		labels      map[string]string
		wantGroup   string
		wantErr     string
	}{
		"no scheduling annotation leaves the pod untouched": {
			annotations: map[string]string{},
			labels:      labels,
		},
		"lws mode": {
			annotations: map[string]string{WorkloadSchedulingAnnotationKey: string(SchedulingModeLWS)},
			labels:      labels,
			wantGroup:   "test-lws-lws",
		},
		"replica mode": {
			annotations: map[string]string{WorkloadSchedulingAnnotationKey: string(SchedulingModeReplica)},
			labels:      labels,
			wantGroup:   "test-lws-1-rev1",
		},
		"legacy true value means replica mode": {
			annotations: map[string]string{WorkloadSchedulingAnnotationKey: "true"},
			labels:      labels,
			wantGroup:   "test-lws-1-rev1",
		},
		"role mode on a worker": {
			annotations: map[string]string{WorkloadSchedulingAnnotationKey: string(SchedulingModeRole)},
			labels:      labels,
			wantGroup:   "test-lws-1-worker-rev1",
		},
		"unsupported mode": {
			annotations: map[string]string{WorkloadSchedulingAnnotationKey: "bogus"},
			labels:      labels,
			wantErr:     `unsupported workload-aware scheduling mode "bogus"`,
		},
	}

	provider := NewKubernetesProvider(fake.NewClientBuilder().Build())
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			pod := podFor(tc.annotations, tc.labels)
			err := provider.InjectPodGroupMetadata(pod)
			if tc.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.wantErr)
				assert.Nil(t, pod.Spec.SchedulingGroup)
				return
			}
			require.NoError(t, err)
			if tc.wantGroup == "" {
				assert.Nil(t, pod.Spec.SchedulingGroup)
				return
			}
			require.NotNil(t, pod.Spec.SchedulingGroup)
			require.NotNil(t, pod.Spec.SchedulingGroup.PodGroupName)
			assert.Equal(t, tc.wantGroup, *pod.Spec.SchedulingGroup.PodGroupName)
		})
	}

	t.Run("role mode on the leader", func(t *testing.T) {
		leaderLabels := map[string]string{}
		for k, v := range labels {
			leaderLabels[k] = v
		}
		leaderLabels[leaderworkerset.WorkerIndexLabelKey] = "0"
		pod := podFor(map[string]string{WorkloadSchedulingAnnotationKey: string(SchedulingModeRole)}, leaderLabels)

		require.NoError(t, provider.InjectPodGroupMetadata(pod))
		require.NotNil(t, pod.Spec.SchedulingGroup)
		assert.Equal(t, "test-lws-1-leader-rev1", *pod.Spec.SchedulingGroup.PodGroupName)
	})

	t.Run("an explicit workload name overrides the set name", func(t *testing.T) {
		pod := podFor(map[string]string{
			WorkloadSchedulingAnnotationKey: string(SchedulingModeLWS),
			WorkloadNameAnnotationKey:       "custom-workload",
		}, labels)

		require.NoError(t, provider.InjectPodGroupMetadata(pod))
		require.NotNil(t, pod.Spec.SchedulingGroup)
		assert.Equal(t, "custom-workload-lws", *pod.Spec.SchedulingGroup.PodGroupName)
	})
}

func TestKubernetesProvider_CreatePodGroupIfNotExists(t *testing.T) {
	// The Kubernetes provider pre-creates scheduling objects in
	// ReconcileScheduling, so the pod-driven hook is a no-op.
	provider := NewKubernetesProvider(fake.NewClientBuilder().Build())
	assert.NoError(t, provider.CreatePodGroupIfNotExists(context.Background(), testScheduledLWS(), &corev1.Pod{}))
}

func TestWorkloadAPIError(t *testing.T) {
	t.Run("a missing API maps to the APINotAvailable reason", func(t *testing.T) {
		err := workloadAPIError(ReasonWorkloadCreateFailed, &apimeta.NoKindMatchError{
			GroupKind: schema.GroupKind{Group: "scheduling.k8s.io", Kind: "Workload"},
		})
		assert.Equal(t, ReasonAPINotAvailable, ReconcileErrorReason(err))
	})

	t.Run("other errors keep the fallback reason", func(t *testing.T) {
		err := workloadAPIError(ReasonWorkloadCreateFailed, errors.New("boom"))
		assert.Equal(t, ReasonWorkloadCreateFailed, ReconcileErrorReason(err))
	})
}

func TestGangAtSelectedLevel(t *testing.T) {
	gangPolicy := &schedulingv1alpha3.WorkloadCompositePodGroupSchedulingPolicy{
		Gang: &schedulingv1alpha3.WorkloadCompositePodGroupGangSchedulingPolicy{},
	}
	basicPolicy := &schedulingv1alpha3.WorkloadCompositePodGroupSchedulingPolicy{
		Basic: &schedulingv1alpha3.WorkloadCompositePodGroupBasicSchedulingPolicy{},
	}
	leafGang := &schedulingv1alpha3.WorkloadPodGroupSchedulingPolicy{
		Gang: &schedulingv1alpha3.WorkloadPodGroupGangSchedulingPolicy{},
	}
	leafBasic := &schedulingv1alpha3.WorkloadPodGroupSchedulingPolicy{
		Basic: &schedulingv1alpha3.WorkloadPodGroupBasicSchedulingPolicy{},
	}

	tests := map[string]struct {
		mutate func(*leaderworkerset.LeaderWorkerSet)
		mode   SchedulingMode
		want   bool
	}{
		"lws level defaults to non-gang": {
			mode: SchedulingModeLWS,
			want: false,
		},
		"lws level with an explicit gang policy": {
			mutate: func(lws *leaderworkerset.LeaderWorkerSet) {
				lws.Spec.Scheduling.SchedulingPolicy = gangPolicy
			},
			mode: SchedulingModeLWS,
			want: true,
		},
		"replica level defaults to gang": {
			mode: SchedulingModeReplica,
			want: true,
		},
		"replica level with an explicit basic policy": {
			mutate: func(lws *leaderworkerset.LeaderWorkerSet) {
				lws.Spec.Scheduling.Replica = &leaderworkerset.LeaderWorkerSetReplicaScheduling{
					SchedulingPolicy: basicPolicy,
				}
			},
			mode: SchedulingModeReplica,
			want: false,
		},
		"role level with a gang leader": {
			mutate: func(lws *leaderworkerset.LeaderWorkerSet) {
				lws.Spec.Scheduling.Replica = &leaderworkerset.LeaderWorkerSetReplicaScheduling{
					Leader: &leaderworkerset.LeaderWorkerSetLeaderScheduling{SchedulingPolicy: leafGang},
				}
			},
			mode: SchedulingModeRole,
			want: true,
		},
		"role level with a basic leader and worker": {
			mutate: func(lws *leaderworkerset.LeaderWorkerSet) {
				lws.Spec.Scheduling.Replica = &leaderworkerset.LeaderWorkerSetReplicaScheduling{
					Leader: &leaderworkerset.LeaderWorkerSetLeaderScheduling{SchedulingPolicy: leafBasic},
					Worker: &leaderworkerset.LeaderWorkerSetWorkerScheduling{SchedulingPolicy: leafBasic},
				}
			},
			mode: SchedulingModeRole,
			want: false,
		},
		"unknown mode": {
			mode: SchedulingMode("bogus"),
			want: false,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			lws := testScheduledLWS()
			if tc.mutate != nil {
				tc.mutate(lws)
			}
			assert.Equal(t, tc.want, gangAtSelectedLevel(lws, tc.mode))
		})
	}
}

func TestTopologyAtSelectedLevel(t *testing.T) {
	topology := []schedulingv1alpha3.TopologyConstraint{{Key: "topology.kubernetes.io/rack"}}

	tests := map[string]struct {
		mutate func(*leaderworkerset.LeaderWorkerSet)
		mode   SchedulingMode
		want   bool
	}{
		"no constraints": {
			mode: SchedulingModeLWS,
			want: false,
		},
		"lws level topology": {
			mutate: func(lws *leaderworkerset.LeaderWorkerSet) {
				lws.Spec.Scheduling.SchedulingConstraints = &schedulingv1alpha3.WorkloadCompositePodGroupSchedulingConstraints{Topology: topology}
			},
			mode: SchedulingModeLWS,
			want: true,
		},
		"replica level topology": {
			mutate: func(lws *leaderworkerset.LeaderWorkerSet) {
				lws.Spec.Scheduling.Replica = &leaderworkerset.LeaderWorkerSetReplicaScheduling{
					SchedulingConstraints: &schedulingv1alpha3.WorkloadCompositePodGroupSchedulingConstraints{Topology: topology},
				}
			},
			mode: SchedulingModeReplica,
			want: true,
		},
		"replica level without constraints": {
			mode: SchedulingModeReplica,
			want: false,
		},
		"role level topology on the worker": {
			mutate: func(lws *leaderworkerset.LeaderWorkerSet) {
				lws.Spec.Scheduling.Replica = &leaderworkerset.LeaderWorkerSetReplicaScheduling{
					Worker: &leaderworkerset.LeaderWorkerSetWorkerScheduling{
						SchedulingConstraints: &schedulingv1alpha3.WorkloadPodGroupSchedulingConstraints{Topology: topology},
					},
				}
			},
			mode: SchedulingModeRole,
			want: true,
		},
		"role level without constraints": {
			mutate: func(lws *leaderworkerset.LeaderWorkerSet) {
				lws.Spec.Scheduling.Replica = &leaderworkerset.LeaderWorkerSetReplicaScheduling{
					Leader: &leaderworkerset.LeaderWorkerSetLeaderScheduling{},
				}
			},
			mode: SchedulingModeRole,
			want: false,
		},
		"unknown mode": {
			mode: SchedulingMode("bogus"),
			want: false,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			lws := testScheduledLWS()
			if tc.mutate != nil {
				tc.mutate(lws)
			}
			assert.Equal(t, tc.want, topologyAtSelectedLevel(lws, tc.mode))
		})
	}
}

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
	"strings"
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

func TestKubernetesProvider_InjectPodGroupMetadataGroupIncarnation(t *testing.T) {
	provider := NewKubernetesProvider(fake.NewClientBuilder().Build())
	// workloadName stands in for the workload name the LWS controller writes on
	// pod templates.
	const workloadName = "test-lws-k7m2q"
	// podFor returns a pod of group 1 at revision rev1 with the annotations of
	// its template. The statefulset controller recreates a leader with the same
	// name, labels and annotations.
	podFor := func(mode SchedulingMode, workerIndex string, annotations map[string]string) *corev1.Pod {
		podAnnotations := map[string]string{
			WorkloadSchedulingAnnotationKey: string(mode),
			WorkloadNameAnnotationKey:       workloadName,
		}
		for k, v := range annotations {
			podAnnotations[k] = v
		}
		return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
			Name:        "test-lws-1",
			Annotations: podAnnotations,
			Labels: map[string]string{
				leaderworkerset.SetNameLabelKey:     "test-lws",
				leaderworkerset.GroupIndexLabelKey:  "1",
				leaderworkerset.RevisionKey:         "rev1",
				leaderworkerset.WorkerIndexLabelKey: workerIndex,
			},
		}}
	}
	admit := func(t *testing.T, pod *corev1.Pod) string {
		t.Helper()
		require.NoError(t, provider.InjectPodGroupMetadata(pod))
		require.NotNil(t, pod.Spec.SchedulingGroup)
		require.NotNil(t, pod.Spec.SchedulingGroup.PodGroupName)
		return *pod.Spec.SchedulingGroup.PodGroupName
	}
	// incarnationOf returns the group incarnation the webhook recorded on pod.
	incarnationOf := func(t *testing.T, pod *corev1.Pod) string {
		t.Helper()
		incarnation, found := strings.CutPrefix(pod.Annotations[WorkloadNameAnnotationKey], workloadName+".")
		require.True(t, found, "workload name %q has no group incarnation", pod.Annotations[WorkloadNameAnnotationKey])
		require.Len(t, incarnation, groupIncarnationLength)
		require.True(t, isGroupIncarnation(incarnation), "invalid group incarnation %q", incarnation)
		return incarnation
	}

	for mode, suffix := range map[SchedulingMode]string{
		SchedulingModeReplica: "-1-rev1",
		SchedulingModeRole:    "-1-leader-rev1",
	} {
		t.Run(string(mode)+" mode ordinal leader", func(t *testing.T) {
			t.Run("every admission draws its own incarnation", func(t *testing.T) {
				first, second := podFor(mode, "0", nil), podFor(mode, "0", nil)
				firstName, secondName := admit(t, first), admit(t, second)
				for _, pod := range []*corev1.Pod{first, second} {
					assert.Equal(t, workloadName+"."+incarnationOf(t, pod)+suffix, *pod.Spec.SchedulingGroup.PodGroupName)
				}
				assert.NotEqual(t, firstName, secondName, "a recreated leader must not reuse the PodGroups of its predecessor")
			})
			t.Run("reinvocation keeps the incarnation", func(t *testing.T) {
				pod := podFor(mode, "0", nil)
				name := admit(t, pod)
				incarnated := pod.Annotations[WorkloadNameAnnotationKey]
				assert.Equal(t, name, admit(t, pod))
				assert.Equal(t, incarnated, pod.Annotations[WorkloadNameAnnotationKey])
			})
			t.Run("a malformed incarnation is replaced", func(t *testing.T) {
				for _, malformed := range []string{".", ".x7k2", ".X7K2P", ".x7k2p.b"} {
					pod := podFor(mode, "0", map[string]string{WorkloadNameAnnotationKey: workloadName + malformed})
					name := admit(t, pod)
					assert.Equal(t, workloadName+"."+incarnationOf(t, pod)+suffix, name, malformed)
				}
			})
		})
	}

	t.Run("workers keep the workload name of their template", func(t *testing.T) {
		incarnated := workloadName + ".x7k2p"
		for mode, want := range map[SchedulingMode]string{
			SchedulingModeReplica: incarnated + "-1-rev1",
			SchedulingModeRole:    incarnated + "-1-worker-rev1",
		} {
			pod := podFor(mode, "2", map[string]string{WorkloadNameAnnotationKey: incarnated})
			assert.Equal(t, want, admit(t, pod), string(mode))
			assert.Equal(t, incarnated, pod.Annotations[WorkloadNameAnnotationKey], string(mode))
		}
		pod := podFor(SchedulingModeReplica, "2", nil)
		assert.Equal(t, workloadName+"-1-rev1", admit(t, pod))
		assert.Equal(t, workloadName, pod.Annotations[WorkloadNameAnnotationKey])
	})

	t.Run("hash leaders get no incarnation", func(t *testing.T) {
		pod := podFor(SchedulingModeReplica, "0", map[string]string{
			leaderworkerset.GroupIdentityAnnotationKey: string(leaderworkerset.GroupIdentityHash),
		})
		assert.Equal(t, workloadName+"-1-rev1", admit(t, pod))
		assert.Equal(t, workloadName, pod.Annotations[WorkloadNameAnnotationKey])
	})

	t.Run("whole-LWS pods get no incarnation", func(t *testing.T) {
		pod := podFor(SchedulingModeLWS, "0", nil)
		assert.Equal(t, workloadName+"-lws", admit(t, pod))
		assert.Equal(t, workloadName, pod.Annotations[WorkloadNameAnnotationKey])
	})

	t.Run("leaders without a workload name get no incarnation", func(t *testing.T) {
		pod := podFor(SchedulingModeReplica, "0", nil)
		delete(pod.Annotations, WorkloadNameAnnotationKey)
		assert.Equal(t, "test-lws-1-rev1", admit(t, pod))
		assert.NotContains(t, pod.Annotations, WorkloadNameAnnotationKey)
	})
}

func TestKubernetesProvider_CreatePodGroupIfNotExists(t *testing.T) {
	ctx := context.Background()
	lws := testScheduledLWS()
	fakeClient := newKubernetesFakeClientBuilder().Build()
	provider := NewKubernetesProvider(fakeClient)
	require.NoError(t, provider.ReconcileScheduling(ctx, lws, 1, "revision-1"))

	leader := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-lws-0",
			Namespace: lws.Namespace,
			Labels: map[string]string{
				leaderworkerset.SetNameLabelKey:     lws.Name,
				leaderworkerset.GroupIndexLabelKey:  "0",
				leaderworkerset.WorkerIndexLabelKey: "0",
				leaderworkerset.RevisionKey:         "revision-1",
			},
		},
	}
	assert.NoError(t, provider.CreatePodGroupIfNotExists(ctx, lws, leader))
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

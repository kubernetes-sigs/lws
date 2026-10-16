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
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"

	disaggv1 "sigs.k8s.io/lws/api/disaggregatedset/v1"
	leaderworkerset "sigs.k8s.io/lws/api/leaderworkerset/v1"
	disaggutils "sigs.k8s.io/lws/pkg/utils/disaggregatedset"
	"sigs.k8s.io/lws/pkg/webhooks"
	"sigs.k8s.io/lws/test/wrappers"
)

func TestValidateHashSubRoles(t *testing.T) {
	ds := &disaggv1.DisaggregatedSet{Spec: disaggv1.DisaggregatedSetSpec{Roles: []disaggv1.DisaggregatedRoleSpec{{
		Name: "model", SubRoles: []disaggv1.DisaggregatedSubRoleSpec{{Name: "a"}, {Name: "b"}},
	}}}}
	ds.Spec.Roles[0].Spec.GroupIdentity = leaderworkerset.GroupIdentityHash
	webhook := &DisaggregatedSetWebhook{}
	_, err := webhook.ValidateCreate(t.Context(), ds)
	require.NoError(t, err)
	_, err = webhook.ValidateUpdate(t.Context(), ds.DeepCopy(), ds)
	require.NoError(t, err)
}

func TestChildAdmissionConstraints(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*disaggv1.DisaggregatedSet)
		error  string
	}{
		{"valid name", func(ds *disaggv1.DisaggregatedSet) { ds.Name = "model-1" }, ""},
		{"dotted name", func(ds *disaggv1.DisaggregatedSet) { ds.Name = "my.model" }, "metadata.name"},
		{"numeric prefix", func(ds *disaggv1.DisaggregatedSet) { ds.Name = "1model" }, "metadata.name"},
		{"zero group size", func(ds *disaggv1.DisaggregatedSet) {
			ds.Spec.Roles[0].Spec.LeaderWorkerTemplate.Size = ptr.To[int32](0)
		}, "size must be equal or greater than 1"},
		{"subgroup topology without subgroup", func(ds *disaggv1.DisaggregatedSet) {
			ds.Spec.Roles[0].Annotations = map[string]string{leaderworkerset.SubGroupExclusiveKeyAnnotationKey: "rack"}
		}, "without subGroupSize"},
		{"subgroup topology with subgroup", func(ds *disaggv1.DisaggregatedSet) {
			ds.Spec.Roles[0].Annotations = map[string]string{leaderworkerset.SubGroupExclusiveKeyAnnotationKey: "rack"}
			ds.Spec.Roles[0].Spec.LeaderWorkerTemplate.SubGroupPolicy = &leaderworkerset.SubGroupPolicy{SubGroupSize: ptr.To[int32](3)}
		}, ""},
		{"compatible subgroup", func(ds *disaggv1.DisaggregatedSet) {
			ds.Spec.Roles[0].Spec.LeaderWorkerTemplate.SubGroupPolicy = &leaderworkerset.SubGroupPolicy{SubGroupSize: ptr.To[int32](3)}
		}, ""},
		{"default group size", func(ds *disaggv1.DisaggregatedSet) {
			ds.Spec.Roles[0].Spec.LeaderWorkerTemplate.Size = nil
			ds.Spec.Roles[0].Spec.LeaderWorkerTemplate.SubGroupPolicy = &leaderworkerset.SubGroupPolicy{SubGroupSize: ptr.To[int32](1)}
		}, ""},
		{"subgroup larger than default group", func(ds *disaggv1.DisaggregatedSet) {
			ds.Spec.Roles[0].Spec.LeaderWorkerTemplate.Size = nil
			ds.Spec.Roles[0].Spec.LeaderWorkerTemplate.SubGroupPolicy = &leaderworkerset.SubGroupPolicy{SubGroupSize: ptr.To[int32](2)}
		}, "subGroupSize cannot be larger than size"},
		{"incompatible subgroup", func(ds *disaggv1.DisaggregatedSet) {
			ds.Spec.Roles[0].Spec.LeaderWorkerTemplate.SubGroupPolicy = &leaderworkerset.SubGroupPolicy{SubGroupSize: ptr.To[int32](4)}
		}, "spec.roles[0].spec.leaderWorkerTemplate.subGroupPolicy.subGroupSize"},
		{"missing subgroup size", func(ds *disaggv1.DisaggregatedSet) {
			ds.Spec.Roles[0].Spec.LeaderWorkerTemplate.SubGroupPolicy = &leaderworkerset.SubGroupPolicy{}
		}, "subGroupSize is required"},
		{"zero subgroup size", func(ds *disaggv1.DisaggregatedSet) {
			ds.Spec.Roles[0].Spec.LeaderWorkerTemplate.SubGroupPolicy = &leaderworkerset.SubGroupPolicy{SubGroupSize: ptr.To[int32](0)}
		}, "subGroupSize must be equal or greater than 1"},
		{"leader excluded", func(ds *disaggv1.DisaggregatedSet) {
			ds.Spec.Roles[0].Spec.LeaderWorkerTemplate.SubGroupPolicy = &leaderworkerset.SubGroupPolicy{SubGroupSize: ptr.To[int32](3), Type: ptr.To(leaderworkerset.SubGroupPolicyTypeLeaderExcluded)}
		}, "size-1 must be divisible"},
		{"compatible leader excluded", func(ds *disaggv1.DisaggregatedSet) {
			ds.Spec.Roles[0].Spec.LeaderWorkerTemplate.SubGroupPolicy = &leaderworkerset.SubGroupPolicy{SubGroupSize: ptr.To[int32](5), Type: ptr.To(leaderworkerset.SubGroupPolicyTypeLeaderExcluded)}
		}, ""},
		{"aggregate at limit", func(ds *disaggv1.DisaggregatedSet) {
			ds.Spec.Roles[0].SubRoles = []disaggv1.DisaggregatedSubRoleSpec{{Name: "a", Replicas: ptr.To[int32](500000)}, {Name: "b", Replicas: ptr.To[int32](500000)}}
		}, ""},
		{"aggregate above limit", func(ds *disaggv1.DisaggregatedSet) {
			ds.Spec.Roles[0].SubRoles = []disaggv1.DisaggregatedSubRoleSpec{{Name: "a", Replicas: ptr.To[int32](600000)}, {Name: "b", Replicas: ptr.To[int32](600000)}}
		}, "spec.roles[0].subRoles"},
		{"external child target is unknown", func(ds *disaggv1.DisaggregatedSet) {
			ds.Spec.Roles[0].SubRoles = []disaggv1.DisaggregatedSubRoleSpec{{Name: "a", Replicas: ptr.To[int32](1000000)}, {Name: "b", Scaling: &disaggv1.RoleScaling{Mode: disaggv1.RoleScalingExternal}}}
		}, ""},
		{"group count times size", func(ds *disaggv1.DisaggregatedSet) {
			ds.Spec.Roles[0].Spec.Replicas = ptr.To[int32](1000000)
			ds.Spec.Roles[0].Spec.LeaderWorkerTemplate.Size = ptr.To[int32](2148)
		}, "product of replicas"},
		{"aggregate count times size", func(ds *disaggv1.DisaggregatedSet) {
			ds.Spec.Roles[0].SubRoles = []disaggv1.DisaggregatedSubRoleSpec{{Name: "a", Replicas: ptr.To[int32](500000)}, {Name: "b", Replicas: ptr.To[int32](500000)}}
			ds.Spec.Roles[0].Spec.LeaderWorkerTemplate.Size = ptr.To[int32](2148)
		}, "product of replicas"},
		{"ignored parent count", func(ds *disaggv1.DisaggregatedSet) {
			ds.Spec.Roles[0].Spec.Replicas = ptr.To[int32](1000000)
			ds.Spec.Roles[0].Spec.LeaderWorkerTemplate.Size = ptr.To[int32](2148)
			ds.Spec.Roles[0].SubRoles = []disaggv1.DisaggregatedSubRoleSpec{{Name: "a"}, {Name: "b"}}
		}, ""},
		{"external count unresolved", func(ds *disaggv1.DisaggregatedSet) {
			ds.Spec.Roles[0].Spec.Replicas = ptr.To[int32](1000000)
			ds.Spec.Roles[0].Spec.LeaderWorkerTemplate.Size = ptr.To[int32](2148)
			ds.Spec.Roles[0].Scaling = &disaggv1.RoleScaling{Mode: disaggv1.RoleScalingExternal}
		}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ds := &disaggv1.DisaggregatedSet{ObjectMeta: metav1.ObjectMeta{Name: "model", Namespace: "default"}}
			ds.Spec.Roles = []disaggv1.DisaggregatedRoleSpec{{Name: "prefill", LeaderWorkerSetTemplateSpec: leaderworkerset.LeaderWorkerSetTemplateSpec{
				Spec: leaderworkerset.LeaderWorkerSetSpec{Replicas: ptr.To[int32](1), LeaderWorkerTemplate: leaderworkerset.LeaderWorkerTemplate{Size: ptr.To[int32](6)}},
			}}}
			old := ds.DeepCopy()
			tc.change(ds)
			before := ds.DeepCopy()
			hook := &DisaggregatedSetWebhook{}
			_, createErr := hook.ValidateCreate(t.Context(), ds)
			_, updateErr := hook.ValidateUpdate(t.Context(), old, ds)
			require.Equal(t, before, ds, "validation must not mutate the parent")
			if tc.error != "" {
				require.ErrorContains(t, createErr, tc.error)
				require.ErrorContains(t, updateErr, tc.error)
				return
			}
			require.NoError(t, createErr)
			require.NoError(t, updateErr)
			// Check accepted configurations against native child admission too.
			role := ds.Spec.Roles[0]
			lws := &leaderworkerset.LeaderWorkerSet{ObjectMeta: metav1.ObjectMeta{
				Name: disaggutils.GenerateName(ds.Name, 0, disaggutils.ComputeRevision(ds.Spec.Roles), role.Name), Namespace: ds.Namespace,
				Annotations: role.Annotations,
			}, Spec: role.Spec}
			lws.Spec.Replicas = validationReplicasForRole(role)
			// Unknown External children may resolve to zero; do not invent capacity.
			for _, child := range role.SubRoles {
				if child.Scaling != nil && child.Scaling.Mode == disaggv1.RoleScalingExternal {
					*lws.Spec.Replicas -= ptr.Deref(child.Replicas, 1)
				}
			}
			// Size is defaulted by the CRD schema, not the mutating webhook.
			lws.Spec.LeaderWorkerTemplate.Size = ptr.To(ptr.Deref(role.Spec.LeaderWorkerTemplate.Size, 1))
			if role.Scaling != nil && role.Scaling.Mode == disaggv1.RoleScalingExternal {
				lws.Spec.Replicas = ptr.To[int32](1)
			}
			native := &webhooks.LeaderWorkerSetWebhook{}
			require.NoError(t, native.Default(t.Context(), lws))
			_, err := native.ValidateCreate(t.Context(), lws)
			require.NoError(t, err)
		})
	}
}

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
			name: "invalid placement policy with malformed topology key",
			obj: &disaggv1.DisaggregatedSet{
				ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
				Spec: disaggv1.DisaggregatedSetSpec{
					PlacementPolicy: &disaggv1.PlacementPolicy{
						Type:     disaggv1.PlacementExclusiveSlice,
						Topology: "invalid topology key",
					},
					Roles: []disaggv1.DisaggregatedRoleSpec{
						{Name: "prefill", LeaderWorkerSetTemplateSpec: leaderworkerset.LeaderWorkerSetTemplateSpec{Spec: leaderworkerset.LeaderWorkerSetSpec{Replicas: ptr.To(int32(2))}}},
						{Name: "decode", LeaderWorkerSetTemplateSpec: leaderworkerset.LeaderWorkerSetTemplateSpec{Spec: leaderworkerset.LeaderWorkerSetSpec{Replicas: ptr.To(int32(2))}}},
					},
				},
			},
			expectError: true,
			errorMsg:    "spec.placementPolicy.topology",
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
								ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{leaderworkerset.ExclusiveKeyAnnotationKey: "cloud.google.com/gke-nodepool"}},
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
											ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{leaderworkerset.ExclusiveKeyAnnotationKey: "cloud.google.com/gke-nodepool"}},
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
								ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{leaderworkerset.SubGroupExclusiveKeyAnnotationKey: "cloud.google.com/gke-nodepool"}},
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
											ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{leaderworkerset.SubGroupExclusiveKeyAnnotationKey: "cloud.google.com/gke-nodepool"}},
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
											ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{leaderworkerset.SubGroupExclusiveKeyAnnotationKey: "cloud.google.com/gke-nodepool"}},
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
		// --- Generated name length validation tests ---
		{
			// With slices=1 (default), overhead = 3 separators + 1 slice digit + 8 revision + 13 max suffix (1 dash + 1 group index digit + 11 hash) = 25.
			// dsName(31) + roleName(7) + 25 = 63 → exactly at limit.
			name: "generated names at exact 63-char limit is accepted",
			obj: &disaggv1.DisaggregatedSet{
				ObjectMeta: metav1.ObjectMeta{Name: strings.Repeat("a", 31), Namespace: "default"},
				Spec: disaggv1.DisaggregatedSetSpec{
					Roles: []disaggv1.DisaggregatedRoleSpec{
						{
							Name: strings.Repeat("b", 7),
							LeaderWorkerSetTemplateSpec: leaderworkerset.LeaderWorkerSetTemplateSpec{
								Spec: leaderworkerset.LeaderWorkerSetSpec{Replicas: ptr.To(int32(1))},
							},
						},
						{
							Name: "b",
							LeaderWorkerSetTemplateSpec: leaderworkerset.LeaderWorkerSetTemplateSpec{
								Spec: leaderworkerset.LeaderWorkerSetSpec{Replicas: ptr.To(int32(1))},
							},
						},
					},
				},
			},
			expectError: false,
		},
		{
			// dsName(31) + roleName(8) + 25 = 64 → 1 char over.
			name: "generated names 1 char over limit is rejected",
			obj: &disaggv1.DisaggregatedSet{
				ObjectMeta: metav1.ObjectMeta{Name: strings.Repeat("a", 31), Namespace: "default"},
				Spec: disaggv1.DisaggregatedSetSpec{
					Roles: []disaggv1.DisaggregatedRoleSpec{
						{
							Name: strings.Repeat("b", 8),
							LeaderWorkerSetTemplateSpec: leaderworkerset.LeaderWorkerSetTemplateSpec{
								Spec: leaderworkerset.LeaderWorkerSetSpec{Replicas: ptr.To(int32(1))},
							},
						},
						{
							Name: "b",
							LeaderWorkerSetTemplateSpec: leaderworkerset.LeaderWorkerSetTemplateSpec{
								Spec: leaderworkerset.LeaderWorkerSetSpec{Replicas: ptr.To(int32(1))},
							},
						},
					},
				},
			},
			expectError: true,
			errorMsg:    "would exceed the DNS-1035 limit",
		},
		{
			// Long DS name (40 chars) with a short role (3 chars): 40+3+25=68 → rejected.
			name: "long DS name with short role name exceeds limit",
			obj: &disaggv1.DisaggregatedSet{
				ObjectMeta: metav1.ObjectMeta{Name: strings.Repeat("x", 40), Namespace: "default"},
				Spec: disaggv1.DisaggregatedSetSpec{
					Roles: []disaggv1.DisaggregatedRoleSpec{
						{
							Name: "abc",
							LeaderWorkerSetTemplateSpec: leaderworkerset.LeaderWorkerSetTemplateSpec{
								Spec: leaderworkerset.LeaderWorkerSetSpec{Replicas: ptr.To(int32(1))},
							},
						},
						{
							Name: "de",
							LeaderWorkerSetTemplateSpec: leaderworkerset.LeaderWorkerSetTemplateSpec{
								Spec: leaderworkerset.LeaderWorkerSetSpec{Replicas: ptr.To(int32(1))},
							},
						},
					},
				},
			},
			expectError: true,
			errorMsg:    "would exceed the DNS-1035 limit",
		},
		{
			// slices=100 → max index 99 → 2 digits. Overhead = 3+2+8+13 = 26.
			// dsName(31) + roleName(7) + 26 = 64 → rejected (would be accepted with slices=1).
			name: "multi-slice worst case pushes name over limit",
			obj: &disaggv1.DisaggregatedSet{
				ObjectMeta: metav1.ObjectMeta{Name: strings.Repeat("a", 31), Namespace: "default"},
				Spec: disaggv1.DisaggregatedSetSpec{
					Slices: ptr.To(int32(100)),
					Roles: []disaggv1.DisaggregatedRoleSpec{
						{
							Name: strings.Repeat("b", 7),
							LeaderWorkerSetTemplateSpec: leaderworkerset.LeaderWorkerSetTemplateSpec{
								Spec: leaderworkerset.LeaderWorkerSetSpec{Replicas: ptr.To(int32(1))},
							},
						},
						{
							Name: "b",
							LeaderWorkerSetTemplateSpec: leaderworkerset.LeaderWorkerSetTemplateSpec{
								Spec: leaderworkerset.LeaderWorkerSetSpec{Replicas: ptr.To(int32(1))},
							},
						},
					},
				},
			},
			expectError: true,
			errorMsg:    "would exceed the DNS-1035 limit",
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

func TestValidateRolloutReplicaDefaults(t *testing.T) {
	webhook := &DisaggregatedSetWebhook{}
	// Preserve DS's existing handling of omitted and zero template replicas.
	// Integration tests cover the API server's replica defaults.
	for _, tc := range []struct {
		name           string
		replicas       *int32
		external       bool
		maxUnavailable int32
		wantError      bool
	}{
		{name: "static zero", replicas: ptr.To[int32](0)},
		{name: "static positive", replicas: ptr.To[int32](1), wantError: true},
		{name: "static default"},
		{name: "static default with valid budget", maxUnavailable: 1},
		{name: "external zero", replicas: ptr.To[int32](0), external: true},
		{name: "external zero with valid budget", replicas: ptr.To[int32](0), external: true, maxUnavailable: 1},
		{name: "external default", external: true},
		{name: "external positive", replicas: ptr.To[int32](1), external: true, wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			obj := wrappers.BuildDisaggregatedSet("test", "default").
				WithRole("prefill", 1, "nginx").
				WithRole("decode", 1, "nginx").Obj()
			for i := range obj.Spec.Roles {
				role := &obj.Spec.Roles[i]
				role.Spec.Replicas = tc.replicas
				role.Spec.RolloutStrategy.RollingUpdateConfiguration = &leaderworkerset.RollingUpdateConfiguration{
					MaxUnavailable: intstr.FromInt32(tc.maxUnavailable),
				}
				if tc.external {
					role.Scaling = &disaggv1.RoleScaling{Mode: disaggv1.RoleScalingExternal}
				}
			}
			original := obj.DeepCopy()
			oldObj := obj.DeepCopy()
			for i := range oldObj.Spec.Roles {
				oldObj.Spec.Roles[i].Spec.RolloutStrategy.RollingUpdateConfiguration.MaxUnavailable = intstr.FromInt32(1)
			}
			_, createErr := webhook.ValidateCreate(t.Context(), obj)
			_, updateErr := webhook.ValidateUpdate(t.Context(), oldObj, obj)
			require.Equal(t, original, obj, "validation must not default or rewrite the role")
			for _, err := range []error{createErr, updateErr} {
				if tc.wantError {
					require.Error(t, err)
					require.Contains(t, err.Error(), "spec.roles[0].spec.rolloutStrategy.rollingUpdateConfiguration.maxUnavailable")
				} else {
					require.NoError(t, err)
				}
			}
			// Unchanged legacy budgets remain updateable when omitted
			// replicas are returned with their API default.
			updated := obj.DeepCopy()
			if tc.replicas == nil {
				for i := range updated.Spec.Roles {
					updated.Spec.Roles[i].Spec.Replicas = ptr.To[int32](1)
				}
			}
			_, err := webhook.ValidateUpdate(t.Context(), obj, updated)
			require.NoError(t, err)
			require.Equal(t, original, obj)
		})
	}
}

func TestValidateZeroReplicaRolloutStrategy(t *testing.T) {
	webhook := &DisaggregatedSetWebhook{}
	for _, fieldName := range []string{"maxSurge", "maxUnavailable"} {
		for _, tc := range []struct {
			name      string
			value     intstr.IntOrString
			wantError bool
		}{
			{name: "negative integer", value: intstr.FromInt32(-1), wantError: true},
			{name: "invalid percentage", value: intstr.FromString("invalid"), wantError: true},
			{name: "percentage above 100", value: intstr.FromString("101%"), wantError: true},
			{name: "zero integer", value: intstr.FromInt32(0)},
			{name: "zero percentage", value: intstr.FromString("0%")},
		} {
			t.Run(fieldName+"/"+tc.name, func(t *testing.T) {
				obj := &disaggv1.DisaggregatedSet{
					ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
				}
				for _, name := range []string{"prefill", "decode"} {
					obj.Spec.Roles = append(obj.Spec.Roles, disaggv1.DisaggregatedRoleSpec{
						Name: name,
						LeaderWorkerSetTemplateSpec: leaderworkerset.LeaderWorkerSetTemplateSpec{
							Spec: leaderworkerset.LeaderWorkerSetSpec{
								Replicas: ptr.To[int32](0),
								RolloutStrategy: leaderworkerset.RolloutStrategy{
									RollingUpdateConfiguration: &leaderworkerset.RollingUpdateConfiguration{},
								},
							},
						},
					})
				}
				oldObj := obj.DeepCopy()
				rollout := obj.Spec.Roles[0].Spec.RolloutStrategy.RollingUpdateConfiguration
				if fieldName == "maxSurge" {
					rollout.MaxSurge = tc.value
				} else {
					rollout.MaxUnavailable = tc.value
				}

				_, createErr := webhook.ValidateCreate(context.Background(), obj)
				_, updateErr := webhook.ValidateUpdate(context.Background(), oldObj, obj)
				for _, err := range []error{createErr, updateErr} {
					if tc.wantError {
						require.ErrorContains(t, err, "spec.roles[0].spec.rolloutStrategy.rollingUpdateConfiguration."+fieldName)
					} else {
						require.NoError(t, err)
					}
				}
			})
		}
	}
}

func TestValidateSubRoleRolloutOverrides(t *testing.T) {
	zero, one := intstr.FromInt(0), intstr.FromInt(1)
	zeroPercent, quarter := intstr.FromString("0%"), intstr.FromString("25%")
	parent := &leaderworkerset.RollingUpdateConfiguration{MaxSurge: zero, MaxUnavailable: quarter}
	for _, tc := range []struct {
		name               string
		parent             *leaderworkerset.RollingUpdateConfiguration
		replicas           *int32
		external           bool
		surge, unavailable *intstr.IntOrString
		wantError          string
	}{
		{name: "inherit existing small-child budgets", parent: parent},
		{name: "inherit default unavailable", surge: &zero},
		{name: "explicit zero overrides default unavailable", unavailable: &zero, wantError: "maxUnavailable"},
		{name: "override both", surge: &zero, unavailable: &one},
		{name: "inherit unavailable", parent: parent, surge: &one},
		{name: "partial inherited pair rounds to zero", parent: parent, unavailable: &quarter, wantError: "maxUnavailable"},
		{name: "percent uses child count", replicas: ptr.To[int32](4), surge: &zero, unavailable: &quarter},
		{name: "zero child permits zero pair", replicas: ptr.To[int32](0), surge: &zero, unavailable: &zeroPercent},
		{name: "external unknown target permits positive percentage", external: true, surge: &zero, unavailable: &quarter},
		{name: "external rejects universally zero pair", external: true, surge: &zero, unavailable: &zeroPercent, wantError: "maxUnavailable"},
		{name: "negative surge", surge: ptr.To(intstr.FromInt(-1)), wantError: "maxSurge"},
		{name: "negative unavailable", unavailable: ptr.To(intstr.FromInt(-1)), wantError: "maxUnavailable"},
		{name: "malformed surge", surge: ptr.To(intstr.FromString("bad")), wantError: "maxSurge"},
		{name: "malformed unavailable", unavailable: ptr.To(intstr.FromString("bad")), wantError: "maxUnavailable"},
		{name: "excess surge percentage", surge: ptr.To(intstr.FromString("101%")), wantError: "maxSurge"},
		{name: "excess unavailable percentage", unavailable: ptr.To(intstr.FromString("101%")), wantError: "maxUnavailable"},
		{name: "zero child still validates formats", replicas: ptr.To[int32](0), surge: ptr.To(intstr.FromString("bad")), wantError: "maxSurge"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			child := disaggv1.DisaggregatedSubRoleSpec{Name: "hot", Replicas: tc.replicas, MaxSurge: tc.surge, MaxUnavailable: tc.unavailable}
			if tc.external {
				child.Scaling = &disaggv1.RoleScaling{Mode: disaggv1.RoleScalingExternal}
			}
			obj := &disaggv1.DisaggregatedSet{ObjectMeta: metav1.ObjectMeta{Name: "test"}}
			obj.Spec.Roles = []disaggv1.DisaggregatedRoleSpec{{
				Name:     "model",
				SubRoles: []disaggv1.DisaggregatedSubRoleSpec{child, {Name: "cold", Replicas: ptr.To[int32](3)}},
			}}
			obj.Spec.Roles[0].Spec.RolloutStrategy.RollingUpdateConfiguration = tc.parent
			oldObj := obj.DeepCopy()
			oldObj.Spec.Roles[0].SubRoles[0].MaxSurge = &one
			oldObj.Spec.Roles[0].SubRoles[0].MaxUnavailable = &zero
			webhook := &DisaggregatedSetWebhook{}
			_, createErr := webhook.ValidateCreate(context.Background(), obj)
			_, updateErr := webhook.ValidateUpdate(context.Background(), oldObj, obj)
			for _, err := range []error{createErr, updateErr} {
				if tc.wantError != "" {
					require.ErrorContains(t, err, "spec.roles[0].subRoles[0]."+tc.wantError)
				} else {
					require.NoError(t, err)
				}
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

	t.Run("role annotations preserve legacy templates", func(t *testing.T) {
		subgroupKey := leaderworkerset.SubGroupExclusiveKeyAnnotationKey
		groupKey := leaderworkerset.ExclusiveKeyAnnotationKey
		for _, tc := range []struct {
			name                        string
			oldAnnotations, annotations map[string]string
			wantError                   string
		}{
			{
				name:        "add unrelated annotation",
				annotations: map[string]string{"example.com/note": "new"},
			},
			{
				name:           "change unrelated annotation",
				oldAnnotations: map[string]string{"example.com/note": "old"},
				annotations:    map[string]string{"example.com/note": "new"},
			},
			{
				name:           "remove unrelated annotation",
				oldAnnotations: map[string]string{"example.com/note": "old"},
			},
			{
				name:           "remove invalid annotation",
				oldAnnotations: map[string]string{"invalid key": "old"},
			},
			{
				name:        "reject new invalid annotation",
				annotations: map[string]string{"invalid key": "new"},
				wantError:   "spec.roles[0].metadata.annotations",
			},
			{
				name:           "preserve unchanged subgroup annotation",
				oldAnnotations: map[string]string{subgroupKey: "rack"},
				annotations:    map[string]string{subgroupKey: "rack", "example.com/note": "new"},
			},
			{
				name:           "revalidate changed subgroup annotation",
				oldAnnotations: map[string]string{subgroupKey: "rack"},
				annotations:    map[string]string{subgroupKey: "zone"},
				wantError:      "spec.roles[0].spec.leaderWorkerTemplate.subGroupPolicy.subGroupSize",
			},
			{
				name:        "revalidate added empty subgroup annotation",
				annotations: map[string]string{subgroupKey: ""},
				wantError:   "spec.roles[0].spec.leaderWorkerTemplate.subGroupPolicy.subGroupSize",
			},
			{
				name:           "revalidate removed empty subgroup annotation",
				oldAnnotations: map[string]string{subgroupKey: ""},
				wantError:      "spec.roles[0].spec.leaderWorkerTemplate.subGroupPolicy.subGroupSize",
			},
			{
				name:           "preserve unchanged exclusive-topology annotation",
				oldAnnotations: map[string]string{groupKey: "rack"},
				annotations:    map[string]string{groupKey: "rack", "example.com/note": "new"},
			},
			{
				name:           "revalidate changed exclusive-topology annotation",
				oldAnnotations: map[string]string{groupKey: "rack"},
				annotations:    map[string]string{groupKey: "zone"},
				wantError:      "spec.roles[0].spec.leaderWorkerTemplate.subGroupPolicy.subGroupSize",
			},
		} {
			t.Run(tc.name, func(t *testing.T) {
				oldObj := validObj.DeepCopy()
				role := &oldObj.Spec.Roles[0]
				role.Annotations = tc.oldAnnotations
				role.Spec.LeaderWorkerTemplate.Size = ptr.To[int32](5)
				role.Spec.LeaderWorkerTemplate.SubGroupPolicy = &leaderworkerset.SubGroupPolicy{SubGroupSize: ptr.To[int32](3)}
				updated := oldObj.DeepCopy()
				updated.Spec.Roles[0].Annotations = tc.annotations
				original, before := oldObj.DeepCopy(), updated.DeepCopy()
				_, err := webhook.ValidateUpdate(ctx, oldObj, updated)
				require.Equal(t, original, oldObj)
				require.Equal(t, before, updated)
				if tc.wantError != "" {
					require.ErrorContains(t, err, tc.wantError)
				} else {
					require.NoError(t, err)
				}
			})
		}
	})

	t.Run("sub-role scaling semantics", func(t *testing.T) {
		for _, tc := range []struct {
			name      string
			old, new  *disaggv1.RoleScaling
			wantError bool
		}{
			{name: "omitted to Static", new: &disaggv1.RoleScaling{Mode: disaggv1.RoleScalingStatic}},
			{name: "Static to omitted", old: &disaggv1.RoleScaling{Mode: disaggv1.RoleScalingStatic}},
			{name: "empty to omitted", old: &disaggv1.RoleScaling{}},
			{name: "omitted to External", new: &disaggv1.RoleScaling{Mode: disaggv1.RoleScalingExternal}, wantError: true},
			{name: "External to omitted", old: &disaggv1.RoleScaling{Mode: disaggv1.RoleScalingExternal}, wantError: true},
		} {
			t.Run(tc.name, func(t *testing.T) {
				oldObj := validObj.DeepCopy()
				role := &oldObj.Spec.Roles[0]
				role.SubRoles = []disaggv1.DisaggregatedSubRoleSpec{{Name: "hot", Scaling: tc.old}, {Name: "cold"}}
				role.Spec.LeaderWorkerTemplate.Size = ptr.To[int32](5)
				role.Spec.LeaderWorkerTemplate.SubGroupPolicy = &leaderworkerset.SubGroupPolicy{SubGroupSize: ptr.To[int32](3)}
				updated := oldObj.DeepCopy()
				updated.Spec.Roles[0].SubRoles[0].Scaling = tc.new
				original, before := oldObj.DeepCopy(), updated.DeepCopy()
				_, err := webhook.ValidateUpdate(ctx, oldObj, updated)
				require.Equal(t, original, oldObj)
				require.Equal(t, before, updated)
				if tc.wantError {
					require.ErrorContains(t, err, "subGroupPolicy.subGroupSize")
				} else {
					require.NoError(t, err)
				}
			})
		}
	})

	t.Run("sub-role replica changes", func(t *testing.T) {
		for _, tc := range []struct {
			name        string
			oldReplicas [2]*int32
			replicas    [2]*int32
			wantError   bool
		}{
			{
				name:        "partial scale to zero invalidates parent budget",
				oldReplicas: [2]*int32{ptr.To[int32](1), ptr.To[int32](1)},
				replicas:    [2]*int32{ptr.To[int32](0), ptr.To[int32](1)},
				wantError:   true,
			},
			{
				name:      "partial scale to zero uses defaulted targets",
				replicas:  [2]*int32{ptr.To[int32](0), nil},
				wantError: true,
			},
			{
				name:        "partial scale to zero preserves valid parent budget",
				oldReplicas: [2]*int32{ptr.To[int32](1), ptr.To[int32](2)},
				replicas:    [2]*int32{ptr.To[int32](0), ptr.To[int32](2)},
			},
			{
				name:        "all sub-roles scale to zero",
				oldReplicas: [2]*int32{ptr.To[int32](1), ptr.To[int32](1)},
				replicas:    [2]*int32{ptr.To[int32](0), ptr.To[int32](0)},
			},
			{
				name:     "defaulted targets are unchanged",
				replicas: [2]*int32{ptr.To[int32](1), ptr.To[int32](1)},
			},
		} {
			t.Run(tc.name, func(t *testing.T) {
				oldObj := validObj.DeepCopy()
				role := &oldObj.Spec.Roles[0]
				role.SubRoles = []disaggv1.DisaggregatedSubRoleSpec{
					{Name: "hot", Replicas: tc.oldReplicas[0]},
					{Name: "cold", Replicas: tc.oldReplicas[1]},
				}
				role.Spec.RolloutStrategy.RollingUpdateConfiguration = &leaderworkerset.RollingUpdateConfiguration{
					MaxUnavailable: intstr.FromString("50%"),
				}
				_, err := webhook.ValidateCreate(ctx, oldObj)
				require.NoError(t, err)
				updated := oldObj.DeepCopy()
				for i := range updated.Spec.Roles[0].SubRoles {
					updated.Spec.Roles[0].SubRoles[i].Replicas = tc.replicas[i]
				}
				original, before := oldObj.DeepCopy(), updated.DeepCopy()
				_, err = webhook.ValidateUpdate(ctx, oldObj, updated)
				require.Equal(t, original, oldObj)
				require.Equal(t, before, updated)
				if tc.wantError {
					require.ErrorContains(t, err, "spec.roles[0].spec.rolloutStrategy.rollingUpdateConfiguration.maxUnavailable")
				} else {
					require.NoError(t, err)
				}
			})
		}
	})

	t.Run("placement policy topology update", func(t *testing.T) {
		for _, tc := range []struct {
			name      string
			oldPolicy *disaggv1.PlacementPolicy
			newPolicy *disaggv1.PlacementPolicy
			wantError string
		}{
			{
				name: "unchanged invalid topology key is accepted",
				oldPolicy: &disaggv1.PlacementPolicy{
					Type:     disaggv1.PlacementExclusiveSlice,
					Topology: "invalid topology key",
				},
				newPolicy: &disaggv1.PlacementPolicy{
					Type:     disaggv1.PlacementExclusiveSlice,
					Topology: "invalid topology key",
				},
			},
			{
				name: "changed invalid topology key is rejected",
				oldPolicy: &disaggv1.PlacementPolicy{
					Type:     disaggv1.PlacementExclusiveSlice,
					Topology: "invalid topology key",
				},
				newPolicy: &disaggv1.PlacementPolicy{
					Type:     disaggv1.PlacementExclusiveSlice,
					Topology: "another invalid key",
				},
				wantError: "spec.placementPolicy.topology",
			},
			{
				name: "newly added invalid topology key is rejected",
				newPolicy: &disaggv1.PlacementPolicy{
					Type:     disaggv1.PlacementExclusiveSlice,
					Topology: "invalid topology key",
				},
				wantError: "spec.placementPolicy.topology",
			},
			{
				name: "updating to valid topology key is accepted",
				oldPolicy: &disaggv1.PlacementPolicy{
					Type:     disaggv1.PlacementExclusiveSlice,
					Topology: "invalid topology key",
				},
				newPolicy: &disaggv1.PlacementPolicy{
					Type:     disaggv1.PlacementExclusiveSlice,
					Topology: "kubernetes.io/hostname",
				},
			},
		} {
			t.Run(tc.name, func(t *testing.T) {
				oldObj := validObj.DeepCopy()
				oldObj.Spec.PlacementPolicy = tc.oldPolicy
				newObj := validObj.DeepCopy()
				newObj.Spec.PlacementPolicy = tc.newPolicy
				original, before := oldObj.DeepCopy(), newObj.DeepCopy()
				_, err := webhook.ValidateUpdate(ctx, oldObj, newObj)
				require.Equal(t, original, oldObj)
				require.Equal(t, before, newObj)
				if tc.wantError != "" {
					require.ErrorContains(t, err, tc.wantError)
				} else {
					require.NoError(t, err)
				}
			})
		}
	})

	t.Run("finalizer removal with unchanged invalid topology key", func(t *testing.T) {
		oldObj := validObj.DeepCopy()
		oldObj.Finalizers = []string{"example.com/cleanup"}
		now := metav1.Now()
		oldObj.DeletionTimestamp = &now
		oldObj.Spec.PlacementPolicy = &disaggv1.PlacementPolicy{
			Type:     disaggv1.PlacementExclusiveSlice,
			Topology: "invalid topology key",
		}
		newObj := oldObj.DeepCopy()
		newObj.Finalizers = nil
		_, err := webhook.ValidateUpdate(ctx, oldObj, newObj)
		require.NoError(t, err)
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

	for _, subRoles := range []bool{false, true} {
		t.Run(fmt.Sprintf("finalizer cleanup preserves legacy templates subRoles=%t", subRoles), func(t *testing.T) {
			oldObj := wrappers.BuildDisaggregatedSet("legacy", "default").WithRole("prefill", 4, "nginx").Obj()
			oldObj.Finalizers = []string{"example.com/cleanup"}
			now := metav1.Now()
			oldObj.DeletionTimestamp = &now
			role := &oldObj.Spec.Roles[0]
			external := &disaggv1.RoleScaling{Mode: disaggv1.RoleScalingExternal}
			if subRoles {
				role.SubRoles = []disaggv1.DisaggregatedSubRoleSpec{{Name: "hot"}, {Name: "cold", Scaling: external}}
			} else {
				role.Scaling = external
			}
			role.Spec.LeaderWorkerTemplate.Size = ptr.To[int32](5)
			role.Spec.LeaderWorkerTemplate.SubGroupPolicy = &leaderworkerset.SubGroupPolicy{SubGroupSize: ptr.To[int32](3)}
			updated := oldObj.DeepCopy()
			updated.Finalizers = nil
			_, err := webhook.ValidateDelete(ctx, oldObj)
			require.NoError(t, err)
			_, err = webhook.ValidateUpdate(ctx, oldObj, updated)
			require.NoError(t, err)
			require.Equal(t, oldObj.Spec, updated.Spec)

			updated.Spec.Roles[0].Spec.LeaderWorkerTemplate.WorkerTemplate.Spec.Containers[0].Image = "nginx:1.27"
			_, err = webhook.ValidateUpdate(ctx, oldObj, updated)
			require.ErrorContains(t, err, "subGroupPolicy.subGroupSize")
		})
	}
}

func TestValidateExternalScalingRules(t *testing.T) {
	webhook := &DisaggregatedSetWebhook{}
	ctx := context.Background()
	external := &disaggv1.RoleScaling{Mode: disaggv1.RoleScalingExternal}

	t.Run("preserves budget validation when switching Static to External", func(t *testing.T) {
		oldObj := wrappers.BuildDisaggregatedSet("test", "default").WithRole("prefill", 4, "nginx").Obj()
		oldObj.Spec.Roles[0].Spec.RolloutStrategy.RollingUpdateConfiguration = &leaderworkerset.RollingUpdateConfiguration{
			MaxUnavailable: intstr.FromString("50%"),
		}
		_, err := webhook.ValidateCreate(ctx, oldObj)
		require.NoError(t, err)
		updated := oldObj.DeepCopy()
		updated.Spec.Roles[0].Scaling = external
		original, before := oldObj.DeepCopy(), updated.DeepCopy()
		_, err = webhook.ValidateUpdate(ctx, oldObj, updated)
		require.NoError(t, err)
		require.Equal(t, original, oldObj)
		require.Equal(t, before, updated)

		updated.Spec.Roles[0].Spec.RolloutStrategy.RollingUpdateConfiguration.MaxUnavailable = intstr.FromInt32(0)
		_, err = webhook.ValidateUpdate(ctx, oldObj, updated)
		require.ErrorContains(t, err, "spec.roles[0].spec.rolloutStrategy.rollingUpdateConfiguration.maxUnavailable")
	})

	t.Run("validates External budgets without a replica count", func(t *testing.T) {
		zero, zeroPercent, quarter := intstr.FromInt32(0), intstr.FromString("0%"), intstr.FromString("25%")
		for _, tc := range []struct {
			name        string
			replicas    int32
			external    bool
			unavailable intstr.IntOrString
			wantError   bool
		}{
			{name: "External percentage that rounds to zero at its replicas", replicas: 1, external: true, unavailable: quarter},
			{name: "External percentage with replicas above one", replicas: 3, external: true, unavailable: quarter},
			{name: "External zero percentage", replicas: 1, external: true, unavailable: zeroPercent, wantError: true},
			{name: "External zero integer", replicas: 3, external: true, unavailable: zero, wantError: true},
			{name: "Static percentage that rounds to zero", replicas: 3, unavailable: quarter, wantError: true},
			{name: "Static percentage that resolves to one", replicas: 4, unavailable: quarter},
		} {
			t.Run(tc.name, func(t *testing.T) {
				obj := wrappers.BuildDisaggregatedSet("test", "default").
					WithRole("prefill", 1, "nginx").
					WithRole("decode", 1, "nginx").Obj()
				for i := range obj.Spec.Roles {
					role := &obj.Spec.Roles[i]
					role.Spec.Replicas = ptr.To(tc.replicas)
					role.Spec.RolloutStrategy.RollingUpdateConfiguration = &leaderworkerset.RollingUpdateConfiguration{
						MaxSurge:       zero,
						MaxUnavailable: tc.unavailable,
					}
					if tc.external {
						role.Scaling = external
					}
				}
				oldObj := obj.DeepCopy()
				for i := range oldObj.Spec.Roles {
					oldObj.Spec.Roles[i].Spec.RolloutStrategy.RollingUpdateConfiguration.MaxUnavailable = intstr.FromInt32(1)
				}
				_, createErr := webhook.ValidateCreate(ctx, obj)
				_, updateErr := webhook.ValidateUpdate(ctx, oldObj, obj)
				for _, err := range []error{createErr, updateErr} {
					if tc.wantError {
						require.ErrorContains(t, err, "spec.roles[0].spec.rolloutStrategy.rollingUpdateConfiguration.maxUnavailable")
					} else {
						require.NoError(t, err)
					}
				}
			})
		}
	})

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

	buildDisaggregatedSet := func(spec leaderworkerset.LeaderWorkerSetSpec) *disaggv1.DisaggregatedSet {
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
			obj: buildDisaggregatedSet(leaderworkerset.LeaderWorkerSetSpec{
				Replicas:      ptr.To(int32(2)),
				GroupIdentity: leaderworkerset.GroupIdentityHash,
			}),
			expectError: false,
		},
		{
			name: "hash role with subGroupPolicy is accepted",
			obj: buildDisaggregatedSet(leaderworkerset.LeaderWorkerSetSpec{
				Replicas:      ptr.To(int32(2)),
				GroupIdentity: leaderworkerset.GroupIdentityHash,
				LeaderWorkerTemplate: leaderworkerset.LeaderWorkerTemplate{
					Size: ptr.To[int32](2),
					SubGroupPolicy: &leaderworkerset.SubGroupPolicy{
						SubGroupSize: ptr.To(int32(2)),
					},
				},
			}),
			expectError: false,
		},
		{
			name: "hash role with volumeClaimTemplates is rejected",
			obj: buildDisaggregatedSet(leaderworkerset.LeaderWorkerSetSpec{
				Replicas:      ptr.To(int32(2)),
				GroupIdentity: leaderworkerset.GroupIdentityHash,
				LeaderWorkerTemplate: leaderworkerset.LeaderWorkerTemplate{
					VolumeClaimTemplates: []corev1.PersistentVolumeClaim{{}},
				},
			}),
			expectError: true,
			errorMsg:    "volumeClaimTemplates are not supported with groupIdentity Hash",
		},
		{
			name: "hash role with maxGroupRestarts is accepted",
			obj: buildDisaggregatedSet(leaderworkerset.LeaderWorkerSetSpec{
				Replicas:      ptr.To(int32(2)),
				GroupIdentity: leaderworkerset.GroupIdentityHash,
				LeaderWorkerTemplate: leaderworkerset.LeaderWorkerTemplate{
					MaxGroupRestarts: ptr.To(int32(1)),
				},
			}),
			expectError: false,
		},
		{
			name: "hash role with maxGroupRestarts and NoneRestartPolicy is rejected",
			obj: buildDisaggregatedSet(leaderworkerset.LeaderWorkerSetSpec{
				Replicas:      ptr.To(int32(2)),
				GroupIdentity: leaderworkerset.GroupIdentityHash,
				LeaderWorkerTemplate: leaderworkerset.LeaderWorkerTemplate{
					MaxGroupRestarts: ptr.To(int32(1)),
					RestartPolicy:    leaderworkerset.NoneRestartPolicy,
				},
			}),
			expectError: true,
			errorMsg:    "maxGroupRestarts is only supported when restartPolicy recreates the group",
		},
		{
			name: "hash role with scheduling is accepted",
			obj: buildDisaggregatedSet(leaderworkerset.LeaderWorkerSetSpec{
				Replicas:      ptr.To(int32(2)),
				GroupIdentity: leaderworkerset.GroupIdentityHash,
				Scheduling:    &leaderworkerset.LeaderWorkerSetScheduling{},
			}),
			expectError: false,
		},
		{
			name: "ordinal role with subGroupPolicy is accepted",
			obj: buildDisaggregatedSet(leaderworkerset.LeaderWorkerSetSpec{
				Replicas:      ptr.To(int32(2)),
				GroupIdentity: leaderworkerset.GroupIdentityOrdinal,
				LeaderWorkerTemplate: leaderworkerset.LeaderWorkerTemplate{
					Size: ptr.To[int32](2),
					SubGroupPolicy: &leaderworkerset.SubGroupPolicy{
						SubGroupSize: ptr.To(int32(2)),
					},
				},
			}),
			expectError: false,
		},
		{
			name: "hash role with restartBackoff is accepted",
			obj: buildDisaggregatedSet(leaderworkerset.LeaderWorkerSetSpec{
				Replicas:      ptr.To(int32(2)),
				GroupIdentity: leaderworkerset.GroupIdentityHash,
				LeaderWorkerTemplate: leaderworkerset.LeaderWorkerTemplate{
					RestartBackoff: &leaderworkerset.RestartBackoff{
						BaseSeconds: ptr.To(int32(10)),
						CapSeconds:  ptr.To(int32(300)),
					},
				},
			}),
			expectError: false,
		},
		{
			name: "hash role with restartBackoff and NoneRestartPolicy is rejected",
			obj: buildDisaggregatedSet(leaderworkerset.LeaderWorkerSetSpec{
				Replicas:      ptr.To(int32(2)),
				GroupIdentity: leaderworkerset.GroupIdentityHash,
				LeaderWorkerTemplate: leaderworkerset.LeaderWorkerTemplate{
					RestartBackoff: &leaderworkerset.RestartBackoff{
						BaseSeconds: ptr.To(int32(10)),
						CapSeconds:  ptr.To(int32(300)),
					},
					RestartPolicy: leaderworkerset.NoneRestartPolicy,
				},
			}),
			expectError: true,
			errorMsg:    "restartBackoff is only supported when restartPolicy recreates the group",
		},
		{
			name: "role with invalid restartBackoff base > cap is rejected",
			obj: buildDisaggregatedSet(leaderworkerset.LeaderWorkerSetSpec{
				Replicas: ptr.To(int32(2)),
				LeaderWorkerTemplate: leaderworkerset.LeaderWorkerTemplate{
					RestartBackoff: &leaderworkerset.RestartBackoff{
						BaseSeconds: ptr.To(int32(600)),
						CapSeconds:  ptr.To(int32(300)),
					},
				},
			}),
			expectError: true,
			errorMsg:    "baseSeconds must not be greater than capSeconds",
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

func TestValidateCreatePodTerminationPolicy(t *testing.T) {
	webhook := &DisaggregatedSetWebhook{}
	ctx := context.Background()

	buildDisaggregatedSet := func(policy leaderworkerset.PodTerminationPolicyType) *disaggv1.DisaggregatedSet {
		return &disaggv1.DisaggregatedSet{
			ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
			Spec: disaggv1.DisaggregatedSetSpec{
				Roles: []disaggv1.DisaggregatedRoleSpec{
					{
						Name: "prefill",
						LeaderWorkerSetTemplateSpec: leaderworkerset.LeaderWorkerSetTemplateSpec{
							Spec: leaderworkerset.LeaderWorkerSetSpec{
								PodTerminationPolicy: policy,
							},
						},
					},
				},
			},
		}
	}

	tests := []struct {
		name        string
		policy      leaderworkerset.PodTerminationPolicyType
		expectError bool
		errorMsg    string
	}{
		{
			name:        "empty policy is accepted",
			policy:      "",
			expectError: false,
		},
		{
			name:        "Sequential policy is accepted",
			policy:      leaderworkerset.SequentialPodTerminationPolicy,
			expectError: false,
		},
		{
			name:        "Parallel policy is accepted",
			policy:      leaderworkerset.ParallelPodTerminationPolicy,
			expectError: false,
		},
		{
			name:        "invalid policy is rejected",
			policy:      "InvalidPolicy",
			expectError: true,
			errorMsg:    "Unsupported value",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := webhook.ValidateCreate(ctx, buildDisaggregatedSet(tc.policy))
			if tc.expectError {
				require.Error(t, err)
				require.Contains(t, err.Error(), tc.errorMsg)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestValidatePlacementWithRolePlacement(t *testing.T) {
	const sliceTopology = "cloud.google.com/gke-nodepool"
	tests := []struct {
		name        string
		annotations map[string]string
		policy      *leaderworkerset.PlacementPolicy
		subGroups   bool
		errorMsg    string
	}{
		{
			name:     "group placementPolicy on the slice topology",
			policy:   &leaderworkerset.PlacementPolicy{Type: leaderworkerset.PlacementExclusiveTopology, Topology: sliceTopology},
			errorMsg: "spec.roles[0].spec.placementPolicy.topology: Forbidden",
		},
		{
			name: "subgroup placementPolicy on the slice topology",
			policy: &leaderworkerset.PlacementPolicy{Type: leaderworkerset.PlacementNone,
				SubGroup: &leaderworkerset.SubGroupPlacementPolicy{Type: leaderworkerset.PlacementExclusiveTopology, Topology: sliceTopology}},
			subGroups: true,
			errorMsg:  "spec.roles[0].spec.placementPolicy.subGroup.topology: Forbidden",
		},
		{
			name:   "group placementPolicy on a finer topology",
			policy: &leaderworkerset.PlacementPolicy{Type: leaderworkerset.PlacementExclusiveTopology, Topology: "kubernetes.io/hostname"},
		},
		{
			name: "subgroup placementPolicy on a finer topology",
			policy: &leaderworkerset.PlacementPolicy{Type: leaderworkerset.PlacementNone,
				SubGroup: &leaderworkerset.SubGroupPlacementPolicy{Type: leaderworkerset.PlacementExclusiveTopology, Topology: "kubernetes.io/hostname"}},
			subGroups: true,
		},
		{
			name:        "exclusive-topology annotation on a finer topology",
			annotations: map[string]string{leaderworkerset.ExclusiveKeyAnnotationKey: "kubernetes.io/hostname"},
		},
		{
			name:        "subgroup-exclusive-topology annotation on a finer topology",
			annotations: map[string]string{leaderworkerset.SubGroupExclusiveKeyAnnotationKey: "kubernetes.io/hostname"},
			subGroups:   true,
		},
		{
			name:   "placementPolicy None",
			policy: &leaderworkerset.PlacementPolicy{Type: leaderworkerset.PlacementNone},
		},
	}

	webhook := &DisaggregatedSetWebhook{}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			spec := leaderworkerset.LeaderWorkerSetSpec{Replicas: ptr.To(int32(2)), PlacementPolicy: tt.policy}
			if tt.subGroups {
				spec.LeaderWorkerTemplate.SubGroupPolicy = &leaderworkerset.SubGroupPolicy{SubGroupSize: ptr.To(int32(1))}
			}
			obj := &disaggv1.DisaggregatedSet{
				ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
				Spec: disaggv1.DisaggregatedSetSpec{
					PlacementPolicy: &disaggv1.PlacementPolicy{
						Type:     disaggv1.PlacementExclusiveSlice,
						Topology: sliceTopology,
					},
					Roles: []disaggv1.DisaggregatedRoleSpec{
						{Name: "prefill", LeaderWorkerSetTemplateSpec: leaderworkerset.LeaderWorkerSetTemplateSpec{
							ObjectMeta: metav1.ObjectMeta{Annotations: tt.annotations}, Spec: spec}},
						{Name: "decode", LeaderWorkerSetTemplateSpec: leaderworkerset.LeaderWorkerSetTemplateSpec{Spec: leaderworkerset.LeaderWorkerSetSpec{Replicas: ptr.To(int32(2))}}},
					},
				},
			}
			_, err := webhook.ValidateCreate(context.Background(), obj)
			if tt.errorMsg == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			require.Contains(t, err.Error(), tt.errorMsg)
		})
	}
}

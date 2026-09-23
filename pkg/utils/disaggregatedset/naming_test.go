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
	"testing"

	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/utils/ptr"

	disaggregatedsetv1 "sigs.k8s.io/lws/api/disaggregatedset/v1"
	leaderworkersetv1 "sigs.k8s.io/lws/api/leaderworkerset/v1"
)

func TestGenerateName(t *testing.T) {
	assert.Equal(t, "ds-0-abcd1234-prefill", GenerateName("ds", 0, "abcd1234", testUtilsRolePrefill))
	assert.Equal(t, "ds-3-abcd1234-decode", GenerateName("ds", 3, "abcd1234", testUtilsRoleDecode))
}

func TestGenerateLegacyName(t *testing.T) {
	// The legacy name carries no slice segment; it identifies objects created
	// before the slices feature, which are adopted as slice 0.
	assert.Equal(t, "ds-abcd1234-prefill", GenerateLegacyName("ds", "abcd1234", testUtilsRolePrefill))
	assert.NotEqual(t, GenerateName("ds", 0, "abcd1234", testUtilsRolePrefill), GenerateLegacyName("ds", "abcd1234", testUtilsRolePrefill))
}

func TestGenerateLabels(t *testing.T) {
	labels := GenerateLabels("ds", 2, "abcd1234", testUtilsRoleDecode)

	assert.Equal(t, map[string]string{
		"app":                               "ds-2-decode",
		disaggregatedsetv1.RoleLabelKey:     testUtilsRoleDecode,
		disaggregatedsetv1.SliceLabelKey:    "2",
		disaggregatedsetv1.SetNameLabelKey:  "ds",
		disaggregatedsetv1.RevisionLabelKey: "abcd1234",
	}, labels)

	// Generated labels must satisfy the slice matcher used to adopt objects.
	assert.True(t, SliceLabelMatches(labels, 2))
	assert.False(t, SliceLabelMatches(labels, 1))
	assert.True(t, HasSliceLabel(labels))
}

func roleSpec(name string, image string) disaggregatedsetv1.DisaggregatedRoleSpec {
	return disaggregatedsetv1.DisaggregatedRoleSpec{
		Name: name,
		LeaderWorkerSetTemplateSpec: leaderworkersetv1.LeaderWorkerSetTemplateSpec{
			Spec: leaderworkersetv1.LeaderWorkerSetSpec{
				LeaderWorkerTemplate: leaderworkersetv1.LeaderWorkerTemplate{
					Size: ptr.To[int32](2),
					WorkerTemplate: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{{Name: "main", Image: image}},
						},
					},
				},
			},
		},
	}
}

func TestComputeRevision(t *testing.T) {
	roles := []disaggregatedsetv1.DisaggregatedRoleSpec{
		roleSpec(testUtilsRolePrefill, "image:v1"),
		roleSpec(testUtilsRoleDecode, "image:v1"),
	}

	revision := ComputeRevision(roles)
	assert.Len(t, revision, revisionLength)
	assert.Equal(t, revision, ComputeRevision(roles), "revision must be deterministic")

	t.Run("template changes produce a new revision", func(t *testing.T) {
		changed := []disaggregatedsetv1.DisaggregatedRoleSpec{
			roleSpec(testUtilsRolePrefill, "image:v2"),
			roleSpec(testUtilsRoleDecode, "image:v1"),
		}
		assert.NotEqual(t, revision, ComputeRevision(changed))
	})

	t.Run("role order is part of the revision", func(t *testing.T) {
		reordered := []disaggregatedsetv1.DisaggregatedRoleSpec{
			roleSpec(testUtilsRoleDecode, "image:v1"),
			roleSpec(testUtilsRolePrefill, "image:v1"),
		}
		assert.NotEqual(t, revision, ComputeRevision(reordered))
	})

	t.Run("an empty and a defaulted Ordinal groupIdentity hash the same", func(t *testing.T) {
		// Objects persisted before the field existed must keep their revision
		// once the API server starts defaulting it on reads.
		defaulted := []disaggregatedsetv1.DisaggregatedRoleSpec{
			roleSpec(testUtilsRolePrefill, "image:v1"),
			roleSpec(testUtilsRoleDecode, "image:v1"),
		}
		for i := range defaulted {
			defaulted[i].Spec.GroupIdentity = leaderworkersetv1.GroupIdentityOrdinal
		}
		assert.Equal(t, revision, ComputeRevision(defaulted))
	})

	t.Run("hash groupIdentity produces a different revision", func(t *testing.T) {
		hashed := []disaggregatedsetv1.DisaggregatedRoleSpec{
			roleSpec(testUtilsRolePrefill, "image:v1"),
			roleSpec(testUtilsRoleDecode, "image:v1"),
		}
		for i := range hashed {
			hashed[i].Spec.GroupIdentity = leaderworkersetv1.GroupIdentityHash
		}
		assert.NotEqual(t, revision, ComputeRevision(hashed))
	})

	t.Run("no roles", func(t *testing.T) {
		assert.Len(t, ComputeRevision(nil), revisionLength)
	})
}

func TestGetRoleConfigs(t *testing.T) {
	disaggregatedSet := &disaggregatedsetv1.DisaggregatedSet{
		Spec: disaggregatedsetv1.DisaggregatedSetSpec{
			Roles: []disaggregatedsetv1.DisaggregatedRoleSpec{
				roleSpec(testUtilsRolePrefill, "image:v1"),
				roleSpec(testUtilsRoleDecode, "image:v2"),
			},
		},
	}

	configs := GetRoleConfigs(disaggregatedSet)
	assert.Len(t, configs, NumRequiredRoles)
	assert.Same(t, &disaggregatedSet.Spec.Roles[0], configs[testUtilsRolePrefill])
	assert.Same(t, &disaggregatedSet.Spec.Roles[1], configs[testUtilsRoleDecode])

	t.Run("no roles", func(t *testing.T) {
		assert.Empty(t, GetRoleConfigs(&disaggregatedsetv1.DisaggregatedSet{}))
	})
}

func TestGetRoleNames(t *testing.T) {
	disaggregatedSet := &disaggregatedsetv1.DisaggregatedSet{
		Spec: disaggregatedsetv1.DisaggregatedSetSpec{
			Roles: []disaggregatedsetv1.DisaggregatedRoleSpec{
				roleSpec(testUtilsRolePrefill, "image:v1"),
				roleSpec(testUtilsRoleDecode, "image:v1"),
			},
		},
	}

	// Order follows the spec so callers can rely on a deterministic rollout order.
	assert.Equal(t, []string{testUtilsRolePrefill, testUtilsRoleDecode}, GetRoleNames(disaggregatedSet))
	assert.Empty(t, GetRoleNames(&disaggregatedsetv1.DisaggregatedSet{}))
}

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

func TestGenerateLabels(t *testing.T) {
	labels := GenerateLabels("ds", 2, "abcd1234", testUtilsRoleDecode)

	assert.Equal(t, map[string]string{
		"app":                               "ds-2-decode",
		disaggregatedsetv1.RoleLabelKey:     testUtilsRoleDecode,
		disaggregatedsetv1.SliceLabelKey:    "2",
		disaggregatedsetv1.SetNameLabelKey:  "ds",
		disaggregatedsetv1.RevisionLabelKey: "abcd1234",
	}, labels)
}

func roleSpec(name string, image string) disaggregatedsetv1.DisaggregatedRoleSpec {
	return disaggregatedsetv1.DisaggregatedRoleSpec{
		Name: name,
		LeaderWorkerSetTemplateSpec: leaderworkersetv1.LeaderWorkerSetTemplateSpec{
			Spec: leaderworkersetv1.LeaderWorkerSetSpec{
				StartupPolicy: leaderworkersetv1.LeaderCreatedStartupPolicy,
				GroupIdentity: leaderworkersetv1.GroupIdentityOrdinal,
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

	t.Run("startup policy changes produce a new revision", func(t *testing.T) {
		changed := []disaggregatedsetv1.DisaggregatedRoleSpec{
			roleSpec(testUtilsRolePrefill, "image:v1"),
			roleSpec(testUtilsRoleDecode, "image:v1"),
		}
		changed[0].Spec.StartupPolicy = leaderworkersetv1.LeaderReadyStartupPolicy
		assert.NotEqual(t, revision, ComputeRevision(changed))
	})

	t.Run("network configuration changes produce a new revision", func(t *testing.T) {
		changed := []disaggregatedsetv1.DisaggregatedRoleSpec{
			roleSpec(testUtilsRolePrefill, "image:v1"),
			roleSpec(testUtilsRoleDecode, "image:v1"),
		}
		policy := leaderworkersetv1.SubdomainUniquePerReplica
		changed[0].Spec.NetworkConfig = &leaderworkersetv1.NetworkConfig{SubdomainPolicy: &policy}
		assert.NotEqual(t, revision, ComputeRevision(changed))
	})

	t.Run("LWS metadata changes produce a new revision", func(t *testing.T) {
		withLabel := []disaggregatedsetv1.DisaggregatedRoleSpec{
			roleSpec(testUtilsRolePrefill, "image:v1"),
			roleSpec(testUtilsRoleDecode, "image:v1"),
		}
		withLabel[0].Labels = map[string]string{"queue": "gpu"}
		assert.NotEqual(t, revision, ComputeRevision(withLabel))

		withAnnotation := []disaggregatedsetv1.DisaggregatedRoleSpec{
			roleSpec(testUtilsRolePrefill, "image:v1"),
			roleSpec(testUtilsRoleDecode, "image:v1"),
		}
		withAnnotation[0].Annotations = map[string]string{"example.com/config": "enabled"}
		assert.NotEqual(t, revision, ComputeRevision(withAnnotation))
	})

	t.Run("replicas remain outside the revision", func(t *testing.T) {
		changed := []disaggregatedsetv1.DisaggregatedRoleSpec{
			roleSpec(testUtilsRolePrefill, "image:v1"),
			roleSpec(testUtilsRoleDecode, "image:v1"),
		}
		changed[0].Spec.Replicas = ptr.To[int32](10)
		assert.Equal(t, revision, ComputeRevision(changed))
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

func TestComputeRevisionV1Compatibility(t *testing.T) {
	roles := []disaggregatedsetv1.DisaggregatedRoleSpec{
		roleSpec(testUtilsRolePrefill, "image:v1"),
		roleSpec(testUtilsRoleDecode, "image:v1"),
	}

	// This value was produced by the original revision algorithm. Pinning it
	// protects the upgrade path: unversioned DisaggregatedSets must continue to
	// resolve to the revision already stored on their existing LWS objects.
	assert.Equal(t, "c51abc75", ComputeRevisionV1(roles))

	for i := range roles {
		roles[i].Spec.GroupIdentity = ""
	}
	assert.Equal(t, "c51abc75", ComputeRevisionV1(roles))
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

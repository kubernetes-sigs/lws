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

package disaggregatedset

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	disaggregatedsetv1 "sigs.k8s.io/lws/api/disaggregatedset/v1"
	leaderworkersetv1 "sigs.k8s.io/lws/api/leaderworkerset/v1"
	disaggregatedsetutils "sigs.k8s.io/lws/pkg/utils/disaggregatedset"
)

func TestManagerSyncGroupReplacementPolicy(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, leaderworkersetv1.AddToScheme(scheme))
	ds := testManagerDS("test-deployment")

	get := func(t *testing.T, c client.Client) leaderworkersetv1.GroupReplacementPolicyType {
		var got leaderworkersetv1.LeaderWorkerSet
		require.NoError(t, c.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "test-lws"}, &got))
		return got.Spec.GroupReplacementPolicy
	}

	t.Run("patches Immediate onto a PostTermination LWS", func(t *testing.T) {
		existing := buildOwnedManagerTestLWS("test-lws", 3, ds)
		existing.Spec.GroupReplacementPolicy = leaderworkersetv1.GroupReplacementPostTermination
		fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(existing).Build()

		manager := NewLeaderWorkerSetManager(fakeClient)
		require.NoError(t, manager.SyncGroupReplacementPolicy(context.Background(), existing, leaderworkersetv1.GroupReplacementImmediate))
		require.Equal(t, leaderworkersetv1.GroupReplacementImmediate, get(t, fakeClient))
	})

	t.Run("patches back to PostTermination", func(t *testing.T) {
		existing := buildOwnedManagerTestLWS("test-lws", 3, ds)
		existing.Spec.GroupReplacementPolicy = leaderworkersetv1.GroupReplacementImmediate
		fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(existing).Build()

		manager := NewLeaderWorkerSetManager(fakeClient)
		require.NoError(t, manager.SyncGroupReplacementPolicy(context.Background(), existing, leaderworkersetv1.GroupReplacementPostTermination))
		require.Equal(t, leaderworkersetv1.GroupReplacementPostTermination, get(t, fakeClient))
	})

	t.Run("empty desired means PostTermination and does not patch a defaulted LWS", func(t *testing.T) {
		existing := buildOwnedManagerTestLWS("test-lws", 3, ds)
		existing.Spec.GroupReplacementPolicy = leaderworkersetv1.GroupReplacementPostTermination
		fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(existing).Build()
		before := existing.ResourceVersion

		manager := NewLeaderWorkerSetManager(fakeClient)
		require.NoError(t, manager.SyncGroupReplacementPolicy(context.Background(), existing, ""))

		var got leaderworkersetv1.LeaderWorkerSet
		require.NoError(t, fakeClient.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "test-lws"}, &got))
		require.Equal(t, before, got.ResourceVersion, "no-op sync must not write")
	})

	t.Run("empty on both sides is a no-op", func(t *testing.T) {
		existing := buildOwnedManagerTestLWS("test-lws", 3, ds)
		fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(existing).Build()
		before := existing.ResourceVersion

		manager := NewLeaderWorkerSetManager(fakeClient)
		require.NoError(t, manager.SyncGroupReplacementPolicy(context.Background(), existing, ""))

		var got leaderworkersetv1.LeaderWorkerSet
		require.NoError(t, fakeClient.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "test-lws"}, &got))
		require.Equal(t, before, got.ResourceVersion)
	})
}

// The policy is a live knob, not a template change: flipping it must not
// produce a new DisaggregatedSet revision (which would roll every group).
func TestComputeRevisionIgnoresGroupReplacementPolicy(t *testing.T) {
	buildRoles := func(policy leaderworkersetv1.GroupReplacementPolicyType) []disaggregatedsetv1.DisaggregatedRoleSpec {
		return []disaggregatedsetv1.DisaggregatedRoleSpec{
			{
				Name: "prefill",
				LeaderWorkerSetTemplateSpec: leaderworkersetv1.LeaderWorkerSetTemplateSpec{Spec: leaderworkersetv1.LeaderWorkerSetSpec{
					GroupIdentity:          leaderworkersetv1.GroupIdentityHash,
					GroupReplacementPolicy: policy,
					LeaderWorkerTemplate: leaderworkersetv1.LeaderWorkerTemplate{
						Size: ptr.To(int32(2)),
					},
				}},
			},
		}
	}

	base := disaggregatedsetutils.ComputeRevision(buildRoles(""))
	require.Equal(t, base, disaggregatedsetutils.ComputeRevision(buildRoles(leaderworkersetv1.GroupReplacementPostTermination)))
	require.Equal(t, base, disaggregatedsetutils.ComputeRevision(buildRoles(leaderworkersetv1.GroupReplacementImmediate)))
}

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

package disaggregatedset_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	leaderworkersetv1 "sigs.k8s.io/lws/api/leaderworkerset/v1"

	disaggregatedsetv1 "sigs.k8s.io/lws/api/disaggregatedset/v1"
	controller "sigs.k8s.io/lws/pkg/controllers/disaggregatedset"
	disaggregatedsetutils "sigs.k8s.io/lws/pkg/utils/disaggregatedset"
	"sigs.k8s.io/lws/test/wrappers"
)

// A groupReplacementPolicy change on a role reaches every Hash LWS of that role
// even while a rollout is in progress, old revision included. Ordinal LWS and
// LWS of roles removed from spec are left alone.
func TestGroupReplacementPolicySyncedDuringRollout(t *testing.T) {
	ctx := context.Background()
	scheme := wrappers.DisaggregatedSetTestScheme()

	disaggregatedSet := wrappers.BuildDisaggregatedSet("grp-sync", "default").
		WithRole(testControllerRolePrefill, 2, "nginx:2.0").
		Obj()
	disaggregatedSet.Spec.Roles[0].Spec.GroupIdentity = leaderworkersetv1.GroupIdentityHash
	disaggregatedSet.Spec.Roles[0].Spec.GroupReplacementPolicy = leaderworkersetv1.GroupReplacementImmediate
	newRevision := disaggregatedsetutils.ComputeRevision(disaggregatedSet.Spec.Roles)

	withIdentity := func(lws *leaderworkersetv1.LeaderWorkerSet, identity leaderworkersetv1.GroupIdentityType) *leaderworkersetv1.LeaderWorkerSet {
		lws.Spec.GroupIdentity = identity
		lws.Spec.GroupReplacementPolicy = leaderworkersetv1.GroupReplacementPostTermination
		return lws
	}
	// A serving old revision and a partially scaled new one: a rollout in progress.
	oldHash := withIdentity(createOldLeaderWorkerSet(disaggregatedSet, testControllerRolePrefill, "oldhash0", 2), leaderworkersetv1.GroupIdentityHash)
	newHash := withIdentity(createOldLeaderWorkerSet(disaggregatedSet, testControllerRolePrefill, newRevision, 1), leaderworkersetv1.GroupIdentityHash)
	// The role moved from Ordinal to Hash, so an older revision is still Ordinal.
	oldOrdinal := withIdentity(createOldLeaderWorkerSet(disaggregatedSet, testControllerRolePrefill, "oldord00", 1), leaderworkersetv1.GroupIdentityOrdinal)
	// A role that is no longer in spec, still draining.
	removedRole := withIdentity(createOldLeaderWorkerSet(disaggregatedSet, testControllerRoleDecode, "oldhash0", 1), leaderworkersetv1.GroupIdentityHash)

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(disaggregatedSet, oldHash, newHash, oldOrdinal, removedRole).
		WithStatusSubresource(&disaggregatedsetv1.DisaggregatedSet{}, &leaderworkersetv1.LeaderWorkerSet{}).Build()
	reconciler := &controller.DisaggregatedSetReconciler{
		Client:     fakeClient,
		Scheme:     scheme,
		LWSManager: controller.NewLeaderWorkerSetManager(fakeClient),
		Record:     events.NewFakeRecorder(100),
	}

	_, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: disaggregatedSet.Name, Namespace: disaggregatedSet.Namespace}})
	require.NoError(t, err)

	policyOf := func(lws *leaderworkersetv1.LeaderWorkerSet) leaderworkersetv1.GroupReplacementPolicyType {
		got := &leaderworkersetv1.LeaderWorkerSet{}
		require.NoError(t, fakeClient.Get(ctx, client.ObjectKeyFromObject(lws), got))
		return got.Spec.GroupReplacementPolicy
	}
	assert.Equal(t, leaderworkersetv1.GroupReplacementImmediate, policyOf(oldHash), "old revision Hash LWS")
	assert.Equal(t, leaderworkersetv1.GroupReplacementImmediate, policyOf(newHash), "new revision Hash LWS")
	assert.Equal(t, leaderworkersetv1.GroupReplacementPostTermination, policyOf(oldOrdinal), "Ordinal LWS must not be synced")
	assert.Equal(t, leaderworkersetv1.GroupReplacementPostTermination, policyOf(removedRole), "LWS of a removed role must not be synced")
}

//go:build harness && !harness_legacy_scale_up

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
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	disaggv1 "sigs.k8s.io/lws/api/disaggregatedset/v1"
	leaderv1 "sigs.k8s.io/lws/api/leaderworkerset/v1"
	disaggutils "sigs.k8s.io/lws/pkg/utils/disaggregatedset"
)

func TestHarnessObserveIndependentScalingPolicies(t *testing.T) {
	for _, identity := range []leaderv1.GroupIdentityType{leaderv1.GroupIdentityOrdinal, leaderv1.GroupIdentityHash} {
		for _, up := range []disaggv1.RolloutScalingPolicy{"", disaggv1.RolloutScalingDeferred, disaggv1.RolloutScalingImmediate} {
			for _, down := range []disaggv1.RolloutScalingPolicy{disaggv1.RolloutScalingDeferred, disaggv1.RolloutScalingImmediate} {
				t.Run(fmt.Sprintf("%s/up=%s/down=%s", identity, up, down), func(t *testing.T) {
					// Demand is ten per role. Four old Ready groups and two new
					// loading groups have already been requested. Immediate may
					// request the missing four without pretending they are Ready.
					ds := newTwoRoleTestDisaggregatedSet([2]int32{10, 10}, [2]int{1, 1}, [2]int{})
					ds.Annotations = map[string]string{disaggv1.RevisionHashVersionAnnotationKey: disaggv1.RevisionHashVersion}
					ds.Spec.ScalingPolicy = &disaggv1.DisaggregatedSetScalingPolicy{ScaleUpDuringRollout: up, ScaleDownDuringRollout: down}
					for i := range ds.Spec.Roles {
						ds.Spec.Roles[i].Spec.GroupIdentity = identity
						ds.Spec.Roles[i].Spec.LeaderWorkerTemplate.Size = ptr.To[int32](1)
					}
					revision := disaggutils.ComputeRevision(ds.Spec.Roles)
					objects := []client.Object{ds}
					for _, role := range testRoleNames() {
						old := revisionLWS("old", role, 4, 4, time.Unix(1000, 0), 4)
						target := revisionLWS(revision, role, 2, 0, time.Unix(2000, 0), 10)
						old.Spec.GroupIdentity, target.Spec.GroupIdentity = identity, identity
						old.Spec.LeaderWorkerTemplate.Size, target.Spec.LeaderWorkerTemplate.Size = ptr.To[int32](1), ptr.To[int32](1)
						objects = append(objects, replicaGroupObjects(old, 4, 4)...)
						objects = append(objects, replicaGroupObjects(target, 2, 0)...)
					}
					reader := struct{ client.Reader }{newTestClient(objects...)}
					report, err := HarnessObserve(t.Context(), reader, client.ObjectKeyFromObject(ds))
					require.NoError(t, err)
					slices := report["slices"].([]map[string]any)
					require.Len(t, slices, 1)
					candidates := slices[0]["candidates"].([]map[string]any)
					require.Len(t, candidates, 1)
					candidate := candidates[0]
					state := candidate["state"].(RolloutState)
					require.Equal(t, up == disaggv1.RolloutScalingImmediate, state.ScaleUpDuringRollout)
					require.Equal(t, down == disaggv1.RolloutScalingImmediate, state.ScaleDownDuringRollout)
					require.Equal(t, RoleReplicaState{4, 4}, state.ActiveOld.ReadyReplicas)
					require.Equal(t, RoleReplicaState{0, 0}, state.Target.ReadyReplicas)
					require.Equal(t, harnessStep(ComputeNextStep(state)), candidate["preview"])
					growthLimit := 2
					if up == disaggv1.RolloutScalingImmediate {
						growthLimit = 6
						preview, ok := candidate["preview"].(map[string]any)
						require.True(t, ok)
						require.Equal(t, RoleReplicaState{6, 6}, preview["new"])
						require.Equal(t, RoleReplicaState{4, 4}, preview["past"])
					} else {
						require.Nil(t, candidate["preview"], "Deferred waits for loading groups")
					}
					for _, row := range candidate["bounds"].([]map[string]any) {
						require.Equal(t, growthLimit, row["hardGrowthLimit"])
					}
				})
			}
		}
	}
}

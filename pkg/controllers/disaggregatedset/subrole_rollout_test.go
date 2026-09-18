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
	"testing"

	"github.com/stretchr/testify/assert"
	leaderworkersetv1 "sigs.k8s.io/lws/api/leaderworkerset/v1"
)

func TestPlanSubRoleQuotasKeepsEveryTargetPositivePoolDuringRollout(t *testing.T) {
	newLWS := &leaderworkersetv1.LeaderWorkerSet{}
	newLWS.Name = "new"
	oldLWS := &leaderworkersetv1.LeaderWorkerSet{}
	oldLWS.Name = "old"
	states := []subRoleWorkloadState{
		{lws: newLWS, capacity: 1, observed: SubRoleAssignmentSummary{Replicas: map[string]int{"short": 1}}},
		{lws: oldLWS, capacity: 2, observed: SubRoleAssignmentSummary{Replicas: map[string]int{"short": 2, "long": 1}}},
	}

	got := planSubRoleQuotas(states, []string{"short", "long"}, map[string]int{"short": 2, "long": 1})
	assert.Equal(t, map[string]int{"short": 1, "long": 1}, got["old"])
	assert.Equal(t, map[string]int{"short": 1, "long": 0}, got["new"])
}

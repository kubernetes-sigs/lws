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
	"github.com/stretchr/testify/require"
)

func TestComputeNextStepDisjointGrowthUsesSharedReadyFraction(t *testing.T) {
	// Dimensions are W, P, D. New-role budgets are S=0/U=1; W's remembered
	// unavailable budget is separate. Specs alone cannot advance the next batch.
	for _, tc := range []struct {
		name                       string
		baseline, old, unavailable int
		spec, ready, target        RoleReplicaState
		want                       *UpdateStep
	}{
		{"first pair uses two released slots", 4, 4, 2,
			[]int{0, 0}, []int{0, 0}, []int{2, 2},
			&UpdateStep{Past: []int{2, 0, 0}, New: []int{0, 1, 1}}},
		{"ready Prefill waits for first Decode", 4, 2, 2,
			[]int{1, 1}, []int{1, 0}, []int{2, 2}, nil},
		{"ready Decode waits for first Prefill", 4, 2, 2,
			[]int{1, 1}, []int{0, 1}, []int{2, 2}, nil},
		{"first ready pair releases the next pair", 4, 2, 2,
			[]int{1, 1}, []int{1, 1}, []int{2, 2},
			&UpdateStep{Past: []int{0, 0, 0}, New: []int{0, 2, 2}}},
		{"larger rollout advances after first ready pair", 8, 6, 2,
			[]int{1, 1}, []int{1, 1}, []int{4, 4},
			&UpdateStep{Past: []int{4, 0, 0}, New: []int{0, 2, 2}}},
		{"second ready Prefill waits for second Decode", 8, 4, 2,
			[]int{2, 2}, []int{2, 1}, []int{4, 4}, nil},
		{"second ready pair releases the third pair", 8, 4, 2,
			[]int{2, 2}, []int{2, 2}, []int{4, 4},
			&UpdateStep{Past: []int{2, 0, 0}, New: []int{0, 3, 3}}},
		{"old U1 still releases only one slot", 4, 4, 1,
			[]int{0, 0}, []int{0, 0}, []int{2, 2},
			&UpdateStep{Past: []int{3, 0, 0}, New: []int{0, 1, 1}}},
		{"old U1 cannot complete a half-ready pair", 4, 3, 1,
			[]int{1, 1}, []int{1, 0}, []int{2, 2}, nil},
		{"unequal targets round shared ready credit down", 8, 4, 2,
			[]int{1, 2}, []int{1, 2}, []int{3, 5},
			&UpdateStep{Past: []int{4, 0, 0}, New: []int{0, 2, 2}}},
		{"API-sized targets preserve precise shared fractions", 2147483647, 715827883, 2,
			[]int{1431655764, 2}, []int{1431655764, 2}, []int{2147483647, 3},
			&UpdateStep{Past: []int{715827881, 0, 0}, New: []int{0, 1431655765, 2}}},
		{"already issued surplus is not retracted", 8, 4, 2,
			[]int{3, 2}, []int{2, 1}, []int{4, 4}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state := rolloutState(
				[]int{tc.baseline, 0, 0}, []int{tc.old, 0, 0}, []int{tc.old, 0, 0}, nil, nil,
				append([]int{0}, tc.spec...), append([]int{0}, tc.ready...), append([]int{0}, tc.target...),
				configs([]int{0, 0, 0}, []int{tc.unavailable, 1, 1}),
			)
			step := ComputeNextStep(state)
			require.Equal(t, tc.want, step)
			if step != nil {
				require.NoError(t, validateUpdateStep(state, step))
			}
		})
	}
}

func TestComputeNextStepDisjointGrowthUsesCommittedTargetReadiness(t *testing.T) {
	state := rolloutState(
		[]int{8, 0, 0}, []int{4, 0, 0}, []int{4, 0, 0}, nil, nil,
		[]int{0, 2, 2}, []int{0, 2, 1}, []int{0, 4, 4},
		configs([]int{0, 0, 0}, []int{2, 1, 1}),
	)
	state.Target.RawReadyReplicas = RoleReplicaState{0, 2, 2}
	assert.Nil(t, ComputeNextStep(state), "Decode reserved for deletion cannot release a third Prefill")
}

func TestComputeNextStepDisjointGrowthDoesNotRelaxOldBudgetForUnschedulablePair(t *testing.T) {
	state := rolloutState(
		[]int{4, 0, 0}, []int{3, 0, 0}, []int{3, 0, 0}, nil, nil,
		[]int{0, 1, 1}, []int{0, 1, 0}, []int{0, 2, 2},
		configs([]int{0, 0, 0}, []int{1, 1, 1}),
	)
	state.Target.UnschedulableRoles[2] = true
	assert.Nil(t, ComputeNextStep(state), "the growth cap does not make old U1 sufficient for a two-role pair")
}

func TestComputeNextStepDisjointGrowthKeepsSharedFractionForOverlappingCandidate(t *testing.T) {
	// The active revision shares P with the target, but parked W still makes
	// this a disjoint replacement. Target D readiness must also bound P growth.
	state := rolloutState(
		[]int{0, 1, 0}, []int{0, 1, 0}, []int{0, 1, 0}, []int{4, 0, 0}, []int{4, 0, 0},
		[]int{0, 1, 1}, []int{0, 1, 0}, []int{0, 4, 4},
		configs([]int{1, 1, 1}, []int{0, 0, 0}),
	)
	assert.Nil(t, ComputeNextStep(state))
}

func TestComputeNextStepDisjointGrowthDoesNotUseParkedTargetReadiness(t *testing.T) {
	state := rolloutState(
		[]int{8, 0, 0}, []int{4, 0, 0}, []int{4, 0, 0}, []int{0, 1, 1}, []int{0, 1, 1},
		[]int{0, 2, 2}, []int{0, 2, 1}, []int{0, 4, 4},
		configs([]int{0, 0, 0}, []int{2, 1, 1}),
	)
	assert.Nil(t, ComputeNextStep(state), "parked Decode cannot release a third target Prefill")
}

func TestComputeNextStepSameRoleGrowthKeepsPerRoleReadyCredit(t *testing.T) {
	state := rolloutState(
		[]int{4, 4}, []int{3, 3}, []int{3, 3}, nil, nil,
		[]int{1, 1}, []int{1, 0}, []int{4, 4},
		configs([]int{1, 1}, []int{0, 0}),
	)
	step := ComputeNextStep(state)
	require.Equal(t, &UpdateStep{Past: []int{3, 3}, New: []int{2, 1}}, step)
	require.NoError(t, validateUpdateStep(state, step))
}

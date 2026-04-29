/*
Copyright 2026.

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

package controller

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// step is a helper to create UpdateStep instances for tests
func step(past, new []int) UpdateStep {
	return UpdateStep{
		Past: past,
		New:  new,
	}
}

// completes checks if rollout completes correctly (old=0, new=target)
func completes(steps []UpdateStep, target []int) bool {
	if len(steps) == 0 {
		return false
	}
	last := steps[len(steps)-1]
	// Check all old replicas are 0
	for _, v := range last.Past {
		if v != 0 {
			return false
		}
	}
	// Check new replicas match target
	if len(last.New) != len(target) {
		return false
	}
	for i, v := range last.New {
		if v != target[i] {
			return false
		}
	}
	return true
}

// totalAtStep returns total replica count at a step
func totalAtStep(s UpdateStep) int {
	total := 0
	for _, v := range s.Past {
		total += v
	}
	for _, v := range s.New {
		total += v
	}
	return total
}

// stepsEqual compares two step slices for equality
func stepsEqual(a, b []UpdateStep) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !stepEqual(a[i], b[i]) {
			return false
		}
	}
	return true
}

// stepEqual compares two UpdateStep instances for equality
func stepEqual(a, b UpdateStep) bool {
	if len(a.Past) != len(b.Past) || len(a.New) != len(b.New) {
		return false
	}
	for i := range a.Past {
		if a.Past[i] != b.Past[i] {
			return false
		}
	}
	for i := range a.New {
		if a.New[i] != b.New[i] {
			return false
		}
	}
	return true
}

// =============================================================================
// Exact Step Sequence Tests
// =============================================================================

func TestComputeAllSteps_ExactSequence(t *testing.T) {
	testCases := []struct {
		name        string
		sourceRole0 int
		sourceRole1 int
		targetRole0 int
		targetRole1 int
		config      []RollingUpdateConfig
		expected    []UpdateStep
	}{
		// Small symmetric cases (decoupled: scale-up then scale-down alternately)
		{
			name:        "small_1_1_surge1",
			sourceRole0: 1, sourceRole1: 1, targetRole0: 1, targetRole1: 1,
			config: DefaultRollingUpdateConfig(2),
			expected: []UpdateStep{
				step([]int{1, 1}, []int{0, 0}),
				step([]int{1, 1}, []int{1, 1}), // scale up
				step([]int{0, 0}, []int{1, 1}), // scale down
			},
		},
		{
			name:        "small_2_2_surge1",
			sourceRole0: 2, sourceRole1: 2, targetRole0: 2, targetRole1: 2,
			config: DefaultRollingUpdateConfig(2),
			expected: []UpdateStep{
				step([]int{2, 2}, []int{0, 0}),
				step([]int{2, 2}, []int{1, 1}), // scale up (surge: 2+1 <= 2+1)
				step([]int{1, 1}, []int{1, 1}), // scale down
				step([]int{1, 1}, []int{2, 2}), // scale up (surge: 1+2 <= 2+1)
				step([]int{0, 0}, []int{2, 2}), // scale down
			},
		},
		{
			name:        "small_3_3_surge1",
			sourceRole0: 3, sourceRole1: 3, targetRole0: 3, targetRole1: 3,
			config: DefaultRollingUpdateConfig(2),
			expected: []UpdateStep{
				step([]int{3, 3}, []int{0, 0}),
				step([]int{3, 3}, []int{1, 1}), // scale up
				step([]int{2, 2}, []int{1, 1}), // scale down
				step([]int{2, 2}, []int{2, 2}), // scale up
				step([]int{1, 1}, []int{2, 2}), // scale down
				step([]int{1, 1}, []int{3, 3}), // scale up
				step([]int{0, 0}, []int{3, 3}), // scale down
			},
		},
		// Medium asymmetric cases (decoupled steps)
		{
			name:        "medium_6_2_surge1",
			sourceRole0: 6, sourceRole1: 2, targetRole0: 6, targetRole1: 2,
			config: DefaultRollingUpdateConfig(2),
			expected: []UpdateStep{
				step([]int{6, 2}, []int{0, 0}),
				step([]int{6, 2}, []int{1, 1}),
				step([]int{5, 2}, []int{1, 1}),
				step([]int{5, 2}, []int{2, 1}),
				step([]int{4, 2}, []int{2, 1}),
				step([]int{4, 2}, []int{3, 1}),
				step([]int{3, 1}, []int{3, 1}),
				step([]int{3, 1}, []int{4, 2}),
				step([]int{2, 1}, []int{4, 2}),
				step([]int{2, 1}, []int{5, 2}),
				step([]int{1, 1}, []int{5, 2}),
				step([]int{1, 1}, []int{6, 2}),
				step([]int{0, 0}, []int{6, 2}),
			},
		},
		{
			name:        "medium_6_2_surge2",
			sourceRole0: 6, sourceRole1: 2, targetRole0: 6, targetRole1: 2,
			config: []RollingUpdateConfig{{MaxSurge: 2}, {MaxSurge: 2}},
			expected: []UpdateStep{
				step([]int{6, 2}, []int{0, 0}),
				step([]int{6, 2}, []int{2, 1}),
				step([]int{4, 2}, []int{2, 1}),
				step([]int{4, 2}, []int{4, 2}),
				step([]int{2, 1}, []int{4, 2}),
				step([]int{2, 1}, []int{6, 2}),
				step([]int{0, 0}, []int{6, 2}),
			},
		},
		{
			name:        "medium_6_4_surge2",
			sourceRole0: 6, sourceRole1: 4, targetRole0: 6, targetRole1: 4,
			config: []RollingUpdateConfig{{MaxSurge: 2}, {MaxSurge: 2}},
			expected: []UpdateStep{
				step([]int{6, 4}, []int{0, 0}),
				step([]int{6, 4}, []int{2, 2}),
				step([]int{4, 3}, []int{2, 2}),
				step([]int{4, 3}, []int{4, 3}),
				step([]int{2, 2}, []int{4, 3}),
				step([]int{2, 2}, []int{6, 4}),
				step([]int{0, 0}, []int{6, 4}),
			},
		},
		// Asymmetric cases (gradual interleaved drain)
		{
			name:        "asymmetric_10_1_surge1",
			sourceRole0: 10, sourceRole1: 1, targetRole0: 10, targetRole1: 1,
			config: DefaultRollingUpdateConfig(2),
			expected: []UpdateStep{
				step([]int{10, 1}, []int{0, 0}),
				step([]int{10, 1}, []int{1, 1}),
				step([]int{9, 1}, []int{1, 1}),
				step([]int{9, 1}, []int{2, 1}),
				step([]int{8, 1}, []int{2, 1}),
				step([]int{8, 1}, []int{3, 1}),
				step([]int{7, 1}, []int{3, 1}),
				step([]int{7, 1}, []int{4, 1}),
				step([]int{6, 1}, []int{4, 1}),
				step([]int{6, 1}, []int{5, 1}),
				step([]int{5, 1}, []int{5, 1}),
				step([]int{5, 1}, []int{6, 1}),
				step([]int{4, 1}, []int{6, 1}),
				step([]int{4, 1}, []int{7, 1}),
				step([]int{3, 1}, []int{7, 1}),
				step([]int{3, 1}, []int{8, 1}),
				step([]int{2, 1}, []int{8, 1}),
				step([]int{2, 1}, []int{9, 1}),
				step([]int{1, 1}, []int{9, 1}),
				step([]int{1, 1}, []int{10, 1}),
				step([]int{0, 0}, []int{10, 1}),
			},
		},
		{
			name:        "asymmetric_1_10_surge1",
			sourceRole0: 1, sourceRole1: 10, targetRole0: 1, targetRole1: 10,
			config: DefaultRollingUpdateConfig(2),
			expected: []UpdateStep{
				step([]int{1, 10}, []int{0, 0}),
				step([]int{1, 10}, []int{1, 1}),
				step([]int{1, 9}, []int{1, 1}),
				step([]int{1, 9}, []int{1, 2}),
				step([]int{1, 8}, []int{1, 2}),
				step([]int{1, 8}, []int{1, 3}),
				step([]int{1, 7}, []int{1, 3}),
				step([]int{1, 7}, []int{1, 4}),
				step([]int{1, 6}, []int{1, 4}),
				step([]int{1, 6}, []int{1, 5}),
				step([]int{1, 5}, []int{1, 5}),
				step([]int{1, 5}, []int{1, 6}),
				step([]int{1, 4}, []int{1, 6}),
				step([]int{1, 4}, []int{1, 7}),
				step([]int{1, 3}, []int{1, 7}),
				step([]int{1, 3}, []int{1, 8}),
				step([]int{1, 2}, []int{1, 8}),
				step([]int{1, 2}, []int{1, 9}),
				step([]int{1, 1}, []int{1, 9}),
				step([]int{1, 1}, []int{1, 10}),
				step([]int{0, 0}, []int{1, 10}),
			},
		},
		// Large symmetric cases (decoupled: alternating scale-up/scale-down)
		{
			name:        "large_10_10_surge1",
			sourceRole0: 10, sourceRole1: 10, targetRole0: 10, targetRole1: 10,
			config: DefaultRollingUpdateConfig(2),
			expected: []UpdateStep{
				step([]int{10, 10}, []int{0, 0}),
				step([]int{10, 10}, []int{1, 1}), // scale up
				step([]int{9, 9}, []int{1, 1}),   // scale down
				step([]int{9, 9}, []int{2, 2}),   // scale up
				step([]int{8, 8}, []int{2, 2}),   // scale down
				step([]int{8, 8}, []int{3, 3}),
				step([]int{7, 7}, []int{3, 3}),
				step([]int{7, 7}, []int{4, 4}),
				step([]int{6, 6}, []int{4, 4}),
				step([]int{6, 6}, []int{5, 5}),
				step([]int{5, 5}, []int{5, 5}),
				step([]int{5, 5}, []int{6, 6}),
				step([]int{4, 4}, []int{6, 6}),
				step([]int{4, 4}, []int{7, 7}),
				step([]int{3, 3}, []int{7, 7}),
				step([]int{3, 3}, []int{8, 8}),
				step([]int{2, 2}, []int{8, 8}),
				step([]int{2, 2}, []int{9, 9}),
				step([]int{1, 1}, []int{9, 9}),
				step([]int{1, 1}, []int{10, 10}),
				step([]int{0, 0}, []int{10, 10}),
			},
		},
		{
			// Surge constraint: old + new <= 10 + 3 = 13
			// Interleaves scale-up and scale-down to respect surge
			// When surge blocks scale-up, drain+scale-up are combined to avoid capacity dips
			name:        "large_10_10_surge3",
			sourceRole0: 10, sourceRole1: 10, targetRole0: 10, targetRole1: 10,
			config: []RollingUpdateConfig{{MaxSurge: 3}, {MaxSurge: 3}},
			expected: []UpdateStep{
				step([]int{10, 10}, []int{0, 0}),
				step([]int{10, 10}, []int{3, 3}), // scale up (10+3=13)
				step([]int{8, 8}, []int{3, 3}),   // scale down
				step([]int{8, 8}, []int{5, 5}),   // scale up (8+5=13)
				step([]int{5, 5}, []int{8, 8}),   // drain+scale combined (5+8=13)
				step([]int{3, 3}, []int{8, 8}),   // scale down (to allow 10)
				step([]int{3, 3}, []int{10, 10}), // scale up (3+10=13)
				step([]int{0, 0}, []int{10, 10}), // final drain
			},
		},
		{
			name:        "large_12_6_surge2",
			sourceRole0: 12, sourceRole1: 6, targetRole0: 12, targetRole1: 6,
			config: []RollingUpdateConfig{{MaxSurge: 2}, {MaxSurge: 2}},
			expected: []UpdateStep{
				step([]int{12, 6}, []int{0, 0}),
				step([]int{12, 6}, []int{2, 1}),
				step([]int{10, 5}, []int{2, 1}),
				step([]int{10, 5}, []int{4, 2}),
				step([]int{8, 4}, []int{4, 2}),
				step([]int{8, 4}, []int{6, 3}),
				step([]int{6, 3}, []int{6, 3}),
				step([]int{6, 3}, []int{8, 4}),
				step([]int{4, 2}, []int{8, 4}),
				step([]int{4, 2}, []int{10, 5}),
				step([]int{2, 1}, []int{10, 5}),
				step([]int{2, 1}, []int{12, 6}),
				step([]int{0, 0}, []int{12, 6}),
			},
		},
		// Scale up/down scenarios (decoupled)
		{
			name:        "scale_up_1_1_to_3_3",
			sourceRole0: 1, sourceRole1: 1, targetRole0: 3, targetRole1: 3,
			config: DefaultRollingUpdateConfig(2),
			expected: []UpdateStep{
				step([]int{1, 1}, []int{0, 0}),
				step([]int{1, 1}, []int{1, 1}), // scale up
				step([]int{1, 1}, []int{2, 2}), // scale up (old still 1, new 2, surge ok: 1+2<=3+1)
				step([]int{1, 1}, []int{3, 3}), // scale up (new at target)
				step([]int{0, 0}, []int{3, 3}), // scale down
			},
		},
		{
			// Scale up 4→6 with surge=1: max total = 6+1 = 7
			// Interleaves scale-up and scale-down to respect surge
			// When surge blocks scale-up, drain+scale-up are combined to avoid capacity dips
			name:        "scale_up_4_4_to_6_6",
			sourceRole0: 4, sourceRole1: 4, targetRole0: 6, targetRole1: 6,
			config: DefaultRollingUpdateConfig(2),
			expected: []UpdateStep{
				step([]int{4, 4}, []int{0, 0}),
				step([]int{4, 4}, []int{1, 1}), // scale up (4+1=5)
				step([]int{4, 4}, []int{2, 2}), // scale up (4+2=6)
				step([]int{4, 4}, []int{3, 3}), // scale up (4+3=7)
				step([]int{3, 3}, []int{4, 4}), // drain+scale combined (3+4=7)
				step([]int{2, 2}, []int{5, 5}), // drain+scale combined (2+5=7)
				step([]int{1, 1}, []int{6, 6}), // drain+scale combined (1+6=7)
				step([]int{0, 0}, []int{6, 6}), // final drain
			},
		},
		{
			// Scale down 5→2: must drain to target+surge=3 before scale-up
			name:        "scale_down_5_5_to_2_2",
			sourceRole0: 5, sourceRole1: 5, targetRole0: 2, targetRole1: 2,
			config: DefaultRollingUpdateConfig(2),
			expected: []UpdateStep{
				step([]int{5, 5}, []int{0, 0}),
				step([]int{4, 4}, []int{0, 0}), // drain (5 > 2+1=3)
				step([]int{3, 3}, []int{0, 0}), // drain
				step([]int{2, 2}, []int{0, 0}), // drain to target
				step([]int{2, 2}, []int{1, 1}), // scale up (2+1=3 <= 2+1=3)
				step([]int{1, 1}, []int{1, 1}), // drain
				step([]int{1, 1}, []int{2, 2}), // scale up (new at target)
				step([]int{0, 0}, []int{2, 2}), // drain all old
			},
		},
		{
			// Mixed: role0 scales up (3→5), role1 scales down (5→3)
			// Role1 must drain to 3+1=4 before scale-up can proceed
			name:        "mixed_scale_3_5_to_5_3",
			sourceRole0: 3, sourceRole1: 5, targetRole0: 5, targetRole1: 3,
			config: DefaultRollingUpdateConfig(2),
			expected: []UpdateStep{
				step([]int{3, 5}, []int{0, 0}),
				step([]int{3, 4}, []int{0, 0}), // drain role1 (5 > 3+1=4)
				step([]int{2, 3}, []int{0, 0}), // drain both
				step([]int{2, 3}, []int{1, 1}), // scale up (2+1<=6, 3+1=4 <= 3+1=4)
				step([]int{2, 2}, []int{1, 1}), // drain role1
				step([]int{2, 2}, []int{2, 2}), // scale up
				step([]int{2, 2}, []int{3, 2}), // scale up
				step([]int{1, 1}, []int{3, 2}), // drain
				step([]int{1, 1}, []int{4, 3}), // scale up
				step([]int{1, 1}, []int{5, 3}), // scale up (new at target)
				step([]int{0, 0}, []int{5, 3}), // drain all old
			},
		},
		{
			// Asymmetric: role0 scales up (2→4), role1 scales down (4→2)
			// Role1 must drain to 2+1=3 before scale-up
			name:        "asymmetric_2_4_to_4_2",
			sourceRole0: 2, sourceRole1: 4, targetRole0: 4, targetRole1: 2,
			config: DefaultRollingUpdateConfig(2),
			expected: []UpdateStep{
				step([]int{2, 4}, []int{0, 0}),
				step([]int{2, 3}, []int{0, 0}), // drain role1 (4 > 2+1=3)
				step([]int{1, 2}, []int{0, 0}), // drain both
				step([]int{1, 2}, []int{1, 1}), // scale up (1+1<=5, 2+1=3 <= 2+1=3)
				step([]int{1, 2}, []int{2, 1}), // scale up
				step([]int{1, 1}, []int{2, 1}), // drain role1
				step([]int{1, 1}, []int{3, 2}), // scale up
				step([]int{1, 1}, []int{4, 2}), // scale up (new at target)
				step([]int{0, 0}, []int{4, 2}), // drain all old
			},
		},
		{
			// Proportional: role0 scales up (3→4), role1 scales down (5→2)
			// Role1 must drain to 2+1=3 before scale-up
			name:        "proportional_3_5_to_4_2",
			sourceRole0: 3, sourceRole1: 5, targetRole0: 4, targetRole1: 2,
			config: DefaultRollingUpdateConfig(2),
			expected: []UpdateStep{
				step([]int{3, 5}, []int{0, 0}),
				step([]int{3, 4}, []int{0, 0}), // drain role1 (5 > 2+1=3)
				step([]int{2, 3}, []int{0, 0}), // drain both
				step([]int{2, 2}, []int{0, 0}), // drain role1 to target
				step([]int{2, 2}, []int{1, 1}), // scale up (2+1<=5, 2+1=3 <= 2+1=3)
				step([]int{2, 2}, []int{2, 1}), // scale up
				step([]int{1, 1}, []int{2, 1}), // drain
				step([]int{1, 1}, []int{3, 2}), // scale up
				step([]int{1, 1}, []int{4, 2}), // scale up (new at target)
				step([]int{0, 0}, []int{4, 2}), // drain all old
			},
		},
		{
			name:        "medium_4_4_surge2",
			sourceRole0: 4, sourceRole1: 4, targetRole0: 4, targetRole1: 4,
			config: []RollingUpdateConfig{{MaxSurge: 2}, {MaxSurge: 2}},
			expected: []UpdateStep{
				step([]int{4, 4}, []int{0, 0}),
				step([]int{4, 4}, []int{2, 2}), // scale up (surge: 4+2<=4+2)
				step([]int{2, 2}, []int{2, 2}), // scale down
				step([]int{2, 2}, []int{4, 4}), // scale up (new at target)
				step([]int{0, 0}, []int{4, 4}), // scale down
			},
		},
		{
			name:        "asymmetric_surge_4_6",
			sourceRole0: 4, sourceRole1: 6, targetRole0: 4, targetRole1: 6,
			config: []RollingUpdateConfig{{MaxSurge: 2}, {MaxSurge: 3}},
			expected: []UpdateStep{
				step([]int{4, 6}, []int{0, 0}),
				step([]int{4, 6}, []int{2, 3}),
				step([]int{2, 3}, []int{2, 3}),
				step([]int{2, 3}, []int{4, 6}),
				step([]int{0, 0}, []int{4, 6}),
			},
		},
		{
			name:        "asymmetric_5_3_surge2",
			sourceRole0: 5, sourceRole1: 3, targetRole0: 5, targetRole1: 3,
			config: []RollingUpdateConfig{{MaxSurge: 2}, {MaxSurge: 2}},
			expected: []UpdateStep{
				step([]int{5, 3}, []int{0, 0}),
				step([]int{5, 3}, []int{2, 1}),
				step([]int{4, 2}, []int{2, 1}),
				step([]int{3, 2}, []int{2, 1}),
				step([]int{3, 2}, []int{4, 2}),
				step([]int{2, 1}, []int{4, 2}),
				step([]int{2, 1}, []int{5, 3}),
				step([]int{0, 0}, []int{5, 3}),
			},
		},
		// Edge cases
		{
			name:        "fresh_deploy_0_0_to_3_3",
			sourceRole0: 0, sourceRole1: 0, targetRole0: 3, targetRole1: 3,
			config: DefaultRollingUpdateConfig(2),
			expected: []UpdateStep{
				step([]int{0, 0}, []int{0, 0}),
				step([]int{0, 0}, []int{1, 1}),
				step([]int{0, 0}, []int{2, 2}),
				step([]int{0, 0}, []int{3, 3}),
			},
		},
		{
			name:        "empty_0_0_to_0_0",
			sourceRole0: 0, sourceRole1: 0, targetRole0: 0, targetRole1: 0,
			config: DefaultRollingUpdateConfig(2),
			expected: []UpdateStep{
				step([]int{0, 0}, []int{0, 0}),
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			actual := ComputeAllSteps(
				[]int{tc.sourceRole0, tc.sourceRole1},
				[]int{tc.targetRole0, tc.targetRole1},
				tc.config,
			)
			assert.True(t, stepsEqual(actual, tc.expected), "ComputeAllSteps mismatch:\ngot:  %v\nwant: %v", actual, tc.expected)
		})
	}
}

// =============================================================================
// Unavailable Strategy Tests
// =============================================================================

func TestUnavailableBasic_Symmetric4_4(t *testing.T) {
	steps := ComputeAllSteps([]int{4, 4}, []int{4, 4}, []RollingUpdateConfig{{MaxUnavailable: 1}, {MaxUnavailable: 1}})
	require.True(t, completes(steps, []int{4, 4}), "rollout should complete")

	// Verify total drops below 8 (unavailable pattern)
	minTotal := 100
	for _, s := range steps {
		if total := totalAtStep(s); total < minTotal {
			minTotal = total
		}
	}
	assert.Less(t, minTotal, 8, "total should drop below 8 for unavailable pattern")
}

func TestUnavailableBasic_StepSequence(t *testing.T) {
	steps := ComputeAllSteps([]int{4, 4}, []int{4, 4}, []RollingUpdateConfig{{MaxUnavailable: 1}, {MaxUnavailable: 1}})

	// Verify unavailable pattern: old decreases faster than new increases
	foundUnavailablePattern := false
	for i := 1; i < len(steps); i++ {
		prev := steps[i-1]
		curr := steps[i]
		oldDecreased := curr.Past[0] < prev.Past[0] || curr.Past[1] < prev.Past[1]
		newIncrease := (curr.New[0] - prev.New[0]) + (curr.New[1] - prev.New[1])
		oldDecrease := (prev.Past[0] - curr.Past[0]) + (prev.Past[1] - curr.Past[1])
		if oldDecreased && oldDecrease > newIncrease {
			foundUnavailablePattern = true
			break
		}
	}
	if !foundUnavailablePattern {
		t.Error("No unavailable pattern found")
	}
}

func TestSurgePriority_SurgeTakesPriorityOverUnavailable(t *testing.T) {
	cfg := []RollingUpdateConfig{{MaxSurge: 1, MaxUnavailable: 1}, {MaxSurge: 1, MaxUnavailable: 1}}
	steps := ComputeAllSteps([]int{4, 4}, []int{4, 4}, cfg)
	require.True(t, completes(steps, []int{4, 4}), "rollout should complete")

	// With surge > 0, total should never drop below target (8)
	for _, s := range steps {
		total := totalAtStep(s)
		assert.GreaterOrEqual(t, total, 8, "total should never drop below 8 when surge > 0")
	}
}

func TestSurgePriority_UnavailableWhenSurgeZero(t *testing.T) {
	cfg := []RollingUpdateConfig{{MaxUnavailable: 1}, {MaxUnavailable: 1}}
	steps := ComputeAllSteps([]int{4, 4}, []int{4, 4}, cfg)
	require.True(t, completes(steps, []int{4, 4}), "rollout should complete")

	// With surge=0, total should drop below target (8)
	minTotal := 100
	for _, s := range steps {
		if total := totalAtStep(s); total < minTotal {
			minTotal = total
		}
	}
	assert.Less(t, minTotal, 8, "total should drop below 8 when surge=0")
}

func TestSurgePriority_Surge2Behavior(t *testing.T) {
	cfg := []RollingUpdateConfig{{MaxSurge: 2}, {MaxSurge: 2}}
	steps := ComputeAllSteps([]int{6, 6}, []int{6, 6}, cfg)
	require.True(t, completes(steps, []int{6, 6}), "rollout should complete")

	// With surge=2, total should never drop below target (12)
	for _, s := range steps {
		total := totalAtStep(s)
		assert.GreaterOrEqual(t, total, 12, "surge should not drop below target")
	}
}

func TestSurgePriority_Unavailable2Behavior(t *testing.T) {
	cfg := []RollingUpdateConfig{{MaxUnavailable: 2}, {MaxUnavailable: 2}}
	steps := ComputeAllSteps([]int{6, 6}, []int{6, 6}, cfg)
	require.True(t, completes(steps, []int{6, 6}), "rollout should complete")

	// With unavailable=2, total should drop below target (12)
	minTotal := 100
	for _, s := range steps {
		if total := totalAtStep(s); total < minTotal {
			minTotal = total
		}
	}
	assert.Less(t, minTotal, 12, "unavailable should drop below target")
}

func TestSurgePriority_BothSurgeAndUnavailableUsesSurge(t *testing.T) {
	cfg := []RollingUpdateConfig{{MaxSurge: 2, MaxUnavailable: 2}, {MaxSurge: 2, MaxUnavailable: 2}}
	steps := ComputeAllSteps([]int{6, 6}, []int{6, 6}, cfg)
	require.True(t, completes(steps, []int{6, 6}), "rollout should complete")

	// With both set, surge takes priority (total >= 12)
	for _, s := range steps {
		total := totalAtStep(s)
		assert.GreaterOrEqual(t, total, 12, "surge should take priority")
	}
}

func TestMixedSurgeUnavailable_Role0SurgeRole1Unavailable(t *testing.T) {
	cfg := []RollingUpdateConfig{{MaxSurge: 1}, {MaxUnavailable: 1}}
	steps := ComputeAllSteps([]int{4, 4}, []int{4, 4}, cfg)
	assert.True(t, completes(steps, []int{4, 4}), "rollout should complete")
}

func TestMixedSurgeUnavailable_Role0UnavailableRole1Surge(t *testing.T) {
	cfg := []RollingUpdateConfig{{MaxUnavailable: 1}, {MaxSurge: 1}}
	steps := ComputeAllSteps([]int{4, 4}, []int{4, 4}, cfg)
	assert.True(t, completes(steps, []int{4, 4}), "rollout should complete")
}

func TestMixedSurgeUnavailable_Asymmetric(t *testing.T) {
	testCases := []struct {
		sp, sd int
	}{
		{6, 2}, {2, 6}, {8, 4},
	}

	cfg := []RollingUpdateConfig{{MaxSurge: 1}, {MaxUnavailable: 1}}

	for _, tc := range testCases {
		steps := ComputeAllSteps([]int{tc.sp, tc.sd}, []int{tc.sp, tc.sd}, cfg)
		assert.True(t, completes(steps, []int{tc.sp, tc.sd}), "sp=%d, sd=%d: rollout should complete", tc.sp, tc.sd)
	}
}

func TestUnavailableEdgeCases_ScaleUpWithUnavailable(t *testing.T) {
	steps := ComputeAllSteps([]int{2, 2}, []int{4, 4}, []RollingUpdateConfig{{MaxUnavailable: 1}, {MaxUnavailable: 1}})
	if !completes(steps, []int{4, 4}) {
		t.Error("Scale-up with unavailable did not complete")
	}
}

func TestUnavailableEdgeCases_ScaleDownWithUnavailable(t *testing.T) {
	steps := ComputeAllSteps([]int{4, 4}, []int{2, 2}, []RollingUpdateConfig{{MaxUnavailable: 1}, {MaxUnavailable: 1}})
	if !completes(steps, []int{2, 2}) {
		t.Error("Scale-down with unavailable did not complete")
	}
}

func TestUnavailableEdgeCases_FreshDeployWithUnavailable(t *testing.T) {
	steps := ComputeAllSteps([]int{0, 0}, []int{4, 4}, []RollingUpdateConfig{{MaxUnavailable: 1}, {MaxUnavailable: 1}})
	if !completes(steps, []int{4, 4}) {
		t.Error("Fresh deploy with unavailable did not complete")
	}
}

func TestUnavailableEdgeCases_NoInfiniteLoop(t *testing.T) {
	testCases := []struct {
		size, unavailable int
	}{
		{4, 1}, {6, 2}, {10, 5},
	}

	for _, tc := range testCases {
		steps := ComputeAllSteps([]int{tc.size, tc.size}, []int{tc.size, tc.size}, []RollingUpdateConfig{{MaxUnavailable: tc.unavailable}, {MaxUnavailable: tc.unavailable}})
		assert.LessOrEqual(t, len(steps), tc.size*4, "size=%d, unavailable=%d: too many steps (%d)", tc.size, tc.unavailable, len(steps))
		assert.True(t, completes(steps, []int{tc.size, tc.size}), "size=%d, unavailable=%d: rollout should complete", tc.size, tc.unavailable)
	}
}

// =============================================================================
// Helper Function Tests
// =============================================================================

func TestBatchSize(t *testing.T) {
	testCases := []struct {
		maxSurge, maxUnavailable, expected int
	}{
		{1, 0, 1},
		{2, 0, 2},
		{0, 1, 1},
		{0, 2, 2},
		{0, 0, 1}, // minimum is 1
		{3, 2, 3}, // surge takes priority
	}

	for _, tc := range testCases {
		result := batchSize(tc.maxSurge, tc.maxUnavailable)
		assert.Equal(t, tc.expected, result, "batchSize(%d, %d)", tc.maxSurge, tc.maxUnavailable)
	}
}

func TestComputeTotalSteps(t *testing.T) {
	testCases := []struct {
		source, target RoleReplicaState
		config         []RollingUpdateConfig
		expected       int
	}{
		{
			RoleReplicaState{4, 4}, RoleReplicaState{4, 4},
			DefaultRollingUpdateConfig(2), 4,
		},
		{
			RoleReplicaState{6, 2}, RoleReplicaState{6, 2},
			DefaultRollingUpdateConfig(2), 6,
		},
		{
			RoleReplicaState{4, 4}, RoleReplicaState{4, 4},
			[]RollingUpdateConfig{{MaxSurge: 2}, {MaxSurge: 2}}, 2,
		},
		{
			RoleReplicaState{0, 0}, RoleReplicaState{3, 3},
			DefaultRollingUpdateConfig(2), 3,
		},
	}

	for _, tc := range testCases {
		result := computeTotalSteps(tc.source, tc.target, tc.config)
		assert.Equal(t, tc.expected, result, "computeTotalSteps(%v, %v, %v)", tc.source, tc.target, tc.config)
	}
}

func TestCorrectAbnormalState_Normal(t *testing.T) {
	// Normal state should return nil
	currentOld := RoleReplicaState{2, 2}
	currentNew := RoleReplicaState{2, 2}
	source := RoleReplicaState{4, 4}

	result := correctAbnormalState(currentOld, currentNew, source)
	assert.Nil(t, result, "normal state should return nil")
}

func TestCorrectAbnormalState_Abnormal(t *testing.T) {
	// Abnormal state: old > source
	currentOld := RoleReplicaState{5, 5}
	currentNew := RoleReplicaState{2, 2}
	source := RoleReplicaState{4, 4}

	result := correctAbnormalState(currentOld, currentNew, source)
	require.NotNil(t, result, "abnormal state should return correction step")
	assert.Equal(t, 4, result.Past[0], "old role0 should be clamped to source")
	assert.Equal(t, 4, result.Past[1], "old role1 should be clamped to source")
	assert.Equal(t, 2, result.New[0], "new role0 should be unchanged")
	assert.Equal(t, 2, result.New[1], "new role1 should be unchanged")
}

func TestComputeNextStep_ReturnsNilWhenDone(t *testing.T) {
	cfg := DefaultRollingUpdateConfig(2)

	testCases := []struct {
		name       string
		source     RoleReplicaState
		currentOld RoleReplicaState
		currentNew RoleReplicaState
		targetNew  RoleReplicaState
	}{
		{
			name:       "exactly at target",
			source:     RoleReplicaState{3, 6},
			currentOld: RoleReplicaState{0, 0},
			currentNew: RoleReplicaState{3, 6},
			targetNew:  RoleReplicaState{3, 6},
		},
		{
			name:       "new exceeds target",
			source:     RoleReplicaState{3, 6},
			currentOld: RoleReplicaState{0, 0},
			currentNew: RoleReplicaState{4, 7},
			targetNew:  RoleReplicaState{3, 6},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			result := ComputeNextStep(tc.source, tc.currentOld, tc.currentNew, tc.targetNew, cfg)
			assert.Nil(t, result, "should return nil when rollout is complete")
		})
	}
}

func TestComputeNextStep_FreshStart(t *testing.T) {
	cfg := DefaultRollingUpdateConfig(2)

	source := RoleReplicaState{4, 4}
	currentOld := RoleReplicaState{4, 4}
	currentNew := RoleReplicaState{0, 0}
	targetNew := RoleReplicaState{4, 4}

	result := ComputeNextStep(source, currentOld, currentNew, targetNew, cfg)
	require.NotNil(t, result, "fresh start should return a step")

	// First step should create some new replicas
	assert.Greater(t, result.New[0], 0, "first step should create new role0 replicas")
	assert.Greater(t, result.New[1], 0, "first step should create new role1 replicas")
}

// =============================================================================
// Coverage Gap Tests - computeNextNewReplicas edge cases
// =============================================================================

func TestComputeNextNewReplicas_EdgeCases(t *testing.T) {
	testCases := []struct {
		name       string
		target     RoleReplicaState
		currentNew RoleReplicaState
		totalSteps int
		checkFunc  func(t *testing.T, result RoleReplicaState)
	}{
		{
			name:       "target_role0_zero",
			target:     RoleReplicaState{0, 4},
			currentNew: RoleReplicaState{0, 2},
			totalSteps: 4,
			checkFunc: func(t *testing.T, result RoleReplicaState) {
				assert.Equal(t, 0, result[0], "role0 should remain 0 when target is 0")
				assert.Greater(t, result[1], 2, "role1 should increase")
			},
		},
		{
			name:       "target_role1_zero",
			target:     RoleReplicaState{4, 0},
			currentNew: RoleReplicaState{2, 0},
			totalSteps: 4,
			checkFunc: func(t *testing.T, result RoleReplicaState) {
				assert.Greater(t, result[0], 2, "role0 should increase")
				assert.Equal(t, 0, result[1], "role1 should remain 0 when target is 0")
			},
		},
		{
			name:       "total_steps_zero",
			target:     RoleReplicaState{4, 4},
			currentNew: RoleReplicaState{2, 2},
			totalSteps: 0,
			checkFunc: func(t *testing.T, result RoleReplicaState) {
				assert.Equal(t, 4, result[0], "should return target when totalSteps is 0")
				assert.Equal(t, 4, result[1], "should return target when totalSteps is 0")
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			result := computeNextNewReplicas(tc.target, tc.currentNew, tc.totalSteps)
			tc.checkFunc(t, result)
		})
	}
}

// =============================================================================
// Coverage Gap Tests - computeNextOldReplicas edge cases
// =============================================================================

func TestComputeNextOldReplicas_EdgeCases(t *testing.T) {
	testCases := []struct {
		name       string
		source     RoleReplicaState
		currentOld RoleReplicaState
		totalSteps int
		checkFunc  func(t *testing.T, result RoleReplicaState)
	}{
		{
			name:       "source_role0_zero",
			source:     RoleReplicaState{0, 4},
			currentOld: RoleReplicaState{0, 3},
			totalSteps: 4,
			checkFunc: func(t *testing.T, result RoleReplicaState) {
				assert.Equal(t, 0, result[0], "role0 should remain 0")
				assert.LessOrEqual(t, result[1], 3, "role1 should decrease or stay same")
			},
		},
		{
			name:       "source_role1_zero",
			source:     RoleReplicaState{4, 0},
			currentOld: RoleReplicaState{3, 0},
			totalSteps: 4,
			checkFunc: func(t *testing.T, result RoleReplicaState) {
				assert.LessOrEqual(t, result[0], 3, "role0 should decrease or stay same")
				assert.Equal(t, 0, result[1], "role1 should remain 0")
			},
		},
		{
			name:       "total_steps_zero",
			source:     RoleReplicaState{4, 4},
			currentOld: RoleReplicaState{2, 2},
			totalSteps: 0,
			checkFunc: func(t *testing.T, result RoleReplicaState) {
				assert.Equal(t, 0, result[0], "should return zeros when totalSteps is 0")
				assert.Equal(t, 0, result[1], "should return zeros when totalSteps is 0")
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			result := computeNextOldReplicas(tc.source, tc.currentOld, tc.totalSteps)
			tc.checkFunc(t, result)
		})
	}
}

// =============================================================================
// N-Role Tests (3, 4, 5 roles)
// =============================================================================

func TestNRole_RolloutCompletes(t *testing.T) {
	testCases := []struct {
		name    string
		source  []int
		target  []int
		surge   []int
		unavail []int
	}{
		// 3-role scenarios
		{"3role_symmetric", []int{3, 3, 3}, []int{3, 3, 3}, []int{1, 1, 1}, []int{0, 0, 0}},
		{"3role_asymmetric", []int{6, 3, 2}, []int{6, 3, 2}, []int{2, 1, 1}, []int{0, 0, 0}},
		{"3role_different_surge", []int{4, 4, 4}, []int{4, 4, 4}, []int{2, 1, 3}, []int{0, 0, 0}},
		{"3role_scale_up", []int{2, 2, 2}, []int{4, 4, 4}, []int{1, 1, 1}, []int{0, 0, 0}},
		{"3role_scale_down", []int{4, 4, 4}, []int{2, 2, 2}, []int{1, 1, 1}, []int{0, 0, 0}},
		{"3role_fresh_deploy", []int{0, 0, 0}, []int{3, 3, 3}, []int{1, 1, 1}, []int{0, 0, 0}},
		{"3role_unavailable", []int{4, 4, 4}, []int{4, 4, 4}, []int{0, 0, 0}, []int{1, 1, 1}},
		{"3role_mixed_surge_unavail", []int{4, 4, 4}, []int{4, 4, 4}, []int{1, 0, 2}, []int{0, 1, 0}},

		// 4-role scenarios
		{"4role_symmetric", []int{4, 4, 4, 4}, []int{4, 4, 4, 4}, []int{1, 1, 1, 1}, []int{0, 0, 0, 0}},
		{"4role_asymmetric", []int{8, 4, 2, 1}, []int{8, 4, 2, 1}, []int{2, 2, 1, 1}, []int{0, 0, 0, 0}},
		{"4role_scale_up", []int{1, 1, 1, 1}, []int{3, 3, 3, 3}, []int{1, 1, 1, 1}, []int{0, 0, 0, 0}},
		{"4role_scale_down", []int{5, 5, 5, 5}, []int{2, 2, 2, 2}, []int{1, 1, 1, 1}, []int{0, 0, 0, 0}},
		{"4role_fresh_deploy", []int{0, 0, 0, 0}, []int{4, 4, 4, 4}, []int{1, 1, 1, 1}, []int{0, 0, 0, 0}},

		// 5-role scenarios
		{"5role_symmetric", []int{5, 5, 5, 5, 5}, []int{5, 5, 5, 5, 5}, []int{1, 1, 1, 1, 1}, []int{0, 0, 0, 0, 0}},
		{"5role_asymmetric", []int{10, 5, 3, 2, 1}, []int{10, 5, 3, 2, 1}, []int{2, 2, 1, 1, 1}, []int{0, 0, 0, 0, 0}},
		{"5role_scale_up", []int{1, 1, 1, 1, 1}, []int{2, 2, 2, 2, 2}, []int{1, 1, 1, 1, 1}, []int{0, 0, 0, 0, 0}},
		{"5role_scale_down", []int{6, 6, 6, 6, 6}, []int{3, 3, 3, 3, 3}, []int{1, 1, 1, 1, 1}, []int{0, 0, 0, 0, 0}},
		{"5role_fresh_deploy", []int{0, 0, 0, 0, 0}, []int{5, 5, 5, 5, 5}, []int{1, 1, 1, 1, 1}, []int{0, 0, 0, 0, 0}},

		// Role addition: 2 roles -> 3 roles (a,b -> a,b,c)
		{"add_role_2to3", []int{4, 4, 0}, []int{4, 4, 4}, []int{1, 1, 1}, []int{0, 0, 0}},
		{"add_role_3to4", []int{3, 3, 3, 0}, []int{3, 3, 3, 3}, []int{1, 1, 1, 1}, []int{0, 0, 0, 0}},
		{"add_role_5to6", []int{2, 2, 2, 2, 2, 0}, []int{2, 2, 2, 2, 2, 2}, []int{1, 1, 1, 1, 1, 1}, []int{0, 0, 0, 0, 0, 0}},

		// Role removal: 3 roles -> 2 roles (a,b,c -> a,b)
		{"remove_role_3to2", []int{4, 4, 4}, []int{4, 4, 0}, []int{1, 1, 1}, []int{0, 0, 0}},
		{"remove_role_4to3", []int{3, 3, 3, 3}, []int{3, 3, 3, 0}, []int{1, 1, 1, 1}, []int{0, 0, 0, 0}},

		// Role rename (simultaneous add + remove): a,b,c,d,i -> a,b,c,d,h
		// Sorted order becomes [a,b,c,d,h,i] with source h=0, target i=0
		{"rename_role_5to5", []int{10, 10, 10, 10, 0, 10}, []int{10, 10, 10, 10, 10, 0}, []int{1, 1, 1, 1, 1, 1}, []int{0, 0, 0, 0, 0, 0}},

		// Scale-down scenarios (maxSurge constraint regression tests)
		{"scale_down_prefill_up_decode", []int{10, 2}, []int{6, 8}, []int{2, 2}, []int{0, 0}},
		{"scale_down_both", []int{10, 10}, []int{4, 4}, []int{2, 2}, []int{0, 0}},
		{"scale_down_asymmetric", []int{8, 4}, []int{3, 2}, []int{1, 1}, []int{0, 0}},

		// maxUnavailable constraint regression test (asymmetric roles)
		{"unavail_asymmetric_5_2", []int{5, 2}, []int{5, 2}, []int{0, 0}, []int{1, 1}},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := make([]RollingUpdateConfig, len(tc.source))
			for i := range cfg {
				cfg[i] = RollingUpdateConfig{MaxSurge: tc.surge[i], MaxUnavailable: tc.unavail[i]}
			}
			steps := ComputeAllSteps(tc.source, tc.target, cfg)
			require.True(t, completes(steps, tc.target), "rollout should complete")
		})
	}
}

func TestNRole_SurgeConstraint(t *testing.T) {
	testCases := []struct {
		name   string
		source []int
		target []int
		surge  []int
	}{
		{"3role", []int{3, 3, 3}, []int{3, 3, 3}, []int{1, 1, 1}},
		{"4role", []int{4, 4, 4, 4}, []int{4, 4, 4, 4}, []int{1, 1, 1, 1}},
		{"5role", []int{5, 5, 5, 5, 5}, []int{5, 5, 5, 5, 5}, []int{1, 1, 1, 1, 1}},
		// Scale-down scenarios (maxSurge constraint regression tests)
		{"scale_down_prefill_up_decode", []int{10, 2}, []int{6, 8}, []int{2, 2}},
		{"scale_down_both", []int{10, 10}, []int{4, 4}, []int{2, 2}},
		{"scale_down_asymmetric", []int{8, 4}, []int{3, 2}, []int{1, 1}},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := make([]RollingUpdateConfig, len(tc.source))
			for i := range cfg {
				cfg[i] = RollingUpdateConfig{MaxSurge: tc.surge[i]}
			}
			steps := ComputeAllSteps(tc.source, tc.target, cfg)

			// Verify per-role maxSurge constraint: old[i] + new[i] <= target[i] + surge[i]
			// Only check when new[i] > 0 (scale-up has started for this role).
			// Drain-only steps (new=0) may exceed constraint while removing old pods.
			for stepIdx, s := range steps {
				for roleIdx := range tc.target {
					if s.New[roleIdx] == 0 {
						continue // Drain-only step, surge constraint doesn't apply
					}
					maxAllowed := tc.target[roleIdx] + tc.surge[roleIdx]
					actual := s.Past[roleIdx] + s.New[roleIdx]
					assert.LessOrEqual(t, actual, maxAllowed,
						"step %d, role %d: old(%d) + new(%d) = %d exceeds target(%d) + surge(%d) = %d",
						stepIdx, roleIdx, s.Past[roleIdx], s.New[roleIdx], actual,
						tc.target[roleIdx], tc.surge[roleIdx], maxAllowed)
				}
			}
		})
	}
}

func TestNRole_UnavailableConstraint(t *testing.T) {
	testCases := []struct {
		name    string
		source  []int
		target  []int
		unavail []int
	}{
		{"symmetric_4_4", []int{4, 4}, []int{4, 4}, []int{1, 1}},
		{"asymmetric_5_2", []int{5, 2}, []int{5, 2}, []int{1, 1}},
		{"asymmetric_2_5", []int{2, 5}, []int{2, 5}, []int{1, 1}},
		{"3role_symmetric", []int{3, 3, 3}, []int{3, 3, 3}, []int{1, 1, 1}},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := make([]RollingUpdateConfig, len(tc.source))
			for i := range cfg {
				cfg[i] = RollingUpdateConfig{MaxUnavailable: tc.unavail[i]}
			}
			steps := ComputeAllSteps(tc.source, tc.target, cfg)

			// Verify rollout completes
			require.True(t, completes(steps, tc.target), "rollout should complete")

			// Verify per-role maxUnavailable constraint: old[i] + new[i] >= target[i] - unavail[i]
			// Only enforced when source[i] >= target[i] (system had enough replicas)
			for stepIdx, s := range steps {
				for roleIdx := range tc.target {
					if tc.source[roleIdx] < tc.target[roleIdx] {
						continue // Scale-up scenario, maxUnavailable not enforced
					}
					minRequired := tc.target[roleIdx] - tc.unavail[roleIdx]
					actual := s.Past[roleIdx] + s.New[roleIdx]
					assert.GreaterOrEqual(t, actual, minRequired,
						"step %d, role %d: old(%d) + new(%d) = %d below target(%d) - unavail(%d) = %d",
						stepIdx, roleIdx, s.Past[roleIdx], s.New[roleIdx], actual,
						tc.target[roleIdx], tc.unavail[roleIdx], minRequired)
				}
			}
		})
	}
}

func TestNRole_DefaultConfig(t *testing.T) {
	for _, numRoles := range []int{3, 4, 5} {
		t.Run(fmt.Sprintf("%d_roles", numRoles), func(t *testing.T) {
			cfg := DefaultRollingUpdateConfig(numRoles)
			assert.Equal(t, numRoles, len(cfg))
			for i := 0; i < numRoles; i++ {
				assert.Equal(t, 1, cfg[i].MaxSurge, "default surge should be 1")
				assert.Equal(t, 0, cfg[i].MaxUnavailable, "default unavailable should be 0")
			}
		})
	}
}

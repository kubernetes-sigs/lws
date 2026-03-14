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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// step is a helper to create UpdateStep instances for tests
func step(pastPrefill, pastDecode, newPrefill, newDecode int) UpdateStep {
	return UpdateStep{
		PastPrefill: pastPrefill,
		PastDecode:  pastDecode,
		NewPrefill:  newPrefill,
		NewDecode:   newDecode,
	}
}

// config is a helper to create RollingUpdateConfig instances for tests
func config(prefillMaxSurge, prefillMaxUnavailable, decodeMaxSurge, decodeMaxUnavailable int) RollingUpdateConfig {
	return RollingUpdateConfig{
		PrefillMaxSurge:       prefillMaxSurge,
		PrefillMaxUnavailable: prefillMaxUnavailable,
		DecodeMaxSurge:        decodeMaxSurge,
		DecodeMaxUnavailable:  decodeMaxUnavailable,
	}
}

// completes checks if rollout completes correctly (old=0, new=target)
func completes(steps []UpdateStep, targetPrefill, targetDecode int) bool {
	if len(steps) == 0 {
		return false
	}
	last := steps[len(steps)-1]
	return last.PastPrefill == 0 && last.PastDecode == 0 &&
		last.NewPrefill == targetPrefill && last.NewDecode == targetDecode
}

// totalAtStep returns total replica count at a step
func totalAtStep(s UpdateStep) int {
	return s.PastPrefill + s.PastDecode + s.NewPrefill + s.NewDecode
}

// stepsEqual compares two step slices for equality
func stepsEqual(a, b []UpdateStep) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
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
		name          string
		sourcePrefill int
		sourceDecode  int
		targetPrefill int
		targetDecode  int
		config        RollingUpdateConfig
		expected      []UpdateStep
	}{
		// Small symmetric cases (decoupled: scale-up then scale-down alternately)
		{
			name:          "small_1_1_surge1",
			sourcePrefill: 1, sourceDecode: 1, targetPrefill: 1, targetDecode: 1,
			config: DefaultRollingUpdateConfig(),
			expected: []UpdateStep{
				step(1, 1, 0, 0),
				step(1, 1, 1, 1), // scale up
				step(0, 0, 1, 1), // scale down
			},
		},
		{
			name:          "small_2_2_surge1",
			sourcePrefill: 2, sourceDecode: 2, targetPrefill: 2, targetDecode: 2,
			config: DefaultRollingUpdateConfig(),
			expected: []UpdateStep{
				step(2, 2, 0, 0),
				step(2, 2, 1, 1), // scale up (surge: 2+1 <= 2+1)
				step(1, 1, 1, 1), // scale down
				step(1, 1, 2, 2), // scale up (surge: 1+2 <= 2+1)
				step(0, 0, 2, 2), // scale down
			},
		},
		{
			name:          "small_3_3_surge1",
			sourcePrefill: 3, sourceDecode: 3, targetPrefill: 3, targetDecode: 3,
			config: DefaultRollingUpdateConfig(),
			expected: []UpdateStep{
				step(3, 3, 0, 0),
				step(3, 3, 1, 1), // scale up
				step(2, 2, 1, 1), // scale down
				step(2, 2, 2, 2), // scale up
				step(1, 1, 2, 2), // scale down
				step(1, 1, 3, 3), // scale up
				step(0, 0, 3, 3), // scale down
			},
		},
		// Medium asymmetric cases (decoupled steps)
		{
			name:          "medium_6_2_surge1",
			sourcePrefill: 6, sourceDecode: 2, targetPrefill: 6, targetDecode: 2,
			config: DefaultRollingUpdateConfig(),
			expected: []UpdateStep{
				step(6, 2, 0, 0),
				step(6, 2, 1, 1),
				step(5, 2, 1, 1),
				step(5, 2, 2, 1),
				step(4, 2, 2, 1),
				step(4, 2, 3, 1),
				step(3, 1, 3, 1),
				step(3, 1, 4, 2),
				step(2, 1, 4, 2),
				step(2, 1, 5, 2),
				step(1, 1, 5, 2),
				step(1, 1, 6, 2),
				step(0, 0, 6, 2),
			},
		},
		{
			name:          "medium_6_2_surge2",
			sourcePrefill: 6, sourceDecode: 2, targetPrefill: 6, targetDecode: 2,
			config: config(2, 0, 2, 0),
			expected: []UpdateStep{
				step(6, 2, 0, 0),
				step(6, 2, 2, 1),
				step(4, 2, 2, 1),
				step(4, 2, 4, 2),
				step(2, 1, 4, 2),
				step(2, 1, 6, 2),
				step(0, 0, 6, 2),
			},
		},
		{
			name:          "medium_6_4_surge2",
			sourcePrefill: 6, sourceDecode: 4, targetPrefill: 6, targetDecode: 4,
			config: config(2, 0, 2, 0),
			expected: []UpdateStep{
				step(6, 4, 0, 0),
				step(6, 4, 2, 2),
				step(4, 3, 2, 2),
				step(4, 3, 4, 3),
				step(2, 2, 4, 3),
				step(2, 2, 6, 4),
				step(0, 0, 6, 4),
			},
		},
		// Asymmetric cases (gradual interleaved drain)
		{
			name:          "asymmetric_10_1_surge1",
			sourcePrefill: 10, sourceDecode: 1, targetPrefill: 10, targetDecode: 1,
			config: DefaultRollingUpdateConfig(),
			expected: []UpdateStep{
				step(10, 1, 0, 0),
				step(10, 1, 1, 1),
				step(9, 1, 1, 1),
				step(9, 1, 2, 1),
				step(8, 1, 2, 1),
				step(8, 1, 3, 1),
				step(7, 1, 3, 1),
				step(7, 1, 4, 1),
				step(6, 1, 4, 1),
				step(6, 1, 5, 1),
				step(5, 1, 5, 1),
				step(5, 1, 6, 1),
				step(4, 1, 6, 1),
				step(4, 1, 7, 1),
				step(3, 1, 7, 1),
				step(3, 1, 8, 1),
				step(2, 1, 8, 1),
				step(2, 1, 9, 1),
				step(1, 1, 9, 1),
				step(1, 1, 10, 1),
				step(0, 0, 10, 1),
			},
		},
		{
			name:          "asymmetric_1_10_surge1",
			sourcePrefill: 1, sourceDecode: 10, targetPrefill: 1, targetDecode: 10,
			config: DefaultRollingUpdateConfig(),
			expected: []UpdateStep{
				step(1, 10, 0, 0),
				step(1, 10, 1, 1),
				step(1, 9, 1, 1),
				step(1, 9, 1, 2),
				step(1, 8, 1, 2),
				step(1, 8, 1, 3),
				step(1, 7, 1, 3),
				step(1, 7, 1, 4),
				step(1, 6, 1, 4),
				step(1, 6, 1, 5),
				step(1, 5, 1, 5),
				step(1, 5, 1, 6),
				step(1, 4, 1, 6),
				step(1, 4, 1, 7),
				step(1, 3, 1, 7),
				step(1, 3, 1, 8),
				step(1, 2, 1, 8),
				step(1, 2, 1, 9),
				step(1, 1, 1, 9),
				step(1, 1, 1, 10),
				step(0, 0, 1, 10),
			},
		},
		// Large symmetric cases (decoupled: alternating scale-up/scale-down)
		{
			name:          "large_10_10_surge1",
			sourcePrefill: 10, sourceDecode: 10, targetPrefill: 10, targetDecode: 10,
			config: DefaultRollingUpdateConfig(),
			expected: []UpdateStep{
				step(10, 10, 0, 0),
				step(10, 10, 1, 1), // scale up
				step(9, 9, 1, 1),   // scale down
				step(9, 9, 2, 2),   // scale up
				step(8, 8, 2, 2),   // scale down
				step(8, 8, 3, 3),
				step(7, 7, 3, 3),
				step(7, 7, 4, 4),
				step(6, 6, 4, 4),
				step(6, 6, 5, 5),
				step(5, 5, 5, 5),
				step(5, 5, 6, 6),
				step(4, 4, 6, 6),
				step(4, 4, 7, 7),
				step(3, 3, 7, 7),
				step(3, 3, 8, 8),
				step(2, 2, 8, 8),
				step(2, 2, 9, 9),
				step(1, 1, 9, 9),
				step(1, 1, 10, 10),
				step(0, 0, 10, 10),
			},
		},
		{
			// Surge constraint: old + new <= 10 + 3 = 13
			// Interleaves scale-up and scale-down to respect surge
			name:          "large_10_10_surge3",
			sourcePrefill: 10, sourceDecode: 10, targetPrefill: 10, targetDecode: 10,
			config: config(3, 0, 3, 0),
			expected: []UpdateStep{
				step(10, 10, 0, 0),
				step(10, 10, 3, 3), // scale up (10+3=13)
				step(8, 8, 3, 3),   // scale down
				step(8, 8, 5, 5),   // scale up (8+5=13)
				step(5, 5, 5, 5),   // scale down (to allow 8)
				step(5, 5, 8, 8),   // scale up (5+8=13)
				step(3, 3, 8, 8),   // scale down (to allow 10)
				step(3, 3, 10, 10), // scale up (3+10=13)
				step(0, 0, 10, 10), // final drain
			},
		},
		{
			name:          "large_12_6_surge2",
			sourcePrefill: 12, sourceDecode: 6, targetPrefill: 12, targetDecode: 6,
			config: config(2, 0, 2, 0),
			expected: []UpdateStep{
				step(12, 6, 0, 0),
				step(12, 6, 2, 1),
				step(10, 5, 2, 1),
				step(10, 5, 4, 2),
				step(8, 4, 4, 2),
				step(8, 4, 6, 3),
				step(6, 3, 6, 3),
				step(6, 3, 8, 4),
				step(4, 2, 8, 4),
				step(4, 2, 10, 5),
				step(2, 1, 10, 5),
				step(2, 1, 12, 6),
				step(0, 0, 12, 6),
			},
		},
		// Scale up/down scenarios (decoupled)
		{
			name:          "scale_up_1_1_to_3_3",
			sourcePrefill: 1, sourceDecode: 1, targetPrefill: 3, targetDecode: 3,
			config: DefaultRollingUpdateConfig(),
			expected: []UpdateStep{
				step(1, 1, 0, 0),
				step(1, 1, 1, 1), // scale up
				step(1, 1, 2, 2), // scale up (old still 1, new 2, surge ok: 1+2<=3+1)
				step(1, 1, 3, 3), // scale up (new at target)
				step(0, 0, 3, 3), // scale down
			},
		},
		{
			// Scale up 4→6 with surge=1: max total = 6+1 = 7
			// Interleaves scale-up and scale-down to respect surge
			name:          "scale_up_4_4_to_6_6",
			sourcePrefill: 4, sourceDecode: 4, targetPrefill: 6, targetDecode: 6,
			config: DefaultRollingUpdateConfig(),
			expected: []UpdateStep{
				step(4, 4, 0, 0),
				step(4, 4, 1, 1), // scale up (4+1=5)
				step(4, 4, 2, 2), // scale up (4+2=6)
				step(4, 4, 3, 3), // scale up (4+3=7)
				step(3, 3, 3, 3), // scale down (to allow 4)
				step(3, 3, 4, 4), // scale up (3+4=7)
				step(2, 2, 4, 4), // scale down (to allow 5)
				step(2, 2, 5, 5), // scale up (2+5=7)
				step(1, 1, 5, 5), // scale down (to allow 6)
				step(1, 1, 6, 6), // scale up (1+6=7)
				step(0, 0, 6, 6), // final drain
			},
		},
		{
			name:          "scale_down_5_5_to_2_2",
			sourcePrefill: 5, sourceDecode: 5, targetPrefill: 2, targetDecode: 2,
			config: DefaultRollingUpdateConfig(),
			expected: []UpdateStep{
				step(5, 5, 0, 0),
				step(4, 4, 0, 0), // scale down (can't scale up: surge=1, 5+1>2+1)
				step(3, 3, 0, 0), // scale down
				step(2, 2, 0, 0), // scale down (now old=2=target)
				step(2, 2, 1, 1), // scale up (surge: 2+1<=2+1)
				step(1, 1, 1, 1), // scale down
				step(1, 1, 2, 2), // scale up (new at target)
				step(0, 0, 2, 2), // scale down
			},
		},
		{
			name:          "mixed_scale_3_5_to_5_3",
			sourcePrefill: 3, sourceDecode: 5, targetPrefill: 5, targetDecode: 3,
			config: DefaultRollingUpdateConfig(),
			expected: []UpdateStep{
				step(3, 5, 0, 0),
				step(3, 4, 0, 0),
				step(2, 3, 0, 0),
				step(2, 3, 1, 1),
				step(2, 2, 1, 1),
				step(2, 2, 2, 2),
				step(2, 2, 3, 2),
				step(1, 1, 3, 2),
				step(1, 1, 4, 3),
				step(1, 1, 5, 3),
				step(0, 0, 5, 3),
			},
		},
		{
			name:          "asymmetric_2_4_to_4_2",
			sourcePrefill: 2, sourceDecode: 4, targetPrefill: 4, targetDecode: 2,
			config: DefaultRollingUpdateConfig(),
			expected: []UpdateStep{
				step(2, 4, 0, 0),
				step(2, 3, 0, 0),
				step(1, 2, 0, 0),
				step(1, 2, 1, 1),
				step(1, 2, 2, 1),
				step(1, 1, 2, 1),
				step(1, 1, 3, 2),
				step(1, 1, 4, 2),
				step(0, 0, 4, 2),
			},
		},
		{
			name:          "proportional_3_5_to_4_2",
			sourcePrefill: 3, sourceDecode: 5, targetPrefill: 4, targetDecode: 2,
			config: DefaultRollingUpdateConfig(),
			expected: []UpdateStep{
				step(3, 5, 0, 0),
				step(3, 4, 0, 0),
				step(2, 3, 0, 0),
				step(2, 2, 0, 0),
				step(2, 2, 1, 1),
				step(2, 2, 2, 1),
				step(1, 1, 2, 1),
				step(1, 1, 3, 2),
				step(1, 1, 4, 2),
				step(0, 0, 4, 2),
			},
		},
		{
			name:          "medium_4_4_surge2",
			sourcePrefill: 4, sourceDecode: 4, targetPrefill: 4, targetDecode: 4,
			config: config(2, 0, 2, 0),
			expected: []UpdateStep{
				step(4, 4, 0, 0),
				step(4, 4, 2, 2), // scale up (surge: 4+2<=4+2)
				step(2, 2, 2, 2), // scale down
				step(2, 2, 4, 4), // scale up (new at target)
				step(0, 0, 4, 4), // scale down
			},
		},
		{
			name:          "asymmetric_surge_4_6",
			sourcePrefill: 4, sourceDecode: 6, targetPrefill: 4, targetDecode: 6,
			config: config(2, 0, 3, 0),
			expected: []UpdateStep{
				step(4, 6, 0, 0),
				step(4, 6, 2, 3),
				step(2, 3, 2, 3),
				step(2, 3, 4, 6),
				step(0, 0, 4, 6),
			},
		},
		// Edge cases
		{
			name:          "fresh_deploy_0_0_to_3_3",
			sourcePrefill: 0, sourceDecode: 0, targetPrefill: 3, targetDecode: 3,
			config: DefaultRollingUpdateConfig(),
			expected: []UpdateStep{
				step(0, 0, 0, 0),
				step(0, 0, 1, 1),
				step(0, 0, 2, 2),
				step(0, 0, 3, 3),
			},
		},
		{
			name:          "empty_0_0_to_0_0",
			sourcePrefill: 0, sourceDecode: 0, targetPrefill: 0, targetDecode: 0,
			config: DefaultRollingUpdateConfig(),
			expected: []UpdateStep{
				step(0, 0, 0, 0),
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			actual := ComputeAllSteps(tc.sourcePrefill, tc.sourceDecode, tc.targetPrefill, tc.targetDecode, tc.config)
			assert.True(t, stepsEqual(actual, tc.expected), "ComputeAllSteps mismatch:\ngot:  %v\nwant: %v", actual, tc.expected)
		})
	}
}

// =============================================================================
// Unavailable Strategy Tests
// =============================================================================

func TestUnavailableBasic_Symmetric4_4(t *testing.T) {
	steps := ComputeAllSteps(4, 4, 4, 4, config(0, 1, 0, 1))
	require.True(t, completes(steps, 4, 4), "rollout should complete")

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
	steps := ComputeAllSteps(4, 4, 4, 4, config(0, 1, 0, 1))

	// Verify unavailable pattern: old decreases faster than new increases
	foundUnavailablePattern := false
	for i := 1; i < len(steps); i++ {
		prev := steps[i-1]
		curr := steps[i]
		oldDecreased := curr.PastPrefill < prev.PastPrefill || curr.PastDecode < prev.PastDecode
		newIncrease := (curr.NewPrefill - prev.NewPrefill) + (curr.NewDecode - prev.NewDecode)
		oldDecrease := (prev.PastPrefill - curr.PastPrefill) + (prev.PastDecode - curr.PastDecode)
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
	cfg := RollingUpdateConfig{
		PrefillMaxSurge: 1, PrefillMaxUnavailable: 1,
		DecodeMaxSurge: 1, DecodeMaxUnavailable: 1,
	}
	steps := ComputeAllSteps(4, 4, 4, 4, cfg)
	require.True(t, completes(steps, 4, 4), "rollout should complete")

	// With surge > 0, total should never drop below target (8)
	for _, s := range steps {
		total := totalAtStep(s)
		assert.GreaterOrEqual(t, total, 8, "total should never drop below 8 when surge > 0")
	}
}

func TestSurgePriority_UnavailableWhenSurgeZero(t *testing.T) {
	cfg := RollingUpdateConfig{
		PrefillMaxSurge: 0, PrefillMaxUnavailable: 1,
		DecodeMaxSurge: 0, DecodeMaxUnavailable: 1,
	}
	steps := ComputeAllSteps(4, 4, 4, 4, cfg)
	require.True(t, completes(steps, 4, 4), "rollout should complete")

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
	cfg := RollingUpdateConfig{
		PrefillMaxSurge: 2, PrefillMaxUnavailable: 0,
		DecodeMaxSurge: 2, DecodeMaxUnavailable: 0,
	}
	steps := ComputeAllSteps(6, 6, 6, 6, cfg)
	require.True(t, completes(steps, 6, 6), "rollout should complete")

	// With surge=2, total should never drop below target (12)
	for _, s := range steps {
		total := totalAtStep(s)
		assert.GreaterOrEqual(t, total, 12, "surge should not drop below target")
	}
}

func TestSurgePriority_Unavailable2Behavior(t *testing.T) {
	cfg := RollingUpdateConfig{
		PrefillMaxSurge: 0, PrefillMaxUnavailable: 2,
		DecodeMaxSurge: 0, DecodeMaxUnavailable: 2,
	}
	steps := ComputeAllSteps(6, 6, 6, 6, cfg)
	require.True(t, completes(steps, 6, 6), "rollout should complete")

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
	cfg := RollingUpdateConfig{
		PrefillMaxSurge: 2, PrefillMaxUnavailable: 2,
		DecodeMaxSurge: 2, DecodeMaxUnavailable: 2,
	}
	steps := ComputeAllSteps(6, 6, 6, 6, cfg)
	require.True(t, completes(steps, 6, 6), "rollout should complete")

	// With both set, surge takes priority (total >= 12)
	for _, s := range steps {
		total := totalAtStep(s)
		assert.GreaterOrEqual(t, total, 12, "surge should take priority")
	}
}

func TestMixedSurgeUnavailable_PrefillSurgeDecodeUnavailable(t *testing.T) {
	cfg := RollingUpdateConfig{
		PrefillMaxSurge: 1, PrefillMaxUnavailable: 0,
		DecodeMaxSurge: 0, DecodeMaxUnavailable: 1,
	}
	steps := ComputeAllSteps(4, 4, 4, 4, cfg)
	assert.True(t, completes(steps, 4, 4), "rollout should complete")
}

func TestMixedSurgeUnavailable_PrefillUnavailableDecodeSurge(t *testing.T) {
	cfg := RollingUpdateConfig{
		PrefillMaxSurge: 0, PrefillMaxUnavailable: 1,
		DecodeMaxSurge: 1, DecodeMaxUnavailable: 0,
	}
	steps := ComputeAllSteps(4, 4, 4, 4, cfg)
	assert.True(t, completes(steps, 4, 4), "rollout should complete")
}

func TestMixedSurgeUnavailable_Asymmetric(t *testing.T) {
	testCases := []struct {
		sp, sd int
	}{
		{6, 2}, {2, 6}, {8, 4},
	}

	cfg := RollingUpdateConfig{
		PrefillMaxSurge: 1, PrefillMaxUnavailable: 0,
		DecodeMaxSurge: 0, DecodeMaxUnavailable: 1,
	}

	for _, tc := range testCases {
		steps := ComputeAllSteps(tc.sp, tc.sd, tc.sp, tc.sd, cfg)
		assert.True(t, completes(steps, tc.sp, tc.sd), "sp=%d, sd=%d: rollout should complete", tc.sp, tc.sd)
	}
}

func TestUnavailableEdgeCases_ScaleUpWithUnavailable(t *testing.T) {
	steps := ComputeAllSteps(2, 2, 4, 4, config(0, 1, 0, 1))
	if !completes(steps, 4, 4) {
		t.Error("Scale-up with unavailable did not complete")
	}
}

func TestUnavailableEdgeCases_ScaleDownWithUnavailable(t *testing.T) {
	steps := ComputeAllSteps(4, 4, 2, 2, config(0, 1, 0, 1))
	if !completes(steps, 2, 2) {
		t.Error("Scale-down with unavailable did not complete")
	}
}

func TestUnavailableEdgeCases_FreshDeployWithUnavailable(t *testing.T) {
	steps := ComputeAllSteps(0, 0, 4, 4, config(0, 1, 0, 1))
	if !completes(steps, 4, 4) {
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
		steps := ComputeAllSteps(tc.size, tc.size, tc.size, tc.size, config(0, tc.unavailable, 0, tc.unavailable))
		assert.LessOrEqual(t, len(steps), tc.size*4, "size=%d, unavailable=%d: too many steps (%d)", tc.size, tc.unavailable, len(steps))
		assert.True(t, completes(steps, tc.size, tc.size), "size=%d, unavailable=%d: rollout should complete", tc.size, tc.unavailable)
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
		source, target PhaseReplicaState
		config         RollingUpdateConfig
		expected       int
	}{
		{
			PhaseReplicaState{4, 4}, PhaseReplicaState{4, 4},
			DefaultRollingUpdateConfig(), 4,
		},
		{
			PhaseReplicaState{6, 2}, PhaseReplicaState{6, 2},
			DefaultRollingUpdateConfig(), 6,
		},
		{
			PhaseReplicaState{4, 4}, PhaseReplicaState{4, 4},
			config(2, 0, 2, 0), 2,
		},
		{
			PhaseReplicaState{0, 0}, PhaseReplicaState{3, 3},
			DefaultRollingUpdateConfig(), 3,
		},
	}

	for _, tc := range testCases {
		result := computeTotalSteps(tc.source, tc.target, tc.config)
		assert.Equal(t, tc.expected, result, "computeTotalSteps(%v, %v, %v)", tc.source, tc.target, tc.config)
	}
}

func TestCorrectAbnormalState_Normal(t *testing.T) {
	// Normal state should return nil
	currentOld := PhaseReplicaState{Prefill: 2, Decode: 2}
	currentNew := PhaseReplicaState{Prefill: 2, Decode: 2}
	source := PhaseReplicaState{Prefill: 4, Decode: 4}

	result := correctAbnormalState(currentOld, currentNew, source)
	assert.Nil(t, result, "normal state should return nil")
}

func TestCorrectAbnormalState_Abnormal(t *testing.T) {
	// Abnormal state: old > source
	currentOld := PhaseReplicaState{Prefill: 5, Decode: 5}
	currentNew := PhaseReplicaState{Prefill: 2, Decode: 2}
	source := PhaseReplicaState{Prefill: 4, Decode: 4}

	result := correctAbnormalState(currentOld, currentNew, source)
	require.NotNil(t, result, "abnormal state should return correction step")
	assert.Equal(t, 4, result.PastPrefill, "old prefill should be clamped to source")
	assert.Equal(t, 4, result.PastDecode, "old decode should be clamped to source")
	assert.Equal(t, 2, result.NewPrefill, "new prefill should be unchanged")
	assert.Equal(t, 2, result.NewDecode, "new decode should be unchanged")
}

func TestComputeNextStep_ReturnsNilWhenDone(t *testing.T) {
	cfg := DefaultRollingUpdateConfig()

	testCases := []struct {
		name       string
		source     PhaseReplicaState
		currentOld PhaseReplicaState
		currentNew PhaseReplicaState
		targetNew  PhaseReplicaState
	}{
		{
			name:       "exactly at target",
			source:     PhaseReplicaState{3, 6},
			currentOld: PhaseReplicaState{0, 0},
			currentNew: PhaseReplicaState{3, 6},
			targetNew:  PhaseReplicaState{3, 6},
		},
		{
			name:       "new exceeds target",
			source:     PhaseReplicaState{3, 6},
			currentOld: PhaseReplicaState{0, 0},
			currentNew: PhaseReplicaState{4, 7},
			targetNew:  PhaseReplicaState{3, 6},
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
	cfg := DefaultRollingUpdateConfig()

	source := PhaseReplicaState{4, 4}
	currentOld := PhaseReplicaState{4, 4}
	currentNew := PhaseReplicaState{0, 0}
	targetNew := PhaseReplicaState{4, 4}

	result := ComputeNextStep(source, currentOld, currentNew, targetNew, cfg)
	require.NotNil(t, result, "fresh start should return a step")

	// First step should create some new replicas
	assert.Greater(t, result.NewPrefill, 0, "first step should create new prefill replicas")
	assert.Greater(t, result.NewDecode, 0, "first step should create new decode replicas")
}

// =============================================================================
// Coverage Gap Tests - computeNextNewReplicas edge cases
// =============================================================================

func TestComputeNextNewReplicas_EdgeCases(t *testing.T) {
	testCases := []struct {
		name       string
		target     PhaseReplicaState
		currentNew PhaseReplicaState
		totalSteps int
		checkFunc  func(t *testing.T, result PhaseReplicaState)
	}{
		{
			name:       "target_prefill_zero",
			target:     PhaseReplicaState{Prefill: 0, Decode: 4},
			currentNew: PhaseReplicaState{Prefill: 0, Decode: 2},
			totalSteps: 4,
			checkFunc: func(t *testing.T, result PhaseReplicaState) {
				assert.Equal(t, 0, result.Prefill, "prefill should remain 0 when target is 0")
				assert.Greater(t, result.Decode, 2, "decode should increase")
			},
		},
		{
			name:       "target_decode_zero",
			target:     PhaseReplicaState{Prefill: 4, Decode: 0},
			currentNew: PhaseReplicaState{Prefill: 2, Decode: 0},
			totalSteps: 4,
			checkFunc: func(t *testing.T, result PhaseReplicaState) {
				assert.Greater(t, result.Prefill, 2, "prefill should increase")
				assert.Equal(t, 0, result.Decode, "decode should remain 0 when target is 0")
			},
		},
		{
			name:       "total_steps_zero",
			target:     PhaseReplicaState{Prefill: 4, Decode: 4},
			currentNew: PhaseReplicaState{Prefill: 2, Decode: 2},
			totalSteps: 0,
			checkFunc: func(t *testing.T, result PhaseReplicaState) {
				assert.Equal(t, 4, result.Prefill, "should return target when totalSteps is 0")
				assert.Equal(t, 4, result.Decode, "should return target when totalSteps is 0")
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
		source     PhaseReplicaState
		currentOld PhaseReplicaState
		totalSteps int
		checkFunc  func(t *testing.T, result PhaseReplicaState)
	}{
		{
			name:       "source_prefill_zero",
			source:     PhaseReplicaState{Prefill: 0, Decode: 4},
			currentOld: PhaseReplicaState{Prefill: 0, Decode: 3},
			totalSteps: 4,
			checkFunc: func(t *testing.T, result PhaseReplicaState) {
				assert.Equal(t, 0, result.Prefill, "prefill should remain 0")
				assert.LessOrEqual(t, result.Decode, 3, "decode should decrease or stay same")
			},
		},
		{
			name:       "source_decode_zero",
			source:     PhaseReplicaState{Prefill: 4, Decode: 0},
			currentOld: PhaseReplicaState{Prefill: 3, Decode: 0},
			totalSteps: 4,
			checkFunc: func(t *testing.T, result PhaseReplicaState) {
				assert.LessOrEqual(t, result.Prefill, 3, "prefill should decrease or stay same")
				assert.Equal(t, 0, result.Decode, "decode should remain 0")
			},
		},
		{
			name:       "total_steps_zero",
			source:     PhaseReplicaState{Prefill: 4, Decode: 4},
			currentOld: PhaseReplicaState{Prefill: 2, Decode: 2},
			totalSteps: 0,
			checkFunc: func(t *testing.T, result PhaseReplicaState) {
				assert.Equal(t, 0, result.Prefill, "should return zeros when totalSteps is 0")
				assert.Equal(t, 0, result.Decode, "should return zeros when totalSteps is 0")
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

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
	"github.com/stretchr/testify/require"
)

func step(past, new []int) UpdateStep {
	return UpdateStep{Past: past, New: new}
}

func configs(surge, unavailable []int) []RollingUpdateConfig {
	result := make([]RollingUpdateConfig, len(surge))
	for i := range surge {
		result[i] = RollingUpdateConfig{MaxSurge: surge[i], MaxUnavailable: unavailable[i]}
	}
	return result
}

func rolloutCompletes(steps []UpdateStep, target []int) bool {
	if len(steps) == 0 {
		return false
	}
	last := steps[len(steps)-1]
	for i := range target {
		if last.Past[i] != 0 || last.New[i] < target[i] {
			return false
		}
	}
	return true
}

func TestFractionalStepCount(t *testing.T) {
	for _, tc := range []struct {
		name     string
		replicas []int
		want     int
	}{
		{"symmetric", []int{8, 8}, 8},
		{"asymmetric", []int{8, 10}, 10},
		{"zero role", []int{0, 4}, 4},
		{"empty", []int{0, 0}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, fractionalStepCount(tc.replicas))
		})
	}
}

func TestReplicaFractionCoordination(t *testing.T) {
	t.Run("largest replica fraction bounds skew", func(t *testing.T) {
		initial := []int{8, 3}
		steps := ComputeAllSteps(initial, initial, configs([]int{1, 1}, []int{0, 0}))
		require.True(t, rolloutCompletes(steps, initial))

		for i, state := range steps {
			progressP := float64(state.New[0]) / 8
			progressD := float64(state.New[1]) / 3
			assert.InDelta(t, progressP, progressD, 1.0/3.0,
				"step %d exceeds largestReplicaFraction", i)
		}
	})

	t.Run("largest role advances one pod at a time", func(t *testing.T) {
		initial := []int{2, 10}
		steps := ComputeAllSteps(initial, initial, configs([]int{1, 1}, []int{0, 0}))
		for i := 1; i < len(steps); i++ {
			assert.LessOrEqual(t, steps[i].New[1]-steps[i-1].New[1], 1,
				"step %d exceeds smallestReplicaFraction", i)
		}
	})
}

func TestComputeNextStep(t *testing.T) {
	t.Run("complete", func(t *testing.T) {
		result := ComputeNextStep(
			[]int{3, 6}, []int{0, 0}, []int{4, 7}, []int{3, 6},
			configs([]int{1, 1}, []int{0, 0}),
		)
		assert.Nil(t, result)
	})

	t.Run("fresh rollout", func(t *testing.T) {
		result := ComputeNextStep(
			[]int{4, 4}, []int{4, 4}, []int{0, 0}, []int{4, 4},
			configs([]int{1, 1}, []int{0, 0}),
		)
		require.NotNil(t, result)
		assert.Positive(t, result.New[0])
		assert.Positive(t, result.New[1])
	})

	t.Run("new side catches up to released capacity", func(t *testing.T) {
		result := ComputeNextStep(
			[]int{5, 5}, []int{3, 3}, []int{0, 0}, []int{5, 5},
			configs([]int{0, 0}, []int{2, 2}),
		)
		require.NotNil(t, result)
		assert.Equal(t, []int{2, 2}, result.New)
	})
}

func TestComputeAllSteps(t *testing.T) {
	for _, tc := range []struct {
		name               string
		initial, target    []int
		surge, unavailable []int
		want               []UpdateStep
	}{
		{
			name:    "symmetric 8P 4D",
			initial: []int{8, 4}, target: []int{8, 4},
			surge: []int{2, 2}, unavailable: []int{2, 2},
			want: []UpdateStep{
				step([]int{8, 4}, []int{0, 0}),
				step([]int{6, 3}, []int{2, 1}),
				step([]int{4, 2}, []int{4, 2}),
				step([]int{2, 1}, []int{6, 3}),
				step([]int{0, 0}, []int{8, 4}),
			},
		},
		{
			name:    "asymmetric scale",
			initial: []int{10, 2}, target: []int{6, 8},
			surge: []int{2, 2}, unavailable: []int{0, 0},
			want: []UpdateStep{
				step([]int{10, 2}, []int{0, 0}),
				step([]int{9, 2}, []int{2, 3}),
				step([]int{8, 2}, []int{3, 5}),
				step([]int{7, 2}, []int{4, 7}),
				step([]int{6, 2}, []int{5, 8}),
				step([]int{5, 1}, []int{6, 8}),
				step([]int{4, 1}, []int{6, 8}),
				step([]int{3, 1}, []int{6, 8}),
				step([]int{2, 1}, []int{6, 8}),
				step([]int{1, 1}, []int{6, 8}),
				step([]int{0, 0}, []int{6, 8}),
			},
		},
		{
			name:    "zero surge",
			initial: []int{4, 4}, target: []int{4, 4},
			surge: []int{0, 0}, unavailable: []int{2, 2},
			want: []UpdateStep{
				step([]int{4, 4}, []int{0, 0}),
				step([]int{2, 2}, []int{0, 0}),
				step([]int{2, 2}, []int{2, 2}),
				step([]int{0, 0}, []int{2, 2}),
				step([]int{0, 0}, []int{4, 4}),
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := ComputeAllSteps(tc.initial, tc.target, configs(tc.surge, tc.unavailable))
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestPlannerProgress(t *testing.T) {
	t.Run("imbalanced zero surge does not wedge", func(t *testing.T) {
		for _, initial := range [][]int{{1, 4}, {1, 5}} {
			steps := ComputeAllSteps(initial, initial, configs([]int{0, 0}, []int{1, 1}))
			assert.True(t, rolloutCompletes(steps, initial), "rollout stopped for %v", initial)
		}
	})

	t.Run("larger budgets reduce steps", func(t *testing.T) {
		initial := []int{20, 4}
		small := ComputeAllSteps(initial, initial, configs([]int{1, 1}, []int{0, 0}))
		large := ComputeAllSteps(initial, initial, configs([]int{3, 3}, []int{2, 2}))
		require.True(t, rolloutCompletes(small, initial))
		require.True(t, rolloutCompletes(large, initial))
		assert.Less(t, len(large), len(small))
	})

	t.Run("old and new use independent fractions", func(t *testing.T) {
		initial, target := []int{4, 4}, []int{12, 3}
		assert.Equal(t, 4, fractionalStepCount(initial))
		assert.Equal(t, 12, fractionalStepCount(target))
		assert.True(t, rolloutCompletes(
			ComputeAllSteps(initial, target, configs([]int{2, 2}, []int{2, 2})), target))
	})
}

func TestNRolePlannerInvariants(t *testing.T) {
	for _, tc := range []struct {
		name               string
		initial, target    []int
		surge, unavailable []int
	}{
		{
			name:        "three roles symmetric",
			initial:     []int{6, 3, 2},
			target:      []int{6, 3, 2},
			surge:       []int{1, 1, 1},
			unavailable: []int{0, 0, 0},
		},
		{
			name:        "three roles mixed scaling and budgets",
			initial:     []int{3, 8, 2},
			target:      []int{7, 3, 5},
			surge:       []int{2, 1, 1},
			unavailable: []int{0, 1, 1},
		},
		{
			name:        "add a third role",
			initial:     []int{4, 4, 0},
			target:      []int{4, 4, 4},
			surge:       []int{1, 1, 1},
			unavailable: []int{0, 0, 0},
		},
		{
			name:        "remove a fourth role",
			initial:     []int{4, 4, 4, 4},
			target:      []int{4, 4, 4, 0},
			surge:       []int{1, 1, 1, 1},
			unavailable: []int{0, 0, 0, 0},
		},
		{
			name:        "replace one of four roles",
			initial:     []int{4, 4, 0, 3},
			target:      []int{4, 4, 3, 0},
			surge:       []int{1, 1, 1, 1},
			unavailable: []int{0, 0, 0, 0},
		},
		{
			name:        "five roles with extreme imbalance",
			initial:     []int{1, 2, 10, 50, 100},
			target:      []int{1, 2, 10, 50, 100},
			surge:       []int{1, 1, 1, 1, 1},
			unavailable: []int{0, 0, 0, 0, 0},
		},
		{
			name:        "five roles scale up",
			initial:     []int{1, 2, 3, 4, 5},
			target:      []int{5, 6, 7, 8, 9},
			surge:       []int{1, 2, 1, 2, 1},
			unavailable: []int{0, 0, 0, 0, 0},
		},
		{
			name:        "five roles scale down",
			initial:     []int{9, 8, 7, 6, 5},
			target:      []int{5, 4, 3, 2, 1},
			surge:       []int{1, 1, 1, 1, 1},
			unavailable: []int{1, 2, 1, 2, 1},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertPlannerRolloutInvariants(t, tc.initial, tc.target, configs(tc.surge, tc.unavailable))
		})
	}
}

func assertPlannerRolloutInvariants(
	t *testing.T,
	initial, target []int,
	config []RollingUpdateConfig,
) {
	t.Helper()
	require.Len(t, target, len(initial))
	require.Len(t, config, len(initial))

	steps := ComputeAllSteps(initial, target, config)
	require.NotEmpty(t, steps)
	assert.Equal(t, initial, steps[0].Past)
	assert.Equal(t, make([]int, len(initial)), steps[0].New)
	require.True(t, rolloutCompletes(steps, target), "rollout stopped at %+v", steps[len(steps)-1])
	assert.Equal(t, make([]int, len(initial)), steps[len(steps)-1].Past)
	assert.Equal(t, target, steps[len(steps)-1].New)

	for stepIndex, state := range steps {
		require.Len(t, state.Past, len(initial))
		require.Len(t, state.New, len(initial))
		for roleIndex := range initial {
			assert.GreaterOrEqual(t, state.Past[roleIndex], 0, "step %d role %d has negative old replicas", stepIndex, roleIndex)
			assert.GreaterOrEqual(t, state.New[roleIndex], 0, "step %d role %d has negative new replicas", stepIndex, roleIndex)
			assert.LessOrEqual(t, state.Past[roleIndex], initial[roleIndex], "step %d role %d grows the old side", stepIndex, roleIndex)
			assert.LessOrEqual(t, state.New[roleIndex], target[roleIndex], "step %d role %d exceeds its target", stepIndex, roleIndex)

			total := state.Past[roleIndex] + state.New[roleIndex]
			ceiling := max(initial[roleIndex], target[roleIndex]) + config[roleIndex].MaxSurge
			floor := max(0, min(initial[roleIndex], target[roleIndex])-config[roleIndex].MaxUnavailable)
			assert.LessOrEqual(t, total, ceiling, "step %d role %d exceeds its surge ceiling", stepIndex, roleIndex)
			assert.GreaterOrEqual(t, total, floor, "step %d role %d crosses its availability floor", stepIndex, roleIndex)

			if stepIndex > 0 {
				previous := steps[stepIndex-1]
				assert.LessOrEqual(t, state.Past[roleIndex], previous.Past[roleIndex], "step %d role %d reverses old-side progress", stepIndex, roleIndex)
				assert.GreaterOrEqual(t, state.New[roleIndex], previous.New[roleIndex], "step %d role %d reverses new-side progress", stepIndex, roleIndex)
			}
		}
		if stepIndex > 0 {
			previous := steps[stepIndex-1]
			assert.NotEqual(t, previous, state, "step %d makes no progress", stepIndex)
		}
	}
}

func TestNRoleNewSideReplicaFractionCoordination(t *testing.T) {
	target := []int{2, 3, 5, 7, 11}
	steps := ComputeAllSteps(target, target, configs(
		[]int{1, 1, 1, 1, 1},
		[]int{0, 0, 0, 0, 0},
	))
	require.True(t, rolloutCompletes(steps, target))

	for stepIndex, state := range steps {
		minProgress, maxProgress := 1.0, 0.0
		for roleIndex, size := range target {
			progress := float64(state.New[roleIndex]) / float64(size)
			minProgress = min(minProgress, progress)
			maxProgress = max(maxProgress, progress)
		}
		assert.LessOrEqual(t, maxProgress-minProgress, 1.0/2.0+1e-9,
			"step %d exceeds the largest replica fraction", stepIndex)
	}
}

func TestLeastAdvancedStep(t *testing.T) {
	roleSizes := []int{8, 4}
	for _, tc := range []struct {
		name     string
		current  []int
		draining bool
		step     int
	}{
		{"new at zero", []int{0, 0}, false, 0},
		{"new at 25%", []int{2, 1}, false, 1},
		{"new limited by slow role", []int{6, 1}, false, 1},
		{"old at zero", []int{8, 4}, true, 0},
		{"old at 25%", []int{6, 3}, true, 1},
		{"old limited by slow role", []int{4, 3}, true, 1},
		{"old fully drained", []int{0, 0}, true, 4},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.step, leastAdvancedStep(tc.current, roleSizes, 4, tc.draining))
		})
	}
}

func TestWantReplicas(t *testing.T) {
	for _, tc := range []struct {
		size, step, stepCount int
		draining              bool
		want                  int
	}{
		{8, 0, 4, false, 0}, {8, 1, 4, false, 2}, {8, 4, 4, false, 8},
		{8, 0, 4, true, 8}, {8, 1, 4, true, 6}, {8, 4, 4, true, 0},
		{11, 1, 3, false, 4}, {11, 2, 3, false, 8},
		{8, 1, 6, false, 2}, {8, 2, 6, false, 3},
		{8, 0, 0, false, 0}, {8, 0, 0, true, 8},
	} {
		assert.Equal(t, tc.want, wantReplicas(tc.size, tc.step, tc.stepCount, tc.draining))
	}
}

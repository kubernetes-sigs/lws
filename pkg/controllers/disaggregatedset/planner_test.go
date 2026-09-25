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
	"fmt"
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

func readySnapshot(
	initialOld, currentOld, currentNew, targetNew RoleReplicaState,
	config []RollingUpdateConfig,
) rolloutSnapshot {
	snapshot := make(rolloutSnapshot, len(initialOld))
	for i := range initialOld {
		snapshot[i] = roleRolloutSnapshot{
			InitialOldReplicas:    initialOld[i],
			ActiveOldSpecReplicas: currentOld[i],
			OldSpecReplicas:       currentOld[i],
			OldReadyReplicas:      currentOld[i],
			NewSpecReplicas:       currentNew[i],
			NewReadyReplicas:      currentNew[i],
			NewTargetReplicas:     targetNew[i],
			Config:                config[i],
		}
	}
	return snapshot
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

func TestComputeNextStep(t *testing.T) {
	t.Run("complete", func(t *testing.T) {
		result := ComputeNextStep(readySnapshot(
			[]int{3, 6}, []int{0, 0}, []int{4, 7}, []int{3, 6},
			configs([]int{1, 1}, []int{0, 0}),
		), nil)
		assert.Nil(t, result)
	})

	t.Run("fresh rollout", func(t *testing.T) {
		result := ComputeNextStep(readySnapshot(
			[]int{4, 4}, []int{4, 4}, []int{0, 0}, []int{4, 4},
			configs([]int{1, 1}, []int{0, 0}),
		), nil)
		require.NotNil(t, result)
		assert.Positive(t, result.New[0])
		assert.Positive(t, result.New[1])
	})

	t.Run("new side catches up to released capacity", func(t *testing.T) {
		result := ComputeNextStep(readySnapshot(
			[]int{5, 5}, []int{3, 3}, []int{0, 0}, []int{5, 5},
			configs([]int{0, 0}, []int{2, 2}),
		), nil)
		require.NotNil(t, result)
		assert.Equal(t, []int{2, 2}, result.New)
	})

	t.Run("allows progress up to the fractional window", func(t *testing.T) {
		result := ComputeNextStep(readySnapshot(
			[]int{2, 2}, []int{2, 2}, []int{0, 1}, []int{2, 2},
			configs([]int{1, 1}, []int{0, 0}),
		), nil)
		require.NotNil(t, result)
		assert.Equal(t, []int{2, 1}, result.Past,
			"Decode may drain one replica while Prefill remains at the other edge of the window")
		assert.Equal(t, []int{1, 1}, result.New)
	})

	t.Run("phase target does not lower the global availability floor", func(t *testing.T) {
		snapshot := rolloutSnapshot{{
			InitialOldReplicas: 6, ActiveOldSpecReplicas: 3, OldSpecReplicas: 3, OldReadyReplicas: 2,
			NewSpecReplicas: 4, NewReadyReplicas: 4, NewTargetReplicas: 6,
			Config: RollingUpdateConfig{MaxSurge: 1},
		}}
		assert.Nil(t, ComputeNextStep(snapshot, RoleReplicaState{1}))
	})

	t.Run("pipelines through a narrow window while the first batch is unready", func(t *testing.T) {
		snapshot := readySnapshot(
			[]int{20, 20}, []int{18, 18}, []int{2, 2}, []int{20, 20},
			configs([]int{2, 2}, []int{2, 2}),
		)
		for i := range snapshot {
			snapshot[i].NewReadyReplicas = 0
		}

		result := ComputeNextStep(snapshot, nil)

		require.NotNil(t, result)
		assert.Equal(t, RoleReplicaState{18, 18}, result.Past, "availability floors prevent another old drain")
		assert.Equal(t, RoleReplicaState{4, 4}, result.New, "a second batch is issued inside the 1/20 window")
	})
}

func TestComputeAllSteps(t *testing.T) {
	want := []UpdateStep{
		step([]int{10, 2}, []int{0, 0}),
		step([]int{9, 2}, []int{2, 2}),
		step([]int{8, 2}, []int{3, 4}),
		step([]int{7, 2}, []int{4, 6}),
		step([]int{6, 2}, []int{5, 8}),
		step([]int{5, 1}, []int{6, 8}),
		step([]int{4, 1}, []int{6, 8}),
		step([]int{3, 1}, []int{6, 8}),
		step([]int{2, 1}, []int{6, 8}),
		step([]int{1, 1}, []int{6, 8}),
		step([]int{0, 0}, []int{6, 8}),
	}
	assert.Equal(t, want, ComputeAllSteps(
		[]int{10, 2}, []int{6, 8}, configs([]int{2, 2}, []int{0, 0})))
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
		{"three roles symmetric", []int{6, 3, 2}, []int{6, 3, 2}, []int{1, 1, 1}, []int{0, 0, 0}},
		{"three roles mixed scaling and budgets", []int{3, 8, 2}, []int{7, 3, 5}, []int{2, 1, 1}, []int{0, 1, 1}},
		{"add a third role", []int{4, 4, 0}, []int{4, 4, 4}, []int{1, 1, 1}, []int{0, 0, 0}},
		{"remove a fourth role", []int{4, 4, 4, 4}, []int{4, 4, 4, 0}, []int{1, 1, 1, 1}, []int{0, 0, 0, 0}},
		{"replace one of four roles", []int{4, 4, 0, 3}, []int{4, 4, 3, 0}, []int{1, 1, 1, 1}, []int{0, 0, 0, 0}},
		{"five roles with extreme imbalance", []int{1, 2, 10, 50, 100}, []int{1, 2, 10, 50, 100}, []int{1, 1, 1, 1, 1}, []int{0, 0, 0, 0, 0}},
		{"five roles scale up", []int{1, 2, 3, 4, 5}, []int{5, 6, 7, 8, 9}, []int{1, 2, 1, 2, 1}, []int{0, 0, 0, 0, 0}},
		{"five roles scale down", []int{9, 8, 7, 6, 5}, []int{5, 4, 3, 2, 1}, []int{1, 1, 1, 1, 1}, []int{1, 2, 1, 2, 1}},
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

		oldProgress := make(RoleReplicaState, len(initial))
		for i := range initial {
			oldProgress[i] = max(0, initial[i]-min(state.Past[i], initial[i]))
		}
		assertProgressWithinFractionalWindow(t, initial, oldProgress, stepIndex, "old")
		assertProgressWithinFractionalWindow(t, target, state.New, stepIndex, "new")
	}
}

func assertProgressWithinFractionalWindow(
	t *testing.T,
	roleReplicaCounts, progress RoleReplicaState,
	stepIndex int,
	side string,
) {
	t.Helper()
	minProgress, maxProgress := 1.0, 0.0
	minPositiveRoleReplicaCount := 0
	for i, roleReplicaCount := range roleReplicaCounts {
		if roleReplicaCount <= 0 {
			continue
		}
		roleProgress := float64(progress[i]) / float64(roleReplicaCount)
		minProgress = min(minProgress, roleProgress)
		maxProgress = max(maxProgress, roleProgress)
		if minPositiveRoleReplicaCount == 0 || roleReplicaCount < minPositiveRoleReplicaCount {
			minPositiveRoleReplicaCount = roleReplicaCount
		}
	}
	if minPositiveRoleReplicaCount == 0 {
		return
	}
	assert.LessOrEqual(t,
		maxProgress-minProgress,
		1.0/float64(minPositiveRoleReplicaCount)+1e-9,
		"step %d exceeds the %s-side fractional window", stepIndex, side,
	)
}

func TestLeastAdvancedStep(t *testing.T) {
	roleReplicaCounts := []int{8, 4}
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
			assert.Equal(t, tc.step, leastAdvancedStep(tc.current, roleReplicaCounts, 4, tc.draining))
		})
	}
}

func TestReplicasAtFractionalStep(t *testing.T) {
	for _, tc := range []struct {
		roleReplicaCount, step, stepCount int
		draining                          bool
		want                              int
	}{
		{8, 0, 4, false, 0}, {8, 1, 4, false, 2}, {8, 4, 4, false, 8},
		{8, 0, 4, true, 8}, {8, 1, 4, true, 6}, {8, 4, 4, true, 0},
		{11, 1, 3, false, 4}, {11, 2, 3, false, 8},
		{8, 1, 6, false, 2}, {8, 2, 6, false, 3},
		{8, 0, 0, false, 0}, {8, 0, 0, true, 8},
	} {
		assert.Equal(t, tc.want, replicasAtFractionalStep(tc.roleReplicaCount, tc.step, tc.stepCount, tc.draining))
	}
}

func TestFractionalWindowBounds(t *testing.T) {
	counts := RoleReplicaState{8, 4}
	assert.Equal(t, RoleReplicaState{4, 1},
		boundGrowingRoleTargetsToWindow(RoleReplicaState{0, 1}, counts, RoleReplicaState{8, 1}))
	assert.Equal(t, RoleReplicaState{8, 3},
		boundDrainingRoleTargetsToWindow(counts, counts, RoleReplicaState{8, 2}))
	assert.Equal(t, RoleReplicaState{6, 1},
		boundGrowingRoleTargetsToWindow(RoleReplicaState{6, 1}, counts, RoleReplicaState{8, 1}), "must not reverse existing growth")
	assert.Equal(t, RoleReplicaState{2, 3},
		boundDrainingRoleTargetsToWindow(RoleReplicaState{2, 3}, counts, RoleReplicaState{0, 3}), "must not reverse existing drain")
}

func TestHardNewReplicaLimits(t *testing.T) {
	plannerTargets := []int{8, 4}
	snapshot := rolloutSnapshot{
		{
			InitialOldReplicas: 8, ActiveOldSpecReplicas: 6, OldSpecReplicas: 6, NewSpecReplicas: 3, NewReadyReplicas: 0, NewTargetReplicas: 8,
			Config: RollingUpdateConfig{MaxSurge: 2, MaxUnavailable: 2},
		},
		{
			InitialOldReplicas: 4, ActiveOldSpecReplicas: 3, OldSpecReplicas: 3, NewSpecReplicas: 2, NewReadyReplicas: 0, NewTargetReplicas: 4,
			Config: RollingUpdateConfig{MaxSurge: 2, MaxUnavailable: 2},
		},
	}

	bounded := boundNewReplicaTargetsByHardLimits(snapshot, plannerTargets)
	assert.Equal(t, 4, bounded[0],
		"prefill is capped by both pending allowance and remaining surge")
	assert.Equal(t, 2, bounded[1],
		"decode cannot grow while its pending allowance is fully consumed")

	snapshot[0].OldSpecReplicas = 5
	snapshot[0].NewReadyReplicas = 1
	snapshot[1].OldSpecReplicas = 2
	snapshot[1].NewReadyReplicas = 1
	bounded = boundNewReplicaTargetsByHardLimits(snapshot, plannerTargets)
	assert.Equal(t, 5, bounded[0],
		"readiness plus released surge headroom opens the next shared fraction")
	assert.Equal(t, 3, bounded[1])
}

func TestComputeNextStepUsesReadySafeDrain(t *testing.T) {
	snapshot := rolloutSnapshot{
		{InitialOldReplicas: 4, ActiveOldSpecReplicas: 1, OldSpecReplicas: 1, OldReadyReplicas: 1, NewSpecReplicas: 4, NewReadyReplicas: 2, NewTargetReplicas: 4,
			Config: RollingUpdateConfig{MaxSurge: 1, MaxUnavailable: 1}},
		{InitialOldReplicas: 1, ActiveOldSpecReplicas: 1, OldSpecReplicas: 1, OldReadyReplicas: 1, NewSpecReplicas: 1, NewReadyReplicas: 1, NewTargetReplicas: 1,
			Config: RollingUpdateConfig{MaxSurge: 1, MaxUnavailable: 1}},
	}
	step := ComputeNextStep(snapshot, nil)
	require.NotNil(t, step)
	assert.Equal(t, []int{1, 0}, step.Past)
}

func TestComputeNextStepZeroSurgeUsesAvailableSlot(t *testing.T) {
	snapshot := rolloutSnapshot{{
		InitialOldReplicas:    4,
		ActiveOldSpecReplicas: 4,
		OldSpecReplicas:       4,
		OldReadyReplicas:      4,
		NewTargetReplicas:     4,
		Config:                RollingUpdateConfig{MaxSurge: 0, MaxUnavailable: 1},
	}}
	step := ComputeNextStep(snapshot, nil)
	require.NotNil(t, step)
	assert.Equal(t, RoleReplicaState{3}, step.Past,
		"one availability slot is used to make room for a replacement")

	// The same configured budget is no longer usable while one old replica is
	// unavailable. Readiness, rather than another forced drain, must unblock it.
	snapshot[0].OldReadyReplicas = 3
	assert.Nil(t, ComputeNextStep(snapshot, nil))

	snapshot[0].OldReadyReplicas = 4
	step = ComputeNextStep(snapshot, nil)
	require.NotNil(t, step, "the slot becomes available again when the old replica recovers")
	assert.Equal(t, RoleReplicaState{3}, step.Past)
}

func TestExecutorStateTransitionsExhaustive(t *testing.T) {
	configCases := [][]RollingUpdateConfig{
		configs([]int{1, 1}, []int{0, 0}),
		configs([]int{0, 0}, []int{1, 1}),
		configs([]int{1, 1}, []int{1, 1}),
		configs([]int{2, 2}, []int{1, 1}),
		configs([]int{2, 1}, []int{1, 2}),
	}

	for initialP := 0; initialP <= 5; initialP++ {
		for initialD := 0; initialD <= 5; initialD++ {
			for targetP := 0; targetP <= 5; targetP++ {
				for targetD := 0; targetD <= 5; targetD++ {
					initial := []int{initialP, initialD}
					target := []int{targetP, targetD}
					for configIndex, config := range configCases {
						scenario := fmt.Sprintf("initial=%v target=%v config=%d", initial, target, configIndex)
						state := make(rolloutSnapshot, len(initial))
						for i := range initial {
							state[i] = roleRolloutSnapshot{
								InitialOldReplicas:    initial[i],
								ActiveOldSpecReplicas: initial[i],
								OldSpecReplicas:       initial[i],
								OldReadyReplicas:      initial[i],
								NewTargetReplicas:     target[i],
								Config:                config[i],
							}
						}

						completed := false
						for iteration := 0; iteration < 100; iteration++ {
							assertRolloutSnapshotInvariants(t, state, scenario, iteration)
							if isRolloutSpecComplete(state) {
								if isRolloutReady(state) {
									completed = true
									break
								}
								if !makeOneNewReplicaReady(state) {
									t.Fatalf("complete Spec state cannot become Ready: %s", scenario)
								}
								continue
							}

							step := ComputeNextStep(state, nil)
							if step == nil {
								if !makeOneNewReplicaReady(state) {
									t.Fatalf("blocked without pending work: %s state=%+v", scenario, state)
								}
								continue
							}

							changed := false
							for i, roleState := range state {
								drain := min(max(0, roleState.ActiveOldSpecReplicas-step.Past[i]), maxSafeDrain(roleState))
								if drain > 0 {
									roleState.ActiveOldSpecReplicas -= drain
									roleState.OldSpecReplicas -= drain
									roleState.OldReadyReplicas = min(roleState.OldReadyReplicas, roleState.OldSpecReplicas)
									changed = true
								}
								if step.New[i] > roleState.NewSpecReplicas {
									roleState.NewSpecReplicas = step.New[i]
									changed = true
								}
								state[i] = roleState
							}
							if !changed && !makeOneNewReplicaReady(state) {
								t.Fatalf("progress plan made no mutation and has no pending work: %s state=%+v", scenario, state)
							}
						}
						if !completed {
							t.Fatalf("rollout did not complete within transition bound: %s state=%+v", scenario, state)
						}
					}
				}
			}
		}
	}
}

func makeOneNewReplicaReady(state rolloutSnapshot) bool {
	for i, roleState := range state {
		if roleState.NewReadyReplicas < roleState.NewSpecReplicas {
			roleState.NewReadyReplicas++
			state[i] = roleState
			return true
		}
	}
	return false
}

func assertRolloutSnapshotInvariants(t *testing.T, state rolloutSnapshot, scenario string, iteration int) {
	t.Helper()
	targets := make(RoleReplicaState, len(state))
	newReplicas := make(RoleReplicaState, len(state))
	budgetSteps := 0
	for _, roleState := range state {
		budgetSteps = max(budgetSteps, roleState.InitialOldReplicas, roleState.NewTargetReplicas)
	}
	for i, roleState := range state {
		targets[i] = roleState.NewTargetReplicas
		newReplicas[i] = roleState.NewSpecReplicas
		roleReplicaCount := max(roleState.InitialOldReplicas, roleState.NewTargetReplicas)
		ceiling := roleReplicaCount + roleState.Config.MaxSurge
		floor := max(0, min(roleState.InitialOldReplicas, roleState.NewTargetReplicas)-roleState.Config.MaxUnavailable)
		if roleState.OldSpecReplicas+roleState.NewSpecReplicas > ceiling {
			t.Fatalf("surge ceiling violated at iteration %d for %s role=%d: state=%+v", iteration, scenario, i, roleState)
		}
		if roleState.OldReadyReplicas+roleState.NewReadyReplicas < floor {
			t.Fatalf("availability floor violated at iteration %d for %s role=%d: state=%+v", iteration, scenario, i, roleState)
		}
		pendingAllowance := projectBudget(roleReplicaCount, roleState.Config.MaxSurge+roleState.Config.MaxUnavailable, budgetSteps)
		if roleState.NewSpecReplicas-roleState.NewReadyReplicas > pendingAllowance {
			t.Fatalf("pending allowance violated at iteration %d for %s role=%d: state=%+v", iteration, scenario, i, roleState)
		}
	}
	assertProgressWithinFractionalWindow(t, targets, newReplicas, iteration, scenario+" new")
}

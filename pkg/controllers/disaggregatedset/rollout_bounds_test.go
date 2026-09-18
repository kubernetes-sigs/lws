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
)

func TestAvailabilityBounds(t *testing.T) {
	assert.Equal(t, 2, committedReadyReplicas(makeLWS(withReplicas(2), withReadyReplicas(4))))
	assert.Equal(t, 1, committedReadyReplicas(makeLWS(withReplicas(3), withReadyReplicas(1))))
	assert.Zero(t, committedReadyReplicas(nil))
	assert.Equal(t, 1, maxSafeDrain(roleRolloutSnapshot{
		InitialOldReplicas: 3, OldSpecReplicas: 3, OldReadyReplicas: 3, NewReadyReplicas: 1, NewTargetReplicas: 4,
	}))
	assert.Equal(t, 4, maxSafeDrain(roleRolloutSnapshot{
		InitialOldReplicas: 4, OldSpecReplicas: 4, OldReadyReplicas: 0, NewReadyReplicas: 4, NewTargetReplicas: 4,
	}), "unavailable old replicas can drain once the new revision satisfies the floor")
	assert.False(t, isRolloutReady(rolloutSnapshot{{NewSpecReplicas: 4, NewReadyReplicas: 2, NewTargetReplicas: 4}}))
	assert.True(t, isRolloutReady(rolloutSnapshot{{NewSpecReplicas: 4, NewReadyReplicas: 4, NewTargetReplicas: 4}}))
}

func TestHardNewReplicaLimits(t *testing.T) {
	plannerTargets := []int{8, 4}
	snapshot := rolloutSnapshot{
		{
			InitialOldReplicas: 8, OldSpecReplicas: 6, NewSpecReplicas: 3, NewReadyReplicas: 0, NewTargetReplicas: 8,
			Config: RollingUpdateConfig{MaxSurge: 2, MaxUnavailable: 2},
		},
		{
			InitialOldReplicas: 4, OldSpecReplicas: 3, NewSpecReplicas: 2, NewReadyReplicas: 0, NewTargetReplicas: 4,
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

func TestFractionalCoordinationWindowUsesKEPExample(t *testing.T) {
	snapshot := rolloutSnapshot{
		{NewTargetReplicas: 8}, // Prefill
		{NewTargetReplicas: 4}, // Decode
	}

	// Decode is the least-advanced role at 1/4. The window is also 1/4 wide,
	// so Prefill may advance no farther than 1/2, or 4/8.
	bounded := boundNewReplicaTargetsToCoordinationWindow(snapshot, RoleReplicaState{8, 1})

	assert.Equal(t, RoleReplicaState{4, 1}, bounded)
}

func TestFractionalCoordinationWindowPreservesLargestReplicaFraction(t *testing.T) {
	plannerTargets := []int{2, 2}
	snapshot := rolloutSnapshot{
		{
			NewSpecReplicas: 1, NewReadyReplicas: 1, NewTargetReplicas: 2,
			Config: RollingUpdateConfig{MaxSurge: 1},
		},
		{
			NewSpecReplicas: 1, NewReadyReplicas: 0, NewTargetReplicas: 3,
			Config: RollingUpdateConfig{MaxSurge: 1},
		},
	}

	hardBounded := boundNewReplicaTargetsByHardLimits(snapshot, plannerTargets)
	bounded := boundNewReplicaTargetsToCoordinationWindow(snapshot, hardBounded)
	assert.Equal(t, 1, bounded[0],
		"100%% versus 33%% would exceed largestReplicaFraction=1/2")
	assert.Equal(t, 1, bounded[1])

	snapshot[1].NewReadyReplicas = 1
	hardBounded = boundNewReplicaTargetsByHardLimits(snapshot, plannerTargets)
	bounded = boundNewReplicaTargetsToCoordinationWindow(snapshot, hardBounded)
	assert.Equal(t, 2, bounded[0])
	assert.Equal(t, 2, bounded[1])
}

func TestEnsureExecutableStepUsesSafeDrain(t *testing.T) {
	snapshot := rolloutSnapshot{
		{InitialOldReplicas: 4, OldSpecReplicas: 1, OldReadyReplicas: 1, NewSpecReplicas: 4, NewReadyReplicas: 2, NewTargetReplicas: 4,
			Config: RollingUpdateConfig{MaxSurge: 1, MaxUnavailable: 1}},
		{InitialOldReplicas: 1, OldSpecReplicas: 1, OldReadyReplicas: 1, NewSpecReplicas: 1, NewReadyReplicas: 1, NewTargetReplicas: 1,
			Config: RollingUpdateConfig{MaxSurge: 1, MaxUnavailable: 1}},
	}
	step := &UpdateStep{Past: []int{1, 1}, New: []int{4, 1}}

	ensureExecutableStep(snapshot, step)

	assert.Equal(t, []int{1, 0}, step.Past)
}

func TestExecutorStateTransitionsExhaustive(t *testing.T) {
	roles := []string{"p", "d"}
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
						state := make(rolloutSnapshot, len(roles))
						for i := range roles {
							state[i] = roleRolloutSnapshot{
								InitialOldReplicas: initial[i],
								OldSpecReplicas:    initial[i],
								OldReadyReplicas:   initial[i],
								NewTargetReplicas:  target[i],
								Config:             config[i],
							}
						}

						completed := false
						for iteration := 0; iteration < 100; iteration++ {
							assertRolloutSnapshotInvariants(t, roles, state, scenario, iteration)
							initialOld, currentOld, currentNew, targetNew := plannerInputs(state)
							if isComplete(currentOld, currentNew, targetNew) {
								if isRolloutReady(state) {
									completed = true
									break
								}
								if !makeOneNewReplicaReady(state) {
									t.Fatalf("complete Spec state cannot become Ready: %s", scenario)
								}
								continue
							}

							step := ComputeNextStep(initialOld, currentOld, currentNew, targetNew, config)
							if step == nil {
								if !makeOneNewReplicaReady(state) {
									t.Fatalf("blocked without pending work: %s state=%+v", scenario, state)
								}
								continue
							}

							boundedNew := boundNewReplicaTargetsByHardLimits(state, step.New)
							boundedNew = boundNewReplicaTargetsToCoordinationWindow(state, boundedNew)
							step.New = boundedNew
							ensureExecutableStep(state, step)
							changed := false
							for i, roleState := range state {
								drain := min(max(0, roleState.OldSpecReplicas-step.Past[i]), maxSafeDrain(roleState))
								if drain > 0 {
									roleState.OldSpecReplicas -= drain
									roleState.OldReadyReplicas = min(roleState.OldReadyReplicas, roleState.OldSpecReplicas)
									changed = true
								}
								if boundedNew[i] > roleState.NewSpecReplicas {
									roleState.NewSpecReplicas = boundedNew[i]
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

func assertRolloutSnapshotInvariants(t *testing.T, roles []string, state rolloutSnapshot, scenario string, iteration int) {
	t.Helper()
	newMinProgress, newMaxProgress := 1.0, 0.0
	minPositiveTarget := 0
	budgetSteps := 0
	for _, roleState := range state {
		budgetSteps = max(budgetSteps, roleState.InitialOldReplicas, roleState.NewTargetReplicas)
	}
	for i, roleState := range state {
		roleReplicaCount := max(roleState.InitialOldReplicas, roleState.NewTargetReplicas)
		ceiling := roleReplicaCount + roleState.Config.MaxSurge
		floor := max(0, min(roleState.InitialOldReplicas, roleState.NewTargetReplicas)-roleState.Config.MaxUnavailable)
		if roleState.OldSpecReplicas+roleState.NewSpecReplicas > ceiling {
			t.Fatalf("surge ceiling violated at iteration %d for %s role=%s: state=%+v", iteration, scenario, roles[i], roleState)
		}
		if roleState.OldReadyReplicas+roleState.NewReadyReplicas < floor {
			t.Fatalf("availability floor violated at iteration %d for %s role=%s: state=%+v", iteration, scenario, roles[i], roleState)
		}
		pendingAllowance := projectBudget(roleReplicaCount, roleState.Config.MaxSurge+roleState.Config.MaxUnavailable, budgetSteps)
		if roleState.NewSpecReplicas-roleState.NewReadyReplicas > pendingAllowance {
			t.Fatalf("pending allowance violated at iteration %d for %s role=%s: state=%+v", iteration, scenario, roles[i], roleState)
		}
		if roleState.NewTargetReplicas > 0 {
			progress := float64(roleState.NewSpecReplicas) / float64(roleState.NewTargetReplicas)
			newMinProgress = min(newMinProgress, progress)
			newMaxProgress = max(newMaxProgress, progress)
			if minPositiveTarget == 0 || roleState.NewTargetReplicas < minPositiveTarget {
				minPositiveTarget = roleState.NewTargetReplicas
			}
		}
	}
	if minPositiveTarget > 0 && newMaxProgress-newMinProgress > 1/float64(minPositiveTarget)+1e-9 {
		t.Fatalf("fractional coordination window violated at iteration %d for %s: min=%f max=%f", iteration, scenario, newMinProgress, newMaxProgress)
	}
}

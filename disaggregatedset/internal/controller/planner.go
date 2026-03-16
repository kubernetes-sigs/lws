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

// Package controller provides rolling update planning and execution for DisaggregatedSet.
//
// # Rolling Update Algorithm
//
// The planner uses a linear scaling function that approximates discrete steps of a
// linear interpolation between source and target replica counts:
//
//	newAtStep(i) = ceil(i * target / totalSteps)    // scale up: 0 → target
//	oldAtStep(i) = source - floor(i * source / totalSteps)  // scale down: source → 0
//
// Since the controller is stateless, we compute only the needed step directly:
// derive the current step index from observed replicas, then compute the next step's target.
//
// The complexity comes from:
//   - Decoupling: each step changes EITHER old OR new, not both
//   - Surge constraints: old + new <= target + maxSurge
//   - N dimensions: all phases must stay coordinated
package controller

import (
	"math"
)

// UpdateStep represents a single step in the rolling update plan.
// It tracks the replica counts for both old (past) and new deployments
// for N phases (slice-based for flexibility).
type UpdateStep struct {
	Past []int
	New  []int
}

// PhaseReplicaState holds the replica counts for N phases.
// Used for source, current, and target state in rolling update planning.
type PhaseReplicaState = []int

// RollingUpdateConfig holds the rolling update constraints per phase.
type RollingUpdateConfig struct {
	MaxSurge       []int
	MaxUnavailable []int
}

// DefaultRollingUpdateConfig returns the default rolling update config for N phases (maxSurge=1, maxUnavailable=0).
func DefaultRollingUpdateConfig(numPhases int) RollingUpdateConfig {
	surge := make([]int, numPhases)
	unavail := make([]int, numPhases)
	for i := 0; i < numPhases; i++ {
		surge[i] = 1
		unavail[i] = 0
	}
	return RollingUpdateConfig{
		MaxSurge:       surge,
		MaxUnavailable: unavail,
	}
}

// batchSize returns the batch size: surge if > 0, else max(1, unavailable).
func batchSize(maxSurge, maxUnavailable int) int {
	if maxSurge > 0 {
		return maxSurge
	}
	return max(1, maxUnavailable)
}

// computeTotalSteps computes the total number of steps for the rollout.
// Based on the maximum replicas (source or target) for each dimension,
// divided by the batch size (surge or unavailable), taking the max across dimensions.
func computeTotalSteps(source, target PhaseReplicaState, config RollingUpdateConfig) int {
	totalSteps := 0
	numPhases := len(source)
	for i := 0; i < numPhases; i++ {
		maxReplicas := max(source[i], target[i], 0)
		phaseBatchSize := batchSize(config.MaxSurge[i], config.MaxUnavailable[i])
		phaseSteps := (maxReplicas + phaseBatchSize - 1) / phaseBatchSize
		totalSteps = max(totalSteps, phaseSteps)
	}
	return totalSteps
}

// computeNextNewReplicas computes the next new replica count for scale-up.
//
// Linear interpolation: newAtStep(i) = ceil(i * target / totalSteps)
//
// Uses min step index across dimensions to keep phases in sync.
func computeNextNewReplicas(target, currentNew PhaseReplicaState, totalSteps int) PhaseReplicaState {
	numPhases := len(target)
	if totalSteps == 0 {
		result := make([]int, numPhases)
		copy(result, target)
		return result
	}

	// Step 1: figure out which step we're at based on current replicas
	stepIndex := func(current, targetVal int) int {
		if targetVal == 0 {
			return totalSteps
		}
		return int(float64(current) * float64(totalSteps) / float64(targetVal))
	}

	minStepIdx := totalSteps
	for i := 0; i < numPhases; i++ {
		stepIdx := stepIndex(currentNew[i], target[i])
		minStepIdx = min(minStepIdx, stepIdx)
	}
	nextStepIdx := minStepIdx + 1

	// Step 2: compute how many replicas we should have at the next step
	computeNew := func(targetVal, currentVal int) int {
		progress := float64(nextStepIdx) * float64(targetVal) / float64(totalSteps)
		computed := min(int(math.Ceil(progress)), targetVal)
		return max(computed, currentVal) // never decrease
	}

	result := make([]int, numPhases)
	for i := 0; i < numPhases; i++ {
		result[i] = computeNew(target[i], currentNew[i])
	}
	return result
}

// computeNextOldReplicas computes the next old replica count for scale-down.
//
// Linear interpolation: oldAtStep(i) = source - floor(i * source / totalSteps)
//
// Uses max step index across dimensions to ensure all phases drain together.
func computeNextOldReplicas(source, currentOld PhaseReplicaState, totalSteps int) PhaseReplicaState {
	numPhases := len(source)
	if totalSteps == 0 {
		return make([]int, numPhases)
	}

	// Step 1: figure out which step we're at based on how many replicas were removed
	// Skip phases with source=0 (new phases) - they don't affect drain timing
	stepIndex := func(removed, sourceVal int) int {
		if sourceVal == 0 {
			return 0 // New phases don't contribute to drain step calculation
		}
		return int(float64(removed) * float64(totalSteps) / float64(sourceVal))
	}

	maxStepIdx := 0
	for i := 0; i < numPhases; i++ {
		if source[i] == 0 {
			continue // Skip new phases
		}
		removed := source[i] - currentOld[i]
		stepIdx := stepIndex(removed, source[i])
		maxStepIdx = max(maxStepIdx, stepIdx)
	}
	nextStepIdx := maxStepIdx + 1

	// Step 2: compute how many replicas should remain at the next step
	computeOld := func(sourceVal, currentVal int) int {
		progress := float64(nextStepIdx) * float64(sourceVal) / float64(totalSteps)
		computed := max(0, sourceVal-int(math.Floor(progress)))
		return min(computed, currentVal) // never increase
	}

	result := make([]int, numPhases)
	for i := 0; i < numPhases; i++ {
		result[i] = computeOld(source[i], currentOld[i])
	}
	return result
}

// correctAbnormalState corrects abnormal states where old replicas exceed the inferred source.
// This shouldn't happen in normal rollouts (old starts at source and only decreases),
// but can occur from interrupted rollouts or manual intervention.
// Returns a correction step if needed, nil otherwise.
func correctAbnormalState(currentOld, currentNew, source PhaseReplicaState) *UpdateStep {
	numPhases := len(source)
	expectedOld := make([]int, numPhases)
	needsCorrection := false
	for i := 0; i < numPhases; i++ {
		expectedOld[i] = min(source[i], currentOld[i])
		if currentOld[i] > expectedOld[i] {
			needsCorrection = true
		}
	}

	if needsCorrection {
		newCopy := make([]int, numPhases)
		copy(newCopy, currentNew)
		return &UpdateStep{
			Past: expectedOld,
			New:  newCopy,
		}
	}
	return nil
}

// ComputeNextStep computes the next step in the rolling update.
// Returns nil when no more steps are needed (current state equals target).
// Each step changes EITHER old OR new replicas, not both (decoupled steps).
//
// Parameters:
//   - source: the original/initial replica counts before the rolling update started
//   - currentOld: current replica counts for old workloads (what's still running)
//   - currentNew: current replica counts for new workloads (what's already deployed)
//   - targetNew: target replica counts for new workloads (from spec)
//   - config: rolling update constraints (maxSurge, maxUnavailable)
func ComputeNextStep(source, currentOld, currentNew, targetNew PhaseReplicaState, config RollingUpdateConfig) *UpdateStep {
	numPhases := len(source)

	// If already at target (no old replicas, new at target), no more steps needed
	allOldZero := true
	allNewAtTarget := true
	for i := 0; i < numPhases; i++ {
		if currentOld[i] != 0 {
			allOldZero = false
		}
		if currentNew[i] < targetNew[i] {
			allNewAtTarget = false
		}
	}
	if allOldZero && allNewAtTarget {
		return nil
	}

	totalNumSteps := computeTotalSteps(source, targetNew, config)
	if totalNumSteps == 0 {
		return nil
	}

	correction := correctAbnormalState(currentOld, currentNew, source)
	if correction != nil {
		return correction
	}

	// If new replicas are at target but old replicas still exist, drain them
	if allNewAtTarget {
		return &UpdateStep{
			Past: make([]int, numPhases),
			New:  currentNew,
		}
	}

	// Compute what new replicas should be at the next step
	nextNewState := computeNextNewReplicas(targetNew, currentNew, totalNumSteps)

	needsScaleUp := false
	for i := 0; i < numPhases; i++ {
		if nextNewState[i] > currentNew[i] {
			needsScaleUp = true
			break
		}
	}

	// Check surge constraint for scale-up (old + new <= max(source, target) + surge)
	// Use max(source, target) so that dimensions scaling down (source > target) don't
	// block scale-up — the system already runs source replicas, surge is relative to that.
	// Skip removed phases (target=0) - they don't need surge protection
	surgeOK := true
	for i := 0; i < numPhases; i++ {
		if targetNew[i] == 0 {
			continue // Removed phases just drain, no surge constraint
		}
		if currentOld[i]+nextNewState[i] > max(source[i], targetNew[i])+config.MaxSurge[i] {
			surgeOK = false
			break
		}
	}

	if needsScaleUp && surgeOK {
		// Scale up new replicas (keep old unchanged)
		return &UpdateStep{
			Past: currentOld,
			New:  nextNewState,
		}
	}

	// Scale down: first try proportional drain
	nextOldState := computeNextOldReplicas(source, currentOld, totalNumSteps)

	// Enforce maxUnavailable constraint: old + new >= target - maxUnavailable per phase.
	// Only enforce when source >= target for that phase — when scaling up (source < target),
	// the system never had enough replicas to maintain the target level.
	minOld := make([]int, numPhases)
	for i := 0; i < numPhases; i++ {
		if source[i] >= targetNew[i] {
			minOld[i] = max(0, targetNew[i]-config.MaxUnavailable[i]-currentNew[i])
		}
		nextOldState[i] = max(nextOldState[i], minOld[i])
	}

	needsScaleDown := false
	for i := 0; i < numPhases; i++ {
		if nextOldState[i] < currentOld[i] {
			needsScaleDown = true
			break
		}
	}

	if needsScaleDown {
		// Scale down old replicas (keep new unchanged)
		return &UpdateStep{
			Past: nextOldState,
			New:  currentNew,
		}
	}

	// Proportional drain didn't help - surge is blocking and old step is behind.
	// Drain exactly what's needed to allow the next scale-up.
	if needsScaleUp {
		drainedOld := make([]int, numPhases)
		needsDrain := false
		for i := 0; i < numPhases; i++ {
			maxOld := max(source[i], targetNew[i]) + config.MaxSurge[i] - nextNewState[i]
			drainedOld[i] = max(0, min(currentOld[i], maxOld))
			// Enforce maxUnavailable constraint on fallback drain path
			drainedOld[i] = max(drainedOld[i], minOld[i])
			if drainedOld[i] < currentOld[i] {
				needsDrain = true
			}
		}

		if needsDrain {
			return &UpdateStep{
				Past: drainedOld,
				New:  currentNew,
			}
		}
	}

	// No progress - should not happen in normal operation
	return nil
}

// ComputeAllSteps generates the full step sequence from source to target for N phases.
// This is useful for testing and visualization.
func ComputeAllSteps(source, target PhaseReplicaState, config RollingUpdateConfig) []UpdateStep {
	numPhases := len(source)

	// Make copies of source for current state
	currentOld := make([]int, numPhases)
	copy(currentOld, source)
	currentNew := make([]int, numPhases)

	// Safety limit to prevent infinite loops
	maxReplicas := 0
	for i := 0; i < numPhases; i++ {
		maxReplicas = max(maxReplicas, source[i], target[i])
	}
	maxSteps := maxReplicas*2 + 10

	// Initial step: all old replicas, no new replicas
	initialPast := make([]int, numPhases)
	copy(initialPast, source)
	initialNew := make([]int, numPhases)
	steps := []UpdateStep{{Past: initialPast, New: initialNew}}

	for i := 0; i < maxSteps; i++ {
		nextStep := ComputeNextStep(source, currentOld, currentNew, target, config)
		if nextStep == nil {
			break
		}

		steps = append(steps, *nextStep)
		currentOld = nextStep.Past
		currentNew = nextStep.New
	}

	return steps
}

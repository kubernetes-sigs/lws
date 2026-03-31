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
//   - N dimensions: all roles must stay coordinated
package controller

import (
	"math"
)

// UpdateStep represents a single step in the rolling update plan.
// It tracks the replica counts for both old (past) and new deployments
// for N roles (slice-based for flexibility).
type UpdateStep struct {
	Past []int
	New  []int
}

// RoleReplicaState holds the replica counts for N roles.
// Used for source, current, and target state in rolling update planning.
type RoleReplicaState = []int

// RollingUpdateConfig holds the rolling update constraints per role.
type RollingUpdateConfig struct {
	MaxSurge       int
	MaxUnavailable int
}

// DefaultRollingUpdateConfig returns the default rolling update config for N roles (maxSurge=1, maxUnavailable=0).
func DefaultRollingUpdateConfig(numRoles int) []RollingUpdateConfig {
	configs := make([]RollingUpdateConfig, numRoles)
	for i := 0; i < numRoles; i++ {
		configs[i].MaxSurge = 1
		configs[i].MaxUnavailable = 0
	}
	return configs
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
func computeTotalSteps(source, target RoleReplicaState, config []RollingUpdateConfig) int {
	totalSteps := 0
	numRoles := len(source)
	for i := 0; i < numRoles; i++ {
		maxReplicas := max(source[i], target[i], 0)
		roleBatchSize := batchSize(config[i].MaxSurge, config[i].MaxUnavailable)
		roleSteps := (maxReplicas + roleBatchSize - 1) / roleBatchSize
		totalSteps = max(totalSteps, roleSteps)
	}
	return totalSteps
}

// computeNextNewReplicas computes the next new replica count for scale-up.
//
// Linear interpolation: newAtStep(i) = ceil(i * target / totalSteps)
//
// Uses min step index across dimensions to keep roles in sync.
func computeNextNewReplicas(target, currentNew RoleReplicaState, totalSteps int) RoleReplicaState {
	numRoles := len(target)
	if totalSteps == 0 {
		result := make([]int, numRoles)
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
	for i := 0; i < numRoles; i++ {
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

	result := make([]int, numRoles)
	for i := 0; i < numRoles; i++ {
		result[i] = computeNew(target[i], currentNew[i])
	}
	return result
}

// computeNextOldReplicas computes the next old replica count for scale-down.
//
// Linear interpolation: oldAtStep(i) = source - floor(i * source / totalSteps)
//
// Uses max step index across dimensions to ensure all roles drain together.
func computeNextOldReplicas(source, currentOld RoleReplicaState, totalSteps int) RoleReplicaState {
	numRoles := len(source)
	if totalSteps == 0 {
		return make([]int, numRoles)
	}

	// Step 1: figure out which step we're at based on how many replicas were removed
	// Skip roles with source=0 (new roles) - they don't affect drain timing
	stepIndex := func(removed, sourceVal int) int {
		if sourceVal == 0 {
			return 0 // New roles don't contribute to drain step calculation
		}
		return int(float64(removed) * float64(totalSteps) / float64(sourceVal))
	}

	// Find the role that has drained the most (highest step index).
	// Using max ensures roles sync up: the most-drained role sets the pace,
	// and lagging roles catch up in the next step.
	maxStepIdx := 0
	for i := 0; i < numRoles; i++ {
		if source[i] == 0 {
			continue
		}
		removed := source[i] - currentOld[i]
		maxStepIdx = max(maxStepIdx, stepIndex(removed, source[i]))
	}
	nextStepIdx := maxStepIdx + 1

	// Step 2: compute how many replicas should remain at the next step
	computeOld := func(sourceVal, currentVal int) int {
		progress := float64(nextStepIdx) * float64(sourceVal) / float64(totalSteps)
		computed := max(0, sourceVal-int(math.Floor(progress)))
		return min(computed, currentVal) // never increase
	}

	result := make([]int, numRoles)
	for i := 0; i < numRoles; i++ {
		result[i] = computeOld(source[i], currentOld[i])
	}
	return result
}

// correctAbnormalState corrects abnormal states where old replicas exceed the inferred source.
// This shouldn't happen in normal rollouts (old starts at source and only decreases),
// but can occur from interrupted rollouts or manual intervention.
// Returns a correction step if needed, nil otherwise.
func correctAbnormalState(currentOld, currentNew, source RoleReplicaState) *UpdateStep {
	numRoles := len(source)
	expectedOld := make([]int, numRoles)
	needsCorrection := false
	for i := 0; i < numRoles; i++ {
		expectedOld[i] = min(source[i], currentOld[i])
		if currentOld[i] > expectedOld[i] {
			needsCorrection = true
		}
	}

	if needsCorrection {
		newCopy := make([]int, numRoles)
		copy(newCopy, currentNew)
		return &UpdateStep{
			Past: expectedOld,
			New:  newCopy,
		}
	}
	return nil
}

// isComplete returns true if the rollout is done (all old=0, all new>=target).
func isComplete(currentOld, currentNew, targetNew RoleReplicaState) bool {
	for i := range currentOld {
		if currentOld[i] != 0 || currentNew[i] < targetNew[i] {
			return false
		}
	}
	return true
}

// isNewAtTarget returns true if all new replicas have reached their target.
func isNewAtTarget(currentNew, targetNew RoleReplicaState) bool {
	for i := range currentNew {
		if currentNew[i] < targetNew[i] {
			return false
		}
	}
	return true
}

// canScaleUp checks if scaling to nextNew would violate surge constraints.
// Constraint: old + new <= target + maxSurge (always relative to target, not source)
func canScaleUp(currentOld, nextNew, source, targetNew RoleReplicaState, config []RollingUpdateConfig) bool {
	for i := range currentOld {
		if targetNew[i] == 0 {
			continue // Removed roles just drain, no surge constraint
		}
		if currentOld[i]+nextNew[i] > targetNew[i]+config[i].MaxSurge {
			return false
		}
	}
	return true
}

// computeMinOld computes the minimum old replicas per role to satisfy maxUnavailable.
// Only enforced when source >= target (system had enough replicas to maintain availability).
func computeMinOld(source, currentNew, targetNew RoleReplicaState, config []RollingUpdateConfig) []int {
	minOld := make([]int, len(source))
	for i := range source {
		if source[i] >= targetNew[i] {
			minOld[i] = max(0, targetNew[i]-config[i].MaxUnavailable-currentNew[i])
		}
	}
	return minOld
}

// tryScaleUp attempts to scale up new replicas if surge allows.
func tryScaleUp(currentOld, currentNew, nextNew RoleReplicaState, source, targetNew RoleReplicaState, config []RollingUpdateConfig) *UpdateStep {
	needsScaleUp := false
	for i := range currentNew {
		if nextNew[i] > currentNew[i] {
			needsScaleUp = true
			break
		}
	}
	if !needsScaleUp {
		return nil
	}
	if !canScaleUp(currentOld, nextNew, source, targetNew, config) {
		return nil
	}
	return &UpdateStep{Past: currentOld, New: nextNew}
}

// tryProportionalDrain attempts to drain old replicas using linear interpolation.
func tryProportionalDrain(source, currentOld, currentNew, targetNew RoleReplicaState, minOld []int, totalSteps int, config []RollingUpdateConfig) *UpdateStep {
	nextOld := computeNextOldReplicas(source, currentOld, totalSteps)

	// Apply maxUnavailable floor
	for i := range nextOld {
		nextOld[i] = max(nextOld[i], minOld[i])
	}

	applyOrphanPrevention(nextOld, currentNew, source, targetNew, config)

	needsScaleDown := false
	for i := range nextOld {
		if nextOld[i] < currentOld[i] {
			needsScaleDown = true
			break
		}
	}
	if !needsScaleDown {
		return nil
	}
	return &UpdateStep{Past: nextOld, New: currentNew}
}

// canDrainAllToZero checks if all roles have sufficient new replicas to satisfy
// maxUnavailable after draining all old replicas to 0.
func canDrainAllToZero(nextNew, source, target RoleReplicaState, config []RollingUpdateConfig) bool {
	for i := range target {
		if source[i] >= target[i] {
			minRequired := target[i] - config[i].MaxUnavailable
			if nextNew[i] < minRequired {
				return false
			}
		}
	}
	return true
}

// applyOrphanPrevention adjusts nextOld to prevent orphan situations where one role
// drains to 0 while others still have old replicas. If any role would drain to 0:
//   - If all roles can safely drain (satisfy maxUnavailable): drain all to 0
//   - Otherwise: keep would-be-zero roles at 1 until safe to drain together
func applyOrphanPrevention(nextOld, currentNew, source, target RoleReplicaState, config []RollingUpdateConfig) {
	anyDrainsToZero := false
	allDrainToZero := true
	for i := range nextOld {
		if source[i] == 0 {
			continue
		}
		if nextOld[i] == 0 {
			anyDrainsToZero = true
		} else {
			allDrainToZero = false
		}
	}

	// No orphan situation
	if !anyDrainsToZero || allDrainToZero {
		return
	}

	// Orphan detected: some roles drain to 0 but not all
	if canDrainAllToZero(currentNew, source, target, config) {
		for i := range nextOld {
			nextOld[i] = 0
		}
		return
	}

	// Can't drain all yet - keep would-be-zero roles at 1
	for i := range nextOld {
		if nextOld[i] == 0 && source[i] > 0 {
			nextOld[i] = 1
		}
	}
}

// tryForceDrain drains exactly enough old replicas to unblock the next scale-up,
// and performs the scale-up in the same step to avoid capacity dips.
func tryForceDrain(currentOld, currentNew, nextNew RoleReplicaState, source, targetNew RoleReplicaState, config []RollingUpdateConfig) *UpdateStep {
	drainedOld := make([]int, len(currentOld))
	needsDrain := false

	for i := range currentOld {
		maxOld := targetNew[i] + config[i].MaxSurge - nextNew[i]
		drainedOld[i] = max(0, min(currentOld[i], maxOld))
		// Compute minOld using nextNew to ensure maxUnavailable is respected
		if source[i] >= targetNew[i] {
			minOldForRole := max(0, targetNew[i]-config[i].MaxUnavailable-nextNew[i])
			drainedOld[i] = max(drainedOld[i], minOldForRole)
		}
		if drainedOld[i] < currentOld[i] {
			needsDrain = true
		}
	}
	if !needsDrain {
		return nil
	}

	applyOrphanPrevention(drainedOld, nextNew, source, targetNew, config)

	return &UpdateStep{Past: drainedOld, New: nextNew}
}

// ComputeNextStep computes the next step in the rolling update.
// Returns nil when no more steps are needed (current state equals target).
// Steps are generally decoupled (change old OR new), but when maxSurge=0 forces
// a drain before scale-up, both are combined to avoid capacity dips.
func ComputeNextStep(source, currentOld, currentNew, targetNew RoleReplicaState, config []RollingUpdateConfig) *UpdateStep {
	if isComplete(currentOld, currentNew, targetNew) {
		return nil
	}

	totalSteps := computeTotalSteps(source, targetNew, config)
	if totalSteps == 0 {
		return nil
	}

	if step := correctAbnormalState(currentOld, currentNew, source); step != nil {
		return step
	}

	// New at target but old remains: drain all
	if isNewAtTarget(currentNew, targetNew) {
		return &UpdateStep{Past: make([]int, len(source)), New: currentNew}
	}

	nextNew := computeNextNewReplicas(targetNew, currentNew, totalSteps)
	minOld := computeMinOld(source, currentNew, targetNew, config)

	if step := tryScaleUp(currentOld, currentNew, nextNew, source, targetNew, config); step != nil {
		return step
	}
	if step := tryProportionalDrain(source, currentOld, currentNew, targetNew, minOld, totalSteps, config); step != nil {
		return step
	}
	if step := tryForceDrain(currentOld, currentNew, nextNew, source, targetNew, config); step != nil {
		return step
	}

	return nil
}

// ComputeAllSteps generates the full step sequence from source to target for N roles.
// This is useful for testing and visualization.
func ComputeAllSteps(source, target RoleReplicaState, config []RollingUpdateConfig) []UpdateStep {
	numRoles := len(source)

	// Make copies of source for current state
	currentOld := make([]int, numRoles)
	copy(currentOld, source)
	currentNew := make([]int, numRoles)

	// Safety limit to prevent infinite loops
	maxReplicas := 0
	for i := 0; i < numRoles; i++ {
		maxReplicas = max(maxReplicas, source[i], target[i])
	}
	maxSteps := maxReplicas*2 + 10

	// Initial step: all old replicas, no new replicas
	initialPast := make([]int, numRoles)
	copy(initialPast, source)
	initialNew := make([]int, numRoles)
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

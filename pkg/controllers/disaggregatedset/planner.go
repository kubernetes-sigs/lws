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

// Package disaggregatedset provides rolling update planning and execution for DisaggregatedSet.
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
package disaggregatedset

import (
	"math"
)

type UpdateStep struct {
	Past []int
	New  []int
}

type RoleReplicaState = []int

type RollingUpdateConfig struct {
	MaxSurge       int
	MaxUnavailable int
}

func DefaultRollingUpdateConfig(numRoles int) []RollingUpdateConfig {
	configs := make([]RollingUpdateConfig, numRoles)
	for i := 0; i < numRoles; i++ {
		configs[i].MaxSurge = 1
		configs[i].MaxUnavailable = 0
	}
	return configs
}

func batchSize(maxSurge, maxUnavailable int) int {
	if maxSurge > 0 {
		return maxSurge
	}
	return max(1, maxUnavailable)
}

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

func computeNextNewReplicas(target, currentNew RoleReplicaState, totalSteps int) RoleReplicaState {
	numRoles := len(target)
	if totalSteps == 0 {
		result := make([]int, numRoles)
		copy(result, target)
		return result
	}

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

	computeNew := func(targetVal, currentVal int) int {
		progress := float64(nextStepIdx) * float64(targetVal) / float64(totalSteps)
		computed := min(int(math.Ceil(progress)), targetVal)
		return max(computed, currentVal)
	}

	result := make([]int, numRoles)
	for i := 0; i < numRoles; i++ {
		result[i] = computeNew(target[i], currentNew[i])
	}
	return result
}

func computeNextOldReplicas(source, currentOld RoleReplicaState, totalSteps int) RoleReplicaState {
	numRoles := len(source)
	if totalSteps == 0 {
		return make([]int, numRoles)
	}

	stepIndex := func(removed, sourceVal int) int {
		if sourceVal == 0 {
			return 0
		}
		return int(float64(removed) * float64(totalSteps) / float64(sourceVal))
	}

	maxStepIdx := 0
	for i := 0; i < numRoles; i++ {
		if source[i] == 0 {
			continue
		}
		removed := source[i] - currentOld[i]
		maxStepIdx = max(maxStepIdx, stepIndex(removed, source[i]))
	}
	nextStepIdx := maxStepIdx + 1

	computeOld := func(sourceVal, currentVal int) int {
		progress := float64(nextStepIdx) * float64(sourceVal) / float64(totalSteps)
		computed := max(0, sourceVal-int(math.Floor(progress)))
		return min(computed, currentVal)
	}

	result := make([]int, numRoles)
	for i := 0; i < numRoles; i++ {
		result[i] = computeOld(source[i], currentOld[i])
	}
	return result
}

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

func isComplete(currentOld, currentNew, targetNew RoleReplicaState) bool {
	for i := range currentOld {
		if currentOld[i] != 0 || currentNew[i] < targetNew[i] {
			return false
		}
	}
	return true
}

func isNewAtTarget(currentNew, targetNew RoleReplicaState) bool {
	for i := range currentNew {
		if currentNew[i] < targetNew[i] {
			return false
		}
	}
	return true
}

func canScaleUp(currentOld, nextNew, source, targetNew RoleReplicaState, config []RollingUpdateConfig) bool {
	for i := range currentOld {
		if targetNew[i] == 0 {
			continue
		}
		if currentOld[i]+nextNew[i] > targetNew[i]+config[i].MaxSurge {
			return false
		}
	}
	return true
}

func computeMinOld(source, currentNew, targetNew RoleReplicaState, config []RollingUpdateConfig) []int {
	minOld := make([]int, len(source))
	for i := range source {
		if source[i] >= targetNew[i] {
			minOld[i] = max(0, targetNew[i]-config[i].MaxUnavailable-currentNew[i])
		}
	}
	return minOld
}

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

func tryProportionalDrain(source, currentOld, currentNew, targetNew RoleReplicaState, minOld []int, totalSteps int, config []RollingUpdateConfig) *UpdateStep {
	nextOld := computeNextOldReplicas(source, currentOld, totalSteps)

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

	if !anyDrainsToZero || allDrainToZero {
		return
	}

	if canDrainAllToZero(currentNew, source, target, config) {
		for i := range nextOld {
			nextOld[i] = 0
		}
		return
	}

	for i := range nextOld {
		if nextOld[i] == 0 && source[i] > 0 {
			nextOld[i] = 1
		}
	}
}

func tryForceDrain(currentOld, currentNew, nextNew RoleReplicaState, source, targetNew RoleReplicaState, config []RollingUpdateConfig) *UpdateStep {
	drainedOld := make([]int, len(currentOld))
	needsDrain := false

	for i := range currentOld {
		maxOld := targetNew[i] + config[i].MaxSurge - nextNew[i]
		drainedOld[i] = max(0, min(currentOld[i], maxOld))
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

func ComputeAllSteps(source, target RoleReplicaState, config []RollingUpdateConfig) []UpdateStep {
	numRoles := len(source)

	currentOld := make([]int, numRoles)
	copy(currentOld, source)
	currentNew := make([]int, numRoles)

	maxReplicas := 0
	for i := 0; i < numRoles; i++ {
		maxReplicas = max(maxReplicas, source[i], target[i])
	}
	maxSteps := maxReplicas*2 + 10

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

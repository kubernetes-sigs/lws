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

// Package controller provides rolling update planning and execution for DisaggDeployment.
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
//   - Two dimensions: prefill and decode must stay coordinated
package controller

import (
	"math"
)

// UpdateStep represents a single step in the rolling update plan.
// It tracks the replica counts for both old (past) and new deployments
// for both prefill and decode phases.
type UpdateStep struct {
	PastPrefill int
	PastDecode  int
	NewPrefill  int
	NewDecode   int
}

// PhaseReplicaState holds the replica counts for both phases.
// Used for source, current, and target state in rolling update planning.
type PhaseReplicaState struct {
	Prefill int
	Decode  int
}

// RollingUpdateConfig holds the rolling update constraints per phase.
type RollingUpdateConfig struct {
	PrefillMaxSurge       int
	PrefillMaxUnavailable int
	DecodeMaxSurge        int
	DecodeMaxUnavailable  int
}

// DefaultRollingUpdateConfig returns the default rolling update config (maxSurge=1, maxUnavailable=0).
func DefaultRollingUpdateConfig() RollingUpdateConfig {
	return RollingUpdateConfig{
		PrefillMaxSurge:       1,
		PrefillMaxUnavailable: 0,
		DecodeMaxSurge:        1,
		DecodeMaxUnavailable:  0,
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
	prefillMaxReplicas := max(source.Prefill, target.Prefill, 0)
	decodeMaxReplicas := max(source.Decode, target.Decode, 0)
	prefillBatchSize := batchSize(config.PrefillMaxSurge, config.PrefillMaxUnavailable)
	decodeBatchSize := batchSize(config.DecodeMaxSurge, config.DecodeMaxUnavailable)

	totalPrefillSteps := (prefillMaxReplicas + prefillBatchSize - 1) / prefillBatchSize
	totalDecodeSteps := (decodeMaxReplicas + decodeBatchSize - 1) / decodeBatchSize
	return max(totalPrefillSteps, totalDecodeSteps)
}

// computeNextNewReplicas computes the next new replica count for scale-up.
//
// Linear interpolation: newAtStep(i) = ceil(i * target / totalSteps)
//
// Uses min step index across dimensions to keep prefill/decode in sync.
func computeNextNewReplicas(target, currentNew PhaseReplicaState, totalSteps int) PhaseReplicaState {
	if totalSteps == 0 {
		return target
	}

	// Step 1: figure out which step we're at based on current replicas
	stepIndex := func(current, targetVal int) int {
		if targetVal == 0 {
			return totalSteps
		}
		return int(float64(current) * float64(totalSteps) / float64(targetVal))
	}
	stepPrefill := stepIndex(currentNew.Prefill, target.Prefill)
	stepDecode := stepIndex(currentNew.Decode, target.Decode)
	nextStepIdx := min(stepPrefill, stepDecode) + 1

	// Step 2: compute how many replicas we should have at the next step
	computeNew := func(targetVal, currentVal int) int {
		progress := float64(nextStepIdx) * float64(targetVal) / float64(totalSteps)
		computed := min(int(math.Ceil(progress)), targetVal)
		return max(computed, currentVal) // never decrease
	}

	return PhaseReplicaState{
		Prefill: computeNew(target.Prefill, currentNew.Prefill),
		Decode:  computeNew(target.Decode, currentNew.Decode),
	}
}

// computeNextOldReplicas computes the next old replica count for scale-down.
//
// Linear interpolation: oldAtStep(i) = source - floor(i * source / totalSteps)
//
// Uses max step index across dimensions to ensure both phases drain together.
func computeNextOldReplicas(source, currentOld PhaseReplicaState, totalSteps int) PhaseReplicaState {
	if totalSteps == 0 {
		return PhaseReplicaState{Prefill: 0, Decode: 0}
	}

	// Step 1: figure out which step we're at based on how many replicas were removed
	stepIndex := func(removed, sourceVal int) int {
		if sourceVal == 0 {
			return totalSteps
		}
		return int(float64(removed) * float64(totalSteps) / float64(sourceVal))
	}
	removedPrefill := source.Prefill - currentOld.Prefill
	removedDecode := source.Decode - currentOld.Decode
	stepPrefill := stepIndex(removedPrefill, source.Prefill)
	stepDecode := stepIndex(removedDecode, source.Decode)
	nextStepIdx := max(stepPrefill, stepDecode) + 1

	// Step 2: compute how many replicas should remain at the next step
	computeOld := func(sourceVal, currentVal int) int {
		progress := float64(nextStepIdx) * float64(sourceVal) / float64(totalSteps)
		computed := max(0, sourceVal-int(math.Floor(progress)))
		return min(computed, currentVal) // never increase
	}

	return PhaseReplicaState{
		Prefill: computeOld(source.Prefill, currentOld.Prefill),
		Decode:  computeOld(source.Decode, currentOld.Decode),
	}
}

// correctAbnormalState corrects abnormal states where old replicas exceed the inferred source.
// This shouldn't happen in normal rollouts (old starts at source and only decreases),
// but can occur from interrupted rollouts or manual intervention.
// Returns a correction step if needed, nil otherwise.
func correctAbnormalState(currentOld, currentNew, source PhaseReplicaState) *UpdateStep {
	expectedOldPrefill := min(source.Prefill, currentOld.Prefill)
	expectedOldDecode := min(source.Decode, currentOld.Decode)

	if currentOld.Prefill > expectedOldPrefill || currentOld.Decode > expectedOldDecode {
		return &UpdateStep{
			PastPrefill: expectedOldPrefill,
			PastDecode:  expectedOldDecode,
			NewPrefill:  currentNew.Prefill,
			NewDecode:   currentNew.Decode,
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
	// If already at target (no old replicas, new at target), no more steps needed
	if currentOld.Prefill == 0 && currentOld.Decode == 0 &&
		currentNew.Prefill >= targetNew.Prefill && currentNew.Decode >= targetNew.Decode {
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
	if currentNew.Prefill >= targetNew.Prefill && currentNew.Decode >= targetNew.Decode {
		return &UpdateStep{
			PastPrefill: 0,
			PastDecode:  0,
			NewPrefill:  currentNew.Prefill,
			NewDecode:   currentNew.Decode,
		}
	}

	// Compute what new replicas should be at the next step
	nextNewState := computeNextNewReplicas(targetNew, currentNew, totalNumSteps)

	needsScaleUp := nextNewState.Prefill > currentNew.Prefill || nextNewState.Decode > currentNew.Decode

	// Check surge constraint for scale-up (old + new <= target + surge)
	prefillSurgeOK := currentOld.Prefill+nextNewState.Prefill <= targetNew.Prefill+config.PrefillMaxSurge
	decodeSurgeOK := currentOld.Decode+nextNewState.Decode <= targetNew.Decode+config.DecodeMaxSurge

	if needsScaleUp && prefillSurgeOK && decodeSurgeOK {
		// Scale up new replicas (keep old unchanged)
		return &UpdateStep{
			PastPrefill: currentOld.Prefill,
			PastDecode:  currentOld.Decode,
			NewPrefill:  nextNewState.Prefill,
			NewDecode:   nextNewState.Decode,
		}
	}

	// Scale down: first try proportional drain
	nextOldState := computeNextOldReplicas(source, currentOld, totalNumSteps)

	needsScaleDown := nextOldState.Prefill < currentOld.Prefill || nextOldState.Decode < currentOld.Decode

	if needsScaleDown {
		// Scale down old replicas (keep new unchanged)
		return &UpdateStep{
			PastPrefill: nextOldState.Prefill,
			PastDecode:  nextOldState.Decode,
			NewPrefill:  currentNew.Prefill,
			NewDecode:   currentNew.Decode,
		}
	}

	// Proportional drain didn't help - surge is blocking and old step is behind.
	// Drain exactly what's needed to allow the next scale-up.
	if needsScaleUp {
		maxOldPrefill := targetNew.Prefill + config.PrefillMaxSurge - nextNewState.Prefill
		maxOldDecode := targetNew.Decode + config.DecodeMaxSurge - nextNewState.Decode
		drainedOldPrefill := max(0, min(currentOld.Prefill, maxOldPrefill))
		drainedOldDecode := max(0, min(currentOld.Decode, maxOldDecode))

		if drainedOldPrefill < currentOld.Prefill || drainedOldDecode < currentOld.Decode {
			return &UpdateStep{
				PastPrefill: drainedOldPrefill,
				PastDecode:  drainedOldDecode,
				NewPrefill:  currentNew.Prefill,
				NewDecode:   currentNew.Decode,
			}
		}
	}

	// No progress - should not happen in normal operation
	return nil
}

// ComputeAllSteps generates the full step sequence from source to target.
// This is useful for testing and visualization.
func ComputeAllSteps(pastPrefill, pastDecode, newPrefill, newDecode int, config RollingUpdateConfig) []UpdateStep {
	targetNew := PhaseReplicaState{Prefill: newPrefill, Decode: newDecode}
	source := PhaseReplicaState{Prefill: pastPrefill, Decode: pastDecode}
	currentOld := PhaseReplicaState{Prefill: pastPrefill, Decode: pastDecode}
	currentNew := PhaseReplicaState{Prefill: 0, Decode: 0}

	// Safety limit to prevent infinite loops
	maxSteps := max(pastPrefill, pastDecode, newPrefill, newDecode)*2 + 10

	steps := []UpdateStep{{PastPrefill: pastPrefill, PastDecode: pastDecode, NewPrefill: 0, NewDecode: 0}}

	for i := 0; i < maxSteps; i++ {
		nextStep := ComputeNextStep(source, currentOld, currentNew, targetNew, config)
		if nextStep == nil {
			break
		}

		steps = append(steps, *nextStep)
		currentOld.Prefill = nextStep.PastPrefill
		currentOld.Decode = nextStep.PastDecode
		currentNew.Prefill = nextStep.NewPrefill
		currentNew.Decode = nextStep.NewDecode
	}

	return steps
}

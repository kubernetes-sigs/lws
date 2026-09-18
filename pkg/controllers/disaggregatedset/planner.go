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

// Package disaggregatedset plans and executes rolling updates for DisaggregatedSet.
//
// Each side of a rollout advances on its own fraction scale. The local
// variables in ComputeNextStep map to the KEP terms as follows:
//
//	newStepCount = fractionalStepCount(targetNew)  = max(targetNew)
//	oldStepCount = fractionalStepCount(initialOld) = max(initialOld)
//	smallestReplicaFraction for each side = 1 / that side's step count
//
// The executor stores the denominator of largestReplicaFraction in
// fractionalCoordinationWindow.largestReplicaFractionDenominator. It is the
// smallest role.NewTargetReplicas value greater than zero.
//
// At step k, a role targets ceil(size*k/stepCount) new replicas or
// ceil(size*(stepCount-k)/stepCount) old replicas. The least-advanced role
// determines side progress, keeping role ratios within the rounding error of
// one replica (largestReplicaFraction).
//
// Spec replicas represent issued work and drive this planner. The executor
// separately uses Ready replicas to enforce availability and bound pending
// work. MaxSurge and MaxUnavailable are projected onto the fraction scale for
// proportional planning; the executor enforces their raw per-role limits.
package disaggregatedset

type UpdateStep struct {
	Past RoleReplicaState
	New  RoleReplicaState
}

type RoleReplicaState = []int

type RollingUpdateConfig struct {
	MaxSurge       int
	MaxUnavailable int
}

// fractionalStepCount returns max(replicas). One step represents the KEP's
// smallestReplicaFraction for that side: 1 / fractionalStepCount.
func fractionalStepCount(replicas RoleReplicaState) int {
	maxReplicas := 0
	for _, replicas := range replicas {
		maxReplicas = max(maxReplicas, replicas)
	}
	return maxReplicas
}

// sideProgress returns the step reached by every non-empty role. Both sides
// use ceiling targets, so the inverse differs for growth and drain.
func sideProgress(current, sizes RoleReplicaState, totalSteps int, drained bool) int {
	if totalSteps == 0 {
		return 0
	}
	progress := totalSteps
	for i, size := range sizes {
		if size == 0 {
			continue
		}
		count := current[i]
		var roleProgress int
		if drained {
			count = min(count, size)
			roleProgress = (totalSteps*(size-count+1) - 1) / size
			roleProgress = min(max(roleProgress, 0), totalSteps)
		} else {
			roleProgress = count * totalSteps / size
		}
		progress = min(progress, roleProgress)
	}
	return progress
}

func wantReplicas(roleSize, step, totalSteps int, drained bool) int {
	if totalSteps == 0 {
		if drained {
			return roleSize
		}
		return 0
	}
	if drained {
		step = totalSteps - step
	}
	return (roleSize*step + totalSteps - 1) / totalSteps
}

func ComputeNextStep(
	initialOld, currentOld, currentNew, targetNew RoleReplicaState,
	config []RollingUpdateConfig,
) *UpdateStep {
	if isComplete(currentOld, currentNew, targetNew) {
		return nil
	}

	// Fractions are represented as integer checkpoints, not floating-point
	// values. These assignments map the code directly to the KEP formulas:
	//
	//   newStepCount = max(targetNew);  new smallestReplicaFraction = 1 / newStepCount
	//   oldStepCount = max(initialOld); old smallestReplicaFraction = 1 / oldStepCount
	newStepCount := fractionalStepCount(targetNew)
	oldStepCount := fractionalStepCount(initialOld)
	budgetSteps := max(newStepCount, oldStepCount)

	// The least-advanced role selects the shared checkpoint. Projecting every
	// role from that checkpoint with ceiling division keeps the ideal plan
	// within one replica of the smallest non-empty role. That is the KEP's
	// largestReplicaFraction. The executor calculates its denominator explicitly
	// and reapplies the window after readiness clamps roles independently.
	currentNewStep := sideProgress(currentNew, targetNew, newStepCount, false)
	currentOldStep := sideProgress(currentOld, initialOld, oldStepCount, true)

	// Each side normally advances by one checkpoint. If the old side has
	// already drained farther, the new side catches up to the equivalent
	// checkpoint on its own scale.
	nextNewStep := min(max(
		currentNewStep+1,
		projectProgressStep(currentOldStep, oldStepCount, newStepCount),
	), newStepCount)
	nextOldStep := min(currentOldStep+1, oldStepCount)

	maxSurge, maxUnavailable := 0, 0
	for _, cfg := range config {
		maxSurge = max(maxSurge, cfg.MaxSurge)
		maxUnavailable = max(maxUnavailable, cfg.MaxUnavailable)
	}
	newTargetStep := min(nextNewStep+maxSurge, newStepCount)
	oldTargetStep := min(nextOldStep+maxUnavailable, oldStepCount)

	past := make(RoleReplicaState, len(initialOld))
	now := make(RoleReplicaState, len(initialOld))
	addNew := make(RoleReplicaState, len(initialOld))
	// All three values below are per-role Spec counts. The executor applies the
	// Ready-based safety limit after the planner returns.
	fractionalDrain := make(RoleReplicaState, len(initialOld))   // Drain requested by the fractional schedule.
	specDrainHeadroom := make(RoleReplicaState, len(initialOld)) // Drain allowed above the planner's Spec floor.
	proposedDrain := make(RoleReplicaState, len(initialOld))     // Requested drain capped by the available headroom.

	for i := range initialOld {
		roleSize := max(initialOld[i], targetNew[i])
		ceiling := roleSize + projectBudget(roleSize, config[i].MaxSurge, budgetSteps)
		floor := max(0, min(initialOld[i], targetNew[i])-projectBudget(roleSize, config[i].MaxUnavailable, budgetSteps))
		if config[i].MaxSurge == 0 && config[i].MaxUnavailable == 0 {
			ceiling++
		}

		total := currentOld[i] + currentNew[i]
		addNew[i] = min(max(wantReplicas(targetNew[i], newTargetStep, newStepCount, false)-currentNew[i], 0), max(0, ceiling-total))
		fractionalDrain[i] = max(0, currentOld[i]-wantReplicas(initialOld[i], oldTargetStep, oldStepCount, true))
		specDrainHeadroom[i] = max(0, total-floor)
		proposedDrain[i] = min(fractionalDrain[i], specDrainHeadroom[i])
	}

	// If one role cannot drain proportionally yet, let new growth open its
	// budget before draining the other roles. When no growth is possible, keep
	// the safe drain so a valid zero-surge rollout cannot wedge.
	blocked, draining := false, false
	for i := range proposedDrain {
		blocked = blocked || fractionalDrain[i] > 0 && proposedDrain[i] == 0
		draining = draining || proposedDrain[i] > 0
	}
	if blocked && draining && anyPositive(addNew) {
		clear(proposedDrain)
	}

	for i := range initialOld {
		past[i] = currentOld[i] - proposedDrain[i]
		now[i] = currentNew[i] + addNew[i]
	}
	if anyChange(past, now, currentOld, currentNew) {
		return &UpdateStep{Past: past, New: now}
	}

	// Rounding can block both sides at zero surge. Open one floor-safe slot for
	// replacement capacity rather than reporting completion.
	for i := range currentOld {
		if currentOld[i] > 0 && currentNew[i] < targetNew[i] && specDrainHeadroom[i] > 0 {
			past[i]--
			return &UpdateStep{Past: past, New: now}
		}
	}
	return nil
}

func anyPositive(values RoleReplicaState) bool {
	for _, value := range values {
		if value > 0 {
			return true
		}
	}
	return false
}

func isComplete(currentOld, currentNew, targetNew RoleReplicaState) bool {
	for i := range currentOld {
		if currentOld[i] != 0 || currentNew[i] < targetNew[i] {
			return false
		}
	}
	return true
}

func anyChange(past, now, currentOld, currentNew RoleReplicaState) bool {
	for i := range past {
		if past[i] != currentOld[i] || now[i] != currentNew[i] {
			return true
		}
	}
	return false
}

func projectBudget(roleSize, budget, totalSteps int) int {
	if roleSize <= 0 || budget <= 0 || totalSteps <= 0 {
		return 0
	}
	return (roleSize*budget + totalSteps - 1) / totalSteps
}

func projectProgressStep(step, fromSteps, toSteps int) int {
	if step <= 0 || fromSteps <= 0 || toSteps <= 0 {
		return 0
	}
	return min((step*toSteps+fromSteps-1)/fromSteps, toSteps)
}

// ComputeAllSteps simulates a complete rollout for tests and plan-steps.
func ComputeAllSteps(initialOld, target RoleReplicaState, config []RollingUpdateConfig) []UpdateStep {
	currentOld := append(RoleReplicaState(nil), initialOld...)
	currentNew := make(RoleReplicaState, len(initialOld))
	steps := []UpdateStep{{Past: append(RoleReplicaState(nil), initialOld...), New: make(RoleReplicaState, len(initialOld))}}

	for range max(fractionalStepCount(initialOld), fractionalStepCount(target))*4 + 10 {
		next := ComputeNextStep(initialOld, currentOld, currentNew, target, config)
		if next == nil {
			break
		}
		steps = append(steps, *next)
		currentOld, currentNew = next.Past, next.New
	}
	return steps
}

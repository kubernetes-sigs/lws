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

// fractionalSteps describes the planner's next position on the old and new
// fraction scales. Each side has its own step count because its role sizes may
// differ from the other side.
type fractionalSteps struct {
	newStep         int // New-side fractional step proposed for this iteration.
	newStepCount    int // Total number of fractional steps on the new side.
	oldStep         int // Old-side fractional step proposed for this iteration.
	oldStepCount    int // Total number of fractional steps on the old side.
	budgetStepCount int // Common scale used to project surge and unavailable budgets.
}

// replicaChanges contains the per-role changes calculated for one planner
// iteration. All values are replica counts, not fractional steps.
type replicaChanges struct {
	growBy         RoleReplicaState // New replicas allowed by the schedule and surge ceiling.
	scheduledDrain RoleReplicaState // Old replicas the fractional schedule wants to remove.
	drainHeadroom  RoleReplicaState // Old replicas removable without crossing the Spec floor.
	drainBy        RoleReplicaState // Scheduled drain capped by the available headroom.
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

// leastAdvancedStep returns the progress step of the least-advanced non-empty
// role. Both sides use ceiling targets, so the inverse differs for growth and
// drain.
func leastAdvancedStep(current, roleSizes RoleReplicaState, stepCount int, draining bool) int {
	if stepCount == 0 {
		return 0
	}
	progress := stepCount
	for i, roleSize := range roleSizes {
		if roleSize == 0 {
			continue
		}
		count := current[i]
		var roleProgress int
		if draining {
			count = min(count, roleSize)
			roleProgress = (stepCount*(roleSize-count+1) - 1) / roleSize
			roleProgress = min(max(roleProgress, 0), stepCount)
		} else {
			roleProgress = count * stepCount / roleSize
		}
		progress = min(progress, roleProgress)
	}
	return progress
}

func wantReplicas(roleSize, step, stepCount int, draining bool) int {
	if stepCount == 0 {
		if draining {
			return roleSize
		}
		return 0
	}
	if draining {
		step = stepCount - step
	}
	return (roleSize*step + stepCount - 1) / stepCount
}

func ComputeNextStep(
	initialOld, currentOld, currentNew, targetNew RoleReplicaState,
	config []RollingUpdateConfig,
) *UpdateStep {
	if isComplete(currentOld, currentNew, targetNew) {
		return nil
	}

	steps := nextFractionalSteps(initialOld, currentOld, currentNew, targetNew, config)
	changes := calculateReplicaChanges(initialOld, currentOld, currentNew, targetNew, config, steps)
	postponeDrainWhileGrowthCanUnblock(&changes)
	if next := applyReplicaChanges(currentOld, currentNew, changes); next != nil {
		return next
	}
	return openReplacementSlot(currentOld, currentNew, targetNew, changes.drainHeadroom)
}

// nextFractionalSteps advances both sides on their respective fraction scales.
// MaxSurge lets the new side propose farther growth; MaxUnavailable lets the
// old side propose farther drain.
func nextFractionalSteps(
	initialOld, currentOld, currentNew, targetNew RoleReplicaState,
	config []RollingUpdateConfig,
) fractionalSteps {
	newStepCount := fractionalStepCount(targetNew)
	oldStepCount := fractionalStepCount(initialOld)

	// The least-advanced role selects the shared fractional step. Projecting
	// every role from that step with ceiling division keeps the ideal plan
	// within one replica of the smallest non-empty role. That is the KEP's
	// largestReplicaFraction.
	currentNewStep := leastAdvancedStep(currentNew, targetNew, newStepCount, false)
	currentOldStep := leastAdvancedStep(currentOld, initialOld, oldStepCount, true)

	// Each side normally advances by one fractional step. If the old side has
	// already drained farther, the new side catches up to the equivalent step
	// on its own scale.
	nextNewStep := min(max(
		currentNewStep+1,
		projectProgressStep(currentOldStep, oldStepCount, newStepCount),
	), newStepCount)
	nextOldStep := min(currentOldStep+1, oldStepCount)

	// The fractional schedule needs one shared lookahead even though budgets are
	// configured per role. Use the largest budget so a smaller budget does not
	// limit every role. This only widens the proposal: calculateReplicaChanges
	// still applies each role's own surge ceiling and availability floor.
	maxSurge, maxUnavailable := 0, 0
	for _, cfg := range config {
		maxSurge = max(maxSurge, cfg.MaxSurge)
		maxUnavailable = max(maxUnavailable, cfg.MaxUnavailable)
	}
	return fractionalSteps{
		newStep:         min(nextNewStep+maxSurge, newStepCount),
		newStepCount:    newStepCount,
		oldStep:         min(nextOldStep+maxUnavailable, oldStepCount),
		oldStepCount:    oldStepCount,
		budgetStepCount: max(newStepCount, oldStepCount),
	}
}

// calculateReplicaChanges projects the next fractional steps into replica
// counts, then caps growth by the surge ceiling and drain by the Spec floor.
// The executor applies the Ready-based safety limits after the planner returns.
func calculateReplicaChanges(
	initialOld, currentOld, currentNew, targetNew RoleReplicaState,
	config []RollingUpdateConfig,
	steps fractionalSteps,
) replicaChanges {
	changes := replicaChanges{
		growBy:         make(RoleReplicaState, len(initialOld)),
		scheduledDrain: make(RoleReplicaState, len(initialOld)),
		drainHeadroom:  make(RoleReplicaState, len(initialOld)),
		drainBy:        make(RoleReplicaState, len(initialOld)),
	}
	for i := range initialOld {
		roleSize := max(initialOld[i], targetNew[i])
		ceiling := roleSize + projectBudget(roleSize, config[i].MaxSurge, steps.budgetStepCount)
		floor := max(0, min(initialOld[i], targetNew[i])-
			projectBudget(roleSize, config[i].MaxUnavailable, steps.budgetStepCount))
		if config[i].MaxSurge == 0 && config[i].MaxUnavailable == 0 {
			ceiling++
		}

		total := currentOld[i] + currentNew[i]
		wantedNew := wantReplicas(targetNew[i], steps.newStep, steps.newStepCount, false)
		wantedOld := wantReplicas(initialOld[i], steps.oldStep, steps.oldStepCount, true)
		changes.growBy[i] = min(max(wantedNew-currentNew[i], 0), max(0, ceiling-total))
		changes.scheduledDrain[i] = max(0, currentOld[i]-wantedOld)
		changes.drainHeadroom[i] = max(0, total-floor)
		changes.drainBy[i] = min(changes.scheduledDrain[i], changes.drainHeadroom[i])
	}
	return changes
}

// postponeDrainWhileGrowthCanUnblock avoids applying only the drainable part
// of a shared fractional step. If growth can open headroom for a blocked role,
// it happens before any role drains. If no growth is possible, the floor-safe
// drain remains so zero-surge rollouts can continue.
func postponeDrainWhileGrowthCanUnblock(changes *replicaChanges) {
	blocked, draining := false, false
	for i := range changes.drainBy {
		blocked = blocked || changes.scheduledDrain[i] > 0 && changes.drainBy[i] == 0
		draining = draining || changes.drainBy[i] > 0
	}
	if blocked && draining && anyPositive(changes.growBy) {
		clear(changes.drainBy)
	}
}

func applyReplicaChanges(
	currentOld, currentNew RoleReplicaState,
	changes replicaChanges,
) *UpdateStep {
	nextOld := make(RoleReplicaState, len(currentOld))
	nextNew := make(RoleReplicaState, len(currentNew))
	for i := range currentOld {
		nextOld[i] = currentOld[i] - changes.drainBy[i]
		nextNew[i] = currentNew[i] + changes.growBy[i]
	}
	if !anyChange(nextOld, nextNew, currentOld, currentNew) {
		return nil
	}
	return &UpdateStep{Past: nextOld, New: nextNew}
}

// openReplacementSlot handles a rounding wedge when neither side otherwise
// changes. It drains one floor-safe old replica so replacement growth can
// start on a later iteration.
func openReplacementSlot(
	currentOld, currentNew, targetNew, drainHeadroom RoleReplicaState,
) *UpdateStep {
	for i := range currentOld {
		if currentOld[i] > 0 && currentNew[i] < targetNew[i] && drainHeadroom[i] > 0 {
			nextOld := append(RoleReplicaState(nil), currentOld...)
			nextNew := append(RoleReplicaState(nil), currentNew...)
			nextOld[i]--
			return &UpdateStep{Past: nextOld, New: nextNew}
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

func projectBudget(roleSize, budget, stepCount int) int {
	if roleSize <= 0 || budget <= 0 || stepCount <= 0 {
		return 0
	}
	return (roleSize*budget + stepCount - 1) / stepCount
}

func projectProgressStep(step, fromStepCount, toStepCount int) int {
	if step <= 0 || fromStepCount <= 0 || toStepCount <= 0 {
		return 0
	}
	return min((step*toStepCount+fromStepCount-1)/fromStepCount, toStepCount)
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

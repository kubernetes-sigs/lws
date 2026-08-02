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
// # Rolling update algorithm
//
// A rollout has two sides — OLD (draining) and NEW (scaling up) — that
// advance independently in discrete minUnit ticks. Per side:
//
//	totalSteps = max(role_sizes)   // finest granularity: one pod of the largest role
//	minUnit    = 1 / totalSteps    // side-level fraction per tick
//
// Example: on a 5P/2D side, minUnit = 20%. Prefill moves one pod per tick;
// decode holds at its current count for several ticks between its own moves.
//
// wantReplicas for a role at step k uses CEIL on both sides:
//   - NEW: ceil(roleSize × k / totalSteps)
//   - OLD: ceil(roleSize × (totalSteps−k) / totalSteps)
//
// Role ratio (5:2 stays ≈5:2 at every tick) is protected by two functions
// working together:
//   - sideProgress() returns min(step reached across roles) so the side
//     can't advance past its slowest role.
//   - wantReplicas() then gives each role its proportional share at that
//     shared step k (ceil × k / totalSteps).
//
// Ceil on the OLD side additionally enforces the aliveness invariant:
// every role holds at ≥1 while any other role is still draining. All
// roles reach 0 in the same final tick, never before.
//
// # Capacity envelope
//
// maxSurge and maxUnavailable remain absolute per-role API limits. The planner
// projects them onto the shared minUnit scale to choose ratio-preserving moves;
// consequently a role may intentionally use less than its allowed maximum:
//
//	surge_pods      = ceil(roleSize × maxSurge       / totalSteps)
//	unavail_pods    = ceil(roleSize × maxUnavailable / totalSteps)
//	planning_ceiling = max(initialOld, target) + surge_pods
//	planning_floor   = max(0, min(initialOld, target) - unavail_pods)
//
// The executor independently enforces the raw per-role MaxSurge ceiling and
// MaxUnavailable floor as hard safety bounds.
//
// # Per-tick step
//
// Each tick advances the baseline step by 1 (progressStep), then targets a
// spec step further ahead by the full budget so one reconcile fills the
// whole pipeline:
//
//	specTargetStep_new = progressStep + maxSurge
//	specTargetStep_old = progressStep + maxUnavail
//
// addBudget = planningCeiling − total and drainBudget = total −
// planningFloor cap the actual per-tick move.
package disaggregatedset

// RoleStepState reports the target replica count for one role at one step.
type RoleStepState struct {
	Replicas int
}

// UpdateStep is the planner's per-step output for both revisions.
type UpdateStep struct {
	Past map[string]RoleStepState // old revision target counts (drain to here)
	New  map[string]RoleStepState // new revision target counts (scale up to here)
}

// PlanStatus describes whether a planner invocation made progress, completed
// the rollout, or could not find a legal state transition from the observed
// state. Callers must not treat a blocked plan as completion.
type PlanStatus string

const (
	PlanProgress PlanStatus = "Progress"
	PlanComplete PlanStatus = "Complete"
	PlanBlocked  PlanStatus = "Blocked"
)

// PlanResult is the outcome of one planner invocation. Step is populated only
// when Status is PlanProgress.
type PlanResult struct {
	Status PlanStatus
	Step   *UpdateStep
}

// RollingUpdateConfig holds the per-role surge/unavailable budgets.
type RollingUpdateConfig struct {
	MaxSurge       int
	MaxUnavailable int
}

// sideSize returns the side's total step count = max(replica count across
// roles in the side). Returns 0 if the side has no work (all replicas 0).
// Using max gives the finest granularity: one minUnit = 1/max_role_size,
// matching the largest role's atomic pod. See package doc.
func sideSize(replicas map[string]int) int {
	maxN := 0
	for _, n := range replicas {
		if n > maxN {
			maxN = n
		}
	}
	return maxN
}

// sideProgress returns the step the slowest role on the side has reached —
// i.e. min across roles of "max step k for which the role's current count
// still matches wantReplicas at step k". `sizes` gives each role's anchor
// count (target for NEW, initial for OLD), matching wantReplicas' roleSize.
//
// Both sides use ceil in wantReplicas, so:
//   - NEW: role at step k iff current >= ceil(size*k/totalSteps). Max
//     reached = floor(current * totalSteps / size).
//   - OLD: role at step k iff current <= ceil(size*(totalSteps-k)/totalSteps).
//     A small role can "hold" at its current count for multiple consecutive
//     steps (its ideal remaining rounds the same). Max reached solves for
//     largest k where the ceil-want is still >= current, closed-form:
//     floor((totalSteps*(size - current + 1) - 1) / size).
func sideProgress(roles []string, current, sizes map[string]int, totalSteps int, drained bool) int {
	if totalSteps == 0 {
		return 0
	}
	minStepReached := totalSteps
	for _, role := range roles {
		size := sizes[role]
		if size == 0 {
			continue
		}
		count := current[role]
		var stepReached int
		if drained {
			if count > size {
				count = size
			}
			stepReached = (totalSteps*(size-count+1) - 1) / size
			if stepReached < 0 {
				stepReached = 0
			}
			if stepReached > totalSteps {
				stepReached = totalSteps
			}
		} else {
			stepReached = count * totalSteps / size
		}
		if stepReached < minStepReached {
			minStepReached = stepReached
		}
	}
	return minStepReached
}

// wantReplicas returns the smallest replica count for a role to be considered
// "at" side step k/N. Ceil on BOTH sides to preserve two invariants:
//   - NEW: a role is at step k iff (current * N) >= (k * target). Without ceil,
//     the rollout can stall when target/N is not an integer.
//   - OLD: a role holds at ≥1 while any progress remains, until the final
//     tick where ideal remaining rounds to 0. Prevents small roles from
//     hitting 0 before large roles finish draining (the aliveness invariant).
func wantReplicas(roleSize, step, totalSteps int, drained bool) int {
	if totalSteps == 0 {
		if drained {
			return roleSize
		}
		return 0
	}
	var num int
	if drained {
		// ceil(roleSize * (totalSteps - step) / totalSteps)
		num = roleSize * (totalSteps - step)
	} else {
		// ceil(roleSize * step / totalSteps)
		num = roleSize * step
	}
	return (num + totalSteps - 1) / totalSteps
}

// ComputeNextStep returns the outcome of planning the next reconcile.
//
// See the package doc for the algorithm. In short:
//  1. Find each side's progress (the slowest role's fraction-done step).
//  2. Aim to advance each side by one step (= one minimalUnit).
//  3. For each role, compute the desired count at that next step, then cap by
//     the surge / unavailable budgets.
func ComputeNextStep(
	roleNames []string,
	initialOld, currentOld, currentNew, targetNew map[string]int,
	config map[string]RollingUpdateConfig,
) PlanResult {
	if isComplete(roleNames, currentOld, currentNew, targetNew) {
		return PlanResult{Status: PlanComplete}
	}

	// 1. Each side's total step count = max role size on that side.
	newTotalSteps := sideSize(targetNew)
	oldTotalSteps := sideSize(initialOld)
	// Budget projections use the larger of the two so a role's slack is
	// consistent across both drain and grow. During pure image-change
	// rollouts (initialOld == target) they're equal.
	budgetSteps := max(newTotalSteps, oldTotalSteps)

	// 2. Two step concepts. progressStep is the baseline advance (+1 minUnit
	// per tick) used for completion detection. specTargetStep is where spec
	// can reach *this tick*: baseline + surge (NEW) / + unavail (OLD). This
	// lets each tick fill the full surge/unavail budget in one round-trip
	// instead of leaving it on the table.
	newProgressStep := min(sideProgress(roleNames, currentNew, targetNew, newTotalSteps, false)+1, newTotalSteps)
	oldProgressStep := min(sideProgress(roleNames, currentOld, initialOld, oldTotalSteps, true)+1, oldTotalSteps)
	newSurge, oldUnavail := 0, 0
	for _, role := range roleNames {
		if s := config[role].MaxSurge; s > newSurge {
			newSurge = s
		}
		if u := config[role].MaxUnavailable; u > oldUnavail {
			oldUnavail = u
		}
	}
	newSpecStep := min(newProgressStep+newSurge, newTotalSteps)
	oldSpecStep := min(oldProgressStep+oldUnavail, oldTotalSteps)

	// 3. Per role, compute desired counts and cap by budget.
	past := make(map[string]RoleStepState, len(roleNames))
	now := make(map[string]RoleStepState, len(roleNames))
	drainOldMap := make(map[string]int, len(roleNames))
	drainWantMap := make(map[string]int, len(roleNames))
	addNewMap := make(map[string]int, len(roleNames))

	for _, role := range roleNames {
		wantNew := wantReplicas(targetNew[role], newSpecStep, newTotalSteps, false)
		wantOld := wantReplicas(initialOld[role], oldSpecStep, oldTotalSteps, true)

		cfg := config[role]
		roleSize := max(initialOld[role], targetNew[role])
		// Project the per-role API limits onto the shared minUnit scale.
		// Largest role can use the full allowance; smaller roles intentionally
		// use less to keep the planned move proportional.
		surgePods := projectBudget(roleSize, cfg.MaxSurge, budgetSteps)
		unavailPods := projectBudget(roleSize, cfg.MaxUnavailable, budgetSteps)
		planningCeiling := roleSize + surgePods
		planningFloor := max(0, min(initialOld[role], targetNew[role])-unavailPods)
		if cfg.MaxSurge == 0 && cfg.MaxUnavailable == 0 {
			// Default: allow +1 above target so rollouts can still progress.
			planningCeiling = roleSize + 1
		}

		total := currentNew[role] + currentOld[role]
		addBudget := max(0, planningCeiling-total)
		drainBudget := max(0, total-planningFloor)

		addNewMap[role] = min(max(wantNew-currentNew[role], 0), addBudget)
		drainWantMap[role] = max(0, currentOld[role]-wantOld)
		drainOldMap[role] = min(max(drainWantMap[role], 0), drainBudget)
	}

	// Aliveness cap: if some role wants to drain but is budget-blocked, and
	// another role is actually draining this tick, hold both back while NEW is
	// growing. The added replicas open drain budget for the blocked role on the
	// next tick. If NEW cannot grow, keep the already budget-capped drain: it is
	// the only legal progress available and prevents a permanent no-op wedge.
	anyBlocked, anyDraining := false, false
	for _, role := range roleNames {
		if drainWantMap[role] > 0 && drainOldMap[role] == 0 {
			anyBlocked = true
		}
		if drainOldMap[role] > 0 {
			anyDraining = true
		}
	}
	if anyBlocked && anyDraining && anyPositive(addNewMap) {
		for role := range drainOldMap {
			drainOldMap[role] = 0
		}
	}

	for _, role := range roleNames {
		now[role] = RoleStepState{Replicas: currentNew[role] + addNewMap[role]}
		past[role] = RoleStepState{Replicas: currentOld[role] - drainOldMap[role]}
	}

	// No-op detection: if nothing changes, signal "no work this reconcile".
	if !anyChange(roleNames, past, now, currentOld, currentNew) {
		return PlanResult{Status: PlanBlocked}
	}
	return PlanResult{
		Status: PlanProgress,
		Step:   &UpdateStep{Past: past, New: now},
	}
}

func anyPositive(values map[string]int) bool {
	for _, value := range values {
		if value > 0 {
			return true
		}
	}
	return false
}

func isComplete(roleNames []string, currentOld, currentNew, targetNew map[string]int) bool {
	for _, role := range roleNames {
		if currentOld[role] != 0 || currentNew[role] < targetNew[role] {
			return false
		}
	}
	return true
}

func anyChange(roleNames []string, past, now map[string]RoleStepState, curOld, curNew map[string]int) bool {
	for _, role := range roleNames {
		if now[role].Replicas != curNew[role] || past[role].Replicas != curOld[role] {
			return true
		}
	}
	return false
}

// projectBudget converts an absolute per-role API limit into the proportional
// allowance used to plan a coordinated P:D move: ceil(roleSize × limit /
// totalSteps). The raw limit remains the executor's hard safety bound.
func projectBudget(roleSize, mult, totalSteps int) int {
	if totalSteps <= 0 || mult <= 0 || roleSize <= 0 {
		return 0
	}
	return (roleSize*mult + totalSteps - 1) / totalSteps
}

// ComputeAllSteps simulates a full rollout by repeatedly calling ComputeNextStep.
// Used in tests to validate the complete rollout sequence.
func ComputeAllSteps(
	roleNames []string,
	initialOld, target map[string]int,
	config map[string]RollingUpdateConfig,
) []UpdateStep {
	currentOld := make(map[string]int, len(roleNames))
	currentNew := make(map[string]int, len(roleNames))
	for _, role := range roleNames {
		currentOld[role] = initialOld[role]
		currentNew[role] = 0
	}

	maxReplicas := 0
	for _, role := range roleNames {
		maxReplicas = max(maxReplicas, initialOld[role], target[role])
	}
	maxSteps := maxReplicas*4 + 10

	initial := UpdateStep{
		Past: make(map[string]RoleStepState, len(roleNames)),
		New:  make(map[string]RoleStepState, len(roleNames)),
	}
	for _, role := range roleNames {
		initial.Past[role] = RoleStepState{Replicas: initialOld[role]}
		initial.New[role] = RoleStepState{Replicas: 0}
	}
	steps := []UpdateStep{initial}

	for range maxSteps {
		result := ComputeNextStep(roleNames, initialOld, currentOld, currentNew, target, config)
		if result.Status != PlanProgress {
			break
		}
		next := result.Step
		steps = append(steps, *next)
		for _, role := range roleNames {
			currentOld[role] = next.Past[role].Replicas
			currentNew[role] = next.New[role].Replicas
		}
	}
	return steps
}

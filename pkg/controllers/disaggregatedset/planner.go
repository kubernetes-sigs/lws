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
// # Rolling update algorithm
//
// A DisaggregatedSet rollout has several roles (e.g. prefill, decode) running
// at two revisions: the OLD one (being drained) and the NEW one (being scaled
// up). The planner decides, on every reconcile, how many replicas to add to
// the new revision and how many to drain from the old one for each role.
//
// ## The two sides
//
// We treat the rollout as two independent "sides":
//   - NEW: scales from 0 up to its target replica count.
//   - OLD: drains from its initial replica count down to 0.
//
// They are coupled only through the per-role capacity envelope below
// (surge ceiling, unavailable floor).
//
// ## minUnit and side ticks
//
// The side advances in discrete minUnit ticks:
//
//	minUnit    = 1 / max(role_sizes in side)
//	totalSteps = max(role_sizes in side)
//
// max (not min) so the tick matches the FINEST atomic move on the side —
// one pod of the largest role. Smaller roles absorb the tick via ceil-rounding
// and hold at their current count for multiple ticks between their own moves.
//
// For a 5P/2D side, minUnit = 1/5 = 20%. Side ticks at 0/20/40/60/80/100%.
// Prefill advances one pod per tick (0→1→2→3→4→5). Decode holds at 1
// through several ticks, jumping 0→1 at tick 1 and 1→2 at tick 3.
//
// ## Aliveness invariant
//
// OLD-side wantReplicas uses ceil (not floor) so a role with a small target
// holds at ≥1 while larger roles still have replicas to drain. All roles
// reach 0 simultaneously at the final tick, when the ideal remaining rounds
// to 0. This prevents the "half-populated revision" state (e.g. prefill=1,
// decode=0) which would break serving.
//
// ## Capacity envelope
//
// maxSurge and maxUnavailable are user-declared safety budgets, expressed as
// minUnit multipliers (side-level fractions), NOT raw per-role pod counts.
// Per role, they project as:
//
//	surge_pods   = ceil(role_size × maxSurge       / totalSteps)
//	unavail_pods = ceil(role_size × maxUnavailable / totalSteps)
//	ceiling      = max(initialOld, target) + surge_pods
//	floor        = max(0, min(initialOld, target) - unavail_pods)
//
// Smaller roles get proportionally smaller absolute slack; the largest role
// (which sets totalSteps) gets the full multiplier. This keeps the budget
// balanced against the side as a whole rather than granting each role the
// same absolute slack regardless of its size.
//
// ## Per-tick step model
//
// Two step concepts coexist:
//
//   - progressStep  = sideProgress + 1                          (baseline advance)
//   - specTargetStep_new = min(progressStep + maxSurge, N)      (NEW-side)
//   - specTargetStep_old = min(progressStep + maxUnavail, N)    (OLD-side)
//
// progressStep governs completion detection. specTargetStep drives wantNew /
// wantOld so each tick asks for the full surge / unavail budget's worth of
// deltas. addBudget / drainBudget still cap the actual per-tick delta:
//
//	total       = currentNew + currentOld
//	addBudget   = ceiling - total          (max new pods to add)
//	drainBudget = total - floor            (max old pods to drain)
//
// Baseline (surge=0, unavail=0) advances 1 minUnit per tick. With larger
// budgets, each tick fills the full pipeline in one API round-trip.
package disaggregatedset

// RoleStepState reports the target replica count for one role at one step.
// (Earlier revisions of this package also exposed sync-window indices here;
// they were removed when the planner switched to a side-progress model where
// sync windows are implicit in the replica counts.)
type RoleStepState struct {
	Replicas int
}

// UpdateStep is the planner's per-step output for both revisions.
type UpdateStep struct {
	Past map[string]RoleStepState // old revision target counts (drain to here)
	New  map[string]RoleStepState // new revision target counts (scale up to here)
}

// RollingUpdateConfig holds the per-role surge/unavailable budgets.
type RollingUpdateConfig struct {
	MaxSurge       int
	MaxUnavailable int
}

func DefaultRollingUpdateConfig(numRoles int) []RollingUpdateConfig {
	configs := make([]RollingUpdateConfig, numRoles)
	for i := range numRoles {
		configs[i].MaxSurge = 1
		configs[i].MaxUnavailable = 0
	}
	return configs
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
// still matches wantReplicas at step k".
//
// Both sides use ceil in wantReplicas, so:
//   - NEW: role at step k iff current >= ceil(target*k/totalSteps). Max
//     reached = floor(current * totalSteps / target).
//   - OLD: role at step k iff current <= ceil(initial*(totalSteps-k)/totalSteps).
//     A small role can "hold" at its current count for multiple consecutive
//     steps (its ideal remaining rounds the same). Max reached solves for
//     largest k where the ceil-want is still >= current, closed-form:
//     floor((totalSteps*(initial - current + 1) - 1) / initial).
func sideProgress(roles []string, current, total map[string]int, totalSteps int, drained bool) int {
	if totalSteps == 0 {
		return 0
	}
	minStep := totalSteps
	for _, role := range roles {
		denom := total[role]
		if denom == 0 {
			continue
		}
		cur := current[role]
		var s int
		if drained {
			if cur > denom {
				cur = denom
			}
			s = (totalSteps*(denom-cur+1) - 1) / denom
			if s < 0 {
				s = 0
			}
			if s > totalSteps {
				s = totalSteps
			}
		} else {
			s = cur * totalSteps / denom
		}
		if s < minStep {
			minStep = s
		}
	}
	return minStep
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

// ComputeNextStep returns the per-role deltas for the next reconcile, or nil
// if the rollout has reached its target.
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
) *UpdateStep {
	if isComplete(roleNames, currentOld, currentNew, targetNew) {
		return nil
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
		// Project MaxSurge/MaxUnavailable minUnit multipliers onto per-role
		// pod counts. Largest role gets the full multiplier; smaller roles
		// get proportionally less absolute slack.
		surgePods := projectBudget(roleSize, cfg.MaxSurge, budgetSteps)
		unavailPods := projectBudget(roleSize, cfg.MaxUnavailable, budgetSteps)
		ceiling := roleSize + surgePods
		floor := max(0, min(initialOld[role], targetNew[role])-unavailPods)
		if cfg.MaxSurge == 0 && cfg.MaxUnavailable == 0 {
			// Default: allow +1 above target so rollouts can still progress.
			ceiling = roleSize + 1
		}

		total := currentNew[role] + currentOld[role]
		addBudget := max(0, ceiling-total)
		drainBudget := max(0, total-floor)

		addNewMap[role] = clamp(wantNew-currentNew[role], 0, addBudget)
		drainWantMap[role] = max(0, currentOld[role]-wantOld)
		drainOldMap[role] = clamp(drainWantMap[role], 0, drainBudget)
	}

	// Aliveness cap: if some role wants to drain but is budget-blocked, and
	// another role is actually draining this tick, hold both back to keep
	// old-side roles in step. Prevents the executor from having to see
	// past[X]=0 while past[Y]>0 (which would over-drain via coordinated
	// retirement). Next tick, addNew grows total → drainBudget grows →
	// the blocked role can join in.
	anyBlocked, anyDraining := false, false
	for _, role := range roleNames {
		if drainWantMap[role] > 0 && drainOldMap[role] == 0 {
			anyBlocked = true
		}
		if drainOldMap[role] > 0 {
			anyDraining = true
		}
	}
	if anyBlocked && anyDraining {
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
		return nil
	}
	return &UpdateStep{Past: past, New: now}
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

func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// projectBudget converts a side-level minUnit multiplier (maxSurge or
// maxUnavailable) into a per-role pod count: ceil(roleSize × mult / totalSteps).
// The largest role (roleSize == totalSteps) gets the full multiplier;
// smaller roles get proportionally less absolute slack, keeping the budget
// balanced against the side as a whole.
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
		next := ComputeNextStep(roleNames, initialOld, currentOld, currentNew, target, config)
		if next == nil {
			break
		}
		steps = append(steps, *next)
		for _, role := range roleNames {
			currentOld[role] = next.Past[role].Replicas
			currentNew[role] = next.New[role].Replicas
		}
	}
	return steps
}

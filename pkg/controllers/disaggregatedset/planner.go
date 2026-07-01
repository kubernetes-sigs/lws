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
// (surge ceiling, unavailable floor) — there is no forced lockstep between
// "old has drained 50%" and "new has scaled to 50%".
//
// ## Side progress and step size
//
// Each side's progress is measured as the SLOWEST role's fraction of the work
// done. Concretely, if NEW has prefill 3/8 and decode 1/4, the side's progress
// is min(3/8, 1/4) = 1/4 — i.e. "every role has reached at least the 25%
// mark". The same idea for OLD, with "drained fraction" = (initial - current) / initial.
//
// Side advancement happens in discrete steps. Each step advances the side's
// progress by exactly 1/min(replicas in side), the smallest fraction the
// slowest role can express. For 8 prefill / 4 decode this is 1/4 → the side
// has 4 steps total at 25%, 50%, 75%, 100%.
//
// ## Capacity envelope
//
// maxSurge and maxUnavailable are the operator's safety budgets. Per role:
//
//	ceiling = max(initialOld, target) + maxSurge      // total spec cap
//	floor   = max(0, min(initialOld, target) - maxUnavailable)  // min ready
//
// Per step, deltas are capped by:
//
//	addBudget   = ceiling - currentTotal     (how many new we can add)
//	drainBudget = currentTotal - floor       (how many old we can drain)
//
// Larger budgets ⇒ bigger per-step deltas ⇒ faster rollout.
//
// ## ComputeNextStep, in five lines of pseudo-code
//
//	progress(side) = min role fraction across the side
//	nextStep = progress + 1/min(replicas in side)
//	for each role:
//	    want = round(target * nextStep)        # new side scales up
//	         = round(initial * (1 - nextStep)) # old side drains
//	    cap by addBudget / drainBudget; emit deltas
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

// sideSize returns the side's total step count = min(replica count across
// roles in the side). Returns 0 if the side has no work (all replicas 0).
func sideSize(replicas map[string]int) int {
	minN := 0
	for _, n := range replicas {
		if n > 0 && (minN == 0 || n < minN) {
			minN = n
		}
	}
	return minN
}

// sideProgress returns the step (out of totalSteps) the slowest role on the
// side has reached. "fractionFn" maps a role's (current, total) to the integer
// numerator of its fraction-of-work-done. Defined per-side:
//   - NEW: fractionDone = current / target
//   - OLD: fractionDrained = (initial - current) / initial
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
		num := current[role]
		if drained {
			num = denom - current[role]
		}
		// Step = floor(num * totalSteps / denom). Integer math, no rounding error.
		s := num * totalSteps / denom
		if s < minStep {
			minStep = s
		}
	}
	return minStep
}

// wantReplicas returns the smallest replica count for a role to be considered
// "at" side step k/N:
//   - NEW: ceil(target * k / N) — minimum count to claim progress = step k.
//   - OLD: floor(initial * (N - k) / N) — remaining after the matching drain.
//
// Using ceil on NEW (instead of floor) matches sideProgress's "strict" step
// formula: a role is at step k iff (current * N) >= (k * target). Without ceil,
// the rollout can stall when target/N is not an integer (e.g. target=8, N=6:
// the side wants step 1 but floor(8/6)=1 = current, so addNew=0 forever).
func wantReplicas(roleSize, step, totalSteps int, drained bool) int {
	if totalSteps == 0 {
		if drained {
			return roleSize
		}
		return 0
	}
	if drained {
		// floor(roleSize * (totalSteps - step) / totalSteps)
		return roleSize * (totalSteps - step) / totalSteps
	}
	// ceil(roleSize * step / totalSteps)
	num := roleSize * step
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

	// 1. Each side's total step count = 1 / minimalUnit.
	newTotalSteps := sideSize(targetNew)
	oldTotalSteps := sideSize(initialOld)

	// 2. Side progress = slowest role's step. Next step = +1 (capped).
	nextNewStep := min(sideProgress(roleNames, currentNew, targetNew, newTotalSteps, false)+1, newTotalSteps)
	nextOldStep := min(sideProgress(roleNames, currentOld, initialOld, oldTotalSteps, true)+1, oldTotalSteps)

	// 3. Per role, compute desired counts and cap by budget.
	past := make(map[string]RoleStepState, len(roleNames))
	now := make(map[string]RoleStepState, len(roleNames))

	for _, role := range roleNames {
		wantNew := wantReplicas(targetNew[role], nextNewStep, newTotalSteps, false)
		wantOld := wantReplicas(initialOld[role], nextOldStep, oldTotalSteps, true)

		cfg := config[role]
		ceiling := max(initialOld[role], targetNew[role]) + cfg.MaxSurge
		floor := max(0, min(initialOld[role], targetNew[role])-cfg.MaxUnavailable)
		if cfg.MaxSurge == 0 && cfg.MaxUnavailable == 0 {
			// Default: allow +1 above target so rollouts can still progress.
			ceiling = max(initialOld[role], targetNew[role]) + 1
		}

		total := currentNew[role] + currentOld[role]
		addBudget := max(0, ceiling-total)
		drainBudget := max(0, total-floor)

		addNew := clamp(wantNew-currentNew[role], 0, addBudget)
		drainOld := clamp(currentOld[role]-wantOld, 0, drainBudget)

		now[role] = RoleStepState{Replicas: currentNew[role] + addNew}
		past[role] = RoleStepState{Replicas: currentOld[role] - drainOld}
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

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
// # Rolling Update Algorithm — Percentage-Block Model
//
// Each revision is treated as a "block" with a progress percentage (0% → 100%).
// Each revision has its own minimalUnit (max(1/N_i) across its roles), and the
// global sync point interval is max(minimalUnit_old, minimalUnit_new).
//
// The planner uses a two-level step structure:
//
//   - Cross-role sync points (big steps): all roles must reach a sync point
//     before any can cross it.
//   - Per-role sub-steps (small steps): each role advances 1 replica at a time
//     between sync points, at its own granularity.
//
// Capacity constraints (per role):
//
//	ceiling: old + new <= max(initialOld, target) + maxSurge
//	floor:   old + new >= min(initialOld, target) - maxUnavailable
package disaggregatedset

// RoleStepState tracks a single role's position in the two-level step structure.
type RoleStepState struct {
	CrossRoleStep int // sync point index this role has reached
	RoleStep      int // sub-step index within the current sync point
	Replicas      int // target replica count for this role at this step
}

// UpdateStep is the planner output: the target replica counts per role for
// old and new revisions, along with their position in the step structure.
type UpdateStep struct {
	Past map[string]RoleStepState
	New  map[string]RoleStepState
}

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

// frac represents an exact rational number to avoid float precision issues.
type frac struct {
	num, den int
}

func (f frac) mul(n int) frac {
	return frac{f.num * n, f.den}
}

func floorFrac(f frac) int {
	return f.num / f.den
}

func ceilFrac(f frac) int {
	return (f.num + f.den - 1) / f.den
}

// revisionMinimalUnit returns max(1/N_i) for a single revision's replica counts.
func revisionMinimalUnit(replicas map[string]int) frac {
	minN := 0
	for _, n := range replicas {
		if n > 0 && (minN == 0 || n < minN) {
			minN = n
		}
	}
	if minN == 0 {
		return frac{1, 1}
	}
	return frac{1, minN}
}

func numSyncPoints(unit frac) int {
	return unit.den / unit.num
}

// newSyncTarget returns the target new-replica count for a role at a given
// new-side sync point.
func newSyncTarget(target int, syncPct frac) int {
	return floorFrac(frac{target * syncPct.num, syncPct.den})
}

// oldSyncTarget returns the target old-replica count for a role at a given
// old-side sync point. Old drains down, so this returns the REMAINING old count.
func oldSyncTarget(initialOld int, syncPct frac) int {
	remaining := frac{initialOld * (syncPct.den - syncPct.num), syncPct.den}
	return ceilFrac(remaining)
}

// deriveNewProgress computes each role's new-side sync position from the
// current new-replica count relative to the new revision's sync points.
// A role with target=0 (e.g. a removed role) is reported as fully done.
func deriveNewProgress(roleNames []string, targets, currentNew map[string]int, unit frac) map[string]RoleStepState {
	nSync := numSyncPoints(unit)
	states := make(map[string]RoleStepState, len(roleNames))
	for _, role := range roleNames {
		target := targets[role]
		cur := currentNew[role]
		if target == 0 {
			states[role] = RoleStepState{CrossRoleStep: nSync, RoleStep: 0, Replicas: cur}
			continue
		}
		crossStep := 0
		roleStep := 0
		for s := 1; s <= nSync; s++ {
			if cur >= newSyncTarget(target, unit.mul(s)) {
				crossStep = s
				roleStep = 0
			} else {
				prev := 0
				if s > 1 {
					prev = newSyncTarget(target, unit.mul(s-1))
				}
				roleStep = cur - prev
				break
			}
		}
		states[role] = RoleStepState{CrossRoleStep: crossStep, RoleStep: roleStep, Replicas: cur}
	}
	return states
}

// deriveOldProgress computes each role's old-side sync position. Old drains, so
// "progress" means "how much has been drained" — we measure curOld against
// per-sync oldTarget (remaining count). A role with initialOld=0 is fully done.
func deriveOldProgress(roleNames []string, initial, currentOld map[string]int, unit frac) map[string]RoleStepState {
	nSync := numSyncPoints(unit)
	states := make(map[string]RoleStepState, len(roleNames))
	for _, role := range roleNames {
		init := initial[role]
		cur := currentOld[role]
		if init == 0 {
			states[role] = RoleStepState{CrossRoleStep: nSync, RoleStep: 0, Replicas: cur}
			continue
		}
		crossStep := 0
		roleStep := 0
		for s := 1; s <= nSync; s++ {
			if cur <= oldSyncTarget(init, unit.mul(s)) {
				crossStep = s
				roleStep = 0
			} else {
				prev := init
				if s > 1 {
					prev = oldSyncTarget(init, unit.mul(s-1))
				}
				roleStep = prev - cur
				break
			}
		}
		states[role] = RoleStepState{CrossRoleStep: crossStep, RoleStep: roleStep, Replicas: cur}
	}
	return states
}

// ComputeNextStep computes the next scaling step for a rolling update.
//
// Two-minU model: old and new revisions progress at their own native rhythms
// (one minimalUnit each). Within a single revision, all roles coordinate at
// that revision's sync points. Across revisions, no forced lockstep — the
// maxSurge ceiling and maxUnavailable floor are the only cross-side coupling.
func ComputeNextStep(
	roleNames []string,
	initialOld, currentOld, currentNew, targetNew map[string]int,
	config map[string]RollingUpdateConfig,
) *UpdateStep {
	if isComplete(roleNames, currentOld, currentNew, targetNew) {
		return nil
	}

	oldUnit := revisionMinimalUnit(initialOld)
	newUnit := revisionMinimalUnit(targetNew)
	nNewSync := numSyncPoints(newUnit)
	nOldSync := numSyncPoints(oldUnit)

	newProgress := deriveNewProgress(roleNames, targetNew, currentNew, newUnit)
	oldProgress := deriveOldProgress(roleNames, initialOld, currentOld, oldUnit)

	// Per-side floor of progress: only roles AT this level can advance to the
	// next sync barrier on that side. Coordinates roles within a revision.
	minNewCross := nNewSync
	minOldCross := nOldSync
	for _, role := range roleNames {
		if newProgress[role].CrossRoleStep < minNewCross {
			minNewCross = newProgress[role].CrossRoleStep
		}
		if oldProgress[role].CrossRoleStep < minOldCross {
			minOldCross = oldProgress[role].CrossRoleStep
		}
	}

	nextNewSync := min(minNewCross+1, nNewSync)
	nextOldSync := min(minOldCross+1, nOldSync)
	newSyncPct := newUnit.mul(nextNewSync)
	oldSyncPct := oldUnit.mul(nextOldSync)

	pastStep := make(map[string]RoleStepState, len(roleNames))
	newStep := make(map[string]RoleStepState, len(roleNames))

	for _, role := range roleNames {
		target := targetNew[role]
		initial := initialOld[role]
		curNew := currentNew[role]
		curOld := currentOld[role]
		cfg := config[role]

		newTarget := newSyncTarget(target, newSyncPct)
		oldTarget := oldSyncTarget(initial, oldSyncPct)

		// Decide each side independently. A role parked at a higher
		// new-side sync barrier doesn't advance new (preserve within-new
		// role ratio). Same for old. The two parking decisions are
		// independent — a role can advance new but be parked on old, or
		// vice versa.
		newParked := newProgress[role].CrossRoleStep > minNewCross
		oldParked := oldProgress[role].CrossRoleStep > minOldCross

		// Capacity envelope for this role.
		ceiling := max(initial, target) + cfg.MaxSurge
		floor := max(0, min(initial, target)-cfg.MaxUnavailable)
		if cfg.MaxSurge == 0 && cfg.MaxUnavailable == 0 {
			ceiling = max(initial, target) + 1
		}

		currentTotal := curNew + curOld
		addBudget := max(0, ceiling-currentTotal)
		drainBudget := max(0, currentTotal-floor)

		addNew := 0
		if !newParked {
			addNew = max(0, min(newTarget-curNew, addBudget))
		}
		drainOld := 0
		if !oldParked {
			drainOld = max(0, min(curOld-oldTarget, drainBudget))
		}

		// Guarantee at least one operation when an unparked side has work
		// but its budget is zero at this instant (other side will free room
		// on the next reconcile).
		if addNew == 0 && drainOld == 0 {
			if !newParked && curNew < newTarget && currentTotal+1 <= ceiling {
				addNew = 1
			} else if !oldParked && curOld > oldTarget && currentTotal-1 >= floor {
				drainOld = 1
			}
		}

		nextNew := curNew + addNew
		nextOld := curOld - drainOld

		// Update each side's sync position separately.
		nextNewCross := newProgress[role].CrossRoleStep
		nextNewRoleStep := newProgress[role].RoleStep
		if !newParked {
			nextNewRoleStep++
			if nextNew >= newTarget {
				nextNewCross = nextNewSync
				nextNewRoleStep = 0
			}
		}
		nextOldCross := oldProgress[role].CrossRoleStep
		nextOldRoleStep := oldProgress[role].RoleStep
		if !oldParked {
			nextOldRoleStep++
			if nextOld <= oldTarget {
				nextOldCross = nextOldSync
				nextOldRoleStep = 0
			}
		}

		pastStep[role] = RoleStepState{
			CrossRoleStep: nextOldCross,
			RoleStep:      nextOldRoleStep,
			Replicas:      nextOld,
		}
		newStep[role] = RoleStepState{
			CrossRoleStep: nextNewCross,
			RoleStep:      nextNewRoleStep,
			Replicas:      nextNew,
		}
	}

	// Check if any role actually changed.
	changed := false
	for _, role := range roleNames {
		if newStep[role].Replicas != currentNew[role] || pastStep[role].Replicas != currentOld[role] {
			changed = true
			break
		}
	}
	if !changed {
		return nil
	}

	return &UpdateStep{Past: pastStep, New: newStep}
}

func isComplete(roleNames []string, currentOld, currentNew, targetNew map[string]int) bool {
	for _, role := range roleNames {
		if currentOld[role] != 0 || currentNew[role] < targetNew[role] {
			return false
		}
	}
	return true
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

	initialStep := UpdateStep{
		Past: make(map[string]RoleStepState, len(roleNames)),
		New:  make(map[string]RoleStepState, len(roleNames)),
	}
	for _, role := range roleNames {
		initialStep.Past[role] = RoleStepState{Replicas: initialOld[role]}
		initialStep.New[role] = RoleStepState{Replicas: 0}
	}
	steps := []UpdateStep{initialStep}

	for range maxSteps {
		nextStep := ComputeNextStep(roleNames, initialOld, currentOld, currentNew, target, config)
		if nextStep == nil {
			break
		}
		steps = append(steps, *nextStep)
		for _, role := range roleNames {
			currentOld[role] = nextStep.Past[role].Replicas
			currentNew[role] = nextStep.New[role].Replicas
		}
	}

	return steps
}

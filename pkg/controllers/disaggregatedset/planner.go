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

// globalMinimalUnit returns the coarsest minimalUnit across old and new revisions.
// This determines the sync point interval.
func globalMinimalUnit(initialOld, targetNew map[string]int) frac {
	oldUnit := revisionMinimalUnit(initialOld)
	newUnit := revisionMinimalUnit(targetNew)
	// max(1/a, 1/b) = 1/min(a,b)
	minDen := oldUnit.den
	if newUnit.den < minDen {
		minDen = newUnit.den
	}
	return frac{1, minDen}
}

func numSyncPoints(unit frac) int {
	return unit.den / unit.num
}

// syncTargets computes the target replica counts for a role at a given sync point.
// Each revision follows its own curve: new scales up, old drains down.
func syncTargets(target, initialOld int, syncPct frac) (newTarget, oldTarget int) {
	newTarget = floorFrac(frac{target * syncPct.num, syncPct.den})
	remaining := frac{initialOld * (syncPct.den - syncPct.num), syncPct.den}
	oldTarget = ceilFrac(remaining)
	return
}

// deriveRoleProgress computes the current cross-role step and role step for each role
// by examining current new replica counts relative to the sync point targets.
func deriveRoleProgress(
	roleNames []string,
	targets map[string]int,
	currentNew map[string]int,
	unit frac,
) map[string]RoleStepState {
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
			syncPct := unit.mul(s)
			newTarget, _ := syncTargets(target, 0, syncPct)
			if cur >= newTarget {
				crossStep = s
				roleStep = 0
			} else {
				prevTarget := 0
				if s > 1 {
					prevPct := unit.mul(s - 1)
					prevTarget, _ = syncTargets(target, 0, prevPct)
				}
				roleStep = cur - prevTarget
				break
			}
		}
		states[role] = RoleStepState{CrossRoleStep: crossStep, RoleStep: roleStep, Replicas: cur}
	}
	return states
}

// ComputeNextStep computes the next scaling step for a rolling update.
// It derives the current progress from replica counts, enforces sync point
// boundaries, and returns the next sub-step (or nil if the rollout is complete).
func ComputeNextStep(
	roleNames []string,
	initialOld, currentOld, currentNew, targetNew map[string]int,
	config map[string]RollingUpdateConfig,
) *UpdateStep {
	if isComplete(roleNames, currentOld, currentNew, targetNew) {
		return nil
	}

	unit := globalMinimalUnit(initialOld, targetNew)
	nSync := numSyncPoints(unit)

	progress := deriveRoleProgress(roleNames, targetNew, currentNew, unit)

	// Find the minimum cross-role step — only roles at this level can advance.
	minCrossStep := nSync
	for _, role := range roleNames {
		if progress[role].CrossRoleStep < minCrossStep {
			minCrossStep = progress[role].CrossRoleStep
		}
	}

	nextSyncIdx := minCrossStep + 1
	if nextSyncIdx > nSync {
		nextSyncIdx = nSync
	}
	syncPct := unit.mul(nextSyncIdx)

	pastStep := make(map[string]RoleStepState, len(roleNames))
	newStep := make(map[string]RoleStepState, len(roleNames))

	for _, role := range roleNames {
		target := targetNew[role]
		initial := initialOld[role]
		curNew := currentNew[role]
		curOld := currentOld[role]
		cfg := config[role]

		newTarget, oldTarget := syncTargets(target, initial, syncPct)

		// Only advance roles at the minimum sync point.
		if progress[role].CrossRoleStep > minCrossStep {
			pastStep[role] = RoleStepState{
				CrossRoleStep: progress[role].CrossRoleStep,
				RoleStep:      progress[role].RoleStep,
				Replicas:      curOld,
			}
			newStep[role] = RoleStepState{
				CrossRoleStep: progress[role].CrossRoleStep,
				RoleStep:      progress[role].RoleStep,
				Replicas:      curNew,
			}
			continue
		}

		// Advance new by 1 replica, drain old by 1 replica (toward sync targets).
		nextNew := min(curNew+1, newTarget)
		nextOld := max(curOld-1, oldTarget)

		// Capacity ceiling: old + new <= max(initialOld, target) + maxSurge.
		ceiling := max(initial, target) + cfg.MaxSurge
		if cfg.MaxSurge == 0 && cfg.MaxUnavailable == 0 {
			ceiling = max(initial, target) + 1
		}
		if nextNew+nextOld > ceiling {
			nextNew = curNew
		}

		// Capacity floor: old + new >= min(initialOld, target) - maxUnavailable.
		floor := max(0, min(initial, target)-cfg.MaxUnavailable)
		if nextNew+nextOld < floor {
			nextOld = curOld
		}

		roleProgress := progress[role]
		nextRoleStep := roleProgress.RoleStep + 1
		nextCrossStep := roleProgress.CrossRoleStep

		if nextNew >= newTarget && nextOld <= oldTarget {
			nextCrossStep = nextSyncIdx
			nextRoleStep = 0
		}

		pastStep[role] = RoleStepState{
			CrossRoleStep: nextCrossStep,
			RoleStep:      nextRoleStep,
			Replicas:      nextOld,
		}
		newStep[role] = RoleStepState{
			CrossRoleStep: nextCrossStep,
			RoleStep:      nextRoleStep,
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

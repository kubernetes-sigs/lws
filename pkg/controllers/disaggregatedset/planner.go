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

// Package disaggregatedset plans and executes DisaggregatedSet rolling updates.
package disaggregatedset

import "slices"

// A plan uses one recovery allowance at a time, never a combination of them.
type StepKind uint8

const (
	OrdinaryStep            StepKind = iota
	BootstrapSurgeStep               // One temporary surge slot to unblock coordination.
	UnavailableFallbackStep          // One extra unavailable replica after scheduler rejection.
	TargetRebalanceStep              // Reclaim safe surplus in a renamed-role target.
)

type UpdateStep struct {
	Past, New RoleReplicaState
	Kind      StepKind
}

// Planning and validation use the same ordinary bounds. Validation still checks
// the proposed counts independently: sharing bounds is not trusting the plan.
type rolloutPlan struct {
	snapshot                   rolloutSnapshot
	phaseTargets, growthLimits RoleReplicaState
	ordinary                   UpdateStep
}

func prepareRolloutPlan(state RolloutState) rolloutPlan {
	snapshot := snapshotForRolloutState(state)
	phase, limits := targetReplicasForActiveRevision(snapshot), hardNewReplicaLimits(snapshot)
	past := furthestOldTargets(snapshot)
	next := furthestNewTargets(snapshot, phase, limits)
	// Never spend the same Ready capacity on old and target drains together.
	if state.ScaleDownDuringRollout && slices.Equal(past, state.ActiveOld.SpecReplicas) {
		for i, replicas := range furthestTargetScaleDownTargets(snapshot) {
			if replicas < state.Target.SpecReplicas[i] {
				next[i] = replicas
			}
		}
	}
	return rolloutPlan{
		snapshot: snapshot, phaseTargets: phase, growthLimits: limits,
		ordinary: UpdateStep{Past: past, New: next},
	}
}

// ComputeNextStep prefers ordinary progress, then recovery of target surplus,
// then one bootstrap surge slot. Scheduler rejection is the last resort.
func ComputeNextStep(state RolloutState) *UpdateStep {
	if !validRolloutState(state) {
		return nil
	}
	plan := prepareRolloutPlan(state)
	if plan.changes(&plan.ordinary) {
		return &plan.ordinary
	}
	if step := disjointSurplusStep(plan.snapshot); step != nil {
		return step
	}
	if step := plan.bootstrapStep(); step != nil {
		return step
	}
	past := furthestOldTargets(snapshotWithUnavailableFallback(plan.snapshot))
	if !drainsOverSurgeUnschedulableRole(plan.snapshot, past, state.Target.UnschedulableRoles) {
		return nil
	}
	return &UpdateStep{Past: past, New: slicesClone(state.Target.SpecReplicas), Kind: UnavailableFallbackStep}
}

func (plan rolloutPlan) changes(step *UpdateStep) bool {
	return anyChange(step.Past, step.New, plan.snapshot.ActiveOld.SpecReplicas, plan.snapshot.Target.SpecReplicas)
}

func (plan rolloutPlan) bootstrapStep() *UpdateStep {
	snapshot, allowed := snapshotWithBootstrapSurge(plan.snapshot, plan.phaseTargets, plan.growthLimits)
	if !allowed {
		return nil
	}
	next := furthestNewTargets(snapshot, plan.phaseTargets, hardNewReplicaLimits(snapshot))
	step := &UpdateStep{Past: plan.ordinary.Past, New: next, Kind: BootstrapSurgeStep}
	if !plan.changes(step) {
		return nil
	}
	return step
}

// Parked revisions reduce this candidate's replacement work. Renamed roles
// instead need target-only progress; every required target role must start.
func targetReplicasForActiveRevision(snapshot rolloutSnapshot) RoleReplicaState {
	targets := make(RoleReplicaState, len(snapshot.Config))
	disjoint := false
	for i := range targets {
		disjoint = disjoint || snapshot.unreplaced(i) > 0
	}
	for i, desired := range snapshot.Target.DesiredReplicas {
		parked := max(0, snapshot.old.parkedReady[i].usable)
		if disjoint {
			parked = 0
		}
		residual := desired - parked
		if desired > 0 {
			residual = max(1, residual)
		}
		targets[i] = max(snapshot.Target.SpecReplicas[i], residual)
	}
	return targets
}

func furthestNewTargets(snapshot rolloutSnapshot, phase, limits RoleReplicaState) RoleReplicaState {
	targets := make(RoleReplicaState, len(phase))
	for i := range targets {
		targets[i] = min(phase[i], limits[i])
	}
	return boundGrowingSnapshotTargets(snapshot, phase, targets)
}

// Immediate scale-up fills requested capacity, not a readiness shortfall. Count
// all observed old Spec, including loading groups; a proposed drain frees no room.
func (snapshot rolloutSnapshot) immediateScaleUpTarget(role int) int {
	if !snapshot.ScaleUpDuringRollout {
		return 0
	}
	// A renamed role replaces differently named capacity. Do not mistake its
	// missing same-role history for a request to create its entire fleet at once.
	if snapshot.AvailabilityBaseline[role] == 0 && slices.ContainsFunc(snapshot.old.disjointSpec, func(n int) bool { return n > 0 }) {
		return 0
	}
	return max(0, snapshot.Target.DesiredReplicas[role]-snapshot.old.spec[role])
}

func boundGrowingSnapshotTargets(snapshot rolloutSnapshot, phase, proposed RoleReplicaState) RoleReplicaState {
	bounded := boundGrowingRoleTargetsToWindow(snapshot.Target.SpecReplicas, phase, proposed)
	for i := range bounded {
		// Only the missing requested capacity may bypass rollout pacing. Growth
		// above it still follows the ordinary fractional window and hard limits.
		bounded[i] = max(bounded[i], min(proposed[i], snapshot.immediateScaleUpTarget(i)))
	}
	return bounded
}

// Intersect surge, pending-readiness and renamed-role replacement limits.
// Already-issued replicas are never retracted by a growth calculation.
func hardNewReplicaLimits(snapshot rolloutSnapshot) RoleReplicaState {
	limits := make(RoleReplicaState, len(snapshot.Config))
	released := disjointGrowthLimits(snapshot)
	progress, hasTarget := coordinationWindowForProgress(snapshot.Target.DesiredReplicas, snapshot.Target.ReadyReplicas)
	steps, disjoint := 0, false
	for i := range limits {
		steps = max(steps, snapshot.ActiveOld.InitialReplicas[i], snapshot.Target.DesiredReplicas[i])
		disjoint = disjoint || snapshot.unreplaced(i) > 0
	}
	for i := range limits {
		limit, credit := snapshot.pendingGrowthLimit(i, progress, steps, disjoint)
		limit = max(limit, snapshot.immediateScaleUpTarget(i))
		if released != nil {
			replacement := released[i]
			if hasTarget && progress.leastAdvancedReplicas > 0 {
				// The final old groups may have to retire together. A complete
				// target may advance once, still within surge and pending limits.
				replacement = max(replacement, credit+1)
			}
			limit = min(limit, replacement)
			if hasTarget && progress.leastAdvancedReplicas == 0 {
				limit = min(limit, 1) // Start every role before adding a second group.
			}
		}
		limits[i] = max(snapshot.Target.SpecReplicas[i], min(snapshot.Target.DesiredReplicas[i], limit))
	}
	return limits
}

func (snapshot rolloutSnapshot) pendingGrowthLimit(role int, progress fractionalCoordinationWindow, steps int, disjoint bool) (limit, credit int) {
	limit, credit = snapshot.surgeCeiling(role)-snapshot.old.spec[role], snapshot.Target.ReadyReplicas[role]
	if snapshot.old.spec[role] <= 0 && !disjoint {
		return
	}
	if disjoint && progress.leastAdvancedTarget > 0 {
		// Faster readiness cannot fund another batch while a peer lags behind.
		credit = progress.readyCredit(snapshot.Target.DesiredReplicas[role])
	}
	config := snapshot.Config[role]
	replicas := max(snapshot.ActiveOld.InitialReplicas[role], snapshot.Target.DesiredReplicas[role])
	return min(limit, credit+projectBudget(replicas, config.MaxSurge+config.MaxUnavailable, steps)), credit
}

// Replacement progress counts actual old Spec reductions, not proposed drains.
// These fractions coordinate replica counts; they do not estimate GPU capacity.
func disjointGrowthLimits(snapshot rolloutSnapshot) RoleReplicaState {
	progress := fractionalCoordinationWindow{}
	hasOld, hasProgress := false, false
	for i, desired := range snapshot.Target.DesiredReplicas {
		if desired == 0 && snapshot.unreplaced(i) > 0 {
			baseline, spec := snapshot.AvailabilityBaseline[i], snapshot.old.disjointSpec[i]
			progress.includeProgress(baseline, max(0, baseline-spec))
			hasOld, hasProgress = hasOld || spec > 0, hasProgress || baseline > spec
		}
	}
	if !hasOld || progress.leastAdvancedTarget == 0 {
		return nil
	}
	limits := make(RoleReplicaState, len(snapshot.Config))
	for i, desired := range snapshot.Target.DesiredReplicas {
		numerator, denominator := int64(desired)*int64(progress.leastAdvancedReplicas), int64(progress.leastAdvancedTarget)
		limits[i] = int((numerator+denominator-1)/denominator) + snapshot.Config[i].MaxSurge
		if hasProgress && desired > 0 {
			limits[i] = max(1, limits[i]) // A surviving old singleton must not block startup.
		}
	}
	return limits
}

// Reclaim renamed-role surplus only within the ordinary target drain budget.
// Keep its last Ready group even if the native controller deletes Ready first.
func disjointSurplusStep(snapshot rolloutSnapshot) *UpdateStep {
	target := snapshot.Target
	limits := disjointGrowthLimits(snapshot)
	if limits == nil || allRequiredRolesPresent(target.RequiredRoles, target.ReadyReplicas) {
		return nil
	}
	budget, minimum := targetScaleDownBounds(snapshot)
	next := slicesClone(target.SpecReplicas)
	for i, desired := range target.DesiredReplicas {
		if desired > 0 && snapshot.AvailabilityBaseline[i] > 0 {
			return nil
		}
		keepReady := minimumSpecToPreserveReady(target.SpecReplicas[i], target.ReadyReplicas[i], min(1, target.ReadyReplicas[i]))
		next[i] = min(next[i], max(limits[i], min(1, target.SpecReplicas[i]), keepReady, minimum[i]))
	}
	if slices.Equal(next, target.SpecReplicas) || !budget.preserves(next) {
		return nil
	}
	return &UpdateStep{Past: slicesClone(snapshot.ActiveOld.SpecReplicas), New: next, Kind: TargetRebalanceStep}
}

// A first target group, or a fully Ready role blocked at the surge ceiling,
// may borrow one slot. Unready or already over-ceiling work cannot borrow again.
func snapshotWithBootstrapSurge(snapshot rolloutSnapshot, phase, normalLimits RoleReplicaState) (rolloutSnapshot, bool) {
	disjoint := disjointGrowthLimits(snapshot) != nil
	scaleUp, needed := demandExceedsOldBaseline(snapshot), false
	bootstrap := snapshot
	bootstrap.Config = slices.Clone(snapshot.Config)
	for i, config := range snapshot.Config {
		old, current := snapshot.old.spec[i], snapshot.Target.SpecReplicas[i]
		if disjoint && old == 0 {
			continue // A renamed role has no same-role old capacity to reclaim.
		}
		ceiling := snapshot.surgeCeiling(i)
		coordinationBlocked := (snapshot.ScaleDownDuringRollout || scaleUp) && old+current == ceiling && snapshot.Target.ReadyReplicas[i] >= current
		if config.MaxSurge+config.MaxUnavailable > 0 && phase[i] > current && normalLimits[i] == current &&
			(current == 0 || coordinationBlocked) {
			bootstrap.Config[i].MaxSurge = max(config.MaxSurge+1, config.MaxSurge+old-ceiling+1)
			needed = true
		}
	}
	return bootstrap, needed
}

// Persistent scheduler rejection may spend one extra unavailable replica, but
// never lower a positive availability floor to zero.
func snapshotWithUnavailableFallback(snapshot rolloutSnapshot) rolloutSnapshot {
	fallback := snapshot
	fallback.Config = slices.Clone(snapshot.Config)
	for i := range fallback.Config {
		if snapshot.availabilityFloor(i) > 1 {
			fallback.Config[i].MaxUnavailable++
		}
	}
	return fallback
}

func drainsOverSurgeUnschedulableRole(snapshot rolloutSnapshot, proposed RoleReplicaState, unschedulable []bool) bool {
	if len(proposed) != len(snapshot.Config) || len(unschedulable) != len(snapshot.Config) {
		return false
	}
	for i, rejected := range unschedulable {
		if rejected && snapshot.old.spec[i]+snapshot.Target.SpecReplicas[i] > snapshot.surgeCeiling(i) && proposed[i] < snapshot.ActiveOld.SpecReplicas[i] {
			return true
		}
	}
	return false
}

func demandExceedsOldBaseline(snapshot rolloutSnapshot) bool {
	for i, desired := range snapshot.Target.DesiredReplicas {
		if desired > snapshot.AvailabilityBaseline[i] {
			return true
		}
	}
	return false
}

// Historical drain pacing normally applies. A current role may release surplus
// beyond the latest ceiling during opt-in scaling or an increase in demand.
func boundDrainingSnapshotTargets(snapshot rolloutSnapshot, proposed RoleReplicaState) RoleReplicaState {
	old := snapshot.ActiveOld
	bounded := boundDrainingRoleTargetsToWindow(old.SpecReplicas, old.InitialReplicas, proposed)
	if snapshot.ScaleDownDuringRollout || demandExceedsOldBaseline(snapshot) {
		for i, desired := range snapshot.Target.DesiredReplicas {
			if desired > 0 {
				excess := max(0, snapshot.old.spec[i]+snapshot.Target.SpecReplicas[i]-snapshot.surgeCeiling(i))
				bounded[i] = min(bounded[i], max(proposed[i], old.SpecReplicas[i]-excess))
			}
		}
	}
	return bounded
}

// --- Fractional coordination ---

// fractionalCoordinationWindow describes how far one role may progress from
// the least-advanced role. Its lower edge is:
//
//	leastAdvancedReplicas / leastAdvancedTarget
//
// Its width is largestReplicaFraction from KEP 766:
//
//	largestReplicaFraction = 1 / largestReplicaFractionDenominator
type fractionalCoordinationWindow struct {
	leastAdvancedReplicas             int
	leastAdvancedTarget               int
	largestReplicaFractionDenominator int
}

// Convert the least-ready target fraction into whole replacement replicas.
func (window fractionalCoordinationWindow) readyCredit(replicas int) int {
	if window.leastAdvancedTarget == 0 {
		return 0
	}
	numerator := min(window.leastAdvancedReplicas, window.leastAdvancedTarget)
	return int(int64(replicas) * int64(numerator) / int64(window.leastAdvancedTarget))
}

// boundGrowingRoleTargetsToWindow keeps proposed growth within one
// largestReplicaFraction of the least-advanced role. It never reduces a
// current replica count, including when the observed state is already outside
// the window.
func boundGrowingRoleTargetsToWindow(
	current, roleReplicaCounts, proposed RoleReplicaState,
) RoleReplicaState {
	window, ok := coordinationWindowForProgress(roleReplicaCounts, proposed)
	if !ok {
		return proposed
	}

	bounded := make(RoleReplicaState, len(proposed))
	for i, roleReplicaCount := range roleReplicaCounts {
		windowLimit := window.maxProgressReplicasWithinWindow(roleReplicaCount)
		bounded[i] = max(current[i], min(proposed[i], windowLimit))
	}
	return bounded
}

// boundDrainingRoleTargetsToWindow keeps proposed old-side drain progress
// within one largestReplicaFraction of the least-advanced role. Old-side
// progress is the number of replicas removed from initialOld. The function
// never scales an old role back up when the observed state is already outside
// the window.
func boundDrainingRoleTargetsToWindow(
	currentOld, initialOld, proposedOld RoleReplicaState,
) RoleReplicaState {
	proposedDrained := make(RoleReplicaState, len(proposedOld))
	for i, initial := range initialOld {
		proposedDrained[i] = max(0, initial-min(proposedOld[i], initial))
	}

	window, ok := coordinationWindowForProgress(initialOld, proposedDrained)
	if !ok {
		return proposedOld
	}

	bounded := make(RoleReplicaState, len(proposedOld))
	for i, initial := range initialOld {
		maxDrainWithinWindow := window.maxProgressReplicasWithinWindow(initial)
		minRemainingWithinWindow := max(0, initial-maxDrainWithinWindow)
		bounded[i] = min(currentOld[i], max(proposedOld[i], minRemainingWithinWindow))
	}
	return bounded
}

// coordinationWindowForProgress finds the least-advanced proposed progress.
// The smallest positive role replica count defines the window width.
func coordinationWindowForProgress(
	roleReplicaCounts, proposedProgress RoleReplicaState,
) (fractionalCoordinationWindow, bool) {
	window := fractionalCoordinationWindow{}
	for i, roleReplicaCount := range roleReplicaCounts {
		window.includeProgress(roleReplicaCount, proposedProgress[i])
	}
	return window, window.leastAdvancedTarget > 0
}

func (window *fractionalCoordinationWindow) includeProgress(total, progress int) {
	if total <= 0 {
		return
	}
	if window.largestReplicaFractionDenominator == 0 || total < window.largestReplicaFractionDenominator {
		window.largestReplicaFractionDenominator = total
	}
	if window.leastAdvancedTarget == 0 ||
		int64(progress)*int64(window.leastAdvancedTarget) < int64(window.leastAdvancedReplicas)*int64(total) {
		window.leastAdvancedReplicas = progress
		window.leastAdvancedTarget = total
	}
}

// maxProgressReplicasWithinWindow returns the largest whole-replica progress
// count inside this window:
//
//	floor(roleReplicaCount * (leastAdvancedReplicas/leastAdvancedTarget +
//	                          1/largestReplicaFractionDenominator))
func (window fractionalCoordinationWindow) maxProgressReplicasWithinWindow(roleReplicaCount int) int {
	if roleReplicaCount <= 0 {
		return 0
	}

	// Split the lower-edge fraction into whole replicas and a remainder. This
	// avoids multiplying three API replica counts together, which could overflow
	// int64 at their maximum int32 values.
	scaledLowerEdge := int64(roleReplicaCount) * int64(window.leastAdvancedReplicas)
	wholeReplicas := scaledLowerEdge / int64(window.leastAdvancedTarget)
	lowerEdgeRemainder := scaledLowerEdge % int64(window.leastAdvancedTarget)
	replicasFromRemainderAndWindow :=
		(lowerEdgeRemainder*int64(window.largestReplicaFractionDenominator) +
			int64(roleReplicaCount)*int64(window.leastAdvancedTarget)) /
			(int64(window.leastAdvancedTarget) * int64(window.largestReplicaFractionDenominator))

	return min(roleReplicaCount, int(wholeReplicas+replicasFromRemainderAndWindow))
}

func anyChange(past, now, currentOld, currentNew RoleReplicaState) bool {
	for i := range past {
		if past[i] != currentOld[i] || now[i] != currentNew[i] {
			return true
		}
	}
	return false
}

func projectBudget(roleReplicaCount, budget, stepCount int) int {
	if roleReplicaCount <= 0 || budget <= 0 || stepCount <= 0 {
		return 0
	}
	return (roleReplicaCount*budget + stepCount - 1) / stepCount
}

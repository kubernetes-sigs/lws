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
// The window calculation stores the denominator of largestReplicaFraction in
// fractionalCoordinationWindow.largestReplicaFractionDenominator. It is the
// smallest positive role replica count on that side of the rollout.
//
// At step k, a role targets ceil(roleReplicaCount*k/stepCount) new replicas or
// ceil(roleReplicaCount*(stepCount-k)/stepCount) old replicas. The
// least-advanced role determines side progress, keeping role ratios within the
// rounding error of one replica (largestReplicaFraction).
//
// Spec represents replicas already requested from the LWS, including replicas
// that are still starting. The fractional schedule uses Spec counts to track
// rollout progress. ComputeNextStep then uses Ready counts to make that schedule
// safe to execute. MaxSurge and MaxUnavailable are projected onto the fraction
// scale for proportional planning and are also enforced as hard per-role limits.
package disaggregatedset

type UpdateStep struct {
	Past RoleReplicaState
	New  RoleReplicaState
}

// RoleReplicaState contains one replica count per role. The executor resolves
// role names before calling the planner, so this slice and all other per-role
// slices passed to the planner use the same role index.
type RoleReplicaState = []int

type RollingUpdateConfig struct {
	MaxSurge       int
	MaxUnavailable int
}

// roleRolloutSnapshot contains all observed state needed to plan one role. It
// is rebuilt on every reconciliation and is never persisted by the controller.
//
// InitialOldReplicas is the old-side baseline from the initial-replicas
// annotation. Spec counts replicas already requested from the LWS, including
// replicas that are still starting. Ready counts serving capacity and excludes
// replicas already committed to termination.
type roleRolloutSnapshot struct {
	InitialOldReplicas int
	OldSpecReplicas    int
	OldReadyReplicas   int
	NewSpecReplicas    int
	NewReadyReplicas   int
	NewTargetReplicas  int
	Config             RollingUpdateConfig
}

// rolloutSnapshot is index-aligned with the role-name slice used by the
// executor. The planner deliberately works on plain values rather than
// Kubernetes objects, which keeps the decision deterministic and testable.
type rolloutSnapshot []roleRolloutSnapshot

// fractionalSteps describes the planner's next position on the old and new
// fraction scales. Each side has its own step count because its role replica
// counts may differ from the other side.
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
	growBy  RoleReplicaState // New replicas allowed by the schedule and surge ceiling.
	drainBy RoleReplicaState // Scheduled old drain capped by the Spec floor.
}

// ComputeNextStep returns one rollout step that is ready to execute. Ready
// replicas in parked revisions reduce the fractional phase target, while the
// complete snapshot continues to enforce hard surge and availability limits.
// It returns nil when the rollout is complete or must wait for state to change.
func ComputeNextStep(snapshot rolloutSnapshot, parkedReadyReplicas RoleReplicaState) *UpdateStep {
	initialOld, currentOld, currentNew, targetNew, config := plannerInputs(snapshot)
	for i := range targetNew {
		if i < len(parkedReadyReplicas) {
			targetNew[i] = max(currentNew[i], targetNew[i]-parkedReadyReplicas[i])
		}
	}
	proposal := computeFractionalProposal(initialOld, currentOld, currentNew, targetNew, config)
	if proposal == nil {
		return nil
	}

	proposal.New = boundNewReplicaTargetsByHardLimits(snapshot, proposal.New)
	proposal.New = boundGrowingRoleTargetsToWindow(currentNew, targetNew, proposal.New)
	proposal.Past = boundOldReplicaTargetsByAvailability(snapshot, proposal.Past)
	proposal.Past = boundDrainingRoleTargetsToWindow(currentOld, initialOld, proposal.Past)
	if !anyChange(proposal.Past, proposal.New, currentOld, currentNew) {
		return nil
	}
	return proposal
}

// computeFractionalProposal calculates the next Spec-based schedule before
// observed readiness is considered. Callers execute ComputeNextStep instead.
func computeFractionalProposal(
	initialOld, currentOld, currentNew, targetNew RoleReplicaState,
	config []RollingUpdateConfig,
) *UpdateStep {
	if isComplete(currentOld, currentNew, targetNew) {
		return nil
	}

	steps := nextFractionalSteps(initialOld, currentOld, currentNew, targetNew, config)
	changes := calculateReplicaChanges(initialOld, currentOld, currentNew, targetNew, config, steps)
	next := applyReplicaChanges(currentOld, currentNew, changes)
	if next == nil {
		return nil
	}

	// Per-role surge and Spec-floor limits can trim different parts of the
	// shared fractional proposal. Reapply the window explicitly instead of
	// requiring every role to execute the exact same fractional step.
	next.New = boundGrowingRoleTargetsToWindow(currentNew, targetNew, next.New)
	next.Past = boundDrainingRoleTargetsToWindow(currentOld, initialOld, next.Past)
	if !anyChange(next.Past, next.New, currentOld, currentNew) {
		return nil
	}
	return next
}

// plannerInputs projects the full rollout snapshot onto the replica vectors
// used by the fractional arithmetic.
func plannerInputs(snapshot rolloutSnapshot) (
	initialOld, currentOld, currentNew, targetNew RoleReplicaState,
	config []RollingUpdateConfig,
) {
	initialOld = make(RoleReplicaState, len(snapshot))
	currentOld = make(RoleReplicaState, len(snapshot))
	currentNew = make(RoleReplicaState, len(snapshot))
	targetNew = make(RoleReplicaState, len(snapshot))
	config = make([]RollingUpdateConfig, len(snapshot))
	for i, role := range snapshot {
		initialOld[i] = role.InitialOldReplicas
		currentOld[i] = role.OldSpecReplicas
		currentNew[i] = role.NewSpecReplicas
		targetNew[i] = role.NewTargetReplicas
		config[i] = role.Config
	}
	return
}

// boundNewReplicaTargetsByHardLimits caps a fractional proposal by the
// absolute surge and pending-readiness limits. Existing Spec is never reduced,
// even if an externally modified LWS is already outside a limit.
func boundNewReplicaTargetsByHardLimits(
	snapshot rolloutSnapshot,
	plannerTargets RoleReplicaState,
) RoleReplicaState {
	hardLimits := hardNewReplicaLimits(snapshot)
	boundedTargets := make(RoleReplicaState, len(snapshot))
	for i, role := range snapshot {
		boundedTargets[i] = max(role.NewSpecReplicas, min(plannerTargets[i], hardLimits[i]))
	}
	return boundedTargets
}

// hardNewReplicaLimits returns the largest new-revision Spec currently
// permitted for each role. The formulas use the terminology from KEP 766:
//
//	roleReplicaCount = max(initialOld, target)
//	surgeCeiling     = roleReplicaCount + MaxSurge
//	pendingAllowance = projected(roleReplicaCount, MaxSurge + MaxUnavailable)
//
// Therefore newSpec cannot exceed either surgeCeiling-oldSpec or
// newReady+pendingAllowance. Fractional coordination is applied separately.
// coordinateRevisionDrain also uses these hard limits when replacement
// capacity is required to retire a complete old revision.
func hardNewReplicaLimits(snapshot rolloutSnapshot) RoleReplicaState {
	hardLimits := make(RoleReplicaState, len(snapshot))
	budgetSteps := 0
	for _, role := range snapshot {
		budgetSteps = max(budgetSteps, role.InitialOldReplicas, role.NewTargetReplicas)
	}
	for i, role := range snapshot {
		roleReplicaCount := max(role.InitialOldReplicas, role.NewTargetReplicas)
		surgeCeiling := roleReplicaCount + role.Config.MaxSurge
		newSpecAllowedBySurge := surgeCeiling - role.OldSpecReplicas

		pendingAllowance := projectBudget(
			roleReplicaCount,
			role.Config.MaxSurge+role.Config.MaxUnavailable,
			budgetSteps,
		)
		pendingReadinessCeiling := role.NewReadyReplicas + pendingAllowance

		limit := min(newSpecAllowedBySurge, pendingReadinessCeiling)
		hardLimits[i] = max(role.NewSpecReplicas, min(role.NewTargetReplicas, limit))
	}
	return hardLimits
}

// boundOldReplicaTargetsByAvailability caps a fractional drain proposal at the
// Ready-based availability floor observed for each role.
func boundOldReplicaTargetsByAvailability(
	snapshot rolloutSnapshot,
	plannerTargets RoleReplicaState,
) RoleReplicaState {
	boundedTargets := make(RoleReplicaState, len(snapshot))
	for i, role := range snapshot {
		requestedDrain := max(0, role.OldSpecReplicas-plannerTargets[i])
		safeDrain := min(requestedDrain, maxSafeDrain(role))
		boundedTargets[i] = role.OldSpecReplicas - safeDrain
	}
	return boundedTargets
}

// maxSafeDrain returns the number of old replicas that may be removed without
// crossing the hard per-role availability floor:
//
//	availabilityFloor = max(0, min(initialOld, target) - MaxUnavailable)
//
// Ready is capped at Spec while the snapshot is built, so terminating replicas
// cannot be spent twice.
func maxSafeDrain(role roleRolloutSnapshot) int {
	availabilityFloor := max(
		0,
		min(role.InitialOldReplicas, role.NewTargetReplicas)-role.Config.MaxUnavailable,
	)
	// Once the new revision satisfies the floor by itself, old availability is
	// no longer needed and every remaining old Spec replica may be removed. This
	// also lets the rollout clean up old replicas that are already unavailable.
	if role.NewReadyReplicas >= availabilityFloor {
		return role.OldSpecReplicas
	}
	committedReady := role.OldReadyReplicas + role.NewReadyReplicas
	readyReplicasAboveFloor := max(0, committedReady-availabilityFloor)
	return min(role.OldSpecReplicas, readyReplicasAboveFloor)
}

// --- Fractional coordination ---

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
func leastAdvancedStep(current, roleReplicaCounts RoleReplicaState, stepCount int, draining bool) int {
	if stepCount == 0 {
		return 0
	}
	progress := stepCount
	for i, roleReplicaCount := range roleReplicaCounts {
		if roleReplicaCount == 0 {
			continue
		}
		count := current[i]
		var roleProgress int
		if draining {
			count = min(count, roleReplicaCount)
			roleProgress = (stepCount*(roleReplicaCount-count+1) - 1) / roleReplicaCount
			roleProgress = min(max(roleProgress, 0), stepCount)
		} else {
			roleProgress = count * stepCount / roleReplicaCount
		}
		progress = min(progress, roleProgress)
	}
	return progress
}

// wantReplicas projects one fractional step onto a role's replica count.
func wantReplicas(roleReplicaCount, step, stepCount int, draining bool) int {
	if stepCount == 0 {
		if draining {
			return roleReplicaCount
		}
		return 0
	}
	if draining {
		step = stepCount - step
	}
	return (roleReplicaCount*step + stepCount - 1) / stepCount
}

// projectProgressStep expresses a step from one side of the rollout on the
// other side's fractional scale.
func projectProgressStep(step, fromStepCount, toStepCount int) int {
	if step <= 0 || fromStepCount <= 0 || toStepCount <= 0 {
		return 0
	}
	return min((step*toStepCount+fromStepCount-1)/fromStepCount, toStepCount)
}

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
		if roleReplicaCount <= 0 {
			continue
		}
		if window.largestReplicaFractionDenominator == 0 ||
			roleReplicaCount < window.largestReplicaFractionDenominator {
			window.largestReplicaFractionDenominator = roleReplicaCount
		}
		if window.leastAdvancedTarget == 0 ||
			int64(proposedProgress[i])*int64(window.leastAdvancedTarget) <
				int64(window.leastAdvancedReplicas)*int64(roleReplicaCount) {
			window.leastAdvancedReplicas = proposedProgress[i]
			window.leastAdvancedTarget = roleReplicaCount
		}
	}
	return window, window.leastAdvancedTarget > 0
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
// ComputeNextStep applies the Ready-based safety limits before returning.
func calculateReplicaChanges(
	initialOld, currentOld, currentNew, targetNew RoleReplicaState,
	config []RollingUpdateConfig,
	steps fractionalSteps,
) replicaChanges {
	changes := replicaChanges{
		growBy:  make(RoleReplicaState, len(initialOld)),
		drainBy: make(RoleReplicaState, len(initialOld)),
	}
	for i := range initialOld {
		roleReplicaCount := max(initialOld[i], targetNew[i])
		projectedSurgeBudget := projectBudget(roleReplicaCount, config[i].MaxSurge, steps.budgetStepCount)
		projectedUnavailableBudget := projectBudget(roleReplicaCount, config[i].MaxUnavailable, steps.budgetStepCount)

		surgeCeiling := roleReplicaCount + projectedSurgeBudget
		unclampedAvailabilityFloor := min(initialOld[i], targetNew[i]) - projectedUnavailableBudget
		availabilityFloor := max(0, unclampedAvailabilityFloor)
		if config[i].MaxSurge == 0 && config[i].MaxUnavailable == 0 {
			// Without either budget, the steady-state ceiling leaves no room
			// for a replacement. Add one provisional slot to the Spec proposal;
			// ComputeNextStep still applies the hard limits before returning it.
			surgeCeiling++
		}

		total := currentOld[i] + currentNew[i]
		wantedNew := wantReplicas(targetNew[i], steps.newStep, steps.newStepCount, false)
		wantedOld := wantReplicas(initialOld[i], steps.oldStep, steps.oldStepCount, true)
		changes.growBy[i] = min(max(wantedNew-currentNew[i], 0), max(0, surgeCeiling-total))
		changes.drainBy[i] = min(max(0, currentOld[i]-wantedOld), max(0, total-availabilityFloor))
	}
	return changes
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

func isRolloutSpecComplete(snapshot rolloutSnapshot) bool {
	for _, role := range snapshot {
		if role.OldSpecReplicas != 0 || role.NewSpecReplicas < role.NewTargetReplicas {
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

func projectBudget(roleReplicaCount, budget, stepCount int) int {
	if roleReplicaCount <= 0 || budget <= 0 || stepCount <= 0 {
		return 0
	}
	return (roleReplicaCount*budget + stepCount - 1) / stepCount
}

// ComputeAllSteps simulates a complete rollout for tests and plan-steps.
func ComputeAllSteps(initialOld, target RoleReplicaState, config []RollingUpdateConfig) []UpdateStep {
	snapshot := make(rolloutSnapshot, len(initialOld))
	for i := range initialOld {
		snapshot[i] = roleRolloutSnapshot{
			InitialOldReplicas: initialOld[i],
			OldSpecReplicas:    initialOld[i],
			OldReadyReplicas:   initialOld[i],
			NewTargetReplicas:  target[i],
			Config:             config[i],
		}
	}
	steps := []UpdateStep{{Past: append(RoleReplicaState(nil), initialOld...), New: make(RoleReplicaState, len(initialOld))}}

	for range max(fractionalStepCount(initialOld), fractionalStepCount(target))*4 + 10 {
		next := ComputeNextStep(snapshot, nil)
		if next == nil {
			break
		}
		steps = append(steps, *next)
		for i := range snapshot {
			snapshot[i].OldSpecReplicas = next.Past[i]
			snapshot[i].OldReadyReplicas = next.Past[i]
			snapshot[i].NewSpecReplicas = next.New[i]
			snapshot[i].NewReadyReplicas = next.New[i]
		}
	}
	return steps
}

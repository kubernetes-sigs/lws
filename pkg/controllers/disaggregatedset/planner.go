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
// ComputeNextStep intersects fractional coordination, surge, pending-readiness,
// availability, and revision-completeness bounds over observed Spec and Ready
// replicas. Bounded surge and availability fallbacks unblock capacity wedges.
package disaggregatedset

type UpdateStep struct {
	Past RoleReplicaState
	New  RoleReplicaState
	// UsesBootstrapSurge reports that New exceeds a configured surge ceiling
	// to create the first replica of a missing required target role.
	UsesBootstrapSurge bool
	// UsesUnavailableFallback reports that Past uses one additional unavailable
	// replica to release capacity for an unschedulable target role.
	UsesUnavailableFallback bool
}

// RoleReplicaState contains one replica count per role. The executor resolves
// role names before calling the planner, so this slice and all other per-role
// slices passed to the planner use the same role index.
type RoleReplicaState = []int

type RollingUpdateConfig struct {
	MaxSurge       int
	MaxUnavailable int
}

// ActiveRevisionState describes the one old revision considered by a planner
// call. RequiredRoles identifies the roles that must remain together until the
// revision is retired. Every slice in this type is index-aligned.
type ActiveRevisionState struct {
	RequiredRoles    []bool
	InitialReplicas  RoleReplicaState
	SpecReplicas     RoleReplicaState
	RawReadyReplicas RoleReplicaState
	ReadyReplicas    RoleReplicaState
}

// ParkedRevisionState describes one old revision that participates in global
// capacity accounting but is not mutated by this planner call. Keeping parked
// revisions separate is what makes Ready capacity revision-aware.
type ParkedRevisionState struct {
	RequiredRoles    []bool
	SpecReplicas     RoleReplicaState
	RawReadyReplicas RoleReplicaState
	ReadyReplicas    RoleReplicaState
}

// TargetRevisionState describes the revision being rolled out.
type TargetRevisionState struct {
	RequiredRoles      []bool
	SpecReplicas       RoleReplicaState
	RawReadyReplicas   RoleReplicaState
	ReadyReplicas      RoleReplicaState
	DesiredReplicas    RoleReplicaState
	UnschedulableRoles []bool
}

// RolloutState is the value-only input to revision-aware planning. Kubernetes
// objects and role names stay in the executor.
type RolloutState struct {
	ActiveOld ActiveRevisionState
	ParkedOld []ParkedRevisionState
	Target    TargetRevisionState
	// AvailabilityBaseline is shared by every old-revision candidate so
	// selecting a smaller interrupted revision cannot weaken maxUnavailable.
	AvailabilityBaseline RoleReplicaState
	Config               []RollingUpdateConfig
}

// roleRolloutSnapshot is the per-role observed state rebuilt each reconcile.
// Active fields describe the revision being replaced; aggregate Old fields
// also include parked revisions. Committed Ready excludes replicas reserved by
// a pending termination; observed Ready does not.
type roleRolloutSnapshot struct {
	InitialOldReplicas              int                 // Durable replica baseline of the active old revision.
	AvailabilityBaselineReplicas    int                 // Old-side baseline shared by every candidate for availability checks.
	ActiveOldSpecReplicas           int                 // Current Spec replicas of the active old revision.
	ActiveOldCommittedReadyReplicas int                 // Active-old Ready replicas not reserved by a pending deletion.
	ActiveOldUsableReadyReplicas    int                 // Ready replicas from the active revision, or zero when that revision is incomplete.
	OldSpecReplicas                 int                 // Current Spec replicas summed across all old revisions.
	OldUsableReadyReplicas          int                 // Ready replicas from complete old revisions only.
	ObservedUsableReadyReplicas     int                 // Raw Ready replicas from complete revisions before pending deletions are reserved.
	ObservedPerRoleReadyReplicas    int                 // Raw Ready from all structurally complete revisions.
	ReplacementPerRoleReadyReplicas int                 // Committed per-role Ready from structurally complete parked and target revisions.
	NewSpecReplicas                 int                 // Current Spec replicas of the target revision.
	NewCommittedReadyReplicas       int                 // Target Ready replicas excluding replicas committed to termination.
	NewUsableReadyReplicas          int                 // Target Ready replicas, or zero when a required role is not Ready.
	NewTargetReplicas               int                 // Desired target-revision replicas after rollout.
	Config                          RollingUpdateConfig // Surge and availability limits configured for this role.
}

// rolloutSnapshot is index-aligned with the role-name slice used by the
// executor. The planner deliberately works on plain values rather than
// Kubernetes objects, which keeps the decision deterministic and testable.
type rolloutSnapshot []roleRolloutSnapshot

// ComputeNextStep returns the furthest executable targets in the intersection
// of all rollout constraints. When that intersection contains no mutation, it
// may return a marked bootstrap-surge step for a missing target role. If a
// target role is scheduler-unschedulable, it may instead return a marked drain
// using one additional unavailable replica. Partial drains, whole-revision
// retirement, and replacement growth are planner decisions; the executor does
// not repair their targets.
func ComputeNextStep(state RolloutState) *UpdateStep {
	if !validRolloutState(state) {
		return nil
	}

	snapshot := snapshotForRolloutState(state)
	phaseTargets := targetReplicasForActiveRevision(snapshot)
	currentOld := slicesClone(state.ActiveOld.SpecReplicas)
	currentNew := slicesClone(state.Target.SpecReplicas)
	next := &UpdateStep{
		Past: furthestOldTargets(snapshot, state.ActiveOld.RequiredRoles),
		New:  furthestNewTargets(snapshot, phaseTargets),
	}
	if anyChange(next.Past, next.New, currentOld, currentNew) {
		return next
	}

	if bootstrapSnapshot, ok := snapshotWithBootstrapSurge(snapshot, phaseTargets); ok {
		bootstrapNew := furthestNewTargets(bootstrapSnapshot, phaseTargets)
		if anyChange(next.Past, bootstrapNew, currentOld, currentNew) {
			return &UpdateStep{
				Past:               next.Past,
				New:                bootstrapNew,
				UsesBootstrapSurge: true,
			}
		}
	}

	// A bootstrap Pod that the scheduler cannot place may need capacity held by
	// an old replica of the same role. As a last resort, permit one more
	// unavailable replica without reducing a positive availability floor to zero.
	fallbackSnapshot := snapshotWithUnavailableFallback(snapshot)
	fallbackPast := furthestOldTargets(fallbackSnapshot, state.ActiveOld.RequiredRoles)
	if !drainsOverSurgeUnschedulableRole(snapshot, currentOld, fallbackPast, state.Target.UnschedulableRoles) {
		return nil
	}
	return &UpdateStep{
		Past:                    fallbackPast,
		New:                     currentNew,
		UsesUnavailableFallback: true,
	}
}

// drainsOverSurgeUnschedulableRole reports whether the fallback releases an
// old replica for an unschedulable target role whose bootstrap replica still
// exceeds the configured surge ceiling.
func drainsOverSurgeUnschedulableRole(
	snapshot rolloutSnapshot,
	current, target RoleReplicaState,
	unschedulable []bool,
) bool {
	if len(snapshot) != len(current) || len(target) != len(current) || len(unschedulable) != len(current) {
		return false
	}
	for i, role := range snapshot {
		roleReplicaCount := max(role.InitialOldReplicas, role.NewTargetReplicas)
		surgeCeiling := roleReplicaCount + role.Config.MaxSurge
		if unschedulable[i] && role.OldSpecReplicas+role.NewSpecReplicas > surgeCeiling && target[i] < current[i] {
			return true
		}
	}
	return false
}

func validRolloutState(state RolloutState) bool {
	roleCount := len(state.Config)
	if len(state.ActiveOld.RequiredRoles) != roleCount ||
		len(state.ActiveOld.InitialReplicas) != roleCount ||
		len(state.ActiveOld.SpecReplicas) != roleCount ||
		len(state.ActiveOld.RawReadyReplicas) != roleCount ||
		len(state.ActiveOld.ReadyReplicas) != roleCount ||
		len(state.AvailabilityBaseline) != roleCount ||
		len(state.Target.RequiredRoles) != roleCount ||
		len(state.Target.SpecReplicas) != roleCount ||
		len(state.Target.RawReadyReplicas) != roleCount ||
		len(state.Target.ReadyReplicas) != roleCount ||
		len(state.Target.DesiredReplicas) != roleCount ||
		len(state.Target.UnschedulableRoles) != roleCount {
		return false
	}
	for _, revision := range state.ParkedOld {
		if len(revision.RequiredRoles) != roleCount ||
			len(revision.SpecReplicas) != roleCount ||
			len(revision.RawReadyReplicas) != roleCount ||
			len(revision.ReadyReplicas) != roleCount {
			return false
		}
	}
	return true
}

func snapshotForRolloutState(state RolloutState) rolloutSnapshot {
	activeUsableReady := usableReadyReplicas(state.ActiveOld.RequiredRoles, state.ActiveOld.ReadyReplicas)
	activeObservedUsableReady := usableReadyReplicas(state.ActiveOld.RequiredRoles, state.ActiveOld.RawReadyReplicas)
	activeObservedPerRoleReady := structurallyCompleteReadyReplicas(
		state.ActiveOld.RequiredRoles, state.ActiveOld.SpecReplicas, state.ActiveOld.RawReadyReplicas,
	)
	targetUsableReady := usableReadyReplicas(state.Target.RequiredRoles, state.Target.ReadyReplicas)
	targetObservedUsableReady := usableReadyReplicas(state.Target.RequiredRoles, state.Target.RawReadyReplicas)
	targetPerRoleReady := structurallyCompleteReadyReplicas(
		state.Target.RequiredRoles, state.Target.SpecReplicas, state.Target.ReadyReplicas,
	)
	targetObservedPerRoleReady := structurallyCompleteReadyReplicas(
		state.Target.RequiredRoles, state.Target.SpecReplicas, state.Target.RawReadyReplicas,
	)
	parkedSpec := make(RoleReplicaState, len(state.Config))
	parkedUsableReady := make(RoleReplicaState, len(state.Config))
	parkedObservedUsableReady := make(RoleReplicaState, len(state.Config))
	parkedPerRoleReady := make(RoleReplicaState, len(state.Config))
	parkedObservedPerRoleReady := make(RoleReplicaState, len(state.Config))
	for _, revision := range state.ParkedOld {
		ready := usableReadyReplicas(revision.RequiredRoles, revision.ReadyReplicas)
		observedReady := usableReadyReplicas(revision.RequiredRoles, revision.RawReadyReplicas)
		perRoleReady := structurallyCompleteReadyReplicas(
			revision.RequiredRoles, revision.SpecReplicas, revision.ReadyReplicas,
		)
		observedPerRoleReady := structurallyCompleteReadyReplicas(
			revision.RequiredRoles, revision.SpecReplicas, revision.RawReadyReplicas,
		)
		for i := range parkedSpec {
			parkedSpec[i] += revision.SpecReplicas[i]
			parkedUsableReady[i] += ready[i]
			parkedObservedUsableReady[i] += observedReady[i]
			parkedPerRoleReady[i] += perRoleReady[i]
			parkedObservedPerRoleReady[i] += observedPerRoleReady[i]
		}
	}

	snapshot := make(rolloutSnapshot, len(state.Config))
	for i := range snapshot {
		snapshot[i] = roleRolloutSnapshot{
			InitialOldReplicas:              state.ActiveOld.InitialReplicas[i],
			AvailabilityBaselineReplicas:    state.AvailabilityBaseline[i],
			ActiveOldSpecReplicas:           state.ActiveOld.SpecReplicas[i],
			ActiveOldCommittedReadyReplicas: state.ActiveOld.ReadyReplicas[i],
			ActiveOldUsableReadyReplicas:    activeUsableReady[i],
			OldSpecReplicas:                 state.ActiveOld.SpecReplicas[i] + parkedSpec[i],
			OldUsableReadyReplicas:          activeUsableReady[i] + parkedUsableReady[i],
			ObservedUsableReadyReplicas: activeObservedUsableReady[i] + parkedObservedUsableReady[i] +
				targetObservedUsableReady[i],
			ObservedPerRoleReadyReplicas: activeObservedPerRoleReady[i] +
				parkedObservedPerRoleReady[i] + targetObservedPerRoleReady[i],
			ReplacementPerRoleReadyReplicas: parkedPerRoleReady[i] + targetPerRoleReady[i],
			NewSpecReplicas:                 state.Target.SpecReplicas[i],
			NewCommittedReadyReplicas:       state.Target.ReadyReplicas[i],
			NewUsableReadyReplicas:          targetUsableReady[i],
			NewTargetReplicas:               state.Target.DesiredReplicas[i],
			Config:                          state.Config[i],
		}
	}
	return snapshot
}

// structurallyCompleteReadyReplicas keeps per-role readiness independent while
// excluding orphaned roles from revisions that can no longer become usable.
func structurallyCompleteReadyReplicas(
	requiredRoles []bool,
	specReplicas, readyReplicas RoleReplicaState,
) RoleReplicaState {
	ready := make(RoleReplicaState, len(readyReplicas))
	if allRequiredRolesPresent(requiredRoles, specReplicas) {
		copy(ready, readyReplicas)
	}
	return ready
}

// usableReadyReplicas returns no capacity until every required role has at
// least one committed Ready replica. This prevents one role from a partial
// revision from authorizing retirement of its counterpart in another revision.
func usableReadyReplicas(requiredRoles []bool, readyReplicas RoleReplicaState) RoleReplicaState {
	usable := make(RoleReplicaState, len(readyReplicas))
	if !allRequiredRolesPresent(requiredRoles, readyReplicas) {
		return usable
	}
	copy(usable, readyReplicas)
	return usable
}

// allRequiredRolesPresent reports whether at least one role is required and
// every required role has a positive replica count.
func allRequiredRolesPresent(requiredRoles []bool, replicas RoleReplicaState) bool {
	hasRequiredRole := false
	for i, required := range requiredRoles {
		if !required {
			continue
		}
		hasRequiredRole = true
		if replicas[i] == 0 {
			return false
		}
	}
	return hasRequiredRole
}

// targetReplicasForActiveRevision subtracts capacity supplied by complete
// parked revisions. The target revision replaces only the active old revision
// during this planner call, but still needs one replica of every required role
// so its roles can form a usable same-revision unit.
func targetReplicasForActiveRevision(snapshot rolloutSnapshot) RoleReplicaState {
	targets := make(RoleReplicaState, len(snapshot))
	for i, role := range snapshot {
		parkedReady := max(0, role.OldUsableReadyReplicas-role.ActiveOldUsableReadyReplicas)
		residualTarget := role.NewTargetReplicas - parkedReady
		if role.NewTargetReplicas > 0 {
			residualTarget = max(1, residualTarget)
		}
		targets[i] = max(role.NewSpecReplicas, residualTarget)
	}
	return targets
}

// furthestNewTargets returns the highest target-revision Specs permitted by
// the phase target, hard rollout bounds, and new-side coordination window.
func furthestNewTargets(snapshot rolloutSnapshot, phaseTargets RoleReplicaState) RoleReplicaState {
	current := make(RoleReplicaState, len(snapshot))
	limits := hardNewReplicaLimits(snapshot)
	targets := make(RoleReplicaState, len(snapshot))
	for i, role := range snapshot {
		current[i] = role.NewSpecReplicas
		targets[i] = min(phaseTargets[i], limits[i])
	}
	return boundGrowingRoleTargetsToWindow(current, phaseTargets, targets)
}

// furthestOldTargets returns the lowest active-old Specs permitted by observed
// availability, revision completeness, and the old-side coordination window.
func furthestOldTargets(snapshot rolloutSnapshot, requiredRoles []bool) RoleReplicaState {
	current := make(RoleReplicaState, len(snapshot))
	initial := make(RoleReplicaState, len(snapshot))
	targets := make(RoleReplicaState, len(snapshot))
	for i, role := range snapshot {
		current[i] = role.ActiveOldSpecReplicas
		initial[i] = role.InitialOldReplicas
	}

	// Retiring the complete active revision is the furthest possible drain. If
	// that would lose usable capacity, derive the minimum surviving Spec directly
	// from the Ready capacity that this revision must continue to provide.
	if !availabilityPreserved(snapshot, targets, requiredRoles) {
		activeUsableReadyToPreserve := make(RoleReplicaState, len(snapshot))
		activeMustRemainUsable := false
		for i, role := range snapshot {
			replacementReady := replacementReadyReplicas(role)
			minimumUsableReady := min(role.ObservedUsableReadyReplicas, availabilityFloor(role))
			activeUsableReadyToPreserve[i] = max(0, minimumUsableReady-replacementReady)
			activeMustRemainUsable = activeMustRemainUsable || activeUsableReadyToPreserve[i] > 0
		}

		for i, role := range snapshot {
			if activeMustRemainUsable && requiredRoles[i] && current[i] > 0 {
				activeUsableReadyToPreserve[i] = max(1, activeUsableReadyToPreserve[i])
			}
			minimumUsableSpec := minimumSpecToPreserveReady(
				current[i], role.ActiveOldUsableReadyReplicas, activeUsableReadyToPreserve[i],
			)

			// Revision-complete readiness above decides whether another revision
			// can replace this one. Independently, do not delete this role's own
			// Ready replicas just because another required role is temporarily
			// unready and made the whole revision unusable.
			minimumRoleReady := min(
				role.ObservedPerRoleReadyReplicas,
				availabilityFloor(role),
			)
			activeRoleReadyToPreserve := max(0, minimumRoleReady-role.ReplacementPerRoleReadyReplicas)
			minimumRoleSpec := minimumSpecToPreserveReady(
				current[i], role.ActiveOldCommittedReadyReplicas, activeRoleReadyToPreserve,
			)
			targets[i] = max(minimumUsableSpec, minimumRoleSpec)
		}
	}
	targets = boundDrainingRoleTargetsToWindow(current, initial, targets)
	targets = boundOldTargetsByRevisionCompleteness(current, targets, requiredRoles)
	targets = boundDrainingRoleTargetsToWindow(current, initial, targets)
	// The shared floor may cover a role that this candidate never contained.
	// In that case the candidate cannot make an unrelated partial drain safe.
	if !availabilityPreserved(snapshot, targets, requiredRoles) {
		return current
	}
	return targets
}

// minimumSpecToPreserveReady returns the lowest Spec that cannot remove more
// committed Ready replicas than the active revision can afford to lose. When
// none of its readiness is needed, readiness places no lower bound on Spec.
func minimumSpecToPreserveReady(currentSpec, committedReady, readyToPreserve int) int {
	if readyToPreserve == 0 {
		return 0
	}
	return max(0, currentSpec-max(0, committedReady-readyToPreserve))
}

// replacementReadyReplicas returns committed Ready capacity supplied by
// complete parked and target revisions.
func replacementReadyReplicas(role roleRolloutSnapshot) int {
	parkedReady := role.OldUsableReadyReplicas - role.ActiveOldUsableReadyReplicas
	return parkedReady + role.NewUsableReadyReplicas
}

// boundOldTargetsByRevisionCompleteness makes the surviving required roles a
// single retirement unit. If they cannot all reach zero in this step, each one
// keeps at least one replica. A role already absent after a partial API update
// cannot be restored and is therefore not part of the surviving unit.
func boundOldTargetsByRevisionCompleteness(
	current, proposed RoleReplicaState,
	requiredRoles []bool,
) RoleReplicaState {
	allRetired := true
	for i, required := range requiredRoles {
		if required && current[i] > 0 && proposed[i] > 0 {
			allRetired = false
			break
		}
	}
	if allRetired {
		return proposed
	}

	bounded := slicesClone(proposed)
	for i, required := range requiredRoles {
		if required && current[i] > 0 {
			bounded[i] = max(1, bounded[i])
		}
	}
	return bounded
}

func slicesClone(values RoleReplicaState) RoleReplicaState {
	return append(RoleReplicaState(nil), values...)
}

// hardNewReplicaLimits returns the largest new-revision Spec currently
// permitted for each role. The formulas use the terminology from KEP 766:
//
//	roleReplicaCount = max(initialOld, target)
//	surgeCeiling     = roleReplicaCount + MaxSurge
//	pendingAllowance = projected(roleReplicaCount, MaxSurge + MaxUnavailable)
//
// While old Spec remains, newSpec cannot exceed either
// surgeCeiling-oldSpec or newReady+pendingAllowance. Once the role has no old
// Spec left, waiting for target readiness cannot preserve old availability, so
// only the surge ceiling and desired target remain. Fractional coordination is
// applied separately.
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

		limit := newSpecAllowedBySurge
		if role.OldSpecReplicas > 0 {
			pendingAllowance := projectBudget(
				roleReplicaCount,
				role.Config.MaxSurge+role.Config.MaxUnavailable,
				budgetSteps,
			)
			pendingReadinessCeiling := role.NewCommittedReadyReplicas + pendingAllowance
			limit = min(limit, pendingReadinessCeiling)
		}
		hardLimits[i] = max(role.NewSpecReplicas, min(role.NewTargetReplicas, limit))
	}
	return hardLimits
}

// snapshotWithBootstrapSurge grants enough temporary surge for the first
// replica of each missing required target role. This includes interrupted
// rollouts where parked old revisions already consume a positive MaxSurge.
// ComputeNextStep considers this only when no ordinary mutation exists. Once
// the first replica is issued, the role is no longer eligible, so the planner
// waits for it instead of creating another emergency replica.
func snapshotWithBootstrapSurge(
	snapshot rolloutSnapshot,
	phaseTargets RoleReplicaState,
) (rolloutSnapshot, bool) {
	normalLimits := hardNewReplicaLimits(snapshot)
	bootstrap := append(rolloutSnapshot(nil), snapshot...)
	needed := false
	for i, role := range snapshot {
		if role.Config.MaxSurge+role.Config.MaxUnavailable > 0 &&
			role.NewSpecReplicas == 0 && phaseTargets[i] > 0 && normalLimits[i] == 0 {
			roleReplicaCount := max(role.InitialOldReplicas, role.NewTargetReplicas)
			bootstrap[i].Config.MaxSurge = max(
				role.Config.MaxSurge+1,
				role.OldSpecReplicas-roleReplicaCount+1,
			)
			needed = true
		}
	}
	return bootstrap, needed
}

// snapshotWithUnavailableFallback temporarily adds one MaxUnavailable replica
// where doing so preserves at least one available replica. The planner uses
// this view only after a sustained scheduler rejection and only when ordinary
// and bootstrap steps are both blocked.
func snapshotWithUnavailableFallback(snapshot rolloutSnapshot) rolloutSnapshot {
	fallback := append(rolloutSnapshot(nil), snapshot...)
	for i, role := range fallback {
		if availabilityFloor(role) > 1 {
			fallback[i].Config.MaxUnavailable++
		}
	}
	return fallback
}

// availabilityFloor is the minimum committed Ready capacity required for one
// role in the observed rollout state.
func availabilityFloor(role roleRolloutSnapshot) int {
	return max(
		0,
		min(role.AvailabilityBaselineReplicas, role.NewTargetReplicas)-role.Config.MaxUnavailable,
	)
}

// availabilityPreserved checks the worst case after the requested Spec drain:
// every removed replica may have been Ready. Complete revisions determine
// serving capacity, while per-role readiness is preserved independently so a
// transient dip in one role cannot authorize deletion of another role's Ready
// replicas. Committed Ready evaluates the post-drain state so replicas already
// pending deletion cannot be spent again. A step with no drain is always safe.
func availabilityPreserved(
	snapshot rolloutSnapshot,
	oldTargets RoleReplicaState,
	requiredRoles []bool,
) bool {
	draining := false
	for i, role := range snapshot {
		if oldTargets[i] < role.ActiveOldSpecReplicas {
			draining = true
			break
		}
	}
	if !draining {
		return true
	}

	activeReadyAfter := make(RoleReplicaState, len(snapshot))
	if allRequiredRolesPresent(requiredRoles, oldTargets) {
		for i, role := range snapshot {
			drain := role.ActiveOldSpecReplicas - oldTargets[i]
			activeReadyAfter[i] = max(0, role.ActiveOldUsableReadyReplicas-drain)
			if requiredRoles[i] && oldTargets[i] > 0 && activeReadyAfter[i] == 0 {
				clear(activeReadyAfter)
				break
			}
		}
	}

	for i, role := range snapshot {
		drain := role.ActiveOldSpecReplicas - oldTargets[i]
		replacementReady := replacementReadyReplicas(role)
		minimumUsableAfter := min(role.ObservedUsableReadyReplicas, availabilityFloor(role))
		// Do not reject an unrelated drain for a readiness gap that was already
		// reserved by an in-flight deletion on this unchanged role.
		if !(drain == 0 && activeReadyAfter[i] > 0) &&
			replacementReady+activeReadyAfter[i] < minimumUsableAfter {
			return false
		}

		activeRoleReadyAfter := max(0, role.ActiveOldCommittedReadyReplicas-drain)
		minimumRoleReadyAfter := min(
			role.ObservedPerRoleReadyReplicas,
			availabilityFloor(role),
		)
		// Only draining this role can reduce its own Ready replica count.
		if drain > 0 && role.ReplacementPerRoleReadyReplicas+activeRoleReadyAfter < minimumRoleReadyAfter {
			return false
		}
	}
	return true
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
	requiredOld := make([]bool, len(initialOld))
	requiredTarget := make([]bool, len(target))
	for i := range initialOld {
		requiredOld[i] = initialOld[i] > 0
		requiredTarget[i] = target[i] > 0
	}
	state := RolloutState{
		ActiveOld: ActiveRevisionState{
			RequiredRoles:    requiredOld,
			InitialReplicas:  slicesClone(initialOld),
			SpecReplicas:     slicesClone(initialOld),
			RawReadyReplicas: slicesClone(initialOld),
			ReadyReplicas:    slicesClone(initialOld),
		},
		Target: TargetRevisionState{
			RequiredRoles:      requiredTarget,
			SpecReplicas:       make(RoleReplicaState, len(target)),
			RawReadyReplicas:   make(RoleReplicaState, len(target)),
			ReadyReplicas:      make(RoleReplicaState, len(target)),
			DesiredReplicas:    slicesClone(target),
			UnschedulableRoles: make([]bool, len(target)),
		},
		AvailabilityBaseline: slicesClone(initialOld),
		Config:               config,
	}
	steps := []UpdateStep{{Past: append(RoleReplicaState(nil), initialOld...), New: make(RoleReplicaState, len(initialOld))}}

	for range replicaSum(initialOld) + replicaSum(target) + 10 {
		next := ComputeNextStep(state)
		if next == nil {
			break
		}
		steps = append(steps, *next)
		state.ActiveOld.SpecReplicas = slicesClone(next.Past)
		state.ActiveOld.RawReadyReplicas = slicesClone(next.Past)
		state.ActiveOld.ReadyReplicas = slicesClone(next.Past)
		state.Target.SpecReplicas = slicesClone(next.New)
		state.Target.RawReadyReplicas = slicesClone(next.New)
		state.Target.ReadyReplicas = slicesClone(next.New)
	}
	return steps
}

func replicaSum(replicas RoleReplicaState) int {
	total := 0
	for _, count := range replicas {
		total += count
	}
	return total
}

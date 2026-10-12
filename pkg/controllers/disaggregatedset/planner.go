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

import "slices"

type UpdateStep struct {
	Past RoleReplicaState
	New  RoleReplicaState
	// UsesBootstrapSurge reports that New exceeds a configured surge ceiling
	// to unblock target-revision coordination.
	UsesBootstrapSurge bool
	// UsesUnavailableFallback reports that Past uses one additional unavailable
	// replica to release capacity for an unschedulable target role.
	UsesUnavailableFallback bool
	// UsesTargetRebalance trims previously issued disjoint surplus to its budget.
	UsesTargetRebalance bool
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
	// AllUnscheduled permits cancelling below DesiredReplicas. Missing evidence
	// (nil or false) protects work that may already be scheduled and loading.
	AllUnscheduled []bool
}

// RolloutState is the value-only input to revision-aware planning. Kubernetes
// objects and role names stay in the executor.
type RolloutState struct {
	ActiveOld ActiveRevisionState
	ParkedOld []ParkedRevisionState
	Target    TargetRevisionState
	// AvailabilityBaseline shares historical counts across old candidates for
	// rollout progress and removed-role availability.
	// Rebuild it from non-drained old revisions on each observation: removed
	// roles include only disjoint revisions; shared roles include all.
	AvailabilityBaseline RoleReplicaState
	Config               []RollingUpdateConfig
	// ScaleDownDuringRollout uses the latest desired surge ceiling and permits
	// bounded target-revision reductions. The default preserves #907.
	ScaleDownDuringRollout bool
}

// roleRolloutSnapshot is the per-role observed state rebuilt each reconcile.
// Draining fields describe the revision being reduced: the active old revision
// normally, or the target in targetDrainSnapshot. Replacement fields describe
// the other revisions. OldSpecReplicas always counts every old revision.
// Committed Ready excludes pending deletions; observed Ready does not.
// For removed roles, aggregate Ready excludes overlapping old revisions,
// matching the old revisions that supply their availability baseline.
type roleRolloutSnapshot struct {
	InitialOldReplicas              int                 // Durable replica baseline of the active old revision.
	AvailabilityBaselineReplicas    int                 // Historical old baseline used for rollout progress.
	UnreplacedReplicas              int                 // Old baseline not yet replaced by a complete, disjoint-role target.
	DrainingSpecReplicas            int                 // Current Spec of the revision being reduced.
	DrainingCommittedReadyReplicas  int                 // Its Ready replicas not reserved by a pending deletion.
	DrainingUsableReadyReplicas     int                 // Its committed Ready, or zero when the revision is incomplete.
	DrainingReadinessExcluded       bool                // An overlapping old revision does not supply a removed role's baseline.
	OldSpecReplicas                 int                 // Current Spec replicas summed across all old revisions.
	DisjointOldSpecReplicas         int                 // Current Spec from old revisions whose historical roles do not overlap the target.
	ParkedUsableReadyReplicas       int                 // Committed Ready from complete parked revisions.
	ObservedUsableReadyReplicas     int                 // Raw Ready replicas from complete revisions before pending deletions are reserved.
	ObservedPerRoleReadyReplicas    int                 // Raw Ready from all structurally complete revisions.
	ReplacementPerRoleReadyReplicas int                 // Committed per-role Ready from structurally complete other revisions.
	NewSpecReplicas                 int                 // Current Spec replicas of the target revision.
	NewCommittedReadyReplicas       int                 // Target Ready replicas excluding replicas committed to termination.
	ReplacementUsableReadyReplicas  int                 // Committed Ready from complete other revisions.
	NewTargetReplicas               int                 // Desired target-revision replicas after rollout.
	SurgeBaselineReplicas           int                 // Replica baseline to which MaxSurge is added.
	ScaleDownDuringRollout          bool                // Permit bounded scale corrections and one coordination bootstrap slot.
	Config                          RollingUpdateConfig // Surge and availability limits configured for this role.
}

// rolloutSnapshot is index-aligned with the role-name slice used by the
// executor. The planner deliberately works on plain values rather than
// Kubernetes objects, which keeps the decision deterministic and testable.
type rolloutSnapshot []roleRolloutSnapshot

// ComputeNextStep returns the furthest executable targets in the intersection
// of all rollout constraints. When that intersection contains no mutation, it
// may return a marked bootstrap-surge step for a blocked target role. If a
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
	past, newTargets := ordinaryReplicaTargets(state, snapshot, phaseTargets)
	next := &UpdateStep{Past: past, New: newTargets}
	if anyChange(next.Past, next.New, currentOld, currentNew) {
		return next
	}
	if rebalance := disjointSurplusStep(state, snapshot); rebalance != nil {
		return rebalance
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

// ordinaryReplicaTargets keeps old and target drains in separate decisions so
// they cannot spend the same Ready credit twice. Growth in another role may
// accompany a drain, but cannot supply any Ready credit for it.
func ordinaryReplicaTargets(state RolloutState, snapshot rolloutSnapshot, phaseTargets RoleReplicaState) (RoleReplicaState, RoleReplicaState) {
	past := furthestOldTargets(snapshot, state.ActiveOld.RequiredRoles)
	newTargets := furthestNewTargets(snapshot, phaseTargets)
	if state.ScaleDownDuringRollout && slices.Equal(past, state.ActiveOld.SpecReplicas) {
		for i, replicas := range furthestTargetScaleDownTargets(state, snapshot) {
			if replicas < state.Target.SpecReplicas[i] {
				newTargets[i] = replicas
			}
		}
	}
	return past, newTargets
}

// targetDrainSnapshot remaps only availability fields: the target is draining,
// and old revisions provide replacement capacity. Raw observations stay intact.
// This view is not used for growth limits or old-side rollout coordination.
func targetDrainSnapshot(state RolloutState, observed rolloutSnapshot) rolloutSnapshot {
	old, target := state.ActiveOld, state.Target
	oldReady := summarizeReadiness(old.RequiredRoles, old.SpecReplicas, old.RawReadyReplicas, old.ReadyReplicas)
	targetReady := summarizeReadiness(target.RequiredRoles, target.SpecReplicas, target.RawReadyReplicas, target.ReadyReplicas)
	snapshot := slices.Clone(observed)
	for i := range snapshot {
		role := &snapshot[i]
		oldCapacity := oldReady[i]
		if role.DrainingReadinessExcluded {
			oldCapacity = roleReadiness{}
		}
		role.DrainingSpecReplicas = target.SpecReplicas[i]
		role.DrainingCommittedReadyReplicas = target.ReadyReplicas[i]
		role.DrainingUsableReadyReplicas = targetReady[i].usable
		role.DrainingReadinessExcluded = false
		role.ReplacementUsableReadyReplicas = oldCapacity.usable + observed[i].ParkedUsableReadyReplicas
		role.ReplacementPerRoleReadyReplicas += oldCapacity.perRole - targetReady[i].perRole
		if target.SpecReplicas[i] == 0 {
			role.UnreplacedReplicas = 0
		}
	}
	return snapshot
}

// targetScaleDownBounds also preserves target-only fractional credit
// already needed by disjoint old roles; parked target-role Ready cannot supply it.
// Scheduled or unknown groups additionally retain the final desired count.
func targetScaleDownBounds(state RolloutState, observed rolloutSnapshot) (rolloutSnapshot, RoleReplicaState) {
	snapshot := targetDrainSnapshot(state, observed)
	minimum := minimumAvailableTargets(snapshot, state.Target.RequiredRoles)
	numerator, denominator := disjointTargetReadyFraction(state, observed)
	for i, desired := range state.Target.DesiredReplicas {
		required := int((int64(desired)*int64(numerator) + int64(denominator) - 1) / int64(denominator))
		required = min(required, state.Target.RawReadyReplicas[i])
		minimum[i] = max(minimum[i], minimumSpecToPreserveReady(state.Target.SpecReplicas[i], state.Target.ReadyReplicas[i], required))
		if i >= len(state.Target.AllUnscheduled) || !state.Target.AllUnscheduled[i] ||
			state.Target.RawReadyReplicas[i] > 0 || state.Target.ReadyReplicas[i] > 0 {
			minimum[i] = max(minimum[i], min(state.Target.SpecReplicas[i], desired))
		}
	}
	return snapshot, minimum
}

// disjointTargetReadyFraction is the replacement fraction still needed after
// pending old deletions, without requiring recovery of an already degraded floor.
func disjointTargetReadyFraction(state RolloutState, observed rolloutSnapshot) (numerator, denominator int) {
	denominator = 1
	progress, hasTarget := coordinationWindowForProgress(state.Target.DesiredReplicas, state.Target.RawReadyReplicas)
	if !allRequiredRolesPresent(state.Target.RequiredRoles, state.Target.RawReadyReplicas) {
		progress.leastAdvancedReplicas = 0
	}
	if !hasTarget {
		return
	}
	rawNumerator := min(progress.leastAdvancedReplicas, progress.leastAdvancedTarget)
	for i, role := range observed {
		baseline := state.AvailabilityBaseline[i]
		if state.Target.DesiredReplicas[i] != 0 || baseline == 0 {
			continue
		}
		credit := int(int64(baseline) * int64(rawNumerator) / int64(progress.leastAdvancedTarget))
		floor := max(0, baseline-state.Config[i].MaxUnavailable)
		available := observed[i].ObservedUsableReadyReplicas + credit
		oldReady := role.ParkedUsableReadyReplicas
		if !role.DrainingReadinessExcluded {
			oldReady += role.DrainingUsableReadyReplicas
		}
		needed := max(0, min(available, floor)-oldReady)
		if int64(needed)*int64(denominator) > int64(numerator)*int64(baseline) {
			numerator, denominator = needed, baseline
		}
	}
	return
}

// furthestTargetScaleDownTargets repairs excess over the latest surge ceiling
// when old revisions cannot safely absorb it. Once old Spec is zero, target
// Spec converges exactly. Keep the final target unless every group is known to
// be unscheduled. Required roles always retain their first bootstrap replica.
func furthestTargetScaleDownTargets(state RolloutState, snapshot rolloutSnapshot) RoleReplicaState {
	targets := slices.Clone(state.Target.SpecReplicas)
	drainSnapshot, minimum := targetScaleDownBounds(state, snapshot)
	for i, role := range snapshot {
		if role.NewSpecReplicas <= role.NewTargetReplicas {
			continue
		}
		if role.OldSpecReplicas == 0 {
			targets[i] = role.NewTargetReplicas
		} else {
			targets[i] = min(targets[i], max(0, role.NewTargetReplicas+role.Config.MaxSurge-role.OldSpecReplicas))
		}
		if state.Target.RequiredRoles[i] && role.NewSpecReplicas > 0 {
			targets[i] = max(1, targets[i])
		}
		targets[i] = max(targets[i], minimum[i])
	}
	if !availabilityPreserved(drainSnapshot, targets, state.Target.RequiredRoles) {
		return slices.Clone(state.Target.SpecReplicas)
	}
	return targets
}

// targetScaleDownPreservesAvailability ignores proposed growth: only observed
// Ready replicas can authorize a reduction, even when roles scale oppositely.
func targetScaleDownPreservesAvailability(state RolloutState, targets RoleReplicaState) bool {
	drainsOnly := slices.Clone(targets)
	snapshot, minimum := targetScaleDownBounds(state, snapshotForRolloutState(state))
	for i := range drainsOnly {
		drainsOnly[i] = min(drainsOnly[i], state.Target.SpecReplicas[i])
		if drainsOnly[i] < minimum[i] {
			return false
		}
	}
	return availabilityPreserved(snapshot, drainsOnly, state.Target.RequiredRoles)
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
		surgeCeiling := role.SurgeBaselineReplicas + role.Config.MaxSurge
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
	old, target := state.ActiveOld, state.Target
	overlapsTarget := func(requiredRoles []bool) bool {
		for i, required := range requiredRoles {
			if required && target.DesiredReplicas[i] > 0 {
				return true
			}
		}
		return false
	}
	oldOverlapsTarget := overlapsTarget(old.RequiredRoles)
	activeReady := summarizeReadiness(old.RequiredRoles, old.SpecReplicas, old.RawReadyReplicas, old.ReadyReplicas)
	targetReady := summarizeReadiness(target.RequiredRoles, target.SpecReplicas, target.RawReadyReplicas, target.ReadyReplicas)
	progress, hasTarget := coordinationWindowForProgress(target.DesiredReplicas, target.ReadyReplicas)
	if !allRequiredRolesPresent(target.RequiredRoles, target.ReadyReplicas) {
		progress.leastAdvancedReplicas = 0
	}
	snapshot := make(rolloutSnapshot, len(state.Config))
	for i := range snapshot {
		// Old readiness for removed roles comes from the same disjoint revisions as
		// its baseline. An overlapping revision retires through its shared roles.
		// Residual target-role readiness still counts: target scale-down
		// protects that capacity until its replacements are Ready.
		oldCapacity := activeReady[i]
		excludeOld := target.DesiredReplicas[i] == 0 && oldOverlapsTarget
		if excludeOld {
			oldCapacity = roleReadiness{}
		}
		surgeBaseline := max(state.ActiveOld.InitialReplicas[i], state.Target.DesiredReplicas[i])
		if state.ScaleDownDuringRollout {
			surgeBaseline = state.Target.DesiredReplicas[i]
		}
		snapshot[i] = roleRolloutSnapshot{
			InitialOldReplicas:              old.InitialReplicas[i],
			AvailabilityBaselineReplicas:    state.AvailabilityBaseline[i],
			DrainingSpecReplicas:            old.SpecReplicas[i],
			DrainingCommittedReadyReplicas:  old.ReadyReplicas[i],
			DrainingUsableReadyReplicas:     activeReady[i].usable,
			DrainingReadinessExcluded:       excludeOld,
			OldSpecReplicas:                 old.SpecReplicas[i],
			ObservedUsableReadyReplicas:     oldCapacity.rawUsable + targetReady[i].rawUsable,
			ObservedPerRoleReadyReplicas:    oldCapacity.rawPerRole + targetReady[i].rawPerRole,
			ReplacementPerRoleReadyReplicas: targetReady[i].perRole,
			NewSpecReplicas:                 target.SpecReplicas[i],
			NewCommittedReadyReplicas:       target.ReadyReplicas[i],
			ReplacementUsableReadyReplicas:  targetReady[i].usable,
			NewTargetReplicas:               target.DesiredReplicas[i],
			SurgeBaselineReplicas:           surgeBaseline,
			ScaleDownDuringRollout:          state.ScaleDownDuringRollout,
			Config:                          state.Config[i],
		}
		if !oldOverlapsTarget {
			snapshot[i].DisjointOldSpecReplicas = old.SpecReplicas[i]
		}
		// Removed-role baselines already include only disjoint old revisions.
		if hasTarget && target.DesiredReplicas[i] == 0 {
			baseline := state.AvailabilityBaseline[i]
			numerator := min(progress.leastAdvancedReplicas, progress.leastAdvancedTarget)
			credit := int(int64(baseline) * int64(numerator) / int64(progress.leastAdvancedTarget))
			snapshot[i].UnreplacedReplicas = baseline - credit
		}
	}
	for _, revision := range state.ParkedOld {
		overlaps := overlapsTarget(revision.RequiredRoles)
		ready := summarizeReadiness(revision.RequiredRoles, revision.SpecReplicas, revision.RawReadyReplicas, revision.ReadyReplicas)
		for i, capacity := range ready {
			role := &snapshot[i]
			role.OldSpecReplicas += revision.SpecReplicas[i]
			if !overlaps {
				role.DisjointOldSpecReplicas += revision.SpecReplicas[i]
			} else if target.DesiredReplicas[i] == 0 {
				continue
			}
			role.ParkedUsableReadyReplicas += capacity.usable
			role.ReplacementUsableReadyReplicas += capacity.usable
			role.ObservedUsableReadyReplicas += capacity.rawUsable
			role.ObservedPerRoleReadyReplicas += capacity.rawPerRole
			role.ReplacementPerRoleReadyReplicas += capacity.perRole
		}
	}
	return snapshot
}

type roleReadiness struct {
	usable, rawUsable   int // Serving capacity: every required role is Ready.
	perRole, rawPerRole int // Independent capacity: every required role still has Spec.
}

// summarizeReadiness keeps serving and per-role capacity separate, in both
// observed (raw) and retained (committed) form, for every kind of revision.
func summarizeReadiness(requiredRoles []bool, spec, raw, committed RoleReplicaState) []roleReadiness {
	usable := allRequiredRolesPresent(requiredRoles, committed)
	rawUsable := allRequiredRolesPresent(requiredRoles, raw)
	complete := allRequiredRolesPresent(requiredRoles, spec)
	result := make([]roleReadiness, len(spec))
	for i := range result {
		if usable {
			result[i].usable = committed[i]
		}
		if rawUsable {
			result[i].rawUsable = raw[i]
		}
		if complete {
			result[i].perRole = committed[i]
			result[i].rawPerRole = raw[i]
		}
	}
	return result
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
// Disjoint old roles need target-only replacement progress, so parked capacity
// cannot reduce the target sizes used to coordinate that progress.
func targetReplicasForActiveRevision(snapshot rolloutSnapshot) RoleReplicaState {
	targets := make(RoleReplicaState, len(snapshot))
	disjointReplacement := false
	for _, role := range snapshot {
		disjointReplacement = disjointReplacement || role.UnreplacedReplicas > 0
	}
	for i, role := range snapshot {
		parkedReady := max(0, role.ParkedUsableReadyReplicas)
		if disjointReplacement {
			parkedReady = 0
		}
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
	for i, role := range snapshot {
		current[i] = role.DrainingSpecReplicas
		initial[i] = role.InitialOldReplicas
	}
	targets := minimumAvailableTargets(snapshot, requiredRoles)
	targets = boundDrainingSnapshotTargets(snapshot, current, initial, targets)
	targets = boundOldTargetsByRevisionCompleteness(current, targets, requiredRoles)
	targets = boundDrainingSnapshotTargets(snapshot, current, initial, targets)
	// The shared floor may cover a role that this candidate never contained.
	// In that case the candidate cannot make an unrelated partial drain safe.
	if !availabilityPreserved(snapshot, targets, requiredRoles) {
		return current
	}
	return targets
}

// minimumAvailableTargets is shared by old-revision retirement and target
// scale-down. Coordination and the purpose of the reduction are independent
// of the Ready capacity that must survive it.
func minimumAvailableTargets(snapshot rolloutSnapshot, requiredRoles []bool) RoleReplicaState {
	targets := make(RoleReplicaState, len(snapshot))
	// Removing the entire revision is the furthest possible drain. If
	// that would lose usable capacity, derive the minimum surviving Spec directly
	// from the Ready capacity that this revision must continue to provide.
	if !availabilityPreserved(snapshot, targets, requiredRoles) {
		drainingUsableReadyToPreserve := make(RoleReplicaState, len(snapshot))
		drainingMustRemainUsable := false
		for i, role := range snapshot {
			if role.DrainingReadinessExcluded {
				continue
			}
			minimumUsableReady := min(role.ObservedUsableReadyReplicas, availabilityFloor(role))
			drainingUsableReadyToPreserve[i] = max(0, minimumUsableReady-role.ReplacementUsableReadyReplicas)
			drainingMustRemainUsable = drainingMustRemainUsable || drainingUsableReadyToPreserve[i] > 0
		}

		for i, role := range snapshot {
			if drainingMustRemainUsable && requiredRoles[i] && role.DrainingSpecReplicas > 0 {
				drainingUsableReadyToPreserve[i] = max(1, drainingUsableReadyToPreserve[i])
			}
			minimumUsableSpec := minimumSpecToPreserveReady(
				role.DrainingSpecReplicas, role.DrainingUsableReadyReplicas, drainingUsableReadyToPreserve[i],
			)

			// Revision-complete readiness above decides whether another revision
			// can replace this one. Independently, do not delete this role's own
			// Ready replicas just because another required role is temporarily
			// unready and made the whole revision unusable.
			minimumRoleReady := min(
				role.ObservedPerRoleReadyReplicas,
				availabilityFloor(role),
			)
			if role.DrainingReadinessExcluded {
				minimumRoleReady = 0
			}
			drainingRoleReadyToPreserve := max(0, minimumRoleReady-role.ReplacementPerRoleReadyReplicas)
			minimumRoleSpec := minimumSpecToPreserveReady(
				role.DrainingSpecReplicas, role.DrainingCommittedReadyReplicas, drainingRoleReadyToPreserve,
			)
			targets[i] = max(minimumUsableSpec, minimumRoleSpec)
		}
	}
	return targets
}

// minimumSpecToPreserveReady returns the lowest Spec that cannot remove more
// committed Ready replicas than the draining revision can afford to lose. When
// none of its readiness is needed, readiness places no lower bound on Spec.
func minimumSpecToPreserveReady(currentSpec, committedReady, readyToPreserve int) int {
	if readyToPreserve == 0 {
		return 0
	}
	return max(0, currentSpec-max(0, committedReady-readyToPreserve))
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
//	surgeCeiling     = surgeBaseline + MaxSurge
//	pendingAllowance = projected(roleReplicaCount, MaxSurge + MaxUnavailable)
//
// While same-role old Spec remains, or disjoint roles are being replaced,
// newSpec cannot exceed either surgeCeiling-oldSpec or readinessCredit+pendingAllowance.
// Readiness credit is the role's own committed Ready count for same-role
// replacement, or its share of the least-ready target fraction for disjoint
// replacement. Existing issued Spec is never reduced. Fractional coordination
// is applied separately.
func hardNewReplicaLimits(snapshot rolloutSnapshot) RoleReplicaState {
	hardLimits := make(RoleReplicaState, len(snapshot))
	releasedLimits := disjointGrowthLimits(snapshot)
	readyProgress := fractionalCoordinationWindow{}
	disjointReplacement := false
	budgetSteps := 0
	for _, role := range snapshot {
		readyProgress.includeProgress(role.NewTargetReplicas, role.NewCommittedReadyReplicas)
		budgetSteps = max(budgetSteps, role.InitialOldReplicas, role.NewTargetReplicas)
		disjointReplacement = disjointReplacement || role.UnreplacedReplicas > 0
	}
	hasTarget := readyProgress.leastAdvancedTarget > 0
	for i, role := range snapshot {
		roleReplicaCount := max(role.InitialOldReplicas, role.NewTargetReplicas)
		surgeCeiling := role.SurgeBaselineReplicas + role.Config.MaxSurge
		newSpecAllowedBySurge := surgeCeiling - role.OldSpecReplicas

		limit := newSpecAllowedBySurge
		readinessCredit := role.NewCommittedReadyReplicas
		if role.OldSpecReplicas > 0 || disjointReplacement {
			pendingAllowance := projectBudget(
				roleReplicaCount,
				role.Config.MaxSurge+role.Config.MaxUnavailable,
				budgetSteps,
			)
			if disjointReplacement && hasTarget {
				// Old removal waits for the slowest target role. Use the same
				// progress so faster readiness alone cannot open another batch.
				numerator := min(readyProgress.leastAdvancedReplicas, readyProgress.leastAdvancedTarget)
				readinessCredit = int(int64(role.NewTargetReplicas) * int64(numerator) / int64(readyProgress.leastAdvancedTarget))
			}
			pendingReadinessCeiling := readinessCredit + pendingAllowance
			limit = min(limit, pendingReadinessCeiling)
		}
		if releasedLimits != nil {
			releasedLimit := releasedLimits[i]
			if hasTarget && readyProgress.leastAdvancedReplicas > 0 {
				// The final old groups may have to retire together. Once each
				// target role has committed Ready capacity, allow one group beyond
				// their shared progress, still subject to surge and pending limits.
				releasedLimit = max(releasedLimit, readinessCredit+1)
			}
			limit = min(limit, releasedLimit)
			if hasTarget && readyProgress.leastAdvancedReplicas == 0 {
				// Start every required role before issuing a second group.
				// A later readiness dip must not retract already issued work.
				limit = min(limit, 1)
			}
		}
		hardLimits[i] = max(role.NewSpecReplicas, min(role.NewTargetReplicas, limit))
	}
	return hardLimits
}

// Disjoint growth compares observed old Spec with shared historical baselines,
// not hypothetical deletions in the next plan. Fractions do not estimate GPUs.
func disjointGrowthLimits(snapshot rolloutSnapshot) RoleReplicaState {
	progress := fractionalCoordinationWindow{}
	hasOld := false
	hasProgress := false
	for _, role := range snapshot {
		if role.NewTargetReplicas == 0 && role.UnreplacedReplicas > 0 {
			baseline := role.AvailabilityBaselineReplicas
			progress.includeProgress(baseline, max(0, baseline-role.DisjointOldSpecReplicas))
			hasOld = hasOld || role.DisjointOldSpecReplicas > 0
			hasProgress = hasProgress || baseline > role.DisjointOldSpecReplicas
		}
	}
	if !hasOld || progress.leastAdvancedTarget == 0 {
		return nil
	}
	limits := make(RoleReplicaState, len(snapshot))
	for i, role := range snapshot {
		numerator := int64(role.NewTargetReplicas) * int64(progress.leastAdvancedReplicas)
		denominator := int64(progress.leastAdvancedTarget)
		limits[i] = int((numerator+denominator-1)/denominator) + role.Config.MaxSurge
		// A final old singleton may hold the shared fraction at zero. Progress
		// in another old role is enough to start each target role once.
		if hasProgress && role.NewTargetReplicas > 0 {
			limits[i] = max(1, limits[i])
		}
	}
	return limits
}

// Reclaim incomplete target surplus beyond the old-progress budget, using the
// same availability and startup protection as ordinary target reductions.
// Only a target with no shared old roles can shrink; retain its last Ready
// group per role even if native deletion chooses Ready first.
func disjointSurplusStep(state RolloutState, snapshot rolloutSnapshot) *UpdateStep {
	limits := disjointGrowthLimits(snapshot)
	if limits == nil || allRequiredRolesPresent(state.Target.RequiredRoles, state.Target.ReadyReplicas) {
		return nil
	}
	drainSnapshot, minimum := targetScaleDownBounds(state, snapshot)
	next := slicesClone(state.Target.SpecReplicas)
	for i, role := range snapshot {
		if role.NewTargetReplicas > 0 && state.AvailabilityBaseline[i] > 0 {
			return nil
		}
		keepReady := minimumSpecToPreserveReady(role.NewSpecReplicas, role.NewCommittedReadyReplicas, min(1, role.NewCommittedReadyReplicas))
		keep := max(min(1, role.NewSpecReplicas), keepReady)
		next[i] = min(next[i], max(limits[i], keep, minimum[i]))
	}
	if !anyChange(state.ActiveOld.SpecReplicas, next, state.ActiveOld.SpecReplicas, state.Target.SpecReplicas) ||
		!availabilityPreserved(drainSnapshot, next, state.Target.RequiredRoles) {
		return nil
	}
	return &UpdateStep{Past: slicesClone(state.ActiveOld.SpecReplicas), New: next, UsesTargetRebalance: true}
}

// Recovery is shared across roles: a role whose demand did not change may
// need to release excess capacity so another role can grow.
func demandExceedsOldBaseline(snapshot rolloutSnapshot) bool {
	return slices.ContainsFunc(snapshot, func(role roleRolloutSnapshot) bool {
		return role.NewTargetReplicas > role.AvailabilityBaselineReplicas
	})
}

// snapshotWithBootstrapSurge grants enough temporary surge for the first
// replica of each missing required target role. This includes interrupted
// rollouts where parked old revisions already consume a positive MaxSurge.
// A Ready role at its ceiling can also need one slot to unblock fractional
// coordination: during opt-in scaling, or when demand exceeds old capacity.
// The slot cannot be stacked: a role already above its ceiling or still waiting
// for Ready replicas is ineligible.
// ComputeNextStep considers this only when no ordinary mutation exists.
func snapshotWithBootstrapSurge(
	snapshot rolloutSnapshot,
	phaseTargets RoleReplicaState,
) (rolloutSnapshot, bool) {
	disjointReplacement := disjointGrowthLimits(snapshot) != nil
	scaleUp := demandExceedsOldBaseline(snapshot)
	normalLimits := hardNewReplicaLimits(snapshot)
	bootstrap := append(rolloutSnapshot(nil), snapshot...)
	needed := false
	for i, role := range snapshot {
		// A renamed role has no same-role capacity to reclaim. Unrelated
		// disjoint history must not disable bootstrap for shared old roles.
		if disjointReplacement && role.OldSpecReplicas == 0 {
			continue
		}
		atSurgeCeiling := role.OldSpecReplicas+role.NewSpecReplicas == role.SurgeBaselineReplicas+role.Config.MaxSurge
		coordinationBlocked := (role.ScaleDownDuringRollout || scaleUp) && atSurgeCeiling && role.NewCommittedReadyReplicas >= role.NewSpecReplicas
		if role.Config.MaxSurge+role.Config.MaxUnavailable > 0 &&
			phaseTargets[i] > role.NewSpecReplicas && normalLimits[i] == role.NewSpecReplicas &&
			(role.NewSpecReplicas == 0 || coordinationBlocked) {
			bootstrap[i].Config.MaxSurge = max(
				role.Config.MaxSurge+1,
				role.OldSpecReplicas-role.SurgeBaselineReplicas+1,
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
// role. Existing roles use current demand; removed roles keep the historical
// capacity not yet replaced. Both old and target reductions use this floor.
func availabilityFloor(role roleRolloutSnapshot) int {
	return max(0, max(role.NewTargetReplicas, role.UnreplacedReplicas)-role.Config.MaxUnavailable)
}

// availabilityPreserved checks the worst case after the requested Spec drain:
// every removed replica may have been Ready. Complete revisions determine
// serving capacity, while per-role readiness is preserved independently so a
// transient dip in one role cannot authorize deletion of another role's Ready
// replicas. Committed Ready evaluates the post-drain state so replicas already
// pending deletion cannot be spent again. A step with no drain is always safe.
func availabilityPreserved(
	snapshot rolloutSnapshot,
	remainingTargets RoleReplicaState,
	requiredRoles []bool,
) bool {
	draining := false
	for i, role := range snapshot {
		if remainingTargets[i] < role.DrainingSpecReplicas {
			draining = true
			break
		}
	}
	if !draining {
		return true
	}

	drainingReadyAfter := make(RoleReplicaState, len(snapshot))
	if allRequiredRolesPresent(requiredRoles, remainingTargets) {
		for i, role := range snapshot {
			drain := role.DrainingSpecReplicas - remainingTargets[i]
			drainingReadyAfter[i] = max(0, role.DrainingUsableReadyReplicas-drain)
			if requiredRoles[i] && remainingTargets[i] > 0 && drainingReadyAfter[i] == 0 {
				clear(drainingReadyAfter)
				break
			}
		}
	}

	for i, role := range snapshot {
		if role.DrainingReadinessExcluded {
			continue
		}
		drain := role.DrainingSpecReplicas - remainingTargets[i]
		minimumUsableAfter := min(role.ObservedUsableReadyReplicas, availabilityFloor(role))
		// Do not reject an unrelated drain for a readiness gap that was already
		// reserved by an in-flight deletion on this unchanged role.
		if !(drain == 0 && drainingReadyAfter[i] > 0) &&
			role.ReplacementUsableReadyReplicas+drainingReadyAfter[i] < minimumUsableAfter {
			return false
		}

		drainingRoleReadyAfter := max(0, role.DrainingCommittedReadyReplicas-drain)
		minimumRoleReadyAfter := min(
			role.ObservedPerRoleReadyReplicas,
			availabilityFloor(role),
		)
		// Only draining this role can reduce its own Ready replica count.
		if drain > 0 && role.ReplacementPerRoleReadyReplicas+drainingRoleReadyAfter < minimumRoleReadyAfter {
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

// boundDrainingSnapshotTargets preserves historical rollout coordination,
// except that opt-in scaling or demand above old capacity may release existing
// physical excess. Removed roles have no new size to correct and keep their pacing.
// The correction still passes availability and whole-revision completeness.
func boundDrainingSnapshotTargets(snapshot rolloutSnapshot, current, reference, proposed RoleReplicaState) RoleReplicaState {
	scaleUp := demandExceedsOldBaseline(snapshot)
	bounded := boundDrainingRoleTargetsToWindow(current, reference, proposed)
	for i, role := range snapshot {
		if (role.ScaleDownDuringRollout || scaleUp) && role.NewTargetReplicas > 0 {
			excess := max(0, role.OldSpecReplicas+role.NewSpecReplicas-role.SurgeBaselineReplicas-role.Config.MaxSurge)
			bounded[i] = min(bounded[i], max(proposed[i], current[i]-excess))
		}
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

// ComputeAllSteps simulates a complete rollout for tests and plan-steps.
func ComputeAllSteps(initialOld, target RoleReplicaState, config []RollingUpdateConfig) []UpdateStep {
	requiredOld := make([]bool, len(initialOld))
	requiredTarget := make([]bool, len(target))
	overlapsTarget := false
	for i := range initialOld {
		requiredOld[i] = initialOld[i] > 0
		requiredTarget[i] = target[i] > 0
		overlapsTarget = overlapsTarget || requiredOld[i] && requiredTarget[i]
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
	// Match the executor's baseline contract for ordinary role removal.
	for i := range state.AvailabilityBaseline {
		if overlapsTarget && target[i] == 0 {
			state.AvailabilityBaseline[i] = 0
		}
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
		if replicaSum(next.Past) == 0 {
			clear(state.AvailabilityBaseline)
		}
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

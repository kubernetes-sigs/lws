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

package disaggregatedset

import "slices"

// Every per-role slice uses the same role index, resolved by the executor.
type RoleReplicaState = []int

type RollingUpdateConfig struct {
	MaxSurge       int
	MaxUnavailable int
}

// RawReadyReplicas includes pending deletions; ReadyReplicas excludes them.
// RequiredRoles describes the revision's layout, not its current readiness.
type RevisionState struct {
	RequiredRoles    []bool
	SpecReplicas     RoleReplicaState
	RawReadyReplicas RoleReplicaState
	ReadyReplicas    RoleReplicaState
}

type ActiveRevisionState struct {
	RevisionState
	InitialReplicas RoleReplicaState
}

type ParkedRevisionState = RevisionState

type TargetRevisionState struct {
	RevisionState
	DesiredReplicas    RoleReplicaState
	UnschedulableRoles []bool
	// Only positive evidence permits cancelling startup work below desired.
	AllUnscheduled []bool
}

// RolloutState contains observations only; the planner never modifies them.
// Only ActiveOld and Target may change in the returned plan.
type RolloutState struct {
	ActiveOld ActiveRevisionState
	ParkedOld []ParkedRevisionState
	Target    TargetRevisionState
	// Shared roles include all non-drained old history; removed roles include
	// only revisions with no target-role overlap.
	AvailabilityBaseline RoleReplicaState
	Config               []RollingUpdateConfig
	// Opt-in target reductions use the latest desired count as the surge base.
	ScaleDownDuringRollout bool
	// Opt-in growth fills the gap between desired and already-requested groups.
	ScaleUpDuringRollout bool
}

// A live required role whose budget permits zero Ready groups explicitly
// permits an outage of its revision. Other roles still keep their own Ready
// group floors. Removed roles and a peer's readiness failure do not grant it.
func (state RolloutState) allowsRevisionOutage(revision RevisionState) bool {
	for i, required := range revision.RequiredRoles {
		if required && state.Target.DesiredReplicas[i] > 0 &&
			state.Config[i].MaxUnavailable >= state.Target.DesiredReplicas[i] {
			return true
		}
	}
	return false
}

// Keep the input, not another per-role copy of it. Only readiness summaries,
// old-revision totals and replacement progress need calculating here.
type rolloutSnapshot struct {
	RolloutState
	activeReady, targetReady []roleReadiness
	old                      oldRevisionTotals
	replacementProgress      fractionalCoordinationWindow
}

type oldRevisionTotals struct {
	// All old Spec counts against surge; only disjoint Spec limits renamed growth.
	spec, disjointSpec RoleReplicaState
	parkedReady        []roleReadiness
	activeOverlaps     bool
}

type roleReadiness struct {
	usable, rawUsable   int // Serving capacity: every required role is Ready.
	perRole, rawPerRole int // Individual Ready groups, even in an incomplete revision.
}

func newRevisionState(size int) RevisionState {
	return RevisionState{
		RequiredRoles:    make([]bool, size),
		SpecReplicas:     make(RoleReplicaState, size),
		RawReadyReplicas: make(RoleReplicaState, size),
		ReadyReplicas:    make(RoleReplicaState, size),
	}
}

func newTargetRevisionState(desired RoleReplicaState) TargetRevisionState {
	target := TargetRevisionState{
		RevisionState:      newRevisionState(len(desired)),
		DesiredReplicas:    slicesClone(desired),
		UnschedulableRoles: make([]bool, len(desired)),
		AllUnscheduled:     make([]bool, len(desired)),
	}
	for i, replicas := range desired {
		target.RequiredRoles[i] = replicas > 0
	}
	return target
}

func (revision RevisionState) valid(size int) bool {
	return len(revision.RequiredRoles) == size && len(revision.SpecReplicas) == size &&
		len(revision.RawReadyReplicas) == size && len(revision.ReadyReplicas) == size
}

func validRolloutState(state RolloutState) bool {
	size := len(state.Config)
	if !state.ActiveOld.valid(size) || !state.Target.valid(size) ||
		len(state.ActiveOld.InitialReplicas) != size || len(state.AvailabilityBaseline) != size ||
		len(state.Target.DesiredReplicas) != size || len(state.Target.UnschedulableRoles) != size {
		return false
	}
	return !slices.ContainsFunc(state.ParkedOld, func(revision RevisionState) bool { return !revision.valid(size) })
}

func snapshotForRolloutState(state RolloutState) rolloutSnapshot {
	snapshot := rolloutSnapshot{
		RolloutState: state,
		activeReady:  state.ActiveOld.readiness(),
		targetReady:  state.Target.readiness(),
		old: oldRevisionTotals{
			spec:           slicesClone(state.ActiveOld.SpecReplicas),
			disjointSpec:   make(RoleReplicaState, len(state.Config)),
			parkedReady:    make([]roleReadiness, len(state.Config)),
			activeOverlaps: rolesOverlapTarget(state.ActiveOld.RequiredRoles, state.Target.DesiredReplicas),
		},
		replacementProgress: state.Target.readyProgress(state.Target.ReadyReplicas),
	}
	if !snapshot.old.activeOverlaps {
		copy(snapshot.old.disjointSpec, state.ActiveOld.SpecReplicas)
	}
	for _, revision := range state.ParkedOld {
		snapshot.includeParkedRevision(revision)
	}
	return snapshot
}

func (snapshot rolloutSnapshot) includeParkedRevision(revision RevisionState) {
	overlaps := rolesOverlapTarget(revision.RequiredRoles, snapshot.Target.DesiredReplicas)
	for i, ready := range revision.readiness() {
		snapshot.old.spec[i] += revision.SpecReplicas[i]
		if !overlaps {
			snapshot.old.disjointSpec[i] += revision.SpecReplicas[i]
		} else if snapshot.Target.DesiredReplicas[i] == 0 {
			continue
		}
		snapshot.old.parkedReady[i] = snapshot.old.parkedReady[i].plus(ready)
	}
}

func (snapshot rolloutSnapshot) excludesActive(role int) bool {
	return snapshot.old.activeOverlaps && snapshot.Target.DesiredReplicas[role] == 0
}

func (snapshot rolloutSnapshot) oldReady(role int) roleReadiness {
	ready := snapshot.old.parkedReady[role]
	if !snapshot.excludesActive(role) {
		ready = ready.plus(snapshot.activeReady[role])
	}
	return ready
}

func (snapshot rolloutSnapshot) unreplaced(role int) int {
	progress := snapshot.replacementProgress
	if snapshot.Target.DesiredReplicas[role] != 0 || progress.leastAdvancedTarget == 0 {
		return 0
	}
	baseline := snapshot.AvailabilityBaseline[role]
	return baseline - progress.readyCredit(baseline)
}

func (snapshot rolloutSnapshot) surgeCeiling(role int) int {
	baseline := snapshot.Target.DesiredReplicas[role]
	if !snapshot.ScaleDownDuringRollout {
		baseline = max(baseline, snapshot.ActiveOld.InitialReplicas[role])
	}
	return baseline + snapshot.Config[role].MaxSurge
}

func (snapshot rolloutSnapshot) availabilityFloor(role int) int {
	return max(0, max(snapshot.Target.DesiredReplicas[role], snapshot.unreplaced(role))-snapshot.Config[role].MaxUnavailable)
}

func (target TargetRevisionState) readyProgress(ready RoleReplicaState) fractionalCoordinationWindow {
	progress, _ := coordinationWindowForProgress(target.DesiredReplicas, ready)
	if !allRequiredRolesPresent(target.RequiredRoles, ready) {
		progress.leastAdvancedReplicas = 0
	}
	return progress
}

func (revision RevisionState) readiness() []roleReadiness {
	return summarizeReadiness(revision.RequiredRoles, revision.SpecReplicas, revision.RawReadyReplicas, revision.ReadyReplicas)
}

func (ready roleReadiness) plus(other roleReadiness) roleReadiness {
	return roleReadiness{
		usable: ready.usable + other.usable, rawUsable: ready.rawUsable + other.rawUsable,
		perRole: ready.perRole + other.perRole, rawPerRole: ready.rawPerRole + other.rawPerRole,
	}
}

func summarizeReadiness(required []bool, spec, raw, retained RoleReplicaState) []roleReadiness {
	serving, rawServing := allRequiredRolesPresent(required, retained), allRequiredRolesPresent(required, raw)
	result := make([]roleReadiness, len(spec))
	for i := range result {
		if serving {
			result[i].usable = retained[i]
		}
		if rawServing {
			result[i].rawUsable = raw[i]
		}
		// A permitted partial drain must not erase a surviving role's Ready
		// groups from its budget, including after a later policy change.
		result[i].perRole, result[i].rawPerRole = retained[i], raw[i]
	}
	return result
}

func allRequiredRolesPresent(required []bool, replicas RoleReplicaState) bool {
	hasRole := false
	for i, present := range required {
		if present && replicas[i] == 0 {
			return false
		}
		hasRole = hasRole || present
	}
	return hasRole
}

func rolesOverlapTarget(required []bool, desired RoleReplicaState) bool {
	for i, present := range required {
		if present && desired[i] > 0 {
			return true
		}
	}
	return false
}

func (state *RolloutState) includeHistoricalBaseline(initial, spec RoleReplicaState) {
	if replicaSum(spec) == 0 {
		return
	}
	overlaps := false
	for i, replicas := range initial {
		overlaps = overlaps || replicas > 0 && state.Target.DesiredReplicas[i] > 0
	}
	for i, replicas := range initial {
		if state.Target.DesiredReplicas[i] > 0 || !overlaps {
			state.AvailabilityBaseline[i] = max(state.AvailabilityBaseline[i], replicas)
		}
	}
}

// ComputeAllSteps simulates immediate readiness for tests and plan-steps.
func ComputeAllSteps(initial, target RoleReplicaState, config []RollingUpdateConfig) []UpdateStep {
	state := newSimulationState(initial, target, config)
	steps := []UpdateStep{{Past: slicesClone(initial), New: make(RoleReplicaState, len(initial))}}
	for range replicaSum(initial) + replicaSum(target) + 10 {
		next := ComputeNextStep(state)
		if next == nil {
			break
		}
		steps = append(steps, *next)
		state.ActiveOld.observeFullyReady(next.Past)
		state.Target.observeFullyReady(next.New)
		if replicaSum(next.Past) == 0 {
			clear(state.AvailabilityBaseline)
		}
	}
	return steps
}

func newSimulationState(initial, target RoleReplicaState, config []RollingUpdateConfig) RolloutState {
	old := newRevisionState(len(initial))
	old.observeFullyReady(initial)
	for i, replicas := range initial {
		old.RequiredRoles[i] = replicas > 0
	}
	state := RolloutState{
		ActiveOld: ActiveRevisionState{RevisionState: old, InitialReplicas: slicesClone(initial)},
		Target:    newTargetRevisionState(target), Config: config,
		AvailabilityBaseline: make(RoleReplicaState, len(initial)),
	}
	state.includeHistoricalBaseline(initial, initial)
	return state
}

func (revision *RevisionState) observeFullyReady(spec RoleReplicaState) {
	revision.SpecReplicas = slicesClone(spec)
	revision.RawReadyReplicas = slicesClone(spec)
	revision.ReadyReplicas = slicesClone(spec)
}

func replicaSum(replicas RoleReplicaState) int {
	total := 0
	for _, count := range replicas {
		total += count
	}
	return total
}

func slicesClone(values RoleReplicaState) RoleReplicaState {
	return append(RoleReplicaState(nil), values...)
}

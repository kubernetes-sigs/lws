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

// A drain budget describes only the revision being reduced and the capacity
// that must survive it. It never changes the meaning of the growth input.
type drainBudget struct {
	RevisionState
	roles       []roleDrainBudget
	allowOutage bool
}

type readyCapacity struct{ serving, individual int }

type roleDrainBudget struct {
	serving     int           // Retained serving capacity in the draining revision.
	observed    readyCapacity // Across revisions, before pending deletions.
	replacement readyCapacity // Retained capacity in the other revisions.
	floor       int
	excluded    bool // This old revision does not supply the removed-role baseline.
}

func (snapshot rolloutSnapshot) oldDrain() drainBudget    { return snapshot.drainBudget(false) }
func (snapshot rolloutSnapshot) targetDrain() drainBudget { return snapshot.drainBudget(true) }

func (snapshot rolloutSnapshot) drainBudget(target bool) drainBudget {
	revision, ready := snapshot.ActiveOld.RevisionState, snapshot.activeReady
	if target {
		revision, ready = snapshot.Target.RevisionState, snapshot.targetReady
	}
	budget := drainBudget{
		RevisionState: revision, roles: make([]roleDrainBudget, len(snapshot.Config)),
		allowOutage: !target && snapshot.allowsRevisionOutage(revision),
	}
	for i := range budget.roles {
		old := snapshot.oldReady(i)
		observed := old.plus(snapshot.targetReady[i])
		replacement := snapshot.old.parkedReady[i].plus(snapshot.targetReady[i])
		floor := snapshot.availabilityFloor(i)
		if target {
			replacement = old
			if revision.SpecReplicas[i] == 0 {
				floor = max(0, snapshot.Target.DesiredReplicas[i]-snapshot.Config[i].MaxUnavailable)
			}
		}
		budget.roles[i] = roleDrainBudget{
			serving: ready[i].usable, floor: floor, excluded: !target && snapshot.excludesActive(i),
			observed:    readyCapacity{observed.rawUsable, observed.rawPerRole},
			replacement: readyCapacity{replacement.usable, replacement.perRole},
		}
	}
	return budget
}

// Preserve each role's own readiness and, unless its policy permits a revision
// outage, serving capacity. An unhealthy peer is not permission to delete
// another role's healthy Pods.
func (budget drainBudget) minimumTargets() RoleReplicaState {
	targets := make(RoleReplicaState, len(budget.roles))
	if budget.preserves(targets) {
		return targets
	}
	serving := make(RoleReplicaState, len(targets))
	for i, role := range budget.roles {
		if !role.excluded && !budget.allowOutage {
			serving[i] = max(0, min(role.observed.serving, role.floor)-role.replacement.serving)
		}
	}
	mustServe := replicaSum(serving) > 0
	for i, role := range budget.roles {
		if mustServe && budget.RequiredRoles[i] && budget.SpecReplicas[i] > 0 {
			serving[i] = max(1, serving[i])
		}
		individual := max(0, min(role.observed.individual, role.floor)-role.replacement.individual)
		if role.excluded {
			individual = 0
		}
		targets[i] = max(
			minimumSpecToPreserveReady(budget.SpecReplicas[i], role.serving, serving[i]),
			minimumSpecToPreserveReady(budget.SpecReplicas[i], budget.ReadyReplicas[i], individual),
		)
	}
	return targets
}

// Assume every requested deletion could remove a Ready group. Readiness that
// is already reserved for deletion cannot authorize another deletion.
func (budget drainBudget) preserves(remaining RoleReplicaState) bool {
	if !hasReduction(budget.SpecReplicas, remaining) {
		return true
	}
	serving := budget.servingAfter(remaining)
	for i, role := range budget.roles {
		if role.excluded {
			continue
		}
		removed := budget.SpecReplicas[i] - remaining[i]
		// An unchanged serving role may already have a pending readiness loss.
		if !budget.allowOutage && !(removed == 0 && serving[i] > 0) && role.replacement.serving+serving[i] < min(role.observed.serving, role.floor) {
			return false
		}
		individual := max(0, budget.ReadyReplicas[i]-removed)
		if removed > 0 && role.replacement.individual+individual < min(role.observed.individual, role.floor) {
			return false
		}
	}
	return true
}

func (budget drainBudget) servingAfter(remaining RoleReplicaState) RoleReplicaState {
	ready := make(RoleReplicaState, len(remaining))
	if !allRequiredRolesPresent(budget.RequiredRoles, remaining) {
		return ready
	}
	for i, role := range budget.roles {
		ready[i] = max(0, role.serving-(budget.SpecReplicas[i]-remaining[i]))
		if budget.RequiredRoles[i] && remaining[i] > 0 && ready[i] == 0 {
			clear(ready)
			break
		}
	}
	return ready
}

func minimumSpecToPreserveReady(spec, ready, required int) int {
	if required == 0 {
		return 0
	}
	return max(0, spec-max(0, ready-required))
}

func furthestOldTargets(snapshot rolloutSnapshot) RoleReplicaState {
	old := snapshot.ActiveOld
	budget := snapshot.oldDrain()
	targets := budget.minimumTargets()
	targets = boundDrainingSnapshotTargets(snapshot, targets)
	if !snapshot.allowsRevisionOutage(old.RevisionState) {
		targets = boundOldTargetsByRevisionCompleteness(old.SpecReplicas, targets, old.RequiredRoles)
	}
	targets = boundDrainingSnapshotTargets(snapshot, targets)
	if !budget.preserves(targets) {
		return slicesClone(old.SpecReplicas)
	}
	return targets
}

// Surviving required roles retire together. An already absent role cannot be
// restored and does not prevent the rest of a broken revision from draining.
func boundOldTargetsByRevisionCompleteness(current, proposed RoleReplicaState, required []bool) RoleReplicaState {
	for i, present := range required {
		if present && current[i] > 0 && proposed[i] > 0 {
			bounded := slicesClone(proposed)
			for j, present := range required {
				if present && current[j] > 0 {
					bounded[j] = max(1, bounded[j])
				}
			}
			return bounded
		}
	}
	return proposed
}

// New-side drains must also preserve replacement credit already used to
// retire renamed old roles, and startup work that is scheduled or unknown.
func targetScaleDownBounds(snapshot rolloutSnapshot) (drainBudget, RoleReplicaState) {
	budget := snapshot.targetDrain()
	minimum := budget.minimumTargets()
	numerator, denominator := disjointTargetReadyFraction(snapshot)
	target := snapshot.Target
	for i, desired := range target.DesiredReplicas {
		required := int((int64(desired)*int64(numerator) + int64(denominator) - 1) / int64(denominator))
		required = min(required, target.RawReadyReplicas[i])
		minimum[i] = max(minimum[i], minimumSpecToPreserveReady(target.SpecReplicas[i], target.ReadyReplicas[i], required))
		if i >= len(target.AllUnscheduled) || !target.AllUnscheduled[i] || target.RawReadyReplicas[i] > 0 || target.ReadyReplicas[i] > 0 {
			minimum[i] = max(minimum[i], min(target.SpecReplicas[i], desired))
		}
	}
	return budget, minimum
}

// Calculate the target fraction still needed after pending old deletions,
// without demanding recovery of an already degraded availability floor.
func disjointTargetReadyFraction(snapshot rolloutSnapshot) (numerator, denominator int) {
	denominator = 1
	progress := snapshot.Target.readyProgress(snapshot.Target.RawReadyReplicas)
	if progress.leastAdvancedTarget == 0 {
		return
	}
	for i, baseline := range snapshot.AvailabilityBaseline {
		if snapshot.Target.DesiredReplicas[i] != 0 || baseline == 0 {
			continue
		}
		credit := progress.readyCredit(baseline)
		floor := max(0, baseline-snapshot.Config[i].MaxUnavailable)
		old := snapshot.oldReady(i)
		observed := old.rawUsable + snapshot.targetReady[i].rawUsable + credit
		needed := max(0, min(observed, floor)-old.usable)
		if int64(needed)*int64(denominator) > int64(numerator)*int64(baseline) {
			numerator, denominator = needed, baseline
		}
	}
	return
}

// Old revisions get the first chance to absorb excess. Only then may the
// target shrink towards the latest surge ceiling, subject to its drain budget.
func furthestTargetScaleDownTargets(snapshot rolloutSnapshot) RoleReplicaState {
	target := snapshot.Target
	targets := slicesClone(target.SpecReplicas)
	budget, minimum := targetScaleDownBounds(snapshot)
	for i, desired := range target.DesiredReplicas {
		if targets[i] <= desired {
			continue
		}
		targets[i] = desired
		if snapshot.old.spec[i] > 0 {
			targets[i] = min(target.SpecReplicas[i], max(0, desired+snapshot.Config[i].MaxSurge-snapshot.old.spec[i]))
		}
		if target.RequiredRoles[i] && target.SpecReplicas[i] > 0 {
			targets[i] = max(1, targets[i])
		}
		targets[i] = max(targets[i], minimum[i])
	}
	if !budget.preserves(targets) {
		return slicesClone(target.SpecReplicas)
	}
	return targets
}

func targetScaleDownPreservesAvailability(snapshot rolloutSnapshot, targets RoleReplicaState) bool {
	budget, minimum := targetScaleDownBounds(snapshot)
	drainsOnly := slicesClone(targets)
	for i := range drainsOnly {
		drainsOnly[i] = min(drainsOnly[i], snapshot.Target.SpecReplicas[i])
		if drainsOnly[i] < minimum[i] {
			return false
		}
	}
	return budget.preserves(drainsOnly)
}

func hasReduction(current, target RoleReplicaState) bool {
	for i, replicas := range current {
		if target[i] < replicas {
			return true
		}
	}
	return false
}

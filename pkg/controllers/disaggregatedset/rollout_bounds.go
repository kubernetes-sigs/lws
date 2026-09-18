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

import leaderworkersetv1 "sigs.k8s.io/lws/api/leaderworkerset/v1"

// A planner proposal passes through these execution-time checks in order:
//
//  1. Hard per-role surge and pending-readiness limits.
//  2. The fractional coordination window shared by all new roles.
//  3. A final check that the bounded step can mutate an LWS.
//
// The planner uses Spec only. These checks also use Ready, so they remain in
// the executor layer rather than planner.go.

// roleRolloutSnapshot contains the observed state used to turn a planner
// proposal into a step that is safe to execute. It is rebuilt on every
// reconcile and is not persisted by the controller.
//
// InitialOldReplicas is the old-side baseline from the initial-replicas
// annotations. Spec counts work issued to the cluster, including replicas that
// are still starting. Ready counts serving capacity and excludes replicas
// already committed to termination.
type roleRolloutSnapshot struct {
	InitialOldReplicas int
	OldSpecReplicas    int
	OldReadyReplicas   int
	NewSpecReplicas    int
	NewReadyReplicas   int
	NewTargetReplicas  int
	Config             RollingUpdateConfig
}

// rolloutSnapshot is index-aligned with the role-name slice used by the caller.
type rolloutSnapshot []roleRolloutSnapshot

// committedReadyReplicas is the availability that may safely authorize
// another scale-down. Status can temporarily report more Ready replicas than
// Spec after a previous scale-down; those excess replicas are already
// committed to termination and must not be spent a second time.
func committedReadyReplicas(lws *leaderworkersetv1.LeaderWorkerSet) int {
	if lws == nil {
		return 0
	}
	return max(0, min(int(lws.Status.ReadyReplicas), int(getLWSReplicas(lws))))
}

// boundNewReplicaTargetsByHardLimits limits a planner proposal by the absolute
// per-role surge and pending-readiness limits. These are hard limits: deadlock
// recovery may relax fractional coordination, but it must never exceed them.
// Existing Spec is never reduced, even if an externally modified LWS is
// already outside a limit.
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
// Therefore newSpec cannot exceed either:
//
//	surgeCeiling - oldSpec
//	newReady + pendingAllowance
//
// In the KEP's 8P/4D example with MaxSurge=2 and MaxUnavailable=2,
// budgetSteps is 8. The projected pending allowances are therefore 4P and 2D.
//
// Fractional coordination is intentionally not applied here. The coordinated
// drain fallback uses these hard limits when replacement capacity is required
// to retire an old revision without leaving it incomplete.
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

// fractionalCoordinationWindow describes how far new roles may progress from
// the least-advanced role. Its lower edge is:
//
//	leastAdvancedReplicas / leastAdvancedTarget
//
// Its width is largestReplicaFraction from KEP 766:
//
//	largestReplicaFraction = 1 / largestReplicaFractionDenominator
//
// newFractionalCoordinationWindow sets largestReplicaFractionDenominator to
// the smallest role.NewTargetReplicas value greater than zero.
type fractionalCoordinationWindow struct {
	leastAdvancedReplicas             int
	leastAdvancedTarget               int
	largestReplicaFractionDenominator int
}

// boundNewReplicaTargetsToCoordinationWindow keeps independently bounded role
// targets within one largestReplicaFraction of the least-advanced role. A
// current Spec is never reduced if it is already beyond the window.
func boundNewReplicaTargetsToCoordinationWindow(
	snapshot rolloutSnapshot,
	hardBoundedTargets RoleReplicaState,
) RoleReplicaState {
	window, ok := newFractionalCoordinationWindow(snapshot, hardBoundedTargets)
	if !ok {
		return hardBoundedTargets
	}

	windowBoundedTargets := make(RoleReplicaState, len(snapshot))
	for i, role := range snapshot {
		windowLimit := window.maxReplicasWithinWindow(role.NewTargetReplicas)
		windowBoundedTargets[i] = max(role.NewSpecReplicas, min(hardBoundedTargets[i], windowLimit))
	}
	return windowBoundedTargets
}

// newFractionalCoordinationWindow finds the role with the smallest proposed
// progress fraction. That role sets the lower edge of the moving window. The
// smallest role.NewTargetReplicas value greater than zero becomes
// largestReplicaFractionDenominator and defines the window width.
func newFractionalCoordinationWindow(
	snapshot rolloutSnapshot,
	hardBoundedTargets RoleReplicaState,
) (fractionalCoordinationWindow, bool) {
	window := fractionalCoordinationWindow{}
	for i, role := range snapshot {
		target := role.NewTargetReplicas
		if target <= 0 {
			continue
		}
		if window.largestReplicaFractionDenominator == 0 || target < window.largestReplicaFractionDenominator {
			window.largestReplicaFractionDenominator = target
		}
		if window.leastAdvancedTarget == 0 ||
			int64(hardBoundedTargets[i])*int64(window.leastAdvancedTarget) <
				int64(window.leastAdvancedReplicas)*int64(target) {
			window.leastAdvancedReplicas = hardBoundedTargets[i]
			window.leastAdvancedTarget = target
		}
	}
	return window, window.leastAdvancedTarget > 0
}

// maxReplicasWithinWindow returns the largest whole-replica count whose
// progress remains inside this fractional coordination window:
//
//	floor(target * (leastAdvancedReplicas/leastAdvancedTarget +
//	                1/largestReplicaFractionDenominator))
//
// For the KEP's 8P/4D example, Decode at 1/4 sets the lower edge and the
// window width is 1/4. Prefill may therefore reach at most 4/8.
func (window fractionalCoordinationWindow) maxReplicasWithinWindow(target int) int {
	if target <= 0 {
		return 0
	}

	// Split the lower-edge fraction into whole replicas and a remainder. This
	// avoids multiplying three API replica counts together, which could overflow
	// int64 at their maximum int32 values.
	scaledLowerEdge := int64(target) * int64(window.leastAdvancedReplicas)
	wholeReplicas := scaledLowerEdge / int64(window.leastAdvancedTarget)
	lowerEdgeRemainder := scaledLowerEdge % int64(window.leastAdvancedTarget)
	replicasFromRemainderAndWindow :=
		(lowerEdgeRemainder*int64(window.largestReplicaFractionDenominator) +
			int64(target)*int64(window.leastAdvancedTarget)) /
			(int64(window.leastAdvancedTarget) * int64(window.largestReplicaFractionDenominator))

	return min(target, int(wholeReplicas+replicasFromRemainderAndWindow))
}

// maxSafeDrain returns the number of old replicas that may be removed without
// crossing the KEP's hard per-role availability floor:
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

// ensureExecutableStep handles a planner proposal whose intended drain was
// removed by the Ready-based bounds and whose intended growth was removed by
// the surge limit, pending-readiness limit, or fractional coordination window.
// If one old replica can still be drained without crossing the availability
// floor, it adds that drain to open capacity for a later replacement replica.
func ensureExecutableStep(snapshot rolloutSnapshot, step *UpdateStep) {
	for i, role := range snapshot {
		requestedDrain := max(0, role.OldSpecReplicas-step.Past[i])
		if step.New[i] > role.NewSpecReplicas || min(requestedDrain, maxSafeDrain(role)) > 0 {
			return
		}
	}
	for i, role := range snapshot {
		if role.OldSpecReplicas > 0 && maxSafeDrain(role) > 0 {
			step.Past[i] = role.OldSpecReplicas - 1
			return
		}
	}
}

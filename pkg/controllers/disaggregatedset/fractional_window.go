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

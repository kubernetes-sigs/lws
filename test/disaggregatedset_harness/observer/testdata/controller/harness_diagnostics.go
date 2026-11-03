//go:build harness

/*
Copyright 2026 The Kubernetes Authors.

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

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strconv"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	disaggv1 "sigs.k8s.io/lws/api/disaggregatedset/v1"
	leaderv1 "sigs.k8s.io/lws/api/leaderworkerset/v1"
	"sigs.k8s.io/lws/pkg/computedomain"
	"sigs.k8s.io/lws/pkg/replicagroups"
	disaggutils "sigs.k8s.io/lws/pkg/utils/disaggregatedset"
)

// HarnessObserve is deliberately Reader-only. It does not instantiate a manager
// or call reconciliation: those paths can write labels, baselines and receipts.
// Bounds use the real planner helpers, but previews are hypothetical. This
// observer has neither the controller's cache position nor its pending writes.
func HarnessObserve(ctx context.Context, reader client.Reader, key client.ObjectKey) (map[string]any, error) {
	ds := &disaggv1.DisaggregatedSet{}
	if err := reader.Get(ctx, key, ds); err != nil {
		return nil, err
	}
	var list leaderv1.LeaderWorkerSetList
	if err := reader.List(ctx, &list, client.InNamespace(key.Namespace), client.MatchingLabels{disaggv1.SetNameLabelKey: key.Name}); err != nil {
		return nil, err
	}
	list.Items = slices.DeleteFunc(list.Items, func(lws leaderv1.LeaderWorkerSet) bool { return !metav1.IsControlledBy(&lws, ds) })
	desired, err := harnessDesired(ctx, reader, ds)
	if err != nil {
		return nil, err
	}
	missing := unresolvedReplicaTargetRoles(rolloutRoleNames(ds), desired)
	warnings := []string{"Independent informer observation, not atomic and not a controller decision; the controller's pending-write receipts are not visible here."}
	if len(missing) > 0 {
		warnings = append(warnings, fmt.Sprintf("No owned scaler target for %v; previews are unavailable.", missing))
	}
	revision, err := harnessTargetRevision(ds, len(list.Items) > 0)
	if err != nil {
		return nil, err
	}
	readiness := make(rolloutReadiness)
	snapshots := map[string]*replicagroups.Snapshot{}
	bySlice := map[int][]*leaderv1.LeaderWorkerSet{}
	lwsRows := []map[string]any{}
	slices.SortFunc(list.Items, func(a, b leaderv1.LeaderWorkerSet) int { return cmp.Compare(a.Name, b.Name) })
	for i := range list.Items {
		lws := &list.Items[i]
		if !metav1.IsControlledBy(lws, ds) || !lws.DeletionTimestamp.IsZero() {
			continue
		}
		s, err := replicagroups.Observe(ctx, reader, lws)
		if err != nil {
			return nil, err
		}
		if s == nil {
			warnings = append(warnings, lws.Name+": disappeared during observation")
			continue
		}
		lws = s.LWS
		if err := harnessReadiness(s, readiness); err != nil {
			return nil, fmt.Errorf("%s: %w", lws.Name, err)
		}
		snapshots[lws.Name] = s
		slice, err := strconv.Atoi(lws.Labels[disaggv1.SliceLabelKey])
		if err != nil {
			return nil, fmt.Errorf("%s: invalid slice label", lws.Name)
		}
		bySlice[slice] = append(bySlice[slice], lws)
		lwsRows = append(lwsRows, harnessLWS(s))
		if warning := harnessScaleWarning(s); warning != "" {
			warnings = append(warnings, lws.Name+": "+warning)
		}
		if lws.Annotations[subRolePlanAnnotation] != "" {
			warnings = append(warnings, lws.Name+": accepted subrole assignment is pending; planner preview is theoretical")
		}
	}
	for i := 0; i < int(ptr.Deref(ds.Spec.Slices, 1)); i++ {
		if _, exists := bySlice[i]; !exists {
			bySlice[i] = nil
		}
	}
	indices := make([]int, 0, len(bySlice))
	for index := range bySlice {
		indices = append(indices, index)
	}
	slices.Sort(indices)
	sliceRows := []map[string]any{}
	for _, index := range indices {
		var old disaggutils.RevisionRolesList
		target := disaggutils.RevisionRoles{Revision: revision, Roles: map[string]*leaderv1.LeaderWorkerSet{}}
		for _, grouped := range disaggutils.GroupByRevision(bySlice[index]) {
			if grouped.Revision == revision {
				target = grouped
			} else {
				old = append(old, grouped)
			}
		}
		inputs := buildRolloutInputs(ds, old, target, desired, readiness)
		candidates := []map[string]any{}
		var steadyBounds any
		if len(missing) == 0 {
			for _, active := range orderedRevisionCandidates(old, readiness) {
				state := rolloutStateForRevision(inputs.allRoleNames, old, active, target, inputs.targetReplicas, inputs.config, readiness)
				state.ScaleUpDuringRollout = inputs.scaleUpDuringRollout
				state.ScaleDownDuringRollout = inputs.scaleDownDuringRollout
				row := harnessCandidate(inputs.allRoleNames, state)
				row["revision"] = active.Revision
				// Like the executor, scheduler recovery is considered only after
				// ordinary/bootstrap planning. This separate preview does not
				// imply that this candidate wins over another candidate.
				if row["preview"] == nil {
					state.Target.UnschedulableRoles = harnessUnschedulable(target, inputs, snapshots)
					row["schedulerRecoveryPreview"] = harnessStep(ComputeNextStep(state))
				}
				candidates = append(candidates, row)
			}
			if len(candidates) == 0 {
				state := rolloutStateForRevision(inputs.allRoleNames, old, disaggutils.RevisionRoles{}, target, inputs.targetReplicas, inputs.config, readiness)
				state.ScaleUpDuringRollout = inputs.scaleUpDuringRollout
				state.ScaleDownDuringRollout = inputs.scaleDownDuringRollout
				steadyBounds = harnessCandidate(inputs.allRoleNames, state)["bounds"]
			}
		}
		sliceRows = append(sliceRows, map[string]any{"slice": index, "roles": inputs.allRoleNames, "desired": inputs.targetReplicas, "candidates": candidates, "steadyBounds": steadyBounds})
	}
	return map[string]any{"schemaVersion": 1, "observedAt": time.Now().UTC().Format(time.RFC3339Nano),
		"observation": "independent-informer", "name": ds.Name, "namespace": ds.Namespace,
		"targetRevision": revision, "policy": ds.Spec.ScalingPolicy, "lws": lwsRows, "slices": sliceRows, "warnings": warnings}, nil
}

// Match controller revision selection without writing its migration marker.
func harnessTargetRevision(ds *disaggv1.DisaggregatedSet, hasExistingLWS bool) (string, error) {
	switch ds.Annotations[disaggv1.RevisionHashVersionAnnotationKey] {
	case disaggv1.RevisionHashVersion:
		return disaggutils.ComputeDisaggregatedSetRevision(ds), nil
	case "":
		if hasExistingLWS && !computedomain.DisaggregatedSetUsesAutoComputeDomain(ds) {
			return disaggutils.ComputeRevisionV1(ds.Spec.Roles), nil
		}
		return disaggutils.ComputeDisaggregatedSetRevision(ds), nil
	default:
		return "", fmt.Errorf("unsupported revision hash version")
	}
}

func harnessDesired(ctx context.Context, reader client.Reader, ds *disaggv1.DisaggregatedSet) (map[string]int, error) {
	var list disaggv1.DisaggregatedSetRoleScalerList
	if err := reader.List(ctx, &list, client.InNamespace(ds.Namespace), client.MatchingLabels{disaggv1.SetNameLabelKey: ds.Name}); err != nil {
		return nil, err
	}
	scalers := map[string]*disaggv1.DisaggregatedSetRoleScaler{}
	for i := range list.Items {
		s := &list.Items[i]
		if metav1.IsControlledBy(s, ds) && s.DeletionTimestamp.IsZero() {
			scalers[childRoleKey(s.Labels[disaggv1.RoleLabelKey], s.Labels[disaggv1.SubRoleLabelKey])] = s
		}
	}
	return resolveDesiredReplicasByRole(ds, scalers), nil
}

func harnessReadiness(s *replicagroups.Snapshot, result rolloutReadiness) error {
	if s.LWS.Annotations[subRoleReplicasAnnotation] != "" {
		issued, err := subRoleCounts(s.LWS, subRoleReplicasAnnotation)
		if err != nil {
			return err
		}
		initial, err := subRoleCounts(s.LWS, disaggv1.InitialSubRoleReplicasAnnotationKey)
		if err != nil {
			return err
		}
		observeSubRoleReadiness(&subRoleState{snapshot: s, issued: issued, initial: initial}, result)
		return nil
	}
	a := s.Availability()
	result[s.LWS.Name] = replicaReadiness{raw: int(a.ReadyReplicas), committed: int(a.RetainedReadyReplicas), allUnscheduled: allGroupsUnscheduled(s.Groups, int(getLWSReplicas(s.LWS)))}
	return nil
}

func harnessLWS(s *replicagroups.Snapshot) map[string]any {
	lws, a := s.LWS, s.Availability()
	groups := []map[string]any{}
	for _, group := range s.Groups {
		pods := []map[string]any{}
		scheduled := false
		for _, pod := range group.Pods {
			ready, restarts := false, int32(0)
			for _, condition := range pod.Status.Conditions {
				ready = ready || condition.Type == corev1.PodReady && condition.Status == corev1.ConditionTrue
			}
			for _, status := range pod.Status.ContainerStatuses {
				restarts += status.RestartCount
			}
			scheduled = scheduled || pod.Spec.NodeName != ""
			pods = append(pods, map[string]any{"name": pod.Name, "uid": pod.UID, "ready": ready, "scheduled": pod.Spec.NodeName != "",
				"nodeName": pod.Spec.NodeName, "workerIndex": pod.Labels[leaderv1.WorkerIndexLabelKey], "restartCount": restarts, "phase": pod.Status.Phase,
				"terminating": !pod.DeletionTimestamp.IsZero()})
		}
		child, coherent := groupSubRole(group)
		groups = append(groups, map[string]any{"name": group.Leader.Name, "uid": group.Leader.UID, "ordinal": group.Ordinal,
			"subrole": child, "coherentSubrole": coherent, "ready": group.Ready, "terminating": group.Terminating, "scheduled": scheduled, "pods": pods})
	}
	initial, _ := disaggutils.GetInitialReplicas(lws)
	return map[string]any{"name": lws.Name, "uid": lws.UID, "revision": lws.Labels[disaggv1.RevisionLabelKey], "role": lws.Labels[disaggv1.RoleLabelKey],
		"slice": lws.Labels[disaggv1.SliceLabelKey], "identity": lws.Spec.GroupIdentity, "initial": initial, "spec": getLWSReplicas(lws),
		"rawReady": a.ReadyReplicas, "retainedReady": a.RetainedReadyReplicas,
		"scalingObserved": s.ScalingObserved(), "deletionsObserved": s.DeletionsObserved(), "groups": groups}
}

func harnessScaleWarning(s *replicagroups.Snapshot) string {
	if !s.DeletionsObserved() {
		return "native scale/deletion acknowledgement is pending"
	}
	if !s.ScalingObserved() {
		return "some requested Pods are missing; the controller may still adjust the target"
	}
	return ""
}

func harnessCandidate(names []string, state RolloutState) map[string]any {
	plan := prepareRolloutPlan(state)
	s := plan.snapshot
	released := disjointGrowthLimits(s)
	progress, _ := coordinationWindowForProgress(s.Target.DesiredReplicas, s.Target.ReadyReplicas)
	steps, disjoint := 0, false
	for i := range names {
		steps = max(steps, s.ActiveOld.InitialReplicas[i], s.Target.DesiredReplicas[i])
		disjoint = disjoint || s.unreplaced(i) > 0
	}
	oldBudget := s.oldDrain()
	oldMin := oldBudget.minimumTargets()
	_, targetMin := targetScaleDownBounds(s)
	fallback := snapshotWithUnavailableFallback(s)
	rows := []map[string]any{}
	for i, name := range names {
		pendingLimit, credit := s.pendingGrowthLimit(i, progress, steps, disjoint)
		budget := oldBudget.roles[i]
		var disjointLimit any
		if released != nil {
			disjointLimit = released[i]
		}
		rows = append(rows, map[string]any{"role": name, "maxSurge": s.Config[i].MaxSurge, "maxUnavailable": s.Config[i].MaxUnavailable,
			"availabilityFloor": s.availabilityFloor(i), "surgeCeiling": s.surgeCeiling(i), "fallbackAvailabilityFloor": fallback.availabilityFloor(i),
			"servingNoWorseningFloor": min(budget.observed.serving, budget.floor), "individualNoWorseningFloor": min(budget.observed.individual, budget.floor),
			"oldServingCheckEnabled": !oldBudget.allowOutage && !budget.excluded, "oldIndividualCheckEnabled": !budget.excluded,
			"unreplaced": s.unreplaced(i), "projectedPendingBudget": projectBudget(max(s.ActiveOld.InitialReplicas[i], s.Target.DesiredReplicas[i]), s.Config[i].MaxSurge+s.Config[i].MaxUnavailable, steps),
			"readinessCredit": credit, "pendingGrowthLimit": pendingLimit, "disjointGrowthLimit": disjointLimit, "hardGrowthLimit": plan.growthLimits[i],
			"phaseTarget": plan.phaseTargets[i], "oldMinSpec": oldMin[i], "targetMinSpec": targetMin[i], "oldTotalSpec": s.old.spec[i],
			"oldOrdinaryTarget": plan.ordinary.Past[i], "newOrdinaryTarget": plan.ordinary.New[i],
			"rawServingReady": budget.observed.serving, "rawIndividualReady": budget.observed.individual,
			"retainedServingReady":    s.oldReady(i).usable + s.targetReady[i].usable,
			"retainedIndividualReady": s.oldReady(i).perRole + s.targetReady[i].perRole})
	}
	drained := make([]int, len(names))
	for i, initial := range s.ActiveOld.InitialReplicas {
		drained[i] = max(0, initial-s.ActiveOld.SpecReplicas[i])
	}
	oldWindow, _ := coordinationWindowForProgress(s.ActiveOld.InitialReplicas, drained)
	newWindow, _ := coordinationWindowForProgress(plan.phaseTargets, s.Target.SpecReplicas)
	return map[string]any{"allowOutage": oldBudget.allowOutage, "state": state, "bounds": rows,
		"windows": map[string]any{"old": harnessWindow(oldWindow), "new": harnessWindow(newWindow), "replacement": harnessWindow(s.replacementProgress)},
		"preview": harnessStep(ComputeNextStep(state)), "previewMeaning": "Per-candidate theoretical plan; not controller selection or write authorization."}
}

func harnessWindow(window fractionalCoordinationWindow) map[string]int {
	return map[string]int{"numerator": window.leastAdvancedReplicas, "denominator": window.leastAdvancedTarget, "widthDenominator": window.largestReplicaFractionDenominator}
}

func harnessStep(step *UpdateStep) any {
	if step == nil {
		return nil
	}
	return map[string]any{"past": step.Past, "new": step.New, "kind": []string{"ordinary", "bootstrap", "unavailable-fallback", "target-rebalance"}[step.Kind]}
}

func harnessUnschedulable(target disaggutils.RevisionRoles, inputs rolloutInputs, snapshots map[string]*replicagroups.Snapshot) []bool {
	result := make([]bool, len(inputs.allRoleNames))
	for i, role := range inputs.allRoleNames {
		parent, child := roleParts(role)
		lws := target.Roles[parent]
		observed := inputs.readiness.role(target, role)
		if lws == nil || observed.spec <= observed.committed {
			continue
		}
		for _, group := range snapshots[lws.Name].Groups {
			for _, pod := range group.Pods {
				if (child == "" || pod.Labels[disaggv1.SubRoleLabelKey] == child) && podIsPersistentlyUnschedulable(pod, time.Now()) {
					result[i] = true
				}
			}
		}
	}
	return result
}

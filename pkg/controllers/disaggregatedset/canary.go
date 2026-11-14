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
	"context"
	"fmt"
	"slices"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	disaggv1 "sigs.k8s.io/lws/api/disaggregatedset/v1"
	disaggutils "sigs.k8s.io/lws/pkg/utils/disaggregatedset"
)

func canaryPercent(ds *disaggv1.DisaggregatedSet) (int, bool) {
	strategy := ds.Spec.RolloutStrategy
	if strategy == nil || strategy.CanaryPercent == nil || *strategy.CanaryPercent == 100 {
		return 100, false
	}
	return int(*strategy.CanaryPercent), true
}

// Round new groups up, but keep both revisions for a partial canary. One group
// cannot be split: keep it old and report the limitation before planning.
func canaryOldReplicas(desired, percent int) int {
	if percent == 0 || desired <= 1 {
		return desired
	}
	updated := min(desired-1, max(1, int((int64(desired)*int64(percent)+99)/100)))
	return desired - updated
}

func canarySingletonRole(names []string, desired map[string]int) string {
	for _, name := range names {
		if desired[name] == 1 {
			return name
		}
	}
	return ""
}

const canaryStableAnnotation = "disaggregatedset.x-k8s.io/canary-stable"

func isCanaryStable(revision disaggutils.RevisionRoles) bool {
	for _, lws := range revision.Roles {
		if lws.Annotations[canaryStableAnnotation] == "true" {
			return true
		}
	}
	return false
}

// Keep the recorded baseline while it survives. Without a marker, the oldest
// live revision starts the canary. Tied timestamps cannot identify that revision
// safely: block instead of letting a revision hash choose the stable workload.
func canaryStableRevision(revisions disaggutils.RevisionRolesList, names []string, readiness rolloutReadiness) disaggutils.RevisionRoles {
	candidates := revisions.SortedByNewestTimestamp()
	var oldest, marked disaggutils.RevisionRoles
	ambiguous := false
	for i := len(candidates) - 1; i >= 0; i-- {
		for _, lws := range candidates[i].Roles {
			if lws.Annotations[canaryStableAnnotation] == "true" {
				if marked.Revision != "" && marked.Revision != candidates[i].Revision {
					return disaggutils.RevisionRoles{}
				}
				marked = candidates[i]
			}
		}
		live := false
		for _, name := range names {
			if readiness.role(candidates[i], name).spec > 0 {
				live = true
			}
		}
		if !live {
			continue
		}
		if oldest.Revision == "" {
			oldest = candidates[i]
		} else if oldest.LatestCreationTime().Equal(candidates[i].LatestCreationTime()) {
			ambiguous = true
		}
	}
	if marked.Revision != "" {
		return marked
	}
	if ambiguous {
		return disaggutils.RevisionRoles{}
	}
	return oldest
}

// Persist the identity before any scaling. Mark A itself: B -> C needs no
// annotation transfer, and completing the rollout removes A and its marker.
func (executor *RollingUpdateExecutor) recordCanaryStable(ctx context.Context, ds *disaggv1.DisaggregatedSet, stable disaggutils.RevisionRoles, retain bool) error {
	manager := executor.LWSManager
	for _, lws := range stable.Roles {
		if (lws.Annotations[canaryStableAnnotation] == "true") == retain {
			continue
		}
		if !metav1.IsControlledBy(lws, ds) {
			return fmt.Errorf("stable LWS %s is not controlled by DisaggregatedSet %s", lws.Name, ds.Name)
		}
		updated := lws.DeepCopy()
		patch := client.MergeFromWithOptions(lws.DeepCopy(), client.MergeFromWithOptimisticLock{})
		if updated.Annotations == nil {
			updated.Annotations = make(map[string]string)
		}
		if retain {
			updated.Annotations[canaryStableAnnotation] = "true"
		} else {
			delete(updated.Annotations, canaryStableAnnotation)
		}
		if err := manager.mutate(ctx, updated, false, func(ctx context.Context) error { return manager.client.Patch(ctx, updated, patch) }); err != nil {
			return err
		}
		updated.DeepCopyInto(lws)
	}
	if manager.pending != nil {
		for _, lws := range stable.Roles {
			return manager.pending.observed(ctx, ds.UID, lws.Labels[disaggv1.SliceLabelKey])
		}
	}
	return nil
}

func (inputs rolloutInputs) stateForRevision(old disaggutils.RevisionRolesList, candidate, target disaggutils.RevisionRoles) RolloutState {
	state := rolloutStateForRevision(inputs.allRoleNames, old, candidate, target, inputs.targetReplicas, inputs.config, inputs.readiness)
	state.ScaleUpDuringRollout = inputs.scaleUpDuringRollout
	state.ScaleDownDuringRollout = inputs.scaleDownDuringRollout
	state.ProportionalCanary = inputs.proportionalCanary
	if inputs.proportionalCanary && candidate.Revision == inputs.stableRevision {
		for i := range state.Config {
			state.Config[i].MinOldReplicas = state.Config[i].CanaryOldReplicas
		}
	}
	if inputs.proportionalCanary {
		canaryRoleLayout(&state)
	}
	return state
}

// A role explicitly scaled to zero is no longer required for either revision
// to serve. Its historical initial count must not prevent its last group draining.
func canaryRoleLayout(state *RolloutState) {
	for i, desired := range state.Target.DesiredReplicas {
		if desired == 0 {
			state.ActiveOld.RequiredRoles[i] = false
			for j := range state.ParkedOld {
				state.ParkedOld[j].RequiredRoles[i] = false
			}
		}
	}
}

// Restore A when demand grows or the percentage falls. Older canaries drain
// before the current target. The ordinary forward planner handles promotion,
// including B -> C while A keeps its own share.
func (executor *RollingUpdateExecutor) reconcileCanaryRestoration(
	ctx context.Context, ds *disaggv1.DisaggregatedSet,
	old disaggutils.RevisionRolesList, target disaggutils.RevisionRoles,
	desired map[string]int, inputs rolloutInputs,
) (ctrl.Result, bool, error) {
	if !inputs.proportionalCanary {
		return ctrl.Result{}, false, nil
	}
	percent, _ := canaryPercent(ds)
	if name := canarySingletonRole(inputs.allRoleNames, desired); percent > 0 && name != "" {
		executor.canaryMessage = fmt.Sprintf("Keeping this slice on the stable revision because role %s has only one replica group; scaling continues there until every active role can run on both revisions", name)
	}
	stable := canaryStableRevision(append(slices.Clone(old), target), inputs.allRoleNames, inputs.readiness)
	if stable.Revision == "" {
		executor.canaryMessage = "Cannot identify the stable canary revision unambiguously; complete this rollout at canaryPercent 100 before starting another canary"
		return ctrl.Result{}, true, nil
	}
	if !compatibleCanaryRevision(stable, inputs.allRoleNames, desired) {
		executor.canaryMessage = "Canary requires a surviving stable revision with the same active roles and sub-roles; finish the layout change with canaryPercent 100 before starting a canary"
		return ctrl.Result{}, true, nil
	}
	if err := executor.recordCanaryStable(ctx, ds, stable, true); err != nil {
		return ctrl.Result{}, true, err
	}
	needed := false
	for i, name := range inputs.allRoleNames {
		quota := inputs.config[i].CanaryOldReplicas
		needed = needed || inputs.readiness.role(stable, name).spec < quota ||
			inputs.readiness.role(target, name).spec > inputs.targetReplicas[i]-quota
	}
	if !needed {
		return ctrl.Result{}, false, nil
	}
	others := slices.DeleteFunc(slices.Clone(old), func(r disaggutils.RevisionRoles) bool { return r.Revision == stable.Revision })
	candidates := append(orderedRevisionCandidates(others, inputs.readiness), target)
	var selected disaggutils.RevisionRoles
	var selectedState RolloutState
	var selectedStep *UpdateStep
	for _, candidate := range candidates {
		state := canaryRestorationState(old, target, stable, candidate, inputs)
		step := canaryRestorationStep(state)
		if step == nil {
			continue
		}
		if selectedStep == nil || step.Kind != BootstrapSurgeStep {
			selected, selectedState, selectedStep = candidate, state, step
		}
		if step.Kind != BootstrapSurgeStep {
			break
		}
	}
	if selectedStep == nil {
		// Match forward recovery: only persistent scheduler rejection may spend
		// the bounded availability fallback, after ordinary and bootstrap work.
		unschedulable, err := executor.targetUnschedulableRoles(ctx, stable, inputs.allRoleNames, inputs.readiness)
		if err != nil {
			return ctrl.Result{}, true, err
		}
		if slices.Contains(unschedulable, true) {
			for _, candidate := range candidates {
				state := canaryRestorationState(old, target, stable, candidate, inputs)
				state.Target.UnschedulableRoles = unschedulable
				if step := canaryRestorationStep(state); step != nil {
					selected, selectedState, selectedStep = candidate, state, step
					break
				}
			}
		}
	}
	if selectedStep == nil {
		// A reverse-only pass cannot reduce an oversized stable role. Let the
		// forward planner find any safe progress while its other roles load.
		return ctrl.Result{}, false, nil
	}
	return ctrl.Result{RequeueAfter: time.Second}, true, executor.applyCanaryRestoration(ctx, ds, selected, stable, inputs, selectedState, selectedStep)
}

// Reverse direction using concrete revisions: A grows, one canary drains, and
// all other canaries remain parked. Each revision contributes serving readiness
// only when all of its own required roles are present.
func canaryRestorationState(old disaggutils.RevisionRolesList, target, stable, candidate disaggutils.RevisionRoles, inputs rolloutInputs) RolloutState {
	canaries := slices.DeleteFunc(append(slices.Clone(old), target), func(r disaggutils.RevisionRoles) bool { return r.Revision == stable.Revision })
	state := rolloutStateForRevision(inputs.allRoleNames, canaries, candidate, stable, inputs.targetReplicas, inputs.config, inputs.readiness)
	state.ScaleUpDuringRollout, state.ScaleDownDuringRollout, state.ProportionalCanary = true, true, true
	canaryRoleLayout(&state)
	for i := range state.Config {
		canaryQuota := inputs.targetReplicas[i] - inputs.config[i].CanaryOldReplicas
		parked := 0
		for _, revision := range state.ParkedOld {
			parked += revision.SpecReplicas[i]
		}
		state.Config[i].CanaryOldReplicas = canaryQuota
		state.Config[i].MinOldReplicas = max(0, canaryQuota-parked)
	}
	return state
}

func canaryRestorationStep(state RolloutState) *UpdateStep {
	step := ComputeNextStep(state)
	if step == nil {
		return nil
	}
	// This pass only grows A; ordinary planning selects its reductions.
	for i := range step.New {
		step.New[i] = max(step.New[i], state.Target.SpecReplicas[i])
	}
	if !anyChange(step.Past, step.New, state.ActiveOld.SpecReplicas, state.Target.SpecReplicas) {
		return nil
	}
	return step
}

func (executor *RollingUpdateExecutor) applyCanaryRestoration(ctx context.Context, ds *disaggv1.DisaggregatedSet, draining, growing disaggutils.RevisionRoles, inputs rolloutInputs, state RolloutState, step *UpdateStep) error {
	if err := validateUpdateStep(state, step); err != nil {
		return fmt.Errorf("invalid canary restoration step: %w", err)
	}
	// Keep A's historical templates, rollout strategy and initial annotations.
	if err := executor.scaleRevisionWithStrategy(ctx, ds, growing, inputs.allRoleNames, step.New, scaleUp, false); err != nil {
		return err
	}
	if step.Kind == BootstrapSurgeStep {
		roles := bootstrapSurgeRoleNames(inputs.allRoleNames, state, step)
		executor.Record.Eventf(ds, nil, corev1.EventTypeWarning, EventReasonBootstrapSurge, "Bootstrap", "Created one bootstrap replica for roles %v while restoring the stable canary share", roles)
	}
	if err := executor.scaleRevision(ctx, ds, draining, inputs.allRoleNames, step.Past, scaleDown); err != nil {
		return err
	}
	if step.Kind == UnavailableFallbackStep {
		roles := unavailableFallbackRoleNames(inputs.allRoleNames, state, step)
		executor.Record.Eventf(ds, nil, corev1.EventTypeWarning, EventReasonAvailabilityFallback, "ReleaseCapacity", "Temporarily relaxed availability by at most one replica per role while retiring revision %s to release excess bootstrap capacity for scheduler-unschedulable stable roles %v", draining.Revision, roles)
	}
	return nil
}

func compatibleCanaryRevision(revision disaggutils.RevisionRoles, names []string, desired map[string]int) bool {
	for _, lws := range revision.Roles {
		if !lws.DeletionTimestamp.IsZero() {
			return false
		}
	}
	for _, name := range names {
		parent, child := roleParts(name)
		lws := revision.Roles[parent]
		if _, present := desired[name]; !present || lws == nil || (child != "") != (lws.Annotations[subRoleReplicasAnnotation] != "") {
			return false
		}
		if child != "" {
			counts, err := subRoleCounts(lws, subRoleReplicasAnnotation)
			if _, present := counts[child]; err != nil || !present {
				return false
			}
		}
	}
	return true
}

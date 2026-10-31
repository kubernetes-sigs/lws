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

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	disaggregatedsetv1 "sigs.k8s.io/lws/api/disaggregatedset/v1"
	leaderworkersetv1 "sigs.k8s.io/lws/api/leaderworkerset/v1"
	disaggregatedsetutils "sigs.k8s.io/lws/pkg/utils/disaggregatedset"
	podutils "sigs.k8s.io/lws/pkg/utils/pod"
)

const (
	EventReasonRollingUpdateStarted   = "RollingUpdateStarted"
	EventReasonRollingUpdateCompleted = "RollingUpdateCompleted"
	EventReasonScalingUp              = "ScalingUp"
	EventReasonScalingDown            = "ScalingDown"
	EventReasonBootstrapSurge         = "BootstrapSurge"
	EventReasonAvailabilityFallback   = "AvailabilityFallback"
	EventReasonRevisionDrainBlocked   = "RevisionDrainBlocked"
	EventReasonRollingUpdatePartition = "RollingUpdatePartitioned"
	EventReasonInitialReplicasMissing = "InitialReplicasMissing"
	EventReasonLWSDeleted             = "LWSDeleted"
	unschedulablePodGracePeriod       = time.Minute
	idleRolloutRetry                  = 10 * time.Second
)

type RollingUpdateExecutor struct {
	Record     events.EventRecorder
	LWSManager *LeaderWorkerSetManager
	// PartitionHeld records that a slice's rollout had no step to take
	// because of a role partition; the status reports the hold from it.
	PartitionHeld bool
}

type rolloutInputs struct {
	allRoleNames           []string
	targetReplicas         RoleReplicaState
	config                 []RollingUpdateConfig
	readiness              rolloutReadiness
	scaleUpDuringRollout   bool
	scaleDownDuringRollout bool
}

type scaleDirection string

const (
	scaleUp   scaleDirection = "up"
	scaleDown scaleDirection = "down"
)

// ReconcileRevisionTransition is the entry point for rolling update reconciliation.
// It uses the controller's live revision list, ensures every role exists in the target
// revision, and then continues the rollout by computing and executing its next
// scale step.
//
// desiredReplicasByRole must contain the resolved target for every role. The
// controller establishes this invariant before reconciling any slice.
//
// complete is true only after all old Specs are zero and every target-revision
// role has reached its target in both Spec and Ready replicas.
func (executor *RollingUpdateExecutor) ReconcileRevisionTransition(
	ctx context.Context,
	disaggregatedSet *disaggregatedsetv1.DisaggregatedSet,
	slice int,
	revision string,
	oldRevisions disaggregatedsetutils.RevisionRolesList,
	newRevision *disaggregatedsetutils.RevisionRoles,
	desiredReplicasByRole map[string]int,
) (ctrl.Result, bool, error) {
	roleNames := disaggregatedsetutils.GetRoleNames(disaggregatedSet)
	roleConfigs := disaggregatedsetutils.GetRoleConfigs(disaggregatedSet)

	if len(oldRevisions) == 0 {
		return ctrl.Result{RequeueAfter: time.Second}, false, nil
	}
	// Guardrail: old LWS objects should already have their initial-replicas
	// baseline. Recover it before planning if that annotation is unexpectedly
	// missing or invalid.
	if err := executor.ensureOldInitialReplicas(ctx, disaggregatedSet, oldRevisions); err != nil {
		return ctrl.Result{}, false, err
	}

	created, err := executor.ensureDesiredRevision(ctx, disaggregatedSet, slice, revision, roleNames, roleConfigs, newRevision, desiredReplicasByRole)
	if err != nil {
		return ctrl.Result{}, false, err
	}
	// Plan only after the created LWS objects have been observed again through
	// the Kubernetes client, rather than assuming their persisted state.
	if created || newRevision == nil {
		return ctrl.Result{RequeueAfter: time.Second}, false, nil
	}
	// The slice was used above to discover the relevant LWS objects. Continuing
	// the rollout updates those objects by their actual names, so the executor
	// does not need the slice index below.
	return executor.reconcileExistingRollout(ctx, disaggregatedSet, oldRevisions, *newRevision, desiredReplicasByRole)
}

// ensureDesiredRevision makes the target revision structurally complete before
// planning. Each missing role gets an LWS at 0 replicas so the planner controls
// its growth. The desired replica count is stored in initial-replicas so it
// remains the role's intended baseline if a later revision interrupts it.
// The returned boolean reports whether any LWS was created.
func (executor *RollingUpdateExecutor) ensureDesiredRevision(
	ctx context.Context,
	disaggregatedSet *disaggregatedsetv1.DisaggregatedSet,
	slice int,
	revision string,
	roleNames []string,
	roleConfigs map[string]*disaggregatedsetv1.DisaggregatedRoleSpec,
	newRevision *disaggregatedsetutils.RevisionRoles,
	desiredReplicasByRole map[string]int,
) (bool, error) {
	log := logf.FromContext(ctx)
	if newRevision == nil {
		log.Info("Initiating new rolling update", "revision", revision)
		executor.Record.Eventf(disaggregatedSet, nil, corev1.EventTypeNormal, EventReasonRollingUpdateStarted,
			"Update", "Started rolling update to revision %s", revision)
	}

	created := false
	for _, roleName := range roleNames {
		if newRevision != nil && newRevision.Roles[roleName] != nil {
			continue
		}
		initialReplicas := desiredReplicasByRole[roleName]
		if err := executor.LWSManager.Create(ctx, disaggregatedSet, roleConfigs[roleName], slice, revision, initialReplicas, desiredSubRoles(roleConfigs[roleName], desiredReplicasByRole)); err != nil {
			return false, err
		}
		created = true
	}

	return created, nil
}

// reconcileExistingRollout executes one step of an in-progress rolling update:
//  1. Refresh the current revision's initial replica values.
//  2. Build a snapshot of issued and Ready replicas for every role.
//  3. Ask the planner about old revisions in preference order.
//  4. Apply the first executable plan without changing its meaning.
//
// Informer events trigger prompt progress; idle retries only check elapsed grace
// periods or recover a missed wakeup. Every retry rechecks observations and budgets;
// elapsed retry time never authorizes a change. The rollout is complete only
// after old Specs reach zero and the target revision is Ready.
// The returned complete flag reports that terminal state to the caller.
func (executor *RollingUpdateExecutor) reconcileExistingRollout(
	ctx context.Context,
	disaggregatedSet *disaggregatedsetv1.DisaggregatedSet,
	oldRevisions disaggregatedsetutils.RevisionRolesList,
	targetRevision disaggregatedsetutils.RevisionRoles,
	desiredReplicasByRole map[string]int,
) (result ctrl.Result, complete bool, err error) {
	defer func() {
		if errors.Is(err, errReplicaGroupsPending) {
			result, complete, err = ctrl.Result{RequeueAfter: time.Second}, false, nil
		}
	}()
	log := logf.FromContext(ctx)
	inputs, err := executor.prepareRolloutInputs(ctx, disaggregatedSet, oldRevisions, targetRevision, desiredReplicasByRole)
	if err != nil {
		return ctrl.Result{}, false, err
	}

	specComplete, targetReady := rolloutCompletionStatus(oldRevisions, targetRevision, inputs.allRoleNames, inputs.targetReplicas, inputs.readiness, inputs.scaleDownDuringRollout)
	if specComplete {
		if !targetReady {
			log.V(1).Info("Waiting for target revision to become ready")
			return ctrl.Result{RequeueAfter: idleRolloutRetry}, false, nil
		}
		log.Info("Rolling update complete")
		executor.Record.Eventf(disaggregatedSet, nil, corev1.EventTypeNormal, EventReasonRollingUpdateCompleted, "Update", "Completed rolling update to revision %s", targetRevision.Revision)
		return ctrl.Result{}, true, nil
	}
	candidates := orderedRevisionCandidates(oldRevisions, inputs.readiness)
	if len(candidates) == 0 {
		if inputs.scaleDownDuringRollout {
			// Drained objects may still exist. The target must converge exactly,
			// using the same retained-readiness bounds as any other reduction.
			state := rolloutStateForRevision(inputs.allRoleNames, oldRevisions, disaggregatedsetutils.RevisionRoles{}, targetRevision, inputs.targetReplicas, inputs.config, inputs.readiness)
			state.ScaleUpDuringRollout = inputs.scaleUpDuringRollout
			state.ScaleDownDuringRollout = true
			if step := ComputeNextStep(state); step != nil {
				if err := executor.applyRolloutStep(ctx, disaggregatedSet, targetRevision, inputs, disaggregatedsetutils.RevisionRoles{}, state, step); err != nil {
					return ctrl.Result{}, false, err
				}
			}
		} else if err := executor.scaleRevision(ctx, disaggregatedSet, targetRevision, inputs.allRoleNames, inputs.targetReplicas, scaleUp); err != nil {
			return ctrl.Result{}, false, err
		}
		return ctrl.Result{RequeueAfter: time.Second}, false, nil
	}

	selectedRevision, selectedState, selectedStep, err := executor.selectNextRolloutStep(ctx, candidates, oldRevisions, targetRevision, inputs)
	if err != nil {
		return ctrl.Result{}, false, err
	}
	if selectedStep == nil && partitioned(inputs.config) {
		// The status update records the hold (condition reason RolloutPartitioned
		// and one event) once every role is Ready at its partition.
		log.V(1).Info("Rolling update is held by a role partition; waiting for the partition to change", "revision", targetRevision.Revision)
		executor.PartitionHeld = true
		return ctrl.Result{RequeueAfter: idleRolloutRetry}, false, nil
	}
	if selectedStep == nil {
		reason := "no feasible replica change is currently available within the rollout constraints"
		log.Info("Rolling update is temporarily blocked; waiting for state to change", "reason", reason)
		executor.Record.Eventf(disaggregatedSet, nil, corev1.EventTypeNormal, EventReasonRevisionDrainBlocked, "Wait", "Waiting to retire an old revision: %s", reason)
		return ctrl.Result{RequeueAfter: idleRolloutRetry}, false, nil
	}

	if err := executor.applyRolloutStep(ctx, disaggregatedSet, targetRevision, inputs, selectedRevision, selectedState, selectedStep); err != nil {
		return ctrl.Result{}, false, err
	}

	// Object updates normally trigger the next reconcile immediately. Keep a
	// short fallback after writes while native controllers observe the new Spec.
	return ctrl.Result{RequeueAfter: time.Second}, false, nil
}

func (e *RollingUpdateExecutor) prepareRolloutInputs(ctx context.Context, ds *disaggregatedsetv1.DisaggregatedSet, old disaggregatedsetutils.RevisionRolesList, target disaggregatedsetutils.RevisionRoles, desired map[string]int) (rolloutInputs, error) {
	observed, err := e.LWSManager.collectRolloutObservations(ctx, ds, old, target, desired)
	if err != nil {
		return rolloutInputs{}, err
	}
	inputs := buildRolloutInputs(ds, old, target, desired, observed)
	return inputs, e.syncTargetInitialReplicas(ctx, ds, inputs.allRoleNames, target, inputs.targetReplicas)
}

func buildRolloutInputs(
	disaggregatedSet *disaggregatedsetv1.DisaggregatedSet,
	oldRevisions disaggregatedsetutils.RevisionRolesList,
	targetRevision disaggregatedsetutils.RevisionRoles,
	desiredReplicasByRole map[string]int,
	readiness rolloutReadiness,
) rolloutInputs {
	targetRoleNames := rolloutRoleNames(disaggregatedSet)
	desiredRoles, oldRoles := collectDesiredAndOldRoles(targetRoleNames, append(slices.Clone(oldRevisions), targetRevision), readiness)
	removedRoleNames := sets.List(oldRoles.Difference(desiredRoles))
	allRoleNames := append(slices.Clone(targetRoleNames), removedRoleNames...)
	return rolloutInputs{
		allRoleNames:           allRoleNames,
		targetReplicas:         rolloutTargetReplicas(disaggregatedSet, allRoleNames, desiredRoles, oldRevisions, targetRevision, desiredReplicasByRole, readiness),
		config:                 extractRollingUpdateConfig(disaggregatedSet, allRoleNames, desiredReplicasByRole),
		readiness:              readiness,
		scaleUpDuringRollout:   scaleUpDuringRolloutEnabled(disaggregatedSet),
		scaleDownDuringRollout: scaleDownDuringRolloutEnabled(disaggregatedSet),
	}
}

func scaleUpDuringRolloutEnabled(ds *disaggregatedsetv1.DisaggregatedSet) bool {
	return ds.Spec.ScalingPolicy != nil &&
		ds.Spec.ScalingPolicy.ScaleUpDuringRollout == disaggregatedsetv1.RolloutScalingImmediate
}

func scaleDownDuringRolloutEnabled(ds *disaggregatedsetv1.DisaggregatedSet) bool {
	return ds.Spec.ScalingPolicy != nil &&
		ds.Spec.ScalingPolicy.ScaleDownDuringRollout == disaggregatedsetv1.RolloutScalingImmediate
}

// selectNextRolloutStep asks the planner about old revisions in preference
// order. Each candidate is one whole old revision with at least one non-zero
// role Spec. For A -> B -> C, the candidates slice contains B and A while both
// still have replicas; a fully drained B is omitted. Ordinary progress wins
// over bootstrap surge. Scheduler-unschedulable recovery is considered only
// when neither ordinary nor bootstrap progress is available.
func (executor *RollingUpdateExecutor) selectNextRolloutStep(
	ctx context.Context,
	candidates disaggregatedsetutils.RevisionRolesList,
	oldRevisions disaggregatedsetutils.RevisionRolesList,
	targetRevision disaggregatedsetutils.RevisionRoles,
	inputs rolloutInputs,
) (disaggregatedsetutils.RevisionRoles, RolloutState, *UpdateStep, error) {
	var selectedRevision disaggregatedsetutils.RevisionRoles
	var selectedState RolloutState
	var selectedStep *UpdateStep
	candidateStates := make([]RolloutState, len(candidates))
	for i, candidate := range candidates {
		state := rolloutStateForRevision(inputs.allRoleNames, oldRevisions, candidate, targetRevision, inputs.targetReplicas, inputs.config, inputs.readiness)
		state.ScaleUpDuringRollout = inputs.scaleUpDuringRollout
		state.ScaleDownDuringRollout = inputs.scaleDownDuringRollout
		candidateStates[i] = state
		step := ComputeNextStep(state)
		if step == nil {
			continue
		}
		// Remember the first emergency step, but prefer ordinary progress from
		// any candidate before exceeding a configured surge ceiling.
		if step.Kind == BootstrapSurgeStep {
			if selectedStep == nil {
				selectedRevision, selectedState, selectedStep = candidate, state, step
			}
			continue
		}
		// Keep looking for an ordinary old drain before reducing the target.
		// A safe target reduction still beats an emergency bootstrap step.
		if targetRevisionScalesDown(state, step) {
			if selectedStep == nil || selectedStep.Kind == BootstrapSurgeStep {
				selectedRevision, selectedState, selectedStep = candidate, state, step
			}
			continue
		}
		selectedRevision, selectedState, selectedStep = candidate, state, step
		break
	}
	if selectedStep != nil {
		return selectedRevision, selectedState, selectedStep, nil
	}

	unschedulableRoles, err := executor.targetUnschedulableRoles(ctx, targetRevision, inputs.allRoleNames, inputs.readiness)
	if err != nil {
		return selectedRevision, selectedState, nil, err
	}
	// With no unschedulable role, a second planner pass would use the same state.
	if !slices.Contains(unschedulableRoles, true) {
		return selectedRevision, selectedState, nil, nil
	}
	for i, candidate := range candidates {
		state := candidateStates[i]
		state.Target.UnschedulableRoles = slices.Clone(unschedulableRoles)
		step := ComputeNextStep(state)
		if step == nil {
			continue
		}
		return candidate, state, step, nil
	}
	return selectedRevision, selectedState, nil, nil
}

func targetRevisionScalesDown(state RolloutState, step *UpdateStep) bool {
	for i, replicas := range step.New {
		if replicas < state.Target.SpecReplicas[i] {
			return true
		}
	}
	return false
}

// applyRolloutStep validates and applies one planner decision, then records any
// emergency behavior used by that decision.
func (executor *RollingUpdateExecutor) applyRolloutStep(
	ctx context.Context,
	disaggregatedSet *disaggregatedsetv1.DisaggregatedSet,
	targetRevision disaggregatedsetutils.RevisionRoles,
	inputs rolloutInputs,
	selectedRevision disaggregatedsetutils.RevisionRoles,
	selectedState RolloutState,
	selectedStep *UpdateStep,
) error {
	if err := validateUpdateStep(selectedState, selectedStep); err != nil {
		return fmt.Errorf("planner returned an invalid rollout step: %w", err)
	}
	log := logf.FromContext(ctx)
	logArgs := append([]interface{}{"revision", selectedRevision.Revision}, buildStepLogArgs(inputs.allRoleNames, selectedStep)...)
	log.Info("Next rollout step computed", logArgs...)
	if selectedStep.Kind == TargetRebalanceStep {
		return executor.scaleRevision(ctx, disaggregatedSet, targetRevision, inputs.allRoleNames, selectedStep.New, scaleDown)
	}
	// Child assignment may wait for a healthy retained prefix. New is bounded
	// against pre-drain Specs, so replacement growth can safely precede that wait.
	drain := func() error {
		return executor.scaleRevision(ctx, disaggregatedSet, selectedRevision, inputs.allRoleNames, selectedStep.Past, scaleDown)
	}
	growFirst := slices.ContainsFunc(inputs.allRoleNames, func(name string) bool { return strings.Contains(name, "/") })
	if !growFirst {
		if err := drain(); err != nil {
			return err
		}
	}
	if inputs.scaleDownDuringRollout {
		if err := executor.scaleRevision(ctx, disaggregatedSet, targetRevision, inputs.allRoleNames, selectedStep.New, scaleDown); err != nil {
			return err
		}
	}
	if err := executor.scaleRevision(ctx, disaggregatedSet, targetRevision, inputs.allRoleNames, selectedStep.New, scaleUp); err != nil {
		return err
	}
	if selectedStep.Kind == BootstrapSurgeStep {
		roles := bootstrapSurgeRoleNames(inputs.allRoleNames, selectedState, selectedStep)
		log.Info("Used bootstrap surge to unblock rolling update", "roles", roles)
		executor.Record.Eventf(disaggregatedSet, nil, corev1.EventTypeWarning, EventReasonBootstrapSurge, "Bootstrap", "Created one bootstrap replica for roles %v without a free maxSurge slot to preserve revision completeness", roles)
	}
	if growFirst {
		if err := drain(); err != nil {
			return err
		}
	}
	if selectedStep.Kind == UnavailableFallbackStep {
		roles := unavailableFallbackRoleNames(inputs.allRoleNames, selectedState, selectedStep)
		log.Info("Used availability fallback for scheduler-unschedulable target Pods", "roles", roles)
		executor.Record.Eventf(disaggregatedSet, nil, corev1.EventTypeWarning, EventReasonAvailabilityFallback, "ReleaseCapacity", "Temporarily relaxed availability by at most one replica per role while retiring revision %s to release excess bootstrap capacity for scheduler-unschedulable target roles %v", selectedRevision.Revision, roles)
	}
	return nil
}

// targetUnschedulableRoles reports target roles with a Pod that the scheduler
// has continuously marked Unschedulable for the grace period. Slow startup,
// image pulls, and Pending Pods without this scheduler condition do not qualify.
func (executor *RollingUpdateExecutor) targetUnschedulableRoles(
	ctx context.Context,
	target disaggregatedsetutils.RevisionRoles,
	roleNames []string,
	readiness rolloutReadiness,
) ([]bool, error) {
	result := make([]bool, len(roleNames))
	now := time.Now()
	for i, roleName := range roleNames {
		parent, child := roleParts(roleName)
		lws := target.Roles[parent]
		if lws == nil {
			continue
		}
		// Every issued replica is accounted for as Ready, so this role cannot
		// need scheduler-capacity recovery.
		observed := readiness.role(target, roleName)
		if observed.spec <= observed.committed {
			continue
		}
		pods, err := executor.LWSManager.listPods(ctx, lws)
		if err != nil {
			return nil, err
		}
		for j := range pods {
			if (child == "" || pods[j].Labels[disaggregatedsetv1.SubRoleLabelKey] == child) && podIsPersistentlyUnschedulable(&pods[j], now) {
				result[i] = true
				break
			}
		}
	}
	return result, nil
}

func podIsPersistentlyUnschedulable(pod *corev1.Pod, now time.Time) bool {
	if pod == nil || !pod.DeletionTimestamp.IsZero() || pod.Status.Phase != corev1.PodPending {
		return false
	}
	_, condition := podutils.GetPodCondition(&pod.Status, corev1.PodScheduled)
	return condition != nil && condition.Status == corev1.ConditionFalse &&
		condition.Reason == corev1.PodReasonUnschedulable &&
		!condition.LastTransitionTime.IsZero() &&
		!condition.LastTransitionTime.Add(unschedulablePodGracePeriod).After(now)
}

// --- Helpers ---

func collectDesiredAndOldRoles(specRoleNames []string, revisions disaggregatedsetutils.RevisionRolesList, readiness rolloutReadiness) (desiredRoles, oldRoles sets.Set[string]) {
	desiredRoles = sets.New(specRoleNames...)
	oldRoles = sets.New[string]()
	for _, revision := range revisions {
		for name, lws := range revision.Roles {
			if lws.Annotations[subRoleReplicasAnnotation] == "" {
				oldRoles.Insert(name)
				continue
			}
			for key := range readiness {
				if child, found := strings.CutPrefix(key, lws.Name+"/"); found {
					oldRoles.Insert(childRoleKey(name, child))
				}
			}
		}
	}
	return desiredRoles, oldRoles
}

// orderedRevisionCandidates returns non-empty old revisions in planner
// preference order. Fully unready revisions come first. Each group is newest
// first. A preference is not a decision: the executor continues when a
// candidate-specific plan is blocked.
func orderedRevisionCandidates(oldRevisions disaggregatedsetutils.RevisionRolesList, readiness rolloutReadiness) disaggregatedsetutils.RevisionRolesList {
	fullyUnready := make(disaggregatedsetutils.RevisionRolesList, 0, len(oldRevisions))
	others := make(disaggregatedsetutils.RevisionRolesList, 0, len(oldRevisions))
	for _, revision := range oldRevisions.SortedByNewestTimestamp() {
		replicas := 0
		for _, lws := range revision.Roles {
			replicas += int(getLWSReplicas(lws))
		}
		if replicas == 0 {
			continue
		}
		if revisionIsFullyUnready(revision, readiness) {
			fullyUnready = append(fullyUnready, revision)
		} else {
			others = append(others, revision)
		}
	}
	return append(fullyUnready, others...)
}

// revisionIsFullyUnready reports whether the revision has no Ready replicas.
// This intentionally uses observed readiness rather than committed readiness:
// replicas reserved by an in-flight drain are still Ready replicas.
func revisionIsFullyUnready(revision disaggregatedsetutils.RevisionRoles, readiness rolloutReadiness) bool {
	for _, lws := range revision.Roles {
		if readiness[lws.Name].raw > 0 {
			return false
		}
	}
	return true
}

// rolloutStateForRevision builds a value-only planner input for one candidate.
// Other old revisions are kept separate so the planner counts their Ready
// capacity only when every required role in that revision is Ready.
func rolloutStateForRevision(
	roleNames []string,
	oldRevisions disaggregatedsetutils.RevisionRolesList,
	active disaggregatedsetutils.RevisionRoles,
	target disaggregatedsetutils.RevisionRoles,
	targetReplicas RoleReplicaState,
	config []RollingUpdateConfig,
	readiness rolloutReadiness,
) RolloutState {
	initial, activeState := observeOldRevision(active, roleNames, readiness)
	state := RolloutState{
		ActiveOld:            ActiveRevisionState{RevisionState: activeState, InitialReplicas: initial},
		Target:               observeTargetRevision(target, roleNames, targetReplicas, readiness),
		AvailabilityBaseline: make(RoleReplicaState, len(roleNames)),
		Config:               slices.Clone(config),
	}
	state.includeHistoricalBaseline(initial, activeState.SpecReplicas)
	for _, revision := range oldRevisions {
		if revision.Revision == active.Revision {
			continue
		}
		parkedInitial, parked := observeOldRevision(revision, roleNames, readiness)
		state.ParkedOld = append(state.ParkedOld, parked)
		state.includeHistoricalBaseline(parkedInitial, parked.SpecReplicas)
	}
	return state
}

func observeTargetRevision(revision disaggregatedsetutils.RevisionRoles, roleNames []string, desired RoleReplicaState, readiness rolloutReadiness) TargetRevisionState {
	target := newTargetRevisionState(desired)
	for i, roleName := range roleNames {
		observed := readiness.role(revision, roleName)
		target.SpecReplicas[i] = observed.spec
		target.RawReadyReplicas[i] = observed.raw
		target.ReadyReplicas[i] = observed.committed
		target.AllUnscheduled[i] = observed.allUnscheduled
	}
	return target
}

func observeOldRevision(
	revision disaggregatedsetutils.RevisionRoles,
	roleNames []string,
	readiness rolloutReadiness,
) (RoleReplicaState, ParkedRevisionState) {
	initial := make(RoleReplicaState, len(roleNames))
	state := newRevisionState(len(roleNames))
	for i, roleName := range roleNames {
		observed := readiness.role(revision, roleName)
		state.SpecReplicas[i] = observed.spec
		initial[i] = observed.initial
		state.RequiredRoles[i] = initial[i] > 0
		state.RawReadyReplicas[i] = observed.raw
		state.ReadyReplicas[i] = observed.committed
	}
	return initial, state
}

func rolloutTargetReplicas(
	ds *disaggregatedsetv1.DisaggregatedSet,
	allRoleNames []string,
	desiredRoles sets.Set[string],
	oldRevisions disaggregatedsetutils.RevisionRolesList,
	newRevision disaggregatedsetutils.RevisionRoles,
	desiredReplicasByRole map[string]int,
	readiness rolloutReadiness,
) RoleReplicaState {
	targets := make(RoleReplicaState, len(allRoleNames))
	for i, roleName := range allRoleNames {
		if !desiredRoles.Has(roleName) {
			continue
		}
		targets[i] = desiredReplicasByRole[roleName]
		// Keep an External target from shrinking only while old capacity for
		// this role still overlaps it. Drained old objects or another role's
		// old replicas must not keep this target artificially high.
		if !scaleDownDuringRolloutEnabled(ds) && isExternal(ds, roleName) && readiness.totalReplicas(oldRevisions, roleName) > 0 {
			targets[i] = max(targets[i], readiness.role(newRevision, roleName).spec)
		}
	}
	return targets
}

func isExternal(ds *disaggregatedsetv1.DisaggregatedSet, roleName string) bool {
	for _, p := range ds.Spec.Roles {
		for _, child := range roleLeaves(&p) {
			if childRoleKey(p.Name, child.Name) == roleName {
				return child.Scaling != nil && child.Scaling.Mode == disaggregatedsetv1.RoleScalingExternal
			}
		}
	}
	return false
}

// Ignore invalid history; removed roles receive the minimum allowance below.
// An old LWS strategy may predate a user's later reduction of the budget.
func rememberedMaxUnavailable(ds *disaggregatedsetv1.DisaggregatedSet) map[string]int {
	var budgets map[string]int
	if err := json.Unmarshal([]byte(ds.Annotations[disaggregatedsetv1.MaxUnavailableAnnotationKey]), &budgets); err != nil {
		return nil
	}
	for role, budget := range budgets {
		if budget < 0 || budget > math.MaxInt32 {
			delete(budgets, role)
		}
	}
	return budgets
}

func extractRollingUpdateConfig(
	ds *disaggregatedsetv1.DisaggregatedSet,
	allRoleNames []string,
	desiredReplicasByRole map[string]int,
) []RollingUpdateConfig {
	config := make([]RollingUpdateConfig, len(allRoleNames))
	roleIndex := make(map[string]int, len(allRoleNames))
	remembered := rememberedMaxUnavailable(ds)
	for i, name := range allRoleNames {
		// Removed roles retain larger saved budgets, but can always release one
		// group. Current roles below keep their explicitly configured policy.
		config[i] = defaultRollingUpdateConfig
		config[i].MaxUnavailable = max(config[i].MaxUnavailable, remembered[name])
		roleIndex[name] = i
	}

	for _, role := range ds.Spec.Roles {
		for _, child := range roleLeaves(&role) {
			name := childRoleKey(role.Name, child.Name)
			// Live policy always wins, including resetting an explicit budget.
			rc := disaggregatedsetutils.SubRoleRolloutConfig(&role, &child)
			replicas := desiredReplicasByRole[name]
			// Use GetScaledValueFromIntOrPercent to handle both integers and percentages.
			// For maxSurge, round up (true); for maxUnavailable, round down (false).
			surge, _ := intstr.GetScaledValueFromIntOrPercent(&rc.MaxSurge, replicas, true)
			unavail, _ := intstr.GetScaledValueFromIntOrPercent(&rc.MaxUnavailable, replicas, false)
			cfg := RollingUpdateConfig{MaxSurge: surge, MaxUnavailable: unavail}
			if surge == 0 && unavail == 0 {
				// Both zero makes no progress possible. The webhook rejects it at
				// admission, but a percentage can round down to 0 after a scale-down.
				cfg = defaultRollingUpdateConfig
			}
			if rc.Partition != nil && child.Name == "" {
				// The webhook admits a partition only on a role without sub-roles.
				cfg.Partition = int(*rc.Partition)
			}
			config[roleIndex[name]] = cfg
		}
	}
	return config
}

func partitioned(config []RollingUpdateConfig) bool {
	return slices.ContainsFunc(config, func(c RollingUpdateConfig) bool { return c.Partition > 0 })
}

// defaultRollingUpdateConfig applies to a role without a rollingUpdateConfiguration.
// It matches the LeaderWorkerSet default, so a rollout never needs capacity beyond
// the role's replicas.
var defaultRollingUpdateConfig = RollingUpdateConfig{MaxSurge: 0, MaxUnavailable: 1}

func buildStepLogArgs(roleNames []string, step *UpdateStep) []interface{} {
	args := make([]interface{}, 0, len(roleNames)*4)
	for i, name := range roleNames {
		args = append(args,
			"old_"+name, step.Past[i],
			"new_"+name, step.New[i],
		)
	}
	return args
}

func rolloutCompletionStatus(
	oldRevisions disaggregatedsetutils.RevisionRolesList,
	targetRevision disaggregatedsetutils.RevisionRoles,
	roleNames []string,
	targetReplicas RoleReplicaState,
	readiness rolloutReadiness,
	requireExactSpec bool,
) (specComplete, targetReady bool) {
	targetReady = true
	physicalTargets := make(map[string]int)
	for i, roleName := range roleNames {
		if readiness.totalReplicas(oldRevisions, roleName) != 0 {
			return false, false
		}

		target := targetReplicas[i]
		observed := readiness.role(targetRevision, roleName)
		currentSpec := observed.spec
		if requireExactSpec && currentSpec != target || currentSpec < target {
			return false, false
		}
		if requireExactSpec && observed.committed < target {
			targetReady = false
		}
		parent, _ := roleParts(roleName)
		physicalTargets[parent] += target
	}
	if requireExactSpec {
		return true, targetReady
	}
	// Deferred leaves surplus for steady-state scaling. Subroles share one
	// physical survivor range; their existing assignment code redistributes it.
	for parent, target := range physicalTargets {
		if target == 0 {
			continue
		}
		lws := targetRevision.Roles[parent]
		observed := readiness[lws.Name]
		if s := observed.snapshot; s != nil && s.LWS.Spec.GroupIdentity != leaderworkersetv1.GroupIdentityHash {
			prefix, complete := subRolePrefix(s, target)
			if !complete || int(prefix.Availability().RetainedReadyReplicas) < target {
				targetReady = false
			}
		} else if !hashSurvivorsReady(observed, int(getLWSReplicas(lws)), target) {
			targetReady = false
		}
	}
	return true, targetReady
}

func hashSurvivorsReady(observed replicaReadiness, replicas, target int) bool {
	if observed.committed >= replicas {
		return true
	}
	s := observed.snapshot
	if observed.committed < target || s == nil || !s.ScalingObserved() {
		return false
	}
	activeSets := 0
	for _, rs := range s.ReplicaSets {
		if rs.Spec.Replicas == nil || *rs.Spec.Replicas > 0 {
			activeSets++
		}
	}
	if activeSets != 1 {
		// Deployment can shrink each ReplicaSet independently; Pending leaders
		// in one cannot protect Ready groups in another.
		return false
	}
	notReadyLeaders := 0
	for _, group := range s.Groups {
		leader := group.Leader
		if !group.Terminating && leader.Status.Phase != corev1.PodFailed && leader.Status.Phase != corev1.PodSucceeded &&
			!podutils.PodRunningAndReady(*leader) {
			notReadyLeaders++
		}
	}
	// ReplicaSet prefers unassigned, Pending and not-Ready leaders for deletion.
	// A Ready leader with loading workers is NOT such a victim. Reserve a Ready
	// group for each remaining deletion, using the same observed cohort.
	readyRemovals := max(0, replicas-target-notReadyLeaders)
	return observed.committed-readyRemovals >= target
}

// --- Scaling operations ---

func subRoleTargets(names []string, targets RoleReplicaState) map[string]map[string]int {
	parents := map[string]map[string]int{}
	for i, name := range names {
		parent, child := roleParts(name)
		if child != "" {
			if parents[parent] == nil {
				parents[parent] = map[string]int{}
			}
			parents[parent][child] = targets[i]
		}
	}
	return parents
}

// scaleRevision applies targets only in the requested direction. This prevents
// the target revision from shrinking and old revisions from growing.
func (executor *RollingUpdateExecutor) scaleRevision(
	ctx context.Context,
	ds *disaggregatedsetv1.DisaggregatedSet,
	revision disaggregatedsetutils.RevisionRoles,
	roleNames []string,
	targets RoleReplicaState,
	direction scaleDirection,
) error {
	eventReason := EventReasonScalingUp
	if direction == scaleDown {
		eventReason = EventReasonScalingDown
	} else if direction != scaleUp {
		return fmt.Errorf("unknown scale direction %q", direction)
	}
	action := "Scaling " + string(direction)

	children := subRoleTargets(roleNames, targets)
	roleConfigs := disaggregatedsetutils.GetRoleConfigs(ds)
	log := logf.FromContext(ctx)
	for i, name := range roleNames {
		parent, child := roleParts(name)
		lws := revision.Roles[parent]
		if lws == nil || (child != "") != (lws.Annotations[subRoleReplicasAnnotation] != "") {
			continue
		}
		var strategy *leaderworkersetv1.RolloutStrategy
		if config := roleConfigs[parent]; direction == scaleUp && config != nil {
			// Only the target grows. Refresh its live budgets so a role created
			// at zero with zero/zero budgets can resume; leave old revisions alone.
			strategy = &config.Spec.RolloutStrategy
		}
		if child != "" {
			if counts, found := children[parent]; found {
				delete(children, parent)
				current, err := subRoleCounts(lws, subRoleReplicasAnnotation)
				if err != nil {
					return err
				}
				before := maps.Clone(current)
				// Membership is enrolled by observation, not by scaling another revision.
				for child, count := range current {
					if direction == scaleUp {
						current[child] = max(counts[child], count)
					} else {
						current[child] = min(counts[child], count)
					}
				}
				if maps.Equal(before, current) {
					continue
				}
				state, err := executor.LWSManager.syncSubRoles(ctx, ds, lws, subRoleUpdate{target: current, rolloutStrategy: strategy})
				if err != nil {
					// Accepted targets can proceed independently. Missing observations,
					// label repairs and unrelated metadata writes still require a retry.
					accepted := state.issued
					if state.pending != nil {
						accepted = state.pending
					}
					if !errors.Is(err, errReplicaGroupsPending) || !state.written || !maps.Equal(accepted, current) {
						return err
					}
				}
				revision.Roles[parent] = state.lws
				log.Info(action, "lws", lws.Name, "subrole_targets", current)
				if executor.Record != nil {
					executor.Record.Eventf(ds, nil, corev1.EventTypeNormal, eventReason,
						"Update", "Accepted scaling %s for subroles of LWS %s: %v; physical scale-down waits for group assignment", direction, lws.Name, current)
				}
			}
			continue
		}
		currentSpec := int(getLWSReplicas(lws))
		desiredSpec := targets[i]
		if direction == scaleUp && currentSpec >= desiredSpec {
			continue
		}
		if direction == scaleDown && desiredSpec >= currentSpec {
			continue
		}

		log.Info(action, "lws", lws.Name, "from_spec", currentSpec, "to", desiredSpec)
		if err := executor.LWSManager.scale(ctx, ds, lws, desiredSpec, strategy); err != nil {
			return fmt.Errorf("failed to scale %s: %w", lws.Name, err)
		}
		executor.Record.Eventf(ds, nil, corev1.EventTypeNormal, eventReason,
			"Update", "%s %s LWS %s from %d to %d replicas", action, name, lws.Name, currentSpec, desiredSpec)
	}
	return nil
}

func validateUpdateStep(state RolloutState, step *UpdateStep) error {
	if err := validateStepStructure(state, step); err != nil {
		return err
	}
	plan := prepareRolloutPlan(state)
	if step.Kind == TargetRebalanceStep {
		expected := disjointSurplusStep(plan.snapshot)
		if expected == nil || !slices.Equal(step.Past, expected.Past) || !slices.Equal(step.New, expected.New) {
			return fmt.Errorf("target rebalance does not match the disjoint surplus limit")
		}
		if !targetScaleDownPreservesAvailability(plan.snapshot, step.New) {
			return fmt.Errorf("target rebalance exceeds its retained-readiness or scheduling budget")
		}
		return nil
	}
	snapshot, limits, err := plan.boundsForStep(step)
	if err != nil {
		return err
	}
	if err := plan.validateTargets(step, limits); err != nil {
		return err
	}
	if targetRevisionScalesDown(state, step) {
		if !state.ScaleDownDuringRollout || !slices.Equal(step.Past, state.ActiveOld.SpecReplicas) {
			return fmt.Errorf("target reduction requires opt-in and cannot accompany an old drain")
		}
		if !targetScaleDownPreservesAvailability(plan.snapshot, step.New) {
			return fmt.Errorf("target reduction exceeds its retained-readiness budget")
		}
	}
	if !snapshot.oldDrain().preserves(step.Past) {
		return fmt.Errorf("old targets reduce usable readiness below its safe bound")
	}
	return nil
}

func validateStepStructure(state RolloutState, step *UpdateStep) error {
	if step == nil {
		return fmt.Errorf("step is nil")
	}
	if step.Kind > TargetRebalanceStep {
		return fmt.Errorf("unknown step kind %d", step.Kind)
	}
	if len(step.Past) != len(state.Config) || len(step.New) != len(state.Config) {
		return fmt.Errorf("target lengths do not match role count")
	}
	if !state.allowsRevisionOutage(state.ActiveOld.RevisionState) {
		if bounded := boundOldTargetsByRevisionCompleteness(state.ActiveOld.SpecReplicas, step.Past, state.ActiveOld.RequiredRoles); !slices.Equal(bounded, step.Past) {
			return fmt.Errorf("old targets leave required roles incomplete")
		}
	}
	return nil
}

// Recovery must be both applicable and necessary. Ordinary bounds remain the
// reference for coordination and for deciding whether the marker is truthful.
func (plan rolloutPlan) boundsForStep(step *UpdateStep) (rolloutSnapshot, RoleReplicaState, error) {
	snapshot, limits := plan.snapshot, plan.growthLimits
	if step.Kind == OrdinaryStep {
		return snapshot, limits, nil
	}
	if plan.changes(&plan.ordinary) {
		return snapshot, nil, fmt.Errorf("recovery used while an ordinary rollout step is available")
	}
	if step.Kind == BootstrapSurgeStep {
		bootstrap, allowed := snapshotWithBootstrapSurge(snapshot, plan.phaseTargets, limits)
		if !allowed {
			return snapshot, nil, fmt.Errorf("bootstrap surge used without a blocked target role")
		}
		return bootstrap, hardNewReplicaLimits(bootstrap), nil
	}
	if plan.bootstrapStep() != nil {
		return snapshot, nil, fmt.Errorf("availability fallback used while bootstrap surge is available")
	}
	fallback := snapshotWithUnavailableFallback(snapshot)
	if !slices.Equal(step.Past, furthestOldTargets(fallback)) || !slices.Equal(step.New, snapshot.Target.SpecReplicas) {
		return snapshot, nil, fmt.Errorf("availability fallback does not match the bounded planner target")
	}
	if !drainsOverSurgeUnschedulableRole(fallback, step.Past, snapshot.Target.UnschedulableRoles) {
		return snapshot, nil, fmt.Errorf("availability fallback does not release excess surge for an unschedulable target role")
	}
	return fallback, limits, nil
}

func (plan rolloutPlan) validateTargets(step *UpdateStep, limits RoleReplicaState) error {
	snapshot, phase := plan.snapshot, plan.phaseTargets
	if bounded := boundDrainingSnapshotTargets(snapshot, step.Past); !slices.Equal(bounded, step.Past) {
		return fmt.Errorf("old targets exceed the fractional coordination window")
	}
	growth := slices.Clone(step.New)
	for i := range growth {
		growth[i] = max(growth[i], snapshot.Target.SpecReplicas[i])
	}
	if bounded := boundGrowingSnapshotTargets(snapshot, phase, growth); !slices.Equal(bounded, growth) {
		return fmt.Errorf("new targets exceed the fractional coordination window")
	}
	usedBootstrap := false
	for i, old := range snapshot.ActiveOld.SpecReplicas {
		if step.Past[i] < 0 || step.Past[i] > old {
			return fmt.Errorf("old target %d for role %d is outside [0,%d]", step.Past[i], i, old)
		}
		minimum, maximum := min(snapshot.Target.SpecReplicas[i], plan.ordinary.New[i]), min(limits[i], phase[i])
		if step.New[i] < minimum || step.New[i] > maximum {
			return fmt.Errorf("new target %d for role %d is outside [%d,%d]", step.New[i], i, minimum, maximum)
		}
		usedBootstrap = usedBootstrap || step.New[i] > min(plan.growthLimits[i], phase[i])
	}
	if (step.Kind == BootstrapSurgeStep) != usedBootstrap {
		return fmt.Errorf("bootstrap surge marker does not match the new replica targets")
	}
	if !plan.changes(step) {
		return fmt.Errorf("plan makes no API change")
	}
	return nil
}

func bootstrapSurgeRoleNames(roleNames []string, state RolloutState, step *UpdateStep) []string {
	snapshot := snapshotForRolloutState(state)
	phaseTargets := targetReplicasForActiveRevision(snapshot)
	normalLimits := hardNewReplicaLimits(snapshot)
	roles := make([]string, 0, len(roleNames))
	for i, name := range roleNames {
		if step.New[i] > min(normalLimits[i], phaseTargets[i]) {
			roles = append(roles, name)
		}
	}
	return roles
}

func unavailableFallbackRoleNames(roleNames []string, state RolloutState, step *UpdateStep) []string {
	roles := make([]string, 0, len(roleNames))
	for i, name := range roleNames {
		if state.Target.UnschedulableRoles[i] && step.Past[i] < state.ActiveOld.SpecReplicas[i] {
			roles = append(roles, name)
		}
	}
	return roles
}

// ensureOldInitialReplicas is a guardrail for an unexpected missing or invalid
// initial-replicas annotation. The controller normally writes it while the
// revision is current. Recover from the current Spec and emit a warning without
// changing an existing valid value.
func (executor *RollingUpdateExecutor) ensureOldInitialReplicas(
	ctx context.Context,
	ds *disaggregatedsetv1.DisaggregatedSet,
	oldRevisions disaggregatedsetutils.RevisionRolesList,
) error {
	for _, revision := range oldRevisions {
		for _, lws := range revision.Roles {
			// Child preparation restores its frozen map and parent sum together.
			if lws.Annotations[subRoleReplicasAnnotation] != "" || lws.Annotations[disaggregatedsetv1.InitialSubRoleReplicasAnnotationKey] != "" {
				continue
			}
			if _, ok := disaggregatedsetutils.GetInitialReplicas(lws); ok {
				continue
			}
			initial := int(getLWSReplicas(lws))
			executor.Record.Eventf(ds, nil, corev1.EventTypeWarning, EventReasonInitialReplicasMissing,
				"Recover", "LWS %s has no valid %s annotation; restoring it from current spec.replicas (%d)",
				lws.Name, disaggregatedsetv1.InitialReplicasAnnotationKey, initial)
			if err := executor.LWSManager.UpdateInitialReplicas(ctx, ds, lws, initial); err != nil {
				return fmt.Errorf("failed to backfill initial replicas on %s: %w", lws.Name, err)
			}
		}
	}
	return nil
}

// syncTargetInitialReplicas stores the resolved rollout target while a revision
// is current. This includes any clamp that defers an External scale-down until
// overlapping old capacity is gone. The value freezes when the revision becomes
// old, preserving the baseline from which it must drain.
func (executor *RollingUpdateExecutor) syncTargetInitialReplicas(
	ctx context.Context,
	ds *disaggregatedsetv1.DisaggregatedSet,
	roleNames []string,
	newRevision disaggregatedsetutils.RevisionRoles,
	targetReplicas RoleReplicaState,
) error {
	children := subRoleTargets(roleNames, targetReplicas)
	for i, roleName := range roleNames {
		parent, child := roleParts(roleName)
		lws := newRevision.Roles[parent]
		if lws == nil || (child != "") != (lws.Annotations[subRoleReplicasAnnotation] != "") {
			continue
		}
		if child != "" {
			if counts, found := children[parent]; found {
				delete(children, parent)
				if _, err := executor.LWSManager.syncSubRoles(ctx, ds, lws, subRoleUpdate{initial: counts}); err != nil {
					return err
				}
			}
			continue
		}
		initial := targetReplicas[i]
		current, ok := disaggregatedsetutils.GetInitialReplicas(lws)
		if ok && int(current) == initial {
			continue
		}
		if err := executor.LWSManager.UpdateInitialReplicas(ctx, ds, lws, initial); err != nil {
			return fmt.Errorf("failed to update initial replicas on %s: %w", lws.Name, err)
		}
	}
	return nil
}

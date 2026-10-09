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
	"errors"
	"fmt"
	"slices"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	disaggregatedsetv1 "sigs.k8s.io/lws/api/disaggregatedset/v1"
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
	EventReasonInitialReplicasMissing = "InitialReplicasMissing"
	EventReasonLWSDeleted             = "LWSDeleted"
	unschedulablePodGracePeriod       = time.Minute
)

type RollingUpdateExecutor struct {
	Record     events.EventRecorder
	LWSManager *LeaderWorkerSetManager
}

type rolloutInputs struct {
	targetRoleNames []string
	allRoleNames    []string
	targetReplicas  RoleReplicaState
	config          []RollingUpdateConfig
	readiness       rolloutReadiness
}

type scaleDirection string

const (
	scaleUp   scaleDirection = "up"
	scaleDown scaleDirection = "down"
)

// ReconcileRevisionTransition is the entry point for rolling update reconciliation.
// It fetches current cluster state, ensures every role exists in the target
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
	desiredReplicasByRole map[string]int,
) (ctrl.Result, bool, error) {
	roleNames := disaggregatedsetutils.GetRoleNames(disaggregatedSet)
	roleConfigs := disaggregatedsetutils.GetRoleConfigs(disaggregatedSet)

	oldRevisions, newRevision, err := executor.LWSManager.GetRevisionRolesList(ctx, disaggregatedSet, slice, revision)
	if err != nil {
		return ctrl.Result{}, false, err
	}
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
		if err := executor.LWSManager.Create(ctx, disaggregatedSet, roleConfigs[roleName], slice, revision, 0, initialReplicas); err != nil {
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
// Object updates and a one-second timer trigger the next step. The rollout is
// complete only after the old Specs reach zero and the target revision is Ready.
// The returned complete flag reports that terminal state to the caller.
func (executor *RollingUpdateExecutor) reconcileExistingRollout(
	ctx context.Context,
	disaggregatedSet *disaggregatedsetv1.DisaggregatedSet,
	oldRevisions disaggregatedsetutils.RevisionRolesList,
	targetRevision disaggregatedsetutils.RevisionRoles,
	desiredReplicasByRole map[string]int,
) (ctrl.Result, bool, error) {
	log := logf.FromContext(ctx)
	inputs := buildRolloutInputs(disaggregatedSet, oldRevisions, targetRevision, desiredReplicasByRole)
	if err := executor.syncTargetInitialReplicas(ctx, disaggregatedSet, inputs.allRoleNames, targetRevision, inputs.targetReplicas); err != nil {
		return ctrl.Result{}, false, err
	}
	var err error
	inputs.readiness, err = executor.LWSManager.observeRolloutReadiness(ctx, oldRevisions, targetRevision)
	if errors.Is(err, errReplicaGroupsPending) {
		return ctrl.Result{RequeueAfter: time.Second}, false, nil
	}
	if err != nil {
		return ctrl.Result{}, false, err
	}

	specComplete, targetReady := rolloutCompletionStatus(oldRevisions, targetRevision, inputs.allRoleNames, inputs.targetReplicas, inputs.readiness)
	if specComplete {
		if !targetReady {
			log.V(1).Info("Waiting for target revision to become ready")
			return ctrl.Result{RequeueAfter: time.Second}, false, nil
		}
		log.Info("Rolling update complete")
		executor.Record.Eventf(disaggregatedSet, nil, corev1.EventTypeNormal, EventReasonRollingUpdateCompleted, "Update", "Completed rolling update to revision %s", targetRevision.Revision)
		return ctrl.Result{}, true, nil
	}
	candidates := orderedRevisionCandidates(oldRevisions, inputs.readiness)
	if len(candidates) == 0 {
		if err := executor.scaleRevision(ctx, disaggregatedSet, targetRevision, inputs.targetRoleNames, inputs.targetReplicas, scaleUp); err != nil {
			return ctrl.Result{}, false, err
		}
		return ctrl.Result{RequeueAfter: time.Second}, false, nil
	}

	selectedRevision, selectedState, selectedStep, err := executor.selectNextRolloutStep(ctx, candidates, oldRevisions, targetRevision, inputs)
	if err != nil {
		return ctrl.Result{}, false, err
	}
	if selectedStep == nil {
		reason := "no feasible replica change is currently available within the rollout constraints"
		log.Info("Rolling update is temporarily blocked; waiting for state to change", "reason", reason)
		executor.Record.Eventf(disaggregatedSet, nil, corev1.EventTypeNormal, EventReasonRevisionDrainBlocked, "Wait", "Waiting to retire an old revision: %s", reason)
		return ctrl.Result{RequeueAfter: time.Second}, false, nil
	}

	if err := executor.applyRolloutStep(ctx, disaggregatedSet, targetRevision, inputs, selectedRevision, selectedState, selectedStep); err != nil {
		return ctrl.Result{}, false, err
	}

	// Object updates normally trigger the next reconcile immediately. The
	// timer also lets the planner retry when pending replicas become Ready.
	return ctrl.Result{RequeueAfter: time.Second}, false, nil
}

func buildRolloutInputs(
	disaggregatedSet *disaggregatedsetv1.DisaggregatedSet,
	oldRevisions disaggregatedsetutils.RevisionRolesList,
	targetRevision disaggregatedsetutils.RevisionRoles,
	desiredReplicasByRole map[string]int,
) rolloutInputs {
	targetRoleNames := disaggregatedsetutils.GetRoleNames(disaggregatedSet)
	desiredRoles, oldRoles := collectDesiredAndOldRoles(targetRoleNames, oldRevisions)
	removedRoleNames := sets.List(oldRoles.Difference(desiredRoles))
	allRoleNames := append(slices.Clone(targetRoleNames), removedRoleNames...)
	return rolloutInputs{
		targetRoleNames: targetRoleNames,
		allRoleNames:    allRoleNames,
		targetReplicas:  rolloutTargetReplicas(disaggregatedSet, allRoleNames, desiredRoles, oldRevisions, targetRevision, desiredReplicasByRole),
		config:          extractRollingUpdateConfig(disaggregatedSet, allRoleNames, desiredReplicasByRole),
	}
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
		candidateStates[i] = state
		step := ComputeNextStep(state)
		if step == nil {
			continue
		}
		// Remember the first emergency step, but prefer ordinary progress from
		// any candidate before exceeding a configured surge ceiling.
		if step.UsesBootstrapSurge {
			if selectedStep == nil {
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
	// Apply drains before growth so ordinary steps cannot transiently exceed
	// their surge ceilings. A marked bootstrap step is the sole exception.
	if err := executor.scaleRevision(ctx, disaggregatedSet, selectedRevision, inputs.allRoleNames, selectedStep.Past, scaleDown); err != nil {
		return err
	}
	if err := executor.scaleRevision(ctx, disaggregatedSet, targetRevision, inputs.targetRoleNames, selectedStep.New, scaleUp); err != nil {
		return err
	}
	if selectedStep.UsesBootstrapSurge {
		roles := bootstrapSurgeRoleNames(inputs.allRoleNames, selectedState, selectedStep)
		log.Info("Used bootstrap surge to unblock rolling update", "roles", roles)
		executor.Record.Eventf(disaggregatedSet, nil, corev1.EventTypeWarning, EventReasonBootstrapSurge, "Bootstrap", "Created one bootstrap replica for roles %v without a free maxSurge slot to preserve revision completeness", roles)
	}
	if selectedStep.UsesUnavailableFallback {
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
		lws := target.Roles[roleName]
		if lws == nil {
			continue
		}
		// Every issued replica is accounted for as Ready, so this role cannot
		// need scheduler-capacity recovery.
		if int(getLWSReplicas(lws)) <= readiness[lws.Name].committed {
			continue
		}
		pods, err := executor.LWSManager.listPods(ctx, lws)
		if err != nil {
			return nil, err
		}
		for j := range pods {
			if podIsPersistentlyUnschedulable(&pods[j], now) {
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

func collectDesiredAndOldRoles(specRoleNames []string, oldRevisions disaggregatedsetutils.RevisionRolesList) (desiredRoles, oldRoles sets.Set[string]) {
	desiredRoles = sets.New(specRoleNames...)
	oldRoles = sets.New[string]()
	for _, wl := range oldRevisions {
		for name := range wl.Roles {
			oldRoles.Insert(name)
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
		ActiveOld: ActiveRevisionState{
			RequiredRoles:    activeState.RequiredRoles,
			InitialReplicas:  initial,
			SpecReplicas:     activeState.SpecReplicas,
			RawReadyReplicas: activeState.RawReadyReplicas,
			ReadyReplicas:    activeState.ReadyReplicas,
		},
		Target: TargetRevisionState{
			RequiredRoles:      make([]bool, len(roleNames)),
			SpecReplicas:       make(RoleReplicaState, len(roleNames)),
			RawReadyReplicas:   make(RoleReplicaState, len(roleNames)),
			ReadyReplicas:      make(RoleReplicaState, len(roleNames)),
			DesiredReplicas:    slicesClone(targetReplicas),
			UnschedulableRoles: make([]bool, len(roleNames)),
		},
		AvailabilityBaseline: slicesClone(initial),
		Config:               slices.Clone(config),
	}
	for _, revision := range oldRevisions {
		if revision.Revision == active.Revision {
			continue
		}
		parkedInitial, parked := observeOldRevision(revision, roleNames, readiness)
		state.ParkedOld = append(state.ParkedOld, parked)
		// All candidates share the non-drained old set's baseline. A fully
		// drained revision leaves this set, so the next phase's floor may fall.
		if replicaSum(parked.SpecReplicas) == 0 {
			continue
		}
		for i := range state.AvailabilityBaseline {
			state.AvailabilityBaseline[i] = max(state.AvailabilityBaseline[i], parkedInitial[i])
		}
	}
	for i, roleName := range roleNames {
		state.Target.RequiredRoles[i] = targetReplicas[i] > 0
		lws := target.Roles[roleName]
		if lws != nil {
			state.Target.SpecReplicas[i] = int(getLWSReplicas(lws))
			state.Target.RawReadyReplicas[i] = readiness[lws.Name].raw
			state.Target.ReadyReplicas[i] = readiness[lws.Name].committed
		}
	}
	return state
}

func observeOldRevision(
	revision disaggregatedsetutils.RevisionRoles,
	roleNames []string,
	readiness rolloutReadiness,
) (RoleReplicaState, ParkedRevisionState) {
	initial := make(RoleReplicaState, len(roleNames))
	state := ParkedRevisionState{
		RequiredRoles:    make([]bool, len(roleNames)),
		SpecReplicas:     make(RoleReplicaState, len(roleNames)),
		RawReadyReplicas: make(RoleReplicaState, len(roleNames)),
		ReadyReplicas:    make(RoleReplicaState, len(roleNames)),
	}
	for i, roleName := range roleNames {
		lws := revision.Roles[roleName]
		if lws == nil {
			continue
		}
		state.SpecReplicas[i] = int(getLWSReplicas(lws))
		// Spec may exceed a stale annotation after an External scale-down or a
		// manual edit. Never describe live replicas as outside the old baseline.
		initial[i] = max(revision.GetInitialReplicasPerRole(roleName), state.SpecReplicas[i])
		state.RequiredRoles[i] = initial[i] > 0
		state.RawReadyReplicas[i] = readiness[lws.Name].raw
		state.ReadyReplicas[i] = readiness[lws.Name].committed
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
) RoleReplicaState {
	targets := make(RoleReplicaState, len(allRoleNames))
	for i, roleName := range allRoleNames {
		if !desiredRoles.Has(roleName) {
			continue
		}
		targets[i] = desiredReplicasByRole[roleName]
		lws := newRevision.Roles[roleName]
		// Keep an External target from shrinking only while old capacity for
		// this role still overlaps it. Drained old objects or another role's
		// old replicas must not keep this target artificially high.
		if isExternal(ds, roleName) && oldRevisions.GetTotalReplicasPerRole(roleName) > 0 && lws != nil {
			targets[i] = max(targets[i], int(getLWSReplicas(lws)))
		}
	}
	return targets
}

func isExternal(ds *disaggregatedsetv1.DisaggregatedSet, roleName string) bool {
	for _, p := range ds.Spec.Roles {
		if p.Name == roleName {
			return p.Scaling != nil && p.Scaling.Mode == disaggregatedsetv1.RoleScalingExternal
		}
	}
	return false
}

func extractRollingUpdateConfig(
	ds *disaggregatedsetv1.DisaggregatedSet,
	allRoleNames []string,
	desiredReplicasByRole map[string]int,
) []RollingUpdateConfig {
	config := make([]RollingUpdateConfig, len(allRoleNames))
	roleIndex := make(map[string]int, len(allRoleNames))
	for i, name := range allRoleNames {
		config[i].MaxSurge = 1
		roleIndex[name] = i
	}

	for _, role := range ds.Spec.Roles {
		if rc := role.Spec.RolloutStrategy.RollingUpdateConfiguration; rc != nil {
			replicas := desiredReplicasByRole[role.Name]
			// Use GetScaledValueFromIntOrPercent to handle both integers and percentages.
			// For maxSurge, round up (true); for maxUnavailable, round down (false).
			surge, _ := intstr.GetScaledValueFromIntOrPercent(&rc.MaxSurge, replicas, true)
			unavail, _ := intstr.GetScaledValueFromIntOrPercent(&rc.MaxUnavailable, replicas, false)
			cfg := RollingUpdateConfig{MaxSurge: surge, MaxUnavailable: unavail}
			if surge == 0 && unavail == 0 {
				cfg.MaxSurge = 1
			}
			config[roleIndex[role.Name]] = cfg
		}
	}
	return config
}

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
) (specComplete, targetReady bool) {
	targetReady = true
	for i, roleName := range roleNames {
		if oldRevisions.GetTotalReplicasPerRole(roleName) != 0 {
			return false, false
		}

		target := targetReplicas[i]
		if target == 0 {
			continue
		}

		lws := targetRevision.Roles[roleName]
		if lws == nil || int(getLWSReplicas(lws)) < target {
			return false, false
		}
		if readiness[lws.Name].committed < target {
			targetReady = false
		}
	}
	return true, targetReady
}

// --- Scaling operations ---

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

	log := logf.FromContext(ctx)
	for i, name := range roleNames {
		lws := revision.Roles[name]
		if lws == nil {
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
		if err := executor.LWSManager.Scale(ctx, ds, lws, desiredSpec); err != nil {
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

	snapshot := snapshotForRolloutState(state)
	phaseTargets := targetReplicasForActiveRevision(snapshot)
	normalLimits := hardNewReplicaLimits(snapshot)
	newLimits, availabilitySnapshot, err := validateFallbackSelection(state, step, snapshot, phaseTargets, normalLimits)
	if err != nil {
		return err
	}
	if err := validateReplicaTargets(state, step, snapshot, phaseTargets, normalLimits, newLimits); err != nil {
		return err
	}
	if !availabilityPreserved(availabilitySnapshot, step.Past, state.ActiveOld.RequiredRoles) {
		return fmt.Errorf("old targets reduce usable readiness below its safe bound")
	}
	return nil
}

// validateStepStructure checks that the planner output matches the role layout
// and does not leave only part of the active revision running.
func validateStepStructure(state RolloutState, step *UpdateStep) error {
	if step == nil {
		return fmt.Errorf("step is nil")
	}
	if len(step.Past) != len(state.Config) || len(step.New) != len(state.Config) {
		return fmt.Errorf("target lengths do not match role count")
	}
	if bounded := boundOldTargetsByRevisionCompleteness(state.ActiveOld.SpecReplicas, step.Past, state.ActiveOld.RequiredRoles); !slices.Equal(bounded, step.Past) {
		return fmt.Errorf("old targets leave required roles incomplete")
	}
	return nil
}

// validateFallbackSelection verifies that an emergency policy is used only
// when safer progress is unavailable and returns the bounds for that policy.
func validateFallbackSelection(
	state RolloutState,
	step *UpdateStep,
	snapshot rolloutSnapshot,
	phaseTargets RoleReplicaState,
	normalLimits RoleReplicaState,
) (RoleReplicaState, rolloutSnapshot, error) {
	ordinaryPast := furthestOldTargets(snapshot, state.ActiveOld.RequiredRoles)
	ordinaryNew := furthestNewTargets(snapshot, phaseTargets)
	ordinaryStepAvailable := anyChange(ordinaryPast, ordinaryNew, state.ActiveOld.SpecReplicas, state.Target.SpecReplicas)
	bootstrapSnapshot, bootstrapSurgeAvailable := snapshotWithBootstrapSurge(snapshot, phaseTargets)
	bootstrapStepAvailable := bootstrapSurgeAvailable && anyChange(ordinaryPast, furthestNewTargets(bootstrapSnapshot, phaseTargets), state.ActiveOld.SpecReplicas, state.Target.SpecReplicas)
	newLimits := normalLimits
	availabilitySnapshot := snapshot
	if step.UsesBootstrapSurge && step.UsesUnavailableFallback {
		return nil, nil, fmt.Errorf("bootstrap surge and availability fallback cannot be used together")
	}
	if step.UsesBootstrapSurge {
		if err := validateBootstrapSurgeStep(ordinaryStepAvailable, bootstrapSurgeAvailable); err != nil {
			return nil, nil, err
		}
		newLimits = hardNewReplicaLimits(bootstrapSnapshot)
	}
	if step.UsesUnavailableFallback {
		fallbackSnapshot := snapshotWithUnavailableFallback(snapshot)
		if err := validateUnavailableFallbackStep(state, step, fallbackSnapshot, ordinaryStepAvailable, bootstrapStepAvailable); err != nil {
			return nil, nil, err
		}
		availabilitySnapshot = fallbackSnapshot
	}
	return newLimits, availabilitySnapshot, nil
}

// validateReplicaTargets checks the fractional window and per-role replica
// bounds, and rejects an incorrectly marked or no-op step.
func validateReplicaTargets(
	state RolloutState,
	step *UpdateStep,
	snapshot rolloutSnapshot,
	phaseTargets RoleReplicaState,
	normalLimits RoleReplicaState,
	newLimits RoleReplicaState,
) error {
	if bounded := boundDrainingRoleTargetsToWindow(state.ActiveOld.SpecReplicas, state.ActiveOld.InitialReplicas, step.Past); !slices.Equal(bounded, step.Past) {
		return fmt.Errorf("old targets exceed the fractional coordination window")
	}
	if bounded := boundGrowingRoleTargetsToWindow(state.Target.SpecReplicas, phaseTargets, step.New); !slices.Equal(bounded, step.New) {
		return fmt.Errorf("new targets exceed the fractional coordination window")
	}
	changed := false
	usedBootstrapSurge := false
	for i, role := range snapshot {
		if step.Past[i] < 0 || step.Past[i] > role.ActiveOldSpecReplicas {
			return fmt.Errorf("old target %d for role %d is outside [0,%d]", step.Past[i], i, role.ActiveOldSpecReplicas)
		}
		maxNew := min(newLimits[i], phaseTargets[i])
		if step.New[i] < role.NewSpecReplicas || step.New[i] > maxNew {
			return fmt.Errorf("new target %d for role %d is outside [%d,%d]", step.New[i], i, role.NewSpecReplicas, maxNew)
		}
		usedBootstrapSurge = usedBootstrapSurge || step.New[i] > min(normalLimits[i], phaseTargets[i])
		changed = changed || step.Past[i] < role.ActiveOldSpecReplicas || step.New[i] > role.NewSpecReplicas
	}
	if step.UsesBootstrapSurge != usedBootstrapSurge {
		return fmt.Errorf("bootstrap surge marker does not match the new replica targets")
	}
	if !changed {
		return fmt.Errorf("plan makes no API change")
	}
	return nil
}

func validateBootstrapSurgeStep(
	ordinaryStepAvailable, bootstrapSurgeAvailable bool,
) error {
	if ordinaryStepAvailable {
		return fmt.Errorf("bootstrap surge used while an ordinary rollout step is available")
	}
	if !bootstrapSurgeAvailable {
		return fmt.Errorf("bootstrap surge used without a missing blocked target role")
	}
	return nil
}

func validateUnavailableFallbackStep(
	state RolloutState,
	step *UpdateStep,
	fallbackSnapshot rolloutSnapshot,
	ordinaryStepAvailable, bootstrapStepAvailable bool,
) error {
	if ordinaryStepAvailable {
		return fmt.Errorf("availability fallback used while an ordinary rollout step is available")
	}
	if bootstrapStepAvailable {
		return fmt.Errorf("availability fallback used while bootstrap surge is available")
	}

	expectedPast := furthestOldTargets(fallbackSnapshot, state.ActiveOld.RequiredRoles)
	if !slices.Equal(step.Past, expectedPast) || !slices.Equal(step.New, state.Target.SpecReplicas) {
		return fmt.Errorf("availability fallback does not match the bounded planner target")
	}
	if !drainsOverSurgeUnschedulableRole(
		fallbackSnapshot,
		state.ActiveOld.SpecReplicas,
		step.Past,
		state.Target.UnschedulableRoles,
	) {
		return fmt.Errorf("availability fallback does not release excess surge for an unschedulable target role")
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
	for i, roleName := range roleNames {
		lws := newRevision.Roles[roleName]
		if lws == nil {
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

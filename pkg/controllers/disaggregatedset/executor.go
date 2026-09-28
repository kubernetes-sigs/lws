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
	leaderworkersetv1 "sigs.k8s.io/lws/api/leaderworkerset/v1"
	disaggregatedsetutils "sigs.k8s.io/lws/pkg/utils/disaggregatedset"
)

const (
	EventReasonRollingUpdateStarted   = "RollingUpdateStarted"
	EventReasonRollingUpdateCompleted = "RollingUpdateCompleted"
	EventReasonScalingUp              = "ScalingUp"
	EventReasonScalingDown            = "ScalingDown"
	EventReasonBootstrapSurge         = "BootstrapSurge"
	EventReasonRevisionDrainBlocked   = "RevisionDrainBlocked"
	EventReasonInitialReplicasMissing = "InitialReplicasMissing"
	EventReasonLWSDeleted             = "LWSDeleted"
)

type RollingUpdateExecutor struct {
	Record     events.EventRecorder
	LWSManager *LeaderWorkerSetManager
}

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
		if err := executor.LWSManager.Create(ctx, disaggregatedSet, roleConfigs[roleName], slice, 0, initialReplicas); err != nil {
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
	newRevision disaggregatedsetutils.RevisionRoles,
	desiredReplicasByRole map[string]int,
) (ctrl.Result, bool, error) {
	log := logf.FromContext(ctx)
	specRoleNames := disaggregatedsetutils.GetRoleNames(disaggregatedSet)
	desiredRoles, oldRoles := collectDesiredAndOldRoles(specRoleNames, oldRevisions)
	if err := executor.syncTargetInitialReplicas(ctx, disaggregatedSet, specRoleNames, newRevision, desiredReplicasByRole); err != nil {
		return ctrl.Result{}, false, err
	}

	allRoleNames := append(slices.Clone(specRoleNames), removedRoleNames(oldRoles, desiredRoles)...)
	config := extractRollingUpdateConfig(disaggregatedSet, allRoleNames, desiredReplicasByRole)
	targetReplicas := rolloutTargetReplicas(disaggregatedSet, allRoleNames, desiredRoles, oldRevisions, newRevision, desiredReplicasByRole)

	if isRolloutSpecComplete(oldRevisions, newRevision, allRoleNames, targetReplicas) {
		if !isRolloutReady(oldRevisions, newRevision, allRoleNames, targetReplicas) {
			log.V(1).Info("Waiting for target revision to become ready")
			return ctrl.Result{RequeueAfter: time.Second}, false, nil
		}
		log.Info("Rolling update complete")
		executor.Record.Eventf(disaggregatedSet, nil, corev1.EventTypeNormal, EventReasonRollingUpdateCompleted,
			"Update", "Completed rolling update to revision %s", newRevision.Revision)
		return ctrl.Result{}, true, nil
	}
	candidates := orderedRevisionCandidates(oldRevisions)
	if len(candidates) == 0 {
		if err := executor.scaleUpTargetRevision(ctx, disaggregatedSet, newRevision, specRoleNames, targetReplicas); err != nil {
			return ctrl.Result{}, false, err
		}
		return ctrl.Result{RequeueAfter: time.Second}, false, nil
	}

	var selectedRevision disaggregatedsetutils.RevisionRoles
	var selectedState RolloutState
	var selectedStep *UpdateStep
	for _, candidate := range candidates {
		state := rolloutStateForRevision(allRoleNames, oldRevisions, candidate, newRevision, targetReplicas, config)
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
	if selectedStep == nil {
		reason := "no feasible replica change is currently available within the rollout constraints"
		log.Info("Rolling update is temporarily blocked; waiting for state to change", "reason", reason)
		executor.Record.Eventf(disaggregatedSet, nil, corev1.EventTypeNormal, EventReasonRevisionDrainBlocked,
			"Wait", "Waiting to retire an old revision: %s", reason)
		return ctrl.Result{RequeueAfter: time.Second}, false, nil
	}

	if err := validateUpdateStep(selectedState, selectedStep); err != nil {
		return ctrl.Result{}, false, fmt.Errorf("planner returned an invalid rollout step: %w", err)
	}
	logArgs := append([]interface{}{"revision", selectedRevision.Revision}, buildStepLogArgs(allRoleNames, selectedStep)...)
	log.Info("Next rollout step computed", logArgs...)
	// Apply drains before growth so ordinary steps cannot transiently exceed
	// their surge ceilings. A marked bootstrap step is the sole exception.
	if err := executor.scaleDownActiveRevision(ctx, disaggregatedSet, selectedRevision, allRoleNames, selectedStep.Past); err != nil {
		return ctrl.Result{}, false, err
	}
	if err := executor.scaleUpTargetRevision(ctx, disaggregatedSet, newRevision, specRoleNames, selectedStep.New); err != nil {
		return ctrl.Result{}, false, err
	}
	if selectedStep.UsesBootstrapSurge {
		roles := bootstrapSurgeRoleNames(allRoleNames, selectedState, selectedStep)
		log.Info("Used bootstrap surge to unblock rolling update", "roles", roles)
		executor.Record.Eventf(disaggregatedSet, nil, corev1.EventTypeWarning, EventReasonBootstrapSurge,
			"Bootstrap", "Temporarily exceeded maxSurge by one replica for roles %v to preserve revision completeness", roles)
	}

	// Object updates normally trigger the next reconcile immediately. The
	// timer also lets the planner retry when pending replicas become Ready.
	return ctrl.Result{RequeueAfter: time.Second}, false, nil
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

func removedRoleNames(oldRoles, desiredRoles sets.Set[string]) []string {
	removed := oldRoles.Difference(desiredRoles).UnsortedList()
	slices.Sort(removed)
	return removed
}

// orderedRevisionCandidates returns non-empty old revisions in planner
// preference order. Fully unready revisions come first. Each group is newest
// first. A preference is not a decision: the executor continues when a
// candidate-specific plan is blocked.
func orderedRevisionCandidates(oldRevisions disaggregatedsetutils.RevisionRolesList) disaggregatedsetutils.RevisionRolesList {
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
		if revisionIsFullyUnready(revision) {
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
func revisionIsFullyUnready(revision disaggregatedsetutils.RevisionRoles) bool {
	for _, lws := range revision.Roles {
		if lws.Status.ReadyReplicas > 0 {
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
) RolloutState {
	initial, activeState := observeOldRevision(active, roleNames)
	state := RolloutState{
		ActiveOld: ActiveRevisionState{
			RequiredRoles:   activeState.RequiredRoles,
			InitialReplicas: initial,
			SpecReplicas:    activeState.SpecReplicas,
			ReadyReplicas:   activeState.ReadyReplicas,
		},
		Target: TargetRevisionState{
			RequiredRoles:   make([]bool, len(roleNames)),
			SpecReplicas:    make(RoleReplicaState, len(roleNames)),
			ReadyReplicas:   make(RoleReplicaState, len(roleNames)),
			DesiredReplicas: slicesClone(targetReplicas),
		},
		Config: slices.Clone(config),
	}
	for _, revision := range oldRevisions {
		if revision.Revision == active.Revision {
			continue
		}
		_, parked := observeOldRevision(revision, roleNames)
		state.ParkedOld = append(state.ParkedOld, parked)
	}
	for i, roleName := range roleNames {
		state.Target.RequiredRoles[i] = targetReplicas[i] > 0
		lws := target.Roles[roleName]
		if lws != nil {
			state.Target.SpecReplicas[i] = int(getLWSReplicas(lws))
			state.Target.ReadyReplicas[i] = committedReadyReplicas(lws)
		}
	}
	return state
}

func observeOldRevision(
	revision disaggregatedsetutils.RevisionRoles,
	roleNames []string,
) (RoleReplicaState, ParkedRevisionState) {
	initial := make(RoleReplicaState, len(roleNames))
	state := ParkedRevisionState{
		RequiredRoles: make([]bool, len(roleNames)),
		SpecReplicas:  make(RoleReplicaState, len(roleNames)),
		ReadyReplicas: make(RoleReplicaState, len(roleNames)),
	}
	for i, roleName := range roleNames {
		lws := revision.Roles[roleName]
		if lws == nil {
			continue
		}
		initial[i] = revision.GetInitialReplicasPerRole(roleName)
		state.RequiredRoles[i] = initial[i] > 0
		state.SpecReplicas[i] = int(getLWSReplicas(lws))
		state.ReadyReplicas[i] = committedReadyReplicas(lws)
	}
	return initial, state
}

// committedReadyReplicas returns the Ready capacity that can authorize another
// scale-down. While a previous scale-down is still pending, any replica above
// Spec may be a Ready replica selected for deletion. Reserve all such replicas
// so the same availability capacity cannot be spent twice.
func committedReadyReplicas(lws *leaderworkersetv1.LeaderWorkerSet) int {
	if lws == nil {
		return 0
	}
	specReplicas := int(getLWSReplicas(lws))
	pendingDrain := max(0, int(lws.Status.Replicas)-specReplicas)
	readyAfterPendingDrain := max(0, int(lws.Status.ReadyReplicas)-pendingDrain)
	return min(specReplicas, readyAfterPendingDrain)
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
			cfg := RollingUpdateConfig{MaxSurge: 1, MaxUnavailable: 0}
			if unavail > 0 {
				cfg.MaxUnavailable = unavail
				cfg.MaxSurge = surge
			} else if surge > 0 {
				cfg.MaxSurge = surge
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

func isRolloutSpecComplete(
	oldRevisions disaggregatedsetutils.RevisionRolesList,
	targetRevision disaggregatedsetutils.RevisionRoles,
	roleNames []string,
	targetReplicas RoleReplicaState,
) bool {
	for i, roleName := range roleNames {
		if oldRevisions.GetTotalReplicasPerRole(roleName) != 0 {
			return false
		}
		lws := targetRevision.Roles[roleName]
		if (lws == nil && targetReplicas[i] > 0) ||
			(lws != nil && int(getLWSReplicas(lws)) < targetReplicas[i]) {
			return false
		}
	}
	return true
}

func isRolloutReady(
	oldRevisions disaggregatedsetutils.RevisionRolesList,
	targetRevision disaggregatedsetutils.RevisionRoles,
	roleNames []string,
	targetReplicas RoleReplicaState,
) bool {
	if !isRolloutSpecComplete(oldRevisions, targetRevision, roleNames, targetReplicas) {
		return false
	}
	for i, roleName := range roleNames {
		lws := targetRevision.Roles[roleName]
		if (lws == nil && targetReplicas[i] > 0) ||
			(lws != nil && committedReadyReplicas(lws) < targetReplicas[i]) {
			return false
		}
	}
	return true
}

// --- Scaling operations ---

func (executor *RollingUpdateExecutor) scaleUpTargetRevision(
	ctx context.Context,
	ds *disaggregatedsetv1.DisaggregatedSet,
	targetRevision disaggregatedsetutils.RevisionRoles,
	roleNames []string,
	targets RoleReplicaState,
) error {
	log := logf.FromContext(ctx)
	for i, name := range roleNames {
		lws := targetRevision.Roles[name]
		if lws == nil {
			continue
		}
		currentSpec := int(getLWSReplicas(lws))
		desiredSpec := targets[i]
		if currentSpec >= desiredSpec {
			continue
		}
		lwsName := lws.Name
		log.Info("Scaling up", "lws", lwsName, "from_spec", currentSpec, "from_ready", committedReadyReplicas(lws), "to", desiredSpec)
		if err := executor.LWSManager.Scale(ctx, ds, lwsName, desiredSpec); err != nil {
			return fmt.Errorf("failed to scale %s: %w", lwsName, err)
		}
		executor.Record.Eventf(ds, nil, corev1.EventTypeNormal, EventReasonScalingUp,
			"Update", "Scaling up %s LWS %s from %d to %d replicas", name, lwsName, currentSpec, desiredSpec)
	}
	return nil
}

// scaleDownActiveRevision applies the planner's old-revision targets verbatim.
func (executor *RollingUpdateExecutor) scaleDownActiveRevision(
	ctx context.Context,
	ds *disaggregatedsetv1.DisaggregatedSet,
	activeRevision disaggregatedsetutils.RevisionRoles,
	roleNames []string,
	targets RoleReplicaState,
) error {
	log := logf.FromContext(ctx)
	for i, name := range roleNames {
		lws := activeRevision.Roles[name]
		if lws == nil {
			continue
		}
		currentSpec := int(getLWSReplicas(lws))
		desiredSpec := targets[i]
		if desiredSpec >= currentSpec {
			continue
		}
		log.Info("Scaling down", "lws", lws.Name, "from", currentSpec, "to", desiredSpec)
		if err := executor.LWSManager.Scale(ctx, ds, lws.Name, desiredSpec); err != nil {
			return fmt.Errorf("failed to scale %s: %w", lws.Name, err)
		}
		executor.Record.Eventf(ds, nil, corev1.EventTypeNormal, EventReasonScalingDown,
			"Update", "Scaling down %s LWS %s from %d to %d replicas", name, lws.Name, currentSpec, desiredSpec)
	}
	return nil
}

func validateUpdateStep(state RolloutState, step *UpdateStep) error {
	if step == nil {
		return fmt.Errorf("step is nil")
	}
	if len(step.Past) != len(state.Config) || len(step.New) != len(state.Config) {
		return fmt.Errorf("target lengths do not match role count")
	}
	if bounded := boundOldTargetsByRevisionCompleteness(
		state.ActiveOld.SpecReplicas,
		step.Past,
		state.ActiveOld.RequiredRoles,
	); !slices.Equal(bounded, step.Past) {
		return fmt.Errorf("old targets leave required roles incomplete")
	}

	snapshot := snapshotForRolloutState(state)
	phaseTargets := targetReplicasForActiveRevision(snapshot)
	normalLimits := hardNewReplicaLimits(snapshot)
	newLimits := normalLimits
	if step.UsesBootstrapSurge {
		normalOld := furthestOldTargets(snapshot, state.ActiveOld.RequiredRoles)
		normalNew := furthestNewTargets(snapshot, phaseTargets)
		if anyChange(normalOld, normalNew, state.ActiveOld.SpecReplicas, state.Target.SpecReplicas) {
			return fmt.Errorf("bootstrap surge used while an ordinary rollout step is available")
		}
		bootstrapSnapshot, ok := snapshotWithBootstrapSurge(snapshot, phaseTargets)
		if !ok {
			return fmt.Errorf("bootstrap surge used without a missing blocked target role")
		}
		newLimits = hardNewReplicaLimits(bootstrapSnapshot)
	}
	if bounded := boundDrainingRoleTargetsToWindow(
		state.ActiveOld.SpecReplicas,
		state.ActiveOld.InitialReplicas,
		step.Past,
	); !slices.Equal(bounded, step.Past) {
		return fmt.Errorf("old targets exceed the fractional coordination window")
	}
	if bounded := boundGrowingRoleTargetsToWindow(
		state.Target.SpecReplicas,
		phaseTargets,
		step.New,
	); !slices.Equal(bounded, step.New) {
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
	if !availabilityPreserved(snapshot, step.Past, state.ActiveOld.RequiredRoles) {
		return fmt.Errorf("old targets reduce usable readiness below its safe bound")
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

// syncTargetInitialReplicas follows replica-only and external-scaler changes
// while a revision is current. The value freezes when that revision becomes
// old, preserving the target it would have reached had its rollout completed.
func (executor *RollingUpdateExecutor) syncTargetInitialReplicas(
	ctx context.Context,
	ds *disaggregatedsetv1.DisaggregatedSet,
	roleNames []string,
	newRevision disaggregatedsetutils.RevisionRoles,
	desiredReplicasByRole map[string]int,
) error {
	for _, roleName := range roleNames {
		lws := newRevision.Roles[roleName]
		if lws == nil {
			continue
		}
		initial := desiredReplicasByRole[roleName]
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

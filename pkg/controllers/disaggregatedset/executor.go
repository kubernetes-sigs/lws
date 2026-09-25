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
//  3. Select one old revision and ask the planner for its next safe step.
//  4. Drain that revision, then grow the current revision.
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
	snapshot := buildRolloutSnapshot(disaggregatedSet, allRoleNames, desiredRoles, oldRevisions, newRevision, desiredReplicasByRole, config)

	if isRolloutSpecComplete(snapshot) {
		if !isRolloutReady(snapshot) {
			log.V(1).Info("Waiting for target revision to become ready")
			return ctrl.Result{RequeueAfter: time.Second}, false, nil
		}
		log.Info("Rolling update complete")
		executor.Record.Eventf(disaggregatedSet, nil, corev1.EventTypeNormal, EventReasonRollingUpdateCompleted,
			"Update", "Completed rolling update to revision %s", newRevision.Revision)
		return ctrl.Result{}, true, nil
	}
	activeRevision, hasActiveRevision, fullyUnready := selectRevisionToDrain(oldRevisions)
	var parkedReadyReplicas RoleReplicaState
	activeSpecs := make(RoleReplicaState, len(snapshot))
	revisionsToDrain := oldRevisions
	if hasActiveRevision {
		parkedReadyReplicas, activeSpecs = planningStateForRevision(snapshot, allRoleNames, activeRevision)
		revisionsToDrain = disaggregatedsetutils.RevisionRolesList{activeRevision}
	}

	var nextStep *UpdateStep
	if fullyUnready {
		nextStep = &UpdateStep{Past: make(RoleReplicaState, len(snapshot)), New: make(RoleReplicaState, len(snapshot))}
		for i := range snapshot {
			nextStep.Past[i] = snapshot[i].OldSpecReplicas - activeSpecs[i]
			nextStep.New[i] = snapshot[i].NewSpecReplicas
		}
	} else {
		nextStep = ComputeNextStep(snapshot, parkedReadyReplicas)
	}
	if nextStep == nil {
		log.Info("Rolling update is temporarily blocked; waiting for state to change")
		return ctrl.Result{RequeueAfter: time.Second}, false, nil
	}

	log.Info("Next step computed", buildStepLogArgs(allRoleNames, nextStep)...)
	// Scale down old replicas before scaling up new ones. This ordering ensures
	// the total replica count never exceeds the surge limit between the two
	// API calls: e.g. with surge=0, scaling up first would briefly make
	// (currentOld + nextStep.New) exceed the target before scaleDownOld brings
	// currentOld down.
	if err := executor.scaleDownOld(ctx, disaggregatedSet, revisionsToDrain, allRoleNames, snapshot, nextStep.Past, nextStep.New, parkedReadyReplicas); err != nil {
		return ctrl.Result{}, false, err
	}
	if err := executor.scaleUpNew(ctx, disaggregatedSet, newRevision, specRoleNames, nextStep.New); err != nil {
		return ctrl.Result{}, false, err
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

// selectRevisionToDrain picks one revision for this phase. Revisions with no Ready
// replicas are discarded first; otherwise revisions drain newest to oldest.
func selectRevisionToDrain(oldRevisions disaggregatedsetutils.RevisionRolesList) (
	disaggregatedsetutils.RevisionRoles, bool, bool,
) {
	var newest disaggregatedsetutils.RevisionRoles
	found := false
	for _, revision := range oldRevisions.SortedByNewestTimestamp() {
		replicas := 0
		for _, lws := range revision.Roles {
			replicas += int(getLWSReplicas(lws))
		}
		if replicas == 0 {
			continue
		}
		if !found {
			newest, found = revision, true
		}
		if revisionIsFullyUnready(revision) {
			return revision, true, true
		}
	}
	return newest, found, false
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

// planningStateForRevision returns the Ready capacity parked outside the active
// revision and the active revision's current Specs.
func planningStateForRevision(
	snapshot rolloutSnapshot,
	roleNames []string,
	active disaggregatedsetutils.RevisionRoles,
) (RoleReplicaState, RoleReplicaState) {
	parkedReadyReplicas := make(RoleReplicaState, len(snapshot))
	activeSpecs := make(RoleReplicaState, len(snapshot))
	for i, roleName := range roleNames {
		lws := active.Roles[roleName]
		activeReady := 0
		if lws != nil {
			activeSpecs[i] = int(getLWSReplicas(lws))
			activeReady = committedReadyReplicas(lws)
		}
		parkedReadyReplicas[i] = snapshot[i].OldReadyReplicas - activeReady
	}
	return parkedReadyReplicas, activeSpecs
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

func buildRolloutSnapshot(
	ds *disaggregatedsetv1.DisaggregatedSet,
	allRoleNames []string,
	desiredRoles sets.Set[string],
	oldRevisions disaggregatedsetutils.RevisionRolesList,
	newRevision disaggregatedsetutils.RevisionRoles,
	desiredReplicasByRole map[string]int,
	config []RollingUpdateConfig,
) rolloutSnapshot {
	snapshot := make(rolloutSnapshot, len(allRoleNames))

	for i, roleName := range allRoleNames {
		roleState := roleRolloutSnapshot{
			InitialOldReplicas: oldRevisions.GetMaxInitialReplicasPerRole(roleName),
			OldSpecReplicas:    oldRevisions.GetTotalReplicasPerRole(roleName),
			Config:             config[i],
		}
		for _, revision := range oldRevisions {
			if lws := revision.Roles[roleName]; lws != nil {
				roleState.OldReadyReplicas += committedReadyReplicas(lws)
			}
		}

		if desiredRoles.Has(roleName) {
			lws := newRevision.Roles[roleName]
			if lws != nil {
				roleState.NewSpecReplicas = int(getLWSReplicas(lws))
				roleState.NewReadyReplicas = committedReadyReplicas(lws)
			}
			roleState.NewTargetReplicas = desiredReplicasByRole[roleName]
			// No-shrink guard: an External role mid-rollout must not shrink the
			// new-revision fleet if HPA writes a smaller value while the old
			// revision is still draining. Releases once the rollout completes.
			if isExternal(ds, roleName) && len(oldRevisions) > 0 && lws != nil {
				roleState.NewTargetReplicas = max(roleState.NewTargetReplicas, roleState.NewSpecReplicas)
			}
		}
		snapshot[i] = roleState
	}

	return snapshot
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
			"past_"+name, step.Past[i],
			"new_"+name, step.New[i],
		)
	}
	return args
}

func isRolloutReady(snapshot rolloutSnapshot) bool {
	for _, role := range snapshot {
		if role.OldSpecReplicas != 0 || role.NewReadyReplicas < role.NewTargetReplicas {
			return false
		}
	}
	return true
}

// --- Scaling operations ---

func (executor *RollingUpdateExecutor) scaleUpNew(
	ctx context.Context,
	ds *disaggregatedsetv1.DisaggregatedSet,
	newRevision disaggregatedsetutils.RevisionRoles,
	roleNames []string,
	targetNew RoleReplicaState,
) error {
	log := logf.FromContext(ctx)
	for i, name := range roleNames {
		lws := newRevision.Roles[name]
		if lws == nil {
			continue
		}
		currentSpec := int(getLWSReplicas(lws))
		desiredSpec := targetNew[i]
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

func (executor *RollingUpdateExecutor) scaleDownOld(
	ctx context.Context,
	ds *disaggregatedsetv1.DisaggregatedSet,
	oldRevisions disaggregatedsetutils.RevisionRolesList,
	roleNames []string,
	snapshot rolloutSnapshot,
	targetOld RoleReplicaState,
	targetNew RoleReplicaState,
	parkedReadyReplicas RoleReplicaState,
) error {
	budget := make(RoleReplicaState, len(roleNames))
	for i := range roleNames {
		roleState := snapshot[i]
		budget[i] = max(0, min(roleState.OldSpecReplicas-targetOld[i], maxSafeDrain(roleState)))
	}

	log := logf.FromContext(ctx)
	for _, wl := range oldRevisions.SortedByNewestTimestamp() {
		fullyUnready := revisionIsFullyUnready(wl)
		plannedDrain := make(RoleReplicaState, len(roleNames))
		for i, name := range roleNames {
			if lws := wl.Roles[name]; lws != nil {
				plannedDrain[i] = min(budget[i], int(getLWSReplicas(lws)))
				if fullyUnready {
					plannedDrain[i] = int(getLWSReplicas(lws))
				}
			}
		}
		if !anyPositive(plannedDrain) {
			continue
		}

		blocked := coordinateRevisionDrain(roleNames, wl.Roles, plannedDrain, targetNew, parkedReadyReplicas, snapshot)
		if blocked {
			log.Info("Waiting to retire old revision without leaving it incomplete", "revision", wl.Revision)
			executor.Record.Eventf(ds, nil, corev1.EventTypeNormal, EventReasonRevisionDrainBlocked,
				"Wait", "Waiting to retire revision %s: no safe partial drain or replacement growth is currently available", wl.Revision)
		}

		for i, name := range roleNames {
			lws := wl.Roles[name]
			if lws == nil || plannedDrain[i] == 0 {
				continue
			}
			replicas := int(getLWSReplicas(lws))
			drain := plannedDrain[i]
			newReplicas := replicas - drain
			// Address the discovered LWS by its actual name.
			lwsName := lws.Name
			log.Info("Scaling down", "lws", lwsName, "from", replicas, "to", newReplicas)
			if err := executor.LWSManager.Scale(ctx, ds, lwsName, newReplicas); err != nil {
				return fmt.Errorf("failed to scale %s: %w", lwsName, err)
			}
			executor.Record.Eventf(ds, nil, corev1.EventTypeNormal, EventReasonScalingDown,
				"Update", "Scaling down %s LWS %s from %d to %d replicas", name, lwsName, replicas, newReplicas)
		}
		// Never move a budget past the newest revision that can consume it.
		return nil
	}

	return nil
}

// coordinateRevisionDrain prevents a reconciliation from intentionally leaving
// an old revision without one of its roles. It is called for every proposed
// old-revision drain. Cases 2-4 handle the fixed point where per-role surge or
// availability limits prevent the proposed roles from retiring together. The
// planner cannot manufacture capacity when those hard limits leave neither a
// safe drain nor room for replacement replicas.
//
// The function resolves a proposed drain in this order:
//  1. If every role can retire within its availability budget, retire the whole
//     revision.
//  2. Otherwise, turn proposed full drains into partial drains, leaving at
//     least one replica of every role currently present in the revision.
//  3. If no proposed partial drain remains, use any other safe partial drain
//     in this revision. This can relax fractional lockstep to unblock the
//     newest revision, but every role keeps at least one replica.
//  4. If no partial drain exists, cancel the full drains and grow the new roles
//     that currently prevent coordinated retirement.
//     This may relax fractional lockstep, but never the hard surge or pending
//     readiness limits.
//  5. If neither a partial drain nor replacement growth is currently possible,
//     cancel the unsafe drain and report the step as blocked. A later readiness,
//     capacity, or rollout-budget change can make coordinated retirement possible.
//
// Both drain and targetNew are mutated in place. The return value is true only
// for case 5.
func coordinateRevisionDrain(
	roleNames []string,
	roles map[string]*leaderworkersetv1.LeaderWorkerSet,
	drain RoleReplicaState,
	targetNew RoleReplicaState,
	parkedReadyReplicas RoleReplicaState,
	snapshot rolloutSnapshot,
) bool {
	anyAliveAfter, anyRetired, canRetire := false, false, true
	for i, name := range roleNames {
		lws := roles[name]
		if lws == nil || getLWSReplicas(lws) == 0 {
			continue
		}
		replicas := int(getLWSReplicas(lws))
		anyAliveAfter = anyAliveAfter || replicas > drain[i]
		anyRetired = anyRetired || drain[i] == replicas
		// Availability is spent only by Ready replicas; unready Spec replicas
		// can retire without consuming the Ready-based drain budget.
		canRetire = canRetire && committedReadyReplicas(lws) <= maxSafeDrain(snapshot[i])
	}
	if !anyAliveAfter || !anyRetired {
		return false
	}
	if canRetire {
		for i, name := range roleNames {
			if lws := roles[name]; lws != nil {
				drain[i] = int(getLWSReplicas(lws))
			}
		}
		return false
	}

	// Keep at least one replica of every role until the whole revision can be
	// retired. A full drain of N replicas therefore becomes a partial drain of
	// N-1; a singleton cannot be partially drained.
	for i, name := range roleNames {
		if lws := roles[name]; lws != nil {
			replicas := int(getLWSReplicas(lws))
			if replicas > 0 && drain[i] == replicas {
				drain[i] = replicas - 1
			}
		}
	}
	if anyPositive(drain) {
		return false
	}
	for i, name := range roleNames {
		if lws := roles[name]; lws != nil {
			replicas := int(getLWSReplicas(lws))
			drain[i] = min(max(0, replicas-1), maxSafeDrain(snapshot[i]))
		}
	}
	if anyPositive(drain) {
		return false
	}

	// No safe partial drain remains. Grow only the roles whose current
	// availability prevents the whole revision from retiring. hardNewReplicaLimits
	// deliberately omits the fractional coordination window here: this is the
	// liveness escape hatch, while surge and pending-readiness limits remain hard.
	limits := hardNewReplicaLimits(snapshot)
	for i, name := range roleNames {
		lws := roles[name]
		if lws == nil || getLWSReplicas(lws) == 0 {
			continue
		}
		neededForRetirement := committedReadyReplicas(lws) - maxSafeDrain(snapshot[i])
		if neededForRetirement > 0 {
			phaseTarget := snapshot[i].NewTargetReplicas
			if i < len(parkedReadyReplicas) {
				phaseTarget -= parkedReadyReplicas[i]
			}
			phaseTarget = max(snapshot[i].NewSpecReplicas, phaseTarget)
			targetNew[i] = max(targetNew[i], min(limits[i], phaseTarget, snapshot[i].NewSpecReplicas+neededForRetirement))
		}
	}
	for i := range snapshot {
		if targetNew[i] > snapshot[i].NewSpecReplicas {
			return false
		}
	}
	return true
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

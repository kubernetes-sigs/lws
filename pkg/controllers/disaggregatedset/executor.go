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
	EventReasonLWSDeleted             = "LWSDeleted"
)

type RollingUpdateExecutor struct {
	Record     events.EventRecorder
	LWSManager *LeaderWorkerSetManager
}

// roleRolloutState separates work already issued through Spec from work that
// has completed and is serving through Ready. The planner advances Spec; Ready
// only authorizes additional in-flight work and old-replica drains.
type roleRolloutState struct {
	InitialOld int
	OldSpec    int
	OldReady   int
	NewSpec    int
	NewReady   int
	Target     int
	Config     RollingUpdateConfig
}

// rolloutState is index-aligned with the role-name slice used by the caller.
type rolloutState []roleRolloutState

// ReconcileRollingUpdateNew is the entry point for rolling update reconciliation.
// It fetches current cluster state and either:
//  1. Starts a new rolling update (initRollingUpdate) if no LWS for the target
//     revision exist yet, or
//  2. Continues an in-progress rolling update (ReconcileRollingUpdate) by
//     computing and executing the next scale step.
func (executor *RollingUpdateExecutor) ReconcileRollingUpdateNew(
	ctx context.Context,
	disaggregatedSet *disaggregatedsetv1.DisaggregatedSet,
	slice int,
	revision string,
	scalers map[string]*disaggregatedsetv1.DisaggregatedSetRoleScaler,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	roleNames := disaggregatedsetutils.GetRoleNames(disaggregatedSet)
	roleConfigs := disaggregatedsetutils.GetRoleConfigs(disaggregatedSet)

	oldRevisions, newRevision, err := executor.LWSManager.GetRevisionRolesList(ctx, disaggregatedSet, slice, revision)
	if err != nil {
		return ctrl.Result{}, err
	}
	if len(oldRevisions) == 0 {
		return ctrl.Result{}, nil
	}

	addedRoles, removedRoles := detectRoleChanges(roleNames, oldRevisions)
	if len(addedRoles) > 0 || len(removedRoles) > 0 {
		log.Info("Role changes detected", "added", addedRoles, "removed", removedRoles)
	}

	if newRevision == nil {
		return executor.initRollingUpdate(ctx, disaggregatedSet, slice, revision, roleNames, roleConfigs, oldRevisions)
	}

	return executor.ReconcileRollingUpdate(ctx, disaggregatedSet, oldRevisions, *newRevision, scalers)
}

func (executor *RollingUpdateExecutor) initRollingUpdate(
	ctx context.Context,
	disaggregatedSet *disaggregatedsetv1.DisaggregatedSet,
	slice int,
	revision string,
	roleNames []string,
	roleConfigs map[string]*disaggregatedsetv1.DisaggregatedRoleSpec,
	oldRevisions disaggregatedsetutils.RevisionRolesList,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	log.Info("Initiating new rolling update", "revision", revision)
	executor.Record.Eventf(disaggregatedSet, nil, corev1.EventTypeNormal, EventReasonRollingUpdateStarted,
		"Update", "Started rolling update to revision %s", revision)

	// Snapshot each old LWS's current replica count as the initial-replicas
	// annotation. The planner uses this as the baseline for proportional drain
	// calculations, since Spec.Replicas changes as the rollout progresses.
	for _, oldGrouped := range oldRevisions {
		for _, roleLWS := range oldGrouped.Roles {
			replicas := 1
			if roleLWS.Spec.Replicas != nil {
				replicas = int(*roleLWS.Spec.Replicas)
			}
			// Address by the LWS's actual name so a legacy slice-0 object (whose name
			// has no slice segment) is updated rather than missed.
			if _, err := executor.LWSManager.SetInitialReplicas(ctx, disaggregatedSet.Namespace, roleLWS.Name, replicas); err != nil {
				log.Error(err, "Failed to set initial-replicas annotation", "lws", roleLWS.Name)
			}
		}
	}

	// Create new LWS objects (one per role) for the target revision with 0
	// replicas. The next reconcile loop will start scaling them up.
	for _, roleName := range roleNames {
		if _, err := executor.ensureNewLWSExists(ctx, disaggregatedSet, slice, revision, roleName, roleConfigs[roleName], 0); err != nil {
			return ctrl.Result{}, err
		}
	}

	return ctrl.Result{RequeueAfter: time.Second}, nil
}

// ReconcileRollingUpdate executes one step of an in-progress rolling update.
// Spec counts represent already-issued rollout work and drive the planner.
// Ready counts represent completed work and bound both pending scale-up and
// safe old-replica drain. Old revisions are drained newest-first.
func (executor *RollingUpdateExecutor) ReconcileRollingUpdate(
	ctx context.Context,
	disaggregatedSet *disaggregatedsetv1.DisaggregatedSet,
	oldRevisions disaggregatedsetutils.RevisionRolesList,
	newRevision disaggregatedsetutils.RevisionRoles,
	scalers map[string]*disaggregatedsetv1.DisaggregatedSetRoleScaler,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	specRoleNames := disaggregatedsetutils.GetRoleNames(disaggregatedSet)
	specRoleSet, oldRoleSet := buildRoleSets(specRoleNames, oldRevisions)

	allRoleNames := append(slices.Clone(specRoleNames), removedRoleNames(oldRoleSet, specRoleSet)...)
	config := extractRollingUpdateConfig(disaggregatedSet, allRoleNames, scalers)
	state := buildRolloutState(disaggregatedSet, allRoleNames, specRoleSet, oldRevisions, newRevision, scalers, config)
	initialOld, currentOld, currentNewSpec, targetNew := plannerState(state)

	if isComplete(currentOld, currentNewSpec, targetNew) {
		if !isRolloutReady(state) {
			log.V(1).Info("Waiting for target revision to become ready")
			return ctrl.Result{RequeueAfter: time.Second}, nil
		}
		log.Info("Rolling update complete")
		executor.Record.Eventf(disaggregatedSet, nil, corev1.EventTypeNormal, EventReasonRollingUpdateCompleted,
			"Update", "Completed rolling update to revision %s", newRevision.Revision)
		return ctrl.Result{}, nil
	}
	nextStep := ComputeNextStep(initialOld, currentOld, currentNewSpec, targetNew, config)
	if nextStep == nil {
		log.Info("Rolling update is temporarily blocked; waiting for state to change")
		return ctrl.Result{RequeueAfter: time.Second}, nil
	}
	nextStep.New = boundNewReplicaTargets(state, nextStep.New)
	ensureExecutableStep(state, nextStep)

	log.Info("Next step computed", buildStepLogArgs(allRoleNames, nextStep)...)
	newGrowthPlanned := false
	for i := range allRoleNames {
		if nextStep.New[i] > state[i].NewSpec {
			newGrowthPlanned = true
			break
		}
	}

	// Scale down old replicas before scaling up new ones. This ordering ensures
	// the total replica count never exceeds the surge limit between the two
	// API calls: e.g. with surge=0, scaling up first would briefly make
	// (currentOld + nextStep.New) exceed the target before scaleDownOld brings
	// currentOld down.
	if err := executor.scaleDownOld(ctx, disaggregatedSet, oldRevisions, allRoleNames, state, nextStep.Past, !newGrowthPlanned); err != nil {
		return ctrl.Result{}, err
	}
	if err := executor.scaleUpNew(ctx, disaggregatedSet, newRevision, specRoleNames, nextStep.New); err != nil {
		return ctrl.Result{}, err
	}

	// Object updates normally trigger the next reconcile immediately. The
	// timer also covers a legal no-op while pending replicas become Ready.
	return ctrl.Result{RequeueAfter: time.Second}, nil
}

// --- Helpers ---

func buildRoleSets(specRoleNames []string, oldRevisions disaggregatedsetutils.RevisionRolesList) (spec, old map[string]bool) {
	spec = make(map[string]bool, len(specRoleNames))
	for _, name := range specRoleNames {
		spec[name] = true
	}
	old = make(map[string]bool)
	for _, wl := range oldRevisions {
		for name := range wl.Roles {
			old[name] = true
		}
	}
	return spec, old
}

func removedRoleNames(oldRoleSet, specRoleSet map[string]bool) []string {
	var removed []string
	for role := range oldRoleSet {
		if !specRoleSet[role] {
			removed = append(removed, role)
		}
	}
	slices.Sort(removed)
	return removed
}

func buildRolloutState(
	ds *disaggregatedsetv1.DisaggregatedSet,
	allRoleNames []string,
	specRoleSet map[string]bool,
	oldRevisions disaggregatedsetutils.RevisionRolesList,
	newRevision disaggregatedsetutils.RevisionRoles,
	scalers map[string]*disaggregatedsetv1.DisaggregatedSetRoleScaler,
	config []RollingUpdateConfig,
) rolloutState {
	state := make(rolloutState, len(allRoleNames))

	for i, roleName := range allRoleNames {
		roleState := roleRolloutState{
			InitialOld: oldRevisions.GetTotalInitialReplicasPerRole(roleName),
			OldSpec:    oldRevisions.GetTotalReplicasPerRole(roleName),
			Config:     config[i],
		}
		for _, revision := range oldRevisions {
			if lws := revision.Roles[roleName]; lws != nil {
				roleState.OldReady += committedReadyReplicas(lws)
			}
		}

		if specRoleSet[roleName] {
			lws := newRevision.Roles[roleName]
			if lws != nil {
				roleState.NewSpec = int(getLWSReplicas(lws))
				roleState.NewReady = committedReadyReplicas(lws)
			}
			roleState.Target = getTargetReplicas(ds, roleName, scalers, roleState.NewSpec)
			// No-shrink guard: an External role mid-rollout must not shrink the
			// new-revision fleet if HPA writes a smaller value while the old
			// revision is still draining. Releases once the rollout completes.
			if isExternal(ds, roleName) && len(oldRevisions) > 0 && lws != nil {
				roleState.Target = max(roleState.Target, roleState.NewSpec)
			}
		}
		state[i] = roleState
	}

	return state
}

func plannerState(state rolloutState) (initialOld, currentOld, currentNew, targetNew RoleReplicaState) {
	initialOld = make(RoleReplicaState, len(state))
	currentOld = make(RoleReplicaState, len(state))
	currentNew = make(RoleReplicaState, len(state))
	targetNew = make(RoleReplicaState, len(state))
	for i, role := range state {
		initialOld[i] = role.InitialOld
		currentOld[i] = role.OldSpec
		currentNew[i] = role.NewSpec
		targetNew[i] = role.Target
	}
	return
}

// getTargetReplicas resolves the desired replica count. External roles read
// spec.replicas from the scaler (always materialised since the CRD defaults it
// to 0 and the controller seeds it at creation to avoid draining a running
// Static→External flip).
func getTargetReplicas(ds *disaggregatedsetv1.DisaggregatedSet, roleName string, scalers map[string]*disaggregatedsetv1.DisaggregatedSetRoleScaler, currentNewSpec int) int {
	for _, p := range ds.Spec.Roles {
		if p.Name != roleName {
			continue
		}
		if p.Scaling != nil && p.Scaling.Mode == disaggregatedsetv1.RoleScalingExternal {
			if s := scalers[roleName]; s != nil {
				return int(s.Spec.Replicas)
			}
			return currentNewSpec
		}
		if p.Spec.Replicas == nil {
			return 1
		}
		return int(*p.Spec.Replicas)
	}
	return 1
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
	scalers map[string]*disaggregatedsetv1.DisaggregatedSetRoleScaler,
) []RollingUpdateConfig {
	config := make([]RollingUpdateConfig, len(allRoleNames))
	roleIndex := make(map[string]int, len(allRoleNames))
	for i, name := range allRoleNames {
		config[i].MaxSurge = 1
		roleIndex[name] = i
	}

	for _, role := range ds.Spec.Roles {
		if rc := role.Spec.RolloutStrategy.RollingUpdateConfiguration; rc != nil {
			// For External roles this returns the scaler value (or currentNewSpec=0
			// if none is available); percentages against 0 collapse to 0, which
			// matches how a paused rollout should behave.
			replicas := getTargetReplicas(ds, role.Name, scalers, 0)
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

func isRolloutReady(state rolloutState) bool {
	for _, role := range state {
		if role.OldSpec != 0 || role.NewReady < role.Target {
			return false
		}
	}
	return true
}

// boundNewReplicaTargets applies the executor's hard limits to a planner
// proposal. A role may only consume surge headroom that exists in the current
// Spec footprint, and may only have its pending allowance issued-but-not-ready
// replicas. Existing Spec is never reduced here, even if an externally
// modified object is already outside either bound.
func boundNewReplicaTargets(
	state rolloutState,
	proposed RoleReplicaState,
) RoleReplicaState {
	provisional := make(RoleReplicaState, len(state))
	budgetSteps := 0
	for _, role := range state {
		budgetSteps = max(budgetSteps, role.InitialOld, role.Target)
	}
	for i, roleState := range state {
		roleSize := max(roleState.InitialOld, roleState.Target)
		maxBySurge := roleSize + roleState.Config.MaxSurge - roleState.OldSpec
		pendingAllowance := projectBudget(
			roleSize,
			roleState.Config.MaxSurge+roleState.Config.MaxUnavailable,
			budgetSteps,
		)
		maxByPending := roleState.NewReady + pendingAllowance
		upperBound := max(roleState.NewSpec, min(maxBySurge, maxByPending))
		provisional[i] = max(roleState.NewSpec, min(proposed[i], upperBound))
	}

	// Per-role readiness clamps can trim different parts of the proposal. Keep
	// the resulting progress within one largestReplicaFraction of the slowest
	// role. Integer cross-products keep this exact without rational-number state.
	slowCount, slowTarget, minTarget := 0, 0, 0
	for i, role := range state {
		target := role.Target
		if target <= 0 {
			continue
		}
		if minTarget == 0 || target < minTarget {
			minTarget = target
		}
		if slowTarget == 0 || int64(provisional[i])*int64(slowTarget) < int64(slowCount)*int64(target) {
			slowCount, slowTarget = provisional[i], target
		}
	}
	bounded := make(RoleReplicaState, len(state))
	for i, roleState := range state {
		coordinatedTarget := roleState.Target
		if minTarget > 0 {
			coordinatedTarget = replicaLimit(roleState.Target, slowCount, slowTarget, minTarget)
		}
		bounded[i] = max(roleState.NewSpec, min(provisional[i], coordinatedTarget))
	}
	return bounded
}

// replicaLimit returns floor(target * (slowCount/slowTarget + 1/minTarget)).
// Each multiplication is bounded by two replica counts, which originate from
// int32 API fields and therefore fit in int64.
func replicaLimit(target, slowCount, slowTarget, minTarget int) int {
	base := int64(target) * int64(slowCount)
	whole, remainder := base/int64(slowTarget), base%int64(slowTarget)
	extra := (remainder*int64(minTarget) + int64(target)*int64(slowTarget)) /
		(int64(slowTarget) * int64(minTarget))
	return min(target, int(whole+extra))
}

// maxSafeDrain returns the number of old replicas that may be removed without
// crossing the raw MaxUnavailable availability floor. Ready is capped at Spec
// before entering state, so terminating replicas cannot be spent twice.
func maxSafeDrain(state roleRolloutState) int {
	floor := max(0, min(state.InitialOld, state.Target)-state.Config.MaxUnavailable)
	return min(state.OldSpec, max(0, state.OldReady+state.NewReady-floor))
}

// The planner may pair a drain with growth that readiness/coordination bounds
// later remove. If that leaves a permanent no-op, spend one safe drain to open
// replacement capacity.
func ensureExecutableStep(state rolloutState, step *UpdateStep) {
	for i, role := range state {
		if step.New[i] > role.NewSpec || min(max(0, role.OldSpec-step.Past[i]), maxSafeDrain(role)) > 0 {
			return
		}
	}
	for i, role := range state {
		if role.OldSpec > 0 && maxSafeDrain(role) > 0 {
			step.Past[i] = role.OldSpec - 1
			return
		}
	}
}

func maxTimestamp(wl disaggregatedsetutils.RevisionRoles) time.Time {
	var maxTS time.Time
	for _, lws := range wl.Roles {
		if lws.CreationTimestamp.Time.After(maxTS) {
			maxTS = lws.CreationTimestamp.Time
		}
	}
	return maxTS
}

func sortByNewestTimestamp(revisions disaggregatedsetutils.RevisionRolesList) disaggregatedsetutils.RevisionRolesList {
	sorted := slices.Clone(revisions)
	slices.SortFunc(sorted, func(a, b disaggregatedsetutils.RevisionRoles) int {
		return maxTimestamp(b).Compare(maxTimestamp(a))
	})
	return sorted
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
	state rolloutState,
	targetOld RoleReplicaState,
	allowUncoordinatedDrain bool,
) error {
	budget := make(RoleReplicaState, len(roleNames))
	for i := range roleNames {
		roleState := state[i]
		budget[i] = max(0, min(roleState.OldSpec-targetOld[i], maxSafeDrain(roleState)))
	}

	log := logf.FromContext(ctx)
	for _, wl := range sortByNewestTimestamp(oldRevisions) {
		plannedDrain := make(RoleReplicaState, len(roleNames))
		for i, name := range roleNames {
			if lws := wl.Roles[name]; lws != nil {
				plannedDrain[i] = min(budget[i], int(getLWSReplicas(lws)))
			}
		}
		if !anyPositive(plannedDrain) {
			continue
		}

		coordinateRevisionDrain(roleNames, wl.Roles, plannedDrain, state, allowUncoordinatedDrain)

		for i, name := range roleNames {
			lws := wl.Roles[name]
			if lws == nil || plannedDrain[i] == 0 {
				continue
			}
			replicas := int(getLWSReplicas(lws))
			drain := plannedDrain[i]
			newReplicas := replicas - drain
			// Address by the LWS's actual name so a legacy slice-0 object drains too.
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

// coordinateRevisionDrain keeps all roles in an old revision alive together
// when possible. It may retire the whole revision if every role fits within
// its availability budget. If strict coordination would make a legal rollout
// immobile, the already-budgeted drain is allowed as a last resort.
func coordinateRevisionDrain(
	roleNames []string,
	roles map[string]*leaderworkersetv1.LeaderWorkerSet,
	drain RoleReplicaState,
	state rolloutState,
	allowUncoordinated bool,
) {
	anyAliveAfter, anyRetired, canRetire := false, false, true
	for i, name := range roleNames {
		lws := roles[name]
		if lws == nil || getLWSReplicas(lws) == 0 {
			continue
		}
		replicas := int(getLWSReplicas(lws))
		anyAliveAfter = anyAliveAfter || replicas > drain[i]
		anyRetired = anyRetired || drain[i] == replicas
		canRetire = canRetire && replicas <= maxSafeDrain(state[i])
	}
	if !anyAliveAfter || !anyRetired {
		return
	}
	if canRetire {
		for i, name := range roleNames {
			if lws := roles[name]; lws != nil {
				drain[i] = int(getLWSReplicas(lws))
			}
		}
		return
	}

	hasPartialDrain := false
	for i, name := range roleNames {
		if lws := roles[name]; lws != nil {
			replicas := int(getLWSReplicas(lws))
			hasPartialDrain = hasPartialDrain || drain[i] > 0 && drain[i] < replicas
		}
	}
	if !hasPartialDrain && allowUncoordinated {
		return
	}
	for i, name := range roleNames {
		if lws := roles[name]; lws != nil && drain[i] == int(getLWSReplicas(lws)) {
			drain[i] = 0
		}
	}
}

// --- LWS creation ---

func (executor *RollingUpdateExecutor) ensureNewLWSExists(
	ctx context.Context,
	ds *disaggregatedsetv1.DisaggregatedSet,
	slice int,
	revision, role string,
	config *disaggregatedsetv1.DisaggregatedRoleSpec,
	initialReplicas int,
) (bool, error) {
	lwsName := disaggregatedsetutils.GenerateName(ds.Name, slice, revision, role)
	existing, err := executor.LWSManager.Get(ctx, ds, lwsName)
	if err != nil {
		return false, fmt.Errorf("failed to get LWS %s: %w", lwsName, err)
	}
	if existing != nil {
		return false, nil
	}

	if err := executor.LWSManager.Create(ctx, disaggregatedsetutils.CreateParams{
		DisaggregatedSet: ds,
		Role:             role,
		Slice:            slice,
		Config:           config,
		Revision:         revision,
		Labels:           disaggregatedsetutils.GenerateLabels(ds.Name, slice, revision, role),
		Replicas:         initialReplicas,
	}); err != nil {
		return false, fmt.Errorf("failed to create LWS %s: %w", lwsName, err)
	}
	return true, nil
}

// --- Role change utils ---
func detectRoleChanges(specRoleNames []string, oldRevisions disaggregatedsetutils.RevisionRolesList) ([]string, []string) {
	specRoles, oldRoles := buildRoleSets(specRoleNames, oldRevisions)

	var added, removed []string
	for name := range oldRoles {
		if !specRoles[name] {
			removed = append(removed, name)
		}
	}
	for _, name := range specRoleNames {
		if !oldRoles[name] {
			added = append(added, name)
		}
	}
	return added, removed
}

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
	"math/big"
	"slices"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
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
	Client     client.Client
	Record     events.EventRecorder
	LWSManager *LeaderWorkerSetManager
}

// roleRolloutState separates work already issued through Spec from work that
// has completed and is serving through Ready. The planner advances Spec; Ready
// only authorizes additional in-flight work and old-replica drains.
type roleRolloutState struct {
	InitialOld       int
	OldSpec          int
	OldReady         int
	NewSpec          int
	NewReady         int
	Target           int
	PendingAllowance int
	Config           RollingUpdateConfig
}

type rolloutState map[string]roleRolloutState

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

	return executor.ReconcileRollingUpdate(ctx, disaggregatedSet, slice, oldRevisions, *newRevision, scalers)
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
	slice int,
	oldRevisions disaggregatedsetutils.RevisionRolesList,
	newRevision disaggregatedsetutils.RevisionRoles,
	scalers map[string]*disaggregatedsetv1.DisaggregatedSetRoleScaler,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	specRoleNames := disaggregatedsetutils.GetRoleNames(disaggregatedSet)
	specRoleSet, oldRoleSet := buildRoleSets(specRoleNames, oldRevisions)

	allRoleNames := append(slices.Clone(specRoleNames), removedRoleNames(oldRoleSet, specRoleSet)...)
	config := extractRollingUpdateConfigMap(disaggregatedSet, allRoleNames, scalers)
	state := buildRolloutState(disaggregatedSet, allRoleNames, specRoleSet, oldRevisions, newRevision, scalers, config)
	initialOld, currentOld, currentNewSpec, targetNew := buildPlannerStateMaps(allRoleNames, state)

	plan := ComputeNextStep(allRoleNames, initialOld, currentOld, currentNewSpec, targetNew, config)
	if plan.Status == PlanComplete {
		if !isRolloutReady(allRoleNames, state) {
			log.V(1).Info("Waiting for target revision to become ready")
			return ctrl.Result{RequeueAfter: time.Second}, nil
		}
		log.Info("Rolling update complete")
		executor.Record.Eventf(disaggregatedSet, nil, corev1.EventTypeNormal, EventReasonRollingUpdateCompleted,
			"Update", "Completed rolling update to revision %s", newRevision.Revision)
		return ctrl.Result{}, nil
	}
	if plan.Status == PlanBlocked {
		log.Info("Rolling update is temporarily blocked; waiting for state to change")
		return ctrl.Result{RequeueAfter: time.Second}, nil
	}
	nextStep := plan.Step
	nextStep.New = boundNewReplicaTargets(allRoleNames, state, nextStep.New)

	maxSafeDrain := buildMaxSafeDrain(allRoleNames, state)
	ensureExecutableStep(allRoleNames, state, nextStep, maxSafeDrain)
	log.Info("Next step computed", buildStepLogArgs(allRoleNames, nextStep)...)
	allowUncoordinatedDrain := true
	for _, name := range allRoleNames {
		if nextStep.New[name].Replicas > state[name].NewSpec {
			allowUncoordinatedDrain = false
			break
		}
	}

	// Scale down old replicas before scaling up new ones. This ordering ensures
	// the total replica count never exceeds the surge limit between the two
	// API calls: e.g. with surge=0, scaling up first would briefly make
	// (currentOld + nextStep.New) exceed the target before scaleDownOld brings
	// currentOld down.
	drained, err := executor.scaleDownOld(ctx, disaggregatedSet, oldRevisions, allRoleNames, currentOld, nextStep.Past, maxSafeDrain, allowUncoordinatedDrain)
	if err != nil {
		return ctrl.Result{}, err
	}
	scaled, err := executor.scaleUpNew(ctx, disaggregatedSet, slice, newRevision, allRoleNames, specRoleSet, state, nextStep.New)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !drained && !scaled {
		log.V(1).Info("Rolling update is waiting for capacity or readiness")
		return ctrl.Result{RequeueAfter: time.Second}, nil
	}

	return ctrl.Result{}, nil
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
	config map[string]RollingUpdateConfig,
) rolloutState {
	state := make(rolloutState, len(allRoleNames))

	for _, roleName := range allRoleNames {
		roleState := roleRolloutState{
			InitialOld: oldRevisions.GetTotalInitialReplicasPerRole(roleName),
			OldSpec:    oldRevisions.GetTotalReplicasPerRole(roleName),
			Config:     config[roleName],
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
		state[roleName] = roleState
	}

	initialOld := make(map[string]int, len(allRoleNames))
	targetNew := make(map[string]int, len(allRoleNames))
	for _, roleName := range allRoleNames {
		initialOld[roleName] = state[roleName].InitialOld
		targetNew[roleName] = state[roleName].Target
	}
	budgetSteps := max(sideSize(initialOld), sideSize(targetNew))
	for _, roleName := range allRoleNames {
		roleState := state[roleName]
		roleSize := max(roleState.InitialOld, roleState.Target)
		roleState.PendingAllowance = projectBudget(
			roleSize,
			roleState.Config.MaxSurge+roleState.Config.MaxUnavailable,
			budgetSteps,
		)
		state[roleName] = roleState
	}
	return state
}

func buildPlannerStateMaps(
	roleNames []string,
	state rolloutState,
) (initialOld, currentOld, currentNew, targetNew map[string]int) {
	initialOld = make(map[string]int, len(roleNames))
	currentOld = make(map[string]int, len(roleNames))
	currentNew = make(map[string]int, len(roleNames))
	targetNew = make(map[string]int, len(roleNames))
	for _, roleName := range roleNames {
		roleState := state[roleName]
		initialOld[roleName] = roleState.InitialOld
		currentOld[roleName] = roleState.OldSpec
		currentNew[roleName] = roleState.NewSpec
		targetNew[roleName] = roleState.Target
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

func extractRollingUpdateConfigMap(
	ds *disaggregatedsetv1.DisaggregatedSet,
	allRoleNames []string,
	scalers map[string]*disaggregatedsetv1.DisaggregatedSetRoleScaler,
) map[string]RollingUpdateConfig {
	config := make(map[string]RollingUpdateConfig, len(allRoleNames))
	for _, name := range allRoleNames {
		config[name] = RollingUpdateConfig{MaxSurge: 1, MaxUnavailable: 0}
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
			config[role.Name] = cfg
		}
	}
	return config
}

func buildStepLogArgs(roleNames []string, step *UpdateStep) []interface{} {
	args := make([]interface{}, 0, len(roleNames)*4)
	for _, name := range roleNames {
		args = append(args,
			"past_"+name, step.Past[name].Replicas,
			"new_"+name, step.New[name].Replicas,
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

func isRolloutReady(roleNames []string, state rolloutState) bool {
	for _, name := range roleNames {
		roleState := state[name]
		if roleState.OldSpec != 0 || roleState.NewReady < roleState.Target {
			return false
		}
	}
	return true
}

// boundNewReplicaTargets applies the executor's hard limits to a planner
// proposal. A role may only consume surge headroom that exists in the current
// Spec footprint, and may only have PendingAllowance issued-but-not-ready
// replicas. Existing Spec is never reduced here, even if an externally
// modified object is already outside either bound.
func boundNewReplicaTargets(
	roleNames []string,
	state rolloutState,
	proposed map[string]RoleStepState,
) map[string]RoleStepState {
	targets := make(map[string]int, len(roleNames))
	provisional := make(map[string]int, len(roleNames))
	for _, name := range roleNames {
		roleState := state[name]
		targets[name] = roleState.Target
		roleSize := max(roleState.InitialOld, roleState.Target)
		maxBySurge := roleSize + roleState.Config.MaxSurge - roleState.OldSpec
		maxByPending := roleState.NewReady + roleState.PendingAllowance
		upperBound := max(roleState.NewSpec, min(maxBySurge, maxByPending))
		provisional[name] = max(roleState.NewSpec, min(proposed[name].Replicas, upperBound))
	}

	// Independent per-role clamps can break coordination. For example, with a
	// 2:3 ratio, letting only the two-replica role move from 1 to 2 produces
	// progress of 100% versus 33%, beyond the largestReplicaFraction (1/2).
	// Let roles consume different amounts of their pending windows, but cap the
	// resulting progress spread at the largest one-pod fraction. This is less
	// restrictive than requiring an exact shared step: a one-replica role has
	// an atomic fraction of 1 and must not block a larger role entirely.
	var minProgress *big.Rat
	minPositiveTarget := 0
	for _, name := range roleNames {
		target := targets[name]
		if target <= 0 {
			continue
		}
		progress := new(big.Rat).SetFrac64(int64(provisional[name]), int64(target))
		if minProgress == nil || progress.Cmp(minProgress) < 0 {
			minProgress = progress
		}
		if minPositiveTarget == 0 || target < minPositiveTarget {
			minPositiveTarget = target
		}
	}
	maxCoordinatedProgress := new(big.Rat).SetInt64(1)
	if minPositiveTarget > 0 {
		maxCoordinatedProgress.Add(minProgress, new(big.Rat).SetFrac64(1, int64(minPositiveTarget)))
		if maxCoordinatedProgress.Cmp(new(big.Rat).SetInt64(1)) > 0 {
			maxCoordinatedProgress.SetInt64(1)
		}
	}
	bounded := make(map[string]RoleStepState, len(roleNames))
	for _, name := range roleNames {
		roleState := state[name]
		coordinatedTarget := floorScaledFraction(roleState.Target, maxCoordinatedProgress)
		bounded[name] = RoleStepState{
			Replicas: max(roleState.NewSpec, min(provisional[name], coordinatedTarget)),
		}
	}
	return bounded
}

func floorScaledFraction(scale int, fraction *big.Rat) int {
	if scale <= 0 {
		return 0
	}
	numerator := new(big.Int).Mul(big.NewInt(int64(scale)), fraction.Num())
	return int(new(big.Int).Quo(numerator, fraction.Denom()).Int64())
}

// buildMaxSafeDrain returns the maximum number of old replicas that may be
// removed per role without crossing the raw MaxUnavailable availability
// floor. Ready is capped at Spec before entering state so replicas already
// committed to termination cannot authorize another drain.
func buildMaxSafeDrain(
	roleNames []string,
	state rolloutState,
) map[string]int {
	maxSafeDrain := make(map[string]int, len(roleNames))
	for _, name := range roleNames {
		roleState := state[name]
		floor := max(0, min(roleState.InitialOld, roleState.Target)-roleState.Config.MaxUnavailable)
		maxSafeDrain[name] = min(roleState.OldSpec, max(0, roleState.OldReady+roleState.NewReady-floor))
	}
	return maxSafeDrain
}

// ensureExecutableStep recovers a floor-safe drain when pending/coordination
// bounds remove every scale-up proposed by the planner and its paired
// aliveness rule had suppressed all drains. Without this post-bound check,
// valid zero-surge states can become permanent no-ops. The executor still
// applies its per-revision aliveness policy before issuing the selected drain.
func ensureExecutableStep(
	roleNames []string,
	state rolloutState,
	step *UpdateStep,
	maxSafeDrain map[string]int,
) {
	for _, name := range roleNames {
		if step.New[name].Replicas > state[name].NewSpec ||
			min(max(0, state[name].OldSpec-step.Past[name].Replicas), maxSafeDrain[name]) > 0 {
			return
		}
	}
	for _, name := range roleNames {
		roleState := state[name]
		if roleState.OldSpec > 0 && maxSafeDrain[name] > 0 {
			step.Past[name] = RoleStepState{Replicas: roleState.OldSpec - 1}
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

func sortByNewestTimestamp(revisions disaggregatedsetutils.RevisionRolesList, roleNames []string) disaggregatedsetutils.RevisionRolesList {
	if len(roleNames) == 0 {
		return revisions
	}
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
	slice int,
	newRevision disaggregatedsetutils.RevisionRoles,
	allRoleNames []string,
	specRoleSet map[string]bool,
	state rolloutState,
	targetNew map[string]RoleStepState,
) (bool, error) {
	log := logf.FromContext(ctx)
	changed := false
	for _, name := range allRoleNames {
		if !specRoleSet[name] {
			continue
		}
		lws := newRevision.Roles[name]
		if lws == nil {
			continue
		}
		currentSpec := int(getLWSReplicas(lws))
		desiredSpec := targetNew[name].Replicas
		roleState := state[name]
		roleSize := max(roleState.InitialOld, roleState.Target)
		maxBySurge := roleSize + roleState.Config.MaxSurge - roleState.OldSpec
		maxByPending := roleState.NewReady + roleState.PendingAllowance
		desiredSpec = max(currentSpec, min(desiredSpec, maxBySurge, maxByPending))

		if currentSpec >= desiredSpec {
			continue
		}
		lwsName := disaggregatedsetutils.GenerateName(ds.Name, slice, newRevision.Revision, name)
		log.Info("Scaling up", "lws", lwsName, "from_spec", currentSpec, "from_ready", state[name].NewReady, "to", desiredSpec)
		if err := executor.LWSManager.Scale(ctx, ds, lwsName, desiredSpec); err != nil {
			return changed, fmt.Errorf("failed to scale %s: %w", lwsName, err)
		}
		executor.Record.Eventf(ds, nil, corev1.EventTypeNormal, EventReasonScalingUp,
			"Update", "Scaling up %s LWS %s from %d to %d replicas", name, lwsName, currentSpec, desiredSpec)
		changed = true
	}
	return changed, nil
}

func (executor *RollingUpdateExecutor) scaleDownOld(
	ctx context.Context,
	ds *disaggregatedsetv1.DisaggregatedSet,
	oldRevisions disaggregatedsetutils.RevisionRolesList,
	roleNames []string,
	currentOld map[string]int,
	targetOld map[string]RoleStepState,
	maxSafeDrain map[string]int,
	allowUncoordinatedDrain bool,
) (bool, error) {
	budget := make(map[string]int, len(roleNames))
	for _, name := range roleNames {
		budget[name] = max(0, min(currentOld[name]-targetOld[name].Replicas, maxSafeDrain[name]))
	}

	log := logf.FromContext(ctx)
	changed := false
	for _, wl := range sortByNewestTimestamp(oldRevisions, roleNames) {
		allDone := true
		for _, name := range roleNames {
			if budget[name] > 0 {
				allDone = false
				break
			}
		}
		if allDone {
			break
		}

		// A revision is retired when all of its role Specs are zero. Status may
		// still contain terminating replicas, but they are already committed to
		// disappear and cannot block progress or provide drain capacity.
		specAllZero := true
		for _, name := range roleNames {
			lws, exists := wl.Roles[name]
			if !exists {
				continue
			}
			if getLWSReplicas(lws) > 0 {
				specAllZero = false
			}
		}
		if specAllZero {
			continue
		}

		budgetedDrain := make(map[string]int, len(roleNames))
		plannedDrain := make(map[string]int, len(roleNames))
		for _, name := range roleNames {
			lws, exists := wl.Roles[name]
			if !exists {
				continue
			}
			replicas := int(getLWSReplicas(lws))
			budgetedDrain[name] = min(budget[name], replicas)
			plannedDrain[name] = budgetedDrain[name]
		}

		// Per-revision aliveness: if the planned drain would take some role
		// to 0 while another role stays > 0, choose between:
		//   - Retire the whole revision atomically when ReadyReplicas leave
		//     enough room above every role's raw availability floor.
		//   - Skip drains that would hit 0 (keep those roles alive; budget
		//     rolls to the next revision or next reconcile). Preserves
		//     ratio and avoids over-draining ready pods below floor.
		anyAliveAfter, wouldOrphan, canRetireSafely := false, false, true
		for _, name := range roleNames {
			lws, exists := wl.Roles[name]
			if !exists {
				continue
			}
			replicas := int(getLWSReplicas(lws))
			if replicas == 0 {
				continue
			}
			after := replicas - plannedDrain[name]
			if after > 0 {
				anyAliveAfter = true
			}
			if replicas > maxSafeDrain[name] {
				canRetireSafely = false
			}
		}
		for _, name := range roleNames {
			lws, exists := wl.Roles[name]
			if !exists {
				continue
			}
			replicas := int(getLWSReplicas(lws))
			if replicas == 0 {
				continue
			}
			after := replicas - plannedDrain[name]
			if after == 0 && anyAliveAfter {
				wouldOrphan = true
				break
			}
		}
		if wouldOrphan {
			if canRetireSafely {
				for _, name := range roleNames {
					lws, exists := wl.Roles[name]
					if !exists {
						continue
					}
					plannedDrain[name] = int(getLWSReplicas(lws))
				}
			} else {
				for _, name := range roleNames {
					lws, exists := wl.Roles[name]
					if !exists {
						continue
					}
					replicas := int(getLWSReplicas(lws))
					if replicas > 0 && replicas-plannedDrain[name] == 0 {
						plannedDrain[name] = 0
					}
				}
			}
		}

		// Aliveness is a coordination preference, while the raw availability
		// floor is the safety boundary. If orphan prevention suppressed every
		// otherwise budgeted drain and whole-revision retirement is unsafe, wait
		// when NEW can grow. If no growth is possible, use only the original
		// budgeted move rather than force-retiring the revision or wedging forever.
		if !hasPositiveDrain(plannedDrain) && hasPositiveDrain(budgetedDrain) {
			if !allowUncoordinatedDrain {
				// Do not spill the budget into an older revision. Scaling NEW in
				// this reconcile may make whole-revision retirement safe on the
				// next pass.
				return changed, nil
			}
			log.V(1).Info("Applying budgeted drain to avoid an aliveness deadlock",
				"revision", wl.Revision)
			plannedDrain = budgetedDrain
		}

		revisionDrained := false
		for _, name := range roleNames {
			lws, exists := wl.Roles[name]
			if !exists {
				continue
			}
			replicas := int(getLWSReplicas(lws))
			drain := plannedDrain[name]
			if drain <= 0 {
				continue
			}
			newReplicas := replicas - drain
			// Address by the LWS's actual name so a legacy slice-0 object drains too.
			lwsName := lws.Name
			log.Info("Scaling down", "lws", lwsName, "from", replicas, "to", newReplicas)
			if err := executor.LWSManager.Scale(ctx, ds, lwsName, newReplicas); err != nil {
				return changed, fmt.Errorf("failed to scale %s: %w", lwsName, err)
			}
			executor.Record.Eventf(ds, nil, corev1.EventTypeNormal, EventReasonScalingDown,
				"Update", "Scaling down %s LWS %s from %d to %d replicas", name, lwsName, replicas, newReplicas)
			budget[name] -= drain
			revisionDrained = true
			changed = true
		}
		// Newest-first drain-order invariant: once we've touched a revision,
		// finish this reconcile without moving budget to older revisions.
		// Next reconcile picks up the next revision if this one is done.
		// Cost: extra reconciles for multi-revision cases; benefit: the
		// observable "B drained to 0 before A" property holds even on fast
		// clusters where reconciles chain in <100ms.
		if revisionDrained {
			break
		}
	}

	return changed, nil
}

func hasPositiveDrain(drains map[string]int) bool {
	for _, drain := range drains {
		if drain > 0 {
			return true
		}
	}
	return false
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

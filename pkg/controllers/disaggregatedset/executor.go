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
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	disaggregatedsetv1 "sigs.k8s.io/lws/api/disaggregatedset/v1"
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

// ReconcileRollingUpdateNew is the entry point for rolling update reconciliation.
// It fetches current cluster state and either:
//  1. Starts a new rolling update (initRollingUpdate) if no LWS for the target
//     revision exist yet, or
//  2. Continues an in-progress rolling update (ReconcileRollingUpdate) by
//     computing and executing the next scale step.
func (executor *RollingUpdateExecutor) ReconcileRollingUpdateNew(
	ctx context.Context,
	disaggregatedSet *disaggregatedsetv1.DisaggregatedSet,
	revision string,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	roleNames := disaggregatedsetutils.GetRoleNames(disaggregatedSet)
	roleConfigs := disaggregatedsetutils.GetRoleConfigs(disaggregatedSet)

	oldRevisions, newRevision, err := executor.LWSManager.GetRevisionRolesList(ctx, disaggregatedSet.Namespace, disaggregatedSet.Name, revision)
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
		return executor.initRollingUpdate(ctx, disaggregatedSet, revision, roleNames, roleConfigs, oldRevisions)
	}

	return executor.ReconcileRollingUpdate(ctx, disaggregatedSet, oldRevisions, *newRevision)
}

func (executor *RollingUpdateExecutor) initRollingUpdate(
	ctx context.Context,
	disaggregatedSet *disaggregatedsetv1.DisaggregatedSet,
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
		for roleName, roleLWS := range oldGrouped.Roles {
			lwsName := disaggregatedsetutils.GenerateName(disaggregatedSet.Name, roleName, oldGrouped.Revision)
			replicas := 1
			if roleLWS.Spec.Replicas != nil {
				replicas = int(*roleLWS.Spec.Replicas)
			}
			if _, err := executor.LWSManager.SetInitialReplicas(ctx, disaggregatedSet.Namespace, lwsName, replicas); err != nil {
				log.Error(err, "Failed to set initial-replicas annotation", "lws", lwsName)
			}
		}
	}

	// Create new LWS objects (one per role) for the target revision with 0
	// replicas. The next reconcile loop will start scaling them up.
	for _, roleName := range roleNames {
		if _, err := executor.ensureNewLWSExists(ctx, disaggregatedSet, revision, roleName, roleConfigs[roleName], 0); err != nil {
			return ctrl.Result{}, err
		}
	}

	return ctrl.Result{RequeueAfter: time.Second}, nil
}

// ReconcileRollingUpdate executes one step of an in-progress rolling update:
//  1. Wait for the new revision to stabilize (all roles have ReadyReplicas == Replicas).
//  2. Compute the next scale step using the planner.
//  3. Scale up new revision LWS objects.
//  4. Scale down old revision LWS objects (newest-first, with coordinated drain).
func (executor *RollingUpdateExecutor) ReconcileRollingUpdate(
	ctx context.Context,
	disaggregatedSet *disaggregatedsetv1.DisaggregatedSet,
	oldRevisions disaggregatedsetutils.RevisionRolesList,
	newRevision disaggregatedsetutils.RevisionRoles,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	specRoleNames := disaggregatedsetutils.GetRoleNames(disaggregatedSet)
	specRoleSet, oldRoleSet := buildRoleSets(specRoleNames, oldRevisions)

	allRoleNames := append(slices.Clone(specRoleNames), removedRoleNames(oldRoleSet, specRoleSet)...)
	config := extractRollingUpdateConfigMap(disaggregatedSet, allRoleNames)

	// A revision is "stable enough" to advance when each role has at most
	// (MaxSurge + MaxUnavailable) pending replicas — the full in-flight slack
	// the user authorized. This lets the rollout keep progressing when one
	// slow-starting pod would otherwise gate everything. See isRevisionStable.
	if !isRevisionStable(newRevision, specRoleNames, config) {
		log.V(1).Info("Waiting for new revision to stabilize")
		return ctrl.Result{RequeueAfter: time.Second}, nil
	}

	initialOld, currentOld, currentNew, targetNew := buildPlannerStateMaps(disaggregatedSet, allRoleNames, specRoleSet, oldRevisions, newRevision)

	nextStep := ComputeNextStep(allRoleNames, initialOld, currentOld, currentNew, targetNew, config)
	if nextStep == nil {
		log.Info("Rolling update complete")
		executor.Record.Eventf(disaggregatedSet, nil, corev1.EventTypeNormal, EventReasonRollingUpdateCompleted,
			"Update", "Completed rolling update to revision %s", newRevision.Revision)
		return ctrl.Result{}, nil
	}

	log.Info("Next step computed", buildStepLogArgs(allRoleNames, nextStep)...)

	// Scale down old replicas before scaling up new ones. This ordering ensures
	// the total replica count never exceeds the surge limit between the two
	// API calls: e.g. with surge=0, scaling up first would briefly make
	// (currentOld + nextStep.New) exceed the target before scaleDownOld brings
	// currentOld down.
	if err := executor.scaleDownOld(ctx, disaggregatedSet, oldRevisions, allRoleNames, currentOld, nextStep.Past); err != nil {
		return ctrl.Result{}, err
	}
	if err := executor.scaleUpNew(ctx, disaggregatedSet, newRevision, allRoleNames, specRoleSet, currentNew, nextStep.New, initialOld, targetNew, config); err != nil {
		return ctrl.Result{}, err
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
	return removed
}

func buildPlannerStateMaps(
	ds *disaggregatedsetv1.DisaggregatedSet,
	allRoleNames []string,
	specRoleSet map[string]bool,
	oldRevisions disaggregatedsetutils.RevisionRolesList,
	newRevision disaggregatedsetutils.RevisionRoles,
) (initialOld, currentOld, currentNew, targetNew map[string]int) {
	initialOld = make(map[string]int, len(allRoleNames))
	currentOld = make(map[string]int, len(allRoleNames))
	currentNew = make(map[string]int, len(allRoleNames))
	targetNew = make(map[string]int, len(allRoleNames))

	for _, roleName := range allRoleNames {
		initialOld[roleName] = oldRevisions.GetTotalInitialReplicasPerRole(roleName)
		currentOld[roleName] = oldRevisions.GetTotalReplicasPerRole(roleName)

		if specRoleSet[roleName] {
			if lws := newRevision.Roles[roleName]; lws != nil {
				// Use ReadyReplicas (not Spec) so the planner advances on actual
				// serving capacity. Slow-starting pods don't inflate currentNew,
				// so the planner won't compound pending replicas. The
				// spec-footprint guard in scaleUpNew prevents spec from
				// growing past the surge ceiling.
				currentNew[roleName] = int(lws.Status.ReadyReplicas)
			}
			targetNew[roleName] = getTargetReplicas(ds, roleName)
		}
	}
	return
}

func getTargetReplicas(ds *disaggregatedsetv1.DisaggregatedSet, roleName string) int {
	for _, p := range ds.Spec.Roles {
		if p.Name == roleName {
			if p.Spec.Replicas == nil {
				return 1
			}
			return int(*p.Spec.Replicas)
		}
	}
	return 1
}

func extractRollingUpdateConfigMap(ds *disaggregatedsetv1.DisaggregatedSet, allRoleNames []string) map[string]RollingUpdateConfig {
	config := make(map[string]RollingUpdateConfig, len(allRoleNames))
	for _, name := range allRoleNames {
		config[name] = RollingUpdateConfig{MaxSurge: 1, MaxUnavailable: 0}
	}

	for _, role := range ds.Spec.Roles {
		if rc := role.Spec.RolloutStrategy.RollingUpdateConfiguration; rc != nil {
			replicas := getTargetReplicas(ds, role.Name)
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

// isRevisionStable returns true when the new revision is "stable enough" to
// advance the rollout: each role's pending count (Spec.Replicas - ReadyReplicas)
// must be within its per-role slack budget, which is the sum of the surge and
// unavail budgets projected onto this role's size. See planner package doc for
// the minUnit-multiplier semantics.
func isRevisionStable(rev disaggregatedsetutils.RevisionRoles, roleNames []string, config map[string]RollingUpdateConfig) bool {
	// totalSteps for the projection = max role size across the new revision.
	roleSizes := make(map[string]int, len(roleNames))
	for _, name := range roleNames {
		if lws := rev.Roles[name]; lws != nil {
			roleSizes[name] = int(getLWSReplicas(lws))
		}
	}
	totalSteps := maxRoleSize(roleSizes)

	for _, name := range roleNames {
		lws := rev.Roles[name]
		if lws == nil {
			return false
		}
		spec := int32(getLWSReplicas(lws))
		ready := lws.Status.ReadyReplicas
		pending := spec - ready
		if pending < 0 {
			pending = 0
		}
		var tolerance int32
		if cfg, ok := config[name]; ok {
			slack := projectBudget(int(spec), cfg.MaxSurge+cfg.MaxUnavailable, totalSteps)
			tolerance = int32(slack)
		}
		if pending > tolerance {
			return false
		}
	}
	return true
}

// maxRoleSize returns the largest replica count across roles (used as
// totalSteps for minUnit projections). Returns 0 if the map is empty.
func maxRoleSize(sizes map[string]int) int {
	maxN := 0
	for _, n := range sizes {
		if n > maxN {
			maxN = n
		}
	}
	return maxN
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
	newRevision disaggregatedsetutils.RevisionRoles,
	allRoleNames []string,
	specRoleSet map[string]bool,
	currentNew map[string]int,
	targetNew map[string]RoleStepState,
	initialOld, targetSpec map[string]int,
	config map[string]RollingUpdateConfig,
) error {
	log := logf.FromContext(ctx)
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

		// Spec-footprint guard: cap desired spec at the per-role surge ceiling.
		// MaxSurge is a minUnit multiplier at the side level; project onto
		// this role's pod count so smaller roles get proportionally less
		// absolute slack. See planner package doc.
		totalSteps := maxRoleSize(targetSpec)
		roleSize := max(initialOld[name], targetSpec[name])
		surgePods := projectBudget(roleSize, config[name].MaxSurge, totalSteps)
		ceiling := roleSize + surgePods
		if desiredSpec > ceiling {
			desiredSpec = ceiling
		}

		if currentSpec >= desiredSpec {
			continue
		}
		lwsName := disaggregatedsetutils.GenerateName(ds.Name, name, newRevision.Revision)
		log.Info("Scaling up", "lws", lwsName, "from_spec", currentSpec, "from_ready", currentNew[name], "to", desiredSpec)
		if err := executor.LWSManager.Scale(ctx, ds.Namespace, lwsName, desiredSpec); err != nil {
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
	currentOld map[string]int,
	targetOld map[string]RoleStepState,
) error {
	budget := make(map[string]int, len(roleNames))
	for _, name := range roleNames {
		budget[name] = currentOld[name] - targetOld[name].Replicas
	}

	log := logf.FromContext(ctx)
	drainedAny := false
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

		plannedDrain := make(map[string]int, len(roleNames))
		for _, name := range roleNames {
			lws, exists := wl.Roles[name]
			if !exists {
				continue
			}
			replicas := int(getLWSReplicas(lws))
			plannedDrain[name] = min(budget[name], replicas)
		}

		// Per-revision aliveness: if the planned drain would take some role
		// to 0 while another role stays > 0, choose between:
		//   - Retire the whole revision atomically (when budget allows for
		//     every role — planner clearly wants retirement).
		//   - Skip drains that would hit 0 (keep those roles alive; budget
		//     rolls to the next revision or next reconcile). Preserves
		//     ratio and avoids over-draining ready pods below floor.
		anyAliveAfter, wouldOrphan, canRetire := false, false, true
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
			if replicas > budget[name] {
				canRetire = false
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
			if canRetire {
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
			lwsName := disaggregatedsetutils.GenerateName(ds.Name, name, wl.Revision)
			log.Info("Scaling down", "lws", lwsName, "from", replicas, "to", newReplicas)
			if err := executor.LWSManager.Scale(ctx, ds.Namespace, lwsName, newReplicas); err != nil {
				return fmt.Errorf("failed to scale %s: %w", lwsName, err)
			}
			executor.Record.Eventf(ds, nil, corev1.EventTypeNormal, EventReasonScalingDown,
				"Update", "Scaling down %s LWS %s from %d to %d replicas", name, lwsName, replicas, newReplicas)
			budget[name] -= drain
			drainedAny = true
		}
	}

	// Deadlock fallback: aliveness may have skipped every revision (no drain
	// applied) even though planner still has budget to spend. This happens
	// when no revision can safely partial-drain AND no revision can be
	// fully retired within budget. Force-retire the newest non-empty revision
	// to unstick; the aliveness invariant is briefly broken, but progress
	// resumes next reconcile.
	if !drainedAny {
		remaining := false
		for _, name := range roleNames {
			if budget[name] > 0 {
				remaining = true
				break
			}
		}
		if remaining {
			for _, wl := range sortByNewestTimestamp(oldRevisions, roleNames) {
				anyReplicas := false
				for _, name := range roleNames {
					if lws, ok := wl.Roles[name]; ok && getLWSReplicas(lws) > 0 {
						anyReplicas = true
						break
					}
				}
				if !anyReplicas {
					continue
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
					lwsName := disaggregatedsetutils.GenerateName(ds.Name, name, wl.Revision)
					log.Info("Scaling down (deadlock fallback: full retire)", "lws", lwsName, "from", replicas)
					if err := executor.LWSManager.Scale(ctx, ds.Namespace, lwsName, 0); err != nil {
						return fmt.Errorf("failed to scale %s: %w", lwsName, err)
					}
					executor.Record.Eventf(ds, nil, corev1.EventTypeNormal, EventReasonScalingDown,
						"Update", "Scaling down %s LWS %s from %d to 0 replicas (deadlock fallback)", name, lwsName, replicas)
				}
				break
			}
		}
	}
	return nil
}

// --- LWS creation ---

func (executor *RollingUpdateExecutor) ensureNewLWSExists(
	ctx context.Context,
	ds *disaggregatedsetv1.DisaggregatedSet,
	revision, role string,
	config *disaggregatedsetv1.DisaggregatedRoleSpec,
	initialReplicas int,
) (bool, error) {
	lwsName := disaggregatedsetutils.GenerateName(ds.Name, role, revision)
	existing, err := executor.LWSManager.Get(ctx, ds.Namespace, lwsName)
	if err != nil {
		return false, fmt.Errorf("failed to get LWS %s: %w", lwsName, err)
	}
	if existing != nil {
		return false, nil
	}

	if err := executor.LWSManager.Create(ctx, disaggregatedsetutils.CreateParams{
		DisaggregatedSet: ds,
		Role:             role,
		Config:           config,
		Revision:         revision,
		Labels:           disaggregatedsetutils.GenerateLabels(ds.Name, role, revision),
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

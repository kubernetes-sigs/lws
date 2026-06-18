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
	slice int,
	revision string,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	roleNames := disaggregatedsetutils.GetRoleNames(disaggregatedSet)
	roleConfigs := disaggregatedsetutils.GetRoleConfigs(disaggregatedSet)

	oldRevisions, newRevision, err := executor.LWSManager.GetRevisionRolesList(ctx, disaggregatedSet.Namespace, disaggregatedSet.Name, slice, revision)
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

	return executor.ReconcileRollingUpdate(ctx, disaggregatedSet, slice, oldRevisions, *newRevision)
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

// ReconcileRollingUpdate executes one step of an in-progress rolling update:
//  1. Wait for the new revision to stabilize (all roles have ReadyReplicas == Replicas).
//  2. Compute the next scale step using the planner.
//  3. Scale up new revision LWS objects.
//  4. Scale down old revision LWS objects (newest-first, with coordinated drain).
func (executor *RollingUpdateExecutor) ReconcileRollingUpdate(
	ctx context.Context,
	disaggregatedSet *disaggregatedsetv1.DisaggregatedSet,
	slice int,
	oldRevisions disaggregatedsetutils.RevisionRolesList,
	newRevision disaggregatedsetutils.RevisionRoles,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	specRoleNames := disaggregatedsetutils.GetRoleNames(disaggregatedSet)
	specRoleSet, oldRoleSet := buildRoleSets(specRoleNames, oldRevisions)

	allRoleNames := append(slices.Clone(specRoleNames), removedRoles(oldRoleSet, specRoleSet)...)

	// A revision is "stable" when every role's LWS has ReadyReplicas == Replicas,
	// meaning all pods from the previous scale step are Running and Ready.
	// We wait for stability before computing the next step to avoid over-scaling.
	if !isRevisionStable(newRevision, specRoleNames) {
		log.V(1).Info("Waiting for new revision to stabilize")
		return ctrl.Result{RequeueAfter: time.Second}, nil
	}

	initialOld, currentOld, currentNew, targetNew := buildPlannerState(disaggregatedSet, allRoleNames, specRoleSet, oldRevisions, newRevision)
	config := extractRollingUpdateConfig(disaggregatedSet, allRoleNames)

	nextStep := ComputeNextStep(initialOld, currentOld, currentNew, targetNew, config)
	if nextStep == nil {
		log.Info("Rolling update complete")
		executor.Record.Eventf(disaggregatedSet, nil, corev1.EventTypeNormal, EventReasonRollingUpdateCompleted,
			"Update", "Completed rolling update to revision %s", newRevision.Revision)
		return ctrl.Result{}, nil
	}

	log.Info("Next step computed", buildStepLogArgs(allRoleNames, nextStep)...)

	if err := executor.scaleUpNew(ctx, disaggregatedSet, slice, newRevision, allRoleNames, specRoleSet, currentNew, nextStep.New); err != nil {
		return ctrl.Result{}, err
	}
	if err := executor.scaleDownOld(ctx, disaggregatedSet, oldRevisions, allRoleNames, currentOld, nextStep.Past); err != nil {
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

func removedRoles(oldRoleSet, specRoleSet map[string]bool) []string {
	var removed []string
	for role := range oldRoleSet {
		if !specRoleSet[role] {
			removed = append(removed, role)
		}
	}
	return removed
}

func buildPlannerState(
	ds *disaggregatedsetv1.DisaggregatedSet,
	allRoleNames []string,
	specRoleSet map[string]bool,
	oldRevisions disaggregatedsetutils.RevisionRolesList,
	newRevision disaggregatedsetutils.RevisionRoles,
) (initialOld, currentOld, currentNew, targetNew RoleReplicaState) {
	n := len(allRoleNames)
	initialOld, currentOld, currentNew, targetNew = make(RoleReplicaState, n), make(RoleReplicaState, n), make(RoleReplicaState, n), make(RoleReplicaState, n)

	for i, roleName := range allRoleNames {
		initialOld[i] = oldRevisions.GetTotalInitialReplicasPerRole(roleName)
		currentOld[i] = oldRevisions.GetTotalReplicasPerRole(roleName)

		if specRoleSet[roleName] {
			if lws := newRevision.Roles[roleName]; lws != nil {
				currentNew[i] = int(getLWSReplicas(lws))
			}
			targetNew[i] = getTargetReplicas(ds, roleName)
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

func extractRollingUpdateConfig(ds *disaggregatedsetv1.DisaggregatedSet, allRoleNames []string) []RollingUpdateConfig {
	config := DefaultRollingUpdateConfig(len(allRoleNames))

	roleIndex := make(map[string]int, len(allRoleNames))
	for i, name := range allRoleNames {
		roleIndex[name] = i
	}

	for _, role := range ds.Spec.Roles {
		if rc := role.Spec.RolloutStrategy.RollingUpdateConfiguration; rc != nil {
			i := roleIndex[role.Name]
			replicas := getTargetReplicas(ds, role.Name)
			// Use GetScaledValueFromIntOrPercent to handle both integers and percentages.
			// For maxSurge, round up (true); for maxUnavailable, round down (false).
			surge, _ := intstr.GetScaledValueFromIntOrPercent(&rc.MaxSurge, replicas, true)
			unavail, _ := intstr.GetScaledValueFromIntOrPercent(&rc.MaxUnavailable, replicas, false)
			if unavail > 0 {
				config[i].MaxUnavailable = unavail
				config[i].MaxSurge = surge
			} else if surge > 0 {
				config[i].MaxSurge = surge
			}
		}
	}
	return config
}

func buildStepLogArgs(roleNames []string, step *UpdateStep) []interface{} {
	args := make([]interface{}, 0, len(roleNames)*4)
	for i, name := range roleNames {
		args = append(args, "past"+name, step.Past[i], "new"+name, step.New[i])
	}
	return args
}

func isRevisionStable(rev disaggregatedsetutils.RevisionRoles, roleNames []string) bool {
	for _, name := range roleNames {
		lws := rev.Roles[name]
		if lws == nil {
			return false
		}
		if getLWSReplicas(lws) != lws.Status.ReadyReplicas {
			return false
		}
	}
	return true
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
	current, target RoleReplicaState,
) error {
	log := logf.FromContext(ctx)
	for i, name := range allRoleNames {
		if !specRoleSet[name] || current[i] >= target[i] {
			continue
		}
		lwsName := disaggregatedsetutils.GenerateName(ds.Name, slice, newRevision.Revision, name)
		log.Info("Scaling up", "lws", lwsName, "from", current[i], "to", target[i])
		if err := executor.LWSManager.Scale(ctx, ds.Namespace, lwsName, target[i]); err != nil {
			return fmt.Errorf("failed to scale %s: %w", lwsName, err)
		}
		executor.Record.Eventf(ds, nil, corev1.EventTypeNormal, EventReasonScalingUp,
			"Update", "Scaling up %s LWS %s from %d to %d replicas", name, lwsName, current[i], target[i])
	}
	return nil
}

func (executor *RollingUpdateExecutor) scaleDownOld(
	ctx context.Context,
	ds *disaggregatedsetv1.DisaggregatedSet,
	oldRevisions disaggregatedsetutils.RevisionRolesList,
	roleNames []string,
	current, target RoleReplicaState,
) error {
	budget := make([]int, len(roleNames))
	for i := range roleNames {
		budget[i] = current[i] - target[i]
	}

	log := logf.FromContext(ctx)
	for _, wl := range sortByNewestTimestamp(oldRevisions, roleNames) {
		if allZero(budget) {
			break
		}

		newReplicas := make(map[string]int)
		plannedDrain := make(map[string]int)
		triggersCoordinated := make(map[string]bool)

		for i, name := range roleNames {
			lws, exists := wl.Roles[name]
			if !exists {
				continue
			}
			replicas := int(getLWSReplicas(lws))
			drain := min(budget[i], replicas)
			plannedDrain[name] = drain
			newReplicas[name] = replicas - drain
			if newReplicas[name] == 0 {
				triggersCoordinated[name] = true
			}
		}

		anyTriggered := len(triggersCoordinated) > 0
		if anyTriggered {
			for _, name := range roleNames {
				if _, exists := wl.Roles[name]; exists {
					newReplicas[name] = 0
				}
			}
		}

		for i, name := range roleNames {
			lws, exists := wl.Roles[name]
			if !exists {
				continue
			}
			replicas := int(getLWSReplicas(lws))
			if replicas <= newReplicas[name] {
				continue
			}
			// Address by the LWS's actual name so a legacy slice-0 object drains too.
			lwsName := lws.Name
			log.Info("Scaling down", "lws", lwsName, "from", replicas, "to", newReplicas[name])
			if err := executor.LWSManager.Scale(ctx, ds.Namespace, lwsName, newReplicas[name]); err != nil {
				return fmt.Errorf("failed to scale %s: %w", lwsName, err)
			}
			executor.Record.Eventf(ds, nil, corev1.EventTypeNormal, EventReasonScalingDown,
				"Update", "Scaling down %s LWS %s from %d to %d replicas", name, lwsName, replicas, newReplicas[name])

			if triggersCoordinated[name] || !anyTriggered {
				budget[i] -= plannedDrain[name]
			}
		}
	}
	return nil
}

func allZero(s []int) bool {
	for _, v := range s {
		if v > 0 {
			return false
		}
	}
	return true
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

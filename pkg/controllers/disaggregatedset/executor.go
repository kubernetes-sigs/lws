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
)

const (
	EventReasonRollingUpdateStarted   = "RollingUpdateStarted"
	EventReasonRollingUpdateCompleted = "RollingUpdateCompleted"
	EventReasonWorkloadCreated        = "WorkloadCreated"
	EventReasonScalingUp              = "ScalingUp"
	EventReasonScalingDown            = "ScalingDown"
	EventReasonWorkloadDeleted        = "WorkloadDeleted"
)

type RollingUpdateExecutor struct {
	Client          client.Client
	Record          events.EventRecorder
	WorkloadManager *LeaderWorkerSetManager
}

func (executor *RollingUpdateExecutor) ReconcileRollingUpdateNew(
	ctx context.Context,
	disaggregatedSet *disaggregatedsetv1.DisaggregatedSet,
	revision string,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	roleNames := GetRoleNames(disaggregatedSet)
	roleConfigs := GetRoleConfigs(disaggregatedSet)

	oldWorkloads, newWorkload, err := executor.WorkloadManager.GetGroupedWorkloads(ctx, disaggregatedSet.Namespace, disaggregatedSet.Name, revision)
	if err != nil {
		return ctrl.Result{}, err
	}
	if len(oldWorkloads) == 0 {
		return ctrl.Result{}, nil
	}

	addedRoles, removedRoles := detectRoleChanges(roleNames, oldWorkloads)
	if len(addedRoles) > 0 || len(removedRoles) > 0 {
		log.Info("Role changes detected", "added", addedRoles, "removed", removedRoles)
	}

	if newWorkload == nil {
		return executor.initRollingUpdate(ctx, disaggregatedSet, revision, roleNames, roleConfigs, oldWorkloads)
	}

	return executor.ReconcileRollingUpdate(ctx, disaggregatedSet, oldWorkloads, *newWorkload)
}

func (executor *RollingUpdateExecutor) initRollingUpdate(
	ctx context.Context,
	disaggregatedSet *disaggregatedsetv1.DisaggregatedSet,
	revision string,
	roleNames []string,
	roleConfigs map[string]*disaggregatedsetv1.DisaggregatedRoleSpec,
	oldWorkloads GroupedWorkloads,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	log.Info("Initiating new rolling update", "revision", revision)
	executor.Record.Eventf(disaggregatedSet, nil, corev1.EventTypeNormal, EventReasonRollingUpdateStarted,
		"Update", "Started rolling update to revision %s", revision)

	for _, oldGrouped := range oldWorkloads {
		for roleName, roleWorkload := range oldGrouped.Roles {
			workloadName := GenerateName(disaggregatedSet.Name, roleName, oldGrouped.Revision)
			if _, err := executor.WorkloadManager.SetInitialReplicas(ctx, disaggregatedSet.Namespace, workloadName, roleWorkload.Replicas); err != nil {
				log.Error(err, "Failed to set initial-replicas annotation", "workload", workloadName)
			}
		}
	}

	for _, roleName := range roleNames {
		created, err := executor.ensureNewWorkloadExists(ctx, disaggregatedSet, revision, roleName, roleConfigs[roleName], 0)
		if err != nil {
			return ctrl.Result{}, err
		}
		if created {
			workloadName := GenerateName(disaggregatedSet.Name, roleName, revision)
			log.Info("Created new workload", "role", roleName, "revision", revision)
			executor.Record.Eventf(disaggregatedSet, nil, corev1.EventTypeNormal, EventReasonWorkloadCreated,
				"Create", "Created %s workload %s", roleName, workloadName)
		}
	}

	return ctrl.Result{RequeueAfter: time.Second}, nil
}

func (executor *RollingUpdateExecutor) ReconcileRollingUpdate(
	ctx context.Context,
	disaggregatedSet *disaggregatedsetv1.DisaggregatedSet,
	oldWorkloads GroupedWorkloads,
	newWorkload GroupedWorkload,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	specRoleNames := GetRoleNames(disaggregatedSet)
	specRoleSet, oldRoleSet := buildRoleSets(specRoleNames, oldWorkloads)

	allRoleNames := append(slices.Clone(specRoleNames), removedRoles(oldRoleSet, specRoleSet)...)

	if !isWorkloadStable(newWorkload, specRoleNames) {
		log.V(1).Info("Waiting for new workload to stabilize")
		return ctrl.Result{RequeueAfter: time.Second}, nil
	}

	source, currentOld, currentNew, targetNew := buildPlannerState(disaggregatedSet, allRoleNames, specRoleSet, oldWorkloads, newWorkload)
	config := extractRollingUpdateConfig(disaggregatedSet, allRoleNames)

	nextStep := ComputeNextStep(source, currentOld, currentNew, targetNew, config)
	if nextStep == nil {
		log.Info("Rolling update complete")
		executor.Record.Eventf(disaggregatedSet, nil, corev1.EventTypeNormal, EventReasonRollingUpdateCompleted,
			"Update", "Completed rolling update to revision %s", newWorkload.Revision)
		return ctrl.Result{}, nil
	}

	log.Info("Next step computed", buildStepLogArgs(allRoleNames, nextStep)...)

	if err := executor.scaleUpNew(ctx, disaggregatedSet, newWorkload, allRoleNames, specRoleSet, currentNew, nextStep.New); err != nil {
		return ctrl.Result{}, err
	}
	if err := executor.scaleDownOld(ctx, disaggregatedSet, oldWorkloads, allRoleNames, currentOld, nextStep.Past); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

// --- Helpers ---

func buildRoleSets(specRoleNames []string, oldWorkloads GroupedWorkloads) (spec, old map[string]bool) {
	spec = make(map[string]bool, len(specRoleNames))
	for _, name := range specRoleNames {
		spec[name] = true
	}
	old = make(map[string]bool)
	for _, wl := range oldWorkloads {
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
	oldWorkloads GroupedWorkloads,
	newWorkload GroupedWorkload,
) (source, currentOld, currentNew, targetNew RoleReplicaState) {
	n := len(allRoleNames)
	source, currentOld, currentNew, targetNew = make(RoleReplicaState, n), make(RoleReplicaState, n), make(RoleReplicaState, n), make(RoleReplicaState, n)

	for i, roleName := range allRoleNames {
		source[i] = oldWorkloads.GetTotalInitialReplicasPerRole(roleName)
		currentOld[i] = oldWorkloads.GetTotalReplicasPerRole(roleName)

		if specRoleSet[roleName] {
			currentNew[i] = newWorkload.Roles[roleName].Replicas
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

func isWorkloadStable(workload GroupedWorkload, roleNames []string) bool {
	for _, name := range roleNames {
		if w := workload.Roles[name]; w.Replicas != w.ReadyReplicas {
			return false
		}
	}
	return true
}

func maxTimestamp(wl GroupedWorkload) time.Time {
	var maxTS time.Time
	for _, info := range wl.Roles {
		if info.CreationTimestamp.After(maxTS) {
			maxTS = info.CreationTimestamp
		}
	}
	return maxTS
}

func sortByNewestTimestamp(workloads GroupedWorkloads, roleNames []string) GroupedWorkloads {
	if len(roleNames) == 0 {
		return workloads
	}
	sorted := slices.Clone(workloads)
	slices.SortFunc(sorted, func(a, b GroupedWorkload) int {
		return maxTimestamp(b).Compare(maxTimestamp(a))
	})
	return sorted
}

// --- Scaling operations ---

func (executor *RollingUpdateExecutor) scaleUpNew(
	ctx context.Context,
	ds *disaggregatedsetv1.DisaggregatedSet,
	newWorkload GroupedWorkload,
	allRoleNames []string,
	specRoleSet map[string]bool,
	current, target RoleReplicaState,
) error {
	log := logf.FromContext(ctx)
	for i, name := range allRoleNames {
		if !specRoleSet[name] || current[i] >= target[i] {
			continue
		}
		workloadName := GenerateName(ds.Name, name, newWorkload.Revision)
		log.Info("Scaling up", "workload", workloadName, "from", current[i], "to", target[i])
		if err := executor.WorkloadManager.Scale(ctx, ds.Namespace, workloadName, target[i]); err != nil {
			return fmt.Errorf("failed to scale %s: %w", workloadName, err)
		}
		executor.Record.Eventf(ds, nil, corev1.EventTypeNormal, EventReasonScalingUp,
			"Update", "Scaling up %s workload %s from %d to %d replicas", name, workloadName, current[i], target[i])
	}
	return nil
}

func (executor *RollingUpdateExecutor) scaleDownOld(
	ctx context.Context,
	ds *disaggregatedsetv1.DisaggregatedSet,
	oldWorkloads GroupedWorkloads,
	roleNames []string,
	current, target RoleReplicaState,
) error {
	budget := make([]int, len(roleNames))
	for i := range roleNames {
		budget[i] = current[i] - target[i]
	}

	log := logf.FromContext(ctx)
	for _, wl := range sortByNewestTimestamp(oldWorkloads, roleNames) {
		if allZero(budget) {
			break
		}

		newReplicas := make(map[string]int)
		plannedDrain := make(map[string]int)
		triggersCoordinated := make(map[string]bool)

		for i, name := range roleNames {
			info, exists := wl.Roles[name]
			if !exists {
				continue
			}
			drain := min(budget[i], info.Replicas)
			plannedDrain[name] = drain
			newReplicas[name] = info.Replicas - drain
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
			info, exists := wl.Roles[name]
			if !exists || info.Replicas <= newReplicas[name] {
				continue
			}
			workloadName := GenerateName(ds.Name, name, wl.Revision)
			log.Info("Scaling down", "workload", workloadName, "from", info.Replicas, "to", newReplicas[name])
			if err := executor.WorkloadManager.Scale(ctx, ds.Namespace, workloadName, newReplicas[name]); err != nil {
				return fmt.Errorf("failed to scale %s: %w", workloadName, err)
			}
			executor.Record.Eventf(ds, nil, corev1.EventTypeNormal, EventReasonScalingDown,
				"Update", "Scaling down %s workload %s from %d to %d replicas", name, workloadName, info.Replicas, newReplicas[name])

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

// --- Workload creation ---

func (executor *RollingUpdateExecutor) ensureNewWorkloadExists(
	ctx context.Context,
	ds *disaggregatedsetv1.DisaggregatedSet,
	revision, role string,
	config *disaggregatedsetv1.DisaggregatedRoleSpec,
	initialReplicas int,
) (bool, error) {
	workloadName := GenerateName(ds.Name, role, revision)
	existing, err := executor.WorkloadManager.Get(ctx, ds.Namespace, workloadName)
	if err != nil {
		return false, fmt.Errorf("failed to get workload %s: %w", workloadName, err)
	}
	if existing != nil {
		return false, nil
	}

	if err := executor.WorkloadManager.Create(ctx, CreateParams{
		DisaggregatedSet: ds,
		Role:             role,
		Config:           config,
		Revision:         revision,
		Labels:           GenerateLabels(ds.Name, role, revision),
		Replicas:         initialReplicas,
	}); err != nil {
		return false, fmt.Errorf("failed to create workload %s: %w", workloadName, err)
	}
	return true, nil
}

// --- Role change utils ---
func detectRoleChanges(specRoleNames []string, oldWorkloads GroupedWorkloads) ([]string, []string) {
	specRoles, oldRoles := buildRoleSets(specRoleNames, oldWorkloads)

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

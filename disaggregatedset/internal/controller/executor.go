/*
Copyright 2026.

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

package controller

import (
	"context"
	"fmt"
	"slices"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	disaggv1alpha1 "sigs.k8s.io/disaggregatedset/api/v1alpha1"
)

const (
	EventReasonRollingUpdateStarted   = "RollingUpdateStarted"
	EventReasonRollingUpdateCompleted = "RollingUpdateCompleted"
	EventReasonWorkloadCreated        = "WorkloadCreated"
	EventReasonScalingUp              = "ScalingUp"
	EventReasonScalingDown            = "ScalingDown"
	EventReasonWorkloadDeleted        = "WorkloadDeleted"
	EventReasonPhasePolicyViolation   = "PhasePolicyViolation"
)

// RollingUpdateExecutor handles rolling update plan execution
type RollingUpdateExecutor struct {
	Client          client.Client
	Recorder        record.EventRecorder
	WorkloadManager *LeaderWorkerSetManager
}

// ReconcileRollingUpdateNew is the entry point for rolling updates.
func (executor *RollingUpdateExecutor) ReconcileRollingUpdateNew(
	ctx context.Context,
	disaggregatedSet *disaggv1alpha1.DisaggregatedSet,
	revision string,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	phaseNames := GetPhaseNames(disaggregatedSet)
	phaseConfigs := GetPhaseConfigs(disaggregatedSet)

	oldWorkloads, newWorkload, err := executor.WorkloadManager.GetGroupedWorkloads(ctx, disaggregatedSet.Namespace, disaggregatedSet.Name, revision)
	if err != nil {
		return ctrl.Result{}, err
	}
	if len(oldWorkloads) == 0 {
		return ctrl.Result{}, nil
	}

	// Detect phase changes and enforce policy
	changes := detectPhaseChanges(phaseNames, oldWorkloads)
	if len(changes.Added) > 0 || len(changes.Removed) > 0 {
		if getPhasePolicy(disaggregatedSet.Spec) == disaggv1alpha1.PhasePolicyStrict {
			return ctrl.Result{}, executor.rejectPhaseChanges(disaggregatedSet, changes)
		}
		log.Info("Phase changes detected with Flexible policy", "added", changes.Added, "removed", changes.Removed)
	}

	// If new workload doesn't exist, initialize rolling update
	if newWorkload == nil {
		return executor.initRollingUpdate(ctx, disaggregatedSet, revision, phaseNames, phaseConfigs, oldWorkloads)
	}

	return executor.ReconcileRollingUpdate(ctx, disaggregatedSet, oldWorkloads, *newWorkload)
}

func (executor *RollingUpdateExecutor) initRollingUpdate(
	ctx context.Context,
	disaggregatedSet *disaggv1alpha1.DisaggregatedSet,
	revision string,
	phaseNames []string,
	phaseConfigs map[string]*disaggv1alpha1.DisaggregatedPhaseSpec,
	oldWorkloads GroupedWorkloads,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	log.Info("Initiating new rolling update", "revision", revision)
	executor.Recorder.Eventf(disaggregatedSet, corev1.EventTypeNormal, EventReasonRollingUpdateStarted,
		"Started rolling update to revision %s", revision)

	// Set initial-replicas annotations on all old workloads
	for _, oldGrouped := range oldWorkloads {
		for phaseName, phaseWorkload := range oldGrouped.Phases {
			workloadName := GenerateName(disaggregatedSet.Name, phaseName, oldGrouped.Revision)
			if _, err := executor.WorkloadManager.SetInitialReplicas(ctx, disaggregatedSet.Namespace, workloadName, phaseWorkload.Replicas); err != nil {
				log.Error(err, "Failed to set initial-replicas annotation", "workload", workloadName)
			}
		}
	}

	// Create new workloads with 0 replicas
	for _, phaseName := range phaseNames {
		created, err := executor.ensureNewWorkloadExists(ctx, disaggregatedSet, revision, phaseName, phaseConfigs[phaseName], 0)
		if err != nil {
			return ctrl.Result{}, err
		}
		if created {
			workloadName := GenerateName(disaggregatedSet.Name, phaseName, revision)
			log.Info("Created new workload", "phase", phaseName, "revision", revision)
			executor.Recorder.Eventf(disaggregatedSet, corev1.EventTypeNormal, EventReasonWorkloadCreated,
				"Created %s workload %s", phaseName, workloadName)
		}
	}

	return ctrl.Result{RequeueAfter: time.Second}, nil
}

func (executor *RollingUpdateExecutor) ReconcileRollingUpdate(
	ctx context.Context,
	disaggregatedSet *disaggv1alpha1.DisaggregatedSet,
	oldWorkloads GroupedWorkloads,
	newWorkload GroupedWorkload,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	specPhaseNames := GetPhaseNames(disaggregatedSet)
	specPhaseSet, oldPhaseSet := buildPhaseSets(specPhaseNames, oldWorkloads)

	// allPhaseNames = spec phases + removed phases (for progressive drain)
	allPhaseNames := append(slices.Clone(specPhaseNames), removedPhases(oldPhaseSet, specPhaseSet)...)

	// Wait for new workload stability on spec phases
	if !isWorkloadStable(newWorkload, specPhaseNames) {
		log.V(1).Info("Waiting for new workload to stabilize")
		return ctrl.Result{RequeueAfter: time.Second}, nil
	}

	// Build planner state
	source, currentOld, currentNew, targetNew := buildPlannerState(disaggregatedSet, allPhaseNames, specPhaseSet, oldWorkloads, newWorkload)
	config := extractRollingUpdateConfig(disaggregatedSet, allPhaseNames, specPhaseSet)

	nextStep := ComputeNextStep(source, currentOld, currentNew, targetNew, config)
	if nextStep == nil {
		log.Info("Rolling update complete")
		executor.Recorder.Eventf(disaggregatedSet, corev1.EventTypeNormal, EventReasonRollingUpdateCompleted,
			"Completed rolling update to revision %s", newWorkload.Revision)
		return ctrl.Result{}, nil
	}

	log.Info("Next step computed", buildStepLogArgs(allPhaseNames, nextStep)...)

	// Scale up new (spec phases only), then scale down old (all phases)
	if err := executor.scaleUpNew(ctx, disaggregatedSet, newWorkload, allPhaseNames, specPhaseSet, currentNew, nextStep.New); err != nil {
		return ctrl.Result{}, err
	}
	if err := executor.scaleDownOld(ctx, disaggregatedSet, oldWorkloads, allPhaseNames, currentOld, nextStep.Past); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

// --- Helpers ---

func buildPhaseSets(specPhaseNames []string, oldWorkloads GroupedWorkloads) (spec, old map[string]bool) {
	spec = make(map[string]bool, len(specPhaseNames))
	for _, name := range specPhaseNames {
		spec[name] = true
	}
	old = make(map[string]bool)
	for _, wl := range oldWorkloads {
		for name := range wl.Phases {
			old[name] = true
		}
	}
	return spec, old
}

func removedPhases(oldPhaseSet, specPhaseSet map[string]bool) []string {
	var removed []string
	for phase := range oldPhaseSet {
		if !specPhaseSet[phase] {
			removed = append(removed, phase)
		}
	}
	return removed
}

func buildPlannerState(
	ds *disaggv1alpha1.DisaggregatedSet,
	allPhaseNames []string,
	specPhaseSet map[string]bool,
	oldWorkloads GroupedWorkloads,
	newWorkload GroupedWorkload,
) (source, currentOld, currentNew, targetNew PhaseReplicaState) {
	n := len(allPhaseNames)
	source, currentOld, currentNew, targetNew = make(PhaseReplicaState, n), make(PhaseReplicaState, n), make(PhaseReplicaState, n), make(PhaseReplicaState, n)

	for i, phaseName := range allPhaseNames {
		source[i] = oldWorkloads.GetTotalInitialReplicasPerPhase(phaseName)
		currentOld[i] = oldWorkloads.GetTotalReplicasPerPhase(phaseName)

		if specPhaseSet[phaseName] {
			currentNew[i] = newWorkload.Phases[phaseName].Replicas
			targetNew[i] = getTargetReplicas(ds, phaseName)
		}
		// Removed phases: currentNew=0, targetNew=0 (zero values)
	}
	return
}

func getTargetReplicas(ds *disaggv1alpha1.DisaggregatedSet, phaseName string) int {
	for _, p := range ds.Spec.Phases {
		if p.Name == phaseName {
			if p.Replicas == nil {
				return 1
			}
			return int(*p.Replicas)
		}
	}
	return 1
}

func extractRollingUpdateConfig(ds *disaggv1alpha1.DisaggregatedSet, allPhaseNames []string, specPhaseSet map[string]bool) RollingUpdateConfig {
	config := DefaultRollingUpdateConfig(len(allPhaseNames))

	specConfigs := make(map[string][2]int) // [maxSurge, maxUnavailable]
	for _, phase := range ds.Spec.Phases {
		surge, unavail := 1, 0
		if phase.RolloutStrategy != nil {
			if phase.RolloutStrategy.MaxSurge != nil {
				surge = phase.RolloutStrategy.MaxSurge.IntValue()
			}
			if phase.RolloutStrategy.MaxUnavailable != nil {
				unavail = phase.RolloutStrategy.MaxUnavailable.IntValue()
			}
		}
		specConfigs[phase.Name] = [2]int{surge, unavail}
	}

	for i, name := range allPhaseNames {
		if cfg, ok := specConfigs[name]; ok && specPhaseSet[name] {
			config.MaxSurge[i], config.MaxUnavailable[i] = cfg[0], cfg[1]
		}
	}
	return config
}

func buildStepLogArgs(phaseNames []string, step *UpdateStep) []interface{} {
	args := make([]interface{}, 0, len(phaseNames)*4)
	for i, name := range phaseNames {
		args = append(args, "past"+name, step.Past[i], "new"+name, step.New[i])
	}
	return args
}

func isWorkloadStable(workload GroupedWorkload, phaseNames []string) bool {
	for _, name := range phaseNames {
		if w := workload.Phases[name]; w.Replicas != w.ReadyReplicas {
			return false
		}
	}
	return true
}

func sortByOldestTimestamp(workloads GroupedWorkloads, phaseNames []string) GroupedWorkloads {
	if len(phaseNames) == 0 {
		return workloads
	}
	sorted := slices.Clone(workloads)
	firstPhase := phaseNames[0]
	slices.SortFunc(sorted, func(a, b GroupedWorkload) int {
		return a.Phases[firstPhase].CreationTimestamp.Compare(b.Phases[firstPhase].CreationTimestamp)
	})
	return sorted
}

// --- Scaling operations ---

func (executor *RollingUpdateExecutor) scaleUpNew(
	ctx context.Context,
	ds *disaggv1alpha1.DisaggregatedSet,
	newWorkload GroupedWorkload,
	allPhaseNames []string,
	specPhaseSet map[string]bool,
	current, target PhaseReplicaState,
) error {
	log := logf.FromContext(ctx)
	for i, name := range allPhaseNames {
		if !specPhaseSet[name] || current[i] >= target[i] {
			continue
		}
		workloadName := GenerateName(ds.Name, name, newWorkload.Revision)
		log.Info("Scaling up", "workload", workloadName, "from", current[i], "to", target[i])
		if err := executor.WorkloadManager.Scale(ctx, ds.Namespace, workloadName, target[i]); err != nil {
			return fmt.Errorf("failed to scale %s: %w", workloadName, err)
		}
		executor.Recorder.Eventf(ds, corev1.EventTypeNormal, EventReasonScalingUp,
			"Scaling up %s workload %s from %d to %d replicas", name, workloadName, current[i], target[i])
	}
	return nil
}

func (executor *RollingUpdateExecutor) scaleDownOld(
	ctx context.Context,
	ds *disaggv1alpha1.DisaggregatedSet,
	oldWorkloads GroupedWorkloads,
	phaseNames []string,
	current, target PhaseReplicaState,
) error {
	budget := make([]int, len(phaseNames))
	for i := range phaseNames {
		budget[i] = current[i] - target[i]
	}

	log := logf.FromContext(ctx)
	for _, wl := range sortByOldestTimestamp(oldWorkloads, phaseNames) {
		if allZero(budget) {
			break
		}

		// Calculate planned drain and track which phases trigger coordinated drain
		newReplicas := make(map[string]int)
		plannedDrain := make(map[string]int)
		triggersCoordinated := make(map[string]bool)

		for i, name := range phaseNames {
			info, exists := wl.Phases[name]
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

		// Coordinated drain: if any phase reaches 0, force all to 0
		anyTriggered := len(triggersCoordinated) > 0
		if anyTriggered {
			for _, name := range phaseNames {
				if _, exists := wl.Phases[name]; exists {
					newReplicas[name] = 0
				}
			}
		}

		// Apply scale-downs and update budget
		for i, name := range phaseNames {
			info, exists := wl.Phases[name]
			if !exists || info.Replicas <= newReplicas[name] {
				continue
			}
			workloadName := GenerateName(ds.Name, name, wl.Revision)
			log.Info("Scaling down", "workload", workloadName, "from", info.Replicas, "to", newReplicas[name])
			if err := executor.WorkloadManager.Scale(ctx, ds.Namespace, workloadName, newReplicas[name]); err != nil {
				return fmt.Errorf("failed to scale %s: %w", workloadName, err)
			}
			executor.Recorder.Eventf(ds, corev1.EventTypeNormal, EventReasonScalingDown,
				"Scaling down %s workload %s from %d to %d replicas", name, workloadName, info.Replicas, newReplicas[name])

			// Only consume budget for phases that triggered coordinated drain.
			// Phases forced to drain by coordinated drain don't consume budget (budget recycling).
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
	ds *disaggv1alpha1.DisaggregatedSet,
	revision, phase string,
	config *disaggv1alpha1.DisaggregatedPhaseSpec,
	initialReplicas int,
) (bool, error) {
	workloadName := GenerateName(ds.Name, phase, revision)
	existing, err := executor.WorkloadManager.Get(ctx, ds.Namespace, workloadName)
	if err != nil {
		return false, fmt.Errorf("failed to get workload %s: %w", workloadName, err)
	}
	if existing != nil {
		return false, nil
	}

	if err := executor.WorkloadManager.Create(ctx, CreateParams{
		DisaggregatedSet: ds,
		Phase:            phase,
		Config:           config,
		Revision:         revision,
		Labels:           GenerateLabels(ds.Name, phase, revision),
		Replicas:         initialReplicas,
	}); err != nil {
		return false, fmt.Errorf("failed to create workload %s: %w", workloadName, err)
	}
	return true, nil
}

// --- Phase policy ---

type phaseChanges struct {
	Added, Removed []string
}

func detectPhaseChanges(specPhaseNames []string, oldWorkloads GroupedWorkloads) phaseChanges {
	specPhases, oldPhases := buildPhaseSets(specPhaseNames, oldWorkloads)

	var added, removed []string
	for name := range oldPhases {
		if !specPhases[name] {
			removed = append(removed, name)
		}
	}
	for _, name := range specPhaseNames {
		if !oldPhases[name] {
			added = append(added, name)
		}
	}
	return phaseChanges{Added: added, Removed: removed}
}

func getPhasePolicy(spec disaggv1alpha1.DisaggregatedSetSpec) disaggv1alpha1.PhasePolicy {
	if spec.PhasePolicy == "" {
		return disaggv1alpha1.PhasePolicyStrict
	}
	return spec.PhasePolicy
}

func (executor *RollingUpdateExecutor) rejectPhaseChanges(ds *disaggv1alpha1.DisaggregatedSet, changes phaseChanges) error {
	err := fmt.Errorf("phasePolicy is Strict but phases changed: added=%v, removed=%v", changes.Added, changes.Removed)
	executor.Recorder.Eventf(ds, corev1.EventTypeWarning, EventReasonPhasePolicyViolation,
		"phasePolicy is Strict but phases changed: added=%v, removed=%v", changes.Added, changes.Removed)
	return err
}

// --- Test compatibility wrappers ---

// ExtractRollingUpdateConfig extracts rolling update config for all spec phases.
func ExtractRollingUpdateConfig(ds *disaggv1alpha1.DisaggregatedSet) RollingUpdateConfig {
	phaseNames := GetPhaseNames(ds)
	specPhaseSet := make(map[string]bool, len(phaseNames))
	for _, name := range phaseNames {
		specPhaseSet[name] = true
	}
	return extractRollingUpdateConfig(ds, phaseNames, specPhaseSet)
}

// scaleDownOldWorkloads is a test-compatible wrapper for scaleDownOld.
func scaleDownOldWorkloads(
	ctx context.Context,
	executor *RollingUpdateExecutor,
	ds *disaggv1alpha1.DisaggregatedSet,
	oldWorkloads GroupedWorkloads,
	phaseNames []string,
	toScaleDown PhaseReplicaState,
) error {
	// current = toScaleDown (budget), target = 0 for all
	target := make(PhaseReplicaState, len(phaseNames))
	current := make(PhaseReplicaState, len(phaseNames))
	for i, name := range phaseNames {
		current[i] = oldWorkloads.GetTotalReplicasPerPhase(name)
		target[i] = current[i] - toScaleDown[i]
	}
	return executor.scaleDownOld(ctx, ds, oldWorkloads, phaseNames, current, target)
}

// scaleUpNewWorkload is a test-compatible wrapper for scaleUpNew.
func scaleUpNewWorkload(
	ctx context.Context,
	executor *RollingUpdateExecutor,
	ds *disaggv1alpha1.DisaggregatedSet,
	newWorkload GroupedWorkload,
	phaseNames []string,
	target PhaseReplicaState,
) error {
	specPhaseSet := make(map[string]bool, len(phaseNames))
	for _, name := range phaseNames {
		specPhaseSet[name] = true
	}
	current := make(PhaseReplicaState, len(phaseNames))
	for i, name := range phaseNames {
		current[i] = newWorkload.Phases[name].Replicas
	}
	return executor.scaleUpNew(ctx, ds, newWorkload, phaseNames, specPhaseSet, current, target)
}

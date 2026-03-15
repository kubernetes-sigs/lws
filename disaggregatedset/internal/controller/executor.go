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

// Event reason constants for rolling update lifecycle
const (
	EventReasonRollingUpdateStarted   = "RollingUpdateStarted"
	EventReasonRollingUpdateCompleted = "RollingUpdateCompleted"
	EventReasonWorkloadCreated        = "WorkloadCreated"
	EventReasonScalingUp              = "ScalingUp"
	EventReasonScalingDown            = "ScalingDown"
	EventReasonWorkloadDeleted        = "WorkloadDeleted"
)

// ensureNewWorkloadExists creates the new workload for a phase if it doesn't exist.
// Returns true if a workload was created, false if it already exists.
func (executor *RollingUpdateExecutor) ensureNewWorkloadExists(
	ctx context.Context,
	disaggregatedSet *disaggv1alpha1.DisaggregatedSet,
	revision string,
	phase string,
	config *disaggv1alpha1.DisaggregatedPhaseSpec,
	initialReplicas int,
) (bool, error) {
	workloadName := GenerateName(disaggregatedSet.Name, phase, revision)
	existing, err := executor.WorkloadManager.Get(ctx, disaggregatedSet.Namespace, workloadName)
	if err != nil {
		return false, fmt.Errorf("failed to get workload %s: %w", workloadName, err)
	}
	if existing != nil {
		return false, nil // Already exists
	}

	labels := GenerateLabels(disaggregatedSet.Name, phase, revision)
	if err := executor.WorkloadManager.Create(ctx, CreateParams{
		DisaggregatedSet: disaggregatedSet,
		Phase:            phase,
		Config:           config,
		Revision:         revision,
		Labels:           labels,
		Replicas:         initialReplicas,
	}); err != nil {
		return false, fmt.Errorf("failed to create workload %s: %w", workloadName, err)
	}
	return true, nil
}

// scaleDownOldWorkloads scales down old workloads using a budget mechanism.
// It processes workloads oldest-first (by CreationTimestamp).
// If a workload reaches 0 on one phase, it forces all phases to 0 (coordinated drain).
// This handles the case where a new rolling update is triggered while one is already in progress,
// ensuring we don't leave orphaned single-phase workloads.
// phaseNames is the ordered list of phase names.
// toScaleDown contains the number of replicas to scale down per phase (indexed by phase order).
func scaleDownOldWorkloads(
	ctx context.Context,
	executor *RollingUpdateExecutor,
	disaggregatedSet *disaggv1alpha1.DisaggregatedSet,
	oldWorkloads GroupedWorkloads,
	phaseNames []string,
	toScaleDown []int,
) error {
	log := logf.FromContext(ctx)
	sortedWorkloads := sortByOldestTimestamp(oldWorkloads, phaseNames)
	numPhases := len(phaseNames)

	for _, workload := range sortedWorkloads {
		// Check if we're done
		allZero := true
		for i := 0; i < numPhases; i++ {
			if toScaleDown[i] > 0 {
				allZero = false
				break
			}
		}
		if allZero {
			break
		}

		// Get current replicas and calculate drain amounts
		current := make([]int, numPhases)
		drain := make([]int, numPhases)
		newReplicas := make([]int, numPhases)
		for i, phaseName := range phaseNames {
			current[i] = workload.Phases[phaseName].Replicas
			drain[i] = min(toScaleDown[i], current[i])
			newReplicas[i] = current[i] - drain[i]
		}

		// If any phase reaches 0, force all to 0 (coordinated drain)
		anyZero := false
		for i := 0; i < numPhases; i++ {
			if newReplicas[i] == 0 {
				anyZero = true
				break
			}
		}
		if anyZero {
			for i := 0; i < numPhases; i++ {
				toScaleDown[i] += newReplicas[i]
				newReplicas[i] = 0
			}
		}

		// Apply scale-down for each phase
		for i, phaseName := range phaseNames {
			if current[i] > newReplicas[i] {
				workloadName := GenerateName(disaggregatedSet.Name, phaseName, workload.Revision)
				log.Info("Scaling down old workload", "name", workloadName, "from", current[i], "to", newReplicas[i])
				if err := executor.WorkloadManager.Scale(ctx, disaggregatedSet.Namespace, workloadName, newReplicas[i]); err != nil {
					return fmt.Errorf("failed to scale %s: %w", workloadName, err)
				}
				executor.Recorder.Eventf(disaggregatedSet, corev1.EventTypeNormal, EventReasonScalingDown,
					"Scaling down %s workload %s from %d to %d replicas", phaseName, workloadName, current[i], newReplicas[i])
				toScaleDown[i] -= drain[i]
			}
		}
	}

	return nil
}

// scaleUpNewWorkload scales up the new workload to match the target replicas.
// phaseNames is the ordered list of phase names.
// target contains the target replica counts per phase (indexed by phase order).
func scaleUpNewWorkload(
	ctx context.Context,
	executor *RollingUpdateExecutor,
	disaggregatedSet *disaggv1alpha1.DisaggregatedSet,
	newWorkload GroupedWorkload,
	phaseNames []string,
	target PhaseReplicaState,
) error {
	log := logf.FromContext(ctx)

	for i, phaseName := range phaseNames {
		current := newWorkload.Phases[phaseName].Replicas
		if current < target[i] {
			workloadName := GenerateName(disaggregatedSet.Name, phaseName, newWorkload.Revision)
			log.Info("Scaling up new workload", "name", workloadName, "from", current, "to", target[i])
			if err := executor.WorkloadManager.Scale(ctx, disaggregatedSet.Namespace, workloadName, target[i]); err != nil {
				return fmt.Errorf("failed to scale %s: %w", workloadName, err)
			}
			executor.Recorder.Eventf(disaggregatedSet, corev1.EventTypeNormal, EventReasonScalingUp,
				"Scaling up %s workload %s from %d to %d replicas", phaseName, workloadName, current, target[i])
		}
	}

	return nil
}

// isWorkloadStable returns true if all phases have replicas == readyReplicas.
// phaseNames is the ordered list of phase names.
func isWorkloadStable(workload GroupedWorkload, phaseNames []string) bool {
	for _, phaseName := range phaseNames {
		phaseWorkload := workload.Phases[phaseName]
		if phaseWorkload.Replicas != phaseWorkload.ReadyReplicas {
			return false
		}
	}
	return true
}

// sortByOldestTimestamp returns workloads sorted by CreationTimestamp (oldest first).
// Uses the first phase's timestamp for sorting.
func sortByOldestTimestamp(workloads GroupedWorkloads, phaseNames []string) GroupedWorkloads {
	sorted := slices.Clone(workloads)
	if len(phaseNames) == 0 {
		return sorted
	}
	firstPhase := phaseNames[0]
	slices.SortFunc(sorted, func(a, b GroupedWorkload) int {
		return a.Phases[firstPhase].CreationTimestamp.Compare(b.Phases[firstPhase].CreationTimestamp)
	})
	return sorted
}

func (executor *RollingUpdateExecutor) ReconcileRollingUpdate(
	ctx context.Context,
	disaggregatedSet *disaggv1alpha1.DisaggregatedSet,
	oldWorkloads GroupedWorkloads,
	newWorkload GroupedWorkload,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	phaseNames := GetPhaseNames(disaggregatedSet)

	// Wait for new workload to be stable before computing next step.
	// This ensures we're working with a consistent state.
	if !isWorkloadStable(newWorkload, phaseNames) {
		logArgs := []interface{}{}
		for _, phaseName := range phaseNames {
			logArgs = append(logArgs, phaseName+"Replicas", newWorkload.Phases[phaseName].Replicas)
			logArgs = append(logArgs, phaseName+"Ready", newWorkload.Phases[phaseName].ReadyReplicas)
		}
		log.V(1).Info("Waiting for new workload to stabilize", logArgs...)
		return ctrl.Result{RequeueAfter: time.Second}, nil
	}

	// Build state slices indexed by phase order
	numPhases := len(phaseNames)
	source := make(PhaseReplicaState, numPhases)
	currentOld := make(PhaseReplicaState, numPhases)
	currentNew := make(PhaseReplicaState, numPhases)
	targetNew := make(PhaseReplicaState, numPhases)
	for i, phaseName := range phaseNames {
		source[i] = oldWorkloads.GetTotalInitialReplicasPerPhase(phaseName)
		currentOld[i] = oldWorkloads.GetTotalReplicasPerPhase(phaseName)
		currentNew[i] = newWorkload.Phases[phaseName].Replicas
		targetNew[i] = getReplicasOrDefault(disaggregatedSet.Spec.Phases[i].Replicas)
	}
	config := ExtractRollingUpdateConfig(disaggregatedSet)

	// Compute the next step
	nextStep := ComputeNextStep(source, currentOld, currentNew, targetNew, config)
	if nextStep == nil {
		// Rolling update is complete
		log.Info("Rolling update complete, no more steps needed")
		executor.Recorder.Eventf(disaggregatedSet, corev1.EventTypeNormal, EventReasonRollingUpdateCompleted,
			"Completed rolling update to revision %s", newWorkload.Revision)
		return ctrl.Result{}, nil
	}

	logArgs := []interface{}{}
	for i, phaseName := range phaseNames {
		logArgs = append(logArgs, "past"+phaseName, nextStep.Past[i])
		logArgs = append(logArgs, "new"+phaseName, nextStep.New[i])
	}
	log.Info("Next step computed", logArgs...)

	// Invariant check: planner should never ask for more old replicas than exist
	for i := 0; i < numPhases; i++ {
		if nextStep.Past[i] > currentOld[i] {
			return ctrl.Result{}, fmt.Errorf("invariant violation: step requests more old replicas than exist for phase %d (step: %d, current: %d)", i, nextStep.Past[i], currentOld[i])
		}
	}

	// Scale up new workload first (ensures we never go below minimum capacity if we crash mid-operation)
	needsScaleUp := false
	for i := 0; i < numPhases; i++ {
		if nextStep.New[i] > currentNew[i] {
			needsScaleUp = true
			break
		}
	}
	if needsScaleUp {
		if err := scaleUpNewWorkload(ctx, executor, disaggregatedSet, newWorkload, phaseNames, nextStep.New); err != nil {
			return ctrl.Result{}, err
		}
	}

	// Scale down old workloads after new ones are scaled up and ready
	needsScaleDown := false
	reductions := make([]int, numPhases)
	for i := 0; i < numPhases; i++ {
		if nextStep.Past[i] < currentOld[i] {
			needsScaleDown = true
			reductions[i] = currentOld[i] - nextStep.Past[i]
		}
	}
	if needsScaleDown {
		if err := scaleDownOldWorkloads(ctx, executor, disaggregatedSet, oldWorkloads, phaseNames, reductions); err != nil {
			return ctrl.Result{}, err
		}
	}

	return ctrl.Result{}, nil
}

// ReconcileRollingUpdateNew is the new entry point for rolling updates.
// It handles:
// 1. Fetching and grouping workloads by revision
// 2. Initializing annotations on old workloads when starting a new rolling update
// 3. Creating new workloads if they don't exist
// 4. Executing the rolling update logic
func (executor *RollingUpdateExecutor) ReconcileRollingUpdateNew(
	ctx context.Context,
	disaggregatedSet *disaggv1alpha1.DisaggregatedSet,
	revision string,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	phaseNames := GetPhaseNames(disaggregatedSet)
	phaseConfigs := GetPhaseConfigs(disaggregatedSet)

	// Get workloads
	oldWorkloads, newWorkload, err := executor.WorkloadManager.GetGroupedWorkloads(ctx, disaggregatedSet.Namespace, disaggregatedSet.Name, revision)
	if err != nil {
		return ctrl.Result{}, err
	}

	// Check if we actually have old workloads (rolling update scenario)
	if len(oldWorkloads) == 0 {
		// No old workloads - this shouldn't happen if we're called, but handle gracefully
		return ctrl.Result{}, nil
	}

	// If new workload doesn't exist, this is a fresh rolling update start
	// Initialize annotations on old workloads to record the starting point
	if newWorkload == nil {
		log.Info("Initiating new rolling update", "revision", revision)
		executor.Recorder.Eventf(disaggregatedSet, corev1.EventTypeNormal, EventReasonRollingUpdateStarted,
			"Started rolling update to revision %s", revision)

		// Set initial-replicas annotations on all old workloads
		for _, oldGrouped := range oldWorkloads {
			for phaseName, phaseWorkload := range oldGrouped.Phases {
				workloadName := GenerateName(disaggregatedSet.Name, phaseName, oldGrouped.Revision)
				_, err := executor.WorkloadManager.SetInitialReplicas(
					ctx,
					disaggregatedSet.Namespace,
					workloadName,
					phaseWorkload.Replicas,
				)
				if err != nil {
					log.Error(err, "Failed to set initial-replicas annotation", "workload", workloadName)
					// Continue anyway - not critical for rolling update to proceed
				}
			}
		}

		// Create new workloads for all phases with 0 replicas.
		// Starting at 0 allows the planner to handle the scale-up as part of the first step,
		// which maintains consistency with the old code path and ensures proper step tracking.
		for _, phaseName := range phaseNames {
			config := phaseConfigs[phaseName]
			created, err := executor.ensureNewWorkloadExists(ctx, disaggregatedSet, revision, phaseName, config, 0)
			if err != nil {
				return ctrl.Result{}, err
			}
			if created {
				log.Info("Created new workload", "phase", phaseName, "revision", revision)
				workloadName := GenerateName(disaggregatedSet.Name, phaseName, revision)
				executor.Recorder.Eventf(disaggregatedSet, corev1.EventTypeNormal, EventReasonWorkloadCreated,
					"Created %s workload %s", phaseName, workloadName)
			}
		}

		// Requeue to let the workloads be created
		return ctrl.Result{RequeueAfter: time.Second}, nil
	}

	// Execute the rolling update
	return executor.ReconcileRollingUpdate(ctx, disaggregatedSet, oldWorkloads, *newWorkload)
}

// getReplicasOrDefault returns the value of the pointer or 1 if nil.
func getReplicasOrDefault(replicas *int32) int {
	if replicas == nil {
		return 1
	}
	return int(*replicas)
}

// RollingUpdateExecutor handles rolling update plan execution
type RollingUpdateExecutor struct {
	Client          client.Client
	Recorder        record.EventRecorder
	WorkloadManager *LeaderWorkerSetManager
}

// ExtractRollingUpdateConfig extracts the rolling update config from the DisaggregatedSet spec.
// Returns default values (maxSurge=1, maxUnavailable=0) if not specified.
// The config is indexed by phase order (matching spec.phases order).
func ExtractRollingUpdateConfig(disaggregatedSet *disaggv1alpha1.DisaggregatedSet) RollingUpdateConfig {
	numPhases := len(disaggregatedSet.Spec.Phases)
	config := DefaultRollingUpdateConfig(numPhases)

	for i, phase := range disaggregatedSet.Spec.Phases {
		if phase.RolloutStrategy != nil {
			if phase.RolloutStrategy.MaxSurge != nil {
				config.MaxSurge[i] = phase.RolloutStrategy.MaxSurge.IntValue()
			}
			if phase.RolloutStrategy.MaxUnavailable != nil {
				config.MaxUnavailable[i] = phase.RolloutStrategy.MaxUnavailable.IntValue()
			}
		}
	}

	return config
}

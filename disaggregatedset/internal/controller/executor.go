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
// If a workload reaches 0 on one phase, it forces both phases to 0 (coordinated drain).
// This handles the case where a new rolling update is triggered while one is already in progress,
// ensuring we don't leave orphaned single-phase workloads.
func scaleDownOldWorkloads(
	ctx context.Context,
	executor *RollingUpdateExecutor,
	disaggregatedSet *disaggv1alpha1.DisaggregatedSet,
	oldWorkloads GroupedWorkloads,
	prefillToScaleDown int,
	decodeToScaleDown int,
) error {
	log := logf.FromContext(ctx)
	sortedWorkloads := sortByOldestTimestamp(oldWorkloads)
	for _, workload := range sortedWorkloads {
		if prefillToScaleDown <= 0 && decodeToScaleDown <= 0 {
			break
		}

		currentPrefill := workload.Phases[PhasePrefill].Replicas
		currentDecode := workload.Phases[PhaseDecode].Replicas

		// Calculate how much to drain from this workload
		prefillDrain := min(prefillToScaleDown, currentPrefill)
		decodeDrain := min(decodeToScaleDown, currentDecode)
		newPrefill := currentPrefill - prefillDrain
		newDecode := currentDecode - decodeDrain

		// If either phase reaches 0, force both to 0 (coordinated drain).
		// This handles cases where a new rolling update is triggered mid-rollout,
		// ensuring we don't leave orphaned single-phase workloads.
		if newPrefill == 0 || newDecode == 0 {
			prefillToScaleDown += newPrefill
			decodeToScaleDown += newDecode
			newPrefill = 0
			newDecode = 0
		}

		// Apply scale-down for prefill
		if currentPrefill > newPrefill {
			workloadName := GenerateName(disaggregatedSet.Name, PhasePrefill, workload.Revision)
			log.Info("Scaling down old workload", "name", workloadName, "from", currentPrefill, "to", newPrefill)
			if err := executor.WorkloadManager.Scale(ctx, disaggregatedSet.Namespace, workloadName, newPrefill); err != nil {
				return fmt.Errorf("failed to scale %s: %w", workloadName, err)
			}
			executor.Recorder.Eventf(disaggregatedSet, corev1.EventTypeNormal, EventReasonScalingDown,
				"Scaling down %s workload %s from %d to %d replicas", PhasePrefill, workloadName, currentPrefill, newPrefill)
			prefillToScaleDown -= prefillDrain
		}

		// Apply scale-down for decode
		if currentDecode > newDecode {
			workloadName := GenerateName(disaggregatedSet.Name, PhaseDecode, workload.Revision)
			log.Info("Scaling down old workload", "name", workloadName, "from", currentDecode, "to", newDecode)
			if err := executor.WorkloadManager.Scale(ctx, disaggregatedSet.Namespace, workloadName, newDecode); err != nil {
				return fmt.Errorf("failed to scale %s: %w", workloadName, err)
			}
			executor.Recorder.Eventf(disaggregatedSet, corev1.EventTypeNormal, EventReasonScalingDown,
				"Scaling down %s workload %s from %d to %d replicas", PhaseDecode, workloadName, currentDecode, newDecode)
			decodeToScaleDown -= decodeDrain
		}
	}

	return nil
}

// scaleUpNewWorkload scales up the new workload to match the target replicas.
func scaleUpNewWorkload(
	ctx context.Context,
	executor *RollingUpdateExecutor,
	disaggregatedSet *disaggv1alpha1.DisaggregatedSet,
	newWorkload GroupedWorkload,
	targetPrefill int,
	targetDecode int,
) error {
	log := logf.FromContext(ctx)

	currentPrefill := newWorkload.Phases[PhasePrefill].Replicas
	currentDecode := newWorkload.Phases[PhaseDecode].Replicas

	if currentPrefill < targetPrefill {
		workloadName := GenerateName(disaggregatedSet.Name, PhasePrefill, newWorkload.Revision)
		log.Info("Scaling up new workload", "name", workloadName, "from", currentPrefill, "to", targetPrefill)
		if err := executor.WorkloadManager.Scale(ctx, disaggregatedSet.Namespace, workloadName, targetPrefill); err != nil {
			return fmt.Errorf("failed to scale %s: %w", workloadName, err)
		}
		executor.Recorder.Eventf(disaggregatedSet, corev1.EventTypeNormal, EventReasonScalingUp,
			"Scaling up %s workload %s from %d to %d replicas", PhasePrefill, workloadName, currentPrefill, targetPrefill)
	}

	if currentDecode < targetDecode {
		workloadName := GenerateName(disaggregatedSet.Name, PhaseDecode, newWorkload.Revision)
		log.Info("Scaling up new workload", "name", workloadName, "from", currentDecode, "to", targetDecode)
		if err := executor.WorkloadManager.Scale(ctx, disaggregatedSet.Namespace, workloadName, targetDecode); err != nil {
			return fmt.Errorf("failed to scale %s: %w", workloadName, err)
		}
		executor.Recorder.Eventf(disaggregatedSet, corev1.EventTypeNormal, EventReasonScalingUp,
			"Scaling up %s workload %s from %d to %d replicas", PhaseDecode, workloadName, currentDecode, targetDecode)
	}

	return nil
}

// isWorkloadStable returns true if all phases have replicas == readyReplicas.
func isWorkloadStable(workload GroupedWorkload) bool {
	for _, phase := range []string{PhasePrefill, PhaseDecode} {
		phaseWorkload := workload.Phases[phase]
		if phaseWorkload.Replicas != phaseWorkload.ReadyReplicas {
			return false
		}
	}
	return true
}

// sortByOldestTimestamp returns workloads sorted by CreationTimestamp (oldest first).
func sortByOldestTimestamp(workloads GroupedWorkloads) GroupedWorkloads {
	sorted := slices.Clone(workloads)
	slices.SortFunc(sorted, func(a, b GroupedWorkload) int {
		return a.Phases[PhasePrefill].CreationTimestamp.Compare(b.Phases[PhasePrefill].CreationTimestamp)
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

	// Wait for new workload to be stable before computing next step.
	// This ensures we're working with a consistent state.
	if !isWorkloadStable(newWorkload) {
		log.V(1).Info("Waiting for new workload to stabilize",
			"prefillReplicas", newWorkload.Phases[PhasePrefill].Replicas,
			"prefillReady", newWorkload.Phases[PhasePrefill].ReadyReplicas,
			"decodeReplicas", newWorkload.Phases[PhaseDecode].Replicas,
			"decodeReady", newWorkload.Phases[PhaseDecode].ReadyReplicas)
		return ctrl.Result{RequeueAfter: time.Second}, nil
	}

	source := PhaseReplicaState{
		Prefill: oldWorkloads.GetTotalInitialReplicasPerPhase(PhasePrefill),
		Decode:  oldWorkloads.GetTotalInitialReplicasPerPhase(PhaseDecode),
	}
	currentOld := PhaseReplicaState{
		Prefill: oldWorkloads.GetTotalReplicasPerPhase(PhasePrefill),
		Decode:  oldWorkloads.GetTotalReplicasPerPhase(PhaseDecode),
	}
	currentNew := PhaseReplicaState{
		Prefill: newWorkload.Phases[PhasePrefill].Replicas,
		Decode:  newWorkload.Phases[PhaseDecode].Replicas,
	}
	targetNew := PhaseReplicaState{
		Prefill: getReplicasOrDefault(disaggregatedSet.Spec.Prefill.Replicas),
		Decode:  getReplicasOrDefault(disaggregatedSet.Spec.Decode.Replicas),
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

	log.Info("Next step computed",
		"pastPrefill", nextStep.PastPrefill,
		"pastDecode", nextStep.PastDecode,
		"newPrefill", nextStep.NewPrefill,
		"newDecode", nextStep.NewDecode)

	// Invariant check: planner should never ask for more old replicas than exist
	if nextStep.PastPrefill > currentOld.Prefill || nextStep.PastDecode > currentOld.Decode {
		return ctrl.Result{}, fmt.Errorf("invariant violation: step requests more old replicas than exist (step: %+v, current: %+v)", nextStep, currentOld)
	}

	// Scale up new workload first (ensures we never go below minimum capacity if we crash mid-operation)
	if nextStep.NewPrefill > currentNew.Prefill || nextStep.NewDecode > currentNew.Decode {
		if err := scaleUpNewWorkload(ctx, executor, disaggregatedSet, newWorkload, nextStep.NewPrefill, nextStep.NewDecode); err != nil {
			return ctrl.Result{}, err
		}
	}

	// Scale down old workloads after new ones are scaled up and ready
	if nextStep.PastPrefill < currentOld.Prefill || nextStep.PastDecode < currentOld.Decode {
		prefillReduction := currentOld.Prefill - nextStep.PastPrefill
		decodeReduction := currentOld.Decode - nextStep.PastDecode
		if err := scaleDownOldWorkloads(ctx, executor, disaggregatedSet, oldWorkloads, prefillReduction, decodeReduction); err != nil {
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
			for phase, phaseWorkload := range oldGrouped.Phases {
				workloadName := GenerateName(disaggregatedSet.Name, phase, oldGrouped.Revision)
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

		// Create new workloads for both phases with 0 replicas.
		// Starting at 0 allows the planner to handle the scale-up as part of the first step,
		// which maintains consistency with the old code path and ensures proper step tracking.
		for _, phase := range []string{PhasePrefill, PhaseDecode} {
			config := getPhaseConfig(disaggregatedSet, phase)
			created, err := executor.ensureNewWorkloadExists(ctx, disaggregatedSet, revision, phase, config, 0)
			if err != nil {
				return ctrl.Result{}, err
			}
			if created {
				log.Info("Created new workload", "phase", phase, "revision", revision)
				workloadName := GenerateName(disaggregatedSet.Name, phase, revision)
				executor.Recorder.Eventf(disaggregatedSet, corev1.EventTypeNormal, EventReasonWorkloadCreated,
					"Created %s workload %s", phase, workloadName)
			}
		}

		// Requeue to let the workloads be created
		return ctrl.Result{RequeueAfter: time.Second}, nil
	}

	// Execute the rolling update
	return executor.ReconcileRollingUpdate(ctx, disaggregatedSet, oldWorkloads, *newWorkload)
}

// getPhaseConfig returns the config for a given phase
func getPhaseConfig(disaggregatedSet *disaggv1alpha1.DisaggregatedSet, phase string) *disaggv1alpha1.DisaggregatedPhaseSpec {
	if phase == PhasePrefill {
		return disaggregatedSet.Spec.Prefill
	}
	return disaggregatedSet.Spec.Decode
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
func ExtractRollingUpdateConfig(disaggregatedSet *disaggv1alpha1.DisaggregatedSet) RollingUpdateConfig {
	config := DefaultRollingUpdateConfig()

	if disaggregatedSet.Spec.Prefill != nil && disaggregatedSet.Spec.Prefill.RolloutStrategy != nil {
		rollCfg := disaggregatedSet.Spec.Prefill.RolloutStrategy
		if rollCfg.MaxSurge != nil {
			config.PrefillMaxSurge = rollCfg.MaxSurge.IntValue()
		}
		if rollCfg.MaxUnavailable != nil {
			config.PrefillMaxUnavailable = rollCfg.MaxUnavailable.IntValue()
		}
	}

	if disaggregatedSet.Spec.Decode != nil && disaggregatedSet.Spec.Decode.RolloutStrategy != nil {
		rollCfg := disaggregatedSet.Spec.Decode.RolloutStrategy
		if rollCfg.MaxSurge != nil {
			config.DecodeMaxSurge = rollCfg.MaxSurge.IntValue()
		}
		if rollCfg.MaxUnavailable != nil {
			config.DecodeMaxUnavailable = rollCfg.MaxUnavailable.IntValue()
		}
	}

	return config
}

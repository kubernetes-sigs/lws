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

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	leaderworkerset "sigs.k8s.io/lws/api/leaderworkerset/v1"

	disaggv1alpha1 "sigs.k8s.io/disaggregatedset/api/v1alpha1"
)

// DisaggregatedSetReconciler reconciles a DisaggregatedSet object
type DisaggregatedSetReconciler struct {
	client.Client
	Scheme          *runtime.Scheme
	Recorder        record.EventRecorder
	WorkloadManager *LeaderWorkerSetManager
	ServiceManager  *ServiceManager
}

// +kubebuilder:rbac:groups=disaggregatedset.x-k8s.io,resources=disaggregatedsets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=disaggregatedset.x-k8s.io,resources=disaggregatedsets/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=disaggregatedset.x-k8s.io,resources=disaggregatedsets/finalizers,verbs=update
// +kubebuilder:rbac:groups=leaderworkerset.x-k8s.io,resources=leaderworkersets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=leaderworkerset.x-k8s.io,resources=leaderworkersets/status,verbs=get
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
func (reconciler *DisaggregatedSetReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	// Fetch the DisaggregatedSet instance
	disaggregatedSet := &disaggv1alpha1.DisaggregatedSet{}
	if err := reconciler.Get(ctx, req.NamespacedName, disaggregatedSet); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	log.Info("Reconciling DisaggregatedSet", "name", disaggregatedSet.Name, "namespace", disaggregatedSet.Namespace)

	// Validate both phases are configured - DisaggregatedSet requires both prefill and decode
	if disaggregatedSet.Spec.Prefill == nil || disaggregatedSet.Spec.Decode == nil {
		return ctrl.Result{}, fmt.Errorf("DisaggregatedSet requires both prefill and decode to be configured")
	}

	// Compute revision from both templates (truncated to 8 chars)
	revision := ComputeRevision(disaggregatedSet.Spec.Prefill, disaggregatedSet.Spec.Decode)

	// Cleanup old workloads with 0 replicas on both phases (housekeeping)
	if err := reconciler.cleanupDrainedWorkloads(ctx, disaggregatedSet, revision); err != nil {
		return ctrl.Result{}, err
	}

	// Create rolling update executor
	executor := reconciler.createRollingUpdateExecutor()

	// Get workloads grouped by revision to detect rolling update scenario
	oldWorkloads, _, err := executor.WorkloadManager.GetGroupedWorkloads(ctx, disaggregatedSet.Namespace, disaggregatedSet.Name, revision)
	if err != nil {
		return ctrl.Result{}, err
	}

	// Stateless routing: if old replicas exist → rolling update, else → simple reconcile
	var result ctrl.Result
	if len(oldWorkloads) > 0 && oldWorkloads.GetTotalReplicasPerPhase(PhasePrefill)+oldWorkloads.GetTotalReplicasPerPhase(PhaseDecode) > 0 {
		result, err = executor.ReconcileRollingUpdateNew(ctx, disaggregatedSet, revision)
		if err != nil {
			return result, err
		}
	} else {
		// No rolling update needed - use simple reconciliation
		result, err = reconciler.reconcileSimple(ctx, disaggregatedSet, revision)
		if err != nil {
			return result, err
		}
	}

	// Reconcile Services based on workload readiness
	// Re-fetch workloads to get latest readiness state after reconciliation
	allWorkloads, err := reconciler.WorkloadManager.List(ctx, disaggregatedSet.Namespace, disaggregatedSet.Name, "")
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to list workloads for service reconciliation: %w", err)
	}
	groupedWorkloads := groupWorkloadsByRevision(allWorkloads)

	if err := reconciler.ServiceManager.ReconcileServices(ctx, disaggregatedSet, groupedWorkloads, revision); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to reconcile services: %w", err)
	}

	return result, nil
}

// createRollingUpdateExecutor creates a configured RollingUpdateExecutor
func (reconciler *DisaggregatedSetReconciler) createRollingUpdateExecutor() *RollingUpdateExecutor {
	return &RollingUpdateExecutor{
		Client:          reconciler.Client,
		Recorder:        reconciler.Recorder,
		WorkloadManager: reconciler.WorkloadManager,
	}
}

// reconcileSimple handles non-rolling-update reconciliation (fresh deploy or stable state)
func (reconciler *DisaggregatedSetReconciler) reconcileSimple(ctx context.Context, disaggregatedSet *disaggv1alpha1.DisaggregatedSet, revision string) (ctrl.Result, error) {
	phaseConfigs := GetPhaseConfigs(disaggregatedSet)

	for phase, config := range phaseConfigs {
		if err := reconciler.reconcilePhaseSimple(ctx, disaggregatedSet, phase, config, revision); err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to reconcile %s phase: %w", phase, err)
		}
	}

	// Cleanup any old workloads with 0 replicas that remain from previous rolling updates
	if err := reconciler.cleanupOldWorkloads(ctx, disaggregatedSet, revision); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

// reconcilePhaseSimple handles simple reconciliation for a single phase
func (reconciler *DisaggregatedSetReconciler) reconcilePhaseSimple(ctx context.Context, disaggregatedSet *disaggv1alpha1.DisaggregatedSet, phase string, config *disaggv1alpha1.DisaggregatedPhaseSpec, revision string) error {
	log := logf.FromContext(ctx)

	workloadName := GenerateName(disaggregatedSet.Name, phase, revision)
	labels := GenerateLabels(disaggregatedSet.Name, phase, revision)

	// Check if workload exists
	existing, err := reconciler.WorkloadManager.Get(ctx, disaggregatedSet.Namespace, workloadName)
	if err != nil {
		return fmt.Errorf("failed to get workload %s: %w", workloadName, err)
	}

	desiredReplicas := int32(1) // default
	if config.Replicas != nil {
		desiredReplicas = *config.Replicas
	}

	if existing == nil {
		// Create new workload with full replicas
		log.Info("Creating workload", "phase", phase, "name", workloadName, "replicas", desiredReplicas)
		return reconciler.WorkloadManager.Create(ctx, CreateParams{
			DisaggregatedSet: disaggregatedSet,
			Phase:            phase,
			Config:           config,
			Revision:         revision,
			Labels:           labels,
			Replicas:         int(desiredReplicas),
		})
	}

	// Update if needed
	if existing.Replicas != int(desiredReplicas) {
		log.Info("Scaling workload", "phase", phase, "name", workloadName, "from", existing.Replicas, "to", desiredReplicas)
		if err := reconciler.WorkloadManager.Scale(ctx, disaggregatedSet.Namespace, workloadName, int(desiredReplicas)); err != nil {
			return fmt.Errorf("failed to scale workload %s: %w", workloadName, err)
		}
	}

	return nil
}

// cleanupOldWorkloads deletes old workloads with 0 replicas
func (reconciler *DisaggregatedSetReconciler) cleanupOldWorkloads(ctx context.Context, disaggregatedSet *disaggv1alpha1.DisaggregatedSet, revision string) error {
	log := logf.FromContext(ctx)

	for _, phase := range []string{PhasePrefill, PhaseDecode} {
		workloads, err := reconciler.WorkloadManager.List(ctx, disaggregatedSet.Namespace, disaggregatedSet.Name, phase)
		if err != nil {
			return fmt.Errorf("failed to list workloads for cleanup: %w", err)
		}
		for _, workloadInfo := range workloads {
			if workloadInfo.Revision != revision && workloadInfo.Replicas == 0 {
				log.Info("Deleting old workload", "name", workloadInfo.Name)
				if err := reconciler.WorkloadManager.Delete(ctx, disaggregatedSet.Namespace, workloadInfo.Name); err != nil {
					return fmt.Errorf("failed to delete old workload %s: %w", workloadInfo.Name, err)
				}
			}
		}
	}

	return nil
}

// cleanupDrainedWorkloads deletes old workloads where both phases have 0 replicas.
// This is coordinated cleanup - we only delete when BOTH prefill and decode are drained.
func (reconciler *DisaggregatedSetReconciler) cleanupDrainedWorkloads(ctx context.Context, disaggregatedSet *disaggv1alpha1.DisaggregatedSet, revision string) error {
	log := logf.FromContext(ctx)

	workloads, err := reconciler.WorkloadManager.List(ctx, disaggregatedSet.Namespace, disaggregatedSet.Name, "")
	if err != nil {
		return fmt.Errorf("failed to list workloads for cleanup: %w", err)
	}

	revisionReplicas := make(map[string]map[string]int)
	for _, workload := range workloads { // Group by revision
		if workload.Revision == revision {
			continue
		}
		if revisionReplicas[workload.Revision] == nil {
			revisionReplicas[workload.Revision] = make(map[string]int)
		}
		revisionReplicas[workload.Revision][workload.Phase] = workload.Replicas
	}

	for oldRevision, phases := range revisionReplicas {
		if phases[PhasePrefill] != 0 || phases[PhaseDecode] != 0 { // Delete revisions where both phases are at 0
			continue
		}
		for _, phase := range []string{PhasePrefill, PhaseDecode} {
			workloadName := GenerateName(disaggregatedSet.Name, phase, oldRevision)
			log.Info("Deleting drained workload", "name", workloadName)
			if err := reconciler.WorkloadManager.Delete(ctx, disaggregatedSet.Namespace, workloadName); err != nil {
				return fmt.Errorf("failed to delete workload %s: %w", workloadName, err)
			}
			reconciler.Recorder.Eventf(disaggregatedSet, corev1.EventTypeNormal, EventReasonWorkloadDeleted,
				"Deleted drained workload %s", workloadName)
		}
	}

	return nil
}

// setOwnerReference sets the owner reference manually
func (reconciler *DisaggregatedSetReconciler) setOwnerReference(obj metav1.Object, owner metav1.Object) {
	ownerRefs := obj.GetOwnerReferences()

	newRef := metav1.OwnerReference{
		APIVersion: disaggv1alpha1.GroupVersion.String(),
		Kind:       "DisaggregatedSet",
		Name:       owner.GetName(),
		UID:        owner.GetUID(),
		Controller: ptr.To(true),
	}

	for _, ref := range ownerRefs {
		if ref.UID == newRef.UID {
			return
		}
	}

	obj.SetOwnerReferences(append(ownerRefs, newRef))
}

// SetupWithManager sets up the controller with the Manager.
func (reconciler *DisaggregatedSetReconciler) SetupWithManager(manager ctrl.Manager) error {
	// Initialize workload manager if not already set
	if reconciler.WorkloadManager == nil {
		reconciler.WorkloadManager = NewLeaderWorkerSetManager(manager.GetClient())
	}

	// Initialize service manager if not already set
	if reconciler.ServiceManager == nil {
		reconciler.ServiceManager = NewServiceManager(manager.GetClient(), manager.GetScheme())
	}

	// Initialize event recorder if not already set
	if reconciler.Recorder == nil {
		reconciler.Recorder = manager.GetEventRecorderFor("disaggregatedset-controller")
	}

	// Note: We intentionally do NOT watch Services with Owns() to avoid a race condition
	// where Service deletions trigger reconciles that see stale specs and flip-flop.
	// Services are managed during regular reconciliation triggered by DisaggregatedSet or LWS changes.
	// OwnerReferences ensure Services are garbage collected when DisaggregatedSet is deleted.
	return ctrl.NewControllerManagedBy(manager).
		For(&disaggv1alpha1.DisaggregatedSet{}).
		Owns(&leaderworkerset.LeaderWorkerSet{}).
		Named("disaggregatedset").
		Complete(reconciler)
}

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

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/events"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	leaderworkerset "sigs.k8s.io/lws/api/leaderworkerset/v1"

	disaggregatedsetv1 "sigs.k8s.io/lws/api/disaggregatedset/v1"
)

// DisaggregatedSetReconciler reconciles a DisaggregatedSet object
type DisaggregatedSetReconciler struct {
	client.Client
	Scheme          *runtime.Scheme
	Record          events.EventRecorder
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

func (reconciler *DisaggregatedSetReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	disaggregatedSet := &disaggregatedsetv1.DisaggregatedSet{}
	if err := reconciler.Get(ctx, req.NamespacedName, disaggregatedSet); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	log.Info("Reconciling DisaggregatedSet", "name", disaggregatedSet.Name, "namespace", disaggregatedSet.Namespace)

	revision := ComputeRevision(disaggregatedSet.Spec.Roles)

	if err := reconciler.cleanupDrainedWorkloads(ctx, disaggregatedSet, revision); err != nil {
		return ctrl.Result{}, err
	}

	executor := reconciler.createRollingUpdateExecutor()

	oldWorkloads, _, err := executor.WorkloadManager.GetGroupedWorkloads(ctx, disaggregatedSet.Namespace, disaggregatedSet.Name, revision)
	if err != nil {
		return ctrl.Result{}, err
	}

	var result ctrl.Result
	roleNames := GetRoleNames(disaggregatedSet)
	totalOldReplicas := 0
	for _, roleName := range roleNames {
		totalOldReplicas += oldWorkloads.GetTotalReplicasPerRole(roleName)
	}
	if len(oldWorkloads) > 0 && totalOldReplicas > 0 {
		result, err = executor.ReconcileRollingUpdateNew(ctx, disaggregatedSet, revision)
		if err != nil {
			return result, err
		}
	} else {
		result, err = reconciler.reconcileSimple(ctx, disaggregatedSet, revision)
		if err != nil {
			return result, err
		}
	}

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

func (reconciler *DisaggregatedSetReconciler) createRollingUpdateExecutor() *RollingUpdateExecutor {
	return &RollingUpdateExecutor{
		Client:          reconciler.Client,
		Record:          reconciler.Record,
		WorkloadManager: reconciler.WorkloadManager,
	}
}

func (reconciler *DisaggregatedSetReconciler) reconcileSimple(ctx context.Context, disaggregatedSet *disaggregatedsetv1.DisaggregatedSet, revision string) (ctrl.Result, error) {
	roleConfigs := GetRoleConfigs(disaggregatedSet)

	for role, config := range roleConfigs {
		if err := reconciler.reconcileRoleSimple(ctx, disaggregatedSet, role, config, revision); err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to reconcile %s role: %w", role, err)
		}
	}

	if err := reconciler.cleanupOldWorkloads(ctx, disaggregatedSet, revision); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

func (reconciler *DisaggregatedSetReconciler) reconcileRoleSimple(ctx context.Context, disaggregatedSet *disaggregatedsetv1.DisaggregatedSet, role string, config *disaggregatedsetv1.DisaggregatedRoleSpec, revision string) error {
	log := logf.FromContext(ctx)

	workloadName := GenerateName(disaggregatedSet.Name, role, revision)
	labels := GenerateLabels(disaggregatedSet.Name, role, revision)

	existing, err := reconciler.WorkloadManager.Get(ctx, disaggregatedSet.Namespace, workloadName)
	if err != nil {
		return fmt.Errorf("failed to get workload %s: %w", workloadName, err)
	}

	desiredReplicas := int32(1)
	if config.Spec.Replicas != nil {
		desiredReplicas = *config.Spec.Replicas
	}

	if existing == nil {
		log.Info("Creating workload", "role", role, "name", workloadName, "replicas", desiredReplicas)
		return reconciler.WorkloadManager.Create(ctx, CreateParams{
			DisaggregatedSet: disaggregatedSet,
			Role:             role,
			Config:           config,
			Revision:         revision,
			Labels:           labels,
			Replicas:         int(desiredReplicas),
		})
	}

	if existing.Replicas != int(desiredReplicas) {
		log.Info("Scaling workload", "role", role, "name", workloadName, "from", existing.Replicas, "to", desiredReplicas)
		if err := reconciler.WorkloadManager.Scale(ctx, disaggregatedSet.Namespace, workloadName, int(desiredReplicas)); err != nil {
			return fmt.Errorf("failed to scale workload %s: %w", workloadName, err)
		}
	}

	return nil
}

func (reconciler *DisaggregatedSetReconciler) cleanupOldWorkloads(ctx context.Context, disaggregatedSet *disaggregatedsetv1.DisaggregatedSet, revision string) error {
	log := logf.FromContext(ctx)

	roleNames := GetRoleNames(disaggregatedSet)
	for _, roleName := range roleNames {
		workloads, err := reconciler.WorkloadManager.List(ctx, disaggregatedSet.Namespace, disaggregatedSet.Name, roleName)
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

func (reconciler *DisaggregatedSetReconciler) cleanupDrainedWorkloads(ctx context.Context, disaggregatedSet *disaggregatedsetv1.DisaggregatedSet, revision string) error {
	log := logf.FromContext(ctx)

	workloads, err := reconciler.WorkloadManager.List(ctx, disaggregatedSet.Namespace, disaggregatedSet.Name, "")
	if err != nil {
		return fmt.Errorf("failed to list workloads for cleanup: %w", err)
	}

	revisionReplicas := make(map[string]map[string]int)
	for _, workload := range workloads {
		if workload.Revision == revision {
			continue
		}
		if revisionReplicas[workload.Revision] == nil {
			revisionReplicas[workload.Revision] = make(map[string]int)
		}
		revisionReplicas[workload.Revision][workload.Role] = workload.Replicas
	}

	for oldRevision, roles := range revisionReplicas {
		allDrained := true
		for _, replicas := range roles {
			if replicas != 0 {
				allDrained = false
				break
			}
		}
		if !allDrained {
			continue
		}

		for roleName := range roles {
			workloadName := GenerateName(disaggregatedSet.Name, roleName, oldRevision)
			log.Info("Deleting drained workload", "name", workloadName)
			if err := reconciler.WorkloadManager.Delete(ctx, disaggregatedSet.Namespace, workloadName); err != nil {
				return fmt.Errorf("failed to delete workload %s: %w", workloadName, err)
			}
			reconciler.Record.Eventf(disaggregatedSet, nil, corev1.EventTypeNormal, EventReasonWorkloadDeleted,
				"Delete", "Deleted drained workload %s", workloadName)
		}
	}

	return nil
}

func (reconciler *DisaggregatedSetReconciler) setOwnerReference(obj metav1.Object, owner metav1.Object) {
	ownerRefs := obj.GetOwnerReferences()

	newRef := metav1.OwnerReference{
		APIVersion: disaggregatedsetv1.GroupVersion.String(),
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
func (reconciler *DisaggregatedSetReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if reconciler.WorkloadManager == nil {
		reconciler.WorkloadManager = NewLeaderWorkerSetManager(mgr.GetClient())
	}

	if reconciler.ServiceManager == nil {
		reconciler.ServiceManager = NewServiceManager(mgr.GetClient(), mgr.GetScheme())
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&disaggregatedsetv1.DisaggregatedSet{}).
		Owns(&leaderworkerset.LeaderWorkerSet{}).
		Named("disaggregatedset").
		Complete(reconciler)
}

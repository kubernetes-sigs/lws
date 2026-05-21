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
	leaderworkersetv1 "sigs.k8s.io/lws/api/leaderworkerset/v1"

	disaggregatedsetv1 "sigs.k8s.io/lws/api/disaggregatedset/v1"
	disaggregatedsetutils "sigs.k8s.io/lws/pkg/utils/disaggregatedset"
)

// DisaggregatedSetReconciler reconciles a DisaggregatedSet object
type DisaggregatedSetReconciler struct {
	client.Client
	Scheme         *runtime.Scheme
	Record         events.EventRecorder
	LWSManager     *LeaderWorkerSetManager
	ServiceManager *ServiceManager
}

// +kubebuilder:rbac:groups=disaggregatedset.x-k8s.io,resources=disaggregatedsets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=disaggregatedset.x-k8s.io,resources=disaggregatedsets/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=disaggregatedset.x-k8s.io,resources=disaggregatedsets/finalizers,verbs=update
// +kubebuilder:rbac:groups=leaderworkersetv1.x-k8s.io,resources=leaderworkersets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=leaderworkersetv1.x-k8s.io,resources=leaderworkersets/status,verbs=get
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

func (r *DisaggregatedSetReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	disaggregatedSet := &disaggregatedsetv1.DisaggregatedSet{}
	if err := r.Get(ctx, req.NamespacedName, disaggregatedSet); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	log.Info("Reconciling DisaggregatedSet", "name", disaggregatedSet.Name, "namespace", disaggregatedSet.Namespace)

	revision := disaggregatedsetutils.ComputeRevision(disaggregatedSet.Spec.Roles)

	if err := r.cleanupDrainedLWS(ctx, disaggregatedSet, revision); err != nil {
		return ctrl.Result{}, err
	}

	executor := r.createRollingUpdateExecutor()

	oldRevisions, _, err := executor.LWSManager.GetRevisionRolesList(ctx, disaggregatedSet.Namespace, disaggregatedSet.Name, revision)
	if err != nil {
		return ctrl.Result{}, err
	}

	var result ctrl.Result
	roleNames := disaggregatedsetutils.GetRoleNames(disaggregatedSet)
	totalOldReplicas := 0
	for _, roleName := range roleNames {
		totalOldReplicas += oldRevisions.GetTotalReplicasPerRole(roleName)
	}
	if len(oldRevisions) > 0 && totalOldReplicas > 0 {
		result, err = executor.ReconcileRollingUpdateNew(ctx, disaggregatedSet, revision)
		if err != nil {
			return result, err
		}
	} else {
		result, err = r.reconcileSimple(ctx, disaggregatedSet, revision)
		if err != nil {
			return result, err
		}
	}

	allLWS, err := r.LWSManager.List(ctx, disaggregatedSet.Namespace, disaggregatedSet.Name, "")
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to list LWS for service reconciliation: %w", err)
	}
	revisionRoles := disaggregatedsetutils.GroupByRevision(allLWS)

	if err := r.ServiceManager.ReconcileServices(ctx, disaggregatedSet, revisionRoles, revision); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to reconcile services: %w", err)
	}

	return result, nil
}

func (r *DisaggregatedSetReconciler) createRollingUpdateExecutor() *RollingUpdateExecutor {
	return &RollingUpdateExecutor{
		Client:     r.Client,
		Record:     r.Record,
		LWSManager: r.LWSManager,
	}
}

//nolint:unparam // Result is always empty but signature matches controller-runtime pattern
func (r *DisaggregatedSetReconciler) reconcileSimple(ctx context.Context, disaggregatedSet *disaggregatedsetv1.DisaggregatedSet, revision string) (ctrl.Result, error) {
	roleConfigs := disaggregatedsetutils.GetRoleConfigs(disaggregatedSet)

	for role, config := range roleConfigs {
		if err := r.reconcileRoleSimple(ctx, disaggregatedSet, role, config, revision); err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to reconcile %s role: %w", role, err)
		}
	}

	if err := r.cleanupOldLWS(ctx, disaggregatedSet, revision); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

func (r *DisaggregatedSetReconciler) reconcileRoleSimple(ctx context.Context, disaggregatedSet *disaggregatedsetv1.DisaggregatedSet, role string, config *disaggregatedsetv1.DisaggregatedRoleSpec, revision string) error {
	log := logf.FromContext(ctx)

	lwsName := disaggregatedsetutils.GenerateName(disaggregatedSet.Name, role, revision)
	labels := disaggregatedsetutils.GenerateLabels(disaggregatedSet.Name, role, revision)

	existing, err := r.LWSManager.Get(ctx, disaggregatedSet.Namespace, lwsName)
	if err != nil {
		return fmt.Errorf("failed to get LWS %s: %w", lwsName, err)
	}

	desiredReplicas := int32(1)
	if config.Spec.Replicas != nil {
		desiredReplicas = *config.Spec.Replicas
	}

	if existing == nil {
		log.Info("Creating LWS", "role", role, "name", lwsName, "replicas", desiredReplicas)
		return r.LWSManager.Create(ctx, disaggregatedsetutils.CreateParams{
			DisaggregatedSet: disaggregatedSet,
			Role:             role,
			Config:           config,
			Revision:         revision,
			Labels:           labels,
			Replicas:         int(desiredReplicas),
		})
	}

	existingReplicas := int32(1)
	if existing.Spec.Replicas != nil {
		existingReplicas = *existing.Spec.Replicas
	}
	if existingReplicas != desiredReplicas {
		log.Info("Scaling LWS", "role", role, "name", lwsName, "from", existingReplicas, "to", desiredReplicas)
		if err := r.LWSManager.Scale(ctx, disaggregatedSet.Namespace, lwsName, int(desiredReplicas)); err != nil {
			return fmt.Errorf("failed to scale LWS %s: %w", lwsName, err)
		}
	}

	return nil
}

func (r *DisaggregatedSetReconciler) cleanupOldLWS(ctx context.Context, disaggregatedSet *disaggregatedsetv1.DisaggregatedSet, revision string) error {
	log := logf.FromContext(ctx)

	roleNames := disaggregatedsetutils.GetRoleNames(disaggregatedSet)
	for _, roleName := range roleNames {
		lwsList, err := r.LWSManager.List(ctx, disaggregatedSet.Namespace, disaggregatedSet.Name, roleName)
		if err != nil {
			return fmt.Errorf("failed to list LWS for cleanup: %w", err)
		}
		for _, lws := range lwsList {
			lwsRevision := lws.Labels[disaggregatedsetv1.RevisionLabelKey]
			lwsReplicas := int32(0)
			if lws.Spec.Replicas != nil {
				lwsReplicas = *lws.Spec.Replicas
			}
			if lwsRevision != revision && lwsReplicas == 0 {
				log.Info("Deleting old LWS", "name", lws.Name)
				if err := r.LWSManager.Delete(ctx, disaggregatedSet.Namespace, lws.Name); err != nil {
					return fmt.Errorf("failed to delete old LWS %s: %w", lws.Name, err)
				}
			}
		}
	}

	return nil
}

func (r *DisaggregatedSetReconciler) cleanupDrainedLWS(ctx context.Context, disaggregatedSet *disaggregatedsetv1.DisaggregatedSet, revision string) error {
	log := logf.FromContext(ctx)

	lwsList, err := r.LWSManager.List(ctx, disaggregatedSet.Namespace, disaggregatedSet.Name, "")
	if err != nil {
		return fmt.Errorf("failed to list LWS for cleanup: %w", err)
	}

	revisionReplicas := make(map[string]map[string]int)
	for _, lws := range lwsList {
		lwsRevision := lws.Labels[disaggregatedsetv1.RevisionLabelKey]
		if lwsRevision == revision {
			continue
		}
		if revisionReplicas[lwsRevision] == nil {
			revisionReplicas[lwsRevision] = make(map[string]int)
		}
		lwsRole := lws.Labels[disaggregatedsetv1.RoleLabelKey]
		lwsReplicas := 0
		if lws.Spec.Replicas != nil {
			lwsReplicas = int(*lws.Spec.Replicas)
		}
		revisionReplicas[lwsRevision][lwsRole] = lwsReplicas
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
			lwsName := disaggregatedsetutils.GenerateName(disaggregatedSet.Name, roleName, oldRevision)
			log.Info("Deleting drained LWS", "name", lwsName)
			if err := r.LWSManager.Delete(ctx, disaggregatedSet.Namespace, lwsName); err != nil {
				return fmt.Errorf("failed to delete LWS %s: %w", lwsName, err)
			}
			r.Record.Eventf(disaggregatedSet, nil, corev1.EventTypeNormal, EventReasonLWSDeleted,
				"Delete", "Deleted drained LWS %s", lwsName)
		}
	}

	return nil
}

func (r *DisaggregatedSetReconciler) setOwnerReference(obj metav1.Object, owner metav1.Object) {
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
func (r *DisaggregatedSetReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.LWSManager == nil {
		r.LWSManager = NewLeaderWorkerSetManager(mgr.GetClient())
	}

	if r.ServiceManager == nil {
		r.ServiceManager = NewServiceManager(mgr.GetClient(), mgr.GetScheme())
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&disaggregatedsetv1.DisaggregatedSet{}).
		Owns(&leaderworkersetv1.LeaderWorkerSet{}).
		Named("disaggregatedset").
		Complete(r)
}

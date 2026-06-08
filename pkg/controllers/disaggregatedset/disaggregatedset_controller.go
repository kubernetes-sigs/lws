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
	"strconv"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/events"
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
// +kubebuilder:rbac:groups=leaderworkerset.x-k8s.io,resources=leaderworkersets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=leaderworkerset.x-k8s.io,resources=leaderworkersets/status,verbs=get
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

	// Reconcile proceeds in four steps:
	// 1. Compute the target revision from the current spec.
	// 2. Clean up fully-drained old revisions (all roles at 0 replicas).
	// 3. Reconcile LWS objects — either a rolling update (if old revisions with
	//    replicas exist) or a simple create/scale to the target revision.
	// 4. Reconcile services so that ready revisions get headless services.

	// Step 1: Compute the target revision hash from the spec's role templates.
	revision := disaggregatedsetutils.ComputeRevision(disaggregatedSet.Spec.Roles)
	sliceCount := int(disaggregatedsetutils.GetSlices(disaggregatedSet))

	// Step 2: Delete LWS/services for slices beyond the desired count (slice
	// scale-down). Per-revision drained cleanup runs per slice in reconcileSlice.
	if err := r.cleanupRemovedSlices(ctx, disaggregatedSet, sliceCount); err != nil {
		return ctrl.Result{}, err
	}

	// Step 3: Reconcile LWS objects.
	executor := r.createRollingUpdateExecutor()
	roleNames := disaggregatedsetutils.GetRoleNames(disaggregatedSet)

	var result ctrl.Result
	for slice := range sliceCount {
		sliceResult, err := r.reconcileSlice(ctx, executor, disaggregatedSet, slice, revision, roleNames)
		if err != nil {
			return sliceResult, err
		}
		result = soonerRequeue(result, sliceResult)
	}

	return result, nil
}

// reconcileSlice reconciles a single slice independently: it rolls the slice's LWS
// to the target revision (or scales them simply when no old revision is serving),
// then reconciles that slice's services.
func (r *DisaggregatedSetReconciler) reconcileSlice(
	ctx context.Context,
	executor *RollingUpdateExecutor,
	disaggregatedSet *disaggregatedsetv1.DisaggregatedSet,
	slice int,
	revision string,
	roleNames []string,
) (ctrl.Result, error) {
	if err := r.cleanupDrainedLWS(ctx, disaggregatedSet, slice, revision); err != nil {
		return ctrl.Result{}, err
	}

	oldRevisions, _, err := executor.LWSManager.GetRevisionRolesList(ctx, disaggregatedSet.Namespace, disaggregatedSet.Name, slice, revision)
	if err != nil {
		return ctrl.Result{}, err
	}

	totalOldReplicas := 0
	for _, roleName := range roleNames {
		totalOldReplicas += oldRevisions.GetTotalReplicasPerRole(roleName)
	}
	// If old revisions exist with running replicas, a rolling update is in
	// progress or needs to start. Otherwise, reconcileSimple creates/scales
	// LWS objects directly for the target revision (steady-state path).
	var result ctrl.Result
	if len(oldRevisions) > 0 && totalOldReplicas > 0 {
		result, err = executor.ReconcileRollingUpdateNew(ctx, disaggregatedSet, slice, revision)
	} else {
		result, err = r.reconcileSimple(ctx, disaggregatedSet, slice, revision)
	}
	if err != nil {
		return result, err
	}

	// Step 4: Reconcile headless services for revisions that are ready on all roles.
	allLWS, err := r.LWSManager.List(ctx, disaggregatedSet.Namespace, disaggregatedSet.Name, slice, "")
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to list LWS for service reconciliation: %w", err)
	}
	revisionRoles := disaggregatedsetutils.GroupByRevision(allLWS)

	if err := r.ServiceManager.ReconcileServices(ctx, disaggregatedSet, slice, revisionRoles, revision); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to reconcile services: %w", err)
	}

	return result, nil
}

// soonerRequeue keeps the soonest non-zero RequeueAfter across slices.
func soonerRequeue(a, b ctrl.Result) ctrl.Result {
	if b.RequeueAfter > 0 && (a.RequeueAfter == 0 || b.RequeueAfter < a.RequeueAfter) {
		return b
	}
	return a
}

// cleanupRemovedSlices deletes LWS and services for slice indices at or above the
// desired slice count. Removal is a direct delete; pods terminate via the normal
// pod grace period.
func (r *DisaggregatedSetReconciler) cleanupRemovedSlices(ctx context.Context, disaggregatedSet *disaggregatedsetv1.DisaggregatedSet, desiredSlices int) error {
	log := logf.FromContext(ctx)

	lwsList, err := r.LWSManager.List(ctx, disaggregatedSet.Namespace, disaggregatedSet.Name, -1, "")
	if err != nil {
		return fmt.Errorf("failed to list LWS for slice cleanup: %w", err)
	}
	for _, lws := range lwsList {
		sliceIdx, parseErr := strconv.Atoi(lws.Labels[disaggregatedsetv1.SliceLabelKey])
		if parseErr != nil || sliceIdx < desiredSlices {
			continue
		}
		log.Info("Deleting LWS for removed slice", "name", lws.Name, "slice", sliceIdx)
		if err := r.LWSManager.Delete(ctx, disaggregatedSet.Namespace, lws.Name); err != nil {
			return fmt.Errorf("failed to delete LWS %s: %w", lws.Name, err)
		}
		r.Record.Eventf(disaggregatedSet, nil, corev1.EventTypeNormal, EventReasonLWSDeleted,
			"Delete", "Deleted LWS %s for removed slice %d", lws.Name, sliceIdx)
	}

	return r.ServiceManager.CleanupRemovedSlices(ctx, disaggregatedSet, desiredSlices)
}

func (r *DisaggregatedSetReconciler) createRollingUpdateExecutor() *RollingUpdateExecutor {
	return &RollingUpdateExecutor{
		Client:     r.Client,
		Record:     r.Record,
		LWSManager: r.LWSManager,
	}
}

//nolint:unparam // Result is always empty but signature matches controller-runtime pattern
func (r *DisaggregatedSetReconciler) reconcileSimple(ctx context.Context, disaggregatedSet *disaggregatedsetv1.DisaggregatedSet, slice int, revision string) (ctrl.Result, error) {
	roleConfigs := disaggregatedsetutils.GetRoleConfigs(disaggregatedSet)

	for role, config := range roleConfigs {
		if err := r.reconcileRoleSimple(ctx, disaggregatedSet, slice, role, config, revision); err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to reconcile %s role: %w", role, err)
		}
	}

	return ctrl.Result{}, nil
}

func (r *DisaggregatedSetReconciler) reconcileRoleSimple(ctx context.Context, disaggregatedSet *disaggregatedsetv1.DisaggregatedSet, slice int, role string, config *disaggregatedsetv1.DisaggregatedRoleSpec, revision string) error {
	log := logf.FromContext(ctx)

	lwsName := disaggregatedsetutils.GenerateName(disaggregatedSet.Name, slice, revision, role)
	labels := disaggregatedsetutils.GenerateLabels(disaggregatedSet.Name, slice, revision, role)

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
			Slice:            slice,
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

// cleanupDrainedLWS deletes all LWS objects for old revisions where every role
// has been drained to 0 replicas. This ensures coordinated cleanup: we only
// delete a revision's LWS objects when ALL roles (prefill, decode, etc.) have
// finished draining, preventing partial teardown during rolling updates.
func (r *DisaggregatedSetReconciler) cleanupDrainedLWS(ctx context.Context, disaggregatedSet *disaggregatedsetv1.DisaggregatedSet, slice int, revision string) error {
	log := logf.FromContext(ctx)

	lwsList, err := r.LWSManager.List(ctx, disaggregatedSet.Namespace, disaggregatedSet.Name, slice, "")
	if err != nil {
		return fmt.Errorf("failed to list LWS for cleanup: %w", err)
	}

	// revisionReplicas maps revision -> role -> replica count.
	// Used to check if all roles of a revision have been drained to 0.
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
		if _, exists := revisionReplicas[lwsRevision][lwsRole]; exists {
			log.Info("WARNING: multiple LWS found for same role and revision",
				"role", lwsRole, "revision", lwsRevision, "lws", lws.Name)
		}
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
			lwsName := disaggregatedsetutils.GenerateName(disaggregatedSet.Name, slice, oldRevision, roleName)
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

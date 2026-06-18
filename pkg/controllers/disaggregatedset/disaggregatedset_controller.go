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
	"time"

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

	// Backward compatibility: when slices > 1, a pre-slices (label-less) slice-0
	// deployment that still sits at the target revision must be migrated to the
	// slice-aware form before any sibling slice is created, otherwise the legacy
	// slice-agnostic service would also select the siblings' same-revision pods.
	// While that migration runs we block the slice loop.
	if sliceCount > 1 {
		migrating, migResult, err := r.migrateLegacySlice0(ctx, executor, disaggregatedSet, revision, roleNames)
		if err != nil {
			return ctrl.Result{}, err
		}
		if migrating {
			return migResult, nil
		}
	}

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

	// GetForRole adopts a legacy slice-0 LWS in place, so we do not create a
	// duplicate slice-aware object over a pre-slices deployment.
	existing, err := r.LWSManager.GetForRole(ctx, disaggregatedSet, slice, revision, role)
	if err != nil {
		return fmt.Errorf("failed to get LWS for role %s revision %s: %w", role, revision, err)
	}

	desiredReplicas := int32(1)
	if config.Spec.Replicas != nil {
		desiredReplicas = *config.Spec.Replicas
	}

	if existing == nil {
		lwsName := disaggregatedsetutils.GenerateName(disaggregatedSet.Name, slice, revision, role)
		labels := disaggregatedsetutils.GenerateLabels(disaggregatedSet.Name, slice, revision, role)
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
		log.Info("Scaling LWS", "role", role, "name", existing.Name, "from", existingReplicas, "to", desiredReplicas)
		if err := r.LWSManager.Scale(ctx, disaggregatedSet.Namespace, existing.Name, int(desiredReplicas)); err != nil {
			return fmt.Errorf("failed to scale LWS %s: %w", existing.Name, err)
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

	// revisionLWS maps revision -> role -> LWS for old (non-target) revisions, so a
	// revision's LWS can be deleted by their actual names once every role has drained
	// to 0. Using the listed objects rather than regenerated names handles a legacy
	// slice-0 LWS, whose name has no slice segment.
	revisionLWS := make(map[string]map[string]*leaderworkersetv1.LeaderWorkerSet)
	for _, lws := range lwsList {
		lwsRevision := lws.Labels[disaggregatedsetv1.RevisionLabelKey]
		if lwsRevision == revision {
			continue
		}
		if revisionLWS[lwsRevision] == nil {
			revisionLWS[lwsRevision] = make(map[string]*leaderworkersetv1.LeaderWorkerSet)
		}
		lwsRole := lws.Labels[disaggregatedsetv1.RoleLabelKey]
		if _, exists := revisionLWS[lwsRevision][lwsRole]; exists {
			log.Info("WARNING: multiple LWS found for same role and revision",
				"role", lwsRole, "revision", lwsRevision, "lws", lws.Name)
		}
		revisionLWS[lwsRevision][lwsRole] = lws
	}

	for _, roles := range revisionLWS {
		allDrained := true
		for _, lws := range roles {
			if getLWSReplicas(lws) != 0 {
				allDrained = false
				break
			}
		}
		if !allDrained {
			continue
		}

		for _, lws := range roles {
			log.Info("Deleting drained LWS", "name", lws.Name)
			if err := r.LWSManager.Delete(ctx, disaggregatedSet.Namespace, lws.Name); err != nil {
				return fmt.Errorf("failed to delete LWS %s: %w", lws.Name, err)
			}
			r.Record.Eventf(disaggregatedSet, nil, corev1.EventTypeNormal, EventReasonLWSDeleted,
				"Delete", "Deleted drained LWS %s", lws.Name)
		}
	}

	return nil
}

// migrateLegacySlice0 migrates a pre-slices (label-less) slice-0 deployment to the
// slice-aware form when its LWS still sit at the target revision. Because legacy and
// slice-aware share the revision hash, the normal revision-keyed rollout cannot drive
// this, so it runs a bounded rolling swap reusing the planner on the legacy<->slice-aware
// split. It returns (true, requeue) while in progress, signaling the caller to block the
// slice loop until slice 0 carries the slice label, and (false, _) when there is nothing
// to migrate or the migration has completed.
func (r *DisaggregatedSetReconciler) migrateLegacySlice0(
	ctx context.Context,
	executor *RollingUpdateExecutor,
	disaggregatedSet *disaggregatedsetv1.DisaggregatedSet,
	revision string,
	roleNames []string,
) (bool, ctrl.Result, error) {
	log := logf.FromContext(ctx)

	// Detect legacy (label-less) slice-0 LWS sitting at the target revision.
	legacy := make(map[string]*leaderworkersetv1.LeaderWorkerSet)
	for _, role := range roleNames {
		lws, err := r.LWSManager.Get(ctx, disaggregatedSet.Namespace, disaggregatedsetutils.GenerateLegacyName(disaggregatedSet.Name, revision, role))
		if err != nil {
			return false, ctrl.Result{}, err
		}
		if lws != nil && !disaggregatedsetutils.HasSliceLabel(lws.Labels) {
			legacy[role] = lws
		}
	}
	if len(legacy) == 0 {
		return false, ctrl.Result{}, nil
	}

	log.Info("Migrating legacy slice-0 to slice-aware form before scaling slices", "revision", revision)

	// Ensure the slice-aware slice-0 LWS exist (created at 0 replicas).
	roleConfigs := disaggregatedsetutils.GetRoleConfigs(disaggregatedSet)
	newLWS := make(map[string]*leaderworkersetv1.LeaderWorkerSet)
	createdAny := false
	for _, role := range roleNames {
		existing, err := r.LWSManager.Get(ctx, disaggregatedSet.Namespace, disaggregatedsetutils.GenerateName(disaggregatedSet.Name, 0, revision, role))
		if err != nil {
			return false, ctrl.Result{}, err
		}
		if existing == nil {
			if _, err := executor.ensureNewLWSExists(ctx, disaggregatedSet, 0, revision, role, roleConfigs[role], 0); err != nil {
				return false, ctrl.Result{}, err
			}
			createdAny = true
			continue
		}
		newLWS[role] = existing
	}
	if createdAny {
		return true, ctrl.Result{RequeueAfter: time.Second}, nil
	}

	// Wait for the slice-aware LWS to stabilize before draining the legacy ones further.
	for _, role := range roleNames {
		lws := newLWS[role]
		if getLWSReplicas(lws) != lws.Status.ReadyReplicas {
			return true, ctrl.Result{RequeueAfter: time.Second}, nil
		}
	}

	// Drive a bounded rolling swap with the planner, treating the legacy LWS as the old
	// set and the slice-aware LWS as the new set (both at the same revision).
	n := len(roleNames)
	initialOld := make(RoleReplicaState, n)
	currentOld := make(RoleReplicaState, n)
	currentNew := make(RoleReplicaState, n)
	targetNew := make(RoleReplicaState, n)
	for i, role := range roleNames {
		target := getTargetReplicas(disaggregatedSet, role)
		initialOld[i] = target
		targetNew[i] = target
		if lws, ok := legacy[role]; ok {
			currentOld[i] = int(getLWSReplicas(lws))
		}
		currentNew[i] = int(getLWSReplicas(newLWS[role]))
	}
	config := extractRollingUpdateConfig(disaggregatedSet, roleNames)

	nextStep := ComputeNextStep(initialOld, currentOld, currentNew, targetNew, config)
	if nextStep == nil {
		if err := r.finishLegacyMigration(ctx, disaggregatedSet, revision, roleNames, legacy); err != nil {
			return false, ctrl.Result{}, err
		}
		return false, ctrl.Result{}, nil
	}

	for i, role := range roleNames {
		if currentNew[i] < nextStep.New[i] {
			name := disaggregatedsetutils.GenerateName(disaggregatedSet.Name, 0, revision, role)
			if err := r.LWSManager.Scale(ctx, disaggregatedSet.Namespace, name, nextStep.New[i]); err != nil {
				return false, ctrl.Result{}, fmt.Errorf("failed to scale up %s: %w", name, err)
			}
		}
	}
	for i, role := range roleNames {
		lws, ok := legacy[role]
		if !ok {
			continue
		}
		if int(getLWSReplicas(lws)) > nextStep.Past[i] {
			if err := r.LWSManager.Scale(ctx, disaggregatedSet.Namespace, lws.Name, nextStep.Past[i]); err != nil {
				return false, ctrl.Result{}, fmt.Errorf("failed to scale down %s: %w", lws.Name, err)
			}
		}
	}

	return true, ctrl.Result{RequeueAfter: time.Second}, nil
}

// finishLegacyMigration completes a legacy slice-0 migration: it creates the slice-aware
// slice-0 services, then deletes the legacy LWS and their lingering slice-agnostic
// services (the latter share the target revision so per-revision cleanup would not remove
// them).
func (r *DisaggregatedSetReconciler) finishLegacyMigration(
	ctx context.Context,
	disaggregatedSet *disaggregatedsetv1.DisaggregatedSet,
	revision string,
	roleNames []string,
	legacy map[string]*leaderworkersetv1.LeaderWorkerSet,
) error {
	log := logf.FromContext(ctx)

	for _, role := range roleNames {
		lws, err := r.LWSManager.Get(ctx, disaggregatedSet.Namespace, disaggregatedsetutils.GenerateName(disaggregatedSet.Name, 0, revision, role))
		if err != nil {
			return err
		}
		if lws != nil {
			if err := r.ServiceManager.ensureService(ctx, disaggregatedSet, lws); err != nil {
				return err
			}
		}
	}

	for role, lws := range legacy {
		log.Info("Deleting migrated legacy slice-0 LWS", "name", lws.Name)
		if err := r.LWSManager.Delete(ctx, disaggregatedSet.Namespace, lws.Name); err != nil {
			return fmt.Errorf("failed to delete legacy LWS %s: %w", lws.Name, err)
		}
		if err := r.ServiceManager.DeleteLegacyService(ctx, disaggregatedSet, revision, role); err != nil {
			return err
		}
		r.Record.Eventf(disaggregatedSet, nil, corev1.EventTypeNormal, EventReasonLWSDeleted,
			"Migrate", "Migrated legacy slice-0 LWS %s to slice-aware form", lws.Name)
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

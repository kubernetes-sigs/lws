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
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
	Scheme        *runtime.Scheme
	Record        events.EventRecorder
	LWSManager    *LeaderWorkerSetManager
	ScalerManager *ScalerManager
}

// +kubebuilder:rbac:groups=disaggregatedset.x-k8s.io,resources=disaggregatedsets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=disaggregatedset.x-k8s.io,resources=disaggregatedsets/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=disaggregatedset.x-k8s.io,resources=disaggregatedsets/finalizers,verbs=update
// +kubebuilder:rbac:groups=disaggregatedset.x-k8s.io,resources=disaggregatedsetrolescalers,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=disaggregatedset.x-k8s.io,resources=disaggregatedsetrolescalers/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=leaderworkerset.x-k8s.io,resources=leaderworkersets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=leaderworkerset.x-k8s.io,resources=leaderworkersets/status,verbs=get
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

	// Reconcile proceeds in three steps:
	// 1. Compute the target revision from the current spec.
	// 2. Clean up fully-drained old revisions (all roles at 0 replicas).
	// 3. Reconcile LWS objects — either a rolling update (if old revisions with
	//    replicas exist) or direct reconciliation of the current revision.

	// Step 1: Compute the target revision hash from the spec's role templates.
	revision := disaggregatedsetutils.ComputeRevision(disaggregatedSet.Spec.Roles)
	sliceCount := int(disaggregatedsetutils.GetSlices(disaggregatedSet))

	allLWS, err := r.LWSManager.List(ctx, disaggregatedSet, -1, "")
	if err != nil {
		return ctrl.Result{}, err
	}

	// Step 2: Delete LWS for slices beyond the desired count (slice scale-down).
	// Per-revision drained cleanup runs per slice in reconcileSlice.
	if err := r.cleanupRemovedSlices(ctx, disaggregatedSet, allLWS, sliceCount); err != nil {
		return ctrl.Result{}, err
	}

	// Auto-create / clean up per-role scalers so replica resolution below sees
	// a settled scaler map. Missing scalers are created; scalers whose role is
	// no longer External are deleted (in the same pass). New scalers are seeded
	// with the role's current aggregate replica count so a Static→External flip
	// on a running role does not drain to zero.
	if r.ScalerManager == nil {
		r.ScalerManager = NewScalerManager(r.Client, r.Record)
	}
	seedFor, err := r.seedForRole(ctx, disaggregatedSet)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to compute scaler seeds: %w", err)
	}
	scalers, err := r.ScalerManager.Reconcile(ctx, disaggregatedSet, seedFor)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to reconcile scalers: %w", err)
	}
	desiredReplicasByRole := resolveDesiredReplicasByRole(disaggregatedSet, scalers)

	// Step 3: Reconcile LWS objects.
	executor := r.createRollingUpdateExecutor()
	roleNames := disaggregatedsetutils.GetRoleNames(disaggregatedSet)

	// Backward compatibility. A pre-slices (label-less) LWS is adopted in place as
	// slice 0 by GetForRole, including when slices is raised above 1: nothing about
	// it has to become slice-aware, since the placement affinity terms already treat
	// an unlabeled pod as slice 0 (see SetPlacementAffinities). It converges to a
	// slice-aware name on the next revision change, without a restart.

	// Slices reconcile independently, so a failure in one must not skip the others.
	// Collect per-slice errors and join them; a non-nil result requeues the whole set.
	var result ctrl.Result
	var errs []error
	// Scaler reconciliation should normally resolve every desired role. A target
	// can remain unresolved only when a generated scaler cannot be created or
	// adopted because its name is occupied by an object this DS does not own.
	// This guard is a safety mechanism: never mutate workloads in that case.
	unresolvedRoles := unresolvedReplicaTargetRoles(roleNames, desiredReplicasByRole)
	if len(unresolvedRoles) > 0 {
		log.Info("Waiting for replica targets before reconciling slices", "roles", unresolvedRoles)
		result.RequeueAfter = time.Second
	} else {
		for slice := range sliceCount {
			sliceResult, err := r.reconcileSlice(ctx, executor, disaggregatedSet, slice, revision, desiredReplicasByRole)
			if err != nil {
				errs = append(errs, fmt.Errorf("slice %d: %w", slice, err))
				continue
			}
			result = earliestRequeue(result, sliceResult)
		}
	}
	reconcileErr := errors.Join(errs...)

	// Aggregate observed pod counts across all slices and revisions, then write
	// scaler status. The aggregate matches the aggregate selector shape.
	if err := r.updateScalerStatus(ctx, disaggregatedSet, scalers); err != nil {
		reconcileErr = errors.Join(reconcileErr, err)
	}

	// Status reflects the state observed above regardless of per-slice errors, so
	// a role that failed to reconcile is still visible to clients instead of being
	// silently left out of .status.
	if statusErr := r.updateStatus(ctx, disaggregatedSet, roleNames, revision, desiredReplicasByRole); statusErr != nil {
		return ctrl.Result{}, errors.Join(reconcileErr, fmt.Errorf("failed to update status: %w", statusErr))
	}

	return result, reconcileErr
}

// updateStatus recomputes per-role replica counts and the Available/Progressing
// condition from the LWS objects the DisaggregatedSet owns (aggregated across all
// slices and revisions), and persists the result if anything changed. roleNames is
// always the current spec.roles: a role removed from spec has no RoleStatuses entry
// even while its old LWS objects are still draining down to 0 (see RoleStatuses doc).
func (r *DisaggregatedSetReconciler) updateStatus(ctx context.Context, disaggregatedSet *disaggregatedsetv1.DisaggregatedSet, roleNames []string, revision string, desiredReplicasByRole map[string]int) error {
	roleStatuses := make([]disaggregatedsetv1.RoleStatus, 0, len(roleNames))
	sliceCount := disaggregatedsetutils.GetSlices(disaggregatedSet)
	unresolvedRoles := unresolvedReplicaTargetRoles(roleNames, desiredReplicasByRole)
	available := true

	for _, role := range roleNames {
		lwsList, err := r.LWSManager.List(ctx, disaggregatedSet, -1, role)
		if err != nil {
			return fmt.Errorf("failed to list LWS for role %s status: %w", role, err)
		}

		roleStatus := disaggregatedsetv1.RoleStatus{Name: role}
		for _, lws := range lwsList {
			roleStatus.Replicas += lws.Status.Replicas
			roleStatus.ReadyReplicas += lws.Status.ReadyReplicas
			// Only LWS at the target revision contribute to UpdatedReplicas; a
			// draining old-revision LWS is by definition not updated.
			if lws.Labels[disaggregatedsetv1.RevisionLabelKey] == revision {
				roleStatus.UpdatedReplicas += lws.Status.UpdatedReplicas
			}
		}
		roleStatuses = append(roleStatuses, roleStatus)

		// An External role with no resolved target (e.g. its generated scaler name
		// collided with a foreign, non-owned object — see #981 for the analogous
		// LWS case) cannot be evaluated safely. Treat that as explicitly
		// Progressing instead of guessing a target that might accidentally match.
		desiredPerSlice, targetKnown := desiredReplicasByRole[role]
		if !targetKnown {
			available = false
			continue
		}

		desired := int32(desiredPerSlice) * sliceCount
		if roleStatus.Replicas != desired || roleStatus.ReadyReplicas != desired || roleStatus.UpdatedReplicas != desired {
			available = false
		}
	}

	changed := setRoleStatuses(disaggregatedSet, roleStatuses)
	if setDisaggregatedSetCondition(disaggregatedSet, disaggregatedSetCondition(disaggregatedSet, available, unresolvedRoles)) {
		changed = true
	}
	if disaggregatedSet.Status.ObservedGeneration != disaggregatedSet.Generation {
		disaggregatedSet.Status.ObservedGeneration = disaggregatedSet.Generation
		changed = true
	}

	if !changed {
		return nil
	}
	if err := r.Status().Update(ctx, disaggregatedSet); err != nil {
		return fmt.Errorf("failed to update DisaggregatedSet status: %w", err)
	}
	return nil
}

func setRoleStatuses(disaggregatedSet *disaggregatedsetv1.DisaggregatedSet, roleStatuses []disaggregatedsetv1.RoleStatus) bool {
	if slices.Equal(disaggregatedSet.Status.RoleStatuses, roleStatuses) {
		return false
	}
	disaggregatedSet.Status.RoleStatuses = roleStatuses
	return true
}

func disaggregatedSetCondition(disaggregatedSet *disaggregatedsetv1.DisaggregatedSet, available bool, unresolvedRoles []string) metav1.Condition {
	condType := disaggregatedsetv1.DisaggregatedSetProgressing
	reason, message := "RolloutInProgress", "Not all roles have reached their desired replica count, ready and updated to the current revision"
	if len(unresolvedRoles) > 0 {
		reason = "ReplicaTargetsUnresolved"
		message = fmt.Sprintf("DisaggregatedSetRoleScaler could not be created or adopted for roles: %s", strings.Join(unresolvedRoles, ", "))
	} else if available {
		condType = disaggregatedsetv1.DisaggregatedSetAvailable
		reason, message = "AllRolesReady", "All roles have reached their desired replica count, ready and updated to the current revision"
	}

	return metav1.Condition{
		Type:               string(condType),
		Status:             metav1.ConditionTrue,
		ObservedGeneration: disaggregatedSet.Generation,
		Reason:             reason,
		Message:            message,
	}
}

// exclusiveConditionTypes reports whether t1 and t2 are a mutually-exclusive
// pair, where one being true means the other must be false. Only
// Available/Progressing are exclusive today; a condition type outside that
// pair (added later, or written by another controller) is left untouched by
// setDisaggregatedSetCondition rather than being clobbered just because it
// happened to also be true.
func exclusiveConditionTypes(t1, t2 string) bool {
	pair := func(a, b disaggregatedsetv1.DisaggregatedSetConditionType) bool {
		return (t1 == string(a) && t2 == string(b)) || (t1 == string(b) && t2 == string(a))
	}
	return pair(disaggregatedsetv1.DisaggregatedSetAvailable, disaggregatedsetv1.DisaggregatedSetProgressing)
}

// setDisaggregatedSetCondition records newCondition as true and, since Available and
// Progressing are mutually exclusive, marks the other one of that specific pair as
// false (see exclusiveConditionTypes) — any other condition type is left alone.
// LastTransitionTime is only touched when a condition's Status actually flips, per
// the metav1.Condition contract; a same-Status update (e.g. only ObservedGeneration
// changed) must not look like a fresh transition to clients. Returns whether the
// status changed.
func setDisaggregatedSetCondition(disaggregatedSet *disaggregatedsetv1.DisaggregatedSet, newCondition metav1.Condition) bool {
	now := metav1.Now()
	changed, found := false, false

	for i, cond := range disaggregatedSet.Status.Conditions {
		if cond.Type == newCondition.Type {
			found = true
			if cond.Status != newCondition.Status {
				newCondition.LastTransitionTime = now
				disaggregatedSet.Status.Conditions[i] = newCondition
				changed = true
			} else if cond.ObservedGeneration != newCondition.ObservedGeneration || cond.Reason != newCondition.Reason || cond.Message != newCondition.Message {
				// Status is unchanged, so LastTransitionTime is preserved, but every
				// other field still syncs to the latest computed condition.
				disaggregatedSet.Status.Conditions[i].ObservedGeneration = newCondition.ObservedGeneration
				disaggregatedSet.Status.Conditions[i].Reason = newCondition.Reason
				disaggregatedSet.Status.Conditions[i].Message = newCondition.Message
				changed = true
			}
			continue
		}
		if !exclusiveConditionTypes(cond.Type, newCondition.Type) {
			continue
		}
		if cond.Status == metav1.ConditionTrue {
			// newCondition becoming true is exactly why this mutually-exclusive
			// condition is now false, so it explains the flip with the same
			// Reason/Message rather than leaving this condition's old (now
			// contradictory) ones in place.
			disaggregatedSet.Status.Conditions[i].Status = metav1.ConditionFalse
			disaggregatedSet.Status.Conditions[i].LastTransitionTime = now
			disaggregatedSet.Status.Conditions[i].ObservedGeneration = newCondition.ObservedGeneration
			disaggregatedSet.Status.Conditions[i].Reason = newCondition.Reason
			disaggregatedSet.Status.Conditions[i].Message = newCondition.Message
			changed = true
		} else if cond.ObservedGeneration != newCondition.ObservedGeneration {
			// Already false and staying false — no real transition, so only
			// ObservedGeneration needs to catch up; Reason/Message still
			// accurately describe why it became false and don't need to change.
			disaggregatedSet.Status.Conditions[i].ObservedGeneration = newCondition.ObservedGeneration
			changed = true
		}
	}

	if !found {
		newCondition.LastTransitionTime = now
		disaggregatedSet.Status.Conditions = append(disaggregatedSet.Status.Conditions, newCondition)
		changed = true
	}

	return changed
}

// seedForRole returns a callback that yields the initial spec.replicas value
// for a newly-created scaler. For a Static→External flip the seed is the
// role's current aggregate LWS replica count so the running fleet is not
// drained to 0. For a fresh role (no LWS yet) the seed is 1 rather than 0 so
// vanilla HPA can bootstrap via minReplicas — HPA parks in ScalingDisabled
// when it reads current=0 from /scale, regardless of minReplicas, unless the
// HPAScaleToZero feature gate is enabled. Autoscalers that support scale-from-
// zero (KEDA, HPA with the gate flipped) can still take the role down to 0
// after attach.
func (r *DisaggregatedSetReconciler) seedForRole(ctx context.Context, ds *disaggregatedsetv1.DisaggregatedSet) (func(string) int32, error) {
	all, err := r.LWSManager.List(ctx, ds, -1, "")
	if err != nil {
		return nil, fmt.Errorf("list LWS for scaler seed: %w", err)
	}
	sums := make(map[string]int32)
	seen := make(map[string]bool)
	for _, lws := range all {
		role := lws.Labels[disaggregatedsetv1.RoleLabelKey]
		seen[role] = true
		if lws.Spec.Replicas != nil {
			sums[role] += *lws.Spec.Replicas
		} else {
			sums[role]++
		}
	}
	return func(role string) int32 {
		if !seen[role] {
			return 1
		}
		return sums[role]
	}, nil
}

// updateScalerStatus sums observed replicas across all slices/revisions per
// role and writes the aggregate to each controlled scaler's status. The unit
// is LWS groups (== leader pods), matching spec.replicas that HPA writes and
// the leader-only status.selector that HPA metric-averages over.
func (r *DisaggregatedSetReconciler) updateScalerStatus(
	ctx context.Context,
	ds *disaggregatedsetv1.DisaggregatedSet,
	scalers map[string]*disaggregatedsetv1.DisaggregatedSetRoleScaler,
) error {
	if len(scalers) == 0 {
		return nil
	}
	all, err := r.LWSManager.List(ctx, ds, -1, "")
	if err != nil {
		return fmt.Errorf("list LWS for scaler status: %w", err)
	}
	observed := make(map[string]int32, len(scalers))
	for _, lws := range all {
		role := lws.Labels[disaggregatedsetv1.RoleLabelKey]
		if _, ok := scalers[role]; !ok {
			continue
		}
		observed[role] += lws.Status.Replicas
	}
	return r.ScalerManager.WriteStatus(ctx, ds, scalers, observed)
}

// reconcileSlice reconciles a single slice independently: it rolls the slice's
// LWS to the target revision, or reconciles the current revision directly when
// no old revision is serving.
func (r *DisaggregatedSetReconciler) reconcileSlice(
	ctx context.Context,
	executor *RollingUpdateExecutor,
	disaggregatedSet *disaggregatedsetv1.DisaggregatedSet,
	slice int,
	revision string,
	desiredReplicasByRole map[string]int,
) (ctrl.Result, error) {
	oldRevisions, _, err := executor.LWSManager.GetRevisionRolesList(ctx, disaggregatedSet, slice, revision)
	if err != nil {
		return ctrl.Result{}, err
	}

	// Keep the rolling-update path active while any old revision exists, even
	// after its Spec reaches zero. The executor must observe that terminal state
	// so it can wait for the target revision to become Ready before declaring
	// the rollout complete. The old objects are deleted only after that check.
	var result ctrl.Result
	if len(oldRevisions) > 0 {
		var complete bool
		result, complete, err = executor.ReconcileRevisionTransition(ctx, disaggregatedSet, slice, revision, desiredReplicasByRole)
		if err != nil {
			return result, err
		}
		if complete {
			if err := r.cleanupDrainedLWS(ctx, disaggregatedSet, slice, revision); err != nil {
				return result, err
			}
		}
	} else {
		result, err = r.reconcileCurrentRevision(ctx, disaggregatedSet, slice, revision, desiredReplicasByRole)
		if err != nil {
			return result, err
		}
	}

	return result, nil
}

// earliestRequeue keeps the soonest non-zero RequeueAfter across slices.
func earliestRequeue(a, b ctrl.Result) ctrl.Result {
	if b.RequeueAfter > 0 && (a.RequeueAfter == 0 || b.RequeueAfter < a.RequeueAfter) {
		return b
	}
	return a
}

// cleanupRemovedSlices deletes LWS for slice indices at or above the desired
// slice count.
func (r *DisaggregatedSetReconciler) cleanupRemovedSlices(ctx context.Context, disaggregatedSet *disaggregatedsetv1.DisaggregatedSet, lwsList []*leaderworkersetv1.LeaderWorkerSet, desiredSlices int) error {
	log := logf.FromContext(ctx)

	for _, lws := range lwsList {
		sliceIdx, parseErr := strconv.Atoi(lws.Labels[disaggregatedsetv1.SliceLabelKey])
		if parseErr != nil || sliceIdx < desiredSlices {
			continue
		}
		log.Info("Deleting LWS for removed slice", "name", lws.Name, "slice", sliceIdx)
		if err := r.LWSManager.deleteInForeground(ctx, lws); err != nil {
			return fmt.Errorf("failed to delete LWS %s: %w", lws.Name, err)
		}
		r.Record.Eventf(disaggregatedSet, nil, corev1.EventTypeNormal, EventReasonLWSDeleted,
			"Delete", "Deleted LWS %s for removed slice %d", lws.Name, sliceIdx)
	}

	return nil
}

func (r *DisaggregatedSetReconciler) createRollingUpdateExecutor() *RollingUpdateExecutor {
	return &RollingUpdateExecutor{
		Record:     r.Record,
		LWSManager: r.LWSManager,
	}
}

//nolint:unparam // Result is always empty but signature matches controller-runtime pattern
func (r *DisaggregatedSetReconciler) reconcileCurrentRevision(ctx context.Context, disaggregatedSet *disaggregatedsetv1.DisaggregatedSet, slice int, revision string, desiredReplicasByRole map[string]int) (ctrl.Result, error) {
	roleConfigs := disaggregatedsetutils.GetRoleConfigs(disaggregatedSet)

	for role, config := range roleConfigs {
		if err := r.reconcileCurrentRevisionRole(ctx, disaggregatedSet, slice, role, config, revision, desiredReplicasByRole); err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to reconcile %s role: %w", role, err)
		}
	}

	return ctrl.Result{}, nil
}

func (r *DisaggregatedSetReconciler) reconcileCurrentRevisionRole(ctx context.Context, disaggregatedSet *disaggregatedsetv1.DisaggregatedSet, slice int, role string, config *disaggregatedsetv1.DisaggregatedRoleSpec, revision string, desiredReplicasByRole map[string]int) error {
	log := logf.FromContext(ctx)

	// GetForRole adopts a legacy slice-0 LWS in place, so we do not create a
	// duplicate slice-aware object over a pre-slices deployment.
	existing, err := r.LWSManager.GetForRole(ctx, disaggregatedSet, slice, revision, role)
	if err != nil {
		return fmt.Errorf("failed to get LWS for role %s revision %s: %w", role, revision, err)
	}

	desiredReplicas := int32(desiredReplicasByRole[role])

	if existing == nil {
		return r.LWSManager.Create(ctx, disaggregatedSet, config, slice, int(desiredReplicas), int(desiredReplicas))
	}

	// This revision remains the current target outside a revision transition, so
	// replica-only changes (including external-scaler writes) update its durable
	// initial count before Spec is changed. Once a revision becomes old, the
	// rolling-update path freezes this value.
	initialReplicas, hasInitialReplicas := disaggregatedsetutils.GetInitialReplicas(existing)
	if !hasInitialReplicas || initialReplicas != desiredReplicas {
		if err := r.LWSManager.UpdateInitialReplicas(ctx, disaggregatedSet, existing, int(desiredReplicas)); err != nil {
			return fmt.Errorf("failed to update initial replicas on LWS %s: %w", existing.Name, err)
		}
	}

	existingReplicas := int32(1)
	if existing.Spec.Replicas != nil {
		existingReplicas = *existing.Spec.Replicas
	}
	if existingReplicas != desiredReplicas {
		log.Info("Scaling LWS", "role", role, "name", existing.Name, "from", existingReplicas, "to", desiredReplicas)
		if err := r.LWSManager.Scale(ctx, disaggregatedSet, existing.Name, int(desiredReplicas)); err != nil {
			return fmt.Errorf("failed to scale LWS %s: %w", existing.Name, err)
		}
	}

	// groupReplacementPolicy is not part of the revision, so a change on the
	// role has to be pushed onto the existing LWS rather than waiting for a
	// rollout that will never come.
	if err := r.LWSManager.SyncGroupReplacementPolicy(ctx, existing, config.Spec.GroupReplacementPolicy); err != nil {
		return fmt.Errorf("failed to sync groupReplacementPolicy on LWS %s: %w", existing.Name, err)
	}

	return nil
}

// cleanupDrainedLWS deletes all LWS objects for old revisions where every role
// has been drained to 0 replicas. This ensures coordinated cleanup: we only
// delete a revision's LWS objects when ALL roles (prefill, decode, etc.) have
// finished draining, preventing partial teardown during rolling updates.
func (r *DisaggregatedSetReconciler) cleanupDrainedLWS(ctx context.Context, disaggregatedSet *disaggregatedsetv1.DisaggregatedSet, slice int, revision string) error {
	log := logf.FromContext(ctx)

	lwsList, err := r.LWSManager.List(ctx, disaggregatedSet, slice, "")
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
			if err := r.LWSManager.deleteInForeground(ctx, lws); err != nil {
				return fmt.Errorf("failed to delete LWS %s: %w", lws.Name, err)
			}
			r.Record.Eventf(disaggregatedSet, nil, corev1.EventTypeNormal, EventReasonLWSDeleted,
				"Delete", "Deleted drained LWS %s", lws.Name)
		}
	}

	return nil
}

// TODO(0.11.0): remove legacy slice-0 handling once pre-slices DisaggregatedSets are no
// longer supported: GenerateLegacyName, GetForRole's legacy-name fallback, and the
// label-less branch in SliceLabelMatches.

// SetupWithManager sets up the controller with the Manager.
func (r *DisaggregatedSetReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.LWSManager == nil {
		r.LWSManager = NewLeaderWorkerSetManager(mgr.GetClient())
	}

	if r.ScalerManager == nil {
		r.ScalerManager = NewScalerManager(mgr.GetClient(), r.Record)
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&disaggregatedsetv1.DisaggregatedSet{}).
		Owns(&leaderworkersetv1.LeaderWorkerSet{}).
		Owns(&disaggregatedsetv1.DisaggregatedSetRoleScaler{}).
		Named("disaggregatedset").
		Complete(r)
}

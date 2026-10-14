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
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"sync"
	"time"

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
	"sigs.k8s.io/lws/pkg/replicagroups"
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
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;patch
// +kubebuilder:rbac:groups=apps,resources=replicasets,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

func (r *DisaggregatedSetReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	disaggregatedSet := &disaggregatedsetv1.DisaggregatedSet{}
	if err := r.Get(ctx, req.NamespacedName, disaggregatedSet); err != nil {
		if apierrors.IsNotFound(err) {
			if r.LWSManager.pending != nil {
				r.LWSManager.pending.forget(req.NamespacedName)
			}
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

	allLWS, err := r.LWSManager.ListAll(ctx, disaggregatedSet, "")
	if err != nil {
		return ctrl.Result{}, err
	}

	// Step 1: Compute the target revision with the hash generation selected for
	// this DisaggregatedSet. Existing, unversioned objects retain the legacy hash
	// so a controller upgrade cannot trigger an otherwise unnecessary rollout.
	revision, revisionMarkerPendingObservation, err := r.resolveRevision(ctx, disaggregatedSet, allLWS)
	if err != nil {
		return ctrl.Result{}, err
	}
	if revisionMarkerPendingObservation {
		// DisaggregatedSets and their LWS children are observed by separate
		// informers. If children were created now, the LWS informer could observe
		// them before the DisaggregatedSet informer observes the annotation. The
		// next reconcile would then mistake this new set for an unversioned legacy
		// set. The annotation event will enqueue the set again once its informer
		// cache contains the marker.
		return ctrl.Result{}, nil
	}
	sliceCount := int(disaggregatedsetutils.GetSlices(disaggregatedSet))

	// Step 2: Delete LWS for slices beyond the desired count (slice scale-down).
	// Do this before reconciling retained slices so removed slices cannot
	// participate in subsequent rollout work. Per-revision drained cleanup runs
	// per retained slice in reconcileSlice.
	if err := r.cleanupRemovedSlices(ctx, disaggregatedSet, allLWS, sliceCount); err != nil {
		return ctrl.Result{}, err
	}

	// Seed a new scaler for a running role from its current aggregate replicas,
	// preventing a Static→External change from accidentally requesting zero.
	// External roles get generated scalers; scalers for roles that are no longer
	// External are deleted. Static roles do not need a scaler.
	scalers, err := r.ScalerManager.Reconcile(ctx, disaggregatedSet, r.seedForRole(ctx, disaggregatedSet))
	if errors.Is(err, errReplicaGroupsPending) {
		return ctrl.Result{RequeueAfter: time.Second}, nil
	}
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to reconcile scalers: %w", err)
	}
	// Resolve every role to an integer target: Static roles use their template,
	// while External roles use their scaler. Rollout code receives only these
	// values and does not need to know where they came from.
	desiredReplicasByRole := resolveDesiredReplicasByRole(disaggregatedSet, scalers)

	// Step 3: Reconcile LWS objects.
	executor := r.createRollingUpdateExecutor()
	roleNames := disaggregatedsetutils.GetRoleNames(disaggregatedSet)

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
		if err := r.syncMaxUnavailable(ctx, disaggregatedSet, desiredReplicasByRole); err != nil {
			return ctrl.Result{}, err
		}
		for slice := range sliceCount {
			sliceResult, err := r.reconcileSlice(ctx, executor, disaggregatedSet, slice, revision, desiredReplicasByRole)
			if errors.Is(err, errReplicaGroupsPending) {
				sliceResult, err = ctrl.Result{RequeueAfter: time.Second}, nil
			}
			if err != nil {
				errs = append(errs, fmt.Errorf("slice %d: %w", slice, err))
				continue
			}
			result = earliestRequeue(result, sliceResult)
		}
	}
	reconcileErr := errors.Join(errs...)

	// Observe status after workload writes, including slices that failed to
	// reconcile. DisaggregatedSet and scaler counts come from the same aggregate.
	if statusErr := r.updateStatus(ctx, disaggregatedSet, roleNames, revision, desiredReplicasByRole, scalers); statusErr != nil {
		return ctrl.Result{}, errors.Join(reconcileErr, fmt.Errorf("failed to update status: %w", statusErr))
	}

	return result, reconcileErr
}

// syncMaxUnavailable remembers live budgets, including policy-only updates.
// Persisting resolved counts freezes percentages for removed roles and gives
// every old-revision candidate the same allowance.
func (r *DisaggregatedSetReconciler) syncMaxUnavailable(
	ctx context.Context,
	ds *disaggregatedsetv1.DisaggregatedSet,
	desiredReplicasByRole map[string]int,
) error {
	roles, err := r.LWSManager.retainedRolloutRoleNames(ctx, ds)
	if err != nil {
		return err
	}
	encoded, err := maxUnavailableAnnotation(ds, roles, desiredReplicasByRole)
	if err != nil {
		return err
	}
	if ds.Annotations[disaggregatedsetv1.MaxUnavailableAnnotationKey] == encoded {
		return nil
	}
	patch := client.MergeFromWithOptions(ds.DeepCopy(), client.MergeFromWithOptimisticLock{})
	if ds.Annotations == nil {
		ds.Annotations = make(map[string]string)
	}
	ds.Annotations[disaggregatedsetv1.MaxUnavailableAnnotationKey] = encoded
	if err := r.Patch(ctx, ds, patch); err != nil {
		return fmt.Errorf("persisting rollout maxUnavailable budgets: %w", err)
	}
	return nil
}

func maxUnavailableAnnotation(ds *disaggregatedsetv1.DisaggregatedSet, roleNames []string, desiredReplicasByRole map[string]int) (string, error) {
	config := extractRollingUpdateConfig(ds, roleNames, desiredReplicasByRole)
	budgets := make(map[string]int, len(roleNames))
	for i, role := range roleNames {
		budgets[role] = config[i].MaxUnavailable
	}
	encoded, err := json.Marshal(budgets)
	return string(encoded), err
}

// resolveRevision selects the revision hash generation for a DisaggregatedSet.
// Legacy objects are identified by a missing revision-hash-version annotation
// and an existing owned LWS. They remain on v1 indefinitely, while a new object
// is durably marked with the current version before its first LWS is created.
// revisionMarkerPendingObservation reports that the marker was just persisted;
// the caller must stop reconciliation until the cache observes that update.
func (r *DisaggregatedSetReconciler) resolveRevision(
	ctx context.Context,
	disaggregatedSet *disaggregatedsetv1.DisaggregatedSet,
	existingLWS []*leaderworkersetv1.LeaderWorkerSet,
) (revision string, revisionMarkerPendingObservation bool, err error) {
	revisionHashVersion := disaggregatedSet.Annotations[disaggregatedsetv1.RevisionHashVersionAnnotationKey]
	switch revisionHashVersion {
	case disaggregatedsetv1.RevisionHashVersion:
		return disaggregatedsetutils.ComputeRevision(disaggregatedSet.Spec.Roles), false, nil
	case "":
		if len(existingLWS) > 0 {
			return disaggregatedsetutils.ComputeRevisionV1(disaggregatedSet.Spec.Roles), false, nil
		}

		before := disaggregatedSet.DeepCopy()
		if disaggregatedSet.Annotations == nil {
			disaggregatedSet.Annotations = make(map[string]string, 1)
		}
		disaggregatedSet.Annotations[disaggregatedsetv1.RevisionHashVersionAnnotationKey] = disaggregatedsetv1.RevisionHashVersion
		if err := r.Patch(ctx, disaggregatedSet, client.MergeFrom(before)); err != nil {
			return "", false, fmt.Errorf("failed to initialize revision hash version: %w", err)
		}
		return "", true, nil
	default:
		return "", false, fmt.Errorf("unsupported revision hash version %q", revisionHashVersion)
	}
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
func (r *DisaggregatedSetReconciler) seedForRole(ctx context.Context, ds *disaggregatedsetv1.DisaggregatedSet) func(string) (int32, error) {
	// All missing scalers in this reconcile use one lazily loaded snapshot.
	load := sync.OnceValues(func() (map[string]int32, error) {
		// Only called for a missing scaler. Do not freeze an initial target
		// from a cache that has not yet observed our preceding workload writes.
		if r.LWSManager.pending != nil {
			if err := r.LWSManager.pending.observed(ctx, ds.UID, ""); err != nil {
				return nil, err
			}
		}
		all, err := r.LWSManager.ListAll(ctx, ds, "")
		if err != nil {
			return nil, fmt.Errorf("list LWS for scaler seed: %w", err)
		}
		sums := make(map[string]int32)
		configs := disaggregatedsetutils.GetRoleConfigs(ds)
		for _, lws := range all {
			role := lws.Labels[disaggregatedsetv1.RoleLabelKey]
			sums[role] += int32(getLWSReplicas(lws))
			counts, err := subRoleCounts(lws, subRoleReplicasAnnotation)
			if err != nil {
				return nil, err
			}
			if len(counts) == 0 {
				if config := configs[role]; config != nil && len(config.SubRoles) > 0 {
					defaults := map[string]int{}
					for _, child := range config.SubRoles {
						defaults[child.Name] = int(ptr.Deref(child.Replicas, 1))
					}
					counts = fitSubRoles(defaults, int(getLWSReplicas(lws)))
				}
			}
			for child, count := range counts {
				if child == "" {
					continue // The parent total was already counted above.
				}
				key := childRoleKey(role, child)
				sums[key] += int32(count)
			}
		}
		return sums, nil
	})
	return func(role string) (int32, error) {
		sums, err := load()
		if err != nil {
			return 0, err
		}
		if count, found := sums[role]; found {
			return count, nil
		}
		return 1, nil
	}
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
	if err := r.LWSManager.prepareSlice(ctx, disaggregatedSet, slice); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.syncGroupReplacementPolicies(ctx, disaggregatedSet, slice); err != nil {
		return ctrl.Result{}, err
	}
	if r.LWSManager.pending != nil {
		if err := r.LWSManager.pending.observed(ctx, disaggregatedSet.UID, strconv.Itoa(slice)); err != nil {
			return ctrl.Result{}, err
		}
	}

	oldRevisions, newRevision, err := executor.LWSManager.GetRevisionRolesList(ctx, disaggregatedSet, slice, revision)
	if err != nil {
		return ctrl.Result{}, err
	}
	if r.LWSManager.pending != nil {
		// This also guards steady-state scaling and changes of rollout target.
		// Observe deletions before reusing capacity. A confirmed creation
		// shortfall must not prevent lowering the target or freeing quota.
		target := disaggregatedsetutils.RevisionRoles{}
		if newRevision != nil {
			target = *newRevision
		}
		if _, err := r.LWSManager.observeRolloutReadiness(ctx, oldRevisions, target); err != nil {
			if errors.Is(err, errReplicaGroupsPending) {
				// Hash replacement groups may need routing labels to start.
				// Finish that work while waiting; syncSubRoles still prevents
				// physical scaling until native observations have caught up.
				for _, rev := range append(slices.Clone(oldRevisions), target) {
					for _, lws := range rev.Roles {
						if lws.Spec.GroupIdentity != leaderworkersetv1.GroupIdentityHash {
							continue
						}
						if _, syncErr := r.LWSManager.syncSubRoles(ctx, disaggregatedSet, lws, subRoleUpdate{}); syncErr != nil && !errors.Is(syncErr, errReplicaGroupsPending) {
							return ctrl.Result{}, syncErr
						}
					}
				}
			}
			return ctrl.Result{}, err
		}
	}

	// Keep the rolling-update path active while any old revision exists, even
	// after its Spec reaches zero. The executor must observe that terminal state
	// so it can wait for the target revision to become Ready before declaring
	// the rollout complete. The old objects are deleted only after that check.
	var result ctrl.Result
	if len(oldRevisions) > 0 {
		var complete bool
		result, complete, err = executor.ReconcileRevisionTransition(ctx, disaggregatedSet, slice, revision, oldRevisions, newRevision, desiredReplicasByRole)
		if err != nil {
			return result, err
		}
		if err := r.cleanupDrainedLWS(ctx, disaggregatedSet, slice, revision, complete); err != nil {
			return result, err
		}
	} else {
		result, err = r.reconcileCurrentRevision(ctx, disaggregatedSet, slice, revision, newRevision, desiredReplicasByRole)
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

func desiredSubRoles(role *disaggregatedsetv1.DisaggregatedRoleSpec, desired map[string]int) map[string]int {
	counts := map[string]int{}
	for _, child := range role.SubRoles {
		counts[child.Name] = desired[childRoleKey(role.Name, child.Name)]
	}
	return counts
}

func (r *DisaggregatedSetReconciler) reconcileCurrentRevision(ctx context.Context, disaggregatedSet *disaggregatedsetv1.DisaggregatedSet, slice int, revision string, current *disaggregatedsetutils.RevisionRoles, desiredReplicasByRole map[string]int) (ctrl.Result, error) {
	roleConfigs := disaggregatedsetutils.GetRoleConfigs(disaggregatedSet)
	result := ctrl.Result{}

	for role, config := range roleConfigs {
		// Reuse the observed slice list. If policy sync has not reached the
		// informer yet, the write receipt blocks a second mutation of that LWS.
		var existing *leaderworkersetv1.LeaderWorkerSet
		if current != nil {
			existing = current.Roles[role]
		}
		if err := r.reconcileCurrentRevisionRole(ctx, disaggregatedSet, slice, existing, config, revision, desiredReplicasByRole); err != nil {
			if errors.Is(err, errReplicaGroupsPending) {
				result.RequeueAfter = time.Second
				continue
			}
			return ctrl.Result{}, fmt.Errorf("failed to reconcile %s role: %w", role, err)
		}
	}

	return result, nil
}

func (r *DisaggregatedSetReconciler) reconcileCurrentRevisionRole(ctx context.Context, disaggregatedSet *disaggregatedsetv1.DisaggregatedSet, slice int, existing *leaderworkersetv1.LeaderWorkerSet, config *disaggregatedsetv1.DisaggregatedRoleSpec, revision string, desiredReplicasByRole map[string]int) error {
	log := logf.FromContext(ctx)

	role := config.Name
	desiredReplicas := int32(desiredReplicasByRole[role])

	// Create an empty LWS first. Only a subsequent observed object can be
	// conditionally scaled to the latest desired size.
	if existing == nil {
		return r.LWSManager.Create(ctx, disaggregatedSet, config, slice, revision, int(desiredReplicas))
	}
	counts := desiredSubRoles(config, desiredReplicasByRole)
	// Outside rollout, membership, issued target and effective baseline all follow desired.
	state, err := r.LWSManager.syncSubRoles(ctx, disaggregatedSet, existing, subRoleUpdate{membership: counts, target: counts, initial: counts})
	if err != nil || state != nil {
		// A non-nil state means subrole reconciliation owns this LWS's scaling.
		return err
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
		if err := r.LWSManager.Scale(ctx, disaggregatedSet, existing, int(desiredReplicas)); err != nil {
			return fmt.Errorf("failed to scale LWS %s: %w", existing.Name, err)
		}
	}

	return nil
}

// syncGroupReplacementPolicies pushes each role's groupReplacementPolicy onto
// its Hash LWS, old revisions included. DS rollouts never replace groups in
// place, so the policy only affects recovery and applies mid-rollout too.
func (r *DisaggregatedSetReconciler) syncGroupReplacementPolicies(ctx context.Context, disaggregatedSet *disaggregatedsetv1.DisaggregatedSet, slice int) error {
	roleConfigs := disaggregatedsetutils.GetRoleConfigs(disaggregatedSet)
	lwsList, err := r.LWSManager.ListForSlice(ctx, disaggregatedSet, slice, "")
	if err != nil {
		return fmt.Errorf("failed to list LWS for groupReplacementPolicy sync: %w", err)
	}
	for _, lws := range lwsList {
		if lws.Spec.GroupIdentity != leaderworkersetv1.GroupIdentityHash {
			continue
		}
		config, ok := roleConfigs[lws.Labels[disaggregatedsetv1.RoleLabelKey]]
		if !ok {
			continue
		}
		if err := r.LWSManager.SyncGroupReplacementPolicy(ctx, lws, config.Spec.GroupReplacementPolicy); err != nil {
			return fmt.Errorf("failed to sync groupReplacementPolicy on LWS %s: %w", lws.Name, err)
		}
	}
	return nil
}

// cleanupDrainedLWS bounds retained rollout history without losing the marker
// that keeps an unready target on the rollout path. A partially drained
// revision is never deleted here.
func (r *DisaggregatedSetReconciler) cleanupDrainedLWS(
	ctx context.Context,
	disaggregatedSet *disaggregatedsetv1.DisaggregatedSet,
	slice int,
	revision string,
	rolloutComplete bool,
) error {
	log := logf.FromContext(ctx)

	lwsList, err := r.LWSManager.ListForSlice(ctx, disaggregatedSet, slice, "")
	if err != nil {
		return fmt.Errorf("failed to list LWS for cleanup: %w", err)
	}

	oldLWS := make([]*leaderworkersetv1.LeaderWorkerSet, 0, len(lwsList))
	for _, lws := range lwsList {
		lwsRevision := lws.Labels[disaggregatedsetv1.RevisionLabelKey]
		if lwsRevision == revision || !lws.DeletionTimestamp.IsZero() {
			continue
		}
		oldLWS = append(oldLWS, lws)
	}

	oldRevisions := disaggregatedsetutils.GroupByRevision(oldLWS).SortedByNewestTimestamp()
	hasNonDrainedRevision := false
	drainedRevisions := make(disaggregatedsetutils.RevisionRolesList, 0, len(oldRevisions))
	for _, oldRevision := range oldRevisions {
		allDrained := true
		for _, lws := range oldRevision.Roles {
			if getLWSReplicas(lws) != 0 {
				allDrained = false
				break
			}
		}
		if allDrained {
			drainedRevisions = append(drainedRevisions, oldRevision)
		} else {
			hasNonDrainedRevision = true
		}
	}

	retainedRevision := ""
	if !rolloutComplete && !hasNonDrainedRevision && len(drainedRevisions) > 0 {
		retainedRevision = drainedRevisions[0].Revision
		log.V(1).Info("Retaining newest drained revision until the target is ready", "revision", retainedRevision)
	}

	for _, drainedRevision := range drainedRevisions {
		if drainedRevision.Revision == retainedRevision {
			continue
		}
		for _, lws := range drainedRevision.Roles {
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

func disaggregatedSetRequest(lws *leaderworkersetv1.LeaderWorkerSet) (ctrl.Request, bool) {
	owner := metav1.GetControllerOf(lws)
	if owner == nil || owner.Kind != "DisaggregatedSet" || owner.APIVersion != disaggregatedsetv1.GroupVersion.String() {
		return ctrl.Request{}, false
	}
	return ctrl.Request{NamespacedName: client.ObjectKey{Namespace: lws.Namespace, Name: owner.Name}}, true
}

// SetupWithManager sets up the controller with the Manager.
func (r *DisaggregatedSetReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.LWSManager == nil {
		r.LWSManager = NewLeaderWorkerSetManager(mgr.GetClient())
	}
	observer, err := replicagroups.NewObserver(mgr)
	if err != nil {
		return err
	}
	if err := mgr.Add(observer); err != nil {
		return err
	}
	r.LWSManager.reader = observer
	r.LWSManager.pending = &pendingWrites{Reader: observer, writes: make(map[client.ObjectKey]*pendingWrite)}

	if r.ScalerManager == nil {
		r.ScalerManager = NewScalerManager(mgr.GetClient(), r.Record)
	}

	// LWS changes arrive through the observer, after its own cache is updated.
	// A second Owns watch would wake us against a potentially older observation.
	return ctrl.NewControllerManagedBy(mgr).
		For(&disaggregatedsetv1.DisaggregatedSet{}).
		Owns(&disaggregatedsetv1.DisaggregatedSetRoleScaler{}).
		WatchesRawSource(observer.Source(disaggregatedSetRequest)).
		Named("disaggregatedset").
		Complete(r)
}

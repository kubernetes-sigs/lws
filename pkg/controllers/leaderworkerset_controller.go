/*
Copyright 2023.

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

package controllers

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	schedulingv1beta1 "k8s.io/api/scheduling/v1beta1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	appsapplyv1 "k8s.io/client-go/applyconfigurations/apps/v1"
	coreapplyv1 "k8s.io/client-go/applyconfigurations/core/v1"
	metaapplyv1 "k8s.io/client-go/applyconfigurations/meta/v1"
	"k8s.io/client-go/tools/events"
	"k8s.io/klog/v2"
	"k8s.io/utils/lru"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	leaderworkerset "sigs.k8s.io/lws/api/leaderworkerset/v1"
	"sigs.k8s.io/lws/pkg/schedulerprovider"
	"sigs.k8s.io/lws/pkg/utils"
	controllerutils "sigs.k8s.io/lws/pkg/utils/controller"
	disaggregatedsetutils "sigs.k8s.io/lws/pkg/utils/disaggregatedset"
	podutils "sigs.k8s.io/lws/pkg/utils/pod"
	revisionutils "sigs.k8s.io/lws/pkg/utils/revision"
	statefulsetutils "sigs.k8s.io/lws/pkg/utils/statefulset"
)

// LeaderWorkerSetReconciler reconciles a LeaderWorkerSet object
type LeaderWorkerSetReconciler struct {
	client.Client
	Scheme            *runtime.Scheme
	Record            events.EventRecorder
	SchedulerProvider schedulerprovider.SchedulerProvider

	apiReader             client.Reader // ReplicaSet lookups must not start a cluster-wide informer.
	revisionEqualityCache *lru.Cache
}

var (
	apiGVStr = leaderworkerset.GroupVersion.String()
)

const (
	lwsOwnerKey  = ".metadata.controller"
	fieldManager = "lws"
)

const (
	// FailedCreate Event reason used when a resource creation fails.
	// The event uses the error(s) as the reason.
	FailedCreate      = "FailedCreate"
	GroupsProgressing = "GroupsProgressing"
	GroupsUpdating    = "GroupsUpdating"
	CreatingRevision  = "CreatingRevision"
	FailedUpdate      = "FailedUpdate"
	// Group replacement event reasons, emitted by the pod controller when a
	// gated hash-identity leader is held back or admitted for scheduling.
	GroupReplacementDeferred = "GroupReplacementDeferred"
	GroupReplacementAdmitted = "GroupReplacementAdmitted"
	// GroupUnschedulable reports a group with a member the scheduler could not
	// place, for example a gang waiting for capacity.
	GroupUnschedulable = "GroupUnschedulable"

	// GroupRestartBackoff indicates that group recreation is delayed by restart backoff.
	GroupRestartBackoff = "GroupRestartBackoff"

	// UnexpectedPodGroupOwner indicates that PodGroup ownership prevents reconciliation.
	UnexpectedPodGroupOwner = "UnexpectedPodGroupOwner"

	// Event actions
	Create = "Create"
	Update = "Update"
	Delete = "Delete"
)

// maxRevisionEqualityCacheEntries is the cache size for semantic revision equality results.
const maxRevisionEqualityCacheEntries = 10_000

func NewLeaderWorkerSetReconciler(client client.Client, scheme *runtime.Scheme, record events.EventRecorder, providers ...schedulerprovider.SchedulerProvider) *LeaderWorkerSetReconciler {
	var provider schedulerprovider.SchedulerProvider
	if len(providers) > 0 {
		provider = providers[0]
	}
	return &LeaderWorkerSetReconciler{
		Client:                client,
		apiReader:             client, // SetupWithManager supplies the uncached reader in production.
		Scheme:                scheme,
		Record:                record,
		SchedulerProvider:     provider,
		revisionEqualityCache: lru.New(maxRevisionEqualityCacheEntries),
	}
}

//+kubebuilder:rbac:groups="",resources=events,verbs=create;watch;update;patch;get;list
//+kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;watch;update;patch;get;list
//+kubebuilder:rbac:groups=leaderworkerset.x-k8s.io,resources=leaderworkersets,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=leaderworkerset.x-k8s.io,resources=leaderworkersets/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=leaderworkerset.x-k8s.io,resources=leaderworkersets/finalizers,verbs=update
//+kubebuilder:rbac:groups=apps,resources=statefulsets,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=apps,resources=statefulsets/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=apps,resources=statefulsets/finalizers,verbs=update
//+kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=apps,resources=deployments/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=apps,resources=replicasets,verbs=get
//+kubebuilder:rbac:groups=core,resources=services,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=apps,resources=controllerrevisions,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=apps,resources=controllerrevisions/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=apps,resources=controllerrevisions/finalizers,verbs=update
//+kubebuilder:rbac:groups=scheduling.k8s.io,resources=workloads;podgroups,verbs=get;list;watch;create;update;patch;delete

func (r *LeaderWorkerSetReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	// Get leaderworkerset object
	lws := &leaderworkerset.LeaderWorkerSet{}
	if err := r.Get(ctx, types.NamespacedName{Name: req.Name, Namespace: req.Namespace}, lws); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if lws.DeletionTimestamp != nil {
		if lws.Spec.PodTerminationPolicy == leaderworkerset.ParallelPodTerminationPolicy {
			if err := r.deleteWorkerStatefulSets(ctx, lws); err != nil {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{}, nil
	}

	log := ctrl.LoggerFrom(ctx).WithValues("leaderworkerset", klog.KObj(lws))
	ctx = ctrl.LoggerInto(ctx, log)

	if lws.Spec.GroupIdentity == leaderworkerset.GroupIdentityHash {
		return r.reconcileHash(ctx, lws)
	}

	leaderSts, err := r.getLeaderStatefulSet(ctx, lws)
	if err != nil {
		log.Error(err, "Fetching leader statefulset")
		return ctrl.Result{}, err
	}

	if leaderSts != nil && leaderSts.DeletionTimestamp != nil {
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	// Handles two cases:
	// Case 1: Upgrading the LWS controller from a version that doesn't support controller revision
	// Case 2: Creating the controller revision for a newly created LWS object
	revisionKey := ""
	if leaderSts != nil {
		// Uses the hash in the leader sts to avoid detecting update in the case where LWS controller is upgraded from a version where
		// the revisionKey was used to detect update instead of controller revision.
		revisionKey = revisionutils.GetRevisionKey(leaderSts)
	}
	revision, err := r.getOrCreateRevision(ctx, revisionKey, lws)
	if err != nil {
		log.Error(err, "Creating controller revision")
		return ctrl.Result{}, err
	}

	var updatedRevision *appsv1.ControllerRevision
	if leaderSts != nil {
		updatedRevision, err = r.getUpdatedRevision(ctx, lws, revision)
		if err != nil {
			log.Error(err, "Validating if LWS has been updated")
			return ctrl.Result{}, err
		}
	}
	lwsUpdated := updatedRevision != nil
	if lwsUpdated {
		revision, err = revisionutils.CreateRevision(ctx, r.Client, updatedRevision)
		if err != nil {
			log.Error(err, "Creating revision for updated LWS")
			return ctrl.Result{}, err
		}
		r.Record.Eventf(lws, revision, corev1.EventTypeNormal, CreatingRevision, Create, fmt.Sprintf("Creating revision with key %s for updated LWS", revisionutils.GetRevisionKey(revision)))
	}

	partition, replicas, err := r.rollingUpdateParameters(ctx, lws, leaderSts, revisionutils.GetRevisionKey(revision), lwsUpdated)
	if err != nil {
		log.Error(err, "Rolling partition error")
		return ctrl.Result{}, err
	}
	if err := r.pruneScaledDownGroupRestartCounts(ctx, lws, replicas); err != nil {
		return ctrl.Result{}, err
	}

	if err := r.reconcileWorkloadScheduling(ctx, lws, replicas, revisionutils.GetRevisionKey(revision)); err != nil {
		var scaleDown func() error
		var updateStatus func() (bool, error)
		if leaderSts != nil {
			// replicas is the target of the rollout, which includes the surge
			// replicas, so a scheduling error during a rollout keeps them.
			scaleDown = func() error { return r.scaleDownLeaderStatefulSet(ctx, leaderSts, replicas) }
			updateStatus = func() (bool, error) { return r.updateStatus(ctx, lws, revisionutils.GetRevisionKey(revision)) }
		}
		return r.handleWorkloadSchedulingError(ctx, err, scaleDown, updateStatus)
	}

	if err := r.SSAWithStatefulset(ctx, lws, partition, replicas, revisionutils.GetRevisionKey(revision)); err != nil {
		if leaderSts == nil {
			r.Record.Eventf(lws, nil, corev1.EventTypeWarning, FailedCreate, Create, fmt.Sprintf("Failed to create leader statefulset %s: %v", lws.Name, err))
		} else {
			r.Record.Eventf(lws, nil, corev1.EventTypeWarning, FailedUpdate, Update, fmt.Sprintf("Failed to update leader statefulset %s: %v", lws.Name, err))
		}
		return ctrl.Result{}, err
	}

	if leaderSts == nil {
		// An event is logged to track sts creation.
		r.Record.Eventf(lws, revision, corev1.EventTypeNormal, GroupsProgressing, Create, fmt.Sprintf("Created leader statefulset %s", lws.Name))
	} else if !lwsUpdated && partition != *leaderSts.Spec.UpdateStrategy.RollingUpdate.Partition {
		// An event is logged to track update progress.
		oldPartition := *leaderSts.Spec.UpdateStrategy.RollingUpdate.Partition
		var updateMsg string
		if oldPartition-1 == partition {
			updateMsg = fmt.Sprintf("Updating replica %d", partition)
		} else {
			updateMsg = fmt.Sprintf("Updating replicas %d to %d (inclusive)", partition, oldPartition-1)
		}
		r.Record.Eventf(lws, revision, corev1.EventTypeNormal, GroupsUpdating, updateMsg, updateMsg)
	}

	if err := r.reconcileHeadlessServices(ctx, lws); err != nil {
		log.Error(err, "Creating headless service.")
		r.Record.Eventf(lws, nil, corev1.EventTypeWarning, FailedCreate, Create, fmt.Sprintf("Failed to create headless service for error: %v", err))
		return ctrl.Result{}, err
	}

	updateDone, err := r.updateStatus(ctx, lws, revisionutils.GetRevisionKey(revision))
	if err != nil {
		if apierrors.IsConflict(err) {
			return ctrl.Result{Requeue: true}, nil
		}
		return ctrl.Result{}, err
	}

	if updateDone {
		if err := revisionutils.TruncateRevisions(ctx, r.Client, lws, revisionutils.GetRevisionKey(revision)); err != nil {
			return ctrl.Result{}, err
		}
		if err := r.cleanupObsoleteGroupRestartCounts(ctx, lws, revisionutils.GetRevisionKey(revision)); err != nil {
			return ctrl.Result{}, err
		}
	}
	log.V(2).Info("Leader Reconcile completed.")
	return ctrl.Result{}, nil
}

func (r *LeaderWorkerSetReconciler) pruneScaledDownGroupRestartCounts(ctx context.Context, lws *leaderworkerset.LeaderWorkerSet, activeReplicas int32) error {
	if lws.Annotations == nil || lws.Annotations[leaderworkerset.GroupRestartCountsAnnotationKey] == "" {
		return nil
	}
	return mutateGroupRestartCounts(ctx, r.Client, client.ObjectKeyFromObject(lws), func(latest *leaderworkerset.LeaderWorkerSet, counts map[string]int32) (bool, error) {
		changed := false
		// activeReplicas includes temporary MaxSurge ordinals. Do not discard
		// their counters while those groups are still part of the StatefulSet.
		replicaLimit := max(int64(activeReplicas), int64(*latest.Spec.Replicas))
		for key := range counts {
			separator := strings.LastIndexByte(key, '/')
			if separator < 0 {
				return false, fmt.Errorf("invalid group restart count key %q", key)
			}
			groupIndex, err := strconv.ParseInt(key[separator+1:], 10, 32)
			if err != nil {
				return false, fmt.Errorf("invalid group restart count key %q: %w", key, err)
			}
			if groupIndex >= replicaLimit {
				delete(counts, key)
				changed = true
			}
		}
		return changed, nil
	})
}

func (r *LeaderWorkerSetReconciler) cleanupObsoleteGroupRestartCounts(ctx context.Context, lws *leaderworkerset.LeaderWorkerSet, revisionKey string) error {
	return mutateGroupRestartCounts(ctx, r.Client, client.ObjectKeyFromObject(lws), func(_ *leaderworkerset.LeaderWorkerSet, counts map[string]int32) (bool, error) {
		changed := false
		for key := range counts {
			separator := strings.LastIndexByte(key, '/')
			if separator < 0 {
				return false, fmt.Errorf("invalid group restart count key %q", key)
			}
			if key[:separator] != revisionKey {
				delete(counts, key)
				changed = true
			}
		}
		return changed, nil
	})
}

func (r *LeaderWorkerSetReconciler) reconcileHeadlessServices(ctx context.Context, lws *leaderworkerset.LeaderWorkerSet) error {
	if lws.Spec.NetworkConfig == nil || *lws.Spec.NetworkConfig.SubdomainPolicy == leaderworkerset.SubdomainShared {
		if err := controllerutils.CreateHeadlessServiceIfNotExists(ctx, r.Client, r.Scheme, lws, lws.Name, map[string]string{leaderworkerset.SetNameLabelKey: lws.Name}, lws); err != nil {
			return err
		}
	}
	return nil
}

// reconcileWorkloadScheduling creates the workload-aware scheduling
// prerequisites of an opted-in LeaderWorkerSet and records the outcome on the
// WorkloadSchedulingCreated condition. Both group identity modes run it before
// the leader workload is applied, so no member pod is created before its
// Workload and, for the levels the controller can enumerate, its PodGroups.
func (r *LeaderWorkerSetReconciler) reconcileWorkloadScheduling(ctx context.Context, lws *leaderworkerset.LeaderWorkerSet, replicas int32, revisionKey string) error {
	if lws.Spec.Scheduling == nil {
		return nil
	}
	if r.SchedulerProvider == nil {
		return r.failWorkloadScheduling(ctx, lws, schedulerprovider.ReasonUnsupportedProviderCapability,
			schedulerprovider.NewReconcileError(schedulerprovider.ReasonUnsupportedProviderCapability,
				fmt.Errorf("spec.scheduling requires a configured scheduler provider")))
	}
	if err := r.SchedulerProvider.ReconcileScheduling(ctx, lws, replicas, revisionKey); err != nil {
		return r.failWorkloadScheduling(ctx, lws, schedulerprovider.ReconcileErrorReason(err), err)
	}
	uid, generation, resourceVersion := lws.UID, lws.Generation, lws.ResourceVersion
	if err := r.updateWorkloadSchedulingCondition(ctx, lws, metav1.ConditionTrue, "SchedulingPrerequisitesCreated", "scheduling prerequisites created"); err != nil {
		if !apierrors.IsConflict(err) {
			return err
		}
		return r.refreshAfterSchedulingConditionConflict(ctx, lws, uid, generation, resourceVersion, err)
	}
	return nil
}

// errLeaderWorkerSetChanged is returned by reconcileWorkloadScheduling when the
// LeaderWorkerSet changed after the reconcile read it and the spec cannot be
// verified as unchanged. The rest of the reconcile must not act on the stale
// object, since it could, for example, create pods at an old replica count.
var errLeaderWorkerSetChanged = errors.New("LeaderWorkerSet changed during reconcile")

// refreshAfterSchedulingConditionConflict handles a conflict on the
// WorkloadSchedulingCreated=True write, which means the LWS changed since it
// was read as uid at generation and resourceVersion, possibly its spec. The LWS
// is read again: when the read observes the change and it is the same object
// at the same generation, lws is replaced by the latest object, which the rest
// of the reconcile uses, and the condition write is retried once. Otherwise,
// including when the retry conflicts again, errLeaderWorkerSetChanged is
// returned.
func (r *LeaderWorkerSetReconciler) refreshAfterSchedulingConditionConflict(ctx context.Context, lws *leaderworkerset.LeaderWorkerSet, uid types.UID, generation int64, resourceVersion string, conflictErr error) error {
	log := ctrl.LoggerFrom(ctx)
	latest := &leaderworkerset.LeaderWorkerSet{}
	if err := r.Get(ctx, client.ObjectKeyFromObject(lws), latest); err != nil {
		return errors.Join(conflictErr, err)
	}
	// An unchanged resource version means the cache has not observed the
	// change yet, so the spec cannot be verified. A different UID means the
	// LWS was recreated, so the prerequisites belong to the old object.
	if latest.UID != uid || latest.Generation != generation || latest.ResourceVersion == resourceVersion {
		log.V(2).Info("Stopping the reconcile after a WorkloadSchedulingCreated condition conflict", "error", conflictErr)
		return errLeaderWorkerSetChanged
	}
	// The scheduling prerequisites were created for this generation.
	*lws = *latest
	if err := r.updateWorkloadSchedulingCondition(ctx, lws, metav1.ConditionTrue, "SchedulingPrerequisitesCreated", "scheduling prerequisites created"); err != nil {
		if !apierrors.IsConflict(err) {
			return err
		}
		// The LWS changed again, possibly its spec, since it was read.
		log.V(2).Info("Stopping the reconcile after a repeated WorkloadSchedulingCreated condition conflict", "error", err)
		return errLeaderWorkerSetChanged
	}
	return nil
}

func (r *LeaderWorkerSetReconciler) failWorkloadScheduling(ctx context.Context, lws *leaderworkerset.LeaderWorkerSet, reason string, reconcileErr error) error {
	if r.Record != nil {
		r.Record.Eventf(lws, nil, corev1.EventTypeWarning, reason, "Reconcile", reconcileErr.Error())
	}
	statusErr := r.updateWorkloadSchedulingCondition(ctx, lws, metav1.ConditionFalse, reason, reconcileErr.Error())
	return errors.Join(reconcileErr, statusErr)
}

func (r *LeaderWorkerSetReconciler) updateWorkloadSchedulingCondition(ctx context.Context, lws *leaderworkerset.LeaderWorkerSet, status metav1.ConditionStatus, reason, message string) error {
	changed := apimeta.SetStatusCondition(&lws.Status.Conditions, metav1.Condition{
		Type:               string(leaderworkerset.LeaderWorkerSetWorkloadSchedulingCreated),
		Status:             status,
		ObservedGeneration: lws.Generation,
		Reason:             reason,
		Message:            message,
	})
	if !changed {
		return nil
	}
	if err := r.Status().Update(ctx, lws); err != nil {
		return fmt.Errorf("update WorkloadSchedulingCreated condition: %w", err)
	}
	return nil
}

// handleWorkloadSchedulingError handles an error of reconcileWorkloadScheduling.
// Scale-up and rollouts stay blocked, since new leader pods need the scheduling
// prerequisites, whether the error is retryable or not. Scale-down and status
// updates need none of them, so they still run: scaleDown and updateStatus are
// nil when the leader workload does not exist yet. The error is returned so
// the reconcile is retried. errLeaderWorkerSetChanged instead stops the
// reconcile without acting on the stale object, and requeues it.
func (r *LeaderWorkerSetReconciler) handleWorkloadSchedulingError(
	ctx context.Context,
	err error,
	scaleDown func() error,
	updateStatus func() (bool, error),
) (ctrl.Result, error) {
	if errors.Is(err, errLeaderWorkerSetChanged) {
		// The watch event of the spec change triggers another reconcile.
		return ctrl.Result{Requeue: true}, nil
	}
	ctrl.LoggerFrom(ctx).Error(err, "Reconciling workload-aware scheduling prerequisites")
	if scaleDown != nil {
		if scaleErr := scaleDown(); scaleErr != nil {
			return ctrl.Result{}, errors.Join(err, scaleErr)
		}
	}
	if updateStatus != nil {
		if _, statusErr := updateStatus(); statusErr != nil && !apierrors.IsConflict(statusErr) {
			return ctrl.Result{}, errors.Join(err, statusErr)
		}
	}
	return ctrl.Result{}, err
}

func (r *LeaderWorkerSetReconciler) scaleDownLeaderStatefulSet(ctx context.Context, leaderSts *appsv1.StatefulSet, desiredReplicas int32) error {
	if leaderSts.Spec.Replicas == nil || desiredReplicas >= *leaderSts.Spec.Replicas {
		return nil
	}
	leaderSts.Spec.Replicas = ptr.To(desiredReplicas)
	return r.Update(ctx, leaderSts)
}

// SetupWithManager sets up the controller with the Manager.
func (r *LeaderWorkerSetReconciler) SetupWithManager(mgr ctrl.Manager) error {
	r.apiReader = mgr.GetAPIReader()
	builder := ctrl.NewControllerManagedBy(mgr).
		For(&leaderworkerset.LeaderWorkerSet{}).
		Owns(&appsv1.StatefulSet{}).
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.Service{}).
		Watches(&corev1.Pod{},
			handler.EnqueueRequestsFromMapFunc(enqueueLWSRequests),
			// Reconcile status when an exhausted group appears or is removed. Ignore
			// unrelated kubelet status updates on healthy leader pods. Member pod
			// deletions trigger reconciliation so obsolete PodGroups can be cleaned up.
			builder.WithPredicates(r.podWatchPredicate())).
		Watches(&appsv1.StatefulSet{},
			handler.EnqueueRequestsFromMapFunc(enqueueLWSRequests))
	// Avoid starting informers for APIs that do not exist on pre-1.37 clusters.
	// Upstream Workload resources are watched only when that provider is active.
	if _, ok := r.SchedulerProvider.(*schedulerprovider.KubernetesProvider); ok {
		builder = builder.
			Owns(&schedulingv1beta1.Workload{}).
			Owns(&schedulingv1beta1.PodGroup{})
	}
	return builder.Complete(r)
}

func (r *LeaderWorkerSetReconciler) podWatchPredicate() predicate.Funcs {
	return predicate.Funcs{
		CreateFunc: func(e event.CreateEvent) bool {
			return e.Object != nil && e.Object.GetLabels()[leaderworkerset.WorkerIndexLabelKey] == "0"
		},
		DeleteFunc: func(e event.DeleteEvent) bool {
			if e.Object == nil {
				return false
			}
			if e.Object.GetLabels()[leaderworkerset.WorkerIndexLabelKey] == "0" {
				return true
			}
			if _, ok := r.SchedulerProvider.(*schedulerprovider.KubernetesProvider); !ok {
				return false
			}
			pod, ok := e.Object.(*corev1.Pod)
			return (ok && pod.Spec.SchedulingGroup != nil && pod.Spec.SchedulingGroup.PodGroupName != nil) ||
				e.Object.GetAnnotations()[schedulerprovider.WorkloadSchedulingAnnotationKey] != ""
		},
		UpdateFunc: func(e event.UpdateEvent) bool {
			if e.ObjectNew == nil || e.ObjectNew.GetLabels()[leaderworkerset.WorkerIndexLabelKey] != "0" {
				return false
			}
			if e.ObjectOld == nil {
				return false
			}
			// Only leader pods get this far. The Progressing reason reports
			// unschedulable leaders, and a scheduling result changes no StatefulSet
			// status that would otherwise requeue the LWS.
			oldPod, oldOK := e.ObjectOld.(*corev1.Pod)
			newPod, newOK := e.ObjectNew.(*corev1.Pod)
			if oldOK && newOK && podutils.PodUnschedulable(*oldPod) != podutils.PodUnschedulable(*newPod) {
				return true
			}
			return e.ObjectOld.GetAnnotations()[leaderworkerset.GroupRestartBudgetExhaustedAnnotationKey] !=
				e.ObjectNew.GetAnnotations()[leaderworkerset.GroupRestartBudgetExhaustedAnnotationKey]
		},
		GenericFunc: func(e event.GenericEvent) bool {
			return false
		},
	}
}

func enqueueLWSRequests(ctx context.Context, a client.Object) []reconcile.Request {
	name := a.GetLabels()[leaderworkerset.SetNameLabelKey]
	if name == "" {
		return nil
	}
	return []reconcile.Request{
		{NamespacedName: types.NamespacedName{
			Name:      name,
			Namespace: a.GetNamespace(),
		}},
	}
}

func SetupIndexes(indexer client.FieldIndexer) error {
	return indexer.IndexField(context.Background(), &appsv1.StatefulSet{}, lwsOwnerKey, func(rawObj client.Object) []string {
		// grab the statefulSet object, extract the owner...
		statefulSet := rawObj.(*appsv1.StatefulSet)
		owner := metav1.GetControllerOf(statefulSet)
		if owner == nil {
			return nil
		}
		// ...make sure it's a LeaderWorkerSet...
		if owner.APIVersion != apiGVStr || owner.Kind != "LeaderWorkerSet" {
			return nil
		}
		// ...and if so, return it
		return []string{owner.Name}
	})
}

// Rolling update will always wait for the former replica to be ready then process the next one,
// we didn't consider rollout strategy type here since we only support rollingUpdate now,
// once we have more policies, we should update the logic here.
// Possible scenarios for Partition:
//   - When sts is under creation, partition is always 0 because pods are created in parallel, rolling update is not relevant here.
//   - When sts is in rolling update, the partition will start from the last index to the index 0 processing in maxUnavailable step.
//   - When sts is in rolling update, and Replicas increases, we'll delay the rolling update until the scaling up is done,
//     Partition will not change, new replicas are created using the new template from the get go.
//   - When sts is rolling update, and Replicas decreases, the partition will not change until new Replicas < Partition,
//     in which case Partition will be reset to the new Replicas value.
//   - When sts is ready for a rolling update and Replicas increases at the same time, we'll delay the rolling update until
//     the scaling up is done.
//   - When sts is ready for a rolling update and Replicas decreases at the same time, we'll start the rolling update
//     together with scaling down.
//
// At rest, Partition should always be zero.
//
// For Replicas:
//   - When rolling update, Replicas is equal to (spec.Replicas+maxSurge)
//   - Otherwise, Replicas is equal to spec.Replicas
//   - One exception here is when unready replicas of leaderWorkerSet is equal to MaxSurge,
//     we should reclaim the extra replicas gradually to accommodate for the new replicas.
func (r *LeaderWorkerSetReconciler) rollingUpdateParameters(ctx context.Context, lws *leaderworkerset.LeaderWorkerSet, sts *appsv1.StatefulSet, revisionKey string, leaderWorkerSetUpdated bool) (stsPartition int32, replicas int32, err error) {
	log := ctrl.LoggerFrom(ctx).WithValues("leaderworkerset", klog.KObj(lws))
	ctx = ctrl.LoggerInto(ctx, log)
	lwsReplicas := *lws.Spec.Replicas

	defer func() {
		// Limit the replicas with less than lwsPartition will not be updated.
		stsPartition = max(stsPartition, *lws.Spec.RolloutStrategy.RollingUpdateConfiguration.Partition)
	}()

	// Case 1:
	// If sts not created yet, all partitions should be updated,
	// replicas should not change.
	if sts == nil {
		return 0, lwsReplicas, nil
	}

	stsReplicas := *sts.Spec.Replicas
	maxSurge, err := intstr.GetScaledValueFromIntOrPercent(&lws.Spec.RolloutStrategy.RollingUpdateConfiguration.MaxSurge, int(lwsReplicas), true)
	if err != nil {
		return 0, 0, err
	}
	maxUnavailable, err := intstr.GetScaledValueFromIntOrPercent(&lws.Spec.RolloutStrategy.RollingUpdateConfiguration.MaxUnavailable, int(lwsReplicas), false)
	if err != nil {
		return 0, 0, err
	}
	// No need to burst more than the replicas.
	if maxSurge > int(lwsReplicas) {
		maxSurge = int(lwsReplicas)
	}
	burstReplicas := lwsReplicas + int32(maxSurge)

	// wantReplicas calculates the final replicas if needed.
	wantReplicas := func(unreadyReplicas int32) int32 {
		finalReplicas := calculateRollingUpdateReplicas(lwsReplicas, int32(maxSurge), int32(maxUnavailable), unreadyReplicas)
		if finalReplicas == stsReplicas-1 {
			r.Record.Eventf(lws, nil, corev1.EventTypeNormal, GroupsProgressing, Delete, fmt.Sprintf("deleting surge replica %s-%d", lws.Name, finalReplicas))
		} else if finalReplicas < stsReplicas {
			r.Record.Eventf(lws, nil, corev1.EventTypeNormal, GroupsProgressing, Delete, fmt.Sprintf("deleting surge replicas from %s-%d to %s-%d", lws.Name, finalReplicas, lws.Name, stsReplicas-1))
		}
		return finalReplicas
	}

	// Case 2:
	// Indicates a new rolling update here.
	if leaderWorkerSetUpdated {
		// Processing scaling up/down first prior to rolling update.
		partition := min(lwsReplicas, stsReplicas)
		if stsReplicas < lwsReplicas {
			return partition, lwsReplicas, nil
		}
		return partition, wantReplicas(lwsReplicas), nil
	}

	partition := *sts.Spec.UpdateStrategy.RollingUpdate.Partition
	// DisaggregatedSet approves child reductions using availability across revisions.
	// Do not invent native surge for its replica-only updates. The previous desired
	// count distinguishes a new scale request from existing native rollout surplus.
	dsScale := disaggregatedsetutils.IsOwnedByDisaggregatedSet(lws) &&
		sts.Annotations[leaderworkerset.ReplicasAnnotationKey] == strconv.Itoa(int(stsReplicas))
	rollingUpdateCompleted := partition == 0 && (stsReplicas == lwsReplicas || dsScale)
	// Case 3:
	// In normal cases, return the values directly.
	if rollingUpdateCompleted {
		return 0, lwsReplicas, nil
	}
	if stsReplicas < lwsReplicas {
		return partition, lwsReplicas, nil
	}

	states, err := r.getReplicaStates(ctx, lws, stsReplicas, revisionKey)
	if err != nil {
		return 0, 0, err
	}
	lwsUnreadyReplicas := calculateLWSUnreadyReplicas(states, lwsReplicas)

	originalLwsReplicas, err := strconv.Atoi(sts.Annotations[leaderworkerset.ReplicasAnnotationKey])
	if err != nil {
		return 0, 0, err
	}
	replicasUpdated := originalLwsReplicas != int(*lws.Spec.Replicas)
	// Case 4:
	// Replicas changed during rolling update.
	if replicasUpdated {
		partition := min(partition, burstReplicas)
		return partition, wantReplicas(lwsUnreadyReplicas), nil
	}

	// Case 5:
	// Calculating the Partition during rolling update, no leaderWorkerSet updates happens.

	rollingStep := maxUnavailable
	// Make sure that we always respect the maxUnavailable, or
	// we'll violate it when reclaiming bursted replicas.
	rollingStep += maxSurge - (int(burstReplicas) - int(stsReplicas))

	partition = rollingUpdatePartition(states, stsReplicas, int32(rollingStep), partition)
	return partition, wantReplicas(lwsUnreadyReplicas), nil
}

func (r *LeaderWorkerSetReconciler) SSAWithStatefulset(ctx context.Context, lws *leaderworkerset.LeaderWorkerSet, partition, replicas int32, revisionKey string) error {
	log := ctrl.LoggerFrom(ctx)

	// construct the statefulset apply configuration
	leaderStatefulSetApplyConfig, err := constructLeaderStatefulSetApplyConfiguration(lws, partition, replicas, revisionKey)
	if err != nil {
		log.Error(err, "Constructing StatefulSet apply configuration.")
		return err
	}
	if err := setControllerReferenceWithStatefulSet(lws, leaderStatefulSetApplyConfig, r.Scheme); err != nil {
		log.Error(err, "Setting controller reference.")
		return err
	}
	if err := r.serverSideApply(ctx, leaderStatefulSetApplyConfig); err != nil {
		log.Error(err, "Using server side apply to update leader statefulset")
		return err
	}

	return nil
}

// serverSideApply applies the configuration with the lws field manager. If there
// are conflicts in the fields owned by the lws controller, lws obtains the
// ownership and force overrides them to the desired values.
func (r *LeaderWorkerSetReconciler) serverSideApply(ctx context.Context, applyConfig runtime.ApplyConfiguration) error {
	return r.Apply(ctx, applyConfig, client.FieldOwner(fieldManager), client.ForceOwnership)
}

// updates the condition of the leaderworkerset to either Progressing or Available.
// leaderReplicas is the replica count of the leader StatefulSet, which exceeds
// spec.replicas while a rolling update surges.
func (r *LeaderWorkerSetReconciler) updateConditions(ctx context.Context, lws *leaderworkerset.LeaderWorkerSet, revisionKey string, leaderReplicas int) (bool, bool, error) {
	log := ctrl.LoggerFrom(ctx)
	podSelector := client.MatchingLabels(map[string]string{
		leaderworkerset.SetNameLabelKey:     lws.Name,
		leaderworkerset.WorkerIndexLabelKey: "0",
	})
	leaderPodList := &corev1.PodList{}
	if err := r.List(ctx, leaderPodList, podSelector, client.InNamespace(lws.Namespace)); err != nil {
		log.Error(err, "Fetching leaderPods")
		return false, false, err
	}

	updateStatus := false
	readyCount, updatedCount := 0, 0
	readyNonDegradedCount, degradedGroupCount, desiredDegradedGroupCount := 0, 0, 0
	unschedulableGroupCount := 0
	partitionedUpdatedNonBurstCount, partitionedCurrentNonBurstCount, partitionedUpdatedAndReadyCount := 0, 0, 0
	noWorkerSts := *lws.Spec.LeaderWorkerTemplate.Size == 1
	lwsPartition := *lws.Spec.RolloutStrategy.RollingUpdateConfiguration.Partition

	// Iterate through all leaderPods.
	for _, pod := range leaderPodList.Items {
		index, err := strconv.Atoi(pod.Labels[leaderworkerset.GroupIndexLabelKey])
		if err != nil {
			return false, false, err
		}
		// Surge leaders count too: one that cannot be scheduled stalls the rollout.
		if index < leaderReplicas && pod.DeletionTimestamp == nil && podutils.PodUnschedulable(pod) {
			unschedulableGroupCount++
		}
		degraded := pod.Annotations[leaderworkerset.GroupRestartBudgetExhaustedAnnotationKey] == "true"
		if degraded {
			// Exhaustion is a leader-Pod state and must remain visible even when
			// the worker StatefulSet was never created or is already gone.
			degradedGroupCount++
			if index < int(*lws.Spec.Replicas) {
				desiredDegradedGroupCount++
			}
		}

		var sts appsv1.StatefulSet
		if !noWorkerSts {
			if err := r.Get(ctx, client.ObjectKey{Namespace: lws.Namespace, Name: workerStatefulSetName(&pod)}, &sts); err != nil {
				if client.IgnoreNotFound(err) != nil {
					log.Error(err, "Fetching worker statefulSet")
					return false, false, err
				}
				continue
			}
		}

		if index < int(*lws.Spec.Replicas) && index >= int(lwsPartition) {
			partitionedCurrentNonBurstCount++
		}

		var ready, updated bool
		if (noWorkerSts || statefulsetutils.StatefulsetReady(sts)) && podutils.PodRunningAndReady(pod) {
			ready = true
			readyCount++
		}
		if (noWorkerSts || revisionutils.GetRevisionKey(&sts) == revisionKey) && revisionutils.GetRevisionKey(&pod) == revisionKey {
			updated = true
			updatedCount++
			if index < int(*lws.Spec.Replicas) && index >= int(lwsPartition) {
				// Bursted replicas do not count when determining if rollingUpdate has been completed.
				partitionedUpdatedNonBurstCount++
			}
		}

		if index < int(*lws.Spec.Replicas) {
			if ready {
				if !degraded {
					readyNonDegradedCount++
				}
			}
			if index >= int(lwsPartition) && ready && updated {
				partitionedUpdatedAndReadyCount++
			}
		}
	}

	if lws.Status.ReadyReplicas != int32(readyCount) {
		lws.Status.ReadyReplicas = int32(readyCount)
		updateStatus = true
	}

	if lws.Status.UpdatedReplicas != int32(updatedCount) {
		lws.Status.UpdatedReplicas = int32(updatedCount)
		updateStatus = true
	}

	var conditions []metav1.Condition
	degraded := degradedGroupCount > 0
	rolloutInProgress := partitionedUpdatedNonBurstCount < partitionedCurrentNonBurstCount
	// A replica whose restart budget is exhausted must not make the LWS
	// Available, even while its Pods are still Ready during termination.
	allReplicasReady := readyNonDegradedCount == int(*lws.Spec.Replicas) && partitionedUpdatedAndReadyCount == partitionedCurrentNonBurstCount
	// A degraded group is terminal only when all other desired groups are ready.
	// A missing or unready non-degraded group can still make progress, even
	// though Degraded remains true for the exhausted group.
	progressing := rolloutInProgress || readyNonDegradedCount+desiredDegradedGroupCount < int(*lws.Spec.Replicas)
	if rolloutInProgress {
		// upgradeInProgress is true when the upgrade replicas is smaller than the expected
		// number of total replicas not including the burst replicas
		conditions = append(conditions, makeCondition(leaderworkerset.LeaderWorkerSetUpdateInProgress, lws))
		conditions = append(conditions, makeCondition(leaderworkerset.LeaderWorkerSetProgressing, lws))
	} else if allReplicasReady {
		conditions = append(conditions, makeCondition(leaderworkerset.LeaderWorkerSetAvailable, lws))
	} else if degraded {
		conditions = append(conditions,
			makeFalseCondition(leaderworkerset.LeaderWorkerSetAvailable, lws, "ReplicaRestartBudgetExceeded", "Not all replicas are ready"),
			makeFalseCondition(leaderworkerset.LeaderWorkerSetProgressing, lws, "ReplicaRestartBudgetExceeded", "Automatic recovery is stopped for one or more replicas"),
			makeFalseCondition(leaderworkerset.LeaderWorkerSetUpdateInProgress, lws, "ReplicaRestartBudgetExceeded", "No rolling update is in progress"),
		)
		if progressing {
			conditions[len(conditions)-2] = makeCondition(leaderworkerset.LeaderWorkerSetProgressing, lws)
		}
	} else {
		conditions = append(conditions, makeCondition(leaderworkerset.LeaderWorkerSetProgressing, lws))
		conditions = endUpdateInProgress(conditions, lws)
	}
	if degraded {
		conditions = append(conditions, makeCondition(leaderworkerset.LeaderWorkerSetDegraded, lws))
	} else {
		conditions = append(conditions, makeFalseCondition(leaderworkerset.LeaderWorkerSetDegraded, lws, "AsExpected", "No replica has exhausted its restart budget"))
	}
	explainUnschedulableGroups(conditions, unschedulableGroupCount)

	// updateDone is true when all replicas are updated and ready
	updateDone := (lwsPartition == 0) && partitionedUpdatedAndReadyCount == int(*lws.Spec.Replicas)

	previousConditions := slices.Clone(lws.Status.Conditions)
	updateCondition := setConditions(lws, conditions)
	// if condition changed, record events
	if updateCondition {
		r.recordConditionEvents(lws, previousConditions, conditions, degraded, fmt.Sprintf(", with %d groups ready of total %d groups", readyCount, int(*lws.Spec.Replicas)))
	}
	return updateStatus || updateCondition, updateDone, nil
}

// Updates status and condition of LeaderWorkerSet and returns whether or not an update actually occurred.
func (r *LeaderWorkerSetReconciler) updateStatus(ctx context.Context, lws *leaderworkerset.LeaderWorkerSet, revisionKey string) (bool, error) {
	updateStatus := false
	log := ctrl.LoggerFrom(ctx)

	// Retrieve the leader StatefulSet.
	sts := &appsv1.StatefulSet{}
	if err := r.Get(ctx, types.NamespacedName{Name: lws.Name, Namespace: lws.Namespace}, sts); err != nil {
		log.Error(err, "Error retrieving leader StatefulSet")
		return false, err
	}

	// retrieve the current number of replicas -- the number of leaders
	replicas := int(sts.Status.Replicas)
	if lws.Status.Replicas != int32(replicas) {
		lws.Status.Replicas = int32(replicas)
		updateStatus = true
	}

	if lws.Status.ObservedGeneration != lws.Generation {
		lws.Status.ObservedGeneration = lws.Generation
		updateStatus = true
	}

	selectorUpdated, err := ensureHPAPodSelector(lws)
	if err != nil {
		log.Error(err, "Converting label selector to selector")
		return false, err
	}
	updateStatus = updateStatus || selectorUpdated

	// check if an update is needed
	updateConditions, updateDone, err := r.updateConditions(ctx, lws, revisionKey, int(ptr.Deref(sts.Spec.Replicas, 1)))
	if err != nil {
		return false, err
	}

	if updateStatus || updateConditions {
		if err := r.Status().Update(ctx, lws); err != nil {
			if !apierrors.IsConflict(err) {
				log.Error(err, "Updating LeaderWorkerSet status and/or condition.")
			}
			return false, err
		}
	}
	return updateDone, nil
}

// ensureHPAPodSelector sets the leader pod selector on the status once. Returns
// true when the status changed.
func ensureHPAPodSelector(lws *leaderworkerset.LeaderWorkerSet) (bool, error) {
	if lws.Status.HPAPodSelector != "" {
		return false, nil
	}
	labelSelector := &metav1.LabelSelector{
		MatchLabels: map[string]string{
			leaderworkerset.SetNameLabelKey:     lws.Name,
			leaderworkerset.WorkerIndexLabelKey: "0", // select leaders
		},
	}
	selector, err := metav1.LabelSelectorAsSelector(labelSelector)
	if err != nil {
		return false, err
	}
	lws.Status.HPAPodSelector = selector.String()
	return true, nil
}

type replicaState struct {
	// ready indicates whether both the leader pod and its worker statefulset (if any) are ready.
	ready bool
	// updated indicates whether both the leader pod and its worker statefulset (if any) are updated to the latest revision.
	updated bool
}

func (r *LeaderWorkerSetReconciler) getReplicaStates(ctx context.Context, lws *leaderworkerset.LeaderWorkerSet, stsReplicas int32, revisionKey string) ([]replicaState, error) {
	states := make([]replicaState, stsReplicas)

	podSelector := client.MatchingLabels(map[string]string{
		leaderworkerset.SetNameLabelKey:     lws.Name,
		leaderworkerset.WorkerIndexLabelKey: "0",
	})
	var leaderPodList corev1.PodList
	if err := r.List(ctx, &leaderPodList, podSelector, client.InNamespace(lws.Namespace)); err != nil {
		return nil, err
	}

	// Get a sorted leader pod list matches with the following sorted statefulsets one by one, which means
	// the leader pod and the corresponding worker statefulset has the same index.
	sortedPods := utils.SortByIndex(func(pod corev1.Pod) (int, error) {
		return strconv.Atoi(pod.Labels[leaderworkerset.GroupIndexLabelKey])
	}, leaderPodList.Items, int(stsReplicas))

	stsSelector := client.MatchingLabels(map[string]string{
		leaderworkerset.SetNameLabelKey: lws.Name,
	})
	var stsList appsv1.StatefulSetList
	if err := r.List(ctx, &stsList, stsSelector, client.InNamespace(lws.Namespace)); err != nil {
		return nil, err
	}
	sortedSts := utils.SortByIndex(func(sts appsv1.StatefulSet) (int, error) {
		return strconv.Atoi(sts.Labels[leaderworkerset.GroupIndexLabelKey])
	}, stsList.Items, int(stsReplicas))

	// Once size==1, no worker statefulSets will be created.
	noWorkerSts := *lws.Spec.LeaderWorkerTemplate.Size == 1

	for idx := int32(0); idx < stsReplicas; idx++ {
		nominatedName := fmt.Sprintf("%s-%d", lws.Name, idx)
		// It can happen that the leader pod or the worker statefulset hasn't created yet
		// or under rebuilding, which also indicates not ready.
		if nominatedName != sortedPods[idx].Name || (!noWorkerSts && nominatedName != sortedSts[idx].Name) {
			states[idx] = replicaState{
				ready:   false,
				updated: false,
			}
			continue
		}

		leaderUpdated := revisionutils.GetRevisionKey(&sortedPods[idx]) == revisionKey
		leaderReady := podutils.PodRunningAndReady(sortedPods[idx])

		if noWorkerSts {
			states[idx] = replicaState{
				ready:   leaderReady,
				updated: leaderUpdated,
			}
			continue
		}

		workersUpdated := revisionutils.GetRevisionKey(&sortedSts[idx]) == revisionKey
		workersReady := statefulsetutils.StatefulsetReady(sortedSts[idx])

		states[idx] = replicaState{
			ready:   leaderReady && workersReady,
			updated: leaderUpdated && workersUpdated,
		}
	}

	return states, nil
}

func rollingUpdatePartition(states []replicaState, stsReplicas int32, rollingStep int32, currentPartition int32) int32 {
	continuousReadyReplicas := calculateContinuousReadyReplicas(states)

	// Update up to rollingStep replicas at once.
	var rollingStepPartition = utils.NonZeroValue(stsReplicas - continuousReadyReplicas - rollingStep)

	// rollingStepPartition calculation above disregards the state of replicas with idx<rollingStepPartition.
	// To prevent violating the maxUnavailable, we have to account for these replicas and increase the partition if some are not ready.
	var unavailable int32
	for idx := 0; idx < int(rollingStepPartition); idx++ {
		if !states[idx].ready {
			unavailable++
		}
	}
	var partition = rollingStepPartition + unavailable

	// Reduce the partition if replicas are continuously not ready. It is safe since updating these replicas does not impact
	// the availability of the LWS. This is important to prevent update from getting stuck in case maxUnavailable is already violated
	// (for example, all replicas are not ready when rolling update is started).
	// Note that we never drop the partition below rolliingStepPartition.
	for idx := min(partition, stsReplicas-1); idx >= rollingStepPartition; idx-- {
		if !states[idx].ready || states[idx].updated {
			partition = idx
		} else {
			break
		}
	}

	// That means Partition moves in one direction to make it simple.
	return min(partition, currentPartition)
}

func calculateLWSUnreadyReplicas(states []replicaState, lwsReplicas int32) int32 {
	var unreadyCount int32
	for idx := int32(0); idx < lwsReplicas; idx++ {
		if idx >= int32(len(states)) || !states[idx].ready || !states[idx].updated {
			unreadyCount++
		}
	}
	return unreadyCount
}

func calculateRollingUpdateReplicas(lwsReplicas int32, maxSurge int32, maxUnavailable int32, unreadyReplicas int32) int32 {
	burstReplicas := lwsReplicas + maxSurge
	if unreadyReplicas <= maxSurge {
		// Keep enough surge replicas to cover any desired replicas that are still
		// unavailable beyond the configured maxUnavailable budget. Once the
		// remaining unready desired replicas fit inside the budget, we can reclaim
		// surge capacity gradually.
		requiredSurgeReplicas := utils.NonZeroValue(unreadyReplicas - maxUnavailable)
		return lwsReplicas + requiredSurgeReplicas
	}
	return burstReplicas
}

func calculateContinuousReadyReplicas(states []replicaState) int32 {
	// Count ready replicas at tail (from last index down)
	var continuousReadyCount int32
	for idx := len(states) - 1; idx >= 0; idx-- {
		if !states[idx].ready || !states[idx].updated {
			break
		}
		continuousReadyCount++
	}
	return continuousReadyCount
}

func (r *LeaderWorkerSetReconciler) getLeaderStatefulSet(ctx context.Context, lws *leaderworkerset.LeaderWorkerSet) (*appsv1.StatefulSet, error) {
	sts := &appsv1.StatefulSet{}
	err := r.Get(ctx, types.NamespacedName{Name: lws.Name, Namespace: lws.Namespace}, sts)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	return sts, nil
}

// getOrCreateRevision returns the revision with the given key, creating it when
// none exists. revisionKey is empty when the leader workload does not exist yet.
func (r *LeaderWorkerSetReconciler) getOrCreateRevision(ctx context.Context, revisionKey string, lws *leaderworkerset.LeaderWorkerSet) (*appsv1.ControllerRevision, error) {
	if existingRevision, err := revisionutils.GetRevision(ctx, r.Client, lws, revisionKey); existingRevision != nil || err != nil {
		return existingRevision, err
	}
	revision, err := revisionutils.NewRevision(ctx, r.Client, lws, revisionKey)
	if err != nil {
		return nil, err
	}
	newRevision, err := revisionutils.CreateRevision(ctx, r.Client, revision)
	if err == nil {
		message := fmt.Sprintf("Creating revision with key %s for a newly created LeaderWorkerSet", revision.Labels[leaderworkerset.RevisionKey])
		if revisionKey != "" {
			message = fmt.Sprintf("Creating missing revision with key %s for existing LeaderWorkerSet", revision.Labels[leaderworkerset.RevisionKey])
		}
		r.Record.Eventf(lws, newRevision, corev1.EventTypeNormal, CreatingRevision, Create, message)
	}
	return newRevision, err
}

func (r *LeaderWorkerSetReconciler) getUpdatedRevision(ctx context.Context, lws *leaderworkerset.LeaderWorkerSet, revision *appsv1.ControllerRevision) (*appsv1.ControllerRevision, error) {
	currentRevision, err := revisionutils.NewRevision(ctx, r.Client, lws, "")
	if err != nil {
		return nil, err
	}

	if !revisionutils.EqualRevision(currentRevision, revision) {
		// If raw bytes differ but the revision is semantically equivalent, avoid triggering a spurious rolling update.
		if revisionutils.SetMatchesRevision(lws, currentRevision, revision, r.revisionEqualityCache) {
			return nil, nil
		}
		return currentRevision, nil
	}

	return nil, nil
}

// buildLeaderPodTemplateApplyConfiguration constructs the leader pod template
// shared by the leader statefulset and the leader deployment.
func buildLeaderPodTemplateApplyConfiguration(lws *leaderworkerset.LeaderWorkerSet, revisionKey string) (*coreapplyv1.PodTemplateSpecApplyConfiguration, error) {
	var podTemplateSpec corev1.PodTemplateSpec
	if lws.Spec.LeaderWorkerTemplate.LeaderTemplate != nil {
		podTemplateSpec = *lws.Spec.LeaderWorkerTemplate.LeaderTemplate.DeepCopy()
	} else {
		podTemplateSpec = *lws.Spec.LeaderWorkerTemplate.WorkerTemplate.DeepCopy()
	}
	obj, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&podTemplateSpec)
	if err != nil {
		return nil, err
	}
	var podTemplateApplyConfiguration coreapplyv1.PodTemplateSpecApplyConfiguration
	err = runtime.DefaultUnstructuredConverter.FromUnstructured(obj, &podTemplateApplyConfiguration)
	if err != nil {
		return nil, err
	}

	podTemplateApplyConfiguration.WithLabels(map[string]string{
		leaderworkerset.WorkerIndexLabelKey: "0",
		leaderworkerset.SetNameLabelKey:     lws.Name,
		leaderworkerset.RevisionKey:         revisionKey,
	})
	podAnnotations := make(map[string]string)
	podAnnotations[leaderworkerset.SizeAnnotationKey] = strconv.Itoa(int(*lws.Spec.LeaderWorkerTemplate.Size))
	if topologyKey, ok := utils.ExclusiveTopologyKey(lws); ok {
		podAnnotations[leaderworkerset.ExclusiveKeyAnnotationKey] = topologyKey
	}
	if lws.Spec.LeaderWorkerTemplate.SubGroupPolicy != nil {
		podAnnotations[leaderworkerset.SubGroupPolicyTypeAnnotationKey] = (string(*lws.Spec.LeaderWorkerTemplate.SubGroupPolicy.Type))
		podAnnotations[leaderworkerset.SubGroupSizeAnnotationKey] = strconv.Itoa(int(*lws.Spec.LeaderWorkerTemplate.SubGroupPolicy.SubGroupSize))
		if topologyKey, ok := utils.SubGroupExclusiveTopologyKey(lws); ok {
			podAnnotations[leaderworkerset.SubGroupExclusiveKeyAnnotationKey] = topologyKey
		}
	}

	if lws.Spec.NetworkConfig != nil && *lws.Spec.NetworkConfig.SubdomainPolicy == leaderworkerset.SubdomainUniquePerReplica {
		podAnnotations[leaderworkerset.SubdomainPolicyAnnotationKey] = string(leaderworkerset.SubdomainUniquePerReplica)
	}
	if lws.Spec.Scheduling != nil {
		podAnnotations[schedulerprovider.WorkloadSchedulingAnnotationKey] = schedulerprovider.WorkloadSchedulingValue(lws)
		podAnnotations[schedulerprovider.WorkloadNameAnnotationKey] = schedulerprovider.KubernetesWorkloadName(lws)
	}
	// Sequential is the historical sequential behavior. Stamping it onto the leader
	// pod template would change the StatefulSet and roll existing groups on upgrade.
	if lws.Spec.PodTerminationPolicy == leaderworkerset.ParallelPodTerminationPolicy {
		podAnnotations[leaderworkerset.PodTerminationPolicyAnnotationKey] = string(lws.Spec.PodTerminationPolicy)
	}

	podTemplateApplyConfiguration.WithAnnotations(podAnnotations)

	return &podTemplateApplyConfiguration, nil
}

// leaderMetadata returns the labels and annotations for the leader workload
// object, shared by the leader statefulset and the leader deployment.
func leaderMetadata(lws *leaderworkerset.LeaderWorkerSet, revisionKey string) (map[string]string, map[string]string) {
	labels := mergeMetadata(lws.Labels, map[string]string{
		leaderworkerset.SetNameLabelKey: lws.Name,
		leaderworkerset.RevisionKey:     revisionKey,
		leaderworkerset.RoleLabelKey:    leaderworkerset.RoleLeader,
	})
	annotations := mergeMetadata(lws.Annotations, map[string]string{
		leaderworkerset.ReplicasAnnotationKey: strconv.Itoa(int(*lws.Spec.Replicas)),
	})
	return labels, annotations
}

// constructLeaderStatefulSetApplyConfiguration constructs the applied configuration for the leader StatefulSet
func constructLeaderStatefulSetApplyConfiguration(lws *leaderworkerset.LeaderWorkerSet, partition, replicas int32, revisionKey string) (*appsapplyv1.StatefulSetApplyConfiguration, error) {
	podTemplateApplyConfiguration, err := buildLeaderPodTemplateApplyConfiguration(lws, revisionKey)
	if err != nil {
		return nil, err
	}

	lwsReplicas := int(*lws.Spec.Replicas)
	lwsMaxUnavailable, err := intstr.GetScaledValueFromIntOrPercent(&lws.Spec.RolloutStrategy.RollingUpdateConfiguration.MaxUnavailable, lwsReplicas, false)
	if err != nil {
		return nil, err
	}
	lwsMaxSurge, err := intstr.GetScaledValueFromIntOrPercent(&lws.Spec.RolloutStrategy.RollingUpdateConfiguration.MaxSurge, lwsReplicas, true)
	if err != nil {
		return nil, err
	}
	if lwsMaxSurge > lwsReplicas {
		lwsMaxSurge = lwsReplicas
	}
	stsMaxUnavailableInt := int32(lwsMaxUnavailable + lwsMaxSurge)
	// lwsMaxUnavailable=0 and lwsMaxSurge=0 together should be blocked by webhook,
	// but just in case, we'll make sure that stsMaxUnavailable is at least 1.
	// This also handles the case when lws.Spec.Replicas is 0.
	if stsMaxUnavailableInt < 1 {
		stsMaxUnavailableInt = 1
	}
	stsMaxUnavailable := intstr.FromInt32(stsMaxUnavailableInt)

	// construct statefulset apply configuration
	statefulSetLabels, statefulSetAnnotations := leaderMetadata(lws, revisionKey)
	statefulSetConfig := appsapplyv1.StatefulSet(lws.Name, lws.Namespace).
		WithSpec(appsapplyv1.StatefulSetSpec().
			WithServiceName(lws.Name).
			WithReplicas(replicas).
			WithPodManagementPolicy(appsv1.ParallelPodManagement).
			WithTemplate(podTemplateApplyConfiguration).
			WithUpdateStrategy(appsapplyv1.StatefulSetUpdateStrategy().WithType(appsv1.StatefulSetUpdateStrategyType(lws.Spec.RolloutStrategy.Type)).WithRollingUpdate(
				appsapplyv1.RollingUpdateStatefulSetStrategy().WithMaxUnavailable(stsMaxUnavailable).WithPartition(partition),
			)).
			WithSelector(metaapplyv1.LabelSelector().
				WithMatchLabels(map[string]string{
					leaderworkerset.SetNameLabelKey:     lws.Name,
					leaderworkerset.WorkerIndexLabelKey: "0",
				}))).
		WithLabels(statefulSetLabels).
		WithAnnotations(statefulSetAnnotations)

	pvcApplyConfiguration := controllerutils.GetPVCApplyConfiguration(lws)
	if len(pvcApplyConfiguration) > 0 {
		statefulSetConfig.Spec.WithVolumeClaimTemplates(pvcApplyConfiguration...)
	}

	if lws.Spec.LeaderWorkerTemplate.PersistentVolumeClaimRetentionPolicy != nil {
		pvcRetentionPolicy := &appsapplyv1.StatefulSetPersistentVolumeClaimRetentionPolicyApplyConfiguration{
			WhenDeleted: &lws.Spec.LeaderWorkerTemplate.PersistentVolumeClaimRetentionPolicy.WhenDeleted,
			WhenScaled:  &lws.Spec.LeaderWorkerTemplate.PersistentVolumeClaimRetentionPolicy.WhenScaled,
		}
		statefulSetConfig.Spec.WithPersistentVolumeClaimRetentionPolicy(pvcRetentionPolicy)
	}
	return statefulSetConfig, nil
}

// endUpdateInProgress clears UpdateInProgress once a rollout finishes while
// groups are still becoming ready. Otherwise only Available clears it, which
// flips the status the same way.
func endUpdateInProgress(conditions []metav1.Condition, lws *leaderworkerset.LeaderWorkerSet) []metav1.Condition {
	if c := apimeta.FindStatusCondition(lws.Status.Conditions, string(leaderworkerset.LeaderWorkerSetUpdateInProgress)); c != nil && c.Status == metav1.ConditionTrue {
		ended := *c
		ended.Status = metav1.ConditionFalse
		ended.ObservedGeneration = lws.Generation
		conditions = append(conditions, ended)
	}
	return conditions
}

func makeCondition(conditionType leaderworkerset.LeaderWorkerSetConditionType, lws *leaderworkerset.LeaderWorkerSet) metav1.Condition {
	var condtype, reason, message string
	switch conditionType {
	case leaderworkerset.LeaderWorkerSetAvailable:
		condtype = string(leaderworkerset.LeaderWorkerSetAvailable)
		reason = "AllGroupsReady"
		message = "All replicas are ready"
	case leaderworkerset.LeaderWorkerSetUpdateInProgress:
		condtype = string(leaderworkerset.LeaderWorkerSetUpdateInProgress)
		reason = GroupsUpdating
		message = "Rolling Upgrade is in progress"
	case leaderworkerset.LeaderWorkerSetDegraded:
		condtype = string(leaderworkerset.LeaderWorkerSetDegraded)
		reason = "ReplicaRestartBudgetExceeded"
		message = "Automatic recovery is stopped for one or more replicas"
	default:
		condtype = string(leaderworkerset.LeaderWorkerSetProgressing)
		reason = GroupsProgressing
		message = "Replicas are progressing"
	}

	condition := metav1.Condition{
		Type:               condtype,
		Status:             metav1.ConditionStatus(corev1.ConditionTrue),
		LastTransitionTime: metav1.Now(),
		ObservedGeneration: lws.Generation,
		Reason:             reason,
		Message:            message,
	}
	return condition
}

// explainUnschedulableGroups gives a Progressing=True condition the reason
// GroupUnschedulable when the scheduler could not place some desired groups,
// so that a group waiting for capacity does not look like a normal startup.
func explainUnschedulableGroups(conditions []metav1.Condition, unschedulableGroups int) {
	if unschedulableGroups == 0 {
		return
	}
	for i := range conditions {
		if conditions[i].Type == string(leaderworkerset.LeaderWorkerSetProgressing) && conditions[i].Status == metav1.ConditionTrue {
			conditions[i].Reason = GroupUnschedulable
			conditions[i].Message = fmt.Sprintf("%d replica(s) cannot be scheduled", unschedulableGroups)
		}
	}
}

// conditionEvent is an event to record for a condition.
type conditionEvent struct {
	condition metav1.Condition
	eventType string
}

// conditionEvents returns the events to record after conditions changed from
// previous. The primary condition is reported as before, as a warning when it
// says that groups cannot be scheduled. During a rolling update the primary
// condition is UpdateInProgress, so a Progressing condition that starts
// reporting unschedulable groups, or a different number of them, gets a warning
// of its own, and the primary condition is repeated only if it changed too.
func conditionEvents(previous, conditions []metav1.Condition, degraded bool) []conditionEvent {
	primary := conditions[0]
	if degraded {
		primary = conditions[len(conditions)-1]
	}
	if primary.Reason == GroupUnschedulable {
		return []conditionEvent{{condition: primary, eventType: corev1.EventTypeWarning}}
	}
	var warnings []conditionEvent
	for _, condition := range conditions {
		if condition.Reason == GroupUnschedulable && conditionChanged(previous, condition) {
			warnings = append(warnings, conditionEvent{condition: condition, eventType: corev1.EventTypeWarning})
		}
	}
	if len(warnings) > 0 && !conditionChanged(previous, primary) {
		return warnings
	}
	return append([]conditionEvent{{condition: primary, eventType: corev1.EventTypeNormal}}, warnings...)
}

// conditionChanged reports whether the condition differs from the one of its
// type in previous by more than its observed generation.
func conditionChanged(previous []metav1.Condition, condition metav1.Condition) bool {
	old := apimeta.FindStatusCondition(previous, condition.Type)
	return old == nil || old.Status != condition.Status || old.Reason != condition.Reason || old.Message != condition.Message
}

// recordConditionEvents records the events for a change in conditions, each
// with suffix appended to the condition's message.
func (r *LeaderWorkerSetReconciler) recordConditionEvents(lws *leaderworkerset.LeaderWorkerSet, previous, conditions []metav1.Condition, degraded bool, suffix string) {
	for _, e := range conditionEvents(previous, conditions, degraded) {
		r.Record.Eventf(lws, nil, e.eventType, e.condition.Reason, Update, e.condition.Message+suffix)
	}
}

func makeFalseCondition(conditionType leaderworkerset.LeaderWorkerSetConditionType, lws *leaderworkerset.LeaderWorkerSet, reason, message string) metav1.Condition {
	condition := makeCondition(conditionType, lws)
	condition.Status = metav1.ConditionFalse
	condition.Reason = reason
	condition.Message = message
	return condition
}

func setConditions(lws *leaderworkerset.LeaderWorkerSet, conditions []metav1.Condition) bool {
	shouldUpdate := false
	for _, condition := range conditions {
		// setCondition mutates the status, so it must run for every requested
		// condition. Accumulating with `shouldUpdate || setCondition(...)` would
		// short-circuit and silently drop every condition after the first one
		// that changed.
		if setCondition(lws, condition) {
			shouldUpdate = true
		}
	}

	for i := range lws.Status.Conditions {
		if lws.Status.Conditions[i].ObservedGeneration != lws.Generation {
			lws.Status.Conditions[i].ObservedGeneration = lws.Generation
			shouldUpdate = true
		}
	}

	return shouldUpdate
}

func setCondition(lws *leaderworkerset.LeaderWorkerSet, newCondition metav1.Condition) bool {
	now := metav1.Now()
	found := false
	shouldUpdate := false

	// Conditions are keyed by type and may be either True or False.
	for i, curCondition := range lws.Status.Conditions {
		if newCondition.Type == curCondition.Type {
			if newCondition.Status != curCondition.Status {
				newCondition.LastTransitionTime = now
				lws.Status.Conditions[i] = newCondition
				shouldUpdate = true
			} else if newCondition.ObservedGeneration != curCondition.ObservedGeneration ||
				newCondition.Reason != curCondition.Reason ||
				newCondition.Message != curCondition.Message {
				// Status did not transition, so preserve LastTransitionTime while
				// synchronizing the other user-visible condition fields.
				newCondition.LastTransitionTime = curCondition.LastTransitionTime
				lws.Status.Conditions[i] = newCondition
				shouldUpdate = true
			}
			found = true
		} else {
			// if the conditions are not of the same type, do nothing unless one is Progressing and one is
			// Available and both are true. Must be mutually exclusive.
			if exclusiveConditionTypes(curCondition, newCondition) &&
				(newCondition.Status == metav1.ConditionTrue) && (curCondition.Status == metav1.ConditionTrue) {
				lws.Status.Conditions[i].Status = metav1.ConditionFalse
				lws.Status.Conditions[i].LastTransitionTime = now
				lws.Status.Conditions[i].ObservedGeneration = newCondition.ObservedGeneration
				if lws.Status.Conditions[i].Reason == GroupUnschedulable {
					// The unschedulable count only describes a Progressing LWS; do not
					// leave it on the condition once the LWS is Available.
					defaults := makeCondition(leaderworkerset.LeaderWorkerSetProgressing, lws)
					lws.Status.Conditions[i].Reason = defaults.Reason
					lws.Status.Conditions[i].Message = defaults.Message
				}
				shouldUpdate = true
			}
		}
	}
	if !found {
		newCondition.LastTransitionTime = now
		lws.Status.Conditions = append(lws.Status.Conditions, newCondition)
		shouldUpdate = true
	}
	return shouldUpdate
}

func exclusiveConditionTypes(condition1 metav1.Condition, condition2 metav1.Condition) bool {
	if (condition1.Type == string(leaderworkerset.LeaderWorkerSetAvailable) && condition2.Type == string(leaderworkerset.LeaderWorkerSetProgressing)) ||
		(condition1.Type == string(leaderworkerset.LeaderWorkerSetProgressing) && condition2.Type == string(leaderworkerset.LeaderWorkerSetAvailable)) {
		return true
	}

	if (condition1.Type == string(leaderworkerset.LeaderWorkerSetAvailable) && condition2.Type == string(leaderworkerset.LeaderWorkerSetUpdateInProgress)) ||
		(condition1.Type == string(leaderworkerset.LeaderWorkerSetUpdateInProgress) && condition2.Type == string(leaderworkerset.LeaderWorkerSetAvailable)) {
		return true
	}

	return false
}

// deleteWorkerStatefulSets proactively deletes all worker StatefulSets with foreground
// propagation when the LeaderWorkerSet is being deleted with foreground cascading deletion
// (where lws.DeletionTimestamp != nil is observed). Under standard background cascading
// deletion, the LWS object is removed immediately without a finalizer, and the worker
// StatefulSets are deleted by the pod controller's LWS-not-found path via the leader pod's
// pod-termination-policy annotation.
func (r *LeaderWorkerSetReconciler) deleteWorkerStatefulSets(ctx context.Context, lws *leaderworkerset.LeaderWorkerSet) error {
	var workerStsList appsv1.StatefulSetList
	if err := r.List(ctx, &workerStsList, client.InNamespace(lws.Namespace), client.MatchingLabels{
		leaderworkerset.SetNameLabelKey: lws.Name,
		leaderworkerset.RoleLabelKey:    leaderworkerset.RoleWorker,
	}); err != nil {
		return err
	}
	propagation := metav1.DeletePropagationForeground
	// This list is namespace-scoped. Reuse owner lookups only for this cleanup pass.
	replicaSets := make(map[string]*appsv1.ReplicaSet)
	for i := range workerStsList.Items {
		sts := &workerStsList.Items[i]
		if sts.DeletionTimestamp != nil {
			continue
		}
		// Labels narrow the search; the owner chain determines what we may delete.
		owned, err := r.workerStatefulSetOwnedByLWS(ctx, sts, lws, replicaSets)
		if err != nil {
			return fmt.Errorf("checking owner of worker StatefulSet %s: %w", sts.Name, err)
		}
		if !owned {
			continue
		}
		if err := r.Delete(ctx, sts, &client.DeleteOptions{PropagationPolicy: &propagation}, client.Preconditions{UID: ptr.To(sts.UID)}); client.IgnoreNotFound(err) != nil {
			return err
		}
	}
	return nil
}

// Follow the native owner chain, checking UIDs as well as names. If an owner is
// already gone, leave its workers to garbage collection instead of guessing.
func (r *LeaderWorkerSetReconciler) workerStatefulSetOwnedByLWS(ctx context.Context, workers *appsv1.StatefulSet, lws *leaderworkerset.LeaderWorkerSet, replicaSets map[string]*appsv1.ReplicaSet) (bool, error) {
	parents := []client.Object{&corev1.Pod{}, &appsv1.StatefulSet{}}
	if lws.Spec.GroupIdentity == leaderworkerset.GroupIdentityHash {
		parents = []client.Object{&corev1.Pod{}, &appsv1.ReplicaSet{}, &appsv1.Deployment{}}
	}
	var child client.Object = workers
	for _, parent := range parents {
		ref := metav1.GetControllerOf(child)
		if ref == nil || ref.UID == "" {
			return false, nil
		}
		gvk, err := apiutil.GVKForObject(parent, r.Scheme)
		if err != nil {
			return false, err
		}
		if ref.APIVersion != gvk.GroupVersion().String() || ref.Kind != gvk.Kind {
			return false, nil
		}
		key := client.ObjectKey{Namespace: workers.Namespace, Name: ref.Name}
		if rs, isReplicaSet := parent.(*appsv1.ReplicaSet); isReplicaSet {
			cached, found := replicaSets[ref.Name]
			if !found {
				reader := r.apiReader
				if reader == nil {
					reader = r.Client
				}
				if err := reader.Get(ctx, key, rs); err != nil {
					if !apierrors.IsNotFound(err) {
						return false, err
					}
					rs = nil // Remember missing owners too, until the next cleanup pass.
				}
				cached = rs
				replicaSets[ref.Name] = cached
			}
			if cached == nil {
				return false, nil
			}
			parent = cached
		} else if err := r.Get(ctx, key, parent); err != nil {
			return false, client.IgnoreNotFound(err)
		}
		if parent.GetUID() != ref.UID {
			return false, nil
		}
		child = parent
	}
	ref := metav1.GetControllerOf(child)
	return ref != nil && ref.APIVersion == apiGVStr && ref.Kind == "LeaderWorkerSet" &&
		ref.Name == lws.Name && lws.UID != "" && ref.UID == lws.UID, nil
}

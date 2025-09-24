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
	"fmt"
	"strconv"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	appsapplyv1 "k8s.io/client-go/applyconfigurations/apps/v1"
	coreapplyv1 "k8s.io/client-go/applyconfigurations/core/v1"
	metaapplyv1 "k8s.io/client-go/applyconfigurations/meta/v1"
	"k8s.io/client-go/tools/record"
	"k8s.io/klog/v2"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	leaderworkerset "sigs.k8s.io/lws/api/leaderworkerset/v1"
	"sigs.k8s.io/lws/pkg/utils"
	controllerutils "sigs.k8s.io/lws/pkg/utils/controller"
	podutils "sigs.k8s.io/lws/pkg/utils/pod"
	revisionutils "sigs.k8s.io/lws/pkg/utils/revision"
	statefulsetutils "sigs.k8s.io/lws/pkg/utils/statefulset"
)

// LeaderWorkerSetReconciler reconciles a LeaderWorkerSet object
type LeaderWorkerSetReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	Record record.EventRecorder
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
)

func NewLeaderWorkerSetReconciler(client client.Client, scheme *runtime.Scheme, record record.EventRecorder) *LeaderWorkerSetReconciler {
	return &LeaderWorkerSetReconciler{
		Client: client,
		Scheme: scheme,
		Record: record,
	}
}

//+kubebuilder:rbac:groups="",resources=events,verbs=create;watch;update;patch
//+kubebuilder:rbac:groups=leaderworkerset.x-k8s.io,resources=leaderworkersets,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=leaderworkerset.x-k8s.io,resources=leaderworkersets/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=leaderworkerset.x-k8s.io,resources=leaderworkersets/finalizers,verbs=update
//+kubebuilder:rbac:groups=apps,resources=statefulsets,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=apps,resources=statefulsets/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=apps,resources=statefulsets/finalizers,verbs=update
//+kubebuilder:rbac:groups=core,resources=services,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=core,resources=events,verbs=get;list;watch;create;patch
//+kubebuilder:rbac:groups=apps,resources=controllerrevisions,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=apps,resources=controllerrevisions/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=apps,resources=controllerrevisions/finalizers,verbs=update

func (r *LeaderWorkerSetReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	// Get leaderworkerset object
	lws := &leaderworkerset.LeaderWorkerSet{}
	if err := r.Get(ctx, types.NamespacedName{Name: req.Name, Namespace: req.Namespace}, lws); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if lws.DeletionTimestamp != nil {
		return ctrl.Result{}, nil
	}

	log := ctrl.LoggerFrom(ctx).WithValues("leaderworkerset", klog.KObj(lws))
	ctx = ctrl.LoggerInto(ctx, log)

	leaderSts, err := r.getLeaderStatefulSet(ctx, lws)
	if err != nil {
		log.Error(err, "Fetching leader statefulset")
		return ctrl.Result{}, err
	}

	// Handles two cases:
	// Case 1: Upgrading the LWS controller from a version that doesn't support controller revision
	// Case 2: Creating the controller revision for a newly created LWS object
	revision, err := r.getOrCreateRevisionIfNonExist(ctx, leaderSts, lws, r.Record)
	if err != nil {
		log.Error(err, "Creating controller revision")
		return ctrl.Result{}, err
	}

	updatedRevision, err := r.getUpdatedRevision(ctx, leaderSts, lws, revision)
	if err != nil {
		log.Error(err, "Validating if LWS has been updated")
		return ctrl.Result{}, err
	}
	lwsUpdated := updatedRevision != nil
	if lwsUpdated {
		revision, err = revisionutils.CreateRevision(ctx, r.Client, updatedRevision)
		if err != nil {
			log.Error(err, "Creating revision for updated LWS")
			return ctrl.Result{}, err
		}
		r.Record.Eventf(lws, corev1.EventTypeNormal, CreatingRevision, fmt.Sprintf("Creating revision with key %s for updated LWS", revisionutils.GetRevisionKey(revision)))
	}

	partition, replicas, err := r.rollingUpdateParameters(ctx, lws, leaderSts, revisionutils.GetRevisionKey(revision), lwsUpdated)
	if err != nil {
		log.Error(err, "Rolling partition error")
		return ctrl.Result{}, err
	}

	if err := r.SSAWithStatefulset(ctx, lws, partition, replicas, revisionutils.GetRevisionKey(revision)); err != nil {
		if leaderSts == nil {
			r.Record.Eventf(lws, corev1.EventTypeWarning, FailedCreate, fmt.Sprintf("Failed to create leader statefulset %s", lws.Name))
		}
		return ctrl.Result{}, err
	}

	if leaderSts == nil {
		// An event is logged to track sts creation.
		r.Record.Eventf(lws, corev1.EventTypeNormal, GroupsProgressing, fmt.Sprintf("Created leader statefulset %s", lws.Name))
	} else if !lwsUpdated && partition != *leaderSts.Spec.UpdateStrategy.RollingUpdate.Partition {
		// An event is logged to track update progress.
		r.Record.Eventf(lws, corev1.EventTypeNormal, GroupsUpdating, fmt.Sprintf("Updating replicas %d to %d", *leaderSts.Spec.UpdateStrategy.RollingUpdate.Partition, partition))
	}

	// Create headless service if it does not exist.
	if err := r.reconcileHeadlessServices(ctx, lws); err != nil {
		log.Error(err, "Creating headless service.")
		r.Record.Eventf(lws, corev1.EventTypeWarning, FailedCreate,
			fmt.Sprintf("Failed to create headless service for error: %v", err))
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
	}
	log.V(2).Info("Leader Reconcile completed.")
	return ctrl.Result{}, nil
}

func (r *LeaderWorkerSetReconciler) reconcileHeadlessServices(ctx context.Context, lws *leaderworkerset.LeaderWorkerSet) error {
	if lws.Spec.NetworkConfig == nil || *lws.Spec.NetworkConfig.SubdomainPolicy == leaderworkerset.SubdomainShared {
		if err := controllerutils.CreateHeadlessServiceIfNotExists(ctx, r.Client, r.Scheme, lws, lws.Name, map[string]string{leaderworkerset.SetNameLabelKey: lws.Name}, lws); err != nil {
			return err
		}
		return nil
	}
	return nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *LeaderWorkerSetReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&leaderworkerset.LeaderWorkerSet{}).
		Owns(&appsv1.StatefulSet{}).
		Owns(&corev1.Service{}).
		Watches(&appsv1.StatefulSet{},
			handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, a client.Object) []reconcile.Request {
				return []reconcile.Request{
					{NamespacedName: types.NamespacedName{
						Name:      a.GetLabels()[leaderworkerset.SetNameLabelKey],
						Namespace: a.GetNamespace(),
					}},
				}
			})).
		Complete(r)
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
	// No need to burst more than the replicas.
	if maxSurge > int(lwsReplicas) {
		maxSurge = int(lwsReplicas)
	}
	burstReplicas := lwsReplicas + int32(maxSurge)

	// wantReplicas calculates the final replicas if needed.
	wantReplicas := func(unreadyReplicas int32) int32 {
		if unreadyReplicas <= int32(maxSurge) {
			// When we have n unready replicas and n bursted replicas, we should
			// start to release the burst replica gradually for the accommodation of
			// the unready ones.
			finalReplicas := lwsReplicas + utils.NonZeroValue(int32(unreadyReplicas)-1)
			r.Record.Eventf(lws, corev1.EventTypeNormal, GroupsProgressing, fmt.Sprintf("deleting surge replica %s-%d", lws.Name, finalReplicas))
			return finalReplicas
		}
		return burstReplicas
	}

	// Case 2:
	// Indicates a new rolling update here.
	if leaderWorkerSetUpdated {
		// Processing scaling up/down first prior to rolling update.
		return min(lwsReplicas, stsReplicas), wantReplicas(lwsReplicas), nil
	}

	partition := *sts.Spec.UpdateStrategy.RollingUpdate.Partition
	rollingUpdateCompleted := partition == 0 && stsReplicas == lwsReplicas
	// Case 3:
	// In normal cases, return the values directly.
	if rollingUpdateCompleted {
		return 0, lwsReplicas, nil
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
		return min(partition, burstReplicas), wantReplicas(lwsUnreadyReplicas), nil
	}

	// Case 5:
	// Calculating the Partition during rolling update, no leaderWorkerSet updates happens.

	rollingStep, err := intstr.GetScaledValueFromIntOrPercent(&lws.Spec.RolloutStrategy.RollingUpdateConfiguration.MaxUnavailable, int(lwsReplicas), false)
	if err != nil {
		return 0, 0, err
	}
	// Make sure that we always respect the maxUnavailable, or
	// we'll violate it when reclaiming bursted replicas.
	rollingStep += maxSurge - (int(burstReplicas) - int(stsReplicas))

	return rollingUpdatePartition(states, stsReplicas, int32(rollingStep), partition), wantReplicas(lwsUnreadyReplicas), nil
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
	obj, err := runtime.DefaultUnstructuredConverter.ToUnstructured(leaderStatefulSetApplyConfig)
	if err != nil {
		log.Error(err, "Converting StatefulSet configuration to json.")
		return err
	}
	patch := &unstructured.Unstructured{
		Object: obj,
	}
	// Use server side apply and add fieldmanager to the lws owned fields
	// If there are conflicts in the fields owned by the lws controller, lws will obtain the ownership and force override
	// these fields to the ones desired by the lws controller
	// TODO b/316776287 add E2E test for SSA
	err = r.Patch(ctx, patch, client.Apply, &client.PatchOptions{
		FieldManager: fieldManager,
		Force:        ptr.To[bool](true),
	})
	if err != nil {
		log.Error(err, "Using server side apply to update leader statefulset")
		return err
	}

	return nil
}

// updates the condition of the leaderworkerset to either Progressing or Available.
func (r *LeaderWorkerSetReconciler) updateConditions(ctx context.Context, lws *leaderworkerset.LeaderWorkerSet, revisionKey string) (bool, bool, error) {
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
	readyCount, updatedCount, readyNonBurstWorkerCount := 0, 0, 0
	partitionedUpdatedNonBurstCount, partitionedCurrentNonBurstCount, partitionedUpdatedAndReadyCount := 0, 0, 0
	noWorkerSts := *lws.Spec.LeaderWorkerTemplate.Size == 1
	lwsPartition := *lws.Spec.RolloutStrategy.RollingUpdateConfiguration.Partition

	// Iterate through all leaderPods.
	for _, pod := range leaderPodList.Items {
		index, err := strconv.Atoi(pod.Labels[leaderworkerset.GroupIndexLabelKey])
		if err != nil {
			return false, false, err
		}

		var sts appsv1.StatefulSet
		if !noWorkerSts {
			if err := r.Get(ctx, client.ObjectKey{Namespace: lws.Namespace, Name: pod.Name}, &sts); err != nil {
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
				readyNonBurstWorkerCount++
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
	if partitionedUpdatedNonBurstCount < partitionedCurrentNonBurstCount {
		// upgradeInProgress is true when the upgrade replicas is smaller than the expected
		// number of total replicas not including the burst replicas
		conditions = append(conditions, makeCondition(leaderworkerset.LeaderWorkerSetUpdateInProgress))
		conditions = append(conditions, makeCondition(leaderworkerset.LeaderWorkerSetProgressing))
	} else if readyNonBurstWorkerCount == int(*lws.Spec.Replicas) && partitionedUpdatedAndReadyCount == partitionedCurrentNonBurstCount {
		conditions = append(conditions, makeCondition(leaderworkerset.LeaderWorkerSetAvailable))
	} else {
		conditions = append(conditions, makeCondition(leaderworkerset.LeaderWorkerSetProgressing))
	}

	// updateDone is true when all replicas are updated and ready
	updateDone := (lwsPartition == 0) && partitionedUpdatedAndReadyCount == int(*lws.Spec.Replicas)

	updateCondition := setConditions(lws, conditions)
	// if condition changed, record events
	if updateCondition {
		r.Record.Eventf(lws, corev1.EventTypeNormal, conditions[0].Reason, conditions[0].Message+fmt.Sprintf(", with %d groups ready of total %d groups", readyCount, int(*lws.Spec.Replicas)))
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

	if lws.Status.HPAPodSelector == "" {
		labelSelector := &metav1.LabelSelector{
			MatchLabels: map[string]string{
				leaderworkerset.SetNameLabelKey:     lws.Name,
				leaderworkerset.WorkerIndexLabelKey: "0", // select leaders
			},
		}
		selector, err := metav1.LabelSelectorAsSelector(labelSelector)
		if err != nil {
			log.Error(err, "Converting label selector to selector")
			return false, err
		}

		lws.Status.HPAPodSelector = selector.String()
		updateStatus = true
	}

	// check if an update is needed
	updateConditions, updateDone, err := r.updateConditions(ctx, lws, revisionKey)
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

func (r *LeaderWorkerSetReconciler) getOrCreateRevisionIfNonExist(ctx context.Context, sts *appsv1.StatefulSet, lws *leaderworkerset.LeaderWorkerSet, recorder record.EventRecorder) (*appsv1.ControllerRevision, error) {
	revisionKey := ""
	if sts != nil {
		// Uses the hash in the leader sts to avoid detecting update in the case where LWS controller is upgraded from a version where
		// the revisionKey was used to detect update instead of controller revision.
		revisionKey = revisionutils.GetRevisionKey(sts)
	}
	if stsRevision, err := revisionutils.GetRevision(ctx, r.Client, lws, revisionKey); stsRevision != nil || err != nil {
		return stsRevision, err
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
		recorder.Eventf(lws, corev1.EventTypeNormal, CreatingRevision, message)
	}
	return newRevision, err
}

func (r *LeaderWorkerSetReconciler) getUpdatedRevision(ctx context.Context, sts *appsv1.StatefulSet, lws *leaderworkerset.LeaderWorkerSet, revision *appsv1.ControllerRevision) (*appsv1.ControllerRevision, error) {
	if sts == nil {
		return nil, nil
	}

	currentRevision, err := revisionutils.NewRevision(ctx, r.Client, lws, "")
	if err != nil {
		return nil, err
	}

	if !revisionutils.EqualRevision(currentRevision, revision) {
		return currentRevision, nil
	}

	return nil, nil
}

// constructLeaderStatefulSetApplyConfiguration constructs the applied configuration for the leader StatefulSet
func constructLeaderStatefulSetApplyConfiguration(lws *leaderworkerset.LeaderWorkerSet, partition, replicas int32, revisionKey string) (*appsapplyv1.StatefulSetApplyConfiguration, error) {
	var podTemplateSpec corev1.PodTemplateSpec
	if lws.Spec.LeaderWorkerTemplate.LeaderTemplate != nil {
		podTemplateSpec = *lws.Spec.LeaderWorkerTemplate.LeaderTemplate.DeepCopy()
	} else {
		podTemplateSpec = *lws.Spec.LeaderWorkerTemplate.WorkerTemplate.DeepCopy()
	}
	// construct pod template spec configuration
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
	if lws.Annotations[leaderworkerset.ExclusiveKeyAnnotationKey] != "" {
		podAnnotations[leaderworkerset.ExclusiveKeyAnnotationKey] = lws.Annotations[leaderworkerset.ExclusiveKeyAnnotationKey]
	}
	if lws.Spec.LeaderWorkerTemplate.SubGroupPolicy != nil {
		podAnnotations[leaderworkerset.SubGroupPolicyTypeAnnotationKey] = (string(*lws.Spec.LeaderWorkerTemplate.SubGroupPolicy.Type))
		podAnnotations[leaderworkerset.SubGroupSizeAnnotationKey] = strconv.Itoa(int(*lws.Spec.LeaderWorkerTemplate.SubGroupPolicy.SubGroupSize))
		if lws.Annotations[leaderworkerset.SubGroupExclusiveKeyAnnotationKey] != "" {
			podAnnotations[leaderworkerset.SubGroupExclusiveKeyAnnotationKey] = lws.Annotations[leaderworkerset.SubGroupExclusiveKeyAnnotationKey]
		}
	}

	if lws.Spec.NetworkConfig != nil && *lws.Spec.NetworkConfig.SubdomainPolicy == leaderworkerset.SubdomainUniquePerReplica {
		podAnnotations[leaderworkerset.SubdomainPolicyAnnotationKey] = string(leaderworkerset.SubdomainUniquePerReplica)
	}

	podTemplateApplyConfiguration.WithAnnotations(podAnnotations)

	// construct statefulset apply configuration
	statefulSetConfig := appsapplyv1.StatefulSet(lws.Name, lws.Namespace).
		WithSpec(appsapplyv1.StatefulSetSpec().
			WithServiceName(lws.Name).
			WithReplicas(replicas).
			WithPodManagementPolicy(appsv1.ParallelPodManagement).
			WithTemplate(&podTemplateApplyConfiguration).
			WithUpdateStrategy(appsapplyv1.StatefulSetUpdateStrategy().WithType(appsv1.StatefulSetUpdateStrategyType(lws.Spec.RolloutStrategy.Type)).WithRollingUpdate(
				appsapplyv1.RollingUpdateStatefulSetStrategy().WithMaxUnavailable(lws.Spec.RolloutStrategy.RollingUpdateConfiguration.MaxUnavailable).WithPartition(partition),
			)).
			WithSelector(metaapplyv1.LabelSelector().
				WithMatchLabels(map[string]string{
					leaderworkerset.SetNameLabelKey:     lws.Name,
					leaderworkerset.WorkerIndexLabelKey: "0",
				}))).
		WithLabels(map[string]string{
			leaderworkerset.SetNameLabelKey: lws.Name,
			leaderworkerset.RevisionKey:     revisionKey,
		}).
		WithAnnotations(map[string]string{
			leaderworkerset.ReplicasAnnotationKey: strconv.Itoa(int(*lws.Spec.Replicas)),
		})

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

func makeCondition(conditionType leaderworkerset.LeaderWorkerSetConditionType) metav1.Condition {
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
	default:
		condtype = string(leaderworkerset.LeaderWorkerSetProgressing)
		reason = GroupsProgressing
		message = "Replicas are progressing"
	}

	condition := metav1.Condition{
		Type:               condtype,
		Status:             metav1.ConditionStatus(corev1.ConditionTrue),
		LastTransitionTime: metav1.Now(),
		Reason:             reason,
		Message:            message,
	}
	return condition
}

func setConditions(lws *leaderworkerset.LeaderWorkerSet, conditions []metav1.Condition) bool {
	shouldUpdate := false
	for _, condition := range conditions {
		shouldUpdate = shouldUpdate || setCondition(lws, condition)
	}

	return shouldUpdate
}

func setCondition(lws *leaderworkerset.LeaderWorkerSet, newCondition metav1.Condition) bool {
	newCondition.LastTransitionTime = metav1.Now()
	found := false
	shouldUpdate := false

	// Precondition: newCondition has status true.
	for i, curCondition := range lws.Status.Conditions {
		if newCondition.Type == curCondition.Type {
			if newCondition.Status != curCondition.Status {
				// the conditions match but one is true and one is false. Update the stored condition
				// with the new condition.
				lws.Status.Conditions[i] = newCondition
				shouldUpdate = true
			}
			// if both are true or both are false, do nothing.
			found = true
		} else {
			// if the conditions are not of the same type, do nothing unless one is Progressing and one is
			// Available and both are true. Must be mutually exclusive.
			if exclusiveConditionTypes(curCondition, newCondition) &&
				(newCondition.Status == metav1.ConditionTrue) && (curCondition.Status == metav1.ConditionTrue) {
				// Progressing is true and Available is true. Prevent this.
				lws.Status.Conditions[i].Status = metav1.ConditionFalse
				shouldUpdate = true
			}
		}
	}
	// condition doesn't exist, update only if the status is true
	if newCondition.Status == metav1.ConditionTrue && !found {
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

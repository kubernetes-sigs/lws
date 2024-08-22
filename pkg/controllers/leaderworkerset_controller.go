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
	podutils "sigs.k8s.io/lws/pkg/utils/pod"
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
	FailedCreate = "FailedCreate"
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

func (r *LeaderWorkerSetReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	// Get leaderworkerset object
	lws := &leaderworkerset.LeaderWorkerSet{}
	if err := r.Get(ctx, types.NamespacedName{Name: req.Name, Namespace: req.Namespace}, lws); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	log := ctrl.LoggerFrom(ctx).WithValues("leaderworkerset", klog.KObj(lws))
	ctx = ctrl.LoggerInto(ctx, log)

	partition, replicas, err := r.rollingUpdateParameters(ctx, lws)
	if err != nil {
		log.Error(err, "Rolling partition error")
		return ctrl.Result{}, err
	}

	if err := r.SSAWithStatefulset(ctx, lws, partition, replicas); err != nil {
		return ctrl.Result{}, err
	}

	// Create headless service if it does not exist.
	if err := r.createMultipleHeadlessServices(ctx, lws); err != nil {
		log.Error(err, "Creating headless service.")
		r.Record.Eventf(lws, corev1.EventTypeWarning, FailedCreate,
			fmt.Sprintf("Failed to create headless service for error: %v", err))
		return ctrl.Result{}, err
	}

	err = r.updateStatus(ctx, lws)
	if err != nil {
		return ctrl.Result{}, err
	}

	log.V(2).Info("Leader Reconcile completed.")
	return ctrl.Result{}, nil
}

func (r *LeaderWorkerSetReconciler) createMultipleHeadlessServices(ctx context.Context, lws *leaderworkerset.LeaderWorkerSet) error {
	if lws.Spec.NetworkConfig.SubdomainPolicy == leaderworkerset.SubdomainShared || lws.Spec.NetworkConfig == nil {
		if err := r.createHeadlessServiceIfNotExists(ctx, lws, lws.Name, map[string]string{leaderworkerset.SetNameLabelKey: lws.Name}); err != nil {
			return err
		}
		return nil
	}

	for i := 0; i < int(*lws.Spec.Replicas); i++ {
		err := r.createHeadlessServiceIfNotExists(ctx, lws, fmt.Sprintf("%s-%s", lws.Name, strconv.Itoa(i)), map[string]string{leaderworkerset.GroupIndexLabelKey: strconv.Itoa(i)})
		if err != nil {
			return err
		}
	}
	return nil
}

func (r *LeaderWorkerSetReconciler) createHeadlessServiceIfNotExists(ctx context.Context, lws *leaderworkerset.LeaderWorkerSet, serviceName string, serviceSelector map[string]string) error {
	log := ctrl.LoggerFrom(ctx)
	// If the headless service does not exist in the namespace, create it.
	var headlessService corev1.Service
	if err := r.Get(ctx, types.NamespacedName{Name: serviceName, Namespace: lws.Namespace}, &headlessService); err != nil {
		if client.IgnoreNotFound(err) != nil {
			return err
		}
		headlessService := corev1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Name:      serviceName,
				Namespace: lws.Namespace,
			},
			Spec: corev1.ServiceSpec{
				ClusterIP:                "None", // defines service as headless
				Selector:                 serviceSelector,
				PublishNotReadyAddresses: true,
			},
		}

		// Set the controller owner reference for garbage collection and reconciliation.
		if err := ctrl.SetControllerReference(lws, &headlessService, r.Scheme); err != nil {
			return err
		}
		// create the service in the cluster
		log.V(2).Info("Creating headless service.")
		if err := r.Create(ctx, &headlessService); err != nil {
			return err
		}
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
func (r *LeaderWorkerSetReconciler) rollingUpdateParameters(ctx context.Context, lws *leaderworkerset.LeaderWorkerSet) (int32, int32, error) {
	lwsReplicas := *lws.Spec.Replicas

	sts := &appsv1.StatefulSet{}
	err := r.Get(ctx, types.NamespacedName{Name: lws.Name, Namespace: lws.Namespace}, sts)
	if err != nil {
		// Case 1:
		// If sts not created yet, all partitions should be updated,
		// replicas should not change.
		if apierrors.IsNotFound(err) {
			return 0, lwsReplicas, nil
		}
		return 0, 0, err
	}

	stsReplicas := *sts.Spec.Replicas
	maxSurge, err := intstr.GetValueFromIntOrPercent(&lws.Spec.RolloutStrategy.RollingUpdateConfiguration.MaxSurge, int(lwsReplicas), true)
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
			return lwsReplicas + utils.NonZeroValue(int32(unreadyReplicas)-1)
		}
		return burstReplicas
	}

	// Case 2:
	// Indicates a new rolling update here.
	if templateUpdated(sts, lws) {
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

	continuousReadyReplicas, lwsUnreadyReplicas, err := r.iterateReplicas(ctx, lws, stsReplicas)
	if err != nil {
		return 0, 0, err
	}

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

	rollingStep, err := intstr.GetValueFromIntOrPercent(&lws.Spec.RolloutStrategy.RollingUpdateConfiguration.MaxUnavailable, int(lwsReplicas), false)
	if err != nil {
		return 0, 0, err
	}
	// Make sure that we always respect the maxUnavailable, or
	// we'll violate it when reclaiming bursted replicas.
	rollingStep += maxSurge - (int(burstReplicas) - int(stsReplicas))

	// When updated replicas become not ready again or scaled up replicas are not ready yet,
	// we'll not modify the Partition field. That means Partition moves in one direction to make it simple.
	return min(partition, utils.NonZeroValue(stsReplicas-int32(rollingStep)-continuousReadyReplicas)), wantReplicas(lwsUnreadyReplicas), nil
}

func (r *LeaderWorkerSetReconciler) SSAWithStatefulset(ctx context.Context, lws *leaderworkerset.LeaderWorkerSet, partition, replicas int32) error {
	log := ctrl.LoggerFrom(ctx)

	// construct the statefulset apply configuration
	leaderStatefulSetApplyConfig, err := constructLeaderStatefulSetApplyConfiguration(lws, partition, replicas)
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
func (r *LeaderWorkerSetReconciler) updateConditions(ctx context.Context, lws *leaderworkerset.LeaderWorkerSet) (bool, error) {
	log := ctrl.LoggerFrom(ctx)
	podSelector := client.MatchingLabels(map[string]string{
		leaderworkerset.SetNameLabelKey:     lws.Name,
		leaderworkerset.WorkerIndexLabelKey: "0",
	})
	leaderPodList := &corev1.PodList{}
	if err := r.List(ctx, leaderPodList, podSelector, client.InNamespace(lws.Namespace)); err != nil {
		log.Error(err, "Fetching leaderPods")
		return false, err
	}

	updateStatus := false
	readyCount, updatedCount, updatedNonBurstWorkerCount, currentNonBurstWorkerCount, updatedAndReadyCount := 0, 0, 0, 0, 0
	templateHash := utils.LeaderWorkerTemplateHash(lws)
	noWorkerSts := *lws.Spec.LeaderWorkerTemplate.Size == 1

	// Iterate through all leaderPods.
	for _, pod := range leaderPodList.Items {
		index, err := strconv.Atoi(pod.Labels[leaderworkerset.GroupIndexLabelKey])
		if err != nil {
			return false, err
		}
		if index < int(*lws.Spec.Replicas) {
			currentNonBurstWorkerCount++
		}

		var sts appsv1.StatefulSet
		if !noWorkerSts {
			if err := r.Get(ctx, client.ObjectKey{Namespace: lws.Namespace, Name: pod.Name}, &sts); err != nil {
				log.Error(err, "Fetching worker statefulSet")
				return false, err
			}
		}

		var ready, updated bool
		if (noWorkerSts || statefulsetutils.StatefulsetReady(sts)) && podutils.PodRunningAndReady(pod) {
			ready = true
			readyCount++
		}
		if (noWorkerSts || sts.Labels[leaderworkerset.TemplateRevisionHashKey] == templateHash) && pod.Labels[leaderworkerset.TemplateRevisionHashKey] == templateHash {
			updated = true
			updatedCount++
			if index < int(*lws.Spec.Replicas) {
				// Bursted replicas do not count when determining if rollingUpdate has been completed.
				updatedNonBurstWorkerCount++
			}
		}

		if ready && updated {
			// Bursted replicas should not be counted here.
			if index < int(*lws.Spec.Replicas) {
				updatedAndReadyCount++
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
	if updatedNonBurstWorkerCount < currentNonBurstWorkerCount {
		// upgradeInProgress is true when the upgrade replicas is smaller than the expected
		// number of total replicas not including the burst replicas
		conditions = append(conditions, makeCondition(leaderworkerset.LeaderWorkerSetProgressing))
		conditions = append(conditions, makeCondition(leaderworkerset.LeaderWorkerSetUpgradeInProgress))
	} else if updatedAndReadyCount == int(*lws.Spec.Replicas) {
		conditions = append(conditions, makeCondition(leaderworkerset.LeaderWorkerSetAvailable))
	} else {
		conditions = append(conditions, makeCondition(leaderworkerset.LeaderWorkerSetProgressing))
	}

	updateCondition := setConditions(lws, conditions)
	// if condition changed, record events
	if updateCondition {
		r.Record.Eventf(lws, corev1.EventTypeNormal, conditions[0].Reason, conditions[0].Message+fmt.Sprintf(", with %d groups ready of total %d groups", readyCount, int(*lws.Spec.Replicas)))
	}
	return updateStatus || updateCondition, nil
}

// Updates status and condition of LeaderWorkerSet and returns whether or not an update actually occurred.
func (r *LeaderWorkerSetReconciler) updateStatus(ctx context.Context, lws *leaderworkerset.LeaderWorkerSet) error {
	updateStatus := false
	log := ctrl.LoggerFrom(ctx)

	// Retrieve the leader StatefulSet.
	sts := &appsv1.StatefulSet{}
	if err := r.Get(ctx, types.NamespacedName{Name: lws.Name, Namespace: lws.Namespace}, sts); err != nil {
		log.Error(err, "Error retrieving leader StatefulSet")
		return err
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
			return err
		}

		lws.Status.HPAPodSelector = selector.String()
		updateStatus = true
	}

	// check if an update is needed
	updateConditions, err := r.updateConditions(ctx, lws)
	if err != nil {
		return err
	}
	if updateStatus || updateConditions {
		if err := r.Status().Update(ctx, lws); err != nil {
			log.Error(err, "Updating LeaderWorkerSet status and/or condition.")
			return err
		}
	}
	return nil
}

// iterateReplicas will iterate the leader pods together with corresponding worker statefulsets
// to check the replica state, and return two values and an error in the end:
//   - The first value represents the number of continuous ready replicas ranging from the last index to 0,
//     to help us judge whether we can update the Partition or not.
//   - The second value represents the unready replicas whose index is smaller than leaderWorkerSet Replicas.
func (r *LeaderWorkerSetReconciler) iterateReplicas(ctx context.Context, lws *leaderworkerset.LeaderWorkerSet, stsReplicas int32) (int32, int32, error) {
	podSelector := client.MatchingLabels(map[string]string{
		leaderworkerset.SetNameLabelKey:     lws.Name,
		leaderworkerset.WorkerIndexLabelKey: "0",
	})
	var leaderPodList corev1.PodList
	if err := r.List(ctx, &leaderPodList, podSelector, client.InNamespace(lws.Namespace)); err != nil {
		return 0, 0, err
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
		return 0, 0, err
	}
	sortedSts := utils.SortByIndex(func(sts appsv1.StatefulSet) (int, error) {
		return strconv.Atoi(sts.Labels[leaderworkerset.GroupIndexLabelKey])
	}, stsList.Items, int(stsReplicas))

	templateHash := utils.LeaderWorkerTemplateHash(lws)
	// Once size==1, no worker statefulSets will be created.
	noWorkerSts := *lws.Spec.LeaderWorkerTemplate.Size == 1
	processReplica := func(index int32) (ready bool) {
		nominatedName := fmt.Sprintf("%s-%d", lws.Name, index)
		// It can happen that the leader pod or the worker statefulset hasn't created yet
		// or under rebuilding, which also indicates not ready.
		if nominatedName != sortedPods[index].Name || (!noWorkerSts && nominatedName != sortedSts[index].Name) {
			return false
		}

		podTemplateHash := sortedPods[index].Labels[leaderworkerset.TemplateRevisionHashKey]
		if !(podTemplateHash == templateHash && podutils.PodRunningAndReady(sortedPods[index])) {
			return false
		}

		if noWorkerSts {
			return true
		}

		stsTemplateHash := sortedSts[index].Labels[leaderworkerset.TemplateRevisionHashKey]
		return stsTemplateHash == templateHash && statefulsetutils.StatefulsetReady(sortedSts[index])
	}

	var skip bool
	var continuousReadyReplicas, lwsUnreadyReplicas int32

	for index := stsReplicas - 1; index >= 0; index-- {
		replicaReady := processReplica(index)
		skip = skip || !replicaReady

		if replicaReady && !skip {
			continuousReadyReplicas++
		}
		if !replicaReady && index < *lws.Spec.Replicas {
			lwsUnreadyReplicas++
		}
	}

	return continuousReadyReplicas, lwsUnreadyReplicas, nil
}

// constructLeaderStatefulSetApplyConfiguration constructs the applied configuration for the leader StatefulSet
func constructLeaderStatefulSetApplyConfiguration(lws *leaderworkerset.LeaderWorkerSet, partition, replicas int32) (*appsapplyv1.StatefulSetApplyConfiguration, error) {
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

	templateHash := utils.LeaderWorkerTemplateHash(lws)
	podTemplateApplyConfiguration.WithLabels(map[string]string{
		leaderworkerset.WorkerIndexLabelKey:     "0",
		leaderworkerset.SetNameLabelKey:         lws.Name,
		leaderworkerset.TemplateRevisionHashKey: templateHash,
	})
	podAnnotations := make(map[string]string)
	podAnnotations[leaderworkerset.SizeAnnotationKey] = strconv.Itoa(int(*lws.Spec.LeaderWorkerTemplate.Size))
	if lws.Annotations[leaderworkerset.ExclusiveKeyAnnotationKey] != "" {
		podAnnotations[leaderworkerset.ExclusiveKeyAnnotationKey] = lws.Annotations[leaderworkerset.ExclusiveKeyAnnotationKey]
	}
	if lws.Spec.LeaderWorkerTemplate.SubGroupPolicy != nil {
		podAnnotations[leaderworkerset.SubGroupSizeAnnotationKey] = strconv.Itoa(int(*lws.Spec.LeaderWorkerTemplate.SubGroupPolicy.SubGroupSize))
		if lws.Annotations[leaderworkerset.SubGroupExclusiveKeyAnnotationKey] != "" {
			podAnnotations[leaderworkerset.SubGroupExclusiveKeyAnnotationKey] = lws.Annotations[leaderworkerset.SubGroupExclusiveKeyAnnotationKey]
		}
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
			leaderworkerset.SetNameLabelKey:         lws.Name,
			leaderworkerset.TemplateRevisionHashKey: templateHash,
		}).
		WithAnnotations(map[string]string{
			leaderworkerset.ReplicasAnnotationKey: strconv.Itoa(int(*lws.Spec.Replicas)),
		})
	return statefulSetConfig, nil
}

func makeCondition(conditionType leaderworkerset.LeaderWorkerSetConditionType) metav1.Condition {
	var condtype, reason, message string
	switch conditionType {
	case leaderworkerset.LeaderWorkerSetAvailable:
		condtype = string(leaderworkerset.LeaderWorkerSetAvailable)
		reason = "AllGroupsReady"
		message = "All replicas are ready"
	case leaderworkerset.LeaderWorkerSetUpgradeInProgress:
		condtype = string(leaderworkerset.LeaderWorkerSetUpgradeInProgress)
		reason = "GroupsAreUpgrading"
		message = "Rolling Upgrade is in progress"
	default:
		condtype = string(leaderworkerset.LeaderWorkerSetProgressing)
		reason = "GroupsAreProgressing"
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

	if (condition1.Type == string(leaderworkerset.LeaderWorkerSetAvailable) && condition2.Type == string(leaderworkerset.LeaderWorkerSetUpgradeInProgress)) ||
		(condition1.Type == string(leaderworkerset.LeaderWorkerSetUpgradeInProgress) && condition2.Type == string(leaderworkerset.LeaderWorkerSetAvailable)) {
		return true
	}

	return false
}

func templateUpdated(sts *appsv1.StatefulSet, lws *leaderworkerset.LeaderWorkerSet) bool {
	return sts.Labels[leaderworkerset.TemplateRevisionHashKey] != utils.LeaderWorkerTemplateHash(lws)
}

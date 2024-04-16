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

	partition, err := r.rollingUpdatePartition(ctx, lws)
	if err != nil {
		log.Error(err, "rolling partition error")
		return ctrl.Result{}, err
	}

	if err := r.SSAWithStatefulset(ctx, lws, partition); err != nil {
		return ctrl.Result{}, err
	}

	// Create headless service if it does not exist.
	if err := r.createHeadlessServiceIfNotExists(ctx, lws); err != nil {
		log.Error(err, "Creating headless service.")
		return ctrl.Result{}, err
	}

	err = r.updateStatus(ctx, lws)
	if err != nil {
		return ctrl.Result{}, err
	}

	log.V(2).Info("Leader Reconcile completed.")
	return ctrl.Result{}, nil
}

func (r *LeaderWorkerSetReconciler) createHeadlessServiceIfNotExists(ctx context.Context, lws *leaderworkerset.LeaderWorkerSet) error {
	log := ctrl.LoggerFrom(ctx)
	// If the headless service does not exist in the namespace, create it.
	var headlessService corev1.Service
	if err := r.Get(ctx, types.NamespacedName{Name: lws.Name, Namespace: lws.Namespace}, &headlessService); err != nil {
		if client.IgnoreNotFound(err) != nil {
			return err
		}
		headlessService := corev1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Name:      lws.Name,
				Namespace: lws.Namespace,
			},
			Spec: corev1.ServiceSpec{
				ClusterIP: "None", // defines service as headless
				Selector: map[string]string{
					leaderworkerset.SetNameLabelKey: lws.Name,
				},
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
// Possible scenarios:
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
func (r *LeaderWorkerSetReconciler) rollingUpdatePartition(ctx context.Context, lws *leaderworkerset.LeaderWorkerSet) (int32, error) {
	sts := &appsv1.StatefulSet{}
	err := r.Get(ctx, types.NamespacedName{Name: lws.Name, Namespace: lws.Namespace}, sts)
	if err != nil {
		// If sts not created yet, all partitions should be updated.
		if apierrors.IsNotFound(err) {
			return 0, nil
		}
		return 0, err
	}

	partition := *sts.Spec.UpdateStrategy.RollingUpdate.Partition
	replicas := *lws.Spec.Replicas
	rollingStep, err := intstr.GetValueFromIntOrPercent(&lws.Spec.RolloutStrategy.RollingUpdateConfiguration.MaxUnavailable, int(replicas), false)
	if err != nil {
		return 0, err
	}

	// Indicates a new rolling update here.
	if templateUpdated(sts, lws) {
		// Processing scaling up/down first prior to rolling update.
		if replicasUpdated(sts, lws) {
			return min(*lws.Spec.Replicas, *sts.Spec.Replicas), nil
		} else {
			return utils.NonZeroValue(replicas - int32(rollingStep)), nil
		}
	}

	// Replicas changes during rolling update.
	if replicasUpdated(sts, lws) {
		return min(partition, *lws.Spec.Replicas), nil
	}

	// If Partition is 0, it means it's under creation or rolling update is done or
	// just running normally, return 0 directly under these cases.
	if partition == 0 {
		return 0, nil
	}

	// Calculating the Partition during rolling update, no leaderWorkerSet updates happens.

	podSelector := client.MatchingLabels(map[string]string{
		leaderworkerset.SetNameLabelKey:     lws.Name,
		leaderworkerset.WorkerIndexLabelKey: "0",
	})
	var leaderPodList corev1.PodList
	if err := r.List(ctx, &leaderPodList, podSelector, client.InNamespace(lws.Namespace)); err != nil {
		return 0, err
	}
	// Got a sorted leader pod list matches with the following sorted statefulsets one by one, which means
	// the leader pod and the corresponding worker statefulset has the same index.
	sortedPods := utils.SortByIndex(func(pod corev1.Pod) (int, error) {
		return strconv.Atoi(pod.Labels[leaderworkerset.GroupIndexLabelKey])
	}, leaderPodList.Items, int(replicas))

	stsSelector := client.MatchingLabels(map[string]string{
		leaderworkerset.SetNameLabelKey: lws.Name,
	})
	var stsList appsv1.StatefulSetList
	if err := r.List(ctx, &stsList, stsSelector, client.InNamespace(lws.Namespace)); err != nil {
		return 0, err
	}

	sortedSts := utils.SortByIndex(func(sts appsv1.StatefulSet) (int, error) {
		return strconv.Atoi(sts.Labels[leaderworkerset.GroupIndexLabelKey])
	}, stsList.Items, int(replicas))

	var finishedPart int32
	templateHash := utils.LeaderWorkerTemplateHash(lws)

	for i := replicas - 1; i >= 0; i-- {
		nominatedName := fmt.Sprintf("%s-%d", lws.Name, i)
		// It can happen that the leader pod or the worker statefulset hasn't created yet
		// or under rebuilding, which also indicates not ready.
		if nominatedName != sortedPods[i].Name || nominatedName != sortedSts[i].Name {
			break
		}

		podTemplateHash := sortedPods[i].Labels[leaderworkerset.TemplateRevisionHashKey]
		if !(podTemplateHash == templateHash && podutils.PodRunningAndReady(sortedPods[i])) {
			break
		}

		stsTemplateHash := sortedSts[i].Labels[leaderworkerset.TemplateRevisionHashKey]
		if !(stsTemplateHash == templateHash && statefulsetutils.StatefulsetReady(sortedSts[i])) {
			break
		}

		finishedPart++
	}

	// When updated replicas become not ready again or scaled up replicas are not ready yet,
	// we'll not modify the Partition field. That means Partition moves in one direction to make it simple.
	return min(partition, utils.NonZeroValue(replicas-int32(rollingStep)-finishedPart)), nil
}

func (r *LeaderWorkerSetReconciler) SSAWithStatefulset(ctx context.Context, lws *leaderworkerset.LeaderWorkerSet, partition int32) error {
	log := ctrl.LoggerFrom(ctx)

	// construct the statefulset apply configuration
	leaderStatefulSetApplyConfig, err := constructLeaderStatefulSetApplyConfiguration(lws, partition)
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
	stsSelector := client.MatchingLabels(map[string]string{
		leaderworkerset.SetNameLabelKey: lws.Name,
	})

	// update the condition based on the status of all statefulsets owned by the lws.
	var lwssts appsv1.StatefulSetList
	if err := r.List(ctx, &lwssts, stsSelector, client.InNamespace(lws.Namespace)); err != nil {
		log.Error(err, "Fetching statefulsets managed by leaderworkerset instance")
		return false, err
	}

	updateStatus := false
	readyCount := 0
	updatedCount := 0
	templateHash := utils.LeaderWorkerTemplateHash(lws)

	// Iterate through all statefulsets.
	for _, sts := range lwssts.Items {
		if sts.Name == lws.Name {
			continue
		}

		// this is the worker statefulset.
		if statefulsetutils.StatefulsetReady(sts) {

			// the worker pods are OK.
			// need to check leader pod for this group.
			var leaderPod corev1.Pod
			if err := r.Get(ctx, client.ObjectKey{Namespace: lws.Namespace, Name: sts.Name}, &leaderPod); err != nil {
				log.Error(err, "Fetching leader pod")
				return false, err
			}
			if podutils.PodRunningAndReady(leaderPod) {
				readyCount++

				if sts.Labels[leaderworkerset.TemplateRevisionHashKey] == templateHash && leaderPod.Labels[leaderworkerset.TemplateRevisionHashKey] == templateHash {
					updatedCount++
				}
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

	condition := makeCondition(updatedCount == int(*lws.Spec.Replicas))
	updateCondition := setCondition(lws, condition)
	// if condition changed, record events
	if updateCondition {
		r.Record.Eventf(lws, corev1.EventTypeNormal, condition.Reason, condition.Message+fmt.Sprintf(", with %d groups ready of total %d groups", readyCount, int(*lws.Spec.Replicas)))
	}
	return updateStatus || updateCondition, nil
}

// Updates status and condition of LeaderWorkerSet and returns whether or not an upate actually occurred.
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
	replicas := int(*sts.Spec.Replicas)
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

// constructLeaderStatefulSetApplyConfiguration constructs the applied configuration for the leader StatefulSet
func constructLeaderStatefulSetApplyConfiguration(lws *leaderworkerset.LeaderWorkerSet, partition int32) (*appsapplyv1.StatefulSetApplyConfiguration, error) {
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
	podTemplateApplyConfiguration.WithAnnotations(podAnnotations)

	// construct statefulset apply configuration
	statefulSetConfig := appsapplyv1.StatefulSet(lws.Name, lws.Namespace).
		WithSpec(appsapplyv1.StatefulSetSpec().
			WithServiceName(lws.Name).
			WithReplicas(*lws.Spec.Replicas).
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
		})
	return statefulSetConfig, nil
}

func makeCondition(available bool) metav1.Condition {
	condtype := string(leaderworkerset.LeaderWorkerSetProgressing)
	reason := "GroupsAreProgressing"
	message := "Replicas are progressing"

	if available {
		condtype = string(leaderworkerset.LeaderWorkerSetAvailable)
		reason = "AllGroupsReady"
		message = "All replicas are ready"
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
	return false
}

func templateUpdated(sts *appsv1.StatefulSet, lws *leaderworkerset.LeaderWorkerSet) bool {
	return sts.Labels[leaderworkerset.TemplateRevisionHashKey] != utils.LeaderWorkerTemplateHash(lws)
}

func replicasUpdated(sts *appsv1.StatefulSet, lws *leaderworkerset.LeaderWorkerSet) bool {
	return *sts.Spec.Replicas != *lws.Spec.Replicas
}

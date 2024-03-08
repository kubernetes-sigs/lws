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
	"strconv"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	appsapplyv1 "k8s.io/client-go/applyconfigurations/apps/v1"
	coreapplyv1 "k8s.io/client-go/applyconfigurations/core/v1"
	metaapplyv1 "k8s.io/client-go/applyconfigurations/meta/v1"
	"k8s.io/klog/v2"
	"k8s.io/utils/pointer"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	leaderworkerset "sigs.k8s.io/lws/api/leaderworkerset/v1"
	podutils "sigs.k8s.io/lws/pkg/utils/pod"
	statefulsetutils "sigs.k8s.io/lws/pkg/utils/statefulset"
)

// PodReconciler reconciles a LeaderWorkerSet object
type PodReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

func NewPodReconciler(client client.Client, schema *runtime.Scheme) *PodReconciler {
	return &PodReconciler{Client: client, Scheme: schema}
}

//+kubebuilder:rbac:groups=core,resources=pods,verbs=create;delete;get;list;patch;update;watch
//+kubebuilder:rbac:groups=core,resources=nodes,verbs=get;list;watch;update;patch

func (r *PodReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var pod corev1.Pod
	if err := r.Get(ctx, types.NamespacedName{Name: req.Name, Namespace: req.Namespace}, &pod); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	log := ctrl.LoggerFrom(ctx).WithValues("pod", klog.KObj(&pod))
	ctx = ctrl.LoggerInto(ctx, log)

	// get the leaderWorkerSet name
	lwsName := pod.Labels[leaderworkerset.SetNameLabelKey]
	if lwsName == "" {
		return ctrl.Result{}, errors.New("leaderworkerset.sigs.k8s.io/name label is unexpected missing")
	}
	if _, exist := pod.Labels[leaderworkerset.WorkerIndexLabelKey]; !exist {
		return ctrl.Result{}, errors.New("leaderworkerset.sigs.k8s.io/worker-index label is unexpected missing")
	}
	// get the leaderWorkerSet object
	var leaderWorkerSet leaderworkerset.LeaderWorkerSet
	if err := r.Get(ctx, types.NamespacedName{Name: lwsName, Namespace: pod.Namespace}, &leaderWorkerSet); err != nil {
		return ctrl.Result{}, err
	}
	leaderDeleted, err := r.handleRestartPolicy(ctx, pod, leaderWorkerSet)
	if err != nil {
		return ctrl.Result{}, err
	}
	if leaderDeleted {
		log.V(2).Info("restarting the group")
		return ctrl.Result{}, nil
	}

	// worker pods' reconciliation is only done to handle restart policy
	if !podutils.LeaderPod(pod) {
		return ctrl.Result{}, nil
	}

	// if it's not leader pod or leader pod is being deleted, we should not create the worker statefulset
	// this is critical to avoid race condition in all-or-nothing restart where the worker sts may be created
	// when the leader pod is being deleted
	if pod.DeletionTimestamp != nil {
		log.V(2).Info("skip creating the worker sts since the leader pod is being deleted")
		return ctrl.Result{}, nil
	}

	// logic for handling leader pod
	statefulSet, err := constructWorkerStatefulSetApplyConfiguration(pod, leaderWorkerSet)
	// if exclusive placement is enabled but leader pod is not scheduled, don't create the worker sts
	if topologyKey, found := leaderWorkerSet.Annotations[leaderworkerset.ExclusiveKeyAnnotationKey]; found {
		// check if the leader pod is scheduleds
		if pod.Spec.NodeName == "" {
			log.V(2).Info(fmt.Sprintf("Pod %q is not scheduled yet", pod.Name))
			return ctrl.Result{}, nil
		}
		if err := r.setNodeSelectorForWorkerPods(ctx, &pod, statefulSet, topologyKey); err != nil {
			log.Error(err, "setting node selector for worker pods")
			return ctrl.Result{}, err
		}
	}

	if err != nil {
		return ctrl.Result{}, err
	}
	if err := setControllerReferenceWithStatefulSet(&pod, statefulSet, r.Scheme); err != nil {
		log.Error(err, "Setting controller reference.")
		return ctrl.Result{}, nil
	}
	obj, err := runtime.DefaultUnstructuredConverter.ToUnstructured(statefulSet)
	if err != nil {
		return ctrl.Result{}, err
	}
	patch := &unstructured.Unstructured{
		Object: obj,
	}
	// Use server side apply and add fieldmanagaer to the lws owned fields
	// If there are conflicts in the fields owned by the lws controller, lws will obtain the ownership and force override
	// these fields to the ones desired by the lws controller. These fields are specified in the StatefulSetApplyConfiguration
	// TODO b/316776287 add E2E test for SSA
	err = r.Patch(ctx, patch, client.Apply, &client.PatchOptions{
		FieldManager: fieldManager,
		Force:        pointer.Bool(true),
	})
	if err != nil {
		return ctrl.Result{}, err
	}
	log.V(2).Info("Worker Reconcile completed.")
	return ctrl.Result{}, nil
}

func (r *PodReconciler) handleRestartPolicy(ctx context.Context, pod corev1.Pod, leaderWorkerSet leaderworkerset.LeaderWorkerSet) (bool, error) {
	if leaderWorkerSet.Spec.LeaderWorkerTemplate.RestartPolicy != leaderworkerset.RecreateGroupOnPodRestart {
		return false, nil
	}
	// the leader pod will be deleted if the worker pod is deleted or any containes were restarted
	if !podutils.ContainerRestarted(pod) && !podutils.PodDeleted(pod) {
		return false, nil
	}
	var leader corev1.Pod
	if !podutils.LeaderPod(pod) {
		leaderPodName, ordinal := statefulsetutils.GetParentNameAndOrdinal(pod.Name)
		if ordinal == -1 {
			return false, fmt.Errorf("parsing pod name for pod %s", pod.Name)
		}
		if err := r.Get(ctx, types.NamespacedName{Name: leaderPodName, Namespace: pod.Namespace}, &leader); err != nil {
			return false, err
		}
	} else {
		leader = pod
	}
	// if the leader pod is being deleted, we don't need to send deletion requests
	if leader.DeletionTimestamp != nil {
		return true, nil
	}
	deletionOpt := metav1.DeletePropagationForeground
	if err := r.Delete(ctx, &leader, &client.DeleteOptions{
		PropagationPolicy: &deletionOpt,
	}); err != nil {
		return false, err
	}
	return true, nil
}

func (r *PodReconciler) setNodeSelectorForWorkerPods(ctx context.Context, pod *corev1.Pod, sts *appsapplyv1.StatefulSetApplyConfiguration, topologyKey string) error {
	log := ctrl.LoggerFrom(ctx)
	topologyValue, err := r.topologyValueFromPod(ctx, pod, topologyKey)
	if err != nil {
		log.Error(err, "getting topology from leader pod")
		return err
	}

	// set node selector for worker pods, if worker pods already scheduled to different topology value
	// the following applying logic will automatically update it to match the leader pods, so we don't
	// need to verify if they have the same topology value
	sts.Spec.Template.Spec.WithNodeSelector(map[string]string{
		topologyKey: topologyValue,
	})
	return nil
}

func (r *PodReconciler) topologyValueFromPod(ctx context.Context, pod *corev1.Pod, topologyKey string) (string, error) {
	log := ctrl.LoggerFrom(ctx)

	nodeName := pod.Spec.NodeName
	ns := pod.Namespace

	// Get node the leader pod is running on.
	var node corev1.Node
	if err := r.Get(ctx, types.NamespacedName{Name: nodeName, Namespace: ns}, &node); err != nil {
		// We'll ignore not-found errors, since there is nothing we can do here.
		// A node may not exist temporarily due to a maintenance event or other scenarios.
		log.Error(err, fmt.Sprintf("getting node %s", nodeName))
		return "", client.IgnoreNotFound(err)
	}

	// Get topology (e.g. node pool name) from node labels.
	topology, exists := node.Labels[topologyKey]
	if !exists {
		return "", fmt.Errorf("node does not have topology label: %s", topology)
	}
	return topology, nil
}

// constructLeaderStatefulSetApplyConfiguration constructs the apply configuration for the leader StatefulSet
func constructLeaderStatefulSetApplyConfiguration(lws *leaderworkerset.LeaderWorkerSet) (*appsapplyv1.StatefulSetApplyConfiguration, error) {
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
			WithServiceName(lws.Name).
			WithSelector(metaapplyv1.LabelSelector().
				WithMatchLabels(map[string]string{
					leaderworkerset.SetNameLabelKey:     lws.Name,
					leaderworkerset.WorkerIndexLabelKey: "0",
				}))).
		WithLabels(map[string]string{
			leaderworkerset.SetNameLabelKey: lws.Name,
		})
	return statefulSetConfig, nil
}

// setControllerReferenceWithStatefulSet set controller reference for the StatefulSet
func setControllerReferenceWithStatefulSet(owner metav1.Object, sts *appsapplyv1.StatefulSetApplyConfiguration, scheme *runtime.Scheme) error {
	// Validate the owner.
	ro, ok := owner.(runtime.Object)
	if !ok {
		return fmt.Errorf("%T is not a runtime.Object, cannot call SetOwnerReference", owner)
	}
	gvk, err := apiutil.GVKForObject(ro, scheme)
	if err != nil {
		return err
	}
	sts.WithOwnerReferences(metaapplyv1.OwnerReference().
		WithAPIVersion(gvk.GroupVersion().String()).
		WithKind(gvk.Kind).
		WithName(owner.GetName()).
		WithUID(owner.GetUID()).
		WithBlockOwnerDeletion(true).
		WithController(true))
	return nil
}

// constructWorkerStatefulSetApplyConfiguration constructs the apply configuration for the leader StatefulSet
func constructWorkerStatefulSetApplyConfiguration(pod corev1.Pod, lws leaderworkerset.LeaderWorkerSet) (*appsapplyv1.StatefulSetApplyConfiguration, error) {
	podTemplateSpec := *lws.Spec.LeaderWorkerTemplate.WorkerTemplate.DeepCopy()
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
	labelMap := map[string]string{
		leaderworkerset.GroupIndexLabelKey:      pod.Labels[leaderworkerset.GroupIndexLabelKey],
		leaderworkerset.SetNameLabelKey:         lws.Name,
		leaderworkerset.GroupUniqueHashLabelKey: pod.Labels[leaderworkerset.GroupUniqueHashLabelKey],
	}
	podTemplateApplyConfiguration.WithLabels(labelMap)
	podAnnotations := make(map[string]string)
	podAnnotations[leaderworkerset.SizeAnnotationKey] = strconv.Itoa(int(*lws.Spec.LeaderWorkerTemplate.Size))
	podAnnotations[leaderworkerset.LeaderPodNameAnnotationKey] = pod.Name
	if lws.Annotations[leaderworkerset.ExclusiveKeyAnnotationKey] != "" {
		podAnnotations[leaderworkerset.ExclusiveKeyAnnotationKey] = lws.Annotations[leaderworkerset.ExclusiveKeyAnnotationKey]
	}
	podTemplateApplyConfiguration.WithAnnotations(podAnnotations)
	// construct statefulset apply configuration
	statefulSetConfig := appsapplyv1.StatefulSet(pod.Name, pod.Namespace).
		WithSpec(appsapplyv1.StatefulSetSpec().
			WithServiceName(lws.Name).
			WithReplicas(*lws.Spec.LeaderWorkerTemplate.Size - 1).
			WithPodManagementPolicy(appsv1.ParallelPodManagement).
			WithTemplate(&podTemplateApplyConfiguration).
			WithOrdinals(appsapplyv1.StatefulSetOrdinals().WithStart(1)).
			WithSelector(metaapplyv1.LabelSelector().
				WithMatchLabels(labelMap))).
		WithLabels(labelMap)
	return statefulSetConfig, nil
}

func makeCondition(available bool) metav1.Condition {
	condtype := string(leaderworkerset.LeaderWorkerSetProgressing)
	reason := "GroupsAreProgressing"
	message := "Creating resources"

	if available {
		condtype = string(leaderworkerset.LeaderWorkerSetAvailable)
		reason = "AllGroupsReady"
		message = "all replicas are ready"
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

func exclusiveConditionTypes(condition1 metav1.Condition, condition2 metav1.Condition) bool {
	if (condition1.Type == string(leaderworkerset.LeaderWorkerSetAvailable) && condition2.Type == string(leaderworkerset.LeaderWorkerSetProgressing)) ||
		(condition1.Type == string(leaderworkerset.LeaderWorkerSetProgressing) && condition2.Type == string(leaderworkerset.LeaderWorkerSetAvailable)) {
		return true
	}
	return false
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

func (r *PodReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1.Pod{}).
		WithEventFilter(predicate.NewPredicateFuncs(func(object client.Object) bool {
			if pod, ok := object.(*corev1.Pod); ok {
				_, exist := pod.Labels[leaderworkerset.SetNameLabelKey]
				return exist
			}
			if statefulSet, ok := object.(*appsv1.StatefulSet); ok {
				_, exist := statefulSet.Labels[leaderworkerset.SetNameLabelKey]
				return exist
			}
			return false
		})).Owns(&appsv1.StatefulSet{}).Complete(r)
}

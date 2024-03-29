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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
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
	podutils "sigs.k8s.io/lws/pkg/utils/pod"
)

// LeaderWorkerSetReconciler reconciles a LeaderWorkerSet object
type LeaderWorkerSetReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	Record record.EventRecorder
}

var (
	lwsOwnerKey  = ".metadata.controller"
	apiGVStr     = leaderworkerset.GroupVersion.String()
	fieldManager = "lws"
)

func NewLeaderWorkerSetReconciler(client client.Client, scheme *runtime.Scheme, record record.EventRecorder) *LeaderWorkerSetReconciler {
	return &LeaderWorkerSetReconciler{Client: client, Scheme: scheme, Record: record}
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

	// construct the statefulset apply configuration
	leaderStatefulSetApplyConfig, err := constructLeaderStatefulSetApplyConfiguration(lws)
	if err != nil {
		log.Error(err, "Constructing StatefulSet apply configuration.")
		return ctrl.Result{}, err
	}
	if err := setControllerReferenceWithStatefulSet(lws, leaderStatefulSetApplyConfig, r.Scheme); err != nil {
		log.Error(err, "Setting controller reference.")
		return ctrl.Result{}, nil
	}
	obj, err := runtime.DefaultUnstructuredConverter.ToUnstructured(leaderStatefulSetApplyConfig)
	if err != nil {
		log.Error(err, "Converting StatefulSet configuration to json.")
		return ctrl.Result{}, err
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
		return ctrl.Result{}, err
	}

	stsSelector := client.MatchingLabels(map[string]string{
		leaderworkerset.SetNameLabelKey: lws.Name,
	})
	var lwssts appsv1.StatefulSetList
	if err := r.List(ctx, &lwssts, stsSelector, client.InNamespace(lws.Namespace)); err != nil {
		log.Error(err, "Fetching statefulsets managed by leaderworkerset instance")
		return ctrl.Result{}, nil
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

	// Iterate through all statefulsets.
	for _, sts := range lwssts.Items {
		if sts.Name == lws.Name {
			continue
		}
		// this is the worker statefulset.
		if sts.Status.ReadyReplicas == *sts.Spec.Replicas {
			// the worker pods are OK.
			// need to check leader pod for this group.
			var leaderPod corev1.Pod
			if err := r.Get(ctx, client.ObjectKey{Namespace: lws.Namespace, Name: sts.Name}, &leaderPod); err != nil {
				log.Error(err, "Fetching leader pod")
				return false, err
			}
			if podutils.PodRunningAndReady(&leaderPod) {
				// set to progressing.
				readyCount++
			}
		}
	}

	if lws.Status.ReadyReplicas != readyCount {
		lws.Status.ReadyReplicas = readyCount
		updateStatus = true
	}

	condition := makeCondition(readyCount == int(*lws.Spec.Replicas))
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
	if lws.Status.Replicas != replicas {
		lws.Status.Replicas = replicas
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

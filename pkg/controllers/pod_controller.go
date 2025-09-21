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
	"time"

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
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	leaderworkerset "sigs.k8s.io/lws/api/leaderworkerset/v1"
	"sigs.k8s.io/lws/pkg/schedulerprovider"
	acceleratorutils "sigs.k8s.io/lws/pkg/utils/accelerators"
	controllerutils "sigs.k8s.io/lws/pkg/utils/controller"
	podutils "sigs.k8s.io/lws/pkg/utils/pod"
	revisionutils "sigs.k8s.io/lws/pkg/utils/revision"
	statefulsetutils "sigs.k8s.io/lws/pkg/utils/statefulset"
)

// PodReconciler reconciles a LeaderWorkerSet object
type PodReconciler struct {
	client.Client
	Scheme            *runtime.Scheme
	Record            record.EventRecorder
	SchedulerProvider schedulerprovider.SchedulerProvider
}

func NewPodReconciler(client client.Client, schema *runtime.Scheme, record record.EventRecorder, sp schedulerprovider.SchedulerProvider) *PodReconciler {
	return &PodReconciler{Client: client, Scheme: schema, Record: record, SchedulerProvider: sp}
}

//+kubebuilder:rbac:groups="",resources=events,verbs=create;watch;update;patch
//+kubebuilder:rbac:groups=core,resources=pods,verbs=create;delete;get;list;patch;update;watch
//+kubebuilder:rbac:groups=core,resources=pods/finalizers,verbs=update
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
		// If lws not found, it's mostly because deleted, ignore the error as Pods will be GCed finally.
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	leaderDeleted, err := r.handleRestartPolicy(ctx, pod, leaderWorkerSet)
	if err != nil {
		return ctrl.Result{}, err
	}
	if leaderDeleted {
		return ctrl.Result{}, nil
	}

	// worker pods' reconciliation is only done to handle restart policy
	if !podutils.LeaderPod(pod) {
		return ctrl.Result{}, nil
	}

	// validate leader's annotations to prevent infinite StatefulSet creation loops
	// see issue: https://github.com/kubernetes-sigs/lws/issues/391
	if pod.Annotations[leaderworkerset.LeaderPodNameAnnotationKey] != "" {
		errMsg := fmt.Sprintf("leader pod %s/%s contains mistake annotation '%s': requires Kubernetes ≥v1.27 or v1.26 with StatefulSetStartOrdinal feature",
			pod.Namespace,
			pod.Name,
			leaderworkerset.LeaderPodNameAnnotationKey)
		log.Error(errors.New(errMsg), "validate leader's annotations")
		r.Record.Eventf(&leaderWorkerSet, corev1.EventTypeWarning, FailedCreate, errMsg)
		return ctrl.Result{}, nil
	}

	if leaderWorkerSet.Spec.NetworkConfig != nil && *leaderWorkerSet.Spec.NetworkConfig.SubdomainPolicy == leaderworkerset.SubdomainUniquePerReplica {
		if err := controllerutils.CreateHeadlessServiceIfNotExists(ctx, r.Client, r.Scheme, &leaderWorkerSet, pod.Name, map[string]string{leaderworkerset.SetNameLabelKey: leaderWorkerSet.Name, leaderworkerset.GroupIndexLabelKey: pod.Labels[leaderworkerset.GroupIndexLabelKey]}, &pod); err != nil {
			return ctrl.Result{}, err
		}
	}

	// if it's not leader pod or leader pod is being deleted, we should not create the worker statefulset
	// this is critical to avoid race condition in all-or-nothing restart where the worker sts may be created
	// when the leader pod is being deleted
	if pod.DeletionTimestamp != nil {
		log.V(2).Info("skip creating the worker sts since the leader pod is being deleted")
		return ctrl.Result{}, nil
	}

	if r.SchedulerProvider != nil {
		err = r.SchedulerProvider.CreatePodGroupIfNotExists(ctx, &leaderWorkerSet, &pod)
		if err != nil {
			return ctrl.Result{}, err
		}
	}

	// Once size = 1, no need to create worker statefulSets.
	if *leaderWorkerSet.Spec.LeaderWorkerTemplate.Size == 1 {
		return ctrl.Result{}, nil
	}

	// logic for handling leader pod
	if leaderWorkerSet.Spec.StartupPolicy == leaderworkerset.LeaderReadyStartupPolicy && !podutils.IsPodReady(&pod) {
		log.V(2).Info("defer the creation of the worker statefulset because leader pod is not ready.")
		return ctrl.Result{}, nil
	}
	revision, err := revisionutils.GetRevision(ctx, r.Client, &leaderWorkerSet, revisionutils.GetRevisionKey(&pod))
	if err != nil {
		log.Error(err, "Getting lws revisions")
		return ctrl.Result{}, err
	}
	if revision == nil {
		log.V(2).Info(fmt.Sprintf("Revision has not been created yet, requeing reconciler for pod %s", pod.Name))
		return ctrl.Result{Requeue: true, RequeueAfter: time.Second}, nil
	}
	statefulSet, err := constructWorkerStatefulSetApplyConfiguration(pod, leaderWorkerSet, revision)
	if err != nil {
		return ctrl.Result{}, err
	}

	// if exclusive placement is enabled but leader pod is not scheduled, don't create the worker sts
	if topologyKey, found := leaderWorkerSet.Annotations[leaderworkerset.ExclusiveKeyAnnotationKey]; found {
		// check if the leader pod is scheduled.
		if pod.Spec.NodeName == "" {
			log.V(2).Info(fmt.Sprintf("Pod %q is not scheduled yet", pod.Name))
			return ctrl.Result{}, nil
		}
		if err := r.setNodeSelectorForWorkerPods(ctx, &pod, statefulSet, topologyKey); err != nil {
			log.Error(err, "setting node selector for worker pods")
			return ctrl.Result{}, err
		}
	}

	if err := setControllerReferenceWithStatefulSet(&pod, statefulSet, r.Scheme); err != nil {
		log.Error(err, "Setting controller reference.")
		return ctrl.Result{}, nil
	}

	obj, err := runtime.DefaultUnstructuredConverter.ToUnstructured(statefulSet)
	if err != nil {
		return ctrl.Result{}, err
	}
	workerStatefulSet := &unstructured.Unstructured{
		Object: obj,
	}

	var workerSts appsv1.StatefulSet
	if err := r.Get(ctx, types.NamespacedName{Name: pod.Name, Namespace: leaderWorkerSet.Namespace}, &workerSts); err != nil {
		if client.IgnoreNotFound(err) != nil {
			return ctrl.Result{}, err
		}
		if err = r.Create(ctx, workerStatefulSet); err != nil {
			r.Record.Eventf(&leaderWorkerSet, corev1.EventTypeWarning, FailedCreate, fmt.Sprintf("Failed to create worker statefulset for leader pod %s", pod.Name))
			return ctrl.Result{}, client.IgnoreAlreadyExists(err)
		}
		r.Record.Eventf(&leaderWorkerSet, corev1.EventTypeNormal, GroupsProgressing, fmt.Sprintf("Created worker statefulset for leader pod %s", pod.Name))
	}
	log.V(2).Info("Worker Reconcile completed.")
	return ctrl.Result{}, nil
}

func (r *PodReconciler) handleRestartPolicy(ctx context.Context, pod corev1.Pod, leaderWorkerSet leaderworkerset.LeaderWorkerSet) (bool, error) {
	if leaderWorkerSet.Spec.LeaderWorkerTemplate.RestartPolicy != leaderworkerset.RecreateGroupOnPodRestart {
		return false, nil
	}
	// the leader pod will be deleted if the worker pod is deleted or any container was restarted
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
			// If the error is not found, it is likely caused by the fact that the leader was deleted but the worker statefulset
			// deletion hasn't deleted all the worker pods
			return false, client.IgnoreNotFound(err)
		}
		// Different revision key means that this pod will be deleted soon and alternative will be created with the matching key
		if revisionutils.GetRevisionKey(&leader) != revisionutils.GetRevisionKey(&pod) {
			return false, nil
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
	r.Record.Eventf(&leaderWorkerSet, corev1.EventTypeNormal, "RecreateGroupOnPodRestart", fmt.Sprintf("Worker pod %s failed, deleted leader pod %s to recreate group %s", pod.Name, leader.Name, leader.Labels[leaderworkerset.GroupIndexLabelKey]))
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

// constructWorkerStatefulSetApplyConfiguration constructs the applied configuration for the leader StatefulSet
func constructWorkerStatefulSetApplyConfiguration(leaderPod corev1.Pod, lws leaderworkerset.LeaderWorkerSet, currentRevision *appsv1.ControllerRevision) (*appsapplyv1.StatefulSetApplyConfiguration, error) {
	currentLws, err := revisionutils.ApplyRevision(&lws, currentRevision)
	if err != nil {
		return nil, err
	}
	podTemplateSpec := *currentLws.Spec.LeaderWorkerTemplate.WorkerTemplate.DeepCopy()
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
	selectorMap := map[string]string{
		leaderworkerset.GroupIndexLabelKey:      leaderPod.Labels[leaderworkerset.GroupIndexLabelKey],
		leaderworkerset.SetNameLabelKey:         lws.Name,
		leaderworkerset.GroupUniqueHashLabelKey: leaderPod.Labels[leaderworkerset.GroupUniqueHashLabelKey],
	}
	labelMap := map[string]string{
		leaderworkerset.GroupIndexLabelKey:      leaderPod.Labels[leaderworkerset.GroupIndexLabelKey],
		leaderworkerset.SetNameLabelKey:         lws.Name,
		leaderworkerset.GroupUniqueHashLabelKey: leaderPod.Labels[leaderworkerset.GroupUniqueHashLabelKey],
		leaderworkerset.RevisionKey:             revisionutils.GetRevisionKey(&leaderPod),
	}

	podTemplateApplyConfiguration.WithLabels(labelMap)
	podAnnotations := make(map[string]string)
	podAnnotations[leaderworkerset.SizeAnnotationKey] = strconv.Itoa(int(*lws.Spec.LeaderWorkerTemplate.Size))
	podAnnotations[leaderworkerset.LeaderPodNameAnnotationKey] = leaderPod.Name
	if lws.Annotations[leaderworkerset.ExclusiveKeyAnnotationKey] != "" {
		podAnnotations[leaderworkerset.ExclusiveKeyAnnotationKey] = lws.Annotations[leaderworkerset.ExclusiveKeyAnnotationKey]
	}
	if lws.Spec.LeaderWorkerTemplate.SubGroupPolicy != nil {
		podAnnotations[leaderworkerset.SubGroupSizeAnnotationKey] = strconv.Itoa(int(*lws.Spec.LeaderWorkerTemplate.SubGroupPolicy.SubGroupSize))
		if lws.Annotations[leaderworkerset.SubGroupExclusiveKeyAnnotationKey] != "" {
			podAnnotations[leaderworkerset.SubGroupExclusiveKeyAnnotationKey] = lws.Annotations[leaderworkerset.SubGroupExclusiveKeyAnnotationKey]
		}
	}
	acceleratorutils.AddTPUAnnotations(leaderPod, podAnnotations)
	podTemplateApplyConfiguration.WithAnnotations(podAnnotations)
	serviceName := leaderPod.Name
	if lws.Spec.NetworkConfig == nil || *lws.Spec.NetworkConfig.SubdomainPolicy == leaderworkerset.SubdomainShared {
		serviceName = lws.Name
	}
	// construct statefulset apply configuration
	statefulSetConfig := appsapplyv1.StatefulSet(leaderPod.Name, leaderPod.Namespace).
		WithSpec(appsapplyv1.StatefulSetSpec().
			WithServiceName(serviceName).
			WithReplicas(*lws.Spec.LeaderWorkerTemplate.Size - 1).
			WithPodManagementPolicy(appsv1.ParallelPodManagement).
			WithTemplate(&podTemplateApplyConfiguration).
			WithOrdinals(appsapplyv1.StatefulSetOrdinals().WithStart(1)).
			WithSelector(metaapplyv1.LabelSelector().
				WithMatchLabels(selectorMap))).
		WithLabels(labelMap)

	pvcApplyConfiguration := controllerutils.GetPVCApplyConfiguration(&lws)
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

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
	"k8s.io/client-go/tools/events"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"
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
	Record            events.EventRecorder
	SchedulerProvider schedulerprovider.SchedulerProvider
}

// podReconcileRequest keeps the identity and snapshot of a deleted Pod in the
// workqueue. A StatefulSet can create a replacement with the same namespace
// and name before reconciliation starts, so a namespaced name alone is
// insufficient.
type podReconcileRequest struct {
	types.NamespacedName
	UID        types.UID
	DeletedPod *corev1.Pod
}

func podReconcileRequestForPod(pod *corev1.Pod, deleted bool) podReconcileRequest {
	request := podReconcileRequest{
		NamespacedName: client.ObjectKeyFromObject(pod),
		UID:            pod.UID,
	}
	if deleted {
		request.DeletedPod = pod.DeepCopy()
		if request.DeletedPod.DeletionTimestamp == nil {
			deletionTimestamp := metav1.Now()
			request.DeletedPod.DeletionTimestamp = &deletionTimestamp
		}
	}
	return request
}

func NewPodReconciler(client client.Client, schema *runtime.Scheme, record events.EventRecorder, sp schedulerprovider.SchedulerProvider) *PodReconciler {
	return &PodReconciler{Client: client, Scheme: schema, Record: record, SchedulerProvider: sp}
}

//+kubebuilder:rbac:groups="",resources=events,verbs=create;watch;update;patch
//+kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;watch;update;patch
//+kubebuilder:rbac:groups=core,resources=pods,verbs=delete;get;list;patch;update;watch
//+kubebuilder:rbac:groups=core,resources=pods/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=core,resources=pods/finalizers,verbs=update
//+kubebuilder:rbac:groups=core,resources=nodes,verbs=get;list;watch;update;patch

func (r *PodReconciler) reconcilePod(ctx context.Context, req podReconcileRequest) (ctrl.Result, error) {
	var pod corev1.Pod
	if req.DeletedPod != nil {
		pod = *req.DeletedPod.DeepCopy()
	} else if err := r.Get(ctx, req.NamespacedName, &pod); err != nil {
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
		r.Record.Eventf(&leaderWorkerSet, &pod, corev1.EventTypeWarning, FailedCreate, Create, errMsg)
		return ctrl.Result{}, nil
	}

	// if it's not leader pod or leader pod is being deleted, we should not create the worker statefulset or headless service
	// this is critical to avoid race condition in all-or-nothing restart where resources may be created
	// when the leader pod is being deleted
	if pod.DeletionTimestamp != nil {
		log.V(2).Info("skip creating worker sts and headless service since the leader pod is being deleted")
		return ctrl.Result{}, nil
	}

	if leaderWorkerSet.Spec.NetworkConfig != nil && *leaderWorkerSet.Spec.NetworkConfig.SubdomainPolicy == leaderworkerset.SubdomainUniquePerReplica {
		// The per-replica service is named after the leader's subdomain: the pod
		// name in ordinal mode, a group key derived name in hash mode.
		if err := controllerutils.CreateHeadlessServiceIfNotExists(ctx, r.Client, r.Scheme, &leaderWorkerSet, pod.Spec.Subdomain, map[string]string{leaderworkerset.SetNameLabelKey: leaderWorkerSet.Name, leaderworkerset.GroupIndexLabelKey: pod.Labels[leaderworkerset.GroupIndexLabelKey]}, &pod); err != nil {
			return ctrl.Result{}, err
		}
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

	hashIdentity := leaderWorkerSet.Spec.GroupIdentity == leaderworkerset.GroupIdentityHash

	// logic for handling leader pod
	if leaderWorkerSet.Spec.StartupPolicy == leaderworkerset.LeaderReadyStartupPolicy {
		leaderStarted := podutils.IsPodReady(&pod)
		if hashIdentity {
			// With hash identity, full pod readiness includes the group-ready gate,
			// which in turn waits for the workers. Gate worker creation on container
			// readiness instead to avoid a deadlock.
			leaderStarted = podutils.ContainersReady(&pod)
		}
		if !leaderStarted {
			log.V(2).Info("defer the creation of the worker statefulset because leader pod is not ready.")
			return ctrl.Result{}, nil
		}
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
	// Leader pods always have a DNS identity: the statefulset controller
	// assigns it in ordinal mode, admission in hash mode. The worker statefulset
	// service name and the leader address stamped on its template derive from it.
	if pod.Spec.Hostname == "" || pod.Spec.Subdomain == "" {
		return ctrl.Result{}, fmt.Errorf("leader pod %s/%s has no hostname or subdomain", pod.Namespace, pod.Name)
	}
	statefulSet, err := constructWorkerStatefulSetApplyConfiguration(pod, leaderWorkerSet, revision)
	if err != nil {
		return ctrl.Result{}, err
	}

	// Workers reach the leader through its DNS name, stamped on the worker
	// statefulset template so pod admission can inject LWS_LEADER_ADDRESS
	// without recomputing it.
	templateAnnotations := map[string]string{
		leaderworkerset.LeaderAddressAnnotationKey: fmt.Sprintf("%s.%s.%s", pod.Spec.Hostname, pod.Spec.Subdomain, pod.Namespace),
	}
	if hashIdentity {
		templateAnnotations[leaderworkerset.GroupIdentityAnnotationKey] = string(leaderworkerset.GroupIdentityHash)
	}
	statefulSet.Spec.Template.WithAnnotations(templateAnnotations)

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

	workerStsReady := false
	var workerSts appsv1.StatefulSet
	if err := r.Get(ctx, types.NamespacedName{Name: pod.Name, Namespace: leaderWorkerSet.Namespace}, &workerSts); err != nil {
		if client.IgnoreNotFound(err) != nil {
			return ctrl.Result{}, err
		}
		if err = r.Create(ctx, workerStatefulSet); err != nil {
			if client.IgnoreAlreadyExists(err) != nil {
				r.Record.Eventf(&leaderWorkerSet, &pod, corev1.EventTypeWarning, FailedCreate, Create, fmt.Sprintf("Failed to create worker statefulset for leader pod %s: %v", pod.Name, err))
			}
			return ctrl.Result{}, client.IgnoreAlreadyExists(err)
		}
		r.Record.Eventf(&leaderWorkerSet, &pod, corev1.EventTypeNormal, GroupsProgressing, Create, fmt.Sprintf("Created worker statefulset for leader pod %s", pod.Name))
	} else {
		workerStsReady = statefulsetutils.StatefulsetReady(workerSts)
	}

	if hashIdentity {
		// Maintain the group-ready readiness gate so Deployment rollout pacing
		// counts whole groups instead of bare leader pods.
		if err := r.syncGroupReadyCondition(ctx, &pod, workerStsReady); err != nil {
			return ctrl.Result{}, err
		}
	}
	log.V(2).Info("Worker Reconcile completed.")
	return ctrl.Result{}, nil
}

// syncGroupReadyCondition patches the leader pod's group-ready condition to match
// the readiness of its worker statefulset.
func (r *PodReconciler) syncGroupReadyCondition(ctx context.Context, pod *corev1.Pod, ready bool) error {
	status := corev1.ConditionFalse
	reason := "WorkerStatefulSetNotReady"
	if ready {
		status = corev1.ConditionTrue
		reason = "WorkerStatefulSetReady"
	}
	if _, existing := podutils.GetPodCondition(&pod.Status, leaderworkerset.GroupReadyConditionType); existing != nil && existing.Status == status {
		return nil
	}
	newPod := pod.DeepCopy()
	condition := corev1.PodCondition{
		Type:               leaderworkerset.GroupReadyConditionType,
		Status:             status,
		Reason:             reason,
		LastTransitionTime: metav1.Now(),
	}
	if idx, _ := podutils.GetPodCondition(&newPod.Status, leaderworkerset.GroupReadyConditionType); idx >= 0 {
		newPod.Status.Conditions[idx] = condition
	} else {
		newPod.Status.Conditions = append(newPod.Status.Conditions, condition)
	}
	return r.Status().Patch(ctx, newPod, client.MergeFrom(pod))
}

func (r *PodReconciler) handleRestartPolicy(ctx context.Context, pod corev1.Pod, leaderWorkerSet leaderworkerset.LeaderWorkerSet) (bool, error) {
	log := ctrl.LoggerFrom(ctx)
	policy := leaderWorkerSet.Spec.LeaderWorkerTemplate.RestartPolicy
	if policy != leaderworkerset.RecreateGroupOnPodRestart && policy != leaderworkerset.RecreateGroupAfterStart {
		return false, nil
	}
	// the leader pod will be deleted if the worker pod is deleted or any container was restarted
	if !podutils.ContainerRestarted(pod) && !podutils.PodDeleted(pod) {
		return false, nil
	}

	pendingPods, err := r.pendingPodsInGroup(ctx, pod, int(*leaderWorkerSet.Spec.LeaderWorkerTemplate.Size))
	if err != nil {
		return false, err
	}

	_, hasRecreateGroupAfterStartAnnotation := leaderWorkerSet.Annotations[leaderworkerset.RecreateGroupAfterStartAnnotationKey]

	if pendingPods && (policy == leaderworkerset.RecreateGroupAfterStart || hasRecreateGroupAfterStartAnnotation) {
		log.V(2).Info(fmt.Sprintf("Skipping group recreation because there is a pod pending: %s", pod.Name))
		return false, nil
	}

	var leader corev1.Pod
	if !podutils.LeaderPod(pod) {
		// Prefer the annotation over name parsing: with hash identity the leader
		// name is not ordinal-derived.
		leaderPodName := pod.Annotations[leaderworkerset.LeaderPodNameAnnotationKey]
		if leaderPodName == "" {
			var ordinal int
			leaderPodName, ordinal = statefulsetutils.GetParentNameAndOrdinal(pod.Name)
			if ordinal == -1 {
				return false, fmt.Errorf("parsing pod name for pod %s", pod.Name)
			}
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
		// Ignore worker pods from a stale worker StatefulSet (or test-owned direct pod) so
		// background deletion of the previous group does not recreate the replacement leader again.
		currentGroupWorkerPod, err := r.workerPodBelongsToLeader(ctx, pod, leader)
		if err != nil {
			return false, err
		}
		if !currentGroupWorkerPod {
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
	r.Record.Eventf(&leaderWorkerSet, &leader, corev1.EventTypeNormal, "RecreateGroup", Delete, fmt.Sprintf("Worker pod %s failed, deleted leader pod %s to recreate group %s", pod.Name, leader.Name, leader.Labels[leaderworkerset.GroupIndexLabelKey]))
	return true, nil
}

func (r *PodReconciler) workerPodBelongsToLeader(ctx context.Context, pod corev1.Pod, leader corev1.Pod) (bool, error) {
	owner := metav1.GetControllerOf(&pod)
	if owner == nil {
		return false, nil
	}

	if owner.Kind == "Pod" {
		return owner.Name == leader.Name && owner.UID == leader.UID, nil
	}

	if owner.Kind != "StatefulSet" {
		return false, nil
	}

	var workerSts appsv1.StatefulSet
	if err := r.Get(ctx, types.NamespacedName{Name: owner.Name, Namespace: pod.Namespace}, &workerSts); err != nil {
		return false, client.IgnoreNotFound(err)
	}
	if workerSts.UID != owner.UID {
		return false, nil
	}

	stsOwner := metav1.GetControllerOf(&workerSts)
	if stsOwner == nil {
		return false, nil
	}
	return stsOwner.Kind == "Pod" && stsOwner.Name == leader.Name && stsOwner.UID == leader.UID, nil
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
	nodeName := pod.Spec.NodeName
	ns := pod.Namespace

	// Get node the leader pod is running on.
	var node corev1.Node
	if err := r.Get(ctx, types.NamespacedName{Name: nodeName, Namespace: ns}, &node); err != nil {
		return "", fmt.Errorf("getting node %q: %w", nodeName, err)
	}

	// Get topology (e.g. node pool name) from node labels.
	topology, exists := node.Labels[topologyKey]
	if !exists {
		return "", fmt.Errorf("node does not have topology label: %s", topology)
	}
	return topology, nil
}

func (r *PodReconciler) pendingPodsInGroup(ctx context.Context, pod corev1.Pod, groupSize int) (bool, error) {
	groupIndex := pod.Labels[leaderworkerset.GroupIndexLabelKey]
	lwsName := pod.Labels[leaderworkerset.SetNameLabelKey]

	podSelector := client.MatchingLabels(map[string]string{
		leaderworkerset.SetNameLabelKey:    lwsName,
		leaderworkerset.GroupIndexLabelKey: groupIndex,
	})

	var podList corev1.PodList
	if err := r.List(ctx, &podList, podSelector, client.InNamespace(pod.Namespace)); err != nil {
		return false, err
	}

	if groupSize != len(podList.Items) {
		return true, nil
	}

	for _, groupPod := range podList.Items {
		if groupPod.Status.Phase == corev1.PodPending {
			return true, nil
		}
	}
	return false, nil
}

// setControllerReferenceWithStatefulSet set controller reference for the StatefulSet
func setControllerReferenceWithStatefulSet(owner metav1.Object, sts *appsapplyv1.StatefulSetApplyConfiguration, scheme *runtime.Scheme) error {
	ownerRef, err := controllerOwnerReference(owner, scheme)
	if err != nil {
		return err
	}
	sts.WithOwnerReferences(ownerRef)
	return nil
}

// controllerOwnerReference builds the owner reference apply configuration that
// marks owner as the managing controller.
func controllerOwnerReference(owner metav1.Object, scheme *runtime.Scheme) (*metaapplyv1.OwnerReferenceApplyConfiguration, error) {
	ro, ok := owner.(runtime.Object)
	if !ok {
		return nil, fmt.Errorf("%T is not a runtime.Object, cannot call SetOwnerReference", owner)
	}
	gvk, err := apiutil.GVKForObject(ro, scheme)
	if err != nil {
		return nil, err
	}
	return metaapplyv1.OwnerReference().
		WithAPIVersion(gvk.GroupVersion().String()).
		WithKind(gvk.Kind).
		WithName(owner.GetName()).
		WithUID(owner.GetUID()).
		WithBlockOwnerDeletion(true).
		WithController(true), nil
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
	// Spec-derived values must come from the revision-applied spec (currentLws), not the
	// live one: when an old-revision group is rebuilt mid rolling update, mixing the old
	// pod template with live size/subGroupPolicy/networkConfig breaks the group.
	podAnnotations[leaderworkerset.SizeAnnotationKey] = strconv.Itoa(int(*currentLws.Spec.LeaderWorkerTemplate.Size))
	podAnnotations[leaderworkerset.LeaderPodNameAnnotationKey] = leaderPod.Name
	if lws.Annotations[leaderworkerset.ExclusiveKeyAnnotationKey] != "" {
		podAnnotations[leaderworkerset.ExclusiveKeyAnnotationKey] = lws.Annotations[leaderworkerset.ExclusiveKeyAnnotationKey]
	}
	if currentLws.Spec.LeaderWorkerTemplate.SubGroupPolicy != nil {
		if currentLws.Spec.LeaderWorkerTemplate.SubGroupPolicy.Type != nil {
			podAnnotations[leaderworkerset.SubGroupPolicyTypeAnnotationKey] = string(*currentLws.Spec.LeaderWorkerTemplate.SubGroupPolicy.Type)
		}
		if currentLws.Spec.LeaderWorkerTemplate.SubGroupPolicy.SubGroupSize != nil {
			podAnnotations[leaderworkerset.SubGroupSizeAnnotationKey] = strconv.Itoa(int(*currentLws.Spec.LeaderWorkerTemplate.SubGroupPolicy.SubGroupSize))
		}
		if len(currentLws.Spec.LeaderWorkerTemplate.SubGroupPolicy.SubGroupPlacement) > 0 {
			encodedPlacement, err := leaderworkerset.EncodeSubGroupPlacement(currentLws.Spec.LeaderWorkerTemplate.SubGroupPolicy.SubGroupPlacement)
			if err != nil {
				return nil, err
			}
			podAnnotations[leaderworkerset.SubGroupPlacementAnnotationKey] = encodedPlacement
		}
		if lws.Annotations[leaderworkerset.SubGroupExclusiveKeyAnnotationKey] != "" {
			podAnnotations[leaderworkerset.SubGroupExclusiveKeyAnnotationKey] = lws.Annotations[leaderworkerset.SubGroupExclusiveKeyAnnotationKey]
		}
	}
	acceleratorutils.AddTPUAnnotations(leaderPod, podAnnotations)
	podTemplateApplyConfiguration.WithAnnotations(podAnnotations)
	// The service name always matches the leader's subdomain in every mode and
	// subdomain policy.
	serviceName := leaderPod.Spec.Subdomain
	// construct statefulset apply configuration
	statefulSetLabels := mergeMetadata(lws.Labels, labelMap)
	statefulSetLabels[leaderworkerset.RoleLabelKey] = leaderworkerset.RoleWorker
	statefulSetConfig := appsapplyv1.StatefulSet(leaderPod.Name, leaderPod.Namespace).
		WithSpec(appsapplyv1.StatefulSetSpec().
			WithServiceName(serviceName).
			WithReplicas(*currentLws.Spec.LeaderWorkerTemplate.Size - 1).
			WithPodManagementPolicy(appsv1.ParallelPodManagement).
			WithTemplate(&podTemplateApplyConfiguration).
			WithOrdinals(appsapplyv1.StatefulSetOrdinals().WithStart(1)).
			WithSelector(metaapplyv1.LabelSelector().
				WithMatchLabels(selectorMap))).
		WithLabels(statefulSetLabels).
		WithAnnotations(lws.Annotations)

	pvcApplyConfiguration := controllerutils.GetPVCApplyConfiguration(currentLws)
	if len(pvcApplyConfiguration) > 0 {
		statefulSetConfig.Spec.WithVolumeClaimTemplates(pvcApplyConfiguration...)
	}

	if currentLws.Spec.LeaderWorkerTemplate.PersistentVolumeClaimRetentionPolicy != nil {
		pvcRetentionPolicy := &appsapplyv1.StatefulSetPersistentVolumeClaimRetentionPolicyApplyConfiguration{
			WhenDeleted: &currentLws.Spec.LeaderWorkerTemplate.PersistentVolumeClaimRetentionPolicy.WhenDeleted,
			WhenScaled:  &currentLws.Spec.LeaderWorkerTemplate.PersistentVolumeClaimRetentionPolicy.WhenScaled,
		}
		statefulSetConfig.Spec.WithPersistentVolumeClaimRetentionPolicy(pvcRetentionPolicy)
	}
	return statefulSetConfig, nil
}

func (r *PodReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return builder.TypedControllerManagedBy[podReconcileRequest](mgr).
		Named("pod").
		Watches(&corev1.Pod{}, podEventHandler()).
		Watches(&appsv1.StatefulSet{}, statefulSetEventHandler()).
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
		})).
		Complete(reconcile.TypedFunc[podReconcileRequest](r.reconcilePod))
}

func podEventHandler() handler.TypedEventHandler[client.Object, podReconcileRequest] {
	enqueue := func(object client.Object, deleted bool, queue workqueue.TypedRateLimitingInterface[podReconcileRequest]) {
		pod, ok := object.(*corev1.Pod)
		if !ok || pod == nil {
			return
		}
		queue.Add(podReconcileRequestForPod(pod, deleted))
	}
	return handler.TypedFuncs[client.Object, podReconcileRequest]{
		CreateFunc: func(_ context.Context, event event.TypedCreateEvent[client.Object], queue workqueue.TypedRateLimitingInterface[podReconcileRequest]) {
			enqueue(event.Object, false, queue)
		},
		UpdateFunc: func(_ context.Context, event event.TypedUpdateEvent[client.Object], queue workqueue.TypedRateLimitingInterface[podReconcileRequest]) {
			enqueue(event.ObjectNew, false, queue)
		},
		DeleteFunc: func(_ context.Context, event event.TypedDeleteEvent[client.Object], queue workqueue.TypedRateLimitingInterface[podReconcileRequest]) {
			enqueue(event.Object, true, queue)
		},
		GenericFunc: func(_ context.Context, event event.TypedGenericEvent[client.Object], queue workqueue.TypedRateLimitingInterface[podReconcileRequest]) {
			enqueue(event.Object, false, queue)
		},
	}
}

func statefulSetEventHandler() handler.TypedEventHandler[client.Object, podReconcileRequest] {
	return handler.TypedEnqueueRequestsFromMapFunc(func(_ context.Context, object client.Object) []podReconcileRequest {
		statefulSet, ok := object.(*appsv1.StatefulSet)
		if !ok || statefulSet == nil {
			return nil
		}
		owner := metav1.GetControllerOf(statefulSet)
		if owner == nil || owner.APIVersion != corev1.SchemeGroupVersion.String() || owner.Kind != "Pod" {
			return nil
		}
		return []podReconcileRequest{{
			NamespacedName: types.NamespacedName{Name: owner.Name, Namespace: statefulSet.Namespace},
			UID:            owner.UID,
		}}
	})
}

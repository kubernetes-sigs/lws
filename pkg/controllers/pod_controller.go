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
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	appsapplyv1 "k8s.io/client-go/applyconfigurations/apps/v1"
	coreapplyv1 "k8s.io/client-go/applyconfigurations/core/v1"
	metaapplyv1 "k8s.io/client-go/applyconfigurations/meta/v1"
	"k8s.io/client-go/tools/events"
	"k8s.io/client-go/util/retry"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
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
		// A leader delete event arrives after its finalizers have been released.
		// Replaying recovery from that snapshot could reset the budget or release
		// worker finalizers for a replacement group with the same name/revision.
		// Deleted workers still need restart-policy handling below.
		if podutils.LeaderPod(*req.DeletedPod) {
			return ctrl.Result{}, nil
		}
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
		if apierrors.IsNotFound(err) {
			// The LWS may disappear before its terminating Pods. Release our
			// finalizers so garbage collection can finish without the LWS.
			if podutils.LeaderPod(pod) {
				return ctrl.Result{}, r.removeGroupRestartBudgetFinalizersForGroup(ctx, &pod)
			}
			return ctrl.Result{}, r.removePodGroupRestartBudgetFinalizer(ctx, &pod)
		}
		return ctrl.Result{}, err
	}
	// LWS deletion is workload teardown, not a restart-policy event or recovery
	// of an exhausted group. Release budget finalizers before handleRestartPolicy,
	// without clearing restart accounting or creating a replacement group.
	if leaderWorkerSet.DeletionTimestamp != nil {
		if podutils.LeaderPod(pod) {
			return ctrl.Result{}, r.removeGroupRestartBudgetFinalizersForGroup(ctx, &pod)
		}
		return ctrl.Result{}, r.removePodGroupRestartBudgetFinalizer(ctx, &pod)
	}
	if podutils.LeaderPod(pod) && pod.Annotations[leaderworkerset.GroupRestartBudgetExhaustedAnnotationKey] == "true" {
		teardown, err := r.groupLifecycleTeardownRequested(ctx, &leaderWorkerSet, &pod)
		if err != nil {
			return ctrl.Result{}, err
		}
		if teardown {
			return ctrl.Result{}, r.removeGroupRestartBudgetFinalizersForGroup(ctx, &pod)
		}
		if pod.DeletionTimestamp != nil && pod.Annotations[leaderworkerset.GroupRestartBudgetRecoverAnnotationKey] == "true" {
			if err := r.clearGroupRestartCount(ctx, &leaderWorkerSet, &pod); err != nil {
				return ctrl.Result{}, err
			}
			return ctrl.Result{}, r.removeGroupRestartBudgetFinalizersForGroup(ctx, &pod)
		}
		if pod.DeletionTimestamp != nil {
			return ctrl.Result{}, nil
		}
		_, err = r.terminateExhaustedGroup(ctx, &leaderWorkerSet, &pod)
		return ctrl.Result{}, err
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
		// name in ordinal mode, a group key derived name in hash mode. A stale or
		// terminating service short-circuits the rest of the reconcile, so no
		// group resources are created until the network identity is usable.
		if err := controllerutils.CreateHeadlessServiceIfNotExists(ctx, r.Client, r.Scheme, &leaderWorkerSet, pod.Spec.Subdomain, map[string]string{leaderworkerset.SetNameLabelKey: leaderWorkerSet.Name, leaderworkerset.GroupIndexLabelKey: pod.Labels[leaderworkerset.GroupIndexLabelKey]}, &pod); err != nil {
			return ctrl.Result{}, err
		}
	}

	// The leaf PodGroup has to exist before any member pod can be scheduled.
	// With groupIdentity Hash the group key is only known once admission has
	// stamped this leader pod, so its PodGroups are created here, ahead of the
	// gate that keeps the leader unschedulable.
	if r.SchedulerProvider != nil {
		err = r.SchedulerProvider.CreatePodGroupIfNotExists(ctx, &leaderWorkerSet, &pod)
		if err != nil {
			if errors.Is(err, schedulerprovider.ErrUnexpectedPodGroupOwner) {
				r.Record.Eventf(&pod, &leaderWorkerSet, corev1.EventTypeWarning, UnexpectedPodGroupOwner, Create, "%s", err.Error())
			}
			// Return transient errors too, so controller-runtime retries with backoff
			// if garbage collection is delayed or blocked by a finalizer.
			return ctrl.Result{}, err
		}
	}

	// While the leader is gated, only the group's scheduling prerequisites exist:
	// the per-replica service and the leaf PodGroup are in place before the gate
	// is lifted, but the worker statefulset waits for the group to be admitted.
	// The requeue is a fallback for the leader deletion watch in SetupWithManager.
	if podutils.HasSchedulingGate(&pod, leaderworkerset.GroupReplacementSchedulingGate) {
		admitted, err := r.reconcileGroupReplacementGate(ctx, &pod, &leaderWorkerSet)
		if err != nil {
			return ctrl.Result{}, err
		}
		if !admitted {
			return ctrl.Result{RequeueAfter: groupReplacementRequeueDelay}, nil
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
	// The caller's objects may come from a lagging cache snapshot. Re-read the
	// leader and the LWS so that budget enforcement below uses the persisted
	// restart count and the leader's current annotations; a stale count would
	// let the group restart past MaxGroupRestarts.
	freshLeader := corev1.Pod{}
	if err := r.Get(ctx, client.ObjectKeyFromObject(&leader), &freshLeader); err != nil {
		// The leader is already gone, so the recreate it belonged to is done.
		return true, client.IgnoreNotFound(err)
	}
	if freshLeader.UID != leader.UID {
		// A same-name replacement leader already exists; do not act on it on
		// behalf of the previous group.
		return false, nil
	}
	leader = freshLeader
	var freshLWS leaderworkerset.LeaderWorkerSet
	if err := r.Get(ctx, client.ObjectKeyFromObject(&leaderWorkerSet), &freshLWS); err != nil {
		if !apierrors.IsNotFound(err) {
			return false, err
		}
		// The LWS disappeared after the caller read it. Release any retained
		// Pods and stop here; the stale LWS must not trigger group recreation.
		if podutils.LeaderPod(pod) {
			return true, r.removeGroupRestartBudgetFinalizersForGroup(ctx, &leader)
		}
		return true, r.removePodGroupRestartBudgetFinalizer(ctx, &pod)
	}
	leaderWorkerSet = freshLWS
	// if the leader pod is being deleted, we don't need to send deletion requests
	if leader.DeletionTimestamp != nil {
		return true, nil
	}
	// An exhausted group is terminated once and then held by Pod finalizers until
	// explicit recovery or workload teardown.
	if leader.Annotations[leaderworkerset.GroupRestartBudgetExhaustedAnnotationKey] == "true" {
		return r.terminateExhaustedGroup(ctx, &leaderWorkerSet, &leader)
	}
	// If a restart budget is configured, enforce it: any recreate-triggering
	// failure contributes to the same counter. nil keeps the unbounded legacy
	// behavior.
	if leaderWorkerSet.Spec.LeaderWorkerTemplate.MaxGroupRestarts != nil {
		count, err := r.getPersistedGroupRestartCount(&leaderWorkerSet, &leader)
		if err != nil {
			return false, fmt.Errorf("reading persisted group restart count for %s: %w", leader.Name, err)
		}
		limit := *leaderWorkerSet.Spec.LeaderWorkerTemplate.MaxGroupRestarts
		if count >= limit {
			return r.terminateExhaustedGroup(ctx, &leaderWorkerSet, &leader)
		}
		if err := r.persistGroupRestartCount(ctx, &leaderWorkerSet, &leader, count+1); err != nil {
			return false, fmt.Errorf("updating group restart count for %s: %w", leader.Name, err)
		}
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

func parseGroupRestartCounts(raw string) (map[string]int32, error) {
	if raw == "" {
		return map[string]int32{}, nil
	}
	counts := map[string]int32{}
	if err := json.Unmarshal([]byte(raw), &counts); err != nil {
		return nil, err
	}
	if counts == nil {
		return nil, fmt.Errorf("invalid group restart counts: expected a JSON object")
	}
	for groupIndex, count := range counts {
		if count < 0 {
			return nil, fmt.Errorf("invalid group restart count for group %q: must be non-negative", groupIndex)
		}
	}
	return counts, nil
}

func groupRestartCountKey(leader *corev1.Pod) string {
	return fmt.Sprintf("%s/%s", revisionutils.GetRevisionKey(leader), leader.Labels[leaderworkerset.GroupIndexLabelKey])
}

func (r *PodReconciler) getPersistedGroupRestartCount(lws *leaderworkerset.LeaderWorkerSet, leader *corev1.Pod) (int32, error) {
	if lws.Annotations == nil {
		return 0, nil
	}
	counts, err := parseGroupRestartCounts(lws.Annotations[leaderworkerset.GroupRestartCountsAnnotationKey])
	if err != nil {
		return 0, err
	}
	return counts[groupRestartCountKey(leader)], nil
}

func (r *PodReconciler) persistGroupRestartCount(ctx context.Context, lws *leaderworkerset.LeaderWorkerSet, leader *corev1.Pod, next int32) error {
	key := client.ObjectKeyFromObject(lws)
	countKey := groupRestartCountKey(leader)
	return mutateGroupRestartCounts(ctx, r.Client, key, func(_ *leaderworkerset.LeaderWorkerSet, counts map[string]int32) (bool, error) {
		// A retry may observe a newer count written by another reconcile. Never
		// overwrite it with a value computed from a stale LWS object.
		if counts[countKey] >= next {
			return false, nil
		}
		counts[countKey] = next
		return true, nil
	})
}

func (r *PodReconciler) markGroupRestartBudgetExhausted(ctx context.Context, leader *corev1.Pod) error {
	exhausted := leader.Annotations[leaderworkerset.GroupRestartBudgetExhaustedAnnotationKey] == "true"
	if exhausted && controllerutil.ContainsFinalizer(leader, leaderworkerset.GroupRestartBudgetCleanupFinalizer) {
		return nil
	}
	patch := client.MergeFrom(leader.DeepCopy())
	if leader.Annotations == nil {
		leader.Annotations = map[string]string{}
	}
	// Recovery must be an explicit action taken after exhaustion. Drop any stale
	// or pre-set signal when the group first enters the exhausted state.
	if !exhausted {
		delete(leader.Annotations, leaderworkerset.GroupRestartBudgetRecoverAnnotationKey)
	}
	leader.Annotations[leaderworkerset.GroupRestartBudgetExhaustedAnnotationKey] = "true"
	controllerutil.AddFinalizer(leader, leaderworkerset.GroupRestartBudgetCleanupFinalizer)
	return r.Patch(ctx, leader, patch)
}

func (r *PodReconciler) terminateExhaustedGroup(ctx context.Context, lws *leaderworkerset.LeaderWorkerSet, leader *corev1.Pod) (bool, error) {
	if err := r.markGroupRestartBudgetExhausted(ctx, leader); err != nil {
		return false, err
	}
	if err := r.addGroupRestartBudgetFinalizers(ctx, leader); err != nil {
		return false, err
	}
	if leader.DeletionTimestamp != nil {
		return true, nil
	}
	deletionOpt := metav1.DeletePropagationForeground
	if err := r.Delete(ctx, leader, &client.DeleteOptions{PropagationPolicy: &deletionOpt}); err != nil {
		return false, client.IgnoreNotFound(err)
	}
	r.Record.Eventf(lws, leader, corev1.EventTypeWarning, "ReplicaRestartBudgetExceeded", Delete,
		fmt.Sprintf("Restart budget exhausted; terminated group %s and stopped automatic recovery", leader.Labels[leaderworkerset.GroupIndexLabelKey]))
	return true, nil
}

func (r *PodReconciler) groupPods(ctx context.Context, leader *corev1.Pod) ([]corev1.Pod, error) {
	selector := client.MatchingLabels{
		leaderworkerset.SetNameLabelKey:    leader.Labels[leaderworkerset.SetNameLabelKey],
		leaderworkerset.GroupIndexLabelKey: leader.Labels[leaderworkerset.GroupIndexLabelKey],
		leaderworkerset.RevisionKey:        revisionutils.GetRevisionKey(leader),
	}
	var pods corev1.PodList
	if err := r.List(ctx, &pods, client.InNamespace(leader.Namespace), selector); err != nil {
		return nil, err
	}
	return pods.Items, nil
}

func (r *PodReconciler) addGroupRestartBudgetFinalizers(ctx context.Context, leader *corev1.Pod) error {
	pods, err := r.groupPods(ctx, leader)
	if err != nil {
		return err
	}
	for i := range pods {
		pod := &pods[i]
		if pod.DeletionTimestamp != nil || controllerutil.ContainsFinalizer(pod, leaderworkerset.GroupRestartBudgetCleanupFinalizer) {
			continue
		}
		if !podutils.LeaderPod(*pod) {
			belongs, err := r.workerPodBelongsToLeader(ctx, *pod, *leader)
			if err != nil {
				return err
			}
			if !belongs {
				continue
			}
		}
		patch := client.MergeFrom(pod.DeepCopy())
		controllerutil.AddFinalizer(pod, leaderworkerset.GroupRestartBudgetCleanupFinalizer)
		if err := r.Patch(ctx, pod, patch); err != nil {
			return err
		}
	}
	return nil
}

// removeGroupRestartBudgetFinalizersForGroup releases the retained Pods of an
// exhausted group during explicit recovery or workload teardown (LWS deletion,
// scale-down, or rollout). It removes worker finalizers before the leader's so
// foreground deletion can finish before a replacement group is created. This
// function does not clear the restart count; explicit recovery does that first.
func (r *PodReconciler) removeGroupRestartBudgetFinalizersForGroup(ctx context.Context, leader *corev1.Pod) error {
	pods, err := r.groupPods(ctx, leader)
	if err != nil {
		return err
	}
	// Remove worker finalizers first so foreground garbage collection can finish
	// before the leader StatefulSet creates a replacement with the same name.
	for i := range pods {
		pod := &pods[i]
		if podutils.LeaderPod(*pod) {
			continue
		}
		if err := r.removePodGroupRestartBudgetFinalizer(ctx, pod); err != nil {
			return err
		}
	}
	return r.removePodGroupRestartBudgetFinalizer(ctx, leader)
}

// removePodGroupRestartBudgetFinalizer releases a single Pod, if held by the
// restart-budget finalizer. Worker events use this during LWS teardown.
func (r *PodReconciler) removePodGroupRestartBudgetFinalizer(ctx context.Context, pod *corev1.Pod) error {
	if !controllerutil.ContainsFinalizer(pod, leaderworkerset.GroupRestartBudgetCleanupFinalizer) {
		return nil
	}
	patch := client.MergeFrom(pod.DeepCopy())
	controllerutil.RemoveFinalizer(pod, leaderworkerset.GroupRestartBudgetCleanupFinalizer)
	delete(pod.Annotations, leaderworkerset.GroupRestartBudgetExhaustedAnnotationKey)
	delete(pod.Annotations, leaderworkerset.GroupRestartBudgetRecoverAnnotationKey)
	return r.Patch(ctx, pod, patch)
}

func (r *PodReconciler) clearGroupRestartCount(ctx context.Context, lws *leaderworkerset.LeaderWorkerSet, leader *corev1.Pod) error {
	countKey := groupRestartCountKey(leader)
	return mutateGroupRestartCounts(ctx, r.Client, client.ObjectKeyFromObject(lws), func(_ *leaderworkerset.LeaderWorkerSet, counts map[string]int32) (bool, error) {
		if _, found := counts[countKey]; !found {
			return false, nil
		}
		delete(counts, countKey)
		return true, nil
	})
}

func (r *PodReconciler) groupLifecycleTeardownRequested(ctx context.Context, lws *leaderworkerset.LeaderWorkerSet, leader *corev1.Pod) (bool, error) {
	if lws.Spec.GroupIdentity == leaderworkerset.GroupIdentityHash {
		return r.hashGroupLifecycleTeardownRequested(ctx, lws, leader)
	}
	groupIndex, err := strconv.ParseInt(leader.Labels[leaderworkerset.GroupIndexLabelKey], 10, 32)
	if err != nil {
		return false, fmt.Errorf("parsing group index for pod %s: %w", leader.Name, err)
	}

	var leaderSts appsv1.StatefulSet
	if err := r.Get(ctx, types.NamespacedName{Name: lws.Name, Namespace: lws.Namespace}, &leaderSts); err != nil {
		return false, client.IgnoreNotFound(err)
	}
	// The StatefulSet replica count includes active MaxSurge ordinals. Only an
	// ordinal outside that range is actually being removed by scale-down.
	if groupIndex >= int64(*leaderSts.Spec.Replicas) {
		return true, nil
	}
	desiredRevision := revisionutils.GetRevisionKey(&leaderSts)
	if desiredRevision == "" || desiredRevision == revisionutils.GetRevisionKey(leader) {
		return false, nil
	}
	partition := int32(0)
	if leaderSts.Spec.UpdateStrategy.RollingUpdate != nil && leaderSts.Spec.UpdateStrategy.RollingUpdate.Partition != nil {
		partition = *leaderSts.Spec.UpdateStrategy.RollingUpdate.Partition
	}
	return groupIndex >= int64(partition), nil
}

func (r *PodReconciler) hashGroupLifecycleTeardownRequested(ctx context.Context, lws *leaderworkerset.LeaderWorkerSet, leader *corev1.Pod) (bool, error) {
	var deploy appsv1.Deployment
	if err := r.Get(ctx, types.NamespacedName{Name: lws.Name, Namespace: lws.Namespace}, &deploy); err != nil {
		if !apierrors.IsNotFound(err) {
			return false, err
		}
	} else {
		desiredRevision := revisionutils.GetRevisionKey(&deploy)
		if desiredRevision != "" && desiredRevision != revisionutils.GetRevisionKey(leader) {
			return true, nil
		}
	}

	var leaderPods corev1.PodList
	if err := r.List(ctx, &leaderPods, client.InNamespace(lws.Namespace), client.MatchingLabels{
		leaderworkerset.SetNameLabelKey:     lws.Name,
		leaderworkerset.WorkerIndexLabelKey: "0",
	}); err != nil {
		return false, err
	}

	leaderRevision := revisionutils.GetRevisionKey(leader)
	admittedActive := 0
	var exhausted []corev1.Pod
	for i := range leaderPods.Items {
		p := leaderPods.Items[i]
		if revisionutils.GetRevisionKey(&p) != leaderRevision {
			continue
		}
		if p.Annotations[leaderworkerset.GroupRestartBudgetExhaustedAnnotationKey] == "true" {
			exhausted = append(exhausted, p)
			continue
		}
		if p.DeletionTimestamp == nil && !podutils.HasSchedulingGate(&p, leaderworkerset.GroupReplacementSchedulingGate) {
			admittedActive++
		}
	}
	sort.Slice(exhausted, func(i, j int) bool {
		if !exhausted[i].CreationTimestamp.Equal(&exhausted[j].CreationTimestamp) {
			return exhausted[i].CreationTimestamp.Before(&exhausted[j].CreationTimestamp)
		}
		return exhausted[i].Name < exhausted[j].Name
	})

	allowedExhausted := max(0, int(*lws.Spec.Replicas)-admittedActive)
	for i := range exhausted {
		if exhausted[i].Name == leader.Name {
			if i >= allowedExhausted {
				if err := r.clearGroupRestartCount(ctx, lws, leader); err != nil {
					return false, err
				}
				return true, nil
			}
			return false, nil
		}
	}
	return int(*lws.Spec.Replicas) <= admittedActive, nil
}

// mutateGroupRestartCounts performs a conflict-safe read-modify-write of the
// LWS restart-count annotation. The callback must only mutate counts when it
// returns changed=true. Fetching the LWS on every retry prevents concurrent
// group updates from losing each other's keys.
func mutateGroupRestartCounts(ctx context.Context, c client.Client, key client.ObjectKey, mutate func(*leaderworkerset.LeaderWorkerSet, map[string]int32) (changed bool, err error)) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		latest := &leaderworkerset.LeaderWorkerSet{}
		if err := c.Get(ctx, key, latest); err != nil {
			return err
		}

		var raw string
		if latest.Annotations != nil {
			raw = latest.Annotations[leaderworkerset.GroupRestartCountsAnnotationKey]
		}
		counts, err := parseGroupRestartCounts(raw)
		if err != nil {
			return err
		}
		changed, err := mutate(latest, counts)
		if err != nil || !changed {
			return err
		}

		if latest.Annotations == nil {
			latest.Annotations = map[string]string{}
		}
		if len(counts) == 0 {
			delete(latest.Annotations, leaderworkerset.GroupRestartCountsAnnotationKey)
		} else {
			raw, err := json.Marshal(counts)
			if err != nil {
				return err
			}
			latest.Annotations[leaderworkerset.GroupRestartCountsAnnotationKey] = string(raw)
		}
		return c.Update(ctx, latest)
	})
}

const groupReplacementRequeueDelay = 10 * time.Second

// reconcileGroupReplacementGate decides whether a gated leader pod may start
// scheduling and lifts the gate if so. Under PostTermination every group that
// is still tearing down holds back one gated leader, oldest first, so a
// replacement group only competes for capacity once a previous group has been
// fully removed. Under Immediate, only budget-exhausted retained groups hold
// back their gated replacement leader. Returns true once the gate is gone.
func (r *PodReconciler) reconcileGroupReplacementGate(ctx context.Context, pod *corev1.Pod, lws *leaderworkerset.LeaderWorkerSet) (bool, error) {
	log := ctrl.LoggerFrom(ctx)
	var pods corev1.PodList
	if err := r.List(ctx, &pods, client.InNamespace(pod.Namespace), client.MatchingLabels{
		leaderworkerset.SetNameLabelKey: lws.Name,
	}); err != nil {
		return false, err
	}
	var desiredRevision string
	if lws.Spec.GroupIdentity == leaderworkerset.GroupIdentityHash {
		var deploy appsv1.Deployment
		if err := r.Get(ctx, types.NamespacedName{Name: lws.Name, Namespace: lws.Namespace}, &deploy); err != nil {
			if !apierrors.IsNotFound(err) && !runtime.IsNotRegisteredError(err) {
				return false, err
			}
		} else {
			desiredRevision = revisionutils.GetRevisionKey(&deploy)
		}
		podRevision := revisionutils.GetRevisionKey(pod)
		if desiredRevision != "" && podRevision != "" && podRevision != desiredRevision {
			log.V(2).Info("Deferring gated leader from outdated revision", "podRevision", podRevision, "desiredRevision", desiredRevision)
			return false, nil
		}

		admittedOnRevision := 0
		exhaustedOnRevision := 0
		var gatedOnRevision []corev1.Pod
		for _, p := range pods.Items {
			if !podutils.LeaderPod(p) || revisionutils.GetRevisionKey(&p) != podRevision {
				continue
			}
			if p.Annotations[leaderworkerset.GroupRestartBudgetExhaustedAnnotationKey] == "true" {
				exhaustedOnRevision++
				continue
			}
			if p.DeletionTimestamp != nil {
				continue
			}
			if podutils.HasSchedulingGate(&p, leaderworkerset.GroupReplacementSchedulingGate) {
				gatedOnRevision = append(gatedOnRevision, p)
			} else {
				admittedOnRevision++
			}
		}
		sort.Slice(gatedOnRevision, func(i, j int) bool {
			if !gatedOnRevision[i].CreationTimestamp.Equal(&gatedOnRevision[j].CreationTimestamp) {
				return gatedOnRevision[i].CreationTimestamp.Before(&gatedOnRevision[j].CreationTimestamp)
			}
			return gatedOnRevision[i].Name < gatedOnRevision[j].Name
		})
		revisionRank := -1
		for i := range gatedOnRevision {
			if gatedOnRevision[i].Name == pod.Name {
				revisionRank = i
				break
			}
		}
		lwsReplicas := 1
		if lws.Spec.Replicas != nil {
			lwsReplicas = int(*lws.Spec.Replicas)
		}
		if revisionRank == -1 || admittedOnRevision+exhaustedOnRevision+revisionRank >= lwsReplicas {
			log.V(2).Info("Deferring gated leader exceeding desired replica slots", "admittedOnRevision", admittedOnRevision, "exhaustedOnRevision", exhaustedOnRevision, "revisionRank", revisionRank, "replicas", lwsReplicas)
			return false, nil
		}
	}

	blockingGroups := countTearingDownGroups(pods.Items)
	if lws.Spec.GroupReplacementPolicy == leaderworkerset.GroupReplacementImmediate {
		blockingGroups = countExhaustedGroups(pods.Items)
	}
	if blockingGroups > 0 {
		var gated []corev1.Pod
		for _, p := range pods.Items {
			if podutils.LeaderPod(p) && p.DeletionTimestamp == nil && podutils.HasSchedulingGate(&p, leaderworkerset.GroupReplacementSchedulingGate) {
				gated = append(gated, p)
			}
		}
		sort.Slice(gated, func(i, j int) bool {
			if desiredRevision != "" {
				iDesired := revisionutils.GetRevisionKey(&gated[i]) == desiredRevision
				jDesired := revisionutils.GetRevisionKey(&gated[j]) == desiredRevision
				if iDesired != jDesired {
					return iDesired
				}
			}
			if !gated[i].CreationTimestamp.Equal(&gated[j].CreationTimestamp) {
				return gated[i].CreationTimestamp.Before(&gated[j].CreationTimestamp)
			}
			return gated[i].Name < gated[j].Name
		})
		rank := -1
		for i := range gated {
			if gated[i].Name == pod.Name {
				rank = i
				break
			}
		}
		if rank == -1 || rank >= len(gated)-blockingGroups {
			log.V(2).Info("Deferring group replacement until terminating groups are removed", "terminatingLeaders", blockingGroups, "gatedLeaders", len(gated))
			r.Record.Eventf(lws, pod, corev1.EventTypeNormal, GroupReplacementDeferred, Update, fmt.Sprintf("Leader pod %s waits for %d terminating group(s) to be removed before scheduling", pod.Name, blockingGroups))
			return false, nil
		}
	}
	if lws.Spec.GroupIdentity == leaderworkerset.GroupIdentityHash && lws.Spec.LeaderWorkerTemplate.MaxGroupRestarts != nil {
		if err := r.claimGroupRestartCountForHashLeader(ctx, lws, pod); err != nil {
			return false, err
		}
	}
	newPod := pod.DeepCopy()
	newPod.Spec.SchedulingGates = nil
	for _, gate := range pod.Spec.SchedulingGates {
		if gate.Name != leaderworkerset.GroupReplacementSchedulingGate {
			newPod.Spec.SchedulingGates = append(newPod.Spec.SchedulingGates, gate)
		}
	}
	if err := r.Patch(ctx, newPod, client.MergeFrom(pod)); err != nil {
		return false, err
	}
	*pod = *newPod
	r.Record.Eventf(lws, pod, corev1.EventTypeNormal, GroupReplacementAdmitted, Update, fmt.Sprintf("Leader pod %s admitted for scheduling", pod.Name))
	return true, nil
}

func (r *PodReconciler) claimGroupRestartCountForHashLeader(ctx context.Context, lws *leaderworkerset.LeaderWorkerSet, leader *corev1.Pod) error {
	revisionKey := revisionutils.GetRevisionKey(leader)
	targetKey := groupRestartCountKey(leader)
	if revisionKey == "" || leader.Labels[leaderworkerset.GroupIndexLabelKey] == "" {
		return nil
	}
	return mutateGroupRestartCounts(ctx, r.Client, client.ObjectKeyFromObject(lws), func(_ *leaderworkerset.LeaderWorkerSet, counts map[string]int32) (bool, error) {
		if _, exists := counts[targetKey]; exists {
			return false, nil
		}
		var leaderPods corev1.PodList
		if err := r.List(ctx, &leaderPods, client.InNamespace(leader.Namespace), client.MatchingLabels{
			leaderworkerset.SetNameLabelKey:     lws.Name,
			leaderworkerset.WorkerIndexLabelKey: "0",
		}); err != nil {
			return false, err
		}
		ownedKeys := make(map[string]struct{}, len(leaderPods.Items))
		for i := range leaderPods.Items {
			p := &leaderPods.Items[i]
			// A counter key remains owned while its leader is alive or while its
			// group is retained in the budget-exhausted state.
			if p.DeletionTimestamp == nil || p.Annotations[leaderworkerset.GroupRestartBudgetExhaustedAnnotationKey] == "true" {
				ownedKeys[groupRestartCountKey(p)] = struct{}{}
			}
		}
		prefix := revisionKey + "/"
		bestKey := ""
		var bestCount int32
		for k, count := range counts {
			if !strings.HasPrefix(k, prefix) {
				continue
			}
			if _, owned := ownedKeys[k]; owned {
				continue
			}
			if bestKey == "" || count > bestCount || (count == bestCount && k < bestKey) {
				bestKey = k
				bestCount = count
			}
		}
		if bestKey == "" {
			return false, nil
		}
		counts[targetKey] = bestCount
		delete(counts, bestKey)
		return true, nil
	})
}

func countExhaustedGroups(pods []corev1.Pod) int {
	count := 0
	for i := range pods {
		p := &pods[i]
		if podutils.LeaderPod(*p) && p.Annotations[leaderworkerset.GroupRestartBudgetExhaustedAnnotationKey] == "true" {
			count++
		}
	}
	return count
}

// countTearingDownGroups returns the number of groups in pods whose leader is
// terminating or gone while at least one pod of the group still exists. Pods
// are grouped by the group index label, which leaders and workers share.
func countTearingDownGroups(pods []corev1.Pod) int {
	type groupState struct {
		leaderAlive bool
	}
	groups := map[string]*groupState{}
	for i := range pods {
		p := &pods[i]
		key := p.Labels[leaderworkerset.GroupIndexLabelKey]
		if key == "" {
			// Fall back to the leader name, which is also the worker statefulset name.
			key = p.Name
			if owner := metav1.GetControllerOf(p); owner != nil && !podutils.LeaderPod(*p) {
				key = owner.Name
			}
		}
		state, ok := groups[key]
		if !ok {
			state = &groupState{}
			groups[key] = state
		}
		if podutils.LeaderPod(*p) && p.DeletionTimestamp == nil && p.Annotations[leaderworkerset.GroupRestartBudgetExhaustedAnnotationKey] != "true" {
			state.leaderAlive = true
		}
	}
	count := 0
	for _, state := range groups {
		if !state.leaderAlive {
			count++
		}
	}
	return count
}

// enqueueGatedLeaders queues the gated leader pods of the LeaderWorkerSet that
// obj belongs to. It runs when any pod of the set is deleted, since the last
// pod of an old group leaving is what frees a slot, and when a gated leader is
// created, since that can change which gated leader is oldest.
func (r *PodReconciler) enqueueGatedLeaders(ctx context.Context, obj client.Object, q workqueue.TypedRateLimitingInterface[podReconcileRequest]) {
	changed, ok := obj.(*corev1.Pod)
	if !ok {
		return
	}
	var leaders corev1.PodList
	if err := r.List(ctx, &leaders, client.InNamespace(changed.Namespace), client.MatchingLabels{
		leaderworkerset.SetNameLabelKey:     changed.Labels[leaderworkerset.SetNameLabelKey],
		leaderworkerset.WorkerIndexLabelKey: "0",
	}); err != nil {
		ctrl.LoggerFrom(ctx).Error(err, "listing leader pods for group replacement")
		return
	}
	for i := range leaders.Items {
		if podutils.HasSchedulingGate(&leaders.Items[i], leaderworkerset.GroupReplacementSchedulingGate) {
			q.Add(podReconcileRequestForPod(&leaders.Items[i], false))
		}
	}
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
		return "", fmt.Errorf("node does not have topology label: %s", topologyKey)
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
		podAnnotations[leaderworkerset.SubGroupSizeAnnotationKey] = strconv.Itoa(int(*currentLws.Spec.LeaderWorkerTemplate.SubGroupPolicy.SubGroupSize))
		if lws.Annotations[leaderworkerset.SubGroupExclusiveKeyAnnotationKey] != "" {
			podAnnotations[leaderworkerset.SubGroupExclusiveKeyAnnotationKey] = lws.Annotations[leaderworkerset.SubGroupExclusiveKeyAnnotationKey]
		}
	}
	acceleratorutils.AddTPUAnnotations(leaderPod, podAnnotations)
	if currentLws.Spec.Scheduling != nil {
		podAnnotations[schedulerprovider.WorkloadSchedulingAnnotationKey] = schedulerprovider.WorkloadSchedulingValue(currentLws)
		podAnnotations[schedulerprovider.WorkloadNameAnnotationKey] = schedulerprovider.KubernetesWorkloadName(&lws)
	}
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
		// A gated leader is admitted when the last pod of an old group in the
		// same LeaderWorkerSet disappears or a new gated leader is created.
		Watches(&corev1.Pod{}, handler.TypedFuncs[client.Object, podReconcileRequest]{
			DeleteFunc: func(ctx context.Context, e event.TypedDeleteEvent[client.Object], q workqueue.TypedRateLimitingInterface[podReconcileRequest]) {
				r.enqueueGatedLeaders(ctx, e.Object, q)
			},
			CreateFunc: func(ctx context.Context, e event.TypedCreateEvent[client.Object], q workqueue.TypedRateLimitingInterface[podReconcileRequest]) {
				if p, ok := e.Object.(*corev1.Pod); ok && podutils.HasSchedulingGate(p, leaderworkerset.GroupReplacementSchedulingGate) {
					r.enqueueGatedLeaders(ctx, e.Object, q)
				}
			},
		}).
		Watches(&appsv1.StatefulSet{}, r.statefulSetEventHandler()).
		Watches(&appsv1.Deployment{}, handler.TypedEnqueueRequestsFromMapFunc(r.budgetFinalizedPodRequests)).
		Watches(&leaderworkerset.LeaderWorkerSet{}, handler.TypedEnqueueRequestsFromMapFunc(r.budgetFinalizedPodRequests),
			builder.WithPredicates(predicate.Funcs{
				CreateFunc: func(event.CreateEvent) bool { return true },
				DeleteFunc: func(event.DeleteEvent) bool { return true },
				UpdateFunc: func(e event.UpdateEvent) bool {
					return e.ObjectOld.GetGeneration() != e.ObjectNew.GetGeneration() ||
						(e.ObjectOld.GetDeletionTimestamp() == nil) != (e.ObjectNew.GetDeletionTimestamp() == nil)
				},
				GenericFunc: func(event.GenericEvent) bool { return false },
			})).
		WithEventFilter(predicate.NewPredicateFuncs(func(object client.Object) bool {
			if pod, ok := object.(*corev1.Pod); ok {
				_, exist := pod.Labels[leaderworkerset.SetNameLabelKey]
				return exist
			}
			if statefulSet, ok := object.(*appsv1.StatefulSet); ok {
				_, exist := statefulSet.Labels[leaderworkerset.SetNameLabelKey]
				return exist
			}
			if deployment, ok := object.(*appsv1.Deployment); ok {
				_, exist := deployment.Labels[leaderworkerset.SetNameLabelKey]
				return exist
			}
			if _, ok := object.(*leaderworkerset.LeaderWorkerSet); ok {
				return true
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

func (r *PodReconciler) statefulSetEventHandler() handler.TypedEventHandler[client.Object, podReconcileRequest] {
	return handler.TypedEnqueueRequestsFromMapFunc(func(ctx context.Context, object client.Object) []podReconcileRequest {
		statefulSet, ok := object.(*appsv1.StatefulSet)
		if !ok || statefulSet == nil {
			return nil
		}
		owner := metav1.GetControllerOf(statefulSet)
		if owner == nil {
			return nil
		}
		if owner.APIVersion == leaderworkerset.GroupVersion.String() && owner.Kind == "LeaderWorkerSet" {
			return r.budgetFinalizedPodRequests(ctx, statefulSet)
		}
		if owner.APIVersion != corev1.SchemeGroupVersion.String() || owner.Kind != "Pod" {
			return nil
		}
		return []podReconcileRequest{{
			NamespacedName: types.NamespacedName{Name: owner.Name, Namespace: statefulSet.Namespace},
			UID:            owner.UID,
		}}
	})
}

func (r *PodReconciler) budgetFinalizedPodRequests(ctx context.Context, object client.Object) []podReconcileRequest {
	lwsName := object.GetLabels()[leaderworkerset.SetNameLabelKey]
	if lws, ok := object.(*leaderworkerset.LeaderWorkerSet); ok {
		lwsName = lws.Name
	}
	if lwsName == "" {
		return nil
	}
	var pods corev1.PodList
	if err := r.List(ctx, &pods, client.InNamespace(object.GetNamespace()), client.MatchingLabels{leaderworkerset.SetNameLabelKey: lwsName}); err != nil {
		ctrl.LoggerFrom(ctx).Error(err, "listing restart-budget finalized Pods", "leaderworkerset", lwsName)
		return nil
	}
	requests := make([]podReconcileRequest, 0)
	for i := range pods.Items {
		pod := &pods.Items[i]
		if !controllerutil.ContainsFinalizer(pod, leaderworkerset.GroupRestartBudgetCleanupFinalizer) {
			continue
		}
		requests = append(requests, podReconcileRequestForPod(pod, false))
	}
	return requests
}

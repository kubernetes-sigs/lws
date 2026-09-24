/*
Copyright 2026.

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
	"sort"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	appsapplyv1 "k8s.io/client-go/applyconfigurations/apps/v1"
	coreapplyv1 "k8s.io/client-go/applyconfigurations/core/v1"
	metaapplyv1 "k8s.io/client-go/applyconfigurations/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	leaderworkerset "sigs.k8s.io/lws/api/leaderworkerset/v1"
	podutils "sigs.k8s.io/lws/pkg/utils/pod"
	revisionutils "sigs.k8s.io/lws/pkg/utils/revision"
)

// reconcileHash reconciles a LeaderWorkerSet with GroupIdentity=Hash. Leaders are
// managed through a Deployment instead of a StatefulSet: the ReplicaSet picks
// scale-down victims (unscheduled and not-ready groups before healthy ones) and the
// Deployment paces rollouts, throttled by the group-ready readiness gate that the
// pod controller maintains on leader pods.
func (r *LeaderWorkerSetReconciler) reconcileHash(ctx context.Context, lws *leaderworkerset.LeaderWorkerSet) (ctrl.Result, error) {
	log := ctrl.LoggerFrom(ctx)

	deploy, err := r.getLeaderDeployment(ctx, lws)
	if err != nil {
		log.Error(err, "Fetching leader deployment")
		return ctrl.Result{}, err
	}
	if deploy != nil && deploy.DeletionTimestamp != nil {
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	revisionKey := ""
	if deploy != nil {
		revisionKey = revisionutils.GetRevisionKey(deploy)
	}
	revision, err := r.getOrCreateRevision(ctx, revisionKey, lws)
	if err != nil {
		log.Error(err, "Creating controller revision")
		return ctrl.Result{}, err
	}

	var updatedRevision *appsv1.ControllerRevision
	if deploy != nil {
		updatedRevision, err = r.getUpdatedRevision(ctx, lws, revision)
		if err != nil {
			log.Error(err, "Validating if LWS has been updated")
			return ctrl.Result{}, err
		}
	}
	if updatedRevision != nil {
		revision, err = revisionutils.CreateRevision(ctx, r.Client, updatedRevision)
		if err != nil {
			log.Error(err, "Creating revision for updated LWS")
			return ctrl.Result{}, err
		}
		r.Record.Eventf(lws, revision, corev1.EventTypeNormal, CreatingRevision, Create, fmt.Sprintf("Creating revision with key %s for updated LWS", revisionutils.GetRevisionKey(revision)))
	}

	if err := r.pruneHashGroupRestartCounts(ctx, lws, revisionutils.GetRevisionKey(revision)); err != nil {
		return ctrl.Result{}, err
	}

	// Scheduling prerequisites come before the leader Deployment so a leader pod
	// is never created before its Workload exists. Per-replica PodGroups cannot
	// be enumerated here because admission draws the group key of every leader
	// pod; the pod controller materializes them while the leader is still
	// scheduling gated.
	if err := r.reconcileWorkloadScheduling(ctx, lws, *lws.Spec.Replicas, revisionutils.GetRevisionKey(revision)); err != nil {
		log.Error(err, "Reconciling workload-aware scheduling prerequisites")
		return ctrl.Result{}, err
	}

	if err := r.SSAWithDeployment(ctx, lws, revisionutils.GetRevisionKey(revision)); err != nil {
		if deploy == nil {
			r.Record.Eventf(lws, nil, corev1.EventTypeWarning, FailedCreate, Create, fmt.Sprintf("Failed to create leader deployment %s: %v", lws.Name, err))
		} else {
			r.Record.Eventf(lws, nil, corev1.EventTypeWarning, FailedUpdate, Update, fmt.Sprintf("Failed to update leader deployment %s: %v", lws.Name, err))
		}
		return ctrl.Result{}, err
	}
	if deploy == nil {
		r.Record.Eventf(lws, revision, corev1.EventTypeNormal, GroupsProgressing, Create, fmt.Sprintf("Created leader deployment %s", lws.Name))
	}

	if err := r.reconcileHeadlessServices(ctx, lws); err != nil {
		log.Error(err, "Creating headless service.")
		r.Record.Eventf(lws, nil, corev1.EventTypeWarning, FailedCreate, Create, fmt.Sprintf("Failed to create headless service for error: %v", err))
		return ctrl.Result{}, err
	}

	updateDone, err := r.updateStatusHash(ctx, lws)
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
	log.V(2).Info("Leader Reconcile (hash identity) completed.")
	return ctrl.Result{}, nil
}

func (r *LeaderWorkerSetReconciler) pruneHashGroupRestartCounts(ctx context.Context, lws *leaderworkerset.LeaderWorkerSet, currentRevisionKey string) error {
	if lws.Annotations == nil || lws.Annotations[leaderworkerset.GroupRestartCountsAnnotationKey] == "" {
		return nil
	}
	return mutateGroupRestartCounts(ctx, r.Client, client.ObjectKeyFromObject(lws), func(latest *leaderworkerset.LeaderWorkerSet, counts map[string]int32) (bool, error) {
		var leaderPods corev1.PodList
		if err := r.List(ctx, &leaderPods, client.InNamespace(lws.Namespace), client.MatchingLabels{
			leaderworkerset.SetNameLabelKey:     lws.Name,
			leaderworkerset.WorkerIndexLabelKey: "0",
		}); err != nil {
			return false, err
		}
		ownedKeys := make(map[string]struct{}, len(leaderPods.Items))
		occupiedSlots := 0
		currentRevisionExhausted := 0
		currentRevisionGated := 0
		for i := range leaderPods.Items {
			p := &leaderPods.Items[i]
			exhausted := p.Annotations[leaderworkerset.GroupRestartBudgetExhaustedAnnotationKey] == "true"
			if p.DeletionTimestamp == nil || exhausted {
				ownedKeys[groupRestartCountKey(p)] = struct{}{}
			}
			if revisionutils.GetRevisionKey(p) != currentRevisionKey {
				continue
			}
			if exhausted {
				occupiedSlots++
				currentRevisionExhausted++
				continue
			}
			if p.DeletionTimestamp != nil {
				continue
			}
			if podutils.HasSchedulingGate(p, leaderworkerset.GroupReplacementSchedulingGate) {
				currentRevisionGated++
			} else {
				occupiedSlots++
			}
		}
		replicas := int(*latest.Spec.Replicas)
		maxSurge := 0
		if latest.Spec.RolloutStrategy.RollingUpdateConfiguration != nil {
			if surge, err := intstr.GetScaledValueFromIntOrPercent(&latest.Spec.RolloutStrategy.RollingUpdateConfiguration.MaxSurge, replicas, true); err == nil && surge > 0 {
				maxSurge = min(surge, replicas)
			}
		}
		pendingReplacements := max(0, currentRevisionGated-currentRevisionExhausted)
		activeSlotLimit := max(replicas, min(replicas+maxSurge, occupiedSlots+pendingReplacements))
		maxUnclaimed := max(0, activeSlotLimit-occupiedSlots)
		prefix := currentRevisionKey + "/"
		var unclaimedKeys []string
		for k := range counts {
			if !strings.HasPrefix(k, prefix) {
				continue
			}
			if _, owned := ownedKeys[k]; !owned {
				unclaimedKeys = append(unclaimedKeys, k)
			}
		}
		if len(unclaimedKeys) <= maxUnclaimed {
			return false, nil
		}
		sort.Slice(unclaimedKeys, func(i, j int) bool {
			if counts[unclaimedKeys[i]] != counts[unclaimedKeys[j]] {
				return counts[unclaimedKeys[i]] > counts[unclaimedKeys[j]]
			}
			return unclaimedKeys[i] < unclaimedKeys[j]
		})
		for _, k := range unclaimedKeys[maxUnclaimed:] {
			delete(counts, k)
		}
		return true, nil
	})
}

func (r *LeaderWorkerSetReconciler) getLeaderDeployment(ctx context.Context, lws *leaderworkerset.LeaderWorkerSet) (*appsv1.Deployment, error) {
	deploy := &appsv1.Deployment{}
	if err := r.Get(ctx, types.NamespacedName{Name: lws.Name, Namespace: lws.Namespace}, deploy); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	return deploy, nil
}

func (r *LeaderWorkerSetReconciler) SSAWithDeployment(ctx context.Context, lws *leaderworkerset.LeaderWorkerSet, revisionKey string) error {
	log := ctrl.LoggerFrom(ctx)

	deploymentApplyConfig, err := constructLeaderDeploymentApplyConfiguration(lws, revisionKey)
	if err != nil {
		log.Error(err, "Constructing Deployment apply configuration.")
		return err
	}
	ownerRef, err := controllerOwnerReference(lws, r.Scheme)
	if err != nil {
		log.Error(err, "Setting controller reference.")
		return err
	}
	deploymentApplyConfig.WithOwnerReferences(ownerRef)
	if err := r.serverSideApply(ctx, deploymentApplyConfig); err != nil {
		log.Error(err, "Using server side apply to update leader deployment")
		return err
	}
	return nil
}

// constructLeaderDeploymentApplyConfiguration is the hash-identity analog of
// constructLeaderStatefulSetApplyConfiguration. Rollout pacing (maxSurge and
// maxUnavailable) maps directly onto the Deployment strategy; there is no
// partition equivalent, ordering is delegated to the Deployment controller.
func constructLeaderDeploymentApplyConfiguration(lws *leaderworkerset.LeaderWorkerSet, revisionKey string) (*appsapplyv1.DeploymentApplyConfiguration, error) {
	podTemplateApplyConfiguration, err := buildLeaderPodTemplateApplyConfiguration(lws, revisionKey)
	if err != nil {
		return nil, err
	}
	podTemplateApplyConfiguration.WithAnnotations(map[string]string{
		leaderworkerset.GroupIdentityAnnotationKey: string(leaderworkerset.GroupIdentityHash),
	})

	// Deployments do not propagate a service name into pod subdomains the way
	// statefulsets do, so the template carries the shared headless service as the
	// default. Admission overrides it when subdomainPolicy is UniquePerReplica.
	if podTemplateApplyConfiguration.Spec == nil {
		podTemplateApplyConfiguration.Spec = coreapplyv1.PodSpec()
	}
	podTemplateApplyConfiguration.Spec.WithSubdomain(lws.Name)

	// The gate keeps a leader pod not-ready until its worker statefulset is ready,
	// so the Deployment's maxUnavailable budget counts whole groups.
	if *lws.Spec.LeaderWorkerTemplate.Size > 1 {
		podTemplateApplyConfiguration.Spec.WithReadinessGates(
			coreapplyv1.PodReadinessGate().WithConditionType(leaderworkerset.GroupReadyConditionType))
	}

	deploymentLabels, deploymentAnnotations := leaderMetadata(lws, revisionKey)

	deploymentConfig := appsapplyv1.Deployment(lws.Name, lws.Namespace).
		WithSpec(appsapplyv1.DeploymentSpec().
			WithReplicas(*lws.Spec.Replicas).
			WithTemplate(podTemplateApplyConfiguration).
			WithStrategy(appsapplyv1.DeploymentStrategy().
				WithType(appsv1.RollingUpdateDeploymentStrategyType).
				WithRollingUpdate(appsapplyv1.RollingUpdateDeployment().
					WithMaxUnavailable(lws.Spec.RolloutStrategy.RollingUpdateConfiguration.MaxUnavailable).
					WithMaxSurge(lws.Spec.RolloutStrategy.RollingUpdateConfiguration.MaxSurge))).
			WithSelector(metaapplyv1.LabelSelector().
				WithMatchLabels(map[string]string{
					leaderworkerset.SetNameLabelKey:     lws.Name,
					leaderworkerset.WorkerIndexLabelKey: "0",
				}))).
		WithLabels(deploymentLabels).
		WithAnnotations(deploymentAnnotations)

	return deploymentConfig, nil
}

// updateStatusHash computes LWS status from the leader Deployment. Because pod
// readiness includes the group-ready gate, the Deployment's readyReplicas already
// counts fully ready groups rather than bare leader pods.
func (r *LeaderWorkerSetReconciler) updateStatusHash(ctx context.Context, lws *leaderworkerset.LeaderWorkerSet) (bool, error) {
	log := ctrl.LoggerFrom(ctx)
	updateStatus := false

	deploy := &appsv1.Deployment{}
	if err := r.Get(ctx, types.NamespacedName{Name: lws.Name, Namespace: lws.Namespace}, deploy); err != nil {
		log.Error(err, "Error retrieving leader Deployment")
		return false, err
	}

	leaderPodList := &corev1.PodList{}
	if err := r.List(ctx, leaderPodList, client.InNamespace(lws.Namespace), client.MatchingLabels{
		leaderworkerset.SetNameLabelKey:     lws.Name,
		leaderworkerset.WorkerIndexLabelKey: "0",
	}); err != nil {
		log.Error(err, "Fetching leaderPods")
		return false, err
	}

	deployRevision := revisionutils.GetRevisionKey(deploy)
	degradedGroupCount := 0
	degradedReadyCount := int32(0)
	currentRevisionPodCount := 0
	oldRevisionPodCount := 0
	for i := range leaderPodList.Items {
		pod := &leaderPodList.Items[i]
		if deployRevision != "" {
			if revisionutils.GetRevisionKey(pod) == deployRevision {
				currentRevisionPodCount++
			} else if pod.DeletionTimestamp == nil {
				oldRevisionPodCount++
			}
		}
		if pod.Annotations[leaderworkerset.GroupRestartBudgetExhaustedAnnotationKey] == "true" {
			degradedGroupCount++
			if pod.DeletionTimestamp == nil && podutils.IsPodReady(pod) {
				degradedReadyCount++
			}
		}
	}

	if lws.Status.Replicas != deploy.Status.Replicas {
		lws.Status.Replicas = deploy.Status.Replicas
		updateStatus = true
	}
	if lws.Status.ReadyReplicas != deploy.Status.ReadyReplicas {
		lws.Status.ReadyReplicas = deploy.Status.ReadyReplicas
		updateStatus = true
	}
	if lws.Status.UpdatedReplicas != deploy.Status.UpdatedReplicas {
		lws.Status.UpdatedReplicas = deploy.Status.UpdatedReplicas
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

	var conditions []metav1.Condition
	lwsReplicas := *lws.Spec.Replicas
	readyNonDegradedCount := max(int32(0), deploy.Status.ReadyReplicas-degradedReadyCount)
	degraded := degradedGroupCount > 0
	deploymentCurrent := deploy.Status.ObservedGeneration >= deploy.Generation &&
		(deployRevision == "" || (oldRevisionPodCount == 0 && (len(leaderPodList.Items) == 0 || currentRevisionPodCount >= int(lwsReplicas))))
	updateInProgress := !deploymentCurrent || deploy.Status.UpdatedReplicas < deploy.Status.Replicas
	available := deploymentCurrent && !degraded &&
		deploy.Status.Replicas == lwsReplicas &&
		readyNonDegradedCount == lwsReplicas &&
		deploy.Status.UpdatedReplicas == lwsReplicas
	targetReplicas := max(int(lwsReplicas), int(deploy.Status.Replicas))
	progressing := (updateInProgress && !degraded) || int(readyNonDegradedCount)+degradedGroupCount < targetReplicas
	if updateInProgress {
		if degraded {
			conditions = append(conditions, makeFalseCondition(leaderworkerset.LeaderWorkerSetAvailable, lws, "ReplicaRestartBudgetExceeded", "Not all replicas are ready"))
		}
		conditions = append(conditions, makeCondition(leaderworkerset.LeaderWorkerSetUpdateInProgress, lws))
		if progressing {
			conditions = append(conditions, makeCondition(leaderworkerset.LeaderWorkerSetProgressing, lws))
		} else {
			conditions = append(conditions, makeFalseCondition(leaderworkerset.LeaderWorkerSetProgressing, lws, "ReplicaRestartBudgetExceeded", "Automatic recovery is stopped for one or more replicas"))
		}
	} else if available {
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
	}
	if degraded {
		conditions = append(conditions, makeCondition(leaderworkerset.LeaderWorkerSetDegraded, lws))
	} else {
		conditions = append(conditions, makeFalseCondition(leaderworkerset.LeaderWorkerSetDegraded, lws, "AsExpected", "No replica has exhausted its restart budget"))
	}

	updateCondition := setConditions(lws, conditions)
	if updateCondition {
		eventCondition := conditions[0]
		if degraded {
			eventCondition = conditions[len(conditions)-1]
		}
		r.Record.Eventf(lws, nil, corev1.EventTypeNormal, eventCondition.Reason, Update, eventCondition.Message+fmt.Sprintf(", with %d groups ready of total %d groups", deploy.Status.ReadyReplicas, lwsReplicas))
	}
	if updateStatus || updateCondition {
		if err := r.Status().Update(ctx, lws); err != nil {
			if !apierrors.IsConflict(err) {
				log.Error(err, "Updating LeaderWorkerSet status and/or condition.")
			}
			return false, err
		}
	}
	return available, nil
}

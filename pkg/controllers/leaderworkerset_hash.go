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
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	appsapplyv1 "k8s.io/client-go/applyconfigurations/apps/v1"
	coreapplyv1 "k8s.io/client-go/applyconfigurations/core/v1"
	metaapplyv1 "k8s.io/client-go/applyconfigurations/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"

	leaderworkerset "sigs.k8s.io/lws/api/leaderworkerset/v1"
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

	updatedRevision, err := r.getUpdatedRevision(ctx, deploy != nil, lws, revision)
	if err != nil {
		log.Error(err, "Validating if LWS has been updated")
		return ctrl.Result{}, err
	}
	if updatedRevision != nil {
		revision, err = revisionutils.CreateRevision(ctx, r.Client, updatedRevision)
		if err != nil {
			log.Error(err, "Creating revision for updated LWS")
			return ctrl.Result{}, err
		}
		r.Record.Eventf(lws, revision, corev1.EventTypeNormal, CreatingRevision, Create, fmt.Sprintf("Creating revision with key %s for updated LWS", revisionutils.GetRevisionKey(revision)))
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
	}
	log.V(2).Info("Leader Reconcile (hash identity) completed.")
	return ctrl.Result{}, nil
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
	updateInProgress := deploy.Status.UpdatedReplicas < deploy.Status.Replicas
	available := deploy.Status.Replicas == lwsReplicas &&
		deploy.Status.ReadyReplicas == lwsReplicas &&
		deploy.Status.UpdatedReplicas == lwsReplicas
	if updateInProgress {
		conditions = append(conditions, makeCondition(leaderworkerset.LeaderWorkerSetUpdateInProgress, lws))
		conditions = append(conditions, makeCondition(leaderworkerset.LeaderWorkerSetProgressing, lws))
	} else if available {
		conditions = append(conditions, makeCondition(leaderworkerset.LeaderWorkerSetAvailable, lws))
	} else {
		conditions = append(conditions, makeCondition(leaderworkerset.LeaderWorkerSetProgressing, lws))
	}

	updateCondition := setConditions(lws, conditions)
	if updateCondition {
		r.Record.Eventf(lws, nil, corev1.EventTypeNormal, conditions[0].Reason, Update, conditions[0].Message+fmt.Sprintf(", with %d groups ready of total %d groups", deploy.Status.ReadyReplicas, lwsReplicas))
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

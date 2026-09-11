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
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	leaderworkerset "sigs.k8s.io/lws/api/leaderworkerset/v1"
	revisionutils "sigs.k8s.io/lws/pkg/utils/revision"
)

func (r *LeaderWorkerSetReconciler) combinedReader() client.Reader {
	if r.APIReader != nil {
		return r.APIReader
	}
	// Direct-client tests and embedders without a manager may supply Client.
	// The production manager always installs its uncached APIReader.
	return r.Client
}

func combinedBaseline(sts *appsv1.StatefulSet) (int32, error) {
	n, err := strconv.ParseInt(sts.Annotations[leaderworkerset.ReplicasAnnotationKey], 10, 32)
	if err != nil || n < 0 || sts.Spec.Replicas == nil {
		return 0, fmt.Errorf("missing/invalid pre-operation non-surge replica target")
	}
	return min(int32(n), *sts.Spec.Replicas), nil
}

func (r *LeaderWorkerSetReconciler) reconcileCombinedRollout(ctx context.Context, lws *leaderworkerset.LeaderWorkerSet, sts *appsv1.StatefulSet, revision string, templateChanged bool) (bool, ctrl.Result, error) {
	wait := ctrl.Result{RequeueAfter: time.Second}
	if sts == nil {
		return false, ctrl.Result{}, nil
	}
	raw, active := sts.Annotations[combinedRolloutAnnotation]
	baseline, baselineErr := combinedBaseline(sts)
	rolling := sts.Spec.UpdateStrategy.RollingUpdate
	protected := *lws.Spec.RolloutStrategy.RollingUpdateConfiguration.Partition
	// LWS revisions omit supported top-level topology annotations. Fingerprint
	// the constructed template as well, without conflating it with native hashes.
	config, err := constructLeaderStatefulSetApplyConfiguration(lws, 0, *lws.Spec.Replicas, revision)
	if err != nil {
		return true, wait, err
	}
	templateJSON, err := json.Marshal(config.Spec.Template)
	if err != nil {
		return true, wait, err
	}
	templateKey := fmt.Sprintf("%x", sha256.Sum256(templateJSON))
	if !active {
		if baselineErr != nil {
			return false, ctrl.Result{}, nil
		}
		inProgress := rolling != nil && rolling.Partition != nil && *rolling.Partition > *lws.Spec.RolloutStrategy.RollingUpdateConfiguration.Partition
		inProgress = inProgress || sts.Status.CurrentRevision != sts.Status.UpdateRevision
		// Compare the annotations copied outside the LWS revision, including removal.
		for _, key := range []string{leaderworkerset.ExclusiveKeyAnnotationKey, leaderworkerset.SubGroupExclusiveKeyAnnotationKey} {
			templateChanged = templateChanged || sts.Spec.Template.Annotations[key] != config.Spec.Template.Annotations[key]
		}
		if *lws.Spec.Replicas <= baseline || (!templateChanged && !inProgress) {
			return false, ctrl.Result{}, nil
		}
	}
	if rolling == nil || rolling.Partition == nil || sts.Spec.Replicas == nil || sts.Spec.UpdateStrategy.Type != appsv1.RollingUpdateStatefulSetStrategyType {
		return true, wait, fmt.Errorf("combined rollout requires native RollingUpdate partition")
	}
	// Never construct a write from a stale cached StatefulSet or LWS generation.
	if err := r.combinedFence(ctx, lws, sts); err != nil {
		return true, wait, err
	}
	var state combinedModelState
	if active {
		state, err = combinedModelDecode(raw)
		if err != nil || state.Phase == "" || state.Generation > lws.Generation {
			return true, wait, fmt.Errorf("invalid combined rollout state; no mutation or deletion: %v", err)
		}
	} else {
		state = combinedModelState{Version: 1, Baseline: baseline, Desired: *lws.Spec.Replicas, Generation: lws.Generation, Revision: revision, Phase: combinedFreeze}
	}
	// Initial entry and supersession freeze the ACTUAL old template first.
	// A native per-set sync must acknowledge that fence before publication.
	if !active || state.Revision != revision || state.TargetTemplate != templateKey || protected > state.Protected {
		for ordinal, reservation := range state.Reservations {
			reservation.Revision = revision
			state.Reservations[ordinal] = reservation
		}
		state.Phase, state.Revision, state.Generation = combinedFreeze, revision, lws.Generation
		state.NativeRevision, state.TargetTemplate, state.Protected = "", templateKey, protected
		frozen := sts.DeepCopy()
		frozen.Spec.UpdateStrategy.RollingUpdate.Partition = ptr.To(max(*sts.Spec.Replicas, protected))
		return true, wait, r.applyCombinedStatefulSet(ctx, lws, sts, frozen, combinedModelEncode(state))
	}
	if sts.Status.ObservedGeneration < sts.Generation {
		return true, wait, nil
	}
	if state.Phase == combinedFreeze {
		if *rolling.Partition < *sts.Spec.Replicas {
			return true, wait, fmt.Errorf("combined freeze fence was changed")
		}
		// Keep the old replica count until the new template is acknowledged.
		state.Phase, state.Generation = combinedPublish, lws.Generation
		return true, wait, r.publishCombinedStatefulSet(ctx, lws, sts, state)
	}
	if sts.Status.UpdateRevision == "" || revisionutils.GetRevisionKey(sts) != revision || sts.Spec.Template.Labels[leaderworkerset.RevisionKey] != revision {
		return true, wait, fmt.Errorf("combined rollout template/revision fence changed")
	}
	if state.Phase == combinedPublish {
		if *rolling.Partition < *sts.Spec.Replicas {
			return true, wait, fmt.Errorf("combined publication fence was changed")
		}
		state.NativeRevision = sts.Status.UpdateRevision
		// Only an acknowledged full fence may retire a surviving same-UID
		// obligation. Readiness recovery alone must never refund authorization.
		groups, err := r.observeCombinedGroups(ctx, lws, *sts.Spec.Replicas)
		if err != nil {
			return true, wait, err
		}
		for i := range state.Reservations {
			if i < protected || (i < int32(len(groups)) && combinedLeaderAtTarget(groups[i].leader, revision, state.NativeRevision)) {
				delete(state.Reservations, i)
			}
		}
		state.Phase, state.Generation, state.Protected = combinedActive, lws.Generation, protected
		return true, wait, r.applyCombinedStatefulSet(ctx, lws, sts, sts.DeepCopy(), combinedModelEncode(state))
	}
	if state.NativeRevision == "" || sts.Status.UpdateRevision != state.NativeRevision {
		return true, wait, fmt.Errorf("combined rollout native target changed outside publication fence")
	}
	groups, err := r.observeCombinedGroups(ctx, lws, max(*sts.Spec.Replicas, *lws.Spec.Replicas))
	if err != nil {
		return true, wait, err
	}
	budgets := lws.Spec.RolloutStrategy.RollingUpdateConfiguration
	plan, err := combinedModelPlanUpdate(combinedModelInput{
		baseline: state.Baseline, desired: *lws.Spec.Replicas, replicas: *sts.Spec.Replicas,
		partition: *rolling.Partition, protected: protected, generation: lws.Generation,
		revision: revision, nativeRevision: state.NativeRevision, stsUID: sts.UID, unavailable: budgets.MaxUnavailable, surge: budgets.MaxSurge,
		groups: groups, state: raw,
	})
	if err != nil {
		return true, wait, err
	}
	next := sts.DeepCopy()
	next.Spec.Replicas = ptr.To(plan.replicas)
	next.Spec.UpdateStrategy.RollingUpdate.Partition = ptr.To(plan.partition)
	next.Annotations[leaderworkerset.ReplicasAnnotationKey] = strconv.Itoa(int(*lws.Spec.Replicas))
	if plan.done {
		// Retain B until the scale-down is acknowledged and surplus leaders
		// (including terminating leaders) are gone. Do not count cleanup twice.
		clean := *sts.Spec.Replicas == plan.replicas && sts.Status.Replicas == plan.replicas
		// Ready Pods can precede native promotion. Keep history until the native
		// current template catches up; protected old groups intentionally differ.
		clean = clean && (protected > 0 || sts.Status.CurrentRevision == state.NativeRevision)
		for i := plan.replicas; i < int32(len(groups)); i++ {
			clean = clean && groups[i].leader == nil
		}
		if !clean {
			state.Desired, state.Generation = *lws.Spec.Replicas, lws.Generation
			plan.state = combinedModelEncode(state)
		}
	}
	changed := plan.state != raw || plan.partition != *rolling.Partition || plan.replicas != *sts.Spec.Replicas
	if changed {
		// Reservation and eligibility move atomically; never delete on this
		// pass. Re-observe the applied generation before an explicit deletion.
		return true, wait, r.applyCombinedStatefulSet(ctx, lws, sts, next, plan.state)
	}
	for _, pod := range plan.deletes {
		if err := r.deleteCombinedLeader(ctx, lws, sts, pod); err != nil {
			return true, wait, err
		}
	}
	if plan.blocked && r.Record != nil {
		r.Record.Eventf(lws, sts, corev1.EventTypeNormal, "CombinedRolloutBudgetBlocked", Update, "Waiting for whole-group credit or reservation capacity; cannot expose an unaffordable old suffix")
	}
	if err := r.reconcileHeadlessServices(ctx, lws); err != nil {
		return true, wait, err
	}
	// Legacy status is informational only; it never clears this persisted
	// protocol or truncates protected history during an active operation.
	_, err = r.updateStatus(ctx, lws, revision)
	return true, wait, err
}

func (r *LeaderWorkerSetReconciler) combinedFence(ctx context.Context, lws *leaderworkerset.LeaderWorkerSet, sts *appsv1.StatefulSet) error {
	var liveLWS leaderworkerset.LeaderWorkerSet
	var liveSTS appsv1.StatefulSet
	if err := r.combinedReader().Get(ctx, client.ObjectKeyFromObject(lws), &liveLWS); err != nil {
		return err
	}
	if err := r.combinedReader().Get(ctx, client.ObjectKeyFromObject(sts), &liveSTS); err != nil {
		return err
	}
	if liveLWS.UID != lws.UID || liveLWS.Generation != lws.Generation || liveLWS.ResourceVersion != lws.ResourceVersion || liveLWS.DeletionTimestamp != nil ||
		liveSTS.UID != sts.UID || liveSTS.ResourceVersion != sts.ResourceVersion || liveSTS.DeletionTimestamp != nil ||
		!combinedModelOwnedBy(&liveSTS, "LeaderWorkerSet", liveLWS.UID) {
		return apierrors.NewConflict(appsv1.Resource("statefulsets"), sts.Name, fmt.Errorf("combined rollout observation changed"))
	}
	return nil
}

// Operational deltas must not acquire ownership of merely observed fields.
// Optimistic-lock merge patches atomically couple eligibility and reservations.
func (r *LeaderWorkerSetReconciler) applyCombinedStatefulSet(ctx context.Context, lws *leaderworkerset.LeaderWorkerSet, old, next *appsv1.StatefulSet, state string) error {
	if len(state) > combinedStateLimit {
		return fmt.Errorf("combined rollout state exceeds bound")
	}
	if err := r.combinedFence(ctx, lws, old); err != nil {
		return err
	}
	next.Spec.UpdateStrategy.RollingUpdate.MaxUnavailable = ptr.To(intstr.FromInt(1))
	if next.Annotations == nil {
		next.Annotations = make(map[string]string)
	}
	if state == "" {
		delete(next.Annotations, combinedRolloutAnnotation)
	} else {
		next.Annotations[combinedRolloutAnnotation] = state
	}
	return r.Patch(ctx, next, client.MergeFromWithOptions(old, client.MergeFromWithOptimisticLock{}), client.FieldOwner(fieldManager))
}

// Publish the COMPLETE intended declarative configuration, never the live
// object or a partial apply under the existing manager (which would prune its
// other fields). Independent StatefulSet/template metadata remains unowned.
func (r *LeaderWorkerSetReconciler) publishCombinedStatefulSet(ctx context.Context, lws *leaderworkerset.LeaderWorkerSet, sts *appsv1.StatefulSet, state combinedModelState) error {
	if err := r.combinedFence(ctx, lws, sts); err != nil {
		return err
	}
	config, err := constructLeaderStatefulSetApplyConfiguration(lws, max(*sts.Spec.Replicas, state.Protected), *sts.Spec.Replicas, state.Revision)
	if err != nil {
		return err
	}
	if err := setControllerReferenceWithStatefulSet(lws, config, r.Scheme); err != nil {
		return err
	}
	raw := combinedModelEncode(state)
	if len(raw) > combinedStateLimit {
		return fmt.Errorf("combined rollout state exceeds bound")
	}
	config.WithResourceVersion(sts.ResourceVersion).WithAnnotations(map[string]string{
		combinedRolloutAnnotation:             raw,
		leaderworkerset.ReplicasAnnotationKey: sts.Annotations[leaderworkerset.ReplicasAnnotationKey],
	})
	config.Spec.UpdateStrategy.RollingUpdate.WithMaxUnavailable(intstr.FromInt(1))
	obj, err := runtime.DefaultUnstructuredConverter.ToUnstructured(config)
	if err != nil {
		return err
	}
	patch := &unstructured.Unstructured{Object: obj}
	return r.Patch(ctx, patch, client.Apply, client.FieldOwner(fieldManager), client.ForceOwnership) //nolint:staticcheck // complete declarative SSA with RV fence
}

func (r *LeaderWorkerSetReconciler) deleteCombinedLeader(ctx context.Context, lws *leaderworkerset.LeaderWorkerSet, sts *appsv1.StatefulSet, pod *corev1.Pod) error {
	if err := r.combinedFence(ctx, lws, sts); err != nil {
		return err
	}
	s, err := combinedModelDecode(sts.Annotations[combinedRolloutAnnotation])
	if err != nil {
		return err
	}
	i, err := strconv.Atoi(pod.Labels[leaderworkerset.GroupIndexLabelKey])
	if err != nil {
		return err
	}
	reservation, ok := s.Reservations[int32(i)]
	if !ok || s.Phase != combinedActive || s.Generation != lws.Generation || s.Revision != revisionutils.GetRevisionKey(sts) ||
		sts.Status.ObservedGeneration < sts.Generation || s.NativeRevision == "" || sts.Status.UpdateRevision != s.NativeRevision ||
		reservation.UID != pod.UID || reservation.Revision != s.Revision ||
		int32(i) < *sts.Spec.UpdateStrategy.RollingUpdate.Partition || int32(i) < *lws.Spec.RolloutStrategy.RollingUpdateConfiguration.Partition ||
		!combinedModelOwnedBy(pod, "StatefulSet", sts.UID) || pod.DeletionTimestamp != nil || combinedLeaderAtTarget(pod, s.Revision, s.NativeRevision) {
		return fmt.Errorf("combined deletion is not covered by current reservation")
	}
	// Normal grace, UID/RV-preconditioned, foreground GC removes old workers.
	err = r.Delete(ctx, pod, &client.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &pod.UID, ResourceVersion: &pod.ResourceVersion}, PropagationPolicy: ptr.To(metav1.DeletePropagationForeground)})
	return client.IgnoreNotFound(err)
}

func (r *LeaderWorkerSetReconciler) observeCombinedGroups(ctx context.Context, lws *leaderworkerset.LeaderWorkerSet, count int32) ([]combinedModelGroup, error) {
	var pods corev1.PodList
	var sets appsv1.StatefulSetList
	selector := client.MatchingLabels{leaderworkerset.SetNameLabelKey: lws.Name}
	if err := r.combinedReader().List(ctx, &pods, client.InNamespace(lws.Namespace), selector); err != nil {
		return nil, err
	}
	if err := r.combinedReader().List(ctx, &sets, client.InNamespace(lws.Namespace), selector); err != nil {
		return nil, err
	}
	// Include condemned ordinals for cleanup checks, but the planner never
	// counts them above its planned replica target.
	for _, p := range pods.Items {
		i, err := strconv.ParseInt(p.Labels[leaderworkerset.GroupIndexLabelKey], 10, 32)
		if err == nil && i >= 0 && p.Name == fmt.Sprintf("%s-%d", lws.Name, i) {
			count = max(count, int32(i)+1)
		}
	}
	groups := make([]combinedModelGroup, count)
	for i := range pods.Items {
		p := &pods.Items[i]
		n, err := strconv.ParseInt(p.Labels[leaderworkerset.GroupIndexLabelKey], 10, 32)
		if err != nil || n < 0 || n >= int64(count) {
			continue
		}
		if p.Name == fmt.Sprintf("%s-%d", lws.Name, n) && p.Labels[leaderworkerset.WorkerIndexLabelKey] == "0" {
			groups[n].leader = p
		} else {
			groups[n].pods = append(groups[n].pods, *p)
		}
	}
	for i := range sets.Items {
		w := &sets.Items[i]
		n, err := strconv.ParseInt(w.Labels[leaderworkerset.GroupIndexLabelKey], 10, 32)
		if err == nil && n >= 0 && n < int64(count) && w.Name == fmt.Sprintf("%s-%d", lws.Name, n) {
			groups[n].workers = w
		}
	}
	return groups, nil
}

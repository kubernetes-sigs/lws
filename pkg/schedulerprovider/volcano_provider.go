/*
Copyright 2025.

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

package schedulerprovider

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	volcanov1beta1 "volcano.sh/apis/pkg/apis/scheduling/v1beta1"

	leaderworkerset "sigs.k8s.io/lws/api/leaderworkerset/v1"
	"sigs.k8s.io/lws/pkg/utils"
)

const (
	Volcano                                 ProviderType = "volcano"
	VolcanoAnnotationPrefix                 string       = "volcano.sh/"
	VolcanoNetworkTopologyModeAnnotationKey              = VolcanoAnnotationPrefix + "network-topology-mode"
	VolcanoNetworkTopologyTierAnnotationKey              = VolcanoAnnotationPrefix + "network-topology-highest-tier-allowed"
)

type VolcanoProvider struct {
	client client.Client
}

func NewVolcanoProvider(client client.Client) *VolcanoProvider {
	return &VolcanoProvider{
		client: client,
	}
}

// ReconcileScheduling validates spec.scheduling for Volcano. Replica PodGroups
// are materialized per leader pod via CreatePodGroupIfNotExists before the
// leader pod's scheduling gate is lifted.
func (v *VolcanoProvider) ReconcileScheduling(ctx context.Context, lws *leaderworkerset.LeaderWorkerSet, replicas int32, revision string) error {
	if lws.Spec.Scheduling == nil {
		return nil
	}
	mode, err := SchedulingModeFor(lws)
	if err != nil {
		return NewReconcileError(ReasonInvalidSchedulingConfiguration, err)
	}
	if mode != SchedulingModeReplica {
		return NewReconcileError(ReasonUnsupportedProviderCapability, fmt.Errorf("the Volcano provider supports the typed API only at replica level"))
	}
	if config := lws.Spec.Scheduling.Replica; config != nil {
		if (config.SchedulingPolicy != nil && config.SchedulingPolicy.Basic != nil) || config.SchedulingConstraints != nil || config.DisruptionMode != nil {
			return NewReconcileError(ReasonUnsupportedProviderCapability, fmt.Errorf("the Volcano provider supports only replica gang policy in the typed API"))
		}
	}
	if _, err := NetworkTopologyFromAnnotations(lws); err != nil {
		return NewReconcileError(ReasonInvalidSchedulingConfiguration, err)
	}
	return nil
}

func (v *VolcanoProvider) CreatePodGroupIfNotExists(ctx context.Context, lws *leaderworkerset.LeaderWorkerSet, leaderPod *corev1.Pod) error {
	var pg volcanov1beta1.PodGroup
	pgName := leaderPod.Annotations[volcanov1beta1.KubeGroupNameAnnotationKey]
	if pgName == "" {
		if lws.Spec.Scheduling != nil ||
			leaderPod.Spec.SchedulerName == string(Volcano) ||
			lws.Spec.LeaderWorkerTemplate.WorkerTemplate.Spec.SchedulerName == string(Volcano) {
			return fmt.Errorf("pod %s/%s is missing annotation %q required for Volcano gang scheduling", leaderPod.Namespace, leaderPod.Name, volcanov1beta1.KubeGroupNameAnnotationKey)
		}
		// Pods created before the provider was enabled did not receive the
		// annotation from the CREATE webhook. Leave those groups on their
		// existing scheduler without adding a PodGroup.
		return nil
	}
	log := ctrl.LoggerFrom(ctx).WithValues("podGroup", pgName, "namespace", lws.Namespace)

	// With spec.scheduling, Ordinal PodGroups are owned by the LWS since an
	// ordinal reuses its name across restarts. Hash groups never reuse a name,
	// so the leader owns them and they are garbage collected with it.
	ownedByLWS := lws.Spec.Scheduling != nil && lws.Spec.GroupIdentity != leaderworkerset.GroupIdentityHash

	if err := v.client.Get(ctx, types.NamespacedName{Name: pgName, Namespace: lws.Namespace}, &pg); err == nil {
		if pg.DeletionTimestamp != nil {
			return fmt.Errorf("waiting for podgroup %s/%s to finish deletion", pg.Namespace, pgName)
		}

		owner := metav1.GetControllerOf(&pg)
		if ownedByLWS {
			if owner == nil ||
				owner.APIVersion != leaderworkerset.GroupVersion.String() ||
				owner.Kind != "LeaderWorkerSet" ||
				owner.Name != lws.Name {
				return fmt.Errorf("%w: podgroup %s/%s has controller owner %+v; expected LeaderWorkerSet %s with UID %s", ErrUnexpectedPodGroupOwner, pg.Namespace, pgName, owner, lws.Name, lws.UID)
			}
			if owner.UID == lws.UID {
				return v.syncPodGroupPriorityClassName(ctx, lws, &pg)
			}
			return fmt.Errorf("waiting for podgroup %s/%s owned by previous LeaderWorkerSet UID %s to be deleted; current LeaderWorkerSet UID is %s", pg.Namespace, pgName, owner.UID, lws.UID)
		}
		// Earlier releases made hash PodGroups LWS-owned.
		if lws.Spec.GroupIdentity == leaderworkerset.GroupIdentityHash && owner != nil && owner.Kind == "LeaderWorkerSet" && owner.UID == lws.UID {
			return v.syncPodGroupPriorityClassName(ctx, lws, &pg)
		}

		// LWS-created PodGroups are always controlled by their leader Pod in legacy annotation mode. This should not happen during
		// normal reconciliation, so fail without modifying a same-name PodGroup with an unexpected owner.
		if owner == nil ||
			owner.APIVersion != corev1.SchemeGroupVersion.String() ||
			owner.Kind != "Pod" ||
			owner.Name != leaderPod.Name {
			return fmt.Errorf("%w: podgroup %s/%s has controller owner %+v; expected v1 Pod %s with UID %s", ErrUnexpectedPodGroupOwner, pg.Namespace, pgName, owner, leaderPod.Name, leaderPod.UID)
		}
		if owner.UID == leaderPod.UID {
			return v.syncPodGroupPriorityClassName(ctx, lws, &pg)
		}

		// The stale PodGroup is owned by the previous leader Pod. Wait for owner-reference garbage collection
		// to delete it, then create a PodGroup owned by the current leader Pod on a subsequent reconciliation.
		return fmt.Errorf("waiting for podgroup %s/%s owned by previous leader UID %s to be deleted; current leader UID is %s", pg.Namespace, pgName, owner.UID, leaderPod.UID)
	} else if client.IgnoreNotFound(err) != nil {
		return err
	}

	networkTopology, err := NetworkTopologyFromAnnotations(lws)
	if err != nil {
		return err
	}
	minResources := utils.CalculatePGMinResources(lws)
	pg = volcanov1beta1.PodGroup{
		ObjectMeta: metav1.ObjectMeta{
			Name:      pgName,
			Namespace: lws.Namespace,
			Labels: map[string]string{
				leaderworkerset.SetNameLabelKey:    lws.Name,
				leaderworkerset.GroupIndexLabelKey: leaderPod.Labels[leaderworkerset.GroupIndexLabelKey],
				leaderworkerset.RevisionKey:        leaderPod.Labels[leaderworkerset.RevisionKey],
			},
			Annotations: inheritVolcanoAnnotations(lws),
		},
		Spec: volcanov1beta1.PodGroupSpec{
			// Default startupPolicy is LeaderCreated, Leader and Workers Pods are scheduled together, so MinAvailable is set to size
			MinMember:         *lws.Spec.LeaderWorkerTemplate.Size,
			MinResources:      &minResources,
			PriorityClassName: podGroupPriorityClassName(lws),
			NetworkTopology:   networkTopology,
		},
	}

	// If the StartUpPolicy of lws is LeaderReady, set MinMember to 1 to allow the Leader Pod to be scheduled.
	// However, minResources should still be set to the minResources of the PodGroup (1 Leader + (size-1) Workers).
	// If the cluster resources are insufficient, scheduling the Leader Pod alone would be meaningless,
	// as at least one Worker will definitely not be able to be scheduled.
	if lws.Spec.StartupPolicy == leaderworkerset.LeaderReadyStartupPolicy {
		pg.Spec.MinMember = 1
	}

	if queueName, ok := lws.Annotations[volcanov1beta1.QueueNameAnnotationKey]; ok {
		pg.Spec.Queue = queueName
	}

	var controllerOwner metav1.Object = leaderPod
	if ownedByLWS {
		controllerOwner = lws
	}
	err = ctrl.SetControllerReference(controllerOwner, &pg, v.client.Scheme())
	if err != nil {
		return err
	}

	if err = v.client.Create(ctx, &pg); err != nil {
		return err
	}
	log.V(2).Info("Created PodGroup for LeaderWorkerSet")

	return nil
}

// podGroupPriorityClassName returns the PodGroup priority class, or "" when the
// leader and worker templates disagree and no single class describes the group.
func podGroupPriorityClassName(lws *leaderworkerset.LeaderWorkerSet) string {
	// A size 1 group has only the leader, so the unused worker template does not matter.
	if *lws.Spec.LeaderWorkerTemplate.Size == 1 && lws.Spec.LeaderWorkerTemplate.LeaderTemplate != nil {
		return lws.Spec.LeaderWorkerTemplate.LeaderTemplate.Spec.PriorityClassName
	}
	priorityClassName, consistent := workloadPriorityClassName(lws)
	if !consistent {
		return ""
	}
	return priorityClassName
}

// syncPodGroupPriorityClassName patches an existing PodGroup whose priority class
// does not match the LeaderWorkerSet, so PodGroups created before the field was set get it.
func (v *VolcanoProvider) syncPodGroupPriorityClassName(ctx context.Context, lws *leaderworkerset.LeaderWorkerSet, pg *volcanov1beta1.PodGroup) error {
	desired := podGroupPriorityClassName(lws)
	if pg.Spec.PriorityClassName == desired {
		return nil
	}
	patch := client.MergeFrom(pg.DeepCopy())
	pg.Spec.PriorityClassName = desired
	return v.client.Patch(ctx, pg, patch)
}

func (v *VolcanoProvider) InjectPodGroupMetadata(pod *corev1.Pod) error {
	lwsName := pod.Labels[leaderworkerset.SetNameLabelKey]
	groupIndex := pod.Labels[leaderworkerset.GroupIndexLabelKey]
	pod.Annotations[volcanov1beta1.KubeGroupNameAnnotationKey] = GetPodGroupName(lwsName, groupIndex, pod.Labels[leaderworkerset.RevisionKey])

	return nil
}

// NetworkTopologyFromAnnotations parses the Volcano topology annotations shared
// by admission validation and PodGroup creation. It returns nil when neither is set.
func NetworkTopologyFromAnnotations(lws *leaderworkerset.LeaderWorkerSet) (*volcanov1beta1.NetworkTopologySpec, error) {
	tierValue, hasTier := lws.Annotations[VolcanoNetworkTopologyTierAnnotationKey]
	modeValue, hasMode := lws.Annotations[VolcanoNetworkTopologyModeAnnotationKey]
	if !hasTier {
		if hasMode {
			return nil, fmt.Errorf("LeaderWorkerSet %s/%s: %s requires %s", lws.Namespace, lws.Name, VolcanoNetworkTopologyModeAnnotationKey, VolcanoNetworkTopologyTierAnnotationKey)
		}
		return nil, nil
	}
	tier, err := strconv.Atoi(tierValue)
	if err != nil || tier < 0 {
		return nil, fmt.Errorf("LeaderWorkerSet %s/%s: %s must be a non-negative integer, got %q", lws.Namespace, lws.Name, VolcanoNetworkTopologyTierAnnotationKey, tierValue)
	}
	mode := volcanov1beta1.HardNetworkTopologyMode
	if hasMode {
		mode = volcanov1beta1.NetworkTopologyMode(modeValue)
		if mode != volcanov1beta1.HardNetworkTopologyMode && mode != volcanov1beta1.SoftNetworkTopologyMode {
			return nil, fmt.Errorf("LeaderWorkerSet %s/%s: %s must be hard or soft, got %q", lws.Namespace, lws.Name, VolcanoNetworkTopologyModeAnnotationKey, modeValue)
		}
	}
	return &volcanov1beta1.NetworkTopologySpec{
		Mode:               mode,
		HighestTierAllowed: &tier,
	}, nil
}

func inheritVolcanoAnnotations(lws *leaderworkerset.LeaderWorkerSet) map[string]string {
	res := map[string]string{}
	for k, v := range lws.Annotations {
		if strings.HasPrefix(k, VolcanoAnnotationPrefix) {
			res[k] = v
		}
	}
	return res
}

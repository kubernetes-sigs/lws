/*
Copyright 2023 The Kubernetes Authors.
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

package webhooks

import (
	"context"
	"fmt"
	"strconv"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilrand "k8s.io/apimachinery/pkg/util/rand"
	ctrl "sigs.k8s.io/controller-runtime"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	leaderworkerset "sigs.k8s.io/lws/api/leaderworkerset/v1"
	"sigs.k8s.io/lws/pkg/schedulerprovider"
	"sigs.k8s.io/lws/pkg/utils"
	acceleratorutils "sigs.k8s.io/lws/pkg/utils/accelerators"
	podutils "sigs.k8s.io/lws/pkg/utils/pod"
	statefulsetutils "sigs.k8s.io/lws/pkg/utils/statefulset"
)

type PodWebhook struct {
	SchedulerProvider schedulerprovider.SchedulerProvider
}

func NewPodWebhook(sp schedulerprovider.SchedulerProvider) *PodWebhook {
	return &PodWebhook{SchedulerProvider: sp}
}

func (p *PodWebhook) Setup(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr, &corev1.Pod{}).
		WithDefaulter(p).
		WithValidator(p).
		Complete()
}

//+kubebuilder:webhook:path=/validate--v1-pod,mutating=false,failurePolicy=fail,sideEffects=None,groups="",resources=pods,verbs=create;update,versions=v1,name=vpod.kb.io,sideEffects=None,admissionReviewVersions=v1

// validate admits a pod if a specific annotation exists.
func (p *PodWebhook) validate(ctx context.Context, pod *corev1.Pod) (admission.Warnings, error) {
	log := logf.FromContext(ctx)

	log.V(2).Info("Validating Pod")

	// if pod is not part of leaderworkerset, skip
	_, found := pod.Labels[leaderworkerset.SetNameLabelKey]
	if !found {
		return nil, nil
	}

	return nil, nil
}

func (p *PodWebhook) ValidateCreate(ctx context.Context, pod *corev1.Pod) (admission.Warnings, error) {
	return p.validate(ctx, pod)
}

func (p *PodWebhook) ValidateUpdate(ctx context.Context, oldObj, newObj *corev1.Pod) (admission.Warnings, error) {
	return nil, nil
}

func (p *PodWebhook) ValidateDelete(ctx context.Context, pod *corev1.Pod) (admission.Warnings, error) {
	return nil, nil
}

//+kubebuilder:webhook:path=/mutate--v1-pod,mutating=true,failurePolicy=fail,groups="",resources=pods,verbs=create,versions=v1,name=mpod.kb.io,sideEffects=None,admissionReviewVersions=v1

func (p *PodWebhook) Default(ctx context.Context, pod *corev1.Pod) error {
	log := logf.FromContext(ctx)

	log.V(2).Info("Defaulting Pod")
	// if pod is not part of leaderworkerset, skip
	_, found := pod.Labels[leaderworkerset.SetNameLabelKey]
	if !found {
		return nil
	}
	size, exist := pod.Annotations[leaderworkerset.SizeAnnotationKey]
	if !exist {
		return fmt.Errorf("size annotation is unexpectedly missing for pod %s", pod.Name)
	}
	podCount, err := strconv.Atoi(size)
	if err != nil {
		return err
	}
	// adding labels for pods
	if podutils.LeaderPod(*pod) {
		hashIdentity := pod.Annotations[leaderworkerset.GroupIdentityAnnotationKey] == string(leaderworkerset.GroupIdentityHash)
		var groupUniqueKey string
		if hashIdentity {
			// Hash-identity leaders are created through a Deployment: the pod name is
			// not known at admission (generateName), so the group identity is a fresh
			// random key rather than a name-derived ordinal.
			groupUniqueKey = genGroupUniqueKey(pod.Namespace, utilrand.String(16))
			pod.Labels[leaderworkerset.GroupUniqueHashLabelKey] = groupUniqueKey
			pod.Labels[leaderworkerset.GroupIndexLabelKey] = groupUniqueKey
			// The host name is immutable, so admission is the only chance to set it.
			// The subdomain default comes from the pod template.
			if pod.Spec.Hostname == "" {
				pod.Spec.Hostname = hashLeaderHostname(pod.Labels[leaderworkerset.SetNameLabelKey], groupUniqueKey)
			}
		} else {
			_, groupIndex := statefulsetutils.GetParentNameAndOrdinal(pod.Name)
			if groupIndex == -1 {
				return fmt.Errorf("parsing pod ordinal for pod %s", pod.Name)
			}
			pod.Labels[leaderworkerset.GroupIndexLabelKey] = fmt.Sprint(groupIndex)
			groupUniqueKey = genGroupUniqueKey(pod.Namespace, pod.Name)
			pod.Labels[leaderworkerset.GroupUniqueHashLabelKey] = groupUniqueKey
		}
		subdomainPolicy, foundSubdomainPolicy := pod.Annotations[leaderworkerset.SubdomainPolicyAnnotationKey]
		if foundSubdomainPolicy && subdomainPolicy == string(leaderworkerset.SubdomainUniquePerReplica) {
			// The per replica service is named after the leader: the pod name in
			// ordinal mode, the assigned host name in hash mode.
			pod.Spec.Subdomain = pod.Name
			if hashIdentity {
				pod.Spec.Subdomain = pod.Spec.Hostname
			}
		}
		if epKey, foundEpKey := pod.Annotations[leaderworkerset.ExclusiveKeyAnnotationKey]; foundEpKey {
			SetExclusiveAffinities(pod, groupUniqueKey, epKey, leaderworkerset.GroupUniqueHashLabelKey)
		} else if spKey := pod.Annotations[leaderworkerset.ShareTopologyAnnotationKey]; spKey != "" {
			SetShareAffinities(pod, groupUniqueKey, spKey, leaderworkerset.GroupUniqueHashLabelKey)
		}
		_, foundSubGroupSize := pod.Annotations[leaderworkerset.SubGroupSizeAnnotationKey]
		subGroupPolicyType := pod.Annotations[leaderworkerset.SubGroupPolicyTypeAnnotationKey]
		if foundSubGroupSize && pod.Labels[leaderworkerset.SubGroupIndexLabelKey] == "" && (subGroupPolicyType != string(leaderworkerset.SubGroupPolicyTypeLeaderExcluded)) {
			// The leader pod always lands on SubGroup 0. In hash mode the subgroup
			// hash derives from the group key instead of the leader pod name.
			pod.Labels[leaderworkerset.SubGroupIndexLabelKey] = "0"
			subGroupKeyInput := pod.Name
			if hashIdentity {
				subGroupKeyInput = groupUniqueKey
			}
			subGroupUniqueKey := genGroupUniqueKey(subGroupKeyInput, "0")
			pod.Labels[leaderworkerset.SubGroupUniqueHashLabelKey] = subGroupUniqueKey
			if subEpKey, foundSubEpKey := pod.Annotations[leaderworkerset.SubGroupExclusiveKeyAnnotationKey]; foundSubEpKey {
				SetExclusiveAffinities(pod, subGroupUniqueKey, subEpKey, leaderworkerset.SubGroupUniqueHashLabelKey)
			}
		}
	} else {
		_, workerIndex := statefulsetutils.GetParentNameAndOrdinal(pod.Name)
		if workerIndex == -1 {
			return fmt.Errorf("parsing pod ordinal for pod %s", pod.Name)
		}
		pod.Labels[leaderworkerset.WorkerIndexLabelKey] = fmt.Sprint(workerIndex)
		subGroupSize, foundSubGroupSize := pod.Annotations[leaderworkerset.SubGroupSizeAnnotationKey]
		if foundSubGroupSize && pod.Labels[leaderworkerset.SubGroupIndexLabelKey] == "" {
			subGroupSizeInt, err := strconv.Atoi(subGroupSize)
			if err != nil {
				return err
			}
			// In hash mode the subgroup hash derives from the group key, matching
			// what the leader computed at its own admission when it had no name.
			subGroupKeyInput := pod.Annotations[leaderworkerset.LeaderPodNameAnnotationKey]
			if pod.Annotations[leaderworkerset.GroupIdentityAnnotationKey] == string(leaderworkerset.GroupIdentityHash) {
				subGroupKeyInput = pod.Labels[leaderworkerset.GroupUniqueHashLabelKey]
			}
			subGroupIndexKey := getSubGroupIndex(podCount, subGroupSizeInt, workerIndex)
			pod.Labels[leaderworkerset.SubGroupIndexLabelKey] = subGroupIndexKey
			subGroupUniqueKey := genGroupUniqueKey(subGroupKeyInput, subGroupIndexKey)
			pod.Labels[leaderworkerset.SubGroupUniqueHashLabelKey] = subGroupUniqueKey
			if subEpKey, foundSubEpKey := pod.Annotations[leaderworkerset.SubGroupExclusiveKeyAnnotationKey]; foundSubEpKey {
				SetExclusiveAffinities(pod, subGroupUniqueKey, subEpKey, leaderworkerset.SubGroupUniqueHashLabelKey)
			}
		}
	}

	if p.SchedulerProvider != nil {
		err = p.SchedulerProvider.InjectPodGroupMetadata(pod)
		if err != nil {
			return err
		}
	}

	// injecting env vars if needed
	if acceleratorutils.PodRequestsTPUs(pod.Spec) {
		if err := acceleratorutils.AddTPUVariables(pod, podCount); err != nil {
			return err
		}
	}

	// Worker pods carry the leader's DNS address in an annotation stamped on the
	// worker statefulset template by the pod controller. Leader pods derive it
	// from their own DNS identity. Pods with neither carry an ordinal identity
	// and their labels name the leader.
	leaderAddress := pod.Annotations[leaderworkerset.LeaderAddressAnnotationKey]
	if leaderAddress == "" {
		groupIndex, foundGroupIndex := pod.Labels[leaderworkerset.GroupIndexLabelKey]
		if !foundGroupIndex {
			return fmt.Errorf("no group index label found for pod %s", pod.Name)
		}
		host := fmt.Sprintf("%s-%s", pod.Labels[leaderworkerset.SetNameLabelKey], groupIndex)
		if podutils.LeaderPod(*pod) && pod.Spec.Hostname != "" {
			host = pod.Spec.Hostname
		}
		leaderAddress = fmt.Sprintf("%s.%s.%s", host, pod.Spec.Subdomain, pod.Namespace)
	}
	if err := podutils.AddLWSVariables(pod, leaderAddress); err != nil {
		return err
	}

	return nil
}

func genGroupUniqueKey(ns string, podName string) string {
	return utils.Sha1Hash(fmt.Sprintf("%s/%s", ns, podName))
}

// hashDNSPrefixLength is how much of the group key goes into DNS facing names
// (leader hostname, per-replica service name). 8 hex characters is 32 bits, the
// same width as the pod template hash Deployments already rely on, and keeps
// the LeaderWorkerSet's share of the 63 character name budgets large.
const hashDNSPrefixLength = 8

// hashLeaderHostname is the host name of a hash identity leader: the lws name
// plus a prefix of the group key, the same shape as worker and ordinal names.
func hashLeaderHostname(lwsName, groupUniqueKey string) string {
	key := groupUniqueKey
	if len(key) > hashDNSPrefixLength {
		key = key[:hashDNSPrefixLength]
	}
	return fmt.Sprintf("%s-%s", lwsName, key)
}

// SetExclusiveAffinities set the pod affinity/anti-affinity
func SetExclusiveAffinities(pod *corev1.Pod, groupUniqueKey string, topologyKey string, podAffinityKey string) {
	if exclusiveAffinityApplied(*pod, topologyKey) {
		return
	}
	if pod.Spec.Affinity == nil {
		pod.Spec.Affinity = &corev1.Affinity{}
	}
	if pod.Spec.Affinity.PodAffinity == nil {
		pod.Spec.Affinity.PodAffinity = &corev1.PodAffinity{}
	}
	if pod.Spec.Affinity.PodAntiAffinity == nil {
		pod.Spec.Affinity.PodAntiAffinity = &corev1.PodAntiAffinity{}
	}

	// Pod affinity ensures the pods of this set land on the same topology domain.
	pod.Spec.Affinity.PodAffinity.RequiredDuringSchedulingIgnoredDuringExecution = append(pod.Spec.Affinity.PodAffinity.RequiredDuringSchedulingIgnoredDuringExecution,
		corev1.PodAffinityTerm{
			LabelSelector: &metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{
				{
					Key:      podAffinityKey,
					Operator: metav1.LabelSelectorOpIn,
					Values:   []string{groupUniqueKey},
				},
			}},
			TopologyKey: topologyKey,
		})
	// Pod anti-affinity ensures exclusively this set lands on the topology, preventing multiple sets per topology domain.
	pod.Spec.Affinity.PodAntiAffinity.RequiredDuringSchedulingIgnoredDuringExecution = append(pod.Spec.Affinity.PodAntiAffinity.RequiredDuringSchedulingIgnoredDuringExecution,
		corev1.PodAffinityTerm{
			LabelSelector: &metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{
				{
					Key:      podAffinityKey,
					Operator: metav1.LabelSelectorOpExists,
				},
				{
					Key:      podAffinityKey,
					Operator: metav1.LabelSelectorOpNotIn,
					Values:   []string{groupUniqueKey},
				},
			}},
			TopologyKey: topologyKey,
		})
}

// exclusiveAffinityApplied return true if the exclusive placement terms have been applied
func exclusiveAffinityApplied(pod corev1.Pod, topologyKey string) bool {
	if pod.Spec.Affinity == nil || pod.Spec.Affinity.PodAffinity == nil || pod.Spec.Affinity.PodAntiAffinity == nil {
		return false
	}
	hasAffinity := false
	hasAntiAffinity := false
	for _, podAffinityTerm := range pod.Spec.Affinity.PodAffinity.RequiredDuringSchedulingIgnoredDuringExecution {
		if podAffinityTerm.TopologyKey == topologyKey {
			hasAffinity = true
		}
	}
	for _, term := range pod.Spec.Affinity.PodAntiAffinity.RequiredDuringSchedulingIgnoredDuringExecution {
		if term.TopologyKey == topologyKey {
			hasAntiAffinity = true
		}
	}
	return hasAffinity && hasAntiAffinity
}

func getSubGroupIndex(podCount int, subGroupSize int, workerIndex int) string {
	if (podCount-1)%subGroupSize == 0 && podCount%subGroupSize != 0 {
		// Leader is considered as extra pod, it is part of the first group
		return fmt.Sprint((workerIndex - 1) / subGroupSize)
	}
	return fmt.Sprint(workerIndex / subGroupSize)
}

// SetShareAffinities sets the pod affinity to co-locate all pods of a group on the
// same topology domain, without anti-affinity between groups: unlike exclusive
// placement, multiple groups may share one topology domain.
func SetShareAffinities(pod *corev1.Pod, groupUniqueKey string, topologyKey string, podAffinityKey string) {
	if shareAffinityApplied(*pod, topologyKey) {
		return
	}
	if pod.Spec.Affinity == nil {
		pod.Spec.Affinity = &corev1.Affinity{}
	}
	if pod.Spec.Affinity.PodAffinity == nil {
		pod.Spec.Affinity.PodAffinity = &corev1.PodAffinity{}
	}
	// Pod affinity ensures the pods of this set land on the same topology domain.
	pod.Spec.Affinity.PodAffinity.RequiredDuringSchedulingIgnoredDuringExecution = append(pod.Spec.Affinity.PodAffinity.RequiredDuringSchedulingIgnoredDuringExecution,
		corev1.PodAffinityTerm{
			LabelSelector: &metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{
				{
					Key:      podAffinityKey,
					Operator: metav1.LabelSelectorOpIn,
					Values:   []string{groupUniqueKey},
				},
			}},
			TopologyKey: topologyKey,
		})

}

// shareAffinityApplied returns true if the pod already has a required pod affinity
// term for the given topology key. Unlike exclusiveAffinityApplied it does not
// require an anti-affinity term, since share placement sets none.
func shareAffinityApplied(pod corev1.Pod, topologyKey string) bool {
	if pod.Spec.Affinity == nil || pod.Spec.Affinity.PodAffinity == nil {
		return false
	}
	for _, term := range pod.Spec.Affinity.PodAffinity.RequiredDuringSchedulingIgnoredDuringExecution {
		if term.TopologyKey == topologyKey {
			return true
		}
	}
	return false
}

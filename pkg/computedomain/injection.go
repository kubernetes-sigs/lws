/*
Copyright 2026 The Kubernetes Authors.

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

package computedomain

import (
	"fmt"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	utilrand "k8s.io/apimachinery/pkg/util/rand"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/utils/ptr"

	disaggregatedsetv1 "sigs.k8s.io/lws/api/disaggregatedset/v1"
	leaderworkersetv1 "sigs.k8s.io/lws/api/leaderworkerset/v1"
	podutils "sigs.k8s.io/lws/pkg/utils/pod"
)

const (
	// ChannelClaimName is the reserved pod.spec.resourceClaims entry name
	// injected for LWS-managed ComputeDomain channels.
	ChannelClaimName = "lws-compute-domain-channel"

	// NvidiaGPUResourceName is the extended resource name used to auto-detect
	// NVIDIA GPU containers when compute-domain-containers is not specified.
	NvidiaGPUResourceName = corev1.ResourceName("nvidia.com/gpu")
)

// IsLWSAutoComputeDomainEnabled reports whether replica-scoped ComputeDomain
// management is enabled on lws.
func IsLWSAutoComputeDomainEnabled(lws *leaderworkersetv1.LeaderWorkerSet) bool {
	return lws != nil && lws.Annotations[leaderworkersetv1.AutoComputeDomainAnnotationKey] == leaderworkersetv1.AutoComputeDomainEnabled
}

// IsDisaggregatedSetSliceAutoComputeDomainEnabled reports whether slice-scoped
// ComputeDomain management is enabled at the DisaggregatedSet level.
func IsDisaggregatedSetSliceAutoComputeDomainEnabled(ds *disaggregatedsetv1.DisaggregatedSet) bool {
	return ds != nil && ds.Annotations[disaggregatedsetv1.AutoComputeDomainAnnotationKey] == leaderworkersetv1.AutoComputeDomainEnabled
}

// RoleUsesSliceComputeDomain reports whether role participates in ds's shared
// (slice, revision) ComputeDomain.
func RoleUsesSliceComputeDomain(ds *disaggregatedsetv1.DisaggregatedSet, role *disaggregatedsetv1.DisaggregatedRoleSpec) bool {
	if !IsDisaggregatedSetSliceAutoComputeDomainEnabled(ds) || role == nil {
		return false
	}
	return role.Annotations[disaggregatedsetv1.AutoComputeDomainAnnotationKey] != leaderworkersetv1.AutoComputeDomainDisabled
}

// IsRestartableSidecar reports whether c is a native sidecar initContainer
// (restartPolicy: Always).
func IsRestartableSidecar(c corev1.Container) bool {
	return c.RestartPolicy != nil && *c.RestartPolicy == corev1.ContainerRestartPolicyAlways
}

// ContainerRequestsNvidiaGPU reports whether c requests > 0 nvidia.com/gpu in
// limits or requests.
func ContainerRequestsNvidiaGPU(c corev1.Container) bool {
	if q, ok := c.Resources.Limits[NvidiaGPUResourceName]; ok && q.Sign() > 0 {
		return true
	}
	if q, ok := c.Resources.Requests[NvidiaGPUResourceName]; ok && q.Sign() > 0 {
		return true
	}
	return false
}

// ParseContainerNames parses a comma-separated container name annotation value,
// rejecting empty entries and duplicates.
func ParseContainerNames(raw string) ([]string, error) {
	parts := strings.Split(raw, ",")
	names := make([]string, 0, len(parts))
	seen := sets.New[string]()
	for _, part := range parts {
		name := strings.TrimSpace(part)
		if name == "" {
			return nil, fmt.Errorf("container name list %q contains an empty entry", raw)
		}
		if seen.Has(name) {
			return nil, fmt.Errorf("container name %q is listed more than once", name)
		}
		seen.Insert(name)
		names = append(names, name)
	}
	return names, nil
}

// EligibleContainerNames returns the set of all containers in podSpec that are
// eligible to mount a ComputeDomain channel (regular containers and restartable
// sidecar initContainers).
func EligibleContainerNames(podSpec *corev1.PodSpec) sets.Set[string] {
	eligible := sets.New[string]()
	if podSpec == nil {
		return eligible
	}
	for i := range podSpec.Containers {
		eligible.Insert(podSpec.Containers[i].Name)
	}
	for i := range podSpec.InitContainers {
		if IsRestartableSidecar(podSpec.InitContainers[i]) {
			eligible.Insert(podSpec.InitContainers[i].Name)
		}
	}
	return eligible
}

// TargetedContainerNames resolves which containers in podSpec should receive
// the ComputeDomain ResourceClaim, honoring compute-domain-containers when
// present and otherwise auto-detecting containers requesting nvidia.com/gpu.
func TargetedContainerNames(podSpec *corev1.PodSpec, annotations map[string]string) (sets.Set[string], error) {
	if podSpec == nil {
		return nil, nil
	}
	if raw, exists := annotations[leaderworkersetv1.ComputeDomainContainersAnnotationKey]; exists {
		names, err := ParseContainerNames(raw)
		if err != nil {
			return nil, err
		}
		eligible := EligibleContainerNames(podSpec)
		targeted := sets.New[string]()
		for _, name := range names {
			if !eligible.Has(name) {
				return nil, fmt.Errorf("container %q not found in containers or restartable initContainers", name)
			}
			targeted.Insert(name)
		}
		return targeted, nil
	}

	targeted := sets.New[string]()
	for i := range podSpec.Containers {
		if ContainerRequestsNvidiaGPU(podSpec.Containers[i]) {
			targeted.Insert(podSpec.Containers[i].Name)
		}
	}
	for i := range podSpec.InitContainers {
		if IsRestartableSidecar(podSpec.InitContainers[i]) && ContainerRequestsNvidiaGPU(podSpec.InitContainers[i]) {
			targeted.Insert(podSpec.InitContainers[i].Name)
		}
	}
	return targeted, nil
}

// TemplateHasTargetedContainers reports whether tpl has at least one container
// targeted for ComputeDomain claim injection.
func TemplateHasTargetedContainers(tpl *corev1.PodTemplateSpec) bool {
	if tpl == nil {
		return false
	}
	targeted, err := TargetedContainerNames(&tpl.Spec, tpl.Annotations)
	return err == nil && len(targeted) > 0
}

// LWSNeedsReplicaDomain reports whether lws has replica-scoped
// auto-compute-domain enabled and at least one template with a targeted
// container.
func LWSNeedsReplicaDomain(lws *leaderworkersetv1.LeaderWorkerSet) bool {
	if !IsLWSAutoComputeDomainEnabled(lws) {
		return false
	}
	return TemplateHasTargetedContainers(&lws.Spec.LeaderWorkerTemplate.WorkerTemplate) ||
		TemplateHasTargetedContainers(lws.Spec.LeaderWorkerTemplate.LeaderTemplate)
}

// DisaggregatedSetNeedsSliceDomain reports whether ds has slice-scoped
// auto-compute-domain enabled and at least one enrolled role with a targeted
// container.
func DisaggregatedSetNeedsSliceDomain(ds *disaggregatedsetv1.DisaggregatedSet) bool {
	if !IsDisaggregatedSetSliceAutoComputeDomainEnabled(ds) {
		return false
	}
	for i := range ds.Spec.Roles {
		role := &ds.Spec.Roles[i]
		if !RoleUsesSliceComputeDomain(ds, role) {
			continue
		}
		if TemplateHasTargetedContainers(&role.Spec.LeaderWorkerTemplate.WorkerTemplate) ||
			TemplateHasTargetedContainers(role.Spec.LeaderWorkerTemplate.LeaderTemplate) {
			return true
		}
	}
	return false
}

// InjectPodSpecClaims idempotently injects the ComputeDomain channel claim
// referencing cdName into podSpec and its targeted containers. If zero
// containers are targeted, podSpec is left untouched and false is returned.
// Slices are cloned before mutation so callers sharing shallow-copied specs
// cannot contaminate sibling objects.
func InjectPodSpecClaims(podSpec *corev1.PodSpec, annotations map[string]string, cdName string) (bool, error) {
	targeted, err := TargetedContainerNames(podSpec, annotations)
	if err != nil || len(targeted) == 0 {
		return false, err
	}

	podSpec.ResourceClaims = slices.Clone(podSpec.ResourceClaims)
	foundPodClaim := false
	for i := range podSpec.ResourceClaims {
		if podSpec.ResourceClaims[i].Name == ChannelClaimName {
			podSpec.ResourceClaims[i].ResourceClaimTemplateName = ptr.To(cdName)
			podSpec.ResourceClaims[i].ResourceClaimName = nil
			foundPodClaim = true
			break
		}
	}
	if !foundPodClaim {
		podSpec.ResourceClaims = append(podSpec.ResourceClaims, corev1.PodResourceClaim{
			Name:                      ChannelClaimName,
			ResourceClaimTemplateName: ptr.To(cdName),
		})
	}

	podSpec.Containers = slices.Clone(podSpec.Containers)
	for i := range podSpec.Containers {
		if targeted.Has(podSpec.Containers[i].Name) {
			ensureContainerClaim(&podSpec.Containers[i], ChannelClaimName)
		}
	}
	if len(podSpec.InitContainers) > 0 {
		podSpec.InitContainers = slices.Clone(podSpec.InitContainers)
		for i := range podSpec.InitContainers {
			if IsRestartableSidecar(podSpec.InitContainers[i]) && targeted.Has(podSpec.InitContainers[i].Name) {
				ensureContainerClaim(&podSpec.InitContainers[i], ChannelClaimName)
			}
		}
	}

	return true, nil
}

func ensureContainerClaim(c *corev1.Container, claimName string) {
	c.Resources.Claims = slices.Clone(c.Resources.Claims)
	for i := range c.Resources.Claims {
		if c.Resources.Claims[i].Name == claimName {
			return
		}
	}
	c.Resources.Claims = append(c.Resources.Claims, corev1.ResourceClaim{Name: claimName})
}

// InjectPodClaims resolves the replica ComputeDomain name for a leader or
// worker Pod admitted by PodWebhook and injects the corresponding DRA claims.
// On leader pods with replica-level ComputeDomain enabled, compute-domain-name
// is stamped even if the leader itself has no GPU containers so that worker
// pods in a CPU-leader / GPU-worker topology still join the replica domain.
func InjectPodClaims(pod *corev1.Pod) error {
	if podutils.LeaderPod(*pod) {
		prefix := pod.Annotations[leaderworkersetv1.ComputeDomainPrefixAnnotationKey]
		if prefix == "" {
			return nil
		}
		cdName := pod.Annotations[leaderworkersetv1.ComputeDomainNameAnnotationKey]
		if cdName == "" {
			groupIndex := pod.Labels[leaderworkersetv1.GroupIndexLabelKey]
			revisionKey := pod.Labels[leaderworkersetv1.RevisionKey]
			if groupIndex == "" || revisionKey == "" {
				return fmt.Errorf("leader pod %s/%s is missing group-index or template-revision-hash label for ComputeDomain naming", pod.Namespace, pod.Name)
			}
			hashIdentity := pod.Annotations[leaderworkersetv1.GroupIdentityAnnotationKey] == string(leaderworkersetv1.GroupIdentityHash)
			incarnation := ""
			if !hashIdentity {
				incarnation = utilrand.String(incarnationLength)
			}
			cdName = ReplicaComputeDomainName(prefix, groupIndex, revisionKey, incarnation, hashIdentity)
			if pod.Annotations == nil {
				pod.Annotations = make(map[string]string)
			}
			pod.Annotations[leaderworkersetv1.ComputeDomainNameAnnotationKey] = cdName
		}
		_, err := InjectPodSpecClaims(&pod.Spec, pod.Annotations, cdName)
		return err
	}

	cdName := pod.Annotations[leaderworkersetv1.ComputeDomainNameAnnotationKey]
	if cdName == "" {
		return nil
	}
	_, err := InjectPodSpecClaims(&pod.Spec, pod.Annotations, cdName)
	return err
}

// PodHasComputeDomainClaim reports whether pod carries an LWS-injected
// ComputeDomain ResourceClaim reference.
func PodHasComputeDomainClaim(pod *corev1.Pod) bool {
	if pod == nil {
		return false
	}
	for i := range pod.Spec.ResourceClaims {
		claim := &pod.Spec.ResourceClaims[i]
		if claim.Name == ChannelClaimName && claim.ResourceClaimTemplateName != nil && *claim.ResourceClaimTemplateName != "" {
			return true
		}
	}
	return false
}

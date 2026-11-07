/*
Copyright 2024.

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

package utils

import (
	"crypto/sha1"
	"encoding/hex"
	"os"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	quotav1 "k8s.io/apiserver/pkg/quota/v1"
	resourcehelper "k8s.io/component-helpers/resource"
	"k8s.io/utils/ptr"

	leaderworkerset "sigs.k8s.io/lws/api/leaderworkerset/v1"
)

const (
	defaultNamespace = "lws-system"
)

// Sha1Hash accepts an input string and returns the 40 character SHA1 hash digest of the input string.
func Sha1Hash(s string) string {
	h := sha1.New()
	h.Write([]byte(s))
	return hex.EncodeToString(h.Sum(nil))
}

func NonZeroValue(value int32) int32 {
	if value < 0 {
		return 0
	}
	return value
}

// SortByIndex returns an ascending list, the length of the list is always specified by the parameter.
func SortByIndex[T appsv1.StatefulSet | corev1.Pod | int](indexFunc func(T) (int, error), items []T, length int) []T {
	result := make([]T, length)

	for _, item := range items {
		index, err := indexFunc(item)
		if err != nil {
			// When no index found, continue, this can happen when
			// statefulset doesn't have the index.
			continue
		}

		if index >= length {
			continue
		}
		result[index] = item
	}

	return result
}

// serviceAccountNamespaceFile is the path to the namespace file in a pod.
var serviceAccountNamespaceFile = "/var/run/secrets/kubernetes.io/serviceaccount/namespace"

// GetOperatorNamespace will pick the namespace based on the serviceaccount
func GetOperatorNamespace() string {
	if data, err := os.ReadFile(serviceAccountNamespaceFile); err == nil {
		if ns := strings.TrimSpace(string(data)); len(ns) > 0 {
			return ns
		}
	}
	return defaultNamespace
}

// CalculatePGMinResources calculates the minimum resources needed for an entire PodGroup [1 Leader + (size-1) Worker pods]
func CalculatePGMinResources(lws *leaderworkerset.LeaderWorkerSet) corev1.ResourceList {
	// Calculate leader resources.
	leaderTemplate := lws.Spec.LeaderWorkerTemplate.LeaderTemplate
	if leaderTemplate == nil {
		// If no leader template is specified, use worker template.
		leaderTemplate = &lws.Spec.LeaderWorkerTemplate.WorkerTemplate
	}
	totalResources := resourcehelper.PodRequests(&corev1.Pod{Spec: leaderTemplate.Spec}, resourcehelper.PodResourcesOptions{})

	// Calculate and add worker resources for (size-1) workers.
	workerCount := ptr.Deref(lws.Spec.LeaderWorkerTemplate.Size, 1) - 1
	if workerCount > 0 {
		workerResources := resourcehelper.PodRequests(&corev1.Pod{Spec: lws.Spec.LeaderWorkerTemplate.WorkerTemplate.Spec}, resourcehelper.PodResourcesOptions{})
		for i := int32(0); i < workerCount; i++ {
			totalResources = quotav1.Add(totalResources, workerResources)
		}
	}

	return totalResources
}

// IsExclusivePlacement reports whether the placement type places groups, or
// subgroups, in exclusive topology domains. ExclusiveSlice scopes the
// exclusivity to one LeaderWorkerSet; ExclusiveTopology is exclusive against
// every workload.
func IsExclusivePlacement(t leaderworkerset.PlacementType) bool {
	return t == leaderworkerset.PlacementExclusiveSlice || t == leaderworkerset.PlacementExclusiveTopology
}

// ExclusivePlacement returns the topology key, the placement type (the
// exclusivity scope) and whether group level exclusive placement is enabled.
// An ExclusiveSlice or ExclusiveTopology spec.placementPolicy takes precedence
// over the exclusive-topology annotation. A None type falls back to the
// annotation, because the API server defaults the type to None whenever
// placementPolicy is set, including when only subGroup is. The annotation has
// no scope of its own, so it resolves as ExclusiveTopology, the scope it has
// always had.
func ExclusivePlacement(lws *leaderworkerset.LeaderWorkerSet) (string, leaderworkerset.PlacementType, bool) {
	if policy := lws.Spec.PlacementPolicy; policy != nil && IsExclusivePlacement(policy.Type) {
		return policy.Topology, policy.Type, true
	}
	annotation := lws.Annotations[leaderworkerset.ExclusiveKeyAnnotationKey]
	return annotation, leaderworkerset.PlacementExclusiveTopology, annotation != ""
}

// SubGroupExclusivePlacement returns the topology key, the placement type (the
// exclusivity scope) and whether subgroup level exclusive placement is
// enabled. An ExclusiveSlice or ExclusiveTopology spec.placementPolicy.subGroup
// takes precedence over the subgroup-exclusive-topology annotation, which
// resolves as ExclusiveTopology. Subgroup placement needs subgroups, so it is
// disabled without a subGroupPolicy.
func SubGroupExclusivePlacement(lws *leaderworkerset.LeaderWorkerSet) (string, leaderworkerset.PlacementType, bool) {
	if lws.Spec.LeaderWorkerTemplate.SubGroupPolicy == nil {
		return "", leaderworkerset.PlacementNone, false
	}
	if policy := lws.Spec.PlacementPolicy; policy != nil && policy.SubGroup != nil && IsExclusivePlacement(policy.SubGroup.Type) {
		return policy.SubGroup.Topology, policy.SubGroup.Type, true
	}
	annotation := lws.Annotations[leaderworkerset.SubGroupExclusiveKeyAnnotationKey]
	return annotation, leaderworkerset.PlacementExclusiveTopology, annotation != ""
}

// EffectivePlacementPolicy returns the placement that the field and the
// exclusive-topology annotations resolve to, or nil when neither level is
// exclusive. Both sources of the same placement give the same result, so it is
// what a revision records.
func EffectivePlacementPolicy(lws *leaderworkerset.LeaderWorkerSet) *leaderworkerset.PlacementPolicy {
	groupKey, groupScope, group := ExclusivePlacement(lws)
	subGroupKey, subGroupScope, subGroup := SubGroupExclusivePlacement(lws)
	if !group && !subGroup {
		return nil
	}
	policy := &leaderworkerset.PlacementPolicy{Type: leaderworkerset.PlacementNone}
	if group {
		policy.Type = groupScope
		policy.Topology = groupKey
	}
	if subGroup {
		policy.SubGroup = &leaderworkerset.SubGroupPlacementPolicy{
			Type:     subGroupScope,
			Topology: subGroupKey,
		}
	}
	return policy
}

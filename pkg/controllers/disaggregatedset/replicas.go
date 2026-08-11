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

package disaggregatedset

import (
	"fmt"
	"math"

	disaggregatedsetv1 "sigs.k8s.io/lws/api/disaggregatedset/v1"
	disaggregatedsetutils "sigs.k8s.io/lws/pkg/utils/disaggregatedset"
)

func validateParentReplicaTargets(ds *disaggregatedsetv1.DisaggregatedSet, scalers scalerMap) error {
	for i := range ds.Spec.Roles {
		role := &ds.Spec.Roles[i]
		var total int64
		for _, key := range disaggregatedsetutils.GetRoleKeysForParent(role) {
			total += int64(getTargetReplicas(ds, key, scalers, 0))
		}
		if total > math.MaxInt32 {
			return fmt.Errorf("replica targets for role %s sum to %d, exceeding the maximum LWS replica count %d", role.Name, total, math.MaxInt32)
		}
	}
	return nil
}

type scalerMap map[disaggregatedsetutils.RoleKey]*disaggregatedsetv1.DisaggregatedSetRoleScaler

// getTargetReplicas resolves a logical role target. current is returned while
// an expected External scaler is transiently unavailable.
func getTargetReplicas(ds *disaggregatedsetv1.DisaggregatedSet, key disaggregatedsetutils.RoleKey, scalers scalerMap, current int) int {
	role := disaggregatedsetutils.GetRoleSpec(ds, key.Role)
	if role == nil {
		return 0
	}

	if key.SubRole != "" {
		subRole := disaggregatedsetutils.GetSubRoleSpec(ds, key)
		if subRole == nil {
			return 0
		}
		if subRole.Scaling != nil && subRole.Scaling.Mode == disaggregatedsetv1.RoleScalingExternal {
			if scaler := scalers[key]; scaler != nil {
				return int(scaler.Spec.Replicas)
			}
			return current
		}
		if subRole.Replicas == nil {
			return 1
		}
		return int(*subRole.Replicas)
	}

	if role.Scaling != nil && role.Scaling.Mode == disaggregatedsetv1.RoleScalingExternal {
		if scaler := scalers[key]; scaler != nil {
			return int(scaler.Spec.Replicas)
		}
		return current
	}
	if role.Spec.Replicas == nil {
		return 1
	}
	return int(*role.Spec.Replicas)
}

func getParentTargetReplicas(ds *disaggregatedsetv1.DisaggregatedSet, roleName string, scalers scalerMap, currentByKey map[disaggregatedsetutils.RoleKey]int) int {
	role := disaggregatedsetutils.GetRoleSpec(ds, roleName)
	if role == nil {
		return 0
	}
	keys := disaggregatedsetutils.GetRoleKeysForParent(role)
	total := 0
	missingExternal := false
	for _, key := range keys {
		if isExternal(ds, key) && scalers[key] == nil {
			missingExternal = true
		}
		total += getTargetReplicas(ds, key, scalers, currentByKey[key])
	}
	// If a child scaler is transiently unavailable (including a name conflict),
	// the per-child fallback may be incomplete while labels are still converging.
	// Never let that uncertainty drain existing aggregate parent capacity.
	if missingExternal {
		total = max(total, currentByKey[disaggregatedsetutils.RoleKey{Role: roleName}])
	}
	return total
}

func isExternal(ds *disaggregatedsetv1.DisaggregatedSet, key disaggregatedsetutils.RoleKey) bool {
	if key.SubRole != "" {
		subRole := disaggregatedsetutils.GetSubRoleSpec(ds, key)
		return subRole != nil && subRole.Scaling != nil && subRole.Scaling.Mode == disaggregatedsetv1.RoleScalingExternal
	}
	role := disaggregatedsetutils.GetRoleSpec(ds, key.Role)
	return role != nil && role.Scaling != nil && role.Scaling.Mode == disaggregatedsetv1.RoleScalingExternal
}

func isParentExternal(ds *disaggregatedsetv1.DisaggregatedSet, roleName string) bool {
	role := disaggregatedsetutils.GetRoleSpec(ds, roleName)
	if role == nil {
		return false
	}
	for _, key := range disaggregatedsetutils.GetRoleKeysForParent(role) {
		if isExternal(ds, key) {
			return true
		}
	}
	return false
}

func desiredSubRoleReplicas(ds *disaggregatedsetv1.DisaggregatedSet, roleName string, scalers scalerMap, current map[disaggregatedsetutils.RoleKey]int) map[string]int {
	role := disaggregatedsetutils.GetRoleSpec(ds, roleName)
	if role == nil || len(role.SubRoles) == 0 {
		return nil
	}
	result := make(map[string]int, len(role.SubRoles))
	for _, key := range disaggregatedsetutils.GetRoleKeysForParent(role) {
		result[key.SubRole] = getTargetReplicas(ds, key, scalers, current[key])
	}
	return result
}

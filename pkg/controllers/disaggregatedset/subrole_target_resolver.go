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
)

// RoleKey identifies either a parent role or one of its sub-roles. Keeping the
// two components separate avoids collisions such as "a-b/c" and "a/b-c".
type RoleKey struct {
	Role    string
	SubRole string
}

func (k RoleKey) String() string {
	if k.SubRole == "" {
		return k.Role
	}
	return k.Role + "/" + k.SubRole
}

type ScalerMap map[RoleKey]*disaggregatedsetv1.DisaggregatedSetRoleScaler

// SubRoleTargetResolver resolves the logical replica targets that share each
// physical parent LeaderWorkerSet.
type SubRoleTargetResolver struct{}

func NewSubRoleTargetResolver() *SubRoleTargetResolver {
	return &SubRoleTargetResolver{}
}

func (r *SubRoleTargetResolver) Role(ds *disaggregatedsetv1.DisaggregatedSet, name string) *disaggregatedsetv1.DisaggregatedRoleSpec {
	for i := range ds.Spec.Roles {
		if ds.Spec.Roles[i].Name == name {
			return &ds.Spec.Roles[i]
		}
	}
	return nil
}

func (r *SubRoleTargetResolver) SubRole(ds *disaggregatedsetv1.DisaggregatedSet, key RoleKey) *disaggregatedsetv1.DisaggregatedSubRoleSpec {
	role := r.Role(ds, key.Role)
	if role == nil || key.SubRole == "" {
		return nil
	}
	for i := range role.SubRoles {
		if role.SubRoles[i].Name == key.SubRole {
			return &role.SubRoles[i]
		}
	}
	return nil
}

// Keys returns the independently scalable identities in API order. A role
// with sub-roles expands to its children; an ordinary role remains one key.
func (r *SubRoleTargetResolver) Keys(ds *disaggregatedsetv1.DisaggregatedSet) []RoleKey {
	var keys []RoleKey
	for i := range ds.Spec.Roles {
		keys = append(keys, r.KeysForRole(&ds.Spec.Roles[i])...)
	}
	return keys
}

func (r *SubRoleTargetResolver) KeysForRole(role *disaggregatedsetv1.DisaggregatedRoleSpec) []RoleKey {
	if role == nil {
		return nil
	}
	if len(role.SubRoles) == 0 {
		return []RoleKey{{Role: role.Name}}
	}
	keys := make([]RoleKey, 0, len(role.SubRoles))
	for _, subRole := range role.SubRoles {
		keys = append(keys, RoleKey{Role: role.Name, SubRole: subRole.Name})
	}
	return keys
}

func (r *SubRoleTargetResolver) IsExternal(ds *disaggregatedsetv1.DisaggregatedSet, key RoleKey) bool {
	if key.SubRole != "" {
		subRole := r.SubRole(ds, key)
		return subRole != nil && subRole.Scaling != nil && subRole.Scaling.Mode == disaggregatedsetv1.RoleScalingExternal
	}
	role := r.Role(ds, key.Role)
	return role != nil && role.Scaling != nil && role.Scaling.Mode == disaggregatedsetv1.RoleScalingExternal
}

// Resolve returns one logical target. If an expected External scaler is
// temporarily unavailable, current is retained so a name conflict or cache lag
// cannot unexpectedly drain capacity.
func (r *SubRoleTargetResolver) Resolve(ds *disaggregatedsetv1.DisaggregatedSet, key RoleKey, scalers ScalerMap, current int) int {
	if key.SubRole != "" {
		subRole := r.SubRole(ds, key)
		if subRole == nil {
			return 0
		}
		if r.IsExternal(ds, key) {
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

	role := r.Role(ds, key.Role)
	if role == nil {
		return 0
	}
	if r.IsExternal(ds, key) {
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

func (r *SubRoleTargetResolver) SubRoleTargets(ds *disaggregatedsetv1.DisaggregatedSet, roleName string, scalers ScalerMap, current map[RoleKey]int) map[string]int {
	role := r.Role(ds, roleName)
	if role == nil || len(role.SubRoles) == 0 {
		return nil
	}
	targets := make(map[string]int, len(role.SubRoles))
	for _, key := range r.KeysForRole(role) {
		targets[key.SubRole] = r.Resolve(ds, key, scalers, current[key])
	}
	return targets
}

func (r *SubRoleTargetResolver) ParentTarget(ds *disaggregatedsetv1.DisaggregatedSet, roleName string, scalers ScalerMap, current map[RoleKey]int) int {
	role := r.Role(ds, roleName)
	if role == nil {
		return 0
	}
	keys := r.KeysForRole(role)
	total := 0
	missingExternal := false
	for _, key := range keys {
		if r.IsExternal(ds, key) && scalers[key] == nil {
			missingExternal = true
		}
		total += r.Resolve(ds, key, scalers, current[key])
	}
	if missingExternal {
		total = max(total, current[RoleKey{Role: roleName}])
	}
	return total
}

func (r *SubRoleTargetResolver) ParentHasExternal(ds *disaggregatedsetv1.DisaggregatedSet, roleName string) bool {
	role := r.Role(ds, roleName)
	for _, key := range r.KeysForRole(role) {
		if r.IsExternal(ds, key) {
			return true
		}
	}
	return false
}

func (r *SubRoleTargetResolver) ValidateParentTargets(ds *disaggregatedsetv1.DisaggregatedSet, scalers ScalerMap) error {
	for i := range ds.Spec.Roles {
		role := &ds.Spec.Roles[i]
		var total int64
		for _, key := range r.KeysForRole(role) {
			total += int64(r.Resolve(ds, key, scalers, 0))
		}
		if total > math.MaxInt32 {
			return fmt.Errorf("replica targets for role %s sum to %d, exceeding the maximum LWS replica count %d", role.Name, total, math.MaxInt32)
		}
	}
	return nil
}

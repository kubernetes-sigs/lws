/*
Copyright 2025 The Kubernetes Authors.

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
	disaggregatedsetv1 "sigs.k8s.io/lws/api/disaggregatedset/v1"
)

// RoleTarget is the resolved desired replica count for one role in one slice.
// Hold means no target could be determined (an External role whose scaler is
// unavailable, e.g. a name conflict prevented adoption) and the current count
// must be kept.
type RoleTarget struct {
	Replicas int
	Hold     bool
}

// SliceTargets maps role name to its resolved target for a single slice. It is
// computed once per reconcile, before the per-slice loop, so every slice works
// from a consistent snapshot and the executor never consults scalers directly.
type SliceTargets map[string]RoleTarget

// Resolve returns the role's target, or current when the role has no resolved
// target (unknown role or Hold).
func (t SliceTargets) Resolve(role string, current int) int {
	rt, ok := t[role]
	if !ok || rt.Hold {
		return current
	}
	return rt.Replicas
}

// resolveTargets computes the per-slice replica target for every role in the
// spec. Static roles use the inline per-slice spec.replicas. External roles
// treat scaler.spec.replicas as the total across all slices and split it with
// distribute; a missing scaler resolves to Hold.
func resolveTargets(
	ds *disaggregatedsetv1.DisaggregatedSet,
	sliceCount int,
	scalers map[string]*disaggregatedsetv1.DisaggregatedSetRoleScaler,
) []SliceTargets {
	targets := make([]SliceTargets, sliceCount)
	for i := range targets {
		targets[i] = make(SliceTargets, len(ds.Spec.Roles))
	}

	for _, role := range ds.Spec.Roles {
		if role.Scaling != nil && role.Scaling.Mode == disaggregatedsetv1.RoleScalingExternal {
			scaler := scalers[role.Name]
			if scaler == nil {
				for i := range targets {
					targets[i][role.Name] = RoleTarget{Hold: true}
				}
				continue
			}
			total := int(scaler.Spec.Replicas)
			for i := range targets {
				targets[i][role.Name] = RoleTarget{Replicas: distribute(total, sliceCount, i)}
			}
			continue
		}

		perSlice := 1
		if role.Spec.Replicas != nil {
			perSlice = int(*role.Spec.Replicas)
		}
		for i := range targets {
			targets[i][role.Name] = RoleTarget{Replicas: perSlice}
		}
	}
	return targets
}

// distribute splits an aggregate replica total across slices: every slice gets
// floor(total/slices) and the remainder goes to the lowest-indexed slices, one
// each. The result is a pure function of (total, slices, index), so
// concurrently reconciling slices always compute a consistent split, targets
// differ by at most one across slices, and raising the total never lowers any
// slice's share (or vice versa). Keeping the remainder on low indices means
// slice scale-down, which removes the highest slices first, disturbs the
// smallest shares.
func distribute(total, slices, index int) int {
	if slices <= 0 || total <= 0 {
		return 0
	}
	base := total / slices
	if index < total%slices {
		return base + 1
	}
	return base
}

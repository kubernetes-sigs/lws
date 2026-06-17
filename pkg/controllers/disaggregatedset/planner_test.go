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
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// makeRoles creates role maps from positional slices for compact test definitions.
func makeRoles(names []string, values []int) map[string]int {
	m := make(map[string]int, len(names))
	for i, name := range names {
		m[name] = values[i]
	}
	return m
}

func makeConfig(names []string, surge, unavail []int) map[string]RollingUpdateConfig {
	m := make(map[string]RollingUpdateConfig, len(names))
	for i, name := range names {
		m[name] = RollingUpdateConfig{MaxSurge: surge[i], MaxUnavailable: unavail[i]}
	}
	return m
}

func defaultConfig(names []string) map[string]RollingUpdateConfig {
	surge := make([]int, len(names))
	unavail := make([]int, len(names))
	for i := range names {
		surge[i] = 1
	}
	return makeConfig(names, surge, unavail)
}

func completes(steps []UpdateStep, names []string, target map[string]int) bool {
	if len(steps) == 0 {
		return false
	}
	last := steps[len(steps)-1]
	for _, name := range names {
		if last.Past[name].Replicas != 0 {
			return false
		}
		if last.New[name].Replicas < target[name] {
			return false
		}
	}
	return true
}

func totalPerRole(s UpdateStep, role string) int {
	return s.Past[role].Replicas + s.New[role].Replicas
}

// =============================================================================
// Minimal Unit Tests
// =============================================================================

func TestComputeMinimalUnit(t *testing.T) {
	tests := []struct {
		name    string
		targets map[string]int
		wantNum int
		wantDen int
	}{
		{"symmetric_8_8", map[string]int{"p": 8, "d": 8}, 1, 8},
		{"asymmetric_8_10", map[string]int{"p": 8, "d": 10}, 1, 8},
		{"extreme_2_100", map[string]int{"p": 2, "d": 100}, 1, 2},
		{"single_replica", map[string]int{"p": 1, "d": 10}, 1, 1},
		{"three_roles", map[string]int{"p": 4, "d": 8, "c": 16}, 1, 4},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			unit := revisionMinimalUnit(tc.targets)
			assert.Equal(t, tc.wantNum, unit.num)
			assert.Equal(t, tc.wantDen, unit.den)
		})
	}
}

// =============================================================================
// Rollout Completion Tests
// =============================================================================

func TestRolloutCompletes(t *testing.T) {
	roles2 := []string{"prefill", "decode"}

	tests := []struct {
		name    string
		names   []string
		initial []int
		target  []int
		surge   []int
		unavail []int
	}{
		// 2-role symmetric
		{"2role_1_1", roles2, []int{1, 1}, []int{1, 1}, []int{1, 1}, []int{0, 0}},
		{"2role_4_4", roles2, []int{4, 4}, []int{4, 4}, []int{1, 1}, []int{0, 0}},
		{"2role_8_10", roles2, []int{8, 10}, []int{8, 10}, []int{1, 1}, []int{0, 0}},
		{"2role_10_10", roles2, []int{10, 10}, []int{10, 10}, []int{1, 1}, []int{0, 0}},

		// Asymmetric
		{"asymmetric_2_100", roles2, []int{2, 100}, []int{2, 100}, []int{1, 1}, []int{0, 0}},
		{"asymmetric_6_2", roles2, []int{6, 2}, []int{6, 2}, []int{1, 1}, []int{0, 0}},

		// Scale up/down
		{"scale_up", roles2, []int{2, 2}, []int{6, 6}, []int{1, 1}, []int{0, 0}},
		{"scale_down", roles2, []int{6, 6}, []int{2, 2}, []int{1, 1}, []int{0, 0}},
		{"fresh_deploy", roles2, []int{0, 0}, []int{4, 4}, []int{1, 1}, []int{0, 0}},

		// Surge > 1
		{"surge_2", roles2, []int{8, 10}, []int{8, 10}, []int{2, 2}, []int{0, 0}},
		{"surge_3", roles2, []int{10, 10}, []int{10, 10}, []int{3, 3}, []int{0, 0}},

		// Unavailable
		{"unavail_1", roles2, []int{4, 4}, []int{4, 4}, []int{0, 0}, []int{1, 1}},
		{"unavail_2", roles2, []int{6, 6}, []int{6, 6}, []int{0, 0}, []int{2, 2}},

		// Mixed
		{"mixed_surge_unavail", roles2, []int{4, 4}, []int{4, 4}, []int{1, 1}, []int{1, 1}},

		// 3 roles
		{"3role_sym", []string{"a", "b", "c"}, []int{3, 3, 3}, []int{3, 3, 3}, []int{1, 1, 1}, []int{0, 0, 0}},
		{"3role_asym", []string{"a", "b", "c"}, []int{8, 4, 3}, []int{8, 4, 3}, []int{1, 1, 1}, []int{0, 0, 0}},
		{"3role_fresh", []string{"a", "b", "c"}, []int{0, 0, 0}, []int{4, 4, 4}, []int{1, 1, 1}, []int{0, 0, 0}},

		// 4 roles
		{"4role_sym", []string{"a", "b", "c", "d"}, []int{4, 4, 4, 4}, []int{4, 4, 4, 4}, []int{1, 1, 1, 1}, []int{0, 0, 0, 0}},

		// Role addition/removal
		{"add_role", []string{"a", "b", "c"}, []int{4, 4, 0}, []int{4, 4, 4}, []int{1, 1, 1}, []int{0, 0, 0}},
		{"remove_role", []string{"a", "b", "c"}, []int{4, 4, 4}, []int{4, 4, 0}, []int{1, 1, 1}, []int{0, 0, 0}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			initial := makeRoles(tc.names, tc.initial)
			target := makeRoles(tc.names, tc.target)
			cfg := makeConfig(tc.names, tc.surge, tc.unavail)
			steps := ComputeAllSteps(tc.names, initial, target, cfg)
			require.True(t, completes(steps, tc.names, target), "rollout should complete: got %d steps", len(steps))
		})
	}
}

// =============================================================================
// Capacity Invariant Tests
// =============================================================================

func TestCapacityNeverExceedsTarget(t *testing.T) {
	roles := []string{"prefill", "decode"}
	tests := []struct {
		name    string
		initial []int
		target  []int
	}{
		{"symmetric_8_8", []int{8, 8}, []int{8, 8}},
		{"asymmetric_8_10", []int{8, 10}, []int{8, 10}},
		{"extreme_2_100", []int{2, 100}, []int{2, 100}},
		{"large_1000_1000", []int{1000, 1000}, []int{1000, 1000}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			initial := makeRoles(roles, tc.initial)
			target := makeRoles(roles, tc.target)
			cfg := defaultConfig(roles)
			steps := ComputeAllSteps(roles, initial, target, cfg)

			for i, s := range steps {
				for _, role := range roles {
					total := totalPerRole(s, role)
					assert.LessOrEqual(t, total, target[role]+cfg[role].MaxSurge,
						"step %d, role %s: total %d exceeds target %d + surge %d",
						i, role, total, target[role], cfg[role].MaxSurge)
				}
			}
		})
	}
}

// =============================================================================
// Sync Point Tests
// =============================================================================

func TestSyncPointEnforcement(t *testing.T) {
	// With 8 prefill and 3 decode-long, minimalUnit = 1/3 = 33.3%.
	// Prefill must not cross a sync boundary before decode-long reaches it.
	roles := []string{"P", "DL"}
	initial := makeRoles(roles, []int{8, 3})
	target := makeRoles(roles, []int{8, 3})
	cfg := defaultConfig(roles)
	steps := ComputeAllSteps(roles, initial, target, cfg)

	require.True(t, completes(steps, roles, target))

	for i, s := range steps {
		pState := s.New["P"]
		dlState := s.New["DL"]
		// P should never be more than 1 sync point ahead of DL.
		assert.LessOrEqual(t, pState.CrossRoleStep, dlState.CrossRoleStep+1,
			"step %d: P at sync %d but DL at sync %d", i, pState.CrossRoleStep, dlState.CrossRoleStep)
	}
}

func TestSyncPointStructure_3Roles(t *testing.T) {
	// 8P, 4D, 3DL: minimalUnit = 1/3, 3 sync points.
	roles := []string{"P", "D", "DL"}
	initial := makeRoles(roles, []int{8, 4, 3})
	target := makeRoles(roles, []int{8, 4, 3})
	cfg := defaultConfig(roles)
	steps := ComputeAllSteps(roles, initial, target, cfg)

	require.True(t, completes(steps, roles, target))

	// Verify that the last step has all roles at sync point 3 (= 1/minimalUnit).
	last := steps[len(steps)-1]
	for _, role := range roles {
		assert.Equal(t, 3, last.New[role].CrossRoleStep,
			"role %s should end at sync point 3", role)
	}
}

// =============================================================================
// Per-Role Granularity Tests
// =============================================================================

func TestPerRoleGranularity_DecodeStepping(t *testing.T) {
	// With 2P, 10D: minimalUnit = 1/2 = 50%. Decode advances 1 at a time.
	roles := []string{"P", "D"}
	initial := makeRoles(roles, []int{2, 10})
	target := makeRoles(roles, []int{2, 10})
	cfg := defaultConfig(roles)
	steps := ComputeAllSteps(roles, initial, target, cfg)

	for i := 1; i < len(steps); i++ {
		prevD := steps[i-1].New["D"].Replicas
		currD := steps[i].New["D"].Replicas
		delta := currD - prevD
		assert.LessOrEqual(t, delta, 1,
			"step %d: decode jumped by %d (from %d to %d)", i, delta, prevD, currD)
	}
}

// =============================================================================
// ComputeNextStep Tests
// =============================================================================

func TestComputeNextStep_ReturnsNilWhenDone(t *testing.T) {
	roles := []string{"p", "d"}
	cfg := defaultConfig(roles)

	tests := []struct {
		name       string
		currentOld map[string]int
		currentNew map[string]int
		targetNew  map[string]int
	}{
		{
			"exactly_at_target",
			makeRoles(roles, []int{0, 0}),
			makeRoles(roles, []int{3, 6}),
			makeRoles(roles, []int{3, 6}),
		},
		{
			"new_exceeds_target",
			makeRoles(roles, []int{0, 0}),
			makeRoles(roles, []int{4, 7}),
			makeRoles(roles, []int{3, 6}),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			initialOld := makeRoles(roles, []int{3, 6})
			result := ComputeNextStep(roles, initialOld, tc.currentOld, tc.currentNew, tc.targetNew, cfg)
			assert.Nil(t, result, "should return nil when rollout is complete")
		})
	}
}

func TestComputeNextStep_FreshStart(t *testing.T) {
	roles := []string{"p", "d"}
	cfg := defaultConfig(roles)
	initialOld := makeRoles(roles, []int{4, 4})
	currentOld := makeRoles(roles, []int{4, 4})
	currentNew := makeRoles(roles, []int{0, 0})
	targetNew := makeRoles(roles, []int{4, 4})

	result := ComputeNextStep(roles, initialOld, currentOld, currentNew, targetNew, cfg)
	require.NotNil(t, result)
	assert.Greater(t, result.New["p"].Replicas, 0, "should create new p replicas")
	assert.Greater(t, result.New["d"].Replicas, 0, "should create new d replicas")
}

// =============================================================================
// Surge Constraint Tests
// =============================================================================

func TestSurgeConstraint(t *testing.T) {
	roles := []string{"p", "d"}
	tests := []struct {
		name    string
		initial []int
		target  []int
		surge   []int
	}{
		{"symmetric_4_4_s1", []int{4, 4}, []int{4, 4}, []int{1, 1}},
		{"symmetric_10_10_s3", []int{10, 10}, []int{10, 10}, []int{3, 3}},
		{"asymmetric_6_2_s2", []int{6, 2}, []int{6, 2}, []int{2, 2}},
		{"scale_down", []int{10, 10}, []int{4, 4}, []int{2, 2}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			initial := makeRoles(roles, tc.initial)
			target := makeRoles(roles, tc.target)
			cfg := makeConfig(roles, tc.surge, []int{0, 0})
			steps := ComputeAllSteps(roles, initial, target, cfg)
			require.True(t, completes(steps, roles, target))

			for stepIdx, s := range steps {
				for _, role := range roles {
					newR := s.New[role].Replicas
					if newR == 0 {
						continue
					}
					maxAllowed := max(initial[role], target[role]) + cfg[role].MaxSurge
					actual := totalPerRole(s, role)
					assert.LessOrEqual(t, actual, maxAllowed,
						"step %d, role %s: total %d exceeds max %d",
						stepIdx, role, actual, maxAllowed)
				}
			}
		})
	}
}

// =============================================================================
// Default Config Tests
// =============================================================================

func TestDefaultRollingUpdateConfig(t *testing.T) {
	for _, numRoles := range []int{2, 3, 4, 5} {
		t.Run(fmt.Sprintf("%d_roles", numRoles), func(t *testing.T) {
			cfg := DefaultRollingUpdateConfig(numRoles)
			assert.Equal(t, numRoles, len(cfg))
			for i := 0; i < numRoles; i++ {
				assert.Equal(t, 1, cfg[i].MaxSurge)
				assert.Equal(t, 0, cfg[i].MaxUnavailable)
			}
		})
	}
}

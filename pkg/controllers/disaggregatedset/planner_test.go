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

// TestSideSize verifies the step-count helper: number of steps a side has =
// smallest non-zero role replica count (so the slowest role can express each
// step boundary). 0 indicates an empty side.
func TestSideSize(t *testing.T) {
	tests := []struct {
		name     string
		replicas map[string]int
		want     int
	}{
		{"symmetric_8_8", map[string]int{"p": 8, "d": 8}, 8},
		{"asymmetric_8_10", map[string]int{"p": 8, "d": 10}, 8},
		{"extreme_2_100", map[string]int{"p": 2, "d": 100}, 2},
		{"single_replica", map[string]int{"p": 1, "d": 10}, 1},
		{"three_roles", map[string]int{"p": 4, "d": 8, "c": 16}, 4},
		{"empty_side", map[string]int{"p": 0, "d": 0}, 0},
		{"one_zero_role", map[string]int{"p": 0, "d": 4}, 4},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, sideSize(tc.replicas))
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

// TestSyncCoordination_NewSide checks the in-side coordination invariant:
// for the NEW side, no role's progress fraction (current/target) is more than
// one minimalUnit ahead of another's. This is what the OLD test asserted
// using sync-window indices; the invariant is the same, expressed in
// fractions directly.
func TestSyncCoordination_NewSide(t *testing.T) {
	// 8 prefill, 3 decode → minimalUnit = 1/3 (~33%).
	roles := []string{"P", "DL"}
	initial := makeRoles(roles, []int{8, 3})
	target := makeRoles(roles, []int{8, 3})
	cfg := defaultConfig(roles)
	steps := ComputeAllSteps(roles, initial, target, cfg)
	require.True(t, completes(steps, roles, target))

	const epsilon = 1e-9
	minU := 1.0 / 3.0
	for i, s := range steps {
		pFrac := float64(s.New["P"].Replicas) / float64(target["P"])
		dlFrac := float64(s.New["DL"].Replicas) / float64(target["DL"])
		diff := pFrac - dlFrac
		if diff < 0 {
			diff = -diff
		}
		assert.LessOrEqual(t, diff, minU+epsilon,
			"step %d: P frac %.3f vs DL frac %.3f (diff %.3f > minU %.3f)",
			i, pFrac, dlFrac, diff, minU)
	}
}

// TestRolloutEndsAtTarget verifies the 3-role rollout's final step has all
// roles at their target replica counts (full progress).
func TestRolloutEndsAtTarget(t *testing.T) {
	roles := []string{"P", "D", "DL"}
	initial := makeRoles(roles, []int{8, 4, 3})
	target := makeRoles(roles, []int{8, 4, 3})
	cfg := defaultConfig(roles)
	steps := ComputeAllSteps(roles, initial, target, cfg)

	require.True(t, completes(steps, roles, target))
	last := steps[len(steps)-1]
	for _, role := range roles {
		assert.Equal(t, target[role], last.New[role].Replicas,
			"role %s should end at target %d", role, target[role])
		assert.Equal(t, 0, last.Past[role].Replicas,
			"role %s old side should end fully drained", role)
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
// Step-Size Acceleration Tests
// =============================================================================

// TestSurgeUnavailAcceleration verifies that higher surge/unavail budgets
// produce fewer sub-steps. The planner should use these budgets as per-step
// accelerators, not just guardrails — small surge means small step, big surge
// means big step (up to the next sync boundary).
func TestSurgeUnavailAcceleration(t *testing.T) {
	roles := []string{"p", "d"}
	initial := makeRoles(roles, []int{20, 4})
	target := makeRoles(roles, []int{20, 4})

	// Baseline: surge=1 unavail=0 → ~1 replica per role per sub-step.
	smallCfg := makeConfig(roles, []int{1, 1}, []int{0, 0})
	smallSteps := ComputeAllSteps(roles, initial, target, smallCfg)

	// Boosted: surge=3 unavail=2 → bigger sub-steps within the same sync windows.
	bigCfg := makeConfig(roles, []int{3, 3}, []int{2, 2})
	bigSteps := ComputeAllSteps(roles, initial, target, bigCfg)

	require.True(t, completes(smallSteps, roles, target))
	require.True(t, completes(bigSteps, roles, target))

	// Sanity: both reach the same final state.
	assert.Equal(t, target["p"], smallSteps[len(smallSteps)-1].New["p"].Replicas)
	assert.Equal(t, target["p"], bigSteps[len(bigSteps)-1].New["p"].Replicas)

	// Acceleration: bigger budgets should produce strictly fewer steps.
	assert.Less(t, len(bigSteps), len(smallSteps),
		"surge=3/unavail=2 should produce fewer sub-steps than surge=1/unavail=0 (got %d vs %d)",
		len(bigSteps), len(smallSteps))

	// Surge envelope still respected at every step for both configs.
	for _, role := range roles {
		bigCeil := max(initial[role], target[role]) + bigCfg[role].MaxSurge
		for i, s := range bigSteps {
			tot := totalPerRole(s, role)
			assert.LessOrEqual(t, tot, bigCeil,
				"big config step %d role %s: total %d > ceiling %d", i, role, tot, bigCeil)
		}
	}
}

// =============================================================================
// Two-minimalUnit Tests
// =============================================================================

// TestTwoMinimalUnits_IndependentProgression verifies that with different
// old/new step counts, each side advances at its own rhythm. A=4P/4D →
// C=12P/3D gives oldSize=4 (drain in 4 steps) and newSize=3 (scale up in 3).
func TestTwoMinimalUnits_IndependentProgression(t *testing.T) {
	roles := []string{"prefill", "decode"}
	initial := map[string]int{"prefill": 4, "decode": 4}
	target := map[string]int{"prefill": 12, "decode": 3}
	cfg := makeConfig(roles, []int{2, 2}, []int{2, 2})

	assert.Equal(t, 4, sideSize(initial), "old side has 4 steps (smallest old role)")
	assert.Equal(t, 3, sideSize(target), "new side has 3 steps (smallest new role)")

	steps := ComputeAllSteps(roles, initial, target, cfg)
	require.True(t, completes(steps, roles, target))

	// Verify ceiling never breached.
	for i, s := range steps {
		for _, role := range roles {
			ceil := max(initial[role], target[role]) + cfg[role].MaxSurge
			total := totalPerRole(s, role)
			assert.LessOrEqual(t, total, ceil, "step %d role %s exceeds ceiling", i, role)
		}
	}
}

// TestTwoMinimalUnits_TinyRevisionAbsorbed verifies that summing the old side
// dilutes a tiny revision: a 1P/1D revision mixed with a 20P/4D one yields
// an old-side step count of min(21,5) = 5 (not 1). The drain rhythm is
// governed by the sum, so the tiny revision doesn't poison coordination.
func TestTwoMinimalUnits_TinyRevisionAbsorbed(t *testing.T) {
	combined := map[string]int{"prefill": 21, "decode": 5}
	assert.Equal(t, 5, sideSize(combined),
		"summed old side should have 5 steps, not 1 from tiny B alone")

	// Sanity: tiny B alone would give 1 (the poison case).
	tinyAlone := map[string]int{"prefill": 1, "decode": 1}
	assert.Equal(t, 1, sideSize(tinyAlone), "tiny rev alone has 1 step")
}

// =============================================================================
// Golden / Characterization Tests
// =============================================================================

// formatSteps serializes ComputeAllSteps output to a stable string for the
// golden tests. The replica counts are the contract; sync invariants are
// covered separately by TestSyncCoordination_NewSide.
func formatSteps(steps []UpdateStep) string {
	out := ""
	for i, s := range steps {
		out += fmt.Sprintf("%d: oldP=%d oldD=%d newP=%d newD=%d\n",
			i,
			s.Past["prefill"].Replicas, s.Past["decode"].Replicas,
			s.New["prefill"].Replicas, s.New["decode"].Replicas)
	}
	return out
}

func TestComputeAllSteps_Golden(t *testing.T) {
	roles := []string{"prefill", "decode"}
	tests := []struct {
		name    string
		initial []int
		target  []int
		surge   []int
		unavail []int
		want    string
	}{
		{
			name:    "symmetric_8P4D_surge2_unavail2",
			initial: []int{8, 4}, target: []int{8, 4},
			surge: []int{2, 2}, unavail: []int{2, 2},
			want: `0: oldP=8 oldD=4 newP=0 newD=0
1: oldP=6 oldD=3 newP=2 newD=1
2: oldP=4 oldD=2 newP=4 newD=2
3: oldP=2 oldD=1 newP=6 newD=3
4: oldP=0 oldD=0 newP=8 newD=4
`,
		},
		{
			name:    "asymmetric_scale_10P2D_to_6P8D_surge2",
			initial: []int{10, 2}, target: []int{6, 8},
			surge: []int{2, 2}, unavail: []int{0, 0},
			want: `0: oldP=10 oldD=2 newP=0 newD=0
1: oldP=6 oldD=2 newP=1 newD=2
2: oldP=5 oldD=1 newP=2 newD=3
3: oldP=4 oldD=0 newP=3 newD=4
4: oldP=3 oldD=0 newP=4 newD=6
5: oldP=2 oldD=0 newP=5 newD=7
6: oldP=1 oldD=0 newP=6 newD=8
7: oldP=0 oldD=0 newP=6 newD=8
`,
		},
		{
			name:    "scale_down_20P4D_to_12P4D_surge3_unavail2",
			initial: []int{20, 4}, target: []int{12, 4},
			surge: []int{3, 3}, unavail: []int{2, 2},
			want: `0: oldP=20 oldD=4 newP=0 newD=0
1: oldP=15 oldD=3 newP=3 newD=1
2: oldP=10 oldD=2 newP=6 newD=2
3: oldP=5 oldD=1 newP=9 newD=3
4: oldP=1 oldD=0 newP=12 newD=4
5: oldP=0 oldD=0 newP=12 newD=4
`,
		},
		{
			name:    "surge0_unavail2_4P4D",
			initial: []int{4, 4}, target: []int{4, 4},
			surge: []int{0, 0}, unavail: []int{2, 2},
			want: `0: oldP=4 oldD=4 newP=0 newD=0
1: oldP=3 oldD=3 newP=0 newD=0
2: oldP=2 oldD=2 newP=1 newD=1
3: oldP=1 oldD=1 newP=2 newD=2
4: oldP=0 oldD=0 newP=3 newD=3
5: oldP=0 oldD=0 newP=4 newD=4
`,
		},
		{
			name:    "imbalanced_20P4D_surge3_unavail2",
			initial: []int{20, 4}, target: []int{20, 4},
			surge: []int{3, 3}, unavail: []int{2, 2},
			want: `0: oldP=20 oldD=4 newP=0 newD=0
1: oldP=18 oldD=3 newP=3 newD=1
2: oldP=15 oldD=3 newP=5 newD=1
3: oldP=13 oldD=2 newP=8 newD=2
4: oldP=10 oldD=2 newP=10 newD=2
5: oldP=8 oldD=1 newP=13 newD=3
6: oldP=5 oldD=1 newP=15 newD=3
7: oldP=3 oldD=0 newP=18 newD=4
8: oldP=0 oldD=0 newP=20 newD=4
`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			initial := makeRoles(roles, tc.initial)
			target := makeRoles(roles, tc.target)
			cfg := makeConfig(roles, tc.surge, tc.unavail)
			steps := ComputeAllSteps(roles, initial, target, cfg)
			got := formatSteps(steps)
			if got != tc.want {
				t.Errorf("planner output diverged from golden:\n--- want ---\n%s--- got ---\n%s", tc.want, got)
			}
		})
	}
}

// =============================================================================
// Side-Progress Helper Tests
// =============================================================================

// TestSideProgress_New verifies the helper that finds the step of the slowest
// role on the NEW side (= "everyone has reached at least this fraction").
func TestSideProgress_New(t *testing.T) {
	roles := []string{"prefill", "decode"}
	target := map[string]int{"prefill": 8, "decode": 4}
	totalSteps := 4 // = sideSize(target)

	cases := []struct {
		name    string
		current map[string]int
		want    int
	}{
		{"both at 0", map[string]int{"prefill": 0, "decode": 0}, 0},
		{"both at sync 1 (25%)", map[string]int{"prefill": 2, "decode": 1}, 1},
		{"prefill mid-window, decode parked at 1", map[string]int{"prefill": 3, "decode": 1}, 1},
		{"prefill ahead, decode behind", map[string]int{"prefill": 6, "decode": 1}, 1},
		{"both complete", map[string]int{"prefill": 8, "decode": 4}, 4},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := sideProgress(roles, tc.current, target, totalSteps, false)
			assert.Equal(t, tc.want, got)
		})
	}
}

// TestSideProgress_Old verifies the OLD-side helper (drained fraction).
func TestSideProgress_Old(t *testing.T) {
	roles := []string{"prefill", "decode"}
	initial := map[string]int{"prefill": 8, "decode": 4}
	totalSteps := 4

	cases := []struct {
		name    string
		current map[string]int
		want    int
	}{
		{"no drain yet", map[string]int{"prefill": 8, "decode": 4}, 0},
		{"25% drained both", map[string]int{"prefill": 6, "decode": 3}, 1},
		{"prefill drained more, decode behind", map[string]int{"prefill": 4, "decode": 3}, 1},
		{"fully drained", map[string]int{"prefill": 0, "decode": 0}, 4},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := sideProgress(roles, tc.current, initial, totalSteps, true)
			assert.Equal(t, tc.want, got)
		})
	}
}

// TestWantReplicas verifies the per-role count at a given side step.
func TestWantReplicas(t *testing.T) {
	// NEW side: target=8, totalSteps=4. step k → ceil(8*k/4) = 2k (exact, since divides).
	assert.Equal(t, 0, wantReplicas(8, 0, 4, false))
	assert.Equal(t, 2, wantReplicas(8, 1, 4, false))
	assert.Equal(t, 4, wantReplicas(8, 2, 4, false))
	assert.Equal(t, 8, wantReplicas(8, 4, 4, false))

	// OLD side: initial=8, totalSteps=4. step k → floor(8*(4-k)/4) = 8 - 2k.
	assert.Equal(t, 8, wantReplicas(8, 0, 4, true))
	assert.Equal(t, 6, wantReplicas(8, 1, 4, true))
	assert.Equal(t, 4, wantReplicas(8, 2, 4, true))
	assert.Equal(t, 0, wantReplicas(8, 4, 4, true))

	// Non-divisible NEW: target=11, totalSteps=3. step k → ceil(11*k/3).
	assert.Equal(t, 0, wantReplicas(11, 0, 3, false))
	assert.Equal(t, 4, wantReplicas(11, 1, 3, false)) // ceil(11/3) = 4
	assert.Equal(t, 8, wantReplicas(11, 2, 3, false)) // ceil(22/3) = 8
	assert.Equal(t, 11, wantReplicas(11, 3, 3, false))

	// Non-divisible NEW: target=8, totalSteps=6 (the deadlock case from
	// asymmetric_scale_10P2D_to_6P8D). step 1 → ceil(8/6) = 2.
	assert.Equal(t, 0, wantReplicas(8, 0, 6, false))
	assert.Equal(t, 2, wantReplicas(8, 1, 6, false)) // not 1 — that would deadlock
	assert.Equal(t, 3, wantReplicas(8, 2, 6, false))
	assert.Equal(t, 8, wantReplicas(8, 6, 6, false))

	// Edge: totalSteps=0.
	assert.Equal(t, 0, wantReplicas(8, 0, 0, false))
	assert.Equal(t, 8, wantReplicas(8, 0, 0, true))
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

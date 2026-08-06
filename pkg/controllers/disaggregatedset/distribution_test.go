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
	"testing"

	"github.com/stretchr/testify/assert"
	"k8s.io/utils/ptr"

	disaggregatedsetv1 "sigs.k8s.io/lws/api/disaggregatedset/v1"
	leaderworkersetv1 "sigs.k8s.io/lws/api/leaderworkerset/v1"
)

func TestDistribute(t *testing.T) {
	tests := []struct {
		name   string
		total  int
		slices int
		want   []int
	}{
		{name: "even split", total: 4, slices: 2, want: []int{2, 2}},
		{name: "remainder to lowest slices", total: 5, slices: 2, want: []int{3, 2}},
		{name: "remainder of two", total: 8, slices: 3, want: []int{3, 3, 2}},
		{name: "total below slice count", total: 1, slices: 3, want: []int{1, 0, 0}},
		{name: "zero total", total: 0, slices: 2, want: []int{0, 0}},
		{name: "single slice passthrough", total: 7, slices: 1, want: []int{7}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := make([]int, tc.slices)
			sum := 0
			for i := range tc.slices {
				got[i] = distribute(tc.total, tc.slices, i)
				sum += got[i]
			}
			assert.Equal(t, tc.want, got)
			assert.Equal(t, max(tc.total, 0), sum, "shares must sum to the total")
		})
	}
}

// Raising the total must never lower any slice's share, and lowering it must
// never raise one, so autoscaler steps translate to monotone per-slice moves.
func TestDistributeMonotone(t *testing.T) {
	const slices = 4
	for total := 0; total < 20; total++ {
		for i := range slices {
			assert.GreaterOrEqual(t, distribute(total+1, slices, i), distribute(total, slices, i),
				"total %d -> %d lowered slice %d", total, total+1, i)
		}
	}
}

func TestResolveTargets(t *testing.T) {
	ds := &disaggregatedsetv1.DisaggregatedSet{
		Spec: disaggregatedsetv1.DisaggregatedSetSpec{
			Roles: []disaggregatedsetv1.DisaggregatedRoleSpec{
				{
					Name:    "prefill",
					Scaling: &disaggregatedsetv1.RoleScaling{Mode: disaggregatedsetv1.RoleScalingExternal},
				},
				{
					Name: "decode",
					LeaderWorkerSetTemplateSpec: leaderworkersetv1.LeaderWorkerSetTemplateSpec{
						Spec: leaderworkersetv1.LeaderWorkerSetSpec{Replicas: ptr.To(int32(2))},
					},
				},
			},
		},
	}
	scalers := map[string]*disaggregatedsetv1.DisaggregatedSetRoleScaler{
		"prefill": {Spec: disaggregatedsetv1.DisaggregatedSetRoleScalerSpec{Replicas: 5}},
	}

	targets := resolveTargets(ds, 2, scalers)

	// External: aggregate 5 split 3/2. Static: per-slice count in every slice.
	assert.Equal(t, RoleTarget{Replicas: 3}, targets[0]["prefill"])
	assert.Equal(t, RoleTarget{Replicas: 2}, targets[1]["prefill"])
	assert.Equal(t, RoleTarget{Replicas: 2}, targets[0]["decode"])
	assert.Equal(t, RoleTarget{Replicas: 2}, targets[1]["decode"])

	// A missing scaler resolves to Hold, and Resolve keeps the current count.
	targets = resolveTargets(ds, 2, nil)
	assert.True(t, targets[0]["prefill"].Hold)
	assert.Equal(t, 4, targets[0].Resolve("prefill", 4))
	assert.Equal(t, 2, targets[1].Resolve("decode", 9))
}

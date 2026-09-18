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
	"testing"

	"github.com/stretchr/testify/assert"
	"k8s.io/utils/ptr"

	disaggregatedsetv1 "sigs.k8s.io/lws/api/disaggregatedset/v1"
)

func TestSubRoleTargetResolver(t *testing.T) {
	ds := newDSWithRoles("model", disaggregatedsetv1.DisaggregatedRoleSpec{
		Name: "decode",
		SubRoles: []disaggregatedsetv1.DisaggregatedSubRoleSpec{
			{Name: "short", Scaling: &disaggregatedsetv1.RoleScaling{Mode: disaggregatedsetv1.RoleScalingExternal}},
			{Name: "long", Replicas: ptr.To(int32(2))},
		},
	})
	short := RoleKey{Role: "decode", SubRole: "short"}
	long := RoleKey{Role: "decode", SubRole: "long"}
	scalers := ScalerMap{short: {Spec: disaggregatedsetv1.DisaggregatedSetRoleScalerSpec{Replicas: 3}}}
	resolver := NewSubRoleTargetResolver()

	assert.Equal(t, []RoleKey{short, long}, resolver.Keys(ds))
	assert.Equal(t, 3, resolver.Resolve(ds, short, scalers, 0))
	assert.Equal(t, 2, resolver.Resolve(ds, long, scalers, 0))
	assert.Equal(t, 5, resolver.ParentTarget(ds, "decode", scalers, nil))
}

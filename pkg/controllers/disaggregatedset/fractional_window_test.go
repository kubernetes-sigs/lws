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
)

func TestGrowingRoleTargetsUseKEPWindow(t *testing.T) {
	current := RoleReplicaState{0, 1}
	targets := RoleReplicaState{8, 4}

	// Decode is the least-advanced role at 1/4. The window is also 1/4
	// wide, so Prefill may advance no farther than 1/2, or 4/8.
	bounded := boundGrowingRoleTargetsToWindow(current, targets, RoleReplicaState{8, 1})

	assert.Equal(t, RoleReplicaState{4, 1}, bounded)
}

func TestDrainingRoleTargetsUseKEPWindow(t *testing.T) {
	current := RoleReplicaState{8, 4}
	initial := RoleReplicaState{8, 4}

	// Prefill has not drained. Decode may drain one replica (1/4), but a
	// proposal to drain two replicas (2/4) is capped at the window boundary.
	bounded := boundDrainingRoleTargetsToWindow(current, initial, RoleReplicaState{8, 2})

	assert.Equal(t, RoleReplicaState{8, 3}, bounded)
}

func TestWindowBoundsDoNotReverseObservedProgress(t *testing.T) {
	assert.Equal(t,
		RoleReplicaState{6, 1},
		boundGrowingRoleTargetsToWindow(
			RoleReplicaState{6, 1},
			RoleReplicaState{8, 4},
			RoleReplicaState{8, 1},
		),
	)
	assert.Equal(t,
		RoleReplicaState{2, 3},
		boundDrainingRoleTargetsToWindow(
			RoleReplicaState{2, 3},
			RoleReplicaState{8, 4},
			RoleReplicaState{0, 3},
		),
	)
}

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

package features

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/component-base/featuregate"
)

func TestWorkloadAwareSchedulingDefaultsOff(t *testing.T) {
	assert.False(t, Enabled(WorkloadAwareScheduling))
}

func TestSetFeatureGateDuringTest(t *testing.T) {
	SetFeatureGateDuringTest(t, WorkloadAwareScheduling, true)
	assert.True(t, Enabled(WorkloadAwareScheduling))
}

func TestUnknownFeatureGateRejected(t *testing.T) {
	fg := featuregate.NewFeatureGate()
	require.NoError(t, fg.Add(defaultFeatureGates))
	require.Error(t, fg.SetFromMap(map[string]bool{"UnknownGate": true}))
}

func TestKnown(t *testing.T) {
	assert.Equal(t, []string{string(WorkloadAwareScheduling)}, Known())
}

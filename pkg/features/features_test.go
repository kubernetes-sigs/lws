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
)

func TestNew(t *testing.T) {
	gates, err := New(map[string]bool{WorkloadAwareScheduling: true})
	require.NoError(t, err)
	assert.True(t, gates.Enabled(WorkloadAwareScheduling))

	_, err = New(map[string]bool{"UnknownGate": true})
	require.ErrorContains(t, err, "unknown LWS feature gate")
}

func TestWorkloadAwareSchedulingDefaultsOff(t *testing.T) {
	gates, err := New(nil)
	require.NoError(t, err)
	assert.False(t, gates.Enabled(WorkloadAwareScheduling))
}

/*
Copyright 2026.

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

package fixtures

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestYAMLGeneratesCorrectRolloutStrategyStructure(t *testing.T) {
	config := PrefillDecode("test",
		Phase{Replicas: 2, HasRollout: true, MaxSurge: 1, MaxUnavailable: 0},
		Phase{Replicas: 3, HasRollout: true, MaxSurge: 2, MaxUnavailable: 1},
	)
	yaml := config.YAML()

	// Verify the YAML has the correct nested structure
	require.Contains(t, yaml, "rolloutStrategy:")
	require.Contains(t, yaml, "rollingUpdateConfiguration:")
	require.Contains(t, yaml, "maxSurge: 1")
	require.Contains(t, yaml, "maxSurge: 2")
	require.Contains(t, yaml, "maxUnavailable: 0")
	require.Contains(t, yaml, "maxUnavailable: 1")

	// Verify maxSurge is NOT directly under rolloutStrategy
	lines := strings.Split(yaml, "\n")
	for i, line := range lines {
		if strings.Contains(line, "rolloutStrategy:") && i+1 < len(lines) {
			nextLine := lines[i+1]
			require.Contains(t, nextLine, "rollingUpdateConfiguration:",
				"rollingUpdateConfiguration should be directly under rolloutStrategy")
		}
	}
}

func TestYAMLWithoutRollout(t *testing.T) {
	config := PrefillDecode("test",
		Phase{Replicas: 2, HasRollout: false},
		Phase{Replicas: 3, HasRollout: false},
	)
	yaml := config.YAML()

	// Verify no rolloutStrategy when HasRollout is false
	require.NotContains(t, yaml, "rolloutStrategy:")
	require.NotContains(t, yaml, "rollingUpdateConfiguration:")
}

func TestYAMLWithMetadata(t *testing.T) {
	config := Config{
		Name:      "test",
		Namespace: "default",
		Phases: []Phase{
			{
				Name:           "prefill",
				Replicas:       2,
				LWSLabels:      map[string]string{"kueue.x-k8s.io/queue-name": "gpu-queue"},
				LWSAnnotations: map[string]string{"note": "test"},
			},
			{
				Name:     "decode",
				Replicas: 3,
			},
		},
	}
	yaml := config.YAML()

	require.Contains(t, yaml, "metadata:")
	require.Contains(t, yaml, "kueue.x-k8s.io/queue-name: gpu-queue")
	require.Contains(t, yaml, "note: \"test\"")
}

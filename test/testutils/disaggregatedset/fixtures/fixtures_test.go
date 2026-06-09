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

package fixtures

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/util/intstr"
)

func TestYAMLGeneratesCorrectRolloutStrategyStructure(t *testing.T) {
	config := PrefillDecode("test",
		Role{Replicas: 2, HasRollout: true, MaxSurge: intstr.FromInt(1), MaxUnavailable: intstr.FromInt(0)},
		Role{Replicas: 3, HasRollout: true, MaxSurge: intstr.FromInt(2), MaxUnavailable: intstr.FromString("25%")},
	)
	yaml := config.YAML()

	require.Contains(t, yaml, "spec:\n  roles:")
	require.Contains(t, yaml, "    spec:")
	require.Contains(t, yaml, "      rolloutStrategy:")
	require.Contains(t, yaml, "        rollingUpdateConfiguration:")
	require.Contains(t, yaml, "          maxSurge: 1")
	require.Contains(t, yaml, "          maxSurge: 2")
	require.Contains(t, yaml, "          maxUnavailable: 0")
	require.Contains(t, yaml, "          maxUnavailable: 25%")

	lines := strings.Split(yaml, "\n")
	for i, line := range lines {
		if strings.Contains(line, "      rolloutStrategy:") && i+1 < len(lines) {
			require.Contains(t, lines[i+1], "        rollingUpdateConfiguration:")
		}
	}
}

func TestYAMLWithoutRollout(t *testing.T) {
	config := PrefillDecode("test",
		Role{Replicas: 2, HasRollout: false},
		Role{Replicas: 3, HasRollout: false},
	)
	yaml := config.YAML()

	require.NotContains(t, yaml, "rolloutStrategy:")
	require.NotContains(t, yaml, "rollingUpdateConfiguration:")
}

func TestYAMLWithRoleAndWorkerTemplateMetadata(t *testing.T) {
	config := Config{
		Name:      "test",
		Namespace: "default",
		Roles: []Role{
			{
				Name:           "prefill",
				Replicas:       2,
				Labels:         map[string]string{"app": "prefill"},
				Annotations:    map[string]string{"pod-note": "test"},
				LWSLabels:      map[string]string{"kueue.x-k8s.io/queue-name": "gpu-queue"},
				LWSAnnotations: map[string]string{"lws-note": "test"},
			},
			{
				Name:     "decode",
				Replicas: 3,
			},
		},
	}
	yaml := config.YAML()

	require.Contains(t, yaml, "    metadata:\n      labels:\n        kueue.x-k8s.io/queue-name: gpu-queue")
	require.Contains(t, yaml, "      annotations:\n        lws-note: \"test\"")
	require.Contains(t, yaml, "          metadata:\n            labels:\n              app: prefill")
	require.Contains(t, yaml, "            annotations:\n              pod-note: \"test\"")
}

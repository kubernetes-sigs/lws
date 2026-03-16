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

// Package fixtures provides utilities for building DisaggregatedSet test fixtures.
package fixtures

import (
	"fmt"
	"strings"
)

// Phase holds configuration for a single phase.
type Phase struct {
	Name           string
	Replicas       int
	Image          string
	MaxSurge       int
	MaxUnavailable int
	HasRollout     bool
	Labels         map[string]string
	Annotations    map[string]string
}

// Config holds configuration for generating DisaggregatedSet YAML.
type Config struct {
	Name        string
	Namespace   string
	PhasePolicy string // "Strict" or "Flexible", empty means default
	Phases      []Phase
}

// YAML generates a DisaggregatedSet YAML from config.
func (c Config) YAML() string {
	ns := c.Namespace
	if ns == "" {
		ns = "default"
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf(`apiVersion: disaggregatedset.x-k8s.io/v1alpha1
kind: DisaggregatedSet
metadata:
  name: %s
  namespace: %s
spec:
`, c.Name, ns))

	if c.PhasePolicy != "" {
		sb.WriteString(fmt.Sprintf("  phasePolicy: %s\n", c.PhasePolicy))
	}

	sb.WriteString("  phases:\n")
	for _, p := range c.Phases {
		sb.WriteString(fmt.Sprintf("  - name: %s\n", p.Name))
		sb.WriteString(fmt.Sprintf("    replicas: %d\n", p.Replicas))

		if p.HasRollout {
			sb.WriteString("    rolloutStrategy:\n")
			sb.WriteString(fmt.Sprintf("      maxSurge: %d\n", p.MaxSurge))
			sb.WriteString(fmt.Sprintf("      maxUnavailable: %d\n", p.MaxUnavailable))
		}

		image := p.Image
		if image == "" {
			image = "registry.k8s.io/pause:3.9"
		}
		sb.WriteString("    leaderWorkerTemplate:\n")
		sb.WriteString("      size: 1\n")
		sb.WriteString("      workerTemplate:\n")

		// Add metadata section if labels or annotations are present
		if len(p.Labels) > 0 || len(p.Annotations) > 0 {
			sb.WriteString("        metadata:\n")
			if len(p.Labels) > 0 {
				sb.WriteString("          labels:\n")
				for k, v := range p.Labels {
					sb.WriteString(fmt.Sprintf("            %s: %s\n", k, v))
				}
			}
			if len(p.Annotations) > 0 {
				sb.WriteString("          annotations:\n")
				for k, v := range p.Annotations {
					sb.WriteString(fmt.Sprintf("            %s: \"%s\"\n", k, v))
				}
			}
		}

		sb.WriteString("        spec:\n")
		sb.WriteString("          containers:\n")
		sb.WriteString("          - name: main\n")
		sb.WriteString(fmt.Sprintf("            image: %s\n", image))
	}

	return sb.String()
}

// PrefillDecode creates a 2-phase config with prefill and decode phases.
func PrefillDecode(name string, prefill, decode Phase) Config {
	prefill.Name = "prefill"
	decode.Name = "decode"
	return Config{
		Name:   name,
		Phases: []Phase{prefill, decode},
	}
}

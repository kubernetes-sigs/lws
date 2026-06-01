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

// Package fixtures provides utilities for building DisaggregatedSet test fixtures.
package fixtures

import (
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/util/intstr"
)

// Role holds configuration for a single role.
type Role struct {
	Name           string
	Replicas       int
	Image          string
	MaxSurge       intstr.IntOrString
	MaxUnavailable intstr.IntOrString
	Partition      *int // nil = not set, 0 = valid, >0 = invalid (rejected by webhook)
	HasRollout     bool
	Labels         map[string]string // workerTemplate labels (propagate to pods)
	Annotations    map[string]string // workerTemplate annotations (propagate to pods)
	LWSLabels      map[string]string // LWS CR metadata labels (for Kueue, exclusive-topology)
	LWSAnnotations map[string]string // LWS CR metadata annotations
}

// Config holds configuration for generating DisaggregatedSet YAML.
type Config struct {
	Name      string
	Namespace string
	Roles     []Role
}

// YAML generates a DisaggregatedSet YAML from config.
func (c Config) YAML() string {
	ns := c.Namespace
	if ns == "" {
		ns = "default"
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf(`apiVersion: disaggregatedset.x-k8s.io/v1
kind: DisaggregatedSet
metadata:
  name: %s
  namespace: %s
spec:
`, c.Name, ns))

	sb.WriteString("  roles:\n")
	for _, p := range c.Roles {
		sb.WriteString(fmt.Sprintf("  - name: %s\n", p.Name))

		// LWS CR metadata (labels/annotations on the LWS ObjectMeta) — at role level
		if len(p.LWSLabels) > 0 || len(p.LWSAnnotations) > 0 {
			sb.WriteString("    metadata:\n")
			if len(p.LWSLabels) > 0 {
				sb.WriteString("      labels:\n")
				for k, v := range p.LWSLabels {
					sb.WriteString(fmt.Sprintf("        %s: %s\n", k, v))
				}
			}
			if len(p.LWSAnnotations) > 0 {
				sb.WriteString("      annotations:\n")
				for k, v := range p.LWSAnnotations {
					sb.WriteString(fmt.Sprintf("        %s: \"%s\"\n", k, v))
				}
			}
		}

		// spec: wraps LeaderWorkerSetSpec fields
		sb.WriteString("    spec:\n")
		sb.WriteString(fmt.Sprintf("      replicas: %d\n", p.Replicas))

		if p.HasRollout || p.Partition != nil {
			sb.WriteString("      rolloutStrategy:\n")
			sb.WriteString("        rollingUpdateConfiguration:\n")
			if p.HasRollout {
				sb.WriteString(fmt.Sprintf("          maxSurge: %s\n", p.MaxSurge.String()))
				sb.WriteString(fmt.Sprintf("          maxUnavailable: %s\n", p.MaxUnavailable.String()))
			}
			if p.Partition != nil {
				sb.WriteString(fmt.Sprintf("          partition: %d\n", *p.Partition))
			}
		}

		image := p.Image
		if image == "" {
			image = "registry.k8s.io/pause:3.9"
		}
		sb.WriteString("      leaderWorkerTemplate:\n")
		sb.WriteString("        size: 1\n")
		sb.WriteString("        workerTemplate:\n")

		// Pod template metadata (labels/annotations propagated to pods)
		if len(p.Labels) > 0 || len(p.Annotations) > 0 {
			sb.WriteString("          metadata:\n")
			if len(p.Labels) > 0 {
				sb.WriteString("            labels:\n")
				for k, v := range p.Labels {
					sb.WriteString(fmt.Sprintf("              %s: %s\n", k, v))
				}
			}
			if len(p.Annotations) > 0 {
				sb.WriteString("            annotations:\n")
				for k, v := range p.Annotations {
					sb.WriteString(fmt.Sprintf("              %s: \"%s\"\n", k, v))
				}
			}
		}

		sb.WriteString("          spec:\n")
		sb.WriteString("            containers:\n")
		sb.WriteString("            - name: main\n")
		sb.WriteString(fmt.Sprintf("              image: %s\n", image))
	}

	return sb.String()
}

// PrefillDecode creates a 2-role config with prefill and decode roles.
func PrefillDecode(name string, prefill, decode Role) Config {
	prefill.Name = "prefill"
	decode.Name = "decode"
	return Config{
		Name:  name,
		Roles: []Role{prefill, decode},
	}
}

// Ptr returns a pointer to v.
func Ptr[T any](v T) *T {
	return &v
}

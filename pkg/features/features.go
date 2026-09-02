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

// Package features declares versioned feature gates owned by LWS.
package features

import (
	"fmt"

	"k8s.io/apimachinery/pkg/util/sets"
)

const (
	// WorkloadAwareScheduling enables new opt-ins to the typed spec.scheduling
	// API. Existing scheduled objects continue reconciling when it is disabled.
	WorkloadAwareScheduling = "WorkloadAwareScheduling"
)

var known = sets.New(WorkloadAwareScheduling)

// Gates is an immutable snapshot of the configured LWS feature gates.
type Gates map[string]bool

// New validates and copies a feature gate configuration.
func New(config map[string]bool) (Gates, error) {
	for name := range config {
		if !known.Has(name) {
			return nil, fmt.Errorf("unknown LWS feature gate %q", name)
		}
	}
	gates := make(Gates, len(config))
	for name, enabled := range config {
		gates[name] = enabled
	}
	return gates, nil
}

// Enabled reports whether the named gate is enabled. Alpha gates default off.
func (g Gates) Enabled(name string) bool {
	return g != nil && g[name]
}

// Known returns the set of supported LWS feature-gate names.
func Known() []string {
	return sets.List(known)
}

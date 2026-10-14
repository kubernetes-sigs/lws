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

package main

import (
	"fmt"
	"maps"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"time"

	"sigs.k8s.io/yaml"
)

// Summary is the result of a run, written as summary.yaml.
type Summary struct {
	Phases []PhaseSummary `json:"phases"`
}

// PhaseSummary reports one phase. A latency is measured for each
// LeaderWorkerSet, from the moment the runner sent its write until the
// LeaderWorkerSet reached the state the phase asked for.
type PhaseSummary struct {
	Name string `json:"name"`
	// LeaderWorkerSets is the number of objects the phase changed, and Groups
	// and Pods the totals it added or removed.
	LeaderWorkerSets int   `json:"leaderWorkerSets"`
	Groups           int64 `json:"groups"`
	Pods             int64 `json:"pods"`
	// WallTimeMs runs from the first write until the last LeaderWorkerSet
	// reached its target.
	WallTimeMs      int64   `json:"wallTimeMs"`
	GroupsPerSecond float64 `json:"groupsPerSecond"`
	P50Ms           int64   `json:"p50Ms"`
	P90Ms           int64   `json:"p90Ms"`
	P99Ms           int64   `json:"p99Ms"`
	MaxMs           int64   `json:"maxMs"`
	// Controller is unset when the run does not read the controller metrics.
	Controller *ControllerUsage `json:"controller,omitempty"`
}

// ControllerUsage is what the LWS controller pods used during a phase, summed
// over the pods.
type ControllerUsage struct {
	CPUSeconds float64 `json:"cpuSeconds"`
	// PeakMemoryMiB is the highest resident memory, sampled every second.
	PeakMemoryMiB float64 `json:"peakMemoryMiB"`
	// APIRequests counts the requests the controller sent to the API server.
	APIRequests         int64            `json:"apiRequests"`
	APIRequestsByMethod map[string]int64 `json:"apiRequestsByMethod,omitempty"`
}

func summarizePhase(name string, latencies []time.Duration, wallTime time.Duration, groups, pods int64) PhaseSummary {
	sorted := slices.Sorted(slices.Values(latencies))
	summary := PhaseSummary{
		Name:             name,
		LeaderWorkerSets: len(sorted),
		Groups:           groups,
		Pods:             pods,
		WallTimeMs:       wallTime.Milliseconds(),
		P50Ms:            percentile(sorted, 50).Milliseconds(),
		P90Ms:            percentile(sorted, 90).Milliseconds(),
		P99Ms:            percentile(sorted, 99).Milliseconds(),
		MaxMs:            percentile(sorted, 100).Milliseconds(),
	}
	if wallTime > 0 {
		summary.GroupsPerSecond = round2(float64(groups) / wallTime.Seconds())
	}
	return summary
}

// percentile returns the nearest-rank percentile of sorted, which must be in
// ascending order.
func percentile(sorted []time.Duration, p int) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	return sorted[max((p*len(sorted)+99)/100, 1)-1]
}

// round2 rounds to two decimals, which keeps the summary readable.
func round2(v float64) float64 {
	return math.Round(v*100) / 100
}

func writeSummary(path string, summary *Summary) error {
	out, err := yaml.Marshal(summary)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, out, 0o644)
}

// measurements returns what a range spec can bound, by name.
func (p PhaseSummary) measurements() map[string]float64 {
	m := map[string]float64{
		"wallTimeMs": float64(p.WallTimeMs),
		"p50Ms":      float64(p.P50Ms),
		"p90Ms":      float64(p.P90Ms),
		"p99Ms":      float64(p.P99Ms),
		"maxMs":      float64(p.MaxMs),
	}
	if c := p.Controller; c != nil {
		m["controller.cpuSeconds"] = c.CPUSeconds
		m["controller.peakMemoryMiB"] = c.PeakMemoryMiB
		m["controller.apiRequests"] = float64(c.APIRequests)
	}
	return m
}

// RangeSpec sets upper bounds for the measurements of each phase, for example
// {create: {wallTimeMs: 60000, controller.apiRequests: 5000}}.
type RangeSpec struct {
	Phases map[string]map[string]float64 `json:"phases"`
}

func loadRangeSpec(path string) (*RangeSpec, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	spec := &RangeSpec{}
	if err := yaml.UnmarshalStrict(raw, spec); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	return spec, nil
}

// check returns a message for each bound the summary exceeds or does not have
// a measurement for.
func (r *RangeSpec) check(summary *Summary) []string {
	byName := map[string]PhaseSummary{}
	for _, phase := range summary.Phases {
		byName[phase.Name] = phase
	}
	var failures []string
	for _, name := range slices.Sorted(maps.Keys(r.Phases)) {
		phase, found := byName[name]
		if !found {
			failures = append(failures, fmt.Sprintf("phase %s: not in the summary", name))
			continue
		}
		measured, bounds := phase.measurements(), r.Phases[name]
		for _, metric := range slices.Sorted(maps.Keys(bounds)) {
			got, found := measured[metric]
			switch {
			case !found:
				failures = append(failures, fmt.Sprintf("phase %s: %s is not measured", name, metric))
			case got > bounds[metric]:
				failures = append(failures, fmt.Sprintf("phase %s: %s is %s, above the limit of %s", name, metric,
					strconv.FormatFloat(got, 'f', -1, 64), strconv.FormatFloat(bounds[metric], 'f', -1, 64)))
			}
		}
	}
	return failures
}

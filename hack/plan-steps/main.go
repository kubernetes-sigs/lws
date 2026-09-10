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

// Command plan-steps computes the step-by-step rollout plan for a DisaggregatedSet update.
//
// Usage:
//
//	go run ./hack/plan-steps \
//	  --source '{"prefill": 6, "decode": 2}' \
//	  --target '{"prefill": 6, "decode": 2}' \
//	  --surge '{"prefill": 2, "decode": 2}'
//
// This helps preview how a coordinated DisaggregatedSet rollout will progress.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"

	"sigs.k8s.io/lws/pkg/controllers/disaggregatedset"
)

type updateStep struct {
	Past []int
	New  []int
}

func main() {
	sourceJSON := flag.String("source", "", `Source replicas as JSON, e.g. '{"prefill": 6, "decode": 2}'`)
	targetJSON := flag.String("target", "", `Target replicas as JSON, e.g. '{"prefill": 6, "decode": 2}'`)
	surgeJSON := flag.String("surge", "{}", `Max surge per role as JSON, e.g. '{"prefill": 2, "decode": 1}' (default: 1)`)
	unavailableJSON := flag.String("unavailable", "{}", `Max unavailable per role as JSON, e.g. '{"prefill": 0, "decode": 1}' (default: 0)`)
	flag.Parse()

	if *sourceJSON == "" || *targetJSON == "" {
		fmt.Fprintln(os.Stderr, "Usage: plan-steps --source JSON --target JSON [--surge JSON] [--unavailable JSON]")
		fmt.Fprintln(os.Stderr, "")
		fmt.Fprintln(os.Stderr, "Example:")
		fmt.Fprintln(os.Stderr, `  plan-steps --source '{"prefill": 6, "decode": 2}' --target '{"prefill": 6, "decode": 2}' --surge '{"prefill": 2}'`)
		fmt.Fprintln(os.Stderr, "")
		flag.PrintDefaults()
		os.Exit(1)
	}

	source := parseRoleMap(*sourceJSON, "source")
	target := parseRoleMap(*targetJSON, "target")
	surge := parseRoleMap(*surgeJSON, "surge")
	unavailable := parseRoleMap(*unavailableJSON, "unavailable")

	roleNames := mergedSortedKeys(source, target)
	if len(roleNames) < 2 {
		fmt.Fprintln(os.Stderr, "Error: at least 2 roles required")
		os.Exit(1)
	}

	sourceSlice := make([]int, len(roleNames))
	targetSlice := make([]int, len(roleNames))
	surgeSlice := make([]int, len(roleNames))
	unavailableSlice := make([]int, len(roleNames))
	for i, name := range roleNames {
		sourceSlice[i] = getOrDefault(source, name, 0)
		targetSlice[i] = getOrDefault(target, name, 0)
		surgeSlice[i] = getOrDefault(surge, name, 1)
		unavailableSlice[i] = getOrDefault(unavailable, name, 0)
	}

	fmt.Printf("Roles: %v\n", roleNames)
	fmt.Printf("Source: %s\n", formatRoleValues(roleNames, sourceSlice))
	fmt.Printf("Target: %s\n", formatRoleValues(roleNames, targetSlice))
	fmt.Printf("Config: %s\n\n", formatRoleConfig(roleNames, surgeSlice, unavailableSlice))

	steps := computeAllSteps(sourceSlice, targetSlice, surgeSlice, unavailableSlice)
	printSteps(os.Stdout, roleNames, steps)
}

func computeAllSteps(source, target, surge, unavailable []int) []updateStep {
	config := make([]disaggregatedset.RollingUpdateConfig, len(source))
	for i := range config {
		config[i].MaxSurge = surge[i]
		config[i].MaxUnavailable = unavailable[i]
	}

	plannerSteps := disaggregatedset.ComputeAllSteps(source, target, config)
	steps := make([]updateStep, len(plannerSteps))
	for i, step := range plannerSteps {
		steps[i] = updateStep{
			Past: step.Past,
			New:  step.New,
		}
	}
	return steps
}

func parseRoleMap(jsonStr, name string) map[string]int {
	result := make(map[string]int)
	if jsonStr == "" || jsonStr == "{}" {
		return result
	}
	if err := json.Unmarshal([]byte(jsonStr), &result); err != nil {
		fmt.Fprintf(os.Stderr, "Error parsing %s JSON: %v\n", name, err)
		os.Exit(1)
	}
	return result
}

func mergedSortedKeys(a, b map[string]int) []string {
	seen := make(map[string]bool)
	for k := range a {
		seen[k] = true
	}
	for k := range b {
		seen[k] = true
	}
	keys := make([]string, 0, len(seen))
	for k := range seen {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

func getOrDefault(m map[string]int, key string, defaultVal int) int {
	if v, ok := m[key]; ok {
		return v
	}
	return defaultVal
}

func formatRoleValues(names []string, values []int) string {
	parts := make([]string, len(names))
	for i, name := range names {
		parts[i] = fmt.Sprintf("%s=%d", name, values[i])
	}
	return strings.Join(parts, ", ")
}

func formatRoleConfig(names []string, surge, unavailable []int) string {
	parts := make([]string, len(names))
	for i, name := range names {
		parts[i] = fmt.Sprintf("%s(surge=%d, unavailable=%d)", name, surge[i], unavailable[i])
	}
	return strings.Join(parts, ", ")
}

func printSteps(out *os.File, roleNames []string, steps []updateStep) {
	headers := []string{"Step"}
	for _, name := range roleNames {
		headers = append(headers, "Old "+name)
	}
	for _, name := range roleNames {
		headers = append(headers, "New "+name)
	}
	headers = append(headers, "Total", "Action")

	rows := make([][]string, 0, len(steps))
	for i, step := range steps {
		row := []string{strconv.Itoa(i)}
		total := 0
		for _, v := range step.Past {
			row = append(row, strconv.Itoa(v))
			total += v
		}
		for _, v := range step.New {
			row = append(row, strconv.Itoa(v))
			total += v
		}
		row = append(row, strconv.Itoa(total), describeAction(i, steps, roleNames))
		rows = append(rows, row)
	}

	widths := columnWidths(headers, rows)
	printRow(out, headers, widths)
	printSeparator(out, widths)
	for _, row := range rows {
		printRow(out, row, widths)
	}
}

func columnWidths(headers []string, rows [][]string) []int {
	widths := make([]int, len(headers))
	for i, header := range headers {
		widths[i] = len(header)
	}
	for _, row := range rows {
		for i, value := range row {
			widths[i] = max(widths[i], len(value))
		}
	}
	return widths
}

func printRow(out *os.File, row []string, widths []int) {
	for i, value := range row {
		if i > 0 {
			fmt.Fprint(out, "  ")
		}
		fmt.Fprintf(out, "%-*s", widths[i], value)
	}
	fmt.Fprintln(out)
}

func printSeparator(out *os.File, widths []int) {
	for i, width := range widths {
		if i > 0 {
			fmt.Fprint(out, "  ")
		}
		fmt.Fprint(out, strings.Repeat("-", width))
	}
	fmt.Fprintln(out)
}

func describeAction(stepIndex int, steps []updateStep, roleNames []string) string {
	if stepIndex == 0 {
		return "initial"
	}

	prev := steps[stepIndex-1]
	curr := steps[stepIndex]
	var actions []string

	for i, name := range roleNames {
		if curr.New[i] > prev.New[i] {
			actions = append(actions, fmt.Sprintf("new %s +%d", name, curr.New[i]-prev.New[i]))
		}
	}
	for i, name := range roleNames {
		if curr.Past[i] < prev.Past[i] {
			actions = append(actions, fmt.Sprintf("old %s -%d", name, prev.Past[i]-curr.Past[i]))
		}
	}
	if len(actions) == 0 {
		return "no change"
	}
	return strings.Join(actions, ", ")
}

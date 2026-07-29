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
	Past map[string]int
	New  map[string]int
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

	sourceMap := make(map[string]int, len(roleNames))
	targetMap := make(map[string]int, len(roleNames))
	configMap := make(map[string]disaggregatedset.RollingUpdateConfig, len(roleNames))
	for _, name := range roleNames {
		sourceMap[name] = getOrDefault(source, name, 0)
		targetMap[name] = getOrDefault(target, name, 0)
		configMap[name] = disaggregatedset.RollingUpdateConfig{
			MaxSurge:       getOrDefault(surge, name, 1),
			MaxUnavailable: getOrDefault(unavailable, name, 0),
		}
	}

	fmt.Printf("Roles: %v\n", roleNames)
	fmt.Printf("Source: %s\n", formatRoleMap(roleNames, sourceMap))
	fmt.Printf("Target: %s\n", formatRoleMap(roleNames, targetMap))
	fmt.Printf("Config: %s\n\n", formatConfigMap(roleNames, configMap))

	steps := computeSteps(roleNames, sourceMap, targetMap, configMap)
	printSteps(os.Stdout, roleNames, steps)
}

func computeSteps(roleNames []string, source, target map[string]int, config map[string]disaggregatedset.RollingUpdateConfig) []updateStep {
	plannerSteps := disaggregatedset.ComputeAllSteps(roleNames, source, target, config)
	steps := make([]updateStep, len(plannerSteps))
	for i, step := range plannerSteps {
		past := make(map[string]int, len(roleNames))
		new := make(map[string]int, len(roleNames))
		for _, role := range roleNames {
			past[role] = step.Past[role].Replicas
			new[role] = step.New[role].Replicas
		}
		steps[i] = updateStep{Past: past, New: new}
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

func formatRoleMap(names []string, values map[string]int) string {
	parts := make([]string, len(names))
	for i, name := range names {
		parts[i] = fmt.Sprintf("%s=%d", name, values[name])
	}
	return strings.Join(parts, ", ")
}

func formatConfigMap(names []string, config map[string]disaggregatedset.RollingUpdateConfig) string {
	parts := make([]string, len(names))
	for i, name := range names {
		c := config[name]
		parts[i] = fmt.Sprintf("%s(surge=%d, unavailable=%d)", name, c.MaxSurge, c.MaxUnavailable)
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
		for _, name := range roleNames {
			v := step.Past[name]
			row = append(row, strconv.Itoa(v))
			total += v
		}
		for _, name := range roleNames {
			v := step.New[name]
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

	for _, name := range roleNames {
		if curr.New[name] > prev.New[name] {
			actions = append(actions, fmt.Sprintf("new %s +%d", name, curr.New[name]-prev.New[name]))
		}
	}
	for _, name := range roleNames {
		if curr.Past[name] < prev.Past[name] {
			actions = append(actions, fmt.Sprintf("old %s -%d", name, prev.Past[name]-curr.Past[name]))
		}
	}
	if len(actions) == 0 {
		return "no change"
	}
	return strings.Join(actions, ", ")
}

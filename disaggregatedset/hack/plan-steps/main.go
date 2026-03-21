// Command plan-steps computes the step-by-step rollout plan for a DisaggregatedSet update.
//
// Usage:
//
//	go run ./cmd/plan-steps \
//	  --source '{"prefill": 6, "decode": 2}' \
//	  --target '{"prefill": 6, "decode": 2}' \
//	  --surge '{"prefill": 2, "decode": 2}'
//
// This helps understand what will happen during a specific rollout in advance.
// Supports arbitrary phase names and will support N phases when the planner does.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"

	"github.com/olekukonko/tablewriter"
	"github.com/olekukonko/tablewriter/tw"

	"sigs.k8s.io/disaggregatedset/internal/controller"
)

// updateStep represents a single step with N phases (slice-based for flexibility).
type updateStep struct {
	Past []int
	New  []int
}

func main() {
	sourceJSON := flag.String("source", "", `Source replicas as JSON, e.g. '{"prefill": 6, "decode": 2}'`)
	targetJSON := flag.String("target", "", `Target replicas as JSON, e.g. '{"prefill": 6, "decode": 2}'`)
	surgeJSON := flag.String("surge", "{}", `Max surge per phase as JSON, e.g. '{"prefill": 2, "decode": 1}' (default: 1)`)
	unavailableJSON := flag.String("unavailable", "{}", `Max unavailable per phase as JSON, e.g. '{"prefill": 0, "decode": 1}' (default: 0)`)

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

	source := parsePhaseMap(*sourceJSON, "source")
	target := parsePhaseMap(*targetJSON, "target")
	surge := parsePhaseMap(*surgeJSON, "surge")
	unavailable := parsePhaseMap(*unavailableJSON, "unavailable")

	// Get ordered phase names from union of source and target
	phaseNames := mergedSortedKeys(source, target)
	numPhases := len(phaseNames)

	if numPhases < 2 {
		fmt.Fprintln(os.Stderr, "Error: at least 2 phases required")
		os.Exit(1)
	}

	// Build slices in phase order (missing phases default to 0)
	sourceSlice := make([]int, numPhases)
	targetSlice := make([]int, numPhases)
	surgeSlice := make([]int, numPhases)
	unavailSlice := make([]int, numPhases)
	for i, name := range phaseNames {
		sourceSlice[i] = getOrDefault(source, name, 0)
		targetSlice[i] = getOrDefault(target, name, 0)
		surgeSlice[i] = getOrDefault(surge, name, 1)
		unavailSlice[i] = getOrDefault(unavailable, name, 0)
	}

	// Print configuration
	fmt.Printf("Phases: %v\n", phaseNames)
	fmt.Printf("Source: %s\n", formatPhaseValues(phaseNames, sourceSlice))
	fmt.Printf("Target: %s\n", formatPhaseValues(phaseNames, targetSlice))
	fmt.Printf("Config: %s\n\n", formatPhaseConfig(phaseNames, surgeSlice, unavailSlice))

	// Compute steps (adapter for current 2-phase planner)
	steps := computeAllStepsAdapter(sourceSlice, targetSlice, surgeSlice, unavailSlice)

	// Build table headers dynamically: Step, Old phase1, Old phase2, ..., New phase1, New phase2, ..., Total, Action
	headers := []string{"Step"}
	for _, name := range phaseNames {
		headers = append(headers, "Old "+name)
	}
	for _, name := range phaseNames {
		headers = append(headers, "New "+name)
	}
	headers = append(headers, "Total", "Action")

	// Build table data
	var data [][]string
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

		row = append(row, strconv.Itoa(total))
		row = append(row, describeAction(i, steps, phaseNames))
		data = append(data, row)
	}

	// Create and render table (disable auto-format to preserve phase names like "decode-long-context")
	// Note: WithHeaderAutoFormat must come BEFORE WithHeader since Header() defaults to AutoFormat=On
	table := tablewriter.NewTable(os.Stdout,
		tablewriter.WithHeaderAutoFormat(tw.Off),
		tablewriter.WithHeader(headers),
	)
	table.Bulk(data)
	table.Render()
}

// computeAllStepsAdapter converts N-phase planner steps to local updateStep type.
func computeAllStepsAdapter(source, target, surge, unavail []int) []updateStep {
	config := make([]controller.RollingUpdateConfig, len(source))
	for i := range config {
		config[i].MaxSurge = surge[i]
		config[i].MaxUnavailable = unavail[i]
	}

	// Call N-phase planner
	plannerSteps := controller.ComputeAllSteps(source, target, config)

	// Convert to local step type
	steps := make([]updateStep, len(plannerSteps))
	for i, ps := range plannerSteps {
		steps[i] = updateStep{
			Past: ps.Past,
			New:  ps.New,
		}
	}
	return steps
}

// parsePhaseMap parses a JSON object like '{"prefill": 6, "decode": 2}' into a map
func parsePhaseMap(jsonStr, name string) map[string]int {
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

// mergedSortedKeys returns the union of keys from both maps in sorted order
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

// getOrDefault returns the value for key or defaultVal if not present
func getOrDefault(m map[string]int, key string, defaultVal int) int {
	if v, ok := m[key]; ok {
		return v
	}
	return defaultVal
}

// formatPhaseValues formats phase names and values as "name1=val1, name2=val2, ..."
func formatPhaseValues(names []string, values []int) string {
	parts := make([]string, len(names))
	for i, name := range names {
		parts[i] = fmt.Sprintf("%s=%d", name, values[i])
	}
	return strings.Join(parts, ", ")
}

// formatPhaseConfig formats phase config as "name1(surge=x, unavail=y), ..."
func formatPhaseConfig(names []string, surge, unavail []int) string {
	parts := make([]string, len(names))
	for i, name := range names {
		parts[i] = fmt.Sprintf("%s(surge=%d, unavail=%d)", name, surge[i], unavail[i])
	}
	return strings.Join(parts, ", ")
}

// describeAction returns a human-readable description of what changed in this step
func describeAction(stepIndex int, steps []updateStep, phaseNames []string) string {
	if stepIndex == 0 {
		return "initial"
	}

	prev := steps[stepIndex-1]
	curr := steps[stepIndex]

	var actions []string

	// Check for new replica increases (scale up)
	for i, name := range phaseNames {
		if curr.New[i] > prev.New[i] {
			actions = append(actions, fmt.Sprintf("new %s +%d", name, curr.New[i]-prev.New[i]))
		}
	}

	// Check for old replica decreases (scale down)
	for i, name := range phaseNames {
		if curr.Past[i] < prev.Past[i] {
			actions = append(actions, fmt.Sprintf("old %s -%d", name, prev.Past[i]-curr.Past[i]))
		}
	}

	if len(actions) == 0 {
		return "no change"
	}

	return strings.Join(actions, ", ")
}

// Command plan-steps computes the step-by-step rollout plan for a DisaggDeployment update.
//
// Usage:
//
//	go run ./cmd/plan-steps \
//	  --source-prefill 6 --source-decode 2 \
//	  --target-prefill 6 --target-decode 2 \
//	  --prefill-surge 2 --decode-surge 2
//
// This helps understand what will happen during a specific rollout in advance.
package main

import (
	"flag"
	"fmt"
	"os"
	"strconv"

	"github.com/olekukonko/tablewriter"

	"sigs.k8s.io/disaggregatedset/internal/controller"
)

func main() {
	sourcePrefill := flag.Int("source-prefill", 0, "Source prefill replicas (required)")
	sourceDecode := flag.Int("source-decode", 0, "Source decode replicas (required)")
	targetPrefill := flag.Int("target-prefill", 0, "Target prefill replicas (required)")
	targetDecode := flag.Int("target-decode", 0, "Target decode replicas (required)")
	prefillSurge := flag.Int("prefill-surge", 1, "Max surge for prefill side")
	decodeSurge := flag.Int("decode-surge", 1, "Max surge for decode side")
	prefillUnavailable := flag.Int("prefill-unavailable", 0, "Max unavailable for prefill side")
	decodeUnavailable := flag.Int("decode-unavailable", 0, "Max unavailable for decode side")

	flag.Parse()

	if *sourcePrefill == 0 && *sourceDecode == 0 {
		fmt.Fprintln(os.Stderr, "Usage: plan-steps [options]")
		fmt.Fprintln(os.Stderr, "")
		flag.PrintDefaults()
		os.Exit(1)
	}

	source := controller.SideReplicaState{
		Prefill: *sourcePrefill,
		Decode:  *sourceDecode,
	}
	target := controller.SideReplicaState{
		Prefill: *targetPrefill,
		Decode:  *targetDecode,
	}
	rollingConfig := controller.RollingUpdateConfig{
		PrefillMaxSurge:       *prefillSurge,
		DecodeMaxSurge:        *decodeSurge,
		PrefillMaxUnavailable: *prefillUnavailable,
		DecodeMaxUnavailable:  *decodeUnavailable,
	}

	// Print configuration
	fmt.Printf("Source: prefill=%d, decode=%d\n", source.Prefill, source.Decode)
	fmt.Printf("Target: prefill=%d, decode=%d\n", target.Prefill, target.Decode)
	fmt.Printf("Config: prefill(surge=%d, unavail=%d), decode(surge=%d, unavail=%d)\n\n",
		rollingConfig.PrefillMaxSurge, rollingConfig.PrefillMaxUnavailable,
		rollingConfig.DecodeMaxSurge, rollingConfig.DecodeMaxUnavailable)

	// Build the full plan using ComputeAllSteps
	steps := controller.ComputeAllSteps(source.Prefill, source.Decode, target.Prefill, target.Decode, rollingConfig)

	// Build table data
	var data [][]string
	for i, step := range steps {
		total := step.PastPrefill + step.PastDecode + step.NewPrefill + step.NewDecode
		action := describeAction(i, steps)

		data = append(data, []string{
			strconv.Itoa(i),
			strconv.Itoa(step.PastPrefill),
			strconv.Itoa(step.PastDecode),
			strconv.Itoa(step.NewPrefill),
			strconv.Itoa(step.NewDecode),
			strconv.Itoa(total),
			action,
		})
	}

	// Create and render table
	table := tablewriter.NewTable(os.Stdout,
		tablewriter.WithHeader([]string{"Step", "Old Prefill", "Old Decode", "New Prefill", "New Decode", "Total", "Action"}),
	)
	table.Bulk(data)
	table.Render()
}

// describeAction returns a human-readable description of what changed in this step
func describeAction(stepIndex int, steps []controller.UpdateStep) string {
	if stepIndex == 0 {
		return "initial"
	}

	prev := steps[stepIndex-1]
	curr := steps[stepIndex]

	var actions []string

	if curr.NewPrefill > prev.NewPrefill {
		actions = append(actions, fmt.Sprintf("new prefill +%d", curr.NewPrefill-prev.NewPrefill))
	}
	if curr.NewDecode > prev.NewDecode {
		actions = append(actions, fmt.Sprintf("new decode +%d", curr.NewDecode-prev.NewDecode))
	}
	if curr.PastPrefill < prev.PastPrefill {
		actions = append(actions, fmt.Sprintf("old prefill -%d", prev.PastPrefill-curr.PastPrefill))
	}
	if curr.PastDecode < prev.PastDecode {
		actions = append(actions, fmt.Sprintf("old decode -%d", prev.PastDecode-curr.PastDecode))
	}

	if len(actions) == 0 {
		return "no change"
	}

	result := actions[0]
	for i := 1; i < len(actions); i++ {
		result += ", " + actions[i]
	}
	return result
}

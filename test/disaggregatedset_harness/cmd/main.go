// Copyright 2026 The Kubernetes Authors.
// SPDX-License-Identifier: Apache-2.0
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"

	harness "sigs.k8s.io/lws/test/disaggregatedset_harness"
)

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }

func run(args []string, out, errout io.Writer) int {
	modeOnly := len(args) > 0 && args[0] == "launcher-mode"
	if modeOnly {
		args = args[1:]
	}
	noArgs := len(args) == 0
	f := flag.NewFlagSet("lws-harness", flag.ContinueOnError)
	f.SetOutput(errout)
	var selection harness.Selection
	o := harness.Options{Output: out}
	f.StringVar(&selection.Identity, "identity", "both", "ordinal, hash, or both")
	f.StringVar(&selection.Policy, "policy", "", "Deferred or Immediate (scale-down)")
	f.StringVar(&selection.Variant, "variant", "", "integer or percentage (outage)")
	f.StringVar(&selection.RolloutChain, "rollout-chain", "both", "ab, abc, or both (large)")
	f.BoolVar(&selection.IncludeLarge, "include-large", false, "include the two optional larger rename cases")
	list := f.Bool("list", false, "list selected cases, without cluster access")
	dry := f.Bool("dry-run", false, "validate and describe selected cases without cluster access")
	validate := f.Bool("validate", false, "validate selection only")
	group := f.Bool("process-group", false, "isolate child process group (launcher only)")
	checkpointFlags(f, &o.Auto)
	f.BoolVar(&o.FailFast, "fail-fast", false, "stop on first failed case")
	f.StringVar(&o.Kubeconfig, "kubeconfig", "", "owned private kubeconfig")
	f.StringVar(&o.Context, "context", "", "owned private context")
	f.StringVar(&o.RunDir, "run-dir", "", "owned run directory")
	f.StringVar(&o.Node, "node", "", "owned kind node")
	f.StringVar(&o.ControllerCommit, "controller-commit", "", "checked-out controller commit SHA")
	f.StringVar(&o.Observer, "observer", "", "read-only observer binary built against the checked-out commit")
	f.Usage = func() {
		if modeOnly {
			return
		}
		fmt.Fprintln(f.Output(), "Usage: lws-harness [list|dry-run|run] [case IDs|all|large-all] [options]\nNormally use test/disaggregatedset_harness/run.sh to build the checked-out commit and provision an isolated, owned kind cluster.")
		f.PrintDefaults()
	}
	if len(args) > 0 {
		switch args[0] {
		case "list":
			*list = true
			args = args[1:]
		case "dry-run":
			*dry = true
			args = args[1:]
		case "run":
			args = args[1:]
		}
	}
	// flag deliberately stops at a positional argument. Normalize here so the
	// established `ordinary --identity hash` launcher interface remains accepted.
	var flags, names []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !strings.HasPrefix(a, "-") {
			names = append(names, a)
			continue
		}
		flags = append(flags, a)
		key := strings.TrimLeft(strings.SplitN(a, "=", 2)[0], "-")
		def := f.Lookup(key)
		if def != nil && !strings.Contains(a, "=") {
			if boolean, ok := def.Value.(interface{ IsBoolFlag() bool }); !ok || !boolean.IsBoolFlag() {
				i++
				if i >= len(args) {
					fmt.Fprintln(errout, "missing value for", a)
					return 2
				}
				flags = append(flags, args[i])
			}
		}
	}
	if err := f.Parse(flags); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			if modeOnly {
				fmt.Fprintln(out, "readonly")
			}
			return 0
		}
		return 2
	}
	selection.Names = names
	selected, err := harness.Matrix(selection)
	if err != nil {
		fmt.Fprintln(errout, err)
		return 2
	}
	readOnly := *list || *dry || *validate || noArgs
	if modeOnly {
		mode := "automatic"
		if readOnly {
			mode = "readonly"
		} else if !o.Auto {
			mode = "interactive"
		}
		fmt.Fprintln(out, mode)
		return 0
	}
	if readOnly {
		fmt.Fprintf(out, "%d runs; synthetic slots are not GPUs. No Kubernetes operations performed.\n", len(selected))
		for _, c := range selected {
			fmt.Fprintf(out, "  %-20s / %-7s / %-9s / %s\n", c.Name, c.Identity, c.Policy, c.Variant)
		}
		return 0
	}
	if *group {
		if err := newProcessGroup(); err != nil {
			fmt.Fprintln(errout, err)
			return 2
		}
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()
	_, err = harness.ExecuteSuite(ctx, o, selected)
	if err != nil {
		fmt.Fprintln(errout, err)
		if ctx.Err() != nil || errors.Is(err, context.Canceled) {
			return 130
		}
		return 1
	}
	return 0
}

func checkpointFlags(f *flag.FlagSet, auto *bool) {
	f.BoolVar(auto, "auto", true, "automatic checkpoints")
	f.BoolFunc("interactive", "Enter-to-trigger checkpoints; a switches to auto; q cancels", func(v string) error {
		b, err := strconv.ParseBool(v)
		if err == nil {
			*auto = !b
		}
		return err
	})
}

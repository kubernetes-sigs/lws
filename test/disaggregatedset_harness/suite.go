// Copyright 2026 The Kubernetes Authors.
// SPDX-License-Identifier: Apache-2.0
package harness

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

type Result struct {
	Case             string   `json:"case"`
	Identity         string   `json:"identity"`
	Policy           string   `json:"policy"`
	Passed           bool     `json:"passed"`
	Error            *string  `json:"error"`
	Seconds          float64  `json:"seconds"`
	ControllerCommit string   `json:"controllerCommit"`
	Checks           []string `json:"checks"`
}

var scenarios = map[string]func(*Lab){"ordinary": ordinary, "scale-up": scaleUp, "scale-down": scaleDown, "autoscaler-handoff": autoscalerHandoff, "rename": func(l *Lab) { renamedRollout(l, pair(2, 2), pair(2, 2), 1) }, "rename-outage": renameOutage, "restart-rollback": restartRollback, "subroles": subroles, "native-restart": nativeRestart, "outage": outage, "emergency": emergency, "large-rename": func(l *Lab) { renamedRollout(l, pair(9, 4), pair(8, 3), 4) }, "large-scale-up": largeScaleUp, "large-scale-down": largeScaleDown, "large-ordinary": largeOrdinary, "large-subroles": largeSubroles}

// ExecuteSuite validates ownership before any mutation, creates native raw
// evidence and attempts every selected case unless cancelled or FailFast is set.
// Cluster provisioning belongs to the launcher, not this package.
func ExecuteSuite(ctx context.Context, o Options, selected []Case) (results []Result, err error) {
	o.defaults()
	// Share buffered interactive input across cases; pasted/scripted answers
	// already read ahead by one checkpoint must not disappear on the next case.
	if _, ok := o.Input.(*bufio.Reader); !ok {
		o.Input = bufio.NewReader(o.Input)
	}
	if len(selected) == 0 {
		return nil, fmt.Errorf("no scenarios selected")
	}
	for _, c := range selected {
		allowed, selectionErr := Matrix(Selection{Names: []string{c.Name}})
		valid := false
		for _, candidate := range allowed {
			valid = valid || candidate == c
		}
		if scenarios[c.Name] == nil || selectionErr != nil || !valid {
			return nil, fmt.Errorf("invalid case selection: %+v", c)
		}
	}
	if err = ValidateOwnership(ctx, o); err != nil {
		return nil, err
	}
	e, err := NewEvidence(o.RunDir, o.ControllerCommit)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, e.Close()) }()
	if err = e.Start(ctx, o); err != nil {
		return nil, err
	}
	for i, c := range selected {
		l := newLab(ctx, o, c, e)
		if err = os.Mkdir(l.Directory, 0700); err != nil {
			return results, err
		}
		e.Case(c.ID())
		e.Emit("actions.jsonl", Object{"id": c.ID(), "kind": "case", "phase": "start", "time": unix(l.clock.Now()), "directory": l.Directory})
		fmt.Fprintf(o.Output, "\n%d/%d  %s · %s · %s\n", i+1, len(selected), c.DisplayName(), c.Identity, c.Policy)
		caseErr := runCase(l)
		o.Auto = l.Auto
		result := Result{Case: c.DisplayName(), Identity: c.Identity, Policy: c.Policy, Passed: caseErr == nil, Seconds: l.clock.Now().Sub(l.started).Seconds(), ControllerCommit: o.ControllerCommit, Checks: l.checks}
		if result.Checks == nil {
			result.Checks = []string{}
		}
		if caseErr != nil {
			message := caseErr.Error()
			result.Error = &message
		}
		results = append(results, result)
		e.Emit("actions.jsonl", Object{"id": c.ID(), "kind": "case", "phase": "end", "time": unix(l.clock.Now()), "result": result})
		if err = writeJSON(filepath.Join(l.Directory, "result.json"), result); err != nil {
			return results, err
		}
		if err = writeJSON(filepath.Join(o.RunDir, "results.json"), results); err != nil {
			return results, err
		}
		fmt.Fprintf(o.Output, "%s: %s / %s / %s\n", map[bool]string{true: "PASS", false: "FAIL"}[result.Passed], c.DisplayName(), c.Identity, c.Policy)
		if errors.Is(caseErr, context.Canceled) || errors.Is(caseErr, context.DeadlineExceeded) {
			return results, caseErr
		}
		if ctx.Err() != nil {
			return results, ctx.Err()
		}
		if caseErr != nil && o.FailFast {
			break
		}
		if err = e.Error(); err != nil {
			return results, err
		}
	}
	passed := 0
	for _, r := range results {
		if r.Passed {
			passed++
		}
	}
	fmt.Fprintf(o.Output, "\n%d/%d attempted runs passed; %d selected.\n", passed, len(results), len(selected))
	if passed != len(selected) || len(results) != len(selected) {
		return results, fmt.Errorf("suite incomplete or failed: %d/%d passed", passed, len(selected))
	}
	return results, nil
}
func runCase(l *Lab) (err error) {
	defer func() { err = errors.Join(err, l.Close()) }()
	err = catch(func() {
		l.Reset()
		scenarios[l.Case.Name](l)
		l.Wait("Read-only planner bounds captured", func(Snapshot) bool { return l.HasDiagnostic() }, 20*time.Second, 0, nil)
	})
	if err != nil && l.ctx.Err() == nil {
		captureErr := catch(func() {
			abort(writeJSON(filepath.Join(l.Directory, "failure-snapshot.json"), l.Snapshot()))
			logs := l.K("-n", "lws-system", "logs", "deployment/lws-controller-manager", "--tail=1500")
			abort(os.WriteFile(filepath.Join(l.Directory, "controller-failure.log"), logs, 0600))
		})
		if captureErr != nil {
			fmt.Fprintln(l.Options.Output, "Additional failure capture:", captureErr)
		}
	}
	return err
}

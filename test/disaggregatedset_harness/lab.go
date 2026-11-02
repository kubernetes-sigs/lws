// Copyright 2026 The Kubernetes Authors.
// SPDX-License-Identifier: Apache-2.0
package harness

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

type Lab struct {
	Options        Options
	Case           Case
	Directory      string
	Auto           bool
	ctx            context.Context
	clock          Clock
	started        time.Time
	evidence       *Evidence
	stack          []string
	sequence       int
	checks         []string
	input          *bufio.Reader
	virtual        bool
	observerCancel context.CancelFunc
	observerDone   chan struct{}
	diagnosticMu   sync.Mutex
	diagnostic     Object
	observerErr    error
}

func newLab(ctx context.Context, o Options, c Case, e *Evidence) *Lab {
	o.defaults()
	input, ok := o.Input.(*bufio.Reader)
	if !ok {
		input = bufio.NewReader(o.Input)
	}
	return &Lab{Options: o, Case: c, Directory: filepath.Join(o.RunDir, c.DirectoryName()), Auto: o.Auto, ctx: ctx, clock: o.Clock, started: o.Clock.Now(), evidence: e, input: input}
}
func (l *Lab) parent() any {
	if len(l.stack) == 0 {
		return nil
	}
	return l.stack[len(l.stack)-1]
}
func (l *Lab) action(method string, args []any, kwargs Object, extra Object) func() {
	if args == nil {
		args = []any{}
	}
	if kwargs == nil {
		kwargs = Object{}
	}
	l.sequence++
	id := fmt.Sprintf("%s-a%06d", l.Case.ID(), l.sequence)
	start := l.clock.Now()
	row := Object{"id": id, "parentId": l.parent(), "kind": method, "method": method, "phase": "start", "time": unix(start), "args": args, "kwargs": kwargs}
	for k, v := range extra {
		row[k] = v
	}
	l.evidence.Emit("actions.jsonl", clone(row))
	l.stack = append(l.stack, id)
	return func() {
		p := recover()
		l.stack = l.stack[:len(l.stack)-1]
		row["phase"] = "end"
		row["time"] = unix(l.clock.Now())
		row["startedAt"] = unix(start)
		row["durationSeconds"] = l.clock.Now().Sub(start).Seconds()
		if p != nil {
			row["error"] = fmt.Sprint(p)
		}
		l.evidence.Emit("actions.jsonl", row)
		if p != nil {
			panic(p)
		}
	}
}
func (l *Lab) K(args ...string) []byte { return l.KInput(nil, args...) }
func (l *Lab) KInput(input any, args ...string) []byte {
	mutation := false
	dry := false
	for _, a := range args {
		mutation = mutation || oneOf(a, "apply", "patch", "scale", "delete", "exec", "label", "annotate", "replace", "create")
		dry = dry || strings.HasPrefix(a, "--dry-run")
	}
	a := make([]any, len(args))
	for i, v := range args {
		a[i] = v
	}
	defer l.action("kubectl", a, Object{}, Object{"method": "k", "mutation": mutation, "dryRun": dry})()
	var stdin []byte
	if input != nil {
		stdin = []byte(jsonText(input))
	}
	ctx, cancel := context.WithTimeout(l.ctx, 45*time.Second)
	defer cancel()
	out, stderr, err := l.Options.Executor.Run(ctx, l.Options.Kubectl, l.Options.kube(args...), stdin)
	if err != nil {
		abort(fmt.Errorf("kubectl %s: %w: %s", strings.Join(redactPatch(args), " "), err, strings.TrimSpace(string(stderr))))
	}
	return out
}
func redactPatch(args []string) []string {
	out := append([]string{}, args...)
	for i := 1; i < len(out); i++ {
		if out[i-1] == "-p" {
			out[i] = "<JSON>"
		}
	}
	return out
}
func (l *Lab) JSON(args ...string) Object {
	var o Object
	abort(json.Unmarshal(l.K(args...), &o))
	return o
}
func (l *Lab) Apply(o Object) {
	defer l.action("apply", []any{o}, nil, nil)()
	l.KInput(o, "-n", Namespace, "apply", "-f", "-")
}
func (l *Lab) Patch(o Object) {
	defer l.action("patch_set", []any{o}, nil, nil)()
	l.K("-n", Namespace, "patch", DSResource, SetName, "--type=merge", "-p", jsonText(o))
}
func (l *Lab) Assert(ok bool, message string) {
	phase := "end"
	if !ok {
		phase = "failure"
	}
	l.evidence.Emit("actions.jsonl", Object{"kind": "assert", "phase": phase, "time": unix(l.clock.Now()), "parentId": l.parent(), "passed": ok, "message": message})
	if !ok {
		abort(fmt.Errorf("assertion: %s", message))
	}
}
func (l *Lab) Note(message string) {
	defer l.action("note", []any{message}, nil, nil)()
	fmt.Fprintln(l.Options.Output, message)
}
func (l *Lab) Checkpoint(title, description string) {
	defer l.action("checkpoint", []any{title, description}, nil, nil)()
	fmt.Fprintf(l.Options.Output, "\n━━ %s\n   Scenario trigger: %s\n", title, description)
	abort(appendJSON(filepath.Join(l.Directory, "events.jsonl"), Object{"time": unix(l.clock.Now()), "title": title, "event": description}))
	if !l.Auto {
		fmt.Fprint(l.Options.Output, "[Enter] trigger · [a] automatic · [q] stop: ")
		type response struct {
			line string
			err  error
		}
		reply := make(chan response, 1)
		go func() { line, err := l.input.ReadString('\n'); reply <- response{line, err} }()
		var answer string
		var err error
		select {
		case <-l.ctx.Done():
			abort(l.ctx.Err())
		case value := <-reply:
			answer, err = value.line, value.err
		}
		if err != nil {
			abort(fmt.Errorf("%w: interactive input ended: %v", context.Canceled, err))
		}
		switch strings.TrimSpace(strings.ToLower(answer)) {
		case "a":
			l.Auto = true
		case "q":
			abort(context.Canceled)
		}
	}
}
func appendJSON(path string, value any) error {
	f, e := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if e != nil {
		return e
	}
	defer f.Close()
	return json.NewEncoder(f).Encode(value)
}
func (l *Lab) Sleep(d time.Duration) { abort(l.clock.Sleep(l.ctx, d)) }
func (l *Lab) SetSlots(n int) {
	defer l.action("set_slots", []any{n}, nil, nil)()
	l.K("patch", "node", l.Options.Node, "--subresource=status", "--type=json", "-p", jsonText([]Object{{"op": "add", "path": "/status/capacity/" + strings.ReplaceAll(Slot, "/", "~1"), "value": fmt.Sprint(n)}}))
	deadline := l.clock.Now().Add(45 * time.Second)
	for l.clock.Now().Before(deadline) {
		node := l.JSON("get", "node", l.Options.Node, "-o", "json")
		if at(node, "status", "allocatable", Slot) == fmt.Sprint(n) {
			return
		}
		l.Sleep(time.Second)
	}
	abort(fmt.Errorf("node did not acknowledge synthetic slot capacity %d", n))
}
func (l *Lab) Snapshot() Snapshot {
	items, err := parseResourceList(l.K("-n", Namespace, "get", DSResource+",lws,pods,dsrs", "-o", "json"))
	abort(err)
	s := stateOf(items)
	if s.DS != nil {
		s.Events = objects(l.JSON("-n", Namespace, "get", "events", "--field-selector", "involvedObject.uid="+uid(s.DS), "-o", "json")["items"])
	}
	return s
}

type Predicate func(Snapshot) bool
type Tick func(Snapshot)

func (l *Lab) Wait(label string, p Predicate, timeout time.Duration, stable int, tick Tick) Snapshot {
	defer l.action("wait", []any{label}, Object{"timeout": timeout.Seconds(), "stable": stable}, nil)()
	if timeout == 0 {
		timeout = 180 * time.Second
	}
	if stable == 0 {
		stable = 2
	}
	deadline := l.clock.Now().Add(timeout)
	hits := 0
	for l.clock.Now().Before(deadline) {
		abort(l.ctx.Err())
		abort(l.observerError())
		s := l.Snapshot()
		if tick != nil {
			tick(s)
		}
		abort(appendJSON(filepath.Join(l.Directory, "samples.jsonl"), Object{"time": unix(l.clock.Now()), "checkpoint": label, "ds": s.DS, "lws": s.LWS, "pods": s.Pods, "events": s.Events, "scalers": s.Scalers}))
		if p(s) {
			hits++
		} else {
			hits = 0
		}
		if hits >= stable {
			l.checks = append(l.checks, label)
			fmt.Fprintln(l.Options.Output, "  PASS:", label)
			return s
		}
		l.Sleep(time.Second)
	}
	abort(fmt.Errorf("timeout: %s", label))
	return Snapshot{}
}
func (l *Lab) WaitFor(label string, p Predicate) Snapshot { return l.Wait(label, p, 0, 0, nil) }
func (l *Lab) Hold(label string, seconds int, p Predicate) Snapshot {
	defer l.action("hold", []any{label, seconds}, nil, nil)()
	until := l.clock.Now().Add(time.Duration(seconds) * time.Second)
	return l.Wait(label, func(s Snapshot) bool { l.Assert(p(s), label); return !l.clock.Now().Before(until) }, time.Duration(seconds+30)*time.Second, 1, nil)
}
func (l *Lab) Reset() {
	defer l.action("reset", nil, nil, nil)()
	l.K("-n", Namespace, "delete", DSResource, "--all", "--wait=true", "--timeout=90s")
	l.K("-n", Namespace, "delete", "lws", "--all", "--wait=true", "--timeout=90s")
	l.Wait("previous groups and scalers disappear", func(s Snapshot) bool { return len(s.Pods) == 0 && len(s.LWS) == 0 && len(s.Scalers) == 0 }, 120*time.Second, 0, nil)
	l.SetSlots(64)
}
func (l *Lab) StartObserver() {
	if l.observerCancel != nil {
		return
	}
	ctx, cancel := context.WithCancel(l.ctx)
	cmd := exec.CommandContext(ctx, l.Options.Observer, "--kubeconfig", l.Options.Kubeconfig, "--context", l.Options.Context, "--namespace", Namespace, "--name", SetName, "--watch", "2")
	out, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		abort(err)
	}
	errors, err := os.OpenFile(filepath.Join(l.Directory, "observer-errors.log"), os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0600)
	if err != nil {
		cancel()
		abort(err)
	}
	cmd.Stderr = errors
	if err := cmd.Start(); err != nil {
		cancel()
		_ = errors.Close()
		abort(err)
	}
	l.observerCancel = cancel
	l.observerDone = make(chan struct{})
	go func() {
		defer close(l.observerDone)
		defer errors.Close()
		stream, err := os.OpenFile(filepath.Join(l.Directory, "observer.jsonl"), os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0600)
		if err != nil {
			l.observerFailed(err)
			cancel()
			_ = cmd.Wait()
			return
		}
		defer stream.Close()
		scanner := bufio.NewScanner(out)
		scanner.Buffer(make([]byte, 64*1024), 16*1024*1024)
		for scanner.Scan() {
			line := append([]byte{}, scanner.Bytes()...)
			if _, err := stream.Write(append(line, '\n')); err != nil {
				l.observerFailed(err)
				cancel()
				break
			}
			var row Object
			if json.Unmarshal(line, &row) == nil {
				l.diagnosticMu.Lock()
				l.diagnostic = row
				l.diagnosticMu.Unlock()
			}
		}
		if err := scanner.Err(); err != nil && ctx.Err() == nil {
			l.observerFailed(err)
		}
		waitErr := cmd.Wait()
		if ctx.Err() == nil {
			if waitErr == nil {
				waitErr = fmt.Errorf("observer exited before scenario completed")
			}
			l.observerFailed(waitErr)
		}
	}()
}
func (l *Lab) observerFailed(err error) {
	l.diagnosticMu.Lock()
	defer l.diagnosticMu.Unlock()
	l.diagnostic = nil
	if l.observerErr == nil {
		l.observerErr = fmt.Errorf("read-only observer failed: %w", err)
		// Clear the prior diagnostic in the portable trace too. If the failure
		// itself is a disk error this append may fail; the original error still
		// makes the scenario fail rather than claiming complete evidence.
		_ = appendJSON(filepath.Join(l.Directory, "observer.jsonl"), Object{"schemaVersion": 1, "observedAt": l.clock.Now().UTC().Format(time.RFC3339Nano), "error": l.observerErr.Error()})
	}
}
func (l *Lab) observerError() error {
	l.diagnosticMu.Lock()
	defer l.diagnosticMu.Unlock()
	return l.observerErr
}
func (l *Lab) Close() error {
	if l.observerCancel != nil {
		l.observerCancel()
		<-l.observerDone
	}
	l.diagnosticMu.Lock()
	defer l.diagnosticMu.Unlock()
	return l.observerErr
}
func (l *Lab) HasDiagnostic() bool {
	l.diagnosticMu.Lock()
	defer l.diagnosticMu.Unlock()
	abort(l.observerErr)
	return len(arr(l.diagnostic["slices"])) > 0
}

type CreateOptions struct {
	Version            string
	Surge, Unavailable any
	Size               int
	HoldReady          bool
	Names              []string
	Slots              int
}

func (l *Lab) Create(p, d int, c CreateOptions) {
	if c.Version == "" {
		c.Version = "A"
	}
	if c.Surge == nil {
		c.Surge = 1
	}
	if c.Unavailable == nil {
		c.Unavailable = 0
	}
	if c.Size == 0 {
		c.Size = 1
	}
	if c.Slots == 0 {
		c.Slots = 64
	}
	if len(c.Names) == 0 {
		c.Names = []string{"prefill", "decode"}
	}
	defer l.action("create", []any{p, d}, Object{"version": c.Version, "surge": c.Surge, "unavailable": c.Unavailable, "size": c.Size, "ready": !c.HoldReady, "names": c.Names, "slots": c.Slots}, nil)()
	l.SetSlots(c.Slots)
	roles := []any{}
	for i, n := range []int{p, d} {
		role := c.Names[i]
		roles = append(roles, RoleSpec(role, n, l.Case.Identity, c.Version, budget(c.Surge, role), budget(c.Unavailable, role), c.Size, !c.HoldReady))
	}
	l.Apply(Object{"apiVersion": "disaggregatedset.x-k8s.io/v1", "kind": "DisaggregatedSet", "metadata": Object{"name": SetName}, "spec": Object{"roles": roles, "scalingPolicy": Object{"scaleDownDuringRollout": l.Case.Policy}}})
	l.StartObserver()
	if !c.HoldReady {
		l.Converge(c.Version, Targets{c.Names[0]: p, c.Names[1]: d}, 0)
	}
}
func budget(v any, role string) any {
	switch m := v.(type) {
	case map[string]int:
		return m[role]
	case map[string]any:
		if m[role] == nil {
			return 0
		}
		return m[role]
	}
	return v
}

type Update struct {
	Version, Policy    string
	Counts             Targets
	Ready              *bool
	Names              []string
	Surge, Unavailable any
	ExternalRoles      []string
}

func boolPtr(v bool) *bool { return &v }
func (l *Lab) Update(u Update) {
	defer l.action("update", nil, Object{"version": u.Version, "counts": u.Counts, "policy": u.Policy, "ready": u.Ready, "names": u.Names, "surge": u.Surge, "unavailable": u.Unavailable, "external_roles": u.ExternalRoles}, nil)()
	ds := l.JSON("-n", Namespace, "get", DSResource, SetName, "-o", "json")
	roles := objects(at(clone(ds), "spec", "roles"))
	for i, r := range roles {
		if len(u.Names) > 0 {
			r["name"] = u.Names[i]
		}
		role := str(r["name"])
		spec := obj(r["spec"])
		if n, ok := u.Counts[role]; ok {
			spec["replicas"] = n
		}
		if contains(u.ExternalRoles, role) {
			r["scaling"] = Object{"mode": "External"}
			delete(spec, "replicas")
		}
		configuration := obj(at(spec, "rolloutStrategy", "rollingUpdateConfiguration"))
		if u.Surge != nil {
			configuration["maxSurge"] = budget(u.Surge, role)
		}
		if u.Unavailable != nil {
			configuration["maxUnavailable"] = budget(u.Unavailable, role)
		}
		for _, key := range []string{"workerTemplate", "leaderTemplate"} {
			t := obj(at(spec, "leaderWorkerTemplate", key))
			if t == nil {
				continue
			}
			if u.Version != "" {
				meta := obj(t["metadata"])
				if meta == nil {
					meta = Object{}
					t["metadata"] = meta
				}
				labels := obj(meta["labels"])
				if labels == nil {
					labels = Object{}
					meta["labels"] = labels
				}
				labels[VersionLabel] = u.Version
			}
			if u.Ready != nil {
				value := "0"
				if *u.Ready {
					value = "1"
				}
				for _, c := range objects(at(t, "spec", "containers")) {
					for _, env := range objects(c["env"]) {
						if env["name"] == "AUTO_READY" {
							env["value"] = value
						}
					}
				}
			}
		}
	}
	patch := Object{"spec": Object{"roles": roles}}
	if u.Policy != "" {
		obj(patch["spec"])["scalingPolicy"] = Object{"scaleDownDuringRollout": u.Policy}
	}
	l.Patch(patch)
}
func (l *Lab) Release(version, role string, count int, s *Snapshot) {
	defer l.action("release", nil, Object{"version": version, "role": role, "count": count}, nil)()
	state := Snapshot{}
	if s == nil {
		state = l.Snapshot()
	} else {
		state = *s
	}
	groups := Groups(state, version, role)
	sort.SliceStable(groups, func(i, j int) bool {
		a, b := allPods(groups[i], Ready), allPods(groups[j], Ready)
		if a != b {
			return a
		}
		return name(groups[i][0]) < name(groups[j][0])
	})
	if count >= 0 && count < len(groups) {
		groups = groups[:count]
	}
	for _, g := range groups {
		for _, p := range g {
			l.releasePod(p)
		}
	}
}
func (l *Lab) releasePod(p Object) {
	if Ready(p) || at(p, "status", "phase") != "Running" {
		return
	}
	func() {
		defer func() {
			if r := recover(); r != nil {
				if f, ok := r.(failure); ok && (strings.Contains(f.err.Error(), "not found") || strings.Contains(f.err.Error(), "not running")) {
					return
				}
				panic(r)
			}
		}()
		l.K("-n", Namespace, "exec", name(p), "-c", "demo", "--", "touch", "/tmp/ready")
	}()
}
func (l *Lab) Counts(s Snapshot, v string) map[string]Counts {
	if l.virtual {
		return SubroleCounts(s, v, true)
	}
	return GroupCounts(s, v)
}
func (l *Lab) Count(s Snapshot, v, role string) Counts { return l.Counts(s, v)[role] }
func (l *Lab) Total(s Snapshot, role, field string) int {
	sum := 0
	for _, v := range []string{"A", "B", "C"} {
		c := l.Count(s, v, role)
		switch field {
		case "ready":
			sum += c.Ready
		case "scheduled":
			sum += c.Scheduled
		default:
			sum += c.Spec
		}
	}
	return sum
}
func (l *Lab) Converge(v string, t Targets, timeout time.Duration) Snapshot {
	defer l.action("converge", []any{v, t}, nil, nil)()
	if timeout == 0 {
		timeout = 240 * time.Second
	}
	return l.Wait(v+" converges to "+describe(t), func(s Snapshot) bool { return converged(s, v, t) }, timeout, 3, func(s Snapshot) { l.Release(v, "", -1, &s) })
}
func converged(s Snapshot, v string, t Targets) bool {
	children := false
	for k := range t {
		children = children || strings.Contains(k, "/")
	}
	actual := GroupCounts(s, v)
	if children {
		actual = SubroleCounts(s, v, true)
	}
	for k, n := range t {
		if actual[k].Spec != n || actual[k].Ready != n {
			return false
		}
	}
	expected := 0
	for _, lws := range s.LWS {
		if VersionOf(lws) != v && integer(at(lws, "spec", "replicas"), 1) != 0 {
			return false
		}
		if VersionOf(lws) == v {
			expected += integer(at(lws, "spec", "replicas"), 1) * integer(at(lws, "spec", "leaderWorkerTemplate", "size"), 1)
		}
	}
	if len(s.Pods) != expected || !allPods(s.Pods, func(p Object) bool { return !deleting(p) && label(p, VersionLabel) == v }) {
		return false
	}
	return s.DS != nil && at(s.DS, "status", "observedGeneration") == at(s.DS, "metadata", "generation")
}
func (l *Lab) RestartController() {
	defer l.action("restart_controller", nil, nil, nil)()
	pods := objects(l.JSON("-n", "lws-system", "get", "pods", "-l", "control-plane=controller-manager", "-o", "json")["items"])
	l.Assert(len(pods) == 1, "exactly one owned controller to restart")
	old := uid(pods[0])
	l.K("-n", "lws-system", "delete", "pod", name(pods[0]), "--wait=false")
	deadline := l.clock.Now().Add(120 * time.Second)
	for {
		pods = objects(l.JSON("-n", "lws-system", "get", "pods", "-l", "control-plane=controller-manager", "-o", "json")["items"])
		someReady := false
		for _, p := range pods {
			someReady = someReady || Ready(p)
		}
		if len(pods) > 0 && someReady && allPods(pods, func(p Object) bool { return uid(p) != old }) {
			break
		}
		l.Assert(l.clock.Now().Before(deadline), "replacement controller did not become Ready")
		l.Sleep(time.Second)
	}
	l.K("-n", "lws-system", "rollout", "status", "deployment/lws-controller-manager", "--timeout=120s")
	deadline = l.clock.Now().Add(60 * time.Second)
	for {
		err := catch(func() {
			l.K("-n", Namespace, "patch", DSResource, SetName, "--type=merge", "--dry-run=server", "-p", jsonText(Object{"metadata": Object{"annotations": Object{"harness.lws.x-k8s.io/admission-probe": "ready"}}}))
		})
		if err == nil {
			break
		}
		if !strings.Contains(err.Error(), "failed calling webhook") || !l.clock.Now().Before(deadline) {
			abort(err)
		}
		l.Sleep(time.Second)
	}
}
func catch(f func()) (err error) {
	defer func() {
		if p := recover(); p != nil {
			if failed, ok := p.(failure); ok {
				err = failed.err
			} else {
				panic(p)
			}
		}
	}()
	f()
	return nil
}

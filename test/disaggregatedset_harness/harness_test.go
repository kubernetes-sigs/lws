// Copyright 2026 The Kubernetes Authors.
// SPDX-License-Identifier: Apache-2.0
package harness

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.now }
func (c *fakeClock) Sleep(ctx context.Context, d time.Duration) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
	return nil
}

type command struct {
	program string
	args    []string
	input   []byte
	ctx     context.Context
}
type fakeExecutor struct {
	calls []command
	run   func(command) ([]byte, []byte, error)
}

func (e *fakeExecutor) Run(ctx context.Context, p string, a []string, in []byte) ([]byte, []byte, error) {
	call := command{p, append([]string{}, a...), in, ctx}
	e.calls = append(e.calls, call)
	if e.run != nil {
		return e.run(call)
	}
	return nil, nil, nil
}
func testLab(t *testing.T, e Executor) *Lab {
	t.Helper()
	o := Options{RunDir: t.TempDir(), Kubeconfig: "/private/kubeconfig", Context: "kind-lws-v1-run-test", Node: "lws-v1-run-test-control-plane", Auto: true, Executor: e, Clock: &fakeClock{now: time.Unix(1800000000, 0)}, Output: io.Discard, Input: strings.NewReader(""), Observer: "/nonexistent-observer"}
	l := newLab(context.Background(), o, Case{"ordinary", "Hash", "Deferred", "default"}, nil)
	if err := os.Mkdir(l.Directory, 0700); err != nil {
		t.Fatal(err)
	}
	return l
}
func requireNoError(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func TestUnserializableInputFailsCaseWithoutExecutingCommand(t *testing.T) {
	executor := &fakeExecutor{}
	l := testLab(t, executor)
	err := catch(func() { l.Apply(Object{"invalid": func() {}}) })
	var unsupported *json.UnsupportedTypeError
	if !errors.As(err, &unsupported) {
		t.Fatalf("Apply returned %v, want a recoverable JSON encoding error", err)
	}
	if len(executor.calls) != 0 {
		t.Fatal("invalid manifest invoked a command", executor.calls)
	}
}
func pod(name, version, role, group, index string, isReady bool) Object {
	status := "False"
	if isReady {
		status = "True"
	}
	return Object{"apiVersion": "v1", "kind": "Pod", "metadata": Object{"name": name, "uid": "uid-" + name, "labels": Object{VersionLabel: version, RoleLabel: role, LWSLabel: version + "-" + role, GroupLabel: group, WorkerIndex: index, RevisionLabel: "revision-" + version}}, "spec": Object{"nodeName": "owned"}, "status": Object{"phase": "Running", "conditions": []any{Object{"type": "Ready", "status": status}}}}
}
func fleet(counts map[string]map[string]Counts, size int) Snapshot {
	s := Snapshot{DS: Object{"apiVersion": "disaggregatedset.x-k8s.io/v1", "kind": "DisaggregatedSet", "metadata": Object{"name": SetName, "uid": "ds-uid", "generation": 2}, "status": Object{"observedGeneration": 2}}, LWS: []Object{}, Pods: []Object{}, Events: []Object{}, Scalers: []Object{}}
	for _, v := range keys(counts) {
		for _, r := range keys(counts[v]) {
			c := counts[v][r]
			lws := Object{"apiVersion": "leaderworkerset.x-k8s.io/v1", "kind": "LeaderWorkerSet", "metadata": Object{"name": v + "-" + r, "uid": "uid-" + v + "-" + r, "labels": Object{RoleLabel: r, RevisionLabel: "revision-" + v}}, "spec": RoleSpec(r, c.Spec, "Hash", v, 1, 0, size, true)["spec"]}
			s.LWS = append(s.LWS, lws)
			for i := 0; i < c.Spec; i++ {
				for w := 0; w < size; w++ {
					p := pod(fmt.Sprintf("%s-%s-%d-%d", v, r, i, w), v, r, fmt.Sprintf("%s-%s-%d", v, r, i), fmt.Sprint(w), i < c.Ready)
					if i >= c.Scheduled {
						delete(obj(p["spec"]), "nodeName")
					}
					s.Pods = append(s.Pods, p)
				}
			}
		}
	}
	return s
}

func TestMatrixParity(t *testing.T) {
	for _, test := range []struct {
		s    Selection
		want int
	}{{Selection{}, 26}, {Selection{IncludeLarge: true}, 28}, {Selection{Names: []string{"large-all"}}, 20}, {Selection{Names: []string{"all", "large-all"}}, 46}, {Selection{Names: []string{"all", "all"}}, 26}} {
		m, err := Matrix(test.s)
		requireNoError(t, err)
		if len(m) != test.want {
			t.Fatalf("matrix %+v: got %d want %d", test.s, len(m), test.want)
		}
		seen := map[Case]bool{}
		for _, c := range m {
			if seen[c] || scenarios[c.Name] == nil {
				t.Fatalf("duplicate or unimplemented %+v", c)
			}
			seen[c] = true
		}
	}
	m, err := Matrix(Selection{Names: []string{"large-scale-down"}, Identity: "hash", Policy: "Immediate", RolloutChain: "abc"})
	requireNoError(t, err)
	if !reflect.DeepEqual(m, []Case{{"large-scale-down", "Hash", "Immediate", "abc"}}) {
		t.Fatal(m)
	}
	for _, s := range []Selection{{Names: []string{"unknown"}}, {Identity: "production"}, {Policy: "immediate"}, {Variant: "100%"}, {RolloutChain: "a"}} {
		if _, err := Matrix(s); err == nil {
			t.Fatalf("accepted %+v", s)
		}
	}
}
func TestManifestReadinessAndBudgets(t *testing.T) {
	r := RoleSpec("prefill", 1, "Hash", "B", 0, "100%", 2, false)
	if at(r, "spec", "groupIdentity") != "Hash" || at(r, "spec", "rolloutStrategy", "rollingUpdateConfiguration", "maxUnavailable") != "100%" {
		t.Fatal(r)
	}
	container := objects(at(r, "spec", "leaderWorkerTemplate", "workerTemplate", "spec", "containers"))[0]
	if objects(container["env"])[0]["value"] != "0" || at(container, "resources", "requests", Slot) != "1" {
		t.Fatal(container)
	}
}
func TestWholeGroupCountsRequireLeaderAndWorker(t *testing.T) {
	s := fleet(map[string]map[string]Counts{"A": {"decode": {Spec: 1, Ready: 1, Scheduled: 1}}}, 2)
	if GroupCounts(s, "A")["decode"].Ready != 1 {
		t.Fatal("complete group missing")
	}
	s.Pods = s.Pods[1:]
	c := GroupCounts(s, "A")["decode"]
	if c.Ready != 0 || c.Scheduled != 0 {
		t.Fatal("ready worker without leader counted as group", c)
	}
}
func TestPodRevisionChangesCannotHideBehindStableLWSRevision(t *testing.T) {
	s := fleet(map[string]map[string]Counts{"B": {"prefill": {Spec: 1, Ready: 1, Scheduled: 1}}}, 1)
	workload, previous := revisions(s, "B"), podRevisions(s, "B")
	obj(at(s.Pods[0], "metadata", "labels"))[RevisionLabel] = "unexpected-revision"
	if !sameSet(workload, revisions(s, "B")) {
		t.Fatal("fixture changed workload")
	}
	if sameSet(previous, podRevisions(s, "B")) {
		t.Fatal("Pod revision-only change was hidden")
	}
	delete(obj(at(s.Pods[0], "metadata", "labels")), RevisionLabel)
	if catch(func() { podRevisions(s, "B") }) == nil {
		t.Fatal("missing revision label silently accepted")
	}
}
func TestSubroleAssignmentsRejectIncompleteOrUnpublishedState(t *testing.T) {
	base := fleet(map[string]map[string]Counts{"A": {"decode": {Spec: 1, Ready: 1, Scheduled: 1}}}, 2)
	base.LWS[0]["metadata"].(map[string]any)["annotations"] = Object{ChildCounts: `{"a":1}`}
	for _, p := range base.Pods {
		obj(at(p, "metadata", "labels"))[SubroleLabel] = "a"
	}
	if !assigned(base, Targets{"a": 1}, "A") {
		t.Fatal("valid assignment rejected")
	}
	for _, fault := range []string{"worker", "child", "plan", "published", "terminating", "physical"} {
		s := copySnapshot(base)
		switch fault {
		case "worker":
			obj(at(s.Pods[1], "metadata", "labels"))[WorkerIndex] = "0"
		case "child":
			obj(at(s.Pods[1], "metadata", "labels"))[SubroleLabel] = "b"
		case "plan":
			obj(at(s.LWS[0], "metadata", "annotations"))[Prefix+"subrole-plan"] = "{}"
		case "published":
			obj(at(s.LWS[0], "metadata", "annotations"))[ChildCounts] = `{"a":2}`
		case "terminating":
			obj(s.Pods[1]["metadata"])["deletionTimestamp"] = "now"
		case "physical":
			obj(s.LWS[0]["spec"])["replicas"] = 2
		}
		if assigned(s, Targets{"a": 1}, "A") {
			t.Fatal("accepted", fault)
		}
	}
}
func copySnapshot(s Snapshot) Snapshot {
	b, _ := json.Marshal(s)
	var out Snapshot
	_ = json.Unmarshal(b, &out)
	return out
}
func TestHandoffDetectsHiddenReplacement(t *testing.T) {
	l := testLab(t, &fakeExecutor{})
	before := fleet(map[string]map[string]Counts{"A": {"prefill": {Spec: 3, Ready: 3, Scheduled: 3}, "decode": {Spec: 2, Ready: 2, Scheduled: 2}}}, 2)
	requireNoError(t, catch(func() { handoffPreserved(l, before, before) }))
	for _, fault := range []string{"uid", "replicas", "worker-ready", "lws"} {
		s := copySnapshot(before)
		switch fault {
		case "uid":
			obj(s.Pods[0]["metadata"])["uid"] = "replaced"
		case "replicas":
			obj(s.LWS[0]["spec"])["replicas"] = 0
		case "worker-ready":
			objects(at(s.Pods[1], "status", "conditions"))[0]["status"] = "False"
		case "lws":
			obj(s.LWS[0]["metadata"])["uid"] = "new"
		}
		if catch(func() { handoffPreserved(l, s, before) }) == nil {
			t.Fatal("accepted", fault)
		}
	}
}
func TestReleaseCountsExistingReadyFirst(t *testing.T) {
	e := &fakeExecutor{}
	l := testLab(t, e)
	s := fleet(map[string]map[string]Counts{"B": {"prefill": {Spec: 3, Ready: 1, Scheduled: 3}}}, 1)
	obj(s.Pods[0]["metadata"])["name"] = "z-ready"
	requireNoError(t, catch(func() { l.Release("B", "prefill", 2, &s) }))
	if len(e.calls) != 1 || !contains(e.calls[0].args, name(s.Pods[1])) {
		t.Fatal(e.calls)
	}
	if !reflect.DeepEqual(e.calls[0].args[:4], []string{"--kubeconfig", l.Options.Kubeconfig, "--context", l.Options.Context}) {
		t.Fatal("ambient context used")
	}
}
func TestUpdateExternalOnlyChangesReplicaSource(t *testing.T) {
	roles := []any{RoleSpec("prefill", 3, "Hash", "A", 1, 0, 2, true), RoleSpec("decode", 2, "Hash", "A", 1, 0, 2, true)}
	ds := Object{"spec": Object{"roles": roles}}
	e := &fakeExecutor{run: func(c command) ([]byte, []byte, error) {
		if contains(c.args, "get") {
			return []byte(jsonText(ds)), nil, nil
		}
		return nil, nil, nil
	}}
	l := testLab(t, e)
	requireNoError(t, catch(func() { l.Update(Update{ExternalRoles: []string{"prefill"}}) }))
	call := e.calls[len(e.calls)-1]
	var patch Object
	for i, a := range call.args {
		if a == "-p" {
			requireNoError(t, json.Unmarshal([]byte(call.args[i+1]), &patch))
		}
	}
	rs := objects(at(patch, "spec", "roles"))
	if at(rs[0], "scaling", "mode") != "External" || at(rs[0], "spec", "replicas") != nil || integer(at(rs[1], "spec", "replicas")) != 2 {
		t.Fatal(patch)
	}
	if integer(at(obj(roles[0]), "spec", "replicas")) != 3 {
		t.Fatal("mutated input")
	}
}
func TestConvergenceRequiresPhysicalCleanupAndAcknowledgement(t *testing.T) {
	s := fleet(map[string]map[string]Counts{"B": {"prefill": {Spec: 1, Ready: 1, Scheduled: 1}}}, 1)
	if !converged(s, "B", Targets{"prefill": 1}) {
		t.Fatal("valid convergence rejected")
	}
	old := pod("old", "A", "prefill", "old", "0", true)
	obj(old["metadata"])["deletionTimestamp"] = "now"
	s.Pods = append(s.Pods, old)
	if converged(s, "B", Targets{"prefill": 1}) {
		t.Fatal("terminating old Pod ignored")
	}
	s.Pods = s.Pods[:1]
	obj(s.DS["status"])["observedGeneration"] = 1
	if converged(s, "B", Targets{"prefill": 1}) {
		t.Fatal("unacknowledged generation accepted")
	}
}
func TestPercentageBounds(t *testing.T) {
	f, c := percentBounds(initial)
	if !sameTargets(f, pair(34, 18)) || !sameTargets(c, pair(41, 21)) {
		t.Fatal(f, c)
	}
	f, c = percentBounds(shrunk)
	if !sameTargets(f, pair(26, 13)) || !sameTargets(c, pair(31, 16)) {
		t.Fatal(f, c)
	}
}
func TestLargeCompletionRejectsUnexpectedTargetWorkload(t *testing.T) {
	s := fleet(map[string]map[string]Counts{"B": {"prefill": {Spec: 37, Ready: 37, Scheduled: 37}, "decode": {Spec: 19, Ready: 19, Scheduled: 19}}}, 1)
	if !largeConverged(s, "B", initial, false) {
		t.Fatal("valid final large fleet rejected")
	}
	extra := fleet(map[string]map[string]Counts{"B": {"unexpected": {Spec: 1, Ready: 1, Scheduled: 1}}}, 1)
	s.LWS = append(s.LWS, extra.LWS...)
	s.Pods = append(s.Pods, extra.Pods...)
	if largeConverged(s, "B", initial, false) {
		t.Fatal("extra target LWS/Pod accepted")
	}
}
func TestBootstrapRequiresFirstGroupAndNewMatchingEvent(t *testing.T) {
	l := testLab(t, &fakeExecutor{})
	before := fleet(map[string]map[string]Counts{"A": {"prefill": {Spec: 11, Ready: 11, Scheduled: 11}}, "B": {"prefill": {Spec: 16, Ready: 10, Scheduled: 16}}}, 1)
	s := fleet(map[string]map[string]Counts{"A": {"prefill": {Spec: 11, Ready: 11, Scheduled: 11}}, "B": {"prefill": {Spec: 16, Ready: 10, Scheduled: 16}}, "C": {"prefill": {Spec: 1, Ready: 0, Scheduled: 1}}}, 1)
	b := &firstGroupBootstrap{}
	b.start(l, before, Targets{"prefill": 27})
	if catch(func() { bounds(l, s, Targets{"prefill": 21}, Targets{"prefill": 27}, nil) }) == nil {
		t.Fatal("ordinary ceiling not strict")
	}
	requireNoError(t, catch(func() { bounds(l, s, Targets{"prefill": 21}, Targets{"prefill": 27}, b) }))
	if b.confirmed(s) {
		t.Fatal("invented bootstrap event")
	}
	s.Events = []Object{{"metadata": Object{"uid": "event"}, "reason": "BootstrapSurge", "involvedObject": Object{"uid": "ds-uid"}, "count": 1, "message": "Created one bootstrap replica for roles [prefill]"}}
	if !b.confirmed(s) {
		t.Fatal("matching event not recognized")
	}
	b.start(l, s, Targets{"prefill": 28})
	if b.allows(l, s, "prefill", 27) {
		t.Fatal("already started C eligible")
	}
	b.start(l, before, Targets{"prefill": 27})
	if !b.allows(l, s, "prefill", 27) {
		t.Fatal("first borrow rejected")
	}
	if b.allows(l, before, "prefill", 27) {
		t.Fatal("no excess is not borrowing")
	}
	if b.allows(l, s, "prefill", 27) {
		t.Fatal("repaid exception borrowed twice")
	}
}
func TestMidpointAndGentleDownKeepOldWork(t *testing.T) {
	l := testLab(t, &fakeExecutor{})
	s := fleet(map[string]map[string]Counts{"A": {"prefill": {Spec: 19, Ready: 19}, "decode": {Spec: 10, Ready: 10}}, "B": {"prefill": {Spec: 18, Ready: 18}, "decode": {Spec: 9, Ready: 9}}}, 1)
	if !midpointVisible(l, s, initial) {
		t.Fatal("midpoint rejected")
	}
	s = fleet(map[string]map[string]Counts{"A": {"prefill": {Spec: 8, Ready: 8}, "decode": {Spec: 4, Ready: 4}}}, 1)
	if !scaleDownRetainsOld(l, s, "B", pair(19, 10)) {
		t.Fatal("gentle down rejected")
	}
	s.Pods = nil
	if scaleDownRetainsOld(l, s, "B", pair(19, 10)) {
		t.Fatal("full drain accepted")
	}
}
func TestPauseAndEOFStopWithoutMutation(t *testing.T) {
	for _, input := range []string{"q\n", ""} {
		e := &fakeExecutor{}
		l := testLab(t, e)
		l.Auto = false
		l.input = bufio.NewReader(strings.NewReader(input))
		err := catch(func() { l.Checkpoint("stop", "no event"); l.K("delete", "node", "unsafe") })
		if !errors.Is(err, context.Canceled) || len(e.calls) != 0 {
			t.Fatalf("%q: %v %v", input, err, e.calls)
		}
	}
}
func TestResourceListValidation(t *testing.T) {
	for _, input := range []string{`{}`, `null`, `{"kind":"List","items":null}`, `{"kind":"List","items":[null]}`, `{"kind":"List","items":[{}]}`, `{"kind":"PodList","items":[]}`} {
		if _, err := parseResourceList([]byte(input)); err == nil {
			t.Fatal("fabricated empty state from", input)
		}
	}
	items, err := parseResourceList([]byte(`{"kind":"List","items":[]}`))
	requireNoError(t, err)
	if items == nil || len(items) != 0 {
		t.Fatal("valid empty list missing")
	}
}
func TestObserverStartFailureDoesNotDeadlockClose(t *testing.T) {
	l := testLab(t, &fakeExecutor{})
	if catch(l.StartObserver) == nil {
		t.Fatal("missing executable accepted")
	}
	done := make(chan struct{})
	go func() { l.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Close deadlocked after observer start failure")
	}
}

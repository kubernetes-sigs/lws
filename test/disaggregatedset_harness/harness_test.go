// Copyright 2026 The Kubernetes Authors.
// SPDX-License-Identifier: Apache-2.0
package harness

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

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
	o := Options{
		RunDir:     t.TempDir(),
		Kubeconfig: "/private/kubeconfig",
		Context:    "kind-lws-v1-run-test",
		Node:       "lws-v1-run-test-control-plane",
		Auto:       true,
		Executor:   e,
		Clock:      &fakeClock{now: time.Unix(1800000000, 0)},
		Output:     io.Discard,
		Input:      strings.NewReader(""),
		Observer:   "/nonexistent-observer",
	}
	l := newLab(context.Background(), o, Case{"ordinary", "Hash", "Deferred", "default"}, nil)
	require.NoError(t, os.Mkdir(l.Directory, 0700))
	return l
}

func TestUnserializableInputFailsCaseWithoutExecutingCommand(t *testing.T) {
	executor := &fakeExecutor{}
	l := testLab(t, executor)
	err := catch(func() { l.Apply(Object{"invalid": func() {}}) })
	var unsupported *json.UnsupportedTypeError
	require.ErrorAs(t, err, &unsupported, "Apply should return a recoverable JSON encoding error")
	require.Empty(t, executor.calls, "invalid manifest invoked a command")
}

func TestAbsentSubroleCountsMatchEmptyTargets(t *testing.T) {
	for _, lws := range []Object{
		{},
		{"metadata": Object{"annotations": Object{ChildCounts: ""}}},
	} {
		require.True(t, sameTargets(annotatedCounts(lws), Targets{}), "absent subrole counts did not match empty targets: %v", lws)
	}
}

func pod(name, version, role, group, index string, isReady bool) Object {
	status := "False"
	if isReady {
		status = "True"
	}
	return Object{
		"apiVersion": "v1",
		"kind":       "Pod",
		"metadata": Object{
			"name": name,
			"uid":  "uid-" + name,
			"labels": Object{
				VersionLabel:  version,
				RoleLabel:     role,
				LWSLabel:      version + "-" + role,
				GroupLabel:    group,
				WorkerIndex:   index,
				RevisionLabel: "revision-" + version,
			},
		},
		"spec": Object{"nodeName": "owned"},
		"status": Object{
			"phase": "Running",
			"conditions": []any{
				Object{"type": "Ready", "status": status},
			},
		},
	}
}

func fleet(counts map[string]map[string]Counts, size int) Snapshot {
	s := Snapshot{
		DS: Object{
			"apiVersion": "disaggregatedset.x-k8s.io/v1",
			"kind":       "DisaggregatedSet",
			"metadata":   Object{"name": SetName, "uid": "ds-uid", "generation": 2},
			"status":     Object{"observedGeneration": 2},
		},
		LWS:     []Object{},
		Pods:    []Object{},
		Events:  []Object{},
		Scalers: []Object{},
	}
	for _, v := range keys(counts) {
		for _, r := range keys(counts[v]) {
			c := counts[v][r]
			lws := Object{
				"apiVersion": "leaderworkerset.x-k8s.io/v1",
				"kind":       "LeaderWorkerSet",
				"metadata": Object{
					"name":   v + "-" + r,
					"uid":    "uid-" + v + "-" + r,
					"labels": Object{RoleLabel: r, RevisionLabel: "revision-" + v},
				},
				"spec": RoleSpec(r, c.Spec, "Hash", v, 1, 0, size, true)["spec"],
			}
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
	}{
		{Selection{}, 26},
		{Selection{IncludeLarge: true}, 28},
		{Selection{Names: []string{"large-all"}}, 20},
		{Selection{Names: []string{"all", "large-all"}}, 46},
		{Selection{Names: []string{"all", "all"}}, 26},
	} {
		m, err := Matrix(test.s)
		require.NoError(t, err)
		require.Len(t, m, test.want, "matrix %+v", test.s)
		seen := map[Case]bool{}
		for _, c := range m {
			require.False(t, seen[c], "duplicate %+v", c)
			require.NotNil(t, scenarios[c.Name], "unimplemented %+v", c)
			seen[c] = true
		}
	}
	m, err := Matrix(Selection{Names: []string{"large-scale-down"}, Identity: "hash", Policy: "Immediate", RolloutChain: "abc"})
	require.NoError(t, err)
	require.Equal(t, []Case{{"large-scale-down", "Hash", "Immediate", "abc"}}, m)
	for _, s := range []Selection{
		{Names: []string{"unknown"}},
		{Identity: "production"},
		{Policy: "immediate"},
		{Variant: "100%"},
		{RolloutChain: "a"},
	} {
		_, err := Matrix(s)
		require.Error(t, err, "accepted %+v", s)
	}
}

func TestManifestReadinessAndBudgets(t *testing.T) {
	r := RoleSpec("prefill", 1, "Hash", "B", 0, "100%", 2, false)
	require.Equal(t, "Hash", at(r, "spec", "groupIdentity"))
	require.Equal(t, "100%", at(r, "spec", "rolloutStrategy", "rollingUpdateConfiguration", "maxUnavailable"))
	container := objects(at(r, "spec", "leaderWorkerTemplate", "workerTemplate", "spec", "containers"))[0]
	require.Equal(t, "0", objects(container["env"])[0]["value"])
	require.Equal(t, "1", at(container, "resources", "requests", Slot))
}

func TestWholeGroupCountsRequireLeaderAndWorker(t *testing.T) {
	s := fleet(map[string]map[string]Counts{
		"A": {"decode": {Spec: 1, Ready: 1, Scheduled: 1}},
	}, 2)
	require.Equal(t, 1, GroupCounts(s, "A")["decode"].Ready, "complete group missing")
	s.Pods = s.Pods[1:]
	c := GroupCounts(s, "A")["decode"]
	require.Zero(t, c.Ready, "ready worker without leader counted as group")
	require.Zero(t, c.Scheduled, "scheduled worker without leader counted as group")
}

func TestPodRevisionChangesCannotHideBehindStableLWSRevision(t *testing.T) {
	s := fleet(map[string]map[string]Counts{
		"B": {"prefill": {Spec: 1, Ready: 1, Scheduled: 1}},
	}, 1)
	workload, previous := revisions(s, "B"), podRevisions(s, "B")
	obj(at(s.Pods[0], "metadata", "labels"))[RevisionLabel] = "unexpected-revision"
	require.True(t, sameSet(workload, revisions(s, "B")), "fixture changed workload")
	require.False(t, sameSet(previous, podRevisions(s, "B")), "Pod revision-only change was hidden")
	delete(obj(at(s.Pods[0], "metadata", "labels")), RevisionLabel)
	require.Error(t, catch(func() { podRevisions(s, "B") }), "missing revision label silently accepted")
}

func TestSubroleAssignmentsRejectIncompleteOrUnpublishedState(t *testing.T) {
	base := fleet(map[string]map[string]Counts{
		"A": {"decode": {Spec: 1, Ready: 1, Scheduled: 1}},
	}, 2)
	base.LWS[0]["metadata"].(map[string]any)["annotations"] = Object{ChildCounts: `{"a":1}`}
	for _, p := range base.Pods {
		obj(at(p, "metadata", "labels"))[SubroleLabel] = "a"
	}
	require.True(t, assigned(base, Targets{"a": 1}, "A"), "valid assignment rejected")
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
		require.False(t, assigned(s, Targets{"a": 1}, "A"), "accepted %s", fault)
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
	before := fleet(map[string]map[string]Counts{
		"A": {"prefill": {Spec: 3, Ready: 3, Scheduled: 3}, "decode": {Spec: 2, Ready: 2, Scheduled: 2}},
	}, 2)
	require.NoError(t, catch(func() { handoffPreserved(l, before, before) }))
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
		require.Error(t, catch(func() { handoffPreserved(l, s, before) }), "accepted %s", fault)
	}
}

func TestReleaseCountsExistingReadyFirst(t *testing.T) {
	e := &fakeExecutor{}
	l := testLab(t, e)
	s := fleet(map[string]map[string]Counts{
		"B": {"prefill": {Spec: 3, Ready: 1, Scheduled: 3}},
	}, 1)
	obj(s.Pods[0]["metadata"])["name"] = "z-ready"
	require.NoError(t, catch(func() { l.Release("B", "prefill", 2, &s) }))
	require.Len(t, e.calls, 1)
	require.Contains(t, e.calls[0].args, name(s.Pods[1]))
	require.Equal(t, []string{"--kubeconfig", l.Options.Kubeconfig, "--context", l.Options.Context}, e.calls[0].args[:4], "ambient context used")
}

func TestUpdateExternalOnlyChangesReplicaSource(t *testing.T) {
	roles := []any{RoleSpec("prefill", 3, "Hash", "A", 1, 0, 2, true), RoleSpec("decode", 2, "Hash", "A", 1, 0, 2, true)}
	ds := Object{
		"spec": Object{"roles": roles},
	}
	e := &fakeExecutor{run: func(c command) ([]byte, []byte, error) {
		if slices.Contains(c.args, "get") {
			return []byte(jsonText(ds)), nil, nil
		}
		return nil, nil, nil
	}}
	l := testLab(t, e)
	require.NoError(t, catch(func() { l.Update(Update{ExternalRoles: []string{"prefill"}}) }))
	call := e.calls[len(e.calls)-1]
	var patch Object
	for i, a := range call.args {
		if a == "-p" {
			require.NoError(t, json.Unmarshal([]byte(call.args[i+1]), &patch))
		}
	}
	rs := objects(at(patch, "spec", "roles"))
	require.Equal(t, "External", at(rs[0], "scaling", "mode"))
	require.Nil(t, at(rs[0], "spec", "replicas"))
	require.Equal(t, 2, integer(at(rs[1], "spec", "replicas")))
	require.Equal(t, 3, integer(at(obj(roles[0]), "spec", "replicas")), "mutated input")
}

func TestConvergenceRequiresPhysicalCleanupAndAcknowledgement(t *testing.T) {
	s := fleet(map[string]map[string]Counts{
		"B": {"prefill": {Spec: 1, Ready: 1, Scheduled: 1}},
	}, 1)
	require.True(t, converged(s, "B", Targets{"prefill": 1}), "valid convergence rejected")
	old := pod("old", "A", "prefill", "old", "0", true)
	obj(old["metadata"])["deletionTimestamp"] = "now"
	s.Pods = append(s.Pods, old)
	require.False(t, converged(s, "B", Targets{"prefill": 1}), "terminating old Pod ignored")
	s.Pods = s.Pods[:1]
	obj(s.DS["status"])["observedGeneration"] = 1
	require.False(t, converged(s, "B", Targets{"prefill": 1}), "unacknowledged generation accepted")
}

func TestPercentageBounds(t *testing.T) {
	f, c := percentBounds(initial)
	require.Equal(t, pair(34, 18), f)
	require.Equal(t, pair(41, 21), c)
	f, c = percentBounds(shrunk)
	require.Equal(t, pair(26, 13), f)
	require.Equal(t, pair(31, 16), c)
}

func TestLargeCompletionRejectsUnexpectedTargetWorkload(t *testing.T) {
	s := fleet(map[string]map[string]Counts{
		"B": {"prefill": {Spec: 37, Ready: 37, Scheduled: 37}, "decode": {Spec: 19, Ready: 19, Scheduled: 19}},
	}, 1)
	require.True(t, largeConverged(s, "B", initial, false), "valid final large fleet rejected")
	extra := fleet(map[string]map[string]Counts{
		"B": {"unexpected": {Spec: 1, Ready: 1, Scheduled: 1}},
	}, 1)
	s.LWS = append(s.LWS, extra.LWS...)
	s.Pods = append(s.Pods, extra.Pods...)
	require.False(t, largeConverged(s, "B", initial, false), "extra target LWS/Pod accepted")
}

func TestBootstrapRequiresFirstGroupAndNewMatchingEvent(t *testing.T) {
	l := testLab(t, &fakeExecutor{})
	before := fleet(map[string]map[string]Counts{
		"A": {"prefill": {Spec: 11, Ready: 11, Scheduled: 11}},
		"B": {"prefill": {Spec: 16, Ready: 10, Scheduled: 16}},
	}, 1)
	s := fleet(map[string]map[string]Counts{
		"A": {"prefill": {Spec: 11, Ready: 11, Scheduled: 11}},
		"B": {"prefill": {Spec: 16, Ready: 10, Scheduled: 16}},
		"C": {"prefill": {Spec: 1, Ready: 0, Scheduled: 1}},
	}, 1)
	b := &firstGroupBootstrap{}
	b.start(l, before, Targets{"prefill": 27})
	require.Error(t, catch(func() { bounds(l, s, Targets{"prefill": 21}, Targets{"prefill": 27}, nil) }), "ordinary ceiling not strict")
	require.NoError(t, catch(func() { bounds(l, s, Targets{"prefill": 21}, Targets{"prefill": 27}, b) }))
	require.False(t, b.confirmed(s), "invented bootstrap event")
	s.Events = []Object{
		{
			"metadata":       Object{"uid": "event"},
			"reason":         "BootstrapSurge",
			"involvedObject": Object{"uid": "ds-uid"},
			"count":          1,
			"message":        "Created one bootstrap replica for roles [prefill]",
		},
	}
	require.True(t, b.confirmed(s), "matching event not recognized")
	b.start(l, s, Targets{"prefill": 28})
	require.False(t, b.allows(l, s, "prefill", 27), "already started C eligible")
	b.start(l, before, Targets{"prefill": 27})
	require.True(t, b.allows(l, s, "prefill", 27), "first borrow rejected")
	require.False(t, b.allows(l, before, "prefill", 27), "no excess is not borrowing")
	require.False(t, b.allows(l, s, "prefill", 27), "repaid exception borrowed twice")
}

func TestMidpointAndGentleDownKeepOldWork(t *testing.T) {
	l := testLab(t, &fakeExecutor{})
	s := fleet(map[string]map[string]Counts{
		"A": {"prefill": {Spec: 19, Ready: 19}, "decode": {Spec: 10, Ready: 10}},
		"B": {"prefill": {Spec: 18, Ready: 18}, "decode": {Spec: 9, Ready: 9}},
	}, 1)
	require.True(t, midpointVisible(l, s, initial), "midpoint rejected")
	s = fleet(map[string]map[string]Counts{
		"A": {"prefill": {Spec: 8, Ready: 8}, "decode": {Spec: 4, Ready: 4}},
	}, 1)
	require.True(t, scaleDownRetainsOld(l, s, "B", pair(19, 10)), "gentle down rejected")
	s.Pods = nil
	require.False(t, scaleDownRetainsOld(l, s, "B", pair(19, 10)), "full drain accepted")
}

func TestPauseAndEOFStopWithoutMutation(t *testing.T) {
	for _, input := range []string{"q\n", ""} {
		e := &fakeExecutor{}
		l := testLab(t, e)
		l.Auto = false
		l.input = bufio.NewReader(strings.NewReader(input))
		err := catch(func() {
			l.Checkpoint("stop", "no event")
			l.K("delete", "node", "unsafe")
		})
		require.ErrorIs(t, err, context.Canceled, "input %q", input)
		require.Empty(t, e.calls, "mutation after input %q", input)
	}
}

func TestResourceListValidation(t *testing.T) {
	for _, input := range []string{
		`{}`,
		`null`,
		`{"kind":"List","items":null}`,
		`{"kind":"List","items":[null]}`,
		`{"kind":"List","items":[{}]}`,
		`{"kind":"PodList","items":[]}`,
	} {
		_, err := parseResourceList([]byte(input))
		require.Error(t, err, "fabricated empty state from %s", input)
	}
	items, err := parseResourceList([]byte(`{"kind":"List","items":[]}`))
	require.NoError(t, err)
	require.NotNil(t, items, "valid empty list missing")
	require.Empty(t, items)
}

func TestObserverStartFailureDoesNotDeadlockClose(t *testing.T) {
	l := testLab(t, &fakeExecutor{})
	require.Error(t, catch(l.StartObserver), "missing executable accepted")
	done := make(chan struct{})
	go func() {
		l.Close()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Close deadlocked after observer start failure")
	}
}

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
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func ownershipFixture(t *testing.T) (Options, Object, Object, *fakeExecutor) {
	t.Helper()
	root := t.TempDir()
	cluster := "lws-v1-run-proof"
	proof := Object{
		"cluster":          cluster,
		"node":             cluster + "-control-plane",
		"nodeID":           "owned-id",
		"owner":            "run-proof",
		"controllerCommit": strings.Repeat("a", 40),
	}
	requireNoError(t, writeJSON(filepath.Join(root, "runtime.json"), proof))
	requireNoError(t, os.WriteFile(filepath.Join(root, "owner"), []byte("run-proof\n"), 0600))
	node := Object{
		"Id": "owned-id",
		"Config": Object{
			"Labels": Object{"io.x-k8s.kind.cluster": cluster},
		},
		"Mounts": []any{
			Object{"Destination": "/var/run/lws-v1-harness-owner", "Source": filepath.Join(root, "owner"), "RW": false},
		},
		"NetworkSettings": Object{
			"Ports": Object{
				"6443/tcp": []any{
					Object{"HostIp": "127.0.0.1", "HostPort": "51234"},
				},
			},
		},
	}
	config := Object{
		"current-context": "kind-" + cluster,
		"contexts": []any{
			Object{
				"name":    "kind-" + cluster,
				"context": Object{"cluster": cluster},
			},
		},
		"clusters": []any{
			Object{
				"name":    cluster,
				"cluster": Object{"server": "https://127.0.0.1:51234"},
			},
		},
	}
	e := &fakeExecutor{run: func(c command) ([]byte, []byte, error) {
		if c.program == "docker" {
			return []byte(jsonText([]Object{node})), nil, nil
		}
		if !contains(c.args, "config") {
			t.Fatal("ownership check attempted non-config Kubernetes command", c)
		}
		return []byte(jsonText(config)), nil, nil
	}}
	o := Options{
		RunDir:           root,
		Kubeconfig:       filepath.Join(root, "kubeconfig"),
		Context:          "kind-" + cluster,
		Node:             cluster + "-control-plane",
		ControllerCommit: strings.Repeat("a", 40),
		Observer:         filepath.Join(root, "bin", "observe"),
		Executor:         e,
	}
	return o, node, config, e
}

func TestOwnershipBindsExactDockerNodeAndKubeEndpoint(t *testing.T) {
	for _, fault := range []string{
		"none",
		"other-port",
		"remote",
		"node-id",
		"cluster-label",
		"mount",
		"writable",
		"owner-token",
		"extra-context",
		"private-path",
	} {
		t.Run(fault, func(t *testing.T) {
			o, node, config, e := ownershipFixture(t)
			switch fault {
			case "other-port":
				obj(at(objects(config["clusters"])[0], "cluster"))["server"] = "https://127.0.0.1:59999"
			case "remote":
				obj(at(objects(config["clusters"])[0], "cluster"))["server"] = "https://production.example:51234"
			case "node-id":
				node["Id"] = "replacement-node"
			case "cluster-label":
				obj(at(node, "Config", "Labels"))["io.x-k8s.kind.cluster"] = "different"
			case "mount":
				objects(node["Mounts"])[0]["Source"] = o.RunDir + "/other-owner"
			case "writable":
				objects(node["Mounts"])[0]["RW"] = true
			case "owner-token":
				requireNoError(t, os.WriteFile(filepath.Join(o.RunDir, "owner"), []byte("replaced"), 0600))
			case "extra-context":
				config["contexts"] = append(arr(config["contexts"]), Object{"name": "production"})
			case "private-path":
				o.Kubeconfig = "/tmp/unrelated-kubeconfig"
			}
			err := ValidateOwnership(context.Background(), o)
			if (err == nil) != (fault == "none") {
				t.Fatalf("%s: %v", fault, err)
			}
			for _, call := range e.calls {
				if contains(call.args, "delete") || contains(call.args, "patch") {
					t.Fatal("ownership check mutated", call)
				}
			}
		})
	}
}

func TestOwnsNodeOnlyExactHostTranslation(t *testing.T) {
	p := "/private/run/owner"
	for _, c := range []struct {
		os, mount, id string
		want          bool
	}{
		{"darwin", p, "id", true},
		{"darwin", "/host_mnt" + p, "id", true},
		{"linux", "/host_mnt" + p, "id", false},
		{"darwin", "/host_mnt" + p + "-other", "id", false},
		{"darwin", p, "new-id", false},
	} {
		if OwnsNode(c.id, "cluster", c.mount, "id", "cluster", p, c.os) != c.want {
			t.Fatal(c)
		}
	}
}

func TestPublicSuiteRejectsUnknownVariantBeforeFilesystemOrCommands(t *testing.T) {
	e := &fakeExecutor{}
	_, err := ExecuteSuite(context.Background(), Options{Executor: e, Output: io.Discard}, []Case{{"ordinary", "Hash", "Deferred", "../../outside"}})
	if err == nil || !strings.Contains(err.Error(), "invalid case selection") || len(e.calls) != 0 {
		t.Fatal(err, e.calls)
	}
}

func TestRestartControllerWaitsForAdmissionDryRun(t *testing.T) {
	e := &fakeExecutor{}
	calls := 0
	probeCount := 0
	e.run = func(c command) ([]byte, []byte, error) {
		calls++
		if contains(c.args, "get") {
			id := "new"
			if calls == 1 {
				id = "old"
			}
			return []byte(jsonText(Object{
				"items": []any{
					Object{
						"metadata": Object{"name": "manager-" + id, "uid": id},
						"status": Object{
							"conditions": []any{
								Object{"type": "Ready", "status": "True"},
							},
						},
					},
				},
			})), nil, nil
		}
		if contains(c.args, "patch") {
			if !contains(c.args, "--dry-run=server") {
				t.Fatal("admission probe persisted")
			}
			probeCount++
			if probeCount == 1 {
				return nil, []byte("failed calling webhook: connection refused"), fmt.Errorf("exit 1")
			}
		}
		return nil, nil, nil
	}
	l := testLab(t, e)
	requireNoError(t, catch(l.RestartController))
	if probeCount != 2 {
		t.Fatal("admission not retried", probeCount)
	}
}

func TestEmergencyRestoresExactHooksAfterCancellation(t *testing.T) {
	e := &fakeExecutor{}
	var restores []command
	e.run = func(c command) ([]byte, []byte, error) {
		if c.ctx.Err() != nil {
			return nil, nil, c.ctx.Err()
		}
		a := strings.Join(c.args, " ")
		switch {
		case strings.Contains(a, "get deployment"):
			return []byte(`{"spec":{"replicas":2}}`), nil, nil
		case strings.Contains(a, "get mutatingwebhookconfigurations"):
			return []byte(jsonText(Object{
				"items": []any{
					Object{
						"metadata": Object{"name": "mutate"},
						"webhooks": []any{
							Object{
								"name": "mleaderworkerset.kb.io",
								"namespaceSelector": Object{
									"matchLabels": Object{"keep": "original"},
								},
								"clientConfig": Object{
									"service": Object{"namespace": "lws-system"},
								},
							},
						},
					},
				},
			})), nil, nil
		case strings.Contains(a, "get validatingwebhookconfigurations"):
			return []byte(jsonText(Object{
				"items": []any{
					Object{
						"metadata": Object{"name": "validate"},
						"webhooks": []any{
							Object{
								"name": "vleaderworkerset.kb.io",
								"clientConfig": Object{
									"service": Object{"namespace": "lws-system"},
								},
							},
							Object{
								"name": "vdisaggregatedset.kb.io",
								"clientConfig": Object{
									"service": Object{"namespace": "lws-system"},
								},
							},
						},
					},
				},
			})), nil, nil
		case strings.Contains(a, "get pods"):
			return []byte(`{"items":[]}`), nil, nil
		case strings.Contains(a, "patch") || strings.Contains(a, "scale"):
			restores = append(restores, c)
		}
		return nil, nil, nil
	}
	l := testLab(t, e)
	ctx, cancel := context.WithCancel(l.ctx)
	l.ctx = ctx
	err := catch(func() {
		pausedForAdoption(l, func() {
			cancel()
			abort(context.Canceled)
		})
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if len(restores) != 6 {
		t.Fatalf("expected two exclusions, scale0, two restores, scale2; got %d", len(restores))
	}
	if !contains(restores[5].args, "--replicas=2") {
		t.Fatal("manager count not restored")
	}
	var patch []Object
	for i, a := range restores[4].args {
		if a == "-p" {
			requireNoError(t, json.Unmarshal([]byte(restores[4].args[i+1]), &patch))
		}
	}
	if len(patch) != 1 || at(obj(patch[0]["value"]), "matchLabels", "keep") != "original" {
		t.Fatal("original selector not restored", patch)
	}
}

func TestEvidenceRejectsMalformedSuccessfulRead(t *testing.T) {
	for _, body := range []string{`{}`, `null`, `{"kind":"List","items":null}`} {
		t.Run(body, func(t *testing.T) {
			root := t.TempDir()
			e, err := NewEvidence(root, "fixture")
			requireNoError(t, err)
			o := Options{
				Clock:    RealClock{},
				Executor: &fakeExecutor{run: func(command) ([]byte, []byte, error) { return []byte(body), nil, nil }},
				Kubectl:  "fake",
			}
			if err = e.Start(context.Background(), o); err == nil {
				t.Fatal("accepted malformed response")
			}
			select {
			case <-e.done:
			default:
				t.Fatal("failed Start left the acquisition goroutine running")
			}
			requireNoError(t, e.Close())
			raw, err := os.ReadFile(filepath.Join(root, "film-recording", "snapshots.jsonl"))
			requireNoError(t, err)
			var row Object
			requireNoError(t, json.Unmarshal([]byte(strings.Split(strings.TrimSpace(string(raw)), "\n")[0]), &row))
			if row["state"] != nil || row["error"] == nil {
				t.Fatal("fabricated state", row)
			}
		})
	}
}

func TestEvidenceStartDefaultsAndCancellation(t *testing.T) {
	t.Run("default clock", func(t *testing.T) {
		e, err := NewEvidence(t.TempDir(), "fixture")
		requireNoError(t, err)
		executor := &fakeExecutor{run: func(command) ([]byte, []byte, error) {
			return []byte(`{"kind":"List","items":[]}`), nil, nil
		}}
		requireNoError(t, e.Start(t.Context(), Options{Executor: executor}))
		requireNoError(t, e.Close())
		if len(executor.calls) == 0 || executor.calls[0].program != "kubectl" {
			t.Fatal("default kubectl was not used", executor.calls)
		}
	})
	t.Run("default executor", func(t *testing.T) {
		e, err := NewEvidence(t.TempDir(), "fixture")
		requireNoError(t, err)
		err = e.Start(t.Context(), Options{Kubectl: filepath.Join(t.TempDir(), "missing-kubectl")})
		if err == nil {
			t.Fatal("missing executable did not fail initial acquisition")
		}
		select {
		case <-e.done:
		default:
			t.Fatal("failed Start left the acquisition goroutine running")
		}
		requireNoError(t, e.Close())
	})
	t.Run("cancellation joins acquisition", func(t *testing.T) {
		e, err := NewEvidence(t.TempDir(), "fixture")
		requireNoError(t, err)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		executor := &fakeExecutor{run: func(c command) ([]byte, []byte, error) {
			cancel()
			<-c.ctx.Done()
			return nil, nil, c.ctx.Err()
		}}
		if err = e.Start(ctx, Options{Executor: executor}); !errors.Is(err, context.Canceled) {
			t.Fatalf("Start returned %v, want cancellation", err)
		}
		select {
		case <-e.done:
		default:
			t.Fatal("canceled Start returned before acquisition stopped")
		}
		requireNoError(t, e.Close())
	})
}

func TestEmergencyRestoresAfterUnexpectedPanic(t *testing.T) {
	e := &fakeExecutor{}
	e.run = func(c command) ([]byte, []byte, error) {
		if contains(c.args, "get") && contains(c.args, "deployment") {
			return []byte(`{"spec":{"replicas":2}}`), nil, nil
		}
		if contains(c.args, "get") && contains(c.args, "mutatingwebhookconfigurations") {
			webhooks := []any{}
			for _, name := range []string{"mleaderworkerset.kb.io", "vleaderworkerset.kb.io", "vdisaggregatedset.kb.io"} {
				webhooks = append(webhooks, Object{
					"name": name,
					"namespaceSelector": Object{
						"matchLabels": Object{"keep": "original"},
					},
					"clientConfig": Object{
						"service": Object{"namespace": "lws-system"},
					},
				})
			}
			return []byte(jsonText(Object{
				"items": []any{
					Object{
						"metadata": Object{"name": "fixture"},
						"webhooks": webhooks,
					},
				},
			})), nil, nil
		}
		return []byte(`{"items":[]}`), nil, nil
	}
	l := testLab(t, e)
	var propagated any
	func() {
		defer func() { propagated = recover() }()
		pausedForAdoption(l, func() { panic("unexpected fixture panic") })
	}()
	if propagated != "unexpected fixture panic" {
		t.Fatal("unexpected panic not preserved", propagated)
	}
	patches := []command{}
	restored := false
	for _, call := range e.calls {
		if contains(call.args, "patch") {
			patches = append(patches, call)
		}
		restored = restored || contains(call.args, "--replicas=2")
	}
	if len(patches) != 2 || !restored {
		t.Fatal("panic bypassed restoration", e.calls)
	}
	var undo []Object
	for i, arg := range patches[1].args {
		if arg == "-p" {
			requireNoError(t, json.Unmarshal([]byte(patches[1].args[i+1]), &undo))
		}
	}
	if len(undo) != 3 {
		t.Fatal(undo)
	}
	for _, patch := range undo {
		if at(obj(patch["value"]), "matchLabels", "keep") != "original" {
			t.Fatal("selector not restored exactly", patch)
		}
	}
}

func TestObserverFailureClearsCredit(t *testing.T) {
	l := testLab(t, &fakeExecutor{})
	l.diagnostic = Object{
		"slices": []any{
			Object{"slice": "0"},
		},
	}
	if !l.HasDiagnostic() {
		t.Fatal("fixture missing")
	}
	l.observerFailed(fmt.Errorf("unexpected exit"))
	if catch(func() { l.HasDiagnostic() }) == nil {
		t.Fatal("stale diagnostic survived failure")
	}
	if l.Close() == nil {
		t.Fatal("observer failure lost on close")
	}
}

func TestInteractiveCheckpointCancelsWhileInputBlocked(t *testing.T) {
	executor := &fakeExecutor{}
	l := testLab(t, executor)
	reader, writer := io.Pipe()
	defer reader.Close()
	defer writer.Close()
	l.input = bufio.NewReader(reader)
	l.Auto = false
	ctx, cancel := context.WithCancel(l.ctx)
	l.ctx = ctx
	done := make(chan error, 1)
	go func() {
		done <- catch(func() {
			l.Checkpoint("Awaiting user input", "No mutation yet")
			l.K("delete", "unsafe")
		})
	}()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancellation blocked on stdin")
	}
	if len(executor.calls) > 0 {
		t.Fatal("mutation after cancelled checkpoint")
	}
}

func TestWaitCancellationAndStableSamples(t *testing.T) {
	e := &fakeExecutor{
		run: func(command) ([]byte, []byte, error) { return []byte(`{"kind":"List","items":[]}`), nil, nil },
	}
	l := testLab(t, e)
	hits := 0
	requireNoError(t, catch(func() {
		l.Wait("stable", func(Snapshot) bool {
			hits++
			return hits != 2
		}, 10*time.Second, 3, nil)
	}))
	if hits != 5 {
		t.Fatal("stable count not reset", hits)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	l.ctx = ctx
	before := len(e.calls)
	if err := catch(func() { l.WaitFor("cancel", func(Snapshot) bool { return false }) }); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if len(e.calls) != before {
		t.Fatal("read after cancel")
	}
}

// This optional test fixture is created only in a caller-chosen new directory,
// outside the repository. The Node normalizer can independently consume it to
// verify cross-language compatibility.
func TestEvidenceActionShapesAndPortableFixture(t *testing.T) {
	root := t.TempDir()
	if requested := os.Getenv("LWS_GO_EVIDENCE_FIXTURE"); requested != "" {
		requireNoError(t, os.Mkdir(requested, 0700))
		root = requested
	}
	e, err := NewEvidence(root, strings.Repeat("a", 40))
	requireNoError(t, err)
	c := Case{"ordinary", "Hash", "Deferred", "default"}
	e.Case(c.ID())
	executor := &fakeExecutor{}
	l := newLab(context.Background(), Options{
		RunDir:   root,
		Auto:     true,
		Executor: executor,
		Clock:    &fakeClock{now: time.Unix(1800000000, 0)},
		Output:   io.Discard,
	}, c, e)
	requireNoError(t, os.Mkdir(l.Directory, 0700))
	state := fleet(map[string]map[string]Counts{
		"A": {"prefill": {Spec: 2, Ready: 2, Scheduled: 2}, "decode": {Spec: 4, Ready: 4, Scheduled: 4}},
	}, 1)
	state.DS["spec"] = Object{
		"roles": []any{RoleSpec("prefill", 2, "Hash", "A", 1, 0, 1, true), RoleSpec("decode", 4, "Hash", "A", 1, 0, 1, true)},
	}
	executor.run = func(c command) ([]byte, []byte, error) {
		if contains(c.args, "get") {
			if contains(c.args, "node") {
				return []byte(jsonText(Object{
					"status": Object{
						"allocatable": Object{Slot: "64"},
					},
				})), nil, nil
			}
			return []byte(jsonText(state.DS)), nil, nil
		}
		return nil, nil, nil
	}
	e.Emit("actions.jsonl", Object{"id": c.ID(), "kind": "case", "phase": "start", "time": unix(l.clock.Now())})
	e.Emit("snapshots.jsonl", Object{
		"startedAt":       unix(l.clock.Now()),
		"finishedAt":      unix(l.clock.Now().Add(10 * time.Millisecond)),
		"returncode":      0,
		"state":           state,
		"eventsRequested": true,
	})
	l.Sleep(time.Second)
	requireNoError(t, catch(func() {
		l.Checkpoint("Change demand", "Update replicas, keep template A.")
		l.Update(Update{Counts: pair(3, 5)})
		l.SetSlots(64)
		l.Release("A", "prefill", 1, &state)
		l.Note("Native Go offline capture fixture; not a real cluster recording.")
		l.Assert(true, "Capacity dropped below its floor")
	}))
	l.Sleep(time.Second)
	e.Emit("snapshots.jsonl", Object{
		"startedAt":       unix(l.clock.Now()),
		"finishedAt":      unix(l.clock.Now().Add(10 * time.Millisecond)),
		"returncode":      0,
		"state":           state,
		"eventsRequested": true,
	})
	result := Result{
		Case:             c.DisplayName(),
		Identity:         c.Identity,
		Policy:           c.Policy,
		Passed:           true,
		ControllerCommit: strings.Repeat("a", 40),
		Checks:           []string{"fixture"},
	}
	e.Emit("actions.jsonl", Object{"id": c.ID(), "kind": "case", "phase": "end", "time": unix(l.clock.Now()), "result": result})
	requireNoError(t, writeJSON(filepath.Join(root, "results.json"), []Result{result}))
	requireNoError(t, e.Close())
	raw, err := os.ReadFile(filepath.Join(root, "film-recording", "actions.jsonl"))
	requireNoError(t, err)
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		var row Object
		requireNoError(t, json.Unmarshal([]byte(line), &row))
		if row["method"] != nil {
			if _, ok := row["args"].([]any); !ok {
				t.Fatal("args not array", row)
			}
			if _, ok := row["kwargs"].(map[string]any); !ok {
				t.Fatal("kwargs not object", row)
			}
		}
	}
}

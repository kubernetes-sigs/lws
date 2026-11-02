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
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
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
	require.NoError(t, writeJSON(filepath.Join(root, "runtime.json"), proof))
	require.NoError(t, os.WriteFile(filepath.Join(root, "owner"), []byte("run-proof\n"), 0600))
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
		require.Contains(t, c.args, "config", "ownership check attempted non-config Kubernetes command")
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
				require.NoError(t, os.WriteFile(filepath.Join(o.RunDir, "owner"), []byte("replaced"), 0600))
			case "extra-context":
				config["contexts"] = append(arr(config["contexts"]), Object{"name": "production"})
			case "private-path":
				o.Kubeconfig = "/tmp/unrelated-kubeconfig"
			}
			err := ValidateOwnership(context.Background(), o)
			require.Equal(t, fault == "none", err == nil, "%s: %v", fault, err)
			for _, call := range e.calls {
				require.NotContains(t, call.args, "delete", "ownership check mutated")
				require.NotContains(t, call.args, "patch", "ownership check mutated")
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
		require.Equal(t, c.want, OwnsNode(c.id, "cluster", c.mount, "id", "cluster", p, c.os), "%+v", c)
	}
}

func TestPublicSuiteRejectsUnknownVariantBeforeFilesystemOrCommands(t *testing.T) {
	e := &fakeExecutor{}
	_, err := ExecuteSuite(context.Background(), Options{Executor: e, Output: io.Discard}, []Case{{"ordinary", "Hash", "Deferred", "../../outside"}})
	require.ErrorContains(t, err, "invalid case selection")
	require.Empty(t, e.calls)
}

func TestRestartControllerWaitsForAdmissionDryRun(t *testing.T) {
	e := &fakeExecutor{}
	calls := 0
	probeCount := 0
	e.run = func(c command) ([]byte, []byte, error) {
		calls++
		if slices.Contains(c.args, "get") {
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
		if slices.Contains(c.args, "patch") {
			require.Contains(t, c.args, "--dry-run=server", "admission probe persisted")
			probeCount++
			if probeCount == 1 {
				return nil, []byte("failed calling webhook: connection refused"), fmt.Errorf("exit 1")
			}
		}
		return nil, nil, nil
	}
	l := testLab(t, e)
	require.NoError(t, catch(l.RestartController))
	require.Equal(t, 2, probeCount, "admission not retried")
}

func TestEmergencyRestoresExactHooksAfterFailure(t *testing.T) {
	for _, mode := range []string{"cancellation", "panic"} {
		t.Run(mode, func(t *testing.T) {
			e := &fakeExecutor{run: func(c command) ([]byte, []byte, error) {
				if c.ctx.Err() != nil {
					return nil, nil, c.ctx.Err()
				}
				if slices.Contains(c.args, "get") {
					if slices.Contains(c.args, "deployment") {
						return []byte(`{"spec":{"replicas":2}}`), nil, nil
					}
					var names []string
					if slices.Contains(c.args, "mutatingwebhookconfigurations") {
						names = []string{"mleaderworkerset.kb.io"}
					} else if slices.Contains(c.args, "validatingwebhookconfigurations") {
						names = []string{"vleaderworkerset.kb.io", "vdisaggregatedset.kb.io"}
					}
					if len(names) > 0 {
						webhooks := []any{}
						for _, name := range names {
							hook := Object{
								"name": name,
								"clientConfig": Object{
									"service": Object{"namespace": "lws-system"},
								},
							}
							if name == "mleaderworkerset.kb.io" {
								hook["namespaceSelector"] = Object{"matchLabels": Object{"keep": "original"}}
							}
							webhooks = append(webhooks, hook)
						}
						return []byte(jsonText(Object{
							"items": []any{
								Object{"metadata": Object{"name": names[0]}, "webhooks": webhooks},
							},
						})), nil, nil
					}
				}
				return []byte(`{"items":[]}`), nil, nil
			}}
			l := testLab(t, e)
			ctx, cancel := context.WithCancel(l.ctx)
			defer cancel()
			l.ctx = ctx
			run := func() {
				pausedForAdoption(l, func() {
					if mode == "panic" {
						panic("unexpected fixture panic")
					}
					cancel()
					abort(context.Canceled)
				})
			}
			if mode == "panic" {
				require.PanicsWithValue(t, "unexpected fixture panic", run)
			} else {
				require.ErrorIs(t, catch(run), context.Canceled, "cancellation not preserved")
			}
			var mutations []command
			for _, call := range e.calls {
				if slices.Contains(call.args, "patch") || slices.Contains(call.args, "scale") {
					mutations = append(mutations, call)
				}
			}
			require.Len(t, mutations, 6, "expected two exclusions, scale0, two restores, scale2")
			require.Contains(t, mutations[5].args, "--replicas=2", "manager count not restored")
			for index, want := range map[int][]Object{
				3: {
					{"op": "remove", "path": "/webhooks/0/namespaceSelector"},
					{"op": "remove", "path": "/webhooks/1/namespaceSelector"},
				},
				4: {{
					"op":    "add",
					"path":  "/webhooks/0/namespaceSelector",
					"value": Object{"matchLabels": Object{"keep": "original"}},
				}},
			} {
				var patch []Object
				for i, arg := range mutations[index].args {
					if arg == "-p" {
						require.NoError(t, json.Unmarshal([]byte(mutations[index].args[i+1]), &patch))
					}
				}
				require.Equal(t, want, patch, "original selectors not restored")
			}
		})
	}
}

func TestEvidenceRejectsMalformedSuccessfulRead(t *testing.T) {
	root := t.TempDir()
	e, err := NewEvidence(root, "fixture")
	require.NoError(t, err)
	o := Options{Executor: &fakeExecutor{run: func(command) ([]byte, []byte, error) {
		return []byte(`{"kind":"List","items":null}`), nil, nil
	}}}
	require.Error(t, e.Start(context.Background(), o), "accepted malformed response")
	select {
	case <-e.done:
	default:
		t.Fatal("failed Start left the acquisition goroutine running")
	}
	require.NoError(t, e.Close())
	raw, err := os.ReadFile(filepath.Join(root, "film-recording", "snapshots.jsonl"))
	require.NoError(t, err)
	var row Object
	require.NoError(t, json.NewDecoder(strings.NewReader(string(raw))).Decode(&row))
	require.Nil(t, row["state"], "fabricated state")
	require.NotNil(t, row["error"], "missing acquisition error")
}

func TestEvidenceStartDefaultsAndCancellation(t *testing.T) {
	t.Run("default clock", func(t *testing.T) {
		e, err := NewEvidence(t.TempDir(), "fixture")
		require.NoError(t, err)
		executor := &fakeExecutor{run: func(command) ([]byte, []byte, error) {
			return []byte(`{"kind":"List","items":[]}`), nil, nil
		}}
		require.NoError(t, e.Start(t.Context(), Options{Executor: executor}))
		require.NoError(t, e.Close())
		require.NotEmpty(t, executor.calls)
		require.Equal(t, "kubectl", executor.calls[0].program, "default kubectl was not used")
	})
	t.Run("default executor", func(t *testing.T) {
		e, err := NewEvidence(t.TempDir(), "fixture")
		require.NoError(t, err)
		require.Error(t, e.Start(t.Context(), Options{Kubectl: filepath.Join(t.TempDir(), "missing-kubectl")}), "missing executable did not fail initial acquisition")
		select {
		case <-e.done:
		default:
			t.Fatal("failed Start left the acquisition goroutine running")
		}
		require.NoError(t, e.Close())
	})
	t.Run("cancellation joins acquisition", func(t *testing.T) {
		e, err := NewEvidence(t.TempDir(), "fixture")
		require.NoError(t, err)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		executor := &fakeExecutor{run: func(c command) ([]byte, []byte, error) {
			cancel()
			<-c.ctx.Done()
			return nil, nil, c.ctx.Err()
		}}
		require.ErrorIs(t, e.Start(ctx, Options{Executor: executor}), context.Canceled)
		select {
		case <-e.done:
		default:
			t.Fatal("canceled Start returned before acquisition stopped")
		}
		require.NoError(t, e.Close())
	})
}

func TestObserverFailureClearsCredit(t *testing.T) {
	l := testLab(t, &fakeExecutor{})
	l.diagnostic = Object{
		"slices": []any{
			Object{"slice": "0"},
		},
	}
	require.True(t, l.HasDiagnostic(), "fixture missing")
	l.observerFailed(fmt.Errorf("unexpected exit"))
	require.Error(t, catch(func() { l.HasDiagnostic() }), "stale diagnostic survived failure")
	require.Error(t, l.Close(), "observer failure lost on close")
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
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(time.Second):
		t.Fatal("cancellation blocked on stdin")
	}
	require.Empty(t, executor.calls, "mutation after cancelled checkpoint")
}

func TestWaitCancellationAndStableSamples(t *testing.T) {
	e := &fakeExecutor{
		run: func(command) ([]byte, []byte, error) { return []byte(`{"kind":"List","items":[]}`), nil, nil },
	}
	l := testLab(t, e)
	hits := 0
	require.NoError(t, catch(func() {
		l.Wait("stable", func(Snapshot) bool {
			hits++
			return hits != 2
		}, 10*time.Second, 3, nil)
	}))
	require.Equal(t, 5, hits, "stable count not reset")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	l.ctx = ctx
	before := len(e.calls)
	require.ErrorIs(t, catch(func() { l.WaitFor("cancel", func(Snapshot) bool { return false }) }), context.Canceled)
	require.Len(t, e.calls, before, "read after cancel")
}

// This optional test fixture is created only in a caller-chosen new directory,
// outside the repository. The Node normalizer can independently consume it to
// verify cross-language compatibility.
func TestEvidenceActionShapesAndPortableFixture(t *testing.T) {
	root := t.TempDir()
	if requested := os.Getenv("LWS_GO_EVIDENCE_FIXTURE"); requested != "" {
		require.NoError(t, os.Mkdir(requested, 0700))
		root = requested
	}
	e, err := NewEvidence(root, strings.Repeat("a", 40))
	require.NoError(t, err)
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
	require.NoError(t, os.Mkdir(l.Directory, 0700))
	state := fleet(map[string]map[string]Counts{
		"A": {"prefill": {Spec: 2, Ready: 2, Scheduled: 2}, "decode": {Spec: 4, Ready: 4, Scheduled: 4}},
	}, 1)
	state.DS["spec"] = Object{
		"roles": []any{RoleSpec("prefill", 2, "Hash", "A", 1, 0, 1, true), RoleSpec("decode", 4, "Hash", "A", 1, 0, 1, true)},
	}
	executor.run = func(c command) ([]byte, []byte, error) {
		if slices.Contains(c.args, "get") {
			if slices.Contains(c.args, "node") {
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
	require.NoError(t, catch(func() {
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
	require.NoError(t, writeJSON(filepath.Join(root, "results.json"), []Result{result}))
	require.NoError(t, e.Close())
	raw, err := os.ReadFile(filepath.Join(root, "film-recording", "actions.jsonl"))
	require.NoError(t, err)
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		var row Object
		require.NoError(t, json.Unmarshal([]byte(line), &row))
		if row["method"] != nil {
			require.IsType(t, []any{}, row["args"], "args not array")
			require.IsType(t, map[string]any{}, row["kwargs"], "kwargs not object")
		}
	}
}

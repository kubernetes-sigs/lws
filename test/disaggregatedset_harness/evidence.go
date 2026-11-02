// Copyright 2026 The Kubernetes Authors.
// SPDX-License-Identifier: Apache-2.0
package harness

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Evidence writes the raw-capture schema consumed by the browser player's Node
// normalizer. Raw Kubernetes objects remain local and need review before sharing.
// Periodic snapshots are independent acquisition windows, not controller decisions.
type Evidence struct {
	mu                sync.Mutex
	directory, caseID string
	files             map[string]*os.File
	seq               map[string]int
	err               error
	cancel            context.CancelFunc
	done              chan struct{}
}

func NewEvidence(runDir, commit string) (*Evidence, error) {
	dir := filepath.Join(runDir, "film-recording")
	if err := os.Mkdir(dir, 0700); err != nil {
		return nil, err
	}
	e := &Evidence{directory: dir, files: map[string]*os.File{}, seq: map[string]int{}}
	err := writeJSON(filepath.Join(dir, "metadata.json"), Object{"schemaVersion": 2, "startedAt": unix(time.Now()), "controllerCommit": commit, "periodTargetSeconds": 1, "snapshotEventsRequested": true, "snapshotEventsSource": "same-snapshot-request", "producer": "native-go-harness", "caveats": []string{"API snapshots are non-atomic acquisition windows, not controller decisions.", "Native Go capture records complete periodic API snapshots, actions, checks and independent observer bounds. It does not claim continuous LIST/watch or controller log coverage.", "Read errors are explicit; failed acquisition never fabricates empty resources.", "Assertion messages describe failures; passed=true means the asserted failure was not observed."}})
	return e, err
}
func unix(t time.Time) float64          { return float64(t.UnixNano()) / 1e9 }
func (e *Evidence) Case(id string)      { e.mu.Lock(); defer e.mu.Unlock(); e.caseID = id }
func (e *Evidence) CurrentCase() string { e.mu.Lock(); defer e.mu.Unlock(); return e.caseID }
func (e *Evidence) Emit(stream string, row Object) {
	if e == nil {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.err != nil {
		return
	}
	f := e.files[stream]
	if f == nil {
		var err error
		f, err = os.OpenFile(filepath.Join(e.directory, stream), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
		if err != nil {
			e.err = err
			return
		}
		e.files[stream] = f
	}
	e.seq[stream]++
	row["seq"] = e.seq[stream]
	if _, ok := row["caseId"]; !ok {
		row["caseId"] = e.caseID
	}
	e.err = json.NewEncoder(f).Encode(row)
}
func (e *Evidence) Error() error {
	if e == nil {
		return nil
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.err
}
func (e *Evidence) Start(ctx context.Context, o Options) error {
	o.defaults()
	ctx, e.cancel = context.WithCancel(ctx)
	e.done = make(chan struct{})
	first := make(chan error, 1)
	go func() {
		defer close(e.done)
		initial := true
		var previousStart, previousEnd time.Time
		for ctx.Err() == nil {
			began := o.Clock.Now()
			id := e.CurrentCase()
			call, cancel := context.WithTimeout(ctx, 8*time.Second)
			args := o.kube("-n", Namespace, "get", DSResource+",lws,pods,dsrs,events", "-o", "json")
			out, stderr, err := o.Executor.Run(call, o.Kubectl, args, nil)
			cancel()
			ended := o.Clock.Now()
			row := Object{"caseId": id, "startedAt": unix(began), "finishedAt": unix(ended), "durationSeconds": ended.Sub(began).Seconds(), "periodTargetSeconds": 1, "eventsRequested": true, "eventsSource": "same-snapshot-request", "argv": append([]string{o.Kubectl}, args...), "stdout": string(out), "stderr": string(stderr), "returncode": 0}
			if !previousStart.IsZero() {
				row["startGapSeconds"] = began.Sub(previousStart).Seconds()
				row["previousIdleGapSeconds"] = began.Sub(previousEnd).Seconds()
			}
			if err == nil {
				var items []Object
				items, err = parseResourceList(out)
				if err == nil {
					row["state"] = stateOf(items)
				}
			}
			if err != nil {
				row["returncode"] = nil
				row["error"] = err.Error()
			}
			e.Emit("snapshots.jsonl", row)
			if initial {
				first <- err
				initial = false
			}
			previousStart, previousEnd = began, ended
			if o.Clock.Sleep(ctx, max(time.Millisecond, time.Second-ended.Sub(began))) != nil {
				return
			}
		}
	}()
	select {
	case err := <-first:
		if err != nil {
			e.cancel()
			<-e.done
			return fmt.Errorf("initial evidence acquisition failed: %w", err)
		}
		return nil
	case <-ctx.Done():
		e.cancel()
		<-e.done
		return ctx.Err()
	}
}

// A successful process and syntactically valid JSON are not enough evidence:
// missing/null items must never be reinterpreted as an empty Kubernetes list.
func parseResourceList(data []byte) ([]Object, error) {
	var list Object
	if err := json.Unmarshal(data, &list); err != nil {
		return nil, err
	}
	if str(list["kind"]) != "List" {
		return nil, fmt.Errorf("expected a Kubernetes List response")
	}
	items, ok := list["items"].([]any)
	if !ok {
		return nil, fmt.Errorf("Kubernetes List response has no items array")
	}
	out := make([]Object, 0, len(items))
	for _, item := range items {
		o, ok := item.(map[string]any)
		if !ok || str(o["kind"]) == "" || obj(o["metadata"]) == nil {
			return nil, fmt.Errorf("Kubernetes List contains an invalid resource object")
		}
		out = append(out, o)
	}
	return out, nil
}
func (e *Evidence) Close() error {
	if e == nil {
		return nil
	}
	if e.cancel != nil {
		e.cancel()
		<-e.done
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, f := range e.files {
		if err := f.Close(); err != nil && e.err == nil {
			e.err = err
		}
	}
	return e.err
}
func stateOf(items []Object) Snapshot {
	s := Snapshot{LWS: []Object{}, Pods: []Object{}, Events: []Object{}, Scalers: []Object{}}
	for _, o := range items {
		switch o["kind"] {
		case "DisaggregatedSet":
			s.DS = o
		case "LeaderWorkerSet":
			s.LWS = append(s.LWS, o)
		case "Pod":
			s.Pods = append(s.Pods, o)
		case "Event":
			s.Events = append(s.Events, o)
		case "DisaggregatedSetRoleScaler":
			s.Scalers = append(s.Scalers, o)
		}
	}
	return s
}

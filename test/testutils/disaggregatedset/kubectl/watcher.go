/*
Copyright 2025 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package kubectl

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"maps"
	"os/exec"
	"strconv"
	"strings"
	"sync"
)

// LWSEvent is one LWS state observed by an LWSWatcher.
type LWSEvent struct {
	Revision string
	Role     string
	Spec     int
}

// LWSWatcher watches LWS resources for a DisaggregatedSet via a
// `kubectl get --watch --output-watch-events` subprocess. Every event
// (ADDED, MODIFIED, DELETED) is captured in an in-memory history. Used by
// drain-order tests to observe every state transition
// (including sub-millisecond transients that external polling misses).
type LWSWatcher struct {
	ctx     context.Context
	cancel  context.CancelFunc
	cmd     *exec.Cmd
	mu      sync.Mutex
	history []LWSEvent
	scanErr error
	stderr  *bytes.Buffer
	done    chan struct{}
}

// NewLWSWatcher starts a watcher on LWSes labeled with the given DisaggregatedSet
// name. kubectl performs one consistent list/watch operation: existing objects
// are emitted as initial ADDED events, and subsequent changes are watched from
// the list's resource version. The watcher captures events until Stop is called.
func NewLWSWatcher(deploymentName string) (*LWSWatcher, error) {
	// custom-columns pulls revision label, role label, and spec.replicas from
	// each event's object.  The label key contains dots that we escape with
	// backslash so kubectl's jsonpath doesn't treat them as field traversals.
	args := []string{
		"get", "lws",
		"-l", labelName + "=" + deploymentName,
		"-n", defaultNS,
		"--watch",
		"--output-watch-events=true",
		"-o", `custom-columns=EVENT:.type,REV:.object.metadata.labels['disaggregatedset\.x-k8s\.io/revision'],ROLE:.object.metadata.labels['disaggregatedset\.x-k8s\.io/role'],SPEC:.object.spec.replicas`,
		"--no-headers",
	}
	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, "kubectl", args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("watcher pipe: %w", err)
	}
	stderr := &bytes.Buffer{}
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		cancel()
		return nil, fmt.Errorf("watcher start: %w", err)
	}

	w := &LWSWatcher{
		ctx: ctx, cancel: cancel, cmd: cmd, stderr: stderr, done: make(chan struct{}),
	}
	go w.readLoop(stdout)
	return w, nil
}

func (w *LWSWatcher) readLoop(stdout io.Reader) {
	defer close(w.done)
	scanner := bufio.NewScanner(stdout)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		event, err := parseLWSEvent(line)
		if err != nil {
			w.setReadError(err)
			return
		}
		w.mu.Lock()
		w.history = append(w.history, event)
		w.mu.Unlock()
	}
	if err := scanner.Err(); err != nil && w.ctx.Err() == nil {
		w.setReadError(fmt.Errorf("read LWS watch: %w", err))
		return
	}
	if w.ctx.Err() == nil {
		w.setReadError(io.ErrUnexpectedEOF)
	}
}

func parseLWSEvent(line string) (LWSEvent, error) {
	// Format: "EVENT REV ROLE SPEC" (whitespace-separated).
	parts := strings.Fields(line)
	if len(parts) != 4 {
		return LWSEvent{}, fmt.Errorf("unexpected LWS watch event %q", line)
	}
	if parts[0] != "ADDED" && parts[0] != "MODIFIED" && parts[0] != "DELETED" {
		return LWSEvent{}, fmt.Errorf("unexpected LWS watch event type %q", parts[0])
	}
	spec, err := strconv.Atoi(parts[3])
	if err != nil {
		return LWSEvent{}, fmt.Errorf("parse LWS watch spec %q: %w", parts[3], err)
	}
	if parts[0] == "DELETED" {
		spec = 0
	}
	return LWSEvent{Revision: parts[1], Role: parts[2], Spec: spec}, nil
}

func (w *LWSWatcher) setReadError(err error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.scanErr == nil {
		w.scanErr = err
	}
}

// Stop terminates the kubectl subprocess, waits for the read loop, and returns
// any parsing or watch-process failure.
func (w *LWSWatcher) Stop() error {
	w.cancel()
	<-w.done
	_ = w.cmd.Wait()

	w.mu.Lock()
	readErr := w.scanErr
	w.mu.Unlock()
	if readErr != nil {
		stderr := strings.TrimSpace(w.stderr.String())
		if stderr != "" {
			return fmt.Errorf("LWS watcher: %w: %s", readErr, stderr)
		}
		return readErr
	}
	return nil
}

// Err reports a parser or unexpected watch-stream failure without stopping
// the watcher. Callers that poll watcher history can use it to fail promptly
// instead of waiting for their observation timeout.
func (w *LWSWatcher) Err() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.scanErr
}

// Events returns a snapshot of the captured event history.
func (w *LWSWatcher) Events() []LWSEvent {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make([]LWSEvent, len(w.history))
	copy(out, w.history)
	return out
}

// SawDrainedFirst returns true iff at some point in the captured history
// `firstRev`'s total spec (summed across roles) reached 0 while `secondRev`'s
// total spec was still > 0.
//
// Deletion events (kubectl -o custom-columns with --output-watch-events)
// still emit spec value: for a delete of an LWS with spec=0, the DELETED
// event carries spec=0, which is the correct contribution to the sum.
func (w *LWSWatcher) SawDrainedFirst(firstRev, secondRev string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return sawDrainedFirst(w.history, firstRev, secondRev)
}

func sawDrainedFirst(history []LWSEvent, firstRev, secondRev string) bool {
	// Determine the complete role sets before replaying. Otherwise matching
	// partial sets could produce a false positive before a later event reveals
	// that one revision had an additional role.
	expectedRoles := map[string]map[string]bool{
		firstRev:  {},
		secondRev: {},
	}
	for _, ev := range history {
		if ev.Revision == firstRev || ev.Revision == secondRev {
			expectedRoles[ev.Revision][ev.Role] = true
		}
	}
	if len(expectedRoles[firstRev]) == 0 || !maps.Equal(expectedRoles[firstRev], expectedRoles[secondRev]) {
		return false
	}

	// Cumulative spec per (revision, role) as we replay events chronologically.
	specByRevRole := map[string]map[string]int{
		firstRev:  {},
		secondRev: {},
	}
	seenRoles := map[string]map[string]bool{
		firstRev:  {},
		secondRev: {},
	}
	firstWasPositive := false
	for _, ev := range history {
		if ev.Revision != firstRev && ev.Revision != secondRev {
			continue
		}
		seenRoles[ev.Revision][ev.Role] = true
		specByRevRole[ev.Revision][ev.Role] = ev.Spec
		firstTotal := sumMap(specByRevRole[firstRev])
		secondTotal := sumMap(specByRevRole[secondRev])
		if firstTotal > 0 {
			firstWasPositive = true
		}
		if maps.Equal(seenRoles[firstRev], expectedRoles[firstRev]) &&
			maps.Equal(seenRoles[secondRev], expectedRoles[secondRev]) &&
			firstWasPositive && firstTotal == 0 && secondTotal > 0 {
			return true
		}
	}
	return false
}

func sumMap(m map[string]int) int {
	total := 0
	for _, v := range m {
		total += v
	}
	return total
}

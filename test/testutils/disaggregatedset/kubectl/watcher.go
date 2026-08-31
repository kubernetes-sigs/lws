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
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

// LWSEvent is one MODIFIED / ADDED / DELETED event observed by an LWSWatcher.
type LWSEvent struct {
	Timestamp time.Time
	EventType string // ADDED, MODIFIED, DELETED
	Revision  string
	Role      string
	Spec      int
}

// LWSWatcher watches LWS resources for a DisaggregatedSet via a
// `kubectl get --watch --output-watch-events` subprocess. Every event
// (ADDED, MODIFIED, DELETED) is captured with a timestamp into an in-memory
// history. Used by drain-order tests to observe every state transition
// (including sub-millisecond transients that external polling misses).
type LWSWatcher struct {
	cmd      *exec.Cmd
	stdout   io.ReadCloser
	mu       sync.Mutex
	history  []LWSEvent
	scanErr  error
	stopping bool
	stderr   *bytes.Buffer
	done     chan struct{}
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
	cmd := exec.Command("kubectl", args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("watcher pipe: %w", err)
	}
	stderr := &bytes.Buffer{}
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("watcher start: %w", err)
	}

	w := &LWSWatcher{
		cmd:    cmd,
		stdout: stdout,
		stderr: stderr,
		done:   make(chan struct{}),
	}
	go w.readLoop()
	return w, nil
}

func (w *LWSWatcher) readLoop() {
	defer close(w.done)
	scanner := bufio.NewScanner(w.stdout)
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
	if err := scanner.Err(); err != nil {
		w.setReadError(fmt.Errorf("read LWS watch: %w", err))
		return
	}
	w.mu.Lock()
	if !w.stopping && w.scanErr == nil {
		w.scanErr = io.ErrUnexpectedEOF
	}
	w.mu.Unlock()
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
	return LWSEvent{
		Timestamp: time.Now(),
		EventType: parts[0],
		Revision:  parts[1],
		Role:      parts[2],
		Spec:      spec,
	}, nil
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
	w.mu.Lock()
	w.stopping = true
	w.mu.Unlock()

	var killErr error
	if w.cmd.Process != nil {
		killErr = w.cmd.Process.Kill()
	}
	<-w.done
	waitErr := w.cmd.Wait()

	w.mu.Lock()
	readErr := w.scanErr
	stderr := ""
	if w.stderr != nil {
		stderr = strings.TrimSpace(w.stderr.String())
	}
	w.mu.Unlock()
	if readErr != nil {
		if stderr != "" {
			return fmt.Errorf("LWS watcher: %w: %s", readErr, stderr)
		}
		return readErr
	}
	if killErr != nil {
		if errors.Is(killErr, os.ErrProcessDone) {
			if waitErr != nil {
				return fmt.Errorf("LWS watcher exited before stop: %w", waitErr)
			}
			return fmt.Errorf("LWS watcher exited before stop")
		}
		return fmt.Errorf("stop LWS watcher: %w", killErr)
	}
	// Killing a healthy long-running watch normally makes Wait return a signal
	// error; that is the expected shutdown path.
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
	if !sameRoles(expectedRoles[firstRev], expectedRoles[secondRev]) {
		return false
	}

	// Cumulative spec per (revision, role) as we replay events chronologically.
	specByRevRole := map[string]map[string]int{
		firstRev:  {},
		secondRev: {},
	}
	seen := map[string]bool{}
	seenRoles := map[string]map[string]bool{
		firstRev:  {},
		secondRev: {},
	}
	firstWasPositive := false
	for _, ev := range history {
		if ev.Revision != firstRev && ev.Revision != secondRev {
			continue
		}
		seen[ev.Revision] = true
		seenRoles[ev.Revision][ev.Role] = true
		if ev.EventType == "DELETED" {
			delete(specByRevRole[ev.Revision], ev.Role)
		} else {
			specByRevRole[ev.Revision][ev.Role] = ev.Spec
		}
		firstTotal := sumMap(specByRevRole[firstRev])
		secondTotal := sumMap(specByRevRole[secondRev])
		if firstTotal > 0 {
			firstWasPositive = true
		}
		if seen[firstRev] && seen[secondRev] &&
			sameRoles(seenRoles[firstRev], expectedRoles[firstRev]) &&
			sameRoles(seenRoles[secondRev], expectedRoles[secondRev]) &&
			firstWasPositive && firstTotal == 0 && secondTotal > 0 {
			return true
		}
	}
	return false
}

func sameRoles(a, b map[string]bool) bool {
	if len(a) == 0 || len(a) != len(b) {
		return false
	}
	for role := range a {
		if !b[role] {
			return false
		}
	}
	return true
}

func sumMap(m map[string]int) int {
	total := 0
	for _, v := range m {
		total += v
	}
	return total
}

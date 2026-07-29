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
	"fmt"
	"io"
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
// `kubectl get --watch-only --output-watch-events` subprocess. Every event
// (ADDED, MODIFIED, DELETED) is captured with a timestamp into an in-memory
// history. Used by drain-order tests to observe every state transition
// (including sub-millisecond transients that external polling misses).
type LWSWatcher struct {
	cmd     *exec.Cmd
	stdout  io.ReadCloser
	mu      sync.Mutex
	history []LWSEvent
	scanErr error
	done    chan struct{}
}

// NewLWSWatcher starts a watcher on LWSes labeled with the given DisaggregatedSet
// name. The watcher captures events until Stop() is called. Ready-to-use
// immediately (no initial-list wait needed; kubectl watches from the current
// resourceVersion by default when --watch-only is set).
func NewLWSWatcher(deploymentName string) (*LWSWatcher, error) {
	// custom-columns pulls revision label, role label, and spec.replicas from
	// each event's object.  The label key contains dots that we escape with
	// backslash so kubectl's jsonpath doesn't treat them as field traversals.
	args := []string{
		"get", "lws",
		"-l", labelName + "=" + deploymentName,
		"-n", defaultNS,
		"--watch-only",
		"--output-watch-events=true",
		"-o", `custom-columns=EVENT:.type,REV:.object.metadata.labels['disaggregatedset\.x-k8s\.io/revision'],ROLE:.object.metadata.labels['disaggregatedset\.x-k8s\.io/role'],SPEC:.object.spec.replicas`,
		"--no-headers",
	}
	cmd := exec.Command("kubectl", args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("watcher pipe: %w", err)
	}
	cmd.Stderr = nil
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("watcher start: %w", err)
	}

	w := &LWSWatcher{
		cmd:    cmd,
		stdout: stdout,
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
		// Format: "EVENT REV ROLE SPEC"  (whitespace-separated)
		parts := strings.Fields(line)
		if len(parts) < 4 {
			continue
		}
		spec, err := strconv.Atoi(parts[3])
		if err != nil {
			continue
		}
		w.mu.Lock()
		w.history = append(w.history, LWSEvent{
			Timestamp: time.Now(),
			EventType: parts[0],
			Revision:  parts[1],
			Role:      parts[2],
			Spec:      spec,
		})
		w.mu.Unlock()
	}
	if err := scanner.Err(); err != nil {
		w.mu.Lock()
		w.scanErr = err
		w.mu.Unlock()
	}
}

// Stop terminates the kubectl subprocess and waits for the read loop to finish.
func (w *LWSWatcher) Stop() {
	if w.cmd.Process != nil {
		_ = w.cmd.Process.Kill()
	}
	<-w.done
	_ = w.cmd.Wait()
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

	// Cumulative spec per (revision, role) as we replay events chronologically.
	specByRevRole := map[string]map[string]int{
		firstRev:  {},
		secondRev: {},
	}
	for _, ev := range w.history {
		if ev.Revision != firstRev && ev.Revision != secondRev {
			continue
		}
		if ev.EventType == "DELETED" {
			delete(specByRevRole[ev.Revision], ev.Role)
		} else {
			specByRevRole[ev.Revision][ev.Role] = ev.Spec
		}
		firstTotal := sumMap(specByRevRole[firstRev])
		secondTotal := sumMap(specByRevRole[secondRev])
		if firstTotal == 0 && secondTotal > 0 {
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

/*
Copyright 2026 The Kubernetes Authors.

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
	"errors"
	"io"
	"strings"
	"testing"
)

func TestSawDrainedFirst(t *testing.T) {
	event := func(eventType, revision, role string, spec int) LWSEvent {
		return LWSEvent{EventType: eventType, Revision: revision, Role: role, Spec: spec}
	}

	tests := []struct {
		name     string
		events   []LWSEvent
		first    string
		second   string
		expected bool
	}{
		{
			name: "observed positive revision drains first",
			events: []LWSEvent{
				event("ADDED", "A", "prefill", 2), event("ADDED", "A", "decode", 2),
				event("ADDED", "B", "prefill", 1), event("ADDED", "B", "decode", 1),
				event("MODIFIED", "B", "prefill", 0), event("MODIFIED", "B", "decode", 0),
			},
			first: "B", second: "A", expected: true,
		},
		{
			name: "inverse order is false",
			events: []LWSEvent{
				event("ADDED", "A", "prefill", 2), event("ADDED", "A", "decode", 2),
				event("ADDED", "B", "prefill", 1), event("ADDED", "B", "decode", 1),
				event("MODIFIED", "B", "prefill", 0), event("MODIFIED", "B", "decode", 0),
			},
			first: "A", second: "B", expected: false,
		},
		{
			name: "unseen first revision is not zero",
			events: []LWSEvent{
				event("ADDED", "B", "prefill", 1), event("ADDED", "B", "decode", 1),
			},
			first: "A", second: "B", expected: false,
		},
		{
			name: "unseen second revision is not positive",
			events: []LWSEvent{
				event("ADDED", "A", "prefill", 1), event("ADDED", "A", "decode", 1),
				event("MODIFIED", "A", "prefill", 0), event("MODIFIED", "A", "decode", 0),
			},
			first: "A", second: "B", expected: false,
		},
		{
			name: "initial zero is not a drain transition",
			events: []LWSEvent{
				event("ADDED", "A", "prefill", 0), event("ADDED", "A", "decode", 0),
				event("ADDED", "B", "prefill", 1), event("ADDED", "B", "decode", 1),
			},
			first: "A", second: "B", expected: false,
		},
		{
			name: "missing role history is incomplete",
			events: []LWSEvent{
				event("ADDED", "A", "prefill", 1),
				event("ADDED", "B", "prefill", 1), event("ADDED", "B", "decode", 1),
				event("MODIFIED", "A", "prefill", 0),
			},
			first: "A", second: "B", expected: false,
		},
		{
			name: "a matching partial prefix does not hide a later role mismatch",
			events: []LWSEvent{
				event("ADDED", "A", "prefill", 1), event("ADDED", "B", "prefill", 1),
				event("MODIFIED", "A", "prefill", 0),
				event("ADDED", "B", "decode", 1),
			},
			first: "A", second: "B", expected: false,
		},
		{
			name: "deletion events drain a fully observed revision",
			events: []LWSEvent{
				event("ADDED", "A", "prefill", 2), event("ADDED", "A", "decode", 2),
				event("ADDED", "B", "prefill", 1), event("ADDED", "B", "decode", 1),
				event("DELETED", "A", "prefill", 0), event("DELETED", "A", "decode", 0),
			},
			first: "A", second: "B", expected: true,
		},
		{
			name: "duplicate events do not affect ordering",
			events: []LWSEvent{
				event("ADDED", "A", "prefill", 2), event("ADDED", "A", "decode", 2),
				event("ADDED", "B", "prefill", 1), event("ADDED", "B", "decode", 1),
				event("MODIFIED", "A", "prefill", 2), event("MODIFIED", "A", "prefill", 2),
				event("MODIFIED", "A", "prefill", 0), event("MODIFIED", "A", "decode", 0),
			},
			first: "A", second: "B", expected: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := sawDrainedFirst(tc.events, tc.first, tc.second); got != tc.expected {
				t.Fatalf("sawDrainedFirst() = %v, want %v", got, tc.expected)
			}
		})
	}
}

func TestLWSWatcherReadErrorsAreObservable(t *testing.T) {
	w := &LWSWatcher{
		stdout: io.NopCloser(strings.NewReader("malformed event\n")),
		done:   make(chan struct{}),
	}
	w.readLoop()
	if w.Err() == nil {
		t.Fatal("Err() unexpectedly returned nil after malformed input")
	}

	firstErr := w.Err()
	w.setReadError(errors.New("later"))
	if !errors.Is(w.Err(), firstErr) {
		t.Fatalf("setReadError replaced the first read error: %v", w.Err())
	}
}

func TestLWSWatcherUnexpectedEOFIsObservable(t *testing.T) {
	w := &LWSWatcher{
		stdout: io.NopCloser(strings.NewReader("")),
		done:   make(chan struct{}),
	}
	w.readLoop()
	if !errors.Is(w.Err(), io.ErrUnexpectedEOF) {
		t.Fatalf("Err() = %v, want unexpected EOF", w.Err())
	}
}

func TestParseLWSEvent(t *testing.T) {
	event, err := parseLWSEvent("MODIFIED revision-a prefill 3")
	if err != nil {
		t.Fatalf("parseLWSEvent() unexpected error: %v", err)
	}
	if event.EventType != "MODIFIED" || event.Revision != "revision-a" || event.Role != "prefill" || event.Spec != 3 {
		t.Fatalf("parseLWSEvent() = %+v", event)
	}

	for _, input := range []string{
		"",
		"MODIFIED revision-a prefill",
		"UNKNOWN revision-a prefill 3",
		"MODIFIED revision-a prefill nope",
	} {
		if _, err := parseLWSEvent(input); err == nil {
			t.Errorf("parseLWSEvent(%q) unexpectedly succeeded", input)
		}
	}
}

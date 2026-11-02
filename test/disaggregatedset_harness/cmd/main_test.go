// Copyright 2026 The Kubernetes Authors.
// SPDX-License-Identifier: Apache-2.0
package main

import (
	"flag"
	"io"
	"testing"
)

func TestCheckpointFlagsLastValueWins(t *testing.T) {
	for _, tc := range []struct {
		args []string
		auto bool
	}{
		{nil, true},
		{[]string{"--interactive"}, false},
		{[]string{"--auto", "-interactive"}, false},
		{[]string{"--auto", "--interactive=true"}, false},
		{[]string{"--auto", "--auto=false"}, false},
		{[]string{"-interactive=1"}, false},
		{[]string{"-auto=F"}, false},
		{[]string{"--interactive", "--auto"}, true},
		{[]string{"--interactive", "-auto=true"}, true},
		{[]string{"--auto=false", "--interactive=false"}, true},
		{[]string{"--interactive=true", "-interactive=0"}, true},
	} {
		f := flag.NewFlagSet("checkpoints", flag.ContinueOnError)
		var auto bool
		checkpointFlags(f, &auto)
		if err := f.Parse(tc.args); err != nil {
			t.Fatal(err)
		}
		if auto != tc.auto {
			t.Errorf("%q: auto = %t, want %t", tc.args, auto, tc.auto)
		}
	}
}

func TestCheckpointFlagsRejectInvalidBooleans(t *testing.T) {
	for _, arg := range []string{"--auto=invalid", "-interactive=invalid"} {
		f := flag.NewFlagSet("checkpoints", flag.ContinueOnError)
		f.SetOutput(io.Discard)
		var auto bool
		checkpointFlags(f, &auto)
		if err := f.Parse([]string{arg}); err == nil {
			t.Fatalf("accepted %q", arg)
		}
	}
}

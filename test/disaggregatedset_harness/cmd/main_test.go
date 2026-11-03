// Copyright 2026 The Kubernetes Authors.
// SPDX-License-Identifier: Apache-2.0
package main

import (
	"flag"
	"io"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCheckpointFlagsLastValueWins(t *testing.T) {
	for _, tc := range []struct {
		args []string
		auto bool
	}{
		{nil, true},
		{[]string{"--interactive"}, false},
		{[]string{"--auto", "-interactive"}, false},
		{[]string{"--auto", "--auto=false"}, false},
		{[]string{"--interactive", "--auto"}, true},
		{[]string{"--auto=false", "--interactive=false"}, true},
	} {
		f := flag.NewFlagSet("checkpoints", flag.ContinueOnError)
		var auto bool
		checkpointFlags(f, &auto)
		require.NoError(t, f.Parse(tc.args), "%q", tc.args)
		require.Equal(t, tc.auto, auto, "%q", tc.args)
	}
}

func TestCheckpointFlagsRejectInvalidBooleans(t *testing.T) {
	for _, arg := range []string{"--auto=invalid", "-interactive=invalid"} {
		f := flag.NewFlagSet("checkpoints", flag.ContinueOnError)
		f.SetOutput(io.Discard)
		var auto bool
		checkpointFlags(f, &auto)
		require.Error(t, f.Parse([]string{arg}), "%s", arg)
	}
}

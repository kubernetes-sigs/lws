// Copyright 2026 The Kubernetes Authors.
// SPDX-License-Identifier: Apache-2.0
package main

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestLauncherModeUsesTheDriverParser(t *testing.T) {
	for _, tc := range []struct {
		args []string
		mode string
	}{
		{nil, "readonly"},
		{[]string{"run"}, "automatic"},
		{[]string{"--help"}, "readonly"}, {[]string{"-h"}, "readonly"}, {[]string{"-help=false"}, "readonly"},
		{[]string{"list"}, "readonly"}, {[]string{"dry-run", "large-all"}, "readonly"},
		{[]string{"--list=true"}, "readonly"}, {[]string{"-validate=T"}, "readonly"},
		{[]string{"large-all", "-dry-run=1"}, "readonly"},
		{[]string{"--list=false", "--dry-run=true"}, "readonly"},
		{[]string{"ordinary", "--list=false"}, "automatic"},
		{[]string{"list", "ordinary", "--list=false"}, "automatic"},
		{[]string{"dry-run", "ordinary", "--dry-run=false"}, "automatic"},
		{[]string{"ordinary", "--list", "--list=false"}, "automatic"},
		{[]string{"run", "ordinary", "--identity", "hash", "-auto"}, "automatic"},
		{[]string{"--identity=hash", "ordinary", "-interactive=1"}, "interactive"},
		{[]string{"ordinary", "--auto", "-interactive"}, "interactive"},
		{[]string{"ordinary", "--auto", "--auto=false"}, "interactive"},
		{[]string{"ordinary", "-auto=F"}, "interactive"},
		{[]string{"ordinary", "--interactive", "--auto"}, "automatic"},
		{[]string{"ordinary", "--auto=false", "--interactive=false"}, "automatic"},
		{[]string{"ordinary", "--interactive=invalid"}, ""},
		{[]string{"ordinary", "--auto=invalid"}, ""},
		{[]string{"ordinary", "--unknown"}, ""}, {[]string{"unknown"}, ""},
		{[]string{"ordinary", "--identity"}, ""},
	} {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			var output, diagnostics strings.Builder
			status := run(append([]string{"launcher-mode"}, tc.args...), &output, &diagnostics)
			require.Equal(t, tc.mode != "", status == 0, "%s", diagnostics.String())
			require.Equal(t, tc.mode, strings.TrimSpace(output.String()))
		})
	}
}

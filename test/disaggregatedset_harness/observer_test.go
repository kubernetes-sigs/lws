// Copyright 2026 The Kubernetes Authors.
// SPDX-License-Identifier: Apache-2.0
package harness

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// The overlay accesses private planner symbols, so unit CI must compile it
// against this checkout, not just verify a synthetic overlay's JSON mapping.
func TestObserverOverlayAgainstRepository(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq is required to prepare observer overlays")
	}
	root, err := filepath.Abs("../..")
	require.NoError(t, err)
	prepare, err := filepath.Abs("observer/prepare.sh")
	require.NoError(t, err)
	overlay := filepath.Join(t.TempDir(), "overlay.json")
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()
	command := exec.CommandContext(ctx, "bash", prepare, root, overlay)
	output, err := command.CombinedOutput()
	require.NoError(t, err, "prepare current-source observer: %s", output)
	tags := strings.TrimSpace(string(output))
	require.Equal(t, "harness", tags, "current checkout must use current observer adapters")
	goBin, err := exec.LookPath("go")
	require.NoError(t, err)
	command = exec.CommandContext(ctx, goBin,
		"test", "-overlay="+overlay, "-tags="+tags, "-count=1", "-run=^TestHarness",
		"./pkg/controllers/disaggregatedset", "./test/disaggregatedset_harness/observer")
	command.Dir = root
	output, err = command.CombinedOutput()
	require.NoError(t, err, "test current-source observer: %s", output)
}

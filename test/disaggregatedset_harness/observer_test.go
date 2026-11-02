// Copyright 2026 The Kubernetes Authors.
// SPDX-License-Identifier: Apache-2.0
package harness

import (
	"context"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// The overlay accesses private planner symbols, so unit CI must compile it
// against this checkout, not just verify a synthetic overlay's JSON mapping.
func TestObserverOverlayAgainstRepository(t *testing.T) {
	root, err := filepath.Abs("../..")
	requireNoError(t, err)
	prepare, err := filepath.Abs("observer/prepare.sh")
	requireNoError(t, err)
	overlay := filepath.Join(t.TempDir(), "overlay.json")
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()
	command := exec.CommandContext(ctx, "bash", prepare, root, overlay, "--tests")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("prepare current-source observer: %v\n%s", err, output)
	}
	tags := strings.TrimSpace(string(output))
	command = exec.CommandContext(ctx, filepath.Join(runtime.GOROOT(), "bin", "go"),
		"test", "-overlay="+overlay, "-tags="+tags, "-count=1", "-run=^TestHarness",
		"./pkg/controllers/disaggregatedset", "./test/disaggregatedset_harness/observer")
	command.Dir = root
	if output, err = command.CombinedOutput(); err != nil {
		t.Fatalf("test current-source observer: %v\n%s", err, output)
	}
}

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

package utils

import (
	"os"
	"path/filepath"
	"testing"
)

func TestGetProjectDirFromDisaggregatedSetE2E(t *testing.T) {
	rootDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(rootDir, "go.mod"), nil, 0o600); err != nil {
		t.Fatalf("failed to write go.mod: %v", err)
	}

	e2eDir := filepath.Join(rootDir, "test", "e2e", "disaggregatedset")
	if err := os.MkdirAll(e2eDir, 0o755); err != nil {
		t.Fatalf("failed to create e2e directory: %v", err)
	}
	t.Chdir(e2eDir)

	got, err := GetProjectDir()
	if err != nil {
		t.Fatalf("GetProjectDir() returned error: %v", err)
	}
	if got != rootDir {
		t.Fatalf("GetProjectDir() = %q, want %q", got, rootDir)
	}
}

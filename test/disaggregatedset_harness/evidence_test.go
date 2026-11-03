// Copyright 2026 The Kubernetes Authors.
// SPDX-License-Identifier: Apache-2.0
package harness

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestEvidenceRetainsRawResponseOnlyOnFailure(t *testing.T) {
	pod := Object{"kind": "Pod", "metadata": Object{"name": "demo-0"}, "status": Object{"phase": "Running"}}
	valid := jsonText(Object{"kind": "List", "items": []Object{pod}})
	for _, tc := range []struct {
		name, stdout, wantError string
		err                     error
	}{
		{name: "success", stdout: valid},
		{name: "process failure", stdout: valid, err: fmt.Errorf("exit 1"), wantError: "exit 1"},
		{name: "invalid response", stdout: `{"kind":"List","items":null}`, wantError: "has no items array"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			e, err := NewEvidence(root, "fixture")
			require.NoError(t, err)
			e.Case("ordinary--Hash--Deferred")
			err = e.Start(t.Context(), Options{Executor: &fakeExecutor{run: func(command) ([]byte, []byte, error) {
				return []byte(tc.stdout), []byte("kubectl warning"), tc.err
			}}})
			require.NoError(t, e.Close())
			raw, readErr := os.ReadFile(filepath.Join(root, "film-recording", "snapshots.jsonl"))
			require.NoError(t, readErr)
			var row Object
			require.NoError(t, json.NewDecoder(strings.NewReader(string(raw))).Decode(&row))
			require.Equal(t, "ordinary--Hash--Deferred", row["caseId"])
			require.Equal(t, "kubectl warning", row["stderr"])
			require.Greater(t, row["startedAt"].(float64), float64(0))
			require.GreaterOrEqual(t, row["finishedAt"].(float64), row["startedAt"].(float64))
			if tc.wantError == "" {
				require.NoError(t, err)
				require.NotContains(t, row, "stdout", "successful objects should not be stored twice")
				require.NotContains(t, row, "error")
				require.Equal(t, float64(0), row["returncode"])
				require.Equal(t, []Object{pod}, objects(at(row, "state", "pods")))
			} else {
				require.ErrorContains(t, err, tc.wantError)
				require.Contains(t, row["error"], tc.wantError)
				require.Equal(t, tc.stdout, row["stdout"])
				require.NotContains(t, row, "state", "failed acquisition must not fabricate state")
				require.Nil(t, row["returncode"])
			}
		})
	}
}

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

import "testing"

// TestEvalSingleActiveRevision pins the snapshot classes
// ForSingleActiveRevision must distinguish. Case A used to escape because
// the predicate was revision-agnostic; threading oldRevision closes it.
// See kubernetes-sigs/lws#826.
func TestEvalSingleActiveRevision(t *testing.T) {
	const oldRev = "oldRev"

	cases := []struct {
		name     string
		output   string
		wantPass bool
	}{
		{
			name:     "A_apply_but_not_reconciled",
			output:   "oldRev 2\noldRev 2",
			wantPass: false,
		},
		{
			name:     "B_mid_rollout_new_partial",
			output:   "oldRev 0\noldRev 0\nnewRev 1\nnewRev 0",
			wantPass: false,
		},
		{
			name:     "C_rollout_complete",
			output:   "oldRev 0\noldRev 0\nnewRev 2\nnewRev 2",
			wantPass: true,
		},
		{
			name:     "D_two_revisions_active",
			output:   "oldRev 2\noldRev 2\nnewRev 2\nnewRev 2",
			wantPass: false,
		},
		{
			name:     "E_old_drained_new_partial_start",
			output:   "oldRev 0\noldRev 0\nnewRev 0\nnewRev 1",
			wantPass: false,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			reason := evalSingleActiveRevision(c.output, oldRev)
			gotPass := reason == ""
			if gotPass != c.wantPass {
				t.Fatalf("want pass=%v, got pass=%v reason=%q", c.wantPass, gotPass, reason)
			}
		})
	}
}

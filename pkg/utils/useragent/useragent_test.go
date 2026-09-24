/*
Copyright 2025.

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

package useragent

import (
	"fmt"
	"runtime"
	"testing"
)

func TestDefault(t *testing.T) {
	want := fmt.Sprintf("lws/v0.0.0-main (%s/%s) abcd012", runtime.GOOS, runtime.GOARCH)
	ua := Default()
	if ua != want {
		t.Errorf("Default()=%q, want %q", ua, want)
	}
}

func TestAdjustCommit(t *testing.T) {
	tests := []struct {
		name   string
		commit string
		want   string
	}{
		{
			name:   "empty commit",
			commit: "",
			want:   "unknown",
		},
		{
			name:   "full sha is truncated to the short form",
			commit: "abcd0123456789abcdef0123456789abcdef0123",
			want:   "abcd012",
		},
		{
			name:   "an already short commit is kept as is",
			commit: "abcd012",
			want:   "abcd012",
		},
		{
			name:   "a shorter commit is kept as is",
			commit: "abcd",
			want:   "abcd",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := adjustCommit(tc.commit); got != tc.want {
				t.Errorf("adjustCommit(%q)=%q, want %q", tc.commit, got, tc.want)
			}
		})
	}
}

/*
Copyright 2023.

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

package statefulset

import (
	"testing"

	"github.com/google/go-cmp/cmp"
)

func TestGetParentNameAndOrdinal(t *testing.T) {
	tests := []struct {
		name    string
		wantStr string
		wantOrd int
	}{
		{
			name:    "lws-samples-132",
			wantStr: "lws-samples",
			wantOrd: 132,
		},
		{
			name:    "lws-samples-132-u",
			wantStr: "",
			wantOrd: -1,
		},
		{
			name:    "lws-samples-",
			wantStr: "",
			wantOrd: -1,
		},
		{
			name:    "lws-samples-0",
			wantStr: "lws-samples",
			wantOrd: 0,
		},
		{
			name:    "lws-samples--1",
			wantStr: "lws-samples-",
			wantOrd: 1,
		},
		{
			name:    "lws-samples1",
			wantStr: "",
			wantOrd: -1,
		},
		{
			name:    "lws-samples-1-0",
			wantStr: "lws-samples-1",
			wantOrd: 0,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			parent, ord := GetParentNameAndOrdinal(tc.name)
			if diff := cmp.Diff(tc.wantStr, parent); diff != "" {
				t.Errorf("unexpected parent name: %s", diff)
			}
			if diff := cmp.Diff(tc.wantOrd, ord); diff != "" {
				t.Errorf("unexpected ordinal: %s", diff)
			}
		})
	}
}

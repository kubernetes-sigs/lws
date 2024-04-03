/*
Copyright 2024.

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
	"testing"

	"github.com/google/go-cmp/cmp"
)

func Test_SortByIndex(t *testing.T) {
	testCases := []struct {
		name      string
		inputs    []int
		length    int
		indexFunc func(int) (int, error)
		want      []int
	}{
		{
			name:      "inputs equal to the length",
			inputs:    []int{3, 2, 1, 0},
			length:    4,
			indexFunc: func(index int) (int, error) { return index, nil },
			want:      []int{0, 1, 2, 3},
		},
		{
			name:      "inputs less than the length",
			inputs:    []int{3, 1, 0},
			length:    4,
			indexFunc: func(index int) (int, error) { return index, nil },
			want:      []int{0, 1, 0, 3},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			got := SortByIndex(tc.indexFunc, tc.inputs, tc.length)

			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Fatalf("unexpected result: (-want, +got) %s", diff)
			}
		})
	}
}

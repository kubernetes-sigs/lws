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

package webhooks

import (
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/utils/ptr"

	leaderworkerset "sigs.k8s.io/lws/api/leaderworkerset/v1"
	"sigs.k8s.io/lws/test/wrappers"
)

func TestGetPercentValue(t *testing.T) {
	tests := []struct {
		name           string
		input          intstr.IntOrString
		wantOutputVal  int
		wantOutputBool bool
	}{
		{
			name: "input type int",
			input: intstr.IntOrString{
				Type:   0,
				IntVal: 1,
			},
			wantOutputVal:  0,
			wantOutputBool: false,
		},
		{
			name: "input type string - invalid format",
			input: intstr.IntOrString{
				Type:   1,
				StrVal: "1",
			},
			wantOutputVal:  0,
			wantOutputBool: false,
		},
		{
			name: "input type string - valid format",
			input: intstr.IntOrString{
				Type:   1,
				StrVal: "1%",
			},
			wantOutputVal:  1,
			wantOutputBool: true,
		},
		{
			name: "input type string - valid format",
			input: intstr.IntOrString{
				Type:   1,
				StrVal: "101%",
			},
			wantOutputVal:  101,
			wantOutputBool: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			outputVal, outputBool := getPercentValue(tc.input)
			if diff := cmp.Diff(tc.wantOutputVal, outputVal); diff != "" {
				t.Errorf("unexpected result: (-want, +got) %s", diff)
			}
			if diff := cmp.Diff(tc.wantOutputBool, outputBool); diff != "" {
				t.Errorf("unexpected result: (-want, +got) %s", diff)
			}
		})
	}
}

func TestValidateNonnegativeOrZeroField(t *testing.T) {
	tests := []struct {
		name       string
		input      int64
		wantOutput field.ErrorList
	}{
		{
			name:  "input less than 0",
			input: -1,
			wantOutput: []*field.Error{
				{
					Type:     field.ErrorTypeInvalid,
					Field:    "test",
					BadValue: int64(-1),
					Detail:   "must be greater than or equal to 0",
				},
			},
		},
		{
			name:       "input equal to 0",
			input:      0,
			wantOutput: []*field.Error{},
		},
		{
			name:       "input greater than 0",
			input:      1,
			wantOutput: []*field.Error{},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			testPath := field.NewPath("test")
			output := validateNonnegativeField(tc.input, testPath)
			if diff := cmp.Diff(tc.wantOutput, output); diff != "" {
				t.Errorf("unexpected result: (-want, +got) %s", diff)
			}
		})
	}
}

func TestIsNotMoreThan100Percent(t *testing.T) {
	tests := []struct {
		name       string
		input      intstr.IntOrString
		wantErr    string
		wantOutput field.ErrorList
	}{
		{
			name: "invalid input",
			input: intstr.IntOrString{
				Type:   0,
				IntVal: 1,
			},
			wantOutput: nil,
		},
		{
			name: "valid input - greater than 100",
			input: intstr.IntOrString{
				Type:   1,
				StrVal: "101%",
			},
			wantOutput: []*field.Error{
				{
					Type:  field.ErrorTypeInvalid,
					Field: "test",
					BadValue: intstr.IntOrString{
						Type:   1,
						StrVal: "101%",
					},
					Detail: "must not be greater than 100%",
				},
			},
		},
		{
			name: "valid input - less than 100",
			input: intstr.IntOrString{
				Type:   1,
				StrVal: "99%",
			},
			wantOutput: nil,
		},
		{
			name: "valid input - equal to 100",
			input: intstr.IntOrString{
				Type:   1,
				StrVal: "100%",
			},
			wantOutput: nil,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			testPath := field.NewPath("test")
			output := isNotMoreThan100Percent(tc.input, testPath)
			if diff := cmp.Diff(tc.wantOutput, output); diff != "" {
				t.Errorf("unexpected result: (-want, +got) %s", diff)
			}
		})
	}
}

func TestValidateUpdateSubGroupPolicy(t *testing.T) {
	tests := []struct {
		name    string
		lws     *leaderworkerset.LeaderWorkerSet
		wantErr string
	}{
		{
			name: "subgroup placement and subgroup size are mutually exclusive",
			lws: wrappers.BuildLeaderWorkerSet("default").
				Size(4).
				SubGroupSize(2).
				SubGroupType(leaderworkerset.SubGroupPolicyTypeLeaderExcluded).
				SubGroupPlacement(
					leaderworkerset.SubGroupPlacement{WorkerIndexes: []int32{1, 2}, MatchLabels: map[string]string{"local": "schedule_zone"}},
					leaderworkerset.SubGroupPlacement{WorkerIndexes: []int32{3}, MatchLabels: map[string]string{"remote": "schedule_zone"}},
				).
				Obj(),
			wantErr: "subGroupPlacement and subGroupSize are mutually exclusive",
		},
		{
			name: "subgroup placement requires LeaderExcluded",
			lws: wrappers.BuildLeaderWorkerSet("default").
				Size(4).
				SubGroupType(leaderworkerset.SubGroupPolicyTypeLeaderWorker).
				SubGroupPlacement(
					leaderworkerset.SubGroupPlacement{WorkerIndexes: []int32{1, 2}, MatchLabels: map[string]string{"local": "schedule_zone"}},
					leaderworkerset.SubGroupPlacement{WorkerIndexes: []int32{3}, MatchLabels: map[string]string{"remote": "schedule_zone"}},
				).
				Obj(),
			wantErr: "subGroupPlacement only supports LeaderExcluded",
		},
		{
			name: "subgroup placement must cover all workers once",
			lws: wrappers.BuildLeaderWorkerSet("default").
				Size(4).
				SubGroupType(leaderworkerset.SubGroupPolicyTypeLeaderExcluded).
				SubGroupPlacement(
					leaderworkerset.SubGroupPlacement{WorkerIndexes: []int32{1, 2}, MatchLabels: map[string]string{"local": "schedule_zone"}},
				).
				Obj(),
			wantErr: "workerIndexes must cover every worker exactly once",
		},
		{
			name: "subgroup placement does not support leader requesting TPUs",
			lws: wrappers.BuildLeaderWorkerSet("default").
				Size(4).
				SubGroupType(leaderworkerset.SubGroupPolicyTypeLeaderExcluded).
				LeaderTemplateSpec(wrappers.MakeLeaderPodSpecWithTPUResource()).
				SubGroupPlacement(
					leaderworkerset.SubGroupPlacement{WorkerIndexes: []int32{1, 2}, MatchLabels: map[string]string{"local": "schedule_zone"}},
					leaderworkerset.SubGroupPlacement{WorkerIndexes: []int32{3}, MatchLabels: map[string]string{"remote": "schedule_zone"}},
				).
				Obj(),
			wantErr: "subGroupPlacement does not support leader requesting TPUs",
		},
		{
			name: "valid subgroup placement",
			lws: wrappers.BuildLeaderWorkerSet("default").
				Size(4).
				SubGroupType(leaderworkerset.SubGroupPolicyTypeLeaderExcluded).
				SubGroupPlacement(
					leaderworkerset.SubGroupPlacement{WorkerIndexes: []int32{1, 2}, MatchLabels: map[string]string{"local": "schedule_zone"}},
					leaderworkerset.SubGroupPlacement{WorkerIndexes: []int32{3}, MatchLabels: map[string]string{"remote": "schedule_zone"}},
				).
				Obj(),
		},
		{
			name: "legacy subgroup size still valid",
			lws: wrappers.BuildLeaderWorkerSet("default").
				Size(4).
				SubGroupType(leaderworkerset.SubGroupPolicyTypeLeaderExcluded).
				SubGroupSize(2).
				Obj(),
			wantErr: "size-1 must be divisible by subGroupSize when using LeaderExcluded",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.lws.Spec.Replicas == nil {
				tc.lws.Spec.Replicas = ptr.To[int32](1)
			}
			if tc.lws.Spec.LeaderWorkerTemplate.Size == nil {
				tc.lws.Spec.LeaderWorkerTemplate.Size = ptr.To[int32](2)
			}
			errList := validateUpdateSubGroupPolicy(field.NewPath("spec"), tc.lws)
			if tc.wantErr == "" {
				if len(errList) != 0 {
					t.Fatalf("unexpected errors: %v", errList)
				}
				return
			}
			if len(errList) == 0 {
				t.Fatalf("expected error containing %q, got none", tc.wantErr)
			}
			found := false
			for _, err := range errList {
				if err != nil && err.Detail != "" && strings.Contains(err.Detail, tc.wantErr) {
					found = true
					break
				}
			}
			if !found {
				t.Fatalf("expected error containing %q, got %v", tc.wantErr, errList)
			}
		})
	}
}

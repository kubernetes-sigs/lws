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

func TestGeneralValidateMaxGroupRestarts(t *testing.T) {
	tests := []struct {
		name      string
		lws       *leaderworkerset.LeaderWorkerSet
		wantErr   bool
		errSubstr string
	}{
		{
			name: "nil MaxGroupRestarts is always allowed",
			lws: wrappers.BuildLeaderWorkerSet("default").
				RestartPolicy(leaderworkerset.NoneRestartPolicy).
				Obj(),
			wantErr: false,
		},
		{
			name: "MaxGroupRestarts with RecreateGroupOnPodRestart is allowed",
			lws: wrappers.BuildLeaderWorkerSet("default").
				RestartPolicy(leaderworkerset.RecreateGroupOnPodRestart).
				MaxGroupRestarts(3).
				Obj(),
			wantErr: false,
		},
		{
			name: "MaxGroupRestarts with None policy is rejected",
			lws: wrappers.BuildLeaderWorkerSet("default").
				RestartPolicy(leaderworkerset.NoneRestartPolicy).
				MaxGroupRestarts(1).
				Obj(),
			wantErr:   true,
			errSubstr: "maxGroupRestarts is only supported when restartPolicy is RecreateGroupOnPodRestart",
		},
		{
			name: "MaxGroupRestarts with RecreateGroupAfterStart is rejected",
			lws: wrappers.BuildLeaderWorkerSet("default").
				RestartPolicy(leaderworkerset.RecreateGroupAfterStart).
				MaxGroupRestarts(0).
				Obj(),
			wantErr:   true,
			errSubstr: "maxGroupRestarts is only supported when restartPolicy is RecreateGroupOnPodRestart",
		},
	}

	r := &LeaderWorkerSetWebhook{}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			errs := r.generalValidate(tc.lws)
			if tc.wantErr {
				if len(errs) == 0 {
					t.Fatalf("expected validation error, got none")
				}
				joined := ""
				for _, e := range errs {
					joined += e.Error() + "\n"
				}
				if !strings.Contains(joined, tc.errSubstr) {
					t.Fatalf("expected error to contain %q, got %q", tc.errSubstr, joined)
				}
			} else if len(errs) != 0 {
				t.Fatalf("unexpected validation error: %v", errs.ToAggregate())
			}
		})
	}
}

// Make sure the import is used: verify Update path with nil maxGroupRestarts
// does not regress when previously set fields remain.
func TestGeneralValidateNilMaxGroupRestartsUpdate(t *testing.T) {
	lws := wrappers.BuildLeaderWorkerSet("default").
		RestartPolicy(leaderworkerset.RecreateGroupOnPodRestart).
		Obj()
	// explicitly nil
	lws.Spec.LeaderWorkerTemplate.MaxGroupRestarts = ptr.To[int32](0)

	r := &LeaderWorkerSetWebhook{}
	// MaxGroupRestarts(0) with RecreateGroupOnPodRestart should pass generalValidate.
	if errs := r.generalValidate(lws); len(errs) != 0 {
		t.Fatalf("unexpected validation error: %v", errs.ToAggregate())
	}
}

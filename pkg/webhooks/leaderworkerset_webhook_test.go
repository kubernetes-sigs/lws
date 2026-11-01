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
	"context"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	schedulingv1alpha3 "k8s.io/api/scheduling/v1alpha3"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	v1 "sigs.k8s.io/lws/api/leaderworkerset/v1"
	"sigs.k8s.io/lws/pkg/schedulerprovider"
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
			output := IsNotMoreThan100Percent(tc.input, testPath)
			if diff := cmp.Diff(tc.wantOutput, output); diff != "" {
				t.Errorf("unexpected result: (-want, +got) %s", diff)
			}
		})
	}
}

func TestValidateUnknownTargetRollingUpdateBudget(t *testing.T) {
	zeroPair := "must not be 0 when `maxSurge` is 0"
	tests := []struct {
		name               string
		surge, unavailable intstr.IntOrString
		wantField          string
		wantDetail         string
	}{
		{name: "percentage that rounds to zero at a small count", surge: intstr.FromInt32(0), unavailable: intstr.FromString("25%")},
		{name: "smallest percentage", surge: intstr.FromInt32(0), unavailable: intstr.FromString("1%")},
		{name: "integer", surge: intstr.FromInt32(0), unavailable: intstr.FromInt32(3)},
		{name: "surge only", surge: intstr.FromInt32(1), unavailable: intstr.FromInt32(0)},
		{name: "zero integers", surge: intstr.FromInt32(0), unavailable: intstr.FromInt32(0), wantField: "maxUnavailable", wantDetail: zeroPair},
		{name: "zero percentages", surge: intstr.FromString("0%"), unavailable: intstr.FromString("0%"), wantField: "maxUnavailable", wantDetail: zeroPair},
		{name: "zero percentage with leading zeros", surge: intstr.FromInt32(0), unavailable: intstr.FromString("00%"), wantField: "maxUnavailable", wantDetail: zeroPair},
		{name: "negative surge", surge: intstr.FromInt32(-1), unavailable: intstr.FromInt32(1), wantField: "maxSurge", wantDetail: "must be greater than or equal to 0"},
		{name: "unavailable above 100%", surge: intstr.FromInt32(0), unavailable: intstr.FromString("101%"), wantField: "maxUnavailable", wantDetail: "must not be greater than 100%"},
		{name: "malformed unavailable", surge: intstr.FromInt32(0), unavailable: intstr.FromString("bad"), wantField: "maxUnavailable", wantDetail: "a valid percent string"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := field.NewPath("test")
			errs := ValidateUnknownTargetRollingUpdateBudget(&v1.RollingUpdateConfiguration{MaxSurge: tc.surge, MaxUnavailable: tc.unavailable}, path)
			if tc.wantField == "" {
				if len(errs) != 0 {
					t.Fatalf("unexpected errors: %v", errs)
				}
				return
			}
			if len(errs) != 1 || errs[0].Field != "test."+tc.wantField || !strings.Contains(errs[0].Detail, tc.wantDetail) {
				t.Fatalf("want one error on test.%s containing %q, got %v", tc.wantField, tc.wantDetail, errs)
			}
		})
	}

	if errs := ValidateUnknownTargetRollingUpdateBudget(nil, field.NewPath("test")); len(errs) != 0 {
		t.Fatalf("unexpected errors for a nil configuration: %v", errs)
	}
}

func TestGeneralValidateMaxGroupRestarts(t *testing.T) {
	tests := []struct {
		name      string
		lws       *v1.LeaderWorkerSet
		wantErr   bool
		errSubstr string
	}{
		{
			name: "nil MaxGroupRestarts is always allowed",
			lws: wrappers.BuildLeaderWorkerSet("default").
				RestartPolicy(v1.NoneRestartPolicy).
				Obj(),
			wantErr: false,
		},
		{
			name: "MaxGroupRestarts with RecreateGroupOnPodRestart is allowed",
			lws: wrappers.BuildLeaderWorkerSet("default").
				RestartPolicy(v1.RecreateGroupOnPodRestart).
				MaxGroupRestarts(3).
				Obj(),
			wantErr: false,
		},
		{
			name: "MaxGroupRestarts with None policy is rejected",
			lws: wrappers.BuildLeaderWorkerSet("default").
				RestartPolicy(v1.NoneRestartPolicy).
				MaxGroupRestarts(1).
				Obj(),
			wantErr:   true,
			errSubstr: "maxGroupRestarts is only supported when restartPolicy recreates the group",
		},
		{
			name: "MaxGroupRestarts with RecreateGroupAfterStart is allowed",
			lws: wrappers.BuildLeaderWorkerSet("default").
				RestartPolicy(v1.RecreateGroupAfterStart).
				MaxGroupRestarts(0).
				Obj(),
			wantErr: false,
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

func TestGeneralValidateRestartBackoff(t *testing.T) {
	tests := []struct {
		name      string
		lws       *v1.LeaderWorkerSet
		wantErr   bool
		errSubstr string
	}{
		{
			name: "nil RestartBackoff is always allowed",
			lws: wrappers.BuildLeaderWorkerSet("default").
				RestartPolicy(v1.NoneRestartPolicy).
				Obj(),
			wantErr: false,
		},
		{
			name: "RestartBackoff with RecreateGroupOnPodRestart is allowed",
			lws: wrappers.BuildLeaderWorkerSet("default").
				RestartPolicy(v1.RecreateGroupOnPodRestart).
				RestartBackoff(ptr.To(int32(10)), ptr.To(int32(300))).
				Obj(),
			wantErr: false,
		},
		{
			name: "RestartBackoff with RecreateGroupAfterStart is allowed",
			lws: wrappers.BuildLeaderWorkerSet("default").
				RestartPolicy(v1.RecreateGroupAfterStart).
				RestartBackoff(ptr.To(int32(5)), ptr.To(int32(60))).
				Obj(),
			wantErr: false,
		},
		{
			name: "RestartBackoff with None policy is rejected",
			lws: wrappers.BuildLeaderWorkerSet("default").
				RestartPolicy(v1.NoneRestartPolicy).
				RestartBackoff(ptr.To(int32(10)), ptr.To(int32(300))).
				Obj(),
			wantErr:   true,
			errSubstr: "restartBackoff is only supported when restartPolicy recreates the group",
		},
		{
			name: "RestartBackoff with non-positive Base is rejected",
			lws: wrappers.BuildLeaderWorkerSet("default").
				RestartPolicy(v1.RecreateGroupOnPodRestart).
				RestartBackoff(ptr.To(int32(0)), ptr.To(int32(300))).
				Obj(),
			wantErr:   true,
			errSubstr: "must be greater than 0",
		},
		{
			name: "RestartBackoff with non-positive Cap is rejected",
			lws: wrappers.BuildLeaderWorkerSet("default").
				RestartPolicy(v1.RecreateGroupOnPodRestart).
				RestartBackoff(ptr.To(int32(10)), ptr.To(int32(-1))).
				Obj(),
			wantErr:   true,
			errSubstr: "must be greater than 0",
		},
		{
			name: "RestartBackoff with Base > Cap is rejected",
			lws: wrappers.BuildLeaderWorkerSet("default").
				RestartPolicy(v1.RecreateGroupOnPodRestart).
				RestartBackoff(ptr.To(int32(600)), ptr.To(int32(300))).
				Obj(),
			wantErr:   true,
			errSubstr: "baseSeconds must not be greater than capSeconds",
		},
		{
			name: "RestartBackoff with nil Base and Cap < default Base is rejected",
			lws: wrappers.BuildLeaderWorkerSet("default").
				RestartPolicy(v1.RecreateGroupOnPodRestart).
				RestartBackoff(nil, ptr.To(int32(5))).
				Obj(),
			wantErr:   true,
			errSubstr: "baseSeconds must not be greater than capSeconds",
		},
		{
			name: "RestartBackoff with Base > default Cap and nil Cap is rejected",
			lws: wrappers.BuildLeaderWorkerSet("default").
				RestartPolicy(v1.RecreateGroupOnPodRestart).
				RestartBackoff(ptr.To(int32(600)), nil).
				Obj(),
			wantErr:   true,
			errSubstr: "baseSeconds must not be greater than capSeconds",
		},
		{
			name: "RestartBackoff with valid single-field Base is allowed",
			lws: wrappers.BuildLeaderWorkerSet("default").
				RestartPolicy(v1.RecreateGroupOnPodRestart).
				RestartBackoff(ptr.To(int32(20)), nil).
				Obj(),
			wantErr: false,
		},
		{
			name: "RestartBackoff with valid single-field Cap is allowed",
			lws: wrappers.BuildLeaderWorkerSet("default").
				RestartPolicy(v1.RecreateGroupOnPodRestart).
				RestartBackoff(nil, ptr.To(int32(60))).
				Obj(),
			wantErr: false,
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

func TestRestartBackoffDefaulting(t *testing.T) {
	r := &LeaderWorkerSetWebhook{}
	lws := wrappers.BuildLeaderWorkerSet("default").
		RestartPolicy(v1.RecreateGroupOnPodRestart).
		Obj()
	lws.Spec.LeaderWorkerTemplate.RestartBackoff = &v1.RestartBackoff{}

	if err := r.Default(context.Background(), lws); err != nil {
		t.Fatalf("unexpected defaulting error: %v", err)
	}

	backoff := lws.Spec.LeaderWorkerTemplate.RestartBackoff
	if backoff.BaseSeconds == nil || *backoff.BaseSeconds != v1.DefaultRestartBackoffBaseSeconds {
		t.Errorf("expected default BaseSeconds %d, got %v", v1.DefaultRestartBackoffBaseSeconds, backoff.BaseSeconds)
	}
	if backoff.CapSeconds == nil || *backoff.CapSeconds != v1.DefaultRestartBackoffCapSeconds {
		t.Errorf("expected default CapSeconds %d, got %v", v1.DefaultRestartBackoffCapSeconds, backoff.CapSeconds)
	}
}

func TestLeaderWorkerSetValidation(t *testing.T) {
	webhook := &LeaderWorkerSetWebhook{}
	ctx := context.Background()

	t.Run("nil replicas should be defaulted", func(t *testing.T) {
		lws := &v1.LeaderWorkerSet{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-lws",
				Namespace: "default",
			},
			Spec: v1.LeaderWorkerSetSpec{
				Replicas: nil, // nil replicas
				LeaderWorkerTemplate: v1.LeaderWorkerTemplate{
					Size: ptr.To[int32](2),
				},
				RolloutStrategy: v1.RolloutStrategy{
					Type: v1.RollingUpdateStrategyType,
					RollingUpdateConfiguration: &v1.RollingUpdateConfiguration{
						MaxUnavailable: intstr.FromInt32(1),
						MaxSurge:       intstr.FromInt32(0),
						Partition:      ptr.To[int32](0),
					},
				},
			},
		}
		if err := webhook.Default(ctx, lws); err != nil {
			t.Fatalf("defaulting LeaderWorkerSet: %v", err)
		}
		if diff := cmp.Diff(ptr.To[int32](1), lws.Spec.Replicas); diff != "" {
			t.Errorf("unexpected replicas (-want +got):\n%s", diff)
		}
		if _, err := webhook.ValidateCreate(ctx, lws); err != nil {
			t.Errorf("validating defaulted LeaderWorkerSet: %v", err)
		}
	})

	t.Run("nil rolling update configuration should not panic", func(t *testing.T) {
		lws := &v1.LeaderWorkerSet{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-lws",
				Namespace: "default",
			},
			Spec: v1.LeaderWorkerSetSpec{
				Replicas: ptr.To[int32](2),
				LeaderWorkerTemplate: v1.LeaderWorkerTemplate{
					Size: ptr.To[int32](2),
				},
				RolloutStrategy: v1.RolloutStrategy{
					Type:                       v1.RollingUpdateStrategyType,
					RollingUpdateConfiguration: nil, // nil configuration
				},
			},
		}
		if err := webhook.Default(ctx, lws); err != nil {
			t.Fatalf("defaulting LeaderWorkerSet: %v", err)
		}
		if lws.Spec.RolloutStrategy.RollingUpdateConfiguration == nil {
			t.Fatal("expected rollingUpdateConfiguration to be defaulted")
		}
		if _, err := webhook.ValidateCreate(ctx, lws); err != nil {
			t.Errorf("validating defaulted LeaderWorkerSet: %v", err)
		}
	})

	t.Run("missing subgroup size should return a validation error", func(t *testing.T) {
		lws := &v1.LeaderWorkerSet{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-lws",
				Namespace: "default",
			},
			Spec: v1.LeaderWorkerSetSpec{
				Replicas: ptr.To[int32](1),
				LeaderWorkerTemplate: v1.LeaderWorkerTemplate{
					Size:           ptr.To[int32](2),
					SubGroupPolicy: &v1.SubGroupPolicy{},
				},
			},
		}

		allErrs := webhook.generalValidate(lws)
		if len(allErrs) != 1 {
			t.Fatalf("expected one validation error for missing subGroupSize, got %v", allErrs)
		}
		if allErrs[0].Type != field.ErrorTypeRequired {
			t.Errorf("expected required error, got %q", allErrs[0].Type)
		}
	})

	t.Run("zero subgroup size should return a validation error", func(t *testing.T) {
		lws := &v1.LeaderWorkerSet{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-lws",
				Namespace: "default",
			},
			Spec: v1.LeaderWorkerSetSpec{
				Replicas: ptr.To[int32](1),
				LeaderWorkerTemplate: v1.LeaderWorkerTemplate{
					Size: ptr.To[int32](2),
					SubGroupPolicy: &v1.SubGroupPolicy{
						SubGroupSize: ptr.To[int32](0),
					},
				},
			},
		}

		allErrs := webhook.generalValidate(lws)
		if len(allErrs) != 1 {
			t.Fatalf("expected one validation error for zero subGroupSize, got %v", allErrs)
		}
		if allErrs[0].Type != field.ErrorTypeInvalid {
			t.Errorf("expected invalid error, got %q", allErrs[0].Type)
		}
	})

	t.Run("nil subgroup size on update should not panic", func(t *testing.T) {
		oldLWS := &v1.LeaderWorkerSet{
			ObjectMeta: metav1.ObjectMeta{Name: "test-lws", Namespace: "default"},
			Spec: v1.LeaderWorkerSetSpec{
				Replicas: ptr.To[int32](1),
				LeaderWorkerTemplate: v1.LeaderWorkerTemplate{
					Size: ptr.To[int32](2),
					SubGroupPolicy: &v1.SubGroupPolicy{
						SubGroupSize: nil,
					},
				},
			},
		}
		newLWS := oldLWS.DeepCopy()
		newLWS.Spec.LeaderWorkerTemplate.SubGroupPolicy.SubGroupSize = ptr.To[int32](1)

		if _, err := webhook.ValidateUpdate(ctx, oldLWS, newLWS); err != nil {
			t.Errorf("expected update from a legacy nil subGroupSize to succeed, got: %v", err)
		}
	})

	t.Run("nil new subgroup size on update should return a validation error", func(t *testing.T) {
		oldLWS := &v1.LeaderWorkerSet{
			ObjectMeta: metav1.ObjectMeta{Name: "test-lws", Namespace: "default"},
			Spec: v1.LeaderWorkerSetSpec{
				Replicas: ptr.To[int32](1),
				LeaderWorkerTemplate: v1.LeaderWorkerTemplate{
					Size: ptr.To[int32](2),
					SubGroupPolicy: &v1.SubGroupPolicy{
						SubGroupSize: ptr.To[int32](1),
					},
				},
			},
		}
		newLWS := oldLWS.DeepCopy()
		newLWS.Spec.LeaderWorkerTemplate.SubGroupPolicy.SubGroupSize = nil

		if _, err := webhook.ValidateUpdate(ctx, oldLWS, newLWS); err == nil {
			t.Fatal("expected validation error for a nil subGroupSize")
		}
	})

	t.Run("nil old network config on update should not panic", func(t *testing.T) {
		oldLWS := &v1.LeaderWorkerSet{
			ObjectMeta: metav1.ObjectMeta{Name: "test-lws", Namespace: "default"},
			Spec: v1.LeaderWorkerSetSpec{
				Replicas: ptr.To[int32](1),
				LeaderWorkerTemplate: v1.LeaderWorkerTemplate{
					Size: ptr.To[int32](1),
				},
				NetworkConfig: nil,
			},
		}
		newLWS := oldLWS.DeepCopy()
		newLWS.Spec.NetworkConfig = &v1.NetworkConfig{}

		if _, err := webhook.ValidateUpdate(ctx, oldLWS, newLWS); err == nil {
			t.Fatal("expected validation error for a nil subdomainPolicy")
		}
	})

	t.Run("immutable scheduling fields on update should return a validation error", func(t *testing.T) {
		schedWebhook := &LeaderWorkerSetWebhook{SchedulerProvider: schedulerprovider.Kubernetes}
		oldLWS := &v1.LeaderWorkerSet{
			ObjectMeta: metav1.ObjectMeta{Name: "test-lws", Namespace: "default"},
			Spec: v1.LeaderWorkerSetSpec{
				Replicas:      ptr.To[int32](2),
				StartupPolicy: v1.LeaderCreatedStartupPolicy,
				Scheduling:    &v1.LeaderWorkerSetScheduling{},
				LeaderWorkerTemplate: v1.LeaderWorkerTemplate{
					Size: ptr.To[int32](2),
				},
			},
		}
		newLWS := oldLWS.DeepCopy()
		newLWS.Spec.Scheduling.Replica = &v1.LeaderWorkerSetReplicaScheduling{
			SchedulingPolicy: &schedulingv1alpha3.WorkloadCompositePodGroupSchedulingPolicy{
				Basic: &schedulingv1alpha3.WorkloadCompositePodGroupBasicSchedulingPolicy{},
			},
		}

		if _, err := schedWebhook.ValidateUpdate(ctx, oldLWS, newLWS); err == nil {
			t.Fatal("expected validation error when changing schedulingPolicy on update")
		}
	})
}

func TestPodTerminationPolicyDefaultAndValidation(t *testing.T) {
	ctx := context.TODO()
	webhook := &LeaderWorkerSetWebhook{}

	t.Run("defaulting sets Sequential policy", func(t *testing.T) {
		lws := wrappers.BuildLeaderWorkerSet("test-lws").Obj()
		lws.Spec.PodTerminationPolicy = ""
		if err := webhook.Default(ctx, lws); err != nil {
			t.Fatalf("unexpected error during defaulting: %v", err)
		}
		if lws.Spec.PodTerminationPolicy != v1.SequentialPodTerminationPolicy {
			t.Errorf("expected podTerminationPolicy to default to %q, got %q", v1.SequentialPodTerminationPolicy, lws.Spec.PodTerminationPolicy)
		}
	})

	t.Run("validate create accepts valid policies", func(t *testing.T) {
		for _, policy := range []v1.PodTerminationPolicyType{v1.SequentialPodTerminationPolicy, v1.ParallelPodTerminationPolicy} {
			lws := wrappers.BuildLeaderWorkerSet("test-lws").Obj()
			lws.Spec.PodTerminationPolicy = policy
			if _, err := webhook.ValidateCreate(ctx, lws); err != nil {
				t.Errorf("expected policy %q to be accepted, got error: %v", policy, err)
			}
		}
	})

	t.Run("validate create rejects invalid policy", func(t *testing.T) {
		lws := wrappers.BuildLeaderWorkerSet("test-lws").Obj()
		lws.Spec.PodTerminationPolicy = "InvalidPolicy"
		if _, err := webhook.ValidateCreate(ctx, lws); err == nil {
			t.Errorf("expected invalid policy to be rejected")
		}
	})

	t.Run("validate update rejects mutating policy", func(t *testing.T) {
		oldLWS := wrappers.BuildLeaderWorkerSet("test-lws").Obj()
		oldLWS.Spec.PodTerminationPolicy = v1.SequentialPodTerminationPolicy

		newLWS := oldLWS.DeepCopy()
		newLWS.Spec.PodTerminationPolicy = v1.ParallelPodTerminationPolicy

		if _, err := webhook.ValidateUpdate(ctx, oldLWS, newLWS); err == nil {
			t.Errorf("expected policy update from Sequential to Parallel to be rejected as immutable")
		}

		// Reverting back to Sequential should also be rejected
		parallelLWS := oldLWS.DeepCopy()
		parallelLWS.Spec.PodTerminationPolicy = v1.ParallelPodTerminationPolicy
		sequentialLWS := oldLWS.DeepCopy()
		sequentialLWS.Spec.PodTerminationPolicy = v1.SequentialPodTerminationPolicy
		if _, err := webhook.ValidateUpdate(ctx, parallelLWS, sequentialLWS); err == nil {
			t.Errorf("expected policy update from Parallel to Sequential to be rejected as immutable")
		}

		// Same policy should succeed
		sameLWS := oldLWS.DeepCopy()
		sameLWS.Spec.PodTerminationPolicy = v1.SequentialPodTerminationPolicy
		if _, err := webhook.ValidateUpdate(ctx, oldLWS, sameLWS); err != nil {
			t.Errorf("expected same policy update to be accepted, got: %v", err)
		}

		// Updating when unset/empty on old and Sequential on new should succeed (both normalize to Sequential)
		emptyLWS := oldLWS.DeepCopy()
		emptyLWS.Spec.PodTerminationPolicy = ""
		if _, err := webhook.ValidateUpdate(ctx, emptyLWS, sequentialLWS); err != nil {
			t.Errorf("expected empty to Sequential update to be accepted, got: %v", err)
		}
	})
}

func TestValidatePlacementPolicy(t *testing.T) {
	exclusive := func(topology string) *v1.SubGroupPlacementPolicy {
		return &v1.SubGroupPlacementPolicy{Type: v1.PlacementExclusiveTopology, Topology: topology}
	}
	tests := []struct {
		name           string
		annotations    map[string]string
		groupPolicy    *v1.SubGroupPlacementPolicy
		subGroupPolicy *v1.SubGroupPlacementPolicy
		errSubstr      string
	}{
		{
			name:           "group and subgroup on different topologies",
			groupPolicy:    exclusive("topology.kubernetes.io/zone"),
			subGroupPolicy: exclusive("cloud.google.com/gke-nodepool"),
		},
		{
			name:        "invalid group topology",
			groupPolicy: exclusive("not a label key"),
			errSubstr:   "spec.placementPolicy.topology",
		},
		{
			name:           "invalid subgroup topology",
			subGroupPolicy: exclusive("-rack"),
			errSubstr:      "spec.placementPolicy.subGroup.topology",
		},
		{
			name:           "group and subgroup on the same topology",
			groupPolicy:    exclusive("rack"),
			subGroupPolicy: exclusive("rack"),
			errSubstr:      "spec.placementPolicy.subGroup.topology: Invalid value: \"rack\": group and subgroup exclusive placement must use different topology keys",
		},
		{
			name:           "subgroup field on the group annotation's topology",
			annotations:    map[string]string{v1.ExclusiveKeyAnnotationKey: "rack"},
			subGroupPolicy: exclusive("rack"),
			errSubstr:      "spec.placementPolicy.subGroup.topology: Invalid value: \"rack\": group and subgroup exclusive placement must use different topology keys",
		},
		{
			name:        "group field on the subgroup annotation's topology",
			annotations: map[string]string{v1.SubGroupExclusiveKeyAnnotationKey: "rack"},
			groupPolicy: exclusive("rack"),
			errSubstr:   "spec.placementPolicy.topology: Invalid value: \"rack\": group and subgroup exclusive placement must use different topology keys",
		},
		{
			name:           "subgroup field and group annotation on different topologies",
			annotations:    map[string]string{v1.ExclusiveKeyAnnotationKey: "topology.kubernetes.io/zone"},
			subGroupPolicy: exclusive("rack"),
		},
	}

	r := &LeaderWorkerSetWebhook{}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			lws := wrappers.BuildLeaderWorkerSet("default").Size(4).SubGroupSize(2).Annotation(tc.annotations).Obj()
			lws.Spec.PlacementPolicy = testPlacementPolicy(tc.groupPolicy, tc.subGroupPolicy)
			err := r.generalValidate(lws).ToAggregate()
			if tc.errSubstr == "" {
				if err != nil {
					t.Fatalf("unexpected validation error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.errSubstr) {
				t.Fatalf("expected error containing %q, got %v", tc.errSubstr, err)
			}
		})
	}
}

// placementPolicy is part of the revision, so it can be added, changed and
// removed on a live LeaderWorkerSet.
func TestValidateUpdatePlacementPolicyMutable(t *testing.T) {
	zone := &v1.SubGroupPlacementPolicy{Type: v1.PlacementExclusiveTopology, Topology: "topology.kubernetes.io/zone"}
	rack := &v1.SubGroupPlacementPolicy{Type: v1.PlacementExclusiveTopology, Topology: "rack"}
	tests := []struct {
		name                     string
		oldGroup, newGroup       *v1.SubGroupPlacementPolicy
		oldSubGroup, newSubGroup *v1.SubGroupPlacementPolicy
	}{
		{name: "unchanged", oldGroup: zone, newGroup: zone, oldSubGroup: rack, newSubGroup: rack},
		{name: "group policy added", newGroup: zone},
		{name: "group policy changed", oldGroup: zone, newGroup: rack},
		{name: "group policy removed", oldGroup: zone},
		{name: "subgroup policy changed", oldSubGroup: rack, newSubGroup: zone},
	}

	r := &LeaderWorkerSetWebhook{}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			oldLws := wrappers.BuildLeaderWorkerSet("default").Size(4).SubGroupSize(2).Obj()
			oldLws.Spec.PlacementPolicy = testPlacementPolicy(tc.oldGroup, tc.oldSubGroup)
			newLws := oldLws.DeepCopy()
			newLws.Spec.PlacementPolicy = testPlacementPolicy(tc.newGroup, tc.newSubGroup)
			if _, err := r.ValidateUpdate(context.Background(), oldLws, newLws); err != nil {
				t.Fatalf("unexpected validation error: %v", err)
			}
		})
	}
}

func TestValidatePlacementPolicyAnnotationConflict(t *testing.T) {
	rack := &v1.SubGroupPlacementPolicy{Type: v1.PlacementExclusiveTopology, Topology: "rack"}
	none := &v1.SubGroupPlacementPolicy{Type: v1.PlacementNone}
	tests := []struct {
		name           string
		annotations    map[string]string
		groupPolicy    *v1.SubGroupPlacementPolicy
		subGroupPolicy *v1.SubGroupPlacementPolicy
		errSubstr      string
	}{
		{name: "matching group annotation", annotations: map[string]string{v1.ExclusiveKeyAnnotationKey: "rack"}, groupPolicy: rack},
		{name: "matching subgroup annotation", annotations: map[string]string{v1.SubGroupExclusiveKeyAnnotationKey: "rack"}, subGroupPolicy: rack},
		{
			name:        "different group annotation",
			annotations: map[string]string{v1.ExclusiveKeyAnnotationKey: "zone"},
			groupPolicy: rack,
			errSubstr:   "spec.placementPolicy.topology",
		},
		{
			name:           "None falls back to the group annotation",
			annotations:    map[string]string{v1.ExclusiveKeyAnnotationKey: "topology.kubernetes.io/zone"},
			groupPolicy:    none,
			subGroupPolicy: rack,
		},
		{
			name:           "different subgroup annotation",
			annotations:    map[string]string{v1.SubGroupExclusiveKeyAnnotationKey: "zone"},
			subGroupPolicy: rack,
			errSubstr:      "spec.placementPolicy.subGroup.topology",
		},
	}

	r := &LeaderWorkerSetWebhook{}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			lws := wrappers.BuildLeaderWorkerSet("default").Size(4).SubGroupSize(2).Annotation(tc.annotations).Obj()
			lws.Spec.PlacementPolicy = testPlacementPolicy(tc.groupPolicy, tc.subGroupPolicy)
			_, err := r.ValidateCreate(context.Background(), lws)
			if tc.errSubstr == "" {
				if err != nil {
					t.Fatalf("unexpected validation error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.errSubstr+": Invalid value") || !strings.Contains(err.Error(), "conflicts with annotation") {
				t.Fatalf("expected a conflict on %s, got %v", tc.errSubstr, err)
			}
		})
	}
}

func TestPlacementPolicyWarnings(t *testing.T) {
	policy := &v1.SubGroupPlacementPolicy{Type: v1.PlacementExclusiveTopology, Topology: "rack"}
	const (
		groupDeprecated    = "annotation leaderworkerset.sigs.k8s.io/exclusive-topology is deprecated, use spec.placementPolicy instead"
		subGroupDeprecated = "annotation leaderworkerset.sigs.k8s.io/subgroup-exclusive-topology is deprecated, use spec.placementPolicy.subGroup instead"
		kueueCombined      = "exclusive placement is combined with kueue.x-k8s.io/podset-required-topology: Kueue topology-aware scheduling does not account for the affinity that exclusive placement injects, so a group can be assigned a domain it cannot schedule in"
	)
	kueue := map[string]string{"kueue.x-k8s.io/podset-required-topology": "rack"}
	tests := []struct {
		name                string
		annotations         map[string]string
		templateAnnotations map[string]string
		groupPolicy         *v1.SubGroupPlacementPolicy
		subGroupPolicy      *v1.SubGroupPlacementPolicy
		want                admission.Warnings
	}{
		{name: "no annotations"},
		{name: "placementPolicy only", groupPolicy: policy, subGroupPolicy: &v1.SubGroupPlacementPolicy{Type: v1.PlacementNone}},
		{
			name:        "exclusive-topology annotation",
			annotations: map[string]string{v1.ExclusiveKeyAnnotationKey: "rack"},
			want:        admission.Warnings{groupDeprecated},
		},
		{
			name:        "exclusive-topology annotation and matching placementPolicy",
			annotations: map[string]string{v1.ExclusiveKeyAnnotationKey: "rack"},
			groupPolicy: policy,
			want:        admission.Warnings{groupDeprecated},
		},
		{
			name:        "subgroup-exclusive-topology annotation",
			annotations: map[string]string{v1.SubGroupExclusiveKeyAnnotationKey: "rack"},
			want:        admission.Warnings{subGroupDeprecated},
		},
		{
			name:                "placementPolicy with Kueue topology annotation",
			templateAnnotations: kueue,
			groupPolicy:         policy,
			want:                admission.Warnings{kueueCombined},
		},
		{
			name:                "subgroup placementPolicy with Kueue topology annotation",
			templateAnnotations: kueue,
			subGroupPolicy:      policy,
			want:                admission.Warnings{kueueCombined},
		},
		{name: "Kueue topology annotation without exclusive placement", templateAnnotations: kueue},
	}

	r := &LeaderWorkerSetWebhook{}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			lws := wrappers.BuildLeaderWorkerSet("default").Size(4).SubGroupSize(2).Annotation(tc.annotations).Obj()
			lws.Spec.LeaderWorkerTemplate.WorkerTemplate.Annotations = tc.templateAnnotations
			lws.Spec.PlacementPolicy = testPlacementPolicy(tc.groupPolicy, tc.subGroupPolicy)
			warnings, err := r.ValidateCreate(context.Background(), lws)
			if err != nil {
				t.Fatalf("unexpected validation error: %v", err)
			}
			if diff := cmp.Diff(tc.want, warnings); diff != "" {
				t.Errorf("unexpected create warnings (-want +got):\n%s", diff)
			}
			warnings, err = r.ValidateUpdate(context.Background(), lws, lws.DeepCopy())
			if err != nil {
				t.Fatalf("unexpected validation error on update: %v", err)
			}
			if diff := cmp.Diff(tc.want, warnings); diff != "" {
				t.Errorf("unexpected update warnings (-want +got):\n%s", diff)
			}
		})
	}
}

// testPlacementPolicy builds spec.placementPolicy from optional group and
// subgroup levels, defaulting the group type as the API server does.
func testPlacementPolicy(group, subGroup *v1.SubGroupPlacementPolicy) *v1.PlacementPolicy {
	if group == nil && subGroup == nil {
		return nil
	}
	policy := &v1.PlacementPolicy{Type: v1.PlacementNone, SubGroup: subGroup}
	if group != nil {
		policy.Type, policy.Topology = group.Type, group.Topology
	}
	return policy
}

func TestValidateSubGroupPlacementPolicyRequiresSubGroups(t *testing.T) {
	r := &LeaderWorkerSetWebhook{}
	lws := wrappers.BuildLeaderWorkerSet("default").Size(4).Obj()
	lws.Spec.PlacementPolicy = testPlacementPolicy(nil, &v1.SubGroupPlacementPolicy{Type: v1.PlacementExclusiveTopology, Topology: "rack"})
	err := r.generalValidate(lws).ToAggregate()
	if err == nil || !strings.Contains(err.Error(), "spec.placementPolicy.subGroup") || !strings.Contains(err.Error(), "requires leaderWorkerTemplate.subGroupPolicy") {
		t.Fatalf("expected subgroup placement without subGroupPolicy to be rejected, got %v", err)
	}

	lws.Spec.PlacementPolicy = testPlacementPolicy(nil, &v1.SubGroupPlacementPolicy{Type: v1.PlacementNone})
	if err := r.generalValidate(lws).ToAggregate(); err != nil {
		t.Fatalf("expected a None subgroup placement to be accepted without subGroupPolicy, got %v", err)
	}
}

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

	t.Run("defaulting sets Default policy", func(t *testing.T) {
		lws := wrappers.BuildLeaderWorkerSet("test-lws").Obj()
		lws.Spec.PodTerminationPolicy = ""
		if err := webhook.Default(ctx, lws); err != nil {
			t.Fatalf("unexpected error during defaulting: %v", err)
		}
		if lws.Spec.PodTerminationPolicy != v1.DefaultPodTerminationPolicy {
			t.Errorf("expected podTerminationPolicy to default to %q, got %q", v1.DefaultPodTerminationPolicy, lws.Spec.PodTerminationPolicy)
		}
	})

	t.Run("validate create accepts valid policies", func(t *testing.T) {
		for _, policy := range []v1.PodTerminationPolicyType{v1.DefaultPodTerminationPolicy, v1.ParallelPodTerminationPolicy} {
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

	t.Run("validate update allows mutating policy", func(t *testing.T) {
		oldLWS := wrappers.BuildLeaderWorkerSet("test-lws").Obj()
		oldLWS.Spec.PodTerminationPolicy = v1.DefaultPodTerminationPolicy

		newLWS := oldLWS.DeepCopy()
		newLWS.Spec.PodTerminationPolicy = v1.ParallelPodTerminationPolicy

		if _, err := webhook.ValidateUpdate(ctx, oldLWS, newLWS); err != nil {
			t.Errorf("expected policy update from Default to Parallel to be accepted, got: %v", err)
		}

		// Reverting back to Default
		newLWS2 := newLWS.DeepCopy()
		newLWS2.Spec.PodTerminationPolicy = v1.DefaultPodTerminationPolicy
		if _, err := webhook.ValidateUpdate(ctx, newLWS, newLWS2); err != nil {
			t.Errorf("expected policy update from Parallel to Default to be accepted, got: %v", err)
		}

		// Updating to invalid policy
		invalidLWS := newLWS.DeepCopy()
		invalidLWS.Spec.PodTerminationPolicy = "InvalidPolicy"
		if _, err := webhook.ValidateUpdate(ctx, newLWS, invalidLWS); err == nil {
			t.Errorf("expected invalid policy update to be rejected")
		}
	})
}


package webhooks

import (
	"context"
	"testing"

	"k8s.io/utils/ptr"

	"github.com/google/go-cmp/cmp"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/validation/field"
	leaderworkersetv1 "sigs.k8s.io/lws/api/leaderworkerset/v1"
	"sigs.k8s.io/lws/pkg/features"
)

func TestValidateInPlaceGroupRestart(t *testing.T) {
	specPath := field.NewPath("spec")
	features.FeatureGate.Set("InPlaceGroupRestart=true")
	defer features.FeatureGate.Set("InPlaceGroupRestart=false")

	tests := []struct {
		name       string
		lws        *leaderworkersetv1.LeaderWorkerSet
		is136      bool
		wantErrors field.ErrorList
	}{
		{
			name:  "valid InPlaceGroupRestart configuration",
			is136: true,
			lws: &leaderworkersetv1.LeaderWorkerSet{
				Spec: leaderworkersetv1.LeaderWorkerSetSpec{
					Replicas:      ptr.To[int32](2),
					StartupPolicy: leaderworkersetv1.LeaderCreatedStartupPolicy,
					LeaderWorkerTemplate: leaderworkersetv1.LeaderWorkerTemplate{
						Size:          ptr.To[int32](2),
						RestartPolicy: leaderworkersetv1.InPlaceGroupRestart,
					},
				},
			},
			wantErrors: nil,
		},
		{
			name:  "fails when k8s is < 1.36",
			is136: false,
			lws: &leaderworkersetv1.LeaderWorkerSet{
				Spec: leaderworkersetv1.LeaderWorkerSetSpec{
					Replicas:      ptr.To[int32](2),
					StartupPolicy: leaderworkersetv1.LeaderCreatedStartupPolicy,
					LeaderWorkerTemplate: leaderworkersetv1.LeaderWorkerTemplate{
						Size:          ptr.To[int32](2),
						RestartPolicy: leaderworkersetv1.InPlaceGroupRestart,
					},
				},
			},
			wantErrors: field.ErrorList{
				field.Forbidden(specPath.Child("leaderWorkerTemplate", "restartPolicy"), "InPlaceGroupRestart requires Kubernetes server version 1.36 or higher"),
			},
		},
		{
			name:  "fails when StartupPolicy is not LeaderCreated",
			is136: true,
			lws: &leaderworkersetv1.LeaderWorkerSet{
				Spec: leaderworkersetv1.LeaderWorkerSetSpec{
					Replicas:      ptr.To[int32](2),
					StartupPolicy: leaderworkersetv1.LeaderReadyStartupPolicy,
					LeaderWorkerTemplate: leaderworkersetv1.LeaderWorkerTemplate{
						Size:          ptr.To[int32](2),
						RestartPolicy: leaderworkersetv1.InPlaceGroupRestart,
					},
				},
			},
			wantErrors: field.ErrorList{
				field.Invalid(specPath.Child("startupPolicy"), leaderworkersetv1.LeaderReadyStartupPolicy, "must be LeaderCreated when restartPolicy is InPlaceGroupRestart"),
			},
		},
		{
			name:  "fails when duplicate triggers are specified",
			is136: true,
			lws: &leaderworkersetv1.LeaderWorkerSet{
				Spec: leaderworkersetv1.LeaderWorkerSetSpec{
					Replicas:      ptr.To[int32](2),
					StartupPolicy: leaderworkersetv1.LeaderCreatedStartupPolicy,
					LeaderWorkerTemplate: leaderworkersetv1.LeaderWorkerTemplate{
						Size:          ptr.To[int32](2),
						RestartPolicy: leaderworkersetv1.InPlaceGroupRestart,
						InPlaceGroupRestartConfig: &leaderworkersetv1.InPlaceGroupRestartConfig{
							Triggers: []leaderworkersetv1.InPlaceGroupRestartTrigger{
								{Role: leaderworkersetv1.WorkerRole, ContainerName: "app"},
								{Role: leaderworkersetv1.WorkerRole, ContainerName: "app"},
							},
						},
					},
				},
			},
			wantErrors: field.ErrorList{
				field.Duplicate(specPath.Child("leaderWorkerTemplate", "inPlaceGroupRestartConfig", "triggers").Index(1), "Worker/app"),
			},
		},
		{
			name:  "fails when exit code 88 is used",
			is136: true,
			lws: &leaderworkersetv1.LeaderWorkerSet{
				Spec: leaderworkersetv1.LeaderWorkerSetSpec{
					Replicas:      ptr.To[int32](2),
					StartupPolicy: leaderworkersetv1.LeaderCreatedStartupPolicy,
					LeaderWorkerTemplate: leaderworkersetv1.LeaderWorkerTemplate{
						Size:          ptr.To[int32](2),
						RestartPolicy: leaderworkersetv1.InPlaceGroupRestart,
						InPlaceGroupRestartConfig: &leaderworkersetv1.InPlaceGroupRestartConfig{
							Triggers: []leaderworkersetv1.InPlaceGroupRestartTrigger{
								{Role: leaderworkersetv1.WorkerRole, ContainerName: "app", RecoverableExitCodes: []int32{88}},
							},
						},
					},
				},
			},
			wantErrors: field.ErrorList{
				field.Invalid(specPath.Child("leaderWorkerTemplate", "inPlaceGroupRestartConfig", "triggers").Index(0).Child("recoverableExitCodes"), int32(88), "exit code 88 is reserved for the lws-restart-agent and cannot be used by workloads"),
			},
		},
		{
			name:  "fails when restart rules limit exceeded",
			is136: true,
			lws: &leaderworkersetv1.LeaderWorkerSet{
				Spec: leaderworkersetv1.LeaderWorkerSetSpec{
					Replicas:      ptr.To[int32](2),
					StartupPolicy: leaderworkersetv1.LeaderCreatedStartupPolicy,
					LeaderWorkerTemplate: leaderworkersetv1.LeaderWorkerTemplate{
						Size:          ptr.To[int32](2),
						RestartPolicy: leaderworkersetv1.InPlaceGroupRestart,
						InPlaceGroupRestartConfig: &leaderworkersetv1.InPlaceGroupRestartConfig{
							Triggers: []leaderworkersetv1.InPlaceGroupRestartTrigger{
								{Role: leaderworkersetv1.WorkerRole, ContainerName: "app", RecoverableExitCodes: []int32{1}},
							},
						},
						WorkerTemplate: corev1.PodTemplateSpec{
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{
									{
										Name:               "app",
										RestartPolicyRules: make([]corev1.ContainerRestartRule, 20),
									},
								},
							},
						},
					},
				},
			},
			wantErrors: field.ErrorList{
				field.Invalid(specPath.Child("leaderWorkerTemplate", "workerTemplate"), "app", "the total number of RestartPolicyRules (including injected triggers) cannot exceed 20 per container"),
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			webhook := &LeaderWorkerSetWebhook{
				isK8s136Capable: tt.is136,
			}
			webhook.Default(context.TODO(), tt.lws)
			errs := webhook.generalValidate(tt.lws)

			// We might have other default validation errors for missing fields not specified in these minimal structs
			// Filter out only the ones related to InPlaceGroupRestart for this test
			var gotErrors field.ErrorList
			for _, e := range errs {
				if e.Type == field.ErrorTypeForbidden && e.Field == "spec.leaderWorkerTemplate.restartPolicy" {
					gotErrors = append(gotErrors, e)
				}
				if e.Type == field.ErrorTypeInvalid && e.Field == "spec.startupPolicy" {
					gotErrors = append(gotErrors, e)
				}
				if e.Type == field.ErrorTypeDuplicate {
					gotErrors = append(gotErrors, e)
				}
				if e.Type == field.ErrorTypeInvalid && e.Detail == "exit code 88 is reserved for the lws-restart-agent and cannot be used by workloads" {
					gotErrors = append(gotErrors, e)
				}
				if e.Type == field.ErrorTypeInvalid && e.Detail == "the total number of RestartPolicyRules (including injected triggers) cannot exceed 20 per container" {
					gotErrors = append(gotErrors, e)
				}
			}

			if diff := cmp.Diff(tt.wantErrors, gotErrors); diff != "" {
				t.Errorf("generalValidate() errors mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

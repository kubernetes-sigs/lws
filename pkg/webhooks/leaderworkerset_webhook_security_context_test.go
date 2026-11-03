/*
Copyright 2026.

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

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/utils/ptr"

	"sigs.k8s.io/lws/test/wrappers"
)

func podSpecWithSecurityContext(sc *corev1.SecurityContext) *corev1.PodSpec {
	return &corev1.PodSpec{
		Containers: []corev1.Container{{Name: "main", SecurityContext: sc}},
	}
}

func TestValidatePodTemplateSecurityContexts(t *testing.T) {
	conflicting := &corev1.SecurityContext{
		Privileged:               ptr.To(true),
		AllowPrivilegeEscalation: ptr.To(false),
	}
	privilegedOnly := &corev1.SecurityContext{Privileged: ptr.To(true)}
	privilegedEscalationAllowed := &corev1.SecurityContext{
		Privileged:               ptr.To(true),
		AllowPrivilegeEscalation: ptr.To(true),
	}
	unprivilegedEscalationDenied := &corev1.SecurityContext{
		AllowPrivilegeEscalation: ptr.To(false),
		RunAsNonRoot:             ptr.To(true),
	}

	tests := []struct {
		name       string
		leader     *corev1.PodSpec
		worker     *corev1.PodSpec
		wantErrs   int
		wantFields []string
	}{
		{
			name:     "valid contexts pass",
			leader:   &corev1.PodSpec{Containers: []corev1.Container{{Name: "main", SecurityContext: privilegedOnly}}},
			worker:   podSpecWithSecurityContext(unprivilegedEscalationDenied),
			wantErrs: 0,
		},
		{
			name:     "privileged with escalation allowed passes",
			worker:   podSpecWithSecurityContext(privilegedEscalationAllowed),
			wantErrs: 0,
		},
		{
			name:     "no securityContext passes",
			worker:   &corev1.PodSpec{Containers: []corev1.Container{{Name: "main"}}},
			wantErrs: 0,
		},
		{
			name:     "conflict in worker template",
			worker:   podSpecWithSecurityContext(conflicting),
			wantErrs: 1,
			wantFields: []string{
				"spec.leaderWorkerTemplate.workerTemplate.spec.containers[0].securityContext",
			},
		},
		{
			name:     "conflict in leader template",
			leader:   podSpecWithSecurityContext(conflicting),
			worker:   podSpecWithSecurityContext(privilegedOnly),
			wantErrs: 1,
			wantFields: []string{
				"spec.leaderWorkerTemplate.leaderTemplate.spec.containers[0].securityContext",
			},
		},
		{
			name: "conflict in init container",
			worker: &corev1.PodSpec{
				Containers:     []corev1.Container{{Name: "main"}},
				InitContainers: []corev1.Container{{Name: "init", SecurityContext: conflicting}},
			},
			wantErrs: 1,
			wantFields: []string{
				"spec.leaderWorkerTemplate.workerTemplate.spec.initContainers[0].securityContext",
			},
		},
		{
			name:     "conflicts in both templates",
			leader:   podSpecWithSecurityContext(conflicting),
			worker:   podSpecWithSecurityContext(conflicting),
			wantErrs: 2,
			wantFields: []string{
				"spec.leaderWorkerTemplate.leaderTemplate.spec.containers[0].securityContext",
				"spec.leaderWorkerTemplate.workerTemplate.spec.containers[0].securityContext",
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			lws := wrappers.BuildLeaderWorkerSet("default").Obj()
			if tc.leader != nil {
				lws = wrappers.BuildLeaderWorkerSet("default").LeaderTemplateSpec(*tc.leader).Obj()
			}
			if tc.worker != nil {
				lws.Spec.LeaderWorkerTemplate.WorkerTemplate.Spec = *tc.worker
			}

			errs := ValidatePodTemplateSecurityContexts(
				field.NewPath("spec", "leaderWorkerTemplate"),
				&lws.Spec.LeaderWorkerTemplate,
			)
			if len(errs) != tc.wantErrs {
				t.Fatalf("got %d errors (%v), want %d", len(errs), errs, tc.wantErrs)
			}
			for i, wantField := range tc.wantFields {
				if errs[i].Field != wantField {
					t.Errorf("error[%d].Field = %q, want %q", i, errs[i].Field, wantField)
				}
				if !strings.Contains(errs[i].Detail, "cannot set `allowPrivilegeEscalation` to false and `privileged` to true") {
					t.Errorf("error[%d] must quote the API-server rule, got %q", i, errs[i].Detail)
				}
			}
		})
	}
}

// The check must be wired into admission: ValidateCreate/ValidateUpdate run
// generalValidate, which delegates to ValidateLeaderWorkerSetTemplate.
func TestValidateCreateRejectsConflictingSecurityContext(t *testing.T) {
	lws := wrappers.BuildLeaderWorkerSet("default").
		WorkerTemplateSpec(*podSpecWithSecurityContext(&corev1.SecurityContext{
			Privileged:               ptr.To(true),
			AllowPrivilegeEscalation: ptr.To(false),
		})).
		Obj()

	webhook := &LeaderWorkerSetWebhook{}
	if _, err := webhook.ValidateCreate(context.Background(), lws); err == nil {
		t.Fatal("ValidateCreate must reject privileged=true with allowPrivilegeEscalation=false")
	} else if !strings.Contains(err.Error(), "cannot set `allowPrivilegeEscalation` to false and `privileged` to true") {
		t.Fatalf("error must carry the API-server rule text, got: %v", err)
	}

	// The same template with the conflicting key removed is accepted.
	fixed := lws.DeepCopy()
	fixed.Spec.LeaderWorkerTemplate.WorkerTemplate.Spec.Containers[0].SecurityContext.AllowPrivilegeEscalation = nil
	if _, err := webhook.ValidateCreate(context.Background(), fixed); err != nil {
		t.Fatalf("privileged without allowPrivilegeEscalation must be accepted: %v", err)
	}
}

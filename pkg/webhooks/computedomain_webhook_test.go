/*
Copyright 2026 The Kubernetes Authors.

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
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/utils/ptr"

	v1 "sigs.k8s.io/lws/api/leaderworkerset/v1"
	"sigs.k8s.io/lws/pkg/computedomain"
	"sigs.k8s.io/lws/pkg/features"
	"sigs.k8s.io/lws/test/wrappers"
)

func computeDomainRESTMapper() apimeta.RESTMapper {
	mapper := apimeta.NewDefaultRESTMapper([]schema.GroupVersion{computedomain.GroupVersion})
	mapper.Add(computedomain.GroupVersion.WithKind(computedomain.Kind), apimeta.RESTScopeNamespace)
	return mapper
}

func buildGPULWS() *v1.LeaderWorkerSet {
	lws := wrappers.BuildLeaderWorkerSet("default").Name("gpu-lws").Obj()
	hook := &LeaderWorkerSetWebhook{}
	_ = hook.Default(context.Background(), lws)
	lws.Spec.LeaderWorkerTemplate.WorkerTemplate.Spec.Containers[0].Resources = corev1.ResourceRequirements{
		Limits: corev1.ResourceList{
			computedomain.NvidiaGPUResourceName: resource.MustParse("4"),
		},
	}
	return lws
}

func TestLeaderWorkerSetComputeDomainValidation(t *testing.T) {
	ctx := context.Background()

	t.Run("rejects auto-compute-domain=enabled when feature gate is disabled, allows pre-existing update", func(t *testing.T) {
		features.SetFeatureGateDuringTest(t, features.AutoComputeDomain, false)
		hook := &LeaderWorkerSetWebhook{RESTMapper: computeDomainRESTMapper()}

		lws := buildGPULWS()
		lws.Annotations = map[string]string{v1.AutoComputeDomainAnnotationKey: v1.AutoComputeDomainEnabled}
		if _, err := hook.ValidateCreate(ctx, lws); err == nil || !strings.Contains(err.Error(), "AutoComputeDomain") {
			t.Fatalf("expected feature gate error on create, got %v", err)
		}

		// Updating an already-enabled LWS when feature gate was turned off must remain allowed so users can scale down or disable it.
		scaledDown := lws.DeepCopy()
		scaledDown.Spec.Replicas = ptr.To[int32](0)
		if _, err := hook.ValidateUpdate(ctx, lws, scaledDown); err != nil {
			t.Fatalf("expected pre-existing enabled LWS update to succeed, got %v", err)
		}

		// Newly enabling on update when gate is false must be rejected.
		prevDisabled := buildGPULWS()
		if _, err := hook.ValidateUpdate(ctx, prevDisabled, lws); err == nil || !strings.Contains(err.Error(), "AutoComputeDomain") {
			t.Fatalf("expected feature gate error on newly enabling update, got %v", err)
		}
	})

	t.Run("rejects when ComputeDomain CRD is missing from RESTMapper", func(t *testing.T) {
		features.SetFeatureGateDuringTest(t, features.AutoComputeDomain, true)
		emptyMapper := apimeta.NewDefaultRESTMapper([]schema.GroupVersion{corev1.SchemeGroupVersion})
		hook := &LeaderWorkerSetWebhook{RESTMapper: emptyMapper}

		lws := buildGPULWS()
		lws.Annotations = map[string]string{v1.AutoComputeDomainAnnotationKey: v1.AutoComputeDomainEnabled}
		if _, err := hook.ValidateCreate(ctx, lws); err == nil || !strings.Contains(err.Error(), "ComputeDomain API is not available") {
			t.Fatalf("expected missing CRD error, got %v", err)
		}
	})

	t.Run("validates annotation values, container overrides, reserved claims, and zero-GPU warning", func(t *testing.T) {
		features.SetFeatureGateDuringTest(t, features.AutoComputeDomain, true)
		hook := &LeaderWorkerSetWebhook{RESTMapper: computeDomainRESTMapper()}

		// Valid GPU LWS passes with zero warnings.
		valid := buildGPULWS()
		valid.Annotations = map[string]string{v1.AutoComputeDomainAnnotationKey: v1.AutoComputeDomainEnabled}
		warnings, err := hook.ValidateCreate(ctx, valid)
		if err != nil || len(warnings) != 0 {
			t.Fatalf("expected valid GPU LWS to pass without warnings, warnings=%v err=%v", warnings, err)
		}

		// Invalid annotation value rejected.
		invalidVal := buildGPULWS()
		invalidVal.Annotations = map[string]string{v1.AutoComputeDomainAnnotationKey: "true"}
		if _, err := hook.ValidateCreate(ctx, invalidVal); err == nil {
			t.Fatalf("expected invalid auto-compute-domain value to be rejected")
		}

		// Unknown container in compute-domain-containers rejected.
		badContainer := valid.DeepCopy()
		badContainer.Spec.LeaderWorkerTemplate.WorkerTemplate.Annotations = map[string]string{
			v1.ComputeDomainContainersAnnotationKey: "missing-container",
		}
		if _, err := hook.ValidateCreate(ctx, badContainer); err == nil || !strings.Contains(err.Error(), "missing-container") {
			t.Fatalf("expected unknown container error, got %v", err)
		}

		// Reserved claim name rejected when auto-compute-domain is enabled, but allowed on child LWS when unset.
		reserved := valid.DeepCopy()
		reserved.Spec.LeaderWorkerTemplate.WorkerTemplate.Spec.ResourceClaims = []corev1.PodResourceClaim{{
			Name:                      computedomain.ChannelClaimName,
			ResourceClaimTemplateName: ptr.To("manual"),
		}}
		if _, err := hook.ValidateCreate(ctx, reserved); err == nil || !strings.Contains(err.Error(), computedomain.ChannelClaimName) {
			t.Fatalf("expected reserved claim name error when enabled, got %v", err)
		}
		childSliceLWS := reserved.DeepCopy()
		childSliceLWS.Annotations = nil
		if _, err := hook.ValidateCreate(ctx, childSliceLWS); err != nil {
			t.Fatalf("expected child LWS with slice-injected claim and unset replica annotation to pass, got %v", err)
		}

		// Zero-GPU LWS with auto-compute-domain=enabled returns an admission warning.
		cpuOnly := wrappers.BuildLeaderWorkerSet("default").Name("cpu-lws").Obj()
		_ = hook.Default(ctx, cpuOnly)
		cpuOnly.Annotations = map[string]string{v1.AutoComputeDomainAnnotationKey: v1.AutoComputeDomainEnabled}
		warnings, err = hook.ValidateCreate(ctx, cpuOnly)
		if err != nil || len(warnings) == 0 {
			t.Fatalf("expected warning and nil error for zero-GPU LWS, warnings=%v err=%v", warnings, err)
		}
	})

	t.Run("rejects controller-managed ComputeDomain annotations on templates", func(t *testing.T) {
		features.SetFeatureGateDuringTest(t, features.AutoComputeDomain, true)
		hook := &LeaderWorkerSetWebhook{RESTMapper: computeDomainRESTMapper()}

		for _, key := range []string{v1.ComputeDomainPrefixAnnotationKey, v1.ComputeDomainNameAnnotationKey} {
			for _, enabled := range []bool{true, false} {
				lws := buildGPULWS()
				if enabled {
					lws.Annotations = map[string]string{v1.AutoComputeDomainAnnotationKey: v1.AutoComputeDomainEnabled}
				}
				lws.Spec.LeaderWorkerTemplate.WorkerTemplate.Annotations = map[string]string{key: "shared"}
				if _, err := hook.ValidateCreate(ctx, lws); err == nil || !strings.Contains(err.Error(), key) {
					t.Fatalf("expected %s on workerTemplate to be rejected (opted in=%t), got %v", key, enabled, err)
				}

				leaderSet := buildGPULWS()
				if enabled {
					leaderSet.Annotations = map[string]string{v1.AutoComputeDomainAnnotationKey: v1.AutoComputeDomainEnabled}
				}
				leaderSet.Spec.LeaderWorkerTemplate.LeaderTemplate = leaderSet.Spec.LeaderWorkerTemplate.WorkerTemplate.DeepCopy()
				leaderSet.Spec.LeaderWorkerTemplate.LeaderTemplate.Annotations = map[string]string{key: "shared"}
				if _, err := hook.ValidateCreate(ctx, leaderSet); err == nil || !strings.Contains(err.Error(), key) {
					t.Fatalf("expected %s on leaderTemplate to be rejected (opted in=%t), got %v", key, enabled, err)
				}
			}
		}

		// An object that already carries the annotation stays updatable while the
		// value is unchanged, and changing the value is rejected.
		existing := buildGPULWS()
		existing.Spec.LeaderWorkerTemplate.WorkerTemplate.Annotations = map[string]string{v1.ComputeDomainNameAnnotationKey: "shared"}
		scaled := existing.DeepCopy()
		scaled.Spec.Replicas = ptr.To[int32](3)
		if _, err := hook.ValidateUpdate(ctx, existing, scaled); err != nil {
			t.Fatalf("expected update with an unchanged annotation to succeed, got %v", err)
		}
		changed := existing.DeepCopy()
		changed.Spec.LeaderWorkerTemplate.WorkerTemplate.Annotations[v1.ComputeDomainNameAnnotationKey] = "other"
		if _, err := hook.ValidateUpdate(ctx, existing, changed); err == nil || !strings.Contains(err.Error(), v1.ComputeDomainNameAnnotationKey) {
			t.Fatalf("expected update changing %s to be rejected, got %v", v1.ComputeDomainNameAnnotationKey, err)
		}
	})
}

func TestPodWebhookComputeDomainInjection(t *testing.T) {
	ctx := context.Background()
	pw := NewPodWebhook(nil)

	makeLeader := func(hashMode bool) *corev1.Pod {
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "model-0",
				Namespace: "default",
				Labels: map[string]string{
					v1.SetNameLabelKey:     "model",
					v1.WorkerIndexLabelKey: "0",
					v1.RevisionKey:         "rev1",
				},
				Annotations: map[string]string{
					v1.SizeAnnotationKey:                "2",
					v1.ComputeDomainPrefixAnnotationKey: "model-1a2b3",
				},
			},
			Spec: corev1.PodSpec{
				Subdomain: "model",
				Containers: []corev1.Container{
					{
						Name: "vllm",
						Resources: corev1.ResourceRequirements{
							Limits: corev1.ResourceList{
								computedomain.NvidiaGPUResourceName: resource.MustParse("4"),
							},
						},
					},
					{Name: "sidecar"},
				},
			},
		}
		if hashMode {
			pod.Name = ""
			pod.GenerateName = "model-abc-"
			pod.Annotations[v1.GroupIdentityAnnotationKey] = string(v1.GroupIdentityHash)
		}
		return pod
	}

	t.Run("no-op when AutoComputeDomain gate is disabled", func(t *testing.T) {
		features.SetFeatureGateDuringTest(t, features.AutoComputeDomain, false)
		leader := makeLeader(false)
		if err := pw.Default(ctx, leader); err != nil {
			t.Fatalf("Default failed: %v", err)
		}
		if leader.Annotations[v1.ComputeDomainNameAnnotationKey] != "" || len(leader.Spec.ResourceClaims) != 0 {
			t.Fatalf("expected no ComputeDomain mutation when feature gate is disabled")
		}
	})

	t.Run("ordinal and hash leaders plus worker receive idempotent claims", func(t *testing.T) {
		features.SetFeatureGateDuringTest(t, features.AutoComputeDomain, true)

		ordLeader := makeLeader(false)
		if err := pw.Default(ctx, ordLeader); err != nil {
			t.Fatalf("Default on ordinal leader failed: %v", err)
		}
		cdName := ordLeader.Annotations[v1.ComputeDomainNameAnnotationKey]
		if !strings.HasPrefix(cdName, "model-1a2b3-rev1-0-") {
			t.Fatalf("unexpected ordinal ComputeDomain name %q", cdName)
		}
		if len(ordLeader.Spec.ResourceClaims) != 1 || *ordLeader.Spec.ResourceClaims[0].ResourceClaimTemplateName != cdName {
			t.Fatalf("unexpected pod ResourceClaims: %+v", ordLeader.Spec.ResourceClaims)
		}
		if len(ordLeader.Spec.Containers[0].Resources.Claims) != 1 || len(ordLeader.Spec.Containers[1].Resources.Claims) != 0 {
			t.Fatalf("expected GPU container claimed and CPU sidecar skipped: %+v", ordLeader.Spec.Containers)
		}
		// Reinvoking Default preserves incarnation suffix and does not duplicate claims.
		if err := pw.Default(ctx, ordLeader); err != nil {
			t.Fatalf("reinvoking Default failed: %v", err)
		}
		if ordLeader.Annotations[v1.ComputeDomainNameAnnotationKey] != cdName || len(ordLeader.Spec.ResourceClaims) != 1 {
			t.Fatalf("reinvocation was not idempotent")
		}

		// Hash leader uses groupUniqueKey.
		hashLeader := makeLeader(true)
		if err := pw.Default(ctx, hashLeader); err != nil {
			t.Fatalf("Default on hash leader failed: %v", err)
		}
		wantHashCD := computedomain.BoundedName("model-1a2b3-" + hashLeader.Labels[v1.GroupUniqueHashLabelKey])
		if got := hashLeader.Annotations[v1.ComputeDomainNameAnnotationKey]; got != wantHashCD {
			t.Fatalf("hash leader ComputeDomain name=%q, want %q", got, wantHashCD)
		}

		// Worker pod joins leader's ComputeDomain.
		worker := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "model-0-1",
				Namespace: "default",
				Labels: map[string]string{
					v1.SetNameLabelKey:         "model",
					v1.GroupIndexLabelKey:      "0",
					v1.GroupUniqueHashLabelKey: ordLeader.Labels[v1.GroupUniqueHashLabelKey],
				},
				Annotations: map[string]string{
					v1.SizeAnnotationKey:              "2",
					v1.LeaderPodNameAnnotationKey:     "model-0",
					v1.LeaderAddressAnnotationKey:     "model-0.model.default",
					v1.ComputeDomainNameAnnotationKey: cdName,
				},
			},
			Spec: corev1.PodSpec{
				Subdomain: "model",
				Containers: []corev1.Container{{
					Name: "vllm",
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							computedomain.NvidiaGPUResourceName: resource.MustParse("4"),
						},
					},
				}},
			},
		}
		if err := pw.Default(ctx, worker); err != nil {
			t.Fatalf("Default on worker failed: %v", err)
		}
		if len(worker.Spec.ResourceClaims) != 1 || *worker.Spec.ResourceClaims[0].ResourceClaimTemplateName != cdName {
			t.Fatalf("worker did not receive leader's ComputeDomain claim: %+v", worker.Spec.ResourceClaims)
		}
	})
}

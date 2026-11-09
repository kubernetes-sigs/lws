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

package controllers

import (
	"context"
	"fmt"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation/field"
	appsapplyv1 "k8s.io/client-go/applyconfigurations/apps/v1"
	"k8s.io/client-go/tools/events"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	leaderworkerset "sigs.k8s.io/lws/api/leaderworkerset/v1"
	"sigs.k8s.io/lws/test/wrappers"
)

const invalidTemplateRuleText = "cannot set `allowPrivilegeEscalation` to false and `privileged` to true"

// invalidTemplateErr mimics the API-server rejection observed when a pod
// template combines privileged=true with allowPrivilegeEscalation=false: the
// workload apply fails validation on every reconcile.
func invalidTemplateErr(kind string) error {
	return apierrors.NewInvalid(
		schema.GroupKind{Group: "apps", Kind: kind},
		"test-sample",
		field.ErrorList{field.Invalid(
			field.NewPath("spec", "template", "spec", "containers").Index(0).Child("securityContext"),
			`{"privileged":true,"allowPrivilegeEscalation":false}`,
			invalidTemplateRuleText,
		)},
	)
}

func invalidTemplateTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{
		leaderworkerset.AddToScheme, appsv1.AddToScheme, corev1.AddToScheme,
	} {
		if err := add(scheme); err != nil {
			t.Fatalf("building scheme: %v", err)
		}
	}
	return scheme
}

func invalidTemplateTestLWS() *leaderworkerset.LeaderWorkerSet {
	return wrappers.BuildLeaderWorkerSet("default").Obj()
}

// leaderOnlyTestLWS clears the dedicated leader template so
// needsWorkerTemplateProbe stays false: the leader apply then renders the
// (valid) worker template and the worker probe never runs, keeping tests
// focused on the leader-apply failure path.
func leaderOnlyTestLWS() *leaderworkerset.LeaderWorkerSet {
	return wrappers.BuildLeaderWorkerSet("default").LeaderTemplate(nil).Obj()
}

// conflictingWorkerSpec returns a worker pod spec carrying the securityContext
// combination the API server rejects.
func conflictingWorkerSpec() corev1.PodSpec {
	spec := wrappers.MakeWorkerPodSpec()
	spec.Containers[0].SecurityContext = &corev1.SecurityContext{
		Privileged:               ptr.To(true),
		AllowPrivilegeEscalation: ptr.To(false),
	}
	return spec
}

func getStoredLWS(t *testing.T, c client.Client, lws *leaderworkerset.LeaderWorkerSet) *leaderworkerset.LeaderWorkerSet {
	t.Helper()
	stored := &leaderworkerset.LeaderWorkerSet{}
	key := types.NamespacedName{Name: lws.Name, Namespace: lws.Namespace}
	if err := c.Get(context.Background(), key, stored); err != nil {
		t.Fatalf("get stored lws: %v", err)
	}
	return stored
}

func assertCondition(t *testing.T, lws *leaderworkerset.LeaderWorkerSet, typ leaderworkerset.LeaderWorkerSetConditionType, status metav1.ConditionStatus, reason, messageContains string) *metav1.Condition {
	t.Helper()
	cond := apimeta.FindStatusCondition(lws.Status.Conditions, string(typ))
	if cond == nil {
		t.Fatalf("%s condition missing, conditions: %+v", typ, lws.Status.Conditions)
	}
	if cond.Status != status || cond.Reason != reason {
		t.Fatalf("%s = %s/%s, want %s/%s", typ, cond.Status, cond.Reason, status, reason)
	}
	if messageContains != "" && !strings.Contains(cond.Message, messageContains) {
		t.Fatalf("%s message %q must contain %q", typ, cond.Message, messageContains)
	}
	return cond
}

func reconcileLWS(t *testing.T, r *LeaderWorkerSetReconciler, lws *leaderworkerset.LeaderWorkerSet) error {
	t.Helper()
	_, err := r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Name: lws.Name, Namespace: lws.Namespace},
	})
	return err
}

// recordInvalidTemplate must mark the whole status as rejected: Progressing
// and Available false with reason InvalidTemplate, a True UpdateInProgress
// flipped to false, and status.observedGeneration refreshed so
// generation-aware callers do not treat the previous healthy generation as
// ready at the rejected one.
func TestRecordInvalidTemplateSetsConditions(t *testing.T) {
	scheme := invalidTemplateTestScheme(t)
	lws := invalidTemplateTestLWS()
	lws.Generation = 2
	lws.Status.Conditions = []metav1.Condition{
		makeCondition(leaderworkerset.LeaderWorkerSetProgressing, lws),
		makeCondition(leaderworkerset.LeaderWorkerSetUpdateInProgress, lws),
		makeCondition(leaderworkerset.LeaderWorkerSetAvailable, lws),
	}

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(lws).WithStatusSubresource(lws).Build()
	r := NewLeaderWorkerSetReconciler(c, scheme, events.NewFakeRecorder(16), nil)

	r.recordInvalidTemplate(context.Background(), lws, invalidTemplateErr("StatefulSet"))

	stored := getStoredLWS(t, c, lws)
	assertCondition(t, stored, leaderworkerset.LeaderWorkerSetProgressing, metav1.ConditionFalse, InvalidTemplate, invalidTemplateRuleText)
	assertCondition(t, stored, leaderworkerset.LeaderWorkerSetAvailable, metav1.ConditionFalse, InvalidTemplate, "Not all replicas are ready")
	assertCondition(t, stored, leaderworkerset.LeaderWorkerSetUpdateInProgress, metav1.ConditionFalse, InvalidTemplate, "No rolling update is in progress")
	if stored.Status.ObservedGeneration != lws.Generation {
		t.Fatalf("status.observedGeneration = %d, want %d", stored.Status.ObservedGeneration, lws.Generation)
	}
	for _, cond := range stored.Status.Conditions {
		if cond.ObservedGeneration != lws.Generation {
			t.Fatalf("condition %s observedGeneration = %d, want %d", cond.Type, cond.ObservedGeneration, lws.Generation)
		}
	}
}

// UpdateInProgress must keep its state when it was not True: only an
// interrupted rollout gets flipped.
func TestRecordInvalidTemplateLeavesFalseUpdateInProgressAlone(t *testing.T) {
	scheme := invalidTemplateTestScheme(t)
	lws := invalidTemplateTestLWS()
	lws.Status.Conditions = []metav1.Condition{
		makeFalseCondition(leaderworkerset.LeaderWorkerSetUpdateInProgress, lws, "SomeOtherReason", "No rolling update is in progress"),
	}

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(lws).WithStatusSubresource(lws).Build()
	r := NewLeaderWorkerSetReconciler(c, scheme, events.NewFakeRecorder(16), nil)

	r.recordInvalidTemplate(context.Background(), lws, invalidTemplateErr("StatefulSet"))

	stored := getStoredLWS(t, c, lws)
	assertCondition(t, stored, leaderworkerset.LeaderWorkerSetUpdateInProgress, metav1.ConditionFalse, "SomeOtherReason", "")
}

// A repeated identical failure must not churn the status: the reconcile loop
// retries hot while watch events keep arriving.
func TestRecordInvalidTemplateIdempotent(t *testing.T) {
	scheme := invalidTemplateTestScheme(t)
	lws := invalidTemplateTestLWS()

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(lws).WithStatusSubresource(lws).Build()
	r := NewLeaderWorkerSetReconciler(c, scheme, events.NewFakeRecorder(16), nil)

	ctx := context.Background()
	r.recordInvalidTemplate(ctx, lws, invalidTemplateErr("StatefulSet"))
	first := getStoredLWS(t, c, lws)

	r.recordInvalidTemplate(ctx, lws, invalidTemplateErr("StatefulSet"))
	second := getStoredLWS(t, c, lws)

	if first.ResourceVersion != second.ResourceVersion {
		t.Fatalf("second identical failure must not rewrite status: rv %s -> %s", first.ResourceVersion, second.ResourceVersion)
	}
	assertCondition(t, second, leaderworkerset.LeaderWorkerSetProgressing, metav1.ConditionFalse, InvalidTemplate, invalidTemplateRuleText)
}

// Rollback recovery: when Available becomes True after InvalidTemplate (the
// operator reverted to the running revision or fixed the template),
// Progressing=False must drop the stale failure reason and message and return
// to the normal steady state instead of advertising the old error forever.
func TestSetConditionResetsInvalidTemplateOnRecovery(t *testing.T) {
	lws := invalidTemplateTestLWS()
	lws.Generation = 3
	lws.Status.Conditions = []metav1.Condition{
		makeFalseCondition(leaderworkerset.LeaderWorkerSetProgressing, lws, InvalidTemplate, invalidTemplateRuleText),
		makeFalseCondition(leaderworkerset.LeaderWorkerSetAvailable, lws, InvalidTemplate, "Not all replicas are ready"),
	}

	if !setCondition(lws, makeCondition(leaderworkerset.LeaderWorkerSetAvailable, lws)) {
		t.Fatal("setCondition(Available=True) must report a change")
	}

	assertCondition(t, lws, leaderworkerset.LeaderWorkerSetAvailable, metav1.ConditionTrue, "AllGroupsReady", "")
	prog := assertCondition(t, lws, leaderworkerset.LeaderWorkerSetProgressing, metav1.ConditionFalse, GroupsProgressing, "Replicas are progressing")
	if prog.ObservedGeneration != lws.Generation {
		t.Fatalf("reset Progressing observedGeneration = %d, want %d", prog.ObservedGeneration, lws.Generation)
	}
}

// End-to-end through Reconcile: an invalid leader template failing the leader
// StatefulSet apply must surface on the LWS status.
func TestReconcileSurfacesInvalidLeaderTemplate(t *testing.T) {
	scheme := invalidTemplateTestScheme(t)
	lws := leaderOnlyTestLWS()

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(lws).WithStatusSubresource(lws).
		WithInterceptorFuncs(interceptor.Funcs{
			Apply: func(ctx context.Context, cl client.WithWatch, obj runtime.ApplyConfiguration, opts ...client.ApplyOption) error {
				if _, ok := obj.(*appsapplyv1.StatefulSetApplyConfiguration); ok {
					return invalidTemplateErr("StatefulSet")
				}
				return cl.Apply(ctx, obj, opts...)
			},
		}).Build()
	r := NewLeaderWorkerSetReconciler(c, scheme, events.NewFakeRecorder(16), nil)

	err := reconcileLWS(t, r, lws)
	if !apierrors.IsInvalid(err) {
		t.Fatalf("Reconcile() error = %v, want the invalid-template error", err)
	}

	stored := getStoredLWS(t, c, lws)
	assertCondition(t, stored, leaderworkerset.LeaderWorkerSetProgressing, metav1.ConditionFalse, InvalidTemplate, invalidTemplateRuleText)
	assertCondition(t, stored, leaderworkerset.LeaderWorkerSetAvailable, metav1.ConditionFalse, InvalidTemplate, "")
}

// Transient apply failures (timeouts, conflicts) must NOT mark the template
// invalid: only permanent API-server rejections do.
func TestReconcileTransientApplyErrorKeepsConditions(t *testing.T) {
	scheme := invalidTemplateTestScheme(t)
	lws := leaderOnlyTestLWS()

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(lws).WithStatusSubresource(lws).
		WithInterceptorFuncs(interceptor.Funcs{
			Apply: func(ctx context.Context, cl client.WithWatch, obj runtime.ApplyConfiguration, opts ...client.ApplyOption) error {
				if _, ok := obj.(*appsapplyv1.StatefulSetApplyConfiguration); ok {
					return fmt.Errorf("etcdserver: request timed out")
				}
				return cl.Apply(ctx, obj, opts...)
			},
		}).Build()
	r := NewLeaderWorkerSetReconciler(c, scheme, events.NewFakeRecorder(16), nil)

	if err := reconcileLWS(t, r, lws); err == nil {
		t.Fatal("Reconcile() must propagate the transient error")
	}

	stored := getStoredLWS(t, c, lws)
	if cond := apimeta.FindStatusCondition(stored.Status.Conditions, string(leaderworkerset.LeaderWorkerSetProgressing)); cond != nil && cond.Reason == InvalidTemplate {
		t.Fatalf("transient error must not set InvalidTemplate, got %+v", cond)
	}
}

// With a dedicated leaderTemplate the leader apply never sees the worker
// template; the dry-run probe must catch an invalid worker template before
// groups are recreated with it.
func TestReconcileSurfacesInvalidWorkerTemplate(t *testing.T) {
	scheme := invalidTemplateTestScheme(t)
	lws := wrappers.BuildLeaderWorkerSet("default").WorkerTemplateSpec(conflictingWorkerSpec()).Obj()

	probeApplied := false
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(lws).WithStatusSubresource(lws).
		WithInterceptorFuncs(interceptor.Funcs{
			Apply: func(ctx context.Context, cl client.WithWatch, obj runtime.ApplyConfiguration, opts ...client.ApplyOption) error {
				if sts, ok := obj.(*appsapplyv1.StatefulSetApplyConfiguration); ok && ptr.Deref(sts.GetName(), "") == workerTemplateProbeName {
					probeApplied = true
					return invalidTemplateErr("StatefulSet")
				}
				return cl.Apply(ctx, obj, opts...)
			},
		}).Build()
	r := NewLeaderWorkerSetReconciler(c, scheme, events.NewFakeRecorder(16), nil)

	if err := reconcileLWS(t, r, lws); err == nil {
		t.Fatal("Reconcile() must fail on an invalid worker template")
	}
	if !probeApplied {
		t.Fatal("the worker template probe never ran")
	}

	// The probe must run before the leader apply: a rejected worker template
	// must not stamp the broken revision onto the leader StatefulSet and start
	// recreating leader pods with it.
	leaderSts := &appsv1.StatefulSet{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: lws.Name, Namespace: lws.Namespace}, leaderSts); !apierrors.IsNotFound(err) {
		t.Fatalf("leader statefulset must not be applied when worker template is invalid, got err=%v", err)
	}

	stored := getStoredLWS(t, c, lws)
	assertCondition(t, stored, leaderworkerset.LeaderWorkerSetProgressing, metav1.ConditionFalse, InvalidTemplate, invalidTemplateRuleText)

	// Retries keep failing with the probe gate still armed (the leader
	// StatefulSet never received the new revision), so InvalidTemplate must
	// survive across reconciles instead of being overwritten by a
	// healthy-looking Progressing=True.
	if err := reconcileLWS(t, r, lws); err == nil {
		t.Fatal("second Reconcile() must also fail on an invalid worker template")
	}
	stored = getStoredLWS(t, c, lws)
	assertCondition(t, stored, leaderworkerset.LeaderWorkerSetProgressing, metav1.ConditionFalse, InvalidTemplate, invalidTemplateRuleText)
}

// The hash-identity path applies a leader Deployment; an invalid template must
// surface the same conditions there.
func TestReconcileHashSurfacesInvalidLeaderTemplate(t *testing.T) {
	scheme := invalidTemplateTestScheme(t)
	lws := invalidTemplateTestLWS()
	lws.Spec.GroupIdentity = leaderworkerset.GroupIdentityHash

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(lws).WithStatusSubresource(lws).
		WithInterceptorFuncs(interceptor.Funcs{
			Apply: func(ctx context.Context, cl client.WithWatch, obj runtime.ApplyConfiguration, opts ...client.ApplyOption) error {
				if _, ok := obj.(*appsapplyv1.DeploymentApplyConfiguration); ok {
					return invalidTemplateErr("Deployment")
				}
				return cl.Apply(ctx, obj, opts...)
			},
		}).Build()
	r := NewLeaderWorkerSetReconciler(c, scheme, events.NewFakeRecorder(16), nil)

	if err := reconcileLWS(t, r, lws); err == nil {
		t.Fatal("Reconcile() must fail on an invalid hash-identity leader template")
	}

	stored := getStoredLWS(t, c, lws)
	assertCondition(t, stored, leaderworkerset.LeaderWorkerSetProgressing, metav1.ConditionFalse, InvalidTemplate, invalidTemplateRuleText)
}

// Recovery through Reconcile: once the template rejection stops (fix or
// rollback), the next successful reconcile must replace the stale
// InvalidTemplate reason with the normal progressing state.
func TestReconcileRecoversFromInvalidTemplate(t *testing.T) {
	scheme := invalidTemplateTestScheme(t)
	lws := leaderOnlyTestLWS()

	failApply := true
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(lws).WithStatusSubresource(lws).
		WithInterceptorFuncs(interceptor.Funcs{
			Apply: func(ctx context.Context, cl client.WithWatch, obj runtime.ApplyConfiguration, opts ...client.ApplyOption) error {
				if _, ok := obj.(*appsapplyv1.StatefulSetApplyConfiguration); ok && failApply {
					return invalidTemplateErr("StatefulSet")
				}
				return cl.Apply(ctx, obj, opts...)
			},
		}).Build()
	r := NewLeaderWorkerSetReconciler(c, scheme, events.NewFakeRecorder(64), nil)

	if err := reconcileLWS(t, r, lws); err == nil {
		t.Fatal("first Reconcile() must fail while the template is rejected")
	}
	assertCondition(t, getStoredLWS(t, c, lws), leaderworkerset.LeaderWorkerSetProgressing, metav1.ConditionFalse, InvalidTemplate, "")

	// Operator fixes the template (or rolls back): applies start succeeding.
	failApply = false
	if err := reconcileLWS(t, r, lws); err != nil {
		t.Fatalf("second Reconcile() error = %v, want success", err)
	}

	stored := getStoredLWS(t, c, lws)
	assertCondition(t, stored, leaderworkerset.LeaderWorkerSetProgressing, metav1.ConditionTrue, GroupsProgressing, "")
}

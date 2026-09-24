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
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/utils/lru"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	leaderworkerset "sigs.k8s.io/lws/api/leaderworkerset/v1"
	"sigs.k8s.io/lws/pkg/schedulerprovider"
	"sigs.k8s.io/lws/test/wrappers"
)

// The helpers below are prefixed with lwsSched to keep them clearly scoped to the
// workload-scheduling condition tests in this file.

// lwsSchedConditionType is the condition failWorkloadScheduling and
// updateWorkloadSchedulingCondition manage.
const lwsSchedConditionType = string(leaderworkerset.LeaderWorkerSetWorkloadSchedulingCreated)

func TestVolcanoHashPodDeletionEnqueuesLWS(t *testing.T) {
	queue := workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[reconcile.Request]())
	defer queue.ShutDown()
	handler := volcanoPodDeleteHandler()
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: "leader", Namespace: "default",
		Labels: map[string]string{leaderworkerset.SetNameLabelKey: "example"},
		Annotations: map[string]string{
			leaderworkerset.GroupIdentityAnnotationKey:        string(leaderworkerset.GroupIdentityHash),
			schedulerprovider.WorkloadSchedulingAnnotationKey: "replica",
		},
	}}
	handler.Delete(context.Background(), event.TypedDeleteEvent[client.Object]{Object: pod}, queue)
	if queue.Len() != 1 {
		t.Fatalf("expected one LWS reconcile request, got %d", queue.Len())
	}
	got, _ := queue.Get()
	queue.Done(got)
	want := reconcile.Request{NamespacedName: types.NamespacedName{Name: "example", Namespace: "default"}}
	if got != want {
		t.Errorf("request = %v, want %v", got, want)
	}
	pod.Annotations = nil
	handler.Delete(context.Background(), event.TypedDeleteEvent[client.Object]{Object: pod}, queue)
	if queue.Len() != 0 {
		t.Errorf("legacy pod deletion unexpectedly enqueued an LWS request")
	}
}

// lwsSchedNewReconciler builds a reconciler backed by a fake client seeded with lws.
// The returned recorder captures the events the reconciler emits.
func lwsSchedNewReconciler(t *testing.T, funcs interceptor.Funcs, lws *leaderworkerset.LeaderWorkerSet) (*LeaderWorkerSetReconciler, client.Client, *events.FakeRecorder) {
	t.Helper()
	scheme := lwsStatusScheme(t)
	recorder := events.NewFakeRecorder(10)
	k8sClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&leaderworkerset.LeaderWorkerSet{}).
		WithObjects(lws).
		WithInterceptorFuncs(funcs).
		Build()
	return &LeaderWorkerSetReconciler{
		Client:                k8sClient,
		Scheme:                scheme,
		Record:                recorder,
		revisionEqualityCache: lru.New(maxRevisionEqualityCacheEntries),
	}, k8sClient, recorder
}

// lwsSchedGetCondition reads the WorkloadSchedulingCreated condition straight from
// the API so the tests assert what was persisted, not just the in-memory object.
func lwsSchedGetCondition(t *testing.T, k8sClient client.Client, lws *leaderworkerset.LeaderWorkerSet) *metav1.Condition {
	t.Helper()
	fetched := &leaderworkerset.LeaderWorkerSet{}
	if err := k8sClient.Get(context.Background(), types.NamespacedName{Name: lws.Name, Namespace: lws.Namespace}, fetched); err != nil {
		t.Fatalf("getting lws: %v", err)
	}
	return apimeta.FindStatusCondition(fetched.Status.Conditions, lwsSchedConditionType)
}

func TestUpdateWorkloadSchedulingCondition(t *testing.T) {
	ctx := context.Background()

	t.Run("sets the condition and persists it", func(t *testing.T) {
		lws := wrappers.BuildLeaderWorkerSet("default").Obj()
		lws.Generation = 7
		r, k8sClient, _ := lwsSchedNewReconciler(t, interceptor.Funcs{}, lws)

		if err := r.updateWorkloadSchedulingCondition(ctx, lws, metav1.ConditionTrue, "SchedulingPrerequisitesCreated", "scheduling prerequisites created"); err != nil {
			t.Fatalf("updateWorkloadSchedulingCondition() = %v, want nil", err)
		}

		got := lwsSchedGetCondition(t, k8sClient, lws)
		if got == nil {
			t.Fatalf("condition %s not found", lwsSchedConditionType)
		}
		want := metav1.Condition{
			Type:               lwsSchedConditionType,
			Status:             metav1.ConditionTrue,
			ObservedGeneration: 7,
			Reason:             "SchedulingPrerequisitesCreated",
			Message:            "scheduling prerequisites created",
		}
		if diff := cmp.Diff(want, *got, lwsSchedIgnoreTransitionTime()); diff != "" {
			t.Errorf("unexpected condition (-want +got):\n%s", diff)
		}
	})

	t.Run("flips the condition to false and rewrites reason and message", func(t *testing.T) {
		lws := wrappers.BuildLeaderWorkerSet("default").Obj()
		r, k8sClient, _ := lwsSchedNewReconciler(t, interceptor.Funcs{}, lws)

		if err := r.updateWorkloadSchedulingCondition(ctx, lws, metav1.ConditionTrue, "SchedulingPrerequisitesCreated", "created"); err != nil {
			t.Fatalf("seeding the true condition: %v", err)
		}
		if err := r.updateWorkloadSchedulingCondition(ctx, lws, metav1.ConditionFalse, schedulerprovider.ReasonPodGroupCreateFailed, "podgroup exploded"); err != nil {
			t.Fatalf("updateWorkloadSchedulingCondition() = %v, want nil", err)
		}

		got := lwsSchedGetCondition(t, k8sClient, lws)
		if got == nil {
			t.Fatalf("condition %s not found", lwsSchedConditionType)
		}
		want := metav1.Condition{
			Type:    lwsSchedConditionType,
			Status:  metav1.ConditionFalse,
			Reason:  schedulerprovider.ReasonPodGroupCreateFailed,
			Message: "podgroup exploded",
		}
		if diff := cmp.Diff(want, *got, lwsSchedIgnoreTransitionTime()); diff != "" {
			t.Errorf("unexpected condition (-want +got):\n%s", diff)
		}
	})

	t.Run("skips the status write when nothing changed", func(t *testing.T) {
		lws := wrappers.BuildLeaderWorkerSet("default").Obj()
		statusUpdates := 0
		r, _, _ := lwsSchedNewReconciler(t, interceptor.Funcs{
			SubResourceUpdate: func(ctx context.Context, c client.Client, subResourceName string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
				statusUpdates++
				return c.Status().Update(ctx, obj, opts...)
			},
		}, lws)

		for range 3 {
			if err := r.updateWorkloadSchedulingCondition(ctx, lws, metav1.ConditionTrue, "SchedulingPrerequisitesCreated", "created"); err != nil {
				t.Fatalf("updateWorkloadSchedulingCondition() = %v, want nil", err)
			}
		}

		// Only the first call changes the condition; the rest must short-circuit
		// before touching the API server.
		if statusUpdates != 1 {
			t.Errorf("status updates = %d, want 1", statusUpdates)
		}
	})

	t.Run("wraps a failing status update", func(t *testing.T) {
		lws := wrappers.BuildLeaderWorkerSet("default").Obj()
		updateErr := errors.New("boom")
		r, _, _ := lwsSchedNewReconciler(t, interceptor.Funcs{
			SubResourceUpdate: func(context.Context, client.Client, string, client.Object, ...client.SubResourceUpdateOption) error {
				return updateErr
			},
		}, lws)

		err := r.updateWorkloadSchedulingCondition(ctx, lws, metav1.ConditionFalse, schedulerprovider.ReasonAPINotAvailable, "no api")
		if err == nil {
			t.Fatal("updateWorkloadSchedulingCondition() = nil, want an error")
		}
		if !errors.Is(err, updateErr) {
			t.Errorf("error %v does not wrap %v", err, updateErr)
		}
		if !strings.Contains(err.Error(), "update WorkloadSchedulingCreated condition") {
			t.Errorf("error %q does not name the condition it failed to update", err)
		}
	})
}

func TestFailWorkloadScheduling(t *testing.T) {
	ctx := context.Background()
	reconcileErr := errors.New("scheduling prerequisites failed")

	t.Run("records an event, marks the condition false and returns the reconcile error", func(t *testing.T) {
		lws := wrappers.BuildLeaderWorkerSet("default").Obj()
		r, k8sClient, recorder := lwsSchedNewReconciler(t, interceptor.Funcs{}, lws)

		err := r.failWorkloadScheduling(ctx, lws, schedulerprovider.ReasonWorkloadCreateFailed, reconcileErr)
		if !errors.Is(err, reconcileErr) {
			t.Fatalf("failWorkloadScheduling() = %v, want it to wrap %v", err, reconcileErr)
		}

		got := lwsSchedGetCondition(t, k8sClient, lws)
		if got == nil {
			t.Fatalf("condition %s not found", lwsSchedConditionType)
		}
		want := metav1.Condition{
			Type:    lwsSchedConditionType,
			Status:  metav1.ConditionFalse,
			Reason:  schedulerprovider.ReasonWorkloadCreateFailed,
			Message: reconcileErr.Error(),
		}
		if diff := cmp.Diff(want, *got, lwsSchedIgnoreTransitionTime()); diff != "" {
			t.Errorf("unexpected condition (-want +got):\n%s", diff)
		}

		gotEvents := podCtrlDrainEvents(recorder)
		wantEvents := []string{
			corev1.EventTypeWarning + " " + schedulerprovider.ReasonWorkloadCreateFailed + " " + reconcileErr.Error(),
		}
		if diff := cmp.Diff(wantEvents, gotEvents); diff != "" {
			t.Errorf("unexpected events (-want +got):\n%s", diff)
		}
	})

	t.Run("joins the status error with the reconcile error", func(t *testing.T) {
		lws := wrappers.BuildLeaderWorkerSet("default").Obj()
		statusErr := errors.New("status update rejected")
		r, _, _ := lwsSchedNewReconciler(t, interceptor.Funcs{
			SubResourceUpdate: func(context.Context, client.Client, string, client.Object, ...client.SubResourceUpdateOption) error {
				return statusErr
			},
		}, lws)

		err := r.failWorkloadScheduling(ctx, lws, schedulerprovider.ReasonAPINotAvailable, reconcileErr)
		// Both failures must survive: the reconcile error drives the retry and the
		// status error tells the operator the condition was never published.
		if !errors.Is(err, reconcileErr) {
			t.Errorf("error %v does not wrap the reconcile error %v", err, reconcileErr)
		}
		if !errors.Is(err, statusErr) {
			t.Errorf("error %v does not wrap the status error %v", err, statusErr)
		}
	})

	t.Run("tolerates a nil recorder", func(t *testing.T) {
		lws := wrappers.BuildLeaderWorkerSet("default").Obj()
		r, k8sClient, _ := lwsSchedNewReconciler(t, interceptor.Funcs{}, lws)
		r.Record = nil

		err := r.failWorkloadScheduling(ctx, lws, schedulerprovider.ReasonUnsupportedProviderCapability, reconcileErr)
		if !errors.Is(err, reconcileErr) {
			t.Fatalf("failWorkloadScheduling() = %v, want it to wrap %v", err, reconcileErr)
		}
		// The condition is still published even though no event could be emitted.
		if got := lwsSchedGetCondition(t, k8sClient, lws); got == nil || got.Status != metav1.ConditionFalse {
			t.Errorf("condition = %v, want it present and false", got)
		}
	})
}

func TestNewLeaderWorkerSetReconciler(t *testing.T) {
	scheme := lwsStatusScheme(t)
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	recorder := events.NewFakeRecorder(1)
	provider := &schedulerprovider.VolcanoProvider{}

	t.Run("without a provider", func(t *testing.T) {
		r := NewLeaderWorkerSetReconciler(k8sClient, scheme, recorder)
		if r.SchedulerProvider != nil {
			t.Errorf("SchedulerProvider = %v, want nil", r.SchedulerProvider)
		}
		if r.revisionEqualityCache == nil {
			t.Error("revisionEqualityCache is nil, want an initialised cache")
		}
	})

	t.Run("uses the first provider", func(t *testing.T) {
		// The variadic signature exists so callers can omit the provider entirely;
		// only the first one is ever honoured.
		r := NewLeaderWorkerSetReconciler(k8sClient, scheme, recorder, provider, &schedulerprovider.KubernetesProvider{})
		if r.SchedulerProvider != schedulerprovider.SchedulerProvider(provider) {
			t.Errorf("SchedulerProvider = %v, want %v", r.SchedulerProvider, provider)
		}
	})
}

// lwsSchedIgnoreTransitionTime drops LastTransitionTime, which is wall-clock based.
func lwsSchedIgnoreTransitionTime() cmp.Option {
	return cmpopts.IgnoreFields(metav1.Condition{}, "LastTransitionTime")
}

// lwsSchedFakeProvider is a SchedulerProvider that records what Reconcile asked of
// it and returns a canned error.
type lwsSchedFakeProvider struct {
	reconcileErr error

	calls       int
	gotReplicas int32
	gotRevision string
}

func (p *lwsSchedFakeProvider) ReconcileScheduling(_ context.Context, _ *leaderworkerset.LeaderWorkerSet, replicas int32, revision string) error {
	p.calls++
	p.gotReplicas = replicas
	p.gotRevision = revision
	return p.reconcileErr
}

func (p *lwsSchedFakeProvider) CreatePodGroupIfNotExists(context.Context, *leaderworkerset.LeaderWorkerSet, *corev1.Pod) error {
	return nil
}

func (p *lwsSchedFakeProvider) InjectPodGroupMetadata(*corev1.Pod) error { return nil }

// lwsSchedScheduledLWS returns an ordinal LWS that opts into workload-aware
// scheduling, which is what gates the Reconcile branch under test.
func lwsSchedScheduledLWS() *leaderworkerset.LeaderWorkerSet {
	lws := wrappers.BuildLeaderWorkerSet("default").Obj()
	lws.Spec.Scheduling = &leaderworkerset.LeaderWorkerSetScheduling{}
	return lws
}

func TestReconcileWorkloadScheduling(t *testing.T) {
	ctx := context.Background()

	t.Run("a missing lws is not an error", func(t *testing.T) {
		lws := lwsSchedScheduledLWS()
		r, _, _ := lwsSchedNewReconciler(t, interceptor.Funcs{}, lws)

		got, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: "does-not-exist", Namespace: "default"}})
		if err != nil {
			t.Errorf("Reconcile() = %v, want nil for a deleted object", err)
		}
		if got != (ctrl.Result{}) {
			t.Errorf("Reconcile() result = %v, want the zero result", got)
		}
	})

	t.Run("a deleting lws is skipped", func(t *testing.T) {
		lws := lwsSchedScheduledLWS()
		lws.Finalizers = []string{"lws.example.com/test"}
		lws.DeletionTimestamp = ptr.To(metav1.Now())
		provider := &lwsSchedFakeProvider{}
		r, _, _ := lwsSchedNewReconciler(t, interceptor.Funcs{}, lws)
		r.SchedulerProvider = provider

		if _, err := r.Reconcile(ctx, lwsSchedRequest(lws)); err != nil {
			t.Fatalf("Reconcile() = %v, want nil", err)
		}
		if provider.calls != 0 {
			t.Errorf("provider called %d times, want 0 while the object is being deleted", provider.calls)
		}
	})

	t.Run("spec.scheduling without a provider fails the condition", func(t *testing.T) {
		lws := lwsSchedScheduledLWS()
		r, k8sClient, recorder := lwsSchedNewReconciler(t, interceptor.Funcs{}, lws)
		// SchedulerProvider is deliberately left nil: the operator asked for
		// scheduling but the controller was not started with a provider.

		_, err := r.Reconcile(ctx, lwsSchedRequest(lws))
		if err == nil {
			t.Fatal("Reconcile() = nil, want an error")
		}
		if !strings.Contains(err.Error(), "requires a configured scheduler provider") {
			t.Errorf("error %q does not explain the missing provider", err)
		}

		got := lwsSchedGetCondition(t, k8sClient, lws)
		if got == nil {
			t.Fatalf("condition %s not found", lwsSchedConditionType)
		}
		if got.Status != metav1.ConditionFalse || got.Reason != schedulerprovider.ReasonUnsupportedProviderCapability {
			t.Errorf("condition = (%s, %s), want (False, %s)", got.Status, got.Reason, schedulerprovider.ReasonUnsupportedProviderCapability)
		}
		// Reconcile also emits a Normal CreatingRevision event before reaching the
		// scheduling branch, so assert on the warning specifically.
		wantEvent := corev1.EventTypeWarning + " " + schedulerprovider.ReasonUnsupportedProviderCapability
		if !slices.ContainsFunc(podCtrlDrainEvents(recorder), func(e string) bool { return strings.HasPrefix(e, wantEvent) }) {
			t.Errorf("no event starting with %q was recorded", wantEvent)
		}
	})

	t.Run("a provider failure propagates its reason", func(t *testing.T) {
		lws := lwsSchedScheduledLWS()
		provider := &lwsSchedFakeProvider{
			reconcileErr: schedulerprovider.NewReconcileError(schedulerprovider.ReasonPodGroupCreateFailed, errors.New("podgroup rejected")),
		}
		r, k8sClient, _ := lwsSchedNewReconciler(t, interceptor.Funcs{}, lws)
		r.SchedulerProvider = provider

		_, err := r.Reconcile(ctx, lwsSchedRequest(lws))
		if err == nil {
			t.Fatal("Reconcile() = nil, want the provider error")
		}
		if !strings.Contains(err.Error(), "podgroup rejected") {
			t.Errorf("error %q does not carry the provider failure", err)
		}

		got := lwsSchedGetCondition(t, k8sClient, lws)
		if got == nil {
			t.Fatalf("condition %s not found", lwsSchedConditionType)
		}
		// The reason is lifted off the ReconcileError rather than defaulted.
		if got.Status != metav1.ConditionFalse || got.Reason != schedulerprovider.ReasonPodGroupCreateFailed {
			t.Errorf("condition = (%s, %s), want (False, %s)", got.Status, got.Reason, schedulerprovider.ReasonPodGroupCreateFailed)
		}
	})

	t.Run("a successful provider marks the condition true", func(t *testing.T) {
		lws := lwsSchedScheduledLWS()
		provider := &lwsSchedFakeProvider{}
		r, k8sClient, _ := lwsSchedNewReconciler(t, interceptor.Funcs{}, lws)
		r.SchedulerProvider = provider

		if _, err := r.Reconcile(ctx, lwsSchedRequest(lws)); err != nil {
			t.Fatalf("Reconcile() = %v, want nil", err)
		}

		if provider.calls != 1 {
			t.Fatalf("provider called %d times, want 1", provider.calls)
		}
		// Scheduling prerequisites must be created for the full replica count and
		// tagged with the revision the reconcile is rolling out.
		if provider.gotReplicas != *lws.Spec.Replicas {
			t.Errorf("provider got replicas = %d, want %d", provider.gotReplicas, *lws.Spec.Replicas)
		}
		if provider.gotRevision == "" {
			t.Error("provider got an empty revision key, want the rollout revision")
		}

		got := lwsSchedGetCondition(t, k8sClient, lws)
		if got == nil {
			t.Fatalf("condition %s not found", lwsSchedConditionType)
		}
		if got.Status != metav1.ConditionTrue || got.Reason != "SchedulingPrerequisitesCreated" {
			t.Errorf("condition = (%s, %s), want (True, SchedulingPrerequisitesCreated)", got.Status, got.Reason)
		}
	})

	t.Run("no provider is consulted without spec.scheduling", func(t *testing.T) {
		lws := wrappers.BuildLeaderWorkerSet("default").Obj()
		provider := &lwsSchedFakeProvider{reconcileErr: errors.New("must not be called")}
		r, k8sClient, _ := lwsSchedNewReconciler(t, interceptor.Funcs{}, lws)
		r.SchedulerProvider = provider

		if _, err := r.Reconcile(ctx, lwsSchedRequest(lws)); err != nil {
			t.Fatalf("Reconcile() = %v, want nil", err)
		}
		if provider.calls != 0 {
			t.Errorf("provider called %d times, want 0 when spec.scheduling is unset", provider.calls)
		}
		if got := lwsSchedGetCondition(t, k8sClient, lws); got != nil {
			t.Errorf("condition = %v, want it absent when scheduling is not requested", got)
		}
	})
}

// lwsSchedRequest builds the reconcile request for lws.
func lwsSchedRequest(lws *leaderworkerset.LeaderWorkerSet) ctrl.Request {
	return ctrl.Request{NamespacedName: types.NamespacedName{Name: lws.Name, Namespace: lws.Namespace}}
}

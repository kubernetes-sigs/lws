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
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	appsapplyv1 "k8s.io/client-go/applyconfigurations/apps/v1"
	coreapplyv1 "k8s.io/client-go/applyconfigurations/core/v1"
	"k8s.io/client-go/tools/events"
	"k8s.io/client-go/util/workqueue"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"

	leaderworkerset "sigs.k8s.io/lws/api/leaderworkerset/v1"
	revisionutils "sigs.k8s.io/lws/pkg/utils/revision"
	"sigs.k8s.io/lws/test/wrappers"
)

// podCtrlTestScheme returns a scheme with every type the pod controller touches.
func podCtrlTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{
		corev1.AddToScheme,
		appsv1.AddToScheme,
		leaderworkerset.AddToScheme,
	} {
		if err := add(scheme); err != nil {
			t.Fatalf("building scheme: %v", err)
		}
	}
	return scheme
}

// podCtrlDrainEvents returns every event the recorder holds without blocking.
func podCtrlDrainEvents(recorder *events.FakeRecorder) []string {
	var got []string
	for {
		select {
		case e := <-recorder.Events:
			got = append(got, e)
		default:
			return got
		}
	}
}

func podCtrlNewQueue(t *testing.T) workqueue.TypedRateLimitingInterface[podReconcileRequest] {
	t.Helper()
	queue := workqueue.NewTypedRateLimitingQueue(
		workqueue.DefaultTypedControllerRateLimiter[podReconcileRequest](),
	)
	t.Cleanup(queue.ShutDown)
	return queue
}

// podCtrlDrainQueue returns the queued requests sorted by name, so assertions do
// not depend on the order the handler walked the listed pods.
func podCtrlDrainQueue(t *testing.T, queue workqueue.TypedRateLimitingInterface[podReconcileRequest]) []podReconcileRequest {
	t.Helper()
	var got []podReconcileRequest
	for queue.Len() > 0 {
		request, shutdown := queue.Get()
		if shutdown {
			t.Fatal("queue shut down before all requests were read")
		}
		got = append(got, request)
		queue.Done(request)
	}
	sort.Slice(got, func(i, j int) bool { return got[i].Name < got[j].Name })
	return got
}

func podCtrlRequestNames(requests []podReconcileRequest) []string {
	names := make([]string, 0, len(requests))
	for _, request := range requests {
		names = append(names, request.Name)
	}
	return names
}

// podCtrlBasicLWS builds a LeaderWorkerSet with a worker template and no
// per-replica service, so reconciliation reaches the worker statefulset logic.
func podCtrlBasicLWS() *leaderworkerset.LeaderWorkerSet {
	return wrappers.BuildBasicLeaderWorkerSet("test-lws", "default").
		Replica(1).
		Size(2).
		WorkerTemplateSpec(wrappers.MakeWorkerPodSpec()).
		Obj()
}

// podCtrlLeaderPod builds a schedulable leader pod of lws with a DNS identity.
func podCtrlLeaderPod(lws *leaderworkerset.LeaderWorkerSet, name string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: lws.Namespace,
			UID:       types.UID(name),
			Labels: map[string]string{
				leaderworkerset.SetNameLabelKey:         lws.Name,
				leaderworkerset.WorkerIndexLabelKey:     "0",
				leaderworkerset.GroupIndexLabelKey:      "0",
				leaderworkerset.GroupUniqueHashLabelKey: "group-0",
			},
		},
		Spec: corev1.PodSpec{Hostname: name, Subdomain: lws.Name},
	}
}

// podCtrlRevisionFor builds the ControllerRevision the reconciler expects to
// find for lws, and stamps its key on pods so they resolve to it.
func podCtrlRevisionFor(t *testing.T, scheme *runtime.Scheme, lws *leaderworkerset.LeaderWorkerSet, pods ...*corev1.Pod) *appsv1.ControllerRevision {
	t.Helper()
	revision, err := revisionutils.NewRevision(context.Background(), fake.NewClientBuilder().WithScheme(scheme).Build(), lws, "")
	if err != nil {
		t.Fatalf("building revision: %v", err)
	}
	for _, pod := range pods {
		pod.Labels[leaderworkerset.RevisionKey] = revisionutils.GetRevisionKey(revision)
	}
	return revision
}

func podCtrlGetStatefulSet(t *testing.T, c client.Client, key client.ObjectKey) (*appsv1.StatefulSet, bool) {
	t.Helper()
	var sts appsv1.StatefulSet
	err := c.Get(context.Background(), key, &sts)
	if apierrors.IsNotFound(err) {
		return nil, false
	}
	if err != nil {
		t.Fatalf("getting worker statefulset: %v", err)
	}
	return &sts, true
}

func TestPodCtrlNewPodReconciler(t *testing.T) {
	scheme := podCtrlTestScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	recorder := events.NewFakeRecorder(1)
	provider := &stubSchedulerProvider{}

	reconciler := NewPodReconciler(c, scheme, recorder, provider)

	if reconciler.Client != c {
		t.Errorf("NewPodReconciler() Client = %v, want the client passed in", reconciler.Client)
	}
	if reconciler.Scheme != scheme {
		t.Errorf("NewPodReconciler() Scheme = %v, want the scheme passed in", reconciler.Scheme)
	}
	if reconciler.Record != recorder {
		t.Errorf("NewPodReconciler() Record = %v, want the recorder passed in", reconciler.Record)
	}
	if reconciler.SchedulerProvider != provider {
		t.Errorf("NewPodReconciler() SchedulerProvider = %v, want the provider passed in", reconciler.SchedulerProvider)
	}
}

func TestPodCtrlSyncGroupReadyCondition(t *testing.T) {
	scheme := podCtrlTestScheme(t)
	podReady := corev1.PodCondition{Type: corev1.PodReady, Status: corev1.ConditionTrue}
	groupReady := func(status corev1.ConditionStatus, reason string) corev1.PodCondition {
		return corev1.PodCondition{Type: leaderworkerset.GroupReadyConditionType, Status: status, Reason: reason}
	}

	tests := []struct {
		name           string
		conditions     []corev1.PodCondition
		ready          bool
		wantConditions []corev1.PodCondition
		wantPatches    int
	}{
		{
			name:           "adds a false condition when the worker statefulset is not ready",
			ready:          false,
			wantConditions: []corev1.PodCondition{groupReady(corev1.ConditionFalse, "WorkerStatefulSetNotReady")},
			wantPatches:    1,
		},
		{
			name:           "adds a true condition when the worker statefulset is ready",
			ready:          true,
			wantConditions: []corev1.PodCondition{groupReady(corev1.ConditionTrue, "WorkerStatefulSetReady")},
			wantPatches:    1,
		},
		{
			name:           "flips an existing condition in place and keeps the other conditions",
			conditions:     []corev1.PodCondition{podReady, groupReady(corev1.ConditionTrue, "WorkerStatefulSetReady")},
			ready:          false,
			wantConditions: []corev1.PodCondition{podReady, groupReady(corev1.ConditionFalse, "WorkerStatefulSetNotReady")},
			wantPatches:    1,
		},
		{
			name:           "does not patch when the condition already matches",
			conditions:     []corev1.PodCondition{groupReady(corev1.ConditionTrue, "WorkerStatefulSetReady")},
			ready:          true,
			wantConditions: []corev1.PodCondition{groupReady(corev1.ConditionTrue, "WorkerStatefulSetReady")},
			wantPatches:    0,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			lws := podCtrlBasicLWS()
			pod := podCtrlLeaderPod(lws, "test-lws-0")
			pod.Status.Conditions = tc.conditions

			patches := 0
			c := fake.NewClientBuilder().
				WithScheme(scheme).
				WithObjects(pod).
				WithStatusSubresource(&corev1.Pod{}).
				WithInterceptorFuncs(interceptor.Funcs{
					SubResourcePatch: func(ctx context.Context, cl client.Client, subResourceName string, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
						patches++
						return cl.Status().Patch(ctx, obj, patch, opts...)
					},
				}).
				Build()
			reconciler := &PodReconciler{Client: c, Scheme: scheme, Record: events.NewFakeRecorder(1)}

			if err := reconciler.syncGroupReadyCondition(context.Background(), pod.DeepCopy(), tc.ready); err != nil {
				t.Fatalf("syncGroupReadyCondition() error = %v", err)
			}
			if patches != tc.wantPatches {
				t.Errorf("syncGroupReadyCondition() issued %d status patches, want %d", patches, tc.wantPatches)
			}

			var stored corev1.Pod
			if err := c.Get(context.Background(), client.ObjectKeyFromObject(pod), &stored); err != nil {
				t.Fatalf("getting patched pod: %v", err)
			}
			if diff := cmp.Diff(tc.wantConditions, stored.Status.Conditions,
				cmpopts.IgnoreFields(corev1.PodCondition{}, "LastTransitionTime")); diff != "" {
				t.Errorf("unexpected pod conditions (-want,+got):\n%s", diff)
			}
		})
	}
}

func TestPodCtrlSyncGroupReadyConditionReturnsPatchError(t *testing.T) {
	scheme := podCtrlTestScheme(t)
	lws := podCtrlBasicLWS()
	pod := podCtrlLeaderPod(lws, "test-lws-0")
	wantErr := errors.New("patch rejected")

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(pod).
		WithStatusSubresource(&corev1.Pod{}).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourcePatch: func(context.Context, client.Client, string, client.Object, client.Patch, ...client.SubResourcePatchOption) error {
				return wantErr
			},
		}).
		Build()
	reconciler := &PodReconciler{Client: c, Scheme: scheme, Record: events.NewFakeRecorder(1)}

	if err := reconciler.syncGroupReadyCondition(context.Background(), pod, true); !errors.Is(err, wantErr) {
		t.Fatalf("syncGroupReadyCondition() error = %v, want %v", err, wantErr)
	}
}

func TestPodCtrlEnqueueGatedLeaders(t *testing.T) {
	scheme := podCtrlTestScheme(t)
	lws := podCtrlBasicLWS()

	pod := func(name, setName, workerIndex string, gated bool) *corev1.Pod {
		p := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: lws.Namespace,
				UID:       types.UID(name),
				Labels: map[string]string{
					leaderworkerset.SetNameLabelKey:     setName,
					leaderworkerset.WorkerIndexLabelKey: workerIndex,
				},
			},
		}
		if gated {
			p.Spec.SchedulingGates = []corev1.PodSchedulingGate{{Name: leaderworkerset.GroupReplacementSchedulingGate}}
		}
		return p
	}

	gatedLeader := pod("gated-leader", lws.Name, "0", true)
	otherGatedLeader := pod("another-gated-leader", lws.Name, "0", true)
	runningLeader := pod("running-leader", lws.Name, "0", false)
	gatedWorker := pod("gated-worker", lws.Name, "1", true)
	foreignLeader := pod("foreign-leader", "other-lws", "0", true)
	deletedWorker := pod("deleted-worker", lws.Name, "1", false)

	tests := []struct {
		name      string
		objects   []client.Object
		changed   client.Object
		listErr   error
		wantNames []string
	}{
		{
			name:      "a deleted worker wakes up every gated leader of its set",
			objects:   []client.Object{gatedLeader, otherGatedLeader, runningLeader, gatedWorker, foreignLeader},
			changed:   deletedWorker,
			wantNames: []string{"another-gated-leader", "gated-leader"},
		},
		{
			name:    "sets without gated leaders enqueue nothing",
			objects: []client.Object{runningLeader, gatedWorker},
			changed: deletedWorker,
		},
		{
			name:    "non pod objects are ignored",
			objects: []client.Object{gatedLeader},
			changed: &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: "test-lws-0", Namespace: lws.Namespace}},
		},
		{
			name:    "a failed list enqueues nothing",
			objects: []client.Object{gatedLeader},
			changed: deletedWorker,
			listErr: errors.New("list failed"),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			builder := fake.NewClientBuilder().WithScheme(scheme).WithObjects(tc.objects...)
			if tc.listErr != nil {
				builder = builder.WithInterceptorFuncs(interceptor.Funcs{
					List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
						return tc.listErr
					},
				})
			}
			reconciler := &PodReconciler{Client: builder.Build(), Scheme: scheme, Record: events.NewFakeRecorder(1)}
			queue := podCtrlNewQueue(t)

			reconciler.enqueueGatedLeaders(context.Background(), tc.changed, queue)

			got := podCtrlDrainQueue(t, queue)
			if diff := cmp.Diff(tc.wantNames, podCtrlRequestNames(got), cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("unexpected enqueued leaders (-want,+got):\n%s", diff)
			}
			for _, request := range got {
				if request.DeletedPod != nil {
					t.Errorf("gated leader %s was enqueued as deleted", request.Name)
				}
				if request.UID == "" {
					t.Errorf("gated leader %s was enqueued without its UID", request.Name)
				}
			}
		})
	}
}

func TestPodCtrlReconcilePodEarlyReturns(t *testing.T) {
	scheme := podCtrlTestScheme(t)

	withLWS := func(mutate func(*leaderworkerset.LeaderWorkerSet)) *leaderworkerset.LeaderWorkerSet {
		lws := podCtrlBasicLWS()
		if mutate != nil {
			mutate(lws)
		}
		return lws
	}
	withPod := func(lws *leaderworkerset.LeaderWorkerSet, mutate func(*corev1.Pod)) *corev1.Pod {
		pod := podCtrlLeaderPod(lws, "test-lws-0")
		if mutate != nil {
			mutate(pod)
		}
		return pod
	}
	terminatingLeader := func(lws *leaderworkerset.LeaderWorkerSet) *corev1.Pod {
		pod := podCtrlLeaderPod(lws, "test-lws-old")
		pod.Labels[leaderworkerset.GroupIndexLabelKey] = "1"
		now := metav1.Now()
		pod.DeletionTimestamp = &now
		pod.Finalizers = []string{"foregroundDeletion"}
		return pod
	}

	lwsNoRestart := withLWS(nil)
	lwsSizeOne := withLWS(func(l *leaderworkerset.LeaderWorkerSet) { *l.Spec.LeaderWorkerTemplate.Size = 1 })
	lwsLeaderReady := withLWS(func(l *leaderworkerset.LeaderWorkerSet) {
		l.Spec.StartupPolicy = leaderworkerset.LeaderReadyStartupPolicy
	})
	lwsPostTermination := withLWS(func(l *leaderworkerset.LeaderWorkerSet) {
		l.Spec.GroupReplacementPolicy = leaderworkerset.GroupReplacementPostTermination
	})

	tests := []struct {
		name              string
		lws               *leaderworkerset.LeaderWorkerSet
		pod               *corev1.Pod
		extraObjects      []client.Object
		omitPod           bool
		omitLWS           bool
		deletedPodRequest bool
		listErr           error
		wantErrContains   string
		wantResult        ctrl.Result
		wantEventContains string
	}{
		{
			name: "missing set name label is a hard error",
			lws:  lwsNoRestart,
			pod: withPod(lwsNoRestart, func(p *corev1.Pod) {
				delete(p.Labels, leaderworkerset.SetNameLabelKey)
			}),
			deletedPodRequest: true,
			wantErrContains:   "leaderworkerset.sigs.k8s.io/name label",
		},
		{
			name: "missing worker index label is a hard error",
			lws:  lwsNoRestart,
			pod: withPod(lwsNoRestart, func(p *corev1.Pod) {
				delete(p.Labels, leaderworkerset.WorkerIndexLabelKey)
			}),
			deletedPodRequest: true,
			wantErrContains:   "leaderworkerset.sigs.k8s.io/worker-index label",
		},
		{
			name:    "a pod that is already gone is not an error",
			lws:     lwsNoRestart,
			pod:     withPod(lwsNoRestart, nil),
			omitPod: true,
		},
		{
			name:    "a deleted leaderworkerset is not an error",
			lws:     lwsNoRestart,
			pod:     withPod(lwsNoRestart, nil),
			omitLWS: true,
		},
		{
			name: "worker pods are only reconciled for the restart policy",
			lws:  lwsNoRestart,
			pod: withPod(lwsNoRestart, func(p *corev1.Pod) {
				p.Name = "test-lws-0-1"
				p.Labels[leaderworkerset.WorkerIndexLabelKey] = "1"
			}),
		},
		{
			name: "a leader carrying the leader name annotation is rejected with an event",
			lws:  lwsNoRestart,
			pod: withPod(lwsNoRestart, func(p *corev1.Pod) {
				p.Annotations = map[string]string{leaderworkerset.LeaderPodNameAnnotationKey: "test-lws-0"}
			}),
			wantEventContains: "Warning " + FailedCreate,
		},
		{
			name: "size one groups do not get a worker statefulset",
			lws:  lwsSizeOne,
			pod:  withPod(lwsSizeOne, nil),
		},
		{
			name: "a not ready leader defers worker creation under LeaderReady",
			lws:  lwsLeaderReady,
			pod:  withPod(lwsLeaderReady, nil),
		},
		{
			name: "a gated leader waits while another group tears down",
			lws:  lwsPostTermination,
			pod: withPod(lwsPostTermination, func(p *corev1.Pod) {
				p.Spec.SchedulingGates = []corev1.PodSchedulingGate{{Name: leaderworkerset.GroupReplacementSchedulingGate}}
			}),
			extraObjects:      []client.Object{terminatingLeader(lwsPostTermination)},
			wantResult:        ctrl.Result{RequeueAfter: groupReplacementRequeueDelay},
			wantEventContains: GroupReplacementDeferred,
		},
		{
			name:       "a leader without a revision is requeued",
			lws:        lwsNoRestart,
			pod:        withPod(lwsNoRestart, nil),
			wantResult: ctrl.Result{Requeue: true, RequeueAfter: time.Second},
		},
		{
			name: "a terminating leader does not build its group",
			lws:  lwsNoRestart,
			pod: withPod(lwsNoRestart, func(p *corev1.Pod) {
				now := metav1.Now()
				p.DeletionTimestamp = &now
				p.Finalizers = []string{"leaderworkerset.sigs.k8s.io/test"}
			}),
		},
		{
			name: "a failed revision lookup is retried",
			lws:  lwsNoRestart,
			pod: withPod(lwsNoRestart, func(p *corev1.Pod) {
				p.Labels[leaderworkerset.RevisionKey] = "revision-1"
			}),
			listErr:         errors.New("list failed"),
			wantErrContains: "list failed",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			objects := append([]client.Object{}, tc.extraObjects...)
			if !tc.omitLWS {
				objects = append(objects, tc.lws.DeepCopy())
			}
			if !tc.omitPod {
				objects = append(objects, tc.pod.DeepCopy())
			}
			builder := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...)
			if tc.listErr != nil {
				builder = builder.WithInterceptorFuncs(interceptor.Funcs{
					List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
						return tc.listErr
					},
				})
			}
			c := builder.Build()
			recorder := events.NewFakeRecorder(10)
			reconciler := &PodReconciler{Client: c, Scheme: scheme, Record: recorder}

			result, err := reconciler.reconcilePod(context.Background(), podReconcileRequestForPod(tc.pod, tc.deletedPodRequest))

			switch {
			case tc.wantErrContains == "" && err != nil:
				t.Fatalf("reconcilePod() error = %v, want nil", err)
			case tc.wantErrContains != "" && err == nil:
				t.Fatalf("reconcilePod() error = nil, want an error containing %q", tc.wantErrContains)
			case tc.wantErrContains != "" && !strings.Contains(err.Error(), tc.wantErrContains):
				t.Fatalf("reconcilePod() error = %q, want it to contain %q", err.Error(), tc.wantErrContains)
			}
			if diff := cmp.Diff(tc.wantResult, result); diff != "" {
				t.Errorf("unexpected result (-want,+got):\n%s", diff)
			}
			if _, found := podCtrlGetStatefulSet(t, c, client.ObjectKey{Name: tc.pod.Name, Namespace: tc.pod.Namespace}); found {
				t.Error("reconcilePod() created a worker statefulset, want none")
			}

			gotEvents := podCtrlDrainEvents(recorder)
			if tc.wantEventContains == "" {
				if len(gotEvents) != 0 {
					t.Errorf("reconcilePod() recorded unexpected events: %v", gotEvents)
				}
				return
			}
			if len(gotEvents) != 1 || !strings.Contains(gotEvents[0], tc.wantEventContains) {
				t.Errorf("reconcilePod() events = %v, want exactly one containing %q", gotEvents, tc.wantEventContains)
			}
		})
	}
}

func TestPodCtrlReconcilePodCreatesWorkerStatefulSet(t *testing.T) {
	scheme := podCtrlTestScheme(t)
	lws := podCtrlBasicLWS()
	leader := podCtrlLeaderPod(lws, "test-lws-0")
	revision := podCtrlRevisionFor(t, scheme, lws, leader)

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(lws, leader, revision).Build()
	recorder := events.NewFakeRecorder(10)
	reconciler := &PodReconciler{Client: c, Scheme: scheme, Record: recorder}

	result, err := reconciler.reconcilePod(context.Background(), podReconcileRequestForPod(leader, false))
	if err != nil {
		t.Fatalf("reconcilePod() error = %v", err)
	}
	if !result.IsZero() {
		t.Fatalf("reconcilePod() result = %+v, want zero", result)
	}

	sts, found := podCtrlGetStatefulSet(t, c, client.ObjectKeyFromObject(leader))
	if !found {
		t.Fatal("reconcilePod() did not create the worker statefulset")
	}
	if !metav1.IsControlledBy(sts, leader) {
		t.Errorf("worker statefulset is not controlled by the leader pod: %v", sts.OwnerReferences)
	}
	wantAddress := leader.Spec.Hostname + "." + leader.Spec.Subdomain + "." + leader.Namespace
	if got := sts.Spec.Template.Annotations[leaderworkerset.LeaderAddressAnnotationKey]; got != wantAddress {
		t.Errorf("worker template leader address = %q, want %q", got, wantAddress)
	}
	if _, set := sts.Spec.Template.Annotations[leaderworkerset.GroupIdentityAnnotationKey]; set {
		t.Errorf("ordinal identity should not stamp the group identity annotation: %v", sts.Spec.Template.Annotations)
	}

	gotEvents := podCtrlDrainEvents(recorder)
	if len(gotEvents) != 1 || !strings.Contains(gotEvents[0], GroupsProgressing) {
		t.Errorf("reconcilePod() events = %v, want exactly one %s event", gotEvents, GroupsProgressing)
	}

	// Ordinal identity leaves the group-ready condition alone: the readiness gate
	// only exists in hash mode.
	var stored corev1.Pod
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(leader), &stored); err != nil {
		t.Fatalf("getting leader pod: %v", err)
	}
	if len(stored.Status.Conditions) != 0 {
		t.Errorf("leader pod conditions = %v, want none", stored.Status.Conditions)
	}
}

func TestPodCtrlReconcilePodWorkerStatefulSetCreateFailures(t *testing.T) {
	tests := []struct {
		name              string
		createErr         error
		wantErrContains   string
		wantEventContains string
	}{
		{
			name:      "a lost create race is tolerated silently",
			createErr: apierrors.NewAlreadyExists(appsv1.Resource("statefulsets"), "test-lws-0"),
		},
		{
			name:              "an API failure is reported and retried",
			createErr:         apierrors.NewInternalError(errors.New("etcd unavailable")),
			wantErrContains:   "etcd unavailable",
			wantEventContains: "Warning " + FailedCreate,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			scheme := podCtrlTestScheme(t)
			lws := podCtrlBasicLWS()
			leader := podCtrlLeaderPod(lws, "test-lws-0")
			revision := podCtrlRevisionFor(t, scheme, lws, leader)

			c := fake.NewClientBuilder().
				WithScheme(scheme).
				WithObjects(lws, leader, revision).
				WithInterceptorFuncs(interceptor.Funcs{
					Create: func(context.Context, client.WithWatch, client.Object, ...client.CreateOption) error {
						return tc.createErr
					},
				}).
				Build()
			recorder := events.NewFakeRecorder(10)
			reconciler := &PodReconciler{Client: c, Scheme: scheme, Record: recorder}

			_, err := reconciler.reconcilePod(context.Background(), podReconcileRequestForPod(leader, false))
			switch {
			case tc.wantErrContains == "" && err != nil:
				t.Fatalf("reconcilePod() error = %v, want nil", err)
			case tc.wantErrContains != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErrContains)):
				t.Fatalf("reconcilePod() error = %v, want an error containing %q", err, tc.wantErrContains)
			}

			gotEvents := podCtrlDrainEvents(recorder)
			if tc.wantEventContains == "" {
				if len(gotEvents) != 0 {
					t.Errorf("reconcilePod() recorded unexpected events: %v", gotEvents)
				}
				return
			}
			if len(gotEvents) != 1 || !strings.Contains(gotEvents[0], tc.wantEventContains) {
				t.Errorf("reconcilePod() events = %v, want exactly one containing %q", gotEvents, tc.wantEventContains)
			}
		})
	}
}

func TestPodCtrlReconcilePodSyncsGroupReadyConditionInHashMode(t *testing.T) {
	scheme := podCtrlTestScheme(t)
	ctx := context.Background()
	lws := podCtrlBasicLWS()
	lws.Spec.GroupIdentity = leaderworkerset.GroupIdentityHash
	leader := podCtrlLeaderPod(lws, "test-lws-9f2ac71b")
	revision := podCtrlRevisionFor(t, scheme, lws, leader)

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(lws, leader, revision).
		WithStatusSubresource(&corev1.Pod{}).
		Build()
	reconciler := &PodReconciler{Client: c, Scheme: scheme, Record: events.NewFakeRecorder(10)}
	request := podReconcileRequestForPod(leader, false)

	groupReadyStatus := func() corev1.ConditionStatus {
		t.Helper()
		var stored corev1.Pod
		if err := c.Get(ctx, client.ObjectKeyFromObject(leader), &stored); err != nil {
			t.Fatalf("getting leader pod: %v", err)
		}
		for _, condition := range stored.Status.Conditions {
			if condition.Type == leaderworkerset.GroupReadyConditionType {
				return condition.Status
			}
		}
		t.Fatalf("leader pod has no %s condition: %v", leaderworkerset.GroupReadyConditionType, stored.Status.Conditions)
		return ""
	}

	// First pass creates the worker statefulset, which cannot be ready yet.
	if _, err := reconciler.reconcilePod(ctx, request); err != nil {
		t.Fatalf("reconcilePod() error = %v", err)
	}
	sts, found := podCtrlGetStatefulSet(t, c, client.ObjectKeyFromObject(leader))
	if !found {
		t.Fatal("reconcilePod() did not create the worker statefulset")
	}
	if got := sts.Spec.Template.Annotations[leaderworkerset.GroupIdentityAnnotationKey]; got != string(leaderworkerset.GroupIdentityHash) {
		t.Errorf("worker template group identity = %q, want %q", got, leaderworkerset.GroupIdentityHash)
	}
	if got := groupReadyStatus(); got != corev1.ConditionFalse {
		t.Errorf("group-ready condition = %s, want %s while the workers are not ready", got, corev1.ConditionFalse)
	}

	// Once the worker statefulset reports ready, the gate flips to true.
	sts.Status.AvailableReplicas = *sts.Spec.Replicas
	sts.Status.CurrentRevision = "worker-revision"
	sts.Status.UpdateRevision = "worker-revision"
	if err := c.Status().Update(ctx, sts); err != nil {
		t.Fatalf("updating worker statefulset status: %v", err)
	}
	if _, err := reconciler.reconcilePod(ctx, request); err != nil {
		t.Fatalf("reconcilePod() error = %v", err)
	}
	if got := groupReadyStatus(); got != corev1.ConditionTrue {
		t.Errorf("group-ready condition = %s, want %s once the workers are ready", got, corev1.ConditionTrue)
	}
}

func TestPodCtrlReconcilePodHashModeStartupUsesContainersReady(t *testing.T) {
	// In hash mode full pod readiness includes the group-ready gate, which waits
	// for the workers, so LeaderReady must key off container readiness instead.
	tests := []struct {
		name            string
		conditions      []corev1.PodCondition
		wantStatefulSet bool
	}{
		{
			name:            "containers ready starts the workers even though the pod is not ready",
			conditions:      []corev1.PodCondition{{Type: corev1.ContainersReady, Status: corev1.ConditionTrue}, {Type: corev1.PodReady, Status: corev1.ConditionFalse}},
			wantStatefulSet: true,
		},
		{
			name:       "containers not ready defers the workers",
			conditions: []corev1.PodCondition{{Type: corev1.ContainersReady, Status: corev1.ConditionFalse}},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			scheme := podCtrlTestScheme(t)
			lws := podCtrlBasicLWS()
			lws.Spec.GroupIdentity = leaderworkerset.GroupIdentityHash
			lws.Spec.StartupPolicy = leaderworkerset.LeaderReadyStartupPolicy
			leader := podCtrlLeaderPod(lws, "test-lws-9f2ac71b")
			leader.Status.Conditions = tc.conditions
			revision := podCtrlRevisionFor(t, scheme, lws, leader)

			c := fake.NewClientBuilder().
				WithScheme(scheme).
				WithObjects(lws, leader, revision).
				WithStatusSubresource(&corev1.Pod{}).
				Build()
			reconciler := &PodReconciler{Client: c, Scheme: scheme, Record: events.NewFakeRecorder(10)}

			if _, err := reconciler.reconcilePod(context.Background(), podReconcileRequestForPod(leader, false)); err != nil {
				t.Fatalf("reconcilePod() error = %v", err)
			}
			if _, found := podCtrlGetStatefulSet(t, c, client.ObjectKeyFromObject(leader)); found != tc.wantStatefulSet {
				t.Errorf("worker statefulset exists = %t, want %t", found, tc.wantStatefulSet)
			}
		})
	}
}

func TestPodCtrlReconcilePodExclusivePlacement(t *testing.T) {
	const topologyKey = "topology.kubernetes.io/zone"
	// The controller looks the node up with the pod's namespace, which a real API
	// server ignores for cluster-scoped objects. The fake client keys everything by
	// namespace, so the fixture has to live in the pod's namespace bucket.
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "node-a", Namespace: "default", Labels: map[string]string{topologyKey: "zone-a"}},
	}

	tests := []struct {
		name             string
		nodeName         string
		withNode         bool
		wantStatefulSet  bool
		wantNodeSelector map[string]string
		wantErrContains  string
	}{
		{
			name: "an unscheduled leader defers worker creation",
		},
		{
			name:             "a scheduled leader pins the workers to its topology",
			nodeName:         node.Name,
			withNode:         true,
			wantStatefulSet:  true,
			wantNodeSelector: map[string]string{topologyKey: "zone-a"},
		},
		{
			name:            "a missing node is retried",
			nodeName:        "gone",
			wantErrContains: "getting node",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			scheme := podCtrlTestScheme(t)
			lws := podCtrlBasicLWS()
			lws.Annotations = map[string]string{leaderworkerset.ExclusiveKeyAnnotationKey: topologyKey}
			leader := podCtrlLeaderPod(lws, "test-lws-0")
			leader.Spec.NodeName = tc.nodeName
			revision := podCtrlRevisionFor(t, scheme, lws, leader)

			objects := []client.Object{lws, leader, revision}
			if tc.withNode {
				objects = append(objects, node.DeepCopy())
			}
			c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
			reconciler := &PodReconciler{Client: c, Scheme: scheme, Record: events.NewFakeRecorder(10)}

			_, err := reconciler.reconcilePod(context.Background(), podReconcileRequestForPod(leader, false))
			switch {
			case tc.wantErrContains == "" && err != nil:
				t.Fatalf("reconcilePod() error = %v, want nil", err)
			case tc.wantErrContains != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErrContains)):
				t.Fatalf("reconcilePod() error = %v, want an error containing %q", err, tc.wantErrContains)
			}

			sts, found := podCtrlGetStatefulSet(t, c, client.ObjectKeyFromObject(leader))
			if found != tc.wantStatefulSet {
				t.Fatalf("worker statefulset exists = %t, want %t", found, tc.wantStatefulSet)
			}
			if !found {
				return
			}
			if diff := cmp.Diff(tc.wantNodeSelector, sts.Spec.Template.Spec.NodeSelector); diff != "" {
				t.Errorf("unexpected worker node selector (-want,+got):\n%s", diff)
			}
		})
	}
}

func TestPodCtrlReconcilePodRequiresLeaderDNSIdentity(t *testing.T) {
	scheme := podCtrlTestScheme(t)
	lws := podCtrlBasicLWS()
	leader := podCtrlLeaderPod(lws, "test-lws-0")
	leader.Spec.Hostname = ""
	leader.Spec.Subdomain = ""
	revision := podCtrlRevisionFor(t, scheme, lws, leader)

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(lws, leader, revision).Build()
	reconciler := &PodReconciler{Client: c, Scheme: scheme, Record: events.NewFakeRecorder(10)}

	_, err := reconciler.reconcilePod(context.Background(), podReconcileRequestForPod(leader, false))
	if err == nil || !strings.Contains(err.Error(), "has no hostname or subdomain") {
		t.Fatalf("reconcilePod() error = %v, want an error about the missing DNS identity", err)
	}
	if _, found := podCtrlGetStatefulSet(t, c, client.ObjectKeyFromObject(leader)); found {
		t.Error("reconcilePod() created a worker statefulset for a leader without a DNS identity")
	}
}

func TestPodCtrlHandleRestartPolicy(t *testing.T) {
	scheme := podCtrlTestScheme(t)
	const revisionKey = "revision-1"

	lwsWith := func(policy leaderworkerset.RestartPolicyType, mutate func(*leaderworkerset.LeaderWorkerSet)) *leaderworkerset.LeaderWorkerSet {
		lws := wrappers.BuildLeaderWorkerSet("default").Replica(1).Size(2).RestartPolicy(policy).Obj()
		if mutate != nil {
			mutate(lws)
		}
		return lws
	}
	leaderPod := func(mutate func(*corev1.Pod)) *corev1.Pod {
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-sample-0",
				Namespace: "default",
				UID:       "leader-current",
				Labels: map[string]string{
					leaderworkerset.SetNameLabelKey:     "test-sample",
					leaderworkerset.WorkerIndexLabelKey: "0",
					leaderworkerset.GroupIndexLabelKey:  "0",
					leaderworkerset.RevisionKey:         revisionKey,
				},
			},
		}
		if mutate != nil {
			mutate(pod)
		}
		return pod
	}
	workerPod := func(mutate func(*corev1.Pod)) *corev1.Pod {
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-sample-0-1",
				Namespace: "default",
				UID:       "worker-current",
				Labels: map[string]string{
					leaderworkerset.SetNameLabelKey:     "test-sample",
					leaderworkerset.WorkerIndexLabelKey: "1",
					leaderworkerset.GroupIndexLabelKey:  "0",
					leaderworkerset.RevisionKey:         revisionKey,
				},
			},
		}
		if mutate != nil {
			mutate(pod)
		}
		return pod
	}
	deleting := func(pod *corev1.Pod) *corev1.Pod {
		now := metav1.Now()
		pod.DeletionTimestamp = &now
		pod.Finalizers = []string{"leaderworkerset.sigs.k8s.io/test"}
		return pod
	}
	ownedByLeader := func(leader *corev1.Pod) func(*corev1.Pod) {
		return func(pod *corev1.Pod) {
			pod.OwnerReferences = []metav1.OwnerReference{*metav1.NewControllerRef(leader, corev1.SchemeGroupVersion.WithKind("Pod"))}
		}
	}

	recreateOnRestart := lwsWith(leaderworkerset.RecreateGroupOnPodRestart, nil)

	tests := []struct {
		name            string
		lws             *leaderworkerset.LeaderWorkerSet
		pod             *corev1.Pod
		objects         []client.Object
		listErr         error
		wantDeleted     bool
		wantErrContains string
		wantLeaderGone  bool
		wantEvent       bool
	}{
		{
			name:    "a policy that never recreates the group does nothing",
			lws:     lwsWith(leaderworkerset.NoneRestartPolicy, nil),
			pod:     deleting(workerPod(nil)),
			objects: []client.Object{leaderPod(nil)},
		},
		{
			name:    "a healthy worker does not recreate the group",
			lws:     recreateOnRestart,
			pod:     workerPod(nil),
			objects: []client.Object{leaderPod(nil)},
		},
		{
			name: "a restarted leader container recreates the group",
			lws:  recreateOnRestart,
			pod: leaderPod(func(p *corev1.Pod) {
				p.Status.Phase = corev1.PodRunning
				p.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "leader", RestartCount: 1}}
			}),
			objects:        []client.Object{leaderPod(nil), workerPod(nil)},
			wantDeleted:    true,
			wantLeaderGone: true,
			wantEvent:      true,
		},
		{
			name: "a pending group member defers recreation under RecreateGroupAfterStart",
			lws:  lwsWith(leaderworkerset.RecreateGroupAfterStart, nil),
			pod:  deleting(workerPod(nil)),
			objects: []client.Object{
				leaderPod(func(p *corev1.Pod) { p.Status.Phase = corev1.PodPending }),
				workerPod(nil),
			},
		},
		{
			name: "the RecreateGroupAfterStart annotation defers recreation too",
			lws: lwsWith(leaderworkerset.RecreateGroupOnPodRestart, func(l *leaderworkerset.LeaderWorkerSet) {
				l.Annotations = map[string]string{leaderworkerset.RecreateGroupAfterStartAnnotationKey: "true"}
			}),
			pod: deleting(workerPod(nil)),
			objects: []client.Object{
				leaderPod(func(p *corev1.Pod) { p.Status.Phase = corev1.PodPending }),
				workerPod(nil),
			},
		},
		{
			name:            "a worker name that is not ordinal derived is an error",
			lws:             recreateOnRestart,
			pod:             deleting(workerPod(func(p *corev1.Pod) { p.Name = "worker-without-ordinal" })),
			objects:         []client.Object{leaderPod(nil), workerPod(nil)},
			wantErrContains: "parsing pod name",
		},
		{
			name:    "a worker whose leader is already gone is ignored",
			lws:     recreateOnRestart,
			pod:     deleting(workerPod(nil)),
			objects: []client.Object{workerPod(nil)},
		},
		{
			name: "a worker of a previous revision is ignored",
			lws:  recreateOnRestart,
			pod: deleting(workerPod(func(p *corev1.Pod) {
				p.Labels[leaderworkerset.RevisionKey] = "revision-0"
			})),
			objects: []client.Object{leaderPod(nil), workerPod(nil)},
		},
		{
			name: "the leader name annotation identifies the leader when names are hashed",
			lws:  recreateOnRestart,
			pod: deleting(workerPod(func(p *corev1.Pod) {
				p.Name = "test-sample-9f2ac71b-worker"
				p.Annotations = map[string]string{leaderworkerset.LeaderPodNameAnnotationKey: "test-sample-0"}
				ownedByLeader(leaderPod(nil))(p)
			})),
			objects:        []client.Object{leaderPod(nil), workerPod(nil)},
			wantDeleted:    true,
			wantLeaderGone: true,
			wantEvent:      true,
		},
		{
			name:        "a leader that is already terminating is not deleted again",
			lws:         recreateOnRestart,
			pod:         deleting(leaderPod(nil)),
			objects:     []client.Object{deleting(leaderPod(nil)), workerPod(nil)},
			wantDeleted: true,
		},
		{
			name:            "a failed pod list is propagated",
			lws:             recreateOnRestart,
			pod:             deleting(workerPod(nil)),
			objects:         []client.Object{leaderPod(nil), workerPod(nil)},
			listErr:         errors.New("list failed"),
			wantErrContains: "list failed",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			builder := fake.NewClientBuilder().WithScheme(scheme).WithObjects(tc.objects...)
			if tc.listErr != nil {
				builder = builder.WithInterceptorFuncs(interceptor.Funcs{
					List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
						return tc.listErr
					},
				})
			}
			c := builder.Build()
			recorder := events.NewFakeRecorder(10)
			reconciler := &PodReconciler{Client: c, Scheme: scheme, Record: recorder}

			leaderDeleted, err := reconciler.handleRestartPolicy(context.Background(), *tc.pod, *tc.lws.DeepCopy())
			switch {
			case tc.wantErrContains == "" && err != nil:
				t.Fatalf("handleRestartPolicy() error = %v, want nil", err)
			case tc.wantErrContains != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErrContains)):
				t.Fatalf("handleRestartPolicy() error = %v, want an error containing %q", err, tc.wantErrContains)
			}
			if leaderDeleted != tc.wantDeleted {
				t.Errorf("handleRestartPolicy() leaderDeleted = %t, want %t", leaderDeleted, tc.wantDeleted)
			}

			err = c.Get(context.Background(), client.ObjectKey{Name: "test-sample-0", Namespace: "default"}, &corev1.Pod{})
			switch {
			case tc.wantLeaderGone && !apierrors.IsNotFound(err):
				t.Errorf("leader pod still exists, err = %v", err)
			case !tc.wantLeaderGone && err != nil && !apierrors.IsNotFound(err):
				t.Errorf("getting leader pod: %v", err)
			}

			gotEvents := podCtrlDrainEvents(recorder)
			if tc.wantEvent {
				if len(gotEvents) != 1 || !strings.Contains(gotEvents[0], "RecreateGroup") {
					t.Errorf("handleRestartPolicy() events = %v, want exactly one RecreateGroup event", gotEvents)
				}
				return
			}
			if len(gotEvents) != 0 {
				t.Errorf("handleRestartPolicy() recorded unexpected events: %v", gotEvents)
			}
		})
	}
}

func TestPodCtrlWorkerPodBelongsToLeader(t *testing.T) {
	scheme := podCtrlTestScheme(t)
	leader := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "test-sample-0", Namespace: "default", UID: "leader-current"}}
	otherLeader := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "test-sample-0", Namespace: "default", UID: "leader-previous"}}

	statefulSet := func(uid types.UID, owner *corev1.Pod) *appsv1.StatefulSet {
		sts := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: "test-sample-0", Namespace: "default", UID: uid}}
		if owner != nil {
			sts.OwnerReferences = []metav1.OwnerReference{*metav1.NewControllerRef(owner, corev1.SchemeGroupVersion.WithKind("Pod"))}
		}
		return sts
	}
	currentSts := statefulSet("sts-current", leader)
	previousLeaderSts := statefulSet("sts-previous-owner", otherLeader)
	orphanSts := statefulSet("sts-orphan", nil)

	workerOwnedBy := func(owner metav1.OwnerReference) corev1.Pod {
		return corev1.Pod{ObjectMeta: metav1.ObjectMeta{
			Name:            "test-sample-0-1",
			Namespace:       "default",
			OwnerReferences: []metav1.OwnerReference{owner},
		}}
	}
	stsRef := func(sts *appsv1.StatefulSet) metav1.OwnerReference {
		return *metav1.NewControllerRef(sts, appsv1.SchemeGroupVersion.WithKind("StatefulSet"))
	}
	podRef := func(pod *corev1.Pod) metav1.OwnerReference {
		return *metav1.NewControllerRef(pod, corev1.SchemeGroupVersion.WithKind("Pod"))
	}

	tests := []struct {
		name    string
		pod     corev1.Pod
		objects []client.Object
		want    bool
	}{
		{
			name: "a worker without a controller belongs to nobody",
			pod:  corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "test-sample-0-1", Namespace: "default"}},
		},
		{
			name: "a worker owned directly by the leader belongs to it",
			pod:  workerOwnedBy(podRef(leader)),
			want: true,
		},
		{
			name: "a worker owned by a recreated leader with the same name does not",
			pod:  workerOwnedBy(podRef(otherLeader)),
		},
		{
			name: "an unrelated owner kind does not",
			pod: workerOwnedBy(metav1.OwnerReference{
				APIVersion: "batch/v1", Kind: "Job", Name: "job", UID: "job-uid", Controller: func() *bool { b := true; return &b }(),
			}),
		},
		{
			name: "a worker of the current worker statefulset belongs to the leader",
			pod:  workerOwnedBy(stsRef(currentSts)),
			// The reconciler looks up the statefulset by name, so it must exist.
			objects: []client.Object{currentSts},
			want:    true,
		},
		{
			name: "a worker of a deleted worker statefulset does not",
			pod:  workerOwnedBy(stsRef(currentSts)),
		},
		{
			name:    "a worker of a recreated statefulset with a different UID does not",
			pod:     workerOwnedBy(stsRef(statefulSet("sts-stale", leader))),
			objects: []client.Object{currentSts},
		},
		{
			name:    "a worker of an orphaned statefulset does not",
			pod:     workerOwnedBy(stsRef(orphanSts)),
			objects: []client.Object{orphanSts},
		},
		{
			name:    "a worker of a statefulset owned by a previous leader does not",
			pod:     workerOwnedBy(stsRef(previousLeaderSts)),
			objects: []client.Object{previousLeaderSts},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(tc.objects...).Build()
			reconciler := &PodReconciler{Client: c, Scheme: scheme, Record: events.NewFakeRecorder(1)}

			got, err := reconciler.workerPodBelongsToLeader(context.Background(), tc.pod, *leader)
			if err != nil {
				t.Fatalf("workerPodBelongsToLeader() error = %v", err)
			}
			if got != tc.want {
				t.Errorf("workerPodBelongsToLeader() = %t, want %t", got, tc.want)
			}
		})
	}
}

func TestPodCtrlSetNodeSelectorForWorkerPodsCopiesLeaderTopology(t *testing.T) {
	const topologyKey = "topology.kubernetes.io/zone"
	// Nodes are cluster scoped, so the fake tracker stores them under the empty
	// namespace; leave the pod namespace empty to match the lookup.
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-a", Labels: map[string]string{topologyKey: "zone-a"}}}
	leader := &corev1.Pod{Spec: corev1.PodSpec{NodeName: node.Name}}
	reconciler := &PodReconciler{Client: fake.NewClientBuilder().WithObjects(node).Build()}

	sts := &appsapplyv1.StatefulSetApplyConfiguration{
		Spec: &appsapplyv1.StatefulSetSpecApplyConfiguration{
			Template: &coreapplyv1.PodTemplateSpecApplyConfiguration{
				Spec: &coreapplyv1.PodSpecApplyConfiguration{},
			},
		},
	}
	if err := reconciler.setNodeSelectorForWorkerPods(context.Background(), leader, sts, topologyKey); err != nil {
		t.Fatalf("setNodeSelectorForWorkerPods() error = %v", err)
	}
	if diff := cmp.Diff(map[string]string{topologyKey: "zone-a"}, sts.Spec.Template.Spec.NodeSelector); diff != "" {
		t.Errorf("unexpected worker node selector (-want,+got):\n%s", diff)
	}
}

func TestPodCtrlControllerOwnerReference(t *testing.T) {
	scheme := podCtrlTestScheme(t)
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "test-lws-0", Namespace: "default", UID: "leader-current"}}

	t.Run("marks the owner as the managing controller", func(t *testing.T) {
		ref, err := controllerOwnerReference(pod, scheme)
		if err != nil {
			t.Fatalf("controllerOwnerReference() error = %v", err)
		}
		want := map[string]any{
			"apiVersion":         "v1",
			"kind":               "Pod",
			"name":               pod.Name,
			"uid":                string(pod.UID),
			"controller":         true,
			"blockOwnerDeletion": true,
		}
		got := map[string]any{
			"apiVersion":         *ref.APIVersion,
			"kind":               *ref.Kind,
			"name":               *ref.Name,
			"uid":                string(*ref.UID),
			"controller":         *ref.Controller,
			"blockOwnerDeletion": *ref.BlockOwnerDeletion,
		}
		if diff := cmp.Diff(want, got); diff != "" {
			t.Errorf("unexpected owner reference (-want,+got):\n%s", diff)
		}
	})

	t.Run("rejects owners that are not runtime objects", func(t *testing.T) {
		// *metav1.ObjectMeta satisfies metav1.Object but not runtime.Object.
		_, err := controllerOwnerReference(&metav1.ObjectMeta{Name: "not-an-object"}, scheme)
		if err == nil || !strings.Contains(err.Error(), "not a runtime.Object") {
			t.Fatalf("controllerOwnerReference() error = %v, want an error about a non runtime.Object owner", err)
		}
	})

	t.Run("propagates unknown kinds", func(t *testing.T) {
		_, err := controllerOwnerReference(pod, runtime.NewScheme())
		if err == nil {
			t.Fatal("controllerOwnerReference() error = nil, want an error for a type missing from the scheme")
		}
	})
}

func TestPodCtrlPodEventHandlerEnqueuesLivePods(t *testing.T) {
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name:      "test-lws-0",
		Namespace: "default",
		UID:       "leader-current",
		Labels:    map[string]string{leaderworkerset.SetNameLabelKey: "test-lws"},
	}}
	want := []podReconcileRequest{{
		NamespacedName: types.NamespacedName{Name: pod.Name, Namespace: pod.Namespace},
		UID:            pod.UID,
	}}

	tests := []struct {
		name string
		emit func(h handler.TypedEventHandler[client.Object, podReconcileRequest], queue workqueue.TypedRateLimitingInterface[podReconcileRequest])
	}{
		{
			name: "update",
			emit: func(h handler.TypedEventHandler[client.Object, podReconcileRequest], queue workqueue.TypedRateLimitingInterface[podReconcileRequest]) {
				h.Update(context.Background(), event.TypedUpdateEvent[client.Object]{ObjectOld: pod.DeepCopy(), ObjectNew: pod}, queue)
			},
		},
		{
			name: "generic",
			emit: func(h handler.TypedEventHandler[client.Object, podReconcileRequest], queue workqueue.TypedRateLimitingInterface[podReconcileRequest]) {
				h.Generic(context.Background(), event.TypedGenericEvent[client.Object]{Object: pod}, queue)
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			queue := podCtrlNewQueue(t)
			tc.emit(podEventHandler(), queue)
			if diff := cmp.Diff(want, podCtrlDrainQueue(t, queue)); diff != "" {
				t.Errorf("unexpected requests (-want,+got):\n%s", diff)
			}
		})
	}
}

func TestPodCtrlPodEventHandlerIgnoresNonPods(t *testing.T) {
	var typedNilPod *corev1.Pod
	tests := []struct {
		name   string
		object client.Object
	}{
		{name: "statefulset", object: &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: "test-lws-0", Namespace: "default"}}},
		{name: "typed nil pod", object: typedNilPod},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			queue := podCtrlNewQueue(t)
			handler := podEventHandler()
			handler.Create(context.Background(), event.TypedCreateEvent[client.Object]{Object: tc.object}, queue)
			handler.Update(context.Background(), event.TypedUpdateEvent[client.Object]{ObjectNew: tc.object}, queue)
			handler.Delete(context.Background(), event.TypedDeleteEvent[client.Object]{Object: tc.object}, queue)
			handler.Generic(context.Background(), event.TypedGenericEvent[client.Object]{Object: tc.object}, queue)

			if got := podCtrlDrainQueue(t, queue); len(got) != 0 {
				t.Errorf("podEventHandler() enqueued %v, want nothing", got)
			}
		})
	}
}

func TestPodCtrlStatefulSetEventHandlerIgnoresForeignOwners(t *testing.T) {
	controller := true
	tests := []struct {
		name   string
		object client.Object
	}{
		{
			name:   "not a statefulset",
			object: &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "test-lws-0", Namespace: "default"}},
		},
		{
			name:   "no controller owner",
			object: &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: "test-lws-0", Namespace: "default"}},
		},
		{
			name: "owned by another kind",
			object: &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{
				Name:      "test-lws-0",
				Namespace: "default",
				OwnerReferences: []metav1.OwnerReference{{
					APIVersion: "apps/v1", Kind: "ReplicaSet", Name: "rs", UID: "rs-uid", Controller: &controller,
				}},
			}},
		},
		{
			name: "owned by a pod from another group version",
			object: &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{
				Name:      "test-lws-0",
				Namespace: "default",
				OwnerReferences: []metav1.OwnerReference{{
					APIVersion: "example.com/v1", Kind: "Pod", Name: "test-lws-0", UID: "pod-uid", Controller: &controller,
				}},
			}},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			queue := podCtrlNewQueue(t)
			statefulSetEventHandler().Create(context.Background(), event.TypedCreateEvent[client.Object]{Object: tc.object}, queue)
			if got := podCtrlDrainQueue(t, queue); len(got) != 0 {
				t.Errorf("statefulSetEventHandler() enqueued %v, want nothing", got)
			}
		})
	}
}

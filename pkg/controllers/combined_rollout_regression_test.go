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
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	autoscalingv1 "k8s.io/api/autoscaling/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/event"

	leaderworkerset "sigs.k8s.io/lws/api/leaderworkerset/v1"
	"sigs.k8s.io/lws/test/wrappers"
)

// Real apiserver writes, with explicitly simulated native status and Pods.
// There is no native StatefulSet controller in envtest.
type combinedAPIFixture struct {
	t      *testing.T
	c      client.Client
	scheme *runtime.Scheme
	key    client.ObjectKey
}

func newCombinedAPIFixture(t *testing.T, c client.Client, scheme *runtime.Scheme, namespace, name string, b int) *combinedAPIFixture {
	t.Helper()
	lws := wrappers.BuildLeaderWorkerSet(namespace).Replica(b).Size(1).Obj()
	lws.Name = name
	lws.Spec.RolloutStrategy.RollingUpdateConfiguration.MaxUnavailable = intstr.FromInt(1)
	f := &combinedAPIFixture{t: t, c: c, scheme: scheme, key: client.ObjectKeyFromObject(lws)}
	if err := c.Create(context.Background(), lws); err != nil {
		t.Fatal(err)
	}
	f.reconcile()
	f.ack(true)
	for i := 0; i < b; i++ {
		f.leader(i, true)
	}
	return f
}

func (f *combinedAPIFixture) reconciler() *LeaderWorkerSetReconciler {
	r := NewLeaderWorkerSetReconciler(f.c, f.scheme, fakeEventRecorder{})
	r.apiReader = f.c
	return r
}

func (f *combinedAPIFixture) reconcile() {
	f.t.Helper()
	// Restart every pass, so no test depends on in-memory phase state.
	if _, err := f.reconciler().Reconcile(context.Background(), ctrl.Request{NamespacedName: f.key}); err != nil {
		f.t.Fatal(err)
	}
}

func (f *combinedAPIFixture) sts() *appsv1.StatefulSet {
	f.t.Helper()
	s := &appsv1.StatefulSet{}
	if err := f.c.Get(context.Background(), f.key, s); err != nil {
		f.t.Fatal(err)
	}
	return s
}

func (f *combinedAPIFixture) lws() *leaderworkerset.LeaderWorkerSet {
	f.t.Helper()
	lws := &leaderworkerset.LeaderWorkerSet{}
	if err := f.c.Get(context.Background(), f.key, lws); err != nil {
		f.t.Fatal(err)
	}
	return lws
}

func (f *combinedAPIFixture) change(mutate func(*leaderworkerset.LeaderWorkerSet)) {
	f.t.Helper()
	lws := f.lws()
	mutate(lws)
	if err := f.c.Update(context.Background(), lws); err != nil {
		f.t.Fatal(err)
	}
}

func (f *combinedAPIFixture) state() combinedModelState {
	f.t.Helper()
	s, err := combinedModelDecode(f.sts().Annotations[combinedRolloutAnnotation])
	if err != nil {
		f.t.Fatal(err)
	}
	return s
}

func (f *combinedAPIFixture) ack(promote bool) {
	f.t.Helper()
	s := f.sts()
	b, err := json.Marshal(s.Spec.Template)
	if err != nil {
		f.t.Fatal(err)
	}
	s.Status.UpdateRevision = fmt.Sprintf("native-%x", sha256.Sum256(b))[:40]
	if promote {
		s.Status.CurrentRevision = s.Status.UpdateRevision
	}
	s.Status.ObservedGeneration, s.Status.Replicas = s.Generation, *s.Spec.Replicas
	if err := f.c.Status().Update(context.Background(), s); err != nil {
		f.t.Fatal(err)
	}
}

func (f *combinedAPIFixture) leader(i int, ready bool) *corev1.Pod {
	f.t.Helper()
	s := f.sts()
	p := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("%s-%d", f.key.Name, i), Namespace: f.key.Namespace,
		Labels:          map[string]string{leaderworkerset.SetNameLabelKey: f.key.Name, leaderworkerset.GroupIndexLabelKey: strconv.Itoa(i), leaderworkerset.WorkerIndexLabelKey: "0", leaderworkerset.RevisionKey: s.Spec.Template.Labels[leaderworkerset.RevisionKey], appsv1.ControllerRevisionHashLabelKey: s.Status.UpdateRevision},
		Annotations:     map[string]string{leaderworkerset.SizeAnnotationKey: "1"},
		OwnerReferences: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "StatefulSet", Name: s.Name, UID: s.UID, Controller: ptr.To(true)}}},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "test", Image: "test.invalid/image"}}}}
	if err := f.c.Create(context.Background(), p); err != nil {
		f.t.Fatal(err)
	}
	if ready {
		p.Status.Phase = corev1.PodRunning
		p.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
		if err := f.c.Status().Update(context.Background(), p); err != nil {
			f.t.Fatal(err)
		}
	}
	return p
}

func (f *combinedAPIFixture) activate() {
	f.t.Helper()
	f.reconcile() // freeze
	f.ack(false)
	f.reconcile() // publish
	f.ack(false)
	f.reconcile() // pin native target
	if f.state().NativeRevision != f.sts().Status.UpdateRevision {
		f.t.Fatal("native target not persisted")
	}
}

func (f *combinedAPIFixture) grow(d int32) {
	f.change(func(lws *leaderworkerset.LeaderWorkerSet) {
		lws.Spec.Replicas = ptr.To(d)
		lws.Spec.LeaderWorkerTemplate.LeaderTemplate.Spec.Containers[0].Image = "test.invalid/new"
	})
	f.activate()
	f.reconcile() // persist reservation, without deleting
	f.ack(false)
}

func testCombinedRegressions(t *testing.T, c client.Client, scheme *runtime.Scheme, namespace string) {
	for _, rollback := range []bool{true, false} {
		name := "partition-raise"
		if rollback {
			name = "rollback"
		}
		t.Run(name+" retires only after acknowledged fence across restart", func(t *testing.T) {
			f := newCombinedAPIFixture(t, c, scheme, namespace, name, 2)
			oldImage := f.lws().Spec.LeaderWorkerTemplate.LeaderTemplate.Spec.Containers[0].Image
			f.grow(3)
			reservation, ok := f.state().Reservations[1]
			if !ok {
				t.Fatal("expected ordinal 1 authorization")
			}
			f.change(func(lws *leaderworkerset.LeaderWorkerSet) {
				if rollback {
					lws.Spec.LeaderWorkerTemplate.LeaderTemplate.Spec.Containers[0].Image = oldImage
				} else {
					lws.Spec.RolloutStrategy.RollingUpdateConfiguration.Partition = ptr.To[int32](2)
				}
			})
			f.reconcile()
			f.reconcile() // no native acknowledgement
			if f.state().Phase != combinedFreeze || f.state().Reservations[1].UID != reservation.UID {
				t.Fatal("retired surviving obligation before fence acknowledgement")
			}
			f.ack(false)
			f.reconcile()
			f.ack(false)
			f.reconcile()
			if len(f.state().Reservations) != 0 {
				t.Fatal("impossible same-UID/protected obligation survived fence")
			}
			p := &corev1.Pod{}
			if err := c.Get(context.Background(), client.ObjectKey{Namespace: namespace, Name: name + "-1"}, p); err != nil {
				t.Fatal(err)
			}
			if p.UID != reservation.UID || p.DeletionTimestamp != nil {
				t.Fatal("fenced obligation deleted original leader")
			}
			f.leader(2, true)
			if rollback {
				f.ack(true)
			}
			f.reconcile()
			if f.sts().Annotations[combinedRolloutAnnotation] != "" {
				t.Fatal("ready rollback/protected groups did not complete")
			}
			if !rollback && f.sts().Status.CurrentRevision == f.sts().Status.UpdateRevision {
				t.Fatal("test must exercise protected old native revision")
			}
		})
	}

	t.Run("completion waits for native promotion and retains history", func(t *testing.T) {
		f := newCombinedAPIFixture(t, c, scheme, namespace, "promotion", 1)
		f.grow(2)
		old := &corev1.Pod{}
		if err := c.Get(context.Background(), client.ObjectKey{Namespace: namespace, Name: "promotion-0"}, old); err != nil {
			t.Fatal(err)
		}
		if err := c.Delete(context.Background(), old, client.GracePeriodSeconds(0)); err != nil {
			t.Fatal(err)
		}
		f.leader(0, true)
		f.leader(1, true)
		for i := 0; i < 3; i++ {
			f.ack(false)
			f.reconcile()
		}
		if f.state().Phase != combinedActive {
			t.Fatal("cleared state before native promotion")
		}
		var revisions appsv1.ControllerRevisionList
		if err := c.List(context.Background(), &revisions, client.InNamespace(namespace), client.MatchingLabels{leaderworkerset.SetNameLabelKey: f.key.Name}); err != nil {
			t.Fatal(err)
		}
		if len(revisions.Items) != 2 {
			t.Fatalf("lost protected history: %d", len(revisions.Items))
		}
		f.ack(true)
		f.reconcile()
		if f.sts().Annotations[combinedRolloutAnnotation] != "" {
			t.Fatal("did not complete after native promotion")
		}
		f.change(func(lws *leaderworkerset.LeaderWorkerSet) {
			lws.Spec.Replicas = ptr.To[int32](3)
			lws.Spec.LeaderWorkerTemplate.LeaderTemplate.Spec.Containers[0].Image = "test.invalid/newer"
		})
		f.reconcile()
		if f.state().Phase != combinedFreeze || f.state().Baseline != 2 || f.sts().Status.CurrentRevision != f.sts().Status.UpdateRevision {
			t.Fatal("next update did not start from promoted baseline")
		}
	})

	t.Run("same LWS revision annotation update freezes before publication", func(t *testing.T) {
		f := newCombinedAPIFixture(t, c, scheme, namespace, "annotation", 2)
		f.change(func(lws *leaderworkerset.LeaderWorkerSet) {
			lws.Spec.Replicas = ptr.To[int32](3)
			lws.Spec.RolloutStrategy.RollingUpdateConfiguration.MaxUnavailable = intstr.FromInt(0)
			lws.Spec.RolloutStrategy.RollingUpdateConfiguration.MaxSurge = intstr.FromInt(1)
			lws.Annotations = map[string]string{leaderworkerset.ExclusiveKeyAnnotationKey: "topology.example/a"}
		})
		lwsRevision := f.sts().Labels[leaderworkerset.RevisionKey]
		oldNative := f.sts().Status.UpdateRevision
		f.activate()
		if f.state().Revision != lwsRevision || f.state().NativeRevision == oldNative {
			t.Fatal("test did not produce identical LWS/different native identities")
		}
		f.reconcile()
		f.ack(false)
		p := f.leader(2, true)
		p.Annotations[leaderworkerset.SizeAnnotationKey] = "2" // workers absent
		if err := c.Update(context.Background(), p); err != nil {
			t.Fatal(err)
		}
		f.reconcile()
		if *f.sts().Spec.UpdateStrategy.RollingUpdate.Partition != 2 || len(f.state().Reservations) != 0 {
			t.Fatal("Ready leader with missing workers financed native replacement")
		}
		f.change(func(lws *leaderworkerset.LeaderWorkerSet) {
			lws.Annotations[leaderworkerset.ExclusiveKeyAnnotationKey] = "topology.example/b"
		})
		f.reconcile()
		if f.state().Phase != combinedFreeze || f.sts().Spec.Template.Annotations[leaderworkerset.ExclusiveKeyAnnotationKey] != "topology.example/a" {
			t.Fatal("metadata-only supersession published without freeze")
		}
		f.ack(false)
		f.reconcile()
		f.ack(false)
		f.reconcile()
		if f.state().Revision != lwsRevision || f.sts().Spec.Template.Annotations[leaderworkerset.ExclusiveKeyAnnotationKey] != "topology.example/b" {
			t.Fatal("annotation-only target was not safely republished")
		}
	})

	t.Run("operational patches preserve foreign SSA ownership and reject stale RV", func(t *testing.T) {
		f := newCombinedAPIFixture(t, c, scheme, namespace, "ownership", 1)
		foreign := func(value string) {
			t.Helper()
			obj := &unstructured.Unstructured{Object: map[string]interface{}{"apiVersion": "apps/v1", "kind": "StatefulSet", "metadata": map[string]interface{}{"name": f.key.Name, "namespace": namespace, "annotations": map[string]interface{}{"independent.example/note": value}}, "spec": map[string]interface{}{"template": map[string]interface{}{"metadata": map[string]interface{}{"annotations": map[string]interface{}{"independent.example/template": "unchanged"}}}}}}
			if err := c.Patch(context.Background(), obj, client.Apply, client.FieldOwner("independent")); err != nil {
				t.Fatal(err)
			} //nolint:staticcheck // real SSA ownership regression
		}
		foreign("before")
		f.ack(true)
		f.grow(2)
		foreign("after-publication") // no force: copied ownership would conflict
		if f.sts().Spec.Template.Annotations["independent.example/template"] != "unchanged" {
			t.Fatal("publication pruned foreign template metadata")
		}
		r := f.reconciler()
		old := f.sts()
		next := old.DeepCopy()
		next.Annotations["independent.example/note"] = "stale-write"
		foreign("after-reservation")
		if err := r.applyCombinedStatefulSet(context.Background(), f.lws(), old, next, old.Annotations[combinedRolloutAnnotation]); !apierrors.IsConflict(err) {
			t.Fatalf("stale operational patch accepted: %v", err)
		}
		// Exercise cleanup through the same operational writer without relying
		// on a native controller that is not present in this API test.
		old = f.sts()
		if err := r.applyCombinedStatefulSet(context.Background(), f.lws(), old, old.DeepCopy(), ""); err != nil {
			t.Fatal(err)
		}
		foreign("after-cleanup")
		if f.sts().Annotations[combinedRolloutAnnotation] != "" {
			t.Fatal("cleanup did not remove protocol annotation")
		}
	})
}

func TestCombinedMixedSizeRestartUsesLeaderStamp(t *testing.T) {
	for _, workerRestart := range []bool{false, true} {
		t.Run(fmt.Sprintf("worker=%t", workerRestart), func(t *testing.T) {
			lws := wrappers.BuildLeaderWorkerSet("default").Replica(1).Size(1).Obj()
			lws.Spec.LeaderWorkerTemplate.RestartPolicy = leaderworkerset.RecreateGroupAfterStart
			leader := combinedTestGroup(0, "old", true).leader
			leader.Annotations[leaderworkerset.SizeAnnotationKey] = "2"
			leader.Labels[leaderworkerset.WorkerIndexLabelKey] = "0"
			leader.Labels[leaderworkerset.SetNameLabelKey] = lws.Name
			leader.Labels[leaderworkerset.GroupIndexLabelKey] = "0"
			worker := leader.DeepCopy()
			worker.Name, worker.UID = leader.Name+"-1", "worker"
			worker.Labels[leaderworkerset.WorkerIndexLabelKey] = "1"
			worker.OwnerReferences = []metav1.OwnerReference{{Kind: "Pod", Name: leader.Name, UID: leader.UID, Controller: ptr.To(true)}}
			restarting := leader
			if workerRestart {
				restarting = worker
			}
			restarting.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "test", RestartCount: 1}}
			c := fake.NewClientBuilder().WithScheme(lwsStatusScheme(t)).WithObjects(lws, leader, worker).Build()
			r := NewPodReconciler(c, c.Scheme(), fakeEventRecorder{}, nil)
			deleted, _, err := r.handleRestartPolicy(context.Background(), *restarting, *lws)
			if err != nil || !deleted {
				t.Fatalf("old size-two group suppressed under live size-one: deleted=%t err=%v", deleted, err)
			}
		})
	}
}

// Called from the existing real-apiserver suite. Native controller status and
// Pods are simulated by combinedAPIFixture, not by a running native controller.
func testCombinedUpstreamPrerequisites(t *testing.T, c client.Client, scheme *runtime.Scheme, namespace string) {
	ctx := context.Background()
	t.Run("scheduling gates publication growth and reserved deletion across status writes", func(t *testing.T) {
		f := newCombinedAPIFixture(t, c, scheme, namespace, "upstream-scheduling", 2)
		unavailable := errors.New("scheduling prerequisites unavailable")
		provider := &lwsSchedFakeProvider{reconcileErr: unavailable}
		reconcile := func() error {
			r := f.reconciler()
			r.SchedulerProvider = provider
			_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: f.key})
			return err
		}
		leader := func() *corev1.Pod {
			t.Helper()
			p := &corev1.Pod{}
			if err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: f.key.Name + "-1"}, p); err != nil {
				t.Fatal(err)
			}
			return p
		}
		original := leader()
		unchanged := func(rv string) {
			t.Helper()
			if f.sts().ResourceVersion != rv {
				t.Fatal("blocked pass changed StatefulSet template, replicas or reservations")
			}
			if p := leader(); p.UID != original.UID || p.DeletionTimestamp != nil {
				t.Fatal("blocked pass deleted the reserved leader")
			}
		}
		f.change(func(lws *leaderworkerset.LeaderWorkerSet) {
			lws.Spec.Scheduling = &leaderworkerset.LeaderWorkerSetScheduling{}
			lws.Spec.Replicas = ptr.To[int32](3)
			lws.Spec.LeaderWorkerTemplate.LeaderTemplate.Spec.Containers[0].Image = "test.invalid/scheduled"
		})
		if err := reconcile(); err != nil {
			t.Fatal(err)
		}
		if provider.calls != 0 || f.state().Phase != combinedFreeze || *f.sts().Spec.Replicas != 2 {
			t.Fatal("freeze must retain old template/replicas without scheduling live spec under old revision")
		}
		f.ack(false)
		// Each stage starts on an acknowledged snapshot. Failed prerequisites and
		// successful condition writes must both leave its authorization untouched.
		for _, stage := range []string{"publication", "growth", "deletion"} {
			before := f.sts()
			provider.reconcileErr = unavailable
			for range 2 {
				if err := reconcile(); !errors.Is(err, unavailable) {
					t.Fatalf("%s: want provider error, got %v", stage, err)
				}
				unchanged(before.ResourceVersion)
			}
			wantReplicas := int32(3)
			if stage == "publication" {
				wantReplicas = 2
			}
			if provider.gotRevision != f.state().Revision || provider.gotReplicas != wantReplicas {
				t.Fatalf("%s: prerequisites for wrong target: revision=%s replicas=%d", stage, provider.gotRevision, provider.gotReplicas)
			}
			provider.reconcileErr = nil
			oldLWS := f.lws()
			if err := reconcile(); err != nil {
				t.Fatalf("%s: condition RV write must retry without a reconcile error, got %v", stage, err)
			}
			unchanged(before.ResourceVersion)
			if f.lws().ResourceVersion == oldLWS.ResourceVersion {
				t.Fatal("success condition did not change the persisted RV")
			}
			if err := reconcile(); err != nil {
				t.Fatalf("%s: fresh pass did not progress: %v", stage, err)
			}
			switch stage {
			case "publication":
				if f.state().Phase != combinedPublish || f.sts().Spec.Template.Spec.Containers[0].Image != "test.invalid/scheduled" {
					t.Fatal("successful provider did not publish intended template")
				}
				f.ack(false)
				if err := reconcile(); err != nil {
					t.Fatal(err)
				}
			case "growth":
				if *f.sts().Spec.Replicas != 3 || f.state().Reservations[1].UID != original.UID {
					t.Fatal("successful provider did not grow with the original reservation")
				}
				f.ack(false)
				f.leader(2, false) // native addition, no readiness credit
			case "deletion":
				if p := leader(); p.UID != original.UID || p.DeletionTimestamp == nil {
					t.Fatal("fresh pass did not resume exactly the authorized deletion")
				}
			}
			t.Logf("%s: provider failure blocked; condition write discarded plan; fresh pass progressed", stage)
		}
	})

	t.Run("restart count pruning cannot refresh away a deletion fence", func(t *testing.T) {
		f := newCombinedAPIFixture(t, c, scheme, namespace, "upstream-pruning", 2)
		f.grow(3)
		f.leader(2, false)
		state := f.state()
		f.change(func(lws *leaderworkerset.LeaderWorkerSet) {
			if lws.Annotations == nil {
				lws.Annotations = map[string]string{}
			}
			lws.Annotations[leaderworkerset.GroupRestartCountsAnnotationKey] = fmt.Sprintf(`{"%s/0":1,"%s/9":3}`, state.Revision, state.Revision)
		})
		before := f.sts()
		_, err := f.reconciler().Reconcile(ctx, ctrl.Request{NamespacedName: f.key})
		if err != nil {
			t.Fatalf("pruning RV write must retry without a reconcile error: %v", err)
		}
		if f.sts().ResourceVersion != before.ResourceVersion {
			t.Fatal("pruning pass changed persisted reservations")
		}
		p := &corev1.Pod{}
		key := client.ObjectKey{Namespace: namespace, Name: f.key.Name + "-1"}
		if err := c.Get(ctx, key, p); err != nil {
			t.Fatal(err)
		}
		if p.UID != state.Reservations[1].UID || p.DeletionTimestamp != nil {
			t.Fatal("pruning bypassed the deletion fence")
		}
		counts, err := parseGroupRestartCounts(f.lws().Annotations[leaderworkerset.GroupRestartCountsAnnotationKey])
		if err != nil || len(counts) != 1 || counts[state.Revision+"/0"] != 1 {
			t.Fatalf("pruning lost the live count or retained the scaled-down count: %v %v", counts, err)
		}
		f.reconcile()
		if err := c.Get(ctx, key, p); err != nil || p.DeletionTimestamp == nil {
			t.Fatalf("idempotent pruning wedged fresh authorized deletion: %v", err)
		}
	})

	t.Run("invalid state runs neither scheduling nor restart count pruning", func(t *testing.T) {
		f := newCombinedAPIFixture(t, c, scheme, namespace, "upstream-invalid", 2)
		f.grow(3)
		f.change(func(lws *leaderworkerset.LeaderWorkerSet) {
			lws.Spec.Scheduling = &leaderworkerset.LeaderWorkerSetScheduling{}
			lws.Annotations = map[string]string{leaderworkerset.GroupRestartCountsAnnotationKey: `{"old/9":2}`}
		})
		sts := f.sts()
		sts.Annotations[combinedRolloutAnnotation] = `{"v":`
		if err := c.Update(ctx, sts); err != nil {
			t.Fatal(err)
		}
		before := f.lws()
		provider := &lwsSchedFakeProvider{}
		for range 3 {
			r := f.reconciler()
			r.SchedulerProvider = provider
			if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: f.key}); err == nil {
				t.Fatal("invalid state accepted")
			}
			if provider.calls != 0 || f.lws().ResourceVersion != before.ResourceVersion || f.sts().ResourceVersion != sts.ResourceVersion {
				t.Fatal("invalid state changed scheduling status, restart counts or StatefulSet")
			}
		}
	})
}

type combinedWarningEvent struct {
	regarding, related              runtime.Object
	eventType, reason, action, note string
}

type combinedWarningRecorder struct {
	events []combinedWarningEvent
}

func (r *combinedWarningRecorder) Eventf(regarding, related runtime.Object, eventType, reason, action, note string, args ...interface{}) {
	event := combinedWarningEvent{eventType: eventType, reason: reason, action: action, note: fmt.Sprintf(note, args...)}
	if regarding != nil {
		event.regarding = regarding.DeepCopyObject()
	}
	if related != nil {
		event.related = related.DeepCopyObject()
	}
	r.events = append(r.events, event)
}

// Compare complete API objects, including raw annotations, RVs, specs, status,
// UIDs and deletion timestamps. List RV is unrelated to per-object mutation.
func combinedWarningUnchanged(t *testing.T, f *combinedAPIFixture) func() {
	t.Helper()
	sts, lws := f.sts(), f.lws()
	pods := func() []corev1.Pod {
		t.Helper()
		var list corev1.PodList
		if err := f.c.List(context.Background(), &list, client.InNamespace(f.key.Namespace), client.MatchingLabels{leaderworkerset.SetNameLabelKey: f.key.Name}); err != nil {
			t.Fatal(err)
		}
		return list.Items
	}
	beforePods := pods()
	return func() {
		t.Helper()
		if !reflect.DeepEqual(sts, f.sts()) || !reflect.DeepEqual(lws, f.lws()) || !reflect.DeepEqual(beforePods, pods()) {
			t.Fatal("blocked reconciliation mutated StatefulSet, LWS or Pods (including RV/spec/status/raw state)")
		}
	}
}

func testCombinedInvalidStateWarnings(t *testing.T, c client.Client, scheme *runtime.Scheme, namespace string) {
	ctx := context.Background()
	for _, name := range []string{"malformed-json", "future-generation", "unknown-version", "missing-phase", "zero-generation", "oversized"} {
		t.Run(name, func(t *testing.T) {
			f := newCombinedAPIFixture(t, c, scheme, namespace, "warning-"+name, 2)
			f.grow(3)
			// Simulate native creation without readiness credit. An absent slot
			// would require a new reservation before the recovery control deletes.
			f.leader(2, false)
			original := f.sts().Annotations[combinedRolloutAnnotation]
			state := f.state()
			reservation, ok := state.Reservations[1]
			if !ok || len(state.Reservations) != 1 || state.Phase != combinedActive || state.Baseline != 2 || state.Desired != 3 {
				t.Fatalf("expected active B=2/D=3 with one old ordinal-1 reservation: %+v", state)
			}
			getPod := func(i int) *corev1.Pod {
				t.Helper()
				p := &corev1.Pod{}
				if err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: fmt.Sprintf("%s-%d", f.key.Name, i)}, p); err != nil {
					t.Fatal(err)
				}
				return p
			}
			old0, old1 := getPod(0), getPod(1)
			if old1.UID != reservation.UID || old0.DeletionTimestamp != nil || old1.DeletionTimestamp != nil {
				t.Fatal("fixture must stop before either old leader is deleted")
			}
			diagnostic, cause := "cannot decode persisted state", "invalid combined rollout state"
			switch name {
			case "future-generation":
				state.Generation = f.lws().Generation + 100
				diagnostic = fmt.Sprintf("persisted generation %d is newer than current LWS generation %d", state.Generation, f.lws().Generation)
				cause = diagnostic
			case "unknown-version":
				state.Version++
			case "missing-phase":
				state.Phase = ""
				diagnostic, cause = "persisted state is missing phase", "persisted state is missing phase"
			case "zero-generation":
				state.Generation = 0
			case "malformed-json":
				cause = "unexpected end of JSON input"
			case "oversized":
				cause = "combined rollout state too large"
			}
			corrupt := combinedModelEncode(state)
			if name == "malformed-json" {
				corrupt = `{"v":`
			} else if name == "oversized" {
				corrupt = strings.Repeat("x", combinedStateLimit+1)
			}
			sts := f.sts()
			sts.Annotations[combinedRolloutAnnotation] = corrupt
			if err := c.Update(ctx, sts); err != nil {
				t.Fatalf("real API rejected corrupt annotation: %v", err)
			}
			if f.sts().Annotations[combinedRolloutAnnotation] != corrupt {
				t.Fatal("API did not persist exact corrupt annotation")
			}
			unchanged := combinedWarningUnchanged(t, f)
			checkError := func(err error) {
				t.Helper()
				if err == nil || errors.Unwrap(err) == nil || !strings.Contains(errors.Unwrap(err).Error(), cause) || strings.Contains(err.Error(), "<nil>") {
					t.Fatalf("expected wrapped cause %q, got %v", cause, err)
				}
				if name == "malformed-json" {
					var syntax *json.SyntaxError
					if !errors.As(err, &syntax) {
						t.Fatalf("lost JSON syntax error cause: %v", err)
					}
				}
			}
			for attempt := 1; attempt <= 3; attempt++ {
				recorder := &combinedWarningRecorder{}
				r := NewLeaderWorkerSetReconciler(c, scheme, recorder)
				r.apiReader = c
				_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: f.key})
				checkError(err)
				unchanged()
				if len(recorder.events) != 1 {
					t.Fatalf("attempt %d: expected exactly one warning, got %+v", attempt, recorder.events)
				}
				event := recorder.events[0]
				want := "Invalid combined rollout state; no mutation or deletion: " + diagnostic
				if event.eventType != corev1.EventTypeWarning || event.reason != "CombinedRolloutInvalidState" || event.action != Update || event.note != want || len(event.note) > 256 || strings.Contains(event.note, "<nil>") || strings.Contains(event.note, corrupt) {
					t.Fatalf("attempt %d: wrong or unbounded warning: %+v", attempt, event)
				}
				regarding, lwsOK := event.regarding.(*leaderworkerset.LeaderWorkerSet)
				related, stsOK := event.related.(*appsv1.StatefulSet)
				if !lwsOK || !stsOK || regarding.UID != f.lws().UID || related.UID != sts.UID || client.ObjectKeyFromObject(regarding) != f.key || client.ObjectKeyFromObject(related) != f.key {
					t.Fatalf("wrong regarding LWS / related leader STS: %+v", event)
				}
				t.Logf("fresh controller %d: warning=%s action=%s note=%q; unchanged API objects; error=%v", attempt, event.reason, event.action, event.note, err)
			}
			// The invalid-state branch must also fail closed with no recorder.
			r := NewLeaderWorkerSetReconciler(c, scheme, nil)
			r.apiReader = c
			_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: f.key})
			checkError(err)
			unchanged()

			// Causal control, not a generic operator repair: restore EXACT known
			// valid bytes, never clear state or infer a new baseline/reservation.
			sts = f.sts()
			sts.Annotations[combinedRolloutAnnotation] = original
			if err := c.Update(ctx, sts); err != nil {
				t.Fatal(err)
			}
			recorder := &combinedWarningRecorder{}
			r = NewLeaderWorkerSetReconciler(c, scheme, recorder)
			r.apiReader = c
			if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: f.key}); err != nil {
				t.Fatal(err)
			}
			for _, event := range recorder.events {
				if event.eventType == corev1.EventTypeWarning || event.reason == "CombinedRolloutInvalidState" {
					t.Fatalf("valid restored state emitted warning: %+v", event)
				}
			}
			if p := getPod(1); p.UID != reservation.UID || p.DeletionTimestamp == nil {
				t.Fatal("exact-state restore did not resume authorized old ordinal-1 deletion")
			}
			if !reflect.DeepEqual(old0, getPod(0)) || f.sts().Annotations[combinedRolloutAnnotation] != original {
				t.Fatal("restore control mutated protected old ordinal 0 or reset baseline/reservation")
			}
			t.Log("nil recorder safe; exact-state restore resumed authorized deletion without Warning or state reset")
		})
	}
}

type combinedWarningErrorReader struct {
	client.Reader
	err error
}

func (r combinedWarningErrorReader) Get(context.Context, client.ObjectKey, client.Object, ...client.GetOption) error {
	return r.err
}

func testCombinedInvalidStateWarningFences(t *testing.T, c client.Client, scheme *runtime.Scheme, namespace string) {
	ctx := context.Background()
	for _, name := range []string{"sts-rv", "lws-rv", "lws-generation", "permission", "transient"} {
		t.Run(name, func(t *testing.T) {
			f := newCombinedAPIFixture(t, c, scheme, namespace, "warning-fence-"+name, 2)
			f.grow(3)
			f.leader(2, false)
			sts := f.sts()
			sts.Annotations[combinedRolloutAnnotation] = `{"v":`
			if err := c.Update(ctx, sts); err != nil {
				t.Fatal(err)
			}
			// Hold actual old API observations, not a client wrapper that might
			// mutate the caller's object and accidentally bypass the fence.
			oldLWS, oldSTS := f.lws(), f.sts()
			recorder := &combinedWarningRecorder{}
			r := NewLeaderWorkerSetReconciler(c, scheme, recorder)
			r.apiReader = c
			var readErr error
			switch name {
			case "sts-rv":
				sts = f.sts()
				sts.Annotations["test.example/fence"] = "changed"
				if err := c.Update(ctx, sts); err != nil {
					t.Fatal(err)
				}
				if f.sts().ResourceVersion == oldSTS.ResourceVersion {
					t.Fatal("fixture did not advance StatefulSet RV")
				}
			case "lws-rv", "lws-generation":
				f.change(func(lws *leaderworkerset.LeaderWorkerSet) {
					if name == "lws-generation" {
						lws.Spec.Replicas = ptr.To[int32](4)
					} else {
						if lws.Labels == nil {
							lws.Labels = make(map[string]string)
						}
						lws.Labels["test.example/fence"] = "changed"
					}
				})
				if f.lws().ResourceVersion == oldLWS.ResourceVersion || (name == "lws-generation") != (f.lws().Generation > oldLWS.Generation) {
					t.Fatal("fixture did not advance intended LWS fence")
				}
			case "permission":
				readErr = apierrors.NewForbidden(leaderworkerset.Resource("leaderworkersets"), f.key.Name, errors.New("test denied"))
			case "transient":
				readErr = apierrors.NewTimeoutError("test transient read failure", 1)
			}
			if readErr != nil {
				r.apiReader = combinedWarningErrorReader{Reader: c, err: readErr}
			}
			unchanged := combinedWarningUnchanged(t, f)
			handled, _, err := r.reconcileCombinedRollout(ctx, oldLWS, oldSTS, oldSTS.Labels[leaderworkerset.RevisionKey], false)
			if !handled || (readErr == nil && !apierrors.IsConflict(err)) || (readErr != nil && !errors.Is(err, readErr)) {
				t.Fatalf("expected observation fence failure, got handled=%v error=%v", handled, err)
			}
			if len(recorder.events) != 0 {
				t.Fatalf("observation fence failure misdiagnosed as invalid state: %+v", recorder.events)
			}
			unchanged()
			t.Logf("fence error=%v; events=0; API objects unchanged", err)
		})
	}
}

func TestCombinedOrdinalPodMapping(t *testing.T) {
	for _, identity := range []leaderworkerset.GroupIdentityType{"", leaderworkerset.GroupIdentityOrdinal, leaderworkerset.GroupIdentityHash} {
		for _, worker := range []string{"0", "1"} {
			t.Run(string(identity)+"/worker-"+worker, func(t *testing.T) {
				lws := wrappers.BuildLeaderWorkerSet("default").Obj()
				lws.Spec.GroupIdentity = identity
				scheme := lwsStatusScheme(t)
				c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(lws).Build()
				r := NewLeaderWorkerSetReconciler(c, scheme, fakeEventRecorder{})
				p := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: lws.Namespace, Labels: map[string]string{
					leaderworkerset.SetNameLabelKey: lws.Name, leaderworkerset.WorkerIndexLabelKey: worker,
				}}}
				got := r.enqueueOrdinalPodRequests(context.Background(), p)
				if identity == leaderworkerset.GroupIdentityHash {
					if len(got) != 0 {
						t.Fatal("ordinary Hash Pod updates must not broaden upstream watch")
					}
				} else if len(got) != 1 || got[0].NamespacedName != client.ObjectKeyFromObject(lws) {
					t.Fatalf("ordinal whole-group health event not mapped: %v", got)
				}
				delete(p.Labels, leaderworkerset.SetNameLabelKey)
				if len(r.enqueueOrdinalPodRequests(context.Background(), p)) != 0 {
					t.Fatal("unlabeled pod mapped")
				}
			})
		}
	}
}

func TestCombinedProtocolMetadataIsNotUserMetadata(t *testing.T) {
	for _, value := range []string{"", "{", `{"v":1}`} {
		lws := wrappers.BuildLeaderWorkerSet("default").Obj()
		lws.Annotations = map[string]string{combinedRolloutAnnotation: value, "user.example/note": "keep"}
		_, annotations := leaderMetadata(lws, "revision")
		if _, found := annotations[combinedRolloutAnnotation]; found {
			t.Fatalf("controller-owned protocol key propagated from LWS: %q", value)
		}
		if annotations["user.example/note"] != "keep" || lws.Annotations[combinedRolloutAnnotation] != value {
			t.Fatal("filter altered user metadata or the source object")
		}
	}
}

func TestCombinedPodWatchOnlyRelevantChanges(t *testing.T) {
	pod := combinedTestGroup(0, "old", true).leader
	predicate := combinedPodWatchPredicate()
	if !predicate.Create(event.CreateEvent{Object: pod}) || !predicate.Delete(event.DeleteEvent{Object: pod}) || predicate.Generic(event.GenericEvent{Object: pod}) {
		t.Fatal("unexpected lifecycle predicate")
	}
	for _, tc := range []struct {
		name   string
		change func(*corev1.Pod)
		want   bool
	}{
		{"condition timestamp", func(p *corev1.Pod) { p.Status.Conditions[0].LastTransitionTime = metav1.Now() }, false},
		{"resource version", func(p *corev1.Pod) { p.ResourceVersion = "11" }, false},
		{"unrelated annotation", func(p *corev1.Pod) { p.Annotations["user.example/note"] = "new" }, false},
		{"readiness lost", func(p *corev1.Pod) { p.Status.Conditions[0].Status = corev1.ConditionFalse }, true},
		{"phase", func(p *corev1.Pod) { p.Status.Phase = corev1.PodPending }, true},
		{"termination", func(p *corev1.Pod) { p.DeletionTimestamp = ptr.To(metav1.Now()) }, true},
		{"owner", func(p *corev1.Pod) { p.OwnerReferences[0].UID = "replacement" }, true},
		{"revision", func(p *corev1.Pod) { p.Labels[leaderworkerset.RevisionKey] = "new" }, true},
		{"size", func(p *corev1.Pod) { p.Annotations[leaderworkerset.SizeAnnotationKey] = "2" }, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			next := pod.DeepCopy()
			tc.change(next)
			if got := predicate.Update(event.UpdateEvent{ObjectOld: pod, ObjectNew: next}); got != tc.want {
				t.Fatalf("predicate=%t, want %t", got, tc.want)
			}
		})
	}
}

func testCombinedMetadataLifecycle(t *testing.T, c client.Client, scheme *runtime.Scheme, namespace string) {
	lws := wrappers.BuildLeaderWorkerSet(namespace).Replica(1).Size(1).Obj()
	lws.Name = "protocol-user-metadata"
	lws.Annotations = map[string]string{combinedRolloutAnnotation: "{", "user.example/note": "keep"}
	if err := c.Create(context.Background(), lws); err != nil {
		t.Fatal(err)
	}
	f := &combinedAPIFixture{t: t, c: c, scheme: scheme, key: client.ObjectKeyFromObject(lws)}
	f.reconcile()
	if _, found := f.sts().Annotations[combinedRolloutAnnotation]; found {
		t.Fatal("creation propagated injected protocol state")
	}
	f.ack(true)
	f.leader(0, true)
	f.grow(2)
	if f.state().Phase != combinedActive {
		t.Fatal("injected user metadata prevented controller-owned publication")
	}
	old := &corev1.Pod{}
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: namespace, Name: lws.Name + "-0"}, old); err != nil {
		t.Fatal(err)
	}
	if err := c.Delete(context.Background(), old, client.GracePeriodSeconds(0)); err != nil {
		t.Fatal(err)
	}
	f.leader(0, true)
	f.leader(1, true)
	f.ack(true)
	f.reconcile()
	f.reconcile() // ordinary SSA after controller cleanup must not reintroduce it
	if _, found := f.sts().Annotations[combinedRolloutAnnotation]; found {
		t.Fatal("ordinary SSA reintroduced user-supplied protocol state after cleanup")
	}
	if f.sts().Annotations["user.example/note"] != "keep" || f.lws().Annotations[combinedRolloutAnnotation] != "{" {
		t.Fatal("lifecycle filtering mutated source or unrelated metadata")
	}
}

func testCombinedReviewScaleDown(t *testing.T, c client.Client, scheme *runtime.Scheme, namespace string) {
	ctx := context.Background()
	for _, phase := range []string{combinedFreeze, combinedPublish, combinedActive} {
		for _, desired := range []int32{0, 2} {
			t.Run(fmt.Sprintf("%s-to-%d", phase, desired), func(t *testing.T) {
				f := newCombinedAPIFixture(t, c, scheme, namespace, fmt.Sprintf("drain-%s-%d", phase, desired), 4)
				f.change(func(lws *leaderworkerset.LeaderWorkerSet) {
					lws.Spec.Replicas = ptr.To[int32](6)
					lws.Spec.LeaderWorkerTemplate.LeaderTemplate.Spec.Containers[0].Image = "test.invalid/new"
				})
				f.reconcile()
				if phase != combinedFreeze {
					f.ack(false)
					f.reconcile()
				}
				if phase == combinedActive {
					f.ack(false)
					f.reconcile()
					f.reconcile()
					f.ack(false)
				}
				before, state := f.sts(), f.state()
				if state.Phase != phase {
					t.Fatalf("wrong starting phase: %+v", state)
				}
				// Scale subresource does not match the LWS validating webhook rule.
				// Add scheduling here to exercise a permanently unavailable provider.
				f.change(func(lws *leaderworkerset.LeaderWorkerSet) {
					lws.Spec.Scheduling = &leaderworkerset.LeaderWorkerSetScheduling{}
				})
				scale := &autoscalingv1.Scale{}
				if err := c.SubResource("scale").Get(ctx, f.lws(), scale); err != nil {
					t.Fatal(err)
				}
				scale.Spec.Replicas = desired
				if err := c.SubResource("scale").Update(ctx, f.lws(), client.WithSubResourceBody(scale)); err != nil {
					t.Fatal(err)
				}
				r := f.reconciler() // intentionally no scheduling provider
				if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: f.key}); err != nil {
					t.Fatalf("explicit scale-down must not wait for scheduling or native acknowledgement: %v", err)
				}
				after, next := f.sts(), f.state()
				if *after.Spec.Replicas != desired || next.Desired != desired || next.Baseline != state.Baseline || next.Phase != state.Phase {
					t.Fatalf("scale-down lost target/baseline/phase: replicas=%d state=%+v", *after.Spec.Replicas, next)
				}
				if !reflect.DeepEqual(before.Spec.Template, after.Spec.Template) || *after.Spec.UpdateStrategy.RollingUpdate.Partition < *before.Spec.UpdateStrategy.RollingUpdate.Partition {
					t.Fatal("scale-only path published a template or exposed more old groups")
				}
				wantReservations := make(map[int32]combinedModelReservation)
				for ordinal, reservation := range state.Reservations {
					if ordinal < desired {
						wantReservations[ordinal] = reservation
					}
				}
				if !reflect.DeepEqual(next.Reservations, wantReservations) || next.Generation != state.Generation || next.Revision != state.Revision || next.NativeRevision != state.NativeRevision {
					t.Fatalf("scale-down changed surviving obligations or protocol identity: before=%+v after=%+v", state, next)
				}
				var pods corev1.PodList
				if err := c.List(ctx, &pods, client.InNamespace(namespace), client.MatchingLabels{leaderworkerset.SetNameLabelKey: f.key.Name}); err != nil {
					t.Fatal(err)
				}
				for _, pod := range pods.Items {
					if pod.DeletionTimestamp != nil {
						t.Fatal("scale-only pass explicitly deleted a Pod")
					}
				}
			})
		}
	}
	for _, supersede := range []bool{false, true} {
		for _, replacementPending := range []bool{false, true} {
			t.Run(fmt.Sprintf("surviving-reservation/supersede=%t/pending=%t", supersede, replacementPending), func(t *testing.T) {
				name := fmt.Sprintf("survivor-%t-%t", supersede, replacementPending)
				f := newCombinedAPIFixture(t, c, scheme, namespace, name, 4)
				f.change(func(lws *leaderworkerset.LeaderWorkerSet) {
					lws.Spec.RolloutStrategy.RollingUpdateConfiguration.MaxUnavailable = intstr.FromInt(2)
				})
				f.grow(6)
				original := f.state()
				if len(original.Reservations) != 2 || original.Reservations[2].UID == "" || original.Reservations[3].UID == "" {
					t.Fatalf("fixture needs reservations on both sides of boundary: %+v", original)
				}
				currentUID := original.Reservations[2].UID
				if replacementPending {
					old := &corev1.Pod{}
					key := client.ObjectKey{Namespace: namespace, Name: name + "-2"}
					if err := c.Get(ctx, key, old); err != nil {
						t.Fatal(err)
					}
					if err := c.Delete(ctx, old, client.GracePeriodSeconds(0)); err != nil {
						t.Fatal(err)
					}
					leader := f.leader(2, true)
					currentUID = leader.UID
					leader.Annotations[leaderworkerset.SizeAnnotationKey] = "2" // Ready leader, missing/unready workers
					if err := c.Update(ctx, leader); err != nil {
						t.Fatal(err)
					}
				}
				before := f.sts()
				f.change(func(lws *leaderworkerset.LeaderWorkerSet) { lws.Spec.Replicas = ptr.To[int32](3) })
				f.reconcile()
				scaled := f.sts()
				next := f.state()
				want := map[int32]combinedModelReservation{2: original.Reservations[2]}
				if !reflect.DeepEqual(next.Reservations, want) || next.Baseline != 4 || next.Desired != 3 || next.Generation != original.Generation || next.Revision != original.Revision || next.NativeRevision != original.NativeRevision || *scaled.Spec.UpdateStrategy.RollingUpdate.Partition != *before.Spec.UpdateStrategy.RollingUpdate.Partition {
					t.Fatalf("scale-only pass changed a surviving payment/identity: %+v", next)
				}
				// No native acknowledgement or cleanup. A superseding template must
				// freeze, while same-template growth must await the scale acknowledgement.
				f.change(func(lws *leaderworkerset.LeaderWorkerSet) {
					lws.Spec.Replicas = ptr.To[int32](6)
					if supersede {
						lws.Spec.LeaderWorkerTemplate.LeaderTemplate.Spec.Containers[0].Image = "test.invalid/newest"
					}
				})
				for range 3 {
					f.reconcile()
				}
				if *f.sts().Spec.Replicas != 3 || f.state().Reservations[2].UID != original.Reservations[2].UID || f.state().Baseline != 4 {
					t.Fatal("unacknowledged growth lost payment or grew early")
				}
				if !reflect.DeepEqual(f.sts().Spec.Template, scaled.Spec.Template) || (supersede && f.state().Phase != combinedFreeze) {
					t.Fatal("supersession published before freeze acknowledgement")
				}
				for range 6 {
					f.ack(false)
					f.reconcile()
					if *f.sts().Spec.Replicas == 6 && f.state().Phase == combinedActive {
						break
					}
				}
				state := f.state()
				wantUID := original.Reservations[2].UID
				if supersede {
					// The pending replacement is itself stale for the newest template.
					// Retarget the still-paid obligation, never refund its credit.
					wantUID = currentUID
				}
				if *f.sts().Spec.Replicas != 6 || state.Baseline != 4 || state.Reservations[2].UID != wantUID || state.Reservations[2].Revision != state.Revision {
					t.Fatalf("acknowledged growth failed or refunded unready/same-UID payment: %+v", state)
				}
				if *f.sts().Spec.UpdateStrategy.RollingUpdate.Partition <= 3 && state.Reservations[3].UID != original.Reservations[3].UID {
					t.Fatal("re-exposed condemned old leader without a new persisted reservation")
				}
				// Simulate native replacement/GC and readiness to prove continuation,
				// not just a stable barrier. No ready credit is fabricated in-place.
				var pods corev1.PodList
				if err := c.List(ctx, &pods, client.InNamespace(namespace), client.MatchingLabels{leaderworkerset.SetNameLabelKey: name}); err != nil {
					t.Fatal(err)
				}
				for i := range pods.Items {
					if err := c.Delete(ctx, &pods.Items[i], client.GracePeriodSeconds(0)); err != nil {
						t.Fatal(err)
					}
				}
				f.ack(true)
				for i := 0; i < 6; i++ {
					f.leader(i, true)
				}
				f.reconcile()
				if f.sts().Annotations[combinedRolloutAnnotation] != "" {
					t.Fatal("ready acknowledged continuation did not converge")
				}
			})
		}
	}
	t.Run("scale subresource rounds unavailable to zero without increasing disruption", func(t *testing.T) {
		f := newCombinedAPIFixture(t, c, scheme, namespace, "percentage-scale", 4)
		f.change(func(lws *leaderworkerset.LeaderWorkerSet) {
			lws.Spec.RolloutStrategy.RollingUpdateConfiguration.MaxUnavailable = intstr.FromString("25%")
		})
		f.grow(6)
		scale := &autoscalingv1.Scale{}
		if err := c.SubResource("scale").Get(ctx, f.lws(), scale); err != nil {
			t.Fatal(err)
		}
		scale.Spec.Replicas = 2
		if err := c.SubResource("scale").Update(ctx, f.lws(), client.WithSubResourceBody(scale)); err != nil {
			t.Fatal(err)
		}
		f.reconcile()
		f.ack(false)
		f.reconcile() // persist the post-downscale generation/partition plan
		f.ack(false)
		for range 3 {
			result, err := f.reconciler().Reconcile(ctx, ctrl.Request{NamespacedName: f.key})
			if err != nil || result.RequeueAfter < 30*time.Second {
				t.Fatalf("blocked rollout must use bounded slow resync, got %+v %v", result, err)
			}
		}
		if *f.sts().Spec.Replicas != 2 || f.state().Baseline != 4 || len(f.state().Reservations) != 0 {
			t.Fatal("rounded-zero budget wedged scale-down or authorized another replacement")
		}
	})
}

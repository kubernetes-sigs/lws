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
	"fmt"
	"strconv"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
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
	r.APIReader = f.c
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
			c := fake.NewClientBuilder().WithObjects(leader, worker).Build()
			r := NewPodReconciler(c, c.Scheme(), fakeEventRecorder{}, nil)
			deleted, err := r.handleRestartPolicy(context.Background(), *restarting, *lws)
			if err != nil || !deleted {
				t.Fatalf("old size-two group suppressed under live size-one: deleted=%t err=%v", deleted, err)
			}
		})
	}
}

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
	"strconv"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	leaderworkerset "sigs.k8s.io/lws/api/leaderworkerset/v1"
	"sigs.k8s.io/lws/test/wrappers"
)

// Runs actual Reconcile and real SSA on envtest. Native acknowledgement and
// Pod health are simulated explicitly; this is not native-controller E2E.
func testCombinedReconcile(t *testing.T, c client.Client, scheme *runtime.Scheme, namespace string) {
	for _, tc := range []struct {
		b, d int32
		u    int
	}{{1, 2, 1}, {1, 2, 0}, {4, 8, 1}} {
		t.Run(fmt.Sprintf("%d-to-%d-u%d", tc.b, tc.d, tc.u), func(t *testing.T) {
			ctx := context.Background()
			lws := wrappers.BuildLeaderWorkerSet(namespace).Replica(int(tc.b)).Size(1).Obj()
			lws.Name = fmt.Sprintf("combined-%d-%d", tc.b, tc.u)
			lws.Spec.RolloutStrategy.RollingUpdateConfiguration.MaxUnavailable = intstr.FromInt(tc.u)
			if tc.u == 0 {
				lws.Spec.RolloutStrategy.RollingUpdateConfiguration.MaxSurge = intstr.FromInt(1)
			}
			if err := c.Create(ctx, lws); err != nil {
				t.Fatal(err)
			}
			r := NewLeaderWorkerSetReconciler(c, scheme, fakeEventRecorder{})
			r.APIReader = c
			key := client.ObjectKeyFromObject(lws)
			reconcile := func() {
				t.Helper()
				if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key}); err != nil {
					t.Fatal(err)
				}
			}
			getSTS := func() *appsv1.StatefulSet {
				t.Helper()
				s := &appsv1.StatefulSet{}
				if err := c.Get(ctx, key, s); err != nil {
					t.Fatal(err)
				}
				return s
			}
			ack := func() {
				t.Helper()
				s := getSTS()
				s.Status.ObservedGeneration = s.Generation
				s.Status.CurrentRevision = "native-old"
				s.Status.UpdateRevision = "native-" + s.Spec.Template.Labels[leaderworkerset.RevisionKey]
				s.Status.Replicas = *s.Spec.Replicas
				if err := c.Status().Update(ctx, s); err != nil {
					t.Fatal(err)
				}
			}
			state := func() combinedModelState {
				t.Helper()
				s, err := combinedModelDecode(getSTS().Annotations[combinedRolloutAnnotation])
				if err != nil {
					t.Fatal(err)
				}
				return s
			}
			makePod := func(i int32, ready bool) *corev1.Pod {
				t.Helper()
				s := getSTS()
				p := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("%s-%d", lws.Name, i), Namespace: namespace,
					Labels:          map[string]string{leaderworkerset.SetNameLabelKey: lws.Name, leaderworkerset.GroupIndexLabelKey: strconv.Itoa(int(i)), leaderworkerset.WorkerIndexLabelKey: "0", leaderworkerset.RevisionKey: s.Spec.Template.Labels[leaderworkerset.RevisionKey], appsv1.ControllerRevisionHashLabelKey: s.Status.UpdateRevision},
					Annotations:     map[string]string{leaderworkerset.SizeAnnotationKey: "1"},
					OwnerReferences: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "StatefulSet", Name: s.Name, UID: s.UID, Controller: ptr.To(true)}}},
					Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "test", Image: "test.invalid/image"}}}}
				if err := c.Create(ctx, p); err != nil {
					t.Fatal(err)
				}
				if ready {
					p.Status.Phase = corev1.PodRunning
					p.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
					if err := c.Status().Update(ctx, p); err != nil {
						t.Fatal(err)
					}
				}
				return p
			}
			reconcile()
			ack()
			for i := int32(0); i < tc.b; i++ {
				makePod(i, true)
			}
			oldTemplate := getSTS().Spec.Template.Spec.Containers[0].Image
			if err := c.Get(ctx, key, lws); err != nil {
				t.Fatal(err)
			}
			lws.Spec.Replicas = ptr.To(tc.d)
			lws.Spec.LeaderWorkerTemplate.LeaderTemplate.Spec.Containers[0].Image = "test.invalid/new"
			if err := c.Update(ctx, lws); err != nil {
				t.Fatal(err)
			}
			reconcile()
			if state().Phase != combinedFreeze || state().Baseline != tc.b || getSTS().Spec.Template.Spec.Containers[0].Image != oldTemplate {
				t.Fatal("entry did not freeze old template/baseline")
			}
			revisionCount := func() int {
				t.Helper()
				var revisions appsv1.ControllerRevisionList
				if err := c.List(ctx, &revisions, client.InNamespace(namespace), client.MatchingLabels{leaderworkerset.SetNameLabelKey: lws.Name}); err != nil {
					t.Fatal(err)
				}
				return len(revisions.Items)
			}
			count := revisionCount()
			for i := 0; i < 5; i++ {
				r = NewLeaderWorkerSetReconciler(c, scheme, fakeEventRecorder{})
				r.APIReader = c
				reconcile()
			}
			if revisionCount() != count || count != 2 {
				t.Fatalf("unchanged freeze created duplicate revisions: %d -> %d", count, revisionCount())
			}
			if state().Phase != combinedFreeze {
				t.Fatal("advanced before native freeze acknowledgement")
			}
			ack()
			reconcile()
			if state().Phase != combinedPublish || *getSTS().Spec.Replicas != tc.b || *getSTS().Spec.UpdateStrategy.RollingUpdate.Partition < tc.b {
				t.Fatal("publication was not frozen")
			}
			reconcile()
			if state().Phase != combinedPublish {
				t.Fatal("advanced before native publication acknowledgement")
			}
			ack()
			reconcile()
			reconcile()
			if state().Phase != combinedActive || *getSTS().Spec.Replicas != tc.d {
				t.Fatal("active planner did not create desired additions")
			}
			if len(state().Reservations) != tc.u {
				t.Fatalf("reservations=%v, want %d", state().Reservations, tc.u)
			}
			ack()
			for i := tc.b; i < tc.d; i++ {
				makePod(i, false)
			}
			if tc.u == 0 {
				reconcile()
				if len(state().Reservations) != 0 {
					t.Fatal("Pending addition supplied credit")
				}
				p := &corev1.Pod{}
				if err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: lws.Name + "-1"}, p); err != nil {
					t.Fatal(err)
				}
				p.Status.Phase = corev1.PodRunning
				p.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
				if err := c.Status().Update(ctx, p); err != nil {
					t.Fatal(err)
				}
				reconcile()
				ack()
				if len(state().Reservations) != 1 {
					t.Fatal("Ready addition failed to finance replacement")
				}
			}
			// A restart observes the same persisted payment. A stale LWS
			// generation cannot use it to delete, even with a fresh Pod UID.
			stale := lws.DeepCopy()
			if err := c.Get(ctx, key, lws); err != nil {
				t.Fatal(err)
			}
			stale.Generation = lws.Generation - 1
			pod := &corev1.Pod{}
			if err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: fmt.Sprintf("%s-%d", lws.Name, tc.b-1)}, pod); err != nil {
				t.Fatal(err)
			}
			if err := r.deleteCombinedLeader(ctx, stale, getSTS(), pod); err == nil {
				t.Fatal("stale LWS generation deleted a leader")
			}
			r = NewLeaderWorkerSetReconciler(c, scheme, fakeEventRecorder{})
			r.APIReader = c
			reconcile()
			if err := c.Get(ctx, client.ObjectKeyFromObject(pod), pod); err != nil {
				t.Fatal(err)
			}
			if pod.DeletionTimestamp == nil {
				t.Fatal("authorized old leader not deleted behind Pending addition")
			}
			if len(state().Reservations) != 1 {
				t.Fatal("deletion dropped persisted payment")
			}
			// Supersession and HPA retain B and freeze before any new template.
			if err := c.Get(ctx, key, lws); err != nil {
				t.Fatal(err)
			}
			lws.Spec.Replicas = ptr.To(tc.d + 1)
			lws.Spec.LeaderWorkerTemplate.LeaderTemplate.Spec.Containers[0].Image = "test.invalid/newer"
			if err := c.Update(ctx, lws); err != nil {
				t.Fatal(err)
			}
			before := getSTS().Spec.Template.Spec.Containers[0].Image
			reconcile()
			if state().Baseline != tc.b || state().Phase != combinedFreeze || getSTS().Spec.Template.Spec.Containers[0].Image != before {
				t.Fatal("supersession rebaselined or skipped freeze")
			}
			// Corrupt state must fail closed in the actual Reconcile path.
			s := getSTS()
			s.Annotations[combinedRolloutAnnotation] = "{"
			if err := c.Update(ctx, s); err != nil {
				t.Fatal(err)
			}
			if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key}); err == nil {
				t.Fatal("malformed state accepted")
			}
		})
	}
}

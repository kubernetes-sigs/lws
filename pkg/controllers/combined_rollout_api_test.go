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
	"os"
	"path/filepath"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	leaderworkerset "sigs.k8s.io/lws/api/leaderworkerset/v1"
)

// This tests real API semantics, not fake-client emulation of SSA/deletion.
// envtest has no StatefulSet controller, scheduler, kubelet or garbage collector.
// Replacement creation/status below is explicitly simulated; this is NOT an
// end-to-end reproduction of a native-controller race.
func TestCombinedRolloutAPIPreconditions(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("set KUBEBUILDER_ASSETS to existing local envtest binaries")
	}
	environment := &envtest.Environment{CRDDirectoryPaths: []string{filepath.Join("..", "..", "config", "crd", "bases")}, ErrorIfCRDPathMissing: true}
	config, err := environment.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := environment.Stop(); err != nil {
			t.Errorf("stop envtest: %v", err)
		}
	})
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := leaderworkerset.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	c, err := client.New(config, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "combined-rollout-"}}
	if err := c.Create(ctx, ns); err != nil {
		t.Fatal(err)
	}
	t.Run("production Reconcile persisted phases and fences", func(t *testing.T) {
		testCombinedReconcile(t, c, scheme, ns.Name)
	})
	t.Run("production regression fences and ownership", func(t *testing.T) {
		testCombinedRegressions(t, c, scheme, ns.Name)
	})

	t.Run("SSA persists partition and reservations atomically and rejects stale plans", func(t *testing.T) {
		sts := &appsv1.StatefulSet{
			ObjectMeta: metav1.ObjectMeta{Name: "example", Namespace: ns.Name},
			Spec: appsv1.StatefulSetSpec{
				ServiceName: "example", Replicas: ptr.To[int32](2), PodManagementPolicy: appsv1.ParallelPodManagement,
				Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "example"}},
				Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "example"}},
					Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "test", Image: "test.invalid/image"}}}},
				UpdateStrategy: appsv1.StatefulSetUpdateStrategy{Type: appsv1.RollingUpdateStatefulSetStrategyType,
					RollingUpdate: &appsv1.RollingUpdateStatefulSetStrategy{Partition: ptr.To[int32](2)}},
			},
		}
		if err := c.Create(ctx, sts); err != nil {
			t.Fatal(err)
		}
		oldRV := sts.ResourceVersion
		annotation := "leaderworkerset.sigs.k8s.io/combined-rollout-experiment"
		apply := func(rv, state string, partition int64) error {
			patch := &unstructured.Unstructured{Object: map[string]interface{}{
				"apiVersion": "apps/v1", "kind": "StatefulSet",
				"metadata": map[string]interface{}{"name": sts.Name, "namespace": ns.Name, "resourceVersion": rv,
					"annotations": map[string]interface{}{annotation: state}},
				"spec": map[string]interface{}{"updateStrategy": map[string]interface{}{"type": "RollingUpdate",
					"rollingUpdate": map[string]interface{}{"partition": partition}}},
			}}
			return c.Patch(ctx, patch, client.Apply, client.FieldOwner("combined-rollout-experiment"), client.ForceOwnership) //nolint:staticcheck // exercises the existing controller's SSA path
		}
		if err := apply(oldRV, "reservation-one", 1); err != nil {
			t.Fatal(err)
		}
		if err := apply(oldRV, "stale-second-payment", 0); !apierrors.IsConflict(err) {
			t.Fatalf("stale SSA must conflict even with force ownership; got %v", err)
		}
		if err := c.Get(ctx, client.ObjectKeyFromObject(sts), sts); err != nil {
			t.Fatal(err)
		}
		if *sts.Spec.UpdateStrategy.RollingUpdate.Partition != 1 || sts.Annotations[annotation] != "reservation-one" {
			t.Fatalf("stale write altered partition/reservation: %+v", sts)
		}
		t.Log("real apiserver: stale SSA conflicted; partition and reservation remained atomic")
		sts.Spec.UpdateStrategy.Type = appsv1.OnDeleteStatefulSetStrategyType
		if err := c.Update(ctx, sts); !apierrors.IsInvalid(err) {
			t.Fatalf("OnDelete with rollingUpdate.partition must be rejected: %v", err)
		}
		sts.Spec.UpdateStrategy.RollingUpdate = nil
		if err := c.Update(ctx, sts); err != nil {
			t.Fatal(err)
		}
		t.Log("real apiserver: OnDelete rejected partition; accepted only after removing rollingUpdate")
	})

	t.Run("Pod UID and resourceVersion fences protect retries", func(t *testing.T) {
		pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "example-0", Namespace: ns.Name},
			Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "test", Image: "test.invalid/image"}}}}
		if err := c.Create(ctx, pod); err != nil {
			t.Fatal(err)
		}
		original := pod.DeepCopy()
		deleteOriginal := func() error {
			return c.Delete(ctx, original, &client.DeleteOptions{
				GracePeriodSeconds: ptr.To[int64](0),
				Preconditions:      &metav1.Preconditions{UID: &original.UID, ResourceVersion: &original.ResourceVersion},
			})
		}
		pod.Labels = map[string]string{"observation": "changed"}
		if err := c.Update(ctx, pod); err != nil {
			t.Fatal(err)
		}
		if err := deleteOriginal(); !apierrors.IsConflict(err) {
			t.Fatalf("changed resourceVersion must reject deletion: %v", err)
		}
		original = pod.DeepCopy()
		if err := deleteOriginal(); err != nil {
			t.Fatal(err)
		}
		replacement := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: pod.Name, Namespace: ns.Name}, Spec: pod.Spec}
		if err := c.Create(ctx, replacement); err != nil {
			t.Fatal(err)
		}
		if replacement.UID == original.UID {
			t.Fatal("expected a new UID")
		}
		if err := deleteOriginal(); !apierrors.IsConflict(err) {
			t.Fatalf("replacement UID must reject stale LWS deletion: %v", err)
		}
		// v1.35's native object manager sends a delete by name without these
		// preconditions. Demonstrate the API difference, not an interleaving:
		// only a native-controller test can establish whether it issues such a
		// stale request, given its per-key serialized creation/update loop.
		if err := c.Delete(ctx, original, &client.DeleteOptions{GracePeriodSeconds: ptr.To[int64](0)}); err != nil {
			t.Fatal(err)
		}
		if err := c.Get(ctx, client.ObjectKeyFromObject(replacement), &corev1.Pod{}); !apierrors.IsNotFound(err) {
			t.Fatalf("unconditioned by-name delete should remove the replacement: %v", err)
		}
		t.Log("real apiserver: RV/UID stale deletes conflicted; unconditioned by-name delete removed replacement")
	})
}

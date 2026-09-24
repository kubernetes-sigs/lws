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
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	leaderworkerset "sigs.k8s.io/lws/api/leaderworkerset/v1"
	revisionutils "sigs.k8s.io/lws/pkg/utils/revision"
	"sigs.k8s.io/lws/test/wrappers"
)

// lwsStatusHashGroupSize is the group size used by the hash-identity fixtures.
// It is greater than 1 so that leader pods carry the group-ready readiness gate.
const lwsStatusHashGroupSize = 2

// lwsStatusHashLWS builds a hash-identity LeaderWorkerSet.
func lwsStatusHashLWS(replicas int) *leaderworkerset.LeaderWorkerSet {
	lws := wrappers.BuildLeaderWorkerSet("default").Replica(replicas).Size(lwsStatusHashGroupSize).Obj()
	lws.Spec.GroupIdentity = leaderworkerset.GroupIdentityHash
	lws.UID = types.UID("lws-uid")
	return lws
}

func TestGetLeaderDeployment(t *testing.T) {
	lws := lwsStatusHashLWS(2)
	existing := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: lws.Name, Namespace: lws.Namespace},
		Spec:       appsv1.DeploymentSpec{Replicas: ptr.To[int32](2)},
	}

	tests := []struct {
		name    string
		objs    []client.Object
		funcs   interceptor.Funcs
		wantNil bool
		wantErr bool
	}{
		{
			name: "leader deployment exists, it is returned",
			objs: []client.Object{existing},
		},
		{
			name:    "leader deployment missing, nil is returned without an error",
			wantNil: true,
		},
		{
			name: "non NotFound errors are propagated",
			funcs: interceptor.Funcs{
				Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
					return apierrors.NewInternalError(errors.New("boom"))
				},
			},
			wantNil: true,
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			reconciler, _ := lwsStatusNewReconcilerWithInterceptor(t, tc.funcs, tc.objs...)
			deploy, err := reconciler.getLeaderDeployment(context.Background(), lws)
			if tc.wantErr != (err != nil) {
				t.Fatalf("getLeaderDeployment() error = %v, wantErr %t", err, tc.wantErr)
			}
			if tc.wantNil {
				if deploy != nil {
					t.Fatalf("getLeaderDeployment() = %v, want nil", deploy)
				}
				return
			}
			if deploy == nil {
				t.Fatal("getLeaderDeployment() = nil, want a deployment")
			}
			if *deploy.Spec.Replicas != 2 {
				t.Errorf("getLeaderDeployment() replicas = %d, want 2", *deploy.Spec.Replicas)
			}
		})
	}
}

func TestSSAWithDeployment(t *testing.T) {
	ctx := context.Background()
	lws := lwsStatusHashLWS(3)
	reconciler, k8sClient := lwsStatusNewReconciler(t, lws)

	if err := reconciler.SSAWithDeployment(ctx, lws, "rev-1"); err != nil {
		t.Fatalf("SSAWithDeployment() unexpected error: %v", err)
	}

	var deploy appsv1.Deployment
	key := types.NamespacedName{Name: lws.Name, Namespace: lws.Namespace}
	if err := k8sClient.Get(ctx, key, &deploy); err != nil {
		t.Fatalf("reading back the leader deployment: %v", err)
	}
	if *deploy.Spec.Replicas != 3 {
		t.Errorf("leader deployment replicas = %d, want 3", *deploy.Spec.Replicas)
	}
	if got := revisionutils.GetRevisionKey(&deploy); got != "rev-1" {
		t.Errorf("leader deployment revision key = %q, want %q", got, "rev-1")
	}
	if got := deploy.Spec.Template.Spec.Subdomain; got != lws.Name {
		t.Errorf("leader pod template subdomain = %q, want %q", got, lws.Name)
	}
	// Size > 1 means the group-ready gate paces the rollout.
	wantGates := []corev1.PodReadinessGate{{ConditionType: leaderworkerset.GroupReadyConditionType}}
	if diff := cmp.Diff(wantGates, deploy.Spec.Template.Spec.ReadinessGates); diff != "" {
		t.Errorf("unexpected readiness gates (-want +got):\n%s", diff)
	}
	if got := deploy.Spec.Template.Annotations[leaderworkerset.GroupIdentityAnnotationKey]; got != string(leaderworkerset.GroupIdentityHash) {
		t.Errorf("group identity annotation = %q, want %q", got, leaderworkerset.GroupIdentityHash)
	}
	owner := metav1.GetControllerOf(&deploy)
	if owner == nil || owner.Kind != "LeaderWorkerSet" || owner.Name != lws.Name {
		t.Errorf("leader deployment controller owner = %+v, want the leaderworkerset", owner)
	}

	// Re-applying converges the existing object.
	if err := reconciler.SSAWithDeployment(ctx, lws, "rev-2"); err != nil {
		t.Fatalf("SSAWithDeployment() second apply unexpected error: %v", err)
	}
	if err := k8sClient.Get(ctx, key, &deploy); err != nil {
		t.Fatalf("reading back the leader deployment: %v", err)
	}
	if got := revisionutils.GetRevisionKey(&deploy); got != "rev-2" {
		t.Errorf("leader deployment revision key after re-apply = %q, want %q", got, "rev-2")
	}
}

func TestSSAWithDeploymentOwnerReferenceError(t *testing.T) {
	// A scheme without the LWS types cannot resolve the owner GVK.
	scheme := runtime.NewScheme()
	if err := appsv1.AddToScheme(scheme); err != nil {
		t.Fatalf("building scheme: %v", err)
	}
	reconciler := &LeaderWorkerSetReconciler{
		Client: fake.NewClientBuilder().WithScheme(scheme).Build(),
		Scheme: scheme,
		Record: fakeEventRecorder{},
	}

	if err := reconciler.SSAWithDeployment(context.Background(), lwsStatusHashLWS(2), "rev-1"); err == nil {
		t.Fatal("SSAWithDeployment() error = nil, want an error for the unregistered owner type")
	}
}

func TestUpdateStatusHash(t *testing.T) {
	tests := []struct {
		name                string
		deployStatus        appsv1.DeploymentStatus
		wantAvailable       bool
		wantReadyReplicas   int32
		wantUpdatedReplicas int32
		wantConditions      []string
	}{
		{
			name: "deployment fully rolled out, the set is Available",
			deployStatus: appsv1.DeploymentStatus{
				Replicas: 2, ReadyReplicas: 2, UpdatedReplicas: 2,
			},
			wantAvailable:       true,
			wantReadyReplicas:   2,
			wantUpdatedReplicas: 2,
			wantConditions:      []string{string(leaderworkerset.LeaderWorkerSetAvailable)},
		},
		{
			name: "deployment still has old pods, the set is UpdateInProgress",
			deployStatus: appsv1.DeploymentStatus{
				Replicas: 3, ReadyReplicas: 2, UpdatedReplicas: 2,
			},
			wantAvailable:       false,
			wantReadyReplicas:   2,
			wantUpdatedReplicas: 2,
			// updateStatusHash asks for both UpdateInProgress and Progressing
			// here, and both land in the same reconcile.
			wantConditions: []string{
				string(leaderworkerset.LeaderWorkerSetProgressing),
				string(leaderworkerset.LeaderWorkerSetUpdateInProgress),
			},
		},
		{
			name: "deployment is scaling up, the set is only Progressing",
			deployStatus: appsv1.DeploymentStatus{
				Replicas: 1, ReadyReplicas: 1, UpdatedReplicas: 1,
			},
			wantAvailable:       false,
			wantReadyReplicas:   1,
			wantUpdatedReplicas: 1,
			wantConditions:      []string{string(leaderworkerset.LeaderWorkerSetProgressing)},
		},
		{
			name: "all pods updated but not ready yet, the set is only Progressing",
			deployStatus: appsv1.DeploymentStatus{
				Replicas: 2, ReadyReplicas: 0, UpdatedReplicas: 2,
			},
			wantAvailable:       false,
			wantReadyReplicas:   0,
			wantUpdatedReplicas: 2,
			wantConditions:      []string{string(leaderworkerset.LeaderWorkerSetProgressing)},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			lws := lwsStatusHashLWS(2)
			lws.Generation = 4
			deploy := &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{Name: lws.Name, Namespace: lws.Namespace},
				Spec:       appsv1.DeploymentSpec{Replicas: ptr.To[int32](2)},
				Status:     tc.deployStatus,
			}
			reconciler, k8sClient := lwsStatusNewReconciler(t, lws, deploy)

			available, err := reconciler.updateStatusHash(context.Background(), lws)
			if err != nil {
				t.Fatalf("updateStatusHash() unexpected error: %v", err)
			}
			if available != tc.wantAvailable {
				t.Errorf("updateStatusHash() = %t, want %t", available, tc.wantAvailable)
			}

			var persisted leaderworkerset.LeaderWorkerSet
			if err := k8sClient.Get(context.Background(), types.NamespacedName{Name: lws.Name, Namespace: lws.Namespace}, &persisted); err != nil {
				t.Fatalf("reading back the leaderworkerset: %v", err)
			}
			if persisted.Status.Replicas != tc.deployStatus.Replicas {
				t.Errorf("persisted status.replicas = %d, want %d", persisted.Status.Replicas, tc.deployStatus.Replicas)
			}
			if persisted.Status.ReadyReplicas != tc.wantReadyReplicas {
				t.Errorf("persisted status.readyReplicas = %d, want %d", persisted.Status.ReadyReplicas, tc.wantReadyReplicas)
			}
			if persisted.Status.UpdatedReplicas != tc.wantUpdatedReplicas {
				t.Errorf("persisted status.updatedReplicas = %d, want %d", persisted.Status.UpdatedReplicas, tc.wantUpdatedReplicas)
			}
			if persisted.Status.ObservedGeneration != 4 {
				t.Errorf("persisted status.observedGeneration = %d, want 4", persisted.Status.ObservedGeneration)
			}
			if persisted.Status.HPAPodSelector == "" {
				t.Error("persisted status.hpaPodSelector is empty, want the leader pod selector")
			}
			if diff := cmp.Diff(tc.wantConditions, lwsStatusTrueConditionTypes(&persisted)); diff != "" {
				t.Errorf("unexpected persisted conditions (-want +got):\n%s", diff)
			}
		})
	}
}

func TestUpdateStatusHashDeploymentMissing(t *testing.T) {
	lws := lwsStatusHashLWS(2)
	reconciler, _ := lwsStatusNewReconciler(t, lws)

	if _, err := reconciler.updateStatusHash(context.Background(), lws); !apierrors.IsNotFound(err) {
		t.Fatalf("updateStatusHash() error = %v, want NotFound", err)
	}
}

func TestUpdateStatusHashNoWriteWhenUnchanged(t *testing.T) {
	lws := lwsStatusHashLWS(2)
	lws.Generation = 1
	lws.Status = leaderworkerset.LeaderWorkerSetStatus{
		Replicas:           2,
		ReadyReplicas:      2,
		UpdatedReplicas:    2,
		ObservedGeneration: 1,
		HPAPodSelector:     "leaderworkerset.sigs.k8s.io/name=test-sample,leaderworkerset.sigs.k8s.io/worker-index=0",
		Conditions: []metav1.Condition{{
			Type:               string(leaderworkerset.LeaderWorkerSetAvailable),
			Status:             metav1.ConditionTrue,
			ObservedGeneration: 1,
			Reason:             "AllGroupsReady",
			Message:            "All replicas are ready",
			LastTransitionTime: metav1.Now(),
		}},
	}
	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: lws.Name, Namespace: lws.Namespace},
		Spec:       appsv1.DeploymentSpec{Replicas: ptr.To[int32](2)},
		Status:     appsv1.DeploymentStatus{Replicas: 2, ReadyReplicas: 2, UpdatedReplicas: 2},
	}

	statusWrites := 0
	reconciler, _ := lwsStatusNewReconcilerWithInterceptor(t, interceptor.Funcs{
		SubResourceUpdate: func(ctx context.Context, c client.Client, subResourceName string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
			statusWrites++
			return c.SubResource(subResourceName).Update(ctx, obj, opts...)
		},
	}, lws, deploy)

	available, err := reconciler.updateStatusHash(context.Background(), lws)
	if err != nil {
		t.Fatalf("updateStatusHash() unexpected error: %v", err)
	}
	if !available {
		t.Error("updateStatusHash() = false, want true")
	}
	if statusWrites != 0 {
		t.Errorf("status was written %d times, want 0", statusWrites)
	}
}

func TestReconcileHash(t *testing.T) {
	ctx := context.Background()

	t.Run("first reconcile creates the revision, deployment and headless service", func(t *testing.T) {
		lws := lwsStatusHashLWS(2)
		reconciler, k8sClient := lwsStatusNewReconciler(t, lws)

		result, err := reconciler.reconcileHash(ctx, lws)
		if err != nil {
			t.Fatalf("reconcileHash() unexpected error: %v", err)
		}
		if diff := cmp.Diff(time.Duration(0), result.RequeueAfter); diff != "" {
			t.Errorf("unexpected requeueAfter (-want +got):\n%s", diff)
		}

		key := types.NamespacedName{Name: lws.Name, Namespace: lws.Namespace}
		var deploy appsv1.Deployment
		if err := k8sClient.Get(ctx, key, &deploy); err != nil {
			t.Fatalf("leader deployment was not created: %v", err)
		}

		var revisions appsv1.ControllerRevisionList
		if err := k8sClient.List(ctx, &revisions, client.InNamespace(lws.Namespace)); err != nil {
			t.Fatalf("listing revisions: %v", err)
		}
		if len(revisions.Items) != 1 {
			t.Fatalf("got %d controller revisions, want 1", len(revisions.Items))
		}
		if got, want := revisionutils.GetRevisionKey(&deploy), revisionutils.GetRevisionKey(&revisions.Items[0]); got != want {
			t.Errorf("leader deployment revision key = %q, want the created revision key %q", got, want)
		}

		var service corev1.Service
		if err := k8sClient.Get(ctx, key, &service); err != nil {
			t.Fatalf("headless service was not created: %v", err)
		}
		if service.Spec.ClusterIP != "None" {
			t.Errorf("service clusterIP = %q, want None", service.Spec.ClusterIP)
		}

		// The freshly created deployment reports an empty status, so the set is
		// still progressing.
		var persisted leaderworkerset.LeaderWorkerSet
		if err := k8sClient.Get(ctx, key, &persisted); err != nil {
			t.Fatalf("reading back the leaderworkerset: %v", err)
		}
		want := []string{string(leaderworkerset.LeaderWorkerSetProgressing)}
		if diff := cmp.Diff(want, lwsStatusTrueConditionTypes(&persisted)); diff != "" {
			t.Errorf("unexpected conditions (-want +got):\n%s", diff)
		}
	})

	t.Run("a second reconcile is idempotent and does not create another revision", func(t *testing.T) {
		lws := lwsStatusHashLWS(2)
		reconciler, k8sClient := lwsStatusNewReconciler(t, lws)

		for i := range 2 {
			if _, err := reconciler.reconcileHash(ctx, lws); err != nil {
				t.Fatalf("reconcileHash() call %d unexpected error: %v", i, err)
			}
		}

		var revisions appsv1.ControllerRevisionList
		if err := k8sClient.List(ctx, &revisions, client.InNamespace(lws.Namespace)); err != nil {
			t.Fatalf("listing revisions: %v", err)
		}
		if len(revisions.Items) != 1 {
			t.Errorf("got %d controller revisions after two reconciles, want 1", len(revisions.Items))
		}
	})

	t.Run("leader deployment is being deleted, reconcile backs off", func(t *testing.T) {
		lws := lwsStatusHashLWS(2)
		deletionTimestamp := metav1.NewTime(time.Unix(0, 0))
		deploy := &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{
				Name:              lws.Name,
				Namespace:         lws.Namespace,
				DeletionTimestamp: &deletionTimestamp,
				Finalizers:        []string{"lws.sigs.k8s.io/test"},
			},
			Spec: appsv1.DeploymentSpec{Replicas: ptr.To[int32](2)},
		}
		reconciler, _ := lwsStatusNewReconciler(t, lws, deploy)

		result, err := reconciler.reconcileHash(ctx, lws)
		if err != nil {
			t.Fatalf("reconcileHash() unexpected error: %v", err)
		}
		if result.RequeueAfter != 5*time.Second {
			t.Errorf("reconcileHash() requeueAfter = %v, want 5s", result.RequeueAfter)
		}
	})

	t.Run("template change creates a new revision and rolls it out to the deployment", func(t *testing.T) {
		lws := lwsStatusHashLWS(2)
		reconciler, k8sClient := lwsStatusNewReconciler(t, lws)

		if _, err := reconciler.reconcileHash(ctx, lws); err != nil {
			t.Fatalf("reconcileHash() first call unexpected error: %v", err)
		}
		key := types.NamespacedName{Name: lws.Name, Namespace: lws.Namespace}
		var deploy appsv1.Deployment
		if err := k8sClient.Get(ctx, key, &deploy); err != nil {
			t.Fatalf("leader deployment was not created: %v", err)
		}
		firstRevisionKey := revisionutils.GetRevisionKey(&deploy)

		lws.Spec.LeaderWorkerTemplate.LeaderTemplate.Spec.Containers[0].Image = "nginx:updated"
		if _, err := reconciler.reconcileHash(ctx, lws); err != nil {
			t.Fatalf("reconcileHash() second call unexpected error: %v", err)
		}

		if err := k8sClient.Get(ctx, key, &deploy); err != nil {
			t.Fatalf("reading back the leader deployment: %v", err)
		}
		secondRevisionKey := revisionutils.GetRevisionKey(&deploy)
		if secondRevisionKey == firstRevisionKey {
			t.Errorf("leader deployment revision key = %q, want it to change after the template update", secondRevisionKey)
		}
		if got := deploy.Spec.Template.Spec.Containers[0].Image; got != "nginx:updated" {
			t.Errorf("leader pod template image = %q, want %q", got, "nginx:updated")
		}

		var revisions appsv1.ControllerRevisionList
		if err := k8sClient.List(ctx, &revisions, client.InNamespace(lws.Namespace)); err != nil {
			t.Fatalf("listing revisions: %v", err)
		}
		if len(revisions.Items) != 2 {
			t.Errorf("got %d controller revisions, want 2 after a template update", len(revisions.Items))
		}
	})

	t.Run("rollout completes, stale revisions are truncated", func(t *testing.T) {
		lws := lwsStatusHashLWS(2)
		reconciler, k8sClient := lwsStatusNewReconciler(t, lws)

		if _, err := reconciler.reconcileHash(ctx, lws); err != nil {
			t.Fatalf("reconcileHash() first call unexpected error: %v", err)
		}
		lws.Spec.LeaderWorkerTemplate.LeaderTemplate.Spec.Containers[0].Image = "nginx:updated"
		if _, err := reconciler.reconcileHash(ctx, lws); err != nil {
			t.Fatalf("reconcileHash() second call unexpected error: %v", err)
		}

		// Report the deployment as fully rolled out so that the reconcile considers
		// the update done and garbage collects the superseded revision.
		key := types.NamespacedName{Name: lws.Name, Namespace: lws.Namespace}
		var deploy appsv1.Deployment
		if err := k8sClient.Get(ctx, key, &deploy); err != nil {
			t.Fatalf("reading back the leader deployment: %v", err)
		}
		deploy.Status = appsv1.DeploymentStatus{Replicas: 2, ReadyReplicas: 2, UpdatedReplicas: 2}
		if err := k8sClient.Status().Update(ctx, &deploy); err != nil {
			t.Fatalf("updating the leader deployment status: %v", err)
		}

		if _, err := reconciler.reconcileHash(ctx, lws); err != nil {
			t.Fatalf("reconcileHash() third call unexpected error: %v", err)
		}

		var revisions appsv1.ControllerRevisionList
		if err := k8sClient.List(ctx, &revisions, client.InNamespace(lws.Namespace)); err != nil {
			t.Fatalf("listing revisions: %v", err)
		}
		if len(revisions.Items) != 1 {
			t.Fatalf("got %d controller revisions, want only the current one", len(revisions.Items))
		}
		if err := k8sClient.Get(ctx, key, &deploy); err != nil {
			t.Fatalf("reading back the leader deployment: %v", err)
		}
		if got, want := revisionutils.GetRevisionKey(&revisions.Items[0]), revisionutils.GetRevisionKey(&deploy); got != want {
			t.Errorf("surviving revision key = %q, want the deployed one %q", got, want)
		}
	})

	t.Run("unique per replica subdomains skip the shared headless service", func(t *testing.T) {
		lws := lwsStatusHashLWS(2)
		lws.Spec.NetworkConfig = &leaderworkerset.NetworkConfig{
			SubdomainPolicy: ptr.To(leaderworkerset.SubdomainUniquePerReplica),
		}
		reconciler, k8sClient := lwsStatusNewReconciler(t, lws)

		if _, err := reconciler.reconcileHash(ctx, lws); err != nil {
			t.Fatalf("reconcileHash() unexpected error: %v", err)
		}

		var service corev1.Service
		err := k8sClient.Get(ctx, types.NamespacedName{Name: lws.Name, Namespace: lws.Namespace}, &service)
		if !apierrors.IsNotFound(err) {
			t.Errorf("getting the shared headless service returned %v, want NotFound", err)
		}
	})

	t.Run("fetching the leader deployment fails, the error is propagated", func(t *testing.T) {
		lws := lwsStatusHashLWS(2)
		reconciler, _ := lwsStatusNewReconcilerWithInterceptor(t, interceptor.Funcs{
			Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
				return apierrors.NewInternalError(errors.New("boom"))
			},
		}, lws)

		if _, err := reconciler.reconcileHash(ctx, lws); err == nil {
			t.Fatal("reconcileHash() error = nil, want the get error")
		}
	})

	t.Run("applying the leader deployment fails, the error is propagated", func(t *testing.T) {
		lws := lwsStatusHashLWS(2)
		reconciler, _ := lwsStatusNewReconcilerWithInterceptor(t, interceptor.Funcs{
			Patch: func(context.Context, client.WithWatch, client.Object, client.Patch, ...client.PatchOption) error {
				return apierrors.NewInternalError(errors.New("boom"))
			},
		}, lws)

		if _, err := reconciler.reconcileHash(ctx, lws); err == nil {
			t.Fatal("reconcileHash() error = nil, want the apply error")
		}
	})
}

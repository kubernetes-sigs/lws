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

package controllers

import (
	"context"
	"fmt"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	leaderworkerset "sigs.k8s.io/lws/api/leaderworkerset/v1"
	revisionutils "sigs.k8s.io/lws/pkg/utils/revision"
	"sigs.k8s.io/lws/test/wrappers"
)

func groupRevisionTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{corev1.AddToScheme, appsv1.AddToScheme, leaderworkerset.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	return scheme
}

// A rollout that changes size or subdomainPolicy must not change how the pod
// controller treats groups that still run the old revision.
func TestReconcilePodUsesGroupRevision(t *testing.T) {
	tests := []struct {
		name   string
		update func(*leaderworkerset.LeaderWorkerSet)
	}{
		{
			name: "size changed to 1",
			update: func(lws *leaderworkerset.LeaderWorkerSet) {
				lws.Spec.LeaderWorkerTemplate.Size = ptr.To[int32](1)
			},
		},
		{
			name: "subdomainPolicy changed to UniquePerReplica",
			update: func(lws *leaderworkerset.LeaderWorkerSet) {
				policy := leaderworkerset.SubdomainUniquePerReplica
				lws.Spec.NetworkConfig.SubdomainPolicy = &policy
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			scheme := groupRevisionTestScheme(t)
			lws := wrappers.BuildBasicLeaderWorkerSet("test-lws", "default").
				Size(2).
				SubdomainPolicy(leaderworkerset.SubdomainShared).
				Obj()
			lws.UID = "lws-uid"
			k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(lws).Build()

			// The old group was created from this revision.
			oldRevision, err := revisionutils.NewRevision(ctx, k8sClient, lws, "")
			if err != nil {
				t.Fatal(err)
			}
			if err := k8sClient.Create(ctx, oldRevision); err != nil {
				t.Fatal(err)
			}
			tc.update(lws)
			if err := k8sClient.Update(ctx, lws); err != nil {
				t.Fatal(err)
			}

			sharedService := &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name: lws.Name, Namespace: lws.Namespace,
					OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(lws, leaderworkerset.GroupVersion.WithKind("LeaderWorkerSet"))},
				},
			}
			leader := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-lws-0", Namespace: lws.Namespace, UID: "leader-uid",
					Labels: map[string]string{
						leaderworkerset.SetNameLabelKey:         lws.Name,
						leaderworkerset.WorkerIndexLabelKey:     "0",
						leaderworkerset.GroupIndexLabelKey:      "0",
						leaderworkerset.GroupUniqueHashLabelKey: "group-key",
						leaderworkerset.RevisionKey:             revisionutils.GetRevisionKey(oldRevision),
					},
				},
				Spec: corev1.PodSpec{Hostname: "test-lws-0", Subdomain: lws.Name},
			}
			for _, obj := range []client.Object{sharedService, leader} {
				if err := k8sClient.Create(ctx, obj); err != nil {
					t.Fatal(err)
				}
			}

			reconciler := PodReconciler{Client: k8sClient, Scheme: scheme, Record: fakeEventRecorder{}}
			if _, err := reconciler.reconcilePod(ctx, podReconcileRequestForPod(leader, false)); err != nil {
				t.Fatalf("reconcilePod() error = %v", err)
			}

			var workers appsv1.StatefulSet
			if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(leader), &workers); err != nil {
				t.Fatalf("worker StatefulSet for the old group: %v", err)
			}
			if got := *workers.Spec.Replicas; got != 1 {
				t.Errorf("worker replicas = %d, want 1 (old group size 2)", got)
			}
		})
	}
}

// With RecreateGroupAfterStart, an old group must not look "still starting"
// just because a rollout changed size.
func TestHandleRestartPolicyUsesGroupSize(t *testing.T) {
	// The LWS now has size 2, but the old group was created with size 4.
	lws := wrappers.BuildLeaderWorkerSet("default").Replica(1).Size(2).RestartPolicy(leaderworkerset.RecreateGroupAfterStart).Obj()
	revisionKey := "old-revision"

	leader := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: lws.Name + "-0", Namespace: lws.Namespace, UID: "leader-uid",
			Labels: map[string]string{
				leaderworkerset.SetNameLabelKey:     lws.Name,
				leaderworkerset.WorkerIndexLabelKey: "0",
				leaderworkerset.GroupIndexLabelKey:  "0",
				leaderworkerset.RevisionKey:         revisionKey,
			},
			Annotations: map[string]string{leaderworkerset.SizeAnnotationKey: "4"},
		},
	}
	workerSts := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name: leader.Name, Namespace: leader.Namespace, UID: "sts-uid",
			OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(leader, corev1.SchemeGroupVersion.WithKind("Pod"))},
		},
	}
	objects := []client.Object{lws.DeepCopy(), leader, workerSts}
	var workers []*corev1.Pod
	for i := 1; i < 4; i++ {
		worker := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name: fmt.Sprintf("%s-%d", leader.Name, i), Namespace: lws.Namespace,
				Labels: map[string]string{
					leaderworkerset.SetNameLabelKey:     lws.Name,
					leaderworkerset.WorkerIndexLabelKey: fmt.Sprint(i),
					leaderworkerset.GroupIndexLabelKey:  "0",
					leaderworkerset.RevisionKey:         revisionKey,
				},
				Annotations:     map[string]string{leaderworkerset.SizeAnnotationKey: "4"},
				OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(workerSts, appsv1.SchemeGroupVersion.WithKind("StatefulSet"))},
			},
			Status: corev1.PodStatus{Phase: corev1.PodRunning},
		}
		workers = append(workers, worker)
		objects = append(objects, worker)
	}

	fakeClient := fake.NewClientBuilder().WithScheme(groupRevisionTestScheme(t)).WithObjects(objects...).Build()
	reconciler := PodReconciler{Client: fakeClient, Record: fakeEventRecorder{}}

	deleting := workers[0].DeepCopy()
	now := metav1.Now()
	deleting.DeletionTimestamp = &now
	leaderDeleted, err := reconciler.handleRestartPolicy(context.Background(), *deleting, *lws.DeepCopy())
	if err != nil {
		t.Fatalf("handleRestartPolicy() error = %v", err)
	}
	if !leaderDeleted {
		t.Fatal("handleRestartPolicy() did not recreate the old group")
	}
	if err := fakeClient.Get(context.Background(), client.ObjectKeyFromObject(leader), &corev1.Pod{}); !apierrors.IsNotFound(err) {
		t.Fatalf("leader pod still exists, err = %v", err)
	}
}

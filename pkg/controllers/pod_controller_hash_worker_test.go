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
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	leaderworkerset "sigs.k8s.io/lws/api/leaderworkerset/v1"
	revisionutils "sigs.k8s.io/lws/pkg/utils/revision"
	"sigs.k8s.io/lws/test/wrappers"
)

// Hash workers are named after the leader's host name, so admission can
// compute worker addresses before the leader pod is named.
func TestReconcilePodNamesHashWorkersAfterHostname(t *testing.T) {
	const (
		leaderName = "test-lws-7d9f8b6c4-x2kkp"
		hostname   = "test-lws-9f2ac71b"
	)
	workerSts := func(name string, owner *corev1.Pod) *appsv1.StatefulSet {
		return &appsv1.StatefulSet{
			ObjectMeta: metav1.ObjectMeta{
				Name: name, Namespace: "default",
				OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(owner, corev1.SchemeGroupVersion.WithKind("Pod"))},
			},
			Spec: appsv1.StatefulSetSpec{Replicas: ptr.To[int32](1)},
		}
	}
	otherLeader := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "test-lws-7d9f8b6c4-abcde", Namespace: "default", UID: "other-uid"}}

	tests := []struct {
		name        string
		existing    func(leader *corev1.Pod) []client.Object
		wantErr     bool
		wantSts     string
		wantMissing string
	}{
		{
			name:        "new group",
			wantSts:     hostname,
			wantMissing: leaderName,
		},
		{
			name: "group created before v0.12 keeps its worker statefulset",
			existing: func(leader *corev1.Pod) []client.Object {
				return []client.Object{workerSts(leaderName, leader)}
			},
			wantSts:     leaderName,
			wantMissing: hostname,
		},
		{
			name: "host name taken by another group",
			existing: func(*corev1.Pod) []client.Object {
				return []client.Object{workerSts(hostname, otherLeader)}
			},
			wantErr: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			scheme := groupRevisionTestScheme(t)
			lws := wrappers.BuildBasicLeaderWorkerSet("test-lws", "default").Size(2).Obj()
			lws.Spec.GroupIdentity = leaderworkerset.GroupIdentityHash
			lws.UID = "lws-uid"
			k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(lws).WithStatusSubresource(&corev1.Pod{}).Build()
			revision, err := revisionutils.NewRevision(ctx, k8sClient, lws, "")
			if err != nil {
				t.Fatal(err)
			}
			if err := k8sClient.Create(ctx, revision); err != nil {
				t.Fatal(err)
			}
			leader := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name: leaderName, Namespace: "default", UID: "leader-uid",
					Labels: map[string]string{
						leaderworkerset.SetNameLabelKey:         lws.Name,
						leaderworkerset.WorkerIndexLabelKey:     "0",
						leaderworkerset.GroupIndexLabelKey:      "group-key",
						leaderworkerset.GroupUniqueHashLabelKey: "group-key",
						leaderworkerset.RevisionKey:             revisionutils.GetRevisionKey(revision),
					},
				},
				Spec: corev1.PodSpec{Hostname: hostname, Subdomain: lws.Name},
			}
			objs := []client.Object{leader}
			if tc.existing != nil {
				objs = append(objs, tc.existing(leader)...)
			}
			for _, obj := range objs {
				if err := k8sClient.Create(ctx, obj); err != nil {
					t.Fatal(err)
				}
			}

			reconciler := PodReconciler{Client: k8sClient, Scheme: scheme, Record: fakeEventRecorder{}}
			_, err = reconciler.reconcilePod(ctx, podReconcileRequestForPod(leader, false))
			if tc.wantErr {
				if err == nil {
					t.Fatal("reconcilePod() error = nil, want an ownership error")
				}
				return
			}
			if err != nil {
				t.Fatalf("reconcilePod() error = %v", err)
			}
			var sts appsv1.StatefulSet
			if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: "default", Name: tc.wantSts}, &sts); err != nil {
				t.Fatalf("worker statefulset %s: %v", tc.wantSts, err)
			}
			if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: "default", Name: tc.wantMissing}, &sts); !apierrors.IsNotFound(err) {
				t.Fatalf("worker statefulset %s should not exist, err = %v", tc.wantMissing, err)
			}
		})
	}
}

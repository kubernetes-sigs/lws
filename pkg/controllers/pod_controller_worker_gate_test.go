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

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	leaderworkerset "sigs.k8s.io/lws/api/leaderworkerset/v1"
)

// podCtrlRoutineWorkerPod builds a worker pod that triggered neither a
// container restart nor a deletion, so reconciliation only has to decide
// whether its group has a recreation pending on its leader.
func podCtrlRoutineWorkerPod(lws *leaderworkerset.LeaderWorkerSet, name string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: lws.Namespace,
			UID:       types.UID(name),
			Labels: map[string]string{
				leaderworkerset.SetNameLabelKey:     lws.Name,
				leaderworkerset.WorkerIndexLabelKey: "1",
				leaderworkerset.GroupIndexLabelKey:  "0",
			},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
}

// TestPodCtrlRoutineWorkerSurfacesLeaderReadError covers the leader lookup that
// gates routine worker reconciliation. A missing leader ends the reconcile,
// while any other error has to be returned so the request is retried instead of
// being reported as a successful no-op.
func TestPodCtrlRoutineWorkerSurfacesLeaderReadError(t *testing.T) {
	lws := podCtrlBasicLWS()
	const leaderName = "test-lws-0"
	worker := podCtrlRoutineWorkerPod(lws, leaderName+"-1")
	transientErr := apierrors.NewTimeoutError("etcd is unavailable", 1)

	tests := []struct {
		name string
		// leaderExists stores a leader pod, optionally carrying a pending
		// group recreation.
		leaderExists        bool
		recreatePending     bool
		failLeaderRead      bool
		wantErr             error
		wantRequeueDisabled bool
	}{
		{
			name:           "transient leader read error is retried",
			failLeaderRead: true,
			wantErr:        transientErr,
		},
		{
			name: "missing leader ends the reconcile",
		},
		{
			name:         "leader without a pending recreation ends the reconcile",
			leaderExists: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			scheme := podCtrlTestScheme(t)
			objs := []client.Object{lws.DeepCopy(), worker.DeepCopy()}
			if tc.leaderExists {
				leader := podCtrlLeaderPod(lws, leaderName)
				if tc.recreatePending {
					leader.Annotations = map[string]string{
						leaderworkerset.GroupRecreatePendingAnnotationKey: metav1.Now().Format("2006-01-02T15:04:05Z07:00"),
					}
				}
				objs = append(objs, leader)
			}

			builder := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...)
			if tc.failLeaderRead {
				leaderKey := types.NamespacedName{Name: leaderName, Namespace: lws.Namespace}
				builder = builder.WithInterceptorFuncs(interceptor.Funcs{
					Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
						if _, isPod := obj.(*corev1.Pod); isPod && key == leaderKey {
							return transientErr
						}
						return c.Get(ctx, key, obj, opts...)
					},
				})
			}
			c := builder.Build()
			reconciler := NewPodReconciler(c, scheme, events.NewFakeRecorder(4), nil)

			got, err := reconciler.reconcilePod(context.Background(), podReconcileRequestForPod(worker, false))
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("reconcilePod() error = %v, want %v", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("reconcilePod() error = %v, want nil", err)
			}
			if got != (ctrl.Result{}) {
				t.Fatalf("reconcilePod() = %+v, want an empty result", got)
			}
		})
	}
}

// TestPodCtrlRoutineWorkerIgnoresMissingLeaderKind guards the NotFound branch
// against a scheme change: a NotFound for the leader must stay a successful
// reconcile rather than a returned error.
func TestPodCtrlRoutineWorkerIgnoresMissingLeaderKind(t *testing.T) {
	lws := podCtrlBasicLWS()
	worker := podCtrlRoutineWorkerPod(lws, "test-lws-0-1")
	scheme := podCtrlTestScheme(t)

	notFound := apierrors.NewNotFound(schema.GroupResource{Resource: "pods"}, "test-lws-0")
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(lws.DeepCopy(), worker.DeepCopy()).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if _, isPod := obj.(*corev1.Pod); isPod && key.Name == "test-lws-0" {
					return notFound
				}
				return c.Get(ctx, key, obj, opts...)
			},
		}).
		Build()
	reconciler := NewPodReconciler(c, scheme, events.NewFakeRecorder(4), nil)

	if _, err := reconciler.reconcilePod(context.Background(), podReconcileRequestForPod(worker, false)); err != nil {
		t.Fatalf("reconcilePod() error = %v, want nil for a missing leader", err)
	}
}

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
	"fmt"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	leaderworkerset "sigs.k8s.io/lws/api/leaderworkerset/v1"
)

// enforceDeleteUIDPrecondition makes the fake client honor a UID delete
// precondition the way the API server does. The fake client only checks the
// resourceVersion precondition.
func enforceDeleteUIDPrecondition() interceptor.Funcs {
	return interceptor.Funcs{
		Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			deleteOpts := &client.DeleteOptions{}
			deleteOpts.ApplyOptions(opts)
			if p := deleteOpts.Preconditions; p != nil && p.UID != nil {
				stored := obj.DeepCopyObject().(client.Object)
				if err := c.Get(ctx, client.ObjectKeyFromObject(obj), stored); err != nil {
					return err
				}
				if stored.GetUID() != *p.UID {
					return apierrors.NewConflict(corev1.Resource("pods"), obj.GetName(),
						fmt.Errorf("precondition failed: UID in precondition: %s, UID in object meta: %s", *p.UID, stored.GetUID()))
				}
			}
			return c.Delete(ctx, obj, opts...)
		},
	}
}

// TestTerminateExhaustedGroupOnlyActsOnObservedLeader covers the leader read
// from the cache while the API server already holds a different Pod of the
// same name, as an ordinal leader can be replaced in between. The replacement
// may run a newer revision with its own restart budget, so it must be neither
// marked exhausted nor deleted on behalf of the group it replaced.
func TestTerminateExhaustedGroupOnlyActsOnObservedLeader(t *testing.T) {
	lws := podCtrlBasicLWS()
	const leaderName = "test-lws-0"

	newLeader := func(uid, revision string, exhausted bool) *corev1.Pod {
		leader := podCtrlLeaderPod(lws, leaderName)
		leader.UID = types.UID(uid)
		leader.Labels[leaderworkerset.RevisionKey] = revision
		if exhausted {
			leader.Annotations = map[string]string{leaderworkerset.GroupRestartBudgetExhaustedAnnotationKey: "true"}
			controllerutil.AddFinalizer(leader, leaderworkerset.GroupRestartBudgetCleanupFinalizer)
		}
		return leader
	}

	tests := []struct {
		name string
		// observed is the leader the reconciler read from its cache.
		observed *corev1.Pod
		// replacement, when set, is the same-name Pod the API server holds
		// instead of observed. With neither set the leader is already gone.
		replacement *corev1.Pod
		// leaderStored stores observed itself, the up-to-date case.
		leaderStored bool

		wantDeleted       bool
		wantConflict      bool
		wantLeaderGone    bool
		wantEventContains string
	}{
		{
			name:              "an up-to-date leader is marked exhausted and deleted",
			observed:          newLeader("leader-uid", "rev-1", false),
			leaderStored:      true,
			wantDeleted:       true,
			wantLeaderGone:    true,
			wantEventContains: "ReplicaRestartBudgetExceeded",
		},
		{
			name:         "a replacement on a newer revision is not marked exhausted",
			observed:     newLeader("old-uid", "rev-1", false),
			replacement:  newLeader("new-uid", "rev-2", false),
			wantConflict: true,
		},
		{
			name:        "a replacement is not deleted for an already exhausted leader",
			observed:    newLeader("old-uid", "rev-1", true),
			replacement: newLeader("new-uid", "rev-2", false),
			wantDeleted: true,
		},
		{
			// The replacement shares the revision label, so listing the group
			// by name, index, and revision returns it. It lacks the exhausted
			// annotation, so a finalizer added here would never be released.
			name:        "a same-revision replacement is left alone for an already exhausted leader",
			observed:    newLeader("old-uid", "rev-1", true),
			replacement: newLeader("new-uid", "rev-1", false),
			wantDeleted: true,
		},
		{
			name:           "a leader that is already gone ends the group",
			observed:       newLeader("leader-uid", "rev-1", true),
			wantDeleted:    true,
			wantLeaderGone: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			scheme := podCtrlTestScheme(t)
			c := fake.NewClientBuilder().
				WithScheme(scheme).
				WithObjects(lws.DeepCopy()).
				WithInterceptorFuncs(enforceDeleteUIDPrecondition()).
				Build()

			// Read the observed leader back so it carries the resourceVersion a
			// cached copy would, then swap in the replacement under the same name.
			observed := tc.observed.DeepCopy()
			if err := c.Create(ctx, observed); err != nil {
				t.Fatalf("creating leader: %v", err)
			}
			if !tc.leaderStored {
				stored := observed.DeepCopy()
				stored.Finalizers = nil
				if err := c.Update(ctx, stored); err != nil {
					t.Fatalf("releasing leader finalizers: %v", err)
				}
				if err := c.Delete(ctx, stored); err != nil {
					t.Fatalf("deleting leader: %v", err)
				}
			}
			if tc.replacement != nil {
				replacement := tc.replacement.DeepCopy()
				if err := c.Create(ctx, replacement); err != nil {
					t.Fatalf("creating replacement leader: %v", err)
				}
				// The fake client restarts resourceVersion at 1 for every new
				// object, while the API server draws it from a global counter, so
				// a replacement never shares the version of the Pod it replaced.
				// Write the replacement once more to model that.
				replacement.Labels["touched"] = "true"
				if err := c.Update(ctx, replacement); err != nil {
					t.Fatalf("advancing replacement resourceVersion: %v", err)
				}
			}

			recorder := events.NewFakeRecorder(4)
			reconciler := NewPodReconciler(c, scheme, recorder, nil)
			deleted, err := reconciler.terminateExhaustedGroup(ctx, lws, observed)
			switch {
			case tc.wantConflict:
				if !apierrors.IsConflict(err) {
					t.Fatalf("terminateExhaustedGroup() error = %v, want a conflict", err)
				}
			case err != nil:
				t.Fatalf("terminateExhaustedGroup() error = %v, want nil", err)
			}
			if deleted != tc.wantDeleted {
				t.Errorf("terminateExhaustedGroup() deleted = %v, want %v", deleted, tc.wantDeleted)
			}

			var live corev1.Pod
			getErr := c.Get(ctx, types.NamespacedName{Name: leaderName, Namespace: lws.Namespace}, &live)
			switch {
			case tc.wantLeaderGone:
				// Foreground deletion keeps the finalized leader around with a
				// deletion timestamp; either way it is no longer a live leader.
				if getErr == nil && live.DeletionTimestamp == nil {
					t.Errorf("leader %s/%s is still live, want it deleted", live.Namespace, live.Name)
				}
			case tc.replacement != nil:
				if getErr != nil {
					t.Fatalf("reading replacement leader: %v", getErr)
				}
				if live.UID != tc.replacement.UID {
					t.Fatalf("leader UID = %s, want the replacement %s", live.UID, tc.replacement.UID)
				}
				if live.DeletionTimestamp != nil {
					t.Error("replacement leader was deleted on behalf of the previous group")
				}
				if live.Annotations[leaderworkerset.GroupRestartBudgetExhaustedAnnotationKey] != "" {
					t.Error("replacement leader was marked exhausted on behalf of the previous group")
				}
				if controllerutil.ContainsFinalizer(&live, leaderworkerset.GroupRestartBudgetCleanupFinalizer) {
					t.Error("replacement leader was given the restart budget finalizer")
				}
			}

			gotEvents := podCtrlDrainEvents(recorder)
			if tc.wantEventContains == "" {
				if len(gotEvents) != 0 {
					t.Errorf("recorded events = %v, want none", gotEvents)
				}
				return
			}
			if len(gotEvents) != 1 || !strings.Contains(gotEvents[0], tc.wantEventContains) {
				t.Errorf("recorded events = %v, want exactly one containing %q", gotEvents, tc.wantEventContains)
			}
		})
	}
}

// TestTerminateExhaustedGroupReturnsDeleteError keeps errors other than a
// missing or replaced leader visible to the caller so the request is retried.
func TestTerminateExhaustedGroupReturnsDeleteError(t *testing.T) {
	ctx := context.Background()
	lws := podCtrlBasicLWS()
	scheme := podCtrlTestScheme(t)
	wantErr := apierrors.NewTimeoutError("etcd is unavailable", 1)

	leader := podCtrlLeaderPod(lws, "test-lws-0")
	leader.Annotations = map[string]string{leaderworkerset.GroupRestartBudgetExhaustedAnnotationKey: "true"}
	controllerutil.AddFinalizer(leader, leaderworkerset.GroupRestartBudgetCleanupFinalizer)
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(lws.DeepCopy(), leader).
		WithInterceptorFuncs(interceptor.Funcs{
			Delete: func(context.Context, client.WithWatch, client.Object, ...client.DeleteOption) error {
				return wantErr
			},
		}).
		Build()
	var observed corev1.Pod
	if err := c.Get(ctx, client.ObjectKeyFromObject(leader), &observed); err != nil {
		t.Fatalf("reading leader: %v", err)
	}

	reconciler := NewPodReconciler(c, scheme, events.NewFakeRecorder(4), nil)
	deleted, err := reconciler.terminateExhaustedGroup(ctx, lws, &observed)
	if !errors.Is(err, wantErr) {
		t.Fatalf("terminateExhaustedGroup() error = %v, want %v", err, wantErr)
	}
	if deleted {
		t.Error("terminateExhaustedGroup() deleted = true, want false when the delete failed")
	}
}

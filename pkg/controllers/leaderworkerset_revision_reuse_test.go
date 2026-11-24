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
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	appsapplyv1 "k8s.io/client-go/applyconfigurations/apps/v1"
	"k8s.io/client-go/tools/events"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	leaderworkerset "sigs.k8s.io/lws/api/leaderworkerset/v1"
	revisionutils "sigs.k8s.io/lws/pkg/utils/revision"
	"sigs.k8s.io/lws/test/wrappers"
)

// TestCreateUpdatedRevisionOnRollback rolls a LeaderWorkerSet from template A to B and back to A while the revision of A is still kept, as it is until a rolling update completes.
func TestCreateUpdatedRevisionOnRollback(t *testing.T) {
	tests := []struct {
		name string
		// legacyA drops the placement marker from the revision of A, like a revision created before revisions recorded placement.
		legacyA bool
		// otherDataUnderKeyA stores the data of another template under the revision key of A.
		otherDataUnderKeyA bool
		wantReuse          bool
		wantRevisions      int
		// wantUnderKeyA is the number of revisions under the key of A after the rollback and after truncating to that key.
		wantUnderKeyA int
	}{
		{name: "the revision of the previous template is reused", wantReuse: true, wantRevisions: 2, wantUnderKeyA: 1},
		{name: "a revision that does not record placement is replaced", legacyA: true, wantReuse: false, wantRevisions: 2, wantUnderKeyA: 1},
		{name: "a revision with other data under the same key is not reused", otherDataUnderKeyA: true, wantReuse: false, wantRevisions: 3, wantUnderKeyA: 2},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			scheme := invalidTemplateTestScheme(t)
			lws := wrappers.BuildLeaderWorkerSet("default").Obj()
			lws.UID = types.UID("lws-uid")
			c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(lws).Build()
			recorder := events.NewFakeRecorder(16)
			r := NewLeaderWorkerSetReconciler(c, scheme, recorder, nil)

			originalName := lws.Spec.LeaderWorkerTemplate.WorkerTemplate.Spec.Containers[0].Name
			var revisionA *appsv1.ControllerRevision
			if tc.otherDataUnderKeyA {
				candidateA, err := revisionutils.NewRevision(ctx, c, lws, "")
				if err != nil {
					t.Fatalf("NewRevision() error = %v", err)
				}
				other := lws.DeepCopy()
				other.Spec.LeaderWorkerTemplate.WorkerTemplate.Spec.Containers[0].Name = "other-worker"
				revisionA = createTestRevisionWithKey(t, ctx, c, other, revisionutils.GetRevisionKey(candidateA))
			} else {
				revisionA = createTestRevision(t, ctx, c, lws)
			}
			if tc.legacyA {
				delete(revisionA.Annotations, "leaderworkerset.sigs.k8s.io/placement-recorded")
				if err := c.Update(ctx, revisionA); err != nil {
					t.Fatalf("dropping the placement marker: %v", err)
				}
			}
			keyA := revisionutils.GetRevisionKey(revisionA)
			lws.Spec.LeaderWorkerTemplate.WorkerTemplate.Spec.Containers[0].Name = "new-worker"
			revisionB := createTestRevision(t, ctx, c, lws)
			if keyA == revisionutils.GetRevisionKey(revisionB) {
				t.Fatal("templates A and B must have different revision keys")
			}

			// Roll back to A.
			lws.Spec.LeaderWorkerTemplate.WorkerTemplate.Spec.Containers[0].Name = originalName
			updated, err := r.getUpdatedRevision(ctx, lws, revisionB)
			if err != nil || updated == nil {
				t.Fatalf("getUpdatedRevision() = %v, %v, want a revision for template A", updated, err)
			}
			got, err := r.createUpdatedRevision(ctx, lws, updated)
			if err != nil {
				t.Fatalf("createUpdatedRevision() error = %v", err)
			}
			if revisionutils.GetRevisionKey(got) != keyA {
				t.Fatalf("revision key = %s, want %s", revisionutils.GetRevisionKey(got), keyA)
			}
			if reused := got.Name == revisionA.Name; reused != tc.wantReuse {
				t.Fatalf("reused revision %s = %v, want %v (got %s)", revisionA.Name, reused, tc.wantReuse, got.Name)
			}
			if !revisionutils.EqualRevision(got, updated) {
				t.Fatalf("revision %s does not hold the data of template A", got.Name)
			}
			if recorded, err := revisionutils.RecordsPlacement(got); err != nil || !recorded {
				t.Fatalf("RecordsPlacement(%s) = %v, %v, want true", got.Name, recorded, err)
			}
			// A CreatingRevision event is recorded only when a revision is created, not when one is reused.
			created := 0
			for len(recorder.Events) > 0 {
				if strings.Contains(<-recorder.Events, "Normal "+CreatingRevision) {
					created++
				}
			}
			if wantCreated := map[bool]int{true: 0, false: 1}[tc.wantReuse]; created != wantCreated {
				t.Fatalf("got %d %s events, want %d", created, CreatingRevision, wantCreated)
			}
			if n := countRevisions(t, ctx, c, lws, nil); n != tc.wantRevisions {
				t.Fatalf("got %d revisions, want %d", n, tc.wantRevisions)
			}
			if n := countRevisions(t, ctx, c, lws, map[string]string{leaderworkerset.RevisionKey: keyA}); n != tc.wantUnderKeyA {
				t.Fatalf("got %d revisions under key %s, want %d", n, keyA, tc.wantUnderKeyA)
			}
			// The rollback completes: truncation keeps every revision under the key of A.
			if err := revisionutils.TruncateRevisions(ctx, c, lws, keyA); err != nil {
				t.Fatalf("TruncateRevisions() error = %v", err)
			}
			if n := countRevisions(t, ctx, c, lws, nil); n != tc.wantUnderKeyA {
				t.Fatalf("got %d revisions after truncating to key %s, want %d", n, keyA, tc.wantUnderKeyA)
			}
		})
	}
}

// TestCreateUpdatedRevisionDeleteFailure fails the delete of a revision that does not record placement after its replacement exists.
func TestCreateUpdatedRevisionDeleteFailure(t *testing.T) {
	ctx := context.Background()
	scheme := invalidTemplateTestScheme(t)
	lws := wrappers.BuildLeaderWorkerSet("default").Obj()
	lws.UID = types.UID("lws-uid")
	failDelete := true
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(lws).WithInterceptorFuncs(interceptor.Funcs{
		Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			if failDelete {
				return apierrors.NewServiceUnavailable("delete failed")
			}
			return c.Delete(ctx, obj, opts...)
		},
	}).Build()
	r := NewLeaderWorkerSetReconciler(c, scheme, events.NewFakeRecorder(16), nil)

	revisionA := createTestRevision(t, ctx, c, lws)
	delete(revisionA.Annotations, "leaderworkerset.sigs.k8s.io/placement-recorded")
	if err := c.Update(ctx, revisionA); err != nil {
		t.Fatalf("dropping the placement marker: %v", err)
	}
	keyA := revisionutils.GetRevisionKey(revisionA)
	originalName := lws.Spec.LeaderWorkerTemplate.WorkerTemplate.Spec.Containers[0].Name
	lws.Spec.LeaderWorkerTemplate.WorkerTemplate.Spec.Containers[0].Name = "new-worker"
	revisionB := createTestRevision(t, ctx, c, lws)
	lws.Spec.LeaderWorkerTemplate.WorkerTemplate.Spec.Containers[0].Name = originalName

	updated, err := r.getUpdatedRevision(ctx, lws, revisionB)
	if err != nil || updated == nil {
		t.Fatalf("getUpdatedRevision() = %v, %v, want a revision for template A", updated, err)
	}
	if _, err := r.createUpdatedRevision(ctx, lws, updated); !apierrors.IsServiceUnavailable(err) {
		t.Fatalf("createUpdatedRevision() error = %v, want the delete error", err)
	}

	// The next reconcile reuses the revision that records placement and creates no third revision under the key.
	// The revision without the marker stays, as it does when recordPlacementInRevision fails to delete one.
	failDelete = false
	updated, err = r.getUpdatedRevision(ctx, lws, revisionB)
	if err != nil || updated == nil {
		t.Fatalf("getUpdatedRevision() = %v, %v, want a revision for template A", updated, err)
	}
	got, err := r.createUpdatedRevision(ctx, lws, updated)
	if err != nil {
		t.Fatalf("createUpdatedRevision() error = %v", err)
	}
	if got.Name == revisionA.Name {
		t.Fatalf("createUpdatedRevision() returned %s, which does not record placement", got.Name)
	}
	if n := countRevisions(t, ctx, c, lws, map[string]string{leaderworkerset.RevisionKey: keyA}); n != 2 {
		t.Fatalf("got %d revisions under key %s, want 2", n, keyA)
	}
}

// TestReconcileReusesRevisionOnLeaderApplyRetry exercises the full Reconcile / reconcileHash
// paths when initial leader workload creation fails and later succeeds without template changes.
func TestReconcileReusesRevisionOnLeaderApplyRetry(t *testing.T) {
	tests := []struct {
		name          string
		groupIdentity leaderworkerset.GroupIdentityType
	}{
		{name: "ordinal mode", groupIdentity: leaderworkerset.GroupIdentityOrdinal},
		{name: "hash mode", groupIdentity: leaderworkerset.GroupIdentityHash},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			scheme := invalidTemplateTestScheme(t)
			lws := leaderOnlyTestLWS()
			lws.UID = types.UID("lws-uid")
			lws.Spec.GroupIdentity = tc.groupIdentity

			failApply := true
			c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(lws).WithStatusSubresource(lws).
				WithInterceptorFuncs(interceptor.Funcs{
					Apply: func(ctx context.Context, cl client.WithWatch, obj runtime.ApplyConfiguration, opts ...client.ApplyOption) error {
						if !failApply {
							return cl.Apply(ctx, obj, opts...)
						}
						switch applied := obj.(type) {
						case *appsapplyv1.StatefulSetApplyConfiguration:
							if tc.groupIdentity == leaderworkerset.GroupIdentityOrdinal && ptr.Deref(applied.GetName(), "") != workerTemplateProbeName {
								return apierrors.NewServiceUnavailable("leader statefulset create failed")
							}
						case *appsapplyv1.DeploymentApplyConfiguration:
							if tc.groupIdentity == leaderworkerset.GroupIdentityHash {
								return apierrors.NewServiceUnavailable("leader deployment create failed")
							}
						}
						return cl.Apply(ctx, obj, opts...)
					},
				}).Build()
			recorder := events.NewFakeRecorder(32)
			r := NewLeaderWorkerSetReconciler(c, scheme, recorder, nil)

			drainCreatedEvents := func() int {
				created := 0
				for len(recorder.Events) > 0 {
					if strings.Contains(<-recorder.Events, "Normal "+CreatingRevision) {
						created++
					}
				}
				return created
			}

			// First reconcile creates the revision, then fails applying the leader workload.
			if err := reconcileLWS(t, r, lws); !apierrors.IsServiceUnavailable(err) {
				t.Fatalf("first Reconcile() error = %v, want ServiceUnavailable", err)
			}
			var revisions appsv1.ControllerRevisionList
			if err := c.List(ctx, &revisions, client.MatchingLabels{leaderworkerset.SetNameLabelKey: lws.Name}); err != nil {
				t.Fatalf("listing revisions after first failure: %v", err)
			}
			if len(revisions.Items) != 1 {
				t.Fatalf("after first failure: got %d revisions, want 1", len(revisions.Items))
			}
			firstRevisionName := revisions.Items[0].Name
			if got := drainCreatedEvents(); got != 1 {
				t.Fatalf("after first failure: got %d %s events, want 1", got, CreatingRevision)
			}

			// Second reconcile still fails applying the leader workload and must reuse the revision.
			if err := reconcileLWS(t, r, lws); !apierrors.IsServiceUnavailable(err) {
				t.Fatalf("second Reconcile() error = %v, want ServiceUnavailable", err)
			}
			revisions = appsv1.ControllerRevisionList{}
			if err := c.List(ctx, &revisions, client.MatchingLabels{leaderworkerset.SetNameLabelKey: lws.Name}); err != nil {
				t.Fatalf("listing revisions after second failure: %v", err)
			}
			if len(revisions.Items) != 1 || revisions.Items[0].Name != firstRevisionName {
				t.Fatalf("after second failure: got %d revisions (%v), want only %s", len(revisions.Items), revisions.Items, firstRevisionName)
			}
			if got := drainCreatedEvents(); got != 0 {
				t.Fatalf("after second failure: got %d %s events, want 0", got, CreatingRevision)
			}

			// Third reconcile succeeds in creating the leader workload, still keeping a single revision.
			failApply = false
			if err := reconcileLWS(t, r, lws); err != nil {
				t.Fatalf("third Reconcile() error = %v, want nil", err)
			}
			revisions = appsv1.ControllerRevisionList{}
			if err := c.List(ctx, &revisions, client.MatchingLabels{leaderworkerset.SetNameLabelKey: lws.Name}); err != nil {
				t.Fatalf("listing revisions after recovery: %v", err)
			}
			if len(revisions.Items) != 1 || revisions.Items[0].Name != firstRevisionName {
				t.Fatalf("after recovery: got %d revisions (%v), want only %s", len(revisions.Items), revisions.Items, firstRevisionName)
			}
			if got := drainCreatedEvents(); got != 0 {
				t.Fatalf("after recovery: got %d %s events, want 0", got, CreatingRevision)
			}

			wantKey := revisionutils.GetRevisionKey(&revisions.Items[0])

			if tc.groupIdentity == leaderworkerset.GroupIdentityOrdinal {
				var leaderSts appsv1.StatefulSet
				if err := c.Get(ctx, types.NamespacedName{Name: lws.Name, Namespace: lws.Namespace}, &leaderSts); err != nil {
					t.Fatalf("getting leader StatefulSet: %v", err)
				}
				if gotKey := revisionutils.GetRevisionKey(&leaderSts); gotKey != wantKey {
					t.Fatalf("leader StatefulSet revision key = %s, want %s", gotKey, wantKey)
				}
			} else {
				var leaderDeploy appsv1.Deployment
				if err := c.Get(ctx, types.NamespacedName{Name: lws.Name, Namespace: lws.Namespace}, &leaderDeploy); err != nil {
					t.Fatalf("getting leader Deployment: %v", err)
				}
				if gotKey := revisionutils.GetRevisionKey(&leaderDeploy); gotKey != wantKey {
					t.Fatalf("leader Deployment revision key = %s, want %s", gotKey, wantKey)
				}
			}
		})
	}
}

func createTestRevision(t *testing.T, ctx context.Context, c client.Client, lws *leaderworkerset.LeaderWorkerSet) *appsv1.ControllerRevision {
	t.Helper()
	return createTestRevisionWithKey(t, ctx, c, lws, "")
}

func createTestRevisionWithKey(t *testing.T, ctx context.Context, c client.Client, lws *leaderworkerset.LeaderWorkerSet, revisionKey string) *appsv1.ControllerRevision {
	t.Helper()
	revision, err := revisionutils.NewRevision(ctx, c, lws, revisionKey)
	if err != nil {
		t.Fatalf("NewRevision() error = %v", err)
	}
	revision, err = revisionutils.CreateRevision(ctx, c, revision)
	if err != nil {
		t.Fatalf("CreateRevision() error = %v", err)
	}
	return revision
}

func countRevisions(t *testing.T, ctx context.Context, c client.Client, lws *leaderworkerset.LeaderWorkerSet, extraLabels map[string]string) int {
	t.Helper()
	selector := client.MatchingLabels{leaderworkerset.SetNameLabelKey: lws.Name}
	for k, v := range extraLabels {
		selector[k] = v
	}
	var revisions appsv1.ControllerRevisionList
	if err := c.List(ctx, &revisions, selector); err != nil {
		t.Fatalf("listing revisions: %v", err)
	}
	return len(revisions.Items)
}

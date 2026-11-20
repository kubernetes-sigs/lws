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
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	leaderworkerset "sigs.k8s.io/lws/api/leaderworkerset/v1"
	revisionutils "sigs.k8s.io/lws/pkg/utils/revision"
	"sigs.k8s.io/lws/test/wrappers"
)

// TestCreateUpdatedRevisionOnRollback rolls a LeaderWorkerSet from template A to B and back to A while the revision of A is still kept, as it is until a rolling update completes.
func TestCreateUpdatedRevisionOnRollback(t *testing.T) {
	tests := []struct {
		name string
		// legacyA drops the placement marker from the revision of A, like a revision created before revisions recorded placement.
		legacyA       bool
		wantReuse     bool
		wantRevisions int
	}{
		{name: "the revision of the previous template is reused", wantReuse: true, wantRevisions: 2},
		{name: "a revision that does not record placement is not reused", legacyA: true, wantReuse: false, wantRevisions: 3},
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

			revisionA := createTestRevision(t, ctx, c, lws)
			if tc.legacyA {
				delete(revisionA.Annotations, "leaderworkerset.sigs.k8s.io/placement-recorded")
				if err := c.Update(ctx, revisionA); err != nil {
					t.Fatalf("dropping the placement marker: %v", err)
				}
			}
			originalName := lws.Spec.LeaderWorkerTemplate.WorkerTemplate.Spec.Containers[0].Name
			lws.Spec.LeaderWorkerTemplate.WorkerTemplate.Spec.Containers[0].Name = "new-worker"
			revisionB := createTestRevision(t, ctx, c, lws)
			if revisionutils.GetRevisionKey(revisionA) == revisionutils.GetRevisionKey(revisionB) {
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
			if revisionutils.GetRevisionKey(got) != revisionutils.GetRevisionKey(revisionA) {
				t.Fatalf("revision key = %s, want %s", revisionutils.GetRevisionKey(got), revisionutils.GetRevisionKey(revisionA))
			}
			if reused := got.Name == revisionA.Name; reused != tc.wantReuse {
				t.Fatalf("reused revision %s = %v, want %v (got %s)", revisionA.Name, reused, tc.wantReuse, got.Name)
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
			var revisions appsv1.ControllerRevisionList
			if err := c.List(ctx, &revisions, client.MatchingLabels{leaderworkerset.SetNameLabelKey: lws.Name}); err != nil {
				t.Fatalf("listing revisions: %v", err)
			}
			if len(revisions.Items) != tc.wantRevisions {
				t.Fatalf("got %d revisions, want %d", len(revisions.Items), tc.wantRevisions)
			}
		})
	}
}

func createTestRevision(t *testing.T, ctx context.Context, c client.Client, lws *leaderworkerset.LeaderWorkerSet) *appsv1.ControllerRevision {
	t.Helper()
	revision, err := revisionutils.NewRevision(ctx, c, lws, "")
	if err != nil {
		t.Fatalf("NewRevision() error = %v", err)
	}
	revision, err = revisionutils.CreateRevision(ctx, c, revision)
	if err != nil {
		t.Fatalf("CreateRevision() error = %v", err)
	}
	return revision
}

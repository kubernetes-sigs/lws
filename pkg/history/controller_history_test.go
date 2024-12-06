/*
Copyright 2023.

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
package history

import (
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	apps "k8s.io/api/apps/v1"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/lws/test/testutils"
)

var parentKind = apps.SchemeGroupVersion.WithKind("LeaderWorkerSet")

func TestFindEqualRevisions(t *testing.T) {
	lws1 := testutils.BuildLeaderWorkerSet("test-sample").Obj()
	lws2 := testutils.BuildLeaderWorkerSet("test-sample").LeaderTemplateSpec(testutils.MakeLeaderPodSpecWithTPUResource()).Obj()

	lws1Revision, err := NewControllerRevision(lws1, parentKind, lws1.Labels, testutils.RawLWSTemplate(lws1), 1, lws1.Status.CollisionCount)
	if err != nil {
		t.Fatal(err)
	}

	lws2Revision, err := NewControllerRevision(lws2, parentKind, lws2.Labels, testutils.RawLWSTemplate(lws2), 1, lws2.Status.CollisionCount)
	if err != nil {
		t.Fatal(err)
	}

	lws1.Spec.LeaderWorkerTemplate.LeaderTemplate.Spec.Containers[0].Name = "update-name"
	lws1Revision2, err := NewControllerRevision(lws1, parentKind, lws1.Labels, testutils.RawLWSTemplate(lws1), 1, lws1.Status.CollisionCount)
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name      string
		revision  *apps.ControllerRevision
		revisions []*apps.ControllerRevision
		want      map[string]bool
	}{
		{
			name:      "finds nothing with no matches",
			revision:  lws1Revision,
			revisions: []*apps.ControllerRevision{lws1Revision2, lws2Revision},
			want:      map[string]bool{},
		},
		{
			name:      "finds nothing when empty",
			revision:  lws1Revision,
			revisions: []*apps.ControllerRevision{},
			want:      map[string]bool{},
		},
		{
			name:      "finds equivalent",
			revision:  lws1Revision,
			revisions: []*apps.ControllerRevision{lws1Revision, lws1Revision2, lws2Revision},
			want:      map[string]bool{lws1Revision.Name: true},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			revisions := FindEqualRevisions(tc.revisions, tc.revision)
			if len(revisions) != len(tc.want) {
				t.Errorf("want %d revisions, got %d revisions", len(tc.want), len(revisions))
			}
			for i := range revisions {
				if !tc.want[revisions[i].Name] {
					t.Errorf("Wanted: %s, got: %s", tc.revision.Name, revisions[i].Name)
				}
			}
		})
	}
}

func TestSortControllerRevisions(t *testing.T) {
	lws := testutils.BuildLeaderWorkerSet("test-sample").Obj()
	lwsRevision1, err := NewControllerRevision(lws, parentKind, lws.Labels, testutils.RawLWSTemplate(lws), 1, lws.Status.CollisionCount)
	if err != nil {
		t.Fatal(err)
	}
	lwsRevision2, err := NewControllerRevision(lws, parentKind, lws.Labels, testutils.RawLWSTemplate(lws), 2, lws.Status.CollisionCount)
	if err != nil {
		t.Fatal(err)
	}
	lwsRevision1Time2, err := NewControllerRevision(lws, parentKind, lws.Labels, testutils.RawLWSTemplate(lws), 1, lws.Status.CollisionCount)
	if err != nil {
		t.Fatal(err)
	}
	lwsRevision1Time2.CreationTimestamp = v1.Time{Time: lwsRevision1.CreationTimestamp.Add(time.Second)}

	tests := []struct {
		name      string
		revisions []*apps.ControllerRevision
		want      []*apps.ControllerRevision
	}{
		{
			name:      "already sorted",
			revisions: []*apps.ControllerRevision{lwsRevision1, lwsRevision2},
			want:      []*apps.ControllerRevision{lwsRevision1, lwsRevision2},
		},
		{
			name:      "inverted sorted",
			revisions: []*apps.ControllerRevision{lwsRevision2, lwsRevision1},
			want:      []*apps.ControllerRevision{lwsRevision1, lwsRevision2},
		},
		{
			name:      "same revision name, different timestamp",
			revisions: []*apps.ControllerRevision{lwsRevision1, lwsRevision2, lwsRevision1Time2},
			want:      []*apps.ControllerRevision{lwsRevision1, lwsRevision1Time2, lwsRevision2},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			SortControllerRevisions(tc.revisions)
			if diff := cmp.Diff(tc.revisions, tc.want); diff != "" {
				t.Errorf("error sorting revisions %s", diff)
			}
		})
	}
}

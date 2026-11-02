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

package revision

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

	"github.com/google/go-cmp/cmp"
	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/utils/lru"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	leaderworkerset "sigs.k8s.io/lws/api/leaderworkerset/v1"
	"sigs.k8s.io/lws/test/wrappers"
)

func exclusivePlacement(topology string) *leaderworkerset.PlacementPolicy {
	return &leaderworkerset.PlacementPolicy{Type: leaderworkerset.PlacementExclusiveTopology, Topology: topology}
}

func withExclusiveAnnotation(lws *leaderworkerset.LeaderWorkerSet, topology string) *leaderworkerset.LeaderWorkerSet {
	lws = lws.DeepCopy()
	if lws.Annotations == nil {
		lws.Annotations = map[string]string{}
	}
	lws.Annotations[leaderworkerset.ExclusiveKeyAnnotationKey] = topology
	return lws
}

func withPlacementPolicy(lws *leaderworkerset.LeaderWorkerSet, policy *leaderworkerset.PlacementPolicy) *leaderworkerset.LeaderWorkerSet {
	lws = lws.DeepCopy()
	lws.Spec.PlacementPolicy = policy
	return lws
}

func withoutExclusiveAnnotation(lws *leaderworkerset.LeaderWorkerSet) *leaderworkerset.LeaderWorkerSet {
	lws = lws.DeepCopy()
	delete(lws.Annotations, leaderworkerset.ExclusiveKeyAnnotationKey)
	return lws
}

func mustNewRevision(t *testing.T, lws *leaderworkerset.LeaderWorkerSet) *appsv1.ControllerRevision {
	t.Helper()
	revision, err := NewRevision(context.TODO(), fake.NewClientBuilder().Build(), lws, "")
	if err != nil {
		t.Fatal(err)
	}
	return revision
}

// withoutRecordedPlacement turns a revision into one created before revisions
// recorded placement.
func withoutRecordedPlacement(t *testing.T, revision *appsv1.ControllerRevision) *appsv1.ControllerRevision {
	t.Helper()
	revision = revision.DeepCopy()
	delete(revision.Annotations, placementRecordedAnnotationKey)
	var patch map[string]interface{}
	if err := json.Unmarshal(revision.Data.Raw, &patch); err != nil {
		t.Fatal(err)
	}
	delete(patch["spec"].(map[string]interface{}), "placementPolicy")
	raw, err := json.Marshal(patch)
	if err != nil {
		t.Fatal(err)
	}
	revision.Data.Raw = raw
	return revision
}

// keepsRevision reports whether the controller keeps the existing revision for
// the live object, as getUpdatedRevision decides it.
func keepsRevision(t *testing.T, live *leaderworkerset.LeaderWorkerSet, existing *appsv1.ControllerRevision) bool {
	t.Helper()
	proposed := mustNewRevision(t, live)
	return EqualRevision(proposed, existing) || SetMatchesRevision(live, proposed, existing, lru.New(1))
}

func TestNewRevisionRecordsEffectivePlacement(t *testing.T) {
	lws := wrappers.BuildLeaderWorkerSet("default").Obj()

	fromAnnotation := mustNewRevision(t, withExclusiveAnnotation(lws, "rack"))
	fromField := mustNewRevision(t, withPlacementPolicy(lws, exclusivePlacement("rack")))
	if !bytes.Equal(fromAnnotation.Data.Raw, fromField.Data.Raw) {
		t.Fatalf("the annotation and the field must record the same revision:\nannotation=%s\nfield=%s", fromAnnotation.Data.Raw, fromField.Data.Raw)
	}

	none := mustNewRevision(t, lws)
	explicitNone := mustNewRevision(t, withPlacementPolicy(lws, &leaderworkerset.PlacementPolicy{Type: leaderworkerset.PlacementNone}))
	if !bytes.Equal(none.Data.Raw, explicitNone.Data.Raw) {
		t.Fatalf("an absent policy and type None must record the same revision:\nabsent=%s\nNone=%s", none.Data.Raw, explicitNone.Data.Raw)
	}
	// A LeaderWorkerSet without placement must keep the revision data, and so the
	// hash, it had before placement was recorded, or upgrading the controller
	// would roll every group.
	inData, err := revisionDataHasPlacement(none)
	if err != nil || inData {
		t.Fatalf("a revision without placement must leave placementPolicy out of its data, got inData=%t err=%v", inData, err)
	}
	if !revisionRecordsPlacement(none, inData) {
		t.Fatalf("a revision without placement must still record it")
	}
	if beforeUpgrade := withoutRecordedPlacement(t, none); !EqualRevision(none, beforeUpgrade) || hashRevision(none) != hashRevision(beforeUpgrade) {
		t.Fatalf("a revision without placement must hash as it did before placement was recorded")
	}
}

func TestRevisionPlacementChanges(t *testing.T) {
	lws := wrappers.BuildLeaderWorkerSet("default").Obj()
	rack := withExclusiveAnnotation(lws, "rack")

	tests := []struct {
		name     string
		existing *appsv1.ControllerRevision
		live     *leaderworkerset.LeaderWorkerSet
		wantKeep bool
	}{
		{
			name:     "annotation moved to the field in one update",
			existing: mustNewRevision(t, rack),
			live:     withoutExclusiveAnnotation(withPlacementPolicy(rack, exclusivePlacement("rack"))),
			wantKeep: true,
		},
		{
			name:     "matching field added next to the annotation",
			existing: mustNewRevision(t, rack),
			live:     withPlacementPolicy(rack, exclusivePlacement("rack")),
			wantKeep: true,
		},
		{
			name:     "annotation changed",
			existing: mustNewRevision(t, rack),
			live:     withExclusiveAnnotation(lws, "zone"),
		},
		{
			name:     "annotation added",
			existing: mustNewRevision(t, lws),
			live:     rack,
		},
		{
			name:     "field changed",
			existing: mustNewRevision(t, withPlacementPolicy(lws, exclusivePlacement("rack"))),
			live:     withPlacementPolicy(lws, exclusivePlacement("zone")),
		},
		{
			name:     "placement removed",
			existing: mustNewRevision(t, withPlacementPolicy(lws, exclusivePlacement("rack"))),
			live:     lws,
		},
		{
			name:     "revision from before placement was recorded, after an upgrade",
			existing: withoutRecordedPlacement(t, mustNewRevision(t, rack)),
			live:     rack,
			wantKeep: true,
		},
		{
			name:     "revision from before placement was recorded, without placement",
			existing: withoutRecordedPlacement(t, mustNewRevision(t, lws)),
			live:     lws,
			wantKeep: true,
		},
		{
			// A revision from before placement was recorded cannot tell this
			// apart from adding the field, so the move rolls against it. The
			// controller records placement in such a revision before it
			// compares, which TestNewRevisionRecordingPlacement covers.
			name:     "revision from before placement was recorded, annotation moved to the field",
			existing: withoutRecordedPlacement(t, mustNewRevision(t, rack)),
			live:     withoutExclusiveAnnotation(withPlacementPolicy(rack, exclusivePlacement("rack"))),
		},
		{
			name:     "revision from before placement was recorded, field added",
			existing: withoutRecordedPlacement(t, mustNewRevision(t, lws)),
			live:     withPlacementPolicy(lws, exclusivePlacement("rack")),
		},
		{
			name:     "revision from before placement was recorded, matching field added next to the annotation",
			existing: withoutRecordedPlacement(t, mustNewRevision(t, rack)),
			live:     withPlacementPolicy(rack, exclusivePlacement("rack")),
			wantKeep: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := keepsRevision(t, tc.live, tc.existing); got != tc.wantKeep {
				t.Errorf("keeps the existing revision = %t, want %t", got, tc.wantKeep)
			}
		})
	}
}

func TestApplyRevisionRestoresPlacement(t *testing.T) {
	lws := wrappers.BuildLeaderWorkerSet("default").Obj()
	live := withExclusiveAnnotation(lws, "zone")

	t.Run("a revision that records placement replaces the live annotation", func(t *testing.T) {
		restored, err := ApplyRevision(live, mustNewRevision(t, withExclusiveAnnotation(lws, "rack")))
		if err != nil {
			t.Fatal(err)
		}
		if diff := cmp.Diff(exclusivePlacement("rack"), restored.Spec.PlacementPolicy); diff != "" {
			t.Errorf("restored placementPolicy mismatch (-want +got):\n%s", diff)
		}
		if _, found := restored.Annotations[leaderworkerset.ExclusiveKeyAnnotationKey]; found {
			t.Errorf("restored object kept the live %s annotation", leaderworkerset.ExclusiveKeyAnnotationKey)
		}
	})

	t.Run("a revision from before placement was recorded keeps the live annotation", func(t *testing.T) {
		// The field did not exist before revisions recorded placement, so a
		// live field must not survive into the restored object.
		restored, err := ApplyRevision(withPlacementPolicy(live, exclusivePlacement("zone")), withoutRecordedPlacement(t, mustNewRevision(t, lws)))
		if err != nil {
			t.Fatal(err)
		}
		if restored.Spec.PlacementPolicy != nil {
			t.Errorf("restored placementPolicy = %+v, want nil", restored.Spec.PlacementPolicy)
		}
		if got := restored.Annotations[leaderworkerset.ExclusiveKeyAnnotationKey]; got != "zone" {
			t.Errorf("restored %s annotation = %q, want %q", leaderworkerset.ExclusiveKeyAnnotationKey, got, "zone")
		}
	})
}

func TestSetMatchesRevisionCacheTracksAnnotations(t *testing.T) {
	lws := wrappers.BuildLeaderWorkerSet("default").Obj()
	rack := withExclusiveAnnotation(lws, "rack")
	existing := mustNewRevision(t, rack)
	existing.ResourceVersion = "1"
	cache := lru.New(10)

	if !SetMatchesRevision(rack, mustNewRevision(t, rack), existing, cache) {
		t.Fatal("the revision of the same object must match")
	}
	// Annotations do not bump the generation, so a cached match for the old
	// annotation must not hide the change.
	zone := withExclusiveAnnotation(lws, "zone")
	if zone.Generation != rack.Generation {
		t.Fatalf("test setup: generations differ (%d, %d)", zone.Generation, rack.Generation)
	}
	if SetMatchesRevision(zone, mustNewRevision(t, zone), existing, cache) {
		t.Error("a changed exclusive-topology annotation matched the cached revision")
	}
}

// keepsRevisionAfterUpgrade reports whether the controller keeps a revision
// created before placement was recorded, once it has recorded placement in it
// from the leader workload's pod template annotations.
func keepsRevisionAfterUpgrade(t *testing.T, live *leaderworkerset.LeaderWorkerSet, legacy *appsv1.ControllerRevision, leaderTemplateAnnotations map[string]string) bool {
	t.Helper()
	migrated, err := NewRevisionRecordingPlacement(context.TODO(), fake.NewClientBuilder().Build(), live, legacy, leaderTemplateAnnotations)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := GetRevisionKey(migrated), GetRevisionKey(legacy); got != want {
		t.Fatalf("recorded revision key = %q, want the key of the old revision %q", got, want)
	}
	recorded, err := RecordsPlacement(migrated)
	if err != nil || !recorded {
		t.Fatalf("recorded revision must record placement, got %t, err=%v", recorded, err)
	}
	return keepsRevision(t, live, migrated)
}

func TestNewRevisionRecordingPlacement(t *testing.T) {
	lws := wrappers.BuildLeaderWorkerSet("default").Obj()
	rack := withExclusiveAnnotation(lws, "rack")
	rackTemplate := map[string]string{leaderworkerset.ExclusiveKeyAnnotationKey: "rack"}
	subGroups := wrappers.BuildLeaderWorkerSet("default").Size(4).SubGroupSize(2).Obj()
	subGroupRack := subGroups.DeepCopy()
	subGroupRack.Annotations = map[string]string{leaderworkerset.SubGroupExclusiveKeyAnnotationKey: "rack"}
	subGroupRackTemplate := map[string]string{leaderworkerset.SubGroupExclusiveKeyAnnotationKey: "rack"}

	tests := []struct {
		name             string
		legacy           *appsv1.ControllerRevision
		leaderAnnotation map[string]string
		live             *leaderworkerset.LeaderWorkerSet
		wantKeep         bool
	}{
		{
			name:     "without placement",
			legacy:   withoutRecordedPlacement(t, mustNewRevision(t, lws)),
			live:     lws,
			wantKeep: true,
		},
		{
			name:             "with the annotation",
			legacy:           withoutRecordedPlacement(t, mustNewRevision(t, rack)),
			leaderAnnotation: rackTemplate,
			live:             rack,
			wantKeep:         true,
		},
		{
			name:             "with the subgroup annotation",
			legacy:           withoutRecordedPlacement(t, mustNewRevision(t, subGroupRack)),
			leaderAnnotation: subGroupRackTemplate,
			live:             subGroupRack,
			wantKeep:         true,
		},
		{
			name:   "annotation added",
			legacy: withoutRecordedPlacement(t, mustNewRevision(t, lws)),
			live:   rack,
		},
		{
			name:             "annotation removed",
			legacy:           withoutRecordedPlacement(t, mustNewRevision(t, rack)),
			leaderAnnotation: rackTemplate,
			live:             lws,
		},
		{
			name:             "subgroup annotation changed",
			legacy:           withoutRecordedPlacement(t, mustNewRevision(t, subGroupRack)),
			leaderAnnotation: subGroupRackTemplate,
			live: func() *leaderworkerset.LeaderWorkerSet {
				zone := subGroups.DeepCopy()
				zone.Annotations = map[string]string{leaderworkerset.SubGroupExclusiveKeyAnnotationKey: "zone"}
				return zone
			}(),
		},
		{
			name:   "field added",
			legacy: withoutRecordedPlacement(t, mustNewRevision(t, lws)),
			live:   withPlacementPolicy(lws, exclusivePlacement("rack")),
		},
		{
			name:             "annotation moved to the field in one update",
			legacy:           withoutRecordedPlacement(t, mustNewRevision(t, rack)),
			leaderAnnotation: rackTemplate,
			live:             withoutExclusiveAnnotation(withPlacementPolicy(rack, exclusivePlacement("rack"))),
			wantKeep:         true,
		},
		{
			name:             "matching field added next to the annotation",
			legacy:           withoutRecordedPlacement(t, mustNewRevision(t, rack)),
			leaderAnnotation: rackTemplate,
			live:             withPlacementPolicy(rack, exclusivePlacement("rack")),
			wantKeep:         true,
		},
		{
			// This also covers a change made before the upgraded controller first
			// reconciled: the leader template still holds the old value, so the
			// change rolls instead of being absorbed into the recorded revision.
			name:             "annotation changed",
			legacy:           withoutRecordedPlacement(t, mustNewRevision(t, rack)),
			leaderAnnotation: rackTemplate,
			live:             withExclusiveAnnotation(lws, "zone"),
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := keepsRevisionAfterUpgrade(t, tc.live, tc.legacy, tc.leaderAnnotation); got != tc.wantKeep {
				t.Errorf("keeps the revision = %t, want %t", got, tc.wantKeep)
			}
		})
	}

	t.Run("a revision without placement keeps its data and hash", func(t *testing.T) {
		legacy := withoutRecordedPlacement(t, mustNewRevision(t, lws))
		migrated, err := NewRevisionRecordingPlacement(context.TODO(), fake.NewClientBuilder().Build(), lws, legacy, nil)
		if err != nil {
			t.Fatal(err)
		}
		if !EqualRevision(legacy, migrated) || hashRevision(legacy) != hashRevision(migrated) {
			t.Errorf("recorded revision data changed:\nold=%s\nnew=%s", legacy.Data.Raw, migrated.Data.Raw)
		}
	})
}

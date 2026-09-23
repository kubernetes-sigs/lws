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

package revision

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/lru"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	leaderworkerset "sigs.k8s.io/lws/api/leaderworkerset/v1"
	"sigs.k8s.io/lws/test/wrappers"
)

func TestNewRevisionIgnoresMaxGroupRestarts(t *testing.T) {
	client := fake.NewClientBuilder().Build()

	withoutBudget := wrappers.BuildLeaderWorkerSet("default").MaxGroupRestarts(1).Obj()
	withDifferentBudget := withoutBudget.DeepCopy()
	otherBudget := int32(2)
	withDifferentBudget.Spec.LeaderWorkerTemplate.MaxGroupRestarts = &otherBudget

	first, err := NewRevision(context.TODO(), client, withoutBudget, "")
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewRevision(context.TODO(), client, withDifferentBudget, "")
	if err != nil {
		t.Fatal(err)
	}

	if !bytes.Equal(first.Data.Raw, second.Data.Raw) {
		t.Fatalf("maxGroupRestarts must not change revision data: first=%s second=%s", first.Data.Raw, second.Data.Raw)
	}
	if first.Name != second.Name {
		t.Fatalf("maxGroupRestarts must not change revision identity: first=%q second=%q", first.Name, second.Name)
	}
	if bytes.Contains(first.Data.Raw, []byte("maxGroupRestarts")) {
		t.Fatalf("revision data unexpectedly contains maxGroupRestarts: %s", first.Data.Raw)
	}
}

func TestApplyRevisionPreservesMaxGroupRestarts(t *testing.T) {
	client := fake.NewClientBuilder().Build()

	source := wrappers.BuildLeaderWorkerSet("default").MaxGroupRestarts(1).Obj()
	revision, err := NewRevision(context.TODO(), client, source, "")
	if err != nil {
		t.Fatal(err)
	}

	// Simulate a revision created before maxGroupRestarts was removed from the
	// revision patch. ApplyRevision must preserve the live budget even then.
	var patch map[string]interface{}
	if err := json.Unmarshal(revision.Data.Raw, &patch); err != nil {
		t.Fatal(err)
	}
	spec := patch["spec"].(map[string]interface{})
	template := spec["leaderWorkerTemplate"].(map[string]interface{})
	template["maxGroupRestarts"] = float64(1)
	revision.Data.Raw, err = json.Marshal(patch)
	if err != nil {
		t.Fatal(err)
	}

	live := source.DeepCopy()
	currentBudget := int32(7)
	live.Spec.LeaderWorkerTemplate.MaxGroupRestarts = &currentBudget
	restored, err := ApplyRevision(live, revision)
	if err != nil {
		t.Fatal(err)
	}
	if restored.Spec.LeaderWorkerTemplate.MaxGroupRestarts == nil {
		t.Fatal("ApplyRevision cleared the live maxGroupRestarts budget")
	}
	if got := *restored.Spec.LeaderWorkerTemplate.MaxGroupRestarts; got != currentBudget {
		t.Fatalf("ApplyRevision restored maxGroupRestarts=%d, want %d", got, currentBudget)
	}
}

func TestApplyRevision(t *testing.T) {
	client := fake.NewClientBuilder().Build()

	lws := wrappers.BuildLeaderWorkerSet("default").Obj()
	revision, err := NewRevision(context.TODO(), client, lws, "")
	if err != nil {
		t.Fatal(err)
	}
	currentLws := lws.DeepCopy()

	lws.Spec.LeaderWorkerTemplate.LeaderTemplate.Spec.Containers[0].Name = "update-name"
	subdomainPolicy := leaderworkerset.SubdomainUniquePerReplica
	lws.Spec.NetworkConfig = &leaderworkerset.NetworkConfig{
		SubdomainPolicy: &subdomainPolicy,
	}
	lws.Spec.RolloutStrategy = leaderworkerset.RolloutStrategy{
		Type: leaderworkerset.RollingUpdateStrategyType,
		RollingUpdateConfiguration: &leaderworkerset.RollingUpdateConfiguration{
			MaxUnavailable: intstr.FromInt32(2),
			MaxSurge:       intstr.FromInt(1),
		},
	}
	restoredLws, err := ApplyRevision(lws, revision)
	if err != nil {
		t.Fatal(err)
	}

	restoredRevision, err := NewRevision(context.TODO(), client, restoredLws, "")
	if err != nil {
		t.Fatal(err)
	}

	if !EqualRevision(revision, restoredRevision) {
		t.Errorf("expected value %v, got %v", revision, restoredRevision)
	}

	if diff := cmp.Diff(currentLws.Spec.LeaderWorkerTemplate, restoredLws.Spec.LeaderWorkerTemplate); diff != "" {
		t.Errorf("unexpected restored LeaderWorkerTemplate: %s", diff)
	}

	if diff := cmp.Diff(currentLws.Spec.NetworkConfig, restoredLws.Spec.NetworkConfig); diff != "" {
		t.Errorf("NetworkConfig should be restored %s", diff)
	}

	if diff := cmp.Diff(lws.Spec.RolloutStrategy, restoredLws.Spec.RolloutStrategy); diff != "" {
		t.Errorf("It should not restore/clear non NetworkConfig Spec fields %s,", diff)
	}
}

func TestEqualRevision(t *testing.T) {
	client := fake.NewClientBuilder().Build()
	tests := []struct {
		name             string
		leftLws          *leaderworkerset.LeaderWorkerSet
		rightLws         *leaderworkerset.LeaderWorkerSet
		leftRevisionKey  string
		rightRevisionKey string
		equal            bool
	}{
		{
			name:             "same LeaderWorkerTemplate, networkConfig, should be equal",
			leftLws:          wrappers.BuildLeaderWorkerSet("default").Obj(),
			rightLws:         wrappers.BuildLeaderWorkerSet("default").Obj(),
			leftRevisionKey:  "",
			rightRevisionKey: "",
			equal:            true,
		},
		{
			name:             "same LeaderWorkerTemplate, networkConfig, different revisionKey, should be equal",
			leftLws:          wrappers.BuildLeaderWorkerSet("default").Obj(),
			rightLws:         wrappers.BuildLeaderWorkerSet("default").Obj(),
			leftRevisionKey:  "",
			rightRevisionKey: "templateHash",
			equal:            true,
		},
		{
			name:             "same LeaderWorkerTemplate, shared subdomainpolicy & nil, should be equal",
			leftLws:          wrappers.BuildLeaderWorkerSet("default").SubdomainPolicy(leaderworkerset.SubdomainShared).Obj(),
			rightLws:         wrappers.BuildLeaderWorkerSet("default").SubdomainNil().Obj(),
			leftRevisionKey:  "",
			rightRevisionKey: "",
			equal:            true,
		},
		{
			name:             "left nil, right nil, should be equal",
			leftLws:          nil,
			rightLws:         nil,
			leftRevisionKey:  "",
			rightRevisionKey: "",
			equal:            true,
		},
		{
			name:             "semantically same LeaderWorkerTemplate, different fields set, same networkConfig, should be equal",
			leftLws:          wrappers.BuildLeaderWorkerSet("default").WorkerTemplateSpec(wrappers.MakeWorkerPodSpecWithVolumeAndNilImage()).Obj(),
			rightLws:         wrappers.BuildLeaderWorkerSet("default").WorkerTemplateSpec(wrappers.MakeWorkerPodSpecWithVolume()).Obj(),
			leftRevisionKey:  "",
			rightRevisionKey: "",
			equal:            true,
		},
		{
			name:             "left nil, right non-nil, should not be equal",
			leftLws:          nil,
			rightLws:         wrappers.BuildLeaderWorkerSet("default").Obj(),
			leftRevisionKey:  "",
			rightRevisionKey: "",
			equal:            false,
		},
		{
			name:             "same LeaderWorkerTemplate, different networkConfig, should not be equal",
			leftLws:          wrappers.BuildLeaderWorkerSet("default").SubdomainPolicy(leaderworkerset.SubdomainUniquePerReplica).Obj(),
			rightLws:         wrappers.BuildLeaderWorkerSet("default").Obj(),
			leftRevisionKey:  "",
			rightRevisionKey: "",
			equal:            false,
		},
		{
			name:             "different LeaderWorkerTemplate, same networkConfig, should not be equal",
			leftLws:          wrappers.BuildLeaderWorkerSet("default").Obj(),
			rightLws:         wrappers.BuildLeaderWorkerSet("default").WorkerTemplateSpec(wrappers.MakeLeaderPodSpec()).Obj(),
			leftRevisionKey:  "",
			rightRevisionKey: "",
			equal:            false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var leftRevision *appsv1.ControllerRevision
			var rightRevision *appsv1.ControllerRevision
			var err error
			if tc.leftLws != nil {
				leftRevision, err = NewRevision(context.TODO(), client, tc.leftLws, tc.leftRevisionKey)
				if err != nil {
					t.Fatal(err)
				}
			}
			if tc.rightLws != nil {
				rightRevision, err = NewRevision(context.TODO(), client, tc.rightLws, tc.rightRevisionKey)
				if err != nil {
					t.Fatal(err)
				}
			}
			equal := EqualRevision(leftRevision, rightRevision)
			if tc.equal != equal {
				t.Errorf("Expected equality between controller revisions to be %t, but was %t", tc.equal, equal)
			}
		})
	}
}

func TestSetMatchesRevision(t *testing.T) {
	client := fake.NewClientBuilder().Build()

	lws := wrappers.BuildLeaderWorkerSet("default").Obj()
	lws.UID = types.UID("test-uid")
	lws.Generation = 1

	revision, err := NewRevision(context.TODO(), client, lws, "")
	if err != nil {
		t.Fatal(err)
	}
	revision.ResourceVersion = "100"

	// Build proposed revision from the same LWS (should match).
	proposed, err := NewRevision(context.TODO(), client, lws, "")
	if err != nil {
		t.Fatal(err)
	}

	cache := lru.New(10)

	// First call: cache miss, should match via patch comparison and populate the cache.
	if !SetMatchesRevision(lws, proposed, revision, cache) {
		t.Fatal("expected SetMatchesRevision to return true on first call (cache miss path)")
	}
	if cache.Len() != 1 {
		t.Fatalf("expected cache to have 1 entry after first call, got %d", cache.Len())
	}

	// Second call with the same inputs: should hit the cache and return true immediately.
	proposed.Data.Raw = []byte(`{"spec":{"leaderWorkerTemplate":{"$patch":"replace"}}}`)
	if !SetMatchesRevision(lws, proposed, revision, cache) {
		t.Fatal("expected SetMatchesRevision to return true on second call (cache hit path)")
	}

	// Verify a different LWS generation produces a cache miss, and the mutated proposed
	// revision correctly causes a mismatch.
	lws.Generation = 2
	if SetMatchesRevision(lws, proposed, revision, cache) {
		t.Fatal("expected SetMatchesRevision to return false for different generation with mismatched proposed data")
	}
}

func TestGetHighestRevision(t *testing.T) {
	client := fake.NewClientBuilder().Build()
	lws := wrappers.BuildLeaderWorkerSet("default").Obj()
	revision1, err := NewRevision(context.TODO(), client, lws, "")
	if err != nil {
		t.Fatal(err)
	}
	revision2 := revision1.DeepCopy()
	revision2.Revision = 2
	revision3 := revision2.DeepCopy()
	revision3.Revision = 3
	tests := []struct {
		name             string
		revisions        []*appsv1.ControllerRevision
		expectedRevision *appsv1.ControllerRevision
	}{
		{
			name:             "empty revision list, returns nil",
			revisions:        []*appsv1.ControllerRevision{},
			expectedRevision: nil,
		},
		{
			name:             "only one revision in list, returns it",
			revisions:        []*appsv1.ControllerRevision{revision1},
			expectedRevision: revision1,
		},
		{
			name:             "returns the revision with highest revision number",
			revisions:        []*appsv1.ControllerRevision{revision2, revision3, revision2},
			expectedRevision: revision3,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			revision := getHighestRevision(tc.revisions)
			if tc.expectedRevision == nil {
				if revision != nil {
					t.Errorf("Expected revision to be nil")
				}
			} else {
				if tc.expectedRevision.Revision != revision.Revision {
					t.Errorf("Expected revision number to be %d, but it was %d", tc.expectedRevision.Revision, revision.Revision)
				}
			}
		})
	}
}

func TestGetRevisionKey(t *testing.T) {
	t.Run("returns the revision label when present", func(t *testing.T) {
		lws := wrappers.BuildLeaderWorkerSet("default").Obj()
		lws.Labels = map[string]string{leaderworkerset.RevisionKey: "abc123"}
		if got := GetRevisionKey(lws); got != "abc123" {
			t.Fatalf("GetRevisionKey()=%q, want %q", got, "abc123")
		}
	})
	t.Run("returns empty when labels are nil", func(t *testing.T) {
		lws := wrappers.BuildLeaderWorkerSet("default").Obj()
		lws.Labels = nil
		if got := GetRevisionKey(lws); got != "" {
			t.Fatalf("GetRevisionKey()=%q, want empty", got)
		}
	})
	t.Run("returns empty when the label is absent", func(t *testing.T) {
		lws := wrappers.BuildLeaderWorkerSet("default").Obj()
		lws.Labels = map[string]string{"other": "x"}
		if got := GetRevisionKey(lws); got != "" {
			t.Fatalf("GetRevisionKey()=%q, want empty", got)
		}
	})
}

func TestRevisionName(t *testing.T) {
	if got := revisionName("lws", "h4sh", 3); got != "lws-h4sh-3" {
		t.Fatalf("revisionName()=%q, want %q", got, "lws-h4sh-3")
	}
	long := strings.Repeat("a", 230)
	got := revisionName(long, "h4sh", 1)
	if want := strings.Repeat("a", 220) + "-h4sh-1"; got != want {
		t.Fatalf("revisionName() with long prefix = %d chars, want prefix truncated to 220", len(got))
	}
}

func TestHashRevision(t *testing.T) {
	a := &appsv1.ControllerRevision{Data: runtime.RawExtension{Raw: []byte(`{"a":1}`)}}
	b := &appsv1.ControllerRevision{Data: runtime.RawExtension{Raw: []byte(`{"a":1}`)}}
	c := &appsv1.ControllerRevision{Data: runtime.RawExtension{Raw: []byte(`{"a":2}`)}}
	if hashRevision(a) != hashRevision(b) {
		t.Fatalf("identical data must hash identically")
	}
	if hashRevision(a) == hashRevision(c) {
		t.Fatalf("different data must hash differently")
	}
	empty := &appsv1.ControllerRevision{}
	if hashRevision(empty) == "" {
		t.Fatalf("empty revision must still produce a hash")
	}
}

func TestGetRevisionAndTruncate(t *testing.T) {
	client := fake.NewClientBuilder().Build()
	ctx := context.Background()
	lws := wrappers.BuildLeaderWorkerSet("default").Obj()
	lws.UID = types.UID("owner-uid")

	mk := func(key string, number int64) *appsv1.ControllerRevision {
		cr, err := NewRevision(ctx, client, lws, key)
		if err != nil {
			t.Fatalf("NewRevision: %v", err)
		}
		cr.Revision = number
		cr.Name = revisionName(lws.Name, key, number)
		if _, err := CreateRevision(ctx, client, cr); err != nil {
			t.Fatalf("CreateRevision: %v", err)
		}
		return cr
	}

	t.Run("empty key returns nil without listing", func(t *testing.T) {
		got, err := GetRevision(ctx, client, lws, "")
		if err != nil || got != nil {
			t.Fatalf("GetRevision(\"\")=%v,%v want nil,nil", got, err)
		}
	})

	t.Run("no matching revision returns nil", func(t *testing.T) {
		got, err := GetRevision(ctx, client, lws, "missing")
		if err != nil || got != nil {
			t.Fatalf("GetRevision(missing)=%v,%v want nil,nil", got, err)
		}
	})

	keyA1 := mk("key-a", 1)
	mk("key-b", 2)
	keyA3 := mk("key-a", 3)

	t.Run("single match is returned", func(t *testing.T) {
		got, err := GetRevision(ctx, client, lws, "key-b")
		if err != nil || got == nil || got.Revision != 2 {
			t.Fatalf("GetRevision(key-b)=%v,%v want revision 2", got, err)
		}
	})

	t.Run("multiple matches return the highest revision", func(t *testing.T) {
		got, err := GetRevision(ctx, client, lws, "key-a")
		if err != nil || got == nil {
			t.Fatalf("GetRevision(key-a)=%v,%v", got, err)
		}
		if got.Name != keyA3.Name || got.Name == keyA1.Name {
			t.Fatalf("GetRevision(key-a) returned %s, want %s", got.Name, keyA3.Name)
		}
	})

	t.Run("revisions owned by another controller are ignored", func(t *testing.T) {
		other := wrappers.BuildLeaderWorkerSet("default").Obj()
		other.Name = lws.Name // same name label, different owner UID
		other.UID = types.UID("other-uid")
		cr, err := NewRevision(ctx, client, other, "key-c")
		if err != nil {
			t.Fatalf("NewRevision: %v", err)
		}
		cr.Revision = 9
		cr.Name = revisionName("other", "key-c", 9)
		if _, err := CreateRevision(ctx, client, cr); err != nil {
			t.Fatalf("CreateRevision: %v", err)
		}
		got, err := GetRevision(ctx, client, lws, "key-c")
		if err != nil || got != nil {
			t.Fatalf("GetRevision(key-c) must not see another controller's revision, got %v,%v", got, err)
		}
	})

	t.Run("truncate keeps only the current key", func(t *testing.T) {
		if err := TruncateRevisions(ctx, client, lws, "key-a"); err != nil {
			t.Fatalf("TruncateRevisions: %v", err)
		}
		list := &appsv1.ControllerRevisionList{}
		if err := client.List(ctx, list); err != nil {
			t.Fatalf("List: %v", err)
		}
		var mine []string
		for i := range list.Items {
			ref := metav1.GetControllerOfNoCopy(&list.Items[i])
			if ref != nil && ref.UID == lws.UID {
				mine = append(mine, GetRevisionKey(&list.Items[i]))
			}
		}
		if len(mine) != 2 || mine[0] != "key-a" || mine[1] != "key-a" {
			t.Fatalf("after truncate, owned revision keys = %v, want two key-a", mine)
		}
		if len(list.Items) != 3 {
			t.Fatalf("truncate must not touch another controller's revision, total=%d want 3", len(list.Items))
		}
	})
}

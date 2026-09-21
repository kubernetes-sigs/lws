package disaggregatedset

import (
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	leaderworkersetv1 "sigs.k8s.io/lws/api/leaderworkerset/v1"

	disaggregatedsetv1 "sigs.k8s.io/lws/api/disaggregatedset/v1"
)

const (
	testUtilsRolePrefill = "prefill"
	testUtilsRoleDecode  = "decode"
)

func TestGetInitialReplicas(t *testing.T) {
	t.Run("returns parsed int from valid annotation", func(t *testing.T) {
		leaderWorkerSet := &leaderworkersetv1.LeaderWorkerSet{
			ObjectMeta: metav1.ObjectMeta{
				Name: "test-lws",
				Annotations: map[string]string{
					disaggregatedsetv1.InitialReplicasAnnotationKey: "5",
				},
			},
		}

		replicas, ok := GetInitialReplicas(leaderWorkerSet)
		require.True(t, ok, "should return ok=true for valid annotation")
		assert.Equal(t, int32(5), replicas, "replicas should be 5")
	})

	t.Run("returns zero and false for missing annotation", func(t *testing.T) {
		leaderWorkerSet := &leaderworkersetv1.LeaderWorkerSet{
			ObjectMeta: metav1.ObjectMeta{
				Name:        "test-lws",
				Annotations: map[string]string{},
			},
		}

		replicas, ok := GetInitialReplicas(leaderWorkerSet)
		assert.False(t, ok, "should return ok=false for missing annotation")
		assert.Equal(t, int32(0), replicas, "replicas should be 0")
	})

	t.Run("returns zero and false for nil annotations", func(t *testing.T) {
		leaderWorkerSet := &leaderworkersetv1.LeaderWorkerSet{
			ObjectMeta: metav1.ObjectMeta{
				Name: "test-lws",
			},
		}

		replicas, ok := GetInitialReplicas(leaderWorkerSet)
		assert.False(t, ok, "should return ok=false for nil annotations")
		assert.Equal(t, int32(0), replicas, "replicas should be 0")
	})

	t.Run("returns zero and false for invalid annotation value", func(t *testing.T) {
		leaderWorkerSet := &leaderworkersetv1.LeaderWorkerSet{
			ObjectMeta: metav1.ObjectMeta{
				Name: "test-lws",
				Annotations: map[string]string{
					disaggregatedsetv1.InitialReplicasAnnotationKey: "not-a-number",
				},
			},
		}

		replicas, ok := GetInitialReplicas(leaderWorkerSet)
		assert.False(t, ok, "should return ok=false for invalid annotation")
		assert.Equal(t, int32(0), replicas, "replicas should be 0")
	})

	t.Run("returns zero and false for empty annotation value", func(t *testing.T) {
		leaderWorkerSet := &leaderworkersetv1.LeaderWorkerSet{
			ObjectMeta: metav1.ObjectMeta{
				Name: "test-lws",
				Annotations: map[string]string{
					disaggregatedsetv1.InitialReplicasAnnotationKey: "",
				},
			},
		}

		replicas, ok := GetInitialReplicas(leaderWorkerSet)
		assert.False(t, ok, "should return ok=false for empty annotation")
		assert.Equal(t, int32(0), replicas, "replicas should be 0")
	})

	t.Run("handles zero value annotation", func(t *testing.T) {
		leaderWorkerSet := &leaderworkersetv1.LeaderWorkerSet{
			ObjectMeta: metav1.ObjectMeta{
				Name: "test-lws",
				Annotations: map[string]string{
					disaggregatedsetv1.InitialReplicasAnnotationKey: "0",
				},
			},
		}

		replicas, ok := GetInitialReplicas(leaderWorkerSet)
		require.True(t, ok, "should return ok=true for zero value annotation")
		assert.Equal(t, int32(0), replicas, "replicas should be 0")
	})
}

func TestComputeInitialReplicaState(t *testing.T) {
	t.Run("returns empty map for empty list", func(t *testing.T) {
		lwsList := []leaderworkersetv1.LeaderWorkerSet{}

		state := ComputeInitialReplicaState(lwsList)

		assert.Equal(t, 0, state[testUtilsRolePrefill], "prefill should be 0 for empty list")
		assert.Equal(t, 0, state[testUtilsRoleDecode], "decode should be 0 for empty list")
	})

	t.Run("takes maximum prefill annotation", func(t *testing.T) {
		lwsList := []leaderworkersetv1.LeaderWorkerSet{
			{
				ObjectMeta: metav1.ObjectMeta{
					Name: "lws-1",
					Labels: map[string]string{
						disaggregatedsetv1.RoleLabelKey: testUtilsRolePrefill,
					},
					Annotations: map[string]string{
						disaggregatedsetv1.InitialReplicasAnnotationKey: "3",
					},
				},
				Spec: leaderworkersetv1.LeaderWorkerSetSpec{
					Replicas: ptr.To(int32(3)),
				},
			},
			{
				ObjectMeta: metav1.ObjectMeta{
					Name: "lws-2",
					Labels: map[string]string{
						disaggregatedsetv1.RoleLabelKey: testUtilsRolePrefill,
					},
					Annotations: map[string]string{
						disaggregatedsetv1.InitialReplicasAnnotationKey: "2",
					},
				},
				Spec: leaderworkersetv1.LeaderWorkerSetSpec{
					Replicas: ptr.To(int32(2)),
				},
			},
		}

		state := ComputeInitialReplicaState(lwsList)

		assert.Equal(t, 3, state[testUtilsRolePrefill], "replacement revisions must not be added together")
		assert.Equal(t, 0, state[testUtilsRoleDecode], "decode should be 0")
	})

	t.Run("takes maximum decode annotation", func(t *testing.T) {
		lwsList := []leaderworkersetv1.LeaderWorkerSet{
			{
				ObjectMeta: metav1.ObjectMeta{
					Name: "lws-1",
					Labels: map[string]string{
						disaggregatedsetv1.RoleLabelKey: testUtilsRoleDecode,
					},
					Annotations: map[string]string{
						disaggregatedsetv1.InitialReplicasAnnotationKey: "4",
					},
				},
				Spec: leaderworkersetv1.LeaderWorkerSetSpec{
					Replicas: ptr.To(int32(4)),
				},
			},
			{
				ObjectMeta: metav1.ObjectMeta{
					Name: "lws-2",
					Labels: map[string]string{
						disaggregatedsetv1.RoleLabelKey: testUtilsRoleDecode,
					},
					Annotations: map[string]string{
						disaggregatedsetv1.InitialReplicasAnnotationKey: "6",
					},
				},
				Spec: leaderworkersetv1.LeaderWorkerSetSpec{
					Replicas: ptr.To(int32(6)),
				},
			},
		}

		state := ComputeInitialReplicaState(lwsList)

		assert.Equal(t, 0, state[testUtilsRolePrefill], "prefill should be 0")
		assert.Equal(t, 6, state[testUtilsRoleDecode], "replacement revisions must not be added together")
	})

	t.Run("takes per-role maximum across mixed revisions", func(t *testing.T) {
		lwsList := []leaderworkersetv1.LeaderWorkerSet{
			{
				ObjectMeta: metav1.ObjectMeta{
					Name: "lws-prefill-1",
					Labels: map[string]string{
						disaggregatedsetv1.RoleLabelKey: testUtilsRolePrefill,
					},
					Annotations: map[string]string{
						disaggregatedsetv1.InitialReplicasAnnotationKey: "3",
					},
				},
				Spec: leaderworkersetv1.LeaderWorkerSetSpec{
					Replicas: ptr.To(int32(3)),
				},
			},
			{
				ObjectMeta: metav1.ObjectMeta{
					Name: "lws-decode-1",
					Labels: map[string]string{
						disaggregatedsetv1.RoleLabelKey: testUtilsRoleDecode,
					},
					Annotations: map[string]string{
						disaggregatedsetv1.InitialReplicasAnnotationKey: "6",
					},
				},
				Spec: leaderworkersetv1.LeaderWorkerSetSpec{
					Replicas: ptr.To(int32(6)),
				},
			},
			{
				ObjectMeta: metav1.ObjectMeta{
					Name: "lws-prefill-2",
					Labels: map[string]string{
						disaggregatedsetv1.RoleLabelKey: testUtilsRolePrefill,
					},
					Annotations: map[string]string{
						disaggregatedsetv1.InitialReplicasAnnotationKey: "2",
					},
				},
				Spec: leaderworkersetv1.LeaderWorkerSetSpec{
					Replicas: ptr.To(int32(2)),
				},
			},
		}

		state := ComputeInitialReplicaState(lwsList)

		assert.Equal(t, 3, state[testUtilsRolePrefill])
		assert.Equal(t, 6, state[testUtilsRoleDecode], "decode should be 6")
	})

	t.Run("uses spec.Replicas fallback for missing annotation", func(t *testing.T) {
		lwsList := []leaderworkersetv1.LeaderWorkerSet{
			{
				ObjectMeta: metav1.ObjectMeta{
					Name: "lws-1",
					Labels: map[string]string{
						disaggregatedsetv1.RoleLabelKey: testUtilsRolePrefill,
					},
				},
				Spec: leaderworkersetv1.LeaderWorkerSetSpec{
					Replicas: ptr.To(int32(4)),
				},
			},
		}

		state := ComputeInitialReplicaState(lwsList)

		assert.Equal(t, 4, state[testUtilsRolePrefill], "prefill should be 4 (from spec.Replicas fallback)")
	})

	t.Run("uses spec.Replicas fallback for invalid annotation", func(t *testing.T) {
		lwsList := []leaderworkersetv1.LeaderWorkerSet{
			{
				ObjectMeta: metav1.ObjectMeta{
					Name: "lws-1",
					Labels: map[string]string{
						disaggregatedsetv1.RoleLabelKey: testUtilsRoleDecode,
					},
					Annotations: map[string]string{
						disaggregatedsetv1.InitialReplicasAnnotationKey: "not-a-number",
					},
				},
				Spec: leaderworkersetv1.LeaderWorkerSetSpec{
					Replicas: ptr.To(int32(5)),
				},
			},
		}

		state := ComputeInitialReplicaState(lwsList)

		assert.Equal(t, 5, state[testUtilsRoleDecode], "decode should be 5 (from spec.Replicas fallback)")
	})

	t.Run("handles mixed valid and invalid annotations", func(t *testing.T) {
		lwsList := []leaderworkersetv1.LeaderWorkerSet{
			{
				ObjectMeta: metav1.ObjectMeta{
					Name: "lws-1",
					Labels: map[string]string{
						disaggregatedsetv1.RoleLabelKey: testUtilsRolePrefill,
					},
					Annotations: map[string]string{
						disaggregatedsetv1.InitialReplicasAnnotationKey: "3",
					},
				},
				Spec: leaderworkersetv1.LeaderWorkerSetSpec{
					Replicas: ptr.To(int32(3)),
				},
			},
			{
				ObjectMeta: metav1.ObjectMeta{
					Name: "lws-2",
					Labels: map[string]string{
						disaggregatedsetv1.RoleLabelKey: testUtilsRolePrefill,
					},
					Annotations: map[string]string{
						disaggregatedsetv1.InitialReplicasAnnotationKey: "invalid",
					},
				},
				Spec: leaderworkersetv1.LeaderWorkerSetSpec{
					Replicas: ptr.To(int32(2)),
				},
			},
		}

		state := ComputeInitialReplicaState(lwsList)

		assert.Equal(t, 3, state[testUtilsRolePrefill], "the fallback is another revision target, not additive capacity")
	})

	t.Run("handles nil spec.Replicas with missing annotation", func(t *testing.T) {
		lwsList := []leaderworkersetv1.LeaderWorkerSet{
			{
				ObjectMeta: metav1.ObjectMeta{
					Name: "lws-1",
					Labels: map[string]string{
						disaggregatedsetv1.RoleLabelKey: testUtilsRolePrefill,
					},
				},
				Spec: leaderworkersetv1.LeaderWorkerSetSpec{},
			},
		}

		state := ComputeInitialReplicaState(lwsList)

		assert.Equal(t, 1, state[testUtilsRolePrefill], "prefill should be 1 (default when Replicas is nil)")
	})
}

func TestRevisionRolesListUsesInitialMaximumAndPhysicalSum(t *testing.T) {
	makeRole := func(specReplicas, initialReplicas int32) *leaderworkersetv1.LeaderWorkerSet {
		return &leaderworkersetv1.LeaderWorkerSet{
			ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{
				disaggregatedsetv1.InitialReplicasAnnotationKey: strconv.FormatInt(int64(initialReplicas), 10),
			}},
			Spec: leaderworkersetv1.LeaderWorkerSetSpec{Replicas: ptr.To(specReplicas)},
		}
	}

	// A and B are successive attempts to provide the same 6P/3D capacity.
	// Their initial-replicas values describe one logical baseline, while their
	// Specs describe separate replicas that are both still using cluster capacity.
	revisionA := RevisionRoles{Revision: "A", Roles: map[string]*leaderworkersetv1.LeaderWorkerSet{
		testUtilsRolePrefill: makeRole(5, 6),
		testUtilsRoleDecode:  makeRole(2, 3),
	}}
	revisionB := RevisionRoles{Revision: "B", Roles: map[string]*leaderworkersetv1.LeaderWorkerSet{
		testUtilsRolePrefill: makeRole(3, 6),
		testUtilsRoleDecode:  makeRole(2, 3),
	}}

	t.Run("successive revisions share one initial baseline", func(t *testing.T) {
		oldRevisions := RevisionRolesList{revisionA, revisionB}

		assert.Equal(t, 6, oldRevisions.GetMaxInitialReplicasPerRole(testUtilsRolePrefill), "use max(6, 6), not 6+6")
		assert.Equal(t, 3, oldRevisions.GetMaxInitialReplicasPerRole(testUtilsRoleDecode), "use max(3, 3), not 3+3")
		assert.Equal(t, 8, oldRevisions.GetTotalReplicasPerRole(testUtilsRolePrefill), "physical replicas are A.spec(5) + B.spec(3)")
		assert.Equal(t, 4, oldRevisions.GetTotalReplicasPerRole(testUtilsRoleDecode), "physical replicas are A.spec(2) + B.spec(2)")
	})

	t.Run("removing a partial revision changes only the physical total", func(t *testing.T) {
		oldRevisions := RevisionRolesList{revisionA}

		assert.Equal(t, 6, oldRevisions.GetMaxInitialReplicasPerRole(testUtilsRolePrefill), "the baseline comes from A's initial-replicas, not its partial Spec of 5")
		assert.Equal(t, 3, oldRevisions.GetMaxInitialReplicasPerRole(testUtilsRoleDecode), "the baseline comes from A's initial-replicas, not its partial Spec of 2")
		assert.Equal(t, 5, oldRevisions.GetTotalReplicasPerRole(testUtilsRolePrefill), "only A's physical replicas remain")
		assert.Equal(t, 2, oldRevisions.GetTotalReplicasPerRole(testUtilsRoleDecode), "only A's physical replicas remain")
	})
}

func TestSliceLabelMatches(t *testing.T) {
	cases := []struct {
		name   string
		labels map[string]string
		slice  int
		want   bool
	}{
		{"all slices matches label-less", nil, -1, true},
		{"all slices matches labeled", map[string]string{disaggregatedsetv1.SliceLabelKey: "3"}, -1, true},
		{"slice 0 matches legacy label-less", nil, 0, true},
		{"slice 0 matches empty label", map[string]string{disaggregatedsetv1.SliceLabelKey: ""}, 0, true},
		{"slice 0 matches slice 0", map[string]string{disaggregatedsetv1.SliceLabelKey: "0"}, 0, true},
		{"slice 0 does not match slice 1", map[string]string{disaggregatedsetv1.SliceLabelKey: "1"}, 0, false},
		{"slice 1 does not match legacy label-less", nil, 1, false},
		{"slice 1 matches slice 1", map[string]string{disaggregatedsetv1.SliceLabelKey: "1"}, 1, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, SliceLabelMatches(tc.labels, tc.slice))
		})
	}
}

func revisionRoleLWS(revision, role string, replicas *int32, initialReplicas string) *leaderworkersetv1.LeaderWorkerSet {
	lws := &leaderworkersetv1.LeaderWorkerSet{
		ObjectMeta: metav1.ObjectMeta{
			Name: revision + "-" + role,
			Labels: map[string]string{
				disaggregatedsetv1.RevisionLabelKey: revision,
				disaggregatedsetv1.RoleLabelKey:     role,
			},
		},
		Spec: leaderworkersetv1.LeaderWorkerSetSpec{Replicas: replicas},
	}
	if initialReplicas != "" {
		lws.Annotations = map[string]string{disaggregatedsetv1.InitialReplicasAnnotationKey: initialReplicas}
	}
	return lws
}

func TestGroupByRevision(t *testing.T) {
	t.Run("groups roles under their revision", func(t *testing.T) {
		prefillA := revisionRoleLWS("rev-a", testUtilsRolePrefill, ptr.To[int32](2), "")
		decodeA := revisionRoleLWS("rev-a", testUtilsRoleDecode, ptr.To[int32](3), "")
		prefillB := revisionRoleLWS("rev-b", testUtilsRolePrefill, ptr.To[int32](1), "")

		grouped := GroupByRevision([]*leaderworkersetv1.LeaderWorkerSet{prefillA, decodeA, prefillB})

		require.Len(t, grouped, 2)
		byRevision := map[string]RevisionRoles{}
		for _, g := range grouped {
			byRevision[g.Revision] = g
		}
		require.Contains(t, byRevision, "rev-a")
		require.Contains(t, byRevision, "rev-b")
		assert.Same(t, prefillA, byRevision["rev-a"].Roles[testUtilsRolePrefill])
		assert.Same(t, decodeA, byRevision["rev-a"].Roles[testUtilsRoleDecode])
		assert.Len(t, byRevision["rev-b"].Roles, 1)
		assert.Same(t, prefillB, byRevision["rev-b"].Roles[testUtilsRolePrefill])
	})

	t.Run("empty input yields no revisions", func(t *testing.T) {
		assert.Empty(t, GroupByRevision(nil))
	})

	t.Run("a later object with the same revision and role replaces the earlier one", func(t *testing.T) {
		first := revisionRoleLWS("rev-a", testUtilsRolePrefill, ptr.To[int32](1), "")
		second := revisionRoleLWS("rev-a", testUtilsRolePrefill, ptr.To[int32](2), "")

		grouped := GroupByRevision([]*leaderworkersetv1.LeaderWorkerSet{first, second})

		require.Len(t, grouped, 1)
		assert.Same(t, second, grouped[0].Roles[testUtilsRolePrefill])
	})
}

func TestRevisionRolesLatestCreationTime(t *testing.T) {
	earlier := time.Date(2026, time.September, 17, 10, 0, 0, 0, time.UTC)
	later := earlier.Add(time.Minute)
	revision := RevisionRoles{Roles: map[string]*leaderworkersetv1.LeaderWorkerSet{
		testUtilsRolePrefill: {ObjectMeta: metav1.ObjectMeta{CreationTimestamp: metav1.NewTime(earlier)}},
		testUtilsRoleDecode:  {ObjectMeta: metav1.ObjectMeta{CreationTimestamp: metav1.NewTime(later)}},
	}}

	assert.Equal(t, later, revision.LatestCreationTime())
	assert.True(t, (RevisionRoles{}).LatestCreationTime().IsZero())
}

func TestRevisionRolesListSortedByNewestTimestamp(t *testing.T) {
	base := time.Date(2026, time.September, 17, 10, 0, 0, 0, time.UTC)
	revisions := RevisionRolesList{
		{Revision: "tie-b", Roles: map[string]*leaderworkersetv1.LeaderWorkerSet{
			testUtilsRolePrefill: {ObjectMeta: metav1.ObjectMeta{CreationTimestamp: metav1.NewTime(base.Add(2 * time.Minute))}},
		}},
		{Revision: "oldest", Roles: map[string]*leaderworkersetv1.LeaderWorkerSet{
			testUtilsRolePrefill: {ObjectMeta: metav1.ObjectMeta{CreationTimestamp: metav1.NewTime(base)}},
		}},
		{Revision: "newest", Roles: map[string]*leaderworkersetv1.LeaderWorkerSet{
			testUtilsRolePrefill: {ObjectMeta: metav1.ObjectMeta{CreationTimestamp: metav1.NewTime(base.Add(2 * time.Minute))}},
		}},
		{Revision: "tie-a", Roles: map[string]*leaderworkersetv1.LeaderWorkerSet{
			testUtilsRolePrefill: {ObjectMeta: metav1.ObjectMeta{CreationTimestamp: metav1.NewTime(base.Add(2 * time.Minute))}},
		}},
		{Revision: "middle", Roles: map[string]*leaderworkersetv1.LeaderWorkerSet{
			testUtilsRolePrefill: {ObjectMeta: metav1.ObjectMeta{CreationTimestamp: metav1.NewTime(base.Add(time.Minute))}},
		}},
	}

	sorted := revisions.SortedByNewestTimestamp()

	assert.Equal(t, []string{"newest", "tie-a", "tie-b", "middle", "oldest"},
		[]string{sorted[0].Revision, sorted[1].Revision, sorted[2].Revision, sorted[3].Revision, sorted[4].Revision})
	assert.Equal(t, []string{"tie-b", "oldest", "newest", "tie-a", "middle"},
		[]string{revisions[0].Revision, revisions[1].Revision, revisions[2].Revision, revisions[3].Revision, revisions[4].Revision})
	assert.Empty(t, (RevisionRolesList(nil)).SortedByNewestTimestamp())
}

func TestRevisionRolesListTotals(t *testing.T) {
	revisions := GroupByRevision([]*leaderworkersetv1.LeaderWorkerSet{
		revisionRoleLWS("rev-a", testUtilsRolePrefill, ptr.To[int32](2), "5"),
		revisionRoleLWS("rev-a", testUtilsRoleDecode, ptr.To[int32](3), ""),
		revisionRoleLWS("rev-b", testUtilsRolePrefill, nil, ""),
		revisionRoleLWS("rev-c", testUtilsRolePrefill, ptr.To[int32](4), "not-a-number"),
	})

	t.Run("sums replicas per role across revisions and defaults nil replicas to one", func(t *testing.T) {
		// rev-a prefill 2 + rev-b prefill nil→1 + rev-c prefill 4
		assert.Equal(t, 7, revisions.GetTotalReplicasPerRole(testUtilsRolePrefill))
		assert.Equal(t, 3, revisions.GetTotalReplicasPerRole(testUtilsRoleDecode))
	})

	t.Run("a role missing from every revision totals zero", func(t *testing.T) {
		assert.Equal(t, 0, revisions.GetTotalReplicasPerRole("unknown"))
	})

	t.Run("initial replicas use the maximum revision target and fall back to spec replicas", func(t *testing.T) {
		// max(rev-a annotation 5, rev-b nil→1, rev-c invalid annotation→spec 4)
		assert.Equal(t, 5, revisions.GetMaxInitialReplicasPerRole(testUtilsRolePrefill))
		// decode has no annotation anywhere, so its Spec is the only target.
		assert.Equal(t, 3, revisions.GetMaxInitialReplicasPerRole(testUtilsRoleDecode))
	})
}

func TestGetSlices(t *testing.T) {
	t.Run("defaults to one slice when unset", func(t *testing.T) {
		assert.Equal(t, int32(1), GetSlices(&disaggregatedsetv1.DisaggregatedSet{}))
	})

	t.Run("returns the configured slice count", func(t *testing.T) {
		ds := &disaggregatedsetv1.DisaggregatedSet{}
		ds.Spec.Slices = ptr.To[int32](3)
		assert.Equal(t, int32(3), GetSlices(ds))
	})
}

func TestHasSliceLabel(t *testing.T) {
	assert.False(t, HasSliceLabel(nil), "nil labels")
	assert.False(t, HasSliceLabel(map[string]string{}), "no slice label")
	assert.False(t, HasSliceLabel(map[string]string{disaggregatedsetv1.SliceLabelKey: ""}), "empty slice label is a legacy object")
	assert.True(t, HasSliceLabel(map[string]string{disaggregatedsetv1.SliceLabelKey: "0"}), "slice 0 still counts as labelled")
}

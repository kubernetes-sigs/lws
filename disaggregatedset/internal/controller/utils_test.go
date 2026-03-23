package controller

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	leaderworkerset "sigs.k8s.io/lws/api/leaderworkerset/v1"

	disaggv1alpha1 "sigs.k8s.io/disaggregatedset/api/v1alpha1"
)

// Test-local role names for utils_test.go
const (
	testUtilsRolePrefill = "prefill"
	testUtilsRoleDecode  = "decode"
)

func TestUtilityFunctions(t *testing.T) {
	reconciler := &DisaggregatedSetReconciler{}

	// Test setOwnerReference
	t.Run("setOwnerReference", func(t *testing.T) {
		disaggregatedSet := &disaggv1alpha1.DisaggregatedSet{
			ObjectMeta: metav1.ObjectMeta{
				Name: "test-deployment",
				UID:  types.UID("test-uid-123"),
			},
		}

		replicaSet := &appsv1.ReplicaSet{
			ObjectMeta: metav1.ObjectMeta{
				Name: "test-replicaset",
			},
		}

		reconciler.setOwnerReference(replicaSet, disaggregatedSet)

		require.Len(t, replicaSet.OwnerReferences, 1, "should have 1 owner reference")

		ownerRef := replicaSet.OwnerReferences[0]
		assert.Equal(t, "test-deployment", ownerRef.Name, "owner reference name")
		assert.Equal(t, types.UID("test-uid-123"), ownerRef.UID, "owner reference UID")
		assert.Equal(t, "DisaggregatedSet", ownerRef.Kind, "owner reference kind")
		assert.True(t, *ownerRef.Controller, "owner reference should be a controller")

		// Test that duplicate owner references are not added
		reconciler.setOwnerReference(replicaSet, disaggregatedSet)

		assert.Len(t, replicaSet.OwnerReferences, 1, "should still have 1 owner reference after duplicate call")
	})

}

func TestGetInitialReplicas(t *testing.T) {
	t.Run("returns parsed int from valid annotation", func(t *testing.T) {
		leaderWorkerSet := &leaderworkerset.LeaderWorkerSet{
			ObjectMeta: metav1.ObjectMeta{
				Name: "test-lws",
				Annotations: map[string]string{
					AnnotationInitialReplicas: "5",
				},
			},
		}

		replicas, ok := GetInitialReplicas(leaderWorkerSet)
		require.True(t, ok, "should return ok=true for valid annotation")
		assert.Equal(t, int32(5), replicas, "replicas should be 5")
	})

	t.Run("returns zero and false for missing annotation", func(t *testing.T) {
		leaderWorkerSet := &leaderworkerset.LeaderWorkerSet{
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
		leaderWorkerSet := &leaderworkerset.LeaderWorkerSet{
			ObjectMeta: metav1.ObjectMeta{
				Name: "test-lws",
			},
		}

		replicas, ok := GetInitialReplicas(leaderWorkerSet)
		assert.False(t, ok, "should return ok=false for nil annotations")
		assert.Equal(t, int32(0), replicas, "replicas should be 0")
	})

	t.Run("returns zero and false for invalid annotation value", func(t *testing.T) {
		leaderWorkerSet := &leaderworkerset.LeaderWorkerSet{
			ObjectMeta: metav1.ObjectMeta{
				Name: "test-lws",
				Annotations: map[string]string{
					AnnotationInitialReplicas: "not-a-number",
				},
			},
		}

		replicas, ok := GetInitialReplicas(leaderWorkerSet)
		assert.False(t, ok, "should return ok=false for invalid annotation")
		assert.Equal(t, int32(0), replicas, "replicas should be 0")
	})

	t.Run("returns zero and false for empty annotation value", func(t *testing.T) {
		leaderWorkerSet := &leaderworkerset.LeaderWorkerSet{
			ObjectMeta: metav1.ObjectMeta{
				Name: "test-lws",
				Annotations: map[string]string{
					AnnotationInitialReplicas: "",
				},
			},
		}

		replicas, ok := GetInitialReplicas(leaderWorkerSet)
		assert.False(t, ok, "should return ok=false for empty annotation")
		assert.Equal(t, int32(0), replicas, "replicas should be 0")
	})

	t.Run("handles zero value annotation", func(t *testing.T) {
		leaderWorkerSet := &leaderworkerset.LeaderWorkerSet{
			ObjectMeta: metav1.ObjectMeta{
				Name: "test-lws",
				Annotations: map[string]string{
					AnnotationInitialReplicas: "0",
				},
			},
		}

		replicas, ok := GetInitialReplicas(leaderWorkerSet)
		require.True(t, ok, "should return ok=true for zero value annotation")
		assert.Equal(t, int32(0), replicas, "replicas should be 0")
	})
}

func TestSetInitialReplicas(t *testing.T) {
	t.Run("sets annotation as string on LWS with nil annotations", func(t *testing.T) {
		leaderWorkerSet := &leaderworkerset.LeaderWorkerSet{
			ObjectMeta: metav1.ObjectMeta{
				Name: "test-lws",
			},
			Spec: leaderworkerset.LeaderWorkerSetSpec{
				Replicas: ptr.To(int32(3)),
			},
		}

		SetInitialReplicas(leaderWorkerSet, 3)

		require.NotNil(t, leaderWorkerSet.Annotations, "annotations should be initialized")
		assert.Equal(t, "3", leaderWorkerSet.Annotations[AnnotationInitialReplicas], "annotation should be '3'")
	})

	t.Run("sets annotation as string on LWS with existing annotations", func(t *testing.T) {
		leaderWorkerSet := &leaderworkerset.LeaderWorkerSet{
			ObjectMeta: metav1.ObjectMeta{
				Name: "test-lws",
				Annotations: map[string]string{
					"other-key": "other-value",
				},
			},
			Spec: leaderworkerset.LeaderWorkerSetSpec{
				Replicas: ptr.To(int32(5)),
			},
		}

		SetInitialReplicas(leaderWorkerSet, 5)

		assert.Equal(t, "5", leaderWorkerSet.Annotations[AnnotationInitialReplicas], "annotation should be '5'")
		assert.Equal(t, "other-value", leaderWorkerSet.Annotations["other-key"], "other annotations should be preserved")
	})

	t.Run("overwrites existing annotation", func(t *testing.T) {
		leaderWorkerSet := &leaderworkerset.LeaderWorkerSet{
			ObjectMeta: metav1.ObjectMeta{
				Name: "test-lws",
				Annotations: map[string]string{
					AnnotationInitialReplicas: "10",
				},
			},
			Spec: leaderworkerset.LeaderWorkerSetSpec{
				Replicas: ptr.To(int32(7)),
			},
		}

		SetInitialReplicas(leaderWorkerSet, 7)

		assert.Equal(t, "7", leaderWorkerSet.Annotations[AnnotationInitialReplicas], "annotation should be '7'")
	})

	t.Run("handles zero replicas", func(t *testing.T) {
		leaderWorkerSet := &leaderworkerset.LeaderWorkerSet{
			ObjectMeta: metav1.ObjectMeta{
				Name: "test-lws",
			},
			Spec: leaderworkerset.LeaderWorkerSetSpec{
				Replicas: ptr.To(int32(0)),
			},
		}

		SetInitialReplicas(leaderWorkerSet, 0)

		assert.Equal(t, "0", leaderWorkerSet.Annotations[AnnotationInitialReplicas], "annotation should be '0'")
	})
}

func TestComputeInitialReplicaState(t *testing.T) {
	t.Run("returns empty map for empty list", func(t *testing.T) {
		lwsList := []leaderworkerset.LeaderWorkerSet{}

		state := ComputeInitialReplicaState(lwsList)

		assert.Equal(t, 0, state[testUtilsRolePrefill], "prefill should be 0 for empty list")
		assert.Equal(t, 0, state[testUtilsRoleDecode], "decode should be 0 for empty list")
	})

	t.Run("sums prefill annotations correctly", func(t *testing.T) {
		lwsList := []leaderworkerset.LeaderWorkerSet{
			{
				ObjectMeta: metav1.ObjectMeta{
					Name: "lws-1",
					Labels: map[string]string{
						LabelDisaggRole: testUtilsRolePrefill,
					},
					Annotations: map[string]string{
						AnnotationInitialReplicas: "3",
					},
				},
				Spec: leaderworkerset.LeaderWorkerSetSpec{
					Replicas: ptr.To(int32(3)),
				},
			},
			{
				ObjectMeta: metav1.ObjectMeta{
					Name: "lws-2",
					Labels: map[string]string{
						LabelDisaggRole: testUtilsRolePrefill,
					},
					Annotations: map[string]string{
						AnnotationInitialReplicas: "2",
					},
				},
				Spec: leaderworkerset.LeaderWorkerSetSpec{
					Replicas: ptr.To(int32(2)),
				},
			},
		}

		state := ComputeInitialReplicaState(lwsList)

		assert.Equal(t, 5, state[testUtilsRolePrefill], "prefill should be 5 (3+2)")
		assert.Equal(t, 0, state[testUtilsRoleDecode], "decode should be 0")
	})

	t.Run("sums decode annotations correctly", func(t *testing.T) {
		lwsList := []leaderworkerset.LeaderWorkerSet{
			{
				ObjectMeta: metav1.ObjectMeta{
					Name: "lws-1",
					Labels: map[string]string{
						LabelDisaggRole: testUtilsRoleDecode,
					},
					Annotations: map[string]string{
						AnnotationInitialReplicas: "4",
					},
				},
				Spec: leaderworkerset.LeaderWorkerSetSpec{
					Replicas: ptr.To(int32(4)),
				},
			},
			{
				ObjectMeta: metav1.ObjectMeta{
					Name: "lws-2",
					Labels: map[string]string{
						LabelDisaggRole: testUtilsRoleDecode,
					},
					Annotations: map[string]string{
						AnnotationInitialReplicas: "6",
					},
				},
				Spec: leaderworkerset.LeaderWorkerSetSpec{
					Replicas: ptr.To(int32(6)),
				},
			},
		}

		state := ComputeInitialReplicaState(lwsList)

		assert.Equal(t, 0, state[testUtilsRolePrefill], "prefill should be 0")
		assert.Equal(t, 10, state[testUtilsRoleDecode], "decode should be 10 (4+6)")
	})

	t.Run("sums mixed prefill and decode correctly", func(t *testing.T) {
		lwsList := []leaderworkerset.LeaderWorkerSet{
			{
				ObjectMeta: metav1.ObjectMeta{
					Name: "lws-prefill-1",
					Labels: map[string]string{
						LabelDisaggRole: testUtilsRolePrefill,
					},
					Annotations: map[string]string{
						AnnotationInitialReplicas: "3",
					},
				},
				Spec: leaderworkerset.LeaderWorkerSetSpec{
					Replicas: ptr.To(int32(3)),
				},
			},
			{
				ObjectMeta: metav1.ObjectMeta{
					Name: "lws-decode-1",
					Labels: map[string]string{
						LabelDisaggRole: testUtilsRoleDecode,
					},
					Annotations: map[string]string{
						AnnotationInitialReplicas: "6",
					},
				},
				Spec: leaderworkerset.LeaderWorkerSetSpec{
					Replicas: ptr.To(int32(6)),
				},
			},
			{
				ObjectMeta: metav1.ObjectMeta{
					Name: "lws-prefill-2",
					Labels: map[string]string{
						LabelDisaggRole: testUtilsRolePrefill,
					},
					Annotations: map[string]string{
						AnnotationInitialReplicas: "2",
					},
				},
				Spec: leaderworkerset.LeaderWorkerSetSpec{
					Replicas: ptr.To(int32(2)),
				},
			},
		}

		state := ComputeInitialReplicaState(lwsList)

		assert.Equal(t, 5, state[testUtilsRolePrefill], "prefill should be 5 (3+2)")
		assert.Equal(t, 6, state[testUtilsRoleDecode], "decode should be 6")
	})

	t.Run("uses spec.Replicas fallback for missing annotation", func(t *testing.T) {
		lwsList := []leaderworkerset.LeaderWorkerSet{
			{
				ObjectMeta: metav1.ObjectMeta{
					Name: "lws-1",
					Labels: map[string]string{
						LabelDisaggRole: testUtilsRolePrefill,
					},
					// No annotations
				},
				Spec: leaderworkerset.LeaderWorkerSetSpec{
					Replicas: ptr.To(int32(4)),
				},
			},
		}

		state := ComputeInitialReplicaState(lwsList)

		assert.Equal(t, 4, state[testUtilsRolePrefill], "prefill should be 4 (from spec.Replicas fallback)")
	})

	t.Run("uses spec.Replicas fallback for invalid annotation", func(t *testing.T) {
		lwsList := []leaderworkerset.LeaderWorkerSet{
			{
				ObjectMeta: metav1.ObjectMeta{
					Name: "lws-1",
					Labels: map[string]string{
						LabelDisaggRole: testUtilsRoleDecode,
					},
					Annotations: map[string]string{
						AnnotationInitialReplicas: "not-a-number",
					},
				},
				Spec: leaderworkerset.LeaderWorkerSetSpec{
					Replicas: ptr.To(int32(5)),
				},
			},
		}

		state := ComputeInitialReplicaState(lwsList)

		assert.Equal(t, 5, state[testUtilsRoleDecode], "decode should be 5 (from spec.Replicas fallback)")
	})

	t.Run("handles mixed valid and invalid annotations", func(t *testing.T) {
		lwsList := []leaderworkerset.LeaderWorkerSet{
			{
				ObjectMeta: metav1.ObjectMeta{
					Name: "lws-1",
					Labels: map[string]string{
						LabelDisaggRole: testUtilsRolePrefill,
					},
					Annotations: map[string]string{
						AnnotationInitialReplicas: "3",
					},
				},
				Spec: leaderworkerset.LeaderWorkerSetSpec{
					Replicas: ptr.To(int32(3)),
				},
			},
			{
				ObjectMeta: metav1.ObjectMeta{
					Name: "lws-2",
					Labels: map[string]string{
						LabelDisaggRole: testUtilsRolePrefill,
					},
					Annotations: map[string]string{
						AnnotationInitialReplicas: "invalid",
					},
				},
				Spec: leaderworkerset.LeaderWorkerSetSpec{
					Replicas: ptr.To(int32(2)),
				},
			},
		}

		state := ComputeInitialReplicaState(lwsList)

		// 3 from valid annotation + 2 from spec.Replicas fallback
		assert.Equal(t, 5, state[testUtilsRolePrefill], "prefill should be 5 (3 from valid annotation + 2 from fallback)")
	})

	t.Run("handles nil spec.Replicas with missing annotation", func(t *testing.T) {
		lwsList := []leaderworkerset.LeaderWorkerSet{
			{
				ObjectMeta: metav1.ObjectMeta{
					Name: "lws-1",
					Labels: map[string]string{
						LabelDisaggRole: testUtilsRolePrefill,
					},
				},
				Spec: leaderworkerset.LeaderWorkerSetSpec{
					// nil Replicas defaults to 1
				},
			},
		}

		state := ComputeInitialReplicaState(lwsList)

		// Default is 1 when Replicas is nil
		assert.Equal(t, 1, state[testUtilsRolePrefill], "prefill should be 1 (default when Replicas is nil)")
	})
}

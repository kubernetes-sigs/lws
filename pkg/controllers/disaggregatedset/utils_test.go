package disaggregatedset

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	disaggregatedsetv1 "sigs.k8s.io/lws/api/disaggregatedset/v1"
)

func TestUtilityFunctions(t *testing.T) {
	reconciler := &DisaggregatedSetReconciler{}

	t.Run("setOwnerReference", func(t *testing.T) {
		disaggregatedSet := &disaggregatedsetv1.DisaggregatedSet{
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

		reconciler.setOwnerReference(replicaSet, disaggregatedSet)

		assert.Len(t, replicaSet.OwnerReferences, 1, "should still have 1 owner reference after duplicate call")
	})
}

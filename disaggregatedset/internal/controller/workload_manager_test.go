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

package controller

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	leaderworkerset "sigs.k8s.io/lws/api/leaderworkerset/v1"

	disaggv1alpha1 "sigs.k8s.io/disaggregatedset/api/v1alpha1"
)

// createTestLWSWithAnnotation creates a LeaderWorkerSet for testing with optional annotations.
func createTestLWSWithAnnotation(
	name, namespace string,
	replicas int32,
	annotations map[string]string,
) *leaderworkerset.LeaderWorkerSet {
	return &leaderworkerset.LeaderWorkerSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   namespace,
			Annotations: annotations,
			Labels: map[string]string{
				LabelDisaggName:  "test-deployment",
				LabelDisaggPhase: "prefill",
				LabelRevision:    "abc123",
			},
		},
		Spec: leaderworkerset.LeaderWorkerSetSpec{
			Replicas: ptr.To(replicas),
		},
	}
}

// TestParseInitialReplicasAnnotation tests the parseInitialReplicasAnnotation function.
func TestParseInitialReplicasAnnotation(t *testing.T) {
	testCases := []struct {
		name        string
		annotations map[string]string
		expected    *int
	}{
		{
			name:        "nil annotations map returns nil",
			annotations: nil,
			expected:    nil,
		},
		{
			name:        "missing annotation returns nil",
			annotations: map[string]string{"other-key": "value"},
			expected:    nil,
		},
		{
			name:        "invalid non-numeric annotation returns nil",
			annotations: map[string]string{AnnotationInitialReplicas: "not-a-number"},
			expected:    nil,
		},
		{
			name:        "valid annotation returns correct value",
			annotations: map[string]string{AnnotationInitialReplicas: "5"},
			expected:    ptr.To(5),
		},
		{
			name:        "zero value annotation returns zero",
			annotations: map[string]string{AnnotationInitialReplicas: "0"},
			expected:    ptr.To(0),
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			leaderWorkerSet := &leaderworkerset.LeaderWorkerSet{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: testCase.annotations,
				},
			}
			result := parseInitialReplicasAnnotation(leaderWorkerSet)
			if testCase.expected == nil {
				require.Nil(t, result)
			} else {
				require.NotNil(t, result)
				require.Equal(t, *testCase.expected, *result)
			}
		})
	}
}

// TestGetLWSReplicas tests the getLWSReplicas function.
func TestGetLWSReplicas(t *testing.T) {
	testCases := []struct {
		name     string
		replicas *int32
		expected int32
	}{
		{
			name:     "nil replicas returns 1",
			replicas: nil,
			expected: 1,
		},
		{
			name:     "set replicas returns correct value",
			replicas: ptr.To(int32(5)),
			expected: 5,
		},
		{
			name:     "zero replicas returns 0",
			replicas: ptr.To(int32(0)),
			expected: 0,
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			leaderWorkerSet := &leaderworkerset.LeaderWorkerSet{
				Spec: leaderworkerset.LeaderWorkerSetSpec{
					Replicas: testCase.replicas,
				},
			}
			result := getLWSReplicas(leaderWorkerSet)
			require.Equal(t, testCase.expected, result)
		})
	}
}

// TestManagerGetInitialReplicas tests the manager's GetInitialReplicas method.
func TestManagerGetInitialReplicas(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, leaderworkerset.AddToScheme(scheme))

	testCases := []struct {
		name          string
		existingLWS   *leaderworkerset.LeaderWorkerSet
		expectError   bool
		expectedValue *int
	}{
		{
			name: "returns nil when annotation not set",
			existingLWS: createTestLWSWithAnnotation(
				"test-lws", "default", 3, nil,
			),
			expectError:   false,
			expectedValue: nil,
		},
		{
			name: "returns value when annotation is set",
			existingLWS: createTestLWSWithAnnotation(
				"test-lws", "default", 3,
				map[string]string{AnnotationInitialReplicas: "5"},
			),
			expectError:   false,
			expectedValue: ptr.To(5),
		},
		{
			name:          "returns error when LWS not found",
			existingLWS:   nil,
			expectError:   true,
			expectedValue: nil,
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			var objects []runtime.Object
			if testCase.existingLWS != nil {
				objects = append(objects, testCase.existingLWS)
			}

			fakeClient := fake.NewClientBuilder().
				WithScheme(scheme).
				WithRuntimeObjects(objects...).
				Build()

			manager := NewLeaderWorkerSetManager(fakeClient)
			result, err := manager.GetInitialReplicas(context.Background(), "default", "test-lws")

			if testCase.expectError {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
				if testCase.expectedValue == nil {
					require.Nil(t, result)
				} else {
					require.NotNil(t, result)
					require.Equal(t, *testCase.expectedValue, *result)
				}
			}
		})
	}
}

// TestManagerGetOrSetInitialReplicas tests the manager's GetOrSetInitialReplicas method.
func TestManagerGetOrSetInitialReplicas(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, leaderworkerset.AddToScheme(scheme))

	t.Run("returns existing value without modifying when annotation exists", func(t *testing.T) {
		existingLWS := createTestLWSWithAnnotation(
			"test-lws", "default", 3,
			map[string]string{AnnotationInitialReplicas: "5"},
		)

		fakeClient := fake.NewClientBuilder().
			WithScheme(scheme).
			WithRuntimeObjects(existingLWS).
			Build()

		manager := NewLeaderWorkerSetManager(fakeClient)
		result, err := manager.GetOrSetInitialReplicas(context.Background(), "default", "test-lws", 10)

		require.NoError(t, err)
		require.Equal(t, 5, result) // Should return existing value, not default
	})

	t.Run("sets and returns default value when annotation missing", func(t *testing.T) {
		existingLWS := createTestLWSWithAnnotation(
			"test-lws", "default", 3, nil,
		)

		fakeClient := fake.NewClientBuilder().
			WithScheme(scheme).
			WithRuntimeObjects(existingLWS).
			Build()

		manager := NewLeaderWorkerSetManager(fakeClient)
		result, err := manager.GetOrSetInitialReplicas(context.Background(), "default", "test-lws", 10)

		require.NoError(t, err)
		require.Equal(t, 10, result) // Should return default value
	})

	t.Run("returns error when LWS not found", func(t *testing.T) {
		fakeClient := fake.NewClientBuilder().
			WithScheme(scheme).
			Build()

		manager := NewLeaderWorkerSetManager(fakeClient)
		_, err := manager.GetOrSetInitialReplicas(context.Background(), "default", "nonexistent", 10)

		require.Error(t, err)
	})
}

// TestManagerUpdateInitialReplicasAnnotation tests the manager's UpdateInitialReplicasAnnotation method.
func TestManagerUpdateInitialReplicasAnnotation(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, leaderworkerset.AddToScheme(scheme))

	t.Run("updates annotation when value differs", func(t *testing.T) {
		existingLWS := createTestLWSWithAnnotation(
			"test-lws", "default", 3,
			map[string]string{AnnotationInitialReplicas: "5"},
		)

		fakeClient := fake.NewClientBuilder().
			WithScheme(scheme).
			WithRuntimeObjects(existingLWS).
			Build()

		manager := NewLeaderWorkerSetManager(fakeClient)
		err := manager.UpdateInitialReplicasAnnotation(context.Background(), "default", "test-lws", 10)

		require.NoError(t, err)
	})

	t.Run("skips update when annotation already has correct value", func(t *testing.T) {
		existingLWS := createTestLWSWithAnnotation(
			"test-lws", "default", 3,
			map[string]string{AnnotationInitialReplicas: "5"},
		)

		fakeClient := fake.NewClientBuilder().
			WithScheme(scheme).
			WithRuntimeObjects(existingLWS).
			Build()

		manager := NewLeaderWorkerSetManager(fakeClient)
		err := manager.UpdateInitialReplicasAnnotation(context.Background(), "default", "test-lws", 5)

		require.NoError(t, err)
	})

	t.Run("returns error when LWS not found", func(t *testing.T) {
		fakeClient := fake.NewClientBuilder().
			WithScheme(scheme).
			Build()

		manager := NewLeaderWorkerSetManager(fakeClient)
		err := manager.UpdateInitialReplicasAnnotation(context.Background(), "default", "nonexistent", 10)

		require.Error(t, err)
	})
}

// TestManagerDelete tests the manager's Delete method.
func TestManagerDelete(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, leaderworkerset.AddToScheme(scheme))

	t.Run("successfully deletes existing LWS", func(t *testing.T) {
		existingLWS := createTestLWSWithAnnotation("test-lws", "default", 3, nil)

		fakeClient := fake.NewClientBuilder().
			WithScheme(scheme).
			WithRuntimeObjects(existingLWS).
			Build()

		manager := NewLeaderWorkerSetManager(fakeClient)
		err := manager.Delete(context.Background(), "default", "test-lws")

		require.NoError(t, err)
	})

	t.Run("returns nil when LWS not found (idempotent)", func(t *testing.T) {
		fakeClient := fake.NewClientBuilder().
			WithScheme(scheme).
			Build()

		manager := NewLeaderWorkerSetManager(fakeClient)
		err := manager.Delete(context.Background(), "default", "nonexistent")

		require.NoError(t, err) // Should not error, deletion is idempotent
	})
}

// TestManagerScale tests the manager's Scale method.
func TestManagerScale(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, leaderworkerset.AddToScheme(scheme))

	t.Run("skips patch when already at desired scale", func(t *testing.T) {
		existingLWS := createTestLWSWithAnnotation("test-lws", "default", 5, nil)

		fakeClient := fake.NewClientBuilder().
			WithScheme(scheme).
			WithRuntimeObjects(existingLWS).
			Build()

		manager := NewLeaderWorkerSetManager(fakeClient)
		err := manager.Scale(context.Background(), "default", "test-lws", 5)

		require.NoError(t, err)
	})

	t.Run("scales to new replica count", func(t *testing.T) {
		existingLWS := createTestLWSWithAnnotation("test-lws", "default", 3, nil)

		fakeClient := fake.NewClientBuilder().
			WithScheme(scheme).
			WithRuntimeObjects(existingLWS).
			Build()

		manager := NewLeaderWorkerSetManager(fakeClient)
		err := manager.Scale(context.Background(), "default", "test-lws", 5)

		require.NoError(t, err)
	})

	t.Run("returns error when LWS not found", func(t *testing.T) {
		fakeClient := fake.NewClientBuilder().
			WithScheme(scheme).
			Build()

		manager := NewLeaderWorkerSetManager(fakeClient)
		err := manager.Scale(context.Background(), "default", "nonexistent", 5)

		require.Error(t, err)
	})
}

// TestManagerSetInitialReplicas tests the manager's SetInitialReplicas method.
func TestManagerSetInitialReplicas(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, leaderworkerset.AddToScheme(scheme))

	t.Run("skips update when value already correct", func(t *testing.T) {
		existingLWS := createTestLWSWithAnnotation(
			"test-lws", "default", 3,
			map[string]string{AnnotationInitialReplicas: "5"},
		)

		fakeClient := fake.NewClientBuilder().
			WithScheme(scheme).
			WithRuntimeObjects(existingLWS).
			Build()

		manager := NewLeaderWorkerSetManager(fakeClient)
		oldValue, err := manager.SetInitialReplicas(context.Background(), "default", "test-lws", 5)

		require.NoError(t, err)
		require.NotNil(t, oldValue)
		require.Equal(t, 5, *oldValue)
	})

	t.Run("updates when overwriting different value", func(t *testing.T) {
		existingLWS := createTestLWSWithAnnotation(
			"test-lws", "default", 3,
			map[string]string{AnnotationInitialReplicas: "5"},
		)

		fakeClient := fake.NewClientBuilder().
			WithScheme(scheme).
			WithRuntimeObjects(existingLWS).
			Build()

		manager := NewLeaderWorkerSetManager(fakeClient)
		oldValue, err := manager.SetInitialReplicas(context.Background(), "default", "test-lws", 10)

		require.NoError(t, err)
		require.NotNil(t, oldValue)
		require.Equal(t, 5, *oldValue)
	})

	t.Run("sets annotation when not present", func(t *testing.T) {
		existingLWS := createTestLWSWithAnnotation("test-lws", "default", 3, nil)

		fakeClient := fake.NewClientBuilder().
			WithScheme(scheme).
			WithRuntimeObjects(existingLWS).
			Build()

		manager := NewLeaderWorkerSetManager(fakeClient)
		oldValue, err := manager.SetInitialReplicas(context.Background(), "default", "test-lws", 5)

		require.NoError(t, err)
		require.Nil(t, oldValue)
	})

	t.Run("returns error when LWS not found", func(t *testing.T) {
		fakeClient := fake.NewClientBuilder().
			WithScheme(scheme).
			Build()

		manager := NewLeaderWorkerSetManager(fakeClient)
		_, err := manager.SetInitialReplicas(context.Background(), "default", "nonexistent", 5)

		require.Error(t, err)
	})
}

// TestManagerCreate tests the manager's Create method.
func TestManagerCreate(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, leaderworkerset.AddToScheme(scheme))
	require.NoError(t, disaggv1alpha1.AddToScheme(scheme))

	t.Run("returns nil when LWS already exists (idempotent)", func(t *testing.T) {
		existingLWS := createTestLWSWithAnnotation("test-deploy-abc123-prefill", "default", 3, nil)

		fakeClient := fake.NewClientBuilder().
			WithScheme(scheme).
			WithRuntimeObjects(existingLWS).
			Build()

		manager := NewLeaderWorkerSetManager(fakeClient)
		params := CreateParams{
			DisaggregatedSet: &disaggv1alpha1.DisaggregatedSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-deploy",
					Namespace: "default",
					UID:       "test-uid",
				},
			},
			Phase:    "prefill",
			Revision: "abc123",
			Replicas: 3,
			Labels: map[string]string{
				LabelDisaggName:  "test-deploy",
				LabelDisaggPhase: "prefill",
				LabelRevision:    "abc123",
			},
			Config: &disaggv1alpha1.DisaggregatedPhaseSpec{
				LeaderWorkerTemplate: leaderworkerset.LeaderWorkerTemplate{
					Size: ptr.To(int32(1)),
				},
			},
		}

		err := manager.Create(context.Background(), params)
		require.NoError(t, err) // Should not error, creation is idempotent
	})

	t.Run("successfully creates new LWS", func(t *testing.T) {
		fakeClient := fake.NewClientBuilder().
			WithScheme(scheme).
			Build()

		manager := NewLeaderWorkerSetManager(fakeClient)
		params := CreateParams{
			DisaggregatedSet: &disaggv1alpha1.DisaggregatedSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-deploy",
					Namespace: "default",
					UID:       "test-uid",
				},
			},
			Phase:    "prefill",
			Revision: "abc123",
			Replicas: 3,
			Labels: map[string]string{
				LabelDisaggName:  "test-deploy",
				LabelDisaggPhase: "prefill",
				LabelRevision:    "abc123",
			},
			Config: &disaggv1alpha1.DisaggregatedPhaseSpec{
				LeaderWorkerTemplate: leaderworkerset.LeaderWorkerTemplate{
					Size: ptr.To(int32(1)),
				},
			},
		}

		err := manager.Create(context.Background(), params)
		require.NoError(t, err)
	})
}

// TestComputeRevision tests the ComputeRevision function.
func TestComputeRevision(t *testing.T) {
	t.Run("returns consistent revision for same inputs", func(t *testing.T) {
		phases := []disaggv1alpha1.DisaggregatedPhaseSpec{
			{
				Name:     "prefill",
				Replicas: ptr.To(int32(2)),
				LeaderWorkerTemplate: leaderworkerset.LeaderWorkerTemplate{
					Size: ptr.To(int32(1)),
				},
			},
			{
				Name:     "decode",
				Replicas: ptr.To(int32(3)),
				LeaderWorkerTemplate: leaderworkerset.LeaderWorkerTemplate{
					Size: ptr.To(int32(1)),
				},
			},
		}

		revision1 := ComputeRevision(phases)
		revision2 := ComputeRevision(phases)

		require.Equal(t, revision1, revision2)
		require.Len(t, revision1, 8) // Truncated to 8 characters
	})

	t.Run("returns different revision for different Size", func(t *testing.T) {
		phases1 := []disaggv1alpha1.DisaggregatedPhaseSpec{
			{
				Name:     "prefill",
				Replicas: ptr.To(int32(2)),
				LeaderWorkerTemplate: leaderworkerset.LeaderWorkerTemplate{
					Size: ptr.To(int32(1)),
				},
			},
			{
				Name:     "decode",
				Replicas: ptr.To(int32(3)),
				LeaderWorkerTemplate: leaderworkerset.LeaderWorkerTemplate{
					Size: ptr.To(int32(1)),
				},
			},
		}
		phases2 := []disaggv1alpha1.DisaggregatedPhaseSpec{
			{
				Name:     "prefill",
				Replicas: ptr.To(int32(2)),
				LeaderWorkerTemplate: leaderworkerset.LeaderWorkerTemplate{
					Size: ptr.To(int32(2)), // Different
				},
			},
			{
				Name:     "decode",
				Replicas: ptr.To(int32(3)),
				LeaderWorkerTemplate: leaderworkerset.LeaderWorkerTemplate{
					Size: ptr.To(int32(1)),
				},
			},
		}

		revision1 := ComputeRevision(phases1)
		revision2 := ComputeRevision(phases2)

		require.NotEqual(t, revision1, revision2)
	})

	t.Run("returns different revision for different phase names", func(t *testing.T) {
		phases1 := []disaggv1alpha1.DisaggregatedPhaseSpec{
			{
				Name:     "prefill",
				Replicas: ptr.To(int32(2)),
				LeaderWorkerTemplate: leaderworkerset.LeaderWorkerTemplate{
					Size: ptr.To(int32(1)),
				},
			},
			{
				Name:     "decode",
				Replicas: ptr.To(int32(3)),
				LeaderWorkerTemplate: leaderworkerset.LeaderWorkerTemplate{
					Size: ptr.To(int32(1)),
				},
			},
		}
		phases2 := []disaggv1alpha1.DisaggregatedPhaseSpec{
			{
				Name:     "other-phase",
				Replicas: ptr.To(int32(2)),
				LeaderWorkerTemplate: leaderworkerset.LeaderWorkerTemplate{
					Size: ptr.To(int32(1)),
				},
			},
			{
				Name:     "decode",
				Replicas: ptr.To(int32(3)),
				LeaderWorkerTemplate: leaderworkerset.LeaderWorkerTemplate{
					Size: ptr.To(int32(1)),
				},
			},
		}

		revision1 := ComputeRevision(phases1)
		revision2 := ComputeRevision(phases2)

		require.NotEqual(t, revision1, revision2)
	})

	t.Run("handles empty phases slice", func(t *testing.T) {
		phases := []disaggv1alpha1.DisaggregatedPhaseSpec{}

		revision := ComputeRevision(phases)
		require.Len(t, revision, 8)
	})
}

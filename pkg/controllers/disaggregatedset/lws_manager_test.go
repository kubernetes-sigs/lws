/*
Copyright 2025 The Kubernetes Authors.

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

package disaggregatedset

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	leaderworkersetv1 "sigs.k8s.io/lws/api/leaderworkerset/v1"

	disaggregatedsetv1 "sigs.k8s.io/lws/api/disaggregatedset/v1"
	disaggregatedsetutils "sigs.k8s.io/lws/pkg/utils/disaggregatedset"
	"sigs.k8s.io/lws/test/wrappers"
)

var managerTestLabels = map[string]string{
	disaggregatedsetv1.SetNameLabelKey:  "test-deployment",
	disaggregatedsetv1.RoleLabelKey:     "prefill",
	disaggregatedsetv1.RevisionLabelKey: "abc123",
}

func buildManagerTestLWS(name string, replicas int32, annotations map[string]string) *leaderworkersetv1.LeaderWorkerSet {
	return wrappers.BuildBasicLeaderWorkerSet(name, "default").
		Labels(managerTestLabels).
		Replica(int(replicas)).
		Annotation(annotations).
		Obj()
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
			annotations: map[string]string{disaggregatedsetv1.InitialReplicasAnnotationKey: "not-a-number"},
			expected:    nil,
		},
		{
			name:        "valid annotation returns correct value",
			annotations: map[string]string{disaggregatedsetv1.InitialReplicasAnnotationKey: "5"},
			expected:    ptr.To(5),
		},
		{
			name:        "zero value annotation returns zero",
			annotations: map[string]string{disaggregatedsetv1.InitialReplicasAnnotationKey: "0"},
			expected:    ptr.To(0),
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			leaderWorkerSet := &leaderworkersetv1.LeaderWorkerSet{
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
			leaderWorkerSet := &leaderworkersetv1.LeaderWorkerSet{
				Spec: leaderworkersetv1.LeaderWorkerSetSpec{
					Replicas: testCase.replicas,
				},
			}
			result := getLWSReplicas(leaderWorkerSet)
			require.Equal(t, testCase.expected, result)
		})
	}
}

// TestManagerDelete tests the manager's Delete method.
func TestManagerDelete(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, leaderworkersetv1.AddToScheme(scheme))

	t.Run("successfully deletes existing LWS", func(t *testing.T) {
		existingLWS := buildManagerTestLWS("test-lws", 3, nil)

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
	require.NoError(t, leaderworkersetv1.AddToScheme(scheme))

	t.Run("skips patch when already at desired scale", func(t *testing.T) {
		existingLWS := buildManagerTestLWS("test-lws", 5, nil)

		fakeClient := fake.NewClientBuilder().
			WithScheme(scheme).
			WithRuntimeObjects(existingLWS).
			Build()

		manager := NewLeaderWorkerSetManager(fakeClient)
		err := manager.Scale(context.Background(), "default", "test-lws", 5)

		require.NoError(t, err)
	})

	t.Run("scales to new replica count", func(t *testing.T) {
		existingLWS := buildManagerTestLWS("test-lws", 3, nil)

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

// TestManagerSetInitialReplicas tests the manager's disaggregatedsetutils.SetInitialReplicas method.
func TestManagerSetInitialReplicas(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, leaderworkersetv1.AddToScheme(scheme))

	t.Run("skips update when value already correct", func(t *testing.T) {
		existingLWS := buildManagerTestLWS(
			"test-lws", 3,
			map[string]string{disaggregatedsetv1.InitialReplicasAnnotationKey: "5"},
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
		existingLWS := buildManagerTestLWS(
			"test-lws", 3,
			map[string]string{disaggregatedsetv1.InitialReplicasAnnotationKey: "5"},
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
		existingLWS := buildManagerTestLWS("test-lws", 3, nil)

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
	require.NoError(t, leaderworkersetv1.AddToScheme(scheme))
	require.NoError(t, disaggregatedsetv1.AddToScheme(scheme))

	t.Run("returns nil when LWS already exists (idempotent)", func(t *testing.T) {
		existingLWS := buildManagerTestLWS("test-deploy-0-abc123-prefill", 3, nil)

		fakeClient := fake.NewClientBuilder().
			WithScheme(scheme).
			WithRuntimeObjects(existingLWS).
			Build()

		manager := NewLeaderWorkerSetManager(fakeClient)
		params := disaggregatedsetutils.CreateParams{
			DisaggregatedSet: &disaggregatedsetv1.DisaggregatedSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-deploy",
					Namespace: "default",
					UID:       "test-uid",
				},
			},
			Role:     "prefill",
			Revision: "abc123",
			Replicas: 3,
			Labels: map[string]string{
				disaggregatedsetv1.SetNameLabelKey:  "test-deploy",
				disaggregatedsetv1.RoleLabelKey:     "prefill",
				disaggregatedsetv1.RevisionLabelKey: "abc123",
			},
			Config: &disaggregatedsetv1.DisaggregatedRoleSpec{
				LeaderWorkerSetTemplateSpec: leaderworkersetv1.LeaderWorkerSetTemplateSpec{Spec: leaderworkersetv1.LeaderWorkerSetSpec{
					LeaderWorkerTemplate: leaderworkersetv1.LeaderWorkerTemplate{
						Size: ptr.To(int32(1)),
					},
				}},
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
		params := disaggregatedsetutils.CreateParams{
			DisaggregatedSet: &disaggregatedsetv1.DisaggregatedSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-deploy",
					Namespace: "default",
					UID:       "test-uid",
				},
			},
			Role:     "prefill",
			Revision: "abc123",
			Replicas: 3,
			Labels: map[string]string{
				disaggregatedsetv1.SetNameLabelKey:  "test-deploy",
				disaggregatedsetv1.RoleLabelKey:     "prefill",
				disaggregatedsetv1.RevisionLabelKey: "abc123",
			},
			Config: &disaggregatedsetv1.DisaggregatedRoleSpec{
				LeaderWorkerSetTemplateSpec: leaderworkersetv1.LeaderWorkerSetTemplateSpec{Spec: leaderworkersetv1.LeaderWorkerSetSpec{
					LeaderWorkerTemplate: leaderworkersetv1.LeaderWorkerTemplate{
						Size: ptr.To(int32(1)),
					},
				}},
			},
		}

		err := manager.Create(context.Background(), params)
		require.NoError(t, err)
	})

	t.Run("merges user metadata with system labels taking precedence", func(t *testing.T) {
		fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
		manager := NewLeaderWorkerSetManager(fakeClient)

		err := manager.Create(context.Background(), disaggregatedsetutils.CreateParams{
			DisaggregatedSet: &disaggregatedsetv1.DisaggregatedSet{
				ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default", UID: "uid"},
			},
			Role: "prefill", Revision: "rev1", Replicas: 1,
			Labels: map[string]string{disaggregatedsetv1.SetNameLabelKey: "test", disaggregatedsetv1.RoleLabelKey: "prefill", "app": "system-app"},
			Config: &disaggregatedsetv1.DisaggregatedRoleSpec{
				LeaderWorkerSetTemplateSpec: leaderworkersetv1.LeaderWorkerSetTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{
						Labels:      map[string]string{"kueue.x-k8s.io/queue-name": "q1", "app": "user-app"},
						Annotations: map[string]string{"note": "val"},
					},
					Spec: leaderworkersetv1.LeaderWorkerSetSpec{
						LeaderWorkerTemplate: leaderworkersetv1.LeaderWorkerTemplate{Size: ptr.To(int32(1))},
					},
				},
			},
		})
		require.NoError(t, err)

		var lws leaderworkersetv1.LeaderWorkerSet
		require.NoError(t, fakeClient.Get(context.Background(),
			client.ObjectKey{Name: "test-0-rev1-prefill", Namespace: "default"}, &lws))

		require.Equal(t, "q1", lws.Labels["kueue.x-k8s.io/queue-name"]) // user label
		require.Equal(t, "system-app", lws.Labels["app"])               // system wins
		require.Equal(t, "val", lws.Annotations["note"])                // user annotation
	})
}

// TestComputeRevision tests the disaggregatedsetutils.ComputeRevision function.
func TestComputeRevision(t *testing.T) {
	t.Run("returns consistent revision for same inputs", func(t *testing.T) {
		roles := []disaggregatedsetv1.DisaggregatedRoleSpec{
			{
				Name: "prefill",
				LeaderWorkerSetTemplateSpec: leaderworkersetv1.LeaderWorkerSetTemplateSpec{Spec: leaderworkersetv1.LeaderWorkerSetSpec{
					Replicas: ptr.To(int32(2)),
					LeaderWorkerTemplate: leaderworkersetv1.LeaderWorkerTemplate{
						Size: ptr.To(int32(1)),
					},
				}},
			},
			{
				Name: "decode",
				LeaderWorkerSetTemplateSpec: leaderworkersetv1.LeaderWorkerSetTemplateSpec{Spec: leaderworkersetv1.LeaderWorkerSetSpec{
					Replicas: ptr.To(int32(3)),
					LeaderWorkerTemplate: leaderworkersetv1.LeaderWorkerTemplate{
						Size: ptr.To(int32(1)),
					},
				}},
			},
		}

		revision1 := disaggregatedsetutils.ComputeRevision(roles)
		revision2 := disaggregatedsetutils.ComputeRevision(roles)

		require.Equal(t, revision1, revision2)
		require.Len(t, revision1, 8) // Truncated to 8 characters
	})

	t.Run("returns different revision for different Size", func(t *testing.T) {
		roles1 := []disaggregatedsetv1.DisaggregatedRoleSpec{
			{
				Name: "prefill",
				LeaderWorkerSetTemplateSpec: leaderworkersetv1.LeaderWorkerSetTemplateSpec{Spec: leaderworkersetv1.LeaderWorkerSetSpec{
					Replicas: ptr.To(int32(2)),
					LeaderWorkerTemplate: leaderworkersetv1.LeaderWorkerTemplate{
						Size: ptr.To(int32(1)),
					},
				}},
			},
			{
				Name: "decode",
				LeaderWorkerSetTemplateSpec: leaderworkersetv1.LeaderWorkerSetTemplateSpec{Spec: leaderworkersetv1.LeaderWorkerSetSpec{
					Replicas: ptr.To(int32(3)),
					LeaderWorkerTemplate: leaderworkersetv1.LeaderWorkerTemplate{
						Size: ptr.To(int32(1)),
					},
				}},
			},
		}
		roles2 := []disaggregatedsetv1.DisaggregatedRoleSpec{
			{
				Name: "prefill",
				LeaderWorkerSetTemplateSpec: leaderworkersetv1.LeaderWorkerSetTemplateSpec{Spec: leaderworkersetv1.LeaderWorkerSetSpec{
					Replicas: ptr.To(int32(2)),
					LeaderWorkerTemplate: leaderworkersetv1.LeaderWorkerTemplate{
						Size: ptr.To(int32(2)), // Different
					},
				}},
			},
			{
				Name: "decode",
				LeaderWorkerSetTemplateSpec: leaderworkersetv1.LeaderWorkerSetTemplateSpec{Spec: leaderworkersetv1.LeaderWorkerSetSpec{
					Replicas: ptr.To(int32(3)),
					LeaderWorkerTemplate: leaderworkersetv1.LeaderWorkerTemplate{
						Size: ptr.To(int32(1)),
					},
				}},
			},
		}

		revision1 := disaggregatedsetutils.ComputeRevision(roles1)
		revision2 := disaggregatedsetutils.ComputeRevision(roles2)

		require.NotEqual(t, revision1, revision2)
	})

	t.Run("returns different revision for different role names", func(t *testing.T) {
		roles1 := []disaggregatedsetv1.DisaggregatedRoleSpec{
			{
				Name: "prefill",
				LeaderWorkerSetTemplateSpec: leaderworkersetv1.LeaderWorkerSetTemplateSpec{Spec: leaderworkersetv1.LeaderWorkerSetSpec{
					Replicas: ptr.To(int32(2)),
					LeaderWorkerTemplate: leaderworkersetv1.LeaderWorkerTemplate{
						Size: ptr.To(int32(1)),
					},
				}},
			},
			{
				Name: "decode",
				LeaderWorkerSetTemplateSpec: leaderworkersetv1.LeaderWorkerSetTemplateSpec{Spec: leaderworkersetv1.LeaderWorkerSetSpec{
					Replicas: ptr.To(int32(3)),
					LeaderWorkerTemplate: leaderworkersetv1.LeaderWorkerTemplate{
						Size: ptr.To(int32(1)),
					},
				}},
			},
		}
		roles2 := []disaggregatedsetv1.DisaggregatedRoleSpec{
			{
				Name: "other-role",
				LeaderWorkerSetTemplateSpec: leaderworkersetv1.LeaderWorkerSetTemplateSpec{Spec: leaderworkersetv1.LeaderWorkerSetSpec{
					Replicas: ptr.To(int32(2)),
					LeaderWorkerTemplate: leaderworkersetv1.LeaderWorkerTemplate{
						Size: ptr.To(int32(1)),
					},
				}},
			},
			{
				Name: "decode",
				LeaderWorkerSetTemplateSpec: leaderworkersetv1.LeaderWorkerSetTemplateSpec{Spec: leaderworkersetv1.LeaderWorkerSetSpec{
					Replicas: ptr.To(int32(3)),
					LeaderWorkerTemplate: leaderworkersetv1.LeaderWorkerTemplate{
						Size: ptr.To(int32(1)),
					},
				}},
			},
		}

		revision1 := disaggregatedsetutils.ComputeRevision(roles1)
		revision2 := disaggregatedsetutils.ComputeRevision(roles2)

		require.NotEqual(t, revision1, revision2)
	})

	t.Run("handles empty roles slice", func(t *testing.T) {
		roles := []disaggregatedsetv1.DisaggregatedRoleSpec{}

		revision := disaggregatedsetutils.ComputeRevision(roles)
		require.Len(t, revision, 8)
	})
}

// TestManagerListSliceBucketing verifies that List buckets a label-less (legacy) LWS
// into slice 0 and excludes it from other slices.
func TestManagerListSliceBucketing(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, leaderworkersetv1.AddToScheme(scheme))

	const ds = "test-deployment"
	sliced := func(name, slice string) *leaderworkersetv1.LeaderWorkerSet {
		return wrappers.BuildBasicLeaderWorkerSet(name, "default").Labels(map[string]string{
			disaggregatedsetv1.SetNameLabelKey: ds,
			disaggregatedsetv1.RoleLabelKey:    "prefill",
			disaggregatedsetv1.SliceLabelKey:   slice,
		}).Obj()
	}
	legacy := wrappers.BuildBasicLeaderWorkerSet("legacy", "default").Labels(map[string]string{
		disaggregatedsetv1.SetNameLabelKey: ds,
		disaggregatedsetv1.RoleLabelKey:    "prefill",
	}).Obj()

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).
		WithRuntimeObjects(sliced("s0", "0"), sliced("s1", "1"), legacy).Build()
	manager := NewLeaderWorkerSetManager(fakeClient)

	names := func(list []*leaderworkersetv1.LeaderWorkerSet) []string {
		out := make([]string, 0, len(list))
		for _, l := range list {
			out = append(out, l.Name)
		}
		return out
	}

	t.Run("slice 0 includes label-less legacy", func(t *testing.T) {
		got, err := manager.List(context.Background(), "default", ds, 0, "")
		require.NoError(t, err)
		require.ElementsMatch(t, []string{"s0", "legacy"}, names(got))
	})

	t.Run("slice 1 excludes legacy", func(t *testing.T) {
		got, err := manager.List(context.Background(), "default", ds, 1, "")
		require.NoError(t, err)
		require.ElementsMatch(t, []string{"s1"}, names(got))
	})

	t.Run("all slices returns everything", func(t *testing.T) {
		got, err := manager.List(context.Background(), "default", ds, -1, "")
		require.NoError(t, err)
		require.ElementsMatch(t, []string{"s0", "s1", "legacy"}, names(got))
	})
}

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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
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

func buildManagerTestLWS(annotations map[string]string) *leaderworkersetv1.LeaderWorkerSet {
	return wrappers.BuildBasicLeaderWorkerSet("test-lws", "default").
		Labels(managerTestLabels).
		Replica(3).
		Annotation(annotations).
		Obj()
}

// testManagerDS returns a minimal DisaggregatedSet fixture for ownership
// checks in Scale/GetForRole tests.
func testManagerDS(name string) *disaggregatedsetv1.DisaggregatedSet {
	return wrappers.BuildDisaggregatedSet(name, "default").Obj()
}

// ownerRefFor builds the controller OwnerReference this package's own
// LeaderWorkerSetManager.Create sets, so fixtures match real objects.
func ownerRefFor(ds *disaggregatedsetv1.DisaggregatedSet) metav1.OwnerReference {
	return metav1.OwnerReference{
		APIVersion: disaggregatedsetv1.GroupVersion.String(),
		Kind:       "DisaggregatedSet",
		Name:       ds.Name,
		UID:        ds.UID,
		Controller: ptr.To(true),
	}
}

// buildOwnedManagerTestLWS is buildManagerTestLWS plus a controller
// OwnerReference to owner, for ownership-check tests.
func buildOwnedManagerTestLWS(name string, replicas int32, owner *disaggregatedsetv1.DisaggregatedSet) *leaderworkersetv1.LeaderWorkerSet {
	return wrappers.BuildBasicLeaderWorkerSet(name, "default").
		Labels(managerTestLabels).
		Replica(int(replicas)).
		OwnerReference(ownerRefFor(owner)).
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
		existingLWS := buildManagerTestLWS(nil)

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
	ds := testManagerDS("test-deployment")

	t.Run("skips patch when already at desired scale", func(t *testing.T) {
		existingLWS := buildOwnedManagerTestLWS("test-lws", 5, ds)

		fakeClient := fake.NewClientBuilder().
			WithScheme(scheme).
			WithRuntimeObjects(existingLWS).
			Build()

		manager := NewLeaderWorkerSetManager(fakeClient)
		err := manager.Scale(context.Background(), ds, "test-lws", 5)

		require.NoError(t, err)
	})

	t.Run("scales to new replica count", func(t *testing.T) {
		existingLWS := buildOwnedManagerTestLWS("test-lws", 3, ds)

		fakeClient := fake.NewClientBuilder().
			WithScheme(scheme).
			WithRuntimeObjects(existingLWS).
			Build()

		manager := NewLeaderWorkerSetManager(fakeClient)
		err := manager.Scale(context.Background(), ds, "test-lws", 5)

		require.NoError(t, err)
	})

	t.Run("returns error when LWS not found", func(t *testing.T) {
		fakeClient := fake.NewClientBuilder().
			WithScheme(scheme).
			Build()

		manager := NewLeaderWorkerSetManager(fakeClient)
		err := manager.Scale(context.Background(), ds, "nonexistent", 5)

		require.Error(t, err)
	})

	// Regression test for #981: a same-named LWS that exists but is owned by a
	// different DisaggregatedSet (e.g. left over from a same-named
	// DisaggregatedSet that was deleted and recreated before GC ran) must be
	// refused, not mutated.
	t.Run("refuses to scale a foreign-owned LWS with the same name", func(t *testing.T) {
		foreignDS := testManagerDS("some-other-ds")
		foreignLWS := buildOwnedManagerTestLWS("test-lws", 3, foreignDS)

		fakeClient := fake.NewClientBuilder().
			WithScheme(scheme).
			WithRuntimeObjects(foreignLWS).
			Build()

		manager := NewLeaderWorkerSetManager(fakeClient)
		err := manager.Scale(context.Background(), ds, "test-lws", 5)
		require.Error(t, err, "scaling a foreign-owned LWS must be refused")

		var got leaderworkersetv1.LeaderWorkerSet
		require.NoError(t, fakeClient.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "test-lws"}, &got))
		assert.EqualValues(t, 3, *got.Spec.Replicas, "the foreign LWS must not have been mutated")
	})
}

// TestManagerSetInitialReplicas tests the manager's disaggregatedsetutils.SetInitialReplicas method.
func TestManagerSetInitialReplicas(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, leaderworkersetv1.AddToScheme(scheme))

	t.Run("skips update when value already correct", func(t *testing.T) {
		existingLWS := buildManagerTestLWS(
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
		existingLWS := buildManagerTestLWS(nil)

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

	testDeploy := &disaggregatedsetv1.DisaggregatedSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-deploy",
			Namespace: "default",
			UID:       "test-uid",
		},
	}

	t.Run("returns nil when LWS already exists and is owned by this DS (idempotent)", func(t *testing.T) {
		// Represents a concurrent reconcile of this same DisaggregatedSet
		// having already created it.
		existingLWS := buildOwnedManagerTestLWS("test-deploy-0-abc123-prefill", 3, testDeploy)

		fakeClient := fake.NewClientBuilder().
			WithScheme(scheme).
			WithRuntimeObjects(existingLWS).
			Build()

		manager := NewLeaderWorkerSetManager(fakeClient)
		params := disaggregatedsetutils.CreateParams{
			DisaggregatedSet: testDeploy,
			Role:             "prefill",
			Revision:         "abc123",
			Replicas:         3,
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

	// Regression test for #981 (Copilot review on #983): a same-named LWS that
	// exists but is owned by a different DisaggregatedSet must not be silently
	// treated as "already created" — this DS's owned-object watches will never
	// fire for a foreign object, so silently returning nil here could leave the
	// role permanently missing an LWS. Create must error so the reconcile
	// requeues instead.
	t.Run("errors when the name is taken by a foreign-owned LWS", func(t *testing.T) {
		foreignDS := testManagerDS("some-other-ds")
		foreignLWS := buildOwnedManagerTestLWS("test-deploy-0-abc123-prefill", 3, foreignDS)

		fakeClient := fake.NewClientBuilder().
			WithScheme(scheme).
			WithRuntimeObjects(foreignLWS).
			Build()

		manager := NewLeaderWorkerSetManager(fakeClient)
		params := disaggregatedsetutils.CreateParams{
			DisaggregatedSet: testDeploy,
			Role:             "prefill",
			Revision:         "abc123",
			Replicas:         3,
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
		require.Error(t, err, "must not silently no-op when the name is taken by a foreign-owned LWS")
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

	t.Run("injects placement affinity into leader and worker templates", func(t *testing.T) {
		fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
		manager := NewLeaderWorkerSetManager(fakeClient)

		err := manager.Create(context.Background(), disaggregatedsetutils.CreateParams{
			DisaggregatedSet: &disaggregatedsetv1.DisaggregatedSet{
				ObjectMeta: metav1.ObjectMeta{Name: "test-deploy", Namespace: "default", UID: "uid"},
				Spec: disaggregatedsetv1.DisaggregatedSetSpec{
					PlacementPolicy: &disaggregatedsetv1.PlacementPolicy{
						Type:     disaggregatedsetv1.PlacementExclusiveTopology,
						Topology: "topology.example.com/rack",
					},
				},
			},
			Role: "prefill", Slice: 1, Revision: "abc123", Replicas: 2,
			Labels: map[string]string{
				disaggregatedsetv1.SetNameLabelKey: "test-deploy",
				disaggregatedsetv1.RoleLabelKey:    "prefill",
				disaggregatedsetv1.SliceLabelKey:   "1",
			},
			Config: &disaggregatedsetv1.DisaggregatedRoleSpec{
				LeaderWorkerSetTemplateSpec: leaderworkersetv1.LeaderWorkerSetTemplateSpec{Spec: leaderworkersetv1.LeaderWorkerSetSpec{
					LeaderWorkerTemplate: leaderworkersetv1.LeaderWorkerTemplate{
						Size:           ptr.To(int32(2)),
						LeaderTemplate: &corev1.PodTemplateSpec{},
						WorkerTemplate: corev1.PodTemplateSpec{},
					},
				}},
			},
		})
		require.NoError(t, err)

		var lws leaderworkersetv1.LeaderWorkerSet
		require.NoError(t, fakeClient.Get(context.Background(),
			client.ObjectKey{Name: "test-deploy-1-abc123-prefill", Namespace: "default"}, &lws))

		// Both the leader and worker templates must carry the injected placement terms.
		for name, tmpl := range map[string]*corev1.PodTemplateSpec{
			"leader": lws.Spec.LeaderWorkerTemplate.LeaderTemplate,
			"worker": &lws.Spec.LeaderWorkerTemplate.WorkerTemplate,
		} {
			require.NotNil(t, tmpl.Spec.Affinity, "%s affinity", name)
			require.NotNil(t, tmpl.Spec.Affinity.PodAffinity, "%s podAffinity", name)
			require.NotNil(t, tmpl.Spec.Affinity.PodAntiAffinity, "%s podAntiAffinity", name)

			affTerms := tmpl.Spec.Affinity.PodAffinity.RequiredDuringSchedulingIgnoredDuringExecution
			require.Len(t, affTerms, 1, "%s podAffinity terms", name)
			require.Equal(t, "topology.example.com/rack", affTerms[0].TopologyKey, "%s topologyKey", name)
			require.Equal(t, []metav1.LabelSelectorRequirement{
				{Key: disaggregatedsetv1.SetNameLabelKey, Operator: metav1.LabelSelectorOpIn, Values: []string{"test-deploy"}},
				{Key: disaggregatedsetv1.SliceLabelKey, Operator: metav1.LabelSelectorOpIn, Values: []string{"1"}},
			}, affTerms[0].LabelSelector.MatchExpressions, "%s podAffinity selector", name)

			// ExclusiveTopology => same-set spread + cross-set exclusion = two anti-affinity terms.
			require.Len(t, tmpl.Spec.Affinity.PodAntiAffinity.RequiredDuringSchedulingIgnoredDuringExecution, 2, "%s podAntiAffinity terms", name)
		}
	})

	t.Run("no affinity injected without a placement policy", func(t *testing.T) {
		fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
		manager := NewLeaderWorkerSetManager(fakeClient)

		err := manager.Create(context.Background(), disaggregatedsetutils.CreateParams{
			DisaggregatedSet: &disaggregatedsetv1.DisaggregatedSet{
				ObjectMeta: metav1.ObjectMeta{Name: "test-deploy", Namespace: "default", UID: "uid"},
			},
			Role: "prefill", Slice: 0, Revision: "abc123", Replicas: 1,
			Labels: map[string]string{disaggregatedsetv1.SetNameLabelKey: "test-deploy"},
			Config: &disaggregatedsetv1.DisaggregatedRoleSpec{
				LeaderWorkerSetTemplateSpec: leaderworkersetv1.LeaderWorkerSetTemplateSpec{Spec: leaderworkersetv1.LeaderWorkerSetSpec{
					LeaderWorkerTemplate: leaderworkersetv1.LeaderWorkerTemplate{
						Size:           ptr.To(int32(2)),
						LeaderTemplate: &corev1.PodTemplateSpec{},
					},
				}},
			},
		})
		require.NoError(t, err)

		var lws leaderworkersetv1.LeaderWorkerSet
		require.NoError(t, fakeClient.Get(context.Background(),
			client.ObjectKey{Name: "test-deploy-0-abc123-prefill", Namespace: "default"}, &lws))
		require.Nil(t, lws.Spec.LeaderWorkerTemplate.WorkerTemplate.Spec.Affinity, "worker affinity")
		require.Nil(t, lws.Spec.LeaderWorkerTemplate.LeaderTemplate.Spec.Affinity, "leader affinity")
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

	ds := wrappers.BuildDisaggregatedSet("test-deployment", "default").Obj()
	ownerRef := metav1.OwnerReference{
		APIVersion: disaggregatedsetv1.GroupVersion.String(),
		Kind:       "DisaggregatedSet",
		Name:       ds.Name,
		UID:        ds.UID,
		Controller: ptr.To(true),
	}
	sliced := func(name, slice string) *leaderworkersetv1.LeaderWorkerSet {
		return wrappers.BuildBasicLeaderWorkerSet(name, "default").Labels(map[string]string{
			disaggregatedsetv1.SetNameLabelKey: ds.Name,
			disaggregatedsetv1.RoleLabelKey:    "prefill",
			disaggregatedsetv1.SliceLabelKey:   slice,
		}).OwnerReference(ownerRef).Obj()
	}
	legacy := wrappers.BuildBasicLeaderWorkerSet("legacy", "default").Labels(map[string]string{
		disaggregatedsetv1.SetNameLabelKey: ds.Name,
		disaggregatedsetv1.RoleLabelKey:    "prefill",
	}).OwnerReference(ownerRef).Obj()
	// Same name/role labels, but not actually owned by ds (e.g. a leftover from a
	// same-named DisaggregatedSet that was deleted and recreated) — must never be
	// counted as one of ds's own replicas.
	unowned := wrappers.BuildBasicLeaderWorkerSet("unowned", "default").Labels(map[string]string{
		disaggregatedsetv1.SetNameLabelKey: ds.Name,
		disaggregatedsetv1.RoleLabelKey:    "prefill",
		disaggregatedsetv1.SliceLabelKey:   "0",
	}).Obj()

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).
		WithRuntimeObjects(sliced("s0", "0"), sliced("s1", "1"), legacy, unowned).Build()
	manager := NewLeaderWorkerSetManager(fakeClient)

	names := func(list []*leaderworkersetv1.LeaderWorkerSet) []string {
		out := make([]string, 0, len(list))
		for _, l := range list {
			out = append(out, l.Name)
		}
		return out
	}

	t.Run("slice 0 includes label-less legacy, excludes unowned", func(t *testing.T) {
		got, err := manager.List(context.Background(), ds, 0, "")
		require.NoError(t, err)
		require.ElementsMatch(t, []string{"s0", "legacy"}, names(got))
	})

	t.Run("slice 1 excludes legacy", func(t *testing.T) {
		got, err := manager.List(context.Background(), ds, 1, "")
		require.NoError(t, err)
		require.ElementsMatch(t, []string{"s1"}, names(got))
	})

	t.Run("all slices returns everything owned, excludes unowned", func(t *testing.T) {
		got, err := manager.List(context.Background(), ds, -1, "")
		require.NoError(t, err)
		require.ElementsMatch(t, []string{"s0", "s1", "legacy"}, names(got))
	})
}

// TestManagerGetForRoleIgnoresForeignOwnedLWS is a regression test for #981:
// GetForRole must treat a same-named LWS occupying either the slice-aware or
// the legacy name as absent when it's owned by a different DisaggregatedSet
// (e.g. left over from a same-named DisaggregatedSet that was deleted and
// recreated before GC ran), rather than returning it for the caller to scale.
func TestManagerGetForRoleIgnoresForeignOwnedLWS(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, leaderworkersetv1.AddToScheme(scheme))

	ds := testManagerDS("test-deployment")
	foreignDS := testManagerDS("some-other-ds")
	const revision, role = "abc123", "prefill"

	t.Run("slice-aware name occupied by a foreign LWS reads as absent", func(t *testing.T) {
		slice := 0
		name := disaggregatedsetutils.GenerateName(ds.Name, slice, revision, role)
		foreignLWS := buildOwnedManagerTestLWS(name, 3, foreignDS)

		fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(foreignLWS).Build()
		manager := NewLeaderWorkerSetManager(fakeClient)

		got, err := manager.GetForRole(context.Background(), ds, slice, revision, role)
		require.NoError(t, err)
		assert.Nil(t, got, "a foreign-owned LWS at the generated name must not be returned")
	})

	t.Run("legacy name occupied by a foreign LWS reads as absent", func(t *testing.T) {
		slice := 0
		legacyName := disaggregatedsetutils.GenerateLegacyName(ds.Name, revision, role)
		foreignLWS := buildOwnedManagerTestLWS(legacyName, 3, foreignDS)

		fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(foreignLWS).Build()
		manager := NewLeaderWorkerSetManager(fakeClient)

		got, err := manager.GetForRole(context.Background(), ds, slice, revision, role)
		require.NoError(t, err)
		assert.Nil(t, got, "a foreign-owned LWS at the legacy name must not be returned")
	})

	t.Run("owned LWS at the generated name is still returned normally", func(t *testing.T) {
		slice := 0
		name := disaggregatedsetutils.GenerateName(ds.Name, slice, revision, role)
		ownedLWS := buildOwnedManagerTestLWS(name, 3, ds)

		fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(ownedLWS).Build()
		manager := NewLeaderWorkerSetManager(fakeClient)

		got, err := manager.GetForRole(context.Background(), ds, slice, revision, role)
		require.NoError(t, err)
		require.NotNil(t, got)
		assert.Equal(t, name, got.Name)
	})
}

func TestManagerCreateGroupIdentityPassthrough(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, leaderworkersetv1.AddToScheme(scheme))
	require.NoError(t, disaggregatedsetv1.AddToScheme(scheme))

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
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
		Replicas: 2,
		Labels: map[string]string{
			disaggregatedsetv1.SetNameLabelKey:  "test-deploy",
			disaggregatedsetv1.RoleLabelKey:     "prefill",
			disaggregatedsetv1.RevisionLabelKey: "abc123",
		},
		Config: &disaggregatedsetv1.DisaggregatedRoleSpec{
			LeaderWorkerSetTemplateSpec: leaderworkersetv1.LeaderWorkerSetTemplateSpec{Spec: leaderworkersetv1.LeaderWorkerSetSpec{
				GroupIdentity: leaderworkersetv1.GroupIdentityHash,
				LeaderWorkerTemplate: leaderworkersetv1.LeaderWorkerTemplate{
					Size: ptr.To(int32(2)),
				},
			}},
		},
	}

	require.NoError(t, manager.Create(context.Background(), params))

	lwsName := disaggregatedsetutils.GenerateName("test-deploy", params.Slice, "abc123", "prefill")
	lws, err := manager.Get(context.Background(), "default", lwsName)
	require.NoError(t, err)
	require.NotNil(t, lws)
	require.Equal(t, leaderworkersetv1.GroupIdentityHash, lws.Spec.GroupIdentity)
}

func TestComputeRevisionGroupIdentity(t *testing.T) {
	buildRoles := func(groupIdentity leaderworkersetv1.GroupIdentityType) []disaggregatedsetv1.DisaggregatedRoleSpec {
		return []disaggregatedsetv1.DisaggregatedRoleSpec{
			{
				Name: "prefill",
				LeaderWorkerSetTemplateSpec: leaderworkersetv1.LeaderWorkerSetTemplateSpec{Spec: leaderworkersetv1.LeaderWorkerSetSpec{
					GroupIdentity: groupIdentity,
					LeaderWorkerTemplate: leaderworkersetv1.LeaderWorkerTemplate{
						Size: ptr.To(int32(1)),
					},
				}},
			},
		}
	}

	t.Run("empty and explicit Ordinal produce the same revision", func(t *testing.T) {
		// Objects persisted before the field existed must keep their revision
		// once the API server starts defaulting groupIdentity to Ordinal.
		require.Equal(t,
			disaggregatedsetutils.ComputeRevision(buildRoles("")),
			disaggregatedsetutils.ComputeRevision(buildRoles(leaderworkersetv1.GroupIdentityOrdinal)))
	})

	t.Run("Hash produces a different revision", func(t *testing.T) {
		require.NotEqual(t,
			disaggregatedsetutils.ComputeRevision(buildRoles(leaderworkersetv1.GroupIdentityOrdinal)),
			disaggregatedsetutils.ComputeRevision(buildRoles(leaderworkersetv1.GroupIdentityHash)))
	})
}

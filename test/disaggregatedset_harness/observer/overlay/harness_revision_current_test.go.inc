//go:build harness && !harness_legacy_revision

// Copyright 2026 The Kubernetes Authors.
// SPDX-License-Identifier: Apache-2.0
package disaggregatedset

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	disaggv1 "sigs.k8s.io/lws/api/disaggregatedset/v1"
	leaderv1 "sigs.k8s.io/lws/api/leaderworkerset/v1"
	disaggutils "sigs.k8s.io/lws/pkg/utils/disaggregatedset"
)

func TestHarnessTargetRevisionMatchesEnrollmentAndMigration(t *testing.T) {
	for _, marker := range []string{"", disaggv1.RevisionHashVersion, "unsupported"} {
		for _, enrollment := range []string{"none", "slice", "role", "opt-out"} {
			for _, existing := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/existing=%t", marker, enrollment, existing), func(t *testing.T) {
					ds := newTwoRoleTestDisaggregatedSet([2]int32{2, 2}, [2]int{1, 1}, [2]int{})
					ds.Annotations = map[string]string{disaggv1.RevisionHashVersionAnnotationKey: marker}
					switch enrollment {
					case "slice", "opt-out":
						ds.Annotations[disaggv1.AutoComputeDomainAnnotationKey] = leaderv1.AutoComputeDomainEnabled
					case "role":
						ds.Spec.Roles[0].Annotations = map[string]string{leaderv1.AutoComputeDomainAnnotationKey: leaderv1.AutoComputeDomainEnabled}
					}
					if enrollment == "opt-out" {
						ds.Spec.Roles[0].Annotations = map[string]string{leaderv1.AutoComputeDomainAnnotationKey: leaderv1.AutoComputeDomainDisabled}
					}
					before := ds.DeepCopy()
					got, err := harnessTargetRevision(ds, existing)
					require.Equal(t, before, ds, "observer must not migrate or mutate source objects")
					if marker == "unsupported" {
						require.Error(t, err)
						return
					}
					require.NoError(t, err)
					want := disaggutils.ComputeDisaggregatedSetRevision(ds)
					if marker == "" && existing && enrollment == "none" {
						want = disaggutils.ComputeRevisionV1(ds.Spec.Roles)
					}
					require.Equal(t, want, got)
				})
			}
		}
	}
}

func TestHarnessObserveRecognizesEnrolledTarget(t *testing.T) {
	for _, marker := range []string{"", disaggv1.RevisionHashVersion} {
		t.Run(marker, func(t *testing.T) {
			ds := newTwoRoleTestDisaggregatedSet([2]int32{2, 2}, [2]int{1, 1}, [2]int{})
			ds.Annotations = map[string]string{
				disaggv1.RevisionHashVersionAnnotationKey: marker,
				disaggv1.AutoComputeDomainAnnotationKey:   leaderv1.AutoComputeDomainEnabled,
			}
			revision := disaggutils.ComputeDisaggregatedSetRevision(ds)
			require.NotEqual(t, disaggutils.ComputeRevision(ds.Spec.Roles), revision)
			objects := []client.Object{ds}
			for _, role := range testRoleNames() {
				old := revisionLWS("old", role, 1, 1, time.Unix(1000, 0), 2)
				target := revisionLWS(revision, role, 1, 1, time.Unix(2000, 0), 2)
				old.Spec.LeaderWorkerTemplate.Size, target.Spec.LeaderWorkerTemplate.Size = ptr.To[int32](1), ptr.To[int32](1)
				objects = append(objects, replicaGroupObjects(old, 1, 1)...)
				objects = append(objects, replicaGroupObjects(target, 1, 1)...)
			}
			reader := struct{ client.Reader }{newTestClient(objects...)}
			report, err := HarnessObserve(t.Context(), reader, client.ObjectKeyFromObject(ds))
			require.NoError(t, err)
			require.Equal(t, revision, report["targetRevision"])
			candidates := report["slices"].([]map[string]any)[0]["candidates"].([]map[string]any)
			require.Len(t, candidates, 1, "active target must not be classified as old")
			state := candidates[0]["state"].(RolloutState)
			require.Equal(t, RoleReplicaState{1, 1}, state.Target.ReadyReplicas)
		})
	}
}

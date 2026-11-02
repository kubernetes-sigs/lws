//go:build harness

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

package disaggregatedset

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	disaggv1 "sigs.k8s.io/lws/api/disaggregatedset/v1"
	leaderv1 "sigs.k8s.io/lws/api/leaderworkerset/v1"
	"sigs.k8s.io/lws/pkg/replicagroups"
	disaggutils "sigs.k8s.io/lws/pkg/utils/disaggregatedset"
)

func TestHarnessDiagnosticsUseCurrentPlannerBounds(t *testing.T) {
	for _, tc := range []struct {
		name        string
		state       RolloutState
		floors      []int
		ceilings    []int
		allowOutage bool
	}{
		{"growing demand is not capped by old size",
			rolloutState([]int{2, 7}, []int{2, 4}, []int{2, 4}, nil, nil, []int{6, 5}, []int{6, 5}, []int{8, 7}, configs([]int{1, 1}, []int{0, 1})),
			[]int{8, 6}, []int{9, 8}, false},
		{"singleton explicit unavailability permits an outage",
			rolloutState([]int{1, 2}, []int{1, 2}, []int{1, 2}, nil, nil, []int{0, 0}, []int{0, 0}, []int{1, 2}, configs([]int{0, 0}, []int{1, 1})),
			[]int{0, 1}, []int{1, 2}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			report := harnessCandidate([]string{"prefill", "decode"}, tc.state)
			require.Equal(t, tc.allowOutage, report["allowOutage"])
			rows := report["bounds"].([]map[string]any)
			for i := range rows {
				require.Equal(t, tc.floors[i], rows[i]["availabilityFloor"])
				require.Equal(t, tc.ceilings[i], rows[i]["surgeCeiling"])
			}
			require.Equal(t, harnessStep(ComputeNextStep(tc.state)), report["preview"])
		})
	}
}

func TestHarnessObserveCannotInitializeMetadataOrIncludeForeignObjects(t *testing.T) {
	ds := newTwoRoleTestDisaggregatedSet([2]int32{1, 2}, [2]int{0, 0}, [2]int{1, 1})
	foreign := &leaderv1.LeaderWorkerSet{ObjectMeta: metav1.ObjectMeta{
		Name: "foreign", Namespace: ds.Namespace, Labels: map[string]string{disaggv1.SetNameLabelKey: ds.Name},
	}}
	c := newTestClient(ds, foreign)
	// Expose only Read methods, not a client that might accidentally be used
	// for annotation/assignment initialization.
	reader := struct{ client.Reader }{c}
	report, err := HarnessObserve(context.Background(), reader, client.ObjectKeyFromObject(ds))
	require.NoError(t, err)
	require.Empty(t, report["lws"])
	require.Equal(t, disaggutils.ComputeRevision(ds.Spec.Roles), report["targetRevision"])
	current := &disaggv1.DisaggregatedSet{}
	require.NoError(t, c.Get(context.Background(), client.ObjectKeyFromObject(ds), current))
	require.Empty(t, current.Annotations)
	require.Len(t, report["slices"], 1)
	require.NotNil(t, report["slices"].([]map[string]any)[0]["steadyBounds"])
}

func TestHarnessDistinguishesCreationShortfallFromPendingObservations(t *testing.T) {
	for _, tc := range []struct {
		name                    string
		desired, nativeReplicas int32
		deletionsObserved       bool
		warning                 string
	}{
		{"settled empty workload", 0, 0, true, ""},
		{"confirmed creation shortfall", 2, 0, true, "some requested Pods are missing; the controller may still adjust the target"},
		{"Pod observation still behind native status", 2, 2, false, "native scale/deletion acknowledgement is pending"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// No Pods have been observed. Native status distinguishes an actual
			// creation shortfall from an incomplete view of existing Pods.
			s := &replicagroups.Snapshot{
				LWS: &leaderv1.LeaderWorkerSet{Spec: leaderv1.LeaderWorkerSetSpec{Replicas: ptr.To(tc.desired)}},
				LeaderStatefulSet: &appsv1.StatefulSet{
					ObjectMeta: metav1.ObjectMeta{UID: "native"},
					Spec:       appsv1.StatefulSetSpec{Replicas: ptr.To(tc.desired)},
					Status:     appsv1.StatefulSetStatus{Replicas: tc.nativeReplicas},
				},
			}
			row := harnessLWS(s)
			require.Equal(t, tc.desired == 0, row["scalingObserved"])
			require.Equal(t, tc.deletionsObserved, row["deletionsObserved"])
			require.Equal(t, tc.warning, harnessScaleWarning(s))
		})
	}
}

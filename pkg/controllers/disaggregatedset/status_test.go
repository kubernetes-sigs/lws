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
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	disaggregatedsetv1 "sigs.k8s.io/lws/api/disaggregatedset/v1"
)

// TestSetDisaggregatedSetCondition_ObservedGenerationOnlyDoesNotResetTransitionTime
// guards against a condition-transition-time bug: bumping only ObservedGeneration
// while a condition's Status stays the same must not look like a fresh transition
// to clients (metav1.Condition contract), so LastTransitionTime must be preserved.
func TestSetDisaggregatedSetCondition_ObservedGenerationOnlyDoesNotResetTransitionTime(t *testing.T) {
	original := metav1.NewTime(time.Now().Add(-time.Hour))
	ds := &disaggregatedsetv1.DisaggregatedSet{
		Status: disaggregatedsetv1.DisaggregatedSetStatus{
			Conditions: []metav1.Condition{{
				Type:               string(disaggregatedsetv1.DisaggregatedSetAvailable),
				Status:             metav1.ConditionTrue,
				ObservedGeneration: 1,
				LastTransitionTime: original,
				Reason:             "AllRolesReady",
				Message:            "All roles have reached their desired replica count, ready and updated to the current revision",
			}},
		},
	}

	newCondition := metav1.Condition{
		Type:               string(disaggregatedsetv1.DisaggregatedSetAvailable),
		Status:             metav1.ConditionTrue,
		ObservedGeneration: 2, // spec generation advanced, but the condition's Status did not change.
		Reason:             "AllRolesReady",
		Message:            "All roles have reached their desired replica count, ready and updated to the current revision",
	}

	changed := setDisaggregatedSetCondition(ds, newCondition)

	assert.True(t, changed, "ObservedGeneration bump should still be reported as a status change")
	require.Len(t, ds.Status.Conditions, 1)
	assert.EqualValues(t, 2, ds.Status.Conditions[0].ObservedGeneration, "ObservedGeneration should be updated")
	assert.Equal(t, original.Time, ds.Status.Conditions[0].LastTransitionTime.Time, "LastTransitionTime must not change when Status didn't transition")
}

// TestSetDisaggregatedSetCondition_StatusFlipUpdatesTransitionTime verifies the
// opposite case: an actual Status flip (Available -> Progressing) must refresh
// LastTransitionTime on both the newly-true and newly-false conditions.
func TestSetDisaggregatedSetCondition_StatusFlipUpdatesTransitionTime(t *testing.T) {
	original := metav1.NewTime(time.Now().Add(-time.Hour))
	ds := &disaggregatedsetv1.DisaggregatedSet{
		Status: disaggregatedsetv1.DisaggregatedSetStatus{
			Conditions: []metav1.Condition{{
				Type:               string(disaggregatedsetv1.DisaggregatedSetAvailable),
				Status:             metav1.ConditionTrue,
				ObservedGeneration: 1,
				LastTransitionTime: original,
				Reason:             "AllRolesReady",
				Message:            "All roles have reached their desired replica count, ready and updated to the current revision",
			}},
		},
	}

	newCondition := metav1.Condition{
		Type:               string(disaggregatedsetv1.DisaggregatedSetProgressing),
		Status:             metav1.ConditionTrue,
		ObservedGeneration: 1,
		Reason:             "RolloutInProgress",
		Message:            "Not all roles have reached their desired replica count, ready and updated to the current revision",
	}

	changed := setDisaggregatedSetCondition(ds, newCondition)

	assert.True(t, changed)
	require.Len(t, ds.Status.Conditions, 2)

	available := findCondition(ds.Status.Conditions, string(disaggregatedsetv1.DisaggregatedSetAvailable))
	require.NotNil(t, available)
	assert.Equal(t, metav1.ConditionFalse, available.Status)
	assert.True(t, available.LastTransitionTime.Time.After(original.Time), "a real Status flip must refresh LastTransitionTime")
	assert.Equal(t, newCondition.Reason, available.Reason, "the flipped-to-false condition must not keep a Reason that contradicts its new Status")
	assert.Equal(t, newCondition.Message, available.Message)
	assert.EqualValues(t, 1, available.ObservedGeneration)

	progressing := findCondition(ds.Status.Conditions, string(disaggregatedsetv1.DisaggregatedSetProgressing))
	require.NotNil(t, progressing)
	assert.Equal(t, metav1.ConditionTrue, progressing.Status)
}

// TestSetDisaggregatedSetCondition_SameStatusSyncsReasonAndMessage verifies that
// Reason/Message stay in sync with the newly-computed condition even when Status
// doesn't change (only LastTransitionTime is preserved in that case).
func TestSetDisaggregatedSetCondition_SameStatusSyncsReasonAndMessage(t *testing.T) {
	original := metav1.NewTime(time.Now().Add(-time.Hour))
	ds := &disaggregatedsetv1.DisaggregatedSet{
		Status: disaggregatedsetv1.DisaggregatedSetStatus{
			Conditions: []metav1.Condition{{
				Type:               string(disaggregatedsetv1.DisaggregatedSetAvailable),
				Status:             metav1.ConditionTrue,
				ObservedGeneration: 1,
				LastTransitionTime: original,
				Reason:             "Stale",
				Message:            "stale message from a previous reconcile",
			}},
		},
	}

	newCondition := metav1.Condition{
		Type:               string(disaggregatedsetv1.DisaggregatedSetAvailable),
		Status:             metav1.ConditionTrue,
		ObservedGeneration: 1,
		Reason:             "AllRolesReady",
		Message:            "All roles have reached their desired replica count, ready and updated to the current revision",
	}

	changed := setDisaggregatedSetCondition(ds, newCondition)

	assert.True(t, changed)
	require.Len(t, ds.Status.Conditions, 1)
	assert.Equal(t, "AllRolesReady", ds.Status.Conditions[0].Reason)
	assert.Equal(t, newCondition.Message, ds.Status.Conditions[0].Message)
	assert.Equal(t, original.Time, ds.Status.Conditions[0].LastTransitionTime.Time, "LastTransitionTime must not change when Status didn't transition")
}

// TestSetDisaggregatedSetCondition_StaleFalseConditionObservedGenerationCatchesUp
// is a regression test for #980 review feedback: the *other* condition in the
// exclusive pair, once it has settled to Status=False, was only ever touched by
// the Status-flip branch — so if it stays False across further reconciles while
// spec generation keeps advancing, its ObservedGeneration froze at whatever it
// was on the last flip. That's misleading for a client reading
// conditions[Progressing].observedGeneration expecting it to track how current
// the reported state is. Reason/Message must NOT change, since they still
// accurately describe why the condition became false.
func TestSetDisaggregatedSetCondition_StaleFalseConditionObservedGenerationCatchesUp(t *testing.T) {
	original := metav1.NewTime(time.Now().Add(-time.Hour))
	ds := &disaggregatedsetv1.DisaggregatedSet{
		Status: disaggregatedsetv1.DisaggregatedSetStatus{
			Conditions: []metav1.Condition{
				{
					Type:               string(disaggregatedsetv1.DisaggregatedSetAvailable),
					Status:             metav1.ConditionTrue,
					ObservedGeneration: 2,
					LastTransitionTime: original,
					Reason:             "AllRolesReady",
					Message:            "All roles have reached their desired replica count, ready and updated to the current revision",
				},
				{
					Type:               string(disaggregatedsetv1.DisaggregatedSetProgressing),
					Status:             metav1.ConditionFalse,
					ObservedGeneration: 1, // stale: set on an earlier reconcile, before Available flipped true.
					LastTransitionTime: original,
					Reason:             "AllRolesReady",
					Message:            "All roles have reached their desired replica count, ready and updated to the current revision",
				},
			},
		},
	}

	// Available stays true on a later reconcile (generation advanced, no flip).
	newCondition := metav1.Condition{
		Type:               string(disaggregatedsetv1.DisaggregatedSetAvailable),
		Status:             metav1.ConditionTrue,
		ObservedGeneration: 3,
		Reason:             "AllRolesReady",
		Message:            "All roles have reached their desired replica count, ready and updated to the current revision",
	}

	changed := setDisaggregatedSetCondition(ds, newCondition)

	assert.True(t, changed)
	progressing := findCondition(ds.Status.Conditions, string(disaggregatedsetv1.DisaggregatedSetProgressing))
	require.NotNil(t, progressing)
	assert.Equal(t, metav1.ConditionFalse, progressing.Status, "must not flip back to true")
	assert.EqualValues(t, 3, progressing.ObservedGeneration, "ObservedGeneration must catch up even while staying False")
	assert.Equal(t, original.Time, progressing.LastTransitionTime.Time, "LastTransitionTime must not change when Status didn't transition")
	assert.Equal(t, "AllRolesReady", progressing.Reason, "Reason must not change: it still accurately describes why this became false")
}

// TestSetDisaggregatedSetCondition_LeavesUnrelatedConditionTypeUntouched guards
// against a broader mutual-exclusivity bug: setDisaggregatedSetCondition must
// only flip the specific Available/Progressing pair, not any other Status=True
// condition it happens to find (Copilot review on #980) — otherwise a future
// condition type, or one written by another controller, would get silently
// clobbered just for being true when Available/Progressing changes.
func TestSetDisaggregatedSetCondition_LeavesUnrelatedConditionTypeUntouched(t *testing.T) {
	original := metav1.NewTime(time.Now().Add(-time.Hour))
	ds := &disaggregatedsetv1.DisaggregatedSet{
		Status: disaggregatedsetv1.DisaggregatedSetStatus{
			Conditions: []metav1.Condition{{
				Type:               "SomeUnrelatedCondition",
				Status:             metav1.ConditionTrue,
				ObservedGeneration: 1,
				LastTransitionTime: original,
				Reason:             "Unrelated",
				Message:            "set by something else entirely",
			}},
		},
	}

	newCondition := metav1.Condition{
		Type:               string(disaggregatedsetv1.DisaggregatedSetAvailable),
		Status:             metav1.ConditionTrue,
		ObservedGeneration: 1,
		Reason:             "AllRolesReady",
		Message:            "All roles have reached their desired replica count, ready and updated to the current revision",
	}

	setDisaggregatedSetCondition(ds, newCondition)

	unrelated := findCondition(ds.Status.Conditions, "SomeUnrelatedCondition")
	require.NotNil(t, unrelated)
	assert.Equal(t, metav1.ConditionTrue, unrelated.Status, "a condition type outside the Available/Progressing pair must not be flipped")
	assert.Equal(t, original.Time, unrelated.LastTransitionTime.Time)
	assert.Equal(t, "Unrelated", unrelated.Reason)
}

func findCondition(conditions []metav1.Condition, condType string) *metav1.Condition {
	for i := range conditions {
		if conditions[i].Type == condType {
			return &conditions[i]
		}
	}
	return nil
}

// TestSetDesiredReplicas checks that each role and sub-role publishes the target
// the controller judges availability against, totalled across slices, and that an
// unknown target is left unset rather than reported as a requested zero.
func TestSetDesiredReplicas(t *testing.T) {
	for _, tc := range []struct {
		name    string
		desired map[string]int
		slices  int32
		want    []disaggregatedsetv1.RoleStatus
	}{
		{
			name:    "known targets",
			desired: map[string]int{"prefill": 2, "decode": 0},
			slices:  1,
			want:    []disaggregatedsetv1.RoleStatus{{Name: "prefill", DesiredReplicas: ptr.To[int32](2)}, {Name: "decode", DesiredReplicas: ptr.To[int32](0)}},
		},
		{
			name:    "totalled across slices",
			desired: map[string]int{"prefill": 2, "decode": 3},
			slices:  4,
			want:    []disaggregatedsetv1.RoleStatus{{Name: "prefill", DesiredReplicas: ptr.To[int32](8)}, {Name: "decode", DesiredReplicas: ptr.To[int32](12)}},
		},
		{
			name:    "unknown target is unset",
			desired: map[string]int{"decode": 1},
			slices:  1,
			want:    []disaggregatedsetv1.RoleStatus{{Name: "prefill"}, {Name: "decode", DesiredReplicas: ptr.To[int32](1)}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			statuses := []disaggregatedsetv1.RoleStatus{{Name: "prefill"}, {Name: "decode"}}
			setDesiredReplicas(statuses, tc.desired, tc.slices)
			require.Equal(t, tc.want, statuses)
		})
	}

	t.Run("sub-roles", func(t *testing.T) {
		statuses := []disaggregatedsetv1.RoleStatus{{Name: "model", SubRoleStatuses: []disaggregatedsetv1.SubRoleStatus{{Name: "a"}, {Name: "b"}}}}
		setDesiredReplicas(statuses, map[string]int{"model/a": 2}, 1)
		require.Equal(t, []disaggregatedsetv1.RoleStatus{{Name: "model", SubRoleStatuses: []disaggregatedsetv1.SubRoleStatus{
			{Name: "a", DesiredReplicas: ptr.To[int32](2)}, {Name: "b"},
		}}}, statuses, "a parent whose children are not all resolved has no target")
	})
}

// TestSummarizeRoleStatusesUsesDesiredReplicas checks that availability is judged
// against the published target, so status readers and the controller agree.
func TestSummarizeRoleStatusesUsesDesiredReplicas(t *testing.T) {
	ready := func(n int32, desired *int32) disaggregatedsetv1.RoleStatus {
		return disaggregatedsetv1.RoleStatus{Name: "decode", DesiredReplicas: desired, Replicas: n, ReadyReplicas: n, UpdatedReplicas: n}
	}
	for _, tc := range []struct {
		name   string
		status disaggregatedsetv1.RoleStatus
		want   bool
	}{
		{"at its target", ready(4, ptr.To[int32](4)), true},
		{"below its target", ready(4, ptr.To[int32](6)), false},
		{"scaled to zero", ready(0, ptr.To[int32](0)), true},
		{"unknown target", ready(0, nil), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			replicas, available := summarizeRoleStatuses([]disaggregatedsetv1.RoleStatus{tc.status})
			assert.Equal(t, tc.want, available)
			assert.Equal(t, map[string]int32{"decode": tc.status.Replicas}, replicas)
		})
	}
}

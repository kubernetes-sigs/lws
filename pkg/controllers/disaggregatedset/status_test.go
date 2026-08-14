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

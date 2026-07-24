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
				Message:            "All roles have reached the desired, ready, and updated replica count",
			}},
		},
	}

	newCondition := metav1.Condition{
		Type:               string(disaggregatedsetv1.DisaggregatedSetAvailable),
		Status:             metav1.ConditionTrue,
		ObservedGeneration: 2, // spec generation advanced, but the condition's Status did not change.
		Reason:             "AllRolesReady",
		Message:            "All roles have reached the desired, ready, and updated replica count",
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
				Message:            "All roles have reached the desired, ready, and updated replica count",
			}},
		},
	}

	newCondition := metav1.Condition{
		Type:               string(disaggregatedsetv1.DisaggregatedSetProgressing),
		Status:             metav1.ConditionTrue,
		ObservedGeneration: 1,
		Reason:             "RolloutInProgress",
		Message:            "Not all roles have reached the desired, ready, and updated replica count",
	}

	changed := setDisaggregatedSetCondition(ds, newCondition)

	assert.True(t, changed)
	require.Len(t, ds.Status.Conditions, 2)

	available := findCondition(ds.Status.Conditions, string(disaggregatedsetv1.DisaggregatedSetAvailable))
	require.NotNil(t, available)
	assert.Equal(t, metav1.ConditionFalse, available.Status)
	assert.True(t, available.LastTransitionTime.Time.After(original.Time), "a real Status flip must refresh LastTransitionTime")

	progressing := findCondition(ds.Status.Conditions, string(disaggregatedsetv1.DisaggregatedSetProgressing))
	require.NotNil(t, progressing)
	assert.Equal(t, metav1.ConditionTrue, progressing.Status)
}

func findCondition(conditions []metav1.Condition, condType string) *metav1.Condition {
	for i := range conditions {
		if conditions[i].Type == condType {
			return &conditions[i]
		}
	}
	return nil
}

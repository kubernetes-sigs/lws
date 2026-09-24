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

package controllers

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	leaderworkerset "sigs.k8s.io/lws/api/leaderworkerset/v1"
)

func conditionStatus(lws *leaderworkerset.LeaderWorkerSet, conditionType leaderworkerset.LeaderWorkerSetConditionType) (metav1.Condition, bool) {
	for _, condition := range lws.Status.Conditions {
		if condition.Type == string(conditionType) {
			return condition, true
		}
	}
	return metav1.Condition{}, false
}

func TestMakeCondition(t *testing.T) {
	lws := &leaderworkerset.LeaderWorkerSet{}
	lws.Generation = 3

	tests := []struct {
		name          string
		conditionType leaderworkerset.LeaderWorkerSetConditionType
		wantType      string
		wantReason    string
		wantMessage   string
	}{
		{
			name:          "available",
			conditionType: leaderworkerset.LeaderWorkerSetAvailable,
			wantType:      string(leaderworkerset.LeaderWorkerSetAvailable),
			wantReason:    "AllGroupsReady",
			wantMessage:   "All replicas are ready",
		},
		{
			name:          "update in progress",
			conditionType: leaderworkerset.LeaderWorkerSetUpdateInProgress,
			wantType:      string(leaderworkerset.LeaderWorkerSetUpdateInProgress),
			wantReason:    GroupsUpdating,
			wantMessage:   "Rolling Upgrade is in progress",
		},
		{
			name:          "progressing",
			conditionType: leaderworkerset.LeaderWorkerSetProgressing,
			wantType:      string(leaderworkerset.LeaderWorkerSetProgressing),
			wantReason:    GroupsProgressing,
			wantMessage:   "Replicas are progressing",
		},
		{
			name:          "unknown types fall back to progressing",
			conditionType: leaderworkerset.LeaderWorkerSetConditionType("Bogus"),
			wantType:      string(leaderworkerset.LeaderWorkerSetProgressing),
			wantReason:    GroupsProgressing,
			wantMessage:   "Replicas are progressing",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			condition := makeCondition(tc.conditionType, lws)
			if condition.Type != tc.wantType {
				t.Errorf("type = %s, want %s", condition.Type, tc.wantType)
			}
			if condition.Reason != tc.wantReason {
				t.Errorf("reason = %s, want %s", condition.Reason, tc.wantReason)
			}
			if condition.Message != tc.wantMessage {
				t.Errorf("message = %s, want %s", condition.Message, tc.wantMessage)
			}
			if condition.Status != metav1.ConditionTrue {
				t.Errorf("status = %s, want %s", condition.Status, metav1.ConditionTrue)
			}
			if condition.ObservedGeneration != lws.Generation {
				t.Errorf("observedGeneration = %d, want %d", condition.ObservedGeneration, lws.Generation)
			}
		})
	}
}

func TestSetConditions(t *testing.T) {
	t.Run("adds a new condition", func(t *testing.T) {
		lws := &leaderworkerset.LeaderWorkerSet{}
		if !setConditions(lws, []metav1.Condition{makeCondition(leaderworkerset.LeaderWorkerSetProgressing, lws)}) {
			t.Fatal("setConditions() = false, want true when a condition is added")
		}
		if _, found := conditionStatus(lws, leaderworkerset.LeaderWorkerSetProgressing); !found {
			t.Error("Progressing condition was not added")
		}
	})

	t.Run("re-setting the same condition is a no-op", func(t *testing.T) {
		lws := &leaderworkerset.LeaderWorkerSet{}
		condition := makeCondition(leaderworkerset.LeaderWorkerSetProgressing, lws)
		setConditions(lws, []metav1.Condition{condition})

		if setConditions(lws, []metav1.Condition{condition}) {
			t.Error("setConditions() = true, want false when nothing changed")
		}
		if got := len(lws.Status.Conditions); got != 1 {
			t.Errorf("got %d conditions, want 1", got)
		}
	})

	t.Run("available flips progressing to false", func(t *testing.T) {
		lws := &leaderworkerset.LeaderWorkerSet{}
		setConditions(lws, []metav1.Condition{makeCondition(leaderworkerset.LeaderWorkerSetProgressing, lws)})

		if !setConditions(lws, []metav1.Condition{makeCondition(leaderworkerset.LeaderWorkerSetAvailable, lws)}) {
			t.Fatal("setConditions() = false, want true when the available condition is added")
		}

		// Progressing and Available are mutually exclusive.
		progressing, found := conditionStatus(lws, leaderworkerset.LeaderWorkerSetProgressing)
		if !found {
			t.Fatal("Progressing condition disappeared")
		}
		if progressing.Status != metav1.ConditionFalse {
			t.Errorf("Progressing status = %s, want %s", progressing.Status, metav1.ConditionFalse)
		}
		available, found := conditionStatus(lws, leaderworkerset.LeaderWorkerSetAvailable)
		if !found {
			t.Fatal("Available condition was not added")
		}
		if available.Status != metav1.ConditionTrue {
			t.Errorf("Available status = %s, want %s", available.Status, metav1.ConditionTrue)
		}
	})

	t.Run("available flips update in progress to false", func(t *testing.T) {
		lws := &leaderworkerset.LeaderWorkerSet{}
		setConditions(lws, []metav1.Condition{makeCondition(leaderworkerset.LeaderWorkerSetUpdateInProgress, lws)})
		setConditions(lws, []metav1.Condition{makeCondition(leaderworkerset.LeaderWorkerSetAvailable, lws)})

		updating, found := conditionStatus(lws, leaderworkerset.LeaderWorkerSetUpdateInProgress)
		if !found {
			t.Fatal("UpdateInProgress condition disappeared")
		}
		if updating.Status != metav1.ConditionFalse {
			t.Errorf("UpdateInProgress status = %s, want %s", updating.Status, metav1.ConditionFalse)
		}
	})

	t.Run("a bumped generation refreshes observedGeneration", func(t *testing.T) {
		lws := &leaderworkerset.LeaderWorkerSet{}
		setConditions(lws, []metav1.Condition{makeCondition(leaderworkerset.LeaderWorkerSetProgressing, lws)})

		lws.Generation = 5
		if !setConditions(lws, nil) {
			t.Fatal("setConditions() = false, want true when observedGeneration is stale")
		}
		for _, condition := range lws.Status.Conditions {
			if condition.ObservedGeneration != 5 {
				t.Errorf("condition %s observedGeneration = %d, want 5", condition.Type, condition.ObservedGeneration)
			}
		}
	})

	t.Run("a false condition is recorded", func(t *testing.T) {
		lws := &leaderworkerset.LeaderWorkerSet{}
		condition := makeCondition(leaderworkerset.LeaderWorkerSetProgressing, lws)
		condition.Status = metav1.ConditionFalse

		// False conditions are recorded explicitly: the restart-budget feature
		// surfaces Degraded=False on healthy workloads, so absence cannot be
		// used to skip not-yet-present false conditions.
		if !setConditions(lws, []metav1.Condition{condition}) {
			t.Error("setConditions() = false, want true for a condition that does not exist yet")
		}
		if len(lws.Status.Conditions) != 1 || lws.Status.Conditions[0].Status != metav1.ConditionFalse {
			t.Errorf("got conditions %v, want a single false condition", lws.Status.Conditions)
		}
	})
}

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

package pod

import (
	"testing"

	"github.com/google/go-cmp/cmp"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	leaderworkerset "sigs.k8s.io/lws/api/leaderworkerset/v1"
)

func TestPodDeleted(t *testing.T) {
	now := metav1.Now()
	tests := []struct {
		name string
		pod  corev1.Pod
		want bool
	}{
		{
			name: "pod without a deletion timestamp is not deleted",
			pod:  corev1.Pod{},
			want: false,
		},
		{
			name: "pod with a deletion timestamp is deleted",
			pod:  corev1.Pod{ObjectMeta: metav1.ObjectMeta{DeletionTimestamp: &now}},
			want: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := PodDeleted(tc.pod); got != tc.want {
				t.Errorf("PodDeleted() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestHasSchedulingGate(t *testing.T) {
	tests := []struct {
		name string
		pod  *corev1.Pod
		gate string
		want bool
	}{
		{
			name: "no scheduling gates",
			pod:  &corev1.Pod{},
			gate: leaderworkerset.GroupReplacementSchedulingGate,
			want: false,
		},
		{
			name: "gate present",
			pod: &corev1.Pod{Spec: corev1.PodSpec{SchedulingGates: []corev1.PodSchedulingGate{
				{Name: leaderworkerset.GroupReplacementSchedulingGate},
			}}},
			gate: leaderworkerset.GroupReplacementSchedulingGate,
			want: true,
		},
		{
			name: "a different gate is present",
			pod: &corev1.Pod{Spec: corev1.PodSpec{SchedulingGates: []corev1.PodSchedulingGate{
				{Name: "other-gate"},
			}}},
			gate: leaderworkerset.GroupReplacementSchedulingGate,
			want: false,
		},
		{
			name: "the gate is one of many",
			pod: &corev1.Pod{Spec: corev1.PodSpec{SchedulingGates: []corev1.PodSchedulingGate{
				{Name: "other-gate"},
				{Name: leaderworkerset.GroupReplacementSchedulingGate},
			}}},
			gate: leaderworkerset.GroupReplacementSchedulingGate,
			want: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := HasSchedulingGate(tc.pod, tc.gate); got != tc.want {
				t.Errorf("HasSchedulingGate() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestLeaderPod(t *testing.T) {
	tests := []struct {
		name string
		pod  corev1.Pod
		want bool
	}{
		{
			name: "worker index 0 is the leader",
			pod:  corev1.Pod{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{leaderworkerset.WorkerIndexLabelKey: "0"}}},
			want: true,
		},
		{
			name: "worker index 1 is not the leader",
			pod:  corev1.Pod{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{leaderworkerset.WorkerIndexLabelKey: "1"}}},
			want: false,
		},
		{
			name: "missing worker index label is not the leader",
			pod:  corev1.Pod{},
			want: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := LeaderPod(tc.pod); got != tc.want {
				t.Errorf("LeaderPod() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestPodRunningAndReady(t *testing.T) {
	tests := []struct {
		name string
		pod  corev1.Pod
		want bool
	}{
		{
			name: "running and ready",
			pod: corev1.Pod{Status: corev1.PodStatus{
				Phase:      corev1.PodRunning,
				Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}},
			}},
			want: true,
		},
		{
			name: "running but not ready",
			pod: corev1.Pod{Status: corev1.PodStatus{
				Phase:      corev1.PodRunning,
				Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionFalse}},
			}},
			want: false,
		},
		{
			name: "running without a ready condition",
			pod: corev1.Pod{Status: corev1.PodStatus{
				Phase:      corev1.PodRunning,
				Conditions: []corev1.PodCondition{{Type: corev1.PodScheduled, Status: corev1.ConditionTrue}},
			}},
			want: false,
		},
		{
			name: "running without any condition",
			pod:  corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodRunning}},
			want: false,
		},
		{
			name: "ready but pending",
			pod: corev1.Pod{Status: corev1.PodStatus{
				Phase:      corev1.PodPending,
				Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}},
			}},
			want: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := PodRunningAndReady(tc.pod); got != tc.want {
				t.Errorf("PodRunningAndReady() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestIsPodReady(t *testing.T) {
	tests := []struct {
		name string
		pod  *corev1.Pod
		want bool
	}{
		{
			name: "ready condition is true",
			pod: &corev1.Pod{Status: corev1.PodStatus{
				Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}},
			}},
			want: true,
		},
		{
			name: "ready condition is false",
			pod: &corev1.Pod{Status: corev1.PodStatus{
				Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionFalse}},
			}},
			want: false,
		},
		{
			name: "no conditions",
			pod:  &corev1.Pod{},
			want: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsPodReady(tc.pod); got != tc.want {
				t.Errorf("IsPodReady() = %v, want %v", got, tc.want)
			}
			if got := IsPodReadyConditionTrue(tc.pod.Status); got != tc.want {
				t.Errorf("IsPodReadyConditionTrue() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestContainersReady(t *testing.T) {
	tests := []struct {
		name string
		pod  *corev1.Pod
		want bool
	}{
		{
			name: "containers ready is true",
			pod: &corev1.Pod{Status: corev1.PodStatus{
				Conditions: []corev1.PodCondition{{Type: corev1.ContainersReady, Status: corev1.ConditionTrue}},
			}},
			want: true,
		},
		{
			name: "containers ready is false",
			pod: &corev1.Pod{Status: corev1.PodStatus{
				Conditions: []corev1.PodCondition{{Type: corev1.ContainersReady, Status: corev1.ConditionFalse}},
			}},
			want: false,
		},
		{
			name: "only the pod ready condition is set",
			pod: &corev1.Pod{Status: corev1.PodStatus{
				Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}},
			}},
			want: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := ContainersReady(tc.pod); got != tc.want {
				t.Errorf("ContainersReady() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestGetPodReadyCondition(t *testing.T) {
	readyCondition := corev1.PodCondition{Type: corev1.PodReady, Status: corev1.ConditionTrue, Reason: "Ready"}
	tests := []struct {
		name   string
		status corev1.PodStatus
		want   *corev1.PodCondition
	}{
		{
			name: "ready condition found",
			status: corev1.PodStatus{Conditions: []corev1.PodCondition{
				{Type: corev1.PodScheduled, Status: corev1.ConditionTrue},
				readyCondition,
			}},
			want: &readyCondition,
		},
		{
			name:   "ready condition missing",
			status: corev1.PodStatus{Conditions: []corev1.PodCondition{{Type: corev1.PodScheduled}}},
			want:   nil,
		},
		{
			name:   "nil conditions",
			status: corev1.PodStatus{},
			want:   nil,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if diff := cmp.Diff(tc.want, GetPodReadyCondition(tc.status)); diff != "" {
				t.Errorf("unexpected condition (-want +got): %s", diff)
			}
		})
	}
}

func TestGetPodCondition(t *testing.T) {
	scheduled := corev1.PodCondition{Type: corev1.PodScheduled, Status: corev1.ConditionTrue}
	ready := corev1.PodCondition{Type: corev1.PodReady, Status: corev1.ConditionTrue}
	tests := []struct {
		name          string
		status        *corev1.PodStatus
		conditionType corev1.PodConditionType
		wantIndex     int
		wantCondition *corev1.PodCondition
	}{
		{
			name:          "nil status",
			status:        nil,
			conditionType: corev1.PodReady,
			wantIndex:     -1,
		},
		{
			name:          "nil conditions",
			status:        &corev1.PodStatus{},
			conditionType: corev1.PodReady,
			wantIndex:     -1,
		},
		{
			name:          "condition found at a non zero index",
			status:        &corev1.PodStatus{Conditions: []corev1.PodCondition{scheduled, ready}},
			conditionType: corev1.PodReady,
			wantIndex:     1,
			wantCondition: &ready,
		},
		{
			name:          "condition not in the list",
			status:        &corev1.PodStatus{Conditions: []corev1.PodCondition{scheduled}},
			conditionType: corev1.ContainersReady,
			wantIndex:     -1,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			index, condition := GetPodCondition(tc.status, tc.conditionType)
			if index != tc.wantIndex {
				t.Errorf("GetPodCondition() index = %d, want %d", index, tc.wantIndex)
			}
			if diff := cmp.Diff(tc.wantCondition, condition); diff != "" {
				t.Errorf("unexpected condition (-want +got): %s", diff)
			}

			var conditions []corev1.PodCondition
			if tc.status != nil {
				conditions = tc.status.Conditions
			}
			index, condition = GetPodConditionFromList(conditions, tc.conditionType)
			if index != tc.wantIndex {
				t.Errorf("GetPodConditionFromList() index = %d, want %d", index, tc.wantIndex)
			}
			if diff := cmp.Diff(tc.wantCondition, condition); diff != "" {
				t.Errorf("unexpected condition (-want +got): %s", diff)
			}
		})
	}
}

/*
Copyright 2023.

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
	"strconv"
	"testing"

	"github.com/google/go-cmp/cmp"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	leaderworkerset "sigs.k8s.io/lws/api/leaderworkerset/v1"
	"sigs.k8s.io/lws/test/wrappers"
)

func TestContainerRestarted(t *testing.T) {
	tests := []struct {
		name                     string
		pod                      corev1.Pod
		expectRestartedContainer bool
	}{
		{
			name: "Pod in running phase, InitContainerStatuses has restart count > 0",
			pod: corev1.Pod{
				Status: corev1.PodStatus{
					Phase: corev1.PodRunning,
					InitContainerStatuses: []corev1.ContainerStatus{{
						RestartCount: 1,
					}},
				},
			},
			expectRestartedContainer: true,
		},
		{
			name: "Pod in pending phase, InitContainerStatuses has restart count > 0",
			pod: corev1.Pod{
				Status: corev1.PodStatus{
					Phase: corev1.PodPending,
					InitContainerStatuses: []corev1.ContainerStatus{{
						RestartCount: 1,
					}},
				},
			},
			expectRestartedContainer: true,
		},
		{
			name: "Pod in running phase, ContainerStatuses has restart count > 0",
			pod: corev1.Pod{
				Status: corev1.PodStatus{
					Phase: corev1.PodRunning,
					ContainerStatuses: []corev1.ContainerStatus{{
						RestartCount: 1,
					}},
				},
			},
			expectRestartedContainer: true,
		},
		{
			name: "Pod in Failed status",
			pod: corev1.Pod{
				Status: corev1.PodStatus{
					Phase: corev1.PodFailed,
				},
			},
		},
		{
			name: "Pod in running phase, InitContainerStatuses has restart count = 0, ContainerStatuses = 0",
			pod: corev1.Pod{
				Status: corev1.PodStatus{
					Phase: corev1.PodRunning,
					InitContainerStatuses: []corev1.ContainerStatus{{
						RestartCount: 0,
					}},
					ContainerStatuses: []corev1.ContainerStatus{{
						RestartCount: 0,
					}},
				},
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			containerRestarted := ContainerRestarted(tc.pod)
			if containerRestarted != tc.expectRestartedContainer {
				t.Errorf("Expected value %t, got %t", tc.expectRestartedContainer, containerRestarted)
			}
		})
	}
}

func TestAddLWSVariables(t *testing.T) {
	tests := []struct {
		name                     string
		pod                      *corev1.Pod
		expectedLwsLeaderAddress string
		expectedGroupSize        int
		expectedWorkerIndex      string
	}{
		{
			name:                     "Leader pod",
			pod:                      wrappers.MakePodWithLabels("test-sample", "0", "0", "default", 3),
			expectedLwsLeaderAddress: "test-sample-0.test-sample.default",
			expectedGroupSize:        3,
			expectedWorkerIndex:      "0",
		},
		{
			name:                     "Worker pod",
			pod:                      wrappers.MakePodWithLabels("test-sample", "0", "1", "default", 3),
			expectedLwsLeaderAddress: "test-sample-0.test-sample.default",
			expectedGroupSize:        3,
			expectedWorkerIndex:      "1",
		},
		{
			name:                     "Leader pod, group 1",
			pod:                      wrappers.MakePodWithLabels("test-sample", "1", "0", "default", 2),
			expectedLwsLeaderAddress: "test-sample-1.test-sample.default",
			expectedGroupSize:        2,
			expectedWorkerIndex:      "0",
		},
		{
			name:                     "Worker pod, group 1",
			pod:                      wrappers.MakePodWithLabels("test-sample", "1", "3", "default", 2),
			expectedLwsLeaderAddress: "test-sample-1.test-sample.default",
			expectedGroupSize:        2,
			expectedWorkerIndex:      "3",
		},
		{
			name:                     "Leader pod, group 1, non-default namespace",
			pod:                      wrappers.MakePodWithLabels("test-sample", "1", "3", "lws", 2),
			expectedLwsLeaderAddress: "test-sample-1.test-sample.lws",
			expectedGroupSize:        2,
			expectedWorkerIndex:      "3",
		},
		{
			name:                     "Worker pod, group 1, non-default namespace",
			pod:                      wrappers.MakePodWithLabels("test-sample", "1", "3", "lws", 2),
			expectedLwsLeaderAddress: "test-sample-1.test-sample.lws",
			expectedGroupSize:        2,
			expectedWorkerIndex:      "3",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := AddLWSVariables(tc.pod, tc.expectedLwsLeaderAddress)
			if err != nil {
				t.Fatalf("Error parsing parent: %s", err.Error())
			}
			containers := append(tc.pod.Spec.Containers, tc.pod.Spec.InitContainers...)
			if len(containers) == 0 {
				t.Fatalf("No containers in podSpec %+v", tc.pod.Spec)
			}

			for _, container := range containers {
				if len(container.Env) == 0 {
					t.Errorf("Failed to add LWS Variables to container %+v", container)
				}

				envVar := container.Env[0]
				if diff := cmp.Diff(envVar.Value, tc.expectedLwsLeaderAddress); diff != "" {
					t.Errorf("Unexpected lws leader address %s", diff)
				}
				envVar = container.Env[1]
				if diff := cmp.Diff(envVar.Value, strconv.Itoa(tc.expectedGroupSize)); diff != "" {
					t.Errorf("Unexpected lws group size %s", diff)
				}
				envVar = container.Env[2]
				if diff := cmp.Diff(envVar.Value, tc.expectedWorkerIndex); diff != "" {
					t.Errorf("Unexpected lws worker index %s", diff)
				}
			}
		})
	}
}

func TestGetEnvVarIfInContainer(t *testing.T) {
	tests := []struct {
		name             string
		container        corev1.Container
		envVarName       string
		expectEnvVar     bool
		expectedEnvValue string
	}{
		{
			name: "Container contains the environment variable, returns correct value",
			container: corev1.Container{
				Name: "test",
				Env: []corev1.EnvVar{
					{
						Name:  "PROCESS_PORT",
						Value: "8776",
					},
					{
						Name:  "PROCESS_ID",
						Value: "1",
					},
				},
			},
			envVarName:       "PROCESS_PORT",
			expectEnvVar:     true,
			expectedEnvValue: "8776",
		},
		{
			name: "Container does not contain the environment variable, returns empty string",
			container: corev1.Container{
				Name: "test",
				Env: []corev1.EnvVar{
					{
						Name:  "PROCESS_ADDRESSES",
						Value: "lws-default.0",
					},
					{
						Name:  "PROCESS_ID",
						Value: "1",
					},
				},
			},
			envVarName:       "PROCESS_PORT",
			expectEnvVar:     false,
			expectedEnvValue: "",
		},
		{
			name: "Container does not contain any environment variable, returns empty string",
			container: corev1.Container{
				Name: "test",
			},
			envVarName:       "PROCESS_PORT",
			expectEnvVar:     false,
			expectedEnvValue: "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			envVarInContainer, envVarValue := GetEnvVarValueIfInContainer(&tc.container, tc.envVarName)
			if envVarInContainer != tc.expectEnvVar {
				t.Errorf("Unexpected env var in container, %s with value %s", tc.envVarName, envVarValue)
			}
			if envVarValue != tc.expectedEnvValue {
				t.Errorf("Unexpected env var value, got: %s, expected: %s", envVarValue, tc.expectedEnvValue)
			}
		})
	}
}

func TestPodReadinessHelpers(t *testing.T) {
	ready := corev1.PodCondition{Type: corev1.PodReady, Status: corev1.ConditionTrue}
	notReady := corev1.PodCondition{Type: corev1.PodReady, Status: corev1.ConditionFalse}
	containersReady := corev1.PodCondition{Type: corev1.ContainersReady, Status: corev1.ConditionTrue}

	tests := []struct {
		name                string
		pod                 corev1.Pod
		wantRunningAndReady bool
		wantReady           bool
		wantContainersReady bool
	}{
		{
			name:                "running with ready condition true",
			pod:                 corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{ready, containersReady}}},
			wantRunningAndReady: true,
			wantReady:           true,
			wantContainersReady: true,
		},
		{
			name:                "running but ready condition false",
			pod:                 corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{notReady}}},
			wantRunningAndReady: false,
			wantReady:           false,
		},
		{
			name:                "pending with ready condition true is not running and ready",
			pod:                 corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodPending, Conditions: []corev1.PodCondition{ready}}},
			wantRunningAndReady: false,
			wantReady:           true,
		},
		{
			name:                "running without any ready condition",
			pod:                 corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodRunning}},
			wantRunningAndReady: false,
			wantReady:           false,
		},
		{
			name:                "containers ready without pod ready",
			pod:                 corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{containersReady}}},
			wantRunningAndReady: false,
			wantReady:           false,
			wantContainersReady: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := PodRunningAndReady(tc.pod); got != tc.wantRunningAndReady {
				t.Fatalf("PodRunningAndReady()=%v, want %v", got, tc.wantRunningAndReady)
			}
			pod := tc.pod
			if got := IsPodReady(&pod); got != tc.wantReady {
				t.Fatalf("IsPodReady()=%v, want %v", got, tc.wantReady)
			}
			if got := IsPodReadyConditionTrue(pod.Status); got != tc.wantReady {
				t.Fatalf("IsPodReadyConditionTrue()=%v, want %v", got, tc.wantReady)
			}
			if got := ContainersReady(&pod); got != tc.wantContainersReady {
				t.Fatalf("ContainersReady()=%v, want %v", got, tc.wantContainersReady)
			}
		})
	}
}

func TestGetPodCondition(t *testing.T) {
	ready := corev1.PodCondition{Type: corev1.PodReady, Status: corev1.ConditionTrue}
	scheduled := corev1.PodCondition{Type: corev1.PodScheduled, Status: corev1.ConditionTrue}

	t.Run("nil status", func(t *testing.T) {
		if idx, cond := GetPodCondition(nil, corev1.PodReady); idx != -1 || cond != nil {
			t.Fatalf("GetPodCondition(nil)=%d,%v want -1,nil", idx, cond)
		}
	})
	t.Run("nil conditions slice", func(t *testing.T) {
		if idx, cond := GetPodConditionFromList(nil, corev1.PodReady); idx != -1 || cond != nil {
			t.Fatalf("GetPodConditionFromList(nil)=%d,%v want -1,nil", idx, cond)
		}
	})
	t.Run("empty conditions slice", func(t *testing.T) {
		if idx, cond := GetPodConditionFromList([]corev1.PodCondition{}, corev1.PodReady); idx != -1 || cond != nil {
			t.Fatalf("GetPodConditionFromList(empty)=%d,%v want -1,nil", idx, cond)
		}
	})
	t.Run("returns the index of the matching condition", func(t *testing.T) {
		status := corev1.PodStatus{Conditions: []corev1.PodCondition{scheduled, ready}}
		idx, cond := GetPodCondition(&status, corev1.PodReady)
		if idx != 1 || cond == nil || cond.Type != corev1.PodReady {
			t.Fatalf("GetPodCondition()=%d,%v want index 1 and the Ready condition", idx, cond)
		}
		if got := GetPodReadyCondition(status); got == nil || got.Status != corev1.ConditionTrue {
			t.Fatalf("GetPodReadyCondition()=%v want the Ready condition", got)
		}
	})
	t.Run("returns nil for a type that is not present", func(t *testing.T) {
		status := corev1.PodStatus{Conditions: []corev1.PodCondition{scheduled}}
		if got := GetPodReadyCondition(status); got != nil {
			t.Fatalf("GetPodReadyCondition()=%v want nil", got)
		}
	})
}

func TestPodDeletedAndLeaderPod(t *testing.T) {
	now := metav1.Now()
	if PodDeleted(corev1.Pod{}) {
		t.Fatalf("PodDeleted() without deletionTimestamp must be false")
	}
	if !PodDeleted(corev1.Pod{ObjectMeta: metav1.ObjectMeta{DeletionTimestamp: &now}}) {
		t.Fatalf("PodDeleted() with deletionTimestamp must be true")
	}
	if !LeaderPod(corev1.Pod{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{leaderworkerset.WorkerIndexLabelKey: "0"}}}) {
		t.Fatalf("LeaderPod() with worker index 0 must be true")
	}
	if LeaderPod(corev1.Pod{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{leaderworkerset.WorkerIndexLabelKey: "1"}}}) {
		t.Fatalf("LeaderPod() with worker index 1 must be false")
	}
	if LeaderPod(corev1.Pod{}) {
		t.Fatalf("LeaderPod() without labels must be false")
	}
}

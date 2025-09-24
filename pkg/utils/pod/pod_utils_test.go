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
			err := AddLWSVariables(tc.pod)
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

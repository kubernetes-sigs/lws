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
	"testing"

	corev1 "k8s.io/api/core/v1"
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

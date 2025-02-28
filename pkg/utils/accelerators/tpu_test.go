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

package accelerator

import (
	"testing"

	"github.com/google/go-cmp/cmp"
	"k8s.io/apimachinery/pkg/api/resource"
	leaderworkerset "sigs.k8s.io/lws/api/leaderworkerset/v1"
	"sigs.k8s.io/lws/test/wrappers"

	corev1 "k8s.io/api/core/v1"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestAddTPUVariables(t *testing.T) {
	tests := []struct {
		name                       string
		pod                        *corev1.Pod
		size                       int
		hasWorkerIndexLabelKey     bool
		expectedTpuWorkerHostNames string
		expectedTpuWorkerId        string
		expectedTpuName            string
	}{
		{
			name: "Worker Index is 0",
			pod: &corev1.Pod{
				Spec: wrappers.MakeLeaderPodSpecWithTPUResource(),
				ObjectMeta: v1.ObjectMeta{
					Name:      "test-sample-1",
					Namespace: "default",
					Labels: map[string]string{
						leaderworkerset.WorkerIndexLabelKey: "0",
					},
				},
			},
			size:                       1,
			hasWorkerIndexLabelKey:     true,
			expectedTpuWorkerHostNames: "test-sample-1.default",
			expectedTpuWorkerId:        "0",
			expectedTpuName:            "test-sample-1",
		},
		{
			name: "Worker Index is non-zero, size is above 2",
			pod: &corev1.Pod{
				Spec: wrappers.MakeLeaderPodSpecWithTPUResource(),
				ObjectMeta: v1.ObjectMeta{
					Name:      "test-sample-1-3",
					Namespace: "default",
					Labels: map[string]string{
						leaderworkerset.WorkerIndexLabelKey: "3",
					},
					Annotations: map[string]string{
						LeaderRequestsTPUsAnnotationKey: "true",
					},
				},
			},
			size:                       5,
			hasWorkerIndexLabelKey:     true,
			expectedTpuWorkerHostNames: "test-sample-1.default,test-sample-1-1.default,test-sample-1-2.default,test-sample-1-3.default,test-sample-1-4.default",
			expectedTpuWorkerId:        "3",
			expectedTpuName:            "test-sample-1",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := AddTPUVariables(tc.pod, tc.size, true)
			if err != nil {
				t.Errorf("Error parsing parent: %s", err.Error())
			}
			if len(tc.pod.Spec.Containers[0].Env) == 0 && tc.hasWorkerIndexLabelKey {
				t.Errorf("Failed to add TPU Variables")
			}
			if len(tc.pod.Spec.Containers[0].Env) > 0 && !tc.hasWorkerIndexLabelKey {
				t.Errorf("Added TPU Variables when it wasn't supposed to")
			}

			if tc.hasWorkerIndexLabelKey {
				if diff := cmp.Diff(tc.pod.Spec.Containers[0].Env[0].Value, tc.expectedTpuWorkerHostNames); diff != "" {
					t.Errorf("unexpected add TPU worker host names operation %s", diff)
				}
				if diff := cmp.Diff(tc.pod.Spec.Containers[0].Env[1].Value, tc.expectedTpuWorkerId); diff != "" {
					t.Errorf("unexpected add TPU worker ID operation: %s", diff)
				}
				if diff := cmp.Diff(tc.pod.Spec.Containers[0].Env[2].Value, tc.expectedTpuName); diff != "" {
					t.Errorf("unexpected add TPU Name operation: %s", diff)
				}
			}
		})
	}
}

func TestAddTPUVariablesSubGroup(t *testing.T) {
	tests := []struct {
		name                       string
		pod                        *corev1.Pod
		expectedTpuWorkerHostNames string
		expectedTpuWorkerId        string
		expectedTpuName            string
	}{
		{
			name: "Leader requests TPU resources",
			pod: &corev1.Pod{
				Spec: wrappers.MakeLeaderPodSpecWithTPUResource(),
				ObjectMeta: v1.ObjectMeta{
					Name:      "test-sample-1-3",
					Namespace: "default",
					Labels: map[string]string{
						leaderworkerset.WorkerIndexLabelKey:   "3",
						leaderworkerset.SubGroupIndexLabelKey: "0",
					},
					Annotations: map[string]string{
						LeaderRequestsTPUsAnnotationKey:           "true",
						leaderworkerset.SubGroupSizeAnnotationKey: "5",
					},
				},
			},
			expectedTpuWorkerId:        "3",
			expectedTpuWorkerHostNames: "test-sample-1.default,test-sample-1-1.default,test-sample-1-2.default,test-sample-1-3.default,test-sample-1-4.default",
			expectedTpuName:            "test-sample-1",
		},
		{
			name: "Leader requests TPU resources, worker with subgroup index > 0",
			pod: &corev1.Pod{
				Spec: wrappers.MakeLeaderPodSpecWithTPUResource(),
				ObjectMeta: v1.ObjectMeta{
					Name:      "test-sample-1-7",
					Namespace: "default",
					Labels: map[string]string{
						leaderworkerset.WorkerIndexLabelKey:   "7",
						leaderworkerset.SubGroupIndexLabelKey: "1",
					},
					Annotations: map[string]string{
						LeaderRequestsTPUsAnnotationKey:           "true",
						leaderworkerset.SubGroupSizeAnnotationKey: "4",
					},
				},
			},
			expectedTpuWorkerId:        "3",
			expectedTpuWorkerHostNames: "test-sample-1-4.default,test-sample-1-5.default,test-sample-1-6.default,test-sample-1-7.default",
			expectedTpuName:            "test-sample-1",
		},
		{
			name: "Leader does not request TPU resources, worker with subgroup index > 0",
			pod: &corev1.Pod{
				Spec: wrappers.MakeLeaderPodSpecWithTPUResource(),
				ObjectMeta: v1.ObjectMeta{
					Name:      "test-sample-1-5",
					Namespace: "default",
					Labels: map[string]string{
						leaderworkerset.WorkerIndexLabelKey:   "5",
						leaderworkerset.SubGroupIndexLabelKey: "1",
					},
					Annotations: map[string]string{
						leaderworkerset.SubGroupSizeAnnotationKey: "4",
					},
				},
			},
			expectedTpuWorkerId:        "0",
			expectedTpuWorkerHostNames: "test-sample-1-5.default,test-sample-1-6.default,test-sample-1-7.default,test-sample-1-8.default",
			expectedTpuName:            "test-sample-1",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := addTPUVariablesSubGroup(tc.pod)
			if err != nil {
				t.Errorf("Error parsing parent: %s", err.Error())
			}
			if diff := cmp.Diff(tc.pod.Spec.Containers[0].Env[0].Value, tc.expectedTpuWorkerHostNames); diff != "" {
				t.Errorf("unexpected add TPU worker host names operation %s", diff)
			}
			if diff := cmp.Diff(tc.pod.Spec.Containers[0].Env[1].Value, tc.expectedTpuWorkerId); diff != "" {
				t.Errorf("unexpected add TPU worker ID operation: %s", diff)
			}
			if diff := cmp.Diff(tc.pod.Spec.Containers[0].Env[2].Value, tc.expectedTpuName); diff != "" {
				t.Errorf("unexpected add TPU Name operation: %s", diff)
			}
		})
	}
}

func TestGetContainerRequestingTPUs(t *testing.T) {
	tests := []struct {
		name              string
		podSpec           corev1.PodSpec
		expectedContainer *corev1.Container
	}{
		{
			name:    "Single Container with TPU Resource",
			podSpec: wrappers.MakeLeaderPodSpecWithTPUResource(),
			expectedContainer: &corev1.Container{
				Name:  "worker",
				Image: "busybox",
				Resources: corev1.ResourceRequirements{
					Limits: map[corev1.ResourceName]resource.Quantity{
						corev1.ResourceName("google.com/tpu"): resource.MustParse("4"),
					},
				},
			},
		},
		{
			name:    "Multiple Containers, one with TPU Resource",
			podSpec: wrappers.MakeLeaderPodSpecWithTPUResourceMultipleContainers(),
			expectedContainer: &corev1.Container{
				Name:  "worker",
				Image: "busybox",
				Resources: corev1.ResourceRequirements{
					Limits: map[corev1.ResourceName]resource.Quantity{
						corev1.ResourceName("google.com/tpu"): resource.MustParse("4"),
					},
				},
			},
		},
		{
			name:              "Container without TPU Resource",
			podSpec:           wrappers.MakeLeaderPodSpec(),
			expectedContainer: nil,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			container := getContainerRequestingTPUs(&tc.podSpec)
			if diff := cmp.Diff(tc.expectedContainer, container); diff != "" {
				t.Errorf("unexpected get container operation %s", diff)
			}
		})
	}
}

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
	"strconv"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	leaderworkerset "sigs.k8s.io/lws/api/leaderworkerset/v1"
	podutils "sigs.k8s.io/lws/pkg/utils/pod"
	statefulsetutils "sigs.k8s.io/lws/pkg/utils/statefulset"
	"sigs.k8s.io/lws/test/wrappers"
)

func TestAddTPUVariables(t *testing.T) {
	tests := []struct {
		name                        string
		pod                         *corev1.Pod
		size                        int
		expectedTpuWorkerHostNames  string
		expectedTpuWorkerId         string
		expectedTpuName             string
		expectedTpuProcessAddresses string
		expectedTpuProcessPort      string
	}{
		{
			name: "Leader requests TPU resources, leader pod",
			pod: &corev1.Pod{
				Spec: wrappers.MakeLeaderPodSpecWithTPU(),
				ObjectMeta: v1.ObjectMeta{
					Name:      "test-sample-0",
					Namespace: "default",
					Labels: map[string]string{
						leaderworkerset.WorkerIndexLabelKey: "0",
					},
					Annotations: map[string]string{
						LeaderRequestsTPUsAnnotationKey: "true",
					},
				},
			},
			size:                        2,
			expectedTpuWorkerId:         "0",
			expectedTpuWorkerHostNames:  "test-sample-0.default,test-sample-0-1.default",
			expectedTpuName:             "test-sample-0",
			expectedTpuProcessAddresses: "test-sample-0.default:8476,test-sample-0-1.default:8476",
			expectedTpuProcessPort:      "8476",
		},
		{
			name: "Leader requests TPU resources, worker pod",
			pod: &corev1.Pod{
				Spec: wrappers.MakeLeaderPodSpecWithTPU(),
				ObjectMeta: v1.ObjectMeta{
					Name:      "test-sample-0-0",
					Namespace: "default",
					Labels: map[string]string{
						leaderworkerset.WorkerIndexLabelKey: "1",
					},
					Annotations: map[string]string{
						LeaderRequestsTPUsAnnotationKey: "true",
					},
				},
			},
			size:                        2,
			expectedTpuWorkerId:         "1",
			expectedTpuWorkerHostNames:  "test-sample-0.default,test-sample-0-1.default",
			expectedTpuName:             "test-sample-0",
			expectedTpuProcessAddresses: "test-sample-0.default:8476,test-sample-0-1.default:8476",
			expectedTpuProcessPort:      "8476",
		},
		{
			name: "Leader pod always in TPU mesh",
			pod: &corev1.Pod{
				Spec: wrappers.MakeLeaderPodSpecWithTPU(),
				ObjectMeta: v1.ObjectMeta{
					Name:      "test-sample-0",
					Namespace: "default",
					Labels: map[string]string{
						leaderworkerset.WorkerIndexLabelKey: "0",
					},
				},
			},
			size:                        2,
			expectedTpuWorkerId:         "0",
			expectedTpuWorkerHostNames:  "test-sample-0.default,test-sample-0-1.default",
			expectedTpuName:             "test-sample-0",
			expectedTpuProcessAddresses: "test-sample-0.default:8476,test-sample-0-1.default:8476",
			expectedTpuProcessPort:      "8476",
		},
		{
			name: "Leader does not request TPU resources, worker pod",
			pod: &corev1.Pod{
				Spec: wrappers.MakeLeaderPodSpecWithTPU(),
				ObjectMeta: v1.ObjectMeta{
					Name:      "test-sample-0-0",
					Namespace: "default",
					Labels: map[string]string{
						leaderworkerset.WorkerIndexLabelKey: "1",
					},
				},
			},
			size:                        2,
			expectedTpuWorkerId:         "0",
			expectedTpuWorkerHostNames:  "test-sample-0-1.default",
			expectedTpuName:             "test-sample-0",
			expectedTpuProcessAddresses: "test-sample-0-1.default:8476",
			expectedTpuProcessPort:      "8476",
		},
		{
			name: "Test multi-container TPU_PROCESS_ADDRESSES",
			pod: &corev1.Pod{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						wrappers.MakeContainerWithTPU("c1"),
						wrappers.MakeContainerWithTPU("c2"),
					},
					Subdomain: "default",
				},
				ObjectMeta: v1.ObjectMeta{
					Name:      "test-sample-0",
					Namespace: "default",
					Labels: map[string]string{
						leaderworkerset.WorkerIndexLabelKey: "0",
					},
					Annotations: map[string]string{
						LeaderRequestsTPUsAnnotationKey: "true",
					},
				},
			},
			size:                        2,
			expectedTpuWorkerId:         "0",
			expectedTpuWorkerHostNames:  "test-sample-0.default,test-sample-0.default,test-sample-0-1.default,test-sample-0-1.default",
			expectedTpuName:             "test-sample-0",
			expectedTpuProcessAddresses: "test-sample-0.default:8476,test-sample-0.default:8477,test-sample-0-1.default:8476,test-sample-0-1.default:8477",
			expectedTpuProcessPort:      "8476",
		},
		{
			name: "Test multi-container TPU_PROCESS_ADDRESSES with custom ports",
			pod: &corev1.Pod{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						wrappers.MakeContainerWithTPUAndEnvVars("c1", corev1.EnvVar{Name: TpuProcessPortName, Value: "8478"}),
						wrappers.MakeContainerWithTPUAndEnvVars("c2", corev1.EnvVar{Name: TpuProcessPortName, Value: "8479"}),
					},
					Subdomain: "default",
				},
				ObjectMeta: v1.ObjectMeta{
					Name:      "test-sample-0",
					Namespace: "default",
					Labels: map[string]string{
						leaderworkerset.WorkerIndexLabelKey: "0",
					},
					Annotations: map[string]string{
						LeaderRequestsTPUsAnnotationKey: "true",
					},
				},
			},
			size:                        2,
			expectedTpuWorkerId:         "0",
			expectedTpuWorkerHostNames:  "test-sample-0.default,test-sample-0.default,test-sample-0-1.default,test-sample-0-1.default",
			expectedTpuName:             "test-sample-0",
			expectedTpuProcessAddresses: "test-sample-0.default:8478,test-sample-0.default:8479,test-sample-0-1.default:8478,test-sample-0-1.default:8479",
			expectedTpuProcessPort:      "8478",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Ensure subdomain is set for consistency in tests
			if tc.pod.Spec.Subdomain == "" {
				tc.pod.Spec.Subdomain = "default"
			}
			err := AddTPUVariables(tc.pod, tc.size)
			if err != nil {
				t.Errorf("Error parsing parent: %s", err.Error())
			}
			containers := getContainersRequestingTPUs(&tc.pod.Spec)
			for i, container := range containers {
				var podWorkerIndex int
				if tc.pod.Labels[leaderworkerset.WorkerIndexLabelKey] == "0" {
					podWorkerIndex = 0
				} else {
					_, podWorkerIndex = statefulsetutils.GetParentNameAndOrdinal(tc.pod.Name)
					if tc.pod.Annotations[LeaderRequestsTPUsAnnotationKey] != "true" {
						podWorkerIndex = podWorkerIndex - 1
					}
				}

				foundTpuName, tpuName := podutils.GetEnvVarValueIfInContainer(container, TpuName)
				if !foundTpuName || tpuName != tc.expectedTpuName {
					t.Errorf("Expected TPU_NAME: %s, found %s", tc.expectedTpuName, tpuName)
				}
				foundTpuWorkerId, tpuWorkerId := podutils.GetEnvVarValueIfInContainer(container, TpuWorkerId)
				if !foundTpuWorkerId || tpuWorkerId != strconv.Itoa(i+len(containers)*podWorkerIndex) {
					t.Errorf("Expected TPU_WORKER_ID: %d, found %s", i+len(containers)*podWorkerIndex, tpuWorkerId)
				}
				foundTpuWorkerHostNames, tpuWorkerHostNames := podutils.GetEnvVarValueIfInContainer(container, TpuWorkerHostNames)
				if !foundTpuWorkerHostNames || tpuWorkerHostNames != tc.expectedTpuWorkerHostNames {
					t.Errorf("Expected TPU_WORKER_HOSTNAMES: %s, found %s", tc.expectedTpuWorkerHostNames, tpuWorkerHostNames)
				}
				foundTpuProcessAddresses, tpuProcessAddresses := podutils.GetEnvVarValueIfInContainer(container, TpuProcessAddresses)
				if !foundTpuProcessAddresses || tpuProcessAddresses != tc.expectedTpuProcessAddresses {
					t.Errorf("Expected TPU_PROCESS_ADDRESSES: %s, found %s", tc.expectedTpuProcessAddresses, tpuProcessAddresses)
				}
				foundTpuProcessPort, tpuProcessPort := podutils.GetEnvVarValueIfInContainer(container, TpuProcessPortName)
				// For multi-container, we only check the port of the first container against expectedTpuProcessPort
				if i == 0 {
					if !foundTpuProcessPort || tpuProcessPort != tc.expectedTpuProcessPort {
						t.Errorf("Expected TPU_PROCESS_PORT: %s, found %s", tc.expectedTpuProcessPort, tpuProcessPort)
					}
				}
			}
		})
	}
}

func TestAddTPUVariablesSubGroup(t *testing.T) {
	tests := []struct {
		name                        string
		pod                         *corev1.Pod
		expectedTpuWorkerHostNames  string
		expectedTpuWorkerId         string
		expectedTpuName             string
		expectedTpuProcessAddresses string
		expectedTpuProcessPort      string
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
			expectedTpuWorkerId:         "3",
			expectedTpuWorkerHostNames:  "test-sample-1.default,test-sample-1-1.default,test-sample-1-2.default,test-sample-1-3.default,test-sample-1-4.default",
			expectedTpuName:             "test-sample-1",
			expectedTpuProcessAddresses: "test-sample-1.default:8476,test-sample-1-1.default:8476,test-sample-1-2.default:8476,test-sample-1-3.default:8476,test-sample-1-4.default:8476",
			expectedTpuProcessPort:      "8476",
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
			expectedTpuWorkerId:         "3",
			expectedTpuWorkerHostNames:  "test-sample-1-4.default,test-sample-1-5.default,test-sample-1-6.default,test-sample-1-7.default",
			expectedTpuName:             "test-sample-1",
			expectedTpuProcessAddresses: "test-sample-1-4.default:8476,test-sample-1-5.default:8476,test-sample-1-6.default:8476,test-sample-1-7.default:8476",
			expectedTpuProcessPort:      "8476",
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
			expectedTpuWorkerId:         "0",
			expectedTpuWorkerHostNames:  "test-sample-1-5.default,test-sample-1-6.default,test-sample-1-7.default,test-sample-1-8.default",
			expectedTpuName:             "test-sample-1",
			expectedTpuProcessAddresses: "test-sample-1-5.default:8476,test-sample-1-6.default:8476,test-sample-1-7.default:8476,test-sample-1-8.default:8476",
			expectedTpuProcessPort:      "8476",
		},
		{
			name: "Leader does not request TPU resources, worker with subgroup index > 0, LeaderOnly Subgroup",
			pod: &corev1.Pod{
				Spec: wrappers.MakeLeaderPodSpecWithTPUResource(),
				ObjectMeta: v1.ObjectMeta{
					Name:      "test-sample-0-2",
					Namespace: "default",
					Labels: map[string]string{
						leaderworkerset.WorkerIndexLabelKey:   "2",
						leaderworkerset.SubGroupIndexLabelKey: "0",
					},
					Annotations: map[string]string{
						leaderworkerset.SubGroupSizeAnnotationKey:       "2",
						leaderworkerset.SubGroupPolicyTypeAnnotationKey: string(leaderworkerset.SubGroupPolicyTypeLeaderExcluded),
					},
				},
			},
			expectedTpuWorkerId:         "1",
			expectedTpuWorkerHostNames:  "test-sample-0-1.default,test-sample-0-2.default",
			expectedTpuName:             "test-sample-0",
			expectedTpuProcessAddresses: "test-sample-0-1.default:8476,test-sample-0-2.default:8476",
			expectedTpuProcessPort:      "8476",
		},
		{
			name: "Leader does not request TPU resources, worker with subgroup index > 0, TPU_PROCESS_PORT is already set",
			pod: &corev1.Pod{
				Spec: wrappers.MakeLeaderPodSpecWithTPUAndEnvVars(corev1.EnvVar{Name: TpuProcessPortName, Value: "8478"}),
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
			expectedTpuWorkerId:         "0",
			expectedTpuWorkerHostNames:  "test-sample-1-5.default,test-sample-1-6.default,test-sample-1-7.default,test-sample-1-8.default",
			expectedTpuName:             "test-sample-1",
			expectedTpuProcessAddresses: "test-sample-1-5.default:8478,test-sample-1-6.default:8478,test-sample-1-7.default:8478,test-sample-1-8.default:8478",
			expectedTpuProcessPort:      "8478",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := addTPUVariablesSubGroup(tc.pod)
			if err != nil {
				t.Errorf("Error parsing parent: %s", err.Error())
			}
			for _, envVar := range tc.pod.Spec.Containers[0].Env {
				switch envVar.Name {
				case TpuWorkerHostNames:
					if diff := cmp.Diff(envVar.Value, tc.expectedTpuWorkerHostNames); diff != "" {
						t.Errorf("unexpected add TPU worker host names operation %s", diff)
					}
				case TpuWorkerId:
					if diff := cmp.Diff(envVar.Value, tc.expectedTpuWorkerId); diff != "" {
						t.Errorf("unexpected add TPU worker ID operation: %s", diff)
					}
				case TpuName:
					if diff := cmp.Diff(envVar.Value, tc.expectedTpuName); diff != "" {
						t.Errorf("unexpected add TPU worker ID operation: %s", diff)
					}
				case TpuProcessAddresses:
					if diff := cmp.Diff(envVar.Value, tc.expectedTpuProcessAddresses); diff != "" {
						t.Errorf("unexpected add TPU worker ID operation: %s", diff)
					}
				case TpuProcessPortName:
					if diff := cmp.Diff(envVar.Value, tc.expectedTpuProcessPort); diff != "" {
						t.Errorf("unexpected add TPU worker ID operation: %s", diff)
					}
				}
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

func TestGetContainersRequestingTPUs(t *testing.T) {
	tests := []struct {
		name                 string
		podSpec              *corev1.PodSpec
		expectedNumContainer int
	}{
		{
			name: "pod requests TPU resources",
			podSpec: &corev1.PodSpec{
				Containers: []corev1.Container{
					wrappers.MakeContainerWithTPU("c1"),
				},
			},
			expectedNumContainer: 1,
		},
		{
			name: "pod does not request TPU resources",
			podSpec: &corev1.PodSpec{
				Containers: []corev1.Container{
					{
						Name: "c1",
					},
				},
			},
			expectedNumContainer: 0,
		},
		{
			name: "pod requests multiple TPU resources",
			podSpec: &corev1.PodSpec{
				Containers: []corev1.Container{
					wrappers.MakeContainerWithTPU("c1"),
					wrappers.MakeContainerWithTPU("c2"),
				},
			},
			expectedNumContainer: 2,
		},
		{
			name: "mix containers",
			podSpec: &corev1.PodSpec{
				Containers: []corev1.Container{
					wrappers.MakeContainerWithTPU("c1"),
					{
						Name: "c2",
					},
				},
			},
			expectedNumContainer: 1,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			containers := getContainersRequestingTPUs(tc.podSpec)
			if len(containers) != tc.expectedNumContainer {
				t.Fatalf("Expected %d containers, found %d", tc.expectedNumContainer, len(containers))
			}
		})
	}
}

func TestPodRequestsTPUs(t *testing.T) {
	tests := []struct {
		name     string
		podSpec  corev1.PodSpec
		expected bool
	}{
		{
			name: "requests in containers",
			podSpec: corev1.PodSpec{
				Containers: []corev1.Container{
					wrappers.MakeContainerWithTPU("c1"),
				},
			},
			expected: true,
		},
		{
			name: "requests in init containers",
			podSpec: corev1.PodSpec{
				InitContainers: []corev1.Container{
					wrappers.MakeContainerWithTPU("c1"),
				},
			},
			expected: true,
		},
		{
			name:     "no requests",
			podSpec:  corev1.PodSpec{},
			expected: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if PodRequestsTPUs(tc.podSpec) != tc.expected {
				t.Errorf("Expected %v, got %v", tc.expected, !tc.expected)
			}
		})
	}
}

func TestAddTPUVariablesSkipExisting(t *testing.T) {
	podWithEnv := &corev1.Pod{
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name: "c1",
					Resources: corev1.ResourceRequirements{
						Limits: corev1.ResourceList{TpuResourceName: resource.MustParse("1")},
					},
					Env: []corev1.EnvVar{{Name: TpuWorkerId, Value: "0"}},
				},
			},
		},
	}
	if err := AddTPUVariables(podWithEnv, 1); err != nil {
		t.Errorf("Unexpected error: %v", err)
	}
	if len(podWithEnv.Spec.Containers[0].Env) != 1 {
		t.Errorf("Expected skip injection, but env changed: %v", podWithEnv.Spec.Containers[0].Env)
	}
}

func TestAddTPUVariablesInvalidPodName(t *testing.T) {
	invalidPod := &corev1.Pod{
		ObjectMeta: v1.ObjectMeta{
			Name: "invalidname", // No index
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				wrappers.MakeContainerWithTPU("c1"),
			},
		},
	}
	if err := AddTPUVariables(invalidPod, 1); err == nil {
		t.Error("Expected error for invalid pod name, got nil")
	}
}

func TestAddTPUVariablesWithRequests(t *testing.T) {
	podWithRequests := &corev1.Pod{
		ObjectMeta: v1.ObjectMeta{
			Name: "sample-0",
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name: "c1",
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{TpuResourceName: resource.MustParse("1")},
					},
				},
			},
		},
	}
	if err := AddTPUVariables(podWithRequests, 1); err != nil {
		t.Errorf("Unexpected error: %v", err)
	}
	found := false
	for _, env := range podWithRequests.Spec.Containers[0].Env {
		if env.Name == TpuWorkerId {
			found = true
			break
		}
	}
	if !found {
		t.Error("TPU variables not injected for Request-based TPU spec")
	}
}

func TestAddTPUVariablesSubGroupSkipExisting(t *testing.T) {
	podWithEnv := &corev1.Pod{
		ObjectMeta: v1.ObjectMeta{
			Annotations: map[string]string{leaderworkerset.SubGroupSizeAnnotationKey: "1"},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name: "c1",
					Resources: corev1.ResourceRequirements{
						Limits: corev1.ResourceList{TpuResourceName: resource.MustParse("1")},
					},
					Env: []corev1.EnvVar{{Name: TpuWorkerId, Value: "0"}},
				},
			},
		},
	}
	if err := AddTPUVariables(podWithEnv, 1); err != nil {
		t.Errorf("Unexpected error: %v", err)
	}
}

func TestAddTPUVariablesSubGroupInvalidSize(t *testing.T) {
	invalidSize := &corev1.Pod{
		ObjectMeta: v1.ObjectMeta{
			Annotations: map[string]string{leaderworkerset.SubGroupSizeAnnotationKey: "invalid"},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{wrappers.MakeContainerWithTPU("c1")},
		},
	}
	if err := AddTPUVariables(invalidSize, 1); err == nil {
		t.Error("Expected error for invalid subgroup size, got nil")
	}
}

func TestAddTPUVariablesSubGroupInvalidIndex(t *testing.T) {
	invalidIndex := &corev1.Pod{
		ObjectMeta: v1.ObjectMeta{
			Annotations: map[string]string{leaderworkerset.SubGroupSizeAnnotationKey: "1"},
			Labels:      map[string]string{leaderworkerset.SubGroupIndexLabelKey: "invalid"},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{wrappers.MakeContainerWithTPU("c1")},
		},
	}
	if err := AddTPUVariables(invalidIndex, 1); err == nil {
		t.Error("Expected error for invalid subgroup index, got nil")
	}
}

func TestAddTPUVariablesSubGroupInvalidWorkerIndex(t *testing.T) {
	invalidWorker := &corev1.Pod{
		ObjectMeta: v1.ObjectMeta{
			Annotations: map[string]string{leaderworkerset.SubGroupSizeAnnotationKey: "1"},
			Labels: map[string]string{
				leaderworkerset.SubGroupIndexLabelKey: "0",
				leaderworkerset.WorkerIndexLabelKey:   "invalid",
			},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{wrappers.MakeContainerWithTPU("c1")},
		},
	}
	if err := AddTPUVariables(invalidWorker, 1); err == nil {
		t.Error("Expected error for invalid worker index, got nil")
	}
}

func TestAddTPUVariablesSubGroupInvalidParentName(t *testing.T) {
	invalidParent := &corev1.Pod{
		ObjectMeta: v1.ObjectMeta{
			Name:        "invalidname",
			Annotations: map[string]string{leaderworkerset.SubGroupSizeAnnotationKey: "1"},
			Labels: map[string]string{
				leaderworkerset.SubGroupIndexLabelKey: "0",
				leaderworkerset.WorkerIndexLabelKey:   "1",
			},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{wrappers.MakeContainerWithTPU("c1")},
		},
	}
	if err := AddTPUVariables(invalidParent, 1); err == nil {
		t.Error("Expected error for invalid parent name in subgroup, got nil")
	}
}

func TestGetContainersRequestingTPUsInitCoverage(t *testing.T) {
	pod := &corev1.PodSpec{
		InitContainers: []corev1.Container{
			wrappers.MakeContainerWithTPU("i1"),
		},
	}
	containers := getContainersRequestingTPUs(pod)
	if len(containers) != 1 || containers[0].Name != "i1" {
		t.Errorf("Expected 1 init container, got %d", len(containers))
	}
}

func TestAddTPUVariablesZeroContainers(t *testing.T) {
	pod := &corev1.Pod{
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "c1"}},
		},
	}
	if err := AddTPUVariables(pod, 1); err != nil {
		t.Errorf("Unexpected error: %v", err)
	}
	if len(pod.Spec.Containers[0].Env) != 0 {
		t.Error("Expected no env vars added for pod without TPU containers")
	}
}

func TestAddTPUVariablesInitContainers(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: v1.ObjectMeta{
			Name: "sample-0",
		},
		Spec: corev1.PodSpec{
			InitContainers: []corev1.Container{
				wrappers.MakeContainerWithTPU("i1"),
			},
			Subdomain: "default",
		},
	}
	if err := AddTPUVariables(pod, 1); err != nil {
		t.Errorf("Unexpected error: %v", err)
	}
	if len(pod.Spec.InitContainers[0].Env) == 0 {
		t.Error("Expected env vars added to init container")
	}
}

func TestAddTPUVariablesSubGroupSkip(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: v1.ObjectMeta{
			Annotations: map[string]string{leaderworkerset.SubGroupSizeAnnotationKey: "1"},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name: "c1",
					Resources: corev1.ResourceRequirements{
						Limits: corev1.ResourceList{TpuResourceName: resource.MustParse("1")},
					},
					Env: []corev1.EnvVar{{Name: TpuWorkerHostNames, Value: "alreadySet"}},
				},
			},
		},
	}
	if err := AddTPUVariables(pod, 1); err != nil {
		t.Errorf("Unexpected error: %v", err)
	}
	if len(pod.Spec.Containers[0].Env) != 1 {
		t.Error("Expected skip injection in subgroup, but env changed")
	}
}

func TestAddTPUVariablesSubGroupLeader(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: v1.ObjectMeta{
			Name: "sample-0",
			Labels: map[string]string{
				leaderworkerset.WorkerIndexLabelKey:   "0",
				leaderworkerset.SubGroupIndexLabelKey: "0",
			},
			Annotations: map[string]string{
				leaderworkerset.SubGroupSizeAnnotationKey: "2",
			},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{wrappers.MakeContainerWithTPU("c1")},
		},
	}
	if err := AddTPUVariables(pod, 2); err != nil {
		t.Errorf("Unexpected error: %v", err)
	}
	// Check if hostname contains leader hostname
	found := false
	for _, env := range pod.Spec.Containers[0].Env {
		if env.Name == TpuWorkerHostNames && strings.Contains(env.Value, "sample-0") {
			found = true
			break
		}
	}
	if !found {
		t.Error("Expected leader hostname in TPU_WORKER_HOSTNAMES for subgroup leader")
	}
}

func TestAddTPUVariablesSubGroupErrorParent(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: v1.ObjectMeta{
			Name: "invalidname", // No index
			Labels: map[string]string{
				leaderworkerset.WorkerIndexLabelKey:   "1",
				leaderworkerset.SubGroupIndexLabelKey: "0",
			},
			Annotations: map[string]string{
				leaderworkerset.SubGroupSizeAnnotationKey: "2",
			},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{wrappers.MakeContainerWithTPU("c1")},
		},
	}
	if err := AddTPUVariables(pod, 2); err == nil {
		t.Error("Expected error for invalid parent name in subgroup worker, got nil")
	}
}

func TestAddTPUVariablesSubGroupZeroContainers(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: v1.ObjectMeta{
			Annotations: map[string]string{leaderworkerset.SubGroupSizeAnnotationKey: "1"},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "c1"}},
		},
	}
	if err := AddTPUVariables(pod, 1); err != nil {
		t.Errorf("Unexpected error: %v", err)
	}
}

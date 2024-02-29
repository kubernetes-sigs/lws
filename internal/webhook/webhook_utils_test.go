package webhook

import (
	"testing"

	"github.com/google/go-cmp/cmp"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
	}{
		{
			name: "Worker Index is 0",
			pod: &corev1.Pod{
				Spec: MakeLeaderPodSpecWithTPUResource(),
				ObjectMeta: v1.ObjectMeta{
					Name:      "test-sample-1",
					Namespace: "default",
					Labels: map[string]string{
						"leaderworkerset.sigs.k8s.io/worker-index": "0",
					},
				},
			},
			size:                       1,
			hasWorkerIndexLabelKey:     true,
			expectedTpuWorkerHostNames: "test-sample-1.default",
			expectedTpuWorkerId:        "0",
		},
		{
			name: "Worker Index is non-zero, size is above 2",
			pod: &corev1.Pod{
				Spec: MakeLeaderPodSpecWithTPUResource(),
				ObjectMeta: v1.ObjectMeta{
					Name:      "test-sample-1-3",
					Namespace: "default",
					Labels: map[string]string{
						"leaderworkerset.sigs.k8s.io/worker-index": "3",
					},
				},
			},
			size:                       5,
			hasWorkerIndexLabelKey:     true,
			expectedTpuWorkerHostNames: "test-sample-1.default,test-sample-1-1.default,test-sample-1-2.default,test-sample-1-3.default,test-sample-1-4.default",
			expectedTpuWorkerId:        "3",
		},
		{
			name: "No Worker Index included",
			pod: &corev1.Pod{
				Spec: MakeLeaderPodSpecWithTPUResource(),
				ObjectMeta: v1.ObjectMeta{
					Name:      "test-sample-1-3",
					Namespace: "default",
				},
			},
			size:                   1,
			hasWorkerIndexLabelKey: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := addTPUVariables(tc.pod, tc.size)
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
			podSpec: MakeLeaderPodSpecWithTPUResource(),
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
			podSpec: MakeLeaderPodSpecWithTPUResourceMultipleContainers(),
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
			podSpec:           MakeLeaderPodSpec(),
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

func TestGenGroupUniqueKey(t *testing.T) {
	tests := []struct {
		name        string
		podName     string
		namespace   string
		expectedKey string
	}{
		{
			name:        "same namespace, pod name",
			podName:     "test-sample",
			namespace:   "default",
			expectedKey: "95e88034e460983f51a9952fe128729fbc0663b5",
		},
		{
			name:        "same namespace, different pod name",
			podName:     "podName",
			namespace:   "default",
			expectedKey: "390b34ab671d29e9997d7d4252b8bbf8da02f5b7",
		},
		{
			name:        "different namespace, same pod name",
			podName:     "test-sample",
			namespace:   "leaderworkerset",
			expectedKey: "39f5d7e9122b9d94d3932e3720b43fd3b56347e8",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			key := genGroupUniqueKey(tc.namespace, tc.podName)
			if diff := cmp.Diff(tc.expectedKey, key); diff != "" {
				t.Errorf("unexpected key %s", diff)
			}
		})
	}
}

func TestSetExclusiveAffinities(t *testing.T) {
	tests := []struct {
		name           string
		pod            *corev1.Pod
		groupUniqueKey string
		expectedPod    *corev1.Pod
	}{
		{
			name: "Pod with only Exclusive Topology Annotation",
			pod: &corev1.Pod{
				ObjectMeta: v1.ObjectMeta{
					Annotations: map[string]string{"leaderworkerset.sigs.k8s.io/exclusive-topology": "topologyKey"},
				},
			},
			groupUniqueKey: "test-key",
			expectedPod: &corev1.Pod{
				ObjectMeta: v1.ObjectMeta{
					Annotations: map[string]string{"leaderworkerset.sigs.k8s.io/exclusive-topology": "topologyKey"},
				},
				Spec: corev1.PodSpec{
					Affinity: &corev1.Affinity{
						PodAffinity: &corev1.PodAffinity{
							RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{{
								TopologyKey: "topologyKey",
								LabelSelector: &metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{
									{
										Key:      "leaderworkerset.sigs.k8s.io/group-key",
										Operator: "In",
										Values:   []string{"test-key"},
									},
								}},
							}},
						},
						PodAntiAffinity: &corev1.PodAntiAffinity{
							RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{{
								TopologyKey: "topologyKey",
								LabelSelector: &metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{
									{
										Key:      "leaderworkerset.sigs.k8s.io/group-key",
										Operator: "Exists",
									},
									{
										Key:      "leaderworkerset.sigs.k8s.io/group-key",
										Operator: "NotIn",
										Values:   []string{"test-key"},
									},
								}},
							}},
						},
					},
				},
			},
		},
		{
			name: "Pod with Exclusive Annotation, Affinity, and AntiAffinity",
			pod: &corev1.Pod{
				ObjectMeta: v1.ObjectMeta{
					Annotations: map[string]string{"leaderworkerset.sigs.k8s.io/exclusive-topology": "topologyKey"},
				},
				Spec: corev1.PodSpec{
					Affinity: &corev1.Affinity{
						PodAffinity: &corev1.PodAffinity{
							RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{{TopologyKey: "topologyKey"}},
						},
						PodAntiAffinity: &corev1.PodAntiAffinity{
							RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{{TopologyKey: "topologyKey"}},
						},
					},
				},
			},
			groupUniqueKey: "test-key",
			expectedPod: &corev1.Pod{
				ObjectMeta: v1.ObjectMeta{
					Annotations: map[string]string{"leaderworkerset.sigs.k8s.io/exclusive-topology": "topologyKey"},
				},
				Spec: corev1.PodSpec{
					Affinity: &corev1.Affinity{
						PodAffinity: &corev1.PodAffinity{
							RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{
								{
									TopologyKey: "topologyKey",
								},
								{
									TopologyKey: "topologyKey",
									LabelSelector: &metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{
										{
											Key:      "leaderworkerset.sigs.k8s.io/group-key",
											Operator: "In",
											Values:   []string{"test-key"},
										},
									}},
								},
							},
						},
						PodAntiAffinity: &corev1.PodAntiAffinity{
							RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{
								{
									TopologyKey: "topologyKey",
								},
								{
									TopologyKey: "topologyKey",
									LabelSelector: &metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{
										{
											Key:      "leaderworkerset.sigs.k8s.io/group-key",
											Operator: "Exists",
										},
										{
											Key:      "leaderworkerset.sigs.k8s.io/group-key",
											Operator: "NotIn",
											Values:   []string{"test-key"},
										},
									}},
								},
							},
						},
					},
				},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			SetExclusiveAffinities(tc.pod, tc.groupUniqueKey)
			if diff := cmp.Diff(tc.pod, tc.expectedPod); diff != "" {
				t.Errorf("unexpected set exclusive affinities operation: %s", diff)
			}
		})
	}
}

func TestExclusiveAffinityApplied(t *testing.T) {
	tests := []struct {
		name                              string
		pod                               corev1.Pod
		expectedAppliedExclusivePlacement bool
	}{
		{
			name: "Has annotiation, Pod Affinity and Pod AntiAffinity",
			pod: corev1.Pod{
				ObjectMeta: v1.ObjectMeta{
					Annotations: map[string]string{
						"leaderworkerset.sigs.k8s.io/exclusive-topology": "topologyKey",
					},
				},
				Spec: corev1.PodSpec{
					Affinity: &corev1.Affinity{
						PodAffinity: &corev1.PodAffinity{
							RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{{TopologyKey: "topologyKey"}},
						},
						PodAntiAffinity: &corev1.PodAntiAffinity{
							RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{{TopologyKey: "topologyKey"}},
						},
					},
				},
			},
			expectedAppliedExclusivePlacement: true,
		},
		{
			name: "Has annotiation, Pod Affinity, doesn't have Pod AntiAffinity",
			pod: corev1.Pod{
				ObjectMeta: v1.ObjectMeta{
					Annotations: map[string]string{
						"leaderworkerset.sigs.k8s.io/exclusive-topology": "topologyKey",
					},
				},
				Spec: corev1.PodSpec{
					Affinity: &corev1.Affinity{
						PodAffinity: &corev1.PodAffinity{
							RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{{TopologyKey: "topologyKey"}},
						},
					},
				},
			},
			expectedAppliedExclusivePlacement: false,
		},
		{
			name: "Has annotiation, Pod AntiAffinity, doesn't have Pod Affinity",
			pod: corev1.Pod{
				ObjectMeta: v1.ObjectMeta{
					Annotations: map[string]string{
						"leaderworkerset.sigs.k8s.io/exclusive-topology": "topologyKey",
					},
				},
				Spec: corev1.PodSpec{
					Affinity: &corev1.Affinity{
						PodAntiAffinity: &corev1.PodAntiAffinity{
							RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{{TopologyKey: "topologyKey"}},
						},
					},
				},
			},
			expectedAppliedExclusivePlacement: false,
		},
		{
			name: "Has annotiation, Pod Affinity and Pod AntiAffinity, Topology Key doesn't match",
			pod: corev1.Pod{
				ObjectMeta: v1.ObjectMeta{
					Annotations: map[string]string{
						"leaderworkerset.sigs.k8s.io/exclusive-topology": "topologyKey",
					},
				},
				Spec: corev1.PodSpec{
					Affinity: &corev1.Affinity{
						PodAffinity: &corev1.PodAffinity{
							RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{{TopologyKey: "topologyKey1"}},
						},
						PodAntiAffinity: &corev1.PodAntiAffinity{
							RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{{TopologyKey: "topologyKey"}},
						},
					},
				},
			},
			expectedAppliedExclusivePlacement: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			appliedExclusivePlacement := exclusiveAffinityApplied(tc.pod)
			if appliedExclusivePlacement != tc.expectedAppliedExclusivePlacement {
				t.Errorf("Expected value %t, got %t", tc.expectedAppliedExclusivePlacement, appliedExclusivePlacement)
			}
		})
	}
}

func MakeLeaderPodSpec() corev1.PodSpec {
	return corev1.PodSpec{
		Containers: []corev1.Container{
			{
				Name:  "worker",
				Image: "busybox",
			},
		},
	}
}

func MakeLeaderPodSpecWithTPUResource() corev1.PodSpec {
	return corev1.PodSpec{
		Containers: []corev1.Container{
			{
				Name:  "worker",
				Image: "busybox",
				Resources: corev1.ResourceRequirements{
					Limits: map[corev1.ResourceName]resource.Quantity{
						corev1.ResourceName("google.com/tpu"): resource.MustParse("4"),
					},
				},
			},
		},
		Subdomain: "default",
	}
}

func MakeLeaderPodSpecWithTPUResourceMultipleContainers() corev1.PodSpec {
	return corev1.PodSpec{
		Containers: []corev1.Container{
			{
				Name:  "worker",
				Image: "busybox",
				Resources: corev1.ResourceRequirements{
					Limits: map[corev1.ResourceName]resource.Quantity{
						corev1.ResourceName("google.com/tpu"): resource.MustParse("4"),
					},
				},
			},
			{
				Name:  "leader",
				Image: "nginx:1.14.2",
				Ports: []corev1.ContainerPort{
					{
						ContainerPort: 8080,
						Protocol:      "TCP",
					},
				},
			},
		},
		Subdomain: "default",
	}
}

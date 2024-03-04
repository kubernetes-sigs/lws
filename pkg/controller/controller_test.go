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

package controller

import (
	"testing"

	"github.com/google/go-cmp/cmp"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	appsapplyv1 "k8s.io/client-go/applyconfigurations/apps/v1"
	coreapplyv1 "k8s.io/client-go/applyconfigurations/core/v1"
	metaapplyv1 "k8s.io/client-go/applyconfigurations/meta/v1"
	"k8s.io/utils/ptr"

	leaderworkerset "sigs.k8s.io/lws/api/leaderworkerset/v1"
	testutils "sigs.k8s.io/lws/test/testutils"
)

func TestLeaderStatefulSetApplyConfig(t *testing.T) {
	tests := []struct {
		name            string
		lws             *leaderworkerset.LeaderWorkerSet
		wantApplyConfig *appsapplyv1.StatefulSetApplyConfiguration
	}{
		{
			name: "1 replica, size 1, with empty leader template, exclusive placement disabled",
			lws: testutils.BuildBasicLeaderWorkerSet("test-sample", "default").
				Replica(1).
				WorkerTemplateSpec(testutils.MakeWorkerPodSpec()).
				Size(1).
				RestartPolicy(leaderworkerset.RecreateGroupOnPodRestart).Obj(),
			wantApplyConfig: &appsapplyv1.StatefulSetApplyConfiguration{
				TypeMetaApplyConfiguration: metaapplyv1.TypeMetaApplyConfiguration{
					Kind:       ptr.To[string]("StatefulSet"),
					APIVersion: ptr.To[string]("apps/v1"),
				},
				ObjectMetaApplyConfiguration: &metaapplyv1.ObjectMetaApplyConfiguration{
					Name:      ptr.To[string]("test-sample"),
					Namespace: ptr.To[string]("default"),
					Labels: map[string]string{
						"leaderworkerset.sigs.k8s.io/name": "test-sample",
					},
				},
				Spec: &appsapplyv1.StatefulSetSpecApplyConfiguration{
					Replicas: ptr.To[int32](1),
					Selector: &metaapplyv1.LabelSelectorApplyConfiguration{
						MatchLabels: map[string]string{
							"leaderworkerset.sigs.k8s.io/name":         "test-sample",
							"leaderworkerset.sigs.k8s.io/worker-index": "0",
						},
					},
					Template: &coreapplyv1.PodTemplateSpecApplyConfiguration{
						ObjectMetaApplyConfiguration: &metaapplyv1.ObjectMetaApplyConfiguration{
							Labels: map[string]string{
								"leaderworkerset.sigs.k8s.io/name":         "test-sample",
								"leaderworkerset.sigs.k8s.io/worker-index": "0",
							},
							Annotations: map[string]string{
								"leaderworkerset.sigs.k8s.io/size": "1",
							},
						},
						Spec: &coreapplyv1.PodSpecApplyConfiguration{
							Containers: []coreapplyv1.ContainerApplyConfiguration{
								{
									Name:      ptr.To[string]("leader"),
									Image:     ptr.To[string]("nginx:1.14.2"),
									Ports:     []coreapplyv1.ContainerPortApplyConfiguration{{ContainerPort: ptr.To[int32](8080), Protocol: ptr.To[corev1.Protocol](corev1.ProtocolTCP)}},
									Resources: &coreapplyv1.ResourceRequirementsApplyConfiguration{},
								},
							},
						},
					},
					ServiceName:         ptr.To[string]("test-sample"),
					PodManagementPolicy: ptr.To[appsv1.PodManagementPolicyType](appsv1.ParallelPodManagement),
				},
			},
		},
		{
			name: "1 replica, size 2 , with empty leader template, exclusive placement enabled",
			lws: testutils.BuildBasicLeaderWorkerSet("test-sample", "default").
				Annotation(map[string]string{
					"leaderworkerset.sigs.k8s.io/exclusive-topology": "cloud.google.com/gke-nodepool",
				}).Replica(1).
				WorkerTemplateSpec(testutils.MakeWorkerPodSpec()).
				Size(2).
				RestartPolicy(leaderworkerset.Default).Obj(),
			wantApplyConfig: &appsapplyv1.StatefulSetApplyConfiguration{
				TypeMetaApplyConfiguration: metaapplyv1.TypeMetaApplyConfiguration{
					Kind:       ptr.To[string]("StatefulSet"),
					APIVersion: ptr.To[string]("apps/v1"),
				},
				ObjectMetaApplyConfiguration: &metaapplyv1.ObjectMetaApplyConfiguration{
					Name:      ptr.To[string]("test-sample"),
					Namespace: ptr.To[string]("default"),
					Labels: map[string]string{
						"leaderworkerset.sigs.k8s.io/name": "test-sample",
					},
				},
				Spec: &appsapplyv1.StatefulSetSpecApplyConfiguration{
					Replicas: ptr.To[int32](1),
					Selector: &metaapplyv1.LabelSelectorApplyConfiguration{
						MatchLabels: map[string]string{
							"leaderworkerset.sigs.k8s.io/name":         "test-sample",
							"leaderworkerset.sigs.k8s.io/worker-index": "0",
						},
					},
					Template: &coreapplyv1.PodTemplateSpecApplyConfiguration{
						ObjectMetaApplyConfiguration: &metaapplyv1.ObjectMetaApplyConfiguration{
							Labels: map[string]string{
								"leaderworkerset.sigs.k8s.io/name":         "test-sample",
								"leaderworkerset.sigs.k8s.io/worker-index": "0",
							},
							Annotations: map[string]string{
								"leaderworkerset.sigs.k8s.io/size":               "2",
								"leaderworkerset.sigs.k8s.io/exclusive-topology": "cloud.google.com/gke-nodepool",
							},
						},
						Spec: &coreapplyv1.PodSpecApplyConfiguration{
							Containers: []coreapplyv1.ContainerApplyConfiguration{
								{
									Name:      ptr.To[string]("leader"),
									Image:     ptr.To[string]("nginx:1.14.2"),
									Ports:     []coreapplyv1.ContainerPortApplyConfiguration{{ContainerPort: ptr.To[int32](8080), Protocol: ptr.To[corev1.Protocol](corev1.ProtocolTCP)}},
									Resources: &coreapplyv1.ResourceRequirementsApplyConfiguration{},
								},
							},
						},
					},
					ServiceName:         ptr.To[string]("test-sample"),
					PodManagementPolicy: ptr.To[appsv1.PodManagementPolicyType](appsv1.ParallelPodManagement),
				},
			},
		},
		{
			name: "1 replica, size 2, with leader template, exclusive placement enabled",
			lws: testutils.BuildBasicLeaderWorkerSet("test-sample", "default").Annotation(map[string]string{
				"leaderworkerset.sigs.k8s.io/exclusive-topology": "cloud.google.com/gke-nodepool",
			}).Replica(1).
				WorkerTemplateSpec(testutils.MakeWorkerPodSpec()).
				LeaderTemplateSpec(testutils.MakeLeaderPodSpec()).
				Size(2).
				RestartPolicy(leaderworkerset.Default).Obj(),
			wantApplyConfig: &appsapplyv1.StatefulSetApplyConfiguration{
				TypeMetaApplyConfiguration: metaapplyv1.TypeMetaApplyConfiguration{
					Kind:       ptr.To[string]("StatefulSet"),
					APIVersion: ptr.To[string]("apps/v1"),
				},
				ObjectMetaApplyConfiguration: &metaapplyv1.ObjectMetaApplyConfiguration{
					Name:      ptr.To[string]("test-sample"),
					Namespace: ptr.To[string]("default"),
					Labels: map[string]string{
						"leaderworkerset.sigs.k8s.io/name": "test-sample",
					},
				},
				Spec: &appsapplyv1.StatefulSetSpecApplyConfiguration{
					Replicas: ptr.To[int32](1),
					Selector: &metaapplyv1.LabelSelectorApplyConfiguration{
						MatchLabels: map[string]string{
							"leaderworkerset.sigs.k8s.io/name":         "test-sample",
							"leaderworkerset.sigs.k8s.io/worker-index": "0",
						},
					},
					Template: &coreapplyv1.PodTemplateSpecApplyConfiguration{
						ObjectMetaApplyConfiguration: &metaapplyv1.ObjectMetaApplyConfiguration{
							Labels: map[string]string{
								"leaderworkerset.sigs.k8s.io/name":         "test-sample",
								"leaderworkerset.sigs.k8s.io/worker-index": "0",
							},
							Annotations: map[string]string{
								"leaderworkerset.sigs.k8s.io/size":               "2",
								"leaderworkerset.sigs.k8s.io/exclusive-topology": "cloud.google.com/gke-nodepool",
							},
						},
						Spec: &coreapplyv1.PodSpecApplyConfiguration{
							Containers: []coreapplyv1.ContainerApplyConfiguration{
								{
									Name:      ptr.To[string]("worker"),
									Image:     ptr.To[string]("busybox"),
									Resources: &coreapplyv1.ResourceRequirementsApplyConfiguration{},
								},
							},
						},
					},
					ServiceName:         ptr.To[string]("test-sample"),
					PodManagementPolicy: ptr.To[appsv1.PodManagementPolicyType](appsv1.ParallelPodManagement),
				},
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			stsApplyConfig, err := constructLeaderStatefulSetApplyConfiguration(tc.lws)
			if err != nil {
				t.Errorf("failed with error: %s", err.Error())
			}
			if diff := cmp.Diff(tc.wantApplyConfig, stsApplyConfig); diff != "" {
				t.Errorf("unexpected StatefulSet apply configuration: %s", diff)
			}
		})
	}
}

func TestConstructWorkerStatefulSetApplyConfiguration(t *testing.T) {
	tests := []struct {
		name                  string
		pod                   *corev1.Pod
		lws                   *leaderworkerset.LeaderWorkerSet
		wantStatefulSetConfig *appsapplyv1.StatefulSetApplyConfiguration
	}{
		{
			name: "1 replica, size 1, exclusive placement disabled",
			pod: &corev1.Pod{
				ObjectMeta: v1.ObjectMeta{
					Name:      "test-sample",
					Namespace: "default",
					Labels: map[string]string{
						"leaderworkerset.sigs.k8s.io/worker-index": "0",
						"leaderworkerset.sigs.k8s.io/name":         "test-sample",
						"leaderworkerset.sigs.k8s.io/group-index":  "1",
						"leaderworkerset.sigs.k8s.io/group-key":    "test-key",
					},
				},
			},
			lws: testutils.BuildBasicLeaderWorkerSet("test-sample", "default").
				Replica(1).
				WorkerTemplateSpec(testutils.MakeWorkerPodSpec()).
				Size(1).Obj(),
			wantStatefulSetConfig: &appsapplyv1.StatefulSetApplyConfiguration{
				TypeMetaApplyConfiguration: metaapplyv1.TypeMetaApplyConfiguration{
					Kind:       ptr.To[string]("StatefulSet"),
					APIVersion: ptr.To[string]("apps/v1"),
				},
				ObjectMetaApplyConfiguration: &metaapplyv1.ObjectMetaApplyConfiguration{
					Name:      ptr.To[string]("test-sample"),
					Namespace: ptr.To[string]("default"),
					Labels: map[string]string{
						"leaderworkerset.sigs.k8s.io/name":        "test-sample",
						"leaderworkerset.sigs.k8s.io/group-index": "1",
						"leaderworkerset.sigs.k8s.io/group-key":   "test-key",
					},
				},
				Spec: &appsapplyv1.StatefulSetSpecApplyConfiguration{
					Replicas: ptr.To[int32](0),
					Selector: &metaapplyv1.LabelSelectorApplyConfiguration{
						MatchLabels: map[string]string{
							"leaderworkerset.sigs.k8s.io/name":        "test-sample",
							"leaderworkerset.sigs.k8s.io/group-index": "1",
							"leaderworkerset.sigs.k8s.io/group-key":   "test-key",
						},
					},
					Template: &coreapplyv1.PodTemplateSpecApplyConfiguration{
						ObjectMetaApplyConfiguration: &metaapplyv1.ObjectMetaApplyConfiguration{
							Labels: map[string]string{
								"leaderworkerset.sigs.k8s.io/name":        "test-sample",
								"leaderworkerset.sigs.k8s.io/group-index": "1",
								"leaderworkerset.sigs.k8s.io/group-key":   "test-key",
							},
							Annotations: map[string]string{
								"leaderworkerset.sigs.k8s.io/size":        "1",
								"leaderworkerset.sigs.k8s.io/leader-name": "test-sample",
							},
						},
						Spec: &coreapplyv1.PodSpecApplyConfiguration{
							Containers: []coreapplyv1.ContainerApplyConfiguration{
								{
									Name:      ptr.To[string]("leader"),
									Image:     ptr.To[string]("nginx:1.14.2"),
									Ports:     []coreapplyv1.ContainerPortApplyConfiguration{{ContainerPort: ptr.To[int32](8080), Protocol: ptr.To[corev1.Protocol](corev1.ProtocolTCP)}},
									Resources: &coreapplyv1.ResourceRequirementsApplyConfiguration{},
								},
							},
						},
					},
					Ordinals:            &appsapplyv1.StatefulSetOrdinalsApplyConfiguration{Start: ptr.To[int32](1)},
					ServiceName:         ptr.To[string]("test-sample"),
					PodManagementPolicy: ptr.To[appsv1.PodManagementPolicyType](appsv1.ParallelPodManagement),
				},
			},
		},
		{
			name: "1 replica, size 2, exclusive placement enabled",
			pod: &corev1.Pod{
				ObjectMeta: v1.ObjectMeta{
					Name:      "test-sample",
					Namespace: "default",
					Labels: map[string]string{
						"leaderworkerset.sigs.k8s.io/worker-index": "0",
						"leaderworkerset.sigs.k8s.io/name":         "test-sample",
						"leaderworkerset.sigs.k8s.io/group-index":  "1",
						"leaderworkerset.sigs.k8s.io/group-key":    "test-key",
					},
				},
			},
			lws: testutils.BuildBasicLeaderWorkerSet("test-sample", "default").
				Replica(1).
				WorkerTemplateSpec(testutils.MakeWorkerPodSpec()).
				Annotation(map[string]string{
					"leaderworkerset.sigs.k8s.io/exclusive-topology": "cloud.google.com/gke-nodepool",
				}).Size(2).Obj(),
			wantStatefulSetConfig: &appsapplyv1.StatefulSetApplyConfiguration{
				TypeMetaApplyConfiguration: metaapplyv1.TypeMetaApplyConfiguration{
					Kind:       ptr.To[string]("StatefulSet"),
					APIVersion: ptr.To[string]("apps/v1"),
				},
				ObjectMetaApplyConfiguration: &metaapplyv1.ObjectMetaApplyConfiguration{
					Name:      ptr.To[string]("test-sample"),
					Namespace: ptr.To[string]("default"),
					Labels: map[string]string{
						"leaderworkerset.sigs.k8s.io/name":        "test-sample",
						"leaderworkerset.sigs.k8s.io/group-index": "1",
						"leaderworkerset.sigs.k8s.io/group-key":   "test-key",
					},
				},
				Spec: &appsapplyv1.StatefulSetSpecApplyConfiguration{
					Replicas: ptr.To[int32](1),
					Selector: &metaapplyv1.LabelSelectorApplyConfiguration{
						MatchLabels: map[string]string{
							"leaderworkerset.sigs.k8s.io/name":        "test-sample",
							"leaderworkerset.sigs.k8s.io/group-index": "1",
							"leaderworkerset.sigs.k8s.io/group-key":   "test-key",
						},
					},
					Template: &coreapplyv1.PodTemplateSpecApplyConfiguration{
						ObjectMetaApplyConfiguration: &metaapplyv1.ObjectMetaApplyConfiguration{
							Labels: map[string]string{
								"leaderworkerset.sigs.k8s.io/name":        "test-sample",
								"leaderworkerset.sigs.k8s.io/group-index": "1",
								"leaderworkerset.sigs.k8s.io/group-key":   "test-key",
							},
							Annotations: map[string]string{
								"leaderworkerset.sigs.k8s.io/size":               "2",
								"leaderworkerset.sigs.k8s.io/leader-name":        "test-sample",
								"leaderworkerset.sigs.k8s.io/exclusive-topology": "cloud.google.com/gke-nodepool",
							},
						},
						Spec: &coreapplyv1.PodSpecApplyConfiguration{
							Containers: []coreapplyv1.ContainerApplyConfiguration{
								{
									Name:      ptr.To[string]("leader"),
									Image:     ptr.To[string]("nginx:1.14.2"),
									Ports:     []coreapplyv1.ContainerPortApplyConfiguration{{ContainerPort: ptr.To[int32](8080), Protocol: ptr.To[corev1.Protocol](corev1.ProtocolTCP)}},
									Resources: &coreapplyv1.ResourceRequirementsApplyConfiguration{},
								},
							},
						},
					},
					Ordinals:            &appsapplyv1.StatefulSetOrdinalsApplyConfiguration{Start: ptr.To[int32](1)},
					ServiceName:         ptr.To[string]("test-sample"),
					PodManagementPolicy: ptr.To[appsv1.PodManagementPolicyType](appsv1.ParallelPodManagement),
				},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			statefulSetConfig, err := constructWorkerStatefulSetApplyConfiguration(*tc.pod, *tc.lws)
			if err != nil {
				t.Errorf("failed with error %s", err.Error())
			}
			if diff := cmp.Diff(tc.wantStatefulSetConfig, statefulSetConfig); diff != "" {
				t.Errorf("unexpected StatefulSet apply operation %s", diff)
			}

		})
	}
}

func TestContainerRestared(t *testing.T) {
	tests := []struct {
		name                    string
		pod                     corev1.Pod
		expectRestaredContainer bool
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
			expectRestaredContainer: true,
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
			expectRestaredContainer: true,
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
			expectRestaredContainer: true,
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
			containerRestared := containerRestarted(tc.pod)
			if containerRestared != tc.expectRestaredContainer {
				t.Errorf("Expected value %t, got %t", tc.expectRestaredContainer, containerRestared)
			}
		})
	}
}

func TestExclusiveConditionTypes(t *testing.T) {
	tests := []struct {
		name                          string
		condition1                    metav1.Condition
		condition2                    metav1.Condition
		expectExclusiveConditionTypes bool
	}{
		{
			name:                          "First Condition available, second Progressing",
			condition1:                    metav1.Condition{Type: "Available"},
			condition2:                    metav1.Condition{Type: "Progressing"},
			expectExclusiveConditionTypes: true,
		},
		{
			name:                          "First Condition Progressing, second Available",
			condition1:                    metav1.Condition{Type: "Progressing"},
			condition2:                    metav1.Condition{Type: "Available"},
			expectExclusiveConditionTypes: true,
		},
		{
			name:       "Same Conditions",
			condition1: metav1.Condition{Type: "Progressing"},
			condition2: metav1.Condition{Type: "Progressing"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			exclusiveConditionTypes := exclusiveConditionTypes(tc.condition1, tc.condition2)
			if exclusiveConditionTypes != tc.expectExclusiveConditionTypes {
				t.Errorf("Expected value %t, got %t", tc.expectExclusiveConditionTypes, exclusiveConditionTypes)
			}
		})
	}
}

func TestSetCondition(t *testing.T) {
	tests := []struct {
		name                 string
		condition            metav1.Condition
		lws                  *leaderworkerset.LeaderWorkerSet
		expectedShouldUpdate bool
	}{
		{
			name:      "Different condition type, same condition status",
			condition: metav1.Condition{Type: "Progressing", Status: "True"},
			lws: testutils.BuildBasicLeaderWorkerSet("test-sample", "default").
				Conditions([]metav1.Condition{{Type: "Available", Status: "True"}}).
				Obj(),
			expectedShouldUpdate: true,
		},
		{
			name:      "Same condition type, different condition status",
			condition: metav1.Condition{Type: "Progressing", Status: "True"},
			lws: testutils.BuildBasicLeaderWorkerSet("test-sample", "default").
				Conditions([]metav1.Condition{{Type: "Progressing", Status: "False"}}).
				Obj(),
			expectedShouldUpdate: true,
		},
		{
			name:      "Different conditio type, new condition status is true",
			condition: metav1.Condition{Type: "Progressing", Status: "True"},
			lws: testutils.BuildBasicLeaderWorkerSet("test-sample", "default").
				Conditions([]metav1.Condition{{Type: "Available", Status: "False"}}).
				Obj(),
			expectedShouldUpdate: true,
		},
		{
			name:                 "No initial condition",
			condition:            metav1.Condition{Type: "Progressing", Status: "True"},
			lws:                  testutils.BuildBasicLeaderWorkerSet("test-sample", "default").Obj(),
			expectedShouldUpdate: true,
		},
		{
			name:      "Different condition type, new condition status is false",
			condition: metav1.Condition{Type: "Progressing", Status: "False"},
			lws: testutils.BuildBasicLeaderWorkerSet("test-sample", "default").
				Conditions([]metav1.Condition{{Type: "Available", Status: "True"}}).
				Obj(),
		},
		{
			name:      "Same condition type, Same condition status",
			condition: metav1.Condition{Type: "Progressing", Status: "False"},
			lws: testutils.BuildBasicLeaderWorkerSet("test-sample", "default").
				Conditions([]metav1.Condition{{Type: "Progressing", Status: "False"}}).
				Obj(),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			shouldUpdate := setCondition(tc.lws, tc.condition)
			if shouldUpdate != tc.expectedShouldUpdate {
				t.Errorf("Expected value %t, got %t", tc.expectedShouldUpdate, shouldUpdate)
			}
		})
	}
}

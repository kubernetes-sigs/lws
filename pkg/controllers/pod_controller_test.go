/*
Copyright 2024.

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

	"github.com/google/go-cmp/cmp"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	appsapplyv1 "k8s.io/client-go/applyconfigurations/apps/v1"
	coreapplyv1 "k8s.io/client-go/applyconfigurations/core/v1"
	metaapplyv1 "k8s.io/client-go/applyconfigurations/meta/v1"
	"k8s.io/utils/ptr"
	leaderworkerset "sigs.k8s.io/lws/api/leaderworkerset/v1"
	testutils "sigs.k8s.io/lws/test/testutils"
)

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
						leaderworkerset.PodRoleLabelKey:            "worker",
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
						"leaderworkerset.sigs.k8s.io/name":                   "test-sample",
						"leaderworkerset.sigs.k8s.io/group-index":            "1",
						"leaderworkerset.sigs.k8s.io/group-key":              "test-key",
						"leaderworkerset.sigs.k8s.io/template-revision-hash": "",
						leaderworkerset.PodRoleLabelKey:                      "worker",
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
								"leaderworkerset.sigs.k8s.io/name":                   "test-sample",
								"leaderworkerset.sigs.k8s.io/group-index":            "1",
								"leaderworkerset.sigs.k8s.io/group-key":              "test-key",
								"leaderworkerset.sigs.k8s.io/template-revision-hash": "",
								leaderworkerset.PodRoleLabelKey:                      "worker",
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
						"leaderworkerset.sigs.k8s.io/worker-index":           "0",
						"leaderworkerset.sigs.k8s.io/name":                   "test-sample",
						"leaderworkerset.sigs.k8s.io/group-index":            "1",
						"leaderworkerset.sigs.k8s.io/group-key":              "test-key",
						"leaderworkerset.sigs.k8s.io/template-revision-hash": "",
						leaderworkerset.PodRoleLabelKey:                      "worker",
					},
				},
			},
			lws: testutils.BuildBasicLeaderWorkerSet("test-sample", "default").
				Replica(1).
				WorkerTemplateSpec(testutils.MakeWorkerPodSpec()).
				Annotation(map[string]string{
					"leaderworkerset.sigs.k8s.io/exclusive-topology": "topologyKey",
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
						"leaderworkerset.sigs.k8s.io/name":                   "test-sample",
						"leaderworkerset.sigs.k8s.io/group-index":            "1",
						"leaderworkerset.sigs.k8s.io/group-key":              "test-key",
						"leaderworkerset.sigs.k8s.io/template-revision-hash": "",
						leaderworkerset.PodRoleLabelKey:                      "worker",
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
								"leaderworkerset.sigs.k8s.io/name":                   "test-sample",
								"leaderworkerset.sigs.k8s.io/group-index":            "1",
								"leaderworkerset.sigs.k8s.io/group-key":              "test-key",
								"leaderworkerset.sigs.k8s.io/template-revision-hash": "",
								leaderworkerset.PodRoleLabelKey:                      "worker",
							},
							Annotations: map[string]string{
								"leaderworkerset.sigs.k8s.io/size":               "2",
								"leaderworkerset.sigs.k8s.io/leader-name":        "test-sample",
								"leaderworkerset.sigs.k8s.io/exclusive-topology": "topologyKey",
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
			name: "1 replica, size 2, subgroupsize 2, exclusive placement enabled",
			pod: &corev1.Pod{
				ObjectMeta: v1.ObjectMeta{
					Name:      "test-sample",
					Namespace: "default",
					Labels: map[string]string{
						"leaderworkerset.sigs.k8s.io/worker-index":           "0",
						"leaderworkerset.sigs.k8s.io/name":                   "test-sample",
						"leaderworkerset.sigs.k8s.io/group-index":            "1",
						"leaderworkerset.sigs.k8s.io/group-key":              "test-key",
						"leaderworkerset.sigs.k8s.io/template-revision-hash": "",
						leaderworkerset.PodRoleLabelKey:                      "worker",
					},
				},
			},
			lws: testutils.BuildBasicLeaderWorkerSet("test-sample", "default").
				Replica(1).
				WorkerTemplateSpec(testutils.MakeWorkerPodSpec()).
				Annotation(map[string]string{
					leaderworkerset.SubGroupExclusiveKeyAnnotationKey: "topologyKey",
				}).Size(2).SubGroupSize(2).Obj(),
			wantStatefulSetConfig: &appsapplyv1.StatefulSetApplyConfiguration{
				TypeMetaApplyConfiguration: metaapplyv1.TypeMetaApplyConfiguration{
					Kind:       ptr.To[string]("StatefulSet"),
					APIVersion: ptr.To[string]("apps/v1"),
				},
				ObjectMetaApplyConfiguration: &metaapplyv1.ObjectMetaApplyConfiguration{
					Name:      ptr.To[string]("test-sample"),
					Namespace: ptr.To[string]("default"),
					Labels: map[string]string{
						"leaderworkerset.sigs.k8s.io/name":                   "test-sample",
						"leaderworkerset.sigs.k8s.io/group-index":            "1",
						"leaderworkerset.sigs.k8s.io/template-revision-hash": "",
						"leaderworkerset.sigs.k8s.io/group-key":              "test-key",
						leaderworkerset.PodRoleLabelKey:                      "worker",
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
								"leaderworkerset.sigs.k8s.io/name":                   "test-sample",
								"leaderworkerset.sigs.k8s.io/group-index":            "1",
								"leaderworkerset.sigs.k8s.io/template-revision-hash": "",
								"leaderworkerset.sigs.k8s.io/group-key":              "test-key",
								leaderworkerset.PodRoleLabelKey:                      "worker",
							},
							Annotations: map[string]string{
								"leaderworkerset.sigs.k8s.io/size":                "2",
								"leaderworkerset.sigs.k8s.io/leader-name":         "test-sample",
								leaderworkerset.SubGroupExclusiveKeyAnnotationKey: "topologyKey",
								leaderworkerset.SubGroupSizeAnnotationKey:         "2",
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

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

package controllers

import (
	"context"
	"testing"

	"github.com/google/go-cmp/cmp"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	appsapplyv1 "k8s.io/client-go/applyconfigurations/apps/v1"
	coreapplyv1 "k8s.io/client-go/applyconfigurations/core/v1"
	metaapplyv1 "k8s.io/client-go/applyconfigurations/meta/v1"
	"k8s.io/utils/ptr"

	leaderworkerset "sigs.k8s.io/lws/api/leaderworkerset/v1"

	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	revisionutils "sigs.k8s.io/lws/pkg/utils/revision"
	"sigs.k8s.io/lws/test/wrappers"
)

func TestLeaderStatefulSetApplyConfig(t *testing.T) {
	client := fake.NewClientBuilder().Build()
	lws1 := wrappers.BuildBasicLeaderWorkerSet("test-sample", "default").
		LeaderTemplateSpec(wrappers.MakeLeaderPodSpec()).
		WorkerTemplateSpec(wrappers.MakeWorkerPodSpec()).Obj()
	cr1, err := revisionutils.NewRevision(context.TODO(), client, lws1, "")
	if err != nil {
		t.Fatal(err)
	}
	revisionKey1 := revisionutils.GetRevisionKey(cr1)

	lws2 := wrappers.BuildBasicLeaderWorkerSet("test-sample", "default").
		WorkerTemplateSpec(wrappers.MakeWorkerPodSpec()).Obj()
	cr2, err := revisionutils.NewRevision(context.TODO(), client, lws2, "")
	if err != nil {
		t.Fatal(err)
	}
	revisionKey2 := revisionutils.GetRevisionKey(cr2)

	tests := []struct {
		name            string
		revisionKey     string
		lws             *leaderworkerset.LeaderWorkerSet
		wantApplyConfig *appsapplyv1.StatefulSetApplyConfiguration
	}{
		{
			name:        "1 replica, size 1, with empty leader template, exclusive placement disabled",
			revisionKey: revisionKey2,
			lws: wrappers.BuildBasicLeaderWorkerSet("test-sample", "default").
				Replica(1).
				RolloutStrategy(leaderworkerset.RolloutStrategy{
					Type: leaderworkerset.RollingUpdateStrategyType,
					RollingUpdateConfiguration: &leaderworkerset.RollingUpdateConfiguration{
						MaxUnavailable: intstr.FromInt32(1),
					},
				}).
				WorkerTemplateSpec(wrappers.MakeWorkerPodSpec()).
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
						"leaderworkerset.sigs.k8s.io/name":                   "test-sample",
						"leaderworkerset.sigs.k8s.io/template-revision-hash": revisionKey2,
					},
					Annotations: map[string]string{"leaderworkerset.sigs.k8s.io/replicas": "1"},
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
								"leaderworkerset.sigs.k8s.io/name":                   "test-sample",
								"leaderworkerset.sigs.k8s.io/worker-index":           "0",
								"leaderworkerset.sigs.k8s.io/template-revision-hash": revisionKey2,
							},
							Annotations: map[string]string{
								"leaderworkerset.sigs.k8s.io/size": "1",
							},
						},
						Spec: &coreapplyv1.PodSpecApplyConfiguration{
							Containers: []coreapplyv1.ContainerApplyConfiguration{
								{
									Name:      ptr.To[string]("worker"),
									Image:     ptr.To[string]("nginxinc/nginx-unprivileged:1.27"),
									Ports:     []coreapplyv1.ContainerPortApplyConfiguration{{ContainerPort: ptr.To[int32](8080), Protocol: ptr.To[corev1.Protocol](corev1.ProtocolTCP)}},
									Resources: &coreapplyv1.ResourceRequirementsApplyConfiguration{},
								},
							},
						},
					},
					ServiceName:         ptr.To[string]("test-sample"),
					PodManagementPolicy: ptr.To[appsv1.PodManagementPolicyType](appsv1.ParallelPodManagement),
					UpdateStrategy: appsapplyv1.StatefulSetUpdateStrategy().
						WithType(appsv1.RollingUpdateStatefulSetStrategyType).
						WithRollingUpdate(appsapplyv1.RollingUpdateStatefulSetStrategy().WithPartition(0).WithMaxUnavailable(intstr.FromInt32(1))),
				},
			},
		},
		{
			name:        "1 replica, size 2 , with empty leader template, exclusive placement enabled",
			revisionKey: revisionKey2,
			lws: wrappers.BuildBasicLeaderWorkerSet("test-sample", "default").
				Annotation(map[string]string{
					"leaderworkerset.sigs.k8s.io/exclusive-topology": "topologyKey",
				}).Replica(1).
				RolloutStrategy(leaderworkerset.RolloutStrategy{
					Type: leaderworkerset.RollingUpdateStrategyType,
					RollingUpdateConfiguration: &leaderworkerset.RollingUpdateConfiguration{
						MaxUnavailable: intstr.FromInt32(1),
					},
				}).
				WorkerTemplateSpec(wrappers.MakeWorkerPodSpec()).
				Size(2).
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
						"leaderworkerset.sigs.k8s.io/name":                   "test-sample",
						"leaderworkerset.sigs.k8s.io/template-revision-hash": revisionKey2,
					},
					Annotations: map[string]string{"leaderworkerset.sigs.k8s.io/replicas": "1"},
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
								"leaderworkerset.sigs.k8s.io/name":                   "test-sample",
								"leaderworkerset.sigs.k8s.io/worker-index":           "0",
								"leaderworkerset.sigs.k8s.io/template-revision-hash": revisionKey2,
							},
							Annotations: map[string]string{
								"leaderworkerset.sigs.k8s.io/size":               "2",
								"leaderworkerset.sigs.k8s.io/exclusive-topology": "topologyKey",
							},
						},
						Spec: &coreapplyv1.PodSpecApplyConfiguration{
							Containers: []coreapplyv1.ContainerApplyConfiguration{
								{
									Name:      ptr.To[string]("worker"),
									Image:     ptr.To[string]("nginxinc/nginx-unprivileged:1.27"),
									Ports:     []coreapplyv1.ContainerPortApplyConfiguration{{ContainerPort: ptr.To[int32](8080), Protocol: ptr.To[corev1.Protocol](corev1.ProtocolTCP)}},
									Resources: &coreapplyv1.ResourceRequirementsApplyConfiguration{},
								},
							},
						},
					},
					ServiceName:         ptr.To[string]("test-sample"),
					PodManagementPolicy: ptr.To[appsv1.PodManagementPolicyType](appsv1.ParallelPodManagement),
					UpdateStrategy: appsapplyv1.StatefulSetUpdateStrategy().
						WithType(appsv1.RollingUpdateStatefulSetStrategyType).
						WithRollingUpdate(appsapplyv1.RollingUpdateStatefulSetStrategy().WithPartition(0).WithMaxUnavailable(intstr.FromInt32(1))),
				},
			},
		},
		{
			name:        "2 replica, size 2, with leader template, exclusive placement enabled",
			revisionKey: revisionKey1,
			lws: wrappers.BuildBasicLeaderWorkerSet("test-sample", "default").Annotation(map[string]string{
				"leaderworkerset.sigs.k8s.io/exclusive-topology": "topologyKey",
			}).Replica(2).
				RolloutStrategy(leaderworkerset.RolloutStrategy{
					Type: leaderworkerset.RollingUpdateStrategyType,
					RollingUpdateConfiguration: &leaderworkerset.RollingUpdateConfiguration{
						MaxUnavailable: intstr.FromInt32(1),
					},
				}).
				WorkerTemplateSpec(wrappers.MakeWorkerPodSpec()).
				LeaderTemplateSpec(wrappers.MakeLeaderPodSpec()).
				Size(2).
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
						"leaderworkerset.sigs.k8s.io/name":                   "test-sample",
						"leaderworkerset.sigs.k8s.io/template-revision-hash": revisionKey1,
					},
					Annotations: map[string]string{"leaderworkerset.sigs.k8s.io/replicas": "2"},
				},
				Spec: &appsapplyv1.StatefulSetSpecApplyConfiguration{
					Replicas: ptr.To[int32](2),
					Selector: &metaapplyv1.LabelSelectorApplyConfiguration{
						MatchLabels: map[string]string{
							"leaderworkerset.sigs.k8s.io/name":         "test-sample",
							"leaderworkerset.sigs.k8s.io/worker-index": "0",
						},
					},
					Template: &coreapplyv1.PodTemplateSpecApplyConfiguration{
						ObjectMetaApplyConfiguration: &metaapplyv1.ObjectMetaApplyConfiguration{
							Labels: map[string]string{
								"leaderworkerset.sigs.k8s.io/name":                   "test-sample",
								"leaderworkerset.sigs.k8s.io/worker-index":           "0",
								"leaderworkerset.sigs.k8s.io/template-revision-hash": revisionKey1,
							},
							Annotations: map[string]string{
								"leaderworkerset.sigs.k8s.io/size":               "2",
								"leaderworkerset.sigs.k8s.io/exclusive-topology": "topologyKey",
							},
						},
						Spec: &coreapplyv1.PodSpecApplyConfiguration{
							Containers: []coreapplyv1.ContainerApplyConfiguration{
								{
									Name:      ptr.To[string]("leader"),
									Image:     ptr.To[string]("nginxinc/nginx-unprivileged:1.27"),
									Resources: &coreapplyv1.ResourceRequirementsApplyConfiguration{},
								},
							},
						},
					},
					ServiceName:         ptr.To[string]("test-sample"),
					PodManagementPolicy: ptr.To[appsv1.PodManagementPolicyType](appsv1.ParallelPodManagement),
					UpdateStrategy: appsapplyv1.StatefulSetUpdateStrategy().
						WithType(appsv1.RollingUpdateStatefulSetStrategyType).
						WithRollingUpdate(appsapplyv1.RollingUpdateStatefulSetStrategy().WithPartition(0).WithMaxUnavailable(intstr.FromInt32(1))),
				},
			},
		},
		{
			name:        "2 maxUnavailable, 1 maxSurge, with empty leader template, exclusive placement disabled",
			revisionKey: revisionKey2,
			lws: wrappers.BuildBasicLeaderWorkerSet("test-sample", "default").
				Replica(1).
				RolloutStrategy(leaderworkerset.RolloutStrategy{
					Type: leaderworkerset.RollingUpdateStrategyType,
					RollingUpdateConfiguration: &leaderworkerset.RollingUpdateConfiguration{
						MaxUnavailable: intstr.FromInt32(2),
						MaxSurge:       intstr.FromInt32(1),
					},
				}).
				WorkerTemplateSpec(wrappers.MakeWorkerPodSpec()).
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
						"leaderworkerset.sigs.k8s.io/name":                   "test-sample",
						"leaderworkerset.sigs.k8s.io/template-revision-hash": revisionKey2,
					},
					Annotations: map[string]string{"leaderworkerset.sigs.k8s.io/replicas": "1"},
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
								"leaderworkerset.sigs.k8s.io/name":                   "test-sample",
								"leaderworkerset.sigs.k8s.io/worker-index":           "0",
								"leaderworkerset.sigs.k8s.io/template-revision-hash": revisionKey2,
							},
							Annotations: map[string]string{
								"leaderworkerset.sigs.k8s.io/size": "1",
							},
						},
						Spec: &coreapplyv1.PodSpecApplyConfiguration{
							Containers: []coreapplyv1.ContainerApplyConfiguration{
								{
									Name:      ptr.To[string]("worker"),
									Image:     ptr.To[string]("nginxinc/nginx-unprivileged:1.27"),
									Ports:     []coreapplyv1.ContainerPortApplyConfiguration{{ContainerPort: ptr.To[int32](8080), Protocol: ptr.To[corev1.Protocol](corev1.ProtocolTCP)}},
									Resources: &coreapplyv1.ResourceRequirementsApplyConfiguration{},
								},
							},
						},
					},
					ServiceName:         ptr.To[string]("test-sample"),
					PodManagementPolicy: ptr.To[appsv1.PodManagementPolicyType](appsv1.ParallelPodManagement),
					UpdateStrategy: appsapplyv1.StatefulSetUpdateStrategy().
						WithType(appsv1.RollingUpdateStatefulSetStrategyType).
						WithRollingUpdate(appsapplyv1.RollingUpdateStatefulSetStrategy().WithPartition(0).WithMaxUnavailable(intstr.FromInt32(2))),
				},
			},
		},
		{
			name:        "1 replica, size 2, with leader template, exclusive placement enabled, subgroupsize enabled",
			revisionKey: revisionKey1,
			lws: wrappers.BuildBasicLeaderWorkerSet("test-sample", "default").Annotation(map[string]string{
				leaderworkerset.SubGroupExclusiveKeyAnnotationKey: "topologyKey",
			}).SubGroupSize(2).SubGroupType(leaderworkerset.SubGroupPolicyTypeLeaderWorker).Replica(1).
				RolloutStrategy(leaderworkerset.RolloutStrategy{
					Type: leaderworkerset.RollingUpdateStrategyType,
					RollingUpdateConfiguration: &leaderworkerset.RollingUpdateConfiguration{
						MaxUnavailable: intstr.FromInt32(1),
					},
				}).
				WorkerTemplateSpec(wrappers.MakeWorkerPodSpec()).
				LeaderTemplateSpec(wrappers.MakeLeaderPodSpec()).
				Size(2).
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
						"leaderworkerset.sigs.k8s.io/name":                   "test-sample",
						"leaderworkerset.sigs.k8s.io/template-revision-hash": revisionKey1,
					},
					Annotations: map[string]string{"leaderworkerset.sigs.k8s.io/replicas": "1"},
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
								"leaderworkerset.sigs.k8s.io/name":                   "test-sample",
								"leaderworkerset.sigs.k8s.io/worker-index":           "0",
								"leaderworkerset.sigs.k8s.io/template-revision-hash": revisionKey1,
							},
							Annotations: map[string]string{
								"leaderworkerset.sigs.k8s.io/size":                "2",
								leaderworkerset.SubGroupSizeAnnotationKey:         "2",
								leaderworkerset.SubGroupExclusiveKeyAnnotationKey: "topologyKey",
								leaderworkerset.SubGroupPolicyTypeAnnotationKey:   string(leaderworkerset.SubGroupPolicyTypeLeaderWorker),
							},
						},
						Spec: &coreapplyv1.PodSpecApplyConfiguration{
							Containers: []coreapplyv1.ContainerApplyConfiguration{
								{
									Name:      ptr.To[string]("leader"),
									Image:     ptr.To[string]("nginxinc/nginx-unprivileged:1.27"),
									Resources: &coreapplyv1.ResourceRequirementsApplyConfiguration{},
								},
							},
						},
					},
					ServiceName:         ptr.To[string]("test-sample"),
					PodManagementPolicy: ptr.To[appsv1.PodManagementPolicyType](appsv1.ParallelPodManagement),
					UpdateStrategy: appsapplyv1.StatefulSetUpdateStrategy().
						WithType(appsv1.RollingUpdateStatefulSetStrategyType).
						WithRollingUpdate(appsapplyv1.RollingUpdateStatefulSetStrategy().WithPartition(0).WithMaxUnavailable(intstr.FromInt32(1))),
				},
			},
		},
		{
			name:        "1 replica, size 1, with volumeClaimTemplates and PersistentVolumeClaimRetentionPolicy configured",
			revisionKey: revisionKey2,
			lws: wrappers.BuildBasicLeaderWorkerSet("test-sample", "default").
				Replica(1).
				RolloutStrategy(leaderworkerset.RolloutStrategy{
					Type: leaderworkerset.RollingUpdateStrategyType,
					RollingUpdateConfiguration: &leaderworkerset.RollingUpdateConfiguration{
						MaxUnavailable: intstr.FromInt32(1),
					},
				}).
				WorkerTemplateSpec(wrappers.MakeWorkerPodSpec()).
				Size(1).
				PersistentVolumeClaimRetentionPolicy(&appsv1.StatefulSetPersistentVolumeClaimRetentionPolicy{
					WhenDeleted: appsv1.RetainPersistentVolumeClaimRetentionPolicyType,
					WhenScaled:  appsv1.DeletePersistentVolumeClaimRetentionPolicyType,
				}).
				VolumeClaimTemplates([]corev1.PersistentVolumeClaim{
					{
						ObjectMeta: metav1.ObjectMeta{Name: "pvc1"},
						Spec: corev1.PersistentVolumeClaimSpec{
							AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
							StorageClassName: ptr.To[string]("standard"),
							VolumeMode:       ptr.To[corev1.PersistentVolumeMode](corev1.PersistentVolumeFilesystem),
							Resources: corev1.VolumeResourceRequirements{
								Requests: corev1.ResourceList{
									corev1.ResourceStorage: resource.MustParse("1Gi"),
								},
								Limits: corev1.ResourceList{
									corev1.ResourceStorage: resource.MustParse("2Gi"),
								},
							},
						},
					},
				}).RestartPolicy(leaderworkerset.RecreateGroupOnPodRestart).Obj(),
			wantApplyConfig: &appsapplyv1.StatefulSetApplyConfiguration{
				TypeMetaApplyConfiguration: metaapplyv1.TypeMetaApplyConfiguration{
					Kind:       ptr.To[string]("StatefulSet"),
					APIVersion: ptr.To[string]("apps/v1"),
				},
				ObjectMetaApplyConfiguration: &metaapplyv1.ObjectMetaApplyConfiguration{
					Name:      ptr.To[string]("test-sample"),
					Namespace: ptr.To[string]("default"),
					Labels: map[string]string{
						"leaderworkerset.sigs.k8s.io/name":                   "test-sample",
						"leaderworkerset.sigs.k8s.io/template-revision-hash": revisionKey2,
					},
					Annotations: map[string]string{"leaderworkerset.sigs.k8s.io/replicas": "1"},
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
								"leaderworkerset.sigs.k8s.io/name":                   "test-sample",
								"leaderworkerset.sigs.k8s.io/worker-index":           "0",
								"leaderworkerset.sigs.k8s.io/template-revision-hash": revisionKey2,
							},
							Annotations: map[string]string{
								"leaderworkerset.sigs.k8s.io/size": "1",
							},
						},
						Spec: &coreapplyv1.PodSpecApplyConfiguration{
							Containers: []coreapplyv1.ContainerApplyConfiguration{
								{
									Name:      ptr.To[string]("worker"),
									Image:     ptr.To[string]("nginxinc/nginx-unprivileged:1.27"),
									Ports:     []coreapplyv1.ContainerPortApplyConfiguration{{ContainerPort: ptr.To[int32](8080), Protocol: ptr.To[corev1.Protocol](corev1.ProtocolTCP)}},
									Resources: &coreapplyv1.ResourceRequirementsApplyConfiguration{},
								},
							},
						},
					},
					PersistentVolumeClaimRetentionPolicy: &appsapplyv1.StatefulSetPersistentVolumeClaimRetentionPolicyApplyConfiguration{
						WhenDeleted: ptr.To(appsv1.RetainPersistentVolumeClaimRetentionPolicyType),
						WhenScaled:  ptr.To(appsv1.DeletePersistentVolumeClaimRetentionPolicyType),
					},
					VolumeClaimTemplates: []coreapplyv1.PersistentVolumeClaimApplyConfiguration{
						{
							TypeMetaApplyConfiguration: metaapplyv1.TypeMetaApplyConfiguration{
								Kind:       ptr.To[string]("PersistentVolumeClaim"),
								APIVersion: ptr.To[string]("v1"),
							},
							ObjectMetaApplyConfiguration: &metaapplyv1.ObjectMetaApplyConfiguration{
								Name:      ptr.To[string]("pvc1"),
								Namespace: ptr.To[string]("default"),
							},
							Spec: &coreapplyv1.PersistentVolumeClaimSpecApplyConfiguration{
								AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
								StorageClassName: ptr.To[string]("standard"),
								VolumeMode:       ptr.To[corev1.PersistentVolumeMode](corev1.PersistentVolumeFilesystem),
								Resources: &coreapplyv1.VolumeResourceRequirementsApplyConfiguration{
									Requests: &corev1.ResourceList{
										corev1.ResourceStorage: resource.MustParse("1Gi"),
									},
									Limits: &corev1.ResourceList{
										corev1.ResourceStorage: resource.MustParse("2Gi"),
									},
								},
							},
						},
					},
					ServiceName:         ptr.To[string]("test-sample"),
					PodManagementPolicy: ptr.To[appsv1.PodManagementPolicyType](appsv1.ParallelPodManagement),
					UpdateStrategy: appsapplyv1.StatefulSetUpdateStrategy().
						WithType(appsv1.RollingUpdateStatefulSetStrategyType).
						WithRollingUpdate(appsapplyv1.RollingUpdateStatefulSetStrategy().WithPartition(0).WithMaxUnavailable(intstr.FromInt32(1))),
				},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			stsApplyConfig, err := constructLeaderStatefulSetApplyConfiguration(tc.lws, 0, *tc.lws.Spec.Replicas, tc.revisionKey)
			if err != nil {
				t.Errorf("failed with error: %s", err.Error())
			}
			if diff := cmp.Diff(tc.wantApplyConfig, stsApplyConfig); diff != "" {
				t.Errorf("unexpected StatefulSet apply configuration: %s", diff)
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
		{
			name:                          "First Condition UpdateInProgress, second Available",
			condition1:                    metav1.Condition{Type: string(leaderworkerset.LeaderWorkerSetUpdateInProgress)},
			condition2:                    metav1.Condition{Type: "Available"},
			expectExclusiveConditionTypes: true,
		},
		{
			name:                          "First Condition Available, second UpdateInProgress",
			condition1:                    metav1.Condition{Type: "Available"},
			condition2:                    metav1.Condition{Type: string(leaderworkerset.LeaderWorkerSetUpdateInProgress)},
			expectExclusiveConditionTypes: true,
		},
		{
			name:       "First Condition Progressing, second UpdateInProgress",
			condition1: metav1.Condition{Type: "Progressing"},
			condition2: metav1.Condition{Type: string(leaderworkerset.LeaderWorkerSetUpdateInProgress)},
		},
		{
			name:       "First Condition UpdateInProgress, second Progressing",
			condition1: metav1.Condition{Type: string(leaderworkerset.LeaderWorkerSetUpdateInProgress)},
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
			lws: wrappers.BuildBasicLeaderWorkerSet("test-sample", "default").
				Conditions([]metav1.Condition{{Type: "Available", Status: "True"}}).
				Obj(),
			expectedShouldUpdate: true,
		},
		{
			name:      "Same condition type, different condition status",
			condition: metav1.Condition{Type: "Progressing", Status: "True"},
			lws: wrappers.BuildBasicLeaderWorkerSet("test-sample", "default").
				Conditions([]metav1.Condition{{Type: "Progressing", Status: "False"}}).
				Obj(),
			expectedShouldUpdate: true,
		},
		{
			name:      "Different conditio type, new condition status is true",
			condition: metav1.Condition{Type: "Progressing", Status: "True"},
			lws: wrappers.BuildBasicLeaderWorkerSet("test-sample", "default").
				Conditions([]metav1.Condition{{Type: "Available", Status: "False"}}).
				Obj(),
			expectedShouldUpdate: true,
		},
		{
			name:                 "No initial condition",
			condition:            metav1.Condition{Type: "Progressing", Status: "True"},
			lws:                  wrappers.BuildBasicLeaderWorkerSet("test-sample", "default").Obj(),
			expectedShouldUpdate: true,
		},
		{
			name:      "Different condition type, new condition status is false",
			condition: metav1.Condition{Type: "Progressing", Status: "False"},
			lws: wrappers.BuildBasicLeaderWorkerSet("test-sample", "default").
				Conditions([]metav1.Condition{{Type: "Available", Status: "True"}}).
				Obj(),
		},
		{
			name:      "Same condition type, Same condition status",
			condition: metav1.Condition{Type: "Progressing", Status: "False"},
			lws: wrappers.BuildBasicLeaderWorkerSet("test-sample", "default").
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

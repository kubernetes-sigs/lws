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
	"context"
	"testing"

	"github.com/google/go-cmp/cmp"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	appsapplyv1 "k8s.io/client-go/applyconfigurations/apps/v1"
	coreapplyv1 "k8s.io/client-go/applyconfigurations/core/v1"
	metaapplyv1 "k8s.io/client-go/applyconfigurations/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	leaderworkerset "sigs.k8s.io/lws/api/leaderworkerset/v1"
	revisionutils "sigs.k8s.io/lws/pkg/utils/revision"
	"sigs.k8s.io/lws/test/wrappers"
)

func TestConstructWorkerStatefulSetApplyConfiguration(t *testing.T) {
	client := fake.NewClientBuilder().Build()

	lws := wrappers.BuildBasicLeaderWorkerSet("test-sample", "default").Replica(1).WorkerTemplateSpec(wrappers.MakeWorkerPodSpec()).Size(1).Obj()
	updateRevision, err := revisionutils.NewRevision(context.TODO(), client, lws, "")
	if err != nil {
		t.Fatal(err)
	}
	updateRevisionKey := revisionutils.GetRevisionKey(updateRevision)

	tests := []struct {
		name                  string
		pod                   *corev1.Pod
		lws                   *leaderworkerset.LeaderWorkerSet
		wantStatefulSetConfig *appsapplyv1.StatefulSetApplyConfiguration
		revision              *appsv1.ControllerRevision
	}{
		{
			name:     "1 replica, size 1, exclusive placement disabled",
			revision: updateRevision,
			pod: &corev1.Pod{
				ObjectMeta: v1.ObjectMeta{
					Name:      "test-sample",
					Namespace: "default",
					Labels: map[string]string{
						leaderworkerset.WorkerIndexLabelKey:     "0",
						leaderworkerset.SetNameLabelKey:         "test-sample",
						leaderworkerset.GroupIndexLabelKey:      "1",
						leaderworkerset.GroupUniqueHashLabelKey: "test-key",
						leaderworkerset.RevisionKey:             updateRevisionKey,
					},
				},
			},
			lws: wrappers.BuildBasicLeaderWorkerSet("test-sample", "default").
				Replica(1).
				WorkerTemplateSpec(wrappers.MakeWorkerPodSpec()).
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
						leaderworkerset.SetNameLabelKey:         "test-sample",
						leaderworkerset.GroupIndexLabelKey:      "1",
						leaderworkerset.GroupUniqueHashLabelKey: "test-key",
						leaderworkerset.RevisionKey:             updateRevisionKey,
					},
				},
				Spec: &appsapplyv1.StatefulSetSpecApplyConfiguration{
					Replicas: ptr.To[int32](0),
					Selector: &metaapplyv1.LabelSelectorApplyConfiguration{
						MatchLabels: map[string]string{
							leaderworkerset.SetNameLabelKey:         "test-sample",
							leaderworkerset.GroupIndexLabelKey:      "1",
							leaderworkerset.GroupUniqueHashLabelKey: "test-key",
						},
					},
					Template: &coreapplyv1.PodTemplateSpecApplyConfiguration{
						ObjectMetaApplyConfiguration: &metaapplyv1.ObjectMetaApplyConfiguration{
							Labels: map[string]string{
								leaderworkerset.SetNameLabelKey:         "test-sample",
								leaderworkerset.GroupIndexLabelKey:      "1",
								leaderworkerset.GroupUniqueHashLabelKey: "test-key",
								leaderworkerset.RevisionKey:             updateRevisionKey,
							},
							Annotations: map[string]string{
								"leaderworkerset.sigs.k8s.io/size":        "1",
								"leaderworkerset.sigs.k8s.io/leader-name": "test-sample",
							},
						},
						Spec: &coreapplyv1.PodSpecApplyConfiguration{
							Containers: []coreapplyv1.ContainerApplyConfiguration{
								{
									Name:      ptr.To[string]("worker"),
									Image:     ptr.To[string]("docker.io/nginxinc/nginx-unprivileged:1.27"),
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
			name:     "1 replica, size 2, exclusive placement enabled",
			revision: updateRevision,
			pod: &corev1.Pod{
				ObjectMeta: v1.ObjectMeta{
					Name:      "test-sample",
					Namespace: "default",
					Labels: map[string]string{
						leaderworkerset.WorkerIndexLabelKey:     "0",
						leaderworkerset.SetNameLabelKey:         "test-sample",
						leaderworkerset.GroupIndexLabelKey:      "1",
						leaderworkerset.GroupUniqueHashLabelKey: "test-key",
						leaderworkerset.RevisionKey:             updateRevisionKey,
					},
				},
			},
			lws: wrappers.BuildBasicLeaderWorkerSet("test-sample", "default").
				Replica(1).
				WorkerTemplateSpec(wrappers.MakeWorkerPodSpec()).
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
						leaderworkerset.SetNameLabelKey:         "test-sample",
						leaderworkerset.GroupIndexLabelKey:      "1",
						leaderworkerset.GroupUniqueHashLabelKey: "test-key",
						leaderworkerset.RevisionKey:             updateRevisionKey,
					},
				},
				Spec: &appsapplyv1.StatefulSetSpecApplyConfiguration{
					Replicas: ptr.To[int32](1),
					Selector: &metaapplyv1.LabelSelectorApplyConfiguration{
						MatchLabels: map[string]string{
							leaderworkerset.SetNameLabelKey:         "test-sample",
							leaderworkerset.GroupIndexLabelKey:      "1",
							leaderworkerset.GroupUniqueHashLabelKey: "test-key",
						},
					},
					Template: &coreapplyv1.PodTemplateSpecApplyConfiguration{
						ObjectMetaApplyConfiguration: &metaapplyv1.ObjectMetaApplyConfiguration{
							Labels: map[string]string{
								leaderworkerset.SetNameLabelKey:         "test-sample",
								leaderworkerset.GroupIndexLabelKey:      "1",
								leaderworkerset.GroupUniqueHashLabelKey: "test-key",
								leaderworkerset.RevisionKey:             updateRevisionKey,
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
									Name:      ptr.To[string]("worker"),
									Image:     ptr.To[string]("docker.io/nginxinc/nginx-unprivileged:1.27"),
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
			name:     "1 replica, size 2, subgroupsize 2, exclusive placement enabled",
			revision: updateRevision,
			pod: &corev1.Pod{
				ObjectMeta: v1.ObjectMeta{
					Name:      "test-sample",
					Namespace: "default",
					Labels: map[string]string{
						leaderworkerset.WorkerIndexLabelKey:     "0",
						leaderworkerset.SetNameLabelKey:         "test-sample",
						leaderworkerset.GroupIndexLabelKey:      "1",
						leaderworkerset.GroupUniqueHashLabelKey: "test-key",
						leaderworkerset.RevisionKey:             updateRevisionKey,
					},
				},
			},
			lws: wrappers.BuildBasicLeaderWorkerSet("test-sample", "default").
				Replica(1).
				WorkerTemplateSpec(wrappers.MakeWorkerPodSpec()).
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
						leaderworkerset.SetNameLabelKey:         "test-sample",
						leaderworkerset.GroupIndexLabelKey:      "1",
						leaderworkerset.RevisionKey:             updateRevisionKey,
						leaderworkerset.GroupUniqueHashLabelKey: "test-key",
					},
				},
				Spec: &appsapplyv1.StatefulSetSpecApplyConfiguration{
					Replicas: ptr.To[int32](1),
					Selector: &metaapplyv1.LabelSelectorApplyConfiguration{
						MatchLabels: map[string]string{
							leaderworkerset.SetNameLabelKey:         "test-sample",
							leaderworkerset.GroupIndexLabelKey:      "1",
							leaderworkerset.GroupUniqueHashLabelKey: "test-key",
						},
					},
					Template: &coreapplyv1.PodTemplateSpecApplyConfiguration{
						ObjectMetaApplyConfiguration: &metaapplyv1.ObjectMetaApplyConfiguration{
							Labels: map[string]string{
								leaderworkerset.SetNameLabelKey:         "test-sample",
								leaderworkerset.GroupIndexLabelKey:      "1",
								leaderworkerset.RevisionKey:             updateRevisionKey,
								leaderworkerset.GroupUniqueHashLabelKey: "test-key",
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
									Name:      ptr.To[string]("worker"),
									Image:     ptr.To[string]("docker.io/nginxinc/nginx-unprivileged:1.27"),
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
			name:     "1 replica, size 1, with volumeClaimTemplates and PersistentVolumeClaimRetentionPolicy configured",
			revision: updateRevision,
			pod: &corev1.Pod{
				ObjectMeta: v1.ObjectMeta{
					Name:      "test-sample",
					Namespace: "default",
					Labels: map[string]string{
						leaderworkerset.WorkerIndexLabelKey:     "0",
						leaderworkerset.SetNameLabelKey:         "test-sample",
						leaderworkerset.GroupIndexLabelKey:      "1",
						leaderworkerset.GroupUniqueHashLabelKey: "test-key",
						leaderworkerset.RevisionKey:             updateRevisionKey,
					},
				},
			},
			lws: wrappers.BuildBasicLeaderWorkerSet("test-sample", "default").
				Replica(1).
				WorkerTemplateSpec(wrappers.MakeWorkerPodSpec()).
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
				}).Size(1).Obj(),
			wantStatefulSetConfig: &appsapplyv1.StatefulSetApplyConfiguration{
				TypeMetaApplyConfiguration: metaapplyv1.TypeMetaApplyConfiguration{
					Kind:       ptr.To[string]("StatefulSet"),
					APIVersion: ptr.To[string]("apps/v1"),
				},
				ObjectMetaApplyConfiguration: &metaapplyv1.ObjectMetaApplyConfiguration{
					Name:      ptr.To[string]("test-sample"),
					Namespace: ptr.To[string]("default"),
					Labels: map[string]string{
						leaderworkerset.SetNameLabelKey:         "test-sample",
						leaderworkerset.GroupIndexLabelKey:      "1",
						leaderworkerset.GroupUniqueHashLabelKey: "test-key",
						leaderworkerset.RevisionKey:             updateRevisionKey,
					},
				},
				Spec: &appsapplyv1.StatefulSetSpecApplyConfiguration{
					Replicas: ptr.To[int32](0),
					Selector: &metaapplyv1.LabelSelectorApplyConfiguration{
						MatchLabels: map[string]string{
							leaderworkerset.SetNameLabelKey:         "test-sample",
							leaderworkerset.GroupIndexLabelKey:      "1",
							leaderworkerset.GroupUniqueHashLabelKey: "test-key",
						},
					},
					Template: &coreapplyv1.PodTemplateSpecApplyConfiguration{
						ObjectMetaApplyConfiguration: &metaapplyv1.ObjectMetaApplyConfiguration{
							Labels: map[string]string{
								leaderworkerset.SetNameLabelKey:         "test-sample",
								leaderworkerset.GroupIndexLabelKey:      "1",
								leaderworkerset.GroupUniqueHashLabelKey: "test-key",
								leaderworkerset.RevisionKey:             updateRevisionKey,
							},
							Annotations: map[string]string{
								"leaderworkerset.sigs.k8s.io/size":        "1",
								"leaderworkerset.sigs.k8s.io/leader-name": "test-sample",
							},
						},
						Spec: &coreapplyv1.PodSpecApplyConfiguration{
							Containers: []coreapplyv1.ContainerApplyConfiguration{
								{
									Name:      ptr.To[string]("worker"),
									Image:     ptr.To[string]("docker.io/nginxinc/nginx-unprivileged:1.27"),
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
					Ordinals:            &appsapplyv1.StatefulSetOrdinalsApplyConfiguration{Start: ptr.To[int32](1)},
					ServiceName:         ptr.To[string]("test-sample"),
					PodManagementPolicy: ptr.To[appsv1.PodManagementPolicyType](appsv1.ParallelPodManagement),
				},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			statefulSetConfig, err := constructWorkerStatefulSetApplyConfiguration(*tc.pod, *tc.lws, tc.revision)
			if err != nil {
				t.Errorf("failed with error %s", err.Error())
			}
			if diff := cmp.Diff(tc.wantStatefulSetConfig, statefulSetConfig); diff != "" {
				t.Errorf("unexpected StatefulSet apply operation %s", diff)
			}
		})
	}
}

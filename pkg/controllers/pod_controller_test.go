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
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	appsapplyv1 "k8s.io/client-go/applyconfigurations/apps/v1"
	coreapplyv1 "k8s.io/client-go/applyconfigurations/core/v1"
	metaapplyv1 "k8s.io/client-go/applyconfigurations/meta/v1"
	"k8s.io/client-go/tools/events"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	leaderworkerset "sigs.k8s.io/lws/api/leaderworkerset/v1"
	"sigs.k8s.io/lws/pkg/schedulerprovider"
	podutils "sigs.k8s.io/lws/pkg/utils/pod"
	revisionutils "sigs.k8s.io/lws/pkg/utils/revision"
	"sigs.k8s.io/lws/test/wrappers"
)

type stubSchedulerProvider struct {
	createErr error
	calls     int
	// onCreate observes the pod as the provider sees it, which lets a test
	// assert the ordering against the group replacement scheduling gate.
	onCreate func(*corev1.Pod)
	lws      *leaderworkerset.LeaderWorkerSet
}

func (*stubSchedulerProvider) ReconcileScheduling(context.Context, *leaderworkerset.LeaderWorkerSet, int32, string) error {
	return nil
}

func (s *stubSchedulerProvider) CreatePodGroupIfNotExists(_ context.Context, lws *leaderworkerset.LeaderWorkerSet, pod *corev1.Pod) error {
	s.calls++
	s.lws = lws
	if s.onCreate != nil {
		s.onCreate(pod)
	}
	return s.createErr
}

func (*stubSchedulerProvider) InjectPodGroupMetadata(*corev1.Pod) error {
	return nil
}

func TestPodReconcilerReturnsPodGroupErrors(t *testing.T) {
	testScheme := runtime.NewScheme()
	if err := corev1.AddToScheme(testScheme); err != nil {
		t.Fatal(err)
	}
	if err := leaderworkerset.AddToScheme(testScheme); err != nil {
		t.Fatal(err)
	}

	lws := &leaderworkerset.LeaderWorkerSet{
		ObjectMeta: metav1.ObjectMeta{Name: "test-lws", Namespace: "default"},
		Spec: leaderworkerset.LeaderWorkerSetSpec{
			LeaderWorkerTemplate: leaderworkerset.LeaderWorkerTemplate{Size: ptr.To[int32](1)},
		},
	}
	leaderPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-lws-0",
			Namespace: lws.Namespace,
			Labels: map[string]string{
				leaderworkerset.SetNameLabelKey:     lws.Name,
				leaderworkerset.WorkerIndexLabelKey: "0",
			},
		},
	}
	for _, tc := range []struct {
		name      string
		err       error
		wantEvent bool
	}{
		{name: "waiting for garbage collection", err: errors.New("waiting for podgroup deletion")},
		{name: "unexpected owner", err: fmt.Errorf("%w: conflicting owner", schedulerprovider.ErrUnexpectedPodGroupOwner), wantEvent: true},
		{name: "API failure", err: errors.New("API unavailable")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			provider := &stubSchedulerProvider{createErr: tc.err}
			recorder := events.NewFakeRecorder(1)
			reconciler := &PodReconciler{
				Client:            fake.NewClientBuilder().WithScheme(testScheme).WithObjects(lws, leaderPod).Build(),
				Scheme:            testScheme,
				SchedulerProvider: provider,
				Record:            recorder,
			}
			result, err := reconciler.reconcilePod(context.Background(), podReconcileRequestForPod(leaderPod, false))
			if !errors.Is(err, tc.err) {
				t.Fatalf("reconcilePod() error = %v, want %v", err, tc.err)
			}
			if !result.IsZero() {
				t.Fatalf("expected error-based retry without explicit requeue, got %+v", result)
			}
			if provider.calls != 1 {
				t.Fatalf("provider calls = %d, want 1", provider.calls)
			}
			select {
			case event := <-recorder.Events:
				if !tc.wantEvent || !strings.Contains(event, "Warning UnexpectedPodGroupOwner") || !strings.Contains(event, tc.err.Error()) {
					t.Fatalf("unexpected event: %s", event)
				}
			default:
				if tc.wantEvent {
					t.Fatal("expected unexpected-owner warning event")
				}
			}
		})
	}

	t.Run("missing scheduler provider rejects opted-in pod reconciliation", func(t *testing.T) {
		scheduledLWS := lws.DeepCopy()
		scheduledLWS.Spec.Scheduling = &leaderworkerset.LeaderWorkerSetScheduling{}
		reconciler := &PodReconciler{
			Client: fake.NewClientBuilder().WithScheme(testScheme).WithObjects(scheduledLWS, leaderPod).Build(),
			Scheme: testScheme,
			Record: events.NewFakeRecorder(1),
		}
		result, err := reconciler.reconcilePod(context.Background(), podReconcileRequestForPod(leaderPod, false))
		if err == nil || !strings.Contains(err.Error(), "requires a configured scheduler provider") {
			t.Fatalf("reconcilePod() error = %v, want missing scheduler provider error", err)
		}
		if !result.IsZero() {
			t.Fatalf("expected error-based retry without explicit requeue, got %+v", result)
		}
	})
}

// TestPodReconcilerCreatesPodGroupBeforeUngatingHashLeader pins the ordering
// that workload-aware scheduling depends on in both Ordinal and Hash modes:
// the PodGroup is created while the leader still carries the group replacement
// gate and can therefore not be scheduled yet.
func TestPodReconcilerCreatesPodGroupBeforeUngatingHashLeader(t *testing.T) {
	testScheme := runtime.NewScheme()
	if err := corev1.AddToScheme(testScheme); err != nil {
		t.Fatal(err)
	}
	if err := leaderworkerset.AddToScheme(testScheme); err != nil {
		t.Fatal(err)
	}

	newLWS := func(identity leaderworkerset.GroupIdentityType, policy leaderworkerset.GroupReplacementPolicyType) *leaderworkerset.LeaderWorkerSet {
		return &leaderworkerset.LeaderWorkerSet{
			ObjectMeta: metav1.ObjectMeta{Name: "hash-lws", Namespace: "default"},
			Spec: leaderworkerset.LeaderWorkerSetSpec{
				GroupIdentity:          identity,
				GroupReplacementPolicy: policy,
				Scheduling:             &leaderworkerset.LeaderWorkerSetScheduling{},
				LeaderWorkerTemplate:   leaderworkerset.LeaderWorkerTemplate{Size: ptr.To[int32](1)},
			},
		}
	}
	newLeader := func(name, groupIndex string, gated, terminating bool) *corev1.Pod {
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: "default",
				UID:       types.UID(name),
				Labels: map[string]string{
					leaderworkerset.SetNameLabelKey:     "hash-lws",
					leaderworkerset.WorkerIndexLabelKey: "0",
					leaderworkerset.GroupIndexLabelKey:  groupIndex,
				},
			},
		}
		if gated {
			pod.Spec.SchedulingGates = []corev1.PodSchedulingGate{{Name: leaderworkerset.GroupReplacementSchedulingGate}}
		}
		if terminating {
			now := metav1.Now()
			pod.DeletionTimestamp = &now
			pod.Finalizers = []string{"test/hold"}
		}
		return pod
	}

	for _, tc := range []struct {
		name        string
		identity    leaderworkerset.GroupIdentityType
		policy      leaderworkerset.GroupReplacementPolicyType
		groupIndex  string
		others      []client.Object
		wantUngated bool
	}{
		{
			name:        "hash gate is lifted after the PodGroup exists",
			identity:    leaderworkerset.GroupIdentityHash,
			policy:      leaderworkerset.GroupReplacementImmediate,
			groupIndex:  "group-key-new",
			wantUngated: true,
		},
		{
			name:       "hash PodGroup is created even while the replacement waits",
			identity:   leaderworkerset.GroupIdentityHash,
			policy:     leaderworkerset.GroupReplacementPostTermination,
			groupIndex: "group-key-new",
			others:     []client.Object{newLeader("old", "group-key-old", false, true)},
		},
		{
			name:        "ordinal gate is lifted after the PodGroup exists",
			identity:    leaderworkerset.GroupIdentityOrdinal,
			policy:      leaderworkerset.GroupReplacementPostTermination,
			groupIndex:  "0",
			wantUngated: true,
		},
		{
			name:       "ordinal PodGroup is created while waiting for same-ordinal pod teardown",
			identity:   leaderworkerset.GroupIdentityOrdinal,
			policy:     leaderworkerset.GroupReplacementPostTermination,
			groupIndex: "0",
			others:     []client.Object{newLeader("old-worker", "0", false, true)},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lws := newLWS(tc.identity, tc.policy)
			leader := newLeader("new", tc.groupIndex, true, false)
			objs := append([]client.Object{lws, leader}, tc.others...)

			var gatedAtCreate bool
			provider := &stubSchedulerProvider{onCreate: func(pod *corev1.Pod) {
				gatedAtCreate = podutils.HasSchedulingGate(pod, leaderworkerset.GroupReplacementSchedulingGate)
			}}
			fakeClient := fake.NewClientBuilder().WithScheme(testScheme).WithObjects(objs...).Build()
			reconciler := &PodReconciler{
				Client:            fakeClient,
				Scheme:            testScheme,
				SchedulerProvider: provider,
				Record:            events.NewFakeRecorder(10),
			}

			if _, err := reconciler.reconcilePod(context.Background(), podReconcileRequestForPod(leader, false)); err != nil {
				t.Fatalf("reconcilePod() error = %v", err)
			}
			if provider.calls != 1 {
				t.Fatalf("provider calls = %d, want 1", provider.calls)
			}
			if !gatedAtCreate {
				t.Error("PodGroup was created after the leader had already been ungated")
			}
			var stored corev1.Pod
			if err := fakeClient.Get(context.Background(), client.ObjectKeyFromObject(leader), &stored); err != nil {
				t.Fatalf("getting stored pod: %v", err)
			}
			if gated := podutils.HasSchedulingGate(&stored, leaderworkerset.GroupReplacementSchedulingGate); gated == tc.wantUngated {
				t.Errorf("stored pod gated = %t, want %t", gated, !tc.wantUngated)
			}
		})
	}
}

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
	}{
		{
			name: "1 replica, size 1, exclusive placement disabled",
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
				Spec: corev1.PodSpec{
					Hostname:  "test-sample",
					Subdomain: "test-sample",
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
						leaderworkerset.RoleLabelKey:            leaderworkerset.RoleWorker,
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
			name: "1 replica, size 2, exclusive placement enabled",
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
				Spec: corev1.PodSpec{
					Hostname:  "test-sample",
					Subdomain: "test-sample",
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
						leaderworkerset.RoleLabelKey:            leaderworkerset.RoleWorker,
					},
					Annotations: map[string]string{
						"leaderworkerset.sigs.k8s.io/exclusive-topology": "topologyKey",
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
			name: "1 replica, size 2, subgroupsize 2, exclusive placement enabled",
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
				Spec: corev1.PodSpec{
					Hostname:  "test-sample",
					Subdomain: "test-sample",
				},
			},
			lws: wrappers.BuildBasicLeaderWorkerSet("test-sample", "default").
				Replica(1).
				WorkerTemplateSpec(wrappers.MakeWorkerPodSpec()).
				Annotation(map[string]string{
					leaderworkerset.SubGroupExclusiveKeyAnnotationKey: "topologyKey",
				}).Size(2).SubGroupSize(2).SubGroupType(leaderworkerset.SubGroupPolicyTypeLeaderExcluded).Obj(),
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
						leaderworkerset.RoleLabelKey:            leaderworkerset.RoleWorker,
						leaderworkerset.GroupUniqueHashLabelKey: "test-key",
					},
					Annotations: map[string]string{
						leaderworkerset.SubGroupExclusiveKeyAnnotationKey: "topologyKey",
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
								leaderworkerset.SubGroupPolicyTypeAnnotationKey:   "LeaderExcluded",
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
			name: "1 replica, size 1, with volumeClaimTemplates and PersistentVolumeClaimRetentionPolicy configured",
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
				Spec: corev1.PodSpec{
					Hostname:  "test-sample",
					Subdomain: "test-sample",
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
						leaderworkerset.RoleLabelKey:            leaderworkerset.RoleWorker,
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
			// Build the revision from this test case's lws, mirroring production where the
			// revision snapshots the same spec: revision-covered fields (size, subGroupPolicy,
			// networkConfig, volume claims) are read from the revision by the function under test.
			revision, err := revisionutils.NewRevision(context.TODO(), client, tc.lws, "")
			if err != nil {
				t.Fatal(err)
			}
			revisionKey := revisionutils.GetRevisionKey(revision)
			tc.pod.Labels[leaderworkerset.RevisionKey] = revisionKey
			tc.wantStatefulSetConfig.Labels[leaderworkerset.RevisionKey] = revisionKey
			tc.wantStatefulSetConfig.Spec.Template.Labels[leaderworkerset.RevisionKey] = revisionKey
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

func TestHandleRestartPolicyRespectsMaxGroupRestarts(t *testing.T) {
	tests := []struct {
		name              string
		policy            leaderworkerset.RestartPolicyType
		limit             *int32
		count             int32
		wantLeaderDeleted bool
		wantCount         int32
		wantExhausted     bool
		initialExhausted  bool
	}{
		{name: "nil budget preserves unbounded recreation", wantLeaderDeleted: true},
		{name: "zero budget terminates the first failed group", limit: ptr.To[int32](0), wantLeaderDeleted: true, wantExhausted: true},
		{name: "one remaining restart consumes budget and deletes the leader", limit: ptr.To[int32](1), wantLeaderDeleted: true, wantCount: 1},
		{name: "after-start policy consumes budget and deletes the leader", policy: leaderworkerset.RecreateGroupAfterStart, limit: ptr.To[int32](1), wantLeaderDeleted: true, wantCount: 1},
		{name: "exhausted budget terminates the group without incrementing", limit: ptr.To[int32](1), count: 1, wantLeaderDeleted: true, wantCount: 1, wantExhausted: true},
		{name: "increasing the limit keeps an already exhausted group terminating", limit: ptr.To[int32](2), count: 1, wantLeaderDeleted: true, wantCount: 1, wantExhausted: true, initialExhausted: true},
		{name: "unsetting the limit keeps an already exhausted group terminating", wantLeaderDeleted: true, wantExhausted: true, initialExhausted: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			scheme := runtime.NewScheme()
			if err := corev1.AddToScheme(scheme); err != nil {
				t.Fatal(err)
			}
			if err := leaderworkerset.AddToScheme(scheme); err != nil {
				t.Fatal(err)
			}
			policy := tc.policy
			if policy == "" {
				policy = leaderworkerset.RecreateGroupOnPodRestart
			}
			lws := wrappers.BuildLeaderWorkerSet("default").Replica(1).Size(1).
				RestartPolicy(policy).Obj()
			if tc.limit != nil {
				lws.Spec.LeaderWorkerTemplate.MaxGroupRestarts = tc.limit
			}
			if tc.count > 0 {
				lws.Annotations = map[string]string{
					leaderworkerset.GroupRestartCountsAnnotationKey: fmt.Sprintf(`{"revision-a/0":%d}`, tc.count),
				}
			}
			leader := wrappers.MakePodWithLabels(lws.Name, "0", "0", lws.Namespace, 1)
			leader.Labels[leaderworkerset.RevisionKey] = "revision-a"
			leader.Status.Phase = corev1.PodRunning
			leader.Status.ContainerStatuses = []corev1.ContainerStatus{{RestartCount: 1}}
			if tc.initialExhausted {
				leader.Annotations[leaderworkerset.GroupRestartBudgetExhaustedAnnotationKey] = "true"
				leader.Finalizers = []string{leaderworkerset.GroupRestartBudgetCleanupFinalizer}
			}

			fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(lws, leader).Build()
			r := &PodReconciler{Client: fakeClient, Record: fakeEventRecorder{}}
			deleted, _, err := r.handleRestartPolicy(context.Background(), *leader, *lws.DeepCopy())
			if err != nil {
				t.Fatalf("handleRestartPolicy() error = %v", err)
			}
			if deleted != tc.wantLeaderDeleted {
				t.Fatalf("leaderDeleted = %t, want %t", deleted, tc.wantLeaderDeleted)
			}

			var updatedLWS leaderworkerset.LeaderWorkerSet
			if err := fakeClient.Get(context.Background(), client.ObjectKeyFromObject(lws), &updatedLWS); err != nil {
				t.Fatal(err)
			}
			counts, err := parseGroupRestartCounts(updatedLWS.Annotations[leaderworkerset.GroupRestartCountsAnnotationKey])
			if err != nil {
				t.Fatal(err)
			}
			if got := counts["revision-a/0"]; got != tc.wantCount {
				t.Fatalf("restart count = %d, want %d", got, tc.wantCount)
			}
			if tc.wantExhausted {
				var terminating corev1.Pod
				if err := fakeClient.Get(context.Background(), client.ObjectKeyFromObject(leader), &terminating); err != nil {
					t.Fatal(err)
				}
				if terminating.Annotations[leaderworkerset.GroupRestartBudgetExhaustedAnnotationKey] != "true" {
					t.Fatal("terminating leader is missing the budget exhausted marker")
				}
				if !controllerutil.ContainsFinalizer(&terminating, leaderworkerset.GroupRestartBudgetCleanupFinalizer) {
					t.Fatal("terminating leader is missing the restart-budget cleanup finalizer")
				}
				if terminating.DeletionTimestamp == nil {
					t.Fatal("exhausted leader was not placed into deletion")
				}
			}
		})
	}
}

func TestReconcilePodDuringLWSDeletionOnlyRemovesBudgetFinalizer(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := leaderworkerset.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}

	now := metav1.Now()
	lws := wrappers.BuildLeaderWorkerSet("default").Replica(1).Size(2).
		RestartPolicy(leaderworkerset.RecreateGroupOnPodRestart).Obj()
	lws.DeletionTimestamp = &now
	lws.Finalizers = []string{"test.lws/finalizer"}
	lws.Annotations = map[string]string{
		leaderworkerset.GroupRestartCountsAnnotationKey: `{"revision-a/0":1}`,
	}
	leader := wrappers.MakePodWithLabels(lws.Name, "0", "0", lws.Namespace, 1)
	leader.UID = "leader-uid"
	leader.Labels[leaderworkerset.RevisionKey] = "revision-a"
	leader.Annotations[leaderworkerset.GroupRestartBudgetExhaustedAnnotationKey] = "true"
	leader.Finalizers = []string{leaderworkerset.GroupRestartBudgetCleanupFinalizer}
	worker := wrappers.MakePodWithLabels(lws.Name, "0", "1", lws.Namespace, 2)
	worker.Labels[leaderworkerset.RevisionKey] = "revision-a"
	worker.Finalizers = []string{leaderworkerset.GroupRestartBudgetCleanupFinalizer}
	worker.OwnerReferences = []metav1.OwnerReference{*metav1.NewControllerRef(leader, corev1.SchemeGroupVersion.WithKind("Pod"))}

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(lws, leader, worker).Build()
	r := &PodReconciler{Client: fakeClient, Record: fakeEventRecorder{}}
	if _, err := r.reconcilePod(context.Background(), podReconcileRequestForPod(leader, false)); err != nil {
		t.Fatalf("reconcilePod() error = %v", err)
	}

	var updatedLeader corev1.Pod
	if err := fakeClient.Get(context.Background(), client.ObjectKeyFromObject(leader), &updatedLeader); err != nil {
		t.Fatal(err)
	}
	if controllerutil.ContainsFinalizer(&updatedLeader, leaderworkerset.GroupRestartBudgetCleanupFinalizer) {
		t.Fatal("restart-budget finalizer was not removed during LWS deletion")
	}
	var updatedWorker corev1.Pod
	if err := fakeClient.Get(context.Background(), client.ObjectKeyFromObject(worker), &updatedWorker); err != nil {
		t.Fatal(err)
	}
	if controllerutil.ContainsFinalizer(&updatedWorker, leaderworkerset.GroupRestartBudgetCleanupFinalizer) {
		t.Fatal("worker restart-budget finalizer was not removed during LWS deletion")
	}
	var updatedLWS leaderworkerset.LeaderWorkerSet
	if err := fakeClient.Get(context.Background(), client.ObjectKeyFromObject(lws), &updatedLWS); err != nil {
		t.Fatal(err)
	}
	if got := updatedLWS.Annotations[leaderworkerset.GroupRestartCountsAnnotationKey]; got != `{"revision-a/0":1}` {
		t.Fatalf("restart counts changed during LWS deletion: %q", got)
	}
}

func TestReconcilePodWithoutLWSRemovesGroupFinalizers(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := leaderworkerset.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}

	leader := wrappers.MakePodWithLabels("missing-lws", "0", "0", "default", 2)
	leader.UID = "leader-uid"
	leader.Labels[leaderworkerset.RevisionKey] = "revision-a"
	leader.Finalizers = []string{leaderworkerset.GroupRestartBudgetCleanupFinalizer}
	worker := wrappers.MakePodWithLabels("missing-lws", "0", "1", "default", 2)
	worker.Labels[leaderworkerset.RevisionKey] = "revision-a"
	worker.Finalizers = []string{leaderworkerset.GroupRestartBudgetCleanupFinalizer}
	worker.OwnerReferences = []metav1.OwnerReference{*metav1.NewControllerRef(leader, corev1.SchemeGroupVersion.WithKind("Pod"))}

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(leader, worker).Build()
	r := &PodReconciler{Client: fakeClient, Record: fakeEventRecorder{}}
	if _, err := r.reconcilePod(context.Background(), podReconcileRequestForPod(leader, false)); err != nil {
		t.Fatalf("reconcilePod() error = %v", err)
	}

	for _, pod := range []*corev1.Pod{leader, worker} {
		var updated corev1.Pod
		if err := fakeClient.Get(context.Background(), client.ObjectKeyFromObject(pod), &updated); err != nil {
			t.Fatal(err)
		}
		if controllerutil.ContainsFinalizer(&updated, leaderworkerset.GroupRestartBudgetCleanupFinalizer) {
			t.Fatalf("restart-budget finalizer was not removed from %s", pod.Name)
		}
	}
}

func TestReconcilePodDuringLWSDeletionRemovesWorkerFinalizerWithoutLeaderFinalizer(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := leaderworkerset.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}

	now := metav1.Now()
	lws := wrappers.BuildLeaderWorkerSet("default").Replica(1).Size(2).Obj()
	lws.DeletionTimestamp = &now
	lws.Finalizers = []string{"test.lws/finalizer"}
	leader := wrappers.MakePodWithLabels(lws.Name, "0", "0", lws.Namespace, 2)
	leader.UID = "leader-uid"
	leader.Labels[leaderworkerset.RevisionKey] = "revision-a"
	worker := wrappers.MakePodWithLabels(lws.Name, "0", "1", lws.Namespace, 2)
	worker.Labels[leaderworkerset.RevisionKey] = "revision-a"
	worker.Finalizers = []string{leaderworkerset.GroupRestartBudgetCleanupFinalizer}
	worker.OwnerReferences = []metav1.OwnerReference{*metav1.NewControllerRef(leader, corev1.SchemeGroupVersion.WithKind("Pod"))}

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(lws, leader, worker).Build()
	r := &PodReconciler{Client: fakeClient, Record: fakeEventRecorder{}}
	if _, err := r.reconcilePod(context.Background(), podReconcileRequestForPod(leader, false)); err != nil {
		t.Fatalf("reconcilePod() error = %v", err)
	}

	var updatedWorker corev1.Pod
	if err := fakeClient.Get(context.Background(), client.ObjectKeyFromObject(worker), &updatedWorker); err != nil {
		t.Fatal(err)
	}
	if controllerutil.ContainsFinalizer(&updatedWorker, leaderworkerset.GroupRestartBudgetCleanupFinalizer) {
		t.Fatal("worker restart-budget finalizer was not removed")
	}
}

func TestHandleRestartPolicyRefreshesBudgetState(t *testing.T) {
	for _, deleting := range []bool{false, true} {
		t.Run(fmt.Sprintf("leaderDeleting=%t", deleting), func(t *testing.T) {
			scheme := runtime.NewScheme()
			for _, add := range []func(*runtime.Scheme) error{corev1.AddToScheme, leaderworkerset.AddToScheme} {
				if err := add(scheme); err != nil {
					t.Fatal(err)
				}
			}
			lws := wrappers.BuildLeaderWorkerSet("default").Replica(1).Size(1).
				RestartPolicy(leaderworkerset.RecreateGroupOnPodRestart).MaxGroupRestarts(1).Obj()
			staleLWS := lws.DeepCopy()
			lws.Annotations = map[string]string{leaderworkerset.GroupRestartCountsAnnotationKey: `{"revision-a/0":1}`}
			leader := wrappers.MakePodWithLabels(lws.Name, "0", "0", lws.Namespace, 1)
			leader.UID = "current-leader"
			leader.Labels[leaderworkerset.RevisionKey] = "revision-a"
			leader.Status.Phase = corev1.PodRunning
			leader.Status.ContainerStatuses = []corev1.ContainerStatus{{RestartCount: 1}}
			leader.Finalizers = []string{"test.lws/retain"}
			staleLeader := leader.DeepCopy()
			if deleting {
				now := metav1.Now()
				leader.DeletionTimestamp = &now
			}
			cli := fake.NewClientBuilder().WithScheme(scheme).WithObjects(lws, leader).Build()
			r := &PodReconciler{Client: cli, Record: fakeEventRecorder{}}
			if _, _, err := r.handleRestartPolicy(context.Background(), *staleLeader, *staleLWS); err != nil {
				t.Fatal(err)
			}
			var actual corev1.Pod
			if err := cli.Get(context.Background(), client.ObjectKeyFromObject(leader), &actual); err != nil {
				t.Fatal(err)
			}
			exhausted := actual.Annotations[leaderworkerset.GroupRestartBudgetExhaustedAnnotationKey] == "true"
			if exhausted != !deleting {
				t.Errorf("exhausted=%t, want %t; budget must use the current leader and count", exhausted, !deleting)
			}
		})
	}
}

func TestReconcilePodIgnoresRecoveredLeaderDeletion(t *testing.T) {
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{corev1.AddToScheme, appsv1.AddToScheme, leaderworkerset.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	lws := wrappers.BuildLeaderWorkerSet("default").Replica(1).Size(2).
		RestartPolicy(leaderworkerset.RecreateGroupOnPodRestart).MaxGroupRestarts(2).Obj()
	lws.Annotations = map[string]string{leaderworkerset.GroupRestartCountsAnnotationKey: `{"revision-a/0":2}`}
	leader := wrappers.MakePodWithLabels(lws.Name, "0", "0", lws.Namespace, 2)
	leader.UID = "replacement-leader"
	leader.Labels[leaderworkerset.RevisionKey] = "revision-a"
	leader.Annotations[leaderworkerset.GroupRestartBudgetExhaustedAnnotationKey] = "true"
	leader.Finalizers = []string{leaderworkerset.GroupRestartBudgetCleanupFinalizer}
	worker := wrappers.MakePodWithLabels(lws.Name, "0", "1", lws.Namespace, 2)
	worker.Labels[leaderworkerset.RevisionKey] = "revision-a"
	worker.Finalizers = []string{leaderworkerset.GroupRestartBudgetCleanupFinalizer}
	worker.OwnerReferences = []metav1.OwnerReference{*metav1.NewControllerRef(leader, corev1.SchemeGroupVersion.WithKind("Pod"))}
	oldLeader := leader.DeepCopy()
	oldLeader.UID = "recovered-leader"
	oldLeader.Finalizers = nil
	oldLeader.Annotations[leaderworkerset.GroupRestartBudgetRecoverAnnotationKey] = "true"
	now := metav1.Now()
	oldLeader.DeletionTimestamp = &now
	cli := fake.NewClientBuilder().WithScheme(scheme).WithObjects(lws, leader, worker).Build()
	r := &PodReconciler{Client: cli, Record: fakeEventRecorder{}}
	if _, err := r.reconcilePod(context.Background(), podReconcileRequestForPod(oldLeader, true)); err != nil {
		t.Fatal(err)
	}
	var updatedLWS leaderworkerset.LeaderWorkerSet
	if err := cli.Get(context.Background(), client.ObjectKeyFromObject(lws), &updatedLWS); err != nil {
		t.Fatal(err)
	}
	if got := updatedLWS.Annotations[leaderworkerset.GroupRestartCountsAnnotationKey]; got != lws.Annotations[leaderworkerset.GroupRestartCountsAnnotationKey] {
		t.Errorf("old leader deletion changed replacement restart counts: %q", got)
	}
	var updatedWorker corev1.Pod
	if err := cli.Get(context.Background(), client.ObjectKeyFromObject(worker), &updatedWorker); err != nil {
		t.Fatal(err)
	}
	if !controllerutil.ContainsFinalizer(&updatedWorker, leaderworkerset.GroupRestartBudgetCleanupFinalizer) {
		t.Error("old leader deletion released the replacement worker")
	}
}

func TestExhaustedGroupFinalizesWorkersAndRecoversExplicitly(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := appsv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := leaderworkerset.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}

	lws := wrappers.BuildLeaderWorkerSet("default").Name("test-sample").Replica(2).Size(2).
		RestartPolicy(leaderworkerset.RecreateGroupOnPodRestart).MaxGroupRestarts(1).Obj()
	lws.Annotations = map[string]string{
		leaderworkerset.GroupRestartCountsAnnotationKey: `{"revision-a/0":1,"revision-a/1":1}`,
	}
	leader := wrappers.MakePodWithLabels(lws.Name, "0", "0", lws.Namespace, 2)
	leader.UID = "leader-uid"
	leader.Labels[leaderworkerset.RevisionKey] = "revision-a"
	leader.Annotations = map[string]string{leaderworkerset.GroupRestartBudgetRecoverAnnotationKey: "true"}
	worker := wrappers.MakePodWithLabels(lws.Name, "0", "1", lws.Namespace, 2)
	worker.UID = "worker-uid"
	worker.Labels[leaderworkerset.RevisionKey] = "revision-a"
	worker.OwnerReferences = []metav1.OwnerReference{*metav1.NewControllerRef(leader, corev1.SchemeGroupVersion.WithKind("Pod"))}
	worker.Status.Phase = corev1.PodRunning
	worker.Status.ContainerStatuses = []corev1.ContainerStatus{{RestartCount: 1}}
	otherLeader := wrappers.MakePodWithLabels(lws.Name, "1", "0", lws.Namespace, 2)
	otherLeader.Labels[leaderworkerset.RevisionKey] = "revision-a"
	otherLeader.Finalizers = []string{leaderworkerset.GroupRestartBudgetCleanupFinalizer}

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(lws, leader, worker, otherLeader).Build()
	r := &PodReconciler{Client: fakeClient, Record: fakeEventRecorder{}}
	deleted, _, err := r.handleRestartPolicy(context.Background(), *worker, *lws.DeepCopy())
	if err != nil {
		t.Fatalf("handleRestartPolicy() error = %v", err)
	}
	if !deleted {
		t.Fatal("exhausted group leader was not deleted")
	}

	var terminatingLeader, terminatingWorker corev1.Pod
	if err := fakeClient.Get(context.Background(), client.ObjectKeyFromObject(leader), &terminatingLeader); err != nil {
		t.Fatal(err)
	}
	if err := fakeClient.Get(context.Background(), client.ObjectKeyFromObject(worker), &terminatingWorker); err != nil {
		t.Fatal(err)
	}
	for _, pod := range []*corev1.Pod{&terminatingLeader, &terminatingWorker} {
		if !controllerutil.ContainsFinalizer(pod, leaderworkerset.GroupRestartBudgetCleanupFinalizer) {
			t.Fatalf("Pod %s is missing the restart-budget finalizer", pod.Name)
		}
	}
	if terminatingLeader.DeletionTimestamp == nil {
		t.Fatal("exhausted leader is not terminating")
	}
	if terminatingLeader.Annotations[leaderworkerset.GroupRestartBudgetRecoverAnnotationKey] != "" {
		t.Fatal("a recovery annotation set before exhaustion was not cleared")
	}

	// Deletion alone is not a recovery signal.
	if _, err := r.reconcilePod(context.Background(), podReconcileRequestForPod(&terminatingLeader, false)); err != nil {
		t.Fatalf("reconcilePod() without recovery annotation error = %v", err)
	}
	var stillTerminating corev1.Pod
	if err := fakeClient.Get(context.Background(), client.ObjectKeyFromObject(leader), &stillTerminating); err != nil {
		t.Fatal(err)
	}
	if !controllerutil.ContainsFinalizer(&stillTerminating, leaderworkerset.GroupRestartBudgetCleanupFinalizer) {
		t.Fatal("leader finalizer was removed without explicit recovery")
	}

	if stillTerminating.Annotations == nil {
		stillTerminating.Annotations = map[string]string{}
	}
	stillTerminating.Annotations[leaderworkerset.GroupRestartBudgetRecoverAnnotationKey] = "true"
	if err := fakeClient.Update(context.Background(), &stillTerminating); err != nil {
		t.Fatal(err)
	}
	if _, err := r.reconcilePod(context.Background(), podReconcileRequestForPod(&stillTerminating, false)); err != nil {
		t.Fatalf("reconcilePod() recovery error = %v", err)
	}

	var recoveredWorker corev1.Pod
	if err := fakeClient.Get(context.Background(), client.ObjectKeyFromObject(worker), &recoveredWorker); err != nil {
		t.Fatal(err)
	}
	if controllerutil.ContainsFinalizer(&recoveredWorker, leaderworkerset.GroupRestartBudgetCleanupFinalizer) {
		t.Fatal("worker finalizer was not removed during recovery")
	}
	var untouchedLeader corev1.Pod
	if err := fakeClient.Get(context.Background(), client.ObjectKeyFromObject(otherLeader), &untouchedLeader); err != nil {
		t.Fatal(err)
	}
	if !controllerutil.ContainsFinalizer(&untouchedLeader, leaderworkerset.GroupRestartBudgetCleanupFinalizer) {
		t.Fatal("recovery removed another group's finalizer")
	}
	var updatedLWS leaderworkerset.LeaderWorkerSet
	if err := fakeClient.Get(context.Background(), client.ObjectKeyFromObject(lws), &updatedLWS); err != nil {
		t.Fatal(err)
	}
	counts, err := parseGroupRestartCounts(updatedLWS.Annotations[leaderworkerset.GroupRestartCountsAnnotationKey])
	if err != nil {
		t.Fatal(err)
	}
	if _, found := counts["revision-a/0"]; found || counts["revision-a/1"] != 1 {
		t.Fatalf("recovery changed the wrong restart counts: %#v", counts)
	}
}

func TestGroupLifecycleTeardownRequested(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := appsv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := leaderworkerset.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name        string
		replicas    int32
		stsReplicas int32
		groupIndex  string
		podRevision string
		stsRevision string
		partition   int32
		want        bool
	}{
		{name: "scale down removes the exhausted ordinal", replicas: 1, stsReplicas: 1, groupIndex: "1", podRevision: "revision-a", want: true},
		{name: "active surge ordinal is not scale down", replicas: 1, stsReplicas: 2, groupIndex: "1", podRevision: "revision-a", stsRevision: "revision-a"},
		{name: "rollout removes an ordinal inside the partition", replicas: 2, stsReplicas: 2, groupIndex: "1", podRevision: "revision-a", stsRevision: "revision-b", partition: 1, want: true},
		{name: "rollout preserves an ordinal below the partition", replicas: 2, stsReplicas: 2, groupIndex: "0", podRevision: "revision-a", stsRevision: "revision-b", partition: 1},
		{name: "current revision is not teardown", replicas: 2, stsReplicas: 2, groupIndex: "1", podRevision: "revision-a", stsRevision: "revision-a", partition: 1},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			lws := wrappers.BuildLeaderWorkerSet("default").Name("test-sample").Replica(int(tc.replicas)).Obj()
			leader := wrappers.MakePodWithLabels(lws.Name, tc.groupIndex, "0", lws.Namespace, 1)
			leader.Labels[leaderworkerset.RevisionKey] = tc.podRevision
			sts := &appsv1.StatefulSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      lws.Name,
					Namespace: lws.Namespace,
					Labels:    map[string]string{leaderworkerset.RevisionKey: tc.stsRevision},
				},
				Spec: appsv1.StatefulSetSpec{UpdateStrategy: appsv1.StatefulSetUpdateStrategy{
					RollingUpdate: &appsv1.RollingUpdateStatefulSetStrategy{Partition: ptr.To(tc.partition)},
				}, Replicas: ptr.To(tc.stsReplicas)},
			}
			r := &PodReconciler{Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(sts).Build()}
			got, err := r.groupLifecycleTeardownRequested(context.Background(), lws, leader)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("groupLifecycleTeardownRequested() = %t, want %t", got, tc.want)
			}
		})
	}
}

func TestClearGroupRestartCountResetsOnlyCurrentRevisionAndGroup(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := leaderworkerset.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	lws := wrappers.BuildLeaderWorkerSet("default").Obj()
	lws.Annotations = map[string]string{
		leaderworkerset.GroupRestartCountsAnnotationKey: `{"revision-a/0":1,"revision-a/1":2}`,
	}
	leader := wrappers.MakePodWithLabels(lws.Name, "0", "0", lws.Namespace, 1)
	leader.Labels[leaderworkerset.RevisionKey] = "revision-a"
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(lws).Build()
	r := &PodReconciler{Client: fakeClient}
	if err := r.clearGroupRestartCount(context.Background(), lws, leader); err != nil {
		t.Fatal(err)
	}
	var updated leaderworkerset.LeaderWorkerSet
	if err := fakeClient.Get(context.Background(), client.ObjectKeyFromObject(lws), &updated); err != nil {
		t.Fatal(err)
	}
	counts, err := parseGroupRestartCounts(updated.Annotations[leaderworkerset.GroupRestartCountsAnnotationKey])
	if err != nil {
		t.Fatal(err)
	}
	if _, found := counts["revision-a/0"]; found || counts["revision-a/1"] != 2 {
		t.Fatalf("unexpected counts after reset: %#v", counts)
	}
}

func TestPersistGroupRestartCountPreservesConcurrentGroupUpdates(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := leaderworkerset.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	stored := wrappers.BuildLeaderWorkerSet("default").Obj()
	stored.Annotations = map[string]string{
		leaderworkerset.GroupRestartCountsAnnotationKey: `{"revision-a/1":2}`,
	}
	stale := stored.DeepCopy()
	stale.Annotations[leaderworkerset.GroupRestartCountsAnnotationKey] = `{"revision-a/0":1}`
	leader := wrappers.MakePodWithLabels(stored.Name, "0", "0", stored.Namespace, 1)
	leader.Labels[leaderworkerset.RevisionKey] = "revision-a"

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(stored).Build()
	r := &PodReconciler{Client: fakeClient}
	if err := r.persistGroupRestartCount(context.Background(), stale, leader, 1); err != nil {
		t.Fatal(err)
	}

	var updated leaderworkerset.LeaderWorkerSet
	if err := fakeClient.Get(context.Background(), client.ObjectKeyFromObject(stored), &updated); err != nil {
		t.Fatal(err)
	}
	counts, err := parseGroupRestartCounts(updated.Annotations[leaderworkerset.GroupRestartCountsAnnotationKey])
	if err != nil {
		t.Fatal(err)
	}
	if counts["revision-a/0"] != 1 || counts["revision-a/1"] != 2 {
		t.Fatalf("concurrent group count was lost: %#v", counts)
	}
}

func TestParseGroupRestartCountsRejectsNegativeCount(t *testing.T) {
	if _, err := parseGroupRestartCounts(`{"revision-a/0":-1}`); err == nil {
		t.Fatal("expected negative persisted count to be rejected")
	}
}

func TestParseGroupRestartCountsRejectsNull(t *testing.T) {
	if _, err := parseGroupRestartCounts(`null`); err == nil {
		t.Fatal("null restart counts must be rejected before a map write can panic")
	}
}

func TestSetNodeSelectorForWorkerPodsReturnsNotFoundWhenLeaderNodeIsMissing(t *testing.T) {
	reconciler := PodReconciler{Client: fake.NewClientBuilder().Build()}
	leaderPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default"},
		Spec:       corev1.PodSpec{NodeName: "missing-node"},
	}
	workerStatefulSet := &appsapplyv1.StatefulSetApplyConfiguration{
		Spec: &appsapplyv1.StatefulSetSpecApplyConfiguration{
			Template: &coreapplyv1.PodTemplateSpecApplyConfiguration{
				Spec: &coreapplyv1.PodSpecApplyConfiguration{},
			},
		},
	}

	err := reconciler.setNodeSelectorForWorkerPods(context.Background(), leaderPod, workerStatefulSet, "topology.kubernetes.io/zone")
	if !apierrors.IsNotFound(err) {
		t.Fatalf("setNodeSelectorForWorkerPods() error = %v, want NotFound", err)
	}
	if workerStatefulSet.Spec.Template.Spec.NodeSelector != nil {
		t.Fatalf("setNodeSelectorForWorkerPods() set a node selector after a missing leader node: %v", workerStatefulSet.Spec.Template.Spec.NodeSelector)
	}
}

func TestSetNodeSelectorForWorkerPodsReturnsErrorNamingLabelWhenTopologyLabelMissing(t *testing.T) {
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "node-without-topology-label"},
	}
	reconciler := PodReconciler{Client: fake.NewClientBuilder().WithObjects(node).Build()}
	// Node is cluster-scoped, so its namespace bucket in the fake tracker is "";
	// leave the pod's namespace empty too so the lookup below matches it.
	leaderPod := &corev1.Pod{
		Spec: corev1.PodSpec{NodeName: node.Name},
	}
	workerStatefulSet := &appsapplyv1.StatefulSetApplyConfiguration{
		Spec: &appsapplyv1.StatefulSetSpecApplyConfiguration{
			Template: &coreapplyv1.PodTemplateSpecApplyConfiguration{
				Spec: &coreapplyv1.PodSpecApplyConfiguration{},
			},
		},
	}

	err := reconciler.setNodeSelectorForWorkerPods(context.Background(), leaderPod, workerStatefulSet, "topology.kubernetes.io/zone")
	if err == nil {
		t.Fatal("setNodeSelectorForWorkerPods() error = nil, want error naming the missing topology label")
	}
	if !strings.Contains(err.Error(), "topology.kubernetes.io/zone") {
		t.Fatalf("setNodeSelectorForWorkerPods() error = %q, want it to name the missing label key %q", err.Error(), "topology.kubernetes.io/zone")
	}
}

func TestWorkerStatefulSetApplyConfigPropagatesObjectMeta(t *testing.T) {
	client := fake.NewClientBuilder().Build()
	lws := wrappers.BuildBasicLeaderWorkerSet("test-sample", "default").
		Labels(map[string]string{
			"app":                              "inference",
			leaderworkerset.GroupIndexLabelKey: "user-value",
		}).
		Annotation(map[string]string{"owner": "platform"}).
		Replica(1).
		WorkerTemplateSpec(wrappers.MakeWorkerPodSpec()).
		Size(2).
		Obj()
	revision, err := revisionutils.NewRevision(context.TODO(), client, lws, "")
	if err != nil {
		t.Fatal(err)
	}
	revisionKey := revisionutils.GetRevisionKey(revision)

	leaderPod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-sample",
			Namespace: "default",
			Labels: map[string]string{
				leaderworkerset.SetNameLabelKey:         "test-sample",
				leaderworkerset.GroupIndexLabelKey:      "1",
				leaderworkerset.GroupUniqueHashLabelKey: "test-key",
				leaderworkerset.RevisionKey:             revisionKey,
			},
		},
	}

	statefulSetConfig, err := constructWorkerStatefulSetApplyConfiguration(leaderPod, *lws)
	if err != nil {
		t.Fatalf("failed with error %s", err.Error())
	}

	wantLabels := map[string]string{
		"app":                                   "inference",
		leaderworkerset.SetNameLabelKey:         "test-sample",
		leaderworkerset.GroupIndexLabelKey:      "1",
		leaderworkerset.GroupUniqueHashLabelKey: "test-key",
		leaderworkerset.RevisionKey:             revisionKey,
		leaderworkerset.RoleLabelKey:            leaderworkerset.RoleWorker,
	}
	if diff := cmp.Diff(wantLabels, statefulSetConfig.Labels); diff != "" {
		t.Errorf("unexpected StatefulSet labels: %s", diff)
	}

	wantAnnotations := map[string]string{"owner": "platform"}
	if diff := cmp.Diff(wantAnnotations, statefulSetConfig.Annotations); diff != "" {
		t.Errorf("unexpected StatefulSet annotations: %s", diff)
	}
}

func TestWorkerStatefulSetApplyConfigCarriesLeaderGroupIncarnation(t *testing.T) {
	lws := wrappers.BuildBasicLeaderWorkerSet("test-sample", "default").
		Replica(1).
		WorkerTemplateSpec(wrappers.MakeWorkerPodSpec()).
		Size(2).
		Obj()
	lws.UID = "test-sample-uid"
	lws.Spec.Scheduling = &leaderworkerset.LeaderWorkerSetScheduling{}
	workloadName := schedulerprovider.KubernetesWorkloadName(lws)

	for _, tc := range []struct {
		name string
		// leaderWorkloadName is the workload name the pod webhook recorded on
		// the leader pod.
		leaderWorkloadName string
		want               string
	}{
		{name: "leader with a group incarnation", leaderWorkloadName: workloadName + ".x7k2p", want: workloadName + ".x7k2p"},
		{name: "leader admitted before group incarnations were introduced", leaderWorkloadName: workloadName, want: workloadName},
		{name: "leader without a workload name", want: workloadName},
		{name: "leader with a foreign workload name", leaderWorkloadName: "other-workload.x7k2p", want: workloadName},
	} {
		t.Run(tc.name, func(t *testing.T) {
			leaderPod := corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sample-1",
					Namespace: "default",
					Labels: map[string]string{
						leaderworkerset.SetNameLabelKey:    "test-sample",
						leaderworkerset.GroupIndexLabelKey: "1",
						leaderworkerset.RevisionKey:        "rev1",
					},
				},
			}
			if tc.leaderWorkloadName != "" {
				leaderPod.Annotations = map[string]string{schedulerprovider.WorkloadNameAnnotationKey: tc.leaderWorkloadName}
			}

			statefulSetConfig, err := constructWorkerStatefulSetApplyConfiguration(leaderPod, *lws)
			if err != nil {
				t.Fatalf("failed with error %s", err.Error())
			}
			if got := statefulSetConfig.Spec.Template.Annotations[schedulerprovider.WorkloadNameAnnotationKey]; got != tc.want {
				t.Errorf("worker template workload name = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestPodReconcilerRecreatesGroupSplitByUpgrade(t *testing.T) {
	const incarnation = ".x7k2p"
	for _, tc := range []struct {
		name string
		// leaderIncarnation and templateIncarnation follow the workload name
		// on the leader pod and on the worker statefulset template.
		leaderIncarnation   string
		templateIncarnation string
		// previousLeader makes a previous leader pod of the group control the
		// worker statefulset.
		previousLeader bool
		terminating    bool
		workerNode     string
		wantRecreate   bool
	}{
		{
			name:              "workers created by a previous LWS version do not join the leader's PodGroups",
			leaderIncarnation: incarnation,
			wantRecreate:      true,
		},
		{
			name:              "a split group with a scheduled worker is left alone",
			leaderIncarnation: incarnation,
			workerNode:        "node-1",
		},
		{
			name:                "workers join the leader's PodGroups",
			leaderIncarnation:   incarnation,
			templateIncarnation: incarnation,
		},
		{
			name: "a leader admitted before group incarnations were introduced",
		},
		{
			name:              "a worker statefulset of a previous leader",
			leaderIncarnation: incarnation,
			previousLeader:    true,
		},
		{
			name:              "a terminating worker statefulset",
			leaderIncarnation: incarnation,
			terminating:       true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			scheme := podCtrlTestScheme(t)
			lws := podCtrlBasicLWS()
			lws.UID = "test-lws-uid"
			lws.Spec.Scheduling = &leaderworkerset.LeaderWorkerSetScheduling{}
			workloadName := schedulerprovider.KubernetesWorkloadName(lws)
			leader := podCtrlLeaderPod(lws, "test-lws-0")
			leader.Annotations = map[string]string{schedulerprovider.WorkloadNameAnnotationKey: workloadName + tc.leaderIncarnation}
			revision := podCtrlRevisionFor(t, scheme, lws, leader)
			owner := leader
			if tc.previousLeader {
				owner = leader.DeepCopy()
				owner.UID = "previous-leader"
			}
			workerSts := &appsv1.StatefulSet{
				ObjectMeta: metav1.ObjectMeta{
					Name: leader.Spec.Hostname, Namespace: lws.Namespace, UID: "workers",
					OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(owner, corev1.SchemeGroupVersion.WithKind("Pod"))},
				},
				Spec: appsv1.StatefulSetSpec{
					Replicas: ptr.To[int32](1),
					Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{
						Annotations: map[string]string{schedulerprovider.WorkloadNameAnnotationKey: workloadName + tc.templateIncarnation},
					}},
				},
			}
			if tc.terminating {
				workerSts.Finalizers = []string{"leaderworkerset.sigs.k8s.io/test"}
			}
			worker := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name: leader.Name + "-1", Namespace: lws.Namespace, UID: "worker",
					Labels: map[string]string{
						leaderworkerset.SetNameLabelKey:     lws.Name,
						leaderworkerset.GroupIndexLabelKey:  "0",
						leaderworkerset.WorkerIndexLabelKey: "1",
					},
					OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(workerSts, appsv1.SchemeGroupVersion.WithKind("StatefulSet"))},
				},
				Spec: corev1.PodSpec{NodeName: tc.workerNode},
			}
			// The fake client neither enforces UID preconditions nor
			// propagates deletion, so check the request instead.
			var leaderDeletes []*client.DeleteOptions
			k8sClient := fake.NewClientBuilder().WithScheme(scheme).
				WithObjects(lws, revision, leader, workerSts, worker).
				WithInterceptorFuncs(interceptor.Funcs{
					Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
						if _, isPod := obj.(*corev1.Pod); isPod && obj.GetName() == leader.Name {
							options := &client.DeleteOptions{}
							options.ApplyOptions(opts)
							leaderDeletes = append(leaderDeletes, options)
						}
						return c.Delete(ctx, obj, opts...)
					},
				}).
				Build()
			if tc.terminating {
				if err := k8sClient.Delete(ctx, workerSts); err != nil {
					t.Fatal(err)
				}
			}
			recorder := events.NewFakeRecorder(10)
			reconciler := PodReconciler{Client: k8sClient, Scheme: scheme, Record: recorder, SchedulerProvider: &stubSchedulerProvider{}}

			result, err := reconciler.reconcilePod(ctx, podReconcileRequestForPod(leader, false))
			if err != nil || !result.IsZero() {
				t.Fatalf("reconcilePod() = %+v, %v, want a zero result and no error", result, err)
			}
			var recreateEvents []string
			for _, event := range podCtrlDrainEvents(recorder) {
				if strings.Contains(event, "RecreateGroup") {
					recreateEvents = append(recreateEvents, event)
				}
			}
			err = k8sClient.Get(ctx, client.ObjectKeyFromObject(leader), &corev1.Pod{})
			if !tc.wantRecreate {
				if err != nil || len(leaderDeletes) != 0 || len(recreateEvents) != 0 {
					t.Fatalf("group was recreated: get leader error %v, %d leader deletions, events %v", err, len(leaderDeletes), recreateEvents)
				}
				return
			}
			if !apierrors.IsNotFound(err) {
				t.Errorf("leader pod was not deleted, get error %v", err)
			}
			if len(leaderDeletes) != 1 {
				t.Fatalf("leader pod deletions = %d, want 1", len(leaderDeletes))
			}
			if got := ptr.Deref(leaderDeletes[0].PropagationPolicy, ""); got != metav1.DeletePropagationForeground {
				t.Errorf("leader pod deletion propagation = %q, want %q", got, metav1.DeletePropagationForeground)
			}
			if preconditions := leaderDeletes[0].Preconditions; preconditions == nil || ptr.Deref(preconditions.UID, "") != leader.UID {
				t.Errorf("leader pod deletion preconditions = %+v, want UID %q", preconditions, leader.UID)
			}
			if len(recreateEvents) != 1 || !strings.HasPrefix(recreateEvents[0], corev1.EventTypeNormal+" RecreateGroup ") {
				t.Errorf("events = %v, want one %s RecreateGroup event", recreateEvents, corev1.EventTypeNormal)
			}
		})
	}
}

func TestHandleRestartPolicyUsesCurrentWorkerOwnership(t *testing.T) {
	lws := wrappers.BuildLeaderWorkerSet("default").Replica(1).Size(2).RestartPolicy(leaderworkerset.RecreateGroupOnPodRestart).Obj()
	revisionKey := "revision-1"

	makeLeaderPod := func(uid types.UID) *corev1.Pod {
		return &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      lws.Name + "-0",
				Namespace: lws.Namespace,
				UID:       uid,
				Labels: map[string]string{
					leaderworkerset.SetNameLabelKey:     lws.Name,
					leaderworkerset.WorkerIndexLabelKey: "0",
					leaderworkerset.GroupIndexLabelKey:  "0",
					leaderworkerset.RevisionKey:         revisionKey,
				},
			},
		}
	}

	makeWorkerStatefulSet := func(uid types.UID, leader *corev1.Pod) *appsv1.StatefulSet {
		return &appsv1.StatefulSet{
			ObjectMeta: metav1.ObjectMeta{
				Name:            leader.Name,
				Namespace:       leader.Namespace,
				UID:             uid,
				OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(leader, corev1.SchemeGroupVersion.WithKind("Pod"))},
			},
		}
	}

	makeWorkerPod := func(owner metav1.OwnerReference) *corev1.Pod {
		return &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      lws.Name + "-0-1",
				Namespace: lws.Namespace,
				Labels: map[string]string{
					leaderworkerset.SetNameLabelKey:     lws.Name,
					leaderworkerset.WorkerIndexLabelKey: "1",
					leaderworkerset.GroupIndexLabelKey:  "0",
					leaderworkerset.RevisionKey:         revisionKey,
				},
				OwnerReferences: []metav1.OwnerReference{owner},
			},
		}
	}

	deletingWorker := func(w *corev1.Pod) corev1.Pod {
		p := w.DeepCopy()
		now := v1.Now()
		p.DeletionTimestamp = &now
		return *p
	}

	stsRef := func(s *appsv1.StatefulSet) metav1.OwnerReference {
		return *metav1.NewControllerRef(s, appsv1.SchemeGroupVersion.WithKind("StatefulSet"))
	}

	currentLeader := makeLeaderPod("leader-current")
	currentSts := makeWorkerStatefulSet("sts-current", currentLeader)
	staleSts := makeWorkerStatefulSet("sts-stale", currentLeader)

	tests := []struct {
		name              string
		objects           []client.Object
		reconciledPod     corev1.Pod
		wantLeaderDeleted bool
	}{
		{
			name:              "current worker statefulset owner triggers group recreation",
			objects:           []client.Object{currentLeader, currentSts, makeWorkerPod(stsRef(currentSts))},
			reconciledPod:     deletingWorker(makeWorkerPod(stsRef(currentSts))),
			wantLeaderDeleted: true,
		},
		{
			name:              "stale worker statefulset owner is ignored",
			objects:           []client.Object{currentLeader, currentSts, makeWorkerPod(stsRef(staleSts))},
			reconciledPod:     deletingWorker(makeWorkerPod(stsRef(staleSts))),
			wantLeaderDeleted: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			scheme := runtime.NewScheme()
			for _, add := range []func(*runtime.Scheme) error{corev1.AddToScheme, appsv1.AddToScheme, leaderworkerset.AddToScheme} {
				if err := add(scheme); err != nil {
					t.Fatal(err)
				}
			}
			objects := append([]client.Object{lws.DeepCopy()}, tc.objects...)
			fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
			reconciler := PodReconciler{Client: fakeClient, Record: fakeEventRecorder{}}

			leaderDeleted, _, err := reconciler.handleRestartPolicy(context.Background(), tc.reconciledPod, *lws.DeepCopy())
			if err != nil {
				t.Fatalf("handleRestartPolicy() error = %v", err)
			}
			if leaderDeleted != tc.wantLeaderDeleted {
				t.Fatalf("handleRestartPolicy() leaderDeleted = %t, want %t", leaderDeleted, tc.wantLeaderDeleted)
			}

			var leader corev1.Pod
			err = fakeClient.Get(context.Background(), client.ObjectKey{Name: lws.Name + "-0", Namespace: lws.Namespace}, &leader)
			if tc.wantLeaderDeleted && !apierrors.IsNotFound(err) {
				t.Fatalf("leader pod still exists after recreation trigger, err = %v", err)
			}
			if !tc.wantLeaderDeleted && err != nil {
				t.Fatalf("leader pod should still exist, err = %v", err)
			}
		})
	}
}

func TestPodEventHandlerKeepsDeletedPodIdentity(t *testing.T) {
	controller := true
	oldPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-sample-0-1",
			Namespace: "default",
			UID:       "worker-old",
			Labels: map[string]string{
				leaderworkerset.SetNameLabelKey:     "test-sample",
				leaderworkerset.WorkerIndexLabelKey: "1",
				leaderworkerset.GroupIndexLabelKey:  "0",
				leaderworkerset.RevisionKey:         "revision-1",
			},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "apps/v1",
				Kind:       "StatefulSet",
				Name:       "test-sample-0",
				UID:        "worker-sts",
				Controller: &controller,
			}},
		},
	}
	replacementPod := oldPod.DeepCopy()
	replacementPod.UID = "worker-replacement"
	wantDeletedPod := oldPod.DeepCopy()

	queue := workqueue.NewTypedRateLimitingQueue(
		workqueue.DefaultTypedControllerRateLimiter[podReconcileRequest](),
	)
	defer queue.ShutDown()

	handler := podEventHandler()
	handler.Delete(context.Background(), event.TypedDeleteEvent[client.Object]{Object: oldPod}, queue)
	handler.Create(context.Background(), event.TypedCreateEvent[client.Object]{Object: replacementPod}, queue)
	oldPod.Labels[leaderworkerset.SetNameLabelKey] = "mutated-after-enqueue"

	if got := queue.Len(); got != 2 {
		t.Fatalf("queue length = %d, want 2 distinct requests", got)
	}

	requests := make(map[types.UID]podReconcileRequest, 2)
	for range 2 {
		request, shutdown := queue.Get()
		if shutdown {
			t.Fatal("queue shut down before returning both requests")
		}
		requests[request.UID] = request
		queue.Done(request)
	}

	gotDeleted := requests[oldPod.UID]
	if gotDeleted.DeletedPod == nil {
		t.Fatal("deleted Pod request does not contain a Pod snapshot")
	}
	if gotDeleted.DeletedPod == oldPod {
		t.Fatal("deleted Pod request contains the informer object instead of a deep copy")
	}
	if gotDeleted.DeletedPod.DeletionTimestamp == nil {
		t.Fatal("deleted Pod snapshot does not have a deletion timestamp")
	}

	wantDeletedPod.DeletionTimestamp = gotDeleted.DeletedPod.DeletionTimestamp.DeepCopy()
	wantDeleted := podReconcileRequest{
		NamespacedName: types.NamespacedName{Name: oldPod.Name, Namespace: oldPod.Namespace},
		UID:            oldPod.UID,
		DeletedPod:     wantDeletedPod,
	}
	if diff := cmp.Diff(wantDeleted, gotDeleted); diff != "" {
		t.Fatalf("unexpected deleted Pod request (-want,+got):\n%s", diff)
	}
	if got := requests[replacementPod.UID]; got.DeletedPod != nil {
		t.Fatalf("replacement Pod request is unexpectedly marked deleted: %#v", got)
	}
}

func TestStatefulSetEventHandlerEnqueuesControllerPod(t *testing.T) {
	controller := true
	statefulSet := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{
		Name:      "test-sample-0",
		Namespace: "default",
		OwnerReferences: []metav1.OwnerReference{{
			APIVersion: "v1",
			Kind:       "Pod",
			Name:       "test-sample-0",
			UID:        "leader-current",
			Controller: &controller,
		}},
	}}
	queue := workqueue.NewTypedRateLimitingQueue(
		workqueue.DefaultTypedControllerRateLimiter[podReconcileRequest](),
	)
	defer queue.ShutDown()

	r := &PodReconciler{Client: fake.NewClientBuilder().Build()}
	r.statefulSetEventHandler().Create(
		context.Background(),
		event.TypedCreateEvent[client.Object]{Object: statefulSet},
		queue,
	)

	request, shutdown := queue.Get()
	if shutdown {
		t.Fatal("queue shut down before returning the request")
	}
	queue.Done(request)
	want := podReconcileRequest{
		NamespacedName: types.NamespacedName{Name: "test-sample-0", Namespace: "default"},
		UID:            "leader-current",
	}
	if diff := cmp.Diff(want, request); diff != "" {
		t.Fatalf("unexpected StatefulSet owner request (-want,+got):\n%s", diff)
	}
}

func TestReconcileDeletedWorkerAfterSameNameReplacement(t *testing.T) {
	lws := wrappers.BuildLeaderWorkerSet("default").
		Name("test-sample").
		Replica(1).
		Size(2).
		RestartPolicy(leaderworkerset.RecreateGroupOnPodRestart).
		Obj()
	revisionKey := "revision-1"

	leaderPod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name:      "test-sample-0",
		Namespace: lws.Namespace,
		UID:       "leader-current",
		Labels: map[string]string{
			leaderworkerset.SetNameLabelKey:     lws.Name,
			leaderworkerset.WorkerIndexLabelKey: "0",
			leaderworkerset.GroupIndexLabelKey:  "0",
			leaderworkerset.RevisionKey:         revisionKey,
		},
	}}
	workerStatefulSet := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{
		Name:            leaderPod.Name,
		Namespace:       leaderPod.Namespace,
		UID:             "worker-sts-current",
		OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(leaderPod, corev1.SchemeGroupVersion.WithKind("Pod"))},
	}}
	workerLabels := map[string]string{
		leaderworkerset.SetNameLabelKey:     lws.Name,
		leaderworkerset.WorkerIndexLabelKey: "1",
		leaderworkerset.GroupIndexLabelKey:  "0",
		leaderworkerset.RevisionKey:         revisionKey,
	}
	deletedWorker := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name:            "test-sample-0-1",
		Namespace:       lws.Namespace,
		UID:             "worker-old",
		Labels:          workerLabels,
		OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(workerStatefulSet, appsv1.SchemeGroupVersion.WithKind("StatefulSet"))},
	}}
	replacementWorker := deletedWorker.DeepCopy()
	replacementWorker.UID = "worker-replacement"

	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = appsv1.AddToScheme(scheme)
	_ = leaderworkerset.AddToScheme(scheme)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		lws, leaderPod, workerStatefulSet, replacementWorker,
	).Build()
	reconciler := PodReconciler{Client: fakeClient, Scheme: scheme, Record: fakeEventRecorder{}}

	if _, err := reconciler.reconcilePod(context.Background(), podReconcileRequestForPod(deletedWorker, true)); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	var leader corev1.Pod
	err := fakeClient.Get(context.Background(), client.ObjectKeyFromObject(leaderPod), &leader)
	if !apierrors.IsNotFound(err) {
		t.Fatalf("leader Pod still exists after deleted worker replacement, err = %v", err)
	}
}

func TestReconcileLeaderPodDeletingSkipsHeadlessService(t *testing.T) {
	subdomainPolicy := leaderworkerset.SubdomainUniquePerReplica
	lws := wrappers.BuildLeaderWorkerSet("default").
		Name("test-sample").
		Replica(1).
		Size(2).
		SubdomainPolicy(subdomainPolicy).
		Obj()

	deletionTimestamp := metav1.Now()
	leaderPod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "test-sample-0",
			Namespace:         "default",
			DeletionTimestamp: &deletionTimestamp,
			Finalizers:        []string{"leaderworkerset.sigs.k8s.io/test"},
			Labels: map[string]string{
				leaderworkerset.SetNameLabelKey:     "test-sample",
				leaderworkerset.WorkerIndexLabelKey: "0",
				leaderworkerset.GroupIndexLabelKey:  "0",
			},
		},
	}

	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = appsv1.AddToScheme(scheme)
	_ = leaderworkerset.AddToScheme(scheme)

	client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(lws, &leaderPod).Build()
	reconciler := PodReconciler{
		Client: client,
		Scheme: scheme,
		Record: fakeEventRecorder{},
	}

	req := podReconcileRequest{
		NamespacedName: types.NamespacedName{
			Name:      leaderPod.Name,
			Namespace: leaderPod.Namespace,
		},
	}

	res, err := reconciler.reconcilePod(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error during reconcile: %v", err)
	}
	if res.Requeue {
		t.Errorf("expected no requeue, got %v", res)
	}

	var svcList corev1.ServiceList
	if err := client.List(context.Background(), &svcList); err != nil {
		t.Fatalf("failed to list services: %v", err)
	}
	if len(svcList.Items) != 0 {
		t.Errorf("expected 0 services created for deleting leader pod, got %d", len(svcList.Items))
	}
}

func TestPodReconcilerWaitsForStaleHeadlessService(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := appsv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := leaderworkerset.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	for _, terminating := range []bool{false, true} {
		t.Run(fmt.Sprintf("terminating=%t", terminating), func(t *testing.T) {
			lws := wrappers.BuildBasicLeaderWorkerSet("test-lws", "default").
				Size(2).
				SubdomainPolicy(leaderworkerset.SubdomainUniquePerReplica).
				Obj()
			lws.UID = "current-lws"
			k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(lws).Build()
			revision, err := revisionutils.NewRevision(ctx, k8sClient, lws, "")
			if err != nil {
				t.Fatal(err)
			}
			if err := k8sClient.Create(ctx, revision); err != nil {
				t.Fatal(err)
			}
			leader := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-lws-0", Namespace: lws.Namespace, UID: "current-leader",
					Labels: map[string]string{
						leaderworkerset.SetNameLabelKey:         lws.Name,
						leaderworkerset.WorkerIndexLabelKey:     "0",
						leaderworkerset.GroupIndexLabelKey:      "0",
						leaderworkerset.GroupUniqueHashLabelKey: "current-group",
						leaderworkerset.RevisionKey:             revisionutils.GetRevisionKey(revision),
					},
				},
				Spec: corev1.PodSpec{Hostname: "test-lws-0", Subdomain: "test-lws-0"},
			}
			if err := k8sClient.Create(ctx, leader); err != nil {
				t.Fatal(err)
			}
			previousLeader := leader.DeepCopy()
			previousLeader.UID = "previous-leader"
			service := &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name: leader.Spec.Subdomain, Namespace: lws.Namespace,
					OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(previousLeader, corev1.SchemeGroupVersion.WithKind("Pod"))},
				},
			}
			if terminating {
				service.Finalizers = []string{"leaderworkerset.sigs.k8s.io/test"}
			}
			if err := k8sClient.Create(ctx, service); err != nil {
				t.Fatal(err)
			}
			if terminating {
				if err := k8sClient.Delete(ctx, service); err != nil {
					t.Fatal(err)
				}
			}
			if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(service), service); err != nil {
				t.Fatal(err)
			}
			provider := &stubSchedulerProvider{}
			reconciler := PodReconciler{
				Client: k8sClient, Scheme: scheme, Record: fakeEventRecorder{}, SchedulerProvider: provider,
			}
			req := podReconcileRequestForPod(leader, false)
			for range 2 {
				result, err := reconciler.reconcilePod(ctx, req)
				if err == nil || !result.IsZero() {
					t.Fatalf("expected error-based retry while service is stale, got result %+v, error %v", result, err)
				}
			}
			if provider.calls != 0 {
				t.Fatalf("PodGroup reconciliation proceeded before service was usable: %d calls", provider.calls)
			}
			var workers appsv1.StatefulSet
			if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(leader), &workers); !apierrors.IsNotFound(err) {
				t.Fatalf("expected no worker StatefulSet while service is stale, got error %v", err)
			}
			var actual corev1.Service
			if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(service), &actual); err != nil {
				t.Fatal(err)
			}
			if diff := cmp.Diff(service, &actual); diff != "" {
				t.Fatalf("stale service was modified (-want +got):\n%s", diff)
			}
			// Simulate garbage collection without changing the replacement leader.
			if terminating {
				actual.Finalizers = nil
				if err := k8sClient.Update(ctx, &actual); err != nil {
					t.Fatal(err)
				}
			} else if err := k8sClient.Delete(ctx, &actual); err != nil {
				t.Fatal(err)
			}
			result, err := reconciler.reconcilePod(ctx, req)
			if err != nil || !result.IsZero() {
				t.Fatalf("expected successful retry after service deletion, got result %+v, error %v", result, err)
			}
			if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(service), &actual); err != nil {
				t.Fatal(err)
			}
			if !metav1.IsControlledBy(&actual, leader) || actual.DeletionTimestamp != nil {
				t.Fatalf("replacement service is not usable by the current leader: %+v", actual)
			}
			if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(leader), &workers); err != nil {
				t.Fatal(err)
			}
			if workers.Spec.ServiceName != actual.Name || !metav1.IsControlledBy(&workers, leader) {
				t.Fatalf("workers do not belong to the replacement group: %+v", workers)
			}
			if provider.calls != 1 {
				t.Fatalf("PodGroup reconciliation calls = %d, want 1", provider.calls)
			}
		})
	}
}

func TestConstructWorkerStatefulSetServiceNameHashUniquePerReplica(t *testing.T) {
	client := fake.NewClientBuilder().Build()

	subdomainPolicy := leaderworkerset.SubdomainUniquePerReplica
	lws := wrappers.BuildLeaderWorkerSet("default").
		Name("test-sample").
		Replica(1).
		Size(2).
		SubdomainPolicy(subdomainPolicy).
		Obj()
	lws.Spec.GroupIdentity = leaderworkerset.GroupIdentityHash

	revision, err := revisionutils.NewRevision(context.TODO(), client, lws, "")
	if err != nil {
		t.Fatal(err)
	}

	leaderPod := corev1.Pod{
		ObjectMeta: v1.ObjectMeta{
			Name:      "test-sample-7d9f8b6c4-x2kkp",
			Namespace: "default",
			Labels: map[string]string{
				leaderworkerset.WorkerIndexLabelKey:     "0",
				leaderworkerset.SetNameLabelKey:         "test-sample",
				leaderworkerset.GroupIndexLabelKey:      "test-key",
				leaderworkerset.GroupUniqueHashLabelKey: "test-key",
				leaderworkerset.RevisionKey:             revisionutils.GetRevisionKey(revision),
			},
		},
		Spec: corev1.PodSpec{
			Hostname:  "test-sample-9f2ac71b",
			Subdomain: "test-sample-9f2ac71b",
		},
	}

	cfg, err := constructWorkerStatefulSetApplyConfiguration(leaderPod, *lws)
	if err != nil {
		t.Fatal(err)
	}
	if got := *cfg.Spec.ServiceName; got != "test-sample-9f2ac71b" {
		t.Errorf("expected the worker StatefulSet to use the group key derived service name, got %q", got)
	}
}

func TestReconcileGroupReplacementGate(t *testing.T) {
	lws := wrappers.BuildLeaderWorkerSet("default").Replica(2).Size(2).Obj()
	lws.Spec.GroupIdentity = leaderworkerset.GroupIdentityHash

	base := time.Now()
	leader := func(name string, createdOffset time.Duration, gated, terminating bool) *corev1.Pod {
		p := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:              name,
				Namespace:         lws.Namespace,
				CreationTimestamp: metav1.NewTime(base.Add(createdOffset)),
				Labels: map[string]string{
					leaderworkerset.SetNameLabelKey:     lws.Name,
					leaderworkerset.WorkerIndexLabelKey: "0",
					leaderworkerset.GroupIndexLabelKey:  name,
				},
			},
		}
		if gated {
			p.Spec.SchedulingGates = []corev1.PodSchedulingGate{{Name: leaderworkerset.GroupReplacementSchedulingGate}}
		}
		if terminating {
			now := metav1.Now()
			p.DeletionTimestamp = &now
			p.Finalizers = []string{"foregroundDeletion"}
		}
		return p
	}
	// worker builds a worker pod of the group led by leaderName. A rolling
	// update or scale down deletes the leader in the background, so the leader
	// object can be gone while its workers still hold capacity.
	worker := func(leaderName string, terminating bool) *corev1.Pod {
		p := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:              leaderName + "-1",
				Namespace:         lws.Namespace,
				CreationTimestamp: metav1.NewTime(base.Add(-time.Minute)),
				Labels: map[string]string{
					leaderworkerset.SetNameLabelKey:     lws.Name,
					leaderworkerset.WorkerIndexLabelKey: "1",
					leaderworkerset.GroupIndexLabelKey:  leaderName,
				},
			},
		}
		if terminating {
			now := metav1.Now()
			p.DeletionTimestamp = &now
			p.Finalizers = []string{"test/hold"}
		}
		return p
	}

	tests := []struct {
		name         string
		policy       leaderworkerset.GroupReplacementPolicyType
		pods         []*corev1.Pod
		reconciled   string
		wantAdmitted bool
	}{
		{
			name:         "immediate policy lifts the gate while a group is still terminating",
			policy:       leaderworkerset.GroupReplacementImmediate,
			pods:         []*corev1.Pod{leader("new", 0, true, false), leader("old", -time.Minute, false, true)},
			reconciled:   "new",
			wantAdmitted: true,
		},
		{
			name:         "no terminating groups admits the gated leader",
			policy:       leaderworkerset.GroupReplacementPostTermination,
			pods:         []*corev1.Pod{leader("new", 0, true, false), leader("running", -time.Minute, false, false)},
			reconciled:   "new",
			wantAdmitted: true,
		},
		{
			name:         "a terminating group holds back the only gated leader",
			policy:       leaderworkerset.GroupReplacementPostTermination,
			pods:         []*corev1.Pod{leader("new", 0, true, false), leader("old", -time.Minute, false, true)},
			reconciled:   "new",
			wantAdmitted: false,
		},
		{
			name:         "oldest gated leader takes the free slot",
			policy:       leaderworkerset.GroupReplacementPostTermination,
			pods:         []*corev1.Pod{leader("first", 0, true, false), leader("second", time.Second, true, false), leader("old", -time.Minute, false, true)},
			reconciled:   "first",
			wantAdmitted: true,
		},
		{
			name:         "newest gated leader waits when only one slot is free",
			policy:       leaderworkerset.GroupReplacementPostTermination,
			pods:         []*corev1.Pod{leader("first", 0, true, false), leader("second", time.Second, true, false), leader("old", -time.Minute, false, true)},
			reconciled:   "second",
			wantAdmitted: false,
		},
		{
			name:         "two terminating groups hold back two gated leaders",
			policy:       leaderworkerset.GroupReplacementPostTermination,
			pods:         []*corev1.Pod{leader("first", 0, true, false), leader("second", time.Second, true, false), leader("old-a", -time.Minute, false, true), leader("old-b", -time.Minute, false, true)},
			reconciled:   "first",
			wantAdmitted: false,
		},
		{
			name:         "a leader that is already gone still holds while its worker exists",
			policy:       leaderworkerset.GroupReplacementPostTermination,
			pods:         []*corev1.Pod{leader("new", 0, true, false), worker("old", true)},
			reconciled:   "new",
			wantAdmitted: false,
		},
		{
			name:         "a terminating leader and its worker count as one group",
			policy:       leaderworkerset.GroupReplacementPostTermination,
			pods:         []*corev1.Pod{leader("first", 0, true, false), leader("second", time.Second, true, false), leader("old", -time.Minute, false, true), worker("old", true)},
			reconciled:   "first",
			wantAdmitted: true,
		},
		{
			name:         "a terminating worker under a live leader is not a tearing down group",
			policy:       leaderworkerset.GroupReplacementPostTermination,
			pods:         []*corev1.Pod{leader("new", 0, true, false), leader("running", -time.Minute, false, false), worker("running", true)},
			reconciled:   "new",
			wantAdmitted: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			objs := make([]client.Object, 0, len(tc.pods))
			for _, p := range tc.pods {
				objs = append(objs, p)
			}
			fakeClient := fake.NewClientBuilder().WithObjects(objs...).Build()
			reconciler := PodReconciler{Client: fakeClient, Record: fakeEventRecorder{}}
			testLws := lws.DeepCopy()
			testLws.Spec.GroupReplacementPolicy = tc.policy

			var pod corev1.Pod
			if err := fakeClient.Get(context.Background(), types.NamespacedName{Namespace: lws.Namespace, Name: tc.reconciled}, &pod); err != nil {
				t.Fatalf("getting reconciled pod: %v", err)
			}
			admitted, err := reconciler.reconcileGroupReplacementGate(context.Background(), &pod, testLws)
			if err != nil {
				t.Fatalf("reconcileGroupReplacementGate() error = %v", err)
			}
			if admitted != tc.wantAdmitted {
				t.Fatalf("reconcileGroupReplacementGate() admitted = %t, want %t", admitted, tc.wantAdmitted)
			}
			var stored corev1.Pod
			if err := fakeClient.Get(context.Background(), types.NamespacedName{Namespace: lws.Namespace, Name: tc.reconciled}, &stored); err != nil {
				t.Fatalf("getting stored pod: %v", err)
			}
			if gated := podutils.HasSchedulingGate(&stored, leaderworkerset.GroupReplacementSchedulingGate); gated == tc.wantAdmitted {
				t.Errorf("stored pod gated = %t, want %t", gated, !tc.wantAdmitted)
			}
		})
	}
}

func TestHashGroupRestartBudgetHandoffAndExhaustion(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := appsv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := leaderworkerset.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}

	for _, policy := range []leaderworkerset.GroupReplacementPolicyType{
		leaderworkerset.GroupReplacementPostTermination,
		leaderworkerset.GroupReplacementImmediate,
	} {
		t.Run(string(policy), func(t *testing.T) {
			ctx := context.Background()
			lws := wrappers.BuildLeaderWorkerSet("default").Name("hash-budget").Replica(1).Size(2).
				RestartPolicy(leaderworkerset.RecreateGroupOnPodRestart).MaxGroupRestarts(1).Obj()
			lws.Spec.GroupIdentity = leaderworkerset.GroupIdentityHash
			lws.Spec.GroupReplacementPolicy = policy

			makeLeader := func(name, groupHash string, gated, restarted bool) *corev1.Pod {
				p := wrappers.MakePodWithLabels(lws.Name, groupHash, "0", lws.Namespace, 2)
				p.Name = name
				p.UID = types.UID("uid-" + name)
				p.Labels[leaderworkerset.GroupUniqueHashLabelKey] = groupHash
				p.Labels[leaderworkerset.RevisionKey] = "revision-a"
				p.Annotations[leaderworkerset.GroupIdentityAnnotationKey] = string(leaderworkerset.GroupIdentityHash)
				if gated {
					p.Spec.SchedulingGates = []corev1.PodSchedulingGate{{Name: leaderworkerset.GroupReplacementSchedulingGate}}
				}
				if restarted {
					p.Status.Phase = corev1.PodRunning
					p.Status.ContainerStatuses = []corev1.ContainerStatus{{RestartCount: 1}}
				}
				return p
			}

			leader1 := makeLeader("hash-budget-l1", "hash-1", false, true)
			fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(lws, leader1).Build()
			r := &PodReconciler{Client: fakeClient, Scheme: scheme, Record: fakeEventRecorder{}}

			// 1. First failure consumes budget (0 -> 1) and deletes leader1.
			deleted, _, err := r.handleRestartPolicy(ctx, *leader1, *lws)
			if err != nil {
				t.Fatalf("handleRestartPolicy(leader1) error = %v", err)
			}
			if !deleted {
				t.Fatal("expected leader1 to be deleted for group recreation")
			}

			var updatedLWS leaderworkerset.LeaderWorkerSet
			if err := fakeClient.Get(ctx, client.ObjectKeyFromObject(lws), &updatedLWS); err != nil {
				t.Fatal(err)
			}
			counts, err := parseGroupRestartCounts(updatedLWS.Annotations[leaderworkerset.GroupRestartCountsAnnotationKey])
			if err != nil {
				t.Fatal(err)
			}
			if counts["revision-a/hash-1"] != 1 {
				t.Fatalf("counts = %#v, want revision-a/hash-1 = 1", counts)
			}

			// 2. Replacement leader2 arrives gated and claims the counter (hash-1 -> hash-2) when admitted.
			leader2 := makeLeader("hash-budget-l2", "hash-2", true, false)
			if err := fakeClient.Create(ctx, leader2); err != nil {
				t.Fatal(err)
			}
			admitted, err := r.reconcileGroupReplacementGate(ctx, leader2, &updatedLWS)
			if err != nil {
				t.Fatalf("reconcileGroupReplacementGate(leader2) error = %v", err)
			}
			if !admitted {
				t.Fatal("expected leader2 to be admitted")
			}

			if err := fakeClient.Get(ctx, client.ObjectKeyFromObject(lws), &updatedLWS); err != nil {
				t.Fatal(err)
			}
			counts, err = parseGroupRestartCounts(updatedLWS.Annotations[leaderworkerset.GroupRestartCountsAnnotationKey])
			if err != nil {
				t.Fatal(err)
			}
			if counts["revision-a/hash-2"] != 1 {
				t.Fatalf("counts after handoff = %#v, want revision-a/hash-2 = 1", counts)
			}
			if _, found := counts["revision-a/hash-1"]; found {
				t.Fatalf("stale counter key revision-a/hash-1 was not removed: %#v", counts)
			}

			// 3. Second failure on leader2 exhausts the budget (count 1 >= limit 1).
			leader2.Status.Phase = corev1.PodRunning
			leader2.Status.ContainerStatuses = []corev1.ContainerStatus{{RestartCount: 1}}
			if err := fakeClient.Status().Update(ctx, leader2); err != nil {
				t.Fatal(err)
			}
			if _, _, err := r.handleRestartPolicy(ctx, *leader2, updatedLWS); err != nil {
				t.Fatalf("handleRestartPolicy(leader2) error = %v", err)
			}

			var exhaustedLeader corev1.Pod
			if err := fakeClient.Get(ctx, client.ObjectKeyFromObject(leader2), &exhaustedLeader); err != nil {
				t.Fatalf("expected exhausted leader2 to be retained by finalizer: %v", err)
			}
			if exhaustedLeader.Annotations[leaderworkerset.GroupRestartBudgetExhaustedAnnotationKey] != "true" {
				t.Fatal("expected leader2 to be marked group-restart-budget-exhausted=true")
			}
			if !controllerutil.ContainsFinalizer(&exhaustedLeader, leaderworkerset.GroupRestartBudgetCleanupFinalizer) {
				t.Fatal("expected leader2 to carry group-restart-budget-cleanup finalizer")
			}

			// 4. ReplicaSet creates gated replacement leader3; it must be held back under both PostTermination and Immediate.
			leader3 := makeLeader("hash-budget-l3", "hash-3", true, false)
			if err := fakeClient.Create(ctx, leader3); err != nil {
				t.Fatal(err)
			}
			admitted, err = r.reconcileGroupReplacementGate(ctx, leader3, &updatedLWS)
			if err != nil {
				t.Fatalf("reconcileGroupReplacementGate(leader3) error = %v", err)
			}
			if admitted {
				t.Fatalf("expected leader3 to remain gated while leader2 is budget-exhausted under policy %s", policy)
			}

			// 5. Operator sets recover=true on retained leader2; counter is cleared, finalizer removed, and leader3 is admitted with count 0.
			exhaustedLeader.Annotations[leaderworkerset.GroupRestartBudgetRecoverAnnotationKey] = "true"
			if err := fakeClient.Update(ctx, &exhaustedLeader); err != nil {
				t.Fatal(err)
			}
			if _, err := r.reconcilePod(ctx, podReconcileRequestForPod(&exhaustedLeader, false)); err != nil {
				t.Fatalf("reconcilePod(recover leader2) error = %v", err)
			}
			if err := fakeClient.Get(ctx, client.ObjectKeyFromObject(lws), &updatedLWS); err != nil {
				t.Fatal(err)
			}
			counts, err = parseGroupRestartCounts(updatedLWS.Annotations[leaderworkerset.GroupRestartCountsAnnotationKey])
			if err != nil {
				t.Fatal(err)
			}
			if len(counts) != 0 {
				t.Fatalf("expected counts to be cleared after recover=true, got %#v", counts)
			}

			if err := fakeClient.Get(ctx, client.ObjectKeyFromObject(leader3), leader3); err != nil {
				t.Fatal(err)
			}
			admitted, err = r.reconcileGroupReplacementGate(ctx, leader3, &updatedLWS)
			if err != nil {
				t.Fatalf("reconcileGroupReplacementGate(leader3 after recovery) error = %v", err)
			}
			if !admitted {
				t.Fatal("expected leader3 to be admitted after leader2 recovery")
			}
		})
	}
}

func TestHashGroupLifecycleTeardownRequested(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := appsv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := leaderworkerset.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	lws := wrappers.BuildLeaderWorkerSet("default").Name("hash-teardown").Replica(1).Size(2).
		RestartPolicy(leaderworkerset.RecreateGroupOnPodRestart).MaxGroupRestarts(1).Obj()
	lws.Spec.GroupIdentity = leaderworkerset.GroupIdentityHash
	lws.Annotations = map[string]string{
		leaderworkerset.GroupRestartCountsAnnotationKey: `{"revision-a/hash-exhausted":1}`,
	}

	activeLeader := wrappers.MakePodWithLabels(lws.Name, "hash-active", "0", lws.Namespace, 2)
	activeLeader.Name = "hash-teardown-active"
	activeLeader.Labels[leaderworkerset.RevisionKey] = "revision-a"

	exhaustedLeader := wrappers.MakePodWithLabels(lws.Name, "hash-exhausted", "0", lws.Namespace, 2)
	exhaustedLeader.Name = "hash-teardown-exhausted"
	exhaustedLeader.Labels[leaderworkerset.RevisionKey] = "revision-a"
	exhaustedLeader.Annotations[leaderworkerset.GroupRestartBudgetExhaustedAnnotationKey] = "true"

	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      lws.Name,
			Namespace: lws.Namespace,
			Labels:    map[string]string{leaderworkerset.RevisionKey: "revision-a"},
		},
		Spec: appsv1.DeploymentSpec{Replicas: ptr.To[int32](1)},
	}

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(lws, deploy, activeLeader, exhaustedLeader).Build()
	r := &PodReconciler{Client: fakeClient}

	// Because replicas=1 and activeLeader already occupies the 1 slot, exhaustedLeader is scaled down.
	teardown, err := r.groupLifecycleTeardownRequested(ctx, lws, exhaustedLeader)
	if err != nil {
		t.Fatalf("groupLifecycleTeardownRequested() error = %v", err)
	}
	if !teardown {
		t.Fatal("expected scaled-down hash exhausted group to request teardown")
	}

	var updatedLWS leaderworkerset.LeaderWorkerSet
	if err := fakeClient.Get(ctx, client.ObjectKeyFromObject(lws), &updatedLWS); err != nil {
		t.Fatal(err)
	}
	if got := updatedLWS.Annotations[leaderworkerset.GroupRestartCountsAnnotationKey]; got != "" {
		t.Fatalf("expected scaled-down hash group restart count to be cleared, got %q", got)
	}

	// Verify scale-down teardown also succeeds when the surviving active group is still gated
	// alongside the exhausted group's gated replacement (preventing circular deadlock).
	lwsGated := lws.DeepCopy()
	lwsGated.Annotations = map[string]string{
		leaderworkerset.GroupRestartCountsAnnotationKey: `{"revision-a/hash-exhausted":1}`,
	}
	gatedReplacementForExhausted := wrappers.MakePodWithLabels(lws.Name, "hash-gated-1", "0", lws.Namespace, 2)
	gatedReplacementForExhausted.Name = "hash-teardown-gated-1"
	gatedReplacementForExhausted.Labels[leaderworkerset.RevisionKey] = "revision-a"
	gatedReplacementForExhausted.Spec.SchedulingGates = []corev1.PodSchedulingGate{{Name: leaderworkerset.GroupReplacementSchedulingGate}}

	gatedActiveLeader := wrappers.MakePodWithLabels(lws.Name, "hash-gated-2", "0", lws.Namespace, 2)
	gatedActiveLeader.Name = "hash-teardown-gated-2"
	gatedActiveLeader.Labels[leaderworkerset.RevisionKey] = "revision-a"
	gatedActiveLeader.Spec.SchedulingGates = []corev1.PodSchedulingGate{{Name: leaderworkerset.GroupReplacementSchedulingGate}}

	fakeClientGated := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		lwsGated, deploy, exhaustedLeader, gatedReplacementForExhausted, gatedActiveLeader,
	).Build()
	rGated := &PodReconciler{Client: fakeClientGated}
	teardown, err = rGated.groupLifecycleTeardownRequested(ctx, lwsGated, exhaustedLeader)
	if err != nil {
		t.Fatalf("groupLifecycleTeardownRequested(gated active) error = %v", err)
	}
	if !teardown {
		t.Fatal("expected scaled-down hash exhausted group to request teardown when surviving active group is gated")
	}
}

func TestReconcileGroupReplacementGateRejectsOutdatedRevisionAndExcessReplicas(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := appsv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := leaderworkerset.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	lws := wrappers.BuildLeaderWorkerSet("default").Name("hash-gate-rollout").Replica(1).Size(2).
		RestartPolicy(leaderworkerset.RecreateGroupOnPodRestart).MaxGroupRestarts(1).Obj()
	lws.Spec.GroupIdentity = leaderworkerset.GroupIdentityHash

	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      lws.Name,
			Namespace: lws.Namespace,
			Labels:    map[string]string{leaderworkerset.RevisionKey: "revision-b"},
		},
		Spec: appsv1.DeploymentSpec{Replicas: ptr.To[int32](1)},
	}

	now := metav1.Now()
	oldGatedLeader := wrappers.MakePodWithLabels(lws.Name, "hash-old", "0", lws.Namespace, 2)
	oldGatedLeader.Name = "hash-gate-rollout-old"
	oldGatedLeader.CreationTimestamp = metav1.NewTime(now.Add(-10 * time.Second))
	oldGatedLeader.Labels[leaderworkerset.RevisionKey] = "revision-a"
	oldGatedLeader.Spec.SchedulingGates = []corev1.PodSchedulingGate{{Name: leaderworkerset.GroupReplacementSchedulingGate}}

	newGatedLeader := wrappers.MakePodWithLabels(lws.Name, "hash-new", "0", lws.Namespace, 2)
	newGatedLeader.Name = "hash-gate-rollout-new"
	newGatedLeader.CreationTimestamp = now
	newGatedLeader.Labels[leaderworkerset.RevisionKey] = "revision-b"
	newGatedLeader.Spec.SchedulingGates = []corev1.PodSchedulingGate{{Name: leaderworkerset.GroupReplacementSchedulingGate}}

	excessGatedLeader := wrappers.MakePodWithLabels(lws.Name, "hash-excess", "0", lws.Namespace, 2)
	excessGatedLeader.Name = "hash-gate-rollout-excess"
	excessGatedLeader.CreationTimestamp = metav1.NewTime(now.Add(10 * time.Second))
	excessGatedLeader.Labels[leaderworkerset.RevisionKey] = "revision-b"
	excessGatedLeader.Spec.SchedulingGates = []corev1.PodSchedulingGate{{Name: leaderworkerset.GroupReplacementSchedulingGate}}

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(lws, deploy, oldGatedLeader, newGatedLeader, excessGatedLeader).Build()
	r := &PodReconciler{Client: fakeClient, Record: fakeEventRecorder{}}

	// Outdated-revision gated leader must remain gated after rollout advances deploy to revision-b.
	admitted, err := r.reconcileGroupReplacementGate(ctx, oldGatedLeader, lws)
	if err != nil {
		t.Fatalf("reconcileGroupReplacementGate(oldGatedLeader) error = %v", err)
	}
	if admitted {
		t.Fatal("expected outdated-revision gated leader to remain gated")
	}

	// Current-revision gated leader filling the 1 desired slot should be admitted.
	admitted, err = r.reconcileGroupReplacementGate(ctx, newGatedLeader, lws)
	if err != nil {
		t.Fatalf("reconcileGroupReplacementGate(newGatedLeader) error = %v", err)
	}
	if !admitted {
		t.Fatal("expected current-revision gated leader to be admitted")
	}

	// Once newGatedLeader is admitted, excessGatedLeader exceeds lws.Spec.Replicas=1 and must remain gated.
	admitted, err = r.reconcileGroupReplacementGate(ctx, excessGatedLeader, lws)
	if err != nil {
		t.Fatalf("reconcileGroupReplacementGate(excessGatedLeader) error = %v", err)
	}
	if admitted {
		t.Fatal("expected excess gated leader beyond Spec.Replicas to remain gated")
	}
}

func TestBudgetFinalizedPodRequests(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme(corev1) error = %v", err)
	}
	if err := leaderworkerset.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme(leaderworkerset) error = %v", err)
	}

	lws := wrappers.BuildLeaderWorkerSet("default").Name("test-lws").Replica(1).Size(2).Obj()
	finalizedLeader := wrappers.MakePodWithLabels(lws.Name, "hash-a", "0", lws.Namespace, 2)
	finalizedLeader.Name = "finalized-leader"
	finalizedLeader.Finalizers = []string{leaderworkerset.GroupRestartBudgetCleanupFinalizer}

	finalizedWorker := wrappers.MakePodWithLabels(lws.Name, "hash-a", "1", lws.Namespace, 2)
	finalizedWorker.Name = "finalized-worker"
	finalizedWorker.Finalizers = []string{leaderworkerset.GroupRestartBudgetCleanupFinalizer}

	gatedLeader := wrappers.MakePodWithLabels(lws.Name, "hash-b", "0", lws.Namespace, 2)
	gatedLeader.Name = "gated-leader"
	gatedLeader.Spec.SchedulingGates = []corev1.PodSchedulingGate{{Name: leaderworkerset.GroupReplacementSchedulingGate}}

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(lws, finalizedLeader, finalizedWorker, gatedLeader).Build()
	r := &PodReconciler{Client: fakeClient}

	// Non-deleting LWS update should enqueue finalized leader and gated leader, but NOT finalized worker.
	reqs := r.budgetFinalizedPodRequests(ctx, lws)
	if len(reqs) != 2 {
		t.Fatalf("budgetFinalizedPodRequests(non-deleting) returned %d requests, want 2: %v", len(reqs), reqs)
	}

	// Deleting LWS update should also include finalized worker pods for finalizer cleanup.
	deletingLws := lws.DeepCopy()
	now := metav1.Now()
	deletingLws.DeletionTimestamp = &now
	reqsDeleting := r.budgetFinalizedPodRequests(ctx, deletingLws)
	if len(reqsDeleting) != 3 {
		t.Fatalf("budgetFinalizedPodRequests(deleting) returned %d requests, want 3: %v", len(reqsDeleting), reqsDeleting)
	}
}

func TestComputeBackoffDelay(t *testing.T) {
	defaultBackoff := &leaderworkerset.RestartBackoff{}
	tests := []struct {
		name    string
		backoff *leaderworkerset.RestartBackoff
		count   int32
		want    time.Duration
	}{
		{
			name:    "default backoff count 0 returns base 10s",
			backoff: defaultBackoff,
			count:   0,
			want:    10 * time.Second,
		},
		{
			name:    "default backoff count 1 returns 20s",
			backoff: defaultBackoff,
			count:   1,
			want:    20 * time.Second,
		},
		{
			name:    "default backoff count 2 returns 40s",
			backoff: defaultBackoff,
			count:   2,
			want:    40 * time.Second,
		},
		{
			name:    "default backoff count 3 returns 80s",
			backoff: defaultBackoff,
			count:   3,
			want:    80 * time.Second,
		},
		{
			name:    "default backoff count 4 returns 160s",
			backoff: defaultBackoff,
			count:   4,
			want:    160 * time.Second,
		},
		{
			name:    "default backoff count 5 caps at 5m (300s)",
			backoff: defaultBackoff,
			count:   5,
			want:    5 * time.Minute,
		},
		{
			name:    "default backoff count 35 overflow protection caps at 5m",
			backoff: defaultBackoff,
			count:   35,
			want:    5 * time.Minute,
		},
		{
			name:    "negative count is treated as 0",
			backoff: defaultBackoff,
			count:   -5,
			want:    10 * time.Second,
		},
		{
			name: "custom backoff base 2s, cap 10s, count 0",
			backoff: &leaderworkerset.RestartBackoff{
				BaseSeconds: ptr.To(int32(2)),
				CapSeconds:  ptr.To(int32(10)),
			},
			count: 0,
			want:  2 * time.Second,
		},
		{
			name: "custom backoff base 2s, cap 10s, count 2",
			backoff: &leaderworkerset.RestartBackoff{
				BaseSeconds: ptr.To(int32(2)),
				CapSeconds:  ptr.To(int32(10)),
			},
			count: 2,
			want:  8 * time.Second,
		},
		{
			name: "custom backoff base 2s, cap 10s, count 3 caps at 10s",
			backoff: &leaderworkerset.RestartBackoff{
				BaseSeconds: ptr.To(int32(2)),
				CapSeconds:  ptr.To(int32(10)),
			},
			count: 3,
			want:  10 * time.Second,
		},
		{
			name: "maxBackoffExponent boundary: count 30 computes 2^30 with large cap",
			backoff: &leaderworkerset.RestartBackoff{
				BaseSeconds: ptr.To(int32(1)),
				CapSeconds:  ptr.To(int32(2147483647)),
			},
			count: 30,
			want:  (1 << 30) * time.Second,
		},
		{
			name: "maxBackoffExponent boundary: count 31 caps at capSeconds",
			backoff: &leaderworkerset.RestartBackoff{
				BaseSeconds: ptr.To(int32(1)),
				CapSeconds:  ptr.To(int32(2147483647)),
			},
			count: 31,
			want:  2147483647 * time.Second,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := computeBackoffDelay(tc.backoff, tc.count)
			if got != tc.want {
				t.Errorf("computeBackoffDelay() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestHandleRestartPolicyRestartBackoff(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = appsv1.AddToScheme(scheme)
	_ = leaderworkerset.AddToScheme(scheme)

	t.Run("within backoff window: delays recreation and returns RequeueAfter", func(t *testing.T) {
		lws := wrappers.BuildLeaderWorkerSet("default").
			RestartPolicy(leaderworkerset.RecreateGroupOnPodRestart).
			RestartBackoff(ptr.To(int32(10)), ptr.To(int32(300))).
			Obj()

		now := time.Now()
		leader := wrappers.MakePodWithLabels(lws.Name, "0", "0", lws.Namespace, 1)
		leader.CreationTimestamp = metav1.NewTime(now.Add(-2 * time.Second))
		leader.Labels[leaderworkerset.RevisionKey] = "revision-a"
		leader.Status.Phase = corev1.PodRunning
		leader.Status.ContainerStatuses = []corev1.ContainerStatus{{RestartCount: 1}}

		fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(lws, leader).Build()
		recorder := events.NewFakeRecorder(10)
		r := &PodReconciler{Client: fakeClient, Record: recorder}

		deleted, res, err := r.handleRestartPolicy(context.Background(), *leader, *lws.DeepCopy())
		if err != nil {
			t.Fatalf("handleRestartPolicy() error = %v", err)
		}
		if deleted {
			t.Fatal("expected leader NOT to be deleted within backoff window")
		}
		// fake.Client serializes metav1.Time with RFC3339 whole-second precision, which can truncate up to ~1s off CreationTimestamp.
		if res.RequeueAfter < 5*time.Second || res.RequeueAfter > 9*time.Second {
			t.Fatalf("expected RequeueAfter ~8s, got %v", res.RequeueAfter)
		}

		var currentLeader corev1.Pod
		if err := fakeClient.Get(context.Background(), client.ObjectKeyFromObject(leader), &currentLeader); err != nil {
			t.Fatalf("failed to get leader: %v", err)
		}
		if currentLeader.Annotations[leaderworkerset.GroupRecreatePendingAnnotationKey] == "" {
			t.Fatal("expected leader to have GroupRecreatePendingAnnotationKey set")
		}

		select {
		case ev := <-recorder.Events:
			if !strings.Contains(ev, "GroupRestartBackoff") {
				t.Fatalf("expected GroupRestartBackoff event, got %q", ev)
			}
		default:
			t.Fatal("expected GroupRestartBackoff event to be emitted")
		}

		var updatedLWS leaderworkerset.LeaderWorkerSet
		if err := fakeClient.Get(context.Background(), client.ObjectKeyFromObject(lws), &updatedLWS); err != nil {
			t.Fatal(err)
		}
		if updatedLWS.Annotations[leaderworkerset.GroupRestartCountsAnnotationKey] != "" {
			t.Fatalf("expected empty restart counts while waiting, got %q", updatedLWS.Annotations[leaderworkerset.GroupRestartCountsAnnotationKey])
		}
	})

	t.Run("after backoff window: deletes leader and increments restart count", func(t *testing.T) {
		lws := wrappers.BuildLeaderWorkerSet("default").
			RestartPolicy(leaderworkerset.RecreateGroupOnPodRestart).
			RestartBackoff(ptr.To(int32(10)), ptr.To(int32(300))).
			Obj()

		now := time.Now()
		leader := wrappers.MakePodWithLabels(lws.Name, "0", "0", lws.Namespace, 1)
		leader.CreationTimestamp = metav1.NewTime(now.Add(-12 * time.Second))
		leader.Labels[leaderworkerset.RevisionKey] = "revision-a"
		leader.Status.Phase = corev1.PodRunning
		leader.Status.ContainerStatuses = []corev1.ContainerStatus{{RestartCount: 1}}

		fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(lws, leader).Build()
		r := &PodReconciler{Client: fakeClient, Record: fakeEventRecorder{}}

		deleted, res, err := r.handleRestartPolicy(context.Background(), *leader, *lws.DeepCopy())
		if err != nil {
			t.Fatalf("handleRestartPolicy() error = %v", err)
		}
		if !deleted {
			t.Fatal("expected leader to be deleted after backoff window")
		}
		if res.RequeueAfter != 0 {
			t.Fatalf("expected RequeueAfter == 0, got %v", res.RequeueAfter)
		}

		var updatedLWS leaderworkerset.LeaderWorkerSet
		if err := fakeClient.Get(context.Background(), client.ObjectKeyFromObject(lws), &updatedLWS); err != nil {
			t.Fatal(err)
		}
		counts, err := parseGroupRestartCounts(updatedLWS.Annotations[leaderworkerset.GroupRestartCountsAnnotationKey])
		if err != nil {
			t.Fatal(err)
		}
		if counts["revision-a/0"] != 1 {
			t.Fatalf("expected restart count 1, got %d", counts["revision-a/0"])
		}
	})

	t.Run("exponential backoff for count 1", func(t *testing.T) {
		lws := wrappers.BuildLeaderWorkerSet("default").
			RestartPolicy(leaderworkerset.RecreateGroupOnPodRestart).
			RestartBackoff(ptr.To(int32(10)), ptr.To(int32(300))).
			Obj()
		lws.Annotations = map[string]string{
			leaderworkerset.GroupRestartCountsAnnotationKey: `{"revision-a/0":1}`,
		}

		now := time.Now()
		leader := wrappers.MakePodWithLabels(lws.Name, "0", "0", lws.Namespace, 1)
		leader.CreationTimestamp = metav1.NewTime(now.Add(-15 * time.Second))
		leader.Labels[leaderworkerset.RevisionKey] = "revision-a"
		leader.Status.Phase = corev1.PodRunning
		leader.Status.ContainerStatuses = []corev1.ContainerStatus{{RestartCount: 1}}

		fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(lws, leader).Build()
		r := &PodReconciler{Client: fakeClient, Record: fakeEventRecorder{}}

		deleted, res, err := r.handleRestartPolicy(context.Background(), *leader, *lws.DeepCopy())
		if err != nil {
			t.Fatalf("handleRestartPolicy() error = %v", err)
		}
		if deleted {
			t.Fatal("expected leader NOT to be deleted within count=1 backoff window")
		}
		if res.RequeueAfter < 3*time.Second || res.RequeueAfter > 6*time.Second {
			t.Fatalf("expected RequeueAfter ~5s, got %v", res.RequeueAfter)
		}
	})

	t.Run("worker pod failure triggers backoff based on leader creation timestamp", func(t *testing.T) {
		lws := wrappers.BuildLeaderWorkerSet("default").
			RestartPolicy(leaderworkerset.RecreateGroupOnPodRestart).
			RestartBackoff(ptr.To(int32(10)), ptr.To(int32(300))).
			Size(2).
			Obj()

		now := time.Now()
		leader := wrappers.MakePodWithLabels(lws.Name, "0", "0", lws.Namespace, 2)
		leader.UID = "leader-uid"
		leader.CreationTimestamp = metav1.NewTime(now.Add(-3 * time.Second))
		leader.Labels[leaderworkerset.RevisionKey] = "revision-a"

		worker := wrappers.MakePodWithLabels(lws.Name, "0", "1", lws.Namespace, 2)
		worker.Labels[leaderworkerset.RevisionKey] = "revision-a"
		worker.Status.Phase = corev1.PodRunning
		worker.Status.ContainerStatuses = []corev1.ContainerStatus{{RestartCount: 1}}
		worker.OwnerReferences = []metav1.OwnerReference{*metav1.NewControllerRef(leader, corev1.SchemeGroupVersion.WithKind("Pod"))}

		fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(lws, leader, worker).Build()
		r := &PodReconciler{Client: fakeClient, Scheme: scheme, Record: fakeEventRecorder{}}

		deleted, res, err := r.handleRestartPolicy(context.Background(), *worker, *lws.DeepCopy())
		if err != nil {
			t.Fatalf("handleRestartPolicy() error = %v", err)
		}
		if deleted {
			t.Fatal("expected leader NOT to be deleted yet")
		}
		if res.RequeueAfter < 4*time.Second || res.RequeueAfter > 8*time.Second {
			t.Fatalf("expected RequeueAfter ~7s, got %v", res.RequeueAfter)
		}
	})

	t.Run("coexistence with MaxGroupRestarts: terminates immediately when exhausted", func(t *testing.T) {
		lws := wrappers.BuildLeaderWorkerSet("default").
			RestartPolicy(leaderworkerset.RecreateGroupOnPodRestart).
			MaxGroupRestarts(1).
			RestartBackoff(ptr.To(int32(10)), ptr.To(int32(300))).
			Obj()
		lws.Annotations = map[string]string{
			leaderworkerset.GroupRestartCountsAnnotationKey: `{"revision-a/0":1}`,
		}

		now := time.Now()
		leader := wrappers.MakePodWithLabels(lws.Name, "0", "0", lws.Namespace, 1)
		leader.CreationTimestamp = metav1.NewTime(now.Add(-1 * time.Second))
		leader.Labels[leaderworkerset.RevisionKey] = "revision-a"
		leader.Status.Phase = corev1.PodRunning
		leader.Status.ContainerStatuses = []corev1.ContainerStatus{{RestartCount: 1}}

		fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(lws, leader).Build()
		r := &PodReconciler{Client: fakeClient, Record: fakeEventRecorder{}}

		deleted, res, err := r.handleRestartPolicy(context.Background(), *leader, *lws.DeepCopy())
		if err != nil {
			t.Fatalf("handleRestartPolicy() error = %v", err)
		}
		if !deleted {
			t.Fatal("expected leader to be terminated immediately upon budget exhaustion")
		}
		if res.RequeueAfter != 0 {
			t.Fatalf("expected RequeueAfter == 0 for exhausted termination, got %v", res.RequeueAfter)
		}

		var currentLeader corev1.Pod
		if err := fakeClient.Get(context.Background(), client.ObjectKeyFromObject(leader), &currentLeader); err != nil {
			t.Fatal(err)
		}
		if currentLeader.Annotations[leaderworkerset.GroupRestartBudgetExhaustedAnnotationKey] != "true" {
			t.Fatal("expected leader to be marked exhausted")
		}
	})

	t.Run("RecreateGroupAfterStart with restartBackoff: pending replacement worker at requeue time does not drop recreation", func(t *testing.T) {
		lws := wrappers.BuildLeaderWorkerSet("default").
			RestartPolicy(leaderworkerset.RecreateGroupAfterStart).
			RestartBackoff(ptr.To(int32(10)), ptr.To(int32(300))).
			Size(2).
			Obj()

		now := time.Now()
		leader := wrappers.MakePodWithLabels(lws.Name, "0", "0", lws.Namespace, 2)
		leader.UID = "leader-uid"
		leader.CreationTimestamp = metav1.NewTime(now.Add(-2 * time.Second))
		leader.Labels[leaderworkerset.RevisionKey] = "revision-a"
		leader.Annotations = map[string]string{leaderworkerset.SizeAnnotationKey: "2"}

		workerSts := &appsv1.StatefulSet{
			ObjectMeta: metav1.ObjectMeta{
				Name: leader.Name, Namespace: leader.Namespace, UID: "worker-sts-uid",
				OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(leader, corev1.SchemeGroupVersion.WithKind("Pod"))},
			},
		}

		worker := wrappers.MakePodWithLabels(lws.Name, "0", "1", lws.Namespace, 2)
		worker.Labels[leaderworkerset.RevisionKey] = "revision-a"
		worker.Annotations = map[string]string{
			leaderworkerset.SizeAnnotationKey:          "2",
			leaderworkerset.LeaderPodNameAnnotationKey: leader.Name,
		}
		worker.Status.Phase = corev1.PodRunning
		worker.OwnerReferences = []metav1.OwnerReference{*metav1.NewControllerRef(workerSts, appsv1.SchemeGroupVersion.WithKind("StatefulSet"))}

		fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(lws, leader, workerSts, worker).Build()
		r := &PodReconciler{Client: fakeClient, Scheme: scheme, Record: fakeEventRecorder{}}

		// 1. Worker fails (evicted/deleted). Group was fully running, so it passes the AfterStart guard.
		deletedWorker := worker.DeepCopy()
		deletionTimestamp := metav1.Now()
		deletedWorker.DeletionTimestamp = &deletionTimestamp
		deleted, res, err := r.handleRestartPolicy(context.Background(), *deletedWorker, *lws.DeepCopy())
		if err != nil {
			t.Fatalf("handleRestartPolicy() error = %v", err)
		}
		if deleted {
			t.Fatal("expected leader NOT to be deleted yet within backoff window")
		}
		if res.RequeueAfter == 0 {
			t.Fatal("expected RequeueAfter > 0")
		}

		// Verify decision annotation is recorded on leader
		var leaderWithAnnotation corev1.Pod
		if err := fakeClient.Get(context.Background(), client.ObjectKeyFromObject(leader), &leaderWithAnnotation); err != nil {
			t.Fatal(err)
		}
		if leaderWithAnnotation.Annotations[leaderworkerset.GroupRecreatePendingAnnotationKey] == "" {
			t.Fatal("expected GroupRecreatePendingAnnotationKey set on leader")
		}

		// 2. StatefulSet replaces the worker. The replacement worker has same name, is Pending, no deletion timestamp, no restarts.
		replacementWorker := wrappers.MakePodWithLabels(lws.Name, "0", "1", lws.Namespace, 2)
		replacementWorker.Labels[leaderworkerset.RevisionKey] = "revision-a"
		replacementWorker.Annotations = map[string]string{
			leaderworkerset.SizeAnnotationKey:          "2",
			leaderworkerset.LeaderPodNameAnnotationKey: leader.Name,
		}
		replacementWorker.Status.Phase = corev1.PodPending
		replacementWorker.OwnerReferences = []metav1.OwnerReference{*metav1.NewControllerRef(workerSts, appsv1.SchemeGroupVersion.WithKind("StatefulSet"))}
		if err := fakeClient.Update(context.Background(), replacementWorker); err != nil {
			t.Fatal(err)
		}

		// 3. Requeue time arrives (simulate leader age >= backoff delay).
		leaderWithAnnotation.CreationTimestamp = metav1.NewTime(now.Add(-12 * time.Second))
		if err := fakeClient.Update(context.Background(), &leaderWithAnnotation); err != nil {
			t.Fatal(err)
		}

		// Reconcile replacement worker at requeue time.
		deleted, res, err = r.handleRestartPolicy(context.Background(), *replacementWorker, *lws.DeepCopy())
		if err != nil {
			t.Fatalf("handleRestartPolicy() at requeue error = %v", err)
		}
		if !deleted {
			t.Fatal("expected leader to be deleted after backoff delay despite replacement worker being Pending")
		}
		if res.RequeueAfter != 0 {
			t.Fatalf("expected RequeueAfter == 0, got %v", res.RequeueAfter)
		}

		// Verify leader pod is deleted
		var currentLeader corev1.Pod
		if err := fakeClient.Get(context.Background(), client.ObjectKeyFromObject(leader), &currentLeader); !apierrors.IsNotFound(err) {
			t.Fatalf("expected leader to be deleted, got err = %v", err)
		}

		// Verify restart count was incremented
		var updatedLWS leaderworkerset.LeaderWorkerSet
		if err := fakeClient.Get(context.Background(), client.ObjectKeyFromObject(lws), &updatedLWS); err != nil {
			t.Fatal(err)
		}
		counts, err := parseGroupRestartCounts(updatedLWS.Annotations[leaderworkerset.GroupRestartCountsAnnotationKey])
		if err != nil {
			t.Fatal(err)
		}
		if counts["revision-a/0"] != 1 {
			t.Fatalf("expected restart count 1, got %d", counts["revision-a/0"])
		}
	})

	t.Run("leader container restart does not skip worker statefulset creation during backoff", func(t *testing.T) {
		lws := wrappers.BuildLeaderWorkerSet("default").
			RestartPolicy(leaderworkerset.RecreateGroupOnPodRestart).
			RestartBackoff(ptr.To(int32(10)), ptr.To(int32(300))).
			Size(2).
			Obj()

		now := time.Now()
		leader := wrappers.MakePodWithLabels(lws.Name, "0", "0", lws.Namespace, 2)
		leader.CreationTimestamp = metav1.NewTime(now.Add(-2 * time.Second))
		leader.Status.Phase = corev1.PodRunning
		leader.Status.ContainerStatuses = []corev1.ContainerStatus{{RestartCount: 1}}
		leader.Spec.Hostname = leader.Name
		leader.Spec.Subdomain = lws.Name

		tempClient := fake.NewClientBuilder().WithScheme(scheme).Build()
		revision, err := revisionutils.NewRevision(context.Background(), tempClient, lws, "")
		if err != nil {
			t.Fatal(err)
		}
		leader.Labels[leaderworkerset.RevisionKey] = revisionutils.GetRevisionKey(revision)

		fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(lws, revision, leader).Build()
		r := &PodReconciler{Client: fakeClient, Scheme: scheme, Record: fakeEventRecorder{}}

		res, err := r.reconcilePod(context.Background(), podReconcileRequestForPod(leader, false))
		if err != nil {
			t.Fatalf("reconcilePod() error = %v", err)
		}
		if res.RequeueAfter < 5*time.Second || res.RequeueAfter > 9*time.Second {
			t.Fatalf("expected RequeueAfter ~8s, got %v", res.RequeueAfter)
		}

		var workerSts appsv1.StatefulSet
		if err := fakeClient.Get(context.Background(), client.ObjectKeyFromObject(leader), &workerSts); err != nil {
			t.Fatalf("expected worker StatefulSet to be created during backoff, got %v", err)
		}
	})

	t.Run("size == 1: leader container restart propagates backoff and does not drop on watch update", func(t *testing.T) {
		lws := wrappers.BuildLeaderWorkerSet("default").
			RestartPolicy(leaderworkerset.RecreateGroupOnPodRestart).
			RestartBackoff(ptr.To(int32(10)), ptr.To(int32(300))).
			Size(1).
			Obj()

		now := time.Now()
		leader := wrappers.MakePodWithLabels(lws.Name, "0", "0", lws.Namespace, 1)
		leader.CreationTimestamp = metav1.NewTime(now.Add(-2 * time.Second))
		leader.Status.Phase = corev1.PodRunning
		leader.Status.ContainerStatuses = []corev1.ContainerStatus{{RestartCount: 1}}
		leader.Spec.Hostname = leader.Name
		leader.Spec.Subdomain = lws.Name

		tempClient := fake.NewClientBuilder().WithScheme(scheme).Build()
		revision, err := revisionutils.NewRevision(context.Background(), tempClient, lws, "")
		if err != nil {
			t.Fatal(err)
		}
		leader.Labels[leaderworkerset.RevisionKey] = revisionutils.GetRevisionKey(revision)

		fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(lws, revision, leader).Build()
		r := &PodReconciler{Client: fakeClient, Scheme: scheme, Record: fakeEventRecorder{}}

		// 1. Initial reconcile on leader container restart
		res, err := r.reconcilePod(context.Background(), podReconcileRequestForPod(leader, false))
		if err != nil {
			t.Fatalf("reconcilePod() error = %v", err)
		}
		if res.RequeueAfter < 5*time.Second || res.RequeueAfter > 9*time.Second {
			t.Fatalf("expected RequeueAfter ~8s, got %v", res.RequeueAfter)
		}

		// Verify pending annotation is set on leader
		var leaderWithAnnotation corev1.Pod
		if err := fakeClient.Get(context.Background(), client.ObjectKeyFromObject(leader), &leaderWithAnnotation); err != nil {
			t.Fatal(err)
		}
		if leaderWithAnnotation.Annotations[leaderworkerset.GroupRecreatePendingAnnotationKey] == "" {
			t.Fatal("expected GroupRecreatePendingAnnotationKey to be set on leader")
		}

		// 2. Re-entrant reconcile triggered by watch update from annotation write
		res, err = r.reconcilePod(context.Background(), podReconcileRequestForPod(&leaderWithAnnotation, false))
		if err != nil {
			t.Fatalf("reconcilePod() on watch update error = %v", err)
		}
		if res.RequeueAfter < 5*time.Second || res.RequeueAfter > 9*time.Second {
			t.Fatalf("expected RequeueAfter ~8s preserved on watch update, got %v", res.RequeueAfter)
		}
	})

	t.Run("startupPolicy: LeaderReady with unready leader container restart propagates backoff", func(t *testing.T) {
		lws := wrappers.BuildLeaderWorkerSet("default").
			StartupPolicy(leaderworkerset.LeaderReadyStartupPolicy).
			RestartPolicy(leaderworkerset.RecreateGroupOnPodRestart).
			RestartBackoff(ptr.To(int32(10)), ptr.To(int32(300))).
			Size(2).
			Obj()

		now := time.Now()
		leader := wrappers.MakePodWithLabels(lws.Name, "0", "0", lws.Namespace, 2)
		leader.CreationTimestamp = metav1.NewTime(now.Add(-2 * time.Second))
		leader.Status.Phase = corev1.PodRunning
		leader.Status.Conditions = []corev1.PodCondition{
			{Type: corev1.PodReady, Status: corev1.ConditionFalse},
		}
		leader.Status.ContainerStatuses = []corev1.ContainerStatus{{RestartCount: 1}}
		leader.Spec.Hostname = leader.Name
		leader.Spec.Subdomain = lws.Name

		tempClient := fake.NewClientBuilder().WithScheme(scheme).Build()
		revision, err := revisionutils.NewRevision(context.Background(), tempClient, lws, "")
		if err != nil {
			t.Fatal(err)
		}
		leader.Labels[leaderworkerset.RevisionKey] = revisionutils.GetRevisionKey(revision)

		fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(lws, revision, leader).Build()
		r := &PodReconciler{Client: fakeClient, Scheme: scheme, Record: fakeEventRecorder{}}

		res, err := r.reconcilePod(context.Background(), podReconcileRequestForPod(leader, false))
		if err != nil {
			t.Fatalf("reconcilePod() error = %v", err)
		}
		if res.RequeueAfter < 5*time.Second || res.RequeueAfter > 9*time.Second {
			t.Fatalf("expected RequeueAfter ~8s, got %v", res.RequeueAfter)
		}

		// Ensure worker statefulset was not created because leader is unready
		var workerSts appsv1.StatefulSet
		if err := fakeClient.Get(context.Background(), client.ObjectKeyFromObject(leader), &workerSts); !apierrors.IsNotFound(err) {
			t.Fatalf("expected worker StatefulSet not to be created for unready leader, got err = %v", err)
		}
	})
}

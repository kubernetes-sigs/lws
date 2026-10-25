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
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	appsapplyv1 "k8s.io/client-go/applyconfigurations/apps/v1"
	coreapplyv1 "k8s.io/client-go/applyconfigurations/core/v1"
	metaapplyv1 "k8s.io/client-go/applyconfigurations/meta/v1"
	"k8s.io/client-go/tools/events"
	"k8s.io/utils/lru"
	"k8s.io/utils/ptr"

	disaggregatedset "sigs.k8s.io/lws/api/disaggregatedset/v1"
	leaderworkerset "sigs.k8s.io/lws/api/leaderworkerset/v1"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/lws/pkg/schedulerprovider"
	revisionutils "sigs.k8s.io/lws/pkg/utils/revision"
	"sigs.k8s.io/lws/test/wrappers"
)

type fakeEventRecorder struct{}

func (fakeEventRecorder) Eventf(regarding runtime.Object, related runtime.Object, eventtype, reason, action, note string, args ...interface{}) {
}

func TestParallelWorkerCleanupOwnership(t *testing.T) {
	for _, identity := range []leaderworkerset.GroupIdentityType{leaderworkerset.GroupIdentityOrdinal, leaderworkerset.GroupIdentityHash} {
		scenarios := []string{"owned", "unowned", "missing leader", "replaced leader", "wrong owner kind", "wrong owner version", "missing workload", "replaced workload", "foreign LWS", "read error", "replacement during delete"}
		if identity == leaderworkerset.GroupIdentityHash {
			scenarios = append(scenarios, "replaced replica set", "missing replica set", "replica set read error", "constructor without setup", "literal without setup")
		}
		for _, scenario := range scenarios {
			t.Run(string(identity)+"/"+scenario, func(t *testing.T) {
				scheme := runtime.NewScheme()
				require.NoError(t, leaderworkerset.AddToScheme(scheme))
				require.NoError(t, appsv1.AddToScheme(scheme))
				require.NoError(t, corev1.AddToScheme(scheme))
				lws := &leaderworkerset.LeaderWorkerSet{ObjectMeta: metav1.ObjectMeta{Name: "model", Namespace: "default", UID: "lws"}}
				lws.Spec.GroupIdentity = identity
				workloadMeta := metav1.ObjectMeta{Name: lws.Name, Namespace: lws.Namespace, UID: "leaders", OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(lws, leaderworkerset.GroupVersion.WithKind("LeaderWorkerSet"))}}
				var workload client.Object = &appsv1.StatefulSet{ObjectMeta: workloadMeta}
				leaderOwner := metav1.NewControllerRef(workload, appsv1.SchemeGroupVersion.WithKind("StatefulSet"))
				objects := []client.Object{lws}
				if identity == leaderworkerset.GroupIdentityHash {
					workload = &appsv1.Deployment{ObjectMeta: workloadMeta}
					rs := &appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Name: "model-rs", Namespace: lws.Namespace, UID: "rs", OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(workload, appsv1.SchemeGroupVersion.WithKind("Deployment"))}}}
					if scenario != "missing replica set" {
						objects = append(objects, rs)
					}
					leaderOwner = metav1.NewControllerRef(rs, appsv1.SchemeGroupVersion.WithKind("ReplicaSet"))
					if scenario == "replaced replica set" {
						rs.UID = "replacement"
					}
				}
				leader := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "model-0", Namespace: lws.Namespace, UID: "leader", OwnerReferences: []metav1.OwnerReference{*leaderOwner}}}
				workers := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{
					Name: leader.Name, Namespace: lws.Namespace, UID: "workers",
					Labels:          map[string]string{leaderworkerset.SetNameLabelKey: lws.Name, leaderworkerset.RoleLabelKey: leaderworkerset.RoleWorker},
					OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(leader, corev1.SchemeGroupVersion.WithKind("Pod"))},
				}}
				switch scenario {
				case "unowned":
					workers.OwnerReferences = nil
				case "replaced leader":
					leader.UID = "replacement"
				case "wrong owner kind":
					workers.OwnerReferences[0].Kind = "ReplicaSet"
				case "wrong owner version":
					workers.OwnerReferences[0].APIVersion = "other.example/v1"
				case "replaced workload":
					workload.SetUID("replacement")
				case "foreign LWS":
					workload.GetOwnerReferences()[0].UID = "another-lws"
				}
				objects = append(objects, workers)
				if scenario != "missing workload" {
					objects = append(objects, workload)
				}
				if scenario != "missing leader" {
					objects = append(objects, leader)
				}
				storage := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
				withoutSetup := scenario == "constructor without setup" || scenario == "literal without setup"
				cl := interceptor.NewClient(storage, interceptor.Funcs{
					Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
						_, isReplicaSet := obj.(*appsv1.ReplicaSet)
						if !withoutSetup {
							require.False(t, isReplicaSet, "ReplicaSet lookup must bypass the manager cache")
						}
						if scenario == "read error" {
							return fmt.Errorf("owner lookup failed")
						}
						return c.Get(ctx, key, obj, opts...)
					},
					Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
						if scenario == "replacement during delete" {
							require.NoError(t, c.Delete(ctx, workers))
							replacement := workers.DeepCopy()
							replacement.UID, replacement.OwnerReferences = "replacement", nil
							replacement.ResourceVersion = ""
							require.NoError(t, c.Create(ctx, replacement))
							// The fake client does not enforce UID preconditions.
							options := (&client.DeleteOptions{}).ApplyOptions(opts)
							require.NotNil(t, options.Preconditions)
							require.Equal(t, workers.UID, ptr.Deref(options.Preconditions.UID, ""))
							return apierrors.NewConflict(appsv1.Resource("statefulsets"), obj.GetName(), fmt.Errorf("UID precondition failed"))
						}
						return c.Delete(ctx, obj, opts...)
					},
				})
				r := NewLeaderWorkerSetReconciler(cl, scheme, events.NewFakeRecorder(5))
				apiReads := 0
				apiReader := interceptor.NewClient(storage, interceptor.Funcs{
					Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
						apiReads++
						require.IsType(t, &appsv1.ReplicaSet{}, obj, "other owner lookups must stay cached")
						if scenario == "replica set read error" {
							return fmt.Errorf("owner lookup failed")
						}
						return c.Get(ctx, key, obj, opts...)
					},
				})
				switch scenario {
				case "constructor without setup":
					require.NotNil(t, r.apiReader, "constructor should provide a usable default reader")
				case "literal without setup":
					r = &LeaderWorkerSetReconciler{Client: cl, Scheme: scheme}
				default:
					r.apiReader = apiReader
				}
				var err error
				require.NotPanics(t, func() { err = r.deleteWorkerStatefulSets(t.Context(), lws) })
				if identity == leaderworkerset.GroupIdentityOrdinal || withoutSetup {
					require.Zero(t, apiReads)
				} else if scenario == "owned" {
					require.Equal(t, 1, apiReads)
				}
				switch scenario {
				case "read error", "replica set read error":
					require.ErrorContains(t, err, "owner lookup failed")
				case "replacement during delete":
					require.True(t, apierrors.IsConflict(err), "stale cleanup must conflict: %v", err)
				default:
					require.NoError(t, err)
				}
				var remaining appsv1.StatefulSetList
				require.NoError(t, cl.List(t.Context(), &remaining, client.MatchingLabels{leaderworkerset.RoleLabelKey: leaderworkerset.RoleWorker}))
				if scenario == "owned" || withoutSetup {
					require.Empty(t, remaining.Items)
				} else {
					require.Len(t, remaining.Items, 1, "unverified or replaced workers must survive")
				}
			})
		}
	}
}

func TestParallelWorkerCleanupReusesReplicaSets(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, leaderworkerset.AddToScheme(scheme))
	require.NoError(t, appsv1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	lws := &leaderworkerset.LeaderWorkerSet{ObjectMeta: metav1.ObjectMeta{Name: "model", Namespace: "default", UID: "lws"}}
	lws.Spec.GroupIdentity = leaderworkerset.GroupIdentityHash
	deployment := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{
		Name: lws.Name, Namespace: lws.Namespace, UID: "deployment",
		OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(lws, leaderworkerset.GroupVersion.WithKind("LeaderWorkerSet"))},
	}}
	rsA := &appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{
		Name: "rs-a", Namespace: lws.Namespace, UID: "rs-a",
		OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(deployment, appsv1.SchemeGroupVersion.WithKind("Deployment"))},
	}}
	rsB := rsA.DeepCopy()
	rsB.Name, rsB.UID = "rs-b", "rs-b"
	ownerA := *metav1.NewControllerRef(rsA, appsv1.SchemeGroupVersion.WithKind("ReplicaSet"))
	ownerB := *metav1.NewControllerRef(rsB, appsv1.SchemeGroupVersion.WithKind("ReplicaSet"))
	staleOwner := ownerA
	staleOwner.UID = "old-rs-a"
	objects := []client.Object{lws, deployment, rsA} // rs-b is initially missing.
	for i, owner := range []metav1.OwnerReference{ownerA, ownerA, staleOwner, ownerB, ownerB} {
		name := fmt.Sprintf("model-%d", i)
		leader := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: lws.Namespace, UID: types.UID(name), OwnerReferences: []metav1.OwnerReference{owner},
		}}
		workers := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: lws.Namespace, UID: types.UID(name + "-workers"),
			Labels:          map[string]string{leaderworkerset.SetNameLabelKey: lws.Name, leaderworkerset.RoleLabelKey: leaderworkerset.RoleWorker},
			OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(leader, corev1.SchemeGroupVersion.WithKind("Pod"))},
		}}
		objects = append(objects, leader, workers)
	}
	storage := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
	cl := interceptor.NewClient(storage, interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			_, isReplicaSet := obj.(*appsv1.ReplicaSet)
			require.False(t, isReplicaSet, "ReplicaSet lookup must bypass the manager cache")
			return c.Get(ctx, key, obj, opts...)
		},
	})
	r := NewLeaderWorkerSetReconciler(cl, scheme, events.NewFakeRecorder(5))
	reads := map[string]int{}
	r.apiReader = interceptor.NewClient(storage, interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			require.IsType(t, &appsv1.ReplicaSet{}, obj)
			reads[key.Name]++
			return c.Get(ctx, key, obj, opts...)
		},
	})
	require.NoError(t, r.deleteWorkerStatefulSets(t.Context(), lws))
	require.Equal(t, map[string]int{"rs-a": 1, "rs-b": 1}, reads, "read each owner once, including missing owners")
	var remaining appsv1.StatefulSetList
	require.NoError(t, storage.List(t.Context(), &remaining))
	require.Len(t, remaining.Items, 3, "workers with a stale or missing owner must survive")

	// A new pass must refresh both successful and missing lookups, not retain a cache.
	require.NoError(t, storage.Create(t.Context(), rsB))
	require.NoError(t, r.deleteWorkerStatefulSets(t.Context(), lws))
	require.Equal(t, map[string]int{"rs-a": 2, "rs-b": 2}, reads)
	require.NoError(t, storage.List(t.Context(), &remaining))
	require.Len(t, remaining.Items, 1)
	require.Equal(t, "model-2", remaining.Items[0].Name, "a reused ReplicaSet must still match each leader's owner UID")
}

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
		stsReplicas     *int32
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
						"leaderworkerset.sigs.k8s.io/role":                   "leader",
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
									Image:     ptr.To[string]("docker.io/nginxinc/nginx-unprivileged:1.27"),
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
						"leaderworkerset.sigs.k8s.io/role":                   "leader",
					},
					Annotations: map[string]string{
						"leaderworkerset.sigs.k8s.io/exclusive-topology": "topologyKey",
						"leaderworkerset.sigs.k8s.io/replicas":           "1",
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
									Image:     ptr.To[string]("docker.io/nginxinc/nginx-unprivileged:1.27"),
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
						"leaderworkerset.sigs.k8s.io/role":                   "leader",
					},
					Annotations: map[string]string{
						"leaderworkerset.sigs.k8s.io/exclusive-topology": "topologyKey",
						"leaderworkerset.sigs.k8s.io/replicas":           "2",
					},
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
									Image:     ptr.To[string]("docker.io/nginxinc/nginx-unprivileged:1.27"),
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
						"leaderworkerset.sigs.k8s.io/role":                   "leader",
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
									Image:     ptr.To[string]("docker.io/nginxinc/nginx-unprivileged:1.27"),
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
						WithRollingUpdate(appsapplyv1.RollingUpdateStatefulSetStrategy().WithPartition(0).WithMaxUnavailable(intstr.FromInt32(3))),
				},
			},
		},
		{
			name:        "0 maxUnavailable, 2 maxSurge, with empty leader template, exclusive placement disabled",
			revisionKey: revisionKey2,
			lws: wrappers.BuildBasicLeaderWorkerSet("test-sample", "default").
				Replica(1).
				RolloutStrategy(leaderworkerset.RolloutStrategy{
					Type: leaderworkerset.RollingUpdateStrategyType,
					RollingUpdateConfiguration: &leaderworkerset.RollingUpdateConfiguration{
						MaxUnavailable: intstr.FromInt32(0),
						MaxSurge:       intstr.FromInt32(2),
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
						"leaderworkerset.sigs.k8s.io/role":                   "leader",
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
									Image:     ptr.To[string]("docker.io/nginxinc/nginx-unprivileged:1.27"),
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
						// maxSurge is capped at 1 (the value of lwsReplicas),
						// so stsMaxUnavailableInt = 0 (lwsMaxUnavailable) + 1 (capped maxSurge) = 1.
						WithRollingUpdate(appsapplyv1.RollingUpdateStatefulSetStrategy().WithPartition(0).WithMaxUnavailable(intstr.FromInt32(1))),
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
						"leaderworkerset.sigs.k8s.io/role":                   "leader",
					},
					Annotations: map[string]string{
						"leaderworkerset.sigs.k8s.io/replicas":                    "1",
						"leaderworkerset.sigs.k8s.io/subgroup-exclusive-topology": "topologyKey",
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
									Image:     ptr.To[string]("docker.io/nginxinc/nginx-unprivileged:1.27"),
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
						"leaderworkerset.sigs.k8s.io/role":                   "leader",
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
					ServiceName:         ptr.To[string]("test-sample"),
					PodManagementPolicy: ptr.To[appsv1.PodManagementPolicyType](appsv1.ParallelPodManagement),
					UpdateStrategy: appsapplyv1.StatefulSetUpdateStrategy().
						WithType(appsv1.RollingUpdateStatefulSetStrategyType).
						WithRollingUpdate(appsapplyv1.RollingUpdateStatefulSetStrategy().WithPartition(0).WithMaxUnavailable(intstr.FromInt32(1))),
				},
			},
		},
		{
			name:        "0 replica, 0 maxUnavailable, 0 maxSurge, with empty leader template, exclusive placement disabled",
			revisionKey: revisionKey2,
			lws: wrappers.BuildBasicLeaderWorkerSet("test-sample", "default").
				Replica(0).
				RolloutStrategy(leaderworkerset.RolloutStrategy{
					Type: leaderworkerset.RollingUpdateStrategyType,
					RollingUpdateConfiguration: &leaderworkerset.RollingUpdateConfiguration{
						MaxUnavailable: intstr.FromInt32(0),
						MaxSurge:       intstr.FromInt32(0),
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
						"leaderworkerset.sigs.k8s.io/role":                   "leader",
					},
					Annotations: map[string]string{"leaderworkerset.sigs.k8s.io/replicas": "0"},
				},
				Spec: &appsapplyv1.StatefulSetSpecApplyConfiguration{
					Replicas: ptr.To[int32](0),
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
									Image:     ptr.To[string]("docker.io/nginxinc/nginx-unprivileged:1.27"),
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
						// Sts maxUnavailable is forced to be at least 1,
						// even if lws maxUnavailable=0 and lws maxSurge=0.
						WithRollingUpdate(appsapplyv1.RollingUpdateStatefulSetStrategy().WithPartition(0).WithMaxUnavailable(intstr.FromInt32(1))),
				},
			},
		},
		{
			// Validates maxSurge uses lws replicas, not sts replicas.
			name:        "1 maxUnavailable, 50% maxSurge, 2 lws replicas, 3 sts replicas currently",
			revisionKey: revisionKey2,
			stsReplicas: ptr.To[int32](3),
			lws: wrappers.BuildBasicLeaderWorkerSet("test-sample", "default").
				Replica(2).
				RolloutStrategy(leaderworkerset.RolloutStrategy{
					Type: leaderworkerset.RollingUpdateStrategyType,
					RollingUpdateConfiguration: &leaderworkerset.RollingUpdateConfiguration{
						MaxUnavailable: intstr.FromInt32(1),
						MaxSurge:       intstr.FromString("50%"),
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
						"leaderworkerset.sigs.k8s.io/role":                   "leader",
					},
					Annotations: map[string]string{"leaderworkerset.sigs.k8s.io/replicas": "2"},
				},
				Spec: &appsapplyv1.StatefulSetSpecApplyConfiguration{
					Replicas: ptr.To[int32](3), // using stsReplicas
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
									Image:     ptr.To[string]("docker.io/nginxinc/nginx-unprivileged:1.27"),
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
						// maxUnavailable=1, maxSurge=50% of 2 replicas (lwsReplicas) = 1.
						// So stsMaxUnavailableInt = 1 + 1 = 2
						WithRollingUpdate(appsapplyv1.RollingUpdateStatefulSetStrategy().WithPartition(0).WithMaxUnavailable(intstr.FromInt32(2))),
				},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			stsReplicas := *tc.lws.Spec.Replicas
			if tc.stsReplicas != nil {
				stsReplicas = *tc.stsReplicas
			}
			stsApplyConfig, err := constructLeaderStatefulSetApplyConfiguration(tc.lws, 0, stsReplicas, tc.revisionKey)
			if err != nil {
				t.Errorf("failed with error: %s", err.Error())
			}
			if diff := cmp.Diff(tc.wantApplyConfig, stsApplyConfig); diff != "" {
				t.Errorf("unexpected StatefulSet apply configuration: %s", diff)
			}
		})
	}
}

func TestLeaderStatefulSetApplyConfigPropagatesObjectMeta(t *testing.T) {
	lws := wrappers.BuildBasicLeaderWorkerSet("test-sample", "default").
		Labels(map[string]string{
			"app":                           "inference",
			leaderworkerset.SetNameLabelKey: "user-value",
		}).
		Annotation(map[string]string{
			"owner":                               "platform",
			leaderworkerset.ReplicasAnnotationKey: "user-value",
		}).
		Replica(2).
		RolloutStrategy(leaderworkerset.RolloutStrategy{
			Type: leaderworkerset.RollingUpdateStrategyType,
			RollingUpdateConfiguration: &leaderworkerset.RollingUpdateConfiguration{
				MaxUnavailable: intstr.FromInt32(1),
			},
		}).
		WorkerTemplateSpec(wrappers.MakeWorkerPodSpec()).
		Size(1).
		RestartPolicy(leaderworkerset.RecreateGroupOnPodRestart).
		Obj()

	stsApplyConfig, err := constructLeaderStatefulSetApplyConfiguration(lws, 0, 2, "revision-1")
	if err != nil {
		t.Fatalf("failed with error: %s", err.Error())
	}

	wantLabels := map[string]string{
		"app":                           "inference",
		leaderworkerset.SetNameLabelKey: "test-sample",
		leaderworkerset.RevisionKey:     "revision-1",
		leaderworkerset.RoleLabelKey:    leaderworkerset.RoleLeader,
	}
	if diff := cmp.Diff(wantLabels, stsApplyConfig.Labels); diff != "" {
		t.Errorf("unexpected StatefulSet labels: %s", diff)
	}

	wantAnnotations := map[string]string{
		"owner":                               "platform",
		leaderworkerset.ReplicasAnnotationKey: "2",
	}
	if diff := cmp.Diff(wantAnnotations, stsApplyConfig.Annotations); diff != "" {
		t.Errorf("unexpected StatefulSet annotations: %s", diff)
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

func TestCalculateRollingUpdateReplicas(t *testing.T) {
	tests := []struct {
		name           string
		lwsReplicas    int32
		maxSurge       int32
		maxUnavailable int32
		unready        int32
		wantReplicas   int32
	}{
		{
			name:           "keeps surge replicas until maxUnavailable budget covers unready desired replicas",
			lwsReplicas:    1,
			maxSurge:       1,
			maxUnavailable: 0,
			unready:        1,
			wantReplicas:   2,
		},
		{
			name:           "reclaims surge replicas gradually once enough desired replicas are ready",
			lwsReplicas:    4,
			maxSurge:       2,
			maxUnavailable: 1,
			unready:        2,
			wantReplicas:   5,
		},
		{
			name:           "reclaims surge even before partition reaches zero when maxUnavailable permits it",
			lwsReplicas:    2,
			maxSurge:       2,
			maxUnavailable: 1,
			unready:        2,
			wantReplicas:   3,
		},
		{
			name:           "falls back to desired replicas when all desired replicas are ready",
			lwsReplicas:    1,
			maxSurge:       1,
			maxUnavailable: 0,
			unready:        0,
			wantReplicas:   1,
		},
		{
			name:           "reclaims surge when maxUnavailable permits an unready desired replica",
			lwsReplicas:    1,
			maxSurge:       1,
			maxUnavailable: 1,
			unready:        1,
			wantReplicas:   1,
		},
		{
			name:           "does not surge when maxSurge is zero",
			lwsReplicas:    3,
			maxSurge:       0,
			maxUnavailable: 0,
			unready:        1,
			wantReplicas:   3,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := calculateRollingUpdateReplicas(tc.lwsReplicas, tc.maxSurge, tc.maxUnavailable, tc.unready)
			if got != tc.wantReplicas {
				t.Fatalf("calculateRollingUpdateReplicas()=%d, want %d", got, tc.wantReplicas)
			}
		})
	}
}

func TestRollingUpdateParametersScaleUpDoesNotCreateExtraSurge(t *testing.T) {
	reconciler := &LeaderWorkerSetReconciler{Record: fakeEventRecorder{}}
	lws := wrappers.BuildBasicLeaderWorkerSet("test-sample", "default").
		Replica(3).
		Size(1).
		RolloutStrategy(leaderworkerset.RolloutStrategy{
			Type: leaderworkerset.RollingUpdateStrategyType,
			RollingUpdateConfiguration: &leaderworkerset.RollingUpdateConfiguration{
				Partition:      ptr.To[int32](0),
				MaxUnavailable: intstr.FromInt32(0),
				MaxSurge:       intstr.FromInt32(1),
			},
		}).Obj()
	sts := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:        lws.Name,
			Namespace:   lws.Namespace,
			Annotations: map[string]string{leaderworkerset.ReplicasAnnotationKey: strconv.Itoa(2)},
		},
		Spec: appsv1.StatefulSetSpec{
			Replicas: ptr.To[int32](2),
			UpdateStrategy: appsv1.StatefulSetUpdateStrategy{
				Type: appsv1.RollingUpdateStatefulSetStrategyType,
				RollingUpdate: &appsv1.RollingUpdateStatefulSetStrategy{
					Partition: ptr.To[int32](0),
				},
			},
		},
	}

	partition, replicas, err := reconciler.rollingUpdateParameters(context.Background(), lws, sts, "rev-new", false)
	if err != nil {
		t.Fatalf("rollingUpdateParameters() unexpected error: %v", err)
	}
	if partition != 0 {
		t.Fatalf("rollingUpdateParameters() partition=%d, want 0", partition)
	}
	if replicas != 3 {
		t.Fatalf("rollingUpdateParameters() replicas=%d, want 3", replicas)
	}
}

func TestRollingUpdateParametersScaleUpWithTemplateUpdateDoesNotCreateExtraSurge(t *testing.T) {
	reconciler := &LeaderWorkerSetReconciler{Record: fakeEventRecorder{}}
	lws := wrappers.BuildBasicLeaderWorkerSet("test-sample", "default").
		Replica(3).
		Size(1).
		RolloutStrategy(leaderworkerset.RolloutStrategy{
			Type: leaderworkerset.RollingUpdateStrategyType,
			RollingUpdateConfiguration: &leaderworkerset.RollingUpdateConfiguration{
				Partition:      ptr.To[int32](0),
				MaxUnavailable: intstr.FromInt32(0),
				MaxSurge:       intstr.FromInt32(1),
			},
		}).Obj()
	sts := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:        lws.Name,
			Namespace:   lws.Namespace,
			Annotations: map[string]string{leaderworkerset.ReplicasAnnotationKey: strconv.Itoa(2)},
		},
		Spec: appsv1.StatefulSetSpec{
			Replicas: ptr.To[int32](2),
			UpdateStrategy: appsv1.StatefulSetUpdateStrategy{
				Type: appsv1.RollingUpdateStatefulSetStrategyType,
				RollingUpdate: &appsv1.RollingUpdateStatefulSetStrategy{
					Partition: ptr.To[int32](0),
				},
			},
		},
	}

	partition, replicas, err := reconciler.rollingUpdateParameters(context.Background(), lws, sts, "rev-new", true)
	if err != nil {
		t.Fatalf("rollingUpdateParameters() unexpected error: %v", err)
	}
	if partition != 2 {
		t.Fatalf("rollingUpdateParameters() partition=%d, want 2", partition)
	}
	if replicas != 3 {
		t.Fatalf("rollingUpdateParameters() replicas=%d, want 3", replicas)
	}
}

func TestRollingUpdateParametersTemplateUpdateReclaimsSurgeWhenAllowed(t *testing.T) {
	reconciler := &LeaderWorkerSetReconciler{Record: fakeEventRecorder{}}
	lws := wrappers.BuildBasicLeaderWorkerSet("test-sample", "default").
		Replica(2).
		Size(1).
		RolloutStrategy(leaderworkerset.RolloutStrategy{
			Type: leaderworkerset.RollingUpdateStrategyType,
			RollingUpdateConfiguration: &leaderworkerset.RollingUpdateConfiguration{
				Partition:      ptr.To[int32](0),
				MaxUnavailable: intstr.FromInt32(1),
				MaxSurge:       intstr.FromInt32(2),
			},
		}).Obj()
	sts := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:        lws.Name,
			Namespace:   lws.Namespace,
			Annotations: map[string]string{leaderworkerset.ReplicasAnnotationKey: strconv.Itoa(2)},
		},
		Spec: appsv1.StatefulSetSpec{
			Replicas: ptr.To[int32](2),
			UpdateStrategy: appsv1.StatefulSetUpdateStrategy{
				Type: appsv1.RollingUpdateStatefulSetStrategyType,
				RollingUpdate: &appsv1.RollingUpdateStatefulSetStrategy{
					Partition: ptr.To[int32](0),
				},
			},
		},
	}

	partition, replicas, err := reconciler.rollingUpdateParameters(context.Background(), lws, sts, "rev-new", true)
	if err != nil {
		t.Fatalf("rollingUpdateParameters() unexpected error: %v", err)
	}
	if partition != 2 {
		t.Fatalf("rollingUpdateParameters() partition=%d, want 2", partition)
	}
	if replicas != 3 {
		t.Fatalf("rollingUpdateParameters() replicas=%d, want 3", replicas)
	}
}

func TestDSOrdinalScaleDownDoesNotWaitForStartup(t *testing.T) {
	for _, size := range []int{1, 2} {
		for _, tc := range []struct {
			name        string
			owner       string
			previous    string
			partition   int32
			newTemplate bool
			oldGroup    bool
			want        int32
		}{
			{"DS explicit scale", "ds", "4", 0, false, false, 3},
			{"DS partition zero with old group", "ds", "4", 0, false, true, 3},
			{"DS rollout surplus", "ds", "3", 0, false, false, 4},
			{"DS active native partition", "ds", "4", 2, false, false, 4},
			{"DS new template and scale", "ds", "4", 0, true, false, 4},
			{"standalone scale unchanged", "", "4", 0, false, false, 4},
			{"standalone partition zero with old group", "", "4", 0, false, true, 4},
			{"unrelated controller", "other-kind", "4", 0, false, false, 4},
			{"unrelated API", "other-api", "4", 0, false, false, 4},
			{"DS is not the controller", "non-controller", "4", 0, false, false, 4},
		} {
			t.Run(fmt.Sprintf("%s/size%d", tc.name, size), func(t *testing.T) {
				lws := wrappers.BuildLeaderWorkerSet("default").Replica(3).Size(size).
					RolloutStrategy(leaderworkerset.RolloutStrategy{Type: leaderworkerset.RollingUpdateStrategyType,
						RollingUpdateConfiguration: &leaderworkerset.RollingUpdateConfiguration{
							Partition: ptr.To[int32](0), MaxSurge: intstr.FromInt(1), MaxUnavailable: intstr.FromInt(0),
						}}).Obj()
				// Labels alone must not opt standalone LWS into DS scaling semantics.
				lws.Labels = map[string]string{disaggregatedset.SetNameLabelKey: "ds"}
				if tc.owner != "" {
					owner := metav1.OwnerReference{APIVersion: disaggregatedset.GroupVersion.String(), Kind: "DisaggregatedSet",
						Name: "ds", UID: "ds-uid", Controller: ptr.To(true)}
					switch tc.owner {
					case "other-kind":
						owner.Kind = "OtherController"
					case "other-api":
						owner.APIVersion = "other.example/v1"
					case "non-controller":
						owner.Controller = ptr.To(false)
					}
					lws.OwnerReferences = []metav1.OwnerReference{owner}
				}
				sts := lwsStatusRollingUpdateSts(lws, 4, tc.partition, tc.previous)
				// Ordinal 1 stays unready, including when only its worker is loading.
				var objects []client.Object
				for i := range 4 {
					revision := "new"
					if tc.oldGroup && i == 0 {
						revision = "old"
					}
					objects = append(objects, lwsStatusLeaderPod(lws, i, revision, size > 1 || i != 1))
					if size > 1 {
						objects = append(objects, lwsStatusWorkerSts(lws, i, revision, i != 1))
					}
				}
				r, _ := lwsStatusNewReconciler(t, objects...)
				_, replicas, err := r.rollingUpdateParameters(t.Context(), lws, sts, "new", tc.newTemplate)
				require.NoError(t, err)
				require.Equal(t, tc.want, replicas)
			})
		}
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
			name:      "Same condition type, different condition status",
			condition: metav1.Condition{Type: "Progressing", Status: "True", ObservedGeneration: 1},
			lws: wrappers.BuildBasicLeaderWorkerSet("test-sample", "default").
				Generation(1).
				Conditions([]metav1.Condition{{Type: "Progressing", Status: "False", ObservedGeneration: 1}}).
				Obj(),
			expectedShouldUpdate: true,
		},
		{
			name:      "Different condition type, new condition status is true",
			condition: metav1.Condition{Type: "Progressing", Status: "True", ObservedGeneration: 1},
			lws: wrappers.BuildBasicLeaderWorkerSet("test-sample", "default").
				Generation(1).
				Conditions([]metav1.Condition{{Type: "Available", Status: "False", ObservedGeneration: 1}}).
				Obj(),
			expectedShouldUpdate: true,
		},
		{
			name:                 "No initial condition",
			condition:            metav1.Condition{Type: "Progressing", Status: "True", ObservedGeneration: 1},
			lws:                  wrappers.BuildBasicLeaderWorkerSet("test-sample", "default").Generation(1).Obj(),
			expectedShouldUpdate: true,
		},
		{
			name:      "Same condition type, Same condition status",
			condition: metav1.Condition{Type: "Progressing", Status: "False"},
			lws: wrappers.BuildBasicLeaderWorkerSet("test-sample", "default").
				Conditions([]metav1.Condition{{Type: "Progressing", Status: "False"}}).
				Obj(),
		},
		{
			name:      "Same condition type, same status, but generation advanced",
			condition: metav1.Condition{Type: "Available", Status: "True", ObservedGeneration: 2},
			lws: wrappers.BuildBasicLeaderWorkerSet("test-sample", "default").
				Generation(2).
				Conditions([]metav1.Condition{{Type: "Available", Status: "True", ObservedGeneration: 1}}).
				Obj(),
			expectedShouldUpdate: true,
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

func TestSetConditionUpdatesDetailsWithoutStatusTransition(t *testing.T) {
	transitionTime := metav1.Now()
	lws := wrappers.BuildBasicLeaderWorkerSet("test-sample", "default").
		Generation(2).
		Conditions([]metav1.Condition{{
			Type:               "Available",
			Status:             metav1.ConditionFalse,
			ObservedGeneration: 1,
			Reason:             "OldReason",
			Message:            "old message",
			LastTransitionTime: transitionTime,
		}}).
		Obj()

	updated := metav1.Condition{
		Type:               "Available",
		Status:             metav1.ConditionFalse,
		ObservedGeneration: 2,
		Reason:             "ReplicaRestartBudgetExceeded",
		Message:            "Not all replicas are ready",
	}
	if !setCondition(lws, updated) {
		t.Fatal("setCondition() should update changed condition details")
	}
	got := lws.Status.Conditions[0]
	if got.Reason != updated.Reason || got.Message != updated.Message || got.ObservedGeneration != updated.ObservedGeneration {
		t.Fatalf("condition details were not updated: %#v", got)
	}
	if !got.LastTransitionTime.Time.Equal(transitionTime.Time) {
		t.Fatalf("LastTransitionTime changed without a status transition: got %v, want %v", got.LastTransitionTime, transitionTime)
	}
}

func TestSetConditionPreservesOtherConditionDetailsOnExclusiveTransition(t *testing.T) {
	lws := wrappers.BuildBasicLeaderWorkerSet("test-sample", "default").
		Generation(1).
		Conditions([]metav1.Condition{{
			Type:               string(leaderworkerset.LeaderWorkerSetProgressing),
			Status:             metav1.ConditionTrue,
			ObservedGeneration: 1,
			Reason:             GroupsProgressing,
			Message:            "Replicas are progressing",
		}}).
		Obj()

	if !setCondition(lws, metav1.Condition{
		Type:               string(leaderworkerset.LeaderWorkerSetAvailable),
		Status:             metav1.ConditionTrue,
		ObservedGeneration: 1,
		Reason:             "AllGroupsReady",
		Message:            "All replicas are ready",
	}) {
		t.Fatal("setCondition() should transition Progressing to False")
	}
	progressing := apimeta.FindStatusCondition(lws.Status.Conditions, string(leaderworkerset.LeaderWorkerSetProgressing))
	if progressing == nil || progressing.Status != metav1.ConditionFalse ||
		progressing.Reason != GroupsProgressing || progressing.Message != "Replicas are progressing" {
		t.Fatalf("Progressing condition = %#v, want False with its original details", progressing)
	}
}

func TestGetUpdatedRevision(t *testing.T) {
	client := fake.NewClientBuilder().Build()

	tests := []struct {
		name           string
		lws            *leaderworkerset.LeaderWorkerSet
		modifyRevision func(*appsv1.ControllerRevision)
		expectUpdate   bool
	}{
		{
			name:         "revision matches current spec, no update",
			lws:          wrappers.BuildLeaderWorkerSet("default").Obj(),
			expectUpdate: false,
		},
		{
			name: "revision has old serialization with creationTimestamp null, semantic match, no update",
			lws:  wrappers.BuildLeaderWorkerSet("default").Obj(),
			modifyRevision: func(rev *appsv1.ControllerRevision) {
				// Simulate old (before v1.34) client-go serialization that includes "creationTimestamp":null
				rev.Data.Raw = []byte(strings.ReplaceAll(string(rev.Data.Raw), `"metadata":{}`, `"metadata":{"creationTimestamp":null}`))
			},
			expectUpdate: false,
		},
		{
			name: "revision has different spec, should trigger update",
			lws:  wrappers.BuildLeaderWorkerSet("default").Obj(),
			modifyRevision: func(rev *appsv1.ControllerRevision) {
				// Simulate a real spec change by modifying the container name
				rev.Data.Raw = []byte(strings.ReplaceAll(string(rev.Data.Raw), `"name":"leader"`, `"name":"changed"`))
			},
			expectUpdate: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			reconciler := &LeaderWorkerSetReconciler{
				Client:                client,
				Record:                fakeEventRecorder{},
				revisionEqualityCache: lru.New(100),
			}

			revision, err := revisionutils.NewRevision(context.TODO(), client, tc.lws, "")
			if err != nil {
				t.Fatal(err)
			}

			if tc.modifyRevision != nil {
				tc.modifyRevision(revision)
			}

			updatedRevision, err := reconciler.getUpdatedRevision(context.TODO(), tc.lws, revision)
			if err != nil {
				t.Fatal(err)
			}

			gotUpdate := updatedRevision != nil
			if gotUpdate != tc.expectUpdate {
				t.Errorf("expected update=%t, got update=%t", tc.expectUpdate, gotUpdate)
			}
		})
	}
}

func TestEnqueueLWSRequests(t *testing.T) {
	tests := []struct {
		name   string
		object client.Object
		want   []reconcile.Request
	}{
		{
			name: "unrelated statefulset without lws label",
			object: &appsv1.StatefulSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "unrelated-sts",
					Namespace: "default",
				},
			},
			want: nil,
		},
		{
			name: "statefulset with empty lws label",
			object: &appsv1.StatefulSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "empty-label-sts",
					Namespace: "default",
					Labels: map[string]string{
						leaderworkerset.SetNameLabelKey: "",
					},
				},
			},
			want: nil,
		},
		{
			name: "lws-managed statefulset with valid lws label",
			object: &appsv1.StatefulSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "lws-sts",
					Namespace: "default",
					Labels: map[string]string{
						leaderworkerset.SetNameLabelKey: "my-lws",
					},
				},
			},
			want: []reconcile.Request{
				{
					NamespacedName: types.NamespacedName{
						Name:      "my-lws",
						Namespace: "default",
					},
				},
			},
		},
		{
			name: "unrelated pod without lws label",
			object: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "unrelated-pod",
					Namespace: "default",
				},
			},
			want: nil,
		},
		{
			name: "lws-managed pod with valid lws label",
			object: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "my-lws-0-1",
					Namespace: "default",
					Labels: map[string]string{
						leaderworkerset.SetNameLabelKey: "my-lws",
					},
				},
			},
			want: []reconcile.Request{
				{
					NamespacedName: types.NamespacedName{
						Name:      "my-lws",
						Namespace: "default",
					},
				},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := enqueueLWSRequests(context.Background(), tc.object)
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Errorf("unexpected reconcile requests (-want +got):\n%s", diff)
			}
		})
	}
}

func TestLeaderWorkerSetPodWatchPredicate(t *testing.T) {
	fakeClient := fake.NewClientBuilder().Build()
	k8sProvider := schedulerprovider.NewKubernetesProvider(fakeClient)
	volcanoProvider := schedulerprovider.NewVolcanoProvider(fakeClient)

	leaderPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-lws-0",
			Namespace: "default",
			Labels: map[string]string{
				leaderworkerset.SetNameLabelKey:     "test-lws",
				leaderworkerset.WorkerIndexLabelKey: "0",
			},
		},
	}
	workerPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-lws-0-1",
			Namespace: "default",
			Labels: map[string]string{
				leaderworkerset.SetNameLabelKey:     "test-lws",
				leaderworkerset.WorkerIndexLabelKey: "1",
			},
		},
	}
	scheduledWorkerPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-lws-0-1",
			Namespace: "default",
			Labels: map[string]string{
				leaderworkerset.SetNameLabelKey:     "test-lws",
				leaderworkerset.WorkerIndexLabelKey: "1",
			},
		},
		Spec: corev1.PodSpec{
			SchedulingGroup: &corev1.PodSchedulingGroup{
				PodGroupName: ptr.To("test-lws-0-pg"),
			},
		},
	}
	annotatedWorkerPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-lws-0-1",
			Namespace: "default",
			Labels: map[string]string{
				leaderworkerset.SetNameLabelKey:     "test-lws",
				leaderworkerset.WorkerIndexLabelKey: "1",
			},
			Annotations: map[string]string{
				schedulerprovider.WorkloadSchedulingAnnotationKey: "replica",
			},
		},
	}

	t.Run("CreateFunc", func(t *testing.T) {
		r := &LeaderWorkerSetReconciler{SchedulerProvider: k8sProvider}
		pred := r.podWatchPredicate()

		if !pred.Create(event.CreateEvent{Object: leaderPod}) {
			t.Errorf("expected Create(leaderPod) to be true, got false")
		}
		if pred.Create(event.CreateEvent{Object: workerPod}) {
			t.Errorf("expected Create(workerPod) to be false, got true")
		}
		if pred.Create(event.CreateEvent{Object: nil}) {
			t.Errorf("expected Create(nil) to be false, got true")
		}
	})

	t.Run("UpdateFunc", func(t *testing.T) {
		r := &LeaderWorkerSetReconciler{SchedulerProvider: k8sProvider}
		pred := r.podWatchPredicate()

		leaderWithAnnotation := leaderPod.DeepCopy()
		leaderWithAnnotation.Annotations = map[string]string{
			leaderworkerset.GroupRestartBudgetExhaustedAnnotationKey: "true",
		}
		leaderWithUnrelatedAnnotation := leaderPod.DeepCopy()
		leaderWithUnrelatedAnnotation.Annotations = map[string]string{
			"unrelated": "true",
		}

		if !pred.Update(event.UpdateEvent{ObjectOld: leaderPod, ObjectNew: leaderWithAnnotation}) {
			t.Errorf("expected Update(leaderPod -> leaderWithAnnotation) to be true, got false")
		}
		if pred.Update(event.UpdateEvent{ObjectOld: leaderPod, ObjectNew: leaderWithUnrelatedAnnotation}) {
			t.Errorf("expected Update(leaderPod -> leaderWithUnrelatedAnnotation) to be false, got true")
		}
		if pred.Update(event.UpdateEvent{ObjectOld: leaderWithAnnotation, ObjectNew: leaderWithAnnotation.DeepCopy()}) {
			t.Errorf("expected Update(leaderWithAnnotation -> leaderWithAnnotation) to be false, got true")
		}
		workerWithAnnotation := workerPod.DeepCopy()
		workerWithAnnotation.Annotations = map[string]string{
			leaderworkerset.GroupRestartBudgetExhaustedAnnotationKey: "true",
		}
		if pred.Update(event.UpdateEvent{ObjectOld: workerPod, ObjectNew: workerWithAnnotation}) {
			t.Errorf("expected Update(workerPod -> workerWithAnnotation) to be false, got true")
		}
		unschedulable := func(pod *corev1.Pod, reason string) *corev1.Pod {
			pod = pod.DeepCopy()
			pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Reason: reason}}
			return pod
		}
		if !pred.Update(event.UpdateEvent{ObjectOld: leaderPod, ObjectNew: unschedulable(leaderPod, corev1.PodReasonUnschedulable)}) {
			t.Errorf("expected Update(leaderPod -> unschedulable leaderPod) to be true, got false")
		}
		if !pred.Update(event.UpdateEvent{ObjectOld: unschedulable(leaderPod, corev1.PodReasonUnschedulable), ObjectNew: leaderPod}) {
			t.Errorf("expected Update(unschedulable leaderPod -> leaderPod) to be true, got false")
		}
		if pred.Update(event.UpdateEvent{ObjectOld: leaderPod, ObjectNew: unschedulable(leaderPod, corev1.PodReasonSchedulingGated)}) {
			t.Errorf("expected Update(leaderPod -> gated leaderPod) to be false, got true")
		}
		if pred.Update(event.UpdateEvent{ObjectOld: workerPod, ObjectNew: unschedulable(workerPod, corev1.PodReasonUnschedulable)}) {
			t.Errorf("expected Update(workerPod -> unschedulable workerPod) to be false, got true")
		}
		if pred.Update(event.UpdateEvent{ObjectOld: nil, ObjectNew: leaderPod}) {
			t.Errorf("expected Update(nil, leaderPod) to be false, got true")
		}
		if pred.Update(event.UpdateEvent{ObjectOld: leaderPod, ObjectNew: nil}) {
			t.Errorf("expected Update(leaderPod, nil) to be false, got true")
		}
	})

	t.Run("DeleteFunc", func(t *testing.T) {
		rK8s := &LeaderWorkerSetReconciler{SchedulerProvider: k8sProvider}
		predK8s := rK8s.podWatchPredicate()

		rVolcano := &LeaderWorkerSetReconciler{SchedulerProvider: volcanoProvider}
		predVolcano := rVolcano.podWatchPredicate()

		rNil := &LeaderWorkerSetReconciler{SchedulerProvider: nil}
		predNil := rNil.podWatchPredicate()

		if !predK8s.Delete(event.DeleteEvent{Object: leaderPod}) {
			t.Errorf("expected Delete(leaderPod, k8sProvider) to be true, got false")
		}
		if !predVolcano.Delete(event.DeleteEvent{Object: leaderPod}) {
			t.Errorf("expected Delete(leaderPod, volcanoProvider) to be true, got false")
		}
		if !predNil.Delete(event.DeleteEvent{Object: leaderPod}) {
			t.Errorf("expected Delete(leaderPod, nilProvider) to be true, got false")
		}

		if !predK8s.Delete(event.DeleteEvent{Object: scheduledWorkerPod}) {
			t.Errorf("expected Delete(scheduledWorkerPod, k8sProvider) to be true, got false")
		}
		if !predK8s.Delete(event.DeleteEvent{Object: annotatedWorkerPod}) {
			t.Errorf("expected Delete(annotatedWorkerPod, k8sProvider) to be true, got false")
		}
		if predK8s.Delete(event.DeleteEvent{Object: workerPod}) {
			t.Errorf("expected Delete(workerPod, k8sProvider) to be false, got true")
		}
		if predVolcano.Delete(event.DeleteEvent{Object: scheduledWorkerPod}) {
			t.Errorf("expected Delete(scheduledWorkerPod, volcanoProvider) to be false, got true")
		}
		if predNil.Delete(event.DeleteEvent{Object: scheduledWorkerPod}) {
			t.Errorf("expected Delete(scheduledWorkerPod, nilProvider) to be false, got true")
		}
		if predK8s.Delete(event.DeleteEvent{Object: nil}) {
			t.Errorf("expected Delete(nil) to be false, got true")
		}
	})

	t.Run("GenericFunc", func(t *testing.T) {
		r := &LeaderWorkerSetReconciler{SchedulerProvider: k8sProvider}
		pred := r.podWatchPredicate()

		if pred.Generic(event.GenericEvent{Object: leaderPod}) {
			t.Errorf("expected Generic(leaderPod) to be false, got true")
		}
		if pred.Generic(event.GenericEvent{Object: workerPod}) {
			t.Errorf("expected Generic(workerPod) to be false, got true")
		}
	})
}

func TestMakeConditionDegraded(t *testing.T) {
	lws := wrappers.BuildLeaderWorkerSet("default").Obj()
	condition := makeCondition(leaderworkerset.LeaderWorkerSetDegraded, lws)
	if condition.Type != string(leaderworkerset.LeaderWorkerSetDegraded) || condition.Status != metav1.ConditionTrue {
		t.Fatalf("unexpected condition: %#v", condition)
	}
	if condition.Reason != "ReplicaRestartBudgetExceeded" {
		t.Fatalf("reason = %q, want ReplicaRestartBudgetExceeded", condition.Reason)
	}
}

func TestUpdateConditionsKeepsProgressingForAnotherReplica(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := leaderworkerset.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	lws := wrappers.BuildLeaderWorkerSet("default").Replica(2).Size(1).Obj()
	degraded := wrappers.MakePodWithLabels(lws.Name, "0", "0", lws.Namespace, 1)
	degraded.Labels[leaderworkerset.RevisionKey] = "revision-a"
	degraded.Annotations[leaderworkerset.GroupRestartBudgetExhaustedAnnotationKey] = "true"
	degraded.Status.Phase = corev1.PodPending
	progressing := wrappers.MakePodWithLabels(lws.Name, "1", "0", lws.Namespace, 1)
	progressing.Labels[leaderworkerset.RevisionKey] = "revision-a"
	progressing.Status.Phase = corev1.PodPending

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(lws, degraded, progressing).Build()
	r := &LeaderWorkerSetReconciler{Client: fakeClient, Record: fakeEventRecorder{}}
	if _, _, err := r.updateConditions(context.Background(), lws, "revision-a", int(*lws.Spec.Replicas)); err != nil {
		t.Fatal(err)
	}
	var progressingCondition, degradedCondition *metav1.Condition
	for i := range lws.Status.Conditions {
		condition := &lws.Status.Conditions[i]
		switch condition.Type {
		case string(leaderworkerset.LeaderWorkerSetProgressing):
			progressingCondition = condition
		case string(leaderworkerset.LeaderWorkerSetDegraded):
			degradedCondition = condition
		}
	}
	if progressingCondition == nil || progressingCondition.Status != metav1.ConditionTrue {
		t.Fatalf("Progressing condition = %#v, want True", progressingCondition)
	}
	if degradedCondition == nil || degradedCondition.Status != metav1.ConditionTrue {
		t.Fatalf("Degraded condition = %#v, want True", degradedCondition)
	}
}

func TestUpdateConditionsReportsUnschedulableGroups(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := leaderworkerset.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	scheduled := func(status corev1.ConditionStatus, reason string) func(*corev1.Pod) {
		return func(pod *corev1.Pod) {
			pod.Status.Phase = corev1.PodPending
			pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodScheduled, Status: status, Reason: reason}}
		}
	}
	tests := []struct {
		name        string
		leaders     []func(*corev1.Pod)
		wantReason  string
		wantMessage string
	}{
		{
			name:        "an unschedulable leader explains Progressing",
			leaders:     []func(*corev1.Pod){scheduled(corev1.ConditionFalse, corev1.PodReasonUnschedulable), scheduled(corev1.ConditionTrue, "")},
			wantReason:  GroupUnschedulable,
			wantMessage: "1 replica(s) cannot be scheduled",
		},
		{
			name:        "a gated leader has not been tried by the scheduler",
			leaders:     []func(*corev1.Pod){scheduled(corev1.ConditionFalse, corev1.PodReasonSchedulingGated), scheduled(corev1.ConditionTrue, "")},
			wantReason:  GroupsProgressing,
			wantMessage: "Replicas are progressing",
		},
		{
			name:        "a placed leader that is still starting keeps the default reason",
			leaders:     []func(*corev1.Pod){scheduled(corev1.ConditionTrue, ""), scheduled(corev1.ConditionTrue, "")},
			wantReason:  GroupsProgressing,
			wantMessage: "Replicas are progressing",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			lws := wrappers.BuildLeaderWorkerSet("default").Replica(2).Size(1).Obj()
			objects := []client.Object{lws}
			for i, mutate := range tc.leaders {
				leader := wrappers.MakePodWithLabels(lws.Name, strconv.Itoa(i), "0", lws.Namespace, 1)
				leader.Labels[leaderworkerset.RevisionKey] = "revision-a"
				mutate(leader)
				objects = append(objects, leader)
			}
			fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
			r := &LeaderWorkerSetReconciler{Client: fakeClient, Record: fakeEventRecorder{}}
			if _, _, err := r.updateConditions(context.Background(), lws, "revision-a", int(*lws.Spec.Replicas)); err != nil {
				t.Fatal(err)
			}
			progressing := apimeta.FindStatusCondition(lws.Status.Conditions, string(leaderworkerset.LeaderWorkerSetProgressing))
			if progressing == nil || progressing.Status != metav1.ConditionTrue {
				t.Fatalf("Progressing condition = %#v, want True", progressing)
			}
			if progressing.Reason != tc.wantReason || progressing.Message != tc.wantMessage {
				t.Errorf("Progressing reason, message = %q, %q, want %q, %q", progressing.Reason, progressing.Message, tc.wantReason, tc.wantMessage)
			}
		})
	}
}

func TestUpdateConditionsReportsUnschedulableSurgeGroups(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := leaderworkerset.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	lws := wrappers.BuildLeaderWorkerSet("default").Replica(1).Size(1).Obj()
	unschedulable := func(index, revision string) *corev1.Pod {
		leader := wrappers.MakePodWithLabels(lws.Name, index, "0", lws.Namespace, 1)
		leader.Labels[leaderworkerset.RevisionKey] = revision
		leader.Status.Phase = corev1.PodPending
		leader.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Reason: corev1.PodReasonUnschedulable}}
		return leader
	}
	// The desired replica still runs the old revision, so a rolling update is in progress.
	desired := wrappers.MakePodWithLabels(lws.Name, "0", "0", lws.Namespace, 1)
	desired.Labels[leaderworkerset.RevisionKey] = "revision-old"
	desired.Status.Phase = corev1.PodRunning
	desired.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
	surge := unschedulable("1", "revision-a")
	// A leader past the leader StatefulSet's replicas is about to be removed.
	scaledDown := unschedulable("2", "revision-a")

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(lws, desired, surge, scaledDown).Build()
	r := &LeaderWorkerSetReconciler{Client: fakeClient, Record: fakeEventRecorder{}}
	if _, _, err := r.updateConditions(context.Background(), lws, "revision-a", 2); err != nil {
		t.Fatal(err)
	}
	progressing := apimeta.FindStatusCondition(lws.Status.Conditions, string(leaderworkerset.LeaderWorkerSetProgressing))
	if progressing == nil || progressing.Status != metav1.ConditionTrue {
		t.Fatalf("Progressing condition = %#v, want True", progressing)
	}
	if progressing.Reason != GroupUnschedulable || progressing.Message != "1 replica(s) cannot be scheduled" {
		t.Errorf("Progressing reason, message = %q, %q, want %q, %q", progressing.Reason, progressing.Message, GroupUnschedulable, "1 replica(s) cannot be scheduled")
	}
}

func TestConditionEvents(t *testing.T) {
	lws := wrappers.BuildLeaderWorkerSet("default").Obj()
	updating := makeCondition(leaderworkerset.LeaderWorkerSetUpdateInProgress, lws)
	progressing := makeCondition(leaderworkerset.LeaderWorkerSetProgressing, lws)
	notDegraded := makeFalseCondition(leaderworkerset.LeaderWorkerSetDegraded, lws, "AsExpected", "No replica has exhausted its restart budget")
	unschedulable := func(groups int) metav1.Condition {
		conditions := []metav1.Condition{makeCondition(leaderworkerset.LeaderWorkerSetProgressing, lws)}
		explainUnschedulableGroups(conditions, groups)
		return conditions[0]
	}
	type event struct{ reason, message, eventType string }

	tests := []struct {
		name       string
		previous   []metav1.Condition
		conditions []metav1.Condition
		want       []event
	}{
		{
			name:       "a primary condition without unschedulable groups is a normal event",
			conditions: []metav1.Condition{progressing, notDegraded},
			want:       []event{{progressing.Reason, progressing.Message, corev1.EventTypeNormal}},
		},
		{
			name:       "a primary condition with unschedulable groups is a warning",
			conditions: []metav1.Condition{unschedulable(1), notDegraded},
			want:       []event{{GroupUnschedulable, "1 replica(s) cannot be scheduled", corev1.EventTypeWarning}},
		},
		{
			name:       "a rolling update that starts with unschedulable groups reports both",
			previous:   []metav1.Condition{progressing, notDegraded},
			conditions: []metav1.Condition{updating, unschedulable(1), notDegraded},
			want: []event{
				{updating.Reason, updating.Message, corev1.EventTypeNormal},
				{GroupUnschedulable, "1 replica(s) cannot be scheduled", corev1.EventTypeWarning},
			},
		},
		{
			name:       "a new unschedulable count during a rolling update does not repeat the update event",
			previous:   []metav1.Condition{updating, unschedulable(1), notDegraded},
			conditions: []metav1.Condition{updating, unschedulable(2), notDegraded},
			want:       []event{{GroupUnschedulable, "2 replica(s) cannot be scheduled", corev1.EventTypeWarning}},
		},
		{
			name:       "an unchanged unschedulable count during a rolling update is not reported again",
			previous:   []metav1.Condition{unschedulable(1), notDegraded},
			conditions: []metav1.Condition{updating, unschedulable(1), notDegraded},
			want:       []event{{updating.Reason, updating.Message, corev1.EventTypeNormal}},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var got []event
			for _, e := range conditionEvents(tc.previous, tc.conditions, false) {
				got = append(got, event{e.condition.Reason, e.condition.Message, e.eventType})
			}
			if diff := cmp.Diff(tc.want, got, cmp.AllowUnexported(event{})); diff != "" {
				t.Errorf("conditionEvents() mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestSetConditionsClearsUnschedulableReasonWhenAvailable(t *testing.T) {
	lws := wrappers.BuildLeaderWorkerSet("default").Obj()
	conditions := []metav1.Condition{makeCondition(leaderworkerset.LeaderWorkerSetProgressing, lws)}
	explainUnschedulableGroups(conditions, 1)
	lws.Status.Conditions = conditions

	setConditions(lws, []metav1.Condition{makeCondition(leaderworkerset.LeaderWorkerSetAvailable, lws)})

	got := apimeta.FindStatusCondition(lws.Status.Conditions, string(leaderworkerset.LeaderWorkerSetProgressing))
	if got == nil || got.Status != metav1.ConditionFalse {
		t.Fatalf("Progressing condition = %#v, want False", got)
	}
	if got.Reason != GroupsProgressing || got.Message != "Replicas are progressing" {
		t.Errorf("Progressing reason, message = %q, %q, want the defaults", got.Reason, got.Message)
	}
}

func TestUpdateConditionsReportsDegradedWithoutWorkerStatefulSet(t *testing.T) {
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
	lws := wrappers.BuildLeaderWorkerSet("default").Replica(1).Size(2).Obj()
	leader := wrappers.MakePodWithLabels(lws.Name, "0", "0", lws.Namespace, 2)
	leader.Labels[leaderworkerset.RevisionKey] = "revision-a"
	leader.Annotations[leaderworkerset.GroupRestartBudgetExhaustedAnnotationKey] = "true"

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(lws, leader).Build()
	r := &LeaderWorkerSetReconciler{Client: fakeClient, Record: fakeEventRecorder{}}
	if _, _, err := r.updateConditions(context.Background(), lws, "revision-a", int(*lws.Spec.Replicas)); err != nil {
		t.Fatal(err)
	}
	condition := apimeta.FindStatusCondition(lws.Status.Conditions, string(leaderworkerset.LeaderWorkerSetDegraded))
	if condition == nil || condition.Status != metav1.ConditionTrue {
		t.Fatalf("Degraded condition = %#v, want True", condition)
	}
}

func TestUpdateConditionsDoesNotReportDegradedReplicaAvailable(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := leaderworkerset.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	lws := wrappers.BuildLeaderWorkerSet("default").Replica(1).Size(1).Obj()
	leader := wrappers.MakePodWithLabels(lws.Name, "0", "0", lws.Namespace, 1)
	leader.Labels[leaderworkerset.RevisionKey] = "revision-a"
	leader.Annotations[leaderworkerset.GroupRestartBudgetExhaustedAnnotationKey] = "true"
	leader.Status.Phase = corev1.PodRunning
	leader.Status.Conditions = []corev1.PodCondition{{
		Type:   corev1.PodReady,
		Status: corev1.ConditionTrue,
	}}

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(lws, leader).Build()
	r := &LeaderWorkerSetReconciler{Client: fakeClient, Record: fakeEventRecorder{}}
	if _, _, err := r.updateConditions(context.Background(), lws, "revision-a", int(*lws.Spec.Replicas)); err != nil {
		t.Fatal(err)
	}
	available := apimeta.FindStatusCondition(lws.Status.Conditions, string(leaderworkerset.LeaderWorkerSetAvailable))
	if available == nil || available.Status != metav1.ConditionFalse {
		t.Fatalf("Available condition = %#v, want False", available)
	}
	degraded := apimeta.FindStatusCondition(lws.Status.Conditions, string(leaderworkerset.LeaderWorkerSetDegraded))
	if degraded == nil || degraded.Status != metav1.ConditionTrue {
		t.Fatalf("Degraded condition = %#v, want True", degraded)
	}
}

func TestUpdateConditionsReportsActiveSurgeReplicaDegraded(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := leaderworkerset.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	lws := wrappers.BuildLeaderWorkerSet("default").Replica(1).Size(1).Obj()
	desired := wrappers.MakePodWithLabels(lws.Name, "0", "0", lws.Namespace, 1)
	desired.Labels[leaderworkerset.RevisionKey] = "revision-a"
	desired.Status.Phase = corev1.PodRunning
	desired.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
	surge := wrappers.MakePodWithLabels(lws.Name, "1", "0", lws.Namespace, 1)
	surge.Labels[leaderworkerset.RevisionKey] = "revision-a"
	surge.Annotations[leaderworkerset.GroupRestartBudgetExhaustedAnnotationKey] = "true"

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(lws, desired, surge).Build()
	r := &LeaderWorkerSetReconciler{Client: fakeClient, Record: fakeEventRecorder{}}
	if _, _, err := r.updateConditions(context.Background(), lws, "revision-a", int(*lws.Spec.Replicas)); err != nil {
		t.Fatal(err)
	}
	degraded := apimeta.FindStatusCondition(lws.Status.Conditions, string(leaderworkerset.LeaderWorkerSetDegraded))
	if degraded == nil || degraded.Status != metav1.ConditionTrue {
		t.Fatalf("Degraded condition = %#v, want True", degraded)
	}
}

func TestCleanupObsoleteGroupRestartCounts(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := leaderworkerset.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	lws := wrappers.BuildLeaderWorkerSet("default").Obj()
	lws.Annotations = map[string]string{
		leaderworkerset.GroupRestartCountsAnnotationKey: `{"old-revision/0":1,"revision-a/0":2,"revision-a/1":3}`,
	}
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(lws).Build()
	r := &LeaderWorkerSetReconciler{Client: fakeClient}
	if err := r.cleanupObsoleteGroupRestartCounts(context.Background(), lws, "revision-a"); err != nil {
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
	if len(counts) != 2 || counts["revision-a/0"] != 2 || counts["revision-a/1"] != 3 {
		t.Fatalf("unexpected counts after revision cleanup: %#v", counts)
	}
}

func TestSetConditionsAppliesMixedStatusConditions(t *testing.T) {
	lws := wrappers.BuildBasicLeaderWorkerSet("test-sample", "default").Generation(1).Obj()
	conditions := []metav1.Condition{
		makeFalseCondition(leaderworkerset.LeaderWorkerSetProgressing, lws, "ReplicaRestartBudgetExceeded", "Automatic recovery stopped"),
		makeCondition(leaderworkerset.LeaderWorkerSetDegraded, lws),
	}
	if !setConditions(lws, conditions) {
		t.Fatal("setConditions() should report an update")
	}
	if len(lws.Status.Conditions) != 2 {
		t.Fatalf("condition count = %d, want 2", len(lws.Status.Conditions))
	}
	if lws.Status.Conditions[0].Status != metav1.ConditionFalse || lws.Status.Conditions[1].Status != metav1.ConditionTrue {
		t.Fatalf("unexpected conditions: %#v", lws.Status.Conditions)
	}
}

func TestPruneScaledDownGroupRestartCounts(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := leaderworkerset.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	lws := wrappers.BuildLeaderWorkerSet("default").Replica(1).Obj()
	lws.Annotations = map[string]string{
		leaderworkerset.GroupRestartCountsAnnotationKey: `{"revision-a/0":1,"revision-a/1":2}`,
	}
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(lws).Build()
	r := &LeaderWorkerSetReconciler{Client: fakeClient}
	if err := r.pruneScaledDownGroupRestartCounts(context.Background(), lws, 1); err != nil {
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
	if counts["revision-a/0"] != 1 {
		t.Fatalf("unexpected counts after scale-down: %#v", counts)
	}
	if _, found := counts["revision-a/1"]; found {
		t.Fatalf("scaled-down replica count was not removed: %#v", counts)
	}
}

func TestPruneScaledDownGroupRestartCountsPreservesActiveSurgeReplica(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := leaderworkerset.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	lws := wrappers.BuildLeaderWorkerSet("default").Replica(1).Obj()
	lws.Annotations = map[string]string{
		leaderworkerset.GroupRestartCountsAnnotationKey: `{"revision-a/0":1,"revision-a/1":2,"revision-a/2":3}`,
	}
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(lws).Build()
	r := &LeaderWorkerSetReconciler{Client: fakeClient}
	if err := r.pruneScaledDownGroupRestartCounts(context.Background(), lws, 2); err != nil {
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
	if counts["revision-a/0"] != 1 || counts["revision-a/1"] != 2 {
		t.Fatalf("active replica counts were removed: %#v", counts)
	}
	if _, found := counts["revision-a/2"]; found {
		t.Fatalf("inactive replica count was not removed: %#v", counts)
	}
}

func TestCalculateContinuousReadyReplicas(t *testing.T) {
	tests := []struct {
		name   string
		states []replicaState
		want   int32
	}{
		{
			name:   "no replicas",
			states: nil,
			want:   0,
		},
		{
			name: "all replicas ready and updated",
			states: []replicaState{
				{ready: true, updated: true},
				{ready: true, updated: true},
				{ready: true, updated: true},
			},
			want: 3,
		},
		{
			name: "counting stops at the first replica that is not updated",
			states: []replicaState{
				{ready: true, updated: true},
				{ready: true, updated: false},
				{ready: true, updated: true},
			},
			want: 1,
		},
		{
			name: "an updated but not ready tail replica counts as zero",
			states: []replicaState{
				{ready: true, updated: true},
				{ready: true, updated: true},
				{ready: false, updated: true},
			},
			want: 0,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := calculateContinuousReadyReplicas(tc.states)
			if got != tc.want {
				t.Fatalf("calculateContinuousReadyReplicas()=%d, want %d", got, tc.want)
			}
		})
	}
}

func TestCalculateLWSUnreadyReplicas(t *testing.T) {
	tests := []struct {
		name        string
		states      []replicaState
		lwsReplicas int32
		want        int32
	}{
		{
			name:        "all desired replicas ready and updated",
			states:      []replicaState{{ready: true, updated: true}, {ready: true, updated: true}},
			lwsReplicas: 2,
			want:        0,
		},
		{
			name:        "ready but not updated replicas count as unready",
			states:      []replicaState{{ready: true, updated: true}, {ready: true, updated: false}},
			lwsReplicas: 2,
			want:        1,
		},
		{
			name:        "replicas without a state yet count as unready",
			states:      []replicaState{{ready: true, updated: true}, {ready: true, updated: false}},
			lwsReplicas: 4,
			want:        3,
		},
		{
			name:        "surge replicas beyond the desired count are ignored",
			states:      []replicaState{{ready: true, updated: true}, {ready: false, updated: false}, {ready: false, updated: false}},
			lwsReplicas: 1,
			want:        0,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := calculateLWSUnreadyReplicas(tc.states, tc.lwsReplicas)
			if got != tc.want {
				t.Fatalf("calculateLWSUnreadyReplicas()=%d, want %d", got, tc.want)
			}
		})
	}
}

func TestRollingUpdatePartition(t *testing.T) {
	ru := replicaState{ready: true, updated: true}
	rn := replicaState{ready: true, updated: false}
	nn := replicaState{ready: false, updated: false}

	tests := []struct {
		name             string
		states           []replicaState
		rollingStep      int32
		currentPartition int32
		want             int32
	}{
		{
			name:             "all replicas ready and updated drops the partition to zero",
			states:           []replicaState{ru, ru, ru, ru},
			rollingStep:      2,
			currentPartition: 4,
			want:             0,
		},
		{
			name:             "rollout start updates rollingStep replicas from the tail",
			states:           []replicaState{rn, rn, rn, rn},
			rollingStep:      1,
			currentPartition: 4,
			want:             3,
		},
		{
			name:             "partition advances by rollingStep once the tail replica is ready and updated",
			states:           []replicaState{rn, rn, rn, ru},
			rollingStep:      1,
			currentPartition: 3,
			want:             2,
		},
		{
			name:             "a not ready replica below the update window holds the partition",
			states:           []replicaState{nn, rn, rn, ru},
			rollingStep:      1,
			currentPartition: 3,
			want:             3,
		},
		{
			name:             "a not ready replica at the window edge is still updated so the rollout cannot get stuck",
			states:           []replicaState{nn, rn, nn, ru},
			rollingStep:      1,
			currentPartition: 3,
			want:             2,
		},
		{
			name:             "partition never moves back up",
			states:           []replicaState{rn, rn, rn, rn},
			rollingStep:      1,
			currentPartition: 1,
			want:             1,
		},
		{
			name:             "rollingStep larger than the replica count updates everything at once",
			states:           []replicaState{rn, rn, rn},
			rollingStep:      5,
			currentPartition: 3,
			want:             0,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := rollingUpdatePartition(tc.states, int32(len(tc.states)), tc.rollingStep, tc.currentPartition)
			if got != tc.want {
				t.Fatalf("rollingUpdatePartition()=%d, want %d", got, tc.want)
			}
		})
	}
}

func TestPodTerminationPolicyApplyConfigAndDeletion(t *testing.T) {
	ctx := context.TODO()

	t.Run("parallel policy annotates the leader pod template", func(t *testing.T) {
		lws := wrappers.BuildBasicLeaderWorkerSet("test-sample", "default").
			WorkerTemplateSpec(wrappers.MakeWorkerPodSpec()).
			Size(2).
			Obj()
		lws.Spec.PodTerminationPolicy = leaderworkerset.ParallelPodTerminationPolicy

		template, err := buildLeaderPodTemplateApplyConfiguration(lws, "rev-1")
		if err != nil {
			t.Fatalf("buildLeaderPodTemplateApplyConfiguration() failed: %v", err)
		}
		if template.Annotations[leaderworkerset.PodTerminationPolicyAnnotationKey] != string(leaderworkerset.ParallelPodTerminationPolicy) {
			t.Errorf("expected annotation %s to be %s, got %s",
				leaderworkerset.PodTerminationPolicyAnnotationKey,
				leaderworkerset.ParallelPodTerminationPolicy,
				template.Annotations[leaderworkerset.PodTerminationPolicyAnnotationKey])
		}
	})

	t.Run("sequential and empty policies do not annotate the leader pod template", func(t *testing.T) {
		for _, policy := range []leaderworkerset.PodTerminationPolicyType{"", leaderworkerset.SequentialPodTerminationPolicy} {
			lws := wrappers.BuildBasicLeaderWorkerSet("test-sample", "default").
				WorkerTemplateSpec(wrappers.MakeWorkerPodSpec()).
				Size(2).
				Obj()
			lws.Spec.PodTerminationPolicy = policy

			template, err := buildLeaderPodTemplateApplyConfiguration(lws, "rev-1")
			if err != nil {
				t.Fatalf("buildLeaderPodTemplateApplyConfiguration() failed for policy %q: %v", policy, err)
			}
			if _, ok := template.Annotations[leaderworkerset.PodTerminationPolicyAnnotationKey]; ok {
				t.Errorf("policy %q should not set %s, got %s",
					policy,
					leaderworkerset.PodTerminationPolicyAnnotationKey,
					template.Annotations[leaderworkerset.PodTerminationPolicyAnnotationKey])
			}
		}
	})

	t.Run("lws deletion with Parallel policy deletes worker statefulsets", func(t *testing.T) {
		scheme := runtime.NewScheme()
		_ = leaderworkerset.AddToScheme(scheme)
		_ = appsv1.AddToScheme(scheme)
		_ = corev1.AddToScheme(scheme)

		now := metav1.Now()
		lws := wrappers.BuildBasicLeaderWorkerSet("test-sample", "default").
			WorkerTemplateSpec(wrappers.MakeWorkerPodSpec()).
			Size(2).
			Obj()
		lws.Finalizers = []string{"test-finalizer"}
		lws.DeletionTimestamp = &now
		lws.Spec.PodTerminationPolicy = leaderworkerset.ParallelPodTerminationPolicy
		lws.UID = "lws"
		leaders := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{
			Name: lws.Name, Namespace: lws.Namespace, UID: "leaders",
			OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(lws, leaderworkerset.GroupVersion.WithKind("LeaderWorkerSet"))},
		}}
		objects := []client.Object{lws, leaders}
		for i := range 2 {
			name := fmt.Sprintf("%s-%d", lws.Name, i)
			leader := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
				Name: name, Namespace: lws.Namespace, UID: types.UID(name),
				OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(leaders, appsv1.SchemeGroupVersion.WithKind("StatefulSet"))},
			}}
			workers := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{
				Name: name, Namespace: lws.Namespace, UID: types.UID(name + "-workers"),
				Labels:          map[string]string{leaderworkerset.SetNameLabelKey: lws.Name, leaderworkerset.RoleLabelKey: leaderworkerset.RoleWorker},
				OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(leader, corev1.SchemeGroupVersion.WithKind("Pod"))},
			}}
			objects = append(objects, leader, workers)
		}

		fakeClient := fake.NewClientBuilder().
			WithScheme(scheme).
			WithObjects(objects...).
			Build()

		reconciler := NewLeaderWorkerSetReconciler(fakeClient, scheme, events.NewFakeRecorder(10), nil)
		_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: lws.Name, Namespace: lws.Namespace}})
		if err != nil {
			t.Fatalf("Reconcile() error = %v", err)
		}

		var stsList appsv1.StatefulSetList
		if err := fakeClient.List(ctx, &stsList, client.InNamespace("default"), client.MatchingLabels{leaderworkerset.RoleLabelKey: leaderworkerset.RoleWorker}); err != nil {
			t.Fatalf("List() error = %v", err)
		}
		if len(stsList.Items) != 0 {
			t.Errorf("expected 0 worker statefulsets after parallel deletion, got %d", len(stsList.Items))
		}
	})

	t.Run("lws deletion with Sequential policy does not delete worker statefulsets directly", func(t *testing.T) {
		scheme := runtime.NewScheme()
		_ = leaderworkerset.AddToScheme(scheme)
		_ = appsv1.AddToScheme(scheme)
		_ = corev1.AddToScheme(scheme)

		now := metav1.Now()
		lws := wrappers.BuildBasicLeaderWorkerSet("test-sample", "default").
			WorkerTemplateSpec(wrappers.MakeWorkerPodSpec()).
			Size(2).
			Obj()
		lws.Finalizers = []string{"test-finalizer"}
		lws.DeletionTimestamp = &now
		lws.Spec.PodTerminationPolicy = leaderworkerset.SequentialPodTerminationPolicy

		workerSts := &appsv1.StatefulSet{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-sample-0",
				Namespace: "default",
				Labels: map[string]string{
					leaderworkerset.SetNameLabelKey: "test-sample",
					leaderworkerset.RoleLabelKey:    leaderworkerset.RoleWorker,
				},
			},
		}

		fakeClient := fake.NewClientBuilder().
			WithScheme(scheme).
			WithObjects(lws, workerSts).
			Build()

		reconciler := NewLeaderWorkerSetReconciler(fakeClient, scheme, events.NewFakeRecorder(10), nil)
		_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: lws.Name, Namespace: lws.Namespace}})
		if err != nil {
			t.Fatalf("Reconcile() error = %v", err)
		}

		var stsList appsv1.StatefulSetList
		if err := fakeClient.List(ctx, &stsList, client.InNamespace("default")); err != nil {
			t.Fatalf("List() error = %v", err)
		}
		if len(stsList.Items) != 1 {
			t.Errorf("expected worker statefulset to be retained under Sequential policy, got %d", len(stsList.Items))
		}
	})
}

func TestLeaderPodTemplateExclusivePlacement(t *testing.T) {
	annotations := map[string]string{
		leaderworkerset.ExclusiveKeyAnnotationKey:         "zone",
		leaderworkerset.SubGroupExclusiveKeyAnnotationKey: "rack",
	}
	tests := []struct {
		name            string
		annotations     map[string]string
		policy          *leaderworkerset.PlacementPolicy
		wantGroupKey    string
		wantSubGroupKey string
	}{
		{
			name: "placementPolicy",
			policy: &leaderworkerset.PlacementPolicy{Type: leaderworkerset.PlacementExclusiveTopology, Topology: "block",
				SubGroup: &leaderworkerset.SubGroupPlacementPolicy{Type: leaderworkerset.PlacementExclusiveTopology, Topology: "host"}},
			wantGroupKey:    "block",
			wantSubGroupKey: "host",
		},
		{
			name:        "placementPolicy takes precedence over the annotations",
			annotations: annotations,
			policy: &leaderworkerset.PlacementPolicy{Type: leaderworkerset.PlacementExclusiveTopology, Topology: "block",
				SubGroup: &leaderworkerset.SubGroupPlacementPolicy{Type: leaderworkerset.PlacementExclusiveTopology, Topology: "host"}},
			wantGroupKey:    "block",
			wantSubGroupKey: "host",
		},
		{
			name:        "a None group level falls back to the annotation",
			annotations: annotations,
			policy: &leaderworkerset.PlacementPolicy{Type: leaderworkerset.PlacementNone,
				SubGroup: &leaderworkerset.SubGroupPlacementPolicy{Type: leaderworkerset.PlacementExclusiveTopology, Topology: "rack"}},
			wantGroupKey:    "zone",
			wantSubGroupKey: "rack",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			lws := wrappers.BuildLeaderWorkerSet("default").Size(4).SubGroupSize(2).
				SubGroupType(leaderworkerset.SubGroupPolicyTypeLeaderWorker).Annotation(tc.annotations).Obj()
			lws.Spec.PlacementPolicy = tc.policy
			podTemplate, err := buildLeaderPodTemplateApplyConfiguration(lws, "revision-1")
			if err != nil {
				t.Fatalf("failed with error: %s", err.Error())
			}
			if got := podTemplate.Annotations[leaderworkerset.ExclusiveKeyAnnotationKey]; got != tc.wantGroupKey {
				t.Errorf("exclusive-topology annotation = %q, want %q", got, tc.wantGroupKey)
			}
			if got := podTemplate.Annotations[leaderworkerset.SubGroupExclusiveKeyAnnotationKey]; got != tc.wantSubGroupKey {
				t.Errorf("subgroup-exclusive-topology annotation = %q, want %q", got, tc.wantSubGroupKey)
			}
		})
	}
}

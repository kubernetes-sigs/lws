/*
Copyright 2023 The Kubernetes Authors.
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
	"encoding/json"
	"fmt"
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	autoscalingv1 "k8s.io/api/autoscaling/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	leaderworkerset "sigs.k8s.io/lws/api/leaderworkerset/v1"
	"sigs.k8s.io/lws/pkg/controllers"
	"sigs.k8s.io/lws/pkg/schedulerprovider"
	revisionutils "sigs.k8s.io/lws/pkg/utils/revision"
	testing "sigs.k8s.io/lws/test/testutils"
	"sigs.k8s.io/lws/test/wrappers"
)

var _ = ginkgo.Describe("LeaderWorkerSet controller", func() {
	// class representing an update
	type update struct {
		lwsUpdateFn       func(*leaderworkerset.LeaderWorkerSet)
		checkLWSState     func(*leaderworkerset.LeaderWorkerSet)
		checkLWSCondition func(context.Context, client.Client, *leaderworkerset.LeaderWorkerSet, time.Duration)
	}
	// class representing a testCase
	type testCase struct {
		makeLeaderWorkerSet func(nsName string) *wrappers.LeaderWorkerSetWrapper
		updates             []*update
	}
	ginkgo.DescribeTable("leaderWorkerSet creating or updating",
		func(tc *testCase) {
			ctx := context.Background()
			// Create test namespace for each entry.
			ns := &corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					GenerateName: "lws-ns-",
				},
			}
			gomega.Expect(k8sClient.Create(ctx, ns)).To(gomega.Succeed())
			// Create LeaderWorkerSet
			lws := tc.makeLeaderWorkerSet(ns.Name).Obj()
			// Verify LeaderWorkerSet created successfully.
			ginkgo.By(fmt.Sprintf("creating LeaderWorkerSet %s", lws.Name))
			gomega.Expect(k8sClient.Create(ctx, lws)).To(gomega.Succeed())
			var leaderSts appsv1.StatefulSet
			testing.GetLeaderStatefulset(ctx, lws, k8sClient, &leaderSts)
			// create leader pods for lws controller
			gomega.Expect(testing.CreateLeaderPods(ctx, leaderSts, k8sClient, lws, 0, int(*lws.Spec.Replicas))).To(gomega.Succeed())
			// Perform a series of updates to LeaderWorkerSet resources and check
			// resulting LeaderWorkerSet state after each update.
			for _, up := range tc.updates {
				var leaderWorkerSet leaderworkerset.LeaderWorkerSet
				gomega.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: lws.Name, Namespace: lws.Namespace}, &leaderWorkerSet)).To(gomega.Succeed())
				if up.lwsUpdateFn != nil {
					up.lwsUpdateFn(&leaderWorkerSet)
				}

				// after update, get the latest lws
				gomega.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: lws.Name, Namespace: lws.Namespace}, &leaderWorkerSet)).To(gomega.Succeed())
				if up.checkLWSState != nil {
					up.checkLWSState(&leaderWorkerSet)
				}
				if up.checkLWSCondition != nil {
					up.checkLWSCondition(ctx, k8sClient, &leaderWorkerSet, testing.Timeout)
				}
			}
		},
		ginkgo.Entry("scale up number of groups", &testCase{
			makeLeaderWorkerSet: func(nsName string) *wrappers.LeaderWorkerSetWrapper {
				return wrappers.BuildLeaderWorkerSet(nsName).Replica(2)
			},
			updates: []*update{
				{
					lwsUpdateFn: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.UpdateReplicaCount(ctx, k8sClient, lws, int32(3))
						var leaderSts appsv1.StatefulSet
						gomega.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: lws.Name, Namespace: lws.Namespace}, &leaderSts)).To(gomega.Succeed())
						gomega.Expect(testing.CreateLeaderPods(ctx, leaderSts, k8sClient, lws, 2, 3)).To(gomega.Succeed())
					},
					checkLWSState: func(deployment *leaderworkerset.LeaderWorkerSet) {
						testing.ExpectValidLeaderStatefulSet(ctx, k8sClient, deployment, 3)
						testing.ExpectValidWorkerStatefulSets(ctx, deployment, k8sClient, true)
					},
				},
			},
		}),
		ginkgo.Entry("scale down number of groups", &testCase{
			makeLeaderWorkerSet: func(nsName string) *wrappers.LeaderWorkerSetWrapper {
				return wrappers.BuildLeaderWorkerSet(nsName).Replica(4)
			},
			updates: []*update{
				{
					lwsUpdateFn: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.UpdateReplicaCount(ctx, k8sClient, lws, int32(3))
						var leaderSts appsv1.StatefulSet
						gomega.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: lws.Name, Namespace: lws.Namespace}, &leaderSts)).To(gomega.Succeed())
						testing.DeleteLeaderPods(ctx, k8sClient, lws)
					},
					checkLWSState: func(deployment *leaderworkerset.LeaderWorkerSet) {
						testing.ExpectValidLeaderStatefulSet(ctx, k8sClient, deployment, 3)
						testing.ExpectValidWorkerStatefulSets(ctx, deployment, k8sClient, true)
					},
				},
			},
		}),
		ginkgo.Entry("scale down to 0", &testCase{
			makeLeaderWorkerSet: func(nsName string) *wrappers.LeaderWorkerSetWrapper {
				return wrappers.BuildLeaderWorkerSet(nsName).Replica(2)
			},
			updates: []*update{
				{
					lwsUpdateFn: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.UpdateReplicaCount(ctx, k8sClient, lws, int32(0))
						var leaderSts appsv1.StatefulSet
						gomega.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: lws.Name, Namespace: lws.Namespace}, &leaderSts)).To(gomega.Succeed())
						testing.DeleteLeaderPods(ctx, k8sClient, lws)
					},
					checkLWSState: func(deployment *leaderworkerset.LeaderWorkerSet) {
						testing.ExpectValidLeaderStatefulSet(ctx, k8sClient, deployment, 0)
						testing.ExpectValidWorkerStatefulSets(ctx, deployment, k8sClient, true)
					},
				},
			},
		}),
		ginkgo.Entry("scale up from 0", &testCase{
			makeLeaderWorkerSet: func(nsName string) *wrappers.LeaderWorkerSetWrapper {
				return wrappers.BuildLeaderWorkerSet(nsName).Replica(0)
			},
			updates: []*update{
				{
					lwsUpdateFn: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.UpdateReplicaCount(ctx, k8sClient, lws, int32(3))
						var leaderSts appsv1.StatefulSet
						gomega.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: lws.Name, Namespace: lws.Namespace}, &leaderSts)).To(gomega.Succeed())
						gomega.Expect(testing.CreateLeaderPods(ctx, leaderSts, k8sClient, lws, 0, 3)).To(gomega.Succeed())
					},
					checkLWSState: func(deployment *leaderworkerset.LeaderWorkerSet) {
						testing.ExpectValidLeaderStatefulSet(ctx, k8sClient, deployment, 3)
						testing.ExpectValidWorkerStatefulSets(ctx, deployment, k8sClient, true)
					},
				},
			},
		}),
		ginkgo.Entry("group size is 1", &testCase{
			makeLeaderWorkerSet: func(nsName string) *wrappers.LeaderWorkerSetWrapper {
				return wrappers.BuildLeaderWorkerSet(nsName).Size(1)
			},
			updates: []*update{
				{
					checkLWSState: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.ExpectValidLeaderStatefulSet(ctx, k8sClient, lws, 2)
						testing.ExpectValidWorkerStatefulSets(ctx, lws, k8sClient, true)
					},
				},
				{
					lwsUpdateFn: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.SetLeaderPodsToReady(ctx, k8sClient, lws, 0, int(*lws.Spec.Replicas))
					},
					checkLWSState: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.ExpectLeaderWorkerSetAvailable(ctx, k8sClient, lws, "All replicas are ready")
					},
				},
			},
		}),
		ginkgo.Entry("zero replicas", &testCase{
			makeLeaderWorkerSet: func(nsName string) *wrappers.LeaderWorkerSetWrapper {
				return wrappers.BuildLeaderWorkerSet(nsName).Replica(0)
			},
			updates: []*update{
				{
					checkLWSState: func(deployment *leaderworkerset.LeaderWorkerSet) {
						testing.ExpectValidLeaderStatefulSet(ctx, k8sClient, deployment, 0)
						testing.ExpectValidWorkerStatefulSets(ctx, deployment, k8sClient, true)
					},
				},
			},
		}),
		ginkgo.Entry("Successfully create a leaderworkerset with 2 groups, size as 4.", &testCase{
			makeLeaderWorkerSet: wrappers.BuildLeaderWorkerSet,
			updates: []*update{
				{
					checkLWSState: func(deployment *leaderworkerset.LeaderWorkerSet) {
						testing.ExpectLeaderSetExist(ctx, deployment, k8sClient)
						testing.ValidateEvent(ctx, k8sClient, controllers.GroupsProgressing, corev1.EventTypeNormal, "Created leader statefulset test-sample", deployment.Namespace)
						testing.ValidateEvent(ctx, k8sClient, controllers.GroupsProgressing, corev1.EventTypeNormal, "Created worker statefulset for leader pod test-sample-0", deployment.Namespace)
						testing.ValidateEvent(ctx, k8sClient, controllers.GroupsProgressing, corev1.EventTypeNormal, "Created worker statefulset for leader pod test-sample-1", deployment.Namespace)
						testing.ExpectValidLeaderStatefulSet(ctx, k8sClient, deployment, 2)
						testing.ExpectValidWorkerStatefulSets(ctx, deployment, k8sClient, true)
					},
				},
			},
		}),
		ginkgo.Entry("Deleted worker statefulSet will be recreated", &testCase{
			makeLeaderWorkerSet: func(nsName string) *wrappers.LeaderWorkerSetWrapper {
				return wrappers.BuildLeaderWorkerSet(nsName).Replica(2)
			},
			updates: []*update{
				{
					// First, wait for the worker StatefulSet to be created
					checkLWSState: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.ExpectValidLeaderStatefulSet(ctx, k8sClient, lws, 2)
						testing.ExpectValidWorkerStatefulSets(ctx, lws, k8sClient, true)
					},
				},
				{
					// Delete one worker StatefulSet and verify it is recreated
					lwsUpdateFn: func(lws *leaderworkerset.LeaderWorkerSet) {
						var statefulsetToDelete appsv1.StatefulSet
						gomega.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: lws.Name + "-0", Namespace: lws.Namespace}, &statefulsetToDelete)).To(gomega.Succeed())
						gomega.Expect(k8sClient.Delete(ctx, &statefulsetToDelete)).To(gomega.Succeed())
					},
					checkLWSState: func(deployment *leaderworkerset.LeaderWorkerSet) {
						testing.ExpectValidLeaderStatefulSet(ctx, k8sClient, deployment, 2)
						testing.ExpectValidWorkerStatefulSets(ctx, deployment, k8sClient, true)
					},
				},
			},
		}),
		ginkgo.Entry("headless service created", &testCase{
			makeLeaderWorkerSet: wrappers.BuildLeaderWorkerSet,
			updates: []*update{
				{
					checkLWSState: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.ExpectValidServices(ctx, k8sClient, lws, 1)
					},
				},
			},
		}),
		ginkgo.Entry("service deleted will be recreated", &testCase{
			makeLeaderWorkerSet: wrappers.BuildLeaderWorkerSet,
			updates: []*update{
				{
					checkLWSState: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.ExpectValidServices(ctx, k8sClient, lws, 1)
					},
				},
				{
					// Fetch the headless service and force delete it, so we can test if it is recreated.
					lwsUpdateFn: func(lws *leaderworkerset.LeaderWorkerSet) {
						var service corev1.Service
						gomega.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: lws.Name, Namespace: lws.Namespace}, &service)).To(gomega.Succeed())
						gomega.Expect(k8sClient.Delete(ctx, &service)).To(gomega.Succeed())
					},
					// Service should be recreated during reconciliation
					checkLWSState: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.ExpectValidServices(ctx, k8sClient, lws, 1)
					},
				},
			},
		}),
		ginkgo.Entry("subdomain policy LeadersSharedWorkersDedicated, more than one headless service created", &testCase{
			makeLeaderWorkerSet: func(nsName string) *wrappers.LeaderWorkerSetWrapper {
				return wrappers.BuildLeaderWorkerSet(nsName).SubdomainPolicy(leaderworkerset.SubdomainUniquePerReplica)
			},
			updates: []*update{
				{
					checkLWSState: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.ExpectValidServices(ctx, k8sClient, lws, 2)
					},
				},
			},
		}),
		ginkgo.Entry("leader statefulset deleted will be recreated", &testCase{
			makeLeaderWorkerSet: wrappers.BuildLeaderWorkerSet,
			updates: []*update{
				{
					// Fetch the headless service and force delete it, so we can test if it is recreated.
					lwsUpdateFn: func(lws *leaderworkerset.LeaderWorkerSet) {
						var leaderSts appsv1.StatefulSet
						gomega.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: lws.Name, Namespace: lws.Namespace}, &leaderSts)).To(gomega.Succeed())
						gomega.Expect(k8sClient.Delete(ctx, &leaderSts)).To(gomega.Succeed())
					},
					// Service should be recreated during reconciliation
					checkLWSState: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.ExpectValidLeaderStatefulSet(ctx, k8sClient, lws, 2)
					},
				},
			},
		}),
		ginkgo.Entry("Able to get scale subResource information", &testCase{
			makeLeaderWorkerSet: wrappers.BuildLeaderWorkerSet,
			updates: []*update{
				{
					checkLWSState: func(lws *leaderworkerset.LeaderWorkerSet) {
						var scale autoscalingv1.Scale
						gomega.Expect(k8sClient.SubResource("scale").Get(ctx, lws, &scale)).To(gomega.Succeed())
						gomega.Expect(int32(scale.Spec.Replicas)).To(gomega.Equal(*lws.Spec.Replicas))
						gomega.Expect(int32(scale.Status.Replicas)).To(gomega.Equal(lws.Status.Replicas))
						gomega.Expect(lws.Status.HPAPodSelector).To(gomega.Equal("leaderworkerset.sigs.k8s.io/name=test-sample,leaderworkerset.sigs.k8s.io/worker-index=0"))
					},
				},
			},
		}),
		ginkgo.Entry("HPA is able trigger scaling up/down through scale endpoint", &testCase{
			makeLeaderWorkerSet: wrappers.BuildLeaderWorkerSet,
			updates: []*update{
				{
					checkLWSState: func(lws *leaderworkerset.LeaderWorkerSet) {
						var scale autoscalingv1.Scale
						gomega.Expect(k8sClient.SubResource("scale").Get(ctx, lws, &scale)).To(gomega.Succeed())
						gomega.Expect(int32(scale.Spec.Replicas)).To(gomega.Equal(*lws.Spec.Replicas))
						gomega.Expect(int32(scale.Status.Replicas)).To(gomega.Equal(lws.Status.Replicas))
						gomega.Expect(lws.Status.HPAPodSelector).To(gomega.Equal("leaderworkerset.sigs.k8s.io/name=test-sample,leaderworkerset.sigs.k8s.io/worker-index=0"))
					},
				},
				{
					lwsUpdateFn: func(lws *leaderworkerset.LeaderWorkerSet) {
						dep := &leaderworkerset.LeaderWorkerSet{ObjectMeta: metav1.ObjectMeta{Namespace: lws.Namespace, Name: lws.Name}}
						scale := &autoscalingv1.Scale{Spec: autoscalingv1.ScaleSpec{Replicas: 3}}
						gomega.Expect(k8sClient.SubResource("scale").Update(ctx, dep, client.WithSubResourceBody(scale))).To(gomega.Succeed())
					},
					checkLWSState: func(lws *leaderworkerset.LeaderWorkerSet) {
						gomega.Eventually(func() (int32, error) {
							var leaderWorkerSet leaderworkerset.LeaderWorkerSet
							if err := k8sClient.Get(ctx, types.NamespacedName{Name: lws.Name, Namespace: lws.Namespace}, &leaderWorkerSet); err != nil {
								return 0, err
							}
							return *leaderWorkerSet.Spec.Replicas, nil
						}, testing.Timeout, testing.Interval).Should(gomega.Equal(int32(3)))
					},
				},
			},
		}),
		ginkgo.Entry("Test available state", &testCase{
			makeLeaderWorkerSet: wrappers.BuildLeaderWorkerSet,
			updates: []*update{
				{
					lwsUpdateFn: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.SetSuperPodToReady(ctx, k8sClient, lws, 2)
					},
					checkLWSState: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.ExpectLeaderWorkerSetAvailable(ctx, k8sClient, lws, "All replicas are ready")
					},
				},
			},
		}),
		ginkgo.Entry("Testing condition switch from progressing to available to progressing", &testCase{
			makeLeaderWorkerSet: wrappers.BuildLeaderWorkerSet,
			updates: []*update{
				{
					checkLWSState: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.ExpectLeaderWorkerSetProgressing(ctx, k8sClient, lws, "Replicas are progressing")
						testing.ValidateEvent(ctx, k8sClient, controllers.GroupsProgressing, corev1.EventTypeNormal, "Replicas are progressing, with 0 groups ready of total 2 groups", lws.Namespace)
						// Force groups to ready.
						testing.SetSuperPodToReady(ctx, k8sClient, lws, 2)
						testing.ExpectLeaderWorkerSetNotProgressing(ctx, k8sClient, lws, "Replicas are progressing")
						testing.ExpectLeaderWorkerSetAvailable(ctx, k8sClient, lws, "All replicas are ready")
						testing.ValidateEvent(ctx, k8sClient, "AllGroupsReady", corev1.EventTypeNormal, "All replicas are ready, with 2 groups ready of total 2 groups", lws.Namespace)
						// Force a reconcile. Refetch most recent version of LWS, increase replicas.
						patch := client.MergeFrom(&leaderworkerset.LeaderWorkerSet{
							ObjectMeta: metav1.ObjectMeta{
								Namespace: lws.Namespace,
								Name:      lws.Name,
							},
						})
						gomega.Expect(k8sClient.Patch(ctx, &leaderworkerset.LeaderWorkerSet{
							ObjectMeta: metav1.ObjectMeta{
								Namespace: lws.Namespace,
								Name:      lws.Name,
							},
							Spec: leaderworkerset.LeaderWorkerSetSpec{
								Replicas: ptr.To[int32](3),
							},
						}, patch)).To(gomega.Succeed())
						testing.ExpectLeaderWorkerSetProgressing(ctx, k8sClient, lws, "Replicas are progressing")
						testing.ExpectLeaderWorkerSetUnavailable(ctx, k8sClient, lws, "All replicas are ready")
						// Check most recent event.
						testing.ValidateEvent(ctx, k8sClient, controllers.GroupsProgressing, corev1.EventTypeNormal, "Replicas are progressing, with 2 groups ready of total 3 groups", lws.Namespace)
					},
				},
			},
		}),
		ginkgo.Entry("Leader/worker pods/Statefulset have required labels and annotations when exclusive placement enabled", &testCase{
			makeLeaderWorkerSet: func(nsName string) *wrappers.LeaderWorkerSetWrapper {
				return wrappers.BuildLeaderWorkerSet(nsName).ExclusivePlacement()
			},
			updates: []*update{
				{
					checkLWSState: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.ExpectValidLeaderStatefulSet(ctx, k8sClient, lws, 2)
						testing.ExpectValidWorkerStatefulSets(ctx, lws, k8sClient, false)
					},
				},
			},
		}),
		ginkgo.Entry("Pod restart will not recreate the pod group when restart policy is None", &testCase{
			makeLeaderWorkerSet: func(nsName string) *wrappers.LeaderWorkerSetWrapper {
				return wrappers.BuildLeaderWorkerSet(nsName).RestartPolicy(leaderworkerset.NoneRestartPolicy).Replica(1).Size(3)
			},
			updates: []*update{
				{
					lwsUpdateFn: func(lws *leaderworkerset.LeaderWorkerSet) {
						var leaderPod corev1.Pod
						gomega.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: lws.Name + "-0", Namespace: lws.Namespace}, &leaderPod)).To(gomega.Succeed())
						testing.CreateWorkerPodsForLeaderPod(ctx, leaderPod, k8sClient, *lws)
						// delete one worker pod
						var workers corev1.PodList
						gomega.Expect(k8sClient.List(ctx, &workers, client.InNamespace(lws.Namespace), &client.MatchingLabels{"worker.pod": "workers"})).To(gomega.Succeed())
						gomega.Expect(k8sClient.Delete(ctx, &workers.Items[0])).To(gomega.Succeed())
					},
					checkLWSState: func(lws *leaderworkerset.LeaderWorkerSet) {
						var leaderPod corev1.Pod
						gomega.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: lws.Name + "-0", Namespace: lws.Namespace}, &leaderPod)).To(gomega.Succeed())
						gomega.Expect(leaderPod.DeletionTimestamp == nil).To(gomega.BeTrue())
						var leaders corev1.PodList
						gomega.Expect(k8sClient.List(ctx, &leaders, client.InNamespace(lws.Namespace), &client.MatchingLabels{leaderworkerset.WorkerIndexLabelKey: "0"})).To(gomega.Succeed())
						gomega.Expect(len(leaders.Items)).To(gomega.Equal(1))
						var workers corev1.PodList
						gomega.Expect(k8sClient.List(ctx, &workers, client.InNamespace(lws.Namespace), &client.MatchingLabels{"worker.pod": "workers"})).To(gomega.Succeed())
						gomega.Expect(len(workers.Items)).To(gomega.Equal(2))
					},
				},
			},
		}),
		ginkgo.Entry("Pod restart will delete the pod group when restart policy is RecreateGroupOnPodRestart", &testCase{
			makeLeaderWorkerSet: func(nsName string) *wrappers.LeaderWorkerSetWrapper {
				return wrappers.BuildLeaderWorkerSet(nsName).RestartPolicy(leaderworkerset.RecreateGroupOnPodRestart).Replica(1).Size(3)
			},
			updates: []*update{
				{
					lwsUpdateFn: func(lws *leaderworkerset.LeaderWorkerSet) {
						var leaderPod corev1.Pod
						gomega.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: lws.Name + "-0", Namespace: lws.Namespace}, &leaderPod)).To(gomega.Succeed())
						testing.CreateWorkerPodsForLeaderPod(ctx, leaderPod, k8sClient, *lws)
						// delete one worker pod
						var workers corev1.PodList
						gomega.Expect(k8sClient.List(ctx, &workers, client.InNamespace(lws.Namespace), &client.MatchingLabels{"worker.pod": "workers"})).To(gomega.Succeed())
						gomega.Expect(k8sClient.Delete(ctx, &workers.Items[0])).To(gomega.Succeed())
					},
					checkLWSState: func(lws *leaderworkerset.LeaderWorkerSet) {
						// we could only check the leader pod is marked for deletion since it will be pending on its dependents; and the dependents
						// won't be deleted automatically in integration test
						var leaderPod corev1.Pod
						gomega.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: lws.Name + "-0", Namespace: lws.Namespace}, &leaderPod)).To(gomega.Succeed())
						gomega.Expect(leaderPod.DeletionTimestamp != nil).To(gomega.BeTrue())
						testing.ValidateEvent(ctx, k8sClient, "RecreateGroupOnPodRestart", corev1.EventTypeNormal, "Worker pod test-sample-0-1 failed, deleted leader pod test-sample-0 to recreate group 0", lws.Namespace)
					},
				},
			},
		}),
		ginkgo.Entry("Replicas are processing will set condition to progressing with correct message with correct event", &testCase{
			makeLeaderWorkerSet: wrappers.BuildLeaderWorkerSet,
			updates: []*update{
				{
					checkLWSState: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.ExpectValidLeaderStatefulSet(ctx, k8sClient, lws, 2)
						testing.ExpectValidWorkerStatefulSets(ctx, lws, k8sClient, true)
						testing.ExpectLeaderWorkerSetProgressing(ctx, k8sClient, lws, "Replicas are progressing")
						testing.ValidateEvent(ctx, k8sClient, controllers.GroupsProgressing, corev1.EventTypeNormal, "Replicas are progressing, with 0 groups ready of total 2 groups", lws.Namespace)
					},
				},
			},
		}),
		ginkgo.Entry("Ensure UpdateInProgress condition is not set during LWS initialization", &testCase{
			makeLeaderWorkerSet: func(nsName string) *wrappers.LeaderWorkerSetWrapper {
				return wrappers.BuildLeaderWorkerSet(nsName).Replica(4).MaxSurge(2)
			},
			updates: []*update{
				{
					checkLWSState: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.ExpectLeaderWorkerSetProgressing(ctx, k8sClient, lws, "Replicas are progressing")
						gomega.Expect(func() error {
							for _, c := range lws.Status.Conditions {
								if c.Type == string(leaderworkerset.LeaderWorkerSetUpdateInProgress) && c.Status == metav1.ConditionTrue {
									return fmt.Errorf("UpdateInProgress condition is not expected")
								}
							}
							return nil
						}()).To(gomega.Succeed())
					},
				},
			},
		}),
		ginkgo.Entry("Test default progressing state", &testCase{
			makeLeaderWorkerSet: wrappers.BuildLeaderWorkerSet,
			updates: []*update{
				{
					lwsUpdateFn: func(lws *leaderworkerset.LeaderWorkerSet) {
						lws.Status.Conditions = []metav1.Condition{}
						gomega.Eventually(k8sClient.Status().Update(ctx, lws), testing.Timeout, testing.Interval).Should(gomega.Succeed())
					},
					checkLWSState: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.ExpectLeaderWorkerSetProgressing(ctx, k8sClient, lws, "Replicas are progressing")
					},
				},
			},
		}),
		ginkgo.Entry("Leaderworkerset has available state with correct event", &testCase{
			makeLeaderWorkerSet: wrappers.BuildLeaderWorkerSet,
			updates: []*update{
				{
					lwsUpdateFn: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.SetSuperPodToReady(ctx, k8sClient, lws, 2)
					},
					checkLWSState: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.ExpectLeaderWorkerSetAvailable(ctx, k8sClient, lws, "All replicas are ready")
						testing.ValidateEvent(ctx, k8sClient, "AllGroupsReady", corev1.EventTypeNormal, "All replicas are ready, with 2 groups ready of total 2 groups", lws.Namespace)
					},
				},
			},
		}),

		// Rolling update test cases
		ginkgo.Entry("leaderTemplate changed with default strategy", &testCase{
			makeLeaderWorkerSet: func(nsName string) *wrappers.LeaderWorkerSetWrapper {
				return wrappers.BuildLeaderWorkerSet(nsName).Replica(4)
			},
			updates: []*update{
				{
					// Set lws to available condition.
					lwsUpdateFn: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.SetSuperPodToReady(ctx, k8sClient, lws, 4)
					},
					checkLWSState: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.ExpectLeaderWorkerSetAvailable(ctx, k8sClient, lws, "All replicas are ready")
						testing.ExpectStatefulsetPartitionEqualTo(ctx, k8sClient, lws, 0)
						testing.ExpectValidLeaderStatefulSet(ctx, k8sClient, lws, 4)
						testing.ExpectValidWorkerStatefulSets(ctx, lws, k8sClient, true)
						testing.ExpectLeaderWorkerSetStatusReplicas(ctx, k8sClient, lws, 4, 4)
					},
				},
				{
					// Check the rolling update initial state.
					lwsUpdateFn: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.UpdateLeaderTemplate(ctx, k8sClient, lws)
					},
					checkLWSState: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.ExpectValidLeaderStatefulSet(ctx, k8sClient, lws, 4)
						testing.ExpectLeaderWorkerSetUnavailable(ctx, k8sClient, lws, "All replicas are ready")
						testing.ExpectLeaderWorkerSetProgressing(ctx, k8sClient, lws, "Replicas are progressing")
						testing.ExpectLeaderWorkerSetUpgradeInProgress(ctx, k8sClient, lws, "Rolling Upgrade is in progress")
						// This should be 4 at the first step, however, reconciliation syncs quickly and
						// soon updated to 3 (replicas-maxUnavailable), it's fine here.
						testing.ValidateEvent(ctx, k8sClient, controllers.GroupsUpdating, corev1.EventTypeNormal, "Updating replicas 4 to 3", lws.Namespace)
						testing.ExpectStatefulsetPartitionEqualTo(ctx, k8sClient, lws, 3)
						testing.ExpectLeaderWorkerSetStatusReplicas(ctx, k8sClient, lws, 4, 0)
					},
				},
				{
					// Rolling update 1 replica.
					lwsUpdateFn: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.SetPodGroupToReady(ctx, k8sClient, lws.Name+"-3", lws)
					},
					checkLWSState: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.ExpectValidLeaderStatefulSet(ctx, k8sClient, lws, 4)
						testing.ExpectLeaderWorkerSetUnavailable(ctx, k8sClient, lws, "All replicas are ready")
						testing.ExpectLeaderWorkerSetProgressing(ctx, k8sClient, lws, "Replicas are progressing")
						testing.ExpectLeaderWorkerSetUpgradeInProgress(ctx, k8sClient, lws, "Rolling Upgrade is in progress")
						testing.ValidateEvent(ctx, k8sClient, controllers.GroupsUpdating, corev1.EventTypeNormal, "Updating replicas 3 to 2", lws.Namespace)
						testing.ExpectStatefulsetPartitionEqualTo(ctx, k8sClient, lws, 2)
						testing.ExpectLeaderWorkerSetStatusReplicas(ctx, k8sClient, lws, 4, 1)
					},
				},
				{
					// Update the 1-index replica will not change the partition.
					lwsUpdateFn: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.SetPodGroupToReady(ctx, k8sClient, lws.Name+"-1", lws)
					},
					checkLWSState: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.ExpectLeaderWorkerSetUnavailable(ctx, k8sClient, lws, "All replicas are ready")
						testing.ExpectLeaderWorkerSetProgressing(ctx, k8sClient, lws, "Replicas are progressing")
						testing.ExpectLeaderWorkerSetUpgradeInProgress(ctx, k8sClient, lws, "Rolling Upgrade is in progress")
						testing.ExpectStatefulsetPartitionEqualTo(ctx, k8sClient, lws, 2)
						testing.ExpectValidLeaderStatefulSet(ctx, k8sClient, lws, 4)
						testing.ExpectLeaderWorkerSetStatusReplicas(ctx, k8sClient, lws, 4, 2)
					},
				},
				{
					// Make the 3-index replica unready will not move the partition back.
					lwsUpdateFn: func(lws *leaderworkerset.LeaderWorkerSet) {
						var sts appsv1.StatefulSet
						gomega.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: lws.Name + "-3", Namespace: lws.Namespace}, &sts)).To(gomega.Succeed())
						testing.SetStatefulsetToUnReady(ctx, k8sClient, &sts)
					},
					checkLWSState: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.ExpectLeaderWorkerSetUnavailable(ctx, k8sClient, lws, "All replicas are ready")
						testing.ExpectLeaderWorkerSetProgressing(ctx, k8sClient, lws, "Replicas are progressing")
						testing.ExpectLeaderWorkerSetUpgradeInProgress(ctx, k8sClient, lws, "Rolling Upgrade is in progress")
						testing.ExpectStatefulsetPartitionEqualTo(ctx, k8sClient, lws, 2)
						testing.ExpectValidLeaderStatefulSet(ctx, k8sClient, lws, 4)
						// 3-index status is unready but template already updated.
						testing.ExpectLeaderWorkerSetStatusReplicas(ctx, k8sClient, lws, 3, 2)
					},
				},
				{
					// Rolling update all the replicas will make the leader statefulset ready again.
					lwsUpdateFn: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.SetSuperPodToReady(ctx, k8sClient, lws, 4)
					},
					checkLWSState: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.ExpectValidLeaderStatefulSet(ctx, k8sClient, lws, 4)
						testing.ExpectValidWorkerStatefulSets(ctx, lws, k8sClient, true)
						testing.ExpectStatefulsetPartitionEqualTo(ctx, k8sClient, lws, 0)
						testing.ExpectLeaderWorkerSetNotProgressing(ctx, k8sClient, lws, "Replicas are progressing")
						testing.ExpectLeaderWorkerSetNoUpgradeInProgress(ctx, k8sClient, lws, "Rolling Upgrade is in progress")
						testing.ExpectLeaderWorkerSetAvailable(ctx, k8sClient, lws, "All replicas are ready")
						testing.ExpectLeaderWorkerSetStatusReplicas(ctx, k8sClient, lws, 4, 4)
					},
				},
			},
		}),
		ginkgo.Entry("workerTemplate changed with maxUnavailable=2", &testCase{
			makeLeaderWorkerSet: func(nsName string) *wrappers.LeaderWorkerSetWrapper {
				return wrappers.BuildLeaderWorkerSet(nsName).Replica(4).MaxUnavailable(2)
			},
			updates: []*update{
				{
					// Set lws to available condition.
					lwsUpdateFn: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.SetSuperPodToReady(ctx, k8sClient, lws, 4)
					},
					checkLWSState: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.ExpectStatefulsetPartitionEqualTo(ctx, k8sClient, lws, 0)
						testing.ExpectValidLeaderStatefulSet(ctx, k8sClient, lws, 4)
						testing.ExpectValidWorkerStatefulSets(ctx, lws, k8sClient, true)
						testing.ExpectLeaderWorkerSetAvailable(ctx, k8sClient, lws, "All replicas are ready")
						testing.ExpectLeaderWorkerSetStatusReplicas(ctx, k8sClient, lws, 4, 4)
					},
				},
				{
					// Update the worker template.
					lwsUpdateFn: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.UpdateWorkerTemplate(ctx, k8sClient, lws)
					},
					checkLWSState: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.ExpectValidLeaderStatefulSet(ctx, k8sClient, lws, 4)
						testing.ExpectLeaderWorkerSetUnavailable(ctx, k8sClient, lws, "All replicas are ready")
						testing.ExpectLeaderWorkerSetProgressing(ctx, k8sClient, lws, "Replicas are progressing")
						testing.ExpectLeaderWorkerSetUpgradeInProgress(ctx, k8sClient, lws, "Rolling Upgrade is in progress")
						testing.ExpectStatefulsetPartitionEqualTo(ctx, k8sClient, lws, 2)
						testing.ExpectLeaderWorkerSetStatusReplicas(ctx, k8sClient, lws, 4, 0)
					},
				},
				{
					// Rolling update index-3 replica.
					lwsUpdateFn: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.SetPodGroupToReady(ctx, k8sClient, lws.Name+"-3", lws)
					},
					checkLWSState: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.ExpectValidLeaderStatefulSet(ctx, k8sClient, lws, 4)
						testing.ExpectLeaderWorkerSetUnavailable(ctx, k8sClient, lws, "All replicas are ready")
						testing.ExpectLeaderWorkerSetProgressing(ctx, k8sClient, lws, "Replicas are progressing")
						testing.ExpectLeaderWorkerSetUpgradeInProgress(ctx, k8sClient, lws, "Rolling Upgrade is in progress")
						testing.ExpectStatefulsetPartitionEqualTo(ctx, k8sClient, lws, 1)
						testing.ExpectLeaderWorkerSetStatusReplicas(ctx, k8sClient, lws, 4, 1)
					},
				},
				{
					// Rolling update index-2 replicas.
					lwsUpdateFn: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.SetPodGroupToReady(ctx, k8sClient, lws.Name+"-2", lws)
					},
					checkLWSState: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.ExpectValidLeaderStatefulSet(ctx, k8sClient, lws, 4)
						testing.ExpectLeaderWorkerSetUnavailable(ctx, k8sClient, lws, "All replicas are ready")
						testing.ExpectLeaderWorkerSetProgressing(ctx, k8sClient, lws, "Replicas are progressing")
						testing.ExpectLeaderWorkerSetUpgradeInProgress(ctx, k8sClient, lws, "Rolling Upgrade is in progress")
						testing.ExpectStatefulsetPartitionEqualTo(ctx, k8sClient, lws, 0)
						testing.ExpectLeaderWorkerSetStatusReplicas(ctx, k8sClient, lws, 4, 2)
					},
				},
				{
					// Rolling update the rest 2 replicas.
					lwsUpdateFn: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.SetPodGroupToReady(ctx, k8sClient, lws.Name+"-1", lws)
						testing.SetPodGroupToReady(ctx, k8sClient, lws.Name+"-0", lws)
					},
					checkLWSState: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.ExpectValidLeaderStatefulSet(ctx, k8sClient, lws, 4)
						testing.ExpectValidWorkerStatefulSets(ctx, lws, k8sClient, true)
						testing.ExpectLeaderWorkerSetAvailable(ctx, k8sClient, lws, "All replicas are ready")
						testing.ExpectLeaderWorkerSetNotProgressing(ctx, k8sClient, lws, "Replicas are progressing")
						testing.ExpectLeaderWorkerSetNoUpgradeInProgress(ctx, k8sClient, lws, "Rolling Upgrade is in progress")
						testing.ExpectStatefulsetPartitionEqualTo(ctx, k8sClient, lws, 0)
						testing.ExpectLeaderWorkerSetStatusReplicas(ctx, k8sClient, lws, 4, 4)
					},
				},
			},
		}),
		ginkgo.Entry("workerTemplate changed with maxUnavailable greater than replicas", &testCase{
			makeLeaderWorkerSet: func(nsName string) *wrappers.LeaderWorkerSetWrapper {
				return wrappers.BuildLeaderWorkerSet(nsName).Replica(4).MaxUnavailable(10)
			},
			updates: []*update{
				{
					// Set lws to available condition.
					lwsUpdateFn: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.SetSuperPodToReady(ctx, k8sClient, lws, 4)
					},
					checkLWSState: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.ExpectStatefulsetPartitionEqualTo(ctx, k8sClient, lws, 0)
						testing.ExpectValidLeaderStatefulSet(ctx, k8sClient, lws, 4)
						testing.ExpectValidWorkerStatefulSets(ctx, lws, k8sClient, true)
						testing.ExpectLeaderWorkerSetAvailable(ctx, k8sClient, lws, "All replicas are ready")
						testing.ExpectLeaderWorkerSetStatusReplicas(ctx, k8sClient, lws, 4, 4)
					},
				},
				{
					// Update the worker template.
					lwsUpdateFn: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.UpdateWorkerTemplate(ctx, k8sClient, lws)
					},
					checkLWSState: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.ExpectValidLeaderStatefulSet(ctx, k8sClient, lws, 4)
						testing.ExpectLeaderWorkerSetUnavailable(ctx, k8sClient, lws, "All replicas are ready")
						testing.ExpectLeaderWorkerSetProgressing(ctx, k8sClient, lws, "Replicas are progressing")
						testing.ExpectLeaderWorkerSetUpgradeInProgress(ctx, k8sClient, lws, "Rolling Upgrade is in progress")
						testing.ExpectStatefulsetPartitionEqualTo(ctx, k8sClient, lws, 0)
						testing.ExpectLeaderWorkerSetStatusReplicas(ctx, k8sClient, lws, 4, 0)
					},
				},
				{
					// Set all groups to ready.
					lwsUpdateFn: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.SetSuperPodToReady(ctx, k8sClient, lws, 4)
					},
					checkLWSState: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.ExpectValidLeaderStatefulSet(ctx, k8sClient, lws, 4)
						testing.ExpectValidWorkerStatefulSets(ctx, lws, k8sClient, true)
						testing.ExpectStatefulsetPartitionEqualTo(ctx, k8sClient, lws, 0)
						testing.ExpectLeaderWorkerSetAvailable(ctx, k8sClient, lws, "All replicas are ready")
						testing.ExpectLeaderWorkerSetNotProgressing(ctx, k8sClient, lws, "Replicas are progressing")
						testing.ExpectLeaderWorkerSetNoUpgradeInProgress(ctx, k8sClient, lws, "Rolling Upgrade is in progress")
						testing.ExpectLeaderWorkerSetStatusReplicas(ctx, k8sClient, lws, 4, 4)
					},
				},
			},
		}),
		ginkgo.Entry("rolling update with both worker template and number of replicas changed", &testCase{
			makeLeaderWorkerSet: func(nsName string) *wrappers.LeaderWorkerSetWrapper {
				return wrappers.BuildLeaderWorkerSet(nsName).Replica(4)
			},
			updates: []*update{
				{
					// Set lws to available condition.
					lwsUpdateFn: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.SetSuperPodToReady(ctx, k8sClient, lws, 4)
					},
					checkLWSState: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.ExpectStatefulsetPartitionEqualTo(ctx, k8sClient, lws, 0)
						testing.ExpectValidLeaderStatefulSet(ctx, k8sClient, lws, 4)
						testing.ExpectValidWorkerStatefulSets(ctx, lws, k8sClient, true)
						testing.ExpectLeaderWorkerSetAvailable(ctx, k8sClient, lws, "All replicas are ready")
						testing.ExpectLeaderWorkerSetStatusReplicas(ctx, k8sClient, lws, 4, 4)
					},
				},
				{
					// Update the worker template and the replicas.
					lwsUpdateFn: func(lws *leaderworkerset.LeaderWorkerSet) {
						var leaderworkerset leaderworkerset.LeaderWorkerSet
						gomega.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: lws.Name, Namespace: lws.Namespace}, &leaderworkerset)).To(gomega.Succeed())
						leaderworkerset.Spec.Replicas = ptr.To[int32](6)
						leaderworkerset.Spec.LeaderWorkerTemplate.WorkerTemplate.Spec.Containers[0].Name = "new-worker"
						gomega.Expect(k8sClient.Update(ctx, &leaderworkerset)).To(gomega.Succeed())

						var leaderSts appsv1.StatefulSet
						gomega.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: leaderworkerset.Name, Namespace: leaderworkerset.Namespace}, &leaderSts)).To(gomega.Succeed())
						// Manually create leader pods here because we have no statefulset controller.
						gomega.Expect(testing.CreateLeaderPods(ctx, leaderSts, k8sClient, lws, 4, 6)).To(gomega.Succeed())
					},
					checkLWSState: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.ExpectValidLeaderStatefulSet(ctx, k8sClient, lws, 6)
						testing.ExpectLeaderWorkerSetUnavailable(ctx, k8sClient, lws, "All replicas are ready")
						testing.ExpectLeaderWorkerSetProgressing(ctx, k8sClient, lws, "Replicas are progressing")
						testing.ExpectLeaderWorkerSetUpgradeInProgress(ctx, k8sClient, lws, "Rolling Upgrade is in progress")
						// When scaling up the Replicas, Partition will not change, so the new created Pods will apply with the new template.
						testing.ExpectStatefulsetPartitionEqualTo(ctx, k8sClient, lws, 4)
						// We haven't set the replica-4, replica-5 to ready, so the readyReplicas is 4, the updatedReplicas is 0.
						testing.ExpectLeaderWorkerSetStatusReplicas(ctx, k8sClient, lws, 4, 0)
					},
				},
				{
					// Set all groups to ready.
					lwsUpdateFn: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.SetSuperPodToReady(ctx, k8sClient, lws, 6)
					},
					checkLWSState: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.ExpectValidLeaderStatefulSet(ctx, k8sClient, lws, 6)
						testing.ExpectValidWorkerStatefulSets(ctx, lws, k8sClient, true)
						testing.ExpectStatefulsetPartitionEqualTo(ctx, k8sClient, lws, 0)
						testing.ExpectLeaderWorkerSetAvailable(ctx, k8sClient, lws, "All replicas are ready")
						testing.ExpectLeaderWorkerSetNotProgressing(ctx, k8sClient, lws, "Replicas are progressing")
						testing.ExpectLeaderWorkerSetNoUpgradeInProgress(ctx, k8sClient, lws, "Rolling Upgrade is in progress")
						testing.ExpectLeaderWorkerSetStatusReplicas(ctx, k8sClient, lws, 6, 6)
					},
				},
			},
		}),
		ginkgo.Entry("replicas increases during rolling update", &testCase{
			makeLeaderWorkerSet: func(nsName string) *wrappers.LeaderWorkerSetWrapper {
				return wrappers.BuildLeaderWorkerSet(nsName).Replica(4)
			},
			updates: []*update{
				{
					// Set lws to available condition.
					lwsUpdateFn: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.SetSuperPodToReady(ctx, k8sClient, lws, 4)
					},
					checkLWSState: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.ExpectLeaderWorkerSetAvailable(ctx, k8sClient, lws, "All replicas are ready")
						testing.ExpectStatefulsetPartitionEqualTo(ctx, k8sClient, lws, 0)
						testing.ExpectValidLeaderStatefulSet(ctx, k8sClient, lws, 4)
						testing.ExpectValidWorkerStatefulSets(ctx, lws, k8sClient, true)
						testing.ExpectLeaderWorkerSetStatusReplicas(ctx, k8sClient, lws, 4, 4)
					},
				},
				{
					// Update the worker template.
					lwsUpdateFn: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.UpdateWorkerTemplate(ctx, k8sClient, lws)
					},
					checkLWSState: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.ExpectValidLeaderStatefulSet(ctx, k8sClient, lws, 4)
						testing.ExpectLeaderWorkerSetUnavailable(ctx, k8sClient, lws, "All replicas are ready")
						testing.ExpectLeaderWorkerSetProgressing(ctx, k8sClient, lws, "Replicas are progressing")
						testing.ExpectLeaderWorkerSetUpgradeInProgress(ctx, k8sClient, lws, "Rolling Upgrade is in progress")
						testing.ExpectStatefulsetPartitionEqualTo(ctx, k8sClient, lws, 3)
						testing.ExpectLeaderWorkerSetStatusReplicas(ctx, k8sClient, lws, 4, 0)
					},
				},
				{
					// Rolling update index-3 replica.
					lwsUpdateFn: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.SetPodGroupToReady(ctx, k8sClient, lws.Name+"-3", lws)
					},
					checkLWSState: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.ExpectValidLeaderStatefulSet(ctx, k8sClient, lws, 4)
						testing.ExpectLeaderWorkerSetUnavailable(ctx, k8sClient, lws, "All replicas are ready")
						testing.ExpectLeaderWorkerSetProgressing(ctx, k8sClient, lws, "Replicas are progressing")
						testing.ExpectLeaderWorkerSetUpgradeInProgress(ctx, k8sClient, lws, "Rolling Upgrade is in progress")
						testing.ExpectStatefulsetPartitionEqualTo(ctx, k8sClient, lws, 2)
						testing.ExpectLeaderWorkerSetStatusReplicas(ctx, k8sClient, lws, 4, 1)
					},
				},
				{
					// Update the replicas during rolling update.
					lwsUpdateFn: func(lws *leaderworkerset.LeaderWorkerSet) {
						var leaderworkerset leaderworkerset.LeaderWorkerSet

						gomega.Eventually(func() error {
							if err := k8sClient.Get(ctx, types.NamespacedName{Name: lws.Name, Namespace: lws.Namespace}, &leaderworkerset); err != nil {
								return err
							}
							leaderworkerset.Spec.Replicas = ptr.To[int32](6)
							return k8sClient.Update(ctx, &leaderworkerset)
						}, testing.Timeout, testing.Interval).Should(gomega.Succeed())

						var leaderSts appsv1.StatefulSet
						gomega.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: leaderworkerset.Name, Namespace: leaderworkerset.Namespace}, &leaderSts)).To(gomega.Succeed())
						// Manually create leader pods here because we have no statefulset controller.
						gomega.Expect(testing.CreateLeaderPods(ctx, leaderSts, k8sClient, lws, 4, 6)).To(gomega.Succeed())
						testing.SetPodGroupToReady(ctx, k8sClient, lws.Name+"-4", lws)
						testing.SetPodGroupToReady(ctx, k8sClient, lws.Name+"-5", lws)
					},
					checkLWSState: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.ExpectValidLeaderStatefulSet(ctx, k8sClient, lws, 6)
						testing.ExpectLeaderWorkerSetUnavailable(ctx, k8sClient, lws, "All replicas are ready")
						testing.ExpectLeaderWorkerSetProgressing(ctx, k8sClient, lws, "Replicas are progressing")
						testing.ExpectLeaderWorkerSetUpgradeInProgress(ctx, k8sClient, lws, "Rolling Upgrade is in progress")
						testing.ExpectStatefulsetPartitionEqualTo(ctx, k8sClient, lws, 2)
						testing.ExpectLeaderWorkerSetStatusReplicas(ctx, k8sClient, lws, 6, 3)
					},
				},
				{
					// Set all groups to ready.
					lwsUpdateFn: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.SetSuperPodToReady(ctx, k8sClient, lws, 6)
					},
					checkLWSState: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.ExpectValidLeaderStatefulSet(ctx, k8sClient, lws, 6)
						testing.ExpectValidWorkerStatefulSets(ctx, lws, k8sClient, true)
						testing.ExpectStatefulsetPartitionEqualTo(ctx, k8sClient, lws, 0)
						testing.ExpectLeaderWorkerSetAvailable(ctx, k8sClient, lws, "All replicas are ready")
						testing.ExpectLeaderWorkerSetNotProgressing(ctx, k8sClient, lws, "Replicas are progressing")
						testing.ExpectLeaderWorkerSetNoUpgradeInProgress(ctx, k8sClient, lws, "Rolling Upgrade is in progress")
						testing.ExpectLeaderWorkerSetStatusReplicas(ctx, k8sClient, lws, 6, 6)
					},
				},
			},
		}),
		ginkgo.Entry("replicas decreases during rolling update", &testCase{
			makeLeaderWorkerSet: func(nsName string) *wrappers.LeaderWorkerSetWrapper {
				return wrappers.BuildLeaderWorkerSet(nsName).Replica(6)
			},
			updates: []*update{
				{
					// Set lws to available condition.
					lwsUpdateFn: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.SetSuperPodToReady(ctx, k8sClient, lws, 6)
					},
					checkLWSState: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.ExpectLeaderWorkerSetAvailable(ctx, k8sClient, lws, "All replicas are ready")
						testing.ExpectStatefulsetPartitionEqualTo(ctx, k8sClient, lws, 0)
						testing.ExpectValidLeaderStatefulSet(ctx, k8sClient, lws, 6)
						testing.ExpectValidWorkerStatefulSets(ctx, lws, k8sClient, true)
						testing.ExpectLeaderWorkerSetStatusReplicas(ctx, k8sClient, lws, 6, 6)
					},
				},
				{
					// Update the worker template.
					lwsUpdateFn: func(lws *leaderworkerset.LeaderWorkerSet) {
						gomega.Eventually(func() error {
							var leaderworkerset leaderworkerset.LeaderWorkerSet
							if err := k8sClient.Get(ctx, types.NamespacedName{Name: lws.Name, Namespace: lws.Namespace}, &leaderworkerset); err != nil {
								return err
							}
							leaderworkerset.Spec.LeaderWorkerTemplate.WorkerTemplate.Spec.Containers[0].Image = "nginxinc/nginx-unprivileged:1.26"
							return k8sClient.Update(ctx, &leaderworkerset)
						}, testing.Timeout, testing.Interval).Should(gomega.Succeed())
					},
					checkLWSState: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.ExpectValidLeaderStatefulSet(ctx, k8sClient, lws, 6)
						testing.ExpectLeaderWorkerSetUnavailable(ctx, k8sClient, lws, "All replicas are ready")
						testing.ExpectLeaderWorkerSetProgressing(ctx, k8sClient, lws, "Replicas are progressing")
						testing.ExpectLeaderWorkerSetUpgradeInProgress(ctx, k8sClient, lws, "Rolling Upgrade is in progress")
						testing.ExpectStatefulsetPartitionEqualTo(ctx, k8sClient, lws, 5)
						testing.ExpectLeaderWorkerSetStatusReplicas(ctx, k8sClient, lws, 6, 0)
					},
				},
				{
					// Update the replicas during rolling update.
					lwsUpdateFn: func(lws *leaderworkerset.LeaderWorkerSet) {
						var leaderworkerset leaderworkerset.LeaderWorkerSet

						gomega.Eventually(func() error {
							if err := k8sClient.Get(ctx, types.NamespacedName{Name: lws.Name, Namespace: lws.Namespace}, &leaderworkerset); err != nil {
								return err
							}
							leaderworkerset.Spec.Replicas = ptr.To[int32](3)
							return k8sClient.Update(ctx, &leaderworkerset)
						}, testing.Timeout, testing.Interval).Should(gomega.Succeed())
						// Manually delete leader pods here because we have no statefulset controller.
						testing.DeleteLeaderPods(ctx, k8sClient, &leaderworkerset)
					},
					checkLWSState: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.ExpectValidLeaderStatefulSet(ctx, k8sClient, lws, 3)
						testing.ExpectLeaderWorkerSetUnavailable(ctx, k8sClient, lws, "All replicas are ready")
						testing.ExpectLeaderWorkerSetProgressing(ctx, k8sClient, lws, "Replicas are progressing")
						testing.ExpectLeaderWorkerSetUpgradeInProgress(ctx, k8sClient, lws, "Rolling Upgrade is in progress")
						testing.ExpectStatefulsetPartitionEqualTo(ctx, k8sClient, lws, 2)
						testing.ExpectLeaderWorkerSetStatusReplicas(ctx, k8sClient, lws, 3, 0)
					},
				},
				{
					// Set all groups to ready.
					lwsUpdateFn: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.SetSuperPodToReady(ctx, k8sClient, lws, 3)
					},
					checkLWSState: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.ExpectValidLeaderStatefulSet(ctx, k8sClient, lws, 3)
						testing.ExpectValidWorkerStatefulSets(ctx, lws, k8sClient, true)
						testing.ExpectStatefulsetPartitionEqualTo(ctx, k8sClient, lws, 0)
						testing.ExpectLeaderWorkerSetAvailable(ctx, k8sClient, lws, "All replicas are ready")
						testing.ExpectLeaderWorkerSetNotProgressing(ctx, k8sClient, lws, "Replicas are progressing")
						testing.ExpectLeaderWorkerSetNoUpgradeInProgress(ctx, k8sClient, lws, "Rolling Upgrade is in progress")
						testing.ExpectLeaderWorkerSetStatusReplicas(ctx, k8sClient, lws, 3, 3)
					},
				},
			},
		}),
		ginkgo.Entry("rolling update with maxSurge set", &testCase{
			makeLeaderWorkerSet: func(nsName string) *wrappers.LeaderWorkerSetWrapper {
				return wrappers.BuildLeaderWorkerSet(nsName).Replica(4).MaxSurge(1)
			},
			updates: []*update{
				{
					// Set lws to available condition.
					lwsUpdateFn: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.SetSuperPodToReady(ctx, k8sClient, lws, 4)
					},
					checkLWSState: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.ExpectLeaderWorkerSetAvailable(ctx, k8sClient, lws, "All replicas are ready")
						testing.ExpectStatefulsetPartitionEqualTo(ctx, k8sClient, lws, 0)
						testing.ExpectValidLeaderStatefulSet(ctx, k8sClient, lws, 4)
						testing.ExpectValidWorkerStatefulSets(ctx, lws, k8sClient, true)
						testing.ExpectLeaderWorkerSetStatusReplicas(ctx, k8sClient, lws, 4, 4)
					},
				},
				{
					// Update the worker template.
					lwsUpdateFn: func(lws *leaderworkerset.LeaderWorkerSet) {
						gomega.Eventually(func() error {
							var leaderworkerset leaderworkerset.LeaderWorkerSet
							if err := k8sClient.Get(ctx, types.NamespacedName{Name: lws.Name, Namespace: lws.Namespace}, &leaderworkerset); err != nil {
								return err
							}
							leaderworkerset.Spec.LeaderWorkerTemplate.WorkerTemplate.Spec.Containers[0].Name = "new-worker"
							return k8sClient.Update(ctx, &leaderworkerset)
						}, testing.Timeout, testing.Interval).Should(gomega.Succeed())

						var leaderSts appsv1.StatefulSet
						testing.GetLeaderStatefulset(ctx, lws, k8sClient, &leaderSts)
						// Create leader pod for maxSurge.
						gomega.Expect(testing.CreateLeaderPods(ctx, leaderSts, k8sClient, lws, int(*lws.Spec.Replicas), int(*lws.Spec.Replicas)+1)).To(gomega.Succeed())
					},
					checkLWSState: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.ExpectValidLeaderStatefulSet(ctx, k8sClient, lws, 5)
						testing.ExpectLeaderWorkerSetUnavailable(ctx, k8sClient, lws, "All replicas are ready")
						testing.ExpectLeaderWorkerSetProgressing(ctx, k8sClient, lws, "Replicas are progressing")
						testing.ExpectLeaderWorkerSetUpgradeInProgress(ctx, k8sClient, lws, "Rolling Upgrade is in progress")
						testing.ExpectStatefulsetPartitionEqualTo(ctx, k8sClient, lws, 3)
						testing.ExpectLeaderWorkerSetStatusReplicas(ctx, k8sClient, lws, 4, 0)
					},
				},
				{
					// Rolling update index-4 replica.
					lwsUpdateFn: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.SetPodGroupToReady(ctx, k8sClient, lws.Name+"-4", lws)
					},
					checkLWSState: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.ExpectValidLeaderStatefulSet(ctx, k8sClient, lws, 5)
						testing.ExpectLeaderWorkerSetUnavailable(ctx, k8sClient, lws, "All replicas are ready")
						testing.ExpectLeaderWorkerSetProgressing(ctx, k8sClient, lws, "Replicas are progressing")
						testing.ExpectLeaderWorkerSetUpgradeInProgress(ctx, k8sClient, lws, "Rolling Upgrade is in progress")
						testing.ExpectStatefulsetPartitionEqualTo(ctx, k8sClient, lws, 2)
						testing.ExpectLeaderWorkerSetStatusReplicas(ctx, k8sClient, lws, 5, 1)
					},
				},
				{
					// Rolling update index-3 replica.
					lwsUpdateFn: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.SetPodGroupToReady(ctx, k8sClient, lws.Name+"-3", lws)
					},
					checkLWSState: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.ExpectValidLeaderStatefulSet(ctx, k8sClient, lws, 5)
						testing.ExpectLeaderWorkerSetUnavailable(ctx, k8sClient, lws, "All replicas are ready")
						testing.ExpectLeaderWorkerSetProgressing(ctx, k8sClient, lws, "Replicas are progressing")
						testing.ExpectLeaderWorkerSetUpgradeInProgress(ctx, k8sClient, lws, "Rolling Upgrade is in progress")
						testing.ExpectStatefulsetPartitionEqualTo(ctx, k8sClient, lws, 1)
						testing.ExpectLeaderWorkerSetStatusReplicas(ctx, k8sClient, lws, 5, 2)
					},
				},
				{
					// Rolling update index-2 replica.
					lwsUpdateFn: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.SetPodGroupToReady(ctx, k8sClient, lws.Name+"-2", lws)
					},
					checkLWSState: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.ExpectValidLeaderStatefulSet(ctx, k8sClient, lws, 5)
						testing.ExpectLeaderWorkerSetUnavailable(ctx, k8sClient, lws, "All replicas are ready")
						testing.ExpectLeaderWorkerSetProgressing(ctx, k8sClient, lws, "Replicas are progressing")
						testing.ExpectLeaderWorkerSetUpgradeInProgress(ctx, k8sClient, lws, "Rolling Upgrade is in progress")
						testing.ExpectStatefulsetPartitionEqualTo(ctx, k8sClient, lws, 0)
						testing.ExpectLeaderWorkerSetStatusReplicas(ctx, k8sClient, lws, 5, 3)
					},
				},
				{
					// Rolling update index-1 replica.
					lwsUpdateFn: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.SetPodGroupToReady(ctx, k8sClient, lws.Name+"-1", lws)
						// Reclaim the replica.
						testing.DeleteLeaderPod(ctx, k8sClient, lws, 4, 5)
					},
					checkLWSState: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.ExpectValidLeaderStatefulSet(ctx, k8sClient, lws, 4)
						testing.ExpectLeaderWorkerSetUnavailable(ctx, k8sClient, lws, "All replicas are ready")
						testing.ExpectLeaderWorkerSetProgressing(ctx, k8sClient, lws, "Replicas are progressing")
						testing.ExpectLeaderWorkerSetUpgradeInProgress(ctx, k8sClient, lws, "Rolling Upgrade is in progress")
						testing.ExpectStatefulsetPartitionEqualTo(ctx, k8sClient, lws, 0)
						testing.ExpectLeaderWorkerSetStatusReplicas(ctx, k8sClient, lws, 4, 3)
					},
				},
				{
					// Rolling update index-0 replica.
					lwsUpdateFn: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.SetPodGroupToReady(ctx, k8sClient, lws.Name+"-0", lws)
					},
					checkLWSState: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.ExpectValidLeaderStatefulSet(ctx, k8sClient, lws, 4)
						testing.ExpectValidWorkerStatefulSets(ctx, lws, k8sClient, true)
						testing.ExpectLeaderWorkerSetAvailable(ctx, k8sClient, lws, "All replicas are ready")
						testing.ExpectLeaderWorkerSetNotProgressing(ctx, k8sClient, lws, "Replicas are progressing")
						testing.ExpectLeaderWorkerSetNoUpgradeInProgress(ctx, k8sClient, lws, "Rolling Upgrade is in progress")
						testing.ExpectStatefulsetPartitionEqualTo(ctx, k8sClient, lws, 0)
						testing.ExpectLeaderWorkerSetStatusReplicas(ctx, k8sClient, lws, 4, 4)
					},
				},
			},
		}),
		ginkgo.Entry("rolling update with replicas scaled up and maxSurge set", &testCase{
			makeLeaderWorkerSet: func(nsName string) *wrappers.LeaderWorkerSetWrapper {
				return wrappers.BuildLeaderWorkerSet(nsName).MaxSurge(2)
			},
			updates: []*update{
				{
					// Set lws to available condition.
					lwsUpdateFn: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.SetSuperPodToReady(ctx, k8sClient, lws, 2)
					},
					checkLWSState: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.ExpectLeaderWorkerSetAvailable(ctx, k8sClient, lws, "All replicas are ready")
						testing.ExpectStatefulsetPartitionEqualTo(ctx, k8sClient, lws, 0)
						testing.ExpectValidLeaderStatefulSet(ctx, k8sClient, lws, 2)
						testing.ExpectValidWorkerStatefulSets(ctx, lws, k8sClient, true)
						testing.ExpectLeaderWorkerSetStatusReplicas(ctx, k8sClient, lws, 2, 2)
					},
				},
				{
					// Update the worker template and replicas.
					lwsUpdateFn: func(lws *leaderworkerset.LeaderWorkerSet) {
						gomega.Eventually(func() error {
							var leaderworkerset leaderworkerset.LeaderWorkerSet
							if err := k8sClient.Get(ctx, types.NamespacedName{Name: lws.Name, Namespace: lws.Namespace}, &leaderworkerset); err != nil {
								return err
							}
							leaderworkerset.Spec.LeaderWorkerTemplate.WorkerTemplate.Spec.Containers[0].Name = "new-worker"
							leaderworkerset.Spec.Replicas = ptr.To[int32](4)
							return k8sClient.Update(ctx, &leaderworkerset)
						}, testing.Timeout, testing.Interval).Should(gomega.Succeed())

						var leaderSts appsv1.StatefulSet
						testing.GetLeaderStatefulset(ctx, lws, k8sClient, &leaderSts)
						// Create leader pod for maxSurge.
						gomega.Expect(testing.CreateLeaderPods(ctx, leaderSts, k8sClient, lws, 2, 6)).To(gomega.Succeed())
					},
					checkLWSState: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.ExpectValidLeaderStatefulSet(ctx, k8sClient, lws, 6)
						testing.ExpectLeaderWorkerSetUnavailable(ctx, k8sClient, lws, "All replicas are ready")
						testing.ExpectLeaderWorkerSetProgressing(ctx, k8sClient, lws, "Replicas are progressing")
						testing.ExpectLeaderWorkerSetUpgradeInProgress(ctx, k8sClient, lws, "Rolling Upgrade is in progress")
						testing.ExpectStatefulsetPartitionEqualTo(ctx, k8sClient, lws, 2)
					},
				},
				{
					// Set all groups to ready.
					lwsUpdateFn: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.SetSuperPodToReady(ctx, k8sClient, lws, 6)
					},
					checkLWSState: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.ExpectLeaderWorkerSetAvailable(ctx, k8sClient, lws, "All replicas are ready")
					},
				},
				{
					lwsUpdateFn: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.DeleteLeaderPods(ctx, k8sClient, lws)
					},
					checkLWSState: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.ExpectValidLeaderStatefulSet(ctx, k8sClient, lws, 4)
						testing.ExpectValidWorkerStatefulSets(ctx, lws, k8sClient, true)
						testing.ExpectLeaderWorkerSetAvailable(ctx, k8sClient, lws, "All replicas are ready")
						testing.ExpectLeaderWorkerSetNotProgressing(ctx, k8sClient, lws, "Replicas are progressing")
						testing.ExpectLeaderWorkerSetNoUpgradeInProgress(ctx, k8sClient, lws, "Rolling Upgrade is in progress")
						testing.ExpectStatefulsetPartitionEqualTo(ctx, k8sClient, lws, 0)
						testing.ExpectLeaderWorkerSetStatusReplicas(ctx, k8sClient, lws, 4, 4)
					},
				},
			},
		}),
		ginkgo.Entry("rolling update with replicas scaled down and maxSurge set", &testCase{
			makeLeaderWorkerSet: func(nsName string) *wrappers.LeaderWorkerSetWrapper {
				return wrappers.BuildLeaderWorkerSet(nsName).Replica(6).MaxSurge(2)
			},
			updates: []*update{
				{
					// Set lws to available condition.
					lwsUpdateFn: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.SetSuperPodToReady(ctx, k8sClient, lws, 6)
					},
					checkLWSState: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.ExpectLeaderWorkerSetAvailable(ctx, k8sClient, lws, "All replicas are ready")
						testing.ExpectStatefulsetPartitionEqualTo(ctx, k8sClient, lws, 0)
						testing.ExpectValidLeaderStatefulSet(ctx, k8sClient, lws, 6)
						testing.ExpectValidWorkerStatefulSets(ctx, lws, k8sClient, true)
						testing.ExpectLeaderWorkerSetStatusReplicas(ctx, k8sClient, lws, 6, 6)
					},
				},
				{
					// Update the worker template and replicas.
					lwsUpdateFn: func(lws *leaderworkerset.LeaderWorkerSet) {
						gomega.Eventually(func() error {
							var leaderworkerset leaderworkerset.LeaderWorkerSet
							if err := k8sClient.Get(ctx, types.NamespacedName{Name: lws.Name, Namespace: lws.Namespace}, &leaderworkerset); err != nil {
								return err
							}
							leaderworkerset.Spec.LeaderWorkerTemplate.WorkerTemplate.Spec.Containers[0].Name = "new-worker"
							leaderworkerset.Spec.Replicas = ptr.To[int32](3)
							return k8sClient.Update(ctx, &leaderworkerset)
						}, testing.Timeout, testing.Interval).Should(gomega.Succeed())
					},
					checkLWSState: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.ExpectValidLeaderStatefulSet(ctx, k8sClient, lws, 5)
						testing.ExpectLeaderWorkerSetUnavailable(ctx, k8sClient, lws, "All replicas are ready")
						testing.ExpectLeaderWorkerSetProgressing(ctx, k8sClient, lws, "Replicas are progressing")
						testing.ExpectLeaderWorkerSetUpgradeInProgress(ctx, k8sClient, lws, "Rolling Upgrade is in progress")
						// Partition is updated from 3 to 2.
						testing.ExpectStatefulsetPartitionEqualTo(ctx, k8sClient, lws, 2)
					},
				},
				{
					// Set all groups to ready.
					lwsUpdateFn: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.DeleteLeaderPod(ctx, k8sClient, lws, 5, 6)
						testing.SetSuperPodToReady(ctx, k8sClient, lws, 5)
					},
					checkLWSState: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.ExpectLeaderWorkerSetAvailable(ctx, k8sClient, lws, "All replicas are ready")
					},
				},
				{
					lwsUpdateFn: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.DeleteLeaderPods(ctx, k8sClient, lws)
					},
					checkLWSState: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.ExpectValidLeaderStatefulSet(ctx, k8sClient, lws, 3)
						testing.ExpectValidWorkerStatefulSets(ctx, lws, k8sClient, true)
						testing.ExpectLeaderWorkerSetAvailable(ctx, k8sClient, lws, "All replicas are ready")
						testing.ExpectLeaderWorkerSetNotProgressing(ctx, k8sClient, lws, "Replicas are progressing")
						testing.ExpectLeaderWorkerSetNoUpgradeInProgress(ctx, k8sClient, lws, "Rolling Upgrade is in progress")
						testing.ExpectStatefulsetPartitionEqualTo(ctx, k8sClient, lws, 0)
						testing.ExpectLeaderWorkerSetStatusReplicas(ctx, k8sClient, lws, 3, 3)
					},
				},
			},
		}),
		ginkgo.Entry("rolling update with maxSurge greater than replicas", &testCase{
			makeLeaderWorkerSet: func(nsName string) *wrappers.LeaderWorkerSetWrapper {
				return wrappers.BuildLeaderWorkerSet(nsName).MaxSurge(4)
			},
			updates: []*update{
				{
					// Set lws to available condition.
					lwsUpdateFn: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.SetSuperPodToReady(ctx, k8sClient, lws, 2)
					},
					checkLWSState: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.ExpectLeaderWorkerSetAvailable(ctx, k8sClient, lws, "All replicas are ready")
						testing.ExpectStatefulsetPartitionEqualTo(ctx, k8sClient, lws, 0)
						testing.ExpectValidLeaderStatefulSet(ctx, k8sClient, lws, 2)
						testing.ExpectValidWorkerStatefulSets(ctx, lws, k8sClient, true)
						testing.ExpectLeaderWorkerSetStatusReplicas(ctx, k8sClient, lws, 2, 2)
					},
				},
				{
					// Update the worker template.
					lwsUpdateFn: func(lws *leaderworkerset.LeaderWorkerSet) {
						gomega.Eventually(func() error {
							var leaderworkerset leaderworkerset.LeaderWorkerSet
							if err := k8sClient.Get(ctx, types.NamespacedName{Name: lws.Name, Namespace: lws.Namespace}, &leaderworkerset); err != nil {
								return err
							}
							leaderworkerset.Spec.LeaderWorkerTemplate.WorkerTemplate.Spec.Containers[0].Name = "new-worker"
							return k8sClient.Update(ctx, &leaderworkerset)
						}, testing.Timeout, testing.Interval).Should(gomega.Succeed())

						var leaderSts appsv1.StatefulSet
						testing.GetLeaderStatefulset(ctx, lws, k8sClient, &leaderSts)
						// Create leader pod for maxSurge.
						gomega.Expect(testing.CreateLeaderPods(ctx, leaderSts, k8sClient, lws, 2, 3)).To(gomega.Succeed())
					},
					checkLWSState: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.ExpectValidLeaderStatefulSet(ctx, k8sClient, lws, 3)
						testing.ExpectLeaderWorkerSetUnavailable(ctx, k8sClient, lws, "All replicas are ready")
						testing.ExpectLeaderWorkerSetProgressing(ctx, k8sClient, lws, "Replicas are progressing")
						testing.ExpectLeaderWorkerSetUpgradeInProgress(ctx, k8sClient, lws, "Rolling Upgrade is in progress")
						testing.ExpectStatefulsetPartitionEqualTo(ctx, k8sClient, lws, 1)
					},
				},
				{
					// Set all groups to ready.
					lwsUpdateFn: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.SetSuperPodToReady(ctx, k8sClient, lws, 3)
					},
					checkLWSState: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.ExpectLeaderWorkerSetAvailable(ctx, k8sClient, lws, "All replicas are ready")
						testing.ExpectLeaderWorkerSetNotProgressing(ctx, k8sClient, lws, "Replicas are progressing")
						testing.ExpectLeaderWorkerSetNoUpgradeInProgress(ctx, k8sClient, lws, "Rolling Upgrade is in progress")
					},
				},
				{
					lwsUpdateFn: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.DeleteLeaderPods(ctx, k8sClient, lws)
					},
					checkLWSState: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.ExpectValidLeaderStatefulSet(ctx, k8sClient, lws, 2)
						testing.ExpectValidWorkerStatefulSets(ctx, lws, k8sClient, true)
						testing.ExpectLeaderWorkerSetAvailable(ctx, k8sClient, lws, "All replicas are ready")
						testing.ExpectLeaderWorkerSetNotProgressing(ctx, k8sClient, lws, "Replicas are progressing")
						testing.ExpectLeaderWorkerSetNoUpgradeInProgress(ctx, k8sClient, lws, "Rolling Upgrade is in progress")
						testing.ExpectStatefulsetPartitionEqualTo(ctx, k8sClient, lws, 0)
						testing.ExpectLeaderWorkerSetStatusReplicas(ctx, k8sClient, lws, 2, 2)
					},
				},
			},
		}),
		ginkgo.Entry("scale up and down during rolling update with maxSurge set", &testCase{
			makeLeaderWorkerSet: func(nsName string) *wrappers.LeaderWorkerSetWrapper {
				return wrappers.BuildLeaderWorkerSet(nsName).Replica(4).MaxSurge(2)
			},
			updates: []*update{
				{
					// Set lws to available condition.
					lwsUpdateFn: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.SetSuperPodToReady(ctx, k8sClient, lws, 4)
					},
					checkLWSState: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.ExpectLeaderWorkerSetAvailable(ctx, k8sClient, lws, "All replicas are ready")
						testing.ExpectStatefulsetPartitionEqualTo(ctx, k8sClient, lws, 0)
						testing.ExpectValidLeaderStatefulSet(ctx, k8sClient, lws, 4)
						testing.ExpectValidWorkerStatefulSets(ctx, lws, k8sClient, true)
						testing.ExpectLeaderWorkerSetStatusReplicas(ctx, k8sClient, lws, 4, 4)
					},
				},
				{
					// Update the worker template.
					lwsUpdateFn: func(lws *leaderworkerset.LeaderWorkerSet) {
						gomega.Eventually(func() error {
							var leaderworkerset leaderworkerset.LeaderWorkerSet
							if err := k8sClient.Get(ctx, types.NamespacedName{Name: lws.Name, Namespace: lws.Namespace}, &leaderworkerset); err != nil {
								return err
							}
							leaderworkerset.Spec.LeaderWorkerTemplate.WorkerTemplate.Spec.Containers[0].Name = "new-worker"
							return k8sClient.Update(ctx, &leaderworkerset)
						}, testing.Timeout, testing.Interval).Should(gomega.Succeed())

						var leaderSts appsv1.StatefulSet
						testing.GetLeaderStatefulset(ctx, lws, k8sClient, &leaderSts)
						// Create leader pod for maxSurge.
						gomega.Expect(testing.CreateLeaderPods(ctx, leaderSts, k8sClient, lws, 4, 6)).To(gomega.Succeed())
					},
					checkLWSState: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.ExpectValidLeaderStatefulSet(ctx, k8sClient, lws, 6)
						testing.ExpectLeaderWorkerSetUnavailable(ctx, k8sClient, lws, "All replicas are ready")
						testing.ExpectLeaderWorkerSetProgressing(ctx, k8sClient, lws, "Replicas are progressing")
						testing.ExpectLeaderWorkerSetUpgradeInProgress(ctx, k8sClient, lws, "Rolling Upgrade is in progress")
						testing.ExpectStatefulsetPartitionEqualTo(ctx, k8sClient, lws, 3)
						testing.ExpectLeaderWorkerSetStatusReplicas(ctx, k8sClient, lws, 4, 0)
					},
				},
				{
					// Scale up during rolling update.
					lwsUpdateFn: func(lws *leaderworkerset.LeaderWorkerSet) {
						gomega.Eventually(func() error {
							var leaderworkerset leaderworkerset.LeaderWorkerSet
							if err := k8sClient.Get(ctx, types.NamespacedName{Name: lws.Name, Namespace: lws.Namespace}, &leaderworkerset); err != nil {
								return err
							}
							leaderworkerset.Spec.Replicas = ptr.To[int32](6)
							return k8sClient.Update(ctx, &leaderworkerset)
						}, testing.Timeout, testing.Interval).Should(gomega.Succeed())

						var leaderSts appsv1.StatefulSet
						testing.GetLeaderStatefulset(ctx, lws, k8sClient, &leaderSts)
						// Create leader pod for maxSurge.
						gomega.Expect(testing.CreateLeaderPods(ctx, leaderSts, k8sClient, lws, 6, 8)).To(gomega.Succeed())
					},
					checkLWSState: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.ExpectValidLeaderStatefulSet(ctx, k8sClient, lws, 8)
						testing.ExpectLeaderWorkerSetUnavailable(ctx, k8sClient, lws, "All replicas are ready")
						testing.ExpectLeaderWorkerSetProgressing(ctx, k8sClient, lws, "Replicas are progressing")
						testing.ExpectLeaderWorkerSetUpgradeInProgress(ctx, k8sClient, lws, "Rolling Upgrade is in progress")
						testing.ExpectStatefulsetPartitionEqualTo(ctx, k8sClient, lws, 3)
						// The last 2 replicas are updated but not ready.
						testing.ExpectLeaderWorkerSetStatusReplicas(ctx, k8sClient, lws, 4, 2)
					},
				},
				{
					// Rolling update last two replica.
					lwsUpdateFn: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.SetPodGroupToReady(ctx, k8sClient, lws.Name+"-7", lws)
						testing.SetPodGroupToReady(ctx, k8sClient, lws.Name+"-6", lws)
					},
					checkLWSState: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.ExpectValidLeaderStatefulSet(ctx, k8sClient, lws, 8)
						testing.ExpectLeaderWorkerSetUnavailable(ctx, k8sClient, lws, "All replicas are ready")
						testing.ExpectLeaderWorkerSetProgressing(ctx, k8sClient, lws, "Replicas are progressing")
						testing.ExpectLeaderWorkerSetUpgradeInProgress(ctx, k8sClient, lws, "Rolling Upgrade is in progress")
						testing.ExpectStatefulsetPartitionEqualTo(ctx, k8sClient, lws, 3)
						testing.ExpectLeaderWorkerSetStatusReplicas(ctx, k8sClient, lws, 6, 2)
					},
				},
				{
					// Scale down during rolling update.
					lwsUpdateFn: func(lws *leaderworkerset.LeaderWorkerSet) {
						gomega.Eventually(func() error {
							var leaderworkerset leaderworkerset.LeaderWorkerSet
							if err := k8sClient.Get(ctx, types.NamespacedName{Name: lws.Name, Namespace: lws.Namespace}, &leaderworkerset); err != nil {
								return err
							}
							leaderworkerset.Spec.Replicas = ptr.To[int32](2)
							return k8sClient.Update(ctx, &leaderworkerset)
						}, testing.Timeout, testing.Interval).Should(gomega.Succeed())
						testing.DeleteLeaderPod(ctx, k8sClient, lws, 4, 8)
						// Reclaim the last replica.
						testing.DeleteLeaderPod(ctx, k8sClient, lws, 3, 4)
					},
					checkLWSState: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.ExpectValidLeaderStatefulSet(ctx, k8sClient, lws, 3)
						testing.ExpectLeaderWorkerSetUnavailable(ctx, k8sClient, lws, "All replicas are ready")
						testing.ExpectLeaderWorkerSetProgressing(ctx, k8sClient, lws, "Replicas are progressing")
						testing.ExpectLeaderWorkerSetUpgradeInProgress(ctx, k8sClient, lws, "Rolling Upgrade is in progress")
						testing.ExpectStatefulsetPartitionEqualTo(ctx, k8sClient, lws, 1)
						testing.ExpectLeaderWorkerSetStatusReplicas(ctx, k8sClient, lws, 3, 0)
					},
				},
				{
					// Rolling update index-2 replica.
					lwsUpdateFn: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.SetPodGroupToReady(ctx, k8sClient, lws.Name+"-2", lws)
					},
					checkLWSState: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.ExpectValidLeaderStatefulSet(ctx, k8sClient, lws, 3)
						testing.ExpectLeaderWorkerSetUnavailable(ctx, k8sClient, lws, "All replicas are ready")
						testing.ExpectLeaderWorkerSetProgressing(ctx, k8sClient, lws, "Replicas are progressing")
						testing.ExpectLeaderWorkerSetUpgradeInProgress(ctx, k8sClient, lws, "Rolling Upgrade is in progress")
						testing.ExpectStatefulsetPartitionEqualTo(ctx, k8sClient, lws, 0)
						testing.ExpectLeaderWorkerSetStatusReplicas(ctx, k8sClient, lws, 3, 1)
					},
				},
				{
					// Rolling update index-1 replica.
					lwsUpdateFn: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.SetPodGroupToReady(ctx, k8sClient, lws.Name+"-1", lws)
						// Reclaim the last replica.
						testing.DeleteLeaderPod(ctx, k8sClient, lws, 2, 3)
					},
					checkLWSState: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.ExpectValidLeaderStatefulSet(ctx, k8sClient, lws, 2)
						testing.ExpectLeaderWorkerSetUnavailable(ctx, k8sClient, lws, "All replicas are ready")
						testing.ExpectLeaderWorkerSetProgressing(ctx, k8sClient, lws, "Replicas are progressing")
						testing.ExpectLeaderWorkerSetUpgradeInProgress(ctx, k8sClient, lws, "Rolling Upgrade is in progress")
						testing.ExpectStatefulsetPartitionEqualTo(ctx, k8sClient, lws, 0)
						testing.ExpectLeaderWorkerSetStatusReplicas(ctx, k8sClient, lws, 2, 1)
					},
				},
				{
					// Rolling update index-0 replica.
					lwsUpdateFn: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.SetPodGroupToReady(ctx, k8sClient, lws.Name+"-0", lws)
					},
					checkLWSState: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.ExpectValidLeaderStatefulSet(ctx, k8sClient, lws, 2)
						testing.ExpectValidWorkerStatefulSets(ctx, lws, k8sClient, true)
						testing.ExpectLeaderWorkerSetAvailable(ctx, k8sClient, lws, "All replicas are ready")
						testing.ExpectLeaderWorkerSetNotProgressing(ctx, k8sClient, lws, "Replicas are progressing")
						testing.ExpectLeaderWorkerSetNoUpgradeInProgress(ctx, k8sClient, lws, "Rolling Upgrade is in progress")
						testing.ExpectStatefulsetPartitionEqualTo(ctx, k8sClient, lws, 0)
						testing.ExpectLeaderWorkerSetStatusReplicas(ctx, k8sClient, lws, 2, 2)
					},
				},
			},
		}),
		ginkgo.Entry("multiple rolling update with maxSurge set", &testCase{
			makeLeaderWorkerSet: func(nsName string) *wrappers.LeaderWorkerSetWrapper {
				return wrappers.BuildLeaderWorkerSet(nsName).Replica(4).MaxSurge(2)
			},
			updates: []*update{
				{
					// Set lws to available condition.
					lwsUpdateFn: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.SetSuperPodToReady(ctx, k8sClient, lws, 4)
					},
					checkLWSState: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.ExpectLeaderWorkerSetAvailable(ctx, k8sClient, lws, "All replicas are ready")
						testing.ExpectStatefulsetPartitionEqualTo(ctx, k8sClient, lws, 0)
						testing.ExpectValidLeaderStatefulSet(ctx, k8sClient, lws, 4)
						testing.ExpectValidWorkerStatefulSets(ctx, lws, k8sClient, true)
						testing.ExpectLeaderWorkerSetStatusReplicas(ctx, k8sClient, lws, 4, 4)
						testing.ExpectRevisions(ctx, k8sClient, lws, 1)
					},
				},
				{
					// Update the worker template.
					lwsUpdateFn: func(lws *leaderworkerset.LeaderWorkerSet) {
						gomega.Eventually(func() error {
							var leaderworkerset leaderworkerset.LeaderWorkerSet
							if err := k8sClient.Get(ctx, types.NamespacedName{Name: lws.Name, Namespace: lws.Namespace}, &leaderworkerset); err != nil {
								return err
							}
							leaderworkerset.Spec.LeaderWorkerTemplate.WorkerTemplate.Spec.Containers[0].Name = "new-worker"
							return k8sClient.Update(ctx, &leaderworkerset)
						}, testing.Timeout, testing.Interval).Should(gomega.Succeed())

						var leaderSts appsv1.StatefulSet
						testing.GetLeaderStatefulset(ctx, lws, k8sClient, &leaderSts)
						// Create leader pod for maxSurge.
						gomega.Expect(testing.CreateLeaderPods(ctx, leaderSts, k8sClient, lws, 4, 6)).To(gomega.Succeed())
					},
					checkLWSState: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.ExpectValidLeaderStatefulSet(ctx, k8sClient, lws, 6)
						testing.ExpectLeaderWorkerSetUnavailable(ctx, k8sClient, lws, "All replicas are ready")
						testing.ExpectLeaderWorkerSetProgressing(ctx, k8sClient, lws, "Replicas are progressing")
						testing.ExpectLeaderWorkerSetUpgradeInProgress(ctx, k8sClient, lws, "Rolling Upgrade is in progress")
						testing.ExpectStatefulsetPartitionEqualTo(ctx, k8sClient, lws, 3)
						testing.ExpectLeaderWorkerSetStatusReplicas(ctx, k8sClient, lws, 4, 0)
						testing.ExpectRevisions(ctx, k8sClient, lws, 2)
					},
				},
				{
					// Rolling update last two replica.
					lwsUpdateFn: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.SetPodGroupToReady(ctx, k8sClient, lws.Name+"-5", lws)
						testing.SetPodGroupToReady(ctx, k8sClient, lws.Name+"-4", lws)
					},
					checkLWSState: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.ExpectValidLeaderStatefulSet(ctx, k8sClient, lws, 6)
						testing.ExpectLeaderWorkerSetUnavailable(ctx, k8sClient, lws, "All replicas are ready")
						testing.ExpectLeaderWorkerSetProgressing(ctx, k8sClient, lws, "Replicas are progressing")
						testing.ExpectLeaderWorkerSetUpgradeInProgress(ctx, k8sClient, lws, "Rolling Upgrade is in progress")
						testing.ExpectStatefulsetPartitionEqualTo(ctx, k8sClient, lws, 1)
						testing.ExpectLeaderWorkerSetStatusReplicas(ctx, k8sClient, lws, 6, 2)
						testing.ExpectRevisions(ctx, k8sClient, lws, 2)
					},
				},
				{
					// Update the worker template again.
					lwsUpdateFn: func(lws *leaderworkerset.LeaderWorkerSet) {
						gomega.Eventually(func() error {
							var leaderworkerset leaderworkerset.LeaderWorkerSet
							if err := k8sClient.Get(ctx, types.NamespacedName{Name: lws.Name, Namespace: lws.Namespace}, &leaderworkerset); err != nil {
								return err
							}
							leaderworkerset.Spec.LeaderWorkerTemplate.WorkerTemplate.Spec.Containers[0].Name = "new-worker-2"
							return k8sClient.Update(ctx, &leaderworkerset)
						}, testing.Timeout, testing.Interval).Should(gomega.Succeed())
					},
					checkLWSState: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.ExpectValidLeaderStatefulSet(ctx, k8sClient, lws, 6)
						testing.ExpectLeaderWorkerSetUnavailable(ctx, k8sClient, lws, "All replicas are ready")
						testing.ExpectLeaderWorkerSetProgressing(ctx, k8sClient, lws, "Replicas are progressing")
						testing.ExpectLeaderWorkerSetUpgradeInProgress(ctx, k8sClient, lws, "Rolling Upgrade is in progress")
						// Partition will transit from 4 to 3.
						testing.ExpectStatefulsetPartitionEqualTo(ctx, k8sClient, lws, 3)
						testing.ExpectLeaderWorkerSetStatusReplicas(ctx, k8sClient, lws, 6, 0)
						testing.ExpectRevisions(ctx, k8sClient, lws, 3)
					},
				},
				{
					// Set all groups to ready.
					lwsUpdateFn: func(lws *leaderworkerset.LeaderWorkerSet) {
						// We didn't delete the leader pod yet.
						testing.SetSuperPodToReady(ctx, k8sClient, lws, 6)
					},
					checkLWSState: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.ExpectLeaderWorkerSetAvailable(ctx, k8sClient, lws, "All replicas are ready")
					},
				},
				{
					lwsUpdateFn: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.DeleteLeaderPods(ctx, k8sClient, lws)
					},
					checkLWSState: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.ExpectValidLeaderStatefulSet(ctx, k8sClient, lws, 4)
						testing.ExpectValidWorkerStatefulSets(ctx, lws, k8sClient, true)
						testing.ExpectLeaderWorkerSetAvailable(ctx, k8sClient, lws, "All replicas are ready")
						testing.ExpectLeaderWorkerSetNotProgressing(ctx, k8sClient, lws, "Replicas are progressing")
						testing.ExpectLeaderWorkerSetNoUpgradeInProgress(ctx, k8sClient, lws, "Rolling Upgrade is in progress")
						testing.ExpectStatefulsetPartitionEqualTo(ctx, k8sClient, lws, 0)
						testing.ExpectLeaderWorkerSetStatusReplicas(ctx, k8sClient, lws, 4, 4)
						testing.ExpectRevisions(ctx, k8sClient, lws, 1)
					},
				},
			},
		}),
		ginkgo.Entry("Not updated worker gets recreated with old worker spec if restarted during update", &testCase{
			makeLeaderWorkerSet: func(nsName string) *wrappers.LeaderWorkerSetWrapper {
				return wrappers.BuildLeaderWorkerSet(nsName).Replica(4)
			},
			updates: []*update{
				{
					// Set lws to available condition.
					lwsUpdateFn: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.SetSuperPodToReady(ctx, k8sClient, lws, 4)
					},
					checkLWSState: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.ExpectLeaderWorkerSetAvailable(ctx, k8sClient, lws, "All replicas are ready")
						testing.ExpectValidLeaderStatefulSet(ctx, k8sClient, lws, 4)
						testing.ExpectValidWorkerStatefulSets(ctx, lws, k8sClient, true)
						testing.ExpectLeaderWorkerSetStatusReplicas(ctx, k8sClient, lws, 4, 4)
						testing.ExpectRevisions(ctx, k8sClient, lws, 1)
					},
				},
				{
					// Check the rolling update initial state.
					lwsUpdateFn: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.UpdateWorkerTemplate(ctx, k8sClient, lws)
					},
					checkLWSState: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.ExpectValidLeaderStatefulSet(ctx, k8sClient, lws, 4)
						testing.ExpectLeaderWorkerSetUnavailable(ctx, k8sClient, lws, "All replicas are ready")
						testing.ExpectLeaderWorkerSetProgressing(ctx, k8sClient, lws, "Replicas are progressing")
						testing.ExpectLeaderWorkerSetUpgradeInProgress(ctx, k8sClient, lws, "Rolling Upgrade is in progress")
						testing.ExpectLeaderWorkerSetStatusReplicas(ctx, k8sClient, lws, 4, 0)
						testing.ExpectRevisions(ctx, k8sClient, lws, 2)
					},
				},
				{
					// Rolling update 1 replica.
					lwsUpdateFn: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.SetPodGroupToReady(ctx, k8sClient, lws.Name+"-3", lws)
					},
					checkLWSState: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.ExpectValidLeaderStatefulSet(ctx, k8sClient, lws, 4)
						testing.ExpectUpdatedWorkerStatefulSet(ctx, k8sClient, lws, lws.Name+"-3")
						testing.ExpectNotUpdatedWorkerStatefulSet(ctx, k8sClient, lws, lws.Name+"-2")
						testing.ExpectNotUpdatedWorkerStatefulSet(ctx, k8sClient, lws, lws.Name+"-1")
						testing.ExpectNotUpdatedWorkerStatefulSet(ctx, k8sClient, lws, lws.Name+"-0")
						testing.ExpectRevisions(ctx, k8sClient, lws, 2)
						testing.ExpectLeaderWorkerSetUnavailable(ctx, k8sClient, lws, "All replicas are ready")
						testing.ExpectLeaderWorkerSetProgressing(ctx, k8sClient, lws, "Replicas are progressing")
						testing.ExpectLeaderWorkerSetUpgradeInProgress(ctx, k8sClient, lws, "Rolling Upgrade is in progress")
						testing.ExpectLeaderWorkerSetStatusReplicas(ctx, k8sClient, lws, 4, 1)
					},
				},
				{
					// Delete the leaderPod and re-create it to force workerSts to be created
					lwsUpdateFn: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.DeleteLeaderPod(ctx, k8sClient, lws, 0, 1)
						var leaderSts appsv1.StatefulSet
						gomega.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: lws.Name, Namespace: lws.Namespace}, &leaderSts)).To(gomega.Succeed())
						testing.CreateLeaderPodsFromRevisionNumber(ctx, leaderSts, k8sClient, lws, 0, 1, 1)
					},
					checkLWSState: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.ExpectValidLeaderStatefulSet(ctx, k8sClient, lws, 4)
						testing.ExpectUpdatedWorkerStatefulSet(ctx, k8sClient, lws, lws.Name+"-3")
						testing.ExpectNotUpdatedWorkerStatefulSet(ctx, k8sClient, lws, lws.Name+"-2")
						testing.ExpectNotUpdatedWorkerStatefulSet(ctx, k8sClient, lws, lws.Name+"-1")
						testing.ExpectNotUpdatedWorkerStatefulSet(ctx, k8sClient, lws, lws.Name+"-0")
						testing.ExpectRevisions(ctx, k8sClient, lws, 2)
						testing.ExpectLeaderWorkerSetUnavailable(ctx, k8sClient, lws, "All replicas are ready")
						testing.ExpectLeaderWorkerSetProgressing(ctx, k8sClient, lws, "Replicas are progressing")
						testing.ExpectLeaderWorkerSetUpgradeInProgress(ctx, k8sClient, lws, "Rolling Upgrade is in progress")
					},
				},
				{
					// Rolling update all the replicas will make the leader statefulset ready again.
					lwsUpdateFn: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.SetSuperPodToReady(ctx, k8sClient, lws, 4)
					},
					checkLWSState: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.ExpectValidLeaderStatefulSet(ctx, k8sClient, lws, 4)
						testing.ExpectValidWorkerStatefulSets(ctx, lws, k8sClient, true)
						testing.ExpectLeaderWorkerSetNotProgressing(ctx, k8sClient, lws, "Replicas are progressing")
						testing.ExpectLeaderWorkerSetNoUpgradeInProgress(ctx, k8sClient, lws, "Rolling Upgrade is in progress")
						testing.ExpectLeaderWorkerSetAvailable(ctx, k8sClient, lws, "All replicas are ready")
						testing.ExpectRevisions(ctx, k8sClient, lws, 1)
					},
				},
			},
		}),
		ginkgo.Entry("leader with RecreateGroupOnPodRestart only gets restarted once during rolling update", &testCase{
			makeLeaderWorkerSet: func(nsName string) *wrappers.LeaderWorkerSetWrapper {
				return wrappers.BuildLeaderWorkerSet(nsName).Replica(4).RestartPolicy(leaderworkerset.RecreateGroupOnPodRestart)
			},
			updates: []*update{
				{
					// Set lws to available condition.
					lwsUpdateFn: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.SetSuperPodToReady(ctx, k8sClient, lws, 4)
						var leaderPod corev1.Pod
						gomega.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: lws.Name + "-3", Namespace: lws.Namespace}, &leaderPod)).To(gomega.Succeed())
						testing.CreateWorkerPodsForLeaderPod(ctx, leaderPod, k8sClient, *lws)
					},
					checkLWSState: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.ExpectLeaderWorkerSetAvailable(ctx, k8sClient, lws, "All replicas are ready")
						testing.ExpectValidLeaderStatefulSet(ctx, k8sClient, lws, 4)
						testing.ExpectValidWorkerStatefulSets(ctx, lws, k8sClient, true)
						testing.ExpectLeaderWorkerSetStatusReplicas(ctx, k8sClient, lws, 4, 4)
					},
				},
				{
					// Check the rolling update initial state.
					lwsUpdateFn: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.UpdateWorkerTemplate(ctx, k8sClient, lws)
					},
					checkLWSState: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.ExpectValidLeaderStatefulSet(ctx, k8sClient, lws, 4)
						testing.ExpectLeaderWorkerSetUnavailable(ctx, k8sClient, lws, "All replicas are ready")
						testing.ExpectLeaderWorkerSetProgressing(ctx, k8sClient, lws, "Replicas are progressing")
						testing.ExpectLeaderWorkerSetUpgradeInProgress(ctx, k8sClient, lws, "Rolling Upgrade is in progress")
						testing.ExpectLeaderWorkerSetStatusReplicas(ctx, k8sClient, lws, 4, 0)
					},
				},
				{
					// Update LeaderPod. Triggers the first restart
					lwsUpdateFn: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.SetPodGroupToReady(ctx, k8sClient, lws.Name+"-3", lws)
					},
					checkLWSState: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.ExpectValidLeaderStatefulSet(ctx, k8sClient, lws, 4)
						testing.ExpectLeaderWorkerSetUnavailable(ctx, k8sClient, lws, "All replicas are ready")
						testing.ExpectLeaderWorkerSetProgressing(ctx, k8sClient, lws, "Replicas are progressing")
						testing.ExpectLeaderWorkerSetUpgradeInProgress(ctx, k8sClient, lws, "Rolling Upgrade is in progress")
						testing.ExpectLeaderWorkerSetStatusReplicas(ctx, k8sClient, lws, 4, 1)
					},
				},
				{ // The workerPods are deleted to update them. This should not delete the leaderPod
					lwsUpdateFn: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.DeleteWorkerPods(ctx, k8sClient, lws)
					},
					checkLWSState: func(lws *leaderworkerset.LeaderWorkerSet) {
						var leaderPod corev1.Pod
						gomega.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: lws.Name + "-3", Namespace: lws.Namespace}, &leaderPod)).To(gomega.Succeed())
						gomega.Consistently(leaderPod.DeletionTimestamp == nil, testing.Timeout, testing.Interval).Should(gomega.BeTrue())
					},
				},
				{
					// Rolling update all the replicas will make the leader statefulset ready again.
					lwsUpdateFn: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.SetSuperPodToReady(ctx, k8sClient, lws, 4)
						// Need to create the updated workerPods since the sts controller doesn't exist in integration tests
						var leaderPod corev1.Pod
						gomega.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: lws.Name + "-3", Namespace: lws.Namespace}, &leaderPod)).To(gomega.Succeed())
						testing.CreateWorkerPodsForLeaderPod(ctx, leaderPod, k8sClient, *lws)

					},
					checkLWSState: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.ExpectValidLeaderStatefulSet(ctx, k8sClient, lws, 4)
						testing.ExpectValidWorkerStatefulSets(ctx, lws, k8sClient, true)
						testing.ExpectLeaderWorkerSetNotProgressing(ctx, k8sClient, lws, "Replicas are progressing")
						testing.ExpectLeaderWorkerSetNoUpgradeInProgress(ctx, k8sClient, lws, "Rolling Upgrade is in progress")
						testing.ExpectLeaderWorkerSetAvailable(ctx, k8sClient, lws, "All replicas are ready")
					},
				},
				{ // Validate that RecreateGroupOnPodRestart works as intended after update
					lwsUpdateFn: func(lws *leaderworkerset.LeaderWorkerSet) {
						var workers corev1.PodList
						gomega.Expect(k8sClient.List(ctx, &workers, client.InNamespace(lws.Namespace), &client.MatchingLabels{"worker.pod": "workers", leaderworkerset.GroupIndexLabelKey: "3"})).To(gomega.Succeed())
						gomega.Expect(k8sClient.Delete(ctx, &workers.Items[0])).To(gomega.Succeed())
					},
					checkLWSState: func(lws *leaderworkerset.LeaderWorkerSet) {
						// we could only check the leader pod is marked for deletion since it will be pending on its dependents; and the dependents
						// won't be deleted automatically in integration test
						var leaderPod corev1.Pod
						gomega.Eventually(func() bool {
							gomega.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: lws.Name + "-3", Namespace: lws.Namespace}, &leaderPod)).To(gomega.Succeed())
							return leaderPod.DeletionTimestamp != nil
						}, testing.Timeout, testing.Interval).Should(gomega.BeTrue())
					},
				},
			},
		}),
		ginkgo.Entry("if a leaderSts exists, but a matching controllerRevision doesn't, it will create one that matches the leaderSts", &testCase{
			makeLeaderWorkerSet: func(nsName string) *wrappers.LeaderWorkerSetWrapper {
				return wrappers.BuildLeaderWorkerSet(nsName)
			},
			updates: []*update{
				{
					// Update LeaderPod. Triggers the first restart
					lwsUpdateFn: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.SetSuperPodToReady(ctx, k8sClient, lws, 2)
					},
					checkLWSState: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.ExpectLeaderWorkerSetAvailable(ctx, k8sClient, lws, "All replicas are ready")
						testing.ExpectRevisions(ctx, k8sClient, lws, 1)
						// Special case we are trying to test by overwriting the revision key that the leaderSts has.
						gomega.Eventually(func() bool {
							var leaderSts appsv1.StatefulSet
							if err := k8sClient.Get(ctx, types.NamespacedName{Name: lws.Name, Namespace: lws.Namespace}, &leaderSts); err != nil {
								return false
							}
							leaderSts.Labels[leaderworkerset.RevisionKey] = "template-hash"
							if err := k8sClient.Update(ctx, &leaderSts); err != nil {
								return false
							}
							revision, err := revisionutils.GetRevision(ctx, k8sClient, lws, "template-hash")
							gomega.Expect(err).ShouldNot(gomega.HaveOccurred())
							return revision != nil
						}, testing.Timeout, testing.Interval).Should(gomega.BeTrue())
						testing.SetSuperPodToReady(ctx, k8sClient, lws, 2)
						testing.ExpectLeaderWorkerSetAvailable(ctx, k8sClient, lws, "All replicas are ready")
						testing.ExpectRevisions(ctx, k8sClient, lws, 1)
						testing.ExpectLeaderWorkerSetStatusReplicas(ctx, k8sClient, lws, 2, 2)
					},
				},
			},
		}),
		ginkgo.Entry("create a leaderworkerset with spec.startupPolicy=LeaderReady", &testCase{
			makeLeaderWorkerSet: func(nsName string) *wrappers.LeaderWorkerSetWrapper {
				return wrappers.BuildLeaderWorkerSet(nsName).Replica(4).StartupPolicy(leaderworkerset.LeaderReadyStartupPolicy)
			},
			updates: []*update{
				{
					checkLWSState: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.ExpectWorkerStatefulSetsNotCreated(ctx, k8sClient, lws)
					},
				},
				{
					lwsUpdateFn: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.SetLeaderPodsToReady(ctx, k8sClient, lws, 0, 2)
					},
					checkLWSState: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.ExpectSpecifiedWorkerStatefulSetsCreated(ctx, k8sClient, lws, 0, 2)
						testing.ExpectSpecifiedWorkerStatefulSetsNotCreated(ctx, k8sClient, lws, 2, 4)
					},
				},
				{
					lwsUpdateFn: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.SetLeaderPodsToReady(ctx, k8sClient, lws, 2, 4)
					},
					checkLWSState: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.ExpectSpecifiedWorkerStatefulSetsCreated(ctx, k8sClient, lws, 2, 4)
					},
				},
			},
		}),
		ginkgo.Entry("create a leaderworkerset with spec.startupPolicy=LeaderCreated", &testCase{
			makeLeaderWorkerSet: func(nsName string) *wrappers.LeaderWorkerSetWrapper {
				return wrappers.BuildLeaderWorkerSet(nsName).Replica(4).StartupPolicy(leaderworkerset.LeaderCreatedStartupPolicy)
			},
			updates: []*update{
				{
					checkLWSState: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.ExpectSpecifiedWorkerStatefulSetsCreated(ctx, k8sClient, lws, 0, 4)
					},
				},
			},
		}),
		ginkgo.Entry("rolling update with unhealthy pods below partition", &testCase{
			makeLeaderWorkerSet: func(nsName string) *wrappers.LeaderWorkerSetWrapper {
				return wrappers.BuildLeaderWorkerSet(nsName).Replica(4).MaxUnavailable(2)
			},
			updates: []*update{
				{
					// Set lws to available condition.
					lwsUpdateFn: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.SetSuperPodToReady(ctx, k8sClient, lws, 4)
					},
					checkLWSState: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.ExpectLeaderWorkerSetAvailable(ctx, k8sClient, lws, "All replicas are ready")
						testing.ExpectStatefulsetPartitionEqualTo(ctx, k8sClient, lws, 0)
						testing.ExpectValidLeaderStatefulSet(ctx, k8sClient, lws, 4)
						testing.ExpectValidWorkerStatefulSets(ctx, lws, k8sClient, true)
						testing.ExpectLeaderWorkerSetStatusReplicas(ctx, k8sClient, lws, 4, 4)
					},
				},
				{
					// Make pod-1 not ready
					lwsUpdateFn: func(lws *leaderworkerset.LeaderWorkerSet) {
						var sts appsv1.StatefulSet
						gomega.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: lws.Name + "-1", Namespace: lws.Namespace}, &sts)).To(gomega.Succeed())
						testing.SetStatefulsetToUnReady(ctx, k8sClient, &sts)
					},
					checkLWSState: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.ExpectLeaderWorkerSetUnavailable(ctx, k8sClient, lws, "All replicas are ready")
						testing.ExpectLeaderWorkerSetProgressing(ctx, k8sClient, lws, "Replicas are progressing")
						testing.ExpectLeaderWorkerSetStatusReplicas(ctx, k8sClient, lws, 3, 4)
					},
				},
				{
					// Update the worker template to trigger rolling update
					lwsUpdateFn: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.UpdateWorkerTemplate(ctx, k8sClient, lws)
					},
					checkLWSState: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.ExpectValidLeaderStatefulSet(ctx, k8sClient, lws, 4)
						testing.ExpectLeaderWorkerSetUnavailable(ctx, k8sClient, lws, "All replicas are ready")
						testing.ExpectLeaderWorkerSetProgressing(ctx, k8sClient, lws, "Replicas are progressing")
						testing.ExpectLeaderWorkerSetUpgradeInProgress(ctx, k8sClient, lws, "Rolling Upgrade is in progress")

						// With MaxUnavailable=1 and one pod already not ready (pod-1),
						// the partition should be adjusted to 3 instead of 2. If partition was moved to 2 that would bring
						// the number of unavailable pods to 3 (while MaxUnavailable=2).
						// This is the key test for the bugfix.
						testing.ExpectStatefulsetPartitionEqualTo(ctx, k8sClient, lws, 3)
						testing.ExpectLeaderWorkerSetStatusReplicas(ctx, k8sClient, lws, 3, 0)
					},
				},
				{
					// Complete the rolling update
					lwsUpdateFn: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.SetSuperPodToReady(ctx, k8sClient, lws, 4)
					},
					checkLWSState: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.ExpectValidLeaderStatefulSet(ctx, k8sClient, lws, 4)
						testing.ExpectValidWorkerStatefulSets(ctx, lws, k8sClient, true)
						testing.ExpectStatefulsetPartitionEqualTo(ctx, k8sClient, lws, 0)
						testing.ExpectLeaderWorkerSetAvailable(ctx, k8sClient, lws, "All replicas are ready")
						testing.ExpectLeaderWorkerSetNotProgressing(ctx, k8sClient, lws, "Replicas are progressing")
						testing.ExpectLeaderWorkerSetNoUpgradeInProgress(ctx, k8sClient, lws, "Rolling Upgrade is in progress")
						testing.ExpectLeaderWorkerSetStatusReplicas(ctx, k8sClient, lws, 4, 4)
					},
				},
			},
		}),
		ginkgo.Entry("rolling update with no ready replicas", &testCase{
			makeLeaderWorkerSet: func(nsName string) *wrappers.LeaderWorkerSetWrapper {
				return wrappers.BuildLeaderWorkerSet(nsName).Replica(2).MaxUnavailable(1)
			},
			updates: []*update{
				{
					// Set lws to available condition.
					lwsUpdateFn: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.SetSuperPodToReady(ctx, k8sClient, lws, 2)
					},
					checkLWSState: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.ExpectLeaderWorkerSetAvailable(ctx, k8sClient, lws, "All replicas are ready")
						testing.ExpectStatefulsetPartitionEqualTo(ctx, k8sClient, lws, 0)
						testing.ExpectValidLeaderStatefulSet(ctx, k8sClient, lws, 2)
						testing.ExpectValidWorkerStatefulSets(ctx, lws, k8sClient, true)
						testing.ExpectLeaderWorkerSetStatusReplicas(ctx, k8sClient, lws, 2, 2)
					},
				},
				{
					// Set replica 0 to not ready
					lwsUpdateFn: func(lws *leaderworkerset.LeaderWorkerSet) {
						var sts appsv1.StatefulSet
						gomega.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: lws.Name + "-0", Namespace: lws.Namespace}, &sts)).To(gomega.Succeed())
						testing.SetStatefulsetToUnReady(ctx, k8sClient, &sts)
					},
					checkLWSState: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.ExpectLeaderWorkerSetUnavailable(ctx, k8sClient, lws, "All replicas are ready")
						testing.ExpectStatefulsetPartitionEqualTo(ctx, k8sClient, lws, 0)
					},
				},
				{
					// Set replica 1 to not ready
					lwsUpdateFn: func(lws *leaderworkerset.LeaderWorkerSet) {
						var sts appsv1.StatefulSet
						gomega.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: lws.Name + "-1", Namespace: lws.Namespace}, &sts)).To(gomega.Succeed())
						testing.SetStatefulsetToUnReady(ctx, k8sClient, &sts)
					},
					checkLWSState: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.ExpectLeaderWorkerSetUnavailable(ctx, k8sClient, lws, "All replicas are ready")
						testing.ExpectStatefulsetPartitionEqualTo(ctx, k8sClient, lws, 0)
					},
				},
				{
					// Trigger rolling update
					lwsUpdateFn: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.UpdateWorkerTemplate(ctx, k8sClient, lws)
					},
					checkLWSState: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.ExpectValidLeaderStatefulSet(ctx, k8sClient, lws, 2)
						testing.ExpectLeaderWorkerSetUnavailable(ctx, k8sClient, lws, "All replicas are ready")
						testing.ExpectLeaderWorkerSetProgressing(ctx, k8sClient, lws, "Replicas are progressing")
						testing.ExpectLeaderWorkerSetUpgradeInProgress(ctx, k8sClient, lws, "Rolling Upgrade is in progress")
						testing.ExpectStatefulsetPartitionEqualTo(ctx, k8sClient, lws, 1)
						testing.ExpectLeaderWorkerSetStatusReplicas(ctx, k8sClient, lws, 0, 0)
					},
				},
				{
					// Make replica 1 ready
					lwsUpdateFn: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.SetPodGroupToReady(ctx, k8sClient, lws.Name+"-1", lws)
					},
					checkLWSState: func(lws *leaderworkerset.LeaderWorkerSet) {
						// Verify partition moves to 0 since replica 1 is now ready
						testing.ExpectStatefulsetPartitionEqualTo(ctx, k8sClient, lws, 0)
					},
				},
				{
					// Make replica 0 ready
					lwsUpdateFn: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.SetPodGroupToReady(ctx, k8sClient, lws.Name+"-0", lws)
					},
					checkLWSState: func(lws *leaderworkerset.LeaderWorkerSet) {
						// Verify both replicas are ready and updated
						testing.ExpectLeaderWorkerSetAvailable(ctx, k8sClient, lws, "All replicas are ready")
					},
				},
			},
		}),
		ginkgo.Entry("resize should update the size of the replicas", &testCase{
			makeLeaderWorkerSet: func(nsName string) *wrappers.LeaderWorkerSetWrapper {
				return wrappers.BuildLeaderWorkerSet(nsName).Replica(2).Size(2)
			},
			updates: []*update{
				{
					// Set lws to available condition.
					lwsUpdateFn: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.SetSuperPodToReady(ctx, k8sClient, lws, 2)
					},
					checkLWSState: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.ExpectLeaderWorkerSetAvailable(ctx, k8sClient, lws, "All replicas are ready")
						testing.ExpectStatefulsetPartitionEqualTo(ctx, k8sClient, lws, 0)
						testing.ExpectValidLeaderStatefulSet(ctx, k8sClient, lws, 2)
						testing.ExpectValidWorkerStatefulSets(ctx, lws, k8sClient, true)
						testing.ExpectLeaderWorkerSetStatusReplicas(ctx, k8sClient, lws, 2, 2)
					},
				},
				{
					// Update size to 3.
					lwsUpdateFn: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.UpdateSize(ctx, k8sClient, lws, 3)
						testing.SetSuperPodToReady(ctx, k8sClient, lws, 2)
					},
					checkLWSState: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.ExpectLeaderWorkerSetAvailable(ctx, k8sClient, lws, "All replicas are ready")
						testing.ExpectStatefulsetPartitionEqualTo(ctx, k8sClient, lws, 0)
						testing.ExpectValidLeaderStatefulSet(ctx, k8sClient, lws, 2)
						testing.ExpectValidWorkerStatefulSets(ctx, lws, k8sClient, true)
						testing.ExpectLeaderWorkerSetStatusReplicas(ctx, k8sClient, lws, 2, 2)
						testing.ValidateEvent(ctx, k8sClient, controllers.GroupsUpdating, corev1.EventTypeNormal, "Rolling Upgrade is in progress, with 2 groups ready of total 2 groups", lws.Namespace)
					},
				},
			},
		}),
		ginkgo.Entry("rolling update with the partition setting", &testCase{
			makeLeaderWorkerSet: func(nsName string) *wrappers.LeaderWorkerSetWrapper {
				return wrappers.BuildLeaderWorkerSet(nsName).Replica(6).Partition(4)
			},
			updates: []*update{
				{
					// Set lws to available condition.
					lwsUpdateFn: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.SetSuperPodToReady(ctx, k8sClient, lws, 6)
					},
					checkLWSState: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.ExpectLeaderWorkerSetAvailable(ctx, k8sClient, lws, "All replicas are ready")
						testing.ExpectStatefulsetPartitionEqualTo(ctx, k8sClient, lws, 4)
						testing.ExpectValidLeaderStatefulSet(ctx, k8sClient, lws, 6)
						testing.ExpectValidWorkerStatefulSets(ctx, lws, k8sClient, true)
						testing.ExpectLeaderWorkerSetStatusReplicas(ctx, k8sClient, lws, 6, 6)
					},
				},
				{
					// Trigger rolling update
					lwsUpdateFn: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.UpdateWorkerTemplate(ctx, k8sClient, lws)
					},
					checkLWSState: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.ExpectLeaderWorkerSetProgressing(ctx, k8sClient, lws, "Replicas are progressing")
						testing.ExpectLeaderWorkerSetUpgradeInProgress(ctx, k8sClient, lws, "Rolling Upgrade is in progress")
						testing.ExpectStatefulsetPartitionEqualTo(ctx, k8sClient, lws, 5)
						testing.ExpectValidLeaderStatefulSet(ctx, k8sClient, lws, 6)
						testing.ExpectLeaderWorkerSetStatusReplicas(ctx, k8sClient, lws, 6, 0)
					},
				},
				{
					// Make group-5 ready, should update group 4
					lwsUpdateFn: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.SetPodGroupToReady(ctx, k8sClient, lws.Name+"-5", lws)
					},
					checkLWSState: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.ExpectLeaderWorkerSetProgressing(ctx, k8sClient, lws, "Replicas are progressing")
						testing.ExpectLeaderWorkerSetUpgradeInProgress(ctx, k8sClient, lws, "Rolling Upgrade is in progress")
						testing.ExpectStatefulsetPartitionEqualTo(ctx, k8sClient, lws, 4)
						testing.ExpectValidLeaderStatefulSet(ctx, k8sClient, lws, 6)
						testing.ExpectLeaderWorkerSetStatusReplicas(ctx, k8sClient, lws, 6, 1)
					},
				},
				{
					// Make group-4 ready, partition update complete
					lwsUpdateFn: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.SetPodGroupToReady(ctx, k8sClient, lws.Name+"-4", lws)
					},
					checkLWSState: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.ExpectLeaderWorkerSetAvailable(ctx, k8sClient, lws, "All replicas are ready")
						testing.ExpectStatefulsetPartitionEqualTo(ctx, k8sClient, lws, 4)
						testing.ExpectValidLeaderStatefulSet(ctx, k8sClient, lws, 6)
						testing.ExpectLeaderWorkerSetStatusReplicas(ctx, k8sClient, lws, 6, 2)
					},
				},
				{
					// Set lws partition to 2, rolling update start
					lwsUpdateFn: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.SetLwsPartition(ctx, k8sClient, lws, 2)
					},
					checkLWSState: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.ExpectLeaderWorkerSetProgressing(ctx, k8sClient, lws, "Replicas are progressing")
						testing.ExpectLeaderWorkerSetUpgradeInProgress(ctx, k8sClient, lws, "Rolling Upgrade is in progress")
						testing.ExpectStatefulsetPartitionEqualTo(ctx, k8sClient, lws, 3)
						testing.ExpectValidLeaderStatefulSet(ctx, k8sClient, lws, 6)
						testing.ExpectLeaderWorkerSetStatusReplicas(ctx, k8sClient, lws, 6, 2)
					},
				},
				{
					// Make group-3 ready, should update group 2
					lwsUpdateFn: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.SetPodGroupToReady(ctx, k8sClient, lws.Name+"-3", lws)
					},
					checkLWSState: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.ExpectLeaderWorkerSetProgressing(ctx, k8sClient, lws, "Replicas are progressing")
						testing.ExpectLeaderWorkerSetUpgradeInProgress(ctx, k8sClient, lws, "Rolling Upgrade is in progress")
						testing.ExpectStatefulsetPartitionEqualTo(ctx, k8sClient, lws, 2)
						testing.ExpectValidLeaderStatefulSet(ctx, k8sClient, lws, 6)
						testing.ExpectLeaderWorkerSetStatusReplicas(ctx, k8sClient, lws, 6, 3)
					},
				},
				{
					// Make group-2 ready, partition update complete
					lwsUpdateFn: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.SetPodGroupToReady(ctx, k8sClient, lws.Name+"-2", lws)
					},
					checkLWSState: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.ExpectLeaderWorkerSetAvailable(ctx, k8sClient, lws, "All replicas are ready")
						testing.ExpectStatefulsetPartitionEqualTo(ctx, k8sClient, lws, 2)
						testing.ExpectValidLeaderStatefulSet(ctx, k8sClient, lws, 6)
						testing.ExpectLeaderWorkerSetStatusReplicas(ctx, k8sClient, lws, 6, 4)
					},
				},
			},
		}),
		ginkgo.Entry("rolling update with the partition and maxSurge setting", &testCase{
			makeLeaderWorkerSet: func(nsName string) *wrappers.LeaderWorkerSetWrapper {
				return wrappers.BuildLeaderWorkerSet(nsName).Replica(3).MaxSurge(1).Partition(2)
			},
			updates: []*update{
				{
					// Set lws to available condition.
					lwsUpdateFn: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.SetSuperPodToReady(ctx, k8sClient, lws, 3)
					},
					checkLWSState: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.ExpectLeaderWorkerSetAvailable(ctx, k8sClient, lws, "All replicas are ready")
						testing.ExpectStatefulsetPartitionEqualTo(ctx, k8sClient, lws, 2)
						testing.ExpectValidLeaderStatefulSet(ctx, k8sClient, lws, 3)
						testing.ExpectValidWorkerStatefulSets(ctx, lws, k8sClient, true)
						testing.ExpectLeaderWorkerSetStatusReplicas(ctx, k8sClient, lws, 3, 3)
					},
				},
				{
					// Trigger rolling update
					lwsUpdateFn: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.UpdateWorkerTemplate(ctx, k8sClient, lws)
					},
					checkLWSState: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.ExpectLeaderWorkerSetProgressing(ctx, k8sClient, lws, "Replicas are progressing")
						testing.ExpectLeaderWorkerSetUpgradeInProgress(ctx, k8sClient, lws, "Rolling Upgrade is in progress")
						testing.ExpectStatefulsetPartitionEqualTo(ctx, k8sClient, lws, 2)
						testing.ExpectValidLeaderStatefulSet(ctx, k8sClient, lws, 4)
						testing.ExpectLeaderWorkerSetStatusReplicas(ctx, k8sClient, lws, 3, 0)
					},
				},
				{
					// create surge pod
					lwsUpdateFn: func(lws *leaderworkerset.LeaderWorkerSet) {
						var leaderSts appsv1.StatefulSet
						testing.GetLeaderStatefulset(ctx, lws, k8sClient, &leaderSts)
						gomega.Expect(testing.CreateLeaderPods(ctx, leaderSts, k8sClient, lws, int(*lws.Spec.Replicas), int(*leaderSts.Spec.Replicas))).To(gomega.Succeed())
					},
				},
				{
					// the new group-3 and group-2 ready, will keep partition to 2
					lwsUpdateFn: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.SetPodGroupToReady(ctx, k8sClient, lws.Name+"-3", lws)
						testing.SetPodGroupToReady(ctx, k8sClient, lws.Name+"-2", lws)
					},
					checkLWSState: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.ExpectLeaderWorkerSetAvailable(ctx, k8sClient, lws, "All replicas are ready")
						testing.ExpectStatefulsetPartitionEqualTo(ctx, k8sClient, lws, 2)
						testing.ExpectValidLeaderStatefulSet(ctx, k8sClient, lws, 4)
						testing.ExpectLeaderWorkerSetStatusReplicas(ctx, k8sClient, lws, 4, 2)
					},
				},
				{
					// Set lws partition to 0, rolling update start
					lwsUpdateFn: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.SetLwsPartition(ctx, k8sClient, lws, 0)
					},
					checkLWSState: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.ExpectLeaderWorkerSetProgressing(ctx, k8sClient, lws, "Replicas are progressing")
						testing.ExpectLeaderWorkerSetUpgradeInProgress(ctx, k8sClient, lws, "Rolling Upgrade is in progress")
						testing.ExpectStatefulsetPartitionEqualTo(ctx, k8sClient, lws, 0)
						testing.ExpectValidLeaderStatefulSet(ctx, k8sClient, lws, 4)
						testing.ExpectLeaderWorkerSetStatusReplicas(ctx, k8sClient, lws, 4, 2)
					},
				},
				{
					// Make group-1,group-0 ready, update sts replicas to lws replicas
					lwsUpdateFn: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.SetPodGroupToReady(ctx, k8sClient, lws.Name+"-1", lws)
						testing.SetPodGroupToReady(ctx, k8sClient, lws.Name+"-0", lws)
					},
					checkLWSState: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.ExpectLeaderWorkerSetAvailable(ctx, k8sClient, lws, "All replicas are ready")
						testing.ExpectStatefulsetPartitionEqualTo(ctx, k8sClient, lws, 0)
						testing.ExpectValidLeaderStatefulSet(ctx, k8sClient, lws, 3)
						testing.ExpectLeaderWorkerSetStatusReplicas(ctx, k8sClient, lws, 4, 4)
					},
				},
				{
					// delete surge pod
					lwsUpdateFn: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.DeleteLeaderPod(ctx, k8sClient, lws, 3, 4)
					},
					checkLWSState: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.ExpectLeaderWorkerSetAvailable(ctx, k8sClient, lws, "All replicas are ready")
						testing.ExpectStatefulsetPartitionEqualTo(ctx, k8sClient, lws, 0)
						testing.ExpectValidLeaderStatefulSet(ctx, k8sClient, lws, 3)
						testing.ExpectLeaderWorkerSetStatusReplicas(ctx, k8sClient, lws, 3, 3)
					},
				},
			},
		}),
	) // end of DescribeTable

	ginkgo.Context("with gang scheduling enabled", ginkgo.Ordered, func() {
		ginkgo.Context("with volcano scheduler provider", ginkgo.Ordered, func() {
			ginkgo.BeforeAll(func() {
				// Create Volcano provider for gang scheduling tests
				sp, err := schedulerprovider.NewSchedulerProvider(schedulerprovider.Volcano, k8sClient)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				podController.SchedulerProvider = sp
			})

			ginkgo.AfterAll(func() {
				// Reset the SchedulerProvider to nil
				podController.SchedulerProvider = nil
			})

			type gangTestCase struct {
				makeLeaderWorkerSet func(nsName string) *wrappers.LeaderWorkerSetWrapper
				updates             []*update
				checkPodGroups      func(context.Context, client.Client, *leaderworkerset.LeaderWorkerSet) // New function to check PodGroups
			}

			ginkgo.DescribeTable("gang scheduling integration tests",
				func(tc *gangTestCase) {
					ctx := context.Background()
					// Create test namespace for each entry.
					ns := &corev1.Namespace{
						ObjectMeta: metav1.ObjectMeta{
							GenerateName: "lws-gang-ns-",
						},
					}
					gomega.Expect(k8sClient.Create(ctx, ns)).To(gomega.Succeed())

					// Create LeaderWorkerSet with volcano scheduler
					lws := tc.makeLeaderWorkerSet(ns.Name).Obj()
					if lws.Spec.LeaderWorkerTemplate.LeaderTemplate != nil {
						lws.Spec.LeaderWorkerTemplate.LeaderTemplate.Spec.SchedulerName = "volcano"
					}
					lws.Spec.LeaderWorkerTemplate.WorkerTemplate.Spec.SchedulerName = "volcano"
					// Verify LeaderWorkerSet created successfully.
					ginkgo.By(fmt.Sprintf("creating LeaderWorkerSet %s", lws.Name))
					gomega.Expect(k8sClient.Create(ctx, lws)).To(gomega.Succeed())
					var leaderSts appsv1.StatefulSet
					testing.GetLeaderStatefulset(ctx, lws, k8sClient, &leaderSts)
					// create leader pods for lws controller
					injectFn := func(pod *corev1.Pod) {
						err := podController.SchedulerProvider.InjectPodGroupMetadata(pod)
						gomega.Expect(err).NotTo(gomega.HaveOccurred())
					}
					gomega.Expect(testing.CreateLeaderPodsWithInjectFn(ctx, leaderSts, k8sClient, lws, 0, int(*lws.Spec.Replicas), injectFn)).To(gomega.Succeed())

					// Check PodGroups are created correctly
					if tc.checkPodGroups != nil {
						tc.checkPodGroups(ctx, k8sClient, lws)
					}

					// Perform a series of updates to LeaderWorkerSet resources and check
					// resulting LeaderWorkerSet state after each update.
					for _, up := range tc.updates {
						var leaderWorkerSet leaderworkerset.LeaderWorkerSet
						gomega.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: lws.Name, Namespace: lws.Namespace}, &leaderWorkerSet)).To(gomega.Succeed())
						if up.lwsUpdateFn != nil {
							up.lwsUpdateFn(&leaderWorkerSet)
						}

						// after update, get the latest lws
						gomega.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: lws.Name, Namespace: lws.Namespace}, &leaderWorkerSet)).To(gomega.Succeed())
						if up.checkLWSState != nil {
							up.checkLWSState(&leaderWorkerSet)
						}
						if up.checkLWSCondition != nil {
							up.checkLWSCondition(ctx, k8sClient, &leaderWorkerSet, testing.Timeout)
						}
					}
				},
				ginkgo.Entry("PodGroup creation with correct owner references", &gangTestCase{
					makeLeaderWorkerSet: func(nsName string) *wrappers.LeaderWorkerSetWrapper {
						return wrappers.BuildLeaderWorkerSet(nsName).Name("test-gang-basic").Replica(2)
					},
					checkPodGroups: func(ctx context.Context, c client.Client, lws *leaderworkerset.LeaderWorkerSet) {
						testing.ExpectValidPodGroups(ctx, c, schedulerprovider.Volcano, lws, int(*lws.Spec.Replicas))
					},
				}),
				ginkgo.Entry("Rolling update with PodGroup recreation", &gangTestCase{
					makeLeaderWorkerSet: func(nsName string) *wrappers.LeaderWorkerSetWrapper {
						return wrappers.BuildLeaderWorkerSet(nsName).Name("test-gang-rolling").Replica(3)
					},
					checkPodGroups: func(ctx context.Context, c client.Client, lws *leaderworkerset.LeaderWorkerSet) {
						testing.ExpectValidPodGroups(ctx, c, schedulerprovider.Volcano, lws, int(*lws.Spec.Replicas))
					},
					updates: []*update{
						{
							// Set LWS to available condition first
							lwsUpdateFn: func(lws *leaderworkerset.LeaderWorkerSet) {
								testing.SetSuperPodToReady(ctx, k8sClient, lws, 3)
							},
							checkLWSState: func(lws *leaderworkerset.LeaderWorkerSet) {
								testing.ExpectLeaderWorkerSetAvailable(ctx, k8sClient, lws, "All replicas are ready")
								testing.ExpectStatefulsetPartitionEqualTo(ctx, k8sClient, lws, 0)
								// Verify initial PodGroups are created correctly
								testing.ExpectValidPodGroups(ctx, k8sClient, schedulerprovider.Volcano, lws, int(*lws.Spec.Replicas))
							},
						},
						{
							// Trigger rolling update by updating worker template
							lwsUpdateFn: func(lws *leaderworkerset.LeaderWorkerSet) {
								testing.UpdateWorkerTemplate(ctx, k8sClient, lws)
							},
							checkLWSState: func(lws *leaderworkerset.LeaderWorkerSet) {
								// Verify rolling update has started
								testing.ExpectLeaderWorkerSetProgressing(ctx, k8sClient, lws, "Replicas are progressing")
								testing.ExpectLeaderWorkerSetUpgradeInProgress(ctx, k8sClient, lws, "Rolling Upgrade is in progress")
								// With 3 replicas and maxUnavailable=1, partition should start at 2 (updating replica 2 first)
								testing.ExpectStatefulsetPartitionEqualTo(ctx, k8sClient, lws, 2)
							},
						},
						{
							// Simulate replica-2 update completion
							lwsUpdateFn: func(lws *leaderworkerset.LeaderWorkerSet) {
								testing.UpdatePodGroupAtIndex(ctx, k8sClient, schedulerprovider.Volcano, podController.SchedulerProvider, lws, "2")
								// Set new replica-2 to ready
								testing.SetPodGroupToReady(ctx, k8sClient, lws.Name+"-2", lws)
							},
							checkLWSState: func(lws *leaderworkerset.LeaderWorkerSet) {
								testing.ExpectValidPodGroupAtIndex(ctx, k8sClient, schedulerprovider.Volcano, lws, "2")
								// Partition should move to 1 (next replica to update)
								testing.ExpectStatefulsetPartitionEqualTo(ctx, k8sClient, lws, 1)
							},
						},
						{
							// Simulate replica-1 update completion
							lwsUpdateFn: func(lws *leaderworkerset.LeaderWorkerSet) {
								testing.UpdatePodGroupAtIndex(ctx, k8sClient, schedulerprovider.Volcano, podController.SchedulerProvider, lws, "1")
								testing.SetPodGroupToReady(ctx, k8sClient, lws.Name+"-1", lws)
							},
							checkLWSState: func(lws *leaderworkerset.LeaderWorkerSet) {
								testing.ExpectValidPodGroupAtIndex(ctx, k8sClient, schedulerprovider.Volcano, lws, "1")
								// Partition should move to 0 (last replica to update)
								testing.ExpectStatefulsetPartitionEqualTo(ctx, k8sClient, lws, 0)
							},
						},
						{
							// Simulate replica-0 update completion
							lwsUpdateFn: func(lws *leaderworkerset.LeaderWorkerSet) {
								testing.UpdatePodGroupAtIndex(ctx, k8sClient, schedulerprovider.Volcano, podController.SchedulerProvider, lws, "0")
								testing.SetPodGroupToReady(ctx, k8sClient, lws.Name+"-0", lws)
							},
							checkLWSState: func(lws *leaderworkerset.LeaderWorkerSet) {
								testing.ExpectValidPodGroupAtIndex(ctx, k8sClient, schedulerprovider.Volcano, lws, "0")
								// Verify rolling update completion
								testing.ExpectLeaderWorkerSetAvailable(ctx, k8sClient, lws, "All replicas are ready")
								testing.ExpectLeaderWorkerSetNotProgressing(ctx, k8sClient, lws, "Replicas are progressing")
								testing.ExpectLeaderWorkerSetNoUpgradeInProgress(ctx, k8sClient, lws, "Rolling Upgrade is in progress")
							},
						},
					},
				}),
			)
		})
	}) // end of gang scheduling Context
}) // end of Describe

func ToUnstructured(o client.Object) (*unstructured.Unstructured, error) {
	serialized, err := json.Marshal(o)
	if err != nil {
		return nil, err
	}
	u := &unstructured.Unstructured{}
	return u, json.Unmarshal(serialized, u)
}

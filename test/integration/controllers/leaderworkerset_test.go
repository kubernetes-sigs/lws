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
	v1 "k8s.io/api/autoscaling/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	leaderworkerset "sigs.k8s.io/lws/api/leaderworkerset/v1"
	testing "sigs.k8s.io/lws/test/testutils"
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
		makeLeaderWorkerSet func(nsName string) *testing.LeaderWorkerSetWrapper
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
			makeLeaderWorkerSet: func(nsName string) *testing.LeaderWorkerSetWrapper {
				return testing.BuildLeaderWorkerSet(nsName).Replica(2)
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
						testing.ExpectValidReplicasCount(ctx, deployment, 3, k8sClient)
						testing.ExpectValidLeaderStatefulSet(ctx, deployment, k8sClient)
						testing.ExpectValidWorkerStatefulSets(ctx, deployment, k8sClient, true)
					},
				},
			},
		}),
		ginkgo.Entry("scale down number of groups", &testCase{
			makeLeaderWorkerSet: func(nsName string) *testing.LeaderWorkerSetWrapper {
				return testing.BuildLeaderWorkerSet(nsName).Replica(4)
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
						testing.ExpectValidReplicasCount(ctx, deployment, 3, k8sClient)
						testing.ExpectValidLeaderStatefulSet(ctx, deployment, k8sClient)
						testing.ExpectValidWorkerStatefulSets(ctx, deployment, k8sClient, true)
					},
				},
			},
		}),
		ginkgo.Entry("scale down to 0", &testCase{
			makeLeaderWorkerSet: func(nsName string) *testing.LeaderWorkerSetWrapper {
				return testing.BuildLeaderWorkerSet(nsName).Replica(2)
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
						testing.ExpectValidReplicasCount(ctx, deployment, 0, k8sClient)
						testing.ExpectValidLeaderStatefulSet(ctx, deployment, k8sClient)
						testing.ExpectValidWorkerStatefulSets(ctx, deployment, k8sClient, true)
					},
				},
			},
		}),
		ginkgo.Entry("scale up from 0", &testCase{
			makeLeaderWorkerSet: func(nsName string) *testing.LeaderWorkerSetWrapper {
				return testing.BuildLeaderWorkerSet(nsName).Replica(0)
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
						testing.ExpectValidReplicasCount(ctx, deployment, 3, k8sClient)
						testing.ExpectValidLeaderStatefulSet(ctx, deployment, k8sClient)
						testing.ExpectValidWorkerStatefulSets(ctx, deployment, k8sClient, true)
					},
				},
			},
		}),
		ginkgo.Entry("group size is 1", &testCase{
			makeLeaderWorkerSet: func(nsName string) *testing.LeaderWorkerSetWrapper {
				return testing.BuildLeaderWorkerSet(nsName).Size(1)
			},
			updates: []*update{
				{
					checkLWSState: func(deployment *leaderworkerset.LeaderWorkerSet) {
						testing.ExpectValidReplicasCount(ctx, deployment, 2, k8sClient)
						testing.ExpectValidLeaderStatefulSet(ctx, deployment, k8sClient)
						testing.ExpectValidWorkerStatefulSets(ctx, deployment, k8sClient, true)
					},
				},
			},
		}),
		ginkgo.Entry("zero replicas", &testCase{
			makeLeaderWorkerSet: func(nsName string) *testing.LeaderWorkerSetWrapper {
				return testing.BuildLeaderWorkerSet(nsName).Replica(0)
			},
			updates: []*update{
				{
					checkLWSState: func(deployment *leaderworkerset.LeaderWorkerSet) {
						testing.ExpectValidReplicasCount(ctx, deployment, 0, k8sClient)
						testing.ExpectValidLeaderStatefulSet(ctx, deployment, k8sClient)
						testing.ExpectValidWorkerStatefulSets(ctx, deployment, k8sClient, true)
					},
				},
			},
		}),
		ginkgo.Entry("Successfully create a leaderworkerset with 2 groups, size as 4.", &testCase{
			makeLeaderWorkerSet: testing.BuildLeaderWorkerSet,
			updates: []*update{
				{
					checkLWSState: func(deployment *leaderworkerset.LeaderWorkerSet) {
						testing.ExpectLeaderSetExist(ctx, deployment, k8sClient)
						testing.ExpectValidLeaderStatefulSet(ctx, deployment, k8sClient)
						testing.ExpectValidWorkerStatefulSets(ctx, deployment, k8sClient, true)
					},
				},
			},
		}),
		ginkgo.Entry("Deleted worker statefulSet will be recreated", &testCase{
			makeLeaderWorkerSet: func(nsName string) *testing.LeaderWorkerSetWrapper {
				return testing.BuildLeaderWorkerSet(nsName).Replica(2)
			},
			updates: []*update{
				{
					lwsUpdateFn: func(lws *leaderworkerset.LeaderWorkerSet) {
						var statefulsetToDelete appsv1.StatefulSet
						gomega.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: lws.Name + "-0", Namespace: lws.Namespace}, &statefulsetToDelete)).To(gomega.Succeed())
						gomega.Expect(k8sClient.Delete(ctx, &statefulsetToDelete)).To(gomega.Succeed())
					},
					checkLWSState: func(deployment *leaderworkerset.LeaderWorkerSet) {
						testing.ExpectValidLeaderStatefulSet(ctx, deployment, k8sClient)
						testing.ExpectValidWorkerStatefulSets(ctx, deployment, k8sClient, true)
					},
				},
			},
		}),
		ginkgo.Entry("headless service created", &testCase{
			makeLeaderWorkerSet: testing.BuildLeaderWorkerSet,
			updates: []*update{
				{
					checkLWSState: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.ExpectValidServices(ctx, k8sClient, lws)
					},
				},
			},
		}),
		ginkgo.Entry("service deleted will be recreated", &testCase{
			makeLeaderWorkerSet: testing.BuildLeaderWorkerSet,
			updates: []*update{
				{
					checkLWSState: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.ExpectValidServices(ctx, k8sClient, lws)
					},
				},
				{
					// Fetch the headless service and force delete it, so we can test if it is recreated.
					lwsUpdateFn: func(lws *leaderworkerset.LeaderWorkerSet) {
						var service corev1.Service
						gomega.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: lws.Name, Namespace: lws.Namespace}, &service)).To(gomega.Succeed())
						gomega.Expect(k8sClient.Delete(ctx, &service)).To(gomega.Succeed())
					},
					// Service should be recreated during reconcilation
					checkLWSState: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.ExpectValidServices(ctx, k8sClient, lws)
					},
				},
			},
		}),
		ginkgo.Entry("leader statefulset deleted will be recreated", &testCase{
			makeLeaderWorkerSet: testing.BuildLeaderWorkerSet,
			updates: []*update{
				{
					// Fetch the headless service and force delete it, so we can test if it is recreated.
					lwsUpdateFn: func(lws *leaderworkerset.LeaderWorkerSet) {
						var leaderSts appsv1.StatefulSet
						gomega.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: lws.Name, Namespace: lws.Namespace}, &leaderSts)).To(gomega.Succeed())
						gomega.Expect(k8sClient.Delete(ctx, &leaderSts)).To(gomega.Succeed())
					},
					// Service should be recreated during reconcilation
					checkLWSState: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.ExpectValidLeaderStatefulSet(ctx, lws, k8sClient)
					},
				},
			},
		}),
		ginkgo.Entry("Able to get scale subResource information", &testCase{
			makeLeaderWorkerSet: testing.BuildLeaderWorkerSet,
			updates: []*update{
				{
					checkLWSState: func(lws *leaderworkerset.LeaderWorkerSet) {
						var scale v1.Scale
						gomega.Expect(k8sClient.SubResource("scale").Get(ctx, lws, &scale)).To(gomega.Succeed())
						gomega.Expect(int32(scale.Spec.Replicas)).To(gomega.Equal(*lws.Spec.Replicas))
						gomega.Expect(int32(scale.Status.Replicas)).To(gomega.Equal(lws.Status.Replicas))
						gomega.Expect(lws.Status.HPAPodSelector).To(gomega.Equal("leaderworkerset.sigs.k8s.io/name=test-sample,leaderworkerset.sigs.k8s.io/worker-index=0"))
					},
				},
			},
		}),
		ginkgo.Entry("HPA is able trigger scaling up/down through scale endpoint", &testCase{
			makeLeaderWorkerSet: testing.BuildLeaderWorkerSet,
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
			makeLeaderWorkerSet: testing.BuildLeaderWorkerSet,
			updates: []*update{
				{
					lwsUpdateFn: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.SetPodGroupsToReady(ctx, k8sClient, lws)
					},
					checkLWSState: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.ExpectLeaderWorkerSetAvailable(ctx, k8sClient, lws, "All replicas are ready")
					},
				},
			},
		}),
		ginkgo.Entry("Testing condition switch from progressing to available to progressing", &testCase{
			makeLeaderWorkerSet: testing.BuildLeaderWorkerSet,
			updates: []*update{
				{
					checkLWSState: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.ExpectLeaderWorkerSetProgressing(ctx, k8sClient, lws, "Replicas are progressing")
						testing.ValidateLatestEvent(ctx, k8sClient, "GroupsAreProgressing", corev1.EventTypeNormal, "Replicas are progressing, with 0 groups ready of total 2 groups", lws.Namespace)
						// Force groups to ready.
						testing.SetPodGroupsToReady(ctx, k8sClient, lws)
						testing.ExpectLeaderWorkerSetNotProgressing(ctx, k8sClient, lws, "Replicas are progressing")
						testing.ExpectLeaderWorkerSetAvailable(ctx, k8sClient, lws, "All replicas are ready")
						testing.ValidateLatestEvent(ctx, k8sClient, "AllGroupsReady", corev1.EventTypeNormal, "All replicas are ready, with 2 groups ready of total 2 groups", lws.Namespace)
						// Force a reconcile. Refetch most recent version of LWS, increase replicas.
						patch := client.MergeFrom(&leaderworkerset.LeaderWorkerSet{
							ObjectMeta: metav1.ObjectMeta{
								Namespace: lws.Namespace,
								Name:      lws.Name,
							},
						})
						k8sClient.Patch(ctx, &leaderworkerset.LeaderWorkerSet{
							ObjectMeta: metav1.ObjectMeta{
								Namespace: lws.Namespace,
								Name:      lws.Name,
							},
							Spec: leaderworkerset.LeaderWorkerSetSpec{
								Replicas: ptr.To[int32](3),
							},
						}, patch)
						testing.ExpectLeaderWorkerSetProgressing(ctx, k8sClient, lws, "Replicas are progressing")
						testing.ExpectLeaderWorkerSetUnavailable(ctx, k8sClient, lws, "All replicas are ready")
						// Check most recent event.
						testing.ValidateLatestEvent(ctx, k8sClient, "GroupsAreProgressing", corev1.EventTypeNormal, "Replicas are progressing, with 2 groups ready of total 3 groups", lws.Namespace)
					},
				},
			},
		}),
		ginkgo.Entry("Leader/worker pods/Statefulset have required labels and annotations when exclusive placement enabled", &testCase{
			makeLeaderWorkerSet: func(nsName string) *testing.LeaderWorkerSetWrapper {
				return testing.BuildLeaderWorkerSet(nsName).ExclusivePlacement()
			},
			updates: []*update{
				{
					checkLWSState: func(deployment *leaderworkerset.LeaderWorkerSet) {
						testing.ExpectValidLeaderStatefulSet(ctx, deployment, k8sClient)
						testing.ExpectValidWorkerStatefulSets(ctx, deployment, k8sClient, false)
					},
				},
			},
		}),
		ginkgo.Entry("Pod restart will not recreate the pod group when restart policy is Default", &testCase{
			makeLeaderWorkerSet: func(nsName string) *testing.LeaderWorkerSetWrapper {
				return testing.BuildLeaderWorkerSet(nsName).RestartPolicy(leaderworkerset.DefaultRestartPolicy).Replica(1).Size(3)
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
			makeLeaderWorkerSet: func(nsName string) *testing.LeaderWorkerSetWrapper {
				return testing.BuildLeaderWorkerSet(nsName).RestartPolicy(leaderworkerset.RecreateGroupOnPodRestart).Replica(1).Size(3)
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
					},
				},
			},
		}),
		ginkgo.Entry("Replicas are processing will set condition to progressing with correct message with correct event", &testCase{
			makeLeaderWorkerSet: testing.BuildLeaderWorkerSet,
			updates: []*update{
				{
					checkLWSState: func(lws *leaderworkerset.LeaderWorkerSet) {
						gomega.Eventually(func() (int32, error) {
							var leaderWorkerSet leaderworkerset.LeaderWorkerSet
							if err := k8sClient.Get(ctx, types.NamespacedName{Name: lws.Name, Namespace: lws.Namespace}, &leaderWorkerSet); err != nil {
								return -1, err
							}
							return leaderWorkerSet.Status.Replicas, nil
						}, testing.Timeout, testing.Interval).Should(gomega.Equal(int32(2)))
						testing.ExpectValidLeaderStatefulSet(ctx, lws, k8sClient)
						testing.ExpectValidWorkerStatefulSets(ctx, lws, k8sClient, true)
						testing.ExpectLeaderWorkerSetProgressing(ctx, k8sClient, lws, "Replicas are progressing")
						testing.ValidateLatestEvent(ctx, k8sClient, "GroupsAreProgressing", corev1.EventTypeNormal, "Replicas are progressing, with 0 groups ready of total 2 groups", lws.Namespace)
					},
				},
			},
		}),
		ginkgo.Entry("Test default progressing state", &testCase{
			makeLeaderWorkerSet: testing.BuildLeaderWorkerSet,
			updates: []*update{
				{
					lwsUpdateFn: func(lws *leaderworkerset.LeaderWorkerSet) {
						lws.Status.Conditions = []metav1.Condition{}
						gomega.Expect(k8sClient.Status().Update(ctx, lws)).Should(gomega.Succeed())
					},
					checkLWSState: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.ExpectLeaderWorkerSetProgressing(ctx, k8sClient, lws, "Replicas are progressing")
					},
				},
			},
		}),
		ginkgo.Entry("Leaderworkerset has available state with correct event", &testCase{
			makeLeaderWorkerSet: testing.BuildLeaderWorkerSet,
			updates: []*update{
				{
					lwsUpdateFn: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.SetPodGroupsToReady(ctx, k8sClient, lws)
					},
					checkLWSState: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.ExpectLeaderWorkerSetAvailable(ctx, k8sClient, lws, "All replicas are ready")
						testing.ValidateLatestEvent(ctx, k8sClient, "AllGroupsReady", corev1.EventTypeNormal, "All replicas are ready, with 2 groups ready of total 2 groups", lws.Namespace)
					},
				},
			},
		}),

		// Rolling update test cases
		ginkgo.Entry("leaderTemplate changed with default strategy", &testCase{
			makeLeaderWorkerSet: func(nsName string) *testing.LeaderWorkerSetWrapper {
				return testing.BuildLeaderWorkerSet(nsName).Replica(4)
			},
			updates: []*update{
				{
					// Set lws to available condition.
					lwsUpdateFn: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.SetPodGroupsToReady(ctx, k8sClient, lws)
					},
					checkLWSState: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.ExpectLeaderWorkerSetAvailable(ctx, k8sClient, lws, "All replicas are ready")
						testing.ExpectStatefulsetPartitionEqualTo(ctx, k8sClient, lws, 0)
						testing.ExpectValidLeaderStatefulSet(ctx, lws, k8sClient)
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
						testing.ExpectValidLeaderStatefulSet(ctx, lws, k8sClient)
						testing.ExpectLeaderWorkerSetUnavailable(ctx, k8sClient, lws, "All replicas are ready")
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
						testing.ExpectValidLeaderStatefulSet(ctx, lws, k8sClient)
						testing.ExpectLeaderWorkerSetUnavailable(ctx, k8sClient, lws, "All replicas are ready")
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
						testing.ExpectStatefulsetPartitionEqualTo(ctx, k8sClient, lws, 2)
						testing.ExpectValidLeaderStatefulSet(ctx, lws, k8sClient)
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
						testing.ExpectStatefulsetPartitionEqualTo(ctx, k8sClient, lws, 2)
						testing.ExpectValidLeaderStatefulSet(ctx, lws, k8sClient)
						// 3-index status is unready but template already updated.
						testing.ExpectLeaderWorkerSetStatusReplicas(ctx, k8sClient, lws, 3, 2)
					},
				},
				{
					// Rolling update all the replicas will make the leader statefulset ready again.
					lwsUpdateFn: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.SetPodGroupsToReady(ctx, k8sClient, lws)
					},
					checkLWSState: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.ExpectValidLeaderStatefulSet(ctx, lws, k8sClient)
						testing.ExpectValidWorkerStatefulSets(ctx, lws, k8sClient, true)
						testing.ExpectStatefulsetPartitionEqualTo(ctx, k8sClient, lws, 0)
						testing.ExpectLeaderWorkerSetAvailable(ctx, k8sClient, lws, "All replicas are ready")
						testing.ExpectLeaderWorkerSetStatusReplicas(ctx, k8sClient, lws, 4, 4)
					},
				},
			},
		}),
		ginkgo.Entry("leaderTemplate changed with maxUnavailable=2", &testCase{
			makeLeaderWorkerSet: func(nsName string) *testing.LeaderWorkerSetWrapper {
				return testing.BuildLeaderWorkerSet(nsName).Replica(4).MaxUnavailable(2)
			},
			updates: []*update{
				{
					// Set lws to available condition.
					lwsUpdateFn: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.SetPodGroupsToReady(ctx, k8sClient, lws)
					},
					checkLWSState: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.ExpectStatefulsetPartitionEqualTo(ctx, k8sClient, lws, 0)
						testing.ExpectValidLeaderStatefulSet(ctx, lws, k8sClient)
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
						testing.ExpectValidLeaderStatefulSet(ctx, lws, k8sClient)
						testing.ExpectLeaderWorkerSetUnavailable(ctx, k8sClient, lws, "All replicas are ready")
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
						testing.ExpectValidLeaderStatefulSet(ctx, lws, k8sClient)
						testing.ExpectLeaderWorkerSetUnavailable(ctx, k8sClient, lws, "All replicas are ready")
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
						testing.ExpectValidLeaderStatefulSet(ctx, lws, k8sClient)
						testing.ExpectLeaderWorkerSetUnavailable(ctx, k8sClient, lws, "All replicas are ready")
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
						testing.ExpectValidLeaderStatefulSet(ctx, lws, k8sClient)
						testing.ExpectValidWorkerStatefulSets(ctx, lws, k8sClient, true)
						testing.ExpectLeaderWorkerSetAvailable(ctx, k8sClient, lws, "All replicas are ready")
						testing.ExpectStatefulsetPartitionEqualTo(ctx, k8sClient, lws, 0)
						testing.ExpectLeaderWorkerSetStatusReplicas(ctx, k8sClient, lws, 4, 4)
					},
				},
			},
		}),
		ginkgo.Entry("leaderTemplate changed with maxUnavailable greater than replicas", &testCase{
			makeLeaderWorkerSet: func(nsName string) *testing.LeaderWorkerSetWrapper {
				return testing.BuildLeaderWorkerSet(nsName).Replica(4).MaxUnavailable(10)
			},
			updates: []*update{
				{
					// Set lws to available condition.
					lwsUpdateFn: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.SetPodGroupsToReady(ctx, k8sClient, lws)
					},
					checkLWSState: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.ExpectStatefulsetPartitionEqualTo(ctx, k8sClient, lws, 0)
						testing.ExpectValidLeaderStatefulSet(ctx, lws, k8sClient)
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
						testing.ExpectValidLeaderStatefulSet(ctx, lws, k8sClient)
						testing.ExpectLeaderWorkerSetUnavailable(ctx, k8sClient, lws, "All replicas are ready")
						testing.ExpectStatefulsetPartitionEqualTo(ctx, k8sClient, lws, 0)
						testing.ExpectLeaderWorkerSetStatusReplicas(ctx, k8sClient, lws, 4, 0)
					},
				},
				{
					// Set all groups to ready.
					lwsUpdateFn: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.SetPodGroupsToReady(ctx, k8sClient, lws)
					},
					checkLWSState: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.ExpectValidLeaderStatefulSet(ctx, lws, k8sClient)
						testing.ExpectValidWorkerStatefulSets(ctx, lws, k8sClient, true)
						testing.ExpectStatefulsetPartitionEqualTo(ctx, k8sClient, lws, 0)
						testing.ExpectLeaderWorkerSetAvailable(ctx, k8sClient, lws, "All replicas are ready")
						testing.ExpectLeaderWorkerSetStatusReplicas(ctx, k8sClient, lws, 4, 4)
					},
				},
			},
		}),
		ginkgo.Entry("rolling update with both worker template and number of replicas changed", &testCase{
			makeLeaderWorkerSet: func(nsName string) *testing.LeaderWorkerSetWrapper {
				return testing.BuildLeaderWorkerSet(nsName).Replica(4)
			},
			updates: []*update{
				{
					// Set lws to available condition.
					lwsUpdateFn: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.SetPodGroupsToReady(ctx, k8sClient, lws)
					},
					checkLWSState: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.ExpectStatefulsetPartitionEqualTo(ctx, k8sClient, lws, 0)
						testing.ExpectValidLeaderStatefulSet(ctx, lws, k8sClient)
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
						testing.ExpectValidLeaderStatefulSet(ctx, lws, k8sClient)
						testing.ExpectLeaderWorkerSetUnavailable(ctx, k8sClient, lws, "All replicas are ready")
						// When scaling up the Replicas, Partition will not change, so the new created Pods will apply with the new template.
						testing.ExpectStatefulsetPartitionEqualTo(ctx, k8sClient, lws, 4)
						// We haven't set the replica-4, replica-5 to ready, so the readyReplicas is 4, the updatedReplicas is 0.
						testing.ExpectLeaderWorkerSetStatusReplicas(ctx, k8sClient, lws, 4, 0)
					},
				},
				{
					// Set all groups to ready.
					lwsUpdateFn: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.SetPodGroupsToReady(ctx, k8sClient, lws)
					},
					checkLWSState: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.ExpectValidLeaderStatefulSet(ctx, lws, k8sClient)
						testing.ExpectValidWorkerStatefulSets(ctx, lws, k8sClient, true)
						testing.ExpectStatefulsetPartitionEqualTo(ctx, k8sClient, lws, 0)
						testing.ExpectLeaderWorkerSetAvailable(ctx, k8sClient, lws, "All replicas are ready")
						testing.ExpectLeaderWorkerSetStatusReplicas(ctx, k8sClient, lws, 6, 6)
					},
				},
			},
		}),
		ginkgo.Entry("replicas increases during rolling update", &testCase{
			makeLeaderWorkerSet: func(nsName string) *testing.LeaderWorkerSetWrapper {
				return testing.BuildLeaderWorkerSet(nsName).Replica(4)
			},
			updates: []*update{
				{
					// Set lws to available condition.
					lwsUpdateFn: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.SetPodGroupsToReady(ctx, k8sClient, lws)
					},
					checkLWSState: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.ExpectLeaderWorkerSetAvailable(ctx, k8sClient, lws, "All replicas are ready")
						testing.ExpectStatefulsetPartitionEqualTo(ctx, k8sClient, lws, 0)
						testing.ExpectValidLeaderStatefulSet(ctx, lws, k8sClient)
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
						testing.ExpectValidLeaderStatefulSet(ctx, lws, k8sClient)
						testing.ExpectLeaderWorkerSetUnavailable(ctx, k8sClient, lws, "All replicas are ready")
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
						testing.ExpectValidLeaderStatefulSet(ctx, lws, k8sClient)
						testing.ExpectLeaderWorkerSetUnavailable(ctx, k8sClient, lws, "All replicas are ready")
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
						testing.ExpectValidLeaderStatefulSet(ctx, lws, k8sClient)
						testing.ExpectLeaderWorkerSetUnavailable(ctx, k8sClient, lws, "All replicas are ready")
						testing.ExpectStatefulsetPartitionEqualTo(ctx, k8sClient, lws, 2)
						testing.ExpectLeaderWorkerSetStatusReplicas(ctx, k8sClient, lws, 6, 3)
					},
				},
				{
					// Set all groups to ready.
					lwsUpdateFn: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.SetPodGroupsToReady(ctx, k8sClient, lws)
					},
					checkLWSState: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.ExpectValidLeaderStatefulSet(ctx, lws, k8sClient)
						testing.ExpectValidWorkerStatefulSets(ctx, lws, k8sClient, true)
						testing.ExpectStatefulsetPartitionEqualTo(ctx, k8sClient, lws, 0)
						testing.ExpectLeaderWorkerSetAvailable(ctx, k8sClient, lws, "All replicas are ready")
						testing.ExpectLeaderWorkerSetStatusReplicas(ctx, k8sClient, lws, 6, 6)
					},
				},
			},
		}),
		ginkgo.Entry("replicas decreases during rolling update", &testCase{
			makeLeaderWorkerSet: func(nsName string) *testing.LeaderWorkerSetWrapper {
				return testing.BuildLeaderWorkerSet(nsName).Replica(6)
			},
			updates: []*update{
				{
					// Set lws to available condition.
					lwsUpdateFn: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.SetPodGroupsToReady(ctx, k8sClient, lws)
					},
					checkLWSState: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.ExpectLeaderWorkerSetAvailable(ctx, k8sClient, lws, "All replicas are ready")
						testing.ExpectStatefulsetPartitionEqualTo(ctx, k8sClient, lws, 0)
						testing.ExpectValidLeaderStatefulSet(ctx, lws, k8sClient)
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
							leaderworkerset.Spec.LeaderWorkerTemplate.WorkerTemplate.Spec.Containers[0].Image = "nginx:1.16.1"
							return k8sClient.Update(ctx, &leaderworkerset)
						}, testing.Timeout, testing.Interval).Should(gomega.Succeed())
					},
					checkLWSState: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.ExpectValidLeaderStatefulSet(ctx, lws, k8sClient)
						testing.ExpectLeaderWorkerSetUnavailable(ctx, k8sClient, lws, "All replicas are ready")
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
						testing.ExpectValidLeaderStatefulSet(ctx, lws, k8sClient)
						testing.ExpectLeaderWorkerSetUnavailable(ctx, k8sClient, lws, "All replicas are ready")
						testing.ExpectStatefulsetPartitionEqualTo(ctx, k8sClient, lws, 2)
						testing.ExpectLeaderWorkerSetStatusReplicas(ctx, k8sClient, lws, 3, 0)
					},
				},
				{
					// Set all groups to ready.
					lwsUpdateFn: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.SetPodGroupsToReady(ctx, k8sClient, lws)
					},
					checkLWSState: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.ExpectValidLeaderStatefulSet(ctx, lws, k8sClient)
						testing.ExpectValidWorkerStatefulSets(ctx, lws, k8sClient, true)
						testing.ExpectStatefulsetPartitionEqualTo(ctx, k8sClient, lws, 0)
						testing.ExpectLeaderWorkerSetAvailable(ctx, k8sClient, lws, "All replicas are ready")
						testing.ExpectLeaderWorkerSetStatusReplicas(ctx, k8sClient, lws, 3, 3)
					},
				},
			},
		}),
	) // end of DescribeTable
}) // end of Describe

func ToUnstructured(o client.Object) (*unstructured.Unstructured, error) {
	serialized, err := json.Marshal(o)
	if err != nil {
		return nil, err
	}
	u := &unstructured.Unstructured{}
	return u, json.Unmarshal(serialized, u)
}

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
						testing.ExpectValidLeaderStatefulSet(ctx, k8sClient, deployment, 3)
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
						testing.ExpectValidLeaderStatefulSet(ctx, k8sClient, deployment, 3)
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
						testing.ExpectValidLeaderStatefulSet(ctx, k8sClient, deployment, 0)
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
						testing.ExpectValidLeaderStatefulSet(ctx, k8sClient, deployment, 3)
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
			makeLeaderWorkerSet: func(nsName string) *testing.LeaderWorkerSetWrapper {
				return testing.BuildLeaderWorkerSet(nsName).Replica(0)
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
			makeLeaderWorkerSet: testing.BuildLeaderWorkerSet,
			updates: []*update{
				{
					checkLWSState: func(deployment *leaderworkerset.LeaderWorkerSet) {
						testing.ExpectLeaderSetExist(ctx, deployment, k8sClient)
						testing.ExpectValidLeaderStatefulSet(ctx, k8sClient, deployment, 2)
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
						testing.ExpectValidLeaderStatefulSet(ctx, k8sClient, deployment, 2)
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
		ginkgo.Entry("subdomain policy LeadersSharedWorkersDedicated, more than one headless service created", &testCase{
			makeLeaderWorkerSet: func(nsName string) *testing.LeaderWorkerSetWrapper {
				return testing.BuildLeaderWorkerSet(nsName).SubdomainPolicy(leaderworkerset.SubdomainLeadersSharedWorkersDedicated)
			},
			updates: []*update{
				{
					checkLWSState: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.ExpectValidServices(ctx, k8sClient, lws)
					},
				},
			},
		}),
		ginkgo.Entry("able to create right amount of services, subdomain policy updated", &testCase{
			makeLeaderWorkerSet: func(nsName string) *testing.LeaderWorkerSetWrapper {
				return testing.BuildLeaderWorkerSet(nsName).SubdomainPolicy(leaderworkerset.SubdomainShared)
			},
			updates: []*update{
				{
					lwsUpdateFn: func(lws *leaderworkerset.LeaderWorkerSet) {
						var lwsToUpdate leaderworkerset.LeaderWorkerSet
						gomega.Eventually(func() error {
							if err := k8sClient.Get(ctx, types.NamespacedName{Name: lws.Name, Namespace: lws.Namespace}, &lwsToUpdate); err != nil {
								return err
							}
							lwsToUpdate.Spec.NetworkConfig.SubdomainPolicy = leaderworkerset.SubdomainLeadersSharedWorkersDedicated
							return k8sClient.Update(ctx, &lwsToUpdate)
						}, testing.Timeout, testing.Interval).Should(gomega.Succeed())
					},
					checkLWSState: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.ExpectValidServices(ctx, k8sClient, lws)
					},
				},
				{
					lwsUpdateFn: func(lws *leaderworkerset.LeaderWorkerSet) {
						var lwsToUpdate leaderworkerset.LeaderWorkerSet
						gomega.Eventually(func() error {
							if err := k8sClient.Get(ctx, types.NamespacedName{Name: lws.Name, Namespace: lws.Namespace}, &lwsToUpdate); err != nil {
								return err
							}
							lwsToUpdate.Spec.NetworkConfig.SubdomainPolicy = leaderworkerset.SubdomainShared
							return k8sClient.Update(ctx, &lwsToUpdate)
						}, testing.Timeout, testing.Interval).Should(gomega.Succeed())
					},
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
						testing.ExpectValidLeaderStatefulSet(ctx, k8sClient, lws, 2)
					},
				},
			},
		}),
		ginkgo.Entry("Able to get scale subResource information", &testCase{
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
						testing.SetPodGroupsToReady(ctx, k8sClient, lws, 2)
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
						testing.SetPodGroupsToReady(ctx, k8sClient, lws, 2)
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
					checkLWSState: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.ExpectValidLeaderStatefulSet(ctx, k8sClient, lws, 2)
						testing.ExpectValidWorkerStatefulSets(ctx, lws, k8sClient, false)
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
						testing.ExpectValidLeaderStatefulSet(ctx, k8sClient, lws, 2)
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
						testing.SetPodGroupsToReady(ctx, k8sClient, lws, 2)
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
						testing.SetPodGroupsToReady(ctx, k8sClient, lws, 4)
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
						testing.SetPodGroupsToReady(ctx, k8sClient, lws, 4)
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
		ginkgo.Entry("leaderTemplate changed with maxUnavailable=2", &testCase{
			makeLeaderWorkerSet: func(nsName string) *testing.LeaderWorkerSetWrapper {
				return testing.BuildLeaderWorkerSet(nsName).Replica(4).MaxUnavailable(2)
			},
			updates: []*update{
				{
					// Set lws to available condition.
					lwsUpdateFn: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.SetPodGroupsToReady(ctx, k8sClient, lws, 4)
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
		ginkgo.Entry("leaderTemplate changed with maxUnavailable greater than replicas", &testCase{
			makeLeaderWorkerSet: func(nsName string) *testing.LeaderWorkerSetWrapper {
				return testing.BuildLeaderWorkerSet(nsName).Replica(4).MaxUnavailable(10)
			},
			updates: []*update{
				{
					// Set lws to available condition.
					lwsUpdateFn: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.SetPodGroupsToReady(ctx, k8sClient, lws, 4)
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
						testing.SetPodGroupsToReady(ctx, k8sClient, lws, 4)
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
			makeLeaderWorkerSet: func(nsName string) *testing.LeaderWorkerSetWrapper {
				return testing.BuildLeaderWorkerSet(nsName).Replica(4)
			},
			updates: []*update{
				{
					// Set lws to available condition.
					lwsUpdateFn: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.SetPodGroupsToReady(ctx, k8sClient, lws, 4)
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
						testing.SetPodGroupsToReady(ctx, k8sClient, lws, 6)
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
			makeLeaderWorkerSet: func(nsName string) *testing.LeaderWorkerSetWrapper {
				return testing.BuildLeaderWorkerSet(nsName).Replica(4)
			},
			updates: []*update{
				{
					// Set lws to available condition.
					lwsUpdateFn: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.SetPodGroupsToReady(ctx, k8sClient, lws, 4)
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
						testing.SetPodGroupsToReady(ctx, k8sClient, lws, 6)
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
			makeLeaderWorkerSet: func(nsName string) *testing.LeaderWorkerSetWrapper {
				return testing.BuildLeaderWorkerSet(nsName).Replica(6)
			},
			updates: []*update{
				{
					// Set lws to available condition.
					lwsUpdateFn: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.SetPodGroupsToReady(ctx, k8sClient, lws, 6)
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
							leaderworkerset.Spec.LeaderWorkerTemplate.WorkerTemplate.Spec.Containers[0].Image = "nginx:1.16.1"
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
						testing.SetPodGroupsToReady(ctx, k8sClient, lws, 3)
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
			makeLeaderWorkerSet: func(nsName string) *testing.LeaderWorkerSetWrapper {
				return testing.BuildLeaderWorkerSet(nsName).Replica(4).MaxSurge(1)
			},
			updates: []*update{
				{
					// Set lws to available condition.
					lwsUpdateFn: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.SetPodGroupsToReady(ctx, k8sClient, lws, 4)
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
			makeLeaderWorkerSet: func(nsName string) *testing.LeaderWorkerSetWrapper {
				return testing.BuildLeaderWorkerSet(nsName).MaxSurge(2)
			},
			updates: []*update{
				{
					// Set lws to available condition.
					lwsUpdateFn: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.SetPodGroupsToReady(ctx, k8sClient, lws, 2)
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
						testing.SetPodGroupsToReady(ctx, k8sClient, lws, 6)
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
			makeLeaderWorkerSet: func(nsName string) *testing.LeaderWorkerSetWrapper {
				return testing.BuildLeaderWorkerSet(nsName).Replica(6).MaxSurge(2)
			},
			updates: []*update{
				{
					// Set lws to available condition.
					lwsUpdateFn: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.SetPodGroupsToReady(ctx, k8sClient, lws, 6)
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
						testing.SetPodGroupsToReady(ctx, k8sClient, lws, 5)
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
			makeLeaderWorkerSet: func(nsName string) *testing.LeaderWorkerSetWrapper {
				return testing.BuildLeaderWorkerSet(nsName).MaxSurge(4)
			},
			updates: []*update{
				{
					// Set lws to available condition.
					lwsUpdateFn: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.SetPodGroupsToReady(ctx, k8sClient, lws, 2)
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
						testing.SetPodGroupsToReady(ctx, k8sClient, lws, 3)
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
			makeLeaderWorkerSet: func(nsName string) *testing.LeaderWorkerSetWrapper {
				return testing.BuildLeaderWorkerSet(nsName).Replica(4).MaxSurge(2)
			},
			updates: []*update{
				{
					// Set lws to available condition.
					lwsUpdateFn: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.SetPodGroupsToReady(ctx, k8sClient, lws, 4)
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
			makeLeaderWorkerSet: func(nsName string) *testing.LeaderWorkerSetWrapper {
				return testing.BuildLeaderWorkerSet(nsName).Replica(4).MaxSurge(2)
			},
			updates: []*update{
				{
					// Set lws to available condition.
					lwsUpdateFn: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.SetPodGroupsToReady(ctx, k8sClient, lws, 4)
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
					},
				},
				{
					// Set all groups to ready.
					lwsUpdateFn: func(lws *leaderworkerset.LeaderWorkerSet) {
						// We didn't delete the leader pod yet.
						testing.SetPodGroupsToReady(ctx, k8sClient, lws, 6)
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
		ginkgo.Entry("create a leaderworkerset with spec.startupPolicy=LeaderReady", &testCase{
			makeLeaderWorkerSet: func(nsName string) *testing.LeaderWorkerSetWrapper {
				return testing.BuildLeaderWorkerSet(nsName).Replica(4).StartupPolicy(leaderworkerset.LeaderReadyStartupPolicy)
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
			makeLeaderWorkerSet: func(nsName string) *testing.LeaderWorkerSetWrapper {
				return testing.BuildLeaderWorkerSet(nsName).Replica(4).StartupPolicy(leaderworkerset.LeaderCreatedStartupPolicy)
			},
			updates: []*update{
				{
					checkLWSState: func(lws *leaderworkerset.LeaderWorkerSet) {
						testing.ExpectSpecifiedWorkerStatefulSetsCreated(ctx, k8sClient, lws, 0, 4)
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

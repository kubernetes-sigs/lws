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

package webhooks

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/go-cmp/cmp"
	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	leaderworkerset "sigs.k8s.io/lws/api/leaderworkerset/v1"
	acceleratorutils "sigs.k8s.io/lws/pkg/utils/accelerators"
	"sigs.k8s.io/lws/pkg/webhooks"
	testutils "sigs.k8s.io/lws/test/testutils"
)

var _ = ginkgo.Describe("leaderworkerset pod defaulting, creation and update", func() {
	// Each test runs in a separate namespace.
	var ns *corev1.Namespace
	ginkgo.BeforeEach(func() {
		// Create test namespace before each test.
		ns = &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: "test-ns-",
			},
		}
		gomega.Expect(k8sClient.Create(ctx, ns)).To(gomega.Succeed())

		gomega.Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns.Namespace, Name: ns.Name}, ns)).Should(gomega.Succeed())
	})
	type testDefaultingCase struct {
		makePod          func(ns *corev1.Namespace) corev1.Pod
		checkExpectedPod func(expected corev1.Pod, got corev1.Pod) error
	}
	ginkgo.DescribeTable("test defaulting",
		func(tc *testDefaultingCase) {
			ctx := context.Background()
			// Create LeaderWorkerSet pods
			ginkgo.By("creating lws pod")
			expectedPod := tc.makePod(ns)
			gomega.Expect(k8sClient.Create(ctx, &expectedPod)).To(gomega.Succeed())
			var gotPod corev1.Pod
			gomega.Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: expectedPod.Namespace, Name: expectedPod.Name}, &gotPod)).To(gomega.Succeed())
			gomega.Expect(tc.checkExpectedPod(expectedPod, gotPod)).To(gomega.Succeed())
		},
		ginkgo.Entry("not leaderworkerset pods should have no effect", &testDefaultingCase{
			makePod: func(ns *corev1.Namespace) corev1.Pod {
				return corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "randompod",
						Namespace: ns.Name,
						Labels: map[string]string{
							"random-label": "random-value",
						},
					},
					Spec: testutils.MakeLeaderPodSpec(),
				}
			},
			checkExpectedPod: func(expected corev1.Pod, got corev1.Pod) error {
				// since the pod webhook only mutates labels, we only do label diffing here
				if diff := cmp.Diff(got.Labels, map[string]string{
					"random-label": "random-value",
				}); diff != "" {
					return errors.New("pod labels mismatch: " + diff)
				}
				return nil
			},
		}),
		ginkgo.Entry("worker index label is populated for worker pods", &testDefaultingCase{
			makePod: func(ns *corev1.Namespace) corev1.Pod {
				return corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-1-1",
						Namespace: ns.Name,
						Labels: map[string]string{
							leaderworkerset.SetNameLabelKey:    "test",
							leaderworkerset.GroupIndexLabelKey: "1",
						},
					},
					Spec: testutils.MakeWorkerPodSpec(),
				}
			},
			checkExpectedPod: func(expected corev1.Pod, got corev1.Pod) error {
				if diff := cmp.Diff(got.Labels, map[string]string{
					leaderworkerset.SetNameLabelKey:     "test",
					leaderworkerset.WorkerIndexLabelKey: "1",
					leaderworkerset.GroupIndexLabelKey:  "1",
				}); diff != "" {
					return errors.New("pod labels mismatch: " + diff)
				}
				return nil
			},
		}),
		ginkgo.Entry("worker index label is populated for leader pods", &testDefaultingCase{
			makePod: func(ns *corev1.Namespace) corev1.Pod {
				return corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-1",
						Namespace: ns.Name,
						Labels: map[string]string{
							leaderworkerset.SetNameLabelKey: "test",
							// expect the worker index label already be populated
							leaderworkerset.WorkerIndexLabelKey: "0",
						},
					},
					Spec: testutils.MakeWorkerPodSpec(),
				}
			},
			checkExpectedPod: func(expected corev1.Pod, got corev1.Pod) error {
				if got.Labels[leaderworkerset.GroupUniqueHashLabelKey] != "" {
					got.Labels[leaderworkerset.GroupUniqueHashLabelKey] = "uniqueHash"
				}
				if diff := cmp.Diff(got.Labels, map[string]string{
					leaderworkerset.GroupIndexLabelKey:      "1",
					leaderworkerset.SetNameLabelKey:         "test",
					leaderworkerset.GroupUniqueHashLabelKey: "uniqueHash",
					leaderworkerset.WorkerIndexLabelKey:     "0",
				}); diff != "" {
					return errors.New("pod labels mismatch: " + diff)
				}
				return nil
			},
		}),
		ginkgo.Entry("Leader requests TPU resources, expect subgroup labels to be populated in leader", &testDefaultingCase{
			makePod: func(ns *corev1.Namespace) corev1.Pod {
				return corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-1",
						Namespace: ns.Name,
						Labels: map[string]string{
							leaderworkerset.SetNameLabelKey: "test",
							// expect the worker index label already be populated
							leaderworkerset.WorkerIndexLabelKey: "0",
							// expect subgroupsize label to already be populated
							leaderworkerset.SubGroupSizeLabelKey: "4",
						},
						Annotations: map[string]string{
							leaderworkerset.SizeAnnotationKey:                "5",
							acceleratorutils.LeaderRequestsTPUsAnnotationKey: "true",
						},
					},
					Spec: testutils.MakeLeaderPodSpecWithTPUResource(),
				}
			},
			checkExpectedPod: func(expected corev1.Pod, got corev1.Pod) error {
				if got.Labels[leaderworkerset.GroupUniqueHashLabelKey] != "" {
					got.Labels[leaderworkerset.GroupUniqueHashLabelKey] = "uniqueHash"
				}
				if diff := cmp.Diff(got.Labels, map[string]string{
					leaderworkerset.SubGroupSizeLabelKey:        "4",
					leaderworkerset.GroupIndexLabelKey:          "1",
					leaderworkerset.SetNameLabelKey:             "test",
					leaderworkerset.GroupUniqueHashLabelKey:     "uniqueHash",
					leaderworkerset.WorkerIndexLabelKey:         "0",
					leaderworkerset.SubGroupIndexLabelKey:       "0",
					leaderworkerset.SubGroupWorkerIndexLabelKey: "0",
				}); diff != "" {
					return errors.New("pod labels mismatch: " + diff)
				}
				return nil
			},
		}),
		ginkgo.Entry("Leader requests TPU resources, expect subgroup labels in worker to be populated", &testDefaultingCase{
			makePod: func(ns *corev1.Namespace) corev1.Pod {
				return corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-1-3",
						Namespace: ns.Name,
						Labels: map[string]string{
							leaderworkerset.SetNameLabelKey: "test",
							// expect the worker index label already be populated
							leaderworkerset.WorkerIndexLabelKey: "3",
							// expect subgroupsize label to already be populated
							leaderworkerset.SubGroupSizeLabelKey: "2",
						},
						Annotations: map[string]string{
							leaderworkerset.SizeAnnotationKey:                "4",
							acceleratorutils.LeaderRequestsTPUsAnnotationKey: "true",
						},
					},
					Spec: testutils.MakeWorkerPodSpec(),
				}
			},
			checkExpectedPod: func(expected corev1.Pod, got corev1.Pod) error {
				if got.Labels[leaderworkerset.GroupUniqueHashLabelKey] != "" {
					got.Labels[leaderworkerset.GroupUniqueHashLabelKey] = "uniqueHash"
				}
				if diff := cmp.Diff(got.Labels, map[string]string{
					leaderworkerset.SubGroupSizeLabelKey:        "2",
					leaderworkerset.SetNameLabelKey:             "test",
					leaderworkerset.GroupUniqueHashLabelKey:     "uniqueHash",
					leaderworkerset.WorkerIndexLabelKey:         "3",
					leaderworkerset.SubGroupIndexLabelKey:       "1",
					leaderworkerset.SubGroupWorkerIndexLabelKey: "1",
				}); diff != "" {
					return errors.New("pod labels mismatch: " + diff)
				}
				return nil
			},
		}),
		ginkgo.Entry("Leader doesn't request TPU resources, expect subgroup labels in worker to be populated", &testDefaultingCase{
			makePod: func(ns *corev1.Namespace) corev1.Pod {
				return corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-1-4",
						Namespace: ns.Name,
						Labels: map[string]string{
							leaderworkerset.SetNameLabelKey: "test",
							// expect the worker index label already be populated
							leaderworkerset.WorkerIndexLabelKey: "4",
							// expect subgroupsize label to already be populated
							leaderworkerset.SubGroupSizeLabelKey: "2",
						},
						Annotations: map[string]string{
							leaderworkerset.SizeAnnotationKey: "5",
						},
					},
					Spec: testutils.MakeWorkerPodSpec(),
				}
			},
			checkExpectedPod: func(expected corev1.Pod, got corev1.Pod) error {
				if got.Labels[leaderworkerset.GroupUniqueHashLabelKey] != "" {
					got.Labels[leaderworkerset.GroupUniqueHashLabelKey] = "uniqueHash"
				}
				if diff := cmp.Diff(got.Labels, map[string]string{
					leaderworkerset.SubGroupSizeLabelKey:        "2",
					leaderworkerset.SetNameLabelKey:             "test",
					leaderworkerset.GroupUniqueHashLabelKey:     "uniqueHash",
					leaderworkerset.WorkerIndexLabelKey:         "4",
					leaderworkerset.SubGroupIndexLabelKey:       "1",
					leaderworkerset.SubGroupWorkerIndexLabelKey: "1",
				}); diff != "" {
					return errors.New("pod labels mismatch: " + diff)
				}
				return nil
			},
		}),
		ginkgo.Entry("Pod requesting TPUs which doesn't belong to LWS will not have TPU env vars populated", &testDefaultingCase{
			makePod: func(ns *corev1.Namespace) corev1.Pod {
				return corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-1",
						Namespace: ns.Name,
					},
					Spec: testutils.MakeWorkerPodSpecWithTPUResource(),
				}
			},
			checkExpectedPod: func(expected corev1.Pod, got corev1.Pod) error {
				if testutils.HasTPUEnvVarsPopulated(got) {
					return fmt.Errorf("got unexpected TPU env vars for pod %s", got.Name)
				}
				return nil
			},
		}),
		ginkgo.Entry("Pod not requesting TPUs will not have env vars populated for leader pods", &testDefaultingCase{
			makePod: func(ns *corev1.Namespace) corev1.Pod {
				return corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-1",
						Namespace: ns.Name,
						Labels: map[string]string{
							leaderworkerset.SetNameLabelKey:     "test",
							leaderworkerset.WorkerIndexLabelKey: "0",
						},
					},
					Spec: testutils.MakeWorkerPodSpec(),
				}
			},
			checkExpectedPod: func(expected corev1.Pod, got corev1.Pod) error {
				if testutils.HasTPUEnvVarsPopulated(got) {
					return fmt.Errorf("got unexpected TPU env vars for pod %s", got.Name)
				}
				return nil
			},
		}),
		ginkgo.Entry("Pod not requesting TPUs will not have env vars populated for worker pods", &testDefaultingCase{
			makePod: func(ns *corev1.Namespace) corev1.Pod {
				return corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-1-2",
						Namespace: ns.Name,
						Labels: map[string]string{
							leaderworkerset.SetNameLabelKey:    "test",
							leaderworkerset.GroupIndexLabelKey: "1",
						},
					},
					Spec: testutils.MakeWorkerPodSpec(),
				}
			},
			checkExpectedPod: func(expected corev1.Pod, got corev1.Pod) error {
				if testutils.HasTPUEnvVarsPopulated(got) {
					return fmt.Errorf("got unexpected TPU env vars for pod %s", got.Name)
				}
				return nil
			},
		}),
		ginkgo.Entry("Pod requesting TPUs in lws with size 5 will have env var populated in leader pod", &testDefaultingCase{
			makePod: func(ns *corev1.Namespace) corev1.Pod {
				return corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-sample-1",
						Namespace: ns.Name,
						Labels: map[string]string{
							leaderworkerset.SetNameLabelKey:     "test-sample",
							leaderworkerset.WorkerIndexLabelKey: "0",
						},
						Annotations: map[string]string{
							leaderworkerset.SizeAnnotationKey: "5",
						},
					},
					Spec: testutils.MakeLeaderPodSpecWithTPUResource(),
				}
			},
			checkExpectedPod: func(expected corev1.Pod, got corev1.Pod) error {
				if !testutils.HasTPUEnvVarsPopulated(got) {
					return fmt.Errorf("should expect TPU env vars for pod %s", got.Name)
				}
				if err := testutils.CheckTPUContainerHasCorrectEnvVars(got, "test-sample-1.default,test-sample-1-1.default,test-sample-1-2.default,test-sample-1-3.default,test-sample-1-4.default"); err != nil {
					return err
				}
				return nil
			},
		}),
		ginkgo.Entry("Pod requesting TPUs in lws with subgroupsize 5 will have env var populated in leader pod", &testDefaultingCase{
			makePod: func(ns *corev1.Namespace) corev1.Pod {
				return corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-sample-1",
						Namespace: ns.Name,
						Labels: map[string]string{
							leaderworkerset.SetNameLabelKey:      "test-sample",
							leaderworkerset.WorkerIndexLabelKey:  "0",
							leaderworkerset.SubGroupSizeLabelKey: "5",
						},
						Annotations: map[string]string{
							leaderworkerset.SizeAnnotationKey: "5",
						},
					},
					Spec: testutils.MakeLeaderPodSpecWithTPUResource(),
				}
			},
			checkExpectedPod: func(expected corev1.Pod, got corev1.Pod) error {
				if !testutils.HasTPUEnvVarsPopulated(got) {
					return fmt.Errorf("should expect TPU env vars for pod %s", got.Name)
				}
				if err := testutils.CheckTPUContainerHasCorrectEnvVars(got, "test-sample-1.default,test-sample-1-1.default,test-sample-1-2.default,test-sample-1-3.default,test-sample-1-4.default"); err != nil {
					return err
				}
				return nil
			},
		}),
		ginkgo.Entry("Worker Pod requesting TPUs in lws, leader too", &testDefaultingCase{
			makePod: func(ns *corev1.Namespace) corev1.Pod {
				return corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-sample-1-3",
						Namespace: ns.Name,
						Labels: map[string]string{
							leaderworkerset.SetNameLabelKey:     "test-sample",
							leaderworkerset.WorkerIndexLabelKey: "3",
							leaderworkerset.GroupIndexLabelKey:  "1",
						},
						Annotations: map[string]string{
							leaderworkerset.SizeAnnotationKey:                "5",
							acceleratorutils.LeaderRequestsTPUsAnnotationKey: "true",
						},
					},
					Spec: testutils.MakeWorkerPodSpecWithTPUResource(),
				}
			},
			checkExpectedPod: func(expected corev1.Pod, got corev1.Pod) error {
				if !testutils.HasTPUEnvVarsPopulated(got) {
					return fmt.Errorf("should expect TPU env vars for pod %s", got.Name)
				}
				if err := testutils.CheckTPUContainerHasCorrectEnvVars(got, "test-sample-1.default,test-sample-1-1.default,test-sample-1-2.default,test-sample-1-3.default,test-sample-1-4.default"); err != nil {
					return err
				}
				return nil
			},
		}),
		ginkgo.Entry("Worker Pod requesting TPUs in lws using subgrouping, leader too", &testDefaultingCase{
			makePod: func(ns *corev1.Namespace) corev1.Pod {
				return corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-sample-1-3",
						Namespace: ns.Name,
						Labels: map[string]string{
							leaderworkerset.SetNameLabelKey:      "test-sample",
							leaderworkerset.WorkerIndexLabelKey:  "3",
							leaderworkerset.SubGroupSizeLabelKey: "5",
						},
						Annotations: map[string]string{
							leaderworkerset.SizeAnnotationKey:                "10",
							acceleratorutils.LeaderRequestsTPUsAnnotationKey: "true",
						},
					},
					Spec: testutils.MakeWorkerPodSpecWithTPUResource(),
				}
			},
			checkExpectedPod: func(expected corev1.Pod, got corev1.Pod) error {
				if !testutils.HasTPUEnvVarsPopulated(got) {
					return fmt.Errorf("should expect TPU env vars for pod %s", got.Name)
				}
				if err := testutils.CheckTPUContainerHasCorrectEnvVars(got, "test-sample-1.default,test-sample-1-1.default,test-sample-1-2.default,test-sample-1-3.default,test-sample-1-4.default"); err != nil {
					return err
				}
				return nil
			},
		}),
		ginkgo.Entry("Worker Pod requesting TPUs in lws, leader doesn't", &testDefaultingCase{
			makePod: func(ns *corev1.Namespace) corev1.Pod {
				return corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-sample-1-3",
						Namespace: ns.Name,
						Labels: map[string]string{
							leaderworkerset.SetNameLabelKey:     "test-sample",
							leaderworkerset.WorkerIndexLabelKey: "3",
							leaderworkerset.GroupIndexLabelKey:  "1",
						},
						Annotations: map[string]string{
							leaderworkerset.SizeAnnotationKey: "5",
						},
					},
					Spec: testutils.MakeWorkerPodSpecWithTPUResource(),
				}
			},
			checkExpectedPod: func(expected corev1.Pod, got corev1.Pod) error {
				if !testutils.HasTPUEnvVarsPopulated(got) {
					return fmt.Errorf("should expect TPU env vars for pod %s", got.Name)
				}
				if err := testutils.CheckTPUContainerHasCorrectEnvVars(got, "test-sample-1-1.default,test-sample-1-2.default,test-sample-1-3.default,test-sample-1-4.default"); err != nil {
					return err
				}
				return nil
			},
		}),
		ginkgo.Entry("Worker Pod requesting TPUs in lws using subgrouping, leader doesn't", &testDefaultingCase{
			makePod: func(ns *corev1.Namespace) corev1.Pod {
				return corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-sample-1-7",
						Namespace: ns.Name,
						Labels: map[string]string{
							leaderworkerset.SetNameLabelKey:      "test-sample",
							leaderworkerset.WorkerIndexLabelKey:  "7",
							leaderworkerset.SubGroupSizeLabelKey: "5",
						},
						Annotations: map[string]string{
							leaderworkerset.SizeAnnotationKey: "11",
						},
					},
					Spec: testutils.MakeWorkerPodSpecWithTPUResource(),
				}
			},
			checkExpectedPod: func(expected corev1.Pod, got corev1.Pod) error {
				if !testutils.HasTPUEnvVarsPopulated(got) {
					return fmt.Errorf("should expect TPU env vars for pod %s", got.Name)
				}
				if err := testutils.CheckTPUContainerHasCorrectEnvVars(got, "test-sample-1-6.default,test-sample-1-7.default,test-sample-1-8.default,test-sample-1-9.default,test-sample-1-10.default"); err != nil {
					return err
				}
				return nil
			},
		}),
		ginkgo.Entry("Pod requesting TPUs in lws with size 2 will have env var populated in worker pod", &testDefaultingCase{
			makePod: func(ns *corev1.Namespace) corev1.Pod {
				return corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-sample-1-1",
						Namespace: ns.Name,
						Labels: map[string]string{
							leaderworkerset.SetNameLabelKey:     "test-sample",
							leaderworkerset.WorkerIndexLabelKey: "1",
							leaderworkerset.GroupIndexLabelKey:  "1",
						},
						Annotations: map[string]string{
							leaderworkerset.SizeAnnotationKey:                "2",
							acceleratorutils.LeaderRequestsTPUsAnnotationKey: "true",
						},
					},
					Spec: testutils.MakeWorkerPodSpecWithTPUResource(),
				}
			},
			checkExpectedPod: func(expected corev1.Pod, got corev1.Pod) error {
				if !testutils.HasTPUEnvVarsPopulated(got) {
					return fmt.Errorf("should expect TPU env vars for pod %s", got.Name)
				}
				if err := testutils.CheckTPUContainerHasCorrectEnvVars(got, "test-sample-1.default,test-sample-1-1.default"); err != nil {
					return err
				}
				return nil
			},
		}),
		ginkgo.Entry("Pod requesting TPUs in lws with size 0 will have env var populated in worker pod", &testDefaultingCase{
			makePod: func(ns *corev1.Namespace) corev1.Pod {
				return corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-sample-1",
						Namespace: ns.Name,
						Labels: map[string]string{
							leaderworkerset.SetNameLabelKey:     "test-sample",
							leaderworkerset.WorkerIndexLabelKey: "0",
						},
						Annotations: map[string]string{
							leaderworkerset.SizeAnnotationKey: "1",
						},
					},
					Spec: testutils.MakeLeaderPodSpecWithTPUResource(),
				}
			},
			checkExpectedPod: func(expected corev1.Pod, got corev1.Pod) error {
				if !testutils.HasTPUEnvVarsPopulated(got) {
					return fmt.Errorf("should expect TPU env vars for pod %s", got.Name)
				}
				if err := testutils.CheckTPUContainerHasCorrectEnvVars(got, "test-sample-1.default"); err != nil {
					return err
				}
				return nil
			},
		}),
		ginkgo.Entry("Leader pod with exclusive placement enabled have pod affinity/anti-affinity", &testDefaultingCase{
			makePod: func(ns *corev1.Namespace) corev1.Pod {
				return corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-sample-1",
						Namespace: ns.Name,
						Labels: map[string]string{
							leaderworkerset.SetNameLabelKey:     "test-sample",
							leaderworkerset.WorkerIndexLabelKey: "0",
						},
						Annotations: map[string]string{
							leaderworkerset.SizeAnnotationKey:         "4",
							leaderworkerset.ExclusiveKeyAnnotationKey: "cloud.google.com/gke-nodepool",
						},
					},
					Spec: testutils.MakeLeaderPodSpecWithTPUResource(),
				}
			},
			checkExpectedPod: func(expected corev1.Pod, got corev1.Pod) error {
				gomega.Expect(testutils.ValidatePodExclusivePlacementTerms(got)).To(gomega.BeTrue())
				return nil
			},
		}),
		ginkgo.Entry("Leader pod with exclusive placement disabled will not have pod affinity/anti-affinity", &testDefaultingCase{
			makePod: func(ns *corev1.Namespace) corev1.Pod {
				return corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-sample-1",
						Namespace: ns.Name,
						Labels: map[string]string{
							leaderworkerset.SetNameLabelKey:     "test-sample",
							leaderworkerset.WorkerIndexLabelKey: "0",
						},
						Annotations: map[string]string{
							leaderworkerset.SizeAnnotationKey: "4",
						},
					},
					Spec: testutils.MakeLeaderPodSpecWithTPUResource(),
				}
			},
			checkExpectedPod: func(expected corev1.Pod, got corev1.Pod) error {
				gomega.Expect(testutils.ValidatePodExclusivePlacementTerms(got)).To(gomega.BeFalse())
				return nil
			},
		}),
		ginkgo.Entry("Leader pod with exclusive placement enabled will not re-apply exclusive terms", &testDefaultingCase{
			makePod: func(ns *corev1.Namespace) corev1.Pod {
				pod := &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-sample-1",
						Namespace: ns.Name,
						Labels: map[string]string{
							leaderworkerset.SetNameLabelKey:     "test-sample",
							leaderworkerset.WorkerIndexLabelKey: "0",
						},
						Annotations: map[string]string{
							leaderworkerset.SizeAnnotationKey:         "4",
							leaderworkerset.ExclusiveKeyAnnotationKey: "cloud.google.com/gke-nodepool",
						},
					},
					Spec: testutils.MakeLeaderPodSpecWithTPUResource(),
				}
				webhooks.SetExclusiveAffinities(pod, "uniquehash")
				return *pod
			},
			checkExpectedPod: func(expected corev1.Pod, got corev1.Pod) error {
				gomega.Expect(testutils.ValidatePodExclusivePlacementTerms(got)).To(gomega.BeTrue())
				return nil
			},
		}),
		ginkgo.Entry("Leader pod with exclusive placement enabled will not override other affinity terms", &testDefaultingCase{
			makePod: func(ns *corev1.Namespace) corev1.Pod {
				pod := &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-sample-1",
						Namespace: ns.Name,
						Labels: map[string]string{
							leaderworkerset.SetNameLabelKey:     "test-sample",
							leaderworkerset.WorkerIndexLabelKey: "0",
						},
						Annotations: map[string]string{
							leaderworkerset.SizeAnnotationKey:         "4",
							leaderworkerset.ExclusiveKeyAnnotationKey: "cloud.google.com/gke-nodepool",
						},
					},
					Spec: testutils.MakeLeaderPodSpecWithTPUResource(),
				}
				pod.Spec.Affinity = &corev1.Affinity{}
				pod.Spec.Affinity.PodAffinity = &corev1.PodAffinity{}
				pod.Spec.Affinity.PodAntiAffinity = &corev1.PodAntiAffinity{}
				pod.Spec.Affinity.PodAffinity.RequiredDuringSchedulingIgnoredDuringExecution = append(pod.Spec.Affinity.PodAffinity.RequiredDuringSchedulingIgnoredDuringExecution,
					corev1.PodAffinityTerm{
						LabelSelector: &metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{
							{
								Key:      "key",
								Operator: metav1.LabelSelectorOpIn,
								Values:   []string{"value"},
							},
						}},
						TopologyKey: "topology",
					})
				pod.Spec.Affinity.PodAntiAffinity.RequiredDuringSchedulingIgnoredDuringExecution = append(pod.Spec.Affinity.PodAntiAffinity.RequiredDuringSchedulingIgnoredDuringExecution,
					corev1.PodAffinityTerm{
						LabelSelector: &metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{
							{
								Key:      "key",
								Operator: metav1.LabelSelectorOpExists,
							},
							{
								Key:      "key",
								Operator: metav1.LabelSelectorOpNotIn,
								Values:   []string{"value"},
							},
						}},
						TopologyKey: "topology",
					})
				return *pod
			},
			checkExpectedPod: func(expected corev1.Pod, got corev1.Pod) error {
				gomega.Expect(testutils.ValidatePodExclusivePlacementTerms(got)).To(gomega.BeTrue())
				if got.Spec.Affinity.PodAffinity.RequiredDuringSchedulingIgnoredDuringExecution[0].LabelSelector.MatchExpressions[0].Key != "key" && got.Spec.Affinity.PodAntiAffinity.RequiredDuringSchedulingIgnoredDuringExecution[0].LabelSelector.MatchExpressions[0].Key != "key" {
					return fmt.Errorf("existing pod affinity terms are unexpectedly overridden")
				}
				return nil
			},
		}),
		ginkgo.Entry("Pod has leader address env var populated", &testDefaultingCase{
			makePod: func(ns *corev1.Namespace) corev1.Pod {
				return corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-sample-1-1",
						Namespace: ns.Name,
						Labels: map[string]string{
							leaderworkerset.SetNameLabelKey:     "test-sample",
							leaderworkerset.WorkerIndexLabelKey: "1",
							leaderworkerset.GroupIndexLabelKey:  "1",
						},
					},
					Spec: testutils.MakePodSpecWithInitContainer(),
				}
			},
			checkExpectedPod: func(expected corev1.Pod, got corev1.Pod) error {
				if !testutils.HasLWSEnvVarsPopulated(got) {
					return fmt.Errorf("should expect leader address env var for pod %s", got.Name)
				}
				expectedLeaderAddress := fmt.Sprintf("test-sample-1.test-sample.%s", expected.ObjectMeta.Namespace)
				if err := testutils.CheckContainerHasCorrectEnvVar(got, corev1.EnvVar{Name: leaderworkerset.LwsLeaderAddress, Value: expectedLeaderAddress}); err != nil {
					return err
				}
				return nil
			},
		}),
	)

	type testValidationCase struct {
		makePod               func(ns *corev1.Namespace) corev1.Pod
		podCreationShouldFail bool
		updatePod             func(pod *corev1.Pod) corev1.Pod
		podUpdateShouldFail   bool
	}
	ginkgo.DescribeTable("test pod validation for create and update",
		func(tc *testValidationCase) {
			ctx := context.Background()
			// create pod
			ginkgo.By("createing Lws pod")
			pod := tc.makePod(ns)
			// Verify lws created successfully.
			ginkgo.By("checking that pod creation succeeds")
			if tc.podCreationShouldFail {
				gomega.Expect(k8sClient.Create(ctx, &pod)).Should(gomega.Not(gomega.Succeed()))
				return
			}
			gomega.Expect(k8sClient.Create(ctx, &pod)).Should(gomega.Succeed())
			var fetchedPod corev1.Pod
			gomega.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: pod.Name, Namespace: pod.Namespace}, &fetchedPod)).Should(gomega.Succeed())

			if tc.updatePod != nil {
				tc.updatePod(&fetchedPod)
				// Verify leaderworkerset created successfully.
				if tc.podUpdateShouldFail {
					gomega.Expect(k8sClient.Update(ctx, &fetchedPod)).Should(gomega.Not(gomega.Succeed()))
				} else {
					gomega.Expect(k8sClient.Update(ctx, &fetchedPod)).Should(gomega.Succeed())
				}
			}
		},
		ginkgo.Entry("not leaderworkerset pods should have no effect", &testValidationCase{
			makePod: func(ns *corev1.Namespace) corev1.Pod {
				return corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "randompod",
						Namespace: ns.Name,
						Labels: map[string]string{
							"random-label": "random-value",
						},
					},
					Spec: testutils.MakeLeaderPodSpec(),
				}
			},
			podCreationShouldFail: false,
		}),
	)
})

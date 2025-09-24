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
	volcanov1beta1 "volcano.sh/apis/pkg/apis/scheduling/v1beta1"

	leaderworkerset "sigs.k8s.io/lws/api/leaderworkerset/v1"
	"sigs.k8s.io/lws/pkg/schedulerprovider"
	acceleratorutils "sigs.k8s.io/lws/pkg/utils/accelerators"
	"sigs.k8s.io/lws/pkg/webhooks"
	testutils "sigs.k8s.io/lws/test/testutils"
	"sigs.k8s.io/lws/test/wrappers"
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
					Spec: wrappers.MakeLeaderPodSpec(),
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
						Annotations: map[string]string{
							leaderworkerset.SizeAnnotationKey: "2",
						},
					},
					Spec: wrappers.MakeWorkerPodSpec(),
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
						Annotations: map[string]string{
							leaderworkerset.SizeAnnotationKey: "2",
						},
					},
					Spec: wrappers.MakeWorkerPodSpec(),
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
						},
						Annotations: map[string]string{
							leaderworkerset.SizeAnnotationKey:                "5",
							acceleratorutils.LeaderRequestsTPUsAnnotationKey: "true",
							leaderworkerset.SubGroupSizeAnnotationKey:        "4",
						},
					},
					Spec: wrappers.MakeLeaderPodSpecWithTPUResource(),
				}
			},
			checkExpectedPod: func(expected corev1.Pod, got corev1.Pod) error {
				if got.Labels[leaderworkerset.GroupUniqueHashLabelKey] != "" {
					got.Labels[leaderworkerset.GroupUniqueHashLabelKey] = "uniqueHash"
				}
				if got.Labels[leaderworkerset.SubGroupUniqueHashLabelKey] != "" {
					got.Labels[leaderworkerset.SubGroupUniqueHashLabelKey] = "uniqueHash"
				}
				if diff := cmp.Diff(got.Labels, map[string]string{
					leaderworkerset.GroupIndexLabelKey:         "1",
					leaderworkerset.SetNameLabelKey:            "test",
					leaderworkerset.GroupUniqueHashLabelKey:    "uniqueHash",
					leaderworkerset.SubGroupUniqueHashLabelKey: "uniqueHash",
					leaderworkerset.WorkerIndexLabelKey:        "0",
					leaderworkerset.SubGroupIndexLabelKey:      "0",
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
							leaderworkerset.GroupIndexLabelKey:  "1",
						},
						Annotations: map[string]string{
							leaderworkerset.SizeAnnotationKey:                "4",
							acceleratorutils.LeaderRequestsTPUsAnnotationKey: "true",
							leaderworkerset.SubGroupSizeAnnotationKey:        "2",
						},
					},
					Spec: wrappers.MakeWorkerPodSpec(),
				}
			},
			checkExpectedPod: func(expected corev1.Pod, got corev1.Pod) error {
				if got.Labels[leaderworkerset.SubGroupUniqueHashLabelKey] != "" {
					got.Labels[leaderworkerset.SubGroupUniqueHashLabelKey] = "uniqueHash"
				}
				if diff := cmp.Diff(got.Labels, map[string]string{
					leaderworkerset.SetNameLabelKey:            "test",
					leaderworkerset.SubGroupUniqueHashLabelKey: "uniqueHash",
					leaderworkerset.WorkerIndexLabelKey:        "3",
					leaderworkerset.SubGroupIndexLabelKey:      "1",
					leaderworkerset.GroupIndexLabelKey:         "1",
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
							leaderworkerset.GroupIndexLabelKey:  "1",
						},
						Annotations: map[string]string{
							leaderworkerset.SizeAnnotationKey:         "5",
							leaderworkerset.SubGroupSizeAnnotationKey: "2",
						},
					},
					Spec: wrappers.MakeWorkerPodSpecWithTPUResource(),
				}
			},
			checkExpectedPod: func(expected corev1.Pod, got corev1.Pod) error {
				if got.Labels[leaderworkerset.SubGroupUniqueHashLabelKey] != "" {
					got.Labels[leaderworkerset.SubGroupUniqueHashLabelKey] = "uniqueHash"
				}
				if diff := cmp.Diff(got.Labels, map[string]string{
					leaderworkerset.SetNameLabelKey:            "test",
					leaderworkerset.SubGroupUniqueHashLabelKey: "uniqueHash",
					leaderworkerset.WorkerIndexLabelKey:        "4",
					leaderworkerset.SubGroupIndexLabelKey:      "1",
					leaderworkerset.GroupIndexLabelKey:         "1",
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
					Spec: wrappers.MakeWorkerPodSpecWithTPUResource(),
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
						Annotations: map[string]string{
							leaderworkerset.SizeAnnotationKey: "2",
						},
					},
					Spec: wrappers.MakeWorkerPodSpec(),
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
						Annotations: map[string]string{
							leaderworkerset.SizeAnnotationKey: "3",
						},
					},
					Spec: wrappers.MakeWorkerPodSpec(),
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
					Spec: wrappers.MakeLeaderPodSpecWithTPUResource(),
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
		ginkgo.Entry("Leader pod requesting TPUs with subdomainPolicy UniquePerReplica will inject the subdomain", &testDefaultingCase{
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
							leaderworkerset.SizeAnnotationKey:            "5",
							leaderworkerset.SubdomainPolicyAnnotationKey: string(leaderworkerset.SubdomainUniquePerReplica),
						},
					},
					Spec: wrappers.MakeLeaderPodSpecWithTPUResource(),
				}
			},
			checkExpectedPod: func(expected corev1.Pod, got corev1.Pod) error {
				if !testutils.HasTPUEnvVarsPopulated(got) {
					return fmt.Errorf("should expect TPU env vars for pod %s", got.Name)
				}
				if err := testutils.CheckTPUContainerHasCorrectEnvVars(got, "test-sample-1.test-sample-1,test-sample-1-1.test-sample-1,test-sample-1-2.test-sample-1,test-sample-1-3.test-sample-1,test-sample-1-4.test-sample-1"); err != nil {
					return err
				}
				if !testutils.HasLWSEnvVarsPopulated(got) {
					return fmt.Errorf("should expect lws env vars for pod %s", got.Name)
				}
				lwsLeaderAddress := corev1.EnvVar{
					Name:  "LWS_LEADER_ADDRESS",
					Value: fmt.Sprintf("test-sample-1.test-sample-1.%s", expected.ObjectMeta.Namespace),
				}
				if err := testutils.CheckContainerHasCorrectEnvVar(got, lwsLeaderAddress); err != nil {
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
							leaderworkerset.SetNameLabelKey:     "test-sample",
							leaderworkerset.WorkerIndexLabelKey: "0",
						},
						Annotations: map[string]string{
							leaderworkerset.SizeAnnotationKey:         "5",
							leaderworkerset.SubGroupSizeAnnotationKey: "5",
						},
					},
					Spec: wrappers.MakeLeaderPodSpecWithTPUResource(),
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
					Spec: wrappers.MakeWorkerPodSpecWithTPUResource(),
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
							leaderworkerset.SetNameLabelKey:     "test-sample",
							leaderworkerset.WorkerIndexLabelKey: "3",
							leaderworkerset.GroupIndexLabelKey:  "1",
						},
						Annotations: map[string]string{
							leaderworkerset.SizeAnnotationKey:                "10",
							acceleratorutils.LeaderRequestsTPUsAnnotationKey: "true",
							leaderworkerset.SubGroupSizeAnnotationKey:        "5",
						},
					},
					Spec: wrappers.MakeWorkerPodSpecWithTPUResource(),
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
					Spec: wrappers.MakeWorkerPodSpecWithTPUResource(),
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
							leaderworkerset.SetNameLabelKey:     "test-sample",
							leaderworkerset.WorkerIndexLabelKey: "7",
							leaderworkerset.GroupIndexLabelKey:  "1",
						},
						Annotations: map[string]string{
							leaderworkerset.SizeAnnotationKey:         "11",
							leaderworkerset.SubGroupSizeAnnotationKey: "5",
						},
					},
					Spec: wrappers.MakeWorkerPodSpecWithTPUResource(),
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
					Spec: wrappers.MakeWorkerPodSpecWithTPUResource(),
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
					Spec: wrappers.MakeLeaderPodSpecWithTPUResource(),
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
							leaderworkerset.ExclusiveKeyAnnotationKey: "topologyKey",
						},
					},
					Spec: wrappers.MakeLeaderPodSpecWithTPUResource(),
				}
			},
			checkExpectedPod: func(expected corev1.Pod, got corev1.Pod) error {
				gomega.Expect(testutils.ValidatePodExclusivePlacementTerms(got, leaderworkerset.ExclusiveKeyAnnotationKey, leaderworkerset.GroupUniqueHashLabelKey)).To(gomega.BeTrue())
				return nil
			},
		}),
		ginkgo.Entry("Leader pod with exclusive placement and subgroup exclusive placement enabled will have two pod affinity/anti-affinity", &testDefaultingCase{
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
							leaderworkerset.SizeAnnotationKey:                 "4",
							leaderworkerset.ExclusiveKeyAnnotationKey:         "topologyKey",
							leaderworkerset.SubGroupExclusiveKeyAnnotationKey: "topologyKey-2",
							leaderworkerset.SubGroupSizeAnnotationKey:         "2",
						},
					},
					Spec: wrappers.MakeLeaderPodSpecWithTPUResource(),
				}
			},
			checkExpectedPod: func(expected corev1.Pod, got corev1.Pod) error {
				gomega.Expect(testutils.ValidatePodExclusivePlacementTerms(got, leaderworkerset.ExclusiveKeyAnnotationKey, leaderworkerset.GroupUniqueHashLabelKey)).To(gomega.BeTrue())
				gomega.Expect(testutils.ValidatePodExclusivePlacementTerms(got, leaderworkerset.SubGroupExclusiveKeyAnnotationKey, leaderworkerset.SubGroupUniqueHashLabelKey)).To(gomega.BeTrue())
				return nil
			},
		}),
		ginkgo.Entry("Worker pod with exclusive placement and subgroup exclusive placement enabled will only have subgroup pod affinity/anti-affinity", &testDefaultingCase{
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
							leaderworkerset.SizeAnnotationKey:                 "4",
							leaderworkerset.ExclusiveKeyAnnotationKey:         "topologyKey",
							leaderworkerset.SubGroupExclusiveKeyAnnotationKey: "topologyKey-2",
							leaderworkerset.SubGroupSizeAnnotationKey:         "2",
						},
					},
					Spec: wrappers.MakeLeaderPodSpecWithTPUResource(),
				}
			},
			checkExpectedPod: func(expected corev1.Pod, got corev1.Pod) error {
				gomega.Expect(testutils.ValidatePodExclusivePlacementTerms(got, leaderworkerset.ExclusiveKeyAnnotationKey, leaderworkerset.GroupUniqueHashLabelKey)).To(gomega.BeFalse())
				gomega.Expect(testutils.ValidatePodExclusivePlacementTerms(got, leaderworkerset.SubGroupExclusiveKeyAnnotationKey, leaderworkerset.SubGroupUniqueHashLabelKey)).To(gomega.BeTrue())
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
					Spec: wrappers.MakeLeaderPodSpecWithTPUResource(),
				}
			},
			checkExpectedPod: func(expected corev1.Pod, got corev1.Pod) error {
				gomega.Expect(testutils.ValidatePodExclusivePlacementTerms(got, leaderworkerset.ExclusiveKeyAnnotationKey, leaderworkerset.GroupUniqueHashLabelKey)).To(gomega.BeFalse())
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
							leaderworkerset.ExclusiveKeyAnnotationKey: "topologyKey",
						},
					},
					Spec: wrappers.MakeLeaderPodSpecWithTPUResource(),
				}
				webhooks.SetExclusiveAffinities(pod, "uniquehash", "topologyKey", leaderworkerset.GroupUniqueHashLabelKey)
				return *pod
			},
			checkExpectedPod: func(expected corev1.Pod, got corev1.Pod) error {
				gomega.Expect(testutils.ValidatePodExclusivePlacementTerms(got, leaderworkerset.ExclusiveKeyAnnotationKey, leaderworkerset.GroupUniqueHashLabelKey)).To(gomega.BeTrue())
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
							leaderworkerset.ExclusiveKeyAnnotationKey: "topologyKey",
						},
					},
					Spec: wrappers.MakeLeaderPodSpecWithTPUResource(),
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
				gomega.Expect(testutils.ValidatePodExclusivePlacementTerms(got, leaderworkerset.ExclusiveKeyAnnotationKey, leaderworkerset.GroupUniqueHashLabelKey)).To(gomega.BeTrue())
				if got.Spec.Affinity.PodAffinity.RequiredDuringSchedulingIgnoredDuringExecution[0].LabelSelector.MatchExpressions[0].Key != "key" && got.Spec.Affinity.PodAntiAffinity.RequiredDuringSchedulingIgnoredDuringExecution[0].LabelSelector.MatchExpressions[0].Key != "key" {
					return fmt.Errorf("existing pod affinity terms are unexpectedly overridden")
				}
				return nil
			},
		}),
		ginkgo.Entry("Leader env var should be populated and leader address env should be the first env var", &testDefaultingCase{
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
							leaderworkerset.SizeAnnotationKey: "2",
						},
					},
					Spec: wrappers.MakePodSpecWithInitContainer(),
				}
			},
			checkExpectedPod: func(expected corev1.Pod, got corev1.Pod) error {
				if !testutils.HasLWSEnvVarsPopulated(got) {
					return fmt.Errorf("should expect lws env var for pod %s", got.Name)
				}
				expectedLeaderAddress := fmt.Sprintf("test-sample-1.test-sample.%s", expected.ObjectMeta.Namespace)
				if err := testutils.CheckContainerHasCorrectEnvVar(got, corev1.EnvVar{Name: leaderworkerset.LwsLeaderAddress, Value: expectedLeaderAddress}); err != nil {
					return err
				}
				expectedGroupSize := fmt.Sprintf("%d", 2)
				if err := testutils.CheckContainerHasCorrectEnvVar(got, corev1.EnvVar{Name: leaderworkerset.LwsGroupSize, Value: expectedGroupSize}); err != nil {
					return err
				}
				if err := testutils.IsContainerFirstEnvVarLWSLeaderAddress(got); err != nil {
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
			ginkgo.By("creating Lws pod")
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
					Spec: wrappers.MakeLeaderPodSpec(),
				}
			},
			podCreationShouldFail: false,
		}),
	)

	ginkgo.Context("with gang scheduling webhook configuration", ginkgo.Ordered, func() {
		ginkgo.Context("with volcano scheduler provider", ginkgo.Ordered, func() {
			ginkgo.BeforeAll(func() {
				sp, err := schedulerprovider.NewSchedulerProvider("volcano", k8sClient)
				gomega.Expect(err).ShouldNot(gomega.HaveOccurred())
				gomega.Expect(sp).ShouldNot(gomega.BeNil())
				pw.SchedulerProvider = sp
			})

			ginkgo.AfterAll(func() {
				pw.SchedulerProvider = nil
			})

			type gangSchedulingTestcase struct {
				makePod  func(ns *corev1.Namespace) *corev1.Pod
				checkPod func(ctx context.Context, pod *corev1.Pod)
			}

			ginkgo.DescribeTable("gang scheduling webhook tests", func(tc gangSchedulingTestcase) {
				pod := tc.makePod(ns)
				gomega.Expect(k8sClient.Create(ctx, pod)).Should(gomega.Succeed())
				var fetchedPod corev1.Pod
				gomega.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: pod.Name, Namespace: pod.Namespace}, &fetchedPod)).Should(gomega.Succeed())
				tc.checkPod(ctx, &fetchedPod)
			},
				ginkgo.Entry("should add pod group annotation when creating a lws pod", gangSchedulingTestcase{
					makePod: func(ns *corev1.Namespace) *corev1.Pod {
						return &corev1.Pod{
							ObjectMeta: metav1.ObjectMeta{
								Name:      "test-pod-0",
								Namespace: ns.Name,
								Labels: map[string]string{
									leaderworkerset.SetNameLabelKey:    "test",
									leaderworkerset.GroupIndexLabelKey: "0",
									leaderworkerset.RevisionKey:        "1",
								},
								Annotations: map[string]string{
									leaderworkerset.SizeAnnotationKey: "2",
								},
							},
							Spec: wrappers.MakeLeaderPodSpec(),
						}
					},
					checkPod: func(ctx context.Context, pod *corev1.Pod) {
						gomega.Expect(pod.Annotations).To(gomega.HaveKeyWithValue(volcanov1beta1.KubeGroupNameAnnotationKey, "test-0-1"))
					},
				}),
			)
		})
	})
})

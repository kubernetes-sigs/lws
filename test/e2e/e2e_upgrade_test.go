/*
Copyright 2026 The Kubernetes Authors.
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

package e2e

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	testing "sigs.k8s.io/lws/test/testutils"
	"sigs.k8s.io/lws/test/wrappers"
)

var _ = ginkgo.Describe("LeaderWorkerSet controller upgrade", func() {
	var ns *corev1.Namespace

	ginkgo.BeforeEach(func() {
		ns = &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: "test-upgrade-ns-",
			},
		}
		gomega.Expect(k8sClient.Create(ctx, ns)).To(gomega.Succeed())

		gomega.Eventually(func() bool {
			err := k8sClient.Get(ctx, types.NamespacedName{Namespace: ns.Namespace, Name: ns.Name}, ns)
			return err == nil
		}, timeout, interval).Should(gomega.BeTrue())
	})

	ginkgo.AfterEach(func() {
		gomega.Expect(testing.DeleteNamespace(ctx, k8sClient, ns)).To(gomega.Succeed())
	})

	ginkgo.It("should not disrupt workloads during upgrade", func() {
		ginkgo.By("creating a LeaderWorkerSet with the old controller version")
		lws := wrappers.BuildLeaderWorkerSet(ns.Name).Replica(2).Size(2).Obj()
		testing.MustCreateLws(ctx, k8sClient, lws)

		ginkgo.By("waiting for the LeaderWorkerSet to be ready")
		testing.ExpectLeaderWorkerSetAvailable(ctx, k8sClient, lws, "All replicas are ready")

		ginkgo.By("recording existing pod UIDs and restart counts")
		pods := &corev1.PodList{}
		gomega.Expect(k8sClient.List(ctx, pods, client.InNamespace(ns.Name))).To(gomega.Succeed())
		gomega.Expect(len(pods.Items)).To(gomega.Equal(4))

		podUIDs := make(map[string]types.UID)
		podRestartCounts := make(map[string]int32)
		for _, pod := range pods.Items {
			podUIDs[pod.Name] = pod.UID
			var restarts int32
			for _, containerStatus := range pod.Status.ContainerStatuses {
				restarts += containerStatus.RestartCount
			}
			podRestartCounts[pod.Name] = restarts
		}

		ginkgo.By("upgrading the controller to the current build")
		cwd, err := os.Getwd()
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		imageTag := os.Getenv("IMAGE_TAG")
		gomega.Expect(imageTag).NotTo(gomega.BeEmpty(), "IMAGE_TAG env var must be set")

		configManagerDir := filepath.Join(cwd, "..", "..", "config", "manager")
		kustomizeConfigDir := filepath.Join(cwd, "..", "..", "test", "e2e", "config")

		cmdSetImage := exec.Command("kustomize", "edit", "set", "image", fmt.Sprintf("controller=%s", imageTag))
		cmdSetImage.Dir = configManagerDir
		_, err = testing.Run(cmdSetImage)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		cmdApply := exec.Command("sh", "-c", fmt.Sprintf("kustomize build %s | kubectl apply --server-side -f -", kustomizeConfigDir))
		_, err = testing.Run(cmdApply)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		ginkgo.By("waiting for the upgraded controller deployment to be ready")
		verifyControllerUpgraded := func(g gomega.Gomega) {
			deployment := &appsv1.Deployment{}
			err := k8sClient.Get(ctx, types.NamespacedName{Namespace: "lws-system", Name: "lws-controller-manager"}, deployment)
			g.Expect(err).NotTo(gomega.HaveOccurred())

			g.Expect(deployment.Status.AvailableReplicas).To(gomega.Equal(deployment.Status.Replicas))
			g.Expect(deployment.Spec.Template.Spec.Containers[0].Image).To(gomega.Equal(imageTag))
		}
		gomega.Eventually(verifyControllerUpgraded, 5*time.Minute, 5*time.Second).Should(gomega.Succeed())

		ginkgo.By("verifying existing workload pods are not recreated or restarted")
		time.Sleep(10 * time.Second)

		currentPods := &corev1.PodList{}
		gomega.Expect(k8sClient.List(ctx, currentPods, client.InNamespace(ns.Name))).To(gomega.Succeed())
		gomega.Expect(len(currentPods.Items)).To(gomega.Equal(4))

		for _, pod := range currentPods.Items {
			recordedUID, exists := podUIDs[pod.Name]
			gomega.Expect(exists).To(gomega.BeTrue(), fmt.Sprintf("Pod %s was recreated under a new name or was not recorded originally", pod.Name))
			gomega.Expect(pod.UID).To(gomega.Equal(recordedUID), fmt.Sprintf("Pod %s UID changed from %s to %s", pod.Name, recordedUID, pod.UID))

			var currentRestarts int32
			for _, containerStatus := range pod.Status.ContainerStatuses {
				currentRestarts += containerStatus.RestartCount
			}
			recordedRestarts := podRestartCounts[pod.Name]
			gomega.Expect(currentRestarts).To(gomega.Equal(recordedRestarts), fmt.Sprintf("Pod %s container restarted: before %d, after %d", pod.Name, recordedRestarts, currentRestarts))
		}

		ginkgo.By("verifying the upgraded controller remains healthy by creating a new LeaderWorkerSet")
		newLws := wrappers.BuildLeaderWorkerSet(ns.Name).Replica(1).Size(2).Obj()
		newLws.Name = "leaderworkerset-sample-2"
		testing.MustCreateLws(ctx, k8sClient, newLws)

		testing.ExpectLeaderWorkerSetAvailable(ctx, k8sClient, newLws, "New LeaderWorkerSet replica is ready")
	})
})

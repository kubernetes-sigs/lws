/*
Copyright 2024 The Kubernetes Authors.
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
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"sigs.k8s.io/controller-runtime/pkg/client"
	leaderworkerset "sigs.k8s.io/lws/api/leaderworkerset/v1"
	v1 "sigs.k8s.io/lws/api/leaderworkerset/v1"
	"sigs.k8s.io/lws/test/testutils"
	testing "sigs.k8s.io/lws/test/testutils"
	"sigs.k8s.io/lws/test/wrappers"
)

var _ = ginkgo.Describe("leaderWorkerSet e2e tests", func() {

	// Each test runs in a separate namespace.
	var ns *corev1.Namespace
	var lws *leaderworkerset.LeaderWorkerSet

	ginkgo.BeforeEach(func() {
		// Create test namespace before each test.
		ns = &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: "test-ns-",
			},
		}
		gomega.Expect(k8sClient.Create(ctx, ns)).To(gomega.Succeed())

		// Wait for namespace to exist before proceeding with test.
		gomega.Eventually(func() bool {
			err := k8sClient.Get(ctx, types.NamespacedName{Namespace: ns.Namespace, Name: ns.Name}, ns)
			return err == nil
		}, timeout, interval).Should(gomega.BeTrue())
	})

	ginkgo.AfterEach(func() {
		gomega.Expect(testing.DeleteNamespace(ctx, k8sClient, ns)).To(gomega.Succeed())
	})

	ginkgo.It("Can deploy lws with 'replicas', 'size', and 'restart policy' set", func() {
		lws = wrappers.BuildLeaderWorkerSet(ns.Name).Replica(4).Size(5).RestartPolicy(v1.NoneRestartPolicy).Obj()
		testing.MustCreateLws(ctx, k8sClient, lws)
		testing.ExpectLeaderWorkerSetAvailable(ctx, k8sClient, lws, "All replicas are ready")

		statefulSets := &appsv1.StatefulSetList{}
		testing.GetStatefulSets(ctx, lws, k8sClient, statefulSets)

		pods := &corev1.PodList{}
		testing.ExpectValidPods(ctx, k8sClient, lws, pods)

		gomega.Expect(*lws.Spec.Replicas).To(gomega.Equal(int32(4)))
		gomega.Expect(*lws.Spec.LeaderWorkerTemplate.Size).To(gomega.Equal(int32(5)))
		gomega.Expect(lws.Spec.LeaderWorkerTemplate.RestartPolicy).To(gomega.Equal(v1.NoneRestartPolicy))

		expectedLabels := []string{v1.SetNameLabelKey, v1.GroupIndexLabelKey, v1.WorkerIndexLabelKey, v1.RevisionKey}
		expectedAnnotations := []string{v1.LeaderPodNameAnnotationKey, v1.SizeAnnotationKey}

		for _, pod := range pods.Items {
			for _, expectedLabel := range expectedLabels {
				gomega.Expect(pod.Labels[expectedLabel]).To(gomega.Not(gomega.BeNil()))
			}

			for _, expectedAnnotation := range expectedAnnotations {
				gomega.Expect(pod.Labels[expectedAnnotation]).To(gomega.Not(gomega.BeNil()))
			}
		}

		for _, statefulSet := range statefulSets.Items {
			for _, expectedLabel := range expectedLabels {
				gomega.Expect(statefulSet.Labels[expectedLabel]).To(gomega.Not(gomega.BeNil()))
			}
		}
	})

	ginkgo.It("Can create/update a lws with size=1", func() {
		lws = wrappers.BuildLeaderWorkerSet(ns.Name).Replica(4).MaxSurge(1).Size(1).RestartPolicy(v1.RecreateGroupOnPodRestart).Obj()
		testing.MustCreateLws(ctx, k8sClient, lws)

		testing.ExpectValidLeaderStatefulSet(ctx, k8sClient, lws, 4)
		testing.ExpectValidWorkerStatefulSets(ctx, lws, k8sClient, true)
		testing.ExpectLeaderWorkerSetAvailable(ctx, k8sClient, lws, "All replicas are ready")

		testing.UpdateWorkerTemplate(ctx, k8sClient, lws)
		// Happen during rolling update.
		testing.ExpectValidLeaderStatefulSet(ctx, k8sClient, lws, 5)

		testing.ExpectValidLeaderStatefulSet(ctx, k8sClient, lws, 4)
		testing.ExpectValidWorkerStatefulSets(ctx, lws, k8sClient, true)
		testing.ExpectLeaderWorkerSetAvailable(ctx, k8sClient, lws, "All replicas are ready")
	})

	ginkgo.It("Can delete a lws with foreground", func() {
		lws = wrappers.BuildLeaderWorkerSet(ns.Name).Obj()
		testing.MustCreateLws(ctx, k8sClient, lws)

		testing.ExpectLeaderWorkerSetAvailable(ctx, k8sClient, lws, "All replicas are ready")

		// delete lws with foreground
		testing.DeleteLWSWithForground(ctx, k8sClient, lws)

		// Check that the leaderWorkerSet is deleted
		testing.ExpectLeaderWorkerSetNotExist(ctx, lws, k8sClient)
	})

	ginkgo.It("Can perform a rolling update", func() {
		lws := wrappers.BuildLeaderWorkerSet(ns.Name).Obj()
		testing.MustCreateLws(ctx, k8sClient, lws)

		// Wait for leaderWorkerSet to be ready then update it.
		testing.ExpectLeaderWorkerSetAvailable(ctx, k8sClient, lws, "All replicas are ready")
		testing.UpdateWorkerTemplate(ctx, k8sClient, lws)

		testing.ExpectValidLeaderStatefulSet(ctx, k8sClient, lws, 2)
		testing.ExpectValidWorkerStatefulSets(ctx, lws, k8sClient, true)
		testing.ExpectValidPods(ctx, k8sClient, lws, &corev1.PodList{})
		// Wait for leaderWorkerSet to be ready again.
		testing.ExpectLeaderWorkerSetAvailable(ctx, k8sClient, lws, "All replicas are ready")
	})

	ginkgo.It("Can perform a rolling update with maxSurge set", func() {
		lws := wrappers.BuildLeaderWorkerSet(ns.Name).Replica(4).MaxSurge(4).Obj()
		testing.MustCreateLws(ctx, k8sClient, lws)

		// Wait for leaderWorkerSet to be ready then update it.
		testing.ExpectLeaderWorkerSetAvailable(ctx, k8sClient, lws, "All replicas are ready")
		testing.UpdateWorkerTemplate(ctx, k8sClient, lws)

		// Happen during rolling update.
		testing.ExpectValidLeaderStatefulSet(ctx, k8sClient, lws, 7)

		// Rolling update completes.
		testing.ExpectValidLeaderStatefulSet(ctx, k8sClient, lws, 4)
		testing.ExpectValidWorkerStatefulSets(ctx, lws, k8sClient, true)
		testing.ExpectValidPods(ctx, k8sClient, lws, &corev1.PodList{})
		// Wait for leaderWorkerSet to be ready again.
		testing.ExpectLeaderWorkerSetAvailable(ctx, k8sClient, lws, "All replicas are ready")
	})

	ginkgo.It("Can perform a rolling update even if old lws not ready", func() {
		// Create lws with not exist image.
		lws := wrappers.BuildLeaderWorkerSet(ns.Name).LeaderTemplate(nil).Size(1).Replica(2).MaxSurge(1).MaxUnavailable(0).Obj()
		lws.Spec.LeaderWorkerTemplate.WorkerTemplate.Spec.Containers[0].Image = "not-exist-image:v1"
		testing.MustCreateLws(ctx, k8sClient, lws)

		//Update lws.
		testing.UpdateWorkerTemplateImage(ctx, k8sClient, lws)

		// Rolling update completes.
		testing.ExpectValidLeaderStatefulSet(ctx, k8sClient, lws, 2)
		testing.ExpectValidWorkerStatefulSets(ctx, lws, k8sClient, true)
		testing.ExpectValidPods(ctx, k8sClient, lws, &corev1.PodList{})
		// Wait for leaderWorkerSet to be ready again.
		testing.ExpectLeaderWorkerSetAvailable(ctx, k8sClient, lws, "All replicas are ready")
	})

	ginkgo.It("Can deploy lws with subgroupsize set", func() {
		leaderPodSpec := wrappers.MakeLeaderPodSpecWithTPUResource()
		workerPodSpec := wrappers.MakeWorkerPodSpecWithTPUResource()
		lws := wrappers.BuildLeaderWorkerSet(ns.Name).Replica(2).Size(4).SubGroupSize(2).LeaderTemplateSpec(leaderPodSpec).WorkerTemplateSpec(workerPodSpec).Obj()

		testing.MustCreateLws(ctx, k8sClient, lws)

		lwsPods := &corev1.PodList{}
		testing.ExpectValidPods(ctx, k8sClient, lws, lwsPods)

		expectedLabels := []string{v1.SetNameLabelKey, v1.GroupIndexLabelKey, v1.WorkerIndexLabelKey, v1.RevisionKey, v1.SubGroupIndexLabelKey}
		expectedAnnotations := []string{v1.LeaderPodNameAnnotationKey, v1.SizeAnnotationKey, v1.SubGroupSizeAnnotationKey}

		for _, pod := range lwsPods.Items {

			gomega.Expect(testing.HasTPUEnvVarsPopulated(pod)).To(gomega.BeTrue())

			for _, expectedLabel := range expectedLabels {
				gomega.Expect(pod.Labels[expectedLabel]).To(gomega.Not(gomega.BeNil()))
			}

			for _, expectedAnnotation := range expectedAnnotations {
				gomega.Expect(pod.Labels[expectedAnnotation]).To(gomega.Not(gomega.BeNil()))
			}

		}
	})
	ginkgo.It("Can perform a rolling update with subgroupsize and MaxSurge set", func() {
		lws := wrappers.BuildLeaderWorkerSet(ns.Name).Replica(4).MaxSurge(4).SubGroupSize(1).Obj()
		testing.MustCreateLws(ctx, k8sClient, lws)

		// Wait for leaderWorkerSet to be ready then update it.
		testing.ExpectLeaderWorkerSetAvailable(ctx, k8sClient, lws, "All replicas are ready")
		testing.UpdateWorkerTemplate(ctx, k8sClient, lws)

		// Happen during rolling update.
		testing.ExpectValidLeaderStatefulSet(ctx, k8sClient, lws, 7)

		// Rolling update completes.
		testing.ExpectValidLeaderStatefulSet(ctx, k8sClient, lws, 4)
		testing.ExpectValidWorkerStatefulSets(ctx, lws, k8sClient, true)
		testing.ExpectValidPods(ctx, k8sClient, lws, &corev1.PodList{})
		// Wait for leaderWorkerSet to be ready again.
		testing.ExpectLeaderWorkerSetAvailable(ctx, k8sClient, lws, "All replicas are ready")
	})

	ginkgo.It("Adds env vars to containers when using TPU", func() {
		leaderPodSpec := wrappers.MakeLeaderPodSpecWithTPUResource()
		workerPodSpec := wrappers.MakeWorkerPodSpecWithTPUResource()
		lws = wrappers.BuildLeaderWorkerSet(ns.Name).Replica(2).Size(4).LeaderTemplateSpec(leaderPodSpec).WorkerTemplateSpec(workerPodSpec).Obj()

		testing.MustCreateLws(ctx, k8sClient, lws)
		lwsPods := &corev1.PodList{}
		testing.ExpectValidPods(ctx, k8sClient, lws, lwsPods)

		for _, p := range lwsPods.Items {
			gomega.Expect(testing.HasTPUEnvVarsPopulated(p)).To(gomega.BeTrue())
		}
	})

	ginkgo.It("When changing size, recreates the Pods with correct count and size annotation", func() {
		replicas := 2
		size := 2
		lws := wrappers.BuildLeaderWorkerSet(ns.Name).Replica(replicas).Size(size).Obj()
		testing.MustCreateLws(ctx, k8sClient, lws)

		testing.ExpectValidLeaderStatefulSet(ctx, k8sClient, lws, int32(replicas))
		testing.ExpectLeaderWorkerSetAvailable(ctx, k8sClient, lws, "All replicas are ready")

		newSize := 3
		testing.UpdateSize(ctx, k8sClient, lws, int32(newSize))
		testing.ExpectLeaderWorkerSetAvailable(ctx, k8sClient, lws, "All replicas are ready")

		lwsPods := &corev1.PodList{}
		testing.ExpectValidPods(ctx, k8sClient, lws, lwsPods)

		for _, p := range lwsPods.Items {
			gomega.Expect(testing.CheckAnnotation(p, leaderworkerset.SizeAnnotationKey, strconv.Itoa(newSize))).To(gomega.Succeed())
		}
		gomega.Expect(len(lwsPods.Items) == newSize*replicas).To(gomega.BeTrue())
	})

	ginkgo.It("When changing subdomainPolicy, adds correct env vars", func() {
		leaderPodSpec := wrappers.MakeLeaderPodSpecWithTPUResource()
		workerPodSpec := wrappers.MakeWorkerPodSpecWithTPUResource()
		lws := wrappers.BuildLeaderWorkerSet(ns.Name).Replica(1).Size(2).LeaderTemplateSpec(leaderPodSpec).WorkerTemplateSpec(workerPodSpec).Obj()
		testing.MustCreateLws(ctx, k8sClient, lws)
		testing.ExpectValidPods(ctx, k8sClient, lws, &corev1.PodList{})
		testing.UpdateSubdomainPolicy(ctx, k8sClient, lws, leaderworkerset.SubdomainUniquePerReplica)
		lwsPods := &corev1.PodList{}
		testing.ExpectValidPods(ctx, k8sClient, lws, lwsPods)

		for _, pod := range lwsPods.Items {
			gomega.Expect(testing.HasTPUEnvVarsPopulated(pod)).To(gomega.BeTrue())
			gomega.Expect(testing.CheckTPUContainerHasCorrectEnvVars(pod, "test-sample-0.test-sample-0,test-sample-0-1.test-sample-0")).Should(gomega.Succeed())
		}

		testing.UpdateSubdomainPolicy(ctx, k8sClient, lws, leaderworkerset.SubdomainShared)
		lwsPodsAfterUpgrade := &corev1.PodList{}
		testing.ExpectValidPods(ctx, k8sClient, lws, lwsPodsAfterUpgrade)

		for _, pod := range lwsPodsAfterUpgrade.Items {
			gomega.Expect(testing.HasTPUEnvVarsPopulated(pod)).To(gomega.BeTrue())
			gomega.Expect(testing.CheckTPUContainerHasCorrectEnvVars(pod, "test-sample-0.test-sample,test-sample-0-1.test-sample")).Should(gomega.Succeed())
		}
	})

	ginkgo.It("headless services scale up during MaxSurge", func() {
		lws := wrappers.BuildLeaderWorkerSet(ns.Name).Replica(4).MaxSurge(4).SubdomainPolicy(leaderworkerset.SubdomainUniquePerReplica).Obj()
		testing.MustCreateLws(ctx, k8sClient, lws)

		// Happen during rolling update.
		testing.ExpectValidServices(ctx, k8sClient, lws, 4)
		testing.ExpectValidLeaderStatefulSet(ctx, k8sClient, lws, 4)

		testing.UpdateWorkerTemplate(ctx, k8sClient, lws)

		testing.ExpectValidLeaderStatefulSet(ctx, k8sClient, lws, 7)
		testing.ExpectValidServices(ctx, k8sClient, lws, 7)
		// Rolling update completes.
		testing.ExpectValidLeaderStatefulSet(ctx, k8sClient, lws, 4)
		testing.ExpectValidWorkerStatefulSets(ctx, lws, k8sClient, true)
		testing.ExpectValidPods(ctx, k8sClient, lws, &corev1.PodList{})
		// Wait for leaderWorkerSet to be ready again.
		testing.ExpectLeaderWorkerSetAvailable(ctx, k8sClient, lws, "All replicas are ready")
		testing.ExpectValidServices(ctx, k8sClient, lws, 4)
	})

	ginkgo.It("Doesn't add env vars to containers when not using TPU", func() {
		leaderPodSpec := wrappers.MakeLeaderPodSpec()
		workerPodSpec := wrappers.MakeWorkerPodSpec()
		lws = wrappers.BuildLeaderWorkerSet(ns.Name).Replica(2).Size(2).LeaderTemplateSpec(leaderPodSpec).WorkerTemplateSpec(workerPodSpec).Obj()

		testing.MustCreateLws(ctx, k8sClient, lws)
		lwsPods := &corev1.PodList{}
		testing.ExpectValidPods(ctx, k8sClient, lws, lwsPods)

		for _, p := range lwsPods.Items {
			gomega.Expect(testing.HasTPUEnvVarsPopulated(p)).To(gomega.BeFalse())
		}
	})

	ginkgo.It("Pod restart will delete the pod group when restart policy is RecreateGroupOnPodRestart", func() {
		lws = wrappers.BuildLeaderWorkerSet(ns.Name).Replica(1).Size(3).RestartPolicy(v1.RecreateGroupOnPodRestart).Obj()
		testing.MustCreateLws(ctx, k8sClient, lws)
		testing.ExpectLeaderWorkerSetAvailable(ctx, k8sClient, lws, "All replicas are ready")

		initialPods := &corev1.PodList{}
		testing.ExpectValidPods(ctx, k8sClient, lws, initialPods)

		// Get all lws pod UIDs now and compare them to the UIDs after deletion
		// With RecreateGroupOnPodRestart they should all be different
		initialPodUIDs := make(map[types.UID]struct{})
		for _, w := range initialPods.Items {
			initialPodUIDs[w.UID] = struct{}{}
		}

		gomega.Expect(k8sClient.Delete(ctx, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: lws.Namespace, Name: lws.Name + "-0-1"}})).To(gomega.Succeed())

		gomega.Eventually(func() (int, error) {
			finalPods := &corev1.PodList{}
			err := k8sClient.List(ctx, finalPods, client.InNamespace(lws.Namespace))
			if err != nil {
				return -1, err
			}

			// Ensure the initial and final pod lists are of equal length
			if len(finalPods.Items) != len(initialPods.Items) {
				return -1, nil
			}

			numberOfPodsInCommon := 0
			for _, p := range finalPods.Items {
				_, ok := initialPodUIDs[p.UID]
				if ok {
					numberOfPodsInCommon++
				}
			}
			return numberOfPodsInCommon, nil
		}, timeout, interval).Should(gomega.Equal(0))
	})

	// metricsRoleBindingName := "lws-metrics-reader-rolebinding"
	serviceAccountName := "lws-controller-manager"
	metricsServiceName := "lws-controller-manager-metrics-service"
	namespace := "lws-system"
	if ns := os.Getenv("LWS_NAMESPACE"); ns != "" {
		namespace = ns
	}
	var controllerPodName string

	ginkgo.It("should ensure the metrics endpoint is serving metrics", func() {

		ginkgo.By("fetching the controller pod name")
		cmd := exec.Command("kubectl", "get",
			"pods", "-l", "control-plane=controller-manager",
			"-n", namespace,
			"-o", "go-template={{ range .items }}"+
				"{{ if not .metadata.deletionTimestamp }}"+
				"{{ .metadata.name }}"+
				"{{ \"\\n\" }}{{ end }}{{ end }}",
		)
		podOutput, err := testutils.Run(cmd)
		gomega.Expect(err).NotTo(gomega.HaveOccurred(), "Failed to retrieve controller-manager pod information")
		podNames := testutils.GetNonEmptyLines(podOutput)
		controllerPodName = podNames[0]

		ginkgo.By("validating that the metrics service is available")
		cmd = exec.Command("kubectl", "get", "service", metricsServiceName, "-n", namespace)
		_, err = testutils.Run(cmd)
		gomega.Expect(err).NotTo(gomega.HaveOccurred(), "Metrics service should exist")

		ginkgo.By("getting the service account token")
		token, err := serviceAccountToken(serviceAccountName, namespace)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(token).NotTo(gomega.BeEmpty())

		ginkgo.By("waiting for the metrics endpoint to be ready")
		verifyMetricsEndpointReady := func(g gomega.Gomega) {
			cmd := exec.Command("kubectl", "get", "endpoints", metricsServiceName, "-n", namespace)
			g.Expect(err).NotTo(gomega.HaveOccurred())
			output, err := testutils.Run(cmd)
			g.Expect(err).Should(gomega.BeNil())
			g.Expect(output).To(gomega.ContainSubstring("8443"), "Metrics endpoint is not ready")
		}
		gomega.Eventually(verifyMetricsEndpointReady).Should(gomega.Succeed())

		ginkgo.By("verifying that the controller manager is serving the metrics server")
		verifyMetricsServerStarted := func(g gomega.Gomega) {
			cmd := exec.Command("kubectl", "logs", controllerPodName, "-n", namespace)
			output, err := testutils.Run(cmd)
			g.Expect(err).NotTo(gomega.HaveOccurred())
			g.Expect(output).To(gomega.ContainSubstring("controller-runtime.metrics\tServing metrics server"),
				"Metrics server not yet started")
		}
		gomega.Eventually(verifyMetricsServerStarted).Should(gomega.Succeed())

		ginkgo.By("creating the curl-metrics job to access the metrics endpoint")
		cmd = exec.Command("kubectl", "create", "job", "curl-metrics",
			"--namespace", namespace,
			"--image=curlimages/curl:7.78.0",
			"--", "/bin/sh", "-c", fmt.Sprintf(
				"curl -v -k -H 'Authorization: Bearer %s' https://%s.%s.svc.cluster.local:8443/metrics",
				token, metricsServiceName, namespace))
		_, err = testutils.Run(cmd)
		gomega.Expect(err).NotTo(gomega.HaveOccurred(), "Failed to create curl-metrics job")

		ginkgo.By("waiting for the curl-metrics pod to complete.")
		verifyCurlUp := func(g gomega.Gomega) {
			cmd := exec.Command("kubectl", "get", "job", "curl-metrics",
				"-o", "jsonpath={.status.succeeded}",
				"-n", namespace)
			output, err := testutils.Run(cmd)
			g.Expect(err).NotTo(gomega.HaveOccurred())
			g.Expect(output).To(gomega.Equal("1"), "curl pod in wrong status")
		}
		gomega.Eventually(verifyCurlUp, 5*time.Minute).Should(gomega.Succeed())

		ginkgo.By("getting the metrics by checking curl-metrics logs")
		metricsOutput := getMetricsOutput(namespace)
		gomega.Expect(metricsOutput).To(gomega.ContainSubstring(
			"controller_runtime_webhook_requests_total",
		))

		ginkgo.By("cleaning up the curl-metrics job")
		cmd = exec.Command("kubectl", "delete", "job", "curl-metrics", "-n", namespace)
		_, err = testutils.Run(cmd)
		gomega.Expect(err).To(gomega.BeNil())
	})
})

// tokenRequest is a simplified representation of the Kubernetes TokenRequest API response,
// containing only the token field that we need to extract.
type tokenRequest struct {
	Status struct {
		Token string `json:"token"`
	} `json:"status"`
}

// serviceAccountToken returns a token for the specified service account in the given namespace.
// It uses the Kubernetes TokenRequest API to generate a token by directly sending a request
// and parsing the resulting token from the API response.
func serviceAccountToken(serviceAccountName, namespace string) (string, error) {
	const tokenRequestRawString = `{
		"apiVersion": "authentication.k8s.io/v1",
		"kind": "TokenRequest"
	}`

	// Temporary file to store the token request
	secretName := fmt.Sprintf("%s-token-request", serviceAccountName)
	tokenRequestFile := filepath.Join("/tmp", secretName)
	err := os.WriteFile(tokenRequestFile, []byte(tokenRequestRawString), os.FileMode(0o644))
	if err != nil {
		return "", err
	}

	var out string
	verifyTokenCreation := func(g gomega.Gomega) {
		// Execute kubectl command to create the token
		cmd := exec.Command("kubectl", "create", "--raw", fmt.Sprintf(
			"/api/v1/namespaces/%s/serviceaccounts/%s/token",
			namespace,
			serviceAccountName,
		), "-f", tokenRequestFile)

		output, err := cmd.CombinedOutput()
		g.Expect(err).NotTo(gomega.HaveOccurred())

		// Parse the JSON output to extract the token
		var token tokenRequest
		err = json.Unmarshal(output, &token)
		g.Expect(err).NotTo(gomega.HaveOccurred())

		out = token.Status.Token
	}
	gomega.Eventually(verifyTokenCreation).Should(gomega.Succeed())

	return out, err
}

// getMetricsOutput retrieves and returns the logs from the curl pod used to access the metrics endpoint.
func getMetricsOutput(namespace string) string {
	ginkgo.By("getting the curl-metrics logs")
	cmd := exec.Command("kubectl", "logs", "job/curl-metrics", "-n", namespace)
	metricsOutput, err := testutils.Run(cmd)
	gomega.Expect(err).NotTo(gomega.HaveOccurred(), "Failed to retrieve logs from curl-metrics job")
	gomega.Expect(metricsOutput).To(gomega.ContainSubstring("HTTP/1.1 200 OK"))
	return metricsOutput
}

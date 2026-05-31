/*
Copyright 2025 The Kubernetes Authors.

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
	"os/exec"
	"strconv"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/util/intstr"

	utils "sigs.k8s.io/lws/test/testutils/disaggregatedset"
	"sigs.k8s.io/lws/test/testutils/disaggregatedset/fixtures"
	"sigs.k8s.io/lws/test/testutils/disaggregatedset/kubectl"
)

// Operator namespace where the controller is deployed
const namespace = "lws-system"

var controllerPodName string

// applyYAML applies a YAML string using kubectl
func applyYAML(yaml string) error {
	_, err := kubectl.Apply(yaml).Run()
	return err
}

var _ = Describe("DisaggregatedSet E2E Tests", Ordered, func() {
	// Collect debug info on test failure
	AfterEach(func() {
		specReport := CurrentSpecReport()
		if specReport.Failed() {
			By("Fetching controller manager pod logs")
			if controllerPodName != "" {
				cmd := exec.Command("kubectl", "logs", controllerPodName, "-n", namespace)
				controllerLogs, err := utils.Run(cmd)
				if err == nil {
					_, _ = fmt.Fprintf(GinkgoWriter, "Controller logs:\n%s\n", controllerLogs)
				}
			}

			By("Fetching Kubernetes events")
			cmd := exec.Command("kubectl", "get", "events", "-n", "default", "--sort-by=.lastTimestamp")
			eventsOutput, err := utils.Run(cmd)
			if err == nil {
				_, _ = fmt.Fprintf(GinkgoWriter, "Kubernetes events:\n%s\n", eventsOutput)
			}
		}
	})

	SetDefaultEventuallyTimeout(2 * time.Minute)
	SetDefaultEventuallyPollingInterval(time.Second)

	Context("Operator Deployment", func() {
		It("should have the controller-manager running", func() {
			By("checking if controller-manager is already deployed")
			cmd := exec.Command("kubectl", "get", "deployment",
				"lws-controller-manager", "-n", namespace, "-o", "name")
			output, err := utils.Run(cmd)
			if err == nil && strings.Contains(output, "deployment") {
				_, _ = fmt.Fprintf(GinkgoWriter, "Controller-manager already deployed, skipping deployment\n")
			}

			By("validating that the controller-manager pod is running")
			verifyControllerUp := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pods", "-l", "control-plane=controller-manager",
					"-o", "go-template={{ range .items }}"+
						"{{ if not .metadata.deletionTimestamp }}"+
						"{{ .metadata.name }}"+
						"{{ \"\\n\" }}{{ end }}{{ end }}",
					"-n", namespace,
				)

				podOutput, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				podNames := utils.GetNonEmptyLines(podOutput)
				g.Expect(len(podNames)).To(BeNumerically(">=", 1), "expected at least 1 controller pod running")
				controllerPodName = podNames[0]
				g.Expect(controllerPodName).To(ContainSubstring("controller-manager"))

				cmd = exec.Command("kubectl", "get", "pods", controllerPodName,
					"-o", "jsonpath={.status.phase}", "-n", namespace)
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("Running"))
			}
			Eventually(verifyControllerUp).Should(Succeed())

			By("waiting for webhook to be ready")
			// The webhook needs time for certificate provisioning and endpoint registration.
			// We verify readiness by attempting a dry-run create that triggers the webhook.
			verifyWebhookReady := func(g Gomega) {
				yaml := fixtures.PrefillDecode("webhook-test",
					fixtures.Role{Replicas: 1},
					fixtures.Role{Replicas: 1},
				).YAML()
				cmd := exec.Command("kubectl", "apply", "--dry-run=server", "-f", "-")
				cmd.Stdin = strings.NewReader(yaml)
				_, err := utils.Run(cmd)
				// We just need the webhook to respond, not necessarily succeed.
				// Connection errors indicate webhook not ready, validation errors are fine.
				if err != nil {
					errStr := err.Error()
					// These errors mean webhook is not ready yet
					g.Expect(errStr).NotTo(ContainSubstring("connection refused"), "webhook not ready")
					g.Expect(errStr).NotTo(ContainSubstring("no endpoints available"), "webhook not ready")
					g.Expect(errStr).NotTo(ContainSubstring("connection reset"), "webhook not ready")
				}
			}
			Eventually(verifyWebhookReady, 2*time.Minute, 2*time.Second).Should(Succeed())
		})
	})

	Context("Webhook Validation", func() {
		It("should reject DisaggregatedSet with partition set", func() {
			By("attempting to create a DisaggregatedSet with partition=1")
			yaml := fixtures.PrefillDecode("test-invalid-partition",
				fixtures.Role{Replicas: 1, Partition: fixtures.Ptr(1)},
				fixtures.Role{Replicas: 1},
			).YAML()
			err := applyYAML(yaml)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("partition"))
		})
	})

	Context("Basic Deployment", func() {
		const deploymentName = "test-basic"

		AfterEach(func() {
			By("cleaning up the DisaggregatedSet")
			kubectl.CleanupDeployment(deploymentName)
		})

		It("should create LWS resources for prefill and decode roles", func() {
			By("creating a DisaggregatedSet")
			yaml := fixtures.PrefillDecode(deploymentName,
				fixtures.Role{Replicas: 1},
				fixtures.Role{Replicas: 1},
			).YAML()
			Expect(applyYAML(yaml)).To(Succeed())

			By("verifying LWS resources are created for both roles")
			Eventually(func(g Gomega) {
				g.Expect(kubectl.CountLWSByRole(deploymentName, "prefill")).To(Equal(1))
				g.Expect(kubectl.CountLWSByRole(deploymentName, "decode")).To(Equal(1))
			}, 60*time.Second, time.Second).Should(Succeed())

			By("verifying pods become ready")
			Eventually(func(g Gomega) {
				g.Expect(kubectl.CountRunningPods(deploymentName)).To(Equal(2))
			}, 90*time.Second, time.Second).Should(Succeed())
		})
	})

	Context("Rolling Update with Coordinated Drain", func() {
		const deploymentName = "test-rolling"

		AfterEach(func() {
			By("cleaning up the DisaggregatedSet")
			kubectl.CleanupDeployment(deploymentName)
		})

		It("should complete rolling update with both roles scaling together", func() {
			By("creating initial DisaggregatedSet")
			yaml := fixtures.PrefillDecode(deploymentName,
				fixtures.Role{Replicas: 2},
				fixtures.Role{Replicas: 2},
			).YAML()
			Expect(applyYAML(yaml)).To(Succeed())

			By("waiting for initial deployment to stabilize")
			kubectl.ForRunningPodCount(deploymentName, 4)

			By("triggering rolling update by changing image")
			yamlV2 := fixtures.PrefillDecode(deploymentName,
				fixtures.Role{Replicas: 2, Image: "registry.k8s.io/pause:3.10"},
				fixtures.Role{Replicas: 2, Image: "registry.k8s.io/pause:3.10"},
			).YAML()
			Expect(applyYAML(yamlV2)).To(Succeed())

			By("waiting for rolling update to complete")
			kubectl.ForSingleActiveRevision(deploymentName)

			By("verifying no orphaned single-role workloads exist")
			output, err := kubectl.LWS(deploymentName).
				JSONPath(`{range .items[*]}{.metadata.labels.disaggregatedset\.x-k8s\.io/revision},{.metadata.labels.disaggregatedset\.x-k8s\.io/role},{.spec.replicas}{"\n"}{end}`).
				Run()
			Expect(err).NotTo(HaveOccurred())

			// Group by revision and check both roles exist or both are 0
			revisionRoles := make(map[string]map[string]int)
			for _, line := range kubectl.GetNonEmptyLines(output) {
				parts := strings.Split(line, ",")
				if len(parts) == 3 {
					revision, role := parts[0], parts[1]
					var replicas int
					_, _ = fmt.Sscanf(parts[2], "%d", &replicas)
					if revisionRoles[revision] == nil {
						revisionRoles[revision] = make(map[string]int)
					}
					revisionRoles[revision][role] = replicas
				}
			}

			for revision, roles := range revisionRoles {
				prefillReplicas := roles["prefill"]
				decodeReplicas := roles["decode"]
				// If one role has replicas, the other must too (coordinated)
				// Or both must be 0 (drained)
				if prefillReplicas > 0 || decodeReplicas > 0 {
					Expect(prefillReplicas).To(BeNumerically(">", 0),
						fmt.Sprintf("Revision %s has decode replicas but no prefill (orphaned)", revision))
					Expect(decodeReplicas).To(BeNumerically(">", 0),
						fmt.Sprintf("Revision %s has prefill replicas but no decode (orphaned)", revision))
				}
			}
		})

		It("should handle percentage-based maxSurge and maxUnavailable", func() {
			By("creating DisaggregatedSet with percentage rollout config")
			// 4 replicas with 50% surge = 2 surge, 25% unavailable = 1 unavailable
			yaml := fixtures.PrefillDecode(deploymentName,
				fixtures.Role{Replicas: 4, HasRollout: true, MaxSurge: intstr.FromString("50%"), MaxUnavailable: intstr.FromString("25%")},
				fixtures.Role{Replicas: 4, HasRollout: true, MaxSurge: intstr.FromString("50%"), MaxUnavailable: intstr.FromString("25%")},
			).YAML()
			Expect(applyYAML(yaml)).To(Succeed())

			By("waiting for initial deployment to stabilize")
			kubectl.ForRunningPodCount(deploymentName, 8)

			By("triggering rolling update by changing image")
			yamlV2 := fixtures.PrefillDecode(deploymentName,
				fixtures.Role{Replicas: 4, Image: "registry.k8s.io/pause:3.10", HasRollout: true, MaxSurge: intstr.FromString("50%"), MaxUnavailable: intstr.FromString("25%")},
				fixtures.Role{Replicas: 4, Image: "registry.k8s.io/pause:3.10", HasRollout: true, MaxSurge: intstr.FromString("50%"), MaxUnavailable: intstr.FromString("25%")},
			).YAML()
			Expect(applyYAML(yamlV2)).To(Succeed())

			By("waiting for rolling update to complete")
			kubectl.ForSingleActiveRevision(deploymentName)

			By("verifying final state has correct replicas")
			kubectl.ForRunningPodCount(deploymentName, 8)
		})
	})

	Context("Scaling", func() {
		const deploymentName = "test-scaling"

		AfterEach(func() {
			kubectl.CleanupDeployment(deploymentName)
		})

		It("should scale replicas up and down", func() {
			By("creating DisaggregatedSet with 1 replica each")
			yaml := fixtures.PrefillDecode(deploymentName,
				fixtures.Role{Replicas: 1},
				fixtures.Role{Replicas: 1},
			).YAML()
			Expect(applyYAML(yaml)).To(Succeed())

			By("waiting for initial deployment")
			kubectl.ForPodCount(deploymentName, 2)

			By("scaling up to 3 replicas each")
			yamlScaledUp := fixtures.PrefillDecode(deploymentName,
				fixtures.Role{Replicas: 3},
				fixtures.Role{Replicas: 3},
			).YAML()
			Expect(applyYAML(yamlScaledUp)).To(Succeed())

			By("verifying scale up")
			kubectl.ForPodCount(deploymentName, 6)

			By("scaling down to 1 replica each")
			Expect(applyYAML(yaml)).To(Succeed())

			By("verifying scale down")
			kubectl.ForPodCount(deploymentName, 2)
		})
	})

	Context("Service Creation", func() {
		const deploymentName = "test-service"

		AfterEach(func() {
			kubectl.CleanupDeployment(deploymentName)
		})

		It("should create headless portless private services automatically", func() {
			By("creating DisaggregatedSet")
			yaml := fixtures.PrefillDecode(deploymentName,
				fixtures.Role{Replicas: 1},
				fixtures.Role{Replicas: 1},
			).YAML()
			Expect(applyYAML(yaml)).To(Succeed())

			By("waiting for pods to be ready")
			kubectl.ForRunningPodCount(deploymentName, 2)

			By("verifying headless portless services are created for both roles")
			for _, role := range []string{"prefill", "decode"} {
				Eventually(func(g Gomega) {
					// Check service is headless
					output, err := kubectl.ServiceByRole(deploymentName, role).
						JSONPath("{.items[0].spec.clusterIP}").Run()
					g.Expect(err).NotTo(HaveOccurred())
					g.Expect(strings.TrimSpace(output)).To(Equal("None"), "%s service should be headless", role)

					// Check service has no ports
					output, err = kubectl.ServiceByRole(deploymentName, role).
						JSONPath("{.items[0].spec.ports}").Run()
					g.Expect(err).NotTo(HaveOccurred())
					g.Expect(strings.TrimSpace(output)).To(BeEmpty(), "%s service should have no ports", role)
				}, 60*time.Second, time.Second).Should(Succeed())
			}

			By("verifying EndpointSlice is created with Ready pod endpoints")
			for _, role := range []string{"prefill", "decode"} {
				Eventually(func(g Gomega) {
					// Check EndpointSlice exists
					output, err := kubectl.EndpointSliceByRole(deploymentName, role).
						Output("name").Run()
					g.Expect(err).NotTo(HaveOccurred())
					g.Expect(len(kubectl.GetNonEmptyLines(output))).To(BeNumerically(">=", 1),
						"%s EndpointSlice should exist", role)

					// Check has Ready endpoints
					output, err = kubectl.EndpointSliceByRole(deploymentName, role).
						JSONPath("{.items[0].endpoints[*].conditions.ready}").Run()
					g.Expect(err).NotTo(HaveOccurred())
					g.Expect(output).To(ContainSubstring("true"),
						"%s EndpointSlice should have Ready endpoints", role)

					// Check is portless
					output, err = kubectl.EndpointSliceByRole(deploymentName, role).
						JSONPath("{.items[0].ports}").Run()
					g.Expect(err).NotTo(HaveOccurred())
					trimmed := strings.TrimSpace(output)
					g.Expect(trimmed == "" || trimmed == "null").To(BeTrue(),
						"%s EndpointSlice should be portless, got: %q", role, trimmed)
				}, 60*time.Second, time.Second).Should(Succeed())
			}
		})
	})

	Context("Labels and Annotations Propagation", func() {
		const deploymentName = "test-labels"

		AfterEach(func() {
			kubectl.CleanupDeployment(deploymentName)
		})

		It("should propagate custom labels and annotations to LWS", func() {
			By("creating DisaggregatedSet with custom labels and annotations")
			yaml := fixtures.PrefillDecode(deploymentName,
				fixtures.Role{
					Replicas: 1,
					Labels: map[string]string{
						"custom-label": "prefill-value",
						"env":          "production",
					},
					Annotations: map[string]string{
						"custom-annotation":    "prefill-annotation",
						"prometheus.io/scrape": "true",
					},
				},
				fixtures.Role{
					Replicas: 1,
					Labels: map[string]string{
						"custom-label": "decode-value",
					},
					Annotations: map[string]string{
						"custom-annotation": "decode-annotation",
					},
				},
			).YAML()
			Expect(applyYAML(yaml)).To(Succeed())

			By("verifying LWS has merged labels and user annotations")
			Eventually(func(g Gomega) {
				// Check prefill LWS labels
				output, err := kubectl.LWSByRole(deploymentName, "prefill").
					JSONPath("{.items[0].spec.leaderWorkerTemplate.workerTemplate.metadata.labels}").Run()
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(ContainSubstring("custom-label"))
				g.Expect(output).To(ContainSubstring("prefill-value"))
				g.Expect(output).To(ContainSubstring("env"))
				g.Expect(output).To(ContainSubstring("production"))
				g.Expect(output).To(ContainSubstring("disaggregatedset.x-k8s.io/name"))
				g.Expect(output).To(ContainSubstring("disaggregatedset.x-k8s.io/role"))

				// Check prefill LWS annotations
				output, err = kubectl.LWSByRole(deploymentName, "prefill").
					JSONPath("{.items[0].spec.leaderWorkerTemplate.workerTemplate.metadata.annotations}").Run()
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(ContainSubstring("custom-annotation"))
				g.Expect(output).To(ContainSubstring("prefill-annotation"))
				g.Expect(output).To(ContainSubstring("prometheus.io/scrape"))
			}, 60*time.Second, time.Second).Should(Succeed())

			By("verifying Pods have user labels and annotations propagated")
			Eventually(func(g Gomega) {
				// Check prefill pod labels
				output, err := kubectl.PodsByRole(deploymentName, "prefill").
					JSONPath("{.items[0].metadata.labels}").Run()
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(ContainSubstring("custom-label"))
				g.Expect(output).To(ContainSubstring("prefill-value"))
				g.Expect(output).To(ContainSubstring("env"))
				g.Expect(output).To(ContainSubstring("production"))

				// Check prefill pod annotations
				output, err = kubectl.PodsByRole(deploymentName, "prefill").
					JSONPath("{.items[0].metadata.annotations}").Run()
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(ContainSubstring("custom-annotation"))
				g.Expect(output).To(ContainSubstring("prefill-annotation"))
				g.Expect(output).To(ContainSubstring("prometheus.io/scrape"))

				// Check decode pod labels
				output, err = kubectl.PodsByRole(deploymentName, "decode").
					JSONPath("{.items[0].metadata.labels}").Run()
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(ContainSubstring("custom-label"))
				g.Expect(output).To(ContainSubstring("decode-value"))

				// Check decode pod annotations
				output, err = kubectl.PodsByRole(deploymentName, "decode").
					JSONPath("{.items[0].metadata.annotations}").Run()
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(ContainSubstring("custom-annotation"))
				g.Expect(output).To(ContainSubstring("decode-annotation"))
			}, 60*time.Second, time.Second).Should(Succeed())

			By("verifying Services have standard labels only")
			Eventually(func(g Gomega) {
				output, err := kubectl.ServiceByRole(deploymentName, "prefill").
					JSONPath("{.items[0].metadata.labels}").Run()
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(ContainSubstring("disaggregatedset.x-k8s.io/name"))
				g.Expect(output).To(ContainSubstring("disaggregatedset.x-k8s.io/role"))
				g.Expect(output).To(ContainSubstring("disaggregatedset.x-k8s.io/revision"))
			}, 60*time.Second, time.Second).Should(Succeed())
		})

		It("should propagate LWS CR metadata labels for Kueue integration", func() {
			By("creating DisaggregatedSet with LWS CR metadata labels")
			yaml := fixtures.PrefillDecode(deploymentName,
				fixtures.Role{
					Replicas: 1,
					LWSLabels: map[string]string{
						"kueue.x-k8s.io/queue-name":                      "prefill-queue",
						"leaderworkerset.sigs.k8s.io/exclusive-topology": "rack",
					},
					LWSAnnotations: map[string]string{
						"custom-lws-annotation": "prefill-value",
					},
				},
				fixtures.Role{
					Replicas: 1,
					LWSLabels: map[string]string{
						"kueue.x-k8s.io/queue-name": "decode-queue",
					},
				},
			).YAML()
			Expect(applyYAML(yaml)).To(Succeed())

			By("verifying LWS CR has user-provided metadata labels")
			Eventually(func(g Gomega) {
				// Check prefill LWS CR metadata labels
				output, err := kubectl.LWSByRole(deploymentName, "prefill").
					JSONPath("{.items[0].metadata.labels}").Run()
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(ContainSubstring("kueue.x-k8s.io/queue-name"))
				g.Expect(output).To(ContainSubstring("prefill-queue"))
				g.Expect(output).To(ContainSubstring("leaderworkerset.sigs.k8s.io/exclusive-topology"))
				g.Expect(output).To(ContainSubstring("rack"))
				// System labels should also be present
				g.Expect(output).To(ContainSubstring("disaggregatedset.x-k8s.io/name"))
				g.Expect(output).To(ContainSubstring("disaggregatedset.x-k8s.io/role"))

				// Check prefill LWS CR metadata annotations
				output, err = kubectl.LWSByRole(deploymentName, "prefill").
					JSONPath("{.items[0].metadata.annotations}").Run()
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(ContainSubstring("custom-lws-annotation"))
				g.Expect(output).To(ContainSubstring("prefill-value"))

				// Check decode LWS CR metadata labels
				output, err = kubectl.LWSByRole(deploymentName, "decode").
					JSONPath("{.items[0].metadata.labels}").Run()
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(ContainSubstring("kueue.x-k8s.io/queue-name"))
				g.Expect(output).To(ContainSubstring("decode-queue"))
			}, 60*time.Second, time.Second).Should(Succeed())
		})
	})

	Context("Cleanup and Deletion", func() {
		const deploymentName = "test-cleanup"

		It("should garbage collect all child resources when deleted", func() {
			By("creating DisaggregatedSet")
			yaml := fixtures.PrefillDecode(deploymentName,
				fixtures.Role{Replicas: 1},
				fixtures.Role{Replicas: 1},
			).YAML()
			Expect(applyYAML(yaml)).To(Succeed())

			By("waiting for resources to be created")
			Eventually(func(g Gomega) {
				g.Expect(kubectl.CountLWS(deploymentName)).To(Equal(2))
				g.Expect(kubectl.CountRunningPods(deploymentName)).To(Equal(2))
				g.Expect(kubectl.CountService(deploymentName)).To(Equal(2))
			}, 90*time.Second, time.Second).Should(Succeed())

			By("deleting the DisaggregatedSet")
			_, err := kubectl.Delete("disaggregatedset", deploymentName).Namespace("default").Run()
			Expect(err).NotTo(HaveOccurred())

			By("verifying all LWS resources are garbage collected")
			kubectl.ForLWSCount(deploymentName, 0)

			By("verifying all Services are garbage collected")
			Eventually(func(g Gomega) {
				g.Expect(kubectl.CountService(deploymentName)).To(Equal(0))
			}, 30*time.Second, time.Second).Should(Succeed())

			By("verifying all pods are removed")
			kubectl.ForPodCount(deploymentName, 0)
		})
	})

	Context("Rolling Update Step Tracking", func() {
		const deploymentName = "test-rollout-steps"

		// Test cases matching plan-steps output
		testCases := []rolloutTestCase{
			{
				Name:          "scale-down-role0-scale-up-role1",
				SourcePrefill: 10,
				SourceDecode:  2,
				TargetPrefill: 6,
				TargetDecode:  8,
				PrefillSurge:  intstr.FromInt(2),
				DecodeSurge:   intstr.FromInt(2),
				// Expected steps from: go run ./hack/plan-steps --source '{"prefill":10,"decode":2}' --target '{"prefill":6,"decode":8}' --surge '{"prefill":2,"decode":2}'
				ExpectedSteps: []rolloutState{
					{OldPrefill: 10, OldDecode: 2, NewPrefill: 0, NewDecode: 0}, // step 0: initial
					{OldPrefill: 8, OldDecode: 2, NewPrefill: 0, NewDecode: 0},  // step 1: old role0 -2
					{OldPrefill: 6, OldDecode: 2, NewPrefill: 0, NewDecode: 0},  // step 2: old role0 -2
					{OldPrefill: 6, OldDecode: 2, NewPrefill: 2, NewDecode: 2},  // step 3: new role0 +2, new role1 +2
					{OldPrefill: 4, OldDecode: 1, NewPrefill: 2, NewDecode: 2},  // step 4: old role0 -2, old role1 -1
					{OldPrefill: 4, OldDecode: 1, NewPrefill: 3, NewDecode: 4},  // step 5: new role0 +1, new role1 +2
					{OldPrefill: 4, OldDecode: 1, NewPrefill: 4, NewDecode: 5},  // step 6: new role0 +1, new role1 +1
					{OldPrefill: 2, OldDecode: 1, NewPrefill: 4, NewDecode: 5},  // step 7: old role0 -2
					{OldPrefill: 2, OldDecode: 1, NewPrefill: 5, NewDecode: 7},  // step 8: new role0 +1, new role1 +2
					{OldPrefill: 2, OldDecode: 1, NewPrefill: 6, NewDecode: 8},  // step 9: new role0 +1, new role1 +1
					{OldPrefill: 0, OldDecode: 0, NewPrefill: 6, NewDecode: 8},  // step 10: old role0 -2, old role1 -1
				},
			},
			{
				Name:           "surge-0-unavail-2-batches-by-unavailable",
				SourcePrefill:  4,
				SourceDecode:   4,
				TargetPrefill:  4,
				TargetDecode:   4,
				PrefillSurge:   intstr.FromInt(0),
				DecodeSurge:    intstr.FromInt(0),
				PrefillUnavail: intstr.FromInt(2),
				DecodeUnavail:  intstr.FromInt(2),
				// Expected steps from: go run ./hack/plan-steps --source '{"prefill":4,"decode":4}' --target '{"prefill":4,"decode":4}' --surge '{"prefill":0,"decode":0}' --unavailable '{"prefill":2,"decode":2}'
				ExpectedSteps: []rolloutState{
					{OldPrefill: 4, OldDecode: 4, NewPrefill: 0, NewDecode: 0}, // step 0: initial
					{OldPrefill: 2, OldDecode: 2, NewPrefill: 0, NewDecode: 0}, // step 1: drain 2 each
					{OldPrefill: 2, OldDecode: 2, NewPrefill: 2, NewDecode: 2}, // step 2: scale up 2 each
					{OldPrefill: 0, OldDecode: 0, NewPrefill: 2, NewDecode: 2}, // step 3: drain 2 each
					{OldPrefill: 0, OldDecode: 0, NewPrefill: 4, NewDecode: 4}, // step 4: scale up 2 each
				},
			},
		}

		AfterEach(func() {
			_, _ = kubectl.Delete("disaggregatedset", deploymentName).Namespace("default").IgnoreNotFound().Timeout("60s").RunQuiet()
			_, _ = kubectl.Delete("lws").Label("disaggregatedset.x-k8s.io/name", deploymentName).Namespace("default").IgnoreNotFound().Timeout("60s").RunQuiet()
			_, _ = kubectl.Delete("pods").Label("disaggregatedset.x-k8s.io/name", deploymentName).Namespace("default").IgnoreNotFound().GracePeriod(0).Force().RunQuiet()
			kubectl.ForLWSCount(deploymentName, 0)
			kubectl.ForPodCount(deploymentName, 0)
		})

		for _, tc := range testCases {
			tc := tc // capture range variable
			It(fmt.Sprintf("should track rollout steps for %s", tc.Name), func() {
				By("creating initial DisaggregatedSet with source replicas")
				initialYaml := fixtures.PrefillDecode(deploymentName,
					fixtures.Role{Replicas: tc.SourcePrefill, HasRollout: true, MaxSurge: tc.PrefillSurge, MaxUnavailable: tc.PrefillUnavail},
					fixtures.Role{Replicas: tc.SourceDecode, HasRollout: true, MaxSurge: tc.DecodeSurge, MaxUnavailable: tc.DecodeUnavail},
				).YAML()
				Expect(applyYAML(initialYaml)).To(Succeed())

				By("waiting for initial deployment to stabilize")
				expectedInitialPods := tc.SourcePrefill + tc.SourceDecode
				kubectl.ForRunningPodCountWithTimeout(deploymentName, expectedInitialPods, 3*time.Minute)

				// Get the initial revision
				oldRevision := kubectl.GetRevision(deploymentName)
				Expect(oldRevision).NotTo(BeEmpty())
				_, _ = fmt.Fprintf(GinkgoWriter, "Initial revision: %s\n", oldRevision)

				// Capture initial state BEFORE triggering update
				initialState := getCurrentRolloutState(deploymentName, oldRevision)
				_, _ = fmt.Fprintf(GinkgoWriter, "Initial state captured: %s\n", initialState)

				By("triggering rolling update by changing image and target replicas")
				updatedYaml := fixtures.PrefillDecode(deploymentName,
					fixtures.Role{Replicas: tc.TargetPrefill, Image: "registry.k8s.io/pause:3.10", HasRollout: true, MaxSurge: tc.PrefillSurge, MaxUnavailable: tc.PrefillUnavail},
					fixtures.Role{Replicas: tc.TargetDecode, Image: "registry.k8s.io/pause:3.10", HasRollout: true, MaxSurge: tc.DecodeSurge, MaxUnavailable: tc.DecodeUnavail},
				).YAML()
				Expect(applyYAML(updatedYaml)).To(Succeed())

				By("tracking rollout states")
				// Start with the initial state we captured before the update
				observedStates := []rolloutState{initialState}
				lastState := initialState

				// Poll rapidly to capture states
				finalState := tc.ExpectedSteps[len(tc.ExpectedSteps)-1]
				Eventually(func(g Gomega) bool {
					state := getCurrentRolloutState(deploymentName, oldRevision)

					// Record state if it's different from the last one
					if !state.Equals(lastState) {
						observedStates = append(observedStates, state)
						_, _ = fmt.Fprintf(GinkgoWriter, "Observed state %d: %s\n", len(observedStates)-1, state)
						lastState = state
					}

					// Check if we've reached the final state
					return state.Equals(finalState)
				}, 5*time.Minute, 100*time.Millisecond).Should(BeTrue(), "should reach final state")

				By("verifying observed states are valid")
				_, _ = fmt.Fprintf(GinkgoWriter, "\n=== Rollout Summary ===\n")
				_, _ = fmt.Fprintf(GinkgoWriter, "Total observed states: %d\n", len(observedStates))
				_, _ = fmt.Fprintf(GinkgoWriter, "Expected steps: %d\n", len(tc.ExpectedSteps))

				// Build a set of valid states from expected steps
				validStates := make(map[string]bool)
				for _, step := range tc.ExpectedSteps {
					validStates[step.String()] = true
				}

				// Check that all observed states are valid
				for i, observed := range observedStates {
					_, _ = fmt.Fprintf(GinkgoWriter, "State %d: %s", i, observed)
					if validStates[observed.String()] {
						_, _ = fmt.Fprintf(GinkgoWriter, " ✓\n")
					} else {
						_, _ = fmt.Fprintf(GinkgoWriter, " (intermediate)\n")
					}
				}

				// Verify first and last states
				Expect(observedStates[0]).To(Equal(tc.ExpectedSteps[0]), "initial state should match")
				Expect(observedStates[len(observedStates)-1]).To(Equal(finalState), "final state should match")

				// Verify invariants throughout rollout
				By("verifying surge limits were respected")
				maxPrefillSurge := tc.PrefillSurge.IntValue()
				maxDecodeSurge := tc.DecodeSurge.IntValue()
				for _, state := range observedStates {
					totalPrefill := state.OldPrefill + state.NewPrefill
					totalDecode := state.OldDecode + state.NewDecode

					maxAllowedPrefill := max(tc.SourcePrefill, tc.TargetPrefill) + maxPrefillSurge
					maxAllowedDecode := max(tc.SourceDecode, tc.TargetDecode) + maxDecodeSurge

					Expect(totalPrefill).To(BeNumerically("<=", maxAllowedPrefill),
						"prefill surge limit exceeded at state %s", state)
					Expect(totalDecode).To(BeNumerically("<=", maxAllowedDecode),
						"decode surge limit exceeded at state %s", state)
				}
			})
		}
	})

	Context("N-Role Rolling Update (3 roles)", func() {
		const deploymentName = "test-3role-rolling"

		AfterEach(func() {
			kubectl.CleanupDeployment(deploymentName)
		})

		It("should complete rolling update with 3 roles scaling together", func() {
			By("creating initial 3-role DisaggregatedSet")
			initialYaml := fixtures.Config{
				Name: deploymentName,
				Roles: []fixtures.Role{
					{Name: "prefill", Replicas: 2, HasRollout: true, MaxSurge: intstr.FromInt(1)},
					{Name: "decode", Replicas: 2, HasRollout: true, MaxSurge: intstr.FromInt(1)},
					{Name: "encode", Replicas: 2, HasRollout: true, MaxSurge: intstr.FromInt(1)},
				},
			}.YAML()
			Expect(applyYAML(initialYaml)).To(Succeed())

			By("waiting for initial deployment to stabilize")
			kubectl.ForRunningPodCountWithTimeout(deploymentName, 6, 3*time.Minute)

			By("verifying 3 LWS resources exist (one per role)")
			kubectl.ForLWSCount(deploymentName, 3)

			By("triggering rolling update by changing image")
			updatedYaml := fixtures.Config{
				Name: deploymentName,
				Roles: []fixtures.Role{
					{Name: "prefill", Replicas: 2, Image: "registry.k8s.io/pause:3.10", HasRollout: true, MaxSurge: intstr.FromInt(1)},
					{Name: "decode", Replicas: 2, Image: "registry.k8s.io/pause:3.10", HasRollout: true, MaxSurge: intstr.FromInt(1)},
					{Name: "encode", Replicas: 2, Image: "registry.k8s.io/pause:3.10", HasRollout: true, MaxSurge: intstr.FromInt(1)},
				},
			}.YAML()
			Expect(applyYAML(updatedYaml)).To(Succeed())

			By("waiting for rolling update to complete")
			kubectl.ForSingleActiveRevision(deploymentName)

			By("verifying all 3 roles have correct pod count")
			kubectl.ForRunningPodCount(deploymentName, 6)
		})
	})

	Context("N-Role Rename (add + remove role)", func() {
		const deploymentName = "test-role-rename"

		AfterEach(func() {
			kubectl.CleanupDeployment(deploymentName)
		})

		It("should handle role rename (remove old role, add new role) progressively", func() {
			By("creating initial 3-role DisaggregatedSet with roles: prefill, decode, encode")
			initialYaml := fixtures.Config{
				Name: deploymentName,
				Roles: []fixtures.Role{
					{Name: "prefill", Replicas: 2, HasRollout: true, MaxSurge: intstr.FromInt(1)},
					{Name: "decode", Replicas: 2, HasRollout: true, MaxSurge: intstr.FromInt(1)},
					{Name: "encode", Replicas: 2, HasRollout: true, MaxSurge: intstr.FromInt(1)},
				},
			}.YAML()
			Expect(applyYAML(initialYaml)).To(Succeed())

			By("waiting for initial deployment to stabilize")
			kubectl.ForRunningPodCountWithTimeout(deploymentName, 6, 3*time.Minute)

			// Get the initial revision
			oldRevision := kubectl.GetRevision(deploymentName)
			Expect(oldRevision).NotTo(BeEmpty())

			By("applying update that renames 'encode' to 'decode-long-context'")
			// This is effectively a role rename: prefill, decode, encode -> prefill, decode, decode-long-context
			updatedYaml := fixtures.Config{
				Name: deploymentName,
				Roles: []fixtures.Role{
					{Name: "prefill", Replicas: 2, Image: "registry.k8s.io/pause:3.10", HasRollout: true, MaxSurge: intstr.FromInt(1)},
					{Name: "decode", Replicas: 2, Image: "registry.k8s.io/pause:3.10", HasRollout: true, MaxSurge: intstr.FromInt(1)},
					{Name: "decode-long-context", Replicas: 2, Image: "registry.k8s.io/pause:3.10", HasRollout: true, MaxSurge: intstr.FromInt(1)},
				},
			}.YAML()
			Expect(applyYAML(updatedYaml)).To(Succeed())

			By("waiting for rolling update to complete")
			kubectl.ForRevisionDrained(deploymentName, oldRevision)

			By("verifying new roles exist with correct replicas")
			kubectl.ForRoleReplicas(deploymentName, "decode-long-context", 2)

			Eventually(func(g Gomega) {
				// Verify encode role from old revision is scaled to 0
				g.Expect(kubectl.GetTotalReplicas(deploymentName, oldRevision)).To(Equal(0))
			}, 60*time.Second, time.Second).Should(Succeed())

			By("verifying total running pods is correct")
			kubectl.ForRunningPodCount(deploymentName, 6)
		})
	})

	Context("Add role with rolling update ", func() {
		const deploymentName = "test-add-role"

		AfterEach(func() {
			kubectl.CleanupDeployment(deploymentName)
		})

		It("should add new role and roll all roles to new revision", func() {
			By("creating initial 2-role DisaggregatedSet")
			initialYaml := fixtures.Config{
				Name: deploymentName,
				Roles: []fixtures.Role{
					{Name: "alpha", Replicas: 2, HasRollout: true, MaxSurge: intstr.FromInt(1)},
					{Name: "beta", Replicas: 3, HasRollout: true, MaxSurge: intstr.FromInt(1)},
				},
			}.YAML()
			Expect(applyYAML(initialYaml)).To(Succeed())

			By("waiting for initial deployment to stabilize")
			kubectl.ForRunningPodCountWithTimeout(deploymentName, 5, 3*time.Minute)

			By("recording initial revision")
			oldRevision := kubectl.GetRevision(deploymentName)
			Expect(oldRevision).NotTo(BeEmpty())

			By("adding new role 'gamma'")
			updatedYaml := fixtures.Config{
				Name: deploymentName,
				Roles: []fixtures.Role{
					{Name: "alpha", Replicas: 2, HasRollout: true, MaxSurge: intstr.FromInt(1)},
					{Name: "beta", Replicas: 3, HasRollout: true, MaxSurge: intstr.FromInt(1)},
					{Name: "gamma", Replicas: 5, HasRollout: true, MaxSurge: intstr.FromInt(1)},
				},
			}.YAML()
			Expect(applyYAML(updatedYaml)).To(Succeed())

			By("waiting for rolling update to complete - all roles at new revision")
			kubectl.ForRevisionDrained(deploymentName, oldRevision)

			By("verifying gamma role exists with correct replicas")
			kubectl.ForRoleReplicas(deploymentName, "gamma", 5)

			By("verifying total running pods")
			kubectl.ForRunningPodCount(deploymentName, 10)
		})
	})

	Context("Add role with progressive rolling update ", func() {
		const deploymentName = "test-add-role-progressive"

		AfterEach(func() {
			kubectl.CleanupDeployment(deploymentName)
		})

		It("should add new role with progressive rollout (not brutal drain)", func() {
			By("creating initial 2-role DisaggregatedSet and larger replica counts")
			// Use larger replica counts to ensure we can observe intermediate states
			initialYaml := fixtures.Config{
				Name: deploymentName,
				Roles: []fixtures.Role{
					{Name: "prefill", Replicas: 4, HasRollout: true, MaxSurge: intstr.FromInt(1)},
					{Name: "decode", Replicas: 6, HasRollout: true, MaxSurge: intstr.FromInt(1)},
				},
			}.YAML()
			Expect(applyYAML(initialYaml)).To(Succeed())

			By("waiting for initial deployment to stabilize")
			kubectl.ForRunningPodCountWithTimeout(deploymentName, 10, 3*time.Minute)

			By("recording initial revision")
			oldRevision := kubectl.GetRevision(deploymentName)
			Expect(oldRevision).NotTo(BeEmpty())

			By("adding new role 'encode' - this should trigger progressive rollout")
			updatedYaml := fixtures.Config{
				Name: deploymentName,
				Roles: []fixtures.Role{
					{Name: "prefill", Replicas: 4, HasRollout: true, MaxSurge: intstr.FromInt(1)},
					{Name: "decode", Replicas: 6, HasRollout: true, MaxSurge: intstr.FromInt(1)},
					{Name: "encode", Replicas: 2, HasRollout: true, MaxSurge: intstr.FromInt(1)},
				},
			}.YAML()
			Expect(applyYAML(updatedYaml)).To(Succeed())

			By("tracking rollout to verify progressive scaling (not brutal drain)")
			result := kubectl.TrackProgressiveRollout(deploymentName, oldRevision, 10, 12)

			By("verifying progressive rollout occurred (not brutal drain)")
			Expect(result.SawIntermediateOld || result.SawIntermediateNew).To(BeTrue(),
				"Should have observed intermediate states during progressive rollout.")

			By("verifying final state - all roles at new revision")
			kubectl.ForRevisionDrained(deploymentName, oldRevision)

			By("verifying encode role exists with correct replicas")
			kubectl.ForRoleReplicas(deploymentName, "encode", 2)

			By("verifying total running pods")
			kubectl.ForRunningPodCount(deploymentName, 12)
		})
	})

	Context("Remove role with progressive rolling update ", func() {
		const deploymentName = "test-remove-role-progressive"

		AfterEach(func() {
			kubectl.CleanupDeployment(deploymentName)
		})

		It("should remove role with progressive rollout for remaining roles", func() {
			By("creating initial 3-role DisaggregatedSet")
			initialYaml := fixtures.Config{
				Name: deploymentName,
				Roles: []fixtures.Role{
					{Name: "prefill", Replicas: 4, HasRollout: true, MaxSurge: intstr.FromInt(1)},
					{Name: "decode", Replicas: 6, HasRollout: true, MaxSurge: intstr.FromInt(1)},
					{Name: "encode", Replicas: 2, HasRollout: true, MaxSurge: intstr.FromInt(1)},
				},
			}.YAML()
			Expect(applyYAML(initialYaml)).To(Succeed())

			By("waiting for initial deployment to stabilize")
			kubectl.ForRunningPodCountWithTimeout(deploymentName, 12, 3*time.Minute)

			By("recording initial revision")
			oldRevision := kubectl.GetRevision(deploymentName)
			Expect(oldRevision).NotTo(BeEmpty())

			By("removing 'encode' role - this should trigger progressive rollout")
			updatedYaml := fixtures.Config{
				Name: deploymentName,
				Roles: []fixtures.Role{
					{Name: "prefill", Replicas: 4, HasRollout: true, MaxSurge: intstr.FromInt(1)},
					{Name: "decode", Replicas: 6, HasRollout: true, MaxSurge: intstr.FromInt(1)},
				},
			}.YAML()
			Expect(applyYAML(updatedYaml)).To(Succeed())

			By("tracking rollout to verify progressive scaling")
			result := kubectl.TrackProgressiveRollout(deploymentName, oldRevision, 12, 10)

			By("verifying progressive rollout occurred")
			Expect(result.SawIntermediateOld || result.SawIntermediateNew).To(BeTrue(),
				"Should have observed intermediate states during progressive rollout.")

			By("verifying final state - all roles at new revision, encode gone")
			kubectl.ForRevisionDrained(deploymentName, oldRevision)

			By("verifying encode role no longer exists or has 0 replicas")
			Eventually(func(g Gomega) {
				g.Expect(kubectl.GetTotalReplicas(deploymentName, oldRevision)).To(Equal(0))
			}, 30*time.Second, time.Second).Should(Succeed())

			By("verifying total running pods (no encode role)")
			kubectl.ForRunningPodCount(deploymentName, 10)
		})
	})

	Context("Mid-rollout A→B→C (newest-first drain)", func() {
		const deploymentName = "test-abc-drain"

		AfterEach(func() {
			kubectl.CleanupDeployment(deploymentName)
		})

		It("should drain B (broken intermediate) before A (stable original)", func() {
			By("creating initial deployment A (6 replicas per role)")
			yamlA := fixtures.PrefillDecode(deploymentName,
				fixtures.Role{Replicas: 6, HasRollout: true, MaxSurge: intstr.FromInt(1)},
				fixtures.Role{Replicas: 6, HasRollout: true, MaxSurge: intstr.FromInt(1)},
			).YAML()
			Expect(applyYAML(yamlA)).To(Succeed())
			kubectl.ForRunningPodCountWithTimeout(deploymentName, 12, 3*time.Minute)
			revisionA := kubectl.GetRevision(deploymentName)
			Expect(revisionA).NotTo(BeEmpty())
			_, _ = fmt.Fprintf(GinkgoWriter, "\n=== Revision A (stable original): %s ===\n", revisionA)

			By("triggering update B (simulates bad deploy)")
			yamlB := fixtures.PrefillDecode(deploymentName,
				fixtures.Role{Replicas: 6, Image: "registry.k8s.io/pause:3.10", HasRollout: true, MaxSurge: intstr.FromInt(1)},
				fixtures.Role{Replicas: 6, Image: "registry.k8s.io/pause:3.10", HasRollout: true, MaxSurge: intstr.FromInt(1)},
			).YAML()
			Expect(applyYAML(yamlB)).To(Succeed())

			By("waiting for B rollout to start (new LWS created)")
			Eventually(func() int {
				return kubectl.CountLWS(deploymentName)
			}, 30*time.Second, time.Second).Should(BeNumerically(">", 2))

			// Find revision B
			revisionB := ""
			Eventually(func(g Gomega) {
				output, err := kubectl.LWS(deploymentName).
					JSONPath(`{range .items[*]}{.metadata.labels.disaggregatedset\.x-k8s\.io/revision}{"\n"}{end}`).
					RunQuiet()
				g.Expect(err).NotTo(HaveOccurred())
				for _, rev := range kubectl.GetNonEmptyLines(output) {
					if rev != revisionA {
						revisionB = rev
					}
				}
				g.Expect(revisionB).NotTo(BeEmpty())
			}, 30*time.Second, time.Second).Should(Succeed())
			_, _ = fmt.Fprintf(GinkgoWriter, "=== Revision B (bad intermediate): %s ===\n", revisionB)

			By("waiting for B to partially roll out (at least 3 prefill ready)")
			Eventually(func() int {
				output, _ := kubectl.LWSByRevision(deploymentName, revisionB).
					JSONPath(`{range .items[*]}{.metadata.labels.disaggregatedset\.x-k8s\.io/role} {.status.readyReplicas}{"\n"}{end}`).
					RunQuiet()
				for _, line := range kubectl.GetNonEmptyLines(output) {
					parts := strings.Fields(line)
					if len(parts) == 2 && parts[0] == "prefill" {
						n, _ := strconv.Atoi(parts[1])
						return n
					}
				}
				return 0
			}, 3*time.Minute, time.Second).Should(BeNumerically(">=", 3), "B prefill should have at least 3 ready replicas")

			By("triggering update C (the fix) mid-rollout")
			yamlC := fixtures.PrefillDecode(deploymentName,
				fixtures.Role{Replicas: 6, Image: "registry.k8s.io/pause:3.8", HasRollout: true, MaxSurge: intstr.FromInt(1)},
				fixtures.Role{Replicas: 6, Image: "registry.k8s.io/pause:3.8", HasRollout: true, MaxSurge: intstr.FromInt(1)},
			).YAML()
			Expect(applyYAML(yamlC)).To(Succeed())

			// Find revision C
			revisionC := ""
			Eventually(func(g Gomega) {
				output, err := kubectl.LWS(deploymentName).
					JSONPath(`{range .items[*]}{.metadata.labels.disaggregatedset\.x-k8s\.io/revision}{"\n"}{end}`).
					RunQuiet()
				g.Expect(err).NotTo(HaveOccurred())
				for _, rev := range kubectl.GetNonEmptyLines(output) {
					if rev != revisionA && rev != revisionB {
						revisionC = rev
					}
				}
				g.Expect(revisionC).NotTo(BeEmpty())
			}, 30*time.Second, time.Second).Should(Succeed())
			_, _ = fmt.Fprintf(GinkgoWriter, "=== Revision C (the fix / target): %s ===\n\n", revisionC)

			By("tracking drain order: B must drain to 0 before A")
			bDrainedFirst := false
			aDrainedBeforeB := false

			Eventually(func(g Gomega) bool {
				aTotal := kubectl.GetTotalReplicas(deploymentName, revisionA)
				bTotal := kubectl.GetTotalReplicas(deploymentName, revisionB)
				cTotal := kubectl.GetTotalReplicas(deploymentName, revisionC)

				_, _ = fmt.Fprintf(GinkgoWriter, "A(%s)=%d  B(%s)=%d  C(%s)=%d\n",
					revisionA[:8], aTotal, revisionB[:8], bTotal, revisionC[:8], cTotal)

				if bTotal == 0 && aTotal > 0 {
					bDrainedFirst = true
				}
				if aTotal == 0 && bTotal > 0 {
					aDrainedBeforeB = true
				}

				return aTotal == 0 && bTotal == 0
			}, 5*time.Minute, 500*time.Millisecond).Should(BeTrue(), "both A and B should fully drain")

			Expect(bDrainedFirst).To(BeTrue(), "B (newer intermediate) should drain to 0 before A (stable original)")
			Expect(aDrainedBeforeB).To(BeFalse(), "A should NOT drain to 0 while B still has replicas")

			By("verifying final state: only C at target replicas")
			kubectl.ForSingleActiveRevision(deploymentName)
			kubectl.ForRunningPodCount(deploymentName, 12)
		})
	})

	Context("Rename role with rolling update ", func() {
		const deploymentName = "test-rename-role"

		AfterEach(func() {
			kubectl.CleanupDeployment(deploymentName)
		})

		It("should drain old role progressively and roll all roles to new revision", func() {
			By("creating initial 3-role DisaggregatedSet")
			initialYaml := fixtures.Config{
				Name: deploymentName,
				Roles: []fixtures.Role{
					{Name: "alpha", Replicas: 2, HasRollout: true, MaxSurge: intstr.FromInt(1)},
					{Name: "beta", Replicas: 3, HasRollout: true, MaxSurge: intstr.FromInt(1)},
					{Name: "zeta", Replicas: 5, HasRollout: true, MaxSurge: intstr.FromInt(1)},
				},
			}.YAML()
			Expect(applyYAML(initialYaml)).To(Succeed())

			By("waiting for initial deployment to stabilize")
			kubectl.ForRunningPodCountWithTimeout(deploymentName, 10, 3*time.Minute)

			By("recording initial revision")
			oldRevision := kubectl.GetRevision(deploymentName)
			Expect(oldRevision).NotTo(BeEmpty())

			By("renaming 'zeta' to 'gamma' (remove zeta, add gamma)")
			updatedYaml := fixtures.Config{
				Name: deploymentName,
				Roles: []fixtures.Role{
					{Name: "alpha", Replicas: 2, HasRollout: true, MaxSurge: intstr.FromInt(1)},
					{Name: "beta", Replicas: 3, HasRollout: true, MaxSurge: intstr.FromInt(1)},
					{Name: "gamma", Replicas: 5, HasRollout: true, MaxSurge: intstr.FromInt(1)},
				},
			}.YAML()
			Expect(applyYAML(updatedYaml)).To(Succeed())

			By("verifying zeta is progressively drained (removed role)")
			kubectl.ForRoleReplicas(deploymentName, "zeta", 0)

			By("waiting for rolling update to complete")
			kubectl.ForRevisionDrained(deploymentName, oldRevision)

			By("verifying gamma role exists with correct replicas")
			kubectl.ForRoleReplicas(deploymentName, "gamma", 5)

			By("verifying total running pods")
			kubectl.ForRunningPodCount(deploymentName, 10)
		})
	})
})

// rolloutState represents the replica counts at a point in time
type rolloutState struct {
	OldPrefill int
	OldDecode  int
	NewPrefill int
	NewDecode  int
}

func (s rolloutState) String() string {
	return fmt.Sprintf("old(p=%d,d=%d) new(p=%d,d=%d)", s.OldPrefill, s.OldDecode, s.NewPrefill, s.NewDecode)
}

func (s rolloutState) Equals(other rolloutState) bool {
	return s.OldPrefill == other.OldPrefill &&
		s.OldDecode == other.OldDecode &&
		s.NewPrefill == other.NewPrefill &&
		s.NewDecode == other.NewDecode
}

// rolloutTestCase defines a rolling update scenario to test
type rolloutTestCase struct {
	Name           string
	SourcePrefill  int
	SourceDecode   int
	TargetPrefill  int
	TargetDecode   int
	PrefillSurge   intstr.IntOrString
	DecodeSurge    intstr.IntOrString
	PrefillUnavail intstr.IntOrString
	DecodeUnavail  intstr.IntOrString
	ExpectedSteps  []rolloutState
}

// getCurrentRolloutState queries the cluster for current LWS replica counts
func getCurrentRolloutState(deploymentName, oldRevision string) rolloutState {
	output, err := kubectl.LWS(deploymentName).
		JSONPath(`{range .items[*]}{.metadata.labels.disaggregatedset\.x-k8s\.io/revision},{.metadata.labels.disaggregatedset\.x-k8s\.io/role},{.spec.replicas}{"\n"}{end}`).
		RunQuiet()
	if err != nil {
		return rolloutState{}
	}

	state := rolloutState{}
	for _, line := range kubectl.GetNonEmptyLines(output) {
		parts := strings.Split(line, ",")
		if len(parts) != 3 {
			continue
		}
		revision := parts[0]
		role := parts[1]
		replicas, _ := strconv.Atoi(parts[2])

		isOld := revision == oldRevision
		if role == "prefill" {
			if isOld {
				state.OldPrefill = replicas
			} else {
				state.NewPrefill = replicas
			}
		} else if role == "decode" {
			if isOld {
				state.OldDecode = replicas
			} else {
				state.NewDecode = replicas
			}
		}
	}

	return state
}

//go:build e2e
// +build e2e

/*
Copyright 2026.

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

	"sigs.k8s.io/disaggregatedset/test/utils"
	"sigs.k8s.io/disaggregatedset/test/utils/fixtures"
	"sigs.k8s.io/disaggregatedset/test/utils/kubectl"
)

// Operator namespace where the controller is deployed
const namespace = "disaggregatedset-system"

var controllerPodName string

// applyYAML applies a YAML string using kubectl
func applyYAML(yaml string) error {
	_, err := kubectl.Apply(yaml).Run()
	return err
}

var _ = Describe("DisaggregatedSet E2E Tests", Ordered, func() {
	// Deploy the operator before all tests (if not already deployed by hack/e2e-test.sh)
	BeforeAll(func() {
		By("checking if controller-manager is already deployed")
		cmd := exec.Command("kubectl", "get", "deployment", "disaggregatedset-controller-manager",
			"-n", namespace, "-o", "name")
		output, err := utils.Run(cmd)
		if err == nil && strings.Contains(output, "deployment") {
			_, _ = fmt.Fprintf(GinkgoWriter, "Controller-manager already deployed, skipping deployment\n")
			return
		}

		By("creating operator namespace")
		cmd = exec.Command("kubectl", "create", "ns", namespace, "--dry-run=client", "-o", "yaml")
		output, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred())
		cmd = exec.Command("kubectl", "apply", "-f", "-")
		cmd.Stdin = strings.NewReader(output)
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred())

		By("deploying the controller-manager")
		cmd = exec.Command("make", "deploy", fmt.Sprintf("IMG=%s", projectImage))
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to deploy the controller-manager")
	})

	// Note: We don't undeploy in AfterAll because hack/e2e-test.sh handles cleanup
	// This allows tests to be run both standalone and via the hack script

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
				g.Expect(podNames).To(HaveLen(1), "expected 1 controller pod running")
				controllerPodName = podNames[0]
				g.Expect(controllerPodName).To(ContainSubstring("controller-manager"))

				cmd = exec.Command("kubectl", "get", "pods", controllerPodName,
					"-o", "jsonpath={.status.phase}", "-n", namespace)
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("Running"))
			}
			Eventually(verifyControllerUp).Should(Succeed())
		})
	})

	Context("Webhook Validation", func() {
		It("should reject DisaggregatedSet with partition set", func() {
			By("attempting to create a DisaggregatedSet with partition=1")
			yaml := fixtures.PrefillDecode("test-invalid-partition",
				fixtures.Phase{Replicas: 1, Partition: fixtures.Ptr(1)},
				fixtures.Phase{Replicas: 1},
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

		It("should create LWS resources for prefill and decode phases", func() {
			By("creating a DisaggregatedSet")
			yaml := fixtures.PrefillDecode(deploymentName,
				fixtures.Phase{Replicas: 1},
				fixtures.Phase{Replicas: 1},
			).YAML()
			Expect(applyYAML(yaml)).To(Succeed())

			By("verifying LWS resources are created for both phases")
			Eventually(func(g Gomega) {
				g.Expect(kubectl.CountLWSByPhase(deploymentName, "prefill")).To(Equal(1))
				g.Expect(kubectl.CountLWSByPhase(deploymentName, "decode")).To(Equal(1))
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

		It("should complete rolling update with both phases scaling together", func() {
			By("creating initial DisaggregatedSet")
			yaml := fixtures.PrefillDecode(deploymentName,
				fixtures.Phase{Replicas: 2},
				fixtures.Phase{Replicas: 2},
			).YAML()
			Expect(applyYAML(yaml)).To(Succeed())

			By("waiting for initial deployment to stabilize")
			kubectl.ForRunningPodCount(deploymentName, 4)

			By("triggering rolling update by changing image")
			yamlV2 := fixtures.PrefillDecode(deploymentName,
				fixtures.Phase{Replicas: 2, Image: "registry.k8s.io/pause:3.10"},
				fixtures.Phase{Replicas: 2, Image: "registry.k8s.io/pause:3.10"},
			).YAML()
			Expect(applyYAML(yamlV2)).To(Succeed())

			By("waiting for rolling update to complete")
			kubectl.ForSingleActiveRevision(deploymentName)

			By("verifying no orphaned single-phase workloads exist")
			output, err := kubectl.LWS(deploymentName).
				JSONPath(`{range .items[*]}{.metadata.labels.disaggregatedset\.x-k8s\.io/revision},{.metadata.labels.disaggregatedset\.x-k8s\.io/phase},{.spec.replicas}{"\n"}{end}`).
				Run()
			Expect(err).NotTo(HaveOccurred())

			// Group by revision and check both phases exist or both are 0
			revisionPhases := make(map[string]map[string]int)
			for _, line := range kubectl.GetNonEmptyLines(output) {
				parts := strings.Split(line, ",")
				if len(parts) == 3 {
					revision, phase := parts[0], parts[1]
					var replicas int
					_, _ = fmt.Sscanf(parts[2], "%d", &replicas)
					if revisionPhases[revision] == nil {
						revisionPhases[revision] = make(map[string]int)
					}
					revisionPhases[revision][phase] = replicas
				}
			}

			for revision, phases := range revisionPhases {
				prefillReplicas := phases["prefill"]
				decodeReplicas := phases["decode"]
				// If one phase has replicas, the other must too (coordinated)
				// Or both must be 0 (drained)
				if prefillReplicas > 0 || decodeReplicas > 0 {
					Expect(prefillReplicas).To(BeNumerically(">", 0),
						fmt.Sprintf("Revision %s has decode replicas but no prefill (orphaned)", revision))
					Expect(decodeReplicas).To(BeNumerically(">", 0),
						fmt.Sprintf("Revision %s has prefill replicas but no decode (orphaned)", revision))
				}
			}
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
				fixtures.Phase{Replicas: 1},
				fixtures.Phase{Replicas: 1},
			).YAML()
			Expect(applyYAML(yaml)).To(Succeed())

			By("waiting for initial deployment")
			kubectl.ForPodCount(deploymentName, 2)

			By("scaling up to 3 replicas each")
			yamlScaledUp := fixtures.PrefillDecode(deploymentName,
				fixtures.Phase{Replicas: 3},
				fixtures.Phase{Replicas: 3},
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
				fixtures.Phase{Replicas: 1},
				fixtures.Phase{Replicas: 1},
			).YAML()
			Expect(applyYAML(yaml)).To(Succeed())

			By("waiting for pods to be ready")
			kubectl.ForRunningPodCount(deploymentName, 2)

			By("verifying headless portless services are created for both phases")
			for _, phase := range []string{"prefill", "decode"} {
				Eventually(func(g Gomega) {
					// Check service is headless
					output, err := kubectl.ServiceByPhase(deploymentName, phase).
						JSONPath("{.items[0].spec.clusterIP}").Run()
					g.Expect(err).NotTo(HaveOccurred())
					g.Expect(strings.TrimSpace(output)).To(Equal("None"), "%s service should be headless", phase)

					// Check service has no ports
					output, err = kubectl.ServiceByPhase(deploymentName, phase).
						JSONPath("{.items[0].spec.ports}").Run()
					g.Expect(err).NotTo(HaveOccurred())
					g.Expect(strings.TrimSpace(output)).To(BeEmpty(), "%s service should have no ports", phase)
				}, 60*time.Second, time.Second).Should(Succeed())
			}

			By("verifying EndpointSlice is created with Ready pod endpoints")
			for _, phase := range []string{"prefill", "decode"} {
				Eventually(func(g Gomega) {
					// Check EndpointSlice exists
					output, err := kubectl.EndpointSliceByPhase(deploymentName, phase).
						Output("name").Run()
					g.Expect(err).NotTo(HaveOccurred())
					g.Expect(len(kubectl.GetNonEmptyLines(output))).To(BeNumerically(">=", 1),
						"%s EndpointSlice should exist", phase)

					// Check has Ready endpoints
					output, err = kubectl.EndpointSliceByPhase(deploymentName, phase).
						JSONPath("{.items[0].endpoints[*].conditions.ready}").Run()
					g.Expect(err).NotTo(HaveOccurred())
					g.Expect(output).To(ContainSubstring("true"),
						"%s EndpointSlice should have Ready endpoints", phase)

					// Check is portless
					output, err = kubectl.EndpointSliceByPhase(deploymentName, phase).
						JSONPath("{.items[0].ports}").Run()
					g.Expect(err).NotTo(HaveOccurred())
					trimmed := strings.TrimSpace(output)
					g.Expect(trimmed == "" || trimmed == "null").To(BeTrue(),
						"%s EndpointSlice should be portless, got: %q", phase, trimmed)
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
				fixtures.Phase{
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
				fixtures.Phase{
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
				output, err := kubectl.LWSByPhase(deploymentName, "prefill").
					JSONPath("{.items[0].spec.leaderWorkerTemplate.workerTemplate.metadata.labels}").Run()
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(ContainSubstring("custom-label"))
				g.Expect(output).To(ContainSubstring("prefill-value"))
				g.Expect(output).To(ContainSubstring("env"))
				g.Expect(output).To(ContainSubstring("production"))
				g.Expect(output).To(ContainSubstring("disaggregatedset.x-k8s.io/name"))
				g.Expect(output).To(ContainSubstring("disaggregatedset.x-k8s.io/phase"))

				// Check prefill LWS annotations
				output, err = kubectl.LWSByPhase(deploymentName, "prefill").
					JSONPath("{.items[0].spec.leaderWorkerTemplate.workerTemplate.metadata.annotations}").Run()
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(ContainSubstring("custom-annotation"))
				g.Expect(output).To(ContainSubstring("prefill-annotation"))
				g.Expect(output).To(ContainSubstring("prometheus.io/scrape"))
			}, 60*time.Second, time.Second).Should(Succeed())

			By("verifying Pods have user labels and annotations propagated")
			Eventually(func(g Gomega) {
				// Check prefill pod labels
				output, err := kubectl.PodsByPhase(deploymentName, "prefill").
					JSONPath("{.items[0].metadata.labels}").Run()
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(ContainSubstring("custom-label"))
				g.Expect(output).To(ContainSubstring("prefill-value"))
				g.Expect(output).To(ContainSubstring("env"))
				g.Expect(output).To(ContainSubstring("production"))

				// Check prefill pod annotations
				output, err = kubectl.PodsByPhase(deploymentName, "prefill").
					JSONPath("{.items[0].metadata.annotations}").Run()
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(ContainSubstring("custom-annotation"))
				g.Expect(output).To(ContainSubstring("prefill-annotation"))
				g.Expect(output).To(ContainSubstring("prometheus.io/scrape"))

				// Check decode pod labels
				output, err = kubectl.PodsByPhase(deploymentName, "decode").
					JSONPath("{.items[0].metadata.labels}").Run()
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(ContainSubstring("custom-label"))
				g.Expect(output).To(ContainSubstring("decode-value"))

				// Check decode pod annotations
				output, err = kubectl.PodsByPhase(deploymentName, "decode").
					JSONPath("{.items[0].metadata.annotations}").Run()
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(ContainSubstring("custom-annotation"))
				g.Expect(output).To(ContainSubstring("decode-annotation"))
			}, 60*time.Second, time.Second).Should(Succeed())

			By("verifying Services have standard labels only")
			Eventually(func(g Gomega) {
				output, err := kubectl.ServiceByPhase(deploymentName, "prefill").
					JSONPath("{.items[0].metadata.labels}").Run()
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(ContainSubstring("disaggregatedset.x-k8s.io/name"))
				g.Expect(output).To(ContainSubstring("disaggregatedset.x-k8s.io/phase"))
				g.Expect(output).To(ContainSubstring("disaggregatedset.x-k8s.io/revision"))
			}, 60*time.Second, time.Second).Should(Succeed())
		})

		It("should propagate LWS CR metadata labels for Kueue integration", func() {
			By("creating DisaggregatedSet with LWS CR metadata labels")
			yaml := fixtures.PrefillDecode(deploymentName,
				fixtures.Phase{
					Replicas: 1,
					LWSLabels: map[string]string{
						"kueue.x-k8s.io/queue-name":                      "prefill-queue",
						"leaderworkerset.sigs.k8s.io/exclusive-topology": "rack",
					},
					LWSAnnotations: map[string]string{
						"custom-lws-annotation": "prefill-value",
					},
				},
				fixtures.Phase{
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
				output, err := kubectl.LWSByPhase(deploymentName, "prefill").
					JSONPath("{.items[0].metadata.labels}").Run()
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(ContainSubstring("kueue.x-k8s.io/queue-name"))
				g.Expect(output).To(ContainSubstring("prefill-queue"))
				g.Expect(output).To(ContainSubstring("leaderworkerset.sigs.k8s.io/exclusive-topology"))
				g.Expect(output).To(ContainSubstring("rack"))
				// System labels should also be present
				g.Expect(output).To(ContainSubstring("disaggregatedset.x-k8s.io/name"))
				g.Expect(output).To(ContainSubstring("disaggregatedset.x-k8s.io/phase"))

				// Check prefill LWS CR metadata annotations
				output, err = kubectl.LWSByPhase(deploymentName, "prefill").
					JSONPath("{.items[0].metadata.annotations}").Run()
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(ContainSubstring("custom-lws-annotation"))
				g.Expect(output).To(ContainSubstring("prefill-value"))

				// Check decode LWS CR metadata labels
				output, err = kubectl.LWSByPhase(deploymentName, "decode").
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
				fixtures.Phase{Replicas: 1},
				fixtures.Phase{Replicas: 1},
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
				Name:          "scale-down-phase0-scale-up-phase1",
				SourcePrefill: 10,
				SourceDecode:  2,
				TargetPrefill: 6,
				TargetDecode:  8,
				PrefillSurge:  2,
				DecodeSurge:   2,
				// Expected steps from: go run ./cmd/plan-steps --source-phase0 10 --source-phase1 2 --target-phase0 6 --target-phase1 8 --phase0-surge 2 --phase1-surge 2
				ExpectedSteps: []rolloutState{
					{OldPrefill: 10, OldDecode: 2, NewPrefill: 0, NewDecode: 0}, // step 0: initial
					{OldPrefill: 8, OldDecode: 2, NewPrefill: 0, NewDecode: 0},  // step 1: old phase0 -2
					{OldPrefill: 6, OldDecode: 2, NewPrefill: 0, NewDecode: 0},  // step 2: old phase0 -2
					{OldPrefill: 6, OldDecode: 2, NewPrefill: 2, NewDecode: 2},  // step 3: new phase0 +2, new phase1 +2
					{OldPrefill: 4, OldDecode: 1, NewPrefill: 2, NewDecode: 2},  // step 4: old phase0 -2, old phase1 -1
					{OldPrefill: 4, OldDecode: 1, NewPrefill: 3, NewDecode: 4},  // step 5: new phase0 +1, new phase1 +2
					{OldPrefill: 4, OldDecode: 1, NewPrefill: 4, NewDecode: 5},  // step 6: new phase0 +1, new phase1 +1
					{OldPrefill: 2, OldDecode: 1, NewPrefill: 4, NewDecode: 5},  // step 7: old phase0 -2
					{OldPrefill: 2, OldDecode: 1, NewPrefill: 5, NewDecode: 7},  // step 8: new phase0 +1, new phase1 +2
					{OldPrefill: 2, OldDecode: 1, NewPrefill: 6, NewDecode: 8},  // step 9: new phase0 +1, new phase1 +1
					{OldPrefill: 0, OldDecode: 0, NewPrefill: 6, NewDecode: 8},  // step 10: old phase0 -2, old phase1 -1
				},
			},
		}

		AfterEach(func() {
			// Use longer timeout for larger deployments
			_, _ = kubectl.Delete("disaggregatedset", deploymentName).Namespace("default").IgnoreNotFound().Timeout("60s").RunQuiet()
			_, _ = kubectl.Delete("lws").Label("disaggregatedset.x-k8s.io/name", deploymentName).Namespace("default").IgnoreNotFound().Timeout("60s").RunQuiet()
			_, _ = kubectl.Delete("pods").Label("disaggregatedset.x-k8s.io/name", deploymentName).Namespace("default").IgnoreNotFound().GracePeriod(0).Force().RunQuiet()
		})

		for _, tc := range testCases {
			tc := tc // capture range variable
			It(fmt.Sprintf("should track rollout steps for %s", tc.Name), func() {
				By("creating initial DisaggregatedSet with source replicas")
				initialYaml := fixtures.PrefillDecode(deploymentName,
					fixtures.Phase{Replicas: tc.SourcePrefill, HasRollout: true, MaxSurge: tc.PrefillSurge},
					fixtures.Phase{Replicas: tc.SourceDecode, HasRollout: true, MaxSurge: tc.DecodeSurge},
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
					fixtures.Phase{Replicas: tc.TargetPrefill, Image: "registry.k8s.io/pause:3.10", HasRollout: true, MaxSurge: tc.PrefillSurge},
					fixtures.Phase{Replicas: tc.TargetDecode, Image: "registry.k8s.io/pause:3.10", HasRollout: true, MaxSurge: tc.DecodeSurge},
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
				maxPrefillSurge := tc.PrefillSurge
				maxDecodeSurge := tc.DecodeSurge
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

	Context("N-Phase Rolling Update (3 phases)", func() {
		const deploymentName = "test-3phase-rolling"

		AfterEach(func() {
			kubectl.CleanupDeployment(deploymentName)
		})

		It("should complete rolling update with 3 phases scaling together", func() {
			By("creating initial 3-phase DisaggregatedSet")
			initialYaml := fixtures.Config{
				Name: deploymentName,
				Phases: []fixtures.Phase{
					{Name: "prefill", Replicas: 2, HasRollout: true, MaxSurge: 1},
					{Name: "decode", Replicas: 2, HasRollout: true, MaxSurge: 1},
					{Name: "encode", Replicas: 2, HasRollout: true, MaxSurge: 1},
				},
			}.YAML()
			Expect(applyYAML(initialYaml)).To(Succeed())

			By("waiting for initial deployment to stabilize")
			kubectl.ForRunningPodCountWithTimeout(deploymentName, 6, 3*time.Minute)

			By("verifying 3 LWS resources exist (one per phase)")
			kubectl.ForLWSCount(deploymentName, 3)

			By("triggering rolling update by changing image")
			updatedYaml := fixtures.Config{
				Name: deploymentName,
				Phases: []fixtures.Phase{
					{Name: "prefill", Replicas: 2, Image: "registry.k8s.io/pause:3.10", HasRollout: true, MaxSurge: 1},
					{Name: "decode", Replicas: 2, Image: "registry.k8s.io/pause:3.10", HasRollout: true, MaxSurge: 1},
					{Name: "encode", Replicas: 2, Image: "registry.k8s.io/pause:3.10", HasRollout: true, MaxSurge: 1},
				},
			}.YAML()
			Expect(applyYAML(updatedYaml)).To(Succeed())

			By("waiting for rolling update to complete")
			kubectl.ForSingleActiveRevision(deploymentName)

			By("verifying all 3 phases have correct pod count")
			kubectl.ForRunningPodCount(deploymentName, 6)
		})
	})

	Context("N-Phase Rename (add + remove phase)", func() {
		const deploymentName = "test-phase-rename"

		AfterEach(func() {
			kubectl.CleanupDeployment(deploymentName)
		})

		It("should handle phase rename (remove old phase, add new phase) progressively", func() {
			By("creating initial 3-phase DisaggregatedSet with phases: prefill, decode, encode")
			initialYaml := fixtures.Config{
				Name: deploymentName,
				Phases: []fixtures.Phase{
					{Name: "prefill", Replicas: 2, HasRollout: true, MaxSurge: 1},
					{Name: "decode", Replicas: 2, HasRollout: true, MaxSurge: 1},
					{Name: "encode", Replicas: 2, HasRollout: true, MaxSurge: 1},
				},
			}.YAML()
			Expect(applyYAML(initialYaml)).To(Succeed())

			By("waiting for initial deployment to stabilize")
			kubectl.ForRunningPodCountWithTimeout(deploymentName, 6, 3*time.Minute)

			// Get the initial revision
			oldRevision := kubectl.GetRevision(deploymentName)
			Expect(oldRevision).NotTo(BeEmpty())

			By("applying update that renames 'encode' to 'decode-long-context'")
			// This is effectively a phase rename: prefill, decode, encode -> prefill, decode, decode-long-context
			updatedYaml := fixtures.Config{
				Name: deploymentName,
				Phases: []fixtures.Phase{
					{Name: "prefill", Replicas: 2, Image: "registry.k8s.io/pause:3.10", HasRollout: true, MaxSurge: 1},
					{Name: "decode", Replicas: 2, Image: "registry.k8s.io/pause:3.10", HasRollout: true, MaxSurge: 1},
					{Name: "decode-long-context", Replicas: 2, Image: "registry.k8s.io/pause:3.10", HasRollout: true, MaxSurge: 1},
				},
			}.YAML()
			Expect(applyYAML(updatedYaml)).To(Succeed())

			By("waiting for rolling update to complete")
			kubectl.ForRevisionDrained(deploymentName, oldRevision)

			By("verifying new phases exist with correct replicas")
			kubectl.ForPhaseReplicas(deploymentName, "decode-long-context", 2)

			Eventually(func(g Gomega) {
				// Verify encode phase from old revision is scaled to 0
				g.Expect(kubectl.GetTotalReplicas(deploymentName, oldRevision)).To(Equal(0))
			}, 60*time.Second, time.Second).Should(Succeed())

			By("verifying total running pods is correct")
			kubectl.ForRunningPodCount(deploymentName, 6)
		})
	})

	Context("Add phase with rolling update ", func() {
		const deploymentName = "test-add-phase"

		AfterEach(func() {
			kubectl.CleanupDeployment(deploymentName)
		})

		It("should add new phase and roll all phases to new revision", func() {
			By("creating initial 2-phase DisaggregatedSet")
			initialYaml := fixtures.Config{
				Name: deploymentName,
				Phases: []fixtures.Phase{
					{Name: "alpha", Replicas: 2, HasRollout: true, MaxSurge: 1},
					{Name: "beta", Replicas: 3, HasRollout: true, MaxSurge: 1},
				},
			}.YAML()
			Expect(applyYAML(initialYaml)).To(Succeed())

			By("waiting for initial deployment to stabilize")
			kubectl.ForRunningPodCountWithTimeout(deploymentName, 5, 3*time.Minute)

			By("recording initial revision")
			oldRevision := kubectl.GetRevision(deploymentName)
			Expect(oldRevision).NotTo(BeEmpty())

			By("adding new phase 'gamma'")
			updatedYaml := fixtures.Config{
				Name: deploymentName,
				Phases: []fixtures.Phase{
					{Name: "alpha", Replicas: 2, HasRollout: true, MaxSurge: 1},
					{Name: "beta", Replicas: 3, HasRollout: true, MaxSurge: 1},
					{Name: "gamma", Replicas: 5, HasRollout: true, MaxSurge: 1},
				},
			}.YAML()
			Expect(applyYAML(updatedYaml)).To(Succeed())

			By("waiting for rolling update to complete - all phases at new revision")
			kubectl.ForRevisionDrained(deploymentName, oldRevision)

			By("verifying gamma phase exists with correct replicas")
			kubectl.ForPhaseReplicas(deploymentName, "gamma", 5)

			By("verifying total running pods")
			kubectl.ForRunningPodCount(deploymentName, 10)
		})
	})

	Context("Add phase with progressive rolling update ", func() {
		const deploymentName = "test-add-phase-progressive"

		AfterEach(func() {
			kubectl.CleanupDeployment(deploymentName)
		})

		It("should add new phase with progressive rollout (not brutal drain)", func() {
			By("creating initial 2-phase DisaggregatedSet and larger replica counts")
			// Use larger replica counts to ensure we can observe intermediate states
			initialYaml := fixtures.Config{
				Name: deploymentName,
				Phases: []fixtures.Phase{
					{Name: "prefill", Replicas: 4, HasRollout: true, MaxSurge: 1},
					{Name: "decode", Replicas: 6, HasRollout: true, MaxSurge: 1},
				},
			}.YAML()
			Expect(applyYAML(initialYaml)).To(Succeed())

			By("waiting for initial deployment to stabilize")
			kubectl.ForRunningPodCountWithTimeout(deploymentName, 10, 3*time.Minute)

			By("recording initial revision")
			oldRevision := kubectl.GetRevision(deploymentName)
			Expect(oldRevision).NotTo(BeEmpty())

			By("adding new phase 'encode' - this should trigger progressive rollout")
			updatedYaml := fixtures.Config{
				Name: deploymentName,
				Phases: []fixtures.Phase{
					{Name: "prefill", Replicas: 4, HasRollout: true, MaxSurge: 1},
					{Name: "decode", Replicas: 6, HasRollout: true, MaxSurge: 1},
					{Name: "encode", Replicas: 2, HasRollout: true, MaxSurge: 1},
				},
			}.YAML()
			Expect(applyYAML(updatedYaml)).To(Succeed())

			By("tracking rollout to verify progressive scaling (not brutal drain)")
			result := kubectl.TrackProgressiveRollout(deploymentName, oldRevision, 10, 12)

			By("verifying progressive rollout occurred (not brutal drain)")
			Expect(result.SawIntermediateOld || result.SawIntermediateNew).To(BeTrue(),
				"Should have observed intermediate states during progressive rollout.")

			By("verifying final state - all phases at new revision")
			kubectl.ForRevisionDrained(deploymentName, oldRevision)

			By("verifying encode phase exists with correct replicas")
			kubectl.ForPhaseReplicas(deploymentName, "encode", 2)

			By("verifying total running pods")
			kubectl.ForRunningPodCount(deploymentName, 12)
		})
	})

	Context("Remove phase with progressive rolling update ", func() {
		const deploymentName = "test-remove-phase-progressive"

		AfterEach(func() {
			kubectl.CleanupDeployment(deploymentName)
		})

		It("should remove phase with progressive rollout for remaining phases", func() {
			By("creating initial 3-phase DisaggregatedSet")
			initialYaml := fixtures.Config{
				Name: deploymentName,
				Phases: []fixtures.Phase{
					{Name: "prefill", Replicas: 4, HasRollout: true, MaxSurge: 1},
					{Name: "decode", Replicas: 6, HasRollout: true, MaxSurge: 1},
					{Name: "encode", Replicas: 2, HasRollout: true, MaxSurge: 1},
				},
			}.YAML()
			Expect(applyYAML(initialYaml)).To(Succeed())

			By("waiting for initial deployment to stabilize")
			kubectl.ForRunningPodCountWithTimeout(deploymentName, 12, 3*time.Minute)

			By("recording initial revision")
			oldRevision := kubectl.GetRevision(deploymentName)
			Expect(oldRevision).NotTo(BeEmpty())

			By("removing 'encode' phase - this should trigger progressive rollout")
			updatedYaml := fixtures.Config{
				Name: deploymentName,
				Phases: []fixtures.Phase{
					{Name: "prefill", Replicas: 4, HasRollout: true, MaxSurge: 1},
					{Name: "decode", Replicas: 6, HasRollout: true, MaxSurge: 1},
				},
			}.YAML()
			Expect(applyYAML(updatedYaml)).To(Succeed())

			By("tracking rollout to verify progressive scaling")
			result := kubectl.TrackProgressiveRollout(deploymentName, oldRevision, 12, 10)

			By("verifying progressive rollout occurred")
			Expect(result.SawIntermediateOld || result.SawIntermediateNew).To(BeTrue(),
				"Should have observed intermediate states during progressive rollout.")

			By("verifying final state - all phases at new revision, encode gone")
			kubectl.ForRevisionDrained(deploymentName, oldRevision)

			By("verifying encode phase no longer exists or has 0 replicas")
			Eventually(func(g Gomega) {
				g.Expect(kubectl.GetTotalReplicas(deploymentName, oldRevision)).To(Equal(0))
			}, 30*time.Second, time.Second).Should(Succeed())

			By("verifying total running pods (no encode phase)")
			kubectl.ForRunningPodCount(deploymentName, 10)
		})
	})

	Context("Rename phase with rolling update ", func() {
		const deploymentName = "test-rename-phase"

		AfterEach(func() {
			kubectl.CleanupDeployment(deploymentName)
		})

		It("should drain old phase progressively and roll all phases to new revision", func() {
			By("creating initial 3-phase DisaggregatedSet")
			initialYaml := fixtures.Config{
				Name: deploymentName,
				Phases: []fixtures.Phase{
					{Name: "alpha", Replicas: 2, HasRollout: true, MaxSurge: 1},
					{Name: "beta", Replicas: 3, HasRollout: true, MaxSurge: 1},
					{Name: "zeta", Replicas: 5, HasRollout: true, MaxSurge: 1},
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
				Phases: []fixtures.Phase{
					{Name: "alpha", Replicas: 2, HasRollout: true, MaxSurge: 1},
					{Name: "beta", Replicas: 3, HasRollout: true, MaxSurge: 1},
					{Name: "gamma", Replicas: 5, HasRollout: true, MaxSurge: 1},
				},
			}.YAML()
			Expect(applyYAML(updatedYaml)).To(Succeed())

			By("verifying zeta is progressively drained (removed phase)")
			kubectl.ForPhaseReplicas(deploymentName, "zeta", 0)

			By("waiting for rolling update to complete")
			kubectl.ForRevisionDrained(deploymentName, oldRevision)

			By("verifying gamma phase exists with correct replicas")
			kubectl.ForPhaseReplicas(deploymentName, "gamma", 5)

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
	Name          string
	SourcePrefill int
	SourceDecode  int
	TargetPrefill int
	TargetDecode  int
	PrefillSurge  int
	DecodeSurge   int
	ExpectedSteps []rolloutState
}

// getCurrentRolloutState queries the cluster for current LWS replica counts
func getCurrentRolloutState(deploymentName, oldRevision string) rolloutState {
	output, err := kubectl.LWS(deploymentName).
		JSONPath(`{range .items[*]}{.metadata.labels.disaggregatedset\.x-k8s\.io/revision},{.metadata.labels.disaggregatedset\.x-k8s\.io/phase},{.spec.replicas}{"\n"}{end}`).
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
		phase := parts[1]
		replicas, _ := strconv.Atoi(parts[2])

		isOld := revision == oldRevision
		if phase == "prefill" {
			if isOld {
				state.OldPrefill = replicas
			} else {
				state.NewPrefill = replicas
			}
		} else if phase == "decode" {
			if isOld {
				state.OldDecode = replicas
			} else {
				state.NewDecode = replicas
			}
		}
	}

	return state
}

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
)

// Operator namespace where the controller is deployed
const namespace = "disaggregatedset-system"

var controllerPodName string

// disaggregatedSetConfig holds configuration for generating DisaggregatedSet YAML
type disaggregatedSetConfig struct {
	Name           string
	Namespace      string
	PrefillConfig  phaseConfig
	DecodeConfig   phaseConfig
}

// phaseConfig holds configuration for a single phase (prefill or decode)
type phaseConfig struct {
	Replicas       int
	Image          string
	MaxSurge       int  // 0 means not set
	MaxUnavailable int  // 0 means not set (but 0 is also valid)
	HasRollout     bool // whether to include rollout strategy
}

// buildDisaggregatedSetYAML generates a DisaggregatedSet YAML from config
func buildDisaggregatedSetYAML(cfg disaggregatedSetConfig) string {
	if cfg.Namespace == "" {
		cfg.Namespace = "default"
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf(`apiVersion: disaggregatedset.x-k8s.io/v1alpha1
kind: DisaggregatedSet
metadata:
  name: %s
  namespace: %s
spec:
  phases:
`, cfg.Name, cfg.Namespace))

	// Prefill phase
	sb.WriteString(buildPhaseYAML("prefill", cfg.PrefillConfig))

	// Decode phase
	sb.WriteString(buildPhaseYAML("decode", cfg.DecodeConfig))

	return sb.String()
}

// buildPhaseYAML generates YAML for a single phase configuration as an array element
func buildPhaseYAML(phaseName string, cfg phaseConfig) string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("  - name: %s\n", phaseName))
	sb.WriteString(fmt.Sprintf("    replicas: %d\n", cfg.Replicas))

	// Rollout strategy
	if cfg.HasRollout {
		sb.WriteString("    rolloutStrategy:\n")
		sb.WriteString(fmt.Sprintf("      maxSurge: %d\n", cfg.MaxSurge))
		sb.WriteString(fmt.Sprintf("      maxUnavailable: %d\n", cfg.MaxUnavailable))
	}

	// Leader worker template
	image := cfg.Image
	if image == "" {
		image = "registry.k8s.io/pause:3.9"
	}
	sb.WriteString("    leaderWorkerTemplate:\n")
	sb.WriteString("      size: 1\n")
	sb.WriteString("      workerTemplate:\n")
	sb.WriteString("        spec:\n")
	sb.WriteString("          containers:\n")
	sb.WriteString("          - name: main\n")
	sb.WriteString(fmt.Sprintf("            image: %s\n", image))

	return sb.String()
}

// nPhaseDisaggregatedSetConfig holds configuration for N-phase DisaggregatedSet YAML
type nPhaseDisaggregatedSetConfig struct {
	Name        string
	Namespace   string
	PhasePolicy string // "Strict" or "Flexible", empty means default (Strict)
	Phases      []nPhaseSpec
}

// nPhaseSpec holds configuration for a single phase in N-phase config
type nPhaseSpec struct {
	Name           string
	Replicas       int
	Image          string
	MaxSurge       int
	MaxUnavailable int
	HasRollout     bool
}

// buildNPhaseDisaggregatedSetYAML generates a DisaggregatedSet YAML with N phases
func buildNPhaseDisaggregatedSetYAML(cfg nPhaseDisaggregatedSetConfig) string {
	if cfg.Namespace == "" {
		cfg.Namespace = "default"
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf(`apiVersion: disaggregatedset.x-k8s.io/v1alpha1
kind: DisaggregatedSet
metadata:
  name: %s
  namespace: %s
spec:
`, cfg.Name, cfg.Namespace))

	// Add phasePolicy if specified
	if cfg.PhasePolicy != "" {
		sb.WriteString(fmt.Sprintf("  phasePolicy: %s\n", cfg.PhasePolicy))
	}

	sb.WriteString("  phases:\n")

	for _, phase := range cfg.Phases {
		sb.WriteString(fmt.Sprintf("  - name: %s\n", phase.Name))
		sb.WriteString(fmt.Sprintf("    replicas: %d\n", phase.Replicas))

		if phase.HasRollout {
			sb.WriteString("    rolloutStrategy:\n")
			sb.WriteString(fmt.Sprintf("      maxSurge: %d\n", phase.MaxSurge))
			sb.WriteString(fmt.Sprintf("      maxUnavailable: %d\n", phase.MaxUnavailable))
		}

		image := phase.Image
		if image == "" {
			image = "registry.k8s.io/pause:3.9"
		}
		sb.WriteString("    leaderWorkerTemplate:\n")
		sb.WriteString("      size: 1\n")
		sb.WriteString("      workerTemplate:\n")
		sb.WriteString("        spec:\n")
		sb.WriteString("          containers:\n")
		sb.WriteString("          - name: main\n")
		sb.WriteString(fmt.Sprintf("            image: %s\n", image))
	}

	return sb.String()
}

// cleanupDeployment removes a DisaggregatedSet and all related resources
func cleanupDeployment(deploymentName string) {
	cmd := exec.Command("kubectl", "delete", "disaggregatedset", deploymentName,
		"-n", "default", "--ignore-not-found", "--timeout=30s")
	_, _ = utils.Run(cmd)

	cmd = exec.Command("kubectl", "delete", "lws", "-l",
		fmt.Sprintf("disaggregatedset.x-k8s.io/name=%s", deploymentName),
		"-n", "default", "--ignore-not-found", "--timeout=30s")
	_, _ = utils.Run(cmd)

	cmd = exec.Command("kubectl", "delete", "pods", "-l",
		fmt.Sprintf("disaggregatedset.x-k8s.io/name=%s", deploymentName),
		"-n", "default", "--ignore-not-found", "--grace-period=0", "--force")
	_, _ = utils.Run(cmd)

	cmd = exec.Command("kubectl", "delete", "svc", "-l",
		fmt.Sprintf("disaggregatedset.x-k8s.io/name=%s", deploymentName),
		"-n", "default", "--ignore-not-found")
	_, _ = utils.Run(cmd)
}

// applyYAML applies a YAML string using kubectl
func applyYAML(yaml string) error {
	cmd := exec.Command("kubectl", "apply", "-f", "-")
	cmd.Stdin = strings.NewReader(yaml)
	_, err := utils.Run(cmd)
	return err
}

// countPods returns the number of pods matching the deployment label
func countPods(deploymentName string) int {
	cmd := exec.Command("kubectl", "get", "pods", "-l",
		fmt.Sprintf("disaggregatedset.x-k8s.io/name=%s", deploymentName),
		"-n", "default", "--no-headers")
	output, err := utils.Run(cmd)
	if err != nil {
		return 0
	}
	return len(utils.GetNonEmptyLines(output))
}

// countRunningPods returns the number of running pods matching the deployment label
func countRunningPods(deploymentName string) int {
	cmd := exec.Command("kubectl", "get", "pods", "-l",
		fmt.Sprintf("disaggregatedset.x-k8s.io/name=%s", deploymentName),
		"-n", "default", "--no-headers", "--field-selector=status.phase=Running")
	output, err := utils.Run(cmd)
	if err != nil {
		return 0
	}
	return len(utils.GetNonEmptyLines(output))
}

var _ = Describe("DisaggregatedSet E2E Tests", Ordered, func() {
	// Deploy the operator before all tests
	BeforeAll(func() {
		By("creating operator namespace")
		cmd := exec.Command("kubectl", "create", "ns", namespace, "--dry-run=client", "-o", "yaml")
		output, err := utils.Run(cmd)
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

	// Undeploy the operator after all tests
	AfterAll(func() {
		By("undeploying the controller-manager")
		cmd := exec.Command("make", "undeploy")
		_, _ = utils.Run(cmd)
	})

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

	Context("Basic Deployment", func() {
		const deploymentName = "test-basic"

		AfterEach(func() {
			By("cleaning up the DisaggregatedSet")
			cleanupDeployment(deploymentName)
		})

		It("should create LWS resources for prefill and decode phases", func() {
			By("creating a DisaggregatedSet")
			yaml := buildDisaggregatedSetYAML(disaggregatedSetConfig{
				Name:          deploymentName,
				PrefillConfig: phaseConfig{Replicas: 1},
				DecodeConfig:  phaseConfig{Replicas: 1},
			})
			Expect(applyYAML(yaml)).To(Succeed())

			By("verifying LWS resources are created for both phases")
			Eventually(func(g Gomega) {
				// Check prefill LWS
				cmd := exec.Command("kubectl", "get", "lws", "-l",
					fmt.Sprintf("disaggregatedset.x-k8s.io/name=%s,disaggregatedset.x-k8s.io/phase=prefill", deploymentName),
					"-n", "default", "-o", "name")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(utils.GetNonEmptyLines(output)).To(HaveLen(1))

				// Check decode LWS
				cmd = exec.Command("kubectl", "get", "lws", "-l",
					fmt.Sprintf("disaggregatedset.x-k8s.io/name=%s,disaggregatedset.x-k8s.io/phase=decode", deploymentName),
					"-n", "default", "-o", "name")
				output, err = utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(utils.GetNonEmptyLines(output)).To(HaveLen(1))
			}, 60*time.Second, time.Second).Should(Succeed())

			By("verifying pods become ready")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pods", "-l",
					fmt.Sprintf("disaggregatedset.x-k8s.io/name=%s", deploymentName),
					"-n", "default", "-o", "jsonpath={.items[*].status.phase}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				phases := utils.GetNonEmptyLines(output)
				// Should have 2 pods (1 prefill + 1 decode), all Running
				g.Expect(output).To(ContainSubstring("Running"))
				g.Expect(len(phases)).To(BeNumerically(">=", 1))
			}, 90*time.Second, time.Second).Should(Succeed())
		})
	})

	Context("Rolling Update with Coordinated Drain", func() {
		const deploymentName = "test-rolling"

		AfterEach(func() {
			By("cleaning up the DisaggregatedSet")
			cleanupDeployment(deploymentName)
		})

		It("should complete rolling update with both phases scaling together", func() {
			By("creating initial DisaggregatedSet")
			yaml := buildDisaggregatedSetYAML(disaggregatedSetConfig{
				Name:          deploymentName,
				PrefillConfig: phaseConfig{Replicas: 2},
				DecodeConfig:  phaseConfig{Replicas: 2},
			})
			Expect(applyYAML(yaml)).To(Succeed())

			By("waiting for initial deployment to stabilize")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pods", "-l",
					fmt.Sprintf("disaggregatedset.x-k8s.io/name=%s", deploymentName),
					"-n", "default", "--no-headers")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				lines := utils.GetNonEmptyLines(output)
				// Should have 4 pods (2 prefill + 2 decode)
				g.Expect(len(lines)).To(Equal(4))
				// All should be ready (contain "1/1")
				for _, line := range lines {
					g.Expect(line).To(ContainSubstring("Running"))
				}
			}, 2*time.Minute, time.Second).Should(Succeed())

			By("triggering rolling update by changing image")
			yamlV2 := buildDisaggregatedSetYAML(disaggregatedSetConfig{
				Name:          deploymentName,
				PrefillConfig: phaseConfig{Replicas: 2, Image: "registry.k8s.io/pause:3.10"},
				DecodeConfig:  phaseConfig{Replicas: 2, Image: "registry.k8s.io/pause:3.10"},
			})
			Expect(applyYAML(yamlV2)).To(Succeed())

			By("waiting for rolling update to complete")
			Eventually(func(g Gomega) {
				// Get all LWS for this deployment
				cmd := exec.Command("kubectl", "get", "lws", "-l",
					fmt.Sprintf("disaggregatedset.x-k8s.io/name=%s", deploymentName),
					"-n", "default", "-o", "jsonpath={range .items[*]}{.metadata.labels.disaggregatedset\\.x-k8s\\.io/revision} {.spec.replicas}\\n{end}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())

				// Parse revision -> total replicas
				revisionReplicas := make(map[string]int)
				for _, line := range utils.GetNonEmptyLines(output) {
					var revision string
					var replicas int
					_, _ = fmt.Sscanf(line, "%s %d", &revision, &replicas)
					if revision != "" {
						revisionReplicas[revision] += replicas
					}
				}

				// Should only have one revision with replicas (old ones scaled to 0)
				activeRevisions := 0
				for _, replicas := range revisionReplicas {
					if replicas > 0 {
						activeRevisions++
					}
				}
				g.Expect(activeRevisions).To(Equal(1), "Expected only one active revision after rolling update")
			}, 3*time.Minute, 2*time.Second).Should(Succeed())

			By("verifying no orphaned single-phase workloads exist")
			cmd := exec.Command("kubectl", "get", "lws", "-l",
				fmt.Sprintf("disaggregatedset.x-k8s.io/name=%s", deploymentName),
				"-n", "default", "-o", "jsonpath={range .items[*]}{.metadata.labels.disaggregatedset\\.x-k8s\\.io/revision},{.metadata.labels.disaggregatedset\\.x-k8s\\.io/phase},{.spec.replicas}\\n{end}")
			output, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			// Group by revision and check both phases exist or both are 0
			revisionPhases := make(map[string]map[string]int)
			for _, line := range utils.GetNonEmptyLines(output) {
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

			// Note: Service and EndpointSlice verification is done in the dedicated
			// "Service Creation" test. During rolling updates, services are managed
			// by the controller and will be verified there.
		})
	})

	Context("Scaling", func() {
		const deploymentName = "test-scaling"

		AfterEach(func() {
			cleanupDeployment(deploymentName)
		})

		It("should scale replicas up and down", func() {
			By("creating DisaggregatedSet with 1 replica each")
			yaml := buildDisaggregatedSetYAML(disaggregatedSetConfig{
				Name:          deploymentName,
				PrefillConfig: phaseConfig{Replicas: 1},
				DecodeConfig:  phaseConfig{Replicas: 1},
			})
			Expect(applyYAML(yaml)).To(Succeed())

			By("waiting for initial deployment")
			Eventually(func(g Gomega) {
				g.Expect(countPods(deploymentName)).To(Equal(2))
			}, 90*time.Second, time.Second).Should(Succeed())

			By("scaling up to 3 replicas each")
			yamlScaledUp := buildDisaggregatedSetYAML(disaggregatedSetConfig{
				Name:          deploymentName,
				PrefillConfig: phaseConfig{Replicas: 3},
				DecodeConfig:  phaseConfig{Replicas: 3},
			})
			Expect(applyYAML(yamlScaledUp)).To(Succeed())

			By("verifying scale up")
			Eventually(func(g Gomega) {
				g.Expect(countPods(deploymentName)).To(Equal(6))
			}, 2*time.Minute, time.Second).Should(Succeed())

			By("scaling down to 1 replica each")
			Expect(applyYAML(yaml)).To(Succeed()) // Original yaml with 1 replica

			By("verifying scale down")
			Eventually(func(g Gomega) {
				g.Expect(countPods(deploymentName)).To(Equal(2))
			}, 2*time.Minute, time.Second).Should(Succeed())
		})
	})

	Context("Service Creation", func() {
		const deploymentName = "test-service"

		AfterEach(func() {
			cleanupDeployment(deploymentName)
		})

		It("should create headless portless private services automatically", func() {
			By("creating DisaggregatedSet")
			yaml := buildDisaggregatedSetYAML(disaggregatedSetConfig{
				Name:          deploymentName,
				PrefillConfig: phaseConfig{Replicas: 1},
				DecodeConfig:  phaseConfig{Replicas: 1},
			})
			Expect(applyYAML(yaml)).To(Succeed())

			By("waiting for pods to be ready")
			Eventually(func(g Gomega) {
				g.Expect(countRunningPods(deploymentName)).To(Equal(2))
			}, 90*time.Second, time.Second).Should(Succeed())

			By("verifying headless portless services are created for both phases")
			Eventually(func(g Gomega) {
				// Check prefill service exists and is headless
				cmd := exec.Command("kubectl", "get", "svc", "-l",
					fmt.Sprintf("disaggregatedset.x-k8s.io/name=%s,disaggregatedset.x-k8s.io/phase=prefill", deploymentName),
					"-n", "default", "-o", "jsonpath={.items[0].spec.clusterIP}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(strings.TrimSpace(output)).To(Equal("None"), "prefill service should be headless")

				// Check prefill service has no ports (portless)
				cmd = exec.Command("kubectl", "get", "svc", "-l",
					fmt.Sprintf("disaggregatedset.x-k8s.io/name=%s,disaggregatedset.x-k8s.io/phase=prefill", deploymentName),
					"-n", "default", "-o", "jsonpath={.items[0].spec.ports}")
				output, err = utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(strings.TrimSpace(output)).To(BeEmpty(), "prefill service should have no ports")

				// Check decode service exists and is headless
				cmd = exec.Command("kubectl", "get", "svc", "-l",
					fmt.Sprintf("disaggregatedset.x-k8s.io/name=%s,disaggregatedset.x-k8s.io/phase=decode", deploymentName),
					"-n", "default", "-o", "jsonpath={.items[0].spec.clusterIP}")
				output, err = utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(strings.TrimSpace(output)).To(Equal("None"), "decode service should be headless")

				// Check decode service has no ports (portless)
				cmd = exec.Command("kubectl", "get", "svc", "-l",
					fmt.Sprintf("disaggregatedset.x-k8s.io/name=%s,disaggregatedset.x-k8s.io/phase=decode", deploymentName),
					"-n", "default", "-o", "jsonpath={.items[0].spec.ports}")
				output, err = utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(strings.TrimSpace(output)).To(BeEmpty(), "decode service should have no ports")
			}, 60*time.Second, time.Second).Should(Succeed())

			By("verifying EndpointSlice is created with Ready pod endpoints")
			Eventually(func(g Gomega) {
				// Check prefill EndpointSlice exists
				cmd := exec.Command("kubectl", "get", "endpointslice", "-l",
					fmt.Sprintf("disaggregatedset.x-k8s.io/name=%s,disaggregatedset.x-k8s.io/phase=prefill", deploymentName),
					"-n", "default", "-o", "name")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(len(utils.GetNonEmptyLines(output))).To(BeNumerically(">=", 1), "prefill EndpointSlice should exist")

				// Check prefill EndpointSlice has Ready endpoints
				cmd = exec.Command("kubectl", "get", "endpointslice", "-l",
					fmt.Sprintf("disaggregatedset.x-k8s.io/name=%s,disaggregatedset.x-k8s.io/phase=prefill", deploymentName),
					"-n", "default", "-o", "jsonpath={.items[0].endpoints[*].conditions.ready}")
				output, err = utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(ContainSubstring("true"), "prefill EndpointSlice should have Ready endpoints")

				// Check prefill EndpointSlice is portless (ports should be null or empty)
				cmd = exec.Command("kubectl", "get", "endpointslice", "-l",
					fmt.Sprintf("disaggregatedset.x-k8s.io/name=%s,disaggregatedset.x-k8s.io/phase=prefill", deploymentName),
					"-n", "default", "-o", "jsonpath={.items[0].ports}")
				output, err = utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				// Portless services have ports: null (jsonpath returns "null") or ports: [] (jsonpath returns "")
				trimmedOutput := strings.TrimSpace(output)
				g.Expect(trimmedOutput == "" || trimmedOutput == "null").To(BeTrue(), "prefill EndpointSlice should be portless, got: %q", trimmedOutput)

				// Check decode EndpointSlice exists
				cmd = exec.Command("kubectl", "get", "endpointslice", "-l",
					fmt.Sprintf("disaggregatedset.x-k8s.io/name=%s,disaggregatedset.x-k8s.io/phase=decode", deploymentName),
					"-n", "default", "-o", "name")
				output, err = utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(len(utils.GetNonEmptyLines(output))).To(BeNumerically(">=", 1), "decode EndpointSlice should exist")

				// Check decode EndpointSlice has Ready endpoints
				cmd = exec.Command("kubectl", "get", "endpointslice", "-l",
					fmt.Sprintf("disaggregatedset.x-k8s.io/name=%s,disaggregatedset.x-k8s.io/phase=decode", deploymentName),
					"-n", "default", "-o", "jsonpath={.items[0].endpoints[*].conditions.ready}")
				output, err = utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(ContainSubstring("true"), "decode EndpointSlice should have Ready endpoints")

				// Check decode EndpointSlice is portless (ports should be null or empty)
				cmd = exec.Command("kubectl", "get", "endpointslice", "-l",
					fmt.Sprintf("disaggregatedset.x-k8s.io/name=%s,disaggregatedset.x-k8s.io/phase=decode", deploymentName),
					"-n", "default", "-o", "jsonpath={.items[0].ports}")
				output, err = utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				// Portless services have ports: null (jsonpath returns "null") or ports: [] (jsonpath returns "")
				trimmedOutput = strings.TrimSpace(output)
				g.Expect(trimmedOutput == "" || trimmedOutput == "null").To(BeTrue(), "decode EndpointSlice should be portless, got: %q", trimmedOutput)
			}, 60*time.Second, time.Second).Should(Succeed())
		})
	})

	Context("Labels and Annotations Propagation", func() {
		const deploymentName = "test-labels"

		AfterEach(func() {
			cleanupDeployment(deploymentName)
		})

		It("should propagate custom labels and annotations to LWS", func() {
			By("creating DisaggregatedSet with custom labels and annotations")
			yaml := `apiVersion: disaggregatedset.x-k8s.io/v1alpha1
kind: DisaggregatedSet
metadata:
  name: test-labels
  namespace: default
spec:
  phases:
  - name: prefill
    replicas: 1
    leaderWorkerTemplate:
      size: 1
      workerTemplate:
        metadata:
          labels:
            custom-label: prefill-value
            env: production
          annotations:
            custom-annotation: prefill-annotation
            prometheus.io/scrape: "true"
        spec:
          containers:
          - name: main
            image: registry.k8s.io/pause:3.9
  - name: decode
    replicas: 1
    leaderWorkerTemplate:
      size: 1
      workerTemplate:
        metadata:
          labels:
            custom-label: decode-value
          annotations:
            custom-annotation: decode-annotation
        spec:
          containers:
          - name: main
            image: registry.k8s.io/pause:3.9
`
			Expect(applyYAML(yaml)).To(Succeed())

			By("verifying LWS has merged labels and user annotations")
			Eventually(func(g Gomega) {
				// Check prefill LWS labels
				cmd := exec.Command("kubectl", "get", "lws", "-l",
					fmt.Sprintf("disaggregatedset.x-k8s.io/name=%s,disaggregatedset.x-k8s.io/phase=prefill", deploymentName),
					"-n", "default", "-o", "jsonpath={.items[0].spec.leaderWorkerTemplate.workerTemplate.metadata.labels}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				// Verify user labels are present
				g.Expect(output).To(ContainSubstring("custom-label"))
				g.Expect(output).To(ContainSubstring("prefill-value"))
				g.Expect(output).To(ContainSubstring("env"))
				g.Expect(output).To(ContainSubstring("production"))
				// Verify auto-populated labels are present
				g.Expect(output).To(ContainSubstring("disaggregatedset.x-k8s.io/name"))
				g.Expect(output).To(ContainSubstring("disaggregatedset.x-k8s.io/phase"))

				// Check prefill LWS annotations
				cmd = exec.Command("kubectl", "get", "lws", "-l",
					fmt.Sprintf("disaggregatedset.x-k8s.io/name=%s,disaggregatedset.x-k8s.io/phase=prefill", deploymentName),
					"-n", "default", "-o", "jsonpath={.items[0].spec.leaderWorkerTemplate.workerTemplate.metadata.annotations}")
				output, err = utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(ContainSubstring("custom-annotation"))
				g.Expect(output).To(ContainSubstring("prefill-annotation"))
				g.Expect(output).To(ContainSubstring("prometheus.io/scrape"))
			}, 60*time.Second, time.Second).Should(Succeed())

			By("verifying Services have standard labels only")
			Eventually(func(g Gomega) {
				// Check prefill service labels (only standard labels, no user-configurable ones)
				cmd := exec.Command("kubectl", "get", "svc", "-l",
					fmt.Sprintf("disaggregatedset.x-k8s.io/name=%s,disaggregatedset.x-k8s.io/phase=prefill", deploymentName),
					"-n", "default", "-o", "jsonpath={.items[0].metadata.labels}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				// Verify standard labels are present
				g.Expect(output).To(ContainSubstring("disaggregatedset.x-k8s.io/name"))
				g.Expect(output).To(ContainSubstring("disaggregatedset.x-k8s.io/phase"))
				g.Expect(output).To(ContainSubstring("disaggregatedset.x-k8s.io/revision"))
			}, 60*time.Second, time.Second).Should(Succeed())
		})
	})

	Context("Cleanup and Deletion", func() {
		const deploymentName = "test-cleanup"

		It("should garbage collect all child resources when deleted", func() {
			By("creating DisaggregatedSet")
			yaml := buildDisaggregatedSetYAML(disaggregatedSetConfig{
				Name:          deploymentName,
				PrefillConfig: phaseConfig{Replicas: 1},
				DecodeConfig:  phaseConfig{Replicas: 1},
			})
			Expect(applyYAML(yaml)).To(Succeed())

			By("waiting for resources to be created")
			Eventually(func(g Gomega) {
				// Check LWS resources
				cmd := exec.Command("kubectl", "get", "lws", "-l",
					fmt.Sprintf("disaggregatedset.x-k8s.io/name=%s", deploymentName),
					"-n", "default", "-o", "name")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(len(utils.GetNonEmptyLines(output))).To(Equal(2))

				// Check pods are running (needed for service creation)
				g.Expect(countRunningPods(deploymentName)).To(Equal(2))

				// Check Services are created (automatic headless services)
				cmd = exec.Command("kubectl", "get", "svc", "-l",
					fmt.Sprintf("disaggregatedset.x-k8s.io/name=%s", deploymentName),
					"-n", "default", "-o", "name")
				output, err = utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(len(utils.GetNonEmptyLines(output))).To(Equal(2))
			}, 90*time.Second, time.Second).Should(Succeed())

			By("deleting the DisaggregatedSet")
			cmd := exec.Command("kubectl", "delete", "disaggregatedset", deploymentName, "-n", "default")
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("verifying all LWS resources are garbage collected")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "lws", "-l",
					fmt.Sprintf("disaggregatedset.x-k8s.io/name=%s", deploymentName),
					"-n", "default", "-o", "name")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(len(utils.GetNonEmptyLines(output))).To(Equal(0))
			}, 60*time.Second, time.Second).Should(Succeed())

			By("verifying all Services are garbage collected")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "svc", "-l",
					fmt.Sprintf("disaggregatedset.x-k8s.io/name=%s", deploymentName),
					"-n", "default", "-o", "name")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(len(utils.GetNonEmptyLines(output))).To(Equal(0))
			}, 30*time.Second, time.Second).Should(Succeed())

			By("verifying all pods are removed")
			Eventually(func(g Gomega) {
				// Use kubectl to count pods not in Terminating state
				cmd := exec.Command("kubectl", "get", "pods", "-l",
					fmt.Sprintf("disaggregatedset.x-k8s.io/name=%s", deploymentName),
					"-n", "default", "-o", "name")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(len(utils.GetNonEmptyLines(output))).To(Equal(0))
			}, 90*time.Second, time.Second).Should(Succeed())
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
			cmd := exec.Command("kubectl", "delete", "disaggregatedset", deploymentName,
				"-n", "default", "--ignore-not-found", "--timeout=60s")
			_, _ = utils.Run(cmd)

			cmd = exec.Command("kubectl", "delete", "lws", "-l",
				fmt.Sprintf("disaggregatedset.x-k8s.io/name=%s", deploymentName),
				"-n", "default", "--ignore-not-found", "--timeout=60s")
			_, _ = utils.Run(cmd)

			cmd = exec.Command("kubectl", "delete", "pods", "-l",
				fmt.Sprintf("disaggregatedset.x-k8s.io/name=%s", deploymentName),
				"-n", "default", "--ignore-not-found", "--grace-period=0", "--force")
			_, _ = utils.Run(cmd)
		})

		for _, tc := range testCases {
			tc := tc // capture range variable
			It(fmt.Sprintf("should track rollout steps for %s", tc.Name), func() {
				By("creating initial DisaggregatedSet with source replicas")
				initialYaml := buildDisaggregatedSetYAML(disaggregatedSetConfig{
					Name: deploymentName,
					PrefillConfig: phaseConfig{
						Replicas:   tc.SourcePrefill,
						HasRollout: true,
						MaxSurge:   tc.PrefillSurge,
					},
					DecodeConfig: phaseConfig{
						Replicas:   tc.SourceDecode,
						HasRollout: true,
						MaxSurge:   tc.DecodeSurge,
					},
				})
				Expect(applyYAML(initialYaml)).To(Succeed())

				By("waiting for initial deployment to stabilize")
				expectedInitialPods := tc.SourcePrefill + tc.SourceDecode
				Eventually(func(g Gomega) {
					g.Expect(countRunningPods(deploymentName)).To(Equal(expectedInitialPods))
				}, 3*time.Minute, time.Second).Should(Succeed())

				// Get the initial revision
				cmd := exec.Command("kubectl", "get", "lws", "-l",
					fmt.Sprintf("disaggregatedset.x-k8s.io/name=%s", deploymentName),
					"-n", "default", "-o", "jsonpath={.items[0].metadata.labels.disaggregatedset\\.x-k8s\\.io/revision}")
				oldRevision, err := utils.Run(cmd)
				Expect(err).NotTo(HaveOccurred())
				oldRevision = strings.TrimSpace(oldRevision)
				_, _ = fmt.Fprintf(GinkgoWriter, "Initial revision: %s\n", oldRevision)

				// Capture initial state BEFORE triggering update
				initialState := getCurrentRolloutState(deploymentName, oldRevision)
				_, _ = fmt.Fprintf(GinkgoWriter, "Initial state captured: %s\n", initialState)

				By("triggering rolling update by changing image and target replicas")
				updatedYaml := buildDisaggregatedSetYAML(disaggregatedSetConfig{
					Name: deploymentName,
					PrefillConfig: phaseConfig{
						Replicas:   tc.TargetPrefill,
						Image:      "registry.k8s.io/pause:3.10",
						HasRollout: true,
						MaxSurge:   tc.PrefillSurge,
					},
					DecodeConfig: phaseConfig{
						Replicas:   tc.TargetDecode,
						Image:      "registry.k8s.io/pause:3.10",
						HasRollout: true,
						MaxSurge:   tc.DecodeSurge,
					},
				})
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
			cleanupDeployment(deploymentName)
		})

		It("should complete rolling update with 3 phases scaling together", func() {
			By("creating initial 3-phase DisaggregatedSet")
			initialYaml := buildNPhaseDisaggregatedSetYAML(nPhaseDisaggregatedSetConfig{
				Name: deploymentName,
				Phases: []nPhaseSpec{
					{Name: "prefill", Replicas: 2, HasRollout: true, MaxSurge: 1},
					{Name: "decode", Replicas: 2, HasRollout: true, MaxSurge: 1},
					{Name: "encode", Replicas: 2, HasRollout: true, MaxSurge: 1},
				},
			})
			Expect(applyYAML(initialYaml)).To(Succeed())

			By("waiting for initial deployment to stabilize")
			Eventually(func(g Gomega) {
				g.Expect(countRunningPods(deploymentName)).To(Equal(6)) // 2 per phase * 3 phases
			}, 3*time.Minute, time.Second).Should(Succeed())

			By("verifying 3 LWS resources exist (one per phase)")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "lws", "-l",
					fmt.Sprintf("disaggregatedset.x-k8s.io/name=%s", deploymentName),
					"-n", "default", "-o", "name")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(len(utils.GetNonEmptyLines(output))).To(Equal(3))
			}, 60*time.Second, time.Second).Should(Succeed())

			By("triggering rolling update by changing image")
			updatedYaml := buildNPhaseDisaggregatedSetYAML(nPhaseDisaggregatedSetConfig{
				Name: deploymentName,
				Phases: []nPhaseSpec{
					{Name: "prefill", Replicas: 2, Image: "registry.k8s.io/pause:3.10", HasRollout: true, MaxSurge: 1},
					{Name: "decode", Replicas: 2, Image: "registry.k8s.io/pause:3.10", HasRollout: true, MaxSurge: 1},
					{Name: "encode", Replicas: 2, Image: "registry.k8s.io/pause:3.10", HasRollout: true, MaxSurge: 1},
				},
			})
			Expect(applyYAML(updatedYaml)).To(Succeed())

			By("waiting for rolling update to complete")
			Eventually(func(g Gomega) {
				// Get all LWS and check only one revision has replicas
				cmd := exec.Command("kubectl", "get", "lws", "-l",
					fmt.Sprintf("disaggregatedset.x-k8s.io/name=%s", deploymentName),
					"-n", "default", "-o", "jsonpath={range .items[*]}{.metadata.labels.disaggregatedset\\.x-k8s\\.io/revision} {.spec.replicas}\\n{end}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())

				revisionReplicas := make(map[string]int)
				for _, line := range utils.GetNonEmptyLines(output) {
					var revision string
					var replicas int
					_, _ = fmt.Sscanf(line, "%s %d", &revision, &replicas)
					if revision != "" {
						revisionReplicas[revision] += replicas
					}
				}

				activeRevisions := 0
				for _, replicas := range revisionReplicas {
					if replicas > 0 {
						activeRevisions++
					}
				}
				g.Expect(activeRevisions).To(Equal(1), "Expected only one active revision after rolling update")
			}, 4*time.Minute, 2*time.Second).Should(Succeed())

			By("verifying all 3 phases have correct pod count")
			Eventually(func(g Gomega) {
				g.Expect(countRunningPods(deploymentName)).To(Equal(6))
			}, 60*time.Second, time.Second).Should(Succeed())
		})
	})

	Context("N-Phase Rename (add + remove phase)", func() {
		const deploymentName = "test-phase-rename"

		AfterEach(func() {
			cleanupDeployment(deploymentName)
		})

		It("should handle phase rename (remove old phase, add new phase) progressively", func() {
			By("creating initial 3-phase DisaggregatedSet with phases: prefill, decode, encode and Flexible policy")
			initialYaml := buildNPhaseDisaggregatedSetYAML(nPhaseDisaggregatedSetConfig{
				Name:        deploymentName,
				PhasePolicy: "Flexible",
				Phases: []nPhaseSpec{
					{Name: "prefill", Replicas: 2, HasRollout: true, MaxSurge: 1},
					{Name: "decode", Replicas: 2, HasRollout: true, MaxSurge: 1},
					{Name: "encode", Replicas: 2, HasRollout: true, MaxSurge: 1},
				},
			})
			Expect(applyYAML(initialYaml)).To(Succeed())

			By("waiting for initial deployment to stabilize")
			Eventually(func(g Gomega) {
				g.Expect(countRunningPods(deploymentName)).To(Equal(6))
			}, 3*time.Minute, time.Second).Should(Succeed())

			// Get the initial revision
			cmd := exec.Command("kubectl", "get", "lws", "-l",
				fmt.Sprintf("disaggregatedset.x-k8s.io/name=%s", deploymentName),
				"-n", "default", "-o", "jsonpath={.items[0].metadata.labels.disaggregatedset\\.x-k8s\\.io/revision}")
			oldRevision, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			oldRevision = strings.TrimSpace(oldRevision)

			By("applying update that renames 'encode' to 'decode-long-context'")
			// This is effectively a phase rename: prefill, decode, encode -> prefill, decode, decode-long-context
			updatedYaml := buildNPhaseDisaggregatedSetYAML(nPhaseDisaggregatedSetConfig{
				Name:        deploymentName,
				PhasePolicy: "Flexible",
				Phases: []nPhaseSpec{
					{Name: "prefill", Replicas: 2, Image: "registry.k8s.io/pause:3.10", HasRollout: true, MaxSurge: 1},
					{Name: "decode", Replicas: 2, Image: "registry.k8s.io/pause:3.10", HasRollout: true, MaxSurge: 1},
					{Name: "decode-long-context", Replicas: 2, Image: "registry.k8s.io/pause:3.10", HasRollout: true, MaxSurge: 1},
				},
			})
			Expect(applyYAML(updatedYaml)).To(Succeed())

			By("waiting for rolling update to complete")
			Eventually(func(g Gomega) {
				// Verify old revision has 0 replicas
				cmd := exec.Command("kubectl", "get", "lws", "-l",
					fmt.Sprintf("disaggregatedset.x-k8s.io/name=%s,disaggregatedset.x-k8s.io/revision=%s", deploymentName, oldRevision),
					"-n", "default", "-o", "jsonpath={range .items[*]}{.spec.replicas}\\n{end}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())

				totalOldReplicas := 0
				for _, line := range utils.GetNonEmptyLines(output) {
					replicas, _ := strconv.Atoi(line)
					totalOldReplicas += replicas
				}
				g.Expect(totalOldReplicas).To(Equal(0), "Old revision should have 0 replicas")
			}, 4*time.Minute, 2*time.Second).Should(Succeed())

			By("verifying new phases exist with correct replicas")
			Eventually(func(g Gomega) {
				// Check decode-long-context phase exists (the new phase)
				cmd := exec.Command("kubectl", "get", "lws", "-l",
					fmt.Sprintf("disaggregatedset.x-k8s.io/name=%s,disaggregatedset.x-k8s.io/phase=decode-long-context", deploymentName),
					"-n", "default", "-o", "jsonpath={.items[0].spec.replicas}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				replicas, _ := strconv.Atoi(strings.TrimSpace(output))
				g.Expect(replicas).To(Equal(2), "decode-long-context phase should have 2 replicas")

				// Verify encode phase from old revision is scaled to 0 or deleted
				cmd = exec.Command("kubectl", "get", "lws", "-l",
					fmt.Sprintf("disaggregatedset.x-k8s.io/name=%s,disaggregatedset.x-k8s.io/phase=encode", deploymentName),
					"-n", "default", "-o", "jsonpath={range .items[*]}{.spec.replicas}\\n{end}")
				output, err = utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				for _, line := range utils.GetNonEmptyLines(output) {
					replicas, _ := strconv.Atoi(line)
					g.Expect(replicas).To(Equal(0), "encode phase should be scaled to 0")
				}
			}, 60*time.Second, time.Second).Should(Succeed())

			By("verifying total running pods is correct (waiting for old pods to terminate)")
			Eventually(func(g Gomega) {
				g.Expect(countRunningPods(deploymentName)).To(Equal(6))
			}, 2*time.Minute, time.Second).Should(Succeed())
		})
	})

	Context("Add phase with rolling update (Flexible policy)", func() {
		const deploymentName = "test-add-phase"

		AfterEach(func() {
			cleanupDeployment(deploymentName)
		})

		It("should add new phase and roll all phases to new revision", func() {
			By("creating initial 2-phase DisaggregatedSet with Flexible policy")
			initialYaml := buildNPhaseDisaggregatedSetYAML(nPhaseDisaggregatedSetConfig{
				Name:        deploymentName,
				PhasePolicy: "Flexible",
				Phases: []nPhaseSpec{
					{Name: "alpha", Replicas: 2, HasRollout: true, MaxSurge: 1},
					{Name: "beta", Replicas: 3, HasRollout: true, MaxSurge: 1},
				},
			})
			Expect(applyYAML(initialYaml)).To(Succeed())

			By("waiting for initial deployment to stabilize")
			Eventually(func(g Gomega) {
				g.Expect(countRunningPods(deploymentName)).To(Equal(5))
			}, 3*time.Minute, time.Second).Should(Succeed())

			By("recording initial revision")
			cmd := exec.Command("kubectl", "get", "lws", "-l",
				fmt.Sprintf("disaggregatedset.x-k8s.io/name=%s", deploymentName),
				"-n", "default", "-o", "jsonpath={.items[0].metadata.labels.disaggregatedset\\.x-k8s\\.io/revision}")
			oldRevision, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			oldRevision = strings.TrimSpace(oldRevision)
			Expect(oldRevision).NotTo(BeEmpty())

			By("adding new phase 'gamma'")
			updatedYaml := buildNPhaseDisaggregatedSetYAML(nPhaseDisaggregatedSetConfig{
				Name:        deploymentName,
				PhasePolicy: "Flexible",
				Phases: []nPhaseSpec{
					{Name: "alpha", Replicas: 2, HasRollout: true, MaxSurge: 1},
					{Name: "beta", Replicas: 3, HasRollout: true, MaxSurge: 1},
					{Name: "gamma", Replicas: 5, HasRollout: true, MaxSurge: 1},
				},
			})
			Expect(applyYAML(updatedYaml)).To(Succeed())

			By("waiting for rolling update to complete - all phases at new revision")
			Eventually(func(g Gomega) {
				// Verify old revision has 0 replicas
				cmd := exec.Command("kubectl", "get", "lws", "-l",
					fmt.Sprintf("disaggregatedset.x-k8s.io/name=%s,disaggregatedset.x-k8s.io/revision=%s", deploymentName, oldRevision),
					"-n", "default", "-o", "jsonpath={range .items[*]}{.spec.replicas}\\n{end}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())

				totalOldReplicas := 0
				for _, line := range utils.GetNonEmptyLines(output) {
					replicas, _ := strconv.Atoi(line)
					totalOldReplicas += replicas
				}
				g.Expect(totalOldReplicas).To(Equal(0), "Old revision should have 0 replicas")
			}, 4*time.Minute, 2*time.Second).Should(Succeed())

			By("verifying gamma phase exists with correct replicas")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "lws", "-l",
					fmt.Sprintf("disaggregatedset.x-k8s.io/name=%s,disaggregatedset.x-k8s.io/phase=gamma", deploymentName),
					"-n", "default", "-o", "jsonpath={.items[0].spec.replicas}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				replicas, _ := strconv.Atoi(strings.TrimSpace(output))
				g.Expect(replicas).To(Equal(5), "gamma phase should have 5 replicas")
			}, 60*time.Second, time.Second).Should(Succeed())

			By("verifying total running pods")
			Eventually(func(g Gomega) {
				g.Expect(countRunningPods(deploymentName)).To(Equal(10)) // 2 + 3 + 5
			}, 2*time.Minute, time.Second).Should(Succeed())
		})
	})

	Context("Add phase with progressive rolling update (Flexible policy)", func() {
		const deploymentName = "test-add-phase-progressive"

		AfterEach(func() {
			cleanupDeployment(deploymentName)
		})

		It("should add new phase with progressive rollout (not brutal drain)", func() {
			By("creating initial 2-phase DisaggregatedSet with Flexible policy and larger replica counts")
			// Use larger replica counts to ensure we can observe intermediate states
			initialYaml := buildNPhaseDisaggregatedSetYAML(nPhaseDisaggregatedSetConfig{
				Name:        deploymentName,
				PhasePolicy: "Flexible",
				Phases: []nPhaseSpec{
					{Name: "prefill", Replicas: 4, HasRollout: true, MaxSurge: 1},
					{Name: "decode", Replicas: 6, HasRollout: true, MaxSurge: 1},
				},
			})
			Expect(applyYAML(initialYaml)).To(Succeed())

			By("waiting for initial deployment to stabilize")
			Eventually(func(g Gomega) {
				g.Expect(countRunningPods(deploymentName)).To(Equal(10)) // 4 + 6
			}, 3*time.Minute, time.Second).Should(Succeed())

			By("recording initial revision")
			cmd := exec.Command("kubectl", "get", "lws", "-l",
				fmt.Sprintf("disaggregatedset.x-k8s.io/name=%s", deploymentName),
				"-n", "default", "-o", "jsonpath={.items[0].metadata.labels.disaggregatedset\\.x-k8s\\.io/revision}")
			oldRevision, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			oldRevision = strings.TrimSpace(oldRevision)
			Expect(oldRevision).NotTo(BeEmpty())

			By("adding new phase 'encode' - this should trigger progressive rollout")
			updatedYaml := buildNPhaseDisaggregatedSetYAML(nPhaseDisaggregatedSetConfig{
				Name:        deploymentName,
				PhasePolicy: "Flexible",
				Phases: []nPhaseSpec{
					{Name: "prefill", Replicas: 4, HasRollout: true, MaxSurge: 1},
					{Name: "decode", Replicas: 6, HasRollout: true, MaxSurge: 1},
					{Name: "encode", Replicas: 2, HasRollout: true, MaxSurge: 1},
				},
			})
			Expect(applyYAML(updatedYaml)).To(Succeed())

			By("tracking rollout to verify progressive scaling (not brutal drain)")
			// The key invariant: old workloads should NOT all go to 0 immediately
			// We should see intermediate states where old workloads have > 0 replicas
			// while new workloads are scaling up

			sawIntermediateOldReplicas := false
			sawNewWorkloadsScalingUp := false

			// Poll rapidly to capture intermediate states
			startTime := time.Now()
			maxDuration := 4 * time.Minute
			for time.Since(startTime) < maxDuration {
				// Get old workload replicas
				cmd := exec.Command("kubectl", "get", "lws", "-l",
					fmt.Sprintf("disaggregatedset.x-k8s.io/name=%s,disaggregatedset.x-k8s.io/revision=%s", deploymentName, oldRevision),
					"-n", "default", "-o", "jsonpath={range .items[*]}{.spec.replicas} {end}")
				output, err := utils.Run(cmd)
				if err == nil {
					oldTotal := 0
					for _, s := range strings.Fields(strings.TrimSpace(output)) {
						replicas, _ := strconv.Atoi(s)
						oldTotal += replicas
					}

					// Get new workload replicas
					cmd = exec.Command("kubectl", "get", "lws", "-l",
						fmt.Sprintf("disaggregatedset.x-k8s.io/name=%s,disaggregatedset.x-k8s.io/revision!=%s", deploymentName, oldRevision),
						"-n", "default", "-o", "jsonpath={range .items[*]}{.spec.replicas} {end}")
					output, err = utils.Run(cmd)
					if err == nil {
						newTotal := 0
						for _, s := range strings.Fields(strings.TrimSpace(output)) {
							replicas, _ := strconv.Atoi(s)
							newTotal += replicas
						}

						_, _ = fmt.Fprintf(GinkgoWriter, "State: old=%d, new=%d\n", oldTotal, newTotal)

						// Track intermediate states
						if oldTotal > 0 && oldTotal < 10 {
							sawIntermediateOldReplicas = true
						}
						if newTotal > 0 && newTotal < 12 { // target is 4+6+2=12
							sawNewWorkloadsScalingUp = true
						}

						// Check if rollout is complete
						if oldTotal == 0 && newTotal == 12 {
							break
						}
					}
				}

				time.Sleep(200 * time.Millisecond)
			}

			By("verifying progressive rollout occurred (not brutal drain)")
			// The critical assertion: we should have seen intermediate states
			// If the old code bug existed, old workloads would go 10 -> 0 immediately
			// With the fix, we should see intermediate states like 10 -> 9 -> 8 -> ... -> 0
			Expect(sawIntermediateOldReplicas || sawNewWorkloadsScalingUp).To(BeTrue(),
				"Should have observed intermediate states during progressive rollout. "+
					"If this fails, old workloads may have been brutally drained to 0 immediately.")

			By("verifying final state - all phases at new revision")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "lws", "-l",
					fmt.Sprintf("disaggregatedset.x-k8s.io/name=%s,disaggregatedset.x-k8s.io/revision=%s", deploymentName, oldRevision),
					"-n", "default", "-o", "jsonpath={range .items[*]}{.spec.replicas} {end}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())

				totalOldReplicas := 0
				for _, s := range strings.Fields(strings.TrimSpace(output)) {
					replicas, _ := strconv.Atoi(s)
					totalOldReplicas += replicas
				}
				g.Expect(totalOldReplicas).To(Equal(0), "Old revision should have 0 replicas")
			}, 60*time.Second, time.Second).Should(Succeed())

			By("verifying encode phase exists with correct replicas")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "lws", "-l",
					fmt.Sprintf("disaggregatedset.x-k8s.io/name=%s,disaggregatedset.x-k8s.io/phase=encode", deploymentName),
					"-n", "default", "-o", "jsonpath={.items[0].spec.replicas}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				replicas, _ := strconv.Atoi(strings.TrimSpace(output))
				g.Expect(replicas).To(Equal(2), "encode phase should have 2 replicas")
			}, 60*time.Second, time.Second).Should(Succeed())

			By("verifying total running pods")
			Eventually(func(g Gomega) {
				g.Expect(countRunningPods(deploymentName)).To(Equal(12)) // 4 + 6 + 2
			}, 2*time.Minute, time.Second).Should(Succeed())
		})
	})

	Context("Remove phase with progressive rolling update (Flexible policy)", func() {
		const deploymentName = "test-remove-phase-progressive"

		AfterEach(func() {
			cleanupDeployment(deploymentName)
		})

		It("should remove phase with progressive rollout for remaining phases", func() {
			By("creating initial 3-phase DisaggregatedSet with Flexible policy")
			initialYaml := buildNPhaseDisaggregatedSetYAML(nPhaseDisaggregatedSetConfig{
				Name:        deploymentName,
				PhasePolicy: "Flexible",
				Phases: []nPhaseSpec{
					{Name: "prefill", Replicas: 4, HasRollout: true, MaxSurge: 1},
					{Name: "decode", Replicas: 6, HasRollout: true, MaxSurge: 1},
					{Name: "encode", Replicas: 2, HasRollout: true, MaxSurge: 1},
				},
			})
			Expect(applyYAML(initialYaml)).To(Succeed())

			By("waiting for initial deployment to stabilize")
			Eventually(func(g Gomega) {
				g.Expect(countRunningPods(deploymentName)).To(Equal(12)) // 4 + 6 + 2
			}, 3*time.Minute, time.Second).Should(Succeed())

			By("recording initial revision")
			cmd := exec.Command("kubectl", "get", "lws", "-l",
				fmt.Sprintf("disaggregatedset.x-k8s.io/name=%s", deploymentName),
				"-n", "default", "-o", "jsonpath={.items[0].metadata.labels.disaggregatedset\\.x-k8s\\.io/revision}")
			oldRevision, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			oldRevision = strings.TrimSpace(oldRevision)
			Expect(oldRevision).NotTo(BeEmpty())

			By("removing 'encode' phase - this should trigger progressive rollout")
			updatedYaml := buildNPhaseDisaggregatedSetYAML(nPhaseDisaggregatedSetConfig{
				Name:        deploymentName,
				PhasePolicy: "Flexible",
				Phases: []nPhaseSpec{
					{Name: "prefill", Replicas: 4, HasRollout: true, MaxSurge: 1},
					{Name: "decode", Replicas: 6, HasRollout: true, MaxSurge: 1},
				},
			})
			Expect(applyYAML(updatedYaml)).To(Succeed())

			By("tracking rollout to verify progressive scaling (encode drains progressively with other phases)")
			// Track states immediately after applying update to capture intermediate states
			// Old workloads: 12 total (4 prefill + 6 decode + 2 encode)
			// New workloads: 10 target (4 prefill + 6 decode, no encode)
			sawIntermediateOldReplicas := false
			sawNewWorkloadsScalingUp := false

			startTime := time.Now()
			maxDuration := 4 * time.Minute
			for time.Since(startTime) < maxDuration {
				// Get ALL old workload replicas (including encode which is progressively draining)
				cmd := exec.Command("kubectl", "get", "lws", "-l",
					fmt.Sprintf("disaggregatedset.x-k8s.io/name=%s,disaggregatedset.x-k8s.io/revision=%s", deploymentName, oldRevision),
					"-n", "default", "-o", "jsonpath={range .items[*]}{.spec.replicas} {end}")
				output, err := utils.Run(cmd)
				if err == nil {
					oldTotal := 0
					for _, s := range strings.Fields(strings.TrimSpace(output)) {
						replicas, _ := strconv.Atoi(s)
						oldTotal += replicas
					}

					// Get new workload replicas
					cmd = exec.Command("kubectl", "get", "lws", "-l",
						fmt.Sprintf("disaggregatedset.x-k8s.io/name=%s,disaggregatedset.x-k8s.io/revision!=%s", deploymentName, oldRevision),
						"-n", "default", "-o", "jsonpath={range .items[*]}{.spec.replicas} {end}")
					output, err = utils.Run(cmd)
					if err == nil {
						newTotal := 0
						for _, s := range strings.Fields(strings.TrimSpace(output)) {
							replicas, _ := strconv.Atoi(s)
							newTotal += replicas
						}

						_, _ = fmt.Fprintf(GinkgoWriter, "State: old=%d, new=%d\n", oldTotal, newTotal)

						// Track intermediate states
						// Old workloads start at 12, should decrease progressively
						if oldTotal > 0 && oldTotal < 12 {
							sawIntermediateOldReplicas = true
						}
						// New workloads target is 10 (4+6), should increase progressively
						if newTotal > 0 && newTotal < 10 {
							sawNewWorkloadsScalingUp = true
						}

						// Check if rollout is complete (old=0, new=10)
						if oldTotal == 0 && newTotal == 10 {
							break
						}
					}
				}

				time.Sleep(100 * time.Millisecond) // Poll faster to catch intermediate states
			}

			By("verifying progressive rollout occurred")
			Expect(sawIntermediateOldReplicas || sawNewWorkloadsScalingUp).To(BeTrue(),
				"Should have observed intermediate states during progressive rollout. "+
					"Old workloads should drain progressively (12->0) while new scale up (0->10).")

			By("verifying final state - all phases at new revision, encode gone")
			Eventually(func(g Gomega) {
				// Old revision should have 0 replicas
				cmd := exec.Command("kubectl", "get", "lws", "-l",
					fmt.Sprintf("disaggregatedset.x-k8s.io/name=%s,disaggregatedset.x-k8s.io/revision=%s", deploymentName, oldRevision),
					"-n", "default", "-o", "jsonpath={range .items[*]}{.spec.replicas} {end}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())

				totalOldReplicas := 0
				for _, s := range strings.Fields(strings.TrimSpace(output)) {
					replicas, _ := strconv.Atoi(s)
					totalOldReplicas += replicas
				}
				g.Expect(totalOldReplicas).To(Equal(0), "Old revision should have 0 replicas")
			}, 60*time.Second, time.Second).Should(Succeed())

			By("verifying encode phase no longer exists or has 0 replicas")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "lws", "-l",
					fmt.Sprintf("disaggregatedset.x-k8s.io/name=%s,disaggregatedset.x-k8s.io/phase=encode", deploymentName),
					"-n", "default", "-o", "jsonpath={range .items[*]}{.spec.replicas} {end}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				for _, s := range strings.Fields(strings.TrimSpace(output)) {
					replicas, _ := strconv.Atoi(s)
					g.Expect(replicas).To(Equal(0), "encode phase should be drained to 0")
				}
			}, 30*time.Second, time.Second).Should(Succeed())

			By("verifying total running pods (no encode phase)")
			Eventually(func(g Gomega) {
				g.Expect(countRunningPods(deploymentName)).To(Equal(10)) // 4 + 6
			}, 2*time.Minute, time.Second).Should(Succeed())
		})
	})

	Context("Rename phase with rolling update (Flexible policy)", func() {
		const deploymentName = "test-rename-phase"

		AfterEach(func() {
			cleanupDeployment(deploymentName)
		})

		It("should drain old phase progressively and roll all phases to new revision", func() {
			By("creating initial 3-phase DisaggregatedSet with Flexible policy")
			initialYaml := buildNPhaseDisaggregatedSetYAML(nPhaseDisaggregatedSetConfig{
				Name:        deploymentName,
				PhasePolicy: "Flexible",
				Phases: []nPhaseSpec{
					{Name: "alpha", Replicas: 2, HasRollout: true, MaxSurge: 1},
					{Name: "beta", Replicas: 3, HasRollout: true, MaxSurge: 1},
					{Name: "zeta", Replicas: 5, HasRollout: true, MaxSurge: 1},
				},
			})
			Expect(applyYAML(initialYaml)).To(Succeed())

			By("waiting for initial deployment to stabilize")
			Eventually(func(g Gomega) {
				g.Expect(countRunningPods(deploymentName)).To(Equal(10))
			}, 3*time.Minute, time.Second).Should(Succeed())

			By("recording initial revision")
			cmd := exec.Command("kubectl", "get", "lws", "-l",
				fmt.Sprintf("disaggregatedset.x-k8s.io/name=%s", deploymentName),
				"-n", "default", "-o", "jsonpath={.items[0].metadata.labels.disaggregatedset\\.x-k8s\\.io/revision}")
			oldRevision, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			oldRevision = strings.TrimSpace(oldRevision)

			By("renaming 'zeta' to 'gamma' (remove zeta, add gamma)")
			updatedYaml := buildNPhaseDisaggregatedSetYAML(nPhaseDisaggregatedSetConfig{
				Name:        deploymentName,
				PhasePolicy: "Flexible",
				Phases: []nPhaseSpec{
					{Name: "alpha", Replicas: 2, HasRollout: true, MaxSurge: 1},
					{Name: "beta", Replicas: 3, HasRollout: true, MaxSurge: 1},
					{Name: "gamma", Replicas: 5, HasRollout: true, MaxSurge: 1},
				},
			})
			Expect(applyYAML(updatedYaml)).To(Succeed())

			By("verifying zeta is progressively drained (removed phase)")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "lws", "-l",
					fmt.Sprintf("disaggregatedset.x-k8s.io/name=%s,disaggregatedset.x-k8s.io/phase=zeta", deploymentName),
					"-n", "default", "-o", "jsonpath={range .items[*]}{.spec.replicas}\\n{end}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				for _, line := range utils.GetNonEmptyLines(output) {
					replicas, _ := strconv.Atoi(line)
					g.Expect(replicas).To(Equal(0), "zeta phase should be drained to 0")
				}
			}, 60*time.Second, time.Second).Should(Succeed())

			By("waiting for rolling update to complete")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "lws", "-l",
					fmt.Sprintf("disaggregatedset.x-k8s.io/name=%s,disaggregatedset.x-k8s.io/revision=%s", deploymentName, oldRevision),
					"-n", "default", "-o", "jsonpath={range .items[*]}{.spec.replicas}\\n{end}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())

				totalOldReplicas := 0
				for _, line := range utils.GetNonEmptyLines(output) {
					replicas, _ := strconv.Atoi(line)
					totalOldReplicas += replicas
				}
				g.Expect(totalOldReplicas).To(Equal(0), "Old revision should have 0 replicas")
			}, 4*time.Minute, 2*time.Second).Should(Succeed())

			By("verifying gamma phase exists with correct replicas")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "lws", "-l",
					fmt.Sprintf("disaggregatedset.x-k8s.io/name=%s,disaggregatedset.x-k8s.io/phase=gamma", deploymentName),
					"-n", "default", "-o", "jsonpath={.items[0].spec.replicas}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				replicas, _ := strconv.Atoi(strings.TrimSpace(output))
				g.Expect(replicas).To(Equal(5), "gamma phase should have 5 replicas")
			}, 60*time.Second, time.Second).Should(Succeed())

			By("verifying total running pods")
			Eventually(func(g Gomega) {
				g.Expect(countRunningPods(deploymentName)).To(Equal(10)) // 2 + 3 + 5
			}, 2*time.Minute, time.Second).Should(Succeed())
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
	// Get all LWS for this deployment with their revision, phase, and replicas
	cmd := exec.Command("kubectl", "get", "lws", "-l",
		fmt.Sprintf("disaggregatedset.x-k8s.io/name=%s", deploymentName),
		"-n", "default", "-o",
		"jsonpath={range .items[*]}{.metadata.labels.disaggregatedset\\.x-k8s\\.io/revision},{.metadata.labels.disaggregatedset\\.x-k8s\\.io/phase},{.spec.replicas}\n{end}")

	output, err := utils.Run(cmd)
	if err != nil {
		return rolloutState{}
	}

	state := rolloutState{}
	for _, line := range utils.GetNonEmptyLines(output) {
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

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
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	utils "sigs.k8s.io/lws/test/testutils/disaggregatedset"
)

func TestE2E(t *testing.T) {
	RegisterFailHandler(Fail)
	_, _ = fmt.Fprintf(GinkgoWriter, "Starting disaggregatedset e2e test suite\n")
	RunSpecs(t, "DisaggregatedSet E2E Suite")
}

var _ = BeforeSuite(func() {
	By("verifying LWS controller is ready")
	Eventually(func() error {
		cmd := exec.Command("kubectl", "get", "deployment", "lws-controller-manager",
			"-n", "lws-system", "-o", "jsonpath={.status.availableReplicas}")
		output, err := utils.Run(cmd)
		if err != nil {
			return fmt.Errorf("LWS controller not found: %w", err)
		}
		if output == "" || output == "0" {
			return fmt.Errorf("LWS controller not ready, availableReplicas: %s", output)
		}
		return nil
	}, 2*time.Minute, 2*time.Second).Should(Succeed(), "LWS controller must be running")

	By("verifying DisaggregatedSet CRD is installed")
	cmd := exec.Command("kubectl", "get", "crd", "disaggregatedsets.disaggregatedset.x-k8s.io")
	_, err := utils.Run(cmd)
	Expect(err).NotTo(HaveOccurred(), "DisaggregatedSet CRD must be installed")
})

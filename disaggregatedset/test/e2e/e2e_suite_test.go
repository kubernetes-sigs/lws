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
	"os"
	"os/exec"
	"strings"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"sigs.k8s.io/disaggregatedset/test/utils"
)

var (
	// skipLWSInstall skips LWS installation if already present
	skipLWSInstall = os.Getenv("LWS_INSTALL_SKIP") == "true"

	// isLWSAlreadyInstalled tracks if LWS was pre-installed
	isLWSAlreadyInstalled = false

	// projectImage is the operator image that will be built and loaded
	projectImage = "controller:e2e"

	// kindCluster is the name of the Kind cluster to use
	kindCluster = "kind"

	// originalKubeContext stores the original kubectl context to restore after tests
	originalKubeContext = ""
)

func init() {
	if img := os.Getenv("IMG"); img != "" {
		projectImage = img
	}
	if cluster := os.Getenv("KIND_CLUSTER"); cluster != "" {
		kindCluster = cluster
	}
}

// TestE2E runs the end-to-end test suite for the DisaggDeployment operator.
func TestE2E(t *testing.T) {
	RegisterFailHandler(Fail)
	_, _ = fmt.Fprintf(GinkgoWriter, "Starting disaggregatedset e2e test suite\n")
	RunSpecs(t, "E2E Suite")
}

var _ = BeforeSuite(func() {
	By("switching kubectl context to Kind cluster")
	// Save the original context to restore later
	cmd := exec.Command("kubectl", "config", "current-context")
	output, err := utils.Run(cmd)
	if err == nil {
		originalKubeContext = strings.TrimSpace(output)
	}
	// Switch to the Kind cluster context
	kindContext := fmt.Sprintf("kind-%s", kindCluster)
	cmd = exec.Command("kubectl", "config", "use-context", kindContext)
	_, err = utils.Run(cmd)
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "Failed to switch kubectl context to Kind cluster")

	By("building the operator image")
	cmd = exec.Command("make", "docker-build", fmt.Sprintf("IMG=%s", projectImage))
	_, err = utils.Run(cmd)
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "Failed to build the operator image")

	By("loading the operator image to Kind")
	err = utils.LoadImageToKindClusterWithName(projectImage)
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "Failed to load the operator image into Kind")

	// Install LWS if not skipped and not already installed
	if !skipLWSInstall {
		By("checking if LWS is already installed")
		isLWSAlreadyInstalled = utils.IsLWSInstalled()
		if !isLWSAlreadyInstalled {
			_, _ = fmt.Fprintf(GinkgoWriter, "Installing LWS controller...\n")
			Expect(utils.InstallLWS()).To(Succeed(), "Failed to install LWS")
		} else {
			_, _ = fmt.Fprintf(GinkgoWriter, "LWS is already installed, skipping installation\n")
		}
	}

	By("waiting for LWS to be ready")
	Expect(utils.WaitForLWSReady()).To(Succeed(), "LWS is not ready")

	By("installing DisaggDeployment CRDs")
	cmd = exec.Command("make", "install")
	_, err = utils.Run(cmd)
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "Failed to install CRDs")
})

var _ = AfterSuite(func() {
	By("uninstalling CRDs")
	cmd := exec.Command("make", "uninstall")
	_, _ = utils.Run(cmd)

	// Only uninstall LWS if we installed it
	if !skipLWSInstall && !isLWSAlreadyInstalled {
		_, _ = fmt.Fprintf(GinkgoWriter, "Uninstalling LWS controller...\n")
		utils.UninstallLWS()
	}

	// Restore the original kubectl context
	if originalKubeContext != "" {
		By("restoring original kubectl context")
		cmd = exec.Command("kubectl", "config", "use-context", originalKubeContext)
		_, _ = utils.Run(cmd)
	}

	// Delete the Kind cluster to ensure a clean state for the next run
	// This prevents image caching issues where old images persist across test runs
	By("deleting Kind cluster")
	kindBin := os.Getenv("KIND")
	if kindBin == "" {
		kindBin = "kind"
	}
	cmd = exec.Command(kindBin, "delete", "cluster", "--name", kindCluster)
	_, _ = utils.Run(cmd)
})

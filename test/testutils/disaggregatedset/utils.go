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

package utils

import (
	"fmt"
	"os"
	"os/exec"
	"strings"

	. "github.com/onsi/ginkgo/v2" //nolint:revive,staticcheck
)

const (
	// LWS release version to install
	lwsVersion = "v0.8.0"
	lwsURLTmpl = "https://github.com/kubernetes-sigs/lws/releases/download/%s/manifests.yaml"

	defaultKindBinary  = "kind"
	defaultKindCluster = "kind"
)

func warnError(err error) {
	_, _ = fmt.Fprintf(GinkgoWriter, "warning: %v\n", err)
}

// Run executes the provided command within the project directory.
func Run(cmd *exec.Cmd) (string, error) {
	dir, _ := GetProjectDir()
	cmd.Dir = dir

	if err := os.Chdir(cmd.Dir); err != nil {
		_, _ = fmt.Fprintf(GinkgoWriter, "chdir dir: %q\n", err)
	}

	cmd.Env = append(os.Environ(), "GO111MODULE=on")
	command := strings.Join(cmd.Args, " ")
	_, _ = fmt.Fprintf(GinkgoWriter, "running: %q\n", command)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return string(output), fmt.Errorf("%q failed with error %q: %w", command, string(output), err)
	}

	return string(output), nil
}

// GetProjectDir returns the directory where the project is located.
func GetProjectDir() (string, error) {
	workingDir, err := os.Getwd()
	if err != nil {
		return workingDir, fmt.Errorf("failed to get current working directory: %w", err)
	}
	// Handle being run from test/e2e subdirectory
	workingDir = strings.ReplaceAll(workingDir, "/test/e2e", "")
	workingDir = strings.ReplaceAll(workingDir, "/test/utils", "")
	return workingDir, nil
}

// GetNonEmptyLines converts command output string into individual lines,
// ignoring empty elements.
func GetNonEmptyLines(output string) []string {
	var result []string
	elements := strings.Split(output, "\n")
	for _, element := range elements {
		if element != "" {
			result = append(result, element)
		}
	}
	return result
}

// LoadImageToKindClusterWithName loads a local docker image to the Kind cluster.
func LoadImageToKindClusterWithName(name string) error {
	cluster := defaultKindCluster
	if v, ok := os.LookupEnv("KIND_CLUSTER"); ok {
		cluster = v
	}
	kindOptions := []string{"load", "docker-image", name, "--name", cluster}
	kindBinary := defaultKindBinary
	if v, ok := os.LookupEnv("KIND"); ok {
		kindBinary = v
	}
	cmd := exec.Command(kindBinary, kindOptions...)
	_, err := Run(cmd)
	return err
}

// InstallLWS installs the LeaderWorkerSet controller from release manifests.
func InstallLWS() error {
	url := fmt.Sprintf(lwsURLTmpl, lwsVersion)
	_, _ = fmt.Fprintf(GinkgoWriter, "Installing LWS %s from %s\n", lwsVersion, url)

	cmd := exec.Command("kubectl", "apply", "--server-side", "-f", url)
	if _, err := Run(cmd); err != nil {
		return fmt.Errorf("failed to install LWS: %w", err)
	}

	// Wait for LWS controller deployment to be ready
	cmd = exec.Command("kubectl", "wait", "deployment/lws-controller-manager",
		"--for", "condition=Available",
		"--namespace", "lws-system",
		"--timeout", "5m",
	)
	if _, err := Run(cmd); err != nil {
		return fmt.Errorf("failed waiting for LWS controller: %w", err)
	}

	return nil
}

// UninstallLWS removes the LeaderWorkerSet controller.
func UninstallLWS() {
	url := fmt.Sprintf(lwsURLTmpl, lwsVersion)
	cmd := exec.Command("kubectl", "delete", "-f", url, "--ignore-not-found")
	if _, err := Run(cmd); err != nil {
		warnError(err)
	}
}

// IsLWSInstalled checks if LWS CRDs are installed in the cluster.
func IsLWSInstalled() bool {
	cmd := exec.Command("kubectl", "get", "crd", "leaderworkersets.leaderworkerset.x-k8s.io")
	_, err := Run(cmd)
	return err == nil
}

// WaitForLWSReady waits for LWS webhook to be available by creating a test LWS.
func WaitForLWSReady() error {
	// Check if LWS controller is ready
	cmd := exec.Command("kubectl", "get", "deployment", "lws-controller-manager",
		"-n", "lws-system", "-o", "jsonpath={.status.availableReplicas}")
	output, err := Run(cmd)
	if err != nil {
		return fmt.Errorf("LWS controller not found: %w", err)
	}
	if output == "" || output == "0" {
		return fmt.Errorf("LWS controller not ready, availableReplicas: %s", output)
	}
	return nil
}

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

package kubectl

import (
	"strconv"
	"strings"
)

const (
	labelName     = "disaggregatedset.x-k8s.io/name"
	labelRole     = "disaggregatedset.x-k8s.io/role"
	labelRevision = "disaggregatedset.x-k8s.io/revision"
	defaultNS     = "default"
)

// --- LWS Queries ---

// LWS returns a builder for querying LWS resources by deployment name.
func LWS(deploymentName string) *Builder {
	return Get("lws").Label(labelName, deploymentName).Namespace(defaultNS)
}

// LWSByRevision returns a builder for querying LWS by deployment and revision.
func LWSByRevision(deploymentName, revision string) *Builder {
	return LWS(deploymentName).Label(labelRevision, revision)
}

// LWSNotRevision returns a builder for querying LWS NOT matching a revision.
func LWSNotRevision(deploymentName, revision string) *Builder {
	return Get("lws").
		Label(labelName, deploymentName).
		Label(labelRevision+"!=", revision).
		Namespace(defaultNS)
}

// LWSByRole returns a builder for querying LWS by deployment and role.
func LWSByRole(deploymentName, role string) *Builder {
	return LWS(deploymentName).Label(labelRole, role)
}

// --- Pod Queries ---

// Pods returns a builder for querying pods by deployment name.
func Pods(deploymentName string) *Builder {
	return Get("pods").Label(labelName, deploymentName).Namespace(defaultNS)
}

// PodsByRole returns a builder for querying pods by deployment and role.
func PodsByRole(deploymentName, role string) *Builder {
	return Pods(deploymentName).Label(labelRole, role)
}

// RunningPods returns a builder for querying running pods.
func RunningPods(deploymentName string) *Builder {
	return Pods(deploymentName).FieldSelector("status.phase=Running")
}

// --- Service Queries ---

// Service returns a builder for querying services by deployment name.
func Service(deploymentName string) *Builder {
	return Get("svc").Label(labelName, deploymentName).Namespace(defaultNS)
}

// ServiceByRole returns a builder for querying services by deployment and role.
func ServiceByRole(deploymentName, role string) *Builder {
	return Service(deploymentName).Label(labelRole, role)
}

// --- EndpointSlice Queries ---

// EndpointSlice returns a builder for querying endpoint slices by deployment.
func EndpointSlice(deploymentName string) *Builder {
	return Get("endpointslice").Label(labelName, deploymentName).Namespace(defaultNS)
}

// EndpointSliceByRole returns a builder for querying endpoint slices by role.
func EndpointSliceByRole(deploymentName, role string) *Builder {
	return EndpointSlice(deploymentName).Label(labelRole, role)
}

// --- Counting Helpers ---

// CountPods returns the number of pods for a deployment.
func CountPods(deploymentName string) int {
	output, err := Pods(deploymentName).Output("name").RunQuiet()
	if err != nil {
		return 0
	}
	return len(GetNonEmptyLines(output))
}

// CountRunningPods returns the number of running pods for a deployment.
func CountRunningPods(deploymentName string) int {
	output, err := RunningPods(deploymentName).NoHeaders().RunQuiet()
	if err != nil {
		return 0
	}
	return len(GetNonEmptyLines(output))
}

// CountLWS returns the number of LWS resources for a deployment.
func CountLWS(deploymentName string) int {
	output, err := LWS(deploymentName).Output("name").RunQuiet()
	if err != nil {
		return 0
	}
	return len(GetNonEmptyLines(output))
}

// CountLWSByRole returns the number of LWS for a deployment and role.
func CountLWSByRole(deploymentName, role string) int {
	output, err := LWSByRole(deploymentName, role).Output("name").RunQuiet()
	if err != nil {
		return 0
	}
	return len(GetNonEmptyLines(output))
}

// CountService returns the number of services for a deployment.
func CountService(deploymentName string) int {
	output, err := Service(deploymentName).Output("name").RunQuiet()
	if err != nil {
		return 0
	}
	return len(GetNonEmptyLines(output))
}

// --- Replica Helpers ---

// GetTotalReplicas returns total replicas across all LWS for a revision.
func GetTotalReplicas(deploymentName, revision string) int {
	output, err := LWSByRevision(deploymentName, revision).
		JSONPath("{range .items[*]}{.spec.replicas} {end}").RunQuiet()
	if err != nil {
		return 0
	}
	return sumInts(output)
}

// GetTotalReplicasNotRevision returns total replicas for LWS NOT matching revision.
func GetTotalReplicasNotRevision(deploymentName, revision string) int {
	output, err := LWSNotRevision(deploymentName, revision).
		JSONPath("{range .items[*]}{.spec.replicas} {end}").RunQuiet()
	if err != nil {
		return 0
	}
	return sumInts(output)
}

// GetRevision returns the revision label of the first LWS for a deployment.
func GetRevision(deploymentName string) string {
	output, err := LWS(deploymentName).
		JSONPath("{.items[0].metadata.labels.disaggregatedset\\.x-k8s\\.io/revision}").RunQuiet()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(output)
}

// --- Utility Functions ---

// GetNonEmptyLines splits output into non-empty lines.
func GetNonEmptyLines(output string) []string {
	var result []string
	for _, line := range strings.Split(output, "\n") {
		if strings.TrimSpace(line) != "" {
			result = append(result, line)
		}
	}
	return result
}

// sumInts parses space-separated integers and returns their sum.
func sumInts(output string) int {
	total := 0
	for _, s := range strings.Fields(strings.TrimSpace(output)) {
		if n, err := strconv.Atoi(s); err == nil {
			total += n
		}
	}
	return total
}

// --- Cleanup Helpers ---

// CleanupDeployment removes a DisaggregatedSet and all related resources.
func CleanupDeployment(deploymentName string) {
	_, _ = Delete("disaggregatedset", deploymentName).Namespace(defaultNS).IgnoreNotFound().Timeout("30s").RunQuiet()
	_, _ = Delete("lws").Label(labelName, deploymentName).Namespace(defaultNS).IgnoreNotFound().Timeout("30s").RunQuiet()
	_, _ = Delete("pods").Label(labelName, deploymentName).Namespace(defaultNS).IgnoreNotFound().GracePeriod(0).Force().RunQuiet()
	_, _ = Delete("svc").Label(labelName, deploymentName).Namespace(defaultNS).IgnoreNotFound().RunQuiet()
}

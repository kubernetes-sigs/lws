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
	"time"

	. "github.com/onsi/gomega" //nolint:revive,staticcheck
)

// Default timeouts
const (
	DefaultTimeout  = 2 * time.Minute
	DefaultInterval = time.Second
)

// ForPodCount waits until the deployment has exactly count pods.
func ForPodCount(deploymentName string, count int) {
	Eventually(func(g Gomega) {
		g.Expect(CountPods(deploymentName)).To(Equal(count))
	}, DefaultTimeout, DefaultInterval).Should(Succeed())
}

// ForRunningPodCount waits until the deployment has exactly count running pods.
func ForRunningPodCount(deploymentName string, count int) {
	Eventually(func(g Gomega) {
		g.Expect(CountRunningPods(deploymentName)).To(Equal(count))
	}, DefaultTimeout, DefaultInterval).Should(Succeed())
}

// ForRunningPodCountWithTimeout waits with custom timeout.
func ForRunningPodCountWithTimeout(deploymentName string, count int, timeout time.Duration) {
	Eventually(func(g Gomega) {
		g.Expect(CountRunningPods(deploymentName)).To(Equal(count))
	}, timeout, DefaultInterval).Should(Succeed())
}

// ForLWSCount waits until the deployment has exactly count LWS resources.
func ForLWSCount(deploymentName string, count int) {
	Eventually(func(g Gomega) {
		g.Expect(CountLWS(deploymentName)).To(Equal(count))
	}, DefaultTimeout, DefaultInterval).Should(Succeed())
}

// ForRevisionDrained waits until all LWS of the given revision have 0 replicas.
func ForRevisionDrained(deploymentName, revision string) {
	Eventually(func(g Gomega) {
		g.Expect(GetTotalReplicas(deploymentName, revision)).To(Equal(0))
	}, 4*time.Minute, 2*time.Second).Should(Succeed())
}

// ForSingleActiveRevision waits until oldRevision is fully drained and
// exactly one other revision has every role at replicas > 0.
// Passing oldRevision closes the "apply but not yet reconciled" window
// where the old revision would otherwise satisfy a revision-agnostic check
// before the rollout begins (kubernetes-sigs/lws#826).
func ForSingleActiveRevision(deploymentName, oldRevision string) {
	Eventually(func(g Gomega) {
		output, err := LWS(deploymentName).
			JSONPath(`{range .items[*]}{.metadata.labels.disaggregatedset\.x-k8s\.io/revision} {.spec.replicas}{"\n"}{end}`).
			RunQuiet()
		g.Expect(err).NotTo(HaveOccurred())

		g.Expect(evalSingleActiveRevision(output, oldRevision)).To(BeEmpty())
	}, 4*time.Minute, 2*time.Second).Should(Succeed())
}

// evalSingleActiveRevision is the pure-data predicate behind
// ForSingleActiveRevision, extracted for unit testing.
func evalSingleActiveRevision(output, oldRevision string) string {
	revReplicas := make(map[string]int)
	revZeroRoles := make(map[string]int)
	for _, line := range GetNonEmptyLines(output) {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		n, err := strconv.Atoi(fields[1])
		if err != nil {
			continue
		}
		revReplicas[fields[0]] += n
		if n == 0 {
			revZeroRoles[fields[0]]++
		}
	}

	if total, ok := revReplicas[oldRevision]; ok && total > 0 {
		return "old revision " + oldRevision + " not drained yet: total replicas=" + strconv.Itoa(total)
	}

	active := 0
	for rev, total := range revReplicas {
		if total > 0 {
			if revZeroRoles[rev] != 0 {
				return "revision " + rev + " mid-rollout: some roles still at 0 replicas"
			}
			active++
		}
	}
	if active != 1 {
		return "expected exactly one active revision, got " + strconv.Itoa(active)
	}
	return ""
}

// ForRoleReplicas waits until a role has the expected replica count.
// If replicas=0 and the LWS doesn't exist, that counts as 0 replicas.
func ForRoleReplicas(deploymentName, role string, replicas int) {
	Eventually(func(g Gomega) {
		output, err := LWSByRole(deploymentName, role).
			JSONPath("{.items[*].spec.replicas}").RunQuiet()
		if err != nil {
			// If error and expecting 0 replicas, LWS may not exist which is fine
			if replicas == 0 {
				return
			}
			g.Expect(err).NotTo(HaveOccurred())
		}
		// Empty output means no LWS found
		if strings.TrimSpace(output) == "" {
			g.Expect(replicas).To(Equal(0), "Role LWS not found but expected %d replicas", replicas)
			return
		}
		g.Expect(sumInts(output)).To(Equal(replicas))
	}, DefaultTimeout, DefaultInterval).Should(Succeed())
}

// ProgressiveRolloutResult holds the result of tracking a progressive rollout.
type ProgressiveRolloutResult struct {
	SawIntermediateOld bool
	SawIntermediateNew bool
}

// TrackProgressiveRollout tracks rollout states and returns whether intermediate states were observed.
// Parameters: startOld/startNew are initial replica counts, targetNew is final new replica count.
func TrackProgressiveRollout(deploymentName, oldRevision string, startOld, targetNew int) ProgressiveRolloutResult {
	result := ProgressiveRolloutResult{}
	startTime := time.Now()

	for time.Since(startTime) < 4*time.Minute {
		oldTotal := GetTotalReplicas(deploymentName, oldRevision)
		newTotal := GetTotalReplicasNotRevision(deploymentName, oldRevision)

		// Track intermediate states
		if oldTotal > 0 && oldTotal < startOld {
			result.SawIntermediateOld = true
		}
		if newTotal > 0 && newTotal < targetNew {
			result.SawIntermediateNew = true
		}

		// Check if rollout is complete
		if oldTotal == 0 && newTotal == targetNew {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	return result
}

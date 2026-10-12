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
	"os/exec"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/util/intstr"
	utils "sigs.k8s.io/lws/test/testutils/disaggregatedset"
	"sigs.k8s.io/lws/test/testutils/disaggregatedset/fixtures"
	"sigs.k8s.io/lws/test/testutils/disaggregatedset/kubectl"
)

var _ = Describe("Scaling During a Rolling Update", func() {
	DescribeTable("pipelines target changes before the previous batch is Ready", func(identity string) {
		name := "moving-target-" + strings.ToLower(identity)
		DeferCleanup(kubectl.CleanupDeployment, name)
		manifest := func(replicas int, updated bool) string {
			role := fixtures.Role{Replicas: replicas, GroupIdentity: identity,
				HasRollout: true, MaxSurge: intstr.FromString("25%"), MaxUnavailable: intstr.FromString("25%"),
				HoldReadiness: updated}
			if updated {
				role.Labels = map[string]string{"scaling-test-version": "target"}
			}
			config := fixtures.PrefillDecode(name, role, role)
			config.ScalingDuringRolloutPolicy = "AdvanceRollout"
			return config.YAML()
		}
		By("stabilizing the source revision")
		Expect(applyYAML(manifest(8, false))).To(Succeed())
		kubectl.ForRunningPodCountWithTimeout(name, 16, 3*time.Minute)
		oldRevision, err := kubectl.GetRevision(name)
		Expect(err).NotTo(HaveOccurred())

		By("holding every target Pod unready, independently of wall-clock timing")
		Expect(applyYAML(manifest(8, true))).To(Succeed())
		Eventually(func(g Gomega) {
			obs, err := getCurrentRolloutObservation(name, oldRevision)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(obs.Spec.NewPrefill).To(Equal(4)) // The full pending allowance, not a transient first batch.
			g.Expect(obs.Spec.NewDecode).To(Equal(4))
			g.Expect(obs.NewReadyPrefill + obs.NewReadyDecode).To(BeZero())
		}, 3*time.Minute, time.Second).Should(Succeed())
		targetRevisions := func() (string, error) {
			return kubectl.LWS(name).LabelNot("disaggregatedset.x-k8s.io/revision", oldRevision).
				JSONPath(`{range .items[*]}{.metadata.labels.disaggregatedset\.x-k8s\.io/revision}{"\n"}{end}`).RunQuiet()
		}
		targetRevision, err := targetRevisions()
		Expect(err).NotTo(HaveOccurred())
		Expect(strings.Fields(targetRevision)).To(HaveLen(2))
		Expect(strings.Fields(targetRevision)[0]).To(Equal(strings.Fields(targetRevision)[1]))

		By("raising the target and issuing more groups while all four previous groups remain unready")
		Expect(applyYAML(manifest(12, true))).To(Succeed())
		var raised rolloutObservation
		Eventually(func(g Gomega) {
			raised, err = getCurrentRolloutObservation(name, oldRevision)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(raised.Spec.NewPrefill).To(Equal(6))
			g.Expect(raised.Spec.NewDecode).To(Equal(6))
			g.Expect(raised.NewReadyPrefill + raised.NewReadyDecode).To(BeZero())
			g.Expect(raised.Spec.OldPrefill).To(BeNumerically(">", 0))
		}, 3*time.Minute, time.Second).Should(Succeed())
		sameRevision, err := targetRevisions()
		Expect(err).NotTo(HaveOccurred())
		Expect(sameRevision).To(Equal(targetRevision), "replica edits must not create another revision")

		By("lowering the target while the rollout is still held, draining old capacity first")
		Expect(applyYAML(manifest(5, true))).To(Succeed())
		Eventually(func(g Gomega) {
			obs, err := getCurrentRolloutObservation(name, oldRevision)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(obs.Spec.OldPrefill).To(BeNumerically("<", raised.Spec.OldPrefill))
			g.Expect(obs.Spec.OldPrefill).To(BeNumerically(">", 0))
			g.Expect(obs.NewReadyPrefill + obs.NewReadyDecode).To(BeZero())
			g.Expect(obs.OldReadyPrefill).To(BeNumerically(">=", 4)) // floor(25% of 5)=1 unavailable.
		}, 3*time.Minute, time.Second).Should(Succeed())

		By("releasing readiness, including later batches, and reaching the exact latest target")
		Eventually(func(g Gomega) {
			pods, err := kubectl.Get("pods").Label("disaggregatedset.x-k8s.io/name", name).
				Label("scaling-test-version", "target").FieldSelector("status.phase=Running").Output("name").RunQuiet()
			g.Expect(err).NotTo(HaveOccurred())
			for _, pod := range strings.Fields(pods) {
				// A terminating group may disappear between list and exec. Retrying
				// the final state handles that race without changing the template.
				_, _ = utils.Run(exec.Command("kubectl", "exec", pod, "--", "touch", "/tmp/ready"))
			}
			obs, err := getCurrentRolloutObservation(name, oldRevision)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(obs.Spec).To(Equal(rolloutState{NewPrefill: 5, NewDecode: 5}))
			g.Expect(obs.NewReadyPrefill).To(Equal(5))
			g.Expect(obs.NewReadyDecode).To(Equal(5))
		}, 5*time.Minute, time.Second).Should(Succeed())
	}, Entry("Ordinal", "Ordinal"), Entry("Hash", "Hash"))
})

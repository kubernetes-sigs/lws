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
	DescribeTable("applies scale-up and scale-down policies independently before the previous batch is Ready", func(identity, up, down string) {
		// Leave room for generated LWS/Pod suffixes within the DNS name limit.
		name := "moving-target-" + strings.ToLower(identity+"-"+up[:1]+down[:1])
		DeferCleanup(kubectl.CleanupDeployment, name)
		manifest := func(replicas int, updated bool) string {
			version := "source"
			if updated {
				version = "target"
			}
			role := fixtures.Role{Replicas: replicas, GroupIdentity: identity,
				HasRollout: true, MaxSurge: intstr.FromString("25%"), MaxUnavailable: intstr.FromString("25%"),
				HoldReadiness: true, Labels: map[string]string{"scaling-test-version": version}}
			config := fixtures.PrefillDecode(name, role, role)
			config.RolloutScaleUpPolicy, config.RolloutScaleDownPolicy = up, down
			return config.YAML()
		}
		setReadiness := func(version string, ready bool) error {
			for _, role := range []string{"prefill", "decode"} {
				pods, err := kubectl.Get("pods").Label("disaggregatedset.x-k8s.io/name", name).
					Label("scaling-test-version", version).Label("disaggregatedset.x-k8s.io/role", role).
					FieldSelector("status.phase=Running").Output("name").RunQuiet()
				if err != nil {
					return err
				}
				for _, pod := range strings.Fields(pods) {
					command := []string{"touch", "/tmp/ready"}
					if !ready {
						command = []string{"rm", "-f", "/tmp/ready"}
					}
					// A terminating group may disappear between list and exec. The
					// caller retries until observed readiness reaches its goal.
					_, _ = utils.Run(exec.Command("kubectl", append([]string{"exec", pod, "--"}, command...)...))
					if !ready {
						break // Lose one group's readiness, preserving a healthy old revision.
					}
				}
			}
			return nil
		}
		By("stabilizing the source revision")
		Expect(applyYAML(manifest(8, false))).To(Succeed())
		kubectl.ForRunningPodCountWithTimeout(name, 16, 3*time.Minute)
		oldRevision, err := kubectl.GetRevision(name)
		Expect(err).NotTo(HaveOccurred())
		Eventually(func(g Gomega) {
			g.Expect(setReadiness("source", true)).To(Succeed())
			obs, err := getCurrentRolloutObservation(name, oldRevision)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(obs.OldReadyPrefill).To(Equal(8))
			g.Expect(obs.OldReadyDecode).To(Equal(8))
		}, 3*time.Minute, time.Second).Should(Succeed())

		By("holding every target Pod unready, independently of wall-clock timing")
		Expect(applyYAML(manifest(8, true))).To(Succeed())
		Eventually(func(g Gomega) {
			obs, err := getCurrentRolloutObservation(name, oldRevision)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(obs.Spec.NewPrefill).To(Equal(4)) // The full pending allowance, not a transient first batch.
			g.Expect(obs.Spec.NewDecode).To(Equal(4))
			g.Expect(obs.Spec.OldPrefill).To(Equal(6))
			g.Expect(obs.Spec.OldDecode).To(Equal(6))
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

		By("raising desired 8 to 16: Deferred allows 8 pending groups; Immediate fills the 10 missing requests")
		Expect(applyYAML(manifest(16, true))).To(Succeed())
		raisedTarget := 8 // maxSurge + maxUnavailable = 4 + 4, with no Ready target groups.
		if up == "Immediate" {
			raisedTarget = 10 // desired - old Spec = 16 - 6, still below desired + surge.
		}
		var raised rolloutObservation
		Eventually(func(g Gomega) {
			raised, err = getCurrentRolloutObservation(name, oldRevision)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(raised.Spec).To(Equal(rolloutState{OldPrefill: 6, OldDecode: 6, NewPrefill: raisedTarget, NewDecode: raisedTarget}))
			g.Expect(raised.OldReadyPrefill).To(Equal(6))
			g.Expect(raised.OldReadyDecode).To(Equal(6))
			g.Expect(raised.NewReadyPrefill + raised.NewReadyDecode).To(BeZero())
		}, 3*time.Minute, time.Second).Should(Succeed())
		// The later scale-down checks protect scheduled startup work, not
		// wholly unscheduled groups that may safely be cancelled further.
		kubectl.ForRunningPodCountWithTimeout(name, 2*(6+raisedTarget), 3*time.Minute)
		sameRevision, err := targetRevisions()
		Expect(err).NotTo(HaveOccurred())
		Expect(sameRevision).To(Equal(targetRevision), "replica edits must not create another revision")

		By("losing old readiness without changing requested replicas: neither policy issues additional groups")
		Eventually(func(g Gomega) {
			g.Expect(setReadiness("source", false)).To(Succeed())
			obs, err := getCurrentRolloutObservation(name, oldRevision)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(obs.OldReadyPrefill).To(Equal(5))
			g.Expect(obs.OldReadyDecode).To(Equal(5))
		}, time.Minute, time.Second).Should(Succeed())
		Consistently(func(g Gomega) {
			obs, err := getCurrentRolloutObservation(name, oldRevision)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(obs.Spec).To(Equal(raised.Spec))
		}, 10*time.Second, time.Second).Should(Succeed())
		Eventually(func(g Gomega) {
			g.Expect(setReadiness("source", true)).To(Succeed())
			obs, err := getCurrentRolloutObservation(name, oldRevision)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(obs.OldReadyPrefill).To(Equal(6))
			g.Expect(obs.OldReadyDecode).To(Equal(6))
		}, time.Minute, time.Second).Should(Succeed())

		By("lowering the target while the rollout is still held, draining old capacity first")
		Expect(applyYAML(manifest(5, true))).To(Succeed())
		retainedTarget := raisedTarget
		if down == "Immediate" {
			retainedTarget = 5
		}
		Eventually(func(g Gomega) {
			obs, err := getCurrentRolloutObservation(name, oldRevision)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(obs.Spec).To(Equal(rolloutState{OldPrefill: 4, OldDecode: 4, NewPrefill: retainedTarget, NewDecode: retainedTarget}))
			g.Expect(obs.NewReadyPrefill + obs.NewReadyDecode).To(BeZero())
			g.Expect(obs.OldReadyPrefill).To(Equal(4)) // floor(25% of 5)=1 unavailable.
			g.Expect(obs.OldReadyDecode).To(Equal(4))
		}, 3*time.Minute, time.Second).Should(Succeed())
		sameRevision, err = targetRevisions()
		Expect(err).NotTo(HaveOccurred())
		Expect(sameRevision).To(Equal(targetRevision), "scale-down must keep the same target revision too")

		By("releasing readiness, including later batches, and reaching the exact latest target")
		Eventually(func(g Gomega) {
			g.Expect(setReadiness("target", true)).To(Succeed())
			obs, err := getCurrentRolloutObservation(name, oldRevision)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(obs.Spec).To(Equal(rolloutState{NewPrefill: 5, NewDecode: 5}))
			g.Expect(obs.NewReadyPrefill).To(Equal(5))
			g.Expect(obs.NewReadyDecode).To(Equal(5))
		}, 5*time.Minute, time.Second).Should(Succeed())
	},
		Entry("Ordinal / up Deferred / down Deferred", "Ordinal", "Deferred", "Deferred"),
		Entry("Ordinal / up Deferred / down Immediate", "Ordinal", "Deferred", "Immediate"),
		Entry("Ordinal / up Immediate / down Deferred", "Ordinal", "Immediate", "Deferred"),
		Entry("Ordinal / up Immediate / down Immediate", "Ordinal", "Immediate", "Immediate"),
		Entry("Hash / up Deferred / down Deferred", "Hash", "Deferred", "Deferred"),
		Entry("Hash / up Deferred / down Immediate", "Hash", "Deferred", "Immediate"),
		Entry("Hash / up Immediate / down Deferred", "Hash", "Immediate", "Deferred"),
		Entry("Hash / up Immediate / down Immediate", "Hash", "Immediate", "Immediate"),
	)
})

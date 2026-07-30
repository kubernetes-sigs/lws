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
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	utils "sigs.k8s.io/lws/test/testutils/disaggregatedset"
	"sigs.k8s.io/lws/test/testutils/disaggregatedset/fixtures"
	"sigs.k8s.io/lws/test/testutils/disaggregatedset/kubectl"
)

// KEP-849 per-role external scaling: the DS controller auto-creates a
// DisaggregatedSetRoleScaler for every role with scaling.mode: External and
// exposes /scale on it. These tests guard the invariants that unit tests can't
// reach because they need a real apiserver in the loop:
//   - GET /scale on a freshly auto-created scaler MUST return 200 (yankay/lws#15:
//     HPA reads /scale before writing; a 500 here deadlocks the HPA loop).
//   - kubectl scale on the scaler propagates through to the underlying LWS.
//   - Deleting the parent DS cascades and removes the scaler via the ownerRef.
var _ = Describe("DisaggregatedSet HPA External Scaling", Ordered, func() {
	SetDefaultEventuallyTimeout(2 * time.Minute)
	SetDefaultEventuallyPollingInterval(time.Second)

	const dsName = "test-hpa"
	const scalerName = "test-hpa-prefill"

	AfterEach(func() {
		By("cleaning up the DisaggregatedSet")
		kubectl.CleanupDeployment(dsName)
	})

	It("auto-creates a scaler and GET /scale succeeds on it before any write", func() {
		By("creating a DisaggregatedSet with an External prefill role")
		yaml := fixtures.PrefillDecode(dsName,
			fixtures.Role{External: true},
			fixtures.Role{Replicas: 1},
		).YAML()
		Expect(applyYAML(yaml)).To(Succeed())

		By("waiting for the controller to auto-create the scaler")
		Eventually(func(g Gomega) {
			_, err := utils.Run(exec.Command("kubectl", "get", "dsrs", scalerName, "-o", "name"))
			g.Expect(err).NotTo(HaveOccurred())
		}).Should(Succeed())

		By("verifying the scaler carries the parent DS as controller ownerRef")
		out, err := utils.Run(exec.Command("kubectl", "get", "dsrs", scalerName,
			"-o", `jsonpath={.metadata.ownerReferences[?(@.controller==true)].name}`))
		Expect(err).NotTo(HaveOccurred())
		Expect(strings.TrimSpace(out)).To(Equal(dsName))

		By("calling GET /scale on the fresh scaler — this is the regression guard for yankay/lws#15")
		// The apiserver's CRD /scale handler extracts spec.replicas at read time.
		// A pristine scaler MUST expose a materialised replicas value so HPA/KEDA
		// can perform their bootstrap read before their first write. If this ever
		// returns "the spec replicas field does not exist", HPA deadlocks in
		// AbleToScale=False / FailedGetScale.
		scaleURL := fmt.Sprintf(
			"/apis/disaggregatedset.x-k8s.io/v1/namespaces/default/disaggregatedsetrolescalers/%s/scale",
			scalerName)
		Eventually(func(g Gomega) {
			raw, err := utils.Run(exec.Command("kubectl", "get", "--raw", scaleURL))
			g.Expect(err).NotTo(HaveOccurred(),
				"GET /scale on a freshly auto-created scaler must succeed; a 500 here deadlocks HPA")
			g.Expect(raw).To(ContainSubstring(`"kind":"Scale"`))
			g.Expect(raw).To(ContainSubstring(`"spec"`))
		}).Should(Succeed())
	})

	It("kubectl scale on the scaler propagates spec.replicas to the LWS", func() {
		By("creating a DisaggregatedSet with an External prefill role")
		yaml := fixtures.PrefillDecode(dsName,
			fixtures.Role{External: true},
			fixtures.Role{Replicas: 1},
		).YAML()
		Expect(applyYAML(yaml)).To(Succeed())

		By("waiting for the scaler to exist")
		Eventually(func(g Gomega) {
			_, err := utils.Run(exec.Command("kubectl", "get", "dsrs", scalerName, "-o", "name"))
			g.Expect(err).NotTo(HaveOccurred())
		}).Should(Succeed())

		By("writing spec.replicas=3 via the /scale subresource (same call HPA/KEDA use)")
		_, err := utils.Run(exec.Command("kubectl", "scale",
			"disaggregatedsetrolescaler/"+scalerName, "--replicas=3"))
		Expect(err).NotTo(HaveOccurred())

		By("verifying the LWS for prefill scales to 3")
		Eventually(func(g Gomega) {
			g.Expect(kubectl.GetTotalReplicas(dsName, kubectl.GetRevision(dsName))).To(BeNumerically(">=", 3))
		}).Should(Succeed())
	})

	It("cascades: deleting the DS removes the auto-created scaler via ownerRef", func() {
		By("creating a DisaggregatedSet with an External prefill role")
		yaml := fixtures.PrefillDecode(dsName,
			fixtures.Role{External: true},
			fixtures.Role{Replicas: 1},
		).YAML()
		Expect(applyYAML(yaml)).To(Succeed())

		By("waiting for the scaler to exist")
		Eventually(func(g Gomega) {
			_, err := utils.Run(exec.Command("kubectl", "get", "dsrs", scalerName, "-o", "name"))
			g.Expect(err).NotTo(HaveOccurred())
		}).Should(Succeed())

		By("deleting the DisaggregatedSet")
		_, err := kubectl.Delete("disaggregatedset", dsName).Run()
		Expect(err).NotTo(HaveOccurred())

		By("verifying the scaler is garbage-collected")
		Eventually(func(g Gomega) {
			out, err := utils.Run(exec.Command("kubectl", "get", "dsrs", scalerName,
				"--ignore-not-found", "-o", "name"))
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(strings.TrimSpace(out)).To(BeEmpty())
		}).Should(Succeed())
	})

	// KEP-948: the scaler value is an aggregate across slices, split by
	// distribute() with the remainder on the lowest slice indices. Slice-count
	// changes rebalance the same total rather than multiplying it.
	It("distributes an aggregate /scale write across slices and rebalances on slice-count changes", func() {
		By("creating a 2-slice DisaggregatedSet with an External prefill role")
		cfg := fixtures.PrefillDecode(dsName,
			fixtures.Role{External: true},
			fixtures.Role{Replicas: 1},
		)
		cfg.Slices = 2
		Expect(applyYAML(cfg.YAML())).To(Succeed())

		By("waiting for the scaler, seeded to the slice count (one group per slice)")
		Eventually(func(g Gomega) {
			out, err := utils.Run(exec.Command("kubectl", "get", "dsrs", scalerName,
				"-o", "jsonpath={.spec.replicas}"))
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(strings.TrimSpace(out)).To(Equal("2"))
		}).Should(Succeed())
		Eventually(func(g Gomega) {
			g.Expect(kubectl.GetRoleReplicasBySlice(dsName, "prefill", 0)).To(Equal(1))
			g.Expect(kubectl.GetRoleReplicasBySlice(dsName, "prefill", 1)).To(Equal(1))
		}).Should(Succeed())

		By("writing an aggregate of 5 via /scale and expecting a 3/2 split, remainder on slice 0")
		_, err := utils.Run(exec.Command("kubectl", "scale",
			"disaggregatedsetrolescaler/"+scalerName, "--replicas=5"))
		Expect(err).NotTo(HaveOccurred())
		Eventually(func(g Gomega) {
			g.Expect(kubectl.GetRoleReplicasBySlice(dsName, "prefill", 0)).To(Equal(3))
			g.Expect(kubectl.GetRoleReplicasBySlice(dsName, "prefill", 1)).To(Equal(2))
		}).Should(Succeed())

		By("verifying scaler status converges to the aggregate")
		Eventually(func(g Gomega) {
			out, err := utils.Run(exec.Command("kubectl", "get", "dsrs", scalerName,
				"-o", "jsonpath={.status.replicas}"))
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(strings.TrimSpace(out)).To(Equal("5"))
		}).Should(Succeed())

		By("raising slices to 3 and expecting the same total rebalanced to 2/2/1")
		_, err = utils.Run(exec.Command("kubectl", "patch", "disaggregatedset", dsName,
			"--type=merge", "-p", `{"spec":{"slices":3}}`))
		Expect(err).NotTo(HaveOccurred())
		Eventually(func(g Gomega) {
			g.Expect(kubectl.GetRoleReplicasBySlice(dsName, "prefill", 0)).To(Equal(2))
			g.Expect(kubectl.GetRoleReplicasBySlice(dsName, "prefill", 1)).To(Equal(2))
			g.Expect(kubectl.GetRoleReplicasBySlice(dsName, "prefill", 2)).To(Equal(1))
		}).Should(Succeed())

		By("dropping slices back to 2 and expecting the remaining slices to absorb the total")
		_, err = utils.Run(exec.Command("kubectl", "patch", "disaggregatedset", dsName,
			"--type=merge", "-p", `{"spec":{"slices":2}}`))
		Expect(err).NotTo(HaveOccurred())
		Eventually(func(g Gomega) {
			g.Expect(kubectl.GetRoleReplicasBySlice(dsName, "prefill", 0)).To(Equal(3))
			g.Expect(kubectl.GetRoleReplicasBySlice(dsName, "prefill", 1)).To(Equal(2))
			g.Expect(kubectl.GetRoleReplicasBySlice(dsName, "prefill", 2)).To(Equal(0))
		}).Should(Succeed())
	})
})

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
	"slices"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	utils "sigs.k8s.io/lws/test/testutils/disaggregatedset"
	"sigs.k8s.io/lws/test/testutils/disaggregatedset/fixtures"
	"sigs.k8s.io/lws/test/testutils/disaggregatedset/kubectl"
)

const subRoleLabel = "disaggregatedset.x-k8s.io/subrole"

var _ = Describe("DisaggregatedSet Sub-Roles", Ordered, func() {
	SetDefaultEventuallyTimeout(3 * time.Minute)
	SetDefaultEventuallyPollingInterval(time.Second)

	Context("Static assignment and rollout", func() {
		const dsName = "test-subroles"

		AfterEach(func() { kubectl.CleanupDeployment(dsName) })

		It("shares one LWS, relabels in place, and preserves the split across rollout", func() {
			initial := staticSubRoleFixture(dsName, "registry.k8s.io/pause:3.9", 2, 1)
			Expect(applyYAML(initial.YAML())).To(Succeed())

			By("creating one physical LWS with the summed target")
			Eventually(func(g Gomega) {
				g.Expect(kubectl.CountLWSByRole(dsName, "decode")).To(Equal(1))
				g.Expect(roleReplicas(dsName, "decode")).To(Equal("3"))
				g.Expect(subRolePodCount(dsName, "short")).To(Equal(2))
				g.Expect(subRolePodCount(dsName, "long")).To(Equal(1))
				g.Expect(kubectl.CountService(dsName)).To(Equal(3))
			}).Should(Succeed())
			beforeUIDs := podUIDs(dsName)
			oldRevision := kubectl.GetRevision(dsName)

			By("changing only sub-role targets without changing revision or Pods")
			rebalanced := staticSubRoleFixture(dsName, "registry.k8s.io/pause:3.9", 1, 2)
			Expect(applyYAML(rebalanced.YAML())).To(Succeed())
			Eventually(func(g Gomega) {
				g.Expect(subRolePodCount(dsName, "short")).To(Equal(1))
				g.Expect(subRolePodCount(dsName, "long")).To(Equal(2))
			}).Should(Succeed())
			Expect(kubectl.GetRevision(dsName)).To(Equal(oldRevision))
			Expect(podUIDs(dsName)).To(Equal(beforeUIDs))

			By("rolling the shared template while retaining the logical split")
			updated := staticSubRoleFixture(dsName, "registry.k8s.io/pause:3.10", 1, 2)
			Expect(applyYAML(updated.YAML())).To(Succeed())
			kubectl.ForSingleActiveRevision(dsName, oldRevision)
			Eventually(func(g Gomega) {
				g.Expect(subRolePodCount(dsName, "short")).To(Equal(1))
				g.Expect(subRolePodCount(dsName, "long")).To(Equal(2))
			}).Should(Succeed())
		})
	})

	Context("External scaling", func() {
		const dsName = "test-subrole-hpa"
		const scalerName = "test-subrole-hpa-decode-short"

		AfterEach(func() { kubectl.CleanupDeployment(dsName) })

		It("exposes and applies a scaler for one sub-role", func() {
			fixture := fixtures.Config{Name: dsName, Roles: []fixtures.Role{{
				Name: "decode",
				SubRoles: []fixtures.SubRole{
					{Name: "short", External: true},
					{Name: "long", Replicas: fixtures.Ptr(2)},
				},
			}}}
			Expect(applyYAML(fixture.YAML())).To(Succeed())

			Eventually(func(g Gomega) {
				out, err := utils.Run(exec.Command("kubectl", "get", "dsrs", scalerName,
					"-o", "jsonpath={.status.selector}"))
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(out).To(ContainSubstring(subRoleLabel + "=short"))
			}).Should(Succeed())

			_, err := utils.Run(exec.Command("kubectl", "scale", "disaggregatedsetrolescaler/"+scalerName, "--replicas=3"))
			Expect(err).NotTo(HaveOccurred())
			Eventually(func(g Gomega) {
				g.Expect(roleReplicas(dsName, "decode")).To(Equal("5"))
				g.Expect(subRolePodCount(dsName, "short")).To(Equal(3))
				g.Expect(subRolePodCount(dsName, "long")).To(Equal(2))
			}).Should(Succeed())
		})
	})
})

func staticSubRoleFixture(name, image string, short, long int) fixtures.Config {
	return fixtures.Config{Name: name, Roles: []fixtures.Role{{
		Name:  "decode",
		Image: image,
		SubRoles: []fixtures.SubRole{
			{Name: "short", Replicas: fixtures.Ptr(short)},
			{Name: "long", Replicas: fixtures.Ptr(long)},
		},
	}}}
}

func subRolePodCount(dsName, subRole string) int {
	out, err := kubectl.Pods(dsName).Label(subRoleLabel, subRole).Output("name").RunQuiet()
	if err != nil {
		return 0
	}
	return len(kubectl.GetNonEmptyLines(out))
}

func roleReplicas(dsName, role string) string {
	out, err := kubectl.LWSByRole(dsName, role).JSONPath("{.items[0].spec.replicas}").RunQuiet()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

func podUIDs(dsName string) []string {
	out, err := kubectl.Pods(dsName).JSONPath(`{range .items[*]}{.metadata.uid}{"\n"}{end}`).RunQuiet()
	if err != nil {
		return nil
	}
	uids := strings.Fields(out)
	slices.Sort(uids)
	return uids
}

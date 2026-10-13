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
	"encoding/json"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
	disaggv1 "sigs.k8s.io/lws/api/disaggregatedset/v1"
	leaderv1 "sigs.k8s.io/lws/api/leaderworkerset/v1"
	"sigs.k8s.io/lws/test/testutils/disaggregatedset/fixtures"
	"sigs.k8s.io/lws/test/testutils/disaggregatedset/kubectl"
	"sigs.k8s.io/yaml"
)

var _ = Describe("Ordinal subroles", func() {
	It("scales and rolls out two parents while retaining coherent group routing", func() {
		const name = "ordinal-subroles"
		DeferCleanup(kubectl.CleanupDeployment, name)
		role := fixtures.Role{Replicas: 1, HasRollout: true, MaxSurge: intstr.FromInt(1), MaxUnavailable: intstr.FromInt(1)}
		var ds disaggv1.DisaggregatedSet
		Expect(yaml.Unmarshal([]byte(fixtures.PrefillDecode(name, role, role).YAML()), &ds)).To(Succeed())
		for _, phase := range []struct {
			name, version string
			counts        [2][2]int32
		}{
			{"create", "old", [2][2]int32{{1, 1}, {1, 1}}},
			{"scale each parent independently", "old", [2][2]int32{{2, 1}, {1, 2}}},
			{"roll out both parents", "new", [2][2]int32{{2, 1}, {1, 2}}},
			{"shrink both parents", "new", [2][2]int32{{1, 1}, {1, 1}}},
		} {
			By(phase.name)
			want := map[string]int{}
			for i := range ds.Spec.Roles {
				r := &ds.Spec.Roles[i]
				r.SubRoles = nil
				for j, child := range []string{"a", "b"} {
					r.SubRoles = append(r.SubRoles, disaggv1.DisaggregatedSubRoleSpec{Name: child, Replicas: ptr.To(phase.counts[i][j])})
					want[r.Name+"/"+child] = int(phase.counts[i][j])
				}
				r.Spec.LeaderWorkerTemplate.Size = ptr.To[int32](2)
				r.Spec.StartupPolicy = leaderv1.LeaderCreatedStartupPolicy
				r.Spec.RolloutStrategy.Type = leaderv1.RollingUpdateStrategyType
				r.Spec.LeaderWorkerTemplate.WorkerTemplate.Labels = map[string]string{"subrole-test-version": phase.version}
				r.Spec.LeaderWorkerTemplate.WorkerTemplate.Spec.Containers[0].Image = "busybox:1.36"
				r.Spec.LeaderWorkerTemplate.WorkerTemplate.Spec.Containers[0].Command = []string{"sleep", "3600"}
			}
			manifest, err := json.Marshal(ds)
			Expect(err).NotTo(HaveOccurred())
			Expect(applyYAML(string(manifest))).To(Succeed())
			Eventually(func(g Gomega) {
				output, err := kubectl.LWS(name).Output("json").RunQuiet()
				g.Expect(err).NotTo(HaveOccurred())
				var workloads leaderv1.LeaderWorkerSetList
				g.Expect(json.Unmarshal([]byte(output), &workloads)).To(Succeed())
				g.Expect(workloads.Items).To(HaveLen(2), "one physical LWS per parent after old revisions drain")
				for _, lws := range workloads.Items {
					parent := lws.Labels[disaggv1.RoleLabelKey]
					g.Expect(int(lws.Status.ReadyReplicas)).To(Equal(want[parent+"/a"] + want[parent+"/b"]))
				}
				output, err = kubectl.Pods(name).Output("json").RunQuiet()
				g.Expect(err).NotTo(HaveOccurred())
				var pods corev1.PodList
				g.Expect(json.Unmarshal([]byte(output), &pods)).To(Succeed())
				counts, routing, members := map[string]int{}, map[string]string{}, map[string]int{}
				for _, pod := range pods.Items {
					if !pod.DeletionTimestamp.IsZero() {
						continue
					}
					parent, child := pod.Labels[disaggv1.RoleLabelKey], pod.Labels[disaggv1.SubRoleLabelKey]
					g.Expect(want).To(HaveKey(parent + "/" + child))
					g.Expect(pod.Labels["subrole-test-version"]).To(Equal(phase.version))
					group := pod.Labels[leaderv1.SetNameLabelKey] + "/" + pod.Labels[leaderv1.GroupIndexLabelKey]
					if assigned, exists := routing[group]; exists {
						g.Expect(child).To(Equal(assigned), "leader and worker must have the same subrole")
					}
					routing[group], members[group] = child, members[group]+1
					if pod.Labels[leaderv1.WorkerIndexLabelKey] == "0" {
						counts[parent+"/"+child]++
					}
				}
				g.Expect(counts).To(Equal(want))
				for _, count := range members {
					g.Expect(count).To(Equal(2))
				}
				output, err = kubectl.Get("disaggregatedset/" + name).Output("json").RunQuiet()
				g.Expect(err).NotTo(HaveOccurred())
				var live disaggv1.DisaggregatedSet
				g.Expect(json.Unmarshal([]byte(output), &live)).To(Succeed())
				g.Expect(meta.IsStatusConditionTrue(live.Status.Conditions, string(disaggv1.DisaggregatedSetSubRolesAssigned))).To(BeTrue())
				g.Expect(live.Status.RoleStatuses).To(HaveLen(2))
				for _, parent := range live.Status.RoleStatuses {
					g.Expect(parent.SubRoleStatuses).To(HaveLen(2))
					for _, child := range parent.SubRoleStatuses {
						count := int32(want[parent.Name+"/"+child.Name])
						g.Expect(child).To(Equal(disaggv1.SubRoleStatus{Name: child.Name, Replicas: count, ReadyReplicas: count, UpdatedReplicas: count}))
					}
				}
			}, 3*time.Minute, time.Second).Should(Succeed())
		}
	})
})

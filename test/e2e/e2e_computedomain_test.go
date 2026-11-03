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
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	resourcev1 "k8s.io/api/resource/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/util/retry"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	dsv1 "sigs.k8s.io/lws/api/disaggregatedset/v1"
	lwsv1 "sigs.k8s.io/lws/api/leaderworkerset/v1"
	"sigs.k8s.io/lws/pkg/computedomain"
	podutils "sigs.k8s.io/lws/pkg/utils/pod"
	"sigs.k8s.io/lws/test/wrappers"
)

const (
	fakeGPUNodePoolLabel          = "run.ai/simulated-gpu-node-pool"
	computeDomainVersionLabel     = "computedomain.e2e.lws.x-k8s.io/version"
	computeDomainTimeout          = 3 * time.Minute
	computeDomainTerminationGrace = computeDomainTimeout + time.Minute
)

type computeDomainFixture struct {
	workload client.Object
	podCount int
}

type computeDomainBinding struct {
	Pod      corev1.Pod
	Domain   computedomain.ComputeDomain
	Template resourcev1.ResourceClaimTemplate
	Claim    resourcev1.ResourceClaim
}

var _ = ginkgo.Describe("AutoComputeDomain", ginkgo.Label("FakeGPU", "AutoComputeDomain"), ginkgo.Serial, func() {
	var ns *corev1.Namespace

	ginkgo.BeforeEach(func() {
		ns = nil
		if os.Getenv("USE_FAKE_GPU_OPERATOR") != "true" {
			ginkgo.Skip("requires USE_FAKE_GPU_OPERATOR=true")
		}
		gomega.Expect(os.Getenv("KUBECONFIG")).NotTo(gomega.BeEmpty(), "use a dedicated kubeconfig")
		gomega.Expect(string(schedulerProvider)).To(gomega.BeEmpty())
		gomega.Expect(computedomain.AddToScheme(k8sClient.Scheme())).To(gomega.Succeed())
		gomega.Expect(dsv1.AddToScheme(k8sClient.Scheme())).To(gomega.Succeed())
		gomega.Expect(resourcev1.AddToScheme(k8sClient.Scheme())).To(gomega.Succeed())
		gomega.Expect(k8sClient.List(ctx, &computedomain.ComputeDomainList{}, client.Limit(1))).To(gomega.Succeed())
		var nodes corev1.NodeList
		gomega.Expect(k8sClient.List(ctx, &nodes, client.MatchingLabels{fakeGPUNodePoolLabel: "default"})).To(gomega.Succeed())
		gomega.Expect(len(nodes.Items)).To(gomega.BeNumerically(">=", 4), "two groups need four dedicated channel devices")

		ns = &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "computedomain-e2e-"}}
		gomega.Expect(k8sClient.Create(ctx, ns)).To(gomega.Succeed())
		ginkgo.DeferCleanup(func() {
			cleanupCtx, cancel := context.WithTimeout(ctx, computeDomainTerminationGrace+time.Minute)
			defer cancel()
			gomega.Expect(k8sClient.Delete(cleanupCtx, ns)).To(gomega.Succeed())
			gomega.Eventually(cleanupCtx, func(g gomega.Gomega) {
				// Include Pods created after a failed assertion; never strip finalizers.
				for _, pod := range listComputeDomainPods(cleanupCtx, ns.Name, "source") {
					if pod.Status.Phase == corev1.PodRunning {
						if err := execComputeDomainPod(cleanupCtx, pod, "touch", "/tmp/release"); err != nil {
							ginkgo.GinkgoWriter.Printf("Releasing %s: %v\n", pod.Name, err)
						}
					}
				}
				expectObjectDeleted(cleanupCtx, g, ns)
			}, computeDomainTerminationGrace+time.Minute, time.Second).Should(gomega.Succeed())
		})
	})

	ginkgo.JustAfterEach(func() {
		if ns == nil || !ginkgo.CurrentSpecReport().Failed() {
			return
		}
		for _, list := range []client.ObjectList{
			&lwsv1.LeaderWorkerSetList{}, &dsv1.DisaggregatedSetList{},
			&corev1.PodList{}, &computedomain.ComputeDomainList{},
			&resourcev1.ResourceClaimTemplateList{}, &resourcev1.ResourceClaimList{}, &corev1.EventList{},
		} {
			if err := k8sClient.List(ctx, list, client.InNamespace(ns.Name)); err != nil {
				ginkgo.GinkgoWriter.Printf("Collecting %T: %v\n", list, err)
				continue
			}
			ginkgo.AddReportEntry(fmt.Sprintf("%T", list), list, ginkgo.ReportEntryVisibilityNever)
		}
	})

	ginkgo.DescribeTable("retains domains after workload deletion until referencing Pods exit",
		func(disaggregated bool, identity lwsv1.GroupIdentityType) {
			ginkgo.By("creating a workload with two groups or slices")
			fixture := createComputeDomainFixture(ns.Name, disaggregated, identity, 2)
			ginkgo.By("waiting for allocated claims and a Ready workload")
			bindings := expectComputeDomainWorkloadReady(fixture)
			ginkgo.By("deleting the workload and checking domain retention and cleanup")
			cleanupCtx, cancel := context.WithTimeout(ctx, computeDomainTimeout)
			defer cancel()
			gomega.Expect(k8sClient.Delete(cleanupCtx, fixture.workload,
				client.Preconditions{UID: ptr.To(fixture.workload.GetUID())},
				client.PropagationPolicy(metav1.DeletePropagationBackground))).To(gomega.Succeed())
			gomega.Eventually(cleanupCtx, func(g gomega.Gomega) {
				expectObjectDeleted(cleanupCtx, g, fixture.workload)
				expectComputeDomainDraining(cleanupCtx, g, bindings)
			}, computeDomainTimeout, time.Second).Should(gomega.Succeed())
			gomega.Consistently(cleanupCtx, func(g gomega.Gomega) {
				expectComputeDomainDraining(cleanupCtx, g, bindings)
			}, 5*time.Second, time.Second).Should(gomega.Succeed())
			releaseComputeDomainPods(cleanupCtx, bindings)
			gomega.Eventually(cleanupCtx, func(g gomega.Gomega) {
				for _, list := range []client.ObjectList{
					&corev1.PodList{}, &computedomain.ComputeDomainList{},
					&resourcev1.ResourceClaimTemplateList{}, &resourcev1.ResourceClaimList{},
				} {
					gomega.Expect(k8sClient.List(cleanupCtx, list, client.InNamespace(ns.Name))).To(gomega.Succeed())
					g.Expect(meta.ExtractList(list)).To(gomega.BeEmpty())
				}
			}, computeDomainTimeout, time.Second).Should(gomega.Succeed())
		},
		ginkgo.Entry("LWS Ordinal", false, lwsv1.GroupIdentityOrdinal),
		ginkgo.Entry("LWS Hash", false, lwsv1.GroupIdentityHash),
		ginkgo.Entry("DisaggregatedSet", true, lwsv1.GroupIdentityOrdinal),
	)

	ginkgo.DescribeTable("cleans up scaled-down domains without replacing the surviving group",
		func(disaggregated bool, identity lwsv1.GroupIdentityType) {
			ginkgo.By("creating a workload with two groups or slices")
			fixture := createComputeDomainFixture(ns.Name, disaggregated, identity, 2)
			ginkgo.By("waiting for allocated claims and a Ready workload")
			before := expectComputeDomainWorkloadReady(fixture)
			ginkgo.By("scaling down and checking surviving bindings and removed resources")
			drainCtx, cancel := context.WithTimeout(ctx, computeDomainTimeout)
			defer cancel()
			gomega.Expect(retry.RetryOnConflict(retry.DefaultRetry, func() error {
				if err := k8sClient.Get(drainCtx, client.ObjectKeyFromObject(fixture.workload), fixture.workload); err != nil {
					return err
				}
				if ds, ok := fixture.workload.(*dsv1.DisaggregatedSet); ok {
					ds.Spec.Slices = ptr.To[int32](1)
				} else {
					fixture.workload.(*lwsv1.LeaderWorkerSet).Spec.Replicas = ptr.To[int32](1)
				}
				return k8sClient.Update(drainCtx, fixture.workload)
			})).To(gomega.Succeed())
			fixture.podCount /= 2
			groupLabel := lwsv1.GroupIndexLabelKey
			if disaggregated {
				groupLabel = dsv1.SliceLabelKey
			}
			var removed, retained []computeDomainBinding
			gomega.Eventually(drainCtx, func(g gomega.Gomega) {
				// Observe the selected group; Hash identities are not numeric ordinals.
				var removedGroup string
				for _, pod := range listComputeDomainPods(drainCtx, ns.Name, "source") {
					if pod.DeletionTimestamp != nil {
						removedGroup = pod.Labels[groupLabel]
						break
					}
				}
				g.Expect(removedGroup).NotTo(gomega.BeEmpty())
				removed, retained = nil, nil
				for _, b := range before {
					if b.Pod.Labels[groupLabel] == removedGroup {
						removed = append(removed, b)
					} else {
						retained = append(retained, b)
					}
				}
				g.Expect(removed).To(gomega.HaveLen(fixture.podCount))
				expectComputeDomainDraining(drainCtx, g, removed)
			}, computeDomainTimeout, time.Second).Should(gomega.Succeed())
			gomega.Consistently(drainCtx, func(g gomega.Gomega) {
				expectComputeDomainDraining(drainCtx, g, removed)
				var pods []corev1.Pod
				for _, b := range retained {
					pod := b.Pod.DeepCopy()
					expectObjectExists(drainCtx, g, pod)
					pods = append(pods, *pod)
				}
				expectComputeDomainBindingsUnchanged(g, retained, expectComputeDomainBindings(drainCtx, g, pods))
			}, 5*time.Second, time.Second).Should(gomega.Succeed())
			releaseComputeDomainPods(drainCtx, removed)
			gomega.Eventually(drainCtx, func(g gomega.Gomega) {
				expectComputeDomainBindingsDeleted(drainCtx, g, removed)
			}, computeDomainTimeout, time.Second).Should(gomega.Succeed())
			expectComputeDomainBindingsUnchanged(gomega.Default, retained, expectComputeDomainWorkloadReady(fixture))
		},
		ginkgo.Entry("LWS Ordinal", false, lwsv1.GroupIdentityOrdinal),
		ginkgo.Entry("LWS Hash", false, lwsv1.GroupIdentityHash),
		ginkgo.Entry("DisaggregatedSet", true, lwsv1.GroupIdentityOrdinal),
	)

	ginkgo.DescribeTable("isolates new-revision domains while old Pods drain during rollout",
		func(disaggregated bool, identity lwsv1.GroupIdentityType) {
			ginkgo.By("creating a workload with one group or slice")
			// One old group and one new group fit on four exclusive channel devices.
			fixture := createComputeDomainFixture(ns.Name, disaggregated, identity, 1)
			ginkgo.By("waiting for allocated claims and a Ready workload")
			before := expectComputeDomainWorkloadReady(fixture)
			ginkgo.By("rolling out a new template and checking domain isolation and cleanup")
			drainCtx, cancel := context.WithTimeout(ctx, computeDomainTimeout)
			defer cancel()
			gomega.Expect(retry.RetryOnConflict(retry.DefaultRetry, func() error {
				if err := k8sClient.Get(drainCtx, client.ObjectKeyFromObject(fixture.workload), fixture.workload); err != nil {
					return err
				}
				update := func(spec *lwsv1.LeaderWorkerSetSpec) {
					template := &spec.LeaderWorkerTemplate.WorkerTemplate
					template.Labels[computeDomainVersionLabel] = "target"
					// Surge Pods must exit normally when rollout scales back down.
					template.Spec.Containers[0].Lifecycle = nil
				}
				if ds, ok := fixture.workload.(*dsv1.DisaggregatedSet); ok {
					for i := range ds.Spec.Roles {
						update(&ds.Spec.Roles[i].Spec)
					}
				} else {
					update(&fixture.workload.(*lwsv1.LeaderWorkerSet).Spec)
				}
				return k8sClient.Update(drainCtx, fixture.workload)
			})).To(gomega.Succeed())
			checkRollout := func(g gomega.Gomega) {
				expectComputeDomainDraining(drainCtx, g, before)
				pods := listComputeDomainPods(drainCtx, ns.Name, "target")
				g.Expect(pods).To(gomega.HaveLen(fixture.podCount))
				current := expectComputeDomainBindings(drainCtx, g, pods)
				expectComputeDomainGroups(g, current, disaggregated, 1)
				for _, b := range current {
					g.Expect(b.Pod.Labels[lwsv1.RevisionKey]).NotTo(gomega.BeEmpty(), b.Pod.Name)
					for _, old := range before {
						description := fmt.Sprintf("new Pod %s, old Pod %s", b.Pod.Name, old.Pod.Name)
						g.Expect(b.Pod.UID).NotTo(gomega.Equal(old.Pod.UID), description)
						g.Expect(b.Pod.Labels[lwsv1.RevisionKey]).NotTo(gomega.Equal(old.Pod.Labels[lwsv1.RevisionKey]), description)
						g.Expect(b.Domain.UID).NotTo(gomega.Equal(old.Domain.UID), "%s: domains %s and %s", description, b.Domain.Name, old.Domain.Name)
						g.Expect(b.Template.UID).NotTo(gomega.Equal(old.Template.UID), "%s: templates %s and %s", description, b.Template.Name, old.Template.Name)
						g.Expect(b.Claim.UID).NotTo(gomega.Equal(old.Claim.UID), "%s: claims %s and %s", description, b.Claim.Name, old.Claim.Name)
						g.Expect(b.Pod.Spec.NodeName).NotTo(gomega.Equal(old.Pod.Spec.NodeName), description)
					}
				}
			}
			gomega.Eventually(drainCtx, checkRollout, computeDomainTimeout, time.Second).Should(gomega.Succeed())
			gomega.Consistently(drainCtx, checkRollout, 5*time.Second, time.Second).Should(gomega.Succeed())
			releaseComputeDomainPods(drainCtx, before)
			gomega.Eventually(drainCtx, func(g gomega.Gomega) {
				expectComputeDomainBindingsDeleted(drainCtx, g, before)
			}, computeDomainTimeout, time.Second).Should(gomega.Succeed())
			for _, b := range expectComputeDomainWorkloadReady(fixture) {
				gomega.Expect(b.Pod.Labels[computeDomainVersionLabel]).To(gomega.Equal("target"), b.Pod.Name)
			}
		},
		ginkgo.Entry("LWS Ordinal", false, lwsv1.GroupIdentityOrdinal),
		ginkgo.Entry("LWS Hash", false, lwsv1.GroupIdentityHash),
		ginkgo.Entry("DisaggregatedSet", true, lwsv1.GroupIdentityOrdinal),
	)
})

func createComputeDomainFixture(ns string, disaggregated bool, identity lwsv1.GroupIdentityType, groups int) computeDomainFixture {
	image := "docker.io/library/busybox:1.38.0"
	template := corev1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{computeDomainVersionLabel: "source"}},
		Spec: corev1.PodSpec{
			NodeSelector: map[string]string{fakeGPUNodePoolLabel: "default"},
			// Drain assertions share a deadline shorter than this grace period.
			TerminationGracePeriodSeconds: ptr.To(int64(computeDomainTerminationGrace / time.Second)),
			Containers: []corev1.Container{
				{
					Name: "app", Image: image, ImagePullPolicy: corev1.PullIfNotPresent,
					Command: []string{"sh", "-c", "trap 'exit 0' TERM; while :; do sleep 1 & wait $!; done"},
					Resources: corev1.ResourceRequirements{Limits: corev1.ResourceList{
						computedomain.NvidiaGPUResourceName: resource.MustParse("1"),
					}},
					Lifecycle: &corev1.Lifecycle{PreStop: &corev1.LifecycleHandler{
						Exec: &corev1.ExecAction{Command: []string{"sh", "-c", "touch /tmp/draining; while [ ! -f /tmp/release ]; do sleep 1; done"}},
					}},
				},
			},
		},
	}
	lws := wrappers.BuildLeaderWorkerSet(ns).Name("model").Replica(groups).Size(2).MaxSurge(1).MaxUnavailable(0).Obj()
	lws.Spec.GroupIdentity = identity
	lws.Spec.PodTerminationPolicy = lwsv1.ParallelPodTerminationPolicy
	lws.Spec.LeaderWorkerTemplate.LeaderTemplate = nil
	lws.Spec.LeaderWorkerTemplate.WorkerTemplate = template
	lws.Spec.RolloutStrategy.RollingUpdateConfiguration.Partition = nil
	lws.Annotations = map[string]string{lwsv1.AutoComputeDomainAnnotationKey: lwsv1.AutoComputeDomainEnabled}
	fixture := computeDomainFixture{workload: lws, podCount: 2 * groups}
	if disaggregated {
		ds := wrappers.BuildDisaggregatedSet("serving", ns).UID("").Slices(int32(groups)).Obj()
		ds.Annotations = map[string]string{dsv1.AutoComputeDomainAnnotationKey: lwsv1.AutoComputeDomainEnabled}
		for _, name := range []string{"prefill", "decode"} {
			spec := lws.Spec.DeepCopy()
			spec.Replicas = ptr.To[int32](1)
			spec.LeaderWorkerTemplate.Size = ptr.To[int32](1)
			role := dsv1.DisaggregatedRoleSpec{Name: name, LeaderWorkerSetTemplateSpec: lwsv1.LeaderWorkerSetTemplateSpec{Spec: *spec}}
			ds.Spec.Roles = append(ds.Spec.Roles, role)
		}
		fixture.workload = ds
	}
	gomega.Expect(k8sClient.Create(ctx, fixture.workload)).To(gomega.Succeed())
	return fixture
}

func listComputeDomainPods(ctx context.Context, ns, version string) []corev1.Pod {
	var pods corev1.PodList
	options := []client.ListOption{client.InNamespace(ns)}
	if version != "" {
		options = append(options, client.MatchingLabels{computeDomainVersionLabel: version})
	}
	gomega.Expect(k8sClient.List(ctx, &pods, options...)).To(gomega.Succeed())
	return pods.Items
}

// Follow API references and UIDs, not production naming or revision functions.
func expectComputeDomainBindings(ctx context.Context, g gomega.Gomega, pods []corev1.Pod) []computeDomainBinding {
	result := make([]computeDomainBinding, 0, len(pods))
	for _, pod := range pods {
		g.Expect(pod.DeletionTimestamp).To(gomega.BeNil(), pod.Name)
		g.Expect(pod.Status.Phase).To(gomega.Equal(corev1.PodRunning), pod.Name)
		g.Expect(podutils.IsPodReady(&pod)).To(gomega.BeTrue(), pod.Name)
		b := computeDomainBinding{Pod: pod}
		g.Expect(pod.Spec.ResourceClaims).To(gomega.HaveLen(1), pod.Name)
		ref := pod.Spec.ResourceClaims[0]
		g.Expect(ref.Name).To(gomega.Equal(computedomain.ChannelClaimName), pod.Name)
		g.Expect(ref.ResourceClaimTemplateName).NotTo(gomega.BeNil(), pod.Name)
		b.Template.SetNamespace(pod.Namespace)
		b.Template.SetName(*ref.ResourceClaimTemplateName)
		expectObjectExists(ctx, g, &b.Template)
		g.Expect(b.Template.DeletionTimestamp).To(gomega.BeNil(), "Pod %s, template %s", pod.Name, b.Template.Name)
		owner := metav1.GetControllerOf(&b.Template)
		g.Expect(owner).NotTo(gomega.BeNil(), "Pod %s, template %s", pod.Name, b.Template.Name)
		g.Expect(owner.Kind).To(gomega.Equal(computedomain.Kind), b.Template.Name)
		g.Expect(owner.APIVersion).To(gomega.Equal(computedomain.GroupVersion.String()), b.Template.Name)
		b.Domain.SetNamespace(pod.Namespace)
		b.Domain.SetName(owner.Name)
		expectObjectExists(ctx, g, &b.Domain)
		g.Expect(b.Domain.UID).To(gomega.Equal(owner.UID), "Pod %s, domain %s, template %s", pod.Name, b.Domain.Name, b.Template.Name)
		g.Expect(b.Domain.Spec.Channel).NotTo(gomega.BeNil(), b.Domain.Name)
		g.Expect(b.Domain.Spec.Channel.ResourceClaimTemplate.Name).To(gomega.Equal(b.Template.Name), "Pod %s, domain %s", pod.Name, b.Domain.Name)
		g.Expect(b.Domain.DeletionTimestamp).To(gomega.BeNil(), b.Domain.Name)

		g.Expect(pod.Status.ResourceClaimStatuses).To(gomega.HaveLen(1), pod.Name)
		status := pod.Status.ResourceClaimStatuses[0]
		g.Expect(status.Name).To(gomega.Equal(ref.Name), pod.Name)
		g.Expect(status.ResourceClaimName).NotTo(gomega.BeNil(), pod.Name)
		b.Claim.SetNamespace(pod.Namespace)
		b.Claim.SetName(*status.ResourceClaimName)
		expectObjectExists(ctx, g, &b.Claim)
		owner = metav1.GetControllerOf(&b.Claim)
		g.Expect(owner).NotTo(gomega.BeNil(), "Pod %s, claim %s", pod.Name, b.Claim.Name)
		g.Expect(owner.Kind).To(gomega.Equal("Pod"), b.Claim.Name)
		g.Expect(owner.UID).To(gomega.Equal(pod.UID), "Pod %s, claim %s", pod.Name, b.Claim.Name)
		g.Expect(b.Claim.Status.Allocation).NotTo(gomega.BeNil(), "Pod %s, claim %s", pod.Name, b.Claim.Name)
		g.Expect(b.Claim.Status.Allocation.Devices.Results).NotTo(gomega.BeEmpty(), b.Claim.Name)
		g.Expect(b.Claim.Status.ReservedFor).To(gomega.ContainElement(gomega.HaveField("UID", pod.UID)), "Pod %s, claim %s", pod.Name, b.Claim.Name)
		found := false
		for _, config := range b.Claim.Spec.Devices.Config {
			if config.Opaque != nil && config.Opaque.Driver == "compute-domain.nvidia.com" {
				var parameters struct {
					DomainID string `json:"domainID"`
				}
				g.Expect(json.Unmarshal(config.Opaque.Parameters.Raw, &parameters)).To(gomega.Succeed(), b.Claim.Name)
				g.Expect(parameters.DomainID).To(gomega.Equal(string(b.Domain.UID)), "Pod %s, claim %s, domain %s", pod.Name, b.Claim.Name, b.Domain.Name)
				found = true
			}
		}
		g.Expect(found).To(gomega.BeTrue(), "Pod %s: claim %s must carry the domain UID", pod.Name, b.Claim.Name)
		for _, allocation := range b.Claim.Status.Allocation.Devices.Results {
			g.Expect(allocation.Driver).To(gomega.Equal("compute-domain.nvidia.com"), b.Claim.Name)
		}
		g.Expect(pod.Spec.Containers[0].Resources.Claims).To(gomega.Equal([]corev1.ResourceClaim{{Name: ref.Name}}), "Pod %s, container app", pod.Name)
		result = append(result, b)
	}
	return result
}

func expectComputeDomainWorkloadReady(fixture computeDomainFixture) []computeDomainBinding {
	readyCtx, cancel := context.WithTimeout(ctx, computeDomainTimeout)
	defer cancel()
	var result []computeDomainBinding
	gomega.Eventually(readyCtx, func(g gomega.Gomega) {
		pods := listComputeDomainPods(readyCtx, fixture.workload.GetNamespace(), "")
		g.Expect(pods).To(gomega.HaveLen(fixture.podCount))
		result = expectComputeDomainBindings(readyCtx, g, pods)
		expectObjectExists(readyCtx, g, fixture.workload)
		ds, disaggregated := fixture.workload.(*dsv1.DisaggregatedSet)
		var groups int
		if disaggregated {
			groups = int(*ds.Spec.Slices)
			g.Expect(ds.Status.ObservedGeneration).To(gomega.Equal(ds.Generation))
			g.Expect(meta.IsStatusConditionTrue(ds.Status.Conditions, string(dsv1.DisaggregatedSetAvailable))).To(gomega.BeTrue())
		} else {
			lws := fixture.workload.(*lwsv1.LeaderWorkerSet)
			groups = int(*lws.Spec.Replicas)
			g.Expect(lws.Status.ObservedGeneration).To(gomega.Equal(lws.Generation))
			g.Expect(lws.Status.ReadyReplicas).To(gomega.Equal(*lws.Spec.Replicas))
			g.Expect(lws.Status.UpdatedReplicas).To(gomega.Equal(*lws.Spec.Replicas))
			g.Expect(meta.IsStatusConditionTrue(lws.Status.Conditions, string(lwsv1.LeaderWorkerSetAvailable))).To(gomega.BeTrue())
		}
		expectComputeDomainGroups(g, result, disaggregated, groups)
		var allDomains computedomain.ComputeDomainList
		gomega.Expect(k8sClient.List(readyCtx, &allDomains, client.InNamespace(fixture.workload.GetNamespace()))).To(gomega.Succeed())
		g.Expect(allDomains.Items).To(gomega.HaveLen(groups))
	}, computeDomainTimeout, time.Second).Should(gomega.Succeed())
	return result
}

func expectComputeDomainGroups(g gomega.Gomega, bindings []computeDomainBinding, disaggregated bool, groups int) {
	domains := map[string]types.UID{}
	nodes := map[string]sets.Set[string]{}
	claims := sets.New[types.UID]()
	for _, b := range bindings {
		group := b.Pod.Labels[lwsv1.GroupIndexLabelKey]
		if disaggregated {
			group = b.Pod.Labels[dsv1.SliceLabelKey]
		}
		g.Expect(group).NotTo(gomega.BeEmpty())
		if uid, exists := domains[group]; exists {
			g.Expect(b.Domain.UID).To(gomega.Equal(uid), "same group or slice must share a domain")
		} else {
			for _, uid := range domains {
				g.Expect(b.Domain.UID).NotTo(gomega.Equal(uid), "different groups or slices must not share a domain")
			}
			domains[group] = b.Domain.UID
			nodes[group] = sets.New[string]()
		}
		nodes[group].Insert(b.Pod.Spec.NodeName)
		g.Expect(claims.Has(b.Claim.UID)).To(gomega.BeFalse(), "each Pod must have its own claim")
		claims.Insert(b.Claim.UID)
	}
	g.Expect(domains).To(gomega.HaveLen(groups))
	for _, members := range nodes {
		g.Expect(members.Len()).To(gomega.Equal(2), "exercise cross-node membership")
	}
}

func execComputeDomainPod(ctx context.Context, pod corev1.Pod, command ...string) error {
	execCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	kubectl := os.Getenv("KUBECTL")
	if kubectl == "" {
		kubectl = "kubectl"
	}
	args := []string{"--kubeconfig", os.Getenv("KUBECONFIG"), "--request-timeout=15s", "-n", pod.Namespace, "exec", pod.Name, "-c", "app", "--"}
	out, err := exec.CommandContext(execCtx, kubectl, append(args, command...)...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("exec %s: %w: %s", pod.Name, err, out)
	}
	return nil
}

func releaseComputeDomainPods(ctx context.Context, bindings []computeDomainBinding) {
	for _, b := range bindings {
		pod := b.Pod.DeepCopy()
		expectObjectExists(ctx, gomega.Default, pod)
		gomega.Expect(pod.UID).To(gomega.Equal(b.Pod.UID), "never release a replacement Pod")
		gomega.Expect(execComputeDomainPod(ctx, *pod, "touch", "/tmp/release")).To(gomega.Succeed())
	}
}

func expectComputeDomainDraining(ctx context.Context, g gomega.Gomega, bindings []computeDomainBinding) {
	draining := false
	for _, b := range bindings {
		pod := b.Pod.DeepCopy()
		expectObjectExists(ctx, g, pod)
		g.Expect(pod.UID).To(gomega.Equal(b.Pod.UID), pod.Name)
		g.Expect(pod.Status.ContainerStatuses).To(gomega.ContainElement(gomega.And(
			gomega.HaveField("Name", "app"),
			gomega.HaveField("State.Running", gomega.Not(gomega.BeNil())),
		)), pod.Name)
		if pod.DeletionTimestamp != nil {
			g.Expect(execComputeDomainPod(ctx, *pod, "test", "-f", "/tmp/draining")).To(gomega.Succeed())
			draining = true
		}
		for _, original := range []client.Object{&b.Domain, &b.Template, &b.Claim} {
			current := original.DeepCopyObject().(client.Object)
			expectObjectExists(ctx, g, current)
			g.Expect(current.GetUID()).To(gomega.Equal(original.GetUID()), "Pod %s, %T %s", pod.Name, original, original.GetName())
			if claim, ok := current.(*resourcev1.ResourceClaim); ok {
				g.Expect(claim.Status.Allocation).To(gomega.Equal(b.Claim.Status.Allocation), "Pod %s, claim %s", pod.Name, claim.Name)
				g.Expect(claim.Status.ReservedFor).To(gomega.ContainElement(gomega.HaveField("UID", pod.UID)), "Pod %s, claim %s", pod.Name, claim.Name)
			} else {
				g.Expect(current.GetDeletionTimestamp()).To(gomega.BeNil(), "Pod %s, %T %s: a finalizer alone does not keep the domain usable", pod.Name, current, current.GetName())
			}
		}
	}
	// Garbage collection need not request deletion of every Pod simultaneously.
	g.Expect(draining).To(gomega.BeTrue(), "a participating Pod must have entered preStop")
}

func expectComputeDomainBindingsUnchanged(g gomega.Gomega, before, after []computeDomainBinding) {
	g.Expect(after).To(gomega.HaveLen(len(before)))
	for _, original := range before {
		index := slices.IndexFunc(after, func(b computeDomainBinding) bool { return b.Pod.UID == original.Pod.UID })
		g.Expect(index).To(gomega.BeNumerically(">=", 0), original.Pod.Name)
		g.Expect(after[index].Domain.UID).To(gomega.Equal(original.Domain.UID), "Pod %s, domain %s", original.Pod.Name, original.Domain.Name)
		g.Expect(after[index].Template.UID).To(gomega.Equal(original.Template.UID), "Pod %s, template %s", original.Pod.Name, original.Template.Name)
		g.Expect(after[index].Claim.UID).To(gomega.Equal(original.Claim.UID), "Pod %s, claim %s", original.Pod.Name, original.Claim.Name)
	}
}

func expectComputeDomainBindingsDeleted(ctx context.Context, g gomega.Gomega, bindings []computeDomainBinding) {
	for _, b := range bindings {
		// Ordinal rollout can recreate a Pod under the same name.
		var pod corev1.Pod
		err := k8sClient.Get(ctx, client.ObjectKeyFromObject(&b.Pod), &pod)
		gomega.Expect(client.IgnoreNotFound(err)).To(gomega.Succeed())
		g.Expect(pod.UID).NotTo(gomega.Equal(b.Pod.UID), b.Pod.Name)
		expectObjectDeleted(ctx, g, &b.Domain)
		expectObjectDeleted(ctx, g, &b.Template)
		expectObjectDeleted(ctx, g, &b.Claim)
	}
}

func expectObjectExists(ctx context.Context, g gomega.Gomega, obj client.Object) {
	err := k8sClient.Get(ctx, client.ObjectKeyFromObject(obj), obj)
	gomega.Expect(client.IgnoreNotFound(err)).To(gomega.Succeed(), "%T %s", obj, client.ObjectKeyFromObject(obj))
	g.Expect(err).NotTo(gomega.HaveOccurred(), "%T %s", obj, client.ObjectKeyFromObject(obj))
}

func expectObjectDeleted(ctx context.Context, g gomega.Gomega, obj client.Object) {
	err := k8sClient.Get(ctx, client.ObjectKeyFromObject(obj), obj.DeepCopyObject().(client.Object))
	gomega.Expect(client.IgnoreNotFound(err)).To(gomega.Succeed())
	g.Expect(apierrors.IsNotFound(err)).To(gomega.BeTrue(), "%T %s still exists", obj, obj.GetName())
}

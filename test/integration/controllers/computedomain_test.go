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

package controllers

import (
	"fmt"
	"maps"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	disaggregatedsetv1 "sigs.k8s.io/lws/api/disaggregatedset/v1"
	leaderworkerset "sigs.k8s.io/lws/api/leaderworkerset/v1"
	"sigs.k8s.io/lws/pkg/computedomain"
	"sigs.k8s.io/lws/pkg/features"
	disaggregatedsetutils "sigs.k8s.io/lws/pkg/utils/disaggregatedset"
	podutils "sigs.k8s.io/lws/pkg/utils/pod"
	"sigs.k8s.io/lws/pkg/webhooks"
	testing "sigs.k8s.io/lws/test/testutils"
	"sigs.k8s.io/lws/test/wrappers"
)

var _ = ginkgo.Describe("AutoComputeDomain controller lifecycle", func() {
	var (
		ns  *corev1.Namespace
		pw  *webhooks.PodWebhook
		lws *leaderworkerset.LeaderWorkerSet
	)

	gpuContainer := func(name string) corev1.Container {
		return corev1.Container{
			Name:  name,
			Image: "nvcr.io/nvidia/vllm:latest",
			Resources: corev1.ResourceRequirements{
				Limits: corev1.ResourceList{
					computedomain.NvidiaGPUResourceName: resource.MustParse("4"),
				},
			},
		}
	}

	admitPodFromStatefulSet := func(sts *appsv1.StatefulSet, ordinal int) *corev1.Pod {
		podName := fmt.Sprintf("%s-%d", sts.Name, ordinal)
		labels := maps.Clone(sts.Spec.Template.Labels)
		if labels == nil {
			labels = map[string]string{}
		}
		annotations := maps.Clone(sts.Spec.Template.Annotations)
		if annotations == nil {
			annotations = map[string]string{}
		}
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:        podName,
				Namespace:   sts.Namespace,
				Labels:      labels,
				Annotations: annotations,
			},
			Spec: *sts.Spec.Template.Spec.DeepCopy(),
		}
		pod.Spec.Hostname = podName
		pod.Spec.Subdomain = sts.Spec.ServiceName
		gomega.Expect(pw.Default(ctx, pod)).To(gomega.Succeed())
		gomega.Expect(k8sClient.Create(ctx, pod)).To(gomega.Succeed())
		return pod
	}

	// envtest runs no garbage collector. Delete a ComputeDomain the way garbage
	// collection does once its owner is gone, and let the protection finalizer
	// hold it.
	gcDeleteComputeDomain := func(name string) {
		var cd computedomain.ComputeDomain
		gomega.Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns.Name, Name: name}, &cd)).To(gomega.Succeed())
		gomega.Expect(k8sClient.Delete(ctx, &cd)).To(gomega.Succeed())
	}

	ginkgo.BeforeEach(func() {
		features.SetFeatureGateDuringTest(ginkgo.GinkgoTB(), features.AutoComputeDomain, true)
		pw = webhooks.NewPodWebhook(nil)
		ns = &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "lws-cd-ns-"}}
		gomega.Expect(k8sClient.Create(ctx, ns)).To(gomega.Succeed())
	})

	ginkgo.AfterEach(func() {
		gomega.Expect(testing.DeleteNamespace(ctx, k8sClient, ns)).To(gomega.Succeed())
	})

	ginkgo.It("manages per-replica ComputeDomains across creation, scale-down finalizer hold, and ordinal incarnation restart", func() {
		lws = wrappers.BuildLeaderWorkerSet(ns.Name).
			Name("deepseek").
			Replica(2).
			Size(2).
			Obj()
		lwsHook := &webhooks.LeaderWorkerSetWebhook{}
		gomega.Expect(lwsHook.Default(ctx, lws)).To(gomega.Succeed())
		lws.Spec.GroupReplacementPolicy = leaderworkerset.GroupReplacementImmediate
		lws.Annotations = map[string]string{
			leaderworkerset.AutoComputeDomainAnnotationKey: leaderworkerset.AutoComputeDomainEnabled,
		}
		lws.Spec.LeaderWorkerTemplate.LeaderTemplate = nil
		lws.Spec.LeaderWorkerTemplate.WorkerTemplate.Spec.Containers = []corev1.Container{
			gpuContainer("vllm"),
			{Name: "routing-proxy", Image: "envoy:latest"},
		}
		gomega.Expect(k8sClient.Create(ctx, lws)).To(gomega.Succeed())

		var leaderSts appsv1.StatefulSet
		gomega.Eventually(func(g gomega.Gomega) {
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns.Name, Name: lws.Name}, &leaderSts)).To(gomega.Succeed())
			g.Expect(leaderSts.Spec.Template.Annotations[leaderworkerset.ComputeDomainPrefixAnnotationKey]).To(
				gomega.Equal(computedomain.UIDPrefix(lws.Name, lws.UID)),
			)
		}, testing.Timeout, testing.Interval).Should(gomega.Succeed())

		// Admit leader pods 0 and 1 via PodWebhook.Default.
		leader0 := admitPodFromStatefulSet(&leaderSts, 0)
		leader1 := admitPodFromStatefulSet(&leaderSts, 1)
		cdName0 := leader0.Annotations[leaderworkerset.ComputeDomainNameAnnotationKey]
		cdName1 := leader1.Annotations[leaderworkerset.ComputeDomainNameAnnotationKey]
		gomega.Expect(cdName0).NotTo(gomega.BeEmpty())
		gomega.Expect(cdName1).NotTo(gomega.BeEmpty())
		gomega.Expect(cdName0).NotTo(gomega.Equal(cdName1))

		// Verify both ComputeDomains are created with protection finalizer, leader gates lift,
		// and worker StatefulSets inherit the matching compute-domain-name.
		var workerSts0, workerSts1 appsv1.StatefulSet
		gomega.Eventually(func(g gomega.Gomega) {
			var cd0, cd1 computedomain.ComputeDomain
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns.Name, Name: cdName0}, &cd0)).To(gomega.Succeed())
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns.Name, Name: cdName1}, &cd1)).To(gomega.Succeed())
			g.Expect(controllerutil.ContainsFinalizer(&cd0, leaderworkerset.ComputeDomainProtectionFinalizer)).To(gomega.BeTrue())
			g.Expect(cd0.Spec.Channel.ResourceClaimTemplate.Name).To(gomega.Equal(cdName0))
			g.Expect(metav1.IsControlledBy(&cd0, lws)).To(gomega.BeTrue())
			g.Expect(metav1.IsControlledBy(&cd1, lws)).To(gomega.BeTrue())

			var refreshedLeader0 corev1.Pod
			g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(leader0), &refreshedLeader0)).To(gomega.Succeed())
			g.Expect(podutils.HasSchedulingGate(&refreshedLeader0, leaderworkerset.GroupReplacementSchedulingGate)).To(gomega.BeFalse())

			g.Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns.Name, Name: leader0.Name}, &workerSts0)).To(gomega.Succeed())
			g.Expect(workerSts0.Spec.Template.Annotations[leaderworkerset.ComputeDomainNameAnnotationKey]).To(gomega.Equal(cdName0))
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns.Name, Name: leader1.Name}, &workerSts1)).To(gomega.Succeed())
			g.Expect(workerSts1.Spec.Template.Annotations[leaderworkerset.ComputeDomainNameAnnotationKey]).To(gomega.Equal(cdName1))
		}, testing.Timeout, testing.Interval).Should(gomega.Succeed())

		worker0 := admitPodFromStatefulSet(&workerSts0, 1)
		worker1 := admitPodFromStatefulSet(&workerSts1, 1)
		gomega.Expect(worker0.Spec.Containers[0].Resources.Claims).To(gomega.HaveLen(1))
		gomega.Expect(worker0.Spec.Containers[1].Resources.Claims).To(gomega.BeEmpty())

		// Scale down replicas 2 -> 1 while worker1 still exists: cdName1 must be retained until worker1 is deleted.
		gomega.Eventually(func(g gomega.Gomega) {
			var latest leaderworkerset.LeaderWorkerSet
			g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(lws), &latest)).To(gomega.Succeed())
			latest.Spec.Replicas = ptr.To[int32](1)
			g.Expect(k8sClient.Update(ctx, &latest)).To(gomega.Succeed())
		}, testing.Timeout, testing.Interval).Should(gomega.Succeed())

		gomega.Expect(k8sClient.Delete(ctx, leader1)).To(gomega.Succeed())
		gomega.Expect(k8sClient.Delete(ctx, &workerSts1)).To(gomega.Succeed())

		gomega.Consistently(func(g gomega.Gomega) {
			var cd1 computedomain.ComputeDomain
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns.Name, Name: cdName1}, &cd1)).To(gomega.Succeed())
			g.Expect(cd1.DeletionTimestamp.IsZero()).To(gomega.BeTrue())
			g.Expect(controllerutil.ContainsFinalizer(&cd1, leaderworkerset.ComputeDomainProtectionFinalizer)).To(gomega.BeTrue())
		}, "2s", testing.Interval).Should(gomega.Succeed())

		// Once worker1 finishes terminating, cdName1 is cleaned up automatically.
		gomega.Expect(k8sClient.Delete(ctx, worker1)).To(gomega.Succeed())
		gomega.Eventually(func() bool {
			var cd1 computedomain.ComputeDomain
			return apierrors.IsNotFound(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns.Name, Name: cdName1}, &cd1))
		}, testing.Timeout, testing.Interval).Should(gomega.BeTrue())

		// Simulate ordinal 0 restart while worker0 is held by a kubelet finalizer:
		// new leader0 incarnation gets a fresh ComputeDomain immediately while old cdName0 stays unmarked until worker0 exits.
		const holdFinalizer = "lws.test/kubelet-unprepare"
		gomega.Eventually(func(g gomega.Gomega) {
			var w0 corev1.Pod
			g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(worker0), &w0)).To(gomega.Succeed())
			controllerutil.AddFinalizer(&w0, holdFinalizer)
			g.Expect(k8sClient.Update(ctx, &w0)).To(gomega.Succeed())
		}, testing.Timeout, testing.Interval).Should(gomega.Succeed())

		gomega.Expect(k8sClient.Delete(ctx, leader0)).To(gomega.Succeed())
		gomega.Expect(k8sClient.Delete(ctx, worker0)).To(gomega.Succeed())
		gomega.Eventually(func() bool {
			var p corev1.Pod
			return apierrors.IsNotFound(k8sClient.Get(ctx, client.ObjectKeyFromObject(leader0), &p))
		}, testing.Timeout, testing.Interval).Should(gomega.BeTrue())

		gomega.Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns.Name, Name: lws.Name}, &leaderSts)).To(gomega.Succeed())
		restartedLeader0 := admitPodFromStatefulSet(&leaderSts, 0)
		newCDName0 := restartedLeader0.Annotations[leaderworkerset.ComputeDomainNameAnnotationKey]
		gomega.Expect(newCDName0).NotTo(gomega.Equal(cdName0))

		gomega.Eventually(func(g gomega.Gomega) {
			var newCD, oldCD computedomain.ComputeDomain
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns.Name, Name: newCDName0}, &newCD)).To(gomega.Succeed())
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns.Name, Name: cdName0}, &oldCD)).To(gomega.Succeed())
			g.Expect(oldCD.DeletionTimestamp.IsZero()).To(gomega.BeTrue())
		}, testing.Timeout, testing.Interval).Should(gomega.Succeed())

		// Release worker0's finalizer -> old cdName0 is cleaned up while newCDName0 stays active.
		gomega.Eventually(func(g gomega.Gomega) {
			var w0 corev1.Pod
			g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(worker0), &w0)).To(gomega.Succeed())
			controllerutil.RemoveFinalizer(&w0, holdFinalizer)
			g.Expect(k8sClient.Update(ctx, &w0)).To(gomega.Succeed())
		}, testing.Timeout, testing.Interval).Should(gomega.Succeed())

		gomega.Eventually(func(g gomega.Gomega) {
			var oldCD, newCD computedomain.ComputeDomain
			g.Expect(apierrors.IsNotFound(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns.Name, Name: cdName0}, &oldCD))).To(gomega.BeTrue())
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns.Name, Name: newCDName0}, &newCD)).To(gomega.Succeed())
		}, testing.Timeout, testing.Interval).Should(gomega.Succeed())
	})

	ginkgo.It("owns Hash groupIdentity ComputeDomains by the LeaderWorkerSet and releases the finalizer on cascade delete after Pods exit", func() {
		lws = wrappers.BuildLeaderWorkerSet(ns.Name).
			Name("hash-model").
			Replica(1).
			Size(2).
			Obj()
		lwsHook := &webhooks.LeaderWorkerSetWebhook{}
		gomega.Expect(lwsHook.Default(ctx, lws)).To(gomega.Succeed())
		lws.Spec.GroupIdentity = leaderworkerset.GroupIdentityHash
		lws.Annotations = map[string]string{
			leaderworkerset.AutoComputeDomainAnnotationKey: leaderworkerset.AutoComputeDomainEnabled,
		}
		lws.Spec.LeaderWorkerTemplate.LeaderTemplate = nil
		lws.Spec.LeaderWorkerTemplate.WorkerTemplate.Spec.Containers = []corev1.Container{gpuContainer("vllm")}
		gomega.Expect(k8sClient.Create(ctx, lws)).To(gomega.Succeed())

		var deploy appsv1.Deployment
		gomega.Eventually(func(g gomega.Gomega) {
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns.Name, Name: lws.Name}, &deploy)).To(gomega.Succeed())
			g.Expect(deploy.Spec.Template.Annotations[leaderworkerset.ComputeDomainPrefixAnnotationKey]).NotTo(gomega.BeEmpty())
		}, testing.Timeout, testing.Interval).Should(gomega.Succeed())

		const holdFinalizer = "lws.test/pod-hold"
		leader := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: lws.Name + "-rs-",
				Namespace:    ns.Name,
				Labels:       maps.Clone(deploy.Spec.Template.Labels),
				Annotations:  maps.Clone(deploy.Spec.Template.Annotations),
				Finalizers:   []string{holdFinalizer},
			},
			Spec: *deploy.Spec.Template.Spec.DeepCopy(),
		}
		gomega.Expect(pw.Default(ctx, leader)).To(gomega.Succeed())
		gomega.Expect(k8sClient.Create(ctx, leader)).To(gomega.Succeed())

		cdName := leader.Annotations[leaderworkerset.ComputeDomainNameAnnotationKey]
		gomega.Expect(cdName).NotTo(gomega.BeEmpty())

		gomega.Eventually(func(g gomega.Gomega) {
			var cd computedomain.ComputeDomain
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns.Name, Name: cdName}, &cd)).To(gomega.Succeed())
			g.Expect(metav1.IsControlledBy(&cd, lws)).To(gomega.BeTrue())
			var workerSts appsv1.StatefulSet
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns.Name, Name: leader.Spec.Hostname}, &workerSts)).To(gomega.Succeed())
			g.Expect(workerSts.Spec.Template.Annotations[leaderworkerset.ComputeDomainNameAnnotationKey]).To(gomega.Equal(cdName))
		}, testing.Timeout, testing.Interval).Should(gomega.Succeed())

		// Delete parent LWS and request leader pod deletion while leader pod is held by finalizer.
		gomega.Expect(k8sClient.Delete(ctx, lws)).To(gomega.Succeed())
		gomega.Expect(k8sClient.Delete(ctx, leader)).To(gomega.Succeed())

		// Garbage collection deletes the ComputeDomain while the terminating leader
		// still references it, and the finalizer holds it.
		gcDeleteComputeDomain(cdName)
		gomega.Consistently(func(g gomega.Gomega) {
			var cd computedomain.ComputeDomain
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns.Name, Name: cdName}, &cd)).To(gomega.Succeed())
		}, "2s", testing.Interval).Should(gomega.Succeed())

		// Remove the pod finalizer: the reconciler releases the protection finalizer.
		gomega.Eventually(func(g gomega.Gomega) {
			var p corev1.Pod
			g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(leader), &p)).To(gomega.Succeed())
			controllerutil.RemoveFinalizer(&p, holdFinalizer)
			g.Expect(k8sClient.Update(ctx, &p)).To(gomega.Succeed())
		}, testing.Timeout, testing.Interval).Should(gomega.Succeed())

		gomega.Eventually(func() bool {
			var cd computedomain.ComputeDomain
			return apierrors.IsNotFound(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns.Name, Name: cdName}, &cd))
		}, testing.Timeout, testing.Interval).Should(gomega.BeTrue())
	})

	ginkgo.It("manages per-(slice, revision) ComputeDomains and child LWS claims for DisaggregatedSet with role opt-out and slice scale-down", func() {
		makeRole := func(name string, optOut bool) disaggregatedsetv1.DisaggregatedRoleSpec {
			var anns map[string]string
			if optOut {
				anns = map[string]string{
					disaggregatedsetv1.AutoComputeDomainAnnotationKey: leaderworkerset.AutoComputeDomainDisabled,
				}
			}
			return disaggregatedsetv1.DisaggregatedRoleSpec{
				Name: name,
				LeaderWorkerSetTemplateSpec: leaderworkerset.LeaderWorkerSetTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{Annotations: anns},
					Spec: leaderworkerset.LeaderWorkerSetSpec{
						Replicas:      ptr.To[int32](1),
						StartupPolicy: leaderworkerset.LeaderCreatedStartupPolicy,
						RolloutStrategy: leaderworkerset.RolloutStrategy{
							Type: leaderworkerset.RollingUpdateStrategyType,
						},
						LeaderWorkerTemplate: leaderworkerset.LeaderWorkerTemplate{
							Size: ptr.To[int32](2),
							WorkerTemplate: corev1.PodTemplateSpec{
								Spec: corev1.PodSpec{Containers: []corev1.Container{gpuContainer("vllm")}},
							},
						},
					},
				},
			}
		}

		ds := &disaggregatedsetv1.DisaggregatedSet{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "serving",
				Namespace: ns.Name,
				Annotations: map[string]string{
					disaggregatedsetv1.AutoComputeDomainAnnotationKey: leaderworkerset.AutoComputeDomainEnabled,
				},
			},
			Spec: disaggregatedsetv1.DisaggregatedSetSpec{
				Slices: ptr.To[int32](2),
				Roles: []disaggregatedsetv1.DisaggregatedRoleSpec{
					makeRole("prefill", false),
					makeRole("decode", false),
					makeRole("encoder", true),
				},
			},
		}
		gomega.Expect(k8sClient.Create(ctx, ds)).To(gomega.Succeed())

		rev := disaggregatedsetutils.ComputeDisaggregatedSetRevision(ds)
		var cdSlice0Name, cdSlice1Name string

		gomega.Eventually(func(g gomega.Gomega) {
			var persistedDS disaggregatedsetv1.DisaggregatedSet
			g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(ds), &persistedDS)).To(gomega.Succeed())
			cdSlice0Name = computedomain.SliceComputeDomainName(&persistedDS, 0, rev)
			cdSlice1Name = computedomain.SliceComputeDomainName(&persistedDS, 1, rev)

			var cd0, cd1 computedomain.ComputeDomain
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns.Name, Name: cdSlice0Name}, &cd0)).To(gomega.Succeed())
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns.Name, Name: cdSlice1Name}, &cd1)).To(gomega.Succeed())

			var prefill0, decode0, encoder0 leaderworkerset.LeaderWorkerSet
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{
				Namespace: ns.Name,
				Name:      disaggregatedsetutils.GenerateName(ds.Name, 0, rev, "prefill"),
			}, &prefill0)).To(gomega.Succeed())
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{
				Namespace: ns.Name,
				Name:      disaggregatedsetutils.GenerateName(ds.Name, 0, rev, "decode"),
			}, &decode0)).To(gomega.Succeed())
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{
				Namespace: ns.Name,
				Name:      disaggregatedsetutils.GenerateName(ds.Name, 0, rev, "encoder"),
			}, &encoder0)).To(gomega.Succeed())

			g.Expect(prefill0.Spec.LeaderWorkerTemplate.WorkerTemplate.Spec.ResourceClaims).To(gomega.HaveLen(1))
			g.Expect(*prefill0.Spec.LeaderWorkerTemplate.WorkerTemplate.Spec.ResourceClaims[0].ResourceClaimTemplateName).To(gomega.Equal(cdSlice0Name))
			g.Expect(decode0.Spec.LeaderWorkerTemplate.WorkerTemplate.Spec.ResourceClaims).To(gomega.HaveLen(1))
			g.Expect(*decode0.Spec.LeaderWorkerTemplate.WorkerTemplate.Spec.ResourceClaims[0].ResourceClaimTemplateName).To(gomega.Equal(cdSlice0Name))
			g.Expect(encoder0.Spec.LeaderWorkerTemplate.WorkerTemplate.Spec.ResourceClaims).To(gomega.BeEmpty())
		}, testing.Timeout, testing.Interval).Should(gomega.Succeed())

		// Create a slice 1 pod still holding cdSlice1Name when slice 1 scales down:
		// cdSlice1Name must stay unmarked (DeletionTimestamp zero) until the pod exits.
		slice1Pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "serving-slice1-draining-pod",
				Namespace: ns.Name,
				Labels: map[string]string{
					disaggregatedsetv1.SetNameLabelKey: ds.Name,
					disaggregatedsetv1.SliceLabelKey:   "1",
				},
			},
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{gpuContainer("vllm")},
				ResourceClaims: []corev1.PodResourceClaim{{
					Name:                      computedomain.ChannelClaimName,
					ResourceClaimTemplateName: ptr.To(cdSlice1Name),
				}},
			},
		}
		gomega.Expect(k8sClient.Create(ctx, slice1Pod)).To(gomega.Succeed())

		// Scale slices 2 -> 1: remove foreground finalizers on slice 1 child LWSs (envtest has no GC controller),
		// verify cdSlice1Name stays unmarked while slice1Pod exists, then delete slice1Pod and verify cleanup.
		gomega.Eventually(func(g gomega.Gomega) {
			var latest disaggregatedsetv1.DisaggregatedSet
			g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(ds), &latest)).To(gomega.Succeed())
			latest.Spec.Slices = ptr.To[int32](1)
			g.Expect(k8sClient.Update(ctx, &latest)).To(gomega.Succeed())
		}, testing.Timeout, testing.Interval).Should(gomega.Succeed())

		gomega.Eventually(func(g gomega.Gomega) {
			var lwsList leaderworkerset.LeaderWorkerSetList
			g.Expect(k8sClient.List(ctx, &lwsList, client.InNamespace(ns.Name), client.MatchingLabels{
				disaggregatedsetv1.SetNameLabelKey: ds.Name,
				disaggregatedsetv1.SliceLabelKey:   "1",
			})).To(gomega.Succeed())
			for i := range lwsList.Items {
				child := &lwsList.Items[i]
				if len(child.Finalizers) > 0 && !child.DeletionTimestamp.IsZero() {
					child.Finalizers = nil
					g.Expect(client.IgnoreNotFound(k8sClient.Update(ctx, child))).To(gomega.Succeed())
				}
			}
			g.Expect(lwsList.Items).To(gomega.BeEmpty())
		}, testing.Timeout, testing.Interval).Should(gomega.Succeed())

		gomega.Consistently(func(g gomega.Gomega) {
			var cd1 computedomain.ComputeDomain
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns.Name, Name: cdSlice1Name}, &cd1)).To(gomega.Succeed())
			g.Expect(cd1.DeletionTimestamp.IsZero()).To(gomega.BeTrue())
		}, "2s", testing.Interval).Should(gomega.Succeed())

		gomega.Expect(k8sClient.Delete(ctx, slice1Pod)).To(gomega.Succeed())

		gomega.Eventually(func(g gomega.Gomega) {
			var cd1, cd0 computedomain.ComputeDomain
			g.Expect(apierrors.IsNotFound(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns.Name, Name: cdSlice1Name}, &cd1))).To(gomega.BeTrue())
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns.Name, Name: cdSlice0Name}, &cd0)).To(gomega.Succeed())
		}, testing.Timeout, testing.Interval).Should(gomega.Succeed())
	})
})

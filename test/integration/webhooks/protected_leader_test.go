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

package webhooks

import (
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	admissionreviewv1 "k8s.io/api/admission/v1"
	admissionv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	leaderv1 "sigs.k8s.io/lws/api/leaderworkerset/v1"
	"sigs.k8s.io/lws/test/wrappers"
)

var _ = ginkgo.Describe("protected leader DELETE admission", func() {
	var lws *leaderv1.LeaderWorkerSet
	var pod *corev1.Pod
	var webhookConfig *admissionv1.ValidatingWebhookConfiguration
	const webhookIndex = 3
	ginkgo.BeforeEach(func() {
		// envtest loads raw controller-gen manifests. Apply the same CEL patch
		// as Kustomize so these tests exercise deployment-time request matching.
		webhookConfig = &admissionv1.ValidatingWebhookConfiguration{}
		gomega.Expect(k8sClient.Get(ctx, client.ObjectKey{Name: "validating-webhook-configuration"}, webhookConfig)).To(gomega.Succeed())
		gomega.Expect(webhookConfig.Webhooks).To(gomega.HaveLen(4))
		gomega.Expect(webhookConfig.Webhooks[2].Name).To(gomega.Equal("vpod.kb.io"))
		gomega.Expect(webhookConfig.Webhooks[webhookIndex].Name).To(gomega.Equal("vprotectedleader.kb.io"))
		original := webhookConfig.DeepCopy()
		ginkgo.DeferCleanup(func() {
			gomega.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(webhookConfig), webhookConfig)).To(gomega.Succeed())
			webhookConfig.Webhooks = original.Webhooks
			gomega.Expect(k8sClient.Update(ctx, webhookConfig)).To(gomega.Succeed())
		})
		data, err := os.ReadFile(filepath.Join("..", "..", "..", "config", "webhook", "validating-patch.yaml"))
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		var patches []struct {
			Path  string          `json:"path"`
			Value json.RawMessage `json:"value"`
		}
		gomega.Expect(yaml.Unmarshal(data, &patches)).To(gomega.Succeed())
		for _, patch := range patches {
			if patch.Path == "/webhooks/3/matchConditions" {
				gomega.Expect(json.Unmarshal(patch.Value, &webhookConfig.Webhooks[webhookIndex].MatchConditions)).To(gomega.Succeed())
			}
		}
		gomega.Expect(webhookConfig.Webhooks[webhookIndex].MatchConditions).To(gomega.HaveLen(1))
		gomega.Expect(k8sClient.Update(ctx, webhookConfig)).To(gomega.Succeed())
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "protected-delete-"}}
		gomega.Expect(k8sClient.Create(ctx, ns)).To(gomega.Succeed())
		lws = wrappers.BuildLeaderWorkerSet(ns.Name).Replica(1).Size(1).Obj()
		gomega.Expect(k8sClient.Create(ctx, lws)).To(gomega.Succeed())
		pod = wrappers.MakePodWithLabels(lws.Name, "0", "0", ns.Name, 1)
		gomega.Expect(k8sClient.Create(ctx, pod)).To(gomega.Succeed())
		pod.Annotations[leaderv1.GroupScaleProtectionAnnotationKey] = string(lws.UID) + "/" + string(pod.UID)
		gomega.Expect(k8sClient.Update(ctx, pod)).To(gomega.Succeed())
		gomega.Eventually(func() error { return k8sClient.Delete(ctx, pod, client.DryRunAll) }).Should(gomega.MatchError(gomega.ContainSubstring("is retained")))
	})

	ginkgo.It("authorizes exact victims and native replacements without disarming survivors", func() {
		pod.Annotations[leaderv1.GroupScaleVictimAnnotationKey] = "plan/" + string(pod.UID)
		gomega.Expect(k8sClient.Update(ctx, pod)).To(gomega.Succeed())
		gomega.Expect(k8sClient.Delete(ctx, pod, client.DryRunAll)).To(gomega.MatchError(gomega.ContainSubstring("is retained")))
		lws.Annotations = map[string]string{leaderv1.GroupScalePlanAnnotationKey: "different-plan"}
		gomega.Expect(k8sClient.Update(ctx, lws)).To(gomega.Succeed())
		gomega.Expect(k8sClient.Delete(ctx, pod, client.DryRunAll)).To(gomega.MatchError(gomega.ContainSubstring("is retained")))
		lws.Annotations[leaderv1.GroupScalePlanAnnotationKey] = "plan"
		gomega.Expect(k8sClient.Update(ctx, lws)).To(gomega.Succeed())
		gomega.Expect(k8sClient.Delete(ctx, pod, client.DryRunAll)).To(gomega.Succeed())
		delete(lws.Annotations, leaderv1.GroupScalePlanAnnotationKey)
		gomega.Expect(k8sClient.Update(ctx, lws)).To(gomega.Succeed())
		gomega.Expect(k8sClient.Delete(ctx, pod, client.DryRunAll)).To(gomega.MatchError(gomega.ContainSubstring("is retained")))

		pod.Annotations[leaderv1.GroupReplacementDeleteAnnotationKey] = "previous-pod-uid"
		gomega.Expect(k8sClient.Update(ctx, pod)).To(gomega.Succeed())
		gomega.Expect(k8sClient.Delete(ctx, pod, client.DryRunAll)).To(gomega.MatchError(gomega.ContainSubstring("is retained")))
		pod.Annotations[leaderv1.GroupReplacementDeleteAnnotationKey] = string(pod.UID)
		gomega.Expect(k8sClient.Update(ctx, pod)).To(gomega.Succeed())
		gomega.Expect(k8sClient.Delete(ctx, pod)).To(gomega.Succeed())
	})

	ginkgo.It("bypasses an unavailable protection endpoint for ordinary Pods and administrative overrides", func() {
		gomega.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(webhookConfig), webhookConfig)).To(gomega.Succeed())
		badURL := *webhookConfig.Webhooks[webhookIndex].ClientConfig.URL + "/unavailable"
		webhookConfig.Webhooks[webhookIndex].ClientConfig.URL = &badURL
		gomega.Expect(k8sClient.Update(ctx, webhookConfig)).To(gomega.Succeed())
		gomega.Eventually(func() error { return k8sClient.Delete(ctx, pod, client.DryRunAll) }).Should(gomega.MatchError(gomega.ContainSubstring("failed calling webhook")))

		ordinary := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "ordinary", Namespace: pod.Namespace}, Spec: wrappers.MakeLeaderPodSpec()}
		gomega.Expect(k8sClient.Create(ctx, ordinary)).To(gomega.Succeed())
		gomega.Expect(k8sClient.Delete(ctx, ordinary)).To(gomega.Succeed())
		worker := wrappers.MakePodWithLabels(lws.Name, "0", "1", pod.Namespace, 1)
		worker.Annotations[leaderv1.GroupScaleProtectionAnnotationKey] = string(lws.UID) + "/irrelevant-worker-marker"
		gomega.Expect(k8sClient.Create(ctx, worker)).To(gomega.Succeed())
		gomega.Expect(k8sClient.Delete(ctx, worker)).To(gomega.Succeed())
		terminal := wrappers.MakePodWithLabels(lws.Name, "1", "0", pod.Namespace, 1)
		gomega.Expect(k8sClient.Create(ctx, terminal)).To(gomega.Succeed())
		terminal.Annotations[leaderv1.GroupScaleProtectionAnnotationKey] = string(lws.UID) + "/" + string(terminal.UID)
		gomega.Expect(k8sClient.Update(ctx, terminal)).To(gomega.Succeed())
		terminal.Status.Phase = corev1.PodFailed
		gomega.Expect(k8sClient.Status().Update(ctx, terminal)).To(gomega.Succeed())
		gomega.Expect(k8sClient.Delete(ctx, terminal)).To(gomega.Succeed())

		delete(pod.Annotations, leaderv1.GroupScaleProtectionAnnotationKey)
		gomega.Expect(k8sClient.Update(ctx, pod)).To(gomega.Succeed())
		gomega.Expect(k8sClient.Delete(ctx, pod)).To(gomega.Succeed())
	})

	ginkgo.It("allows parent teardown to release protected leaders", func() {
		lws.Finalizers = []string{"test/hold-parent"}
		gomega.Expect(k8sClient.Update(ctx, lws)).To(gomega.Succeed())
		gomega.Expect(k8sClient.Delete(ctx, lws)).To(gomega.Succeed())
		gomega.Expect(k8sClient.Delete(ctx, pod)).To(gomega.Succeed())
	})

	ginkgo.It("UID-bound health and scale intent cannot authorize same-name replacements", func() {
		stale := pod.DeepCopy()
		pod.Labels["concurrent-label"] = "preserved"
		pod.Annotations["concurrent-annotation"] = "preserved"
		gomega.Expect(k8sClient.Update(ctx, pod)).To(gomega.Succeed())
		data, err := json.Marshal(map[string]any{"metadata": map[string]any{
			"uid": stale.UID, "annotations": map[string]string{leaderv1.GroupReplacementDeleteAnnotationKey: string(stale.UID)},
		}})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(k8sClient.Patch(ctx, stale, client.RawPatch(types.MergePatchType, data))).To(gomega.Succeed())
		gomega.Expect(stale.Labels["concurrent-label"]).To(gomega.Equal("preserved"))
		gomega.Expect(stale.Annotations["concurrent-annotation"]).To(gomega.Equal("preserved"))
		gomega.Expect(stale.Annotations[leaderv1.GroupScaleProtectionAnnotationKey]).To(gomega.Equal(string(lws.UID) + "/" + string(stale.UID)))
		gomega.Expect(k8sClient.Delete(ctx, stale, client.GracePeriodSeconds(0))).To(gomega.Succeed())
		replacement := wrappers.MakePodWithLabels(lws.Name, "0", "0", pod.Namespace, 1)
		gomega.Expect(k8sClient.Create(ctx, replacement)).To(gomega.Succeed())
		gomega.Expect(replacement.UID).NotTo(gomega.Equal(stale.UID))
		err = k8sClient.Patch(ctx, stale, client.RawPatch(types.MergePatchType, data))
		gomega.Expect(apierrors.IsInvalid(err)).To(gomega.BeTrue(), "%v", err)
		gomega.Expect(err).To(gomega.MatchError(gomega.ContainSubstring("metadata.uid")))
		err = k8sClient.Delete(ctx, stale, client.Preconditions{UID: &stale.UID})
		gomega.Expect(apierrors.IsConflict(err)).To(gomega.BeTrue(), "%v", err)
		gomega.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(replacement), replacement)).To(gomega.Succeed())
		gomega.Expect(replacement.Annotations[leaderv1.GroupReplacementDeleteAnnotationKey]).To(gomega.BeEmpty())
		// Repairing persistent protection on the replacement cannot revive a
		// copied victim token: the token itself must identify this exact Pod UID.
		lws.Annotations = map[string]string{leaderv1.GroupScalePlanAnnotationKey: "plan"}
		gomega.Expect(k8sClient.Update(ctx, lws)).To(gomega.Succeed())
		replacement.Annotations[leaderv1.GroupScaleProtectionAnnotationKey] = string(lws.UID) + "/" + string(replacement.UID)
		replacement.Annotations[leaderv1.GroupScaleVictimAnnotationKey] = "plan/" + string(stale.UID)
		gomega.Expect(k8sClient.Update(ctx, replacement)).To(gomega.Succeed())
		gomega.Expect(k8sClient.Delete(ctx, replacement, client.DryRunAll)).To(gomega.MatchError(gomega.ContainSubstring("is retained")))
	})

	ginkgo.It("rechecks DELETE admission when protection is installed during an admitted delete", func() {
		// A test-only second endpoint delays the first real DELETE. The
		// production matchCondition correctly skips this initially unprotected
		// Pod; after its metadata changes, storage must rerun admission.
		delete(pod.Annotations, leaderv1.GroupScaleProtectionAnnotationKey)
		pod.Labels["test.lws.io/protection-race"] = string(pod.UID)
		gomega.Expect(k8sClient.Update(ctx, pod)).To(gomega.Succeed())
		started, release := make(chan struct{}), make(chan struct{})
		var once sync.Once
		var installed, blocked atomic.Bool
		server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			review := admissionreviewv1.AdmissionReview{}
			if err := json.NewDecoder(r.Body).Decode(&review); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			if ptr.Deref(review.Request.DryRun, false) {
				installed.Store(true)
			} else if blocked.CompareAndSwap(false, true) {
				close(started)
				select {
				case <-release:
				case <-r.Context().Done():
					return
				}
			}
			review.Response = &admissionreviewv1.AdmissionResponse{UID: review.Request.UID, Allowed: true}
			review.Request = nil
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(review)
		}))
		ginkgo.DeferCleanup(server.Close)
		ginkgo.DeferCleanup(func() { once.Do(func() { close(release) }) })
		delay := &admissionv1.ValidatingWebhookConfiguration{ObjectMeta: metav1.ObjectMeta{Name: "protected-race-" + string(pod.UID)}, Webhooks: []admissionv1.ValidatingWebhook{{
			Name: "delay-delete.test.lws.io", AdmissionReviewVersions: []string{"v1"},
			ClientConfig:  admissionv1.WebhookClientConfig{URL: &server.URL, CABundle: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})},
			FailurePolicy: ptr.To(admissionv1.Fail), SideEffects: ptr.To(admissionv1.SideEffectClassNone), TimeoutSeconds: ptr.To[int32](5),
			ObjectSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"test.lws.io/protection-race": string(pod.UID)}},
			Rules:          []admissionv1.RuleWithOperations{{Operations: []admissionv1.OperationType{admissionv1.Delete}, Rule: admissionv1.Rule{APIGroups: []string{""}, APIVersions: []string{"v1"}, Resources: []string{"pods"}}}},
		}}}
		gomega.Expect(k8sClient.Create(ctx, delay)).To(gomega.Succeed())
		ginkgo.DeferCleanup(func() { gomega.Expect(k8sClient.Delete(ctx, delay)).To(gomega.Succeed()) })
		gomega.Eventually(func() bool {
			return k8sClient.Delete(ctx, pod, client.DryRunAll) == nil && installed.Load()
		}).Should(gomega.BeTrue())
		result := make(chan error, 1)
		deletePod := pod.DeepCopy()
		go func() { result <- k8sClient.Delete(ctx, deletePod, client.GracePeriodSeconds(0)) }()
		gomega.Eventually(started).Should(gomega.BeClosed())
		pod.Annotations[leaderv1.GroupScaleProtectionAnnotationKey] = string(lws.UID) + "/" + string(pod.UID)
		gomega.Expect(k8sClient.Update(ctx, pod)).To(gomega.Succeed())
		once.Do(func() { close(release) })
		var err error
		gomega.Eventually(result).Should(gomega.Receive(&err))
		gomega.Expect(err).To(gomega.MatchError(gomega.ContainSubstring("is retained")))
		gomega.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(pod), pod)).To(gomega.Succeed())
		gomega.Expect(pod.DeletionTimestamp).To(gomega.BeNil())
	})
})

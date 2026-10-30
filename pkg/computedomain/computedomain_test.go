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

package computedomain

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	disaggregatedsetv1 "sigs.k8s.io/lws/api/disaggregatedset/v1"
	leaderworkersetv1 "sigs.k8s.io/lws/api/leaderworkerset/v1"
)

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{
		corev1.AddToScheme,
		leaderworkersetv1.AddToScheme,
		disaggregatedsetv1.AddToScheme,
		AddToScheme,
	} {
		if err := add(scheme); err != nil {
			t.Fatalf("failed to add scheme: %v", err)
		}
	}
	return scheme
}

func newFakeClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	scheme := testScheme(t)
	return fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objs...).
		WithIndex(&corev1.Pod{}, PodClaimTemplateIndex, PodClaimTemplateNames).
		Build()
}

func gpuResources(count string) corev1.ResourceRequirements {
	return corev1.ResourceRequirements{
		Limits: corev1.ResourceList{
			NvidiaGPUResourceName: resource.MustParse(count),
		},
	}
}

func TestNamingAndBounds(t *testing.T) {
	p1 := UIDPrefix("deepseek", types.UID("uid-a"))
	p2 := UIDPrefix("deepseek", types.UID("uid-b"))
	if p1 == p2 {
		t.Fatalf("expected distinct UID prefixes for distinct UIDs, got %q", p1)
	}

	short := "deepseek-12345-rev1-0-abcde"
	if got := BoundedName(short); got != short {
		t.Fatalf("BoundedName modified short name: got %q, want %q", got, short)
	}

	longPrefix := strings.Repeat("a", 50) + "--" + strings.Repeat("b", 30)
	b1 := BoundedName(longPrefix + "-0")
	b2 := BoundedName(longPrefix + "-1")
	if len(b1) > validation.DNS1035LabelMaxLength {
		t.Fatalf("BoundedName exceeded 63 chars: len=%d (%q)", len(b1), b1)
	}
	if errs := validation.IsDNS1035Label(b1); len(errs) > 0 {
		t.Fatalf("BoundedName produced invalid DNS-1035 label %q: %v", b1, errs)
	}
	if b1 == b2 {
		t.Fatalf("expected collision-free truncation for distinct suffixes, got %q", b1)
	}
	if strings.HasSuffix(b1[:len(b1)-overflowHashLength-1], "-") {
		t.Fatalf("unexpected trailing hyphen before digest in %q", b1)
	}

	ordName := ReplicaComputeDomainName("lws-1a2b3", "0", "rev1", "inc99")
	if ordName != "lws-1a2b3-rev1-0-inc99" {
		t.Fatalf("unexpected ordinal ComputeDomain name: %q", ordName)
	}

	ds := &disaggregatedsetv1.DisaggregatedSet{
		ObjectMeta: metav1.ObjectMeta{Name: "serving", UID: types.UID("ds-uid-1")},
	}
	sliceName := SliceComputeDomainName(ds, 2, "abcdef12")
	if !strings.HasSuffix(sliceName, "-s2-abcdef12") {
		t.Fatalf("unexpected slice ComputeDomain name: %q", sliceName)
	}
}

func TestTargetedContainerNamesAndSpecInjection(t *testing.T) {
	restartAlways := corev1.ContainerRestartPolicyAlways
	podSpec := &corev1.PodSpec{
		InitContainers: []corev1.Container{
			{
				Name:      "one-shot-gpu-init",
				Resources: gpuResources("1"),
			},
			{
				Name:          "gpu-sidecar",
				RestartPolicy: &restartAlways,
				Resources:     gpuResources("1"),
			},
			{
				Name:          "cpu-sidecar",
				RestartPolicy: &restartAlways,
			},
		},
		Containers: []corev1.Container{
			{
				Name:      "vllm-worker",
				Resources: gpuResources("4"),
			},
			{
				Name: "routing-proxy",
			},
		},
	}

	// Default auto-detection targets vllm-worker and restartable gpu-sidecar only.
	cloned := podSpec.DeepCopy()
	injected, err := InjectPodSpecClaims(cloned, nil, "cd-replica-0")
	if err != nil {
		t.Fatalf("InjectPodSpecClaims failed: %v", err)
	}
	if !injected {
		t.Fatalf("expected claims to be injected")
	}
	// Caller's original slices must remain untouched even on shallow copies.
	shallowCopy := *podSpec
	if _, err := InjectPodSpecClaims(&shallowCopy, nil, "cd-other"); err != nil {
		t.Fatalf("InjectPodSpecClaims on shallow copy failed: %v", err)
	}
	if len(podSpec.ResourceClaims) != 0 || len(podSpec.Containers[0].Resources.Claims) != 0 {
		t.Fatalf("InjectPodSpecClaims mutated original podSpec slices")
	}

	wantPodClaims := []corev1.PodResourceClaim{{
		Name:                      ChannelClaimName,
		ResourceClaimTemplateName: ptr.To("cd-replica-0"),
	}}
	if diff := cmp.Diff(wantPodClaims, cloned.ResourceClaims); diff != "" {
		t.Fatalf("unexpected pod ResourceClaims (-want +got):\n%s", diff)
	}
	if len(cloned.Containers[0].Resources.Claims) != 1 || cloned.Containers[0].Resources.Claims[0].Name != ChannelClaimName {
		t.Fatalf("expected vllm-worker to receive channel claim, got %+v", cloned.Containers[0].Resources.Claims)
	}
	if len(cloned.Containers[1].Resources.Claims) != 0 {
		t.Fatalf("expected CPU routing-proxy to be skipped, got %+v", cloned.Containers[1].Resources.Claims)
	}
	if len(cloned.InitContainers[0].Resources.Claims) != 0 {
		t.Fatalf("expected non-restartable initContainer to be skipped, got %+v", cloned.InitContainers[0].Resources.Claims)
	}
	if len(cloned.InitContainers[1].Resources.Claims) != 1 {
		t.Fatalf("expected restartable GPU sidecar to receive claim, got %+v", cloned.InitContainers[1].Resources.Claims)
	}
	if len(cloned.InitContainers[2].Resources.Claims) != 0 {
		t.Fatalf("expected CPU restartable sidecar to be skipped, got %+v", cloned.InitContainers[2].Resources.Claims)
	}

	// Idempotent re-injection must not duplicate claims.
	if _, err := InjectPodSpecClaims(cloned, nil, "cd-replica-0"); err != nil {
		t.Fatalf("second InjectPodSpecClaims failed: %v", err)
	}
	if len(cloned.ResourceClaims) != 1 || len(cloned.Containers[0].Resources.Claims) != 1 {
		t.Fatalf("re-injection duplicated claims: pod=%d container=%d", len(cloned.ResourceClaims), len(cloned.Containers[0].Resources.Claims))
	}

	// Explicit compute-domain-containers override works for DRA-allocated GPU containers with 0 nvidia.com/gpu.
	draPodSpec := &corev1.PodSpec{
		Containers: []corev1.Container{
			{Name: "dra-vllm"},
			{Name: "metrics"},
		},
	}
	ok, err := InjectPodSpecClaims(draPodSpec, map[string]string{
		leaderworkersetv1.ComputeDomainContainersAnnotationKey: "dra-vllm",
	}, "cd-dra")
	if err != nil || !ok {
		t.Fatalf("expected explicit container targeting to succeed, ok=%v err=%v", ok, err)
	}
	if len(draPodSpec.Containers[0].Resources.Claims) != 1 || len(draPodSpec.Containers[1].Resources.Claims) != 0 {
		t.Fatalf("unexpected explicit container claims: %+v", draPodSpec.Containers)
	}

	// Missing container in compute-domain-containers returns error.
	if _, err := TargetedContainerNames(draPodSpec, map[string]string{
		leaderworkersetv1.ComputeDomainContainersAnnotationKey: "nonexistent",
	}); err == nil {
		t.Fatalf("expected error for unknown container in compute-domain-containers")
	}

	// Zero-GPU pod template receives neither container claims nor pod-level ResourceClaims.
	cpuOnly := &corev1.PodSpec{
		Containers: []corev1.Container{{Name: "router"}},
	}
	ok, err = InjectPodSpecClaims(cpuOnly, nil, "cd-unused")
	if err != nil || ok || len(cpuOnly.ResourceClaims) != 0 {
		t.Fatalf("expected zero-GPU podSpec to remain untouched, ok=%v err=%v claims=%+v", ok, err, cpuOnly.ResourceClaims)
	}
}

func TestInjectPodClaimsCPULeaderGPUWorker(t *testing.T) {
	// CPU-only leader still gets compute-domain-name stamped so GPU workers join the replica domain,
	// while the CPU leader itself gets zero ResourceClaims.
	leader := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "lws-0",
			Namespace: "default",
			Labels: map[string]string{
				leaderworkersetv1.SetNameLabelKey:     "lws",
				leaderworkersetv1.WorkerIndexLabelKey: "0",
				leaderworkersetv1.GroupIndexLabelKey:  "0",
				leaderworkersetv1.RevisionKey:         "rev1",
			},
			Annotations: map[string]string{
				leaderworkersetv1.ComputeDomainPrefixAnnotationKey: "lws-12345",
			},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "cpu-coordinator"}},
		},
	}
	if err := InjectPodClaims(leader); err != nil {
		t.Fatalf("InjectPodClaims on CPU leader failed: %v", err)
	}
	cdName := leader.Annotations[leaderworkersetv1.ComputeDomainNameAnnotationKey]
	if cdName == "" {
		t.Fatalf("expected compute-domain-name to be stamped on CPU leader")
	}
	if len(leader.Spec.ResourceClaims) != 0 {
		t.Fatalf("expected zero ResourceClaims on CPU-only leader, got %+v", leader.Spec.ResourceClaims)
	}

	// Reinvoking webhook preserves the exact incarnation suffix.
	if err := InjectPodClaims(leader); err != nil {
		t.Fatalf("second InjectPodClaims on leader failed: %v", err)
	}
	if got := leader.Annotations[leaderworkersetv1.ComputeDomainNameAnnotationKey]; got != cdName {
		t.Fatalf("reinvocation changed compute-domain-name: got %q, want %q", got, cdName)
	}

	// GPU worker carrying compute-domain-name receives matching claim.
	worker := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "lws-0-1",
			Namespace: "default",
			Labels: map[string]string{
				leaderworkersetv1.SetNameLabelKey:     "lws",
				leaderworkersetv1.WorkerIndexLabelKey: "1",
			},
			Annotations: map[string]string{
				leaderworkersetv1.ComputeDomainNameAnnotationKey: cdName,
			},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "gpu-worker", Resources: gpuResources("4")}},
		},
	}
	if err := InjectPodClaims(worker); err != nil {
		t.Fatalf("InjectPodClaims on GPU worker failed: %v", err)
	}
	if !PodHasComputeDomainClaim(worker) || *worker.Spec.ResourceClaims[0].ResourceClaimTemplateName != cdName {
		t.Fatalf("worker did not receive matching ComputeDomain claim: %+v", worker.Spec.ResourceClaims)
	}
}

func TestVerifyAPI(t *testing.T) {
	emptyMapper := apimeta.NewDefaultRESTMapper([]schema.GroupVersion{corev1.SchemeGroupVersion})
	if err := VerifyAPI(emptyMapper); err == nil {
		t.Fatalf("expected VerifyAPI to fail when ComputeDomain GVK is absent")
	}

	withCD := apimeta.NewDefaultRESTMapper([]schema.GroupVersion{GroupVersion})
	withCD.Add(GroupVersion.WithKind(Kind), apimeta.RESTScopeNamespace)
	if err := VerifyAPI(withCD); err != nil {
		t.Fatalf("expected VerifyAPI to succeed when ComputeDomain is mapped, got %v", err)
	}
}

func TestEnsureCleanupAndReconcilerLifecycle(t *testing.T) {
	ctx := context.Background()
	lws := &leaderworkersetv1.LeaderWorkerSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "model",
			Namespace: "default",
			UID:       types.UID("lws-uid-1"),
			Annotations: map[string]string{
				leaderworkersetv1.AutoComputeDomainAnnotationKey: leaderworkersetv1.AutoComputeDomainEnabled,
			},
		},
		Spec: leaderworkersetv1.LeaderWorkerSetSpec{
			LeaderWorkerTemplate: leaderworkersetv1.LeaderWorkerTemplate{
				WorkerTemplate: corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{{Name: "gpu", Resources: gpuResources("2")}},
					},
				},
			},
		},
	}

	cdName := "model-12345-rev1-0-inc01"
	leaderPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "model-0",
			Namespace: "default",
			UID:       types.UID("leader-uid-1"),
			Labels: map[string]string{
				leaderworkersetv1.SetNameLabelKey:     "model",
				leaderworkersetv1.WorkerIndexLabelKey: "0",
				leaderworkersetv1.GroupIndexLabelKey:  "0",
				leaderworkersetv1.RevisionKey:         "rev1",
			},
			Annotations: map[string]string{
				leaderworkersetv1.ComputeDomainNameAnnotationKey: cdName,
			},
		},
		Spec: corev1.PodSpec{
			SchedulingGates: []corev1.PodSchedulingGate{{Name: leaderworkersetv1.GroupReplacementSchedulingGate}},
			ResourceClaims: []corev1.PodResourceClaim{{
				Name:                      ChannelClaimName,
				ResourceClaimTemplateName: ptr.To(cdName),
			}},
		},
	}

	c := newFakeClient(t, lws, leaderPod)
	if err := EnsureReplicaComputeDomain(ctx, c, lws, leaderPod); err != nil {
		t.Fatalf("EnsureReplicaComputeDomain failed: %v", err)
	}

	cd := &ComputeDomain{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: "default", Name: cdName}, cd); err != nil {
		t.Fatalf("expected ComputeDomain %s to exist: %v", cdName, err)
	}
	if cd.Spec.NumNodes != 0 || cd.Spec.Channel == nil || cd.Spec.Channel.ResourceClaimTemplate.Name != cdName {
		t.Fatalf("unexpected ComputeDomain spec: %+v", cd.Spec)
	}
	if !controllerutil.ContainsFinalizer(cd, leaderworkersetv1.ComputeDomainProtectionFinalizer) {
		t.Fatalf("expected protection finalizer on ComputeDomain")
	}
	if !metav1.IsControlledBy(cd, leaderPod) {
		t.Fatalf("expected ComputeDomain to be controlled by the leader Pod")
	}
	if ref := metav1.GetControllerOf(cd); ref.BlockOwnerDeletion == nil || *ref.BlockOwnerDeletion {
		t.Fatalf("expected the leader Pod owner reference not to block owner deletion: %+v", ref)
	}
	// Verify DeepCopy works cleanly.
	if copyObj := cd.DeepCopyObject(); copyObj == nil {
		t.Fatalf("expected non-nil DeepCopyObject")
	}

	// A leader Pod with another UID does not adopt the ComputeDomain.
	otherLeader := leaderPod.DeepCopy()
	otherLeader.UID = types.UID("different-uid")
	if err := EnsureReplicaComputeDomain(ctx, c, lws, otherLeader); !errors.Is(err, ErrUnexpectedComputeDomainOwner) {
		t.Fatalf("expected ErrUnexpectedComputeDomainOwner, got %v", err)
	}

	// Suppose replica 0's leader is replaced by a new incarnation while a worker pod still holds cdName.
	workerHoldingOldCD := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "model-0-1",
			Namespace: "default",
		},
		Spec: corev1.PodSpec{
			ResourceClaims: []corev1.PodResourceClaim{{
				Name:                      ChannelClaimName,
				ResourceClaimTemplateName: ptr.To(cdName),
			}},
		},
	}
	if err := c.Create(ctx, workerHoldingOldCD); err != nil {
		t.Fatalf("create worker pod: %v", err)
	}
	if err := c.Delete(ctx, leaderPod); err != nil {
		t.Fatalf("delete old leader pod: %v", err)
	}

	// Garbage collection deletes the ComputeDomain once its leader Pod is gone.
	// The finalizer holds it while workerHoldingOldCD still references it.
	if err := c.Delete(ctx, cd); err != nil {
		t.Fatalf("delete ComputeDomain as garbage collection would: %v", err)
	}
	r := NewReconciler(c)
	cdReq := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: cdName}}
	if _, err := r.Reconcile(ctx, cdReq); err != nil {
		t.Fatalf("Reconcile while worker references the ComputeDomain: %v", err)
	}
	if err := c.Get(ctx, types.NamespacedName{Namespace: "default", Name: cdName}, cd); err != nil {
		t.Fatalf("expected ComputeDomain to remain while worker pod references it: %v", err)
	}

	// Once the worker pod disappears, the finalizer is released.
	if err := c.Delete(ctx, workerHoldingOldCD); err != nil {
		t.Fatalf("delete worker pod: %v", err)
	}
	if _, err := r.Reconcile(ctx, cdReq); err != nil {
		t.Fatalf("Reconcile after worker deletion: %v", err)
	}
	if err := c.Get(ctx, types.NamespacedName{Namespace: "default", Name: cdName}, cd); !apierrors.IsNotFound(err) {
		t.Fatalf("expected ComputeDomain to be removed once unreferenced, got err=%v", err)
	}

	// Test DisaggregatedSet slice lifecycle + Reconciler orphan cleanup when parent DS is deleted.
	ds := &disaggregatedsetv1.DisaggregatedSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "disagg",
			Namespace: "default",
			UID:       types.UID("ds-uid-1"),
			Annotations: map[string]string{
				disaggregatedsetv1.AutoComputeDomainAnnotationKey: leaderworkersetv1.AutoComputeDomainEnabled,
			},
		},
		Spec: disaggregatedsetv1.DisaggregatedSetSpec{
			Roles: []disaggregatedsetv1.DisaggregatedRoleSpec{{
				Name: "prefill",
				LeaderWorkerSetTemplateSpec: leaderworkersetv1.LeaderWorkerSetTemplateSpec{
					Spec: lws.Spec,
				},
			}},
		},
	}
	if err := c.Create(ctx, ds); err != nil {
		t.Fatalf("create DisaggregatedSet: %v", err)
	}
	if err := EnsureSliceComputeDomain(ctx, c, ds, 0, "revA", false); err != nil {
		t.Fatalf("EnsureSliceComputeDomain failed: %v", err)
	}
	sliceCDName := SliceComputeDomainName(ds, 0, "revA")
	slicePod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "prefill-0", Namespace: "default"},
		Spec: corev1.PodSpec{
			ResourceClaims: []corev1.PodResourceClaim{{
				Name:                      ChannelClaimName,
				ResourceClaimTemplateName: ptr.To(sliceCDName),
			}},
		},
	}
	if err := c.Create(ctx, slicePod); err != nil {
		t.Fatalf("create slice pod: %v", err)
	}

	// Garbage collection of the deleted DisaggregatedSet deletes the slice
	// ComputeDomain while slicePod is still terminating.
	sliceCD := &ComputeDomain{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: "default", Name: sliceCDName}, sliceCD); err != nil {
		t.Fatalf("get slice ComputeDomain: %v", err)
	}
	if err := c.Delete(ctx, sliceCD); err != nil {
		t.Fatalf("delete slice ComputeDomain as garbage collection would: %v", err)
	}
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: sliceCDName}}); err != nil {
		t.Fatalf("Reconciler.Reconcile while pod active failed: %v", err)
	}
	if err := c.Get(ctx, types.NamespacedName{Namespace: "default", Name: sliceCDName}, sliceCD); err != nil {
		t.Fatalf("expected slice ComputeDomain to remain while slicePod exists: %v", err)
	}

	// Delete slicePod and reconcile again -> slice ComputeDomain is cleaned up.
	if err := c.Delete(ctx, slicePod); err != nil {
		t.Fatalf("delete slicePod: %v", err)
	}
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: sliceCDName}}); err != nil {
		t.Fatalf("Reconciler.Reconcile after pod deleted failed: %v", err)
	}
	if err := c.Get(ctx, types.NamespacedName{Namespace: "default", Name: sliceCDName}, sliceCD); !apierrors.IsNotFound(err) {
		t.Fatalf("expected orphaned slice ComputeDomain to be deleted, got err=%v", err)
	}
}

type sliceQueue struct {
	reqs []reconcile.Request
}

func (q *sliceQueue) Add(req reconcile.Request) {
	q.reqs = append(q.reqs, req)
}

func TestComputeDomainEdgeCasesAndHelpers(t *testing.T) {
	// VerifyAPI nil, missing, and present cases.
	if err := VerifyAPI(nil); err != nil {
		t.Fatalf("VerifyAPI(nil) failed: %v", err)
	}
	emptyMapper := apimeta.NewDefaultRESTMapper([]schema.GroupVersion{{Group: "", Version: "v1"}})
	if err := VerifyAPI(emptyMapper); err == nil {
		t.Fatalf("expected VerifyAPI to fail when ComputeDomain GVK is absent")
	}
	cdMapper := apimeta.NewDefaultRESTMapper([]schema.GroupVersion{GroupVersion})
	cdMapper.Add(GroupVersion.WithKind(Kind), apimeta.RESTScopeNamespace)
	if err := VerifyAPI(cdMapper); err != nil {
		t.Fatalf("VerifyAPI failed when ComputeDomain GVK is served: %v", err)
	}

	// DeepCopy coverage for ComputeDomain and ComputeDomainList.
	var nilCD *ComputeDomain
	if nilCD.DeepCopy() != nil || nilCD.DeepCopyObject() != nil {
		t.Fatalf("expected nil ComputeDomain DeepCopy to return nil")
	}
	var nilList *ComputeDomainList
	if nilList.DeepCopy() != nil || nilList.DeepCopyObject() != nil {
		t.Fatalf("expected nil ComputeDomainList DeepCopy to return nil")
	}
	list := &ComputeDomainList{
		Items: []ComputeDomain{{
			ObjectMeta: metav1.ObjectMeta{Name: "cd-1"},
			Spec: ComputeDomainSpec{
				NumNodes: 0,
				Channel: &ComputeDomainChannelSpec{
					ResourceClaimTemplate: ComputeDomainResourceClaimTemplate{Name: "cd-1"},
				},
			},
		}},
	}
	copiedObj := list.DeepCopyObject().(*ComputeDomainList)
	copiedObj.Items[0].Spec.Channel.ResourceClaimTemplate.Name = "mutated"
	if list.Items[0].Spec.Channel.ResourceClaimTemplate.Name != "cd-1" {
		t.Fatalf("expected DeepCopyObject to isolate nested Channel struct")
	}

	ctx := context.Background()
	scheme := testScheme(t)
	builder := fake.NewClientBuilder().WithScheme(scheme)
	c := builder.WithIndex(&corev1.Pod{}, PodClaimTemplateIndex, PodClaimTemplateNames).Build()

	lws := &leaderworkersetv1.LeaderWorkerSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "model",
			Namespace: "default",
			UID:       types.UID("uid-a"),
			Annotations: map[string]string{
				leaderworkersetv1.AutoComputeDomainAnnotationKey: leaderworkersetv1.AutoComputeDomainEnabled,
			},
		},
	}
	if err := c.Create(ctx, lws); err != nil {
		t.Fatalf("create LWS: %v", err)
	}

	cdName := ReplicaComputeDomainName(UIDPrefix(lws.Name, lws.UID), "rev1", "0", "inc1")
	// Pre-create an unowned ComputeDomain with the same name -> must return ErrUnexpectedComputeDomainOwner.
	conflictCD := &ComputeDomain{
		ObjectMeta: metav1.ObjectMeta{Name: cdName, Namespace: "default"},
	}
	if err := c.Create(ctx, conflictCD); err != nil {
		t.Fatalf("create conflict CD: %v", err)
	}
	leader := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "model-0",
			Namespace: "default",
			UID:       types.UID("leader-uid-a"),
			Labels: map[string]string{
				leaderworkersetv1.SetNameLabelKey:     "model",
				leaderworkersetv1.WorkerIndexLabelKey: "0",
				leaderworkersetv1.GroupIndexLabelKey:  "0",
				leaderworkersetv1.RevisionKey:         "rev1",
			},
			Annotations: map[string]string{
				leaderworkersetv1.ComputeDomainNameAnnotationKey: cdName,
			},
		},
		Spec: corev1.PodSpec{
			SchedulingGates: []corev1.PodSchedulingGate{{Name: leaderworkersetv1.GroupReplacementSchedulingGate}},
		},
	}
	if err := EnsureReplicaComputeDomain(ctx, c, lws, leader); !errors.Is(err, ErrUnexpectedComputeDomainOwner) {
		t.Fatalf("expected ErrUnexpectedComputeDomainOwner, got %v", err)
	}
	_ = c.Delete(ctx, conflictCD)

	// Owned ComputeDomain missing finalizer & labels is repaired via metadata Patch, and ensure succeeds once ungated.
	ownerRef := *metav1.NewControllerRef(leader, corev1.SchemeGroupVersion.WithKind("Pod"))
	bareCD := &ComputeDomain{
		ObjectMeta: metav1.ObjectMeta{
			Name:            cdName,
			Namespace:       "default",
			OwnerReferences: []metav1.OwnerReference{ownerRef},
		},
	}
	if err := c.Create(ctx, bareCD); err != nil {
		t.Fatalf("create bare CD: %v", err)
	}
	if err := EnsureReplicaComputeDomain(ctx, c, lws, leader); err != nil {
		t.Fatalf("EnsureReplicaComputeDomain metadata repair failed: %v", err)
	}
	repaired := &ComputeDomain{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: "default", Name: cdName}, repaired); err != nil {
		t.Fatalf("get repaired CD: %v", err)
	}
	if !controllerutil.ContainsFinalizer(repaired, leaderworkersetv1.ComputeDomainProtectionFinalizer) ||
		repaired.Labels[leaderworkersetv1.SetNameLabelKey] != "model" {
		t.Fatalf("expected metadata repair to restore finalizer and labels: %+v", repaired.ObjectMeta)
	}
	leader.Spec.SchedulingGates = nil
	if err := EnsureReplicaComputeDomain(ctx, c, lws, leader); err != nil {
		t.Fatalf("EnsureReplicaComputeDomain for an admitted leader failed: %v", err)
	}

	// Reconciler leaves a ComputeDomain that is not terminating alone.
	r := NewReconciler(c)
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: cdName}}); err != nil {
		t.Fatalf("Reconcile of a live ComputeDomain failed: %v", err)
	}
	if err := c.Get(ctx, types.NamespacedName{Namespace: "default", Name: cdName}, repaired); err != nil ||
		!controllerutil.ContainsFinalizer(repaired, leaderworkersetv1.ComputeDomainProtectionFinalizer) {
		t.Fatalf("expected a live ComputeDomain to keep its finalizer, err=%v", err)
	}
	q := &sliceQueue{}
	enqueuePodReferencedComputeDomains(&corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "default"},
		Spec: corev1.PodSpec{
			ResourceClaims: []corev1.PodResourceClaim{{
				Name:                      ChannelClaimName,
				ResourceClaimTemplateName: ptr.To(cdName),
			}},
		},
	}, q)
	if len(q.reqs) != 1 || q.reqs[0].Name != cdName {
		t.Fatalf("unexpected enqueuePodReferencedComputeDomains result: %+v", q.reqs)
	}

	// CleanupDisaggregatedSetComputeDomains retains draining child LWS revision and deletes drained revision.
	ds := &disaggregatedsetv1.DisaggregatedSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "serving",
			Namespace: "default",
			UID:       types.UID("ds-uid-edge"),
			Annotations: map[string]string{
				disaggregatedsetv1.AutoComputeDomainAnnotationKey: leaderworkersetv1.AutoComputeDomainEnabled,
			},
		},
		Spec: disaggregatedsetv1.DisaggregatedSetSpec{
			Roles: []disaggregatedsetv1.DisaggregatedRoleSpec{{
				Name: "prefill",
				LeaderWorkerSetTemplateSpec: leaderworkersetv1.LeaderWorkerSetTemplateSpec{
					Spec: leaderworkersetv1.LeaderWorkerSetSpec{
						LeaderWorkerTemplate: leaderworkersetv1.LeaderWorkerTemplate{
							WorkerTemplate: corev1.PodTemplateSpec{
								Spec: corev1.PodSpec{
									Containers: []corev1.Container{{
										Name: "vllm",
										Resources: corev1.ResourceRequirements{
											Limits: corev1.ResourceList{NvidiaGPUResourceName: resource.MustParse("1")},
										},
									}},
								},
							},
						},
					},
				},
			}},
		},
	}
	if err := EnsureSliceComputeDomain(ctx, c, ds, 0, "oldrev", false); err != nil {
		t.Fatalf("EnsureSliceComputeDomain oldrev: %v", err)
	}
	if err := EnsureSliceComputeDomain(ctx, c, ds, 0, "newrev", false); err != nil {
		t.Fatalf("EnsureSliceComputeDomain newrev: %v", err)
	}
	drainingChild := &leaderworkersetv1.LeaderWorkerSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "serving-0-oldrev-prefill",
			Namespace: "default",
			Labels: map[string]string{
				disaggregatedsetv1.SliceLabelKey:    "0",
				disaggregatedsetv1.RevisionLabelKey: "oldrev",
			},
		},
	}
	if err := CleanupDisaggregatedSetComputeDomains(ctx, c, ds, []*leaderworkersetv1.LeaderWorkerSet{drainingChild}, 1, "newrev"); err != nil {
		t.Fatalf("CleanupDisaggregatedSetComputeDomains with draining child: %v", err)
	}
	oldSliceName := SliceComputeDomainName(ds, 0, "oldrev")
	if err := c.Get(ctx, types.NamespacedName{Namespace: "default", Name: oldSliceName}, &ComputeDomain{}); err != nil {
		t.Fatalf("expected draining revision ComputeDomain to be retained: %v", err)
	}
	if err := CleanupDisaggregatedSetComputeDomains(ctx, c, ds, nil, 1, "newrev"); err != nil {
		t.Fatalf("CleanupDisaggregatedSetComputeDomains after drain: %v", err)
	}
	drained := &ComputeDomain{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: "default", Name: oldSliceName}, drained); err != nil {
		t.Fatalf("get drained revision ComputeDomain: %v", err)
	}
	if drained.DeletionTimestamp.IsZero() {
		t.Fatalf("expected drained revision ComputeDomain to be deleted, leaving the finalizer to hold it")
	}
	newSliceName := SliceComputeDomainName(ds, 0, "newrev")
	current := &ComputeDomain{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: "default", Name: newSliceName}, current); err != nil || !current.DeletionTimestamp.IsZero() {
		t.Fatalf("expected current revision ComputeDomain to be kept, err=%v", err)
	}
}

func terminatingTestFixtures() (*leaderworkersetv1.LeaderWorkerSet, *ComputeDomain, *corev1.Pod) {
	leaderUID := types.UID("leader-uid-1")
	lws := &leaderworkersetv1.LeaderWorkerSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "model",
			Namespace: "default",
			UID:       types.UID("lws-uid-1"),
			Annotations: map[string]string{
				leaderworkersetv1.AutoComputeDomainAnnotationKey: leaderworkersetv1.AutoComputeDomainEnabled,
			},
		},
		Spec: leaderworkersetv1.LeaderWorkerSetSpec{
			LeaderWorkerTemplate: leaderworkersetv1.LeaderWorkerTemplate{
				WorkerTemplate: corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{{Name: "gpu", Resources: gpuResources("2")}},
					},
				},
			},
		},
	}
	cdName := "model-12345-rev1-0-inc01"
	cd := &ComputeDomain{
		ObjectMeta: metav1.ObjectMeta{
			Name:              cdName,
			Namespace:         "default",
			DeletionTimestamp: ptr.To(metav1.Now()),
			Finalizers:        []string{leaderworkersetv1.ComputeDomainProtectionFinalizer},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "v1",
				Kind:       "Pod",
				Name:       "model-0",
				UID:        leaderUID,
				Controller: ptr.To(true),
			}},
		},
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "model-0",
			Namespace: "default",
			UID:       leaderUID,
			Labels: map[string]string{
				leaderworkersetv1.SetNameLabelKey:     "model",
				leaderworkersetv1.WorkerIndexLabelKey: "0",
				leaderworkersetv1.GroupIndexLabelKey:  "0",
				leaderworkersetv1.RevisionKey:         "rev1",
			},
			Annotations: map[string]string{
				leaderworkersetv1.ComputeDomainNameAnnotationKey: cdName,
			},
		},
		Spec: corev1.PodSpec{
			ResourceClaims: []corev1.PodResourceClaim{{
				Name:                      ChannelClaimName,
				ResourceClaimTemplateName: ptr.To(cdName),
			}},
		},
	}
	return lws, cd, pod
}

func TestEnsureReplicaComputeDomainTerminating(t *testing.T) {
	ctx := context.Background()

	t.Run("gated leader waits for the terminating ComputeDomain", func(t *testing.T) {
		lws, cd, leader := terminatingTestFixtures()
		leader.Spec.SchedulingGates = []corev1.PodSchedulingGate{{Name: leaderworkersetv1.GroupReplacementSchedulingGate}}
		c := newFakeClient(t, lws, cd, leader)
		if err := EnsureReplicaComputeDomain(ctx, c, lws, leader); !errors.Is(err, ErrComputeDomainTerminating) {
			t.Fatalf("expected ErrComputeDomainTerminating for a gated leader, got %v", err)
		}
	})

	t.Run("admitted leader is not blocked by the terminating ComputeDomain", func(t *testing.T) {
		lws, cd, leader := terminatingTestFixtures()
		c := newFakeClient(t, lws, cd, leader)
		if err := EnsureReplicaComputeDomain(ctx, c, lws, leader); err != nil {
			t.Fatalf("expected no error for an admitted leader, got %v", err)
		}
	})
}

func TestReconcilerReleasesTerminatingComputeDomain(t *testing.T) {
	ctx := context.Background()
	lws, cd, pod := terminatingTestFixtures()
	c := newFakeClient(t, lws, cd, pod)
	r := NewReconciler(c)
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: cd.Namespace, Name: cd.Name}}

	// The ComputeDomain was deleted (by garbage collection or by hand) while a
	// pod still references it, so the finalizer must hold.
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("Reconcile while referenced: %v", err)
	}
	got := &ComputeDomain{}
	if err := c.Get(ctx, req.NamespacedName, got); err != nil {
		t.Fatalf("expected ComputeDomain to remain while a pod references it: %v", err)
	}
	if !controllerutil.ContainsFinalizer(got, leaderworkersetv1.ComputeDomainProtectionFinalizer) {
		t.Fatalf("expected protection finalizer to remain while a pod references the ComputeDomain")
	}

	if err := c.Delete(ctx, pod); err != nil {
		t.Fatalf("delete pod: %v", err)
	}
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("Reconcile after pod deletion: %v", err)
	}
	if err := c.Get(ctx, req.NamespacedName, got); !apierrors.IsNotFound(err) {
		t.Fatalf("expected ComputeDomain to be released and removed once unreferenced, got err=%v", err)
	}
}

func TestEnsureSliceComputeDomainTerminating(t *testing.T) {
	ctx := context.Background()
	ds := &disaggregatedsetv1.DisaggregatedSet{
		ObjectMeta: metav1.ObjectMeta{Name: "serving", Namespace: "default", UID: types.UID("ds-uid-term")},
	}
	cdName := SliceComputeDomainName(ds, 0, "rev1")
	terminating := &ComputeDomain{
		ObjectMeta: metav1.ObjectMeta{
			Name:              cdName,
			Namespace:         "default",
			DeletionTimestamp: ptr.To(metav1.Now()),
			Finalizers:        []string{leaderworkersetv1.ComputeDomainProtectionFinalizer},
			OwnerReferences:   []metav1.OwnerReference{*metav1.NewControllerRef(ds, disaggregatedsetv1.GroupVersion.WithKind("DisaggregatedSet"))},
		},
	}

	t.Run("slice without children waits for the terminating ComputeDomain", func(t *testing.T) {
		c := newFakeClient(t, ds.DeepCopy(), terminating.DeepCopy())
		if err := EnsureSliceComputeDomain(ctx, c, ds, 0, "rev1", false); !errors.Is(err, ErrComputeDomainTerminating) {
			t.Fatalf("expected ErrComputeDomainTerminating, got %v", err)
		}
	})

	t.Run("slice with running children is not blocked", func(t *testing.T) {
		c := newFakeClient(t, ds.DeepCopy(), terminating.DeepCopy())
		if err := EnsureSliceComputeDomain(ctx, c, ds, 0, "rev1", true); err != nil {
			t.Fatalf("expected no error for a slice with children, got %v", err)
		}
	})
}

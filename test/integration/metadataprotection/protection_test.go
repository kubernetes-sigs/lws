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

package metadataprotection

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/rest"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

const manager = "system:serviceaccount:lws-system:lws-controller-manager"
const other = "system:serviceaccount:other-system:other-controller"
const scheduler = "system:serviceaccount:volcano-system:volcano-scheduler"
const ns = "metadata-protection-test"

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func assertDenied(t *testing.T, err error) {
	t.Helper()
	if !apierrors.IsForbidden(err) || !strings.Contains(err.Error(), "LWS/DisaggregatedSet") {
		t.Fatalf("expected metadata policy rejection, got %v", err)
	}
}

func render(t *testing.T, name string, args ...string) []byte {
	t.Helper()
	cmd := toolCommand(name, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("%s: %v\n%s", name, err, stderr.String())
	}
	return out
}

func toolCommand(name string, args ...string) *exec.Cmd {
	if configured := os.Getenv(strings.ToUpper(name)); configured != "" {
		name = configured
	}
	return exec.Command(name, args...)
}

func decode(t *testing.T, data []byte) []*unstructured.Unstructured {
	t.Helper()
	decoder := yaml.NewYAMLOrJSONDecoder(bytes.NewReader(data), 4096)
	var objects []*unstructured.Unstructured
	for {
		obj := &unstructured.Unstructured{}
		err := decoder.Decode(obj)
		if err == io.EOF {
			break
		}
		must(t, err)
		if obj.GetKind() != "" {
			objects = append(objects, obj)
		}
	}
	return objects
}

func installPolicy(t *testing.T, ctx context.Context, c client.Client, root string) {
	t.Helper()
	var installed int
	for _, obj := range decode(t, render(t, "kustomize", "build", filepath.Join(root, "config/default"))) {
		if obj.GetKind() == "ValidatingAdmissionPolicy" || obj.GetKind() == "ValidatingAdmissionPolicyBinding" || obj.GetName() == "lws-metadata-writer" {
			must(t, c.Create(ctx, obj))
			installed++
		}
	}
	if installed != 4 {
		t.Fatalf("expected policy, binding and manager permission; installed %d resources", installed)
	}
}

func TestMetadataProtection(t *testing.T) {
	ctx := context.Background()
	root := filepath.Join("..", "..", "..")
	kubernetesVersion := os.Getenv("LWS_METADATA_TEST_K8S_VERSION")
	if kubernetesVersion == "" {
		kubernetesVersion = "1.37.0"
	}
	env := &envtest.Environment{
		UseExistingCluster:          ptr.To(false),
		DownloadBinaryAssets:        os.Getenv("KUBEBUILDER_ASSETS") == "",
		DownloadBinaryAssetsVersion: kubernetesVersion,
		BinaryAssetsDirectory:       filepath.Join(root, "bin"),
		CRDDirectoryPaths:           []string{filepath.Join(root, "config/crd/bases")},
		ErrorIfCRDPathMissing:       true,
		ControlPlaneStartTimeout:    60 * time.Second,
	}
	authorizations := countMetadataAuthorizations(t, env)
	cfg, err := env.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := env.Stop(); err != nil {
			t.Error(err)
		}
	})
	admin, err := client.New(cfg, client.Options{})
	if err != nil {
		t.Fatal(err)
	}

	must(t, admin.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}))
	// Both request identities have the same broad resource rights within this
	// disposable namespace. Admission, rather than RBAC, must distinguish writes.
	must(t, admin.Create(ctx, &rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Name: "writers", Namespace: ns}, Rules: []rbacv1.PolicyRule{{APIGroups: []string{"*"}, Resources: []string{"*"}, Verbs: []string{"*"}}}}))
	must(t, admin.Create(ctx, &rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "writers", Namespace: ns}, RoleRef: rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "Role", Name: "writers"}, Subjects: []rbacv1.Subject{{Kind: "User", APIGroup: "rbac.authorization.k8s.io", Name: manager}, {Kind: "User", APIGroup: "rbac.authorization.k8s.io", Name: other}, {Kind: "User", APIGroup: "rbac.authorization.k8s.io", Name: "system:serviceaccount:kube-system:statefulset-controller"}, {Kind: "User", APIGroup: "rbac.authorization.k8s.io", Name: "system:serviceaccount:kube-system:replicaset-controller"}, {Kind: "User", APIGroup: "rbac.authorization.k8s.io", Name: "system:serviceaccount:kube-system:deployment-controller"}, {Kind: "User", APIGroup: "rbac.authorization.k8s.io", Name: "system:kube-controller-manager"}}}))
	actor := func(name string) client.Client {
		t.Helper()
		copy := rest.CopyConfig(cfg)
		copy.Impersonate = rest.ImpersonationConfig{UserName: name}
		c, e := client.New(copy, client.Options{})
		must(t, e)
		return c
	}
	allowed, denied := actor(manager), actor(other)
	must(t, admin.Create(ctx, &rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "scheduler", Namespace: ns}, RoleRef: rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "Role", Name: "writers"}, Subjects: []rbacv1.Subject{{Kind: "User", APIGroup: "rbac.authorization.k8s.io", Name: scheduler}}}))
	installPolicy(t, ctx, admin, root)

	const labelKey = "leaderworkerset.sigs.k8s.io/name"

	newPod := func(name string) *corev1.Pod {
		return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}, Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "main", Image: "registry.k8s.io/pause:3.10"}}}}
	}
	patch := func(c client.Client, obj client.Object, body string) error {
		return c.Patch(ctx, obj, client.RawPatch(types.MergePatchType, []byte(body)))
	}

	warmup := newPod("warmup")
	warmup.Labels = map[string]string{labelKey: "original"}
	must(t, admin.Create(ctx, warmup))
	deadline := time.Now().Add(30 * time.Second)
	for {
		err = patch(denied, warmup, fmt.Sprintf(`{"metadata":{"labels":{%q:%q}}}`, labelKey, fmt.Sprint(time.Now().UnixNano())))
		if apierrors.IsForbidden(err) && strings.Contains(err.Error(), "LWS/DisaggregatedSet") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("policy did not become effective: %v", err)
		}
		time.Sleep(100 * time.Millisecond)
	}

	t.Run("unchanged_metadata_skips_authorizer", func(t *testing.T) {
		p := newPod("unchanged-metadata")
		p.Labels = map[string]string{labelKey: "original"}
		p.Annotations = map[string]string{"leaderworkerset.sigs.k8s.io/size": "2"}
		must(t, allowed.Create(ctx, p))
		before := authorizations.Load()
		must(t, denied.Create(ctx, newPod("ordinary-pod")))
		must(t, patch(denied, p, `{"metadata":{"labels":{"app":"changed"},"annotations":{"leaderworkerset.sigs.k8s.io/recover":"true"}}}`))
		must(t, denied.Status().Patch(ctx, p, client.RawPatch(types.MergePatchType, []byte(`{"status":{"phase":"Pending"}}`))))
		native := newPod("native-no-authorization")
		native.Labels = p.Labels
		must(t, actor("system:serviceaccount:kube-system:statefulset-controller").Create(ctx, native))
		if got := authorizations.Load() - before; got != 0 {
			t.Fatalf("unchanged metadata/native creation caused %d authorization webhook calls", got)
		}
		assertDenied(t, patch(denied, p, fmt.Sprintf(`{"metadata":{"labels":{%q:"changed"}}}`, labelKey)))
		if authorizations.Load() == before {
			t.Fatal("a protected change did not consult the authorization webhook")
		}
	})
	t.Run("all_public_annotation_exceptions_remain_editable", func(t *testing.T) {
		for i, key := range []string{
			"leaderworkerset.sigs.k8s.io/exclusive-topology",
			"leaderworkerset.sigs.k8s.io/subgroup-exclusive-topology",
			"leaderworkerset.sigs.k8s.io/experimental-recreate-group-after-start",
			"leaderworkerset.sigs.k8s.io/recover",
			"leaderworkerset.sigs.k8s.io/auto-compute-domain",
			"leaderworkerset.sigs.k8s.io/compute-domain-containers",
			"disaggregatedset.x-k8s.io/auto-compute-domain",
		} {
			p := newPod(fmt.Sprintf("public-annotation-%d", i))
			p.Annotations = map[string]string{key: "initial"}
			must(t, denied.Create(ctx, p))
			must(t, patch(denied, p, fmt.Sprintf(`{"metadata":{"annotations":{%q:"changed"}}}`, key)))
			must(t, patch(denied, p, fmt.Sprintf(`{"metadata":{"annotations":{%q:null}}}`, key)))
			assertDenied(t, patch(denied, p, fmt.Sprintf(`{"metadata":{"labels":{%q:""}}}`, key)))
		}
	})
	t.Run("prefixes_require_the_exact_domain_and_slash", func(t *testing.T) {
		p := newPod("similar-prefixes")
		p.Labels = map[string]string{
			"leaderworkerset.sigs.k8s.io.example/name": "initial",
			"disaggregatedset.x-k8s.io.example/name":   "initial",
			"leaderworkerset.sigs.k8s.io":              "initial",
			"example.com/leaderworkerset.sigs.k8s.io":  "initial",
		}
		p.Annotations = map[string]string{corev1.PodDeletionCost: "20", "example.com/owner": "other"}
		must(t, denied.Create(ctx, p))
		must(t, patch(denied, p, `{"metadata":{"labels":null,"annotations":null}}`))
	})
	t.Run("write_routes_and_whole_map_removal", func(t *testing.T) {
		p := newPod("routes")
		key := labelKey
		p.Labels = map[string]string{key: "original"}
		must(t, admin.Create(ctx, p))
		changed := p.DeepCopy()
		changed.Labels[key] = "changed"
		assertDenied(t, denied.Update(ctx, changed))
		assertDenied(t, patch(denied, p, `{"metadata":{"labels":null}}`))
		jsonPatch := fmt.Sprintf(`[{"op":"remove","path":"/metadata/labels/%s"}]`, strings.ReplaceAll(key, "/", "~1"))
		assertDenied(t, denied.Patch(ctx, p, client.RawPatch(types.JSONPatchType, []byte(jsonPatch))))
		apply := fmt.Sprintf(`{"apiVersion":"v1","kind":"Pod","metadata":{"name":"routes","namespace":%q,"labels":{%q:"changed"}}}`, ns, key)
		assertDenied(t, denied.Patch(ctx, p, client.RawPatch(types.ApplyPatchType, []byte(apply)), client.FieldOwner("lws-controller-manager"), client.ForceOwnership))
		status := fmt.Sprintf(`{"metadata":{"labels":{%q:"changed"}}}`, key)
		assertDenied(t, denied.Status().Patch(ctx, p, client.RawPatch(types.MergePatchType, []byte(status))))
		must(t, denied.Status().Patch(ctx, p, client.RawPatch(types.MergePatchType, []byte(`{"status":{"phase":"Pending"}}`))))
	})
	t.Run("scheduler_binding_annotations", func(t *testing.T) {
		for _, legacy := range []bool{false, true} {
			p := newPod(fmt.Sprintf("binding-%t", legacy))
			key := "leaderworkerset.sigs.k8s.io/size"
			p.Annotations = map[string]string{key: "2"}
			must(t, allowed.Create(ctx, p))
			binding := &corev1.Binding{ObjectMeta: metav1.ObjectMeta{Name: p.Name, Namespace: ns}, Target: corev1.ObjectReference{Kind: "Node", Name: "test-node"}}
			bind := func(c client.Client) error {
				if legacy {
					return c.Create(ctx, binding)
				}
				return c.SubResource("binding").Create(ctx, p, binding)
			}
			// Binding can write Pod metadata, including through the legacy endpoint.
			binding.Labels = map[string]string{labelKey: "injected"}
			assertDenied(t, bind(denied))
			assertDenied(t, bind(actor(scheduler)))
			binding.Labels = nil
			// Volcano copies the existing Pod annotations into its binding request.
			binding.Annotations = p.Annotations
			assertDenied(t, bind(denied))
			assertDenied(t, patch(actor(scheduler), p, fmt.Sprintf(`{"metadata":{"annotations":{%q:"3"}}}`, key)))
			must(t, bind(actor(scheduler)))
			must(t, admin.Get(ctx, client.ObjectKeyFromObject(p), p))
			if p.Spec.NodeName != "test-node" || p.Annotations[key] != "2" {
				t.Fatalf("binding did not preserve Pod annotations: %#v", p)
			}
		}
	})
	t.Run("native_controller_create_requests_still_allowed", func(t *testing.T) {
		for i, name := range []string{"system:serviceaccount:kube-system:statefulset-controller", "system:serviceaccount:kube-system:replicaset-controller"} {
			p := newPod(fmt.Sprintf("native-create-%d", i))
			p.Labels = map[string]string{labelKey: "model"}
			must(t, actor(name).Create(ctx, p))
		}
	})
	t.Run("shared_controller_identity_cannot_inject_metadata", func(t *testing.T) {
		shared := actor("system:kube-controller-manager")
		for _, kind := range []string{"Pod", "ReplicaSet", "ControllerRevision"} {
			obj := workload(kind, "shared-"+strings.ToLower(kind))
			obj.SetLabels(map[string]string{labelKey: "model"})
			assertDenied(t, shared.Create(ctx, obj))
			obj.SetLabels(nil)
			obj.SetAnnotations(map[string]string{"disaggregatedset.x-k8s.io/name": "model"})
			assertDenied(t, shared.Create(ctx, obj))
		}
		must(t, shared.Create(ctx, newPod("shared-ordinary-pod")))
	})
	t.Run("native_exceptions_are_scoped", func(t *testing.T) {
		deploymentController := actor("system:serviceaccount:kube-system:deployment-controller")
		obj := workload("ReplicaSet", "native-rs")
		obj.SetAnnotations(map[string]string{"leaderworkerset.sigs.k8s.io/replicas": "1"})
		must(t, deploymentController.Create(ctx, obj))
		must(t, patch(deploymentController, obj, `{"metadata":{"annotations":{"leaderworkerset.sigs.k8s.io/replicas":"2"}}}`))
		assertDenied(t, patch(denied, obj, `{"metadata":{"annotations":{"leaderworkerset.sigs.k8s.io/replicas":"3"}}}`))
		assertDenied(t, patch(deploymentController, obj, `{"spec":{"template":{"metadata":{"labels":{"leaderworkerset.sigs.k8s.io/worker-index":"1"}}}}}`))
		p := newPod("native-no-update")
		p.Labels = map[string]string{labelKey: "original"}
		stsController := actor("system:serviceaccount:kube-system:statefulset-controller")
		must(t, stsController.Create(ctx, p))
		assertDenied(t, patch(stsController, p, fmt.Sprintf(`{"metadata":{"labels":{%q:"changed"}}}`, labelKey)))
		fake := newPod("spoofed-account")
		fake.Spec.ServiceAccountName = "lws-controller-manager"
		fake.Labels = map[string]string{labelKey: "original"}
		fake.OwnerReferences = []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "StatefulSet", Name: "model", UID: "fake-owner", Controller: ptr.To(true)}}
		assertDenied(t, denied.Create(ctx, fake))
	})
	t.Run("nested_metadata_cannot_be_injected_or_removed", func(t *testing.T) {
		cases := []struct {
			kind string
			path []string
		}{
			{"Pod", []string{"metadata"}},
			{"LeaderWorkerSet", []string{"metadata"}},
			{"DisaggregatedSet", []string{"metadata"}},
			{"Deployment", []string{"spec", "template", "metadata"}},
			{"StatefulSet", []string{"spec", "template", "metadata"}},
			{"ReplicaSet", []string{"spec", "template", "metadata"}},
			{"LeaderWorkerSet", []string{"spec", "leaderWorkerTemplate", "workerTemplate", "metadata"}},
			{"LeaderWorkerSet", []string{"spec", "leaderWorkerTemplate", "leaderTemplate", "metadata"}},
			{"ControllerRevision", []string{"data", "spec", "template", "metadata"}},
			{"ControllerRevision", []string{"data", "spec", "leaderWorkerTemplate", "workerTemplate", "metadata"}},
		}
		for i, tc := range cases {
			for _, field := range []string{"labels", "annotations"} {
				for prefixIndex, prefix := range []string{"leaderworkerset.sigs.k8s.io/", "disaggregatedset.x-k8s.io/"} {
					t.Run(fmt.Sprintf("%s-%d-%s-%d", tc.kind, i, field, prefixIndex), func(t *testing.T) {
						obj := workload(tc.kind, fmt.Sprintf("nested-%d-%s-%d", i, field, prefixIndex))
						key := prefix + "future-key"
						path := append(append([]string{}, tc.path...), field)
						must(t, unstructured.SetNestedStringMap(obj.Object, map[string]string{key: "original", "app": "test"}, path...))
						assertDenied(t, denied.Create(ctx, obj.DeepCopy()))
						must(t, allowed.Create(ctx, obj))
						// ControllerRevision data is also immutable in the Kubernetes API.
						if tc.kind == "ControllerRevision" {
							return
						}
						changed := obj.DeepCopy()
						must(t, unstructured.SetNestedStringMap(changed.Object, map[string]string{key: "changed", "app": "test"}, path...))
						assertDenied(t, denied.Update(ctx, changed))
						must(t, allowed.Update(ctx, changed))
						removed := changed.DeepCopy()
						unstructured.RemoveNestedField(removed.Object, append(path, key)...)

						assertDenied(t, denied.Update(ctx, removed))
						must(t, allowed.Update(ctx, removed))
						must(t, unstructured.SetNestedField(removed.Object, "", append(path, key)...))
						assertDenied(t, denied.Update(ctx, removed))
						must(t, allowed.Update(ctx, removed))
						if tc.kind != "Pod" {
							field := "replicas"
							if tc.kind == "DisaggregatedSet" {
								field = "slices"
							}
							must(t, patch(denied, removed, fmt.Sprintf(`{"spec":{%q:2}}`, field)))
						}
					})
				}
			}
		}
	})
	t.Run("disaggregatedset_role_metadata_is_matched_by_role_name", func(t *testing.T) {
		for _, slot := range []string{"metadata", "leaderTemplate", "workerTemplate"} {
			t.Run(slot, func(t *testing.T) {
				obj := workload("DisaggregatedSet", "roles-"+strings.ToLower(slot))
				roles, _, err := unstructured.NestedSlice(obj.Object, "spec", "roles")
				must(t, err)
				path := []string{"metadata", "labels"}
				if slot != "metadata" {
					path = []string{"spec", "leaderWorkerTemplate", slot, "metadata", "labels"}
				}
				for i, r := range roles {
					must(t, unstructured.SetNestedStringMap(r.(map[string]any), map[string]string{"disaggregatedset.x-k8s.io/role": fmt.Sprint(i)}, path...))
				}
				must(t, unstructured.SetNestedSlice(obj.Object, roles, "spec", "roles"))
				assertDenied(t, denied.Create(ctx, obj.DeepCopy()))
				must(t, allowed.Create(ctx, obj))
				roles[0], roles[1] = roles[1], roles[0]
				must(t, unstructured.SetNestedSlice(obj.Object, roles, "spec", "roles"))
				must(t, denied.Update(ctx, obj))
				roles[0].(map[string]any)["name"] = "renamed"
				must(t, unstructured.SetNestedSlice(obj.Object, roles, "spec", "roles"))
				assertDenied(t, denied.Update(ctx, obj))
			})
		}
	})
	t.Run("scale_subresources_remain_usable", func(t *testing.T) {
		for _, kind := range []string{"LeaderWorkerSet", "DisaggregatedSetRoleScaler"} {
			obj := workload(kind, "scale-"+strings.ToLower(kind))
			obj.SetLabels(map[string]string{"disaggregatedset.x-k8s.io/name": "model"})
			must(t, allowed.Create(ctx, obj))
			must(t, denied.SubResource("scale").Patch(ctx, obj, client.RawPatch(types.MergePatchType, []byte(`{"spec":{"replicas":3}}`))))
		}
	})
	t.Run("unrelated_opaque_revision_data_remains_usable", func(t *testing.T) {
		for i, data := range []any{map[string]any{"spec": "opaque"}, map[string]any{"metadata": map[string]any{"labels": "opaque"}}} {
			obj := workload("ControllerRevision", fmt.Sprintf("opaque-%d", i))
			obj.Object["data"] = data
			must(t, denied.Create(ctx, obj))
		}
	})
	t.Logf("Tested metadata protection against Kubernetes %s", kubernetesVersion)
}

func workload(kind, name string) *unstructured.Unstructured {
	template := func() map[string]any {
		return map[string]any{"metadata": map[string]any{"labels": map[string]any{"app": "test"}}, "spec": map[string]any{"containers": []any{map[string]any{"name": "main", "image": "registry.k8s.io/pause:3.10"}}}}
	}
	lwsSpec := func() map[string]any {
		return map[string]any{"replicas": int64(1), "leaderWorkerTemplate": map[string]any{"size": int64(1), "leaderTemplate": template(), "workerTemplate": template()}}
	}
	obj := &unstructured.Unstructured{Object: map[string]any{}}
	group := "apps"
	switch kind {
	case "Pod":
		group = ""
		obj.Object["spec"] = template()["spec"]
	case "LeaderWorkerSet":
		group = "leaderworkerset.x-k8s.io"
		obj.Object["spec"] = lwsSpec()
	case "DisaggregatedSet":
		group = "disaggregatedset.x-k8s.io"
		obj.Object["spec"] = map[string]any{"slices": int64(1), "roles": []any{map[string]any{"name": "prefill", "spec": lwsSpec()}, map[string]any{"name": "decode", "spec": lwsSpec()}}}
	case "DisaggregatedSetRoleScaler":
		group = "disaggregatedset.x-k8s.io"
		obj.Object["spec"] = map[string]any{"replicas": int64(1)}
	case "ControllerRevision":
		obj.Object["revision"] = int64(1)
		obj.Object["data"] = map[string]any{}
	default:
		obj.Object["spec"] = map[string]any{"replicas": int64(1), "selector": map[string]any{"matchLabels": map[string]any{"app": "test"}}, "template": template()}
		if kind == "StatefulSet" {
			obj.Object["spec"].(map[string]any)["serviceName"] = "test"
		}
	}
	obj.SetGroupVersionKind(schema.GroupVersionKind{Group: group, Version: "v1", Kind: kind})
	obj.SetName(name)
	obj.SetNamespace(ns)
	return obj
}

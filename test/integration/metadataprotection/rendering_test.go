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
	"os"
	"path/filepath"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestPolicyPackaging(t *testing.T) {
	root := filepath.Join("..", "..", "..")
	chart := filepath.Join(root, "charts/lws")
	t.Run("helm_custom_service_account_and_namespace", func(t *testing.T) {
		objects := decode(t, render(t, "helm", "template", "custom", chart, "--kube-version", "1.30.0", "--namespace", "custom-system", "--set", "serviceAccount.name=custom-manager", "--set", "metadataProtection.bindingAnnotationUsernames[0]=custom-scheduler", "--show-only", "templates/admission/metadata-protection.yaml"))
		if len(objects) != 4 {
			t.Fatalf("expected policy, binding, role and role binding; got %d", len(objects))
		}
		var policyName string
		for _, obj := range objects {
			switch obj.GetKind() {
			case "ValidatingAdmissionPolicy":
				policyName = obj.GetName()
				variables, _, err := unstructured.NestedSlice(obj.Object, "spec", "variables")
				must(t, err)
				found := false
				for _, v := range variables {
					variable := v.(map[string]any)
					if variable["name"] == "bindingAnnotationUsernames" {
						found = variable["expression"] == `["custom-scheduler"]`
					}
				}
				if !found {
					t.Fatal("custom binding identity was not rendered")
				}
			case "ValidatingAdmissionPolicyBinding":
				name, _, err := unstructured.NestedString(obj.Object, "spec", "policyName")
				must(t, err)
				if name != policyName {
					t.Fatalf("policy binding points at %q, expected %q", name, policyName)
				}
			case "ClusterRoleBinding":
				subjects, _, err := unstructured.NestedSlice(obj.Object, "subjects")
				must(t, err)
				subject := subjects[0].(map[string]any)
				if subject["name"] != "custom-manager" || subject["namespace"] != "custom-system" {
					t.Fatalf("incorrect allowed identity: %#v", subject)
				}
			}
		}
	})
	t.Run("disabled_and_old_kubernetes", func(t *testing.T) {
		objects := decode(t, render(t, "helm", "template", "lws", chart, "--kube-version", "1.29.0", "--set", "metadataProtection.enabled=false"))
		for _, obj := range objects {
			if obj.GetKind() == "ValidatingAdmissionPolicy" || obj.GetKind() == "ValidatingAdmissionPolicyBinding" {
				t.Fatal("disabled chart rendered admission policy")
			}
		}
		cmd := toolCommand("helm", "template", "lws", chart, "--kube-version", "1.29.0")
		out, err := cmd.CombinedOutput()
		if err == nil || !bytes.Contains(out, []byte("metadataProtection requires Kubernetes 1.30")) {
			t.Fatalf("expected version error, got %v: %s", err, out)
		}
	})
	t.Run("missing_legacy_values_enable_protection", func(t *testing.T) {
		objects := decode(t, render(t, "helm", "template", "lws", chart, "--kube-version", "1.30.0", "--set", "metadataProtection=null", "--show-only", "templates/admission/metadata-protection.yaml"))
		if len(objects) != 4 {
			t.Fatalf("missing metadataProtection values disabled protection: %d objects", len(objects))
		}
	})
	t.Run("kustomize_outer_namespace_and_prefix", func(t *testing.T) {
		// Put the temporary overlay inside config so Kustomize's normal file
		// restrictions apply. No --load-restrictor override is needed.
		dir, err := os.MkdirTemp(filepath.Join(root, "config"), "metadata-test-")
		must(t, err)
		t.Cleanup(func() { os.RemoveAll(dir) })
		must(t, os.WriteFile(filepath.Join(dir, "kustomization.yaml"), []byte("apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources:\n- ../default\nnamespace: outer-system\nnamePrefix: outer-\n"), 0600))
		objects := decode(t, render(t, "kustomize", "build", dir))
		for _, obj := range objects {
			if obj.GetKind() == "ClusterRoleBinding" && obj.GetName() == "outer-lws-metadata-writer" {
				subjects, _, err := unstructured.NestedSlice(obj.Object, "subjects")
				must(t, err)
				subject := subjects[0].(map[string]any)
				if subject["name"] != "outer-lws-controller-manager" || subject["namespace"] != "outer-system" {
					t.Fatalf("incorrect transformed allowed identity: %#v", subject)
				}
				return
			}
		}
		t.Fatal("missing metadata-writer binding")
	})
}

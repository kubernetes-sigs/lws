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

// Package examples validates every manifest that is shown in the documentation
// or shipped as a sample, so that the YAML users copy cannot drift from the API.
package examples

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	yamlutil "k8s.io/apimachinery/pkg/util/yaml"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"

	configv1 "sigs.k8s.io/lws/api/config/v1"
	configv1alpha1 "sigs.k8s.io/lws/api/config/v1alpha1"
	disaggv1 "sigs.k8s.io/lws/api/disaggregatedset/v1"
	lwsv1 "sigs.k8s.io/lws/api/leaderworkerset/v1"
	"sigs.k8s.io/lws/pkg/webhooks"
	dswebhooks "sigs.k8s.io/lws/pkg/webhooks/disaggregatedset"
)

// exampleDirs are the directories, relative to the repository root, whose
// manifests are rendered in the website or shipped as samples.
var exampleDirs = []string{
	"site/static/examples",
	"docs/examples",
	"config/samples",
}

// skippedFiles are YAML files under exampleDirs that are not Kubernetes manifests.
var skippedFiles = map[string]bool{
	"kustomization.yaml": true,
}

func newScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{
		clientgoscheme.AddToScheme,
		lwsv1.AddToScheme,
		disaggv1.AddToScheme,
		configv1.AddToScheme,
		configv1alpha1.AddToScheme,
	} {
		if err := add(scheme); err != nil {
			t.Fatalf("failed to build scheme: %v", err)
		}
	}
	return scheme
}

func repoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("failed to resolve repository root: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("repository root %q does not contain go.mod: %v", root, err)
	}
	return root
}

func listManifests(t *testing.T, root string) []string {
	t.Helper()
	var files []string
	for _, dir := range exampleDirs {
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || skippedFiles[d.Name()] {
				return nil
			}
			if ext := filepath.Ext(d.Name()); ext == ".yaml" || ext == ".yml" {
				files = append(files, path)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("failed to walk %s: %v", dir, err)
		}
	}
	if len(files) == 0 {
		t.Fatal("no example manifests found; check exampleDirs")
	}
	return files
}

// splitDocuments returns the non-empty YAML documents in a multi-document file.
func splitDocuments(t *testing.T, path string) [][]byte {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("failed to open %s: %v", path, err)
	}
	defer f.Close()

	var docs [][]byte
	reader := yamlutil.NewYAMLReader(bufio.NewReader(f))
	for {
		doc, err := reader.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("failed to read %s: %v", path, err)
		}
		if isBlank(doc) {
			continue
		}
		docs = append(docs, doc)
	}
	return docs
}

// isBlank reports whether a YAML document contains only whitespace and comments.
func isBlank(doc []byte) bool {
	for _, line := range bytes.Split(doc, []byte("\n")) {
		trimmed := bytes.TrimSpace(line)
		if len(trimmed) == 0 || bytes.HasPrefix(trimmed, []byte("#")) || bytes.Equal(trimmed, []byte("---")) {
			continue
		}
		return false
	}
	return true
}

// TestExampleManifests decodes every documented manifest with strict field
// checking and runs LeaderWorkerSet and DisaggregatedSet objects through the
// same defaulting and validation the admission webhooks apply in a cluster.
// Kinds the scheme does not know (for example KEDA or Kueue objects) only need
// to be well-formed YAML with an apiVersion and a kind.
func TestExampleManifests(t *testing.T) {
	root := repoRoot(t)
	scheme := newScheme(t)
	decoder := serializer.NewCodecFactory(scheme, serializer.EnableStrict).UniversalDeserializer()
	ctx := context.Background()

	for _, path := range listManifests(t, root) {
		rel, _ := filepath.Rel(root, path)
		t.Run(rel, func(t *testing.T) {
			for i, doc := range splitDocuments(t, path) {
				var typeMeta metav1.TypeMeta
				if err := yamlutil.Unmarshal(doc, &typeMeta); err != nil {
					t.Fatalf("document %d is not valid YAML: %v", i, err)
				}
				if typeMeta.APIVersion == "" || typeMeta.Kind == "" {
					t.Fatalf("document %d has no apiVersion or kind", i)
				}

				obj, gvk, err := decoder.Decode(doc, nil, nil)
				if runtime.IsNotRegisteredError(err) {
					if isProjectGroup(typeMeta.GroupVersionKind().Group) {
						t.Fatalf("document %d: %s belongs to this project but is not a registered type; the apiVersion or kind is stale", i, typeMeta.GroupVersionKind())
					}
					t.Logf("document %d: %s is not a registered type, checked syntax only", i, typeMeta.GroupVersionKind())
					continue
				}
				if err != nil {
					t.Fatalf("document %d (%s) failed strict decoding: %v", i, typeMeta.GroupVersionKind(), err)
				}

				switch o := obj.(type) {
				case *lwsv1.LeaderWorkerSet:
					// The API server fills in the namespace of a namespaced
					// object before admission runs; mirror that here so the
					// validation sees what the webhook would see in a cluster.
					if o.Namespace == "" {
						o.Namespace = metav1.NamespaceDefault
					}
					webhook := &webhooks.LeaderWorkerSetWebhook{}
					if err := webhook.Default(ctx, o); err != nil {
						t.Fatalf("document %d (%s): defaulting failed: %v", i, gvk, err)
					}
					warnings, err := webhook.ValidateCreate(ctx, o)
					if err != nil {
						t.Fatalf("document %d (%s): webhook validation failed: %v", i, gvk, err)
					}
					logWarnings(t, i, warnings)
				case *disaggv1.DisaggregatedSet:
					if o.Namespace == "" {
						o.Namespace = metav1.NamespaceDefault
					}
					webhook := &dswebhooks.DisaggregatedSetWebhook{}
					warnings, err := webhook.ValidateCreate(ctx, o)
					if err != nil {
						t.Fatalf("document %d (%s): webhook validation failed: %v", i, gvk, err)
					}
					logWarnings(t, i, warnings)
				}
			}
		})
	}
}

// isProjectGroup reports whether an API group is served by this project, in
// which case every documented manifest must decode against the current types.
func isProjectGroup(group string) bool {
	return group == lwsv1.GroupVersion.Group ||
		group == disaggv1.GroupVersion.Group ||
		strings.HasSuffix(group, "lws.x-k8s.io")
}

func logWarnings(t *testing.T, doc int, warnings []string) {
	t.Helper()
	if len(warnings) > 0 {
		t.Logf("document %d: webhook warnings: %s", doc, strings.Join(warnings, "; "))
	}
}

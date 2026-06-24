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
	"context"
	"maps"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	appsapplyv1 "k8s.io/client-go/applyconfigurations/apps/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// mergeStatefulSetMetadata returns a new metadata map with user entries first and
// controller-owned entries overriding conflicting keys.
func mergeStatefulSetMetadata(userMetadata, controllerMetadata map[string]string) map[string]string {
	merged := make(map[string]string, len(userMetadata)+len(controllerMetadata))
	maps.Copy(merged, userMetadata)
	maps.Copy(merged, controllerMetadata)
	if len(merged) == 0 {
		return nil
	}
	return merged
}

// applyStatefulSetMetadata server-side applies top-level labels and annotations
// for a StatefulSet so removed LWS-owned metadata keys are pruned on later syncs.
func applyStatefulSetMetadata(ctx context.Context, k8sClient client.Client, name, namespace string, desiredLabels, desiredAnnotations map[string]string) error {
	if desiredLabels == nil {
		desiredLabels = map[string]string{}
	}
	if desiredAnnotations == nil {
		desiredAnnotations = map[string]string{}
	}

	statefulSetConfig := appsapplyv1.StatefulSet(name, namespace).
		WithLabels(desiredLabels).
		WithAnnotations(desiredAnnotations)
	obj, err := runtime.DefaultUnstructuredConverter.ToUnstructured(statefulSetConfig)
	if err != nil {
		return err
	}
	patch := &unstructured.Unstructured{
		Object: obj,
	}
	return k8sClient.Patch(ctx, patch, client.Apply, &client.PatchOptions{
		FieldManager: fieldManager,
		Force:        ptr.To[bool](true),
	})
}

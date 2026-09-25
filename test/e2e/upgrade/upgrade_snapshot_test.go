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

package upgrade

import (
	"context"
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	leaderworkersetv1 "sigs.k8s.io/lws/api/leaderworkerset/v1"
)

func TestGeneratedServiceSnapshots(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}

	names := []string{"existing-ds-0-revision-prefill", "existing-ds-0-revision-decode"}
	generated := make([]leaderworkersetv1.LeaderWorkerSet, 0, len(names))
	services := make([]*corev1.Service, 0, len(names))
	for _, name := range names {
		lws := leaderworkersetv1.LeaderWorkerSet{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace, UID: types.UID(name + "-owner")},
		}
		generated = append(generated, lws)
		services = append(services, &corev1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Name:            name,
				Namespace:       testNamespace,
				UID:             types.UID(name + "-service"),
				Labels:          map[string]string{leaderworkersetv1.SetNameLabelKey: name},
				OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(&lws, leaderworkersetv1.GroupVersion.WithKind("LeaderWorkerSet"))},
			},
		})
	}

	oldCtx, oldClient := ctx, k8sClient
	t.Cleanup(func() { ctx, k8sClient = oldCtx, oldClient })
	ctx = context.Background()
	k8sClient = fake.NewClientBuilder().WithScheme(scheme).WithObjects(services[0], services[1]).Build()

	got, err := generatedServiceSnapshots(generated)
	if err != nil {
		t.Fatal(err)
	}
	want := []serviceSnapshot{
		{Name: names[1], UID: services[1].UID},
		{Name: names[0], UID: services[0].UID},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("generated Service snapshots = %v, want %v", got, want)
	}

	services[0].OwnerReferences = nil
	k8sClient = fake.NewClientBuilder().WithScheme(scheme).WithObjects(services[0], services[1]).Build()
	if _, err := generatedServiceSnapshots(generated); err == nil {
		t.Fatal("expected an error for a Service not owned by its generated LeaderWorkerSet")
	}
}

/*
Copyright 2023.

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

package controller

import (
	"context"
	"testing"

	"github.com/google/go-cmp/cmp"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	coreapplyv1 "k8s.io/client-go/applyconfigurations/core/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	leaderworkerset "sigs.k8s.io/lws/api/leaderworkerset/v1"

	"sigs.k8s.io/lws/test/wrappers"
)

func TestGetPVCApplyConfiguration(t *testing.T) {
	tests := []struct {
		name     string
		lws      *leaderworkerset.LeaderWorkerSet
		expected []*coreapplyv1.PersistentVolumeClaimApplyConfiguration
	}{
		{
			name:     "No PVC templates in LeaderWorkerSet",
			lws:      wrappers.BuildBasicLeaderWorkerSet("test-sample", "default").Obj(),
			expected: []*coreapplyv1.PersistentVolumeClaimApplyConfiguration{},
		},
		{
			name: "Single PVC template with all fields",
			lws: wrappers.BuildBasicLeaderWorkerSet("test-sample", "default").
				VolumeClaimTemplates([]corev1.PersistentVolumeClaim{
					{
						ObjectMeta: metav1.ObjectMeta{Name: "pvc1"},
						Spec: corev1.PersistentVolumeClaimSpec{
							AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
							StorageClassName: ptr.To[string]("standard"),
							VolumeMode:       ptr.To[corev1.PersistentVolumeMode](corev1.PersistentVolumeFilesystem),
							Resources: corev1.VolumeResourceRequirements{
								Requests: corev1.ResourceList{
									corev1.ResourceStorage: resource.MustParse("1Gi"),
								},
								Limits: corev1.ResourceList{
									corev1.ResourceStorage: resource.MustParse("2Gi"),
								},
							},
						},
					},
				}).Obj(),
			expected: []*coreapplyv1.PersistentVolumeClaimApplyConfiguration{
				coreapplyv1.PersistentVolumeClaim("pvc1", "default").
					WithSpec(coreapplyv1.PersistentVolumeClaimSpec().
						WithAccessModes(corev1.ReadWriteOnce).
						WithStorageClassName("standard").
						WithVolumeMode(corev1.PersistentVolumeFilesystem).
						WithResources(&coreapplyv1.VolumeResourceRequirementsApplyConfiguration{
							Requests: &corev1.ResourceList{
								corev1.ResourceStorage: resource.MustParse("1Gi"),
							},
							Limits: &corev1.ResourceList{
								corev1.ResourceStorage: resource.MustParse("2Gi"),
							},
						}),
					),
			},
		},
		{
			name: "Multiple PVC templates with partial fields",
			lws: wrappers.BuildBasicLeaderWorkerSet("test-sample", "default").
				VolumeClaimTemplates([]corev1.PersistentVolumeClaim{
					{
						ObjectMeta: metav1.ObjectMeta{Name: "pvc1"},
						Spec: corev1.PersistentVolumeClaimSpec{
							AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
						},
					},
					{
						ObjectMeta: metav1.ObjectMeta{Name: "pvc2"},
						Spec: corev1.PersistentVolumeClaimSpec{
							AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany},
							StorageClassName: ptr.To[string]("fast"),
						},
					},
				}).Obj(),
			expected: []*coreapplyv1.PersistentVolumeClaimApplyConfiguration{
				coreapplyv1.PersistentVolumeClaim("pvc1", "default").
					WithSpec(coreapplyv1.PersistentVolumeClaimSpec().
						WithAccessModes(corev1.ReadWriteOnce),
					),
				coreapplyv1.PersistentVolumeClaim("pvc2", "default").
					WithSpec(coreapplyv1.PersistentVolumeClaimSpec().
						WithAccessModes(corev1.ReadWriteMany).
						WithStorageClassName("fast"),
					),
			},
		},
		{
			name:     "Nil LeaderWorkerSet",
			lws:      nil,
			expected: []*coreapplyv1.PersistentVolumeClaimApplyConfiguration{},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := GetPVCApplyConfiguration(tc.lws)
			if diff := cmp.Diff(tc.expected, result); diff != "" {
				t.Errorf("Unexpected PVC apply configuration (-want +got):\n%s", diff)
			}
		})
	}
}

func TestCreateHeadlessServiceAdoptsPrecreatedServiceForLeaderPod(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("adding corev1 to scheme: %v", err)
	}
	if err := leaderworkerset.AddToScheme(scheme); err != nil {
		t.Fatalf("adding leaderworkerset to scheme: %v", err)
	}

	lws := wrappers.BuildLeaderWorkerSet("default").
		SubdomainPolicy(leaderworkerset.SubdomainUniquePerReplica).
		Obj()
	lws.UID = types.UID("lws-uid")
	pod := wrappers.MakePodWithLabels(lws.Name, "0", "0", lws.Namespace, 2)
	pod.UID = types.UID("pod-uid")
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()

	selector := map[string]string{
		leaderworkerset.SetNameLabelKey:    lws.Name,
		leaderworkerset.GroupIndexLabelKey: "0",
	}
	if err := CreateHeadlessServiceIfNotExists(context.Background(), k8sClient, scheme, lws, pod.Name, selector, lws); err != nil {
		t.Fatalf("precreate Service: %v", err)
	}
	if err := CreateHeadlessServiceIfNotExists(context.Background(), k8sClient, scheme, lws, pod.Name, selector, pod); err != nil {
		t.Fatalf("adopt Service: %v", err)
	}

	var service corev1.Service
	if err := k8sClient.Get(context.Background(), types.NamespacedName{Namespace: lws.Namespace, Name: pod.Name}, &service); err != nil {
		t.Fatalf("get Service: %v", err)
	}
	owner := metav1.GetControllerOf(&service)
	if owner == nil || owner.UID != pod.UID || owner.Kind != "Pod" {
		t.Fatalf("controller owner = %#v, want Pod UID %q", owner, pod.UID)
	}
}

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
	"testing"

	"github.com/google/go-cmp/cmp"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	coreapplyv1 "k8s.io/client-go/applyconfigurations/core/v1"
	"k8s.io/utils/ptr"

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

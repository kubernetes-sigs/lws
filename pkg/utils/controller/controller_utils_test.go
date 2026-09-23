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
	coreapplyv1 "k8s.io/client-go/applyconfigurations/core/v1"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	leaderworkerset "sigs.k8s.io/lws/api/leaderworkerset/v1"

	"sigs.k8s.io/lws/test/wrappers"
)

func TestCreateHeadlessServiceIfNotExists(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := leaderworkerset.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	lws := &leaderworkerset.LeaderWorkerSet{
		ObjectMeta: metav1.ObjectMeta{Name: "test-lws", Namespace: "default", UID: "current-lws"},
	}
	leader := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "test-lws-0", Namespace: lws.Namespace, UID: "current-leader"},
	}
	selector := map[string]string{leaderworkerset.SetNameLabelKey: lws.Name}
	for _, owner := range []client.Object{leader, lws} {
		t.Run(owner.GetName(), func(t *testing.T) {
			for _, tc := range []struct {
				name    string
				mutate  func(*corev1.Service)
				create  bool
				wantErr bool
			}{
				{name: "create missing service", create: true},
				{name: "reuse current service"},
				{
					name: "previous owner UID",
					mutate: func(svc *corev1.Service) {
						svc.OwnerReferences[0].UID = "previous-owner"
					},
					wantErr: true,
				},
				{
					name: "missing owner",
					mutate: func(svc *corev1.Service) {
						svc.OwnerReferences = nil
					},
					wantErr: true,
				},
				{
					name: "non-controller owner",
					mutate: func(svc *corev1.Service) {
						svc.OwnerReferences[0].Controller = ptr.To(false)
					},
					wantErr: true,
				},
				{
					name: "foreign owner",
					mutate: func(svc *corev1.Service) {
						svc.OwnerReferences[0].Name = "other-owner"
						svc.OwnerReferences[0].UID = "other-owner"
					},
					wantErr: true,
				},
				{
					name: "terminating service with current owner",
					mutate: func(svc *corev1.Service) {
						now := metav1.Now()
						svc.DeletionTimestamp = &now
						svc.Finalizers = []string{"leaderworkerset.sigs.k8s.io/test"}
					},
					wantErr: true,
				},
			} {
				t.Run(tc.name, func(t *testing.T) {
					svc := &corev1.Service{
						ObjectMeta: metav1.ObjectMeta{Name: owner.GetName(), Namespace: lws.Namespace},
						Spec: corev1.ServiceSpec{
							ClusterIP:                corev1.ClusterIPNone,
							Selector:                 selector,
							PublishNotReadyAddresses: true,
						},
					}
					if err := ctrl.SetControllerReference(owner, svc, scheme); err != nil {
						t.Fatal(err)
					}
					if tc.mutate != nil {
						tc.mutate(svc)
					}
					builder := fake.NewClientBuilder().WithScheme(scheme)
					if !tc.create {
						builder.WithObjects(svc)
					}
					k8sClient := builder.Build()
					var before corev1.Service
					if !tc.create {
						if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(svc), &before); err != nil {
							t.Fatal(err)
						}
					}
					err := CreateHeadlessServiceIfNotExists(ctx, k8sClient, scheme, lws, svc.Name, selector, owner)
					if (err != nil) != tc.wantErr {
						t.Fatalf("CreateHeadlessServiceIfNotExists() error = %v, wantErr %t", err, tc.wantErr)
					}
					var actual corev1.Service
					if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(svc), &actual); err != nil {
						t.Fatal(err)
					}
					if !tc.create {
						if diff := cmp.Diff(&before, &actual); diff != "" {
							t.Fatalf("existing service was modified (-want +got):\n%s", diff)
						}
					} else if !metav1.IsControlledBy(&actual, owner) || !cmp.Equal(svc.Spec, actual.Spec) {
						t.Fatalf("unexpected service: %+v", actual)
					}
				})
			}
		})
	}
}

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

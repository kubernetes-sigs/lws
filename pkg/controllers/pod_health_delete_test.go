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
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	leaderv1 "sigs.k8s.io/lws/api/leaderworkerset/v1"
	"sigs.k8s.io/lws/internal/groupdeletion"
	"sigs.k8s.io/lws/pkg/schedulerprovider"
	"sigs.k8s.io/lws/test/wrappers"
)

func TestNativeHealthDeleteAuthorization(t *testing.T) {
	for _, tc := range []struct {
		name           string
		protected      bool
		authorized     string
		failDelete     bool
		metadataChange bool
		failPatch      bool
	}{
		{name: "ordinary leader is not patched"},
		{name: "protected leader is authorized before deletion", protected: true},
		{name: "exact authorization is idempotent", protected: true, authorized: "leader-uid"},
		{name: "old UID authorization is replaced", protected: true, authorized: "old-uid"},
		{name: "failed delete retains authorization for retry", protected: true, failDelete: true},
		{name: "concurrent metadata does not conflict or get clobbered", protected: true, metadataChange: true},
		{name: "failed authorization prevents deletion", protected: true, failPatch: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			scheme := runtime.NewScheme()
			require.NoError(t, corev1.AddToScheme(scheme))
			leader := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "leader", Namespace: "test", UID: "leader-uid", Annotations: map[string]string{}}}
			if tc.protected {
				leader.Annotations[groupdeletion.ProtectionAnnotation] = "lws-uid/leader-uid"
			}
			if tc.authorized != "" {
				leader.Annotations[groupdeletion.ReplacementAnnotation] = tc.authorized
			}
			base := fake.NewClientBuilder().WithScheme(scheme).WithObjects(leader).Build()
			require.NoError(t, base.Get(t.Context(), client.ObjectKeyFromObject(leader), leader))
			if tc.metadataChange {
				concurrent := leader.DeepCopy()
				concurrent.Labels = map[string]string{"unrelated": "updated"}
				require.NoError(t, base.Update(t.Context(), concurrent))
			}
			patches, deletes := 0, 0
			failure := errors.New("delete unavailable")
			c := interceptor.NewClient(base, interceptor.Funcs{
				Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
					patches++
					data, err := patch.Data(obj)
					require.NoError(t, err)
					require.JSONEq(t, `{"metadata":{"uid":"leader-uid","annotations":{"leaderworkerset.sigs.k8s.io/replacement-delete":"leader-uid"}}}`, string(data))
					if tc.failPatch {
						return failure
					}
					return c.Patch(ctx, obj, patch, opts...)
				},
				Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
					deletes++
					actual := &corev1.Pod{}
					require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(obj), actual))
					if tc.protected {
						require.Equal(t, string(actual.UID), actual.Annotations[groupdeletion.ReplacementAnnotation])
					}
					if tc.metadataChange {
						require.Equal(t, "updated", actual.Labels["unrelated"])
					}
					options := (&client.DeleteOptions{}).ApplyOptions(opts)
					require.Equal(t, metav1.DeletePropagationForeground, *options.PropagationPolicy)
					require.Equal(t, leader.UID, *options.Preconditions.UID)
					if tc.failDelete && deletes == 1 {
						return failure
					}
					return c.Delete(ctx, obj, opts...)
				},
			})
			err := groupdeletion.DeleteForReplacement(t.Context(), c, leader)
			if tc.failPatch {
				require.ErrorIs(t, err, failure)
				require.Zero(t, deletes)
				require.NoError(t, base.Get(t.Context(), client.ObjectKeyFromObject(leader), leader))
				require.Empty(t, leader.Annotations[groupdeletion.ReplacementAnnotation])
				return
			}
			if tc.failDelete {
				require.ErrorIs(t, err, failure)
				require.NoError(t, base.Get(t.Context(), client.ObjectKeyFromObject(leader), leader))
				require.Equal(t, string(leader.UID), leader.Annotations[groupdeletion.ReplacementAnnotation])
				err = groupdeletion.DeleteForReplacement(t.Context(), c, leader)
				require.Equal(t, 2, deletes)
			}
			require.NoError(t, err)
			wantPatches := 0
			if tc.protected && tc.authorized != string(leader.UID) {
				wantPatches = 1
			}
			require.Equal(t, wantPatches, patches)
			require.True(t, apierrors.IsNotFound(base.Get(t.Context(), client.ObjectKeyFromObject(leader), &corev1.Pod{})))
		})
	}
}

func TestNativeHealthAuthorizationDoesNotRecountMetadataConflict(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, leaderv1.AddToScheme(scheme))
	lws := wrappers.BuildLeaderWorkerSet("test").Replica(1).Size(1).RestartPolicy(leaderv1.RecreateGroupOnPodRestart).Obj()
	limit := int32(1)
	lws.Spec.LeaderWorkerTemplate.MaxGroupRestarts = &limit
	leader := wrappers.MakePodWithLabels(lws.Name, "0", "0", lws.Namespace, 1)
	leader.UID = "leader-uid"
	leader.Labels[leaderv1.RevisionKey] = "revision"
	leader.Annotations[groupdeletion.ProtectionAnnotation] = "lws/leader-uid"
	leader.Status.Phase = corev1.PodRunning
	leader.Status.ContainerStatuses = []corev1.ContainerStatus{{RestartCount: 1}}
	base := fake.NewClientBuilder().WithScheme(scheme).WithObjects(lws, leader).Build()
	r := &PodReconciler{Record: fakeEventRecorder{}, Client: interceptor.NewClient(base, interceptor.Funcs{
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			if _, isPod := obj.(*corev1.Pod); isPod {
				current := &corev1.Pod{}
				require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(obj), current))
				current.Labels["concurrent-subrole-repair"] = "updated"
				require.NoError(t, c.Update(ctx, current))
			}
			return c.Patch(ctx, obj, patch, opts...)
		},
	})}
	deleted, result, err := r.handleRestartPolicy(t.Context(), *leader, *lws)
	require.NoError(t, err)
	require.True(t, deleted)
	require.True(t, result.IsZero())
	require.NoError(t, base.Get(t.Context(), client.ObjectKeyFromObject(lws), lws))
	counts, err := parseGroupRestartCounts(lws.Annotations[leaderv1.GroupRestartCountsAnnotationKey])
	require.NoError(t, err)
	require.Equal(t, int32(1), counts["revision/0"])
}

func TestNativeHealthDeleteRequiresProtectedUID(t *testing.T) {
	// The table above checks the helper's UID precondition. Real API tests
	// prove enforcement against a same-name replacement; the fake cannot.
	leader := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "leader", Annotations: map[string]string{
		groupdeletion.ProtectionAnnotation: "lws-uid/old-uid",
	}}}
	// No client call is permitted without an identity.
	require.ErrorContains(t, groupdeletion.DeleteForReplacement(t.Context(), nil, leader), "without its UID")
}

func TestProtectedGroupSplitByUpgradeAuthorization(t *testing.T) {
	deleteFailure := errors.New("delete unavailable")
	for _, tc := range []struct {
		name      string
		deleteErr error
		wantErr   error
	}{
		{name: "protected leader is authorized before upgrade recreation"},
		{name: "missing leader completes recreation", deleteErr: apierrors.NewNotFound(corev1.Resource("pods"), "leader")},
		{name: "replaced leader completes recreation", deleteErr: apierrors.NewConflict(corev1.Resource("pods"), "leader", errors.New("UID changed"))},
		{name: "delete failure is retried", deleteErr: deleteFailure, wantErr: deleteFailure},
	} {
		t.Run(tc.name, func(t *testing.T) {
			scheme := podCtrlTestScheme(t)
			lws := podCtrlBasicLWS()
			lws.UID = "lws-uid"
			lws.Spec.GroupIdentity = leaderv1.GroupIdentityHash
			lws.Spec.Scheduling = &leaderv1.LeaderWorkerSetScheduling{}
			workloadName := schedulerprovider.KubernetesWorkloadName(lws)
			leader := podCtrlLeaderPod(lws, "leader")
			leader.Annotations = map[string]string{
				schedulerprovider.WorkloadNameAnnotationKey: workloadName + ".x7k2p",
				groupdeletion.ProtectionAnnotation:          string(lws.UID) + "/" + string(leader.UID),
			}
			workerSts := &appsv1.StatefulSet{
				ObjectMeta: metav1.ObjectMeta{
					Name: "workers", Namespace: lws.Namespace, UID: "workers-uid",
					OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(leader, corev1.SchemeGroupVersion.WithKind("Pod"))},
				},
				Spec: appsv1.StatefulSetSpec{Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{schedulerprovider.WorkloadNameAnnotationKey: workloadName},
				}}},
			}
			deletes := 0
			c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(lws, leader, workerSts).
				WithInterceptorFuncs(interceptor.Funcs{
					Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
						deletes++
						actual := &corev1.Pod{}
						require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(obj), actual))
						require.Equal(t, string(leader.UID), actual.Annotations[groupdeletion.ReplacementAnnotation])
						options := (&client.DeleteOptions{}).ApplyOptions(opts)
						require.Equal(t, leader.UID, *options.Preconditions.UID)
						require.Equal(t, metav1.DeletePropagationForeground, *options.PropagationPolicy)
						if tc.deleteErr != nil {
							return tc.deleteErr
						}
						return c.Delete(ctx, obj, opts...)
					},
				}).Build()
			r := &PodReconciler{Client: c, Record: fakeEventRecorder{}}
			recreated, err := r.recreateGroupSplitByUpgrade(t.Context(), lws, leader, workerSts)
			require.ErrorIs(t, err, tc.wantErr)
			require.Equal(t, tc.wantErr == nil, recreated)
			require.Equal(t, 1, deletes)
			require.NoError(t, c.Get(t.Context(), client.ObjectKeyFromObject(lws), lws))
			require.Empty(t, lws.Annotations[leaderv1.GroupRestartCountsAnnotationKey])
		})
	}
}

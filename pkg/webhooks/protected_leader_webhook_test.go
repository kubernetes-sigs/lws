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

package webhooks

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	leaderv1 "sigs.k8s.io/lws/api/leaderworkerset/v1"
)

func TestProtectedLeaderDeletion(t *testing.T) {
	for _, tc := range []struct {
		name           string
		mutate         func(*leaderv1.LeaderWorkerSet, *corev1.Pod)
		absent, denied bool
	}{
		{name: "stale ReplicaSet picks protected survivor", denied: true},
		{name: "selected victim", mutate: func(l *leaderv1.LeaderWorkerSet, p *corev1.Pod) {
			l.Annotations[leaderv1.GroupScalePlanAnnotationKey] = "plan"
			p.Annotations[leaderv1.GroupScaleVictimAnnotationKey] = "plan/pod"
		}},
		{name: "administrative override", mutate: func(_ *leaderv1.LeaderWorkerSet, p *corev1.Pod) { clear(p.Annotations) }},
		{name: "prepared but not armed", denied: true, mutate: func(_ *leaderv1.LeaderWorkerSet, p *corev1.Pod) {
			p.Annotations[leaderv1.GroupScaleVictimAnnotationKey] = "plan/pod"
		}},
		{name: "empty plan cannot authorize a victim", denied: true, mutate: func(_ *leaderv1.LeaderWorkerSet, p *corev1.Pod) {
			p.Annotations[leaderv1.GroupScaleVictimAnnotationKey] = "/pod"
		}},
		{name: "new transaction does not authorize old victim intent", denied: true, mutate: func(l *leaderv1.LeaderWorkerSet, p *corev1.Pod) {
			l.Annotations[leaderv1.GroupScalePlanAnnotationKey] = "next"
			p.Annotations[leaderv1.GroupScaleVictimAnnotationKey] = "previous/pod"
		}},
		{name: "armed plan does not authorize an unmarked survivor", denied: true, mutate: func(l *leaderv1.LeaderWorkerSet, _ *corev1.Pod) {
			l.Annotations[leaderv1.GroupScalePlanAnnotationKey] = "plan"
		}},
		{name: "copied victim intent stays invalid after protection repair", denied: true, mutate: func(l *leaderv1.LeaderWorkerSet, p *corev1.Pod) {
			l.Annotations[leaderv1.GroupScalePlanAnnotationKey] = "plan"
			p.Annotations[leaderv1.GroupScaleVictimAnnotationKey] = "plan/previous-pod"
		}},
		{name: "native health replacement", mutate: func(_ *leaderv1.LeaderWorkerSet, p *corev1.Pod) {
			p.Annotations[leaderv1.GroupReplacementDeleteAnnotationKey] = string(p.UID)
		}},
		{name: "stale native replacement authorization", denied: true, mutate: func(_ *leaderv1.LeaderWorkerSet, p *corev1.Pod) {
			p.Annotations[leaderv1.GroupReplacementDeleteAnnotationKey] = "previous-uid"
		}},
		{name: "replacement LWS", mutate: func(l *leaderv1.LeaderWorkerSet, _ *corev1.Pod) { l.UID = "replacement" }},
		{name: "replacement Pod with copied annotations", mutate: func(_ *leaderv1.LeaderWorkerSet, p *corev1.Pod) { p.UID = "replacement" }},
		{name: "deleting parent", mutate: func(l *leaderv1.LeaderWorkerSet, _ *corev1.Pod) {
			now := metav1.Now()
			l.DeletionTimestamp = &now
			l.Finalizers = []string{"test"}
		}},
		{name: "missing parent", absent: true},
		{name: "failed Pod garbage collection", mutate: func(_ *leaderv1.LeaderWorkerSet, p *corev1.Pod) { p.Status.Phase = corev1.PodFailed }},
		{name: "completed Pod garbage collection", mutate: func(_ *leaderv1.LeaderWorkerSet, p *corev1.Pod) { p.Status.Phase = corev1.PodSucceeded }},
		{name: "already terminating Pod", mutate: func(_ *leaderv1.LeaderWorkerSet, p *corev1.Pod) { now := metav1.Now(); p.DeletionTimestamp = &now }},
		{name: "workers are not fenced", mutate: func(_ *leaderv1.LeaderWorkerSet, p *corev1.Pod) { p.Labels[leaderv1.WorkerIndexLabelKey] = "1" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lws := &leaderv1.LeaderWorkerSet{ObjectMeta: metav1.ObjectMeta{Name: "pool", Namespace: "default", UID: "parent", Annotations: map[string]string{}}}
			pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "leader", Namespace: "default", UID: "pod", Labels: map[string]string{leaderv1.SetNameLabelKey: "pool", leaderv1.WorkerIndexLabelKey: "0"}, Annotations: map[string]string{leaderv1.GroupScaleProtectionAnnotationKey: "parent/pod"}}}
			if tc.mutate != nil {
				tc.mutate(lws, pod)
			}
			scheme := runtime.NewScheme()
			require.NoError(t, leaderv1.AddToScheme(scheme))
			builder := fake.NewClientBuilder().WithScheme(scheme)
			if !tc.absent {
				builder = builder.WithObjects(lws)
			}
			_, err := (&protectedLeaderWebhook{reader: builder.Build()}).ValidateDelete(context.Background(), pod)
			if tc.denied {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

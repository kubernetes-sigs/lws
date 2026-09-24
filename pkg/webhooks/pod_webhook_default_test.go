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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	leaderworkerset "sigs.k8s.io/lws/api/leaderworkerset/v1"
)

// ordinalPod builds an ordinal-identity pod as the statefulset controller would
// submit it, before admission fills in the derived labels.
func ordinalPod(name string, annotations, labels map[string]string) *corev1.Pod {
	base := map[string]string{leaderworkerset.SetNameLabelKey: "test-lws"}
	for k, v := range labels {
		base[k] = v
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   "default",
			Labels:      base,
			Annotations: annotations,
		},
		Spec: corev1.PodSpec{
			Subdomain:  "test-lws",
			Containers: []corev1.Container{{Name: "main"}},
		},
	}
}

func TestPodWebhookDefaultErrors(t *testing.T) {
	webhook := &PodWebhook{}
	ctx := context.Background()

	tests := map[string]struct {
		pod     *corev1.Pod
		wantErr string
	}{
		"missing size annotation": {
			pod: ordinalPod("test-lws-0", nil, map[string]string{
				leaderworkerset.WorkerIndexLabelKey: "0",
			}),
			wantErr: "size annotation is unexpectedly missing",
		},
		"non numeric size annotation": {
			pod: ordinalPod("test-lws-0", map[string]string{
				leaderworkerset.SizeAnnotationKey: "three",
			}, map[string]string{
				leaderworkerset.WorkerIndexLabelKey: "0",
			}),
			wantErr: "invalid syntax",
		},
		"leader pod name without an ordinal": {
			pod: ordinalPod("no-ordinal", map[string]string{
				leaderworkerset.SizeAnnotationKey: "2",
			}, map[string]string{
				leaderworkerset.WorkerIndexLabelKey: "0",
			}),
			wantErr: "parsing pod ordinal for pod no-ordinal",
		},
		"worker pod name without an ordinal": {
			pod: ordinalPod("no-ordinal", map[string]string{
				leaderworkerset.SizeAnnotationKey: "2",
			}, nil),
			wantErr: "parsing pod ordinal for pod no-ordinal",
		},
		"non numeric subgroup size annotation": {
			pod: ordinalPod("test-lws-0-1", map[string]string{
				leaderworkerset.SizeAnnotationKey:         "4",
				leaderworkerset.SubGroupSizeAnnotationKey: "two",
			}, nil),
			wantErr: "invalid syntax",
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			err := webhook.Default(ctx, tc.pod)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}

func TestPodWebhookDefaultSkipsNonLWSPods(t *testing.T) {
	webhook := &PodWebhook{}
	// A pod without the set-name label is not managed by LWS, so defaulting
	// must leave it completely untouched even though it has no annotations.
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "unrelated", Namespace: "default"}}
	original := pod.DeepCopy()

	require.NoError(t, webhook.Default(context.Background(), pod))
	assert.Equal(t, original, pod)
}

func TestPodWebhookDefaultLeaderAddress(t *testing.T) {
	webhook := &PodWebhook{}
	ctx := context.Background()

	t.Run("an explicit leader address annotation wins", func(t *testing.T) {
		pod := ordinalPod("test-lws-0-1", map[string]string{
			leaderworkerset.SizeAnnotationKey:          "2",
			leaderworkerset.LeaderAddressAnnotationKey: "test-lws-0.test-lws.default",
		}, nil)

		require.NoError(t, webhook.Default(ctx, pod))
		assert.Equal(t, "test-lws-0.test-lws.default", pod.Spec.Containers[0].Env[0].Value)
	})

	t.Run("the leader address is derived from the group index", func(t *testing.T) {
		pod := ordinalPod("test-lws-3", map[string]string{
			leaderworkerset.SizeAnnotationKey: "2",
		}, map[string]string{
			leaderworkerset.WorkerIndexLabelKey: "0",
		})

		require.NoError(t, webhook.Default(ctx, pod))
		assert.Equal(t, "3", pod.Labels[leaderworkerset.GroupIndexLabelKey])
		assert.Equal(t, "test-lws-3.test-lws.default", pod.Spec.Containers[0].Env[0].Value)
	})
}

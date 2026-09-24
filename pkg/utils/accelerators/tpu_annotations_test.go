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

package accelerator

import (
	"testing"

	"github.com/google/go-cmp/cmp"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

func tpuLeaderPod(requestTPUs bool) corev1.Pod {
	container := corev1.Container{Name: "leader", Image: "leader:latest"}
	if requestTPUs {
		container.Resources = corev1.ResourceRequirements{
			Limits: corev1.ResourceList{TpuResourceName: resource.MustParse("4")},
		}
	}
	return corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{container}}}
}

func TestAddTPUAnnotations(t *testing.T) {
	tests := []struct {
		name        string
		leaderPod   corev1.Pod
		annotations map[string]string
		want        map[string]string
	}{
		{
			name:        "leader requests TPUs",
			leaderPod:   tpuLeaderPod(true),
			annotations: map[string]string{},
			want:        map[string]string{LeaderRequestsTPUsAnnotationKey: "true"},
		},
		{
			name:        "leader does not request TPUs",
			leaderPod:   tpuLeaderPod(false),
			annotations: map[string]string{},
			want:        map[string]string{},
		},
		{
			name:        "existing annotations are preserved",
			leaderPod:   tpuLeaderPod(true),
			annotations: map[string]string{"example.com/key": "value"},
			want: map[string]string{
				"example.com/key":               "value",
				LeaderRequestsTPUsAnnotationKey: "true",
			},
		},
		{
			name:      "TPUs requested by an init container",
			leaderPod: corev1.Pod{Spec: corev1.PodSpec{InitContainers: tpuLeaderPod(true).Spec.Containers}},
			// The worker statefulset template needs the annotation even when
			// only an init container asks for TPUs.
			annotations: map[string]string{},
			want:        map[string]string{LeaderRequestsTPUsAnnotationKey: "true"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			AddTPUAnnotations(tc.leaderPod, tc.annotations)
			if diff := cmp.Diff(tc.want, tc.annotations); diff != "" {
				t.Errorf("unexpected annotations (-want +got): %s", diff)
			}
		})
	}
}

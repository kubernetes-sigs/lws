/*
Copyright 2024.

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

package utils

import (
	"testing"

	"github.com/google/go-cmp/cmp"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/utils/ptr"

	leaderworkerset "sigs.k8s.io/lws/api/leaderworkerset/v1"
)

func Test_SortByIndex(t *testing.T) {
	testCases := []struct {
		name      string
		inputs    []int
		length    int
		indexFunc func(int) (int, error)
		want      []int
	}{
		{
			name:      "inputs equal to the length",
			inputs:    []int{3, 2, 1, 0},
			length:    4,
			indexFunc: func(index int) (int, error) { return index, nil },
			want:      []int{0, 1, 2, 3},
		},
		{
			name:      "inputs less than the length",
			inputs:    []int{3, 1, 0},
			length:    4,
			indexFunc: func(index int) (int, error) { return index, nil },
			want:      []int{0, 1, 0, 3},
		},
		{
			name:      "inputs larger than the length",
			inputs:    []int{3, 0, 2, 5, 6, 7, 4},
			length:    4,
			indexFunc: func(index int) (int, error) { return index, nil },
			want:      []int{0, 0, 2, 3},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			got := SortByIndex(tc.indexFunc, tc.inputs, tc.length)

			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Errorf("unexpected result: (-want, +got) %s", diff)
			}
		})
	}
}

func TestCalculatePGMinResources(t *testing.T) {
	testCases := []struct {
		name string
		lws  *leaderworkerset.LeaderWorkerSet
		want corev1.ResourceList
	}{
		{
			name: "leader and worker with different resources",
			lws: &leaderworkerset.LeaderWorkerSet{
				Spec: leaderworkerset.LeaderWorkerSetSpec{
					LeaderWorkerTemplate: leaderworkerset.LeaderWorkerTemplate{
						Size: ptr.To[int32](3),
						LeaderTemplate: &corev1.PodTemplateSpec{
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{
									{
										Resources: corev1.ResourceRequirements{
											Requests: corev1.ResourceList{
												corev1.ResourceCPU:    resource.MustParse("1"),
												corev1.ResourceMemory: resource.MustParse("1Gi"),
											},
										},
									},
								},
							},
						},
						WorkerTemplate: corev1.PodTemplateSpec{
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{
									{
										Resources: corev1.ResourceRequirements{
											Requests: corev1.ResourceList{
												corev1.ResourceCPU:    resource.MustParse("2"),
												corev1.ResourceMemory: resource.MustParse("2Gi"),
											},
										},
									},
								},
							},
						},
					},
				},
			},
			want: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("5"),   // 1 (leader) + 2 * 2 (workers)
				corev1.ResourceMemory: resource.MustParse("5Gi"), // 1 (leader) + 2 * 2 (workers)
			},
		},
		{
			name: "only worker template specified",
			lws: &leaderworkerset.LeaderWorkerSet{
				Spec: leaderworkerset.LeaderWorkerSetSpec{
					LeaderWorkerTemplate: leaderworkerset.LeaderWorkerTemplate{
						Size: ptr.To[int32](3),
						WorkerTemplate: corev1.PodTemplateSpec{
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{
									{
										Resources: corev1.ResourceRequirements{
											Requests: corev1.ResourceList{
												corev1.ResourceCPU:    resource.MustParse("2"),
												corev1.ResourceMemory: resource.MustParse("2Gi"),
											},
										},
									},
								},
							},
						},
					},
				},
			},
			want: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("6"),   // 2 (leader) + 2 * 2 (workers)
				corev1.ResourceMemory: resource.MustParse("6Gi"), // 2 (leader) + 2 * 2 (workers)
			},
		},
		{
			name: "size is 1",
			lws: &leaderworkerset.LeaderWorkerSet{
				Spec: leaderworkerset.LeaderWorkerSetSpec{
					LeaderWorkerTemplate: leaderworkerset.LeaderWorkerTemplate{
						Size: ptr.To[int32](1),
						LeaderTemplate: &corev1.PodTemplateSpec{
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{
									{
										Resources: corev1.ResourceRequirements{
											Requests: corev1.ResourceList{
												corev1.ResourceCPU: resource.MustParse("1"),
											},
										},
									},
								},
							},
						},
					},
				},
			},
			want: corev1.ResourceList{
				corev1.ResourceCPU: resource.MustParse("1"),
			},
		},
		{
			name: "no resource requests",
			lws: &leaderworkerset.LeaderWorkerSet{
				Spec: leaderworkerset.LeaderWorkerSetSpec{
					LeaderWorkerTemplate: leaderworkerset.LeaderWorkerTemplate{
						Size: ptr.To[int32](3),
						WorkerTemplate: corev1.PodTemplateSpec{
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{{}},
							},
						},
					},
				},
			},
			want: corev1.ResourceList{},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			got := CalculatePGMinResources(tc.lws)
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Errorf("CalculatePGMinResources() mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

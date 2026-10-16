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

func Test_CalculatePGMinResources(t *testing.T) {
	// container returns a container requesting the given amount of cpu, so that
	// each test case can express expectations as plain multiples of that request.
	container := func(cpu string) corev1.Container {
		return corev1.Container{
			Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceCPU: resource.MustParse(cpu),
				},
			},
		}
	}
	podSpec := func(cpu string) corev1.PodSpec {
		return corev1.PodSpec{Containers: []corev1.Container{container(cpu)}}
	}

	testCases := []struct {
		name string
		lws  *leaderworkerset.LeaderWorkerSet
		want corev1.ResourceList
	}{
		{
			// Size counts the leader, so a group of 3 reserves one leader plus
			// two workers, not three workers.
			name: "leader plus size-1 workers",
			lws: &leaderworkerset.LeaderWorkerSet{
				Spec: leaderworkerset.LeaderWorkerSetSpec{
					LeaderWorkerTemplate: leaderworkerset.LeaderWorkerTemplate{
						Size:           ptr.To[int32](3),
						LeaderTemplate: &corev1.PodTemplateSpec{Spec: podSpec("1")},
						WorkerTemplate: corev1.PodTemplateSpec{Spec: podSpec("2")},
					},
				},
			},
			want: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("5")},
		},
		{
			// A group of one is just the leader; the size-1 worker term must not
			// underflow into a negative loop count.
			name: "size one reserves the leader only",
			lws: &leaderworkerset.LeaderWorkerSet{
				Spec: leaderworkerset.LeaderWorkerSetSpec{
					LeaderWorkerTemplate: leaderworkerset.LeaderWorkerTemplate{
						Size:           ptr.To[int32](1),
						LeaderTemplate: &corev1.PodTemplateSpec{Spec: podSpec("1")},
						WorkerTemplate: corev1.PodTemplateSpec{Spec: podSpec("2")},
					},
				},
			},
			want: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1")},
		},
		{
			// An unset size defaults to 1, which again means leader only. Guards
			// against the default changing without the reservation changing.
			name: "unset size defaults to one",
			lws: &leaderworkerset.LeaderWorkerSet{
				Spec: leaderworkerset.LeaderWorkerSetSpec{
					LeaderWorkerTemplate: leaderworkerset.LeaderWorkerTemplate{
						LeaderTemplate: &corev1.PodTemplateSpec{Spec: podSpec("1")},
						WorkerTemplate: corev1.PodTemplateSpec{Spec: podSpec("2")},
					},
				},
			},
			want: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1")},
		},
		{
			// Without a leader template the leader runs the worker spec, so the
			// reservation is size copies of the worker request.
			name: "no leader template falls back to the worker template",
			lws: &leaderworkerset.LeaderWorkerSet{
				Spec: leaderworkerset.LeaderWorkerSetSpec{
					LeaderWorkerTemplate: leaderworkerset.LeaderWorkerTemplate{
						Size:           ptr.To[int32](3),
						WorkerTemplate: corev1.PodTemplateSpec{Spec: podSpec("2")},
					},
				},
			},
			want: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("6")},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			got := CalculatePGMinResources(tc.lws)
			for name, wantQty := range tc.want {
				gotQty, ok := got[name]
				if !ok {
					t.Fatalf("resource %s missing from result %v", name, got)
				}
				if gotQty.Cmp(wantQty) != 0 {
					t.Errorf("%s: got %s, want %s", name, gotQty.String(), wantQty.String())
				}
			}
		})
	}
}

func Test_Sha1Hash(t *testing.T) {
	testCases := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "empty string",
			input: "",
			want:  "da39a3ee5e6b4b0d3255bfef95601890afd80709",
		},
		{
			name:  "namespaced pod name",
			input: "default/lws-0",
			want:  "9f7c49a9534189e737848b962d712086e1d5dfd8",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			// The digest must stay stable: it is embedded in pod labels, so a
			// change would recreate every group.
			if got := Sha1Hash(tc.input); got != tc.want {
				t.Errorf("Sha1Hash(%q) = %s, want %s", tc.input, got, tc.want)
			}
		})
	}
}

func Test_NonZeroValue(t *testing.T) {
	testCases := []struct {
		name  string
		input int32
		want  int32
	}{
		{name: "negative value is clamped to zero", input: -3, want: 0},
		{name: "zero stays zero", input: 0, want: 0},
		{name: "positive value is unchanged", input: 7, want: 7},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			if got := NonZeroValue(tc.input); got != tc.want {
				t.Errorf("NonZeroValue(%d) = %d, want %d", tc.input, got, tc.want)
			}
		})
	}
}

func Test_GetOperatorNamespace(t *testing.T) {
	// Outside a pod there is no service account namespace file, so the default
	// namespace is returned.
	if got := GetOperatorNamespace(); got != defaultNamespace {
		t.Errorf("GetOperatorNamespace() = %s, want %s", got, defaultNamespace)
	}
}

func Test_ExclusiveTopologyKey(t *testing.T) {
	testCases := []struct {
		name       string
		annotation *string
		policyType leaderworkerset.PlacementType
		topology   string
		wantKey    string
		wantOK     bool
	}{
		{name: "neither set"},
		{name: "annotation only", annotation: ptr.To("rack"), wantKey: "rack", wantOK: true},
		{name: "empty annotation", annotation: ptr.To("")},
		{name: "placementPolicy only", policyType: leaderworkerset.PlacementExclusiveTopology, topology: "block", wantKey: "block", wantOK: true},
		{name: "placementPolicy takes precedence over the annotation", annotation: ptr.To("rack"), policyType: leaderworkerset.PlacementExclusiveTopology, topology: "block", wantKey: "block", wantOK: true},
		{name: "placementPolicy None falls back to the annotation", annotation: ptr.To("rack"), policyType: leaderworkerset.PlacementNone, wantKey: "rack", wantOK: true},
		{name: "placementPolicy None without the annotation", policyType: leaderworkerset.PlacementNone},
	}

	for _, tc := range testCases {
		t.Run("group/"+tc.name, func(t *testing.T) {
			lws := &leaderworkerset.LeaderWorkerSet{}
			if tc.annotation != nil {
				lws.Annotations = map[string]string{leaderworkerset.ExclusiveKeyAnnotationKey: *tc.annotation}
			}
			if tc.policyType != "" {
				lws.Spec.PlacementPolicy = &leaderworkerset.PlacementPolicy{Type: tc.policyType, Topology: tc.topology}
			}
			if gotKey, gotOK := ExclusiveTopologyKey(lws); gotKey != tc.wantKey || gotOK != tc.wantOK {
				t.Errorf("ExclusiveTopologyKey() = (%q, %t), want (%q, %t)", gotKey, gotOK, tc.wantKey, tc.wantOK)
			}
		})
		t.Run("subgroup/"+tc.name, func(t *testing.T) {
			lws := &leaderworkerset.LeaderWorkerSet{}
			if tc.annotation != nil {
				lws.Annotations = map[string]string{leaderworkerset.SubGroupExclusiveKeyAnnotationKey: *tc.annotation}
			}
			lws.Spec.LeaderWorkerTemplate.SubGroupPolicy = &leaderworkerset.SubGroupPolicy{SubGroupSize: ptr.To[int32](2)}
			if tc.policyType != "" {
				lws.Spec.PlacementPolicy = &leaderworkerset.PlacementPolicy{
					Type:     leaderworkerset.PlacementNone,
					SubGroup: &leaderworkerset.SubGroupPlacementPolicy{Type: tc.policyType, Topology: tc.topology},
				}
			}
			if gotKey, gotOK := SubGroupExclusiveTopologyKey(lws); gotKey != tc.wantKey || gotOK != tc.wantOK {
				t.Errorf("SubGroupExclusiveTopologyKey() = (%q, %t), want (%q, %t)", gotKey, gotOK, tc.wantKey, tc.wantOK)
			}
		})
	}

	t.Run("subgroup/annotation without subGroupPolicy", func(t *testing.T) {
		lws := &leaderworkerset.LeaderWorkerSet{}
		lws.Annotations = map[string]string{leaderworkerset.SubGroupExclusiveKeyAnnotationKey: "rack"}
		if gotKey, gotOK := SubGroupExclusiveTopologyKey(lws); gotKey != "" || gotOK {
			t.Errorf("SubGroupExclusiveTopologyKey() = (%q, %t), want (\"\", false)", gotKey, gotOK)
		}
	})
}

func Test_EffectivePlacementPolicy(t *testing.T) {
	exclusive := func(topology string) *leaderworkerset.PlacementPolicy {
		return &leaderworkerset.PlacementPolicy{Type: leaderworkerset.PlacementExclusiveTopology, Topology: topology}
	}
	testCases := []struct {
		name        string
		annotations map[string]string
		policy      *leaderworkerset.PlacementPolicy
		want        *leaderworkerset.PlacementPolicy
	}{
		{name: "no placement"},
		{name: "None field without annotations", policy: &leaderworkerset.PlacementPolicy{Type: leaderworkerset.PlacementNone}},
		{
			name:        "annotations",
			annotations: map[string]string{leaderworkerset.ExclusiveKeyAnnotationKey: "rack", leaderworkerset.SubGroupExclusiveKeyAnnotationKey: "host"},
			want: &leaderworkerset.PlacementPolicy{Type: leaderworkerset.PlacementExclusiveTopology, Topology: "rack",
				SubGroup: &leaderworkerset.SubGroupPlacementPolicy{Type: leaderworkerset.PlacementExclusiveTopology, Topology: "host"}},
		},
		{
			name: "field",
			policy: &leaderworkerset.PlacementPolicy{Type: leaderworkerset.PlacementExclusiveTopology, Topology: "rack",
				SubGroup: &leaderworkerset.SubGroupPlacementPolicy{Type: leaderworkerset.PlacementExclusiveTopology, Topology: "host"}},
			want: &leaderworkerset.PlacementPolicy{Type: leaderworkerset.PlacementExclusiveTopology, Topology: "rack",
				SubGroup: &leaderworkerset.SubGroupPlacementPolicy{Type: leaderworkerset.PlacementExclusiveTopology, Topology: "host"}},
		},
		{
			name:        "group field and subgroup annotation",
			annotations: map[string]string{leaderworkerset.SubGroupExclusiveKeyAnnotationKey: "host"},
			policy:      exclusive("rack"),
			want: &leaderworkerset.PlacementPolicy{Type: leaderworkerset.PlacementExclusiveTopology, Topology: "rack",
				SubGroup: &leaderworkerset.SubGroupPlacementPolicy{Type: leaderworkerset.PlacementExclusiveTopology, Topology: "host"}},
		},
		{
			name:        "subgroup annotation only",
			annotations: map[string]string{leaderworkerset.SubGroupExclusiveKeyAnnotationKey: "host"},
			want: &leaderworkerset.PlacementPolicy{Type: leaderworkerset.PlacementNone,
				SubGroup: &leaderworkerset.SubGroupPlacementPolicy{Type: leaderworkerset.PlacementExclusiveTopology, Topology: "host"}},
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			lws := &leaderworkerset.LeaderWorkerSet{}
			lws.Annotations = tc.annotations
			lws.Spec.PlacementPolicy = tc.policy
			lws.Spec.LeaderWorkerTemplate.SubGroupPolicy = &leaderworkerset.SubGroupPolicy{SubGroupSize: ptr.To[int32](2)}
			if diff := cmp.Diff(tc.want, EffectivePlacementPolicy(lws)); diff != "" {
				t.Errorf("EffectivePlacementPolicy() mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

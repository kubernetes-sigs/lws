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

package disaggregatedset

import (
	"testing"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	disaggregatedsetv1 "sigs.k8s.io/lws/api/disaggregatedset/v1"
	leaderworkersetv1 "sigs.k8s.io/lws/api/leaderworkerset/v1"
)

// TestSubRoleCounts checks that a malformed count annotation fails the read
// instead of seeding the scaler with wrong totals, and that only issued counts
// must add up to the physical replicas.
func TestSubRoleCounts(t *testing.T) {
	issued := subRoleReplicasAnnotation
	initial := disaggregatedsetv1.InitialSubRoleReplicasAnnotationKey
	for _, tc := range []struct {
		name     string
		key      string
		value    string
		replicas *int32
		want     map[string]int
		wantErr  string
	}{
		{name: "absent", key: issued},
		{name: "issued counts match replicas", key: issued, value: `{"a":2,"b":1}`, replicas: ptr.To[int32](3), want: map[string]int{"a": 2, "b": 1}},
		{name: "issued counts match default replicas", key: issued, value: `{"a":1}`, want: map[string]int{"a": 1}},
		{name: "issued counts differ from replicas", key: issued, value: `{"a":2,"b":2}`, replicas: ptr.To[int32](3), wantErr: "must sum to physical replicas"},
		{name: "initial counts may differ from replicas", key: initial, value: `{"a":2,"b":2}`, replicas: ptr.To[int32](0), want: map[string]int{"a": 2, "b": 2}},
		{name: "invalid JSON", key: issued, value: `{"a":`, wantErr: "invalid " + issued},
		{name: "null map", key: initial, value: `null`, wantErr: "must contain a count map"},
		{name: "negative count summing to replicas", key: issued, value: `{"a":-1,"b":2}`, wantErr: "invalid sub-role replica counts"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lws := &leaderworkersetv1.LeaderWorkerSet{Spec: leaderworkersetv1.LeaderWorkerSetSpec{Replicas: tc.replicas}}
			if tc.value != "" {
				lws.ObjectMeta = metav1.ObjectMeta{Annotations: map[string]string{tc.key: tc.value}}
			}
			counts, err := subRoleCounts(lws, tc.key)
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
				require.Nil(t, counts)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.want, counts)
		})
	}
}

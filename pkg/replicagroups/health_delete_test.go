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

package replicagroups

import (
	"testing"

	"github.com/stretchr/testify/require"

	leaderv1 "sigs.k8s.io/lws/api/leaderworkerset/v1"
)

func TestHealthDeleteIntentIsNotRetainedReadiness(t *testing.T) {
	for _, identity := range []leaderv1.GroupIdentityType{leaderv1.GroupIdentityOrdinal, leaderv1.GroupIdentityHash} {
		for _, exactUID := range []bool{false, true} {
			t.Run(string(identity)+"/"+map[bool]string{false: "old UID", true: "exact UID"}[exactUID], func(t *testing.T) {
				f := newFixture(identity, 2, 3)
				leader := f.leaders[0]
				if leader.Annotations == nil {
					leader.Annotations = map[string]string{}
				}
				leader.Annotations[leaderv1.GroupReplacementDeleteAnnotationKey] = "old-uid"
				if exactUID {
					leader.Annotations[leaderv1.GroupReplacementDeleteAnnotationKey] = string(leader.UID)
				}
				snapshot, err := Observe(t.Context(), newReader(t, f.objects()...), f.lws)
				require.NoError(t, err)
				want := int32(2)
				if exactUID {
					want--
				}
				require.Equal(t, Availability{ReadyReplicas: 2, RetainedReadyReplicas: want}, snapshot.Availability())
			})
		}
	}
}

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
	"context"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	disaggv1 "sigs.k8s.io/lws/api/disaggregatedset/v1"
	leaderworkerset "sigs.k8s.io/lws/api/leaderworkerset/v1"
)

// With role "prefill" and one slice, the generated LWS name is the DS name plus
// 19 characters: "-0-", an 8 character revision, "-prefill".
func TestValidateCreateHashGeneratedNames(t *testing.T) {
	tests := []struct {
		name      string
		dsNameLen int
		size      int32
		wantErr   bool
	}{
		{name: "size 2 at the limit", dsNameLen: 24, size: 2},
		{name: "size 2 over the limit", dsNameLen: 25, size: 2, wantErr: true},
		{name: "size 1 at the limit", dsNameLen: 35, size: 1},
		{name: "size 1 over the limit", dsNameLen: 36, size: 1, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ds := &disaggv1.DisaggregatedSet{
				ObjectMeta: metav1.ObjectMeta{Name: strings.Repeat("a", tc.dsNameLen), Namespace: "default"},
				Spec: disaggv1.DisaggregatedSetSpec{
					Roles: []disaggv1.DisaggregatedRoleSpec{{
						Name: "prefill",
						LeaderWorkerSetTemplateSpec: leaderworkerset.LeaderWorkerSetTemplateSpec{Spec: leaderworkerset.LeaderWorkerSetSpec{
							Replicas:             ptr.To(int32(2)),
							GroupIdentity:        leaderworkerset.GroupIdentityHash,
							LeaderWorkerTemplate: leaderworkerset.LeaderWorkerTemplate{Size: ptr.To(tc.size)},
						}},
					}},
				},
			}
			_, err := (&DisaggregatedSetWebhook{}).ValidateCreate(context.Background(), ds)
			if gotErr := err != nil; gotErr != tc.wantErr {
				t.Errorf("ValidateCreate() error = %v, wantErr %t", err, tc.wantErr)
			}
		})
	}
}

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
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	disaggv1 "sigs.k8s.io/lws/api/disaggregatedset/v1"
	leaderworkerset "sigs.k8s.io/lws/api/leaderworkerset/v1"
)

var externalScaling = &disaggv1.RoleScaling{Mode: disaggv1.RoleScalingExternal}

func generatedNamesDS(nameLen int, roles ...disaggv1.DisaggregatedRoleSpec) *disaggv1.DisaggregatedSet {
	return &disaggv1.DisaggregatedSet{
		ObjectMeta: metav1.ObjectMeta{Name: strings.Repeat("a", nameLen), Namespace: "default"},
		Spec:       disaggv1.DisaggregatedSetSpec{Roles: roles},
	}
}

func externalPrefill() disaggv1.DisaggregatedRoleSpec {
	return disaggv1.DisaggregatedRoleSpec{Name: "prefill", Scaling: externalScaling}
}

func staticPrefill() disaggv1.DisaggregatedRoleSpec {
	return disaggv1.DisaggregatedRoleSpec{
		Name:                        "prefill",
		LeaderWorkerSetTemplateSpec: leaderworkerset.LeaderWorkerSetTemplateSpec{Spec: leaderworkerset.LeaderWorkerSetSpec{Replicas: ptr.To[int32](1)}},
	}
}

func partitionedPrefill(children ...disaggv1.DisaggregatedSubRoleSpec) disaggv1.DisaggregatedRoleSpec {
	return disaggv1.DisaggregatedRoleSpec{Name: "prefill", SubRoles: children}
}

// With role "prefill" and one slice, the generated LWS name is the DS name plus
// 19 characters: "-0-", an 8 character revision, "-prefill". The worker
// StatefulSet appends "-<groupIndex>", and its Pods' controller-revision-hash
// label appends up to 11 more. A new External role reserves 8 digits for the
// group index, as many as a hash group key takes: the DS name can have at most
// 63 - 19 - 1 - 8 - 11 = 24 characters.
func TestValidateCreateExternalGeneratedNames(t *testing.T) {
	hash := externalPrefill()
	hash.Spec.GroupIdentity = leaderworkerset.GroupIdentityHash
	hash.Spec.LeaderWorkerTemplate.Size = ptr.To[int32](2)
	for _, tc := range []struct {
		name      string
		nameLen   int
		role      disaggv1.DisaggregatedRoleSpec
		wantError bool
	}{
		{name: "External at the limit", nameLen: 24, role: externalPrefill()},
		{name: "External over the limit", nameLen: 25, role: externalPrefill(), wantError: true},
		{name: "External accepted before for one group", nameLen: 31, role: externalPrefill(), wantError: true},
		{name: "Static keeps its replica-sized budget", nameLen: 31, role: staticPrefill()},
		{name: "External sub-role over the limit", nameLen: 25, role: partitionedPrefill(
			disaggv1.DisaggregatedSubRoleSpec{Name: "a"},
			disaggv1.DisaggregatedSubRoleSpec{Name: "b", Scaling: externalScaling},
		), wantError: true},
		{name: "External sub-role at the limit", nameLen: 24, role: partitionedPrefill(
			disaggv1.DisaggregatedSubRoleSpec{Name: "b", Scaling: externalScaling},
		)},
		{name: "Static sub-roles keep their replica-sized budget", nameLen: 31, role: partitionedPrefill(
			disaggv1.DisaggregatedSubRoleSpec{Name: "a"},
		)},
		// Hash names do not contain a group index.
		{name: "External with groupIdentity Hash", nameLen: 24, role: hash},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := (&DisaggregatedSetWebhook{}).ValidateCreate(t.Context(), generatedNamesDS(tc.nameLen, tc.role))
			if !tc.wantError {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			require.Contains(t, err.Error(), "would exceed the DNS-1035 limit")
			require.Contains(t, err.Error(), "scaling.mode External reserves 8 characters for the group index")
		})
	}
}

// The manager option changes the reserved digits: the DS name can have at most
// 63 - 19 - 1 - digits - 11 characters, and the error names the option.
func TestValidateCreateExternalGroupIndexDigitsOption(t *testing.T) {
	for _, tc := range []struct {
		digits, maxNameLen int32
		wantInError        string
	}{
		{digits: 1, maxNameLen: 31, wantInError: "reserves 1 characters"},
		{digits: 6, maxNameLen: 26, wantInError: "reserves 6 characters"},
		{digits: 10, maxNameLen: 22, wantInError: "reserves 10 characters"},
		// Zero is what the manager passes when the option is not set.
		{digits: 0, maxNameLen: 24, wantInError: "reserves 8 characters"},
	} {
		t.Run(tc.wantInError, func(t *testing.T) {
			hook := &DisaggregatedSetWebhook{ExternalGroupIndexDigits: tc.digits}
			_, err := hook.ValidateCreate(t.Context(), generatedNamesDS(int(tc.maxNameLen), externalPrefill()))
			require.NoError(t, err)

			_, err = hook.ValidateCreate(t.Context(), generatedNamesDS(int(tc.maxNameLen)+1, externalPrefill()))
			require.Error(t, err)
			require.Contains(t, err.Error(), tc.wantInError)
			require.Contains(t, err.Error(), "disaggregatedSetExternalGroupIndexDigits")
		})
	}
}

// Only a role or sub-role that becomes External, or an External one that leaves
// groupIdentity Hash and so gains a group index in its names, is held to the
// External budget, so stored DisaggregatedSets accepted earlier stay updatable.
func TestValidateUpdateExternalGeneratedNames(t *testing.T) {
	relabeled := func(role disaggv1.DisaggregatedRoleSpec) disaggv1.DisaggregatedRoleSpec {
		role.Labels = map[string]string{"team": "serving"}
		return role
	}
	withHash := func(role disaggv1.DisaggregatedRoleSpec) disaggv1.DisaggregatedRoleSpec {
		role.Spec.GroupIdentity = leaderworkerset.GroupIdentityHash
		role.Spec.LeaderWorkerTemplate.Size = ptr.To[int32](1)
		return role
	}
	externalSubRole := func() disaggv1.DisaggregatedRoleSpec {
		return partitionedPrefill(disaggv1.DisaggregatedSubRoleSpec{Name: "a", Scaling: externalScaling})
	}
	for _, tc := range []struct {
		name      string
		old, new  disaggv1.DisaggregatedRoleSpec
		wantError bool
	}{
		{name: "existing External role", old: externalPrefill(), new: relabeled(externalPrefill())},
		{name: "Static role becomes External", old: staticPrefill(), new: externalPrefill(), wantError: true},
		{
			name: "existing External sub-role",
			old:  partitionedPrefill(disaggv1.DisaggregatedSubRoleSpec{Name: "a", Scaling: externalScaling}),
			new:  relabeled(partitionedPrefill(disaggv1.DisaggregatedSubRoleSpec{Name: "a", Scaling: externalScaling})),
		},
		{
			name: "new External sub-role",
			old:  partitionedPrefill(disaggv1.DisaggregatedSubRoleSpec{Name: "a", Scaling: externalScaling}),
			new: partitionedPrefill(
				disaggv1.DisaggregatedSubRoleSpec{Name: "a", Scaling: externalScaling},
				disaggv1.DisaggregatedSubRoleSpec{Name: "b", Scaling: externalScaling},
			),
			wantError: true,
		},
		{name: "External role leaves Hash", old: withHash(externalPrefill()), new: externalPrefill(), wantError: true},
		{name: "External sub-role leaves Hash", old: withHash(externalSubRole()), new: externalSubRole(), wantError: true},
		{name: "External role stays Hash", old: withHash(externalPrefill()), new: relabeled(withHash(externalPrefill()))},
		{name: "Static role leaves Hash", old: withHash(staticPrefill()), new: staticPrefill()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := (&DisaggregatedSetWebhook{}).ValidateUpdate(t.Context(), generatedNamesDS(31, tc.old), generatedNamesDS(31, tc.new))
			if !tc.wantError {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			require.Contains(t, err.Error(), "scaling.mode External reserves 8 characters for the group index")
		})
	}
}

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

package schedulerprovider

import (
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	schedulingv1alpha3 "k8s.io/api/scheduling/v1alpha3"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	leaderworkerset "sigs.k8s.io/lws/api/leaderworkerset/v1"
)

func TestPodGroupNames(t *testing.T) {
	assert.Equal(t, "lws-1-dd6699c7c", GetPodGroupName("lws", "1", "dd6699c7c"))
	assert.Equal(t, "lws-1-leader-dd6699c7c", GetRolePodGroupName("lws", "1", "leader", "dd6699c7c"))
	assert.Equal(t, "lws-1-worker-dd6699c7c", GetRolePodGroupName("lws", "1", "worker", "dd6699c7c"))
	assert.Equal(t, "lws-lws", GetLWSGroupName("lws"))

	// The role name keeps leader and worker PodGroups of the same replica apart.
	assert.NotEqual(t,
		GetRolePodGroupName("lws", "1", "leader", "dd6699c7c"),
		GetRolePodGroupName("lws", "1", "worker", "dd6699c7c"))
}

func TestReconcileError(t *testing.T) {
	cause := errors.New("api server said no")
	err := NewReconcileError(ReasonWorkloadCreateFailed, cause)

	// The underlying error stays visible for retries and diagnostics.
	assert.Equal(t, cause.Error(), err.Error())
	assert.ErrorIs(t, err, cause)

	var reconcileErr *ReconcileError
	require.ErrorAs(t, err, &reconcileErr)
	assert.Equal(t, ReasonWorkloadCreateFailed, reconcileErr.Reason)
}

func TestReconcileErrorReason(t *testing.T) {
	tests := map[string]struct {
		err  error
		want string
	}{
		"nil error falls back to the invalid configuration reason": {
			err:  nil,
			want: ReasonInvalidSchedulingConfiguration,
		},
		"plain error falls back to the invalid configuration reason": {
			err:  errors.New("boom"),
			want: ReasonInvalidSchedulingConfiguration,
		},
		"reconcile error carries its reason": {
			err:  NewReconcileError(ReasonAPINotAvailable, errors.New("boom")),
			want: ReasonAPINotAvailable,
		},
		"wrapped reconcile error still carries its reason": {
			err:  fmt.Errorf("reconciling: %w", NewReconcileError(ReasonPodGroupCreateFailed, errors.New("boom"))),
			want: ReasonPodGroupCreateFailed,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, tc.want, ReconcileErrorReason(tc.err))
		})
	}
}

func TestNewSchedulerProvider(t *testing.T) {
	fakeClient := fake.NewClientBuilder().Build()

	tests := map[string]struct {
		providerType ProviderType
		wantErr      string
	}{
		"volcano":     {providerType: Volcano},
		"kubernetes":  {providerType: Kubernetes},
		"unsupported": {providerType: ProviderType("yarn"), wantErr: "unsupported scheduler provider type yarn"},
		"empty":       {providerType: ProviderType(""), wantErr: "unsupported scheduler provider type"},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			provider, err := NewSchedulerProvider(tc.providerType, fakeClient)
			if tc.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.wantErr)
				assert.Nil(t, provider)
				return
			}
			require.NoError(t, err)
			assert.NotNil(t, provider)
		})
	}
}

func TestSupportedSchedulerProviders(t *testing.T) {
	// Every supported name must map to a provider, otherwise the error message
	// produced by NewSchedulerProvider would advertise an unusable option.
	for _, name := range SupportedSchedulerProviders.UnsortedList() {
		provider, err := NewSchedulerProvider(ProviderType(name), fake.NewClientBuilder().Build())
		require.NoErrorf(t, err, "provider %s is advertised as supported", name)
		assert.NotNil(t, provider)
	}
}

func TestWorkloadSchedulingValue(t *testing.T) {
	tests := map[string]struct {
		mutate func(*leaderworkerset.LeaderWorkerSet)
		want   string
	}{
		"empty scheduling is replica mode": {
			want: string(SchedulingModeReplica),
		},
		"lws level": {
			mutate: func(lws *leaderworkerset.LeaderWorkerSet) {
				lws.Spec.Scheduling.SchedulingConstraints = &schedulingv1alpha3.WorkloadCompositePodGroupSchedulingConstraints{}
			},
			want: string(SchedulingModeLWS),
		},
		"role level": {
			mutate: func(lws *leaderworkerset.LeaderWorkerSet) {
				lws.Spec.Scheduling.Replica = &leaderworkerset.LeaderWorkerSetReplicaScheduling{
					Leader: &leaderworkerset.LeaderWorkerSetLeaderScheduling{},
				}
			},
			want: string(SchedulingModeRole),
		},
		"unset scheduling falls back to replica": {
			mutate: func(lws *leaderworkerset.LeaderWorkerSet) {
				lws.Spec.Scheduling = nil
			},
			want: string(SchedulingModeReplica),
		},
		"ambiguous scheduling falls back to replica": {
			mutate: func(lws *leaderworkerset.LeaderWorkerSet) {
				lws.Spec.Scheduling.SchedulingConstraints = &schedulingv1alpha3.WorkloadCompositePodGroupSchedulingConstraints{}
				lws.Spec.Scheduling.Replica = &leaderworkerset.LeaderWorkerSetReplicaScheduling{}
			},
			want: string(SchedulingModeReplica),
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			lws := testScheduledLWS()
			if tc.mutate != nil {
				tc.mutate(lws)
			}
			assert.Equal(t, tc.want, WorkloadSchedulingValue(lws))
		})
	}
}

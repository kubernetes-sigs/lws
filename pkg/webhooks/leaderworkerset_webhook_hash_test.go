/*
Copyright 2026.

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

	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/utils/ptr"

	v1 "sigs.k8s.io/lws/api/leaderworkerset/v1"
	"sigs.k8s.io/lws/test/wrappers"
)

func hashLws(name string) *v1.LeaderWorkerSet {
	lws := wrappers.BuildBasicLeaderWorkerSet(name, "default").
		Replica(2).
		RolloutStrategy(v1.RolloutStrategy{
			Type: v1.RollingUpdateStrategyType,
			RollingUpdateConfiguration: &v1.RollingUpdateConfiguration{
				MaxUnavailable: intstr.FromInt32(1),
				Partition:      ptr.To[int32](0),
			},
		}).
		WorkerTemplateSpec(wrappers.MakeWorkerPodSpec()).
		Size(2).
		RestartPolicy(v1.RecreateGroupOnPodRestart).Obj()
	lws.Spec.GroupIdentity = v1.GroupIdentityHash
	return lws
}

func TestValidateHashGroupIdentity(t *testing.T) {
	webhook := &LeaderWorkerSetWebhook{}

	valid := hashLws("valid")
	if _, err := webhook.ValidateCreate(context.TODO(), valid); err != nil {
		t.Errorf("valid hash lws rejected: %v", err)
	}

	subGroup := hashLws("subgroup")
	subGroup.Spec.LeaderWorkerTemplate.SubGroupPolicy = &v1.SubGroupPolicy{SubGroupSize: ptr.To[int32](2)}
	if _, err := webhook.ValidateCreate(context.TODO(), subGroup); err != nil {
		t.Errorf("expected subGroupPolicy to be accepted with groupIdentity Hash: %v", err)
	}

	partitioned := hashLws("partitioned")
	partitioned.Spec.RolloutStrategy.RollingUpdateConfiguration.Partition = ptr.To[int32](1)
	if _, err := webhook.ValidateCreate(context.TODO(), partitioned); err == nil {
		t.Error("expected non-zero partition to be rejected with groupIdentity Hash")
	}

	uniqueSubdomain := hashLws("subdomain")
	policy := v1.SubdomainUniquePerReplica
	uniqueSubdomain.Spec.NetworkConfig = &v1.NetworkConfig{SubdomainPolicy: &policy}
	if _, err := webhook.ValidateCreate(context.TODO(), uniqueSubdomain); err != nil {
		t.Errorf("expected UniquePerReplica subdomain policy to be accepted with groupIdentity Hash: %v", err)
	}

	// Workload-aware scheduling is supported with hash-named groups: the
	// per-replica PodGroups are materialized from the group key that admission
	// draws for every leader pod.
	scheduled := hashLws("scheduled")
	scheduled.Spec.Scheduling = &v1.LeaderWorkerSetScheduling{}
	if errs := ValidateGroupIdentity(field.NewPath("spec"), &scheduled.Spec); len(errs) > 0 {
		t.Errorf("expected scheduling to be accepted with groupIdentity Hash: %v", errs)
	}
}

func TestGroupIdentityImmutable(t *testing.T) {
	webhook := &LeaderWorkerSetWebhook{}

	oldLws := hashLws("immutable")
	newLws := oldLws.DeepCopy()
	newLws.Spec.GroupIdentity = v1.GroupIdentityOrdinal
	if _, err := webhook.ValidateUpdate(context.TODO(), oldLws, newLws); err == nil {
		t.Error("expected groupIdentity change Hash -> Ordinal to be rejected")
	}

	// Empty and Ordinal are the same identity scheme; changing between them is allowed.
	oldDefault := hashLws("default")
	oldDefault.Spec.GroupIdentity = ""
	newDefault := oldDefault.DeepCopy()
	newDefault.Spec.GroupIdentity = v1.GroupIdentityOrdinal
	if _, err := webhook.ValidateUpdate(context.TODO(), oldDefault, newDefault); err != nil {
		t.Errorf("empty -> Ordinal should be allowed, got: %v", err)
	}
}

func TestGroupReplacementPolicyDefaultAndValidation(t *testing.T) {
	webhook := &LeaderWorkerSetWebhook{}

	defaulted := hashLws("defaulted")
	if err := webhook.Default(context.TODO(), defaulted); err != nil {
		t.Fatalf("defaulting lws: %v", err)
	}
	if defaulted.Spec.GroupReplacementPolicy != v1.GroupReplacementPostTermination {
		t.Errorf("expected groupReplacementPolicy to default to PostTermination, got %q", defaulted.Spec.GroupReplacementPolicy)
	}

	immediateHash := hashLws("immediate-hash")
	immediateHash.Spec.GroupReplacementPolicy = v1.GroupReplacementImmediate
	if _, err := webhook.ValidateCreate(context.TODO(), immediateHash); err != nil {
		t.Errorf("expected Immediate to be accepted with groupIdentity Hash: %v", err)
	}

	immediateOrdinal := hashLws("immediate-ordinal")
	immediateOrdinal.Spec.GroupIdentity = v1.GroupIdentityOrdinal
	immediateOrdinal.Spec.GroupReplacementPolicy = v1.GroupReplacementImmediate
	if _, err := webhook.ValidateCreate(context.TODO(), immediateOrdinal); err == nil {
		t.Error("expected Immediate to be rejected with groupIdentity Ordinal")
	}

	postTerminationOrdinal := hashLws("post-termination-ordinal")
	postTerminationOrdinal.Spec.GroupIdentity = v1.GroupIdentityOrdinal
	postTerminationOrdinal.Spec.GroupReplacementPolicy = v1.GroupReplacementPostTermination
	if _, err := webhook.ValidateCreate(context.TODO(), postTerminationOrdinal); err != nil {
		t.Errorf("expected PostTermination to be accepted with groupIdentity Ordinal: %v", err)
	}
}

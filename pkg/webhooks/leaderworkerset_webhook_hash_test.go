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
	if _, err := webhook.ValidateCreate(context.TODO(), subGroup); err == nil {
		t.Error("expected subGroupPolicy to be rejected with groupIdentity Hash")
	}

	partitioned := hashLws("partitioned")
	partitioned.Spec.RolloutStrategy.RollingUpdateConfiguration.Partition = ptr.To[int32](1)
	if _, err := webhook.ValidateCreate(context.TODO(), partitioned); err == nil {
		t.Error("expected non-zero partition to be rejected with groupIdentity Hash")
	}

	uniqueSubdomain := hashLws("subdomain")
	policy := v1.SubdomainUniquePerReplica
	uniqueSubdomain.Spec.NetworkConfig = &v1.NetworkConfig{SubdomainPolicy: &policy}
	if _, err := webhook.ValidateCreate(context.TODO(), uniqueSubdomain); err == nil {
		t.Error("expected UniquePerReplica subdomain policy to be rejected with groupIdentity Hash")
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

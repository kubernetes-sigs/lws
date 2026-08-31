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

package controllers

import (
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	leaderworkerset "sigs.k8s.io/lws/api/leaderworkerset/v1"
	"sigs.k8s.io/lws/test/wrappers"
)

func TestLeaderDeploymentApplyConfig(t *testing.T) {
	lws := wrappers.BuildBasicLeaderWorkerSet("test-hash", "default").
		Replica(4).
		RolloutStrategy(leaderworkerset.RolloutStrategy{
			Type: leaderworkerset.RollingUpdateStrategyType,
			RollingUpdateConfiguration: &leaderworkerset.RollingUpdateConfiguration{
				MaxUnavailable: intstr.FromInt32(1),
				MaxSurge:       intstr.FromInt32(2),
			},
		}).
		WorkerTemplateSpec(wrappers.MakeWorkerPodSpec()).
		Size(2).
		RestartPolicy(leaderworkerset.RecreateGroupOnPodRestart).Obj()
	lws.Spec.GroupIdentity = leaderworkerset.GroupIdentityHash

	deployConfig, err := constructLeaderDeploymentApplyConfiguration(lws, "rev-1")
	if err != nil {
		t.Fatal(err)
	}

	if got := *deployConfig.Spec.Replicas; got != 4 {
		t.Errorf("replicas = %d, want 4", got)
	}
	if got := deployConfig.Spec.Selector.MatchLabels[leaderworkerset.SetNameLabelKey]; got != "test-hash" {
		t.Errorf("selector set-name = %q, want test-hash", got)
	}
	if got := deployConfig.Spec.Selector.MatchLabels[leaderworkerset.WorkerIndexLabelKey]; got != "0" {
		t.Errorf("selector worker-index = %q, want 0", got)
	}
	if got := *deployConfig.Spec.Strategy.Type; got != appsv1.RollingUpdateDeploymentStrategyType {
		t.Errorf("strategy type = %q, want RollingUpdate", got)
	}
	if got := *deployConfig.Spec.Strategy.RollingUpdate.MaxUnavailable; got != intstr.FromInt32(1) {
		t.Errorf("maxUnavailable = %v, want 1", got)
	}
	if got := *deployConfig.Spec.Strategy.RollingUpdate.MaxSurge; got != intstr.FromInt32(2) {
		t.Errorf("maxSurge = %v, want 2", got)
	}

	template := deployConfig.Spec.Template
	if got := template.Labels[leaderworkerset.RevisionKey]; got != "rev-1" {
		t.Errorf("template revision label = %q, want rev-1", got)
	}
	if got := template.Labels[leaderworkerset.WorkerIndexLabelKey]; got != "0" {
		t.Errorf("template worker-index label = %q, want 0", got)
	}
	if got := template.Annotations[leaderworkerset.GroupIdentityAnnotationKey]; got != string(leaderworkerset.GroupIdentityHash) {
		t.Errorf("template group-identity annotation = %q, want Hash", got)
	}
	if got := template.Annotations[leaderworkerset.SizeAnnotationKey]; got != "2" {
		t.Errorf("template size annotation = %q, want 2", got)
	}

	if got := template.Spec.Subdomain; got == nil || *got != "test-hash" {
		t.Errorf("template subdomain = %v, want test-hash", got)
	}

	foundGate := false
	for _, gate := range template.Spec.ReadinessGates {
		if gate.ConditionType != nil && *gate.ConditionType == leaderworkerset.GroupReadyConditionType {
			foundGate = true
		}
	}
	if !foundGate {
		t.Error("expected group-ready readiness gate on leader template with size > 1")
	}
}

func TestLeaderDeploymentApplyConfigSizeOneHasNoGate(t *testing.T) {
	lws := wrappers.BuildBasicLeaderWorkerSet("test-hash", "default").
		Replica(2).
		RolloutStrategy(leaderworkerset.RolloutStrategy{
			Type: leaderworkerset.RollingUpdateStrategyType,
			RollingUpdateConfiguration: &leaderworkerset.RollingUpdateConfiguration{
				MaxUnavailable: intstr.FromInt32(1),
			},
		}).
		WorkerTemplateSpec(wrappers.MakeWorkerPodSpec()).
		Size(1).
		RestartPolicy(leaderworkerset.RecreateGroupOnPodRestart).Obj()
	lws.Spec.GroupIdentity = leaderworkerset.GroupIdentityHash

	deployConfig, err := constructLeaderDeploymentApplyConfiguration(lws, "rev-1")
	if err != nil {
		t.Fatal(err)
	}
	for _, gate := range deployConfig.Spec.Template.Spec.ReadinessGates {
		if gate.ConditionType != nil && *gate.ConditionType == leaderworkerset.GroupReadyConditionType {
			t.Error("size 1 groups must not carry the group-ready readiness gate")
		}
	}
}

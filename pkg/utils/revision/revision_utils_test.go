/*
Copyright 2023.

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

package revision

import (
	"context"
	"testing"

	"github.com/google/go-cmp/cmp"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	leaderworkerset "sigs.k8s.io/lws/api/leaderworkerset/v1"
)

func TestApplyRevision(t *testing.T) {

	client := fake.NewClientBuilder().Build()

	lws := BuildLeaderWorkerSet("default")
	revision, err := NewRevision(context.TODO(), client, lws, "")
	if err != nil {
		t.Fatal(err)
	}
	currentLws := lws.DeepCopy()

	lws.Spec.LeaderWorkerTemplate.LeaderTemplate.Spec.Containers[0].Name = "update-name"
	subdomainPolicy := leaderworkerset.SubdomainUniquePerReplica
	lws.Spec.NetworkConfig = &leaderworkerset.NetworkConfig{
		SubdomainPolicy: &subdomainPolicy,
	}
	restoredLws, err := ApplyRevision(lws, revision)
	if err != nil {
		t.Fatal(err)
	}

	restoredRevision, err := NewRevision(context.TODO(), client, restoredLws, "")
	if err != nil {
		t.Fatal(err)
	}

	if !EqualRevision(revision, restoredRevision) {
		t.Errorf("expected value %v, got %v", revision, restoredRevision)
	}

	if diff := cmp.Diff(currentLws.Spec.LeaderWorkerTemplate, restoredLws.Spec.LeaderWorkerTemplate); diff != "" {
		t.Errorf("unexpected restored LeaderWorkerTemplate: %s", diff)
	}

	if diff := cmp.Diff(currentLws.Spec.NetworkConfig, restoredLws.Spec.NetworkConfig); diff != "" {
		t.Errorf("NetworkConfig should be restored %s", diff)
	}
}

func BuildLeaderWorkerSet(nsName string) *leaderworkerset.LeaderWorkerSet {
	lws := leaderworkerset.LeaderWorkerSet{}
	lws.Name = "test-sample"
	lws.Namespace = nsName
	lws.Spec = leaderworkerset.LeaderWorkerSetSpec{}
	lws.Spec.Replicas = ptr.To[int32](2)
	lws.Spec.LeaderWorkerTemplate = leaderworkerset.LeaderWorkerTemplate{RestartPolicy: leaderworkerset.RecreateGroupOnPodRestart}
	lws.Spec.LeaderWorkerTemplate.Size = ptr.To[int32](2)
	lws.Spec.LeaderWorkerTemplate.LeaderTemplate = &corev1.PodTemplateSpec{}
	lws.Spec.LeaderWorkerTemplate.LeaderTemplate.Spec = MakeLeaderPodSpec()
	lws.Spec.LeaderWorkerTemplate.WorkerTemplate.Spec = MakeWorkerPodSpec()
	// Manually set this for we didn't enable webhook in controller tests.
	lws.Spec.RolloutStrategy = leaderworkerset.RolloutStrategy{
		Type: leaderworkerset.RollingUpdateStrategyType,
		RollingUpdateConfiguration: &leaderworkerset.RollingUpdateConfiguration{
			MaxUnavailable: intstr.FromInt32(1),
			MaxSurge:       intstr.FromInt(0),
		},
	}
	lws.Spec.StartupPolicy = leaderworkerset.LeaderCreatedStartupPolicy
	subdomainPolicy := leaderworkerset.SubdomainShared
	lws.Spec.NetworkConfig = &leaderworkerset.NetworkConfig{
		SubdomainPolicy: &subdomainPolicy,
	}

	return &lws
}

func MakeLeaderPodSpec() corev1.PodSpec {
	return corev1.PodSpec{
		Containers: []corev1.Container{
			{
				Name:  "worker",
				Image: "nginx:1.14.2",
			},
		},
	}
}

func MakeWorkerPodSpec() corev1.PodSpec {
	return corev1.PodSpec{
		Containers: []corev1.Container{
			{
				Name:  "leader",
				Image: "nginx:1.14.2",
				Ports: []corev1.ContainerPort{
					{
						ContainerPort: 8080,
						Protocol:      "TCP",
					},
				},
			},
		},
	}
}

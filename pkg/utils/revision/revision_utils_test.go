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
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	leaderworkerset "sigs.k8s.io/lws/api/leaderworkerset/v1"
)

func TestApplyRevision(t *testing.T) {
	client := fake.NewClientBuilder().Build()

	lws := BuildLeaderWorkerSet("default").Obj()
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
	lws.Spec.RolloutStrategy = leaderworkerset.RolloutStrategy{
		Type: leaderworkerset.RollingUpdateStrategyType,
		RollingUpdateConfiguration: &leaderworkerset.RollingUpdateConfiguration{
			MaxUnavailable: intstr.FromInt32(2),
			MaxSurge:       intstr.FromInt(1),
		},
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

	if diff := cmp.Diff(lws.Spec.RolloutStrategy, restoredLws.Spec.RolloutStrategy); diff != "" {
		t.Errorf("It should not restore/clear non NetworkConfig Spec fields %s,", diff)
	}
}

func TestEqualRevision(t *testing.T) {
	client := fake.NewClientBuilder().Build()
	tests := []struct {
		name             string
		leftLws          *leaderworkerset.LeaderWorkerSet
		rightLws         *leaderworkerset.LeaderWorkerSet
		leftRevisionKey  string
		rightRevisionKey string
		equal            bool
	}{
		{
			name:             "same LeaderWorkerTemplate, networkConfig, should be equal",
			leftLws:          BuildLeaderWorkerSet("default").Obj(),
			rightLws:         BuildLeaderWorkerSet("default").Obj(),
			leftRevisionKey:  "",
			rightRevisionKey: "",
			equal:            true,
		},
		{
			name:             "same LeaderWorkerTemplate, networkConfig, different revisionKey, should be equal",
			leftLws:          BuildLeaderWorkerSet("default").Obj(),
			rightLws:         BuildLeaderWorkerSet("default").Obj(),
			leftRevisionKey:  "",
			rightRevisionKey: "templateHash",
			equal:            true,
		},
		{
			name:             "left nil, right nil, should be equal",
			leftLws:          nil,
			rightLws:         nil,
			leftRevisionKey:  "",
			rightRevisionKey: "",
			equal:            true,
		},
		{
			name:             "left nil, right non-nil, should not be equal",
			leftLws:          nil,
			rightLws:         BuildLeaderWorkerSet("default").Obj(),
			leftRevisionKey:  "",
			rightRevisionKey: "",
			equal:            false,
		},
		{
			name:             "same LeaderWorkerTemplate, different networkConfig, should not be equal",
			leftLws:          BuildLeaderWorkerSet("default").SubdomainPolicy(leaderworkerset.SubdomainUniquePerReplica).Obj(),
			rightLws:         BuildLeaderWorkerSet("default").Obj(),
			leftRevisionKey:  "",
			rightRevisionKey: "",
			equal:            false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var leftRevision *appsv1.ControllerRevision
			var rightRevision *appsv1.ControllerRevision
			var err error
			if tc.leftLws != nil {
				leftRevision, err = NewRevision(context.TODO(), client, tc.leftLws, tc.leftRevisionKey)
				if err != nil {
					t.Fatal(err)
				}
			}
			if tc.rightLws != nil {
				rightRevision, err = NewRevision(context.TODO(), client, tc.rightLws, tc.rightRevisionKey)
				if err != nil {
					t.Fatal(err)
				}
			}
			equal := EqualRevision(leftRevision, rightRevision)
			if tc.equal != equal {
				t.Errorf("Expected equality between controller revisions to be %t, but was %t", tc.equal, equal)
			}
		})
	}
}

func TestGetHighestRevision(t *testing.T) {
	client := fake.NewClientBuilder().Build()
	lws := BuildLeaderWorkerSet("default").Obj()
	revision1, err := NewRevision(context.TODO(), client, lws, "")
	if err != nil {
		t.Fatal(err)
	}
	revision2 := revision1.DeepCopy()
	revision2.Revision = 2
	revision3 := revision2.DeepCopy()
	revision3.Revision = 3
	tests := []struct {
		name             string
		revisions        []*appsv1.ControllerRevision
		expectedRevision *appsv1.ControllerRevision
	}{
		{
			name:             "empty revision list, returns nil",
			revisions:        []*appsv1.ControllerRevision{},
			expectedRevision: nil,
		},
		{
			name:             "only one revision in list, returns it",
			revisions:        []*appsv1.ControllerRevision{revision1},
			expectedRevision: revision1,
		},
		{
			name:             "returns the revision with highest revision number",
			revisions:        []*appsv1.ControllerRevision{revision2, revision3, revision2},
			expectedRevision: revision3,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			revision := getHighestRevision(tc.revisions)
			if tc.expectedRevision == nil {
				if revision != nil {
					t.Errorf("Expected revision to be nil")
				}
			} else {
				if tc.expectedRevision.Revision != revision.Revision {
					t.Errorf("Expected revision number to be %d, but it was %d", tc.expectedRevision.Revision, revision.Revision)
				}
			}
		})
	}
}

type LeaderWorkerSetWrapper struct {
	leaderworkerset.LeaderWorkerSet
}

func BuildLeaderWorkerSet(nsName string) *LeaderWorkerSetWrapper {
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

	return &LeaderWorkerSetWrapper{
		lws,
	}
}

func (lwsWrapper *LeaderWorkerSetWrapper) Obj() *leaderworkerset.LeaderWorkerSet {
	return &lwsWrapper.LeaderWorkerSet
}

func (lwsWrapper *LeaderWorkerSetWrapper) SubdomainPolicy(subdomainPolicy leaderworkerset.SubdomainPolicy) *LeaderWorkerSetWrapper {
	lwsWrapper.Spec.NetworkConfig = &leaderworkerset.NetworkConfig{
		SubdomainPolicy: &subdomainPolicy,
	}
	return lwsWrapper
}

func (lwsWrapper *LeaderWorkerSetWrapper) MaxUnavailable(value int) *LeaderWorkerSetWrapper {
	lwsWrapper.Spec.RolloutStrategy.RollingUpdateConfiguration.MaxUnavailable = intstr.FromInt(value)
	return lwsWrapper
}

func (lwsWrapper *LeaderWorkerSetWrapper) MaxSurge(value int) *LeaderWorkerSetWrapper {
	lwsWrapper.Spec.RolloutStrategy.RollingUpdateConfiguration.MaxSurge = intstr.FromInt(value)
	return lwsWrapper
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

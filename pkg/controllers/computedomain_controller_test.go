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

package controllers

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	leaderworkerset "sigs.k8s.io/lws/api/leaderworkerset/v1"
	"sigs.k8s.io/lws/pkg/computedomain"
	"sigs.k8s.io/lws/pkg/features"
	"sigs.k8s.io/lws/test/wrappers"
)

func TestComputeDomainTemplatePropagation(t *testing.T) {
	lws := wrappers.BuildLeaderWorkerSet("default").Name("deepseek").Size(2).Obj()
	lws.UID = types.UID("uid-12345")
	lws.Annotations = map[string]string{
		leaderworkerset.AutoComputeDomainAnnotationKey: leaderworkerset.AutoComputeDomainEnabled,
	}
	lws.Spec.LeaderWorkerTemplate.WorkerTemplate.Spec.Containers[0].Resources = corev1.ResourceRequirements{
		Limits: corev1.ResourceList{
			computedomain.NvidiaGPUResourceName: resource.MustParse("4"),
		},
	}

	t.Run("stamps compute-domain-prefix on leader template and compute-domain-name on worker STS when gate enabled", func(t *testing.T) {
		features.SetFeatureGateDuringTest(t, features.AutoComputeDomain, true)

		leaderTpl, err := buildLeaderPodTemplateApplyConfiguration(lws, "rev1")
		if err != nil {
			t.Fatalf("buildLeaderPodTemplateApplyConfiguration: %v", err)
		}
		wantPrefix := computedomain.UIDPrefix(lws.Name, lws.UID)
		if got := leaderTpl.Annotations[leaderworkerset.ComputeDomainPrefixAnnotationKey]; got != wantPrefix {
			t.Fatalf("leader template compute-domain-prefix=%q, want %q", got, wantPrefix)
		}

		leaderPod := corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "deepseek-0",
				Namespace: "default",
				Labels: map[string]string{
					leaderworkerset.SetNameLabelKey:     lws.Name,
					leaderworkerset.GroupIndexLabelKey:  "0",
					leaderworkerset.WorkerIndexLabelKey: "0",
					leaderworkerset.RevisionKey:         "rev1",
				},
				Annotations: map[string]string{
					leaderworkerset.ComputeDomainNameAnnotationKey: "deepseek-uid-rev1-0-inc1",
				},
			},
			Spec: corev1.PodSpec{
				Hostname:  "deepseek-0",
				Subdomain: "deepseek",
			},
		}
		workerSts, err := constructWorkerStatefulSetApplyConfiguration(leaderPod, *lws)
		if err != nil {
			t.Fatalf("constructWorkerStatefulSetApplyConfiguration: %v", err)
		}
		if got := workerSts.Spec.Template.Annotations[leaderworkerset.ComputeDomainNameAnnotationKey]; got != "deepseek-uid-rev1-0-inc1" {
			t.Fatalf("worker STS compute-domain-name=%q, want %q", got, "deepseek-uid-rev1-0-inc1")
		}
	})

	t.Run("omits annotations when AutoComputeDomain feature gate is disabled", func(t *testing.T) {
		features.SetFeatureGateDuringTest(t, features.AutoComputeDomain, false)

		leaderTpl, err := buildLeaderPodTemplateApplyConfiguration(lws, "rev1")
		if err != nil {
			t.Fatalf("buildLeaderPodTemplateApplyConfiguration: %v", err)
		}
		if _, found := leaderTpl.Annotations[leaderworkerset.ComputeDomainPrefixAnnotationKey]; found {
			t.Fatalf("expected no compute-domain-prefix when gate is disabled")
		}
	})
}

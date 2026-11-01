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
	"fmt"
	"testing"

	"github.com/google/go-cmp/cmp"
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

	t.Run("templates do not change when the AutoComputeDomain feature gate is toggled", func(t *testing.T) {
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
		templates := func(enabled bool) (leader, worker map[string]string) {
			t.Run(fmt.Sprintf("gate=%t", enabled), func(t *testing.T) {
				features.SetFeatureGateDuringTest(t, features.AutoComputeDomain, enabled)
				leaderTpl, err := buildLeaderPodTemplateApplyConfiguration(lws, "rev1")
				if err != nil {
					t.Fatalf("buildLeaderPodTemplateApplyConfiguration: %v", err)
				}
				workerSts, err := constructWorkerStatefulSetApplyConfiguration(leaderPod, *lws)
				if err != nil {
					t.Fatalf("constructWorkerStatefulSetApplyConfiguration: %v", err)
				}
				leader, worker = leaderTpl.Annotations, workerSts.Spec.Template.Annotations
			})
			return leader, worker
		}
		leaderOn, workerOn := templates(true)
		leaderOff, workerOff := templates(false)
		if _, found := leaderOn[leaderworkerset.ComputeDomainPrefixAnnotationKey]; !found {
			t.Fatalf("expected compute-domain-prefix on the leader template")
		}
		if _, found := workerOn[leaderworkerset.ComputeDomainNameAnnotationKey]; !found {
			t.Fatalf("expected compute-domain-name on the worker template")
		}
		if diff := cmp.Diff(leaderOn, leaderOff); diff != "" {
			t.Fatalf("leader template annotations changed with the gate (-on +off):\n%s", diff)
		}
		if diff := cmp.Diff(workerOn, workerOff); diff != "" {
			t.Fatalf("worker template annotations changed with the gate (-on +off):\n%s", diff)
		}
	})

	t.Run("omits annotations when the LeaderWorkerSet is not opted in", func(t *testing.T) {
		features.SetFeatureGateDuringTest(t, features.AutoComputeDomain, true)
		optedOut := lws.DeepCopy()
		optedOut.Annotations = nil

		leaderTpl, err := buildLeaderPodTemplateApplyConfiguration(optedOut, "rev1")
		if err != nil {
			t.Fatalf("buildLeaderPodTemplateApplyConfiguration: %v", err)
		}
		if _, found := leaderTpl.Annotations[leaderworkerset.ComputeDomainPrefixAnnotationKey]; found {
			t.Fatalf("expected no compute-domain-prefix when the LeaderWorkerSet is not opted in")
		}
	})
}

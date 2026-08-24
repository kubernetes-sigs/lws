/*
Copyright 2023 The Kubernetes Authors.
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
	"fmt"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	leaderworkerset "sigs.k8s.io/lws/api/leaderworkerset/v1"
)

func hashLeaderPod(lwsName string, extraAnnotations map[string]string) *corev1.Pod {
	annotations := map[string]string{
		leaderworkerset.SizeAnnotationKey:          "2",
		leaderworkerset.GroupIdentityAnnotationKey: string(leaderworkerset.GroupIdentityHash),
	}
	for k, v := range extraAnnotations {
		annotations[k] = v
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: lwsName + "-",
			Namespace:    "default",
			Labels: map[string]string{
				leaderworkerset.SetNameLabelKey:     lwsName,
				leaderworkerset.WorkerIndexLabelKey: "0",
			},
			Annotations: annotations,
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "leader"}}},
	}
}

func TestHashLeaderDNSDefaults(t *testing.T) {
	webhook := &PodWebhook{}
	pod := hashLeaderPod("hash-dns", nil)
	if err := webhook.Default(context.TODO(), pod); err != nil {
		t.Fatalf("defaulting hash leader: %v", err)
	}
	key := pod.Labels[leaderworkerset.GroupUniqueHashLabelKey]
	if key == "" {
		t.Fatal("expected a group key to be assigned")
	}
	if pod.Spec.Hostname != hashDNSPrefix(key) {
		t.Errorf("expected hostname %q, got %q", hashDNSPrefix(key), pod.Spec.Hostname)
	}
	if pod.Spec.Subdomain != "hash-dns" {
		t.Errorf("expected subdomain %q, got %q", "hash-dns", pod.Spec.Subdomain)
	}
	wantAddress := fmt.Sprintf("%s.%s.%s", pod.Spec.Hostname, pod.Spec.Subdomain, pod.Namespace)
	found := false
	for _, env := range pod.Spec.Containers[0].Env {
		if env.Name == leaderworkerset.LwsLeaderAddress {
			found = true
			if env.Value != wantAddress {
				t.Errorf("expected %s %q, got %q", leaderworkerset.LwsLeaderAddress, wantAddress, env.Value)
			}
		}
	}
	if !found {
		t.Errorf("expected %s env var on the leader", leaderworkerset.LwsLeaderAddress)
	}
}

func TestHashLeaderUniquePerReplicaSubdomain(t *testing.T) {
	webhook := &PodWebhook{}
	pod := hashLeaderPod("hash-upr", map[string]string{
		leaderworkerset.SubdomainPolicyAnnotationKey: string(leaderworkerset.SubdomainUniquePerReplica),
	})
	if err := webhook.Default(context.TODO(), pod); err != nil {
		t.Fatalf("defaulting hash leader: %v", err)
	}
	key := pod.Labels[leaderworkerset.GroupUniqueHashLabelKey]
	want := fmt.Sprintf("hash-upr-%s", hashDNSPrefix(key))
	if pod.Spec.Subdomain != want {
		t.Errorf("expected per-replica subdomain %q, got %q", want, pod.Spec.Subdomain)
	}
}

func TestHashLeaderSubGroupLabels(t *testing.T) {
	webhook := &PodWebhook{}
	pod := hashLeaderPod("hash-sub", map[string]string{
		leaderworkerset.SubGroupSizeAnnotationKey:         "2",
		leaderworkerset.SubGroupExclusiveKeyAnnotationKey: "topo",
	})
	if err := webhook.Default(context.TODO(), pod); err != nil {
		t.Fatalf("defaulting hash leader: %v", err)
	}
	key := pod.Labels[leaderworkerset.GroupUniqueHashLabelKey]
	if got := pod.Labels[leaderworkerset.SubGroupIndexLabelKey]; got != "0" {
		t.Errorf("expected leader subgroup index 0, got %q", got)
	}
	if got, want := pod.Labels[leaderworkerset.SubGroupUniqueHashLabelKey], genGroupUniqueKey(key, "0"); got != want {
		t.Errorf("expected subgroup hash derived from the group key %q, got %q", want, got)
	}
	if !exclusiveAffinityApplied(*pod, "topo") {
		t.Error("expected subgroup exclusive placement affinity to be applied")
	}
}

func TestHashWorkerSubGroupKeyFromGroupKey(t *testing.T) {
	webhook := &PodWebhook{}
	groupKey := genGroupUniqueKey("default", "some-random-input")
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "hash-sub-x2kkp-1",
			Namespace: "default",
			Labels: map[string]string{
				leaderworkerset.SetNameLabelKey:         "hash-sub",
				leaderworkerset.GroupIndexLabelKey:      groupKey,
				leaderworkerset.GroupUniqueHashLabelKey: groupKey,
			},
			Annotations: map[string]string{
				leaderworkerset.SizeAnnotationKey:          "4",
				leaderworkerset.SubGroupSizeAnnotationKey:  "2",
				leaderworkerset.GroupIdentityAnnotationKey: string(leaderworkerset.GroupIdentityHash),
				leaderworkerset.LeaderPodNameAnnotationKey: "hash-sub-x2kkp",
				leaderworkerset.LeaderAddressAnnotationKey: "host.hash-sub.default",
			},
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "worker"}}},
	}
	if err := webhook.Default(context.TODO(), pod); err != nil {
		t.Fatalf("defaulting hash worker: %v", err)
	}
	subGroupIndex := pod.Labels[leaderworkerset.SubGroupIndexLabelKey]
	if got, want := pod.Labels[leaderworkerset.SubGroupUniqueHashLabelKey], genGroupUniqueKey(groupKey, subGroupIndex); got != want {
		t.Errorf("expected worker subgroup hash derived from the group key %q, got %q", want, got)
	}
	if unwanted := genGroupUniqueKey("hash-sub-x2kkp", subGroupIndex); pod.Labels[leaderworkerset.SubGroupUniqueHashLabelKey] == unwanted {
		t.Error("worker subgroup hash still derived from the leader pod name")
	}
}

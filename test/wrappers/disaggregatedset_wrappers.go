/*
Copyright 2025 The Kubernetes Authors.
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
package wrappers

import (
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"

	disaggregatedsetv1 "sigs.k8s.io/lws/api/disaggregatedset/v1"
	leaderworkerset "sigs.k8s.io/lws/api/leaderworkerset/v1"
)

// --- DisaggregatedSet wrapper ---

type DisaggregatedSetWrapper struct {
	disaggregatedsetv1.DisaggregatedSet
}

func BuildDisaggregatedSet(name, namespace string) *DisaggregatedSetWrapper {
	return &DisaggregatedSetWrapper{
		DisaggregatedSet: disaggregatedsetv1.DisaggregatedSet{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: namespace,
				UID:       types.UID(name + "-uid"),
			},
		},
	}
}

func (w *DisaggregatedSetWrapper) Obj() *disaggregatedsetv1.DisaggregatedSet {
	return &w.DisaggregatedSet
}

func (w *DisaggregatedSetWrapper) UID(uid string) *DisaggregatedSetWrapper {
	w.ObjectMeta.UID = types.UID(uid)
	return w
}

func (w *DisaggregatedSetWrapper) WithRole(name string, replicas int32, image string) *DisaggregatedSetWrapper {
	w.Spec.Roles = append(w.Spec.Roles, disaggregatedsetv1.DisaggregatedRoleSpec{
		Name: name,
		LeaderWorkerSetTemplateSpec: leaderworkerset.LeaderWorkerSetTemplateSpec{Spec: leaderworkerset.LeaderWorkerSetSpec{
			Replicas: ptr.To(replicas),
			LeaderWorkerTemplate: leaderworkerset.LeaderWorkerTemplate{
				Size:           ptr.To(int32(1)),
				WorkerTemplate: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: image}}}},
			},
		}},
	})
	return w
}

func (w *DisaggregatedSetWrapper) WithRoleNoReplicas(name string, image string) *DisaggregatedSetWrapper {
	w.Spec.Roles = append(w.Spec.Roles, disaggregatedsetv1.DisaggregatedRoleSpec{
		Name: name,
		LeaderWorkerSetTemplateSpec: leaderworkerset.LeaderWorkerSetTemplateSpec{Spec: leaderworkerset.LeaderWorkerSetSpec{
			LeaderWorkerTemplate: leaderworkerset.LeaderWorkerTemplate{
				WorkerTemplate: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: image}}}},
			},
		}},
	})
	return w
}

func (w *DisaggregatedSetWrapper) WithRollout(role string, surge, unavail intstr.IntOrString) *DisaggregatedSetWrapper {
	for i := range w.Spec.Roles {
		if w.Spec.Roles[i].Name == role {
			w.Spec.Roles[i].Spec.RolloutStrategy = leaderworkerset.RolloutStrategy{
				RollingUpdateConfiguration: &leaderworkerset.RollingUpdateConfiguration{
					MaxSurge:       surge,
					MaxUnavailable: unavail,
				},
			}
			break
		}
	}
	return w
}

// --- LWS builder for disaggregatedset tests ---

func BuildDisaggregatedSetLWS(name, namespace, role, revision string) *LeaderWorkerSetWrapper {
	return &LeaderWorkerSetWrapper{
		LeaderWorkerSet: leaderworkerset.LeaderWorkerSet{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: namespace,
				Labels: map[string]string{
					disaggregatedsetv1.RoleLabelKey:     role,
					disaggregatedsetv1.SetNameLabelKey:  name,
					disaggregatedsetv1.RevisionLabelKey: revision,
				},
			},
		},
	}
}

func (lwsWrapper *LeaderWorkerSetWrapper) Namespace(ns string) *LeaderWorkerSetWrapper {
	lwsWrapper.ObjectMeta.Namespace = ns
	return lwsWrapper
}

func (lwsWrapper *LeaderWorkerSetWrapper) Labels(labels map[string]string) *LeaderWorkerSetWrapper {
	lwsWrapper.ObjectMeta.Labels = labels
	return lwsWrapper
}

func (lwsWrapper *LeaderWorkerSetWrapper) StatusReplicas(n int32) *LeaderWorkerSetWrapper {
	lwsWrapper.Status.Replicas = n
	return lwsWrapper
}

func (lwsWrapper *LeaderWorkerSetWrapper) ReadyReplicas(n int32) *LeaderWorkerSetWrapper {
	lwsWrapper.Status.ReadyReplicas = n
	return lwsWrapper
}

func (lwsWrapper *LeaderWorkerSetWrapper) CreationTimestamp(t time.Time) *LeaderWorkerSetWrapper {
	lwsWrapper.ObjectMeta.CreationTimestamp = metav1.Time{Time: t}
	return lwsWrapper
}

func (lwsWrapper *LeaderWorkerSetWrapper) OwnerReference(ref metav1.OwnerReference) *LeaderWorkerSetWrapper {
	lwsWrapper.ObjectMeta.OwnerReferences = []metav1.OwnerReference{ref}
	return lwsWrapper
}

// --- Role spec builder ---

func MakeRoleSpec(
	name string,
	replicas int32,
	podSpec corev1.PodSpec,
	surge, unavail intstr.IntOrString,
) disaggregatedsetv1.DisaggregatedRoleSpec {
	return disaggregatedsetv1.DisaggregatedRoleSpec{
		Name: name,
		LeaderWorkerSetTemplateSpec: leaderworkerset.LeaderWorkerSetTemplateSpec{Spec: leaderworkerset.LeaderWorkerSetSpec{
			Replicas: ptr.To(replicas),
			LeaderWorkerTemplate: leaderworkerset.LeaderWorkerTemplate{
				Size:           ptr.To(int32(1)),
				WorkerTemplate: corev1.PodTemplateSpec{Spec: podSpec},
			},
			RolloutStrategy: leaderworkerset.RolloutStrategy{
				RollingUpdateConfiguration: &leaderworkerset.RollingUpdateConfiguration{
					MaxSurge: surge, MaxUnavailable: unavail,
				},
			},
		}},
	}
}

// --- Shared test scheme ---

func DisaggregatedSetTestScheme() *runtime.Scheme {
	scheme := runtime.NewScheme()
	_ = disaggregatedsetv1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)
	_ = leaderworkerset.AddToScheme(scheme)
	return scheme
}

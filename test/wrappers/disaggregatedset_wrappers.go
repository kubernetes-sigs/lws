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
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"

	disaggregatedsetv1 "sigs.k8s.io/lws/api/disaggregatedset/v1"
	leaderworkersetv1 "sigs.k8s.io/lws/api/leaderworkerset/v1"
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

func (w *DisaggregatedSetWrapper) Slices(n int32) *DisaggregatedSetWrapper {
	w.Spec.Slices = ptr.To(n)
	return w
}

func (w *DisaggregatedSetWrapper) WithRole(name string, replicas int32, image string) *DisaggregatedSetWrapper {
	w.Spec.Roles = append(w.Spec.Roles, disaggregatedsetv1.DisaggregatedRoleSpec{
		Name: name,
		LeaderWorkerSetTemplateSpec: leaderworkersetv1.LeaderWorkerSetTemplateSpec{Spec: leaderworkersetv1.LeaderWorkerSetSpec{
			Replicas: ptr.To(replicas),
			LeaderWorkerTemplate: leaderworkersetv1.LeaderWorkerTemplate{
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
		LeaderWorkerSetTemplateSpec: leaderworkersetv1.LeaderWorkerSetTemplateSpec{Spec: leaderworkersetv1.LeaderWorkerSetSpec{
			LeaderWorkerTemplate: leaderworkersetv1.LeaderWorkerTemplate{
				WorkerTemplate: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: image}}}},
			},
		}},
	})
	return w
}

func (w *DisaggregatedSetWrapper) WithRollout(role string, surge, unavail intstr.IntOrString) *DisaggregatedSetWrapper {
	for i := range w.Spec.Roles {
		if w.Spec.Roles[i].Name == role {
			w.Spec.Roles[i].Spec.RolloutStrategy = leaderworkersetv1.RolloutStrategy{
				RollingUpdateConfiguration: &leaderworkersetv1.RollingUpdateConfiguration{
					MaxSurge:       surge,
					MaxUnavailable: unavail,
				},
			}
			break
		}
	}
	return w
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
		LeaderWorkerSetTemplateSpec: leaderworkersetv1.LeaderWorkerSetTemplateSpec{Spec: leaderworkersetv1.LeaderWorkerSetSpec{
			Replicas: ptr.To(replicas),
			LeaderWorkerTemplate: leaderworkersetv1.LeaderWorkerTemplate{
				Size:           ptr.To(int32(1)),
				WorkerTemplate: corev1.PodTemplateSpec{Spec: podSpec},
			},
			RolloutStrategy: leaderworkersetv1.RolloutStrategy{
				RollingUpdateConfiguration: &leaderworkersetv1.RollingUpdateConfiguration{
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
	_ = leaderworkersetv1.AddToScheme(scheme)
	return scheme
}

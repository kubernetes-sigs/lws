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

package webhooks

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	schedulingv1alpha3 "k8s.io/api/scheduling/v1alpha3"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	leaderworkerset "sigs.k8s.io/lws/api/leaderworkerset/v1"
	"sigs.k8s.io/lws/pkg/features"
	"sigs.k8s.io/lws/pkg/schedulerprovider"
)

func TestValidateScheduling(t *testing.T) {
	tests := map[string]struct {
		mutate     func(*leaderworkerset.LeaderWorkerSet)
		enableGate bool
		wantErrs   int
	}{
		"empty scheduling defaults to replica-sized gang": {
			enableGate: true,
		},
		"feature gate disabled": {
			wantErrs: 1,
		},
		"rejects composite minGroupCount": {
			enableGate: true,
			mutate: func(lws *leaderworkerset.LeaderWorkerSet) {
				lws.Spec.Scheduling.SchedulingPolicy = &schedulingv1alpha3.WorkloadCompositePodGroupSchedulingPolicy{
					Gang: &schedulingv1alpha3.WorkloadCompositePodGroupGangSchedulingPolicy{MinGroupCount: ptr.To[int32](2)},
				}
			},
			wantErrs: 1,
		},
		"gang is incompatible with LeaderReady": {
			enableGate: true,
			mutate: func(lws *leaderworkerset.LeaderWorkerSet) {
				lws.Spec.StartupPolicy = leaderworkerset.LeaderReadyStartupPolicy
			},
			wantErrs: 1,
		},
		"multiple active levels are rejected": {
			enableGate: true,
			mutate: func(lws *leaderworkerset.LeaderWorkerSet) {
				lws.Spec.Scheduling.SchedulingConstraints = &schedulingv1alpha3.WorkloadCompositePodGroupSchedulingConstraints{}
				lws.Spec.Scheduling.Replica = &leaderworkerset.LeaderWorkerSetReplicaScheduling{}
			},
			wantErrs: 1,
		},
		"role leaves must use the workload priority": {
			enableGate: true,
			mutate: func(lws *leaderworkerset.LeaderWorkerSet) {
				lws.Spec.LeaderWorkerTemplate.LeaderTemplate = &corev1.PodTemplateSpec{Spec: corev1.PodSpec{PriorityClassName: "other-priority"}}
				lws.Spec.Scheduling.Replica = &leaderworkerset.LeaderWorkerSetReplicaScheduling{
					Leader: &leaderworkerset.LeaderWorkerSetLeaderScheduling{},
				}
			},
			wantErrs: 1,
		},
		"whole LWS gang supports zero replicas": {
			enableGate: true,
			mutate: func(lws *leaderworkerset.LeaderWorkerSet) {
				lws.Spec.Replicas = ptr.To[int32](0)
				lws.Spec.Scheduling.SchedulingPolicy = &schedulingv1alpha3.WorkloadCompositePodGroupSchedulingPolicy{
					Gang: &schedulingv1alpha3.WorkloadCompositePodGroupGangSchedulingPolicy{},
				}
			},
		},
		"worker-only gang is compatible with LeaderReady": {
			enableGate: true,
			mutate: func(lws *leaderworkerset.LeaderWorkerSet) {
				lws.Spec.StartupPolicy = leaderworkerset.LeaderReadyStartupPolicy
				lws.Spec.Scheduling.Replica = &leaderworkerset.LeaderWorkerSetReplicaScheduling{
					Worker: &leaderworkerset.LeaderWorkerSetWorkerScheduling{
						SchedulingPolicy: &schedulingv1alpha3.WorkloadPodGroupSchedulingPolicy{
							Gang: &schedulingv1alpha3.WorkloadPodGroupGangSchedulingPolicy{},
						},
					},
				}
			},
		},
		"leaf gang minimum must equal membership": {
			enableGate: true,
			mutate: func(lws *leaderworkerset.LeaderWorkerSet) {
				lws.Spec.Scheduling.Replica = &leaderworkerset.LeaderWorkerSetReplicaScheduling{
					Worker: &leaderworkerset.LeaderWorkerSetWorkerScheduling{
						SchedulingPolicy: &schedulingv1alpha3.WorkloadPodGroupSchedulingPolicy{
							Gang: &schedulingv1alpha3.WorkloadPodGroupGangSchedulingPolicy{MinCount: ptr.To[int32](1)},
						},
					},
				}
			},
			wantErrs: 1,
		},
		"leader and worker priority must match": {
			enableGate: true,
			mutate: func(lws *leaderworkerset.LeaderWorkerSet) {
				lws.Spec.LeaderWorkerTemplate.LeaderTemplate = &corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{PriorityClassName: "other-priority"},
				}
			},
			wantErrs: 1,
		},
		"worker resource claims matching the worker template are admitted": {
			enableGate: true,
			mutate: func(lws *leaderworkerset.LeaderWorkerSet) {
				lws.Spec.LeaderWorkerTemplate.WorkerTemplate.Spec.ResourceClaims = []corev1.PodResourceClaim{{
					Name: "gpu", ResourceClaimName: ptr.To("shared-gpu"),
				}}
				lws.Spec.Scheduling.Replica = &leaderworkerset.LeaderWorkerSetReplicaScheduling{
					Worker: &leaderworkerset.LeaderWorkerSetWorkerScheduling{
						ResourceClaims: []schedulingv1alpha3.WorkloadPodGroupResourceClaim{{
							Name: "gpu", ResourceClaimName: ptr.To("shared-gpu"),
						}},
					},
				}
			},
		},
		"worker resource claims must match the worker template": {
			enableGate: true,
			mutate: func(lws *leaderworkerset.LeaderWorkerSet) {
				lws.Spec.Scheduling.Replica = &leaderworkerset.LeaderWorkerSetReplicaScheduling{
					Worker: &leaderworkerset.LeaderWorkerSetWorkerScheduling{
						ResourceClaims: []schedulingv1alpha3.WorkloadPodGroupResourceClaim{{
							Name: "gpu", ResourceClaimName: ptr.To("shared-gpu"),
						}},
					},
				}
			},
			wantErrs: 1,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			lws := validScheduledLWS()
			if tc.mutate != nil {
				tc.mutate(lws)
			}
			if tc.enableGate {
				features.SetFeatureGateDuringTest(t, features.WorkloadAwareScheduling, true)
			}
			hook := &LeaderWorkerSetWebhook{
				SchedulerProvider: schedulerprovider.Kubernetes,
			}
			errs := hook.validateScheduling(context.Background(), nil, lws)
			assert.Len(t, errs, tc.wantErrs, "errors: %v", errs)
		})
	}
}

func TestValidateSchedulingUpdate(t *testing.T) {
	features.SetFeatureGateDuringTest(t, features.WorkloadAwareScheduling, true)
	hook := &LeaderWorkerSetWebhook{
		SchedulerProvider: schedulerprovider.Kubernetes,
	}

	t.Run("active level is immutable", func(t *testing.T) {
		oldLWS := validScheduledLWS()
		newLWS := oldLWS.DeepCopy()
		newLWS.Spec.Scheduling = &leaderworkerset.LeaderWorkerSetScheduling{
			SchedulingPolicy: &schedulingv1alpha3.WorkloadCompositePodGroupSchedulingPolicy{
				Basic: &schedulingv1alpha3.WorkloadCompositePodGroupBasicSchedulingPolicy{},
			},
		}
		errs := hook.validateScheduling(context.Background(), oldLWS, newLWS)
		assert.NotEmpty(t, errs)
		assert.Contains(t, errs.ToAggregate().Error(), "cannot switch the active scheduling level")
	})

	t.Run("generated replica minCount follows size", func(t *testing.T) {
		oldLWS := validScheduledLWS()
		newLWS := oldLWS.DeepCopy()
		newLWS.Spec.LeaderWorkerTemplate.Size = ptr.To[int32](4)
		assert.Empty(t, hook.validateScheduling(context.Background(), oldLWS, newLWS))
	})

	t.Run("existing scheduling remains updateable after gate is disabled", func(t *testing.T) {
		oldLWS := validScheduledLWS()
		newLWS := oldLWS.DeepCopy()
		newLWS.Spec.Replicas = ptr.To[int32](0)
		features.SetFeatureGateDuringTest(t, features.WorkloadAwareScheduling, false)
		disabledHook := &LeaderWorkerSetWebhook{SchedulerProvider: schedulerprovider.Kubernetes}
		assert.Empty(t, disabledHook.validateScheduling(context.Background(), oldLWS, newLWS))
	})

	t.Run("existing scheduling remains updateable after scheduler provider is removed", func(t *testing.T) {
		oldLWS := validScheduledLWS()
		newLWS := oldLWS.DeepCopy()
		newLWS.Spec.Replicas = ptr.To[int32](0)
		unconfiguredHook := &LeaderWorkerSetWebhook{}
		assert.Empty(t, unconfiguredHook.validateScheduling(context.Background(), oldLWS, newLWS))
		// New opt-ins are still rejected without a configured provider.
		errs := unconfiguredHook.validateScheduling(context.Background(), nil, newLWS)
		assert.NotEmpty(t, errs)
		assert.Contains(t, errs.ToAggregate().Error(), "requires a configured scheduler provider")
	})

	t.Run("existing scheduling remains updateable when v1beta1 API is missing", func(t *testing.T) {
		oldLWS := validScheduledLWS()
		newLWS := oldLWS.DeepCopy()
		newLWS.Spec.Replicas = ptr.To[int32](0)
		missingAPIHook := &LeaderWorkerSetWebhook{
			SchedulerProvider: schedulerprovider.Kubernetes,
			RESTMapper:        apimeta.NewDefaultRESTMapper(nil),
		}
		assert.Empty(t, missingAPIHook.validateScheduling(context.Background(), oldLWS, newLWS))
		// New opt-ins are rejected when the upstream v1beta1 API is unavailable.
		errs := missingAPIHook.validateScheduling(context.Background(), nil, newLWS)
		assert.NotEmpty(t, errs)
		assert.Contains(t, errs.ToAggregate().Error(), "scheduling.k8s.io/v1beta1 Workload API is not available")
	})

	t.Run("pod webhook rejects workload-aware pod when scheduler provider is nil", func(t *testing.T) {
		pw := NewPodWebhook(nil)
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-lws-0",
				Namespace: "default",
				Labels: map[string]string{
					leaderworkerset.SetNameLabelKey:     "test-lws",
					leaderworkerset.WorkerIndexLabelKey: "0",
				},
				Annotations: map[string]string{
					leaderworkerset.SizeAnnotationKey:                 "2",
					schedulerprovider.WorkloadSchedulingAnnotationKey: string(schedulerprovider.SchedulingModeReplica),
				},
			},
			Spec: corev1.PodSpec{
				Subdomain:  "test-lws",
				Containers: []corev1.Container{{Name: "c", Image: "img"}},
			},
		}
		err := pw.Default(context.Background(), pod)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "no scheduler provider is configured")
	})

	t.Run("priority class is immutable", func(t *testing.T) {
		oldLWS := validScheduledLWS()
		newLWS := oldLWS.DeepCopy()
		newLWS.Spec.LeaderWorkerTemplate.WorkerTemplate.Spec.PriorityClassName = "new-priority"
		errs := hook.validateScheduling(context.Background(), oldLWS, newLWS)
		assert.NotEmpty(t, errs)
		assert.Contains(t, errs.ToAggregate().Error(), "cannot change priorityClassName")
	})

	t.Run("delegation rejects role mode", func(t *testing.T) {
		controller := true
		lws := validScheduledLWS()
		lws.Annotations = map[string]string{schedulerprovider.GroupTemplateNameAnnotation: "template"}
		lws.OwnerReferences = []metav1.OwnerReference{{
			APIVersion: "example.test/v1", Kind: "Parent", Name: "parent", Controller: &controller,
		}}
		lws.Spec.Scheduling.Replica = &leaderworkerset.LeaderWorkerSetReplicaScheduling{
			Leader: &leaderworkerset.LeaderWorkerSetLeaderScheduling{},
		}
		errs := hook.validateScheduling(context.Background(), nil, lws)
		assert.NotEmpty(t, errs)
		assert.Contains(t, errs.ToAggregate().Error(), "supports only whole-LWS or replica mode")
	})

	t.Run("delegation annotations are immutable once spec.scheduling is set", func(t *testing.T) {
		controller := true
		ownerRef := []metav1.OwnerReference{{
			APIVersion: "example.test/v1", Kind: "Parent", Name: "parent", Controller: &controller,
		}}

		delegatedLWS := validScheduledLWS()
		delegatedLWS.OwnerReferences = ownerRef
		delegatedLWS.Annotations = map[string]string{
			schedulerprovider.GroupTemplateNameAnnotation:       "template-a",
			schedulerprovider.ParentCompositePodGroupAnnotation: "parent-a",
		}

		standaloneLWS := validScheduledLWS()
		standaloneLWS.OwnerReferences = ownerRef

		cases := []struct {
			name      string
			oldLWS    *leaderworkerset.LeaderWorkerSet
			mutate    func(*leaderworkerset.LeaderWorkerSet)
			wantError string
		}{
			{
				name:   "adding group-template-name on update fails",
				oldLWS: standaloneLWS,
				mutate: func(l *leaderworkerset.LeaderWorkerSet) {
					l.Annotations = map[string]string{schedulerprovider.GroupTemplateNameAnnotation: "template-a"}
				},
				wantError: schedulerprovider.GroupTemplateNameAnnotation,
			},
			{
				name:   "removing group-template-name on update fails",
				oldLWS: delegatedLWS,
				mutate: func(l *leaderworkerset.LeaderWorkerSet) {
					delete(l.Annotations, schedulerprovider.GroupTemplateNameAnnotation)
					delete(l.Annotations, schedulerprovider.ParentCompositePodGroupAnnotation)
				},
				wantError: schedulerprovider.GroupTemplateNameAnnotation,
			},
			{
				name:   "changing group-template-name on update fails",
				oldLWS: delegatedLWS,
				mutate: func(l *leaderworkerset.LeaderWorkerSet) {
					l.Annotations[schedulerprovider.GroupTemplateNameAnnotation] = "template-b"
				},
				wantError: schedulerprovider.GroupTemplateNameAnnotation,
			},
			{
				name: "adding parent-compositepodgroup on update fails",
				oldLWS: func() *leaderworkerset.LeaderWorkerSet {
					l := delegatedLWS.DeepCopy()
					delete(l.Annotations, schedulerprovider.ParentCompositePodGroupAnnotation)
					return l
				}(),
				mutate: func(l *leaderworkerset.LeaderWorkerSet) {
					l.Annotations[schedulerprovider.ParentCompositePodGroupAnnotation] = "parent-a"
				},
				wantError: schedulerprovider.ParentCompositePodGroupAnnotation,
			},
			{
				name:   "removing parent-compositepodgroup on update fails",
				oldLWS: delegatedLWS,
				mutate: func(l *leaderworkerset.LeaderWorkerSet) {
					delete(l.Annotations, schedulerprovider.ParentCompositePodGroupAnnotation)
				},
				wantError: schedulerprovider.ParentCompositePodGroupAnnotation,
			},
			{
				name:   "changing parent-compositepodgroup on update fails",
				oldLWS: delegatedLWS,
				mutate: func(l *leaderworkerset.LeaderWorkerSet) {
					l.Annotations[schedulerprovider.ParentCompositePodGroupAnnotation] = "parent-b"
				},
				wantError: schedulerprovider.ParentCompositePodGroupAnnotation,
			},
		}

		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				newLWS := tc.oldLWS.DeepCopy()
				tc.mutate(newLWS)
				errs := hook.validateScheduling(context.Background(), tc.oldLWS, newLWS)
				assert.NotEmpty(t, errs)
				assert.Contains(t, errs.ToAggregate().Error(), tc.wantError)
				assert.Contains(t, errs.ToAggregate().Error(), "immutable once spec.scheduling is set")
			})
		}

		// Updating replicas and unrelated annotations while keeping delegation annotations succeeds.
		unchanged := delegatedLWS.DeepCopy()
		unchanged.Spec.Replicas = ptr.To[int32](3)
		unchanged.Annotations["example.com/other"] = "value"
		assert.Empty(t, hook.validateScheduling(context.Background(), delegatedLWS, unchanged))
	})
}

func TestValidateSchedulingKEPConstraints(t *testing.T) {
	features.SetFeatureGateDuringTest(t, features.WorkloadAwareScheduling, true)
	hook := &LeaderWorkerSetWebhook{
		SchedulerProvider: schedulerprovider.Kubernetes,
	}

	t.Run("leader resource claim must match the leader template", func(t *testing.T) {
		lws := validScheduledLWS()
		lws.Spec.LeaderWorkerTemplate.LeaderTemplate = &corev1.PodTemplateSpec{Spec: corev1.PodSpec{
			PriorityClassName: "high-priority",
			Containers:        []corev1.Container{{Name: "leader", Image: "leader:latest"}},
		}}
		lws.Spec.LeaderWorkerTemplate.WorkerTemplate.Spec.ResourceClaims = []corev1.PodResourceClaim{{
			Name: "gpu", ResourceClaimName: ptr.To("shared-gpu"),
		}}
		lws.Spec.Scheduling.Replica = &leaderworkerset.LeaderWorkerSetReplicaScheduling{
			Leader: &leaderworkerset.LeaderWorkerSetLeaderScheduling{
				ResourceClaims: []schedulingv1alpha3.WorkloadPodGroupResourceClaim{{
					Name: "gpu", ResourceClaimName: ptr.To("shared-gpu"),
				}},
			},
		}
		errs := hook.validateScheduling(context.Background(), nil, lws)
		assert.NotEmpty(t, errs)
		assert.Contains(t, errs.ToAggregate().Error(), "matching reference in every member pod template")
	})

	t.Run("managed templates cannot preset schedulingGroup", func(t *testing.T) {
		lws := validScheduledLWS()
		lws.Spec.LeaderWorkerTemplate.WorkerTemplate.Spec.SchedulingGroup = &corev1.PodSchedulingGroup{
			PodGroupName: ptr.To("user-managed"),
		}
		errs := hook.validateScheduling(context.Background(), nil, lws)
		assert.NotEmpty(t, errs)
		assert.Contains(t, errs.ToAggregate().Error(), "must not set spec.schedulingGroup")
	})
}

func validScheduledLWS() *leaderworkerset.LeaderWorkerSet {
	return &leaderworkerset.LeaderWorkerSet{
		ObjectMeta: metav1.ObjectMeta{Name: "test-lws", Namespace: "default"},
		Spec: leaderworkerset.LeaderWorkerSetSpec{
			Scheduling: &leaderworkerset.LeaderWorkerSetScheduling{},
			LeaderWorkerTemplate: leaderworkerset.LeaderWorkerTemplate{
				Size: ptr.To[int32](3),
				WorkerTemplate: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
					PriorityClassName: "high-priority",
					Containers:        []corev1.Container{{Name: "worker", Image: "worker:latest"}},
				}},
			},
		},
	}
}

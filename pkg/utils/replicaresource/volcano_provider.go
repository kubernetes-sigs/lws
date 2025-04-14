/*
Copyright 2025.

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

package replicaresource

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	quotav1 "k8s.io/apiserver/pkg/quota/v1"
	quotacore "k8s.io/kubernetes/pkg/quota/v1/evaluator/core"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	volcanov1beta1 "volcano.sh/apis/pkg/apis/scheduling/v1beta1"

	leaderworkerset "sigs.k8s.io/lws/api/leaderworkerset/v1"
)

type VolcanoProvider struct {
	*defaultBaseResourceProvider
	client client.Client
}

func NewVolcanoProvider(client client.Client) *VolcanoProvider {
	return &VolcanoProvider{
		client: client,
	}
}

func (v *VolcanoProvider) CreatePodGroupIfNotExists(ctx context.Context, lws *leaderworkerset.LeaderWorkerSet, leaderPod *corev1.Pod) error {
	var pg volcanov1beta1.PodGroup
	pgName := leaderPod.Annotations[volcanov1beta1.KubeGroupNameAnnotationKey]

	if err := v.client.Get(ctx, types.NamespacedName{Name: pgName, Namespace: lws.Namespace}, &pg); err != nil {
		if client.IgnoreNotFound(err) != nil {
			return err
		}
		pg = volcanov1beta1.PodGroup{
			ObjectMeta: metav1.ObjectMeta{
				Name:      pgName,
				Namespace: lws.Namespace,
			},
			Spec: volcanov1beta1.PodGroupSpec{
				// Default startupPolicy is LeaderCreated, Leader and Workers Pods are scheduled together, so MinAvailable is set to size
				MinMember:    *lws.Spec.LeaderWorkerTemplate.Size,
				MinResources: v.getMinResources(lws),
			},
		}

		// If the StartUpPolicy of lws is LeaderReady, set MinMember to 1 to allow the Leader Pod to be scheduled.
		// However, minResources should still be set to the minResources of the PodGroup (1 Leader + (size-1) Workers).
		// If the cluster resources are insufficient, scheduling the Leader Pod alone would be meaningless,
		// as at least one Worker will definitely not be able to be scheduled.
		if lws.Spec.StartupPolicy == leaderworkerset.LeaderReadyStartupPolicy {
			pg.Spec.MinMember = 1
		}

		if queueName, ok := lws.Annotations[volcanov1beta1.QueueNameAnnotationKey]; ok {
			pg.Spec.Queue = queueName
		}

		err = ctrl.SetControllerReference(leaderPod, &pg, v.client.Scheme())
		if err != nil {
			return err
		}

		if err = v.client.Create(ctx, &pg); err != nil {
			return err
		}
	}

	return nil
}

func (v *VolcanoProvider) SetPodMeta(pod *corev1.Pod) error {
	lwsName := pod.Labels[leaderworkerset.SetNameLabelKey]
	groupIndex := pod.Labels[leaderworkerset.GroupIndexLabelKey]
	pod.Annotations[volcanov1beta1.KubeGroupNameAnnotationKey] = GetPodGroupName(lwsName, groupIndex)

	return nil
}

// TODO: Need to rewrite the calculation method for pod resource usage
func (v *VolcanoProvider) getMinResources(lws *leaderworkerset.LeaderWorkerSet) corev1.ResourceList {
	res := corev1.ResourceList{}

	// calculate leader min resources
	leaderTemplate := lws.Spec.LeaderWorkerTemplate.LeaderTemplate
	// if leaderTemplate is nil, use workerTemplate instead
	if leaderTemplate == nil {
		leaderTemplate = &lws.Spec.LeaderWorkerTemplate.WorkerTemplate
	}
	res = quotav1.Add(res, quotacore.PodUsageFunc(&corev1.Pod{Spec: leaderTemplate.Spec}))

	// calculate workers min resources
	for i := int32(0); i < ptr.Deref(lws.Spec.LeaderWorkerTemplate.Size, 1)-1; i++ {
		res = quotav1.Add(res, quotacore.PodUsageFunc(&corev1.Pod{Spec: lws.Spec.LeaderWorkerTemplate.WorkerTemplate.Spec}))
	}

	return res
}

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

package schedulerprovider

import (
	"context"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	volcanov1beta1 "volcano.sh/apis/pkg/apis/scheduling/v1beta1"

	leaderworkerset "sigs.k8s.io/lws/api/leaderworkerset/v1"
	"sigs.k8s.io/lws/pkg/utils"
)

const (
	Volcano                 ProviderType = "volcano"
	VolcanoAnnotationPrefix string       = "volcano.sh/"
)

type VolcanoProvider struct {
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
	log := ctrl.LoggerFrom(ctx).WithValues("podGroup", pgName, "namespace", lws.Namespace)

	if err := v.client.Get(ctx, types.NamespacedName{Name: pgName, Namespace: lws.Namespace}, &pg); err != nil {
		if client.IgnoreNotFound(err) != nil {
			return err
		}
		minResources := utils.CalculatePGMinResources(lws)
		pg = volcanov1beta1.PodGroup{
			ObjectMeta: metav1.ObjectMeta{
				Name:      pgName,
				Namespace: lws.Namespace,
				Labels: map[string]string{
					leaderworkerset.SetNameLabelKey:    lws.Name,
					leaderworkerset.GroupIndexLabelKey: leaderPod.Labels[leaderworkerset.GroupIndexLabelKey],
					leaderworkerset.RevisionKey:        leaderPod.Labels[leaderworkerset.RevisionKey],
				},
				Annotations: inheritVolcanoAnnotations(lws),
			},
			Spec: volcanov1beta1.PodGroupSpec{
				// Default startupPolicy is LeaderCreated, Leader and Workers Pods are scheduled together, so MinAvailable is set to size
				MinMember:    *lws.Spec.LeaderWorkerTemplate.Size,
				MinResources: &minResources,
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
		log.V(2).Info("Created PodGroup for LeaderWorkerSet")
	}

	return nil
}

func (v *VolcanoProvider) InjectPodGroupMetadata(pod *corev1.Pod) error {
	lwsName := pod.Labels[leaderworkerset.SetNameLabelKey]
	groupIndex := pod.Labels[leaderworkerset.GroupIndexLabelKey]
	pod.Annotations[volcanov1beta1.KubeGroupNameAnnotationKey] = GetPodGroupName(lwsName, groupIndex, pod.Labels[leaderworkerset.RevisionKey])

	return nil
}

func inheritVolcanoAnnotations(lws *leaderworkerset.LeaderWorkerSet) map[string]string {
	res := map[string]string{}
	for k, v := range lws.Annotations {
		if strings.HasPrefix(k, VolcanoAnnotationPrefix) {
			res[k] = v
		}
	}
	return res
}

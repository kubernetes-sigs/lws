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

package disaggregatedset

import (
	"context"
	"fmt"
	"maps"
	"strconv"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	leaderworkerset "sigs.k8s.io/lws/api/leaderworkerset/v1"

	disaggregatedsetv1 "sigs.k8s.io/lws/api/disaggregatedset/v1"
)

type GroupedWorkload struct {
	Revision string
	Roles    map[string]WorkloadInfo
}

type GroupedWorkloads []GroupedWorkload

func (groupedWorkloads GroupedWorkloads) GetTotalReplicasPerRole(role string) int {
	total := 0
	for _, workload := range groupedWorkloads {
		total += workload.Roles[role].Replicas
	}
	return total
}

func (groupedWorkloads GroupedWorkloads) GetTotalInitialReplicasPerRole(role string) int {
	total := 0
	for _, workload := range groupedWorkloads {
		total += workload.Roles[role].InitialReplicas
	}
	return total
}

type LeaderWorkerSetManager struct {
	client client.Client
}

func NewLeaderWorkerSetManager(c client.Client) *LeaderWorkerSetManager {
	return &LeaderWorkerSetManager{client: c}
}

func mergeLabels(userLabels, autoLabels map[string]string) map[string]string {
	merged := make(map[string]string, len(userLabels)+len(autoLabels))
	maps.Copy(merged, userLabels)
	maps.Copy(merged, autoLabels)
	return merged
}

func copyAnnotations(annotations map[string]string) map[string]string {
	if len(annotations) == 0 {
		return nil
	}
	return maps.Clone(annotations)
}

func (manager *LeaderWorkerSetManager) Create(ctx context.Context, params CreateParams) error {
	workloadName := GenerateName(params.DisaggregatedSet.Name, params.Role, params.Revision)
	replicas := int32(params.Replicas)
	config := params.Config

	workerLabels := mergeLabels(config.Spec.LeaderWorkerTemplate.WorkerTemplate.Labels, params.Labels)
	workerAnnotations := copyAnnotations(config.Spec.LeaderWorkerTemplate.WorkerTemplate.Annotations)

	lwsTemplate := leaderworkerset.LeaderWorkerTemplate{
		Size: config.Spec.LeaderWorkerTemplate.Size,
		WorkerTemplate: corev1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{
				Labels:      workerLabels,
				Annotations: workerAnnotations,
			},
			Spec: config.Spec.LeaderWorkerTemplate.WorkerTemplate.Spec,
		},
		RestartPolicy:                        config.Spec.LeaderWorkerTemplate.RestartPolicy,
		VolumeClaimTemplates:                 config.Spec.LeaderWorkerTemplate.VolumeClaimTemplates,
		PersistentVolumeClaimRetentionPolicy: config.Spec.LeaderWorkerTemplate.PersistentVolumeClaimRetentionPolicy,
	}

	if config.Spec.LeaderWorkerTemplate.LeaderTemplate != nil {
		leaderLabels := mergeLabels(config.Spec.LeaderWorkerTemplate.LeaderTemplate.Labels, params.Labels)
		leaderAnnotations := copyAnnotations(config.Spec.LeaderWorkerTemplate.LeaderTemplate.Annotations)
		lwsTemplate.LeaderTemplate = &corev1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{
				Labels:      leaderLabels,
				Annotations: leaderAnnotations,
			},
			Spec: config.Spec.LeaderWorkerTemplate.LeaderTemplate.Spec,
		}
	}

	lwsTemplate.SubGroupPolicy = config.Spec.LeaderWorkerTemplate.SubGroupPolicy

	lwsNetworkConfig := config.Spec.NetworkConfig

	lwsLabels := mergeLabels(config.ObjectMeta.Labels, params.Labels)
	lwsAnnotations := copyAnnotations(config.ObjectMeta.Annotations)

	leaderWorkerSet := &leaderworkerset.LeaderWorkerSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:        workloadName,
			Namespace:   params.DisaggregatedSet.Namespace,
			Labels:      lwsLabels,
			Annotations: lwsAnnotations,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: disaggregatedsetv1.GroupVersion.String(),
				Kind:       "DisaggregatedSet",
				Name:       params.DisaggregatedSet.Name,
				UID:        params.DisaggregatedSet.UID,
				Controller: ptr.To(true),
			}},
		},
		Spec: leaderworkerset.LeaderWorkerSetSpec{
			Replicas:             &replicas,
			StartupPolicy:        config.Spec.StartupPolicy,
			LeaderWorkerTemplate: lwsTemplate,
			NetworkConfig:        lwsNetworkConfig,
		},
	}

	if err := manager.client.Create(ctx, leaderWorkerSet); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return nil
		}
		return fmt.Errorf("failed to create LeaderWorkerSet %s: %w", workloadName, err)
	}

	return nil
}

func (manager *LeaderWorkerSetManager) Scale(ctx context.Context, namespace, name string, replicas int) error {
	leaderWorkerSet := &leaderworkerset.LeaderWorkerSet{}
	if err := manager.client.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, leaderWorkerSet); err != nil {
		return fmt.Errorf("failed to get LeaderWorkerSet %s for scaling: %w", name, err)
	}

	if int(getLWSReplicas(leaderWorkerSet)) == replicas {
		return nil
	}

	replicas32 := int32(replicas)
	patch := client.MergeFrom(leaderWorkerSet.DeepCopy())
	leaderWorkerSet.Spec.Replicas = &replicas32
	if err := manager.client.Patch(ctx, leaderWorkerSet, patch); err != nil {
		return fmt.Errorf("failed to scale LeaderWorkerSet %s: %w", name, err)
	}

	return nil
}

func (manager *LeaderWorkerSetManager) UpdateInitialReplicasAnnotation(ctx context.Context, namespace, name string, replicas int) error {
	leaderWorkerSet := &leaderworkerset.LeaderWorkerSet{}
	if err := manager.client.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, leaderWorkerSet); err != nil {
		return fmt.Errorf("failed to get LeaderWorkerSet %s for annotation update: %w", name, err)
	}

	currentValue, ok := GetInitialReplicas(leaderWorkerSet)
	if ok && int(currentValue) == replicas {
		return nil
	}

	patch := client.MergeFrom(leaderWorkerSet.DeepCopy())
	SetInitialReplicas(leaderWorkerSet, int32(replicas))
	if err := manager.client.Patch(ctx, leaderWorkerSet, patch); err != nil {
		return fmt.Errorf("failed to update annotation on LeaderWorkerSet %s: %w", name, err)
	}

	return nil
}

func (manager *LeaderWorkerSetManager) Get(ctx context.Context, namespace, name string) (*WorkloadInfo, error) {
	leaderWorkerSet := &leaderworkerset.LeaderWorkerSet{}
	err := manager.client.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, leaderWorkerSet)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("failed to get LeaderWorkerSet %s: %w", name, err)
	}

	initialReplicas, hasAnnotation := GetInitialReplicas(leaderWorkerSet)
	if !hasAnnotation {
		initialReplicas = getLWSReplicas(leaderWorkerSet)
	}

	return &WorkloadInfo{
		Name:                         leaderWorkerSet.Name,
		Namespace:                    leaderWorkerSet.Namespace,
		Role:                         leaderWorkerSet.Labels[LabelDisaggRole],
		Revision:                     leaderWorkerSet.Labels[LabelRevision],
		Replicas:                     int(getLWSReplicas(leaderWorkerSet)),
		ReadyReplicas:                int(leaderWorkerSet.Status.ReadyReplicas),
		InitialReplicas:              int(initialReplicas),
		HasInitialReplicasAnnotation: hasAnnotation,
		CreationTimestamp:            leaderWorkerSet.CreationTimestamp.Time,
	}, nil
}

func (manager *LeaderWorkerSetManager) List(ctx context.Context, namespace, disaggDeploymentName, role string) ([]WorkloadInfo, error) {
	workloadList := &leaderworkerset.LeaderWorkerSetList{}

	labels := client.MatchingLabels{LabelDisaggName: disaggDeploymentName}
	if role != "" {
		labels[LabelDisaggRole] = role
	}

	if err := manager.client.List(ctx, workloadList, client.InNamespace(namespace), labels); err != nil {
		return nil, fmt.Errorf("failed to list LeaderWorkerSets for %s/%s: %w", namespace, disaggDeploymentName, err)
	}

	result := make([]WorkloadInfo, 0, len(workloadList.Items))
	for i := range workloadList.Items {
		leaderWorkerSet := &workloadList.Items[i]
		initialReplicas, hasAnnotation := GetInitialReplicas(leaderWorkerSet)
		if !hasAnnotation {
			initialReplicas = getLWSReplicas(leaderWorkerSet)
		}
		result = append(result, WorkloadInfo{
			Name:                         leaderWorkerSet.Name,
			Namespace:                    leaderWorkerSet.Namespace,
			Role:                         leaderWorkerSet.Labels[LabelDisaggRole],
			Revision:                     leaderWorkerSet.Labels[LabelRevision],
			Replicas:                     int(getLWSReplicas(leaderWorkerSet)),
			ReadyReplicas:                int(leaderWorkerSet.Status.ReadyReplicas),
			InitialReplicas:              int(initialReplicas),
			HasInitialReplicasAnnotation: hasAnnotation,
			CreationTimestamp:            leaderWorkerSet.CreationTimestamp.Time,
		})
	}

	return result, nil
}

func (manager *LeaderWorkerSetManager) Delete(ctx context.Context, namespace, name string) error {
	leaderWorkerSet := &leaderworkerset.LeaderWorkerSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
	}

	if err := manager.client.Delete(ctx, leaderWorkerSet); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("failed to delete LeaderWorkerSet %s: %w", name, err)
	}

	return nil
}

func getLWSReplicas(leaderWorkerSet *leaderworkerset.LeaderWorkerSet) int32 {
	if leaderWorkerSet.Spec.Replicas == nil {
		return 1
	}
	return *leaderWorkerSet.Spec.Replicas
}

func groupWorkloadsByRevision(workloads []WorkloadInfo) GroupedWorkloads {
	byRevision := make(map[string]*GroupedWorkload)
	for _, workload := range workloads {
		if byRevision[workload.Revision] == nil {
			byRevision[workload.Revision] = &GroupedWorkload{
				Revision: workload.Revision,
				Roles:    make(map[string]WorkloadInfo),
			}
		}
		byRevision[workload.Revision].Roles[workload.Role] = workload
	}

	result := make(GroupedWorkloads, 0, len(byRevision))
	for _, grouped := range byRevision {
		result = append(result, *grouped)
	}
	return result
}

func (manager *LeaderWorkerSetManager) GetGroupedWorkloads(
	ctx context.Context,
	namespace, disaggDeploymentName, revision string,
) (GroupedWorkloads, *GroupedWorkload, error) {
	workloads, err := manager.List(ctx, namespace, disaggDeploymentName, "")
	if err != nil {
		return nil, nil, fmt.Errorf("failed to list workloads: %w", err)
	}

	var oldWorkloadInfos []WorkloadInfo
	var newWorkloadInfos []WorkloadInfo
	for _, workload := range workloads {
		if workload.Revision == revision {
			newWorkloadInfos = append(newWorkloadInfos, workload)
		} else {
			oldWorkloadInfos = append(oldWorkloadInfos, workload)
		}
	}

	oldWorkloads := groupWorkloadsByRevision(oldWorkloadInfos)
	newGrouped := groupWorkloadsByRevision(newWorkloadInfos)

	var newWorkload *GroupedWorkload
	if len(newGrouped) > 0 {
		newWorkload = &newGrouped[0]
	}

	return oldWorkloads, newWorkload, nil
}

func parseInitialReplicasAnnotation(leaderWorkerSet *leaderworkerset.LeaderWorkerSet) *int {
	if leaderWorkerSet.Annotations == nil {
		return nil
	}
	valueStr, ok := leaderWorkerSet.Annotations[AnnotationInitialReplicas]
	if !ok {
		return nil
	}
	parsed, err := strconv.Atoi(valueStr)
	if err != nil {
		return nil
	}
	return &parsed
}

func (manager *LeaderWorkerSetManager) patchInitialReplicasAnnotation(
	ctx context.Context,
	leaderWorkerSet *leaderworkerset.LeaderWorkerSet,
	value int,
) error {
	patch := client.MergeFrom(leaderWorkerSet.DeepCopy())
	if leaderWorkerSet.Annotations == nil {
		leaderWorkerSet.Annotations = make(map[string]string)
	}
	leaderWorkerSet.Annotations[AnnotationInitialReplicas] = strconv.Itoa(value)
	return manager.client.Patch(ctx, leaderWorkerSet, patch)
}

func (manager *LeaderWorkerSetManager) SetInitialReplicas(
	ctx context.Context,
	namespace, name string,
	replicas int,
) (*int, error) {
	log := logf.FromContext(ctx)

	leaderWorkerSet := &leaderworkerset.LeaderWorkerSet{}
	if err := manager.client.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, leaderWorkerSet); err != nil {
		return nil, fmt.Errorf("failed to get LeaderWorkerSet %s: %w", name, err)
	}

	oldValue := parseInitialReplicasAnnotation(leaderWorkerSet)

	if oldValue != nil && *oldValue != replicas {
		log.Info("WARNING: Overwriting initial-replicas annotation with different value",
			"workload", name,
			"oldValue", *oldValue,
			"newValue", replicas)
	}

	if oldValue != nil && *oldValue == replicas {
		return oldValue, nil
	}

	if err := manager.patchInitialReplicasAnnotation(ctx, leaderWorkerSet, replicas); err != nil {
		return nil, fmt.Errorf("failed to update initial-replicas annotation on %s: %w", name, err)
	}

	return oldValue, nil
}

func (manager *LeaderWorkerSetManager) GetInitialReplicas(
	ctx context.Context,
	namespace, name string,
) (*int, error) {
	leaderWorkerSet := &leaderworkerset.LeaderWorkerSet{}
	if err := manager.client.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, leaderWorkerSet); err != nil {
		return nil, fmt.Errorf("failed to get LeaderWorkerSet %s: %w", name, err)
	}

	return parseInitialReplicasAnnotation(leaderWorkerSet), nil
}

func (manager *LeaderWorkerSetManager) GetOrSetInitialReplicas(
	ctx context.Context,
	namespace, name string,
	defaultValue int,
) (int, error) {
	leaderWorkerSet := &leaderworkerset.LeaderWorkerSet{}
	if err := manager.client.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, leaderWorkerSet); err != nil {
		return 0, fmt.Errorf("failed to get LeaderWorkerSet %s: %w", name, err)
	}

	if existing := parseInitialReplicasAnnotation(leaderWorkerSet); existing != nil {
		return *existing, nil
	}

	if err := manager.patchInitialReplicasAnnotation(ctx, leaderWorkerSet, defaultValue); err != nil {
		return 0, fmt.Errorf("failed to set initial-replicas annotation on %s: %w", name, err)
	}

	return defaultValue, nil
}

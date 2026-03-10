/*
Copyright 2026.

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

package controller

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

	disaggv1alpha1 "sigs.k8s.io/disaggregatedset/api/v1alpha1"
)

// GroupedWorkload groups workloads by revision, containing both prefill and decode sides.
type GroupedWorkload struct {
	Revision string
	Sides    map[string]WorkloadInfo
}

// GroupedWorkloads is a slice of GroupedWorkload.
type GroupedWorkloads []GroupedWorkload

// GetTotalReplicasPerSide returns the sum of current replicas for a given side across all grouped workloads.
func (groupedWorkloads GroupedWorkloads) GetTotalReplicasPerSide(side string) int {
	total := 0
	for _, workload := range groupedWorkloads {
		total += workload.Sides[side].Replicas
	}
	return total
}

// GetTotalInitialReplicasPerSide returns the sum of initial replicas for a given side across all grouped workloads.
func (groupedWorkloads GroupedWorkloads) GetTotalInitialReplicasPerSide(side string) int {
	total := 0
	for _, workload := range groupedWorkloads {
		total += workload.Sides[side].InitialReplicas
	}
	return total
}

// LeaderWorkerSetManager manages LeaderWorkerSet resources for DisaggregatedSet
type LeaderWorkerSetManager struct {
	client client.Client
}

// NewLeaderWorkerSetManager creates a new LeaderWorkerSetManager
func NewLeaderWorkerSetManager(c client.Client) *LeaderWorkerSetManager {
	return &LeaderWorkerSetManager{client: c}
}

// mergeLabels merges user-provided labels with auto-populated labels.
// Auto-populated labels take precedence over user labels.
func mergeLabels(userLabels, autoLabels map[string]string) map[string]string {
	merged := make(map[string]string, len(userLabels)+len(autoLabels))
	// Copy user labels first
	maps.Copy(merged, userLabels)
	// Overlay auto-populated labels (these take precedence)
	maps.Copy(merged, autoLabels)
	return merged
}

// copyAnnotations creates a copy of annotations map.
// Returns nil if the input is nil or empty.
func copyAnnotations(annotations map[string]string) map[string]string {
	if len(annotations) == 0 {
		return nil
	}
	return maps.Clone(annotations)
}

// Create creates a new LeaderWorkerSet with the given configuration.
// All fields from DisaggSideConfig are passed through to the LWS spec.
// User-provided labels and annotations from workerTemplate.metadata are merged with
// auto-populated labels. Auto-populated labels take precedence.
func (manager *LeaderWorkerSetManager) Create(ctx context.Context, params CreateParams) error {
	workloadName := GenerateName(params.DisaggregatedSet.Name, params.Side, params.Revision)
	replicas := int32(params.Replicas)
	config := params.Config

	// Merge user-provided labels with auto-populated labels for workerTemplate
	workerLabels := mergeLabels(config.LeaderWorkerTemplate.WorkerTemplate.Labels, params.Labels)
	workerAnnotations := copyAnnotations(config.LeaderWorkerTemplate.WorkerTemplate.Annotations)

	// Build LWS LeaderWorkerTemplate from DisaggSideConfig.LeaderWorkerTemplate
	lwsTemplate := leaderworkerset.LeaderWorkerTemplate{
		Size: config.LeaderWorkerTemplate.Size,
		WorkerTemplate: corev1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{
				Labels:      workerLabels,
				Annotations: workerAnnotations,
			},
			Spec: config.LeaderWorkerTemplate.WorkerTemplate.Spec,
		},
		RestartPolicy:                        config.LeaderWorkerTemplate.RestartPolicy,
		VolumeClaimTemplates:                 config.LeaderWorkerTemplate.VolumeClaimTemplates,
		PersistentVolumeClaimRetentionPolicy: config.LeaderWorkerTemplate.PersistentVolumeClaimRetentionPolicy,
	}

	// Pass through LeaderTemplate if specified
	if config.LeaderWorkerTemplate.LeaderTemplate != nil {
		// Merge user-provided labels with auto-populated labels for leaderTemplate
		leaderLabels := mergeLabels(config.LeaderWorkerTemplate.LeaderTemplate.Labels, params.Labels)
		leaderAnnotations := copyAnnotations(config.LeaderWorkerTemplate.LeaderTemplate.Annotations)
		lwsTemplate.LeaderTemplate = &corev1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{
				Labels:      leaderLabels,
				Annotations: leaderAnnotations,
			},
			Spec: config.LeaderWorkerTemplate.LeaderTemplate.Spec,
		}
	}

	// Pass through SubGroupPolicy directly (already leaderworkerset type)
	lwsTemplate.SubGroupPolicy = config.LeaderWorkerTemplate.SubGroupPolicy

	// NOTE: We do NOT set RolloutStrategy on the LWS.
	// Our operator handles rolling updates by creating NEW LWS workloads and scaling them.
	// The DisaggSideConfig.RolloutStrategy is used by our planner/executor, not by the LWS controller.

	// Pass through NetworkConfig directly (already leaderworkerset type)
	lwsNetworkConfig := config.NetworkConfig

	leaderWorkerSet := &leaderworkerset.LeaderWorkerSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      workloadName,
			Namespace: params.DisaggregatedSet.Namespace,
			Labels:    params.Labels,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: disaggv1alpha1.GroupVersion.String(),
				Kind:       "DisaggregatedSet",
				Name:       params.DisaggregatedSet.Name,
				UID:        params.DisaggregatedSet.UID,
				Controller: ptr.To(true),
			}},
		},
		Spec: leaderworkerset.LeaderWorkerSetSpec{
			Replicas:             &replicas,
			StartupPolicy:        config.StartupPolicy,
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

// Scale adjusts the replica count of an existing LeaderWorkerSet.
// Note: Only replicas are scaled; other fields are treated as read-only.
func (manager *LeaderWorkerSetManager) Scale(ctx context.Context, namespace, name string, replicas int) error {
	leaderWorkerSet := &leaderworkerset.LeaderWorkerSet{}
	if err := manager.client.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, leaderWorkerSet); err != nil {
		return fmt.Errorf("failed to get LeaderWorkerSet %s for scaling: %w", name, err)
	}

	if int(getLWSReplicas(leaderWorkerSet)) == replicas {
		return nil // Already at desired scale
	}

	replicas32 := int32(replicas)
	patch := client.MergeFrom(leaderWorkerSet.DeepCopy())
	leaderWorkerSet.Spec.Replicas = &replicas32
	if err := manager.client.Patch(ctx, leaderWorkerSet, patch); err != nil {
		return fmt.Errorf("failed to scale LeaderWorkerSet %s: %w", name, err)
	}

	return nil
}

// UpdateInitialReplicasAnnotation updates the initial-replicas annotation on an existing LWS.
// This is used to reset the annotation when a new rolling update is initiated.
func (manager *LeaderWorkerSetManager) UpdateInitialReplicasAnnotation(ctx context.Context, namespace, name string, replicas int) error {
	leaderWorkerSet := &leaderworkerset.LeaderWorkerSet{}
	if err := manager.client.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, leaderWorkerSet); err != nil {
		return fmt.Errorf("failed to get LeaderWorkerSet %s for annotation update: %w", name, err)
	}

	// Check if annotation already has the correct value
	currentValue, ok := GetInitialReplicas(leaderWorkerSet)
	if ok && int(currentValue) == replicas {
		return nil // Already has correct annotation
	}

	patch := client.MergeFrom(leaderWorkerSet.DeepCopy())
	SetInitialReplicas(leaderWorkerSet, int32(replicas))
	if err := manager.client.Patch(ctx, leaderWorkerSet, patch); err != nil {
		return fmt.Errorf("failed to update annotation on LeaderWorkerSet %s: %w", name, err)
	}

	return nil
}

// Get retrieves LeaderWorkerSet info, returns nil if not found
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
		initialReplicas = getLWSReplicas(leaderWorkerSet) // fallback to current replicas
	}

	return &WorkloadInfo{
		Name:                         leaderWorkerSet.Name,
		Namespace:                    leaderWorkerSet.Namespace,
		Side:                         leaderWorkerSet.Labels[LabelDisaggSide],
		Revision:                     leaderWorkerSet.Labels[LabelRevision],
		Replicas:                     int(getLWSReplicas(leaderWorkerSet)),
		ReadyReplicas:                int(leaderWorkerSet.Status.ReadyReplicas),
		InitialReplicas:              int(initialReplicas),
		HasInitialReplicasAnnotation: hasAnnotation,
		CreationTimestamp:            leaderWorkerSet.CreationTimestamp.Time,
	}, nil
}

// List returns all LeaderWorkerSets for a DisaggregatedSet.
// If side is empty, returns workloads for all sides.
func (manager *LeaderWorkerSetManager) List(ctx context.Context, namespace, disaggDeploymentName, side string) ([]WorkloadInfo, error) {
	workloadList := &leaderworkerset.LeaderWorkerSetList{}

	labels := client.MatchingLabels{LabelDisaggName: disaggDeploymentName}
	if side != "" {
		labels[LabelDisaggSide] = side
	}

	if err := manager.client.List(ctx, workloadList, client.InNamespace(namespace), labels); err != nil {
		return nil, fmt.Errorf("failed to list LeaderWorkerSets for %s/%s: %w", namespace, disaggDeploymentName, err)
	}

	result := make([]WorkloadInfo, 0, len(workloadList.Items))
	for i := range workloadList.Items {
		leaderWorkerSet := &workloadList.Items[i]
		initialReplicas, hasAnnotation := GetInitialReplicas(leaderWorkerSet)
		if !hasAnnotation {
			initialReplicas = getLWSReplicas(leaderWorkerSet) // fallback to current replicas
		}
		result = append(result, WorkloadInfo{
			Name:                         leaderWorkerSet.Name,
			Namespace:                    leaderWorkerSet.Namespace,
			Side:                         leaderWorkerSet.Labels[LabelDisaggSide],
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

// Delete removes a LeaderWorkerSet
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

// getLWSReplicas safely gets the replica count from a LeaderWorkerSet
func getLWSReplicas(leaderWorkerSet *leaderworkerset.LeaderWorkerSet) int32 {
	if leaderWorkerSet.Spec.Replicas == nil {
		return 1
	}
	return *leaderWorkerSet.Spec.Replicas
}

// groupWorkloadsByRevision groups a list of WorkloadInfo by revision into GroupedWorkloads.
func groupWorkloadsByRevision(workloads []WorkloadInfo) GroupedWorkloads {
	byRevision := make(map[string]*GroupedWorkload)
	for _, workload := range workloads {
		if byRevision[workload.Revision] == nil {
			byRevision[workload.Revision] = &GroupedWorkload{
				Revision: workload.Revision,
				Sides:    make(map[string]WorkloadInfo),
			}
		}
		byRevision[workload.Revision].Sides[workload.Side] = workload
	}

	result := make(GroupedWorkloads, 0, len(byRevision))
	for _, grouped := range byRevision {
		result = append(result, *grouped)
	}
	return result
}

// GetGroupedWorkloads fetches all workloads from the cluster and groups them by revision.
// Returns (oldWorkloads, newWorkload, error).
// oldWorkloads contains all workloads that don't match the current revision (empty if none).
// newWorkload is nil if no workload exists for the current revision.
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

// parseInitialReplicasAnnotation extracts the initial-replicas value from a LeaderWorkerSet's annotations.
// Returns nil if the annotation is not set or invalid.
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

// patchInitialReplicasAnnotation patches the initial-replicas annotation on a LeaderWorkerSet.
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

// SetInitialReplicas sets the initial-replicas annotation on a workload.
// Always updates the annotation, even if already set.
// Returns the previous value (nil if not set) and any error.
// Logs a warning if overwriting a different value.
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

	// Warn if overwriting a different value
	if oldValue != nil && *oldValue != replicas {
		log.Info("WARNING: Overwriting initial-replicas annotation with different value",
			"workload", name,
			"oldValue", *oldValue,
			"newValue", replicas)
	}

	// Skip update if value is already correct
	if oldValue != nil && *oldValue == replicas {
		return oldValue, nil
	}

	if err := manager.patchInitialReplicasAnnotation(ctx, leaderWorkerSet, replicas); err != nil {
		return nil, fmt.Errorf("failed to update initial-replicas annotation on %s: %w", name, err)
	}

	return oldValue, nil
}

// GetInitialReplicas reads the initial-replicas annotation from a workload.
// Returns nil if the annotation is not set.
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

// GetOrSetInitialReplicas reads the initial-replicas annotation, or sets it if not present.
// If the annotation exists, returns the existing value without modifying it.
// If the annotation doesn't exist, sets it to defaultValue and returns defaultValue.
func (manager *LeaderWorkerSetManager) GetOrSetInitialReplicas(
	ctx context.Context,
	namespace, name string,
	defaultValue int,
) (int, error) {
	leaderWorkerSet := &leaderworkerset.LeaderWorkerSet{}
	if err := manager.client.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, leaderWorkerSet); err != nil {
		return 0, fmt.Errorf("failed to get LeaderWorkerSet %s: %w", name, err)
	}

	// Check if annotation already exists
	if existing := parseInitialReplicasAnnotation(leaderWorkerSet); existing != nil {
		return *existing, nil
	}

	if err := manager.patchInitialReplicasAnnotation(ctx, leaderWorkerSet, defaultValue); err != nil {
		return 0, fmt.Errorf("failed to set initial-replicas annotation on %s: %w", name, err)
	}

	return defaultValue, nil
}

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

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	leaderworkersetv1 "sigs.k8s.io/lws/api/leaderworkerset/v1"

	disaggregatedsetv1 "sigs.k8s.io/lws/api/disaggregatedset/v1"
	disaggregatedsetutils "sigs.k8s.io/lws/pkg/utils/disaggregatedset"
)

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

func (manager *LeaderWorkerSetManager) Create(ctx context.Context, params disaggregatedsetutils.CreateParams) error {
	lwsName := disaggregatedsetutils.GenerateName(params.DisaggregatedSet.Name, params.Role, params.Revision)
	replicas := int32(params.Replicas)
	config := params.Config

	// Copy the spec and override replicas.
	lwsSpec := config.Spec
	lwsSpec.Replicas = &replicas

	// Inject system labels (role, name, revision) into pod templates.
	// These don't come from the user's spec — services select pods by them.
	lwsSpec.LeaderWorkerTemplate.WorkerTemplate.Labels = mergeLabels(config.Spec.LeaderWorkerTemplate.WorkerTemplate.Labels, params.Labels)
	// Defensive copy: struct copy is shallow, so maps are shared with the original config.
	lwsSpec.LeaderWorkerTemplate.WorkerTemplate.Annotations = copyAnnotations(config.Spec.LeaderWorkerTemplate.WorkerTemplate.Annotations)

	if lwsSpec.LeaderWorkerTemplate.LeaderTemplate != nil {
		lwsSpec.LeaderWorkerTemplate.LeaderTemplate = lwsSpec.LeaderWorkerTemplate.LeaderTemplate.DeepCopy()
		lwsSpec.LeaderWorkerTemplate.LeaderTemplate.Labels = mergeLabels(config.Spec.LeaderWorkerTemplate.LeaderTemplate.Labels, params.Labels)
		lwsSpec.LeaderWorkerTemplate.LeaderTemplate.Annotations = copyAnnotations(config.Spec.LeaderWorkerTemplate.LeaderTemplate.Annotations)
	}

	leaderWorkerSet := &leaderworkersetv1.LeaderWorkerSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:        lwsName,
			Namespace:   params.DisaggregatedSet.Namespace,
			Labels:      mergeLabels(config.ObjectMeta.Labels, params.Labels),
			Annotations: copyAnnotations(config.ObjectMeta.Annotations),
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: disaggregatedsetv1.GroupVersion.String(),
				Kind:       "DisaggregatedSet",
				Name:       params.DisaggregatedSet.Name,
				UID:        params.DisaggregatedSet.UID,
				Controller: ptr.To(true),
			}},
		},
		Spec: lwsSpec,
	}

	if err := manager.client.Create(ctx, leaderWorkerSet); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return nil
		}
		return fmt.Errorf("failed to create LeaderWorkerSet %s: %w", lwsName, err)
	}

	return nil
}

func (manager *LeaderWorkerSetManager) Scale(ctx context.Context, namespace, name string, replicas int) error {
	leaderWorkerSet := &leaderworkersetv1.LeaderWorkerSet{}
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
	leaderWorkerSet := &leaderworkersetv1.LeaderWorkerSet{}
	if err := manager.client.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, leaderWorkerSet); err != nil {
		return fmt.Errorf("failed to get LeaderWorkerSet %s for annotation update: %w", name, err)
	}

	currentValue, ok := disaggregatedsetutils.GetInitialReplicas(leaderWorkerSet)
	if ok && int(currentValue) == replicas {
		return nil
	}

	patch := client.MergeFrom(leaderWorkerSet.DeepCopy())
	disaggregatedsetutils.SetInitialReplicas(leaderWorkerSet, int32(replicas))
	if err := manager.client.Patch(ctx, leaderWorkerSet, patch); err != nil {
		return fmt.Errorf("failed to update annotation on LeaderWorkerSet %s: %w", name, err)
	}

	return nil
}

func (manager *LeaderWorkerSetManager) Get(ctx context.Context, namespace, name string) (*leaderworkersetv1.LeaderWorkerSet, error) {
	lws := &leaderworkersetv1.LeaderWorkerSet{}
	err := manager.client.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, lws)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("failed to get LeaderWorkerSet %s: %w", name, err)
	}
	return lws, nil
}

func (manager *LeaderWorkerSetManager) List(ctx context.Context, namespace, disaggDeploymentName, role string) ([]*leaderworkersetv1.LeaderWorkerSet, error) {
	lwsObjList := &leaderworkersetv1.LeaderWorkerSetList{}

	labels := client.MatchingLabels{disaggregatedsetv1.SetNameLabelKey: disaggDeploymentName}
	if role != "" {
		labels[disaggregatedsetv1.RoleLabelKey] = role
	}

	if err := manager.client.List(ctx, lwsObjList, client.InNamespace(namespace), labels); err != nil {
		return nil, fmt.Errorf("failed to list LeaderWorkerSets for %s/%s: %w", namespace, disaggDeploymentName, err)
	}

	result := make([]*leaderworkersetv1.LeaderWorkerSet, len(lwsObjList.Items))
	for i := range lwsObjList.Items {
		result[i] = &lwsObjList.Items[i]
	}
	return result, nil
}

func (manager *LeaderWorkerSetManager) Delete(ctx context.Context, namespace, name string) error {
	leaderWorkerSet := &leaderworkersetv1.LeaderWorkerSet{
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

func getLWSReplicas(leaderWorkerSet *leaderworkersetv1.LeaderWorkerSet) int32 {
	if leaderWorkerSet.Spec.Replicas == nil {
		return 1
	}
	return *leaderWorkerSet.Spec.Replicas
}

func (manager *LeaderWorkerSetManager) GetRevisionRolesList(
	ctx context.Context,
	namespace, disaggDeploymentName, revision string,
) (disaggregatedsetutils.RevisionRolesList, *disaggregatedsetutils.RevisionRoles, error) {
	lwsList, err := manager.List(ctx, namespace, disaggDeploymentName, "")
	if err != nil {
		return nil, nil, fmt.Errorf("failed to list LWS: %w", err)
	}

	var oldLWS []*leaderworkersetv1.LeaderWorkerSet
	var newLWS []*leaderworkersetv1.LeaderWorkerSet
	for _, lws := range lwsList {
		if lws.Labels[disaggregatedsetv1.RevisionLabelKey] == revision {
			newLWS = append(newLWS, lws)
		} else {
			oldLWS = append(oldLWS, lws)
		}
	}

	oldRevisions := disaggregatedsetutils.GroupByRevision(oldLWS)
	newGrouped := disaggregatedsetutils.GroupByRevision(newLWS)

	var newRevision *disaggregatedsetutils.RevisionRoles
	if len(newGrouped) > 0 {
		newRevision = &newGrouped[0]
	}

	return oldRevisions, newRevision, nil
}

func parseInitialReplicasAnnotation(leaderWorkerSet *leaderworkersetv1.LeaderWorkerSet) *int {
	if leaderWorkerSet.Annotations == nil {
		return nil
	}
	valueStr, ok := leaderWorkerSet.Annotations[disaggregatedsetv1.InitialReplicasAnnotationKey]
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
	leaderWorkerSet *leaderworkersetv1.LeaderWorkerSet,
	value int,
) error {
	patch := client.MergeFrom(leaderWorkerSet.DeepCopy())
	if leaderWorkerSet.Annotations == nil {
		leaderWorkerSet.Annotations = make(map[string]string)
	}
	leaderWorkerSet.Annotations[disaggregatedsetv1.InitialReplicasAnnotationKey] = strconv.Itoa(value)
	return manager.client.Patch(ctx, leaderWorkerSet, patch)
}

func (manager *LeaderWorkerSetManager) SetInitialReplicas(
	ctx context.Context,
	namespace, name string,
	replicas int,
) (*int, error) {
	log := logf.FromContext(ctx)

	leaderWorkerSet := &leaderworkersetv1.LeaderWorkerSet{}
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
	leaderWorkerSet := &leaderworkersetv1.LeaderWorkerSet{}
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
	leaderWorkerSet := &leaderworkersetv1.LeaderWorkerSet{}
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

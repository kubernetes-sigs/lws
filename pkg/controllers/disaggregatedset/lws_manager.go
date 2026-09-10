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
	lwsName := disaggregatedsetutils.GenerateName(params.DisaggregatedSet.Name, params.Slice, params.Revision, params.Role)
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
	// Inject placement affinity (no-op when no policy is set). The helper deep-copies
	// any existing affinity, so the shared worker template is not mutated.
	disaggregatedsetutils.SetPlacementAffinities(&lwsSpec.LeaderWorkerTemplate.WorkerTemplate.Spec, params.DisaggregatedSet.Name, params.Slice, params.DisaggregatedSet.Spec.PlacementPolicy)

	if lwsSpec.LeaderWorkerTemplate.LeaderTemplate != nil {
		lwsSpec.LeaderWorkerTemplate.LeaderTemplate = lwsSpec.LeaderWorkerTemplate.LeaderTemplate.DeepCopy()
		lwsSpec.LeaderWorkerTemplate.LeaderTemplate.Labels = mergeLabels(config.Spec.LeaderWorkerTemplate.LeaderTemplate.Labels, params.Labels)
		lwsSpec.LeaderWorkerTemplate.LeaderTemplate.Annotations = copyAnnotations(config.Spec.LeaderWorkerTemplate.LeaderTemplate.Annotations)
		disaggregatedsetutils.SetPlacementAffinities(&lwsSpec.LeaderWorkerTemplate.LeaderTemplate.Spec, params.DisaggregatedSet.Name, params.Slice, params.DisaggregatedSet.Spec.PlacementPolicy)
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
		if !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("failed to create LeaderWorkerSet %s: %w", lwsName, err)
		}
		// Name is taken. If we already own it, a concurrent reconcile of this
		// same DisaggregatedSet beat us to it — no-op. If it's foreign-owned
		// (see #981), error instead of silently no-oping: this DS's watches
		// won't fire again for a foreign object it doesn't own, so a silent
		// return here could leave the role permanently missing an LWS until
		// some unrelated trigger causes another reconcile. Returning an error
		// requeues instead.
		existing := &leaderworkersetv1.LeaderWorkerSet{}
		if getErr := manager.client.Get(ctx, types.NamespacedName{Name: lwsName, Namespace: params.DisaggregatedSet.Namespace}, existing); getErr != nil {
			return fmt.Errorf("failed to get existing LeaderWorkerSet %s after create conflict: %w", lwsName, getErr)
		}
		if !metav1.IsControlledBy(existing, params.DisaggregatedSet) {
			return fmt.Errorf("LeaderWorkerSet %s exists but is not controlled by DisaggregatedSet %s; refusing to adopt it", lwsName, params.DisaggregatedSet.Name)
		}
		return nil
	}

	log := logf.FromContext(ctx)
	log.Info("Created LWS", "name", lwsName, "role", params.Role, "revision", params.Revision, "replicas", params.Replicas)
	return nil
}

// Scale patches the LWS named name to replicas, but only if it's actually
// controller-owned by ds. A same-named LWS that exists but isn't owned by ds
// — e.g. left over from a same-named DisaggregatedSet that was deleted and
// recreated before garbage collection ran — is refused rather than mutated;
// see #981.
func (manager *LeaderWorkerSetManager) Scale(ctx context.Context, ds *disaggregatedsetv1.DisaggregatedSet, name string, replicas int) error {
	leaderWorkerSet := &leaderworkersetv1.LeaderWorkerSet{}
	if err := manager.client.Get(ctx, types.NamespacedName{Name: name, Namespace: ds.Namespace}, leaderWorkerSet); err != nil {
		return fmt.Errorf("failed to get LeaderWorkerSet %s for scaling: %w", name, err)
	}
	if !metav1.IsControlledBy(leaderWorkerSet, ds) {
		return fmt.Errorf("LeaderWorkerSet %s exists but is not controlled by DisaggregatedSet %s; refusing to scale it", name, ds.Name)
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

// Get returns the LWS named name, but only if it's actually controller-owned
// by ds — consistent with List's ownership filtering. A same-named LWS that
// exists but isn't owned by ds (e.g. left over from a same-named
// DisaggregatedSet that was deleted and recreated before garbage collection
// ran) is treated as absent (nil, nil) rather than returned for the caller to
// read or mutate as if it were this DisaggregatedSet's own.
func (manager *LeaderWorkerSetManager) Get(ctx context.Context, ds *disaggregatedsetv1.DisaggregatedSet, name string) (*leaderworkersetv1.LeaderWorkerSet, error) {
	lws := &leaderworkersetv1.LeaderWorkerSet{}
	err := manager.client.Get(ctx, types.NamespacedName{Name: name, Namespace: ds.Namespace}, lws)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("failed to get LeaderWorkerSet %s: %w", name, err)
	}
	if !metav1.IsControlledBy(lws, ds) {
		return nil, nil
	}
	return lws, nil
}

// List returns the LWS controlled by disaggregatedSet, filtered to one slice. A
// slice < 0 matches all slices. Slice 0 also matches legacy (pre-slices) LWS that
// carry no slice label, so they are reconciled as slice 0. Slice filtering is
// client-side because "slice label == 0 OR absent" cannot be expressed as a label
// selector. Results are additionally filtered by controller-owner UID (every LWS
// this manager creates is owned by its DisaggregatedSet), so an unrelated LWS that
// happens to carry matching name/role labels — e.g. hand-crafted, or left over from
// a same-named DisaggregatedSet that was deleted and recreated — cannot be
// mistaken for one of this DisaggregatedSet's own replicas.
func (manager *LeaderWorkerSetManager) List(ctx context.Context, disaggregatedSet *disaggregatedsetv1.DisaggregatedSet, slice int, role string) ([]*leaderworkersetv1.LeaderWorkerSet, error) {
	lwsObjList := &leaderworkersetv1.LeaderWorkerSetList{}

	labels := client.MatchingLabels{disaggregatedsetv1.SetNameLabelKey: disaggregatedSet.Name}
	if role != "" {
		labels[disaggregatedsetv1.RoleLabelKey] = role
	}

	if err := manager.client.List(ctx, lwsObjList, client.InNamespace(disaggregatedSet.Namespace), labels); err != nil {
		return nil, fmt.Errorf("failed to list LeaderWorkerSets for %s/%s: %w", disaggregatedSet.Namespace, disaggregatedSet.Name, err)
	}

	result := make([]*leaderworkersetv1.LeaderWorkerSet, 0, len(lwsObjList.Items))
	for i := range lwsObjList.Items {
		lws := &lwsObjList.Items[i]
		if metav1.IsControlledBy(lws, disaggregatedSet) && disaggregatedsetutils.SliceLabelMatches(lws.Labels, slice) {
			result = append(result, lws)
		}
	}
	return result, nil
}

// GetForRole returns the existing LWS for (slice, revision, role) that is
// actually controller-owned by ds, or nil if none. It looks up the
// slice-aware name and, for slice 0, falls back to the legacy (pre-slices)
// name so a legacy object is adopted in place rather than duplicated. A
// same-named LWS occupied by a foreign object is treated as absent rather
// than returned for the caller to mutate.
func (manager *LeaderWorkerSetManager) GetForRole(ctx context.Context, ds *disaggregatedsetv1.DisaggregatedSet, slice int, revision, role string) (*leaderworkersetv1.LeaderWorkerSet, error) {
	lws, err := manager.Get(ctx, ds, disaggregatedsetutils.GenerateName(ds.Name, slice, revision, role))
	if err != nil {
		return nil, err
	}
	if lws != nil || slice != 0 {
		return lws, nil
	}
	return manager.Get(ctx, ds, disaggregatedsetutils.GenerateLegacyName(ds.Name, revision, role))
}

// deleteInForeground deletes the LWS so Kubernetes removes its children — including
// the private Service — before the LWS itself. The UID precondition keeps a same-named
// replacement created since the caller read this object from being deleted instead.
func (manager *LeaderWorkerSetManager) deleteInForeground(ctx context.Context, leaderWorkerSet *leaderworkersetv1.LeaderWorkerSet) error {
	if err := manager.client.Delete(ctx, leaderWorkerSet,
		client.PropagationPolicy(metav1.DeletePropagationForeground),
		client.Preconditions{UID: ptr.To(leaderWorkerSet.UID)},
	); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("failed to delete LeaderWorkerSet %s: %w", leaderWorkerSet.Name, err)
	}

	return nil
}

func getLWSReplicas(leaderWorkerSet *leaderworkersetv1.LeaderWorkerSet) int32 {
	if leaderWorkerSet.Spec.Replicas == nil {
		return 1
	}
	return *leaderWorkerSet.Spec.Replicas
}

// GetRevisionRolesList fetches all LWS for a DisaggregatedSet, splits them into
// old (non-target) and new (target revision), and groups each set by revision.
// Returns: (oldRevisions, newRevision, error). newRevision is nil if no LWS
// exist for the target revision yet.
func (manager *LeaderWorkerSetManager) GetRevisionRolesList(
	ctx context.Context,
	disaggregatedSet *disaggregatedsetv1.DisaggregatedSet, slice int, revision string,
) (disaggregatedsetutils.RevisionRolesList, *disaggregatedsetutils.RevisionRoles, error) {
	lwsList, err := manager.List(ctx, disaggregatedSet, slice, "")
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

	// The target revision should have at most one RevisionRoles entry (one LWS
	// per role grouped together). Take index 0 since GroupByRevision returns
	// one entry per unique revision hash, and all newLWS share the same revision.
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

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

func (manager *LeaderWorkerSetManager) Create(
	ctx context.Context,
	disaggregatedSet *disaggregatedsetv1.DisaggregatedSet,
	role *disaggregatedsetv1.DisaggregatedRoleSpec,
	slice int,
	startingReplicas, initialReplicas int,
) error {
	revision := disaggregatedsetutils.ComputeRevision(disaggregatedSet.Spec.Roles)
	lwsName := disaggregatedsetutils.GenerateName(disaggregatedSet.Name, slice, revision, role.Name)
	labels := disaggregatedsetutils.GenerateLabels(disaggregatedSet.Name, slice, revision, role.Name)
	replicas := int32(startingReplicas)

	// Copy the spec and override replicas.
	lwsSpec := role.Spec
	lwsSpec.Replicas = &replicas

	// Inject system labels (role, name, revision, slice) into pod templates.
	// These don't come from the user's spec — they identify the pod's place in the
	// set for placement affinity, status, and client-side Pod discovery.
	lwsSpec.LeaderWorkerTemplate.WorkerTemplate.Labels = mergeLabels(role.Spec.LeaderWorkerTemplate.WorkerTemplate.Labels, labels)
	// Defensive copy: struct copy is shallow, so maps are shared with the original config.
	lwsSpec.LeaderWorkerTemplate.WorkerTemplate.Annotations = copyAnnotations(role.Spec.LeaderWorkerTemplate.WorkerTemplate.Annotations)
	// Inject placement affinity (no-op when no policy is set). The helper deep-copies
	// any existing affinity, so the shared worker template is not mutated.
	disaggregatedsetutils.SetPlacementAffinities(&lwsSpec.LeaderWorkerTemplate.WorkerTemplate.Spec, disaggregatedSet.Name, slice, disaggregatedSet.Spec.PlacementPolicy)

	if lwsSpec.LeaderWorkerTemplate.LeaderTemplate != nil {
		lwsSpec.LeaderWorkerTemplate.LeaderTemplate = lwsSpec.LeaderWorkerTemplate.LeaderTemplate.DeepCopy()
		lwsSpec.LeaderWorkerTemplate.LeaderTemplate.Labels = mergeLabels(role.Spec.LeaderWorkerTemplate.LeaderTemplate.Labels, labels)
		lwsSpec.LeaderWorkerTemplate.LeaderTemplate.Annotations = copyAnnotations(role.Spec.LeaderWorkerTemplate.LeaderTemplate.Annotations)
		disaggregatedsetutils.SetPlacementAffinities(&lwsSpec.LeaderWorkerTemplate.LeaderTemplate.Spec, disaggregatedSet.Name, slice, disaggregatedSet.Spec.PlacementPolicy)
	}

	leaderWorkerSet := &leaderworkersetv1.LeaderWorkerSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:        lwsName,
			Namespace:   disaggregatedSet.Namespace,
			Labels:      mergeLabels(role.ObjectMeta.Labels, labels),
			Annotations: copyAnnotations(role.ObjectMeta.Annotations),
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: disaggregatedsetv1.GroupVersion.String(),
				Kind:       "DisaggregatedSet",
				Name:       disaggregatedSet.Name,
				UID:        disaggregatedSet.UID,
				Controller: ptr.To(true),
			}},
		},
		Spec: lwsSpec,
	}
	// startingReplicas is the initial Spec value. initialReplicas is the revision's
	// intended size, which may be larger when a rollout starts the LWS at zero.
	setInitialReplicasAnnotation(leaderWorkerSet, initialReplicas)

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
		if getErr := manager.client.Get(ctx, types.NamespacedName{Name: lwsName, Namespace: disaggregatedSet.Namespace}, existing); getErr != nil {
			return fmt.Errorf("failed to get existing LeaderWorkerSet %s after create conflict: %w", lwsName, getErr)
		}
		if !metav1.IsControlledBy(existing, disaggregatedSet) {
			return fmt.Errorf("LeaderWorkerSet %s exists but is not controlled by DisaggregatedSet %s; refusing to adopt it", lwsName, disaggregatedSet.Name)
		}
		return nil
	}

	log := logf.FromContext(ctx)
	log.Info("Created LWS", "name", lwsName, "role", role.Name, "revision", revision, "replicas", startingReplicas)
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

// SyncGroupReplacementPolicy patches leaderWorkerSet so its
// groupReplacementPolicy matches the role's desired policy. The policy is a
// live knob on the LWS (it is not part of the LWS revision and changing it
// does not roll pods), so the DisaggregatedSet syncs it in place instead of
// bumping its own revision. An empty desired value means the API default,
// PostTermination. leaderWorkerSet must already be known to be owned by ds.
func (manager *LeaderWorkerSetManager) SyncGroupReplacementPolicy(ctx context.Context, leaderWorkerSet *leaderworkersetv1.LeaderWorkerSet, desired leaderworkersetv1.GroupReplacementPolicyType) error {
	if desired == "" {
		desired = leaderworkersetv1.GroupReplacementPostTermination
	}
	current := leaderWorkerSet.Spec.GroupReplacementPolicy
	if current == "" {
		current = leaderworkersetv1.GroupReplacementPostTermination
	}
	if current == desired {
		return nil
	}

	patch := client.MergeFrom(leaderWorkerSet.DeepCopy())
	leaderWorkerSet.Spec.GroupReplacementPolicy = desired
	if err := manager.client.Patch(ctx, leaderWorkerSet, patch); err != nil {
		return fmt.Errorf("failed to set groupReplacementPolicy on LeaderWorkerSet %s: %w", leaderWorkerSet.Name, err)
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

// ListForSlice returns the LWS controlled by disaggregatedSet for one slice.
func (manager *LeaderWorkerSetManager) ListForSlice(ctx context.Context, disaggregatedSet *disaggregatedsetv1.DisaggregatedSet, slice int, role string) ([]*leaderworkersetv1.LeaderWorkerSet, error) {
	return manager.list(ctx, disaggregatedSet, role, client.MatchingLabels{
		disaggregatedsetv1.SliceLabelKey: strconv.Itoa(slice),
	})
}

// ListAll returns the LWS controlled by disaggregatedSet across all slices.
func (manager *LeaderWorkerSetManager) ListAll(ctx context.Context, disaggregatedSet *disaggregatedsetv1.DisaggregatedSet, role string) ([]*leaderworkersetv1.LeaderWorkerSet, error) {
	return manager.list(ctx, disaggregatedSet, role, client.HasLabels{
		disaggregatedsetv1.SliceLabelKey,
	})
}

// list additionally filters by controller-owner UID, so an unrelated LWS that
// happens to carry matching name/role labels — e.g. hand-crafted, or left over
// from a same-named DisaggregatedSet that was deleted and recreated — cannot be
// mistaken for one of this DisaggregatedSet's own replicas.
func (manager *LeaderWorkerSetManager) list(ctx context.Context, disaggregatedSet *disaggregatedsetv1.DisaggregatedSet, role string, options ...client.ListOption) ([]*leaderworkersetv1.LeaderWorkerSet, error) {
	lwsObjList := &leaderworkersetv1.LeaderWorkerSetList{}

	labels := client.MatchingLabels{disaggregatedsetv1.SetNameLabelKey: disaggregatedSet.Name}
	if role != "" {
		labels[disaggregatedsetv1.RoleLabelKey] = role
	}

	listOptions := []client.ListOption{client.InNamespace(disaggregatedSet.Namespace), labels}
	listOptions = append(listOptions, options...)
	if err := manager.client.List(ctx, lwsObjList, listOptions...); err != nil {
		return nil, fmt.Errorf("failed to list LeaderWorkerSets for %s/%s: %w", disaggregatedSet.Namespace, disaggregatedSet.Name, err)
	}

	result := make([]*leaderworkersetv1.LeaderWorkerSet, 0, len(lwsObjList.Items))
	for i := range lwsObjList.Items {
		lws := &lwsObjList.Items[i]
		if metav1.IsControlledBy(lws, disaggregatedSet) {
			result = append(result, lws)
		}
	}
	return result, nil
}

// GetForRole returns the existing LWS for (slice, revision, role) that is
// actually controller-owned by ds, or nil if none. A same-named LWS occupied
// by a foreign object is treated as absent rather than returned for the caller
// to mutate.
func (manager *LeaderWorkerSetManager) GetForRole(ctx context.Context, ds *disaggregatedsetv1.DisaggregatedSet, slice int, revision, role string) (*leaderworkersetv1.LeaderWorkerSet, error) {
	return manager.Get(ctx, ds, disaggregatedsetutils.GenerateName(ds.Name, slice, revision, role))
}

// deleteInForeground deletes the LWS so Kubernetes removes its children — the
// StatefulSets and Services the LeaderWorkerSet controller owns — before the LWS
// itself. The UID precondition keeps a same-named replacement created since the
// caller read this object from being deleted instead.
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
	lwsList, err := manager.ListForSlice(ctx, disaggregatedSet, slice, "")
	if err != nil {
		return nil, nil, fmt.Errorf("failed to list LWS: %w", err)
	}

	var oldLWS []*leaderworkersetv1.LeaderWorkerSet
	var newLWS []*leaderworkersetv1.LeaderWorkerSet
	for _, lws := range lwsList {
		if lws.Labels[disaggregatedsetv1.RevisionLabelKey] == revision {
			newLWS = append(newLWS, lws)
			continue
		}
		// Deletion is irreversible. A terminating old LWS must not keep the
		// rollout path active or be treated as capacity the planner can control.
		if !lws.DeletionTimestamp.IsZero() {
			continue
		}
		oldLWS = append(oldLWS, lws)
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
	value, ok := disaggregatedsetutils.GetInitialReplicas(leaderWorkerSet)
	if !ok {
		return nil
	}
	parsed := int(value)
	return &parsed
}

func setInitialReplicasAnnotation(leaderWorkerSet *leaderworkersetv1.LeaderWorkerSet, replicas int) {
	if leaderWorkerSet.Annotations == nil {
		leaderWorkerSet.Annotations = make(map[string]string)
	}
	leaderWorkerSet.Annotations[disaggregatedsetv1.InitialReplicasAnnotationKey] = strconv.Itoa(replicas)
}

// UpdateInitialReplicas persists the initial-replicas annotation and keeps the
// supplied LWS object synchronized for the remainder of the reconciliation.
func (manager *LeaderWorkerSetManager) UpdateInitialReplicas(
	ctx context.Context,
	ds *disaggregatedsetv1.DisaggregatedSet,
	leaderWorkerSet *leaderworkersetv1.LeaderWorkerSet,
	replicas int,
) error {
	current := &leaderworkersetv1.LeaderWorkerSet{}
	key := types.NamespacedName{Name: leaderWorkerSet.Name, Namespace: ds.Namespace}
	if err := manager.client.Get(ctx, key, current); err != nil {
		return fmt.Errorf("failed to get LeaderWorkerSet %s: %w", leaderWorkerSet.Name, err)
	}
	if !metav1.IsControlledBy(current, ds) {
		return fmt.Errorf("LeaderWorkerSet %s exists but is not controlled by DisaggregatedSet %s; refusing to update initial replicas", current.Name, ds.Name)
	}

	currentValue := parseInitialReplicasAnnotation(current)
	if currentValue == nil || *currentValue != replicas {
		patch := client.MergeFrom(current.DeepCopy())
		setInitialReplicasAnnotation(current, replicas)
		if err := manager.client.Patch(ctx, current, patch); err != nil {
			return fmt.Errorf("failed to update initial-replicas annotation on %s: %w", current.Name, err)
		}
	}

	setInitialReplicasAnnotation(leaderWorkerSet, replicas)
	return nil
}

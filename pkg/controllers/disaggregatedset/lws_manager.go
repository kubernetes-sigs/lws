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
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/google/uuid"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	leaderworkersetv1 "sigs.k8s.io/lws/api/leaderworkerset/v1"

	disaggregatedsetv1 "sigs.k8s.io/lws/api/disaggregatedset/v1"
	"sigs.k8s.io/lws/pkg/replicagroups"
	disaggregatedsetutils "sigs.k8s.io/lws/pkg/utils/disaggregatedset"
)

type LeaderWorkerSetManager struct {
	client           client.Client
	reader           client.Reader
	pending          *pendingWrites
	observeReadiness func(context.Context, []*leaderworkersetv1.LeaderWorkerSet) (rolloutReadiness, error)
}

func NewLeaderWorkerSetManager(c client.Client) *LeaderWorkerSetManager {
	manager := &LeaderWorkerSetManager{client: c, reader: c}
	manager.observeReadiness = manager.observeCachedReadiness
	return manager
}

var errReplicaGroupsPending = errors.New("replica-group observation no longer matches the rollout")

type replicaReadiness struct {
	spec, initial int // Issued and frozen baseline counts for a child role.
	raw           int // Ready groups, including groups committed to removal.
	committed     int // Ready groups that can authorize another drain.
	// Positive evidence only: missing or incomplete observations stay false.
	allUnscheduled bool
}

// Child keys use LWS-name/child; ordinary keys retain the physical LWS name.
type rolloutReadiness map[string]replicaReadiness

// Check each physical workload before assigning children or planning a slice.
func (manager *LeaderWorkerSetManager) observeRolloutReadiness(ctx context.Context, old disaggregatedsetutils.RevisionRolesList, target disaggregatedsetutils.RevisionRoles) (rolloutReadiness, error) {
	var workloads []*leaderworkersetv1.LeaderWorkerSet
	for _, revision := range append(slices.Clone(old), target) {
		for _, lws := range revision.Roles {
			workloads = append(workloads, lws)
		}
	}
	return manager.observeReadiness(ctx, workloads)
}

func (readiness rolloutReadiness) role(revision disaggregatedsetutils.RevisionRoles, name string) replicaReadiness {
	parent, child := roleParts(name)
	lws := revision.Roles[parent]
	if lws == nil || (child != "") != (lws.Annotations[subRoleReplicasAnnotation] != "") {
		return replicaReadiness{}
	}
	observed := readiness[childRoleKey(lws.Name, child)]
	if child == "" {
		observed.spec = int(getLWSReplicas(lws))
		observed.initial = max(observed.spec, revision.GetInitialReplicasPerRole(parent))
	}
	return observed
}

func (readiness rolloutReadiness) totalReplicas(revisions disaggregatedsetutils.RevisionRolesList, name string) int {
	total := 0
	for _, revision := range revisions {
		total += readiness.role(revision, name).spec
	}
	return total
}

// collectRolloutObservations visits physical LWS objects once for readiness.
// The metadata prepass resumes accepted assignments before changing any role's
// membership or baseline. Physical revisions never acquire synthetic children.
func (manager *LeaderWorkerSetManager) collectRolloutObservations(
	ctx context.Context,
	ds *disaggregatedsetv1.DisaggregatedSet,
	old disaggregatedsetutils.RevisionRolesList,
	target disaggregatedsetutils.RevisionRoles,
	desired map[string]int,
) (rolloutReadiness, error) {
	revisions := append(slices.Clone(old), target)
	pending := false
	for _, revision := range revisions {
		for _, lws := range revision.Roles {
			if lws.Annotations[subRolePlanAnnotation] != "" {
				if _, err := manager.syncSubRoles(ctx, ds, lws, subRoleUpdate{}); err != nil && !errors.Is(err, errReplicaGroupsPending) {
					return nil, err
				}
				pending = true
			}
		}
	}
	if pending {
		return nil, errReplicaGroupsPending
	}
	configs, result := disaggregatedsetutils.GetRoleConfigs(ds), make(rolloutReadiness)
	var ordinary []*leaderworkersetv1.LeaderWorkerSet
	for i, revision := range revisions {
		for name, lws := range revision.Roles {
			// Only the appended target follows desired membership. Old revisions
			// and removed parents keep their recorded layout while draining.
			var counts map[string]int
			if config := configs[name]; i == len(old) && config != nil {
				counts = desiredSubRoles(config, desired)
			}
			state, err := manager.syncSubRoles(ctx, ds, lws, subRoleUpdate{membership: counts})
			if err != nil {
				return nil, err
			}
			if state != nil {
				revision.Roles[name] = state.snapshot.LWS
				observeSubRoleReadiness(state, result)
				continue
			}
			ordinary = append(ordinary, lws)
		}
	}
	observed, err := manager.observeReadiness(ctx, ordinary)
	if err != nil {
		return nil, err
	}
	maps.Copy(result, observed)
	return result, nil
}

func observeSubRoleReadiness(state *subRoleState, observed rolloutReadiness) {
	s, counts, initial := state.snapshot, state.issued, state.initial
	// Preserve baseline-only names as well as issued children. Only Ready is
	// masked: Availability must still see every potential physical deletion victim.
	children := make(map[string]int, len(initial)+len(counts))
	maps.Copy(children, initial)
	maps.Copy(children, counts)
	view := *s
	view.Groups = slices.Clone(s.Groups)
	raw := 0
	for child := range children {
		var groups []replicagroups.Group
		for i, group := range s.Groups {
			assigned, coherent := groupSubRole(group)
			view.Groups[i].Ready = group.Ready && coherent && assigned == child
			if coherent && assigned == child {
				groups = append(groups, group)
			}
		}
		availability := view.Availability()
		raw += int(availability.ReadyReplicas)
		observed[childRoleKey(s.LWS.Name, child)] = replicaReadiness{
			spec: counts[child], initial: max(counts[child], initial[child]),
			raw: int(availability.ReadyReplicas), committed: int(availability.RetainedReadyReplicas),
			allUnscheduled: allGroupsUnscheduled(groups, counts[child]),
		}
	}
	observed[s.LWS.Name] = replicaReadiness{raw: raw}
}

// observeCachedReadiness adapts shared group observations to readiness and scheduling facts.
// sets come from the same informer as discovery. The controller waits for write
// receipts and native scale acknowledgement before planning another step.
// Unknown observations must retry, not look like zero serving capacity: that
// could lower the planner's no-worsening floor.
func (manager *LeaderWorkerSetManager) observeCachedReadiness(ctx context.Context, sets []*leaderworkersetv1.LeaderWorkerSet) (rolloutReadiness, error) {
	result := make(rolloutReadiness, len(sets))
	if len(sets) == 0 {
		return result, nil
	}
	for _, lws := range sets {
		snapshot, err := replicagroups.Observe(ctx, manager.reader, lws)
		if err != nil {
			return nil, err
		}
		if snapshot == nil || snapshot.LWS.Generation != lws.Generation ||
			!lws.DeletionTimestamp.IsZero() || !snapshot.LWS.DeletionTimestamp.IsZero() {
			return nil, fmt.Errorf("LeaderWorkerSet %s: %w", lws.Name, errReplicaGroupsPending)
		}
		if manager.pending != nil && !snapshot.DeletionsObserved() {
			return nil, fmt.Errorf("waiting for native scaling of %s: %w", lws.Name, errReplicaGroupsPending)
		}
		availability := snapshot.Availability()
		result[lws.Name] = replicaReadiness{
			raw:            int(availability.ReadyReplicas),
			committed:      int(availability.RetainedReadyReplicas),
			allUnscheduled: allGroupsUnscheduled(snapshot.Groups, int(getLWSReplicas(snapshot.LWS))),
		}
	}
	return result, nil
}

// allGroupsUnscheduled permits cancelling below the final target only when no
// group has begun using a node. Missing groups or workers are unknown, not
// unscheduled; an assigned worker protects the role even if its leader is unbound.
// These are facts from the existing observation, not a scheduling reservation.
func allGroupsUnscheduled(groups []replicagroups.Group, replicas int) bool {
	if replicas == 0 || len(groups) != replicas {
		return false
	}
	for _, group := range groups {
		// The observer already validates group ownership and the size annotation.
		size, _ := strconv.Atoi(group.Leader.Annotations[leaderworkersetv1.SizeAnnotationKey])
		if group.Terminating || len(group.Pods) != size {
			return false
		}
		for _, pod := range group.Pods {
			if pod.Spec.NodeName != "" || pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
				return false
			}
		}
	}
	return true
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
	revision string,
	initialReplicas int,
	initialSubRoles map[string]int,
) error {
	// The reconciler selects the hash generation. Recomputing here would assign
	// current-version names and labels to children of a legacy DisaggregatedSet.
	lwsName := disaggregatedsetutils.GenerateName(disaggregatedSet.Name, slice, revision, role.Name)
	labels := disaggregatedsetutils.GenerateLabels(disaggregatedSet.Name, slice, revision, role.Name)
	// Create never starts Pods: an ambiguous request may commit after the user
	// changes the desired revision. Growth is a separate conditional update,
	// selected from the current plan after the informer observes this object.
	replicas := int32(0)

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
	// Preserve the revision's intended size even though it starts empty.
	setInitialReplicasAnnotation(leaderWorkerSet, initialReplicas)
	if len(initialSubRoles) > 0 {
		setSubRoleJSON(leaderWorkerSet, disaggregatedsetv1.InitialSubRoleReplicasAnnotationKey, initialSubRoles)
	}
	leaderWorkerSet.Annotations[observationReceiptAnnotation] = unscaledReceiptPrefix + uuid.NewString()

	// No receipt expectation or captured retry is needed for an empty LWS. A delayed
	// creation cannot spend a rollout budget; the next reconcile follows the
	// latest desired revision, including if this object has already disappeared.
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
		if getErr := manager.reader.Get(ctx, types.NamespacedName{Name: lwsName, Namespace: disaggregatedSet.Namespace}, existing); getErr != nil {
			if apierrors.IsNotFound(getErr) {
				return errReplicaGroupsPending
			}
			return fmt.Errorf("failed to get existing LeaderWorkerSet %s after create conflict: %w", lwsName, getErr)
		}
		if !metav1.IsControlledBy(existing, disaggregatedSet) {
			return fmt.Errorf("LeaderWorkerSet %s exists but is not controlled by DisaggregatedSet %s; refusing to adopt it", lwsName, disaggregatedSet.Name)
		}
		return nil
	}
	if manager.pending != nil && leaderWorkerSet.UID != "" {
		// This successful creation supplied a new UID: no previous leader
		// could have left a conditional request against it. Skip takeover,
		// but growth must still use the object read from the informer.
		manager.pending.mu.Lock()
		if manager.pending.initialized == nil {
			manager.pending.initialized = make(map[types.UID]client.ObjectKey)
		}
		manager.pending.initialized[leaderWorkerSet.UID] = client.ObjectKeyFromObject(disaggregatedSet)
		manager.pending.mu.Unlock()
	}

	log := logf.FromContext(ctx)
	log.Info("Created empty LWS", "name", lwsName, "role", role.Name, "revision", revision)
	return nil
}

// Scale reads only the informer. The resource-version precondition rejects an
// obsolete write at the API server, including a status-only conflict. Retrying
// from a later observation is preferable to refreshing through a live GET.
func (manager *LeaderWorkerSetManager) Scale(ctx context.Context, ds *disaggregatedsetv1.DisaggregatedSet, observed *leaderworkersetv1.LeaderWorkerSet, replicas int) error {
	return manager.scale(ctx, ds, observed, replicas, nil)
}

func (manager *LeaderWorkerSetManager) scale(ctx context.Context, ds *disaggregatedsetv1.DisaggregatedSet, observed *leaderworkersetv1.LeaderWorkerSet, replicas int, strategy *leaderworkersetv1.RolloutStrategy) error {
	if strategy != nil && int(getLWSReplicas(observed)) == replicas {
		candidate := observed.DeepCopy()
		if !setRolloutStrategy(candidate, strategy) {
			return nil // A steady-state no-op needs no further reads or writes.
		}
	}
	name := observed.Name
	leaderWorkerSet := &leaderworkersetv1.LeaderWorkerSet{}
	if err := manager.reader.Get(ctx, types.NamespacedName{Name: name, Namespace: ds.Namespace}, leaderWorkerSet); err != nil {
		return fmt.Errorf("failed to get LeaderWorkerSet %s for scaling: %w", name, err)
	}
	if !metav1.IsControlledBy(leaderWorkerSet, ds) {
		return fmt.Errorf("LeaderWorkerSet %s exists but is not controlled by DisaggregatedSet %s; refusing to scale it", name, ds.Name)
	}
	if leaderWorkerSet.UID != observed.UID || leaderWorkerSet.Generation != observed.Generation ||
		getLWSReplicas(leaderWorkerSet) != getLWSReplicas(observed) || !leaderWorkerSet.DeletionTimestamp.IsZero() ||
		leaderWorkerSet.Annotations[subRoleReplicasAnnotation] != "" {
		return apierrors.NewConflict(leaderworkersetv1.GroupVersion.WithResource("leaderworkersets").GroupResource(), name,
			errors.New("LeaderWorkerSet changed since scale observation"))
	}

	before := leaderWorkerSet.DeepCopy()
	leaderWorkerSet.Spec.Replicas = ptr.To(int32(replicas))
	setRolloutStrategy(leaderWorkerSet, strategy)
	if equality.Semantic.DeepEqual(before.Spec, leaderWorkerSet.Spec) {
		return nil
	}
	patch := client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})
	if err := manager.mutate(ctx, leaderWorkerSet, false, func(ctx context.Context) error {
		return manager.client.Patch(ctx, leaderWorkerSet, patch)
	}); err != nil {
		return fmt.Errorf("failed to scale LeaderWorkerSet %s: %w", name, err)
	}

	return nil
}

// The current revision's budgets are live settings, like replicas. Publish them
// together: a zero/zero budget is valid at zero replicas, but prevents resuming.
// Subrole scaling can take several writes; defer a budget that is only valid at
// the final count (for example, maxUnavailable=10% when growing from 1 to 10).
func setRolloutStrategy(lws *leaderworkersetv1.LeaderWorkerSet, desired *leaderworkersetv1.RolloutStrategy) bool {
	if desired == nil {
		return false // Preserve old revisions' recorded native settings.
	}
	strategy := defaultedRolloutStrategy(desired)
	if equality.Semantic.DeepEqual(defaultedRolloutStrategy(&lws.Spec.RolloutStrategy), strategy) {
		return false
	}
	config := strategy.RollingUpdateConfiguration
	surge, surgeErr := intstr.GetScaledValueFromIntOrPercent(&config.MaxSurge, int(getLWSReplicas(lws)), true)
	unavailable, unavailableErr := intstr.GetScaledValueFromIntOrPercent(&config.MaxUnavailable, int(getLWSReplicas(lws)), false)
	if surgeErr != nil || unavailableErr != nil || getLWSReplicas(lws) != 0 && surge == 0 && unavailable == 0 {
		return false
	}
	lws.Spec.RolloutStrategy = *strategy
	return true
}

func defaultedRolloutStrategy(desired *leaderworkersetv1.RolloutStrategy) *leaderworkersetv1.RolloutStrategy {
	strategy := desired.DeepCopy()
	if strategy.Type == "" {
		strategy.Type = leaderworkersetv1.RollingUpdateStrategyType
	}
	if strategy.RollingUpdateConfiguration == nil {
		strategy.RollingUpdateConfiguration = &leaderworkersetv1.RollingUpdateConfiguration{MaxUnavailable: intstr.FromInt(1)}
	}
	// Match the API default so omitted partition does not cause repeated patches.
	config := strategy.RollingUpdateConfiguration
	if config.Partition == nil {
		config.Partition = ptr.To[int32](0)
	}
	return strategy
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

	// Reject stale observations, including a same-name replacement owned by someone else.
	patch := client.MergeFromWithOptions(leaderWorkerSet.DeepCopy(), client.MergeFromWithOptimisticLock{})
	leaderWorkerSet.Spec.GroupReplacementPolicy = desired
	if err := manager.mutate(ctx, leaderWorkerSet, false, func(ctx context.Context) error {
		return manager.client.Patch(ctx, leaderWorkerSet, patch)
	}); err != nil {
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
	err := manager.reader.Get(ctx, types.NamespacedName{Name: name, Namespace: ds.Namespace}, lws)
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

// listPods returns Pods belonging to one LeaderWorkerSet.
func (manager *LeaderWorkerSetManager) listPods(ctx context.Context, lws *leaderworkersetv1.LeaderWorkerSet) ([]corev1.Pod, error) {
	pods := &corev1.PodList{}
	if err := manager.reader.List(ctx, pods,
		client.InNamespace(lws.Namespace),
		client.MatchingLabels{leaderworkersetv1.SetNameLabelKey: lws.Name},
	); err != nil {
		return nil, fmt.Errorf("failed to list Pods for LeaderWorkerSet %s: %w", lws.Name, err)
	}
	return pods.Items, nil
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
	if err := manager.reader.List(ctx, lwsObjList, listOptions...); err != nil {
		return nil, fmt.Errorf("failed to list LeaderWorkerSets for %s/%s: %w", disaggregatedSet.Namespace, disaggregatedSet.Name, err)
	}

	result := make([]*leaderworkersetv1.LeaderWorkerSet, 0, len(lwsObjList.Items))
	for i := range lwsObjList.Items {
		lws := &lwsObjList.Items[i]
		if metav1.IsControlledBy(lws, disaggregatedSet) {
			result = append(result, lws)
		}
	}
	if err := validateUniqueRolloutRoles(disaggregatedSet, result); err != nil {
		return nil, err
	}
	return result, nil
}

// validateUniqueRolloutRoles checks owned LWS before grouping, so planning and
// cleanup cannot silently discard capacity from a duplicate role.
func validateUniqueRolloutRoles(ds *disaggregatedsetv1.DisaggregatedSet, ownedLWS []*leaderworkersetv1.LeaderWorkerSet) error {
	type roleKey struct{ slice, revision, role string }
	seen := make(map[roleKey]string, len(ownedLWS))
	for _, lws := range ownedLWS {
		key := roleKey{
			slice:    lws.Labels[disaggregatedsetv1.SliceLabelKey],
			revision: lws.Labels[disaggregatedsetv1.RevisionLabelKey],
			role:     lws.Labels[disaggregatedsetv1.RoleLabelKey],
		}
		if previous, exists := seen[key]; exists {
			return fmt.Errorf("DisaggregatedSet %s/%s: LeaderWorkerSets %q and %q claim the same slice %q, revision %q, role %q",
				ds.Namespace, ds.Name, previous, lws.Name, key.slice, key.revision, key.role)
		}
		seen[key] = lws.Name
	}
	return nil
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
	if err := manager.mutate(ctx, leaderWorkerSet, true, func(ctx context.Context) error {
		return manager.client.Delete(ctx, leaderWorkerSet,
			client.PropagationPolicy(metav1.DeletePropagationForeground),
			client.Preconditions{UID: ptr.To(leaderWorkerSet.UID), ResourceVersion: ptr.To(leaderWorkerSet.ResourceVersion)},
		)
	}); err != nil {
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

// retainedRolloutRoleNames includes current roles and every role whose budget
// must survive while old workloads drain, including removed children.
func (m *LeaderWorkerSetManager) retainedRolloutRoleNames(ctx context.Context, ds *disaggregatedsetv1.DisaggregatedSet) ([]string, error) {
	// Keep history while writes are outstanding; stale absence must not prune
	// a role or child that still has capacity in an older revision.
	roles := sets.New(rolloutRoleNames(ds)...)
	if m.pending != nil {
		if err := m.pending.observed(ctx, ds.UID, ""); err != nil {
			if !errors.Is(err, errReplicaGroupsPending) {
				return nil, err
			}
			for role := range rememberedMaxUnavailable(ds) {
				roles.Insert(role)
			}
		}
	}
	workloads, err := m.list(ctx, ds, "")
	if err != nil {
		return nil, err
	}
	for _, lws := range workloads {
		parent := lws.Labels[disaggregatedsetv1.RoleLabelKey]
		if lws.Annotations[subRoleReplicasAnnotation] == "" && lws.Annotations[disaggregatedsetv1.InitialSubRoleReplicasAnnotationKey] == "" {
			roles.Insert(parent)
			continue
		}
		for _, key := range []string{subRoleReplicasAnnotation, disaggregatedsetv1.InitialSubRoleReplicasAnnotationKey} {
			counts, err := subRoleCounts(lws, key)
			if err != nil {
				return nil, err
			}
			for child := range counts {
				roles.Insert(childRoleKey(parent, child))
			}
		}
	}
	return sets.List(roles), nil
}

// GetRevisionRolesList fetches all LWS for a DisaggregatedSet, splits them into
// old (non-target) and new (target revision), and groups each set by revision.
// Returns: (oldRevisions, newRevision, error). newRevision is nil if no LWS
// exist for the target revision yet.
func (manager *LeaderWorkerSetManager) GetRevisionRolesList(
	ctx context.Context,
	disaggregatedSet *disaggregatedsetv1.DisaggregatedSet, slice int, revision string,
) (disaggregatedsetutils.RevisionRolesList, *disaggregatedsetutils.RevisionRoles, error) {
	// The per-slice write barrier prevents an older Spec from spending the
	// previous step's capacity twice while the informer catches up.
	lwsList, err := manager.list(ctx, disaggregatedSet, "", client.MatchingLabels{
		disaggregatedsetv1.SliceLabelKey: strconv.Itoa(slice),
	})
	if err != nil {
		return nil, nil, fmt.Errorf("failed to list LWS: %w", err)
	}

	var oldLWS []*leaderworkersetv1.LeaderWorkerSet
	var newLWS []*leaderworkersetv1.LeaderWorkerSet
	for _, lws := range lwsList {
		// Deletion is irreversible. A terminating LWS is neither controllable old
		// capacity nor a usable target. If a target is terminating, reconciliation
		// waits until its deterministic name can be created again.
		if !lws.DeletionTimestamp.IsZero() {
			continue
		}
		if lws.Labels[disaggregatedsetv1.RevisionLabelKey] == revision {
			newLWS = append(newLWS, lws)
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
	if err := manager.reader.Get(ctx, key, current); err != nil {
		return fmt.Errorf("failed to get LeaderWorkerSet %s: %w", leaderWorkerSet.Name, err)
	}
	if !metav1.IsControlledBy(current, ds) {
		return fmt.Errorf("LeaderWorkerSet %s exists but is not controlled by DisaggregatedSet %s; refusing to update initial replicas", current.Name, ds.Name)
	}
	if current.Annotations[subRoleReplicasAnnotation] != "" {
		return errReplicaGroupsPending
	}

	currentValue := parseInitialReplicasAnnotation(current)
	if currentValue == nil || *currentValue != replicas {
		patch := client.MergeFromWithOptions(current.DeepCopy(), client.MergeFromWithOptimisticLock{})
		setInitialReplicasAnnotation(current, replicas)
		if err := manager.mutate(ctx, current, false, func(ctx context.Context) error {
			return manager.client.Patch(ctx, current, patch)
		}); err != nil {
			return fmt.Errorf("failed to update initial-replicas annotation on %s: %w", current.Name, err)
		}
		if manager.pending != nil {
			if err := manager.pending.observed(ctx, ds.UID, current.Labels[disaggregatedsetv1.SliceLabelKey]); err != nil {
				return err
			}
		}
	}

	current.DeepCopyInto(leaderWorkerSet)
	return nil
}

func (manager *LeaderWorkerSetManager) mutate(ctx context.Context, lws *leaderworkersetv1.LeaderWorkerSet, deleting bool, apply func(context.Context) error) error {
	if getLWSReplicas(lws) > 0 && strings.HasPrefix(lws.Annotations[observationReceiptAnnotation], unscaledReceiptPrefix) {
		// End bootstrap in the same conditional write that first starts Pods,
		// including callers without receipt tracking (e.g. unit-test clients).
		delete(lws.Annotations, observationReceiptAnnotation)
	}
	if manager.pending == nil {
		return apply(ctx)
	}
	return manager.pending.mutate(ctx, lws, deleting, apply)
}

// prepareSlice invalidates conditional LWS requests left in flight by a previous
// process. A cold cache includes committed writes, but not future commits. Before
// planning, change each existing LWS's resource version with a metadata-only
// receipt and observe it. Either an old request wins first (our patch conflicts),
// or our patch wins and the old request can no longer apply.
// This protects LWS mutations, not writes to their descendants; native scaling
// must still be observed before granting availability credit.
func (manager *LeaderWorkerSetManager) prepareSlice(ctx context.Context, ds *disaggregatedsetv1.DisaggregatedSet, slice int) error {
	o := manager.pending
	if o == nil {
		return nil
	}
	if err := o.observed(ctx, ds.UID, strconv.Itoa(slice)); err != nil {
		return err
	}
	workloads, err := manager.ListForSlice(ctx, ds, slice, "")
	if err != nil {
		return err
	}
	wrote := false
	for _, lws := range workloads {
		o.mu.Lock()
		_, initialized := o.initialized[lws.UID]
		o.mu.Unlock()
		if initialized || !lws.DeletionTimestamp.IsZero() {
			continue
		}
		patch := client.MergeFromWithOptions(lws.DeepCopy(), client.MergeFromWithOptimisticLock{})
		if err := manager.mutate(ctx, lws, false, func(ctx context.Context) error {
			return manager.client.Patch(ctx, lws, patch)
		}); err != nil {
			return err
		}
		wrote = true
	}
	if wrote {
		return fmt.Errorf("waiting to observe slice takeover: %w", errReplicaGroupsPending)
	}
	return nil
}

// A receipt identifies our write without waiting to see one exact resource
// version that a watch is allowed to coalesce away.
const observationReceiptAnnotation = "disaggregatedset.x-k8s.io/observation-receipt"

// Keep the bootstrap bit in the existing receipt, not a second annotation.
// Metadata writes and takeover preserve it; the first positive scale clears it.
const unscaledReceiptPrefix = "unscaled:"

func isUnscaledCreation(lws *leaderworkersetv1.LeaderWorkerSet) bool {
	return getLWSReplicas(lws) == 0 && strings.HasPrefix(lws.Annotations[observationReceiptAnnotation], unscaledReceiptPrefix)
}

// pendingWrites remembers conditional mutations until the informer observes them.
// Every such write in a slice must be observed before its next rollout step.
// Creation is untracked because new LWS objects always start at zero replicas.
type pendingWrites struct {
	client.Reader
	mu     sync.Mutex
	writes map[client.ObjectKey]*pendingWrite
	// UIDs created here or whose writes were observed; restart repeats takeover.
	// The parent key allows cleanup when a DisaggregatedSet disappears.
	initialized map[types.UID]client.ObjectKey
}

type pendingWrite struct {
	owner   types.UID
	parent  client.ObjectKey
	slice   string
	uid     types.UID
	receipt string
	deleted bool
	// Version read from this informer and used as the mutation precondition.
	baseVersion string
	// Replaying the same conditional mutation resolves an ambiguous transport
	// error. We never turn a timeout into permission to forget a possible write.
	retry func(context.Context) error
}

func (o *pendingWrites) mutate(ctx context.Context, lws *leaderworkersetv1.LeaderWorkerSet, deleting bool, apply func(context.Context) error) error {
	owner := metav1.GetControllerOf(lws)
	if owner == nil {
		return errors.New("observed writes require a controller-owned LeaderWorkerSet")
	}
	key := client.ObjectKeyFromObject(lws)
	o.mu.Lock()
	previous, exists := o.writes[key]
	var previousWrite pendingWrite
	if exists {
		previousWrite = *previous
	}
	o.mu.Unlock()
	if exists {
		if err := o.checkWrite(ctx, key, previousWrite); err != nil {
			return err
		}
	}
	written := &pendingWrite{
		owner: owner.UID, parent: client.ObjectKey{Namespace: lws.Namespace, Name: owner.Name},
		slice: lws.Labels[disaggregatedsetv1.SliceLabelKey], uid: lws.UID,
		deleted: deleting, baseVersion: lws.ResourceVersion,
	}
	if !deleting {
		if lws.Annotations == nil {
			lws.Annotations = make(map[string]string)
		}
		written.receipt = uuid.NewString()
		if isUnscaledCreation(lws) {
			written.receipt = unscaledReceiptPrefix + written.receipt
		}
		lws.Annotations[observationReceiptAnnotation] = written.receipt
	}
	// Record before the request, including when its outcome is not known.
	o.mu.Lock()
	o.writes[key] = written
	o.mu.Unlock()
	err := apply(ctx)
	o.mu.Lock()
	defer o.mu.Unlock()
	if apierrors.IsConflict(err) || apierrors.IsInvalid(err) || apierrors.IsForbidden(err) || apierrors.IsAlreadyExists(err) || apierrors.IsNotFound(err) {
		delete(o.writes, key) // This request definitively did not mutate the object.
	} else if err != nil {
		written.retry = apply
	}
	return err
}

// Once the parent has disappeared from its informer, it cannot authorize more
// rollout steps. Release its ledger entries instead of leaking them forever.
func (o *pendingWrites) forget(parent client.ObjectKey) {
	o.mu.Lock()
	defer o.mu.Unlock()
	for key, written := range o.writes {
		if written.parent == parent {
			delete(o.writes, key)
		}
	}
	for uid, owner := range o.initialized {
		if owner == parent {
			delete(o.initialized, uid)
		}
	}
}

// observed checks a whole slice, not just the next LWS to change. Otherwise a
// delayed update to A could authorize another deletion in B using A's old credit.
func (o *pendingWrites) observed(ctx context.Context, owner types.UID, slice string) error {
	o.mu.Lock()
	writes := make(map[client.ObjectKey]pendingWrite)
	for key, written := range o.writes {
		if written.owner == owner && (slice == "" || written.slice == slice) {
			writes[key] = *written
		}
	}
	o.mu.Unlock()
	for key, written := range writes {
		if err := o.checkWrite(ctx, key, written); err != nil {
			return err
		}
	}
	return nil
}

func (o *pendingWrites) checkWrite(ctx context.Context, key client.ObjectKey, written pendingWrite) error {
	lws := &leaderworkersetv1.LeaderWorkerSet{}
	err := o.Get(ctx, key, lws)
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	// An object previously read from this informer cannot disappear or change
	// UID without being deleted. Creates need no receipts: they start empty.
	seen := apierrors.IsNotFound(err) || lws.UID != written.uid || !lws.DeletionTimestamp.IsZero()
	receiptObserved := err == nil && !written.deleted && lws.DeletionTimestamp.IsZero() &&
		lws.UID == written.uid && lws.Annotations[observationReceiptAnnotation] == written.receipt
	seen = seen || receiptObserved
	// This informer supplied baseVersion. A different version of the same UID
	// means an unapplied conditional request can no longer succeed. Resolve the
	// ambiguous outcome and replan; do not replay it against a newer version.
	obsolete := written.retry != nil && written.baseVersion != "" && err == nil &&
		lws.UID == written.uid && lws.ResourceVersion != written.baseVersion
	if seen || obsolete {
		o.mu.Lock()
		if current := o.writes[key]; current != nil && current.receipt == written.receipt && current.uid == written.uid {
			delete(o.writes, key)
			if receiptObserved {
				if o.initialized == nil {
					o.initialized = make(map[types.UID]client.ObjectKey)
				}
				o.initialized[lws.UID] = written.parent
			} else if seen {
				delete(o.initialized, written.uid)
			}
		}
		o.mu.Unlock()
		return nil
	}
	if written.retry != nil {
		// A conflict may mean the first request succeeded. Retain the
		// expectation until the informer supplies evidence, not until a timer expires.
		err := written.retry(ctx)
		if err != nil && !apierrors.IsConflict(err) && !apierrors.IsAlreadyExists(err) {
			return err
		}
		if err == nil {
			o.mu.Lock()
			if current := o.writes[key]; current != nil && current.receipt == written.receipt {
				current.retry = nil // Accepted; only the watch is outstanding now.
			}
			o.mu.Unlock()
		}
	}
	return fmt.Errorf("waiting to observe write to %s: %w", key, errReplicaGroupsPending)
}

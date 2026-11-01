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
	"strconv"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	disaggregatedsetv1 "sigs.k8s.io/lws/api/disaggregatedset/v1"
	leaderworkersetv1 "sigs.k8s.io/lws/api/leaderworkerset/v1"
	"sigs.k8s.io/lws/pkg/replicagroups"
	disaggregatedsetutils "sigs.k8s.io/lws/pkg/utils/disaggregatedset"
)

// updateStatus publishes cached counts and conditions.
// Scalers use the same counts as the DisaggregatedSet's nested role status.
func (r *DisaggregatedSetReconciler) updateStatus(ctx context.Context, ds *disaggregatedsetv1.DisaggregatedSet, roleNames []string, revision string, desiredReplicasByRole map[string]int, scalers map[string]*disaggregatedsetv1.DisaggregatedSetRoleScaler) error {
	roleStatuses, assigned, err := r.LWSManager.collectRoleStatuses(ctx, ds, roleNames, revision, desiredReplicasByRole)
	if err != nil {
		return err
	}
	// Availability is judged against the published targets, so set them first.
	sliceCount := disaggregatedsetutils.GetSlices(ds)
	setDesiredReplicas(roleStatuses, desiredReplicasByRole, sliceCount)
	setMinAvailableReplicas(ds, roleStatuses, desiredReplicasByRole, sliceCount)
	observedReplicas, available := summarizeRoleStatuses(roleStatuses)

	changed := setRoleStatuses(ds, roleStatuses)
	changed = setSubRolesAssignedCondition(ds, assigned) || changed
	unresolvedRoles := unresolvedReplicaTargetRoles(roleNames, desiredReplicasByRole)
	changed = setDisaggregatedSetCondition(ds, disaggregatedSetCondition(ds, available && assigned, unresolvedRoles)) || changed
	changed = setDisaggregatedSetCondition(ds, minimumReplicasAvailableCondition(ds, roleStatuses)) || changed
	if ds.Status.ObservedGeneration != ds.Generation {
		ds.Status.ObservedGeneration = ds.Generation
		changed = true
	}

	// A failure of one status writer must not suppress the other.
	var scalerErr error
	if len(scalers) > 0 {
		scalerErr = r.ScalerManager.WriteStatus(ctx, ds, scalers, observedReplicas)
	}
	err = r.publishStatus(ctx, ds, changed)
	if errors.Is(err, errStatusPending) && scalerErr != nil {
		// A normal status retry must not hide a scaler failure.
		return scalerErr
	}
	return errors.Join(scalerErr, err)
}

var errStatusPending = errors.New("waiting for the DisaggregatedSet informer to observe a newer version")

const (
	statusVersionTTL       = time.Minute
	maxStaleStatusVersions = 4096 // Limits retry suppression, not supported DisaggregatedSets.
)

type statusVersion struct {
	uid             types.UID
	resourceVersion string
}

// publishStatus avoids repeating writes against a version known to be stale.
// Child events may enqueue us before the parent informer observes our last write.
// Keep optimistic concurrency: another writer can still win after our cache advances.
func (r *DisaggregatedSetReconciler) publishStatus(ctx context.Context, ds *disaggregatedsetv1.DisaggregatedSet, changed bool) error {
	key := client.ObjectKeyFromObject(ds)
	version := statusVersion{uid: ds.UID, resourceVersion: ds.ResourceVersion}
	versions := r.staleStatusVersions
	if stale, ok := versions.Get(key); ok {
		if stale == version {
			return errStatusPending
		}
		// Watch events may skip versions; do not wait for the exact write response.
		versions.Remove(key)
	}
	if !changed {
		return nil
	}
	err := r.Status().Update(ctx, ds)
	if apierrors.IsConflict(err) {
		versions.Add(key, version, statusVersionTTL)
		return errStatusPending
	}
	if err != nil {
		return fmt.Errorf("failed to update DisaggregatedSet status: %w", err)
	}
	if ds.ResourceVersion != version.resourceVersion {
		versions.Add(key, version, statusVersionTTL)
	}
	return nil
}

// collectRoleStatuses aggregates all slices and revisions. Only current roles
// and children are published, in spec order; removed children still count toward
// their parent's total while their old groups drain.
func (m *LeaderWorkerSetManager) collectRoleStatuses(ctx context.Context, ds *disaggregatedsetv1.DisaggregatedSet, roleNames []string, revision string, desiredReplicasByRole map[string]int) ([]disaggregatedsetv1.RoleStatus, bool, error) {
	all, err := m.ListAll(ctx, ds, "")
	if err != nil {
		return nil, false, fmt.Errorf("failed to list LWS for status: %w", err)
	}
	byRole := make(map[string][]*leaderworkersetv1.LeaderWorkerSet)
	for _, lws := range all {
		role := lws.Labels[disaggregatedsetv1.RoleLabelKey]
		byRole[role] = append(byRole[role], lws)
	}
	statuses := make([]disaggregatedsetv1.RoleStatus, 0, len(roleNames))
	configs := disaggregatedsetutils.GetRoleConfigs(ds)
	capacity := servingCapacity{}
	allAssigned := true
	for _, role := range roleNames {
		workloads := byRole[role]
		delete(byRole, role)
		status := disaggregatedsetv1.RoleStatus{Name: role}
		if config := configs[role]; config != nil && len(config.SubRoles) > 0 {
			var assigned bool
			status, assigned, err = m.subRoleStatus(ctx, config, workloads, revision, capacity)
			if err != nil {
				return nil, false, err
			}
			allAssigned = allAssigned && assigned
		} else {
			for _, lws := range workloads {
				status.Replicas += lws.Status.Replicas
				status.ReadyReplicas += lws.Status.ReadyReplicas
				if lws.Labels[disaggregatedsetv1.RevisionLabelKey] == revision {
					status.UpdatedReplicas += lws.Status.UpdatedReplicas
				}
				if err := m.observeServingLayout(ctx, capacity, lws, role, revision); err != nil {
					return nil, false, err
				}
			}
		}
		statuses = append(statuses, status)
	}
	// Removed roles publish no status, but an old revision cannot serve
	// without them.
	for role, workloads := range byRole {
		for _, lws := range workloads {
			if err := m.observeServingLayout(ctx, capacity, lws, role, revision); err != nil {
				return nil, false, err
			}
		}
	}
	capacity.requireTargets(ds, revision, desiredReplicasByRole)
	capacity.setAvailableReplicas(statuses)
	return statuses, allAssigned, nil
}

// observeServingLayout counts a workload that the current spec does not partition.
// The children of an old partitioned workload stay separately required while it
// drains. A workload of the current revision counts as one role, since dropping
// sub-roles updates it in place, and so does one without a valid issued layout: as
// in subRoleStatus, an invalid record does not withhold every other publication.
func (m *LeaderWorkerSetManager) observeServingLayout(ctx context.Context, capacity servingCapacity, lws *leaderworkersetv1.LeaderWorkerSet, role, revision string) error {
	membership, _ := subRoleCounts(lws, subRoleReplicasAnnotation)
	if lws.Labels[disaggregatedsetv1.RevisionLabelKey] == revision || membership == nil {
		capacity.observeWorkload(lws, role)
		return nil
	}
	initial, _ := subRoleCounts(lws, disaggregatedsetv1.InitialSubRoleReplicasAnnotationKey)
	s, err := replicagroups.Observe(ctx, m.reader, lws)
	if err != nil {
		return err
	}
	if s == nil {
		return errReplicaGroupsPending
	}
	serving := capacity.revision(s.LWS)
	serving.requireSubRoles(role, membership, initial)
	for _, group := range activeSubRoleGroups(s) {
		name, coherent := groupSubRole(group)
		if _, known := membership[name]; known && coherent {
			serving.observe(childRoleKey(role, name), groupReadyReplicas(group), false)
		}
	}
	return nil
}

// servingCapacity tallies Ready groups by slice and revision. As in the rollout
// planner, a revision serves only while every role it requires has a Ready
// group, so a Ready Prefill does not count without a Ready Decode alongside it.
type servingCapacity map[servingRevisionKey]*servingRevision

type servingRevisionKey struct{ slice, revision string }

type servingRevision struct {
	ready    map[string]int32 // Keyed by parent or parent/child.
	required map[string]bool
}

func (capacity servingCapacity) revision(lws *leaderworkersetv1.LeaderWorkerSet) *servingRevision {
	return capacity.revisionAt(servingRevisionKey{lws.Labels[disaggregatedsetv1.SliceLabelKey], lws.Labels[disaggregatedsetv1.RevisionLabelKey]})
}

func (capacity servingCapacity) revisionAt(key servingRevisionKey) *servingRevision {
	revision := capacity[key]
	if revision == nil {
		revision = &servingRevision{ready: map[string]int32{}, required: map[string]bool{}}
		capacity[key] = revision
	}
	return revision
}

func (revision *servingRevision) observe(role string, ready int32, required bool) {
	revision.ready[role] += ready
	revision.required[role] = revision.required[role] || required
}

// requireSubRoles makes each child a separate required role, as it is for the planner.
func (revision *servingRevision) requireSubRoles(role string, layouts ...map[string]int) {
	for _, counts := range layouts {
		for child, count := range counts {
			revision.observe(childRoleKey(role, child), 0, count > 0)
		}
	}
}

// requireTargets makes the current revision require every role with a positive
// target in each slice, so a role whose workload is missing still stops its peers
// from counting as available.
func (capacity servingCapacity) requireTargets(ds *disaggregatedsetv1.DisaggregatedSet, revision string, desiredReplicasByRole map[string]int) {
	for slice := range disaggregatedsetutils.GetSlices(ds) {
		current := capacity.revisionAt(servingRevisionKey{strconv.Itoa(int(slice)), revision})
		for _, leaf := range rolloutRoleNames(ds) {
			current.observe(leaf, 0, desiredReplicasByRole[leaf] > 0)
		}
	}
}

// observeWorkload counts an unpartitioned workload. Like the planner, a role is
// required by its revision while its intended replica count is positive.
func (capacity servingCapacity) observeWorkload(lws *leaderworkersetv1.LeaderWorkerSet, role string) {
	capacity.revision(lws).observe(role, lws.Status.ReadyReplicas, intendedReplicas(lws) > 0)
}

func intendedReplicas(lws *leaderworkersetv1.LeaderWorkerSet) int32 {
	initial, _ := disaggregatedsetutils.GetInitialReplicas(lws)
	return max(getLWSReplicas(lws), initial)
}

func (revision *servingRevision) serves() bool {
	requiresRole := false
	for role, required := range revision.required {
		if required && revision.ready[role] == 0 {
			return false
		}
		requiresRole = requiresRole || required
	}
	return requiresRole
}

// setAvailableReplicas totals serving capacity across slices and revisions. A
// parent includes groups of children removed from its spec, like its other counts.
func (capacity servingCapacity) setAvailableReplicas(statuses []disaggregatedsetv1.RoleStatus) {
	available := make(map[string]int32)
	for _, revision := range capacity {
		if !revision.serves() {
			continue
		}
		for key, ready := range revision.ready {
			parent, _ := roleParts(key)
			available[parent] += ready
			if key != parent {
				available[key] += ready
			}
		}
	}
	for i := range statuses {
		parent := &statuses[i]
		// Recorded child groups and a workload's own count can briefly disagree.
		parent.AvailableReplicas = min(available[parent.Name], parent.ReadyReplicas)
		for j := range parent.SubRoleStatuses {
			child := &parent.SubRoleStatuses[j]
			child.AvailableReplicas = available[childRoleKey(parent.Name, child.Name)]
		}
	}
}

// setDesiredReplicas publishes the target of every parent and child, totalled
// across slices. An unknown target stays unset so readers never mistake it for a
// requested zero.
func setDesiredReplicas(statuses []disaggregatedsetv1.RoleStatus, desiredReplicasByRole map[string]int, sliceCount int32) {
	desired := func(key string) *int32 {
		perSlice, known := desiredReplicasByRole[key]
		if !known {
			return nil
		}
		return ptr.To(int32(perSlice) * sliceCount)
	}
	for i := range statuses {
		parent := &statuses[i]
		parent.DesiredReplicas = desired(parent.Name)
		for j := range parent.SubRoleStatuses {
			child := &parent.SubRoleStatuses[j]
			child.DesiredReplicas = desired(childRoleKey(parent.Name, child.Name))
		}
	}
}

// setMinAvailableReplicas publishes the planner's availability floor for every
// current parent and child, totalled across slices. Budgets are resolved per
// slice against the latest target, exactly as the rollout planner resolves them.
// Removed roles have no published status, so their historical floors are omitted.
func setMinAvailableReplicas(ds *disaggregatedsetv1.DisaggregatedSet, statuses []disaggregatedsetv1.RoleStatus, desiredReplicasByRole map[string]int, sliceCount int32) {
	leaves := rolloutRoleNames(ds)
	config := extractRollingUpdateConfig(ds, leaves, desiredReplicasByRole)
	floors := make(map[string]int32, len(leaves))
	for i, leaf := range leaves {
		if desired, known := desiredReplicasByRole[leaf]; known {
			floors[leaf] = int32(max(0, desired-config[i].MaxUnavailable)) * sliceCount
		}
	}
	floor := func(key string) *int32 {
		if value, known := floors[key]; known {
			return ptr.To(value)
		}
		return nil
	}
	for i := range statuses {
		parent := &statuses[i]
		if len(parent.SubRoleStatuses) == 0 {
			parent.MinAvailableReplicas = floor(parent.Name)
			continue
		}
		total := ptr.To[int32](0)
		for j := range parent.SubRoleStatuses {
			child := &parent.SubRoleStatuses[j]
			child.MinAvailableReplicas = floor(childRoleKey(parent.Name, child.Name))
			if child.MinAvailableReplicas == nil {
				total = nil
			} else if total != nil {
				*total += *child.MinAvailableReplicas
			}
		}
		parent.MinAvailableReplicas = total
	}
}

// minimumReplicasAvailableCondition reports whether every parent and child is
// serving at least its floor. Unlike Available, a scale-up or a rollout that
// stays within maxUnavailable leaves it True.
func minimumReplicasAvailableCondition(ds *disaggregatedsetv1.DisaggregatedSet, statuses []disaggregatedsetv1.RoleStatus) metav1.Condition {
	var unknown, below []string
	check := func(name string, floor *int32, available int32) {
		if floor == nil {
			unknown = append(unknown, name)
		} else if available < *floor {
			below = append(below, fmt.Sprintf("%s (%d/%d)", name, available, *floor))
		}
	}
	for _, parent := range statuses {
		check(parent.Name, parent.MinAvailableReplicas, parent.AvailableReplicas)
		for _, child := range parent.SubRoleStatuses {
			check(childRoleKey(parent.Name, child.Name), child.MinAvailableReplicas, child.AvailableReplicas)
		}
	}
	condition := metav1.Condition{
		Type:               string(disaggregatedsetv1.DisaggregatedSetMinimumReplicasAvailable),
		Status:             metav1.ConditionTrue,
		ObservedGeneration: ds.Generation,
		Reason:             "MinimumReplicasAvailable",
		Message:            "Every role has at least its minimum available replicas",
	}
	// A known failure outranks an unknown floor, which could only add to it.
	switch {
	case len(below) > 0:
		condition.Status = metav1.ConditionFalse
		condition.Reason = "MinimumReplicasUnavailable"
		condition.Message = fmt.Sprintf("Roles below their minimum available replicas (available/minimum): %s", listRoles(below))
		if len(unknown) > 0 {
			condition.Message += fmt.Sprintf("; minimum available replicas are unknown for roles: %s", listRoles(unknown))
		}
	case len(unknown) > 0:
		condition.Status = metav1.ConditionUnknown
		condition.Reason = "ReplicaTargetsUnresolved"
		condition.Message = fmt.Sprintf("Minimum available replicas are unknown for roles: %s", listRoles(unknown))
	}
	return condition
}

// maxConditionRoles bounds a role list so that a condition message stays within
// the API's length limit, which would otherwise reject the whole status update.
const maxConditionRoles = 10

func listRoles(names []string) string {
	if len(names) <= maxConditionRoles {
		return strings.Join(names, ", ")
	}
	return fmt.Sprintf("%s and %d more", strings.Join(names[:maxConditionRoles], ", "), len(names)-maxConditionRoles)
}

// summarizeRoleStatuses projects public nested status into scaler keys and
// checks that every parent and child has reached its published target.
func summarizeRoleStatuses(statuses []disaggregatedsetv1.RoleStatus) (map[string]int32, bool) {
	replicasByRole := make(map[string]int32)
	available := true
	record := func(name string, desired *int32, replicas, ready, updated int32) {
		replicasByRole[name] = replicas
		// Unknown targets must not be mistaken for a requested zero.
		available = available && desired != nil && replicas == *desired && ready == *desired && updated == *desired
	}
	for _, parent := range statuses {
		record(parent.Name, parent.DesiredReplicas, parent.Replicas, parent.ReadyReplicas, parent.UpdatedReplicas)
		for _, child := range parent.SubRoleStatuses {
			record(childRoleKey(parent.Name, child.Name), child.DesiredReplicas, child.Replicas, child.ReadyReplicas, child.UpdatedReplicas)
		}
	}
	return replicasByRole, available
}

func setSubRolesAssignedCondition(ds *disaggregatedsetv1.DisaggregatedSet, assigned bool) bool {
	for _, role := range ds.Status.RoleStatuses {
		if len(role.SubRoleStatuses) == 0 {
			continue
		}
		condition := metav1.Condition{
			Type:               string(disaggregatedsetv1.DisaggregatedSetSubRolesAssigned),
			Status:             metav1.ConditionTrue,
			Reason:             "Assigned",
			Message:            "Every replica group has a coherent sub-role assignment",
			ObservedGeneration: ds.Generation,
		}
		if !assigned {
			condition.Status = metav1.ConditionFalse
			condition.Reason = "AssignmentPending"
			condition.Message = "Waiting for group sub-role assignments"
		}
		return setDisaggregatedSetCondition(ds, condition)
	}
	return meta.RemoveStatusCondition(&ds.Status.Conditions, string(disaggregatedsetv1.DisaggregatedSetSubRolesAssigned))
}

// subRoleStatus counts coherent groups in the parent, including historical
// layouts. Current child entries are the matching subset of that aggregate.
func (m *LeaderWorkerSetManager) subRoleStatus(ctx context.Context, role *disaggregatedsetv1.DisaggregatedRoleSpec, workloads []*leaderworkersetv1.LeaderWorkerSet, revision string, capacity servingCapacity) (disaggregatedsetv1.RoleStatus, bool, error) {
	status := disaggregatedsetv1.RoleStatus{Name: role.Name, SubRoleStatuses: make([]disaggregatedsetv1.SubRoleStatus, len(role.SubRoles))}
	index := map[string]int{}
	for i, child := range role.SubRoles {
		index[child.Name] = i
		status.SubRoleStatuses[i].Name = child.Name
	}
	assigned := true
	for _, lws := range workloads {
		// Status shares the indexed informer reader used for rollout observations.
		s, err := replicagroups.Observe(ctx, m.reader, lws)
		if err != nil {
			return status, false, err
		}
		if s == nil {
			return status, false, errReplicaGroupsPending
		}
		membership, err := subRoleCounts(s.LWS, subRoleReplicasAnnotation)
		if err != nil {
			return status, false, err
		}
		current := s.LWS.Labels[disaggregatedsetv1.RevisionLabelKey] == revision
		serving := capacity.revision(s.LWS)
		// The current revision is required by its targets: sub-roles change in place,
		// so its recorded layout can name children that the spec no longer has.
		if !current {
			// The executor reports an invalid frozen baseline; status falls back to
			// the issued counts rather than withholding every other publication.
			initial, _ := subRoleCounts(s.LWS, disaggregatedsetv1.InitialSubRoleReplicasAnnotationKey)
			serving.requireSubRoles(role.Name, membership, initial)
			if membership == nil && initial == nil {
				serving.observe(role.Name, 0, intendedReplicas(s.LWS) > 0)
			}
		}
		for _, group := range activeSubRoleGroups(s) {
			name, coherent := groupSubRole(group)
			i, known := index[name]
			valid := known
			if !current {
				_, valid = membership[name]
				valid = valid || membership == nil && name == ""
			}
			if !valid || !coherent {
				assigned = false
				continue
			}
			ready, updated := groupReadyReplicas(group), int32(0)
			if current {
				updated = 1
			}
			status.Replicas++
			status.ReadyReplicas += ready
			status.UpdatedReplicas += updated
			serving.observe(childRoleKey(role.Name, name), ready, false)
			if known {
				child := &status.SubRoleStatuses[i]
				child.Replicas++
				child.ReadyReplicas += ready
				child.UpdatedReplicas += updated
			}
		}
	}
	return status, assigned, nil
}

func groupReadyReplicas(group replicagroups.Group) int32 {
	if group.Ready && !group.Terminating {
		return 1
	}
	return 0
}

func setRoleStatuses(disaggregatedSet *disaggregatedsetv1.DisaggregatedSet, roleStatuses []disaggregatedsetv1.RoleStatus) bool {
	if equality.Semantic.DeepEqual(disaggregatedSet.Status.RoleStatuses, roleStatuses) {
		return false
	}
	disaggregatedSet.Status.RoleStatuses = roleStatuses
	return true
}

func disaggregatedSetCondition(disaggregatedSet *disaggregatedsetv1.DisaggregatedSet, available bool, unresolvedRoles []string) metav1.Condition {
	condType := disaggregatedsetv1.DisaggregatedSetProgressing
	reason, message := "RolloutInProgress", "Not all roles have reached their desired replica count, ready and updated to the current revision"
	if len(unresolvedRoles) > 0 {
		reason = "ReplicaTargetsUnresolved"
		message = fmt.Sprintf("DisaggregatedSetRoleScaler could not be created or adopted for roles: %s", listRoles(unresolvedRoles))
	} else if available {
		condType = disaggregatedsetv1.DisaggregatedSetAvailable
		reason, message = "AllRolesReady", "All roles have reached their desired replica count, ready and updated to the current revision"
	}

	return metav1.Condition{
		Type:               string(condType),
		Status:             metav1.ConditionTrue,
		ObservedGeneration: disaggregatedSet.Generation,
		Reason:             reason,
		Message:            message,
	}
}

// exclusiveConditionTypes reports whether t1 and t2 are a mutually-exclusive
// pair, where one being true means the other must be false. Only
// Available/Progressing are exclusive today; a condition type outside that
// pair (added later, or written by another controller) is left untouched by
// setDisaggregatedSetCondition rather than being clobbered just because it
// happened to also be true.
func exclusiveConditionTypes(t1, t2 string) bool {
	pair := func(a, b disaggregatedsetv1.DisaggregatedSetConditionType) bool {
		return (t1 == string(a) && t2 == string(b)) || (t1 == string(b) && t2 == string(a))
	}
	return pair(disaggregatedsetv1.DisaggregatedSetAvailable, disaggregatedsetv1.DisaggregatedSetProgressing)
}

// setDisaggregatedSetCondition records newCondition as true and, since Available and
// Progressing are mutually exclusive, marks the other one of that specific pair as
// false (see exclusiveConditionTypes) — any other condition type is left alone.
// LastTransitionTime is only touched when a condition's Status actually flips, per
// the metav1.Condition contract; a same-Status update (e.g. only ObservedGeneration
// changed) must not look like a fresh transition to clients. Returns whether the
// status changed.
func setDisaggregatedSetCondition(disaggregatedSet *disaggregatedsetv1.DisaggregatedSet, newCondition metav1.Condition) bool {
	now := metav1.Now()
	changed, found := false, false

	for i, cond := range disaggregatedSet.Status.Conditions {
		if cond.Type == newCondition.Type {
			found = true
			if cond.Status != newCondition.Status {
				newCondition.LastTransitionTime = now
				disaggregatedSet.Status.Conditions[i] = newCondition
				changed = true
			} else if cond.ObservedGeneration != newCondition.ObservedGeneration || cond.Reason != newCondition.Reason || cond.Message != newCondition.Message {
				// Status is unchanged, so LastTransitionTime is preserved, but every
				// other field still syncs to the latest computed condition.
				disaggregatedSet.Status.Conditions[i].ObservedGeneration = newCondition.ObservedGeneration
				disaggregatedSet.Status.Conditions[i].Reason = newCondition.Reason
				disaggregatedSet.Status.Conditions[i].Message = newCondition.Message
				changed = true
			}
			continue
		}
		if !exclusiveConditionTypes(cond.Type, newCondition.Type) {
			continue
		}
		if cond.Status == metav1.ConditionTrue {
			// newCondition becoming true is exactly why this mutually-exclusive
			// condition is now false, so it explains the flip with the same
			// Reason/Message rather than leaving this condition's old (now
			// contradictory) ones in place.
			disaggregatedSet.Status.Conditions[i].Status = metav1.ConditionFalse
			disaggregatedSet.Status.Conditions[i].LastTransitionTime = now
			disaggregatedSet.Status.Conditions[i].ObservedGeneration = newCondition.ObservedGeneration
			disaggregatedSet.Status.Conditions[i].Reason = newCondition.Reason
			disaggregatedSet.Status.Conditions[i].Message = newCondition.Message
			changed = true
		} else if cond.ObservedGeneration != newCondition.ObservedGeneration {
			// Already false and staying false — no real transition, so only
			// ObservedGeneration needs to catch up; Reason/Message still
			// accurately describe why it became false and don't need to change.
			disaggregatedSet.Status.Conditions[i].ObservedGeneration = newCondition.ObservedGeneration
			changed = true
		}
	}

	if !found {
		newCondition.LastTransitionTime = now
		disaggregatedSet.Status.Conditions = append(disaggregatedSet.Status.Conditions, newCondition)
		changed = true
	}

	return changed
}

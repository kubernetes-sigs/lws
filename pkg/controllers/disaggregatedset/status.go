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
	"strings"

	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	disaggregatedsetv1 "sigs.k8s.io/lws/api/disaggregatedset/v1"
	leaderworkersetv1 "sigs.k8s.io/lws/api/leaderworkerset/v1"
	"sigs.k8s.io/lws/pkg/replicagroups"
	disaggregatedsetutils "sigs.k8s.io/lws/pkg/utils/disaggregatedset"
)

// updateStatus publishes cached counts and conditions.
// Scalers use the same counts as the DisaggregatedSet's nested role status.
func (r *DisaggregatedSetReconciler) updateStatus(ctx context.Context, ds *disaggregatedsetv1.DisaggregatedSet, roleNames []string, revision string, desiredReplicasByRole map[string]int, scalers map[string]*disaggregatedsetv1.DisaggregatedSetRoleScaler) error {
	roleStatuses, assigned, err := r.LWSManager.collectRoleStatuses(ctx, ds, roleNames, revision)
	if err != nil {
		return err
	}
	observedReplicas, available := summarizeRoleStatuses(roleStatuses, desiredReplicasByRole, disaggregatedsetutils.GetSlices(ds))

	changed := setRoleStatuses(ds, roleStatuses)
	changed = setSubRolesAssignedCondition(ds, assigned) || changed
	unresolvedRoles := unresolvedReplicaTargetRoles(roleNames, desiredReplicasByRole)
	changed = setDisaggregatedSetCondition(ds, disaggregatedSetCondition(ds, available && assigned, unresolvedRoles)) || changed
	if ds.Status.ObservedGeneration != ds.Generation {
		ds.Status.ObservedGeneration = ds.Generation
		changed = true
	}

	// A failure of one status writer must not suppress the other.
	var statusErr error
	if len(scalers) > 0 {
		statusErr = r.ScalerManager.WriteStatus(ctx, ds, scalers, observedReplicas)
	}
	if changed {
		if err := r.Status().Update(ctx, ds); err != nil {
			statusErr = errors.Join(statusErr, fmt.Errorf("failed to update DisaggregatedSet status: %w", err))
		}
	}
	return statusErr
}

// collectRoleStatuses aggregates all slices and revisions. Only current roles
// and children are published, in spec order; removed children still count toward
// their parent's total while their old groups drain.
func (m *LeaderWorkerSetManager) collectRoleStatuses(ctx context.Context, ds *disaggregatedsetv1.DisaggregatedSet, roleNames []string, revision string) ([]disaggregatedsetv1.RoleStatus, bool, error) {
	statuses := make([]disaggregatedsetv1.RoleStatus, 0, len(roleNames))
	configs := disaggregatedsetutils.GetRoleConfigs(ds)
	allAssigned := true
	for _, role := range roleNames {
		workloads, err := m.ListAll(ctx, ds, role)
		if err != nil {
			return nil, false, fmt.Errorf("failed to list LWS for role %s status: %w", role, err)
		}
		status := disaggregatedsetv1.RoleStatus{Name: role}
		if config := configs[role]; config != nil && len(config.SubRoles) > 0 {
			var assigned bool
			status, assigned, err = m.subRoleStatus(ctx, config, workloads, revision)
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
			}
		}
		statuses = append(statuses, status)
	}
	return statuses, allAssigned, nil
}

// summarizeRoleStatuses projects public nested status into scaler keys and
// checks that every parent and child has reached its own target.
func summarizeRoleStatuses(statuses []disaggregatedsetv1.RoleStatus, desiredReplicasByRole map[string]int, sliceCount int32) (map[string]int32, bool) {
	replicasByRole := make(map[string]int32)
	available := true
	record := func(name string, replicas, ready, updated int32) {
		replicasByRole[name] = replicas
		perSlice, known := desiredReplicasByRole[name]
		desired := int32(perSlice) * sliceCount
		// Unknown targets must not be mistaken for a requested zero.
		available = available && known && replicas == desired && ready == desired && updated == desired
	}
	for _, parent := range statuses {
		record(parent.Name, parent.Replicas, parent.ReadyReplicas, parent.UpdatedReplicas)
		for _, child := range parent.SubRoleStatuses {
			record(childRoleKey(parent.Name, child.Name), child.Replicas, child.ReadyReplicas, child.UpdatedReplicas)
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
func (m *LeaderWorkerSetManager) subRoleStatus(ctx context.Context, role *disaggregatedsetv1.DisaggregatedRoleSpec, workloads []*leaderworkersetv1.LeaderWorkerSet, revision string) (disaggregatedsetv1.RoleStatus, bool, error) {
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
			var ready, updated int32
			if group.Ready && !group.Terminating {
				ready = 1
			}
			if current {
				updated = 1
			}
			status.Replicas++
			status.ReadyReplicas += ready
			status.UpdatedReplicas += updated
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
		message = fmt.Sprintf("DisaggregatedSetRoleScaler could not be created or adopted for roles: %s", strings.Join(unresolvedRoles, ", "))
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

/*
Copyright 2026 The Kubernetes Authors.

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
	"reflect"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	disaggregatedsetv1 "sigs.k8s.io/lws/api/disaggregatedset/v1"
	disaggregatedsetutils "sigs.k8s.io/lws/pkg/utils/disaggregatedset"
)

func (r *DisaggregatedSetReconciler) reconcileAssignments(
	ctx context.Context,
	ds *disaggregatedsetv1.DisaggregatedSet,
	targetRevision string,
	scalers scalerMap,
) (bool, error) {
	if r.AssignmentManager == nil {
		r.AssignmentManager = NewAssignmentManager(r.Client)
	}
	lwsList, err := r.LWSManager.List(ctx, ds.Namespace, ds.Name, -1, "")
	if err != nil {
		return false, fmt.Errorf("list LWS for sub-role assignment: %w", err)
	}

	converged := true
	for _, lws := range lwsList {
		roleName := lws.Labels[disaggregatedsetv1.RoleLabelKey]
		role := disaggregatedsetutils.GetRoleSpec(ds, roleName)
		if role == nil {
			continue
		}
		if len(role.SubRoles) == 0 {
			changed, _, reconcileErr := r.AssignmentManager.Reconcile(ctx, ds.Namespace, lws.Name, nil, nil)
			if reconcileErr != nil {
				return false, reconcileErr
			}
			converged = converged && !changed
			continue
		}

		order, valid := subRoleOrderAndSet(role)
		physicalReplicas := int(getLWSReplicas(lws))
		current := make(map[disaggregatedsetutils.RoleKey]int, len(order))
		observed, observeErr := r.AssignmentManager.Observe(ctx, ds.Namespace, lws.Name, valid)
		if observeErr != nil {
			return false, observeErr
		}
		// After a scale-down write, high-ordinal Pods may still be terminating.
		// Do not rebalance labels during that window or the prepared victim
		// ordering could be undone before StatefulSet removes those groups.
		if len(observed.GroupIndexes) > physicalReplicas {
			converged = false
			continue
		}
		for _, name := range order {
			current[disaggregatedsetutils.RoleKey{Role: roleName, SubRole: name}] = observed.Replicas[name]
		}

		var desired map[string]int
		if lws.Labels[disaggregatedsetv1.RevisionLabelKey] == targetRevision {
			desired = fitSubRoleTargets(order, desiredSubRoleReplicas(ds, roleName, scalers, current), physicalReplicas)
		} else if observed.Unassigned == 0 && sumCounts(observed.Replicas) == physicalReplicas {
			desired = observed.Replicas
		} else {
			desired = fitSubRoleTargets(order, desiredSubRoleReplicas(ds, roleName, scalers, current), physicalReplicas)
		}

		changed, summary, reconcileErr := r.AssignmentManager.Reconcile(ctx, ds.Namespace, lws.Name, order, desired)
		if reconcileErr != nil {
			return false, reconcileErr
		}
		if changed || summary.Unassigned != 0 || !hasExpectedGroupOrdinals(summary, physicalReplicas) {
			converged = false
		}
	}
	return converged, nil
}

func (r *DisaggregatedSetReconciler) updateDisaggregatedSetStatus(
	ctx context.Context,
	ds *disaggregatedsetv1.DisaggregatedSet,
	targetRevision string,
) error {
	lwsList, err := r.LWSManager.List(ctx, ds.Namespace, ds.Name, -1, "")
	if err != nil {
		return fmt.Errorf("list LWS for DisaggregatedSet status: %w", err)
	}

	roleStatuses := make([]disaggregatedsetv1.RoleStatus, 0, len(ds.Spec.Roles))
	allAssigned := true
	for i := range ds.Spec.Roles {
		role := &ds.Spec.Roles[i]
		status := disaggregatedsetv1.RoleStatus{Name: role.Name}
		if len(role.SubRoles) == 0 {
			for _, lws := range lwsList {
				if lws.Labels[disaggregatedsetv1.RoleLabelKey] != role.Name {
					continue
				}
				status.Replicas += lws.Status.Replicas
				status.ReadyReplicas += lws.Status.ReadyReplicas
				if lws.Labels[disaggregatedsetv1.RevisionLabelKey] == targetRevision {
					status.UpdatedReplicas += lws.Status.Replicas
				}
			}
			roleStatuses = append(roleStatuses, status)
			continue
		}

		_, valid := subRoleOrderAndSet(role)
		status.SubRoleStatuses = make([]disaggregatedsetv1.SubRoleStatus, len(role.SubRoles))
		bySubRole := make(map[string]*disaggregatedsetv1.SubRoleStatus, len(role.SubRoles))
		for i, subRole := range role.SubRoles {
			status.SubRoleStatuses[i].Name = subRole.Name
			bySubRole[subRole.Name] = &status.SubRoleStatuses[i]
		}
		for _, lws := range lwsList {
			if lws.Labels[disaggregatedsetv1.RoleLabelKey] != role.Name {
				continue
			}
			summary, observeErr := r.AssignmentManager.Observe(ctx, ds.Namespace, lws.Name, valid)
			if observeErr != nil {
				return observeErr
			}
			if summary.Unassigned != 0 || !hasExpectedGroupOrdinals(summary, int(getLWSReplicas(lws))) {
				allAssigned = false
			}
			for name, count := range summary.Replicas {
				subStatus := bySubRole[name]
				if subStatus == nil {
					continue
				}
				subStatus.Replicas += int32(count)
				subStatus.ReadyReplicas += int32(summary.ReadyReplicas[name])
				if lws.Labels[disaggregatedsetv1.RevisionLabelKey] == targetRevision {
					subStatus.UpdatedReplicas += int32(count)
				}
			}
		}
		for _, subStatus := range status.SubRoleStatuses {
			status.Replicas += subStatus.Replicas
			status.ReadyReplicas += subStatus.ReadyReplicas
			status.UpdatedReplicas += subStatus.UpdatedReplicas
		}
		roleStatuses = append(roleStatuses, status)
	}

	desired := ds.DeepCopy()
	desired.Status.RoleStatuses = roleStatuses
	conditionStatus := metav1.ConditionTrue
	reason := "AssignmentsConverged"
	message := "Every live LWS group has a valid sub-role assignment"
	if !allAssigned {
		conditionStatus = metav1.ConditionFalse
		reason = "AssignmentsPending"
		message = "One or more live LWS groups are waiting for a valid sub-role assignment"
	}
	apimeta.SetStatusCondition(&desired.Status.Conditions, metav1.Condition{
		Type:               disaggregatedsetv1.DisaggregatedSetSubRolesAssigned,
		Status:             conditionStatus,
		ObservedGeneration: ds.Generation,
		Reason:             reason,
		Message:            message,
	})
	if reflect.DeepEqual(ds.Status, desired.Status) {
		return nil
	}
	if err := r.Status().Patch(ctx, desired, client.MergeFrom(ds)); err != nil {
		return fmt.Errorf("patch DisaggregatedSet status: %w", err)
	}
	return nil
}

func subRoleOrderAndSet(role *disaggregatedsetv1.DisaggregatedRoleSpec) ([]string, map[string]bool) {
	order := make([]string, 0, len(role.SubRoles))
	valid := make(map[string]bool, len(role.SubRoles))
	for _, subRole := range role.SubRoles {
		order = append(order, subRole.Name)
		valid[subRole.Name] = true
	}
	return order, valid
}

func sumCounts(counts map[string]int) int {
	total := 0
	for _, count := range counts {
		total += count
	}
	return total
}

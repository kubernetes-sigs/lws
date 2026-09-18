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
	"maps"
	"slices"
	"strings"

	leaderworkersetv1 "sigs.k8s.io/lws/api/leaderworkerset/v1"

	disaggregatedsetv1 "sigs.k8s.io/lws/api/disaggregatedset/v1"
	disaggregatedsetutils "sigs.k8s.io/lws/pkg/utils/disaggregatedset"
)

type subRoleWorkloadState struct {
	lws      *leaderworkersetv1.LeaderWorkerSet
	capacity int
	observed SubRoleAssignmentSummary
}

// SubRoleRolloutCoordinator translates a physical parent-role rollout step
// into sub-role assignments. The existing planner remains authoritative for
// aggregate surge and unavailability; this coordinator ensures the groups it
// keeps or adds retain the requested routing-pool distribution.
type SubRoleRolloutCoordinator struct {
	lwsManager  *LeaderWorkerSetManager
	assignments *SubRoleAssignmentReconciler
	targets     *SubRoleTargetResolver
}

func NewSubRoleRolloutCoordinator(
	lwsManager *LeaderWorkerSetManager,
	assignments *SubRoleAssignmentReconciler,
	targets *SubRoleTargetResolver,
) *SubRoleRolloutCoordinator {
	return &SubRoleRolloutCoordinator{
		lwsManager:  lwsManager,
		assignments: assignments,
		targets:     targets,
	}
}

// ReconcileAssignments labels all currently observed groups in one slice. It
// returns false while Pods are still appearing/disappearing or a patch was made.
func (c *SubRoleRolloutCoordinator) ReconcileAssignments(
	ctx context.Context,
	ds *disaggregatedsetv1.DisaggregatedSet,
	slice int,
	targetRevision string,
	scalers ScalerMap,
) (bool, error) {
	all, err := c.lwsManager.List(ctx, ds, slice, "")
	if err != nil {
		return false, fmt.Errorf("list LWS for sub-role assignment: %w", err)
	}
	converged := true
	for i := range ds.Spec.Roles {
		role := &ds.Spec.Roles[i]
		var roleLWS []*leaderworkersetv1.LeaderWorkerSet
		for _, lws := range all {
			if lws.Labels[disaggregatedsetv1.RoleLabelKey] == role.Name {
				roleLWS = append(roleLWS, lws)
			}
		}
		roleConverged, err := c.reconcileRole(ctx, ds, role, roleLWS, targetRevision, scalers, nil)
		if err != nil {
			return false, err
		}
		converged = converged && roleConverged
	}
	return converged, nil
}

// SnapshotInitialAssignments records the old revision's distribution before a
// rollout starts. Reconciliation must already have converged, otherwise a
// partial snapshot could become permanent.
func (c *SubRoleRolloutCoordinator) SnapshotInitialAssignments(
	ctx context.Context,
	ds *disaggregatedsetv1.DisaggregatedSet,
	oldRevisions disaggregatedsetutils.RevisionRolesList,
) error {
	for _, revision := range oldRevisions {
		for roleName, lws := range revision.Roles {
			role := c.targets.Role(ds, roleName)
			if role == nil || len(role.SubRoles) == 0 {
				continue
			}
			order, valid := subRoleOrderAndSet(role)
			summary, err := c.assignments.Observe(ctx, ds.Namespace, lws.Name, valid)
			if err != nil {
				return err
			}
			if summary.Unassigned != 0 || !hasExpectedGroupOrdinals(summary, int(getLWSReplicas(lws))) {
				return fmt.Errorf("cannot snapshot sub-role assignments for %s before assignments converge", lws.Name)
			}
			distribution := make(map[string]int, len(order))
			for _, name := range order {
				distribution[name] = summary.Replicas[name]
			}
			if _, err := c.lwsManager.SetInitialSubRoleReplicas(ctx, lws, distribution); err != nil {
				return err
			}
		}
	}
	return nil
}

// PrepareScaleDown assigns the post-scale distribution and places its retained
// groups on low ordinals before the caller lowers LWS.spec.replicas.
func (c *SubRoleRolloutCoordinator) PrepareScaleDown(
	ctx context.Context,
	ds *disaggregatedsetv1.DisaggregatedSet,
	slice int,
	targetRevision, roleName string,
	victim *leaderworkersetv1.LeaderWorkerSet,
	retained int,
	scalers ScalerMap,
) error {
	role := c.targets.Role(ds, roleName)
	if role == nil || len(role.SubRoles) == 0 || retained >= int(getLWSReplicas(victim)) {
		return nil
	}
	all, err := c.lwsManager.List(ctx, ds, slice, roleName)
	if err != nil {
		return fmt.Errorf("list LWS for sub-role scale-down: %w", err)
	}
	overrides := map[string]int{victim.Name: retained}
	_, err = c.reconcileRole(ctx, ds, role, all, targetRevision, scalers, overrides)
	if err != nil {
		return err
	}

	states, complete, err := c.observeStates(ctx, ds.Namespace, role, all, targetRevision, overrides)
	if err != nil {
		return err
	}
	if !complete {
		return fmt.Errorf("cannot prepare scale-down for %s before all current groups are observed", victim.Name)
	}
	current := aggregateSubRoleCounts(roleName, states)
	quotas := planSubRoleQuotas(states, subRoleNames(role), c.targets.SubRoleTargets(ds, roleName, scalers, current))
	return c.prepareVictim(ctx, ds.Namespace, victim, subRoleNames(role), quotas[victim.Name])
}

func (c *SubRoleRolloutCoordinator) prepareVictim(
	ctx context.Context,
	namespace string,
	victim *leaderworkersetv1.LeaderWorkerSet,
	order []string,
	retained map[string]int,
) error {
	_, err := c.assignments.PrepareScaleDown(ctx, namespace, victim.Name, order, retained)
	return err
}

func (c *SubRoleRolloutCoordinator) reconcileRole(
	ctx context.Context,
	ds *disaggregatedsetv1.DisaggregatedSet,
	role *disaggregatedsetv1.DisaggregatedRoleSpec,
	workloads []*leaderworkersetv1.LeaderWorkerSet,
	targetRevision string,
	scalers ScalerMap,
	capacityOverrides map[string]int,
) (bool, error) {
	if len(role.SubRoles) == 0 {
		converged := true
		for _, lws := range workloads {
			changed, _, err := c.assignments.Reconcile(ctx, ds.Namespace, lws.Name, nil, nil)
			if err != nil {
				return false, err
			}
			converged = converged && !changed
		}
		return converged, nil
	}

	states, complete, err := c.observeStates(ctx, ds.Namespace, role, workloads, targetRevision, capacityOverrides)
	if err != nil || !complete {
		return false, err
	}
	order := subRoleNames(role)
	current := aggregateSubRoleCounts(role.Name, states)
	targets := c.targets.SubRoleTargets(ds, role.Name, scalers, current)
	quotas := planSubRoleQuotas(states, order, targets)

	converged := true
	for _, state := range states {
		changed, summary, err := c.assignments.Reconcile(ctx, ds.Namespace, state.lws.Name, order, quotas[state.lws.Name])
		if err != nil {
			return false, err
		}
		if changed || summary.Unassigned != 0 {
			converged = false
		}
	}
	return converged, nil
}

func (c *SubRoleRolloutCoordinator) observeStates(
	ctx context.Context,
	namespace string,
	role *disaggregatedsetv1.DisaggregatedRoleSpec,
	workloads []*leaderworkersetv1.LeaderWorkerSet,
	targetRevision string,
	capacityOverrides map[string]int,
) ([]subRoleWorkloadState, bool, error) {
	_, valid := subRoleOrderAndSet(role)
	states := make([]subRoleWorkloadState, 0, len(workloads))
	complete := true
	for _, lws := range workloads {
		summary, err := c.assignments.Observe(ctx, namespace, lws.Name, valid)
		if err != nil {
			return nil, false, err
		}
		observedCapacity := len(summary.GroupIndexes)
		currentCapacity := int(getLWSReplicas(lws))
		if observedCapacity != currentCapacity {
			complete = false
		}
		capacity := observedCapacity
		if override, found := capacityOverrides[lws.Name]; found {
			if override < 0 || override > observedCapacity {
				return nil, false, fmt.Errorf("invalid retained replica count %d for LWS %s with %d observed groups", override, lws.Name, observedCapacity)
			}
			capacity = override
		}
		states = append(states, subRoleWorkloadState{lws: lws, capacity: capacity, observed: summary})
	}
	slices.SortStableFunc(states, func(a, b subRoleWorkloadState) int {
		aTarget := a.lws.Labels[disaggregatedsetv1.RevisionLabelKey] == targetRevision
		bTarget := b.lws.Labels[disaggregatedsetv1.RevisionLabelKey] == targetRevision
		if aTarget != bTarget {
			if aTarget {
				return -1
			}
			return 1
		}
		return strings.Compare(a.lws.Name, b.lws.Name)
	})
	return states, complete, nil
}

func aggregateSubRoleCounts(roleName string, states []subRoleWorkloadState) map[RoleKey]int {
	result := make(map[RoleKey]int)
	for _, state := range states {
		for name, count := range state.observed.Replicas {
			result[RoleKey{Role: roleName, SubRole: name}] += count
		}
	}
	return result
}

// planSubRoleQuotas is pure. It preserves valid assignments globally before
// allocating deficits, while preferring the target revision through states'
// ordering. Capacity may describe a proposed post-scale size.
func planSubRoleQuotas(states []subRoleWorkloadState, order []string, final map[string]int) map[string]map[string]int {
	total := 0
	for _, state := range states {
		total += state.capacity
	}
	desired := fitSubRoleTargets(order, final, total)
	remaining := maps.Clone(desired)
	quotas := make(map[string]map[string]int, len(states))
	free := make(map[string]int, len(states))

	for _, state := range states {
		quota := make(map[string]int, len(order))
		slots := state.capacity
		for _, name := range order {
			keep := min(state.observed.Replicas[name], remaining[name], slots)
			quota[name] = keep
			remaining[name] -= keep
			slots -= keep
		}
		quotas[state.lws.Name] = quota
		free[state.lws.Name] = slots
	}

	for _, state := range states {
		for free[state.lws.Name] > 0 {
			name := largestSubRoleDeficit(order, desired, subtractSubRoleCounts(desired, remaining))
			if name == "" {
				if len(order) == 0 {
					break
				}
				name = order[0]
			} else {
				remaining[name]--
			}
			quotas[state.lws.Name][name]++
			free[state.lws.Name]--
		}
	}
	return quotas
}

func subtractSubRoleCounts(total, remaining map[string]int) map[string]int {
	result := make(map[string]int, len(total))
	for name, count := range total {
		result[name] = count - remaining[name]
	}
	return result
}

func subRoleNames(role *disaggregatedsetv1.DisaggregatedRoleSpec) []string {
	result := make([]string, 0, len(role.SubRoles))
	for _, subRole := range role.SubRoles {
		result = append(result, subRole.Name)
	}
	return result
}

func subRoleOrderAndSet(role *disaggregatedsetv1.DisaggregatedRoleSpec) ([]string, map[string]bool) {
	order := subRoleNames(role)
	valid := make(map[string]bool, len(order))
	for _, name := range order {
		valid[name] = true
	}
	return order, valid
}

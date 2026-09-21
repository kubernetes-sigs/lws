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
	"slices"
	"strconv"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	disaggregatedsetv1 "sigs.k8s.io/lws/api/disaggregatedset/v1"
	leaderworkersetv1 "sigs.k8s.io/lws/api/leaderworkerset/v1"
)

type replicaGroup struct {
	index  int
	leader *corev1.Pod
	pods   []*corev1.Pod
}

// SubRoleAssignmentSummary is the leader-based observed assignment state for
// one LeaderWorkerSet.
type SubRoleAssignmentSummary struct {
	Replicas      map[string]int
	ReadyReplicas map[string]int
	Unassigned    int
	GroupIndexes  []int
}

// SubRoleAssignmentReconciler maintains the controller-owned sub-role label on
// every Pod in an ordinal LWS replica group.
type SubRoleAssignmentReconciler struct {
	client client.Client
}

func NewSubRoleAssignmentReconciler(c client.Client) *SubRoleAssignmentReconciler {
	return &SubRoleAssignmentReconciler{client: c}
}

func (r *SubRoleAssignmentReconciler) listGroups(ctx context.Context, namespace, lwsName string) ([]replicaGroup, error) {
	list := &corev1.PodList{}
	if err := r.client.List(ctx, list, client.InNamespace(namespace), client.MatchingLabels{
		leaderworkersetv1.SetNameLabelKey: lwsName,
	}); err != nil {
		return nil, fmt.Errorf("list Pods for LWS %s: %w", lwsName, err)
	}

	byIndex := make(map[int]*replicaGroup)
	for i := range list.Items {
		pod := &list.Items[i]
		if !pod.DeletionTimestamp.IsZero() {
			continue
		}
		index, err := strconv.Atoi(pod.Labels[leaderworkersetv1.GroupIndexLabelKey])
		if err != nil {
			continue
		}
		group := byIndex[index]
		if group == nil {
			group = &replicaGroup{index: index}
			byIndex[index] = group
		}
		group.pods = append(group.pods, pod)
		if pod.Labels[leaderworkersetv1.WorkerIndexLabelKey] == "0" {
			group.leader = pod
		}
	}

	groups := make([]replicaGroup, 0, len(byIndex))
	for _, group := range byIndex {
		if group.leader != nil {
			groups = append(groups, *group)
		}
	}
	slices.SortFunc(groups, func(a, b replicaGroup) int { return a.index - b.index })
	return groups, nil
}

func (r *SubRoleAssignmentReconciler) Observe(ctx context.Context, namespace, lwsName string, validSubRoles map[string]bool) (SubRoleAssignmentSummary, error) {
	groups, err := r.listGroups(ctx, namespace, lwsName)
	if err != nil {
		return SubRoleAssignmentSummary{}, err
	}
	return summarizeGroups(groups, validSubRoles), nil
}

// Reconcile keeps valid assignments where possible and fills remaining target
// deficits in API order. It patches only Pods whose effective assignment differs.
func (r *SubRoleAssignmentReconciler) Reconcile(ctx context.Context, namespace, lwsName string, order []string, desired map[string]int) (bool, SubRoleAssignmentSummary, error) {
	groups, err := r.listGroups(ctx, namespace, lwsName)
	if err != nil {
		return false, SubRoleAssignmentSummary{}, err
	}
	assignments := allocateSubRoles(groups, order, desired)

	changed := false
	for _, group := range groups {
		groupChanged, err := r.patchGroup(ctx, group, assignments[group.index])
		if err != nil {
			return changed, SubRoleAssignmentSummary{}, err
		}
		changed = changed || groupChanged
	}
	return changed, summarizeAssignments(groups, assignments), nil
}

// allocateSubRoles is the pure assignment step. Existing valid labels are kept
// up to their desired counts; remaining groups fill the largest deficit, with
// API order and then group ordinal providing deterministic tie-breaking.
func allocateSubRoles(groups []replicaGroup, order []string, desired map[string]int) map[int]string {
	valid := make(map[string]bool, len(order))
	for _, name := range order {
		valid[name] = true
	}

	assigned := make(map[string]int, len(order))
	result := make(map[int]string, len(groups))
	available := make([]replicaGroup, 0, len(groups))
	for _, group := range groups {
		name := group.leader.Labels[disaggregatedsetv1.SubRoleLabelKey]
		if valid[name] && assigned[name] < desired[name] {
			result[group.index] = name
			assigned[name]++
		} else {
			available = append(available, group)
		}
	}

	for _, group := range available {
		name := largestSubRoleDeficit(order, desired, assigned)
		if name == "" {
			// A caller normally fits desired to the physical group count. During
			// observation races, preserve a valid label instead of adding churn.
			current := group.leader.Labels[disaggregatedsetv1.SubRoleLabelKey]
			if valid[current] {
				name = current
			} else if len(order) > 0 {
				name = order[0]
			}
		}
		result[group.index] = name
		if name != "" {
			assigned[name]++
		}
	}
	return result
}

// PrepareScaleDown arranges the assignment multiset so the low ordinals kept
// by StatefulSet match retained and the high ordinals are the intended victims.
// A changed result must be observed on a later reconcile before replicas shrink.
func (r *SubRoleAssignmentReconciler) PrepareScaleDown(ctx context.Context, namespace, lwsName string, order []string, retained map[string]int) (bool, error) {
	groups, err := r.listGroups(ctx, namespace, lwsName)
	if err != nil {
		return false, err
	}
	retainTotal := sumSubRoleCounts(retained)
	if retainTotal >= len(groups) {
		return false, nil
	}
	for ordinal, group := range groups {
		if group.index != ordinal {
			return false, fmt.Errorf("cannot prepare scale-down for LWS %s: expected group ordinal %d, observed %d", lwsName, ordinal, group.index)
		}
	}

	available := make(map[string]int, len(order))
	for _, group := range groups {
		available[group.leader.Labels[disaggregatedsetv1.SubRoleLabelKey]]++
	}
	for name, count := range retained {
		if available[name] < count {
			return false, fmt.Errorf("cannot retain %d groups for sub-role %s in LWS %s: only %d assigned", count, name, lwsName, available[name])
		}
	}

	desiredByGroup := make(map[int]string, len(groups))
	usedRetained := make(map[string]int, len(order))
	for i := 0; i < retainTotal; i++ {
		name := groups[i].leader.Labels[disaggregatedsetv1.SubRoleLabelKey]
		if usedRetained[name] < retained[name] {
			desiredByGroup[groups[i].index] = name
			usedRetained[name]++
		}
	}
	for i := 0; i < retainTotal; i++ {
		if desiredByGroup[groups[i].index] != "" {
			continue
		}
		name := largestSubRoleDeficit(order, retained, usedRetained)
		desiredByGroup[groups[i].index] = name
		usedRetained[name]++
	}

	remaining := make(map[string]int, len(available))
	for name, count := range available {
		remaining[name] = count - usedRetained[name]
	}
	for i := retainTotal; i < len(groups); i++ {
		current := groups[i].leader.Labels[disaggregatedsetv1.SubRoleLabelKey]
		if remaining[current] > 0 {
			desiredByGroup[groups[i].index] = current
			remaining[current]--
			continue
		}
		for _, name := range order {
			if remaining[name] > 0 {
				desiredByGroup[groups[i].index] = name
				remaining[name]--
				break
			}
		}
	}

	changed := false
	for _, group := range groups {
		groupChanged, err := r.patchGroup(ctx, group, desiredByGroup[group.index])
		if err != nil {
			return changed, err
		}
		changed = changed || groupChanged
	}
	return changed, nil
}

func (r *SubRoleAssignmentReconciler) patchGroup(ctx context.Context, group replicaGroup, desired string) (bool, error) {
	changed := false
	pods := slices.Clone(group.pods)
	slices.SortStableFunc(pods, func(a, b *corev1.Pod) int { return workerIndex(a) - workerIndex(b) })
	for _, pod := range pods {
		if pod.Labels[disaggregatedsetv1.SubRoleLabelKey] == desired {
			continue
		}
		before := pod.DeepCopy()
		if pod.Labels == nil {
			pod.Labels = make(map[string]string)
		}
		if desired == "" {
			delete(pod.Labels, disaggregatedsetv1.SubRoleLabelKey)
		} else {
			pod.Labels[disaggregatedsetv1.SubRoleLabelKey] = desired
		}
		if err := r.client.Patch(ctx, pod, client.MergeFrom(before)); err != nil && !apierrors.IsNotFound(err) {
			return changed, fmt.Errorf("patch Pod %s sub-role assignment: %w", pod.Name, err)
		}
		changed = true
	}
	return changed, nil
}

func summarizeGroups(groups []replicaGroup, valid map[string]bool) SubRoleAssignmentSummary {
	summary := SubRoleAssignmentSummary{Replicas: make(map[string]int), ReadyReplicas: make(map[string]int)}
	for _, group := range groups {
		summary.GroupIndexes = append(summary.GroupIndexes, group.index)
		name := group.leader.Labels[disaggregatedsetv1.SubRoleLabelKey]
		if !valid[name] {
			summary.Unassigned++
			continue
		}
		summary.Replicas[name]++
		if podReady(group.leader) {
			summary.ReadyReplicas[name]++
		}
	}
	return summary
}

func summarizeAssignments(groups []replicaGroup, assignments map[int]string) SubRoleAssignmentSummary {
	summary := SubRoleAssignmentSummary{Replicas: make(map[string]int), ReadyReplicas: make(map[string]int)}
	for _, group := range groups {
		summary.GroupIndexes = append(summary.GroupIndexes, group.index)
		name := assignments[group.index]
		if name == "" {
			summary.Unassigned++
			continue
		}
		summary.Replicas[name]++
		if podReady(group.leader) {
			summary.ReadyReplicas[name]++
		}
	}
	return summary
}

func largestSubRoleDeficit(order []string, desired, assigned map[string]int) string {
	best := ""
	bestDeficit := 0
	for _, name := range order {
		if deficit := desired[name] - assigned[name]; deficit > bestDeficit {
			best, bestDeficit = name, deficit
		}
	}
	return best
}

// fitSubRoleTargets returns the deterministic prefix of the final assignment
// vector for total physical groups.
func fitSubRoleTargets(order []string, final map[string]int, total int) map[string]int {
	result := make(map[string]int, len(order))
	for range max(total, 0) {
		name := largestSubRoleDeficit(order, final, result)
		if name == "" {
			if len(order) == 0 {
				break
			}
			name = order[0]
		}
		result[name]++
	}
	return result
}

func sumSubRoleCounts(counts map[string]int) int {
	total := 0
	for _, count := range counts {
		total += count
	}
	return total
}

func hasExpectedGroupOrdinals(summary SubRoleAssignmentSummary, replicas int) bool {
	if len(summary.GroupIndexes) != replicas {
		return false
	}
	for ordinal, index := range summary.GroupIndexes {
		if index != ordinal {
			return false
		}
	}
	return true
}

func podReady(pod *corev1.Pod) bool {
	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.PodReady {
			return condition.Status == corev1.ConditionTrue
		}
	}
	return false
}

func workerIndex(pod *corev1.Pod) int {
	index, _ := strconv.Atoi(pod.Labels[leaderworkersetv1.WorkerIndexLabelKey])
	return index
}

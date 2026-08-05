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
	leaderworkersetv1 "sigs.k8s.io/lws/api/leaderworkerset/v1"

	disaggregatedsetv1 "sigs.k8s.io/lws/api/disaggregatedset/v1"
)

type replicaGroup struct {
	index  int
	leader *corev1.Pod
	pods   []*corev1.Pod
}

// AssignmentSummary contains leader-based observed counts for one LWS.
type AssignmentSummary struct {
	Replicas      map[string]int
	ReadyReplicas map[string]int
	Unassigned    int
	GroupIndexes  []int
}

// AssignmentManager maintains the controller-owned sub-role label on every
// Pod in an LWS replica group.
type AssignmentManager struct {
	client client.Client
}

func NewAssignmentManager(c client.Client) *AssignmentManager {
	return &AssignmentManager{client: c}
}

func (m *AssignmentManager) listGroups(ctx context.Context, namespace, lwsName string) ([]replicaGroup, error) {
	list := &corev1.PodList{}
	if err := m.client.List(ctx, list, client.InNamespace(namespace), client.MatchingLabels{
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

// Observe returns leader-based assignment counts without changing Pods.
func (m *AssignmentManager) Observe(ctx context.Context, namespace, lwsName string, validSubRoles map[string]bool) (AssignmentSummary, error) {
	groups, err := m.listGroups(ctx, namespace, lwsName)
	if err != nil {
		return AssignmentSummary{}, err
	}
	return summarizeGroups(groups, validSubRoles), nil
}

func summarizeGroups(groups []replicaGroup, validSubRoles map[string]bool) AssignmentSummary {
	summary := AssignmentSummary{
		Replicas:      make(map[string]int),
		ReadyReplicas: make(map[string]int),
	}
	for _, group := range groups {
		summary.GroupIndexes = append(summary.GroupIndexes, group.index)
		subRole := group.leader.Labels[disaggregatedsetv1.SubRoleLabelKey]
		if !validSubRoles[subRole] {
			summary.Unassigned++
			continue
		}
		summary.Replicas[subRole]++
		if podReady(group.leader) {
			summary.ReadyReplicas[subRole]++
		}
	}
	return summary
}

// Reconcile assigns every observed group to one sub-role. Valid assignments
// are kept up to desired counts; surplus and unassigned groups fill the largest
// remaining deficit, with spec order as the deterministic tie-breaker.
func (m *AssignmentManager) Reconcile(ctx context.Context, namespace, lwsName string, subRoleOrder []string, desired map[string]int) (bool, AssignmentSummary, error) {
	groups, err := m.listGroups(ctx, namespace, lwsName)
	if err != nil {
		return false, AssignmentSummary{}, err
	}
	valid := make(map[string]bool, len(subRoleOrder))
	for _, name := range subRoleOrder {
		valid[name] = true
	}

	assigned := make(map[string]int, len(subRoleOrder))
	desiredByGroup := make(map[int]string, len(groups))
	available := make([]replicaGroup, 0)
	for _, group := range groups {
		name := group.leader.Labels[disaggregatedsetv1.SubRoleLabelKey]
		if valid[name] && assigned[name] < desired[name] {
			desiredByGroup[group.index] = name
			assigned[name]++
		} else {
			available = append(available, group)
		}
	}

	for _, group := range available {
		name := largestDeficit(subRoleOrder, desired, assigned)
		if name == "" {
			// The caller normally fits desired to the physical group count. If
			// there is temporary excess, preserve a valid assignment rather than
			// causing avoidable routing churn.
			current := group.leader.Labels[disaggregatedsetv1.SubRoleLabelKey]
			if valid[current] {
				name = current
			} else if len(subRoleOrder) > 0 {
				name = subRoleOrder[0]
			}
		}
		desiredByGroup[group.index] = name
		assigned[name]++
	}

	changed := false
	for _, group := range groups {
		groupChanged, err := m.patchGroup(ctx, group, desiredByGroup[group.index])
		if err != nil {
			return changed, AssignmentSummary{}, err
		}
		changed = changed || groupChanged
	}
	return changed, summarizeDesired(groups, desiredByGroup), nil
}

// PrepareScaleDown moves the assignments that must survive into the low
// ordinals retained by StatefulSet/LWS scale-down. It preserves the overall
// assignment multiset, so this step is a pure label swap.
func (m *AssignmentManager) PrepareScaleDown(ctx context.Context, namespace, lwsName string, subRoleOrder []string, retained map[string]int) (bool, error) {
	groups, err := m.listGroups(ctx, namespace, lwsName)
	if err != nil {
		return false, err
	}
	retainTotal := 0
	for _, count := range retained {
		retainTotal += count
	}
	if retainTotal >= len(groups) {
		return false, nil
	}
	for ordinal, group := range groups {
		if group.index != ordinal {
			return false, fmt.Errorf("cannot prepare scale-down for LWS %s: expected group ordinal %d, observed %d", lwsName, ordinal, group.index)
		}
	}

	available := make(map[string]int, len(subRoleOrder))
	for _, group := range groups {
		name := group.leader.Labels[disaggregatedsetv1.SubRoleLabelKey]
		available[name]++
	}
	for name, count := range retained {
		if available[name] < count {
			return false, fmt.Errorf("cannot retain %d groups for sub-role %s in LWS %s: only %d assigned", count, name, lwsName, available[name])
		}
	}

	desiredByGroup := make(map[int]string, len(groups))
	usedRetained := make(map[string]int, len(subRoleOrder))
	// Preserve low-ordinal labels when they fit the retained target.
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
		name := largestDeficit(subRoleOrder, retained, usedRetained)
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
		for _, name := range subRoleOrder {
			if remaining[name] > 0 {
				desiredByGroup[groups[i].index] = name
				remaining[name]--
				break
			}
		}
	}

	changed := false
	for _, group := range groups {
		groupChanged, err := m.patchGroup(ctx, group, desiredByGroup[group.index])
		if err != nil {
			return changed, err
		}
		changed = changed || groupChanged
	}
	return changed, nil
}

func (m *AssignmentManager) patchGroup(ctx context.Context, group replicaGroup, desired string) (bool, error) {
	changed := false
	// Patch the leader first; it is the authoritative assignment.
	pods := slices.Clone(group.pods)
	slices.SortStableFunc(pods, func(a, b *corev1.Pod) int {
		return workerIndex(a) - workerIndex(b)
	})
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
		if err := m.client.Patch(ctx, pod, client.MergeFrom(before)); err != nil && !apierrors.IsNotFound(err) {
			return changed, fmt.Errorf("patch Pod %s sub-role assignment: %w", pod.Name, err)
		}
		changed = true
	}
	return changed, nil
}

func largestDeficit(order []string, desired, assigned map[string]int) string {
	best := ""
	bestDeficit := 0
	for _, name := range order {
		deficit := desired[name] - assigned[name]
		if deficit > bestDeficit {
			best, bestDeficit = name, deficit
		}
	}
	return best
}

func summarizeDesired(groups []replicaGroup, desiredByGroup map[int]string) AssignmentSummary {
	summary := AssignmentSummary{Replicas: make(map[string]int), ReadyReplicas: make(map[string]int)}
	for _, group := range groups {
		summary.GroupIndexes = append(summary.GroupIndexes, group.index)
		name := desiredByGroup[group.index]
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

func hasExpectedGroupOrdinals(summary AssignmentSummary, replicas int) bool {
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

// fitSubRoleTargets truncates final targets to a physical LWS replica count
// using the same largest-deficit and spec-order rules as assignment.
func fitSubRoleTargets(order []string, final map[string]int, total int) map[string]int {
	result := make(map[string]int, len(order))
	for range max(total, 0) {
		name := largestDeficit(order, final, result)
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

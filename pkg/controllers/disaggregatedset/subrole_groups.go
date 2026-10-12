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
	"encoding/json"
	"fmt"
	"maps"
	"math"
	"slices"
	"strconv"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	disaggv1 "sigs.k8s.io/lws/api/disaggregatedset/v1"
	leaderv1 "sigs.k8s.io/lws/api/leaderworkerset/v1"
	"sigs.k8s.io/lws/pkg/replicagroups"
	disaggutils "sigs.k8s.io/lws/pkg/utils/disaggregatedset"
)

const (
	subRoleReplicasAnnotation = "disaggregatedset.x-k8s.io/subrole-replicas"
	subRolePlanAnnotation     = "disaggregatedset.x-k8s.io/subrole-plan"
)

func subRoleCounts(lws *leaderv1.LeaderWorkerSet, key string) (map[string]int, error) {
	if lws.Annotations[key] == "" {
		return nil, nil
	}
	var counts map[string]int
	if err := json.Unmarshal([]byte(lws.Annotations[key]), &counts); err != nil {
		return nil, fmt.Errorf("invalid %s: %w", key, err)
	}
	if counts == nil {
		return nil, fmt.Errorf("%s must contain a count map", key)
	}
	if err := validateSubRoleCounts(counts); err != nil {
		return nil, err
	}
	if key == subRoleReplicasAnnotation && countSubRoles(counts) != int(getLWSReplicas(lws)) {
		return nil, fmt.Errorf("issued sub-role counts must sum to physical replicas")
	}
	return counts, nil
}

func validateSubRoleCounts(counts map[string]int) error {
	total := 0
	for name, count := range counts {
		if count < 0 || count > math.MaxInt32-total || name == "" && len(counts) != 1 {
			return fmt.Errorf("invalid sub-role replica counts %v", counts)
		}
		total += count
	}
	return nil
}

func countSubRoles(counts map[string]int) int {
	total := 0
	for _, count := range counts {
		total += count
	}
	return total
}

func setSubRoleJSON(lws *leaderv1.LeaderWorkerSet, key string, counts map[string]int) {
	if lws.Annotations == nil {
		lws.Annotations = map[string]string{}
	}
	value, _ := json.Marshal(counts)
	lws.Annotations[key] = string(value)
}

func fitSubRoles(counts map[string]int, total int) map[string]int {
	result := maps.Clone(counts)
	names := slices.Sorted(maps.Keys(counts))
	for _, name := range names {
		result[name] = min(counts[name], total)
		total -= result[name]
	}
	if len(names) > 0 {
		result[names[0]] += total
	}
	return result
}

func groupSubRole(group replicagroups.Group) (string, bool) {
	name := group.Leader.Labels[disaggv1.SubRoleLabelKey]
	for _, pod := range group.Pods {
		if pod.Labels[disaggv1.SubRoleLabelKey] != name {
			return name, false
		}
	}
	return name, true
}

func activeSubRoleGroups(s *replicagroups.Snapshot) []replicagroups.Group {
	return slices.DeleteFunc(slices.Clone(s.Groups), func(group replicagroups.Group) bool {
		leader := group.Leader
		return !leader.DeletionTimestamp.IsZero() || leader.Status.Phase == corev1.PodFailed || leader.Status.Phase == corev1.PodSucceeded
	})
}

// Select actual ordinal slots: a hole never pulls a higher group into the prefix.
func subRolePrefix(s *replicagroups.Snapshot, count int) (replicagroups.Snapshot, bool) {
	start := 0
	if sts := s.LeaderStatefulSet; sts != nil && sts.Spec.Ordinals != nil {
		start = int(sts.Spec.Ordinals.Start)
	}
	prefix := *s
	prefix.Groups = nil
	complete := true
	for _, group := range s.Groups {
		if group.Ordinal >= start && group.Ordinal < start+count {
			complete = complete && group.Ordinal == start+len(prefix.Groups)
			prefix.Groups = append(prefix.Groups, group)
		}
	}
	return prefix, complete && len(prefix.Groups) == count
}

// Keep known leader assignments up to each target, then fill deficits in name order.
func stickySubRoleAssignments(groups []replicagroups.Group, target map[string]int) map[types.UID]string {
	remaining := maps.Clone(target)
	assignments := map[types.UID]string{}
	var unassigned []types.UID
	for _, group := range groups {
		name := group.Leader.Labels[disaggv1.SubRoleLabelKey]
		if remaining[name] == 0 {
			unassigned = append(unassigned, group.Leader.UID)
			continue
		}
		assignments[group.Leader.UID] = name
		remaining[name]--
	}
	for _, name := range slices.Sorted(maps.Keys(remaining)) {
		for remaining[name] > 0 && len(unassigned) > 0 {
			assignments[unassigned[0]] = name
			remaining[name]--
			unassigned = unassigned[1:]
		}
	}
	return assignments
}

func subRoleNativeSettled(s *replicagroups.Snapshot) bool {
	sts := s.LeaderStatefulSet
	return sts != nil && s.LWS.DeletionTimestamp.IsZero() && s.LWS.Status.ObservedGeneration >= s.LWS.Generation &&
		sts.DeletionTimestamp.IsZero() && sts.Status.ObservedGeneration >= sts.Generation &&
		ptr.Deref(sts.Spec.Replicas, 1) == getLWSReplicas(s.LWS)
}

// Metadata intent must match too: annotation changes do not bump generation.
func (m *LeaderWorkerSetManager) checkSubRoleLWS(ctx context.Context, expected *leaderv1.LeaderWorkerSet) error {
	live := &leaderv1.LeaderWorkerSet{}
	if err := m.apiReader.Get(ctx, client.ObjectKeyFromObject(expected), live); err != nil {
		return err
	}
	if expected.UID == "" || live.UID != expected.UID || live.ResourceVersion != expected.ResourceVersion ||
		!live.DeletionTimestamp.IsZero() || metav1.GetControllerOf(live) == nil {
		return errReplicaGroupsPending
	}
	return nil
}

func (m *LeaderWorkerSetManager) labelSubRoleGroup(ctx context.Context, lws *leaderv1.LeaderWorkerSet, group replicagroups.Group, name string) error {
	// Workers first, leader last. A partial write supplies neither child's Ready credit.
	pods := append(slices.Clone(group.Pods[1:]), group.Leader)
	for _, pod := range pods {
		if value, exists := pod.Labels[disaggv1.SubRoleLabelKey]; value == name && (name != "" || !exists) {
			continue
		}
		if err := m.checkSubRoleLWS(ctx, lws); err != nil {
			return err
		}
		labels := mergeLabels(pod.Labels, map[string]string{disaggv1.SubRoleLabelKey: name})
		if name == "" {
			delete(labels, disaggv1.SubRoleLabelKey)
		}
		patch, _ := json.Marshal([]map[string]any{
			{"op": "test", "path": "/metadata/uid", "value": pod.UID},
			{"op": "test", "path": "/metadata/resourceVersion", "value": pod.ResourceVersion},
			{"op": "add", "path": "/metadata/labels", "value": labels},
		})
		if err := m.client.Patch(ctx, pod.DeepCopy(), client.RawPatch(types.JSONPatchType, patch)); err != nil {
			return err
		}
	}
	return nil
}

// Each update is independent: nil preserves membership, accepts no scale, or
// freezes history. An explicitly empty membership removes child routing.
type subRoleUpdate struct {
	membership, target, initial map[string]int
}

type subRoleState struct {
	lws                      *leaderv1.LeaderWorkerSet
	snapshot                 *replicagroups.Snapshot
	issued, initial, pending map[string]int
}

func (m *LeaderWorkerSetManager) syncSubRoles(ctx context.Context, ds *disaggv1.DisaggregatedSet, lws *leaderv1.LeaderWorkerSet, update subRoleUpdate) (*subRoleState, error) {
	if len(update.membership)+len(update.target)+len(update.initial) == 0 && lws.Annotations[subRoleReplicasAnnotation] == "" && lws.Annotations[disaggv1.InitialSubRoleReplicasAnnotationKey] == "" && lws.Annotations[subRolePlanAnnotation] == "" {
		return nil, nil
	}
	state := &subRoleState{lws: lws}
	if !metav1.IsControlledBy(lws, ds) {
		return state, fmt.Errorf("LeaderWorkerSet %s is not owned by DisaggregatedSet %s", lws.Name, ds.Name)
	}
	var err error
	if state.issued, err = subRoleCounts(lws, subRoleReplicasAnnotation); err != nil {
		return state, err
	}
	if state.pending, err = subRoleCounts(lws, subRolePlanAnnotation); err != nil {
		return state, err
	}
	// Accepted assignments finish even if baseline metadata or new requests are invalid.
	if state.pending != nil {
		return state, m.syncSubRoleAssignments(ctx, state)
	}
	if state.initial, err = subRoleCounts(lws, disaggv1.InitialSubRoleReplicasAnnotationKey); err != nil {
		return state, err
	}
	membership := update.membership
	if membership == nil {
		membership = state.issued
		if len(membership) == 0 {
			membership = state.initial
		}
	}
	for _, counts := range []map[string]int{membership, update.target, update.initial} {
		if err := validateSubRoleCounts(counts); err != nil {
			return state, err
		}
	}
	if len(membership) == 0 {
		state.pending = map[string]int{"": int(getLWSReplicas(lws))}
		return state, m.saveSubRoles(ctx, state)
	}
	baseline := state.issued
	if len(state.issued) == 0 {
		state.issued = fitSubRoles(membership, int(getLWSReplicas(lws)))
		baseline = membership // Even a zero-sized bootstrap retains every intended child.
	}
	if len(state.initial) == 0 {
		total, _ := disaggutils.GetInitialReplicas(lws)
		state.initial = fitSubRoles(baseline, max(int(total), int(getLWSReplicas(lws))))
	}
	for child := range membership {
		if _, exists := state.issued[child]; !exists {
			state.issued[child] = 0
		}
	}
	if update.initial != nil {
		state.initial = update.initial
	}
	if err := m.saveSubRoles(ctx, state); err != nil {
		return state, err // Observe metadata publication before assignment or planning.
	}
	if err := m.syncSubRoleAssignments(ctx, state); err != nil || update.target == nil {
		return state, err
	}
	target, decreasing := maps.Clone(update.target), false
	for name, count := range state.issued {
		decreasing = decreasing || target[name] < count
		if _, found := target[name]; !found {
			target[name] = 0
		}
	}
	if decreasing {
		state.pending = target
	} else {
		state.issued = target
	}
	return state, m.saveSubRoles(ctx, state)
}

// One conditional write owns all durable child state and the matching parent sums.
// A write returns Pending so subsequent decisions use its observed result.
func (m *LeaderWorkerSetManager) saveSubRoles(ctx context.Context, state *subRoleState) error {
	live := state.lws.DeepCopy()
	for key, counts := range map[string]map[string]int{subRoleReplicasAnnotation: state.issued, subRolePlanAnnotation: state.pending} {
		delete(live.Annotations, key)
		if counts != nil {
			setSubRoleJSON(live, key, counts)
		}
	}
	if state.issued != nil {
		live.Spec.Replicas = ptr.To(int32(countSubRoles(state.issued)))
	}
	if state.initial != nil {
		setSubRoleJSON(live, disaggv1.InitialSubRoleReplicasAnnotationKey, state.initial)
		live.Annotations[disaggv1.InitialReplicasAnnotationKey] = strconv.Itoa(countSubRoles(state.initial))
	} else if state.issued == nil && state.pending == nil {
		delete(live.Annotations, disaggv1.InitialSubRoleReplicasAnnotationKey)
	}
	if maps.Equal(live.Annotations, state.lws.Annotations) && getLWSReplicas(live) == getLWSReplicas(state.lws) {
		return nil
	}
	if live.UID == "" || !live.DeletionTimestamp.IsZero() || metav1.GetControllerOf(live) == nil {
		return errReplicaGroupsPending
	}
	before := state.lws.DeepCopy()
	before.UID = "" // Include UID and resourceVersion to reject replacement and concurrent intent.
	if err := m.client.Patch(ctx, live, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
		return err
	}
	return errReplicaGroupsPending
}

func (m *LeaderWorkerSetManager) syncSubRoleAssignments(ctx context.Context, state *subRoleState) error {
	s, err := replicagroups.Observe(ctx, m.apiReader, state.lws)
	if err != nil {
		return err
	}
	if s == nil {
		return errReplicaGroupsPending
	}
	if err := m.checkSubRoleLWS(ctx, state.lws); err != nil {
		return err
	}
	state.snapshot = s
	if s.LWS.Spec.GroupIdentity == leaderv1.GroupIdentityHash {
		return fmt.Errorf("sub-role assignment requires Ordinal identity")
	}
	issued, pending := state.issued, state.pending
	target := pending
	if target == nil {
		target = issued
	}
	_, removing := target[""]
	survivors := min(int(getLWSReplicas(s.LWS)), countSubRoles(target))
	prefix, complete := subRolePrefix(s, survivors)
	assignments := stickySubRoleAssignments(prefix.Groups, target)
	if removing {
		clear(assignments)
		prefix.Groups = nil
	}
	// Partial writes and newborn labels must finish before application health can gate progress.
	for _, group := range s.Groups {
		name, coherent := groupSubRole(group)
		assignment, retained := assignments[group.Leader.UID]
		if !removing && (!coherent || retained && name == "") {
			if err := m.labelSubRoleGroup(ctx, s.LWS, group, assignment); err != nil {
				return err
			}
			return errReplicaGroupsPending
		}
	}
	healthy := complete && int(prefix.Availability().RetainedReadyReplicas) == survivors
	if pending != nil && !removing && !healthy {
		return errReplicaGroupsPending
	}
	// Move retained groups only with a fully healthy prefix. Clear all outgoing
	// routing before publishing the physical target in this same pass.
	for _, group := range s.Groups {
		assignment, retained := assignments[group.Leader.UID]
		if retained && group.Leader.Labels[disaggv1.SubRoleLabelKey] == assignment {
			continue
		}
		if retained && !healthy {
			return errReplicaGroupsPending
		}
		if err := m.labelSubRoleGroup(ctx, s.LWS, group, assignment); err != nil {
			return err
		}
		if retained {
			return errReplicaGroupsPending
		}
	}
	if pending != nil {
		state.issued, state.pending = target, nil
		if removing {
			state.issued, state.initial = nil, nil
		}
		return m.saveSubRoles(ctx, state)
	}
	if !subRoleNativeSettled(s) {
		return errReplicaGroupsPending
	}
	return nil
}

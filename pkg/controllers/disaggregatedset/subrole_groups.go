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
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
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
	subRoleReplicasAnnotation             = "disaggregatedset.x-k8s.io/subrole-replicas"
	subRolePlanAnnotation                 = "disaggregatedset.x-k8s.io/subrole-plan"
	subRoleOriginalDeletionCostAnnotation = "disaggregatedset.x-k8s.io/original-deletion-cost"
	// Lowest valid deletion cost; scheduling, phase and readiness still take priority.
	subRoleDeletionCost = "-2147483648" // math.MinInt32
)

// Hash retains the accepted targets and Ready floors across label/scale retries.
// Equal issued and pending counts mean the physical scale has been published.
// Ordinal pending work keeps its existing plain count-map format.
type subRolePlan struct {
	Counts     map[string]int `json:"counts"`
	ReadyFloor map[string]int `json:"readyFloor"`
}

func readSubRolePlan(lws *leaderv1.LeaderWorkerSet) (*subRolePlan, error) {
	value := lws.Annotations[subRolePlanAnnotation]
	if value == "" || lws.Spec.GroupIdentity != leaderv1.GroupIdentityHash {
		return nil, nil
	}
	var plan subRolePlan
	if err := json.Unmarshal([]byte(value), &plan); err != nil {
		return nil, fmt.Errorf("invalid %s: %w", subRolePlanAnnotation, err)
	}
	if plan.Counts == nil || plan.ReadyFloor == nil {
		return nil, fmt.Errorf("Hash sub-role plan requires counts and Ready floors")
	}
	if err := validateSubRoleCounts(plan.Counts); err != nil {
		return nil, err
	}
	for child, floor := range plan.ReadyFloor {
		if floor < 0 || floor > plan.Counts[child] {
			return nil, fmt.Errorf("invalid sub-role readiness floor")
		}
	}
	return &plan, nil
}

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

func setSubRoleJSON(lws *leaderv1.LeaderWorkerSet, key string, value any) {
	if lws.Annotations == nil {
		lws.Annotations = map[string]string{}
	}
	encoded, _ := json.Marshal(value)
	lws.Annotations[key] = string(encoded)
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
	if s == nil || s.LWS == nil {
		return false
	}
	if s.LWS.Spec.GroupIdentity == leaderv1.GroupIdentityHash {
		return hashSubRoleNativeAcknowledged(s, false)
	}
	sts := s.LeaderStatefulSet
	return sts != nil && s.LWS.DeletionTimestamp.IsZero() && s.LWS.Status.ObservedGeneration >= s.LWS.Generation &&
		sts.DeletionTimestamp.IsZero() && sts.Status.ObservedGeneration >= sts.Generation &&
		ptr.Deref(sts.Spec.Replicas, 1) == getLWSReplicas(s.LWS)
}

// Metadata intent must match too: annotation changes do not bump generation.
func (m *LeaderWorkerSetManager) checkSubRoleLWS(ctx context.Context, expected *leaderv1.LeaderWorkerSet) error {
	live := &leaderv1.LeaderWorkerSet{}
	if err := m.reader.Get(ctx, client.ObjectKeyFromObject(expected), live); err != nil {
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
	pods = slices.DeleteFunc(pods, func(pod *corev1.Pod) bool {
		value, exists := pod.Labels[disaggv1.SubRoleLabelKey]
		return value == name && (name != "" || !exists)
	})
	for i, pod := range pods {
		// Check once before starting and again before making the group coherent.
		// During repair, the final changed Pod may be a worker, not the leader.
		if i == 0 || i == len(pods)-1 {
			if err := m.checkSubRoleLWS(ctx, lws); err != nil {
				return err
			}
		}
		if pod.UID == "" || pod.ResourceVersion == "" {
			return errReplicaGroupsPending
		}
		var label any = name
		if name == "" {
			label = nil // Merge-patch null removes only our routing label.
		}
		// Keep both preconditions. Unlike a JSON Patch test failure, an outdated
		// resourceVersion produces a distinct Conflict rather than generic Invalid.
		patch, _ := json.Marshal(map[string]any{"metadata": map[string]any{
			"uid": pod.UID, "resourceVersion": pod.ResourceVersion,
			"labels": map[string]any{disaggv1.SubRoleLabelKey: label},
		}})
		if err := m.client.Patch(ctx, pod.DeepCopy(), client.RawPatch(types.MergePatchType, patch)); err != nil {
			cause, _ := apierrors.StatusCause(err, metav1.CauseTypeFieldValueInvalid)
			if apierrors.IsConflict(err) || apierrors.IsInvalid(err) && cause.Field == "metadata.uid" {
				return errReplicaGroupsPending
			}
			return err // Invalid labels and other genuine errors remain visible.
		}
	}
	return nil
}

// Each update is independent: nil preserves membership, accepts no scale, or
// freezes history. An explicitly empty membership removes child routing.
type subRoleUpdate struct {
	membership, target, initial map[string]int
	rolloutStrategy             *leaderv1.RolloutStrategy
}

type subRoleState struct {
	lws                      *leaderv1.LeaderWorkerSet
	snapshot                 *replicagroups.Snapshot
	issued, initial, pending map[string]int
	written                  bool // A conditional LWS write succeeded in this call.
	plan                     *subRolePlan
	rolloutStrategy          *leaderv1.RolloutStrategy
}

func (m *LeaderWorkerSetManager) syncSubRoles(ctx context.Context, ds *disaggv1.DisaggregatedSet, lws *leaderv1.LeaderWorkerSet, update subRoleUpdate) (*subRoleState, error) {
	if len(update.membership)+len(update.target)+len(update.initial) == 0 && lws.Annotations[subRoleReplicasAnnotation] == "" && lws.Annotations[disaggv1.InitialSubRoleReplicasAnnotationKey] == "" && lws.Annotations[subRolePlanAnnotation] == "" {
		return nil, nil
	}
	state := &subRoleState{lws: lws, rolloutStrategy: update.rolloutStrategy}
	if !metav1.IsControlledBy(lws, ds) {
		return state, fmt.Errorf("LeaderWorkerSet %s is not owned by DisaggregatedSet %s", lws.Name, ds.Name)
	}
	var err error
	if state.issued, err = subRoleCounts(lws, subRoleReplicasAnnotation); err != nil {
		return state, err
	}
	if state.plan, err = readSubRolePlan(lws); err != nil {
		return state, err
	}
	if state.plan != nil {
		state.pending = state.plan.Counts
	} else if state.pending, err = subRoleCounts(lws, subRolePlanAnnotation); err != nil {
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
		state.startHashPlan()
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
		state.startHashPlan()
	} else {
		state.issued = target
	}
	return state, m.saveSubRoles(ctx, state)
}

func (state *subRoleState) startHashPlan() {
	if state.lws.Spec.GroupIdentity == leaderv1.GroupIdentityHash {
		floor := map[string]int{}
		if state.snapshot != nil {
			for _, group := range activeSubRoleGroups(state.snapshot) {
				if name, coherent := groupSubRole(group); coherent && retainedSubRoleEligible(group) {
					floor[name]++
				}
			}
			for name, ready := range floor {
				floor[name] = max(0, ready-max(0, state.issued[name]-state.pending[name]))
			}
		}
		state.plan = &subRolePlan{Counts: maps.Clone(state.pending), ReadyFloor: floor}
	}
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
	if state.plan != nil {
		setSubRoleJSON(live, subRolePlanAnnotation, state.plan)
	}
	if state.issued != nil {
		live.Spec.Replicas = ptr.To(int32(countSubRoles(state.issued)))
	}
	setRolloutStrategy(live, state.rolloutStrategy)
	if state.initial != nil {
		setSubRoleJSON(live, disaggv1.InitialSubRoleReplicasAnnotationKey, state.initial)
		live.Annotations[disaggv1.InitialReplicasAnnotationKey] = strconv.Itoa(countSubRoles(state.initial))
	} else if state.issued == nil && state.pending == nil {
		delete(live.Annotations, disaggv1.InitialSubRoleReplicasAnnotationKey)
	}
	if maps.Equal(live.Annotations, state.lws.Annotations) && equality.Semantic.DeepEqual(live.Spec, state.lws.Spec) {
		return nil
	}
	if live.UID == "" || !live.DeletionTimestamp.IsZero() || metav1.GetControllerOf(live) == nil {
		return errReplicaGroupsPending
	}
	before := state.lws.DeepCopy()
	before.UID = "" // Include UID and resourceVersion to reject replacement and concurrent intent.
	if err := m.mutate(ctx, live, false, func(ctx context.Context) error {
		return m.client.Patch(ctx, live, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{}))
	}); err != nil {
		return err
	}
	state.lws, state.written = live, true
	return errReplicaGroupsPending
}

func (m *LeaderWorkerSetManager) syncSubRoleAssignments(ctx context.Context, state *subRoleState) error {
	s, err := replicagroups.Observe(ctx, m.reader, state.lws)
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
		return m.syncHashSubRoleAssignments(ctx, state)
	}
	if m.pending != nil && !s.DeletionsObserved() {
		return errReplicaGroupsPending
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
	labelled := false
	for _, group := range s.Groups {
		name, coherent := groupSubRole(group)
		assignment, retained := assignments[group.Leader.UID]
		if !removing && (!coherent || retained && name == "") {
			if err := m.labelSubRoleGroup(ctx, s.LWS, group, assignment); err != nil {
				return err
			}
			labelled = true
		}
	}
	if labelled {
		return errReplicaGroupsPending
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

// Reserve each Ready floor, then prefer eligible Ready groups for every remaining
// slot. Keep existing child labels within each readiness class, not across them.
func readySubRoleAssignments(groups []replicagroups.Group, counts, floor map[string]int) map[types.UID]string {
	var ready, unready []replicagroups.Group
	for _, group := range groups {
		if retainedSubRoleEligible(group) {
			ready = append(ready, group)
		} else {
			unready = append(unready, group)
		}
	}
	assignments := stickySubRoleAssignments(ready, floor)
	if len(assignments) != countSubRoles(floor) {
		return nil
	}
	remaining := maps.Clone(counts)
	for _, name := range assignments {
		remaining[name]--
	}
	ready = slices.DeleteFunc(ready, func(group replicagroups.Group) bool {
		_, reserved := assignments[group.Leader.UID]
		return reserved
	})
	for _, candidates := range [][]replicagroups.Group{ready, unready} {
		for uid, name := range stickySubRoleAssignments(candidates, remaining) {
			assignments[uid] = name
			remaining[name]--
		}
	}
	return assignments
}

func retainedSubRoleEligible(group replicagroups.Group) bool {
	return group.Ready && !group.Terminating &&
		group.Leader.Annotations[leaderv1.GroupRestartBudgetExhaustedAnnotationKey] != "true" &&
		(group.WorkerStatefulSet == nil || group.WorkerStatefulSet.Status.ObservedGeneration >= group.WorkerStatefulSet.Generation)
}

// Before publishing a scale, acknowledged creation may still be short (e.g. quota).
// Allow cancelling that growth, but do not skip observed deletions or native rollout.
func hashSubRoleNativeAcknowledged(s *replicagroups.Snapshot, allowMissing bool) bool {
	replicas := getLWSReplicas(s.LWS)
	deployment := s.LeaderDeployment
	if !s.LWS.DeletionTimestamp.IsZero() || s.LWS.Status.ObservedGeneration < s.LWS.Generation ||
		deployment == nil || !deployment.DeletionTimestamp.IsZero() ||
		ptr.Deref(deployment.Spec.Replicas, 1) != replicas || deployment.Status.ObservedGeneration < deployment.Generation {
		return false
	}
	active := map[types.UID]int32{}
	for _, group := range activeSubRoleGroups(s) {
		if owner := metav1.GetControllerOf(group.Leader); owner != nil {
			active[owner.UID]++
		}
	}
	total, present, nonzero := int64(0), int32(0), 0
	for _, rs := range s.ReplicaSets {
		n := ptr.Deref(rs.Spec.Replicas, 1)
		if !rs.DeletionTimestamp.IsZero() || rs.Status.ObservedGeneration < rs.Generation ||
			rs.Status.Replicas != active[rs.UID] || active[rs.UID] > n {
			return false
		}
		total += int64(n)
		present += active[rs.UID]
		if n > 0 {
			nonzero++
		}
	}
	if present != replicas && (!allowMissing || slices.ContainsFunc(s.Groups, func(group replicagroups.Group) bool {
		return group.Terminating || group.Leader.Status.Phase == corev1.PodFailed || group.Leader.Status.Phase == corev1.PodSucceeded
	})) {
		return false
	}
	return total == int64(replicas) && nonzero <= 1
}

// ReplicaSet chooses leader Pods for deletion in this order:
//  1. Unscheduled before scheduled.
//  2. Pending before Unknown before Running.
//  3. Not Ready before Ready.
//  4. Lower deletion cost before higher cost, then other tie-breakers.
//
// This is intentional: keep scheduled, healthy leaders before honoring our
// sub-role preference. These checks concern the leader, not the whole group.
// Cost is best-effort within one ReplicaSet, whose cache may lag our writes;
// syncHashSubRoleAssignments repairs actual survivors instead of deleting Pods.
// See https://github.com/kubernetes/kubernetes/blob/f78e722310e50bcaca9276be22276d9e91d91308/pkg/controller/controller_utils.go#L843.
// Save the previous cost on the same Pod so retries and restarts can restore it.
// The backup uses "none" for absence and "gated" for the native temporary -100;
// numeric values are restored verbatim. Cleanup leaves newer external edits untouched.
func (m *LeaderWorkerSetManager) preferSubRoleDeletion(ctx context.Context, group replicagroups.Group, prefer bool) (bool, error) {
	pod := group.Leader
	desired := pod.DeepCopy()
	cost, hasCost := pod.Annotations[corev1.PodDeletionCost]
	previous, overridden := pod.Annotations[subRoleOriginalDeletionCostAnnotation]
	gated := slices.ContainsFunc(pod.Spec.SchedulingGates, func(g corev1.PodSchedulingGate) bool { return g.Name == leaderv1.GroupReplacementSchedulingGate })
	if prefer {
		if !overridden || cost != subRoleDeletionCost {
			previous = cost
			if !hasCost {
				previous = "none"
			} else if cost == "-100" && gated {
				previous = "gated"
			}
		}
		desired.Annotations = mergeLabels(desired.Annotations, map[string]string{
			corev1.PodDeletionCost: subRoleDeletionCost, subRoleOriginalDeletionCostAnnotation: previous,
		})
	} else if overridden {
		delete(desired.Annotations, subRoleOriginalDeletionCostAnnotation)
		if cost == subRoleDeletionCost {
			switch previous {
			case "none":
				delete(desired.Annotations, corev1.PodDeletionCost)
			case "gated":
				delete(desired.Annotations, corev1.PodDeletionCost)
				if gated {
					desired.Annotations[corev1.PodDeletionCost] = "-100"
				}
			default:
				desired.Annotations[corev1.PodDeletionCost] = previous
			}
		}
	}
	if maps.Equal(pod.Annotations, desired.Annotations) {
		return false, nil
	}
	before := pod.DeepCopy()
	before.UID = "" // Include UID and resourceVersion to fence replacement/concurrent edits.
	return true, m.client.Patch(ctx, desired, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{}))
}

func (m *LeaderWorkerSetManager) syncHashSubRoleAssignments(ctx context.Context, state *subRoleState) error {
	s := state.snapshot
	target := state.issued
	if state.pending != nil {
		target = state.pending
	}
	preparing := state.pending != nil && !maps.Equal(state.issued, target)
	waiting := !hashSubRoleNativeAcknowledged(s, state.pending == nil || preparing)
	groups := activeSubRoleGroups(s)
	_, removing := target[""]
	assignments := stickySubRoleAssignments(groups, target)
	floorMet := true
	// Restore saved floors after native deletion too, whenever survivors allow it.
	if state.pending != nil && !removing {
		if ready := readySubRoleAssignments(groups, target, state.plan.ReadyFloor); ready != nil {
			assignments = ready
		} else {
			floorMet = false
		}
	}
	// An overlapping native rollout may need newborn readiness to finish. Give
	// its temporary surplus groups routing too, without changing deletion costs.
	startup := assignments
	if waiting {
		counts := maps.Clone(target)
		maps.DeleteFunc(counts, func(_ string, n int) bool { return n == 0 })
		startup = stickySubRoleAssignments(groups, fitSubRoles(counts, len(groups)))
	}
	// Newborn/partial labels may be needed for readiness itself.
	changed := false
	for _, group := range groups {
		name, coherent := groupSubRole(group)
		if assignment, retained := startup[group.Leader.UID]; retained && !removing && (!coherent || name == "") {
			if err := m.labelSubRoleGroup(ctx, s.LWS, group, assignment); err != nil {
				return err
			}
			changed = true
		}
	}
	// Keep costs until native scale completes. Never delete preferred victims
	// ourselves or assume ReplicaSet honored their costs.
	if changed || waiting {
		return errReplicaGroupsPending
	}
	if preparing && (!floorMet ||
		len(assignments) != min(int(getLWSReplicas(s.LWS)), countSubRoles(target))) {
		return errReplicaGroupsPending
	}
	healthy := len(assignments) == countSubRoles(target)
	for _, group := range groups {
		if _, retained := assignments[group.Leader.UID]; retained {
			healthy = healthy && retainedSubRoleEligible(group)
		}
	}
	for _, group := range groups {
		assignment, retained := assignments[group.Leader.UID]
		name, coherent := groupSubRole(group)
		if !retained || name == assignment && coherent {
			continue
		}
		if state.pending == nil && !healthy {
			return errReplicaGroupsPending
		}
		if err := m.labelSubRoleGroup(ctx, s.LWS, group, assignment); err != nil {
			return err
		}
		changed = true
	}
	if changed {
		return errReplicaGroupsPending
	}
	// Parent intent was checked for this pass; each cost patch still checks the Pod.
	for _, group := range groups {
		_, retained := assignments[group.Leader.UID]
		updated, err := m.preferSubRoleDeletion(ctx, group, !retained && !removing)
		if err != nil {
			return err
		}
		changed = changed || updated
	}
	if changed {
		return errReplicaGroupsPending
	}
	if preparing {
		state.issued = maps.Clone(target)
		return m.saveSubRoles(ctx, state)
	}
	// Missing growth must not prevent accepting a lower target. Once published,
	// a pending plan still finishes only at its exact physical count.
	if len(assignments) != len(groups) || state.pending != nil &&
		(len(groups) != countSubRoles(target) || !subRoleNativeSettled(s)) {
		return errReplicaGroupsPending
	}
	if state.pending != nil {
		state.pending, state.plan = nil, nil
		if removing {
			state.issued, state.initial = nil, nil
		}
		return m.saveSubRoles(ctx, state)
	}
	return nil
}

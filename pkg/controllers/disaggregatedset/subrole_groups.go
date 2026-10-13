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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/uuid"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	disaggv1 "sigs.k8s.io/lws/api/disaggregatedset/v1"
	leaderv1 "sigs.k8s.io/lws/api/leaderworkerset/v1"
	"sigs.k8s.io/lws/internal/groupdeletion"
	"sigs.k8s.io/lws/pkg/replicagroups"
	disaggutils "sigs.k8s.io/lws/pkg/utils/disaggregatedset"
)

const (
	subRoleReplicasAnnotation   = "disaggregatedset.x-k8s.io/subrole-replicas"
	subRolePlanAnnotation       = "disaggregatedset.x-k8s.io/subrole-plan"
	legacySubRolePlanAnnotation = "disaggregatedset.x-k8s.io/subrole-scale-plan"
)

func hasPendingSubRolePlan(lws *leaderv1.LeaderWorkerSet) bool {
	return lws.Annotations[subRolePlanAnnotation] != "" || lws.Annotations[legacySubRolePlanAnnotation] != ""
}

// Hash needs an immutable transaction ID before its exact victims can be armed.
// Ordinal pending work remains a plain count map. Hash preserves the accepted
// per-child Ready floors, including when replaying previously serialized plans.
type subRolePlan struct {
	ID         string         `json:"id,omitempty"`
	Armed      bool           `json:"armed,omitempty"`
	Counts     map[string]int `json:"counts"`
	ReadyFloor map[string]int `json:"readyFloor"`
}

func readSubRolePlan(lws *leaderv1.LeaderWorkerSet) (*subRolePlan, error) {
	key, value := subRolePlanAnnotation, lws.Annotations[subRolePlanAnnotation]
	if legacy := lws.Annotations[legacySubRolePlanAnnotation]; legacy != "" {
		if value != "" && value != legacy {
			return nil, fmt.Errorf("conflicting sub-role plan annotations")
		}
		if value == "" {
			key, value = legacySubRolePlanAnnotation, legacy
		}
	}
	if value == "" {
		return nil, nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(value), &fields); err != nil {
		return nil, fmt.Errorf("invalid %s: %w", key, err)
	}
	counts := fields["counts"]
	if len(counts) == 0 || counts[0] != '{' {
		if key == legacySubRolePlanAnnotation || lws.Spec.GroupIdentity == leaderv1.GroupIdentityHash {
			return nil, fmt.Errorf("Hash sub-role plan requires transaction identity and counts")
		}
		return nil, nil // Published Ordinal count-map format.
	}
	var plan subRolePlan
	if err := json.Unmarshal([]byte(value), &plan); err != nil {
		return nil, fmt.Errorf("invalid %s: %w", key, err)
	}
	if plan.Counts == nil || lws.Spec.GroupIdentity == leaderv1.GroupIdentityHash && plan.ID == "" {
		return nil, fmt.Errorf("incomplete sub-role transaction")
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

// Only cached objects authorize this no-op; actual assignment and scale writes
// still require live observations. A saved Assigned condition is not proof:
// Pods can be replaced or relabeled without changing the DS generation.
func cachedSubRolesMatchDesired(ctx context.Context, reader client.Reader, lws *leaderv1.LeaderWorkerSet, desired map[string]int) bool {
	if hasPendingSubRolePlan(lws) || lws.Annotations[groupdeletion.PlanAnnotation] != "" {
		return false
	}
	for _, key := range []string{subRoleReplicasAnnotation, disaggv1.InitialSubRoleReplicasAnnotationKey} {
		counts, err := subRoleCounts(lws, key)
		if err != nil || !maps.Equal(counts, desired) {
			return false
		}
	}
	s, err := replicagroups.Observe(ctx, reader, lws)
	if err != nil || s == nil || s.LWS.ResourceVersion != lws.ResourceVersion || !subRoleNativeSettled(s) {
		return false
	}
	count := countSubRoles(desired)
	if len(s.Groups) != count {
		return false
	}
	hash := lws.Spec.GroupIdentity == leaderv1.GroupIdentityHash
	if !hash {
		if _, complete := subRolePrefix(s, count); !complete {
			return false
		}
	}
	remaining := maps.Clone(desired)
	for _, group := range s.Groups {
		name, coherent := groupSubRole(group)
		if !coherent || remaining[name] == 0 || !subRoleGroupComplete(group) {
			return false
		}
		if hash && (!subRoleProtected(lws, group) || group.Leader.Annotations[groupdeletion.VictimAnnotation] != "" || group.Leader.Annotations[groupdeletion.ReplacementAnnotation] != "") {
			return false
		}
		remaining[name]--
	}
	return true
}

// Complete means the expected, nonterminal Pods exist and the worker controller
// has observed their target. Pods may still be unscheduled or loading: neither
// scheduling nor readiness changes their sub-role assignment.
func subRoleGroupComplete(group replicagroups.Group) bool {
	size, _ := strconv.Atoi(group.Leader.Annotations[leaderv1.SizeAnnotationKey])
	if group.Terminating || len(group.Pods) != size ||
		group.Leader.Status.Phase == corev1.PodFailed || group.Leader.Status.Phase == corev1.PodSucceeded {
		return false
	}
	workers := group.WorkerStatefulSet
	if size > 1 && (workers == nil || workers.Status.ObservedGeneration < workers.Generation || ptr.Deref(workers.Spec.Replicas, 0) != int32(size-1)) {
		return false
	}
	for i, pod := range group.Pods[1:] {
		if pod.Name != fmt.Sprintf("%s-%d", workers.Name, i+1) || pod.Status.Phase == corev1.PodFailed || pod.Status.Phase == corev1.PodSucceeded {
			return false
		}
	}
	return true
}

func subRoleNativeSettled(s *replicagroups.Snapshot) bool {
	if s == nil || s.LWS == nil {
		return false
	}
	if s.LWS.Spec.GroupIdentity == leaderv1.GroupIdentityHash {
		return subRoleNativeObserved(s, true)
	}
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
	var changed []*corev1.Pod
	for _, pod := range pods {
		desired := pod.DeepCopy()
		desired.Labels = mergeLabels(pod.Labels, map[string]string{disaggv1.SubRoleLabelKey: name})
		if name == "" {
			delete(desired.Labels, disaggv1.SubRoleLabelKey)
		}
		if pod.UID == group.Leader.UID {
			if desired.Annotations == nil {
				desired.Annotations = map[string]string{}
			}
			if name != "" && lws.Spec.GroupIdentity == leaderv1.GroupIdentityHash {
				desired.Annotations[groupdeletion.ProtectionAnnotation] = string(lws.UID) + "/" + string(pod.UID)
			} else {
				delete(desired.Annotations, groupdeletion.ProtectionAnnotation)
			}
			delete(desired.Annotations, groupdeletion.VictimAnnotation)
		}
		if !maps.Equal(desired.Labels, pod.Labels) || !maps.Equal(desired.Annotations, pod.Annotations) {
			changed = append(changed, desired)
		}
	}
	for i, pod := range changed {
		// Check once before starting and again before making the group coherent.
		// During repair, the final changed Pod may be a worker, not the leader.
		if i == 0 || i == len(changed)-1 {
			if err := m.checkSubRoleLWS(ctx, lws); err != nil {
				return err
			}
		}
		patch, _ := json.Marshal([]map[string]any{
			{"op": "test", "path": "/metadata/uid", "value": pod.UID},
			{"op": "test", "path": "/metadata/resourceVersion", "value": pod.ResourceVersion},
			{"op": "add", "path": "/metadata/labels", "value": pod.Labels},
			{"op": "add", "path": "/metadata/annotations", "value": pod.Annotations},
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
	written                  bool // A conditional LWS write succeeded in this call.
	plan                     *subRolePlan
}

func (m *LeaderWorkerSetManager) syncSubRoles(ctx context.Context, ds *disaggv1.DisaggregatedSet, lws *leaderv1.LeaderWorkerSet, update subRoleUpdate) (*subRoleState, error) {
	if len(update.membership)+len(update.target)+len(update.initial) == 0 && lws.Annotations[subRoleReplicasAnnotation] == "" && lws.Annotations[disaggv1.InitialSubRoleReplicasAnnotationKey] == "" && !hasPendingSubRolePlan(lws) {
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
				if name, coherent := groupSubRole(group); coherent && group.Ready {
					floor[name]++
				}
			}
			for name, ready := range floor {
				floor[name] = max(0, ready-max(0, state.issued[name]-state.pending[name]))
			}
		}
		state.plan = &subRolePlan{ID: string(uuid.NewUUID()), Counts: maps.Clone(state.pending), ReadyFloor: floor}
	}
}

// One conditional write owns all durable child state and the matching parent sums.
// A write returns Pending so subsequent decisions use its observed result.
func (m *LeaderWorkerSetManager) saveSubRoles(ctx context.Context, state *subRoleState) error {
	live := state.lws.DeepCopy()
	delete(live.Annotations, legacySubRolePlanAnnotation)
	for key, counts := range map[string]map[string]int{subRoleReplicasAnnotation: state.issued, subRolePlanAnnotation: state.pending} {
		delete(live.Annotations, key)
		if counts != nil {
			setSubRoleJSON(live, key, counts)
		}
	}
	if state.plan != nil {
		setSubRoleJSON(live, subRolePlanAnnotation, state.plan)
	}
	if live.Spec.GroupIdentity == leaderv1.GroupIdentityHash {
		delete(live.Annotations, groupdeletion.PlanAnnotation)
		if state.plan != nil && state.plan.Armed {
			live.Annotations[groupdeletion.PlanAnnotation] = state.plan.ID
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
	state.lws, state.written = live, true
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
		return m.syncHashSubRoleAssignments(ctx, state)
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
	if state.plan != nil && !state.plan.Armed {
		if !subRoleNativeObserved(s, true) {
			return errReplicaGroupsPending
		}
		if !removing {
			assignments = readySubRoleAssignments(prefix.Groups, target, state.plan.ReadyFloor)
			if assignments == nil {
				return errReplicaGroupsPending
			}
		}
	}
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
	if state.plan != nil {
		// Legacy unarmed plans retain their accepted floors, on the actual
		// prefix only. Armed plans already published their physical drain and
		// repair replacement labels without rechecking Ready credit.
		healthy = state.plan.Armed || complete
	}
	if pending != nil && !removing && !healthy {
		return errReplicaGroupsPending
	}
	// New count-map plans require a healthy prefix; legacy object plans keep
	// their accepted floors. Clear outgoing routing before publishing Spec.
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
		state.plan = nil
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

// Hash has no retained ordinal prefix. Before arming, reserve accepted Ready
// floors first; after arming, exact victims can never be promoted to survivors.
// New UIDs fill vacancies only, never enlarge the protected survivor set.
func subRolePlanAssignments(s *replicagroups.Snapshot, plan *subRolePlan) map[types.UID]string {
	target := countSubRoles(plan.Counts)
	groups := activeSubRoleGroups(s)
	var retained, fresh []replicagroups.Group
	if plan.Armed {
		_, removing := plan.Counts[""]
		for _, group := range groups {
			if subRoleVictim(group, plan) {
				continue
			}
			if removing || subRoleProtected(s.LWS, group) {
				retained = append(retained, group)
			} else {
				fresh = append(fresh, group)
			}
		}
		if len(retained) > target {
			return nil
		}
		retained = append(retained, fresh[:min(len(fresh), target-len(retained))]...)
	} else {
		return readySubRoleAssignments(groups, plan.Counts, plan.ReadyFloor)
	}
	return stickySubRoleAssignments(retained, plan.Counts)
}

// Reserve accepted Ready floors before filling count-only slots. Callers own
// the physical candidate set: legacy Ordinal replay passes only its prefix.
func readySubRoleAssignments(groups []replicagroups.Group, counts, floor map[string]int) map[types.UID]string {
	if floor == nil {
		floor = counts // A missing legacy floor must not grant credit.
	}
	var ready []replicagroups.Group
	for _, group := range groups {
		if retainedSubRoleEligible(group) {
			ready = append(ready, group)
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
	var unassigned []replicagroups.Group
	for _, group := range groups {
		if _, reserved := assignments[group.Leader.UID]; !reserved {
			unassigned = append(unassigned, group)
		}
	}
	maps.Copy(assignments, stickySubRoleAssignments(unassigned, remaining))
	return assignments
}

func retainedSubRoleEligible(group replicagroups.Group) bool {
	leader := group.Leader
	return group.Ready && !group.Terminating &&
		leader.Annotations[leaderv1.GroupRestartBudgetExhaustedAnnotationKey] != "true" &&
		leader.Annotations[groupdeletion.ReplacementAnnotation] != string(leader.UID) &&
		(group.WorkerStatefulSet == nil || group.WorkerStatefulSet.Status.ObservedGeneration >= group.WorkerStatefulSet.Generation)
}

func subRoleVictim(group replicagroups.Group, plan *subRolePlan) bool {
	return groupdeletion.IsVictim(group.Leader, plan.ID)
}

func subRoleProtected(lws *leaderv1.LeaderWorkerSet, group replicagroups.Group) bool {
	return lws.UID != "" && groupdeletion.ProtectionOwner(group.Leader) == lws.UID
}

// Native counters acknowledge physical intent; they never supply Ready credit.
// Explicit victim deletion waits for native targets, completion also for counts.
func subRoleNativeTargetsApplied(s *replicagroups.Snapshot) bool {
	return subRoleNativeObserved(s, false)
}

func subRoleNativeObserved(s *replicagroups.Snapshot, acknowledged bool) bool {
	if s == nil || s.LWS == nil || !s.LWS.DeletionTimestamp.IsZero() {
		return false
	}
	replicas := ptr.Deref(s.LWS.Spec.Replicas, 1)
	if s.LWS.Status.ObservedGeneration < s.LWS.Generation {
		return false
	}
	if sts := s.LeaderStatefulSet; sts != nil {
		return sts.DeletionTimestamp.IsZero() && ptr.Deref(sts.Spec.Replicas, 1) == replicas &&
			(!acknowledged || sts.Status.ObservedGeneration >= sts.Generation && sts.Status.Replicas == replicas)
	}
	dep := s.LeaderDeployment
	if dep == nil || !dep.DeletionTimestamp.IsZero() || ptr.Deref(dep.Spec.Replicas, 1) != replicas || dep.Status.ObservedGeneration < dep.Generation {
		return false
	}
	active := map[types.UID]int32{}
	for _, group := range activeSubRoleGroups(s) {
		if owner := metav1.GetControllerOf(group.Leader); owner != nil {
			active[owner.UID]++
		}
	}
	total, nonzero := int64(0), 0
	for _, rs := range s.ReplicaSets {
		n := ptr.Deref(rs.Spec.Replicas, 1)
		if n > 0 {
			nonzero++
		}
		if !rs.DeletionTimestamp.IsZero() || acknowledged &&
			(rs.Status.ObservedGeneration < rs.Generation || rs.Status.Replicas != n || active[rs.UID] != n) {
			return false
		}
		total += int64(n)
	}
	return total == int64(replicas) && nonzero <= 1
}

func (m *LeaderWorkerSetManager) markSubRoleVictim(ctx context.Context, lws *leaderv1.LeaderWorkerSet, group replicagroups.Group, plan *subRolePlan) (bool, error) {
	if subRoleVictim(group, plan) && subRoleProtected(lws, group) {
		return false, nil
	}
	if err := m.checkSubRoleLWS(ctx, lws); err != nil {
		return false, err
	}
	pod := group.Leader
	annotations := mergeLabels(pod.Annotations, map[string]string{
		groupdeletion.ProtectionAnnotation: string(lws.UID) + "/" + string(pod.UID),
		groupdeletion.VictimAnnotation:     plan.ID + "/" + string(pod.UID),
	})
	patch, _ := json.Marshal([]map[string]any{
		{"op": "test", "path": "/metadata/uid", "value": pod.UID},
		{"op": "test", "path": "/metadata/resourceVersion", "value": pod.ResourceVersion},
		{"op": "add", "path": "/metadata/annotations", "value": annotations},
	})
	return true, m.client.Patch(ctx, pod.DeepCopy(), client.RawPatch(types.JSONPatchType, patch))
}

// Only unassigned, unlabelled, unprotected extras may be removed outside the
// accepted victim set. UID/RV guards fence concurrent enrollment or replacement.
func (m *LeaderWorkerSetManager) deleteSubRoleExtras(ctx context.Context, s *replicagroups.Snapshot, assignments map[types.UID]string) (bool, error) {
	if s.LWS.Spec.GroupIdentity != leaderv1.GroupIdentityHash || len(assignments) != int(getLWSReplicas(s.LWS)) || !subRoleNativeTargetsApplied(s) {
		return false, nil
	}
	changed := false
	for _, group := range activeSubRoleGroups(s) {
		if _, assigned := assignments[group.Leader.UID]; assigned || subRoleProtected(s.LWS, group) || group.Leader.Labels[disaggv1.SubRoleLabelKey] != "" {
			continue
		}
		if err := m.checkSubRoleLWS(ctx, s.LWS); err != nil {
			return changed, err
		}
		pod := group.Leader
		if err := m.client.Delete(ctx, pod, client.Preconditions{UID: &pod.UID, ResourceVersion: &pod.ResourceVersion}); err != nil && !apierrors.IsNotFound(err) {
			return changed, err
		}
		changed = true
	}
	return changed, nil
}

func (m *LeaderWorkerSetManager) syncHashSubRoleAssignments(ctx context.Context, state *subRoleState) error {
	s, plan := state.snapshot, state.plan
	active := s.LWS.Annotations[groupdeletion.PlanAnnotation]
	if plan == nil && active != "" || plan != nil && (plan.Armed && active != plan.ID || !plan.Armed && active != "") {
		return fmt.Errorf("LWS %s has inconsistent sub-role delete authorization", s.LWS.Name)
	}
	if plan == nil {
		plan = &subRolePlan{Armed: true, Counts: state.issued}
	} else if plan.Armed {
		if int(getLWSReplicas(s.LWS)) != countSubRoles(plan.Counts) {
			return fmt.Errorf("LWS %s Spec changed during sub-role transaction %s", s.LWS.Name, plan.ID)
		}
		if !subRoleNativeTargetsApplied(s) {
			return errReplicaGroupsPending
		}
		// A victim may occupy a replacement vacancy even when total count is
		// already at target. Delete it before requiring enough survivors.
		victims := false
		for _, group := range activeSubRoleGroups(s) {
			if !subRoleVictim(group, plan) {
				continue
			}
			if err := m.checkSubRoleLWS(ctx, s.LWS); err != nil {
				return err
			}
			pod := group.Leader
			if err := m.client.Delete(ctx, pod, client.Preconditions{UID: &pod.UID, ResourceVersion: &pod.ResourceVersion}); err != nil && !apierrors.IsNotFound(err) {
				return err
			}
			victims = true
		}
		if victims {
			return errReplicaGroupsPending
		}
	} else if !subRoleNativeSettled(s) {
		return errReplicaGroupsPending
	}
	assignments := subRolePlanAssignments(s, plan)
	if assignments == nil {
		return errReplicaGroupsPending
	}
	_, removing := plan.Counts[""]
	healthy := len(assignments) == min(int(getLWSReplicas(s.LWS)), countSubRoles(plan.Counts))
	groups := activeSubRoleGroups(s)
	for _, group := range groups {
		if _, retained := assignments[group.Leader.UID]; retained {
			healthy = healthy && retainedSubRoleEligible(group)
		}
	}
	// Finish newborn/partial labels before health gates, just as Ordinal does.
	changed := false
	for _, group := range groups {
		assignment, retained := assignments[group.Leader.UID]
		name, coherent := groupSubRole(group)
		if retained && (!coherent || name == "") && !removing {
			if err := m.labelSubRoleGroup(ctx, s.LWS, group, assignment); err != nil {
				return err
			}
			changed = true
		}
	}
	if changed {
		return errReplicaGroupsPending
	}
	if !plan.Armed && len(assignments) != min(int(getLWSReplicas(s.LWS)), countSubRoles(plan.Counts)) {
		return errReplicaGroupsPending
	}
	for _, group := range groups {
		assignment, retained := assignments[group.Leader.UID]
		if !retained {
			if !plan.Armed {
				marked, err := m.markSubRoleVictim(ctx, s.LWS, group, plan)
				if err != nil {
					return err
				}
				changed = changed || marked
			}
			continue
		}
		name, coherent := groupSubRole(group)
		protectionMatches := subRoleProtected(s.LWS, group)
		if removing {
			protectionMatches = group.Leader.Annotations[groupdeletion.ProtectionAnnotation] == ""
		}
		if name == assignment && coherent && protectionMatches && group.Leader.Annotations[groupdeletion.VictimAnnotation] == "" {
			continue
		}
		if state.plan == nil && name != assignment && !healthy {
			return errReplicaGroupsPending
		}
		if err := m.labelSubRoleGroup(ctx, s.LWS, group, assignment); err != nil {
			return err
		}
		changed = true
	}
	// Observe every metadata repair before publishing deletion authority or Spec.
	if changed {
		return errReplicaGroupsPending
	}
	if !plan.Armed {
		state.issued = maps.Clone(plan.Counts)
		plan.Armed = true
		return m.saveSubRoles(ctx, state)
	}
	if changed, err := m.deleteSubRoleExtras(ctx, s, assignments); changed || err != nil {
		if err != nil {
			return err
		}
		return errReplicaGroupsPending
	}
	if len(groups) != countSubRoles(plan.Counts) || len(assignments) != len(groups) || !subRoleNativeSettled(s) {
		return errReplicaGroupsPending
	}
	if state.plan != nil {
		state.pending, state.plan = nil, nil
		if removing {
			state.issued, state.initial = nil, nil
		}
		return m.saveSubRoles(ctx, state)
	}
	return nil
}

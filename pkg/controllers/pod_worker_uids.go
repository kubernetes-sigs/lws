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

package controllers

import (
	"context"
	"crypto/sha256"
	"fmt"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	leaderworkerset "sigs.k8s.io/lws/api/leaderworkerset/v1"
	podutils "sigs.k8s.io/lws/pkg/utils/pod"
	revisionutils "sigs.k8s.io/lws/pkg/utils/revision"
	statefulsetutils "sigs.k8s.io/lws/pkg/utils/statefulset"
)

// workerUIDRecordDelay is how soon a leader is reconciled again while its
// workers are not all present yet, so the UID record is written once they are.
const workerUIDRecordDelay = time.Second

// workerUIDListMax is the largest UID list stored verbatim. A larger group
// stores a sha256 of the same canonical list plus the worker count, which
// still fits in an annotation and still detects a missing or replaced worker.
var workerUIDListMax = 64 * 1024

const workerUIDHashPrefix = "sha256:"

// restartPolicyTracksWorkerUIDs reports whether a lost or replaced worker
// should recreate the group, which is the only reason to remember worker UIDs.
func restartPolicyTracksWorkerUIDs(policy leaderworkerset.RestartPolicyType) bool {
	return policy == leaderworkerset.RecreateGroupOnPodRestart || policy == leaderworkerset.RecreateGroupAfterStart
}

// workerFollowsRestartPolicy reports whether a healthy worker (no container
// restart and no deletion timestamp) still needs restart-policy handling.
// The common case, a worker whose UID is already recorded, returns false
// without listing ControllerRevisions. When the record is not written yet and
// this leader's revision recreates the group, it writes the record once every
// worker exists.
func (r *PodReconciler) workerFollowsRestartPolicy(ctx context.Context, pod *corev1.Pod, lws *leaderworkerset.LeaderWorkerSet) (bool, error) {
	leader, err := r.leaderOfWorker(ctx, pod)
	if err != nil || leader == nil {
		// Match the previous fast path: a worker whose leader cannot be read
		// has nothing to reconcile. The leader deletion that orphans a worker
		// is handled by the deletion event, which does not reach this path.
		return false, nil
	}
	if leader.Annotations[leaderworkerset.GroupRecreatePendingAnnotationKey] != "" {
		return true, nil
	}
	record, err := parseWorkerUIDRecord(leader.Annotations[leaderworkerset.WorkerUIDsAnnotationKey])
	if err != nil {
		return false, err
	}
	// A rolling update can change the leader's revision without replacing the
	// pod. The previous revision's workers are removed on purpose, so a record
	// from that revision is not a failure. Record the new revision's workers
	// once they exist.
	if record.empty() || record.revision != revisionutils.GetRevisionKey(leader) {
		tracked, err := r.leaderTracksWorkerUIDs(ctx, lws, leader)
		if err != nil || !tracked {
			return false, err
		}
		_, err = r.ensureWorkerUIDRecord(ctx, leader, ptr.Deref(lws.Spec.LeaderWorkerTemplate.Size, 1))
		return false, err
	}
	if !record.hashed && record.contains(pod.UID) {
		return false, nil
	}
	if record.hashed {
		_, _, lost, err := r.workerIdentityLost(ctx, leader)
		return lost, err
	}
	// A stale worker from a previous group has a UID the new leader did not
	// record. It must not recreate the group that replaced it.
	if revisionutils.GetRevisionKey(pod) != revisionutils.GetRevisionKey(leader) {
		return false, nil
	}
	belongs, err := r.workerPodBelongsToLeader(ctx, *pod, *leader)
	if err != nil || !belongs {
		return false, err
	}
	return true, nil
}

// leaderOfWorker returns the worker's leader, or nil when the worker name
// does not identify one. A Get error is returned to the caller.
func (r *PodReconciler) leaderOfWorker(ctx context.Context, pod *corev1.Pod) (*corev1.Pod, error) {
	name := pod.Annotations[leaderworkerset.LeaderPodNameAnnotationKey]
	if name == "" {
		var ordinal int
		name, ordinal = statefulsetutils.GetParentNameAndOrdinal(pod.Name)
		if name == "" || ordinal == -1 {
			return nil, nil
		}
	}
	var leader corev1.Pod
	if err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: pod.Namespace}, &leader); err != nil {
		return nil, err
	}
	return &leader, nil
}

// leaderTracksWorkerUIDs reports whether the leader's own revision, not the
// live LeaderWorkerSet, recreates the group on a worker failure. The answer is
// cached for the process: a rolling update replaces the leader rather than
// changing its revision in place.
func (r *PodReconciler) leaderTracksWorkerUIDs(ctx context.Context, lws *leaderworkerset.LeaderWorkerSet, leader *corev1.Pod) (bool, error) {
	revisionKey := revisionutils.GetRevisionKey(leader)
	if revisionKey == "" || leader.UID == "" {
		return false, nil
	}
	cacheKey := string(leader.UID) + "/" + revisionKey
	if value, ok := r.workerUIDPolicy.Load(cacheKey); ok {
		tracked, _ := value.(bool)
		return tracked, nil
	}
	revision, err := revisionutils.GetRevision(ctx, r.Client, lws, revisionKey)
	if err != nil {
		return false, err
	}
	// The cache has not observed the revision the leader was admitted with.
	// Do not cache that as "do not track": the next reconcile can see it.
	if revision == nil {
		return false, nil
	}
	group, err := revisionutils.ApplyRevision(lws, revision)
	if err != nil {
		return false, err
	}
	tracked := restartPolicyTracksWorkerUIDs(group.Spec.LeaderWorkerTemplate.RestartPolicy)
	r.workerUIDPolicy.Store(cacheKey, tracked)
	return tracked, nil
}

// finishLeaderReconcile writes the worker UID record once the group has formed.
// Until then the leader is reconciled again, unless this reconcile is already
// waiting on something else (a backoff delay must not be shortened).
func (r *PodReconciler) finishLeaderReconcile(ctx context.Context, leader *corev1.Pod, groupLws *leaderworkerset.LeaderWorkerSet, result ctrl.Result) (ctrl.Result, error) {
	r.rememberWorkerUIDPolicy(leader, groupLws)
	if !restartPolicyTracksWorkerUIDs(groupLws.Spec.LeaderWorkerTemplate.RestartPolicy) {
		return result, nil
	}
	pending, err := r.ensureWorkerUIDRecord(ctx, leader, ptr.Deref(groupLws.Spec.LeaderWorkerTemplate.Size, 1))
	if err != nil {
		return ctrl.Result{}, err
	}
	// A reconcile that is already waiting, such as a restart backoff, keeps its
	// own delay. RequeueAfter is the supported way to ask for another pass.
	if pending && result.RequeueAfter == 0 {
		result = mergeResult(result, ctrl.Result{RequeueAfter: workerUIDRecordDelay})
	}
	return result, nil
}

// rememberWorkerUIDPolicy caches the revision policy already applied to groupLws
// so a following worker update does not list ControllerRevisions again.
func (r *PodReconciler) rememberWorkerUIDPolicy(leader *corev1.Pod, groupLws *leaderworkerset.LeaderWorkerSet) {
	revisionKey := revisionutils.GetRevisionKey(leader)
	if revisionKey == "" || leader.UID == "" {
		return
	}
	r.workerUIDPolicy.Store(string(leader.UID)+"/"+revisionKey, restartPolicyTracksWorkerUIDs(groupLws.Spec.LeaderWorkerTemplate.RestartPolicy))
}

// ensureWorkerUIDRecord patches the leader with its workers' UIDs once the
// workers of the leader's current revision all exist and none are terminating.
// pending is true when that revision should be recorded but has not formed.
// A record for a different revision is replaced: a rolling update keeps the
// leader pod and deletes the previous revision's workers, which is not a
// failure. A record for the current revision is not rewritten. A recreated
// group gets a new leader, so its record starts empty.
func (r *PodReconciler) ensureWorkerUIDRecord(ctx context.Context, leader *corev1.Pod, sizeDefault int32) (pending bool, err error) {
	revision := revisionutils.GetRevisionKey(leader)
	expected := podGroupSize(leader, sizeDefault) - 1
	if expected <= 0 || revision == "" || leader.DeletionTimestamp != nil {
		return false, nil
	}
	existing, err := parseWorkerUIDRecord(leader.Annotations[leaderworkerset.WorkerUIDsAnnotationKey])
	if err != nil {
		return false, err
	}
	if existing.revision == revision && !existing.empty() {
		return false, nil
	}
	workers, err := r.leaderWorkers(ctx, leader)
	if err != nil {
		return false, err
	}
	live := workersWithoutDeletion(workers)
	if len(live) != expected || len(live) != len(workers) {
		return true, nil
	}
	if err := r.writeWorkerUIDs(ctx, leader, revision, live); err != nil {
		return false, err
	}
	ctrl.LoggerFrom(ctx).V(2).Info("Recorded worker UIDs on leader", "leader", leader.Name, "revision", revision, "workers", len(live))
	return false, nil
}

// writeWorkerUIDs sets the leader's worker UID annotation for revision.
// A record for the same revision is left in place. A conflict is ignored
// when another reconcile already wrote the annotation.
func (r *PodReconciler) writeWorkerUIDs(ctx context.Context, leader *corev1.Pod, revision string, workers []corev1.Pod) error {
	uids := make([]types.UID, len(workers))
	for i := range workers {
		uids[i] = workers[i].UID
	}
	formatted := formatWorkerUIDRecord(revision, uids)
	if leader.Annotations[leaderworkerset.WorkerUIDsAnnotationKey] == formatted {
		return nil
	}
	existing, err := parseWorkerUIDRecord(leader.Annotations[leaderworkerset.WorkerUIDsAnnotationKey])
	if err != nil {
		return err
	}
	if existing.revision == revision && !existing.empty() {
		return nil
	}
	original := leader.DeepCopy()
	if leader.Annotations == nil {
		leader.Annotations = map[string]string{}
	}
	leader.Annotations[leaderworkerset.WorkerUIDsAnnotationKey] = formatted
	if err := r.Patch(ctx, leader, client.MergeFrom(original)); err != nil {
		leader.Annotations = original.Annotations
		if apierrors.IsConflict(err) {
			var fresh corev1.Pod
			if getErr := r.Get(ctx, client.ObjectKeyFromObject(leader), &fresh); getErr == nil {
				freshRecord, parseErr := parseWorkerUIDRecord(fresh.Annotations[leaderworkerset.WorkerUIDsAnnotationKey])
				if parseErr == nil && freshRecord.revision == revision && !freshRecord.empty() {
					if leader.Annotations == nil {
						leader.Annotations = map[string]string{}
					}
					leader.Annotations[leaderworkerset.WorkerUIDsAnnotationKey] = fresh.Annotations[leaderworkerset.WorkerUIDsAnnotationKey]
					return nil
				}
			}
		}
		return err
	}
	return nil
}

// workerIdentityLost reports whether the leader's worker UID record no longer
// matches the workers that belong to it. lostPod is a worker whose UID is not
// recorded, when one is still present. missingUID is set when a recorded
// worker is gone and no replacement is present. An empty record is not a loss:
// the group has not been recorded yet.
func (r *PodReconciler) workerIdentityLost(ctx context.Context, leader *corev1.Pod) (lostPod *corev1.Pod, missingUID types.UID, lost bool, err error) {
	record, err := parseWorkerUIDRecord(leader.Annotations[leaderworkerset.WorkerUIDsAnnotationKey])
	if err != nil || record.empty() || record.revision != revisionutils.GetRevisionKey(leader) {
		return nil, "", false, err
	}
	workers, err := r.leaderWorkers(ctx, leader)
	if err != nil {
		return nil, "", false, err
	}
	current := make([]types.UID, len(workers))
	var unknown *corev1.Pod
	for i := range workers {
		current[i] = workers[i].UID
		if unknown == nil && !record.contains(workers[i].UID) {
			unknown = workers[i].DeepCopy()
		}
	}
	if record.matches(current) {
		return nil, "", false, nil
	}
	if unknown != nil {
		return unknown, "", true, nil
	}
	for uid := range record.uids {
		if !uidListed(current, uid) {
			return nil, uid, true, nil
		}
	}
	// A hashed record does not keep each UID, so a mismatch with no current
	// worker still has to recreate the group.
	return nil, "", true, nil
}

// leaderWorkers lists the non-leader pods of the leader's group that belong to
// this leader and share its revision. Terminating pods are included: they
// still carry the UID that was recorded.
func (r *PodReconciler) leaderWorkers(ctx context.Context, leader *corev1.Pod) ([]corev1.Pod, error) {
	selector := map[string]string{
		leaderworkerset.SetNameLabelKey: leader.Labels[leaderworkerset.SetNameLabelKey],
	}
	if group := leader.Labels[leaderworkerset.GroupIndexLabelKey]; group != "" {
		selector[leaderworkerset.GroupIndexLabelKey] = group
	}
	var listed corev1.PodList
	if err := r.List(ctx, &listed, client.InNamespace(leader.Namespace), client.MatchingLabels(selector)); err != nil {
		return nil, err
	}
	revision := revisionutils.GetRevisionKey(leader)
	workers := make([]corev1.Pod, 0, len(listed.Items))
	for i := range listed.Items {
		pod := &listed.Items[i]
		if pod.UID == leader.UID || podutils.LeaderPod(*pod) {
			continue
		}
		if revisionutils.GetRevisionKey(pod) != revision {
			continue
		}
		belongs, err := r.workerPodBelongsToLeader(ctx, *pod, *leader)
		if err != nil {
			return nil, err
		}
		if belongs {
			workers = append(workers, *pod)
		}
	}
	return workers, nil
}

// missingWorkerPod is the stand-in passed to restart-policy handling when a
// recorded worker no longer exists. It is not written to the API. Its revision
// matches the leader, and it is not itself a leader, so handling treats it as
// the failed worker.
func missingWorkerPod(leader *corev1.Pod, uid types.UID) corev1.Pod {
	now := metav1.Now()
	name := string(uid)
	if name == "" {
		name = leader.Name + "-missing"
	}
	return corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: leader.Namespace,
			UID:       uid,
			Labels: map[string]string{
				leaderworkerset.SetNameLabelKey:     leader.Labels[leaderworkerset.SetNameLabelKey],
				leaderworkerset.GroupIndexLabelKey:  leader.Labels[leaderworkerset.GroupIndexLabelKey],
				leaderworkerset.WorkerIndexLabelKey: "1",
				leaderworkerset.RevisionKey:         revisionutils.GetRevisionKey(leader),
			},
			Annotations: map[string]string{
				leaderworkerset.LeaderPodNameAnnotationKey: leader.Name,
				leaderworkerset.SizeAnnotationKey:          leader.Annotations[leaderworkerset.SizeAnnotationKey],
			},
			DeletionTimestamp: &now,
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
}

func workersWithoutDeletion(workers []corev1.Pod) []corev1.Pod {
	live := make([]corev1.Pod, 0, len(workers))
	for i := range workers {
		if workers[i].DeletionTimestamp == nil {
			live = append(live, workers[i])
		}
	}
	return live
}

// workerUIDRecord is the parsed leader annotation. revision is the leader
// revision the UIDs were recorded for.
type workerUIDRecord struct {
	revision string
	uids     map[types.UID]struct{}
	hash     string
	count    int
	hashed   bool
}

func (r workerUIDRecord) empty() bool {
	return r.revision == "" && !r.hashed && len(r.uids) == 0
}

func (r workerUIDRecord) contains(uid types.UID) bool {
	if r.hashed {
		return false
	}
	_, ok := r.uids[uid]
	return ok
}

// matches reports whether current is the recorded set. A hashed record matches
// when the count and the hash of the canonical UID list both agree.
func (r workerUIDRecord) matches(current []types.UID) bool {
	if r.hashed {
		sum, count := hashWorkerUIDs(current)
		return count == r.count && sum == r.hash
	}
	if len(current) != len(r.uids) {
		return false
	}
	for _, uid := range current {
		if _, ok := r.uids[uid]; !ok {
			return false
		}
	}
	return true
}

func parseWorkerUIDRecord(value string) (workerUIDRecord, error) {
	if value == "" {
		return workerUIDRecord{}, nil
	}
	revision, payload, ok := strings.Cut(value, "|")
	if !ok || revision == "" || payload == "" {
		return workerUIDRecord{}, fmt.Errorf("parsing worker UID record %q", value)
	}
	if strings.HasPrefix(payload, workerUIDHashPrefix) {
		rest := strings.TrimPrefix(payload, workerUIDHashPrefix)
		hexPart, countPart, found := strings.Cut(rest, ":")
		var count int
		if !found || len(hexPart) != sha256.Size*2 || !parseDecimal(countPart, &count) || count <= 0 {
			return workerUIDRecord{}, fmt.Errorf("parsing worker UID record %q", value)
		}
		return workerUIDRecord{revision: revision, hash: hexPart, count: count, hashed: true}, nil
	}
	uids := make(map[types.UID]struct{})
	for _, part := range strings.Split(payload, ",") {
		if part == "" {
			return workerUIDRecord{}, fmt.Errorf("parsing worker UID record %q", value)
		}
		uids[types.UID(part)] = struct{}{}
	}
	return workerUIDRecord{revision: revision, uids: uids, count: len(uids)}, nil
}

func formatWorkerUIDRecord(revision string, uids []types.UID) string {
	joined := canonicalWorkerUIDs(uids)
	payload := joined
	if len(joined) > workerUIDListMax {
		sum, count := hashWorkerUIDs(uids)
		payload = fmt.Sprintf("%s%s:%d", workerUIDHashPrefix, sum, count)
	}
	return revision + "|" + payload
}

func canonicalWorkerUIDs(uids []types.UID) string {
	copied := append([]types.UID(nil), uids...)
	sort.Slice(copied, func(i, j int) bool { return copied[i] < copied[j] })
	parts := make([]string, len(copied))
	for i, uid := range copied {
		parts[i] = string(uid)
	}
	return strings.Join(parts, ",")
}

func hashWorkerUIDs(uids []types.UID) (string, int) {
	sum := sha256.Sum256([]byte(canonicalWorkerUIDs(uids)))
	return fmt.Sprintf("%x", sum[:]), len(uids)
}

func uidListed(uids []types.UID, uid types.UID) bool {
	for _, current := range uids {
		if current == uid {
			return true
		}
	}
	return false
}

func parseDecimal(value string, out *int) bool {
	if value == "" {
		return false
	}
	n := 0
	for _, ch := range value {
		if ch < '0' || ch > '9' {
			return false
		}
		n = n*10 + int(ch-'0')
	}
	*out = n
	return true
}

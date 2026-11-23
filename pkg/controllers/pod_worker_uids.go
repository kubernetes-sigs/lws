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
	"encoding/hex"
	"fmt"
	"slices"
	"strconv"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/validation"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	leaderworkerset "sigs.k8s.io/lws/api/leaderworkerset/v1"
	podutils "sigs.k8s.io/lws/pkg/utils/pod"
	revisionutils "sigs.k8s.io/lws/pkg/utils/revision"
)

const (
	workerPodGroupIndex = "lws.workerPodGroup"
	workerUIDHashPrefix = "sha256:"
	workerUIDStarted    = ":started"
)

func restartPolicyTracksWorkerUIDs(policy leaderworkerset.RestartPolicyType) bool {
	return policy == leaderworkerset.RecreateGroupOnPodRestart || policy == leaderworkerset.RecreateGroupAfterStart
}

func requiresStartedGroup(policy leaderworkerset.RestartPolicyType, lws *leaderworkerset.LeaderWorkerSet) bool {
	_, legacy := lws.Annotations[leaderworkerset.RecreateGroupAfterStartAnnotationKey]
	return policy == leaderworkerset.RecreateGroupAfterStart || legacy
}

// recordWorkerUIDs establishes the baseline for this leader and revision. The
// AfterStart policy needs evidence that the group had started before it lost a
// worker, so it records a group only once all its members have left Pending.
func (r *PodReconciler) recordWorkerUIDs(ctx context.Context, leader *corev1.Pod, group *leaderworkerset.LeaderWorkerSet) error {
	policy := group.Spec.LeaderWorkerTemplate.RestartPolicy
	revision := revisionutils.GetRevisionKey(leader)
	wantWorkers := podGroupSize(leader, ptr.Deref(group.Spec.LeaderWorkerTemplate.Size, 1)) - 1
	if !restartPolicyTracksWorkerUIDs(policy) || wantWorkers < 1 || revision == "" || leader.DeletionTimestamp != nil {
		return nil
	}
	afterStart := requiresStartedGroup(policy, group)
	record := parseWorkerUIDRecord(leader.Annotations[leaderworkerset.WorkerUIDsAnnotationKey])
	if record.revision == revision && (!afterStart || record.started) {
		return nil
	}
	workers, err := r.workerUIDMembers(ctx, leader)
	if err != nil || len(workers) != wantWorkers {
		return err
	}
	for i := range workers {
		if workers[i].DeletionTimestamp != nil || workers[i].UID == "" {
			return nil
		}
	}
	state, _ := groupStartStateForPods(append(workers, *leader), wantWorkers+1)
	started := state == groupStarted
	if afterStart && !started {
		return nil
	}

	record = workerUIDRecord{revision: revision, digest: workerUIDDigest(workers), count: len(workers), started: started}
	updated := leader.DeepCopy()
	if updated.Annotations == nil {
		updated.Annotations = make(map[string]string)
	}
	updated.Annotations[leaderworkerset.WorkerUIDsAnnotationKey] = record.String()
	if err := validation.ValidateAnnotationsSize(updated.Annotations); err != nil {
		ctrl.LoggerFrom(ctx).Info("Worker UID record does not fit in leader annotations", "leader", client.ObjectKeyFromObject(leader))
		return nil
	}
	// A cache snapshot can predate the first record or even belong to a deleted
	// leader. The resource version protects both the baseline and leader UID.
	patch := client.MergeFromWithOptions(leader, client.MergeFromWithOptimisticLock{})
	return client.IgnoreNotFound(r.Patch(ctx, updated, patch))
}

type workerUIDLoss struct {
	record  workerUIDRecord
	workers []corev1.Pod
}

// missingWorkerName recovers a single missing ordinal even when the record
// contains only a digest. A digest mismatch at equal count identifies a group
// failure without identifying which worker was replaced.
func (loss *workerUIDLoss) missingWorkerName(leader *corev1.Pod) string {
	if loss.record.count-len(loss.workers) != 1 {
		return ""
	}
	names := make(map[string]struct{}, len(loss.workers))
	for i := range loss.workers {
		names[loss.workers[i].Name] = struct{}{}
	}
	prefix := workerStatefulSetName(leader) + "-"
	for ordinal := 1; ordinal <= loss.record.count; ordinal++ {
		name := prefix + strconv.Itoa(ordinal)
		if _, exists := names[name]; !exists {
			return name
		}
	}
	return ""
}

func (r *PodReconciler) detectWorkerUIDLoss(ctx context.Context, leader *corev1.Pod) (*workerUIDLoss, error) {
	record := parseWorkerUIDRecord(leader.Annotations[leaderworkerset.WorkerUIDsAnnotationKey])
	if record.revision == "" || record.revision != revisionutils.GetRevisionKey(leader) {
		return nil, nil
	}
	workers, err := r.workerUIDMembers(ctx, leader)
	if err != nil {
		return nil, err
	}
	if record.count == len(workers) && record.digest == workerUIDDigest(workers) {
		return nil, nil
	}
	return &workerUIDLoss{record: record, workers: workers}, nil
}

func podWorkerGroupKey(pod *corev1.Pod) string {
	name, group := pod.Labels[leaderworkerset.SetNameLabelKey], pod.Labels[leaderworkerset.GroupIndexLabelKey]
	if name == "" || group == "" {
		return ""
	}
	return name + "/" + group
}

func indexWorkerPodGroup(obj client.Object) []string {
	pod := obj.(*corev1.Pod)
	if key := podWorkerGroupKey(pod); key != "" && !podutils.LeaderPod(*pod) {
		return []string{key}
	}
	return nil
}

// workerUIDMembers reads only this group and resolves the ownership of its
// worker StatefulSet once. A same-ordinal worker of a previous leader or
// revision does not belong to this leader's record.
func (r *PodReconciler) workerUIDMembers(ctx context.Context, leader *corev1.Pod) ([]corev1.Pod, error) {
	key := podWorkerGroupKey(leader)
	if key == "" {
		return nil, nil
	}
	var pods corev1.PodList
	if err := r.List(ctx, &pods, client.InNamespace(leader.Namespace), client.MatchingFields{workerPodGroupIndex: key}); err != nil {
		return nil, err
	}
	if len(pods.Items) == 0 {
		return nil, nil
	}
	var sts appsv1.StatefulSet
	if err := r.Get(ctx, client.ObjectKey{Namespace: leader.Namespace, Name: workerStatefulSetName(leader)}, &sts); client.IgnoreNotFound(err) != nil {
		return nil, err
	}
	stsOwned := sts.UID != "" && metav1.IsControlledBy(&sts, leader)
	revision := revisionutils.GetRevisionKey(leader)
	workers := pods.Items[:0]
	for i := range pods.Items {
		pod := &pods.Items[i]
		if revisionutils.GetRevisionKey(pod) != revision {
			continue
		}
		owner := metav1.GetControllerOf(pod)
		if owner == nil {
			continue
		}
		if (owner.Kind == "Pod" && owner.Name == leader.Name && owner.UID == leader.UID) ||
			(owner.Kind == "StatefulSet" && stsOwned && owner.Name == sts.Name && owner.UID == sts.UID) {
			workers = append(workers, *pod)
		}
	}
	return workers, nil
}

type workerUIDRecord struct {
	revision string
	digest   string
	count    int
	started  bool
}

func (record workerUIDRecord) String() string {
	value := fmt.Sprintf("%s|%s%s:%d", record.revision, workerUIDHashPrefix, record.digest, record.count)
	if record.started {
		value += workerUIDStarted
	}
	return value
}

func parseWorkerUIDRecord(value string) workerUIDRecord {
	revision, payload, ok := strings.Cut(value, "|")
	if !ok || revision == "" || payload == "" {
		return workerUIDRecord{}
	}
	hash, hashed := strings.CutPrefix(payload, workerUIDHashPrefix)
	if !hashed {
		// The first version of the leader record also accepted plain UID lists.
		// Neither of its formats established that the group had started.
		uids := strings.Split(payload, ",")
		if slices.Contains(uids, "") {
			return workerUIDRecord{}
		}
		return workerUIDRecord{revision: revision, digest: digestUIDs(uids), count: len(uids)}
	}
	hash, started := strings.CutSuffix(hash, workerUIDStarted)
	digest, rawCount, found := strings.Cut(hash, ":")
	count, err := strconv.Atoi(rawCount)
	if !found || err != nil || count < 1 || len(digest) != sha256.Size*2 {
		return workerUIDRecord{}
	}
	if _, err := hex.DecodeString(digest); err != nil {
		return workerUIDRecord{}
	}
	return workerUIDRecord{revision: revision, digest: digest, count: count, started: started}
}

func workerUIDDigest(workers []corev1.Pod) string {
	uids := make([]string, len(workers))
	for i := range workers {
		uids[i] = string(workers[i].UID)
	}
	return digestUIDs(uids)
}

func digestUIDs(uids []string) string {
	slices.Sort(uids)
	sum := sha256.Sum256([]byte(strings.Join(uids, ",")))
	return hex.EncodeToString(sum[:])
}

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

// Package replicagroups observes LWS replica groups and their retained
// availability. It does not choose scale-down victims, assign sub-roles, or
// calculate consumers' availability budgets.
package replicagroups

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strconv"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	leaderworkersetv1 "sigs.k8s.io/lws/api/leaderworkerset/v1"
	podutils "sigs.k8s.io/lws/pkg/utils/pod"
	statefulsetutils "sigs.k8s.io/lws/pkg/utils/statefulset"
)

// Snapshot contains one LWS and its observed, UID-verified descendants. Exactly
// one leader workload is populated, unless it is missing or foreign-owned.
// ReplicaSets are present only for Hash identity. Groups includes unready,
// terminating, and excess groups, sorted by ordinal and then leader name.
//
// This is an ordered observation, NOT an atomic multi-resource snapshot. The
// native controllers retain their specs and observed generations so consumers
// can apply the acknowledgements required by their own decisions.
type Snapshot struct {
	LWS               *leaderworkersetv1.LeaderWorkerSet
	LeaderStatefulSet *appsv1.StatefulSet
	LeaderDeployment  *appsv1.Deployment
	ReplicaSets       []*appsv1.ReplicaSet
	Groups            []Group
}

// Group is identified by Leader.UID, not by a mutable label or a reusable name.
// WorkerStatefulSet is nil if absent or not owned by this leader UID. Pods
// contains the leader followed by all observed Pods owned by that worker set,
// including unready, terminating, and excess workers. Leaderless remnants are
// not groups and are not included.
type Group struct {
	Leader            *corev1.Pod
	WorkerStatefulSet *appsv1.StatefulSet
	Pods              []*corev1.Pod
	Ordinal           int // -1 for Hash identity or an invalid StatefulSet Pod name.
	// Ready checks the running/Ready leader, worker-set availability and
	// revision convergence, and every expected worker Pod. It does not imply
	// nontermination, observed generations, or availability credit.
	Ready bool
	// Terminating reports a deletion timestamp on any observed group Pod or
	// the worker StatefulSet. Pending, unissued deletions are policy-specific.
	Terminating bool
}

// Observe reads the expected LWS, its native controllers, and finally its Pods
// through the supplied reader. Callers making safety-sensitive decisions must
// pass an uncached API reader (mgr.GetAPIReader()), not a cached client. All Pod
// membership and readiness facts come from the same namespace-scoped Pod list.
//
// A missing or same-named replacement LWS returns (nil, nil). A missing or
// foreign-owned leader workload returns an empty snapshot of the live LWS.
// Read failures or invalid group-size metadata return an error, never a partial
// observation or a fallback to status counters. expected must identify a
// persisted LWS by a nonempty UID.
// Objects returned by Observe are read-only; mutation consumers must copy them
// and enforce their own UID/resource-version preconditions when writing.
// Consumers planning from an earlier LWS spec must also match its generation
// against Snapshot.LWS. A nil snapshot is unknown, not an observed zero Ready.
func Observe(ctx context.Context, reader client.Reader, expected *leaderworkersetv1.LeaderWorkerSet) (*Snapshot, error) {
	lws, err := readLWS(ctx, reader, expected)
	if err != nil || lws == nil {
		return nil, err
	}
	snapshot := &Snapshot{LWS: lws}
	hash := lws.Spec.GroupIdentity == leaderworkersetv1.GroupIdentityHash
	var workload client.Object = &appsv1.StatefulSet{}
	if hash {
		workload = &appsv1.Deployment{}
	}
	if err := reader.Get(ctx, client.ObjectKeyFromObject(lws), workload); err != nil {
		if client.IgnoreNotFound(err) != nil {
			return nil, fmt.Errorf("reading leader workload: %w", err)
		}
		return snapshot, nil
	}
	if !metav1.IsControlledBy(workload, lws) {
		return snapshot, nil
	}

	leaderOwners := make(map[types.UID]bool)
	switch workload := workload.(type) {
	case *appsv1.StatefulSet:
		snapshot.LeaderStatefulSet = workload
		leaderOwners[workload.UID] = true
	case *appsv1.Deployment:
		snapshot.LeaderDeployment = workload
		var replicaSets appsv1.ReplicaSetList
		if err := reader.List(ctx, &replicaSets, client.InNamespace(lws.Namespace),
			client.MatchingLabels{leaderworkersetv1.SetNameLabelKey: lws.Name}); err != nil {
			return nil, fmt.Errorf("listing leader ReplicaSets: %w", err)
		}
		for i := range replicaSets.Items {
			rs := &replicaSets.Items[i]
			if metav1.IsControlledBy(rs, workload) {
				snapshot.ReplicaSets = append(snapshot.ReplicaSets, rs)
				leaderOwners[rs.UID] = true
			}
		}
		slices.SortFunc(snapshot.ReplicaSets, func(a, b *appsv1.ReplicaSet) int { return cmp.Compare(a.Name, b.Name) })
	}

	// Do not filter by desired size: even a leader-only target can have residual
	// workers, and consumers need to see their termination state.
	var workerSets appsv1.StatefulSetList
	if err := reader.List(ctx, &workerSets, client.InNamespace(lws.Namespace),
		client.MatchingLabels{leaderworkersetv1.SetNameLabelKey: lws.Name}); err != nil {
		return nil, fmt.Errorf("listing worker StatefulSets: %w", err)
	}
	workersByName := make(map[string]*appsv1.StatefulSet, len(workerSets.Items))
	for i := range workerSets.Items {
		workersByName[workerSets.Items[i].Name] = &workerSets.Items[i]
	}

	var pods corev1.PodList
	if err := reader.List(ctx, &pods, client.InNamespace(lws.Namespace),
		client.MatchingLabels{leaderworkersetv1.SetNameLabelKey: lws.Name}); err != nil {
		return nil, fmt.Errorf("listing group Pods: %w", err)
	}
	podsByOwner := make(map[types.UID][]*corev1.Pod)
	for i := range pods.Items {
		pod := &pods.Items[i]
		if owner := metav1.GetControllerOf(pod); owner != nil {
			podsByOwner[owner.UID] = append(podsByOwner[owner.UID], pod)
		}
	}
	for i := range pods.Items {
		leader := &pods.Items[i]
		owner := metav1.GetControllerOf(leader)
		if owner == nil || !leaderOwners[owner.UID] || !podutils.LeaderPod(*leader) {
			continue
		}
		group := Group{Leader: leader, Pods: []*corev1.Pod{leader}, Ordinal: -1}
		if !hash {
			parent, ordinal := statefulsetutils.GetParentNameAndOrdinal(leader.Name)
			if parent == workload.GetName() {
				group.Ordinal = ordinal
			}
		}
		// Hash leaders have a hostname chosen at admission, before their Pod
		// name is known. The worker StatefulSet uses that hostname in both modes.
		workerName := leader.Spec.Hostname
		if workerName == "" {
			workerName = leader.Name
		}
		if workers := workersByName[workerName]; workers != nil && metav1.IsControlledBy(workers, leader) {
			group.WorkerStatefulSet = workers
			members := podsByOwner[workers.UID]
			slices.SortFunc(members, func(a, b *corev1.Pod) int {
				_, ai := statefulsetutils.GetParentNameAndOrdinal(a.Name)
				_, bi := statefulsetutils.GetParentNameAndOrdinal(b.Name)
				return cmp.Or(cmp.Compare(ai, bi), cmp.Compare(a.Name, b.Name))
			})
			group.Pods = append(group.Pods, members...)
			group.Terminating = !workers.DeletionTimestamp.IsZero()
		}
		for _, pod := range group.Pods {
			group.Terminating = group.Terminating || !pod.DeletionTimestamp.IsZero()
		}
		// Older groups retain their revision's size. An unknown size is not
		// zero readiness: consumers may use raw readiness as a no-worsening floor.
		sizeValue := leader.Annotations[leaderworkersetv1.SizeAnnotationKey]
		size, err := strconv.Atoi(sizeValue)
		if err != nil || size < 1 {
			return nil, fmt.Errorf("leader Pod %s has invalid group size %q", leader.Name, sizeValue)
		}
		group.Ready = ready(group, size)
		snapshot.Groups = append(snapshot.Groups, group)
	}
	slices.SortFunc(snapshot.Groups, func(a, b Group) int {
		return cmp.Or(cmp.Compare(a.Ordinal, b.Ordinal), cmp.Compare(a.Leader.Name, b.Leader.Name))
	})
	return snapshot, nil
}

func readLWS(ctx context.Context, reader client.Reader, expected *leaderworkersetv1.LeaderWorkerSet) (*leaderworkersetv1.LeaderWorkerSet, error) {
	if expected == nil || expected.UID == "" {
		return nil, fmt.Errorf("observing replica groups requires an LWS UID")
	}
	lws := &leaderworkersetv1.LeaderWorkerSet{}
	if err := reader.Get(ctx, client.ObjectKeyFromObject(expected), lws); err != nil {
		return nil, client.IgnoreNotFound(err)
	}
	if lws.UID != expected.UID {
		return nil, nil
	}
	return lws, nil
}

// ready reports current whole-group readiness, not permission to drain another
// group. Ready can coexist with Terminating, a degraded annotation, an
// unobserved generation, or a pending scale-down decision. Consumers must apply
// those facts separately. In particular, a Ready leader alone is insufficient.
func ready(group Group, size int) bool {
	if !podutils.PodRunningAndReady(*group.Leader) {
		return false
	}
	if size == 1 {
		return true
	}
	workers := group.WorkerStatefulSet
	if workers == nil || workers.Spec.Replicas == nil || int(*workers.Spec.Replicas) != size-1 ||
		!statefulsetutils.StatefulsetReady(*workers) {
		return false
	}
	members := make(map[string]*corev1.Pod, len(group.Pods)-1)
	for _, pod := range group.Pods[1:] {
		members[pod.Name] = pod
	}
	for i := 1; i < size; i++ {
		worker := members[fmt.Sprintf("%s-%d", workers.Name, i)]
		if worker == nil || !podutils.PodRunningAndReady(*worker) {
			return false
		}
	}
	return true
}

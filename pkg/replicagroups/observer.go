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
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/selection"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/pager"
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

	var replicaSets appsv1.ReplicaSetList
	if hash {
		if err := reader.List(ctx, &replicaSets, client.InNamespace(lws.Namespace),
			client.MatchingLabels{leaderworkersetv1.SetNameLabelKey: lws.Name}); err != nil {
			return nil, fmt.Errorf("listing leader ReplicaSets: %w", err)
		}
	}

	// Do not filter by desired size: even a leader-only target can have residual
	// workers, and consumers need to see their termination state.
	var workerSets appsv1.StatefulSetList
	if err := reader.List(ctx, &workerSets, client.InNamespace(lws.Namespace),
		client.MatchingLabels{leaderworkersetv1.SetNameLabelKey: lws.Name}); err != nil {
		return nil, fmt.Errorf("listing worker StatefulSets: %w", err)
	}
	var pods corev1.PodList
	if err := reader.List(ctx, &pods, client.InNamespace(lws.Namespace),
		client.MatchingLabels{leaderworkersetv1.SetNameLabelKey: lws.Name}); err != nil {
		return nil, fmt.Errorf("listing group Pods: %w", err)
	}
	return buildSnapshot(lws, workload, replicaSets.Items, workerSets.Items, pods.Items)
}

// Bound selector length independently of the number of roles or revisions.
const maxLWSPerBatch = 100

// ObserveMany is Observe's batched counterpart for LWS in one namespace. It
// first lists live LWS, then reads native workloads followed by Pods for each
// bounded batch.
// options scope the LWS list (for example, to one owner's slice); only objects
// matching an expected name and UID are observed. Missing or replaced LWS have
// no map entry: consumers must treat that as unknown, not zero readiness.
//
// Like Observe, this requires an uncached reader for safety-sensitive decisions,
// returns read-only objects, and is ordered rather than atomic. Consumers must
// compare generations against their earlier intents and guard writes. All list
// pages are read; any read or metadata error returns no partial result. Callers
// needing independent failure domains should use separate batches for them.
func ObserveMany(ctx context.Context, reader client.Reader, expected []*leaderworkersetv1.LeaderWorkerSet, options ...client.ListOption) (map[types.UID]*Snapshot, error) {
	result := make(map[types.UID]*Snapshot, len(expected))
	if len(expected) == 0 {
		return result, nil
	}
	namespace := ""
	wanted := make(map[string]types.UID, len(expected))
	for _, lws := range expected {
		if lws == nil || lws.UID == "" || lws.Name == "" || lws.Namespace == "" {
			return nil, fmt.Errorf("observing replica groups requires an LWS namespace, name and UID")
		}
		if namespace != "" && namespace != lws.Namespace {
			return nil, fmt.Errorf("batched LWS observations require one namespace")
		}
		if uid, found := wanted[lws.Name]; found && uid != lws.UID {
			return nil, fmt.Errorf("conflicting expected UIDs for LeaderWorkerSet %s", lws.Name)
		}
		namespace = lws.Namespace
		wanted[lws.Name] = lws.UID
	}
	var live leaderworkersetv1.LeaderWorkerSetList
	if err := listAll(ctx, reader, &live, append(slices.Clone(options), client.InNamespace(namespace))...); err != nil {
		return nil, err
	}
	var matched []*leaderworkersetv1.LeaderWorkerSet
	for i := range live.Items {
		lws := &live.Items[i]
		if uid, found := wanted[lws.Name]; found && uid == lws.UID {
			matched = append(matched, lws)
		}
	}
	for batch := range slices.Chunk(matched, maxLWSPerBatch) {
		if err := observeBatch(ctx, reader, batch, result); err != nil {
			return nil, err
		}
	}
	return result, nil
}

type batchResources struct {
	lws         *leaderworkersetv1.LeaderWorkerSet
	leader      client.Object
	replicaSets []appsv1.ReplicaSet
	workerSets  []appsv1.StatefulSet
	pods        []corev1.Pod
}

func observeBatch(ctx context.Context, reader client.Reader, live []*leaderworkersetv1.LeaderWorkerSet, result map[types.UID]*Snapshot) error {
	byName := make(map[string]*batchResources, len(live))
	names, hasHash := make([]string, 0, len(live)), false
	for _, lws := range live {
		byName[lws.Name] = &batchResources{lws: lws}
		names = append(names, lws.Name)
		hasHash = hasHash || lws.Spec.GroupIdentity == leaderworkersetv1.GroupIdentityHash
	}
	requirement, err := labels.NewRequirement(leaderworkersetv1.SetNameLabelKey, selection.In, names)
	if err != nil {
		return err
	}
	options := []client.ListOption{client.InNamespace(live[0].Namespace), client.MatchingLabelsSelector{Selector: labels.NewSelector().Add(*requirement)}}

	// Partition each resource once; snapshot assembly never scans another LWS's
	// Pods. Leaders are also indexed by name, independently of their label.
	var statefulSets appsv1.StatefulSetList
	if err := listAll(ctx, reader, &statefulSets, options...); err != nil {
		return err
	}
	for i := range statefulSets.Items {
		sts := &statefulSets.Items[i]
		if resources := byName[sts.Labels[leaderworkersetv1.SetNameLabelKey]]; resources != nil {
			resources.workerSets = append(resources.workerSets, *sts)
		}
		if resources := byName[sts.Name]; resources != nil && resources.lws.Spec.GroupIdentity != leaderworkersetv1.GroupIdentityHash {
			resources.leader = sts
		}
	}
	if hasHash {
		var deployments appsv1.DeploymentList
		if err := listAll(ctx, reader, &deployments, options...); err != nil {
			return err
		}
		for i := range deployments.Items {
			deployment := &deployments.Items[i]
			if resources := byName[deployment.Name]; resources != nil && resources.lws.Spec.GroupIdentity == leaderworkersetv1.GroupIdentityHash {
				resources.leader = deployment
			}
		}
	}
	// Observe finds leaders by name even when their label is missing or wrong.
	// Preserve that contract, reading fallback Deployments before ReplicaSets.
	for _, resources := range byName {
		if resources.leader != nil {
			continue
		}
		var leader client.Object = &appsv1.StatefulSet{}
		if resources.lws.Spec.GroupIdentity == leaderworkersetv1.GroupIdentityHash {
			leader = &appsv1.Deployment{}
		}
		if err := reader.Get(ctx, client.ObjectKeyFromObject(resources.lws), leader); err != nil {
			if client.IgnoreNotFound(err) != nil {
				return fmt.Errorf("reading leader workload: %w", err)
			}
			continue
		}
		resources.leader = leader
	}
	if hasHash {
		var replicaSets appsv1.ReplicaSetList
		if err := listAll(ctx, reader, &replicaSets, options...); err != nil {
			return err
		}
		for _, rs := range replicaSets.Items {
			if resources := byName[rs.Labels[leaderworkersetv1.SetNameLabelKey]]; resources != nil {
				resources.replicaSets = append(resources.replicaSets, rs)
			}
		}
	}
	var pods corev1.PodList
	if err := listAll(ctx, reader, &pods, options...); err != nil {
		return err
	}
	for _, pod := range pods.Items {
		if resources := byName[pod.Labels[leaderworkersetv1.SetNameLabelKey]]; resources != nil {
			resources.pods = append(resources.pods, pod)
		}
	}
	for _, resources := range byName {
		snapshot, err := buildSnapshot(resources.lws, resources.leader, resources.replicaSets, resources.workerSets, resources.pods)
		if err != nil {
			return err
		}
		result[resources.lws.UID] = snapshot
	}
	return nil
}

// listAll bounds individual responses without treating a partial page as a
// complete observation (which could grant credit to groups awaiting deletion).
func listAll(ctx context.Context, reader client.Reader, list client.ObjectList, options ...client.ListOption) error {
	query := (&client.ListOptions{}).ApplyOptions(options)
	if query.Raw != nil {
		query.Raw = query.Raw.DeepCopy()
	}
	query.Limit, query.Continue = 500, ""
	pages := pager.New(func(ctx context.Context, pageOptions metav1.ListOptions) (runtime.Object, error) {
		// A fresh destination keeps prior Items and continuation metadata out
		// of the next decode. list stays empty until every page has been read.
		page := list.DeepCopyObject().(client.ObjectList)
		request := *query
		request.Raw = &pageOptions
		request.Limit, request.Continue = pageOptions.Limit, pageOptions.Continue
		return page, reader.List(ctx, page, &request)
	})
	// An expired observation needs a retry, not an unbounded replacement read.
	pages.FullListIfExpired = false
	all, _, err := pages.List(ctx, *query.AsListOptions())
	if err != nil {
		return fmt.Errorf("listing %T: %w", list, err)
	}
	items, err := meta.ExtractList(all)
	if err != nil {
		return err
	}
	return meta.SetList(list, items)
}

// buildSnapshot is shared by single and batched observations. Descendant lists
// are scoped to one LWS by label; owner UIDs, not labels, establish membership.
func buildSnapshot(lws *leaderworkersetv1.LeaderWorkerSet, workload client.Object,
	replicaSets []appsv1.ReplicaSet, workerSets []appsv1.StatefulSet, pods []corev1.Pod,
) (*Snapshot, error) {
	snapshot := &Snapshot{LWS: lws}
	if workload == nil || !metav1.IsControlledBy(workload, lws) {
		return snapshot, nil
	}
	hash := lws.Spec.GroupIdentity == leaderworkersetv1.GroupIdentityHash
	leaderOwners := make(map[types.UID]bool)
	switch workload := workload.(type) {
	case *appsv1.StatefulSet:
		snapshot.LeaderStatefulSet = workload
		leaderOwners[workload.UID] = true
	case *appsv1.Deployment:
		snapshot.LeaderDeployment = workload
		for i := range replicaSets {
			rs := &replicaSets[i]
			if metav1.IsControlledBy(rs, workload) {
				snapshot.ReplicaSets = append(snapshot.ReplicaSets, rs)
				leaderOwners[rs.UID] = true
			}
		}
		slices.SortFunc(snapshot.ReplicaSets, func(a, b *appsv1.ReplicaSet) int { return cmp.Compare(a.Name, b.Name) })
	}
	workersByName := make(map[string]*appsv1.StatefulSet, len(workerSets))
	for i := range workerSets {
		workersByName[workerSets[i].Name] = &workerSets[i]
	}
	podsByOwner := make(map[types.UID][]*corev1.Pod)
	for i := range pods {
		pod := &pods[i]
		if owner := metav1.GetControllerOf(pod); owner != nil {
			podsByOwner[owner.UID] = append(podsByOwner[owner.UID], pod)
		}
	}
	for i := range pods {
		leader := &pods[i]
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

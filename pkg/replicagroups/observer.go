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
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	toolscache "k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	leaderworkersetv1 "sigs.k8s.io/lws/api/leaderworkerset/v1"
	podutils "sigs.k8s.io/lws/pkg/utils/pod"
	statefulsetutils "sigs.k8s.io/lws/pkg/utils/statefulset"
)

// Observer owns a cold-on-leadership informer cache. Its initial lists are
// ordered and consistent, so a new leader does not start from a warm standby's
// stale view. Reads after startup use only the cache.
type Observer struct {
	cache   cache.Cache
	ready   chan struct{}
	changed func(*leaderworkersetv1.LeaderWorkerSet)
	// Set by Source before startup; nil for a read-only observer.
	queueReady <-chan struct{}
}

const setNameIndex = "replicagroups.setName"

// NewObserver constructs the shared reader; register it with mgr.Add before
// starting the manager. Use Source to enqueue changes in a controller.
// Consumers still own write tracking and their rollout budgets.
func NewObserver(mgr manager.Manager) (*Observer, error) {
	selector, err := labels.Parse(leaderworkersetv1.SetNameLabelKey)
	if err != nil {
		return nil, err
	}
	c, err := cache.New(mgr.GetConfig(), cache.Options{
		Scheme: mgr.GetScheme(), Mapper: mgr.GetRESTMapper(), HTTPClient: mgr.GetHTTPClient(),
		NewInformer: consistentInitialInformer, DefaultTransform: cache.TransformStripManagedFields(),
		// Pods and ReplicaSets are always selected by the LWS label. Keep the
		// leader workloads unfiltered: Observe also resolves those by name.
		// ByObject configures cache shells, leaving informer registration lazy.
		ByObject: map[client.Object]cache.ByObject{
			&corev1.Pod{}:        {Label: selector},
			&appsv1.ReplicaSet{}: {Label: selector},
		},
	})
	if err != nil {
		return nil, err
	}
	return &Observer{cache: c, ready: make(chan struct{})}, nil
}

// Source maps read-only LWS notifications to controller requests. Configure it
// once before starting the manager. The observer waits for the queue before
// replaying initial events; direct Add calls deduplicate bursts without loss.
// Returning false from request ignores an LWS unrelated to the consumer.
func (o *Observer) Source(request func(*leaderworkersetv1.LeaderWorkerSet) (reconcile.Request, bool)) source.Source {
	ready := make(chan struct{})
	o.queueReady = ready
	return source.Func(func(_ context.Context, queue workqueue.TypedRateLimitingInterface[reconcile.Request]) error {
		o.changed = func(lws *leaderworkersetv1.LeaderWorkerSet) {
			if key, ok := request(lws); ok {
				queue.Add(key)
			}
		}
		close(ready)
		return nil
	})
}

// A reflector's legacy initial LIST uses RV=0 (possibly stale). Bootstrap must
// include the previous leader's writes, even when streaming lists are disabled.
// This changes the informer's LIST semantics, not the reconciliation read path.
func consistentInitialInformer(lw toolscache.ListerWatcher, object runtime.Object, resync time.Duration, indexes toolscache.Indexers) toolscache.SharedIndexInformer {
	watcher := toolscache.ToListerWatcherWithContext(lw)
	return toolscache.NewSharedIndexInformer(&toolscache.ListWatch{
		ListWithContextFunc: func(ctx context.Context, options metav1.ListOptions) (runtime.Object, error) {
			if options.ResourceVersion == "0" {
				options.ResourceVersion = ""
			}
			return watcher.ListWithContext(ctx, options)
		},
		WatchFuncWithContext: watcher.WatchWithContext,
	}, object, resync, indexes)
}

func (*Observer) NeedLeaderElection() bool { return true }

func (o *Observer) Start(ctx context.Context) error {
	if o.queueReady != nil {
		select {
		case <-ctx.Done():
			return nil
		case <-o.queueReady:
		}
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	finished := make(chan error, 1)
	go func() { finished <- o.cache.Start(ctx) }()
	// Register lazily and in causal order. The initial Pod list follows the
	// workload lists; subsequent observations use only these running informers.
	for _, object := range []client.Object{
		&leaderworkersetv1.LeaderWorkerSet{}, &appsv1.StatefulSet{},
		&appsv1.Deployment{}, &appsv1.ReplicaSet{}, &corev1.Pod{},
	} {
		informer, err := o.cache.GetInformer(ctx, object)
		if err != nil {
			return err
		}
		// GetInformer can return before syncing if cache.Start has not entered
		// its goroutine yet. Explicitly wait before starting the next kind.
		if !toolscache.WaitForCacheSync(ctx.Done(), informer.HasSynced) {
			return ctx.Err()
		}
		if err := o.cache.IndexField(ctx, object, setNameIndex, func(object client.Object) []string {
			if name := object.GetLabels()[leaderworkersetv1.SetNameLabelKey]; name != "" {
				return []string{name}
			}
			return nil
		}); err != nil {
			return err
		}
		if _, err := informer.AddEventHandler(toolscache.ResourceEventHandlerFuncs{
			AddFunc: o.notify, UpdateFunc: func(_, current any) { o.notify(current) }, DeleteFunc: o.notify,
		}); err != nil {
			return err
		}
	}
	close(o.ready)
	return <-finished
}

func (o *Observer) waitForStartup(ctx context.Context) error {
	select {
	case <-o.ready:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (o *Observer) Get(ctx context.Context, key client.ObjectKey, object client.Object, options ...client.GetOption) error {
	if err := o.waitForStartup(ctx); err != nil {
		return err
	}
	return o.cache.Get(ctx, key, object, options...)
}

func (o *Observer) List(ctx context.Context, objects client.ObjectList, options ...client.ListOption) error {
	if err := o.waitForStartup(ctx); err != nil {
		return err
	}
	query := (&client.ListOptions{}).ApplyOptions(options)
	if query.LabelSelector != nil {
		if name, exact := query.LabelSelector.RequiresExactMatch(leaderworkersetv1.SetNameLabelKey); exact {
			indexed := fields.OneTermEqualSelector(setNameIndex, name)
			if query.FieldSelector == nil {
				query.FieldSelector = indexed
			} else {
				query.FieldSelector = fields.AndSelectors(query.FieldSelector, indexed)
			}
		}
	}
	return o.cache.List(ctx, objects, query)
}

func (o *Observer) notify(value any) {
	if tombstone, ok := value.(toolscache.DeletedFinalStateUnknown); ok {
		value = tombstone.Obj
	}
	object, ok := value.(client.Object)
	if !ok || o.changed == nil {
		return
	}
	lws, ok := object.(*leaderworkersetv1.LeaderWorkerSet)
	if !ok {
		name := object.GetLabels()[leaderworkersetv1.SetNameLabelKey]
		if name == "" {
			return
		}
		lws = &leaderworkersetv1.LeaderWorkerSet{}
		if err := o.cache.Get(context.Background(), client.ObjectKey{Namespace: object.GetNamespace(), Name: name}, lws, client.UnsafeDisableDeepCopy); err != nil {
			return
		}
	}
	o.changed(lws)
}

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

// Observe reads one LWS and its UID-verified descendants from an informer-backed
// reader. Observer supplies indexed lookups; no API batches or pagination are
// needed. This is not an atomic snapshot or a read-after-write guarantee:
// mutation consumers must track their writes, check ScalingObserved, and guard
// writes with UID/resource-version preconditions.
//
// Missing or replaced LWS return nil, not zero availability. Read errors or
// malformed group sizes return no snapshot. Results are read-only; callers
// planning from an earlier LWS must also check its generation.
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
	return buildSnapshot(lws, workload, itemPointers(replicaSets.Items), itemPointers(workerSets.Items), itemPointers(pods.Items))
}

// itemPointers shares the list's read-only objects without copying their structs.
func itemPointers[T any](items []T) []*T {
	pointers := make([]*T, len(items))
	for i := range items {
		pointers[i] = &items[i]
	}
	return pointers
}

// buildSnapshot scopes descendants by owner UID, not merely their LWS label.
func buildSnapshot(lws *leaderworkersetv1.LeaderWorkerSet, workload client.Object,
	replicaSets []*appsv1.ReplicaSet, workerSets []*appsv1.StatefulSet, pods []*corev1.Pod,
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
		for _, rs := range replicaSets {
			if metav1.IsControlledBy(rs, workload) {
				snapshot.ReplicaSets = append(snapshot.ReplicaSets, rs)
				leaderOwners[rs.UID] = true
			}
		}
		slices.SortFunc(snapshot.ReplicaSets, func(a, b *appsv1.ReplicaSet) int { return cmp.Compare(a.Name, b.Name) })
	}
	workersByName := make(map[string]*appsv1.StatefulSet, len(workerSets))
	for _, workers := range workerSets {
		workersByName[workers.Name] = workers
	}
	podsByOwner := make(map[types.UID][]*corev1.Pod)
	for _, pod := range pods {
		if owner := metav1.GetControllerOf(pod); owner != nil {
			podsByOwner[owner.UID] = append(podsByOwner[owner.UID], pod)
		}
	}
	for _, leader := range pods {
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
	if expected == nil || expected.UID == "" || expected.Name == "" || expected.Namespace == "" {
		return nil, fmt.Errorf("observing replica groups requires an LWS namespace, name and UID")
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

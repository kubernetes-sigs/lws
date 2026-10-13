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
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	toolscache "k8s.io/client-go/tools/cache"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	disaggregatedsetv1 "sigs.k8s.io/lws/api/disaggregatedset/v1"
	leaderworkersetv1 "sigs.k8s.io/lws/api/leaderworkerset/v1"
)

// A receipt identifies our write without comparing opaque resource versions or
// waiting to see one exact version that a watch is allowed to coalesce away.
const observationReceiptAnnotation = "disaggregatedset.x-k8s.io/observation-receipt"

// informerObservation owns a cold-on-leadership cache. A restarted/warm standby
// must first observe the previous leader's writes; process-local expectations
// alone cannot provide that guarantee. After startup, expectations prevent our
// own writes from being forgotten while the informers catch up.
type informerObservation struct {
	cache   cache.Cache
	ready   chan struct{}
	mu      sync.Mutex
	writes  map[client.ObjectKey]*observedWrite
	enqueue func(client.Object)
}

type observedWrite struct {
	owner   types.UID
	parent  client.ObjectKey
	slice   string
	uid     types.UID
	receipt string
	deleted bool
	// Replaying the same conditional mutation resolves an ambiguous transport
	// error. We never turn a timeout into permission to forget a possible write.
	retry func(context.Context) error
}

func newInformerObservation(mgr manager.Manager) (*informerObservation, error) {
	c, err := cache.New(mgr.GetConfig(), cache.Options{
		Scheme: mgr.GetScheme(), Mapper: mgr.GetRESTMapper(), HTTPClient: mgr.GetHTTPClient(),
		NewInformer: consistentInitialInformer,
	})
	if err != nil {
		return nil, err
	}
	return &informerObservation{cache: c, ready: make(chan struct{}), writes: make(map[client.ObjectKey]*observedWrite)}, nil
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

func (*informerObservation) NeedLeaderElection() bool { return true }

func (o *informerObservation) Start(ctx context.Context) error {
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
		if _, err := informer.AddEventHandler(toolscache.ResourceEventHandlerFuncs{
			AddFunc: o.changed, UpdateFunc: func(_, current any) { o.changed(current) }, DeleteFunc: o.changed,
		}); err != nil {
			return err
		}
	}
	close(o.ready)
	return <-finished
}

func (o *informerObservation) waitForStartup(ctx context.Context) error {
	select {
	case <-o.ready:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (o *informerObservation) Get(ctx context.Context, key client.ObjectKey, object client.Object, options ...client.GetOption) error {
	if err := o.waitForStartup(ctx); err != nil {
		return err
	}
	return o.cache.Get(ctx, key, object, options...)
}

func (o *informerObservation) List(ctx context.Context, objects client.ObjectList, options ...client.ListOption) error {
	if err := o.waitForStartup(ctx); err != nil {
		return err
	}
	return o.cache.List(ctx, objects, options...)
}

func (o *informerObservation) mutate(ctx context.Context, lws *leaderworkersetv1.LeaderWorkerSet, deleting bool, apply func(context.Context) error) error {
	owner := metav1.GetControllerOf(lws)
	if owner == nil {
		return errors.New("observed writes require a controller-owned LeaderWorkerSet")
	}
	key := client.ObjectKeyFromObject(lws)
	o.mu.Lock()
	previous, exists := o.writes[key]
	var previousWrite observedWrite
	if exists {
		previousWrite = *previous
	}
	o.mu.Unlock()
	if exists {
		if err := o.checkWrite(ctx, key, previousWrite); err != nil {
			return err
		}
	}
	written := &observedWrite{owner: owner.UID, parent: client.ObjectKey{Namespace: lws.Namespace, Name: owner.Name}, slice: lws.Labels[disaggregatedsetv1.SliceLabelKey], uid: lws.UID, deleted: deleting}
	if !deleting {
		if lws.Annotations == nil {
			lws.Annotations = make(map[string]string)
		}
		written.receipt = uuid.NewString()
		lws.Annotations[observationReceiptAnnotation] = written.receipt
	}
	// Record before the request, including when its outcome is not known.
	o.mu.Lock()
	o.writes[key] = written
	o.mu.Unlock()
	err := apply(ctx)
	o.mu.Lock()
	defer o.mu.Unlock()
	if err == nil {
		written.uid = lws.UID // Create returns the persisted UID.
	} else if apierrors.IsConflict(err) || apierrors.IsInvalid(err) || apierrors.IsForbidden(err) || apierrors.IsAlreadyExists(err) || apierrors.IsNotFound(err) {
		delete(o.writes, key) // This request definitively did not mutate the object.
	} else {
		written.retry = apply
	}
	return err
}

// Once the parent has disappeared from its informer, it cannot authorize more
// rollout steps. Release its ledger entries instead of leaking them forever.
func (o *informerObservation) forget(parent client.ObjectKey) {
	o.mu.Lock()
	defer o.mu.Unlock()
	for key, written := range o.writes {
		if written.parent == parent {
			delete(o.writes, key)
		}
	}
}

// observed checks a whole slice, not just the next LWS to change. Otherwise a
// delayed update to A could authorize another deletion in B using A's old credit.
func (o *informerObservation) observed(ctx context.Context, owner types.UID, slice string) error {
	o.mu.Lock()
	writes := make(map[client.ObjectKey]observedWrite)
	for key, written := range o.writes {
		if written.owner == owner && (slice == "" || written.slice == slice) {
			writes[key] = *written
		}
	}
	o.mu.Unlock()
	for key, written := range writes {
		if err := o.checkWrite(ctx, key, written); err != nil {
			return err
		}
	}
	return nil
}

func (o *informerObservation) checkWrite(ctx context.Context, key client.ObjectKey, written observedWrite) error {
	lws := &leaderworkersetv1.LeaderWorkerSet{}
	err := o.Get(ctx, key, lws)
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	seen := written.deleted && (apierrors.IsNotFound(err) || lws.UID != written.uid || !lws.DeletionTimestamp.IsZero())
	seen = seen || err == nil && !written.deleted &&
		(written.uid == "" || lws.UID == written.uid) && lws.Annotations[observationReceiptAnnotation] == written.receipt
	if seen {
		o.mu.Lock()
		if current := o.writes[key]; current != nil && current.receipt == written.receipt && current.uid == written.uid {
			delete(o.writes, key)
		}
		o.mu.Unlock()
		return nil
	}
	if written.retry != nil {
		// A conflict may mean the first request succeeded. Retain the
		// expectation until its receipt is visible, not until a timer expires.
		err := written.retry(ctx)
		if err != nil && !apierrors.IsConflict(err) && !apierrors.IsAlreadyExists(err) {
			return err
		}
		if err == nil {
			o.mu.Lock()
			if current := o.writes[key]; current != nil && current.receipt == written.receipt {
				current.retry = nil // Accepted; only the watch is outstanding now.
			}
			o.mu.Unlock()
		}
	}
	return fmt.Errorf("waiting to observe write to %s: %w", key, errReplicaGroupsPending)
}

// Workload and Pod events wake the parent directly; we do not need an LWS status
// update or a fixed polling delay to notice that a scale operation caught up.
func (o *informerObservation) changed(value any) {
	if tombstone, ok := value.(toolscache.DeletedFinalStateUnknown); ok {
		value = tombstone.Obj
	}
	object, ok := value.(client.Object)
	if !ok || o.enqueue == nil {
		return
	}
	lws, ok := object.(*leaderworkersetv1.LeaderWorkerSet)
	if !ok {
		name := object.GetLabels()[leaderworkersetv1.SetNameLabelKey]
		if name == "" {
			return
		}
		lws = &leaderworkersetv1.LeaderWorkerSet{}
		if err := o.cache.Get(context.Background(), client.ObjectKey{Namespace: object.GetNamespace(), Name: name}, lws); err != nil {
			return
		}
	}
	owner := metav1.GetControllerOf(lws)
	if owner != nil && owner.Kind == "DisaggregatedSet" && owner.APIVersion == disaggregatedsetv1.GroupVersion.String() {
		o.enqueue(&disaggregatedsetv1.DisaggregatedSet{ObjectMeta: metav1.ObjectMeta{Name: owner.Name, Namespace: lws.Namespace}})
	}
}

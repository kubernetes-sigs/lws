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

package computedomain

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"strconv"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/workqueue"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	disaggregatedsetv1 "sigs.k8s.io/lws/api/disaggregatedset/v1"
	leaderworkersetv1 "sigs.k8s.io/lws/api/leaderworkerset/v1"
	podutils "sigs.k8s.io/lws/pkg/utils/pod"
)

const (
	// PodClaimTemplateIndex indexes Pods by referenced ResourceClaimTemplateName
	// so ComputeDomain reference checks run in O(1) against the local cache.
	PodClaimTemplateIndex = "leaderworkerset.sigs.k8s.io/pod-compute-domain-claim-template"

	// OwnerUIDLabelKey records the UID of the LeaderWorkerSet or
	// DisaggregatedSet that manages a ComputeDomain. It replaces an owner
	// reference: garbage collection would delete the ComputeDomain as soon as
	// its owner is gone, and the NVIDIA controller tears down the IMEX DaemonSet
	// on deletionTimestamp, while the workload's Pods are still draining.
	OwnerUIDLabelKey = "leaderworkerset.sigs.k8s.io/compute-domain-owner-uid"
)

// ErrUnexpectedComputeDomainOwner indicates that a ComputeDomain with the
// desired name already exists and is not managed by the expected workload.
var ErrUnexpectedComputeDomainOwner = errors.New("unexpected ComputeDomain owner")

// ErrComputeDomainTerminating indicates that the desired ComputeDomain exists
// but is being deleted.
var ErrComputeDomainTerminating = errors.New("ComputeDomain is terminating")

// SetupIndexes registers the Pod ResourceClaimTemplateName field index used for
// ComputeDomain cleanup and finalizer release.
func SetupIndexes(indexer client.FieldIndexer) error {
	return indexer.IndexField(context.Background(), &corev1.Pod{}, PodClaimTemplateIndex, PodClaimTemplateNames)
}

// PodClaimTemplateNames returns the ResourceClaimTemplate names a Pod
// references. It is the extractor for PodClaimTemplateIndex.
func PodClaimTemplateNames(raw client.Object) []string {
	pod, ok := raw.(*corev1.Pod)
	if !ok || len(pod.Spec.ResourceClaims) == 0 {
		return nil
	}
	var names []string
	for i := range pod.Spec.ResourceClaims {
		if tpl := pod.Spec.ResourceClaims[i].ResourceClaimTemplateName; tpl != nil && *tpl != "" {
			names = append(names, *tpl)
		}
	}
	return names
}

// VerifyAPI checks that resource.nvidia.com/v1beta1 ComputeDomain is served by
// the cluster API server.
func VerifyAPI(restMapper apimeta.RESTMapper) error {
	if restMapper == nil {
		return nil
	}
	if _, err := restMapper.RESTMapping(GroupKind, Version); err != nil {
		return fmt.Errorf("resource.nvidia.com/v1beta1 ComputeDomain API is not available: %w", err)
	}
	return nil
}

// EnsureReplicaComputeDomain ensures the ComputeDomain backing leaderPod's
// replica exists, managed by lws. The NVIDIA ComputeDomain controller tears down
// the IMEX DaemonSet and ResourceClaimTemplate as soon as deletionTimestamp is
// set, so no ComputeDomain is deleted while a Pod references it:
// CleanupLWSComputeDomains deletes an obsolete one once its leader is gone, and
// Reconciler deletes one whose LeaderWorkerSet is gone.
//
// A terminating ComputeDomain blocks a gated leader, but not one already
// admitted: its group is running, and returning an error would stop the pod
// reconciler from reaching the worker StatefulSet.
func EnsureReplicaComputeDomain(ctx context.Context, c client.Client, lws *leaderworkersetv1.LeaderWorkerSet, leaderPod *corev1.Pod) error {
	cdName := LeaderComputeDomainName(leaderPod)
	if cdName == "" {
		return nil
	}
	labels := map[string]string{
		leaderworkersetv1.SetNameLabelKey:    lws.Name,
		leaderworkersetv1.GroupIndexLabelKey: leaderPod.Labels[leaderworkersetv1.GroupIndexLabelKey],
		leaderworkersetv1.RevisionKey:        leaderPod.Labels[leaderworkersetv1.RevisionKey],
		OwnerUIDLabelKey:                     string(lws.UID),
	}

	err := ensureComputeDomain(ctx, c, lws.Namespace, cdName, labels, "LeaderWorkerSet", lws.Name)
	if errors.Is(err, ErrComputeDomainTerminating) && !podutils.HasSchedulingGate(leaderPod, leaderworkersetv1.GroupReplacementSchedulingGate) {
		return nil
	}
	return err
}

// EnsureSliceComputeDomain ensures the ComputeDomain backing a DisaggregatedSet
// (slice, revision) exists before child LeaderWorkerSets scale up. A terminating
// ComputeDomain only blocks a slice that has no children for the revision yet:
// existing children run against it, and blocking would stop scaling and
// rollout until a new revision.
func EnsureSliceComputeDomain(ctx context.Context, c client.Client, ds *disaggregatedsetv1.DisaggregatedSet, slice int, revision string, hasChildren bool) error {
	cdName := SliceComputeDomainName(ds, slice, revision)
	labels := map[string]string{
		disaggregatedsetv1.SetNameLabelKey:  ds.Name,
		disaggregatedsetv1.SliceLabelKey:    strconv.Itoa(slice),
		disaggregatedsetv1.RevisionLabelKey: revision,
		OwnerUIDLabelKey:                    string(ds.UID),
	}
	err := ensureComputeDomain(ctx, c, ds.Namespace, cdName, labels, "DisaggregatedSet", ds.Name)
	if errors.Is(err, ErrComputeDomainTerminating) && hasChildren {
		return nil
	}
	return err
}

// ensureComputeDomain creates cdName or repairs its metadata. labels must carry
// OwnerUIDLabelKey; an existing ComputeDomain with another owner UID is not
// adopted.
func ensureComputeDomain(ctx context.Context, c client.Client, namespace, cdName string, labels map[string]string, ownerKind, ownerName string) error {
	existing := &ComputeDomain{}
	err := c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: cdName}, existing)
	if err != nil {
		if !apierrors.IsNotFound(err) {
			return fmt.Errorf("get ComputeDomain %s/%s: %w", namespace, cdName, err)
		}
		cd := &ComputeDomain{
			ObjectMeta: metav1.ObjectMeta{
				Name:       cdName,
				Namespace:  namespace,
				Labels:     maps.Clone(labels),
				Finalizers: []string{leaderworkersetv1.ComputeDomainProtectionFinalizer},
			},
			Spec: ComputeDomainSpec{
				NumNodes: 0,
				Channel: &ComputeDomainChannelSpec{
					ResourceClaimTemplate: ComputeDomainResourceClaimTemplate{
						Name: cdName,
					},
				},
			},
		}
		if createErr := c.Create(ctx, cd); createErr != nil {
			if !apierrors.IsAlreadyExists(createErr) {
				return fmt.Errorf("create ComputeDomain %s/%s: %w", namespace, cdName, createErr)
			}
			if getErr := c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: cdName}, existing); getErr != nil {
				return fmt.Errorf("get ComputeDomain %s/%s after create conflict: %w", namespace, cdName, getErr)
			}
		} else {
			return nil
		}
	}

	if existing.Labels[OwnerUIDLabelKey] != labels[OwnerUIDLabelKey] {
		return fmt.Errorf("%w: ComputeDomain %s/%s is not managed by %s %s", ErrUnexpectedComputeDomainOwner, namespace, cdName, ownerKind, ownerName)
	}
	if !existing.DeletionTimestamp.IsZero() {
		return fmt.Errorf("%w: %s/%s", ErrComputeDomainTerminating, namespace, cdName)
	}

	needFinalizer := !controllerutil.ContainsFinalizer(existing, leaderworkersetv1.ComputeDomainProtectionFinalizer)
	needLabels := !hasLabels(existing.Labels, labels)
	if !needFinalizer && !needLabels {
		return nil
	}

	// Patch metadata only so immutable ComputeDomain.spec (including any fields
	// defaulted by the CRD schema such as allocationMode) is never touched.
	before := existing.DeepCopy()
	if needFinalizer {
		controllerutil.AddFinalizer(existing, leaderworkersetv1.ComputeDomainProtectionFinalizer)
	}
	if needLabels {
		if existing.Labels == nil {
			existing.Labels = make(map[string]string, len(labels))
		}
		maps.Copy(existing.Labels, labels)
	}
	if err := c.Patch(ctx, existing, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
		return fmt.Errorf("patch ComputeDomain %s/%s metadata: %w", namespace, cdName, err)
	}
	return nil
}

func hasLabels(actual, expected map[string]string) bool {
	for k, v := range expected {
		if actual[k] != v {
			return false
		}
	}
	return true
}

// CleanupLWSComputeDomains deletes obsolete LWS-owned ComputeDomains once no
// active leader Pod claims them and no Pod references their ResourceClaimTemplate.
func CleanupLWSComputeDomains(ctx context.Context, c client.Client, lws *leaderworkersetv1.LeaderWorkerSet) error {
	owned, err := listManagedComputeDomains(ctx, c, lws.Namespace, leaderworkersetv1.SetNameLabelKey, lws.Name, lws.UID)
	if err != nil {
		return fmt.Errorf("list ComputeDomains for LeaderWorkerSet %s/%s: %w", lws.Namespace, lws.Name, err)
	}
	if len(owned) == 0 {
		return nil
	}

	desired := make(map[string]struct{})
	if lws.DeletionTimestamp.IsZero() {
		leaders := &corev1.PodList{}
		if err := c.List(ctx, leaders, client.InNamespace(lws.Namespace), client.MatchingLabels{
			leaderworkersetv1.SetNameLabelKey:     lws.Name,
			leaderworkersetv1.WorkerIndexLabelKey: "0",
		}); err != nil {
			return fmt.Errorf("list leader Pods for LeaderWorkerSet %s/%s before ComputeDomain cleanup: %w", lws.Namespace, lws.Name, err)
		}
		for i := range leaders.Items {
			pod := &leaders.Items[i]
			if !pod.DeletionTimestamp.IsZero() {
				continue
			}
			if cdName := LeaderComputeDomainName(pod); cdName != "" {
				desired[cdName] = struct{}{}
			}
		}
	}

	for _, cd := range owned {
		if _, keep := desired[cd.Name]; keep {
			continue
		}
		if err := releaseAndDeleteIfUnreferenced(ctx, c, cd); err != nil {
			return err
		}
	}
	return nil
}

// CleanupDisaggregatedSetComputeDomains deletes obsolete DisaggregatedSet-owned
// ComputeDomains once their (slice, revision) has no remaining child LWSs and
// no Pod references their ResourceClaimTemplate.
func CleanupDisaggregatedSetComputeDomains(
	ctx context.Context,
	c client.Client,
	ds *disaggregatedsetv1.DisaggregatedSet,
	childLWS []*leaderworkersetv1.LeaderWorkerSet,
	desiredSlices int,
	currentRevision string,
) error {
	owned, err := listManagedComputeDomains(ctx, c, ds.Namespace, disaggregatedsetv1.SetNameLabelKey, ds.Name, ds.UID)
	if err != nil {
		return fmt.Errorf("list ComputeDomains for DisaggregatedSet %s/%s: %w", ds.Namespace, ds.Name, err)
	}
	if len(owned) == 0 {
		return nil
	}

	desired := make(map[string]struct{})
	if ds.DeletionTimestamp.IsZero() && DisaggregatedSetNeedsSliceDomain(ds) {
		for slice := range desiredSlices {
			desired[SliceComputeDomainName(ds, slice, currentRevision)] = struct{}{}
		}
	}
	for _, child := range childLWS {
		sliceIdx, err := strconv.Atoi(child.Labels[disaggregatedsetv1.SliceLabelKey])
		rev := child.Labels[disaggregatedsetv1.RevisionLabelKey]
		if err == nil && rev != "" {
			desired[SliceComputeDomainName(ds, sliceIdx, rev)] = struct{}{}
		}
	}

	for _, cd := range owned {
		if _, keep := desired[cd.Name]; keep {
			continue
		}
		if err := releaseAndDeleteIfUnreferenced(ctx, c, cd); err != nil {
			return err
		}
	}
	return nil
}

// listManagedComputeDomains lists the ComputeDomains in namespace that the
// workload named name with UID uid manages; setNameLabelKey selects the
// workload kind.
func listManagedComputeDomains(ctx context.Context, c client.Client, namespace, setNameLabelKey, name string, uid types.UID) ([]*ComputeDomain, error) {
	cdList := &ComputeDomainList{}
	if err := c.List(ctx, cdList, client.InNamespace(namespace), client.MatchingLabels{
		setNameLabelKey:  name,
		OwnerUIDLabelKey: string(uid),
	}); err != nil {
		return nil, err
	}
	owned := make([]*ComputeDomain, len(cdList.Items))
	for i := range cdList.Items {
		owned[i] = &cdList.Items[i]
	}
	return owned, nil
}

func releaseAndDeleteIfUnreferenced(ctx context.Context, c client.Client, cd *ComputeDomain) error {
	referencingPods := &corev1.PodList{}
	if err := c.List(ctx, referencingPods, client.InNamespace(cd.Namespace), client.MatchingFields{
		PodClaimTemplateIndex: cd.Name,
	}, client.Limit(1)); err != nil {
		return fmt.Errorf("list Pods referencing ComputeDomain %s/%s: %w", cd.Namespace, cd.Name, err)
	}
	if len(referencingPods.Items) > 0 {
		return nil
	}

	if controllerutil.ContainsFinalizer(cd, leaderworkersetv1.ComputeDomainProtectionFinalizer) {
		before := cd.DeepCopy()
		controllerutil.RemoveFinalizer(cd, leaderworkersetv1.ComputeDomainProtectionFinalizer)
		if err := c.Patch(ctx, cd, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
			if apierrors.IsNotFound(err) {
				return nil
			}
			return fmt.Errorf("remove protection finalizer from ComputeDomain %s/%s: %w", cd.Namespace, cd.Name, err)
		}
	}

	if cd.DeletionTimestamp.IsZero() {
		if err := c.Delete(ctx, cd, client.Preconditions{UID: &cd.UID}); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete unused ComputeDomain %s/%s: %w", cd.Namespace, cd.Name, err)
		}
	}
	return nil
}

// Reconciler deletes a managed ComputeDomain whose LeaderWorkerSet or
// DisaggregatedSet is gone once no Pod references it, and releases the
// protection finalizer from a ComputeDomain deleted by hand once no Pod
// references it. Managed ComputeDomains carry no owner reference, so deleting
// the workload does not mark them terminating while its Pods drain.
type Reconciler struct {
	client.Client
}

func NewReconciler(c client.Client) *Reconciler {
	return &Reconciler{Client: c}
}

func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	cd := &ComputeDomain{}
	if err := r.Get(ctx, req.NamespacedName, cd); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !cd.DeletionTimestamp.IsZero() {
		if !controllerutil.ContainsFinalizer(cd, leaderworkersetv1.ComputeDomainProtectionFinalizer) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, releaseAndDeleteIfUnreferenced(ctx, r.Client, cd)
	}
	gone, err := r.ownerGone(ctx, cd)
	if err != nil || !gone {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, releaseAndDeleteIfUnreferenced(ctx, r.Client, cd)
}

// ownerGone reports whether the workload recorded on a managed ComputeDomain no
// longer exists. It reads the owner from the cache: the ComputeDomain was
// created by a controller that had already observed its owner in the same
// cache, so a miss means the owner was deleted, not that the cache lags.
func (r *Reconciler) ownerGone(ctx context.Context, cd *ComputeDomain) (bool, error) {
	uid := cd.Labels[OwnerUIDLabelKey]
	if uid == "" {
		return false, nil
	}
	var owner client.Object
	var name string
	if name = cd.Labels[disaggregatedsetv1.SetNameLabelKey]; name != "" {
		owner = &disaggregatedsetv1.DisaggregatedSet{}
	} else if name = cd.Labels[leaderworkersetv1.SetNameLabelKey]; name != "" {
		owner = &leaderworkersetv1.LeaderWorkerSet{}
	} else {
		return false, nil
	}
	if err := r.Get(ctx, types.NamespacedName{Namespace: cd.Namespace, Name: name}, owner); err != nil {
		if apierrors.IsNotFound(err) {
			return true, nil
		}
		return false, fmt.Errorf("get owner of ComputeDomain %s/%s: %w", cd.Namespace, cd.Name, err)
	}
	return string(owner.GetUID()) != uid, nil
}

func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	terminating := func(obj client.Object) bool {
		return obj != nil && obj.GetDeletionTimestamp() != nil &&
			controllerutil.ContainsFinalizer(obj, leaderworkersetv1.ComputeDomainProtectionFinalizer)
	}
	managed := func(obj client.Object) bool {
		return obj != nil && obj.GetLabels()[OwnerUIDLabelKey] != ""
	}
	deleteOnly := predicate.Funcs{
		CreateFunc:  func(event.CreateEvent) bool { return false },
		UpdateFunc:  func(event.UpdateEvent) bool { return false },
		DeleteFunc:  func(event.DeleteEvent) bool { return true },
		GenericFunc: func(event.GenericEvent) bool { return false },
	}
	return ctrl.NewControllerManagedBy(mgr).
		Named("computedomain").
		For(&ComputeDomain{}, builder.WithPredicates(predicate.Funcs{
			// Create covers ComputeDomains whose owner was deleted, or that
			// started terminating, while the controller was down.
			CreateFunc:  func(e event.CreateEvent) bool { return managed(e.Object) || terminating(e.Object) },
			UpdateFunc:  func(e event.UpdateEvent) bool { return terminating(e.ObjectNew) },
			DeleteFunc:  func(event.DeleteEvent) bool { return false },
			GenericFunc: func(event.GenericEvent) bool { return false },
		})).
		Watches(&corev1.Pod{}, handler.Funcs{
			DeleteFunc: func(_ context.Context, e event.TypedDeleteEvent[client.Object], q workqueue.TypedRateLimitingInterface[reconcile.Request]) {
				enqueuePodReferencedComputeDomains(e.Object, q)
			},
		}).
		Watches(&leaderworkersetv1.LeaderWorkerSet{},
			handler.EnqueueRequestsFromMapFunc(r.managedComputeDomainRequests(leaderworkersetv1.SetNameLabelKey)),
			builder.WithPredicates(deleteOnly)).
		Watches(&disaggregatedsetv1.DisaggregatedSet{},
			handler.EnqueueRequestsFromMapFunc(r.managedComputeDomainRequests(disaggregatedsetv1.SetNameLabelKey)),
			builder.WithPredicates(deleteOnly)).
		Complete(r)
}

// managedComputeDomainRequests maps a deleted workload to the ComputeDomains it
// managed, so they are deleted once their last Pod exits.
func (r *Reconciler) managedComputeDomainRequests(setNameLabelKey string) handler.MapFunc {
	return func(ctx context.Context, obj client.Object) []reconcile.Request {
		owned, err := listManagedComputeDomains(ctx, r.Client, obj.GetNamespace(), setNameLabelKey, obj.GetName(), obj.GetUID())
		if err != nil {
			ctrl.LoggerFrom(ctx).Error(err, "Listing ComputeDomains of a deleted workload", "workload", client.ObjectKeyFromObject(obj))
			return nil
		}
		reqs := make([]reconcile.Request, len(owned))
		for i, cd := range owned {
			reqs[i] = reconcile.Request{NamespacedName: client.ObjectKeyFromObject(cd)}
		}
		return reqs
	}
}

func enqueuePodReferencedComputeDomains(obj client.Object, q interface{ Add(reconcile.Request) }) {
	pod, ok := obj.(*corev1.Pod)
	if !ok || pod == nil {
		return
	}
	for i := range pod.Spec.ResourceClaims {
		if tpl := pod.Spec.ResourceClaims[i].ResourceClaimTemplateName; tpl != nil && *tpl != "" {
			q.Add(reconcile.Request{
				NamespacedName: types.NamespacedName{
					Namespace: pod.Namespace,
					Name:      *tpl,
				},
			})
		}
	}
}

// MapComputeDomainToOwner returns a MapFunc that maps a managed ComputeDomain
// to its owning workload identified by setNameLabelKey.
func MapComputeDomainToOwner(setNameLabelKey string) handler.MapFunc {
	return func(_ context.Context, obj client.Object) []reconcile.Request {
		if obj == nil {
			return nil
		}
		labels := obj.GetLabels()
		ownerName := labels[setNameLabelKey]
		if ownerName == "" || labels[OwnerUIDLabelKey] == "" {
			return nil
		}
		return []reconcile.Request{{
			NamespacedName: types.NamespacedName{Namespace: obj.GetNamespace(), Name: ownerName},
		}}
	}
}

// EnqueueOwnerForDeletedPod resolves each ComputeDomain referenced by a deleted
// Pod from the local informer cache and enqueues the owning workload identified
// by setNameLabelKey on the ComputeDomain's labels, so unlabeled or relabeled
// Pods still wake obsolete-domain cleanup.
func EnqueueOwnerForDeletedPod(ctx context.Context, c client.Client, setNameLabelKey string, obj client.Object, q interface{ Add(reconcile.Request) }) {
	if obj == nil {
		return
	}
	tplNames := PodClaimTemplateNames(obj)
	if len(tplNames) == 0 {
		return
	}
	ns := obj.GetNamespace()
	for _, tplName := range tplNames {
		cd := &ComputeDomain{}
		if err := c.Get(ctx, types.NamespacedName{Namespace: ns, Name: tplName}, cd); err != nil {
			continue
		}
		if ownerName := cd.Labels[setNameLabelKey]; ownerName != "" && cd.Labels[OwnerUIDLabelKey] != "" {
			q.Add(reconcile.Request{
				NamespacedName: types.NamespacedName{Namespace: ns, Name: ownerName},
			})
		}
	}
}

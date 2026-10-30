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
	"k8s.io/utils/ptr"
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
)

// ErrUnexpectedComputeDomainOwner indicates that a ComputeDomain with the
// desired name already exists and is not controlled by the expected workload.
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
// replica exists. The ComputeDomain is controller-owned by the leader Pod, so
// garbage collection deletes it once that leader incarnation is gone and the
// protection finalizer holds it until the group's last Pod terminates. Owner
// deletion is not blocked: the finalizer waits for the leader, so blocking
// would deadlock a foreground delete of the leader.
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
	}
	ownerRef := metav1.OwnerReference{
		APIVersion:         corev1.SchemeGroupVersion.String(),
		Kind:               "Pod",
		Name:               leaderPod.Name,
		UID:                leaderPod.UID,
		Controller:         ptr.To(true),
		BlockOwnerDeletion: ptr.To(false),
	}

	err := ensureComputeDomain(ctx, c, lws.Namespace, cdName, labels, ownerRef)
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
	}
	ownerRef := *metav1.NewControllerRef(ds, disaggregatedsetv1.GroupVersion.WithKind("DisaggregatedSet"))
	err := ensureComputeDomain(ctx, c, ds.Namespace, cdName, labels, ownerRef)
	if errors.Is(err, ErrComputeDomainTerminating) && hasChildren {
		return nil
	}
	return err
}

func ensureComputeDomain(ctx context.Context, c client.Client, namespace, cdName string, labels map[string]string, ownerRef metav1.OwnerReference) error {
	existing := &ComputeDomain{}
	err := c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: cdName}, existing)
	if err != nil {
		if !apierrors.IsNotFound(err) {
			return fmt.Errorf("get ComputeDomain %s/%s: %w", namespace, cdName, err)
		}
		cd := &ComputeDomain{
			ObjectMeta: metav1.ObjectMeta{
				Name:            cdName,
				Namespace:       namespace,
				Labels:          maps.Clone(labels),
				Finalizers:      []string{leaderworkersetv1.ComputeDomainProtectionFinalizer},
				OwnerReferences: []metav1.OwnerReference{ownerRef},
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

	ctrlOwner := metav1.GetControllerOf(existing)
	if ctrlOwner == nil || ctrlOwner.UID != ownerRef.UID {
		return fmt.Errorf("%w: ComputeDomain %s/%s is not controlled by %s %s", ErrUnexpectedComputeDomainOwner, namespace, cdName, ownerRef.Kind, ownerRef.Name)
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

// CleanupDisaggregatedSetComputeDomains deletes obsolete DisaggregatedSet-owned
// ComputeDomains once their (slice, revision) has no remaining child LWSs. The
// protection finalizer holds each one until no Pod references it.
func CleanupDisaggregatedSetComputeDomains(
	ctx context.Context,
	c client.Client,
	ds *disaggregatedsetv1.DisaggregatedSet,
	childLWS []*leaderworkersetv1.LeaderWorkerSet,
	desiredSlices int,
	currentRevision string,
) error {
	cdList := &ComputeDomainList{}
	if err := c.List(ctx, cdList, client.InNamespace(ds.Namespace), client.MatchingLabels{
		disaggregatedsetv1.SetNameLabelKey: ds.Name,
	}); err != nil {
		return fmt.Errorf("list ComputeDomains for DisaggregatedSet %s/%s: %w", ds.Namespace, ds.Name, err)
	}
	owned := make([]*ComputeDomain, 0, len(cdList.Items))
	for i := range cdList.Items {
		if metav1.IsControlledBy(&cdList.Items[i], ds) {
			owned = append(owned, &cdList.Items[i])
		}
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
		if _, keep := desired[cd.Name]; keep || !cd.DeletionTimestamp.IsZero() {
			continue
		}
		if err := c.Delete(ctx, cd); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete unused ComputeDomain %s/%s: %w", cd.Namespace, cd.Name, err)
		}
	}
	return nil
}

func releaseIfUnreferenced(ctx context.Context, c client.Client, cd *ComputeDomain) error {
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
	return nil
}

// Reconciler releases the LWS ComputeDomain protection finalizer from a
// terminating ComputeDomain once no Pod references it. Garbage collection of
// the owning leader Pod or DisaggregatedSet, or slice cleanup, starts the
// deletion.
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
	if cd.DeletionTimestamp.IsZero() || !controllerutil.ContainsFinalizer(cd, leaderworkersetv1.ComputeDomainProtectionFinalizer) {
		return ctrl.Result{}, nil
	}
	return ctrl.Result{}, releaseIfUnreferenced(ctx, r.Client, cd)
}

func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	terminating := func(obj client.Object) bool {
		return obj != nil && obj.GetDeletionTimestamp() != nil &&
			controllerutil.ContainsFinalizer(obj, leaderworkersetv1.ComputeDomainProtectionFinalizer)
	}
	return ctrl.NewControllerManagedBy(mgr).
		Named("computedomain").
		For(&ComputeDomain{}, builder.WithPredicates(predicate.Funcs{
			// Create covers ComputeDomains that started terminating while the
			// controller was down.
			CreateFunc:  func(e event.CreateEvent) bool { return terminating(e.Object) },
			UpdateFunc:  func(e event.UpdateEvent) bool { return terminating(e.ObjectNew) },
			DeleteFunc:  func(event.DeleteEvent) bool { return false },
			GenericFunc: func(event.GenericEvent) bool { return false },
		})).
		Watches(&corev1.Pod{}, handler.Funcs{
			DeleteFunc: func(_ context.Context, e event.TypedDeleteEvent[client.Object], q workqueue.TypedRateLimitingInterface[reconcile.Request]) {
				enqueuePodReferencedComputeDomains(e.Object, q)
			},
		}).
		Complete(r)
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

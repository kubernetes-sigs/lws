/*
Copyright 2025 The Kubernetes Authors.

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
	"fmt"
	"math"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	disaggregatedsetv1 "sigs.k8s.io/lws/api/disaggregatedset/v1"
	leaderworkersetv1 "sigs.k8s.io/lws/api/leaderworkerset/v1"
)

const (
	EventReasonScalerCreated  = "ScalerCreated"
	EventReasonScalerDeleted  = "ScalerDeleted"
	EventReasonScalerConflict = "ScalerConflict"
)

// ScalerManager auto-creates one DisaggregatedSetRoleScaler per External role or child
// and garbage-collects scalers whose role has been removed or switched back to
// Static. It also writes back status.replicas / status.selector / conditions so
// HPA sees a valid /scale target.
type ScalerManager struct {
	client client.Client
	record events.EventRecorder
}

func NewScalerManager(c client.Client, r events.EventRecorder) *ScalerManager {
	return &ScalerManager{client: c, record: r}
}

// Keys use a slash, forbidden in role names, to distinguish a child from a
// hyphenated parent. Admission rejects collisions in the generated DNS names.
func childRoleKey(parent, child string) string {
	if child == "" {
		return parent
	}
	return parent + "/" + child
}

func roleParts(key string) (parent, child string) {
	parent, child, _ = strings.Cut(key, "/")
	return
}

// roleLeaves uses the same scaling fields for an ordinary role and its children.
// An ordinary role is the single unnamed leaf; no Kubernetes object is copied.
func roleLeaves(role *disaggregatedsetv1.DisaggregatedRoleSpec) []disaggregatedsetv1.DisaggregatedSubRoleSpec {
	if len(role.SubRoles) > 0 {
		return role.SubRoles
	}
	return []disaggregatedsetv1.DisaggregatedSubRoleSpec{{Replicas: role.Spec.Replicas, Scaling: role.Scaling}}
}

func rolloutRoleNames(ds *disaggregatedsetv1.DisaggregatedSet) []string {
	var names []string
	for _, role := range ds.Spec.Roles {
		for _, child := range roleLeaves(&role) {
			names = append(names, childRoleKey(role.Name, child.Name))
		}
	}
	return names
}

// ScalerName is the deterministic name for a parent or child role's scaler.
func ScalerName(dsName, role string) string { return dsName + "-" + strings.ReplaceAll(role, "/", "-") }

// resolveDesiredReplicasByRole resolves one replica target for every role whose
// target is known. Static roles use the LWS template; External roles use their
// scaler. An External role with no owned scaler is omitted so the controller
// can pause workload reconciliation instead of guessing a target. Partitioned
// roles additionally expose a parent sum only after every child is resolved.
func resolveDesiredReplicasByRole(
	ds *disaggregatedsetv1.DisaggregatedSet,
	scalers map[string]*disaggregatedsetv1.DisaggregatedSetRoleScaler,
) map[string]int {
	desiredReplicasByRole := make(map[string]int, len(ds.Spec.Roles))
	for i := range ds.Spec.Roles {
		role := &ds.Spec.Roles[i]
		var total int64
		complete := true
		for _, leaf := range roleLeaves(role) {
			key := childRoleKey(role.Name, leaf.Name)
			count := ptr.Deref(leaf.Replicas, 1)
			if leaf.Scaling != nil && leaf.Scaling.Mode == disaggregatedsetv1.RoleScalingExternal {
				scaler := scalers[key]
				if scaler == nil {
					complete = false
					continue
				}
				count = scaler.Spec.Replicas
			}
			if count < 0 {
				complete = false
				continue
			}
			desiredReplicasByRole[key] = int(count)
			total += int64(count)
		}
		// Missing children and overflowing sums must not become smaller targets.
		if complete && total <= math.MaxInt32 {
			desiredReplicasByRole[role.Name] = int(total)
		}
	}
	return desiredReplicasByRole
}

// Every desired role should normally have a resolved target after
// ScalerManager.Reconcile. Currently, a target can remain unresolved only when
// an External role's generated scaler name is occupied by an object this
// DisaggregatedSet does not own, so the scaler can be neither created nor
// adopted. This check is a safety mechanism: callers must pause instead of
// guessing a target.
func unresolvedReplicaTargetRoles(roleNames []string, desiredReplicasByRole map[string]int) []string {
	var unresolved []string
	for _, roleName := range roleNames {
		if _, ok := desiredReplicasByRole[roleName]; !ok {
			unresolved = append(unresolved, roleName)
		}
	}
	return unresolved
}

// Reconcile ensures a scaler exists for every External role and deletes scalers
// owned by this DS whose role is no longer External. seedFor is called with a
// role name to compute the initial spec.replicas for a new scaler — typically
// the role's current LWS replica count so a Static→External flip does not
// drain a running role to 0. Returns role -> *Scaler.
func (m *ScalerManager) Reconcile(
	ctx context.Context,
	ds *disaggregatedsetv1.DisaggregatedSet,
	seedFor func(role string) (int32, error),
) (map[string]*disaggregatedsetv1.DisaggregatedSetRoleScaler, error) {
	log := logf.FromContext(ctx)

	externalRoles := make(map[string]bool)
	for i := range ds.Spec.Roles {
		role := &ds.Spec.Roles[i]
		for _, leaf := range roleLeaves(role) {
			if leaf.Scaling != nil && leaf.Scaling.Mode == disaggregatedsetv1.RoleScalingExternal {
				externalRoles[childRoleKey(role.Name, leaf.Name)] = true
			}
		}
	}

	list := &disaggregatedsetv1.DisaggregatedSetRoleScalerList{}
	if err := m.client.List(ctx, list, client.InNamespace(ds.Namespace),
		client.MatchingLabels{disaggregatedsetv1.SetNameLabelKey: ds.Name}); err != nil {
		return nil, fmt.Errorf("list scalers: %w", err)
	}

	existing := make(map[string]*disaggregatedsetv1.DisaggregatedSetRoleScaler)
	for i := range list.Items {
		s := &list.Items[i]
		if !isControlledBy(s, ds.UID) {
			continue
		}
		role := childRoleKey(s.Labels[disaggregatedsetv1.RoleLabelKey], s.Labels[disaggregatedsetv1.SubRoleLabelKey])
		if !externalRoles[role] {
			log.Info("Deleting scaler for role no longer External", "scaler", s.Name, "role", role)
			if err := m.client.Delete(ctx, s, client.Preconditions{UID: ptr.To(s.UID)}); err != nil && !apierrors.IsNotFound(err) {
				return nil, fmt.Errorf("delete scaler %s: %w", s.Name, err)
			}
			m.record.Eventf(ds, nil, corev1.EventTypeNormal, EventReasonScalerDeleted, "Delete",
				"Deleted scaler %s (role %s is no longer External)", s.Name, role)
			continue
		}
		if s.DeletionTimestamp.IsZero() {
			existing[role] = s
		}
	}

	for role := range externalRoles {
		if _, ok := existing[role]; ok {
			continue
		}
		seed := int32(0)
		if seedFor != nil {
			var err error
			seed, err = seedFor(role)
			if err != nil {
				return nil, fmt.Errorf("compute scaler seed for %s: %w", role, err)
			}
		}
		s, err := m.create(ctx, ds, role, seed)
		if err != nil {
			return nil, err
		}
		if s != nil {
			existing[role] = s
		}
	}
	return existing, nil
}

func (m *ScalerManager) create(ctx context.Context, ds *disaggregatedsetv1.DisaggregatedSet, role string, seed int32) (*disaggregatedsetv1.DisaggregatedSetRoleScaler, error) {
	name := ScalerName(ds.Name, role)
	parent, child := roleParts(role)
	scaler := &disaggregatedsetv1.DisaggregatedSetRoleScaler{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ds.Namespace,
			Labels: map[string]string{
				disaggregatedsetv1.SetNameLabelKey: ds.Name,
				disaggregatedsetv1.RoleLabelKey:    parent,
			},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion:         disaggregatedsetv1.GroupVersion.String(),
				Kind:               "DisaggregatedSet",
				Name:               ds.Name,
				UID:                ds.UID,
				Controller:         ptr.To(true),
				BlockOwnerDeletion: ptr.To(true),
			}},
		},
		Spec: disaggregatedsetv1.DisaggregatedSetRoleScalerSpec{Replicas: seed},
	}
	if child != "" {
		scaler.Labels[disaggregatedsetv1.SubRoleLabelKey] = child
	}
	if err := m.client.Create(ctx, scaler); err == nil {
		logf.FromContext(ctx).Info("Created scaler", "scaler", name, "role", role)
		m.record.Eventf(ds, nil, corev1.EventTypeNormal, EventReasonScalerCreated,
			"Create", "Created scaler %s for role %s", name, role)
		return scaler, nil
	} else if !apierrors.IsAlreadyExists(err) {
		return nil, fmt.Errorf("create scaler %s: %w", name, err)
	}
	// Name is taken. Adopt only if we already control it; otherwise emit a
	// warning and stay out.
	cur := &disaggregatedsetv1.DisaggregatedSetRoleScaler{}
	if err := m.client.Get(ctx, types.NamespacedName{Namespace: ds.Namespace, Name: name}, cur); err != nil {
		return nil, fmt.Errorf("get existing scaler %s: %w", name, err)
	}
	if !isControlledBy(cur, ds.UID) || !cur.DeletionTimestamp.IsZero() ||
		cur.Labels[disaggregatedsetv1.RoleLabelKey] != parent || cur.Labels[disaggregatedsetv1.SubRoleLabelKey] != child {
		m.record.Eventf(ds, nil, corev1.EventTypeWarning, EventReasonScalerConflict,
			"Conflict", "Scaler %s does not match a live controlled role; not adopting", name)
		return nil, nil
	}
	return cur, nil
}

// WriteStatus updates status.replicas / status.selector / observedGeneration
// / Ready condition on each controlled scaler. observedReplicas[role] is the
// aggregate pod count across all revisions for the role.
func (m *ScalerManager) WriteStatus(
	ctx context.Context,
	ds *disaggregatedsetv1.DisaggregatedSet,
	scalers map[string]*disaggregatedsetv1.DisaggregatedSetRoleScaler,
	observedReplicas map[string]int32,
) error {
	for role, s := range scalers {
		parent, child := roleParts(role)
		desired := s.DeepCopy()
		desired.Status.Replicas = observedReplicas[role]
		// Selector filters to leader pods only (worker-index=0), one per LWS
		// group. HPA's per-pod-metric averaging divides its metric sum by the
		// count of matching pods; because spec.replicas (what HPA writes) and
		// status.replicas (what HPA reads) both count LWS groups, the selector
		// must match one pod per group for the ratio math to stay consistent.
		// Users typically want to scale on the leader's signal anyway (leader
		// handles ingress; workers are downstream compute).
		desired.Status.Selector = fmt.Sprintf("%s=%s,%s=%s,%s=0",
			disaggregatedsetv1.SetNameLabelKey, ds.Name,
			disaggregatedsetv1.RoleLabelKey, parent,
			leaderworkersetv1.WorkerIndexLabelKey)
		if child != "" {
			desired.Status.Selector += fmt.Sprintf(",%s=%s", disaggregatedsetv1.SubRoleLabelKey, child)
		}
		desired.Status.ObservedGeneration = s.Generation
		apimeta.SetStatusCondition(&desired.Status.Conditions, metav1.Condition{
			Type: disaggregatedsetv1.DisaggregatedSetRoleScalerReady, Status: metav1.ConditionTrue,
			ObservedGeneration: s.Generation, Reason: "Bound",
			Message: "Scaler bound to a live DisaggregatedSet role",
		})
		if err := m.client.Status().Patch(ctx, desired, client.MergeFrom(s)); err != nil {
			return fmt.Errorf("patch scaler %s status: %w", s.Name, err)
		}
	}
	return nil
}

func isControlledBy(obj client.Object, uid types.UID) bool {
	for _, ref := range obj.GetOwnerReferences() {
		if ref.Controller != nil && *ref.Controller && ref.UID == uid {
			return true
		}
	}
	return false
}

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
	disaggregatedsetutils "sigs.k8s.io/lws/pkg/utils/disaggregatedset"
)

const (
	EventReasonScalerCreated  = "ScalerCreated"
	EventReasonScalerDeleted  = "ScalerDeleted"
	EventReasonScalerConflict = "ScalerConflict"
)

// ScalerManager auto-creates one DisaggregatedSetRoleScaler per External role
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

// ScalerName is the deterministic name for a role or sub-role scaler.
func ScalerName(dsName string, key disaggregatedsetutils.RoleKey) string {
	name := dsName + "-" + key.Role
	if key.SubRole != "" {
		name += "-" + key.SubRole
	}
	return name
}

// Reconcile ensures a scaler exists for every External role and deletes scalers
// owned by this DS whose role is no longer External. seedFor is called with a
// role name to compute the initial spec.replicas for a new scaler — typically
// the role's current LWS replica count so a Static→External flip does not
// drain a running role to 0. Returns role -> *Scaler.
func (m *ScalerManager) Reconcile(
	ctx context.Context,
	ds *disaggregatedsetv1.DisaggregatedSet,
	seedFor func(key disaggregatedsetutils.RoleKey, existing scalerMap) int32,
) (scalerMap, error) {
	log := logf.FromContext(ctx)

	externalRoles := make(map[disaggregatedsetutils.RoleKey]bool)
	for _, role := range ds.Spec.Roles {
		if len(role.SubRoles) == 0 {
			if role.Scaling != nil && role.Scaling.Mode == disaggregatedsetv1.RoleScalingExternal {
				externalRoles[disaggregatedsetutils.RoleKey{Role: role.Name}] = true
			}
			continue
		}
		for _, subRole := range role.SubRoles {
			if subRole.Scaling != nil && subRole.Scaling.Mode == disaggregatedsetv1.RoleScalingExternal {
				externalRoles[disaggregatedsetutils.RoleKey{Role: role.Name, SubRole: subRole.Name}] = true
			}
		}
	}

	list := &disaggregatedsetv1.DisaggregatedSetRoleScalerList{}
	if err := m.client.List(ctx, list, client.InNamespace(ds.Namespace),
		client.MatchingLabels{disaggregatedsetv1.SetNameLabelKey: ds.Name}); err != nil {
		return nil, fmt.Errorf("list scalers: %w", err)
	}

	existing := make(scalerMap)
	for i := range list.Items {
		s := &list.Items[i]
		if !isControlledBy(s, ds.UID) {
			continue
		}
		key := disaggregatedsetutils.RoleKey{
			Role:    s.Labels[disaggregatedsetv1.RoleLabelKey],
			SubRole: s.Labels[disaggregatedsetv1.SubRoleLabelKey],
		}
		if !externalRoles[key] {
			log.Info("Deleting scaler for role no longer External", "scaler", s.Name, "role", key.String())
			if err := m.client.Delete(ctx, s); err != nil && !apierrors.IsNotFound(err) {
				return nil, fmt.Errorf("delete scaler %s: %w", s.Name, err)
			}
			m.record.Eventf(ds, nil, corev1.EventTypeNormal, EventReasonScalerDeleted, "Delete",
				"Deleted scaler %s (role %s is no longer External)", s.Name, key.String())
			continue
		}
		existing[key] = s
	}

	seeds := make(map[disaggregatedsetutils.RoleKey]int32)
	for key := range externalRoles {
		if _, ok := existing[key]; ok {
			continue
		}
		if seedFor != nil {
			seeds[key] = seedFor(key, existing)
		}
	}
	for key := range externalRoles {
		if _, ok := existing[key]; ok {
			continue
		}
		s, err := m.create(ctx, ds, key, seeds[key])
		if err != nil {
			return nil, err
		}
		if s != nil {
			existing[key] = s
		}
	}
	return existing, nil
}

func (m *ScalerManager) create(ctx context.Context, ds *disaggregatedsetv1.DisaggregatedSet, key disaggregatedsetutils.RoleKey, seed int32) (*disaggregatedsetv1.DisaggregatedSetRoleScaler, error) {
	name := ScalerName(ds.Name, key)
	labels := map[string]string{
		disaggregatedsetv1.SetNameLabelKey: ds.Name,
		disaggregatedsetv1.RoleLabelKey:    key.Role,
	}
	if key.SubRole != "" {
		labels[disaggregatedsetv1.SubRoleLabelKey] = key.SubRole
	}
	scaler := &disaggregatedsetv1.DisaggregatedSetRoleScaler{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ds.Namespace,
			Labels:    labels,
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
	if err := m.client.Create(ctx, scaler); err == nil {
		logf.FromContext(ctx).Info("Created scaler", "scaler", name, "role", key.String())
		m.record.Eventf(ds, nil, corev1.EventTypeNormal, EventReasonScalerCreated,
			"Create", "Created scaler %s for role %s", name, key.String())
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
	if !isControlledBy(cur, ds.UID) {
		m.record.Eventf(ds, nil, corev1.EventTypeWarning, EventReasonScalerConflict,
			"Conflict", "Scaler %s exists but is not controlled by this DisaggregatedSet; not adopting", name)
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
	scalers scalerMap,
	observedReplicas map[disaggregatedsetutils.RoleKey]int32,
) error {
	for key, s := range scalers {
		desired := s.DeepCopy()
		desired.Status.Replicas = observedReplicas[key]
		// Selector filters to leader pods only (worker-index=0), one per LWS
		// group. HPA's per-pod-metric averaging divides its metric sum by the
		// count of matching pods; because spec.replicas (what HPA writes) and
		// status.replicas (what HPA reads) both count LWS groups, the selector
		// must match one pod per group for the ratio math to stay consistent.
		// Users typically want to scale on the leader's signal anyway (leader
		// handles ingress; workers are downstream compute).
		desired.Status.Selector = fmt.Sprintf("%s=%s,%s=%s",
			disaggregatedsetv1.SetNameLabelKey, ds.Name,
			disaggregatedsetv1.RoleLabelKey, key.Role)
		if key.SubRole != "" {
			desired.Status.Selector += fmt.Sprintf(",%s=%s", disaggregatedsetv1.SubRoleLabelKey, key.SubRole)
		}
		desired.Status.Selector += fmt.Sprintf(",%s=0", leaderworkersetv1.WorkerIndexLabelKey)
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

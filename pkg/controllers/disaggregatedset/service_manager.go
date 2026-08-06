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
	"maps"
	"strconv"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	disaggregatedsetv1 "sigs.k8s.io/lws/api/disaggregatedset/v1"
	leaderworkersetv1 "sigs.k8s.io/lws/api/leaderworkerset/v1"
	disaggregatedsetutils "sigs.k8s.io/lws/pkg/utils/disaggregatedset"
)

type ServiceManager struct {
	client client.Client
	scheme *runtime.Scheme
}

type serviceScope struct {
	revision string
	role     string
}

func NewServiceManager(k8sClient client.Client, scheme *runtime.Scheme) *ServiceManager {
	return &ServiceManager{
		client: k8sClient,
		scheme: scheme,
	}
}

func (manager *ServiceManager) ReconcileServices(
	ctx context.Context,
	deployment *disaggregatedsetv1.DisaggregatedSet,
	slice int,
	revisionRoles disaggregatedsetutils.RevisionRolesList,
	targetRevision string,
) error {
	desiredNames := make(map[string]bool)
	preserveRemovedRoleSubRoles := make(map[serviceScope]bool)
	for _, group := range revisionRoles {
		if group.Revision != targetRevision && revisionDrained(group) {
			continue
		}
		for roleName, lws := range group.Roles {
			if lws == nil {
				continue
			}
			service := manager.buildService(deployment, lws, "")
			desiredNames[service.Name] = true
			if err := manager.ensureService(ctx, service); err != nil {
				return fmt.Errorf("failed to ensure service for %s: %w", roleName, err)
			}

			role := disaggregatedsetutils.GetRoleSpec(deployment, roleName)
			if role == nil {
				// A removed parent can still be serving on an old revision. Its
				// sub-role Services cannot be reconstructed from the new spec, so
				// retain any existing ones until that revision drains.
				preserveRemovedRoleSubRoles[serviceScope{revision: group.Revision, role: roleName}] = true
				continue
			}
			for _, subRole := range role.SubRoles {
				service := manager.buildService(deployment, lws, subRole.Name)
				desiredNames[service.Name] = true
				if err := manager.ensureService(ctx, service); err != nil {
					return fmt.Errorf("failed to ensure service for %s/%s: %w", roleName, subRole.Name, err)
				}
			}
		}
	}

	if err := manager.cleanupDrainedServices(ctx, deployment, slice, desiredNames, preserveRemovedRoleSubRoles); err != nil {
		return fmt.Errorf("failed to cleanup drained services: %w", err)
	}

	return nil
}

func revisionDrained(group disaggregatedsetutils.RevisionRoles) bool {
	for _, lws := range group.Roles {
		if lws != nil && getLWSReplicas(lws) > 0 {
			return false
		}
	}
	return true
}

func (manager *ServiceManager) ensureService(
	ctx context.Context,
	service *corev1.Service,
) error {
	log := logf.FromContext(ctx)

	if err := manager.client.Create(ctx, service); err != nil {
		if apierrors.IsAlreadyExists(err) {
			log.V(1).Info("Service already exists", "service", service.Name)
			return nil
		}
		return fmt.Errorf("failed to create service %s: %w", service.Name, err)
	}

	log.V(1).Info("Created Service", "service", service.Name)
	return nil
}

// buildService builds the per-revision headless service for an LWS. The service is
// named after the LWS (<lws>-prv) and its selector mirrors the LWS's own DS labels,
// so it targets exactly that LWS's pods: a legacy slice-0 LWS (no slice label) yields
// a slice-agnostic selector that matches its label-less pods, while a slice-aware LWS
// yields a slice-scoped selector.
func (manager *ServiceManager) buildService(
	deployment *disaggregatedsetv1.DisaggregatedSet,
	lws *leaderworkersetv1.LeaderWorkerSet,
	subRole string,
) *corev1.Service {
	selector := map[string]string{
		disaggregatedsetv1.SetNameLabelKey:  deployment.Name,
		disaggregatedsetv1.RoleLabelKey:     lws.Labels[disaggregatedsetv1.RoleLabelKey],
		disaggregatedsetv1.RevisionLabelKey: lws.Labels[disaggregatedsetv1.RevisionLabelKey],
	}
	if disaggregatedsetutils.HasSliceLabel(lws.Labels) {
		selector[disaggregatedsetv1.SliceLabelKey] = lws.Labels[disaggregatedsetv1.SliceLabelKey]
	}
	name := lws.Name + "-prv"
	if subRole != "" {
		selector[disaggregatedsetv1.SubRoleLabelKey] = subRole
		name = lws.Name + "-" + subRole + "-prv"
	}

	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: deployment.Namespace,
			Labels:    maps.Clone(selector),
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: disaggregatedsetv1.GroupVersion.String(),
				Kind:       "DisaggregatedSet",
				Name:       deployment.Name,
				UID:        deployment.UID,
				Controller: ptr.To(true),
			}},
		},
		Spec: corev1.ServiceSpec{
			ClusterIP: corev1.ClusterIPNone,
			Selector:  selector,
		},
	}
}

func (manager *ServiceManager) cleanupDrainedServices(
	ctx context.Context,
	deployment *disaggregatedsetv1.DisaggregatedSet,
	slice int,
	desiredNames map[string]bool,
	preserveRemovedRoleSubRoles map[serviceScope]bool,
) error {
	log := logf.FromContext(ctx)

	// List all of the DisaggregatedSet's services and filter to this slice client-side
	// so a legacy slice-0 service (which has no slice label) is included in slice 0's
	// cleanup and removed once its revision drains.
	serviceList := &corev1.ServiceList{}
	if err := manager.client.List(ctx, serviceList,
		client.InNamespace(deployment.Namespace),
		client.MatchingLabels{disaggregatedsetv1.SetNameLabelKey: deployment.Name},
	); err != nil {
		return fmt.Errorf("failed to list services: %w", err)
	}

	for i := range serviceList.Items {
		service := &serviceList.Items[i]
		if !disaggregatedsetutils.SliceLabelMatches(service.Labels, slice) {
			continue
		}
		if service.Labels[disaggregatedsetv1.SubRoleLabelKey] != "" && preserveRemovedRoleSubRoles[serviceScope{
			revision: service.Labels[disaggregatedsetv1.RevisionLabelKey],
			role:     service.Labels[disaggregatedsetv1.RoleLabelKey],
		}] {
			continue
		}
		if !desiredNames[service.Name] {
			log.Info("Deleting drained or obsolete Service", "service", service.Name)
			if err := manager.client.Delete(ctx, service); err != nil {
				if apierrors.IsNotFound(err) {
					continue
				}
				return fmt.Errorf("failed to delete service %s: %w", service.Name, err)
			}
		}
	}

	return nil
}

// CleanupRemovedSlices deletes services whose slice index is at or above the
// desired slice count.
func (manager *ServiceManager) CleanupRemovedSlices(
	ctx context.Context,
	deployment *disaggregatedsetv1.DisaggregatedSet,
	desiredSlices int,
) error {
	log := logf.FromContext(ctx)

	serviceList := &corev1.ServiceList{}
	if err := manager.client.List(ctx, serviceList,
		client.InNamespace(deployment.Namespace),
		client.MatchingLabels{disaggregatedsetv1.SetNameLabelKey: deployment.Name},
	); err != nil {
		return fmt.Errorf("failed to list services: %w", err)
	}

	for i := range serviceList.Items {
		service := &serviceList.Items[i]
		sliceIdx, err := strconv.Atoi(service.Labels[disaggregatedsetv1.SliceLabelKey])
		if err != nil || sliceIdx < desiredSlices {
			continue
		}
		log.Info("Deleting Service for removed slice", "service", service.Name, "slice", sliceIdx)
		if err := manager.client.Delete(ctx, service); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return fmt.Errorf("failed to delete service %s: %w", service.Name, err)
		}
	}

	return nil
}

// DeleteLegacyServices deletes every pre-slices, slice-agnostic Service for a role and
// revision, including its parent and sub-role Services. Used during legacy slice-0
// migration: these Services share the target revision, so per-revision drained cleanup
// never removes them before sibling slices are created.
func (manager *ServiceManager) DeleteLegacyServices(
	ctx context.Context,
	deployment *disaggregatedsetv1.DisaggregatedSet,
	revision, role string,
) error {
	services := &corev1.ServiceList{}
	if err := manager.client.List(ctx, services,
		client.InNamespace(deployment.Namespace),
		client.MatchingLabels{
			disaggregatedsetv1.SetNameLabelKey:  deployment.Name,
			disaggregatedsetv1.RevisionLabelKey: revision,
			disaggregatedsetv1.RoleLabelKey:     role,
		},
	); err != nil {
		return fmt.Errorf("failed to list legacy Services for role %s: %w", role, err)
	}
	for i := range services.Items {
		service := &services.Items[i]
		if disaggregatedsetutils.HasSliceLabel(service.Labels) {
			continue
		}
		if err := manager.client.Delete(ctx, service); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("failed to delete legacy Service %s: %w", service.Name, err)
		}
	}
	return nil
}

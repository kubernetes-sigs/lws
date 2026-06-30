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
	log := logf.FromContext(ctx)
	roleNames := disaggregatedsetutils.GetRoleNames(deployment)

	var targetGroup *disaggregatedsetutils.RevisionRoles
	for i := range revisionRoles {
		if revisionRoles[i].Revision == targetRevision {
			targetGroup = &revisionRoles[i]
			break
		}
	}

	if targetGroup == nil || !revisionReadyOnAllRoles(*targetGroup, roleNames) {
		log.V(1).Info("Target revision not ready on all roles, keeping existing services",
			"targetRevision", targetRevision)
		return nil
	}

	// Create one headless service per role, derived from the target revision's LWS so
	// the selector matches that LWS's pods. A legacy slice-0 LWS yields a slice-agnostic
	// service; a slice-aware LWS yields a slice-scoped one.
	for _, roleName := range roleNames {
		lws := targetGroup.Roles[roleName]
		if lws == nil {
			continue
		}
		if err := manager.ensureService(ctx, deployment, lws); err != nil {
			return fmt.Errorf("failed to ensure service for %s: %w", roleName, err)
		}
	}

	if err := manager.cleanupDrainedServices(ctx, deployment, slice, revisionRoles, targetRevision, roleNames); err != nil {
		return fmt.Errorf("failed to cleanup drained services: %w", err)
	}

	return nil
}

// revisionReadyOnAllRoles reports whether every role has a ready LWS
// (ReadyReplicas >= 1) in the group.
func revisionReadyOnAllRoles(group disaggregatedsetutils.RevisionRoles, roleNames []string) bool {
	for _, roleName := range roleNames {
		lws, hasRole := group.Roles[roleName]
		if !hasRole || lws.Status.ReadyReplicas < 1 {
			return false
		}
	}
	return true
}

func (manager *ServiceManager) ensureService(
	ctx context.Context,
	deployment *disaggregatedsetv1.DisaggregatedSet,
	lws *leaderworkersetv1.LeaderWorkerSet,
) error {
	log := logf.FromContext(ctx)

	service := manager.buildService(deployment, lws)

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
) *corev1.Service {
	selector := map[string]string{
		disaggregatedsetv1.SetNameLabelKey:  deployment.Name,
		disaggregatedsetv1.RoleLabelKey:     lws.Labels[disaggregatedsetv1.RoleLabelKey],
		disaggregatedsetv1.RevisionLabelKey: lws.Labels[disaggregatedsetv1.RevisionLabelKey],
	}
	if disaggregatedsetutils.HasSliceLabel(lws.Labels) {
		selector[disaggregatedsetv1.SliceLabelKey] = lws.Labels[disaggregatedsetv1.SliceLabelKey]
	}

	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      lws.Name + "-prv",
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
	revisionRoles disaggregatedsetutils.RevisionRolesList,
	targetRevision string,
	roleNames []string,
) error {
	log := logf.FromContext(ctx)

	readyRevisionSet := make(map[string]bool)
	for _, group := range revisionRoles {
		if revisionReadyOnAllRoles(group, roleNames) {
			readyRevisionSet[group.Revision] = true
		}
	}

	readyRevisionSet[targetRevision] = true

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
		serviceRevision := service.Labels[disaggregatedsetv1.RevisionLabelKey]

		if !readyRevisionSet[serviceRevision] {
			log.Info("Deleting drained Service", "service", service.Name, "revision", serviceRevision, "targetRevision", targetRevision)
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

func GenerateServiceName(baseName string, slice int, revision, role string) string {
	return fmt.Sprintf("%s-%d-%s-%s-prv", baseName, slice, revision, role)
}

// DeleteLegacyService deletes the pre-slices, slice-agnostic service for a role and
// revision. Used during legacy slice-0 migration: the legacy service shares the target
// revision, so per-revision drained cleanup never removes it.
func (manager *ServiceManager) DeleteLegacyService(
	ctx context.Context,
	deployment *disaggregatedsetv1.DisaggregatedSet,
	revision, role string,
) error {
	name := disaggregatedsetutils.GenerateLegacyName(deployment.Name, revision, role) + "-prv"
	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: deployment.Namespace}}
	if err := manager.client.Delete(ctx, svc); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("failed to delete legacy service %s: %w", name, err)
	}
	return nil
}

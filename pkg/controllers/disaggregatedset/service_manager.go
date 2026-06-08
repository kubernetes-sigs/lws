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
	"slices"
	"strconv"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	disaggregatedsetv1 "sigs.k8s.io/lws/api/disaggregatedset/v1"
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

	var readyRevisions []string
	for _, group := range revisionRoles {
		rolesReady := true
		logArgs := []interface{}{"revision", group.Revision}

		for _, roleName := range roleNames {
			lws, hasRole := group.Roles[roleName]
			if !hasRole || lws.Status.ReadyReplicas < 1 {
				rolesReady = false
				break
			}
			logArgs = append(logArgs, roleName+"Ready", lws.Status.ReadyReplicas)
		}

		if rolesReady {
			readyRevisions = append(readyRevisions, group.Revision)
			log.V(1).Info("Revision is ready on all roles", logArgs...)
		}
	}

	if len(readyRevisions) == 0 {
		log.V(1).Info("No revisions are ready on all roles, skipping Service creation")
		return nil
	}

	targetRevisionReady := slices.Contains(readyRevisions, targetRevision)

	if !targetRevisionReady {
		log.V(1).Info("Target revision not ready on all roles, keeping existing services",
			"targetRevision", targetRevision,
			"readyRevisions", readyRevisions)
		return nil
	}

	for _, roleName := range roleNames {
		if err := manager.ensureService(ctx, deployment, slice, roleName, targetRevision); err != nil {
			return fmt.Errorf("failed to ensure service for %s: %w", roleName, err)
		}
	}

	if err := manager.cleanupDrainedServices(ctx, deployment, slice, revisionRoles, targetRevision, roleNames); err != nil {
		return fmt.Errorf("failed to cleanup drained services: %w", err)
	}

	return nil
}

func (manager *ServiceManager) ensureService(
	ctx context.Context,
	deployment *disaggregatedsetv1.DisaggregatedSet,
	slice int,
	roleName string,
	revision string,
) error {
	log := logf.FromContext(ctx)

	service := manager.buildService(deployment, slice, roleName, revision)

	if err := manager.client.Create(ctx, service); err != nil {
		if apierrors.IsAlreadyExists(err) {
			log.V(1).Info("Service already exists", "service", service.Name)
			return nil
		}
		return fmt.Errorf("failed to create service %s: %w", service.Name, err)
	}

	log.V(1).Info("Created Service", "service", service.Name, "revision", revision, "role", roleName)
	return nil
}

func (manager *ServiceManager) buildService(
	deployment *disaggregatedsetv1.DisaggregatedSet,
	slice int,
	roleName string,
	revision string,
) *corev1.Service {
	serviceName := GenerateServiceName(deployment.Name, slice, revision, roleName)

	labels := map[string]string{
		disaggregatedsetv1.SetNameLabelKey:  deployment.Name,
		disaggregatedsetv1.SliceLabelKey:    strconv.Itoa(slice),
		disaggregatedsetv1.RoleLabelKey:     roleName,
		disaggregatedsetv1.RevisionLabelKey: revision,
	}

	selector := map[string]string{
		disaggregatedsetv1.SetNameLabelKey:  deployment.Name,
		disaggregatedsetv1.SliceLabelKey:    strconv.Itoa(slice),
		disaggregatedsetv1.RoleLabelKey:     roleName,
		disaggregatedsetv1.RevisionLabelKey: revision,
	}

	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      serviceName,
			Namespace: deployment.Namespace,
			Labels:    labels,
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
		rolesReady := true
		for _, roleName := range roleNames {
			lws, hasRole := group.Roles[roleName]
			if !hasRole || lws.Status.ReadyReplicas < 1 {
				rolesReady = false
				break
			}
		}
		if rolesReady {
			readyRevisionSet[group.Revision] = true
		}
	}

	readyRevisionSet[targetRevision] = true

	serviceList := &corev1.ServiceList{}
	if err := manager.client.List(ctx, serviceList,
		client.InNamespace(deployment.Namespace),
		client.MatchingLabels{
			disaggregatedsetv1.SetNameLabelKey: deployment.Name,
			disaggregatedsetv1.SliceLabelKey:   strconv.Itoa(slice),
		},
	); err != nil {
		return fmt.Errorf("failed to list services: %w", err)
	}

	for i := range serviceList.Items {
		service := &serviceList.Items[i]
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

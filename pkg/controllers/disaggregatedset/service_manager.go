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
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
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

// migrateLegacyServices transfers Services created by an older controller to
// their matching LWS. A DS-owned Service without a matching LWS is an orphan
// left by an interrupted old-controller cleanup and is deleted. Foreign and
// ownerless Services are never changed.
func (manager *ServiceManager) migrateLegacyServices(
	ctx context.Context,
	deployment *disaggregatedsetv1.DisaggregatedSet,
	lwsList []*leaderworkersetv1.LeaderWorkerSet,
) error {
	lwsByService := make(map[string]*leaderworkersetv1.LeaderWorkerSet, len(lwsList))
	for _, lws := range lwsList {
		lwsByService[disaggregatedsetutils.PrivateServiceName(lws.Name)] = lws
	}

	serviceList := &corev1.ServiceList{}
	if err := manager.client.List(ctx, serviceList,
		client.InNamespace(deployment.Namespace),
		client.MatchingLabels{disaggregatedsetv1.SetNameLabelKey: deployment.Name},
	); err != nil {
		return fmt.Errorf("failed to list legacy services: %w", err)
	}

	log := logf.FromContext(ctx)
	for i := range serviceList.Items {
		service := &serviceList.Items[i]
		if !metav1.IsControlledBy(service, deployment) || !isPrivateRoleService(service) {
			continue
		}

		if lws := lwsByService[service.Name]; lws != nil {
			if exists, controlled, err := manager.transferServiceOwnership(ctx, deployment, lws); err != nil {
				return err
			} else if exists && !controlled {
				return fmt.Errorf("legacy service %s could not be transferred to LeaderWorkerSet %s", service.Name, lws.Name)
			}
			continue
		}

		log.Info("Deleting orphaned legacy Service", "service", service.Name)
		if err := manager.client.Delete(ctx, service); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("failed to delete orphaned legacy service %s: %w", service.Name, err)
		}
	}

	return nil
}

func isPrivateRoleService(service *corev1.Service) bool {
	return strings.HasSuffix(service.Name, disaggregatedsetutils.PrivateServiceSuffix) &&
		service.Labels[disaggregatedsetv1.RoleLabelKey] != "" &&
		service.Labels[disaggregatedsetv1.RevisionLabelKey] != ""
}

func (manager *ServiceManager) ReconcileServices(
	ctx context.Context,
	deployment *disaggregatedsetv1.DisaggregatedSet,
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

	service, err := manager.buildService(deployment, lws)
	if err != nil {
		return fmt.Errorf("failed to set owner for service %s: %w", disaggregatedsetutils.PrivateServiceName(lws.Name), err)
	}

	if err := manager.client.Create(ctx, service); err != nil {
		if apierrors.IsAlreadyExists(err) {
			exists, controlled, transferErr := manager.transferServiceOwnership(ctx, deployment, lws)
			if transferErr != nil {
				return transferErr
			}
			if !exists {
				// Deleted between our Create and this Get; the next reconcile recreates it.
				return nil
			}
			if !controlled {
				return fmt.Errorf("service %s exists but is not controlled by LeaderWorkerSet %s (owned by another object or pending garbage collection from a previous generation)", service.Name, lws.Name)
			}
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
) (*corev1.Service, error) {
	selector := map[string]string{
		disaggregatedsetv1.SetNameLabelKey:  deployment.Name,
		disaggregatedsetv1.RoleLabelKey:     lws.Labels[disaggregatedsetv1.RoleLabelKey],
		disaggregatedsetv1.RevisionLabelKey: lws.Labels[disaggregatedsetv1.RevisionLabelKey],
	}
	if disaggregatedsetutils.HasSliceLabel(lws.Labels) {
		selector[disaggregatedsetv1.SliceLabelKey] = lws.Labels[disaggregatedsetv1.SliceLabelKey]
	}

	service := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      disaggregatedsetutils.PrivateServiceName(lws.Name),
			Namespace: deployment.Namespace,
			Labels:    maps.Clone(selector),
		},
		Spec: corev1.ServiceSpec{
			ClusterIP: corev1.ClusterIPNone,
			Selector:  selector,
		},
	}
	if err := controllerutil.SetControllerReference(lws, service, manager.scheme); err != nil {
		return nil, err
	}
	return service, nil
}

// transferServiceOwnership moves a Service created by an older controller from
// the DisaggregatedSet to its corresponding LWS. Foreign Services are untouched.
// The booleans report whether the Service exists and whether it is controlled by
// the LWS after the call.
func (manager *ServiceManager) transferServiceOwnership(
	ctx context.Context,
	deployment *disaggregatedsetv1.DisaggregatedSet,
	lws *leaderworkersetv1.LeaderWorkerSet,
) (bool, bool, error) {
	service := &corev1.Service{}
	if err := manager.client.Get(ctx, client.ObjectKey{Name: disaggregatedsetutils.PrivateServiceName(lws.Name), Namespace: deployment.Namespace}, service); err != nil {
		if apierrors.IsNotFound(err) {
			return false, false, nil
		}
		return false, false, fmt.Errorf("failed to get service %s: %w", disaggregatedsetutils.PrivateServiceName(lws.Name), err)
	}
	if metav1.IsControlledBy(service, lws) {
		return true, true, nil
	}
	if !metav1.IsControlledBy(service, deployment) {
		return true, false, nil
	}

	patch := client.MergeFromWithOptions(service.DeepCopy(), client.MergeFromWithOptimisticLock{})
	if err := controllerutil.RemoveControllerReference(deployment, service, manager.scheme); err != nil {
		return true, false, fmt.Errorf("failed to remove DisaggregatedSet owner from service %s: %w", service.Name, err)
	}
	if err := controllerutil.SetControllerReference(lws, service, manager.scheme); err != nil {
		return true, false, fmt.Errorf("failed to set LeaderWorkerSet owner on service %s: %w", service.Name, err)
	}
	if err := manager.client.Patch(ctx, service, patch); err != nil {
		return true, false, fmt.Errorf("failed to transfer ownership of service %s: %w", service.Name, err)
	}
	return true, true, nil
}

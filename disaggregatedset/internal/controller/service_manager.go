/*
Copyright 2026.

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

package controller

import (
	"context"
	"fmt"
	"slices"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	disaggv1alpha1 "sigs.k8s.io/disaggregatedset/api/v1alpha1"
)

// ServiceManager manages Service resources for DisaggregatedSet.
// It coordinates Service creation based on cross-phase readiness:
// Services are only created when both prefill and decode phases have at least 1 ready replica.
// Services are headless (clusterIP: None) and portless to enable EndpointSlice-based discovery.
type ServiceManager struct {
	client client.Client
	scheme *runtime.Scheme
}

// NewServiceManager creates a new ServiceManager.
func NewServiceManager(k8sClient client.Client, scheme *runtime.Scheme) *ServiceManager {
	return &ServiceManager{
		client: k8sClient,
		scheme: scheme,
	}
}

// ReconcileServices reconciles Services for a DisaggregatedSet.
// It creates headless portless Services when both phases are ready and cleans up old Services.
// The targetRevision parameter is the current revision from the deployment spec.
func (manager *ServiceManager) ReconcileServices(
	ctx context.Context,
	deployment *disaggv1alpha1.DisaggregatedSet,
	groupedWorkloads GroupedWorkloads,
	targetRevision string,
) error {
	log := logf.FromContext(ctx)

	// Find revisions where both phases are ready (readyReplicas >= 1)
	var readyRevisions []string
	for _, group := range groupedWorkloads {
		prefillInfo, hasPrefill := group.Phases[PhasePrefill]
		decodeInfo, hasDecode := group.Phases[PhaseDecode]

		if hasPrefill && hasDecode && prefillInfo.ReadyReplicas >= 1 && decodeInfo.ReadyReplicas >= 1 {
			readyRevisions = append(readyRevisions, group.Revision)
			log.V(1).Info("Revision is ready on both phases",
				"revision", group.Revision,
				"prefillReady", prefillInfo.ReadyReplicas,
				"decodeReady", decodeInfo.ReadyReplicas)
		}
	}

	if len(readyRevisions) == 0 {
		log.V(1).Info("No revisions are ready on both phases, skipping Service creation")
		return nil
	}

	// Check if target revision is ready on both phases
	targetRevisionReady := slices.Contains(readyRevisions, targetRevision)

	// Only create services for the target revision (current spec).
	// If target revision is not ready, keep existing services unchanged to prevent flip-flop.
	// This ensures we only ever move forward to the new version, never backward.
	if !targetRevisionReady {
		log.V(1).Info("Target revision not ready on both phases, keeping existing services",
			"targetRevision", targetRevision,
			"readyRevisions", readyRevisions)
		return nil
	}

	// Create/ensure headless portless Services for both phases
	phases := []string{PhasePrefill, PhaseDecode}
	for _, phaseName := range phases {
		if err := manager.ensureService(ctx, deployment, phaseName, targetRevision); err != nil {
			return fmt.Errorf("failed to ensure service for %s: %w", phaseName, err)
		}
	}

	// Only cleanup old services when the old revision is NO LONGER ready (fully drained).
	// This prevents flip-flopping during rolling updates when both versions are ready.
	// We only delete services for revisions that have 0 ready replicas on both phases.
	if err := manager.cleanupDrainedServices(ctx, deployment, groupedWorkloads, targetRevision); err != nil {
		return fmt.Errorf("failed to cleanup drained services: %w", err)
	}

	return nil
}

// ensureService creates a headless portless Service for a specific phase and revision.
func (manager *ServiceManager) ensureService(
	ctx context.Context,
	deployment *disaggv1alpha1.DisaggregatedSet,
	phaseName string,
	revision string,
) error {
	log := logf.FromContext(ctx)

	service := manager.buildService(deployment, phaseName, revision)

	// Try to create the Service
	if err := manager.client.Create(ctx, service); err != nil {
		if apierrors.IsAlreadyExists(err) {
			log.V(1).Info("Service already exists", "service", service.Name)
			return nil
		}
		return fmt.Errorf("failed to create service %s: %w", service.Name, err)
	}

	log.V(1).Info("Created Service", "service", service.Name, "revision", revision, "phase", phaseName)
	return nil
}

// buildService constructs a headless portless Service object for a specific phase and revision.
func (manager *ServiceManager) buildService(
	deployment *disaggv1alpha1.DisaggregatedSet,
	phaseName string,
	revision string,
) *corev1.Service {
	serviceName := GenerateServiceName(deployment.Name, phaseName, revision)

	// Standard labels only - no user configuration
	labels := map[string]string{
		LabelDisaggName:  deployment.Name,
		LabelDisaggPhase: phaseName,
		LabelRevision:    revision,
	}

	// Selector matches pod labels for this phase and revision
	selector := map[string]string{
		LabelDisaggName:  deployment.Name,
		LabelDisaggPhase: phaseName,
		LabelRevision:    revision,
	}

	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      serviceName,
			Namespace: deployment.Namespace,
			Labels:    labels,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: disaggv1alpha1.GroupVersion.String(),
				Kind:       "DisaggregatedSet",
				Name:       deployment.Name,
				UID:        deployment.UID,
				Controller: ptr.To(true),
			}},
		},
		Spec: corev1.ServiceSpec{
			ClusterIP: corev1.ClusterIPNone, // Headless
			Selector:  selector,
			// No ports - portless service for EndpointSlice discovery
		},
	}
}

// cleanupDrainedServices deletes Services for revisions that are no longer ready on both phases.
// This is safer than cleanupOldServices because it only removes services for versions
// that have been fully drained (0 ready replicas), preventing flip-flop during rolling updates.
func (manager *ServiceManager) cleanupDrainedServices(
	ctx context.Context,
	deployment *disaggv1alpha1.DisaggregatedSet,
	groupedWorkloads GroupedWorkloads,
	targetRevision string,
) error {
	log := logf.FromContext(ctx)

	// Build a set of revisions that still have ready replicas on both phases
	readyRevisionSet := make(map[string]bool)
	for _, group := range groupedWorkloads {
		prefillInfo, hasPrefill := group.Phases[PhasePrefill]
		decodeInfo, hasDecode := group.Phases[PhaseDecode]

		if hasPrefill && hasDecode && prefillInfo.ReadyReplicas >= 1 && decodeInfo.ReadyReplicas >= 1 {
			readyRevisionSet[group.Revision] = true
		}
	}

	// Always keep services for the target revision
	readyRevisionSet[targetRevision] = true

	// List all Services for this DisaggregatedSet
	serviceList := &corev1.ServiceList{}
	if err := manager.client.List(ctx, serviceList,
		client.InNamespace(deployment.Namespace),
		client.MatchingLabels{LabelDisaggName: deployment.Name},
	); err != nil {
		return fmt.Errorf("failed to list services: %w", err)
	}

	// Delete Services for revisions that are no longer ready
	for i := range serviceList.Items {
		service := &serviceList.Items[i]
		serviceRevision := service.Labels[LabelRevision]

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

// GenerateServiceName generates the name for a private Service.
// Format: {baseName}-{revision}-{phase}-prv
func GenerateServiceName(baseName, phase, revision string) string {
	return fmt.Sprintf("%s-%s-%s-prv", baseName, revision, phase)
}

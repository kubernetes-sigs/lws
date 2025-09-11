/*
Copyright 2023.

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

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	coreapplyv1 "k8s.io/client-go/applyconfigurations/core/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	leaderworkerset "sigs.k8s.io/lws/api/leaderworkerset/v1"
)

func CreateHeadlessServiceIfNotExists(ctx context.Context, k8sClient client.Client, Scheme *runtime.Scheme, lws *leaderworkerset.LeaderWorkerSet, serviceName string, serviceSelector map[string]string, owner metav1.Object) error {
	log := ctrl.LoggerFrom(ctx)
	// If the headless service does not exist in the namespace, create it.
	var headlessService corev1.Service
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: serviceName, Namespace: lws.Namespace}, &headlessService); err != nil {
		if client.IgnoreNotFound(err) != nil {
			return err
		}
		headlessService := corev1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Name:      serviceName,
				Namespace: lws.Namespace,
				Labels:    map[string]string{leaderworkerset.SetNameLabelKey: lws.Name},
			},
			Spec: corev1.ServiceSpec{
				ClusterIP:                "None", // defines service as headless
				Selector:                 serviceSelector,
				PublishNotReadyAddresses: true,
			},
		}

		// Set the controller owner reference for garbage collection and reconciliation.
		if err := ctrl.SetControllerReference(owner, &headlessService, Scheme); err != nil {
			return err
		}
		// create the service in the cluster
		log.V(2).Info("Creating headless service.")
		if err := k8sClient.Create(ctx, &headlessService); err != nil {
			return err
		}
	}
	return nil
}

func GetPVCApplyConfiguration(lws *leaderworkerset.LeaderWorkerSet) []*coreapplyv1.PersistentVolumeClaimApplyConfiguration {
	pvcApplyConfiguration := []*coreapplyv1.PersistentVolumeClaimApplyConfiguration{}
	if lws == nil {
		return pvcApplyConfiguration
	}

	for _, pvc := range lws.Spec.LeaderWorkerTemplate.VolumeClaimTemplates {
		pvcSpecApplyConfig := coreapplyv1.PersistentVolumeClaimSpec().WithAccessModes(pvc.Spec.AccessModes...)
		if pvc.Spec.StorageClassName != nil {
			pvcSpecApplyConfig = pvcSpecApplyConfig.WithStorageClassName(*pvc.Spec.StorageClassName)
		}
		if pvc.Spec.VolumeMode != nil {
			pvcSpecApplyConfig = pvcSpecApplyConfig.WithVolumeMode(*pvc.Spec.VolumeMode)
		}
		if pvc.Spec.Resources.Requests != nil || pvc.Spec.Resources.Limits != nil {
			vrrApplyConfig := coreapplyv1.VolumeResourceRequirementsApplyConfiguration{}
			if pvc.Spec.Resources.Requests != nil {
				vrrApplyConfig.Requests = &pvc.Spec.Resources.Requests
			}
			if pvc.Spec.Resources.Limits != nil {
				vrrApplyConfig.Limits = &pvc.Spec.Resources.Limits
			}
			pvcSpecApplyConfig = pvcSpecApplyConfig.WithResources(&vrrApplyConfig)
		}
		config := coreapplyv1.PersistentVolumeClaim(pvc.Name, lws.Namespace).
			WithSpec(pvcSpecApplyConfig)
		pvcApplyConfiguration = append(pvcApplyConfiguration, config)
	}
	return pvcApplyConfiguration
}

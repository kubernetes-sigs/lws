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
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	leaderworkerset "sigs.k8s.io/lws/api/leaderworkerset/v1"
)

func CreateHeadlessServiceIfNotExists(ctx context.Context, k8sClient client.Client, Scheme *runtime.Scheme, lws *leaderworkerset.LeaderWorkerSet, serviceName string, serviceSelector map[string]string, owner metav1.Object) error {
	return CreateServiceIfNotExists(ctx, k8sClient, Scheme, lws, serviceName, serviceSelector, true, owner)
}

func CreateServiceIfNotExists(ctx context.Context, k8sClient client.Client, Scheme *runtime.Scheme, lws *leaderworkerset.LeaderWorkerSet, serviceName string, serviceSelector map[string]string, isHeadlessService bool, owner metav1.Object) error {
	log := ctrl.LoggerFrom(ctx)
	// If the service does not exist in the namespace, create it.
	var service corev1.Service
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: serviceName, Namespace: lws.Namespace}, &service); err != nil {
		if client.IgnoreNotFound(err) != nil {
			return err
		}
		// Get ports from worker template containers
		var ports []corev1.ServicePort
		for _, container := range lws.Spec.LeaderWorkerTemplate.WorkerTemplate.Spec.Containers {
			for _, containerPort := range container.Ports {
				ports = append(ports, corev1.ServicePort{
					Name:       containerPort.Name,
					Port:       containerPort.ContainerPort,
					Protocol:   containerPort.Protocol,
					TargetPort: intstr.FromInt(int(containerPort.ContainerPort)),
				})
			}
		}

		service := corev1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Name:      serviceName,
				Namespace: lws.Namespace,
				Labels:    map[string]string{leaderworkerset.SetNameLabelKey: lws.Name},
			},
			Spec: corev1.ServiceSpec{
				Selector:                 serviceSelector,
				PublishNotReadyAddresses: true,
			},
		}
		if isHeadlessService {
			service.Spec.ClusterIP = "None" // defines service as headless
		} else {
			service.Spec.Ports = ports // expose ports
		}

		// Set the controller owner reference for garbage collection and reconciliation.
		if err := ctrl.SetControllerReference(owner, &service, Scheme); err != nil {
			return err
		}
		// create the service in the cluster
		if isHeadlessService {
			log.V(2).Info("Creating headless service.")
		} else {
			log.V(2).Info("Creating service.")
		}
		if err := k8sClient.Create(ctx, &service); err != nil {
			return err
		}
	}
	return nil
}

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

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	configv1alpha1 "k8s.io/component-base/config/v1alpha1"
)

// +k8s:defaulter-gen=true
// +kubebuilder:object:root=true

// Configuration is the Schema for the configurations API
type Configuration struct {
	metav1.TypeMeta `json:",inline"`

	// ControllerManager returns the configurations for controllers
	ControllerManager `json:",inline"`

	// InternalCertManagerment is configuration for internalCertManagerment
	InternalCertManagement *InternalCertManagement `json:"internalCertManagement,omitempty"`

	// ClientConnection is configuration of the client while connecting to API Server
	ClientConnection *ClientConnection `json:"clientConnection,omitempty"`
}

type ControllerManager struct {
	// Webhook contains the controllers webhook configuration
	// +optional
	Webhook ControllerWebhook `json:"webhook,omitempty"`

	// LeaderElection is the LeaderElection config to be used when configuring
	// the manager.Manager leader election
	// +optional
	LeaderElection *configv1alpha1.LeaderElectionConfiguration `json:"leaderElection,omitempty"`

	// Metrics contains the controller metrics configuration
	// +optional
	Metrics ControllerMetrics `json:"metrics,omitempty"`

	// Health contains the controller health configuration
	// +optional
	Health ControllerHealth `json:"health,omitempty"`
}

// ControllerWebhook defines the webhook server for the controller.
type ControllerWebhook struct {
	// Port is the port that the webhook server serves at.
	// It is used to set webhook.Server.Port.
	// +optional
	Port *int `json:"port,omitempty"`

	// Host is the hostname that the webhook server binds to.
	// It is used to set webhook.Server.Host.
	// +optional
	Host string `json:"host,omitempty"`

	// CertDir is the directory that contains the server key and certificate.
	// if not set, webhook server would look up the server key and certificate in
	// {TempDir}/k8s-webhook-server/serving-certs. The server key and certificate
	// must be named tls.key and tls.crt, respectively.
	// +optional
	CertDir string `json:"certDir,omitempty"`
}

// ControllerMetrics defines the metrics configs.
type ControllerMetrics struct {
	// BindAddress is the TCP address that the controller should bind to
	// for serving prometheus metrics.
	// It can be set to "0" to disable the metrics serving.
	// +optional
	BindAddress string `json:"bindAddress,omitempty"`
}

// ControllerHealth defines the health configs.
type ControllerHealth struct {
	// HealthProbeBindAddress is the TCP address that the controller should bind to
	// for serving health probes
	// It can be set to "0" or "" to disable serving the health probe.
	// +optional
	HealthProbeBindAddress string `json:"healthProbeBindAddress,omitempty"`

	// ReadinessEndpointName, defaults to "readyz"
	// +optional
	ReadinessEndpointName string `json:"readinessEndpointName,omitempty"`

	// LivenessEndpointName, defaults to "healthz"
	// +optional
	LivenessEndpointName string `json:"livenessEndpointName,omitempty"`
}

// InternalCertManagement defines internal certificate management configs
type InternalCertManagement struct {
	// Enable controls whether to enable internal cert management or not.
	// Defaults to true. If you want to use a third-party management, e.g. cert-manager,
	// set it to false. See the user guide for more information.
	Enable *bool `json:"enable,omitempty"`

	// WebhookServiceName is the name of the Service used as part of the DNSName.
	// Defaults to lws-webhook-service.
	WebhookServiceName *string `json:"webhookServiceName,omitempty"`

	// WebhookSecretName is the name of the Secret used to store CA and server certs.
	// Defaults to lws-webhook-server-cert.
	WebhookSecretName *string `json:"webhookSecretName,omitempty"`
}

// ClientConnection defines the connection related fields while connecting to API Server
type ClientConnection struct {
	// QPS controls the number of queries per second allowed for K8S api server
	// connection.
	QPS *float32 `json:"qps,omitempty"`

	// Burst allows extra queries to accumulate when a client is exceeding its rate.
	Burst *int32 `json:"burst,omitempty"`
}

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
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	configv1alpha1 "k8s.io/component-base/config/v1alpha1"
	"k8s.io/utils/ptr"

	configv1 "sigs.k8s.io/lws/api/config/v1"
)

func newFullV1alpha1Config() *Configuration {
	return &Configuration{
		TypeMeta: metav1.TypeMeta{
			APIVersion: GroupVersion.String(),
			Kind:       "Configuration",
		},
		ControllerManager: ControllerManager{
			Webhook: ControllerWebhook{
				Port:    ptr.To(9443),
				Host:    "webhook.example.com",
				CertDir: "/custom/cert/dir",
			},
			LeaderElection: &configv1alpha1.LeaderElectionConfiguration{
				LeaderElect:   ptr.To(true),
				LeaseDuration: metav1.Duration{Duration: 15 * time.Second},
				RenewDeadline: metav1.Duration{Duration: 10 * time.Second},
				RetryPeriod:   metav1.Duration{Duration: 2 * time.Second},
				ResourceLock:  "leases",
				ResourceName:  "test-leader-election",
			},
			Metrics: ControllerMetrics{
				BindAddress: ":8443",
			},
			Health: ControllerHealth{
				HealthProbeBindAddress: ":8081",
				ReadinessEndpointName:  "/readyz",
				LivenessEndpointName:   "/healthz",
			},
			TLS: &TLSOptions{
				MinVersion:   "VersionTLS13",
				CipherSuites: []string{"TLS_AES_128_GCM_SHA256"},
			},
		},
		InternalCertManagement: &InternalCertManagement{
			Enable:             ptr.To(true),
			WebhookServiceName: ptr.To("lws-webhook-service"),
			WebhookSecretName:  ptr.To("lws-webhook-server-cert"),
		},
		GangSchedulingManagement: &GangSchedulingManagement{
			SchedulerProvider: ptr.To("volcano"),
		},
		FeatureGates: map[string]bool{
			"WorkloadAwareScheduling": true,
			"CustomGate":              false,
		},
		ClientConnection: &ClientConnection{
			QPS:   ptr.To[float32](100),
			Burst: ptr.To[int32](200),
		},
	}
}

func newFullV1Config() *configv1.Configuration {
	return &configv1.Configuration{
		TypeMeta: metav1.TypeMeta{
			APIVersion: configv1.GroupVersion.String(),
			Kind:       "Configuration",
		},
		ControllerManager: configv1.ControllerManager{
			Webhook: configv1.ControllerWebhook{
				Port:    ptr.To(9443),
				Host:    "webhook.example.com",
				CertDir: "/custom/cert/dir",
			},
			LeaderElection: &configv1alpha1.LeaderElectionConfiguration{
				LeaderElect:   ptr.To(true),
				LeaseDuration: metav1.Duration{Duration: 15 * time.Second},
				RenewDeadline: metav1.Duration{Duration: 10 * time.Second},
				RetryPeriod:   metav1.Duration{Duration: 2 * time.Second},
				ResourceLock:  "leases",
				ResourceName:  "test-leader-election",
			},
			Metrics: configv1.ControllerMetrics{
				BindAddress: ":8443",
			},
			Health: configv1.ControllerHealth{
				HealthProbeBindAddress: ":8081",
				ReadinessEndpointName:  "/readyz",
				LivenessEndpointName:   "/healthz",
			},
			TLS: &configv1.TLSOptions{
				MinVersion:   "VersionTLS13",
				CipherSuites: []string{"TLS_AES_128_GCM_SHA256"},
			},
		},
		InternalCertManagement: &configv1.InternalCertManagement{
			Enable:             ptr.To(true),
			WebhookServiceName: ptr.To("lws-webhook-service"),
			WebhookSecretName:  ptr.To("lws-webhook-server-cert"),
		},
		GangSchedulingManagement: &configv1.GangSchedulingManagement{
			SchedulerProvider: ptr.To("volcano"),
		},
		FeatureGates: map[string]bool{
			"WorkloadAwareScheduling": true,
			"CustomGate":              false,
		},
		ClientConnection: &configv1.ClientConnection{
			QPS:   ptr.To[float32](100),
			Burst: ptr.To[int32](200),
		},
	}
}

func TestRoundTrip_v1alpha1_v1_v1alpha1(t *testing.T) {
	testCases := map[string]struct {
		original *Configuration
	}{
		"full populated configuration": {
			original: newFullV1alpha1Config(),
		},
		"empty configuration": {
			original: &Configuration{},
		},
		"partial configuration with nil pointers": {
			original: &Configuration{
				ControllerManager: ControllerManager{
					Webhook: ControllerWebhook{
						Port: ptr.To(8080),
					},
					Metrics: ControllerMetrics{
						BindAddress: ":9090",
					},
				},
				FeatureGates: map[string]bool{
					"TestGate": true,
				},
			},
		},
	}

	for name, tc := range testCases {
		t.Run(name, func(t *testing.T) {
			v1Obj := &configv1.Configuration{}
			if err := Convert_v1alpha1_Configuration_To_v1_Configuration(tc.original, v1Obj, nil); err != nil {
				t.Fatalf("conversion v1alpha1 -> v1 failed: %v", err)
			}

			roundTripped := &Configuration{}
			if err := Convert_v1_Configuration_To_v1alpha1_Configuration(v1Obj, roundTripped, nil); err != nil {
				t.Fatalf("conversion v1 -> v1alpha1 failed: %v", err)
			}

			if diff := cmp.Diff(tc.original, roundTripped, cmpopts.IgnoreFields(Configuration{}, "TypeMeta")); diff != "" {
				t.Errorf("round trip diff (-original +roundTripped):\n%s", diff)
			}
		})
	}
}

func TestRoundTrip_v1_v1alpha1_v1(t *testing.T) {
	testCases := map[string]struct {
		original *configv1.Configuration
	}{
		"full populated configuration": {
			original: newFullV1Config(),
		},
		"empty configuration": {
			original: &configv1.Configuration{},
		},
		"partial configuration with nil pointers": {
			original: &configv1.Configuration{
				ControllerManager: configv1.ControllerManager{
					Webhook: configv1.ControllerWebhook{
						Port: ptr.To(8080),
					},
					Metrics: configv1.ControllerMetrics{
						BindAddress: ":9090",
					},
				},
				FeatureGates: map[string]bool{
					"TestGate": true,
				},
			},
		},
	}

	for name, tc := range testCases {
		t.Run(name, func(t *testing.T) {
			v1alpha1Obj := &Configuration{}
			if err := Convert_v1_Configuration_To_v1alpha1_Configuration(tc.original, v1alpha1Obj, nil); err != nil {
				t.Fatalf("conversion v1 -> v1alpha1 failed: %v", err)
			}

			roundTripped := &configv1.Configuration{}
			if err := Convert_v1alpha1_Configuration_To_v1_Configuration(v1alpha1Obj, roundTripped, nil); err != nil {
				t.Fatalf("conversion v1alpha1 -> v1 failed: %v", err)
			}

			if diff := cmp.Diff(tc.original, roundTripped, cmpopts.IgnoreFields(configv1.Configuration{}, "TypeMeta")); diff != "" {
				t.Errorf("round trip diff (-original +roundTripped):\n%s", diff)
			}
		})
	}
}

func TestScheme_Conversion(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := AddToScheme(scheme); err != nil {
		t.Fatalf("failed to add v1alpha1 to scheme: %v", err)
	}
	if err := configv1.AddToScheme(scheme); err != nil {
		t.Fatalf("failed to add v1 to scheme: %v", err)
	}

	originalV1alpha1 := newFullV1alpha1Config()
	convertedV1 := &configv1.Configuration{}

	if err := scheme.Convert(originalV1alpha1, convertedV1, nil); err != nil {
		t.Fatalf("scheme.Convert v1alpha1 -> v1 failed: %v", err)
	}

	expectedV1 := newFullV1Config()
	if diff := cmp.Diff(expectedV1, convertedV1, cmpopts.IgnoreFields(configv1.Configuration{}, "TypeMeta")); diff != "" {
		t.Errorf("unexpected diff after scheme.Convert (-want +got):\n%s", diff)
	}

	roundTrippedV1alpha1 := &Configuration{}
	if err := scheme.Convert(convertedV1, roundTrippedV1alpha1, nil); err != nil {
		t.Fatalf("scheme.Convert v1 -> v1alpha1 failed: %v", err)
	}

	if diff := cmp.Diff(originalV1alpha1, roundTrippedV1alpha1, cmpopts.IgnoreFields(Configuration{}, "TypeMeta")); diff != "" {
		t.Errorf("unexpected diff after scheme round trip (-want +got):\n%s", diff)
	}
}

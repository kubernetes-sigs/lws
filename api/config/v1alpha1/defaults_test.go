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

	"github.com/google/go-cmp/cmp"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	configv1alpha1 "k8s.io/component-base/config/v1alpha1"
	"k8s.io/utils/ptr"
)

const (
	overwriteWebhookPort            = 9444
	overwriteMetricBindAddress      = ":38081"
	overwriteHealthProbeBindAddress = ":38080"
	overwriteLeaderElectionID       = "foo.lws.x-k8s.io"
)

func TestSetDefaults_Configuration(t *testing.T) {
	defaultCtrlManagerConfigurationSpec := ControllerManager{
		LeaderElection: &configv1alpha1.LeaderElectionConfiguration{
			LeaderElect:   ptr.To(true),
			LeaseDuration: metav1.Duration{Duration: DefaultLeaderElectionLeaseDuration},
			RenewDeadline: metav1.Duration{Duration: DefaultLeaderElectionRenewDeadline},
			RetryPeriod:   metav1.Duration{Duration: DefaultLeaderElectionRetryPeriod},
			ResourceLock:  DefaultResourceLock,
			ResourceName:  DefaultLeaderElectionID,
		},
		Webhook: ControllerWebhook{
			Port: ptr.To(DefaultWebhookPort),
		},
		Metrics: ControllerMetrics{
			BindAddress: DefaultMetricsBindAddress,
		},
		Health: ControllerHealth{
			HealthProbeBindAddress: DefaultHealthProbeBindAddress,
		},
	}
	defaultClientConnection := &ClientConnection{
		QPS:   ptr.To(DefaultClientConnectionQPS),
		Burst: ptr.To(DefaultClientConnectionBurst),
	}

	testCases := map[string]struct {
		original *Configuration
		want     *Configuration
	}{
		"defaulting namespace": {
			original: &Configuration{
				InternalCertManagement: &InternalCertManagement{
					Enable: ptr.To(false),
				},
			},
			want: &Configuration{
				ControllerManager: defaultCtrlManagerConfigurationSpec,
				InternalCertManagement: &InternalCertManagement{
					Enable: ptr.To(false),
				},
				ClientConnection: defaultClientConnection,
			},
		},
		"defaulting ControllerManager": {
			original: &Configuration{
				ControllerManager: ControllerManager{
					LeaderElection: &configv1alpha1.LeaderElectionConfiguration{
						LeaderElect: ptr.To(true),
					},
				},
				InternalCertManagement: &InternalCertManagement{
					Enable: ptr.To(false),
				},
			},
			want: &Configuration{
				ControllerManager: ControllerManager{
					Webhook: ControllerWebhook{
						Port: ptr.To(DefaultWebhookPort),
					},
					Metrics: ControllerMetrics{
						BindAddress: DefaultMetricsBindAddress,
					},
					Health: ControllerHealth{
						HealthProbeBindAddress: DefaultHealthProbeBindAddress,
					},
					LeaderElection: &configv1alpha1.LeaderElectionConfiguration{
						LeaderElect:   ptr.To(true),
						LeaseDuration: metav1.Duration{Duration: DefaultLeaderElectionLeaseDuration},
						RenewDeadline: metav1.Duration{Duration: DefaultLeaderElectionRenewDeadline},
						RetryPeriod:   metav1.Duration{Duration: DefaultLeaderElectionRetryPeriod},
						ResourceLock:  DefaultResourceLock,
						ResourceName:  DefaultLeaderElectionID,
					},
				},
				InternalCertManagement: &InternalCertManagement{
					Enable: ptr.To(false),
				},
				ClientConnection: defaultClientConnection,
			},
		},
		"should not default ControllerManager": {
			original: &Configuration{
				ControllerManager: ControllerManager{
					Webhook: ControllerWebhook{
						Port: ptr.To(overwriteWebhookPort),
					},
					Metrics: ControllerMetrics{
						BindAddress: overwriteMetricBindAddress,
					},
					Health: ControllerHealth{
						HealthProbeBindAddress: overwriteHealthProbeBindAddress,
					},
					LeaderElection: &configv1alpha1.LeaderElectionConfiguration{
						LeaderElect:   ptr.To(true),
						LeaseDuration: metav1.Duration{Duration: DefaultLeaderElectionLeaseDuration},
						RenewDeadline: metav1.Duration{Duration: DefaultLeaderElectionRenewDeadline},
						RetryPeriod:   metav1.Duration{Duration: DefaultLeaderElectionRetryPeriod},
						ResourceLock:  DefaultResourceLock,
						ResourceName:  overwriteLeaderElectionID,
					},
				},
				InternalCertManagement: &InternalCertManagement{
					Enable: ptr.To(false),
				},
			},
			want: &Configuration{
				ControllerManager: ControllerManager{
					Webhook: ControllerWebhook{
						Port: ptr.To(overwriteWebhookPort),
					},
					Metrics: ControllerMetrics{
						BindAddress: overwriteMetricBindAddress,
					},
					Health: ControllerHealth{
						HealthProbeBindAddress: overwriteHealthProbeBindAddress,
					},
					LeaderElection: &configv1alpha1.LeaderElectionConfiguration{
						LeaderElect:   ptr.To(true),
						LeaseDuration: metav1.Duration{Duration: DefaultLeaderElectionLeaseDuration},
						RenewDeadline: metav1.Duration{Duration: DefaultLeaderElectionRenewDeadline},
						RetryPeriod:   metav1.Duration{Duration: DefaultLeaderElectionRetryPeriod},
						ResourceLock:  DefaultResourceLock,
						ResourceName:  overwriteLeaderElectionID,
					},
				},
				InternalCertManagement: &InternalCertManagement{
					Enable: ptr.To(false),
				},
				ClientConnection: defaultClientConnection,
			},
		},
		"should not set LeaderElectionID": {
			original: &Configuration{
				ControllerManager: ControllerManager{
					LeaderElection: &configv1alpha1.LeaderElectionConfiguration{
						LeaderElect: ptr.To(false),
					},
				},
				InternalCertManagement: &InternalCertManagement{
					Enable: ptr.To(false),
				},
			},
			want: &Configuration{
				ControllerManager: ControllerManager{
					Webhook: ControllerWebhook{
						Port: ptr.To(DefaultWebhookPort),
					},
					Metrics: ControllerMetrics{
						BindAddress: DefaultMetricsBindAddress,
					},
					Health: ControllerHealth{
						HealthProbeBindAddress: DefaultHealthProbeBindAddress,
					},
					LeaderElection: &configv1alpha1.LeaderElectionConfiguration{
						LeaderElect:   ptr.To(false),
						LeaseDuration: metav1.Duration{Duration: DefaultLeaderElectionLeaseDuration},
						RenewDeadline: metav1.Duration{Duration: DefaultLeaderElectionRenewDeadline},
						RetryPeriod:   metav1.Duration{Duration: DefaultLeaderElectionRetryPeriod},
						ResourceLock:  DefaultResourceLock,
						ResourceName:  DefaultLeaderElectionID,
					},
				},
				InternalCertManagement: &InternalCertManagement{
					Enable: ptr.To(false),
				},
				ClientConnection: defaultClientConnection,
			},
		},
		"defaulting InternalCertManagement": {
			original: &Configuration{},
			want: &Configuration{
				ControllerManager: defaultCtrlManagerConfigurationSpec,
				InternalCertManagement: &InternalCertManagement{
					Enable:             ptr.To(true),
					WebhookServiceName: ptr.To(DefaultWebhookServiceName),
					WebhookSecretName:  ptr.To(DefaultWebhookSecretName),
				},
				ClientConnection: defaultClientConnection,
			},
		},
		"should not default InternalCertManagement": {
			original: &Configuration{
				InternalCertManagement: &InternalCertManagement{
					Enable: ptr.To(false),
				},
			},
			want: &Configuration{
				ControllerManager: defaultCtrlManagerConfigurationSpec,
				InternalCertManagement: &InternalCertManagement{
					Enable: ptr.To(false),
				},
				ClientConnection: defaultClientConnection,
			},
		},
		"should not default values in custom ClientConnection": {
			original: &Configuration{
				InternalCertManagement: &InternalCertManagement{
					Enable: ptr.To(false),
				},
				ClientConnection: &ClientConnection{
					QPS:   ptr.To[float32](123.0),
					Burst: ptr.To[int32](456),
				},
			},
			want: &Configuration{
				ControllerManager: defaultCtrlManagerConfigurationSpec,
				InternalCertManagement: &InternalCertManagement{
					Enable: ptr.To(false),
				},
				ClientConnection: &ClientConnection{
					QPS:   ptr.To[float32](123.0),
					Burst: ptr.To[int32](456),
				},
			},
		},
		"should default empty custom ClientConnection": {
			original: &Configuration{
				InternalCertManagement: &InternalCertManagement{
					Enable: ptr.To(false),
				},
				ClientConnection: &ClientConnection{},
			},
			want: &Configuration{
				ControllerManager: defaultCtrlManagerConfigurationSpec,
				InternalCertManagement: &InternalCertManagement{
					Enable: ptr.To(false),
				},
				ClientConnection: defaultClientConnection,
			},
		},
	}

	for name, tc := range testCases {
		t.Run(name, func(t *testing.T) {
			SetDefaults_Configuration(tc.original)
			if diff := cmp.Diff(tc.want, tc.original); diff != "" {
				t.Errorf("unexpected error (-want,+got):\n%s", diff)
			}
		})
	}
}

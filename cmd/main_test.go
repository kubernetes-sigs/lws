/*
Copyright 2025.

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

package main

import (
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrlcache "sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
)

func TestApply(t *testing.T) {
	tmpDir := t.TempDir()

	testConfig := filepath.Join(tmpDir, "test_config.yaml")
	if err := os.WriteFile(testConfig, []byte(`
apiVersion: config.lws.x-k8s.io/v1alpha1
kind: Configuration
health:
  healthProbeBindAddress: :8081
leaderElection:
  leaderElect: true
  resourceName: test
  leaseDuration: 5m
  renewDeadline: 5m
  retryPeriod: 5m
  resourceLock: test
webhook:
  port: 9443
internalCertManagement:
  enable: true
  webhookServiceName: lws-tenant-a-webhook-service
  webhookSecretName: lws-tenant-a-webhook-server-cert
`), os.FileMode(0600)); err != nil {
		t.Fatal(err)
	}

	ctrlOptsCmpOpts := []cmp.Option{
		cmpopts.IgnoreUnexported(ctrl.Options{}),
		cmpopts.IgnoreUnexported(webhook.DefaultServer{}),
		cmpopts.IgnoreUnexported(ctrlcache.Options{}),
		cmpopts.IgnoreUnexported(net.ListenConfig{}),
		cmpopts.IgnoreFields(ctrl.Options{}, "Scheme", "Logger", "Metrics", "WebhookServer", "LeaderElectionNamespace"),
	}

	testCases := []struct {
		name                     string
		flagtrack                map[string]bool
		configFile               string
		probeAddr                string
		enableLeaderElection     bool
		leaderElectLeaseDuration time.Duration
		leaderElectRenewDeadline time.Duration
		leaderElectRetryPeriod   time.Duration
		leaderElectResourceLock  string
		leaderElectionID         string
		expectedOpts             ctrl.Options
	}{
		{
			name:       "flags overwrite",
			configFile: testConfig,
			flagtrack: map[string]bool{
				"health-probe-bind-address":   true,
				"leader-elect":                true,
				"leader-elect-lease-duration": true,
				"leader-elect-renew-deadline": true,
				"leader-elect-retry-period":   true,
				"leader-elect-resource-lock":  true,
				"leader-elect-resource-name":  true,
			},
			probeAddr:                ":9443",
			enableLeaderElection:     false,
			leaderElectLeaseDuration: 1 * time.Minute,
			leaderElectRenewDeadline: 1 * time.Minute,
			leaderElectRetryPeriod:   1 * time.Minute,
			leaderElectResourceLock:  "changed",
			leaderElectionID:         "changed",
			expectedOpts: ctrl.Options{
				LeaderElection:             false,
				LeaderElectionResourceLock: "changed",
				LeaderElectionID:           "changed",
				LeaseDuration:              ptr.To(1 * time.Minute),
				RenewDeadline:              ptr.To(1 * time.Minute),
				RetryPeriod:                ptr.To(1 * time.Minute),
				HealthProbeBindAddress:     ":9443",
				LivenessEndpointName:       "/healthz",
				ReadinessEndpointName:      "/readyz",
			},
		},
		{
			name:                     "no flag overwrite",
			configFile:               testConfig,
			flagtrack:                map[string]bool{},
			probeAddr:                ":9443",
			enableLeaderElection:     false,
			leaderElectLeaseDuration: 1 * time.Minute,
			leaderElectRenewDeadline: 1 * time.Minute,
			leaderElectRetryPeriod:   1 * time.Minute,
			leaderElectResourceLock:  "changed",
			leaderElectionID:         "changed",
			expectedOpts: ctrl.Options{
				LeaderElection:             true,
				LeaderElectionResourceLock: "test",
				LeaderElectionID:           "test",
				LeaseDuration:              ptr.To(5 * time.Minute),
				RenewDeadline:              ptr.To(5 * time.Minute),
				RetryPeriod:                ptr.To(5 * time.Minute),
				HealthProbeBindAddress:     ":8081",
				LivenessEndpointName:       "/healthz",
				ReadinessEndpointName:      "/readyz",
			},
		},
		{
			name:       "partial flag overwrite",
			configFile: testConfig,
			flagtrack: map[string]bool{
				"health-probe-bind-address": true,
				"leader-elect":              true,
			},
			probeAddr:                ":9443",
			enableLeaderElection:     false,
			leaderElectLeaseDuration: 1 * time.Minute,
			leaderElectRenewDeadline: 1 * time.Minute,
			leaderElectRetryPeriod:   1 * time.Minute,
			leaderElectResourceLock:  "changed",
			leaderElectionID:         "changed",
			expectedOpts: ctrl.Options{
				LeaderElection:             false,
				LeaderElectionResourceLock: "test",
				LeaderElectionID:           "test",
				LeaseDuration:              ptr.To(5 * time.Minute),
				RenewDeadline:              ptr.To(5 * time.Minute),
				RetryPeriod:                ptr.To(5 * time.Minute),
				HealthProbeBindAddress:     ":9443",
				LivenessEndpointName:       "/healthz",
				ReadinessEndpointName:      "/readyz",
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			flagsSet = tc.flagtrack
			opts, _, err := apply(tc.configFile,
				tc.probeAddr,
				tc.enableLeaderElection,
				tc.leaderElectLeaseDuration,
				tc.leaderElectRenewDeadline,
				tc.leaderElectRetryPeriod,
				tc.leaderElectResourceLock,
				tc.leaderElectionID,
				"metrics-addr")
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if diff := cmp.Diff(tc.expectedOpts, opts, ctrlOptsCmpOpts...); diff != "" {
				t.Errorf("Unexpected options (-want +got):\n%s", diff)
			}
		})
	}
}

/*
Copyright 2026 The Kubernetes Authors.

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
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	configapi "sigs.k8s.io/lws/api/config/v1"
	"sigs.k8s.io/lws/pkg/computedomain"
	"sigs.k8s.io/lws/pkg/features"
	"sigs.k8s.io/lws/pkg/schedulerprovider"
)

type fakeWebhookServer struct {
	webhook.Server
	startedErr error
}

func (s *fakeWebhookServer) StartedChecker() healthz.Checker {
	return func(*http.Request) error { return s.startedErr }
}

type fakeManager struct {
	ctrl.Manager
	webhookServer      *fakeWebhookServer
	webhookServerCalls int
	readyz             healthz.Checker
}

func (m *fakeManager) AddHealthzCheck(string, healthz.Checker) error { return nil }

func (m *fakeManager) AddReadyzCheck(_ string, check healthz.Checker) error {
	m.readyz = check
	return nil
}

func (m *fakeManager) GetWebhookServer() webhook.Server {
	m.webhookServerCalls++
	return m.webhookServer
}

func TestReadyzCheck(t *testing.T) {
	errNotStarted := errors.New("webhook server has not been started yet")

	testCases := []struct {
		name                   string
		enableWebhooks         bool
		webhooksRegistered     bool
		startedErr             error
		wantErr                bool
		wantWebhookServerCalls int
	}{
		{
			name:       "webhooks disabled",
			startedErr: errNotStarted,
		},
		{
			name:           "webhooks not registered yet",
			enableWebhooks: true,
			wantErr:        true,
		},
		{
			name:                   "webhook server not started",
			enableWebhooks:         true,
			webhooksRegistered:     true,
			startedErr:             errNotStarted,
			wantErr:                true,
			wantWebhookServerCalls: 1,
		},
		{
			name:                   "webhook server started",
			enableWebhooks:         true,
			webhooksRegistered:     true,
			wantWebhookServerCalls: 1,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			mgr := &fakeManager{webhookServer: &fakeWebhookServer{startedErr: tc.startedErr}}
			webhooksReady := make(chan struct{})
			if tc.webhooksRegistered {
				close(webhooksReady)
			}

			setupHealthzAndReadyzCheck(mgr, webhooksReady, tc.enableWebhooks)

			err := mgr.readyz(httptest.NewRequest(http.MethodGet, "/readyz", nil))
			if gotErr := err != nil; gotErr != tc.wantErr {
				t.Errorf("readyz() error = %v, wantErr %v", err, tc.wantErr)
			}
			if mgr.webhookServerCalls != tc.wantWebhookServerCalls {
				t.Errorf("GetWebhookServer() calls = %d, want %d", mgr.webhookServerCalls, tc.wantWebhookServerCalls)
			}
		})
	}
}

func defaultTestRESTMapper() *meta.DefaultRESTMapper {
	mapper := meta.NewDefaultRESTMapper([]schema.GroupVersion{
		corev1.SchemeGroupVersion,
		appsv1.SchemeGroupVersion,
	})
	mapper.Add(corev1.SchemeGroupVersion.WithKind("Pod"), meta.RESTScopeNamespace)
	mapper.Add(appsv1.SchemeGroupVersion.WithKind("ReplicaSet"), meta.RESTScopeNamespace)
	return mapper
}

func newTestManager(t *testing.T, mapper meta.RESTMapper) ctrl.Manager {
	t.Helper()
	if mapper == nil {
		mapper = defaultTestRESTMapper()
	}
	mgr, err := ctrl.NewManager(&rest.Config{Host: "https://127.0.0.1:6443"}, ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: "0",
		WebhookServer:          webhook.NewServer(webhook.Options{Port: 0}),
		Controller: config.Controller{
			SkipNameValidation: ptr.To(true),
		},
		MapperProvider: func(*rest.Config, *http.Client) (meta.RESTMapper, error) {
			return mapper, nil
		},
	})
	if err != nil {
		t.Fatalf("ctrl.NewManager() unexpected error: %v", err)
	}
	return mgr
}

func TestSetupControllersIndependentOfCertsReady(t *testing.T) {
	mgr := newTestManager(t, nil)
	cfg := configapi.Configuration{
		GangSchedulingManagement: &configapi.GangSchedulingManagement{
			SchedulerProvider: ptr.To(string(schedulerprovider.Kubernetes)),
		},
	}

	certsReady := make(chan struct{})
	webhooksReady := make(chan struct{})

	// setupControllers must succeed immediately even though certsReady is still open.
	sp, providerType, err := setupControllers(mgr, cfg)
	if err != nil {
		t.Fatalf("setupControllers() unexpected error: %v", err)
	}
	if sp == nil {
		t.Fatal("setupControllers() returned nil SchedulerProvider")
	}
	if providerType != schedulerprovider.Kubernetes {
		t.Fatalf("setupControllers() providerType = %q, want %q", providerType, schedulerprovider.Kubernetes)
	}

	errCh := make(chan error, 1)
	go func() {
		errCh <- setupWebhooks(mgr, certsReady, webhooksReady, sp, providerType)
	}()

	// While certsReady is open, webhooksReady must remain open.
	select {
	case <-webhooksReady:
		t.Fatal("webhooksReady closed before certsReady")
	case err := <-errCh:
		t.Fatalf("setupWebhooks() returned before certsReady closed: %v", err)
	case <-time.After(50 * time.Millisecond):
	}

	close(certsReady)

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("setupWebhooks() unexpected error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for setupWebhooks() after closing certsReady")
	}

	select {
	case <-webhooksReady:
	default:
		t.Fatal("expected webhooksReady to be closed after setupWebhooks() completed")
	}

	mux := mgr.GetWebhookServer().WebhookMux()
	expectedPaths := []string{
		"/mutate-leaderworkerset-x-k8s-io-v1-leaderworkerset",
		"/validate-leaderworkerset-x-k8s-io-v1-leaderworkerset",
		"/mutate--v1-pod",
		"/validate--v1-pod",
		"/validate-disaggregatedset-x-k8s-io-v1-disaggregatedset",
	}
	for _, p := range expectedPaths {
		req := httptest.NewRequest(http.MethodPost, p, nil)
		if _, pattern := mux.Handler(req); pattern != p {
			t.Errorf("webhook path %q not registered on mux (got pattern %q)", p, pattern)
		}
	}
}

func TestSetupControllersErrors(t *testing.T) {
	t.Run("unknown scheduler provider", func(t *testing.T) {
		mgr := newTestManager(t, nil)
		cfg := configapi.Configuration{
			GangSchedulingManagement: &configapi.GangSchedulingManagement{
				SchedulerProvider: ptr.To("unknown-provider"),
			},
		}
		if _, _, err := setupControllers(mgr, cfg); err == nil {
			t.Fatal("expected error for unknown scheduler provider, got nil")
		}
	})

	t.Run("AutoComputeDomain without CRD fails", func(t *testing.T) {
		features.SetFeatureGateDuringTest(t, features.AutoComputeDomain, true)
		mgr := newTestManager(t, nil)
		_, _, err := setupControllers(mgr, configapi.Configuration{})
		if err == nil || !strings.Contains(err.Error(), "ComputeDomain CRD") {
			t.Fatalf("setupControllers() error = %v, want ComputeDomain CRD error", err)
		}
	})

	t.Run("AutoComputeDomain with CRD succeeds", func(t *testing.T) {
		features.SetFeatureGateDuringTest(t, features.AutoComputeDomain, true)
		gv := schema.GroupVersion{Group: computedomain.GroupKind.Group, Version: computedomain.Version}
		mapper := defaultTestRESTMapper()
		mapper.Add(gv.WithKind(computedomain.GroupKind.Kind), meta.RESTScopeNamespace)
		mgr := newTestManager(t, mapper)
		if _, _, err := setupControllers(mgr, configapi.Configuration{}); err != nil {
			t.Fatalf("setupControllers() unexpected error: %v", err)
		}
	})
}

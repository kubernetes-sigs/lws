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
	"testing"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
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

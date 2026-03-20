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

package config

import (
	"crypto/tls"
	"reflect"
	"testing"

	configapi "sigs.k8s.io/lws/api/config/v1alpha1"
)

func TestParseTLSOptions(t *testing.T) {
	tests := []struct {
		name    string
		config  *configapi.TLSOptions
		want    *TLS
		wantErr bool
	}{
		{
			name:    "nil config",
			config:  nil,
			want:    nil,
			wantErr: false,
		},
		{
			name: "valid min version",
			config: &configapi.TLSOptions{
				MinVersion: "VersionTLS12",
			},
			want: &TLS{
				MinVersion: tls.VersionTLS12,
			},
		},
		{
			name: "invalid min version",
			config: &configapi.TLSOptions{
				MinVersion: "InvalidVersion",
			},
			wantErr: true,
		},
		{
			name: "valid cipher suites",
			config: &configapi.TLSOptions{
				CipherSuites: []string{"TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256"}, // ← TLS 1.2 ✓
			},
			want: &TLS{
				CipherSuites: []uint16{tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256},
			},
		},
		{
			name: "invalid cipher suites",
			config: &configapi.TLSOptions{
				CipherSuites: []string{"TLS_INVALID_CIPHER"},
			},
			wantErr: true,
		},
		{
			name: "invalid: min version TLS 1.3 with cipher suites",
			config: &configapi.TLSOptions{
				MinVersion:   "VersionTLS13",
				CipherSuites: []string{"TLS_AES_256_GCM_SHA384"},
			},
			wantErr: true,
		},
		{
			name: "valid min version and cipher suites",
			config: &configapi.TLSOptions{
				MinVersion:   "VersionTLS12",
				CipherSuites: []string{"TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256"},
			},
			want: &TLS{
				MinVersion:   tls.VersionTLS12,
				CipherSuites: []uint16{tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseTLSOptions(tt.config)

			if (err != nil) != tt.wantErr {
				t.Errorf("ParseTLSOptions() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			if !tt.wantErr && !reflect.DeepEqual(got, tt.want) {
				t.Errorf("ParseTLSOptions() mismatch: want %v, got %v", tt.want, got)
			}
		})
	}
}

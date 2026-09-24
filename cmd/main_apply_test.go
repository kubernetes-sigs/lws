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
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// applyErrWithDefaults calls apply with placeholder flag values and returns only
// the error, so tests only have to care about the config file.
func applyErrWithDefaults(t *testing.T, configFile string) error {
	t.Helper()
	_, _, err := apply(configFile,
		":8081",
		false,
		1*time.Minute,
		1*time.Minute,
		1*time.Minute,
		"leases",
		"lws",
		":8080")
	return err
}

func TestApplyErrors(t *testing.T) {
	tmpDir := t.TempDir()

	t.Run("missing config file", func(t *testing.T) {
		flagsSet = map[string]bool{}
		if err := applyErrWithDefaults(t, filepath.Join(tmpDir, "does-not-exist.yaml")); err == nil {
			t.Fatal("apply() with a missing config file must return an error")
		}
	})

	t.Run("malformed config file", func(t *testing.T) {
		malformed := filepath.Join(tmpDir, "malformed.yaml")
		if err := os.WriteFile(malformed, []byte("not: [valid"), os.FileMode(0600)); err != nil {
			t.Fatal(err)
		}
		flagsSet = map[string]bool{}
		if err := applyErrWithDefaults(t, malformed); err == nil {
			t.Fatal("apply() with a malformed config file must return an error")
		}
	})

	t.Run("invalid TLS configuration", func(t *testing.T) {
		invalidTLS := filepath.Join(tmpDir, "invalid_tls.yaml")
		if err := os.WriteFile(invalidTLS, []byte(`
apiVersion: config.lws.x-k8s.io/v1alpha1
kind: Configuration
tls:
  minVersion: TLS99
`), os.FileMode(0600)); err != nil {
			t.Fatal(err)
		}
		flagsSet = map[string]bool{}
		err := applyErrWithDefaults(t, invalidTLS)
		if err == nil {
			t.Fatal("apply() with an unknown TLS version must return an error")
		}
		if !strings.Contains(err.Error(), "TLS options") {
			t.Errorf("apply() error = %v, want it to mention the TLS options", err)
		}
	})
}

func TestApplyMetricsBindAddress(t *testing.T) {
	tmpDir := t.TempDir()
	configFile := filepath.Join(tmpDir, "metrics.yaml")
	if err := os.WriteFile(configFile, []byte(`
apiVersion: config.lws.x-k8s.io/v1alpha1
kind: Configuration
metrics:
  bindAddress: :7777
`), os.FileMode(0600)); err != nil {
		t.Fatal(err)
	}

	t.Run("the config value is used when the flag is unset", func(t *testing.T) {
		flagsSet = map[string]bool{}
		opts, _, err := apply(configFile, ":8081", false, time.Minute, time.Minute, time.Minute, "leases", "lws", ":9999")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if opts.Metrics.BindAddress != ":7777" {
			t.Errorf("metrics bind address = %s, want :7777 from the config file", opts.Metrics.BindAddress)
		}
	})

	t.Run("the flag wins when it is set", func(t *testing.T) {
		flagsSet = map[string]bool{"metrics-bind-address": true}
		opts, _, err := apply(configFile, ":8081", false, time.Minute, time.Minute, time.Minute, "leases", "lws", ":9999")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if opts.Metrics.BindAddress != ":9999" {
			t.Errorf("metrics bind address = %s, want :9999 from the flag", opts.Metrics.BindAddress)
		}
	})
}

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
	"errors"
	"fmt"

	cliflag "k8s.io/component-base/cli/flag"

	configapi "sigs.k8s.io/lws/api/config/v1alpha1"
)

// TLS holds the parsed TLS configuration
type TLS struct {
	MinVersion   uint16
	CipherSuites []uint16
}

// ParseTLSOptions parses the API configuration into a TLS struct
func ParseTLSOptions(cfg *configapi.TLSOptions) (*TLS, error) {
	ret := &TLS{}
	var errRet error
	if cfg == nil {
		return nil, nil
	}

	// TLS 1.3 validation check (Added here because LWS doesn't have a separate validation webhook)
	if cfg.MinVersion == "VersionTLS13" && len(cfg.CipherSuites) > 0 {
		return nil, fmt.Errorf("cipher suites may not be specified when minVersion is 'VersionTLS13'")
	}

	version, err := convertTLSMinVersion(cfg.MinVersion)
	if err != nil {
		errRet = errors.Join(errRet, err)

	}
	ret.MinVersion = version

	// Set cipher suites
	if len(cfg.CipherSuites) > 0 {
		cipherSuites, err := convertCipherSuites(cfg.CipherSuites)
		if err != nil {
			errRet = errors.Join(errRet, err)
		}
		if err == nil && len(cipherSuites) > 0 {
			ret.CipherSuites = cipherSuites
		}
	}
	return ret, errRet
}

// BuildTLSOptions converts TLSOptions from the configuration to controller-runtime TLSOpts
func BuildTLSOptions(tlsOptions *TLS) []func(*tls.Config) {
	if tlsOptions == nil {
		return nil
	}

	var tlsOpts []func(*tls.Config)

	tlsOpts = append(tlsOpts, func(c *tls.Config) {
		if tlsOptions.MinVersion != 0 {
			c.MinVersion = tlsOptions.MinVersion
		}
		if len(tlsOptions.CipherSuites) > 0 {
			c.CipherSuites = tlsOptions.CipherSuites
		}
	})

	return tlsOpts
}

// convertTLSMinVersion converts a TLS version string to the corresponding uint16 constant
func convertTLSMinVersion(tlsMinVersion string) (uint16, error) {
	if tlsMinVersion == "" {
		return 0, nil // Don't default if not provided, let Go handle it
	}
	if tlsMinVersion == "VersionTLS11" || tlsMinVersion == "VersionTLS10" {
		return 0, errors.New("invalid minVersion. Please use VersionTLS12 or VersionTLS13")
	}
	version, err := cliflag.TLSVersion(tlsMinVersion)
	if err != nil {
		return 0, fmt.Errorf("invalid minVersion: %w. Please use VersionTLS12 or VersionTLS13", err)
	}
	return version, nil
}

// convertCipherSuites converts cipher suite names to their crypto/tls constants
func convertCipherSuites(cipherSuites []string) ([]uint16, error) {
	if len(cipherSuites) == 0 {
		return nil, nil
	}
	suites, err := cliflag.TLSCipherSuites(cipherSuites)
	if err != nil {
		return nil, fmt.Errorf("invalid cipher suites: %w. Please use the secure cipher names: %v", err, cliflag.PreferredTLSCipherNames())
	}
	return suites, nil
}

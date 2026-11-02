//go:build !linux && !darwin

// Copyright 2026 The Kubernetes Authors.
// SPDX-License-Identifier: Apache-2.0
package main

import "fmt"

func newProcessGroup() error {
	return fmt.Errorf("isolated launcher process groups require Linux or macOS")
}

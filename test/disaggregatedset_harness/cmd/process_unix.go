//go:build linux || darwin

// Copyright 2026 The Kubernetes Authors.
// SPDX-License-Identifier: Apache-2.0
package main

import "syscall"

func newProcessGroup() error {
	_, err := syscall.Setsid()
	return err
}

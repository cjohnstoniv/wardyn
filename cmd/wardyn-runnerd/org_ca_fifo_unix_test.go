// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package main

import (
	"syscall"
	"testing"
)

// makeAnchorFIFO creates a FIFO at path for the shared anchor-file cases.
func makeAnchorFIFO(t *testing.T, path string) {
	t.Helper()
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Skip(err)
	}
}

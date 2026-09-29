// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"os"
	"testing"
)

// mustMkdirAll creates dir (and any missing parents) with perm, failing the
// test immediately on error. It is the "make a REAL directory this fixture
// binds against" prologue that user_drives_test.go, user_drives_run_test.go,
// user_drives_resolve_test.go and user_drives_probe_strand_test.go each
// repeated by hand: every host_path/share fixture below needs a directory
// that actually exists on disk (the env-ceiling check resolves it), and Go's
// error-per-call idiom turned that single fact into a three-line block at
// every call site.
func mustMkdirAll(t *testing.T, dir string, perm os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(dir, perm); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
}

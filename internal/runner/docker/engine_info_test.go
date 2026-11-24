// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package docker

import (
	"context"
	"testing"

	"github.com/google/uuid"
)

// engineVersionOf is the fake engine's version, as a create reads it.
func engineVersionOf(t *testing.T, d *Driver) string {
	t.Helper()
	v, err := d.engineVersion(context.Background())
	if err != nil {
		t.Fatalf("engineVersion: %v", err)
	}
	return v
}

// TestCreateSandbox_ReadsTheEngineOnce: a container sandbox create reads the Docker Engine's details once, and reads
// them again on the next create rather than keeping the version.
func TestCreateSandbox_ReadsTheEngineOnce(t *testing.T) {
	f := newFakeDocker()
	f.images["busybox:latest"] = true
	f.info.Architecture = "x86_64" // so engineArch's cache holds after the first create
	d := newTestDriver(f)
	if _, err := d.CreateSandbox(context.Background(), testSpec()); err != nil {
		t.Fatalf("CreateSandbox (warm): %v", err)
	}
	f.mu.Lock()
	f.infoCalls = 0
	f.mu.Unlock()
	spec := testSpec()
	spec.RunID = uuid.New()
	if _, err := d.CreateSandbox(context.Background(), spec); err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.infoCalls != 1 {
		t.Fatalf("a create made %d Info calls, want 1", f.infoCalls)
	}
}

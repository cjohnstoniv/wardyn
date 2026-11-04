// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker && unix

package docker

import (
	"context"
	"errors"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/runner"
)

// TestMacVMFreezeIsUnsupported: a macOS VM has no pause, so Freeze and Thaw say so (as a container
// under another runtime does) instead of reporting a pause that never happened.
func TestMacVMFreezeIsUnsupported(t *testing.T) {
	r := newMacRig(t)
	sb, err := r.d.CreateSandbox(context.Background(), macSpec())
	if err != nil {
		t.Fatal(err)
	}
	if err := r.d.FreezeSandbox(context.Background(), sb.Ref); !errors.Is(err, runner.ErrFreezeUnsupported) {
		t.Fatalf("FreezeSandbox = %v, want ErrFreezeUnsupported", err)
	}
	if err := r.d.ThawSandbox(context.Background(), sb.Ref); !errors.Is(err, runner.ErrFreezeUnsupported) {
		t.Fatalf("ThawSandbox = %v, want ErrFreezeUnsupported", err)
	}
}

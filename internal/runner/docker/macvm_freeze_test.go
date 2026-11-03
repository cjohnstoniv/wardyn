// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker && unix

package docker

import (
	"context"
	"testing"
)

// TestMacVMFreezeIsNil pins today's answer: a macOS VM ref is no container, so Freeze and Thaw find
// nothing and report success without pausing anything.
func TestMacVMFreezeIsNil(t *testing.T) {
	r := newMacRig(t)
	sb, err := r.d.CreateSandbox(context.Background(), macSpec())
	if err != nil {
		t.Fatal(err)
	}
	if err := r.d.FreezeSandbox(context.Background(), sb.Ref); err != nil {
		t.Fatalf("FreezeSandbox = %v", err)
	}
	if err := r.d.ThawSandbox(context.Background(), sb.Ref); err != nil {
		t.Fatalf("ThawSandbox = %v", err)
	}
}

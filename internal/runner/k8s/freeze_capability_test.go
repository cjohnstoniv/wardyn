// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package k8s

import (
	"context"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestCapabilitiesNeverAdvertiseFreeze pins the conformance claim behind
// long-holds design rev 4 §3's "control plane refuses pause on Kubernetes": a
// stopped pod is gone (its ephemeral, node-local writable layer with it), so
// there is nothing FreezeSandbox could pause and ThawSandbox resume — unlike
// Docker's `docker pause`, which suspends a container's processes in place
// without losing its filesystem. The Driver type has no FreezeSandbox method
// at all, so it can never satisfy runner.Freezer, and Classes() never sets
// ClassSupport.Freeze for CC1, CC2 or CC3 — the orchestrator (Capabilities,
// substrateFor's Freeze[c] = cs.Freeze[c]) therefore has nothing true to
// aggregate for any class this substrate serves, whatever RuntimeClass an
// operator pins. No existing test reads ClassSupport.Freeze or the Freezer
// assertion at all.
func TestCapabilitiesNeverAdvertiseFreeze(t *testing.T) {
	// A Driver is never a runner.Freezer: this is a compile-time-checkable
	// fact today, but the point of asserting it here, at runtime, is that a
	// future FreezeSandbox added to this package (even an unverified one, the
	// way the docker substrate's Freeze:false-until-verified language warns
	// about) trips this test the moment it exists, rather than silently
	// starting to satisfy the interface unnoticed.
	d, cs := newTestDriver(t, Config{ConfinementRuntimes: map[types.ConfinementClass]string{
		types.CC2: "gvisor", types.CC3: "my-microvm",
	}})
	if _, ok := any(d).(runner.Freezer); ok {
		t.Fatal("the k8s Driver satisfies runner.Freezer; a stopped pod is gone, so this substrate must never claim it can pause and resume one")
	}

	mustCreateRuntimeClass(t, cs, "gvisor", "runsc")
	mustCreateRuntimeClass(t, cs, "my-microvm", "firecracker")

	got, err := d.Classes(context.Background())
	if err != nil {
		t.Fatalf("Classes: %v", err)
	}
	// Sanity: CC2 and CC3 are actually present, so the Freeze checks below
	// are exercising real advertised classes, not an empty set.
	for _, want := range []types.ConfinementClass{types.CC1, types.CC2, types.CC3} {
		if !containsClass(got.Classes, want) {
			t.Fatalf("Classes = %v; want it to contain %s so the Freeze assertion below is meaningful", got.Classes, want)
		}
	}
	for _, c := range got.Classes {
		if got.Freeze[c] {
			t.Errorf("ClassSupport.Freeze[%s] = true; the k8s substrate must never advertise Freeze for any class", c)
		}
	}
}

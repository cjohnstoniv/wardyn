// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"strings"
	"testing"
)

// D4: WARDYN_REQUIRE_OPERATOR_SET_EGRESS defaults ON as of 0.7.
//
// The gate itself shipped in 0.6 — built, wired, documented, tested — and OFF,
// because flipping it narrows egress for existing workspaces on upgrade. 0.7
// flips it, because the asymmetry was the anomaly: the SECRET side of the very
// same switch statement has always applied this provenance check
// unconditionally, and calls the boundary "security-critical — do not relax".
// The reason is identical for egress — a hostile, or simply never-reviewed,
// repo could widen a run's allowlist just by naming a host in a committed file,
// with no operator ever acting.
//
// This pins the DEFAULT, which no internal/api test can: those construct
// api.Config directly and set the field explicitly, so every one of them would
// stay green if the flag default silently reverted.
func TestRequireOperatorSetEgress_DefaultsOn(t *testing.T) {
	// The flag declaration is the single source of the default; read it there
	// rather than booting a daemon.
	src, err := os.ReadFile("boot_flags.go")
	if err != nil {
		t.Fatalf("read boot_flags.go: %v", err)
	}
	const decl = `flagBool("require-operator-set-egress", "WARDYN_REQUIRE_OPERATOR_SET_EGRESS", `
	i := strings.Index(string(src), decl)
	if i < 0 {
		t.Fatalf("could not find the require-operator-set-egress flag declaration — this test can no longer see the default it exists to pin")
	}
	rest := string(src)[i+len(decl):]
	if !strings.HasPrefix(rest, "true,") {
		got, _, _ := strings.Cut(rest, ",")
		t.Errorf("WARDYN_REQUIRE_OPERATOR_SET_EGRESS default = %q, want true.\n"+
			"A scan_seeded egress host comes from the workspace scanner reading UNTRUSTED repo content; "+
			"auto-adding it lets a repo widen its own run's allowlist with no operator action. "+
			"The secret side of the same switch has always refused this unconditionally.", got)
	}
}

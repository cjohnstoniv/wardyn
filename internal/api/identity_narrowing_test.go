// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import "testing"

func TestIdentityNarrowing_DefaultIsAudit(t *testing.T) {
	t.Setenv(envIdentityNarrowing, "")
	if identityNarrowingEnforced() {
		t.Fatal("unset must not enforce")
	}
	if identityNarrowingDefault != "audit" {
		t.Fatalf("default = %q, want audit", identityNarrowingDefault)
	}
	if got := identityNarrowingMode(); got != "audit" {
		t.Fatalf("mode = %q, want audit", got)
	}
}

func TestIdentityNarrowing_ValidateRefusesOtherValues(t *testing.T) {
	for _, v := range []string{"on", "true", "Enforce", " audit"} {
		t.Setenv(envIdentityNarrowing, v)
		if err := validateIdentityNarrowingSwitch(); err == nil {
			t.Errorf("%q: want an error", v)
		}
	}
	for _, v := range []string{"audit", "enforce", ""} {
		t.Setenv(envIdentityNarrowing, v)
		if err := validateIdentityNarrowingSwitch(); err != nil {
			t.Errorf("%q: %v", v, err)
		}
	}
}

func TestIdentityNarrowing_EnforceSelects(t *testing.T) {
	t.Setenv(envIdentityNarrowing, "enforce")
	if !identityNarrowingEnforced() {
		t.Fatal("enforce must select")
	}
	if got := identityNarrowingMode(); got != "enforce" {
		t.Fatalf("mode = %q, want enforce", got)
	}
}

// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"strings"
	"testing"
)

// The 0.9 name of the PUT-existing-only switch is refused by name at boot; the new name and an empty
// old name boot.
func TestRefuseRenamedGovernanceEnv(t *testing.T) {
	err := refuseRenamedGovernanceEnv([]string{"PATH=/bin", "WARDYN_GOVERNANCE_PROFILE_PUT_EXISTING_ONLY=true"})
	if err == nil || !strings.Contains(err.Error(), "WARDYN_GOVERNANCE_UNIT_PUT_EXISTING_ONLY") ||
		!strings.Contains(err.Error(), "WARDYN_GOVERNANCE_PROFILE_PUT_EXISTING_ONLY") {
		t.Errorf("old name: err = %v, want a refusal naming both names", err)
	}
	for _, env := range [][]string{
		{"WARDYN_GOVERNANCE_UNIT_PUT_EXISTING_ONLY=true"},
		{"WARDYN_GOVERNANCE_PROFILE_PUT_EXISTING_ONLY="},
		nil,
	} {
		if err := refuseRenamedGovernanceEnv(env); err != nil {
			t.Errorf("env %v: refused (%v), want it to boot", env, err)
		}
	}
}

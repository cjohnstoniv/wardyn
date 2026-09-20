// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestValidatePolicySpec_PushRulesNil pins the wire-compat contract issue #176
// requires: a nil push_rules — every policy authored before this field existed
// — must validate exactly as it always did.
func TestValidatePolicySpec_PushRulesNil(t *testing.T) {
	spec := types.RunPolicySpec{MinConfinementClass: types.CC2}
	if err := validatePolicySpec(spec); err != nil {
		t.Errorf("nil push_rules rejected: %v", err)
	}
}

// TestValidatePolicySpec_PushRulesBounds pins push_rules' write-time shape:
// at most maxPushRulesDenyPaths entries, each at most maxPushRulesPathBytes
// bytes and free of control characters, and max_inspect_pack_mib in
// 0..maxPushRulesInspectPackMiB. Phase one only validates the strings — no
// matcher runs against them here (that lands with the enforcement change).
func TestValidatePolicySpec_PushRulesBounds(t *testing.T) {
	spec := func(pr *types.PushRulesSpec) types.RunPolicySpec {
		return types.RunPolicySpec{MinConfinementClass: types.CC2, PushRules: pr}
	}

	tooManyPaths := make([]string, maxPushRulesDenyPaths+1)
	for i := range tooManyPaths {
		tooManyPaths[i] = "a"
	}

	refused := []struct {
		name         string
		spec         types.RunPolicySpec
		wantContains string
	}{
		{"too many deny_paths", spec(&types.PushRulesSpec{DenyPaths: tooManyPaths}),
			"push_rules.deny_paths: at most"},
		{"empty deny_paths entry", spec(&types.PushRulesSpec{DenyPaths: []string{""}}),
			"push_rules.deny_paths[0]: empty entry"},
		{"deny_paths entry too long", spec(&types.PushRulesSpec{DenyPaths: []string{strings.Repeat("a", maxPushRulesPathBytes+1)}}),
			"push_rules.deny_paths[0]: exceeds"},
		{"deny_paths entry carries a NUL", spec(&types.PushRulesSpec{DenyPaths: []string{"deploy/\x00secret"}}),
			"push_rules.deny_paths[0]: control character"},
		{"deny_paths entry carries a control character", spec(&types.PushRulesSpec{DenyPaths: []string{"deploy/\x01"}}),
			"push_rules.deny_paths[0]: control character"},
		{"max_inspect_pack_mib negative", spec(&types.PushRulesSpec{MaxInspectPackMiB: -1}),
			"push_rules.max_inspect_pack_mib must be between"},
		{"max_inspect_pack_mib above the cap", spec(&types.PushRulesSpec{MaxInspectPackMiB: maxPushRulesInspectPackMiB + 1}),
			"push_rules.max_inspect_pack_mib must be between"},
	}
	for _, tc := range refused {
		t.Run(tc.name, func(t *testing.T) {
			err := validatePolicySpec(tc.spec)
			if err == nil {
				t.Fatalf("accepted %+v", tc.spec)
			}
			if !strings.Contains(err.Error(), tc.wantContains) {
				t.Errorf("error %q does not name %q", err, tc.wantContains)
			}
		})
	}

	// Negative control: production shapes at/under every bound must pass.
	for _, ok := range []types.RunPolicySpec{
		spec(nil),
		spec(&types.PushRulesSpec{}),
		spec(&types.PushRulesSpec{DenyPaths: []string{".github/workflows/**", "infra/**"}, MaxInspectPackMiB: 8}),
		spec(&types.PushRulesSpec{MaxInspectPackMiB: maxPushRulesInspectPackMiB}),
	} {
		if err := validatePolicySpec(ok); err != nil {
			t.Errorf("rejected a policy inside the bounds %+v: %v", ok, err)
		}
	}
}

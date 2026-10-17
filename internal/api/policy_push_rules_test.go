// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"

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
// each deny_paths entry at most maxPushRulesPathBytes bytes and free of
// control characters, and max_inspect_pack_mib in 0..maxPushRulesInspectPackMiB.
// Phase one only validates the strings — no matcher runs against them here
// (that lands with the enforcement change).
//
// Deliberately NO "too many deny_paths" case: unlike allowed_domains,
// deny_paths carries no count cap (see maxPushRulesPathBytes' own doc comment
// for why — a clamp-merged deny_paths can legitimately exceed what either
// side authored on its own, and a member must never be refused for a bound
// their own policy never violated).
func TestValidatePolicySpec_PushRulesBounds(t *testing.T) {
	spec := func(pr *types.PushRulesSpec) types.RunPolicySpec {
		return types.RunPolicySpec{MinConfinementClass: types.CC2, PushRules: pr}
	}

	refused := []struct {
		name         string
		spec         types.RunPolicySpec
		wantContains string
	}{
		{"empty deny_paths entry", spec(&types.PushRulesSpec{DenyPaths: []string{""}}),
			"push_rules.deny_paths[0]: empty entry"},
		{"deny_paths entry too long", spec(&types.PushRulesSpec{DenyPaths: []string{strings.Repeat("a", maxPushRulesPathBytes+1)}}),
			"push_rules.deny_paths[0]: exceeds"},
		{"deny_paths entry carries a NUL", spec(&types.PushRulesSpec{DenyPaths: []string{"deploy/\x00secret"}}),
			"push_rules.deny_paths[0]: control character"},
		{"deny_paths entry carries a control character", spec(&types.PushRulesSpec{DenyPaths: []string{"deploy/\x01"}}),
			"push_rules.deny_paths[0]: control character"},
		// Each of these would match nothing, and a deny rule that silently
		// matches nothing reads as enforcement that is not there.
		{"deny_paths entry with a leading ./", spec(&types.PushRulesSpec{DenyPaths: []string{"./infra/**"}}),
			"push_rules.deny_paths[0]: \"./infra/**\" has an empty"},
		{"deny_paths entry with an empty segment", spec(&types.PushRulesSpec{DenyPaths: []string{"infra//**"}}),
			"push_rules.deny_paths[0]: \"infra//**\" has an empty"},
		{"deny_paths entry with a .. segment", spec(&types.PushRulesSpec{DenyPaths: []string{"ok/**", "infra/../x"}}),
			"push_rules.deny_paths[1]: \"infra/../x\" has an empty"},
		{"deny_paths entry that is only a separator", spec(&types.PushRulesSpec{DenyPaths: []string{"/"}}),
			"push_rules.deny_paths[0]: \"/\" has an empty"},
		// #271: refused by DenyPathSegments too, so the broker fails closed on
		// a policy that bypassed this door.
		{"deny_paths entry with leading whitespace", spec(&types.PushRulesSpec{DenyPaths: []string{" infra/**"}}),
			"push_rules.deny_paths[0]: \" infra/**\" has leading or trailing whitespace"},
		{"deny_paths entry with trailing whitespace", spec(&types.PushRulesSpec{DenyPaths: []string{"infra/** "}}),
			"push_rules.deny_paths[0]: \"infra/** \" has leading or trailing whitespace"},
		{"deny_paths entry that is not valid UTF-8", spec(&types.PushRulesSpec{DenyPaths: []string{"deploy/\xff\xfe"}}),
			"push_rules.deny_paths[0]: \"deploy/\\xff\\xfe\" is not valid UTF-8"},
		{"max_inspect_pack_mib negative", spec(&types.PushRulesSpec{MaxInspectPackMiB: -1}),
			"push_rules.max_inspect_pack_mib must be between"},
		{"max_inspect_pack_mib above the cap", spec(&types.PushRulesSpec{MaxInspectPackMiB: maxPushRulesInspectPackMiB + 1}),
			"push_rules.max_inspect_pack_mib must be between"},
		// require_review_paths shares deny_paths' language and limits.
		{"empty require_review_paths entry", spec(&types.PushRulesSpec{RequireReviewPaths: []string{""}}),
			"push_rules.require_review_paths[0]: empty entry"},
		{"require_review_paths entry too long", spec(&types.PushRulesSpec{RequireReviewPaths: []string{strings.Repeat("a", maxPushRulesPathBytes+1)}}),
			"push_rules.require_review_paths[0]: exceeds"},
		{"require_review_paths entry carries a control character", spec(&types.PushRulesSpec{RequireReviewPaths: []string{"ci/\x01"}}),
			"push_rules.require_review_paths[0]: control character"},
		{"require_review_paths entry with a .. segment", spec(&types.PushRulesSpec{RequireReviewPaths: []string{"ok/**", "a/../b"}}),
			"push_rules.require_review_paths[1]: \"a/../b\" has an empty"},
		{"hold_seconds negative", spec(&types.PushRulesSpec{HoldSeconds: -1}),
			"push_rules.hold_seconds must be between 0 and 600"},
		{"max_file_size_mib negative", spec(&types.PushRulesSpec{MaxFileSizeMiB: -1}),
			"push_rules.max_file_size_mib must be between 0 and 32"},
		{"max_file_size_mib above the cap", spec(&types.PushRulesSpec{MaxFileSizeMiB: maxPushRulesFileSizeMiB + 1}),
			"the most a pack can carry is 32 MiB"},
		{"deny_new_executables is not enforced yet", spec(&types.PushRulesSpec{DenyNewExecutables: true}),
			"push_rules.deny_new_executables is not supported yet"},
		{"hold_seconds above the proxy's hold ceiling", spec(&types.PushRulesSpec{HoldSeconds: maxPushRulesHoldSeconds + 1}),
			"push_rules.hold_seconds must be between 0 and 600"},
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
		spec(&types.PushRulesSpec{DenyPaths: []string{"infra/", "/infra/**"}}), // trailing and leading separators read, not refused
		spec(&types.PushRulesSpec{MaxInspectPackMiB: maxPushRulesInspectPackMiB}),
		spec(&types.PushRulesSpec{MaxFileSizeMiB: maxPushRulesFileSizeMiB}),
		spec(&types.PushRulesSpec{RequireReviewPaths: []string{".github/workflows/**"}, HoldSeconds: maxPushRulesHoldSeconds}),
		spec(&types.PushRulesSpec{DenyPaths: []string{"secrets/**"}, RequireReviewPaths: []string{"infra/"}}),
	} {
		if err := validatePolicySpec(ok); err != nil {
			t.Errorf("rejected a policy inside the bounds %+v: %v", ok, err)
		}
	}

	// A deny_paths list well past any per-list count an operator ceiling or a
	// member's own proposal would author alone (e.g. a 100-entry
	// composer.Clamp union of a 64-entry ceiling and a 64-entry proposal) must
	// still validate — the decision this test pins.
	manyPaths := make([]string, 100)
	for i := range manyPaths {
		manyPaths[i] = fmt.Sprintf("path-%d/**", i)
	}
	if err := validatePolicySpec(spec(&types.PushRulesSpec{DenyPaths: manyPaths})); err != nil {
		t.Errorf("rejected a 100-entry deny_paths (no count cap by design): %v", err)
	}
}

// gitpack refuses every object over 32 MiB, so a larger max_file_size_mib never took effect. A save
// above it is refused at the route with a sentence an operator can act on.
func TestCreatePolicyRefusesMaxFileSizeAboveWhatAPackCarries(t *testing.T) {
	h := newHarness(t)
	w := do(t, h.srv, http.MethodPost, "/api/v1/policies", adminToken,
		`{"name":"p","spec":{"min_confinement_class":"CC2","push_rules":{"max_file_size_mib":33}}}`)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "the most a pack can carry is 32 MiB") {
		t.Fatalf("POST = %d %s, want 400 naming the 32 MiB limit", w.Code, w.Body.String())
	}
}

// A value saved when the bound was 1024 is lowered to 32 where it is read (the default policy file at
// boot, a stored policy at launch, a profile ceiling), with the same effect, instead of failing to load.
func TestStoredMaxFileSizeAboveTheBoundIsLoweredOnRead(t *testing.T) {
	t.Run("the default policy file at boot", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "policy.json")
		if err := os.WriteFile(path, []byte(`{"min_confinement_class":"CC2","push_rules":{"max_file_size_mib":500}}`), 0o600); err != nil {
			t.Fatal(err)
		}
		spec, err := LoadPolicySpec(path)
		if err != nil {
			t.Fatalf("LoadPolicySpec refused a pre-bound value: %v", err)
		}
		if got := spec.PushRules.MaxFileSizeMiB; got != maxPushRulesFileSizeMiB {
			t.Errorf("max_file_size_mib = %d, want %d", got, maxPushRulesFileSizeMiB)
		}
	})
	t.Run("a stored policy at launch", func(t *testing.T) {
		stored := types.RunPolicy{ID: uuid.New(), Name: "old", Spec: types.RunPolicySpec{
			MinConfinementClass: types.CC2, PushRules: &types.PushRulesSpec{MaxFileSizeMiB: 500, HoldSeconds: 5}}}
		h := newHarness(t)
		h.srv.cfg.Store = &policyListStore{capStore: &capStore{}, pols: []types.RunPolicy{stored}}
		spec, _, _, err := h.srv.resolvePolicy(context.Background(), &stored.ID, governanceCeiling{})
		if err != nil {
			t.Fatal(err)
		}
		if got := spec.PushRules.MaxFileSizeMiB; got != maxPushRulesFileSizeMiB || spec.PushRules.HoldSeconds != 5 {
			t.Errorf("push_rules = %+v, want max_file_size_mib %d and the rest kept", *spec.PushRules, maxPushRulesFileSizeMiB)
		}
		if stored.Spec.PushRules.MaxFileSizeMiB != 500 {
			t.Errorf("the stored row was edited in place: %d", stored.Spec.PushRules.MaxFileSizeMiB)
		}
		if err := validatePolicySpec(spec); err != nil {
			t.Errorf("the lowered spec does not validate: %v", err)
		}
	})
}

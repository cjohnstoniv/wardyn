// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// TestPushRules_NilIsByteIdenticalToTodaysWire is issue #176's own bar: a nil
// PushRules must be byte-identical to what a RunPolicySpec authored before
// this field existed produces on the wire. A representative, fully-populated
// spec (every OTHER field set, PushRules left nil) must marshal with no
// "push_rules" key at all — proved by marshalling it TWICE, once from a spec
// built with the field never mentioned and once with PushRules explicitly set
// to nil, and asserting the two are byte-for-byte the same JSON.
func TestPushRules_NilIsByteIdenticalToTodaysWire(t *testing.T) {
	build := func() RunPolicySpec {
		return RunPolicySpec{
			AllowedDomains:      []string{"api.anthropic.com", "github.com"},
			DeniedDomains:       []string{"evil.example"},
			FirstUseApproval:    FirstUseDenyWithReview,
			MinConfinementClass: CC2,
			EligibleGrants:      []GrantSpec{{Kind: GrantGitHubToken, RequiresApproval: true}},
			GitPushAnyBranch:    true,
		}
	}

	before := build() // as authored today, no knowledge of push_rules
	after := build()
	after.PushRules = nil // explicit, same as the zero value

	beforeJSON, err := json.Marshal(before)
	if err != nil {
		t.Fatalf("marshal before: %v", err)
	}
	afterJSON, err := json.Marshal(after)
	if err != nil {
		t.Fatalf("marshal after: %v", err)
	}
	if string(beforeJSON) != string(afterJSON) {
		t.Fatalf("nil push_rules changed the wire:\nbefore: %s\nafter:  %s", beforeJSON, afterJSON)
	}
	if strings.Contains(string(afterJSON), "push_rules") {
		t.Errorf("nil push_rules appeared on the wire: %s", afterJSON)
	}
}

// TestPushRulesSpecRoundTrip pins the wire shape once push_rules IS set.
func TestPushRulesSpecRoundTrip(t *testing.T) {
	in := RunPolicySpec{
		MinConfinementClass: CC2,
		PushRules: &PushRulesSpec{
			DenyPaths:         []string{".github/workflows/**", "infra/**"},
			MaxInspectPackMiB: 8,
		},
	}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(b), `"deny_paths":[".github/workflows/**","infra/**"]`) {
		t.Errorf("json = %s, want deny_paths present verbatim", b)
	}
	if !strings.Contains(string(b), `"max_inspect_pack_mib":8`) {
		t.Errorf("json = %s, want max_inspect_pack_mib present", b)
	}
	var out RunPolicySpec
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !reflect.DeepEqual(in, out) {
		t.Errorf("round-trip mismatch:\n got: %+v\nwant: %+v", out, in)
	}
}

// TestRunPolicySpecClone_DeepCopiesPushRules pins Clone's own documented
// discipline (RunPolicySpec.Clone's doc comment) for the new field: the
// clone must share NO backing array with the receiver, or a per-run mutation
// of DenyPaths would leak into every other run sharing the same stored/
// default policy.
func TestRunPolicySpecClone_DeepCopiesPushRules(t *testing.T) {
	orig := RunPolicySpec{
		MinConfinementClass: CC2,
		PushRules: &PushRulesSpec{DenyPaths: []string{".github/workflows/**"}, MaxInspectPackMiB: 4,
			RequireReviewPaths: []string{"infra/**"}},
	}
	clone := orig.Clone()
	if clone.PushRules == orig.PushRules {
		t.Fatal("Clone did not reallocate the PushRules pointer")
	}
	clone.PushRules.DenyPaths[0] = "mutated"
	clone.PushRules.MaxInspectPackMiB = 99
	if orig.PushRules.DenyPaths[0] != ".github/workflows/**" {
		t.Error("mutating the clone's DenyPaths leaked into the original — Clone aliased the backing array")
	}
	clone.PushRules.RequireReviewPaths[0] = "mutated"
	if orig.PushRules.RequireReviewPaths[0] != "infra/**" {
		t.Error("mutating the clone's RequireReviewPaths leaked into the original — Clone aliased the backing array")
	}
	if orig.PushRules.MaxInspectPackMiB != 4 {
		t.Error("mutating the clone's MaxInspectPackMiB leaked into the original")
	}

	// nil PushRules clones to nil, not an empty-but-present struct.
	var nilOrig RunPolicySpec
	if got := nilOrig.Clone().PushRules; got != nil {
		t.Errorf("Clone of a nil PushRules = %+v, want nil", got)
	}
}

// TestDenyPathSegments pins the one reading of a deny_paths entry that
// write-time validation and the broker's matcher share: a leading "/" is
// dropped, a trailing "/" means everything beneath the directory, and an
// empty, "." or ".." segment — which no git path contains, so the entry could
// never match — is refused.
func TestDenyPathSegments(t *testing.T) {
	for pattern, want := range map[string]string{
		"infra/**":  "infra|**",
		"/infra/**": "infra|**",
		"infra/":    "infra|**",
		"/infra/":   "infra|**",
		"infra":     "infra",
		"**/*.pem":  "**|*.pem",
		".github/":  ".github|**",
	} {
		got, err := DenyPathSegments(pattern)
		if err != nil || strings.Join(got, "|") != want {
			t.Errorf("DenyPathSegments(%q) = %q, %v; want %q", pattern, strings.Join(got, "|"), err, want)
		}
	}
	for _, pattern := range []string{"", "/", "//infra", "./infra/**", "infra//**", "infra/./x",
		"infra/../x", "..", ".", "infra//"} {
		if got, err := DenyPathSegments(pattern); err == nil {
			t.Errorf("DenyPathSegments(%q) = %q, want a refusal", pattern, got)
		}
	}
}

// TestPushRulesIsSetCountsReviewPaths: a spec carrying only review paths is a
// rule — the broker must buffer, inspect and advertise no-thin for it — while
// a hold with nothing to hold reads as absent.
func TestPushRulesIsSetCountsReviewPaths(t *testing.T) {
	if !(&PushRulesSpec{RequireReviewPaths: []string{"a/**"}}).IsSet() {
		t.Error("require_review_paths alone reads as no rule")
	}
	if (&PushRulesSpec{HoldSeconds: 60}).IsSet() {
		t.Error("hold_seconds alone reads as a rule, but there is nothing to hold")
	}
}

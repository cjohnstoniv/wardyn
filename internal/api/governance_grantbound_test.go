// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/composer"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// ghScopeJSON builds a github_token scope. Written here rather than reused from
// clamp_test.go's helper because that one lives in internal/composer.
func ghScopeJSON(t *testing.T, repos []string, perms map[string]string) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(map[string]any{"repos": repos, "permissions": perms})
	if err != nil {
		t.Fatalf("marshal github scope: %v", err)
	}
	return b
}

// apiKeyScope builds an api_key grant scope — the (host, header, secret_name)
// pairing storedSecretGrantPairing reads.
func apiKeyScope(t *testing.T, host, secret string) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(map[string]any{
		"host": host, "header": "Authorization", "secret_name": secret,
	})
	if err != nil {
		t.Fatalf("marshal api_key scope: %v", err)
	}
	return b
}

// TestGovernanceProfileGrantBound is the PF-22/PF-28 pin: a governance profile
// may NARROW the deployment's credential eligibility and may never MINT new
// eligibility.
//
// The escape it closes is not hypothetical. Whoever holds the profile-authoring
// surface would otherwise write a profile carrying an arbitrary grant pairing,
// assign it to themselves — their own ceiling IS their assigned profile, since
// the resolver's operator short-circuit keys on the admin tier they do not hold
// — and filterMemberGrants plus the dispatch injection would then deliver any
// operator-stored secret into their own sandbox, with self-authored egress to
// carry it out.
//
// Four refusal axes plus the acceptance case, because the comparator has to be
// MONOTONE rather than membership (which would let approval be stripped) or
// equality (which would refuse a legitimately stricter profile, i.e. the whole
// point of authoring one).
func TestGovernanceProfileGrantBound(t *testing.T) {
	const (
		corpHost   = "api.corp.example"
		corpSecret = "corp-api-key"
	)
	// The deployment ceiling: one approval-gated api_key pairing at a 300s TTL,
	// and a github_token scoped to one repo at read.
	deployment := []types.GrantSpec{
		{
			Kind:             types.GrantAPIKey,
			Scope:            apiKeyScope(t, corpHost, corpSecret),
			TTLSeconds:       300,
			RequiresApproval: true,
		},
		{
			Kind:  types.GrantGitHubToken,
			Scope: ghScopeJSON(t, []string{"octocat/Hello-World"}, map[string]string{"contents": "read"}),
		},
	}

	cases := []struct {
		name    string
		profile []types.GrantSpec
		// wantErr is a substring the refusal must name, or "" for accept.
		wantErr string
	}{
		{
			name:    "empty profile grants are trivially within the ceiling",
			profile: nil,
		},
		{
			// The whole reason the bound is not equality: a profile whose job
			// is to be STRICTER than the deployment must be writable.
			name: "stricter profile accepted: approval forced on, shorter TTL",
			profile: []types.GrantSpec{{
				Kind:             types.GrantAPIKey,
				Scope:            apiKeyScope(t, corpHost, corpSecret),
				TTLSeconds:       60,
				RequiresApproval: true,
			}},
		},
		{
			name: "pairing missing: a secret the deployment never paired with this host",
			profile: []types.GrantSpec{{
				Kind:             types.GrantAPIKey,
				Scope:            apiKeyScope(t, corpHost, "some-other-secret"),
				TTLSeconds:       60,
				RequiresApproval: true,
			}},
			wantErr: "not in the deployment ceiling",
		},
		{
			name: "kind missing: a grant kind the deployment does not provision at all",
			profile: []types.GrantSpec{{
				Kind:  types.GrantGitPAT,
				Scope: json.RawMessage(`{"host":"ghe.corp.example","secret_name":"corp-pat"}`),
			}},
			wantErr: "not in the deployment ceiling",
		},
		{
			// A stripped approval flag auto-mints the injection at proxy boot
			// with no human in the loop — the single most valuable axis here.
			name: "approval stripped: the deployment requires it, the profile does not",
			profile: []types.GrantSpec{{
				Kind:             types.GrantAPIKey,
				Scope:            apiKeyScope(t, corpHost, corpSecret),
				TTLSeconds:       60,
				RequiresApproval: false,
			}},
			wantErr: "strips requires_approval",
		},
		{
			name: "TTL raised above the ceiling's",
			profile: []types.GrantSpec{{
				Kind:             types.GrantAPIKey,
				Scope:            apiKeyScope(t, corpHost, corpSecret),
				TTLSeconds:       900,
				RequiresApproval: true,
			}},
			wantErr: "ttl_seconds resolves to 900s, above the deployment ceiling's 300s",
		},
		{
			// PF-28. A raw `profile.TTL <= ceiling.TTL` ACCEPTS this: 0 < 300.
			// But 0 means "the 3600s default", so the "narrower" profile mints
			// credentials that live twelve times as long as the ceiling's.
			// Both sides normalize through clampGrants' own rule first.
			name: "TTL 0 normalizes to the 3600s default, not to 'unset'",
			profile: []types.GrantSpec{{
				Kind:             types.GrantAPIKey,
				Scope:            apiKeyScope(t, corpHost, corpSecret),
				TTLSeconds:       0,
				RequiresApproval: true,
			}},
			wantErr: "ttl_seconds resolves to 3600s, above the deployment ceiling's 300s",
		},
		{
			// Pairing checks CANNOT see this: storedSecretGrantPairing reports
			// covered=false for github_token (it names no stored secret), so
			// without the scope leg a profile widens the repo set for free.
			name: "github repo widened beyond the ceiling's repo set",
			profile: []types.GrantSpec{{
				Kind:  types.GrantGitHubToken,
				Scope: ghScopeJSON(t, []string{"octocat/Hello-World", "corp/secrets"}, map[string]string{"contents": "read"}),
			}},
			wantErr: `github repo "corp/secrets" is outside`,
		},
		{
			name: "github permission raised above the ceiling's level",
			profile: []types.GrantSpec{{
				Kind:  types.GrantGitHubToken,
				Scope: ghScopeJSON(t, []string{"octocat/Hello-World"}, map[string]string{"contents": "write"}),
			}},
			wantErr: `at "write" is above the deployment ceiling's "read"`,
		},
		{
			name: "github permission the ceiling does not carry at all",
			profile: []types.GrantSpec{{
				Kind:  types.GrantGitHubToken,
				Scope: ghScopeJSON(t, []string{"octocat/Hello-World"}, map[string]string{"administration": "read"}),
			}},
			wantErr: `github permission "administration" is not in the deployment ceiling`,
		},
		{
			// A narrower github scope is exactly what a profile is for.
			name: "github scope narrowed: fewer repos, same permission level",
			profile: []types.GrantSpec{{
				Kind:  types.GrantGitHubToken,
				Scope: ghScopeJSON(t, nil, map[string]string{"contents": "read"}),
			}},
		},
		{
			// Fail closed: an unreadable pairing means nothing can be said
			// about whether it is in the ceiling, so it is not a silent pass.
			name: "undecodable scope is refused, never waved through",
			profile: []types.GrantSpec{{
				Kind:  types.GrantAPIKey,
				Scope: json.RawMessage(`{"host":`),
			}},
			wantErr: "invalid scope",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			err := governanceGrantsWithinCeiling(tc.profile, deployment)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("governanceGrantsWithinCeiling = %v, want nil (a stricter profile must be writable)", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("governanceGrantsWithinCeiling = nil, want a refusal naming %q", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("refusal = %q, want it to name %q", err.Error(), tc.wantErr)
			}
		})
	}
}

// TestGovernanceGrantBoundDominationIsPerGrant pins the one thing an axis-by-
// axis check gets wrong: a profile grant must be dominated by ONE ceiling grant
// on EVERY axis. Given two same-kind ceiling grants, a profile must not be able
// to take its pairing from the first and its TTL headroom from the second.
func TestGovernanceGrantBoundDominationIsPerGrant(t *testing.T) {
	const hostA, hostB = "a.corp.example", "b.corp.example"
	deployment := []types.GrantSpec{
		// Pairing A: tight TTL.
		{Kind: types.GrantAPIKey, Scope: apiKeyScope(t, hostA, "secret-a"), TTLSeconds: 60},
		// Pairing B: generous TTL, different pairing entirely.
		{Kind: types.GrantAPIKey, Scope: apiKeyScope(t, hostB, "secret-b"), TTLSeconds: 3600},
	}
	profile := []types.GrantSpec{
		{Kind: types.GrantAPIKey, Scope: apiKeyScope(t, hostA, "secret-a"), TTLSeconds: 3600},
	}
	err := governanceGrantsWithinCeiling(profile, deployment)
	if err == nil {
		t.Fatal("pairing A at pairing B's TTL was accepted; the comparator is matching axes " +
			"against DIFFERENT ceiling grants instead of requiring one that dominates on all of them")
	}
	if !strings.Contains(err.Error(), "ttl_seconds resolves to 3600s, above the deployment ceiling's 60s") {
		t.Errorf("refusal = %q, want it to name pairing A's own 60s ceiling", err.Error())
	}
}

// TestGovernanceOmissionWarnings pins the advisory half of a profile write: a
// profile REPLACES the deployment ceiling rather than composing with it, so it
// narrows and widens BY OMISSION, silently. The write response says what was
// omitted — and says it as a WARNING, never a refusal, because both directions
// are legitimate and refusing either would make DefaultPolicy a floor this
// feature deliberately does not have.
func TestGovernanceOmissionWarnings(t *testing.T) {
	deployment := types.RunPolicySpec{
		AllowedDomains:      []string{"pypi.org", "files.pythonhosted.org"},
		DeniedDomains:       []string{"internal.corp.example"},
		MinConfinementClass: types.CC2,
		EligibleGrants:      []types.GrantSpec{{Kind: types.GrantAPIKey}},
	}

	t.Run("a faithful copy warns about nothing", func(t *testing.T) {
		if got := governanceOmissionWarnings(deployment, deployment); len(got) != 0 {
			t.Errorf("warnings = %v, want none for a ceiling identical to the deployment default", got)
		}
	})

	t.Run("omissions and widenings are each named", func(t *testing.T) {
		profile := types.RunPolicySpec{
			AllowedDomains:      []string{"pypi.org"}, // drops one allowed host
			DeniedDomains:       nil,                  // drops the wall entirely
			AllowAllEgress:      true,                 // widens
			MinConfinementClass: types.CC1,            // weaker than the deployment's
			EligibleGrants:      []types.GrantSpec{},  // drops the api_key lane
		}
		got := governanceOmissionWarnings(profile, deployment)
		for _, want := range []string{
			"internal.corp.example",      // the dropped deny
			"files.pythonhosted.org",     // the dropped allow
			"api_key",                    // the dropped grant lane
			"WEAKER than the deployment", // the confinement downgrade
			"allow_all_egress",           // the egress widening
		} {
			found := false
			for _, w := range got {
				if strings.Contains(w, want) {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("warnings %v do not name %q", got, want)
			}
		}
	})

	t.Run("a stricter profile does not warn about being stricter", func(t *testing.T) {
		profile := deployment
		profile.DeniedDomains = []string{"internal.corp.example", "also.blocked.example"}
		profile.MinConfinementClass = types.CC3
		if got := governanceOmissionWarnings(profile, deployment); len(got) != 0 {
			t.Errorf("warnings = %v, want none: adding a deny and raising confinement omits nothing", got)
		}
	})
}

// TestClampAndComparatorAreOneRule is the cross-package pin: the WRITE-TIME
// comparator (governanceGrantsWithinCeiling, here) and the RUNTIME clamp
// (composer.Clamp) must answer the same question about the same input.
//
// They did not. The comparator searched per-grant — "some SINGLE ceiling grant
// dominates on EVERY axis" — while the clamp indexed the ceiling by KIND alone
// and let the last same-kind entry supply the approval posture and TTL. So the
// exact fixture TestGovernanceGrantBoundDominationIsPerGrant refuses (pairing A
// asking for pairing B's TTL headroom) sailed through composer.Clamp untouched
// at 3600s. Both now consult composer's ceilingGrantsBounding/PairingInCeiling,
// so the disagreement is not merely fixed but unrepresentable.
//
// The two do NOT have identical OUTPUTS, and should not: the comparator REFUSES
// a profile grant that exceeds its ceiling (a profile is authored once and must
// be readable as what it permits), while the clamp NARROWS a run's grant to the
// bound (a run is bounded, not rejected, so a member's task still launches).
// What must agree is the BOUND each applies — which is what this asserts.
func TestClampAndComparatorAreOneRule(t *testing.T) {
	const hostA, hostB = "a.corp.example", "b.corp.example"
	// The SAME fixture TestGovernanceGrantBoundDominationIsPerGrant uses.
	deployment := []types.GrantSpec{
		{Kind: types.GrantAPIKey, Scope: apiKeyScope(t, hostA, "secret-a"), TTLSeconds: 60},
		{Kind: types.GrantAPIKey, Scope: apiKeyScope(t, hostB, "secret-b"), TTLSeconds: 3600},
	}
	proposal := types.GrantSpec{Kind: types.GrantAPIKey, Scope: apiKeyScope(t, hostA, "secret-a"), TTLSeconds: 3600}

	// The comparator refuses it, naming pairing A's own 60s ceiling.
	if err := governanceGrantsWithinCeiling([]types.GrantSpec{proposal}, deployment); err == nil {
		t.Fatal("the comparator accepted pairing A at pairing B's TTL — this fixture no longer exercises the disagreement")
	}
	// The clamp must bound it to the same 60s rather than pass it at 3600.
	clamped, warns := composer.Clamp(
		types.RunPolicySpec{EligibleGrants: []types.GrantSpec{proposal}},
		types.RunPolicySpec{EligibleGrants: deployment},
	)
	if len(clamped.EligibleGrants) != 1 {
		t.Fatalf("clamp dropped the grant (warns=%q); it should bound it, not refuse it", warns)
	}
	if got := clamped.EligibleGrants[0].TTLSeconds; got != 60 {
		t.Errorf("composer.Clamp left ttl_seconds=%d for a pairing whose own ceiling entry caps it at 60 — "+
			"the clamp and the comparator disagree about the same input", got)
	}
}

// TestStoredSecretPairingMatchesComposer is the drift guard for the seam this
// consolidation leaves behind. storedSecretGrantPairing decodes a grant for
// DELIVERY (it fails a malformed scope closed, and validates an env_secret's
// variable name because that name is written into a process environment);
// composer.GrantPairing decodes it for MATCHING. Two decoders of one wire shape
// is exactly the arrangement that produced this finding, so pin that they read
// the same pairing out of every kind.
func TestStoredSecretPairingMatchesComposer(t *testing.T) {
	for _, g := range []types.GrantSpec{
		{Kind: types.GrantAPIKey, Scope: apiKeyScope(t, "a.corp.example", "secret-a")},
		{Kind: types.GrantGitPAT, Scope: mustJSON(map[string]any{"host": "gitlab.corp.io", "secret_name": "pat"})},
		{Kind: types.GrantSSHKey, Scope: mustJSON(map[string]any{"host": "github.com", "key_secret_ref": "k", "known_hosts_secret_ref": "kh"})},
		{Kind: types.GrantEnvSecret, Scope: mustJSON(map[string]any{"name": "CORP_TOKEN", "secret_name": "corp"})},
		{Kind: types.GrantGitHubToken, Scope: ghScopeJSON(t, []string{"o/r"}, nil)},
		{Kind: types.GrantCloudSTS, Scope: mustJSON(map[string]any{})},
	} {
		wantHost, wantSecret, wantKH, wantCovered, err := storedSecretGrantPairing(g)
		if err != nil {
			t.Fatalf("%s: storedSecretGrantPairing: %v", g.Kind, err)
		}
		gotHost, gotSecret, gotKH, gotCovered, ok := composer.GrantPairing(g)
		if !ok {
			t.Errorf("%s: composer.GrantPairing could not decode a scope the delivery decoder accepted", g.Kind)
			continue
		}
		if gotCovered != wantCovered || gotHost != wantHost || gotSecret != wantSecret || gotKH != wantKH {
			t.Errorf("%s: composer.GrantPairing = (%q,%q,%q,covered=%v), storedSecretGrantPairing = (%q,%q,%q,covered=%v) — the two decoders have drifted",
				g.Kind, gotHost, gotSecret, gotKH, gotCovered, wantHost, wantSecret, wantKH, wantCovered)
		}
	}
}

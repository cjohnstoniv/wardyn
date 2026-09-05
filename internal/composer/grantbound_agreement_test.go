// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package composer

import (
	"encoding/json"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The runtime half of F014's SECOND round. Round 1 replaced clampGrants' kind-keyed
// map with a pairing search, which closed the original widening but left the clamp
// and the write-time comparator (internal/api's governanceGrantWithinCeiling) still
// disagreeing in two executed ways — both pinned here, and both a consequence of the
// clamp MEETING every candidate ceiling grant where the comparator asks whether ONE
// of them dominates:
//
//   - github_token. It names no stored secret, so every same-kind ceiling entry was a
//     candidate and their repo allowlists were intersected in sequence. A ceiling
//     carving two repo sets (the only way to give two repo sets different permission
//     levels, since one scope carries one permissions map) therefore emptied a
//     proposal that named ONE of them — while the comparator accepted it, because that
//     entry dominates. Narrowing to nothing is not a widening, but it IS the two rules
//     giving one input two answers, which is what F014 is about.
//   - The same pairing written twice with different bounds. Round 1's search returned
//     the FIRST match and stopped, so the identical ceiling SET clamped to (approval
//     true, ttl 300) in one slice order and (approval false, ttl 3600) in the other.
//
// The rule that closes both: among the ceiling grants whose IDENTITY covers the
// proposal (same kind, and for the stored-secret kinds the same pairing), if ONE
// dominates the proposal on every remaining axis then the clamp bounds against THAT
// grant alone — which is exactly the question the comparator asks, so the two now
// answer alike. Only when none dominates does the clamp fall back to the meet, which
// can merely narrow and is order-independent either way.

func githubGrant(t *testing.T, repos []string, perms map[string]string, approval bool, ttl int) types.GrantSpec {
	t.Helper()
	return types.GrantSpec{
		Kind:             types.GrantGitHubToken,
		Scope:            mustJSON(t, map[string]any{"repos": repos, "permissions": perms}),
		RequiresApproval: approval,
		TTLSeconds:       ttl,
	}
}

func apiKeyGrant(t *testing.T, host, secret string, approval bool, ttl int) types.GrantSpec {
	t.Helper()
	return types.GrantSpec{
		Kind:             types.GrantAPIKey,
		Scope:            mustJSON(t, map[string]any{"host": host, "secret_name": secret}),
		RequiresApproval: approval,
		TTLSeconds:       ttl,
	}
}

func clampOne(t *testing.T, g types.GrantSpec, ceiling ...types.GrantSpec) (types.GrantSpec, []string) {
	t.Helper()
	var warns []string
	out := clampGrants([]types.GrantSpec{g}, types.RunPolicySpec{EligibleGrants: ceiling}, &warns)
	if len(out) != 1 {
		t.Fatalf("proposal was dropped (warns=%q); these cases are about the BOUND, so it must survive", warns)
	}
	return out[0], warns
}

func githubRepos(t *testing.T, g types.GrantSpec) []string {
	t.Helper()
	var sc struct {
		Repos []string `json:"repos"`
	}
	if err := json.Unmarshal(g.Scope, &sc); err != nil {
		t.Fatalf("decode clamped github scope %s: %v", g.Scope, err)
	}
	return sc.Repos
}

// TestClampGitHubBoundsByTheEntryThatCoversIt is RESIDUAL 1: a ceiling carving two
// repo sets must not empty a proposal that names one of them.
func TestClampGitHubBoundsByTheEntryThatCoversIt(t *testing.T) {
	alpha := githubGrant(t, []string{"org/alpha"}, map[string]string{"contents": "write"}, false, 3600)
	beta := githubGrant(t, []string{"org/beta"}, map[string]string{"contents": "write"}, false, 3600)
	proposal := githubGrant(t, []string{"org/beta"}, map[string]string{"contents": "read"}, false, 3600)

	for _, c := range []struct {
		name    string
		ceiling []types.GrantSpec
	}{
		{"[alpha,beta]", []types.GrantSpec{alpha, beta}},
		{"[beta,alpha]", []types.GrantSpec{beta, alpha}},
	} {
		got, warns := clampOne(t, proposal, c.ceiling...)
		repos := githubRepos(t, got)
		if len(repos) != 1 || repos[0] != "org/beta" {
			t.Errorf("%s: clamped repos = %v, want [org/beta] — the ceiling entry for org/beta covers this proposal, "+
				"so intersecting it against org/alpha's entry as well removes access the operator granted "+
				"(warns=%q)", c.name, repos, warns)
		}
	}
}

// TestClampDuplicatePairingIsOrderIndependent is RESIDUAL 2: one pairing written
// twice with different bounds is still a SET, so slice order cannot move the answer.
func TestClampDuplicatePairingIsOrderIndependent(t *testing.T) {
	strict := apiKeyGrant(t, "vendor.example", "vendor_key", true, 300)
	loose := apiKeyGrant(t, "vendor.example", "vendor_key", false, 3600)
	proposal := apiKeyGrant(t, "vendor.example", "vendor_key", false, 3600)

	forward, _ := clampOne(t, proposal, strict, loose)
	reversed, _ := clampOne(t, proposal, loose, strict)
	if forward.RequiresApproval != reversed.RequiresApproval || forward.TTLSeconds != reversed.TTLSeconds {
		t.Errorf("order dependence on a ceiling naming one pairing twice: [strict,loose] -> (approval=%v ttl=%d), "+
			"[loose,strict] -> (approval=%v ttl=%d)",
			forward.RequiresApproval, forward.TTLSeconds, reversed.RequiresApproval, reversed.TTLSeconds)
	}
}

// TestClampNegativeTTLResolvesToTheCap pins the third divergence, the one
// internal/api/governance_grantbound.go:50-54 wrote down rather than fixed: the
// comparator resolves a NEGATIVE ttl_seconds to the broker maximum (so a ceiling of
// 300 refuses it), while the clamp tested only `== 0` and passed the negative
// through untouched. A negative TTL that survives the clamp is a grant the
// comparator would refuse.
func TestClampNegativeTTLResolvesToTheCap(t *testing.T) {
	ceiling := apiKeyGrant(t, "vendor.example", "vendor_key", false, 300)
	got, _ := clampOne(t, apiKeyGrant(t, "vendor.example", "vendor_key", false, -1), ceiling)
	if got.TTLSeconds != 300 {
		t.Errorf("clamped ttl_seconds = %d, want 300 — a non-positive TTL means the broker maximum, "+
			"which this ceiling caps at 300", got.TTLSeconds)
	}
}

// TestCeilingGrantsCoveringIsTheIdentityAxis pins the exported search the write-time
// comparator is meant to adopt (internal/api/governance_grantbound.go), so the two
// sites select from ONE definition instead of two that have already drifted twice.
func TestCeilingGrantsCoveringIsTheIdentityAxis(t *testing.T) {
	corp := sshKeyGrant(t, "git.corp.example", "corp_key", true, 300)
	public := sshKeyGrant(t, "git.public.example", "public_key", false, 3600)
	ceiling := []types.GrantSpec{corp, public}

	if got := CeilingGrantsCovering(sshKeyGrant(t, "GIT.CORP.EXAMPLE.", "corp_key", false, 3600), ceiling); len(got) != 1 {
		t.Errorf("covering(corp pairing) returned %d grants, want exactly the corp entry", len(got))
	}
	if got := CeilingGrantsCovering(sshKeyGrant(t, "git.elsewhere.example", "other_key", false, 60), ceiling); len(got) != 0 {
		t.Errorf("covering(pairing in no ceiling entry) returned %d grants, want none — "+
			"the identity axis is the pairing, not the kind", len(got))
	}
	// A kind that names no stored secret has same-kind membership for an identity.
	gh := []types.GrantSpec{githubGrant(t, []string{"org/alpha"}, nil, false, 3600), githubGrant(t, []string{"org/beta"}, nil, false, 3600)}
	if got := CeilingGrantsCovering(githubGrant(t, []string{"org/beta"}, nil, false, 3600), gh); len(got) != 2 {
		t.Errorf("covering(github_token) returned %d grants, want both same-kind entries", len(got))
	}
	// An undecodable proposal scope names no pairing, so it is covered by nothing.
	junk := types.GrantSpec{Kind: types.GrantSSHKey, Scope: json.RawMessage(`{"nope":1}`)}
	if got := CeilingGrantsCovering(junk, ceiling); len(got) != 0 {
		t.Errorf("covering(undecodable scope) returned %d grants, want none (fail closed)", len(got))
	}
}

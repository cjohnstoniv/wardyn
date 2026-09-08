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
// normalizeGrantTTLSeconds (internal/api/governance_grantbound.go) wrote down in
// its own doc comment rather than fixed: the comparator resolves a NEGATIVE
// ttl_seconds to the broker maximum (so a ceiling of 300 refuses it), while the
// clamp tested only `== 0` and passed the negative through untouched. A negative
// TTL that survives the clamp is a grant the comparator would refuse.
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

// ─── THE TWO DOMINATION AXES A MUTATION COULD DELETE UNSEEN ──────────────────
//
// grantDominatedBy asks three questions, and clampGrants' whole shape turns on
// the answer: when ONE covering ceiling grant dominates the proposal the clamp
// bounds against THAT grant alone, and otherwise it MEETS every candidate. So a
// domination axis that stops being asked does not fail loudly — it moves a
// proposal from the meet to a single wider entry, and the run gets MORE than the
// operator's ceiling wrote.
//
// Two of the three axes were pinned by nothing. Deleting grantDominatedBy's
// approval guard, or relaxing normalizeClampTTL's `<= 0` to `== 0`, left the
// whole internal/api + internal/composer suite at exit 0 while each measurably
// widened a github_token proposal's repo set. The cases below are the shapes
// that separate the meet from the shortcut; the sibling axes (the scope axis,
// and the shortcut itself) already had theirs.
//
// WHY GITHUB_TOKEN CARRIES BOTH. It names no stored secret, so every same-kind
// ceiling entry covers the proposal and the meet has something to intersect —
// which makes "did the clamp meet, or take the shortcut?" observable in the repo
// list. On a paired kind the two answers coincide and the mutation hides.

// TestClampApprovalAxisKeepsTheProposalOnTheMeet pins grantDominatedBy's FIRST
// question: a ceiling grant that REQUIRES approval does not dominate a proposal
// that does not.
//
// The ceiling carves two entries: a wide one (both repos, 3600s) that requires
// approval, and a narrow one (one repo, 300s) that does not. Neither dominates a
// proposal for both repos at 600s without approval — the wide one because of the
// approval axis, the narrow one because of the TTL — so the clamp MEETS them:
// repos intersect to the narrow set, the TTL cap is the minimum, and approval is
// forced on because one of them requires it.
//
// Drop the approval guard and the wide entry dominates, the clamp bounds against
// it ALONE, and the run keeps both repos at 600s. That is the operator's
// approval-gated breadth handed over on a proposal that declined the gate.
func TestClampApprovalAxisKeepsTheProposalOnTheMeet(t *testing.T) {
	wideNeedsApproval := githubGrant(t, []string{"org/alpha", "org/beta"}, map[string]string{"contents": "read"}, true, 3600)
	narrowNoApproval := githubGrant(t, []string{"org/alpha"}, map[string]string{"contents": "read"}, false, 300)
	proposal := githubGrant(t, []string{"org/alpha", "org/beta"}, map[string]string{"contents": "read"}, false, 600)

	for _, c := range []struct {
		name    string
		ceiling []types.GrantSpec
	}{
		{"[wide,narrow]", []types.GrantSpec{wideNeedsApproval, narrowNoApproval}},
		{"[narrow,wide]", []types.GrantSpec{narrowNoApproval, wideNeedsApproval}},
	} {
		got, warns := clampOne(t, proposal, c.ceiling...)
		repos := githubRepos(t, got)
		if len(repos) != 1 || repos[0] != "org/alpha" {
			t.Errorf("%s: clamped repos = %v, want [org/alpha]. The only ceiling entry carrying org/beta requires "+
				"approval, and this proposal declined it, so that entry cannot dominate — the clamp must meet both "+
				"entries. Handing back org/beta is the run receiving breadth the operator gated behind an approval "+
				"(warns=%q)", c.name, repos, warns)
		}
		if got.TTLSeconds != 300 {
			t.Errorf("%s: clamped ttl_seconds = %d, want 300 — the meet takes the strictest cap, and 600 is the "+
				"approval-requiring entry's allowance", c.name, got.TTLSeconds)
		}
		if !got.RequiresApproval {
			t.Errorf("%s: clamped requires_approval = false; one bounding entry requires it, and the operator can "+
				"only ever tighten", c.name)
		}
	}
}

// TestClampNegativeTTLIsNotDominatedByAShorterCeiling pins normalizeClampTTL's
// SECOND reading — the one that lives inside grantDominatedBy rather than in the
// output cap.
//
// A negative ttl_seconds means the broker maximum, exactly as
// internal/api's normalizeGrantTTLSeconds reads it. So a proposal at -1 asks for
// 3600 and is dominated by NEITHER a 300s nor a 600s ceiling entry, and the
// clamp meets them: repos intersect down to the narrow set.
//
// Relax the fold to `== 0` and -1 stays -1, which is below every ceiling TTL, so
// the wide entry dominates and the clamp bounds against it alone — the proposal
// keeps both repos. TestClampNegativeTTLResolvesToTheCap cannot see this: it
// asserts the OUTPUT TTL, which the `g.TTLSeconds <= 0` cap fixes at 300 either
// way. The widening is in the repo list, and only a ceiling with two entries has
// one.
func TestClampNegativeTTLIsNotDominatedByAShorterCeiling(t *testing.T) {
	wideShortTTL := githubGrant(t, []string{"org/alpha", "org/beta"}, map[string]string{"contents": "read"}, false, 300)
	narrowLongTTL := githubGrant(t, []string{"org/alpha"}, map[string]string{"contents": "read"}, false, 600)
	proposal := githubGrant(t, []string{"org/alpha", "org/beta"}, map[string]string{"contents": "read"}, false, -1)

	for _, c := range []struct {
		name    string
		ceiling []types.GrantSpec
	}{
		{"[wide,narrow]", []types.GrantSpec{wideShortTTL, narrowLongTTL}},
		{"[narrow,wide]", []types.GrantSpec{narrowLongTTL, wideShortTTL}},
	} {
		got, warns := clampOne(t, proposal, c.ceiling...)
		repos := githubRepos(t, got)
		if len(repos) != 1 || repos[0] != "org/alpha" {
			t.Errorf("%s: clamped repos = %v, want [org/alpha]. ttl_seconds=-1 resolves to the broker maximum, which "+
				"exceeds BOTH ceiling entries, so neither dominates and the clamp must meet them. Reading -1 as a "+
				"short TTL makes the widest entry dominate and hands the run its repo set (warns=%q)",
				c.name, repos, warns)
		}
		if got.TTLSeconds != 300 {
			t.Errorf("%s: clamped ttl_seconds = %d, want 300 (the strictest cap)", c.name, got.TTLSeconds)
		}
	}
}

// TestNormalizeClampTTLReadsEveryNonPositiveAsTheMaximum states the rule the two
// cases above depend on, directly, so a change to it is a change to a documented
// contract and not a silent re-partitioning of which grants dominate which.
// internal/api's normalizeGrantTTLSeconds gives the identical reading; that is
// what makes the clamp and the write-time comparator comparable at all.
func TestNormalizeClampTTLReadsEveryNonPositiveAsTheMaximum(t *testing.T) {
	for _, ttl := range []int{0, -1, -300, maxGrantTTLSeconds + 1} {
		if got := normalizeClampTTL(ttl); got != maxGrantTTLSeconds {
			t.Errorf("normalizeClampTTL(%d) = %d, want %d — a value the comparator resolves to the broker maximum "+
				"must resolve to it here too, or a proposal one side refuses is dominated on the other", ttl, got, maxGrantTTLSeconds)
		}
	}
	for _, ttl := range []int{1, 300, maxGrantTTLSeconds} {
		if got := normalizeClampTTL(ttl); got != ttl {
			t.Errorf("normalizeClampTTL(%d) = %d, want it unchanged", ttl, got)
		}
	}
}

// TestClampApprovalAxisThroughTheExportedClamp runs the approval shape through
// Clamp itself, the entry point boundMemberSpec calls, so the axis is pinned at
// the boundary a caller actually reaches and not only at the helper.
func TestClampApprovalAxisThroughTheExportedClamp(t *testing.T) {
	ceiling := types.RunPolicySpec{EligibleGrants: []types.GrantSpec{
		githubGrant(t, []string{"org/alpha", "org/beta"}, map[string]string{"contents": "read"}, true, 3600),
		githubGrant(t, []string{"org/alpha"}, map[string]string{"contents": "read"}, false, 300),
	}}
	proposed := types.RunPolicySpec{EligibleGrants: []types.GrantSpec{
		githubGrant(t, []string{"org/alpha", "org/beta"}, map[string]string{"contents": "read"}, false, 600),
	}}

	out, warns := Clamp(proposed, ceiling)
	if len(out.EligibleGrants) != 1 {
		t.Fatalf("Clamp returned %d grants, want 1 (warns=%q)", len(out.EligibleGrants), warns)
	}
	if repos := githubRepos(t, out.EligibleGrants[0]); len(repos) != 1 || repos[0] != "org/alpha" {
		t.Errorf("Clamp handed the run repos %v, want [org/alpha] — org/beta is only in the entry that requires "+
			"approval, which this proposal declined (warns=%q)", repos, warns)
	}
}

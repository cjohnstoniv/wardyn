// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package composer

import (
	"encoding/json"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

func sshKeyGrant(t *testing.T, host, keyRef string, approval bool, ttl int) types.GrantSpec {
	t.Helper()
	sc, err := json.Marshal(map[string]any{"host": host, "key_secret_ref": keyRef})
	if err != nil {
		t.Fatalf("marshal ssh_key scope: %v", err)
	}
	return types.GrantSpec{Kind: types.GrantSSHKey, Scope: sc, RequiresApproval: approval, TTLSeconds: ttl}
}

// TestClampGrantsBoundsByPairingNotKind is the pin for the runtime half of the
// one comparator.
//
// clampGrants used to index the ceiling by KIND alone (`ceilByKind[cg.Kind] = cg`
// for every entry), so a ceiling holding two grants of one kind — the normal
// shape for two SSH forges, two api_key hosts or two git_pat hosts — collapsed
// to whichever came LAST, and that arbitrary grant supplied the approval posture
// and the TTL for every proposal of the kind. Two consequences, both here:
//
//   - APPROVAL STRIPPED. A proposal naming the STRICT forge's pairing was
//     clamped against the PERMISSIVE forge's grant and kept requires_approval
//     false — and a stripped approval flag auto-mints the injection at proxy
//     boot with no human in the loop.
//   - ORDER DEPENDENCE. The same ceiling SET produced different clamps
//     depending on slice order, in a codebase where a ceiling is a set
//     everywhere else.
func TestClampGrantsBoundsByPairingNotKind(t *testing.T) {
	corp := sshKeyGrant(t, "git.corp.example", "corp_key", true, 300)
	public := sshKeyGrant(t, "git.public.example", "public_key", false, 3600)
	// The proposal names the CORP pairing but asks for the PUBLIC posture.
	proposal := sshKeyGrant(t, "git.corp.example", "corp_key", false, 3600)

	clamp := func(t *testing.T, ceiling ...types.GrantSpec) types.GrantSpec {
		t.Helper()
		var warns []string
		out := clampGrants([]types.GrantSpec{proposal}, types.RunPolicySpec{EligibleGrants: ceiling}, &warns)
		if len(out) != 1 {
			t.Fatalf("proposal was dropped (warns=%q); this test is about the BOUND, so it must survive", warns)
		}
		return out[0]
	}

	forward := clamp(t, corp, public)
	if !forward.RequiresApproval {
		t.Error("requires_approval = false — the ceiling grant for git.corp.example sets it, so the pairing the proposal NAMES must supply the posture")
	}
	if forward.TTLSeconds != 300 {
		t.Errorf("ttl_seconds = %d, want 300 — git.corp.example's own ceiling TTL, not the other forge's 3600", forward.TTLSeconds)
	}

	// Same SET, reversed slice. A ceiling is a set; the answer must not move.
	reversed := clamp(t, public, corp)
	if reversed.RequiresApproval != forward.RequiresApproval || reversed.TTLSeconds != forward.TTLSeconds {
		t.Errorf("order dependence: [corp,public] -> (approval=%v ttl=%d), [public,corp] -> (approval=%v ttl=%d)",
			forward.RequiresApproval, forward.TTLSeconds, reversed.RequiresApproval, reversed.TTLSeconds)
	}

	// A proposal whose pairing NO ceiling entry names falls back to the meet of
	// every same-kind grant — the strictest bound, never an arbitrary one. It is
	// still kept (the pairing gate is filterMemberGrants' job, not the clamp's),
	// but it cannot pick up the permissive forge's posture on the way through.
	var warns []string
	unpaired := sshKeyGrant(t, "git.elsewhere.example", "other_key", false, 3600)
	out := clampGrants([]types.GrantSpec{unpaired}, types.RunPolicySpec{EligibleGrants: []types.GrantSpec{public, corp}}, &warns)
	if len(out) != 1 {
		t.Fatalf("an unpaired same-kind proposal was dropped by the CLAMP; that is the grant filter's decision, not this one (warns=%q)", warns)
	}
	if !out[0].RequiresApproval || out[0].TTLSeconds != 300 {
		t.Errorf("unpaired proposal clamped to (approval=%v ttl=%d), want the strictest same-kind bound (true, 300)",
			out[0].RequiresApproval, out[0].TTLSeconds)
	}
}

// TestGrantPairingIsExact pins the identity axis both callers now share: the
// host compares case- and trailing-dot-insensitively (a ceiling written either
// way must bind), the secret refs compare byte-exactly (a secret name is a store
// key, not a hostname), ssh_key's known_hosts ref is part of the match, and an
// undecodable scope matches NOTHING rather than everything.
func TestGrantPairingIsExact(t *testing.T) {
	ceiling := []types.GrantSpec{sshKeyGrant(t, "Git.Corp.Example.", "corp_key", true, 300)}
	for _, c := range []struct {
		name                     string
		host, secret, knownHosts string
		want                     bool
	}{
		{"exact", "git.corp.example", "corp_key", "", true},
		{"host case and trailing dot", "GIT.CORP.EXAMPLE.", "corp_key", "", true},
		{"different secret", "git.corp.example", "other_key", "", false},
		{"different host", "git.other.example", "corp_key", "", false},
		{"member-chosen known_hosts", "git.corp.example", "corp_key", "any-secret", false},
	} {
		sc, err := json.Marshal(map[string]any{
			"host": c.host, "key_secret_ref": c.secret, "known_hosts_secret_ref": c.knownHosts,
		})
		if err != nil {
			t.Fatalf("marshal ssh_key scope: %v", err)
		}
		proposed := types.GrantSpec{Kind: types.GrantSSHKey, Scope: sc}
		if got := PairingInCeiling(proposed, ceiling); got != c.want {
			t.Errorf("%s: PairingInCeiling = %v, want %v", c.name, got, c.want)
		}
	}

	// A wildcard/undecodable ceiling scope carries no pairing and authorizes no
	// member-chosen one.
	junk := []types.GrantSpec{{Kind: types.GrantSSHKey, Scope: json.RawMessage(`{"nope":1}`)}}
	if PairingInCeiling(sshKeyGrant(t, "git.corp.example", "corp_key", false, 3600), junk) {
		t.Error("an undecodable ceiling scope matched a pairing — a wildcard entry must authorize the KIND, never a member's chosen secret+host")
	}
	if _, _, _, covered, ok := GrantPairing(junk[0]); !covered || ok {
		t.Errorf("GrantPairing(undecodable ssh_key) = (covered=%v ok=%v), want (true, false)", covered, ok)
	}
	// github_token names no stored secret: covered=false is the honest answer,
	// and same-kind membership is its identity test.
	if _, _, _, covered, ok := GrantPairing(types.GrantSpec{Kind: types.GrantGitHubToken}); covered || !ok {
		t.Errorf("GrantPairing(github_token) = (covered=%v ok=%v), want (false, true)", covered, ok)
	}
}

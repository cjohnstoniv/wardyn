// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"slices"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
)

// TestGovernancePreviewGroupNormalizationMatchesEnforcement is F154.
//
// POST /governance/preview answers ONE question — "which profile would bind a
// principal presenting this claim" — so there has to be ONE answer. It
// re-implemented the group fold as a plain strings.ToLower with no ASCII guard,
// and that is not the snapshot's rule: oidc.sessionGroups applies the guard
// BEFORE the fold. strings.ToLower maps U+212A KELVIN SIGN to ASCII 'k' and
// U+0130 to ASCII 'i', so a crafted claim folded onto the real,
// operator-authored ASCII group and the preview reported that the profile BINDS
// it — while enforcement refused the same claim outright, dropped it from the
// snapshot and stamped the snapshot truncated.
//
// The assertion is AGREEMENT with oidc.CanonicalGroupSubject itself, not a
// hand-listed expectation: a second normalizer that happens to agree today is
// the thing that drifted.
func TestGovernancePreviewGroupNormalizationMatchesEnforcement(t *testing.T) {
	claims := []string{
		"eng-team",
		"  Eng-Team  ",
		"Wardyn.Contractors",
		"\u212Aubernetes-admins", // U+212A KELVIN SIGN, folds to ASCII 'k'
		"\u0130nfra",             // U+0130, folds to ASCII 'i'
		"\u00e9quipe-fr",
		"\u0438\u043d\u0436\u0435\u043d\u0435\u0440\u044b",
		"   ",
	}
	got, msg := normalizeGovernancePreviewGroups(claims)
	if msg != "" {
		t.Fatalf("preview refused the whole list: %s", msg)
	}

	// EVERY claim the snapshot can carry survives, canonicalized exactly as a
	// login would carry it; every claim it cannot is absent.
	for _, c := range claims {
		canon, ok := oidc.CanonicalGroupSubject(c)
		if ok {
			if !slices.Contains(got, canon) {
				t.Errorf("claim %+q canonicalizes to %q for a login, but the preview dropped it — the preview "+
					"would report 'no assignment' for a principal the run really matches", c, canon)
			}
			continue
		}
		// The load-bearing half: a claim enforcement refuses must not appear in
		// the preview's input AT ALL, folded or otherwise.
		folded, _ := oidc.CanonicalGroupSubject("kubernetes-admins")
		for _, g := range got {
			if g == folded && (c == "\u212Aubernetes-admins") {
				t.Errorf("claim %+q folded onto the operator-authored group %q — the preview reports the profile "+
					"BINDS a claim the enforcement path refuses outright (ok=false), drops from the snapshot, and "+
					"stamps the snapshot truncated", c, g)
			}
		}
		if slices.Contains(got, "infra") && c == "\u0130nfra" {
			t.Errorf("claim %+q folded onto %q — same divergence, different code point", c, "infra")
		}
	}

	// De-duplication and ORDER survive: order is the user tier's tie-break and
	// the group half must not quietly re-sort a shared code path's contract.
	if len(got) != 2 || got[0] != "eng-team" || got[1] != "wardyn.contractors" {
		t.Errorf("normalized = %v, want [eng-team wardyn.contractors] plus nothing else — the two spellings of "+
			"eng-team de-duplicate and every crafted claim is gone", got)
	}
}

// TestGovernancePreviewGroupsKeepTheCountCap: the group half must not lose the
// hostile-input bound the shared function carried.
func TestGovernancePreviewGroupsKeepTheCountCap(t *testing.T) {
	in := make([]string, maxGovernancePreviewClaims+1)
	for i := range in {
		in[i] = "g"
	}
	if _, msg := normalizeGovernancePreviewGroups(in); msg == "" {
		t.Errorf("a list past maxGovernancePreviewClaims (%d) was accepted — the group half dropped the cap the "+
			"user half keeps", maxGovernancePreviewClaims)
	}
}

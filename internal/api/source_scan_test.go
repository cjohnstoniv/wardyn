// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/internal/workspacescan"
)

// The invariant that once broke live: every row the scan seeder writes must
// pass the SAME validation any PUT of that contract goes through — a scan
// detects env-var names ("AWS_DEFAULT_REGION") the secret store can never
// hold, so the seeder maps them onto the storable grammar instead of writing
// keys that wedge the Requirements save with a 400 forever.
func TestSeedSourceRequirements_EveryRowValidates(t *testing.T) {
	p := workspacescan.WorkspaceProfile{
		RequiredSecrets: []workspacescan.SecretNeed{
			{Name: "AWS_DEFAULT_REGION"},
			{Name: "DATABASE_URL", Optional: true},
			{Name: "already-storable.key"},
			{Name: "___"}, // nothing storable remains — must be skipped, not seeded
		},
		EgressDomains: []string{"registry.npmjs.org"},
	}
	seed := seedSourceRequirements(types.SourceLocalDir, "/srv/app", p)

	for key, req := range seed {
		if msg := validateWorkspaceRequirement(key, req); msg != "" {
			t.Fatalf("seeder wrote a row its own validator rejects: %s", msg)
		}
	}
	if row := seed["secret:aws-default-region"]; row.Level != "required" || row.Provenance != "scan_seeded" {
		t.Fatalf("env-var name not mapped to storable secret name: %+v (keys=%v)", row, keysOf(seed))
	}
	if row := seed["secret:database-url"]; row.Level != "optional" {
		t.Fatalf("optional flag lost in mapping: %+v", row)
	}
	if _, ok := seed["secret:already-storable.key"]; !ok {
		t.Fatalf("already-storable name must pass through unchanged: keys=%v", keysOf(seed))
	}
	if len(seed) != 5 { // 3 secrets + 1 egress + 1 write (unstorable name skipped)
		t.Fatalf("want 5 seeded rows, got %d: %v", len(seed), keysOf(seed))
	}
}

// TestSeedSourceRequirements_SuggestedEgressNeverSeeded is the end-to-end half
// of the W6-S1-3 fix, at the exact boundary the finding's acceptance
// criterion names: "an AI-suggested host is NOT auto-unioned into a run's
// allowlist without operator approval". workspacescan.AdviseProfile (ai.go)
// now lands an AI-suggested host in SuggestedEgress, never EgressDomains (see
// ai_test.go); this test locks in the OTHER half of that guarantee — that
// seedSourceRequirements (the only producer of the "required"/auto-unioned
// egress:<host> contract rows applyWorkspaceRequirements folds into a run's
// AllowedDomains) reads ONLY EgressDomains, so a host that exists ONLY in
// SuggestedEgress never becomes a contract row — even for a future profile
// carrying nothing BUT an AI suggestion, egress can never auto-widen.
func TestSeedSourceRequirements_SuggestedEgressNeverSeeded(t *testing.T) {
	p := workspacescan.WorkspaceProfile{
		SuggestedEgress: []string{"evil.example.com"},
	}
	seed := seedSourceRequirements(types.SourceRepo, "", p)
	if len(seed) != 0 {
		t.Fatalf("a profile with only SuggestedEgress must seed nothing (it is display-only, never a contract row): %v", keysOf(seed))
	}
}

func keysOf(m map[string]types.WorkspaceRequirement) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

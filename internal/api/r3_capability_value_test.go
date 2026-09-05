// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestCapabilityWorkspaceValueIsCanonicalized is F142.
//
// The SUBJECT half of a capability grant is canonicalized at the write boundary
// (group subjects through oidc.CanonicalGroupSubject, user subjects lowercased)
// and the egress_host VALUE gets the proxy's own shape check. The workspace
// VALUE got neither, while the resolver compares it by exact string equality
// against uuid.UUID.String() — always canonical lowercase-hyphenated.
//
// So all four alternative spellings uuid.Parse accepts were stored 201-Created,
// rendered on the Permissions screen as an active DENY, and matched nothing:
// capValueOverlaps answered false for every request naming that workspace. A
// deny that protects nothing is the one outcome capabilities.go promises cannot
// happen, and nothing in the UI or the audit trail distinguished it from the
// canonical row that works.
func TestCapabilityWorkspaceValueIsCanonicalized(t *testing.T) {
	const canonical = "6b1f0e7a-1f4d-4f1e-9a2c-0d3e5f6a7b8c"

	t.Run("every spelling uuid.Parse accepts folds onto the canonical form", func(t *testing.T) {
		for _, form := range []string{
			canonical,
			"6B1F0E7A-1F4D-4F1E-9A2C-0D3E5F6A7B8C",
			"{6b1f0e7a-1f4d-4f1e-9a2c-0d3e5f6a7b8c}",
			"urn:uuid:6b1f0e7a-1f4d-4f1e-9a2c-0d3e5f6a7b8c",
			"6b1f0e7a1f4d4f1e9a2c0d3e5f6a7b8c",
			"  6b1f0e7a-1f4d-4f1e-9a2c-0d3e5f6a7b8c  ",
		} {
			g := types.CapabilityGrant{
				SubjectType: types.CapabilitySubjectUser, Subject: "sub-alice",
				Capability: capWorkspace, Value: form, Effect: types.CapabilityDeny,
			}
			if err := validateCapabilityGrant(&g); err != nil {
				t.Fatalf("%q was refused: %v — every spelling uuid.Parse accepts names the same workspace", form, err)
			}
			if g.Value != canonical {
				t.Errorf("%q stored as %q, want the canonical %q", form, g.Value, canonical)
			}
			// THE PROPERTY THAT MATTERS, asserted rather than inferred from the
			// stored string: the deny actually bites a request naming this
			// workspace. A row that validates and does not fire is exactly the
			// defect.
			if !capValueOverlaps(capWorkspace, g.Value, canonical) {
				t.Errorf("a DENY written as %q does not fire for workspace %s — the row renders as active and "+
					"protects nothing", form, canonical)
			}
		}
	})

	// A value uuid.Parse cannot read at all can never name a workspace, so
	// storing it would be the same inert row by another route.
	t.Run("a value that is not a uuid is refused", func(t *testing.T) {
		for _, bad := range []string{"not-a-uuid", "6b1f0e7a-1f4d-4f1e-9a2c", "payments-workspace"} {
			g := types.CapabilityGrant{
				SubjectType: types.CapabilitySubjectUser, Subject: "sub-alice",
				Capability: capWorkspace, Value: bad, Effect: types.CapabilityDeny,
			}
			if err := validateCapabilityGrant(&g); err == nil {
				t.Errorf("ACCEPTED %q as a workspace value (stored %q) — it can never match anything", bad, g.Value)
			}
		}
	})

	// The wildcard is this table's own spelling for "every value of this kind",
	// not a workspace id — the same exemption the egress_host arm makes.
	t.Run("the wildcard is exempt", func(t *testing.T) {
		g := types.CapabilityGrant{
			SubjectType: types.CapabilitySubjectAll, Capability: capWorkspace,
			Value: capWildcard, Effect: types.CapabilityDeny,
		}
		if err := validateCapabilityGrant(&g); err != nil {
			t.Fatalf("the wildcard was refused: %v", err)
		}
		if g.Value != capWildcard {
			t.Errorf("wildcard stored as %q", g.Value)
		}
	})

	// The exact-compare kinds keep their own case, which is the decision the
	// fix records: a secret name is authored elsewhere with its own case, and
	// folding it here would stop it matching the row secrets.go stores.
	t.Run("the exact-compare kinds are stored verbatim", func(t *testing.T) {
		for _, kind := range []string{capSecret, capAgent, capImage, capIntegration} {
			g := types.CapabilityGrant{
				SubjectType: types.CapabilitySubjectUser, Subject: "sub-alice",
				Capability: kind, Value: "Acme-Prod-DB", Effect: types.CapabilityDeny,
			}
			if err := validateCapabilityGrant(&g); err != nil {
				t.Fatalf("%s value refused: %v", kind, err)
			}
			if g.Value != "Acme-Prod-DB" {
				t.Errorf("%s value stored as %q, want it verbatim — these identifiers are authored elsewhere "+
					"with their own case", kind, g.Value)
			}
		}
	})
}

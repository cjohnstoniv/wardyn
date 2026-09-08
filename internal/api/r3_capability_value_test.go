// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
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

	// The kinds whose vocabulary is NOT provably narrower than what is typed
	// keep their own case: an agent id is not held to a closed catalog at the
	// run boundary (a BYOA run names its own) and an image ref's tag may
	// legitimately carry uppercase, so folding them would invent a canonical
	// form the resolver does not use.
	t.Run("agent and image are stored verbatim", func(t *testing.T) {
		for _, kind := range []string{capAgent, capImage} {
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

	// SECRET AND INTEGRATION are the sibling kinds F142's title names, and this
	// subtest used to pin the defect: it asserted "Acme-Prod-DB" was stored
	// verbatim for capSecret, on a rationale that was inverted. secretNameRE
	// forbids uppercase in every stored secret name and integrationRefRE does
	// the same for an integration id, so an uppercase value could never match
	// the row it names — the workspace arm's inert DENY, on another kind.
	// Folding can only ever make the grant match the row its author meant.
	t.Run("secret and integration fold onto the grammar their rows use", func(t *testing.T) {
		for kind, want := range map[string]string{capSecret: "acme-prod-db", capIntegration: "corp-artifactory"} {
			in := map[string]string{capSecret: "Acme-Prod-DB", capIntegration: "Corp-Artifactory"}[kind]
			g := types.CapabilityGrant{
				SubjectType: types.CapabilitySubjectUser, Subject: "sub-alice",
				Capability: kind, Value: "  " + in + "  ", Effect: types.CapabilityDeny,
			}
			if err := validateCapabilityGrant(&g); err != nil {
				t.Fatalf("%s value %q refused: %v", kind, in, err)
			}
			if g.Value != want {
				t.Errorf("%s value %q stored as %q, want %q — an uppercase value names a row that cannot exist, "+
					"so the row renders as an active DENY and never fires", kind, in, g.Value, want)
			}
			if !capValueOverlaps(kind, g.Value, want) {
				t.Errorf("a %s DENY written %q does not fire for %q", kind, in, want)
			}
		}
	})

	t.Run("a secret or integration value that no row could ever carry is refused", func(t *testing.T) {
		for _, tc := range []struct{ kind, value string }{
			{capSecret, "acme prod db"},          // spaces are outside secretNameRE
			{capSecret, "-leading-hyphen"},       // must start alphanumeric
			{capSecret, "\u212Aim-secret"},       // KELVIN SIGN: non-ASCII, refused BEFORE the fold
			{capIntegration, "corp/artifactory"}, // outside integrationRefRE
		} {
			g := types.CapabilityGrant{
				SubjectType: types.CapabilitySubjectUser, Subject: "sub-alice",
				Capability: tc.kind, Value: tc.value, Effect: types.CapabilityDeny,
			}
			if err := validateCapabilityGrant(&g); err == nil {
				t.Errorf("ACCEPTED %s value %q (stored %q) — it can never match anything", tc.kind, tc.value, g.Value)
			}
		}
	})
}

// inertGrantStore serves a grant table written by an OLDER Wardyn: the rows the
// per-kind value rule would refuse or fold today, beside the canonical ones.
type inertGrantStore struct {
	store.Store
	rows []types.CapabilityGrant
}

func (s *inertGrantStore) ListCapabilityGrants(context.Context) ([]types.CapabilityGrant, error) {
	return s.rows, nil
}

func (s *inertGrantStore) GetCapabilityEnforcement(context.Context) (map[string]bool, error) {
	return map[string]bool{capWorkspace: true}, nil
}

// TestListPermissionsMarksRowsThatCanNeverMatch is F142's second residual: the
// value rule is a WRITE-boundary rule, and capability_grants shipped in v0.6.0.
//
// Every non-canonical row written before it survives the upgrade unchanged and
// keeps rendering on the Permissions screen as an active DENY that has never
// once fired — with no signal anywhere. The upgrade must not silently rewrite
// them (a normalizing migration would make an inert ALLOW start granting,
// unreviewed, at boot) and must not drop them, so it says so where the operator
// is looking at the row.
//
// The marker asks the WRITE boundary's own function, so it cannot drift from
// the rule it describes.
func TestListPermissionsMarksRowsThatCanNeverMatch(t *testing.T) {
	const canonical = "6b1f0e7a-1f4d-4f1e-9a2c-0d3e5f6a7b8c"
	rows := []types.CapabilityGrant{
		{ID: uuid.New(), SubjectType: types.CapabilitySubjectUser, Subject: "sub-alice",
			Capability: capWorkspace, Value: canonical, Effect: types.CapabilityDeny},
		{ID: uuid.New(), SubjectType: types.CapabilitySubjectUser, Subject: "sub-alice",
			Capability: capWorkspace, Value: "6B1F0E7A-1F4D-4F1E-9A2C-0D3E5F6A7B8C", Effect: types.CapabilityDeny},
		{ID: uuid.New(), SubjectType: types.CapabilitySubjectUser, Subject: "sub-alice",
			Capability: capWorkspace, Value: "payments-workspace", Effect: types.CapabilityDeny},
		{ID: uuid.New(), SubjectType: types.CapabilitySubjectUser, Subject: "sub-alice",
			Capability: capSecret, Value: "Acme-Prod-DB", Effect: types.CapabilityDeny},
		{ID: uuid.New(), SubjectType: types.CapabilitySubjectAll,
			Capability: capSecret, Value: capWildcard, Effect: types.CapabilityAllow},
	}
	srv := New(Config{Store: &inertGrantStore{rows: rows}, Audit: &recRecorder{},
		RunnerTarget: "docker", AdminToken: adminToken})

	w := do(t, srv, http.MethodGet, "/api/v1/permissions", adminToken, "")
	if w.Code != http.StatusOK {
		t.Fatalf("GET /permissions = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var got struct {
		Grants []struct {
			Capability string `json:"capability"`
			Value      string `json:"value"`
			Inert      bool   `json:"inert"`
		} `json:"grants"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v; body=%s", err, w.Body.String())
	}
	if len(got.Grants) != len(rows) {
		t.Fatalf("grants = %d, want %d — marking rows must not drop any", len(got.Grants), len(rows))
	}
	want := map[string]bool{
		canonical:                              false,
		"6B1F0E7A-1F4D-4F1E-9A2C-0D3E5F6A7B8C": true,
		"payments-workspace":                   true,
		"Acme-Prod-DB":                         true,
		capWildcard:                            false,
	}
	for _, g := range got.Grants {
		if g.Inert != want[g.Value] {
			t.Errorf("%s=%q inert=%v, want %v — a row the resolver can never be asked about renders as an active "+
				"rule that protects nothing, and an upgrade cannot safely rewrite it", g.Capability, g.Value, g.Inert, want[g.Value])
		}
	}

	// STRICT SUPERSET: every field an existing client decodes is still there.
	if !strings.Contains(w.Body.String(), `"effect":"deny"`) || !strings.Contains(w.Body.String(), `"enforcement"`) {
		t.Errorf("the response is no longer a superset of what a 0.6 client decodes: %s", w.Body.String())
	}
}

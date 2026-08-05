// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"encoding/json"
	"testing"

	"github.com/google/uuid"
)

// The fold is the three-tier split's load-bearing function: everything a run
// gets from a workspace's composition routes through it. The two identities
// at the top are what make migration 0031 provably behavior-identical.

func req(level, prov string) WorkspaceRequirement {
	return WorkspaceRequirement{Level: level, Provenance: prov}
}

func srcWith(locator string, reqs map[string]WorkspaceRequirement) (uuid.UUID, map[uuid.UUID]Source) {
	id := uuid.New()
	return id, map[uuid.UUID]Source{id: {ID: id, Kind: SourceLocalDir, Locator: locator, Requirements: reqs}}
}

func attach(id uuid.UUID, overrides map[string]string) []WorkspaceAttachment {
	return []WorkspaceAttachment{{SourceID: &id, Overrides: overrides}}
}

func foldJSON(t *testing.T, m map[string]WorkspaceRequirement) string {
	t.Helper()
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// ─── the identities ──────────────────────────────────────────────────────────

// Zero attachments (or all-ephemeral): the fold IS the overlay, byte-for-byte.
// Every pre-split workspace migrates with all rows in the overlay and no
// source contracts, so this identity is 0031's behavior-identical guarantee.
func TestFoldWorkspaceContract_ZeroSourcesIsOverlayIdentity(t *testing.T) {
	overlay := map[string]WorkspaceRequirement{
		"secret:acme-key":       req("required", "operator_set"),
		"egress:api.stripe.com": req("optional", "scan_seeded"),
	}
	for name, atts := range map[string][]WorkspaceAttachment{
		"no attachments":  nil,
		"ephemeral only":  {{Ephemeral: true, Target: "/home/agent/work"}},
		"dangling source": {{SourceID: func() *uuid.UUID { u := uuid.New(); return &u }()}},
	} {
		t.Run(name, func(t *testing.T) {
			got := FoldWorkspaceContract(atts, nil, overlay)
			if foldJSON(t, got) != foldJSON(t, overlay) {
				t.Errorf("fold = %s\nwant the overlay verbatim %s", foldJSON(t, got), foldJSON(t, overlay))
			}
		})
	}
	// And nil-everything folds to nil, not an empty map (omitempty round-trip).
	if got := FoldWorkspaceContract(nil, nil, nil); got != nil {
		t.Errorf("fold(nil, nil, nil) = %#v, want nil", got)
	}
}

// One source, no overrides, no overlay: the source's contract verbatim.
func TestFoldWorkspaceContract_SingleSourceNoOverlay(t *testing.T) {
	contract := map[string]WorkspaceRequirement{
		"secret:pg-url":             req("required", "scan_seeded"),
		"egress:registry.npmjs.org": req("required", "scan_seeded"),
	}
	id, sources := srcWith("/home/me/payments", contract)
	got := FoldWorkspaceContract(attach(id, nil), sources, nil)
	if foldJSON(t, got) != foldJSON(t, contract) {
		t.Errorf("fold = %s, want the source contract verbatim", foldJSON(t, got))
	}
}

// ─── the precedence table, one rule at a time ───────────────────────────────

// Rule 2: "off" is this workspace refusing the requirement — the source keeps
// declaring it, siblings keep inheriting it, THIS fold drops it.
func TestFoldWorkspaceContract_OverrideOffDrops(t *testing.T) {
	id, sources := srcWith("/repo", map[string]WorkspaceRequirement{
		"secret:build-key":   req("required", "operator_set"),
		"egress:builds.corp": req("required", "operator_set"),
	})
	got := FoldWorkspaceContract(attach(id, map[string]string{"secret:build-key": OverrideOff}), sources, nil)
	if _, present := got["secret:build-key"]; present {
		t.Error("off-overridden key must not fold in")
	}
	if _, present := got["egress:builds.corp"]; !present {
		t.Error("un-overridden sibling key must survive")
	}
}

// Rule 3: a lane override replaces the source's lane for this workspace only.
func TestFoldWorkspaceContract_OverrideRelanes(t *testing.T) {
	id, sources := srcWith("/repo", map[string]WorkspaceRequirement{
		"egress:api.github.com": req("required", "operator_set"),
	})
	got := FoldWorkspaceContract(attach(id, map[string]string{"egress:api.github.com": OverrideOptional}), sources, nil)
	if got["egress:api.github.com"].Level != "optional" {
		t.Errorf("lane = %q, want the override's optional", got["egress:api.github.com"].Level)
	}
	// Provenance rides along untouched — re-laning isn't re-authoring.
	if got["egress:api.github.com"].Provenance != "operator_set" {
		t.Errorf("provenance = %q, want the source's own", got["egress:api.github.com"].Provenance)
	}
}

// Rule 4 (SECURITY): a source may claim write ONLY on its own locator.
// applyWriteNarrowing widens any mount whose path matches, so a cross-path
// write claim from a shared source would widen a sibling mount in every
// attaching workspace — the one genuine escalation the fold could produce.
func TestFoldWorkspaceContract_CrossPathWriteDropped(t *testing.T) {
	id, sources := srcWith("/home/me/payments", map[string]WorkspaceRequirement{
		"write:/home/me/payments":   req("optional", "operator_set"),
		"write:/home/me/other-repo": req("required", "operator_set"), // hostile/mistaken
	})
	got := FoldWorkspaceContract(attach(id, nil), sources, nil)
	if _, present := got["write:/home/me/other-repo"]; present {
		t.Error("cross-path write claim must be dropped — a source only owns itself")
	}
	if _, present := got["write:/home/me/payments"]; !present {
		t.Error("own-path write claim must survive")
	}
}

// Rule 5: strongest level, weakest provenance — fail-closed on collision.
func TestFoldWorkspaceContract_CollisionMergesStrongLevelWeakProvenance(t *testing.T) {
	idA := uuid.New()
	idB := uuid.New()
	sources := map[uuid.UUID]Source{
		idA: {ID: idA, Kind: SourceLocalDir, Locator: "/a", Requirements: map[string]WorkspaceRequirement{
			"secret:shared-key": req("optional", "operator_set"),
		}},
		idB: {ID: idB, Kind: SourceLocalDir, Locator: "/b", Requirements: map[string]WorkspaceRequirement{
			"secret:shared-key": req("required", "scan_seeded"),
		}},
	}
	atts := []WorkspaceAttachment{{SourceID: &idA}, {SourceID: &idB}}
	got := FoldWorkspaceContract(atts, sources, nil)
	row := got["secret:shared-key"]
	if row.Level != "required" {
		t.Errorf("level = %q, want strongest (required)", row.Level)
	}
	// The scan_seeded contributor poisons auto-grant for the merged row: the
	// run-create trust boundary only auto-mints operator_set.
	if row.Provenance != "scan_seeded" {
		t.Errorf("provenance = %q, want weakest (scan_seeded)", row.Provenance)
	}
}

// Rule 6: the overlay replaces outright — level AND provenance. This is
// exactly the verify-approve promotion: a source's scan_seeded egress row
// becomes workspace-level operator_set.
func TestFoldWorkspaceContract_OverlayReplacesOutright(t *testing.T) {
	id, sources := srcWith("/repo", map[string]WorkspaceRequirement{
		"egress:api.stripe.com": req("optional", "scan_seeded"),
	})
	overlay := map[string]WorkspaceRequirement{
		"egress:api.stripe.com": req("required", "operator_set"),
	}
	got := FoldWorkspaceContract(attach(id, nil), sources, overlay)
	if got["egress:api.stripe.com"] != req("required", "operator_set") {
		t.Errorf("row = %+v, want the overlay verbatim", got["egress:api.stripe.com"])
	}
}

// Determinism: equal inputs marshal byte-equal, whatever map order Go deals.
func TestFoldWorkspaceContract_Deterministic(t *testing.T) {
	id, sources := srcWith("/repo", map[string]WorkspaceRequirement{
		"egress:a.example": req("required", "scan_seeded"),
		"egress:b.example": req("optional", "scan_seeded"),
		"secret:k1":        req("required", "operator_set"),
	})
	first := foldJSON(t, FoldWorkspaceContract(attach(id, nil), sources, nil))
	for i := 0; i < 20; i++ {
		if got := foldJSON(t, FoldWorkspaceContract(attach(id, nil), sources, nil)); got != first {
			t.Fatalf("fold is nondeterministic: %s vs %s", got, first)
		}
	}
}

// The grammar parser, moved home: first-colon split, four known tokens.
func TestSplitRequirementKey_Types(t *testing.T) {
	cases := []struct {
		key, typ, rest string
		ok             bool
	}{
		{"secret:acme-key", "secret", "acme-key", true},
		{"egress:api.github.com", "egress", "api.github.com", true},
		{"write:/home/user:odd", "write", "/home/user:odd", true},
		{"integration:corp-artifactory", "integration", "corp-artifactory", true},
		{"nocolon", "", "", false},
		{"secret:", "", "", false},
		{"unknown:x", "", "", false},
	}
	for _, c := range cases {
		typ, rest, ok := SplitRequirementKey(c.key)
		if typ != c.typ || rest != c.rest || ok != c.ok {
			t.Errorf("SplitRequirementKey(%q) = (%q,%q,%v), want (%q,%q,%v)", c.key, typ, rest, ok, c.typ, c.rest, c.ok)
		}
	}
}

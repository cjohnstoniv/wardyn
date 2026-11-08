// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bytes"
	"os"
	"slices"
	"testing"
)

// TestAuditUnitViewAllowListCensus pins the unit view's data allow list: every key it serves is a key
// docs/AUDIT-ACTIONS.md documents, and none is a key that holds what only the run's owner or the
// security tier may read. A key missing from the list is withheld, so the list only ever grows on purpose.
func TestAuditUnitViewAllowListCensus(t *testing.T) {
	doc, err := os.ReadFile("../../docs/AUDIT-ACTIONS.md")
	if err != nil {
		t.Fatalf("read the audit action docs: %v", err)
	}
	if len(unitViewDataKeys) == 0 {
		t.Fatal("the unit view serves no data key")
	}
	for _, k := range unitViewDataKeys {
		if !bytes.Contains(doc, []byte("`"+k+"`")) {
			t.Errorf("unit view data key %q is not documented in docs/AUDIT-ACTIONS.md", k)
		}
	}
	never := []string{"policy_source", "personal_walls", "wall_preset", "restriction_ids", "overrides_diff",
		"contact", "spec", "inline_policy", "secret_name", "error", "detail", "owner", "subject"}
	for _, k := range never {
		if slices.Contains(unitViewDataKeys, k) {
			t.Errorf("unit view data key %q holds what a unit admin must not read", k)
		}
	}
}

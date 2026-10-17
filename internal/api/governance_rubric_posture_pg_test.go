// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"net/http"
	"slices"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A profile write held for a second person keeps the rubric sentence in the change's notes, so the
// proposer and the approver read what a direct write would have answered. Guarded by WARDYN_TEST_PG.
func TestPG_HeldProfileWriteNotesCarryTheRubricPosture(t *testing.T) {
	e := newGovEnv(t, func(c *Config) { c.Runner = &fakeRunner{capsClasses: []types.ConfinementClass{types.CC1}} })
	const body = `{"name":"held-rubric","ceiling":{"min_confinement_class":"CC1"},"limits":{"autonomy_rubric":{"secrets_powerful":"L1"}}}`
	ch := e.pending(e.call(e.alice, http.MethodPost, "/api/v1/governance/profiles", body))
	var diff struct {
		Notes []string `json:"notes"`
	}
	if err := json.Unmarshal(ch.Diff, &diff); err != nil {
		t.Fatalf("decode diff %s: %v", ch.Diff, err)
	}
	// No model provider on this site: every run folds no credential, so every secrets row can bind.
	const want = "on this deployment a run under this profile grades egress sealed, reviewed or open; " +
		"secrets none, baseline or powerful; and confinement CC1 — so only egress_open, egress_reviewed, egress_sealed, " +
		"secrets_powerful, secrets_baseline, secrets_none and confinement_cc1 can cap it, and confinement_cc2 and " +
		"confinement_cc3 cap nothing here"
	if !slices.Contains(diff.Notes, want) {
		t.Errorf("held change notes = %q, want the rubric sentence %q", diff.Notes, want)
	}
}

// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A profile is synthesised from a run that used an organisation's component:
// the grant the component added is left out, with a line saying so, instead
// of the whole synthesis being refused for a mark nobody may author. The
// answer names no organisation secret.
func TestSynthesizeProfile_ARunThatUsedASharedGrantStillSynthesizes(t *testing.T) {
	runID := uuid.New()
	fake, shared := sharedMintStore(types.AgentRun{ID: runID, Agent: "claude-code", Repo: "org/repo",
		State: types.RunCompleted, ConfinementClass: types.CC2}, types.Workspace{})
	fake.grants, fake.events = fake.grants[:1], fake.events[:2] // the shared grant alone
	cfg := baseTestConfig(newHarness(t), fake)
	cfg.DefaultPolicy = types.RunPolicySpec{AllowedDomains: []string{"api.anthropic.com", "org-api.example"},
		MinConfinementClass: types.CC2, EligibleGrants: []types.GrantSpec{{Kind: types.GrantAPIKey}}}
	srv := New(cfg)

	w := do(t, srv, http.MethodPost, "/api/v1/runs/"+runID.String()+"/profile", adminToken, "")
	body := w.Body.String()
	if w.Code != http.StatusOK {
		t.Fatalf("POST .../profile = %d %s, want 200", w.Code, body)
	}
	if strings.Contains(body, sharedSecretName) {
		t.Errorf("the proposal names the organisation's secret: %s", body)
	}
	var resp profileResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	for _, g := range resp.Proposed.InlinePolicy.EligibleGrants {
		if apiKeyScopeShared(g.Scope) {
			t.Errorf("the proposal carries a shared grant: %s", g.Scope)
		}
	}
	if !slices.ContainsFunc(resp.Warnings, func(warn string) bool {
		return strings.Contains(warn, shared.String()) && strings.Contains(warn, "organisation's component")
	}) {
		t.Errorf("warnings = %v, want one saying the component's grant was left out", resp.Warnings)
	}
}

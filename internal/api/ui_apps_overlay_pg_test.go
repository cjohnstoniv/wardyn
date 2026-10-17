// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestPG_ComposedEmptyUIAppsOverlayDeniesUIApps: a composed profile already stored with an overlay
// of `ui_apps: []` over a base that names an app resolves, and it denies UI apps at every reader:
// a run's create and its preflight preview drop the apps, and the UI-session door refuses a run
// created under it.
func TestPG_ComposedEmptyUIAppsOverlayDeniesUIApps(t *testing.T) {
	ctx := context.Background()
	pg := store.NewPG(throwawayPGPool(t))
	h := newHarness(t)
	cfg := baseTestConfig(h, pg)
	cfg.OIDC = &oidc.Authenticator{}
	cfg.DefaultPolicy = govTestPolicy
	srv := New(cfg)

	docs := types.UIApp{Name: "docs", Port: 8000}
	base, err := pg.UpsertGovernanceProfile(ctx, types.GovernanceProfile{Name: "base", CreatedBy: "seed",
		Ceiling: types.RunPolicySpec{AllowedDomains: []string{"pypi.org"}, MinConfinementClass: types.CC2, UIApps: []types.UIApp{docs}}})
	if err != nil {
		t.Fatal(err)
	}
	// Written straight to the store, as a row from before this rule would be.
	team, err := pg.UpsertGovernanceProfile(ctx, types.GovernanceProfile{Name: "team", CreatedBy: "seed",
		BaseProfileID: &base.ID, Overlay: &types.CeilingOverlay{UIApps: &[]types.UIApp{}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pg.UpsertGovernanceAssignment(ctx, types.GovernanceAssignment{SubjectType: types.CapabilitySubjectUser,
		Subject: "member@corp.example", ProfileID: team.ID, Priority: 1, CreatedBy: "seed"}); err != nil {
		t.Fatal(err)
	}

	mctx := operatorCtx("sub-member", "member@corp.example", oidc.RoleUser)
	r := httptest.NewRequest(http.MethodPost, "/api/v1/runs", nil).WithContext(mctx)
	for _, dryRun := range []bool{true, false} {
		spec := types.RunPolicySpec{MinConfinementClass: types.CC2, UIApps: []types.UIApp{docs}}
		w := httptest.NewRecorder()
		got, _, warns, _, ok := srv.resolveRunPolicy(mctx, w, r, &createRunRequest{Agent: "claude-code", InlinePolicy: &spec}, dryRun)
		if !ok {
			t.Fatalf("dryRun=%v: the profile did not resolve: %d %s", dryRun, w.Code, w.Body)
		}
		if len(got.UIApps) != 0 || !strings.Contains(strings.Join(warns, "\n"), `denied by governance profile "team"`) {
			t.Errorf("dryRun=%v: ui_apps = %+v, warnings = %q; want none and the denial named", dryRun, got.UIApps, warns)
		}
	}

	// The ceiling reads: the member's default policy lists no apps, since a run under it gets
	// none; the admin view shows the base's list beside deny_ui_apps.
	w := httptest.NewRecorder()
	srv.handleGetDefaultPolicy(w, httptest.NewRequest(http.MethodGet, "/api/v1/policies/default", nil).WithContext(mctx))
	var def defaultPolicyResponse
	if err := json.Unmarshal(w.Body.Bytes(), &def); w.Code != http.StatusOK || err != nil || len(def.UIApps) != 0 || def.GovernanceProfileName != "team" {
		t.Errorf("GET /policies/default as the member = %d %s, want 200 with no ui_apps under \"team\"", w.Code, w.Body)
	}
	w = httptest.NewRecorder()
	actx := operatorCtx("sub-admin", "admin@corp.example", oidc.RoleAdmin)
	srv.handleGetGovernance(w, httptest.NewRequest(http.MethodGet, "/api/v1/governance", nil).WithContext(actx))
	var view struct {
		Profiles []governanceProfileView `json:"profiles"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &view); w.Code != http.StatusOK || err != nil {
		t.Fatalf("GET /governance = %d %s (%v)", w.Code, w.Body, err)
	}
	seen := false
	for _, p := range view.Profiles {
		if p.Name != "team" {
			continue
		}
		seen = true
		if eff := p.Effective; len(eff.Ceiling.UIApps) != 1 || eff.Ceiling.UIApps[0].Name != "docs" || !eff.Limits.DenyUIApps {
			t.Errorf("the admin view of team = ui_apps %+v, deny_ui_apps %v; want the base's list and true", eff.Ceiling.UIApps, eff.Limits.DenyUIApps)
		}
	}
	if !seen {
		t.Errorf("GET /governance lists no profile named team: %s", w.Body)
	}

	w = httptest.NewRecorder()
	run := types.AgentRun{ID: uuid.New(), GovernanceProfileID: &team.ID}
	if !srv.refuseUIAppsDenied(w, r, run) || w.Code != http.StatusForbidden {
		t.Errorf("the UI-session door into a run under the profile = %d %s, want 403", w.Code, w.Body)
	}
}

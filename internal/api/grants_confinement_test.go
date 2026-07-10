// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/db"
	"github.com/cjohnstoniv/wardyn/internal/identity/embedded"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// ─── Feature A: confinement_class (WS-2.6) ──────────────────────────────────

// TestParseConfinementClass locks in the request-field validation: empty is
// allowed (inherit the policy minimum), the three known classes parse, and any
// unknown non-empty value is rejected (fail closed → the handler returns 400).
func TestParseConfinementClass(t *testing.T) {
	cases := []struct {
		in     string
		want   types.ConfinementClass
		wantOK bool
	}{
		{"", "", true}, // empty = inherit/unset, allowed
		{"CC1", types.CC1, true},
		{"CC2", types.CC2, true},
		{"CC3", types.CC3, true},
		{"CC9", "", false}, // unknown
		{"cc1", "", false}, // case-sensitive (constants are upper)
		{"garbage", "", false},
	}
	for _, c := range cases {
		got, ok := parseConfinementClass(c.in)
		if ok != c.wantOK || got != c.want {
			t.Errorf("parseConfinementClass(%q) = (%q,%v), want (%q,%v)",
				c.in, got, ok, c.want, c.wantOK)
		}
	}
}

// TestCreateRunRejectsUnknownConfinement asserts the HTTP-layer fail-closed: an
// unknown confinement_class is rejected with 400 BEFORE any store write (the
// harness has no Pool, so reaching the store would panic — the 400 must fire
// first). This proves the validation is wired into handleCreateRun.
func TestCreateRunRejectsUnknownConfinement(t *testing.T) {
	h := newHarness(t)
	body := `{"agent":"claude-code","repo":"acme/widgets","confinement_class":"CC9"}`
	w := do(t, h.srv, http.MethodPost, "/api/v1/runs", adminToken, body)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("unknown confinement: code = %d, want 400; body=%s", w.Code, w.Body.String())
	}
}

// TestCreateRunRejectsWeakerConfinement asserts that a requested class WEAKER
// than the policy minimum (default policy is CC2) is refused with 422 — a run
// may only request equal-or-stronger confinement, never erode the policy floor.
// This also fires before any store write (no Pool in the harness).
func TestCreateRunRejectsWeakerConfinement(t *testing.T) {
	h := newHarness(t)
	body := `{"agent":"claude-code","repo":"acme/widgets","confinement_class":"CC1"}`
	w := do(t, h.srv, http.MethodPost, "/api/v1/runs", adminToken, body)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("weaker confinement: code = %d, want 422; body=%s", w.Code, w.Body.String())
	}
}

// ─── pool-backed end-to-end tests (WARDYN_TEST_PG) ──────────────────────────

// pgHarness builds a Server wired to a real Postgres pool. Guarded by
// WARDYN_TEST_PG; skipped cleanly when unset. Mirrors newHarness but with a
// live store so the create-run / grants paths (which require s.cfg.Pool) run.
func pgHarness(t *testing.T) (*Server, *pgxpool.Pool) {
	t.Helper()
	dsn := os.Getenv("WARDYN_TEST_PG")
	if dsn == "" {
		t.Skip("WARDYN_TEST_PG not set; skipping Postgres-backed api test")
	}
	ctx := context.Background()
	pool, err := db.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	if err := db.Migrate(ctx, pool); err != nil {
		pool.Close()
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(pool.Close)

	audit := &recRecorder{}
	idp, err := embedded.New(nil, "wardyn.local", embedded.NewMemRevocationStore(), audit)
	if err != nil {
		t.Fatalf("embedded.New: %v", err)
	}
	srv := New(Config{
		Store:       store.NewPG(pool),
		Pool:        pool,
		Identity:    idp,
		Approvals:   newFakeApprovals(),
		Broker:      &fakeBroker{},
		Audit:       audit,
		AdminToken:  adminToken,
		TrustDomain: "wardyn.local",
		DefaultPolicy: types.RunPolicySpec{
			AllowedDomains:      []string{"api.anthropic.com"},
			MinConfinementClass: types.CC2,
			EligibleGrants: []types.GrantSpec{
				{Kind: types.GrantGitHubToken, Scope: json.RawMessage(`{"repos":["acme/widgets"]}`), RequiresApproval: true},
			},
		},
		ControlPlaneURL: "http://wardynd:8080",
	})
	return srv, pool
}

// TestCreateRunThreadsConfinement verifies the requested confinement_class is
// parsed and persisted onto the created AgentRun (CC3 > the CC2 policy min).
func TestCreateRunThreadsConfinement(t *testing.T) {
	srv, _ := pgHarness(t)
	body := `{"agent":"claude-code","repo":"acme/widgets","confinement_class":"CC3"}`
	w := do(t, srv, http.MethodPost, "/api/v1/runs", adminToken, body)
	if w.Code != http.StatusCreated {
		t.Fatalf("create run: code = %d, want 201; body=%s", w.Code, w.Body.String())
	}
	var run types.AgentRun
	if err := json.Unmarshal(w.Body.Bytes(), &run); err != nil {
		t.Fatalf("decode run: %v", err)
	}
	if run.ConfinementClass != types.CC3 {
		t.Errorf("confinement_class = %q, want CC3 (threaded from request)", run.ConfinementClass)
	}
}

// TestCreateRunInheritsConfinementWhenUnset verifies an empty confinement_class
// inherits the policy minimum (CC2) — the prior default behavior is preserved.
func TestCreateRunInheritsConfinementWhenUnset(t *testing.T) {
	srv, _ := pgHarness(t)
	body := `{"agent":"claude-code","repo":"acme/widgets"}`
	w := do(t, srv, http.MethodPost, "/api/v1/runs", adminToken, body)
	if w.Code != http.StatusCreated {
		t.Fatalf("create run: code = %d, want 201; body=%s", w.Code, w.Body.String())
	}
	var run types.AgentRun
	if err := json.Unmarshal(w.Body.Bytes(), &run); err != nil {
		t.Fatalf("decode run: %v", err)
	}
	if run.ConfinementClass != types.CC2 {
		t.Errorf("confinement_class = %q, want CC2 (inherited policy minimum)", run.ConfinementClass)
	}
}

// ─── Feature B: GET /runs/{id}/grants (WS-2.4) ──────────────────────────────

// TestListGrantsReturnsRecords creates a run (whose default policy seeds one
// github_token eligibility grant) and asserts GET /runs/{id}/grants returns it.
func TestListGrantsReturnsRecords(t *testing.T) {
	srv, _ := pgHarness(t)

	// Create a run; the seeded policy mints one eligibility grant.
	cw := do(t, srv, http.MethodPost, "/api/v1/runs", adminToken,
		`{"agent":"claude-code","repo":"acme/widgets"}`)
	if cw.Code != http.StatusCreated {
		t.Fatalf("create run: code = %d, want 201; body=%s", cw.Code, cw.Body.String())
	}
	var run types.AgentRun
	if err := json.Unmarshal(cw.Body.Bytes(), &run); err != nil {
		t.Fatalf("decode run: %v", err)
	}

	w := do(t, srv, http.MethodGet, "/api/v1/runs/"+run.ID.String()+"/grants", adminToken, "")
	if w.Code != http.StatusOK {
		t.Fatalf("list grants: code = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var grants []types.CredentialGrant
	if err := json.Unmarshal(w.Body.Bytes(), &grants); err != nil {
		t.Fatalf("decode grants: %v", err)
	}
	if len(grants) != 1 {
		t.Fatalf("grants = %d, want 1", len(grants))
	}
	if grants[0].RunID != run.ID {
		t.Errorf("grant run id = %s, want %s", grants[0].RunID, run.ID)
	}
	if grants[0].Spec.Kind != types.GrantGitHubToken {
		t.Errorf("grant kind = %q, want github_token", grants[0].Spec.Kind)
	}
}

// TestListGrantsUnknownRunNotFound asserts an unknown run id behaves like the
// existing GET /runs/{id} not-found path: 404.
func TestListGrantsUnknownRunNotFound(t *testing.T) {
	srv, _ := pgHarness(t)
	w := do(t, srv, http.MethodGet, "/api/v1/runs/"+uuid.NewString()+"/grants", adminToken, "")
	if w.Code != http.StatusNotFound {
		t.Fatalf("unknown run grants: code = %d, want 404; body=%s", w.Code, w.Body.String())
	}
}

// TestListGrantsInvalidIDBadRequest asserts a malformed run id is 400, matching
// GET /runs/{id}. This runs without a pool (parse fails before any store call).
func TestListGrantsInvalidIDBadRequest(t *testing.T) {
	h := newHarness(t)
	w := do(t, h.srv, http.MethodGet, "/api/v1/runs/not-a-uuid/grants", adminToken, "")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("invalid id grants: code = %d, want 400; body=%s", w.Code, w.Body.String())
	}
}

// TestListGrantsRequiresAuth asserts the endpoint is under the admin-gated group
// (same middleware as GET /runs/{id}): no token → 401, before any store access.
func TestListGrantsRequiresAuth(t *testing.T) {
	h := newHarness(t)
	w := do(t, h.srv, http.MethodGet, "/api/v1/runs/"+uuid.NewString()+"/grants", "", "")
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("no token grants: code = %d, want 401", w.Code)
	}
}

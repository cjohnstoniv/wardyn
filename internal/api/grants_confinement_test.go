// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/runner"
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
// WARDYN_TEST_PG; skipped cleanly when unset (via pgHarnessWithRunner). Mirrors
// newHarness but with a live store so the create-run / grants paths (which
// require a real store) run.
func pgHarness(t *testing.T) (*Server, *pgxpool.Pool) {
	t.Helper()
	srv, pool := pgHarnessWithRunner(t, nil)
	srv.cfg.DefaultPolicy = types.RunPolicySpec{
		AllowedDomains:      []string{"api.anthropic.com"},
		MinConfinementClass: types.CC2,
		EligibleGrants: []types.GrantSpec{
			{Kind: types.GrantGitHubToken, Scope: json.RawMessage(`{"repos":["acme/widgets"]}`), RequiresApproval: true},
		},
	}
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
// resolves to the policy minimum (CC2) when there is no runner to probe —
// pgHarness wires Runner: nil, so strongestAdvertisedAtOrAbove has nothing
// advertised to raise it above the floor (0.7.8: with a real runner this now
// resolves to the STRONGEST advertised class instead; see
// TestCreateRun_ConfinementDefaultMatrix below for that behavior).
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
		t.Errorf("confinement_class = %q, want CC2 (no runner to advertise anything stronger)", run.ConfinementClass)
	}
}

// pgHarnessConfinement builds a pool-backed Server with a configurable runner
// and policy floor — pgHarnessWithRunner (interactive_test.go) hardcodes a CC2
// floor and no egress-domain policy shape this table needs, so the 0.7.8
// confinement-default matrix builds its own minimal harness rather than
// bending that one to a second purpose.
func pgHarnessConfinement(t *testing.T, r runner.Runner, floor types.ConfinementClass) *Server {
	t.Helper()
	srv, _ := pgHarnessWithRunner(t, r)
	srv.cfg.DefaultPolicy.MinConfinementClass = floor
	return srv
}

// TestCreateRun_ConfinementDefaultMatrix is 0.7.8's core behavior change,
// end-to-end through POST /runs: every row of the "Host has / Floor / Default
// / May pick" table, for both an unspecified and an explicit request.
func TestCreateRun_ConfinementDefaultMatrix(t *testing.T) {
	cases := []struct {
		name        string
		advertised  []types.ConfinementClass
		floor       types.ConfinementClass
		wantDefault types.ConfinementClass
	}{
		{"CC1 only, CC1 floor: default CC1 (stock install)", []types.ConfinementClass{types.CC1}, types.CC1, types.CC1},
		{"CC1+CC2, CC1 floor: default CC2 (strongest, not the floor)", []types.ConfinementClass{types.CC1, types.CC2}, types.CC1, types.CC2},
		{"CC1+CC2+CC3, CC1 floor: default CC3", []types.ConfinementClass{types.CC1, types.CC2, types.CC3}, types.CC1, types.CC3},
		{"admin floor CC2, CC1+CC2+CC3 installed: default CC3 (strongest >= floor)", []types.ConfinementClass{types.CC1, types.CC2, types.CC3}, types.CC2, types.CC3},
		{"Kata-only [CC1,CC3], CC1 floor: default CC3 (membership, not rank)", []types.ConfinementClass{types.CC1, types.CC3}, types.CC1, types.CC3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fr := &fakeRunner{capsClasses: tc.advertised}
			srv := pgHarnessConfinement(t, fr, tc.floor)

			// Unspecified request: resolves to the strongest advertised class
			// at or above the floor, never the bare floor.
			w := do(t, srv, http.MethodPost, "/api/v1/runs", adminToken,
				`{"agent":"claude-code","repo":"acme/widgets"}`)
			if w.Code != http.StatusCreated {
				t.Fatalf("unspecified request: code = %d, want 201; body=%s", w.Code, w.Body.String())
			}
			var run types.AgentRun
			if err := json.Unmarshal(w.Body.Bytes(), &run); err != nil {
				t.Fatalf("decode run: %v", err)
			}
			if run.ConfinementClass != tc.wantDefault {
				t.Errorf("unspecified request: confinement_class = %q, want %q", run.ConfinementClass, tc.wantDefault)
			}

			// Explicit request for every class the host advertises (the "May
			// pick" column): each one is accepted at or above the floor.
			for _, want := range tc.advertised {
				if !confinementGE(want, tc.floor) {
					continue // not a legal pick on this floor — covered by the reject case below
				}
				w := do(t, srv, http.MethodPost, "/api/v1/runs", adminToken,
					`{"agent":"claude-code","repo":"acme/widgets","confinement_class":"`+string(want)+`"}`)
				if w.Code != http.StatusCreated {
					t.Fatalf("explicit %s: code = %d, want 201; body=%s", want, w.Code, w.Body.String())
				}
				var got types.AgentRun
				if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
					t.Fatalf("decode run: %v", err)
				}
				if got.ConfinementClass != want {
					t.Errorf("explicit %s: confinement_class = %q, want %q (requested, never re-defaulted)", want, got.ConfinementClass, want)
				}
			}
		})
	}
}

// TestCreateRun_ExplicitFloorAboveAvailability422sByteIdentically: an admin
// floor (or an explicit request) the runner cannot structurally enforce still
// refuses byte-identically to the pre-0.7.8 behavior — strongestAdvertisedAtOrAbove
// falls back to the floor unchanged when nothing advertised meets it, so the
// membership check in resolveEnforcedConfinement is still what 422s it, not a
// silently accepted weaker class.
func TestCreateRun_ExplicitFloorAboveAvailability422sByteIdentically(t *testing.T) {
	fr := &fakeRunner{capsClasses: []types.ConfinementClass{types.CC1}}
	srv := pgHarnessConfinement(t, fr, types.CC2) // admin floor CC2, but only CC1 is installed
	w := do(t, srv, http.MethodPost, "/api/v1/runs", adminToken, `{"agent":"claude-code","repo":"acme/widgets"}`)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("code = %d, want 422; body=%s", w.Code, w.Body.String())
	}
	var body struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	const want = `runner "fake" cannot enforce confinement_class CC2 (available: CC1)`
	if body.Error != want {
		t.Errorf("error = %q, want %q", body.Error, want)
	}
}

// TestCreateRun_RequestBelowAdminFloorStill422s: an explicit request weaker
// than the deployment floor is refused regardless of what the runner
// advertises — the default-resolution rule never loosens the "never weaker
// than the policy minimum" rule for an EXPLICIT request.
func TestCreateRun_RequestBelowAdminFloorStill422s(t *testing.T) {
	fr := &fakeRunner{capsClasses: []types.ConfinementClass{types.CC1, types.CC2, types.CC3}}
	srv := pgHarnessConfinement(t, fr, types.CC2)
	w := do(t, srv, http.MethodPost, "/api/v1/runs", adminToken,
		`{"agent":"claude-code","repo":"acme/widgets","confinement_class":"CC1"}`)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("code = %d, want 422; body=%s", w.Code, w.Body.String())
	}
}

// TestCreateRun_BlastRadiusOverrideStillWins: a write-capable GitHub grant
// raises RequiredConfinementFloor to CC3 (composer's blast-radius rule) even
// when the strongest-advertised default would otherwise have picked a weaker
// class — the override applies AFTER the new default-resolution branch, not
// instead of it.
func TestCreateRun_BlastRadiusOverrideStillWins(t *testing.T) {
	fr := &fakeRunner{capsClasses: []types.ConfinementClass{types.CC1, types.CC2, types.CC3}}
	srv := pgHarnessConfinement(t, fr, types.CC1)
	srv.cfg.DefaultPolicy.EligibleGrants = []types.GrantSpec{
		{Kind: types.GrantGitHubToken, Scope: json.RawMessage(`{"repos":["acme/widgets"],"permissions":{"contents":"write"}}`), RequiresApproval: true},
	}
	w := do(t, srv, http.MethodPost, "/api/v1/runs", adminToken, `{"agent":"claude-code","repo":"acme/widgets"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("create run: code = %d, want 201; body=%s", w.Code, w.Body.String())
	}
	var run types.AgentRun
	if err := json.Unmarshal(w.Body.Bytes(), &run); err != nil {
		t.Fatalf("decode run: %v", err)
	}
	if run.ConfinementClass != types.CC3 {
		t.Errorf("confinement_class = %q, want CC3 (blast-radius override)", run.ConfinementClass)
	}
}

// TestPreflightAndCreateAgreeOnConfinementDefault is the behavioral twin of
// TestPreflightMirrorsLaunchGates (the structural AST guard): fires the SAME
// unspecified-confinement body through preflight and create, and asserts both
// resolve to the same defaulted class — Review and the real launch must never
// disagree about what a run will actually enforce.
func TestPreflightAndCreateAgreeOnConfinementDefault(t *testing.T) {
	fr := &fakeRunner{capsClasses: []types.ConfinementClass{types.CC1, types.CC2}}
	srv := pgHarnessConfinement(t, fr, types.CC1)
	body := `{"agent":"claude-code","repo":"acme/widgets"}`

	pre := do(t, srv, http.MethodPost, "/api/v1/runs/preflight", adminToken, body)
	if pre.Code != http.StatusOK {
		t.Fatalf("preflight: code = %d, want 200; body=%s", pre.Code, pre.Body.String())
	}
	var pf preflightResponse
	if err := json.Unmarshal(pre.Body.Bytes(), &pf); err != nil {
		t.Fatalf("decode preflight: %v", err)
	}

	create := do(t, srv, http.MethodPost, "/api/v1/runs", adminToken, body)
	if create.Code != http.StatusCreated {
		t.Fatalf("create: code = %d, want 201; body=%s", create.Code, create.Body.String())
	}
	var run types.AgentRun
	if err := json.Unmarshal(create.Body.Bytes(), &run); err != nil {
		t.Fatalf("decode run: %v", err)
	}

	if pf.EnforcedConfinementClass != types.CC2 || run.ConfinementClass != types.CC2 {
		t.Errorf("preflight = %q, create = %q, want both CC2 (strongest advertised)", pf.EnforcedConfinementClass, run.ConfinementClass)
	}
	if pf.EnforcedConfinementClass != run.ConfinementClass {
		t.Errorf("preflight (%q) and create (%q) disagree on the enforced class", pf.EnforcedConfinementClass, run.ConfinementClass)
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

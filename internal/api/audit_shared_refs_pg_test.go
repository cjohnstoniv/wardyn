// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/broker"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The broker's own rows and its own answer, with the real broker over real
// Postgres and every row read back through the real store. Resolving and
// minting a shared grant commits credential.mint success rows that carry the
// grant's scope whole; a grant the broker cannot mint records a failure row,
// and a mint after the run was revoked a denied one, both with the scope
// whole. The run's owner reads all of them without the organisation's secret
// name on both feed reads, an admin reads them as committed, and the mint
// answer itself — what the proxy relays into the sandbox — carries no name.
// Skips without WARDYN_TEST_PG.
func TestPG_Audit_AMemberNeverReadsASharedSecretsNameOnTheBrokersMintRows(t *testing.T) {
	const owner = "sub-pg-shared-audit"
	e := newOwnerOnlyPG(t)
	ctx := t.Context()
	pg := store.NewPG(e.pool)
	// The broker's refusals go where wardynd sends them: the audit table.
	e.h.srv.cfg.Broker = broker.New(broker.NewPgxStore(e.pool), e.sec, store.Recorder{Pool: e.pool}, e.h.idp, nil)
	e.h.srv.router = e.h.srv.routes()
	if err := e.sec.Put(ctx, "org-tool-token", []byte("operator-token-value")); err != nil {
		t.Fatal(err)
	}
	run, err := pg.CreateRun(ctx, types.AgentRun{ID: uuid.New(), CreatedBy: owner, Agent: "claude-code",
		ConfinementClass: types.CC1, State: types.RunPending, RunnerTarget: "docker", Task: "t"})
	if err != nil {
		t.Fatal(err)
	}
	grant := func(scope string) types.CredentialGrant {
		t.Helper()
		g, err := pg.CreateGrant(ctx, types.CredentialGrant{ID: uuid.New(), RunID: run.ID,
			Spec: types.GrantSpec{Kind: types.GrantAPIKey, Scope: json.RawMessage(scope)}})
		if err != nil {
			t.Fatal(err)
		}
		return g
	}
	g := grant(pgOrgScope)
	// A shared scope the broker cannot turn into a rule: it names no host.
	unmintable := grant(`{"host":"","require_tls":true,"secret_name":"org-tool-token","shared":true}`)
	token := mintRunTokenAs(t, e.h, run.ID, owner)
	mint := func(id uuid.UUID) (int, string) {
		w := do(t, e.h.srv, http.MethodPost, "/api/v1/internal/credentials/mint", token, `{"grant_id":"`+id.String()+`"}`)
		return w.Code, w.Body.String()
	}
	mintRowsBy := func(outcome string) []string {
		t.Helper()
		rows, err := e.pool.Query(ctx, `SELECT data::text FROM audit_events WHERE run_id = $1 AND action = 'credential.mint' AND outcome = $2 ORDER BY seq`, run.ID, outcome)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var d string
			if err := rows.Scan(&d); err != nil {
				t.Fatal(err)
			}
			out = append(out, d)
		}
		return out
	}

	// Success, twice: the sink's resolve and the mint door.
	rr := do(t, e.h.srv, http.MethodGet, "/api/v1/internal/injection/"+g.ID.String(), token, "")
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "Bearer operator-token-value") {
		t.Fatalf("resolve = %d %s, want the operator's value", rr.Code, rr.Body.String())
	}
	code, body := mint(g.ID)
	if code != http.StatusOK || !strings.Contains(body, "org-api.example") || !strings.Contains(body, `"kind":"api_key"`) {
		t.Fatalf("mint = %d %s, want the minted rule", code, body)
	}
	if strings.Contains(body, "org-tool-token") {
		t.Errorf("the mint answer names the organisation's secret: %s", body)
	}
	// Failure: the broker cannot build a rule for the second grant.
	if code, body := mint(unmintable.ID); code == http.StatusOK || strings.Contains(body, "org-tool-token") {
		t.Errorf("mint of a scope with no host = %d %s, want a refusal that names nothing", code, body)
	}
	// Denied: the run was revoked.
	if _, err := e.pool.Exec(ctx, `INSERT INTO identity_revocations (jti, run_id) VALUES ($1, $2)`, uuid.NewString(), run.ID); err != nil {
		t.Fatal(err)
	}
	if code, body := mint(g.ID); code == http.StatusOK || strings.Contains(body, "org-tool-token") {
		t.Errorf("mint for a revoked run = %d %s, want a refusal that names nothing", code, body)
	}
	for outcome, want := range map[string]int{"success": 2, "failure": 1, "denied": 1} {
		rows := mintRowsBy(outcome)
		if len(rows) != want {
			t.Fatalf("committed credential.mint %s rows = %v, want %d", outcome, rows, want)
		}
		for _, row := range rows {
			if !strings.Contains(row, `"secret_name": "org-tool-token"`) || !strings.Contains(row, `"shared": true`) {
				t.Fatalf("committed credential.mint %s row = %s, want the grant's scope whole", outcome, row)
			}
		}
	}

	member := ssoSession(t, owner, owner+"@corp.example", oidc.RoleUser)
	for _, path := range []string{
		"/api/v1/audit?run_id=" + run.ID.String(),
		"/api/v1/audit?run_id=" + run.ID.String() + "&action=credential.mint",
		"/api/v1/audit/export?run_id=" + run.ID.String(),
	} {
		w := doSSO(t, e.h.srv, http.MethodGet, path, member, "")
		body := w.Body.String()
		if w.Code != http.StatusOK || strings.Count(body, `"action":"credential.mint"`) != 4 {
			t.Fatalf("GET %s as the run's owner = %d %s, want the run's four mint rows", path, w.Code, body)
		}
		for _, want := range []string{`"outcome":"success"`, `"outcome":"failure"`, `"outcome":"denied"`, g.ID.String(), unmintable.ID.String(), "org-api.example"} {
			if !strings.Contains(body, want) {
				t.Errorf("GET %s as the run's owner lacks %s: %s", path, want, body)
			}
		}
		if strings.Contains(body, "org-tool-token") {
			t.Errorf("GET %s serves the organisation's secret name to the run's owner: %s", path, body)
		}
		w = doSSO(t, e.h.srv, http.MethodGet, path, e.admin, "")
		if body := w.Body.String(); w.Code != http.StatusOK || strings.Count(body, "org-tool-token") != 4 {
			t.Errorf("GET %s as an admin = %d %s, want the four rows as they were committed", path, w.Code, body)
		}
	}
}

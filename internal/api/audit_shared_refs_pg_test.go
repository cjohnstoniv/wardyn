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
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The broker's own row, written by the real broker into real Postgres and
// read back through the real store: resolving a shared grant commits a
// credential.mint row that carries the grant's scope whole, the run's owner
// reads that row without the organisation's secret name on both feed reads,
// and an admin reads it as it was committed. Skips without WARDYN_TEST_PG.
func TestPG_Audit_AMemberNeverReadsASharedSecretsNameOnTheBrokersMintRow(t *testing.T) {
	const owner = "sub-pg-shared-audit"
	e := newOwnerOnlyPG(t)
	ctx := t.Context()
	pg := store.NewPG(e.pool)
	if err := e.sec.Put(ctx, "org-tool-token", []byte("operator-token-value")); err != nil {
		t.Fatal(err)
	}
	run, err := pg.CreateRun(ctx, types.AgentRun{ID: uuid.New(), CreatedBy: owner, Agent: "claude-code",
		ConfinementClass: types.CC1, State: types.RunPending, RunnerTarget: "docker", Task: "t"})
	if err != nil {
		t.Fatal(err)
	}
	g, err := pg.CreateGrant(ctx, types.CredentialGrant{ID: uuid.New(), RunID: run.ID,
		Spec: types.GrantSpec{Kind: types.GrantAPIKey, Scope: json.RawMessage(pgOrgScope)}})
	if err != nil {
		t.Fatal(err)
	}
	rr := do(t, e.h.srv, http.MethodGet, "/api/v1/internal/injection/"+g.ID.String(), mintRunTokenAs(t, e.h, run.ID, owner), "")
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "Bearer operator-token-value") {
		t.Fatalf("resolve = %d %s, want the operator's value", rr.Code, rr.Body.String())
	}
	committed := e.mintRows(t)
	if len(committed) != 1 || !strings.Contains(string(mustJSON(committed[0])), `"secret_name":"org-tool-token"`) {
		t.Fatalf("committed credential.mint rows = %v, want one carrying the grant's scope whole", committed)
	}

	member := ssoSession(t, owner, owner+"@corp.example", oidc.RoleUser)
	for _, path := range []string{
		"/api/v1/audit?run_id=" + run.ID.String(),
		"/api/v1/audit?run_id=" + run.ID.String() + "&action=credential.mint",
		"/api/v1/audit/export?run_id=" + run.ID.String(),
	} {
		w := doSSO(t, e.h.srv, http.MethodGet, path, member, "")
		body := w.Body.String()
		if w.Code != http.StatusOK || !strings.Contains(body, "credential.mint") || !strings.Contains(body, g.ID.String()) ||
			!strings.Contains(body, "org-api.example") {
			t.Fatalf("GET %s as the run's owner = %d %s, want the run's mint row", path, w.Code, body)
		}
		if strings.Contains(body, "org-tool-token") {
			t.Errorf("GET %s serves the organisation's secret name to the run's owner: %s", path, body)
		}
		w = doSSO(t, e.h.srv, http.MethodGet, path, e.admin, "")
		if body := w.Body.String(); w.Code != http.StatusOK || !strings.Contains(body, "org-tool-token") {
			t.Errorf("GET %s as an admin = %d %s, want the row as it was committed", path, w.Code, body)
		}
	}
}

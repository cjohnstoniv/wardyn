// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestPG_OwnRowAdmittedGrantNeverReadsTheOperatorRow: a grant admitted only
// because its member owns a secret of that name (the capSecret exemption, and
// the own-key arm for a model host) is read from that member's row alone. Once
// the row is deleted, the mint and the injection sink refuse as for any
// owner_only grant without an own row, and never serve the operator's secret
// of the same name.
func TestPG_OwnRowAdmittedGrantNeverReadsTheOperatorRow(t *testing.T) {
	e := newOwnerOnlyPG(t)
	ctx := context.Background()
	e.h.srv.cfg.DefaultPolicy = types.RunPolicySpec{
		AllowedDomains:      []string{"api.anthropic.com", "git.corp.example"},
		MinConfinementClass: types.CC2,
		EligibleGrants: []types.GrantSpec{
			{Kind: types.GrantGitPAT, Scope: mustJSON(map[string]any{"host": "git.corp.example", "secret_name": "corp-key"})},
			{Kind: types.GrantAPIKey},
		},
	}
	if err := e.sec.Put(ctx, "corp-key", []byte("operator-corp-key")); err != nil {
		t.Fatal(err)
	}
	enforce := func(on bool) {
		t.Helper()
		if _, err := store.NewPG(e.pool).PutCapabilityEnforcement(ctx, map[string]bool{capSecret: on}); err != nil {
			t.Fatal(err)
		}
	}
	// launch has member sub store their own corp-key, launch with one grant
	// naming it, then delete the row; it returns the run's id and its grant.
	launch := func(sub, egress, grant string) (types.CredentialGrant, string) {
		t.Helper()
		member := ssoSession(t, sub, sub+"@corp.example", oidc.RoleUser)
		if w := doSSO(t, e.h.srv, http.MethodPut, "/api/v1/secrets/corp-key", member, `{"value":"`+sub+`-corp-key"}`); w.Code != http.StatusNoContent {
			t.Fatalf("PUT own secret: %d %s", w.Code, w.Body.String())
		}
		created := mustCreate(t, doSSO(t, e.h.srv, http.MethodPost, "/api/v1/runs", member,
			`{"agent":"claude-code","task":"t","inline_policy":{"min_confinement_class":"CC2","allowed_domains":["`+egress+`"],"eligible_grants":[`+grant+`]}}`))
		if w := doSSO(t, e.h.srv, http.MethodDelete, "/api/v1/secrets/corp-key", member, ""); w.Code != http.StatusNoContent {
			t.Fatalf("DELETE own secret: %d %s", w.Code, w.Body.String())
		}
		var run createRunResponse
		if err := json.Unmarshal(created.Body.Bytes(), &run); err != nil {
			t.Fatal(err)
		}
		grants, err := store.NewPG(e.pool).ListGrantsByRun(ctx, run.ID)
		if err != nil || len(grants) != 1 {
			t.Fatalf("run grants = %+v (%v), want the one inline grant", grants, err)
		}
		return grants[0], recordedRunToken(t, e.h, run.ID, sub)
	}

	t.Run("capSecret exemption: git_pat mint refuses once the own row is gone", func(t *testing.T) {
		enforce(true)
		g, token := launch("mallory", "git.corp.example", `{"kind":"git_pat","scope":{"host":"git.corp.example","secret_name":"corp-key"}}`)
		w := do(t, e.h.srv, http.MethodPost, "/api/v1/internal/credentials/mint", token, `{"grant_id":"`+g.ID.String()+`"}`)
		if w.Code == http.StatusOK || strings.Contains(w.Body.String(), "operator-corp-key") {
			t.Fatalf("mint = %d %s, want a refusal and never the operator's secret", w.Code, w.Body.String())
		}
	})

	t.Run("own-key arm: model-host injection refuses once the own row is gone", func(t *testing.T) {
		enforce(false)
		g, token := launch("trent", "api.anthropic.com", `{"kind":"api_key","scope":{"host":"api.anthropic.com","header":"x-api-key","format":"%s","secret_name":"corp-key"}}`)
		w := do(t, e.h.srv, http.MethodGet, "/api/v1/internal/injection/"+g.ID.String(), token, "")
		if w.Code != http.StatusFailedDependency || strings.Contains(w.Body.String(), "operator-corp-key") {
			t.Fatalf("injection = %d %s, want 424 not-found and never the operator's secret", w.Code, w.Body.String())
		}
	})
}

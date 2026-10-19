// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

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

// TestPG_SSHKeyGrantDecidesEachRefOnItsOwn: with capSecret enforced, a member
// who owns their ssh key and holds the org's grant for an operator known_hosts
// file launches an ssh_key grant naming both. The key binds to the member's own
// row, the known_hosts keeps the org grant's read, and the mint serves both.
// Once the own key is deleted the mint refuses and never serves the operator's
// key of the same name. A ref the member neither owns nor is granted (another
// person's key, an org known_hosts with no grant) drops the grant at launch.
func TestPG_SSHKeyGrantDecidesEachRefOnItsOwn(t *testing.T) {
	e := newOwnerOnlyPG(t)
	ctx := context.Background()
	pg := store.NewPG(e.pool)
	e.h.srv.cfg.DefaultPolicy = types.RunPolicySpec{
		AllowedDomains:      []string{"github.com"},
		MinConfinementClass: types.CC2,
	}
	// The ceiling pairs every ref below, so the capSecret check is what decides.
	for _, refs := range [][2]string{{"dev-ssh", "corp-kh"}, {"bob-ssh", "corp-kh"}, {"dev-ssh", "corp-kh-ungranted"}} {
		e.h.srv.cfg.DefaultPolicy.EligibleGrants = append(e.h.srv.cfg.DefaultPolicy.EligibleGrants, types.GrantSpec{Kind: types.GrantSSHKey,
			Scope: mustJSON(map[string]any{"host": "github.com", "key_secret_ref": refs[0], "known_hosts_secret_ref": refs[1]})})
	}
	for name, v := range map[string]string{"dev-ssh": "operator-dev-ssh", "corp-kh": "operator-corp-kh", "corp-kh-ungranted": "operator-ungranted-kh"} {
		if err := e.sec.Put(ctx, name, []byte(v)); err != nil {
			t.Fatal(err)
		}
	}
	if err := e.sec.For("bob").Put(ctx, "bob-ssh", []byte("bob-key")); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.PutCapabilityEnforcement(ctx, map[string]bool{capSecret: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.UpsertCapabilityGrant(ctx, types.CapabilityGrant{ID: uuid.New(), SubjectType: types.CapabilitySubjectUser,
		Subject: "alice", Capability: capSecret, Value: "corp-kh", Effect: types.CapabilityAllow, CreatedBy: "admin"}); err != nil {
		t.Fatal(err)
	}
	member := ssoSession(t, "alice", "alice@corp.example", oidc.RoleUser)
	if w := doSSO(t, e.h.srv, http.MethodPut, "/api/v1/secrets/dev-ssh", member, `{"value":"alice-key"}`); w.Code != http.StatusNoContent {
		t.Fatalf("PUT own secret: %d %s", w.Code, w.Body.String())
	}
	launch := func(keyRef, khRef string) (uuid.UUID, []types.CredentialGrant) {
		t.Helper()
		created := mustCreate(t, doSSO(t, e.h.srv, http.MethodPost, "/api/v1/runs", member,
			`{"agent":"claude-code","task":"t","inline_policy":{"min_confinement_class":"CC2","allowed_domains":["github.com"],"eligible_grants":[`+
				`{"kind":"ssh_key","scope":{"host":"github.com","key_secret_ref":"`+keyRef+`","known_hosts_secret_ref":"`+khRef+`"}}]}}`))
		var run createRunResponse
		if err := json.Unmarshal(created.Body.Bytes(), &run); err != nil {
			t.Fatal(err)
		}
		grants, err := pg.ListGrantsByRun(ctx, run.ID)
		if err != nil {
			t.Fatal(err)
		}
		return run.ID, grants
	}

	// mint launches the mixed grant and mints it once (a mint is single-use);
	// gone deletes the own key between the launch and the mint.
	mint := func(gone bool) *httptest.ResponseRecorder {
		t.Helper()
		runID, grants := launch("dev-ssh", "corp-kh")
		if len(grants) != 1 || grants[0].Spec.OwnerOnly || !slices.Equal(grants[0].Spec.OwnerOnlyRefs, []string{"dev-ssh"}) {
			t.Fatalf("mixed grant = %+v, want one grant binding only the own key", grants)
		}
		if gone {
			if w := doSSO(t, e.h.srv, http.MethodDelete, "/api/v1/secrets/dev-ssh", member, ""); w.Code != http.StatusNoContent {
				t.Fatalf("DELETE own secret: %d %s", w.Code, w.Body.String())
			}
		}
		return do(t, e.h.srv, http.MethodPost, "/api/v1/internal/credentials/mint", recordedRunToken(t, e.h, runID, "alice"),
			`{"grant_id":"`+grants[0].ID.String()+`"}`)
	}
	if w := mint(false); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "alice-key") || !strings.Contains(w.Body.String(), "operator-corp-kh") {
		t.Fatalf("mixed mint = %d %s, want the own key and the org known_hosts", w.Code, w.Body.String())
	}
	if w := mint(true); w.Code == http.StatusOK || strings.Contains(w.Body.String(), "operator-dev-ssh") {
		t.Fatalf("mint after the own key is gone = %d %s, want a refusal and never the operator's key", w.Code, w.Body.String())
	}
	if w := doSSO(t, e.h.srv, http.MethodPut, "/api/v1/secrets/dev-ssh", member, `{"value":"alice-key"}`); w.Code != http.StatusNoContent {
		t.Fatalf("PUT own secret: %d %s", w.Code, w.Body.String())
	}
	for name, refs := range map[string][2]string{
		"another person's key":           {"bob-ssh", "corp-kh"},
		"org known_hosts without grant": {"dev-ssh", "corp-kh-ungranted"},
	} {
		if _, grants := launch(refs[0], refs[1]); len(grants) != 0 {
			t.Errorf("%s: grants = %+v, want the grant dropped", name, grants)
		}
	}
}

// Copyright 2026 The Wardyn Authors
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
	"time"

	"filippo.io/age"
	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/adoscope"
	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	secretspg "github.com/cjohnstoniv/wardyn/internal/secretstore/pg"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// exitingRunner is fakeRunner whose agent exits 0 at once, so the completion
// watcher ends the run the way a finished task does.
type exitingRunner struct{ *fakeRunner }

func (exitingRunner) Wait(context.Context, string) (int, error) { return 0, nil }

// adoAdHocPG is one Postgres-backed server with a minted_pat Azure DevOps row
// and one member's captured sign-in: the deployment and person of every run in
// the tests below.
type adoAdHocPG struct {
	srv     *Server
	pg      store.PG
	pats    *fakeADOPATs
	runner  exitingRunner
	member  *http.Cookie
	subject string
}

func newADOAdHocPG(t *testing.T, baseURLs ...string) *adoAdHocPG {
	t.Helper()
	pool := throwawayPGPool(t)
	ctx := context.Background()
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	sec, err := secretspg.New(pool, id)
	if err != nil {
		t.Fatal(err)
	}
	// The sign-in a minted_pat row captures, written to the real secret store
	// under the member's own subject.
	f := newADOFixture(t)
	f.fake.SetClientSecret(mintTestSecret)
	f.fake.SetConsentedScopes(adoscope.MintScopes()...)
	f.cfg.Scopes = adoscope.MintScopes()
	f.cfg.TokenMode = types.ADOTokenModeMintedPAT
	f.cfg.ClientSecret = mintTestSecret
	f.srv.cfg.Secrets = sec
	f.srv.cfg.Now = time.Now
	subject := f.fake.Subject()
	if w := f.capture(t, subject); w.Code != http.StatusFound {
		t.Fatalf("capture: status %d body %q", w.Code, w.Body.String())
	}

	row := adoEntraTestRow()
	row.ID = f.cfg.RowID
	row.Entra.TenantID, row.Entra.ClientID = f.cfg.TenantID, f.cfg.ClientID
	row.Entra.TokenMode = types.ADOTokenModeMintedPAT
	if len(baseURLs) > 0 {
		row.BaseURLs = baseURLs
	}
	pg := store.NewPG(pool)
	if _, err := pg.PutSiteConfig(ctx, adoSite(row)); err != nil {
		t.Fatal(err)
	}

	h := newHarness(t)
	fx := &adoAdHocPG{srv: h.srv, pg: pg, pats: &fakeADOPATs{}, runner: exitingRunner{&fakeRunner{}}, subject: subject}
	h.srv.cfg.Store = pg
	h.srv.cfg.Secrets = sec
	h.srv.cfg.Audit = store.Recorder{Pool: pool} // the audit table itself, CHECK included
	h.srv.cfg.OIDC = &oidc.Authenticator{}
	h.srv.cfg.Runner = fx.runner
	h.srv.cfg.ADOEntra = func(context.Context) (ADOEntraConfig, bool, error) { return f.cfg, true, nil }
	h.srv.adoPATs = fx.pats
	h.srv.cfg.SessionRevocations = &pgTestRevocations{pool: pool, st: pg}
	h.srv.router = h.srv.routes()
	fx.member = ssoSession(t, subject, "member@corp.example", oidc.RoleUser)
	return fx
}

// launch creates one run as the member and waits for it to end.
func (fx *adoAdHocPG) launch(t *testing.T, body string) uuid.UUID {
	t.Helper()
	return fx.ended(t, doSSO(t, fx.srv, http.MethodPost, "/api/v1/runs", fx.member, body))
}

// ended reads a 201 and waits for its run to end.
func (fx *adoAdHocPG) ended(t *testing.T, w *httptest.ResponseRecorder) uuid.UUID {
	t.Helper()
	if w.Code != http.StatusCreated {
		t.Fatalf("POST /runs = %d %s", w.Code, w.Body.String())
	}
	var created createRunResponse
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the run to end", func() bool {
		run, err := fx.srv.cfg.Store.GetRun(context.Background(), created.ID)
		return err == nil && run.State.IsTerminal()
	})
	return created.ID
}

// tokens is GET /runs/{id}/ado-tokens as an admin (a service's run is not the member's).
func (fx *adoAdHocPG) tokens(t *testing.T, id uuid.UUID) []adoRunToken {
	t.Helper()
	admin := ssoSession(t, "sub-admin", "admin@corp.example", oidc.RoleAdmin)
	w := doSSO(t, fx.srv, http.MethodGet, "/api/v1/runs/"+id.String()+"/ado-tokens", admin, "")
	if w.Code != http.StatusOK {
		t.Fatalf("GET ado-tokens = %d %s", w.Code, w.Body.String())
	}
	var out []adoRunToken
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// skipRows is GET /audit for the run's run.ado_lane.skip rows, as an admin.
func (fx *adoAdHocPG) skipRows(t *testing.T, id uuid.UUID) []types.AuditEvent {
	t.Helper()
	admin := ssoSession(t, "sub-admin", "admin@corp.example", oidc.RoleAdmin)
	w := doSSO(t, fx.srv, http.MethodGet, "/api/v1/audit?run_id="+id.String()+"&action=run.ado_lane.skip", admin, "")
	if w.Code != http.StatusOK {
		t.Fatalf("GET audit = %d %s", w.Code, w.Body.String())
	}
	var rows []types.AuditEvent
	if err := json.Unmarshal(w.Body.Bytes(), &rows); err != nil {
		t.Fatalf("decode audit rows: %v (%s)", err, w.Body.String())
	}
	return rows
}

// adoRunBody is a POST /runs body with the run's own repo and/or a workspace.
func adoRunBody(repo, workspaceID string, ws []string, caps []string) string {
	pol := map[string]any{"min_confinement_class": "CC2", "allowed_domains": []string{"api.anthropic.com"}}
	if caps != nil {
		pol["azure_devops_capabilities"] = caps
	}
	if ws != nil {
		var repos []map[string]string
		for _, r := range ws {
			repos = append(repos, map[string]string{"repo": r})
		}
		pol["workspace_repos"] = repos
	}
	req := map[string]any{"agent": "claude-code", "task": "t", "inline_policy": pol}
	if repo != "" {
		req["repo"] = repo
	}
	if workspaceID != "" {
		req["workspace_id"] = workspaceID
	}
	b, _ := json.Marshal(req)
	return string(b)
}

// TestPG_ADOAdHocRepoRunGetsItsToken reproduces the reported A/B on one
// provider row and one person: three runs whose repository arrives as the
// run's own `repo`, and one whose repository arrives as an onboarded
// workspace. Every one mints a code-read token for its run and has it revoked
// when the run ends. Before the fix the three `repo` runs minted nothing and
// wrote no row, and only the workspace run minted.
func TestPG_ADOAdHocRepoRunGetsItsToken(t *testing.T) {
	fx := newADOAdHocPG(t)
	assertCodeRead := func(name string, id uuid.UUID) {
		t.Helper()
		// The watcher ends the run, then revokes: wait for the revoke.
		var toks []adoRunToken
		waitFor(t, name+"'s token to be revoked", func() bool {
			toks = fx.tokens(t, id)
			return len(toks) != 1 || toks[0].RevokedAt != nil
		})
		if len(toks) != 1 {
			// Errorf, not Fatalf: each run is its own verdict, so the unfixed tree
			// shows the three repo runs failing and the workspace run passing.
			t.Errorf("%s: /ado-tokens = %+v, want 1 entry", name, toks)
			return
		}
		if !slices.Equal(toks[0].Scope, []string{"vso.code"}) {
			t.Errorf("%s: scope %v, want [vso.code] only", name, toks[0].Scope)
		}
		if toks[0].RevokedAt == nil || toks[0].RevokeReason != adoPATRevokeRunEnd {
			t.Errorf("%s: revoked_at=%v reason=%q, want revoked at run end", name, toks[0].RevokedAt, toks[0].RevokeReason)
		}
		if rows := fx.skipRows(t, id); len(rows) != 0 {
			t.Errorf("%s: run.ado_lane.skip rows %+v, want none", name, rows)
		}
	}
	for i := range 3 {
		id := fx.launch(t, adoRunBody(adoTestRepo, "", nil, []string{"code_read"}))
		assertCodeRead("ad-hoc run "+string(rune('1'+i)), id)
	}

	// The onboarded-workspace route: the same repository, as a workspace.
	w := doSSO(t, fx.srv, http.MethodPost, "/api/v1/workspaces", fx.member,
		`{"name":"app","sources":[{"type":"repo","source":"`+adoTestRepo+`"}]}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("POST /workspaces = %d %s", w.Code, w.Body.String())
	}
	var ws struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &ws); err != nil || ws.ID == "" {
		t.Fatalf("decode workspace: %v (%s)", err, w.Body.String())
	}
	assertCodeRead("workspace run", fx.launch(t, adoRunBody("", ws.ID, nil, []string{"code_read"})))

	// The capabilities follow the run's policy, and the row's default without one.
	id := fx.launch(t, adoRunBody(adoTestRepo, "", nil, nil))
	if toks := fx.tokens(t, id); len(toks) != 1 || !slices.Contains(toks[0].Scope, "vso.code_write") {
		t.Errorf("no-list run: /ado-tokens = %+v, want one token at the row's default (code write)", toks)
	}
	for _, c := range fx.pats.creates {
		if !strings.Contains(c.Scope, "vso.code") {
			t.Errorf("a minted token's scope %q does not name the code area", c.Scope)
		}
	}
}

// onboard creates a workspace of one repository as the member.
func (fx *adoAdHocPG) onboard(t *testing.T, repo string) string {
	t.Helper()
	w := doSSO(t, fx.srv, http.MethodPost, "/api/v1/workspaces", fx.member,
		`{"name":"ws-`+uuid.NewString()[:8]+`","sources":[{"type":"repo","source":"`+repo+`"}]}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("POST /workspaces = %d %s", w.Code, w.Body.String())
	}
	var ws struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &ws); err != nil || ws.ID == "" {
		t.Fatalf("decode workspace: %v (%s)", err, w.Body.String())
	}
	return ws.ID
}

// T13 run 4: an onboarded workspace repository in one organisation and `repo`
// in another gets no credential, and says so in a stored row.
func TestPG_ADOCrossOrgRunDeclinesVisibly(t *testing.T) {
	fx := newADOAdHocPG(t, "https://dev.azure.com")
	wsID := fx.onboard(t, adoTestRepo)
	w := doSSO(t, fx.srv, http.MethodPost, "/api/v1/runs", fx.member,
		adoRunBody("https://dev.azure.com/fabrikam/proj/_git/app", wsID, nil, []string{"code_read"}))
	if !strings.Contains(w.Body.String(), adoDeclineCrossRepoConflict.sentence()) {
		t.Errorf("the 201 does not carry the decline sentence: %s", w.Body.String())
	}
	id := fx.ended(t, w)
	if toks := fx.tokens(t, id); len(toks) != 0 {
		t.Errorf("/ado-tokens = %+v, want none", toks)
	}
	rows := fx.skipRows(t, id)
	if len(rows) != 1 || rows[0].Outcome != "denied" || !strings.Contains(string(rows[0].Data), `"reason":"cross_repo_conflict"`) {
		t.Fatalf("run.ado_lane.skip rows = %+v, want one denied cross_repo_conflict row", rows)
	}
	if got := fx.runner.lastSandboxEnv()[adoLaneDeclinedEnv]; got != string(adoDeclineCrossRepoConflict) {
		t.Errorf("%s = %q, want cross_repo_conflict", adoLaneDeclinedEnv, got)
	}
}

// T21: a service token launches an admitted Azure DevOps run without the
// person's connection gate, and the run says why it holds no credential. The
// same draft says so at preflight, and through preview's own handler; a
// service token's preview request is still refused.
func TestPG_ServiceTokenADORunDeclinesNoPerson(t *testing.T) {
	fx := newADOAdHocPG(t)
	ctx := context.Background()
	if _, err := fx.pg.IssueLoginIdentity(ctx, store.LoginIdentity{Principal: fx.subject, Issuer: "https://idp.example",
		TenantID: "t", ObjectID: "o-1", Email: "member@corp.example"}, time.Now()); err != nil {
		t.Fatal(err)
	}
	si, err := fx.pg.CreateServiceIdentity(ctx, types.ServiceIdentity{Name: "ci-ado", Sponsor: fx.subject, UserType: types.UserTypeStandard, CreatedBy: "seed"})
	if err != nil {
		t.Fatal(err)
	}
	raw := newBearer(apiTokenPrefix)
	if _, err := fx.pg.CreateAPIToken(ctx, types.APIToken{Principal: si.Principal(), Role: oidc.RoleUser, UserType: types.UserTypeStandard, Name: "ci"}, raw); err != nil {
		t.Fatal(err)
	}
	body := adoRunBody(adoTestRepo, "", nil, []string{"code_read"})
	sentence := adoDeclineNoPerson.sentence()

	pre := do(t, fx.srv, http.MethodPost, "/api/v1/runs/preflight", raw, body)
	if pre.Code != http.StatusOK || !strings.Contains(pre.Body.String(), sentence) || strings.Contains(pre.Body.String(), `"git_credential"`) {
		t.Errorf("preflight = %d %s, want 200 with the no_person sentence and no connection fact", pre.Code, pre.Body.String())
	}
	if w := do(t, fx.srv, http.MethodPost, "/api/v1/runs/policy-preview", raw, body); w.Code != http.StatusForbidden {
		t.Errorf("a service token's preview = %d, want 403", w.Code)
	}
	// Preview's own handler, with the service identity on the context.
	r := httptest.NewRequest(http.MethodPost, "/api/v1/runs/policy-preview", strings.NewReader(body))
	r = r.WithContext(withServiceIdentity(withHumanIdentity(r.Context(), si.Principal(), "", oidc.RoleUser, types.UserTypeStandard, []string{}, false), si.Name, si.Sponsor))
	pw := httptest.NewRecorder()
	fx.srv.handlePolicyPreview(pw, r)
	var prev policyPreviewResponse
	if err := json.Unmarshal(pw.Body.Bytes(), &prev); err != nil || pw.Code != http.StatusOK ||
		prev.ADOLaneDeclined != string(adoDeclineNoPerson) || !slices.Contains(prev.Warnings, sentence) {
		t.Errorf("preview = %d %s, want ado_lane_declined no_person and its sentence", pw.Code, pw.Body.String())
	}

	w := do(t, fx.srv, http.MethodPost, "/api/v1/runs", raw, body)
	if !strings.Contains(w.Body.String(), sentence) {
		t.Errorf("the 201 does not carry the decline sentence: %s", w.Body.String())
	}
	id := fx.ended(t, w)
	if toks := fx.tokens(t, id); len(toks) != 0 || len(fx.pats.creates) != 0 {
		t.Errorf("/ado-tokens = %+v, mints %d, want none", toks, len(fx.pats.creates))
	}
	rows := fx.skipRows(t, id)
	if len(rows) != 1 || rows[0].Outcome != "denied" || !strings.Contains(string(rows[0].Data), `"reason":"no_person"`) {
		t.Fatalf("run.ado_lane.skip rows = %+v, want one denied no_person row", rows)
	}
	if got := fx.runner.lastSandboxEnv()[adoLaneDeclinedEnv]; got != string(adoDeclineNoPerson) {
		t.Errorf("%s = %q, want no_person", adoLaneDeclinedEnv, got)
	}
}

// T14: the lane's decline row and the three run.artifact.redirect rows a
// withheld token writes are rows the audit table stores (its outcome CHECK).
func TestPG_TheLanesAuditRowsAreStored(t *testing.T) {
	pool := throwawayPGPool(t)
	ctx := context.Background()
	audit := &memAudit{}
	srv := &Server{cfg: Config{Audit: audit, Now: time.Now, Secrets: &memSecrets{m: map[string][]byte{"model-tok": []byte("x"), feedToken: []byte("y")}}}}
	sc := feedSite()
	sc.EgressRedirects = append(sc.EgressRedirects,
		types.EgressRedirect{From: "https://pypi.org/simple/", TokenSecretRef: "model-tok", To: "https://api.anthropic.com/pypi/"},
		types.EgressRedirect{From: "https://proxy.golang.org/", Ecosystem: "go", TokenSecretRef: "absent-tok", To: "https://mirror.corp/go/"})
	run := types.AgentRun{ID: uuid.New()}
	srv.planArtifactRedirect(ctx, run, sc, []string{"registry.npmjs.org", "pypi.org", "proxy.golang.org"}, adoEntraHosts("contoso"))
	rows := audit.find("run.artifact.redirect")
	if len(rows) != 3 {
		t.Fatalf("run.artifact.redirect rows = %+v, want the model-host veto, the lane host and the unresolved token", rows)
	}
	rows = append(rows, adoDeclineAuditEvent(run.ID, adoDeclineCrossRepoConflict, time.Now().UTC()))
	for _, ev := range rows {
		ev := ev
		if err := store.InsertAuditEvent(ctx, pool, &ev); err != nil {
			t.Fatalf("insert %s/%s: %v", ev.Action, ev.Outcome, err)
		}
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE run_id = $1 AND outcome = 'denied'`, run.ID).Scan(&n); err != nil || n != 4 {
		t.Fatalf("stored denied rows = %d (%v), want 4", n, err)
	}
}

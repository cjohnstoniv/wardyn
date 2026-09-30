// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"filippo.io/age"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/broker"
	"github.com/cjohnstoniv/wardyn/internal/egress"
	secretspg "github.com/cjohnstoniv/wardyn/internal/secretstore/pg"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// ownerOnlyPG is a server over a real Postgres run store, a real pg secret
// store and the REAL broker, so a run's git_pat grant is created, persisted and
// minted exactly as wardynd does it. Skips without WARDYN_TEST_PG.
type ownerOnlyPG struct {
	h     *harness
	pool  *pgxpool.Pool
	sec   *secretspg.Store
	admin *http.Cookie
}

func newOwnerOnlyPG(t *testing.T) ownerOnlyPG {
	t.Helper()
	pool := throwawayPGPool(t)
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	sec, err := secretspg.New(pool, id)
	if err != nil {
		t.Fatal(err)
	}
	h := newHarness(t)
	h.srv.cfg.Store = store.NewPG(pool)
	h.srv.cfg.Secrets = sec
	h.srv.cfg.OIDC = &oidc.Authenticator{}
	h.srv.cfg.Broker = broker.New(broker.NewPgxStore(pool), sec, h.audit, h.idp, nil)
	h.srv.router = h.srv.routes()
	return ownerOnlyPG{h: h, pool: pool, sec: sec, admin: ssoSession(t, "root", "root@corp.example", oidc.RoleAdmin)}
}

// storePolicy has an admin store a named policy with one git_pat grant for
// secretName and returns its id; the admin's own namespace is the operator's.
func (e ownerOnlyPG) storePolicy(t *testing.T, name, secretName string, ownerOnly bool) string {
	t.Helper()
	return e.storePolicyFor(t, name, "dev.azure.com", secretName, ownerOnly)
}

// storePolicyFor is storePolicy for a git_pat grant on another host.
func (e ownerOnlyPG) storePolicyFor(t *testing.T, name, host, secretName string, ownerOnly bool) string {
	t.Helper()
	body := `{"name":"` + name + `","spec":{"min_confinement_class":"CC2","allowed_domains":["` + host + `"],` +
		`"eligible_grants":[{"kind":"git_pat","owner_only":` + strconv.FormatBool(ownerOnly) +
		`,"scope":{"host":"` + host + `","secret_name":"` + secretName + `"}}]}}`
	w := doSSO(t, e.h.srv, http.MethodPost, "/api/v1/policies", e.admin, body)
	if w.Code != http.StatusCreated {
		t.Fatalf("admin stores policy %s naming %q: %d, want 201: %s", name, secretName, w.Code, w.Body.String())
	}
	var p types.RunPolicy
	if err := json.Unmarshal(w.Body.Bytes(), &p); err != nil {
		t.Fatal(err)
	}
	return p.ID.String()
}

// createRun has member sub select the stored policy.
func (e ownerOnlyPG) createRun(t *testing.T, sub, policyID string) *httptest.ResponseRecorder {
	t.Helper()
	member := ssoSession(t, sub, sub+"@corp.example", oidc.RoleUser)
	return doSSO(t, e.h.srv, http.MethodPost, "/api/v1/runs", member,
		`{"agent":"claude-code","task":"t","policy_id":"`+policyID+`"}`)
}

// mint mints the run's one git_pat grant through the real mint route, as the
// run's own identity, and returns the response.
func (e ownerOnlyPG) mint(t *testing.T, sub string, created *httptest.ResponseRecorder) *httptest.ResponseRecorder {
	t.Helper()
	var run createRunResponse
	if err := json.Unmarshal(created.Body.Bytes(), &run); err != nil {
		t.Fatalf("decode create-run: %v; body=%s", err, created.Body.String())
	}
	grants, err := store.NewPG(e.pool).ListGrantsByRun(context.Background(), run.ID)
	if err != nil || len(grants) != 1 || grants[0].Spec.Kind != types.GrantGitPAT {
		t.Fatalf("run grants = %+v (%v), want the one git_pat grant", grants, err)
	}
	return do(t, e.h.srv, http.MethodPost, "/api/v1/internal/credentials/mint", recordedRunToken(t, e.h, run.ID, sub),
		`{"grant_id":"`+grants[0].ID.String()+`"}`)
}

// recordedRunToken mints the run's token as create did: the subject, and the
// operator_owned the run row recorded (never re-derived from the subject).
func recordedRunToken(t *testing.T, h *harness, runID uuid.UUID, sub string) string {
	t.Helper()
	run, err := h.srv.cfg.Store.GetRun(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	id, err := h.idp.MintRunIdentity(context.Background(), runID, sub, "", internalAudience, run.OperatorOwned)
	if err != nil {
		t.Fatal(err)
	}
	return id.Token
}

// mintRows is every successful credential.mint row the broker committed.
func (e ownerOnlyPG) mintRows(t *testing.T) []map[string]any {
	t.Helper()
	rows, err := e.pool.Query(context.Background(),
		`SELECT data FROM audit_events WHERE action='credential.mint' AND outcome='success' ORDER BY seq`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []map[string]any
	for rows.Next() {
		var d map[string]any
		if err := rows.Scan(&d); err != nil {
			t.Fatal(err)
		}
		out = append(out, d)
	}
	return out
}

func mintedToken(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	if w.Code != http.StatusOK {
		t.Fatalf("mint: %d, want 200: %s", w.Code, w.Body.String())
	}
	var m mintResponse
	if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
		t.Fatal(err)
	}
	return m.Token
}

// TestOwnerOnlyGrant_NeverServesTheOperatorRow is #1106 end to end: an
// operator row "x" exists and a member selects a stored policy whose git_pat
// grant names x. With owner_only and no row of her own the launch is refused
// with the reason; without the flag today's operator fallback holds (pinned,
// the change is additive); with her own row she gets exactly one mint, from it,
// and the mint row says so. A row deleted after launch is not replaced by the
// operator's at mint either.
func TestOwnerOnlyGrant_NeverServesTheOperatorRow(t *testing.T) {
	e := newOwnerOnlyPG(t)
	ctx := context.Background()
	if err := e.sec.Put(ctx, "x", []byte("operator-pat")); err != nil {
		t.Fatal(err)
	}
	strict := e.storePolicy(t, "strict", "x", true)
	// The unflagged fallback still holds for every forge but Azure DevOps.
	fallback := e.storePolicyFor(t, "fallback", "git.corp.example", "x", false)
	adoUnflagged := e.storePolicy(t, "ado-unflagged", "x", false)

	t.Run("owner_only and no own row: launch refused with the reason", func(t *testing.T) {
		w := e.createRun(t, "alice", strict)
		if w.Code != http.StatusUnprocessableEntity || !strings.Contains(w.Body.String(), `secret \"x\" is owner_only`) {
			t.Fatalf("create = %d %s, want 422 naming the owner_only grant", w.Code, w.Body.String())
		}
	})

	t.Run("no flag: today's operator fallback", func(t *testing.T) {
		before := len(e.mintRows(t))
		if got := mintedToken(t, e.mint(t, "alice", mustCreate(t, e.createRun(t, "alice", fallback)))); got != "operator-pat" {
			t.Fatalf("minted %q, want the operator row (the unflagged fallback is unchanged)", got)
		}
		rows := e.mintRows(t)
		if len(rows) != before+1 || rows[len(rows)-1]["secret_scope"] != "operator" {
			t.Errorf("mint rows = %v, want one more with secret_scope=operator", rows[before:])
		}
	})

	t.Run("an Azure DevOps host with no flag is owner_only anyway: refused at launch, never the operator's row (#1429)", func(t *testing.T) {
		w := e.createRun(t, "alice", adoUnflagged)
		if w.Code != http.StatusUnprocessableEntity || !strings.Contains(w.Body.String(), `secret \"x\" is owner_only`) {
			t.Fatalf("create = %d %s, want 422 naming the owner_only grant, as if the policy had said so", w.Code, w.Body.String())
		}
	})

	t.Run("an Azure DevOps host with no flag and an own row: mints the own row only", func(t *testing.T) {
		if err := e.sec.For("frank").Put(ctx, "x", []byte("frank-pat")); err != nil {
			t.Fatal(err)
		}
		if got := mintedToken(t, e.mint(t, "frank", mustCreate(t, e.createRun(t, "frank", adoUnflagged)))); got != "frank-pat" {
			t.Fatalf("minted %q, want frank's own row", got)
		}
	})

	t.Run("owner_only and an own row: exactly one mint, from it", func(t *testing.T) {
		if err := e.sec.For("bob").Put(ctx, "x", []byte("bob-pat")); err != nil {
			t.Fatal(err)
		}
		before := len(e.mintRows(t))
		if got := mintedToken(t, e.mint(t, "bob", mustCreate(t, e.createRun(t, "bob", strict)))); got != "bob-pat" {
			t.Fatalf("minted %q, want bob's own row", got)
		}
		rows := e.mintRows(t)
		if len(rows) != before+1 || rows[len(rows)-1]["secret_scope"] != "own" {
			t.Errorf("mint rows = %v, want exactly one more with secret_scope=own", rows[before:])
		}
	})

	t.Run("owner_only and the own row gone by mint: refused, never the operator's", func(t *testing.T) {
		if err := e.sec.For("carol").Put(ctx, "x", []byte("carol-pat")); err != nil {
			t.Fatal(err)
		}
		created := mustCreate(t, e.createRun(t, "carol", strict))
		if err := e.sec.For("carol").Delete(ctx, "x"); err != nil {
			t.Fatal(err)
		}
		w := e.mint(t, "carol", created)
		if w.Code == http.StatusOK || strings.Contains(w.Body.String(), "operator-pat") {
			t.Fatalf("mint = %d %s, want a refusal and never the operator's value", w.Code, w.Body.String())
		}
	})
}

// TestOwnerOnlyGrant_LegacyADOGrantWithOwnToken is the 0.8.1 upgrade: a stored
// policy wires the shared token as a git_pat grant for an Azure DevOps host, and
// the row now takes each person's own token. The grant is never carried by the
// pat lane, so it must not refuse the launch of a person who added their own
// token through the door; legacy open mode (no rows) still refuses.
func TestOwnerOnlyGrant_LegacyADOGrantWithOwnToken(t *testing.T) {
	e := newOwnerOnlyPG(t)
	ctx := context.Background()
	legacy := e.storePolicy(t, "legacy-ado", "git-pat-dev-azure-com", false)

	if w := e.createRun(t, "nobody", legacy); w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("legacy open mode: create = %d %s, want 422 (no rows, the owner_only rule holds)", w.Code, w.Body.String())
	}

	sc := types.SiteConfig{WorkspaceProviders: &types.WorkspaceProviders{Git: []types.GitProvider{{
		ID: "ado", Kind: types.GitProviderAzureDevOps, BaseURLs: []string{"https://dev.azure.com/acme"},
		Lanes: []types.GitLane{types.GitLaneEntra}, CredentialSource: types.CredentialSourcePerUser,
		Entra: &types.ADOEntraConfig{TokenMode: types.ADOTokenModeOwnPAT},
	}}}}
	if _, err := e.h.srv.cfg.Store.PutSiteConfig(ctx, sc); err != nil {
		t.Fatalf("PutSiteConfig: %v", err)
	}
	if w := e.createRun(t, "bob", legacy); w.Code != http.StatusUnprocessableEntity || !strings.Contains(w.Body.String(), "has not added their own Azure DevOps token") {
		t.Fatalf("no own token + legacy grant: create = %d %s, want 422 naming the missing token", w.Code, w.Body.String())
	}
	raw, _ := json.Marshal(adoOwnPATBlob{Token: "alice-own", Org: "acme", ExpiresOn: time.Now().AddDate(0, 0, 5)})
	if err := e.sec.For("alice").Put(ctx, adoOwnPATSecretName("ado"), raw); err != nil {
		t.Fatal(err)
	}
	if w := e.createRun(t, "alice", legacy); w.Code != http.StatusCreated {
		t.Fatalf("own token + legacy grant: create = %d %s, want 201", w.Code, w.Body.String())
	}
}

// TestStoredPolicy_SecretRefsResolvePerRunOwner is #1123: an admin stores a
// policy whose git_pat grant names a secret that exists in no namespace, which
// used to 422 against the admin's (operator) namespace. Existence is the run
// owner's, checked at run-create: a member with no row is refused there, and a
// member with one mints from it. An owner_only grant is stored the same way.
func TestStoredPolicy_SecretRefsResolvePerRunOwner(t *testing.T) {
	e := newOwnerOnlyPG(t)
	// A non-Azure DevOps host: an Azure DevOps grant is owner_only whatever the
	// policy says (#1429) and is covered in TestOwnerOnlyGrant_NeverServesTheOperatorRow.
	shared := e.storePolicyFor(t, "per-person", "git.corp.example", "nowhere", false)
	e.storePolicyFor(t, "per-person-strict", "git.corp.example", "nowhere", true)

	if w := e.createRun(t, "dave", shared); w.Code != http.StatusUnprocessableEntity || !strings.Contains(w.Body.String(), `unknown secret \"nowhere\"`) {
		t.Fatalf("member without a row: create = %d %s, want 422 at run-create", w.Code, w.Body.String())
	}
	if err := e.sec.For("erin").Put(context.Background(), "nowhere", []byte("erin-pat")); err != nil {
		t.Fatal(err)
	}
	if got := mintedToken(t, e.mint(t, "erin", mustCreate(t, e.createRun(t, "erin", shared)))); got != "erin-pat" {
		t.Fatalf("minted %q, want erin's own row", got)
	}
}

func mustCreate(t *testing.T, w *httptest.ResponseRecorder) *httptest.ResponseRecorder {
	t.Helper()
	if w.Code != http.StatusCreated {
		t.Fatalf("create run: %d, want 201: %s", w.Code, w.Body.String())
	}
	return w
}

// TestOwnerOnlyGrant_OperatorOwnedRunReadsTheOperatorRow is the lead's rule
// for a run with no person behind it (the admin token, local mode): it can
// store no row but the operator's, so its owner_only grant's own row is that
// one. An OIDC admin is a person: without a row of their own the launch is
// refused, and the sentence names the remedy (the user view).
func TestOwnerOnlyGrant_OperatorOwnedRunReadsTheOperatorRow(t *testing.T) {
	e := newOwnerOnlyPG(t)
	if err := e.sec.Put(context.Background(), "x", []byte("operator-pat")); err != nil {
		t.Fatal(err)
	}
	strict := e.storePolicy(t, "strict", "x", true)
	body := `{"agent":"claude-code","task":"t","policy_id":"` + strict + `"}`

	created := mustCreate(t, do(t, e.h.srv, http.MethodPost, "/api/v1/runs", adminToken, body))
	if got := mintedToken(t, e.mint(t, adminTokenPrincipal, created)); got != "operator-pat" {
		t.Fatalf("admin-token run minted %q, want the operator row (its own)", got)
	}
	if rows := e.mintRows(t); len(rows) != 1 || rows[0]["secret_scope"] != "operator" {
		t.Errorf("mint rows = %v, want one with secret_scope=operator", rows)
	}

	// The admin session itself must launch from the user view (#639: an SSO
	// session of admin tier in the Admin view is refused 409 admin_view before
	// any owner_only check runs); the point here is the OIDC admin identity
	// versus the admin token, not the view gate.
	uv := doSSO(t, e.h.srv, http.MethodPost, "/api/v1/me/view", e.admin, `{"view":"user"}`)
	if uv.Code != http.StatusOK {
		t.Fatalf("admin switches to the user view: %d, want 200: %s", uv.Code, uv.Body.String())
	}
	adminInUserView := sessionCookieFrom(t, uv.Result().Cookies())

	w := doSSO(t, e.h.srv, http.MethodPost, "/api/v1/runs", adminInUserView, body)
	if w.Code != http.StatusUnprocessableEntity || !strings.Contains(w.Body.String(), "from the user view") {
		t.Fatalf("OIDC admin without an own row: create = %d %s, want 422 naming the user view", w.Code, w.Body.String())
	}
}

// TestOwnerOnlyGrant_IdPSubSpelledLikeTheAdminTokenIsRefused: a signed-in
// person whose IdP sub is literally "admin-token", member or admin, was once
// a person the owner_only gate had to tell from the operator (#1106); since
// #1162 that session authenticates nothing, so it launches no run and no mint
// happens. The real admin token still reads the operator row
// (TestOwnerOnlyGrant_OperatorOwnedRunReadsTheOperatorRow), and a run such a
// session created earlier is pinned at the sink
// (TestOwnerOnlyInjectionSink_NeverServesTheOperatorRow).
func TestOwnerOnlyGrant_IdPSubSpelledLikeTheAdminTokenIsRefused(t *testing.T) {
	e := newOwnerOnlyPG(t)
	if err := e.sec.Put(context.Background(), "x", []byte("operator-pat")); err != nil {
		t.Fatal(err)
	}
	strict := e.storePolicy(t, "strict", "x", true)
	for _, role := range []string{oidc.RoleUser, oidc.RoleAdmin} {
		impostor := ssoSession(t, adminTokenPrincipal, "impostor-"+role+"@corp.example", role)
		w := doSSO(t, e.h.srv, http.MethodPost, "/api/v1/runs", impostor, `{"agent":"claude-code","task":"t","policy_id":"`+strict+`"}`)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("%s with sub %q: create = %d %s, want 401 — a reserved principal is no identity", role, adminTokenPrincipal, w.Code, w.Body.String())
		}
	}
	if rows := e.mintRows(t); len(rows) != 0 {
		t.Errorf("mint rows = %v, want none", rows)
	}
}

// TestOwnerOnlyInjectionSink_NeverServesTheOperatorRow pins #1106 at the
// api_key sink, which reads the value itself: a person's owner_only grant with
// only an operator row is refused, unflagged it keeps the fallback, an
// operator-owned run reads the operator row as its own, and a person's own row
// is served.
func TestOwnerOnlyInjectionSink_NeverServesTheOperatorRow(t *testing.T) {
	h, sec := newRunOwnerPGHarness(t)
	if err := sec.Put(context.Background(), "vendor-key", []byte("operator-key")); err != nil {
		t.Fatal(err)
	}
	// The sink serves only a live run, so each subject gets one, under a
	// default policy with no grants of its own.
	h.srv.cfg.DefaultPolicy = types.RunPolicySpec{AllowedDomains: []string{"api.anthropic.com"}, MinConfinementClass: types.CC2}
	// "impostor" is a person whose IdP sub is "admin-token", and whose run
	// predates #1162 (which now refuses that session at the door): launched
	// as a person, then its created_by rewritten to the sub it carried.
	runs, subs := map[string]uuid.UUID{}, map[string]string{"alice": "alice", "admin": adminTokenPrincipal, "impostor": adminTokenPrincipal}
	for who := range subs {
		var w *httptest.ResponseRecorder
		if who == "admin" {
			w = do(t, h.srv, http.MethodPost, "/api/v1/runs", adminToken, `{"agent":"claude-code","task":"t"}`)
		} else {
			w = doSSO(t, h.srv, http.MethodPost, "/api/v1/runs", ssoSession(t, who, who+"@corp.example", oidc.RoleUser), `{"agent":"claude-code","task":"t"}`)
		}
		var run createRunResponse
		if err := json.Unmarshal(mustCreate(t, w).Body.Bytes(), &run); err != nil {
			t.Fatal(err)
		}
		runs[who] = run.ID
	}
	if _, err := h.srv.cfg.Store.(store.PG).Pool.Exec(context.Background(),
		`UPDATE agent_runs SET created_by = $1 WHERE id = $2`, adminTokenPrincipal, runs["impostor"]); err != nil {
		t.Fatal(err)
	}
	resolve := func(who string, ownerOnly bool) *httptest.ResponseRecorder {
		t.Helper()
		h.broker.minted = broker.Minted{
			Kind: types.GrantAPIKey, JTI: "jti-" + who,
			Injection: &egress.InjectionRule{Host: "api.vendor.example", Header: "x-api-key", SecretName: "vendor-key", Format: "%s"},
			OwnerOnly: ownerOnly,
		}
		return do(t, h.srv, http.MethodGet, "/api/v1/internal/injection/"+uuid.NewString(), recordedRunToken(t, h, runs[who], subs[who]), "")
	}
	served := func(w *httptest.ResponseRecorder) string {
		t.Helper()
		var resp injectionResponse
		if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &resp) != nil {
			return fmt.Sprintf("(%d %s)", w.Code, w.Body.String())
		}
		return resp.Value
	}

	if w := resolve("alice", true); w.Code != http.StatusFailedDependency || strings.Contains(w.Body.String(), "operator-key") {
		t.Fatalf("owner_only, no own row: %d %s, want 424 not-found and never the operator key", w.Code, w.Body.String())
	}
	if got := served(resolve("alice", false)); got != "operator-key" {
		t.Fatalf("unflagged: served %q, want today's operator fallback", got)
	}
	if got := served(resolve("admin", true)); got != "operator-key" {
		t.Fatalf("admin-token run: served %q, want the operator row (its own)", got)
	}
	if w := resolve("impostor", true); w.Code != http.StatusFailedDependency || strings.Contains(w.Body.String(), "operator-key") {
		t.Fatalf("person with sub %q: %d %s, want 424 and never the operator key", adminTokenPrincipal, w.Code, w.Body.String())
	}
	if err := sec.For("alice").Put(context.Background(), "vendor-key", []byte("alice-key")); err != nil {
		t.Fatal(err)
	}
	if got := served(resolve("alice", true)); got != "alice-key" {
		t.Fatalf("owner_only, own row: served %q, want alice's", got)
	}
}

// TestOwnerOnlyEnvSecret_NeverServesTheOperatorRow pins #1106 on env_secret
// dispatch, and the secret_scope datum on run.env_secret.resolve.
func TestOwnerOnlyEnvSecret_NeverServesTheOperatorRow(t *testing.T) {
	h, sec := newRunOwnerPGHarness(t)
	ctx := context.Background()
	if err := sec.Put(ctx, "vendor-token", []byte("operator-token")); err != nil {
		t.Fatal(err)
	}
	resolve := func(sub string, operatorOwned, ownerOnly bool) (string, map[string]any) {
		t.Helper()
		policy := types.RunPolicySpec{EligibleGrants: []types.GrantSpec{{
			Kind: types.GrantEnvSecret, OwnerOnly: ownerOnly,
			Scope: json.RawMessage(`{"name":"VENDOR_TOKEN","secret_name":"vendor-token"}`),
		}}}
		env := map[string]string{}
		h.srv.resolveEnvSecretGrants(ctx, types.AgentRun{ID: uuid.New(), CreatedBy: sub, OperatorOwned: operatorOwned}, policy, env)
		h.audit.mu.Lock()
		defer h.audit.mu.Unlock()
		return env["VENDOR_TOKEN"], auditData(t, lastAuditEvent(t, h.audit.events, "run.env_secret.resolve"))
	}

	if v, d := resolve("alice", false, true); v != "" || !strings.Contains(fmt.Sprint(d["reason"]), "owner_only") {
		t.Fatalf("owner_only, no own row: env=%q data=%v, want it skipped with the owner_only reason", v, d)
	}
	if v, d := resolve("alice", false, false); v != "operator-token" || d["secret_scope"] != "operator" {
		t.Fatalf("unflagged: env=%q data=%v, want the operator fallback with secret_scope=operator", v, d)
	}
	if v, d := resolve(adminTokenPrincipal, true, true); v != "operator-token" || d["secret_scope"] != "operator" {
		t.Fatalf("operator-owned run: env=%q data=%v, want the operator row (its own)", v, d)
	}
	if v, d := resolve(adminTokenPrincipal, false, true); v != "" {
		t.Fatalf("a person's run with sub %q: env=%q data=%v, want it skipped", adminTokenPrincipal, v, d)
	}
	if err := sec.For("alice").Put(ctx, "vendor-token", []byte("alice-token")); err != nil {
		t.Fatal(err)
	}
	if v, d := resolve("alice", false, true); v != "alice-token" || d["secret_scope"] != "own" {
		t.Fatalf("owner_only, own row: env=%q data=%v, want alice's with secret_scope=own", v, d)
	}
}

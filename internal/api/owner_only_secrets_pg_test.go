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
	body := `{"name":"` + name + `","spec":{"min_confinement_class":"CC2","allowed_domains":["dev.azure.com"],` +
		`"eligible_grants":[{"kind":"git_pat","owner_only":` + strconv.FormatBool(ownerOnly) +
		`,"scope":{"host":"dev.azure.com","secret_name":"` + secretName + `"}}]}}`
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
	return do(t, e.h.srv, http.MethodPost, "/api/v1/internal/credentials/mint", mintRunTokenAs(t, e.h, run.ID, sub),
		`{"grant_id":"`+grants[0].ID.String()+`"}`)
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
	fallback := e.storePolicy(t, "fallback", "x", false)

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

// TestStoredPolicy_SecretRefsResolvePerRunOwner is #1123: an admin stores a
// policy whose git_pat grant names a secret that exists in no namespace, which
// used to 422 against the admin's (operator) namespace. Existence is the run
// owner's, checked at run-create: a member with no row is refused there, and a
// member with one mints from it. An owner_only grant is stored the same way.
func TestStoredPolicy_SecretRefsResolvePerRunOwner(t *testing.T) {
	e := newOwnerOnlyPG(t)
	shared := e.storePolicy(t, "per-person", "nowhere", false)
	e.storePolicy(t, "per-person-strict", "nowhere", true)

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

	w := doSSO(t, e.h.srv, http.MethodPost, "/api/v1/runs", e.admin, body)
	if w.Code != http.StatusUnprocessableEntity || !strings.Contains(w.Body.String(), "from the user view") {
		t.Fatalf("OIDC admin without an own row: create = %d %s, want 422 naming the user view", w.Code, w.Body.String())
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
	runs := map[string]uuid.UUID{}
	for _, sub := range []string{"alice", adminTokenPrincipal} {
		var w *httptest.ResponseRecorder
		if sub == adminTokenPrincipal {
			w = do(t, h.srv, http.MethodPost, "/api/v1/runs", adminToken, `{"agent":"claude-code","task":"t"}`)
		} else {
			w = doSSO(t, h.srv, http.MethodPost, "/api/v1/runs", ssoSession(t, sub, sub+"@corp.example", oidc.RoleUser), `{"agent":"claude-code","task":"t"}`)
		}
		var run createRunResponse
		if err := json.Unmarshal(mustCreate(t, w).Body.Bytes(), &run); err != nil {
			t.Fatal(err)
		}
		runs[sub] = run.ID
	}
	resolve := func(sub string, ownerOnly bool) *httptest.ResponseRecorder {
		t.Helper()
		h.broker.minted = broker.Minted{
			Kind: types.GrantAPIKey, JTI: "jti-" + sub,
			Injection: &egress.InjectionRule{Host: "api.vendor.example", Header: "x-api-key", SecretName: "vendor-key", Format: "%s"},
			OwnerOnly: ownerOnly,
		}
		return do(t, h.srv, http.MethodGet, "/api/v1/internal/injection/"+uuid.NewString(), mintRunTokenAs(t, h, runs[sub], sub), "")
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
	if got := served(resolve(adminTokenPrincipal, true)); got != "operator-key" {
		t.Fatalf("operator-owned run: served %q, want the operator row (its own)", got)
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
	resolve := func(sub string, ownerOnly bool) (string, map[string]any) {
		t.Helper()
		policy := types.RunPolicySpec{EligibleGrants: []types.GrantSpec{{
			Kind: types.GrantEnvSecret, OwnerOnly: ownerOnly,
			Scope: json.RawMessage(`{"name":"VENDOR_TOKEN","secret_name":"vendor-token"}`),
		}}}
		env := map[string]string{}
		h.srv.resolveEnvSecretGrants(ctx, types.AgentRun{ID: uuid.New(), CreatedBy: sub}, policy, env)
		h.audit.mu.Lock()
		defer h.audit.mu.Unlock()
		return env["VENDOR_TOKEN"], auditData(t, lastAuditEvent(t, h.audit.events, "run.env_secret.resolve"))
	}

	if v, d := resolve("alice", true); v != "" || !strings.Contains(fmt.Sprint(d["reason"]), "owner_only") {
		t.Fatalf("owner_only, no own row: env=%q data=%v, want it skipped with the owner_only reason", v, d)
	}
	if v, d := resolve("alice", false); v != "operator-token" || d["secret_scope"] != "operator" {
		t.Fatalf("unflagged: env=%q data=%v, want the operator fallback with secret_scope=operator", v, d)
	}
	if v, d := resolve(adminTokenPrincipal, true); v != "operator-token" || d["secret_scope"] != "operator" {
		t.Fatalf("operator-owned run: env=%q data=%v, want the operator row (its own)", v, d)
	}
	if err := sec.For("alice").Put(ctx, "vendor-token", []byte("alice-token")); err != nil {
		t.Fatal(err)
	}
	if v, d := resolve("alice", true); v != "alice-token" || d["secret_scope"] != "own" {
		t.Fatalf("owner_only, own row: env=%q data=%v, want alice's with secret_scope=own", v, d)
	}
}

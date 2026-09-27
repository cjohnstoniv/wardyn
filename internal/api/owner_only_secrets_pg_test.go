// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"filippo.io/age"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/broker"
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

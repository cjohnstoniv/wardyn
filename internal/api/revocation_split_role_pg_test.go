// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/db"
)

// splitRolePool creates an app role on owner's database holding exactly the grants the split-role runbook
// (docs/OPERATIONS.md, "Run this as the role you have today") gives it, applied after every migration, and
// returns a pool that serves as that role.
func splitRolePool(t *testing.T, owner *pgxpool.Pool) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()
	raw, err := os.ReadFile("../../docs/OPERATIONS.md")
	if err != nil {
		t.Fatal(err)
	}
	_, rest, ok := strings.Cut(string(raw), "Run this as the role you have today")
	if !ok {
		t.Fatal("docs/OPERATIONS.md no longer has the split-role runbook")
	}
	_, rest, _ = strings.Cut(rest, "```sql\n")
	runbook, _, ok := strings.Cut(rest, "```")
	if !ok || !strings.Contains(runbook, "CREATE ROLE wardyn_app") {
		t.Fatal("the split-role runbook's SQL block was not found")
	}
	role := "wardyn_app_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
	runbook = strings.ReplaceAll(strings.ReplaceAll(runbook, "wardyn_app", role), "'…'", "'pw'")
	if _, err := owner.Exec(ctx, runbook); err != nil {
		t.Fatalf("the split-role runbook: %v", err)
	}
	t.Cleanup(func() {
		_, _ = owner.Exec(context.Background(), `DROP OWNED BY `+role)
		_, _ = owner.Exec(context.Background(), `DROP ROLE IF EXISTS `+role)
	})
	var name string
	if err := owner.QueryRow(ctx, `SELECT current_database()`).Scan(&name); err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(os.Getenv("WARDYN_TEST_PG"))
	if err != nil {
		t.Fatal(err)
	}
	u.User, u.Path = url.UserPassword(role, "pw"), "/"+name
	app, err := db.Connect(ctx, u.String())
	if err != nil {
		t.Fatalf("connect as %s: %v", role, err)
	}
	t.Cleanup(app.Close)
	return app
}

// A split-role install serves as an app role that holds only the runbook's grants, nothing on the revocation
// sequence: a sign-in records a revocation sequence, a token mint works, and a revoke and a sign-out
// everywhere write their cuts and end the sessions issued before them.
func TestPG_SplitRoleOrdersRevocationsWithOnlyTheDocumentedGrants(t *testing.T) {
	owner := throwawayPGPool(t)
	app := splitRolePool(t, owner)
	ctx := context.Background()
	var super, seqUsage bool
	if err := app.QueryRow(ctx, `SELECT rolsuper, has_sequence_privilege(current_user, 'oidc_revocation_seq', 'USAGE')
		FROM pg_roles WHERE rolname = current_user`).Scan(&super, &seqUsage); err != nil {
		t.Fatal(err)
	}
	if super || seqUsage {
		t.Fatalf("premise: the app role is superuser %v, holds USAGE on the sequence %v; want neither", super, seqUsage)
	}
	e := newSCIMEnvOn(t, app)
	admin := accessSession(t, "sec", "sec@corp.example", oidc.RoleSecurityAdmin, []string{})

	signIn := func() *http.Cookie {
		t.Helper()
		c := sessionCookieOf(e.signIn(e.a, "sub-alice", "alice@corp.example"))
		if c == nil {
			t.Fatal("sign-in as the app role gave no session")
		}
		var seq int64
		reissued(t, c, func(s *oidc.Session) { seq = s.RevocationSeq })
		if seq < 1 {
			t.Fatalf("the session recorded revocation sequence %d, want a position", seq)
		}
		return c
	}
	first := signIn()
	w := doSSO(t, e.a.srv, http.MethodPost, "/api/v1/me/tokens", first, `{"name":"ci"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("token mint as the app role = %d %s", w.Code, w.Body.String())
	}
	token := decodeToken(t, w).Token
	if !e.tokenWorks(e.a, token) {
		t.Fatal("control: the minted token does not work")
	}
	if w := doSSO(t, e.b.srv, http.MethodPost, "/api/v1/sessions/revoke", admin, `{"sub":"sub-alice"}`); w.Code != http.StatusNoContent {
		t.Fatalf("revoke as the app role = %d %s", w.Code, w.Body.String())
	}
	if e.cookieWorks(e.a, first) || e.tokenWorks(e.a, token) {
		t.Error("a session or token issued before the revoke still works")
	}

	second := signIn()
	if !e.cookieWorks(e.a, second) {
		t.Fatal("a session issued after the revoke was refused")
	}
	if w := doSSO(t, e.b.srv, http.MethodPost, "/api/v1/sessions/revoke", admin, `{"sub":"sub-alice","sessions_only":true}`); w.Code != http.StatusNoContent {
		t.Fatalf("sign out everywhere as the app role = %d %s", w.Code, w.Body.String())
	}
	if e.cookieWorks(e.a, second) {
		t.Error("a session issued before the sign-out everywhere still works")
	}
}

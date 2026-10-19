// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// What a person's erasure reaches beyond the copies person_erasure_pg_test.go pins: the approvals
// they decided, their runs' Azure DevOps token history and their delegated tokens, and the audit
// subject a later rebind could hand to someone else. Guarded by WARDYN_TEST_PG.

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

func TestPG_PersonErasure_ReachesApprovalsTokenHistoryAndTheAuditSubject(t *testing.T) {
	l := newMaskLab(t)
	fail := false
	a := erasureReplica(l, &fail)
	ctx := t.Context()
	pg := store.NewPG(l.pool)
	const alice, bob = "alice-sub", "bob-sub"
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := l.pool.Exec(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	count := func(q string, args ...any) int {
		t.Helper()
		var n int
		if err := l.pool.QueryRow(ctx, q, args...).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	// Approvals on bob's run, one decided by alice with a typed reason, one by bob.
	run := l.personRun(bob, "bob plan")
	aliceAp, bobAp := uuid.New(), uuid.New()
	for id, by := range map[uuid.UUID]string{aliceAp: alice, bobAp: bob} {
		exec(`INSERT INTO approvals (id, run_id, kind, requested_scope, state, decided_at, decided_by, reason)
			VALUES ($1, $2, 'egress_domain', $3, 'APPROVED', now(), $4, $5)`, id, run.ID, `{"host":"`+id.String()+`.example"}`, by, "ok per "+by+"'s ticket")
	}

	// Token history: a revoked and an expired token of alice's, a revoked one of bob's, and a token of
	// alice's still live because Azure DevOps could not be asked to revoke it (the sweep retries it).
	a.srv.cfg.ADOEntra = func(context.Context) (ADOEntraConfig, bool, error) { return ADOEntraConfig{}, false, nil }
	a.srv.cfg.ADOEntraByRow = func(context.Context, string) (ADOEntraConfig, bool, error) {
		return ADOEntraConfig{}, false, errors.New("injected: the sign-in configuration is unreadable")
	}
	past, ahead := time.Now().UTC().Add(-time.Hour), time.Now().UTC().Add(time.Hour)
	pats := map[string]uuid.UUID{}
	for _, p := range []struct {
		key, owner string
		revoked    bool
		validTo    time.Time
	}{{"alice-revoked", alice, true, past}, {"alice-expired", alice, false, past}, {"bob-revoked", bob, true, past}, {"alice-live", alice, false, ahead}} {
		pat := store.RunPAT{RunID: uuid.New(), AuthorizationID: uuid.New(), Owner: p.owner, ProviderRowID: "row", Org: "contoso",
			Scope: "vso.code", ValidTo: p.validTo}
		if err := pg.InsertRunPAT(ctx, pat); err != nil {
			t.Fatal(err)
		}
		if p.revoked {
			if _, err := pg.MarkRunPATRevoked(ctx, pat.RunID, pat.AuthorizationID, adoPATRevokeDisconnect, ""); err != nil {
				t.Fatal(err)
			}
		}
		pats[p.key] = pat.AuthorizationID
	}
	delegate := uuid.New()
	exec(`INSERT INTO delegates (id, name, idp_client_id, scope_group, credential_sha256) VALUES ($1, 'portal', 'client', 'group', 'sha-delegate')`, delegate)
	for i, who := range []string{alice, alice, bob} {
		exec(`INSERT INTO delegated_tokens (id, delegate_id, token_sha256, principal, email, user_type, groups_truncated, expires_at)
			VALUES ($1, $2, $3, $4, $4 || '@corp.example', 'member', false, now() + interval '10 minutes')`,
			uuid.New(), delegate, "sha-token-"+string(rune('a'+i)), who)
	}

	// Alice's audit subject: her identity row.
	if _, err := pg.UpsertLoginIdentity(ctx, store.LoginIdentity{Principal: alice, Issuer: "https://dex.example", Email: "alice@corp.example"}, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	subject, found, err := pg.AuditSubjectFor(ctx, alice)
	if err != nil || !found {
		t.Fatalf("alice's audit subject = %q, %v, %v", subject, found, err)
	}

	sec := ssoSession(t, "sec-sub", "sec@corp.example", oidc.RoleSecurityAdmin)
	w := doSSO(t, a.srv, http.MethodPost, "/api/v1/people/"+alice+"/erasure", sec, erasureBody("audit_personal_fields", "credentials"))
	if w.Code != http.StatusOK {
		t.Fatalf("erasure = %d %s", w.Code, w.Body)
	}

	var by, reason, state string
	if err := l.pool.QueryRow(ctx, `SELECT decided_by, reason, state FROM approvals WHERE id = $1`, aliceAp).Scan(&by, &reason, &state); err != nil {
		t.Fatal(err)
	}
	if by != "" || reason != "" || state != string(types.ApprovalApproved) {
		t.Errorf("alice's decision = (%q, %q, %q), want her name and reason cleared and the state kept", by, reason, state)
	}
	if n := count(`SELECT count(*) FROM approvals WHERE id = $1 AND decided_by = $2 AND reason <> ''`, bobAp, bob); n != 1 {
		t.Error("bob's decision was touched by alice's erasure")
	}
	for key, want := range map[string]int{"alice-revoked": 0, "alice-expired": 0, "bob-revoked": 1, "alice-live": 1} {
		if n := count(`SELECT count(*) FROM ado_run_pats WHERE authorization_id = $1`, pats[key]); n != want {
			t.Errorf("token row %s: %d left, want %d", key, n, want)
		}
	}
	if n := count(`SELECT count(*) FROM delegated_tokens WHERE principal = $1`, alice); n != 0 {
		t.Errorf("alice's delegated tokens: %d left, want none", n)
	}
	if n := count(`SELECT count(*) FROM delegated_tokens WHERE principal = $1`, bob); n != 1 {
		t.Errorf("bob's delegated tokens: %d left, want his one", n)
	}
	if n := count(`SELECT count(*) FROM principal_keys WHERE owner = $1 AND purpose = 'audit-seal' AND destroyed_at IS NOT NULL`,
		"subject:"+subject); n != 1 {
		t.Errorf("destroyed audit-seal generations under alice's subject = %d, want 1", n)
	}
}

// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package db

import (
	"context"
	"testing"

	"github.com/google/uuid"
)

// TestMigrate_AttachTicketCredentialPurgesOnlyRefreshLeftovers: 0274 deletes the revoked cli_access rows that
// earlier refreshes left behind, and no other row: not a session's first token, not a token revoked by its
// session's end, not a live token, a cli_admin token or a personal token.
func TestMigrate_AttachTicketCredentialPurgesOnlyRefreshLeftovers(t *testing.T) {
	pool := pgPoolIsolated(t)
	ctx := context.Background()
	session := func(ended bool) uuid.UUID {
		t.Helper()
		id := uuid.New()
		endedAt, reason := "NULL", "NULL"
		if ended {
			endedAt, reason = "now() - interval '1 hour'", "'signed_out'"
		}
		if _, err := pool.Exec(ctx, `INSERT INTO cli_sessions (id, principal, role, user_type, machine, store, public_key, secret_sha256, expires_at, ended_at, end_reason)
			VALUES ($1, 'sub-a', 'user', 'standard', 'laptop', 'keychain', $2, $3, now() + interval '1 day', `+endedAt+`, `+reason+`)`,
			id, make([]byte, 32), uuid.NewString()); err != nil {
			t.Fatalf("seed session: %v", err)
		}
		return id
	}
	// token seeds a row created age hours ago, revoked at the given SQL time ("" = live).
	token := func(sess *uuid.UUID, kind, createdAgo, revoked string) uuid.UUID {
		t.Helper()
		id := uuid.New()
		rev := "NULL"
		if revoked != "" {
			rev = revoked
		}
		if _, err := pool.Exec(ctx, `INSERT INTO api_tokens (id, principal, role, user_type, name, token_sha256, created_at, revoked_at, kind, cli_session_id)
			VALUES ($1, 'sub-a', 'user', 'standard', 'n', $2, now() - interval '`+createdAgo+`', `+rev+`, $3, $4)`,
			id, uuid.NewString(), kind, sess); err != nil {
			t.Fatalf("seed token: %v", err)
		}
		return id
	}
	// audited writes an audit row naming id by action, as the revoke door does for a token revoked on purpose.
	audited := func(action string, id uuid.UUID) uuid.UUID {
		t.Helper()
		if _, err := pool.Exec(ctx, `SELECT audit_append(gen_random_uuid(), now(), NULL, 'human', 'sub-a', $1::text, $2::text, 'success', '', NULL)`,
			action, id.String()); err != nil {
			t.Fatalf("seed audit row: %v", err)
		}
		return id
	}
	live, ended := session(false), session(true)
	want := map[string]struct {
		id   uuid.UUID
		keep bool
	}{
		"live: first token, revoked":            {token(&live, "cli_access", "5 hours", "now() - interval '4 hours'"), true},
		"live: refreshed, revoked by a refresh": {token(&live, "cli_access", "4 hours", "now() - interval '3 hours'"), false},
		"live: refreshed, current":              {token(&live, "cli_access", "1 hour", ""), true},
		"live: cli_admin, revoked":              {token(&live, "cli_admin", "3 hours", "now() - interval '2 hours'"), true},
		"ended: first token, revoked at end":    {token(&ended, "cli_access", "6 hours", "now() - interval '1 hour'"), true},
		"ended: refreshed, revoked by refresh":  {token(&ended, "cli_access", "5 hours", "now() - interval '3 hours'"), false},
		"ended: refreshed, revoked at end":      {token(&ended, "cli_access", "3 hours", "now() - interval '1 hour'"), true},
		"personal, revoked":                     {token(nil, "api", "9 hours", "now() - interval '2 hours'"), true},
		"personal, live":                        {token(nil, "api", "9 hours", ""), true},
		"live: refreshed, revoked on purpose":   {audited("token.revoke", token(&live, "cli_access", "2 hours", "now() - interval '30 minutes'")), true},
	}
	sql, err := migrationFS.ReadFile("migrations/0274_attach_ticket_credential.sql")
	if err != nil {
		t.Fatal(err)
	}
	for range 2 { // running it again changes nothing more
		if _, err := pool.Exec(ctx, string(sql)); err != nil {
			t.Fatalf("apply 0274: %v", err)
		}
	}
	for name, w := range want {
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM api_tokens WHERE id = $1`, w.id).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if (n == 1) != w.keep {
			t.Errorf("%s: kept=%v, want %v", name, n == 1, w.keep)
		}
	}
}

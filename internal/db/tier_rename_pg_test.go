// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package db

import (
	"context"
	"testing"

	"github.com/google/uuid"
)

// TestPG_TierRenameRewritesEveryStoredMember applies the tier rename over a
// database holding a 0.7 row in each of the four places "member" was stored,
// then checks no column still holds it and neither CHECK takes it back.
func TestPG_TierRenameRewritesEveryStoredMember(t *testing.T) {
	const renameFloor = "0070"
	pool, schema := partialSchemaPool(t, renameFloor)
	ctx := context.Background()

	memberTok, adminTok := uuid.New(), uuid.New()
	for _, s := range []struct {
		what, q string
		args    []any
	}{
		{"member mapping", `INSERT INTO role_mappings (id, value, role) VALUES (gen_random_uuid(), 'eng-team', 'member')`, nil},
		{"admin mapping", `INSERT INTO role_mappings (id, value, role) VALUES (gen_random_uuid(), 'ops-team', 'admin')`, nil},
		{"member token", `INSERT INTO api_tokens (id, principal, token_sha256, role) VALUES ($1, 'm@example.com', $2, 'member')`,
			[]any{memberTok, uuid.NewString()}},
		{"admin token", `INSERT INTO api_tokens (id, principal, token_sha256, role) VALUES ($1, 'a@example.com', $2, 'admin')`,
			[]any{adminTok, uuid.NewString()}},
		// The 0043 column default, as every pre-0.6 key was backfilled.
		{"member key", `INSERT INTO ssh_public_keys (fingerprint, principal, public_key) VALUES ('SHA256:m', 'm@example.com', 'k')`, nil},
		{"member ticket", `INSERT INTO attach_tickets (token_sha256, run_id, actor_type, principal, expires_at, role)
			VALUES ('t-m', gen_random_uuid(), 'human', 'm@example.com', now() + interval '1 minute', 'member')`, nil},
	} {
		if _, err := pool.Exec(ctx, s.q, s.args...); err != nil {
			t.Fatalf("seed %s in %s: %v", s.what, schema, err)
		}
	}

	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("Migrate applying %s+ over 0.7 rows: %v", renameFloor, err)
	}

	var role string
	var userType *string
	if err := pool.QueryRow(ctx, `SELECT role, user_type FROM role_mappings WHERE value = 'eng-team'`).Scan(&role, &userType); err != nil {
		t.Fatalf("read migrated mapping: %v", err)
	}
	if role != "user" || userType == nil || *userType != "standard" {
		t.Errorf("member mapping = role %q, user_type %v; want user on the built-in standard type", role, userType)
	}
	if err := pool.QueryRow(ctx, `SELECT role, user_type FROM role_mappings WHERE value = 'ops-team'`).Scan(&role, &userType); err != nil {
		t.Fatalf("read admin mapping: %v", err)
	}
	if role != "admin" || userType != nil {
		t.Errorf("admin mapping = role %q, user_type %v; want it untouched", role, userType)
	}

	for _, c := range []struct{ what, q, want string }{
		{"member token", `SELECT role FROM api_tokens WHERE id = '` + memberTok.String() + `'`, "user"},
		{"admin token", `SELECT role FROM api_tokens WHERE id = '` + adminTok.String() + `'`, "admin"},
		{"member key", `SELECT role FROM ssh_public_keys WHERE fingerprint = 'SHA256:m'`, "user"},
		{"member ticket", `SELECT role FROM attach_tickets WHERE token_sha256 = 't-m'`, "user"},
	} {
		if err := pool.QueryRow(ctx, c.q).Scan(&role); err != nil {
			t.Fatalf("read %s: %v", c.what, err)
		}
		if role != c.want {
			t.Errorf("%s role = %q, want %q", c.what, role, c.want)
		}
	}

	// The retired word is refused where a CHECK guards it, and the fail-closed
	// defaults now write the new one.
	if _, err := pool.Exec(ctx, `INSERT INTO role_mappings (id, value, role) VALUES (gen_random_uuid(), 'late', 'member')`); err == nil {
		t.Error("role_mappings accepted role 'member' after the rename")
	}
	if _, err := pool.Exec(ctx, `INSERT INTO api_tokens (id, principal, token_sha256, role) VALUES (gen_random_uuid(), 'x', $1, 'member')`,
		uuid.NewString()); err == nil {
		t.Error("api_tokens accepted role 'member' after the rename")
	}
	if err := pool.QueryRow(ctx, `INSERT INTO api_tokens (id, principal, token_sha256) VALUES (gen_random_uuid(), 'd', $1) RETURNING role`,
		uuid.NewString()).Scan(&role); err != nil || role != "user" {
		t.Errorf("api_tokens default role = %q (%v), want user", role, err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO ssh_public_keys (fingerprint, principal, public_key) VALUES ('SHA256:d', 'd', 'k') RETURNING role`).
		Scan(&role); err != nil || role != "user" {
		t.Errorf("ssh_public_keys default role = %q (%v), want user", role, err)
	}

	// A type a saved mapping names cannot be deleted out from under it.
	if _, err := pool.Exec(ctx, `INSERT INTO user_types (id, name) VALUES ('analyst', 'Analyst')`); err != nil {
		t.Fatalf("seed a custom type: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO role_mappings (id, value, role, user_type) VALUES (gen_random_uuid(), 'quant', 'user', 'analyst')`); err != nil {
		t.Fatalf("map a value to the custom type: %v", err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM user_types WHERE id = 'analyst'`); err == nil {
		t.Error("deleted a user type a role mapping still names")
	}
	if _, err := pool.Exec(ctx, `INSERT INTO role_mappings (id, value, role, user_type) VALUES (gen_random_uuid(), 'ghost', 'user', 'no-such-type')`); err == nil {
		t.Error("a role mapping named a user type that does not exist")
	}
}

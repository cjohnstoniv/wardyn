// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package db

import (
	"context"
	"testing"

	"github.com/google/uuid"
)

// TestPG_TierRenameOverNonEmptyData is the UT-2a (tier rename, #608) x #584
// (capped SSH keys) intersection: it seeds a 0.7-shaped 'member' row in every
// place the old tier word was stored, including a CAPPED key (#584's own
// scenario), applies the rename migration over that non-empty data, and then
// checks the two things TestPG_TierRenameRewritesEveryStoredMember does not:
// the fail-closed column DEFAULTs actually moved to the new word (not just
// what a fresh insert happens to receive), and a NEW capped registration —
// the exact write #584 added — still succeeds under the renamed role rather
// than being caught by the re-created CHECK along with the row it was meant
// to admit.
func TestPG_TierRenameOverNonEmptyData(t *testing.T) {
	const renameFloor = "0074"
	pool, schema := partialSchemaPool(t, renameFloor)
	ctx := context.Background()

	tokID := uuid.New()
	for _, s := range []struct {
		what, q string
		args    []any
	}{
		{"role mapping", `INSERT INTO role_mappings (id, value, role) VALUES (gen_random_uuid(), 'contractors', 'member')`, nil},
		{"api token", `INSERT INTO api_tokens (id, principal, token_sha256, role) VALUES ($1, 'm@example.com', $2, 'member')`,
			[]any{tokID, uuid.NewString()}},
		{"attach ticket", `INSERT INTO attach_tickets (token_sha256, run_id, actor_type, principal, expires_at, role)
			VALUES ('t-nonempty', gen_random_uuid(), 'human', 'm@example.com', now() + interval '1 minute', 'member')`, nil},
		// The 0070 scenario: a key registered through the user view, capped.
		{"capped ssh key", `INSERT INTO ssh_public_keys (fingerprint, principal, public_key, role, capped)
			VALUES ('SHA256:nonempty-capped', 'm@example.com', 'k', 'member', true)`, nil},
	} {
		if _, err := pool.Exec(ctx, s.q, s.args...); err != nil {
			t.Fatalf("seed %s in %s: %v", s.what, schema, err)
		}
	}

	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("Migrate applying %s+ over non-empty 0.7 data: %v", renameFloor, err)
	}

	// The seeded rows read 'user', not 'member'.
	for _, c := range []struct{ what, q, want string }{
		{"role mapping", `SELECT role FROM role_mappings WHERE value = 'contractors'`, "user"},
		{"api token", `SELECT role FROM api_tokens WHERE id = '` + tokID.String() + `'`, "user"},
		{"attach ticket", `SELECT role FROM attach_tickets WHERE token_sha256 = 't-nonempty'`, "user"},
		{"capped ssh key", `SELECT role FROM ssh_public_keys WHERE fingerprint = 'SHA256:nonempty-capped'`, "user"},
	} {
		var role string
		if err := pool.QueryRow(ctx, c.q).Scan(&role); err != nil {
			t.Fatalf("read migrated %s: %v", c.what, err)
		}
		if role != c.want {
			t.Errorf("%s role = %q, want %q", c.what, role, c.want)
		}
	}

	// The fail-closed column DEFAULTs moved with everything else, not just
	// what happens to land on a fresh insert: 0043 and 0045 chose DEFAULT
	// 'member' specifically so an insert omitting role could never end up
	// admin, and that posture is worthless if the rename left the stale word
	// sitting in pg_attrdef while every STORED row was rewritten around it.
	for _, c := range []struct{ table, column, want string }{
		{"api_tokens", "role", "'user'::text"},
		{"ssh_public_keys", "role", "'user'::text"},
	} {
		var def *string
		if err := pool.QueryRow(ctx, `
			SELECT column_default FROM information_schema.columns
			WHERE table_schema = $1 AND table_name = $2 AND column_name = $3`,
			schema, c.table, c.column).Scan(&def); err != nil {
			t.Fatalf("read %s.%s column_default: %v", c.table, c.column, err)
		}
		if def == nil || *def != c.want {
			t.Errorf("%s.%s DEFAULT = %v, want %q — the fail-closed backfill default still writes the retired word",
				c.table, c.column, def, c.want)
		}
	}

	// A NEW capped registration — #584's own write — still succeeds after the
	// rename: the re-created CHECK must admit the row it exists to allow, not
	// just refuse the ones it exists to refuse.
	if _, err := pool.Exec(ctx, `
		INSERT INTO ssh_public_keys (fingerprint, principal, public_key, role, capped)
		VALUES ('SHA256:nonempty-fresh-capped', 'new@example.com', 'k', 'user', true)`); err != nil {
		t.Errorf("a fresh capped key insert with role 'user' was refused after the rename: %v", err)
	}

	// The control: capped + the OLD word is refused (the column CHECK, not
	// just the cap, has moved on), and capped + a PRIVILEGED role is refused
	// (the cap itself still holds).
	if _, err := pool.Exec(ctx, `
		INSERT INTO ssh_public_keys (fingerprint, principal, public_key, role, capped)
		VALUES ('SHA256:nonempty-stale-word', 'x@example.com', 'k', 'member', true)`); err == nil {
		t.Error("ssh_public_keys accepted role 'member' on a fresh insert after the rename")
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO ssh_public_keys (fingerprint, principal, public_key, role, capped)
		VALUES ('SHA256:nonempty-capped-admin', 'y@example.com', 'k', 'admin', true)`); err == nil {
		t.Error("ssh_public_keys accepted a CAPPED row with role 'admin' after the rename — the cap no longer holds")
	}
}

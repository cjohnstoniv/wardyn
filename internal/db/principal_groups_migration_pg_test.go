// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package db

import (
	"fmt"
	"testing"
	"time"
)

const principalGroupsMigration = "0290"

// TestMigration0290_FillsTheRecordFromTheLastSignIns applies the sign-in record migration over people
// who signed in before it: each key-domain login snapshot becomes a record of a sign-in to Wardyn itself
// (portal "") with its groups, its truncation bit and its time, and the email of the identity row its
// person last signed in with, not the one made last. A person with no identity row gets no email; one
// with no snapshot gets no record.
func TestMigration0290_FillsTheRecordFromTheLastSignIns(t *testing.T) {
	pool, _ := partialSchemaPool(t, principalGroupsMigration)
	at := time.Now().UTC().Truncate(time.Second)
	pgExec(t, pool, `INSERT INTO key_domain_login_groups (principal, groups, truncated, verified_at) VALUES
		('sub-ann', '["eng","ops"]', false, $1), ('sub-ben', '[]', true, $1)`, at)
	// The identity she last signed in with was made first, so made-last cannot stand in for it.
	pgExec(t, pool, `INSERT INTO principal_identities (principal, issuer, email_lower, last_login_at, created_at) VALUES
		('sub-ann', 'https://new.example', 'ann@corp.example', $2, $2 - interval '3 days'),
		('sub-ann', 'https://never.example', 'ann.never@corp.example', NULL, $2 - interval '2 days'),
		('sub-ann', 'https://old.example', 'ann.old@corp.example', $1, $2 - interval '1 day'),
		('sub-cat', 'https://new.example', 'cat@corp.example', $2, $2)`, at.Add(-time.Hour), at)

	migrateBefore(t, pool, "0291")

	if got := pgScalar[string](t, pool, `SELECT portal || '|' || email || '|' || groups::text || '|' || truncated::text FROM principal_groups WHERE principal = 'sub-ann'`); got != `|ann@corp.example|["eng", "ops"]|false` {
		t.Errorf("ann's record = %s, want a sign-in to Wardyn itself with the newest sign-in's email and her groups, complete", got)
	}
	if got := pgScalar[time.Time](t, pool, `SELECT signed_in_at FROM principal_groups WHERE principal = 'sub-ann'`); !got.Equal(at) {
		t.Errorf("ann's record time = %s, want the snapshot's %s", got, at)
	}
	if got := pgScalar[string](t, pool, `SELECT email || '|' || groups::text || '|' || truncated::text FROM principal_groups WHERE principal = 'sub-ben'`); got != `|[]|true` {
		t.Errorf("ben's record = %s, want no email, no groups and partial", got)
	}
	if n := pgScalar[int](t, pool, `SELECT count(*)::int FROM principal_groups WHERE principal = 'sub-cat'`); n != 0 {
		t.Errorf("cat has %d records, want none: no login snapshot to fill one from", n)
	}
}

// TestMigration0290_GrantsTheAppRole: on a split-role install the migration itself gives the table to the
// role that already writes and clears key-domain login snapshots, read from the catalog, so the upgrade
// needs no hand grant; a role that may only read those snapshots gets nothing, and nobody gets TRUNCATE.
func TestMigration0290_GrantsTheAppRole(t *testing.T) {
	pool, schema := partialSchemaPool(t, principalGroupsMigration)
	app, _ := auditRole(t, pgPool(t), "wardyn_pg_app")
	reader, _ := auditRole(t, pgPool(t), "wardyn_pg_reader")
	for _, role := range []string{app, reader} {
		pgExec(t, pool, fmt.Sprintf(`GRANT USAGE ON SCHEMA %s TO %s`, schema, role))
	}
	pgExec(t, pool, `GRANT SELECT, INSERT, UPDATE, DELETE ON key_domain_login_groups TO `+app)
	pgExec(t, pool, `GRANT SELECT ON key_domain_login_groups TO `+reader)

	migrateBefore(t, pool, "0291")

	for priv, want := range map[string]bool{"SELECT": true, "INSERT": true, "UPDATE": true, "DELETE": true, "TRUNCATE": false} {
		if got := pgScalar[bool](t, pool, `SELECT has_table_privilege($1, 'principal_groups', $2)`, app, priv); got != want {
			t.Errorf("app role %s on principal_groups = %v, want %v", priv, got, want)
		}
		if pgScalar[bool](t, pool, `SELECT has_table_privilege($1, 'principal_groups', $2)`, reader, priv) {
			t.Errorf("a role that only reads key_domain_login_groups holds %s on principal_groups", priv)
		}
	}
}

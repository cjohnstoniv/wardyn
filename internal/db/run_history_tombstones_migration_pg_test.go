// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package db

import (
	"fmt"
	"testing"
)

const runHistoryTombstonesMigration = "0400"

// TestMigration_RunHistoryTombstones_PG: the migration grants SELECT, INSERT and UPDATE (person erasure blanks a
// tombstone's creator) on the tombstone table to the role that already writes agent_runs, found from the catalog,
// and nothing more; a role that only reads agent_runs gets
// nothing, and the grants on agent_runs itself are unchanged (DELETE on it stays the operator's grant).
func TestMigration_RunHistoryTombstones_PG(t *testing.T) {
	pool, schema := partialSchemaPool(t, runHistoryTombstonesMigration)
	app, _ := auditRole(t, pgPool(t), "wardyn_pg_app")
	reader, _ := auditRole(t, pgPool(t), "wardyn_pg_reader")
	for _, role := range []string{app, reader} {
		pgExec(t, pool, fmt.Sprintf(`GRANT USAGE ON SCHEMA %s TO %s`, schema, role))
	}
	pgExec(t, pool, `GRANT SELECT, INSERT, UPDATE, DELETE ON agent_runs TO `+app)
	pgExec(t, pool, `GRANT SELECT ON agent_runs TO `+reader)
	const runsACL = `SELECT coalesce(relacl::text, '') FROM pg_catalog.pg_class WHERE oid = 'agent_runs'::regclass`
	before := pgScalar[string](t, pool, runsACL)

	migrateBefore(t, pool, "0401")

	for priv, want := range map[string]bool{"SELECT": true, "INSERT": true, "UPDATE": true, "DELETE": false, "TRUNCATE": false} {
		if got := pgScalar[bool](t, pool, `SELECT has_table_privilege($1, 'run_history_tombstones', $2)`, app, priv); got != want {
			t.Errorf("app role %s on run_history_tombstones = %v, want %v", priv, got, want)
		}
		if pgScalar[bool](t, pool, `SELECT has_table_privilege($1, 'run_history_tombstones', $2)`, reader, priv) {
			t.Errorf("a role that only reads agent_runs holds %s on run_history_tombstones", priv)
		}
	}
	if after := pgScalar[string](t, pool, runsACL); after != before {
		t.Errorf("the migration changed the grants on agent_runs: %q, was %q", after, before)
	}
}

// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package db

import (
	"fmt"
	"testing"
)

const runnerEventRefusalWindowsMigration = "0291"

// TestMigration0291_GrantsTheAppRole: on a split-role install the migration itself gives the refusal windows to
// the role that already moves runner spool cursors, which the same refusal transaction writes, read from the
// catalog, so the upgrade needs no hand grant; a role that may only read the cursors gets nothing, and nobody gets
// DELETE or TRUNCATE.
func TestMigration0291_GrantsTheAppRole(t *testing.T) {
	pool, schema := partialSchemaPool(t, runnerEventRefusalWindowsMigration)
	app, _ := auditRole(t, pgPool(t), "wardyn_pg_app")
	reader, _ := auditRole(t, pgPool(t), "wardyn_pg_reader")
	for _, role := range []string{app, reader} {
		pgExec(t, pool, fmt.Sprintf(`GRANT USAGE ON SCHEMA %s TO %s`, schema, role))
	}
	pgExec(t, pool, `GRANT SELECT, INSERT, UPDATE, DELETE ON runner_spool_cursors TO `+app)
	pgExec(t, pool, `GRANT SELECT ON runner_spool_cursors TO `+reader)

	migrateBefore(t, pool, "0292")

	for priv, want := range map[string]bool{"SELECT": true, "INSERT": true, "UPDATE": true, "DELETE": false, "TRUNCATE": false} {
		if got := pgScalar[bool](t, pool, `SELECT has_table_privilege($1, 'runner_event_refusal_windows', $2)`, app, priv); got != want {
			t.Errorf("app role %s on runner_event_refusal_windows = %v, want %v", priv, got, want)
		}
		if pgScalar[bool](t, pool, `SELECT has_table_privilege($1, 'runner_event_refusal_windows', $2)`, reader, priv) {
			t.Errorf("a role that only reads runner_spool_cursors holds %s on runner_event_refusal_windows", priv)
		}
	}
}

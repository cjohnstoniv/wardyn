// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package db

import (
	"fmt"
	"testing"
)

const sweeperFamiliesMigration = "0420"

// TestMigration_SweeperFamilies_PG: the migration grants sweeper_families to the role that already writes
// sweeper_leader, found from the catalog: SELECT and UPDATE only. A role that only reads sweeper_leader gets
// nothing.
func TestMigration_SweeperFamilies_PG(t *testing.T) {
	pool, schema := partialSchemaPool(t, sweeperFamiliesMigration)
	app, _ := auditRole(t, pgPool(t), "wardyn_pg_app")
	reader, _ := auditRole(t, pgPool(t), "wardyn_pg_reader")
	for _, role := range []string{app, reader} {
		pgExec(t, pool, fmt.Sprintf(`GRANT USAGE ON SCHEMA %s TO %s`, schema, role))
	}
	pgExec(t, pool, `GRANT SELECT, UPDATE ON sweeper_leader TO `+app)
	pgExec(t, pool, `GRANT SELECT ON sweeper_leader TO `+reader)
	// "~" sorts after "_", so this applies the migration itself and stops before the next one.
	migrateBefore(t, pool, sweeperFamiliesMigration+"~")
	for priv, want := range map[string]bool{"SELECT": true, "UPDATE": true, "INSERT": false, "DELETE": false, "TRUNCATE": false} {
		if got := pgScalar[bool](t, pool, `SELECT has_table_privilege($1, 'sweeper_families', $2)`, app, priv); got != want {
			t.Errorf("app role %s on sweeper_families = %v, want %v", priv, got, want)
		}
		if pgScalar[bool](t, pool, `SELECT has_table_privilege($1, 'sweeper_families', $2)`, reader, priv) {
			t.Errorf("a role that only reads sweeper_leader holds %s on sweeper_families", priv)
		}
	}
	if n := pgScalar[int64](t, pool, `SELECT count(*) FROM sweeper_families`); n != int64(len(SweeperFamilies)) {
		t.Errorf("sweeper_families has %d rows, want one per family (%d)", n, len(SweeperFamilies))
	}
}

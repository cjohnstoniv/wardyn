// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package db

import (
	"fmt"
	"testing"
)

const driveReclaimFenceMigration = "0427"

// TestMigration_DriveReclaimFence_SplitRoleGrants_PG: the app role, found from the catalog as a role holding UPDATE on
// user_drives, gets SELECT, INSERT, UPDATE and DELETE on drive_reclaims (a settled claim is deleted) and SELECT and
// INSERT on drive_object_holds (a hold goes only with its run); a role that holds nothing on user_drives gets none of
// them. The fixture sets no default privileges. migrateBefore stops at the first file at or after "0428", so 0427 is
// the last one applied; both constants move with the file if it is renumbered.
func TestMigration_DriveReclaimFence_SplitRoleGrants_PG(t *testing.T) {
	pool, schema := partialSchemaPool(t, driveReclaimFenceMigration)
	app, _ := auditRole(t, pgPool(t), "wardyn_drf_app")
	other, _ := auditRole(t, pgPool(t), "wardyn_drf_other")
	for _, role := range []string{app, other} {
		pgExec(t, pool, fmt.Sprintf(`GRANT USAGE ON SCHEMA %s TO %s`, schema, role))
	}
	pgExec(t, pool, `GRANT SELECT, INSERT, UPDATE, DELETE ON user_drives TO `+app)

	migrateBefore(t, pool, "0428")

	for table, privs := range map[string]map[string]bool{
		"drive_reclaims":     {"SELECT": true, "INSERT": true, "UPDATE": true, "DELETE": true, "TRUNCATE": false},
		"drive_object_holds": {"SELECT": true, "INSERT": true, "UPDATE": false, "DELETE": false, "TRUNCATE": false},
	} {
		for priv, want := range privs {
			if got := pgScalar[bool](t, pool, `SELECT has_table_privilege($1, $2::text, $3)`, app, table, priv); got != want {
				t.Errorf("app role %s on %s = %v, want %v", priv, table, got, want)
			}
			if pgScalar[bool](t, pool, `SELECT has_table_privilege($1, $2::text, $3)`, other, table, priv) {
				t.Errorf("a role with nothing on user_drives holds %s on %s", priv, table)
			}
		}
	}
}

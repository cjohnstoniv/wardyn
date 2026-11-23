// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package db

import (
	"context"
	"fmt"
	"slices"
	"testing"
)

const machineSnapshotsMigration = "0400"

// TestMigration0400_GrantsTheAppRole: on a split-role install the migration gives each of its four tables, with
// exactly the verbs the server's SQL uses on it, to every role that may delete attach leases, read from the
// catalog. A role holding every other verb on the leases, and the leases table's owner, which the catalog lists
// among the roles holding DELETE once the table has any grant, get nothing. The three tables granted fewer than
// all four verbs first lose whatever a role held on them, as default privileges hand every new table out, so a
// re-run over ALL granted on them ends with exactly the same verbs. Each verb is checked on each table, held and
// not held.
func TestMigration0400_GrantsTheAppRole(t *testing.T) {
	pool, schema := partialSchemaPool(t, machineSnapshotsMigration)
	app, _ := auditRole(t, pgPool(t), "wardyn_pg_app")
	other, _ := auditRole(t, pgPool(t), "wardyn_pg_other")
	owner, _ := auditRole(t, pgPool(t), "wardyn_pg_owner")
	for _, role := range []string{app, other, owner} {
		pgExec(t, pool, fmt.Sprintf(`GRANT USAGE ON SCHEMA %s TO %s`, schema, role))
	}
	pgExec(t, pool, `GRANT SELECT, INSERT, UPDATE, DELETE ON run_attach_leases TO `+app)
	pgExec(t, pool, `GRANT SELECT, INSERT, UPDATE, TRUNCATE, REFERENCES, TRIGGER ON run_attach_leases TO `+other)
	pgExec(t, pool, `ALTER TABLE run_attach_leases OWNER TO `+owner)
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `ALTER TABLE run_attach_leases OWNER TO CURRENT_USER`) })
	if !pgScalar[bool](t, pool, `SELECT EXISTS (SELECT 1 FROM pg_catalog.pg_class c CROSS JOIN LATERAL pg_catalog.aclexplode(c.relacl) a
		WHERE c.oid = 'run_attach_leases'::regclass AND a.grantee = c.relowner AND a.privilege_type = 'DELETE')`) {
		t.Fatal("the leases table's owner holds no DELETE entry in its acl, so the owner case proves nothing")
	}

	migrateBefore(t, pool, "0401")

	granted := map[string][]string{
		"machine_snapshots":         {"SELECT", "INSERT", "UPDATE", "DELETE"},
		"machine_snapshot_holds":    {"SELECT", "INSERT", "DELETE"},
		"run_fork_sources":          {"SELECT", "INSERT"},
		"run_dispatch_secret_facts": {"SELECT", "INSERT", "UPDATE"},
	}
	verbs := []string{"SELECT", "INSERT", "UPDATE", "DELETE", "TRUNCATE", "REFERENCES", "TRIGGER"}
	check := func(when string) {
		t.Helper()
		for table, want := range granted {
			for _, verb := range verbs {
				if got := pgScalar[bool](t, pool, `SELECT has_table_privilege($1, $2, $3)`, app, table, verb); got != slices.Contains(want, verb) {
					t.Errorf("%s: app role %s on %s = %v, want %v", when, verb, table, got, slices.Contains(want, verb))
				}
				for name, role := range map[string]string{"a role holding every verb on run_attach_leases but DELETE": other, "the owner of run_attach_leases": owner} {
					if pgScalar[bool](t, pool, `SELECT has_table_privilege($1, $2, $3)`, role, table, verb) {
						t.Errorf("%s: %s holds %s on %s", when, name, verb, table)
					}
				}
			}
		}
	}
	check("after the migration")

	// Default privileges hand every new table to the app role with all its verbs; the migration takes back what
	// a narrowed table must not have.
	for table, want := range granted {
		if len(want) < 4 {
			pgExec(t, pool, fmt.Sprintf(`GRANT ALL ON %s TO %s, %s`, table, app, other))
		}
	}
	if !pgScalar[bool](t, pool, `SELECT has_table_privilege($1, 'run_fork_sources', 'DELETE')`, app) {
		t.Fatal("the app role does not hold DELETE on run_fork_sources after GRANT ALL, so the re-run proves nothing")
	}
	pgExec(t, pool, readMigration(t, machineSnapshotsMigration+"_machine_snapshots.sql"))
	check("after a re-run over ALL granted")
}

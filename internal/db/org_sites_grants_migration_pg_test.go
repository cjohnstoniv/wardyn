// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package db

import (
	"context"
	"fmt"
	"testing"
)

const orgSitesMigration = "0400"

// The site migration grants its three tables, found from the catalog: every role but runner_pools' owner that
// holds UPDATE on runner_pools gets SELECT, INSERT, UPDATE and DELETE on each, and nothing more. A role that
// holds every other verb on runner_pools but UPDATE gets nothing, and neither does runner_pools' owner when it is
// not the migrator.
func TestPG_Migration0400_GrantsTheAppRole(t *testing.T) {
	pool, schema := partialSchemaPool(t, orgSitesMigration)
	ctx := context.Background()
	app, _ := auditRole(t, pgPool(t), "wardyn_pg_app")
	reader, _ := auditRole(t, pgPool(t), "wardyn_pg_reader")
	owner, _ := auditRole(t, pgPool(t), "wardyn_pg_owner")
	for _, role := range []string{app, reader, owner} {
		pgExec(t, pool, fmt.Sprintf(`GRANT USAGE ON SCHEMA %s TO %s`, schema, role))
	}
	// The grants a hand-granting install made from the 0.9 note: runner_pools without DELETE.
	pgExec(t, pool, `GRANT SELECT, INSERT, UPDATE ON runner_pools TO `+app)
	readerVerbs := "SELECT, INSERT, DELETE, TRUNCATE, REFERENCES, TRIGGER"
	if pgScalar[bool](t, pool, `SELECT current_setting('server_version_num')::int >= 170000`) {
		readerVerbs += ", MAINTAIN" // a table privilege from PostgreSQL 17
	}
	pgExec(t, pool, `GRANT `+readerVerbs+` ON runner_pools TO `+reader)
	// runner_pools owned by a role other than the migrator: as the owner it holds UPDATE through its own
	// acl entry, and the migration must not grant it the new tables.
	migrator := pgScalar[string](t, pool, `SELECT current_user::text`)
	pgExec(t, pool, fmt.Sprintf(`GRANT CREATE ON SCHEMA %s TO %s`, schema, owner))
	pgExec(t, pool, fmt.Sprintf(`GRANT %s TO %s`, owner, migrator))
	pgExec(t, pool, `ALTER TABLE runner_pools OWNER TO `+owner)
	t.Cleanup(func() { // before the role cleanups, which would otherwise drop the table with its owner
		if _, err := pool.Exec(ctx, `ALTER TABLE runner_pools OWNER TO `+migrator); err != nil {
			t.Logf("give runner_pools back to %s: %v", migrator, err)
		}
	})
	if got := pgScalar[bool](t, pool, `SELECT has_table_privilege($1, 'runner_pools', 'UPDATE')`, owner); !got {
		t.Fatal("runner_pools' new owner does not hold UPDATE on it; the owner case would prove nothing")
	}

	migrateBefore(t, pool, "0401")

	for _, table := range []string{"org_sites", "org_site_pools", "org_site_drives"} {
		if got := pgScalar[string](t, pool, `SELECT tableowner::text FROM pg_tables WHERE schemaname = $1 AND tablename = $2`, schema, table); got != migrator {
			t.Fatalf("%s is owned by %q, want the migrator %q", table, got, migrator)
		}
		for priv, want := range map[string]bool{"SELECT": true, "INSERT": true, "UPDATE": true, "DELETE": true, "TRUNCATE": false, "REFERENCES": false, "TRIGGER": false} {
			if got := pgScalar[bool](t, pool, `SELECT has_table_privilege($1, $2, $3)`, app, table, priv); got != want {
				t.Errorf("a role that updates runner_pools: %s on %s = %v, want %v", priv, table, got, want)
			}
			if pgScalar[bool](t, pool, `SELECT has_table_privilege($1, $2, $3)`, reader, table, priv) {
				t.Errorf("a role holding every verb on runner_pools but UPDATE holds %s on %s", priv, table)
			}
			if pgScalar[bool](t, pool, `SELECT has_table_privilege($1, $2, $3)`, owner, table, priv) {
				t.Errorf("runner_pools' owner was granted %s on %s", priv, table)
			}
		}
	}
}

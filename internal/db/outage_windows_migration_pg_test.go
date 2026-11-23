// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package db

import (
	"context"
	"fmt"
	"testing"
)

const outageWindowsMigration = "0400"

// TestMigration0400_AppRoleOpensAndClosesAWindow_PG: on a split-role install the migration gives the heartbeat and
// the windows to the role that already updates agent_runs, read from the catalog, so the upgrade needs no hand grant.
// That role, which owns nothing and holds no sequence privilege, then beats, opens a window (its identity id), closes
// it and prunes it. A role that may only read agent_runs gets nothing.
func TestMigration0400_AppRoleOpensAndClosesAWindow_PG(t *testing.T) {
	pool, schema := partialSchemaPool(t, outageWindowsMigration)
	app, _ := auditRole(t, pgPool(t), "wardyn_ow_app")
	reader, _ := auditRole(t, pgPool(t), "wardyn_ow_reader")
	for _, role := range []string{app, reader} {
		pgExec(t, pool, fmt.Sprintf(`GRANT USAGE ON SCHEMA %s TO %s`, schema, role))
	}
	pgExec(t, pool, `GRANT SELECT, INSERT, UPDATE, DELETE ON agent_runs TO `+app)
	pgExec(t, pool, `GRANT SELECT ON agent_runs TO `+reader)

	migrateBefore(t, pool, "0401")

	for table, privs := range map[string]map[string]bool{
		"cp_heartbeat":   {"SELECT": true, "INSERT": true, "UPDATE": true, "DELETE": false, "TRUNCATE": false},
		"outage_windows": {"SELECT": true, "INSERT": true, "UPDATE": true, "DELETE": true, "TRUNCATE": false},
	} {
		for priv, want := range privs {
			if got := pgScalar[bool](t, pool, `SELECT has_table_privilege($1, $2, $3)`, app, table, priv); got != want {
				t.Errorf("app role %s on %s = %v, want %v", priv, table, got, want)
			}
			if pgScalar[bool](t, pool, `SELECT has_table_privilege($1, $2, $3)`, reader, table, priv) {
				t.Errorf("a role that only reads agent_runs holds %s on %s", priv, table)
			}
		}
	}
	if pgScalar[bool](t, pool, `SELECT has_sequence_privilege($1, pg_get_serial_sequence('outage_windows', 'id'), 'USAGE')`, app) {
		t.Error("the app role holds USAGE on the window id's sequence; the test no longer shows an identity id needs none")
	}

	ctx := context.Background()
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // nothing is kept
	if _, err := tx.Exec(ctx, `SET LOCAL ROLE `+app); err != nil {
		t.Fatalf("set role: %v", err)
	}
	for _, step := range []struct{ what, sql string }{
		{"lock the heartbeat", `SELECT beat_at FROM cp_heartbeat WHERE singleton FOR UPDATE`},
		{"beat", `UPDATE cp_heartbeat SET beat_at = clock_timestamp() WHERE singleton`},
		{"open a window", `INSERT INTO outage_windows (kind, started_at, detail) VALUES ('server_outage', now() - interval '21 minutes', '{}') ON CONFLICT DO NOTHING`},
		{"close it", `UPDATE outage_windows SET ended_at = greatest(started_at, now()) WHERE kind = 'server_outage' AND ended_at IS NULL`},
		{"prune it", `DELETE FROM outage_windows WHERE ended_at IS NOT NULL AND ended_at < now() + interval '1 second'`},
	} {
		tag, err := tx.Exec(ctx, step.sql)
		if err != nil {
			t.Fatalf("%s as the app role: %v", step.what, err)
		}
		if tag.RowsAffected() != 1 {
			t.Fatalf("%s as the app role touched %d rows, want 1", step.what, tag.RowsAffected())
		}
	}
}

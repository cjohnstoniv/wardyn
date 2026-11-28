// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package db

import (
	"context"
	"fmt"
	"testing"

	"github.com/google/uuid"
)

const runOutageTermsMigration = "0421"

// TestMigration0421_AppRoleRecordsAndReadsOutageTerms_PG: on a split-role install the migration gives run_outage_terms
// to the role that already updates agent_runs, read from the catalog, so the upgrade needs no hand grant: SELECT and
// INSERT only. That role then records a run's terms with the store's own statement, a second write keeps the first
// (ON CONFLICT DO NOTHING needs no UPDATE), and deleting the run removes its row through the cascade with no DELETE
// on the table. A role that may only read agent_runs gets nothing.
func TestMigration0421_AppRoleRecordsAndReadsOutageTerms_PG(t *testing.T) {
	pool, schema := partialSchemaPool(t, runOutageTermsMigration)
	app, appPass := auditRole(t, pgPool(t), "wardyn_rot_app")
	reader, _ := auditRole(t, pgPool(t), "wardyn_rot_reader")
	for _, role := range []string{app, reader} {
		pgExec(t, pool, fmt.Sprintf(`GRANT USAGE ON SCHEMA %s TO %s`, schema, role))
	}
	pgExec(t, pool, `GRANT SELECT, INSERT, UPDATE, DELETE ON agent_runs TO `+app)
	pgExec(t, pool, `GRANT SELECT ON agent_runs TO `+reader)

	migrateBefore(t, pool, "0422")

	for priv, want := range map[string]bool{"SELECT": true, "INSERT": true, "UPDATE": false, "DELETE": false, "TRUNCATE": false} {
		if got := pgScalar[bool](t, pool, `SELECT has_table_privilege($1, 'run_outage_terms', $2)`, app, priv); got != want {
			t.Errorf("app role %s on run_outage_terms = %v, want %v", priv, got, want)
		}
		if pgScalar[bool](t, pool, `SELECT has_table_privilege($1, 'run_outage_terms', $2)`, reader, priv) {
			t.Errorf("a role that only reads agent_runs holds %s on run_outage_terms", priv)
		}
	}

	runID := uuid.New()
	pgExec(t, pool, `INSERT INTO agent_runs (id, created_at, created_by, agent, repo, confinement_class, state, spiffe_id, runner_target)
		VALUES ($1, now(), 'sub-a', 'claude-code', 'repo', 'CC1', 'STARTING', $2, 'docker')`, runID, "spiffe://run/"+runID.String())

	ctx := context.Background()
	tx, err := poolAs(t, schema, app, appPass).Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // nothing is kept
	// The statement store.PG.SetRunOutageTerms runs.
	record := func(allowance int64, strict bool) (int64, bool) {
		t.Helper()
		var gotAllowance int64
		var gotStrict, gotEnd bool
		if err := tx.QueryRow(ctx, `WITH ins AS (
		INSERT INTO run_outage_terms (run_id, allowance_s, strict, end_unattended) VALUES ($1, $2, $3, $4)
		ON CONFLICT (run_id) DO NOTHING RETURNING allowance_s, strict, end_unattended)
	SELECT allowance_s, strict, end_unattended FROM ins
	UNION ALL
	SELECT allowance_s, strict, end_unattended FROM run_outage_terms WHERE run_id = $1 AND NOT EXISTS (SELECT 1 FROM ins)`,
			runID, allowance, strict, false).Scan(&gotAllowance, &gotStrict, &gotEnd); err != nil {
			t.Fatalf("record terms as the app role: %v", err)
		}
		return gotAllowance, gotStrict
	}
	if a, s := record(1800, true); a != 1800 || !s {
		t.Fatalf("first write as the app role = %d %v, want 1800 strict", a, s)
	}
	if a, s := record(7200, false); a != 1800 || !s {
		t.Fatalf("second write as the app role = %d %v, want the first terms kept", a, s)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM agent_runs WHERE id = $1`, runID); err != nil {
		t.Fatalf("delete the run as the app role: %v", err)
	}
	var left int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM run_outage_terms WHERE run_id = $1`, runID).Scan(&left); err != nil || left != 0 {
		t.Fatalf("terms rows left after the run's delete = %d %v, want 0", left, err)
	}
}

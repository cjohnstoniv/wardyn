// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package db

import (
	"context"
	"fmt"
	"testing"

	"github.com/google/uuid"
)

// TestMigration0424_AppRoleUpdatesOnlyProxyPauses_PG: on a split-role install the migration gives the role that
// already updates agent_runs UPDATE on run_outage_terms.proxy_pauses alone, read from the catalog, so the upgrade
// needs no hand grant. That role then runs both of the store's statements on the column: a proxy start's clearing
// update (store.PG.BeginProxyStart) and a loaded report's upsert (store.PG.SetRunProxyPauses). It still cannot change a
// run's terms, and a role that may only read agent_runs gets nothing.
func TestMigration0424_AppRoleUpdatesOnlyProxyPauses_PG(t *testing.T) {
	pool, schema := partialSchemaPool(t, runOutageTermsMigration)
	app, appPass := auditRole(t, pgPool(t), "wardyn_rop_app")
	reader, _ := auditRole(t, pgPool(t), "wardyn_rop_reader")
	for _, role := range []string{app, reader} {
		pgExec(t, pool, fmt.Sprintf(`GRANT USAGE ON SCHEMA %s TO %s`, schema, role))
	}
	pgExec(t, pool, `GRANT SELECT, INSERT, UPDATE, DELETE ON agent_runs TO `+app)
	pgExec(t, pool, `GRANT SELECT ON agent_runs TO `+reader)

	migrateBefore(t, pool, "0425")

	for column, want := range map[string]bool{"proxy_pauses": true, "allowance_s": false, "strict": false, "end_unattended": false, "run_id": false} {
		if got := pgScalar[bool](t, pool, `SELECT has_column_privilege($1, 'run_outage_terms', $2, 'UPDATE')`, app, column); got != want {
			t.Errorf("app role UPDATE on run_outage_terms.%s = %v, want %v", column, got, want)
		}
		if pgScalar[bool](t, pool, `SELECT has_column_privilege($1, 'run_outage_terms', $2, 'UPDATE')`, reader, column) {
			t.Errorf("a role that only reads agent_runs holds UPDATE on run_outage_terms.%s", column)
		}
	}

	// run_proxy_starts predates the catalog grants: a split-role install grants it by hand, as here.
	pgExec(t, pool, `GRANT SELECT, INSERT, UPDATE ON run_proxy_starts TO `+app)
	runID := uuid.New()
	pgExec(t, pool, `INSERT INTO agent_runs (id, created_at, created_by, agent, repo, confinement_class, state, spiffe_id, runner_target)
		VALUES ($1, now(), 'sub-a', 'claude-code', 'repo', 'CC1', 'RUNNING', $2, 'docker')`, runID, "spiffe://run/"+runID.String())

	ctx := context.Background()
	tx, err := poolAs(t, schema, app, appPass).Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // nothing is kept
	pauses := func() bool {
		t.Helper()
		var got bool
		if err := tx.QueryRow(ctx, `SELECT proxy_pauses FROM run_outage_terms WHERE run_id = $1`, runID).Scan(&got); err != nil {
			t.Fatalf("read proxy_pauses as the app role: %v", err)
		}
		return got
	}
	// The statement store.PG.SetRunProxyPauses runs: its insert arm first, then its update arm.
	setPauses := func(on bool) {
		t.Helper()
		if _, err := tx.Exec(ctx, `INSERT INTO run_outage_terms (run_id, allowance_s, strict, end_unattended, proxy_pauses) VALUES
		($1, 3600, false, false, $2) ON CONFLICT (run_id) DO UPDATE SET proxy_pauses = EXCLUDED.proxy_pauses`, runID, on); err != nil {
			t.Fatalf("record proxy pauses %v as the app role: %v", on, err)
		}
	}
	setPauses(false)
	setPauses(true)
	if !pauses() {
		t.Fatal("the loaded report's upsert as the app role did not set proxy_pauses")
	}
	// The statement store.PG.BeginProxyStart runs, with a shorter RETURNING.
	var gen int
	if err := tx.QueryRow(ctx, `
		WITH cleared AS (UPDATE run_outage_terms SET proxy_pauses = false WHERE run_id = $1)
		INSERT INTO run_proxy_starts (run_id, generation, jti, mode, in_place, started_at, state)
		VALUES ($1, $2, $3, $4, $5, now(), 'pending')
		ON CONFLICT (run_id, generation) DO UPDATE SET jti = EXCLUDED.jti, mode = EXCLUDED.mode, in_place = EXCLUDED.in_place,
			started_at = now(), deadline = NULL, expected_sha256 = NULL, reported_sha256 = NULL,
			state = 'pending', confirmed_at = NULL, contained_at = NULL, containment_error = NULL
		RETURNING generation`, runID, 1, "jti-1", "expected", false).Scan(&gen); err != nil || gen != 1 {
		t.Fatalf("begin a proxy start as the app role = %d %v, want generation 1", gen, err)
	}
	if pauses() {
		t.Fatal("the proxy start's clearing update as the app role left proxy_pauses set")
	}
	if _, err := tx.Exec(ctx, `SAVEPOINT terms`); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `UPDATE run_outage_terms SET allowance_s = 7200 WHERE run_id = $1`, runID); err == nil {
		t.Fatal("the app role changed a run's outage allowance, want permission denied")
	}
	if _, err := tx.Exec(ctx, `ROLLBACK TO SAVEPOINT terms`); err != nil {
		t.Fatal(err)
	}
}

// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package db

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

const runSupportGrantsMigration = "0293"

// supportMigrationRun seeds a run in state.
func supportMigrationRun(t *testing.T, pool *pgxpool.Pool, state string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	pgExec(t, pool, `INSERT INTO agent_runs (id, created_at, created_by, agent, repo, confinement_class, state, spiffe_id, runner_target, sandbox_ref)
		VALUES ($1, now(), 'sub-owner', 'claude-code', 'repo', 'CC1', $2, $3, 'docker', '')`, id, state, "spiffe://run/"+id.String())
	return id
}

// supportGrantInsert is one grant row with the columns the CHECKs read; a nil argument is NULL.
const supportGrantInsert = `INSERT INTO run_support_grants (id, run_id, helper, reason, doors_asked, doors, minutes, accepted_at, expires_at)
	VALUES (gen_random_uuid(), $1, $2, $3, $4, $5, $6, $7, $8)`

func supportGrantRefused(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) string {
	t.Helper()
	_, err := pool.Exec(context.Background(), sql, args...)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return ""
	}
	return pgErr.Code + " " + pgErr.ConstraintName
}

// TestMigration0293_RefusesWhatTheGrantCannotBe pins the table's CHECKs and the one-open-grant index: a
// request names a helper, a reason of at most 500 characters and at least one door; an accepted grant has its
// accept time, expiry and minutes together, minutes being 15, 30 or 60, and doors inside those asked; a run has
// one open grant at a time, though any number ended; a grant goes with its run. The two tables the grant rides
// on gain their columns with the defaults old rows need.
func TestMigration0293_RefusesWhatTheGrantCannotBe(t *testing.T) {
	pool, _ := partialSchemaPool(t, "0294")
	run := supportMigrationRun(t, pool, "RUNNING")
	now := time.Now().UTC()
	asked := []string{"terminal", "ssh"}
	long := strings.Repeat("x", 501)
	for name, c := range map[string][]any{
		"an empty helper":                {run, "", "why", asked, nil, nil, nil, nil},
		"a reason over 500 characters":   {run, "h", long, asked, nil, nil, nil, nil},
		"no door asked":                  {run, "h", "why", []string{}, nil, nil, nil, nil},
		"a door that was not asked":      {run, "h", "why", asked, []string{"desktop"}, 15, now, now.Add(time.Hour)},
		"minutes of 20":                  {run, "h", "why", asked, asked, 20, now, now.Add(time.Hour)},
		"an accept time with no expiry":  {run, "h", "why", asked, asked, 15, now, nil},
		"an expiry with no accept time":  {run, "h", "why", asked, asked, 15, nil, now.Add(time.Hour)},
		"minutes with no accept time":    {run, "h", "why", asked, nil, 15, nil, nil},
		"an accept time with no minutes": {run, "h", "why", asked, asked, nil, now, now.Add(time.Hour)},
	} {
		if got := supportGrantRefused(t, pool, supportGrantInsert, c...); !strings.HasPrefix(got, "23514") {
			t.Errorf("%s: insert = %q, want a CHECK violation (23514)", name, got)
		}
	}
	for _, minutes := range []int{15, 30, 60} {
		other := supportMigrationRun(t, pool, "RUNNING")
		if got := supportGrantRefused(t, pool, supportGrantInsert, other, "h", "", asked, []string{"ssh"}, minutes, now, now.Add(time.Hour)); got != "" {
			t.Errorf("an accepted grant for %d minutes with an empty reason was refused: %s", minutes, got)
		}
	}

	pgExec(t, pool, supportGrantInsert, run, "h", "why", asked, nil, nil, nil, nil)
	if got := supportGrantRefused(t, pool, supportGrantInsert, run, "h2", "again", asked, nil, nil, nil, nil); got != "23505 run_support_grants_one_open" {
		t.Errorf("a second open grant on a run = %q, want a unique violation of run_support_grants_one_open", got)
	}
	if got := supportGrantRefused(t, pool, supportGrantInsert, supportMigrationRun(t, pool, "RUNNING"), "h", "why", asked, nil, nil, nil, nil); got != "" {
		t.Errorf("an open grant on another run was refused: %s", got)
	}
	pgExec(t, pool, `UPDATE run_support_grants SET ended_at = now(), end_reason = 'revoked' WHERE run_id = $1`, run)
	for i := 0; i < 2; i++ {
		pgExec(t, pool, supportGrantInsert, run, "h", "again", asked, nil, nil, nil, nil)
		pgExec(t, pool, `UPDATE run_support_grants SET ended_at = now(), end_reason = 'declined' WHERE run_id = $1 AND ended_at IS NULL`, run)
	}
	if n := pgScalar[int](t, pool, `SELECT count(*)::int FROM run_support_grants WHERE run_id = $1`, run); n != 3 {
		t.Errorf("the run holds %d ended grants, want 3: ended grants never count against the one open", n)
	}
	pgExec(t, pool, `DELETE FROM agent_runs WHERE id = $1`, run)
	if n := pgScalar[int](t, pool, `SELECT count(*)::int FROM run_support_grants WHERE run_id = $1`, run); n != 0 {
		t.Errorf("%d grants outlived their run", n)
	}

	for col, want := range map[string]string{
		"attach_tickets.support_grant":   "uuid|YES|",
		"attach_tickets.support_expires": "timestamp with time zone|YES|",
		"run_attach_leases.support":      "boolean|NO|false",
	} {
		tbl, name, _ := strings.Cut(col, ".")
		if got := pgScalar[string](t, pool, `SELECT data_type || '|' || is_nullable || '|' || COALESCE(column_default, '')
			FROM information_schema.columns WHERE table_schema = current_schema() AND table_name = $1 AND column_name = $2`, tbl, name); got != want {
			t.Errorf("%s = %s, want %s", col, got, want)
		}
	}
}

// TestMigration0293_TheTriggerEndsGrantsWithTheRun: a run that is stopped, or marked lost, ends its open
// grant in the same statement as 'run_ended', leaving end_audited false for the sweep; an update of another
// column, or of state to RUNNING, does not; a grant already ended keeps its own reason; and a revive that clears
// lost_at later leaves the grant ended. The function runs as its owner with a path of pg_catalog then pg_temp.
func TestMigration0293_TheTriggerEndsGrantsWithTheRun(t *testing.T) {
	pool, _ := partialSchemaPool(t, "0294")
	open := func(run uuid.UUID) uuid.UUID {
		t.Helper()
		id := uuid.New()
		pgExec(t, pool, `INSERT INTO run_support_grants (id, run_id, helper, reason, doors_asked) VALUES ($1, $2, 'h', 'why', '{ssh}')`, id, run)
		return id
	}
	state := func(id uuid.UUID) string {
		return pgScalar[string](t, pool, `SELECT COALESCE(end_reason, 'open') || '|' || end_audited::text FROM run_support_grants WHERE id = $1`, id)
	}

	running := supportMigrationRun(t, pool, "RUNNING")
	g := open(running)
	pgExec(t, pool, `UPDATE agent_runs SET task = 'other column', state = 'RUNNING' WHERE id = $1`, running)
	if got := state(g); got != "open|false" {
		t.Errorf("after an update of another column and of state to RUNNING the grant is %s, want open|false", got)
	}
	pgExec(t, pool, `UPDATE agent_runs SET lost_at = now(), lost_reason = 'ended' WHERE id = $1`, running)
	if got := state(g); got != "run_ended|false" {
		t.Errorf("after the run was marked lost the grant is %s, want run_ended|false", got)
	}
	pgExec(t, pool, `UPDATE agent_runs SET lost_at = NULL, lost_reason = '' WHERE id = $1`, running)
	if got := state(g); got != "run_ended|false" {
		t.Errorf("after the run was revived the grant is %s, want it still ended as run_ended", got)
	}

	stopped := supportMigrationRun(t, pool, "RUNNING")
	g = open(stopped)
	pgExec(t, pool, `UPDATE agent_runs SET state = 'STOPPED' WHERE id = $1`, stopped)
	if got := state(g); got != "run_ended|false" {
		t.Errorf("after the run stopped the grant is %s, want run_ended|false", got)
	}

	// An update of a column the trigger does not list does not fire it, whatever the run's state.
	over := supportMigrationRun(t, pool, "STOPPED")
	g = open(over)
	pgExec(t, pool, `UPDATE agent_runs SET task = 'edited' WHERE id = $1`, over)
	if got := state(g); got != "open|false" {
		t.Errorf("an update of a column the trigger does not watch changed the grant to %s", got)
	}

	kept := supportMigrationRun(t, pool, "RUNNING")
	g = open(kept)
	pgExec(t, pool, `UPDATE run_support_grants SET ended_at = now(), end_reason = 'revoked', end_audited = true WHERE id = $1`, g)
	pgExec(t, pool, `UPDATE agent_runs SET state = 'STOPPED' WHERE id = $1`, kept)
	if got := state(g); got != "revoked|true" {
		t.Errorf("a grant already ended is %s after its run stopped, want revoked|true untouched", got)
	}

	if got := pgScalar[string](t, pool, `SELECT p.prosecdef::text || '|' || array_to_string(p.proconfig, ',') FROM pg_proc p
		WHERE p.oid = to_regprocedure('run_support_grants_end_with_run()')`); got != "true|search_path=pg_catalog, pg_temp" {
		t.Errorf("run_support_grants_end_with_run = %s, want security definer with search_path = pg_catalog, pg_temp", got)
	}
}

// TestMigration0293_TheTriggerStillRunsForAnAppRoleWithoutTheGrant: a split-role app role that was not given
// the grant table can still stop a run, and the run's open grant still ends, because the function runs as the
// migrator: the trigger never fails a run write.
func TestMigration0293_TheTriggerStillRunsForAnAppRoleWithoutTheGrant(t *testing.T) {
	pool, schema := partialSchemaPool(t, "0294")
	app, pass := auditRole(t, pgPool(t), "wardyn_pg_app")
	pgExec(t, pool, fmt.Sprintf(`GRANT USAGE ON SCHEMA %s TO %s`, schema, app))
	pgExec(t, pool, `GRANT SELECT, UPDATE ON agent_runs TO `+app)
	if pgScalar[bool](t, pool, `SELECT has_table_privilege($1, 'run_support_grants', 'SELECT')`, app) {
		t.Fatal("the app role already reads run_support_grants, so this test proves nothing")
	}
	run := supportMigrationRun(t, pool, "RUNNING")
	pgExec(t, pool, `INSERT INTO run_support_grants (id, run_id, helper, reason, doors_asked) VALUES (gen_random_uuid(), $1, 'h', 'why', '{ssh}')`, run)

	pgExec(t, poolAs(t, schema, app, pass), `UPDATE agent_runs SET state = 'STOPPED' WHERE id = $1`, run)

	if got := pgScalar[string](t, pool, `SELECT COALESCE(end_reason, 'open') FROM run_support_grants WHERE run_id = $1`, run); got != "run_ended" {
		t.Errorf("the grant is %s after the app role stopped the run, want run_ended", got)
	}
}

// TestMigration0293_GrantsTheAppRole: on a split-role install the migration itself gives the table to the
// role that already writes the attach leases and tickets, read from the catalog, so the upgrade needs no hand
// grant; a role that may only read the leases gets nothing, and nobody gets TRUNCATE.
func TestMigration0293_GrantsTheAppRole(t *testing.T) {
	pool, schema := partialSchemaPool(t, runSupportGrantsMigration)
	app, _ := auditRole(t, pgPool(t), "wardyn_pg_app")
	reader, _ := auditRole(t, pgPool(t), "wardyn_pg_reader")
	for _, role := range []string{app, reader} {
		pgExec(t, pool, fmt.Sprintf(`GRANT USAGE ON SCHEMA %s TO %s`, schema, role))
	}
	pgExec(t, pool, `GRANT SELECT, INSERT, UPDATE, DELETE ON run_attach_leases TO `+app)
	pgExec(t, pool, `GRANT SELECT ON run_attach_leases TO `+reader)

	migrateBefore(t, pool, "0294")

	for priv, want := range map[string]bool{"SELECT": true, "INSERT": true, "UPDATE": true, "DELETE": true, "TRUNCATE": false} {
		if got := pgScalar[bool](t, pool, `SELECT has_table_privilege($1, 'run_support_grants', $2)`, app, priv); got != want {
			t.Errorf("app role %s on run_support_grants = %v, want %v", priv, got, want)
		}
		if pgScalar[bool](t, pool, `SELECT has_table_privilege($1, 'run_support_grants', $2)`, reader, priv) {
			t.Errorf("a role that only reads run_attach_leases holds %s on run_support_grants", priv)
		}
	}
}

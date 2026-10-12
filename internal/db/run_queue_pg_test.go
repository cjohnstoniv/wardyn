// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package db

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
)

const runQueueMigration = "0162"

// TestMigration0162_RunsUnchangedAndQueueTablesAdded applies the run queue migration over a database
// holding runs in every state: each agent_runs row is byte-identical on the columns it had before,
// admitted_at is NULL, run_queue exists and is empty, QUEUED is accepted and the retention function
// and the watcher sweep index name it.
func TestMigration0162_RunsUnchangedAndQueueTablesAdded(t *testing.T) {
	pool, schema := partialSchemaPool(t, runQueueMigration)
	ctx := context.Background()

	for _, state := range []string{"PENDING", "STARTING", "RUNNING", "WAITING_FOR_CONFIRMATION", "COMPLETED", "STOPPED", "ARCHIVED", "FAILED", "KILLED"} {
		pgExec(t, pool, `INSERT INTO agent_runs (id, created_by, agent, repo, confinement_class, state, spiffe_id, runner_target)
			VALUES ($1, 'person', 'claude-code', 'repo', 'CC1', $2, 'spiffe://x/'||$2, 'docker')`, uuid.New(), state)
	}
	cols := pgScalar[[]string](t, pool, `SELECT array_agg(column_name::text ORDER BY column_name) FROM information_schema.columns
		WHERE table_schema = current_schema() AND table_name = 'agent_runs'`)
	snapshot := func() string {
		t.Helper()
		rows, err := pool.Query(ctx, `SELECT (SELECT jsonb_object_agg(k, v) FROM jsonb_each(to_jsonb(x)) AS e(k, v) WHERE k = ANY($1))::text
			FROM agent_runs x ORDER BY id`, cols)
		if err != nil {
			t.Fatalf("snapshot: %v", err)
		}
		defer rows.Close()
		var parts []string
		for rows.Next() {
			var s string
			if err := rows.Scan(&s); err != nil {
				t.Fatal(err)
			}
			parts = append(parts, s)
		}
		return strings.Join(parts, "\n")
	}
	before := snapshot()
	if before == "" {
		t.Fatalf("fixture seeded nothing in %s", schema)
	}

	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("Migrate applying 0162 over a seeded database: %v", err)
	}
	if got := snapshot(); got != before {
		t.Errorf("agent_runs changed:\nbefore %s\nafter  %s", before, got)
	}
	if n := pgScalar[int](t, pool, `SELECT count(*)::int FROM agent_runs WHERE admitted_at IS NOT NULL`); n != 0 {
		t.Errorf("%d existing runs gained an admitted_at, want none", n)
	}
	if n := pgScalar[int](t, pool, `SELECT count(*)::int FROM run_queue`); n != 0 {
		t.Errorf("run_queue holds %d rows after the migration, want none", n)
	}
	pgExec(t, pool, `INSERT INTO agent_runs (id, created_by, agent, repo, confinement_class, state, spiffe_id, runner_target)
		VALUES ($1, 'person', 'claude-code', 'repo', 'CC1', 'QUEUED', '', 'docker')`, uuid.New())

	if !pgScalar[bool](t, pool, `SELECT pg_get_functiondef('audit_retention_partitions(text, boolean)'::regprocedure) LIKE '%''QUEUED''%'`) {
		t.Error("audit_retention_partitions does not treat a QUEUED run as live")
	}
	if !pgScalar[bool](t, pool, `SELECT indexdef LIKE '%''QUEUED''%' FROM pg_indexes WHERE schemaname = current_schema() AND indexname = 'agent_runs_watcher_sweep_idx'`) {
		t.Error("agent_runs_watcher_sweep_idx does not list QUEUED")
	}
}

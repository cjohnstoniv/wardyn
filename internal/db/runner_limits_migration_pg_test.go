// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package db

import (
	"strings"
	"testing"

	"github.com/google/uuid"
)

// TestMigration0174_RunnerLimits applies the runner-limits migration over runners, a remote-hosted
// runner and runs from before it. The runners declare no limits (NULL), so they take no run until set;
// a run holding a sandbox now reserves the size it was dispatched with and its main drive, so it is
// counted from the first start, while a finished or waiting run reserves nothing. Afterwards the schema
// refuses half a limit set, a per-run amount past its total, a disk that is not whole GiB, less than one
// CPU, and a remote-hosted runner's Runs at once past 1024.
func TestMigration0174_RunnerLimits(t *testing.T) {
	pool, _ := partialSchemaPool(t, "0174")
	runner := uuid.New()
	pgExec(t, pool, `INSERT INTO runners (id, owner, public_key, key_fingerprint, state, max_runs) VALUES ($1, 'sub-a', $2, $3, 'claimed', 2)`,
		runner, append(runner[:], runner[:]...), "fp-"+runner.String())
	pgExec(t, pool, `INSERT INTO remote_runners (id, name) VALUES ('docker', 'Docker host') ON CONFLICT DO NOTHING`)
	live, done, waiting := uuid.New(), uuid.New(), uuid.New()
	for id, state := range map[uuid.UUID]string{live: "RUNNING", done: "COMPLETED", waiting: "QUEUED"} {
		pgExec(t, pool, `INSERT INTO agent_runs (id, created_by, agent, repo, confinement_class, state, spiffe_id, runner_target,
			runner_id, agent_cpu_limit_millis, agent_memory_limit_mib, disk_mib) VALUES ($1, 'sub-a', 'a', 'r', 'CC1', $2, '', 'docker', $3, 2000, 4096, 10240)`,
			id, state, runner)
	}

	migrateBefore(t, pool, "0175")

	for id, want := range map[uuid.UUID]string{live: "2000/4096/10240", done: "0/0/0", waiting: "0/0/0"} {
		if got := pgScalar[string](t, pool, `SELECT reserved_cpu_millis || '/' || reserved_memory_mib || '/' || reserved_disk_mib FROM agent_runs WHERE id = $1`, id); got != want {
			t.Errorf("run %s reserves %s, want %s", id, got, want)
		}
	}
	if n := pgScalar[int](t, pool, `SELECT count(*)::int FROM runners WHERE id = $1 AND total_cpu_millis IS NULL AND run_disk_mib IS NULL`, runner); n != 1 {
		t.Error("an existing runner was given limits it never declared")
	}
	if n := pgScalar[int](t, pool, `SELECT count(*)::int FROM remote_runners WHERE id = 'docker' AND max_runs IS NULL AND total_cpu_millis IS NULL`); n != 1 {
		t.Error("an existing remote-hosted runner was given limits nobody declared")
	}
	set := func(table, key, cols string) string {
		return `UPDATE ` + table + ` SET ` + cols + ` WHERE id = ` + key
	}
	whole := `total_cpu_millis = 4000, total_memory_mib = 8192, total_disk_mib = 102400, run_cpu_millis = 2000, run_memory_mib = 4096, run_disk_mib = 51200`
	rid := "'" + runner.String() + "'"
	pgExec(t, pool, set("runners", rid, whole))
	pgExec(t, pool, set("remote_runners", "'docker'", "max_runs = 1024, "+whole))
	refused := map[string]string{
		"half a limit set":                set("runners", rid, "run_disk_mib = NULL"),
		"a per-run amount past its total": set("runners", rid, "run_memory_mib = 8193"),
		"a disk that is not whole GiB":    set("runners", rid, "run_disk_mib = 51201"),
		"less than one CPU":               set("runners", rid, "run_cpu_millis = 999"),
		"a remote runner's half a set":    set("remote_runners", "'docker'", "max_runs = NULL"),
		"a remote Runs at once past 1024": set("remote_runners", "'docker'", "max_runs = 1025"),
		"a remote per-run past its total": set("remote_runners", "'docker'", "run_cpu_millis = 4001"),
		"a negative reservation":          `UPDATE agent_runs SET reserved_disk_mib = -1 WHERE id = '` + live.String() + `'`,
	}
	for what, stmt := range refused {
		if _, err := pool.Exec(t.Context(), stmt); err == nil || !strings.Contains(err.Error(), "check") {
			t.Errorf("the schema accepts %s: %v", what, err)
		}
	}
	pgExec(t, pool, set("runners", rid, "total_cpu_millis = NULL, total_memory_mib = NULL, total_disk_mib = NULL, run_cpu_millis = NULL, run_memory_mib = NULL, run_disk_mib = NULL"))
}

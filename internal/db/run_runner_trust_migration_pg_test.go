// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package db

import (
	"strings"
	"testing"

	"github.com/google/uuid"
)

// TestMigration0184_RunRunnerTrustClass applies the run trust-class migration over a local run bound to a
// runner and a remote run from before it: the local run is recorded personal, every runner's class before
// 0181, and the remote run has none. Afterwards the schema refuses a class outside the set.
func TestMigration0184_RunRunnerTrustClass(t *testing.T) {
	pool, _ := partialSchemaPool(t, "0184")
	runner, local, remote := uuid.New(), uuid.New(), uuid.New()
	pgExec(t, pool, `INSERT INTO runners (id, owner, public_key, key_fingerprint, state, max_runs) VALUES ($1, 'sub-a', $2, $3, 'claimed', 2)`,
		runner, append(runner[:], runner[:]...), "fp-"+runner.String())
	pgExec(t, pool, `INSERT INTO agent_runs (id, created_by, agent, repo, confinement_class, state, spiffe_id, runner_target, placement, runner_id)
		VALUES ($1, 'sub-a', 'claude-code', 'repo', 'CC1', 'STOPPED', 'spiffe://x', 'docker', 'local', $2)`, local, runner)
	pgExec(t, pool, `INSERT INTO agent_runs (id, created_by, agent, repo, confinement_class, state, spiffe_id, runner_target)
		VALUES ($1, 'sub-a', 'claude-code', 'repo', 'CC1', 'STOPPED', 'spiffe://y', 'docker')`, remote)

	migrateBefore(t, pool, "0185")

	if got := pgScalar[string](t, pool, `SELECT COALESCE(runner_trust_class, 'none') FROM agent_runs WHERE id = $1`, local); got != "personal" {
		t.Errorf("a local run on a runner became %q, want personal", got)
	}
	if got := pgScalar[string](t, pool, `SELECT COALESCE(runner_trust_class, 'none') FROM agent_runs WHERE id = $1`, remote); got != "none" {
		t.Errorf("a remote run became %q, want no class", got)
	}
	if _, err := pool.Exec(t.Context(), `UPDATE agent_runs SET runner_trust_class = 'trusted' WHERE id = $1`, local); err == nil || !strings.Contains(err.Error(), "check") {
		t.Errorf("the schema accepts an unknown run trust class: %v", err)
	}
	pgExec(t, pool, `UPDATE agent_runs SET runner_trust_class = 'org_cluster' WHERE id = $1`, local)
}

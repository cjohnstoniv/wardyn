// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package db

import (
	"strings"
	"testing"

	"github.com/google/uuid"
)

const agentRunsDataOutMigration = "0292"

// TestMigration0292_FillsDataOutFromThePolicyRows applies the data_out column over runs dispatched before it:
// a run takes the strictest rule among its run.policy.resolve row (run.policy.effective in older releases)
// and its run.policy.generation rows, a resolve row with no rule reads as allowed, a generation row with no
// rule says nothing. A run left with no rule, its policy rows pruned or only generation rows with no rule
// kept, is blocked when it was dispatched and keeps no rule while it waits for dispatch: QUEUED, PENDING or
// STARTING with no sandbox. The column refuses a value that is not a rule.
func TestMigration0292_FillsDataOutFromThePolicyRows(t *testing.T) {
	pool, _ := partialSchemaPool(t, agentRunsDataOutMigration)
	blocked, review, legacy, unset, raised, pruned, ruleless := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	seq := 0
	seed := func(id uuid.UUID, state, ref string) {
		t.Helper()
		seq++
		pgExec(t, pool, `INSERT INTO agent_runs (id, created_by, agent, repo, confinement_class, state, spiffe_id, runner_target, sandbox_ref)
			VALUES ($1, 'sub-a', 'claude-code', 'repo', 'CC1', $2, $3, 'docker', $4)`, id, state, "spiffe://run/"+string(rune('a'+seq)), ref)
	}
	for _, id := range []uuid.UUID{blocked, review, legacy, unset, raised, pruned, ruleless} {
		seed(id, "STOPPED", "")
	}
	// Runs with no policy row left: the first four wait for dispatch, the rest were dispatched.
	queued, pending, starting, created, running, failed := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	seed(queued, "QUEUED", "")
	seed(pending, "PENDING", "")
	seed(starting, "STARTING", "")
	seed(created, "STARTING", "sandbox-created")
	seed(running, "RUNNING", "sandbox-running")
	seed(failed, "FAILED", "")
	policyRow := func(run uuid.UUID, action, data string) {
		t.Helper()
		pgExec(t, pool, `SELECT audit_append(gen_random_uuid(), now(), $1, 'system', 'wardynd', $2, $3, 'success', '', $4::jsonb)`,
			run, action, run.String(), data)
	}
	policyRow(blocked, "run.policy.resolve", `{"data_out":"blocked","allowed_domains":["a.example"]}`)
	policyRow(review, "run.policy.resolve", `{"data_out":"review"}`)
	policyRow(legacy, "run.policy.effective", `{"data_out":"blocked"}`)
	policyRow(unset, "run.policy.resolve", `{"allowed_domains":["a.example"]}`)
	policyRow(raised, "run.policy.resolve", `{"data_out":"review"}`)
	policyRow(raised, "run.policy.generation", `{"generation":2,"policy":{"data_out":"blocked"}}`)
	policyRow(raised, "run.policy.generation", `{"generation":3,"policy":{}}`)   // an egress edit's: it carries no rule
	policyRow(pruned, "run.create", `{"data_out":"allowed"}`)                    // not a policy row
	policyRow(ruleless, "run.policy.generation", `{"generation":2,"policy":{}}`) // its resolve row was pruned

	migrateBefore(t, pool, "0293")

	for _, tc := range []struct {
		name string
		id   uuid.UUID
		want string
	}{
		{"blocked", blocked, "blocked"},
		{"review", review, "review"},
		{"older spelling", legacy, "blocked"},
		{"no rule recorded", unset, "allowed"},
		{"raised by a revive", raised, "blocked"},
		{"policy rows pruned", pruned, "blocked"},
		{"resolve row pruned, a generation row with no rule kept", ruleless, "blocked"},
		{"queued, never dispatched", queued, "none"},
		{"pending, never dispatched", pending, "none"},
		{"starting with no sandbox, before the resolve row", starting, "none"},
		{"starting with a sandbox", created, "blocked"},
		{"running, policy rows pruned", running, "blocked"},
		{"failed, its sandbox ref cleared at the end", failed, "blocked"},
	} {
		if got := pgScalar[string](t, pool, `SELECT COALESCE(data_out, 'none') FROM agent_runs WHERE id = $1`, tc.id); got != tc.want {
			t.Errorf("%s: data_out = %s, want %s", tc.name, got, tc.want)
		}
	}
	if _, err := pool.Exec(t.Context(), `UPDATE agent_runs SET data_out = 'open' WHERE id = $1`, pruned); err == nil || !strings.Contains(err.Error(), "check") {
		t.Errorf("the column accepts a value that is not a rule: %v", err)
	}
}

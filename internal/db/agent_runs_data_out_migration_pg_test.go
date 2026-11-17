// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package db

import (
	"strings"
	"testing"

	"github.com/google/uuid"
)

const agentRunsDataOutMigration = "0291"

// TestMigration0291_FillsDataOutFromThePolicyRows applies the data_out column over runs dispatched before it:
// a run takes the strictest rule among its run.policy.resolve row (run.policy.effective in older releases)
// and its run.policy.generation rows, a resolve row with no rule reads as allowed, and a run whose policy rows
// were pruned keeps no rule. The column refuses a value that is not a rule.
func TestMigration0291_FillsDataOutFromThePolicyRows(t *testing.T) {
	pool, _ := partialSchemaPool(t, agentRunsDataOutMigration)
	blocked, review, legacy, unset, raised, pruned := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	for i, id := range []uuid.UUID{blocked, review, legacy, unset, raised, pruned} {
		pgExec(t, pool, `INSERT INTO agent_runs (id, created_by, agent, repo, confinement_class, state, spiffe_id, runner_target)
			VALUES ($1, 'sub-a', 'claude-code', 'repo', 'CC1', 'STOPPED', $2, 'docker')`, id, "spiffe://run/"+string(rune('a'+i)))
	}
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
	policyRow(raised, "run.policy.generation", `{"generation":3,"policy":{}}`) // an egress edit's: it carries no rule
	policyRow(pruned, "run.create", `{"data_out":"allowed"}`)                  // not a policy row

	migrateBefore(t, pool, "0292")

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
		{"policy rows pruned", pruned, "none"},
	} {
		if got := pgScalar[string](t, pool, `SELECT COALESCE(data_out, 'none') FROM agent_runs WHERE id = $1`, tc.id); got != tc.want {
			t.Errorf("%s: data_out = %s, want %s", tc.name, got, tc.want)
		}
	}
	if _, err := pool.Exec(t.Context(), `UPDATE agent_runs SET data_out = 'open' WHERE id = $1`, pruned); err == nil || !strings.Contains(err.Error(), "check") {
		t.Errorf("the column accepts a value that is not a rule: %v", err)
	}
}

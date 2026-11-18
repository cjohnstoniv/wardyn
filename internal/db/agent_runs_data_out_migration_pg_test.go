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
// kept, is blocked when it was dispatched and created before the newest row retention dropped (with a day for
// clock skew), and keeps no rule while it waits for dispatch (QUEUED, PENDING or STARTING with no sandbox) or
// when it is newer than that. The column refuses a value that is not a rule.
func TestMigration0292_FillsDataOutFromThePolicyRows(t *testing.T) {
	pool, _ := partialSchemaPool(t, agentRunsDataOutMigration)
	blocked, review, legacy, unset, raised, pruned, ruleless := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	seq := 0
	seed := func(id uuid.UUID, state, ref, age string) {
		t.Helper()
		seq++
		pgExec(t, pool, `INSERT INTO agent_runs (id, created_at, created_by, agent, repo, confinement_class, state, spiffe_id, runner_target, sandbox_ref)
			VALUES ($1, now() - $5::interval, 'sub-a', 'claude-code', 'repo', 'CC1', $2, $3, 'docker', $4)`,
			id, state, "spiffe://run/"+string(rune('a'+seq)), ref, age)
	}
	// Retention dropped a partition whose newest row is 30 days old.
	pgExec(t, pool, `INSERT INTO audit_chain_anchors (kind, partition_name, recorded_lo, recorded_hi, row_count, actor)
		VALUES ('drop', 'audit_events_dropped', now() - interval '60 days', now() - interval '30 days', 3, 'test')`)
	for _, id := range []uuid.UUID{blocked, review, legacy, unset, raised, pruned, ruleless} {
		seed(id, "STOPPED", "", "60 days")
	}
	// Runs with no policy row left: the first three wait for dispatch, the rest were dispatched. The last two
	// were created after the dropped rows (one within the day allowed for skew): only the older lost a row.
	queued, pending, starting, created, running, failed := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	newer, skewed := uuid.New(), uuid.New()
	seed(queued, "QUEUED", "", "60 days")
	seed(pending, "PENDING", "", "60 days")
	seed(starting, "STARTING", "", "60 days")
	seed(created, "STARTING", "sandbox-created", "60 days")
	seed(running, "RUNNING", "sandbox-running", "60 days")
	seed(failed, "FAILED", "", "60 days")
	seed(newer, "FAILED", "", "10 days")
	seed(skewed, "FAILED", "", "29 days 12 hours")
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
		{"failed with no policy row, newer than every dropped row", newer, "none"},
		{"failed with no policy row, within the skew allowed after the dropped rows", skewed, "blocked"},
	} {
		if got := pgScalar[string](t, pool, `SELECT COALESCE(data_out, 'none') FROM agent_runs WHERE id = $1`, tc.id); got != tc.want {
			t.Errorf("%s: data_out = %s, want %s", tc.name, got, tc.want)
		}
	}
	if _, err := pool.Exec(t.Context(), `UPDATE agent_runs SET data_out = 'open' WHERE id = $1`, pruned); err == nil || !strings.Contains(err.Error(), "check") {
		t.Errorf("the column accepts a value that is not a rule: %v", err)
	}
}

// TestMigration0292_NothingDroppedMarksNothing: with no partition retention dropped, every policy row a run ever
// wrote is still held, so a run with none never wrote one and keeps no rule. A drop of an empty partition (no
// rows, no recorded_hi) and a legacy split (which moves rows and removes none) do not count as drops of rows.
func TestMigration0292_NothingDroppedMarksNothing(t *testing.T) {
	pool, _ := partialSchemaPool(t, agentRunsDataOutMigration)
	pgExec(t, pool, `INSERT INTO audit_chain_anchors (kind, partition_name, row_count, actor) VALUES ('drop', 'audit_events_empty', 0, 'test')`)
	pgExec(t, pool, `INSERT INTO audit_chain_anchors (kind, partition_name, recorded_lo, recorded_hi, row_count, actor)
		VALUES ('split', 'audit_events_legacy_r1', now() - interval '60 days', now() - interval '30 days', 3, 'test')`)
	failed, stopped := uuid.New(), uuid.New()
	for i, r := range []struct {
		id    uuid.UUID
		state string
	}{{failed, "FAILED"}, {stopped, "STOPPED"}} {
		pgExec(t, pool, `INSERT INTO agent_runs (id, created_at, created_by, agent, repo, confinement_class, state, spiffe_id, runner_target)
			VALUES ($1, now() - interval '90 days', 'sub-a', 'claude-code', 'repo', 'CC1', $2, $3, 'docker')`, r.id, r.state, "spiffe://run/n"+string(rune('a'+i)))
	}

	migrateBefore(t, pool, "0293")

	for _, id := range []uuid.UUID{failed, stopped} {
		if got := pgScalar[string](t, pool, `SELECT COALESCE(data_out, 'none') FROM agent_runs WHERE id = $1`, id); got != "none" {
			t.Errorf("run %s: data_out = %s with nothing dropped, want none", id, got)
		}
	}
}

// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package db

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"
)

// TestUnitsMigration runs the real Migrate() over a 0.9 schema holding governance profiles,
// assignments, a run launched under one, a queued launch and pending changes, and asserts nothing was
// lost by the rename: the rows, the foreign keys (a unit still cannot be deleted while a membership
// binds it), the run's unit, the queue's unit_id and the changes' kinds and JSON keys.
//
// Guarded by WARDYN_TEST_PG: skipped cleanly when unset, must PASS when set.
func TestUnitsMigration(t *testing.T) {
	const floor = "0201"
	pool, _ := partialSchemaPool(t, floor)
	ctx := context.Background()
	base, unit, member, run, pooled := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, q, args...); err != nil {
			t.Fatalf("seed %q: %v", q, err)
		}
	}
	exec(`INSERT INTO governance_profiles (id, name, ceiling) VALUES ($1, 'base', '{"allowed_domains":["pypi.org"]}')`, base)
	exec(`INSERT INTO governance_profiles (id, name, ceiling, base_profile_id, overlay) VALUES ($1, 'team', '{}', $2, '{"denied_domains":["x.example"]}')`, unit, base)
	exec(`INSERT INTO governance_assignments (id, subject_type, subject, profile_id, priority) VALUES ($1, 'user', 'alice@corp.example', $2, 4)`, member, unit)
	exec(`INSERT INTO agent_runs (id, created_by, agent, repo, confinement_class, state, spiffe_id, runner_target, governance_profile_id)
	      VALUES ($1, 'alice', 'claude-code', 'r', 'CC2', 'PENDING', 'spiffe://x', 'docker', $2)`, run, unit)
	exec(`INSERT INTO runner_pools (id, name, scope, created_by) VALUES ($1, 'p', 'organization', 'test')`, pooled)
	exec(`UPDATE agent_runs SET state = 'QUEUED' WHERE id = $1`, run)
	exec(`INSERT INTO run_queue (run_id, pool_id, owner, deadline_at, launch) VALUES ($1, $2, 'alice', now() + interval '1 hour',
	      jsonb_build_object('profile_id', $3::text, 'task', 'profile_id'))`, run, pooled, unit.String())
	exec(`INSERT INTO governance_changes (id, target_kind, op, target_key, payload, diff, base_hash, deployment_hash, proposed_by, expires_at)
	      VALUES ($1, 'governance_profile', 'update', 'team',
	      '{"name":"team","base_profile_id":"`+base.String()+`","note":"profile_id"}',
	      '{"after":{"base_profile_id":"`+base.String()+`"},"changed":["base_profile_id"]}', 'h', 'd', 'bob', now() + interval '1 day')`, uuid.New())
	exec(`INSERT INTO governance_changes (id, target_kind, op, target_key, payload, diff, base_hash, deployment_hash, proposed_by, expires_at)
	      VALUES ($1, 'governance_assignment', 'upsert', 'user/alice', '{"profile_id":"`+unit.String()+`","subject":"alice"}',
	      '{"after":{"profile_id":"`+unit.String()+`"}}', 'h', 'd', 'bob', now() + interval '1 day')`, uuid.New())

	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("Migrate from %s: %v", floor, err)
	}

	var name string
	var parent uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT name, parent_id FROM governance_units WHERE id = $1`, unit).Scan(&name, &parent); err != nil || name != "team" || parent != base {
		t.Errorf("unit = %q parent %v (err %v), want team under %v", name, parent, err, base)
	}
	var subject string
	var prio int
	if err := pool.QueryRow(ctx, `SELECT subject, priority FROM governance_memberships WHERE id = $1 AND unit_id = $2`, member, unit).Scan(&subject, &prio); err != nil || subject != "alice@corp.example" || prio != 4 {
		t.Errorf("membership = %q prio %d (err %v), want alice priority 4 on the unit", subject, prio, err)
	}
	var runUnit uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT unit_id FROM agent_runs WHERE id = $1`, run).Scan(&runUnit); err != nil || runUnit != unit {
		t.Errorf("run unit_id = %v (err %v), want %v", runUnit, err, unit)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM governance_units WHERE id = $1`, unit); err == nil {
		t.Error("deleted a unit a membership binds: the restrict foreign key was lost")
	}
	for c, table := range map[string]string{"governance_units_pkey": "governance_units", "governance_units_name_key": "governance_units",
		"governance_units_parent_id_fkey": "governance_units", "governance_memberships_unit_id_fkey": "governance_memberships"} {
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_constraint WHERE conname = $1 AND conrelid = $2::regclass`, c, table).Scan(&n); err != nil || n != 1 {
			t.Errorf("constraint %s on %s: count %d (err %v), want 1", c, table, n, err)
		}
	}
	// Re-applied over the migrated schema (the newest migration is made pending again by another
	// test), it changes nothing and does not fail.
	if _, err := pool.Exec(ctx, readMigration(t, "0201_governance_units.sql")); err != nil {
		t.Errorf("re-applying 0201: %v", err)
	}

	var launch map[string]any
	var rawLaunch []byte
	if err := pool.QueryRow(ctx, `SELECT launch FROM run_queue WHERE run_id = $1`, run).Scan(&rawLaunch); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(rawLaunch, &launch); err != nil {
		t.Fatal(err)
	}
	if launch["unit_id"] != unit.String() || launch["task"] != "profile_id" {
		t.Errorf("queued launch = %v, want unit_id %s and the value profile_id untouched", launch, unit)
	}
	if _, old := launch["profile_id"]; old {
		t.Errorf("queued launch still carries profile_id: %v", launch)
	}

	rows, err := pool.Query(ctx, `SELECT target_kind, payload::text, diff::text FROM governance_changes ORDER BY target_kind`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	got := map[string][2]string{}
	for rows.Next() {
		var kind, payload, diff string
		if err := rows.Scan(&kind, &payload, &diff); err != nil {
			t.Fatal(err)
		}
		got[kind] = [2]string{payload, diff}
	}
	u, ok := got["governance_unit"]
	if !ok {
		t.Fatalf("no governance_unit change after the migration: %v", got)
	}
	var payload, diff map[string]any
	_ = json.Unmarshal([]byte(u[0]), &payload)
	_ = json.Unmarshal([]byte(u[1]), &diff)
	if payload["parent_id"] != base.String() || payload["note"] != "profile_id" || payload["base_profile_id"] != nil {
		t.Errorf("unit change payload = %v, want parent_id, the value profile_id kept, no base_profile_id", payload)
	}
	if after, _ := diff["after"].(map[string]any); after["parent_id"] != base.String() {
		t.Errorf("unit change diff = %v, want after.parent_id", diff)
	}
	if m, ok := got["governance_membership"]; !ok {
		t.Errorf("no governance_membership change after the migration: %v", got)
	} else {
		var p map[string]any
		_ = json.Unmarshal([]byte(m[0]), &p)
		if p["unit_id"] != unit.String() || p["profile_id"] != nil {
			t.Errorf("membership change payload = %v, want unit_id", p)
		}
	}
}

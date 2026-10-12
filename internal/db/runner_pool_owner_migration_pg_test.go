// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package db

import (
	"context"
	"testing"

	"github.com/google/uuid"
)

const runnerPoolOwnerMigration = "0168"

// TestMigration0168_OwnerlessSelfHostedPoolsAreACleanBreak applies the pool-owner migration over a
// catalogue from before it: each ownerless Self-Hosted pool goes, with its members, the organisation and
// personal default slots naming it and any run waiting for it (ended KILLED); one a run is bound to stays
// as a tombstone. The organisation's preference for Self-Hosted is cleared to none, not switched to
// Remote Provided (what a launch naming nothing then answers is TestPG_ClearedOrganisationPreference
// AsksForAChoice). The Remote Provided pool, its executor and the organisation's remote
// default are untouched, and afterwards the schema
// refuses a live ownerless Self-Hosted pool, an owned Remote Provided pool and an organisation
// Self-Hosted default, while two owners may share a name.
func TestMigration0168_OwnerlessSelfHostedPoolsAreACleanBreak(t *testing.T) {
	pool, _ := partialSchemaPool(t, runnerPoolOwnerMigration)
	ctx := context.Background()
	remote, waited, bound := uuid.New(), uuid.New(), uuid.New()
	for id, hosting := range map[uuid.UUID]string{remote: "remote_provided", waited: "self_hosted", bound: "self_hosted"} {
		pgExec(t, pool, `INSERT INTO runner_pools (id, name, hosting_type) VALUES ($1, $2, $3)`, id, id.String()[:8], hosting)
	}
	runner := uuid.New()
	pgExec(t, pool, `INSERT INTO runners (id, owner, public_key, key_fingerprint, state) VALUES ($1, 'sub-person', $2, $3, 'claimed')`,
		runner, append(runner[:], runner[:]...), "fp-"+runner.String())
	pgExec(t, pool, `INSERT INTO runner_pool_members (pool_id, runner_id) VALUES ($1, $2)`, waited, runner)
	pgExec(t, pool, `INSERT INTO runner_pool_members (pool_id, executor_id) VALUES ($1, 'docker')`, remote)
	pgExec(t, pool, `INSERT INTO runner_pool_org_defaults (preferred_hosting, remote_provided, self_hosted) VALUES ('self_hosted', $1, $2)`, remote, waited)
	pgExec(t, pool, `INSERT INTO principal_prefs (principal, key, value) VALUES ('sub-person', 'runner_pool_defaults.v1', jsonb_build_object('remote_provided', $1::text, 'self_hosted', $2::text))`,
		remote, waited)
	queued, ran := uuid.New(), uuid.New()
	pgExec(t, pool, `INSERT INTO agent_runs (id, created_by, agent, repo, confinement_class, state, spiffe_id, runner_target)
		VALUES ($1, 'sub-person', 'claude-code', 'repo', 'CC1', 'QUEUED', 'spiffe://x/q', 'docker')`, queued)
	pgExec(t, pool, `INSERT INTO run_queue (run_id, pool_id, owner, deadline_at) VALUES ($1, $2, 'sub-person', now() + interval '1 hour')`, queued, waited)
	pgExec(t, pool, `INSERT INTO agent_runs (id, created_by, agent, repo, confinement_class, state, spiffe_id, runner_target, runner_pool_id, runner_pool_selection)
		VALUES ($1, 'sub-person', 'claude-code', 'repo', 'CC1', 'COMPLETED', 'spiffe://x/r', 'docker', $2, 'explicit')`, ran, bound)

	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("Migrate applying 0168 over a seeded catalogue: %v", err)
	}

	if n := pgScalar[int](t, pool, `SELECT count(*)::int FROM runner_pools WHERE id = $1`, waited); n != 0 {
		t.Errorf("the unbound ownerless Self-Hosted pool survived (%d rows)", n)
	}
	if st := pgScalar[string](t, pool, `SELECT state FROM runner_pools WHERE id = $1`, bound); st != "deleted" {
		t.Errorf("the pool a run is bound to is %q, want a tombstone", st)
	}
	if got := pgScalar[uuid.UUID](t, pool, `SELECT runner_pool_id FROM agent_runs WHERE id = $1`, ran); got != bound {
		t.Errorf("the bound run's pool = %s, want %s kept", got, bound)
	}
	if n := pgScalar[int](t, pool, `SELECT count(*)::int FROM runner_pool_members WHERE runner_id IS NOT NULL`); n != 0 {
		t.Errorf("%d runner members survived", n)
	}
	if n := pgScalar[int](t, pool, `SELECT count(*)::int FROM runner_pool_members WHERE pool_id = $1`, remote); n != 1 {
		t.Errorf("the remote pool has %d members, want its executor kept", n)
	}
	if st := pgScalar[string](t, pool, `SELECT state || ':' || status_detail FROM agent_runs WHERE id = $1`, queued); st != "KILLED:queue: runner_pool_removed" {
		t.Errorf("the waiting run is %q, want ended with runner_pool_removed", st)
	}
	if n := pgScalar[int](t, pool, `SELECT count(*)::int FROM run_queue`); n != 0 {
		t.Errorf("%d queue rows survived", n)
	}
	if got := pgScalar[string](t, pool, `SELECT preferred_hosting || ':' || remote_provided::text || ':' || COALESCE(self_hosted::text, 'null') FROM runner_pool_org_defaults`); got != ":"+remote.String()+":null" {
		t.Errorf("organisation defaults = %s, want the Self-Hosted slot and preference cleared and the remote default kept", got)
	}
	if got := pgScalar[string](t, pool, `SELECT value::text FROM principal_prefs WHERE principal = 'sub-person'`); got != `{"remote_provided": "`+remote.String()+`"}` {
		t.Errorf("personal defaults = %s, want only the remote slot", got)
	}
	if owner := pgScalar[*string](t, pool, `SELECT owner FROM runner_pools WHERE id = $1`, remote); owner != nil {
		t.Errorf("the remote pool gained an owner %q", *owner)
	}

	refused := map[string]string{
		"a live ownerless Self-Hosted pool": `INSERT INTO runner_pools (id, name, hosting_type) VALUES (gen_random_uuid(), 'x', 'self_hosted')`,
		"an owned Remote Provided pool":     `INSERT INTO runner_pools (id, name, hosting_type, owner) VALUES (gen_random_uuid(), 'y', 'remote_provided', 'sub-person')`,
		"an empty owner":                    `INSERT INTO runner_pools (id, name, hosting_type, owner) VALUES (gen_random_uuid(), 'z', 'self_hosted', '')`,
		"an organisation Self-Hosted slot":  `UPDATE runner_pool_org_defaults SET self_hosted = '` + bound.String() + `'`,
	}
	for what, stmt := range refused {
		if _, err := pool.Exec(ctx, stmt); err == nil {
			t.Errorf("the schema accepts %s", what)
		}
	}
	pgExec(t, pool, `INSERT INTO runner_pools (id, name, hosting_type, owner) VALUES (gen_random_uuid(), 'Laptop', 'self_hosted', 'sub-a')`)
	pgExec(t, pool, `INSERT INTO runner_pools (id, name, hosting_type, owner) VALUES (gen_random_uuid(), 'Laptop', 'self_hosted', 'sub-b')`)
	if _, err := pool.Exec(ctx, `INSERT INTO runner_pools (id, name, hosting_type, owner) VALUES (gen_random_uuid(), 'laptop', 'self_hosted', 'sub-a')`); err == nil {
		t.Error("one owner may hold two live pools of the same name")
	}
}

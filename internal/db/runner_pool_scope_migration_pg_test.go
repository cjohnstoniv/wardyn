// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package db

import (
	"context"
	"net/url"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

const runnerPoolScopeMigration = "0173"

// migrateBefore applies, in order, every migration not yet applied that sorts before stop, so a test of
// an older migration can read the schema it left before a later one reshapes it.
func migrateBefore(t *testing.T, pool *pgxpool.Pool, stop string) {
	t.Helper()
	ctx := context.Background()
	for _, name := range readMigrationNames(t) {
		if name >= stop {
			return
		}
		var done bool
		if err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE filename = $1)`, name).Scan(&done); err != nil {
			t.Fatal(err)
		}
		if !done {
			if err := applyMigration(ctx, pool, name, readMigration(t, name)); err != nil {
				t.Fatalf("apply %s: %v", name, err)
			}
		}
	}
}

// TestMigration0173_HostingBecomesScope applies the pool-scope migration over a catalogue from before
// it: hosting_type becomes scope and is dropped; executor members become remote_runner_id members with a
// row each and a kind; the organisation defaults keep the remote slot as their one pool and the
// barrier, and lose the preference and the self-hosted slot; every personal v1 document becomes a v2
// document naming the pool it resolved to, or goes; a live organisation pool called "Remote Provided"
// is "Organization". Afterwards the schema refuses an owned organization pool, a live ownerless user
// pool, a member naming a remote runner with no row, and a remote runner id that reads as a UUID.
func TestMigration0173_HostingBecomesScope(t *testing.T) {
	pool, _ := partialSchemaPool(t, runnerPoolScopeMigration)
	ctx := context.Background()
	remote, other, mine := uuid.New(), uuid.New(), uuid.New()
	pgExec(t, pool, `INSERT INTO runner_pools (id, name, hosting_type) VALUES ($1, 'Remote Provided', 'remote_provided'), ($2, 'Builds', 'remote_provided')`, remote, other)
	pgExec(t, pool, `INSERT INTO runner_pools (id, name, hosting_type, owner) VALUES ($1, 'Laptop', 'self_hosted', 'sub-a')`, mine)
	runner := uuid.New()
	pgExec(t, pool, `INSERT INTO runners (id, owner, public_key, key_fingerprint, state, max_runs) VALUES ($1, 'sub-a', $2, $3, 'claimed', 2)`,
		runner, append(runner[:], runner[:]...), "fp-"+runner.String())
	pgExec(t, pool, `INSERT INTO runner_pool_members (pool_id, executor_id) VALUES ($1, 'docker'), ($2, 'docker'), ($2, 'k8s')`, remote, other)
	pgExec(t, pool, `INSERT INTO runner_pool_members (pool_id, runner_id) VALUES ($1, $2)`, mine, runner)
	pgExec(t, pool, `INSERT INTO runner_pool_org_defaults (preferred_hosting, remote_provided, barrier) VALUES ('remote_provided', $1, 'CC2')`, remote)
	docs := map[string]string{
		// preferred self_hosted: the self-hosted slot.
		"sub-a": `{"preferred_hosting": "self_hosted", "self_hosted": "` + mine.String() + `", "remote_provided": "` + other.String() + `"}`,
		// no preference of their own: the organisation's (remote_provided) slot.
		"sub-b": `{"remote_provided": "` + other.String() + `", "self_hosted": "` + mine.String() + `"}`,
		// a preference whose slot is empty: no default, the organisation's applies.
		"sub-c": `{"preferred_hosting": "self_hosted", "remote_provided": "` + other.String() + `"}`,
		// nothing named.
		"sub-d": `{}`,
	}
	for who, doc := range docs {
		pgExec(t, pool, `INSERT INTO principal_prefs (principal, key, value) VALUES ($1, 'runner_pool_defaults.v1', $2::jsonb)`, who, doc)
	}
	pgExec(t, pool, `INSERT INTO governance_changes (id, target_kind, op, target_key, payload, diff, base_hash, deployment_hash, proposed_by, expires_at)
		VALUES (gen_random_uuid(), 'runner_pool_use_policy', 'set', $1, '{"executor":"k8s"}', '{}', 'h', 'd', 'sub-x', now() + interval '1 day')`, remote)

	migrateBefore(t, pool, "0174")

	for id, want := range map[uuid.UUID]string{remote: "organization:Organization", other: "organization:Builds", mine: "user:Laptop"} {
		if got := pgScalar[string](t, pool, `SELECT scope || ':' || name FROM runner_pools WHERE id = $1`, id); got != want {
			t.Errorf("pool %s = %q, want %q", id, got, want)
		}
	}
	if n := pgScalar[int](t, pool, `SELECT count(*)::int FROM information_schema.columns WHERE table_schema = current_schema() AND table_name = 'runner_pools' AND column_name = 'hosting_type'`); n != 0 {
		t.Error("hosting_type survived")
	}
	if got := pgScalar[string](t, pool, `SELECT string_agg(id || '=' || name, ',' ORDER BY id) FROM remote_runners`); got != "docker=Docker host,k8s=Kubernetes cluster" {
		t.Errorf("remote runners = %q", got)
	}
	if got := pgScalar[string](t, pool, `SELECT string_agg(kind || ':' || COALESCE(remote_runner_id, runner_id::text), ',' ORDER BY kind, remote_runner_id) FROM runner_pool_members WHERE pool_id IN ($1, $2)`, other, mine); got != "remote_hosted:docker,remote_hosted:k8s,self_hosted:"+runner.String() {
		t.Errorf("members = %q", got)
	}
	if got := pgScalar[string](t, pool, `SELECT pool_id::text || ':' || barrier FROM runner_pool_org_defaults`); got != remote.String()+":CC2" {
		t.Errorf("organisation defaults = %q, want the remote slot and the barrier kept", got)
	}
	for _, col := range []string{"preferred_hosting", "self_hosted", "remote_provided"} {
		if n := pgScalar[int](t, pool, `SELECT count(*)::int FROM information_schema.columns WHERE table_schema = current_schema() AND table_name = 'runner_pool_org_defaults' AND column_name = $1`, col); n != 0 {
			t.Errorf("runner_pool_org_defaults.%s survived", col)
		}
	}
	if n := pgScalar[int](t, pool, `SELECT count(*)::int FROM principal_prefs WHERE key = 'runner_pool_defaults.v1'`); n != 0 {
		t.Errorf("%d v1 documents survived", n)
	}
	for who, want := range map[string]string{"sub-a": mine.String(), "sub-b": other.String(), "sub-c": "", "sub-d": ""} {
		got := pgScalar[string](t, pool, `SELECT COALESCE((SELECT value->>'pool_id' FROM principal_prefs WHERE principal = $1 AND key = 'runner_pool_default.v2'), '')`, who)
		if got != want {
			t.Errorf("%s's v2 default = %q, want %q", who, got, want)
		}
	}
	if got := pgScalar[string](t, pool, `SELECT payload::text FROM governance_changes`); got != `{"remote_runner_id": "k8s"}` {
		t.Errorf("held change payload = %s", got)
	}

	refused := map[string]string{
		"an owned organization pool":         `INSERT INTO runner_pools (id, name, scope, owner) VALUES (gen_random_uuid(), 'y', 'organization', 'sub-a')`,
		"a live ownerless user pool":         `INSERT INTO runner_pools (id, name, scope) VALUES (gen_random_uuid(), 'x', 'user')`,
		"a scope outside the two":            `INSERT INTO runner_pools (id, name, scope) VALUES (gen_random_uuid(), 'w', 'shared')`,
		"a member with no remote runner row": `INSERT INTO runner_pool_members (pool_id, remote_runner_id) VALUES ('` + other.String() + `', 'nomad')`,
		"a remote runner id that is a UUID":  `INSERT INTO remote_runners (id, name) VALUES ('` + uuid.NewString() + `', 'u')`,
		"listed people with no list":         `INSERT INTO remote_runners (id, name, people_pools) VALUES ('nomad', 'n', 'listed')`,
		"a list on a runner that is off":     `INSERT INTO remote_runners (id, name, people_subjects) VALUES ('nomad', 'n', '[]')`,
	}
	for what, stmt := range refused {
		if _, err := pool.Exec(ctx, stmt); err == nil {
			t.Errorf("the schema accepts %s", what)
		}
	}
}

// TestMigration0173_FindsTheMembersTableByCatalog: the pool-scope migration finds runner_pool_members by the
// catalog, not by the first schema of the search path. With an empty schema ahead of the real one on the
// path, current_schema() names the empty one, and a lookup keyed on it finds no executor_id column, skips the
// conversion, and leaves the members unconverted for 0174 to trip over.
func TestMigration0173_FindsTheMembersTableByCatalog(t *testing.T) {
	pool, schema := partialSchemaPool(t, runnerPoolScopeMigration)
	ctx := context.Background()
	decoy := schema + "_decoy"
	pgExec(t, pool, `CREATE SCHEMA `+decoy)
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DROP SCHEMA IF EXISTS `+decoy+` CASCADE`) })
	remote := uuid.New()
	pgExec(t, pool, `INSERT INTO runner_pools (id, name, hosting_type) VALUES ($1, 'Remote Provided', 'remote_provided')`, remote)
	pgExec(t, pool, `INSERT INTO runner_pool_members (pool_id, executor_id) VALUES ($1, 'docker')`, remote)

	u, err := url.Parse(os.Getenv("WARDYN_TEST_PG"))
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	q.Set("search_path", decoy+","+schema)
	u.RawQuery = q.Encode()
	ahead, err := Connect(ctx, u.String())
	if err != nil {
		t.Fatalf("connect with the decoy schema first: %v", err)
	}
	t.Cleanup(ahead.Close)
	migrateBefore(t, ahead, "0175")

	if got := pgScalar[string](t, pool, `SELECT kind || ':' || remote_runner_id FROM runner_pool_members WHERE pool_id = $1`, remote); got != "remote_hosted:docker" {
		t.Errorf("member = %q, want it converted to remote_hosted:docker", got)
	}
}

// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package db

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
)

// TestPG_WindowsLeaseTriggersWorkOutsideThePublicSchema migrates a whole schema into a throwaway namespace and
// runs the Windows licence take, fence and release there. Each trigger function names its tables through the
// install's own schema and runs with a path of pg_catalog then pg_temp, so it never writes the public schema's
// table of the same name, and never resolves a name through a schema other roles may write.
func TestPG_WindowsLeaseTriggersWorkOutsideThePublicSchema(t *testing.T) {
	pool, schema := probeSchemaPool(t)
	ctx := context.Background()

	for _, fn := range []string{"windows_licence_take", "windows_licence_release", "windows_vm_fence"} {
		got := pgScalar[string](t, pool, `SELECT pg_catalog.array_to_string(p.proconfig, ';') FROM pg_catalog.pg_proc p
			JOIN pg_catalog.pg_namespace n ON n.oid = p.pronamespace WHERE n.nspname = $1 AND p.proname = $2`, schema, fn)
		if got != "search_path=pg_catalog, pg_temp" {
			t.Errorf("%s.%s runs with %q, want search_path=pg_catalog, pg_temp", schema, fn, got)
		}
		if !pgScalar[bool](t, pool, `SELECT p.prosecdef FROM pg_catalog.pg_proc p
			JOIN pg_catalog.pg_namespace n ON n.oid = p.pronamespace WHERE n.nspname = $1 AND p.proname = $2`, schema, fn) {
			t.Errorf("%s.%s is not SECURITY DEFINER", schema, fn)
		}
	}

	pgExec(t, pool, `DELETE FROM site_config`)
	pgExec(t, pool, `INSERT INTO site_config (singleton, config) VALUES (true, '{"features":{"windows_vm":{"enabled":true}}}'::jsonb)`)
	poolID, imageID := uuid.New(), uuid.New()
	ref := "registry.example/windows/server@sha256:" + strings.Repeat("ab", 32)
	pgExec(t, pool, `INSERT INTO windows_licence_pools (id, name, seats, created_by) VALUES ($1, 'Engineering', 1, 'admin')`, poolID)
	pgExec(t, pool, `INSERT INTO windows_images (id, name, ref, licence_pool_id, created_by) VALUES ($1, 'Server', $2, $3, 'admin')`, imageID, ref, poolID)
	pgExec(t, pool, `INSERT INTO windows_licence_holders (id, pool_id, subject, added_by) VALUES ($1, $2, 'ana@example.com', 'admin')`, uuid.New(), poolID)

	run := uuid.New()
	pgExec(t, pool, `INSERT INTO agent_runs (id, created_by, agent, repo, confinement_class, state, spiffe_id, runner_target, machine_kind, image)
		VALUES ($1, 'ana@example.com', 'claude-code', 'repo', 'CC1', 'PENDING', $2, 'docker', 'windows_vm', $3)`, run, "spiffe://t/"+run.String(), ref)
	if n := pgScalar[int64](t, pool, `SELECT count(*) FROM windows_licence_leases WHERE run_id = $1`, run); n != 1 {
		t.Fatalf("the take wrote %d seats in schema %s, want 1", n, schema)
	}
	if n := pgScalar[int64](t, pool, `SELECT count(*) FROM public.windows_licence_leases WHERE run_id = $1`, run); n != 0 {
		t.Fatalf("the take wrote the public schema's table")
	}

	_, err := pool.Exec(ctx, `UPDATE agent_runs SET created_by = 'bo@example.com' WHERE id = $1`, run)
	var pe *pgconn.PgError
	if !errors.As(err, &pe) || pe.Code != "23514" || pe.ConstraintName != "windows_vm_holder_fixed" {
		t.Fatalf("a change of creator in schema %s = %v, want 23514 windows_vm_holder_fixed", schema, err)
	}

	pgExec(t, pool, `UPDATE agent_runs SET state = 'KILLED' WHERE id = $1`, run)
	if n := pgScalar[int64](t, pool, `SELECT count(*) FROM windows_licence_leases WHERE run_id = $1`, run); n != 0 {
		t.Fatalf("the release left %d seats in schema %s", n, schema)
	}
}

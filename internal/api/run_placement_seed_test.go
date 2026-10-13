// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// seedRunPool gives a Postgres-backed test database what every launch now needs: one
// organization pool holding the remote-hosted runners the test runners use, as the organisation's
// default pool. It is the state a deployment is in once its remote pool exists and an administrator has chosen
// it. A member naming an executor the test deployment does not run is simply not a candidate. Idempotent,
// because several tests share one database.
func seedRunPool(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	var exists bool
	if err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM runner_pool_org_defaults)`).Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if exists {
		return
	}
	id := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO remote_runners (id, name) SELECT unnest(ARRAY['fake','docker','k8s']), 'test' ON CONFLICT DO NOTHING`); err != nil {
		t.Fatalf("seed remote runners: %v", err)
	}
	for _, q := range []string{
		`INSERT INTO runner_pools (id, name, scope, created_by) VALUES ($1, 'Test remote pool', 'organization', 'test')`,
		`INSERT INTO runner_pool_members (pool_id, remote_runner_id) SELECT $1, unnest(ARRAY['fake','docker','k8s'])`,
		`INSERT INTO runner_pool_org_defaults (pool_id, updated_by) VALUES ($1, 'test')`,
	} {
		if _, err := pool.Exec(ctx, q, id); err != nil {
			t.Fatalf("seed run pool: %v", err)
		}
	}
}

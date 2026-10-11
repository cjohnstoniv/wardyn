// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// seedRunPool gives a Postgres-backed test database what every launch now needs: one remote-provided
// pool holding the executor names the test runners use, as the organisation's default hosting type and
// pool. It is the state a deployment is in once its remote pool exists and an administrator has chosen
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
	for _, q := range []string{
		`INSERT INTO runner_pools (id, name, hosting_type, created_by) VALUES ($1, 'Test remote pool', 'remote_provided', 'test')`,
		`INSERT INTO runner_pool_members (pool_id, executor_id) SELECT $1, unnest(ARRAY['fake','docker','k8s'])`,
		`INSERT INTO runner_pool_org_defaults (preferred_hosting, remote_provided, updated_by) VALUES ('remote_provided', $1, 'test')`,
	} {
		if _, err := pool.Exec(ctx, q, id); err != nil {
			t.Fatalf("seed run pool: %v", err)
		}
	}
}

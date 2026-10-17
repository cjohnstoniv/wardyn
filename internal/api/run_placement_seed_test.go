// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// seedRunPool gives a Postgres-backed test database what every launch now needs: one
// organization pool holding the remote-hosted runners the test runners use, as the organisation's
// default pool. It is the state a deployment is in once its remote pool exists and an administrator has chosen
// it. A member naming an executor the test deployment does not run is simply not a candidate. Idempotent,
// because several tests share one database.
func seedRunPool(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	// Every remote-hosted runner row a test made (here, or at a server's boot) declares roomy limits.
	defer func() {
		l := roomyRemoteLimits()
		if _, err := pool.Exec(ctx, `UPDATE remote_runners SET max_runs = $1,
			total_cpu_millis = $2, total_memory_mib = $3, total_disk_mib = $4, run_cpu_millis = $5, run_memory_mib = $6, run_disk_mib = $7`,
			l.RunsAtOnce, l.Total.CPUMillis, l.Total.MemoryMiB, l.Total.DiskMiB, l.PerRun.CPUMillis, l.PerRun.MemoryMiB, l.PerRun.DiskMiB); err != nil {
			t.Fatalf("give remote runners limits: %v", err)
		}
	}()
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

// roomyRunnerLimits is a self-hosted runner's limit set with the most runs at once and the largest
// totals a runner may declare, so a fixture's runs are never refused for its limits. A run that names
// no disk gets its runner's per-run disk, so that is the total's share of each run at once.
func roomyRunnerLimits() *types.RunnerLimits {
	most := types.RunnerAmounts{CPUMillis: types.RunnerPoolCPUMillisMax, MemoryMiB: types.RunnerPoolMemoryMiBMax, DiskMiB: types.RunnerPoolDiskMiBMax}
	each := most
	each.DiskMiB /= types.RunnerMaxRunsCeiling
	return &types.RunnerLimits{RunsAtOnce: types.RunnerMaxRunsCeiling, Total: most, PerRun: each}
}

// roomyRemoteLimits is a remote-hosted runner's limit set with the most runs at once and the largest
// totals one may declare; each run's disk is the total's share of each run at once.
func roomyRemoteLimits() types.RunnerLimits {
	most := types.RunnerAmounts{CPUMillis: types.RunnerPoolCPUMillisMax, MemoryMiB: types.RunnerPoolMemoryMiBMax, DiskMiB: types.RunnerPoolDiskMiBMax}
	each := most
	each.DiskMiB /= types.RemoteRunnerMaxRunsCeiling
	return types.RunnerLimits{RunsAtOnce: types.RemoteRunnerMaxRunsCeiling, Total: most, PerRun: each}
}

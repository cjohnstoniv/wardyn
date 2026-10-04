// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/db"
)

// claimSingleInstance is the RUNTIME half of the one-replica safety control:
// it takes db.SingleInstanceLockKey and returns the release to defer for the
// process lifetime, refusing to boot when another instance already holds it.
// The Helm chart's `replicas > 1` render refusal is the other half, and it is
// render-time only — kubectl scale, an HPA or a non-Helm replica edit defeated
// it with no signal at all. See db.SingleInstanceLockKey for the failure this
// guards and for the honest ceiling (an advisory lock dies with its SESSION, so
// a Postgres failover releases it under a live daemon: at most one steady-state
// instance, not mutual exclusion).
//
// ha is WARDYN_HA (validateHAPosture has already required the Kubernetes runner
// and a shared recording store): it skips the claim, because several replicas
// serving one database is the point. The masking registry, the cross-replica
// locks and the sweeper leader election are what make that safe, and none of
// them needs this lock.
func claimSingleInstance(ctx context.Context, pool *pgxpool.Pool, ha bool) (func(), error) {
	if ha {
		// A pool below the minimum makes every replica's sweeper leader run solo
		// (epoch 0, always current): each replica sweeps with no election and no
		// fencing, which defeats the leader-only guarantees HA exists for.
		if mc := pool.Config().MaxConns; mc < db.SweeperLeaderMinConns {
			return nil, fmt.Errorf("refusing to start: WARDYN_HA is set but pool_max_conns=%d; the sweeper leader election needs a pool of at least %d so it can hold its lock. "+
				"Raise pool_max_conns in WARDYN_PG_DSN, or run one replica without WARDYN_HA", mc, db.SweeperLeaderMinConns)
		}
		slog.Info("wardynd: WARDYN_HA is set; the single-instance lock is not taken, and other replicas may serve this database")
		return func() {}, nil
	}
	// The lock holds ONE pooled connection for the whole process lifetime, and
	// pgxpool.Acquire BLOCKS rather than erroring when the pool is empty — a
	// 1-conn pool would hang every request instead of failing visibly.
	if mc := pool.Config().MaxConns; mc < 2 {
		slog.Warn("wardynd: pool_max_conns below 2 — the single-instance lock holds one connection for the process lifetime, leaving none for requests; queries will block waiting for a connection rather than error",
			slog.Int("pool_max_conns", int(mc)))
	}
	release, ok, err := db.TryAdvisoryLock(ctx, pool, db.SingleInstanceLockKey)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, errors.New("refusing to start: another wardynd already holds this database's single-instance lock. " +
			"Without high availability wardynd keeps state per-process that a second instance cannot see: the sandbox " +
			"tracking of the Docker runner, and the rate limiters and connection caps. Stop the other instance, or run " +
			"several replicas on purpose with WARDYN_HA=true (the chart's ha.enabled), which needs the Kubernetes runner " +
			"and a shared recording store (docs/OPERATIONS.md, \"High availability\")")
	}
	slog.Info("wardynd: single-instance lock held for the process lifetime")
	return release, nil
}

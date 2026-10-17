// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
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
		slog.Info("wardynd: WARDYN_HA is set; the single-instance lock is not taken, and other replicas may serve this database")
		return func() {}, nil
	}
	// A connection of its own, not a pooled one: held for the process lifetime,
	// it would leave a small pool that much shorter for every request.
	_, release, ok, err := db.TryAdvisoryLockDedicated(ctx, pool, db.SingleInstanceLockKey)
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

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
// allow is -allow-multi-instance, the runtime twin of the chart's
// allowMultiReplica: it skips the claim entirely rather than logging a refusal
// nobody can act on. It warns every boot on purpose — an acknowledged ceiling
// that goes quiet is one nobody remembers accepting.
func claimSingleInstance(ctx context.Context, pool *pgxpool.Pool, allow bool) (func(), error) {
	if allow {
		slog.Warn("wardynd: -allow-multi-instance is set — the single-instance lock is NOT taken. wardynd's secret-masking registry (internal/secretmask) is process-local and FAILS OPEN, so a recording uploaded to an instance that did not serve the run's proxy injection is persisted verbatim, live credentials in cleartext, with a `success` audit event.")
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
			"wardynd keeps state per-process that a second instance cannot see — the sharpest is the secret-masking " +
			"registry (internal/secretmask), an in-memory, process-local map that FAILS OPEN: secrets are registered on " +
			"whichever instance served the run's proxy injection, so a session recording uploaded to any other instance " +
			"finds an empty snapshot and is persisted VERBATIM, live credentials in cleartext, with a `success` audit " +
			"event. The same holds for the live-attach cast. Stop the other instance (docs/OPERATIONS.md, \"One replica, " +
			"by construction\"), or pass -allow-multi-instance to start anyway and accept that (the chart's allowMultiReplica)")
	}
	slog.Info("wardynd: single-instance lock held for the process lifetime")
	return release, nil
}

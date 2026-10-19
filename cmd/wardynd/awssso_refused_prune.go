// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/api"
	"github.com/cjohnstoniv/wardyn/internal/store"
)

// pruneAWSSSORefusedTokens is pruneAWSSSOSpentTokens for the refused marks: the same retention, since
// past the registration's lifetime the refresh token behind a mark could not be renewed anyway.
// Best-effort, under the reap advisory lock.
func pruneAWSSSORefusedTokens(ctx context.Context, pool *pgxpool.Pool) {
	n, err := store.NewPG(pool).PruneAWSSSORefusedTokens(ctx, time.Now().Add(-api.AWSSSOSpentTokenRetention))
	if err != nil {
		slog.WarnContext(ctx, "wardynd: pruning aws sso refused tokens failed", slog.Any("err", err))
		return
	}
	if n > 0 {
		slog.InfoContext(ctx, "wardynd: pruned stale aws sso refused-token rows", slog.Int("deleted", n))
	}
}

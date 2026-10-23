// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/store"
)

// pruneOIDCSessionSIDs deletes the front-channel logout sid rows (migration 0229) of sign-ins older
// than maxOIDCSessionTTL, the longest a session setting lets a console session last (an ID token that
// itself lasts longer keeps its session past the row). Best-effort, under the reap advisory lock, as
// pruneAWSSSORefusedTokens.
func pruneOIDCSessionSIDs(ctx context.Context, pool *pgxpool.Pool) {
	n, err := store.NewPG(pool).PruneOIDCSessionSIDs(ctx, time.Now().Add(-maxOIDCSessionTTL))
	if err != nil {
		slog.WarnContext(ctx, "wardynd: pruning oidc session sids failed", slog.Any("err", err))
		return
	}
	if n > 0 {
		slog.InfoContext(ctx, "wardynd: pruned stale oidc session sid rows", slog.Int("deleted", n))
	}
}

// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package vaultkv

import (
	"context"
	"sync"

	"github.com/jackc/pgx/v5"
)

// The row flip has committed but its session lock is still held. Queue the
// replacement here so it wins the lock before the migrator's cleanup.
type migrationCommitBarrier struct {
	once  sync.Once
	after func()
}

func (b *migrationCommitBarrier) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	return ctx
}
func (b *migrationCommitBarrier) TraceQueryEnd(_ context.Context, _ *pgx.Conn, d pgx.TraceQueryEndData) {
	if d.Err == nil && d.CommandTag.String() == "COMMIT" {
		b.once.Do(b.after)
	}
}

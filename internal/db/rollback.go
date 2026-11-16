// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package db

import (
	"context"
	"time"
)

// RollbackBound is the longest Rollback waits for the database's answer. A rollback not answered by then closes the
// connection, and the database rolls the transaction back when it sees the connection gone. A variable only so a test
// can shorten it.
var RollbackBound = 5 * time.Second

// Rollback is the deferred rollback of tx: a no-op after Commit. It keeps ctx's values but not its cancellation, so a
// caller that was cancelled still rolls back cleanly, and it ends by ctx's deadline or RollbackBound, whichever comes
// first, so it never holds its caller past the caller's own bound. Once that deadline has passed nothing is sent: pgx
// closes the connection at once, which ends the transaction on the server. A caller whose connection must outlive
// ctx (a migration's own session) passes context.WithoutCancel(ctx), and so waits up to RollbackBound whatever ctx's
// deadline. A plain tx.Rollback(context.WithoutCancel(ctx)) has no bound at all on a connection that stopped
// answering; the repo's guard (internal/db/rollbackguard) refuses it.
func Rollback(ctx context.Context, tx interface{ Rollback(context.Context) error }) {
	d := RollbackBound
	if dl, ok := ctx.Deadline(); ok {
		d = min(d, time.Until(dl))
	}
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), d)
	defer cancel()
	_ = tx.Rollback(rctx)
}

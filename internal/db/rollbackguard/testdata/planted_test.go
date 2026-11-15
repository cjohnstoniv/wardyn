// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Planted rollbacks for rollbackguard's own test. It is under testdata, so it is never compiled.
package planted

func planted(ctx, rctx context.Context, tx pgx.Tx, s store) {
	defer tx.Rollback(context.WithoutCancel(ctx))                              // refused
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()             // refused
	_ = tx.Rollback(context.Background())                                      // refused
	tx.Rollback(context.TODO())                                                // refused
	_ = s.tx.Rollback(context.WithValue(context.WithoutCancel(ctx), key{}, 1)) // refused
	_ = tx.Rollback(                                                           // refused
		context.WithoutCancel(ctx),
	)
	defer tx.Rollback(ctx)                      // allowed: the caller's own context
	defer db.Rollback(ctx, tx)                  // allowed: the bounded form
	db.Rollback(context.WithoutCancel(ctx), tx) // allowed: the bounded form on a bound of its own
	_ = tx.Rollback(rctx)                       // allowed: a context with a bound of its own
}

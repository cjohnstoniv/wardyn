// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Planted rollbacks for rollbackguard's own test. It is under testdata, so it is never compiled.
package planted

import (
	"context"
	. "context"
	stdctx "context"
	"time"
)

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

// The file's own names for the context package: an alias and a dot import.
func plantedImportNames(ctx context.Context, tx pgx.Tx) {
	_ = tx.Rollback(stdctx.WithoutCancel(ctx)) // refused
	_ = tx.Rollback(Background())              // refused
}

// A detached context held in a name, passed on through another name, or bounded after.
func plantedNames(ctx context.Context, tx pgx.Tx) {
	dctx := context.WithoutCancel(ctx)
	_ = tx.Rollback(dctx) // refused
	var bg context.Context = context.Background()
	vctx := context.WithValue(bg, key{}, 1)
	_ = tx.Rollback(vctx) // refused
	cctx, cancel := context.WithCancel(context.TODO())
	defer cancel()
	_ = tx.Rollback(cctx) // refused
	bctx, stop := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
	defer stop()
	_ = tx.Rollback(bctx) // allowed: detached, then bounded
}

// The caller's ctx given a detached context.
func plantedReassigned(ctx context.Context, tx pgx.Tx) {
	ctx = context.Background()
	defer tx.Rollback(ctx) // refused
}

// A method value, and a call through parentheses.
func plantedMethodValue(ctx context.Context, tx pgx.Tx) {
	undo := tx.Rollback
	defer undo(context.WithoutCancel(ctx)) // refused
	defer undo(ctx)                        // allowed: the caller's own context
	(tx.Rollback)(context.Background())    // refused
}

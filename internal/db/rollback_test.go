// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package db

import (
	"context"
	"testing"
	"time"
)

type ctxKey struct{}

// seenTx records the context its Rollback ran on, and that context's error while it ran.
type seenTx struct {
	ctx context.Context
	err error
}

func (t *seenTx) Rollback(ctx context.Context) error { t.ctx, t.err = ctx, ctx.Err(); return nil }

// rolledBackOn runs Rollback on ctx and answers what the transaction saw.
func rolledBackOn(t *testing.T, ctx context.Context) *seenTx {
	t.Helper()
	tx := &seenTx{}
	Rollback(ctx, tx)
	if tx.ctx == nil {
		t.Fatal("Rollback did not roll the transaction back")
	}
	return tx
}

// Rollback ends by the caller's deadline or RollbackBound, whichever comes first, and ignores the caller's
// cancellation. Catches: the rollback on context.WithoutCancel alone (no deadline at all), and a bound that
// outlasts the caller's deadline.
func TestRollback_EndsByTheCallersDeadlineOrItsBound(t *testing.T) {
	base := context.WithValue(t.Context(), ctxKey{}, "v")
	for _, c := range []struct {
		name string
		ctx  func() (context.Context, context.CancelFunc)
		want time.Duration // the rollback's deadline, from now
	}{
		{"no deadline", func() (context.Context, context.CancelFunc) { return context.WithCancel(base) }, RollbackBound},
		{"a deadline past the bound", func() (context.Context, context.CancelFunc) { return context.WithTimeout(base, time.Hour) }, RollbackBound},
		{"a deadline before the bound", func() (context.Context, context.CancelFunc) { return context.WithTimeout(base, time.Second) }, time.Second},
		{"the caller's deadline detached", func() (context.Context, context.CancelFunc) {
			ctx, cancel := context.WithTimeout(base, time.Second)
			return context.WithoutCancel(ctx), cancel
		}, RollbackBound},
	} {
		t.Run(c.name, func(t *testing.T) {
			ctx, cancel := c.ctx()
			defer cancel()
			start := time.Now()
			rctx := rolledBackOn(t, ctx).ctx
			dl, ok := rctx.Deadline()
			if !ok {
				t.Fatal("the rollback ran with no deadline")
			}
			if got := dl.Sub(start); got > c.want+time.Second/2 || got < c.want-time.Second/2 {
				t.Errorf("the rollback's deadline is %v away, want %v", got, c.want)
			}
			if rctx.Value(ctxKey{}) != "v" {
				t.Error("the rollback lost the caller's values")
			}
		})
	}
}

// A caller that was cancelled still rolls back on a live context; one whose deadline has passed rolls back on a
// context already ended, so pgx sends nothing and closes the connection at once.
func TestRollback_ACancelledCallerRollsBackAndAnExpiredOneDoesNotWait(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := rolledBackOn(t, ctx).err; err != nil {
		t.Errorf("a cancelled caller's rollback ran on an ended context: %v", err)
	}
	ctx, cancel = context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
	defer cancel()
	if err := rolledBackOn(t, ctx).err; err == nil {
		t.Error("an expired caller's rollback ran on a live context: it could wait past the caller's deadline")
	}
}

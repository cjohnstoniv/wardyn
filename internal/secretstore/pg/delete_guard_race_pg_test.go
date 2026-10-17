// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package pg

// The guarded Delete's "expected no row, found none" exit. A local-mode Put takes no advisory lock,
// so a row can land between the guard's locked read and the delete; the guard must then leave it.
// Guarded by WARDYN_TEST_PG.

import (
	"context"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/secretstore"
)

// afterQuery runs fn once, on the connection's side, when a statement containing marker finishes.
// The pool's query tracer is the seam, so production code carries no test hook.
type afterQuery struct {
	marker string
	fn     func()
	fired  atomic.Bool
}

type afterQueryKey struct{}

func (a *afterQuery) TraceQueryStart(ctx context.Context, _ *pgx.Conn, d pgx.TraceQueryStartData) context.Context {
	if strings.Contains(d.SQL, a.marker) {
		return context.WithValue(ctx, afterQueryKey{}, true)
	}
	return ctx
}

func (a *afterQuery) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryEndData) {
	if ctx.Value(afterQueryKey{}) != nil && a.fired.CompareAndSwap(false, true) {
		a.fn()
	}
}

func TestDelete_GuardedAbsentRowLeavesARowThatLandsBeforeTheDelete(t *testing.T) {
	s, _, id := newPGStore(t)
	ctx := t.Context()
	cfg, err := pgxpool.ParseConfig(os.Getenv("WARDYN_TEST_PG"))
	if err != nil {
		t.Fatal(err)
	}
	owner, name := "guard-race-"+uuid.NewString(), "wardyn-delete-guard-race"
	writer := s.For(owner)
	hook := &afterQuery{marker: "FOR UPDATE", fn: func() {
		// The guard has read "no row" under FOR UPDATE; a local Put now lands one.
		if err := writer.Put(context.WithoutCancel(ctx), name, []byte("landed")); err != nil {
			t.Errorf("Put inside the window: %v", err)
		}
	}}
	cfg.ConnConfig.Tracer = hook
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	traced, err := New(pool, id)
	if err != nil {
		t.Fatal(err)
	}
	if err := traced.For(owner).Delete(secretstore.WithIfRevision(ctx, ""), name); err != nil {
		t.Fatalf("guarded Delete expecting no row: %v", err)
	}
	if !hook.fired.Load() {
		t.Fatal("the guard's FOR UPDATE read never ran, so the window was not exercised")
	}
	if got := mustGetValue(t, writer, name); got != "landed" {
		t.Fatalf("the row that landed after the guard's read = %q, want it kept", got)
	}
}

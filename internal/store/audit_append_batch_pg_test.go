// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/db"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// batchTracer records, per pgx batch, the SQL of its queries, and the SQL of every query sent on its own.
type batchTracer struct {
	mu      sync.Mutex
	batches [][]string
	alone   []string
}

func (b *batchTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, d pgx.TraceQueryStartData) context.Context {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.alone = append(b.alone, d.SQL)
	return ctx
}

func (b *batchTracer) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func (b *batchTracer) TraceBatchStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceBatchStartData) context.Context {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.batches = append(b.batches, nil)
	return ctx
}

func (b *batchTracer) TraceBatchQuery(_ context.Context, _ *pgx.Conn, d pgx.TraceBatchQueryData) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.batches[len(b.batches)-1] = append(b.batches[len(b.batches)-1], d.SQL)
}

func (b *batchTracer) TraceBatchEnd(context.Context, *pgx.Conn, pgx.TraceBatchEndData) {}

// tracedPool is a second pool over the same database as base, whose connections report to the returned tracer.
func tracedPool(t *testing.T, base *pgxpool.Pool) (*pgxpool.Pool, *batchTracer) {
	t.Helper()
	tr := &batchTracer{}
	cfg := base.Config().Copy()
	cfg.ConnConfig.Tracer = tr
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("open traced pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool, tr
}

func auditProbeEvent() types.AuditEvent {
	return types.AuditEvent{
		ID: uuid.New(), Time: time.Now().UTC(), ActorType: types.ActorSystem, Actor: "test",
		Action: "test.append", Target: "t", Outcome: "success",
	}
}

// requireOneAppendBatch asserts exactly one batch of the three append statements, and no lone chain lock query.
func requireOneAppendBatch(t *testing.T, tr *batchTracer) {
	t.Helper()
	tr.mu.Lock()
	defer tr.mu.Unlock()
	if len(tr.batches) != 1 || len(tr.batches[0]) != 3 {
		t.Fatalf("batches = %q, want one batch of three queries", tr.batches)
	}
	b := tr.batches[0]
	if !strings.Contains(b[0], "lock_timeout") || !strings.Contains(b[1], "pg_advisory_xact_lock") || !strings.Contains(b[2], "audit_append") {
		t.Errorf("batch = %q, want the lock wait bound, the chain lock and the append, in that order", b)
	}
	for _, sql := range tr.alone {
		if strings.Contains(sql, "pg_advisory_xact_lock") {
			t.Errorf("the chain lock was also sent on its own: %q", sql)
		}
	}
}

func TestPG_AuditAppendSendsTheLockAndTheAppendInOneBatch(t *testing.T) {
	base, _ := lockProbeDB(t)
	pool, tr := tracedPool(t, base)
	ev := auditProbeEvent()
	if err := store.InsertAuditEvent(context.Background(), pool, &ev); err != nil {
		t.Fatalf("InsertAuditEvent: %v", err)
	}
	requireOneAppendBatch(t, tr)
	if ev.RowHash == "" {
		t.Error("the event carries no row hash")
	}
}

func TestPG_AuditAppendTxSendsOneBatch(t *testing.T) {
	base, _ := lockProbeDB(t)
	pool, tr := tracedPool(t, base)
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // best-effort
	ev := auditProbeEvent()
	if err := store.InsertAuditEventTx(ctx, tx, &ev); err != nil {
		t.Fatalf("InsertAuditEventTx: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	requireOneAppendBatch(t, tr)
	if ev.RowHash == "" {
		t.Error("the event carries no row hash")
	}
}

// A write that cannot get the chain lock appends nothing, leaves its event with no hashes, and does not cost the
// pool a connection: without the batch's deferred Close, the pipeline stays open, the rollback finds the connection
// busy and pgx destroys it, and the next call has to dial a new one.
func TestPG_AuditAppendAFailedLockAppendsNothing(t *testing.T) {
	pool, dsn := lockProbeDB(t)
	restore := db.AuditChainLockTimeout
	db.AuditChainLockTimeout = 400 * time.Millisecond
	t.Cleanup(func() { db.AuditChainLockTimeout = restore })
	ctx := context.Background()

	warm := auditProbeEvent()
	if err := store.InsertAuditEvent(ctx, pool, &warm); err != nil {
		t.Fatalf("warm-up InsertAuditEvent: %v", err)
	}
	before := pool.Stat().NewConnsCount()

	release := holdChainLock(t, dsn)
	ev := auditProbeEvent()
	err := store.InsertAuditEvent(ctx, pool, &ev)
	release()

	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "55P03" {
		t.Fatalf("InsertAuditEvent = %v, want a 55P03 lock timeout", err)
	}
	if n := auditRowsWithID(t, pool, ev.ID); n != 0 {
		t.Errorf("%d rows carry the failed event's id, want 0", n)
	}
	if ev.RowHash != "" || ev.PrevHash != "" {
		t.Errorf("the failed event carries hashes %q/%q", ev.PrevHash, ev.RowHash)
	}

	good := auditProbeEvent()
	if err := store.InsertAuditEvent(ctx, pool, &good); err != nil {
		t.Fatalf("InsertAuditEvent after the lock cleared: %v", err)
	}
	if after := pool.Stat().NewConnsCount(); after != before {
		t.Errorf("the pool dialled %d new connections across the failed call and the next: the failed one was destroyed", after-before)
	}
}

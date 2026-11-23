// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/db"
	"github.com/cjohnstoniv/wardyn/internal/store"
)

// slotWaitersSQL counts the sessions waiting for the audit chain lock in this database only: advisory locks are
// per database, and the key is split into the class and object halves pg_locks shows.
const slotWaitersSQL = `SELECT count(*) FROM pg_locks WHERE locktype = 'advisory' AND NOT granted AND objsubid = 1
	AND database = (SELECT oid FROM pg_database WHERE datname = current_database())
	AND classid = (($1::bigint >> 32) & 4294967295)::oid AND objid = ($1::bigint & 4294967295)::oid`

// outsideConn is a connection to pool's database that is not one of the pool's: the holder and the poller must not
// draw on the connections the waiting writers need.
func outsideConn(t *testing.T, pool *pgxpool.Pool) *pgx.Conn {
	t.Helper()
	conn, err := pgx.ConnectConfig(context.Background(), pool.Config().ConnConfig.Copy())
	if err != nil {
		t.Fatalf("connect outside the pool: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	return conn
}

// holdChainLockOn takes the chain lock on conn in an open transaction and returns the release.
func holdChainLockOn(t *testing.T, conn *pgx.Conn) func() {
	t.Helper()
	ctx := context.Background()
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatalf("begin holder tx: %v", err)
	}
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, db.AuditChainLockKey); err != nil {
		t.Fatalf("hold the chain lock: %v", err)
	}
	return func() { _ = tx.Rollback(context.Background()) }
}

func isLockRefusal(err error) bool {
	var pgErr *pgconn.PgError
	return errors.Is(err, store.ErrAuditAppendBusy) || (errors.As(err, &pgErr) && pgErr.Code == "55P03")
}

// While another session holds the chain lock, no more than the slot count of the pool's writers wait on it, and the
// rest are refused without holding a connection. A poll that never sees the waiters, or sees too many, fails.
func TestPG_AuditAppendSlotsBoundTheWaitersOnTheChainLock(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	slots := store.AuditAppendSlotCount(pool)
	holder, poller := outsideConn(t, pool), outsideConn(t, pool)
	release := holdChainLockOn(t, holder)
	defer release()

	const writers = 10
	errs := make(chan error, writers)
	var wg sync.WaitGroup
	for range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ev := auditProbeEvent()
			errs <- store.InsertAuditEvent(context.Background(), pool, &ev)
		}()
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()

	ctx := context.Background()
	var peak int
	reached := time.Now().Add(3 * time.Second)
poll:
	for {
		var n int
		if err := poller.QueryRow(ctx, slotWaitersSQL, db.AuditChainLockKey).Scan(&n); err != nil {
			t.Fatalf("poll the waiters: %v", err)
		}
		if n > slots {
			t.Fatalf("%d writers waited on the chain lock at once, with %d slots", n, slots)
		}
		peak = max(peak, n)
		select {
		case <-done:
			break poll
		case <-time.After(20 * time.Millisecond):
		}
		if peak < slots && time.Now().After(reached) {
			t.Fatalf("only %d waiters were ever seen; want %d within 3s", peak, slots)
		}
	}
	if peak < slots {
		t.Fatalf("only %d waiters were ever seen; want %d", peak, slots)
	}
	close(errs)
	for err := range errs {
		if err == nil {
			t.Error("a write succeeded while the holder held the chain lock")
		} else if !isLockRefusal(err) {
			t.Errorf("error %v is neither a 55P03 nor ErrAuditAppendBusy", err)
		}
	}
}

// Every call returns within the chain lock's bound, however many writers there are, and a writer that found every
// slot taken says so.
func TestPG_AuditAppendBusyStaysInsideTheLockBound(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	restore := db.AuditChainLockTimeout
	db.AuditChainLockTimeout = 600 * time.Millisecond
	t.Cleanup(func() { db.AuditChainLockTimeout = restore })
	release := holdChainLockOn(t, outsideConn(t, pool))
	defer release()

	const writers = 10
	type outcome struct {
		err  error
		took time.Duration
	}
	out := make(chan outcome, writers)
	for range writers {
		go func() {
			ev := auditProbeEvent()
			start := time.Now()
			err := store.InsertAuditEvent(context.Background(), pool, &ev)
			out <- outcome{err, time.Since(start)}
		}()
	}
	var busy int
	for range writers {
		o := <-out
		if o.took > db.AuditChainLockTimeout+500*time.Millisecond {
			t.Errorf("a call took %s, over the %s bound plus 500ms", o.took, db.AuditChainLockTimeout)
		}
		if errors.Is(o.err, store.ErrAuditAppendBusy) {
			busy++
		}
	}
	if busy == 0 {
		t.Error("no call answered ErrAuditAppendBusy")
	}
}

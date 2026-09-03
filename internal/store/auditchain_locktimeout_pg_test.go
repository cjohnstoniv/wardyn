// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

// PIN for the review finding that the audit-chain advisory lock had no bound on
// the SYNCHRONOUS request path. Since migration 0056 the BEFORE INSERT trigger
// takes that lock on every insert into audit_events, so one transaction that
// inserted an audit row and stayed open holds up every audit write in the
// process — reachable with no Wardyn bug (an operator's psql session, a seed
// script, a paused migration tool). AuditSpool.Drain bounded its side at 15s a
// pass; store.InsertAuditEvent took the lock on the raw request context against
// a server with lock_timeout = 0 and statement_timeout = 0, and waited forever,
// pinning a request goroutine and a pool connection each time.

import (
	"context"
	"errors"
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

// lockProbeDB gives each test its OWN database: the probe deliberately leaves a
// transaction holding the global chain lock, which in a shared database would
// block every other audit-writing test in the run.
func lockProbeDB(t *testing.T) (*pgxpool.Pool, string) {
	t.Helper()
	pool := throwawayDatabase(t)
	if err := db.Migrate(context.Background(), pool); err != nil {
		t.Fatalf("migrate probe database: %v", err)
	}
	return pool, pool.Config().ConnString()
}

// holdChainLock opens its own connection, takes the chain lock in a transaction
// and leaves it open until the returned func is called — the documented
// reachable state, produced exactly as an operator's stray psql session would.
func holdChainLock(t *testing.T, dsn string) func() {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect holder: %v", err)
	}
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatalf("begin holder tx: %v", err)
	}
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, db.AuditChainLockKey); err != nil {
		t.Fatalf("hold chain lock: %v", err)
	}
	return func() {
		_ = tx.Rollback(context.Background())
		_ = conn.Close(context.Background())
	}
}

func TestPG_AuditWriteGivesUpOnAHeldChainLock(t *testing.T) {
	pool, dsn := lockProbeDB(t)

	// Shorten the production bound so the test measures the MECHANISM rather
	// than spending its wall clock on the real 5s. The bound under test is
	// db.AuditChainLockTimeout itself; that it is honoured is what matters here.
	restore := db.AuditChainLockTimeout
	db.AuditChainLockTimeout = 400 * time.Millisecond
	t.Cleanup(func() { db.AuditChainLockTimeout = restore })

	release := holdChainLock(t, dsn)
	defer release()

	ev := types.AuditEvent{
		ID: uuid.New(), Time: time.Now().UTC(), ActorType: types.ActorHuman,
		Actor: "locked@corp.example", Action: "test.chain.lock", Target: "t",
		Outcome: "success", SourceIP: "127.0.0.1",
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	start := time.Now()
	err := store.InsertAuditEvent(ctx, pool, &ev)
	waited := time.Since(start)

	if err == nil {
		t.Fatal("InsertAuditEvent SUCCEEDED while another transaction held the chain lock; the lock is not doing its job")
	}
	if waited > 5*time.Second {
		t.Fatalf("InsertAuditEvent waited %v on a held chain lock. Unbounded, it waits for as long as the holder "+
			"holds — pinning a request goroutine and a pool connection each time, and at the documented minimum "+
			"pool_max_conns of 3 a handful of these exhausts the pool and every other query blocks behind them", waited)
	}
	// The wait must be the LOCK timeout, not the caller's context giving up:
	// a context deadline would abort a slow-but-progressing insert too, and it
	// would not bind a writer that reaches the lock inside the trigger.
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "55P03" {
		t.Errorf("InsertAuditEvent failed with %v (sqlstate %q) after %v, want lock_not_available (55P03)",
			err, sqlStateOf(err), waited)
	}
	if ctx.Err() != nil {
		t.Error("the caller's context expired; the bound must come from the database, not from the request giving up")
	}
	t.Logf("held chain lock: InsertAuditEvent gave up after %v with %s", waited.Round(time.Millisecond), sqlStateOf(err))
}

// TestPG_AuditWriteResumesWhenTheChainLockClears is the other direction, and the
// one that decides whether the bound is a fix or a relocation of the failure: a
// bound that turned a slow audit write into a permanently failed one would have
// moved the problem. Once the holder commits, the very same write succeeds.
func TestPG_AuditWriteResumesWhenTheChainLockClears(t *testing.T) {
	pool, dsn := lockProbeDB(t)

	restore := db.AuditChainLockTimeout
	db.AuditChainLockTimeout = 400 * time.Millisecond
	t.Cleanup(func() { db.AuditChainLockTimeout = restore })

	release := holdChainLock(t, dsn)
	ctx := context.Background()
	ev := types.AuditEvent{
		ID: uuid.New(), Time: time.Now().UTC(), ActorType: types.ActorHuman,
		Actor: "retried@corp.example", Action: "test.chain.lock.retry", Target: "t",
		Outcome: "success", SourceIP: "127.0.0.1",
	}
	if err := store.InsertAuditEvent(ctx, pool, &ev); err == nil {
		release()
		t.Fatal("precondition: the write succeeded while the lock was held")
	}
	release() // the stray transaction ends, as it eventually does

	// This is what the spool's drain does with the event the request path could
	// not write: replay it. It must land, and it must be chained.
	if err := store.InsertAuditEvent(ctx, pool, &ev); err != nil {
		t.Fatalf("replaying the event after the chain lock cleared: %v — a bounded wait must defer the write, "+
			"not destroy it", err)
	}
	if ev.RowHash == "" {
		t.Error("the replayed event carries no row_hash; it was written outside the chain")
	}
}

// TestAuditChainLockTimeoutIsUnderTheDrainDeadline pins the ORDERING of the two
// bounds. The request path must yield before the background drain does: the
// drain is the thing built to absorb a backlog, so if it gave up first the
// events would queue on the request path instead of in the spool.
func TestAuditChainLockTimeoutIsUnderTheDrainDeadline(t *testing.T) {
	// api.spoolDrainDeadline is unexported and in another package; its value is
	// pinned here by name so a change to either side has to reckon with this.
	const drainPassDeadline = 15 * time.Second
	if db.AuditChainLockTimeout >= drainPassDeadline {
		t.Errorf("the request-path chain lock bound (%s) is not shorter than the drain pass deadline (%s); "+
			"the synchronous writer must give up first", db.AuditChainLockTimeout, drainPassDeadline)
	}
	if db.AuditChainLockTimeout < time.Second {
		t.Errorf("the request-path chain lock bound is %s; a legitimate wait is other audit writers queueing at "+
			"single-digit milliseconds each, and a sub-second bound starts spooling writes that would have "+
			"completed", db.AuditChainLockTimeout)
	}
}

func sqlStateOf(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return ""
}

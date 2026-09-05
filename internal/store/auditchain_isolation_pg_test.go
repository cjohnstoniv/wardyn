// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// PIN for the review finding that audit-chain LINK correctness rested on
// default_transaction_isolation — a USERSET GUC nothing in the tree pinned or
// checked — and that Wardyn's OWN writers forked the chain at REPEATABLE READ.
//
// Since 0056 the head read that decides prev_hash runs INSIDE the trigger, i.e.
// inside the inserting transaction. Under REPEATABLE READ the snapshot that read
// uses is taken by the advisory-lock statement BEFORE the lock is granted, so a
// writer that queued behind the lock reads a head from before the winner
// committed and chains to it: two rows claiming one predecessor, which
// store.VerifyAuditChain reports as a break.
//
// The probe runs in its OWN schema, so a fork it provokes on the unfixed tree can
// never reach the lane's audit_events (and so can never redden the chain-verify
// tests in this package).
package store_test

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/db"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// repeatableReadSchemaPool migrates a complete Wardyn schema into a throwaway
// namespace and returns a pool whose sessions default to REPEATABLE READ — the
// deployment posture under test, reachable with nothing but
// `ALTER ROLE ... SET default_transaction_isolation`.
func repeatableReadSchemaPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("WARDYN_TEST_PG")
	if dsn == "" {
		t.Skip("WARDYN_TEST_PG not set")
	}
	u, err := url.Parse(dsn)
	if err != nil || u.Scheme == "" || u.Host == "" {
		t.Skip("WARDYN_TEST_PG is not a URL-form DSN; cannot point a connection at another schema")
	}
	ctx := context.Background()

	base, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Skipf("connect: %v", err)
	}
	t.Cleanup(base.Close)
	if err := base.Ping(ctx); err != nil {
		t.Skipf("ping: %v", err)
	}
	schema := fmt.Sprintf("wardyn_iso_%d", time.Now().UnixNano()%1_000_000_000)
	if _, err := base.Exec(ctx, `CREATE SCHEMA `+schema); err != nil {
		t.Fatalf("create probe schema: %v", err)
	}
	t.Cleanup(func() {
		if _, err := base.Exec(context.Background(), `DROP SCHEMA IF EXISTS `+schema+` CASCADE`); err != nil {
			t.Logf("cleanup drop schema %s: %v", schema, err)
		}
	})

	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse DSN: %v", err)
	}
	// Set as startup parameters rather than by SET, so EVERY connection this
	// pool opens carries them — including the one the writer under test grabs.
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	cfg.ConnConfig.RuntimeParams["default_transaction_isolation"] = "repeatable read"
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("connect with search_path=%s: %v", schema, err)
	}
	t.Cleanup(pool.Close)

	var iso string
	if err := pool.QueryRow(ctx, `SELECT current_setting('default_transaction_isolation')`).Scan(&iso); err != nil {
		t.Fatalf("read default_transaction_isolation: %v", err)
	}
	if iso != "repeatable read" {
		t.Skipf("default_transaction_isolation is %q, not repeatable read; this server does not accept it as a startup parameter", iso)
	}
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatalf("Migrate into schema %s: %v", schema, err)
	}
	return pool
}

func TestPG_InsertAuditEventDoesNotForkTheChainAtRepeatableRead(t *testing.T) {
	pool := repeatableReadSchemaPool(t)
	ctx := context.Background()

	// The WINNER holds the chain lock in an open transaction. Everything the
	// loser does after this point happens with the lock already taken.
	blocker, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire blocker conn: %v", err)
	}
	defer blocker.Release()
	tx, err := blocker.Begin(ctx)
	if err != nil {
		t.Fatalf("begin blocker tx: %v", err)
	}
	defer tx.Rollback(context.Background()) //nolint:errcheck // committed below on the happy path
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, db.AuditChainLockKey); err != nil {
		t.Fatalf("blocker takes the chain lock: %v", err)
	}

	// The LOSER goes through the real writer. It takes its snapshot on the
	// advisory-lock statement and then parks until the blocker commits.
	loser := types.AuditEvent{
		ID: uuid.New(), Time: time.Now().UTC(), ActorType: types.ActorSystem,
		Actor: "iso-probe", Action: "test.isolation.loser", Outcome: "success",
	}
	done := make(chan error, 1)
	go func() { done <- store.InsertAuditEvent(ctx, pool, &loser) }()

	// Wait until the loser is genuinely BLOCKED on the advisory lock; a sleep
	// here would make the test a race rather than a proof.
	deadline := time.Now().Add(10 * time.Second)
	for {
		var waiting int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM pg_locks WHERE locktype = 'advisory' AND NOT granted`).Scan(&waiting); err != nil {
			t.Fatalf("poll pg_locks: %v", err)
		}
		if waiting > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the second writer never blocked on the chain lock; the probe cannot create the ordering it needs")
		}
		select {
		case err := <-done:
			t.Fatalf("the second writer finished without ever waiting for the lock (err = %v)", err)
		case <-time.After(20 * time.Millisecond):
		}
	}

	// The winner appends and commits WHILE the loser waits.
	var winnerHash string
	if err := tx.QueryRow(ctx, `INSERT INTO audit_events (id, actor_type, actor, action, outcome)
		VALUES (gen_random_uuid(), 'system', 'iso-probe', 'test.isolation.winner', 'success')
		RETURNING row_hash`).Scan(&winnerHash); err != nil {
		t.Fatalf("winner append: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("winner commit: %v", err)
	}

	if err := <-done; err != nil {
		t.Fatalf("InsertAuditEvent (the writer that queued behind the lock): %v", err)
	}

	if loser.PrevHash != winnerHash {
		t.Fatalf("the audit chain FORKED: the second writer's prev_hash is %q, but the row committed before it hashes to %q.\n"+
			"Both rows claim one predecessor, which store.VerifyAuditChain reports as a break. The head read that decides "+
			"prev_hash happens inside the trigger, i.e. inside this transaction, so at REPEATABLE READ it uses the snapshot "+
			"taken by the advisory-lock statement BEFORE the lock was granted. Correctness of the log's link structure must "+
			"not rest on default_transaction_isolation, a GUC any role can set: pin pgx.ReadCommitted on the Begin.",
			loser.PrevHash, winnerHash)
	}
	if loser.RowHash == "" {
		t.Error("the second writer's row has no row_hash")
	}

	// And the sweep agrees, which is the property the fork actually breaks.
	st, err := store.NewPG(pool).VerifyAuditChain(ctx)
	if err != nil {
		t.Fatalf("VerifyAuditChain: %v", err)
	}
	if !st.OK {
		t.Errorf("VerifyAuditChain reports the probe chain broken at seq %d: %s", st.BrokenSeq, st.Reason)
	}
}

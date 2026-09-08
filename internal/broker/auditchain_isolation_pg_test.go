// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// PIN for HANDOFF-1 (the F235/F201 class, handed to this lane by P1's storedb
// lane): the broker's mint transaction wrote its credential.mint row through
// insertAuditEventTx on a transaction started with a BARE Begin, so the audit
// hash chain's LINK correctness rested on default_transaction_isolation — a
// USERSET GUC any role can flip with `ALTER ROLE ... SET`.
//
// Since migration 0056 the head read that decides prev_hash runs INSIDE the
// trigger, i.e. inside the inserting transaction. Under REPEATABLE READ the
// mint tx's snapshot is taken at its FIRST statement (long before it queues on
// the chain's advisory lock), so a mint that waited for the lock still chains to
// the head it saw before the winner committed: two rows claiming one
// predecessor, which store.VerifyAuditChain reports as a break.
//
// The probe runs in its OWN schema, so a fork it provokes on the unfixed tree can
// never reach the lane's audit_events.
package broker

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
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// repeatableReadBrokerPool migrates a complete Wardyn schema into a throwaway
// namespace and returns a pool whose sessions default to REPEATABLE READ — the
// deployment posture under test. Mirrors store's repeatableReadSchemaPool; the
// two packages cannot share a test helper.
func repeatableReadBrokerPool(t *testing.T) *pgxpool.Pool {
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
	schema := fmt.Sprintf("wardyn_brk_iso_%d", time.Now().UnixNano()%1_000_000_000)
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
	// Startup parameters rather than SET, so EVERY connection this pool opens
	// carries them — including the one the mint under test grabs.
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

// TestPG_MintDoesNotForkTheAuditChainAtRepeatableRead runs a REAL mint through
// PgxStore while another writer holds the audit chain lock, and asserts the mint
// row chains to the row that committed while it waited.
func TestPG_MintDoesNotForkTheAuditChainAtRepeatableRead(t *testing.T) {
	pool := repeatableReadBrokerPool(t)
	ctx := context.Background()

	runID := uuid.New()
	seedRun(ctx, t, pool, runID)
	scope := pgGithubScope(t)
	spec := types.GrantSpec{Kind: types.GrantGitHubToken, Scope: scope, RequiresApproval: true, TTLSeconds: 600}
	grantID := pgSeedGrant(ctx, t, pool, runID, spec)
	pgSeedApproval(ctx, t, pool, runID, grantID, scope)

	// The WINNER holds the chain lock in an open transaction. Everything the
	// mint does after this point happens with the lock already taken.
	blocker, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire blocker conn: %v", err)
	}
	defer blocker.Release()
	btx, err := blocker.Begin(ctx)
	if err != nil {
		t.Fatalf("begin blocker tx: %v", err)
	}
	defer btx.Rollback(context.Background()) //nolint:errcheck // committed below on the happy path
	if _, err := btx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, db.AuditChainLockKey); err != nil {
		t.Fatalf("blocker takes the chain lock: %v", err)
	}

	// The LOSER is the real mint. Its in-tx credential.mint insert parks on the
	// advisory lock until the blocker commits.
	b := New(NewPgxStore(pool), nil, &fakeAudit{}, nil, &FakeGitHubMinter{Token: "ghs_iso"})
	type mintResult struct {
		m   Minted
		err error
	}
	done := make(chan mintResult, 1)
	go func() {
		m, err := b.MintForGrant(ctx, callerFor(runID), grantID)
		done <- mintResult{m: m, err: err}
	}()

	// Wait until the mint is genuinely BLOCKED on the chain lock; a sleep here
	// would make this a race rather than a proof.
	deadline := time.Now().Add(10 * time.Second)
	for {
		var waiting int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM pg_locks
			  WHERE locktype = 'advisory' AND NOT granted
			    AND database = (SELECT oid FROM pg_database WHERE datname = current_database())`).Scan(&waiting); err != nil {
			t.Fatalf("poll pg_locks: %v", err)
		}
		if waiting > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the mint never blocked on the chain lock; the probe cannot create the ordering it needs")
		}
		select {
		case r := <-done:
			t.Fatalf("the mint finished without ever waiting for the lock (err = %v)", r.err)
		case <-time.After(20 * time.Millisecond):
		}
	}

	// The winner appends and commits WHILE the mint waits.
	var winnerHash string
	if err := btx.QueryRow(ctx, `INSERT INTO audit_events (id, actor_type, actor, action, outcome)
		VALUES (gen_random_uuid(), 'system', 'brk-iso-probe', 'test.isolation.winner', 'success')
		RETURNING row_hash`).Scan(&winnerHash); err != nil {
		t.Fatalf("winner append: %v", err)
	}
	if err := btx.Commit(ctx); err != nil {
		t.Fatalf("winner commit: %v", err)
	}

	res := <-done
	if res.err != nil {
		t.Fatalf("MintForGrant (the writer that queued behind the chain lock): %v", res.err)
	}

	var prevHash, rowHash string
	if err := pool.QueryRow(ctx,
		`SELECT COALESCE(prev_hash, ''), row_hash FROM audit_events
		  WHERE action = 'credential.mint' AND outcome = 'success' AND data->>'jti' = $1`,
		res.m.JTI).Scan(&prevHash, &rowHash); err != nil {
		t.Fatalf("read back the mint's audit row: %v", err)
	}
	if prevHash != winnerHash {
		// "" means the mint chained as a GENESIS row: its snapshot saw an empty
		// chain, so the probe's two rows are two heads — the same fork, at its
		// starkest.
		t.Fatalf("the audit chain FORKED: the mint's prev_hash is %q, but the row committed before it hashes to %q.\n"+
			"Both rows claim one predecessor, which store.VerifyAuditChain reports as a break. insertAuditEventTx's head "+
			"read happens inside the mint transaction, so at REPEATABLE READ it uses the snapshot the mint tx took at its "+
			"FIRST statement — before it queued for the chain lock. Correctness of the log's link structure must not rest "+
			"on default_transaction_isolation, a GUC any role can set: start the mint tx with TxBeginner.BeginReadCommitted.",
			prevHash, winnerHash)
	}
	if rowHash == "" {
		t.Error("the mint's audit row has no row_hash")
	}
}

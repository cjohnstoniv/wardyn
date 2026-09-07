// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package broker

// PIN for the finding that the broker's mint transaction was the one in-tree
// audit-chain writer left unpinned.
//
// broker.mint does its work and then calls insertAuditEventTx on the SAME
// transaction, so the audit row is chained inside it — and since migration 0056
// the read that decides prev_hash happens inside the trigger, i.e. inside that
// transaction. Under REPEATABLE READ the snapshot is taken by the first
// data-reading statement, BEFORE the chain's advisory lock is granted, so a mint
// that queues behind another writer chains onto a head that writer has already
// superseded. Two rows then claim one predecessor, which store.VerifyAuditChain
// reports as "a row was deleted or reordered" — a permanent false tamper verdict
// over a log nobody touched.
//
// store.InsertAuditEvent and db.beginReadCommitted both pin READ COMMITTED and
// say so in comments; db.go's boot ERROR line tells the operator that "Wardyn's
// own audit writers pin READ COMMITTED per transaction and are unaffected". This
// makes that sentence true of the broker too.
//
// The probe migrates into its OWN schema, so the fork it manufactures cannot
// touch the lane's audit_events, and drives the REAL production adapter
// (NewPgxStore) against a pool whose default_transaction_isolation is
// 'repeatable read' — which is the only thing that differs from the control.

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

// brokerProbeSchema migrates a complete Wardyn schema into a throwaway
// namespace and returns two pools pointed at it: one with the server's own
// default isolation, and one whose sessions default to REPEATABLE READ.
func brokerProbeSchema(t *testing.T) (base, repeatableRead *pgxpool.Pool, schema string) {
	t.Helper()
	dsn := os.Getenv("WARDYN_TEST_PG")
	if dsn == "" {
		t.Skip("WARDYN_TEST_PG not set; skipping the broker audit-chain isolation test")
	}
	ctx := context.Background()
	u, err := url.Parse(dsn)
	if err != nil || u.Scheme == "" || u.Host == "" {
		t.Fatalf("WARDYN_TEST_PG is not a URL-form DSN (%q); this lane declared a server, so it must be reachable", dsn)
	}

	admin, err := db.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer admin.Close()
	schema = fmt.Sprintf("wardyn_bx_%d", time.Now().UnixNano()%1_000_000_000)
	if _, err := admin.Exec(ctx, `CREATE SCHEMA `+schema); err != nil {
		t.Fatalf("create probe schema: %v", err)
	}
	t.Cleanup(func() {
		cleanup, err := db.Connect(context.Background(), dsn)
		if err != nil {
			t.Logf("cleanup connect: %v", err)
			return
		}
		defer cleanup.Close()
		if _, err := cleanup.Exec(context.Background(), `DROP SCHEMA IF EXISTS `+schema+` CASCADE`); err != nil {
			t.Logf("cleanup drop schema %s: %v", schema, err)
		}
	})

	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	if base, err = db.Connect(ctx, u.String()); err != nil {
		t.Fatalf("connect with search_path=%s: %v", schema, err)
	}
	t.Cleanup(base.Close)
	if err := db.Migrate(ctx, base); err != nil {
		t.Fatalf("migrate into schema %s: %v", schema, err)
	}

	cfg, err := pgxpool.ParseConfig(u.String())
	if err != nil {
		t.Fatalf("parse config: %v", err)
	}
	// THE ONE THING THAT DIFFERS. It is a USERSET GUC, so this is exactly what
	// `ALTER ROLE app SET default_transaction_isolation = 'repeatable read'` on
	// a real deployment does to every connection wardynd opens.
	cfg.ConnConfig.RuntimeParams["default_transaction_isolation"] = "repeatable read"
	if repeatableRead, err = pgxpool.NewWithConfig(ctx, cfg); err != nil {
		t.Fatalf("open the repeatable-read pool: %v", err)
	}
	t.Cleanup(repeatableRead.Close)
	return base, repeatableRead, schema
}

func TestPG_BrokerMintDoesNotForkTheChainAtRepeatableRead(t *testing.T) {
	base, rr, schema := brokerProbeSchema(t)
	ctx := context.Background()

	// The probe schema must really be the one in play; otherwise the fork below
	// would be manufactured against the lane's own audit_events.
	var landed int
	if err := base.QueryRow(ctx,
		`SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		 WHERE n.nspname = $1 AND c.relname = 'audit_events'`, schema).Scan(&landed); err != nil {
		t.Fatalf("locate audit_events: %v", err)
	}
	if landed != 1 {
		t.Fatalf("audit_events did not land in schema %s (found %d)", schema, landed)
	}

	// Confirm the pool really defaults to repeatable read, so a green result
	// cannot come from the setting silently not applying.
	var level string
	if err := rr.QueryRow(ctx, `SHOW default_transaction_isolation`).Scan(&level); err != nil {
		t.Fatalf("read default_transaction_isolation: %v", err)
	}
	if level != "repeatable read" {
		t.Fatalf("the probe pool's default_transaction_isolation is %q, want \"repeatable read\"; this test would pass vacuously", level)
	}

	// A BLOCKER holds the chain lock, so the broker's transaction opens, takes
	// its snapshot, and then parks — which is the production shape: two writers,
	// one chain lock.
	blocker, err := base.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire blocker conn: %v", err)
	}
	defer blocker.Release()
	btx, err := blocker.Begin(ctx)
	if err != nil {
		t.Fatalf("begin blocker tx: %v", err)
	}
	defer btx.Rollback(context.Background()) //nolint:errcheck
	if _, err := btx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, db.AuditChainLockKey); err != nil {
		t.Fatalf("blocker takes the chain lock: %v", err)
	}

	// THE PRODUCTION ADAPTER, not a copy of it.
	st := NewPgxStore(rr)
	mintErr := make(chan error, 1)
	go func() {
		tx, err := st.Begin(ctx)
		if err != nil {
			mintErr <- fmt.Errorf("begin: %w", err)
			return
		}
		defer tx.Rollback(context.Background()) //nolint:errcheck
		// The mint does its own reads before it audits; this is the statement
		// that fixes a REPEATABLE READ snapshot, and it is why the level matters.
		var n int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM audit_events`).Scan(&n); err != nil {
			mintErr <- fmt.Errorf("mint's own read: %w", err)
			return
		}
		ev := types.AuditEvent{
			ID: uuid.New(), Time: time.Now().UTC(), ActorType: types.ActorSystem,
			Actor: "broker-isolation-probe", Action: "test.f331.mint", Outcome: "success",
		}
		if err := insertAuditEventTx(ctx, tx, ev); err != nil {
			mintErr <- fmt.Errorf("insertAuditEventTx: %w", err)
			return
		}
		mintErr <- tx.Commit(ctx)
	}()

	// Wait until the broker's transaction is genuinely parked on the lock. If it
	// never parks, the race the test needs did not happen and the result would
	// be meaningless.
	deadline := time.Now().Add(10 * time.Second)
	for {
		var waiting int
		if err := base.QueryRow(ctx,
			`SELECT count(*) FROM pg_locks WHERE locktype = 'advisory' AND objid = $1::bigint & 2147483647 AND NOT granted`,
			db.AuditChainLockKey).Scan(&waiting); err != nil {
			t.Fatalf("poll pg_locks: %v", err)
		}
		if waiting > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the broker's transaction never parked on the chain lock; the probe did not reproduce the race")
		}
		time.Sleep(20 * time.Millisecond)
	}

	// The WINNER commits while the broker waits. Its row_hash is the head the
	// broker's row must chain onto.
	var winner string
	if err := btx.QueryRow(ctx, `INSERT INTO audit_events (id, actor_type, actor, action, outcome)
		VALUES (gen_random_uuid(), 'system', 'blocker', 'test.f331.winner', 'success')
		RETURNING COALESCE(row_hash, '')`).Scan(&winner); err != nil {
		t.Fatalf("blocker appends the winning row: %v", err)
	}
	if winner == "" {
		t.Fatal("the winning row has no row_hash; the chain trigger is not armed in the probe schema")
	}
	if err := btx.Commit(ctx); err != nil {
		t.Fatalf("blocker commit: %v", err)
	}

	if err := <-mintErr; err != nil {
		t.Fatalf("the broker's mint transaction: %v", err)
	}

	var prev string
	if err := base.QueryRow(ctx,
		`SELECT COALESCE(prev_hash, '') FROM audit_events WHERE action = 'test.f331.mint'`).Scan(&prev); err != nil {
		t.Fatalf("read the broker row: %v", err)
	}
	if prev != winner {
		t.Fatalf("the broker's audit row chained onto %q, not onto the committed head %q.\n"+
			"The mint transaction inherited default_transaction_isolation, so its snapshot was taken before the chain "+
			"lock was granted and the trigger's head read answered from it. Two rows now claim one predecessor, which "+
			"store.VerifyAuditChain reports as \"a row was deleted or reordered\" — a permanent false tamper verdict.",
			prev, winner)
	}
}

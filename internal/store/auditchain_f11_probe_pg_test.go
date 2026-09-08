// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// F11 PROBE — destination: internal/store/auditchain_f11_probe_pg_test.go
//
// Live probes for the audit hash chain (migration 0047) and the append-only
// claim behind it. Guarded by WARDYN_TEST_PG like every *_pg_test.go here, and
// reusing this package's helpers (runsPGPool, appendChained, auditSeq).
//
// The tamper steps need a role that can bypass the 0001/0004 triggers — the
// exact residual 0007 documents (table owner / superuser). CI's pg lane and
// the compose/kind stacks both connect as the container superuser, so the
// bypass is `SET LOCAL session_replication_role = replica`; a role that cannot
// do that SKIPS the tamper probes instead of failing them.
//
// Every probe restores the table before it returns: audit_events is shared by
// every test in the package and "a break is permanent" (docs/OPERATIONS.md).
//
// ALL FOUR ARE GREEN PINS NOW. Two were red on feat/v0.7-profiles @ fa910735,
// which is what they were written to prove; both fixes have landed and the
// assertions are unchanged, so a red here is a REGRESSION, not a finding:
//
//	TestPG_ProbeF11_RewrittenRowReportsExactSeq        pins the tamper claim
//	TestPG_ProbeF11_SplicedOutRowReportsSuccessorSeq   pins the splice claim
//	TestPG_ProbeF11_UnchainedRowAfterGenesisIsNotClean was H1 (unchained post-genesis rows verified clean); auditChainWalk rule 3
//	TestPG_ProbeF11_UnlockedWriterDoesNotForkChain     was H5 (a lock-skipping writer forked the chain); migration 0056 + 0057
package store_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/db"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

var errNoTriggerBypass = errors.New("this role cannot disable triggers (needs superuser)")

// withTriggersOff runs fn in one transaction with every ordinary trigger on
// audit_events disabled (session_replication_role = replica) — the DB-admin
// bypass the chain exists to make visible. Returns errNoTriggerBypass when the
// role is not allowed to, so callers can skip rather than fail.
func withTriggersOff(ctx context.Context, pool *pgxpool.Pool, fn func(tx pgx.Tx) error) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // best-effort on the failure path
	if _, err := tx.Exec(ctx, `SET LOCAL session_replication_role = replica`); err != nil {
		return errNoTriggerBypass
	}
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// triggersOff is withTriggersOff for the test body: skip on no-bypass, fatal
// on anything else.
func triggersOff(t *testing.T, pool *pgxpool.Pool, fn func(tx pgx.Tx) error) {
	t.Helper()
	err := withTriggersOff(context.Background(), pool, fn)
	switch {
	case errors.Is(err, errNoTriggerBypass):
		storeSkipOrFatal(t, pool, "skipping tamper probe: %v", err)
	case err != nil:
		t.Fatalf("tamper step: %v", err)
	}
}

// requireTriggerBypass skips up front when the cleanup would not be able to
// undo what the probe does — leaving the shared table broken for every later
// sweep in the run is worse than skipping.
func requireTriggerBypass(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	var super bool
	if err := pool.QueryRow(context.Background(),
		`SELECT rolsuper FROM pg_roles WHERE rolname = current_user`).Scan(&super); err != nil {
		t.Fatalf("read role: %v", err)
	}
	if !super {
		storeSkipOrFatal(t, pool, "WARDYN_TEST_PG role is not a superuser, so the probe could not restore the shared audit_events table afterwards")
	}
}

// sweep runs the real sweep and insists it RAN: a tampered log must come
// back as a finding (OK=false), never as an error — that is the 200-vs-5xx
// promise handleVerifyAuditChain builds on.
func sweep(t *testing.T, pool *pgxpool.Pool) store.AuditChainStatus {
	t.Helper()
	st, err := store.NewPG(pool).VerifyAuditChain(context.Background())
	if err != nil {
		t.Fatalf("VerifyAuditChain returned an ERROR — a tampered log must be a finding (ok=false), not a 5xx: %v", err)
	}
	return st
}

// TestPG_ProbeF11_RewrittenRowReportsExactSeq edits the two fields the existing
// tamper test does not touch — the jsonb payload and the timestamp — and
// demands the sweep name the edited row's seq. It first proves the negative:
// a jsonb-EQUIVALENT rewrite (same value, different key order/whitespace) is
// not an edit, because the hash covers the stored jsonb, not the caller's
// bytes (0047's "data -> embedded as jsonb").
func TestPG_ProbeF11_RewrittenRowReportsExactSeq(t *testing.T) {
	pool := runsPGPool(t)
	requireTriggerBypass(t, pool)
	ctx := context.Background()

	appendChained(t, pool, "f11-edit-before")
	victim := appendChained(t, pool, "f11-edit-victim")
	appendChained(t, pool, "f11-edit-after")
	seq := auditSeq(t, pool, victim.ID)

	restore := func() error {
		return withTriggersOff(ctx, pool, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx,
				`UPDATE audit_events SET data = $1::jsonb, "time" = $2 WHERE seq = $3`,
				string(victim.Data), victim.Time, seq)
			return err
		})
	}
	t.Cleanup(func() {
		if err := restore(); err != nil {
			t.Errorf("restore victim row: %v", err)
		}
	})

	t.Run("jsonb-equivalent rewrite is not an edit", func(t *testing.T) {
		triggersOff(t, pool, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `UPDATE audit_events SET data = $1::jsonb WHERE seq = $2`,
				`{ "m":"x", "a":{"n":2}, "z":1 }`, seq)
			return err
		})
		if st := sweep(t, pool); !st.OK {
			t.Fatalf("a jsonb-equivalent rewrite reported tampering at seq=%d: %s", st.BrokenSeq, st.Reason)
		}
	})

	t.Run("one changed payload value breaks at exactly that seq", func(t *testing.T) {
		triggersOff(t, pool, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `UPDATE audit_events SET data = jsonb_set(data, '{z}', '2') WHERE seq = $1`, seq)
			return err
		})
		st := sweep(t, pool)
		if st.OK {
			t.Fatal("an edited data payload verified CLEAN")
		}
		if st.BrokenSeq != seq {
			t.Errorf("BrokenSeq = %d, want the edited row's seq %d", st.BrokenSeq, seq)
		}
		if !strings.Contains(st.Reason, "edited") {
			t.Errorf("Reason = %q, want the row-edited reason", st.Reason)
		}
		if err := restore(); err != nil {
			t.Fatalf("restore: %v", err)
		}
		if st := sweep(t, pool); !st.OK {
			t.Fatalf("chain still broken after restoring data: seq=%d %s", st.BrokenSeq, st.Reason)
		}
	})

	t.Run("one microsecond on the timestamp breaks at exactly that seq", func(t *testing.T) {
		triggersOff(t, pool, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `UPDATE audit_events SET "time" = "time" + interval '1 microsecond' WHERE seq = $1`, seq)
			return err
		})
		st := sweep(t, pool)
		if st.OK {
			t.Fatal("a row whose timestamp moved by 1µs verified CLEAN (time is not covered by the hash, or is hashed at coarser precision than it is stored)")
		}
		if st.BrokenSeq != seq {
			t.Errorf("BrokenSeq = %d, want %d", st.BrokenSeq, seq)
		}
		if err := restore(); err != nil {
			t.Fatalf("restore: %v", err)
		}
		if st := sweep(t, pool); !st.OK {
			t.Fatalf("chain still broken after restoring time: seq=%d %s", st.BrokenSeq, st.Reason)
		}
	})
}

// TestPG_ProbeF11_SplicedOutRowReportsSuccessorSeq deletes a MIDDLE row with
// the triggers off and demands rule 2 (auditchain.go step): the break is named
// at the SUCCESSOR's seq with the deleted/reordered reason, and it is a finding
// rather than an error. The row is parked in a temp table and put back
// byte-for-byte (OVERRIDING SYSTEM VALUE, triggers off so 0047 does not
// re-chain it) so the shared table is whole again afterwards.
func TestPG_ProbeF11_SplicedOutRowReportsSuccessorSeq(t *testing.T) {
	pool := runsPGPool(t)
	requireTriggerBypass(t, pool)
	ctx := context.Background()

	appendChained(t, pool, "f11-splice-before")
	victim := appendChained(t, pool, "f11-splice-victim")
	after := appendChained(t, pool, "f11-splice-after")
	vSeq := auditSeq(t, pool, victim.ID)
	aSeq := auditSeq(t, pool, after.ID)

	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	t.Cleanup(conn.Release) // registered FIRST so it runs LAST (cleanups are LIFO)
	// No bind parameter: CREATE TABLE AS is a utility statement, and seq is an
	// int64 we just read back, so formatting it in is safe.
	if _, err := conn.Exec(ctx, fmt.Sprintf(`CREATE TEMP TABLE f11_victim AS SELECT * FROM audit_events WHERE seq = %d`, vSeq)); err != nil {
		t.Fatalf("park victim: %v", err)
	}
	t.Cleanup(func() { conn.Exec(ctx, `DROP TABLE IF EXISTS f11_victim`) }) //nolint:errcheck

	onConn := func(fn func(tx pgx.Tx) error) error {
		tx, err := conn.Begin(ctx)
		if err != nil {
			return err
		}
		defer tx.Rollback(ctx) //nolint:errcheck
		if _, err := tx.Exec(ctx, `SET LOCAL session_replication_role = replica`); err != nil {
			return errNoTriggerBypass
		}
		if err := fn(tx); err != nil {
			return err
		}
		return tx.Commit(ctx)
	}
	restore := func() error {
		return onConn(func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `INSERT INTO audit_events OVERRIDING SYSTEM VALUE
				SELECT * FROM f11_victim v WHERE NOT EXISTS (SELECT 1 FROM audit_events a WHERE a.seq = v.seq)`)
			return err
		})
	}
	t.Cleanup(func() {
		if err := restore(); err != nil {
			t.Errorf("restore spliced row: %v", err)
		}
	})

	err = onConn(func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `DELETE FROM audit_events WHERE seq = $1`, vSeq)
		return err
	})
	if errors.Is(err, errNoTriggerBypass) {
		storeSkipOrFatal(t, pool, "%v", err)
	}
	if err != nil {
		t.Fatalf("splice: %v", err)
	}

	st := sweep(t, pool)
	if st.OK {
		t.Fatal("a spliced-out middle row verified CLEAN; rule 2 (prev_hash linkage) is not enforced")
	}
	if st.BrokenSeq != aSeq {
		t.Errorf("BrokenSeq = %d, want the SUCCESSOR's seq %d (the deleted row %d cannot be named — it is gone)", st.BrokenSeq, aSeq, vSeq)
	}
	if !strings.Contains(st.Reason, "deleted or reordered") {
		t.Errorf("Reason = %q, want the deleted/reordered reason", st.Reason)
	}

	if err := restore(); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if again := sweep(t, pool); !again.OK {
		t.Fatalf("chain still broken after putting the row back verbatim: seq=%d %s", again.BrokenSeq, again.Reason)
	}
}

// TestPG_ProbeF11_UnchainedRowAfterGenesisIsNotClean — hypothesis H1.
//
// A DB admin who disables/drops the 0047 trigger (or inserts in replica mode)
// appends a row with NULL prev_hash/row_hash AFTER the chain's genesis. The
// sweep treats every NULL-hash row as "legacy" with no positional constraint
// (auditchain.go: `WHERE row_hash IS NOT NULL` + the Legacy count), and the
// trigger's head lookup skips NULL rows, so the chain simply steps over the
// forged row and verifies clean. The desired property asserted here — legacy
// rows are a PREFIX; a NULL-hash row with seq > first_seq is a finding — does
// did not hold on the RC; rule 3 in auditChainWalk delivers it now, so this is
// a GREEN regression pin.
func TestPG_ProbeF11_UnchainedRowAfterGenesisIsNotClean(t *testing.T) {
	pool := runsPGPool(t)
	requireTriggerBypass(t, pool)
	ctx := context.Background()

	appendChained(t, pool, "f11-null-before")
	forged := uuid.New()
	triggersOff(t, pool, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO audit_events (id, actor_type, actor, action, outcome)
			VALUES ($1, 'system', 'f11-forged-unchained', 'test.chain.forged', 'success')`, forged)
		return err
	})
	t.Cleanup(func() {
		if err := withTriggersOff(ctx, pool, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `DELETE FROM audit_events WHERE id = $1`, forged)
			return err
		}); err != nil {
			t.Errorf("remove forged row: %v", err)
		}
	})
	appendChained(t, pool, "f11-null-after") // chains straight over the forged row
	fSeq := auditSeq(t, pool, forged)

	st := sweep(t, pool)
	if st.OK {
		t.Fatalf("REGRESSION (F11 H1): an UNCHAINED row at seq=%d, written after genesis (first_seq=%d, head_seq=%d), verified CLEAN — "+
			"it is bucketed as legacy=%d instead of reported. A dropped or disabled audit_events_chain trigger therefore makes "+
			"every later row invisible to the sweep while ok stays true.", fSeq, st.FirstSeq, st.HeadSeq, st.Legacy)
	}
	if st.BrokenSeq != fSeq {
		t.Errorf("BrokenSeq = %d, want the unchained row's seq %d", st.BrokenSeq, fSeq)
	}
}

// TestPG_ProbeF11_UnlockedWriterDoesNotForkChain — hypothesis H5.
//
// Any writer that is not one of the two in-tree ones (store.InsertAuditEvent,
// the broker's insertAuditEventTx) — psql, scripts/e2e-backend.sh's seed
// INSERT, the db package's own insertAuditEvent test helper, a future code path
// — used to read the same chain head as a concurrent locked writer, so both
// rows chained to it: a fork. The sweep then reported TAMPERING (rule 2) at the
// LOCKED writer's row although no row was ever altered, and per
// docs/OPERATIONS.md that verdict is permanent.
//
// The invariant is that an un-serialized writer cannot fork the chain — which
// is only deliverable by SERIALIZING it: the chain link and the seq must be
// allocated under one lock (migration 0056 moves both into the trigger). The
// probe therefore runs the locked writer CONCURRENTLY and lets the unlocked one
// commit while it waits, and asserts the strong form: the locked row chains
// ONTO the unlocked row and the sweep is clean.
//
// It cannot be written the other way round. Holding the unlocked writer's
// transaction open ACROSS a synchronous store.InsertAuditEvent call — the shape
// this probe had while it was red — self-deadlocks under any implementation
// that actually serializes: the locked writer waits for a transaction that only
// commits after it returns. Measured, not assumed: with the lock in the trigger
// that shape hangs in store.InsertAuditEvent until the go test timeout kills it.
func TestPG_ProbeF11_UnlockedWriterDoesNotForkChain(t *testing.T) {
	pool := runsPGPool(t)
	requireTriggerBypass(t, pool) // only for the cleanup delete
	ctx := context.Background()

	appendChained(t, pool, "f11-fork-head")

	// Writer R: direct INSERT, no caller-side pg_advisory_xact_lock, in a
	// transaction held open while the locked writer starts.
	rawTx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin raw tx: %v", err)
	}
	defer rawTx.Rollback(ctx) //nolint:errcheck
	rawID := uuid.New()
	if _, err := rawTx.Exec(ctx, `INSERT INTO audit_events (id, actor_type, actor, action, outcome)
		VALUES ($1, 'system', 'f11-unlocked-writer', 'test.chain.fork', 'success')`, rawID); err != nil {
		t.Fatalf("raw insert: %v", err)
	}

	// Writer L: the real path, started while R is still open. It must WAIT for
	// R instead of reading the same head.
	locked := types.AuditEvent{
		ID: uuid.New(), Time: time.Now().UTC(), ActorType: types.ActorSystem,
		Actor: "f11-locked-writer", Action: "test.chain.fork", Outcome: "success",
	}
	done := make(chan error, 1)
	go func() { done <- store.InsertAuditEvent(ctx, pool, &locked) }()

	// Wait for L to be BLOCKED on the chain lock rather than sleeping a guessed
	// interval. On an unserialized tree nothing ever blocks, so the wait falls
	// through after its deadline and the assertions below report the fork.
	blocked := waitForBlockedChainWriter(t, pool, 10*time.Second)

	t.Cleanup(func() {
		// Both rows are this test's own tail. They are removed together: with
		// the chain serialized the locked row chains ONTO the unlocked one, so
		// deleting only the unlocked row would leave a genuine break behind for
		// every later sweep in the package. Truncating the tail verifies clean
		// (auditchain_test.go pins that as a non-promise).
		if err := withTriggersOff(ctx, pool, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `DELETE FROM audit_events WHERE id = ANY($1)`,
				[]uuid.UUID{rawID, locked.ID})
			return err
		}); err != nil {
			t.Errorf("remove probe rows: %v", err)
		}
	})

	if err := rawTx.Commit(ctx); err != nil {
		t.Fatalf("commit raw tx: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatalf("locked insert: %v", err)
	}
	if !blocked {
		t.Errorf("the locked writer never waited for the unlocked one: audit_events inserts are not serialized in the database, "+
			"so any writer that skips %#x forks the chain", db.AuditChainLockKey)
	}

	rSeq := auditSeq(t, pool, rawID)
	lSeq := auditSeq(t, pool, locked.ID)
	if rSeq >= lSeq {
		t.Errorf("seq order = unlocked %d, locked %d; the row that inserted FIRST must hold the lower seq", rSeq, lSeq)
	}
	var rHash string
	if err := pool.QueryRow(ctx, `SELECT row_hash FROM audit_events WHERE id = $1`, rawID).Scan(&rHash); err != nil {
		t.Fatalf("read unlocked row_hash: %v", err)
	}
	if locked.PrevHash != rHash {
		t.Errorf("locked row (seq=%d) chained to %q, want the unlocked row's hash %q (seq=%d) — both rows chained to the same head, "+
			"which is the fork that makes the sweep report a tamper that never happened", lSeq, locked.PrevHash, rHash, rSeq)
	}
	if st := sweep(t, pool); !st.OK {
		t.Fatalf("F11 H5: an unlocked writer (seq=%d) overlapping the locked one (seq=%d) made the sweep report %q at seq=%d, "+
			"although no row was altered — and docs/OPERATIONS.md says a break is permanent", rSeq, lSeq, st.Reason, st.BrokenSeq)
	}
}

// waitForBlockedChainWriter polls pg_locks until some backend is WAITING on the
// audit-chain advisory lock, and reports whether that ever happened. The
// advisory key is split across (classid, objid) exactly as pg_locks exposes it:
// the high 32 bits and the low 32 bits of db.AuditChainLockKey.
func waitForBlockedChainWriter(t *testing.T, pool *pgxpool.Pool, within time.Duration) bool {
	t.Helper()
	ctx := context.Background()
	classid := uint32(uint64(db.AuditChainLockKey) >> 32)
	objid := uint32(uint64(db.AuditChainLockKey) & 0xFFFFFFFF)
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		var n int
		if err := pool.QueryRow(ctx, `
			SELECT count(*) FROM pg_locks
			 WHERE locktype = 'advisory' AND NOT granted
			   AND classid = $1 AND objid = $2`, classid, objid).Scan(&n); err != nil {
			t.Fatalf("poll pg_locks: %v", err)
		}
		if n > 0 {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

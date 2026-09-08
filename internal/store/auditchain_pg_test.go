// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Live chain tests for migration 0047. Guarded by WARDYN_TEST_PG, like every
// other *_pg_test.go in this package.
package store_test

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// appendChained inserts one audit event through the real path and returns it
// with the hashes Postgres computed. Data deliberately carries out-of-order
// keys: jsonb re-sorts them on the way in, so hashing the caller's bytes
// instead of the stored jsonb would make every row fail verification — this is
// the regression that catches it.
func appendChained(t *testing.T, pool *pgxpool.Pool, actor string) types.AuditEvent {
	t.Helper()
	ev := types.AuditEvent{
		ID:        uuid.New(),
		Time:      time.Now().UTC(),
		ActorType: types.ActorSystem,
		Actor:     actor,
		Action:    "test.chain",
		Outcome:   "success",
		Data:      json.RawMessage(`{"z":1,"a":{"n":2},"m":"x"}`),
	}
	if err := store.InsertAuditEvent(context.Background(), pool, &ev); err != nil {
		t.Fatalf("insert audit event: %v", err)
	}
	if ev.RowHash == "" {
		t.Fatal("InsertAuditEvent returned no row_hash; the 0047 trigger did not fire")
	}
	return ev
}

func auditSeq(t *testing.T, pool *pgxpool.Pool, id uuid.UUID) int64 {
	t.Helper()
	var seq int64
	if err := pool.QueryRow(context.Background(),
		`SELECT seq FROM audit_events WHERE id=$1`, id).Scan(&seq); err != nil {
		t.Fatalf("read seq: %v", err)
	}
	return seq
}

// TestPG_AuditChain_LinksAndVerifies proves the chain end to end against a real
// Postgres: consecutive rows link, the head hash the insert path hands back is
// the one the sweep reports, and the sweep re-hashes every row clean.
func TestPG_AuditChain_LinksAndVerifies(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()

	first := appendChained(t, pool, "chain-test-1")
	second := appendChained(t, pool, "chain-test-2")

	if second.PrevHash != first.RowHash {
		t.Errorf("second row prev_hash = %q, want the first row's row_hash %q",
			second.PrevHash, first.RowHash)
	}
	if first.RowHash == second.RowHash {
		t.Error("two different rows produced the same row_hash")
	}

	st, err := store.NewPG(pool).VerifyAuditChain(ctx)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if !st.OK {
		t.Fatalf("chain verify failed on an untampered log: seq=%d %s", st.BrokenSeq, st.Reason)
	}
	if st.HeadHash != second.RowHash {
		t.Errorf("HeadHash = %q, want the newest row's hash %q", st.HeadHash, second.RowHash)
	}
	if st.Checked < 2 {
		t.Errorf("Checked = %d, want at least the 2 rows this test wrote", st.Checked)
	}
}

// TestPG_AuditChain_SurvivesConcurrentWriters is the check behind the design
// note in db.AuditChainLockKey. Every audit writer races for one chain head, so
// a chain that is only correct single-threaded is not a chain. Without the
// caller-side pg_advisory_xact_lock, two writers can have their seq allocated
// in one order and read the head in the other, leaving seq order and chain
// order inverted — which this sweep reports as tampering.
func TestPG_AuditChain_SurvivesConcurrentWriters(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()

	const writers, each = 12, 6
	var wg sync.WaitGroup
	errs := make(chan error, writers*each)
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < each; i++ {
				ev := types.AuditEvent{
					ID:        uuid.New(),
					Time:      time.Now().UTC(),
					ActorType: types.ActorSystem,
					Actor:     fmt.Sprintf("concurrent-%d-%d", w, i),
					Action:    "test.chain.concurrent",
					Outcome:   "success",
				}
				if err := store.InsertAuditEvent(ctx, pool, &ev); err != nil {
					errs <- err
					return
				}
			}
		}(w)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent insert: %v", err)
	}

	st, err := store.NewPG(pool).VerifyAuditChain(ctx)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if !st.OK {
		t.Fatalf("%d concurrent writers broke the chain at seq=%d: %s "+
			"(the append lock is not serializing seq allocation against the head read)",
			writers, st.BrokenSeq, st.Reason)
	}
}

// TestPG_AuditChain_DetectsTamperedMiddleRow is the point of the feature: an
// actor who can bypass the append-only trigger (a table OWNER — exactly the
// residual 0007 documents) rewrites ONE row in the middle of the log, and the
// sweep names it.
//
// The rewrite is undone before the test returns: audit_events is append-only
// and shared by every other test in this package, so leaving it broken would
// fail every later sweep in the same run.
func TestPG_AuditChain_DetectsTamperedMiddleRow(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()

	appendChained(t, pool, "tamper-test-before")
	victim := appendChained(t, pool, "tamper-test-victim")
	appendChained(t, pool, "tamper-test-after")
	victimSeq := auditSeq(t, pool, victim.ID)

	// Play the DB admin: disable the append-only trigger, rewrite one field
	// that the row hash covers, re-enable. Only the 0001 UPDATE/DELETE guard is
	// touched — 0047's BEFORE INSERT chain trigger stays armed.
	setActor := func(actor string) {
		t.Helper()
		if _, err := pool.Exec(ctx,
			`ALTER TABLE audit_events DISABLE TRIGGER audit_events_no_update`); err != nil {
			// The last self-skip in this package the F11 skip floor cannot see:
			// this test's name does not match /^TestPG_ProbeF11_/, so a lane
			// that CAN own the table and silently stopped tampering would still
			// report `ok`. Routed through the same derived discipline.
			storeSkipOrFatal(t, pool, "cannot disable the append-only trigger as this role (%v); "+
				"the tamper case needs table ownership", err)
		}
		defer pool.Exec(ctx, `ALTER TABLE audit_events ENABLE TRIGGER audit_events_no_update`) //nolint:errcheck
		if _, err := pool.Exec(ctx,
			`UPDATE audit_events SET actor=$1 WHERE id=$2`, actor, victim.ID); err != nil {
			t.Fatalf("tamper: %v", err)
		}
	}
	setActor("rewritten-by-a-db-admin")
	t.Cleanup(func() { setActor(victim.Actor) })

	st, err := store.NewPG(pool).VerifyAuditChain(ctx)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if st.OK {
		t.Fatal("a rewritten audit row verified CLEAN; the chain is not tamper-evident")
	}
	if st.BrokenSeq != victimSeq {
		t.Errorf("BrokenSeq = %d, want the rewritten row's seq %d", st.BrokenSeq, victimSeq)
	}
	if st.Reason == "" {
		t.Error("a broken chain must name a reason")
	}

	// And the undo restores it: the chain is a function of the row's contents,
	// so putting the byte back makes it verify again. This also proves the
	// failure above came from the edit and not from something ambient.
	setActor(victim.Actor)
	again, err := store.NewPG(pool).VerifyAuditChain(ctx)
	if err != nil {
		t.Fatalf("verify after restore: %v", err)
	}
	if !again.OK {
		t.Fatalf("chain still broken after restoring the row: seq=%d %s", again.BrokenSeq, again.Reason)
	}
}

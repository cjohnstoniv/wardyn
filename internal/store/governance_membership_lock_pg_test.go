// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/testutil"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestPG_GovernanceMembership_OneSubjectTwoUnitsDoNotWait: a direct assignment write holds only its own unit and
// subject (#3478). While one transaction holds a direct write's locks for a subject in one unit, the same
// subject's write to another unit goes ahead; a write of the same unit and subject still waits for it.
func TestPG_GovernanceMembership_OneSubjectTwoUnitsDoNotWait(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	st := store.NewPG(pool)
	unitA := seedGovernanceUnit(t, st, "test-profile-"+uuid.NewString())
	unitB := seedGovernanceUnit(t, st, "test-profile-"+uuid.NewString())
	subject := "gov-lock-" + uuid.NewString()
	member := func(unit types.GovernanceUnit, priority int) types.GovernanceMembership {
		return types.GovernanceMembership{SubjectType: types.CapabilitySubjectGroup, Subject: subject, UnitID: unit.ID,
			Priority: priority, CreatedBy: "seed"}
	}
	var mu sync.Mutex
	var ids []uuid.UUID
	keep := func(m types.GovernanceMembership) {
		mu.Lock()
		defer mu.Unlock()
		ids = append(ids, m.ID)
	}
	// Registered after the profiles, so it runs before their cleanups (ON DELETE RESTRICT).
	t.Cleanup(func() {
		for _, id := range ids {
			_, _ = st.DeleteGovernanceMembership(ctx, id)
		}
	})
	// The hold is on a pool of its own: the direct write below takes a connection of pool while the hold is open
	// (WARDYN_TEST_POOL_GUARD).
	holdPool, err := pgxpool.New(ctx, pool.Config().ConnString())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(holdPool.Close)

	// The probe session is its own connection too, so asking who waits on whom never borrows from pool.
	watch := testutil.PGConn(t, pool)
	// blockedBy reports whether a session whose query matches pattern waits on holder.
	blockedBy := func(holder int32, pattern string) bool {
		t.Helper()
		var blocked bool
		if err := watch.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_stat_activity
			WHERE datname = current_database() AND $1 = ANY (pg_blocking_pids(pid)) AND query LIKE $2)`, holder, pattern).Scan(&blocked); err != nil {
			t.Fatal(err)
		}
		return blocked
	}
	// open holds a direct write's locks for held and writes it, in a transaction that stays open until release
	// commits it. holder is that transaction's backend pid.
	open := func(t *testing.T, held types.GovernanceMembership) (holder int32, release func()) {
		t.Helper()
		tx, err := holdPool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }) // a no-op after Commit
		if err := tx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&holder); err != nil {
			t.Fatal(err)
		}
		if err := store.LockGovernanceMembershipWrite(ctx, tx, held.UnitID, string(held.SubjectType), held.Subject); err != nil {
			t.Fatal(err)
		}
		first, err := store.UpsertGovernanceMembershipQ(ctx, tx, held)
		if err != nil {
			t.Fatal(err)
		}
		keep(first)
		return holder, func() {
			if err := tx.Commit(ctx); err != nil {
				t.Fatal(err)
			}
		}
	}
	// write starts st's direct write of m and closes the channel it returns when the write has returned.
	write := func(t *testing.T, m types.GovernanceMembership) <-chan struct{} {
		done := make(chan struct{})
		go func() {
			defer close(done)
			saved, err := st.UpsertGovernanceMembership(ctx, m)
			if err != nil {
				t.Errorf("direct write: %v", err)
				return
			}
			keep(saved)
		}()
		return done
	}
	// hung ends a wait that never resolves; it bounds a stuck test, not any timing the test asserts.
	hung := time.After(time.Minute)

	// Another unit: the write returns while the first transaction is still open. If it instead waits, a session
	// shows up blocked on the holder and the test fails; no clock decides either way.
	holder, release := open(t, member(unitA, 5))
	other := write(t, member(unitB, 5))
wait:
	for {
		select {
		case <-other:
			break wait // returned with the hold still open
		case <-hung:
			t.Fatal("the write to another unit neither returned nor waited on the open transaction")
		default:
			if blockedBy(holder, "%") {
				t.Error("the same subject's write to another unit waits for the open transaction, want it to go ahead")
				release()
				<-other
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	release()

	// The same unit and subject: the write waits on the holder's lock, and returns only after the hold commits.
	holder, release = open(t, member(unitA, 6))
	same := write(t, member(unitA, 7))
	for !blockedBy(holder, "%pg_advisory_xact_lock%") {
		select {
		case <-same:
			t.Fatal("a second write of the same unit and subject returned without waiting for the open transaction")
		case <-hung:
			t.Fatal("a second write of the same unit and subject never waited on the open transaction")
		case <-time.After(10 * time.Millisecond):
		}
	}
	select {
	case <-same:
		t.Error("a second write of the same unit and subject returned before the first committed")
	default:
	}
	release()
	<-same
}

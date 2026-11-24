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

	const hold = 500 * time.Millisecond
	// during holds a direct write's locks for held, writes it, and returns how long st's direct write of m took
	// while that transaction stayed open for hold.
	during := func(t *testing.T, held, m types.GovernanceMembership) time.Duration {
		t.Helper()
		tx, err := holdPool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(context.WithoutCancel(ctx)) //nolint:errcheck // a no-op after Commit
		if err := store.LockGovernanceMembershipWrite(ctx, tx, held.UnitID, string(held.SubjectType), held.Subject); err != nil {
			t.Fatal(err)
		}
		first, err := store.UpsertGovernanceMembershipQ(ctx, tx, held)
		if err != nil {
			t.Fatal(err)
		}
		keep(first)
		took := make(chan time.Duration, 1)
		go func() {
			begin := time.Now()
			saved, err := st.UpsertGovernanceMembership(ctx, m)
			if err != nil {
				t.Errorf("direct write: %v", err)
			} else {
				keep(saved)
			}
			took <- time.Since(begin)
		}()
		time.Sleep(hold)
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		return <-took
	}

	if got := during(t, member(unitA, 5), member(unitB, 5)); got >= 100*time.Millisecond {
		t.Errorf("the same subject's write to another unit took %s while the first was open, want under 100ms", got)
	}
	if got := during(t, member(unitA, 6), member(unitA, 7)); got < hold-100*time.Millisecond {
		t.Errorf("a second write of the same unit and subject took %s, want it to wait for the first (about %s)", got, hold)
	}
}

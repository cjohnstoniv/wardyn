// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// The counter fence on a member drive's delete (migration 0299), against a throwaway Postgres database. Guarded by
// WARDYN_TEST_PG; skipped cleanly when unset.

package store_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// fenceRow is a member drive and a handle on its two counters.
func fenceRow(t *testing.T) (*store.PG, *pgxpool.Pool, types.UserDrive) {
	t.Helper()
	pool := runsPGPoolIsolated(t)
	st := store.NewPG(pool)
	d, err := st.CreateMemberDrive(context.Background(), memberDrive("alice", "Notes"), 5)
	if err != nil {
		t.Fatal(err)
	}
	return &st, pool, d
}

// fenceState reads the drive's mark and count, and whether the store treats it as deleting.
func fenceState(t *testing.T, st *store.PG, id uuid.UUID) (started *time.Time, issues int, deleting bool) {
	t.Helper()
	d, err := st.GetMemberDrive(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return d.DeleteStartedAt, d.DeleteIssues, d.Deleting()
}

func markIsStale(t *testing.T, pool *pgxpool.Pool, id uuid.UUID) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`UPDATE user_drives SET delete_started_at = now() - interval '61 seconds' WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
}

// A issues, B takes over and issues, then A's late definitive refusal, and B's own, change nothing: the row stays
// deleting under B's pair (a claim over an odd count never clears, and an earlier holder's pair matches nothing).
func TestPG_DriveFence_StaleHolderCannotClear(t *testing.T) {
	st, pool, d := fenceRow(t)
	ctx := context.Background()
	_, sa, err := st.BeginMemberDriveDelete(ctx, "alice", d.ID)
	if err != nil {
		t.Fatal(err)
	}
	ia, err := st.IssueMemberDriveDelete(ctx, d.ID, sa, 0)
	if err != nil || ia != 1 {
		t.Fatalf("A's issue = %d, %v; want 1", ia, err)
	}
	markIsStale(t, pool, d.ID)
	b, sb, err := st.BeginMemberDriveDeleteAdmin(ctx, d.ID)
	if err != nil || b.DeleteIssues != 1 {
		t.Fatalf("takeover = count %d, %v; want the odd count kept", b.DeleteIssues, err)
	}
	ib, err := st.IssueMemberDriveDelete(ctx, d.ID, sb, b.DeleteIssues)
	if err != nil || ib != 3 {
		t.Fatalf("B's issue = %d, %v; want 3 (a claim over an odd count lands on the next odd count)", ib, err)
	}
	if err := st.ClearMemberDriveIssued(ctx, d.ID, sa, 0, ia); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("A's late clear = %v, want ErrNotFound", err)
	}
	if err := st.ClearMemberDriveIssued(ctx, d.ID, sb, b.DeleteIssues, ib); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("B's own clear over an odd count = %v, want ErrNotFound", err)
	}
	if err := st.ClearMemberDriveClaim(ctx, d.ID, sb, b.DeleteIssues); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("B's pre-flight clear over an odd count = %v, want ErrNotFound", err)
	}
	if err := st.FinishMemberDriveDelete(ctx, d.ID, sa, ia); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("A's late finish = %v, want ErrNotFound", err)
	}
	got, n, deleting := fenceState(t, st, d.ID)
	if got == nil || !got.Equal(sb) || n != 3 || !deleting {
		t.Errorf("after the late answers: mark %v, count %d, deleting %v; want B's mark, 3, true", got, n, deleting)
	}
}

// One claim makes one issue, however it is presented: twice in a row, or from many goroutines at once; and a second
// claim whose mark equals the first's (set by hand, as an equal clock would) still gets its own single issue.
func TestPG_DriveFence_OneIssuePerClaim(t *testing.T) {
	st, pool, d := fenceRow(t)
	ctx := context.Background()
	_, s1, err := st.BeginMemberDriveDelete(ctx, "alice", d.ID)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	won := 0
	for range 8 {
		wg.Go(func() {
			if _, err := st.IssueMemberDriveDelete(ctx, d.ID, s1, 0); err == nil {
				mu.Lock()
				won++
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	if won != 1 {
		t.Fatalf("%d concurrent issues of one pair succeeded, want exactly 1", won)
	}
	if _, err := st.IssueMemberDriveDelete(ctx, d.ID, s1, 0); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("the same pair presented again = %v, want ErrNotFound", err)
	}
	// A refusal that touched nothing clears the claim (count 2); the mark is then set back to the same instant.
	if err := st.ClearMemberDriveIssued(ctx, d.ID, s1, 0, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE user_drives SET delete_started_at = $2 WHERE id = $1`, d.ID, s1); err != nil {
		t.Fatal(err)
	}
	if i, err := st.IssueMemberDriveDelete(ctx, d.ID, s1, 2); err != nil || i != 3 {
		t.Errorf("the second claim's issue at an equal mark = %d, %v; want 3", i, err)
	}
	if _, err := st.IssueMemberDriveDelete(ctx, d.ID, s1, 0); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("the first claim's pair after the second = %v, want ErrNotFound", err)
	}
}

// An odd count with no mark reads as deleting, and a claim over it is accepted but cannot clear; the sweep's own
// claim takes only a mark older than a minute.
func TestPG_DriveFence_OddCountAndTheSweepClaim(t *testing.T) {
	st, pool, d := fenceRow(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `UPDATE user_drives SET delete_issues = 1 WHERE id = $1`, d.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, deleting := fenceState(t, st, d.ID); !deleting {
		t.Error("an odd count with no mark does not read as deleting")
	}
	if _, _, err := st.BeginMemberDriveTakeover(ctx, d.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("the sweep's claim of an unmarked row = %v, want ErrNotFound", err)
	}
	c, s, err := st.BeginMemberDriveDelete(ctx, "alice", d.ID)
	if err != nil || c.DeleteIssues != 1 {
		t.Fatalf("a claim over an odd count = count %d, %v; want it accepted at 1", c.DeleteIssues, err)
	}
	if err := st.ClearMemberDriveClaim(ctx, d.ID, s, 1); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("clearing a claim over an odd count = %v, want ErrNotFound", err)
	}
	if _, _, err := st.BeginMemberDriveTakeover(ctx, d.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("the sweep's claim of a fresh mark = %v, want ErrNotFound", err)
	}
	markIsStale(t, pool, d.ID)
	if _, _, err := st.BeginMemberDriveTakeover(ctx, d.ID); err != nil {
		t.Errorf("the sweep's claim of a mark older than a minute = %v, want it taken", err)
	}
}

// A claim nothing was issued under clears by its own pair and moves the count by two; a finish under the pair
// removes the row.
func TestPG_DriveFence_PreFlightClearAndFinish(t *testing.T) {
	st, _, d := fenceRow(t)
	ctx := context.Background()
	_, s, err := st.BeginMemberDriveDelete(ctx, "alice", d.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.ClearMemberDriveClaim(ctx, d.ID, s, 0); err != nil {
		t.Fatal(err)
	}
	if got, n, deleting := fenceState(t, st, d.ID); got != nil || n != 2 || deleting {
		t.Errorf("after the clear: mark %v, count %d, deleting %v; want none, 2, false", got, n, deleting)
	}
	if err := st.ClearMemberDriveClaim(ctx, d.ID, s, 0); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("the same clear twice = %v, want ErrNotFound", err)
	}
	_, s2, err := st.BeginMemberDriveDelete(ctx, "alice", d.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.FinishMemberDriveDelete(ctx, d.ID, s2, 0); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("a finish under the earlier count = %v, want ErrNotFound", err)
	}
	if err := st.FinishMemberDriveDelete(ctx, d.ID, s2, 2); err != nil {
		t.Errorf("a finish under the claim's own pair = %v", err)
	}
}

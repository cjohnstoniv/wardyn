// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package approval_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/approval"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestExpireStale_OnePoisonRowDoesNotStrandTheRest pins ExpireStale
// aborted the whole sweep on the first non-ErrAlreadyDecided error, and the
// sweeper re-lists in the SAME order every tick (ORDER BY requested_at DESC) —
// so one permanently failing PENDING row stranded every approval sorted after
// it, fleet-wide and forever. The sweep must expire what it can and report the
// failures it collected.
func TestExpireStale_OnePoisonRowDoesNotStrandTheRest(t *testing.T) {
	ctx := t.Context()
	stale := time.Now().UTC().Add(-2 * time.Hour)
	poison := errors.New("approval row is wedged")

	f := &fakeStore{decideErrOn: 1, decideErr: poison}
	for i := range 3 {
		if _, err := f.CreateApproval(ctx, types.ApprovalRequest{
			ID: uuid.New(), RunID: uuid.New(), Kind: types.ApprovalEgressDomain,
			State: types.ApprovalPending, RequestedAt: stale.Add(time.Duration(i) * time.Minute),
		}); err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
	}

	n, err := approval.ExpireStale(ctx, f, time.Hour)
	if !errors.Is(err, poison) {
		t.Fatalf("ExpireStale err = %v, want it to carry the poison row's error", err)
	}
	if n != 2 {
		t.Fatalf("expired = %d, want 2 — the two healthy rows are stranded behind the poison one, "+
			"and every tick re-lists in the same order so they never expire at all", n)
	}
}

// TestExpireStale_AlreadyDecidedStaysSilent is the negative control: a race
// with a concurrent human decision is NOT an error and must not surface in the
// joined error, or every sweep on a busy deployment would report a failure.
func TestExpireStale_AlreadyDecidedStaysSilent(t *testing.T) {
	ctx := t.Context()
	stale := time.Now().UTC().Add(-2 * time.Hour)

	f := &fakeStore{decideErrOn: 1, decideErr: approval.ErrAlreadyDecided}
	for i := range 2 {
		if _, err := f.CreateApproval(ctx, types.ApprovalRequest{
			ID: uuid.New(), RunID: uuid.New(), Kind: types.ApprovalEgressDomain,
			State: types.ApprovalPending, RequestedAt: stale.Add(time.Duration(i) * time.Minute),
		}); err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
	}

	n, err := approval.ExpireStale(ctx, f, time.Hour)
	if err != nil {
		t.Fatalf("ExpireStale err = %v, want nil — a concurrent decide is a race, not a failure", err)
	}
	if n != 1 {
		t.Fatalf("expired = %d, want 1", n)
	}
}

// TestExpireStale_RunWaitAndEndBindBeforeTheDeploymentCutoff pins run limits
// (#567): a request expires at min(requested_at + the run's wait, the run's
// end), which the store hands over as ExpiresAt. A request far younger than the
// deployment's cutoff whose run-level expiry has passed must expire; one whose
// ExpiresAt is still ahead, or unset, waits for the deployment cutoff.
func TestExpireStale_RunWaitAndEndBindBeforeTheDeploymentCutoff(t *testing.T) {
	ctx := t.Context()
	now := time.Now().UTC()
	past, future := now.Add(-time.Minute), now.Add(time.Hour)

	f := &fakeStore{}
	seed := func(expiresAt *time.Time) uuid.UUID {
		t.Helper()
		ap, err := f.CreateApproval(ctx, types.ApprovalRequest{
			ID: uuid.New(), RunID: uuid.New(), Kind: types.ApprovalToolCall,
			State: types.ApprovalPending, RequestedAt: now.Add(-10 * time.Minute), ExpiresAt: expiresAt,
		})
		if err != nil {
			t.Fatalf("seed: %v", err)
		}
		return ap.ID
	}
	due := seed(&past)
	notYet := seed(&future)
	unbounded := seed(nil)

	n, err := approval.ExpireStale(ctx, f, 24*time.Hour)
	if err != nil {
		t.Fatalf("ExpireStale: %v", err)
	}
	if n != 1 {
		t.Fatalf("expired = %d, want 1 — only the request past its run's wait/end is due", n)
	}
	for id, want := range map[uuid.UUID]types.ApprovalState{
		due: types.ApprovalExpired, notYet: types.ApprovalPending, unbounded: types.ApprovalPending,
	} {
		got, _ := f.GetApproval(ctx, id)
		if got.State != want {
			t.Errorf("approval %s state = %s, want %s", id, got.State, want)
		}
	}
}

// TestExpireStale_PagesTheStaleSet: the sweep reads the stale rows in bounded keyset pages and never
// the fleet's whole pending list, and a fresh row stays pending.
func TestExpireStale_PagesTheStaleSet(t *testing.T) {
	ctx := t.Context()
	stale := time.Now().UTC().Add(-2 * time.Hour)
	f := &fakeStore{}
	const total = 600
	for range total {
		if _, err := f.CreateApproval(ctx, types.ApprovalRequest{
			ID: uuid.New(), RunID: uuid.New(), Kind: types.ApprovalEgressDomain,
			State: types.ApprovalPending, RequestedAt: stale,
		}); err != nil {
			t.Fatal(err)
		}
	}
	fresh, _ := f.CreateApproval(ctx, types.ApprovalRequest{
		ID: uuid.New(), RunID: uuid.New(), Kind: types.ApprovalEgressDomain,
		State: types.ApprovalPending, RequestedAt: time.Now().UTC(),
	})

	n, err := approval.ExpireStale(ctx, f, time.Hour)
	if err != nil || n != total {
		t.Fatalf("ExpireStale = %d, %v; want %d, nil", n, err, total)
	}
	if f.listAllCalls != 0 || len(f.pageSizes) < 3 || slices.Max(f.pageSizes) > 256 {
		t.Fatalf("fleet reads = %d, page sizes = %v; want no fleet read and pages of at most 256", f.listAllCalls, f.pageSizes)
	}
	if got, _ := f.GetApproval(ctx, fresh.ID); got.State != types.ApprovalPending {
		t.Fatalf("fresh approval is %s, want it left pending", got.State)
	}
}

// dbClockStore is fakeStore on a database clock of its own: DatabaseNow answers at, or fails with err.
type dbClockStore struct {
	*fakeStore
	at  time.Time
	err error
}

func (s dbClockStore) DatabaseNow(context.Context) (time.Time, error) { return s.at, s.err }

// TestExpireStale_CutoffIsOnTheDatabaseClock: the sweep's now is the database's, the clock that stamps requested_at
// and the run times expires_at comes from. skew is this process's clock less the database's. Catches: a now on this
// process's clock, which expires a row skew early when this process runs ahead and skew late when it runs behind.
func TestExpireStale_CutoffIsOnTheDatabaseClock(t *testing.T) {
	const window = time.Hour
	for _, skew := range []time.Duration{-30 * time.Second, 0, 30 * time.Second} {
		for _, tc := range []struct {
			name string
			// requested is when the row was raised, and expires its own expiry when set, from the database's now.
			requested time.Duration
			expires   *time.Duration
			want      bool
		}{
			{"raised 5s before the cutoff", -window - 5*time.Second, nil, true},
			{"raised 5s after the cutoff", -window + 5*time.Second, nil, false},
			{"its own expiry 5s ago", -time.Minute, new(-5 * time.Second), true},
			{"its own expiry in 5s", -time.Minute, new(5 * time.Second), false},
		} {
			t.Run(fmt.Sprintf("skew %s, %s", skew, tc.name), func(t *testing.T) {
				dbAt := time.Now().UTC().Add(-skew)
				ap := types.ApprovalRequest{ID: uuid.New(), RunID: uuid.New(), Kind: types.ApprovalEgressDomain,
					State: types.ApprovalPending, RequestedAt: dbAt.Add(tc.requested)}
				if tc.expires != nil {
					ap.ExpiresAt = new(dbAt.Add(*tc.expires))
				}
				f := &fakeStore{}
				if _, err := f.CreateApproval(t.Context(), ap); err != nil {
					t.Fatal(err)
				}
				n, err := approval.ExpireStale(t.Context(), dbClockStore{fakeStore: f, at: dbAt}, window)
				if err != nil || (n == 1) != tc.want {
					t.Fatalf("expired %d, err %v; want expired=%v", n, err, tc.want)
				}
			})
		}
	}
}

// TestExpireStale_UnreadableDatabaseClockExpiresNothing: a database clock that cannot be read fails the tick and
// expires nothing, and a store that answers zero has this process's clock stand in.
func TestExpireStale_UnreadableDatabaseClockExpiresNothing(t *testing.T) {
	read := errors.New("database clock: connection reset")
	f := &fakeStore{}
	if _, err := f.CreateApproval(t.Context(), types.ApprovalRequest{ID: uuid.New(), RunID: uuid.New(),
		Kind: types.ApprovalEgressDomain, State: types.ApprovalPending, RequestedAt: time.Now().UTC().Add(-2 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if n, err := approval.ExpireStale(t.Context(), dbClockStore{fakeStore: f, err: read}, time.Hour); !errors.Is(err, read) || n != 0 || f.decideCalls != 0 {
		t.Fatalf("expired %d with %d decides, err %v; want none and the read's error", n, f.decideCalls, err)
	}
	if n, err := approval.ExpireStale(t.Context(), dbClockStore{fakeStore: f}, time.Hour); err != nil || n != 1 {
		t.Fatalf("a zero database clock: expired %d, err %v; want 1 on this process's clock", n, err)
	}
}

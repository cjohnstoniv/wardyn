// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package approval_test

import (
	"errors"
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

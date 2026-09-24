// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestPG_RunLimitsRoundTrip pins migration 0072's four run columns through
// CreateRun and GetRun: the lease end, the wait, the captured limits and the
// profile id. A legacy-shaped run (none set) reads back as no end, wait 0, zero
// limits and no profile.
func TestPG_RunLimitsRoundTrip(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	pg := store.NewPG(pool)

	profileID := uuid.New()
	endsAt := time.Now().UTC().Add(30 * 24 * time.Hour).Truncate(time.Microsecond)
	limits := types.RunLimits{
		MaxEndAheadSec: 30 * 86400, DefaultEndSec: 86400, AllowNoEnd: true,
		MaxWaitSec: 8 * 3600, DefaultWaitSec: 3600, UserChangesLimits: true, PauseIdleAfterSec: 1800,
	}
	r := newRun(types.RunRunning)
	r.EndsAt, r.WaitBudgetSec, r.RunLimits, r.GovernanceProfileID = &endsAt, 3600, limits, &profileID
	persistRun(t, ctx, pool, r)

	got, err := pg.GetRun(ctx, r.ID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if got.EndsAt == nil || !got.EndsAt.Equal(endsAt) {
		t.Errorf("ends_at = %v, want %v", got.EndsAt, endsAt)
	}
	if got.WaitBudgetSec != 3600 || got.RunLimits != limits {
		t.Errorf("wait = %d, limits = %+v; want 3600, %+v", got.WaitBudgetSec, got.RunLimits, limits)
	}
	if got.GovernanceProfileID == nil || *got.GovernanceProfileID != profileID {
		t.Errorf("governance_profile_id = %v, want %s", got.GovernanceProfileID, profileID)
	}

	legacy := persistRun(t, ctx, pool, newRun(types.RunRunning))
	if legacy.EndsAt != nil || legacy.WaitBudgetSec != 0 || legacy.RunLimits != (types.RunLimits{}) || legacy.GovernanceProfileID != nil {
		t.Errorf("legacy run = ends %v wait %d limits %+v profile %v; want none of them",
			legacy.EndsAt, legacy.WaitBudgetSec, legacy.RunLimits, legacy.GovernanceProfileID)
	}
}

// TestPG_ApprovalExpiresAtFollowsTheRun pins the read-time expiry every
// approval reader returns: min(requested_at + the run's wait, the run's end),
// NULL when the run has neither, and a change to the run reaching an
// already-open request with no write to the approval row.
func TestPG_ApprovalExpiresAtFollowsTheRun(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	pg := store.NewPG(pool)
	requested := time.Now().UTC().Truncate(time.Microsecond)

	raise := func(run types.AgentRun) types.ApprovalRequest {
		t.Helper()
		persistRun(t, ctx, pool, run)
		ap, err := pg.CreateApproval(ctx, types.ApprovalRequest{
			ID: uuid.New(), RunID: run.ID, Kind: types.ApprovalToolCall,
			RequestedScope: []byte(`{"tool":"Bash"}`), State: types.ApprovalPending, RequestedAt: requested,
		})
		if err != nil {
			t.Fatalf("CreateApproval: %v", err)
		}
		return ap
	}
	check := func(t *testing.T, name string, got, want *time.Time) {
		t.Helper()
		switch {
		case want == nil && got != nil:
			t.Errorf("%s: expires_at = %v, want none", name, *got)
		case want != nil && (got == nil || !got.Equal(*want)):
			t.Errorf("%s: expires_at = %v, want %v", name, got, *want)
		}
	}

	waitOnly := newRun(types.RunRunning)
	waitOnly.WaitBudgetSec = 3600
	ap := raise(waitOnly)
	byWait := requested.Add(time.Hour)
	check(t, "wait only (RETURNING)", ap.ExpiresAt, &byWait)

	endFirst := newRun(types.RunRunning)
	endsAt := requested.Add(10 * time.Minute)
	endFirst.WaitBudgetSec, endFirst.EndsAt = 3600, &endsAt
	ap = raise(endFirst)
	fresh, err := pg.GetApproval(ctx, ap.ID)
	if err != nil {
		t.Fatalf("GetApproval: %v", err)
	}
	check(t, "the end comes first (GetApproval)", fresh.ExpiresAt, &endsAt)

	ap = raise(newRun(types.RunRunning))
	check(t, "legacy run", ap.ExpiresAt, nil)

	// The run's end moves; the open request's expiry moves with it.
	later := requested.Add(20 * time.Minute)
	if _, err := pool.Exec(ctx, `UPDATE agent_runs SET ends_at=$1 WHERE id=$2`, later, ap.RunID); err != nil {
		t.Fatalf("set ends_at: %v", err)
	}
	pending, err := pg.ListApprovals(ctx, types.ApprovalPending)
	if err != nil {
		t.Fatalf("ListApprovals: %v", err)
	}
	for _, p := range pending {
		if p.ID == ap.ID {
			check(t, "after the run's end changed (ListApprovals)", p.ExpiresAt, &later)
			return
		}
	}
	t.Fatalf("approval %s missing from the PENDING list", ap.ID)
}

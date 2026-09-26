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

// TestPG_RunPause pins migration 0085 through the pause surface: the presence
// stamp, the pause mark's compare on the presence clock, on a still-open
// request (waiting) and on no open request (idle), the columns reading back on
// the run, clearing, and an end clearing a pause.
func TestPG_RunPause(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	pg := store.NewPG(pool)

	run := newRun(types.RunRunning)
	run.SandboxRef, run.WaitBudgetSec = "ref-pause", 3600
	persistRun(t, ctx, pool, run)

	if got, _ := pg.GetRun(ctx, run.ID); got.ActiveAt != nil || got.PausedAt != nil || got.PausedReason != "" {
		t.Fatalf("new run = active %v paused %v %q; want neither", got.ActiveAt, got.PausedAt, got.PausedReason)
	}
	if paused, err := pg.StampRunActive(ctx, run.ID); err != nil || paused {
		t.Fatalf("StampRunActive = %v, %v; want stamped, not paused", paused, err)
	}
	stamped, _ := pg.GetRun(ctx, run.ID)
	if stamped.ActiveAt == nil {
		t.Fatal("active_at not stamped")
	}

	// No open request: a waiting pause does not land; an idle one with a stale
	// presence snapshot does not either.
	if ok, err := pg.MarkRunPaused(ctx, run.ID, types.PauseWaiting, stamped.ActiveAt); err != nil || ok {
		t.Errorf("waiting pause with no request = %v, %v; want false", ok, err)
	}
	stale := stamped.ActiveAt.Add(-time.Minute)
	if ok, err := pg.MarkRunPaused(ctx, run.ID, types.PauseIdle, &stale); err != nil || ok {
		t.Errorf("idle pause on a moved presence clock = %v, %v; want false", ok, err)
	}
	if _, err := pg.MarkRunPaused(ctx, run.ID, "napping", stamped.ActiveAt); err == nil {
		t.Error("an unknown pause reason must be refused at the write boundary")
	}

	// An open tool-call request: listed as open and waiting, and the waiting
	// pause lands exactly once.
	ap, err := pg.CreateApproval(ctx, newApproval(run.ID, time.Now().UTC()))
	if err != nil {
		t.Fatalf("create approval: %v", err)
	}
	c := pauseCandidate(t, pg, run.ID)
	if c == nil || !c.OpenRequest || !c.WaitingRequest {
		t.Fatalf("candidate = %+v; want listed with an open, waiting request", c)
	}
	for i, want := range []bool{true, false} {
		if ok, err := pg.MarkRunPaused(ctx, run.ID, types.PauseWaiting, stamped.ActiveAt); err != nil || ok != want {
			t.Errorf("MarkRunPaused #%d = %v, %v; want %v", i+1, ok, err, want)
		}
	}
	paused, _ := pg.GetRun(ctx, run.ID)
	if paused.PausedAt == nil || paused.PausedReason != types.PauseWaiting || paused.State != types.RunRunning {
		t.Errorf("paused run = %v %q %s; want paused waiting, still RUNNING", paused.PausedAt, paused.PausedReason, paused.State)
	}
	if isPaused, err := pg.StampRunActive(ctx, run.ID); err != nil || !isPaused {
		t.Errorf("StampRunActive on a paused run = %v, %v; want it to say paused", isPaused, err)
	}
	if open, err := pg.RunHasOpenRequest(ctx, run.ID); err != nil || !open {
		t.Errorf("RunHasOpenRequest = %v, %v; want true", open, err)
	}
	for i, want := range []bool{true, false} {
		if ok, err := pg.ClearRunPaused(ctx, run.ID); err != nil || ok != want {
			t.Errorf("ClearRunPaused #%d = %v, %v; want %v", i+1, ok, err, want)
		}
	}

	// An idle pause never lands while a request is open; once it is decided,
	// it does. Then an end clears the pause.
	fresh, _ := pg.GetRun(ctx, run.ID)
	if ok, err := pg.MarkRunPaused(ctx, run.ID, types.PauseIdle, fresh.ActiveAt); err != nil || ok {
		t.Fatalf("idle pause with an open request = %v, %v; want false", ok, err)
	}
	if _, err := pg.DecideApproval(ctx, ap.ID, types.ApprovalDecision{State: types.ApprovalApproved, DecidedBy: "owner"}); err != nil {
		t.Fatalf("decide approval: %v", err)
	}
	if ok, err := pg.MarkRunPaused(ctx, run.ID, types.PauseIdle, fresh.ActiveAt); err != nil || !ok {
		t.Fatalf("idle pause = %v, %v; want true", ok, err)
	}
	now := time.Now().UTC()
	if _, err := pool.Exec(ctx, `UPDATE agent_runs SET ends_at=$2 WHERE id=$1`, run.ID, now.Add(-time.Minute)); err != nil {
		t.Fatalf("set end: %v", err)
	}
	if ok, err := pg.MarkRunEnded(ctx, run.ID, now); err != nil || !ok {
		t.Fatalf("MarkRunEnded = %v, %v", ok, err)
	}
	if ended, _ := pg.GetRun(ctx, run.ID); ended.PausedAt != nil || ended.PausedReason != "" {
		t.Errorf("ended run still paused: %v %q", ended.PausedAt, ended.PausedReason)
	}
	if pauseCandidate(t, pg, run.ID) != nil {
		t.Error("a kept run must not be a pause candidate")
	}
}

// TestPG_RunPause_RefusedOnAKeptRun pins MarkRunPaused's `lost_at IS NULL`
// condition: a run already lost (kept) must never be marked paused, even when
// every other condition (RUNNING, no open request, matching presence
// snapshot) holds — a sweep racing a lose must lose.
func TestPG_RunPause_RefusedOnAKeptRun(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	pg := store.NewPG(pool)

	run := newRun(types.RunRunning)
	run.SandboxRef, run.WaitBudgetSec = "ref-pause-kept", 3600
	persistRun(t, ctx, pool, run)
	if _, err := pg.StampRunActive(ctx, run.ID); err != nil {
		t.Fatalf("StampRunActive: %v", err)
	}
	stamped, _ := pg.GetRun(ctx, run.ID)

	now := time.Now().UTC()
	if ok, err := pg.MarkRunLost(ctx, run.ID, types.LostReboot, now, 0); err != nil || !ok {
		t.Fatalf("MarkRunLost = %v, %v; want true", ok, err)
	}
	if ok, err := pg.MarkRunPaused(ctx, run.ID, types.PauseIdle, stamped.ActiveAt); err != nil || ok {
		t.Errorf("MarkRunPaused on a kept run = %v, %v; want false", ok, err)
	}
	if kept, _ := pg.GetRun(ctx, run.ID); kept.PausedAt != nil {
		t.Errorf("a kept run was marked paused: %v", kept.PausedAt)
	}
}

// TestPG_RunPause_ClearedByLostAndRevive pins the fix for the defect a review
// found: MarkRunLost and MarkRunRevived must clear a pause exactly like
// MarkRunEnded does, above — a run paused when it is lost to a reboot (its
// agent stopped from under it, paused or not) must not still read paused once
// revived, or the run page's files/resources reads would keep 409ing an agent
// that is actually running again.
func TestPG_RunPause_ClearedByLostAndRevive(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	pg := store.NewPG(pool)

	run := newRun(types.RunRunning)
	run.SandboxRef, run.WaitBudgetSec = "ref-pause-revive", 3600
	persistRun(t, ctx, pool, run)
	if _, err := pg.StampRunActive(ctx, run.ID); err != nil {
		t.Fatalf("StampRunActive: %v", err)
	}
	stamped, _ := pg.GetRun(ctx, run.ID)
	if ok, err := pg.MarkRunPaused(ctx, run.ID, types.PauseIdle, stamped.ActiveAt); err != nil || !ok {
		t.Fatalf("MarkRunPaused = %v, %v; want true", ok, err)
	}
	if paused, _ := pg.GetRun(ctx, run.ID); paused.PausedAt == nil {
		t.Fatal("precondition: run is not paused")
	}

	now := time.Now().UTC()
	if ok, err := pg.MarkRunLost(ctx, run.ID, types.LostReboot, now, 0); err != nil || !ok {
		t.Fatalf("MarkRunLost = %v, %v; want true", ok, err)
	}
	if lost, _ := pg.GetRun(ctx, run.ID); lost.PausedAt != nil || lost.PausedReason != "" {
		t.Errorf("a run lost while paused still reads paused: %v %q", lost.PausedAt, lost.PausedReason)
	}

	// Force a paused mark back onto the now-kept row directly (MarkRunPaused
	// itself refuses a kept run — TestPG_RunPause_RefusedOnAKeptRun pins that —
	// so this simulates a stale mark from before this fix, or a race), to pin
	// MarkRunRevived's OWN clear independently of MarkRunLost's above.
	if _, err := pool.Exec(ctx, `UPDATE agent_runs SET paused_at=now(), paused_reason=$2 WHERE id=$1`,
		run.ID, string(types.PauseIdle)); err != nil {
		t.Fatalf("force a stale pause mark: %v", err)
	}

	if ok, err := pg.MarkRunRevived(ctx, run.ID, types.LostReboot); err != nil || !ok {
		t.Fatalf("MarkRunRevived = %v, %v; want true", ok, err)
	}
	revived, err := pg.GetRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if revived.PausedAt != nil || revived.PausedReason != "" {
		t.Errorf("revived run still reads paused: %v %q; want the pause cleared like MarkRunEnded clears it", revived.PausedAt, revived.PausedReason)
	}
	if revived.LostAt != nil || revived.State != types.RunRunning {
		t.Errorf("revived run = lost %v state %s; want live and RUNNING", revived.LostAt, revived.State)
	}
}

// TestPG_RunPause_ReauthCountsOnceItsHoldIsOver: a credential re-auth request
// is open from the start, but it counts toward a waiting pause only once it is
// older than the longest re-auth connection hold.
func TestPG_RunPause_ReauthCountsOnceItsHoldIsOver(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	pg := store.NewPG(pool)
	for _, tc := range []struct {
		name    string
		age     time.Duration
		waiting bool
	}{
		{"young", time.Minute, false},
		{"past its hold", store.ReauthHoldMax + time.Minute, true},
	} {
		run := newRun(types.RunRunning)
		run.SandboxRef, run.WaitBudgetSec = "ref-reauth", 4*3600
		persistRun(t, ctx, pool, run)
		ap := newApproval(run.ID, time.Now().UTC().Add(-tc.age))
		ap.Kind = types.ApprovalCredentialReauth
		if _, err := pg.CreateApproval(ctx, ap); err != nil {
			t.Fatalf("%s: create approval: %v", tc.name, err)
		}
		c := pauseCandidate(t, pg, run.ID)
		if c == nil || !c.OpenRequest || c.WaitingRequest != tc.waiting {
			t.Errorf("%s: candidate = %+v; want open, waiting=%v", tc.name, c, tc.waiting)
		}
	}
}

func pauseCandidate(t *testing.T, pg store.PG, id uuid.UUID) *store.PauseCandidate {
	t.Helper()
	cands, now, err := pg.ListPauseCandidates(context.Background())
	if err != nil {
		t.Fatalf("ListPauseCandidates: %v", err)
	}
	if len(cands) > 0 && now.IsZero() {
		t.Error("ListPauseCandidates returned no store clock")
	}
	for i := range cands {
		if cands[i].Run.ID == id {
			return &cands[i]
		}
	}
	return nil
}

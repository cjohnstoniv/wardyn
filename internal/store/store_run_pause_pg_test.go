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

// TestPG_RunPause pins migration 0071 through the pause surface: the presence
// stamp, the pause mark's compare on the presence clock and on a still-open
// request, the columns reading back on the run, clearing, and an end clearing
// a pause.
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
	if _, err := pg.CreateApproval(ctx, newApproval(run.ID, time.Now().UTC())); err != nil {
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

	// An end clears a pause.
	fresh, _ := pg.GetRun(ctx, run.ID)
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

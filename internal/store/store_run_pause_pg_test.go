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

// TestPG_RunPause pins migration 0097 through the pause surface: the presence
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

	if ok, err := pg.MarkRunRevived(ctx, run.ID, types.LostReboot, nil, 0, false); err != nil || !ok {
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

// TestPG_RunPause_SurvivesAnOutageLoss pins the narrower fix a second review
// found: an outage loss must NOT clear a pause, unlike a reboot. The agent
// keeps running (frozen or not) through an outage — only its proxy is cut —
// so a cleared mark would have resume (run_pause.go) skip the thaw a still
// frozen agent still needs, and the run gets stuck paused forever.
func TestPG_RunPause_SurvivesAnOutageLoss(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	pg := store.NewPG(pool)

	run := newRun(types.RunRunning)
	run.SandboxRef, run.WaitBudgetSec = "ref-pause-outage", 3600
	persistRun(t, ctx, pool, run)
	if _, err := pg.StampRunActive(ctx, run.ID); err != nil {
		t.Fatalf("StampRunActive: %v", err)
	}
	stamped, _ := pg.GetRun(ctx, run.ID)
	if ok, err := pg.MarkRunPaused(ctx, run.ID, types.PauseIdle, stamped.ActiveAt); err != nil || !ok {
		t.Fatalf("MarkRunPaused = %v, %v; want true", ok, err)
	}

	now := time.Now().UTC()
	if ok, err := pg.MarkRunLost(ctx, run.ID, types.LostOutage, now, 0); err != nil || !ok {
		t.Fatalf("MarkRunLost(outage) = %v, %v; want true", ok, err)
	}
	lost, err := pg.GetRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if lost.PausedAt == nil || lost.PausedReason != types.PauseIdle {
		t.Errorf("an outage loss cleared the pause: %v %q; want it to survive — the agent is still frozen", lost.PausedAt, lost.PausedReason)
	}
}

// TestPG_RunPause_SurvivesALiveRestart: an admin's "restart with current
// limits" (MarkRunRevived(id, "")) never touches the agent's own process — it
// only replaces the proxy — so a pause on a live run must survive it exactly
// like an outage loss survives above.
func TestPG_RunPause_SurvivesALiveRestart(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	pg := store.NewPG(pool)

	run := newRun(types.RunRunning)
	run.SandboxRef, run.WaitBudgetSec = "ref-pause-restart", 3600
	persistRun(t, ctx, pool, run)
	if _, err := pg.StampRunActive(ctx, run.ID); err != nil {
		t.Fatalf("StampRunActive: %v", err)
	}
	stamped, _ := pg.GetRun(ctx, run.ID)
	if ok, err := pg.MarkRunPaused(ctx, run.ID, types.PauseIdle, stamped.ActiveAt); err != nil || !ok {
		t.Fatalf("MarkRunPaused = %v, %v; want true", ok, err)
	}

	if ok, err := pg.MarkRunRevived(ctx, run.ID, "", nil, 0, false); err != nil || !ok {
		t.Fatalf("MarkRunRevived(live) = %v, %v; want true", ok, err)
	}
	restarted, err := pg.GetRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if restarted.PausedAt == nil || restarted.PausedReason != types.PauseIdle {
		t.Errorf("a live restart cleared the pause: %v %q; want it to survive — the agent's own process is untouched", restarted.PausedAt, restarted.PausedReason)
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

// TestPG_RunPause_ListsARequestDecidedSincePause pins the candidate flag the sweep's idle backstop reads: a
// request decided before the pause began is not "since", one decided after it is, and a run that is not
// paused never reports it.
func TestPG_RunPause_ListsARequestDecidedSincePause(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	pg := store.NewPG(pool)
	run := newRun(types.RunRunning)
	run.SandboxRef, run.WaitBudgetSec = "ref-decided", 3600
	persistRun(t, ctx, pool, run)
	decide := func(id uuid.UUID) {
		t.Helper()
		if _, err := pg.DecideApproval(ctx, id, types.ApprovalDecision{State: types.ApprovalApproved, DecidedBy: "owner"}); err != nil {
			t.Fatalf("decide approval: %v", err)
		}
	}
	since := func(step string, want bool) {
		t.Helper()
		if c := pauseCandidate(t, pg, run.ID); c == nil || c.RequestDecidedSincePause != want {
			t.Errorf("%s: candidate = %+v; want RequestDecidedSincePause = %v", step, c, want)
		}
	}

	before, err := pg.CreateApproval(ctx, newApproval(run.ID, time.Now().UTC()))
	if err != nil {
		t.Fatalf("create approval: %v", err)
	}
	decide(before.ID)
	since("decided on a run that is not paused", false)

	fresh, _ := pg.GetRun(ctx, run.ID)
	if ok, err := pg.MarkRunPaused(ctx, run.ID, types.PauseIdle, fresh.ActiveAt); err != nil || !ok {
		t.Fatalf("idle pause = %v, %v; want true", ok, err)
	}
	since("decided before the pause", false)

	after, err := pg.CreateApproval(ctx, newApproval(run.ID, time.Now().UTC()))
	if err != nil {
		t.Fatalf("create approval: %v", err)
	}
	since("a request open since the pause", false)
	decide(after.ID)
	since("decided after the pause", true)
}

// TestPG_RunPause_ListsAHandBackSincePause: a takeover handed back after an idle freeze that came while it
// was held is "since" the pause, so the backstop wakes a run whose hand back committed but whose thaw did not
// run; one handed back before the pause is not.
func TestPG_RunPause_ListsAHandBackSincePause(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	pg := store.NewPG(pool)
	run := newRun(types.RunRunning)
	run.SandboxRef, run.WaitBudgetSec = "ref-handback", 3600
	persistRun(t, ctx, pool, run)
	since := func(step string, want bool) {
		t.Helper()
		if c := pauseCandidate(t, pg, run.ID); c == nil || c.RequestDecidedSincePause != want {
			t.Errorf("%s: candidate = %+v; want RequestDecidedSincePause = %v", step, c, want)
		}
	}
	takeover := func() uuid.UUID {
		t.Helper()
		ap := newApproval(run.ID, time.Now().UTC())
		ap.Kind = types.ApprovalHumanTakeover
		created, err := pg.CreateApproval(ctx, ap)
		if err != nil {
			t.Fatalf("create takeover: %v", err)
		}
		if _, _, err := pg.TakeOverApproval(ctx, created.ID, "holder"); err != nil {
			t.Fatalf("take over: %v", err)
		}
		return created.ID
	}
	pause := func() {
		t.Helper()
		fresh, _ := pg.GetRun(ctx, run.ID)
		if ok, err := pg.MarkRunPaused(ctx, run.ID, types.PauseIdle, fresh.ActiveAt); err != nil || !ok {
			t.Fatalf("idle pause while held = %v, %v; want true", ok, err)
		}
	}

	early := takeover()
	if _, err := pg.HandBackApproval(ctx, early, "holder", time.Now().UTC()); err != nil {
		t.Fatalf("hand back: %v", err)
	}
	time.Sleep(10 * time.Millisecond)
	held := takeover()
	pause()
	since("handed back before the pause, another still held", false)
	time.Sleep(10 * time.Millisecond)
	if _, err := pg.HandBackApproval(ctx, held, "holder", time.Now().UTC()); err != nil {
		t.Fatalf("hand back: %v", err)
	}
	since("handed back after the pause", true)
}

// TestPG_RunPause_AHandBackIsReadOnTheDatabaseClock: paused_at is the database's now(), so a hand back
// stamped from an app clock an hour behind it would read as before every pause and the backstop would never
// wake the run, and one an hour ahead would read as after a pause that followed it.
func TestPG_RunPause_AHandBackIsReadOnTheDatabaseClock(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		skew time.Duration
	}{{"app clock an hour behind", -time.Hour}, {"app clock an hour ahead", time.Hour}} {
		pg := store.NewPG(pool)
		pg.Now = func() time.Time { return time.Now().Add(tc.skew) }
		run := newRun(types.RunRunning)
		run.SandboxRef, run.WaitBudgetSec = "ref-handback-"+tc.skew.String(), 3600
		persistRun(t, ctx, pool, run)
		since := func(step string, want bool) {
			t.Helper()
			if c := pauseCandidate(t, pg, run.ID); c == nil || c.RequestDecidedSincePause != want {
				t.Errorf("%s, %s: candidate = %+v; want RequestDecidedSincePause = %v", tc.name, step, c, want)
			}
		}
		heldThenBack := func(handBack bool) uuid.UUID {
			t.Helper()
			ap := newApproval(run.ID, time.Now().UTC())
			ap.Kind = types.ApprovalHumanTakeover
			created, err := pg.CreateApproval(ctx, ap)
			if err != nil {
				t.Fatalf("%s: create takeover: %v", tc.name, err)
			}
			if _, _, err := pg.TakeOverApproval(ctx, created.ID, "holder"); err != nil {
				t.Fatalf("%s: take over: %v", tc.name, err)
			}
			if handBack {
				if _, err := pg.HandBackApproval(ctx, created.ID, "holder", pg.Now().UTC()); err != nil {
					t.Fatalf("%s: hand back: %v", tc.name, err)
				}
			}
			return created.ID
		}

		heldThenBack(true)
		time.Sleep(10 * time.Millisecond)
		held := heldThenBack(false)
		fresh, _ := pg.GetRun(ctx, run.ID)
		if ok, err := pg.MarkRunPaused(ctx, run.ID, types.PauseIdle, fresh.ActiveAt); err != nil || !ok {
			t.Fatalf("%s: idle pause while held = %v, %v; want true", tc.name, ok, err)
		}
		since("handed back before the pause", false)
		time.Sleep(10 * time.Millisecond)
		if _, err := pg.HandBackApproval(ctx, held, "holder", pg.Now().UTC()); err != nil {
			t.Fatalf("%s: hand back: %v", tc.name, err)
		}
		since("handed back after the pause", true)
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

// TestPG_RunPauseSettles pins migration 0132 through store.PauseSettler: a note
// is due only after its delay, a second note moves due_at forward and never
// back, a clear naming a due_at that is no longer the row's leaves it, and the
// run's deletion takes the row with it.
func TestPG_RunPauseSettles(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	ctx := context.Background()
	pg := store.NewPG(pool)
	run := newRun(types.RunRunning)
	run.SandboxRef = "ref-settle"
	persistRun(t, ctx, pool, run)

	due := func() []store.PauseSettle {
		t.Helper()
		got, err := pg.DuePauseSettles(ctx, time.Minute)
		if err != nil {
			t.Fatalf("DuePauseSettles: %v", err)
		}
		return got
	}
	first, err := pg.NotePauseSettle(ctx, run.ID, time.Hour)
	if err != nil {
		t.Fatalf("NotePauseSettle: %v", err)
	}
	if got := due(); len(got) != 0 {
		t.Fatalf("a settle due in an hour is listed due now: %+v", got)
	}
	if again, err := pg.NotePauseSettle(ctx, run.ID, time.Minute); err != nil || !again.Equal(first) {
		t.Errorf("a sooner note = %v, %v; want due_at kept at %v", again, err, first)
	}
	later, err := pg.NotePauseSettle(ctx, run.ID, 2*time.Hour)
	if err != nil || !later.After(first) {
		t.Fatalf("a later note = %v, %v; want due_at moved past %v", later, err, first)
	}
	if err := pg.ClearPauseSettle(ctx, run.ID, first); err != nil {
		t.Fatalf("ClearPauseSettle: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE run_pause_settles SET due_at = due_at - interval '3 hours' WHERE run_id = $1`, run.ID); err != nil {
		t.Fatal(err)
	}
	got := due()
	if len(got) != 1 || got[0].RunID != run.ID {
		t.Fatalf("due settles = %+v; want the run's, which a clear naming an older due_at must not delete", got)
	}
	if err := pg.ClearPauseSettle(ctx, run.ID, got[0].DueAt); err != nil {
		t.Fatalf("ClearPauseSettle: %v", err)
	}
	if got := due(); len(got) != 0 {
		t.Fatalf("a clear naming the row's due_at left %+v", got)
	}

	if _, err := pg.NotePauseSettle(ctx, run.ID, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM agent_runs WHERE id = $1`, run.ID); err != nil {
		t.Fatal(err)
	}
	var left int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM run_pause_settles`).Scan(&left); err != nil || left != 0 {
		t.Errorf("settles left after the run's deletion = %d (%v), want 0", left, err)
	}
}

// TestPG_RunPauseIntent pins the pause's intent write: it records a settle only
// when none is held, never moves a due_at already there, and says whether it
// wrote; PauseSettleFor reads the record, or the zero time when there is none.
func TestPG_RunPauseIntent(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	ctx := context.Background()
	pg := store.NewPG(pool)
	run := newRun(types.RunRunning)
	run.SandboxRef = "ref-intent"
	persistRun(t, ctx, pool, run)

	if got, err := pg.PauseSettleFor(ctx, run.ID); err != nil || !got.IsZero() {
		t.Fatalf("PauseSettleFor with no record = %v, %v; want the zero time", got, err)
	}
	first, wrote, err := pg.NotePauseIntent(ctx, run.ID, time.Hour)
	if err != nil || !wrote {
		t.Fatalf("an intent with no record held = %v, %v, %v; want it written", first, wrote, err)
	}
	if got, err := pg.PauseSettleFor(ctx, run.ID); err != nil || !got.Equal(first) {
		t.Fatalf("PauseSettleFor = %v, %v; want %v", got, err, first)
	}
	if again, wrote, err := pg.NotePauseIntent(ctx, run.ID, 2*time.Hour); err != nil || wrote || !again.Equal(first) {
		t.Errorf("a second intent = %v, %v, %v; want the held due_at %v returned and nothing written", again, wrote, err, first)
	}
	if err := pg.ClearPauseSettle(ctx, run.ID, first); err != nil {
		t.Fatalf("ClearPauseSettle: %v", err)
	}
	if got, err := pg.PauseSettleFor(ctx, run.ID); err != nil || !got.IsZero() {
		t.Fatalf("PauseSettleFor after the clear = %v, %v; want the zero time", got, err)
	}
	if _, wrote, err := pg.NotePauseIntent(ctx, uuid.New(), time.Hour); err == nil || wrote {
		t.Errorf("an intent for a run that does not exist = %v, %v; want an error", wrote, err)
	}
}

// TestPG_RunPauseSettleLateLook pins the late look on the database's clock: a
// due record is listed with Last unset until its due_at is lookFor in the past,
// and with Last set from then on.
func TestPG_RunPauseSettleLateLook(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	ctx := context.Background()
	pg := store.NewPG(pool)
	run := newRun(types.RunRunning)
	run.SandboxRef = "ref-late-look"
	persistRun(t, ctx, pool, run)
	if _, err := pg.NotePauseSettle(ctx, run.ID, 0); err != nil {
		t.Fatal(err)
	}
	last := func(ago time.Duration) bool {
		t.Helper()
		if _, err := pool.Exec(ctx, `UPDATE run_pause_settles SET due_at = now() - make_interval(secs => $2) WHERE run_id = $1`,
			run.ID, ago.Seconds()); err != nil {
			t.Fatal(err)
		}
		got, err := pg.DuePauseSettles(ctx, 2*time.Minute)
		if err != nil || len(got) != 1 || got[0].RunID != run.ID {
			t.Fatalf("DuePauseSettles = %+v, %v; want the run's record", got, err)
		}
		return got[0].Last
	}
	if last(time.Second) {
		t.Error("a record due a second ago is listed as its last settle; want it looked at again until two minutes past due")
	}
	if !last(2*time.Minute + time.Second) {
		t.Error("a record due two minutes and a second ago is not listed as its last settle")
	}
}

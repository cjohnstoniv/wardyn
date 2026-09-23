// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

// THE TWO-CLOCKS HARNESS (B8-F3 + B8-F2). One file, because the two findings are
// one class: a value wardynd stamped from its OWN clock, compared against a
// value POSTGRES stamped, with the skew between them landing inside the
// inequality. Each half has its own fail-open direction and its own victim:
//
//   - B8-F3, the boot heal. approvals.decided_at came from wardynd and
//     workspaces.egress_edited_at from Postgres, and
//     ReconcileWorkspaceEgressDecisions' ONLY newer-action guard is
//     `decided_at < egress_edited_at`. With wardynd running ahead, an operator
//     who approves `always` and then undoes it through the documented PUT gets
//     the decision RE-APPLIED at the next restart: the host is back on the
//     allowlist, durably and fail-OPEN, which is the loss migration 0055 exists
//     to prevent.
//
//   - B8-F2, the idle reaper. agent_runs.updated_at is stamped by Postgres
//     (TouchRun, and every scoped writer) while the reaper measured
//     `wardynd_now - updated_at`. TouchDebounce's 30 seconds is the whole
//     margin, so a few minutes of skew ahead STOPS an actively-attached run and
//     revokes its credentials; skew behind never reaps at all.
//
// The SKEW is simulated honestly, the way session_revocation_clock_pg_test.go
// does it: by handing the app-clock seam a clock that runs ahead of the
// database's — store.PG.Now for the stamps, lifecycle.Config.Now for the
// measurement — and stamping everything wardynd would stamp from that same fast
// clock. Nothing here touches the host's clock; it does not have to.
//
// Guarded by WARDYN_TEST_PG; skipped cleanly without one.

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/api"
	"github.com/cjohnstoniv/wardyn/internal/lifecycle"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// twoClocksSkew is how far ahead of Postgres this deployment's wardynd runs. Minutes,
// because that is the scale both findings describe and it is far past
// TouchDebounce.
const twoClocksSkew = 10 * time.Minute

// twoClocksFastClock is wardynd's clock in these probes: every reading it takes — the
// stamps it writes AND the ages it measures — is twoClocksSkew ahead of the database.
func twoClocksFastClock() time.Time { return time.Now().Add(twoClocksSkew) }

// twoClocksDBNow reads the DATABASE's clock, which is the one every column here is
// compared against.
func twoClocksDBNow(t *testing.T, pool *pgxpool.Pool) time.Time {
	t.Helper()
	var now time.Time
	if err := pool.QueryRow(context.Background(), `SELECT now()`).Scan(&now); err != nil {
		t.Fatalf("read the database clock: %v", err)
	}
	return now
}

// skewedRun persists a RUNNING run stamped by the fast clock, exactly as a fast
// wardynd would stamp it, and returns it.
func skewedRun(t *testing.T, pg store.PG, autoStopSec int) types.AgentRun {
	t.Helper()
	id := uuid.New()
	run, err := pg.CreateRun(context.Background(), types.AgentRun{
		ID: id, CreatedAt: twoClocksFastClock(), UpdatedAt: twoClocksFastClock(),
		CreatedBy: "op@example.com", Agent: "claude-code", Task: "two-clocks probe",
		ConfinementClass: types.CC2, State: types.RunRunning,
		SPIFFEID: "spiffe://wardyn.test/agent-run/" + id.String(), RunnerTarget: "docker",
		AutoStopAfterSec: autoStopSec,
	})
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pg.Pool.Exec(context.Background(), `DELETE FROM agent_runs WHERE id=$1`, id)
	})
	return run
}

// B8-F3: the boot heal

// TestPG_AFastClockCannotResurrectAnUndoneAlwaysApproval drives the whole
// documented sequence — decide `always`, undo it through the PUT, restart — with
// wardynd's clock ten minutes ahead, and asserts the host stays OFF the list.
func TestPG_AFastClockCannotResurrectAnUndoneAlwaysApproval(t *testing.T) {
	pool := revocationPool(t)
	ctx := context.Background()
	// The store wardynd would run with: a fast app clock behind every stamp.
	pg := store.PG{Pool: pool, Now: twoClocksFastClock}
	srv := api.New(api.Config{
		Store:     pg,
		Approvals: &approvalService{st: approvalStore{PG: pg, rec: &fakeAuditRecorder{}}},
	})

	const undone = "undone.probe.example.com"
	const kept = "kept.probe.example.com"

	ws, err := pg.CreateWorkspace(ctx, types.Workspace{
		ID: uuid.New(), Name: "two-clocks-" + uuid.NewString(),
		Status: types.WorkspaceScanned, CreatedAt: twoClocksFastClock(), UpdatedAt: twoClocksFastClock(),
	})
	if err != nil {
		t.Fatalf("create workspace: %v", err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM workspaces WHERE id=$1`, ws.ID) })

	decideAlways := func(t *testing.T, host string) types.ApprovalRequest {
		t.Helper()
		run := skewedRun(t, pg, 0)
		if _, err := pool.Exec(ctx, `UPDATE agent_runs SET workspace_id=$1 WHERE id=$2`, ws.ID, run.ID); err != nil {
			t.Fatalf("link the run to the workspace: %v", err)
		}
		ap, err := pg.CreateApproval(ctx, types.ApprovalRequest{
			ID: uuid.New(), RunID: run.ID, Kind: types.ApprovalEgressDomain,
			RequestedScope: []byte(`{"host":"` + host + `"}`),
			State:          types.ApprovalPending, RequestedAt: twoClocksFastClock(),
		})
		if err != nil {
			t.Fatalf("create approval: %v", err)
		}
		decided, err := pg.DecideApproval(ctx, ap.ID, types.ApprovalDecision{
			State: types.ApprovalApproved, DecidedBy: "op@example.com", Reason: "probe",
			Scope: types.ScopeAlways,
		})
		if err != nil {
			t.Fatalf("decide approval for %s: %v", host, err)
		}
		return decided
	}

	// (1) THE OPERATOR APPROVES `always`, so the host is on the list.
	decided := decideAlways(t, undone)
	if decided.DecidedAt == nil {
		t.Fatal("a decided approval carries no decided_at")
	}
	if at := twoClocksDBNow(t, pool); decided.DecidedAt.After(at) {
		t.Errorf("decided_at = %s, which is AFTER the database's own clock (%s).\n"+
			"It is compared against workspaces.egress_edited_at, which Postgres stamps, by the boot heal's only "+
			"newer-action guard — so for the length of the skew every decision reads as newer than every operator "+
			"edit, and an undone `always` comes back on the next restart", decided.DecidedAt, at)
	}
	if _, err := pg.AddWorkspaceEgressDecision(ctx, ws.ID, undone, true, 64); err != nil {
		t.Fatalf("write the decision onto the workspace: %v", err)
	}

	// "Seconds later", in the documented undo. More than the round trip, less
	// than any plausible skew — the finding is minutes.
	time.Sleep(25 * time.Millisecond)

	// (2) THE OPERATOR UNDOES IT through the documented PUT, which empties the
	// list and stamps egress_edited_at on the DATABASE's clock.
	undoneWS, err := pg.SetWorkspaceApprovedEgress(ctx, ws.ID, nil)
	if err != nil {
		t.Fatalf("the undo PUT: %v", err)
	}
	if undoneWS.EgressEditedAt == nil {
		t.Fatal("the undo PUT did not stamp egress_edited_at; the guard has nothing to compare")
	}

	// (3) THE RESTART.
	if _, err := srv.ReconcileWorkspaceEgressDecisions(ctx); err != nil {
		t.Fatalf("ReconcileWorkspaceEgressDecisions: %v", err)
	}
	after, err := pg.GetWorkspace(ctx, ws.ID)
	if err != nil {
		t.Fatalf("re-read the workspace: %v", err)
	}
	for _, host := range after.ApprovedEgress {
		if host == undone {
			t.Fatalf("the boot heal put %q back on a list the operator had just emptied.\n"+
				"decided_at = %s, egress_edited_at = %s: wardynd stamped the first and Postgres the second, so the "+
				"heal's only newer-action guard is an inequality between two clocks and the skew decides it — "+
				"fail-OPEN, durably, with an audit row saying success",
				undone, decided.DecidedAt, undoneWS.EgressEditedAt)
		}
	}

	// (4) NEGATIVE CONTROL — the heal still heals. A decision made AFTER the last
	// edit is genuinely the operator's newest action, and it is re-applied.
	time.Sleep(25 * time.Millisecond)
	decideAlways(t, kept)
	if _, err := srv.ReconcileWorkspaceEgressDecisions(ctx); err != nil {
		t.Fatalf("ReconcileWorkspaceEgressDecisions (the newer decision): %v", err)
	}
	after, err = pg.GetWorkspace(ctx, ws.ID)
	if err != nil {
		t.Fatalf("re-read the workspace: %v", err)
	}
	var found bool
	for _, host := range after.ApprovedEgress {
		found = found || host == kept
	}
	if !found {
		t.Errorf("a decision made AFTER the operator's last list edit was NOT re-applied (list = %v).\n"+
			"D28's heal is the point of the column; a guard that skips everything is not a fix, it is the other "+
			"failure", after.ApprovedEgress)
	}
}

// B8-F2: the idle reaper

// recordingStopper is lifecycle.Stopper, remembering which runs it was asked to
// stop. It reports the stop as APPLIED so a spurious stop is loud rather than
// swallowed by the no-op arm.
type recordingStopper struct{ stopped []uuid.UUID }

func (s *recordingStopper) StopRun(_ context.Context, runID uuid.UUID, _ time.Time) (lifecycle.StopOutcome, error) {
	s.stopped = append(s.stopped, runID)
	return lifecycle.StopOutcome{Applied: true}, nil
}

// TestPG_AFastClockDoesNotReapAnActiveRun is the reaper half: a run touched by
// an active attach a moment ago, measured by a wardynd whose clock is ten
// minutes ahead of the database that stamped the touch.
func TestPG_AFastClockDoesNotReapAnActiveRun(t *testing.T) {
	pool := revocationPool(t)
	ctx := context.Background()
	pg := store.PG{Pool: pool, Now: twoClocksFastClock}

	// A run whose auto-stop is a minute — an interactive attach session's own
	// order of magnitude. Ten minutes of skew is far past it AND far past the
	// 30s of TouchDebounce slack, which is the finding: the skew is added
	// directly to the measured age, so it only has to exceed the policy.
	run := skewedRun(t, pg, 60)
	// The attach keepalive, which stamps updated_at with the DATABASE's now() —
	// as does every writer of that column but CreateRun.
	if err := pg.TouchRun(ctx, run.ID); err != nil {
		t.Fatalf("TouchRun: %v", err)
	}

	stopper := &recordingStopper{}
	reaper := lifecycle.New(lifecycleStore{pool: pool}, stopper, &fakeAuditRecorder{},
		lifecycle.Config{Now: twoClocksFastClock})
	reaper.Tick(ctx)

	for _, id := range stopper.stopped {
		if id == run.ID {
			t.Fatalf("the reaper stopped a run touched moments ago.\n"+
				"updated_at is stamped by Postgres and the age was measured against wardynd's clock, which is %s "+
				"ahead — so every run in the deployment reads as %s idle and an actively-attached session is "+
				"stopped and its credentials revoked", twoClocksSkew, twoClocksSkew)
		}
	}

	// NEGATIVE CONTROL — a genuinely idle run still stops. Backdated on the
	// DATABASE's clock, which is what a run nobody has touched for two hours
	// actually looks like.
	if _, err := pool.Exec(ctx, `UPDATE agent_runs SET updated_at = now() - interval '2 hours' WHERE id=$1`, run.ID); err != nil {
		t.Fatalf("backdate the run: %v", err)
	}
	stopper.stopped = nil
	reaper.Tick(ctx)
	var sawIt bool
	for _, id := range stopper.stopped {
		sawIt = sawIt || id == run.ID
	}
	if !sawIt {
		t.Errorf("a run genuinely idle for two hours past a one-minute auto-stop was NOT reaped; measuring on the "+
			"database's clock must not turn the reaper off (stopped = %v)", stopper.stopped)
	}
}

// TestPG_AFastClockStampsARunsUpdatedAtOnTheDatabaseClock is CreateRun's own
// half of the same class: it was the ONE writer of agent_runs.updated_at that
// bound the app's clock, so a fast wardynd inserted a row already in the
// database's future — and the reaper, now measuring on the database's clock,
// would read that as a negative age forever.
func TestPG_AFastClockStampsARunsUpdatedAtOnTheDatabaseClock(t *testing.T) {
	pool := revocationPool(t)
	pg := store.PG{Pool: pool, Now: twoClocksFastClock}

	run := skewedRun(t, pg, 0)
	if at := twoClocksDBNow(t, pool); run.UpdatedAt.After(at) {
		t.Errorf("updated_at = %s, which is AFTER the database's own clock (%s).\n"+
			"Every other writer of this column uses now(); this one bound wardynd's clock, so the row the idle "+
			"reaper measures was born %s in the future of the clock it measures with", run.UpdatedAt, at, twoClocksSkew)
	}
	// And it is not now() EITHER: a run whose struct was stamped earlier in the
	// request keeps that instant, back-dated by its own age.
	old := types.AgentRun{
		ID: uuid.New(), CreatedAt: twoClocksFastClock(), UpdatedAt: twoClocksFastClock().Add(-90 * time.Second),
		CreatedBy: "op@example.com", Agent: "claude-code", Task: "aged stamp",
		ConfinementClass: types.CC2, State: types.RunPending, RunnerTarget: "docker",
		SPIFFEID: "spiffe://wardyn.test/agent-run/" + uuid.NewString(),
	}
	got, err := pg.CreateRun(context.Background(), old)
	if err != nil {
		t.Fatalf("create the aged run: %v", err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM agent_runs WHERE id=$1`, got.ID) })
	if age := twoClocksDBNow(t, pool).Sub(got.UpdatedAt); age < 60*time.Second || age > 3*time.Minute {
		t.Errorf("a run stamped 90s before the insert landed with an age of %s; the back-dating is subtracting "+
			"something other than the row's own age", age)
	}
}

// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/google/uuid"
)

// teardownPage sends a fresh session's state page for e's run carrying hold (nil: none, as a runner that
// predates the report sends).
func (e *containmentEnv) teardownPage(hold *runnerwire.TeardownHold) {
	e.t.Helper()
	if err := finalStatePage(e.t, e.srv, e.runnerID, runnerwire.RunState{RunID: e.run.ID, SandboxPresent: true, TeardownHold: hold,
		Execution: &runnerwire.ExecutionResult{Generation: e.gen, Operation: runnerwire.MethodExec, Attempted: true}}); err != nil {
		e.t.Fatalf("state page: %v", err)
	}
}

// TestPG_RunTeardownHoldShownUntilTheRunnerSaysItFinished: the run page shows a kill or end its runner holds
// from the first page that reports it, and keeps showing it held through a page without the report, a
// cleared report of an older hold and a report it cannot read. Only the runner's report that this kill or
// end finished shows it finished; a later hold opens it again.
func TestPG_RunTeardownHoldShownUntilTheRunnerSaysItFinished(t *testing.T) {
	e := newContainmentEnv(t, false)
	want := func(step string, kind types.RunnerActionKind, since time.Time, cleared *time.Time) {
		t.Helper()
		got := e.containment(e.person).TeardownHold
		if got == nil || got.Kind != string(kind) || !got.Since.Equal(since) || (got.ClearedAt == nil) != (cleared == nil) ||
			(cleared != nil && !got.ClearedAt.Equal(*cleared)) {
			t.Fatalf("%s: teardown hold = %+v, want %s since %v, cleared %v", step, got, kind, since, cleared)
		}
	}
	e.teardownPage(nil)
	if got := e.containment(e.person).TeardownHold; got != nil {
		t.Fatalf("a run whose runner reports no hold shows %+v", got)
	}

	since := time.Now().Add(-10 * time.Minute).UTC().Truncate(time.Microsecond)
	done := since.Add(5 * time.Minute)
	e.teardownPage(&runnerwire.TeardownHold{Kind: types.RunnerActionKill, Since: since})
	want("held", types.RunnerActionKill, since, nil)
	e.teardownPage(nil)
	want("a page without the report", types.RunnerActionKill, since, nil)
	e.teardownPage(&runnerwire.TeardownHold{Kind: types.RunnerActionKill, Since: since.Add(-time.Minute), ClearedAt: &done})
	want("a cleared report of an older hold", types.RunnerActionKill, since, nil)
	e.teardownPage(&runnerwire.TeardownHold{Kind: "stop", Since: since, ClearedAt: &done})
	want("a report of a kind this release cannot read", types.RunnerActionKill, since, nil)

	e.teardownPage(&runnerwire.TeardownHold{Kind: types.RunnerActionKill, Since: since, ClearedAt: &done})
	want("cleared", types.RunnerActionKill, since, &done)
	e.teardownPage(nil)
	want("a page without the report once cleared", types.RunnerActionKill, since, &done)

	again := since.Add(7 * time.Minute)
	e.teardownPage(&runnerwire.TeardownHold{Kind: types.RunnerActionEnd, Since: again})
	want("held again", types.RunnerActionEnd, again, nil)

	// A hold the runner made and cleared between two pages is first reported cleared: kept as such.
	if _, err := e.pool.Exec(context.Background(), `DELETE FROM run_teardown_holds WHERE run_id = $1`, e.run.ID); err != nil {
		t.Fatal(err)
	}
	finished := again.Add(time.Minute)
	e.teardownPage(&runnerwire.TeardownHold{Kind: types.RunnerActionEnd, Since: again, ClearedAt: &finished})
	want("first reported cleared", types.RunnerActionEnd, again, &finished)
}

// The boot pass queues a kill the runner refused again only once the runner has reported that the kill or end
// it held the run for finished after that refusal, both by the runner's clock. A hold still open, or one that
// cleared before the refusal, leaves the ref as a refused kill always did; a refusal after the clear stops it
// again, so a kill the runner keeps refusing is not queued on every boot.
func TestPG_OrphanSweep_QueuesARefusedKillAgainOnlyOnceItsHoldClearedAfter(t *testing.T) {
	f := newRunnerLeaseFixture(t)
	f.offline(t)
	run := f.terminalRunnerRun(t)
	sweep := func(kills int) {
		t.Helper()
		if err := f.srv.reconcileOrphanedSandbox(f.ctx); err != nil {
			t.Fatal(err)
		}
		f.teardowns(t, run, 0, kills)
	}
	refusedAt := time.Now().Add(-time.Hour).UTC().Truncate(time.Microsecond)
	refuse := func(at time.Time) {
		t.Helper()
		var id uuid.UUID
		if err := f.st.Pool.QueryRow(f.ctx, `SELECT id FROM runner_pending_actions WHERE run_id = $1 AND kind = 'kill' AND applied_at IS NULL`, run.ID).Scan(&id); err != nil {
			t.Fatal(err)
		}
		prepare := func(_ context.Context, ev types.AuditEvent) (types.AuditEvent, error) { return ev, nil }
		if _, _, err := f.st.RecordRunnerActionResult(f.ctx, f.rn.ID, run.ID, id, types.RunnerActionRefused, "docker: the macOS VM helper is still running", at, prepare); err != nil {
			t.Fatal(err)
		}
	}
	hold := func(cleared *time.Time) {
		t.Helper()
		if _, err := f.st.Pool.Exec(f.ctx, `INSERT INTO run_teardown_holds (run_id, runner_id, kind, since, cleared_at) VALUES ($1, $2, 'kill', $3, $4)
			ON CONFLICT (run_id) DO UPDATE SET cleared_at = EXCLUDED.cleared_at`, run.ID, f.rn.ID, refusedAt.Add(-2*time.Hour), cleared); err != nil {
			t.Fatal(err)
		}
	}
	at := func(d time.Duration) *time.Time { v := refusedAt.Add(d); return &v }

	sweep(1)
	refuse(refusedAt)
	sweep(1) // no hold reported
	hold(nil)
	sweep(1) // still held
	hold(at(-time.Minute))
	sweep(1) // an older hold, cleared before the refusal
	hold(at(time.Minute))
	// Cleared after the refusal: the periodic pass queues it again, before any start.
	if _, err := f.srv.SweepTerminalSandboxes(f.ctx); err != nil {
		t.Fatal(err)
	}
	f.teardowns(t, run, 0, 2)
	sweep(2) // that kill is still open: nothing new
	refuse(*at(2 * time.Minute))
	sweep(2) // refused again after the clear: not queued again
}

// heldKills holds, per local ref, what the fixture's substrate answers a kill with instead of killing.
var heldKills sync.Map // local ref -> error

// KillSandbox answers a kill of a ref in heldKills with its error and leaves the sandbox in place, as a macOS VM
// whose helper outlived its kill does; any other kill is the fake's.
func (s *stallingSub) KillSandbox(ctx context.Context, ref string) error {
	if err, ok := heldKills.Load(ref); ok {
		return err.(error)
	}
	return s.Fake.KillSandbox(ctx, ref)
}

// A kill the runner refused because it could not see the sandbox stop is queued again by the boot pass once the
// runner reports that its own retry finished it, and not while it still holds the run. The runner applies the
// kill, the next boot pass clears the run's ref, and the run is retired through the real org and runner.
func TestPG_HeldKillQueuedAgainOnceTheRunnerFinishedItAndTheRunRetires(t *testing.T) {
	f := newRunnerLeaseFixture(t)
	f.sub.goneIsStopped.Store(true)
	run := f.localRun(t, 0)
	f.srv.recordRunnerOutputWatermark(f.ctx, run, false)
	f.placed(t, &run)
	f.running(t, &run, nil)
	f.spoolOne(t, run)
	// The run ended, but its sandbox's stop never finished, so its ref stays.
	if _, err := f.st.Pool.Exec(f.ctx, `UPDATE agent_runs SET state = 'COMPLETED', ended_at = now() WHERE id = $1`, run.ID); err != nil {
		t.Fatal(err)
	}
	local := "fake-" + run.ID.String()
	heldKills.Store(local, error(&runner.TeardownUnconfirmedError{Err: errors.New("docker: the macOS VM helper is still running")}))
	t.Cleanup(func() { heldKills.Delete(local) })
	bootPass := func(kills int) {
		t.Helper()
		f.offline(t)
		if err := f.srv.reconcileOrphanedSandbox(f.ctx); err != nil {
			t.Fatal(err)
		}
		f.teardowns(t, run, 0, kills)
		f.disconnect = f.connect(t)
	}
	const holds = `SELECT count(*) FROM run_teardown_holds WHERE run_id = $1 AND cleared_at IS `

	bootPass(1) // a kill, which the next session holds and refuses
	f.waitRow(t, "refused kill", `SELECT count(*) FROM runner_pending_actions WHERE run_id = $1 AND kind = 'kill' AND outcome = 'refused'`, run.ID, 1)
	f.waitRow(t, "open hold", holds+`NULL`, run.ID, 1)
	bootPass(1) // still held: not queued again, and the next session's retry cannot finish it either
	f.waitRow(t, "open hold", holds+`NULL`, run.ID, 1)

	heldKills.Delete(local) // the helper exits
	f.reconnect(t)          // the session's retry finishes the kill, and its state page says so
	f.waitRow(t, "cleared hold", holds+`NOT NULL`, run.ID, 1)
	bootPass(2) // queued again; the next session applies it
	f.waitRow(t, "the kill queued again applied", `SELECT count(*) FROM runner_pending_actions WHERE run_id = $1 AND kind = 'kill' AND outcome = 'success'`, run.ID, 1)
	bootPass(2) // the applied kill clears the ref
	if got := f.sandboxRef(t, run); got != "" {
		t.Fatalf("a run whose kill the runner applied keeps its ref %q", got)
	}

	f.produce(t) // the retirement's own kill, applied by the next session
	f.reconnect(t)
	f.produce(t) // its retire_run, settled by the next session
	f.reconnect(t)
	f.waitRow(t, "settled retirement", `SELECT count(*) FROM runner_retirements WHERE run_id = $1 AND state = 'settled'`, run.ID, 1)
}

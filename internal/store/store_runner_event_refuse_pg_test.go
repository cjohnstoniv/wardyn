// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// insertAssertedKill queues an asserted subject's open kill for f's runner, as the org's inventory does.
func (f actionFixture) insertAssertedKill(t *testing.T) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if _, err := f.pool.Exec(t.Context(), `INSERT INTO runner_pending_actions (id, runner_id, subject, asserted_run_id, kind, ref, binding_version, owner)
		VALUES ($1,$2,'asserted',$3,'kill',$4,1,'')`, id, f.rn.ID, uuid.New(), "runner:"+f.rn.ID.String()+"/local"); err != nil {
		t.Fatal(err)
	}
	return id
}

func (f actionFixture) assertedRun(t *testing.T, id uuid.UUID) uuid.UUID {
	t.Helper()
	var run uuid.UUID
	if err := f.pool.QueryRow(t.Context(), `SELECT asserted_run_id FROM runner_pending_actions WHERE id=$1`, id).Scan(&run); err != nil {
		t.Fatal(err)
	}
	return run
}

// A run or execution row that is gone can never take the exit: it is ErrRunnerExecutionRefused, so the
// organisation refuses it for good (DR-0436), and still names the not-found underneath.
func TestPG_StageRunnerExitGoneRunIsRefused(t *testing.T) {
	f := newActionFixture(t)
	ctx := t.Context()
	unissued := f.run(t)
	exit := runnerwire.AgentExit{Generation: uuid.New(), ExecID: "exec-1", Ref: unissued.SandboxRef, ObservedAt: time.Now()}
	if err := f.st.StageRunnerExit(ctx, f.rn.ID, unissued.ID, exit); !errors.Is(err, store.ErrRunnerExecutionRefused) || !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("exit with no execution row = %v, want ErrRunnerExecutionRefused over pgx.ErrNoRows", err)
	}
	gone := f.run(t)
	exit.Generation, exit.Ref = f.issue(t, gone), gone.SandboxRef
	if _, err := f.pool.Exec(ctx, `DELETE FROM agent_runs WHERE id=$1`, gone.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.st.StageRunnerExit(ctx, f.rn.ID, gone.ID, exit); !errors.Is(err, store.ErrRunnerExecutionRefused) || !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("exit for a gone run = %v, want ErrRunnerExecutionRefused over ErrNotFound", err)
	}
}

// The not-found and changed-repeat answers of an action_result are refusals for good, and still errors.Is their
// inner sentinel, so the existing callers' checks hold.
func TestPG_RecordRunnerActionResultWraps(t *testing.T) {
	f := newActionFixture(t)
	ctx := t.Context()
	observed := time.Now().Add(-time.Hour)
	both := func(t *testing.T, name string, err, inner error) {
		t.Helper()
		if !errors.Is(err, store.ErrRunnerActionRefused) || !errors.Is(err, inner) {
			t.Fatalf("%s = %v, want ErrRunnerActionRefused over %v", name, err, inner)
		}
	}
	run := f.run(t)
	a := f.mustQueue(t, f.action(run, types.RunnerActionKill, f.issue(t, run)))
	_, _, err := f.st.RecordRunnerActionResult(ctx, f.rn.ID, run.ID, uuid.New(), types.RunnerActionSuccess, "", observed, prepareSpoolTest)
	both(t, "unknown action", err, store.ErrNotFound)
	if _, first, err := f.st.RecordRunnerActionResult(ctx, f.rn.ID, run.ID, a.ID, types.RunnerActionSuccess, "", observed, prepareSpoolTest); err != nil || !first {
		t.Fatalf("first result: first=%v err=%v", first, err)
	}
	_, _, err = f.st.RecordRunnerActionResult(ctx, f.rn.ID, run.ID, a.ID, types.RunnerActionDeferred, "", observed, prepareSpoolTest)
	both(t, "changed repeat", err, store.ErrConflict)
	gone := f.run(t)
	if _, err := f.pool.Exec(ctx, `DELETE FROM agent_runs WHERE id=$1`, gone.ID); err != nil {
		t.Fatal(err)
	}
	_, _, err = f.st.RecordRunnerActionResult(ctx, f.rn.ID, gone.ID, uuid.New(), types.RunnerActionSuccess, "", observed, prepareSpoolTest)
	both(t, "gone run", err, store.ErrNotFound)

	asserted := f.insertAssertedKill(t)
	subject := f.assertedRun(t, asserted)
	if _, first, err := f.st.RecordRunnerActionResult(ctx, f.rn.ID, subject, asserted, types.RunnerActionSuccess, "", observed, prepareSpoolTest); err != nil || !first {
		t.Fatalf("first asserted result: first=%v err=%v", first, err)
	}
	_, _, err = f.st.RecordRunnerActionResult(ctx, f.rn.ID, subject, asserted, types.RunnerActionDeferred, "", observed, prepareSpoolTest)
	both(t, "asserted changed repeat", err, store.ErrConflict)
	open := f.insertAssertedKill(t)
	if _, err := f.st.RevokeRunner(ctx, f.rn.ID, time.Now().UTC(), "alice", ""); err != nil {
		t.Fatal(err)
	}
	_, _, err = f.st.RecordRunnerActionResult(ctx, f.rn.ID, f.assertedRun(t, open), open, types.RunnerActionSuccess, "", observed, prepareSpoolTest)
	both(t, "asserted result of a revoked runner", err, store.ErrNotFound)
}

func eventRefusal(runID, actionID *uuid.UUID) store.RunnerEventRefusal {
	return store.RunnerEventRefusal{Kind: runnerwire.EventActionResult, Code: runnerwire.RefusalShape, Sentinel: "runner event shape refused",
		RunID: runID, ActionID: actionID, PayloadSHA256: strings.Repeat("0", 64), PayloadBytes: 7, Row: true}
}

// RefuseRunnerEvent closes only this runner's open action, as refused with its receipt, and writes its own row
// in the same transaction.
func TestPG_RefuseRunnerEventSettlesOwnOpenAction(t *testing.T) {
	f := newActionFixture(t)
	ctx := t.Context()
	eventRows := func(runner uuid.UUID) int {
		return f.count(t, `SELECT count(*) FROM audit_events WHERE action='runner.event.refuse' AND outcome='denied' AND actor=$1`, "runner:"+runner.String())
	}
	receipts := func(id uuid.UUID) int {
		return f.count(t, `SELECT count(*) FROM audit_events WHERE action='runner.action.refuse' AND target=$1 AND data->>'reason'='org_refused:shape'`, id.String())
	}
	openAction := func(id uuid.UUID) int {
		return f.count(t, `SELECT count(*) FROM runner_pending_actions WHERE id=$1 AND applied_at IS NULL`, id)
	}
	run := f.run(t)
	a := f.mustQueue(t, f.action(run, types.RunnerActionKill, f.issue(t, run)))

	calls := 0
	failSecond := func(_ context.Context, ev types.AuditEvent) (types.AuditEvent, error) {
		if calls++; calls == 2 {
			return types.AuditEvent{}, errors.New("seal unavailable")
		}
		return ev, nil
	}
	if _, err := f.st.RefuseRunnerEvent(ctx, f.rn.ID, eventRefusal(&run.ID, &a.ID), failSecond); err == nil {
		t.Fatal("a failed event row committed")
	}
	if openAction(a.ID) != 1 || receipts(a.ID) != 0 || eventRows(f.rn.ID) != 0 {
		t.Fatal("the settlement committed without its event row: not one transaction")
	}

	settled, err := f.st.RefuseRunnerEvent(ctx, f.rn.ID, eventRefusal(&run.ID, &a.ID), prepareSpoolTest)
	if err != nil || !settled {
		t.Fatalf("own open action: settled=%v err=%v", settled, err)
	}
	if n := f.count(t, `SELECT count(*) FROM runner_pending_actions WHERE id=$1 AND outcome='refused' AND applied_at IS NOT NULL AND observed_at IS NOT NULL`, a.ID); n != 1 ||
		receipts(a.ID) != 1 || eventRows(f.rn.ID) != 1 {
		t.Fatalf("settled=%d receipts=%d event rows=%d, want 1 each", n, receipts(a.ID), eventRows(f.rn.ID))
	}
	page, _, err := f.st.ListPendingRunnerActions(ctx, f.rn.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range page {
		if p.ID == a.ID {
			t.Fatal("the refused action is still issued in PENDING")
		}
	}

	if settled, err := f.st.RefuseRunnerEvent(ctx, f.rn.ID, eventRefusal(&run.ID, &a.ID), prepareSpoolTest); err != nil || settled ||
		receipts(a.ID) != 1 || eventRows(f.rn.ID) != 2 {
		t.Fatalf("applied action: settled=%v err=%v receipts=%d", settled, err, receipts(a.ID))
	}

	other := claimed(t, f.st, ctx, "other-owner")
	run2 := f.run(t)
	b := f.mustQueue(t, f.action(run2, types.RunnerActionKill, f.issue(t, run2)))
	if settled, err := f.st.RefuseRunnerEvent(ctx, other.ID, eventRefusal(&run2.ID, &b.ID), prepareSpoolTest); err != nil || settled ||
		openAction(b.ID) != 1 || eventRows(other.ID) != 1 {
		t.Fatalf("another runner's action: settled=%v err=%v open=%d rows=%d", settled, err, openAction(b.ID), eventRows(other.ID))
	}
	// run2 is f.rn's, so other's row stays out of its trail and names other, keeping the claim in its data.
	if n := f.count(t, `SELECT count(*) FROM audit_events WHERE action='runner.event.refuse' AND actor=$1 AND run_id IS NULL AND target=$2 AND data->>'claimed_run_id'=$3`,
		"runner:"+other.ID.String(), other.ID.String(), run2.ID.String()); n != 1 {
		t.Fatalf("a refusal naming another runner's run: %d run-less rows naming the runner, want 1", n)
	}
	if n := f.count(t, `SELECT count(*) FROM audit_events WHERE action='runner.event.refuse' AND actor=$1 AND run_id=$2 AND target=$3 AND data->>'claimed_run_id' IS NULL`,
		"runner:"+f.rn.ID.String(), run.ID, run.ID.String()); n != 2 {
		t.Fatalf("own run's refusal rows: %d in its trail, want 2", n)
	}

	asserted := f.insertAssertedKill(t)
	if settled, err := f.st.RefuseRunnerEvent(ctx, f.rn.ID, eventRefusal(nil, &asserted), prepareSpoolTest); err != nil || !settled ||
		openAction(asserted) != 0 || f.count(t, `SELECT count(*) FROM audit_events WHERE action='runner.action.refuse' AND target=$1 AND run_id IS NULL`, asserted.String()) != 1 {
		t.Fatalf("asserted subject: settled=%v err=%v", settled, err)
	}

	before := eventRows(f.rn.ID)
	if settled, err := f.st.RefuseRunnerEvent(ctx, f.rn.ID, store.RunnerEventRefusal{Kind: runnerwire.EventSpool, Code: runnerwire.RefusalShape}, prepareSpoolTest); err != nil || settled ||
		eventRows(f.rn.ID) != before {
		t.Fatalf("suppressed refusal: settled=%v err=%v, rows %d -> %d", settled, err, before, eventRows(f.rn.ID))
	}

	marked := eventRefusal(&run.ID, nil)
	marked.Marker = true
	marked.Summary = &store.RunnerEventRefusalSummary{Suppressed: 5, WindowStart: time.Now().Add(-2 * time.Hour), WindowEnd: time.Now().Add(-time.Hour)}
	if _, err := f.st.RefuseRunnerEvent(ctx, f.rn.ID, marked, prepareSpoolTest); err != nil {
		t.Fatal(err)
	}
	if f.count(t, `SELECT count(*) FROM audit_events WHERE action='runner.event.refuse' AND (data->>'suppressed_count')::int = 5`) != 1 ||
		f.count(t, `SELECT count(*) FROM audit_events WHERE action='runner.event.refuse' AND data->>'suppressed' = 'true'`) != 1 {
		t.Fatal("the summary and the marked row were not both written")
	}
}

// Lock order (DR-0436): RefuseRunnerEvent and a receipt for the same action take runner, run, execution, action,
// then the audit chain, so they serialize and never deadlock; exactly one of them settles the action. Run it under
// -race to also check the goroutines.
func TestPG_RefuseRunnerEventRacesTheReceipt(t *testing.T) {
	f := newActionFixture(t)
	ctx := t.Context()
	observed := time.Now().Add(-time.Minute)
	for i := range 50 {
		run := f.run(t)
		a := f.mustQueue(t, f.action(run, types.RunnerActionKill, f.issue(t, run)))
		outcome, reason := types.RunnerActionSuccess, ""
		if i%2 == 1 {
			outcome, reason = types.RunnerActionRefused, "not this runner's container"
		}
		var (
			wg             sync.WaitGroup
			recErr, refErr error
			first, settled bool
		)
		start := make(chan struct{})
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			_, first, recErr = f.st.RecordRunnerActionResult(ctx, f.rn.ID, run.ID, a.ID, outcome, reason, observed, prepareSpoolTest)
		}()
		go func() {
			defer wg.Done()
			<-start
			settled, refErr = f.st.RefuseRunnerEvent(ctx, f.rn.ID, eventRefusal(&run.ID, &a.ID), prepareSpoolTest)
		}()
		close(start)
		done := make(chan struct{})
		go func() { wg.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(20 * time.Second):
			t.Fatalf("iteration %d: the receipt and the refusal deadlocked", i)
		}
		if refErr != nil {
			t.Fatalf("iteration %d: refusal = %v (a deadlock or lock timeout lands here)", i, refErr)
		}
		if recErr != nil && !errors.Is(recErr, store.ErrConflict) {
			t.Fatalf("iteration %d: receipt = %v", i, recErr)
		}
		if first == settled {
			t.Fatalf("iteration %d: receipt first=%v, refusal settled=%v: want exactly one to settle", i, first, settled)
		}
		want := outcome
		if settled {
			want = types.RunnerActionRefused
		}
		if n := f.count(t, `SELECT count(*) FROM runner_pending_actions WHERE id=$1 AND outcome=$2`, a.ID, want); n != 1 {
			t.Fatalf("iteration %d: action outcome is not the winner's %q", i, want)
		}
		if n := f.count(t, `SELECT count(*) FROM audit_events WHERE target=$1 AND action IN ('runner.action.refuse','runner.action.apply')`, a.ID.String()); n != 1 {
			t.Fatalf("iteration %d: %d receipt rows, want one", i, n)
		}
	}
}

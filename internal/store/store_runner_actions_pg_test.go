// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

type actionFixture struct {
	pool *pgxpool.Pool
	st   store.PG
	rn   types.Runner
}

func newActionFixture(t *testing.T) actionFixture {
	t.Helper()
	pool := runsPGPoolIsolated(t)
	st := store.NewPG(pool)
	return actionFixture{pool: pool, st: st, rn: claimed(t, st, t.Context(), "action-owner")}
}

// localRun is a RUNNING local run bound to rn under createdBy, with its ref set.
func (f actionFixture) localRun(t *testing.T, rn types.Runner, createdBy string) types.AgentRun {
	t.Helper()
	run := newRun(types.RunRunning)
	run.CreatedBy, run.Placement, run.RunnerID, run.EvidenceSource = createdBy, types.PlacementLocal, &rn.ID, types.RunEvidenceRunnerAsserted
	run, err := f.st.CreateRun(t.Context(), run)
	if err != nil {
		t.Fatal(err)
	}
	run.SandboxRef = "runner:" + rn.ID.String() + "/" + run.ID.String()
	if err := f.st.SetSandboxRef(t.Context(), run.ID, run.SandboxRef); err != nil {
		t.Fatal(err)
	}
	return run
}

func (f actionFixture) run(t *testing.T) types.AgentRun { return f.localRun(t, f.rn, f.rn.Owner) }

func (f actionFixture) issue(t *testing.T, run types.AgentRun) uuid.UUID {
	t.Helper()
	gen := uuid.New()
	if _, err := f.st.BeginRunnerExecution(t.Context(), f.rn.ID, run.ID, uuid.Nil, gen, runnerwire.MethodExec); err != nil {
		t.Fatal(err)
	}
	return gen
}

func (f actionFixture) lose(t *testing.T, run types.AgentRun) types.AgentRun {
	t.Helper()
	if _, err := f.pool.Exec(t.Context(), `UPDATE agent_runs SET lost_at=now(), lost_reason='outage' WHERE id=$1`, run.ID); err != nil {
		t.Fatal(err)
	}
	got, err := f.st.GetRun(t.Context(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func (f actionFixture) action(run types.AgentRun, kind types.RunnerActionKind, gen uuid.UUID) types.RunnerPendingAction {
	a := types.RunnerPendingAction{ID: uuid.New(), RunnerID: f.rn.ID, RunID: run.ID, Kind: kind, Ref: run.SandboxRef, Generation: gen}
	if kind == types.RunnerActionStopProxy && run.LostAt != nil {
		lost := *run.LostAt
		a.LostAt = &lost
	}
	return a
}

func (f actionFixture) count(t *testing.T, q string, args ...any) int {
	t.Helper()
	var n int
	if err := f.pool.QueryRow(t.Context(), q, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func (f actionFixture) mustQueue(t *testing.T, a types.RunnerPendingAction) types.RunnerPendingAction {
	t.Helper()
	out, queued, err := f.st.QueueRunnerAction(t.Context(), a)
	if err != nil || !queued {
		t.Fatalf("queue %s: queued=%v err=%v", a.Kind, queued, err)
	}
	return out
}

func failPrepare(context.Context, types.AuditEvent) (types.AuditEvent, error) {
	return types.AuditEvent{}, errors.New("seal unavailable")
}

func TestPG_RunnerActions_QueueBindsOwnerRunRefGenerationAndLoss(t *testing.T) {
	f := newActionFixture(t)
	ctx := t.Context()
	run := f.run(t)
	gen := f.issue(t, run)
	lost := f.lose(t, f.run(t))
	sibling := claimed(t, f.st, ctx, f.rn.Owner)
	other := claimed(t, f.st, ctx, "other-owner")
	foreign := f.localRun(t, f.rn, "intruder")
	remote := f.run(t)
	if _, err := f.pool.Exec(ctx, `UPDATE agent_runs SET placement='remote' WHERE id=$1`, remote.ID); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	cases := map[string]func() types.RunnerPendingAction{
		"run created by another person": func() types.RunnerPendingAction { return f.action(foreign, types.RunnerActionKill, uuid.Nil) },
		"remote placement":              func() types.RunnerPendingAction { return f.action(remote, types.RunnerActionKill, uuid.Nil) },
		"same owner's other runner": func() types.RunnerPendingAction {
			a := f.action(run, types.RunnerActionKill, gen)
			a.RunnerID, a.Ref = sibling.ID, "runner:"+sibling.ID.String()+"/"+run.ID.String()
			return a
		},
		"another owner's runner": func() types.RunnerPendingAction {
			a := f.action(run, types.RunnerActionKill, gen)
			a.RunnerID, a.Ref = other.ID, "runner:"+other.ID.String()+"/"+run.ID.String()
			return a
		},
		"stale ref": func() types.RunnerPendingAction {
			a := f.action(run, types.RunnerActionKill, gen)
			a.Ref = "runner:" + f.rn.ID.String() + "/elsewhere"
			return a
		},
		"foreign ref prefix": func() types.RunnerPendingAction {
			a := f.action(run, types.RunnerActionKill, gen)
			a.Ref = "docker:" + run.ID.String()
			return a
		},
		"stale generation":             func() types.RunnerPendingAction { return f.action(run, types.RunnerActionKill, uuid.New()) },
		"no generation over an issued": func() types.RunnerPendingAction { return f.action(run, types.RunnerActionKill, uuid.Nil) },
		"generation before any issued": func() types.RunnerPendingAction { return f.action(lost, types.RunnerActionKill, uuid.New()) },
		"stop_proxy without loss": func() types.RunnerPendingAction {
			a := f.action(lost, types.RunnerActionStopProxy, uuid.Nil)
			a.LostAt = nil
			return a
		},
		"stop_proxy on a live run": func() types.RunnerPendingAction {
			a := f.action(run, types.RunnerActionStopProxy, gen)
			a.LostAt = &now
			return a
		},
		"stop_proxy on an older loss": func() types.RunnerPendingAction {
			a := f.action(lost, types.RunnerActionStopProxy, uuid.Nil)
			older := lost.LostAt.Add(-time.Second)
			a.LostAt = &older
			return a
		},
		"kill carrying a loss": func() types.RunnerPendingAction {
			a := f.action(lost, types.RunnerActionKill, uuid.Nil)
			a.LostAt = lost.LostAt
			return a
		},
		"quarantine without authority": func() types.RunnerPendingAction {
			a := f.action(run, types.RunnerActionQuarantine, gen)
			a.Quarantine = &types.RunnerQuarantine{ID: uuid.New(), Generation: gen}
			return a
		},
		"proof on a kill": func() types.RunnerPendingAction {
			a := f.action(run, types.RunnerActionKill, gen)
			a.Recovery = &types.RunnerMutationFailure{ID: uuid.New(), Generation: gen, Operation: runnerwire.MethodExec, Contained: true}
			return a
		},
		"unknown kind": func() types.RunnerPendingAction { return f.action(run, "freeze", gen) },
	}
	for name, mk := range cases {
		t.Run(name, func(t *testing.T) {
			if _, _, err := f.st.QueueRunnerAction(ctx, mk()); !errors.Is(err, store.ErrRunnerActionRefused) {
				t.Fatalf("queue = %v, want ErrRunnerActionRefused", err)
			}
			if n := f.count(t, `SELECT count(*) FROM runner_pending_actions`); n != 0 {
				t.Fatalf("a refused queue left %d rows", n)
			}
		})
	}
	kill := f.mustQueue(t, f.action(run, types.RunnerActionKill, gen))
	if !kill.Bound || kill.Owner != f.rn.Owner || kill.Generation != gen || kill.LostAt != nil {
		t.Fatalf("kill binding = %+v", kill)
	}
	stop := f.mustQueue(t, f.action(lost, types.RunnerActionStopProxy, uuid.Nil))
	if !stop.Bound || stop.Generation != uuid.Nil || stop.LostAt == nil || !stop.LostAt.Equal(*lost.LostAt) {
		t.Fatalf("stop_proxy binding = %+v", stop)
	}
	if _, err := f.st.BeginRunnerExecution(ctx, f.rn.ID, run.ID, gen, uuid.New(), runnerwire.MethodStart); !errors.Is(err, store.ErrRunnerExecutionRefused) {
		t.Fatalf("a new generation issued past an open action: %v", err)
	}
	if _, err := f.st.BeginRunnerExecution(ctx, f.rn.ID, run.ID, uuid.Nil, gen, runnerwire.MethodExec); !errors.Is(err, store.ErrRunnerExecutionRefused) {
		t.Fatalf("an ambiguous retry ran past an open action: %v", err)
	}
}

func TestPG_RunnerActions_QueueIdempotenceIsExact(t *testing.T) {
	f := newActionFixture(t)
	ctx := t.Context()
	run := f.run(t)
	gen := f.issue(t, run)
	a := f.action(run, types.RunnerActionKill, gen)
	first := f.mustQueue(t, a)
	if again, queued, err := f.st.QueueRunnerAction(ctx, a); err != nil || queued || again.ID != first.ID {
		t.Fatalf("an identical replay: queued=%v id=%s err=%v", queued, again.ID, err)
	}
	if again, queued, err := f.st.QueueRunnerAction(ctx, f.action(run, types.RunnerActionKill, gen)); err != nil || queued || again.ID != first.ID {
		t.Fatalf("the same open kind: queued=%v id=%s err=%v", queued, again.ID, err)
	}
	changed := a
	changed.Kind = types.RunnerActionEnd
	if _, _, err := f.st.QueueRunnerAction(ctx, changed); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("an action ID replayed with another kind = %v, want ErrConflict", err)
	}
	next := uuid.New()
	if _, err := f.pool.Exec(ctx, `UPDATE runner_executions SET generation=$2 WHERE run_id=$1`, run.ID, next); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.st.QueueRunnerAction(ctx, f.action(run, types.RunnerActionKill, next)); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("an open kill bound to another generation = %v, want ErrConflict", err)
	}
	if n := f.count(t, `SELECT count(*) FROM runner_pending_actions WHERE run_id=$1`, run.ID); n != 1 {
		t.Fatalf("%d rows, want the one exact action", n)
	}

	concurrent := f.run(t)
	var queuedN atomic.Int32
	ids := make(chan uuid.UUID, 8)
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out, queued, err := f.st.QueueRunnerAction(ctx, f.action(concurrent, types.RunnerActionEnd, uuid.Nil))
			if err != nil {
				t.Error(err)
				return
			}
			if queued {
				queuedN.Add(1)
			}
			ids <- out.ID
		}()
	}
	wg.Wait()
	close(ids)
	var want uuid.UUID
	for id := range ids {
		if want == uuid.Nil {
			want = id
		}
		if id != want {
			t.Fatal("concurrent queues returned different actions")
		}
	}
	if queuedN.Load() != 1 {
		t.Fatalf("%d concurrent queues inserted, want one", queuedN.Load())
	}
}

func TestPG_RunnerActions_ResultIsAClosedAuthenticatedCAS(t *testing.T) {
	f := newActionFixture(t)
	ctx := t.Context()
	run := f.run(t)
	gen := f.issue(t, run)
	a := f.mustQueue(t, f.action(run, types.RunnerActionKill, gen))
	unexecuted := f.run(t)
	b := f.mustQueue(t, f.action(unexecuted, types.RunnerActionKill, uuid.Nil))
	sibling := claimed(t, f.st, ctx, f.rn.Owner)
	other := claimed(t, f.st, ctx, "other-owner")
	observed := time.Now().Add(-time.Hour)
	open := func(t *testing.T, id uuid.UUID) {
		t.Helper()
		if n := f.count(t, `SELECT count(*) FROM runner_pending_actions WHERE id=$1 AND applied_at IS NULL`, id); n != 1 {
			t.Fatal("a refused result changed the action")
		}
	}
	refusals := map[string]struct {
		runner, run, action uuid.UUID
		outcome             string
		observed            time.Time
		want                error
	}{
		"outcome outside the closed set": {f.rn.ID, run.ID, a.ID, "applied", observed, store.ErrRunnerActionRefused},
		"no observed time":               {f.rn.ID, run.ID, a.ID, types.RunnerActionSuccess, time.Time{}, store.ErrRunnerActionRefused},
		"unknown action":                 {f.rn.ID, run.ID, uuid.New(), types.RunnerActionSuccess, observed, store.ErrNotFound},
		"another run's event":            {f.rn.ID, unexecuted.ID, a.ID, types.RunnerActionSuccess, observed, store.ErrRunnerActionRefused},
		"another owner's runner":         {other.ID, run.ID, a.ID, types.RunnerActionSuccess, observed, store.ErrRunnerActionRefused},
	}
	for name, c := range refusals {
		t.Run(name, func(t *testing.T) {
			if _, _, err := f.st.RecordRunnerActionResult(ctx, c.runner, c.run, c.action, c.outcome, "", c.observed, prepareSpoolTest); !errors.Is(err, c.want) {
				t.Fatalf("result = %v, want %v", err, c.want)
			}
			open(t, a.ID)
		})
	}
	// Even a run rebound to the same owner's other runner cannot let that runner settle an action queued for this one.
	if _, err := f.pool.Exec(ctx, `UPDATE agent_runs SET runner_id=$2 WHERE id=$1`, unexecuted.ID, sibling.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.st.RecordRunnerActionResult(ctx, sibling.ID, unexecuted.ID, b.ID, types.RunnerActionSuccess, "", observed, prepareSpoolTest); !errors.Is(err, store.ErrRunnerActionRefused) {
		t.Fatalf("another runner's result = %v, want ErrRunnerActionRefused", err)
	}
	open(t, b.ID)

	before := time.Now().Add(-time.Minute)
	done, first, err := f.st.RecordRunnerActionResult(ctx, f.rn.ID, run.ID, a.ID, types.RunnerActionSuccess, "", observed, prepareSpoolTest)
	if err != nil || !first || done.Outcome != types.RunnerActionSuccess || done.AppliedAt == nil || done.AppliedAt.Before(before) ||
		!done.ObservedAt.Equal(observed.UTC().Truncate(time.Microsecond)) {
		t.Fatalf("first result = %+v first=%v err=%v", done, first, err)
	}
	again, first, err := f.st.RecordRunnerActionResult(ctx, f.rn.ID, run.ID, a.ID, types.RunnerActionSuccess, "", observed, prepareSpoolTest)
	if err != nil || first || !again.AppliedAt.Equal(*done.AppliedAt) {
		t.Fatalf("an identical repeat: first=%v err=%v", first, err)
	}
	if _, _, err := f.st.RecordRunnerActionResult(ctx, f.rn.ID, run.ID, a.ID, types.RunnerActionDeferred, "", observed, prepareSpoolTest); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("a changed outcome = %v, want ErrConflict", err)
	}
	if _, _, err := f.st.RecordRunnerActionResult(ctx, f.rn.ID, run.ID, a.ID, types.RunnerActionSuccess, "", observed.Add(time.Second), prepareSpoolTest); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("a changed observed time = %v, want ErrConflict", err)
	}
}

func TestPG_RunnerActions_LegacyRowFencesButGrantsNothing(t *testing.T) {
	f := newActionFixture(t)
	ctx := t.Context()
	run := f.run(t)
	id := uuid.New()
	if _, err := f.pool.Exec(ctx, `INSERT INTO runner_pending_actions (id, runner_id, run_id, kind, ref) VALUES ($1,$2,$3,'kill',$4)`, id, f.rn.ID, run.ID, run.SandboxRef); err != nil {
		t.Fatal(err)
	}
	if has, err := f.st.RunHasPendingRunnerAction(ctx, run.ID); err != nil || !has {
		t.Fatalf("a legacy open row does not fence: %v %v", has, err)
	}
	if page, more, err := f.st.ListPendingRunnerActions(ctx, f.rn.ID); err != nil || len(page) != 0 || more {
		t.Fatalf("a legacy row was delivered: %d %v %v", len(page), more, err)
	}
	if _, _, err := f.st.RecordRunnerActionResult(ctx, f.rn.ID, run.ID, id, types.RunnerActionSuccess, "", time.Now(), prepareSpoolTest); !errors.Is(err, store.ErrRunnerActionRefused) {
		t.Fatalf("a legacy row was settled: %v", err)
	}
	for name, q := range map[string]string{
		"bound without owner":        `INSERT INTO runner_pending_actions (id, runner_id, run_id, kind, ref, binding_version) VALUES ($1,$2,$3,'end',$4,1)`,
		"bound stop_proxy, no loss":  `INSERT INTO runner_pending_actions (id, runner_id, run_id, kind, ref, binding_version, owner) VALUES ($1,$2,$3,'stop_proxy',$4,1,'o')`,
		"bound outcome 'applied'":    `INSERT INTO runner_pending_actions (id, runner_id, run_id, kind, ref, binding_version, owner, applied_at, outcome, observed_at) VALUES ($1,$2,$3,'end',$4,1,'o',now(),'applied',now())`,
		"bound quarantine, no proof": `INSERT INTO runner_pending_actions (id, runner_id, run_id, kind, ref, binding_version, owner) VALUES ($1,$2,$3,'quarantine_run',$4,1,'o')`,
		"legacy row carrying a loss": `INSERT INTO runner_pending_actions (id, runner_id, run_id, kind, ref, containment_lost_at) VALUES ($1,$2,$3,'end',$4,now())`,
	} {
		if _, err := f.pool.Exec(ctx, q, uuid.New(), f.rn.ID, run.ID, run.SandboxRef); err == nil {
			t.Errorf("%s: stored", name)
		}
	}
}

func TestPG_RunnerActions_DeferredReceiptKeepsTheFence(t *testing.T) {
	f := newActionFixture(t)
	ctx := t.Context()
	run := f.run(t)
	gen := f.issue(t, run)
	a := f.mustQueue(t, f.action(run, types.RunnerActionKill, gen))
	if _, first, err := f.st.RecordRunnerActionResult(ctx, f.rn.ID, run.ID, a.ID, types.RunnerActionDeferred, "", time.Now(), prepareSpoolTest); err != nil || !first {
		t.Fatalf("deferred: %v %v", first, err)
	}
	restarted := store.NewPG(f.pool)
	if has, err := restarted.RunHasPendingRunnerAction(ctx, run.ID); err != nil || !has {
		t.Fatalf("a deferred receipt lifted the fence: %v %v", has, err)
	}
	if _, err := restarted.BeginRunnerExecution(ctx, f.rn.ID, run.ID, gen, uuid.New(), runnerwire.MethodStart); !errors.Is(err, store.ErrRunnerExecutionRefused) {
		t.Fatalf("a generation issued past a deferred obligation: %v", err)
	}
	if page, _, err := restarted.ListPendingRunnerActions(ctx, f.rn.ID); err != nil || len(page) != 0 {
		t.Fatalf("a deferred receipt was redelivered: %d %v", len(page), err)
	}
	// Deferred is final for its action ID; convergence takes a fresh action, and
	// settling the earlier obligation is not this queue's to infer.
	fresh := f.mustQueue(t, f.action(run, types.RunnerActionKill, gen))
	if _, first, err := restarted.RecordRunnerActionResult(ctx, f.rn.ID, run.ID, fresh.ID, types.RunnerActionSuccess, "", time.Now(), prepareSpoolTest); err != nil || !first {
		t.Fatalf("fresh success: %v %v", first, err)
	}
	if has, _ := restarted.RunHasPendingRunnerAction(ctx, run.ID); !has {
		t.Fatal("a later success settled a deferred obligation without explicit authority")
	}
}

// A run's deferred kill continues in one successor per completed inventory with
// the identical binding and obligation; the successor's success settles the
// chain and lifts the fence. A run whose ref was cleared since gets none.
func TestPG_RunnerActions_DeferredKillContinuesOncePerCompletedInventory(t *testing.T) {
	f := newActionFixture(t)
	ctx := t.Context()
	run := f.run(t)
	gen := f.issue(t, run)
	kill := f.mustQueue(t, f.action(run, types.RunnerActionKill, gen))
	if _, _, err := f.st.RecordRunnerActionResult(ctx, f.rn.ID, run.ID, kill.ID, types.RunnerActionDeferred, "", time.Now(), prepareSpoolTest); err != nil {
		t.Fatal(err)
	}
	if n, err := f.st.ReissueDeferredRunKills(ctx, f.rn.ID, uuid.NewString()); err != nil || n != 0 {
		t.Fatalf("reissue before any completed inventory = %d, %v", n, err)
	}
	finalInventory(t, f.st, f.rn, uuid.NewString())
	next := uuid.NewString()
	if n, err := f.st.ReissueDeferredRunKills(ctx, f.rn.ID, next); err != nil || n != 1 {
		t.Fatalf("reissue after a completed inventory = %d, %v", n, err)
	}
	if n, err := f.st.ReissueDeferredRunKills(ctx, f.rn.ID, next); err != nil || n != 0 {
		t.Fatalf("second reissue for the same inventory = %d, %v", n, err)
	}
	page, _, err := f.st.ListPendingRunnerActions(ctx, f.rn.ID)
	if err != nil || len(page) != 1 {
		t.Fatalf("pending = %+v, %v", page, err)
	}
	successor := page[0]
	if successor.ID == kill.ID || successor.Kind != types.RunnerActionKill || successor.RunID != run.ID || successor.Ref != kill.Ref ||
		successor.Owner != kill.Owner || successor.Generation != gen || successor.ObligationID != kill.ID {
		t.Fatalf("successor = %+v, deferred kill = %+v", successor, kill)
	}
	if _, _, err := f.st.RecordRunnerActionResult(ctx, f.rn.ID, run.ID, successor.ID, types.RunnerActionSuccess, "", time.Now(), prepareSpoolTest); err != nil {
		t.Fatal(err)
	}
	if n := f.count(t, `SELECT count(*) FROM runner_pending_actions WHERE id=$1 AND settlement='chain' AND settled_by=$2`, kill.ID, successor.ID); n != 1 {
		t.Fatalf("the successor's success settled %d deferred kills, want 1", n)
	}
	if has, err := f.st.RunHasPendingRunnerAction(ctx, run.ID); err != nil || has {
		t.Fatalf("the settled chain still fences the run: %v %v", has, err)
	}

	moved := f.run(t)
	movedGen := f.issue(t, moved)
	movedKill := f.mustQueue(t, f.action(moved, types.RunnerActionKill, movedGen))
	if _, _, err := f.st.RecordRunnerActionResult(ctx, f.rn.ID, moved.ID, movedKill.ID, types.RunnerActionDeferred, "", time.Now(), prepareSpoolTest); err != nil {
		t.Fatal(err)
	}
	if err := f.st.SetSandboxRef(ctx, moved.ID, ""); err != nil {
		t.Fatal(err)
	}
	finalInventory(t, f.st, f.rn, uuid.NewString())
	if n, err := f.st.ReissueDeferredRunKills(ctx, f.rn.ID, uuid.NewString()); err != nil || n != 0 {
		t.Fatalf("reissue for a run whose ref was cleared = %d, %v", n, err)
	}
}

// A page of deferred kills that the rechecks skip (their run's ref was cleared since) stays deferred
// and unchanged; an eligible kill queued after a full page of them is still reissued.
func TestPG_RunnerActions_StaleDeferredKillsDoNotStarveLaterOnes(t *testing.T) {
	f := newActionFixture(t)
	ctx := t.Context()
	defer_ := func(run types.AgentRun) types.RunnerPendingAction {
		t.Helper()
		kill := f.mustQueue(t, f.action(run, types.RunnerActionKill, f.issue(t, run)))
		if _, _, err := f.st.RecordRunnerActionResult(ctx, f.rn.ID, run.ID, kill.ID, types.RunnerActionDeferred, "", time.Now(), prepareSpoolTest); err != nil {
			t.Fatal(err)
		}
		return kill
	}
	for range 100 { // runnerActionPageLimit
		stale := f.run(t)
		defer_(stale)
		if err := f.st.SetSandboxRef(ctx, stale.ID, ""); err != nil {
			t.Fatal(err)
		}
		// Ended, so it frees its slot on the runner for the next one.
		if _, err := f.pool.Exec(ctx, `UPDATE agent_runs SET state = 'FAILED' WHERE id = $1`, stale.ID); err != nil {
			t.Fatal(err)
		}
	}
	last := defer_(f.run(t))
	finalInventory(t, f.st, f.rn, uuid.NewString())
	if n, err := f.st.ReissueDeferredRunKills(ctx, f.rn.ID, uuid.NewString()); err != nil || n != 1 {
		t.Fatalf("reissue behind 100 stale tips = %d, %v; want 1", n, err)
	}
	if n := f.count(t, `SELECT count(*) FROM runner_pending_actions WHERE supersedes = $1`, last.ID); n != 1 {
		t.Fatalf("the eligible kill got %d successors, want 1", n)
	}
	if n := f.count(t, `SELECT count(*) FROM runner_pending_actions WHERE outcome = 'deferred' AND NOT superseded AND settled_at IS NULL`); n != 100 {
		t.Fatalf("%d stale tips left as they were, want 100: an unproved obligation is never settled to make progress", n)
	}
}

func TestPG_RunnerActions_StopProxySuccessAuditsInTheSameTransaction(t *testing.T) {
	for name, issued := range map[string]bool{"issued-execution": true, "before-first-execution": false} {
		t.Run(name, func(t *testing.T) {
			f := newActionFixture(t)
			ctx := t.Context()
			run := f.run(t)
			gen := uuid.Nil
			if issued {
				gen = f.issue(t, run)
			}
			run = f.lose(t, run)
			a := f.mustQueue(t, f.action(run, types.RunnerActionStopProxy, gen))
			observed := time.Now().Add(-time.Hour)
			audits := func() int {
				return f.count(t, `SELECT count(*) FROM audit_events WHERE run_id=$1 AND action='run.containment.reassert'`, run.ID)
			}
			if _, _, err := f.st.RecordRunnerActionResult(ctx, f.rn.ID, run.ID, a.ID, types.RunnerActionSuccess, "", observed, failPrepare); err == nil {
				t.Fatal("a success whose audit failed was recorded")
			}
			if n := f.count(t, `SELECT count(*) FROM runner_pending_actions WHERE id=$1 AND applied_at IS NULL`, a.ID); n != 1 || audits() != 0 {
				t.Fatalf("the receipt or audit escaped rollback: open=%d audits=%d", n, audits())
			}
			if _, first, err := f.st.RecordRunnerActionResult(ctx, f.rn.ID, run.ID, a.ID, types.RunnerActionSuccess, "", observed, prepareSpoolTest); err != nil || !first {
				t.Fatalf("success: %v %v", first, err)
			}
			if _, first, err := f.st.RecordRunnerActionResult(ctx, f.rn.ID, run.ID, a.ID, types.RunnerActionSuccess, "", observed, prepareSpoolTest); err != nil || first {
				t.Fatalf("duplicate: %v %v", first, err)
			}
			if audits() != 1 {
				t.Fatalf("%d late-containment audits, want exactly one", audits())
			}
			got, _ := f.st.GetRun(ctx, run.ID)
			if got.State != types.RunRunning || got.LostAt == nil || !got.LostAt.Equal(*run.LostAt) {
				t.Fatal("a containment receipt restored lifecycle")
			}
		})
	}
}

func TestPG_RunnerActions_StopProxyResultEdges(t *testing.T) {
	f := newActionFixture(t)
	ctx := t.Context()
	audits := func(run uuid.UUID) int {
		return f.count(t, `SELECT count(*) FROM audit_events WHERE run_id=$1 AND action='run.containment.reassert'`, run)
	}

	deferred := f.lose(t, f.run(t))
	d := f.mustQueue(t, f.action(deferred, types.RunnerActionStopProxy, uuid.Nil))
	if _, _, err := f.st.RecordRunnerActionResult(ctx, f.rn.ID, deferred.ID, d.ID, types.RunnerActionDeferred, "", time.Now(), failPrepare); err != nil {
		t.Fatalf("deferred ran the success hook: %v", err)
	}
	if has, _ := f.st.RunHasPendingRunnerAction(ctx, deferred.ID); !has || audits(deferred.ID) != 0 {
		t.Fatal("a deferred stop_proxy settled containment")
	}

	// No execution was issued when the action was queued; one appearing since breaks that binding.
	absent := f.lose(t, f.run(t))
	n := f.mustQueue(t, f.action(absent, types.RunnerActionStopProxy, uuid.Nil))
	if _, err := f.pool.Exec(ctx, `INSERT INTO runner_executions (run_id, runner_id, generation, operation) VALUES ($1,$2,$3,'exec')`, absent.ID, f.rn.ID, uuid.New()); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.st.RecordRunnerActionResult(ctx, f.rn.ID, absent.ID, n.ID, types.RunnerActionSuccess, "", time.Now(), prepareSpoolTest); !errors.Is(err, store.ErrRunnerActionRefused) {
		t.Fatalf("a no-execution binding settled over an issued execution: %v", err)
	}

	// The first receipt rechecks the run's ref, not only its generation.
	moved := f.run(t)
	mk := f.mustQueue(t, f.action(moved, types.RunnerActionKill, uuid.Nil))
	if _, err := f.pool.Exec(ctx, `UPDATE agent_runs SET sandbox_ref=$2 WHERE id=$1`, moved.ID, "runner:"+f.rn.ID.String()+"/elsewhere"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.st.RecordRunnerActionResult(ctx, f.rn.ID, moved.ID, mk.ID, types.RunnerActionSuccess, "", time.Now(), prepareSpoolTest); !errors.Is(err, store.ErrRunnerActionRefused) {
		t.Fatalf("a receipt for a run whose ref changed = %v, want ErrRunnerActionRefused", err)
	}
	if n := f.count(t, `SELECT count(*) FROM runner_pending_actions WHERE id=$1 AND applied_at IS NULL`, mk.ID); n != 1 {
		t.Fatal("a refused receipt changed the action")
	}

	// A run killed after its loss has its own teardown record: the exact receipt settles, with no late audit.
	killed := f.lose(t, f.run(t))
	k := f.mustQueue(t, f.action(killed, types.RunnerActionStopProxy, uuid.Nil))
	if _, err := f.pool.Exec(ctx, `UPDATE agent_runs SET state='KILLED' WHERE id=$1`, killed.ID); err != nil {
		t.Fatal(err)
	}
	if _, first, err := f.st.RecordRunnerActionResult(ctx, f.rn.ID, killed.ID, k.ID, types.RunnerActionSuccess, "", time.Now(), failPrepare); err != nil || !first {
		t.Fatalf("receipt after kill: %v %v", first, err)
	}
	if audits(killed.ID) != 0 {
		t.Fatal("late containment recorded for a run that left its kept loss")
	}
}

func TestPG_RunnerActions_PendingIsABoundedOldestFirstPage(t *testing.T) {
	f := newActionFixture(t)
	ctx := t.Context()
	var order []uuid.UUID
	for range 51 {
		run := f.run(t)
		for _, kind := range []types.RunnerActionKind{types.RunnerActionKill, types.RunnerActionEnd} {
			order = append(order, f.mustQueue(t, f.action(run, kind, uuid.Nil)).ID)
		}
	}
	page, more, err := f.st.ListPendingRunnerActions(ctx, f.rn.ID)
	if err != nil || len(page) != 100 || !more {
		t.Fatalf("page = %d more=%v err=%v; want 100 and more", len(page), more, err)
	}
	for i, a := range page {
		if a.ID != order[i] {
			t.Fatalf("page[%d] is not the %dth queued action: pending actions are not in queue order", i, i)
		}
	}
	for _, a := range page {
		if _, _, err := f.st.RecordRunnerActionResult(ctx, f.rn.ID, a.RunID, a.ID, types.RunnerActionSuccess, "", time.Now(), prepareSpoolTest); err != nil {
			t.Fatal(err)
		}
	}
	if page, more, err = f.st.ListPendingRunnerActions(ctx, f.rn.ID); err != nil || len(page) != 2 || more {
		t.Fatalf("next page = %d more=%v err=%v; want the 2 left", len(page), more, err)
	}
}

func TestPG_RunnerActions_RevokeKillsRunsAndSettlesActionsInOneCommit(t *testing.T) {
	f := newActionFixture(t)
	ctx := t.Context()
	open := f.run(t)
	kill := f.mustQueue(t, f.action(open, types.RunnerActionKill, uuid.Nil))
	deferred := f.run(t)
	end := f.mustQueue(t, f.action(deferred, types.RunnerActionEnd, uuid.Nil))
	if _, _, err := f.st.RecordRunnerActionResult(ctx, f.rn.ID, deferred.ID, end.ID, types.RunnerActionDeferred, "", time.Now(), prepareSpoolTest); err != nil {
		t.Fatal(err)
	}
	done := f.run(t)
	if _, err := f.pool.Exec(ctx, `UPDATE agent_runs SET state='COMPLETED' WHERE id=$1`, done.ID); err != nil {
		t.Fatal(err)
	}
	other := claimed(t, f.st, ctx, f.rn.Owner)
	untouched := f.localRun(t, other, other.Owner)

	// A revoke that cannot commit leaves runner, runs and actions as they were. The hold is on a pool
	// of its own: the revoke asks f.pool for a connection on this goroutine (WARDYN_TEST_POOL_GUARD).
	holder, err := pgxpool.New(ctx, f.pool.Config().ConnString())
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()
	tx, err := holder.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background()) //nolint:errcheck // released below; a no-op then
	if _, err := tx.Exec(ctx, `SELECT 1 FROM agent_runs WHERE id=$1 FOR UPDATE`, open.ID); err != nil {
		t.Fatal(err)
	}
	short, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
	if _, err := f.st.RevokeRunner(short, f.rn.ID, time.Now(), "security-admin", ""); err == nil {
		t.Fatal("a revoke blocked on a run row committed")
	}
	cancel()
	_ = tx.Rollback(ctx)
	if got, _ := f.st.GetRunner(ctx, f.rn.ID); got.State != types.RunnerClaimed {
		t.Fatal("a failed revoke changed the runner")
	}
	if has, _ := f.st.RunHasPendingRunnerAction(ctx, open.ID); !has {
		t.Fatal("a failed revoke lifted the fence")
	}

	rev, err := f.st.RevokeRunner(ctx, f.rn.ID, time.Now().UTC(), f.rn.Owner, types.RunnerRevokeNotMine)
	if err != nil || rev.Runner.State != types.RunnerRevoked || rev.Runner.RevokedAt == nil || len(rev.Killed) != 2 {
		t.Fatalf("revoke = %+v, %v; want 2 runs killed", rev, err)
	}
	if got, _ := f.st.GetRunner(ctx, f.rn.ID); got.RevokedBy != f.rn.Owner || got.RevokeReason != types.RunnerRevokeNotMine {
		t.Fatalf("revoked by %q for %q, want the owner's not_mine", got.RevokedBy, got.RevokeReason)
	}
	for _, k := range rev.Killed {
		if k.From != types.RunRunning {
			t.Fatalf("killed run %s reported from %q, want RUNNING", k.ID, k.From)
		}
	}
	for _, run := range []types.AgentRun{open, deferred} {
		got, _ := f.st.GetRun(ctx, run.ID)
		if got.State != types.RunKilled || got.EndedAt == nil {
			t.Fatalf("run %s on the revoked runner is %s, want KILLED", run.ID, got.State)
		}
		if has, _ := f.st.RunHasPendingRunnerAction(ctx, run.ID); has {
			t.Fatal("a killed run on a revoked runner is still fenced")
		}
	}
	if got, _ := f.st.GetRun(ctx, done.ID); got.State != types.RunCompleted {
		t.Fatal("revocation rewrote a terminal run")
	}
	if got, _ := f.st.GetRun(ctx, untouched.ID); got.State != types.RunRunning {
		t.Fatal("revocation killed another runner's run")
	}
	if n := f.count(t, `SELECT count(*) FROM runner_pending_actions WHERE id = ANY($1) AND settlement='revoked' AND settled_at IS NOT NULL`, []uuid.UUID{kill.ID, end.ID}); n != 2 {
		t.Fatalf("%d of 2 actions settled 'revoked' (rows must be kept)", n)
	}
	if n := f.count(t, `SELECT count(*) FROM runner_pending_actions WHERE id=$1 AND outcome='deferred'`, end.ID); n != 1 {
		t.Fatal("settlement rewrote the deferred receipt")
	}
	if _, err := f.pool.Exec(ctx, `UPDATE runner_pending_actions SET settlement='' , settled_at=NULL WHERE id=$1`, kill.ID); err == nil {
		t.Fatal("a settlement was cleared")
	}
	if acts, _, err := f.st.ListPendingRunnerActions(ctx, f.rn.ID); err != nil || len(acts) != 0 {
		t.Fatalf("a revoked runner still has %d deliverable actions (%v)", len(acts), err)
	}
	if _, err := f.st.RevokeRunner(ctx, f.rn.ID, time.Now(), "security-admin", ""); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("a second revoke = %v, want ErrNotFound", err)
	}
	if _, err := f.st.ClaimRunner(ctx, f.rn.ID, f.rn.Owner, f.rn.KeyFingerprint, time.Now().UTC()); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("claiming a revoked runner = %v, want ErrConflict", err)
	}
	if got, _ := f.st.GetRunner(ctx, f.rn.ID); got.State != types.RunnerRevoked || got.RevokedAt == nil {
		t.Fatalf("after a refused claim the runner is %s, want still revoked", got.State)
	}
	if _, _, err := f.st.QueueRunnerAction(ctx, f.action(open, types.RunnerActionEnd, uuid.Nil)); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("queueing for a revoked runner = %v, want ErrNotFound", err)
	}
	unclaimed, _ := f.st.CreateRunner(ctx, newRunner(f.rn.Owner))
	a := f.action(untouched, types.RunnerActionKill, uuid.Nil)
	a.RunnerID, a.Ref = unclaimed.ID, "runner:"+unclaimed.ID.String()+"/x"
	if _, _, err := f.st.QueueRunnerAction(ctx, a); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("queueing for an unclaimed runner = %v, want ErrNotFound", err)
	}
}

// A queue racing a revoke either commits first and is settled by it, or waits
// on the runner row and then refuses: no unsettled row survives on a revoked runner.
func TestPG_RunnerActions_QueueAndRevokeSerialize(t *testing.T) {
	f := newActionFixture(t)
	ctx := t.Context()
	for range 20 {
		rn := claimed(t, f.st, ctx, f.rn.Owner)
		run := f.localRun(t, rn, rn.Owner)
		a := types.RunnerPendingAction{ID: uuid.New(), RunnerID: rn.ID, RunID: run.ID, Kind: types.RunnerActionKill, Ref: run.SandboxRef}
		var wg sync.WaitGroup
		var qerr, rerr error
		wg.Add(2)
		go func() { defer wg.Done(); _, _, qerr = f.st.QueueRunnerAction(ctx, a) }()
		go func() { defer wg.Done(); _, rerr = f.st.RevokeRunner(ctx, rn.ID, time.Now(), "security-admin", "") }()
		wg.Wait()
		if rerr != nil || (qerr != nil && !errors.Is(qerr, store.ErrNotFound)) {
			t.Fatalf("revoke=%v queue=%v", rerr, qerr)
		}
		if n := f.count(t, `SELECT count(*) FROM runner_pending_actions WHERE runner_id=$1 AND applied_at IS NULL AND settled_at IS NULL`, rn.ID); n != 0 {
			t.Fatalf("%d unsettled open actions survived revocation", n)
		}
	}
}

// A queue that meets a revoke in flight waits for it (FOR SHARE on the runner row) and then
// refuses, instead of inserting a row the revoke's DELETE had already passed.
func TestPG_RunnerActions_QueueWaitsForAnInFlightRevoke(t *testing.T) {
	f := newActionFixture(t)
	ctx := t.Context()
	run := f.run(t)
	tx, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background()) //nolint:errcheck // a no-op after Commit
	if _, err := tx.Exec(ctx, `UPDATE runners SET state='revoked', revoked_at=now() WHERE id=$1`, f.rn.ID); err != nil {
		t.Fatal(err)
	}
	errc := make(chan error, 1)
	go func() {
		_, _, err := f.st.QueueRunnerAction(ctx, f.action(run, types.RunnerActionKill, uuid.Nil))
		errc <- err
	}()
	select {
	case err := <-errc:
		t.Fatalf("the queue did not wait for the revoke in flight (returned %v)", err)
	case <-time.After(300 * time.Millisecond):
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-errc; !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("queue after the revoke committed = %v, want ErrNotFound", err)
	}
	if has, _ := f.st.RunHasPendingRunnerAction(ctx, run.ID); has {
		t.Fatal("a row survived the revoke")
	}
}

func TestPG_RunnerActions_CascadeWithRun(t *testing.T) {
	f := newActionFixture(t)
	ctx := t.Context()
	run := f.run(t)
	f.mustQueue(t, f.action(run, types.RunnerActionEnd, uuid.Nil))
	if _, err := f.pool.Exec(ctx, `DELETE FROM agent_runs WHERE id=$1`, run.ID); err != nil {
		t.Fatal(err)
	}
	if acts, _, _ := f.st.ListPendingRunnerActions(ctx, f.rn.ID); len(acts) != 0 {
		t.Fatalf("%d actions outlived their run", len(acts))
	}
}

// A deferred stop_proxy keeps carrying its loss's obligation: neither a later
// queue nor a reconnect page that still sees the proxy up issues a second one.
func TestPG_RunnerActions_DeferredStopProxyIsNeverReissued(t *testing.T) {
	f := newActionFixture(t)
	ctx := t.Context()
	run := f.run(t)
	gen := f.issue(t, run)
	run = f.lose(t, run)
	first := f.mustQueue(t, f.action(run, types.RunnerActionStopProxy, gen))
	if _, _, err := f.st.RecordRunnerActionResult(ctx, f.rn.ID, run.ID, first.ID, types.RunnerActionDeferred, "", time.Now(), prepareSpoolTest); err != nil {
		t.Fatal(err)
	}
	again, queued, err := f.st.QueueRunnerAction(ctx, f.action(run, types.RunnerActionStopProxy, gen))
	if err != nil || queued || again.ID != first.ID {
		t.Fatalf("a fresh stop_proxy for the deferred loss: queued=%v id=%s err=%v", queued, again.ID, err)
	}
	row := runnerwire.RunState{RunID: run.ID, SandboxPresent: true, Execution: &runnerwire.ExecutionResult{Generation: gen, Operation: runnerwire.MethodExec, Attempted: true}}
	for range 3 {
		if res, err := finalPage(t, f.st, f.rn.ID, row); err != nil || !res.NeedsReconciliation {
			t.Fatalf("a page with the proxy still up: %+v %v", res, err)
		}
	}
	if n := f.count(t, `SELECT count(*) FROM runner_pending_actions WHERE run_id=$1 AND kind='stop_proxy'`, run.ID); n != 1 {
		t.Fatalf("%d stop_proxy rows for one loss, want the one deferred", n)
	}
	if has, _ := f.st.RunHasPendingRunnerAction(ctx, run.ID); !has {
		t.Fatal("the deferred obligation stopped fencing")
	}
}

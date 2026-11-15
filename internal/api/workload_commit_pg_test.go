// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// ambiguousRecord is the store whose record of a workload's new run commits and then answers an error, as a
// commit whose answer was lost does: the record's transaction returns its commit's own error.
type ambiguousRecord struct{ store.PG }

func (s ambiguousRecord) SetWorkloadRun(ctx context.Context, id, claim uuid.UUID, generation int64, runID uuid.UUID) (*uuid.UUID, error) {
	if _, err := s.PG.SetWorkloadRun(ctx, id, claim, generation, runID); err != nil {
		return nil, err
	}
	return nil, errors.New("unexpected EOF")
}

// unreadableRecord is ambiguousRecord whose read of the record's outcome fails too, as does the sweep's.
type unreadableRecord struct{ ambiguousRecord }

func (unreadableRecord) WorkloadHoldsRun(context.Context, string, string, uuid.UUID) (bool, bool, error) {
	return false, false, errors.New("connection refused")
}

func (unreadableRecord) DropWorkloadRunEndIfHeld(context.Context, string, string, uuid.UUID) (bool, error) {
	return false, errors.New("connection refused")
}

// lostRecord is the store whose record of a workload's new run writes nothing and answers an error, as a
// commit that never reached the database does.
type lostRecord struct{ store.PG }

func (lostRecord) SetWorkloadRun(context.Context, uuid.UUID, uuid.UUID, int64, uuid.UUID) (*uuid.UUID, error) {
	return nil, errors.New("unexpected EOF")
}

// replacedDuringReadBack is ambiguousRecord whose record, once it lands, is replaced before it answers by a
// later launch's record that defers the new run's end to after, a queued run: the interleaving a read-back
// can meet while it waits.
type replacedDuringReadBack struct {
	store.PG
	after uuid.UUID
}

func (s replacedDuringReadBack) SetWorkloadRun(ctx context.Context, id, claim uuid.UUID, generation int64, runID uuid.UUID) (*uuid.UUID, error) {
	if _, err := s.PG.SetWorkloadRun(ctx, id, claim, generation, runID); err != nil {
		return nil, err
	}
	if _, err := s.PG.Pool.Exec(ctx, `
		WITH w AS (UPDATE workloads SET current_run_id = $3 WHERE id = $1 RETURNING owner, name, generation)
		INSERT INTO workload_run_ends (run_id, owner, name, generation, reason, after_run_id)
		SELECT $2, owner, name, generation, 'workload_replaced', $3 FROM w`, id, runID, s.after); err != nil {
		return nil, err
	}
	return nil, errors.New("unexpected EOF")
}

// agentRunIDs is every run id stored now.
func agentRunIDs(t *testing.T, e svcEnv) map[uuid.UUID]bool {
	t.Helper()
	rows, err := e.pool.Query(context.Background(), `SELECT id FROM agent_runs`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	ids := map[uuid.UUID]bool{}
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids[id] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return ids
}

// newAgentRun is the one run stored since before was read.
func newAgentRun(t *testing.T, e svcEnv, before map[uuid.UUID]bool) uuid.UUID {
	t.Helper()
	var fresh []uuid.UUID
	for id := range agentRunIDs(t, e) {
		if !before[id] {
			fresh = append(fresh, id)
		}
	}
	if len(fresh) != 1 {
		t.Fatalf("runs stored since = %v, want one", fresh)
	}
	return fresh[0]
}

// ageOwedEnds makes every owed end older than the sweep's grace.
func ageOwedEnds(t *testing.T, e svcEnv) {
	t.Helper()
	if _, err := e.pool.Exec(context.Background(), `UPDATE workload_run_ends SET created_at = now() - interval '1 hour'`); err != nil {
		t.Fatal(err)
	}
}

// wantRunLive fails unless runID's run has not ended.
func wantRunLive(t *testing.T, e svcEnv, runID uuid.UUID, what string) {
	t.Helper()
	if run, err := e.st.GetRun(context.Background(), runID); err != nil || isTerminalRunState(run.State) {
		t.Fatalf("%s %s = %q (%v), want it not ended", what, runID, run.State, err)
	}
}

// A record of a workload's new run that lands and then answers an error (the caller went away during the
// commit, or the connection dropped) keeps the run: it is the workload's, it keeps running and the apply
// answers that it launched. The run it replaced was owed its end in the record, and the sweep ends that one.
func TestPG_Workload_ARecordThatLandedKeepsItsRunThoughItAnsweredAnError(t *testing.T) {
	e := newSvcEnv(t)
	ctx := context.Background()
	pol := e.plainPolicy(t, "wl-pol")
	oldRun := launchedRun(t, decodeApply(t, doSSO(t, e.h.srv, http.MethodPut, "/api/v1/workloads/web", e.mem, workloadBody(pol, "t", "")), http.StatusCreated))
	e.h.srv.cfg.Store = ambiguousRecord{PG: e.st}
	newRun := launchedRun(t, decodeApply(t, doSSO(t, e.h.srv, http.MethodPut, "/api/v1/workloads/web", e.mem, workloadBody(pol, "t2", "")), http.StatusOK))
	e.h.srv.cfg.Store = e.st
	if v, err := e.st.GetWorkload(ctx, "mem", "web"); err != nil || v.CurrentRunID == nil || *v.CurrentRunID != newRun {
		t.Fatalf("workload = %+v (%v), want its current run %s", v.Workload, err, newRun)
	}
	wantRunLive(t, e, newRun, "the workload's run")
	wantRunLive(t, e, oldRun, "the replaced run before the sweep")
	if n := owedEnds(t, e, oldRun); n != 1 {
		t.Fatalf("owed ends of the replaced run = %d, want the one the record wrote", n)
	}
	ageOwedEnds(t, e)
	if err := e.h.srv.sweepWorkloadRunEnds(ctx); err != nil {
		t.Fatal(err)
	}
	wantKilledFor(t, e, oldRun, workloadKillReplaced)
	wantRunLive(t, e, newRun, "the workload's run after the sweep")
}

// A record that answers an error and whose outcome cannot be read back either ends nothing: its run is owed
// its end. A sweep that cannot read whether the workload holds it keeps it running and owed; the next sweep,
// finding that the workload holds it, drops that and keeps it running while it ends the run the record
// replaced.
func TestPG_Workload_ARecordThatCannotBeReadBackOwesItsRunAndTheSweepKeepsIt(t *testing.T) {
	e := newSvcEnv(t)
	ctx := context.Background()
	pol := e.plainPolicy(t, "wl-pol")
	oldRun := launchedRun(t, decodeApply(t, doSSO(t, e.h.srv, http.MethodPut, "/api/v1/workloads/web", e.mem, workloadBody(pol, "t", "")), http.StatusCreated))
	e.h.srv.cfg.Store = unreadableRecord{ambiguousRecord{PG: e.st}}
	if w := doSSO(t, e.h.srv, http.MethodPut, "/api/v1/workloads/web", e.mem, workloadBody(pol, "t2", "")); w.Code != http.StatusInternalServerError {
		t.Fatalf("apply: %d %s, want 500: the record's outcome is unknown", w.Code, w.Body.String())
	}
	e.h.srv.cfg.Store = e.st
	v, err := e.st.GetWorkload(ctx, "mem", "web")
	if err != nil || v.CurrentRunID == nil || *v.CurrentRunID == oldRun {
		t.Fatalf("workload = %+v (%v), want the record's new run current", v.Workload, err)
	}
	newRun := *v.CurrentRunID
	wantRunLive(t, e, newRun, "the workload's run")
	if n := owedEnds(t, e, newRun); n != 1 {
		t.Fatalf("owed ends of the new run = %d, want 1: whether the workload took it was not known", n)
	}
	ageOwedEnds(t, e)
	e.h.srv.cfg.Store = unreadableRecord{ambiguousRecord{PG: e.st}}
	if err := e.h.srv.sweepWorkloadRunEnds(ctx); err != nil {
		t.Fatal(err)
	}
	e.h.srv.cfg.Store = e.st
	wantRunLive(t, e, newRun, "the workload's run after a sweep that could not read whether it is held")
	if n := owedEnds(t, e, newRun); n != 1 {
		t.Fatalf("owed ends of the new run after a sweep that could not read it = %d, want it still owed", n)
	}
	if err := e.h.srv.sweepWorkloadRunEnds(ctx); err != nil {
		t.Fatal(err)
	}
	wantKilledFor(t, e, oldRun, workloadKillReplaced)
	wantRunLive(t, e, newRun, "the workload's run after the sweep")
	if n := owedEnds(t, e, newRun); n != 0 {
		t.Fatalf("owed ends of the workload's run after the sweep = %d, want 0", n)
	}
}

// A record that landed and answered an error, and that a later launch replaced with a queued run before the
// read-back, keeps its run: the run's end waits on that queued run, and the sweep settles it. The apply answers
// that it launched.
func TestPG_Workload_ARecordReplacedBeforeItsReadBackLeavesItsEndToTheQueuedRun(t *testing.T) {
	e := newSvcEnv(t)
	pol := e.plainPolicy(t, "wl-pol")
	launchedRun(t, decodeApply(t, doSSO(t, e.h.srv, http.MethodPut, "/api/v1/workloads/web", e.mem, workloadBody(pol, "t", "")), http.StatusCreated))
	other := launchedRun(t, decodeApply(t, doSSO(t, e.h.srv, http.MethodPut, "/api/v1/workloads/other", e.mem, workloadBody(pol, "t", "")), http.StatusCreated))
	e.h.srv.cfg.Store = replacedDuringReadBack{PG: e.st, after: other}
	newRun := launchedRun(t, decodeApply(t, doSSO(t, e.h.srv, http.MethodPut, "/api/v1/workloads/web", e.mem, workloadBody(pol, "t2", "")), http.StatusOK))
	e.h.srv.cfg.Store = e.st
	wantRunLive(t, e, newRun, "the replaced run whose replacement has not started")
	var after *uuid.UUID
	if err := e.pool.QueryRow(context.Background(), `SELECT after_run_id FROM workload_run_ends WHERE run_id = $1`, newRun).Scan(&after); err != nil ||
		after == nil || *after != other {
		t.Fatalf("the replaced run's owed end waits on %v (%v), want %s", after, err, other)
	}
}

// A record that answers an error and did not land ends the run it was for, as a lost record does: the new run
// is killed workload_replaced and the workload keeps its running run.
func TestPG_Workload_ARecordThatDidNotLandEndsItsRun(t *testing.T) {
	e := newSvcEnv(t)
	ctx := context.Background()
	pol := e.plainPolicy(t, "wl-pol")
	oldRun := launchedRun(t, decodeApply(t, doSSO(t, e.h.srv, http.MethodPut, "/api/v1/workloads/web", e.mem, workloadBody(pol, "t", "")), http.StatusCreated))
	before := agentRunIDs(t, e)
	e.h.srv.cfg.Store = lostRecord{PG: e.st}
	if w := doSSO(t, e.h.srv, http.MethodPut, "/api/v1/workloads/web", e.mem, workloadBody(pol, "t2", "")); w.Code != http.StatusInternalServerError {
		t.Fatalf("apply: %d %s, want 500", w.Code, w.Body.String())
	}
	e.h.srv.cfg.Store = e.st
	wantKilledFor(t, e, newAgentRun(t, e, before), workloadKillReplaced)
	if v, err := e.st.GetWorkload(ctx, "mem", "web"); err != nil || v.CurrentRunID == nil || *v.CurrentRunID != oldRun {
		t.Fatalf("workload = %+v (%v), want run %s kept", v.Workload, err, oldRun)
	}
	wantRunLive(t, e, oldRun, "the workload's run")
}

// boundLaunch is the env's binding of team-a, its service and the request a launch under that binding acts as.
func (e crEnv) boundLaunch(t *testing.T) (store.WorkloadBinding, types.ServiceIdentity, *http.Request) {
	t.Helper()
	ctx := context.Background()
	b, err := e.st.BindingForRunner(ctx, e.runner, "team-a")
	if err != nil {
		t.Fatal(err)
	}
	si, err := e.h.srv.serviceRecord(ctx, svcName)
	if err != nil {
		t.Fatal(err)
	}
	sr, cancel, err := serviceLaunchRequest(si, b.Sponsor)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cancel)
	return b, si, sr
}

// A custom resource's launch that waited in the background past the delete of the namespace binding it found
// (a report's launch, the lapsed-launch sweep's retry) starts no run: it is refused workload_binding_inactive
// with the binding-gone sentence, and the workload's running run keeps running.
func TestPG_WorkloadFromRunner_ALaunchThatWaitedPastItsBindingsDeleteStartsNoRun(t *testing.T) {
	e := newCREnv(t)
	_, run := e.k8sWorkloadWithRun(t, "web", "uid-web")
	e.placeRunsOnTheRunner(t)
	wl := e.putK8sWorkload(t, "web", "uid-web", "t2") // a changed spec owes a launch
	b, si, sr := e.boundLaunch(t)
	if w := doSSO(t, e.h.srv, http.MethodDelete, "/api/v1/workload-bindings/"+b.ID.String(), e.sec, ""); w.Code != http.StatusNoContent {
		t.Fatalf("delete the binding: %d %s, want 204", w.Code, w.Body.String())
	}
	before := agentRunIDs(t, e.svcEnv)
	launch, err := e.h.srv.launchUnderBinding(sr, e.st, b, true, si, wl)
	if err != nil || launch.State != client.WorkloadRefused || launch.Reason != reasonWorkloadBindingInactive ||
		launch.Message != workloadBindingGoneMsg || launch.RunID != nil {
		t.Fatalf("launch = %+v (%v), want refused %s with the binding-gone sentence and no run", launch, err, reasonWorkloadBindingInactive)
	}
	if after := agentRunIDs(t, e.svcEnv); len(after) != len(before) {
		t.Fatalf("runs stored = %d after the launch, want %d: it started a run", len(after), len(before))
	}
	v := e.wlRow(t, "web")
	if v.CurrentRunID == nil || *v.CurrentRunID != run || v.LastRefusal == nil || v.LastRefusal.Reason != reasonWorkloadBindingInactive ||
		v.LastRefusal.Message != workloadBindingGoneMsg {
		t.Fatalf("workload = %+v, refusal %+v; want run %s kept and the refusal recorded", v.Workload, v.LastRefusal, run)
	}
	wantRunLive(t, e.svcEnv, run, "the workload's run")
}

// bindingGoneAtCreate is the store that deletes the namespace binding binding just after a launch's run is
// created: a delete that lands after the launch read its binding and before the launch's record.
type bindingGoneAtCreate struct {
	store.PG
	binding uuid.UUID
}

func (s bindingGoneAtCreate) CreateRun(ctx context.Context, r types.AgentRun) (types.AgentRun, error) {
	created, err := s.PG.CreateRun(ctx, r)
	if _, derr := s.PG.DeleteWorkloadBinding(ctx, s.binding); derr != nil && err == nil {
		err = derr
	}
	return created, err
}

// A namespace binding deleted while a launch under it is starting its run, after the launch read the binding
// and before its record, leaves that run unrecorded: it is ended (run.kill workload_binding_deleted), the
// launch is refused workload_binding_inactive with the binding-gone sentence, and the workload's running run
// keeps running.
func TestPG_WorkloadFromRunner_ABindingDeletedWhileItsRunStartsRecordsNoRun(t *testing.T) {
	e := newCREnv(t)
	ctx := context.Background()
	_, run := e.k8sWorkloadWithRun(t, "web", "uid-web")
	e.placeRunsOnTheRunner(t)
	wl := e.putK8sWorkload(t, "web", "uid-web", "t2") // a changed spec owes a launch
	b, si, sr := e.boundLaunch(t)
	before := agentRunIDs(t, e.svcEnv)
	gone := bindingGoneAtCreate{PG: e.st, binding: b.ID}
	e.h.srv.cfg.Store = gone
	launch, err := e.h.srv.launchUnderBinding(sr, gone, b, true, si, wl)
	e.h.srv.cfg.Store = e.st
	if left, err := e.st.ListWorkloadBindings(ctx, store.Page{}); err != nil || len(left) != 0 {
		t.Fatalf("bindings = %+v (%v), want none: the run was not created through CreateRun, so the binding was not deleted", left, err)
	}
	if err != nil || launch.State != client.WorkloadRefused || launch.Reason != reasonWorkloadBindingInactive || launch.Message != workloadBindingGoneMsg {
		t.Fatalf("launch = %+v (%v), want refused %s with the binding-gone sentence", launch, err, reasonWorkloadBindingInactive)
	}
	ended := newAgentRun(t, e.svcEnv, before)
	wantKilledFor(t, e.svcEnv, ended, workloadKillBindingDeleted)
	// endUntakenWorkloadRun only logs a refused owe, so the kill passes whatever the reason's CHECK admits:
	// the owed end's own write is what needs migration 0280, and the CHECK still refuses any other reason.
	owe := func(reason string) error {
		return e.st.OweWorkloadRunEnd(ctx, store.WorkloadRunEnd{RunID: ended, Owner: wl.Owner, Name: wl.Name, Generation: wl.Generation, Reason: reason})
	}
	var pgErr *pgconn.PgError
	if err := owe("not_a_reason"); !errors.As(err, &pgErr) || pgErr.Code != "23514" {
		t.Fatalf("owe an end with an unknown reason = %v, want the CHECK's refusal (23514)", err)
	}
	if err := owe(workloadKillBindingDeleted); err != nil {
		t.Fatalf("owe an end with %s = %v, want migration 0280 to admit it", workloadKillBindingDeleted, err)
	}
	v := e.wlRow(t, "web")
	if v.CurrentRunID == nil || *v.CurrentRunID != run || v.LastRefusal == nil || v.LastRefusal.Reason != reasonWorkloadBindingInactive {
		t.Fatalf("workload = %+v, refusal %+v; want run %s kept and refused %s", v.Workload, v.LastRefusal, run, reasonWorkloadBindingInactive)
	}
	wantRunLive(t, e.svcEnv, run, "the workload's run")
}

// A custom resource's launch that waits in its pool's queue stores the namespace binding it was authorised
// under in its queued run's authority, the binding its start then holds to (StartQueuedRun).
func TestPG_WorkloadFromRunner_AQueuedLaunchStoresItsBindingInItsAuthority(t *testing.T) {
	e := newCREnv(t)
	ctx := context.Background()
	e.k8sWorkloadWithRun(t, "web", "uid-web")
	e.placeRunsOnTheRunner(t)
	wl := e.putK8sWorkload(t, "web", "uid-web", "t2") // a changed spec owes a launch
	// The pool's one runner takes one run at once and already holds one, so the launch waits in the queue.
	if _, err := e.pool.Exec(ctx, `UPDATE runners SET max_runs = 1 WHERE id = $1`, e.runner); err != nil {
		t.Fatal(err)
	}
	rp, err := e.st.GetRunnerPool(ctx, e.rp.ID)
	if err != nil {
		t.Fatal(err)
	}
	id, now := uuid.New(), time.Now().UTC()
	if _, err := e.st.CreateRun(ctx, types.AgentRun{ID: id, CreatedAt: now, UpdatedAt: now, CreatedBy: "occupant", Agent: "claude-code",
		ConfinementClass: types.CC2, State: types.RunRunning, SPIFFEID: "spiffe://wardyn.local/agent-run/" + id.String(), RunnerTarget: "docker",
		RunnerID: &e.runner, RunnerPoolID: &rp.ID, RunnerPoolRevision: rp.Revision, RunnerPoolSelection: types.RunnerPoolSelectedExplicit}); err != nil {
		t.Fatalf("occupy the runner: %v", err)
	}
	b, si, sr := e.boundLaunch(t)
	launch, err := e.h.srv.launchUnderBinding(sr, e.st, b, true, si, wl)
	if err != nil || launch.State != client.WorkloadLaunched || launch.RunID == nil {
		t.Fatalf("launch = %+v (%v), want launched", launch, err)
	}
	var binding string
	if err := e.pool.QueryRow(ctx, `SELECT COALESCE(authority->>'binding', '') FROM run_queue WHERE run_id = $1`, *launch.RunID).Scan(&binding); err != nil {
		t.Fatalf("the launched run's queue row: %v; want the run waiting in its pool's queue", err)
	}
	if binding != b.ID.String() {
		t.Fatalf("the queued run's stored binding = %q, want %s", binding, b.ID)
	}
}

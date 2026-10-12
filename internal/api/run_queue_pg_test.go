// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// The pool run queue against a real Postgres (docs/design/0.9/pool-queue.md): a launch into a pool at
// its concurrent-run cap waits holding nothing, starts in launch order once its owner's authority is
// re-checked, and ends through distinct recorded reasons. Guarded by WARDYN_TEST_PG: skipped cleanly
// when unset, must PASS when set.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runnerpool"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

type queueEnv struct{ *placementEnv }

func newQueueEnv(t *testing.T) *queueEnv { return &queueEnv{newPlacementEnv(t)} }

// cappedPool is a remote-provided pool on executor alpha that runs at most max runs at once.
func (e *queueEnv) cappedPool(name string, max int) types.RunnerPool {
	e.t.Helper()
	body := fmt.Sprintf(`{"name":%q,"hosting_type":"remote_provided","limits":{"background":{"cpu_millis":{"default":500,"cap":4000},`+
		`"memory_mib":{"default":512,"cap":8192},"lifetime":{"default_sec":3600,"max_sec":7200}},"barriers":["CC1","CC2","CC3"],"max_concurrent_runs":%d}}`, name, max)
	w := e.admin(http.MethodPost, "/api/v1/runner-pools", body)
	if w.Code != http.StatusCreated {
		e.t.Fatalf("create pool = %d %s", w.Code, w.Body)
	}
	p := decodeJSON[types.RunnerPool](e.t, w)
	e.addExecutors(p.ID, "alpha")
	return e.reread(p)
}

func (e *queueEnv) reread(p types.RunnerPool) types.RunnerPool {
	e.t.Helper()
	got, err := e.pg.GetRunnerPool(context.Background(), p.ID)
	if err != nil {
		e.t.Fatal(err)
	}
	return got
}

// occupy puts a RUNNING run in the pool: it holds a slot until free is called, with no dispatch to
// race the test.
func (e *queueEnv) occupy(p types.RunnerPool) types.AgentRun {
	e.t.Helper()
	p = e.reread(p) // members added since the pool was read have moved its revision
	id := uuid.New()
	now := time.Now().UTC()
	r, err := e.pg.CreateRun(context.Background(), types.AgentRun{
		ID: id, CreatedAt: now, UpdatedAt: now, CreatedBy: "occupant", Agent: "claude-code", ConfinementClass: types.CC2,
		State: types.RunRunning, SPIFFEID: "spiffe://wardyn.local/agent-run/" + id.String(), RunnerTarget: "docker",
		RunnerPoolID: &p.ID, RunnerPoolRevision: p.Revision, RunnerPoolSelection: types.RunnerPoolSelectedExplicit,
	})
	if err != nil {
		e.t.Fatalf("occupy: %v", err)
	}
	return r
}

// free ends an occupant the way the server ends any run, so the slot-freeing kick fires.
func (e *queueEnv) free(r types.AgentRun) {
	e.t.Helper()
	if ok, err := e.srv.casRunState(context.Background(), r.ID, types.RunRunning, types.RunCompleted); err != nil || !ok {
		e.t.Fatalf("free: %v %v", ok, err)
	}
}

type queuedLaunchAnswer struct {
	types.AgentRun
	Warnings []string `json:"warnings"`
}

// launch posts a Background run into p as c (nil: the admin token) and returns the answer.
func (e *queueEnv) launch(c *http.Cookie, p types.RunnerPool, extra string) (queuedLaunchAnswer, *httptest.ResponseRecorder) {
	e.t.Helper()
	body := endBody("background", `"runner_pool_id":"`+p.ID.String()+`"`+extra)
	var w *httptest.ResponseRecorder
	if c == nil {
		w = e.admin(http.MethodPost, "/api/v1/runs", body)
	} else {
		w = e.call(c, http.MethodPost, "/api/v1/runs", body)
	}
	var out queuedLaunchAnswer
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return out, w
}

func (e *queueEnv) queuedLaunch(c *http.Cookie, p types.RunnerPool, extra string) queuedLaunchAnswer {
	e.t.Helper()
	out, w := e.launch(c, p, extra)
	if w.Code != http.StatusCreated || out.State != types.RunQueued {
		e.t.Fatalf("launch into a full pool = %d %s; want 201 QUEUED", w.Code, w.Body)
	}
	return out
}

func (e *queueEnv) run(id uuid.UUID) types.AgentRun {
	e.t.Helper()
	r, err := e.pg.GetRun(context.Background(), id)
	if err != nil {
		e.t.Fatal(err)
	}
	return r
}

// eventually polls cond for up to 20 seconds: a promotion's start runs detached.
func (e *queueEnv) eventually(what string, cond func() bool) {
	e.t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			e.t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// waitStarted waits for a promoted run to be started: minted for, written and handed to dispatch.
func (e *queueEnv) waitStarted(id uuid.UUID) types.AgentRun {
	e.t.Helper()
	e.eventually("run "+id.String()+" to start", func() bool {
		r := e.run(id)
		return r.AdmittedAt != nil && r.SPIFFEID != "" && r.State != types.RunPending
	})
	return e.run(id)
}

func (e *queueEnv) waitFailed(id uuid.UUID) types.AgentRun {
	e.t.Helper()
	e.eventually("run "+id.String()+" to fail", func() bool {
		r := e.run(id)
		return r.State == types.RunFailed && r.StatusDetail != "" && r.FailureHint != ""
	})
	return e.run(id)
}

func (e *queueEnv) queueRows(id uuid.UUID) int {
	return e.count(`SELECT count(*) FROM run_queue WHERE run_id = $1`, id)
}

func (e *queueEnv) sweep() {
	e.t.Helper()
	if err := e.srv.SweepRunQueue(context.Background()); err != nil {
		e.t.Fatal(err)
	}
}

func (e *queueEnv) audits(runID uuid.UUID, action string) []types.AuditEvent {
	var out []types.AuditEvent
	for _, ev := range e.h.audit.snapshot() {
		if ev.RunID != nil && *ev.RunID == runID && ev.Action == action {
			out = append(out, ev)
		}
	}
	return out
}

// waitAudit waits for the detached start to have written its audit rows.
func (e *queueEnv) waitAudit(runID uuid.UUID, action string) []types.AuditEvent {
	e.t.Helper()
	e.eventually(action+" rows for "+runID.String(), func() bool { return len(e.audits(runID, action)) > 0 })
	return e.audits(runID, action)
}

func queueAuditData(t *testing.T, ev types.AuditEvent) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(ev.Data, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

// A launch into a pool at its cap is stored QUEUED, answers 201 with the wait sentence, and holds
// nothing: no identity, no grants, no proxy config, no renewal. When a slot frees it is promoted in
// launch order, minted for, and starts with its end counted from the promotion.
func TestPG_QueueAFullPoolWaitsHoldingNothing(t *testing.T) {
	e := newQueueEnv(t)
	pool := e.cappedPool("Busy", 1)
	occupant := e.occupy(pool)

	first := e.queuedLaunch(nil, pool, "")
	second := e.queuedLaunch(nil, pool, "")
	if want := runnerpool.WaitAtCapacityMsg(pool.Name, 1); !slices.Contains(first.Warnings, want) {
		t.Errorf("warnings %q do not carry the wait sentence %q", first.Warnings, want)
	}
	for _, r := range []types.AgentRun{e.run(first.ID), e.run(second.ID)} {
		if r.State != types.RunQueued || r.SPIFFEID != "" || r.AdmittedAt != nil || r.EndsAt != nil || r.LifetimeCapAt != nil || r.GovernanceProfileID != nil {
			t.Errorf("a waiting run carries start-time facts: %+v", r)
		}
		if r.StatusDetail != "queue: runner_pool_at_capacity" {
			t.Errorf("status_detail = %q, want the wait reason", r.StatusDetail)
		}
		if n := e.count(`SELECT count(*) FROM credential_grants WHERE run_id = $1`, r.ID); n != 0 {
			t.Errorf("a waiting run has %d grant rows", n)
		}
		if _, err := e.srv.loadRunProxyConfig(context.Background(), r.ID); err == nil {
			t.Error("a waiting run has a proxy config")
		}
		if len(e.audits(r.ID, "identity.mint")) != 0 {
			t.Error("a waiting run was minted an identity")
		}
	}
	rows := e.audits(first.ID, "run.create")
	if len(rows) != 1 {
		t.Fatalf("run.create rows = %d, want 1", len(rows))
	}
	data := queueAuditData(t, rows[0])
	facts, _ := data["queue"].(map[string]any)
	if _, hasJTI := data["jti"]; hasJTI || facts["reason"] != string(runnerpool.WaitAtCapacity) || facts["pool_id"] != pool.ID.String() {
		t.Errorf("run.create data = %v; want queue facts naming the wait reason and the pool, and no jti", data)
	}

	e.free(occupant)
	started := e.waitStarted(first.ID)
	if got := e.run(second.ID); got.State != types.RunQueued || e.queueRows(second.ID) != 1 {
		t.Errorf("the second launch is %s with %d queue rows; only the oldest may start", got.State, e.queueRows(second.ID))
	}
	if started.EndsAt == nil || len(e.audits(started.ID, "identity.mint")) != 1 {
		t.Fatalf("a promoted run was not minted for and given its end: %+v", started)
	}
	if d := started.EndsAt.Sub(*started.AdmittedAt); d < 3590*time.Second || d > 3610*time.Second {
		t.Errorf("ends %v after admission, want the pool's one-hour default counted from the promotion", d)
	}
	if e.queueRows(first.ID) != 0 {
		t.Error("a promoted run kept its queue row")
	}
	startRows := e.audits(started.ID, "run.queue.start")
	if len(startRows) != 1 || startRows[0].Outcome != "success" || queueAuditData(t, startRows[0])["jti"] == "" {
		t.Errorf("run.queue.start rows = %+v; want one success carrying the jti", startRows)
	}
}

// A kill of a waiting run is 202 KILLED and writes no identity or broker revocation rows: nothing was
// minted. A PATCH on it is 409 run_queued.
func TestPG_QueueKillAndDoorsOfAWaitingRun(t *testing.T) {
	e := newQueueEnv(t)
	pool := e.cappedPool("Busy", 1)
	e.occupy(pool)
	waiting := e.queuedLaunch(nil, pool, "")

	if w := e.admin(http.MethodPatch, "/api/v1/runs/"+waiting.ID.String(), `{"ends_at":null}`); w.Code != http.StatusConflict || wireReason(t, w) != reasonRunQueued {
		t.Errorf("PATCH on a waiting run = %d %s; want 409 %s", w.Code, w.Body, reasonRunQueued)
	}
	w := e.admin(http.MethodPost, "/api/v1/runs/"+waiting.ID.String()+"/kill", "")
	if w.Code != http.StatusAccepted {
		t.Fatalf("kill = %d %s, want 202", w.Code, w.Body)
	}
	if got := e.run(waiting.ID); got.State != types.RunKilled || e.queueRows(waiting.ID) != 0 {
		t.Errorf("killed run is %s with %d queue rows", got.State, e.queueRows(waiting.ID))
	}
	for _, action := range []string{"identity.revoke", "run.revoke", "broker.revoke"} {
		if rows := e.audits(waiting.ID, action); len(rows) != 0 {
			t.Errorf("a kill of a waiting run wrote %d %s rows", len(rows), action)
		}
	}
	kill := e.audits(waiting.ID, "run.kill")
	if len(kill) != 1 || queueAuditData(t, kill[0])["queued"] != true {
		t.Errorf("run.kill rows = %+v; want one with queued: true", kill)
	}
}

// A waiting run past its deadline is FAILED queue_timeout with run.queue.expire and its row gone; the
// request's own max_wait_sec sets the deadline.
func TestPG_QueueTimeout(t *testing.T) {
	e := newQueueEnv(t)
	pool := e.cappedPool("Busy", 1)
	e.occupy(pool)
	waiting := e.queuedLaunch(nil, pool, `,"max_wait_sec":90`)
	entry, err := e.pg.GetRunQueueEntry(context.Background(), waiting.ID)
	if err != nil {
		t.Fatal(err)
	}
	if d := entry.DeadlineAt.Sub(entry.QueuedAt); d != 90*time.Second {
		t.Errorf("deadline is %v after queueing, want max_wait_sec: 90s", d)
	}
	e.sweep()
	if got := e.run(waiting.ID); got.State != types.RunQueued {
		t.Fatalf("a run inside its deadline is %s", got.State)
	}
	e.exec(`UPDATE run_queue SET deadline_at = now() - interval '1 minute' WHERE run_id = $1`, waiting.ID)
	e.sweep()
	got := e.run(waiting.ID)
	if got.State != types.RunFailed || got.StatusDetail != "queue: queue_timeout" || got.FailureHint != types.RunEndQueueTimeout.Sentence() || e.queueRows(waiting.ID) != 0 {
		t.Errorf("timed out run = %s %q %q, %d rows", got.State, got.StatusDetail, got.FailureHint, e.queueRows(waiting.ID))
	}
	if rows := e.audits(waiting.ID, "run.queue.expire"); len(rows) != 1 {
		t.Errorf("run.queue.expire rows = %d, want 1", len(rows))
	}
}

// exec runs a statement on the test database.
func (e *queueEnv) exec(sql string, args ...any) {
	e.t.Helper()
	if _, err := e.poolEnv.pool.Exec(context.Background(), sql, args...); err != nil {
		e.t.Fatal(err)
	}
}

// The start sizes the run against the limits as they stand when it starts, not as they stood at launch,
// and records what it got on run.queue.start as run.create would: a pool cap lowered while the run
// waited cuts the ask to the new cap.
func TestPG_QueueStartRecordsTheSizeItApplied(t *testing.T) {
	e := newQueueEnv(t)
	pool := e.cappedPool("Busy", 1)
	occupant := e.occupy(pool)
	waiting := e.queuedLaunch(nil, pool, `,"resources":{"cpu_millis":3000}`)
	cur := e.reread(pool)
	limits := *cur.Limits
	limits.Background.CPUMillis.Cap = 2000
	if w := e.admin(http.MethodPut, "/api/v1/runner-pools/"+pool.ID.String(), fmt.Sprintf(`{"revision":%d,"limits":%s}`, cur.Revision, mustJSON(limits))); w.Code != http.StatusOK {
		t.Fatalf("lower the pool's CPU cap = %d %s", w.Code, w.Body)
	}
	e.free(occupant)
	rows := e.waitAudit(waiting.ID, "run.queue.start")
	if len(rows) != 1 || rows[0].Outcome != "success" {
		t.Fatalf("run.queue.start rows = %+v; want one success", rows)
	}
	var data struct {
		Applied struct {
			CPUMillis struct {
				Requested, Applied int
				Capped             bool
				Source             string
			} `json:"cpu_millis"`
		} `json:"resources_applied"`
	}
	if err := json.Unmarshal(rows[0].Data, &data); err != nil {
		t.Fatal(err)
	}
	if got := data.Applied.CPUMillis; got.Requested != 3000 || got.Applied != 2000 || !got.Capped || got.Source != string(types.LimitSourcePool) {
		t.Errorf("resources_applied.cpu_millis = %+v; want 3000 asked, 2000 applied, capped by the pool", got)
	}
}

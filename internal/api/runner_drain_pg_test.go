// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// Runner drains against a real Postgres (the runner_drains table): the routes are the super admin's, a drain
// changes where runs start and nothing about a running run, and a lost run's revive on a drained runner is
// refused. Guarded by WARDYN_TEST_PG: skipped when unset.

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/federation"
	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

func drainPath(id uuid.UUID) string { return "/api/v1/runners/" + id.String() + "/drain" }

// drainRunnerAs drains id as c and fails unless it answers 200.
func (e *poolEnv) drainRunnerAs(c *http.Cookie, id uuid.UUID, reason string) types.RunnerView {
	e.t.Helper()
	w := e.as(c, http.MethodPut, drainPath(id), `{"reason":"`+reason+`"}`)
	if w.Code != http.StatusOK {
		e.t.Fatalf("drain = %d %s", w.Code, w.Body)
	}
	return decodeJSON[types.RunnerView](e.t, w)
}

func TestRunnerDrain_Routes_PG(t *testing.T) {
	e := newOrgRunnerEnv(t, false)
	id := e.claimedRunner("sub-person")
	good := `{"reason":"node pool upgrade"}`
	for name, c := range map[string]*http.Cookie{"a member": e.user, "the runner's owner": e.person, "a security admin": e.alice} {
		if w := e.as(c, http.MethodPut, drainPath(id), good); w.Code != http.StatusForbidden {
			t.Fatalf("%s drains = %d %s, want 403", name, w.Code, w.Body)
		}
		if w := e.as(c, http.MethodDelete, drainPath(id), ""); w.Code != http.StatusForbidden {
			t.Fatalf("%s undrains = %d %s, want 403", name, w.Code, w.Body)
		}
	}
	for _, body := range []string{`{}`, `{"reason":"  "}`, `{"reason":"` + strings.Repeat("x", 513) + `"}`} {
		if w := e.as(e.superTwo, http.MethodPut, drainPath(id), body); w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "runner_drain_invalid") {
			t.Fatalf("PUT drain %.30s = %d %s, want 400 runner_drain_invalid", body, w.Code, w.Body)
		}
	}
	if w := e.as(e.superTwo, http.MethodPut, drainPath(uuid.New()), good); w.Code != http.StatusNotFound {
		t.Fatalf("drain of no runner = %d %s, want 404", w.Code, w.Body)
	}
	unclaimed := e.registerWith(e.mintOrg(e.alice).Token)
	if w := e.as(e.superTwo, http.MethodPut, drainPath(unclaimed), good); w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "runner_drain_invalid") {
		t.Fatalf("drain of an unclaimed runner = %d %s, want 409 runner_drain_invalid", w.Code, w.Body)
	}

	first := e.drainRunnerAs(e.superTwo, id, "node pool upgrade")
	if first.Drain == nil || first.Drain.Reason != "node pool upgrade" || first.Drain.CreatedBy != poolSubSuperTwo {
		t.Fatalf("drained view = %+v, want the reason and %s", first.Drain, poolSubSuperTwo)
	}
	again := e.drainRunnerAs(nil, id, "a second reason")
	if again.Drain == nil || again.Drain.Reason != "node pool upgrade" || again.Drain.CreatedBy != poolSubSuperTwo {
		t.Fatalf("a second drain changed the stored one: %+v", again.Drain)
	}
	rows := e.auditData(placement.ActionRunnerDrain, id.String())
	if len(rows) != 1 || rows[0]["reason"] != "node pool upgrade" || rows[0]["running"] != float64(0) {
		t.Fatalf("runner.drain rows = %v, want one with the reason and 0 running", rows)
	}

	w := e.as(nil, http.MethodDelete, drainPath(id), "")
	if w.Code != http.StatusOK || decodeJSON[types.RunnerView](t, w).Drain != nil {
		t.Fatalf("undrain = %d %s, want 200 and no drain", w.Code, w.Body)
	}
	if w := e.as(nil, http.MethodDelete, drainPath(id), ""); w.Code != http.StatusOK {
		t.Fatalf("a second undrain = %d %s, want 200", w.Code, w.Body)
	}
	rows = e.auditData(placement.ActionRunnerUndrain, id.String())
	if len(rows) != 1 || rows[0]["running"] != float64(0) || rows[0]["drained_for_sec"] == nil {
		t.Fatalf("runner.undrain rows = %v, want one with running and drained_for_sec", rows)
	}
	if _, has := rows[0]["reason"]; has {
		t.Fatalf("runner.undrain carries the drainer's text: %v", rows[0])
	}
}

// The runner's owner reads the reason of a drain and never who set it; the administrators' read has both.
func TestRunnerDrain_ViewHidesCreatedByFromOwner_PG(t *testing.T) {
	e := newOrgRunnerEnv(t, false)
	id := e.claimedRunner(e.personP)
	e.drainRunnerAs(e.superTwo, id, "node pool upgrade")
	own := e.call(e.person, http.MethodGet, "/api/v1/me/runners/"+id.String(), "")
	if got := decodeJSON[types.RunnerView](t, own).Drain; own.Code != http.StatusOK || got == nil || got.Reason != "node pool upgrade" || got.CreatedBy != "" {
		t.Fatalf("GET /me/runners/{id} = %d drain %+v, want the reason and no created_by", own.Code, got)
	}
	adm := e.as(nil, http.MethodGet, "/api/v1/runners/"+id.String(), "")
	if got := decodeJSON[types.RunnerView](t, adm).Drain; adm.Code != http.StatusOK || got == nil || got.Reason != "node pool upgrade" || got.CreatedBy != poolSubSuperTwo {
		t.Fatalf("GET /runners/{id} = %d drain %+v, want the reason and %s", adm.Code, got, poolSubSuperTwo)
	}
}

// A revoke leaves the drain row, inert: the drain routes answer 404 for the runner, and a drain of an
// unclaimed runner is refused by the store.
func TestRunnerDrain_RevokeLeavesTheRowInert_PG(t *testing.T) {
	e := newOrgRunnerEnv(t, false)
	ctx := context.Background()
	id := e.claimedRunner("sub-person")
	e.drainRunnerAs(e.superTwo, id, "node pool upgrade")
	if w := e.as(e.alice, http.MethodDelete, "/api/v1/runners/"+id.String(), ""); w.Code >= 300 {
		t.Fatalf("revoke = %d %s", w.Code, w.Body)
	}
	if got, err := e.pg.RunnerDrains(ctx, []uuid.UUID{id}); err != nil || len(got) != 1 {
		t.Fatalf("drains after the revoke = %v, %v; want the row kept", got, err)
	}
	if w := e.as(e.superTwo, http.MethodPut, drainPath(id), `{"reason":"again"}`); w.Code != http.StatusNotFound {
		t.Fatalf("drain of a revoked runner = %d %s, want 404", w.Code, w.Body)
	}
	if w := e.as(e.superTwo, http.MethodDelete, drainPath(id), ""); w.Code != http.StatusNotFound {
		t.Fatalf("undrain of a revoked runner = %d %s, want 404", w.Code, w.Body)
	}
	unclaimed := e.registerWith(e.mintOrg(e.alice).Token)
	if _, _, err := e.pg.DrainRunner(ctx, unclaimed, types.RunnerDrain{CreatedBy: "x", CreatedAt: time.Now().UTC(), Reason: "r"}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("DrainRunner on an unclaimed runner = %v, want ErrNotFound", err)
	}
}

// A run pinned to a drained runner is refused, a pool with another runner places it there, and a pool of
// drained runners refuses a new run.
func TestRunnerDrain_PoolAndPinRefuse_PG(t *testing.T) {
	e := newPlacementEnv(t)
	laptops := e.createPool("Laptops", types.RunnerPoolUser)
	a := e.laptop(e.personP, 4000, 8192, types.CC1, types.CC2)
	b := e.laptop(e.personP, 4000, 8192, types.CC1, types.CC2)
	for _, id := range []uuid.UUID{a, b} {
		e.addToPool(e.person, laptops.ID, id)
		e.stream(id, types.CC1, types.CC2)
	}
	e.personDefaults(e.person, `{"pool_id":"`+laptops.ID.String()+`"}`)
	e.drainRunnerAs(e.superTwo, a, "node pool upgrade")
	pin := e.call(e.person, http.MethodPost, "/api/v1/runs", runBody(`"placement":"local","runner_id":"`+a.String()+`"`))
	if pin.Code != http.StatusConflict || !strings.Contains(pin.Body.String(), "runner_draining") {
		t.Fatalf("a run pinned to a drained runner = %d %s, want 409 runner_draining", pin.Code, pin.Body)
	}
	if run := e.localCreate(runBody("")); run.RunnerID == nil || *run.RunnerID != b {
		t.Fatalf("a run beside a drained runner went to %v, want %s", run.RunnerID, b)
	}
	e.drainRunnerAs(e.superTwo, b, "node pool upgrade")
	w := e.call(e.person, http.MethodPost, "/api/v1/runs", runBody(""))
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "runner_draining") ||
		!strings.Contains(w.Body.String(), "that could take this run is drained for maintenance") {
		t.Fatalf("a run into a pool of drained runners = %d %s, want 409 runner_draining and its sentence", w.Code, w.Body)
	}
}

// A queued run starts on the pool's undrained runner; once every runner is drained it waits, and is never
// refused for the drain.
func TestRunnerDrain_QueuedRunPromotesElsewhere_PG(t *testing.T) {
	e := newQueueEnv(t)
	p, a, b := cappedLaptops(e, 1)
	e.drainRunnerAs(e.superTwo, a, "node pool upgrade")
	for i := 0; i < 4; i++ {
		_, got := launchThroughQueue(e, p, a, b)
		if got.State == types.RunFailed || got.RunnerID == nil || *got.RunnerID != b {
			t.Fatalf("launch %d = %s on %v detail %q; want it started on the undrained runner %s", i, got.State, got.RunnerID, got.StatusDetail, b)
		}
	}
	occupyRunner(e, p, b)
	queued := e.queuedLaunch(e.person, p, "")
	e.drainRunnerAs(e.superTwo, b, "node pool upgrade")
	e.sweep()
	if got := e.run(queued.ID); got.State != types.RunQueued || e.queueRows(queued.ID) != 1 {
		t.Fatalf("with every runner drained the queued run is %s with %d queue rows, want it waiting", got.State, e.queueRows(queued.ID))
	}
}

// A lost run on a drained runner is refused its revive with one denied run.revive row; a live run's proxy
// refresh is not refused.
func TestRunnerDrain_LostRunReviveRefused_PG(t *testing.T) {
	e := newQueueEnv(t)
	p, a, _ := cappedLaptops(e, 2)
	occupyRunner(e, p, a)
	e.drainRunnerAs(e.superTwo, a, "node pool upgrade")
	var run types.AgentRun
	if err := e.pool.QueryRow(context.Background(), `SELECT id FROM agent_runs WHERE runner_id = $1`, a).Scan(&run.ID); err != nil {
		t.Fatal(err)
	}
	run = e.run(run.ID)
	if rerr := e.srv.reviveDrainRefusal(context.Background(), run, types.ActorHuman, "alice"); rerr != nil {
		t.Fatalf("a live run's refresh = %+v, want it allowed", rerr)
	}
	now := time.Now().UTC()
	run.LostAt = &now
	rerr := e.srv.reviveDrainRefusal(context.Background(), run, types.ActorHuman, "alice")
	if rerr == nil || rerr.status != http.StatusConflict || rerr.reason != "runner_draining" {
		t.Fatalf("a lost run's revive = %+v, want 409 runner_draining", rerr)
	}
	if rows := e.audits(run.ID, "run.revive"); len(rows) != 1 || rows[0].Outcome != "denied" || !strings.Contains(string(rows[0].Data), a.String()) {
		t.Fatalf("run.revive rows = %+v, want one denied row naming the runner", rows)
	}
	if w := e.as(nil, http.MethodDelete, drainPath(a), ""); w.Code != http.StatusOK {
		t.Fatalf("undrain = %d %s", w.Code, w.Body)
	}
	if rerr := e.srv.reviveDrainRefusal(context.Background(), run, types.ActorHuman, "alice"); rerr != nil {
		t.Fatalf("after the undrain the revive is refused: %+v", rerr)
	}
}

// POST /runs/{id}/revive on a lost run whose runner is drained answers 409 runner_draining with one denied
// run.revive row, through the real route: it fails if reviveRunProxy stops calling reviveDrainRefusal. After
// the undrain the revive no longer answers that reason.
func TestRunnerDrain_ReviveRouteRefusesLostRun_PG(t *testing.T) {
	const owner = "sub-pg-drain-revive"
	h, _ := newRunOwnerPGHarness(t)
	ctx := t.Context()
	pg := h.srv.cfg.Store.(store.PG)
	runnerID := uuid.New()
	fp := "fp-" + runnerID.String()
	if _, err := pg.CreateRunner(ctx, types.Runner{ID: runnerID, Owner: owner, Name: "laptop", PublicKey: append(runnerID[:], runnerID[:]...),
		KeyFingerprint: fp, OrgURLSHA256: federation.OrgURLSHA256(poolTestOrgURL), Limits: roomyRunnerLimits()}); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.ClaimRunner(ctx, runnerID, owner, fp, time.Now()); err != nil {
		t.Fatal(err)
	}
	run, err := pg.CreateRun(ctx, types.AgentRun{ID: uuid.New(), CreatedBy: owner, Agent: "claude-code", ConfinementClass: types.CC1,
		State: types.RunRunning, RunnerTarget: "docker", Task: "t", RunnerID: &runnerID})
	if err != nil {
		t.Fatal(err)
	}
	if run.State != types.RunRunning {
		if ok, err := pg.UpdateRunStateIf(ctx, run.ID, run.State, types.RunRunning); err != nil || !ok {
			t.Fatalf("UpdateRunStateIf: %v %v", ok, err)
		}
	}
	if err := pg.SetSandboxRef(ctx, run.ID, "wardyn-agent-"+run.ID.String()); err != nil {
		t.Fatal(err)
	}
	if ok, err := pg.MarkRunLost(ctx, run.ID, types.LostOutage, time.Now(), 0); err != nil || !ok {
		t.Fatalf("MarkRunLost: %v %v", ok, err)
	}
	h.srv.cfg.Runner = &pgReviveRunner{fakeRunner: &fakeRunner{}}
	h.srv.cfg.ControlPlaneURL = "http://127.0.0.1:8080"
	if _, set, err := pg.DrainRunner(ctx, runnerID, types.RunnerDrain{CreatedBy: "alice", CreatedAt: time.Now().UTC(), Reason: "node pool upgrade"}); err != nil || !set {
		t.Fatalf("DrainRunner = %v, %v", set, err)
	}
	if code, reason := reviveReason(t, h, run, false); code != http.StatusConflict || reason != "runner_draining" {
		t.Fatalf("revive of a lost run on a drained runner = %d %q, want 409 runner_draining", code, reason)
	}
	denied := 0
	for _, ev := range h.audit.snapshot() {
		if ev.Action == "run.revive" && ev.Outcome == "denied" && ev.RunID != nil && *ev.RunID == run.ID && strings.Contains(string(ev.Data), "runner_draining") {
			denied++
		}
	}
	if denied != 1 {
		t.Fatalf("denied run.revive rows naming runner_draining = %d, want 1", denied)
	}
	if _, cleared, err := pg.UndrainRunner(ctx, runnerID); err != nil || !cleared {
		t.Fatalf("UndrainRunner = %v, %v", cleared, err)
	}
	if _, reason := reviveReason(t, h, run, false); reason == "runner_draining" {
		t.Fatal("after the undrain the revive still answers runner_draining")
	}
}

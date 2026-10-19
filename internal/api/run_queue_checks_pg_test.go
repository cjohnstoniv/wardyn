// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// The queue's start re-checks, pool changes, capacity split and the rest of the section 9 table,
// against a real Postgres. Guarded by WARDYN_TEST_PG.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/hostcapacity"
	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runnerpool"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// narrowPool leaves the pool allowing only the Fence barrier.
func narrowPool(e *queueEnv, pool types.RunnerPool, _ types.AgentRun) {
	cur := e.reread(pool)
	limits := *cur.Limits
	limits.Barriers = []types.ConfinementClass{types.CC1}
	w := e.admin(http.MethodPut, "/api/v1/runner-pools/"+pool.ID.String(), fmt.Sprintf(`{"revision":%d,"limits":%s}`, cur.Revision, mustJSON(limits)))
	if w.Code != http.StatusOK {
		e.t.Fatalf("narrow the pool = %d %s", w.Code, w.Body)
	}
}

// waitingPerson fills the pool and queues a launch of the person's behind it, returning that run and
// the occupant the test frees to promote it.
func (e *queueEnv) waitingPerson(pool types.RunnerPool, extra string) (types.AgentRun, types.AgentRun) {
	e.t.Helper()
	occupant := e.occupy(pool)
	return e.run(e.queuedLaunch(e.person, pool, extra).ID), occupant
}

// Each start re-check of section 7 ends a promoted run FAILED with its own reason in status_detail, the
// sentence in failure_hint, and a run.queue.start failure row; nothing is minted for it.
func TestPG_QueueStartRechecks(t *testing.T) {
	cases := []struct {
		name   string
		reason string
		sent   string
		change func(e *queueEnv, pool types.RunnerPool, run types.AgentRun)
		extra  string
	}{
		{name: "owner deactivated", reason: string(types.RunEndIdentityDeactivated), sent: types.RunEndIdentityDeactivated.Sentence(),
			change: func(e *queueEnv, _ types.RunnerPool, _ types.AgentRun) {
				if _, err := e.pg.IssueLoginIdentity(context.Background(), store.LoginIdentity{Principal: e.personP, Issuer: "https://idp.example", TenantID: "t", ObjectID: "o-1", Email: "person@corp.example"}, time.Now()); err != nil {
					e.t.Fatal(err)
				}
				e.exec(`UPDATE principal_identities SET deactivated_at = now() WHERE principal = $1`, e.personP)
			}},
		{name: "profile reassigned", reason: string(types.RunEndQueueProfileChanged), sent: types.RunEndQueueProfileChanged.Sentence(),
			change: func(e *queueEnv, _ types.RunnerPool, _ types.AgentRun) {
				e.seedAssignment(e.seedProfile("Later", "api.anthropic.com", "github.com"), e.personP)
			}},
		{name: "launch door closed", reason: "capability_agent", sent: "",
			change: func(e *queueEnv, _ types.RunnerPool, _ types.AgentRun) {
				if _, err := e.pg.UpsertCapabilityGrant(context.Background(), types.CapabilityGrant{SubjectType: types.CapabilitySubjectUser,
					Subject: e.personP, Capability: capAgent, Value: "claude-code", Effect: types.CapabilityDeny, CreatedBy: "seed"}); err != nil {
					e.t.Fatal(err)
				}
			}},
		{name: "pool refuses the barrier", reason: string(runnerpool.ReasonBarrierNotAllowed), sent: "", extra: `,"confinement_class":"CC3"`,
			change: narrowPool},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newQueueEnv(t)
			pool := e.cappedPool("Busy", 1)
			waiting, occupant := e.waitingPerson(pool, tc.extra)
			tc.change(e, pool, waiting)
			e.free(occupant)
			got := e.waitFailed(waiting.ID)
			if got.StatusDetail != "queue: "+tc.reason {
				t.Errorf("status_detail = %q, want queue: %s", got.StatusDetail, tc.reason)
			}
			if tc.sent != "" && got.FailureHint != tc.sent {
				t.Errorf("failure_hint = %q, want %q", got.FailureHint, tc.sent)
			}
			if got.SPIFFEID != "" || len(e.audits(got.ID, "identity.mint")) != 0 {
				t.Error("a refused start minted an identity")
			}
			rows := e.waitAudit(got.ID, "run.queue.start")
			if len(rows) != 1 || rows[0].Outcome != "failure" || queueAuditData(t, rows[0])["reason"] != tc.reason {
				t.Errorf("run.queue.start rows = %+v; want one failure naming %s", rows, tc.reason)
			}
		})
	}
}

// A stored policy edited while the run waits ends it queue_policy_changed; an unedited one starts.
func TestPG_QueuePolicyEditedWhileWaiting(t *testing.T) {
	e := newQueueEnv(t)
	pool := e.cappedPool("Busy", 1)
	body := func(domain string) string {
		return `{"name":"reviewed","spec":{"min_confinement_class":"CC2","allowed_domains":["` + domain + `"]}}`
	}
	w := e.admin(http.MethodPost, "/api/v1/policies", body("api.anthropic.com"))
	if w.Code != http.StatusCreated {
		t.Fatalf("store policy = %d %s", w.Code, w.Body)
	}
	policy := decodeJSON[types.RunPolicy](t, w)
	extra := `,"policy_id":"` + policy.ID.String() + `"`

	occupant := e.occupy(pool)
	unedited := e.queuedLaunch(nil, pool, extra)
	edited := e.queuedLaunch(nil, pool, extra)
	if _, err := e.pg.UpdatePolicy(context.Background(), policy.ID, "reviewed", types.RunPolicySpec{MinConfinementClass: types.CC2, AllowedDomains: []string{"pypi.org"}}); err != nil {
		t.Fatal(err)
	}
	e.free(occupant)
	got := e.waitFailed(unedited.ID)
	if got.StatusDetail != "queue: queue_policy_changed" || got.FailureHint != types.RunEndQueuePolicyChanged.Sentence() {
		t.Errorf("a run whose policy changed = %s %q %q", got.State, got.StatusDetail, got.FailureHint)
	}
	_ = edited
}

// A portal launch's start reads the portal's row, never the ten-minute delegated token: a launch that
// waited 11 minutes starts, its rows carry via, and a launch whose portal was revoked meanwhile ends
// queue_portal_revoked.
func TestPG_QueuePortalLaunchAfterTheTokenExpired(t *testing.T) {
	e := newQueueEnv(t)
	pool := e.cappedPool("Busy", 2)
	d, err := e.pg.CreateDelegate(context.Background(), types.Delegate{ID: uuid.New(), Name: "portal", IdPClientID: "c", Group: "g", RegisteredBy: "admin"}, "raw-credential")
	if err != nil {
		t.Fatal(err)
	}
	first, second := e.occupy(pool), e.occupy(pool)
	live := e.queuedLaunch(e.person, pool, "")
	revoked := e.queuedLaunch(e.person, pool, "")
	grant := uuid.New() // the delegated token is long gone: it was swept ten minutes after it was minted
	for _, id := range []uuid.UUID{live.ID, revoked.ID} {
		e.exec(`UPDATE run_queue SET authority = authority || jsonb_build_object('delegate', $2::text, 'grant', $3::text),
			queued_at = queued_at - interval '11 minutes' WHERE run_id = $1`, id, d.ID.String(), grant.String())
	}
	e.free(first)
	started := e.waitStarted(live.ID)
	rows := e.audits(started.ID, "run.queue.start")
	if len(rows) != 1 || rows[0].Outcome != "success" {
		t.Fatalf("run.queue.start rows = %+v; want one success", rows)
	}
	via, ok := queueAuditData(t, rows[0])["via"].(map[string]any)
	if !ok || via["delegate"] != d.ID.String() || via["grant"] != grant.String() {
		t.Errorf("the start row's via = %v; want the portal and the delegated token", queueAuditData(t, rows[0])["via"])
	}

	if _, err := e.pg.RevokeDelegate(context.Background(), d.ID, time.Now()); err != nil {
		t.Fatal(err)
	}
	e.free(second)
	got := e.waitFailed(revoked.ID)
	if got.StatusDetail != "queue: queue_portal_revoked" || got.FailureHint != types.RunEndQueuePortalRevoked.Sentence() {
		t.Errorf("a run whose portal was revoked = %s %q %q", got.State, got.StatusDetail, got.FailureHint)
	}
	if fail := e.waitAudit(revoked.ID, "run.queue.start"); len(fail) != 1 || queueAuditData(t, fail[0])["via"] == nil {
		t.Errorf("the failure row = %+v; want one carrying via", fail)
	}
}

// Removing a pool ends its waiting runs KILLED runner_pool_removed with a system run.kill row; switching
// it off holds them, promotion skipping it.
func TestPG_QueuePoolRemovedAndSwitchedOff(t *testing.T) {
	e := newQueueEnv(t)
	pool := e.cappedPool("Busy", 1)
	occupant := e.occupy(pool)
	waiting := e.queuedLaunch(nil, pool, "")

	w := e.admin(http.MethodPut, "/api/v1/runner-pools/"+pool.ID.String(), fmt.Sprintf(`{"revision":%d,"state":"disabled"}`, pool.Revision))
	if w.Code != http.StatusOK {
		t.Fatalf("switch off = %d %s", w.Code, w.Body)
	}
	e.free(occupant)
	e.sweep()
	if got := e.run(waiting.ID); got.State != types.RunQueued || e.queueRows(waiting.ID) != 1 {
		t.Fatalf("a run waiting for a switched-off pool is %s with %d rows; it must keep waiting", got.State, e.queueRows(waiting.ID))
	}
	if w := e.admin(http.MethodDelete, "/api/v1/runner-pools/"+pool.ID.String(), ""); w.Code != http.StatusNoContent {
		t.Fatalf("delete = %d %s", w.Code, w.Body)
	}
	got := e.run(waiting.ID)
	if got.State != types.RunKilled || got.StatusDetail != "queue: runner_pool_removed" || got.FailureHint != types.RunEndRunnerPoolRemoved.Sentence() || e.queueRows(waiting.ID) != 0 {
		t.Errorf("a run whose pool was removed = %s %q %q, %d rows", got.State, got.StatusDetail, got.FailureHint, e.queueRows(waiting.ID))
	}
	kill := e.audits(waiting.ID, "run.kill")
	if len(kill) != 1 || kill[0].ActorType != types.ActorSystem || queueAuditData(t, kill[0])["reason"] != "runner_pool_removed" || queueAuditData(t, kill[0])["queued"] != true {
		t.Errorf("run.kill rows = %+v; want one system row naming the removal", kill)
	}
}

// queueFitRunner answers the quota fit check with a script and otherwise behaves as the pool runner.
type queueFitRunner struct {
	*poolRunner
	fit runner.Fit
}

func (f *queueFitRunner) CheckFit(context.Context, runner.Resources) (runner.Fit, error) {
	return f.fit, nil
}

func quotaFit(need, left, hard int64) runner.Fit {
	return runner.Fit{Quotas: runner.ReadOK, Nodes: runner.ReadOK, Quota: []runner.QuotaFit{{Name: "runs",
		Axes: []runner.QuotaAxis{{Key: "requests.cpu", Need: need, Left: left, Hard: hard}}}}}
}

// A run that needs more than the quota has left, or the host has room for, waits; one that needs more
// than the quota could ever hold is refused 422 and leaves no row.
func TestPG_QueueFitAndHostCapacitySplit(t *testing.T) {
	e := newQueueEnv(t)
	fit := &queueFitRunner{poolRunner: e.runner, fit: quotaFit(4000, 2000, 10000)}
	e.srv.cfg.Runner = fit
	pool := e.cappedPool("Roomy", 5)

	waiting, w := e.launch(nil, pool, "")
	if w.Code != http.StatusCreated || waiting.State != types.RunQueued {
		t.Fatalf("Need > Left on a pool with a free slot = %d %s; want 201 QUEUED", w.Code, w.Body)
	}
	runs := e.count(`SELECT count(*) FROM agent_runs`)
	fit.fit = quotaFit(12000, 2000, 10000)
	if _, w := e.launch(nil, pool, ""); w.Code != http.StatusUnprocessableEntity || wireReason(t, w) != reasonNamespaceQuotaExceeded {
		t.Errorf("Need > Hard = %d %s; want 422 %s", w.Code, w.Body, reasonNamespaceQuotaExceeded)
	}
	if n := e.count(`SELECT count(*) FROM agent_runs`); n != runs {
		t.Errorf("a refused launch left %d new run rows", n-runs)
	}

	// The room opens up: the sweep starts the waiting run.
	fit.fit = quotaFit(4000, 8000, 10000)
	e.sweep()
	e.waitStarted(waiting.ID)

	// Host capacity at its limit waits too; it does not refuse the launch.
	e.srv.cfg.HostCapacity = hostcapacity.New(hostcapacity.Limits{MinAvailableMiB: 1 << 20}, func() (int, float64, error) { return 100, 0, nil })
	hostWaiting, w := e.launch(nil, pool, "")
	if w.Code != http.StatusCreated || hostWaiting.State != types.RunQueued {
		t.Fatalf("a launch while the host is full = %d %s; want 201 QUEUED", w.Code, w.Body)
	}
	e.sweep()
	if got := e.run(hostWaiting.ID); got.State != types.RunQueued {
		t.Errorf("a promotion while the host is full started the run: %s", got.State)
	}
	e.srv.cfg.HostCapacity = nil
	e.sweep()
	e.waitStarted(hostWaiting.ID)
}

// An all-offline self-hosted pool is refused at enqueue and never queued (T20), even when the pool is
// at its cap and a launch into it would otherwise wait.
func TestOfflineSelfHostedIsRefusedNotQueued(t *testing.T) {
	e := newQueueEnv(t)
	body := `{"name":"Laptops","limits":{"background":{"cpu_millis":{"default":500,"cap":4000},` +
		`"memory_mib":{"default":512,"cap":8192},"lifetime":{"default_sec":3600,"max_sec":7200}},"barriers":["CC1","CC2","CC3"],"max_concurrent_runs":1}}`
	w := e.call(e.person, http.MethodPost, "/api/v1/me/runner-pools", body)
	if w.Code != http.StatusCreated {
		t.Fatalf("create pool = %d %s", w.Code, w.Body)
	}
	pool := e.reread(decodeJSON[types.RunnerPool](t, w))
	laptop := e.laptop(e.personP, 4000, 8192, types.CC1, types.CC2, types.CC3)
	e.addToPool(e.person, pool.ID, laptop)

	e.online[laptop] = false
	e.occupy(pool)
	runs := e.count(`SELECT count(*) FROM agent_runs`)
	_, w = e.launch(e.person, pool, "")
	if w.Code != http.StatusUnprocessableEntity || wireReason(t, w) != string(placement.ReasonRunnerOffline) {
		t.Fatalf("a launch into a pool whose only runner is offline = %d %s; want 422 %s", w.Code, w.Body, placement.ReasonRunnerOffline)
	}
	if n, q := e.count(`SELECT count(*) FROM agent_runs`), e.count(`SELECT count(*) FROM run_queue`); n != runs || q != 0 {
		t.Errorf("the refused launch left %d run rows and %d queue rows", n-runs, q)
	}
}

// A run whose runner became unreachable after it queued is not refused: it keeps its place, says
// runner_pool_no_runner_free, goes back to runner_pool_at_capacity when the pool fills again, and starts
// when the runner is back and a slot frees.
func TestPG_QueueWaitsForARunnerThatWentAway(t *testing.T) {
	e := newQueueEnv(t)
	pool := e.cappedPool("Busy", 1)
	occupant := e.occupy(pool)
	waiting := e.queuedLaunch(nil, pool, "")
	facts := e.runner.execFacts["alpha"]
	delete(e.runner.execFacts, "alpha")
	e.runner.silent = map[string]bool{"alpha": true}
	e.free(occupant)
	e.sweep()
	got := e.run(waiting.ID)
	if got.State != types.RunQueued || got.StatusDetail != "queue: runner_pool_no_runner_free" || e.queueRows(waiting.ID) != 1 {
		t.Fatalf("a run whose executor went away is %s %q with %d rows; want QUEUED waiting with runner_pool_no_runner_free", got.State, got.StatusDetail, e.queueRows(waiting.ID))
	}
	again := e.occupy(pool)
	e.sweep()
	if got := e.run(waiting.ID); got.StatusDetail != "queue: runner_pool_at_capacity" {
		t.Errorf("with the pool full again the run says %q; want runner_pool_at_capacity", got.StatusDetail)
	}
	e.runner.execFacts["alpha"] = facts
	delete(e.runner.silent, "alpha")
	e.free(again)
	e.sweep()
	e.waitStarted(waiting.ID)
}

// midPassFillStore fills the pool while a promotion pass is judging: the pass reads its verdict with no
// lock, so a launch or another pass can fill the pool before the pass writes what it found.
type midPassFillStore struct {
	store.PG
	fill func()
}

func (m midPassFillStore) PromoteRunQueue(ctx context.Context, pass store.PromotePass, admit store.QueueAdmit) ([]store.PromotedRun, []store.QueueHeld, error) {
	return m.PG.PromoteRunQueue(ctx, pass, func(ctx context.Context, p types.RunnerPool, e store.RunQueueEntry) (store.QueueVerdict, error) {
		v, err := admit(ctx, p, e)
		m.fill()
		return v, err
	})
}

// A full pool outranks a reason a pass found while it judged: when the pool fills between the verdict and
// the write, the run keeps runner_pool_at_capacity instead of taking runner_pool_no_runner_free back.
func TestPG_QueueHeldReasonYieldsToAFullPool(t *testing.T) {
	e := newQueueEnv(t)
	pool := e.cappedPool("Busy", 1)
	occupant := e.occupy(pool)
	waiting := e.queuedLaunch(nil, pool, "")
	if got := e.run(waiting.ID).StatusDetail; got != "queue: runner_pool_at_capacity" {
		t.Fatalf("a run behind a full pool says %q; want runner_pool_at_capacity", got)
	}
	delete(e.runner.execFacts, "alpha")
	e.runner.silent = map[string]bool{"alpha": true}
	// The occupant ends with no kick, so no other pass runs beside this one.
	if ok, err := e.pg.UpdateRunStateIf(context.Background(), occupant.ID, types.RunRunning, types.RunCompleted); err != nil || !ok {
		t.Fatalf("end the occupant: %v %v", ok, err)
	}
	e.srv.cfg.Store = midPassFillStore{PG: e.pg, fill: func() { e.occupy(pool) }}
	if err := e.srv.promoteQueue(context.Background(), pool.ID, queueSweepBound); err != nil {
		t.Fatal(err)
	}
	if got := e.run(waiting.ID).StatusDetail; got != "queue: runner_pool_at_capacity" {
		t.Errorf("a pool that filled during the pass: the run says %q; want runner_pool_at_capacity", got)
	}
}

// A person's 101st waiting run for one pool is refused 422 runner_pool_queue_full with no row and no
// audit row; a cancelled run stops counting.
func TestPG_QueueFloodLimit(t *testing.T) {
	e := newQueueEnv(t)
	pool := e.cappedPool("Busy", 1)
	e.occupy(pool)
	var first uuid.UUID
	for i := range queueMaxPerOwner {
		now := time.Now().UTC().Add(time.Duration(i) * time.Millisecond)
		id := uuid.New()
		r, err := e.pg.CreateQueuedRun(context.Background(), types.AgentRun{
			ID: id, CreatedAt: now, UpdatedAt: now, CreatedBy: "admin-token", Agent: "claude-code", ConfinementClass: types.CC2,
			State: types.RunQueued, RunnerTarget: "docker", RunnerPoolID: &pool.ID, RunnerPoolRevision: pool.Revision, RunnerPoolSelection: types.RunnerPoolSelectedExplicit,
		}, store.RunQueueEntry{QueuedAt: now, DeadlineAt: now.Add(time.Hour)}, 0)
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			first = r.ID
		}
	}
	runs, audits := e.count(`SELECT count(*) FROM agent_runs`), len(e.h.audit.snapshot())
	_, w := e.launch(nil, pool, "")
	if w.Code != http.StatusUnprocessableEntity || wireReason(t, w) != string(runnerpool.ReasonQueueFull) {
		t.Fatalf("the 101st waiting run = %d %s; want 422 %s", w.Code, w.Body, runnerpool.ReasonQueueFull)
	}
	if !strings.Contains(w.Body.String(), runnerpool.QueueFullMsg(pool.Name, queueMaxPerOwner)) {
		t.Errorf("the refusal does not say %q: %s", runnerpool.QueueFullMsg(pool.Name, queueMaxPerOwner), w.Body)
	}
	if n := e.count(`SELECT count(*) FROM agent_runs`); n != runs {
		t.Errorf("the refused launch left %d run rows", n-runs)
	}
	for _, ev := range e.h.audit.snapshot()[audits:] {
		if ev.Action == "run.create" {
			t.Errorf("the refused launch wrote a run.create row: %+v", ev)
		}
	}
	if ok, err := e.srv.casRunState(context.Background(), first, types.RunQueued, types.RunKilled); err != nil || !ok {
		t.Fatal(err)
	}
	if _, w := e.launch(nil, pool, ""); w.Code != http.StatusCreated {
		t.Errorf("a launch after one was cancelled = %d %s; the cancelled run must stop counting", w.Code, w.Body)
	}
}

// A promoted run whose start died is failed within a sweep, its slot freed; a plain PENDING run five
// minutes into its build with a two-minute-old lease is left alone.
func TestPG_QueueStaleLeaseFailsOnlyPromotedRuns(t *testing.T) {
	e := newQueueEnv(t)
	pool := e.cappedPool("Busy", 1)
	occupant := e.occupy(pool)
	waiting := e.queuedLaunch(nil, pool, "")
	// The promotion that started it died: promote by hand, with no start behind it.
	e.exec(`DELETE FROM agent_runs WHERE id = $1`, occupant.ID)
	promoted, _, err := e.pg.PromoteRunQueue(context.Background(), store.PromotePass{PoolID: pool.ID, ScanLimit: 10, WatcherOwner: "dead-replica"},
		func(context.Context, types.RunnerPool, store.RunQueueEntry) (store.QueueVerdict, error) {
			return store.QueueVerdict{Admit: true}, nil
		})
	if err != nil || len(promoted) != 1 || promoted[0].Run.ID != waiting.ID {
		t.Fatalf("promote by hand: %v %v", promoted, err)
	}
	other := e.cappedPool("Plain", 5)
	plain := e.occupy(other)
	e.exec(`UPDATE agent_runs SET state = 'PENDING', created_at = now() - interval '5 minutes', watcher_heartbeat = now() - interval '2 minutes' WHERE id = $1`, plain.ID)
	e.exec(`UPDATE agent_runs SET watcher_heartbeat = now() - interval '2 minutes' WHERE id = $1`, waiting.ID)

	e.sweep()
	got := e.run(waiting.ID)
	if got.State != types.RunFailed || got.FailureHint != queueStartLostHint {
		t.Errorf("a promoted run with a dead start = %s %q; want FAILED with the lost-start hint", got.State, got.FailureHint)
	}
	if rows := e.audits(waiting.ID, "run.queue.start"); len(rows) != 1 || queueAuditData(t, rows[0])["reason"] != "start_lost" {
		t.Errorf("run.queue.start rows = %+v; want a start_lost failure", rows)
	}
	if survivor := e.run(plain.ID); survivor.State != types.RunPending {
		t.Errorf("a plain PENDING run in its build was %s by the queue's stale-lease pass", survivor.State)
	}
}

// Erasing a person and suspending a leaver end their waiting runs: the queue row holds their request
// and identity snapshot, and the run would otherwise start under the very person who is gone.
func TestPG_QueueErasureAndLeaverCancelWaitingRuns(t *testing.T) {
	e := newQueueEnv(t)
	pool := e.cappedPool("Busy", 1)
	e.occupy(pool)
	a, b := e.queuedLaunch(e.person, pool, ""), e.queuedLaunch(e.person, pool, "")

	// A leaver's suspension kills what ListNonTerminalRunsBy returns, the waiting runs among it.
	owned, err := e.pg.ListNonTerminalRunsBy(context.Background(), e.personP)
	if err != nil || !slices.ContainsFunc(owned, func(r types.AgentRun) bool { return r.ID == a.ID }) {
		t.Fatalf("a leaver's non-terminal runs omit the waiting run: %v %v", owned, err)
	}
	run := e.run(a.ID)
	applied, killErrs, err := e.srv.killRunCascade(context.Background(), run, types.ActorSystem, scimActor, map[string]any{"reason": "scim_suspend"})
	if err != nil || !applied || len(killErrs) != 0 {
		t.Fatalf("suspension kill = %v %v %v", applied, killErrs, err)
	}
	if e.run(a.ID).State != types.RunKilled || e.queueRows(a.ID) != 0 {
		t.Error("a suspended leaver's waiting run kept its queue row")
	}

	out, err := e.srv.eraseRunTasks(context.Background(), e.personP)
	if err != nil {
		t.Fatal(err)
	}
	if got := out.(map[string]any)["queued_cancelled"]; got != 1 {
		t.Errorf("erase reported %v cancelled waiting runs, want 1", got)
	}
	if e.run(b.ID).State != types.RunKilled || e.queueRows(b.ID) != 0 {
		t.Error("an erased person's waiting run kept its queue row")
	}
	if rows := e.audits(b.ID, "run.kill"); len(rows) != 1 || queueAuditData(t, rows[0])["reason"] != "person_erased" {
		t.Errorf("run.kill rows = %+v; want one naming the erasure", rows)
	}
}

// Nothing reaches a waiting run through the run-token doors or the per-run doors that need a sandbox.
func TestPG_QueueWaitingRunDoors(t *testing.T) {
	e := newQueueEnv(t)
	pool := e.cappedPool("Busy", 1)
	e.occupy(pool)
	waiting := e.queuedLaunch(nil, pool, "")

	token := e.h.mintRunToken(t, waiting.ID)
	w := do(t, e.srv, http.MethodPost, "/api/v1/internal/activity", token, `{}`)
	if w.Code != http.StatusForbidden || wireReason(t, w) != "run_not_found" {
		t.Errorf("an /internal door for a waiting run = %d %s; want 403 run_not_found", w.Code, w.Body)
	}
	for _, path := range []string{"/revive", "/pause", "/resume", "/attach-ticket"} {
		if w := e.admin(http.MethodPost, "/api/v1/runs/"+waiting.ID.String()+path, ""); w.Code < 400 || w.Code >= 500 {
			t.Errorf("POST %s on a waiting run = %d %s; want a 4xx refusal", path, w.Code, w.Body)
		}
	}
	for _, path := range []string{"/files", "/resources"} {
		if w := e.admin(http.MethodGet, "/api/v1/runs/"+waiting.ID.String()+path, ""); w.Code < 400 || w.Code >= 500 {
			t.Errorf("GET %s on a waiting run = %d %s; want a 4xx refusal", path, w.Code, w.Body)
		}
	}
	if got := e.run(waiting.ID); got.State != types.RunQueued {
		t.Errorf("a refused door moved the run to %s", got.State)
	}
}

// A revive refused for a full pool answers 422 runner_pool_full, naming the pool and its cap, and
// audits a denied run.revive row.
func TestPG_QueueReviveRefusedForAFullPool(t *testing.T) {
	e := newQueueEnv(t)
	run := types.AgentRun{ID: uuid.New(), CreatedBy: "alice"}
	rv := fullPoolReviver{pool: "Busy", max: 3}
	rerr := e.srv.claimRevive(context.Background(), rv, run, true, types.ActorHuman, "alice")
	if rerr == nil || rerr.status != http.StatusUnprocessableEntity || rerr.reason != string(runnerpool.ReasonFull) || rerr.msg != runnerpool.FullMsg("Busy", 3) {
		t.Fatalf("claimRevive = %+v; want 422 runner_pool_full naming the pool and cap", rerr)
	}
	rows := e.audits(run.ID, "run.revive")
	if len(rows) != 1 || rows[0].Outcome != "denied" || queueAuditData(t, rows[0])["reason"] != string(runnerpool.ReasonFull) {
		t.Errorf("run.revive rows = %+v; want one denied row", rows)
	}
}

// fullPoolReviver is a store whose revive claim finds the run's pool full.
type fullPoolReviver struct {
	store.RunReviver
	pool string
	max  int
}

func (f fullPoolReviver) MarkRunRevived(context.Context, uuid.UUID, types.LostReason, *store.EndedKept, int, bool) (bool, error) {
	return false, fmt.Errorf("claim: %w", &store.RunnerPoolFullError{Pool: f.pool, Max: f.max})
}

// The start holds the owner to the doors the run itself went through, not only the request's: a model
// provider the owner can no longer use ends a run that chose it.
func TestPG_QueueStartChecksTheDoorsTheRunWentThrough(t *testing.T) {
	e := newQueueEnv(t)
	if _, err := e.pg.UpsertCapabilityGrant(context.Background(), types.CapabilityGrant{SubjectType: types.CapabilitySubjectUser,
		Subject: e.personP, Capability: capModelProvider, Value: "corp", Effect: types.CapabilityDeny, CreatedBy: "seed"}); err != nil {
		t.Fatal(err)
	}
	auth, _ := json.Marshal(queueAuthority{Sub: e.personP, Role: "user", UserType: types.UserTypeStandard, Groups: []string{}})
	launch, _ := json.Marshal(queuedLaunch{Request: json.RawMessage(`{}`)})
	run := types.AgentRun{ID: uuid.New(), Agent: "claude-code", CreatedBy: e.personP, ModelProviderID: "corp"}
	st, err := newQueuedStart(context.Background(), context.Background(), store.PromotedRun{Run: run, Entry: store.RunQueueEntry{Authority: auth, Launch: launch}})
	if err != nil {
		t.Fatal(err)
	}
	_, end := e.srv.queueGovernance(st, createRunRequest{Agent: "claude-code"})
	if end == nil || end.token != reasonOwnerCapabilityModelProvider {
		t.Errorf("end = %+v; want %s", end, reasonOwnerCapabilityModelProvider)
	}
}

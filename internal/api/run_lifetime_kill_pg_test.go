// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// killCountingRunner is a leaseRunner that also counts the sandbox kills.
type killCountingRunner struct {
	*leaseRunner
	kmu   sync.Mutex
	kills int
}

func (r *killCountingRunner) KillSandbox(context.Context, string) error {
	r.kmu.Lock()
	r.kills++
	r.kmu.Unlock()
	return nil
}

func (r *killCountingRunner) killCount() int {
	r.kmu.Lock()
	defer r.kmu.Unlock()
	return r.kills
}

// routedKillRunner sends a sandbox kill through a registered runner's substrate,
// as the orchestrator does for a self-hosted run.
type routedKillRunner struct {
	*fakeRunner
	sub interface {
		KillSandbox(context.Context, string) error
	}
}

func (r *routedKillRunner) KillSandbox(ctx context.Context, ref string) error {
	return r.sub.KillSandbox(ctx, ref)
}

// lifetimeFixture is a Server over real Postgres whose lease sweep ends runs at
// their end and at their lifetime cap (no-end runs design, O2-A and O3-A).
type lifetimeFixture struct {
	srv   *Server
	st    store.PG
	rn    *killCountingRunner
	idp   *countingIdentity
	brk   *raceBroker
	audit *syncAudit
	now   time.Time
}

func newLifetimeFixture(t *testing.T) *lifetimeFixture {
	t.Helper()
	pool := throwawayPGPool(t)
	st := store.NewPG(pool)
	h := newHarness(t)
	f := &lifetimeFixture{
		st:    st,
		rn:    &killCountingRunner{leaseRunner: &leaseRunner{finalizeTailRunner: &finalizeTailRunner{fakeRunner: &fakeRunner{}}}},
		brk:   &raceBroker{},
		audit: &syncAudit{},
		now:   time.Now().UTC().Truncate(time.Second),
	}
	f.srv = h.srv
	f.srv.cfg.Store = st
	f.srv.cfg.Runner = f.rn
	f.srv.cfg.Broker = f.brk
	f.srv.cfg.Audit = f.audit
	f.srv.cfg.EndedRunGrace = 7 * 24 * time.Hour
	f.srv.cfg.Now = func() time.Time { return f.now }
	f.idp = &countingIdentity{Provider: f.srv.cfg.Identity}
	f.srv.cfg.Identity = f.idp
	return f
}

// run stores a RUNNING run with a sandbox; endIn and capIn are offsets from the
// fixture's clock, nil for none.
func (f *lifetimeFixture) run(t *testing.T, exp types.RunExperience, endIn, capIn *time.Duration) types.AgentRun {
	t.Helper()
	r := types.AgentRun{
		ID: uuid.New(), CreatedAt: f.now.Add(-48 * time.Hour), UpdatedAt: f.now, CreatedBy: "life@example.com",
		Agent: "claude-code", Task: "lifetime", ConfinementClass: types.CC2, State: types.RunRunning,
		RunnerTarget: "docker", SandboxRef: "wardyn-agent-" + uuid.NewString()[:8], Experience: exp,
	}
	if endIn != nil {
		at := f.now.Add(*endIn)
		r.EndsAt = &at
	}
	if capIn != nil {
		at := f.now.Add(*capIn)
		r.LifetimeCapAt, r.LifetimeCapSource = &at, types.LimitSourceDeployment
	}
	r, err := f.st.CreateRun(context.Background(), r)
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	return r
}

func (f *lifetimeFixture) sweep(t *testing.T) {
	t.Helper()
	if err := f.srv.sweepRunLeases(context.Background()); err != nil {
		t.Fatalf("sweepRunLeases: %v", err)
	}
}

func (f *lifetimeFixture) get(t *testing.T, id uuid.UUID) types.AgentRun {
	t.Helper()
	r, err := f.st.GetRun(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func dur(d time.Duration) *time.Duration { return &d }

// requireKilledAtLifetime asserts the whole kill: KILLED with the reason in
// status_detail (and projected as status_reason), the run.kill row by
// system/wardynd carrying the reason and both bounds, the sandbox killed, the
// run identity and broker credentials revoked.
func (f *lifetimeFixture) requireKilledAtLifetime(t *testing.T, run types.AgentRun, reason types.RunEndReason) {
	t.Helper()
	got := f.get(t, run.ID)
	if got.State != types.RunKilled || got.StatusDetail != "lifetime: "+string(reason) || got.EndedAt == nil {
		t.Fatalf("state=%s detail=%q ended_at=%v; want KILLED, %q and an end time", got.State, got.StatusDetail, got.EndedAt, "lifetime: "+string(reason))
	}
	runs := []types.AgentRun{got}
	projectStatusDetail(runs)
	if runs[0].StatusReason != string(reason) {
		t.Errorf("projected status_reason = %q, want %q", runs[0].StatusReason, reason)
	}
	if f.rn.killCount() != 1 || f.rn.stopCount() != 0 || f.rn.endCount() != 0 {
		t.Errorf("kills=%d stops=%d ends=%d; want the sandbox killed once", f.rn.killCount(), f.rn.stopCount(), f.rn.endCount())
	}
	if f.idp.count() != 1 || f.brk.count(run.ID) != 1 {
		t.Errorf("identity revokes=%d broker revokes=%d; want 1 each", f.idp.count(), f.brk.count(run.ID))
	}
	kills := f.audit.eventsFor(run.ID, "run.kill")
	if len(kills) != 1 || kills[0].Outcome != "success" || kills[0].ActorType != types.ActorSystem || kills[0].Actor != "wardynd" {
		t.Fatalf("run.kill rows = %+v, want one success by system/wardynd", kills)
	}
	data := leaseAuditData(t, kills[0])
	if data["reason"] != string(reason) || data["source"] != "this deployment" {
		t.Errorf("run.kill data = %v, want reason %q and the cap's source label", data, reason)
	}
	if _, ok := data["ends_at"]; !ok {
		t.Errorf("run.kill data = %v, want ends_at", data)
	}
	if _, ok := data["lifetime_cap_at"]; !ok {
		t.Errorf("run.kill data = %v, want lifetime_cap_at", data)
	}
	if len(f.audit.eventsFor(run.ID, "run.ended")) != 0 {
		t.Error("a run killed at its lifetime was also audited run.ended")
	}
}

// TestLease_BackgroundAtItsEndIsKilledEndReached: a Background task whose end
// passed before its cap is KILLED end_reached, the state and the reason written
// by the one compare-and-set. A late sweep, after the cap too, still says which
// came first.
func TestLease_BackgroundAtItsEndIsKilledEndReached(t *testing.T) {
	for name, cap := range map[string]time.Duration{"cap still ahead": time.Hour, "late sweep past the cap": -time.Minute} {
		t.Run(name, func(t *testing.T) {
			f := newLifetimeFixture(t)
			run := f.run(t, types.ExperienceBackground, dur(-time.Hour), dur(cap))
			f.sweep(t)
			f.requireKilledAtLifetime(t, run, types.RunEndEndReached)
			if got := f.get(t, run.ID); got.LostAt != nil {
				t.Errorf("lost_at = %v; a killed run is not also marked kept", got.LostAt)
			}
		})
	}
}

// TestLease_BackgroundAtTheCapIsKilledMaxLifetimeReached: at its cap a Background
// task is KILLED max_lifetime_reached, whether its end is the cap or it has none.
func TestLease_BackgroundAtTheCapIsKilledMaxLifetimeReached(t *testing.T) {
	for name, end := range map[string]*time.Duration{"end is the cap": dur(-time.Second), "no end": nil} {
		t.Run(name, func(t *testing.T) {
			f := newLifetimeFixture(t)
			run := f.run(t, types.ExperienceBackground, end, dur(-time.Second))
			f.sweep(t)
			f.requireKilledAtLifetime(t, run, types.RunEndMaxLifetimeReached)
		})
	}
}

// TestLease_InteractiveAtItsEndIsKept: an Interactive run at an end before its cap
// is kept exactly as before, and so is a run from an older client (no stored
// experience); a kept run keeps its files grace through its cap, and is torn
// down when the grace runs out.
func TestLease_InteractiveAtItsEndIsKept(t *testing.T) {
	for _, exp := range []types.RunExperience{types.ExperienceInteractive, ""} {
		t.Run("experience "+string(exp), func(t *testing.T) {
			f := newLifetimeFixture(t)
			run := f.run(t, exp, dur(-time.Minute), dur(2*time.Hour))
			f.sweep(t)
			got := f.get(t, run.ID)
			if got.State != types.RunRunning || got.LostAt == nil || got.LostReason != types.LostEnded || got.StatusDetail != "" {
				t.Fatalf("state=%s lost=%v/%q detail=%q; want RUNNING, kept by its end, no end reason", got.State, got.LostAt, got.LostReason, got.StatusDetail)
			}
			if f.rn.endCount() != 1 || f.rn.killCount() != 0 || len(f.audit.eventsFor(run.ID, "run.kill")) != 0 {
				t.Fatalf("ends=%d kills=%d; want the sandbox kept and no kill", f.rn.endCount(), f.rn.killCount())
			}

			// The cap passes while the run is kept: its grace is not cut short.
			f.now = f.now.Add(3 * time.Hour)
			f.sweep(t)
			if got := f.get(t, run.ID); got.State != types.RunRunning || got.StatusDetail != "" || f.rn.killCount() != 0 {
				t.Fatalf("a kept run at its cap: state=%s detail=%q kills=%d; want it left to its grace", got.State, got.StatusDetail, f.rn.killCount())
			}

			// The grace runs out: torn down as today (STOPPED, not KILLED).
			f.now = f.now.Add(8 * 24 * time.Hour)
			f.sweep(t)
			if got := f.get(t, run.ID); got.State != types.RunStopped {
				t.Fatalf("state after the grace = %s, want STOPPED", got.State)
			}
		})
	}
}

// TestLease_InteractiveLiveAtTheCapIsKilled: an Interactive run still live at its
// cap is KILLED max_lifetime_reached, whether its end is the cap, none, or an
// earlier end the sweep never saw.
func TestLease_InteractiveLiveAtTheCapIsKilled(t *testing.T) {
	for name, end := range map[string]*time.Duration{
		"end is the cap":     dur(-time.Second),
		"no end":             nil,
		"earlier unseen end": dur(-time.Hour),
	} {
		t.Run(name, func(t *testing.T) {
			f := newLifetimeFixture(t)
			run := f.run(t, types.ExperienceInteractive, end, dur(-time.Second))
			f.sweep(t)
			f.requireKilledAtLifetime(t, run, types.RunEndMaxLifetimeReached)
		})
	}
}

// TestLease_ARunBeforeItsEndOrCapIsLeftAlone: nothing is killed or kept early.
func TestLease_ARunBeforeItsEndOrCapIsLeftAlone(t *testing.T) {
	f := newLifetimeFixture(t)
	bg := f.run(t, types.ExperienceBackground, dur(time.Hour), dur(2*time.Hour))
	capOnly := f.run(t, types.ExperienceInteractive, nil, dur(time.Hour))
	f.sweep(t)
	for _, id := range []uuid.UUID{bg.ID, capOnly.ID} {
		if got := f.get(t, id); got.State != types.RunRunning || got.LostAt != nil || got.StatusDetail != "" {
			t.Errorf("run %s: state=%s lost=%v detail=%q; want untouched", id, got.State, got.LostAt, got.StatusDetail)
		}
	}
	if f.rn.killCount() != 0 || f.rn.endCount() != 0 {
		t.Errorf("kills=%d ends=%d, want none", f.rn.killCount(), f.rn.endCount())
	}
}

// TestLease_AKilledRunIsNotKilledTwice: a run a person killed first is left alone by
// the lifetime sweep (the compare-and-set lost writes neither state nor reason).
func TestLease_AKilledRunIsNotKilledTwice(t *testing.T) {
	f := newLifetimeFixture(t)
	run := f.run(t, types.ExperienceBackground, dur(-time.Hour), dur(time.Hour))
	if ok, err := f.st.UpdateRunStateIf(context.Background(), run.ID, types.RunRunning, types.RunCompleted); err != nil || !ok {
		t.Fatalf("complete the run: %v %v", ok, err)
	}
	f.srv.killRunAtLifetime(context.Background(), run, types.RunEndEndReached)
	got := f.get(t, run.ID)
	if got.State != types.RunCompleted || got.StatusDetail != "" || f.rn.killCount() != 0 || f.idp.count() != 0 {
		t.Fatalf("state=%s detail=%q kills=%d revokes=%d; want a completed run untouched", got.State, got.StatusDetail, f.rn.killCount(), f.idp.count())
	}
}

// TestLifetimeKill_OfflineRunnerQueuesThePendingKill: a run on a self-hosted
// runner that is offline at its cap is KILLED at the org at once, its credentials
// revoked, and the sandbox stop is a queued pending kill; the kill is audited
// as a success whose sandbox stop is queued.
func TestLifetimeKill_OfflineRunnerQueuesThePendingKill(t *testing.T) {
	f := newAdmissionFixture(t)
	audit := &syncAudit{}
	f.srv.cfg.Audit = audit
	f.srv.cfg.Runner = &routedKillRunner{fakeRunner: &fakeRunner{}, sub: f.srv.runnerEndpoint(f.rn.ID).sub}
	f.srv.cfg.Now = func() time.Time { return time.Now().UTC() }
	capAt := time.Now().UTC().Add(-time.Minute)
	if _, err := f.pool.Exec(f.ctx, `UPDATE agent_runs SET experience='background', lifetime_cap_at=$2, lifetime_cap_source='deployment' WHERE id=$1`, f.run.ID, capAt); err != nil {
		t.Fatal(err)
	}

	if err := f.srv.sweepRunLeases(f.ctx); err != nil {
		t.Fatal(err)
	}
	got, err := f.st.GetRun(f.ctx, f.run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != types.RunKilled || got.StatusDetail != "lifetime: max_lifetime_reached" {
		t.Fatalf("state=%s detail=%q; want KILLED at the org at once", got.State, got.StatusDetail)
	}
	page, _, err := f.st.ListPendingRunnerActions(f.ctx, f.rn.ID)
	if err != nil || len(page) != 1 || page[0].Kind != types.RunnerActionKill || page[0].RunID != f.run.ID {
		t.Fatalf("pending actions = %+v, %v; want the one queued kill", page, err)
	}
	kills := audit.eventsFor(f.run.ID, "run.kill")
	if len(kills) != 1 || kills[0].Outcome != "success" {
		t.Fatalf("run.kill rows = %+v, want one success", kills)
	}
	if data := leaseAuditData(t, kills[0]); data["sandbox_stop"] != "queued" || data["runner_error"] != nil {
		t.Errorf("run.kill data = %v, want sandbox_stop queued and no runner_error", data)
	}
}

// TestDispatch_PodDeadlineFollowsTheRunsOwnCap: the sandbox is created with the
// time the run has left to its own cap (the k8s pods' deadline is built from it),
// and with none for a run that has no cap.
func TestDispatch_PodDeadlineFollowsTheRunsOwnCap(t *testing.T) {
	for _, maxAge := range []time.Duration{0, 48 * time.Hour} {
		fr := &fakeRunner{}
		srv, _ := pgHarnessWithRunner(t, fr)
		srv.cfg.RunMaxAge = maxAge
		got := dispatchAndCaptureSpec(t, srv, fr).Resources.LifetimeCap
		switch {
		case maxAge == 0 && got != 0:
			t.Errorf("a run with no cap was created with lifetime %v, want none", got)
		case maxAge > 0 && (got <= maxAge-time.Hour || got > maxAge):
			t.Errorf("a run capped at %v was created with lifetime %v, want what is left of it", maxAge, got)
		}
	}
}

// TestLease_AStaleSnapshotNeverKillsARunWhoseEndMoved: the sweep lists a run, then a
// person moves its end (or another replica keeps it at its end) before the sweep
// reaches it. The kill's own UPDATE re-checks the reason, so the run stays RUNNING
// with no kill, no teardown and no audit row.
func TestLease_AStaleSnapshotNeverKillsARunWhoseEndMoved(t *testing.T) {
	for name, move := range map[string]string{
		"the end moves to the future": `UPDATE agent_runs SET ends_at = now() + interval '1 hour' WHERE id=$1`,
		"another replica kept it":     `UPDATE agent_runs SET lost_at = now(), lost_reason = 'ended' WHERE id=$1`,
		"the cap moved out":           `UPDATE agent_runs SET lifetime_cap_at = now() + interval '1 day' WHERE id=$1`,
	} {
		t.Run(name, func(t *testing.T) {
			f := newLifetimeFixture(t)
			f.now = time.Now().UTC()
			listed := f.run(t, types.ExperienceBackground, dur(-time.Minute), dur(24*time.Hour))
			if name == "the cap moved out" {
				listed = f.run(t, types.ExperienceInteractive, nil, dur(-time.Minute))
			}
			if _, err := f.st.Pool.Exec(context.Background(), move, listed.ID); err != nil {
				t.Fatal(err)
			}
			f.srv.leaseRun(context.Background(), f.st, listed)
			got := f.get(t, listed.ID)
			if got.State != types.RunRunning || got.StatusDetail != "" {
				t.Fatalf("state=%s detail=%q; want the run left RUNNING", got.State, got.StatusDetail)
			}
			if f.rn.killCount() != 0 || f.idp.count() != 0 || len(f.audit.eventsFor(listed.ID, "run.kill")) != 0 {
				t.Errorf("kills=%d revokes=%d kill rows=%d; want no kill, no teardown, no audit row",
					f.rn.killCount(), f.idp.count(), len(f.audit.eventsFor(listed.ID, "run.kill")))
			}
		})
	}
}

// TestLease_ABackgroundRunKeptAfterAnOutageIsKilledAtItsEnd (N8): a Background run
// kept by an outage is killed end_reached at its end like any Background run; an
// Interactive run kept by an outage is not killed at its end.
func TestLease_ABackgroundRunKeptAfterAnOutageIsKilledAtItsEnd(t *testing.T) {
	f := newLifetimeFixture(t)
	bg := f.run(t, types.ExperienceBackground, dur(-time.Minute), dur(time.Hour))
	inter := f.run(t, types.ExperienceInteractive, dur(-time.Minute), dur(time.Hour))
	for _, id := range []uuid.UUID{bg.ID, inter.ID} {
		if _, err := f.st.Pool.Exec(context.Background(), `UPDATE agent_runs SET lost_at=$2, lost_reason='outage' WHERE id=$1`, id, f.now.Add(-time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	f.sweep(t)
	got := f.get(t, bg.ID)
	if got.State != types.RunKilled || got.StatusDetail != "lifetime: end_reached" {
		t.Errorf("Background outage-kept run: state=%s detail=%q; want KILLED end_reached", got.State, got.StatusDetail)
	}
	if got := f.get(t, inter.ID); got.State == types.RunKilled {
		t.Errorf("Interactive outage-kept run was killed at its end")
	}
}

// TestKill_APersonsKillRecordsAQueuedSandboxStop: a person's kill on an offline
// runner records the sandbox stop as queued, not as a failed step, and the queued
// action is audited as runner.action.request; the runner's receipt is audited later.
func TestKill_APersonsKillRecordsAQueuedSandboxStop(t *testing.T) {
	f := newAdmissionFixture(t)
	audit := &syncAudit{}
	f.srv.cfg.Audit = audit
	f.srv.cfg.Runner = &routedKillRunner{fakeRunner: &fakeRunner{}, sub: f.srv.runnerEndpoint(f.rn.ID).sub}
	run, err := f.st.GetRun(f.ctx, f.run.ID)
	if err != nil {
		t.Fatal(err)
	}
	applied, killData, err := f.srv.killRunCascade(f.ctx, run, types.ActorHuman, "person@example.com", nil)
	if err != nil || !applied {
		t.Fatalf("killRunCascade = %v, %v", applied, err)
	}
	if killData["runner_error"] != nil {
		t.Errorf("kill data = %v; want no runner_error for a queued kill", killData)
	}
	kills := audit.eventsFor(f.run.ID, "run.kill")
	if len(kills) != 1 || kills[0].Outcome != "success" || leaseAuditData(t, kills[0])["sandbox_stop"] != "queued" {
		t.Fatalf("run.kill rows = %+v, want one success with sandbox_stop queued", kills)
	}
	if reqs := audit.eventsFor(f.run.ID, "runner.action.request"); len(reqs) != 1 || reqs[0].Outcome != "success" || leaseAuditData(t, reqs[0])["action"] != "kill" {
		t.Fatalf("runner.action.request rows = %+v, want one for the queued kill", reqs)
	}
}

// TestLease_KillAtTheCapOfAPoolNamesThePool: the run.kill row of a run killed at its pool's lifetime
// carries the pool's name as its source, as run.create wrote it, not "your pool".
func TestLease_KillAtTheCapOfAPoolNamesThePool(t *testing.T) {
	f := newLifetimeFixture(t)
	pool, err := f.st.CreateRunnerPool(context.Background(), types.RunnerPool{ID: uuid.New(), Name: "Night shift", Scope: types.RunnerPoolOrganization}, "test")
	if err != nil {
		t.Fatal(err)
	}
	at := f.now.Add(-time.Second)
	run, err := f.st.CreateRun(context.Background(), types.AgentRun{
		ID: uuid.New(), CreatedAt: f.now.Add(-48 * time.Hour), UpdatedAt: f.now, CreatedBy: "life@example.com",
		Agent: "claude-code", Task: "lifetime", ConfinementClass: types.CC2, State: types.RunRunning,
		RunnerTarget: "docker", SandboxRef: "wardyn-agent-" + uuid.NewString()[:8], Experience: types.ExperienceBackground,
		LifetimeCapAt: &at, LifetimeCapSource: types.LimitSourcePool, RunnerPoolID: &pool.ID,
		RunnerPoolRevision: pool.Revision, RunnerPoolSelection: types.RunnerPoolSelectedExplicit,
	})
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	f.sweep(t)
	kills := f.audit.eventsFor(run.ID, "run.kill")
	if len(kills) != 1 {
		t.Fatalf("run.kill rows = %+v, want one", kills)
	}
	if data := leaseAuditData(t, kills[0]); data["source"] != "Night shift" {
		t.Errorf("run.kill source = %v, want the pool's name", data["source"])
	}
}

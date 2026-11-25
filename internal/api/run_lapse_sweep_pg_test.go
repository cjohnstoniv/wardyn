// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// lapseRunner is lostRunner that also counts sandbox kills.
type lapseRunner struct {
	*lostRunner
	kmu   sync.Mutex
	kills int
}

func (r *lapseRunner) KillSandbox(context.Context, string) error {
	r.kmu.Lock()
	r.kills++
	r.kmu.Unlock()
	return nil
}

func (r *lapseRunner) killCount() int {
	r.kmu.Lock()
	defer r.kmu.Unlock()
	return r.kills
}

// lapseSweepFixture is lifetimeFixture (a Server over real Postgres) with a runner that can cut a proxy.
type lapseSweepFixture struct {
	*lifetimeFixture
	lr *lapseRunner
}

func newLapseSweepFixture(t *testing.T) *lapseSweepFixture {
	t.Helper()
	f := &lapseSweepFixture{lifetimeFixture: newLifetimeFixture(t)}
	code := 0
	f.lr = &lapseRunner{lostRunner: &lostRunner{leaseRunner: f.rn.leaseRunner, exitCode: &code}}
	f.srv.cfg.Runner = f.lr
	return f
}

// serverRun stores a RUNNING run on the server's own Docker, its token last renewed renewedAgo ago.
func (f *lapseSweepFixture) serverRun(t *testing.T, interactive bool, renewedAgo time.Duration) types.AgentRun {
	t.Helper()
	r, err := f.st.CreateRun(context.Background(), types.AgentRun{
		ID: uuid.New(), CreatedAt: f.now.Add(-48 * time.Hour), UpdatedAt: f.now, CreatedBy: "lapse@example.com",
		Agent: "claude-code", Task: "lapse", ConfinementClass: types.CC2, State: types.RunRunning,
		RunnerTarget: "docker", SandboxRef: "wardyn-agent-" + uuid.NewString()[:8], Interactive: interactive,
	})
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	if _, err := f.st.Pool.Exec(context.Background(),
		`UPDATE agent_runs SET token_renewed_at = now() - $2::interval WHERE id = $1`, r.ID, renewedAgo.String()); err != nil {
		t.Fatalf("age the token stamp: %v", err)
	}
	return r
}

func (f *lapseSweepFixture) sweepTokens(t *testing.T) {
	t.Helper()
	if err := f.srv.sweepLapsedRunTokens(context.Background()); err != nil {
		t.Fatalf("sweepLapsedRunTokens: %v", err)
	}
}

// requireKept asserts the run is lost to an outage, still RUNNING, and has a kept run.lost row with the cause.
func (f *lapseSweepFixture) requireKept(t *testing.T, run types.AgentRun) {
	t.Helper()
	got := f.get(t, run.ID)
	if got.State != types.RunRunning || got.LostAt == nil || got.LostReason != types.LostOutage {
		t.Fatalf("run %s: state %s lost %v %q; want RUNNING, lost (outage)", run.ID, got.State, got.LostAt, got.LostReason)
	}
	lost := f.audit.eventsFor(run.ID, "run.lost")
	if len(lost) != 1 {
		t.Fatalf("run %s: run.lost rows = %d, want 1", run.ID, len(lost))
	}
	if d := leaseAuditData(t, lost[0]); d["kept"] != true || d["cause"] != "control_plane_outage" {
		t.Errorf("run %s: run.lost data = %v, want kept:true and cause control_plane_outage", run.ID, d)
	}
}

func (f *lapseSweepFixture) requireNothingEnded(t *testing.T) {
	t.Helper()
	if f.rn.endCount() != 0 || f.rn.stopCount() != 0 || f.lr.killCount() != 0 {
		t.Errorf("EndSandbox %d, StopSandbox %d, KillSandbox %d; want 0, 0, 0", f.rn.endCount(), f.rn.stopCount(), f.lr.killCount())
	}
}

// TestPG_LapseSweep_AfterASeventyMinuteOutageKeepsEveryServerDockerRun is the unit half of test T3.
func TestPG_LapseSweep_AfterASeventyMinuteOutageKeepsEveryServerDockerRun(t *testing.T) {
	f := newLapseSweepFixture(t)
	f.srv.cfg.StoreHealth = recoveringTracker(lapseTestGrace)
	interactive := f.serverRun(t, true, 70*time.Minute)
	headless := f.serverRun(t, false, 70*time.Minute)

	f.sweepTokens(t)

	f.requireKept(t, interactive)
	f.requireKept(t, headless)
	if rows := holdRows(f.audit); len(rows) != 1 {
		t.Fatalf("run.lapse_sweep.hold rows = %d, want 1", len(rows))
	}
	f.requireNothingEnded(t)
}

// TestPG_LapseSweep_NoRunEndsInsideTheGraceAfterRecovery: inside the grace every lapse is kept; past it, with the
// server's other runs renewing, a lone dead proxy ends as at v0.10.2.
func TestPG_LapseSweep_NoRunEndsInsideTheGraceAfterRecovery(t *testing.T) {
	setup := func(t *testing.T, span time.Duration) (*lapseSweepFixture, types.AgentRun) {
		f := newLapseSweepFixture(t)
		for range 9 {
			f.serverRun(t, false, time.Minute)
		}
		run := f.serverRun(t, false, 80*time.Minute) // its token lapsed 15 minutes ago
		f.srv.cfg.StoreHealth = healthyTracker(lapseTestGrace, span)
		return f, run
	}
	t.Run("at 24 minutes every lapse is kept", func(t *testing.T) {
		f, run := setup(t, 24*time.Minute)
		f.sweepTokens(t)
		f.requireKept(t, run)
		f.requireNothingEnded(t)
	})
	t.Run("at 26 minutes the lone dead proxy is ended", func(t *testing.T) {
		f, run := setup(t, 26*time.Minute)
		f.sweepTokens(t)
		if got := f.get(t, run.ID); got.State != types.RunFailed {
			t.Fatalf("state %s, want FAILED: a lone lapse past the grace ends as before", got.State)
		}
		if rows := holdRows(f.audit); len(rows) != 0 {
			t.Errorf("hold rows = %d, want 0", len(rows))
		}
	})
}

// TestPG_LapseSweep_TheLeaseReassertDeletesNothingOnDocker: the lease sweep's re-assert of a held unattended run
// cuts the proxy again and never ends, kills or tears down the sandbox.
func TestPG_LapseSweep_TheLeaseReassertDeletesNothingOnDocker(t *testing.T) {
	f := newLapseSweepFixture(t)
	f.srv.cfg.StoreHealth = recoveringTracker(lapseTestGrace)
	run := f.serverRun(t, false, 70*time.Minute)
	f.sweepTokens(t)
	f.requireKept(t, run)
	cuts := f.lr.proxyStopCount()

	f.sweep(t)
	f.sweep(t)

	if f.lr.proxyStopCount() <= cuts {
		t.Errorf("StopProxy ran %d times after %d: the re-assert must cut the proxy again", f.lr.proxyStopCount(), cuts)
	}
	f.requireNothingEnded(t)
	if got := f.get(t, run.ID); got.State != types.RunRunning {
		t.Errorf("state %s, want RUNNING", got.State)
	}
}

// TestPG_LapseSweep_AKeptUnattendedRunWhoseAgentExitsStaysKept: the watcher sweep never claims a kept run, so an
// agent that exited is not recorded as the run's end.
func TestPG_LapseSweep_AKeptUnattendedRunWhoseAgentExitsStaysKept(t *testing.T) {
	f := newLapseSweepFixture(t)
	f.srv.cfg.StoreHealth = recoveringTracker(lapseTestGrace)
	run := f.serverRun(t, false, 70*time.Minute)
	f.sweepTokens(t)
	f.requireKept(t, run)

	if _, err := f.st.Pool.Exec(context.Background(),
		`UPDATE agent_runs SET watcher_heartbeat = now() - interval '1 hour' WHERE id = $1`, run.ID); err != nil {
		t.Fatalf("stale the watcher heartbeat: %v", err)
	}
	if err := f.srv.sweepRunWatchers(context.Background()); err != nil {
		t.Fatalf("sweepRunWatchers: %v", err)
	}

	got := f.get(t, run.ID)
	if got.State != types.RunRunning || got.LostAt == nil {
		t.Fatalf("state %s lost %v; want RUNNING and still lost", got.State, got.LostAt)
	}
	for _, action := range []string{"run.complete", "run.reconcile"} {
		if hasEvent(f.audit, run.ID, action) {
			t.Errorf("a kept run wrote %s", action)
		}
	}
}

// TestPG_OfflineFromRunner_MarksTheRunnerLinkCause: a run marked lost because its runner disconnected carries
// cause runner_link_lost, with no outage verdict applied.
func TestPG_OfflineFromRunner_MarksTheRunnerLinkCause(t *testing.T) {
	pool, cfg, rn, run, _, _ := runnerExitAPIFixture(t)
	ctx := t.Context()
	cfg.Runner = &offlineContainmentProbe{fakeRunner: &fakeRunner{}}
	if _, err := pool.Exec(ctx, `UPDATE agent_runs SET interactive=false,ends_at=NULL,token_renewed_at=now()-interval '2 hours' WHERE id=$1`, run.ID); err != nil {
		t.Fatal(err)
	}
	srv := New(cfg)
	if err := srv.OfflineFromRunner(ctx, rn.ID); err != nil {
		t.Fatal(err)
	}
	audit, ok := cfg.Audit.(*syncAudit)
	if !ok {
		t.Fatalf("cfg.Audit is %T, want *syncAudit", cfg.Audit)
	}
	lost := audit.eventsFor(run.ID, "run.lost")
	if len(lost) != 1 {
		t.Fatalf("run.lost rows = %d, want 1", len(lost))
	}
	if d := leaseAuditData(t, lost[0]); d["cause"] != "runner_link_lost" {
		t.Errorf("run.lost data = %v, want cause runner_link_lost", d)
	}
}

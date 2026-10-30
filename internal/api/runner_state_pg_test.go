// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/jackc/pgx/v5/pgxpool"
)

type offlineContainmentProbe struct {
	*fakeRunner
	stops atomic.Int32
}

func (p *offlineContainmentProbe) StopProxy(context.Context, string) error {
	p.stops.Add(1)
	return runner.ErrPendingOnRunner
}

func TestRunnerStatePG_OfflineUsesExistingHeadlessLossPath(t *testing.T) {
	pool, cfg, rn, run, _, _ := runnerExitAPIFixture(t)
	ctx := t.Context()
	st := cfg.Store.(store.PG)
	probe := &offlineContainmentProbe{fakeRunner: &fakeRunner{}}
	cfg.Runner = probe
	if _, err := pool.Exec(ctx, `UPDATE agent_runs SET interactive=false,ends_at=NULL WHERE id=$1`, run.ID); err != nil {
		t.Fatal(err)
	}
	srv := New(cfg)
	missing := []runnerwire.RunState{{RunID: run.ID, SandboxPresent: false}}
	if err := finalStatePage(t, srv, rn.ID, missing...); !errors.Is(err, store.ErrRunnerReconciliationRequired) {
		t.Fatalf("fresh missing sandbox admitted: %v", err)
	}
	if err := srv.OfflineFromRunner(ctx, rn.ID); err != nil {
		t.Fatal(err)
	}
	got, _ := st.GetRun(ctx, run.ID)
	if got.LostAt != nil || probe.stops.Load() != 0 {
		t.Fatal("fresh disconnect changed lifecycle")
	}
	if _, err := pool.Exec(ctx, `UPDATE agent_runs SET token_renewed_at=now()-interval '2 hours' WHERE id=$1`, run.ID); err != nil {
		t.Fatal(err)
	}
	if err := srv.OfflineFromRunner(ctx, rn.ID); err != nil {
		t.Fatal(err)
	}
	got, _ = st.GetRun(ctx, run.ID)
	if got.State != types.RunRunning || got.LostAt == nil || got.LostReason != types.LostOutage || probe.stops.Load() != 1 {
		t.Fatalf("headless loss: %+v stops=%d", got, probe.stops.Load())
	}
	if err := srv.OfflineFromRunner(ctx, rn.ID); err != nil {
		t.Fatal(err)
	}
	if err := finalStatePage(t, srv, rn.ID, missing...); err != nil {
		t.Fatalf("lapsed missing sandbox cannot converge: %v", err)
	}
	if err := finalStatePage(t, srv, rn.ID); err != nil {
		t.Fatalf("omitted kept sandbox cannot converge: %v", err)
	}
	if probe.stops.Load() != 1 {
		t.Fatal("offline callback repeated loss effects")
	}
}

// goneAPIRig is runnerExitAPIFixture with no end or lifetime cap and an offline probe as its runner:
// gone is the runner's report of the run's sandbox absent with no exit code, present its twin.
type goneAPIRig struct {
	t             *testing.T
	pool          *pgxpool.Pool
	cfg           Config
	st            store.PG
	rn            types.Runner
	run           types.AgentRun
	probe         *offlineContainmentProbe
	gone, present runnerwire.RunState
}

func newGoneAPIRig(t *testing.T) *goneAPIRig {
	t.Helper()
	pool, cfg, rn, run, exit, _ := runnerExitAPIFixture(t)
	probe := &offlineContainmentProbe{fakeRunner: &fakeRunner{}}
	cfg.Runner = probe
	g := &goneAPIRig{t: t, pool: pool, cfg: cfg, st: cfg.Store.(store.PG), rn: rn, run: run, probe: probe,
		gone: runnerwire.RunState{RunID: run.ID, Ref: exit.Ref}, present: runnerwire.RunState{RunID: run.ID, Ref: exit.Ref, SandboxPresent: true}}
	g.exec(`UPDATE agent_runs SET ends_at=NULL, lifetime_cap_at=NULL WHERE id=$1`)
	return g
}

// exec runs q with the run's id as $1.
func (g *goneAPIRig) exec(q string) {
	g.t.Helper()
	if _, err := g.pool.Exec(g.t.Context(), q, g.run.ID); err != nil {
		g.t.Fatal(err)
	}
}

func (g *goneAPIRig) stamped() bool {
	g.t.Helper()
	var stamped bool
	if err := g.pool.QueryRow(g.t.Context(), `SELECT sandbox_absent_at IS NOT NULL FROM agent_runs WHERE id=$1`, g.run.ID).Scan(&stamped); err != nil {
		g.t.Fatal(err)
	}
	return stamped
}

// confirm sends two absent reports from fresh sessions more than store.RunnerSandboxGoneAfter apart;
// each refuses READY.
func (g *goneAPIRig) confirm(srv *Server) {
	g.t.Helper()
	if err := finalStatePage(g.t, srv, g.rn.ID, g.gone); !errors.Is(err, store.ErrRunnerReconciliationRequired) {
		g.t.Fatalf("first absent report admitted: %v", err)
	}
	g.exec(`UPDATE agent_runs SET sandbox_absent_at=sandbox_absent_at-interval '31 seconds' WHERE id=$1`)
	if err := finalStatePage(g.t, srv, g.rn.ID, g.gone); !errors.Is(err, store.ErrRunnerReconciliationRequired) {
		g.t.Fatalf("confirming absent report admitted: %v", err)
	}
}

func (g *goneAPIRig) lostEvents() []types.AuditEvent {
	return g.cfg.Audit.(*syncAudit).eventsFor(g.run.ID, "run.lost")
}

// awaitGoneEnd polls (no goroutine of its own) until the run is FAILED with its run.lost row, then
// checks it was ended lost (gone) with exactly one run.lost {reason: gone, kept: false} success row.
func (g *goneAPIRig) awaitGoneEnd() {
	g.t.Helper()
	var got types.AgentRun
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		var err error
		if got, err = g.st.GetRun(g.t.Context(), g.run.ID); err != nil {
			g.t.Fatal(err)
		}
		if got.State == types.RunFailed && len(g.lostEvents()) > 0 {
			break
		}
		if time.Now().After(deadline) {
			g.t.Fatalf("gone run not ended: state=%s lost_reason=%q", got.State, got.LostReason)
		}
	}
	g.checkGoneEnd(got)
}

func (g *goneAPIRig) checkGoneEnd(got types.AgentRun) {
	g.t.Helper()
	if got.State != types.RunFailed || got.LostReason != types.LostGone {
		g.t.Fatalf("gone end: state=%s lost_reason=%q", got.State, got.LostReason)
	}
	if got.FailureHint != goneRunFailureHint {
		g.t.Fatalf("gone end: failure_hint=%q, want %q", got.FailureHint, goneRunFailureHint)
	}
	rows := g.lostEvents()
	if len(rows) != 1 {
		g.t.Fatalf("run.lost rows = %d, want 1: %+v", len(rows), rows)
	}
	if data := leaseAuditData(g.t, rows[0]); rows[0].Outcome != "success" || data["reason"] != "gone" || data["kept"] != false {
		g.t.Fatalf("run.lost row: %+v %v", rows[0], data)
	}
	if n := g.probe.stops.Load(); n != 0 {
		g.t.Fatalf("StopProxy called %d times for a gone run", n)
	}
}

func TestRunnerStatePG_GoneSandboxEndsRunFailed(t *testing.T) {
	for name, setup := range map[string]string{
		"interactive": `UPDATE agent_runs SET interactive=true WHERE id=$1`,
		"headless":    `UPDATE agent_runs SET interactive=false WHERE id=$1`,
		"macos_vm":    `UPDATE agent_runs SET machine_kind='macos_vm' WHERE id=$1`,
	} {
		t.Run(name, func(t *testing.T) {
			g := newGoneAPIRig(t)
			g.exec(setup)
			g.cfg.BaseCtx = t.Context()
			srv := New(g.cfg)
			g.confirm(srv)
			g.awaitGoneEnd()
			if err := finalStatePage(t, srv, g.rn.ID); err != nil {
				t.Fatalf("runner not READY after the gone end: %v", err)
			}
		})
	}
}

func TestRunnerStatePG_PresentMacOSVMRunIsKept(t *testing.T) {
	g := newGoneAPIRig(t)
	ctx := t.Context()
	g.exec(`UPDATE agent_runs SET machine_kind='macos_vm' WHERE id=$1`)
	srv := New(g.cfg)
	if err := finalStatePage(t, srv, g.rn.ID, g.present); err != nil || g.stamped() {
		t.Fatalf("present macOS VM run: %v stamped=%v", err, g.stamped())
	}
	g.exec(`UPDATE agent_runs SET sandbox_absent_at=now()-interval '31 seconds' WHERE id=$1`)
	if err := finalStatePage(t, srv, g.rn.ID, g.present); err != nil || g.stamped() {
		t.Fatalf("present report after a stamp: %v stamped=%v", err, g.stamped())
	}
	if err := srv.OfflineFromRunner(ctx, g.rn.ID); err != nil {
		t.Fatal(err)
	}
	got, err := g.st.GetRun(ctx, g.run.ID)
	if err != nil || got.State != types.RunRunning || got.LostAt != nil || len(g.lostEvents()) != 0 {
		t.Fatalf("present run changed: %+v %v run.lost=%d", got, err, len(g.lostEvents()))
	}
}

func TestRunnerStatePG_GoneOutageKeptRunEnds(t *testing.T) {
	g := newGoneAPIRig(t)
	g.exec(`UPDATE agent_runs SET lost_at=now()-interval '2 hours', lost_reason='outage', token_renewed_at=now()-interval '3 hours' WHERE id=$1`)
	g.cfg.BaseCtx = t.Context()
	srv := New(g.cfg)
	g.confirm(srv)
	g.awaitGoneEnd()
}

func TestRunnerStatePG_OfflineSweepEndsGoneRunOnce(t *testing.T) {
	g := newGoneAPIRig(t)
	ctx := t.Context()
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	g.cfg.BaseCtx = cancelled
	srv := New(g.cfg)
	g.confirm(srv)
	for range 2 {
		if err := srv.OfflineFromRunner(ctx, g.rn.ID); err != nil {
			t.Fatal(err)
		}
		got, err := g.st.GetRun(ctx, g.run.ID)
		if err != nil {
			t.Fatal(err)
		}
		g.checkGoneEnd(got)
	}
}

// endingProbe is an offlineContainmentProbe that can keep an ended sandbox (runner.SandboxEnder), so
// the lease sweep's kept-run arm settles a lost run instead of tearing it down as ErrEndUnsupported.
type endingProbe struct{ *offlineContainmentProbe }

func (endingProbe) EndSandbox(context.Context, string) error { return nil }

func TestRunLeasePG_GoneMarkWithoutEndIsFinished(t *testing.T) {
	g := newGoneAPIRig(t)
	ctx := t.Context()
	g.exec(`UPDATE agent_runs SET lost_at=now(), lost_reason='gone' WHERE id=$1`)
	g.cfg.Runner = endingProbe{g.probe}
	srv := New(g.cfg)
	if err := srv.sweepRunLeases(ctx); err != nil {
		t.Fatal(err)
	}
	got, err := g.st.GetRun(ctx, g.run.ID)
	if err != nil {
		t.Fatal(err)
	}
	g.checkGoneEnd(got)
}

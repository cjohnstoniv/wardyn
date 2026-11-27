// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The lost and lease fixtures' runners and stores with MCP sandboxes.
type (
	mcpLostRunner struct {
		*lostRunner
		*mcpSandboxerFake
	}
	mcpGenRunner struct {
		*genStopRunner
		*mcpSandboxerFake
		stopErr error
	}
	mcpLeaseRunner struct {
		*leaseRunner
		*mcpSandboxerFake
	}
	mcpTailRunner struct {
		*finalizeTailRunner
		*mcpSandboxerFake
	}
	mcpLostStore struct {
		*lostStore
		*mcpRowsMem
	}
	mcpGenLostStore struct {
		*genLostStore
		*mcpRowsMem
	}
	mcpLeaseStore struct {
		*leaseStore
		*mcpRowsMem
	}
	mcpTailStore struct {
		*dispatchTestStore
		*mcpRowsMem
	}
)

// StopProxyAt fails when stopErr is set, as a runner that cannot cut the proxy answers.
func (r *mcpGenRunner) StopProxyAt(ctx context.Context, ref string, gen int) error {
	if r.stopErr != nil {
		return r.stopErr
	}
	return r.genStopRunner.StopProxyAt(ctx, ref, gen)
}

// mcpTwoServers is two rows up, ready with live sandboxes, for runID.
func mcpTwoServers(runID uuid.UUID) (*mcpRowsMem, *mcpSandboxerFake) {
	rows := newMCPRowsMem(runID, "files", "search")
	fake := newMCPSandboxerFake()
	fake.startAll(rows.all())
	return rows, fake
}

// assertMCPRowsEnded checks every row is stopped with cause, one run.mcp_sandbox.end row was written for
// each, no sandbox is left and nothing was created.
func assertMCPRowsEnded(t *testing.T, audit *syncAudit, runID uuid.UUID, rows *mcpRowsMem, fake *mcpSandboxerFake, cause string) {
	t.Helper()
	for _, r := range rows.all() {
		if r.State != store.MCPSandboxStopped || r.LastCause != cause || r.StoppedAt == nil {
			t.Errorf("row %s = %s (%q, stopped_at %v), want stopped with cause %q", r.ServerKey, r.State, r.LastCause, r.StoppedAt, cause)
		}
	}
	ends := audit.eventsFor(runID, "run.mcp_sandbox.end")
	if len(ends) != len(rows.all()) {
		t.Errorf("run.mcp_sandbox.end rows = %d, want %d", len(ends), len(rows.all()))
	}
	for _, ev := range ends {
		var data map[string]any
		_ = json.Unmarshal(ev.Data, &data)
		if ev.Outcome != "success" || data["cause"] != cause {
			t.Errorf("run.mcp_sandbox.end = %s %v, want success with cause %q", ev.Outcome, data, cause)
		}
	}
	if n := fake.left(); n != 0 {
		t.Errorf("%d MCP sandboxes left after the end, want 0", n)
	}
	if n := fake.count("create"); n != 0 {
		t.Errorf("%d MCP creates after the end, want 0", n)
	}
}

// TestLostRunEndsMCPSandboxes is I8 on the lost and lease-ended paths: an outage loss and a reboot loss
// (loseRunStopped), the lease's end that keeps the run (endRun), and the two arms that cut a run whose lost
// mark could not be written (containUnwrittenLoss, loseUnconfirmedRun), each of those once with the agent's
// stop failing, end every MCP row with its cause. An outage whose kills fail leaves the rows failed, and the
// watcher's next pass kills the sandboxes. The last row reads no rows and kills by listing.
func TestLostRunEndsMCPSandboxes(t *testing.T) {
	lost := func(t *testing.T, act func(f *lostFixture)) (*lostFixture, *mcpRowsMem, *mcpSandboxerFake) {
		f := newLostFixture(t)
		rows, fake := mcpTwoServers(f.run.ID)
		f.srv.cfg.Store = mcpLostStore{lostStore: f.ls, mcpRowsMem: rows}
		f.srv.cfg.Runner = mcpLostRunner{lostRunner: f.lr, mcpSandboxerFake: fake}
		act(f)
		return f, rows, fake
	}
	t.Run("outage", func(t *testing.T) {
		f, rows, fake := lost(t, func(f *lostFixture) {
			f.srv.lostCuts.Store(f.run.ID, 0) // an earlier unwritten mark's cut: the applied mark is the guard now
			f.ls.lapsed = true
			f.sweepTokens(t)
		})
		if lostAt, reason := f.st.lost(); lostAt == nil || reason != types.LostOutage {
			t.Fatalf("lost = %v %q, want outage", lostAt, reason)
		}
		assertMCPRowsEnded(t, f.audit, f.run.ID, rows, fake, mcpEndLost)
		if _, held := f.srv.lostCuts.Load(f.run.ID); held {
			t.Error("the applied lost mark left this replica's cut entry")
		}
	})
	t.Run("outage, a kill fails once", func(t *testing.T) {
		f := newLostFixture(t)
		rows, fake := mcpTwoServers(f.run.ID)
		fake.killErr = errors.New("the daemon did not answer")
		f.srv.cfg.Store = mcpLostStore{lostStore: f.ls, mcpRowsMem: rows}
		f.srv.cfg.Runner = mcpLostRunner{lostRunner: f.lr, mcpSandboxerFake: fake}
		f.ls.lapsed = true
		f.sweepTokens(t)
		for _, r := range rows.all() {
			if r.State != store.MCPSandboxFailed || r.LastCause != mcpEndLost {
				t.Fatalf("row %s = %s (%q), want failed (lost) while its kill fails", r.ServerKey, r.State, r.LastCause)
			}
		}
		if fake.left() != 2 {
			t.Fatalf("%d sandboxes left, want both while their kills fail", fake.left())
		}
		fake.mu.Lock()
		fake.killErr = nil
		fake.mu.Unlock()
		if f.srv.mcpWatchPass(context.Background(), f.run.ID) {
			t.Fatal("the watcher ended on a lost run")
		}
		if fake.left() != 0 || fake.count("create") != 0 {
			t.Errorf("left %d creates %d after the watcher's pass, want every sandbox killed and none made", fake.left(), fake.count("create"))
		}
	})
	t.Run("reboot", func(t *testing.T) {
		f, rows, fake := lost(t, func(f *lostFixture) { f.sweepWatchers(t) })
		if lostAt, reason := f.st.lost(); lostAt == nil || reason != types.LostReboot {
			t.Fatalf("lost = %v %q, want reboot", lostAt, reason)
		}
		assertMCPRowsEnded(t, f.audit, f.run.ID, rows, fake, mcpEndLost)
	})
	t.Run("lease end keeps the run", func(t *testing.T) {
		f := newLeaseFixture(t, -time.Minute)
		rows, fake := mcpTwoServers(f.run.ID)
		f.srv.cfg.Store = mcpLeaseStore{leaseStore: f.st, mcpRowsMem: rows}
		f.srv.cfg.Runner = mcpLeaseRunner{leaseRunner: f.rn, mcpSandboxerFake: fake}
		f.sweep(t)
		if lostAt, reason := f.st.lost(); lostAt == nil || reason != types.LostEnded || f.st.State() != types.RunRunning {
			t.Fatalf("lost = %v %q, state %s; want the run kept RUNNING and marked ended", lostAt, reason, f.st.State())
		}
		assertMCPRowsEnded(t, f.audit, f.run.ID, rows, fake, mcpEndEnded)
	})
	for _, stopFails := range []bool{false, true} {
		unwritten := func(t *testing.T, act func(f *lostFixture) error) {
			f, gr, gs := newGenStopFixture(t, 3)
			gs.markErr = errors.New("database blip")
			rows, fake := mcpTwoServers(f.run.ID)
			f.srv.cfg.Store = mcpGenLostStore{genLostStore: gs, mcpRowsMem: rows}
			r := &mcpGenRunner{genStopRunner: gr, mcpSandboxerFake: fake}
			if stopFails {
				r.stopErr = errors.New("the runner did not answer")
			}
			f.srv.cfg.Runner = r
			_ = act(f)
			assertMCPRowsEnded(t, f.audit, f.run.ID, rows, fake, mcpEndLost)
			if _, cut := f.srv.lostCuts.Load(f.run.ID); !cut {
				t.Error("the cut was not recorded in the cut set")
			}
		}
		name := map[bool]string{false: "", true: ", stop fails"}[stopFails]
		t.Run("containUnwrittenLoss"+name, func(t *testing.T) {
			unwritten(t, func(f *lostFixture) error { _, err := f.srv.reloseRun(context.Background(), f.run, 3); return err })
		})
		t.Run("loseUnconfirmedRun"+name, func(t *testing.T) {
			unwritten(t, func(f *lostFixture) error {
				_, err := f.srv.loseUnconfirmedRun(context.Background(), f.run, 3)
				return err
			})
		})
	}
	t.Run("row read fails", func(t *testing.T) {
		f, rows, fake := lost(t, func(f *lostFixture) {})
		rows.listErr = errors.New("database blip")
		f.ls.lapsed = true
		f.sweepTokens(t)
		if fake.killAll != 1 || fake.left() != 0 {
			t.Errorf("KillRunMCPSandboxes = %d, sandboxes left %d; want 1 and 0", fake.killAll, fake.left())
		}
		if !f.audit.has(f.run.ID, "run.mcp_sandbox.end", "failure") {
			t.Error("no run.mcp_sandbox.end failure row")
		}
	})
}

// mcpTailFixture is a run of the finalize tail's server with two MCP servers up.
func mcpTailFixture(t *testing.T, state types.RunState) (*Server, *dispatchTestStore, *mcpRowsMem, *mcpSandboxerFake, *syncAudit) {
	t.Helper()
	run := newFinalizeRun()
	run.SandboxRef = "sbx"
	st := &dispatchTestStore{run: run, state: state}
	rows, fake := mcpTwoServers(run.ID)
	audit := &syncAudit{}
	srv := newFinalizeTailServer(t, st, &raceBroker{}, mcpTailRunner{finalizeTailRunner: &finalizeTailRunner{fakeRunner: &fakeRunner{}}, mcpSandboxerFake: fake}, audit)
	srv.cfg.Store = mcpTailStore{dispatchTestStore: st, mcpRowsMem: rows}
	return srv, st, rows, fake, audit
}

// TestRunCompletionEndsMCPRows: the run's end (finalizeRunEffects) ends every MCP row with cause completed
// and clears the run from the cut set.
func TestRunCompletionEndsMCPRows(t *testing.T) {
	srv, st, rows, fake, audit := mcpTailFixture(t, types.RunCompleted)
	srv.lostCuts.Store(st.run.ID, 0)
	_ = srv.finalizeRunEffects(context.Background(), st.run.ID, "sbx", "run.complete", nil, false, false)
	assertMCPRowsEnded(t, audit, st.run.ID, rows, fake, mcpEndCompleted)
	if _, held := srv.lostCuts.Load(st.run.ID); held {
		t.Error("the run's end left it in the cut set")
	}
}

// TestKillCascadeKillsMCPSandboxes: a kill ends every MCP row with cause killed, also for a run killed
// before its agent's sandbox ref was set, and clears the run from the cut set.
func TestKillCascadeKillsMCPSandboxes(t *testing.T) {
	for _, ref := range []string{"sbx", ""} {
		srv, st, rows, fake, audit := mcpTailFixture(t, types.RunKilled)
		run := st.run
		run.SandboxRef = ref
		srv.lostCuts.Store(run.ID, 0)
		srv.killTeardownTail(context.Background(), run, types.ActorHuman, "alice@example.com", nil, false)
		assertMCPRowsEnded(t, audit, run.ID, rows, fake, mcpEndKilled)
		if _, held := srv.lostCuts.Load(run.ID); held {
			t.Errorf("ref %q: the kill left the run in the cut set", ref)
		}
	}
}

// TestKillConfirmWaitsForMCPSandboxes: the kill's background confirm (tryKillConfirm) reaches the agent's
// sandbox only once every MCP sandbox is gone: while one's kill fails it answers busy with the rows up.
func TestKillConfirmWaitsForMCPSandboxes(t *testing.T) {
	srv, st, rows, fake, _ := mcpTailFixture(t, types.RunKilled)
	ctx := context.Background()
	fake.killErr = errors.New("the daemon did not answer")
	if err := srv.tryKillConfirm(ctx, st.run); !errors.Is(err, errKillConfirmBusy) {
		t.Fatalf("confirm with a failing MCP kill = %v, want errKillConfirmBusy", err)
	}
	for _, r := range rows.all() {
		if !mcpRowUp(r) {
			t.Errorf("row %s = %s while its kill fails, want it up", r.ServerKey, r.State)
		}
	}
	fake.killErr = nil
	if err := srv.tryKillConfirm(ctx, st.run); err != nil {
		t.Fatalf("confirm once every MCP kill succeeds = %v, want nil", err)
	}
	for _, r := range rows.all() {
		if r.State != store.MCPSandboxStopped || r.LastCause != mcpEndKilled {
			t.Errorf("row %s = %s (%q), want stopped (killed)", r.ServerKey, r.State, r.LastCause)
		}
	}
}

// TestContainmentWaitsForMCPSandboxes: while an MCP sandbox's kill fails its row stays up, the kill confirm
// answers busy and a terminal run's containment report lists the server (RunContainmentPending.MCPSandboxes); once each kill succeeds the rows are
// stopped, the confirm confirms and the report lists none. A running run's servers are its own, not containment.
func TestContainmentWaitsForMCPSandboxes(t *testing.T) {
	srv, st, rows, fake, _ := mcpTailFixture(t, types.RunKilled)
	run := st.run
	run.State = types.RunKilled
	ctx := context.Background()
	fake.killErr = errors.New("the daemon did not answer")
	if err := srv.confirmMCPSandboxesGone(ctx, run); !errors.Is(err, errKillConfirmBusy) {
		t.Fatalf("confirm with a failing kill = %v, want errKillConfirmBusy", err)
	}
	report := func() []string {
		up, err := srv.mcpSandboxesUpAfterEnd(ctx, run)
		if err != nil {
			t.Fatal(err)
		}
		return up
	}
	if got := report(); len(got) != 2 {
		t.Errorf("pending MCP servers = %v, want both while their kills fail", got)
	}
	fake.killErr = nil
	if err := srv.confirmMCPSandboxesGone(ctx, run); err != nil {
		t.Fatalf("confirm once every kill succeeds = %v, want nil", err)
	}
	for _, r := range rows.all() {
		if r.State != store.MCPSandboxStopped || r.LastCause != mcpEndKilled {
			t.Errorf("row %s = %s (%q), want stopped (killed)", r.ServerKey, r.State, r.LastCause)
		}
	}
	if got := report(); got != nil {
		t.Errorf("pending MCP servers = %v once they are gone, want none", got)
	}
	run.State = types.RunRunning
	srv.cfg.Store = mcpTailStore{dispatchTestStore: st, mcpRowsMem: newMCPRowsMem(run.ID, "files")}
	if up, _ := srv.mcpSandboxesUpAfterEnd(ctx, run); up != nil {
		t.Errorf("a running run's containment lists MCP servers %v", up)
	}
}

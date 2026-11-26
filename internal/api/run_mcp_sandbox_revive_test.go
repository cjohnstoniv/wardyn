// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/db"
	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// mcpSearchRow is a second server that starts from a command, with no secrets.
var mcpSearchRow = types.RunComponent{Ordinal: 1, Name: "Search", MCPHarnesses: []string{"claude-code"},
	Definition: types.ComponentDefinition{Hosts: []string{"api.search.test"}, MCP: &types.ComponentMCP{Transport: types.ComponentMCPTransportStdio,
		Launch: &types.ComponentMCPLaunch{Command: "npx", Args: []string{"-y", "@example/mcp-search@2.0.0"}}}}}

// mcpSearchOwnRow is mcpSearchRow under a policy of its own, which a run's egress edit does not reach.
var mcpSearchOwnRow = mcpRowInMode(mcpSearchRow, types.ComponentMCPSandboxPolicy{Mode: types.ComponentMCPSandboxOwn,
	Spec: &types.RunPolicySpec{AllowedDomains: []string{"api.search.test"}}})

// mcpRowInMode is row with its sandbox policy set to pol; row's own definition is not written.
func mcpRowInMode(row types.RunComponent, pol types.ComponentMCPSandboxPolicy) types.RunComponent {
	mcp := *row.Definition.MCP
	mcp.Sandbox = &types.ComponentMCPSandbox{Policy: pol}
	row.Definition.MCP = &mcp
	return row
}

// newMCPReviveFixture is a run dispatched with its servers ready; two when two is set.
func newMCPReviveFixture(t *testing.T, two bool) *mcpDispatchFixture {
	t.Helper()
	if two {
		return newMCPReviveFixtureOf(t, mcpSearchRow)
	}
	return newMCPReviveFixtureOf(t)
}

// newMCPReviveFixtureOf is a run dispatched with the files server and extra, every one ready.
func newMCPReviveFixtureOf(t *testing.T, extra ...types.RunComponent) *mcpDispatchFixture {
	t.Helper()
	f := newMCPDispatchFixture(t, extra...)
	f.dispatch()
	want := 1 + len(extra)
	if f.state() != types.RunRunning || f.sbx.count("create") != want || f.sbx.left() != want {
		t.Fatalf("dispatch left run %s, creates %d, left %d; want %d up", f.state(), f.sbx.count("create"), f.sbx.left(), want)
	}
	return f
}

func (f *mcpDispatchFixture) current() types.AgentRun {
	r, _ := f.st.dispatchTestStore.GetRun(context.Background(), f.run.ID)
	return r
}

// loseRows is what the lost path leaves: every row ended (lost), every sandbox killed.
func (f *mcpDispatchFixture) loseRows() {
	f.srv.endRunMCPSandboxes(context.Background(), f.run.ID, mcpEndLost)
}

// revivePhase runs a revive's MCP phase under the run's lock, as reviveRunProxy runs it, on ctx and lock.
func (f *mcpDispatchFixture) revivePhase(t *testing.T, ctx, lock context.Context, rebooted, edit bool) {
	t.Helper()
	_, unlock, ok := f.srv.tryLockRunOp(context.Background(), f.run.ID)
	if !ok {
		t.Fatal("take the run lock")
	}
	defer unlock()
	mcpCtx, end := reviveMCPContext(ctx, lock)
	defer end()
	f.srv.reviveMCPSandboxes(mcpCtx, f.current(), 0, rebooted, edit)
}

func (f *mcpDispatchFixture) revive(t *testing.T, rebooted, edit bool) {
	f.revivePhase(t, context.Background(), context.Background(), rebooted, edit)
}

// assertNoneUp: no row is starting or ready, and no sandbox of the run is left.
func (f *mcpDispatchFixture) assertNoneUp(t *testing.T) {
	t.Helper()
	for _, r := range f.st.mcpRowsMem.all() {
		if r.State == store.MCPSandboxStarting || r.State == store.MCPSandboxReady {
			t.Errorf("row %s = %s, want none starting or ready", r.ServerKey, r.State)
		}
	}
	if n := f.sbx.left(); n != 0 {
		t.Errorf("%d MCP sandboxes left, want 0", n)
	}
}

// TestReviveAfterOutageRecreatesMCPSandboxes: an outage-lost run's rows (ended, lost) are each
// re-created by a revive that did not reboot: reopened to starting with a fresh token, the old kept until
// the grace, on the same relay token and pinned address, and settled to ready by the watcher. With the switch
// off the revive is refused naming the server, and nothing is created.
func TestReviveAfterOutageRecreatesMCPSandboxes(t *testing.T) {
	f := newMCPReviveFixture(t, true)
	before := f.st.mcpRowsMem.all()
	first := f.lastMCPSpec()
	f.loseRows()
	if rerr := f.srv.reviveMCPSandboxRefusal(context.Background(), f.current(), false, false, types.ActorHuman, "admin"); rerr != nil {
		t.Fatalf("refusal %+v with the switch on", rerr)
	}
	f.revive(t, false, false)
	if f.sbx.count("create") != 4 || f.sbx.left() != 2 {
		t.Fatalf("creates %d left %d, want both servers re-created", f.sbx.count("create"), f.sbx.left())
	}
	for i, r := range f.st.mcpRowsMem.all() {
		if r.State != store.MCPSandboxStarting || r.SandboxRef == "" || r.CPTokenJTI == before[i].CPTokenJTI ||
			r.PrevCPTokenJTI != before[i].CPTokenJTI || r.RelayAddr != before[i].RelayAddr || r.Restarts != 0 {
			t.Errorf("row %s = %+v, want starting on a fresh token at its pinned address", r.ServerKey, r)
		}
	}
	if again := f.lastMCPSpec(); again.RelayTokenSHA256 == "" || f.sbx.specs[2].RelayTokenSHA256 != first.RelayTokenSHA256 && f.sbx.specs[3].RelayTokenSHA256 != first.RelayTokenSHA256 {
		t.Errorf("the re-created sandboxes moved off their relay tokens")
	}
	if ev := f.audit.eventsFor(f.run.ID, "run.mcp_sandbox.create"); len(ev) != 4 {
		t.Errorf("run.mcp_sandbox.create rows = %d, want 4", len(ev))
	}
	f.pass()
	for _, r := range f.st.mcpRowsMem.all() {
		if r.State != store.MCPSandboxReady {
			t.Errorf("row %s = %s after the settle, want ready", r.ServerKey, r.State)
		}
	}

	t.Run("switch off", func(t *testing.T) {
		f := newMCPReviveFixture(t, false)
		f.loseRows()
		t.Setenv(envMCPOwnSandbox, "")
		rerr := f.srv.reviveMCPSandboxRefusal(context.Background(), f.current(), false, false, types.ActorHuman, "admin")
		if rerr == nil || rerr.status != http.StatusUnprocessableEntity || rerr.reason != reasonComponentDefinitionInvalid ||
			rerr.msg != mcpSandboxSentence(mcpReviveOffSentence, "files", "") {
			t.Fatalf("refusal %+v, want the switch-off refusal naming files", rerr)
		}
		if f.sbx.count("create") != 1 {
			t.Errorf("a refused revive created %d", f.sbx.count("create")-1)
		}
	})
	t.Run("upstream proxy set", func(t *testing.T) {
		f := newMCPReviveFixture(t, false)
		f.loseRows()
		f.st.site = types.SiteConfig{UpstreamProxyURL: "http://proxy.corp.test:3128"}
		rerr := f.srv.reviveMCPSandboxRefusal(context.Background(), f.current(), false, false, types.ActorHuman, "admin")
		if rerr == nil || rerr.reason != reasonMCPOwnSandboxUnsupported {
			t.Fatalf("refusal %+v, want the upstream proxy refusal", rerr)
		}
	})
	t.Run("all up, not rebooted: no refusal and no re-create", func(t *testing.T) {
		f := newMCPReviveFixture(t, false)
		t.Setenv(envMCPOwnSandbox, "")
		if rerr := f.srv.reviveMCPSandboxRefusal(context.Background(), f.current(), false, false, types.ActorHuman, "admin"); rerr != nil {
			t.Fatalf("refusal %+v of a revive that re-creates nothing", rerr)
		}
		t.Setenv(envMCPOwnSandbox, "on")
		f.revive(t, false, false)
		if f.sbx.count("create") != 1 || f.sbx.count("kill") != 0 {
			t.Errorf("creates %d kills %d, want the live server untouched", f.sbx.count("create"), f.sbx.count("kill"))
		}
	})
}

// TestReviveRecreateFailureKeepsRunRunning: a start that fails fails that server only: the run
// stays RUNNING, the row is failed with its cause, nothing it started is left, and a create failure row is
// written.
func TestReviveRecreateFailureKeepsRunRunning(t *testing.T) {
	f := newMCPReviveFixture(t, false)
	f.loseRows()
	f.sbx.mu.Lock()
	f.sbx.startErr = errors.New("engine refused the start")
	f.sbx.mu.Unlock()
	f.revive(t, false, false)
	row := f.st.mcpRowsMem.get(0)
	if f.state() != types.RunRunning || row.State != store.MCPSandboxFailed || row.LastCause != mcpFailStart || f.sbx.left() != 0 {
		t.Fatalf("run %s row %s (%q) left %d", f.state(), row.State, row.LastCause, f.sbx.left())
	}
	if !f.audit.has(f.run.ID, "run.mcp_sandbox.create", "failure") {
		t.Error("no run.mcp_sandbox.create failure row")
	}
}

// TestReviveSucceedsWhileMCPSandboxNotReady: the phase waits for no readiness, so a
// server that never reports ready is left starting, and the watcher fails it (readiness) later; a create
// that blocks until the phase's bound ends fails the row, its failure row written after the bound.
func TestReviveSucceedsWhileMCPSandboxNotReady(t *testing.T) {
	f := newMCPReviveFixture(t, true)
	f.loseRows()
	f.sbx.mu.Lock()
	f.sbx.statusDead = true
	f.sbx.mu.Unlock()
	done := make(chan struct{})
	go func() { f.revive(t, false, false); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the revive's MCP phase waited for readiness")
	}
	for _, r := range f.st.mcpRowsMem.all() {
		if r.State != store.MCPSandboxStarting {
			t.Fatalf("row %s = %s when the phase returned, want starting", r.ServerKey, r.State)
		}
	}
	wait := mcpReadyWait
	mcpReadyWait = -time.Hour // past the wait whatever the fake store's clock reads
	t.Cleanup(func() { mcpReadyWait = wait })
	f.pass()
	for _, r := range f.st.mcpRowsMem.all() {
		if r.State != store.MCPSandboxFailed || r.LastCause != mcpFailReadiness {
			t.Errorf("row %s = %s (%q), want failed readiness", r.ServerKey, r.State, r.LastCause)
		}
	}

	t.Run("create blocks past the bound", func(t *testing.T) {
		f := newMCPReviveFixture(t, false)
		f.loseRows()
		f.sbx.createFn = func(ctx context.Context, _ runner.MCPSandboxSpec) error { <-ctx.Done(); return ctx.Err() }
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		defer cancel()
		f.revivePhase(t, ctx, context.Background(), false, false)
		row := f.st.mcpRowsMem.get(0)
		if row.State != store.MCPSandboxFailed || !f.audit.has(f.run.ID, "run.mcp_sandbox.create", "failure") || f.sbx.left() != 0 {
			t.Fatalf("row %s (%q), create failure row %v, left %d", row.State, row.LastCause,
				f.audit.has(f.run.ID, "run.mcp_sandbox.create", "failure"), f.sbx.left())
		}
	})
}

// TestReviveMCPPhaseInterruptedLeavesNoMCPSandbox: however the phase is cut short,
// no MCP container of the run is left running unrecorded, and no row is starting or ready on a run that was
// killed or lost.
func TestReviveMCPPhaseInterruptedLeavesNoMCPSandbox(t *testing.T) {
	now := time.Now()
	t.Run("lock lost mid-create", func(t *testing.T) {
		f := newMCPReviveFixture(t, false)
		f.loseRows()
		lock, lose := context.WithCancelCause(context.Background())
		f.sbx.createFn = func(ctx context.Context, _ runner.MCPSandboxSpec) error {
			lose(db.ErrLockLost)
			<-ctx.Done()
			return ctx.Err()
		}
		f.revivePhase(t, context.Background(), lock, false, false)
		f.assertNoneUp(t)
		if !f.audit.has(f.run.ID, "run.mcp_sandbox.create", "failure") {
			t.Error("no create failure row for the interrupted create")
		}
	})
	for name, change := range map[string]func(f *mcpDispatchFixture){
		"killed during the first row's create": func(f *mcpDispatchFixture) {
			f.setRun(func(_ *types.AgentRun, s *types.RunState) { *s = types.RunKilled })
		},
		"lost after the first row's reopen": func(f *mcpDispatchFixture) {
			f.setRun(func(r *types.AgentRun, _ *types.RunState) { r.LostAt, r.LostReason = &now, types.LostOutage })
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newMCPReviveFixture(t, true)
			f.loseRows()
			once := false
			f.sbx.createFn = func(context.Context, runner.MCPSandboxSpec) error {
				if !once {
					once = true
					change(f)
				}
				return nil
			}
			f.revive(t, false, false)
			f.assertNoneUp(t)
			if f.sbx.count("create") != 3 {
				t.Errorf("creates %d, want the second row skipped (its reopen false)", f.sbx.count("create")-2)
			}
		})
	}
	t.Run("keep write errors after a start", func(t *testing.T) {
		f := newMCPReviveFixture(t, false)
		f.loseRows()
		f.st.mcpRowsMem.mu.Lock()
		f.st.mcpRowsMem.keepErr = errors.New("database unavailable")
		f.st.mcpRowsMem.mu.Unlock()
		f.revive(t, false, false)
		row := f.st.mcpRowsMem.get(0)
		if f.state() != types.RunRunning || row.State != store.MCPSandboxFailed || row.LastCause != mcpFailStateWrite || f.sbx.left() != 0 {
			t.Fatalf("run %s row %s (%q) left %d: want that server failed state_write, nothing left", f.state(), row.State, row.LastCause, f.sbx.left())
		}
	})
	t.Run("settle racing the revive", func(t *testing.T) {
		f := newMCPReviveFixture(t, true)
		f.loseRows()
		f.sbx.createFn = func(context.Context, runner.MCPSandboxSpec) error { f.pass(); return nil }
		f.revive(t, false, false)
		for _, r := range f.st.mcpRowsMem.all() {
			if r.State != store.MCPSandboxStarting {
				t.Errorf("row %s = %s, want both kept starting", r.ServerKey, r.State)
			}
		}
	})
	t.Run("a person's Stop between the read and the first reopen", func(t *testing.T) {
		f := newMCPReviveFixture(t, true)
		f.sbx.mu.Lock()
		f.sbx.live[0], f.sbx.live[1] = false, false // both crashed: each one's row is ready with a dead sandbox
		f.sbx.mu.Unlock()
		once := false
		f.sbx.killFn = func(id runner.MCPSandboxID) {
			if !once && id.Ordinal == 0 {
				once = true
				_, _ = f.st.StopMCPSandboxRow(context.Background(), f.run.ID, 0, mcpEndPerson)
			}
		}
		f.revive(t, false, false)
		first, second := f.st.mcpRowsMem.get(0), f.st.mcpRowsMem.get(1)
		if first.State != store.MCPSandboxStopped || first.LastCause != mcpEndPerson || second.State != store.MCPSandboxStarting {
			t.Fatalf("first %s (%q) second %s: want the stopped one left and the other re-created", first.State, first.LastCause, second.State)
		}
		if f.sbx.count("create") != 3 {
			t.Errorf("creates %d, want one", f.sbx.count("create")-2)
		}
	})
	t.Run("a person's Stop between the first reopen and its keep", func(t *testing.T) {
		f := newMCPReviveFixture(t, true)
		f.loseRows()
		once := false
		f.sbx.createFn = func(_ context.Context, spec runner.MCPSandboxSpec) error {
			if !once && spec.ID.Ordinal == 0 {
				once = true
				_, _ = f.st.StopMCPSandboxRow(context.Background(), f.run.ID, 0, mcpEndPerson)
			}
			return nil
		}
		f.revive(t, false, false)
		first, second := f.st.mcpRowsMem.get(0), f.st.mcpRowsMem.get(1)
		if first.State != store.MCPSandboxStopped || first.LastCause != mcpEndPerson || second.State != store.MCPSandboxStarting || f.sbx.left() != 1 {
			t.Fatalf("first %s (%q) second %s left %d", first.State, first.LastCause, second.State, f.sbx.left())
		}
		if f.audit.has(f.run.ID, "run.mcp_sandbox.end", "success") && len(f.audit.eventsFor(f.run.ID, "run.mcp_sandbox.end")) > 2 {
			t.Errorf("end rows %d: a row was ended past the loss's two", len(f.audit.eventsFor(f.run.ID, "run.mcp_sandbox.end")))
		}
	})
	t.Run("a reopen that errors", func(t *testing.T) {
		f := newMCPReviveFixture(t, true)
		f.loseRows()
		f.st.mcpRowsMem.mu.Lock()
		f.st.mcpRowsMem.reopenErr = errors.New("database unavailable")
		f.st.mcpRowsMem.mu.Unlock()
		f.revive(t, false, false)
		if f.sbx.count("create") != 2 {
			t.Fatalf("creates %d after a reopen error, want none", f.sbx.count("create")-2)
		}
		for _, r := range f.st.mcpRowsMem.all() {
			if r.State != store.MCPSandboxStopped || r.LastCause != mcpEndLost {
				t.Errorf("row %s = %s (%q), want as the loss left it", r.ServerKey, r.State, r.LastCause)
			}
		}
	})
	t.Run("a cut on this replica after the keep", func(t *testing.T) {
		f := newMCPReviveFixture(t, true)
		f.loseRows()
		f.sbx.createFn = func(context.Context, runner.MCPSandboxSpec) error { f.srv.lostCuts.Store(f.run.ID, 0); return nil }
		f.revive(t, false, false)
		f.assertNoneUp(t)
		if f.sbx.count("create") != 3 {
			t.Errorf("creates %d, want the phase stopped after the first", f.sbx.count("create")-2)
		}
	})
}

// TestReviveRefusedOrLockLostLeavesNoMCPSandbox: a phase whose lock is already lost
// creates nothing, and a cut under an unwritten mark that no revive cleared still keeps the watcher from
// creating.
func TestReviveRefusedOrLockLostLeavesNoMCPSandbox(t *testing.T) {
	t.Run("lock lost before the phase", func(t *testing.T) {
		f := newMCPReviveFixture(t, false)
		f.loseRows()
		lock, lose := context.WithCancelCause(context.Background())
		lose(db.ErrLockLost)
		f.revivePhase(t, context.Background(), lock, true, false)
		if f.sbx.count("create") != 1 {
			t.Fatalf("creates %d under a lost lock", f.sbx.count("create")-1)
		}
	})
	t.Run("unwritten mark kept", func(t *testing.T) {
		f := newMCPReviveFixture(t, false)
		f.srv.lostCuts.Store(f.run.ID, 0)
		f.crash()
		f.pass()
		if _, ok := f.srv.lostCuts.Load(f.run.ID); !ok || f.sbx.count("create") != 1 {
			t.Fatalf("cut entry kept %v, creates %d", ok, f.sbx.count("create"))
		}
	})
}

// TestReviveReplacesMCPProxyConfig: a rebooted revive re-creates even a live, ready server, with a
// fresh token and a freshly sealed config.
func TestReviveReplacesMCPProxyConfig(t *testing.T) {
	f := newMCPReviveFixture(t, false)
	old := f.st.mcpRowsMem.get(0)
	f.revive(t, true, false)
	row := f.st.mcpRowsMem.get(0)
	if f.sbx.count("create") != 2 || f.sbx.left() != 1 || row.CPTokenJTI == old.CPTokenJTI || string(row.ProxyConfigSealed) == string(old.ProxyConfigSealed) {
		t.Fatalf("creates %d left %d, jti moved %v, config resealed %v", f.sbx.count("create"), f.sbx.left(),
			row.CPTokenJTI != old.CPTokenJTI, string(row.ProxyConfigSealed) != string(old.ProxyConfigSealed))
	}
	var pc proxy.Config
	if err := json.Unmarshal(f.lastMCPSpec().ProxyConfig, &pc); err != nil || pc.ControlPlaneAudience != mcpSandboxAudience {
		t.Errorf("config %v audience %q", err, pc.ControlPlaneAudience)
	}
}

// keepAgentPolicy rewrites the agent's kept proxy config with fn applied to its policy, as a live edit
// leaves it before the MCP phase.
func (f *mcpDispatchFixture) keepAgentPolicy(t *testing.T, fn func(p *types.RunPolicySpec)) {
	t.Helper()
	b, err := f.srv.loadRunProxyConfig(context.Background(), f.run.ID)
	if err != nil {
		t.Fatal(err)
	}
	var pc proxy.Config
	if err := json.Unmarshal(b, &pc); err != nil {
		t.Fatal(err)
	}
	fn(&pc.Policy)
	if b, err = json.Marshal(pc); err != nil {
		t.Fatal(err)
	}
	if err := f.srv.storeRunProxyConfig(context.Background(), f.run.ID, b); err != nil {
		t.Fatal(err)
	}
}

// TestLiveEgressEditKeepsMCPSessions: an edit that changes no server's digest touches no row. A "same"
// server copies the run's egress, so an edit that denies a host changes its digest and re-creates it, which
// replaces its session; an "own" server holds a policy of its own, which a run's deny does not reach, so the
// edit leaves it, and its session, as they were. An edit never re-creates a stopped row.
func TestLiveEgressEditKeepsMCPSessions(t *testing.T) {
	f := newMCPReviveFixtureOf(t, mcpSearchOwnRow)
	f.revive(t, false, true)
	if f.sbx.count("kill") != 0 || f.sbx.count("create") != 2 {
		t.Fatalf("an edit that changed no digest: kills %d creates %d", f.sbx.count("kill"), f.sbx.count("create"))
	}
	search := f.st.mcpRowsMem.get(1)
	f.keepAgentPolicy(t, func(p *types.RunPolicySpec) { p.DeniedDomains = append(p.DeniedDomains, "api.files.test") })
	f.revive(t, false, true)
	if f.sbx.count("create") != 3 || f.st.mcpRowsMem.get(0).State != store.MCPSandboxStarting {
		t.Fatalf("creates %d, files row %s: want the same server re-created", f.sbx.count("create"), f.st.mcpRowsMem.get(0).State)
	}
	if got := f.st.mcpRowsMem.get(1); !got.UpdatedAt.Equal(search.UpdatedAt) || got.State != store.MCPSandboxReady || f.sbx.left() != 2 {
		t.Errorf("own server row %+v left %d: want it and its session untouched", got, f.sbx.left())
	}
	f.srv.endRunMCPSandboxes(context.Background(), f.run.ID, mcpEndPerson)
	f.keepAgentPolicy(t, func(p *types.RunPolicySpec) { p.DeniedDomains = append(p.DeniedDomains, "api.search.test") })
	creates := f.sbx.count("create")
	f.revive(t, false, true)
	if f.sbx.count("create") != creates {
		t.Errorf("an edit re-created a stopped row")
	}
}

// mcpLookupNarrowerRow is a third server, "narrower" with an empty overlay.
var mcpLookupNarrowerRow = func() types.RunComponent {
	r := mcpRowInMode(mcpSearchRow, types.ComponentMCPSandboxPolicy{Mode: types.ComponentMCPSandboxNarrower, Overlay: &types.CeilingOverlay{}})
	r.Ordinal, r.Name = 2, "Lookup"
	return r
}()

// TestMCPLiveDeniesReachEveryMode: a restart's or a revive's policy takes the run's live deny set for every
// mode, "own" included, so a host denied since launch is denied to each server and changes its digest, and
// with the set as it was, each digest is the one its row records.
func TestMCPLiveDeniesReachEveryMode(t *testing.T) {
	f := newMCPReviveFixtureOf(t, mcpSearchOwnRow, mcpLookupNarrowerRow)
	ctx := context.Background()
	b, servers, _, err := f.srv.mcpLiveBase(ctx, f.current())
	if err != nil {
		t.Fatal(err)
	}
	denied := b
	denied.deny = append(slices.Clone(b.deny), "denied.example.test")
	for _, row := range f.st.mcpRowsMem.all() {
		srv := servers[slices.IndexFunc(servers, func(m mcpServer) bool { return m.ordinal == row.Ordinal })]
		mode := mcpPolicyModeOf(srv.def)
		if pol, err := f.srv.mcpPolicyAgain(ctx, b, srv, row); err != nil || pol.spec.Digest() != row.PolicyDigest {
			t.Errorf("%s (%s): policy %v, digest moved with no change: re-derive is not stable", row.ServerKey, mode, err)
		}
		pol, err := f.srv.mcpPolicyAgain(ctx, denied, srv, row)
		if err != nil {
			t.Fatalf("%s (%s): %v", row.ServerKey, mode, err)
		}
		if !slices.ContainsFunc(pol.spec.DeniedDomains, func(d string) bool { return strings.Contains(d, "denied.example.test") }) ||
			pol.spec.Digest() == row.PolicyDigest {
			t.Errorf("%s (%s): denies %v, digest kept %v: want the live deny in and the digest changed", row.ServerKey, mode,
				pol.spec.DeniedDomains, pol.spec.Digest() == row.PolicyDigest)
		}
	}
}

// TestLiveEditStoppedEarlyFailsChangedServers: an edit's phase whose lock is lost during its first create
// stops, and the server it did not reach, whose policy the edit changed, is killed and failed rather than
// left running on the policy the edit narrowed.
func TestLiveEditStoppedEarlyFailsChangedServers(t *testing.T) {
	f := newMCPReviveFixture(t, true)
	f.keepAgentPolicy(t, func(p *types.RunPolicySpec) { p.DeniedDomains = append(p.DeniedDomains, "api.other.test") })
	lock, lose := context.WithCancelCause(context.Background())
	f.sbx.createFn = func(ctx context.Context, _ runner.MCPSandboxSpec) error {
		lose(db.ErrLockLost)
		<-ctx.Done() // the phase's context ends with the lock
		return nil
	}
	f.revivePhase(t, context.Background(), lock, false, true)
	if row := f.st.mcpRowsMem.get(1); row.State != store.MCPSandboxFailed {
		t.Fatalf("search row %s (%q), want failed: the edit changed its policy and never re-created it", row.State, row.LastCause)
	}
	f.sbx.mu.Lock()
	held := f.sbx.held[1]
	f.sbx.mu.Unlock()
	if held || f.sbx.count("create") != 3 {
		t.Errorf("search sandbox held %v, creates %d: want it killed and only files re-created", held, f.sbx.count("create"))
	}
}

// TestReviveEndsErasedComponentsMCPRow: a revive ends the row of a server whose component is gone from the
// run's snapshot (superseded), as a restart does, rather than leave it running under the policy it was made
// under.
func TestReviveEndsErasedComponentsMCPRow(t *testing.T) {
	f := newMCPReviveFixture(t, true)
	f.st.comps[1].Erased = true
	f.revive(t, false, false)
	if row := f.st.mcpRowsMem.get(1); row.State != store.MCPSandboxStopped || row.LastCause != mcpEndReplaced {
		t.Fatalf("erased server's row %s (%q), want stopped (superseded)", row.State, row.LastCause)
	}
	f.sbx.mu.Lock()
	held := f.sbx.held[1]
	f.sbx.mu.Unlock()
	if held || !f.audit.has(f.run.ID, "run.mcp_sandbox.end", "success") {
		t.Errorf("sandbox held %v, end row %v: want it killed and its end written", held, f.audit.has(f.run.ID, "run.mcp_sandbox.end", "success"))
	}
	if row := f.st.mcpRowsMem.get(0); row.State != store.MCPSandboxReady || f.sbx.count("create") != 2 {
		t.Errorf("files row %s creates %d: want the live server untouched", row.State, f.sbx.count("create"))
	}
}

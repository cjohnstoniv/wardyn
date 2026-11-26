// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/secretstore/kek"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The restart of an MCP server's sandbox: the watcher's (a dead sandbox, the settle of a starting row) and a
// person's (the Restart route). Each runs under the run's lock and creates only on a live run.

// The restart window (Q6 A): at most mcpRestartMax restarts by the watcher inside mcpRestartWindow, after
// which the row is stopped.
const (
	mcpRestartMax    = 3
	mcpRestartWindow = 10 * time.Minute
)

// mcpWatchEvery is how often the watcher reads the run's MCP sandboxes. A variable so a test can shorten it.
var mcpWatchEvery = 5 * time.Second

var (
	// errMCPRestartRunEnded is a restart of a run on which mcpCreateAllowed is false, or whose row moved on
	// before the restart's write: nothing was created.
	errMCPRestartRunEnded = errors.New("the run's sandbox stopped")
	// errMCPSandboxUnknownKey is a restart of a key the run has no server for.
	errMCPSandboxUnknownKey = errors.New("the run has no such MCP server")
	// errMCPSandboxStarting is a restart of a row that is still starting: nothing was killed.
	errMCPSandboxStarting = errors.New("the MCP server is still starting")
	// errMCPSandboxStore is a restart whose read or write of the run's MCP servers failed.
	errMCPSandboxStore = errors.New("the run's MCP servers could not be read or written")
)

// mcpCreateAllowed reports whether an MCP sandbox of run may be created now (F1, MCP1R-F1), with the run's
// current policy generation it read: the run, read again, is RUNNING with no lost mark (so not kept), its
// containment is not pending, no revive of it is in flight on this replica, and this replica's cut set does
// not hold it (lostCutHolds). A read error answers false. A run read terminal or lost drops its cut entry: the
// store refuses on every replica from then.
func (s *Server) mcpCreateAllowed(ctx context.Context, run types.AgentRun) (int, bool) {
	cur, err := s.cfg.Store.GetRun(ctx, run.ID)
	if err != nil {
		return 0, false
	}
	if isTerminalRunState(cur.State) || cur.LostAt != nil {
		s.lostCuts.Delete(run.ID)
		return 0, false
	}
	if cur.State != types.RunRunning {
		return 0, false
	}
	if _, busy := s.reviving.Load(run.ID); busy {
		return 0, false
	}
	if pending, err := s.runContainmentPending(ctx, cur); pending || err != nil {
		return 0, false
	}
	gen, err := s.currentPolicyGeneration(ctx, run.ID)
	if err != nil || s.lostCutHolds(run.ID, gen) {
		return gen, false
	}
	return gen, true
}

// currentPolicyGeneration is the run's current policy generation, 0 on a store that keeps none.
func (s *Server) currentPolicyGeneration(ctx context.Context, runID uuid.UUID) (int, error) {
	st, ok := s.cfg.Store.(store.RunPolicyGenerations)
	if !ok {
		return 0, nil
	}
	ps, _, err := st.RunPolicyState(ctx, runID)
	if err != nil {
		return 0, err
	}
	return ps.CurrentGeneration(), nil
}

// lostCutHolds reports whether this replica's cut set holds the run at generation gen (MCP52-F2): an entry
// of generation 0, which no advance clears, or one not older than gen. An older entry is ignored and left in
// place: a revive that abandons its generation puts the older one back as current, and the entry holds again.
func (s *Server) lostCutHolds(runID uuid.UUID, gen int) bool {
	v, ok := s.lostCuts.Load(runID)
	if !ok {
		return false
	}
	g, _ := v.(int)
	return g == 0 || g >= gen
}

// mcpLiveBase is what a restart or a revive derives a server's MCP policy from now: the policy the agent's
// proxy runs under as its kept config holds it (P_run after the last seal), the run's component snapshot, and
// the run's live withholding (its owner's current denies and the outer layers), whose credential rule also
// filters the server's secrets.
func (s *Server) mcpLiveBase(ctx context.Context, run types.AgentRun) (mcpPolicyBase, []mcpServer, withholding, error) {
	cfgJSON, err := s.loadRunProxyConfig(ctx, run.ID)
	if err != nil {
		return mcpPolicyBase{}, nil, withholding{}, fmt.Errorf("read the run's proxy config: %w", err)
	}
	var cfg proxy.Config
	if err := json.Unmarshal(cfgJSON, &cfg); err != nil {
		return mcpPolicyBase{}, nil, withholding{}, fmt.Errorf("decode the run's proxy config: %w", err)
	}
	comps, servers, err := s.mcpRunServers(ctx, run.ID)
	if err != nil {
		return mcpPolicyBase{}, nil, withholding{}, err
	}
	w, err := s.liveWithholding(ctx, run)
	if err != nil {
		return mcpPolicyBase{}, nil, withholding{}, fmt.Errorf("read the run's withholding: %w", err)
	}
	return mcpPolicyBase{run: cfg.Policy, comps: comps, layers: w.layers, deny: w.deny}, servers, w, nil
}

// openMCPProxyConfig is the config row's sandbox was created with.
func (s *Server) openMCPProxyConfig(row store.RunMCPSandbox) (proxy.Config, error) {
	if len(row.ProxyConfigSealed) == 0 || len(s.cfg.RunConfigKey) != kek.DEKSize {
		return proxy.Config{}, errRunProxyConfigNotKept
	}
	b, err := kek.Open(s.cfg.RunConfigKey, row.ProxyConfigSealed, kek.Encode("run", row.RunID.String(), "mcp", row.ServerKey))
	if err != nil {
		return proxy.Config{}, fmt.Errorf("open the MCP proxy's config: %w", err)
	}
	var cfg proxy.Config
	if err := json.Unmarshal(b, &cfg); err != nil {
		return proxy.Config{}, fmt.Errorf("decode the MCP proxy's config: %w", err)
	}
	return cfg, nil
}

// mcpPolicyAgain is a server's MCP policy derived again on b, its size held through mcpSandboxResources to the
// size its sandbox was made with (the sealed config's, CPU, memory and disk; the row's reservation when none
// was kept). Its digest differs from the row's only when something narrowed it. "own" was clamped at dispatch
// to the creator's ceiling, which a restart has no request to read again: the policy its row sealed is its
// base, held to the run's knobs and finished with today's layers and denies, so it can only narrow.
func (s *Server) mcpPolicyAgain(ctx context.Context, b mcpPolicyBase, srv mcpServer, row store.RunMCPSandbox) (mcpPolicy, error) {
	cfg, cerr := s.openMCPProxyConfig(row)
	made := &types.ResourceLimits{CPUMillis: int(row.Reserved.CPUMillis), MemoryMiB: int(row.Reserved.MemoryMiB)}
	if cerr == nil && cfg.Policy.Resources != nil {
		made = cfg.Policy.Resources
	}
	b.ceiling.Limits.MaxCPUMillis, b.ceiling.Limits.MaxMemoryMiB, b.ceiling.Limits.MaxEphemeralDiskMiB = made.CPUMillis, made.MemoryMiB, made.DiskMiB
	if mcpPolicyModeOf(srv.def) != types.ComponentMCPSandboxOwn {
		return s.mcpSandboxPolicy(ctx, b, srv.index)
	}
	if cerr != nil {
		return mcpPolicy{}, cerr
	}
	out := mcpPolicy{mode: types.ComponentMCPSandboxOwn, spec: mcpOnlyKnobs(cfg.Policy, b.run)}
	out.spec.Resources = mcpSandboxResources(cfg.Policy.Resources, cfg.Policy.Resources, b.ceiling.Limits)
	out.holds = mcpPolicyFinish(&out.spec, b.layers, b.deny)
	return out, nil
}

// mcpServerGrantsLive is what delivers one server's own secrets now: its env and file secret grants, and the
// header injections of the grants its row holds, each one the run's live withholding still gives.
func (s *Server) mcpServerGrantsLive(srv mcpServer, row store.RunMCPSandbox, grants []types.CredentialGrant, w withholding) mcpServerGrants {
	var g mcpServerGrants
	for _, sec := range srv.def.Secrets {
		spec := componentGrant(sec)
		if w.credentialWithheld(spec) {
			continue
		}
		switch spec.Kind {
		case types.GrantEnvSecret:
			g.env = append(g.env, spec)
		case types.GrantFileSecret:
			g.files = append(g.files, spec)
		}
	}
	for _, cg := range grants {
		if cg.Spec.Kind != types.GrantAPIKey || !slices.Contains(row.GrantIDs, cg.ID) || w.credentialWithheld(cg.Spec) {
			continue
		}
		if rule, err := injectionRuleFromScope(cg.Spec.Scope); err == nil {
			g.inject = append(g.inject, runner.InjectionGrant{GrantID: cg.ID, Rule: rule})
		}
	}
	return g
}

// mcpRecreate is one server's sandbox made again for a restart or a revive: its policy, its own secrets and a
// fresh token and config, on its row's relay token, so the agent's proxy reaches it unchanged, and its rank,
// so it gets its pinned address back.
type mcpRecreate struct {
	srv   mcpServer
	row   store.RunMCPSandbox
	pol   mcpPolicy
	built mcpBuilt
}

// prepareMCPRecreate derives and builds one server's sandbox again. An error creates nothing.
func (s *Server) prepareMCPRecreate(ctx context.Context, run types.AgentRun, b mcpPolicyBase, srv mcpServer, row store.RunMCPSandbox,
	grants []types.CredentialGrant, w withholding) (mcpRecreate, error) {
	pol, err := s.mcpPolicyAgain(ctx, b, srv, row)
	if err != nil {
		return mcpRecreate{}, err
	}
	own, refused := s.resolveMCPServerOwn(ctx, run, srv, s.mcpServerGrantsLive(srv, row, grants, w), grants)
	if refused != "" {
		return mcpRecreate{}, errors.New(refused)
	}
	image := run.Image
	if image == "" {
		return mcpRecreate{}, errors.New("the run records no image")
	}
	built, err := s.buildMCPSandbox(ctx, run, image, srv, pol, own, row.RelayTokenSHA256)
	if err != nil {
		return mcpRecreate{}, err
	}
	return mcpRecreate{srv: srv, row: row, pol: pol, built: built}, nil
}

// createAndStartMCP creates and starts a prepared sandbox and records it. A failure kills what it may have
// left and answers the row's cause. A record the store refuses, because a kill or a person's Stop ended the
// row meanwhile, kills what was created and answers errMCPRestartRunEnded: nothing is started.
func (s *Server) createAndStartMCP(ctx context.Context, run types.AgentRun, rc mcpRecreate) (string, error) {
	st, m, _ := s.mcpSandboxSeams()
	sb, err := m.CreateMCPSandbox(ctx, rc.built.spec)
	if err == nil {
		var recorded bool
		recorded, err = st.RecordMCPSandboxCreated(ctx, run.ID, rc.srv.ordinal, store.MCPSandboxCreated{RelayAddr: sb.RelayAddr,
			PolicyDigest: rc.pol.spec.Digest(), ProxyConfigSealed: rc.built.sealed})
		if err == nil && !recorded {
			err = errMCPRestartRunEnded
		}
		if err != nil {
			_ = m.KillMCPSandbox(ctx, rc.built.spec.ID)
			return mcpFailStateWrite, err
		}
		if err = m.StartMCPSandbox(ctx, rc.built.spec.ID); err != nil {
			_ = m.KillMCPSandbox(ctx, rc.built.spec.ID)
			return mcpCreateCause(err, mcpFailStart), err
		}
		return "", nil
	}
	_ = m.KillMCPSandbox(ctx, rc.built.spec.ID)
	return mcpCreateCause(err, mcpFailCreate), err
}

// restartMCPSandbox restarts the run's server key under the run's lock, which the caller holds (MCP1R-F3):
// mcpCreateAllowed first (false kills what may run and creates nothing), then for the watcher the window
// (past mcpRestartMax in mcpRestartWindow the row is stopped, restart_limit), then the old sandbox killed, the
// row moved to restarting with a fresh token only while it is as read here and the run is live, the sandbox
// created and started, and the row moved to ready only while the run is still live; a false answer kills
// what it started. A hit on the cut set after that write kills it and ends the run's rows (MCP5-F5). A failed
// create or start fails that row alone.
func (s *Server) restartMCPSandbox(ctx context.Context, run types.AgentRun, key string, actorType types.ActorType, actor string, auto bool) error {
	st, m, ok := s.mcpSandboxSeams()
	if !ok {
		return errMCPSandboxUnknownKey
	}
	rows, err := st.ListRunMCPSandboxes(ctx, run.ID)
	if err != nil {
		return fmt.Errorf("%w: %w", errMCPSandboxStore, err)
	}
	i := slices.IndexFunc(rows, func(r store.RunMCPSandbox) bool { return r.ServerKey == key })
	if i < 0 {
		return errMCPSandboxUnknownKey
	}
	row := rows[i]
	id := mcpSandboxIDOf(run, row)
	gen, allowed := s.mcpCreateAllowed(ctx, run)
	if !allowed {
		_ = m.KillMCPSandbox(ctx, id)
		return errMCPRestartRunEnded
	}
	if row.State == store.MCPSandboxStarting {
		return errMCPSandboxStarting // the settle moves it to ready or fails it; a kill now would only end it
	}
	now := s.cfg.Now()
	restarts, window := row.Restarts, row.RestartWindowStart
	if auto {
		if window.IsZero() || now.Sub(window) >= mcpRestartWindow {
			restarts, window = 0, now
		}
		if restarts++; restarts > mcpRestartMax {
			s.endMCPSandbox(ctx, run, row, mcpEndLimit)
			return nil
		}
	}
	b, servers, w, err := s.mcpLiveBase(ctx, run)
	if err != nil {
		return fmt.Errorf("%w: %w", errMCPSandboxStore, err)
	}
	j := slices.IndexFunc(servers, func(m mcpServer) bool { return m.ordinal == row.Ordinal })
	if j < 0 {
		s.endMCPSandbox(ctx, run, row, mcpEndReplaced) // its component is gone from the run's snapshot
		return errMCPSandboxUnknownKey
	}
	grants, err := s.cfg.Store.ListGrantsByRun(ctx, run.ID)
	if err != nil {
		return fmt.Errorf("%w: read the run's grants: %w", errMCPSandboxStore, err)
	}
	rc, err := s.prepareMCPRecreate(ctx, run, b, servers[j], row, grants, w)
	if err != nil {
		s.auditMCPSandboxRestart(ctx, actorType, actor, row, "failure", mcpFailCreate)
		return err
	}
	_ = m.KillMCPSandbox(ctx, id) // the old proxy drains first (P3 item 7)
	if ok, err := st.RestartMCPSandboxIfLive(ctx, run.ID, row.Ordinal, store.MCPSandboxRestart{ReadAt: row.UpdatedAt, JTI: rc.built.jti,
		PrevUntil: now.Add(terminalUploadGrace), Restarts: restarts, WindowStart: window}); err != nil {
		return fmt.Errorf("%w: %w", errMCPSandboxStore, err)
	} else if !ok {
		return errMCPRestartRunEnded
	}
	row.Restarts, row.RestartWindowStart = restarts, window
	if cause, err := s.createAndStartMCP(ctx, run, rc); err != nil {
		slog.WarnContext(ctx, "wardynd: restarting an MCP server's sandbox failed", slog.String("run_id", run.ID.String()),
			slog.String("server", key), slog.Any("err", err))
		_, _ = st.FailMCPSandboxIfUp(ctx, run.ID, row.Ordinal, cause)
		s.auditMCPSandboxRestart(ctx, actorType, actor, row, "failure", cause)
		if errors.Is(err, errMCPRestartRunEnded) {
			return errMCPRestartRunEnded
		}
		return nil
	}
	if ok, err := st.MarkMCPSandboxReadyIfLive(ctx, run.ID, row.Ordinal, mcpSandboxRef(run, row.Ordinal)); err != nil || !ok {
		_ = m.KillMCPSandbox(ctx, id) // the run or the row moved on after the check: what started goes
		return errMCPRestartRunEnded
	}
	if s.lostCutHolds(run.ID, gen) {
		_ = m.KillMCPSandbox(ctx, id)
		s.endRunMCPSandboxes(ctx, run.ID, mcpEndLost)
		return errMCPRestartRunEnded
	}
	s.auditMCPPolicyResolve(ctx, run.ID, row.Ordinal, row.ServerKey, rc.pol)
	s.auditMCPSandboxRestart(ctx, actorType, actor, row, "success", "")
	return nil
}

// settleMCPSandbox is the watcher's settle of a starting row (MCP51-F4), under the run's lock the caller
// holds: a live sandbox moves the row to ready while the run is live (else it is killed and the row left as
// the other writer left it); a sandbox not live mcpReadyWait after the row's last write fails the row,
// readiness, and is killed. It creates nothing, so it does not ask mcpCreateAllowed.
func (s *Server) settleMCPSandbox(ctx context.Context, run types.AgentRun, row store.RunMCPSandbox) {
	st, m, _ := s.mcpSandboxSeams()
	id := mcpSandboxIDOf(run, row)
	sb, err := m.MCPSandboxStatus(ctx, id)
	if err == nil && sb.Live {
		if ok, werr := st.MarkMCPSandboxReadyIfLive(ctx, run.ID, row.Ordinal, mcpSandboxRef(run, row.Ordinal)); werr != nil || !ok {
			_ = m.KillMCPSandbox(ctx, id)
		}
		return
	}
	if s.cfg.Now().Sub(row.UpdatedAt) < mcpReadyWait {
		return
	}
	if ok, _ := st.FailMCPSandboxIfStarting(ctx, run.ID, row.Ordinal, mcpFailReadiness); ok {
		_ = m.KillMCPSandbox(ctx, id)
		s.auditMCPSandboxEnd(ctx, run.ID, types.ActorSystem, "wardynd", &row, "failure", mcpFailReadiness)
	}
}

// startMCPSandboxWatcher starts the run's MCP watcher beside its completion watcher, once per run in this
// process; a run with no MCP sandbox row ends it at its first pass. It runs until the run is terminal.
func (s *Server) startMCPSandboxWatcher(runID uuid.UUID) {
	if _, _, ok := s.mcpSandboxSeams(); !ok {
		return
	}
	if _, dup := s.mcpWatching.LoadOrStore(runID, struct{}{}); dup {
		return
	}
	base := s.cfg.BaseCtx
	if base == nil {
		base = context.Background()
	}
	every := mcpWatchEvery
	go func() {
		defer s.mcpWatching.Delete(runID)
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			select {
			case <-base.Done():
				return
			case <-t.C:
			}
			if s.mcpWatchPass(base, runID) {
				return
			}
		}
	}()
}

// mcpWatchPass is one pass of the watcher: done once the run is terminal or has no rows. A starting row is
// settled, a ready row whose sandbox is dead restarted, and a failed or stopped row whose sandbox is live
// killed, each under the run's lock; a lock it cannot take is a skip, retried at the next pass.
func (s *Server) mcpWatchPass(base context.Context, runID uuid.UUID) bool {
	ctx, cancel := context.WithTimeout(base, mcpSandboxEndTimeout)
	defer cancel()
	st, m, _ := s.mcpSandboxSeams()
	run, err := s.cfg.Store.GetRun(ctx, runID)
	if err != nil {
		return false
	}
	if isTerminalRunState(run.State) {
		return true
	}
	rows, err := st.ListRunMCPSandboxes(ctx, runID)
	if err != nil || len(rows) == 0 {
		return err == nil
	}
	var act []store.RunMCPSandbox
	for _, row := range rows {
		switch row.State {
		case store.MCPSandboxStarting:
			act = append(act, row)
		case store.MCPSandboxReady:
			if sb, serr := m.MCPSandboxStatus(ctx, mcpSandboxIDOf(run, row)); serr == nil && !sb.Live {
				act = append(act, row)
			}
		case store.MCPSandboxFailed, store.MCPSandboxStopped:
			// A failed or stopped row keeps no proxy, so no token of its renews: a kill that failed on the
			// way to it is tried again.
			if sb, serr := m.MCPSandboxStatus(ctx, mcpSandboxIDOf(run, row)); serr == nil && sb.Live {
				act = append(act, row)
			}
		}
	}
	if len(act) == 0 {
		return false
	}
	lctx, unlock, ok := s.tryLockRunOp(ctx, runID)
	if !ok {
		return false
	}
	defer unlock()
	if run, err = s.cfg.Store.GetRun(lctx, runID); err != nil {
		return false
	}
	if rows, err = st.ListRunMCPSandboxes(lctx, runID); err != nil {
		return false
	}
	for _, a := range act {
		i := slices.IndexFunc(rows, func(r store.RunMCPSandbox) bool { return r.Ordinal == a.Ordinal })
		if i < 0 || !rows[i].UpdatedAt.Equal(a.UpdatedAt) {
			continue // another writer moved it since the read: the next pass reads it again
		}
		switch a.State {
		case store.MCPSandboxStarting:
			s.settleMCPSandbox(lctx, run, rows[i])
		case store.MCPSandboxFailed, store.MCPSandboxStopped:
			_ = m.KillMCPSandbox(lctx, mcpSandboxIDOf(run, rows[i]))
		case store.MCPSandboxReady:
			if err := s.restartMCPSandbox(lctx, run, a.ServerKey, types.ActorSystem, "wardynd", true); err != nil && !errors.Is(err, errMCPRestartRunEnded) {
				slog.WarnContext(lctx, "wardynd: the MCP watcher could not restart a server", slog.String("run_id", runID.String()),
					slog.String("server", a.ServerKey), slog.Any("err", err))
			}
		}
	}
	return false
}

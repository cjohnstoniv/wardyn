// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// Start after readback (WARDYN_START_AFTER_READBACK=on, Config.StartAfterReadback). Off, the default, a
// dispatch goes RUNNING once its driver answers, and its proxy's report may land up to proxyReportGrace
// later (run_proxy_readback.go). On, a dispatch holds its run STARTING until that start is enforced. The
// agent's task exec, an attach and every other exec need RUNNING, so none of them runs before the
// readback, on any substrate: the hold is the control plane's, between the create and the first exec.
// It holds a run's first start only. A revive, a restart with current limits or a live egress edit keeps
// the readback's 60 s window: its proxy is replaced under the revive's run lock, whose bound
// (reviveClaimedTimeout) is shorter than the readback's deadline, and its claim reopens attach and exec.
// A start that is not enforced (a store with no readback, a legacy runner or proxy image, a failed,
// superseded or abandoned start, or no report by the wait's end) fails the run with its sandbox stopped.
// What still runs at create is the sandbox's own idle main process (runner.AgentIdleScript, or agent-run
// --idle's workspace prep for an interactive run). An interactive run's boot seed starts the agent from
// that process, so with the hold on it is refused before the create.

// startReadbackPoll is how often a held dispatch reads its start, and startReadbackWait bounds the hold
// past the start's own deadline. Variables so a test can shorten them.
var (
	startReadbackPoll = 500 * time.Millisecond
	startReadbackWait = proxyReportGrace + 15*time.Second
)

// envInteractiveSeed is the sandbox variable an interactive run's boot seed rides (applyDispatchModeEnv).
const envInteractiveSeed = "WARDYN_INTERACTIVE_SEED"

// startAfterReadbackSeedHint fails an interactive run whose task would start its agent as the sandbox boots.
const startAfterReadbackSeedHint = "This deployment starts an agent only after the run's proxy confirms its policy (WARDYN_START_AFTER_READBACK=on), and an interactive run's task starts its agent as the sandbox boots, before that. Start the interactive run without a task, or ask an admin to turn the setting off"

// startAfterReadbackRefused fails, before any create, a run whose sandbox would start its agent at boot
// while the hold is on. true is a run already failed; done is called first, as on every create failure.
func (s *Server) startAfterReadbackRefused(ctx context.Context, run types.AgentRun, spec runner.SandboxSpec, done func()) bool {
	if !s.cfg.StartAfterReadback || spec.Env[envInteractiveSeed] == "" {
		return false
	}
	done()
	s.failAndRevoke(ctx, run.ID, types.RunStarting, startAfterReadbackSeedHint)
	s.recordAudit(ctx, s.auditEvent(&run.ID, types.ActorSystem, "wardynd", "run.create",
		run.ID.String(), "failure", mustJSON(map[string]any{"reason": "policy_unconfirmed", "readback": "boot_seed_before_readback"})))
	s.unoweAgentCast(ctx, run) // no sandbox was created, so it owes no recording
	return true
}

// awaitStartReadback holds a dispatch, after its driver's answer and before its run goes RUNNING, until
// the start is enforced. true at once when the hold is off. false is a run that must not start: one this
// wait failed with its sandbox stopped, one killed or ended while held (its sandbox stopped here, as a
// lost RUNNING write does), or a failed start, which its report's handler, the deadline
// watchdog or the reconcile sweep contains (stopUnconfirmedProxyAt fails a STARTING run).
func (s *Server) awaitStartReadback(ctx context.Context, run types.AgentRun, ref string, start proxyStartRec) bool {
	if !s.cfg.StartAfterReadback {
		return true
	}
	st, ok := s.cfg.Store.(store.RunProxyStarts)
	if !ok || !start.active || start.mode != store.ProxyStartExpected {
		s.abortUnconfirmedSandbox(ctx, run.ID, ref, proxyStartRec{}, "start_unconfirmable", nil)
		return false
	}
	wctx, cancel := context.WithTimeout(ctx, startReadbackWait)
	defer cancel()
	why := "readback_wait_timeout"
	for wctx.Err() == nil {
		p, found, err := st.ActiveProxyStart(wctx, run.ID)
		switch {
		case err != nil: // read again until the wait runs out
		case !found || p.Generation != start.gen:
			why = "start_superseded"
			cancel()
			continue
		case p.State == types.ProxyStartEnforced:
			return true
		case p.State == types.ProxyStartFailed:
			return false
		case p.State == types.ProxyStartPending:
			if s.runLeftStarting(wctx, run.ID) {
				// Killed or ended while held: what dispatch does when its RUNNING write loses, at once.
				s.stopSandboxOrAudit(ctx, run.ID, ref, "run.dispatch")
				s.auditLostStart(ctx, run.ID, ref, nil)
				return false
			}
			s.expireProxyStart(ctx, run.ID, start.gen) // a no-op before the deadline
		default: // unconfirmed, superseded or abandoned: never enforced
			why = "start_" + string(p.State)
			cancel()
			continue
		}
		select {
		case <-wctx.Done():
		case <-time.After(startReadbackPoll):
		}
	}
	s.abortUnconfirmedSandbox(ctx, run.ID, ref, proxyStartRec{}, why, nil)
	return false
}

// runLeftStarting reports whether the run is gone or no longer STARTING. A read that errors otherwise is
// not evidence, and the hold goes on.
func (s *Server) runLeftStarting(ctx context.Context, id uuid.UUID) bool {
	cur, err := s.cfg.Store.GetRun(ctx, id)
	return errors.Is(err, store.ErrNotFound) || (err == nil && cur.State != types.RunStarting)
}

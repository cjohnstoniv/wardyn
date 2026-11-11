// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// A revive's steps after its claim (reviveRunProxy): the generation advance and lock check, a failed replace, the rebooted agent's start, and the fail-closed relose. Moved out of run_revive.go to keep it under scripts/check-file-size.sh's 1000-line gate.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// reviveAdvanceHeld is reviveRunProxy's generation advance after the claim, and the check that its run lock is still held. It returns the generation
// and proxy start the replace acts under, or the revive's refusal.
func (s *Server) reviveAdvanceHeld(ctx, lockCtx context.Context, run types.AgentRun, fate *store.RunLostFate, data map[string]any, actorType types.ActorType, actor string,
	policy types.RunPolicySpec, retiring string, renewed renewedTokens, freshJTI string, expected store.PolicySlot, storedJSON, source []byte, edited bool) (int, proxyStartRec, *reviveError) {
	// The revive's config becomes the run's stored one in the generation's own write, before the new
	// proxy's first row and before the replace: any revive that reads the new generation reads this config.
	gen, start, gerr := s.advanceRevivePolicyAndStart(ctx, run, policy, retiring, renewed, freshJTI, expected, storedJSON, source)
	switch {
	case errors.Is(gerr, store.ErrRunPolicyMoved):
		return 0, proxyStartRec{}, s.revivePolicyMoved(ctx, run, data, actorType, actor, freshJTI, renewed, gerr)
	case gerr != nil:
		// gen is a generation this attempt wrote before the error (abandoned since): the run's newest, and so
		// the one its lost mark is fenced on.
		fence := expected.Generation
		if gen > 0 {
			fence = gen
		}
		return 0, proxyStartRec{}, s.reviveGenerationFailed(ctx, run, fence, data, actorType, actor, retiring, freshJTI, renewed, gerr)
	case edited && gen > 0:
		s.retireViaOrgEngine(run.ID) // the edited config is stored: no engine built from the pre-edit one serves again
	}
	// A hold lost since the claim is a lock another replica may now hold, under which a newer revive may
	// have replaced the proxy: a replace from here would remove that proxy. The old (or newer) proxy is left
	// untouched, as when the replace never reached it.
	// A run that was lost when claimed has no old proxy serving to keep: it gets its lost or ended fate
	// back instead, fenced on this generation (reviveLostHoldRestore).
	if err := s.lockHeld(lockCtx); err != nil {
		err = fmt.Errorf("the run's lock was lost before its proxy was replaced: %w", err)
		if run.LostAt != nil {
			return 0, proxyStartRec{}, s.reviveLostHoldRestore(ctx, run, fate, data, actorType, actor, gen, start, source, freshJTI, renewed, err)
		}
		return 0, proxyStartRec{}, s.reviveKeptAfterAdvance(ctx, run, data, actorType, actor, retiring, gen, start, source, freshJTI, renewed, err)
	}
	return gen, start, nil
}

// reviveReplaceFailed is reviveRunProxy's answer to a proxy replace that failed after the claim: a live run whose old proxy was never touched keeps it,
// and otherwise the run is lost again.
func (s *Server) reviveReplaceFailed(ctx context.Context, run types.AgentRun, data map[string]any, actorType types.ActorType, actor string, gen int, start proxyStartRec, source []byte,
	retiring, freshJTI string, renewed renewedTokens, err error) (reviveResult, *reviveError) {
	data["error"] = err.Error()
	if run.LostAt == nil && !errors.Is(err, runner.ErrProxyReplaceFailed) {
		// The old proxy was never touched (an image that cannot be pulled, a
		// failed inspect): a live run keeps it, renewing its own token. Only
		// the claim's token stamp moved. Its retiring token must stay live
		// too (F4, Fable review) — revoking it here would strand that kept,
		// untouched proxy on a token that can neither renew nor decide,
		// while its allowlisted egress keeps flowing audit-dark for up to
		// the lapsed-token sweep's ~1h05m window.
		return reviveResult{}, s.reviveKeptAfterAdvance(ctx, run, data, actorType, actor, retiring, gen, start, source, freshJTI, renewed, err)
	}
	// The old proxy is, or may be, gone: nothing is left to serve on the
	// retiring token, so revoking it here is safe (F4).
	s.revokeRetiringToken(ctx, retiring, run.ID)
	s.revokeRenewedTokens(ctx, run.ID, renewed.ids(), freshJTI)
	return reviveResult{generation: gen, stored: gen > 0}, s.reviveFailedAfterClaim(ctx, run, gen, data, actorType, actor, reasonReviveProxyReplaceFailedLost,
		"the run's proxy could not be replaced, so the run has no egress and is lost until revived", err)
}

// reviveStartAgent starts a rebooted run's agent behind its new proxy (reviveRunProxy). It is nil once the agent runs.
func (s *Server) reviveStartAgent(ctx, runnerCtx context.Context, starter runner.SandboxStarter, run types.AgentRun, gen int, data map[string]any, actorType types.ActorType, actor string) *reviveError {
	// F2: refresh the watcher lease right before starting the agent — on a
	// multi-replica deployment the claim's own stamp can go stale while
	// ReplaceProxy ran, and a sweep that then adopted this run would probe
	// an agent not yet started and lose it again mid-revive. Best-effort:
	// keepRebootedRun's busy check is the load-bearing guard; this only
	// narrows the window it has to cover.
	if wl, ok := s.cfg.Store.(store.RunWatcherLeaser); ok {
		if err := wl.HeartbeatRunWatcher(ctx, run.ID, watcherOwner); err != nil {
			slog.WarnContext(ctx, "wardynd: heartbeating a revived run's watcher lease before starting its agent failed",
				slog.String("run_id", run.ID.String()), slog.Any("err", err))
		}
	}
	// Only now, behind the new proxy. A failed start is lost (reboot) again,
	// which stops the new proxy with the agent.
	if err := s.startRevivedAgent(runnerCtx, starter, run); err != nil {
		data["error"] = "start agent: " + err.Error()
		return s.reviveFailedAfterClaim(ctx, run, gen, data, actorType, actor, reasonReviveAgentStartFailedLost,
			"the run's agent could not be started, so the run is stopped and lost until revived", err)
	}
	data["agent_started"] = true
	return nil
}

// reviveFailedAfterClaim is the end of a revive whose proxy replace or agent
// start failed after the claim: the run is put back to lost, and the answer says
// "lost" only when that was persisted. When the lost mark could not be written
// the run is not lost on record, its proxy is stopped and its broker revoked
// anyway (reloseRun), and the answer says the recovery is unresolved, for a
// later sweep to settle. "Stopped" or "no egress" is claimed only for a confirmed stop; lost_again
// only for a persisted lost fate. data carries the failure; err is the cause, logged and in the audit row, never in the answer.
// fence is the policy generation the revive acts for (reloseRun): once a newer revive started one, this
// revive loses and stops nothing.
func (s *Server) reviveFailedAfterClaim(ctx context.Context, run types.AgentRun, fence int, data map[string]any, actorType types.ActorType, actor, reason, msg string, err error) *reviveError {
	fate, stopErr := s.reloseRun(ctx, run, fence)
	data["lost_again"] = fate == relostLost
	// A readback failure's stop and fate wording is settleRevivedProxy's, from its own containment result.
	readback := reason == reasonRevivePolicyUnconfirmedLost
	unconfirmed := stopErr != nil && !readback
	if unconfirmed {
		data["containment"] = "unresolved"
	}
	s.recordAudit(ctx, s.auditEvent(&run.ID, actorType, actor, "run.revive", run.ID.String(), "failure", mustJSON(data)))
	switch {
	case fate == relostUnwritten && unconfirmed:
		return reviveRefused(http.StatusServiceUnavailable, reasonReviveRecoveryUnresolved,
			loggedMsg(ctx, "the revive failed and the run could not be recorded as lost; its credentials were revoked, but stopping its proxy is not confirmed yet, and a later sweep recovers it", err))
	case fate == relostUnwritten:
		return reviveRefused(http.StatusServiceUnavailable, reasonReviveRecoveryUnresolved,
			loggedMsg(ctx, "the revive failed and the run could not be recorded as lost; its proxy was stopped and its credentials revoked, and a later sweep recovers it", err))
	case readback:
	case fate == relostUnknown && unconfirmed:
		msg = "the revive failed and its lost mark or end did not land, so the run is being lost or ended by another operation; stopping its proxy is not confirmed yet, and a later sweep settles it"
	case fate == relostUnknown:
		msg = "the revive failed and its lost mark or end did not land, so the run is being lost or ended by another operation; a later sweep settles it"
	case fate == relostEnded && unconfirmed:
		msg = "the revive failed and the run cannot be kept, so it is being ended; stopping its sandbox is not confirmed yet, and a later sweep retries it"
	case fate == relostEnded:
		msg = "the revive failed and the run cannot be kept, so it is being ended"
	case unconfirmed:
		msg = "the revive failed and the run is lost until revived; stopping its proxy is not confirmed yet, and a later sweep retries it"
	}
	return &reviveError{status: http.StatusBadGateway, reason: reason, lost: true, fate: fate, msg: loggedMsg(ctx, msg, err)}
}

// reloseRun is the fail-closed arm after a claim: the run's proxy is gone (or
// was never replaced), or its agent did not start, so it is marked lost again
// and its proxy stopped (a rebooted run's agent too), or torn down when it
// cannot be kept. A rebooted run stays lost (reboot), so the next revive
// starts its agent again. An ended run goes back to ended under its FIRST mark,
// so a failed revive never restarts its files grace.
//
// It reports what was persisted (reloseFate): relostUnwritten when the lost mark
// could not be written, so nothing says the run is lost. The run is contained
// anyway, its proxy stopped and its broker credentials revoked, and a sweep
// settles the rest; the caller must not tell anyone the run is lost. stopErr is
// non-nil when the proxy stop is not confirmed.
//
// fence is the policy generation the revive acts for (loseRunStopped): a lost mark that misses it, a newer
// revive having taken the run over, is not applied, and nothing is stopped.
func (s *Server) reloseRun(ctx context.Context, run types.AgentRun, fence int) (fate reloseFate, stopErr error) {
	reason, at := types.LostOutage, s.cfg.Now()
	switch run.LostReason {
	case types.LostReboot:
		reason = types.LostReboot
	case types.LostEnded:
		reason, at = types.LostEnded, *run.LostAt
	}
	run.LostAt, run.LostReason = nil, ""
	loser, ok := s.cfg.Store.(store.RunLoser)
	leaser, lok := s.cfg.Store.(store.RunLeaser)
	if ok && lok {
		switch o, err := s.loseRunStopped(ctx, loser, leaser, run, reason, types.RunFailed, 0, at, fence); o {
		case loseWriteFailed:
			s.revokeRunBroker(ctx, run.ID)
			run.LostAt, run.LostReason = &at, reason
			if err := s.stopLostSandbox(ctx, run, s.cfg.Now()); err != nil {
				slog.WarnContext(ctx, "wardynd: stopping a run's proxy after its lost mark could not be written failed",
					slog.String("run_id", run.ID.String()), slog.Any("err", err))
				return relostUnwritten, err
			}
			return relostUnwritten, nil
		case loseApplied:
			if errors.Is(err, runner.ErrEndUnsupported) {
				return s.reloseEnded(ctx, run.ID) // the mark landed, but its substrate cannot keep a sandbox: loseRunStopped ended it
			}
			return relostLost, err
		case loseNotApplied:
			return relostUnknown, errors.New("the run's lost mark did not land, so this revive did not stop its proxy")
		}
	}
	slog.WarnContext(ctx, "wardynd: a run whose proxy could not be replaced cannot be kept; tearing it down",
		slog.String("run_id", run.ID.String()))
	s.reconcileFinalize(ctx, run.ID, types.RunFailed, run.SandboxRef, "its proxy could not be replaced at revive")
	return s.reloseEnded(ctx, run.ID)
}

// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/db"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// reviveReadbackSupported refuses, before any change, a revive whose new proxy must confirm its policy
// (an expected start, run_proxy_readback.go) on a substrate that cannot answer the hash of the bytes it
// hands that proxy (no runner.ProxyReviverHashed).
func (s *Server) reviveReadbackSupported(ctx context.Context, run types.AgentRun, rv runner.ProxyReviver) *reviveError {
	if _, ok := s.cfg.Store.(store.RunProxyStarts); !ok {
		return nil
	}
	if _, ok := rv.(runner.ProxyReviverHashed); ok || s.proxyStartMode(ctx, run) == store.ProxyStartLegacy {
		return nil
	}
	return reviveRefused(http.StatusConflict, reasonReviveReadbackUnsupported,
		"this run's substrate cannot confirm a new proxy's policy, so its proxy cannot be replaced")
}

// advanceRevivePolicyAndStart is advanceRunPolicyGeneration, expecting generation expected and storing
// stored, then the new proxy's pending start under that generation and the fresh token. The caller has
// retired the old lineage, so no report on it can land on the start. A start that cannot be recorded
// abandons the generation, storing source back, and is the revive's error.
func (s *Server) advanceRevivePolicyAndStart(ctx context.Context, run types.AgentRun, policy types.RunPolicySpec, retiring string,
	renewed renewedTokens, fresh string, expected store.PolicySlot, stored, source []byte) (int, proxyStartRec, error) {
	gen, err := s.advanceRunPolicyGeneration(ctx, run, policy, retiring, renewed, fresh, expected, stored)
	if err != nil {
		if !errors.Is(err, store.ErrRunPolicyMoved) {
			gen, err = s.reconcileLostAdvance(ctx, run, policy, fresh, expected, source, err)
		}
		return gen, proxyStartRec{}, err
	}
	start, err := s.beginProxyStart(ctx, run, gen, fresh)
	if err != nil {
		s.abandonRunPolicyGeneration(ctx, run, gen, source)
		return gen, proxyStartRec{}, err
	}
	return gen, start, nil
}

// whileLockHeld is ctx, also cancelled once lock, the run lock's own context, ends because the hold was
// lost (db.ErrLockLost): a revive's replace stops on a lost lock, never on a request that ends. stop
// releases it.
func whileLockHeld(ctx, lock context.Context) (context.Context, func()) {
	out, cancel := context.WithCancelCause(ctx)
	after := context.AfterFunc(lock, func() {
		if cause := context.Cause(lock); errors.Is(cause, db.ErrLockLost) {
			cancel(cause)
		}
	})
	return out, func() {
		after()
		cancel(context.Canceled)
	}
}

// replaceRevivedProxyHashed is replaceRevivedProxy through runner.ProxyReviverHashed when rv has it,
// answering the hash of the bytes the new proxy was handed; "" from a reviver without it. On an error
// the start is abandoned, before the caller restores any lineage, so the kept proxy's tokens can never
// confirm it.
func (s *Server) replaceRevivedProxyHashed(ctx context.Context, rv runner.ProxyReviver, run types.AgentRun, cfgJSON []byte, start proxyStartRec) (string, error) {
	var sha string
	var err error
	if h, ok := rv.(runner.ProxyReviverHashed); !ok {
		err = s.replaceRevivedProxy(ctx, rv, run, cfgJSON)
	} else if run.RunnerID == nil {
		sha, err = h.ReplaceProxyHashed(ctx, run.SandboxRef, cfgJSON)
	} else if err = s.renewRunnerLease(ctx, run, leaseWait); err == nil {
		sha, err = h.ReplaceProxyHashed(ctx, run.SandboxRef, cfgJSON)
	}
	// A substrate that cannot hash refuses before anything changes; a start that need not confirm is
	// replaced as before.
	if errors.Is(err, runner.ErrReviveUnsupported) && start.mode != store.ProxyStartExpected {
		sha, err = "", s.replaceRevivedProxy(ctx, rv, run, cfgJSON)
	}
	if err != nil {
		s.abandonProxyStart(context.WithoutCancel(ctx), run.ID, start)
	}
	return sha, err
}

// reviveKeptCurrent is the refusal of a live run's revive whose old proxy was never touched: 409
// revive_readback_unsupported when the substrate cannot hash the new proxy's config
// (runner.ErrReviveUnsupported from ReplaceProxyHashed), else 502 revive_proxy_kept_current.
func reviveKeptCurrent(ctx context.Context, err error) *reviveError {
	if errors.Is(err, runner.ErrReviveUnsupported) {
		return reviveRefused(http.StatusConflict, reasonReviveReadbackUnsupported,
			"this run's substrate cannot confirm a new proxy's policy, so its proxy was not replaced, and the run keeps its current proxy")
	}
	return reviveRefused(http.StatusBadGateway, reasonReviveProxyKeptCurrent,
		loggedMsg(ctx, "the run's proxy was not replaced, and the run keeps its current proxy", err))
}

// settleRevivedProxy retires the replaced proxy's tokens and records the driver's answer for the new
// proxy's start. A start that cannot run (no hash, a report of different bytes, a start no longer the
// run's newest) puts the run back to lost with that proxy stopped, and the answer says when that stop
// is not confirmed. It runs under the revive's run lock, so it contains through the locked variant.
func (s *Server) settleRevivedProxy(ctx context.Context, run types.AgentRun, data map[string]any, actorType types.ActorType, actor string,
	start proxyStartRec, sha, retiring string, renewed renewedTokens, fresh string) *reviveError {
	s.revokeRetiringToken(ctx, retiring, run.ID)
	s.revokeRenewedTokens(ctx, run.ID, renewed.ids(), fresh)
	ok, why, failed := s.proxyStartAnswered(ctx, run.ID, start, sha)
	if ok {
		return nil
	}
	data["error"] = "policy readback: " + why
	rerr := s.reviveFailedAfterClaim(ctx, run, start.gen, data, actorType, actor, reasonRevivePolicyUnconfirmedLost,
		"the run's new proxy did not confirm its policy, so it was stopped", errors.New(why))
	// The answer's stop wording comes from the containment result, its fate from what the store now
	// holds, never from the lost mark's outcome.
	c := s.containProxyStartLocked(ctx, run.ID, start.gen)
	if failed != nil {
		s.auditReadbackFailure(ctx, *failed, c)
	}
	fate := ""
	if rerr.reason == reasonRevivePolicyUnconfirmedLost {
		fate = s.storedRunFate(ctx, run.ID)
		rerr.msg += fate + unconfirmedImageCause
	}
	if c.Contained || errors.Is(c.Err, errLostMarkUnwritten) || errors.Is(c.Err, errRunEndUnwritten) {
		return rerr
	}
	// The sweep retries only a failed start that is still the run's newest.
	retry := ""
	if !c.Superseded && !errors.Is(c.Err, errStartNotFailed) {
		retry = ", and a later sweep retries it"
	}
	switch rerr.reason {
	case reasonRevivePolicyUnconfirmedLost:
		rerr.msg = loggedMsg(ctx, "the run's new proxy did not confirm its policy"+fate+"; "+
			"stopping that proxy is not confirmed yet"+retry+unconfirmedImageCause, errors.New(why))
	case reasonReviveRecoveryUnresolved:
		rerr.msg = loggedMsg(ctx, "the revive failed and the run could not be recorded as lost; its credentials were revoked, "+
			"but stopping its proxy is not confirmed yet"+retry, errors.New(why))
	}
	return rerr
}

// storedRunFate is what the store holds for a run after its revive failed: "; the run is ended" or
// "; the run is lost until revived", or "" when it holds neither or cannot be read. Its read is bound
// apart from the request's, which a readback wait may have run out.
func (s *Server) storedRunFate(ctx context.Context, id uuid.UUID) string {
	fate, _ := s.readRunFate(ctx, id)
	return fate
}

// readRunFate is storedRunFate, also answering whether the run could be read.
func (s *Server) readRunFate(ctx context.Context, id uuid.UUID) (string, bool) {
	ctx, cancel := stopCtx(ctx)
	defer cancel()
	run, err := s.cfg.Store.GetRun(ctx, id)
	if err != nil {
		return "", false
	}
	return fateOf(run), true
}

// fateOf is a stored run's fate as storedRunFate says it.
func fateOf(run types.AgentRun) string {
	switch {
	case run.State.IsTerminal(), run.LostReason == types.LostEnded:
		return "; the run is ended"
	case run.LostAt != nil:
		return "; the run is lost until revived"
	}
	return ""
}

// reloseFate is the fate reloseEnded reads back for a run asked to end: nothing, lost again (the mark written), the
// mark not written (unresolved), the mark not applied or the end not landed (another writer moved the run first), or
// the run ended (also when the mark landed on a substrate that cannot keep a sandbox, so loseRunStopped tore it down).
type reloseFate int

const (
	relostNone reloseFate = iota
	relostLost
	relostUnwritten
	relostUnknown
	relostEnded
)

// reloseEnded is the fate of a run asked to end (loseRunStopped's teardown on a substrate that cannot keep a sandbox,
// which loseUnconfirmedRun reads back, or reloseRun's reconcileFinalize), read back rather than assumed: ended only once the store has it terminal,
// its stop confirmed only once its sandbox ref is cleared (finalizeRunEffects clears it only after its own stop
// succeeded) or its sandbox is kept for its computer folder's pass back (quiesceKeptSandbox stopped its proxy). A run still kept is lost with its stop unconfirmed; any other is left to whatever moves it.
func (s *Server) reloseEnded(ctx context.Context, runID uuid.UUID) (reloseFate, error) {
	cur, err := s.cfg.Store.GetRun(ctx, runID)
	switch {
	case err != nil:
		return relostUnknown, fmt.Errorf("the run could not be read back after its end: %w", err)
	case isTerminalRunState(cur.State) && cur.SandboxRef != "":
		// A sandbox kept for its computer folder's pass back had its proxy stopped before it was kept.
		if kept, ferr := s.computerFolderKeptRead(ctx, runID); kept && ferr == nil {
			return relostEnded, nil
		}
		return relostEnded, errors.New("the run was ended, but its sandbox stop is not confirmed (its ref is still recorded)")
	case isTerminalRunState(cur.State):
		return relostEnded, nil
	case runIsKept(cur):
		return relostLost, errors.New("the run is lost, but its end did not land on a substrate that cannot stop its proxy alone")
	}
	return relostUnknown, errors.New("this revive's end of the run did not land, so it did not stop its proxy")
}

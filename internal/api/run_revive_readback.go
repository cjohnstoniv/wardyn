// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/google/uuid"

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

// advanceRevivePolicyAndStart is advanceRunPolicyGeneration, then the new proxy's pending start under that
// generation and the fresh token. The caller has retired the old lineage, so no report on it can land on
// the start. A start that cannot be recorded abandons the generation and is the revive's error.
func (s *Server) advanceRevivePolicyAndStart(ctx context.Context, run types.AgentRun, policy types.RunPolicySpec, retiring string,
	renewed renewedTokens, fresh string) (int, proxyStartRec, error) {
	gen, err := s.advanceRunPolicyGeneration(ctx, run, policy, retiring, renewed, fresh)
	if err != nil {
		return 0, proxyStartRec{}, err
	}
	start, err := s.beginProxyStart(ctx, run, gen, fresh)
	if err != nil {
		s.abandonRunPolicyGeneration(ctx, run, gen)
		return 0, proxyStartRec{}, err
	}
	return gen, start, nil
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
	rerr := s.reviveFailedAfterClaim(ctx, run, data, actorType, actor, reasonRevivePolicyUnconfirmedLost,
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
		rerr.msg += fate
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
			"stopping that proxy is not confirmed yet"+retry, errors.New(why))
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
	ctx, cancel := stopCtx(ctx)
	defer cancel()
	run, err := s.cfg.Store.GetRun(ctx, id)
	switch {
	case err != nil:
		return ""
	case run.State.IsTerminal():
		return "; the run is ended"
	case run.LostAt != nil:
		return "; the run is lost until revived"
	}
	return ""
}

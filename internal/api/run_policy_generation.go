// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/secretstore/kek"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// advanceRunPolicyGeneration starts the run's next policy generation for a revive whose new proxy will
// enforce policy (the organisation's form, as the run's stored config keeps it), on the fresh token.
// The outgoing proxy's tokens (retiring, the config's, and the last renewal's two) stay on the
// generation it enforced, so a decision it sent before the revive and that lands after is still
// stamped with that generation. The new policy is recorded as run.policy.generation: data.policy as
// auditablePolicy writes it, so its digest can be recomputed from the row, beside data.generation, and
// the row is stamped with its own generation. It runs before the new proxy starts, so that proxy's
// first row is stamped too. A store without generations returns 0 and no error, and the revive then runs unstamped (generation 0 also
// disables the abandon): Postgres implements RunPolicyGenerations, and any other production store must. A failed write is an
// error, and the revive must not start the new proxy (reviveGenerationFailed): with no generation on
// record, the new proxy's rows would be stamped with the old policy it no longer enforces.
//
// expected is the run's newest generation as the revive read it before its source config
// (reviveExpectedGeneration), and stored is the revive's config as the run row keeps it: both land in
// one transaction, only while that generation is still the newest and the run is not lost, else
// store.ErrRunPolicyMoved and nothing changed (revivePolicyMoved). So no revive reads a generation
// without the source config it stored, and none applies a config derived from an older source.
func (s *Server) advanceRunPolicyGeneration(ctx context.Context, run types.AgentRun, policy types.RunPolicySpec, retiring string, renewed renewedTokens, fresh string,
	expected store.PolicySlot, stored []byte) (int, error) {
	st, ok := s.cfg.Store.(store.RunPolicyGenerations)
	if !ok {
		return 0, nil
	}
	retired := renewed.ids()
	// An expired or revoked starting token has no proxy left to send on it, so its id is not needed.
	if claims, err := s.cfg.Identity.Verify(ctx, retiring, internalAudience); err == nil {
		retired = append(retired, claims.JTI)
	}
	sealed, err := s.sealForGeneration(run.ID, stored)
	if err != nil {
		return 0, err
	}
	spec := auditablePolicy(policy)
	gen, err := st.AdvanceRunPolicyGeneration(ctx, run.ID, expected, spec.Digest(), retired, fresh, sealed)
	if err != nil {
		return 0, err
	}
	s.recordAudit(ctx, s.auditEvent(&run.ID, types.ActorSystem, "wardynd", "run.policy.generation",
		run.ID.String(), "success", mustJSON(map[string]any{"generation": gen, "policy": spec})))
	return gen, nil
}

// reviveGenerationFailed ends a revive whose policy generation could not be stored, after the claim and
// before any proxy is replaced, the way a replace that failed at that point ends. A live run keeps its
// untouched proxy, which gets its token lineage back; a lost run is lost again, with the old proxy's
// tokens revoked, since nothing serves on them. A live run whose generation could not be undone
// (errPolicyNotAbandoned) ends the way a lost run does, never answered kept-current: that generation stays
// current, narrower than the kept proxy, so the run is lost again and its proxy stopped (reloseRun), and the
// answer says the stop is confirmed only when it is. Either way the run's rows stay under the generation its proxy enforces, and the next revive tries again.
// expected is the generation the revive read: its lost mark lands only while that is still the newest.
func (s *Server) reviveGenerationFailed(ctx context.Context, run types.AgentRun, expected int, data map[string]any, actorType types.ActorType, actor, retiring, fresh string, renewed renewedTokens, err error) *reviveError {
	undoFailed := errors.Is(err, errPolicyNotAbandoned)
	data["error"] = "record the run's policy generation: " + err.Error()
	if undoFailed {
		data["error"] = "undo the run's policy generation: " + err.Error()
	}
	if run.LostAt == nil && !undoFailed {
		// The claim's bound may have run out; the audit and the lineage restore take a bound of their own.
		kctx, cancelKept := context.WithTimeout(context.WithoutCancel(ctx), killCascadeTimeout)
		defer cancelKept()
		s.recordAudit(kctx, s.auditEvent(&run.ID, actorType, actor, "run.revive", run.ID.String(), "failure", mustJSON(data)))
		s.liveRevived.Delete(run.ID) // nothing was replaced, so nothing to bound
		s.restoreRunTokenLineage(kctx, run.ID, fresh, renewed)
		return reviveRefused(http.StatusBadGateway, reasonReviveProxyKeptCurrent,
			loggedMsg(ctx, "the run's policy could not be recorded, so its proxy was not replaced, and the run keeps its current proxy", err))
	}
	// The claim's bound may have run out, which is the likeliest reason an undo failed: the containment takes a
	// bound of its own.
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), killCascadeTimeout)
	defer cancel()
	s.revokeRetiringToken(cctx, retiring, run.ID)
	s.revokeRenewedTokens(cctx, run.ID, renewed.ids(), fresh)
	msg := "the run's policy could not be recorded, so its proxy was not replaced, and the run is lost until revived"
	if undoFailed {
		msg = "the run's new policy record could not be undone, so the run is lost until revived"
	}
	return s.reviveFailedAfterClaim(cctx, run, expected, data, actorType, actor, reasonReviveProxyReplaceFailedLost, msg, err)
}

// abandonRunPolicyGeneration undoes advanceRunPolicyGeneration's current generation when the revive left
// the old proxy running untouched: the generation before it is current again. Its number stays used.
// restore is the config the revive started from (reviveSourceConfig), stored back in the same write, so
// the run's stored config is again the one its kept proxy runs; a config that cannot be sealed is left
// out, and the generation is abandoned without it. Both land only while gen is still the
// newest: a late abandon leaves a newer revive's generation and config alone. It returns the store's error
// when the undo could not be written: the generation then stays current, and the caller contains the run
// (errPolicyNotAbandoned) rather than answer that it keeps its current proxy.
//
// It also returns what an abandon saw when it changed nothing because a newer generation slot exists
// (store.PolicyAbandonNoop; nil otherwise, and for a store that does not report it). A Current equal to gen
// there means the undo did nothing and gen is still current, so no caller may treat it as undone.
func (s *Server) abandonRunPolicyGeneration(ctx context.Context, run types.AgentRun, gen int, restore []byte) (*store.PolicyAbandonNoop, error) {
	st, ok := s.cfg.Store.(store.RunPolicyGenerations)
	if !ok || gen == 0 {
		return nil, nil
	}
	// The undo takes a bound of its own: the claim's may have run out, and a run left under a generation
	// narrower than its proxy is the worse outcome.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), killCascadeTimeout)
	defer cancel()
	sealed, err := s.sealForGeneration(run.ID, restore)
	if err != nil {
		slog.WarnContext(ctx, "api: sealing the config an abandoned revive started from failed; abandoning without it",
			slog.String("run_id", run.ID.String()), slog.Int("generation", gen), slog.Any("err", err))
	}
	var noop *store.PolicyAbandonNoop
	if nt, ok := st.(store.RunPolicyAbandonNoted); ok {
		noop, err = nt.AbandonRunPolicyGenerationNoted(ctx, run.ID, gen, sealed)
	} else {
		err = st.AbandonRunPolicyGeneration(ctx, run.ID, gen, sealed)
	}
	if err != nil {
		slog.WarnContext(ctx, "api: abandoning an unused policy generation failed",
			slog.String("run_id", run.ID.String()), slog.Int("generation", gen), slog.Any("err", err))
		return nil, err
	}
	return noop, nil
}

// errPolicyNotAbandoned marks a revive whose new generation could not be undone after the old proxy was kept: that generation stays current, narrower than the kept proxy, so the run is contained rather than answered kept-current.
var errPolicyNotAbandoned = errors.New("the revive's policy generation could not be undone")

// sealForGeneration seals a revive's config for the generation write that stores it, as
// storeRunProxyConfig would: nil for no config, or for a deployment that keeps none.
func (s *Server) sealForGeneration(runID uuid.UUID, cfgJSON []byte) ([]byte, error) {
	if _, ok := s.runProxyConfigStore(); !ok || cfgJSON == nil {
		return nil, nil
	}
	sealed, err := kek.Seal(s.cfg.RunConfigKey, cfgJSON, runProxyConfigAAD(runID))
	if err != nil {
		return nil, fmt.Errorf("seal proxy config: %w", err)
	}
	return sealed, nil
}

// reviveExpectedGeneration reads the run's newest policy generation slot (store.NewestPolicySlot: its
// number, and whether it is abandoned), before the revive reads its source config: the slot its advance
// expects. A store without generations fences nothing (the zero slot). A read that fails refuses the
// revive before anything changes.
func (s *Server) reviveExpectedGeneration(ctx context.Context, runID uuid.UUID) (store.PolicySlot, *reviveError) {
	st, ok := s.cfg.Store.(store.RunPolicyGenerations)
	if !ok {
		return store.PolicySlot{}, nil
	}
	ps, _, err := st.RunPolicyState(ctx, runID)
	if err != nil {
		return store.PolicySlot{}, reviveRefused(http.StatusServiceUnavailable, reasonReviveConfigUnreadable, loggedMsg(ctx, "read the run's policy generation", err))
	}
	return store.NewestPolicySlot(ps), nil
}

// revivePolicyMoved ends a revive whose advance found the run moved since it read its generation
// (store.ErrRunPolicyMoved): another revive started or abandoned a generation (a newer one owns the run's
// proxy now), the run was lost, or it left RUNNING. Nothing was written, and nothing is stopped or
// replaced: the old proxy (or the newer revive's) is left as it is, the run's lineage restored, and the
// answer is the kept proxy's.
func (s *Server) revivePolicyMoved(ctx context.Context, run types.AgentRun, data map[string]any, actorType types.ActorType, actor, fresh string, renewed renewedTokens, err error) *reviveError {
	data["error"] = "the run changed since this revive read its policy generation (another revive started or abandoned one, or the run was lost or left RUNNING): " + err.Error()
	// The claim's bound may have run out; the audit and the lineage restore take a bound of their own.
	kctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), killCascadeTimeout)
	defer cancel()
	s.recordAudit(kctx, s.auditEvent(&run.ID, actorType, actor, "run.revive", run.ID.String(), "failure", mustJSON(data)))
	s.liveRevived.Delete(run.ID) // nothing was replaced, so nothing to bound
	s.restoreRunTokenLineage(kctx, run.ID, fresh, renewed)
	return reviveKeptCurrent(ctx, err)
}

// reviveKeptAfterAdvance ends a revive that advanced its generation and then left the old proxy
// untouched: its start abandoned (when it was not already), the generation abandoned with the source
// config stored back, the failure recorded, and the run's lineage restored. The old proxy keeps
// renewing its own token, so its retiring token is not revoked (F4). A generation that cannot be undone
// is the exception: it stays current over the kept proxy, so the run is lost again and its proxy stop
// attempted instead.
func (s *Server) reviveKeptAfterAdvance(ctx context.Context, run types.AgentRun, data map[string]any, actorType types.ActorType, actor, retiring string,
	gen int, start proxyStartRec, source []byte, fresh string, renewed renewedTokens, err error) *reviveError {
	data["error"] = err.Error()
	s.abandonProxyStart(ctx, run.ID, start) // a no-op for one the replace already abandoned
	// The generation is abandoned first, so the failure row carries the one that still applies.
	noop, aerr := s.abandonRunPolicyGeneration(ctx, run, gen, source)
	// The claim's bound may have run out, which is the likeliest reason an undo failed and would fail the
	// lineage restore and the audit too, so the steps below take a bound of their own, started only once
	// the undo has returned so a slow undo cannot use it up.
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), killCascadeTimeout)
	defer cancel()
	fence, msg := gen, "the run's proxy was not replaced and its new policy record could not be undone, so the run is lost until revived"
	if aerr == nil && noop != nil && noop.Current == gen {
		// A newer edit came after this one and was itself undone, which left this one current again: nothing
		// undid it. The mark is fenced on the newest slot, which is what the store compares.
		fence, aerr = noop.Newest, errPolicyNotAbandoned
		msg = "the run's proxy was not replaced and the edit could not be undone because another edit came after it, so the run is lost until revived"
	}
	if aerr != nil {
		// The generation stays current, narrower than the kept proxy: the run is lost again and its proxy
		// stop attempted, fenced on fence, never answered kept-current.
		data["error"] = err.Error() + "; undoing its generation failed: " + aerr.Error()
		s.liveRevived.Delete(run.ID)
		s.revokeRetiringToken(cctx, retiring, run.ID)
		s.revokeRenewedTokens(cctx, run.ID, renewed.ids(), fresh)
		return s.reviveFailedAfterClaim(cctx, run, fence, data, actorType, actor, reasonReviveProxyReplaceFailedLost, msg, err)
	}
	s.recordAudit(cctx, s.auditEvent(&run.ID, actorType, actor, "run.revive", run.ID.String(), "failure", mustJSON(data)))
	s.liveRevived.Delete(run.ID) // nothing was replaced, so nothing to bound
	s.restoreRunTokenLineage(cctx, run.ID, fresh, renewed)
	return reviveKeptCurrent(ctx, err)
}

// reconcileLostAdvance answers an advance that returned an error other than store.ErrRunPolicyMoved,
// which may still have committed (an acknowledgement lost after the commit). It reads the run's policy
// state back once. A generation one past expected whose first token is fresh is this attempt's, written
// after all, wherever it sits in the list (a newer edit may have come and been undone since, abandoned
// entries staying listed), so it is recorded (run.policy.generation) and abandoned with source stored back
// (only when it can be sealed), which lands only while it is still the newest. Its number is returned as
// the generation this attempt holds, for the lost mark's fence. When that undo changed nothing and the
// generation is still current (a newer edit was undone first), the newest slot's number is returned
// instead, with errPolicyNotAbandoned. Any other generation was not this attempt's write (nothing is
// undone, never a newer winner's), and 0 is returned. A read back that fails too is the named unresolved
// outcome: nothing is undone, and the error says whether the generation landed is unknown; the effective
// policy then fails closed on a generation with no audit row.
func (s *Server) reconcileLostAdvance(ctx context.Context, run types.AgentRun, policy types.RunPolicySpec, fresh string, expected store.PolicySlot, source []byte, err error) (int, error) {
	st, ok := s.cfg.Store.(store.RunPolicyGenerations)
	if !ok || fresh == "" {
		return 0, err
	}
	// The claim's bound may have run out; the read-back takes a bound of its own so it does not fail at once.
	rctx, cancelRead := context.WithTimeout(context.WithoutCancel(ctx), killCascadeTimeout)
	ps, _, rerr := st.RunPolicyState(rctx, run.ID)
	cancelRead()
	if rerr != nil {
		return 0, fmt.Errorf("%w (whether the generation was written is unknown: reading it back failed: %v)", err, rerr)
	}
	// The entry is looked for anywhere in the list, not only last: a newer edit may have come and been
	// undone (abandoned entries stay listed) before this read, and this attempt's own entry is then not last.
	var g types.PolicyGeneration
	for _, c := range ps.Generations {
		if c.Generation == expected.Generation+1 && len(c.TokenIDs) > 0 && c.TokenIDs[0] == fresh {
			g = c
			break
		}
	}
	if g.Generation == 0 {
		return 0, err
	}
	s.recordAudit(ctx, s.auditEvent(&run.ID, types.ActorSystem, "wardynd", "run.policy.generation",
		run.ID.String(), "success", mustJSON(map[string]any{"generation": g.Generation, "policy": auditablePolicy(policy)})))
	noop, aerr := s.abandonRunPolicyGeneration(ctx, run, g.Generation, source)
	if aerr != nil {
		return g.Generation, fmt.Errorf("%w (the generation was written all the same, and undoing it failed: %v): %w", err, aerr, errPolicyNotAbandoned)
	}
	if noop != nil && noop.Current == g.Generation {
		// A newer edit came after this one and was itself undone, which left this one current again: the
		// undo changed nothing. The lost mark is fenced on the newest slot, which is what the store compares.
		return noop.Newest, fmt.Errorf("%w (the generation was written all the same, and could not be undone because another edit came after it): %w", err, errPolicyNotAbandoned)
	}
	if noop != nil {
		// A newer edit stands, so the undo changed nothing and nothing here needed it.
		return g.Generation, fmt.Errorf("%w (the generation was written all the same, and a newer edit stands)", err)
	}
	return g.Generation, fmt.Errorf("%w (the generation was written all the same, and is abandoned)", err)
}

// reviveLostFate reads a lost run's fate before the revive's claim clears it (store.RunLostFate), for
// reviveLostHoldRestore. A live run, or a store without the fenced lost writes, has none to read. A read
// that fails refuses the revive before anything changes.
func (s *Server) reviveLostFate(ctx context.Context, run types.AgentRun) (*store.RunLostFate, *reviveError) {
	fl, ok := s.cfg.Store.(store.RunLoserAtGeneration)
	if run.LostAt == nil || !ok {
		return nil, nil
	}
	f, err := fl.RunLostFate(ctx, run.ID)
	if err != nil {
		return nil, reviveRefused(http.StatusServiceUnavailable, reasonReviveAgentStatusUnreadable, loggedMsg(ctx, "read the run's lost mark", err))
	}
	return &f, nil
}

// reviveLostHoldRestore ends the revive of a run that was lost (outage, reboot or ended) when claimed and
// whose lock was lost after its advance, before any replace: there is no serving proxy to keep, so the
// attempt is undone as reviveKeptAfterAdvance undoes it (start and generation abandoned with the source
// config stored back, lineage restored), and then the run gets back the lost mark the claim cleared
// (fate: lost_at, lost_reason, agent_stopped_at), in a write fenced on gen. Nothing is stopped: the old
// proxy was stopped when the run was lost, and nothing was replaced.
//
// A fenced write that misses means a newer revive took the run over (or another writer moved it): that
// revive's outcome stands, and the answer says so. A write that errors fails closed, as a failed replace
// does (reviveFailedAfterClaim: lost again, with the proxy stopped). A store without the fenced write
// (gen 0, or no store.RunLoserAtGeneration) is put back to lost the same way.
func (s *Server) reviveLostHoldRestore(ctx context.Context, run types.AgentRun, fate *store.RunLostFate, data map[string]any, actorType types.ActorType, actor string,
	gen int, start proxyStartRec, source []byte, fresh string, renewed renewedTokens, err error) *reviveError {
	data["error"] = err.Error()
	s.abandonProxyStart(ctx, run.ID, start)
	noop, aerr := s.abandonRunPolicyGeneration(ctx, run, gen, source)
	if aerr != nil {
		data["abandon_error"] = aerr.Error() // the old proxy is already stopped, so none wider serves
	}
	if aerr == nil && noop != nil && noop.Current == gen {
		// The undo changed nothing: gen is current again behind a newer undone edit. No proxy serves here, so
		// the restore below is unchanged; the audit row keeps the fact.
		data["abandon_noop"] = true
	}
	s.liveRevived.Delete(run.ID) // nothing was replaced, so nothing to bound
	s.restoreRunTokenLineage(ctx, run.ID, fresh, renewed)
	const lostMsg = "The run's lock was lost before its proxy was replaced, so the run is lost until revived"
	fl, ok := s.cfg.Store.(store.RunLoserAtGeneration)
	if !ok || fate == nil || gen <= 0 {
		return s.reviveFailedAfterClaim(ctx, run, gen, data, actorType, actor, reasonReviveProxyReplaceFailedLost, lostMsg, err)
	}
	applied, werr := s.fenced(ctx, run.ID, func() (bool, error) { return fl.RestoreRunLostFateAtGeneration(ctx, run.ID, gen, *fate) })
	switch {
	case werr != nil:
		data["error"] = err.Error() + "; restoring its lost mark failed: " + werr.Error()
		return s.reviveFailedAfterClaim(ctx, run, gen, data, actorType, actor, reasonReviveProxyReplaceFailedLost, lostMsg, err)
	case !applied:
		data["superseded"] = true
		s.recordAudit(ctx, s.auditEvent(&run.ID, actorType, actor, "run.revive", run.ID.String(), "failure", mustJSON(data)))
		return reviveRefused(http.StatusConflict, reasonReviveRunChanged, loggedMsg(ctx,
			"the run's lock was lost before its proxy was replaced, and the run changed since (another revive took it over); this revive replaced and stopped nothing", err))
	}
	data["lost_again"] = true
	s.recordAudit(ctx, s.auditEvent(&run.ID, actorType, actor, "run.revive", run.ID.String(), "failure", mustJSON(data)))
	return &reviveError{status: http.StatusConflict, reason: reasonReviveRunChanged, lost: true, fate: relostLost, msg: loggedMsg(ctx,
		"the run's lock was lost before its proxy was replaced, so nothing was replaced, and the run is lost as it was until revived", err)}
}

// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
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
	expected int, stored []byte) (int, error) {
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
// tokens revoked, since nothing serves on them. Either way the run's rows stay under the generation its
// proxy enforces, and the next revive tries again.
func (s *Server) reviveGenerationFailed(ctx context.Context, run types.AgentRun, data map[string]any, actorType types.ActorType, actor, retiring, fresh string, renewed renewedTokens, err error) *reviveError {
	data["error"] = "record the run's policy generation: " + err.Error()
	if run.LostAt == nil {
		s.recordAudit(ctx, s.auditEvent(&run.ID, actorType, actor, "run.revive", run.ID.String(), "failure", mustJSON(data)))
		s.liveRevived.Delete(run.ID) // nothing was replaced, so nothing to bound
		s.restoreRunTokenLineage(ctx, run.ID, fresh, renewed)
		return reviveRefused(http.StatusBadGateway, reasonReviveProxyKeptCurrent,
			loggedMsg(ctx, "the run's policy could not be recorded, so its proxy was not replaced, and the run keeps its current proxy", err))
	}
	s.revokeRetiringToken(ctx, retiring, run.ID)
	s.revokeRenewedTokens(ctx, run.ID, renewed.ids(), fresh)
	return s.reviveFailedAfterClaim(ctx, run, data, actorType, actor, reasonReviveProxyReplaceFailedLost,
		"the run's policy could not be recorded, so its proxy was not replaced, and the run is lost until revived", err)
}

// abandonRunPolicyGeneration undoes advanceRunPolicyGeneration's current generation when the revive left
// the old proxy running untouched: the generation before it is current again. Its number stays used.
// restore is the config the revive started from (reviveSourceConfig), stored back in the same write, so
// the run's stored config is again the one its kept proxy runs. Both land only while gen is still the
// newest: a late abandon leaves a newer revive's generation and config alone.
func (s *Server) abandonRunPolicyGeneration(ctx context.Context, run types.AgentRun, gen int, restore []byte) {
	st, ok := s.cfg.Store.(store.RunPolicyGenerations)
	if !ok || gen == 0 {
		return
	}
	sealed, err := s.sealForGeneration(run.ID, restore)
	if err != nil {
		slog.WarnContext(ctx, "api: sealing the config an abandoned revive started from failed; abandoning without it",
			slog.String("run_id", run.ID.String()), slog.Int("generation", gen), slog.Any("err", err))
	}
	if err := st.AbandonRunPolicyGeneration(ctx, run.ID, gen, sealed); err != nil {
		slog.WarnContext(ctx, "api: abandoning an unused policy generation failed",
			slog.String("run_id", run.ID.String()), slog.Int("generation", gen), slog.Any("err", err))
	}
}

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

// reviveExpectedGeneration reads the run's newest policy generation (store.NewestPolicyGeneration), before
// the revive reads its source config: the generation its advance expects. A store without generations
// fences nothing (0). A read that fails refuses the revive before anything changes.
func (s *Server) reviveExpectedGeneration(ctx context.Context, runID uuid.UUID) (int, *reviveError) {
	st, ok := s.cfg.Store.(store.RunPolicyGenerations)
	if !ok {
		return 0, nil
	}
	ps, _, err := st.RunPolicyState(ctx, runID)
	if err != nil {
		return 0, reviveRefused(http.StatusServiceUnavailable, reasonReviveConfigUnreadable, loggedMsg(ctx, "read the run's policy generation", err))
	}
	return store.NewestPolicyGeneration(ps), nil
}

// revivePolicyMoved ends a revive whose advance found the run moved since it read its generation
// (store.ErrRunPolicyMoved): another revive started a newer one, which owns the run's proxy now, or the
// run was lost. Nothing was written, and nothing is stopped or replaced: the old proxy (or the newer
// revive's) is left as it is, the run's lineage restored, and the answer is the kept proxy's.
func (s *Server) revivePolicyMoved(ctx context.Context, run types.AgentRun, data map[string]any, actorType types.ActorType, actor, fresh string, renewed renewedTokens, err error) *reviveError {
	data["error"] = "another revive changed the run's policy since this one read it: " + err.Error()
	s.recordAudit(ctx, s.auditEvent(&run.ID, actorType, actor, "run.revive", run.ID.String(), "failure", mustJSON(data)))
	s.liveRevived.Delete(run.ID) // nothing was replaced, so nothing to bound
	s.restoreRunTokenLineage(ctx, run.ID, fresh, renewed)
	return reviveKeptCurrent(ctx, err)
}

// reviveKeptAfterAdvance ends a revive that advanced its generation and then left the old proxy
// untouched: its start abandoned (when it was not already), the generation abandoned with the source
// config stored back, the failure recorded, and the run's lineage restored. The old proxy keeps
// renewing its own token, so its retiring token is not revoked (F4).
func (s *Server) reviveKeptAfterAdvance(ctx context.Context, run types.AgentRun, data map[string]any, actorType types.ActorType, actor string,
	gen int, start proxyStartRec, source []byte, fresh string, renewed renewedTokens, err error) *reviveError {
	data["error"] = err.Error()
	s.abandonProxyStart(ctx, run.ID, start) // a no-op for one the replace already abandoned
	// The generation is abandoned first, so the failure row carries the one that still applies.
	s.abandonRunPolicyGeneration(ctx, run, gen, source)
	s.recordAudit(ctx, s.auditEvent(&run.ID, actorType, actor, "run.revive", run.ID.String(), "failure", mustJSON(data)))
	s.liveRevived.Delete(run.ID) // nothing was replaced, so nothing to bound
	s.restoreRunTokenLineage(ctx, run.ID, fresh, renewed)
	return reviveKeptCurrent(ctx, err)
}

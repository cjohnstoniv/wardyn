// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"log/slog"
	"net/http"

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
func (s *Server) advanceRunPolicyGeneration(ctx context.Context, run types.AgentRun, policy types.RunPolicySpec, retiring string, renewed renewedTokens, fresh string) (int, error) {
	st, ok := s.cfg.Store.(store.RunPolicyGenerations)
	if !ok {
		return 0, nil
	}
	retired := renewed.ids()
	// An expired or revoked starting token has no proxy left to send on it, so its id is not needed.
	if claims, err := s.cfg.Identity.Verify(ctx, retiring, internalAudience); err == nil {
		retired = append(retired, claims.JTI)
	}
	spec := auditablePolicy(policy)
	gen, err := st.AdvanceRunPolicyGeneration(ctx, run.ID, spec.Digest(), retired, fresh)
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
func (s *Server) abandonRunPolicyGeneration(ctx context.Context, run types.AgentRun, gen int) {
	st, ok := s.cfg.Store.(store.RunPolicyGenerations)
	if !ok || gen == 0 {
		return
	}
	if err := st.AbandonRunPolicyGeneration(ctx, run.ID, gen); err != nil {
		slog.WarnContext(ctx, "api: abandoning an unused policy generation failed",
			slog.String("run_id", run.ID.String()), slog.Int("generation", gen), slog.Any("err", err))
	}
}

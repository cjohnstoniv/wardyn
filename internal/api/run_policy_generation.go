// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"log/slog"

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
// first row is stamped too. The generation is evidence: a store without it, or a failed write, logs,
// records nothing and returns 0, and the revive goes on.
func (s *Server) advanceRunPolicyGeneration(ctx context.Context, run types.AgentRun, policy types.RunPolicySpec, retiring string, renewed renewedTokens, fresh string) int {
	st, ok := s.cfg.Store.(store.RunPolicyGenerations)
	if !ok {
		return 0
	}
	retired := renewed.ids()
	// An expired or revoked starting token has no proxy left to send on it, so its id is not needed.
	if claims, err := s.cfg.Identity.Verify(ctx, retiring, internalAudience); err == nil {
		retired = append(retired, claims.JTI)
	}
	spec := auditablePolicy(policy)
	gen, err := st.AdvanceRunPolicyGeneration(ctx, run.ID, spec.Digest(), retired, fresh)
	if err != nil {
		slog.WarnContext(ctx, "api: revived run's policy generation not stored, its new proxy's rows are stamped by the current one",
			slog.String("run_id", run.ID.String()), slog.Any("err", err))
		return 0
	}
	s.recordAudit(ctx, s.auditEvent(&run.ID, types.ActorSystem, "wardynd", "run.policy.generation",
		run.ID.String(), "success", mustJSON(map[string]any{"generation": gen, "policy": spec})))
	return gen
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

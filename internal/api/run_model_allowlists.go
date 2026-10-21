// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"log/slog"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
)

// recordModelAllowlists keeps, on the run, the model allowlist each local model provider handed its
// proxy at this dispatch (#3374). The proxy holds the run to that list while the provider's own list
// can change afterwards, and GET /model-providers is the admin's, so the run page reads this record.
// Every dispatch writes it, an empty one included, so a run that no longer serves a local
// model carries none; a revive replays the dispatch's list. A store without the surface, or a
// write that fails, only costs the run page its list: dispatch goes on.
func (s *Server) recordModelAllowlists(ctx context.Context, runID uuid.UUID, lists map[string][]string) {
	st, ok := s.cfg.Store.(store.RunModelAllowlists)
	if !ok {
		return
	}
	if err := st.SetRunModelAllowlists(ctx, runID, lists); err != nil {
		slog.WarnContext(ctx, "api: could not record the run's model allowlists", "run_id", runID, "error", err)
	}
}

// runModelAllowlists is the record GET /runs/{id} serves as model_allowlists; nil when none is held
// or it cannot be read.
func (s *Server) runModelAllowlists(ctx context.Context, id uuid.UUID) map[string][]string {
	st, ok := s.cfg.Store.(store.RunModelAllowlists)
	if !ok {
		return nil
	}
	lists, err := st.RunModelAllowlists(ctx, id)
	if err != nil {
		slog.WarnContext(ctx, "api: could not read the run's model allowlists", "run_id", id, "error", err)
		return nil
	}
	return lists
}

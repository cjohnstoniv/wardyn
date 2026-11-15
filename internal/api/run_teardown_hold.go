// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"log/slog"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// runTeardownHoldStore is the PG read behind GET /runs/{id}/containment's teardown_hold.
type runTeardownHoldStore interface {
	GetRunTeardownHold(ctx context.Context, runID uuid.UUID) (store.RunTeardownHoldRow, error)
}

var _ runTeardownHoldStore = store.PG{}

// runTeardownHold is the run's teardown hold as its runner last reported it on a state page: nil when it
// reported none, or the store keeps no such report.
func (s *Server) runTeardownHold(ctx context.Context, runID uuid.UUID) (*types.RunTeardownHold, error) {
	st, ok := s.cfg.Store.(runTeardownHoldStore)
	if !ok {
		return nil, nil
	}
	h, err := st.GetRunTeardownHold(ctx, runID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &types.RunTeardownHold{Kind: h.Kind, Since: h.Since, ClearedAt: h.ClearedAt}, nil
}

// teardownHoldClearedAfterRefusal reports whether run's runner has reported that the kill or end it held the
// run for finished after it last refused a kill of run's sandbox: the runner has since finished that kill on
// its own, so a kill queued again is applied. False off Postgres or on a failed read, which leaves the ref
// as a refused kill always did.
func (s *Server) teardownHoldClearedAfterRefusal(ctx context.Context, run types.AgentRun) bool {
	st, ok := s.cfg.Store.(store.PG)
	if !ok {
		return false
	}
	cleared, err := st.RunTeardownHoldClearedAfterRefusal(ctx, run.ID, run.SandboxRef)
	if err != nil {
		slog.WarnContext(ctx, "wardynd: orphan sweep could not read the runner's last teardown answer",
			slog.String("run_id", run.ID.String()), slog.Any("err", err))
		return false
	}
	return cleared
}

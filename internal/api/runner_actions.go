// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"

	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/google/uuid"
)

// maxActionResultBytes bounds one action_result event body.
const maxActionResultBytes = 1024

// PendingFromRunner is the PENDING source of a fresh session: one bounded page
// of the runner's oldest open actions. StateFromRunner refuses READY while an
// open action remains, so the next fresh session carries the next page.
func (s *Server) PendingFromRunner(ctx context.Context, runnerID uuid.UUID) ([]runnerwire.PendingAction, error) {
	st, ok := s.cfg.Store.(store.RunnerStore)
	if !ok {
		return nil, store.ErrRunnerActionRefused
	}
	page, _, err := st.ListPendingRunnerActions(ctx, runnerID)
	if err != nil {
		return nil, err
	}
	out := make([]runnerwire.PendingAction, 0, len(page))
	for _, a := range page {
		out = append(out, runnerwire.PendingAction{Retirement: a.Retirement, Quarantine: a.Quarantine, Recovery: a.Recovery,
			ID: a.ID, Kind: a.Kind, RunID: a.RunID, Ref: a.Ref})
	}
	return out, nil
}

// ActionResultFromRunner is the checked Hub.Event action_result adapter.
// runnerID is the stream's authenticated stored identity; the store binds the
// event's run and the action to it in the committing transaction.
func (s *Server) ActionResultFromRunner(ctx context.Context, runnerID uuid.UUID, event runnerwire.Event) error {
	st, ok := s.cfg.Store.(store.RunnerStore)
	if !ok || event.Kind != runnerwire.EventActionResult || event.RunID == nil || len(event.Data) > maxActionResultBytes || s.cfg.RunnerAuditPrepare == nil {
		return store.ErrRunnerActionRefused
	}
	var result runnerwire.ActionResult
	dec := json.NewDecoder(bytes.NewReader(event.Data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&result); err != nil {
		return store.ErrRunnerActionRefused
	}
	if err := dec.Decode(new(any)); !errors.Is(err, io.EOF) {
		return store.ErrRunnerActionRefused
	}
	_, _, err := st.RecordRunnerActionResult(ctx, runnerID, *event.RunID, result.ID, result.Outcome, result.ObservedAt, s.cfg.RunnerAuditPrepare)
	return err
}

// queueRunnerContainment commits STATE's stop_proxy requests before the caller
// returns reconciliation-required, so the next fresh PENDING applies them.
func queueRunnerContainment(ctx context.Context, st store.PG, runnerID uuid.UUID, requests []store.RunnerContainment) error {
	for _, c := range requests {
		lostAt := c.LostAt
		if _, _, err := st.QueueRunnerAction(ctx, types.RunnerPendingAction{ID: uuid.New(), RunnerID: runnerID, RunID: c.RunID,
			Kind: types.RunnerActionStopProxy, Ref: c.Ref, Generation: c.Generation, LostAt: &lostAt}); err != nil {
			return err
		}
	}
	return nil
}

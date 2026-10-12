// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net/http"
	"slices"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// poolRunnerLister reads a Self-Hosted pool's member runners (store.PG).
type poolRunnerLister interface {
	ListRunnerPoolRunners(ctx context.Context, poolID uuid.UUID) ([]types.Runner, error)
}

var _ poolRunnerLister = store.PG{}

// handleListRunnerPoolRunners is GET /api/v1/runner-pools/{id}/runners: a Self-Hosted pool's live runners
// as runner views (name, machine name, state, online, runs at once and running). Its owner reads their own
// pool; a security operator reads any, read-only, with each owner named. Anyone else, and a Remote
// Provided pool for anyone but an operator, gets the unknown-id 404. A Remote Provided pool has no
// runners, so an operator reads [].
func (s *Server) handleListRunnerPoolRunners(w http.ResponseWriter, r *http.Request) {
	ps, ok := s.runnerPoolStore(w)
	if !ok {
		return
	}
	row, ok := s.poolFromPath(w, r, ps, false)
	if !ok {
		return
	}
	operator := s.isSecurityOperator(r.Context())
	owner := row.Pool.HostingType == types.RunnerPoolSelfHosted && row.Pool.Owner != "" && row.Pool.Owner == poolOwner(r.Context())
	if !owner && !operator {
		writePoolNotFound(w)
		return
	}
	rs, ok := s.runnerInventoryStore(w)
	if !ok {
		return
	}
	lister, ok := s.cfg.Store.(poolRunnerLister)
	if !ok {
		writeErrorReason(w, http.StatusNotImplemented, reasonRunnerRegistrationUnavailable, "runner inventory requires the Postgres store backend")
		return
	}
	rows := []types.Runner{}
	if row.Pool.HostingType == types.RunnerPoolSelfHosted {
		var err error
		if rows, err = lister.ListRunnerPoolRunners(r.Context(), row.Pool.ID); err != nil {
			writeServerError(w, r, "list runner pool runners", err)
			return
		}
		rows = slices.DeleteFunc(rows, func(row types.Runner) bool { return row.State == types.RunnerRevoked })
	}
	if views, ok := s.runnerViews(w, r, rs, rows, !owner); ok {
		writeJSON(w, http.StatusOK, append([]types.RunnerView{}, views...))
	}
}

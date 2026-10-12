// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"net/http"
	"slices"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// poolRunnerLister reads a user pool's member runners (store.PG).
type poolRunnerLister interface {
	ListRunnerPoolRunners(ctx context.Context, poolID uuid.UUID) ([]types.Runner, error)
}

var _ poolRunnerLister = store.PG{}

// handleListRunnerPoolMembers is GET /api/v1/runner-pools/{id}/members: a pool's runners, both kinds.
// A self-hosted member carries its runner view (name, machine name, state, online, runs at once and
// running), only ever a runner the pool's owner holds; a remote-hosted member its name, whether this
// server runs it and whether it answered its last health read. A user pool's owner reads their own pool;
// a security operator reads any, read-only, with each runner's owner named. Anyone else, and an
// organization pool for anyone but an operator, gets the unknown-id 404.
func (s *Server) handleListRunnerPoolMembers(w http.ResponseWriter, r *http.Request) {
	ps, ok := s.runnerPoolStore(w)
	if !ok {
		return
	}
	row, ok := s.poolFromPath(w, r, ps, false)
	if !ok {
		return
	}
	operator := s.isSecurityOperator(r.Context())
	owner := row.Pool.Scope == types.RunnerPoolUser && row.Pool.Owner != "" && row.Pool.Owner == poolOwner(r.Context())
	if !owner && !operator {
		writePoolNotFound(w)
		return
	}
	out := []client.RunnerPoolMemberView{}
	if row.Pool.Scope == types.RunnerPoolUser {
		rs, ok := s.runnerInventoryStore(w)
		if !ok {
			return
		}
		lister, ok := s.cfg.Store.(poolRunnerLister)
		if !ok {
			writeErrorReason(w, http.StatusNotImplemented, reasonRunnerRegistrationUnavailable, "runner inventory requires the Postgres store backend")
			return
		}
		rows, err := lister.ListRunnerPoolRunners(r.Context(), row.Pool.ID)
		if err != nil {
			writeServerError(w, r, "list runner pool runners", err)
			return
		}
		rows = slices.DeleteFunc(rows, func(row types.Runner) bool { return row.State == types.RunnerRevoked })
		views, ok := s.runnerViews(w, r, rs, rows, !owner)
		if !ok {
			return
		}
		for i := range views {
			v := views[i]
			out = append(out, client.RunnerPoolMemberView{Kind: types.RunnerKindSelfHosted, ID: v.ID.String(), Name: v.Name, Online: v.Online, Runner: &v})
		}
	}
	if len(row.ExecutorIDs) > 0 {
		rrs, ok := s.remoteRunnerStore(w)
		if !ok {
			return
		}
		for _, id := range row.ExecutorIDs {
			m := client.RunnerPoolMemberView{Kind: types.RunnerKindRemoteHosted, ID: id, Name: id, Configured: s.remoteRunnerConfigured(id)}
			switch rr, err := rrs.GetRemoteRunner(r.Context(), id); {
			case err == nil:
				m.Name = rr.Runner.Name
			case !errors.Is(err, store.ErrNotFound):
				writeServerError(w, r, "read remote runner", err)
				return
			}
			if m.Configured {
				_, m.Online = s.executorFacts(r.Context(), id)
			}
			out = append(out, m)
		}
	}
	writeJSON(w, http.StatusOK, out)
}

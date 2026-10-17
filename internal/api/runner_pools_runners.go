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

// handleListRunnerPoolMembers is GET /api/v1/runner-pools/{id}/members: a pool's runners, both kinds,
// for whoever may read the pool itself (GET /runner-pools/{id}): anyone who may use an organization
// pool, a user pool's owner, and a security operator read-only with each runner's owner named. A
// self-hosted member carries its runner view (name, machine name, state, online, limits and usage),
// only ever a runner the pool's owner holds; a remote-hosted member its name, whether this server runs
// it, whether it answered its last health read, and its limits and usage (every run on it). Another person's user pool is not in the
// caller's catalogue, so it answers the unknown-id 404.
func (s *Server) handleListRunnerPoolMembers(w http.ResponseWriter, r *http.Request) {
	ps, ok := s.runnerPoolStore(w)
	if !ok {
		return
	}
	row, ok := s.poolFromPath(w, r, ps, false)
	if !ok {
		return
	}
	owner := row.Pool.Owner != "" && row.Pool.Owner == poolOwner(r.Context())
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
			out = append(out, client.RunnerPoolMemberView{Kind: types.RunnerKindSelfHosted, ID: v.ID.String(), Name: v.Name, Online: v.Online, Runner: &v,
				Limits: v.Limits, Usage: v.Usage})
		}
	}
	if len(row.ExecutorIDs) > 0 {
		rrs, ok := s.remoteRunnerStore(w)
		if !ok {
			return
		}
		used, err := s.runnersInUse(r.Context(), nil, row.ExecutorIDs)
		if err != nil {
			writeServerError(w, r, "read remote runner usage", err)
			return
		}
		for _, id := range row.ExecutorIDs {
			m := client.RunnerPoolMemberView{Kind: types.RunnerKindRemoteHosted, ID: id, Name: id, Configured: s.remoteRunnerConfigured(id)}
			rr := types.RemoteRunner{ID: id}
			switch got, err := rrs.GetRemoteRunner(r.Context(), id); {
			case err == nil:
				rr = got.Runner
				m.Name, m.Limits = rr.Name, rr.Limits
			case !errors.Is(err, store.ErrNotFound):
				writeServerError(w, r, "read remote runner", err)
				return
			}
			m.Usage = s.remoteUsage(r.Context(), rr, used[id])
			m.Online = m.Usage.Online
			out = append(out, m)
		}
	}
	writeJSON(w, http.StatusOK, out)
}

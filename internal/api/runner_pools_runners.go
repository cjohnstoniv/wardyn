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
	ListOrgRunnerPoolRunners(ctx context.Context, poolID uuid.UUID) ([]types.Runner, error)
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
	if !s.appendRunnerMembers(w, r, row.Pool, owner, &out) {
		return
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

// appendRunnerMembers adds the self-hosted members to out: a user pool's runners its owner holds, or an
// organization pool's organization runners. Every reader of an organization pool sees the runner; only a security
// operator also sees who owns, minted, trusted and revoked it, as the administrators' inventory shows them.
// It reports false after answering the request itself.
func (s *Server) appendRunnerMembers(w http.ResponseWriter, r *http.Request, pool types.RunnerPool, owner bool, out *[]client.RunnerPoolMemberView) bool {
	if _, ok := s.cfg.Store.(store.OrgRunnerPoolStore); !ok && pool.Scope != types.RunnerPoolUser {
		return true // a store that cannot add an organization runner holds none
	}
	rs, ok := s.runnerInventoryStore(w)
	if !ok {
		return false
	}
	lister, ok := s.cfg.Store.(poolRunnerLister)
	if !ok {
		writeErrorReason(w, http.StatusNotImplemented, reasonRunnerRegistrationUnavailable, "runner inventory requires the Postgres store backend")
		return false
	}
	// A user pool is read by its owner (the person's own view) or by a security operator, who sees what the
	// inventory shows. An organization runner has no person's own view: the inventory's, or less.
	list, admin := lister.ListRunnerPoolRunners, !owner
	if pool.Scope != types.RunnerPoolUser {
		list, admin = lister.ListOrgRunnerPoolRunners, true
	}
	rows, err := list(r.Context(), pool.ID)
	if err != nil {
		writeServerError(w, r, "list runner pool runners", err)
		return false
	}
	if pool.Scope == types.RunnerPoolUser {
		rows = slices.DeleteFunc(rows, func(row types.Runner) bool { return row.State == types.RunnerRevoked })
	}
	views, ok := s.runnerViews(w, r, rs, rows, admin)
	if !ok {
		return false
	}
	for i := range views {
		v := views[i]
		if pool.Scope != types.RunnerPoolUser && !s.isSecurityOperator(r.Context()) {
			withoutPeople(&v)
		}
		*out = append(*out, client.RunnerPoolMemberView{Kind: types.RunnerKindSelfHosted, ID: v.ID.String(), Name: v.Name, Online: v.Online, Runner: &v,
			Limits: v.Limits, Usage: v.Usage})
	}
	return true
}

// withoutPeople clears the fields of a runner view that name the people behind it (its owner, who minted,
// trusted and revoked it, and why), which only a security operator or the runner's own owner reads.
func withoutPeople(v *types.RunnerView) {
	v.Owner, v.MintedBy, v.MintedByEmail = "", "", ""
	v.TrustSetBy, v.TrustSetAt = "", nil
	v.RevokedBy, v.RevokedByEmail, v.RevokeReason = "", "", ""
}

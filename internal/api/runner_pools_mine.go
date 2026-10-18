// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/erasure"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// A person's own user pools: the signed-in person creates, changes and deletes them
// (runnerPersonalOwner; an administrative, delegated or device credential has no personal subject).
// Another person's pool answers every verb exactly as an unknown id does. A pool's own limits are its
// owner's to set, and every run in it is still bounded by the owner's governance profile, as any pool's.

func (s *Server) handleCreateMyRunnerPool(w http.ResponseWriter, r *http.Request) {
	ps, ok := s.runnerPoolStore(w)
	if !ok {
		return
	}
	owner, ok := s.runnerPersonalOwner(w, r)
	if !ok {
		return
	}
	var req client.CreateMyRunnerPoolRequest
	if !decodeStrict(w, r, &req) {
		return
	}
	if err := types.ValidateRunnerPoolName(req.Name); err != nil {
		writePoolInvalid(w, err.Error())
		return
	}
	s.createPool(w, r, ps, types.RunnerPool{ID: uuid.New(), Name: req.Name, Scope: types.RunnerPoolUser, Owner: owner, Limits: req.Limits, Recording: req.Recording},
		http.StatusUnprocessableEntity, "You already have 10 user pools, the most one person may keep. Delete one to make another.")
}

func (s *Server) handleUpdateMyRunnerPool(w http.ResponseWriter, r *http.Request) {
	ps, ok := s.runnerPoolStore(w)
	if !ok {
		return
	}
	if cur, ok := s.myPool(w, r, ps); ok {
		s.updatePool(w, r, ps, cur)
	}
}

func (s *Server) handleDeleteMyRunnerPool(w http.ResponseWriter, r *http.Request) {
	ps, ok := s.runnerPoolStore(w)
	if !ok {
		return
	}
	if cur, ok := s.myPool(w, r, ps); ok {
		s.deletePool(w, r, ps, cur)
	}
}

// myPool reads {id} as one of the signed-in person's own live user pools. Any other pool, an
// organization pool included, is writePoolNotFound, byte for byte the answer for an id that never
// existed. ok=false: the answer is written.
func (s *Server) myPool(w http.ResponseWriter, r *http.Request, ps store.RunnerPoolStore) (types.RunnerPool, bool) {
	owner, ok := s.runnerPersonalOwner(w, r)
	if !ok {
		return types.RunnerPool{}, false
	}
	id, ok := parseIDParam(w, r, "id", "runner pool")
	if !ok {
		return types.RunnerPool{}, false
	}
	p, err := ps.GetRunnerPool(r.Context(), id)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		writeServerError(w, r, "read runner pool", err)
		return types.RunnerPool{}, false
	}
	if err != nil || p.State == types.RunnerPoolDeleted || p.Scope != types.RunnerPoolUser || p.Owner != owner {
		writePoolNotFound(w)
		return types.RunnerPool{}, false
	}
	return p, true
}

// eraseRunnerPoolsOf is the runner_pools erasure scope: every live user pool person owns is
// deleted as its owner's delete would (the runs waiting for it ended and audited run.kill), and every
// pool of theirs forgets them. The erasure's own row carries the count: a runner_pool.delete row would
// name the person the erasure is removing. Their runners themselves are untouched.
func (s *Server) eraseRunnerPoolsOf(ctx context.Context, person string) (any, error) {
	ps, ok := s.cfg.Store.(store.RunnerPoolStore)
	if !ok {
		return nil, fmt.Errorf("%w: the store has no runner pools", erasure.ErrNotAvailable)
	}
	deleted, err := ps.DeleteRunnerPoolsByOwner(ctx, person)
	if err != nil {
		return nil, err
	}
	for _, d := range deleted {
		s.auditPoolRemovalCancels(ctx, d.CancelledRuns)
	}
	return map[string]any{"runner_pools": len(deleted)}, nil
}

// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/cjohnstoniv/wardyn/internal/federation"
	"github.com/cjohnstoniv/wardyn/internal/runnerpool"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// orgRunnerInOtherPoolMsg refuses an org runner that already serves another pool: who may use an org
// runner is its one pool's use policy, so a second pool would widen it without that policy's review.
const orgRunnerInOtherPoolMsg = "This organization runner is already in another pool. Remove it from that pool first."

// handleAddRunnerPoolOrgRunner puts a claimed organization runner (K1 §2) in an organization pool. A
// person's runner, an unknown, unclaimed, revoked or other organisation's runner are answered alike,
// with the id as sent.
func (s *Server) handleAddRunnerPoolOrgRunner(w http.ResponseWriter, r *http.Request) {
	ps, ok := s.runnerPoolStore(w)
	if !ok {
		return
	}
	ors, ok := s.cfg.Store.(store.OrgRunnerPoolStore)
	if !ok {
		writePoolRefusal(w, runnerpool.Refusal{Reason: runnerpool.ReasonPoolsUnavailable})
		return
	}
	pool, ok := s.livePool(w, r, ps)
	if !ok {
		return
	}
	runnerID, ok := parseIDParam(w, r, "runner", "runner")
	if !ok {
		return
	}
	m, added, rev, err := ors.AddOrgRunnerToPool(r.Context(), pool.ID, runnerID, federation.OrgURLSHA256(s.cfg.RunnerOrgURL))
	switch {
	case errors.Is(err, store.ErrRunnerInOtherPool):
		writeErrorReason(w, runnerpool.ReasonMemberMismatch.Status(), string(runnerpool.ReasonMemberMismatch), orgRunnerInOtherPoolMsg)
		return
	case errors.Is(err, store.ErrNotFound):
		writePoolRefusal(w, runnerpool.Refusal{Reason: runnerpool.ReasonMemberMismatch, Name: pool.Name, Runner: chi.URLParam(r, "runner")})
		return
	case err != nil:
		writeServerError(w, r, "add organization runner to pool", err)
		return
	}
	status := http.StatusOK
	if added {
		status = http.StatusCreated
		s.auditPool(r, auditPoolRunnerAdd, pool.ID.String(), orgRunnerMemberAudit(pool, runnerID.String(), rev))
	}
	writeJSON(w, status, m)
}

// handleRemoveRunnerPoolOrgRunner takes an organization runner, in any state, out of an organization pool.
func (s *Server) handleRemoveRunnerPoolOrgRunner(w http.ResponseWriter, r *http.Request) {
	ps, ok := s.runnerPoolStore(w)
	if !ok {
		return
	}
	ors, ok := s.cfg.Store.(store.OrgRunnerPoolStore)
	if !ok {
		writePoolRefusal(w, runnerpool.Refusal{Reason: runnerpool.ReasonPoolsUnavailable})
		return
	}
	pool, ok := s.livePool(w, r, ps)
	if !ok {
		return
	}
	runnerID, ok := parseIDParam(w, r, "runner", "runner")
	if !ok {
		return
	}
	removed, rev, err := ors.RemoveOrgRunnerFromPool(r.Context(), pool.ID, runnerID)
	if errors.Is(err, store.ErrNotFound) {
		writePoolNotFound(w)
		return
	}
	if err != nil {
		writeServerError(w, r, "remove organization runner from pool", err)
		return
	}
	if removed {
		s.auditPool(r, auditPoolRunnerRemove, pool.ID.String(), orgRunnerMemberAudit(pool, runnerID.String(), rev))
	}
	w.WriteHeader(http.StatusNoContent)
}

func orgRunnerMemberAudit(pool types.RunnerPool, runnerID string, rev int64) map[string]any {
	return map[string]any{"runner_kind": types.RunnerKindSelfHosted, "runner_id": runnerID, "owner_kind": types.RunnerOwnerOrg, "scope": pool.Scope, "revision": rev}
}

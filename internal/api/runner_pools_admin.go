// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"errors"
	"net/http"
	"reflect"
	"slices"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runnerpool"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// The administrator's half of the catalogue: organization pools, the remote-hosted runners they hold,
// and the organisation's default. Every route is operatorOnly (routes.go). These are not governance-held
// writes: only the pool-use policy is (runner_pools_use_policy.go). A user pool is its owner's alone
// (runner_pools_mine.go): every route here answers a person's pool as an unknown id, though the
// oversight list shows it.

// noOrgUserPoolDefaultMsg refuses an organisation default naming a user pool.
const noOrgUserPoolDefaultMsg = "There is no organization default for user pools; people choose their own."

// personalBarrierDefaultMsg refuses a person's own defaults naming a barrier.
const personalBarrierDefaultMsg = "barrier is an organisation setting; an admin sets it"

// livePool reads an organization pool for an administrator's write; ok=false: the 404 or 500 is
// written. A person's user pool is the 404 an unknown id gets: no administrator writes one.
func (s *Server) livePool(w http.ResponseWriter, r *http.Request, ps store.RunnerPoolStore) (types.RunnerPool, bool) {
	id, ok := parseIDParam(w, r, "id", "runner pool")
	if !ok {
		return types.RunnerPool{}, false
	}
	p, err := ps.GetRunnerPool(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) || (err == nil && (p.State == types.RunnerPoolDeleted || p.Scope != types.RunnerPoolOrganization)) {
		writePoolNotFound(w)
		return types.RunnerPool{}, false
	}
	if err != nil {
		writeServerError(w, r, "read runner pool", err)
		return types.RunnerPool{}, false
	}
	return p, true
}

func (s *Server) handleCreateRunnerPool(w http.ResponseWriter, r *http.Request) {
	ps, ok := s.runnerPoolStore(w)
	if !ok {
		return
	}
	var req client.CreateRunnerPoolRequest
	if !decodeStrict(w, r, &req) {
		return
	}
	if err := types.ValidateRunnerPoolName(req.Name); err != nil {
		writePoolInvalid(w, err.Error())
		return
	}
	s.createPool(w, r, ps, types.RunnerPool{ID: uuid.New(), Name: req.Name, Scope: types.RunnerPoolOrganization, Limits: req.Limits, Recording: req.Recording},
		http.StatusBadRequest, "the catalogue holds at most 200 pools")
}

// createPool validates p's limits, stores it and audits it; fullStatus and full answer a pool cap.
func (s *Server) createPool(w http.ResponseWriter, r *http.Request, ps store.RunnerPoolStore, p types.RunnerPool, fullStatus int, full string) {
	if !p.Recording.Valid() {
		writePoolInvalid(w, "recording is preferred or required")
		return
	}
	if s.refuseRecordingWithoutStore(w, p.Recording) {
		return
	}
	if p.Limits != nil {
		if err := p.Limits.Validate(); err != nil {
			writePoolInvalid(w, err.Error())
			return
		}
	}
	p, err := ps.CreateRunnerPool(r.Context(), p, principalFromRequest(r))
	switch {
	case errors.Is(err, store.ErrConflict):
		writePoolInvalid(w, "a pool with that name already exists")
		return
	case errors.Is(err, store.ErrRunnerPoolLimit):
		writeErrorReason(w, fullStatus, string(runnerpool.ReasonInvalid), full)
		return
	case err != nil:
		writeServerError(w, r, "create runner pool", err)
		return
	}
	data := map[string]any{"name": p.Name, "scope": p.Scope, "revision": p.Revision}
	if p.Limits != nil {
		data["limits"] = p.Limits
	}
	if p.Recording != "" {
		data["recording"] = p.Recording
	}
	s.auditPool(r, auditPoolCreate, p.ID.String(), withPoolOwner(data, p))
	writeJSON(w, http.StatusCreated, p)
}

// withPoolOwner adds a user pool's owner to its audit data.
func withPoolOwner(data map[string]any, p types.RunnerPool) map[string]any {
	if p.Owner != "" {
		data["owner"] = p.Owner
	}
	return data
}

func (s *Server) handleUpdateRunnerPool(w http.ResponseWriter, r *http.Request) {
	ps, ok := s.runnerPoolStore(w)
	if !ok {
		return
	}
	if cur, ok := s.livePool(w, r, ps); ok {
		s.updatePool(w, r, ps, cur)
	}
}

// updatePool applies an update request to cur, a live pool the caller may write.
func (s *Server) updatePool(w http.ResponseWriter, r *http.Request, ps store.RunnerPoolStore, cur types.RunnerPool) {
	var req client.UpdateRunnerPoolRequest
	if !decodeStrict(w, r, &req) {
		return
	}
	if err := req.Validate(); err != nil {
		writePoolInvalid(w, err.Error())
		return
	}
	if req.Recording != nil && s.refuseRecordingWithoutStore(w, *req.Recording) {
		return
	}
	// The audit row below is built from cur, so a request naming any other revision is refused before the
	// store call: one that names a revision another admin's write just made would otherwise be a store
	// no-op audited against the older pool. The store checks the revision again under the pool lock.
	if req.Revision != cur.Revision {
		writePoolRefusal(w, runnerpool.Refusal{Reason: runnerpool.ReasonStale, Name: cur.Name})
		return
	}
	var state *types.RunnerPoolState
	if req.State != nil {
		st := types.RunnerPoolState(*req.State)
		state = &st
	}
	p, err := ps.UpdateRunnerPool(r.Context(), cur.ID, req.Revision, req.Name, state, req.Limits, req.Recording)
	switch {
	case errors.Is(err, store.ErrNotFound):
		writePoolNotFound(w)
		return
	case errors.Is(err, store.ErrRunnerPoolStale):
		writePoolRefusal(w, runnerpool.Refusal{Reason: runnerpool.ReasonStale, Name: cur.Name})
		return
	case errors.Is(err, store.ErrConflict):
		writePoolInvalid(w, "a pool with that name already exists")
		return
	case err != nil:
		writeServerError(w, r, "update runner pool", err)
		return
	}
	if p.Revision != cur.Revision {
		data := map[string]any{
			"scope": p.Scope, "name_before": cur.Name, "name": p.Name,
			"state_before": cur.State, "state": p.State, "revision": p.Revision,
		}
		if !reflect.DeepEqual(cur.Limits, p.Limits) {
			data["limits_before"], data["limits"] = cur.Limits, p.Limits
		}
		if cur.Recording != p.Recording {
			data["recording_before"], data["recording"] = cur.Recording, p.Recording
		}
		s.auditPool(r, auditPoolUpdate, p.ID.String(), withPoolOwner(data, p))
		// A raised cap, or a pool switched back on, may have room for a waiting run.
		s.kickQueue()
	}
	writeJSON(w, http.StatusOK, p)
}

func (s *Server) handleDeleteRunnerPool(w http.ResponseWriter, r *http.Request) {
	ps, ok := s.runnerPoolStore(w)
	if !ok {
		return
	}
	if cur, ok := s.livePool(w, r, ps); ok {
		s.deletePool(w, r, ps, cur)
	}
}

// deletePool deletes cur, a live pool the caller may write.
func (s *Server) deletePool(w http.ResponseWriter, r *http.Request, ps store.RunnerPoolStore, cur types.RunnerPool) {
	d, err := ps.DeleteRunnerPool(r.Context(), cur.ID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		writePoolNotFound(w)
		return
	case errors.Is(err, store.ErrRunnerPoolHasPolicy):
		writeErrorReason(w, http.StatusConflict, string(runnerpool.ReasonInvalid), "clear the use policy first: deleting a pool would clear it without a second human")
		return
	case err != nil:
		writeServerError(w, r, "delete runner pool", err)
		return
	}
	s.auditPoolDeletion(r, d)
	w.WriteHeader(http.StatusNoContent)
}

// auditPoolDeletion writes a delete's rows: the pool, the runs it ended and any organisation default it cleared.
func (s *Server) auditPoolDeletion(r *http.Request, d store.RunnerPoolDeletion) {
	s.auditPool(r, auditPoolDelete, d.Pool.ID.String(), withPoolOwner(map[string]any{
		"name": d.Pool.Name, "scope": d.Pool.Scope, "revision": d.Pool.Revision,
		"remote_runner_ids": nonNilStrings(d.ExecutorIDs), "runner_count": d.RunnerCount}, d.Pool))
	s.auditPoolRemovalCancels(r.Context(), d.CancelledRuns)
	if d.OrgDefaultsBefore != nil {
		s.auditPool(r, auditPoolDefaultSet, "organisation", map[string]any{"before": d.OrgDefaultsBefore, "after": d.OrgDefaultsAfter, "cause": "pool_deleted"})
	}
}

// handleAddRunnerPoolExecutor puts a configured remote-hosted runner in an organization pool. Adding a
// runner to a pool puts it in front of that pool's audience, so when another live pool already holds it
// under a use policy the add is a widening of that policy and is held for a second human, as the policy
// itself is.
func (s *Server) handleAddRunnerPoolExecutor(w http.ResponseWriter, r *http.Request) {
	ps, ok := s.runnerPoolStore(w)
	if !ok {
		return
	}
	pool, ok := s.livePool(w, r, ps)
	if !ok {
		return
	}
	executor := chi.URLParam(r, "rid")
	if s.refuseRemoteMember(w, pool, executor) {
		return
	}
	rows, ok := s.poolCatalogue(w, r, ps, false)
	if !ok {
		return
	}
	mode := govDirect
	if executorUnderOtherPolicy(rows, pool.ID, executor) {
		if mode, ok = s.governanceWriteMode(w, r); !ok {
			return
		}
		if mode == govQueue {
			row, _ := catalogueRow(rows, pool.ID)
			s.holdExecutorChange(w, r, row, executor, false)
			return
		}
	}
	m, added, rev, err := ps.AddRunnerPoolExecutor(r.Context(), pool.ID, executor)
	if errors.Is(err, store.ErrNotFound) {
		writePoolNotFound(w)
		return
	}
	if err != nil {
		writeServerError(w, r, "add runner pool remote runner", err)
		return
	}
	if added {
		s.auditPool(r, auditPoolRunnerAdd, pool.ID.String(), remoteMemberAudit(pool, executor, rev))
		if mode == govBypass {
			s.recordGovernanceBypass(r, govKindRunnerPoolUsePolicy, pool.ID.String(), "success", nil)
		}
	}
	writeJSON(w, http.StatusOK, m)
}

// executorUnderOtherPolicy reports whether a live pool other than poolID holds remote-hosted runner
// executor under a use policy.
func executorUnderOtherPolicy(rows []store.RunnerPoolCatalogueRow, poolID uuid.UUID, executor string) bool {
	return slices.ContainsFunc(rows, func(row store.RunnerPoolCatalogueRow) bool {
		return row.Pool.ID != poolID && row.Policy != nil && slices.Contains(row.ExecutorIDs, executor)
	})
}

// handleRemoveRunnerPoolExecutor needs no configured check: a remote-hosted runner that is no longer
// configured must still be removable. Taking one out of a pool that carries a use policy is held for a
// second human like an add: otherwise one administrator could remove it, so that no policy pool holds
// it any more, and add it to an open pool.
func (s *Server) handleRemoveRunnerPoolExecutor(w http.ResponseWriter, r *http.Request) {
	ps, ok := s.runnerPoolStore(w)
	if !ok {
		return
	}
	pool, ok := s.livePool(w, r, ps)
	if !ok {
		return
	}
	executor := chi.URLParam(r, "rid")
	mode, governed, done := s.detachGovernance(w, r, ps, pool.ID, executor)
	if done {
		return
	}
	removed, rev, err := ps.RemoveRunnerPoolExecutor(r.Context(), pool.ID, executor, !governed)
	if errors.Is(err, store.ErrRunnerPoolGoverned) {
		// A use policy was approved between the catalogue read and the locks: decide again on the
		// catalogue as it now stands, which holds the detach for a second human.
		if mode, governed, done = s.detachGovernance(w, r, ps, pool.ID, executor); done {
			return
		}
		removed, rev, err = ps.RemoveRunnerPoolExecutor(r.Context(), pool.ID, executor, !governed)
		if errors.Is(err, store.ErrRunnerPoolGoverned) {
			writePoolRefusal(w, runnerpool.Refusal{Reason: runnerpool.ReasonStale, Name: pool.Name})
			return
		}
	}
	if errors.Is(err, store.ErrNotFound) {
		writePoolNotFound(w)
		return
	}
	if err != nil {
		writeServerError(w, r, "remove runner pool remote runner", err)
		return
	}
	if removed {
		s.auditPool(r, auditPoolRunnerRemove, pool.ID.String(), remoteMemberAudit(pool, executor, rev))
		if mode == govBypass {
			s.recordGovernanceBypass(r, govKindRunnerPoolUsePolicy, pool.ID.String(), "success", nil)
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

// detachGovernance decides how taking remote-hosted runner executor out of the pool is written, on the
// catalogue as it stands. governed: the pool has a use policy and holds the runner, so the write mode
// was asked for.
// done: the answer is already written (the detach was held, or the mode refused).
func (s *Server) detachGovernance(w http.ResponseWriter, r *http.Request, ps store.RunnerPoolStore, poolID uuid.UUID, executor string) (mode govWriteMode, governed, done bool) {
	rows, ok := s.poolCatalogue(w, r, ps, false)
	if !ok {
		return govDirect, false, true
	}
	row, found := catalogueRow(rows, poolID)
	if !found || row.Policy == nil || !slices.Contains(row.ExecutorIDs, executor) {
		return govDirect, false, false
	}
	if mode, ok = s.governanceWriteMode(w, r); !ok {
		return mode, true, true
	}
	if mode == govQueue {
		s.holdExecutorChange(w, r, row, executor, true)
		return mode, true, true
	}
	return mode, true, false
}

// checkDefaultPool revalidates the pool a default names, as each write of a default does: the pool
// must be in rows (the writer's own catalogue), an organization pool for the organisation's default
// (orgOnly), and switched on. A default only seeds a choice, so this is a convenience for the writer and
// never the admission: every read and launch revalidates. ok=false: the refusal is written.
func checkDefaultPool(w http.ResponseWriter, d types.RunnerPoolDefaults, rows []store.RunnerPoolCatalogueRow, orgOnly bool) bool {
	if d.PoolID == nil {
		return true
	}
	row, found := catalogueRow(rows, *d.PoolID)
	switch {
	case !found:
		writePoolNotFound(w)
	case orgOnly && row.Pool.Scope != types.RunnerPoolOrganization:
		writePoolInvalid(w, noOrgUserPoolDefaultMsg)
	case row.Pool.State != types.RunnerPoolActive:
		writePoolRefusal(w, runnerpool.Refusal{Reason: runnerpool.ReasonUnavailable, Name: row.Pool.Name})
	default:
		return true
	}
	return false
}

// defaultsView keeps the pool d names only when the catalogue lists it: a default naming a pool the
// caller may not use reads as unset, so the read tells nobody a pool exists.
func defaultsView(d types.RunnerPoolDefaults, rows []store.RunnerPoolCatalogueRow) types.RunnerPoolDefaults {
	if d.PoolID != nil {
		if _, found := catalogueRow(rows, *d.PoolID); !found {
			d.PoolID = nil
		}
	}
	return d
}

func (s *Server) handleGetOrgRunnerPoolDefaults(w http.ResponseWriter, r *http.Request) {
	ps, ok := s.runnerPoolStore(w)
	if !ok {
		return
	}
	rows, ok := s.poolCatalogue(w, r, ps, false)
	if !ok {
		return
	}
	d, err := ps.GetRunnerPoolOrgDefaults(r.Context())
	if err != nil {
		writeServerError(w, r, "read organisation runner pool defaults", err)
		return
	}
	writeJSON(w, http.StatusOK, defaultsView(d, rows))
}

func (s *Server) handlePutOrgRunnerPoolDefaults(w http.ResponseWriter, r *http.Request) {
	ps, ok := s.runnerPoolStore(w)
	if !ok {
		return
	}
	var d types.RunnerPoolDefaults
	if !decodeStrict(w, r, &d) {
		return
	}
	if err := d.Validate(); err != nil {
		writePoolInvalid(w, err.Error())
		return
	}
	if s.refuseRecordingWithoutStore(w, d.Recording) {
		return
	}
	rows, ok := s.poolCatalogue(w, r, ps, false)
	if !ok || !checkDefaultPool(w, d, rows, true) {
		return
	}
	before, err := ps.GetRunnerPoolOrgDefaults(r.Context())
	if err == nil {
		err = ps.PutRunnerPoolOrgDefaults(r.Context(), d, principalFromRequest(r))
	}
	if err != nil {
		writeServerError(w, r, "write organisation runner pool defaults", err)
		return
	}
	s.auditPool(r, auditPoolDefaultSet, "organisation", map[string]any{"before": before, "after": d})
	writeJSON(w, http.StatusOK, d)
}

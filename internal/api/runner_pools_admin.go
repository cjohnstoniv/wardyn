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

// The administrator's half of the catalogue: Remote Provided pools, the executors they hold, and the
// organisation's defaults. Every route is operatorOnly (routes.go). These are not governance-held
// writes: only the pool-use policy is (runner_pools_use_policy.go). A Self-Hosted pool is its owner's
// alone (runner_pools_mine.go): create refuses one by name, and every other route here answers a
// person's pool as an unknown id, though the oversight list shows it.

// selfHostedOwnerOnlyMsg refuses an administrator's create of a Self-Hosted pool.
const selfHostedOwnerOnlyMsg = "a Self-Hosted pool belongs to the person whose runners it holds: each person creates their own"

// noOrgSelfHostedDefaultMsg refuses an organisation default naming a Self-Hosted pool or preferring
// Self-Hosted: the organisation's defaults are Remote Provided only.
const noOrgSelfHostedDefaultMsg = "the organisation has no Self-Hosted default: each person chooses their own Self-Hosted pool"

// livePool reads a Remote Provided pool for an administrator's write; ok=false: the 404 or 500 is
// written. A person's Self-Hosted pool is the 404 an unknown id gets: no administrator writes one.
func (s *Server) livePool(w http.ResponseWriter, r *http.Request, ps store.RunnerPoolStore) (types.RunnerPool, bool) {
	id, ok := parseIDParam(w, r, "id", "runner pool")
	if !ok {
		return types.RunnerPool{}, false
	}
	p, err := ps.GetRunnerPool(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) || (err == nil && (p.State == types.RunnerPoolDeleted || p.HostingType == types.RunnerPoolSelfHosted)) {
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
	switch req.HostingType {
	case types.RunnerPoolRemoteProvided:
	case types.RunnerPoolSelfHosted:
		writePoolInvalid(w, selfHostedOwnerOnlyMsg)
		return
	default:
		writePoolInvalid(w, "hosting_type is remote_provided or self_hosted")
		return
	}
	s.createPool(w, r, ps, types.RunnerPool{ID: uuid.New(), Name: req.Name, HostingType: req.HostingType, Limits: req.Limits},
		http.StatusBadRequest, "the catalogue holds at most 200 pools")
}

// createPool validates p's limits, stores it and audits it; fullStatus and full answer a pool cap.
func (s *Server) createPool(w http.ResponseWriter, r *http.Request, ps store.RunnerPoolStore, p types.RunnerPool, fullStatus int, full string) {
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
	data := map[string]any{"name": p.Name, "hosting_type": p.HostingType, "revision": p.Revision}
	if p.Limits != nil {
		data["limits"] = p.Limits
	}
	s.auditPool(r, auditPoolCreate, p.ID.String(), withPoolOwner(data, p))
	writeJSON(w, http.StatusCreated, p)
}

// withPoolOwner adds a Self-Hosted pool's owner to its audit data.
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
	p, err := ps.UpdateRunnerPool(r.Context(), cur.ID, req.Revision, req.Name, state, req.Limits)
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
			"hosting_type": p.HostingType, "name_before": cur.Name, "name": p.Name,
			"state_before": cur.State, "state": p.State, "revision": p.Revision,
		}
		if !reflect.DeepEqual(cur.Limits, p.Limits) {
			data["limits_before"], data["limits"] = cur.Limits, p.Limits
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
		"name": d.Pool.Name, "hosting_type": d.Pool.HostingType, "revision": d.Pool.Revision,
		"executor_ids": nonNilStrings(d.ExecutorIDs), "runner_count": d.RunnerCount}, d.Pool))
	s.auditPoolRemovalCancels(r.Context(), d.CancelledRuns)
	if d.OrgDefaultsBefore != nil {
		s.auditPool(r, auditPoolDefaultSet, "organisation", map[string]any{"before": d.OrgDefaultsBefore, "after": d.OrgDefaultsAfter, "cause": "pool_deleted"})
	}
}

// handleAddRunnerPoolExecutor attaches a configured executor. Attaching an executor to a pool puts it in
// front of that pool's audience, so when another live pool already holds it under a use policy the
// attach is a widening of that policy and is held for a second human, as the policy itself is.
func (s *Server) handleAddRunnerPoolExecutor(w http.ResponseWriter, r *http.Request) {
	ps, ok := s.runnerPoolStore(w)
	if !ok {
		return
	}
	pool, ok := s.livePool(w, r, ps)
	if !ok {
		return
	}
	executor := chi.URLParam(r, "executor")
	if err := (types.RunnerPoolMember{PoolID: pool.ID, ExecutorID: executor}).Validate(pool.HostingType); err != nil {
		writePoolInvalid(w, err.Error())
		return
	}
	if !slices.Contains(s.configuredExecutors(), executor) {
		writeErrorReason(w, runnerpool.ReasonMemberMismatch.Status(), string(runnerpool.ReasonMemberMismatch),
			"executor "+executor+" is not configured on this server; a remote-provided pool holds configured executors only")
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
		writeServerError(w, r, "add runner pool executor", err)
		return
	}
	if added {
		s.auditPool(r, auditPoolExecutorAdd, pool.ID.String(), map[string]any{"executor_id": executor, "hosting_type": pool.HostingType, "revision": rev})
		if mode == govBypass {
			s.recordGovernanceBypass(r, govKindRunnerPoolUsePolicy, pool.ID.String(), "success", nil)
		}
	}
	writeJSON(w, http.StatusOK, m)
}

// executorUnderOtherPolicy reports whether a live pool other than poolID holds executor under a use policy.
func executorUnderOtherPolicy(rows []store.RunnerPoolCatalogueRow, poolID uuid.UUID, executor string) bool {
	return slices.ContainsFunc(rows, func(row store.RunnerPoolCatalogueRow) bool {
		return row.Pool.ID != poolID && row.Policy != nil && slices.Contains(row.ExecutorIDs, executor)
	})
}

// handleRemoveRunnerPoolExecutor needs no configured check: an executor that is no longer configured
// must still be removable. Detaching an executor from a pool that carries a use policy is held for a
// second human like an attach: otherwise one administrator could detach it, so that no policy pool holds
// it any more, and attach it to an open pool.
func (s *Server) handleRemoveRunnerPoolExecutor(w http.ResponseWriter, r *http.Request) {
	ps, ok := s.runnerPoolStore(w)
	if !ok {
		return
	}
	pool, ok := s.livePool(w, r, ps)
	if !ok {
		return
	}
	executor := chi.URLParam(r, "executor")
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
		writeServerError(w, r, "remove runner pool executor", err)
		return
	}
	if removed {
		s.auditPool(r, auditPoolExecutorRemove, pool.ID.String(), map[string]any{"executor_id": executor, "hosting_type": pool.HostingType, "revision": rev})
		if mode == govBypass {
			s.recordGovernanceBypass(r, govKindRunnerPoolUsePolicy, pool.ID.String(), "success", nil)
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

// detachGovernance decides how a detach of executor from the pool is written, on the catalogue as it
// stands. governed: the pool has a use policy and holds the executor, so the write mode was asked for.
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

// checkDefaultPools revalidates the pools a defaults document names, as each write of a default does:
// the pool must be in rows (the writer's own catalogue), be of the hosting type of its slot, and be
// switched on. A default only seeds a choice, so this is a convenience for the writer and never the
// admission: every read and launch revalidates. ok=false: the refusal is written.
func checkDefaultPools(w http.ResponseWriter, d types.RunnerPoolDefaults, rows []store.RunnerPoolCatalogueRow) bool {
	for _, slot := range []struct {
		id      *uuid.UUID
		hosting types.RunnerPoolHosting
	}{{d.RemoteProvided, types.RunnerPoolRemoteProvided}, {d.SelfHosted, types.RunnerPoolSelfHosted}} {
		if slot.id == nil {
			continue
		}
		row, found := catalogueRow(rows, *slot.id)
		switch {
		case !found:
			writePoolNotFound(w)
		case row.Pool.HostingType != slot.hosting:
			writePoolRefusal(w, runnerpool.Refusal{Reason: runnerpool.ReasonMemberMismatch, Name: row.Pool.Name, Hosting: row.Pool.HostingType})
		case row.Pool.State != types.RunnerPoolActive:
			writePoolRefusal(w, runnerpool.Refusal{Reason: runnerpool.ReasonUnavailable, Name: row.Pool.Name})
		default:
			continue
		}
		return false
	}
	return true
}

// defaultsView keeps the ids of d the catalogue lists: a default naming a pool the caller may not use
// reads as unset, so the read tells nobody a pool exists.
func defaultsView(d types.RunnerPoolDefaults, rows []store.RunnerPoolCatalogueRow) types.RunnerPoolDefaults {
	keep := func(id *uuid.UUID) *uuid.UUID {
		if id == nil {
			return nil
		}
		if _, found := catalogueRow(rows, *id); !found {
			return nil
		}
		return id
	}
	d.RemoteProvided, d.SelfHosted = keep(d.RemoteProvided), keep(d.SelfHosted)
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
	if d.SelfHosted != nil || d.PreferredHosting == types.RunnerPoolSelfHosted {
		writePoolInvalid(w, noOrgSelfHostedDefaultMsg)
		return
	}
	rows, ok := s.poolCatalogue(w, r, ps, false)
	if !ok || !checkDefaultPools(w, d, rows) {
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

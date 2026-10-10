// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"errors"
	"net/http"
	"slices"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runnerpool"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// The administrator's half of the catalogue: pools, the executors a remote pool holds, and the
// organisation's defaults. Every route is operatorOnly (routes.go). These are not governance-held
// writes: only the pool-use policy is (runner_pools_use_policy.go).

// livePool reads a pool for an administrator's write; ok=false: the 404 or 500 is written.
func (s *Server) livePool(w http.ResponseWriter, r *http.Request, ps store.RunnerPoolStore) (types.RunnerPool, bool) {
	id, ok := parseIDParam(w, r, "id", "runner pool")
	if !ok {
		return types.RunnerPool{}, false
	}
	p, err := ps.GetRunnerPool(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) || (err == nil && p.State == types.RunnerPoolDeleted) {
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
	if !req.HostingType.Valid() {
		writePoolInvalid(w, "hosting_type is remote_provided or self_hosted")
		return
	}
	p, err := ps.CreateRunnerPool(r.Context(), types.RunnerPool{ID: uuid.New(), Name: req.Name, HostingType: req.HostingType}, principalFromRequest(r))
	switch {
	case errors.Is(err, store.ErrConflict):
		writePoolInvalid(w, "a pool with that name already exists")
		return
	case errors.Is(err, store.ErrRunnerPoolLimit):
		writePoolInvalid(w, "the catalogue holds at most 200 pools")
		return
	case err != nil:
		writeServerError(w, r, "create runner pool", err)
		return
	}
	s.auditPool(r, auditPoolCreate, p.ID.String(), map[string]any{"name": p.Name, "hosting_type": p.HostingType, "revision": p.Revision})
	writeJSON(w, http.StatusCreated, p)
}

func (s *Server) handleUpdateRunnerPool(w http.ResponseWriter, r *http.Request) {
	ps, ok := s.runnerPoolStore(w)
	if !ok {
		return
	}
	cur, ok := s.livePool(w, r, ps)
	if !ok {
		return
	}
	var req client.UpdateRunnerPoolRequest
	if !decodeStrict(w, r, &req) {
		return
	}
	if err := req.Validate(); err != nil {
		writePoolInvalid(w, err.Error())
		return
	}
	var state *types.RunnerPoolState
	if req.State != nil {
		st := types.RunnerPoolState(*req.State)
		state = &st
	}
	p, err := ps.UpdateRunnerPool(r.Context(), cur.ID, req.Revision, req.Name, state)
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
		s.auditPool(r, auditPoolUpdate, p.ID.String(), map[string]any{
			"hosting_type": p.HostingType, "name_before": cur.Name, "name": p.Name,
			"state_before": cur.State, "state": p.State, "revision": p.Revision,
		})
	}
	writeJSON(w, http.StatusOK, p)
}

func (s *Server) handleDeleteRunnerPool(w http.ResponseWriter, r *http.Request) {
	ps, ok := s.runnerPoolStore(w)
	if !ok {
		return
	}
	cur, ok := s.livePool(w, r, ps)
	if !ok {
		return
	}
	p, err := ps.DeleteRunnerPool(r.Context(), cur.ID)
	if errors.Is(err, store.ErrNotFound) {
		writePoolNotFound(w)
		return
	}
	if err != nil {
		writeServerError(w, r, "delete runner pool", err)
		return
	}
	s.auditPool(r, auditPoolDelete, p.ID.String(), map[string]any{"name": p.Name, "hosting_type": p.HostingType, "revision": p.Revision})
	w.WriteHeader(http.StatusNoContent)
}

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
		if pool.HostingType != types.RunnerPoolRemoteProvided {
			writePoolRefusal(w, runnerpool.Refusal{Reason: runnerpool.ReasonMemberMismatch, Name: pool.Name, Hosting: pool.HostingType})
			return
		}
		writePoolInvalid(w, err.Error())
		return
	}
	if !slices.Contains(s.configuredExecutors(), executor) {
		writeErrorReason(w, runnerpool.ReasonMemberMismatch.Status(), string(runnerpool.ReasonMemberMismatch),
			"executor "+executor+" is not configured on this server; a remote-provided pool holds configured executors only")
		return
	}
	m, added, err := ps.AddRunnerPoolExecutor(r.Context(), pool.ID, executor)
	if errors.Is(err, store.ErrNotFound) {
		writePoolNotFound(w)
		return
	}
	if err != nil {
		writeServerError(w, r, "add runner pool executor", err)
		return
	}
	if added {
		s.auditPool(r, auditPoolExecutorAdd, pool.ID.String(), map[string]any{
			"executor_id": executor, "hosting_type": pool.HostingType, "revision": poolRevision(r, ps, pool.ID)})
	}
	writeJSON(w, http.StatusOK, m)
}

// handleRemoveRunnerPoolExecutor needs no configured check: an executor that is no longer configured
// must still be removable.
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
	removed, err := ps.RemoveRunnerPoolExecutor(r.Context(), pool.ID, executor)
	if errors.Is(err, store.ErrNotFound) {
		writePoolNotFound(w)
		return
	}
	if err != nil {
		writeServerError(w, r, "remove runner pool executor", err)
		return
	}
	if removed {
		s.auditPool(r, auditPoolExecutorRemove, pool.ID.String(), map[string]any{
			"executor_id": executor, "hosting_type": pool.HostingType, "revision": poolRevision(r, ps, pool.ID)})
	}
	w.WriteHeader(http.StatusNoContent)
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

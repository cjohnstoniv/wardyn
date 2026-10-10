// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/federation"
	"github.com/cjohnstoniv/wardyn/internal/runnerpool"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The person's half of the catalogue: adding or removing their own claimed runner, and their own
// defaults. Both act only on the signed-in person (runnerPersonalOwner): no route takes an owner or a
// principal, and an administrative, delegated or device credential has no personal subject, so it
// can neither add a runner nor write a preference.

// handleAddMyRunnerToPool puts the caller's own claimed runner in a self-hosted pool they may use. A
// runner that is unknown, unclaimed, revoked, bound to another organisation or another person's is
// answered alike, with the runner id as the caller sent it and never a name.
func (s *Server) handleAddMyRunnerToPool(w http.ResponseWriter, r *http.Request) {
	ps, ok := s.runnerPoolStore(w)
	if !ok {
		return
	}
	owner, ok := s.runnerPersonalOwner(w, r)
	if !ok {
		return
	}
	runnerID, ok := parseIDParam(w, r, "runner", "runner")
	if !ok {
		return
	}
	row, ok := s.poolFromPath(w, r, ps, true)
	if !ok {
		return
	}
	pool := row.Pool
	if err := (types.RunnerPoolMember{PoolID: pool.ID, RunnerID: &runnerID}).Validate(pool.HostingType); err != nil {
		writePoolRefusal(w, runnerpool.Refusal{Reason: runnerpool.ReasonMemberMismatch, Name: pool.Name, Hosting: pool.HostingType})
		return
	}
	m, added, err := ps.AddOwnRunnerToPool(r.Context(), pool.ID, runnerID, owner, federation.OrgURLSHA256(s.cfg.RunnerOrgURL))
	if errors.Is(err, store.ErrNotFound) {
		writePoolRefusal(w, runnerpool.Refusal{Reason: runnerpool.ReasonMemberMismatch, Name: pool.Name, Runner: chi.URLParam(r, "runner")})
		return
	}
	if err != nil {
		writeServerError(w, r, "add runner to pool", err)
		return
	}
	status := http.StatusOK
	if added {
		status = http.StatusCreated
		s.auditPool(r, auditPoolRunnerAdd, pool.ID.String(), map[string]any{
			"runner_id": runnerID, "hosting_type": pool.HostingType, "revision": poolRevision(r, ps, pool.ID)})
	}
	writeJSON(w, status, m)
}

func (s *Server) handleRemoveMyRunnerFromPool(w http.ResponseWriter, r *http.Request) {
	ps, ok := s.runnerPoolStore(w)
	if !ok {
		return
	}
	owner, ok := s.runnerPersonalOwner(w, r)
	if !ok {
		return
	}
	runnerID, ok := parseIDParam(w, r, "runner", "runner")
	if !ok {
		return
	}
	row, ok := s.poolFromPath(w, r, ps, true)
	if !ok {
		return
	}
	removed, err := ps.RemoveOwnRunnerFromPool(r.Context(), row.Pool.ID, runnerID, owner)
	if errors.Is(err, store.ErrNotFound) {
		writePoolNotFound(w)
		return
	}
	if err != nil {
		writeServerError(w, r, "remove runner from pool", err)
		return
	}
	if removed {
		s.auditPool(r, auditPoolRunnerRemove, row.Pool.ID.String(), map[string]any{
			"runner_id": runnerID, "hosting_type": row.Pool.HostingType, "revision": poolRevision(r, ps, row.Pool.ID)})
	}
	w.WriteHeader(http.StatusNoContent)
}

// myDefaultsStore is the store both personal-defaults halves need: the pool catalogue to revalidate
// against and the principal preferences that hold the document. A store without either answers 501,
// so a configured default is never silently dropped.
func (s *Server) myDefaultsStore(w http.ResponseWriter) (store.RunnerPoolStore, store.PrincipalPrefStore, bool) {
	ps, ok := s.runnerPoolStore(w)
	if !ok {
		return nil, nil, false
	}
	prefs, ok := s.cfg.Store.(store.PrincipalPrefStore)
	if !ok {
		writeErrorReason(w, runnerpool.ReasonPoolsUnavailable.Status(), string(runnerpool.ReasonPoolsUnavailable), runnerpool.UnavailableServerMsg())
	}
	return ps, prefs, ok
}

// handleGetMyRunnerPoolDefaults reads the signed-in person's own document, as stored: an unset field
// inherits the organisation's. A read that fails is an error, never an empty document.
func (s *Server) handleGetMyRunnerPoolDefaults(w http.ResponseWriter, r *http.Request) {
	_, prefs, ok := s.myDefaultsStore(w)
	if !ok {
		return
	}
	owner, ok := s.runnerPersonalOwner(w, r)
	if !ok {
		return
	}
	raw, err := prefs.GetPrincipalPref(r.Context(), owner, types.RunnerPoolDefaultsPrefKey)
	var d types.RunnerPoolDefaults
	if err == nil {
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		err = dec.Decode(&d)
	}
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		writeServerError(w, r, "read runner pool defaults", err)
		return
	}
	writeJSON(w, http.StatusOK, d)
}

// handlePutMyRunnerPoolDefaults replaces the signed-in person's defaults. Each pool is revalidated
// against what this person may use right now (id, hosting type of its slot, switched on): an
// administrator's role grants no pool a use policy withholds.
func (s *Server) handlePutMyRunnerPoolDefaults(w http.ResponseWriter, r *http.Request) {
	ps, prefs, ok := s.myDefaultsStore(w)
	if !ok {
		return
	}
	owner, ok := s.runnerPersonalOwner(w, r)
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
	rows, ok := s.poolCatalogue(w, r, ps, true)
	if !ok || !checkDefaultPools(w, d, rows) {
		return
	}
	s.putMyDefaults(w, r, prefs, owner, d, auditPersonDefaultSet)
}

// handleDeleteMyRunnerPoolDefaults clears the document, so every field inherits again.
func (s *Server) handleDeleteMyRunnerPoolDefaults(w http.ResponseWriter, r *http.Request) {
	_, prefs, ok := s.myDefaultsStore(w)
	if !ok {
		return
	}
	owner, ok := s.runnerPersonalOwner(w, r)
	if !ok {
		return
	}
	s.putMyDefaults(w, r, prefs, owner, types.RunnerPoolDefaults{}, auditPersonDefaultClear)
}

func (s *Server) putMyDefaults(w http.ResponseWriter, r *http.Request, prefs store.PrincipalPrefStore, owner string, d types.RunnerPoolDefaults, action string) {
	if err := prefs.PutPrincipalPref(r.Context(), owner, types.RunnerPoolDefaultsPrefKey, mustJSON(d)); err != nil {
		writeServerError(w, r, "write runner pool defaults", err)
		return
	}
	s.auditPool(r, action, "runner_pool_defaults", map[string]any{"preferred_hosting": d.PreferredHosting, "remote_provided": idText(d.RemoteProvided), "self_hosted": idText(d.SelfHosted)})
	if action == auditPersonDefaultClear {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	writeJSON(w, http.StatusOK, d)
}

func idText(id *uuid.UUID) string {
	if id == nil {
		return ""
	}
	return id.String()
}

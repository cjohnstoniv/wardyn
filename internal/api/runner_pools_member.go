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

// The person's half of the catalogue: adding or removing their own claimed self-hosted runner, and
// their own default pool. Both act only on the signed-in person (runnerPersonalOwner): no route takes an owner or a
// principal, and an administrative, delegated or device credential has no personal subject, so it
// can neither add a runner nor write a preference.

// handleAddMyRunnerToPool puts the caller's own claimed self-hosted runner in their own user pool. A
// runner that is unknown, unclaimed, revoked, bound to another organisation or another person's is
// answered alike, with the runner id as the caller sent it and never a name; an organization pool is
// told it takes remote-hosted runners only.
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
	if pool.Scope != types.RunnerPoolUser {
		writeErrorReason(w, runnerpool.ReasonMemberMismatch.Status(), string(runnerpool.ReasonMemberMismatch), runnerpool.OrgPoolSelfHostedMsg())
		return
	}
	m, added, rev, err := ps.AddOwnRunnerToPool(r.Context(), pool.ID, runnerID, owner, federation.OrgURLSHA256(s.cfg.RunnerOrgURL))
	if errors.Is(err, store.ErrRunnerPoolScope) {
		writeErrorReason(w, runnerpool.ReasonMemberMismatch.Status(), string(runnerpool.ReasonMemberMismatch), runnerpool.OrgPoolSelfHostedMsg())
		return
	}
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
		s.auditPool(r, auditPoolRunnerAdd, pool.ID.String(), withPoolOwner(map[string]any{"runner_kind": types.RunnerKindSelfHosted, "runner_id": runnerID, "scope": pool.Scope, "revision": rev}, pool))
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
	removed, rev, err := ps.RemoveOwnRunnerFromPool(r.Context(), row.Pool.ID, runnerID, owner)
	if errors.Is(err, store.ErrNotFound) {
		writePoolNotFound(w)
		return
	}
	if err != nil {
		writeServerError(w, r, "remove runner from pool", err)
		return
	}
	if removed {
		s.auditPool(r, auditPoolRunnerRemove, row.Pool.ID.String(), withPoolOwner(map[string]any{"runner_kind": types.RunnerKindSelfHosted, "runner_id": runnerID, "scope": row.Pool.Scope, "revision": rev}, row.Pool))
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

// handleGetMyRunnerPoolDefaults reads the signed-in person's own default pool: unset, the organisation's
// applies, and a default naming a pool the person may no longer use reads as unset (the resolver reads
// the stored value and refuses it). A read that fails is an error, never an empty document.
func (s *Server) handleGetMyRunnerPoolDefaults(w http.ResponseWriter, r *http.Request) {
	ps, prefs, ok := s.myDefaultsStore(w)
	if !ok {
		return
	}
	owner, ok := s.runnerPersonalOwner(w, r)
	if !ok {
		return
	}
	raw, err := prefs.GetPrincipalPref(r.Context(), owner, types.RunnerPoolDefaultPrefKey)
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
	rows, ok := s.poolCatalogue(w, r, ps, true)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, defaultsView(d, rows))
}

// handlePutMyRunnerPoolDefaults sets the signed-in person's default pool. The pool is revalidated against
// what this person may use right now (in their own catalogue, switched on): an administrator's role
// grants no pool a use policy withholds, and another person's user pool is not in it.
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
	if d.Barrier != "" {
		writePoolInvalid(w, personalBarrierDefaultMsg)
		return
	}
	if d.Revision != 0 {
		writePoolInvalid(w, personalRevisionDefaultMsg)
		return
	}
	rows, ok := s.poolCatalogue(w, r, ps, true)
	if !ok || !checkDefaultPool(w, d, rows, false) {
		return
	}
	s.putMyDefaults(w, r, prefs, owner, d, auditPersonDefaultSet)
}

// handleDeleteMyRunnerPoolDefaults clears the person's default, so the organisation's applies again.
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
	if err := prefs.PutPrincipalPref(r.Context(), owner, types.RunnerPoolDefaultPrefKey, mustJSON(d)); err != nil {
		writeServerError(w, r, "write runner pool defaults", err)
		return
	}
	s.auditPool(r, action, "runner_pool_defaults", map[string]any{"pool_id": idText(d.PoolID)})
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

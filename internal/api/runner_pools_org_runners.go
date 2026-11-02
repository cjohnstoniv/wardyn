// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"slices"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/federation"
	"github.com/cjohnstoniv/wardyn/internal/runnerpool"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// orgRunnerInOtherPoolMsg refuses an org runner that already serves another pool: who may use an org
// runner is its one pool's use policy, so a second pool would widen it without that policy's review.
const orgRunnerInOtherPoolMsg = "This organization runner is already in another pool. Remove it from that pool first."

// orgRunnerNamespaceTakenMsg refuses a runs namespace the org runner already serves for another pool.
func orgRunnerNamespaceTakenMsg(ns string) string {
	return "This runner already serves " + ns + " for another pool."
}

// orgRunnerNamespaceFixedMsg refuses a different runs namespace for a runner already in the pool: the
// namespace is set when the runner is added.
func orgRunnerNamespaceFixedMsg(ns string) string {
	if ns == "" {
		ns = "its default namespace"
	}
	return "This runner is already in this pool, serving " + ns + ". Remove it from the pool and add it again to change that."
}

// orgRunnerNamespaceMsg refuses a runs namespace that is not a Kubernetes namespace name.
const orgRunnerNamespaceMsg = "runs_namespace must be a Kubernetes namespace name: lowercase letters, digits and -, at most 63 characters, starting and ending with a letter or digit."

// addOrgRunnerBody is the optional body of the add: the Kubernetes namespace the runner serves the pool
// in, empty for the runner's default (first) namespace.
type addOrgRunnerBody struct {
	RunsNamespace string `json:"runs_namespace"`
}

// readAddOrgRunnerBody reads the optional add body; it reports false after answering the request itself.
func readAddOrgRunnerBody(w http.ResponseWriter, r *http.Request) (addOrgRunnerBody, bool) {
	var req addOrgRunnerBody
	raw, ok := readCappedBody(w, r, maxJSONBody, "request body")
	if !ok {
		return req, false
	}
	if len(bytes.TrimSpace(raw)) > 0 {
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil {
			writeErrorReason(w, http.StatusBadRequest, reasonInvalidRequestBody, "invalid JSON body: "+err.Error())
			return req, false
		}
	}
	if !types.ValidRunsNamespace(req.RunsNamespace) {
		writeErrorReason(w, http.StatusBadRequest, reasonInvalidRequestBody, orgRunnerNamespaceMsg)
		return req, false
	}
	return req, true
}

// handleAddRunnerPoolOrgRunner puts a claimed organization runner (K1 §2) in an organization pool, serving
// the runs namespace the body names (none: the runner's default). A person's runner, an unknown,
// unclaimed, revoked or other organisation's runner are answered alike, with the id as sent.
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
	body, ok := readAddOrgRunnerBody(w, r)
	if !ok {
		return
	}
	m, added, rev, err := ors.AddOrgRunnerToPool(r.Context(), pool.ID, runnerID, federation.OrgURLSHA256(s.cfg.RunnerOrgURL), body.RunsNamespace)
	switch {
	case errors.Is(err, store.ErrRunnerInOtherPool):
		writeErrorReason(w, runnerpool.ReasonMemberMismatch.Status(), string(runnerpool.ReasonMemberMismatch), orgRunnerInOtherPoolMsg)
		return
	case errors.Is(err, store.ErrRunnerNamespaceTaken):
		writeErrorReason(w, runnerpool.ReasonMemberMismatch.Status(), string(runnerpool.ReasonMemberMismatch), orgRunnerNamespaceTakenMsg(body.RunsNamespace))
		return
	case errors.Is(err, store.ErrRunnerNamespaceFixed):
		writeErrorReason(w, runnerpool.ReasonMemberMismatch.Status(), string(runnerpool.ReasonMemberMismatch), orgRunnerNamespaceFixedMsg(m.RunsNamespace))
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
		data := orgRunnerMemberAudit(pool, runnerID.String(), rev)
		if m.RunsNamespace != "" {
			data["runs_namespace"] = m.RunsNamespace
		}
		s.auditPool(r, auditPoolRunnerAdd, pool.ID.String(), data)
	}
	writeJSON(w, status, m)
}

// handleRemoveRunnerPoolOrgRunner takes an organization runner, in any state, out of an organization pool.
// Taking one out of a pool that carries a use policy is held for a second human like a remote-hosted
// runner's removal: otherwise one administrator could remove a team's runner so that no policy pool holds
// it any more.
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
	mode, governed, done := s.orgRunnerDetachGovernance(w, r, ps, pool.ID, runnerID)
	if done {
		return
	}
	removed, rev, err := ors.RemoveOrgRunnerFromPool(r.Context(), pool.ID, runnerID, !governed)
	if errors.Is(err, store.ErrRunnerPoolGoverned) {
		// A use policy was approved between the catalogue read and the locks: decide again on the
		// catalogue as it now stands, which holds the removal for a second human.
		if mode, governed, done = s.orgRunnerDetachGovernance(w, r, ps, pool.ID, runnerID); done {
			return
		}
		removed, rev, err = ors.RemoveOrgRunnerFromPool(r.Context(), pool.ID, runnerID, !governed)
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
		writeServerError(w, r, "remove organization runner from pool", err)
		return
	}
	if removed {
		s.auditPool(r, auditPoolRunnerRemove, pool.ID.String(), orgRunnerMemberAudit(pool, runnerID.String(), rev))
		if mode == govBypass {
			s.recordGovernanceBypass(r, govKindRunnerPoolUsePolicy, pool.ID.String(), "success", nil)
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

// orgRunnerDetachGovernance decides how taking organization runner runnerID out of the pool is written,
// on the catalogue as it stands. governed: the pool has a use policy and holds the runner, so the write
// mode was asked for. done: the answer is already written (the removal was held, or the mode refused).
func (s *Server) orgRunnerDetachGovernance(w http.ResponseWriter, r *http.Request, ps store.RunnerPoolStore, poolID, runnerID uuid.UUID) (mode govWriteMode, governed, done bool) {
	rows, ok := s.poolCatalogue(w, r, ps, false)
	if !ok {
		return govDirect, false, true
	}
	row, found := catalogueRow(rows, poolID)
	if !found || row.Policy == nil || !slices.Contains(row.OrgRunnerIDs, runnerID.String()) {
		return govDirect, false, false
	}
	if mode, ok = s.governanceWriteMode(w, r); !ok {
		return mode, true, true
	}
	if mode == govQueue {
		s.holdOrgRunnerDetach(w, r, row, runnerID.String())
		return mode, true, true
	}
	return mode, true, false
}

func orgRunnerMemberAudit(pool types.RunnerPool, runnerID string, rev int64) map[string]any {
	return map[string]any{"runner_kind": types.RunnerKindSelfHosted, "runner_id": runnerID, "owner_kind": types.RunnerOwnerOrg, "scope": pool.Scope, "revision": rev}
}

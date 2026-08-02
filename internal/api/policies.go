// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"
	"strings"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// policyRequest is the POST/PUT body for a policy. It is an ALIAS of the public
// SDK type, not a copy, so server and SDK cannot drift.
type policyRequest = client.PolicyRequest

// decodePolicyRequest decodes and structurally validates a policy request body.
// It rejects unknown fields so a typo cannot silently widen behaviour (fail
// closed, mirroring LoadPolicySpec discipline), requires a non-empty name, and
// runs validatePolicySpec over the spec before any store write — policies are
// admin-gated config and a bad spec must never be persisted. Any problem is
// returned as a human-readable message the handler surfaces with HTTP 400.
func decodePolicyRequest(w http.ResponseWriter, r *http.Request) (policyRequest, string) {
	var req policyRequest
	if msg := decodeStrictMsg(w, r, &req); msg != "" {
		return policyRequest{}, msg
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" {
		return policyRequest{}, "name is required"
	}
	if err := validatePolicySpec(req.Spec); err != nil {
		return policyRequest{}, "invalid policy spec: " + err.Error()
	}
	return req, ""
}

// handleListPolicies returns policies in reverse creation order, paginated by
// ?limit=&offset= (see parseListPage).
func (s *Server) handleListPolicies(w http.ResponseWriter, r *http.Request) {
	page, ok := parseListPage(w, r, defaultListLimit)
	if !ok {
		return
	}
	var pageFn func(store.Page) ([]types.RunPolicy, error)
	if pg, ok := s.cfg.Store.(store.Pager); ok {
		pageFn = func(p store.Page) ([]types.RunPolicy, error) { return pg.ListPoliciesPage(r.Context(), p) }
	}
	servePage(w, page, pageFn, func() ([]types.RunPolicy, error) { return s.cfg.Store.ListPolicies(r.Context()) })
}

// handleGetPolicy returns one policy by id (404 when unknown).
func (s *Server) handleGetPolicy(w http.ResponseWriter, r *http.Request) {
	id, ok := parseIDParam(w, r, "id", "policy")
	if !ok {
		return
	}
	p, err := s.cfg.Store.GetPolicy(r.Context(), id)
	if notFoundIf(w, err, "policy") {
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "get policy: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, p)
}

// handleCreatePolicy validates the spec and persists a new policy. Returns 201
// with the created policy, or 400 on an invalid body/spec.
func (s *Server) handleCreatePolicy(w http.ResponseWriter, r *http.Request) {
	req, msg := decodePolicyRequest(w, r)
	if msg != "" {
		writeError(w, http.StatusBadRequest, msg)
		return
	}
	// Authoring fail-fast: a policy's user-workspace mounts/repos must be onboarded
	// (the run-create chokepoint is the load-bearing gate; this surfaces the error
	// at author time instead of at launch).
	if code, err := s.validateWorkspaceSources(r.Context(), req.Spec); err != nil {
		writeError(w, code, "workspace: "+err.Error())
		return
	}
	// Same fail-fast for the sibling reference: an api_key/git_pat/ssh_key grant
	// naming a secret that does not exist. Advisory at author time (the secret can
	// be deleted afterwards) — run-create stays the load-bearing gate.
	if code, err := s.validateInlineSecretRefs(r.Context(), req.Spec); err != nil {
		writeError(w, code, "secret: "+err.Error())
		return
	}
	now := s.cfg.Now().UTC()
	id := uuid.New()
	p := types.RunPolicy{
		ID:        id,
		Name:      req.Name,
		CreatedAt: now,
		UpdatedAt: now,
		Spec:      req.Spec,
	}
	created, err := s.cfg.Store.CreatePolicy(r.Context(), p)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "create policy: "+err.Error())
		return
	}
	s.recordAudit(r.Context(), s.auditEvent(nil, actorTypeFromRequest(r), principalFromRequest(r),
		"policy.create", id.String(), "success", mustJSON(map[string]any{
			"name": created.Name, "min_confinement_class": created.Spec.MinConfinementClass,
		})))
	writeJSON(w, http.StatusCreated, created)
}

// handleUpdatePolicy validates the spec and replaces an existing policy's name
// and spec. Returns 404 when the policy is unknown, 400 on an invalid body/spec.
func (s *Server) handleUpdatePolicy(w http.ResponseWriter, r *http.Request) {
	id, ok := parseIDParam(w, r, "id", "policy")
	if !ok {
		return
	}
	req, msg := decodePolicyRequest(w, r)
	if msg != "" {
		writeError(w, http.StatusBadRequest, msg)
		return
	}
	if code, err := s.validateWorkspaceSources(r.Context(), req.Spec); err != nil {
		writeError(w, code, "workspace: "+err.Error())
		return
	}
	if code, err := s.validateInlineSecretRefs(r.Context(), req.Spec); err != nil {
		writeError(w, code, "secret: "+err.Error())
		return
	}
	updated, err := s.cfg.Store.UpdatePolicy(r.Context(), id, req.Name, req.Spec)
	if notFoundIf(w, err, "policy") {
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "update policy: "+err.Error())
		return
	}
	s.recordAudit(r.Context(), s.auditEvent(nil, actorTypeFromRequest(r), principalFromRequest(r),
		"policy.update", id.String(), "success", mustJSON(map[string]any{
			"name": updated.Name, "min_confinement_class": updated.Spec.MinConfinementClass,
		})))
	writeJSON(w, http.StatusOK, updated)
}

// handleDeletePolicy removes a policy. Returns 404 when unknown, 204 on success.
func (s *Server) handleDeletePolicy(w http.ResponseWriter, r *http.Request) {
	id, ok := parseIDParam(w, r, "id", "policy")
	if !ok {
		return
	}
	err := s.cfg.Store.DeletePolicy(r.Context(), id)
	if notFoundIf(w, err, "policy") {
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "delete policy: "+err.Error())
		return
	}
	s.recordAudit(r.Context(), s.auditEvent(nil, actorTypeFromRequest(r), principalFromRequest(r),
		"policy.delete", id.String(), "success", nil))
	w.WriteHeader(http.StatusNoContent)
}

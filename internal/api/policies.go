// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"fmt"
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
		pageFn = func(p store.Page) ([]types.RunPolicy, error) {
			ps, err := pg.ListPoliciesPage(r.Context(), p)
			return redactPoliciesForRead(ps), err
		}
	}
	servePage(w, page, pageFn, func() ([]types.RunPolicy, error) {
		ps, err := s.cfg.Store.ListPolicies(r.Context())
		return redactPoliciesForRead(ps), err
	})
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
	writeJSON(w, http.StatusOK, redactPolicyForRead(p))
}

// redactPolicyForRead returns p with any llm_inspection.workspace_secret_values
// replaced by a count before it is ever serialized back to a caller. Belt-and-
// braces (W12-S1-1): validatePolicySpec already refuses a WRITE that sets a raw
// value (an operator authors workspace_secret_names instead — see
// types.LLMInspectionSpec), so a stored row should never carry one — but a READ
// path must never re-expose it if that invariant is ever violated (a migration,
// a direct DB edit, ...). Mirrors the run.policy.effective audit redaction
// (runs_dispatch.go) — same shape, different chokepoint. Does not mutate p's
// own LLMInspection (a fresh copy is substituted), so a caller holding the
// original is never surprised by an in-place edit.
func redactPolicyForRead(p types.RunPolicy) types.RunPolicy {
	li := p.Spec.LLMInspection
	if li == nil || len(li.WorkspaceSecretValues) == 0 {
		return p
	}
	cp := *li
	cp.WorkspaceSecretValues = []string{fmt.Sprintf("<%d value(s) redacted>", len(li.WorkspaceSecretValues))}
	p.Spec.LLMInspection = &cp
	return p
}

// redactPoliciesForRead maps redactPolicyForRead over a list read (handleListPolicies).
func redactPoliciesForRead(ps []types.RunPolicy) []types.RunPolicy {
	for i := range ps {
		ps[i] = redactPolicyForRead(ps[i])
	}
	return ps
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

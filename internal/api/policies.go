// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
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
//
// The four policy-READ redaction sites in this file (here x2, handleGetPolicy,
// handleGetDefaultPolicy) all pass isSecurityOperator rather than isOperator:
// a security admin authors governance profiles against these ceilings and
// cannot do it against a redacted copy of them. Policy WRITES stay super-only
// at the router — a stored policy is selectable CONTENT, and a security admin
// who could author one could pair any operator secret with egress of their
// choosing and simply select it.
func (s *Server) handleListPolicies(w http.ResponseWriter, r *http.Request) {
	page, ok := parseListPage(w, r, defaultListLimit)
	if !ok {
		return
	}
	var pageFn func(store.Page) ([]types.RunPolicy, error)
	if pg, ok := s.cfg.Store.(store.Pager); ok {
		pageFn = func(p store.Page) ([]types.RunPolicy, error) {
			ps, err := pg.ListPoliciesPage(r.Context(), p)
			return redactPoliciesForRead(ps, s.isSecurityOperator(r.Context())), err
		}
	}
	servePage(w, page, pageFn, func() ([]types.RunPolicy, error) {
		ps, err := s.cfg.Store.ListPolicies(r.Context())
		return redactPoliciesForRead(ps, s.isSecurityOperator(r.Context())), err
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
	writeJSON(w, http.StatusOK, redactPolicyForRead(p, s.isSecurityOperator(r.Context())))
}

// defaultPolicyResponse is GET /policies/default's body: the resolved ceiling
// spec, EMBEDDED so the shape every existing consumer parses is unchanged, plus
// the name of the governance profile it came from.
//
// governance_profile_name is ADDITIVE and omitted entirely for an unassigned
// caller. "" would be a value the console then has to special-case, while an
// absent key already decodes as "no profile" in the TS mirror. It exists
// because the spec alone cannot answer the question a member actually has: the
// New Run ceiling line and the Getting Started governance row both need to name
// WHICH profile bounds them, and the name is the only handle an admin and a
// member share.
type defaultPolicyResponse struct {
	types.RunPolicySpec
	GovernanceProfileName string `json:"governance_profile_name,omitempty"`
}

// handleGetDefaultPolicy returns THE CALLER'S ceiling — the spec a run created
// without a policy_id gets, and (per composer.Clamp / inline_policy.go) the
// same ceiling their inline policy is clamped against. W14-S1-6: previously
// unexposed by UI, CLI or API — policies.tsx's own comment said so.
// Member-reachable like the other policy reads (routes.go), since members are
// the ones actually clamped by it.
//
// ROUTED through effectiveCeiling, and this endpoint is a good part of why the
// resolver is safe to have at all. Its own documentation defines it as "the
// ceiling a member is clamped against", so the moment ceilings became
// per-principal, answering with Config.DefaultPolicy made it a lie for exactly
// the members under a profile. It is also what makes user-over-group precedence
// defensible: the REAL ceiling is readable by the person it binds, rather than
// being an inference only an admin can make.
func (s *Server) handleGetDefaultPolicy(w http.ResponseWriter, r *http.Request) {
	ceiling, err := s.effectiveCeiling(r.Context())
	if err != nil {
		writeCeilingError(w, err)
		return
	}
	resp := defaultPolicyResponse{
		RunPolicySpec: redactSpecForRead(ceiling.Spec, s.isSecurityOperator(r.Context())),
	}
	if ceiling.Profile != nil {
		resp.GovernanceProfileName = ceiling.Profile.Name
	}
	writeJSON(w, http.StatusOK, resp)
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
func redactPolicyForRead(p types.RunPolicy, operator bool) types.RunPolicy {
	p.Spec = redactSpecForRead(p.Spec, operator)
	return p
}

// redactSpecForRead is redactPolicyForRead's logic, factored out so
// handleGetDefaultPolicy (which reads a bare spec, not a stored types.RunPolicy)
// gets the same redaction without a fake wrapper row.
func redactSpecForRead(spec types.RunPolicySpec, operator bool) types.RunPolicySpec {
	if !operator {
		spec = redactSpecForMember(spec)
	}
	li := spec.LLMInspection
	if li == nil || len(li.WorkspaceSecretValues) == 0 {
		return spec
	}
	cp := *li
	cp.WorkspaceSecretValues = []string{fmt.Sprintf("<%d value(s) redacted>", len(li.WorkspaceSecretValues))}
	spec.LLMInspection = &cp
	return spec
}

// redactPoliciesForRead maps redactPolicyForRead over a list read (handleListPolicies).
func redactPoliciesForRead(ps []types.RunPolicy, operator bool) []types.RunPolicy {
	for i := range ps {
		ps[i] = redactPolicyForRead(ps[i], operator)
	}
	return ps
}

// memberSecretScopeKeys are the eligible-grant scope fields that NAME an
// operator secret. The `secret` capability exists to bound which secret names a
// member can see (it narrows GET /secrets), and a policy read handed the same
// names back to every member — including on GET /policies/default, which every
// member reaches because the ceiling is what clamps them.
var memberSecretScopeKeys = []string{"secret_name", "key_secret_ref", "known_hosts_secret_ref"}

// redactSpecForMember strips the two operator-only details a policy spec
// carries out of a MEMBER-reachable read: the host filesystem paths behind
// workspace_mounts[].source (the blessed ~/.claude credential mount among
// them) and the stored-secret names on the eligible grants. Everything else —
// the egress allowlist, the confinement floor, the grant KINDS and hosts — is
// exactly what a member is being clamped by and stays visible.
//
// Copies before it edits: the default policy is server config held for the
// process's whole life, so an in-place edit here would redact it permanently
// for the operator too.
func redactSpecForMember(spec types.RunPolicySpec) types.RunPolicySpec {
	if len(spec.WorkspaceMounts) > 0 {
		mounts := slices.Clone(spec.WorkspaceMounts)
		for i := range mounts {
			mounts[i].Source = "<redacted>"
		}
		spec.WorkspaceMounts = mounts
	}
	if len(spec.EligibleGrants) > 0 {
		grants := slices.Clone(spec.EligibleGrants)
		for i := range grants {
			grants[i].Scope = redactScopeSecretRefs(grants[i].Scope)
		}
		spec.EligibleGrants = grants
	}
	return spec
}

// redactScopeSecretRefs drops the secret-naming keys from one grant scope,
// leaving the rest (host, repos, permissions, username) intact. A scope that
// does not decode as an object is dropped whole rather than guessed at — the
// same fail-closed direction storedSecretGrantPairing's callers take.
func redactScopeSecretRefs(scope json.RawMessage) json.RawMessage {
	if len(scope) == 0 {
		return scope
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(scope, &fields); err != nil {
		return nil
	}
	found := false
	for _, k := range memberSecretScopeKeys {
		if _, ok := fields[k]; ok {
			delete(fields, k)
			found = true
		}
	}
	if !found {
		return scope
	}
	out, err := json.Marshal(fields)
	if err != nil {
		return nil
	}
	return out
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
	if code, err := s.validateInlineSecretRefs(r.Context(), s.secretOwnerFromRequest(r), req.Spec); err != nil {
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
	if errors.Is(err, store.ErrConflict) {
		// W20-S1-3: run_policies.name is UNIQUE — a duplicate name is a
		// caller-fixable 409, not a raw Postgres 500 (contrast CreateApproval's
		// existing 23505 sentinel for approvals).
		writeError(w, http.StatusConflict, fmt.Sprintf("a policy named %q already exists", req.Name))
		return
	}
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
	if code, err := s.validateInlineSecretRefs(r.Context(), s.secretOwnerFromRequest(r), req.Spec); err != nil {
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

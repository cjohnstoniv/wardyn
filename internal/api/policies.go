// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
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
// returned as a human-readable message, with its wire reason, that the handler
// surfaces with HTTP 400.
func decodePolicyRequest(w http.ResponseWriter, r *http.Request, ado adoHostsLoader) (policyRequest, string, string) {
	var req policyRequest
	if msg := decodeStrictMsg(w, r, &req); msg != "" {
		return policyRequest{}, reasonPolicyRequestInvalid, msg
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" {
		return policyRequest{}, reasonPolicyRequestInvalid, "name is required"
	}
	if len(req.Name) > maxGovernanceUnitNameLen || !controlCharFree(req.Name) { // a handle, bounded like a profile's
		return policyRequest{}, reasonPolicyRequestInvalid, "name is invalid"
	}
	repos := make([]string, len(req.Spec.WorkspaceRepos))
	for i, wr := range req.Spec.WorkspaceRepos {
		repos[i] = wr.Repo
	}
	// A read error leaves no hosts: admission refuses at launch, as for a run.
	hosts, _ := ado.forAddresses(repos...)
	canonicalizeWorkspaceRepos(req.Spec.WorkspaceRepos, hosts)
	if err := validateAuthoredPolicySpec(req.Spec); err != nil {
		return policyRequest{}, specRefusalReason(err, reasonPolicyRequestInvalid), "invalid policy spec: " + err.Error()
	}
	return req, "", ""
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
//
// "Available to" (capPolicy): anyone below the security tier gets only the
// rows the launch door would let them select (capVisible), filtered BEFORE the
// window so the page and X-Wardyn-Truncated count nothing they cannot see. The
// security tier sees every row, redacted only of mount host paths, for the same reason: it
// writes each policy's Available to list and cannot do it against rows it is
// not shown. Selecting one is still capability-bounded at the door.
//
// ponytail: a filtered reader fetches every row and windows in Go, as
// GET /integrations does; stored policies are an admin-authored handful.
func (s *Server) handleListPolicies(w http.ResponseWriter, r *http.Request) {
	page, ok := parseListPage(w, r, defaultListLimit)
	if !ok {
		return
	}
	ctx := r.Context()
	security := s.isSecurityOperator(ctx)
	tier := s.specReadTierFor(ctx)
	var pageFn func(store.Page) ([]types.RunPolicy, error)
	if pg, ok := s.cfg.Store.(store.Pager); ok && security {
		pageFn = func(p store.Page) ([]types.RunPolicy, error) {
			ps, err := pg.ListPoliciesPage(ctx, p)
			return redactPoliciesForRead(ps, tier), err
		}
	}
	servePage(w, r, page, pageFn, func() ([]types.RunPolicy, error) {
		ps, err := s.cfg.Store.ListPolicies(ctx)
		if err != nil {
			return nil, err
		}
		if !security {
			ps = capVisible(ctx, s, capPolicy, ps, func(p types.RunPolicy) string { return p.ID.String() })
		}
		return redactPoliciesForRead(ps, tier), nil
	})
}

// handleGetPolicy returns one policy by id (404 when unknown).
func (s *Server) handleGetPolicy(w http.ResponseWriter, r *http.Request) {
	id, ok := parseIDParam(w, r, "id", "policy")
	if !ok {
		return
	}
	p, err := s.cfg.Store.GetPolicy(r.Context(), id)
	if notFoundIf(w, err, "policy", reasonPolicyNotFound) {
		return
	}
	if err != nil {
		writeServerError(w, r, "get policy", err)
		return
	}
	// Below the security tier, answered as the launch door answers this id
	// (runs.policy): the list leaves it out, so the read by id must too.
	if !s.isSecurityOperator(r.Context()) && s.denyUserCapability(w, r, capPolicy, p.ID.String(), "policies.read",
		"Stored policy "+p.ID.String()+" isn't available to you. Ask your admin, or launch without policy_id.") {
		return
	}
	writeJSON(w, http.StatusOK, redactPolicyForRead(p, s.specReadTierFor(r.Context())))
}

// defaultPolicyResponse is GET /policies/default's body: the resolved ceiling
// spec, EMBEDDED so the shape every existing consumer parses is unchanged, plus
// the name of the governance profile it came from.
//
// unit_name is ADDITIVE and omitted entirely for an unassigned
// caller. "" would be a value the console then has to special-case, while an
// absent key already decodes as "no profile" in the TS mirror. It exists
// because the spec alone cannot answer the question a member actually has: the
// New Run ceiling line and the Getting Started governance row both need to name
// WHICH profile bounds them, and the name is the only handle an admin and a
// member share.
//
// contexts and unit_path are the policy tree's: the caller's own units (omitted for a caller no
// ceiling binds and for one no unit places, whose body stays today's) and, for a security admin only,
// the path of the unit answered, leaf first, names only (absent for the Default unit): a member is
// told the leaf's name and nothing of the units above it. ?run_in= answers for another of the
// caller's units, as POST /runs' run_in does.
type defaultPolicyResponse struct {
	types.RunPolicySpec
	UnitName string                 `json:"unit_name,omitempty"`
	UnitPath []unitPathLink         `json:"unit_path,omitempty"`
	Contexts []defaultPolicyContext `json:"contexts,omitempty"`
}

// defaultPolicyContext is one unit the caller may run in; home is the one a run naming none runs in.
type defaultPolicyContext struct {
	UnitID   uuid.UUID `json:"unit_id"`
	UnitName string    `json:"unit_name"`
	Home     bool      `json:"home"`
}

// handleGetDefaultPolicy returns THE CALLER'S ceiling — the spec a run created
// without a policy_id gets, and (per composer.Clamp / inline_policy.go) the
// same ceiling their inline policy is clamped against. Previously
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
	runIn, err := s.runInFromRequest(r.Context(), createRunRequest{RunIn: r.URL.Query().Get("run_in")})
	ctx := withContextChoice(r.Context(), contextChoice{RunIn: runIn})
	var ceiling governanceCeiling
	if err == nil {
		ceiling, err = s.effectiveCeiling(ctx)
	}
	if refusal := contextRunRefusal(err); refusal != nil {
		refusal.write(s, w, r)
		return
	}
	if err != nil {
		writeCeilingError(w, r, err)
		return
	}
	operator := s.isSecurityOperator(r.Context())
	resp := defaultPolicyResponse{
		RunPolicySpec: redactSpecForRead(ceiling.Spec, s.specReadTierFor(r.Context())),
	}
	if ceiling.Limits.DenyUIApps {
		// A run under this ceiling gets no UI apps, so the read lists none.
		resp.UIApps = nil
	}
	if ceiling.Unit != nil {
		resp.UnitName = ceiling.Unit.Name
		if operator { // the chain is an admin surface (ResolvedUnit.Chain): a member reads the leaf alone
			for _, l := range ceiling.Unit.Chain {
				resp.UnitPath = append(resp.UnitPath, unitPathLink(l))
			}
		}
	}
	if !ceiling.Operator && ceiling.Unit != nil && s.cfg.Store != nil {
		if resp.Contexts, err = s.callerContexts(r.Context()); err != nil {
			writeCeilingError(w, r, err)
			return
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

// callerContexts is the caller's own contexts, as the check route lists them for an admin. With a
// group snapshot that cannot be read, only the rows it cannot hide are listed (selectContext's rule).
func (s *Server) callerContexts(ctx context.Context) ([]defaultPolicyContext, error) {
	subj, err := s.callerSubjects(ctx)
	if err != nil {
		return nil, err
	}
	q := contextQuery{users: subj.users, groups: subj.groups, userType: subj.userType,
		unusable: subj.stale || oidcGroupsTruncatedFromContext(ctx)}
	home, err := s.selectContext(ctx, q, contextChoice{})
	if err != nil {
		return nil, err
	}
	if q.unusable {
		q.groups = nil
	}
	all, _, err := s.checkContexts(ctx, q, home)
	if err != nil {
		return nil, err
	}
	out := make([]defaultPolicyContext, len(all))
	for i, c := range all {
		out[i] = defaultPolicyContext{UnitID: c.UnitID, UnitName: c.UnitName, Home: c.Home}
	}
	return out, nil
}

// redactPolicyForRead returns p with any llm_inspection.workspace_secret_values
// replaced by a count before it is ever serialized back to a caller. Belt-and-
// braces: validatePolicySpec already refuses a WRITE that sets a raw
// value (an operator authors workspace_secret_names instead — see
// types.LLMInspectionSpec), so a stored row should never carry one — but a READ
// path must never re-expose it if that invariant is ever violated (a migration,
// a direct DB edit, ...). Mirrors the run.policy.resolve audit redaction
// (runs_dispatch.go) — same shape, different chokepoint. Does not mutate p's
// own LLMInspection (a fresh copy is substituted), so a caller holding the
// original is never surprised by an in-place edit.
func redactPolicyForRead(p types.RunPolicy, tier specReadTier) types.RunPolicy {
	p.Spec = redactSpecForRead(p.Spec, tier)
	return p
}

// specReadTier is how much of a policy spec a reader may see, in the shape of workspaceReadTier.
// A super admin reads it whole. A security admin reads everything but the host paths behind
// workspace_mounts[].source: a mount's host path is the super admin's to see and set (the tier
// that writes the host, http.go). Everyone else reads it as redactSpecForUser leaves it.
type specReadTier int

const (
	specReadFull specReadTier = iota
	specReadSecurity
	specReadMember
)

// specReadTierFor answers which tier this request reads a policy spec at.
func (s *Server) specReadTierFor(ctx context.Context) specReadTier {
	switch {
	case s.isOperator(ctx):
		return specReadFull
	case s.isSecurityOperator(ctx):
		return specReadSecurity
	}
	return specReadMember
}

// redactSpecForRead is redactPolicyForRead's logic, factored out so
// handleGetDefaultPolicy (which reads a bare spec, not a stored types.RunPolicy)
// gets the same redaction without a fake wrapper row.
func redactSpecForRead(spec types.RunPolicySpec, tier specReadTier) types.RunPolicySpec {
	spec.WorkspaceRepos = redactWorkspaceRepoList(spec.WorkspaceRepos) // a legacy row, every tier
	switch tier {
	case specReadSecurity:
		spec = withholdMountSources(spec)
	case specReadMember:
		spec = redactSpecForUser(spec)
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
func redactPoliciesForRead(ps []types.RunPolicy, tier specReadTier) []types.RunPolicy {
	for i := range ps {
		ps[i] = redactPolicyForRead(ps[i], tier)
	}
	return ps
}

// memberSecretScopeKeys are the eligible-grant scope fields that NAME an
// operator secret. The `secret` capability exists to bound which secret names a
// member can see (it narrows GET /secrets), and a policy read handed the same
// names back to every member — including on GET /policies/default, which every
// member reaches because the ceiling is what clamps them.
var memberSecretScopeKeys = []string{"secret_name", "key_secret_ref", "known_hosts_secret_ref"}

// redactSpecForUser strips the operator-only details a policy spec
// carries out of a MEMBER-reachable read: the host filesystem paths behind
// workspace_mounts[].source (the blessed ~/.claude credential mount among
// them), the stored-secret names on the eligible grants and
// llm_inspection.workspace_secret_names. Everything else —
// the egress allowlist, the confinement floor, the grant KINDS and hosts — is
// exactly what a member is being clamped by and stays visible.
//
// Copies before it edits: the default policy is server config held for the
// process's whole life, so an in-place edit here would redact it permanently
// for the operator too.
func redactSpecForUser(spec types.RunPolicySpec) types.RunPolicySpec {
	spec = withholdMountSources(spec)
	if len(spec.EligibleGrants) > 0 {
		grants := slices.Clone(spec.EligibleGrants)
		for i := range grants {
			grants[i].Scope = redactScopeSecretRefs(grants[i].Scope)
		}
		spec.EligibleGrants = grants
	}
	// The workspace secret names are the operator's secret names too, the same
	// thing memberSecretScopeKeys hides on the grants, so they are counted, not listed.
	if li := spec.LLMInspection; li != nil && len(li.WorkspaceSecretNames) > 0 {
		cp := *li
		cp.WorkspaceSecretNames = []string{fmt.Sprintf("<%d name(s) redacted>", len(li.WorkspaceSecretNames))}
		spec.LLMInspection = &cp
	}
	return spec
}

// mountSourceWithheld is the source a reader below the super admin tier reads in place of a mount's
// host path, and the one spelling a writer sends back to keep that mount (governance_mount_keep.go).
const mountSourceWithheld = "<redacted>"

// withholdMountSources is spec with every workspace_mounts[].source replaced by mountSourceWithheld,
// target and read_only kept. It clones the mounts, for the reason redactSpecForUser does.
func withholdMountSources(spec types.RunPolicySpec) types.RunPolicySpec {
	spec.WorkspaceMounts = withholdMounts(spec.WorkspaceMounts)
	return spec
}

// withholdMounts is withholdMountSources for a bare list (an overlay's), on a copy.
func withholdMounts(in []types.WorkspaceMount) []types.WorkspaceMount {
	if len(in) == 0 {
		return in
	}
	out := slices.Clone(in)
	for i := range out {
		out[i].Source = mountSourceWithheld
	}
	return out
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
	// The decoders that read a scope match its keys without regard to case, so
	// "Secret_Name" names the secret as surely as "secret_name".
	for k := range fields {
		if slices.ContainsFunc(memberSecretScopeKeys, func(m string) bool { return strings.EqualFold(k, m) }) {
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
	req, reason, msg := decodePolicyRequest(w, r, s.adoHostsLoader(r.Context()))
	if msg != "" {
		writeErrorReason(w, http.StatusBadRequest, reason, msg)
		return
	}
	// Authoring fail-fast: a policy's user-workspace mounts/repos must be onboarded
	// (the run-create chokepoint is the load-bearing gate; this surfaces the error
	// at author time instead of at launch).
	// Not the workspace-provider admission verdict, deliberately: that one
	// lives at run create, so a policy naming a repo no provider admits can still be
	// WRITTEN — and is then refused at every door that would clone it. Authoring is
	// not cloning, and narrowing the provider rows must not retroactively make a
	// stored policy unsaveable.
	if code, reason, err := s.validateWorkspaceSources(r.Context(), req.Spec, runCarrierRule{}); err != nil {
		writeErrorReason(w, code, reason, "workspace: "+err.Error())
		return
	}
	// Secret references are checked for shape only. Whether a named secret
	// exists depends on whose run selects the policy, so run-create checks it
	// in that owner's namespace; checking the author's here demanded an
	// operator row, the fallback a per-person credential must not have (#1123).
	needed, err := s.secretRefsOf(req.Spec)
	if err != nil {
		writeErrorReason(w, http.StatusUnprocessableEntity, reasonPolicySecretRefsInvalid, "secret: "+err.Error())
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
		// run_policies.name is UNIQUE — a duplicate name is a
		// caller-fixable 409, not a raw Postgres 500 (contrast CreateApproval's
		// existing 23505 sentinel for approvals).
		writeErrorReason(w, http.StatusConflict, reasonPolicyNameConflict, fmt.Sprintf("a policy named %q already exists", req.Name))
		return
	}
	if err != nil {
		writeServerError(w, r, "create policy", err)
		return
	}
	s.recordAudit(r.Context(), s.auditEvent(nil, actorTypeFromRequest(r), principalFromRequest(r),
		"policy.create", id.String(), "success", mustJSON(map[string]any{
			"name": created.Name, "min_confinement_class": created.Spec.MinConfinementClass, "after": created.Spec,
		})))
	writeJSON(w, http.StatusCreated, client.PolicySaved{RunPolicy: created, Requirements: s.policyRequirements(r, req.Spec, needed)})
}

// handleUpdatePolicy validates the spec and replaces an existing policy's name
// and spec. Returns 404 when the policy is unknown, 400 on an invalid body/spec.
func (s *Server) handleUpdatePolicy(w http.ResponseWriter, r *http.Request) {
	id, ok := parseIDParam(w, r, "id", "policy")
	if !ok {
		return
	}
	req, reason, msg := decodePolicyRequest(w, r, s.adoHostsLoader(r.Context()))
	if msg != "" {
		writeErrorReason(w, http.StatusBadRequest, reason, msg)
		return
	}
	if code, reason, err := s.validateWorkspaceSources(r.Context(), req.Spec, runCarrierRule{}); err != nil {
		writeErrorReason(w, code, reason, "workspace: "+err.Error())
		return
	}
	needed, err := s.secretRefsOf(req.Spec) // shape only, as in create
	if err != nil {
		writeErrorReason(w, http.StatusUnprocessableEntity, reasonPolicySecretRefsInvalid, "secret: "+err.Error())
		return
	}
	// The audit row records the policy as it was and as it is: a spec names secrets, never their values
	// (secretRefsOf). The read and the write are not one step; /policies writes are single-human.
	prev, err := s.cfg.Store.GetPolicy(r.Context(), id)
	if notFoundIf(w, err, "policy", reasonPolicyNotFound) {
		return
	}
	if err != nil {
		writeServerError(w, r, "read policy", err)
		return
	}
	updated, err := s.cfg.Store.UpdatePolicy(r.Context(), id, req.Name, req.Spec)
	if notFoundIf(w, err, "policy", reasonPolicyNotFound) {
		return
	}
	if errors.Is(err, store.ErrConflict) {
		// The rename half of the same rule above: run_policies.name is UNIQUE, so
		// moving a policy ONTO a taken name raises the same 23505 an insert under
		// one does: a caller-fixable 409, never the blanket 500 below carrying the
		// raw Postgres constraint string. Sited after notFoundIf so an unknown id
		// is still a 404.
		writeErrorReason(w, http.StatusConflict, reasonPolicyNameConflict, fmt.Sprintf("a policy named %q already exists", req.Name))
		return
	}
	if err != nil {
		writeServerError(w, r, "update policy", err)
		return
	}
	s.recordAudit(r.Context(), s.auditEvent(nil, actorTypeFromRequest(r), principalFromRequest(r),
		"policy.update", id.String(), "success", mustJSON(map[string]any{
			"name": updated.Name, "min_confinement_class": updated.Spec.MinConfinementClass,
			"before": map[string]any{"name": prev.Name, "spec": prev.Spec},
			"after":  map[string]any{"name": updated.Name, "spec": updated.Spec},
		})))
	writeJSON(w, http.StatusOK, client.PolicySaved{RunPolicy: updated, Requirements: s.policyRequirements(r, req.Spec, needed)})
}

// handleDeletePolicy removes a policy. Returns 404 when unknown, 204 on success.
func (s *Server) handleDeletePolicy(w http.ResponseWriter, r *http.Request) {
	id, ok := parseIDParam(w, r, "id", "policy")
	if !ok {
		return
	}
	prev, err := s.cfg.Store.GetPolicy(r.Context(), id)
	if notFoundIf(w, err, "policy", reasonPolicyNotFound) {
		return
	}
	if err != nil {
		writeServerError(w, r, "read policy", err)
		return
	}
	err = s.cfg.Store.DeletePolicy(r.Context(), id)
	if notFoundIf(w, err, "policy", reasonPolicyNotFound) {
		return
	}
	if err != nil {
		writeServerError(w, r, "delete policy", err)
		return
	}
	s.recordAudit(r.Context(), s.auditEvent(nil, actorTypeFromRequest(r), principalFromRequest(r),
		"policy.delete", id.String(), "success", mustJSON(map[string]any{"name": prev.Name, "before": prev.Spec})))
	w.WriteHeader(http.StatusNoContent)
}

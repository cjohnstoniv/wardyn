// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/authz"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

func (s *Server) repoAdmissionRefusal(r *http.Request, repos ...string) *runRefusal {
	repos = presentRepos(repos)
	if len(repos) == 0 || s.cfg.Store == nil {
		return nil
	}
	sc, err := s.cfg.Store.GetSiteConfig(r.Context())
	if err != nil {
		return runServerError("get site config", err)
	}
	if !providersConfigured(sc) {
		return nil // legacy open mode — byte-identical to 0.7.1
	}
	operator := s.runUngoverned(r.Context())
	for _, repo := range repos {
		if msg := admissionRefusal(sc, repo, operator); msg != "" {
			return runError(admissionRefusalStatus(operator), reasonWorkspaceRepoNotAdmitted, msg)
		}
	}
	s.auditLegacyHostAdmissions(r.Context(), sc, repos)
	return nil
}

func (s *Server) workspaceProvidersRefusal(r *http.Request, target string, repos ...string) *runRefusal {
	if len(repos) == 0 || s.cfg.Store == nil || s.runUngoverned(r.Context()) {
		return nil
	}
	sc, err := s.cfg.Store.GetSiteConfig(r.Context())
	if err != nil {
		return runServerError("get site config", err)
	}
	if !providersConfigured(sc) {
		return nil // legacy open mode — byte-identical to 0.7.1
	}
	seen := map[string]bool{}
	for _, repo := range repos {
		row := admitRepoURL(sc, repoCloneURL(repo)).Provider
		if row.ID == "" || seen[row.ID] {
			continue
		}
		seen[row.ID] = true
		if refusal := s.runCapabilityRefusal(r, capWorkspaceProvider, row.ID, target,
			fmt.Sprintf(capProvider403, row.Kind)); refusal != nil {
			return refusal
		}
	}
	return nil
}

func (s *Server) seedAuthorizedWorkspace(ctx context.Context, r *http.Request, spec *types.RunPolicySpec, req *createRunRequest) ([]string, *runRefusal) {
	// First, before a single source is folded: authorize the SELECTION against
	// the CALLER. Everything below this line reasons about host paths that are
	// about to become binds, and until now nothing on the path asked whose
	// workspace they came from — the only member-mount check downstream is
	// evaluated against the workspace OWNER, so any member (and a security
	// admin, a tier defined never to reach the host) could name another
	// member's workspace id and get their directory bound inside a sandbox they
	// control. The store-less case is left to seedRequestWorkspace, which
	// answers it with its own 422.
	if req.WorkspaceID != nil && s.cfg.Store != nil {
		if _, refusal := s.workspaceLaunchSelection(r, *req.WorkspaceID); refusal != nil {
			return nil, refusal
		}
	}
	ephemeralDirs, seededImageOwner, code, seedReason, seedErr := s.seedRequestWorkspace(ctx, spec, req)
	if seedErr != nil {
		return nil, runError(code, seedReason, "workspace_id: "+seedErr.Error())
	}
	if refusal := s.seededImageRefusal(r, seededImageOwner, req.Image); refusal != nil {
		return nil, refusal
	}
	if msg, reason := s.validateImageBuildRequest(*req); msg != "" {
		return nil, runError(http.StatusBadRequest, reason, msg)
	}
	if code, reason, err := s.validateWorkspaceSources(ctx, *spec); err != nil {
		return nil, runError(code, reason, "workspace: "+err.Error())
	}
	// The caller-scoped twin of the onboarding gate above: onboarded is not the
	// same question as "onboarded BY SOMEONE THIS CALLER MAY LAUNCH AS", and a
	// policy naming a host path directly never passes through the workspace_id
	// door that answers the second one.
	//
	// #656 H1: the not-onboarded arm of BOTH this check and validateWorkspaceSources
	// above answers with the SAME reason (reasonWorkspaceSourceNotOnboarded) —
	// deliberately, since the message is already byte-identical for the same
	// cross-member existence-oracle reason (see authorizeSpecWorkspaceSources' own
	// doc comment): a distinguishable reason would reopen exactly what the shared
	// sentence closes.
	if code, reason, err := s.authorizeSpecWorkspaceSources(ctx, r, *spec); err != nil {
		return nil, runError(code, reason, "workspace: "+err.Error())
	}
	// Both provider gates sit at this chokepoint, over the RESOLVED spec's repos
	// — the un-bypassable one, reached alike by workspace_id, a stored policy and
	// a hand-authored inline policy. ADMISSION runs first (the operator-binding
	// question), then the member capability.
	//
	// HERE and not in validateWorkspaceSources above, which is the other function
	// that sees the resolved spec: this scope holds the request, so the refusal
	// can read the caller's tier (a member's 403 names the kind, an operator's 422
	// lists the addresses) and answer the frozen sentence verbatim rather than
	// through that function's "workspace: " error prefix. Its other two callers
	// are the POLICY write doors (policies.go), where nothing clones.
	repos := repoLocatorsOf(spec.WorkspaceRepos)
	if refusal := s.repoAdmissionRefusal(r, repos...); refusal != nil {
		return nil, refusal
	}
	if refusal := s.workspaceProvidersRefusal(r, "runs.workspace_provider", repos...); refusal != nil {
		return nil, refusal
	}
	return ephemeralDirs, nil
}

// Missing and foreign selections share a 404 before seeding; the seed helper
// otherwise answers a missing row with 422, revealing whether it exists.
func (s *Server) workspaceLaunchSelection(r *http.Request, id uuid.UUID) (types.Workspace, *runRefusal) {
	if s.cfg.Store == nil {
		return types.Workspace{}, runError(http.StatusInternalServerError, reasonWorkspaceStoreUnavailable, "get workspace: no store configured")
	}
	ws, err := s.cfg.Store.GetWorkspace(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		return types.Workspace{}, runError(http.StatusNotFound, reasonWorkspaceNotFound, "workspace not found")
	}
	if err != nil {
		return types.Workspace{}, runServerError("get workspace", err)
	}
	if !s.mayLaunchWorkspace(r, ws) {
		return types.Workspace{}, runDenied(authz.Deny(authz.ReasonNotOwner, ws.ID.String(), "workspace not found").AsIf(authz.Reason(reasonWorkspaceNotFound)))
	}
	return ws, nil
}

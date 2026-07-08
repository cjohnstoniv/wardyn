// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"fmt"
	"net/http"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// systemMountTargets are in-container mount targets authored by WARDYN ITSELF
// (subscription credential staging), NOT by a user-onboarded workspace. A mount
// at one of these targets is EXEMPT from the onboarding-source check: its source
// is the operator's resident creds dir, which is inherently un-onboarded and is
// the credential-staging power subscription mode requires. Keyed on TARGET (not
// source) because the target is what identifies a system mount. The recording
// mount needs no entry — it never flows through spec.WorkspaceMounts (it is a
// driver-config bind, validated separately in the docker driver).
var systemMountTargets = map[string]bool{
	claudeCredTarget:     true, // /home/agent/.claude
	claudeCredJSONTarget: true, // /home/agent/.claude.json
}

// validateWorkspaceSources fails a run closed (422) when any USER-workspace mount
// source or repo on the spec is not a pre-ONBOARDED workspace. It is the
// un-bypassable onboarding gate: called at run-create over the RESOLVED spec
// (inline, stored, OR default policy alike), so no authoring surface — including a
// hand-crafted stored policy — can smuggle an arbitrary host path or repo into a
// sandbox. System mounts (subscription creds) are exempt by target.
//
// It mirrors validateInlineSecretRefs: consults the workspace LIST only (never a
// profile/secret value), returns (statusCode, error) on rejection or (0, nil) when
// every user source is onboarded. The runner.ValidateMount deny-list still runs
// underneath as defense-in-depth; this is a floor-RAISING allow-list on top.
func (s *Server) validateWorkspaceSources(ctx context.Context, spec types.RunPolicySpec) (int, error) {
	// Fast path: nothing to gate if the run declares no user workspaces (a mount
	// at a system target does not count).
	hasUserMount := false
	for _, wm := range spec.WorkspaceMounts {
		if !systemMountTargets[wm.Target] {
			hasUserMount = true
			break
		}
	}
	if !hasUserMount && len(spec.WorkspaceRepos) == 0 {
		return 0, nil
	}

	if s.cfg.Store == nil {
		return http.StatusUnprocessableEntity, fmt.Errorf(
			"workspace onboarding requires a store, but none is configured")
	}
	ws, err := s.cfg.Store.ListWorkspaces(ctx)
	if err != nil {
		return http.StatusUnprocessableEntity, fmt.Errorf("list workspaces: %w", err)
	}
	localSrc := make(map[string]bool)
	repoSrc := make(map[string]bool)
	for _, w := range ws {
		switch w.Kind {
		case types.WorkspaceKindLocalDir:
			localSrc[w.Source] = true
		case types.WorkspaceKindRepo:
			repoSrc[w.Source] = true
		}
	}

	for _, wm := range spec.WorkspaceMounts {
		if systemMountTargets[wm.Target] {
			continue // system creds mount — exempt
		}
		if !localSrc[wm.Source] {
			return http.StatusUnprocessableEntity, fmt.Errorf(
				"mount source %q is not an onboarded local directory (onboard it first via the workspaces API)", wm.Source)
		}
	}
	for _, wr := range spec.WorkspaceRepos {
		if !repoSrc[wr.Repo] {
			return http.StatusUnprocessableEntity, fmt.Errorf(
				"repo %q is not an onboarded repository (onboard it first via the workspaces API)", wr.Repo)
		}
	}
	return 0, nil
}

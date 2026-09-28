// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"log/slog"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// This file is #1267 (closing #1250 too, per its design note): the per-row
// `available_to_you` GET /workspaces and GET /workspaces/{id} stamp. Earlier
// attempts tried to carry restriction FACTS to the console (a restricted
// value list on /me/capabilities) and found no leak-free shape that could
// answer "unavailable" — a value absent from such a list means EITHER "not
// restricted" OR "restricted and refused to me", and the console cannot tell
// the two apart (see the design note on #1250). The fix instead answers the
// ANSWER, not the facts behind it: one derived bit per workspace, computed by
// the identical decide path a launch runs.

// workspaceAvailableToCaller is available_to_you: would the launch path admit
// this workspace for the calling principal, over exactly the values it
// checks —
//
//   - capWorkspace on the row's own id (denyUserRequest's own field);
//   - the git-provider row every repo source's derived clone URL resolves to
//     (workspace_providers.go's admitRepoURL — the SAME derivation
//     denyUserWorkspaceProviders uses, including its "key on Provider, never
//     Admitted" rule and its legacy-open-mode no-op);
//   - the workspace's own model-provider pin (WorkspaceLLMCred.ProviderRef),
//     ONLY when a model-provider block exists (sc.ModelProviders != nil) —
//     the same gate enforceRunModelProvider applies before it ever asks
//     capModelProvider, whose gatesAdminPins bit is what makes an admin's own
//     pin, not only a member's free-text choice, capability-bound.
//
// decide's own step 1 exempts an operator before any of the above ever reads
// a row, so this is always true for one. It carries no restriction contents
// and no other caller's grants — a single derived bit, never the "Only..."
// list itself.
//
// A resolution error answers false, never true: an error is never allowed to
// read as availability, capAllowed's own rule. GetSiteConfig itself is the
// caller's problem (see workspaceAvailabilitySiteConfig) — a site config that
// could not be read degrades to "no providers configured", the same graceful
// answer admissionStamper gives its own question, because this bit is
// advisory (denyUserRequest is the real gate) and a degraded READ must not
// turn into a failed LIST.
func (s *Server) workspaceAvailableToCaller(ctx context.Context, sc types.SiteConfig, ws types.Workspace) bool {
	b := s.capBatchFor(ctx)
	allowed, err := b.allowed(ctx, capWorkspace, ws.ID.String())
	if err != nil {
		slog.ErrorContext(ctx, "api: resolve workspace availability", "err", err, "workspace", ws.ID)
		return false
	}
	if !allowed {
		return false
	}
	if providersConfigured(sc) {
		seen := map[string]bool{}
		for _, repo := range repoSourceLocators(ws.Sources) {
			row := admitRepoURL(sc, repoCloneURL(repo)).Provider
			if row.ID == "" || seen[row.ID] {
				continue
			}
			seen[row.ID] = true
			ok, err := b.allowed(ctx, capWorkspaceProvider, row.ID)
			if err != nil {
				slog.ErrorContext(ctx, "api: resolve workspace provider availability", "err", err, "workspace", ws.ID)
				return false
			}
			if !ok {
				return false
			}
		}
	}
	if ws.LLMCred != nil && ws.LLMCred.ProviderRef != "" && sc.ModelProviders != nil {
		ok, err := b.allowed(ctx, capModelProvider, ws.LLMCred.ProviderRef)
		if err != nil {
			slog.ErrorContext(ctx, "api: resolve workspace model-provider availability", "err", err, "workspace", ws.ID)
			return false
		}
		if !ok {
			return false
		}
	}
	return true
}

// workspaceAvailabilitySiteConfig reads the site config ONCE for the whole
// stamping pass, exactly as admissionStamper does for its own question. A nil
// Store or a read failure answers the zero value — no provider block, no
// model-provider block — so workspaceAvailableToCaller falls back to asking
// only the one check every deployment always has, capWorkspace.
func (s *Server) workspaceAvailabilitySiteConfig(ctx context.Context) types.SiteConfig {
	if s.cfg.Store == nil {
		return types.SiteConfig{}
	}
	sc, err := s.cfg.Store.GetSiteConfig(ctx)
	if err != nil {
		slog.ErrorContext(ctx, "api: get site config for workspace availability", "err", err)
		return types.SiteConfig{}
	}
	return sc
}

// availabilityStamper returns a per-row stamper sharing ONE site-config read
// for the whole page. ctx should already carry a capBatch memo (withCapBatch)
// so every row's capBatchFor call shares the same grants/enforcement/
// restrictions reads instead of one round trip per row; capBatchFor degrades
// gracefully to its own one-shot batch when ctx carries no memo.
func (s *Server) availabilityStamper(ctx context.Context) func(types.Workspace) types.Workspace {
	sc := s.workspaceAvailabilitySiteConfig(ctx)
	return func(ws types.Workspace) types.Workspace {
		ws.AvailableToYou = s.workspaceAvailableToCaller(ctx, sc, ws)
		return ws
	}
}
